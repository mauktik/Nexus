//go:build linux

package detach

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// writeIdentity is WriteIdentity; a test makes it fail.
var writeIdentity = WriteIdentity

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

// launch starts inv's supervisor and records its identity. A supervisor
// waits for that record before it takes spec.json, so one whose identity
// was never written runs nothing and exits (L-51).
func (b *processBackend) launch(_ context.Context, inv Invocation) (err error) {
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

	newCmd := func() *exec.Cmd {
		// Not CommandContext: the supervisor must outlive this call and nexusd.
		cmd := exec.Command(exe, "op-supervise", "--dir", dir, "--process")
		cmd.Env = []string{} // Empty, not nil: nothing of nexusd's environment
		cmd.Dir = "/"
		cmd.Stdin = nil // /dev/null
		cmd.Stdout, cmd.Stderr = logFile, logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return cmd
	}
	// The supervisor goes into its own cgroup, out of nexusd's (for
	// OpenRC, openrc.nexusd), so neither a stop of nexusd with
	// rc_cgroup_cleanup="YES" (ASM-14) nor anything else that ends nexusd's
	// group reaches it.
	group := b.cgroup(inv.ID)
	var cmd *exec.Cmd
	if group == "" {
		cmd = newCmd()
		err = cmd.Start()
	} else {
		var notPlaced error
		cmd, notPlaced, err = startInCgroup(newCmd, group)
		if err == nil && notPlaced != nil {
			b.log().Warn("cannot place an operation supervisor in its cgroup; it runs without one (CON-10)",
				"operationId", inv.ID, "cgroup", group, "error", notPlaced.Error())
			b.removeCgroup(inv.ID, group)
			group = ""
		}
	}
	if err != nil {
		if group != "" {
			b.removeCgroup(inv.ID, group)
		}
		return fmt.Errorf("detach: start supervisor of %s: %w", inv.Unit, err)
	}
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
