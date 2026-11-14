package nginx

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
)

var cursorRE = regexp.MustCompile(spec.CursorPattern)

// status reports the service's live state and the effective settings. It
// runs no configuration test (FR-3): only the init system's status query
// and the cached `nginx -v`.
func (p *Provider) status(ctx context.Context, _ spec.StatusInput) (apitypes.List[spec.Service], error) {
	var none apitypes.List[spec.Service]
	s := p.Settings()
	ctl, err := p.controller(s)
	if err != nil {
		return none, unavailable(err.Error())
	}
	sctx, cancel := context.WithTimeout(ctx, s.StateTimeout)
	defer cancel()
	st, err := ctl.Status(sctx, s.Service)
	if err != nil {
		p.log.Warn("reading the nginx service state failed", "service", s.Service, "error", err.Error())
		return none, unavailable(fmt.Sprintf("the state of service %s cannot be read from %s: %v",
			s.Service, ctl.Init(), err))
	}
	props := spec.ServiceProperties{
		State:          string(st.State),
		Since:          st.Since,
		Version:        p.versions.get(ctx, p.nginx, p.versionTimeout),
		InitSystem:     string(ctl.Init()),
		Service:        s.Service,
		IncludeDir:     s.IncludeDir,
		IncludeSuffix:  s.IncludeSuffix,
		DisabledSuffix: s.DisabledSuffix,
	}
	if st.MainPID > 0 {
		pid := st.MainPID
		props.MainPID = &pid
	}
	svc := apitypes.NewResource(p.host, spec.Name, spec.ServiceResourceType, s.Service, props)
	return apitypes.List[spec.Service]{Items: []spec.Service{svc}}, nil
}

