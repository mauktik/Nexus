//go:build linux

package detach

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// writeIdentity is WriteIdentity; a test makes it fail.
var writeIdentity = WriteIdentity

// readyFD is the descriptor of a process-backend supervisor that holds the
// write end of its readiness pipe, the launcher's ExtraFiles[0].
const readyFD = 3

func init() { signalReady = linuxSignalReady }

// ProcessBackend returns the four process seams of Options for a host
// without systemd. Launch starts `nexusd op-supervise --dir D --process` in
// its own session and records its identity in supervisor.json; UnitState,
// Signal and ListUnits answer from that identity in the systemd vocabulary
// the launcher and recovery already use, so nothing else changes.
func ProcessBackend(cfg ProcessConfig) Options {
	b := &processBackend{cfg: cfg}
	return Options{Launch: b.launch, UnitState: b.unitState, Signal: b.signal, ListUnits: b.listUnits}
}

type processBackend struct {
	cfg         ProcessConfig
	noCgroupLog sync.Once
}

func (b *processBackend) procRoot() string {
	if b.cfg.ProcRoot == "" {
		return "/proc"
	}
	return b.cfg.ProcRoot
}

func (b *processBackend) log() *slog.Logger {
	if b.cfg.Logger == nil {
		return slog.Default()
	}
	return b.cfg.Logger
}

// dir is the operation directory of unit, a name from Unit.
func (b *processBackend) dir(unit string) (string, error) {
	if b.cfg.OpsDir == "" {
		return "", errors.New("detach: process backend has no ops directory")
	}
	if !unitName.MatchString(unit) {
		return "", fmt.Errorf("detach: %q is not an operation unit name", unit)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(unit, UnitPrefix), ".service")
	return filepath.Join(b.cfg.OpsDir, id), nil
}

// launch starts inv's supervisor, waits until it handles SIGTERM, and
// records its identity. A supervisor waits for that record before it takes
// spec.json, so one whose identity was never written runs nothing and exits
// (L-51).
//
// Until the supervisor has called signal.Notify, a SIGTERM kills it with
// the default action and no exit.json, and the operation would end lost.
// So launch returns only once the supervisor has written a byte to its
// readiness pipe, which it does right after signal.Notify: a cancel sent
// as soon as Start returns is then always handled. /proc/<pid>/status
// SigCgt cannot stand in for this: the Go runtime catches SIGTERM from its
// start, and only exits on it, until signal.Notify.
func (b *processBackend) launch(ctx context.Context, inv Invocation) (err error) {
	dir, err := b.dir(Unit(inv.ID))
	if err != nil {
		return err
	}
	if filepath.Clean(inv.Dir) != dir {
		return fmt.Errorf("detach: operation directory %s is not %s", inv.Dir, dir)
	}
	exe := b.cfg.Executable
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return fmt.Errorf("detach: locate nexusd: %w", err)
		}
	}
	logFile, err := os.OpenFile(filepath.Join(dir, SupervisorLog), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("detach: open %s: %w", SupervisorLog, err)
	}
	defer func() {
		// The supervisor has its own descriptor once started.
		if cerr := logFile.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("detach: close %s: %w", SupervisorLog, cerr)
		}
	}()
	if err := logFile.Chmod(0o600); err != nil {
		return fmt.Errorf("detach: chmod %s: %w", SupervisorLog, err)
	}
	newCmd := func(readyW *os.File) func() *exec.Cmd {
		return func() *exec.Cmd {
			// Not CommandContext: the supervisor must outlive this call and nexusd.
			cmd := exec.Command(exe, "op-supervise", "--dir", dir, "--process")
			cmd.Env = []string{} // Empty, not nil: nothing of nexusd's environment
			cmd.Dir = "/"
			cmd.Stdin = nil // /dev/null
			cmd.Stdout, cmd.Stderr = logFile, logFile
			cmd.ExtraFiles = []*os.File{readyW} // readyFD
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			return cmd
		}
	}
	// The supervisor goes into its own cgroup, out of nexusd's (for
	// OpenRC, openrc.nexusd), so neither a stop of nexusd with
	// rc_cgroup_cleanup="YES" (ASM-14) nor anything else that ends nexusd's
	// group reaches it.
	group := b.cgroup(inv.ID)
	cmd, ready, err := b.startSupervisor(inv.ID, newCmd, &group, startInCgroup)
	if err != nil {
		return fmt.Errorf("detach: start supervisor of %s: %w", inv.Unit, err)
	}
	err = awaitReady(ctx, ready, cmd.Process.Pid)
	if group != "" && errors.Is(err, errExitedBeforeReady) && killedBySIGKILL(cmd) {
		// Linux 6.8 SIGKILLs every CLONE_INTO_CGROUP child of a process in
		// a group that cgroup.kill was written to, as OpenRC's
		// rc_cgroup_cleanup="YES" does to openrc.nexusd, while clone3
		// reports success. A plain fork is not killed (WI 3.4).
		b.log().Warn("an operation supervisor was killed as it started in its cgroup; starting it outside and moving it in",
			"operationId", inv.ID, "cgroup", group)
		_ = ready.Close()
		if cmd, ready, err = b.startSupervisor(inv.ID, newCmd, &group, startMoved); err != nil {
			return fmt.Errorf("detach: start supervisor of %s: %w", inv.Unit, err)
		}
		err = awaitReady(ctx, ready, cmd.Process.Pid)
	}
	_ = ready.Close() // Only ever read
	pid := cmd.Process.Pid
	fail := func(err error) error {
		// SIGKILL before the identity exists: the supervisor has taken
		// nothing yet, so no work ran.
		if kerr := cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			err = errors.Join(err, fmt.Errorf("kill supervisor %d: %w", pid, kerr))
		}
		_ = cmd.Wait() // Reaps it; its exit status is the SIGKILL just sent
		if group != "" {
			b.removeCgroup(inv.ID, group)
		}
		return fmt.Errorf("detach: launch %s: %w", inv.Unit, err)
	}
	if err != nil {
		return fail(err)
	}
	id, err := b.identityOf(pid)
	if err != nil {
		return fail(err)
	}
	if err := writeIdentity(dir, id); err != nil {
		return fail(err)
	}
	go func() {
		// Reaps the supervisor while this nexusd lives. After a restart the
		// orphaned supervisors are reaped by init instead.
		if err := cmd.Wait(); err != nil {
			b.log().Warn("operation supervisor exited with an error; see its log",
				"operationId", inv.ID, "pid", pid, "log", filepath.Join(dir, SupervisorLog), "error", err.Error())
		}
	}()
	return nil
}

