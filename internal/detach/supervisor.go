package detach

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/oplog"
)

// PipeWaitDelay bounds how long the supervisor waits, after the child
// exits, for its stdout and stderr to close. A background grandchild that
// inherited them cannot hold the operation open (A-10).
const PipeWaitDelay = 3 * time.Second

// groupPoll is how often the supervisor checks whether a terminated
// child's process group is gone before the grace period ends.
const groupPoll = 50 * time.Millisecond

// SuperviseMode is how a supervisor was started.
type SuperviseMode int

const (
	// ModeUnit is the main process of a systemd unit (`op-supervise --dir`).
	ModeUnit SuperviseMode = iota
	// ModeProcess is a supervisor process started by the process backend
	// (`op-supervise --dir --process`).
	ModeProcess
)

// handshakePoll is how often a ModeProcess supervisor looks for its
// identity, and handshakeTimeout how long it looks.
var (
	handshakePoll    = 20 * time.Millisecond
	handshakeTimeout = LaunchTimeout
)

// diagHook, when set by a test, runs with the supervisor's diagnostics
// function once a ModeProcess supervisor has its identity.
var diagHook func(logf func(format string, args ...any))

// Supervise is `nexusd op-supervise --dir dir`, the main process of an
// operation's unit. It reads and unlinks dir/spec.json, runs the child in
// its own session with stdin on /dev/null and stdout and stderr framed into
// dir/output in arrival order, and writes dir/exit.json. On the spec's
// timeout or a SIGTERM (the cancel signal) it sends SIGTERM to the child's
// process group, then SIGKILL after the grace period. It returns the
// process exit status: non-zero, with no child started and no exit.json,
// when there is no spec.
//
// Diagnostics go to diag, the unit's journal.
//
// ModeProcess is the same after three steps (Data Flow 1, step 3). It sets
// the umask to 0022 before anything else, the umask systemd gives a
// transient unit, so the child and all it starts create files as they would
// under systemd rather than with nexusd's OpenRC umask 077 (L-47). It caps
// diag, which is supervisor.log, at oplog.DefaultCapBytes. And it waits for
// supervisor.json to name its own process, exiting 1 without taking the
// spec if that does not happen within LaunchTimeout: a launch that failed
// before recording the identity leaves no work running (L-51).
func Supervise(dir string, diag io.Writer, mode SuperviseMode) int {
	if mode == ModeProcess {
		syscall.Umask(0o022)
		diag = &cappedWriter{w: diag, left: oplog.DefaultCapBytes}
	}
	// SIGHUP and SIGPIPE are caught and dropped rather than set to SIG_IGN:
	// a caught signal is reset to its default across exec, so the child does
	// not inherit an ignored SIGPIPE. Being caught also makes a write to a
	// closed stdout or stderr fail with EPIPE instead of killing the
	// supervisor.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	defer signal.Stop(sigs)
	logf := func(format string, args ...any) {
		// The journal may be gone; there is nowhere else to report that.
		if _, err := fmt.Fprintf(diag, "op-supervise: "+format+"\n", args...); err != nil {
			return
		}
	}

	if mode == ModeProcess {
		if err := awaitIdentity(dir); err != nil {
			logf("%v; nothing was run", err)
			return 1
		}
		if diagHook != nil {
			diagHook(logf)
		}
	}

	spec, err := takeSpec(dir)
	if errors.Is(err, fs.ErrNotExist) {
		logf("no %s in %s: the operation already started once and is not run again", SpecFile, dir)
		return 1
	}
	if err != nil {
		logf("%v", err)
		return 1
	}
	x := runChild(dir, spec, sigs, logf)
	if err := WriteExit(dir, x); err != nil {
		logf("%v", err)
		return 1
	}
	return 0
}

