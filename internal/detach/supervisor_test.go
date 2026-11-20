// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package detach_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/oplog"
)

// lockedBuffer collects a process's diagnostics.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// supervisor is one real `nexusd op-supervise --dir` process.
type supervisor struct {
	cmd    *exec.Cmd
	dir    string
	diag   *lockedBuffer
	done   chan struct{}
	err    error // Of Wait, once done is closed
	waited time.Time
}

func nexusd(t *testing.T) string {
	t.Helper()
	exe, err := detachtest.Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// shSpec runs script with /bin/sh, with DIR set to the operation directory.
func shSpec(dir, script string) detach.Spec {
	return detach.Spec{
		Argv:       []string{"/bin/sh", "-c", script},
		Env:        []string{"DIR=" + dir, "PATH=/usr/bin:/bin"},
		KillMode:   detach.KillControlGroup,
		MaxRuntime: time.Hour,
		Grace:      time.Second,
	}
}

// startSupervisor writes spec (unless nil) and starts the supervisor on
// dir in its own session, as the launcher does.
func startSupervisor(t *testing.T, dir string, spec *detach.Spec, configure ...func(*exec.Cmd)) *supervisor {
	t.Helper()
	if spec != nil {
		if err := detach.WriteSpec(dir, *spec); err != nil {
			t.Fatal(err)
		}
	}
	s := &supervisor{dir: dir, diag: &lockedBuffer{}, done: make(chan struct{})}
	s.cmd = exec.Command(nexusd(t), "op-supervise", "--dir", dir)
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	s.cmd.Stderr = s.diag
	for _, c := range configure {
		c(s.cmd)
	}
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		s.err = s.cmd.Wait()
		s.waited = time.Now()
		close(s.done)
	}()
	t.Cleanup(func() {
		select {
		case <-s.done:
		default:
			_ = s.cmd.Process.Kill() // Best effort after a failed test; Wait reports the rest.
			<-s.done
		}
	})
	return s
}

// wait waits for the supervisor to exit and returns its exit status.
func (s *supervisor) wait(t *testing.T, limit time.Duration) int {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(limit):
		t.Fatalf("supervisor still running after %s; diagnostics:\n%s", limit, s.diag.String())
	}
	var ee *exec.ExitError
	if s.err != nil && !errors.As(s.err, &ee) {
		t.Fatalf("supervisor: %v", s.err)
	}
	return s.cmd.ProcessState.ExitCode()
}

func readExit(t *testing.T, dir string) detach.Exit {
	t.Helper()
	x, ok, err := detach.ReadExit(dir)
	if err != nil || !ok {
		t.Fatalf("exit.json: ok %v, err %v", ok, err)
	}
	return x
}

// frames reads every output frame of the operation in dir.
func frames(t *testing.T, dir string) []apitypes.Frame {
	t.Helper()
	p, err := oplog.NewReader(filepath.Join(dir, detach.OutputDir)).Read(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return p.Frames
}

func output(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, f := range frames(t, dir) {
		b.WriteString(f.D)
	}
	return b.String()
}

// waitOutput polls the operation's output until it matches re.
func waitOutput(t *testing.T, dir string, re *regexp.Regexp) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, detach.OutputDir)); err == nil {
			if m := re.FindStringSubmatch(output(t, dir)); m != nil {
				return m
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output never matched %s; output so far %q", re, func() string {
		if _, err := os.Stat(filepath.Join(dir, detach.OutputDir)); err != nil {
			return ""
		}
		return output(t, dir)
	}())
	return nil
}

var pidRE = regexp.MustCompile(`pid=(\d+)`)

