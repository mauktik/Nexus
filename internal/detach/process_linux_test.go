//go:build linux

package detach_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/oplog"
)

// TestMain lets this test binary stand in for nexusd where a test needs a
// seam inside the supervisor or a process that launches and exits:
//
//   - `<test binary> op-supervise --dir D [--process]` is nexusd's
//     op-supervise. NEXUS_TEST_HANDSHAKE_TIMEOUT shortens the identity wait,
//     and a file flood-diag in D makes the supervisor write diagnostics past
//     the supervisor.log cap. A file delay-notify in D holding a duration
//     holds the supervisor that long before Supervise installs its signal
//     handlers, the window of a supervisor still starting up, and a file
//     exit-before-notify makes it exit 3 at that point.
//   - GO_WANT_LAUNCHER=1 launches one operation with a process backend and
//     exits at once, as a nexusd that is killed or restarted does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "op-supervise" {
		os.Exit(fakeNexusdSupervise(os.Args[2:]))
	}
	if os.Getenv("GO_WANT_LAUNCHER") == "1" {
		if err := launchAndExit(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeNexusdSupervise(args []string) int {
	fs := flag.NewFlagSet("op-supervise", flag.ContinueOnError)
	dir := fs.String("dir", "", "")
	process := fs.Bool("process", false, "")
	if err := fs.Parse(args); err != nil || *dir == "" {
		return 2
	}
	if v := os.Getenv("NEXUS_TEST_HANDSHAKE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 2
		}
		*detach.HandshakeTimeout = d
	}
	if _, err := os.Stat(filepath.Join(*dir, "flood-diag")); err == nil {
		*detach.DiagHook = func(logf func(string, ...any)) {
			for i := 0; i < int(oplog.DefaultCapBytes/64)+1000; i++ {
				logf("diagnostic line %08d %s", i, strings.Repeat("x", 40))
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(*dir, "delay-notify")); err == nil {
		d, err := time.ParseDuration(strings.TrimSpace(string(b)))
		if err != nil {
			return 2
		}
		time.Sleep(d)
	}
	if _, err := os.Stat(filepath.Join(*dir, "exit-before-notify")); err == nil {
		return 3
	}
	mode := detach.ModeUnit
	if *process {
		mode = detach.ModeProcess
	}
	return detach.Supervise(*dir, os.Stderr, mode)
}

// launchAndExit is the GO_WANT_LAUNCHER helper.
func launchAndExit() error {
	ops, id := os.Getenv("NEXUS_TEST_OPS"), os.Getenv("NEXUS_TEST_ID")
	cfg := detach.ProcessConfig{OpsDir: ops, Executable: os.Getenv("NEXUS_TEST_SUPERVISOR"), CgroupRoot: "-"}
	l, err := detach.New(detach.Config{OpsDir: ops, Executable: cfg.Executable}, detach.ProcessBackend(cfg))
	if err != nil {
		return err
	}
	return l.Start(context.Background(), id, shSpec(filepath.Join(ops, id), "sleep 1.5; exit 7"))
}

// processLauncher is a launcher with the process backend over a fresh ops
// directory. exe empty means the real nexusd.
func processLauncher(t *testing.T, exe string) (*detach.Launcher, detach.Options, string) {
	t.Helper()
	if exe == "" {
		exe = nexusd(t)
	}
	ops := t.TempDir()
	opts := detach.ProcessBackend(detach.ProcessConfig{OpsDir: ops, Executable: exe, CgroupRoot: "-"})
	l, err := detach.New(detach.Config{OpsDir: ops, Executable: exe}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return l, opts, ops
}

// testBinary is this test binary, which TestMain makes a stand-in nexusd.
func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// statOf parses /proc/<pid>/stat independently of the package: field n,
// counted with the fields after the last ')' starting at 3.
func statOf(t *testing.T, pid, n int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	return f[n-3]
}

// endSupervisorAtCleanup kills the operation's supervisor if a failed test
// left it running.
func endSupervisorAtCleanup(t *testing.T, dir string) {
	t.Cleanup(func() {
		if id, err := detach.ReadIdentity(dir); err == nil {
			_ = syscall.Kill(id.PID, syscall.SIGKILL) // Gone already, normally
		}
	})
}

func waitFile(t *testing.T, path string, limit time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %s (%v)", path, limit, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitNoFile waits until path does not exist.
func waitNoFile(t *testing.T, path string, limit time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(limit); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still exists after %s", path, limit)
		}
	}
}

func waitExit(t *testing.T, dir string, limit time.Duration) detach.Exit {
	t.Helper()
	waitFile(t, filepath.Join(dir, detach.ExitFile), limit)
	return readExit(t, dir)
}

func umaskOf(t *testing.T, statusFile string) string {
	t.Helper()
	b, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, "Umask:"); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no Umask: in %s", statusFile)
	return ""
}

func liveness(t *testing.T, opts detach.Options, id string) detach.Liveness {
	t.Helper()
	st, err := opts.UnitState(context.Background(), detach.Unit(id))
	if err != nil {
		t.Fatal(err)
	}
	lv, err := st.Liveness()
	if err != nil {
		t.Fatal(err)
	}
	return lv
}

// nginx-provider TC-13: the process backend launches a session-leader
// supervisor and records its identity (FR-20 items 1 and 2, CON-11, NFR-8).
// Not parallel: the umask is process-wide.
func TestProcessLaunchRecordsIdentityInOwnSession(t *testing.T) {
	l, _, _ := processLauncher(t, "")
	const id = "01JTC13"
	dir := l.Dir(id)
	endSupervisorAtCleanup(t, dir)
	spec := shSpec(dir, `grep '^Umask:' /proc/self/status > "$DIR/marker.tmp"
readlink /proc/self/fd/0 >> "$DIR/marker.tmp"
: > "$DIR/umask-probe"
mv "$DIR/marker.tmp" "$DIR/marker"
sleep 2`)

	old := syscall.Umask(0o077) // nexusd's umask under OpenRC
	t.Cleanup(func() { syscall.Umask(old) })
	if err := l.Start(context.Background(), id, spec); err != nil {
		t.Fatal(err)
	}
	if got := umaskOf(t, "/proc/self/status"); got != "0077" {
		t.Fatalf("Start changed the launching process's umask to %s", got)
	}

	ident, err := detach.ReadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	pid := ident.PID
	if fi, err := os.Stat(filepath.Join(dir, detach.IdentityFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v, %v; want mode 0600", detach.IdentityFile, fi, err)
	}
	if got := statOf(t, pid, 22); got != strconv.FormatUint(ident.StartTicks, 10) {
		t.Errorf("startTicks %d, stat field 22 %s", ident.StartTicks, got)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	if ident.BootID != strings.TrimSpace(string(boot)) {
		t.Errorf("bootId %q, want %q", ident.BootID, strings.TrimSpace(string(boot)))
	}
	if got := statOf(t, pid, 6); got != strconv.Itoa(pid) {
		t.Errorf("supervisor session %s, want its own pid %d", got, pid)
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Fatal(err)
	}
	wantCmd := []string{nexusd(t), "op-supervise", "--dir", dir, "--process"}
	if got := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00"); !slices.Equal(got, wantCmd) {
		t.Errorf("cmdline %q, want %q", got, wantCmd)
	}
	if env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err != nil || len(env) != 0 {
		t.Errorf("supervisor environment %q, %v; want empty", env, err)
	}
	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err != nil || cwd != "/" {
		t.Errorf("supervisor cwd %q, %v; want /", cwd, err)
	}
	if in, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", pid)); err != nil || in != "/dev/null" {
		t.Errorf("supervisor stdin %q, %v; want /dev/null", in, err)
	}

	marker := strings.Split(strings.TrimSpace(string(waitFile(t, filepath.Join(dir, "marker"), 10*time.Second))), "\n")
	if len(marker) != 2 || strings.TrimSpace(strings.TrimPrefix(marker[0], "Umask:")) != "0022" || marker[1] != "/dev/null" {
		t.Errorf("child marker %q, want Umask 0022 and stdin /dev/null", marker)
	}
	fi, err := os.Stat(filepath.Join(dir, "umask-probe"))
	if err != nil {
		t.Fatal(err)
	}
	if want := fs.FileMode(0o666 &^ 0o022); fi.Mode().Perm() != want {
		t.Errorf("umask-probe mode %v, want %v", fi.Mode().Perm(), want)
	}
	if x := waitExit(t, dir, 10*time.Second); x.ExitCode != 0 {
		t.Errorf("exit.json %+v, want exit code 0", x)
	}
}

// nginx-provider TC-14: a supervisor outlives the process that launched it
// and is finalized from exit.json (FR-20 items 1 and 3, FR-21).
func TestSupervisorOutlivesLauncherProcess(t *testing.T) {
	ops := t.TempDir()
	const id = "01JTC14"
	dir := filepath.Join(ops, id)
	endSupervisorAtCleanup(t, dir)
	helper := exec.Command(testBinary(t))
	helper.Env = append(os.Environ(), "GO_WANT_LAUNCHER=1", "NEXUS_TEST_OPS="+ops, "NEXUS_TEST_ID="+id,
		"NEXUS_TEST_SUPERVISOR="+nexusd(t))
	out, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("launcher helper: %v\n%s", err, out)
	}
	helperPID := helper.Process.Pid

	opts := detach.ProcessBackend(detach.ProcessConfig{OpsDir: ops, CgroupRoot: "-"})
	st, err := opts.UnitState(context.Background(), detach.Unit(id))
	if err != nil {
		t.Fatal(err)
	}
	if lv, err := st.Liveness(); st != (detach.UnitState{ActiveState: "active", LoadState: "loaded"}) || lv != detach.Live || err != nil {
		t.Fatalf("after the launcher exited: %+v (%v, %v), want active/loaded, Live", st, lv, err)
	}
	ident, err := detach.ReadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ppid := statOf(t, ident.PID, 4); ppid == strconv.Itoa(helperPID) {
		t.Errorf("supervisor's parent is still the exited launcher %d", helperPID)
	}

	if x := waitExit(t, dir, 15*time.Second); x.ExitCode != 7 {
		t.Fatalf("exit.json %+v, want exit code 7", x)
	}
	// The supervisor exits right after writing exit.json.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		st, err = opts.UnitState(context.Background(), detach.Unit(id))
		if err != nil {
			t.Fatal(err)
		}
		if st == (detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after exit.json: %+v, want inactive/not-found", st)
		}
	}
	if lv, _ := st.Liveness(); lv != detach.Absent {
		t.Fatalf("liveness %v, want Absent", lv)
	}
}

