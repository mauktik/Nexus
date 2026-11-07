package apt

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

var (
	nameRE   = regexp.MustCompile(spec.NamePattern)
	cursorRE = regexp.MustCompile(spec.CursorPattern)
)

// policyBatch bounds the names of one apt-cache policy call.
const policyBatch = 500

func (p *Provider) list(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Package], error) {
	var none apitypes.List[spec.Package]
	after, err := decodeCursor(in.Cursor)
	if err != nil {
		return none, err
	}
	installed, err := p.installed(ctx)
	if err != nil {
		return none, err
	}
	page, next := paginate(installed, after, in.Limit, func(i installedPackage) string { return i.Name })
	out := apitypes.List[spec.Package]{Items: make([]spec.Package, len(page)), NextCursor: next}
	for i, pkg := range page {
		out.Items[i] = apitypes.NewResource(p.host, spec.Name, spec.ResourceType, pkg.Name, pkg.properties())
	}
	return out, nil
}

func (p *Provider) listUpgradable(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Upgradable], error) {
	var none apitypes.List[spec.Upgradable]
	after, err := decodeCursor(in.Cursor)
	if err != nil {
		return none, err
	}
	all, err := p.upgradableAll(ctx)
	if err != nil {
		return none, err
	}
	page, next := paginate(all, after, in.Limit, func(u spec.Upgradable) string { return u.Name })
	return apitypes.List[spec.Upgradable]{Items: slices.Clone(page), NextCursor: next}, nil
}

// get resolves the name exactly before it reaches apt-cache: apt reads a
// name it has no package for as a regular expression, and "+" and "." are
// legal in package names (A-28; captured in testdata, fixture-policy-regex).
// dpkg-query reads the name as a glob, and a valid name holds no glob
// character.
func (p *Provider) get(ctx context.Context, in spec.PackageInput) (spec.PackageDetail, error) {
	var none spec.PackageDetail
	if !nameRE.MatchString(in.Name) || len(in.Name) > spec.MaxNameLength {
		return none, invalid("/name", "must match "+spec.NamePattern)
	}
	native, err := p.architecture(ctx)
	if err != nil {
		return none, unavailable(err.Error())
	}
	base, arch, _ := strings.Cut(in.Name, ":")
	if arch == native {
		arch = ""
	}
	name := base
	if arch != "" {
		name = base + ":" + arch
	}

	props := spec.PackageDetailProperties{PackageProperties: spec.PackageProperties{Package: base, Status: "not-installed"}}
	inDpkg := false
	res, err := p.run(ctx, cmdDpkgQuery(name))
	switch {
	case err != nil:
		return none, p.failed("dpkg-query", res, err)
	case res.ExitCode == 0:
		for _, d := range parseDpkgQuery(res.Stdout) {
			if d.Package != base || !archMatches(d.Architecture, arch, native) {
				continue
			}
			inDpkg = true
			props.Architecture, props.Status = d.Architecture, d.Status
			if listed(d.Status) {
				props.InstalledVersion = d.Version
			}
			break
		}
	case res.ExitCode != 1: // 1: no package matched
		return none, p.failed("dpkg-query", res, nil)
	}

	res, err = p.run(ctx, cmdPkgNames(base))
	if err != nil || res.ExitCode != 0 {
		return none, p.failed("apt-cache pkgnames", res, err)
	}
	inApt := slices.Contains(parsePkgNames(res.Stdout), base)
	if inApt {
		// The base name exists; an architecture apt has no such package
		// for prints nothing here.
		res, err = p.run(ctx, cmdPolicy(name))
		if err != nil || res.ExitCode != 0 {
			return none, p.failed("apt-cache policy", res, err)
		}
		var e policyEntry
		e, inApt = parsePolicy(res.Stdout)[name]
		props.CandidateVersion = e.Candidate
	}
	if !inDpkg && !inApt {
		return none, problem.New(problem.NotFound, "neither apt nor dpkg knows a package of that name")
	}
	if inApt {
		res, err = p.run(ctx, cmdShow(name))
		if err != nil || res.ExitCode != 0 {
			return none, p.failed("apt-cache show", res, err)
		}
		for _, st := range parseStanzas(res.Stdout) {
			if st["Package"] != base {
				continue
			}
			if props.Architecture == "" {
				props.Architecture = st["Architecture"]
			}
			props.Section, props.Priority = st["Section"], st["Priority"]
			props.Maintainer, props.Homepage, props.Source = st["Maintainer"], st["Homepage"], st["Source"]
			props.Summary, props.Description, _ = strings.Cut(st["Description"], "\n")
			break
		}
	}
	props.Upgradable = props.InstalledVersion != "" && props.CandidateVersion != "" &&
		props.CandidateVersion != props.InstalledVersion
	return apitypes.NewResource(p.host, spec.Name, spec.ResourceType, name, props), nil
}

