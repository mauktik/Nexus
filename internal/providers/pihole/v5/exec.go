package v5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// env is the exact environment of every command the helper runs (A-29): the
// Pi-hole scripts need PATH for their own tools and HOME for sqlite3, and
// run in the C locale. There is no TERM.
var env = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"LC_ALL=C",
	"HOME=/root",
}

// Env returns a copy of the exact environment of the helper and of every
// command it runs.
func Env() []string { return slices.Clone(env) }

// execWaitDelay bounds how long Run waits for the child's output to drain
// after the child ended or was killed: a grandchild that inherited a pipe
// must not hold the helper.
const execWaitDelay = 5 * time.Second

// Exec is the real Runner: argv[0] is an absolute path executed directly,
// with Env() and no shell. A stdout or stderr that is an *os.File, such as
// the helper's own, is handed to the child as is, so the supervisor frames
// the child's output live (FR-13).
//
// Its exit reporting contract (A-19):
//   - a normal exit is (status, nil);
//   - a death by signal is (128+signal, nil), the shell convention, so a
//     SIGSEGV is 139, and a line naming the signal goes to stderr;
//   - a child that could not be started, or whose ctx ended, is (-1, err),
//     err wrapping ctx.Err() in the second case.
//
// Only (0, nil) is success.
type Exec struct{}

var _ Runner = Exec{}

// Run runs argv to its end.
func (Exec) Run(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return -1, fmt.Errorf("pihole: command %q is not an absolute path", argv)
	}
	if err := ctx.Err(); err != nil {
		return -1, fmt.Errorf("pihole: %s not started: %w", argv[0], err)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = Env()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = execWaitDelay
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("pihole: start %s: %w", argv[0], err)
	}
	werr := cmd.Wait()
	ps := cmd.ProcessState
	if ps == nil {
		return -1, fmt.Errorf("pihole: %s: %w", argv[0], werr)
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok {
		return -1, fmt.Errorf("pihole: %s: unknown wait status %T", argv[0], ps.Sys())
	}
	if ws.Signaled() {
		if ctx.Err() != nil {
			// The kill was ours: the context ended.
			return -1, fmt.Errorf("pihole: %s stopped: %w", argv[0], ctx.Err())
		}
		// Go's ExitCode() is -1 for a death by signal; the contract is the
		// shell's 128+signal, as `pihole-FTL --test` reported 139 (WI 0.1).
		fmt.Fprintf(stderr, "pihole: %s killed by signal %s\n", argv[0], SignalName(ws.Signal()))
		return 128 + int(ws.Signal()), nil
	}
	if werr != nil && !errors.Is(werr, exec.ErrWaitDelay) {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			return -1, fmt.Errorf("pihole: %s: %w", argv[0], werr)
		}
	}
	return ws.ExitStatus(), nil
}

// signalNames are the specifiers a signal is named by in the oplog.
var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGILL: "SIGILL", syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT",
	syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE", syscall.SIGKILL: "SIGKILL",
	syscall.SIGUSR1: "SIGUSR1", syscall.SIGSEGV: "SIGSEGV", syscall.SIGUSR2: "SIGUSR2",
	syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM", syscall.SIGTERM: "SIGTERM",
}

// SignalName is sig's specifier, such as SIGSEGV, or its number when it has
// none here.
func SignalName(sig syscall.Signal) string {
	if n, ok := signalNames[sig]; ok {
		return n
	}
	return fmt.Sprintf("%d", int(sig))
}