// nginx-provider TC-15: Signal reaches only a verified supervisor, and
// cancel behaves as with detachtest.Local (FR-20 item 3).
func TestSignalCancelsVerifiedSupervisor(t *testing.T) {
	exe := nexusd(t)
	run := func(name string, backend func(ops string) detach.Options) (detach.Exit, string) {
		ops := t.TempDir()
		opts := backend(ops)
		l, err := detach.New(detach.Config{OpsDir: ops, Executable: exe}, opts)
		if err != nil {
			t.Fatal(err)
		}
		const id = "01JTC15"
		dir := l.Dir(id)
		endSupervisorAtCleanup(t, dir)
		spec := shSpec(dir, `trap 'echo term > "$DIR/marker"; exit 3' TERM
echo ready > "$DIR/ready"
while :; do sleep 0.1; done`)
		spec.Grace = 2 * time.Second
		if err := l.Start(context.Background(), id, spec); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		waitFile(t, filepath.Join(dir, "ready"), 10*time.Second)
		// The cancel request Launcher.Cancel records before it signals.
		if err := detach.RequestCancel(dir); err != nil {
			t.Fatal(err)
		}
		if err := opts.Signal(context.Background(), detach.Unit(id), syscall.SIGTERM); err != nil {
			t.Fatalf("%s: Signal: %v", name, err)
		}
		x := waitExit(t, dir, 10*time.Second)
		return x, strings.TrimSpace(string(waitFile(t, filepath.Join(dir, "marker"), time.Second)))
	}
	px, pmarker := run("process", func(ops string) detach.Options {
		return detach.ProcessBackend(detach.ProcessConfig{OpsDir: ops, Executable: exe, CgroupRoot: "-"})
	})
	lx, lmarker := run("local", func(string) detach.Options { return detachtest.Local() })

	if pmarker != "term" || lmarker != "term" {
		t.Errorf("markers: process %q, local %q; want term", pmarker, lmarker)
	}
	if !px.Canceled || px.ExitCode != 3 {
		t.Errorf("process exit.json %+v, want canceled with exit code 3", px)
	}
	px.Started, px.Ended, lx.Started, lx.Ended = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if px != lx {
		t.Errorf("exit.json differs:\nprocess %+v\nlocal   %+v", px, lx)
	}
}