// list reads includeDir once and returns the sites in name order, a page
// at a time. Only names ending in includeSuffix (enabled) or includeSuffix
// plus disabledSuffix (disabled) are sites; dotfiles never are, which keeps
// the helper's hidden temporary files out (FR-7).
func (p *Provider) list(_ context.Context, in spec.ListInput) (apitypes.List[spec.Site], error) {
	var none apitypes.List[spec.Site]
	after, err := decodeCursor(in.Cursor)
	if err != nil {
		return none, err
	}
	s := p.Settings()
	dir, err := openIncludeDir(s)
	if err != nil {
		return none, err
	}
	defer dir.Close()
	entries, err := dir.Readdirnames(-1)
	if err != nil {
		return none, unavailable(fmt.Sprintf("includeDir %s cannot be read: %v", s.IncludeDir, err))
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		if name, ok := siteName(e, s); ok && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	slices.Sort(names)
	page, next := paginate(names, after, in.Limit)
	dirfd := int(dir.Fd())
	out := apitypes.List[spec.Site]{Items: make([]spec.Site, 0, len(page)), NextCursor: next}
	for _, name := range page {
		c, err := classify(dirfd, name, s)
		if err != nil {
			return none, unavailable(fmt.Sprintf("includeDir %s cannot be read: %v", s.IncludeDir, err))
		}
		if !c.Exists() {
			continue // Renamed or removed since the directory was read
		}
		out.Items = append(out.Items, p.siteResource(dirfd, c, s))
	}
	return out, nil
}

// get returns one site by name, with the properties list gives it.
func (p *Provider) get(_ context.Context, in spec.SiteInput) (spec.Site, error) {
	s := p.Settings()
	dir, c, err := lookup(in.Name, s)
	if err != nil {
		return spec.Site{}, err
	}
	defer dir.Close()
	return p.siteResource(int(dir.Fd()), c, s), nil
}

// content returns a managed site's text and SHA-256. It never follows a
// symlink, refuses every unmanaged entry, and refuses rather than truncates
// a file over maxConfigBytes (FR-8).
func (p *Provider) content(_ context.Context, in spec.SiteInput) (spec.SiteContent, error) {
	var none spec.SiteContent
	s := p.Settings()
	dir, c, err := lookup(in.Name, s)
	if err != nil {
		return none, err
	}
	defer dir.Close()
	f := c.File()
	if !c.Managed {
		return none, problem.New(problem.NginxConfigUnmanaged,
			fmt.Sprintf("%s is not managed (%s): its content is not read", f.FileName, c.Reason)).
			With("reason", c.Reason).With("fileName", f.FileName)
	}
	b, digest, err := readManaged(int(dir.Fd()), f, s.MaxConfigBytes)
	if err != nil {
		return none, err
	}
	return spec.SiteContent{ID: apitypes.ResourceID(spec.Name, spec.SitesResourceType, c.Name), Name: c.Name,
		FileName: f.FileName, SHA256: digest, SizeBytes: int64(len(b)), Content: string(b)}, nil
}

// lookup opens includeDir and classifies name: not-found when neither entry
// exists. The caller closes the directory.
func lookup(name string, s Settings) (*os.File, site, error) {
	// The schema admits only NamePattern without ".."; the rest of the name
	// rule (FR-12), the configured suffixes, is checked here, so a name the
	// rule refuses is validation-failed on every op that takes one.
	if err := ValidName(name, s); err != nil {
		return nil, site{}, invalid("/name", err.Error())
	}
	dir, err := openIncludeDir(s)
	if err != nil {
		return nil, site{}, err
	}
	c, err := classify(int(dir.Fd()), name, s)
	if err != nil {
		dir.Close()
		return nil, site{}, unavailable(fmt.Sprintf("includeDir %s cannot be read: %v", s.IncludeDir, err))
	}
	if !c.Exists() {
		dir.Close()
		return nil, site{}, problem.New(problem.NotFound,
			fmt.Sprintf("no site %s: neither %s nor %s exists", name, s.EnabledFile(name), s.DisabledFile(name)))
	}
	return dir, c, nil
}

// siteName is the site a directory entry belongs to, if any.
func siteName(entry string, s Settings) (string, bool) {
	if strings.HasPrefix(entry, ".") {
		return "", false
	}
	for _, sfx := range []string{s.IncludeSuffix + s.DisabledSuffix, s.IncludeSuffix} {
		if name, ok := strings.CutSuffix(entry, sfx); ok && name != "" {
			return name, true
		}
	}
	return "", false
}

// siteResource builds a site's resource from its entry. The SHA-256 is
// streamed for a regular file of at most maxConfigBytes, and is null for
// any other entry or when the file changed under the read.
func (p *Provider) siteResource(dirfd int, c site, s Settings) spec.Site {
	f := c.File()
	props := spec.SiteProperties{
		Enabled:         c.Enabled != nil,
		FileName:        f.FileName,
		Managed:         c.Managed,
		FileType:        f.Type,
		UnmanagedReason: c.Reason,
		Conflict:        c.Conflict,
		SizeBytes:       f.Size,
		ModTime:         f.ModTime,
	}
	if f.Type == spec.FileTypeFile && f.Size <= s.MaxConfigBytes {
		if _, digest, err := readManaged(dirfd, f, s.MaxConfigBytes); err == nil {
			props.SHA256 = &digest
		} else {
			p.log.Debug("no sha256 for an nginx site", "fileName", f.FileName, "error", err.Error())
		}
	}
	return apitypes.NewResource(p.host, spec.Name, spec.SitesResourceType, c.Name, props)
}

// readManaged reads the regular file f in dirfd, at most max bytes, and
// returns its bytes and their SHA-256. The open never follows a symlink and
// never blocks on a FIFO, and the opened file must be the entry classify
// saw, so a swap between the two is refused rather than read.
func readManaged(dirfd int, f *fileStat, max int64) ([]byte, string, error) {
	if f.Size > max {
		return nil, "", tooLarge(f.FileName, f.Size, max)
	}
	var fd int
	err := ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(dirfd, f.FileName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		return nil, "", changed(f.FileName, err)
	}
	file := os.NewFile(uintptr(fd), f.FileName)
	defer file.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, "", changed(f.FileName, err)
	}
	now := newFileStat(f.FileName, &st)
	if now.Type != spec.FileTypeFile || now.Dev != f.Dev || now.Ino != f.Ino || now.UID != f.UID {
		return nil, "", changed(f.FileName, fmt.Errorf("it was replaced"))
	}
	b, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, "", changed(f.FileName, err)
	}
	if int64(len(b)) > max {
		return nil, "", tooLarge(f.FileName, int64(len(b)), max)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// openIncludeDir opens includeDir itself, never through a symlink.
func openIncludeDir(s Settings) (*os.File, error) {
	var fd int
	err := ignoringEINTR(func() (err error) {
		fd, err = unix.Open(s.IncludeDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		return nil, unavailable(fmt.Sprintf("includeDir %s cannot be opened as a directory (not through a symlink): %v",
			s.IncludeDir, err))
	}
	return os.NewFile(uintptr(fd), s.IncludeDir), nil
}

// paginate returns the page of sorted names after the name after, of up to
// limit names, and the cursor of the next page: the unpadded base64url of
// the page's last name, as apt's list.
func paginate(names []string, after string, limit int) ([]string, string) {
	if limit <= 0 {
		limit = spec.DefaultLimit
	}
	start, found := slices.BinarySearch(names, after)
	if found {
		start++
	}
	end := min(start+limit, len(names))
	page := names[start:end]
	next := ""
	if end < len(names) {
		next = base64.RawURLEncoding.EncodeToString([]byte(page[len(page)-1]))
	}
	return page, next
}

// decodeCursor is the name a cursor from paginate carries. Any name a
// directory entry can have is a valid position, so the name itself is not
// checked against the site name rule.
func decodeCursor(c string) (string, error) {
	if c == "" {
		return "", nil
	}
	name, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !cursorRE.MatchString(c) || len(name) == 0 || strings.ContainsRune(string(name), '/') {
		return "", invalid("/cursor", "is not a cursor this server issued")
	}
	return string(name), nil
}

func tooLarge(fileName string, size, max int64) *problem.Error {
	return problem.New(problem.ValidationFailed,
		fmt.Sprintf("%s is %d bytes, larger than maxConfigBytes (%d): it is not read, and nothing is truncated",
			fileName, size, max))
}

func changed(fileName string, err error) *problem.Error {
	return unavailable(fmt.Sprintf("%s changed while it was read: %v; retry", fileName, err))
}

func unavailable(detail string) *problem.Error {
	return problem.New(problem.DependencyUnavailable, detail).With("provider", spec.Name)
}

func invalid(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}
