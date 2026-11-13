package nginx

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/providers/nginx/spec"
)

// managedUID is the owner a managed file has: root. Only this package's
// tests change it, to their own UID, because T0 cannot create root-owned
// files (test-strategy.md). It is deliberately not reachable from Options,
// so no production caller can weaken the rule.
var managedUID uint32

// maxNameLength is the longest site name NamePattern admits.
const maxNameLength = 128

var nameRE = regexp.MustCompile(spec.NamePattern)

// ValidName applies the FR-12 name rule with the configured suffixes. The
// error names the rule the name broke.
func ValidName(name string, s Settings) error {
	switch {
	case name == "":
		return errors.New("a site name must not be empty")
	case !isAlnum(name[0]):
		return errors.New("a site name must start with a letter or digit")
	case len(name) > maxNameLength:
		return fmt.Errorf("a site name must be at most %d characters", maxNameLength)
	case !nameRE.MatchString(name):
		return errors.New("a site name may hold only letters, digits, '.', '_' and '-'")
	case strings.Contains(name, ".."):
		return errors.New(`a site name must not contain ".."`)
	case s.IncludeSuffix != "" && strings.HasSuffix(name, s.IncludeSuffix):
		return fmt.Errorf("a site name must not end in the include suffix %s: name the site without it", s.IncludeSuffix)
	case s.DisabledSuffix != "" && strings.HasSuffix(name, s.DisabledSuffix):
		return fmt.Errorf("a site name must not end in the disabled suffix %s: name the site without it", s.DisabledSuffix)
	}
	return nil
}

func isAlnum(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// fileStat is one directory entry as fstatat(AT_SYMLINK_NOFOLLOW) saw it:
// the entry itself, never what a symlink points to.
type fileStat struct {
	FileName string
	Type     string // spec.FileType*
	UID      uint32
	Size     int64
	ModTime  time.Time
	Dev, Ino uint64 // Identity, to check that a later open found this entry
}

// site is a site name's classification: its enabled and disabled entries
// (nil when absent), whether both exist, and whether the provider manages
// it, with the reason when not.
type site struct {
	Name     string
	Enabled  *fileStat
	Disabled *fileStat
	Conflict bool
	Managed  bool
	Reason   string // spec.Reason*; empty when Managed
}

// Exists reports whether either entry exists.
func (s site) Exists() bool { return s.Enabled != nil || s.Disabled != nil }

// File is the entry the site's properties come from: the enabled one when
// it exists (so a conflict shows the file nginx reads), else the disabled
// one.
func (s site) File() *fileStat {
	if s.Enabled != nil {
		return s.Enabled
	}
	return s.Disabled
}

// classify reads name's two entries in the directory dirfd, without
// following symlinks. Only a regular file owned by managedUID, under a
// valid name, with no counterpart in the other state, is managed. The
// reasons are checked in this order: invalid-name, conflict, symlink,
// not-regular, not-root-owned.
func classify(dirfd int, name string, s Settings) (site, error) {
	c := site{Name: name}
	var err error
	if c.Enabled, err = statAt(dirfd, s.EnabledFile(name)); err != nil {
		return site{}, err
	}
	if c.Disabled, err = statAt(dirfd, s.DisabledFile(name)); err != nil {
		return site{}, err
	}
	if !c.Exists() {
		return c, nil
	}
	c.Conflict = c.Enabled != nil && c.Disabled != nil
	f := c.File()
	switch {
	case ValidName(name, s) != nil:
		c.Reason = spec.ReasonInvalidName
	case c.Conflict:
		c.Reason = spec.ReasonConflict
	case f.Type == spec.FileTypeSymlink:
		c.Reason = spec.ReasonSymlink
	case f.Type != spec.FileTypeFile:
		c.Reason = spec.ReasonNotRegular
	case f.UID != managedUID:
		c.Reason = spec.ReasonNotRootOwned
	}
	c.Managed = c.Reason == ""
	return c, nil
}

// statAt is fstatat(dirfd, fileName, AT_SYMLINK_NOFOLLOW): nil when there
// is no such entry.
func statAt(dirfd int, fileName string) (*fileStat, error) {
	var st unix.Stat_t
	err := ignoringEINTR(func() error { return unix.Fstatat(dirfd, fileName, &st, unix.AT_SYMLINK_NOFOLLOW) })
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", fileName, err)
	}
	return newFileStat(fileName, &st), nil
}

func newFileStat(fileName string, st *unix.Stat_t) *fileStat {
	return &fileStat{FileName: fileName, Type: fileType(uint32(st.Mode)), UID: st.Uid, Size: st.Size,
		ModTime: time.Unix(st.Mtim.Unix()).UTC(), Dev: uint64(st.Dev), Ino: st.Ino}
}

func fileType(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return spec.FileTypeFile
	case unix.S_IFLNK:
		return spec.FileTypeSymlink
	case unix.S_IFDIR:
		return spec.FileTypeDirectory
	}
	return spec.FileTypeOther
}

func ignoringEINTR(f func() error) error {
	for {
		if err := f(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