// nginx-provider TC-16: ListUnits reports only live identities, as unit
// labels (FR-20 item 3, FR-21).
func TestListUnitsReturnsLiveIdentitiesOnly(t *testing.T) {
	l, opts, ops := processLauncher(t, "")
	endSupervisorAtCleanup(t, l.Dir("a"))
	if err := l.Start(context.Background(), "a", shSpec(l.Dir("a"), "sleep 30")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A SIGTERM before the supervisor's signal.Notify kills it with no
		// exit.json; it unlinks spec.json only after that, so the cancel
		// waits for that first.
		waitNoFile(t, filepath.Join(l.Dir("a"), detach.SpecFile), 10*time.Second)
		if err := l.Cancel(context.Background(), "a"); err == nil {
			waitExit(t, l.Dir("a"), 10*time.Second)
		}
	})

	// b: its process has exited and been reaped.
	gone := exec.Command("sleep", "30")
	if err := gone.Start(); err != nil {
		t.Fatal(err)
	}
	b := identityOf(t, gone.Process.Pid)
	_ = gone.Process.Kill() // Ends it; Wait reaps it
	_ = gone.Wait()
	// c: a live process, with another boot's ID.
	c := identityOf(t, os.Getpid())
	c.BootID = "00000000-1111-2222-3333-444444444444"
	for name, ident := range map[string]detach.Identity{"b": b, "c": c} {
		dir := filepath.Join(ops, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := detach.WriteIdentity(dir, ident); err != nil {
			t.Fatal(err)
		}
	}

	units, err := opts.ListUnits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{detach.Unit("a")}; !slices.Equal(units, want) {
		t.Fatalf("ListUnits %q, want %q", units, want)
	}
	// As restart recovery parses an orphan unit (internal/ops/recovery.go).
	if id := strings.TrimSuffix(strings.TrimPrefix(units[0], detach.UnitPrefix), ".service"); id != "a" {
		t.Fatalf("unit %s parses to %q", units[0], id)
	}
}

