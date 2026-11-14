package nginx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The pid directory. On Alpine /run is a tmpfs, and /run/nginx, where
// nginx's pid file is, is made only by the init script's start_pre
// (`checkpath --directory --owner nginx:nginx ${pidfile%/*}`). Until nginx
// has started once since boot, `nginx -t` fails with `open()
// "/run/nginx/nginx.pid" failed (2: No such file or directory)`, and the
// helper tests before the init system runs. So before each live nginx -t
// the helper makes that directory as the init script would.
//
// The pid path is nginx's compiled-in one, from `nginx -V`. A pid directive
// in the configuration overrides it, and is not read: finding it needs the
// whole configuration with its includes, which only `nginx -T` resolves,
// and that dumps the configuration (OOS-2). The init script makes the
// compiled-in directory too, whatever the configuration says.

const (
	// pidDirMode is the mode of a pid directory the helper makes: checkpath's
	// default for a directory.
	pidDirMode = 0o755
	// buildInfoCap is the most output kept from `nginx -V`, a few lines of
	// build information.
	buildInfoCap = 64 << 10
)

// pidRunDirs are the directories in which the helper makes nginx's missing
// pid directory, one level down. Only tests replace it.
var pidRunDirs = []string{"/run", "/var/run"}

// pidDirOwner is the owner a pid directory the helper makes gets. Only
// tests replace it.
var pidDirOwner = nginxOwner

// nginxOwner is the uid of user nginx and the gid of group nginx, each root
// where there is no such user or group: the init script's `checkpath
// --owner nginx:nginx`.
func nginxOwner() (uid, gid int) {
	if u, err := user.Lookup("nginx"); err == nil {
		if id, err := strconv.Atoi(u.Uid); err == nil {
			uid = id
		}
	}
	if g, err := user.LookupGroup("nginx"); err == nil {
		if id, err := strconv.Atoi(g.Gid); err == nil {
			gid = id
		}
	}
	return uid, gid
}

// pidPathRE takes --pid-path= from nginx's configure arguments. configure
// prints an argument whose value holds a space with the value in single
// quotes (auto/options), and a later --pid-path= overrides an earlier one.
var pidPathRE = regexp.MustCompile(`(?:^|\s)--pid-path=('[^']*'|\S+)`)

// pidPathFromV is the compiled-in pid path in `nginx -V`'s output, or ""
// when its configure arguments have none.
func pidPathFromV(out []byte) string {
	for _, line := range bytes.Split(out, []byte("\n")) {
		args, ok := bytes.CutPrefix(line, []byte("configure arguments:"))
		if !ok {
			continue
		}
		m := pidPathRE.FindAllSubmatch(args, -1)
		if m == nil {
			return ""
		}
		return strings.Trim(string(m[len(m)-1][1]), "'")
	}
	return ""
}

// pidPathCache holds the compiled-in pid path of the binary last asked, by
// binaryKey, as versionCache holds its version.
type pidPathCache struct {
	mu      sync.Mutex
	key     binaryKey
	have    bool
	path    string
	err     error
	retryAt time.Time // For a failed run: when to try the same binary again
}

// pidPaths is the helper's cache of `nginx -V`.
var pidPaths pidPathCache

// get returns the compiled-in pid path of n's binary, "" when it has none
// or the binary cannot be found, which nginx -t then reports. A failed
// `nginx -V` is remembered for versionRetry, unless ctx ended it.
func (c *pidPathCache) get(ctx context.Context, n nginxBin) (string, error) {
	fi, err := os.Stat(n.path)
	if err != nil {
		return "", nil
	}
	key := binaryKey{path: n.path, size: fi.Size(), mtime: fi.ModTime()}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && c.key == key && (c.err == nil || time.Now().Before(c.retryAt)) {
		return c.path, c.err
	}
	out, code, err := n.output(ctx, VersionTimeout, buildInfoCap, "-V")
	if err == nil && code != 0 {
		err = fmt.Errorf("%s -V exited %d", n.path, code)
	}
	if ctx.Err() != nil {
		return "", err
	}
	c.key, c.have, c.err, c.path, c.retryAt = key, true, err, "", time.Time{}
	if err != nil {
		c.retryAt = time.Now().Add(versionRetry)
		return "", err
	}
	c.path = pidPathFromV(out)
	return c.path, nil
}

