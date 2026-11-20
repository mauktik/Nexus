// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package detach

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/oplog"
)

// fallbacksMarker in an operation directory makes this test binary, when it
// runs as that operation's supervisor (TestMain's op-supervise), take the
// fallbacks of TC-E12: no CLONE_INTO_CGROUP, no cgroup.kill. It records
// cgroup.freeze of the group after each kill in freezeAfterKill.
const (
	fallbacksMarker = "cgroup-fallbacks"
	freezeAfterKill = "freeze-after-kill"
)

func init() {
	if len(os.Args) < 2 || os.Args[1] != "op-supervise" {
		return
	}
	var dir string
	for i, a := range os.Args {
		if a == "--dir" && i+1 < len(os.Args) {
			dir = os.Args[i+1]
		}
	}
	if _, err := os.Stat(filepath.Join(dir, fallbacksMarker)); err != nil {
		return
	}
	cloneIntoCgroup = func(*exec.Cmd, int) error { return syscall.EINVAL }
	hasCgroupKill = func(string) bool { return false }
	killCgroup = func(path string) error {
		err := kill(path)
		if b, rerr := os.ReadFile(filepath.Join(path, "cgroup.freeze")); rerr == nil {
			f, oerr := os.OpenFile(filepath.Join(dir, freezeAfterKill), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			if oerr == nil {
				_, _ = f.Write(b)
				_ = f.Close()
			}
		}
		return err
	}
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
}

func requireCgroup2(t *testing.T) {
	t.Helper()
	if err := available(defaultCgroupRoot); err != nil {
		t.Skipf("needs a writable cgroup v2 hierarchy: %v", err)
	}
}

var cgOpSeq atomic.Int64

// cgOp is a fresh operation of a launcher with the process backend, whose
// supervisor is this test binary. root is ProcessConfig.CgroupRoot. In
// cleanup it kills whatever the operation left in its cgroup and removes
// the group.
func cgOp(t *testing.T, root string) (l *Launcher, opts Options, id, dir string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ops := t.TempDir()
	opts = ProcessBackend(ProcessConfig{OpsDir: ops, Executable: exe, CgroupRoot: root})
	if l, err = New(Config{OpsDir: ops, Executable: exe}, opts); err != nil {
		t.Fatal(err)
	}
	id = fmt.Sprintf("01CG%d-%d-%d", os.Getpid(), time.Now().UnixNano(), cgOpSeq.Add(1))
	dir = l.Dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ident, err := ReadIdentity(dir); err == nil {
			// It may still be removing its cgroups after exit.json.
			for deadline := time.Now().Add(5 * time.Second); running(ident.PID) && time.Now().Before(deadline); {
				time.Sleep(20 * time.Millisecond)
			}
			_ = syscall.Kill(ident.PID, syscall.SIGKILL) // Gone already, normally
		}
		group := filepath.Join(defaultCgroupRoot, opsCgroup, id)
		if _, err := os.Stat(group); err == nil {
			if err := kill(group); err != nil {
				t.Logf("cleanup: %v", err)
			}
			for deadline := time.Now().Add(2 * time.Second); eventIs(group, "populated") && time.Now().Before(deadline); {
				time.Sleep(20 * time.Millisecond)
			}
			if err := cleanup(group); err != nil {
				t.Logf("cleanup: %v", err)
			}
		}
	})
	return l, opts, id, dir
}

func cgSpec(dir, script string, mode KillMode) Spec {
	return Spec{
		Argv:       []string{"/bin/sh", "-c", script},
		Env:        []string{"DIR=" + dir, "PATH=/usr/sbin:/usr/bin:/sbin:/bin"},
		KillMode:   mode,
		MaxRuntime: time.Hour,
		Grace:      time.Second,
	}
}

func cgWaitFile(t *testing.T, path string, limit time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 && b[len(b)-1] == '\n' {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not complete within %s (%v)", path, limit, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func cgWaitExit(t *testing.T, dir string, limit time.Duration) Exit {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		x, ok, err := ReadExit(dir)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return x
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(dir, SupervisorLog))
			t.Fatalf("no %s within %s; supervisor.log:\n%s", ExitFile, limit, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func pidIn(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(string(cgWaitFile(t, path, 10*time.Second))))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// running reports whether pid is a live process: neither gone nor a zombie
// waiting for init to reap it.
func running(pid int) bool {
	st, err := procStat("/proc", pid)
	return err == nil && st.State != 'Z' && st.State != 'X'
}

// waitGone fails unless pid stops running within limit.
func waitGone(t *testing.T, pid int, limit time.Duration, what string) {
	t.Helper()
	for deadline := time.Now().Add(limit); running(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) still runs after %s", what, pid, limit)
		}
	}
}

