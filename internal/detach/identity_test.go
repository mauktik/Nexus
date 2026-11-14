//go:build linux

package detach

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Seams of this package for the process backend tests in package
// detach_test, which use detachtest and so cannot be in this package.
var (
	WriteIdentityHook = &writeIdentity
	ReadIdentityHook  = &readIdentity
	SyncDirHook       = &syncDir
	HandshakeTimeout  = &handshakeTimeout
	DiagHook          = &diagHook
)

// startSleep starts an unrelated `sleep 30` and ends it in cleanup.
func startSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // Ended either way; Wait reaps it
		_ = cmd.Wait()
	})
	return cmd
}

// identityTestBackend is a process backend over a fresh ops directory, and
// the directory of operation "x" in it.
func identityTestBackend(t *testing.T) (Options, string) {
	t.Helper()
	ops := t.TempDir()
	dir := filepath.Join(ops, "x")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return ProcessBackend(ProcessConfig{OpsDir: ops, CgroupRoot: "-"}), dir
}

func realIdentity(t *testing.T, pid int) Identity {
	t.Helper()
	st, err := procStat("/proc", pid)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := bootID("/proc")
	if err != nil {
		t.Fatal(err)
	}
	return Identity{PID: pid, StartTicks: st.StartTicks, BootID: boot}
}

func wantLiveness(t *testing.T, b Options, want Liveness) {
	t.Helper()
	st, err := b.UnitState(context.Background(), Unit("x"))
	if err != nil {
		t.Fatal(err)
	}
	lv, err := st.Liveness()
	if err != nil || lv != want {
		t.Fatalf("UnitState %+v: liveness %v, %v; want %v", st, lv, err, want)
	}
}

// nginx-provider TC-E7: a forged identity with a reused PID is not live and
// cannot be signaled (FR-20 item 2, L-3).
func TestForgedStartTicksIsAbsentAndUnsignalable(t *testing.T) {
	b, dir := identityTestBackend(t)
	sleep := startSleep(t)
	id := realIdentity(t, sleep.Process.Pid)
	id.StartTicks++
	if err := WriteIdentity(dir, id); err != nil {
		t.Fatal(err)
	}

	wantLiveness(t, b, Absent)
	err := b.Signal(context.Background(), Unit("x"), syscall.SIGTERM)
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("Signal: %v, want an identity mismatch", err)
	}
	if err := syscall.Kill(sleep.Process.Pid, 0); err != nil {
		t.Fatalf("the unrelated sleep was hit: kill(pid, 0): %v", err)
	}
}

// nginx-provider TC-E8: an identity from another boot is never live
// (FR-20 item 2, FR-21).
func TestOtherBootIsAbsent(t *testing.T) {
	b, dir := identityTestBackend(t)
	sleep := startSleep(t)
	id := realIdentity(t, sleep.Process.Pid)
	// Control: the exact identity is live, so only the boot ID differs below.
	if err := WriteIdentity(dir, id); err != nil {
		t.Fatal(err)
	}
	wantLiveness(t, b, Live)

	id.BootID = "00000000-1111-2222-3333-444444444444"
	if err := WriteIdentity(dir, id); err != nil {
		t.Fatal(err)
	}
	wantLiveness(t, b, Absent)
}

// nginx-provider TC-E9: a zombie supervisor is ended (FR-20, L-4, L-6).
func TestZombieIsAbsent(t *testing.T) {
	b, dir := identityTestBackend(t)
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Wait() // Reaps the zombie
		}
	})
	id := realIdentity(t, pid)
	// Not reaped, it stays a zombie once it has exited.
	for deadline := time.Now().Add(5 * time.Second); ; {
		st, err := procStat("/proc", pid)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == 'Z' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still in state %c", pid, st.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := WriteIdentity(dir, id); err != nil {
		t.Fatal(err)
	}
	wantLiveness(t, b, Absent)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	reaped = true
	wantLiveness(t, b, Absent)
}

// statLine is a /proc/<pid>/stat line of a process named comm in state S
// with parent 1, session 4321 and start time start, all 52 fields.
func statLine(comm string, start uint64) string {
	fields := []string{"1234", "(" + comm + ")", "S", "1", "1234", "4321", "0", "-1", "4194560",
		"120", "0", "0", "0", "3", "5", "0", "0", "20", "0", "1", "0", fmt.Sprint(start)}
	for len(fields) < 52 {
		fields = append(fields, "0")
	}
	return strings.Join(fields, " ") + "\n"
}

// nginx-provider TC-E10: /proc stat parsing survives hostile comm values
// (FR-20 item 2, L-3).
func TestProcStatParse(t *testing.T) {
	for _, comm := range []string{"nexusd", "a) (b c", "", ") S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 99 ("} {
		got, err := parseStat([]byte(statLine(comm, 987654)))
		if err != nil {
			t.Fatalf("comm (%s): %v", comm, err)
		}
		want := statFields{State: 'S', PPID: 1, Session: 4321, StartTicks: 987654}
		if got != want {
			t.Fatalf("comm (%s): %+v, want %+v", comm, got, want)
		}
	}

	// Truncated before field 22: the start time is missing.
	line := statLine("a) (b c", 987654)
	fields := strings.Fields(line[strings.LastIndexByte(line, ')')+1:])
	truncated := line[:strings.LastIndexByte(line, ')')+1] + " " + strings.Join(fields[:22-3], " ")
	if got, err := parseStat([]byte(truncated)); err == nil {
		t.Fatalf("truncated line parsed as %+v", got)
	}
	for _, bad := range []string{"", "1234 nexusd S 1", "1234 (x) SS" + strings.Repeat(" 1", 30)} {
		if got, err := parseStat([]byte(bad)); err == nil {
			t.Fatalf("%q parsed as %+v", bad, got)
		}
	}
}

// The parse agrees with the kernel on a real process.
func TestProcStatOfSelf(t *testing.T) {
	st, err := procStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != 'R' && st.State != 'S' || st.PPID != os.Getppid() || st.StartTicks == 0 {
		t.Fatalf("procStat(self) = %+v (ppid %d)", st, os.Getppid())
	}
	if _, err := procStat("/proc", 1<<30); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("procStat of no process: %v, want not exist", err)
	}
}

// No supervisor.json is a definitely absent unit, as a collected unit is;
// an unreadable one is unknown, never absent.
func TestIdentityMissingOrBroken(t *testing.T) {
	b, dir := identityTestBackend(t)
	wantLiveness(t, b, Absent)
	if err := os.WriteFile(filepath.Join(dir, IdentityFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := b.UnitState(context.Background(), Unit("x")); err == nil {
		t.Fatalf("broken identity: %+v, want an error", st)
	}
	if _, err := b.UnitState(context.Background(), "nexus-op-../x.service"); err == nil {
		t.Fatal("a unit name outside the ops directory was accepted")
	}
}