// identityOf is process pid's identity, read as the launcher reads it.
func identityOf(t *testing.T, pid int) detach.Identity {
	t.Helper()
	ticks, err := strconv.ParseUint(statOf(t, pid, 22), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	return detach.Identity{PID: pid, StartTicks: ticks, BootID: strings.TrimSpace(string(boot))}
}

// nginx-provider TC-17: supervisor diagnostics go to supervisor.log and are
// capped (FR-20 item 5). The supervisor is this test binary, whose
// op-supervise floods its diagnostics once it has its identity.
func TestSupervisorLogIsCapped(t *testing.T) {
	l, _, _ := processLauncher(t, testBinary(t))
	const id = "01JTC17"
	dir := l.Dir(id)
	endSupervisorAtCleanup(t, dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flood-diag"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Start(context.Background(), id, shSpec(dir, "exit 0")); err != nil {
		t.Fatal(err)
	}
	if x := waitExit(t, dir, 60*time.Second); x.ExitCode != 0 {
		t.Fatalf("exit.json %+v", x)
	}
	path := filepath.Join(dir, detach.SupervisorLog)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("%s mode %v, want 0600", detach.SupervisorLog, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	marker := lines[len(lines)-1]
	if !strings.Contains(marker, "truncated") {
		t.Fatalf("last line %q is not the truncation marker", marker)
	}
	if max := oplog.DefaultCapBytes + int64(len(marker)+1); fi.Size() > max || fi.Size() < oplog.DefaultCapBytes/2 {
		t.Errorf("%s is %d bytes, want the cap's worth and at most %d", detach.SupervisorLog, fi.Size(), max)
	}
	// Everything the supervisor wrote is its own diagnostics, complete lines.
	for i, line := range lines {
		if !strings.HasPrefix(line, "op-supervise: ") {
			t.Fatalf("line %d of %s is %q", i+1, detach.SupervisorLog, line)
		}
	}
}

// nginx-provider TC-ERR3: a supervisor that never sees its identity exits
// without running anything (FR-20 item 2, L-51).
func TestHandshakeTimeoutRunsNothing(t *testing.T) {
	dir := t.TempDir()
	if err := detach.WriteSpec(dir, shSpec(dir, `: > "$DIR/marker"`)); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(testBinary(t), "op-supervise", "--dir", dir, "--process")
	cmd.Env = []string{"NEXUS_TEST_HANDSHAKE_TIMEOUT=500ms"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var diag bytes.Buffer
	cmd.Stderr = &diag
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("supervisor: %v, want exit status 1; diagnostics:\n%s", err, diag.String())
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill() // Fails the test either way
		<-done
		t.Fatal("supervisor still waiting after 2s")
	}
	if !strings.Contains(diag.String(), "did not name this process") {
		t.Errorf("diagnostics %q do not explain the exit", diag.String())
	}
	if _, err := os.Stat(filepath.Join(dir, detach.SpecFile)); err != nil {
		t.Errorf("spec.json was taken: %v", err)
	}
	for _, name := range []string{detach.ExitFile, "marker"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s exists (%v)", name, err)
		}
	}
}

// nginx-provider TC-ERR4: a failed identity write kills the supervisor and
// fails the launch (FR-20 item 2).
func TestIdentityWriteFailureKillsSupervisor(t *testing.T) {
	l, _, _ := processLauncher(t, "")
	var tried detach.Identity
	saved := *detach.WriteIdentityHook
	*detach.WriteIdentityHook = func(_ string, id detach.Identity) error {
		tried = id
		return syscall.EIO
	}
	t.Cleanup(func() { *detach.WriteIdentityHook = saved })

	const id = "01JTCERR4"
	dir := l.Dir(id)
	if err := l.Start(context.Background(), id, shSpec(dir, `: > "$DIR/marker"`)); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Start: %v, want EIO", err)
	}
	if tried.PID == 0 {
		t.Fatal("the identity write was never attempted")
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", tried.PID))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err == nil && !strings.Contains(string(b), " "+strconv.FormatUint(tried.StartTicks, 10)+" ") {
			break // The PID was reused by another process
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor %d still exists after 1s", tried.PID)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, detach.SpecFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("spec.json not removed (%v)", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "marker")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the child ran (%v)", err)
	}
}