// killAtCleanup SIGKILLs pid at the end of the test, a process the
// operation is expected to leave running.
func killAtCleanup(t *testing.T, pid int) {
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
}

func cgroupOf(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatal(err)
	}
	p, err := parseCgroupFile(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func supervisorPID(t *testing.T, dir string) int {
	t.Helper()
	ident, err := ReadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ident.PID
}

// A setsid grandchild that ignores SIGTERM, so only a SIGKILL of the
// cgroup ends it. It writes its PID to $DIR/gc.
const stubbornGrandchild = `setsid sh -c 'trap "" TERM; echo $$ > "$DIR/gc"; while :; do sleep 1; done' </dev/null >/dev/null 2>&1 &`

// nginx-provider TC-19: KillControlGroup operations run in a per-operation
// cgroup that is removed afterwards (FR-20 item 4, CON-10).
func TestOperationCgroupContainment(t *testing.T) {
	requireRoot(t)
	requireCgroup2(t)
	l, _, id, dir := cgOp(t, "")
	spec := cgSpec(dir, `cat /proc/self/cgroup > "$DIR/marker"; sleep 1`, KillControlGroup)
	if err := l.Start(context.Background(), id, spec); err != nil {
		t.Fatal(err)
	}
	ops := filepath.Join(defaultCgroupRoot, opsCgroup)
	if got, want := cgroupOf(t, supervisorPID(t, dir)), "/"+opsCgroup+"/"+id; got != want {
		t.Errorf("supervisor in cgroup %s, want %s", got, want)
	}
	child, err := parseCgroupFile(cgWaitFile(t, filepath.Join(dir, "marker"), 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/" + opsCgroup + "/" + id + "/" + workCgroup; child != want {
		t.Errorf("child in cgroup %s, want %s", child, want)
	}
	for _, p := range []string{filepath.Join(ops, id), ops} {
		b, err := os.ReadFile(filepath.Join(p, "cgroup.subtree_control"))
		if err != nil {
			t.Fatal(err)
		}
		if s := strings.TrimSpace(string(b)); s != "" {
			t.Errorf("%s/cgroup.subtree_control is %q, want empty", p, s)
		}
	}
	if x := cgWaitExit(t, dir, 15*time.Second); x.ExitCode != 0 {
		t.Fatalf("exit.json %+v, want exit code 0", x)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		_, err := os.Stat(filepath.Join(ops, id))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(dir, SupervisorLog))
			t.Fatalf("%s/%s still exists 5s after exit.json (%v); supervisor.log:\n%s", ops, id, err, b)
		}
	}
}