// runChild runs spec's child to its end and reports how it ended.
func runChild(dir string, spec Spec, sigs <-chan os.Signal, logf func(string, ...any)) Exit {
	// A SIGTERM that arrived before the child exists cancels the operation
	// without starting it.
	for pending := true; pending; {
		select {
		case s := <-sigs:
			if s == syscall.SIGTERM {
				now := time.Now()
				return Exit{ExitCode: -1, Started: now, Ended: now, Canceled: true}
			}
		default:
			pending = false
		}
	}

	var stdout, stderr io.Writer = io.Discard, io.Discard
	w, err := oplog.Open(filepath.Join(dir, OutputDir), oplog.Options{CapBytes: spec.OutputCapBytes})
	if err != nil {
		// Output is a convenience; the operation still runs and reports.
		logf("cannot open the output log: %v", err)
	} else {
		report := func(err error) { logf("output log write failed; further output is dropped: %v", err) }
		stdout = &sink{w: w.Stream(apitypes.StreamStdout), report: report}
		stderr = &sink{w: w.Stream(apitypes.StreamStderr), report: report}
	}
	closeLog := func() {
		if w == nil {
			return
		}
		if err := w.Close(); err != nil {
			logf("cannot close the output log: %v", err)
		}
	}

	env := spec.Env
	if env == nil {
		env = []string{} // Non-nil: a nil Env would pass the supervisor's environment on
	}
	cmd := &exec.Cmd{
		Path: spec.Argv[0], Args: spec.Argv, Env: env, Dir: spec.Dir,
		Stdin: nil, Stdout: stdout, Stderr: stderr, // Nil Stdin is /dev/null
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
		WaitDelay:   PipeWaitDelay,
	}
	if c := spec.Credential; c != nil {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: c.UID, Gid: c.GID, Groups: c.Groups}
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		closeLog()
		return Exit{ExitCode: -1, Started: started, Ended: time.Now(), Error: err.Error()}
	}
	// Setsid makes the child a session and process group leader.
	pgid := cmd.Process.Pid
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var x Exit
	var timeout, kill <-chan time.Time
	if spec.Timeout > 0 {
		t := time.NewTimer(spec.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	var killTimer *time.Timer
	terminating, killed := false, false
	terminate := func() {
		if terminating {
			return
		}
		terminating = true
		signalGroup(pgid, syscall.SIGTERM, logf)
		killTimer = time.NewTimer(spec.Grace)
		kill = killTimer.C
	}
	var waitErr error
loop:
	for {
		select {
		case waitErr = <-waited:
			break loop
		case <-timeout:
			x.TimedOut = true
			terminate()
		case s := <-sigs:
			if s == syscall.SIGTERM {
				x.Canceled = true
				terminate()
			}
		case <-kill:
			signalGroup(pgid, syscall.SIGKILL, logf)
			killed, kill = true, nil
		}
	}
	x.Started, x.Ended = started, time.Now()

	// The child is gone, but a termination in progress still reaches the
	// rest of its group: SIGKILL at the end of the grace period unless the
	// group is already empty.
	if terminating && !killed {
		for groupAlive(pgid) {
			select {
			case <-kill:
				signalGroup(pgid, syscall.SIGKILL, logf)
				killed = true
			case <-time.After(groupPoll):
				continue
			}
			break
		}
	}
	if killTimer != nil {
		killTimer.Stop()
	}
	closeLog()

	x.ExitCode = -1
	if ps := cmd.ProcessState; ps != nil {
		x.ExitCode = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			x.Signal = signalName(ws.Signal())
		}
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) {
		logf("waiting for the child: %v", waitErr)
	}
	return x
}

// signalGroup sends sig to every process of group pgid. An empty group is
// not an error.
func signalGroup(pgid int, sig syscall.Signal, logf func(string, ...any)) {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		logf("cannot send %s to process group %d: %v", signalName(sig), pgid, err)
	}
}

// groupAlive reports whether process group pgid still has a member.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// signalNames are the signals named in exit.json and passed to systemctl
// kill --signal=, which takes the well-known specifiers.
var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGILL: "SIGILL", syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT",
	syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE", syscall.SIGKILL: "SIGKILL",
	syscall.SIGUSR1: "SIGUSR1", syscall.SIGSEGV: "SIGSEGV", syscall.SIGUSR2: "SIGUSR2",
	syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM", syscall.SIGTERM: "SIGTERM",
}

// signalName is the specifier of sig, or its number when it has none here.
func signalName(sig syscall.Signal) string {
	if n, ok := signalNames[sig]; ok {
		return n
	}
	return fmt.Sprintf("%d", int(sig))
}

// sink passes output to the output log until a write fails, then drops it.
// It never fails a write, so the copy from the child's pipe keeps draining
// and the child never blocks on a full pipe.
type sink struct {
	w      io.Writer
	failed atomic.Bool
	report func(error)
}

func (s *sink) Write(p []byte) (int, error) {
	if s.failed.Load() {
		return len(p), nil
	}
	if _, err := s.w.Write(p); err != nil && s.failed.CompareAndSwap(false, true) {
		s.report(err)
	}
	return len(p), nil
}

// awaitIdentity waits until dir/supervisor.json names this process in this
// boot, polling every handshakePoll for up to handshakeTimeout.
func awaitIdentity(dir string) error {
	deadline := time.Now().Add(handshakeTimeout)
	for {
		id, err := ReadIdentity(dir)
		switch {
		case err == nil && id.PID == os.Getpid():
			return verifySelf(id)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			// supervisor.json is written by atomic rename: a bad one stays bad.
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not name this process (%d) within %s", IdentityFile, os.Getpid(), handshakeTimeout)
		}
		time.Sleep(handshakePoll)
	}
}

// verifySelf checks that id, which names this PID, also has this process's
// start time and this boot.
func verifySelf(id Identity) error {
	st, err := procStat("/proc", os.Getpid())
	if err != nil {
		return err
	}
	boot, err := bootID("/proc")
	if err != nil {
		return err
	}
	if id.StartTicks != st.StartTicks || id.BootID != boot {
		return fmt.Errorf("%s names pid %d but not this process: %+v", IdentityFile, id.PID, id)
	}
	return nil
}

// cappedWriter passes at most left bytes on, then one truncation line, then
// nothing. It drops a write that does not fit whole, so every line it
// passes is complete.
type cappedWriter struct {
	mu        sync.Mutex
	w         io.Writer
	left      int64
	truncated bool
}

// truncationLine ends a capped supervisor.log.
const truncationLine = "op-supervise: diagnostics truncated: supervisor.log reached its cap\n"

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return len(p), nil
	}
	if int64(len(p)) > c.left {
		c.truncated = true
		if _, err := io.WriteString(c.w, truncationLine); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	n, err := c.w.Write(p)
	c.left -= int64(n)
	return n, err
}