// CR-A3: writeAtomic renames supervisor.json into place before it syncs the
// directory, so a sync error leaves a valid identity the supervisor may
// already have read. The launch is then not failed (it would orphan the
// child the supervisor started): the record is in place.
func TestIdentityRenamedButNotSyncedLaunchSucceeds(t *testing.T) {
	l, _, _ := processLauncher(t, "")
	saved := *detach.WriteIdentityHook
	*detach.WriteIdentityHook = func(dir string, id detach.Identity) error {
		if err := detach.WriteIdentity(dir, id); err != nil {
			return err
		}
		return syscall.EIO // The directory sync after the rename failed
	}
	t.Cleanup(func() { *detach.WriteIdentityHook = saved })

	const id = "01JTCRA3"
	dir := l.Dir(id)
	endSupervisorAtCleanup(t, dir)
	if err := l.Start(context.Background(), id, shSpec(dir, `: > "$DIR/marker"`)); err != nil {
		t.Fatalf("Start: %v, want success: the identity is in place", err)
	}
	x := waitExit(t, dir, 10*time.Second)
	if x.Canceled || x.Interrupted || x.ExitCode != 0 {
		t.Errorf("exit = %+v, want a normal exit 0", x)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); err != nil {
		t.Errorf("the child did not run: %v", err)
	}
}