// nginx-provider TC-20: cancel kills a setsid grandchild through the cgroup
// (FR-20 item 4, NFR-10).
func TestCancelKillsSetsidGrandchild(t *testing.T) {
	requireRoot(t)
	requireCgroup2(t)
	l, opts, id, dir := cgOp(t, "")
	spec := cgSpec(dir, stubbornGrandchild+"\nexec sleep 1000", KillControlGroup)
	if err := l.Start(context.Background(), id, spec); err != nil {
		t.Fatal(err)
	}
	gc := pidIn(t, filepath.Join(dir, "gc"))
	killAtCleanup(t, gc)
	if err := RequestCancel(dir); err != nil { // As Launcher.Cancel does before it signals
		t.Fatal(err)
	}
	if err := opts.Signal(context.Background(), Unit(id), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitGone(t, gc, spec.Grace+2*time.Second, "setsid grandchild")
	x := cgWaitExit(t, dir, 10*time.Second)
	if !x.Canceled {
		t.Errorf("exit.json %+v, want canceled", x)
	}
	b, err := os.ReadFile(filepath.Join(defaultCgroupRoot, opsCgroup, id, workCgroup, "cgroup.procs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		t.Errorf("work cgroup still has processes %q", s)
	}
}

// nginx-provider TC-21: leftovers are swept for KillControlGroup and never
// for KillProcess (FR-20 item 4, L-10).
func TestLeftoverSweepOnlyForControlGroup(t *testing.T) {
	requireRoot(t)
	requireCgroup2(t)
	const script = `cat /proc/self/cgroup > "$DIR/marker"
sleep 1000 </dev/null >/dev/null 2>&1 &
echo $! > "$DIR/bg"
exit 0`
	run := func(mode KillMode) (id, dir string, bg int) {
		l, _, id, dir := cgOp(t, "")
		if err := l.Start(context.Background(), id, cgSpec(dir, script, mode)); err != nil {
			t.Fatal(err)
		}
		bg = pidIn(t, filepath.Join(dir, "bg"))
		killAtCleanup(t, bg)
		if x := cgWaitExit(t, dir, 15*time.Second); x.ExitCode != 0 {
			t.Fatalf("%s: exit.json %+v, want exit code 0", mode, x)
		}
		return id, dir, bg
	}
	_, _, a := run(KillControlGroup)
	idB, dirB, b := run(KillProcess)

	waitGone(t, a, time.Second+2*time.Second, "KillControlGroup leftover")
	time.Sleep(500 * time.Millisecond)
	if !running(b) {
		t.Errorf("KillProcess leftover %d was ended", b)
	}
	child, err := parseCgroupFile(cgWaitFile(t, filepath.Join(dirB, "marker"), time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/" + opsCgroup + "/" + idB; child != want {
		t.Errorf("KillProcess child in cgroup %s, want %s", child, want)
	}
	if _, err := os.Stat(filepath.Join(defaultCgroupRoot, opsCgroup, idB, workCgroup)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("KillProcess operation has a work cgroup (%v)", err)
	}
}

// nginx-provider TC-E11: without cgroup v2 the operation log records the
// CON-10 limitation once (CON-10, FR-20 item 4).
func TestNoCgroupFallbackNotesCON10(t *testing.T) {
	l, opts, id, dir := cgOp(t, "-")
	spec := cgSpec(dir, `setsid sh -c 'echo $$ > "$DIR/gc"; exec sleep 1000' </dev/null >/dev/null 2>&1 &
sleep 1000 </dev/null >/dev/null 2>&1 &
echo $! > "$DIR/bg"
exec sleep 1000`, KillControlGroup)
	if err := l.Start(context.Background(), id, spec); err != nil {
		t.Fatal(err)
	}
	gc, bg := pidIn(t, filepath.Join(dir, "gc")), pidIn(t, filepath.Join(dir, "bg"))
	killAtCleanup(t, gc)
	killAtCleanup(t, bg)
	if err := RequestCancel(dir); err != nil { // As Launcher.Cancel does before it signals
		t.Fatal(err)
	}
	if err := opts.Signal(context.Background(), Unit(id), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if x := cgWaitExit(t, dir, 10*time.Second); !x.Canceled {
		t.Errorf("exit.json %+v, want canceled", x)
	}
	p, err := oplog.NewReader(filepath.Join(dir, OutputDir)).Read(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, f := range p.Frames {
		if f.S == apitypes.StreamStderr && f.D == con10Note+"\n" {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("%d CON-10 notes in frames %+v, want 1", notes, p.Frames)
	}
	waitGone(t, bg, 2*time.Second, "same-group sleep")
	if !running(gc) {
		t.Errorf("setsid sleep %d was ended without a cgroup", gc)
	}
}

// nginx-provider TC-E12: fallbacks when CLONE_INTO_CGROUP or cgroup.kill
// are unavailable (FR-20 item 4, L-7, L-9).
func TestCgroupFallbacks(t *testing.T) {
	requireRoot(t)
	requireCgroup2(t)
	saved := cloneIntoCgroup
	cloneIntoCgroup = func(*exec.Cmd, int) error { return syscall.EINVAL }
	t.Cleanup(func() { cloneIntoCgroup = saved })

	l, opts, id, dir := cgOp(t, "")
	if err := os.WriteFile(filepath.Join(dir, fallbacksMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := cgSpec(dir, `cat /proc/self/cgroup > "$DIR/marker"
`+stubbornGrandchild+"\nexec sleep 1000", KillControlGroup)
	if err := l.Start(context.Background(), id, spec); err != nil {
		t.Fatal(err)
	}
	if got, want := cgroupOf(t, supervisorPID(t, dir)), "/"+opsCgroup+"/"+id; got != want {
		t.Errorf("supervisor in cgroup %s, want %s (through cgroup.procs)", got, want)
	}
	child, err := parseCgroupFile(cgWaitFile(t, filepath.Join(dir, "marker"), 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/" + opsCgroup + "/" + id + "/" + workCgroup; child != want {
		t.Errorf("child in cgroup %s, want %s (through cgroup.procs)", child, want)
	}
	gc := pidIn(t, filepath.Join(dir, "gc"))
	killAtCleanup(t, gc)
	if err := RequestCancel(dir); err != nil { // As Launcher.Cancel does before it signals
		t.Fatal(err)
	}
	if err := opts.Signal(context.Background(), Unit(id), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitGone(t, gc, spec.Grace+2*time.Second, "setsid grandchild")
	if x := cgWaitExit(t, dir, 10*time.Second); !x.Canceled {
		t.Errorf("exit.json %+v, want canceled", x)
	}
	freeze, err := os.ReadFile(filepath.Join(dir, freezeAfterKill))
	if err != nil {
		t.Fatalf("the freeze fallback never ran: %v", err)
	}
	for _, v := range bytes.Fields(freeze) {
		if string(v) != "0" {
			t.Errorf("cgroup.freeze after kill %q, want 0 each time", freeze)
		}
	}
}

// WI 3.4 finding: on Linux 6.8, once cgroup.kill has been written to the
// group nexusd runs in (OpenRC's rc_cgroup_cleanup="YES" with upstream
// 1cf3944), every child it clones with CLONE_INTO_CGROUP is SIGKILLed at
// once. The launch then starts the supervisor outside its group and moves
// it in, as it does when clone3 is refused.
func TestLaunchFallsBackWhenClonedSupervisorIsKilled(t *testing.T) {
	requireRoot(t)
	requireCgroup2(t)
	saved := cloneIntoCgroup
	var clones atomic.Int32
	cloneIntoCgroup = func(cmd *exec.Cmd, fd int) error {
		clones.Add(1)
		if err := saved(cmd, fd); err != nil {
			return err
		}
		// What the kernel does, with clone3 itself reporting success.
		return syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
	}
	t.Cleanup(func() { cloneIntoCgroup = saved })

	l, _, id, dir := cgOp(t, "")
	// The supervisor reports ready only well after the SIGKILL above.
	if err := os.WriteFile(filepath.Join(dir, "delay-notify"), []byte("300ms"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := cgSpec(dir, `cat /proc/self/cgroup > "$DIR/marker"`, KillControlGroup)
	if err := l.Start(context.Background(), id, spec); err != nil {
		t.Fatalf("Start: %v; supervisor.log:\n%s", err, readOrErr(filepath.Join(dir, SupervisorLog)))
	}
	if n := clones.Load(); n != 1 {
		t.Errorf("%d clones into the cgroup, want 1 then a start outside it", n)
	}
	if got, want := cgroupOf(t, supervisorPID(t, dir)), "/"+opsCgroup+"/"+id; got != want {
		t.Errorf("supervisor in cgroup %s, want %s (through cgroup.procs)", got, want)
	}
	child, err := parseCgroupFile(cgWaitFile(t, filepath.Join(dir, "marker"), 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/" + opsCgroup + "/" + id + "/" + workCgroup; child != want {
		t.Errorf("child in cgroup %s, want %s", child, want)
	}
	if x := cgWaitExit(t, dir, 10*time.Second); x.ExitCode != 0 {
		t.Errorf("exit.json %+v, want exit code 0", x)
	}
}

// readOrErr is path's content, or why it cannot be read, for a message.
func readOrErr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

func TestParseCgroupFile(t *testing.T) {
	for in, want := range map[string]string{
		"0::/nexus-ops/a\n":                      "/nexus-ops/a",
		"12:pids:/x\n1:name=systemd:/y\n0::/z\n": "/z",
		"0::/\n":                                 "/",
	} {
		if got, err := parseCgroupFile([]byte(in)); err != nil || got != want {
			t.Errorf("parseCgroupFile(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := parseCgroupFile([]byte("3:cpu:/a\n")); err == nil {
		t.Error("a v1-only file parsed")
	}
}