// ensurePidDir makes the directory of nginx's compiled-in pid file when it
// is missing and is directly in one of pidRunDirs, as the init script
// would, and says so in the log. It never changes an existing entry and
// never fails the test: whatever it cannot do, nginx -t then reports.
func (n nginxBin) ensurePidDir(ctx context.Context) {
	pid, err := pidPaths.get(ctx, n)
	if err != nil {
		n.say("nginx-op: the pid path cannot be read from %s -V (%v); its directory is not checked", n.path, err)
		return
	}
	if !filepath.IsAbs(pid) {
		return // None, or relative to the prefix: never in /run
	}
	dir := filepath.Dir(filepath.Clean(pid))
	runDir := filepath.Dir(dir)
	if dir == runDir || !slices.Contains(pidRunDirs, runDir) {
		return
	}
	created, err := makePidDir(runDir, filepath.Base(dir))
	if created {
		n.say("nginx-op: created missing pid directory %s (as the init script would)", dir)
	}
	if err != nil {
		n.say("nginx-op: the missing pid directory %s was not made ready: %v", dir, err)
	}
}

// say writes a line to the log, if there is one.
func (n nginxBin) say(format string, args ...any) {
	if n.log != nil {
		say(n.log, format, args...)
	}
}

// makePidDir makes name in runDir, mode pidDirMode, owned by pidDirOwner,
// when nothing by that name exists there, symlink or not. created reports
// that it made the directory, even if setting its owner or mode then
// failed.
func makePidDir(runDir, name string) (created bool, err error) {
	fd, err := openRunDir(runDir)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	err = ignoringEINTR(func() error { return unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) })
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, unix.ENOENT):
		return false, fmt.Errorf("%s: %w", name, err)
	}
	uid, gid := pidDirOwner()
	err = ignoringEINTR(func() error { return unix.Mkdirat(fd, name, pidDirMode) })
	switch {
	case errors.Is(err, unix.EEXIST):
		return false, nil // Made meanwhile, by the init system or another test
	case err != nil:
		return false, fmt.Errorf("mkdir %s: %w", name, err)
	}
	var dfd int
	err = ignoringEINTR(func() (err error) {
		dfd, err = unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		return true, fmt.Errorf("open %s: %w", name, err)
	}
	defer unix.Close(dfd)
	if err := unix.Fchown(dfd, uid, gid); err != nil {
		return true, fmt.Errorf("set its owner to %d:%d: %w", uid, gid, err)
	}
	if err := unix.Fchmod(dfd, pidDirMode); err != nil {
		return true, fmt.Errorf("set its mode to %04o: %w", pidDirMode, err)
	}
	return true, nil
}

// openRunDir opens dir, one of pidRunDirs, without following a symlink
// there, except one to another of pidRunDirs: /var/run is a symlink to
// /run on both distributions.
func openRunDir(dir string) (int, error) {
	fd, err := openDirNoFollow(dir)
	if !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		return fd, err
	}
	target, lerr := os.Readlink(dir)
	if lerr != nil {
		return -1, fmt.Errorf("%s: %w", dir, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(dir), target)
	}
	target = filepath.Clean(target)
	if target == dir || !slices.Contains(pidRunDirs, target) {
		return -1, fmt.Errorf("%s is a symbolic link to %s, which is not a run directory", dir, target)
	}
	return openDirNoFollow(target)
}

func openDirNoFollow(dir string) (int, error) {
	var fd int
	err := ignoringEINTR(func() (err error) {
		fd, err = unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", dir, err)
	}
	return fd, nil
}