// CR-A13: under fd exhaustion the directory sync after the rename fails with
// EMFILE, and so does any read of the record that follows. The record is in
// place all the same, so the launch is not failed: the supervisor has read
// it and started the child, which a kill of the supervisor would orphan.
func TestIdentityRenamedEMFILEOnSyncAndReadBackLaunchSucceeds(t *testing.T) {
	l, _, _ := processLauncher(t, "")
	savedSync, savedRead := *detach.SyncDirHook, *detach.ReadIdentityHook
	t.Cleanup(func() { *detach.SyncDirHook, *detach.ReadIdentityHook = savedSync, savedRead })
	var syncs, reads int
	*detach.SyncDirHook = func(dir string) error {
		if _, err := os.Stat(filepath.Join(dir, detach.IdentityFile)); err != nil {
			return savedSync(dir) // spec.json and the other files before the identity
		}
		syncs++
		return &os.PathError{Op: "open", Path: dir, Err: syscall.EMFILE}
	}
	*detach.ReadIdentityHook = func(string) (detach.Identity, error) {
		reads++
		return detach.Identity{}, &os.PathError{Op: "open", Path: detach.IdentityFile, Err: syscall.EMFILE}
	}

	const id = "01JTCRA13"
	dir := l.Dir(id)
	endSupervisorAtCleanup(t, dir)
	if err := l.Start(context.Background(), id, shSpec(dir, `: > "$DIR/marker"`)); err != nil {
		t.Fatalf("Start: %v, want success: the identity was renamed into place (syncs %d, read-backs %d)", err, syncs, reads)
	}
	if syncs == 0 {
		t.Fatal("the directory sync after the identity rename was never made to fail")
	}
	x := waitExit(t, dir, 10*time.Second)
	if x.Canceled || x.Interrupted || x.ExitCode != 0 {
		t.Errorf("exit = %+v, want a normal exit 0", x)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); err != nil {
		t.Errorf("the child did not run: %v", err)
	}
}

// startupWindowDir makes operation id's directory with a delay-notify file:
// its supervisor (this test binary) starts Supervise only after delay.
func startupWindowDir(t *testing.T, l *detach.Launcher, id string, delay time.Duration) string {
	t.Helper()
	dir := l.Dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "delay-notify"), []byte(delay.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	endSupervisorAtCleanup(t, dir)
	return dir
}

// WI 3.4 finding: a cancel sent as soon as Start returns reaches a
// supervisor that already handles SIGTERM, so the operation ends canceled
// from exit.json instead of lost. The supervisor here takes a second to
// reach signal.Notify, far longer than a real one, which makes the window
// certain rather than likely.
func TestCancelRightAfterStartEndsCanceled(t *testing.T) {
	l, _, _ := processLauncher(t, testBinary(t))
	const id = "01JSIGTERM1"
	dir := startupWindowDir(t, l, id, time.Second)
	if err := l.Start(context.Background(), id, shSpec(dir, `: > "$DIR/marker"; sleep 30`)); err != nil {
		t.Fatal(err)
	}
	if err := l.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel right after Start: %v", err)
	}

	// Lost takes LivenessInterval plus RecheckDelay to be confirmed.
	ctx, cancel := context.WithTimeout(context.Background(), detach.LivenessInterval+detach.RecheckDelay+15*time.Second)
	defer cancel()
	var o detach.Outcome
	select {
	case o = <-l.Watch(ctx, id, time.Now(), time.Hour):
	case <-ctx.Done():
		t.Fatal("no outcome")
	}
	if o.Exit == nil {
		t.Fatalf("outcome %+v, want exit.json (lost: %v); %s:\n%s", o, o.Lost, detach.SupervisorLog, readFileOrErr(filepath.Join(dir, detach.SupervisorLog)))
	}
	if !o.Exit.Canceled || o.Exit.ExitCode != -1 || o.Exit.Error != "" {
		t.Errorf("exit.json %+v, want canceled before the child started (exit code -1)", *o.Exit)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the child ran after the cancel (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, detach.SpecFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("spec.json left behind (%v)", err)
	}
}

