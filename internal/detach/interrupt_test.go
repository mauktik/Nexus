package detach_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/detach"
)

// exitFields is exit.json decoded as a map, so a test names its fields as
// they are on disk.
func exitFields(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, detach.ExitFile))
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("exit.json %s: %v", b, err)
	}
	return f
}

// interruptedNotCanceled reports why exit.json fields f are not those of an
// operation stopped by a SIGTERM nobody asked nexusd for: interrupted, and
// neither canceled nor timed out. Empty when they are.
func interruptedNotCanceled(f map[string]any) string {
	if f["interrupted"] != true || f["canceled"] != false || f["timedOut"] != false {
		return "want interrupted true, canceled false, timedOut false"
	}
	return ""
}

// nginx-provider reboot-race finding (WI 3.5 dry run): a SIGTERM that
// nexusd did not ask for, as systemd sends a unit's main process when it
// stops the unit at shutdown, still stops the child as a cancel does, but it
// is not a cancel. exit.json says interrupted, so the operation ends
// outcome-unknown, not Canceled (TC-51 Expected 5).
func TestUnrequestedSIGTERMIsNotACancel(t *testing.T) {
	dir := t.TempDir()
	spec := shSpec(dir, `trap 'echo child-term >> "$DIR/child"; exit 143' TERM
echo pid=$$
while :; do sleep 0.1; done`)
	s := startSupervisor(t, dir, &spec)
	pid := childPID(t, dir)
	t.Cleanup(func() { killGroup(pid) })

	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.wait(t, 20*time.Second); code != 0 {
		t.Fatalf("supervisor exit %d:\n%s", code, s.diag.String())
	}
	if f := exitFields(t, dir); interruptedNotCanceled(f) != "" {
		t.Fatalf("exit.json %v: %s", f, interruptedNotCanceled(f))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "child")); err != nil || !strings.Contains(string(b), "child-term") {
		t.Fatalf("the child was not sent SIGTERM: %q %v", b, err)
	}
	groupGone(t, pid)
}

// The same finding as init causes it: at shutdown BusyBox init (and
// systemd's final kill) sends SIGTERM to every process, so the supervisor
// and the child get it together, in either order. The child may die of it
// before the supervisor handles its own. Whatever the order, exit.json is
// never a cancel and never the child's own failure: it says interrupted.
// Each round alternates which of the two is signaled first.
func TestShutdownSIGTERMToEveryProcessIsNotAnOutcome(t *testing.T) {
	for round := range 10 {
		dir := t.TempDir()
		spec := shSpec(dir, `echo pid=$$; while :; do sleep 0.1; done`)
		s := startSupervisor(t, dir, &spec)
		pid := childPID(t, dir)
		t.Cleanup(func() { killGroup(pid) })
		waitOutput(t, dir, regexp.MustCompile(`pid=\d+`))

		order, targets := "supervisor first", []int{s.cmd.Process.Pid, -pid}
		if round%2 == 1 {
			order, targets = "child first", []int{-pid, s.cmd.Process.Pid}
		}
		for _, p := range targets {
			if err := syscall.Kill(p, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
		if code := s.wait(t, 20*time.Second); code != 0 {
			t.Fatalf("round %d: supervisor exit %d:\n%s", round, code, s.diag.String())
		}
		if f := exitFields(t, dir); interruptedNotCanceled(f) != "" {
			t.Errorf("round %d (%s): exit.json %v: %s", round, order, f, interruptedNotCanceled(f))
		}
	}
}

// Follow-up to the reboot-race fix: SIGINT is the operator's explicit
// host-side cancel (docs/security.md), `systemctl kill --kill-whom=main
// --signal=SIGINT` on systemd and `kill -INT` on OpenRC, since a SIGTERM
// without nexusd's request is now an interruption. Neither init sends
// SIGINT to services or processes at shutdown. The supervisor records the
// request itself and stops the child as a cancel does: the child gets
// SIGTERM, its group is gone, and exit.json says canceled.
func TestSIGINTIsTheOperatorsCancel(t *testing.T) {
	dir := t.TempDir()
	spec := shSpec(dir, `trap 'echo child-term >> "$DIR/child"; exit 143' TERM
echo pid=$$
while :; do sleep 0.1; done`)
	s := startSupervisor(t, dir, &spec)
	pid := childPID(t, dir)
	t.Cleanup(func() { killGroup(pid) })

	if err := s.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := s.wait(t, 20*time.Second); code != 0 {
		t.Fatalf("supervisor exit %d:\n%s", code, s.diag.String())
	}
	if f := exitFields(t, dir); f["canceled"] != true || f["interrupted"] != nil || f["timedOut"] != false {
		t.Fatalf("exit.json %v: want canceled true, no interrupted, timedOut false", f)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "child")); err != nil || !strings.Contains(string(b), "child-term") {
		t.Fatalf("the child was not sent SIGTERM: %q %v", b, err)
	}
	groupGone(t, pid)
	if ok, err := detach.CancelRequested(dir); !ok || err != nil {
		t.Errorf("no cancel request recorded after SIGINT: %v %v", ok, err)
	}
}
