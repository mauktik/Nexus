package nginx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
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

// TestOutputCap is the most of `nginx -t`'s output a result holds (FR-4).
// The operation log has all of it, up to the output cap.
const TestOutputCap = 64 << 10

// errTimedOut marks a run of nginx that its bound stopped.
var errTimedOut = errors.New("nginx did not finish in time")

// nginxEnv is the whole environment nginx runs with: no variable of
// nexusd's reaches it.
func nginxEnv() []string { return []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"} }

// nginxBin runs the configured nginx binary: an absolute path, executed
// directly with a fixed environment, no shell and no stdin, in a process
// group of its own that is killed whole when the bound expires or ctx ends.
type nginxBin struct {
	path string
	// testTimeout bounds Test, and log receives Test's output as nginx
	// writes it: in the helper, the operation log. A nil log discards it.
	testTimeout time.Duration
	log         io.Writer
}

// run runs nginx with args, bounded by timeout, writing its combined output
// to w, and returns its exit code. The error wraps errTimedOut when the
// bound stopped it.
func (n nginxBin) run(ctx context.Context, timeout time.Duration, w io.Writer, args ...string) (int, error) {
	if !filepath.IsAbs(n.path) {
		return 0, fmt.Errorf("refusing to run nginx %q: the path must be absolute", n.path)
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, n.path, args...)
	cmd.Env = nginxEnv()
	cmd.Dir = "/"
	// One writer for both streams: exec shares one pipe and one copying
	// goroutine, so the writer needs no lock.
	cmd.Stdout, cmd.Stderr = w, w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return 0, fmt.Errorf("%s %v was stopped: %w", n.path, args, ctx.Err())
	case bounded.Err() != nil:
		return 0, fmt.Errorf("%s %v did not finish within %s: %w", n.path, args, timeout, errTimedOut)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		return cmd.ProcessState.ExitCode(), nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// output runs nginx with args, bounded by timeout, and returns its combined
// output, capped at max, and its exit code.
func (n nginxBin) output(ctx context.Context, timeout time.Duration, max int, args ...string) ([]byte, int, error) {
	out := &capWriter{max: max}
	code, err := n.run(ctx, timeout, out, args...)
	return out.buf, code, err
}

// TestOutput is one `nginx -t`: its verdict and its combined output, at
// most TestOutputCap bytes.
type TestOutput struct {
	Valid     bool // nginx -t exited 0
	Output    string
	Truncated bool // The output was longer than TestOutputCap
}

// Test runs `nginx -t`, bounded by testTimeout. Its output goes to the log
// as nginx writes it, and the first TestOutputCap bytes into the result.
// An invalid configuration is a TestOutput with Valid false; the error is
// for a test that could not run or did not finish.
func (n nginxBin) Test(ctx context.Context) (TestOutput, error) {
	w := &capWriter{max: TestOutputCap, tee: n.log}
	code, err := n.run(ctx, n.testTimeout, w, "-t")
	var pathErr *fs.PathError
	switch {
	case errors.Is(err, errTimedOut):
		return TestOutput{}, fmt.Errorf("nginx -t did not finish within %s", n.testTimeout)
	case errors.As(err, &pathErr):
		return TestOutput{}, fmt.Errorf("the nginx binary %s cannot be run: %v", n.path, pathErr.Err)
	case err != nil:
		return TestOutput{}, fmt.Errorf("nginx -t could not run: %v", err)
	}
	// A failed write to the log only stops the copy there (capWriter): the
	// result still has the output, which is what the test reports.
	return TestOutput{Valid: code == 0, Output: string(validUTF8Prefix(w.buf)), Truncated: w.dropped}, nil
}

// validUTF8Prefix drops a multi-byte character the cap cut in two, so the
// output stays valid UTF-8 through JSON.
func validUTF8Prefix(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
		if utf8.Valid(b) {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
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
// so nginx never blocks on a full pipe. With tee set it also copies every
// write there, until the first write to tee fails.
type capWriter struct {
	buf     []byte
	max     int
	dropped bool // More than max bytes were written
	tee     io.Writer
	teeErr  error // The failed write to tee, after which nothing more is copied
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.max - len(w.buf)
	if len(p) > room {
		w.dropped = true
	}
	if room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	if w.tee != nil && w.teeErr == nil {
		if _, err := w.tee.Write(p); err != nil {
			w.teeErr = err
		}
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