// readFileOrErr is path's content, or why it cannot be read, for a message.
func readFileOrErr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// A supervisor that does not reach signal.Notify within the launch bound
// fails the launch: it is killed, spec.json is removed and nothing runs.
func TestLaunchFailsWhenSupervisorNeverReady(t *testing.T) {
	l, opts, _ := processLauncher(t, testBinary(t))
	const id = "01JSIGTERM2"
	dir := startupWindowDir(t, l, id, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := l.Start(ctx, id, shSpec(dir, `: > "$DIR/marker"`))
	if err == nil || !strings.Contains(err.Error(), "did not report that it handles a cancel") {
		t.Fatalf("Start: %v, want a launch that failed waiting for the supervisor", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("Start took %s, past its 500ms bound", took)
	}
	if _, err := os.Stat(filepath.Join(dir, detach.SpecFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("spec.json not removed (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, detach.IdentityFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s written for a failed launch (%v)", detach.IdentityFile, err)
	}
	if lv := liveness(t, opts, id); lv != detach.Absent {
		t.Errorf("liveness %v, want Absent", lv)
	}
}

// A supervisor that exits before signal.Notify fails the launch at once,
// not at the end of the bound.
func TestLaunchFailsWhenSupervisorExitsBeforeReady(t *testing.T) {
	l, _, _ := processLauncher(t, testBinary(t))
	const id = "01JSIGTERM3"
	dir := startupWindowDir(t, l, id, 0)
	if err := os.WriteFile(filepath.Join(dir, "exit-before-notify"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := l.Start(context.Background(), id, shSpec(dir, `: > "$DIR/marker"`))
	if err == nil || !strings.Contains(err.Error(), "exited before") {
		t.Fatalf("Start: %v, want a launch that failed on the supervisor's exit", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("Start took %s to see the supervisor exit", took)
	}
	if _, err := os.Stat(filepath.Join(dir, detach.SpecFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("spec.json not removed (%v)", err)
	}
}

// The readiness pipe ends in the supervisor: the operation's child does not
// inherit it.
func TestReadinessPipeNotInheritedByChild(t *testing.T) {
	l, _, _ := processLauncher(t, "")
	const id = "01JSIGTERM4"
	dir := l.Dir(id)
	endSupervisorAtCleanup(t, dir)
	// Descriptor 3 is where the supervisor received the pipe. The shell
	// opens a redirection there and moves it to 1 before readlink runs.
	spec := shSpec(dir, `readlink /proc/$$/fd/3 > "$DIR/fd3.tmp"; mv "$DIR/fd3.tmp" "$DIR/fd3"`)
	if err := l.Start(context.Background(), id, spec); err != nil {
		t.Fatal(err)
	}
	waitExit(t, dir, 10*time.Second)
	b, err := os.ReadFile(filepath.Join(dir, "fd3"))
	if err != nil {
		t.Fatal(err)
	}
	if fd3 := strings.TrimSpace(string(b)); strings.HasPrefix(fd3, "pipe:") {
		t.Errorf("the child inherited the readiness pipe as fd 3 (%s)", fd3)
	}
}