func childPID(t *testing.T, dir string) int {
	t.Helper()
	n, err := strconv.Atoi(waitOutput(t, dir, pidRE)[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// groupGone waits for process group pgid to have no member left.
func groupGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still has members (kill: %v)", pgid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// getsid is getsid(2); the syscall package exports it only on some
// systems.
func getsid(pid int) (int, error) {
	r, _, errno := syscall.Syscall(syscall.SYS_GETSID, uintptr(pid), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

func killGroup(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // Cleanup: the group may already be gone.
}

// TC-58: the real supervisor runs a real child and writes exit.json
// atomically: spec.json (0600) is unlinked before the child's first
// output, exit.json is absent or complete at every observation, and no
// temporary file remains.
func TestSupervisorRunsAChildAndWritesExitAtomically(t *testing.T) {
	dir := t.TempDir()
	spec := shSpec(dir, `if [ -e "$DIR/spec.json" ]; then echo spec=present; else echo spec=absent; fi
echo working; sleep 0.3; echo done; exit 7`)
	if err := detach.WriteSpec(dir, spec); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, detach.SpecFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("spec.json mode %v, want 0600", fi.Mode().Perm())
	}

	stop := make(chan struct{})
	var observations, complete int
	var partial []string
	var watcher sync.WaitGroup
	watcher.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(filepath.Join(dir, detach.ExitFile))
			observations++
			if err == nil {
				var x detach.Exit
				dec := json.NewDecoder(bytes.NewReader(b))
				dec.DisallowUnknownFields()
				if derr := dec.Decode(&x); derr != nil || x.Ended.IsZero() {
					partial = append(partial, string(b))
				} else {
					complete++
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				partial = append(partial, err.Error())
			}
			time.Sleep(time.Millisecond)
		}
	})
	s := startSupervisor(t, dir, nil)
	code := s.wait(t, 20*time.Second)
	time.Sleep(20 * time.Millisecond) // A few observations of the final file
	close(stop)
	watcher.Wait()

	if code != 0 {
		t.Fatalf("supervisor exit %d; diagnostics:\n%s", code, s.diag.String())
	}
	if len(partial) > 0 {
		t.Fatalf("exit.json was observed partial: %q", partial)
	}
	if complete == 0 || observations < 50 {
		t.Fatalf("%d observations, %d complete; the poll did not watch the run", observations, complete)
	}
	got := output(t, dir)
	if !strings.HasPrefix(got, "spec=absent\n") || !strings.Contains(got, "done\n") {
		t.Fatalf("output %q: spec.json must be gone before the child's first output", got)
	}
	x := readExit(t, dir)
	if x.ExitCode != 7 || x.Signal != "" || x.TimedOut || x.Canceled || x.Error != "" {
		t.Fatalf("exit = %+v, want code 7, no signal, not timed out or canceled", x)
	}
	if x.Started.IsZero() || x.Ended.Before(x.Started) || x.Ended.Sub(x.Started) < 250*time.Millisecond {
		t.Fatalf("started %v, ended %v", x.Started, x.Ended)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != detach.ExitFile && e.Name() != detach.OutputDir {
			t.Fatalf("left behind in the operation directory: %s", e.Name())
		}
	}
}

// TC-59: the child reads EOF from stdin at once, leads its own session with
// no controlling terminal, and its stdout and stderr arrive as separate
// streams in arrival order (A-9).
func TestChildProcessIsolation(t *testing.T) {
	dir := t.TempDir()
	spec := shSpec(dir, `echo pid=$$
if read -r line; then echo stdin=data; else echo stdin=eof; fi
if (exec 3</dev/tty) 2>/dev/null; then echo tty=yes; else echo tty=no; fi
sleep 0.5
echo e1 >&2; sleep 0.1; echo o2; sleep 0.1; echo e2 >&2`)
	// The supervisor's own stdin is a live pipe that never delivers EOF:
	// the child must not inherit it.
	stdin, holdOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer holdOpen.Close()
	s := startSupervisor(t, dir, &spec, func(c *exec.Cmd) { c.Stdin = stdin })
	pid := childPID(t, dir)
	sid, err := getsid(pid)
	if err != nil {
		t.Fatalf("getsid(%d): %v", pid, err)
	}
	if code := s.wait(t, 20*time.Second); code != 0 {
		t.Fatalf("supervisor exit %d:\n%s", code, s.diag.String())
	}
	if sid != pid {
		t.Fatalf("child %d is in session %d, want its own", pid, sid)
	}
	got := output(t, dir)
	if !strings.Contains(got, "stdin=eof\n") || !strings.Contains(got, "tty=no\n") {
		t.Fatalf("output %q: want stdin=eof and tty=no", got)
	}
	var tail []string
	for _, f := range frames(t, dir) {
		if strings.HasPrefix(f.D, "e") || strings.HasPrefix(f.D, "o2") {
			tail = append(tail, f.S+":"+f.D)
		}
	}
	want := []string{"err:e1\n", "out:o2\n", "err:e2\n"}
	if strings.Join(tail, "|") != strings.Join(want, "|") {
		t.Fatalf("frames %q, want %q", tail, want)
	}
}

// TC-60: on timeout the child's group gets SIGTERM; a child that exits on it
// records timedOut with its marker, and one that ignores it is killed by
// SIGKILL after the grace period. No process of either group remains.
func TestTimeoutEscalatesToSIGKILL(t *testing.T) {
	t.Run("exits on SIGTERM", func(t *testing.T) {
		dir := t.TempDir()
		spec := shSpec(dir, `echo pid=$$; trap 'echo marker > "$DIR/marker"; exit 0' TERM
while :; do sleep 0.1; done`)
		spec.Timeout, spec.Grace = time.Second, time.Second
		s := startSupervisor(t, dir, &spec)
		pid := childPID(t, dir)
		t.Cleanup(func() { killGroup(pid) })
		if code := s.wait(t, 20*time.Second); code != 0 {
			t.Fatalf("supervisor exit %d:\n%s", code, s.diag.String())
		}
		x := readExit(t, dir)
		if !x.TimedOut || x.Canceled || x.ExitCode != 0 || x.Signal != "" {
			t.Fatalf("exit = %+v, want timed out with code 0", x)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "marker")); err != nil || string(b) != "marker\n" {
			t.Fatalf("marker %q, %v", b, err)
		}
		groupGone(t, pid)
	})
	t.Run("ignores SIGTERM", func(t *testing.T) {
		dir := t.TempDir()
		spec := shSpec(dir, `echo pid=$$; trap '' TERM
while :; do sleep 0.1; done`)
		spec.Timeout, spec.Grace = time.Second, time.Second
		s := startSupervisor(t, dir, &spec)
		pid := childPID(t, dir)
		t.Cleanup(func() { killGroup(pid) })
		if code := s.wait(t, 20*time.Second); code != 0 {
			t.Fatalf("supervisor exit %d:\n%s", code, s.diag.String())
		}
		x := readExit(t, dir)
		if !x.TimedOut || x.Signal != "SIGKILL" || x.ExitCode != -1 {
			t.Fatalf("exit = %+v, want timed out, killed by SIGKILL", x)
		}
		if el := x.Ended.Sub(x.Started); el < 1900*time.Millisecond || el > 3500*time.Millisecond {
			t.Fatalf("elapsed %s, want close to 2s (timeout 1s + grace 1s)", el)
		}
		groupGone(t, pid)
	})
}

// TC-61: SIGTERM to the supervisor alone, after nexusd recorded its
// cancel request (as Launcher.Cancel does), is the cancel: the child and
// its grandchild in the same group both receive SIGTERM, then SIGKILL after
// the grace period, exit.json says canceled, and the supervisor exits after
// writing it. Without the request it is an interruption
// (TestUnrequestedSIGTERMIsNotACancel).
func TestCancelThroughSupervisorSIGTERM(t *testing.T) {
	dir := t.TempDir()
	spec := shSpec(dir, `sh -c 'trap "echo gc-term >> \"$DIR/gc\"; exit 0" TERM; echo gc=$$; while :; do sleep 0.1; done' &
trap 'echo child-term >> "$DIR/child"' TERM
echo pid=$$
while :; do sleep 0.1; done`)
	spec.Grace = time.Second
	s := startSupervisor(t, dir, &spec)
	pid := childPID(t, dir)
	waitOutput(t, dir, regexp.MustCompile(`gc=\d+`))
	t.Cleanup(func() { killGroup(pid) })

	if err := detach.RequestCancel(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.wait(t, 20*time.Second); code != 0 {
		t.Fatalf("supervisor exit %d:\n%s", code, s.diag.String())
	}
	x := readExit(t, dir)
	if !x.Canceled || x.TimedOut {
		t.Fatalf("exit = %+v, want canceled", x)
	}
	if x.Signal != "SIGKILL" {
		t.Fatalf("exit = %+v: the child kept running after SIGTERM, so SIGKILL must have ended it", x)
	}
	for file, want := range map[string]string{"gc": "gc-term\n", "child": "child-term\n"} {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || !strings.Contains(string(b), want) {
			t.Fatalf("%s: %q, %v; want %q", file, b, err, want)
		}
	}
	// exit.json was in place when the supervisor exited.
	if fi, err := os.Stat(filepath.Join(dir, detach.ExitFile)); err != nil || fi.ModTime().After(s.waited) {
		t.Fatalf("exit.json %v, %v; supervisor exited at %v", fi, err, s.waited)
	}
	groupGone(t, pid)
}

// TC-E11: a background grandchild holding the child's stdout cannot hang
// completion: exit.json is written within PipeWaitDelay plus slack.
func TestBackgroundGrandchildCannotHangCompletion(t *testing.T) {
	dir := t.TempDir()
	spec := shSpec(dir, `echo pid=$$; sleep 300 & exit 0`)
	start := time.Now()
	s := startSupervisor(t, dir, &spec)
	pid := childPID(t, dir)
	t.Cleanup(func() { killGroup(pid) })
	if code := s.wait(t, detach.PipeWaitDelay+10*time.Second); code != 0 {
		t.Fatalf("supervisor exit %d:\n%s", code, s.diag.String())
	}
	if el := time.Since(start); el > detach.PipeWaitDelay+3*time.Second {
		t.Fatalf("completion took %s, want within %s plus slack", el, detach.PipeWaitDelay)
	}
	if x := readExit(t, dir); x.ExitCode != 0 || x.Signal != "" {
		t.Fatalf("exit = %+v, want code 0", x)
	}
}

// TC-E12: SIGHUP and SIGPIPE do not stop the supervisor, nor does its
// parent closing its stdout: the output log is complete and exit.json has
// the child's real status.
func TestSupervisorIgnoresSIGHUPAndSIGPIPE(t *testing.T) {
	dir := t.TempDir()
	spec := shSpec(dir, `echo pid=$$; i=1; while [ $i -le 8 ]; do echo line$i; i=$((i+1)); sleep 0.1; done; exit 3`)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s := startSupervisor(t, dir, &spec, func(c *exec.Cmd) { c.Stdout = w })
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	childPID(t, dir)
	// The parent closes the supervisor's stdout, then sends both signals.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGPIPE} {
		if err := s.cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
	}
	if code := s.wait(t, 20*time.Second); code != 0 {
		t.Fatalf("supervisor exit %d (state %v):\n%s", code, s.cmd.ProcessState, s.diag.String())
	}
	got := output(t, dir)
	for i := 1; i <= 8; i++ {
		if !strings.Contains(got, "line"+strconv.Itoa(i)+"\n") {
			t.Fatalf("output %q misses line%d", got, i)
		}
	}
	if x := readExit(t, dir); x.ExitCode != 3 || x.Canceled || x.TimedOut {
		t.Fatalf("exit = %+v, want the child's code 3", x)
	}
}

// TC-ERR19: with no spec.json, as for a restarted unit, the supervisor
// exits non-zero with a clear message, starts no child and writes no
// exit.json.
func TestMissingSpecPreventsAnyRerun(t *testing.T) {
	dir := t.TempDir()
	s := startSupervisor(t, dir, nil)
	code := s.wait(t, 20*time.Second)
	if code == 0 {
		t.Fatal("supervisor exited 0 without a spec")
	}
	if msg := s.diag.String(); !strings.Contains(msg, "no spec.json") || !strings.Contains(msg, "not run again") {
		t.Fatalf("message %q", msg)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the supervisor left %v: no child may start and no exit.json be written", entries)
	}
}

// Work item 2.1: `nexusd op-supervise --dir D --process` takes nothing until
// supervisor.json names it, then runs the operation as a unit's supervisor
// does, with the umask a systemd transient unit has (L-47, L-51).
func TestProcessModeWaitsForItsIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process mode reads /proc")
	}
	dir := t.TempDir()
	spec := shSpec(dir, `grep '^Umask:' /proc/self/status > "$DIR/marker"`)
	s := startSupervisor(t, dir, &spec, func(c *exec.Cmd) { c.Args = append(c.Args, "--process") })

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, detach.SpecFile)); err != nil {
		t.Fatalf("spec.json taken before the identity was written: %v", err)
	}
	pid := s.cmd.Process.Pid
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	ticks, err := strconv.ParseUint(fields[22-3], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	if err := detach.WriteIdentity(dir, detach.Identity{PID: pid, StartTicks: ticks, BootID: strings.TrimSpace(string(boot))}); err != nil {
		t.Fatal(err)
	}

	if code := s.wait(t, 20*time.Second); code != 0 {
		t.Fatalf("supervisor exit status %d; diagnostics:\n%s", code, s.diag.String())
	}
	if x := readExit(t, dir); x.ExitCode != 0 {
		t.Fatalf("exit = %+v", x)
	}
	marker, err := os.ReadFile(filepath.Join(dir, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(strings.TrimPrefix(string(marker), "Umask:")); got != "0022" {
		t.Fatalf("child umask %q, want 0022", got)
	}
}