// cgroup creates the cgroup v2 group of operation id and returns its path,
// or "" when the supervisor is to run without one: containment is off
// (CgroupRoot "-"), there is no writable cgroup v2 hierarchy (logged once,
// CON-10), or the group cannot be created.
func (b *processBackend) cgroup(id string) string {
	root := b.cfg.CgroupRoot
	switch root {
	case "-":
		return ""
	case "":
		root = defaultCgroupRoot
	}
	if err := available(root); err != nil {
		b.noCgroupLog.Do(func() {
			b.log().Warn("cgroup v2 unavailable: operation supervisors run without a cgroup, and descendants that leave the process group are not covered (CON-10)",
				"error", err.Error())
		})
		return ""
	}
	path, err := create(root, id)
	if err != nil {
		b.log().Warn("cannot create an operation cgroup; the supervisor runs without one (CON-10)",
			"operationId", id, "error", err.Error())
		return ""
	}
	return path
}

// removeCgroup removes the group of a launch that did not go ahead or
// could not use it. A leftover empty group is harmless.
func (b *processBackend) removeCgroup(id, path string) {
	if err := cleanup(path); err != nil {
		b.log().Warn("cannot remove an operation cgroup", "operationId", id, "error", err.Error())
	}
}

// startSupervisor starts the supervisor that newCmd builds around the write
// end of a new readiness pipe, and returns it with the read end. In
// *group, unless that is "", it starts it with start, and when start could
// not place it there it clears *group, having removed the group. When it
// did not start, the group is removed.
func (b *processBackend) startSupervisor(id string, newCmd func(*os.File) func() *exec.Cmd, group *string,
	start func(func() *exec.Cmd, string) (*exec.Cmd, error, error)) (*exec.Cmd, *os.File, error) {
	ready, readyW, err := os.Pipe()
	if err != nil {
		if *group != "" {
			b.removeCgroup(id, *group)
		}
		return nil, nil, fmt.Errorf("readiness pipe: %w", err)
	}
	var cmd *exec.Cmd
	if *group == "" {
		cmd = newCmd(readyW)()
		err = cmd.Start()
	} else {
		var notPlaced error
		cmd, notPlaced, err = start(newCmd(readyW), *group)
		if err == nil && notPlaced != nil {
			b.log().Warn("cannot place an operation supervisor in its cgroup; it runs without one (CON-10)",
				"operationId", id, "cgroup", *group, "error", notPlaced.Error())
			b.removeCgroup(id, *group)
			*group = ""
		}
	}
	// Once the supervisor holds the only write end, a read sees its exit.
	// Closing a pipe end loses nothing.
	_ = readyW.Close()
	if err != nil {
		_ = ready.Close()
		if *group != "" {
			b.removeCgroup(id, *group)
		}
		return nil, nil, err
	}
	return cmd, ready, nil
}

// errExitedBeforeReady is awaitReady's error for a supervisor that exited
// before it reported ready.
var errExitedBeforeReady = errors.New("exited before it could handle a cancel")