// archMatches reports whether a dpkg row of architecture got is the
// package asked for: the named foreign architecture, or else the native one
// or all.
func archMatches(got, want, native string) bool {
	if want != "" {
		return got == want
	}
	return got == native || got == "all"
}

// installedPackage is one listed package with its resource name.
type installedPackage struct {
	Name string // Package, or Package:arch for a foreign architecture
	dpkgPackage
}

func (i installedPackage) properties() spec.PackageProperties {
	return spec.PackageProperties{Package: i.Package, Architecture: i.Architecture, InstalledVersion: i.Version,
		Status: i.Status}
}

// installed lists dpkg's packages in a listed state, sorted by name.
func (p *Provider) installed(ctx context.Context) ([]installedPackage, error) {
	native, err := p.architecture(ctx)
	if err != nil {
		return nil, unavailable(err.Error())
	}
	res, err := p.run(ctx, cmdDpkgQueryAll())
	if err != nil || res.ExitCode != 0 {
		return nil, p.failed("dpkg-query", res, err)
	}
	var out []installedPackage
	seen := map[string]bool{}
	for _, d := range parseDpkgQuery(res.Stdout) {
		if !listed(d.Status) {
			continue
		}
		name := d.Package
		if d.Architecture != native && d.Architecture != "all" {
			name += ":" + d.Architecture
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, installedPackage{Name: name, dpkgPackage: d})
	}
	slices.SortFunc(out, func(a, b installedPackage) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// upgradableCache holds the last computed upgradable list with the state of
// the files it was computed from. apt-get -s takes about a second, against
// the 200 ms read target (A-38), so the list is recomputed only when one of
// those files changed.
type upgradableCache struct {
	mu    sync.Mutex
	key   string
	items []spec.Upgradable
}

// upgradableAll returns every upgradable package, sorted by name.
func (p *Provider) upgradableAll(ctx context.Context) ([]spec.Upgradable, error) {
	c := &p.upgradable
	c.mu.Lock()
	defer c.mu.Unlock()
	// The key is taken before the commands run: a change while they run
	// leaves a stale key, so the next read recomputes.
	key := p.stateKey()
	if c.items != nil && c.key == key {
		return c.items, nil
	}
	items, err := p.computeUpgradable(ctx)
	if err != nil {
		return nil, err
	}
	c.key, c.items = key, items
	return items, nil
}

// computeUpgradable is the installed packages whose apt-cache policy
// Candidate differs from Installed, each classified by the origins of its
// Inst line in a full-upgrade simulation. A package the simulation does
// not upgrade, such as one kept back, has unknown security.
func (p *Provider) computeUpgradable(ctx context.Context) ([]spec.Upgradable, error) {
	installed, err := p.installed(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(installed))
	for i, pkg := range installed {
		names[i] = pkg.Name
	}
	policy := map[string]policyEntry{}
	for chunk := range slices.Chunk(names, policyBatch) {
		res, err := p.run(ctx, cmdPolicy(chunk...))
		if err != nil || res.ExitCode != 0 {
			return nil, p.failed("apt-cache policy", res, err)
		}
		for k, v := range parsePolicy(res.Stdout) {
			policy[k] = v
		}
	}

	insts := map[string]simAction{}
	res, err := p.run(ctx, cmdSimulate())
	switch {
	case err != nil:
		return nil, p.failed("apt-get -s", res, err)
	case res.ExitCode != 0:
		// Unmet dependencies make the simulation fail. The versions still
		// stand; only the security classification is lost.
		p.log.Warn("apt-get -s dist-upgrade failed; security is unknown for every package",
			"exitCode", res.ExitCode, "stderr", firstLine(res.Stderr))
	default:
		for _, a := range parseSimulation(res.Stdout) {
			if a.Op == "Inst" {
				insts[a.Name] = a
			}
		}
	}

	out := []spec.Upgradable{}
	for _, pkg := range installed {
		e, ok := policy[pkg.Name]
		if !ok || e.Installed == "" || e.Candidate == "" || e.Candidate == e.Installed {
			continue
		}
		props := spec.UpgradableProperties{Package: pkg.Package, Architecture: pkg.Architecture,
			InstalledVersion: e.Installed, CandidateVersion: e.Candidate, Security: spec.SecurityUnknown,
			Origins: []string{}}
		if a, ok := insts[pkg.Name]; ok && a.NewVersion == e.Candidate {
			props.Origins = slices.Clone(a.Origins)
			if props.Origins == nil {
				props.Origins = []string{}
			}
			props.Security = classifySecurity(a.Origins)
		}
		// The id and type are the package's, so a client follows it to get.
		r := apitypes.NewResource(p.host, spec.Name, spec.ResourceType, pkg.Name, props)
		out = append(out, r)
	}
	return out, nil
}

// watchedFiles decide whether the upgradable list is still valid: dpkg's
// status (any install, upgrade or removal, by Nexus or not), the package
// indexes (a refresh), and the sources and preferences (pins), each with
// the entries of its directory.
var watchedFiles = []string{
	"/var/lib/dpkg/status",
	"/var/lib/apt/lists",
	"/etc/apt/preferences",
	"/etc/apt/preferences.d",
	"/etc/apt/sources.list",
	"/etc/apt/sources.list.d",
}

// stateKey is the stat of every watched file and of each entry of a watched
// directory: path, inode, size, mode and modification time in nanoseconds,
// or absent.
func (p *Provider) stateKey() string {
	var b strings.Builder
	stat := func(path string) {
		fi, err := os.Stat(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(&b, "%s error %v\n", path, err)
				return
			}
			fmt.Fprintf(&b, "%s absent\n", path)
			return
		}
		fmt.Fprintf(&b, "%s %d %d %v %d\n", path, inode(fi), fi.Size(), fi.Mode(), fi.ModTime().UnixNano())
	}
	for _, f := range watchedFiles {
		path := p.hostPath(f)
		stat(path)
		if f == "/var/lib/apt/lists" {
			continue // Its directory's time changes on every index file replaced.
		}
		if entries, err := os.ReadDir(path); err == nil {
			for _, e := range entries {
				stat(path + "/" + e.Name())
			}
		}
	}
	return b.String()
}

func inode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// paginate returns the page after the item named after, of up to limit
// items, and the cursor of the next page.
func paginate[T any](items []T, after string, limit int, name func(T) string) ([]T, string) {
	if limit <= 0 {
		limit = spec.DefaultLimit
	}
	start, found := slices.BinarySearchFunc(items, after, func(it T, n string) int { return strings.Compare(name(it), n) })
	if found {
		start++
	}
	end := min(start+limit, len(items))
	page := items[start:end]
	next := ""
	if end < len(items) {
		next = base64.RawURLEncoding.EncodeToString([]byte(name(page[len(page)-1])))
	}
	return page, next
}

func decodeCursor(c string) (string, error) {
	if c == "" {
		return "", nil
	}
	name, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !cursorRE.MatchString(c) || !nameRE.Match(name) {
		return "", invalid("/cursor", "is not a cursor this server issued")
	}
	return string(name), nil
}

// failed logs a command failure and answers dependency-unavailable with a
// summary: the exit status and apt's first error line.
func (p *Provider) failed(what string, res result, err error) error {
	p.log.Warn("apt command failed", "command", what, "exitCode", res.ExitCode, "error", fmt.Sprint(err),
		"stderr", string(res.Stderr))
	return unavailable(fmt.Sprintf("%s failed: %s", what, failure(res, err)))
}

func failure(res result, err error) string {
	if err != nil {
		return err.Error()
	}
	s := fmt.Sprintf("exit status %d", res.ExitCode)
	if l := firstLine(res.Stderr); l != "" {
		s += ": " + l
	}
	return s
}

func firstLine(b []byte) string {
	for line := range lines(b) {
		if l := strings.TrimSpace(line); l != "" {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	return ""
}

func unavailable(detail string) *problem.Error {
	return problem.New(problem.DependencyUnavailable, detail).With("provider", spec.Name)
}

func invalid(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}
