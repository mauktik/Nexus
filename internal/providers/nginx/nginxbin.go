package nginx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

// DefaultNginx is the nginx binary on both supported distributions (FR-2a).
const DefaultNginx = "/usr/sbin/nginx"

// VersionTimeout bounds one `nginx -v` (NFR-2, NFR-4): a hung binary yields
// an empty version, never a hung status.
const VersionTimeout = 5 * time.Second

const (
	// versionOutputCap is the most output kept from `nginx -v`, which prints
	// one short line.
	versionOutputCap = 4 << 10
	// versionRetry is how long a failed `nginx -v` of an unchanged binary is
	// remembered before it is tried again, so a binary that hangs does not
	// cost every status call the full bound.
	versionRetry = time.Minute
	// waitDelay bounds the wait for the output pipe after nginx exited or
	// was killed, in case a descendant outside its process group holds it.
	waitDelay = time.Second
)

// versionRE takes the version from `nginx version: nginx/1.26.3` (Alpine)
// or `nginx version: nginx/1.28.3 (Ubuntu)`, which nginx prints on stderr.
var versionRE = regexp.MustCompile(`nginx/([0-9.]+)`)

// env is the whole environment nginx runs with: no variable of nexusd's
// reaches it.
func env() []string { return []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"} }

// nginxBin runs the configured nginx binary: an absolute path, executed
// directly with a fixed environment, no shell and no stdin, in a process
// group of its own that is killed whole when the bound expires.
type nginxBin struct {
	path string
}

// output runs nginx with args, bounded by timeout, and returns its combined
// output, capped at max, and its exit code.
func (n nginxBin) output(ctx context.Context, timeout time.Duration, max int, args ...string) ([]byte, int, error) {
	if !filepath.IsAbs(n.path) {
		return nil, 0, fmt.Errorf("refusing to run nginx %q: the path must be absolute", n.path)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &capWriter{max: max}
	cmd := exec.CommandContext(ctx, n.path, args...)
	cmd.Env = env()
	cmd.Dir = "/"
	// One writer for both streams: exec shares one pipe and one copying
	// goroutine, so the writer needs no lock.
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.buf, 0, fmt.Errorf("%s %v did not finish within %s", n.path, args, timeout)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		return out.buf, cmd.ProcessState.ExitCode(), nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return out.buf, exitErr.ExitCode(), nil
	}
	return out.buf, 0, err
}

// Version runs `nginx -v` and parses the version from its output.
func (n nginxBin) Version(ctx context.Context, timeout time.Duration) (string, error) {
	out, code, err := n.output(ctx, timeout, versionOutputCap, "-v")
	if err != nil {
		return "", err
	}
	m := versionRE.FindSubmatch(out)
	if code != 0 || m == nil {
		return "", fmt.Errorf("%s -v exited %d without a version", n.path, code)
	}
	return string(m[1]), nil
}

// capWriter keeps the first max bytes written to it and discards the rest,
// so nginx never blocks on a full pipe.
type capWriter struct {
	buf []byte
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// binaryKey identifies a build of the binary: an upgrade replaces the file,
// which changes its size or modification time.
type binaryKey struct {
	path  string
	size  int64
	mtime time.Time
}

// versionCache holds the version of the binary last asked, by binaryKey.
// It runs nothing in the background: a status call that finds the binary
// changed runs `nginx -v` again.
type versionCache struct {
	mu      sync.Mutex
	key     binaryKey
	have    bool
	version string
	retryAt time.Time // For a failed run: when to try the same binary again
}

// version returns the version of the nginx at path, or "" when it cannot be
// read within timeout, or within three quarters of ctx's remaining time if
// that is shorter. A failure is remembered (for versionRetry) only when the
// full timeout applied, so a run cut short by a tight caller is retried.
func (c *versionCache) get(ctx context.Context, path string, timeout time.Duration) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	key := binaryKey{path: path, size: fi.Size(), mtime: fi.ModTime()}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && c.key == key && (c.version != "" || time.Now().Before(c.retryAt)) {
		return c.version
	}
	bound := timeout
	if dl, ok := ctx.Deadline(); ok {
		// Leave the caller's deadline room to report: a preflight bounded
		// at a second must still answer, with an empty version.
		bound = min(bound, time.Until(dl)*3/4)
	}
	v, err := nginxBin{path: path}.Version(ctx, bound)
	switch {
	case err == nil:
		c.key, c.have, c.version, c.retryAt = key, true, v, time.Time{}
	case bound == timeout:
		c.key, c.have, c.version, c.retryAt = key, true, "", time.Now().Add(versionRetry)
	}
	return v
}