// killedBySIGKILL reaps cmd, which has exited, and reports whether a
// SIGKILL ended it.
func killedBySIGKILL(cmd *exec.Cmd) bool {
	_ = cmd.Wait() // Its status is read below
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// awaitReady waits until supervisor pid writes to its readiness pipe r,
// for as long as ctx allows and at most LaunchTimeout.
func awaitReady(ctx context.Context, r *os.File, pid int) error {
	ctx, cancel := context.WithTimeout(ctx, LaunchTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		// Ends the Read below; a pipe in the poller cannot fail this.
		_ = r.SetReadDeadline(time.Now())
	})
	defer stop()
	var b [1]byte
	_, err := r.Read(b[:])
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF):
		return fmt.Errorf("supervisor %d %w; see %s", pid, errExitedBeforeReady, SupervisorLog)
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("supervisor %d did not report that it handles a cancel: %w", pid, context.Cause(ctx))
	}
	return fmt.Errorf("supervisor %d readiness: %w", pid, err)
}

// linuxSignalReady is signalReady: it writes one byte to readyFD and closes
// it, when readyFD is the write end of a pipe. A supervisor started any
// other way has no such descriptor there and reports nothing.
func linuxSignalReady() error {
	var st syscall.Stat_t
	if err := syscall.Fstat(readyFD, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return nil
	}
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, readyFD, syscall.F_GETFL, 0)
	if errno != 0 || flags&syscall.O_ACCMODE != syscall.O_WRONLY {
		return nil
	}
	_, werr := syscall.Write(readyFD, []byte{'R'})
	// Closed either way: it has no close-on-exec flag, so the child would
	// inherit it.
	cerr := syscall.Close(readyFD)
	if werr != nil {
		return fmt.Errorf("report readiness: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("close the readiness pipe: %w", cerr)
	}
	return nil
}

// identityOf reads the identity of the just-started supervisor pid.
func (b *processBackend) identityOf(pid int) (Identity, error) {
	st, err := procStat(b.procRoot(), pid)
	if err != nil {
		return Identity{}, err
	}
	if st.State == 'Z' || st.State == 'X' {
		return Identity{}, fmt.Errorf("supervisor %d exited at once", pid)
	}
	boot, err := bootID(b.procRoot())
	if err != nil {
		return Identity{}, err
	}
	return Identity{PID: pid, StartTicks: st.StartTicks, BootID: boot}, nil
}

var (
	unitActive = UnitState{ActiveState: "active", LoadState: "loaded"}
	unitAbsent = UnitState{ActiveState: "inactive", LoadState: "not-found"}
)

// unitState answers from the identity: a live supervisor is an active
// unit, anything definitely not alive is a unit that no longer exists, as
// a collected unit is.
func (b *processBackend) unitState(_ context.Context, unit string) (UnitState, error) {
	dir, err := b.dir(unit)
	if err != nil {
		return UnitState{}, err
	}
	_, mismatch, err := checkIdentity(b.procRoot(), dir)
	if err != nil {
		return UnitState{}, err
	}
	if mismatch != "" {
		return unitAbsent, nil
	}
	return unitActive, nil
}

// signal sends sig to unit's supervisor once its identity is verified. The
// process is held by a pidfd (os.FindProcess) from before the check, so a
// PID reused after the check is not the process signaled (L-3).
func (b *processBackend) signal(_ context.Context, unit string, sig syscall.Signal) error {
	dir, err := b.dir(unit)
	if err != nil {
		return err
	}
	id, err := ReadIdentity(dir)
	if err != nil {
		return fmt.Errorf("detach: signal %s: %w", unit, err)
	}
	p, err := os.FindProcess(id.PID)
	if err != nil {
		return fmt.Errorf("detach: signal %s: %w", unit, err)
	}
	defer func() { _ = p.Release() }() // Only closes the pidfd
	checked, mismatch, err := checkIdentity(b.procRoot(), dir)
	if err != nil {
		return fmt.Errorf("detach: signal %s: %w", unit, err)
	}
	if mismatch == "" && checked.PID != id.PID {
		mismatch = fmt.Sprintf("%s changed from pid %d to %d", IdentityFile, id.PID, checked.PID)
	}
	if mismatch != "" {
		return fmt.Errorf("detach: not signaling %s: supervisor identity mismatch: %s", unit, mismatch)
	}
	if err := p.Signal(sig); err != nil {
		return fmt.Errorf("detach: signal %s (pid %d): %w", unit, id.PID, err)
	}
	return nil
}

// listUnits scans the ops directory for supervisors whose identity is
// live and names them as units, which restart recovery's orphan check
// parses as it parses systemd's list.
func (b *processBackend) listUnits(ctx context.Context) ([]string, error) {
	if b.cfg.OpsDir == "" {
		return nil, errors.New("detach: process backend has no ops directory")
	}
	entries, err := os.ReadDir(b.cfg.OpsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("detach: list operations: %w", err)
	}
	var units []string
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.IsDir() || !validID.MatchString(e.Name()) {
			continue
		}
		_, mismatch, err := checkIdentity(b.procRoot(), filepath.Join(b.cfg.OpsDir, e.Name()))
		if err != nil {
			// One unreadable identity must not hide the others; the orphan
			// check only reports, so leaving this one out is safe.
			b.log().Warn("cannot check an operation supervisor's identity", "operationId", e.Name(), "error", err.Error())
			continue
		}
		if mismatch == "" {
			units = append(units, Unit(e.Name()))
		}
	}
	sort.Strings(units)
	return units, nil
}
