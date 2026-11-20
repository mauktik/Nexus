// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package detachtest runs detached operations on a development machine
// without systemd, for T0 tests. The real systemd-run path is proven in the
// systemd harness (3.5).
package detachtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/detach"
)

// Local returns the three process seams of detach.Options for every T0
// test of a detached operation: a launcher that execs the real
// `nexusd op-supervise` in its own session (Setsid) instead of a unit, a
// UnitState that reports the unit active while that supervisor lives
// (kill(pid, 0)) and absent once it has exited, and a Signal that sends to
// the supervisor's PID. A test takes all three, never Launch alone, so a
// nil UnitState can never reach a real systemctl here.
func Local() detach.Options {
	l := &local{procs: map[string]*proc{}}
	return detach.Options{Launch: l.launch, UnitState: l.state, Signal: l.signal}
}

// Backend is one implementation of the launcher's process seams that the
// behavioral suites run on (NFR-8).
type Backend struct {
	// Name is the backend's subtest name: "local" or "process".
	Name string
	// Options returns a new instance of the backend's seams for a launcher
	// on opsDir, logging to log. A test makes one instance per ops
	// directory and keeps it across restarts of its manager, as the
	// supervisors outlive a nexusd.
	Options func(opsDir string, log *slog.Logger) detach.Options
}

// Backends returns the backends a behavioral suite runs on: local, the
// seams of Local, everywhere, and on Linux also process, the real
// detach.ProcessBackend running the nexusd built by Supervisor. Run as
// root, the process backend contains each supervisor in a cgroup v2 group
// when the host has a writable one; otherwise it runs without (CON-10).
// Off Linux it logs why there is no process backend.
func Backends(t testing.TB) []Backend {
	t.Helper()
	backends := []Backend{{Name: "local", Options: func(string, *slog.Logger) detach.Options { return Local() }}}
	if runtime.GOOS != "linux" {
		t.Logf("detachtest: no process backend on %s: it reads /proc and cgroup v2, which only Linux has", runtime.GOOS)
		return backends
	}
	exe, err := Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	cgroupRoot := "-" // Containment needs root
	if os.Geteuid() == 0 {
		cgroupRoot = ""
	}
	return append(backends, Backend{Name: "process", Options: func(opsDir string, log *slog.Logger) detach.Options {
		return detach.ProcessBackend(detach.ProcessConfig{OpsDir: opsDir, Executable: exe, CgroupRoot: cgroupRoot, Logger: log})
	}})
}

type proc struct {
	pid  int
	done chan struct{} // Closed once the supervisor has exited and been reaped
}

type local struct {
	mu    sync.Mutex
	procs map[string]*proc // By unit
}

func (l *local) launch(_ context.Context, inv detach.Invocation) error {
	exe, err := Supervisor()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "op-supervise", "--dir", inv.Dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("detachtest: start supervisor: %w", err)
	}
	p := &proc{pid: cmd.Process.Pid, done: make(chan struct{})}
	l.mu.Lock()
	l.procs[inv.Unit] = p
	l.mu.Unlock()
	go func() {
		// The exit status is the supervisor's business; reaping it is what
		// matters, so kill(pid, 0) stops seeing a zombie.
		if err := cmd.Wait(); err != nil {
			fmt.Fprintf(os.Stderr, "detachtest: supervisor for %s: %v\n", inv.Unit, err)
		}
		close(p.done)
	}()
	return nil
}

var (
	absent = detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}
	active = detach.UnitState{ActiveState: "active", LoadState: "loaded"}
)

func (l *local) state(_ context.Context, unit string) (detach.UnitState, error) {
	l.mu.Lock()
	p, ok := l.procs[unit]
	l.mu.Unlock()
	if !ok {
		return absent, nil
	}
	select {
	case <-p.done:
		// A unit started with --collect is unloaded once it ends.
		return absent, nil
	default:
	}
	if err := syscall.Kill(p.pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return absent, nil
	}
	return active, nil
}

func (l *local) units(ctx context.Context) ([]string, error) {
	l.mu.Lock()
	names := make([]string, 0, len(l.procs))
	for unit := range l.procs {
		names = append(names, unit)
	}
	l.mu.Unlock()
	var loaded []string
	for _, unit := range names {
		if st, err := l.state(ctx, unit); err == nil && st.LoadState != "not-found" {
			loaded = append(loaded, unit)
		}
	}
	sort.Strings(loaded)
	return loaded, nil
}

func (l *local) signal(_ context.Context, unit string, sig syscall.Signal) error {
	l.mu.Lock()
	p, ok := l.procs[unit]
	l.mu.Unlock()
	if !ok {
		return fmt.Errorf("detachtest: unit %s not loaded", unit)
	}
	select {
	case <-p.done:
		return fmt.Errorf("detachtest: unit %s not loaded", unit)
	default:
	}
	return syscall.Kill(p.pid, sig)
}

var build struct {
	once sync.Once
	path string
	err  error
}

// SupervisorEnv names a prebuilt nexusd that Supervisor returns instead of
// building one. The T1 harnesses set it to the harness nexusd: they run
// these suites as `go test -c` binaries in a container with no Go toolchain.
const SupervisorEnv = "NEXUS_TEST_SUPERVISOR"

// Supervisor returns the path of a nexusd binary built from this module,
// once per test process, for running `nexusd op-supervise` for real, or
// the one $NEXUS_TEST_SUPERVISOR names.
func Supervisor() (string, error) {
	build.once.Do(func() {
		if p := os.Getenv(SupervisorEnv); p != "" {
			build.path = p
			if _, err := os.Stat(p); err != nil {
				build.err = fmt.Errorf("detachtest: %s: %w", SupervisorEnv, err)
			}
			return
		}
		build.path, build.err = buildNexusd()
	})
	return build.path, build.err
}

func buildNexusd() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("detachtest: cannot locate the module")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	parent := filepath.Join(os.TempDir(), "nexus-detachtest")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("detachtest: %w", err)
	}
	pruneOld(parent)
	dir, err := os.MkdirTemp(parent, "build-*")
	if err != nil {
		return "", fmt.Errorf("detachtest: %w", err)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("detachtest: no go tool to build nexusd: %w", err)
	}
	out := filepath.Join(dir, "nexusd")
	cmd := exec.Command(goTool, "build", "-o", out, "./cmd/nexusd")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("detachtest: build nexusd: %w\n%s", err, b)
	}
	return out, nil
}

// pruneOld removes builds of earlier test runs. A build still in use by a
// concurrent test process is younger than the cutoff.
func pruneOld(parent string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
			fmt.Fprintf(os.Stderr, "detachtest: prune %s: %v\n", e.Name(), err)
		}
	}
}
