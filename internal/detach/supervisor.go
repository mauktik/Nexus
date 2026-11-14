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

// processCgroup is the cgroup v2 group a ModeProcess supervisor was started
// in, nexus-ops/<id> (cgroup_linux.go).
type processCgroup interface {
	// makeWork creates the work group a KillControlGroup child runs in.
	// The methods below act on it.
	makeWork() error
	// start starts the command newCmd builds in the work group. It returns
	// the started command, why it is not in the group (nil when it is),
	// and why it did not start.
	start(newCmd func() *exec.Cmd) (cmd *exec.Cmd, notPlaced, err error)
	// kill SIGKILLs everything in the work group.
	kill() error
	// signal sends sig to everything in the work group.
	signal(sig syscall.Signal) error
	// populated reports whether the work group has a live process.
	populated() bool
	// release removes the work group, moves the supervisor out of its
	// group and removes that, best effort.
	release() error
}

// con10Note is the one stderr frame of a ModeProcess KillControlGroup
// operation run without a cgroup.
const con10Note = "cgroup v2 unavailable: descendants that leave the process group are not covered (CON-10)"

// signalReady tells the process backend's launcher that this ModeProcess
// supervisor handles SIGTERM (process_linux.go). Off Linux it does nothing.
var signalReady = func() error { return nil }

// ownCgroup is the group of the ModeProcess supervisor of dir, nil when it
// has none: not on Linux, or a launch that could not place it.
var ownCgroup = func(dir string) processCgroup { return nil }

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
// ModeProcess is the same after four steps (Data Flow 1, step 3). It sets
// the umask to 0022 before anything else, the umask systemd gives a
// transient unit, so the child and all it starts create files as they would
// under systemd rather than with nexusd's OpenRC umask 077 (L-47). It caps
// diag, which is supervisor.log, at oplog.DefaultCapBytes. Once its signal
// handlers are installed, it tells the launcher through its readiness pipe,
// so a cancel sent after the launch never meets the default action of
// SIGTERM, which would end it with no exit.json. And it waits for
// supervisor.json to name its own process, exiting 1 without taking the
// spec if that does not happen within LaunchTimeout: a launch that failed
// before recording the identity leaves no work running (L-51).
//
// A ModeProcess supervisor that nexusd placed in nexus-ops/<id> runs a
// KillControlGroup child in nexus-ops/<id>/work. It adds a SIGKILL of that
// group to the end of a cancel or timeout, ends whatever the child left in
// it once the child exits (SIGTERM, the grace period, SIGKILL), as systemd
// stops a KillMode=control-group unit (L-10), and removes its groups after
// exit.json. Without a group it notes in the output that descendants that
// leave the process group are not covered (CON-10). KillProcess children
// are never swept.
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
		if err := signalReady(); err != nil {
			// The launcher sees the pipe close and fails the launch.
			logf("%v", err)
		}
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
	var cg, work processCgroup
	con10 := false
	if mode == ModeProcess {
		cg = ownCgroup(dir)
		if spec.KillMode == KillControlGroup {
			if cg != nil {
				if err := cg.makeWork(); err != nil {
					logf("%v; the child runs without a cgroup", err)
				} else {
					work = cg
				}
			}
			con10 = work == nil
		}
	}
	x := runChild(dir, spec, sigs, logf, work, con10)
	status := 0
	if err := WriteExit(dir, x); err != nil {
		logf("%v", err)
		status = 1
	}
	if cg != nil {
		if err := cg.release(); err != nil {
			logf("cgroup cleanup: %v", err)
		}
	}
	return status
}

// runChild runs spec's child to its end and reports how it ended. With a
// work group the child runs in it, and it is killed and swept with the
// child's process group. con10 writes the CON-10 note to the output first.
func runChild(dir string, spec Spec, sigs <-chan os.Signal, logf func(string, ...any), work processCgroup, con10 bool) Exit {
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
	if con10 {
		// Whole line, so it is one frame of its own before any child output.
		if _, err := io.WriteString(stderr, con10Note+"\n"); err != nil {
			logf("cannot write the output log: %v", err)
		}
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
	newCmd := func() *exec.Cmd {
		cmd := &exec.Cmd{
			Path: spec.Argv[0], Args: spec.Argv, Env: env, Dir: spec.Dir,
			Stdin: nil, Stdout: stdout, Stderr: stderr, // Nil Stdin is /dev/null
			SysProcAttr: &syscall.SysProcAttr{Setsid: true},
			WaitDelay:   PipeWaitDelay,
		}
		if c := spec.Credential; c != nil {
			cmd.SysProcAttr.Credential = &syscall.Credential{Uid: c.UID, Gid: c.GID, Groups: c.Groups}
		}
		return cmd
	}

	started := time.Now()
	var cmd *exec.Cmd
	if work != nil {
		var notPlaced error
		cmd, notPlaced, err = work.start(newCmd)
		if notPlaced != nil {
			logf("the child is not in its work cgroup: %v", notPlaced)
		}
	} else {
		cmd = newCmd()
		err = cmd.Start()
	}
	if err != nil {
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
			killWork(work, logf)
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
				killWork(work, logf)
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
	if work != nil {
		sweep(work, spec.Grace, logf)
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

// killWork SIGKILLs everything in the child's work group, if it has one.
func killWork(work processCgroup, logf func(string, ...any)) {
	if work == nil {
		return
	}
	if err := work.kill(); err != nil {
		logf("cannot kill the work cgroup: %v", err)
	}
}

// sweepSettle bounds how long sweep waits for SIGKILLed processes to leave
// the work group.
const sweepSettle = time.Second

// sweep ends what the child left in its work group once it exited:
// SIGTERM, up to the grace period for it to go, then SIGKILL (L-10).
func sweep(work processCgroup, grace time.Duration, logf func(string, ...any)) {
	if !work.populated() {
		return
	}
	if err := work.signal(syscall.SIGTERM); err != nil {
		logf("cannot end the processes left in the work cgroup: %v", err)
	}
	for deadline := time.Now().Add(grace); work.populated() && time.Now().Before(deadline); {
		time.Sleep(groupPoll)
	}
	if !work.populated() {
		return
	}
	killWork(work, logf)
	for deadline := time.Now().Add(sweepSettle); work.populated() && time.Now().Before(deadline); {
		time.Sleep(groupPoll)
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
