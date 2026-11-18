// Regression tests of planning/services-provider/code-review.md CR-1 and
// CR-4: a restart that fails while the old instance keeps running, a
// masked unit, and the F-HOSTS fake replaying the captured failures.

package services

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// CR-1: systemctl restart of a running unit that systemd refuses (a masked
// unit: https://www.freedesktop.org/software/systemd/man/latest/systemctl.html,
// "making it impossible to start them"; "Exit status ... a non-zero failure
// code otherwise") leaves the old instance running. The restart did not
// happen, so the operation must fail, not end Succeeded with changed true.
func TestCR1_ASystemdRestartThatFailsWithTheOldInstanceRunningFails(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n",
		ctlOptions{names: []string{"nx-svc-test"}})
	before := w.show("nx-svc-test")
	w.fh.Answer(systemdArgv("restart", "nx-svc-test"), svcctl.Result{ExitCode: 1,
		Output: []byte("Failed to restart nx-svc-test.service: Unit nx-svc-test.service is masked.\n")}, nil)
	r := w.act(spec.Restart.Op(), `{"name":"nx-svc-test"}`, ctlPoll)
	pe := wantProblem(t, r.err, problem.ServiceActionFailed)
	if got := fmt.Sprintf("%s", pe.Extensions["exitCode"]); got != "1" {
		t.Errorf("exitCode = %s, want 1", got)
	}
	if !strings.Contains(pe.Detail, "is masked") || !strings.Contains(pe.Detail, "nx-svc-test") {
		t.Errorf("detail %q lacks the tool's output", pe.Detail)
	}
	if after := w.show("nx-svc-test"); after["MainPID"] != before["MainPID"] || after["ActiveState"] != "active" {
		t.Errorf("the unit changed: %v, was %v", after, before)
	}
}

// CR-1: rc-service restart of a started script whose stop fails exits 1 and
// leaves it started.
func TestCR1_AnOpenRCRestartWhoseStopFailsFails(t *testing.T) {
	w := newCtlWorld(t, ctlOpenRC, "enabled: true\ncontrol:\n  - name: nx-svc-test\n", ctlOptions{})
	w.fh.Answer(openrcArgv("restart", "nx-svc-test"), svcctl.Result{ExitCode: 1,
		Output: []byte(" * Stopping nx-svc-test ...\n * ERROR: nx-svc-test failed to stop\n")}, nil)
	r := w.act(spec.Restart.Op(), `{"name":"nx-svc-test"}`, ctlPoll)
	pe := wantProblem(t, r.err, problem.ServiceActionFailed)
	if got := fmt.Sprintf("%s", pe.Extensions["exitCode"]); got != "1" {
		t.Errorf("exitCode = %s, want 1", got)
	}
	if !strings.Contains(pe.Detail, "failed to stop") {
		t.Errorf("detail %q lacks the tool's output", pe.Detail)
	}
}

// CR-1: a restart whose tool exits non-zero but that did start a new
// instance (a new main PID) is Succeeded: the state is what counts.
func TestCR1_ARestartThatExitsNonZeroButStartedANewInstanceSucceeds(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n",
		ctlOptions{names: []string{"nx-svc-test"}})
	p0 := w.show("nx-svc-test")["MainPID"]
	w.post = func(argv []string, res svcctl.Result) svcctl.Result {
		if slices.Equal(argv, systemdArgv("restart", "nx-svc-test")) {
			res.ExitCode = 1 // The restart happened, and the tool still says it failed.
		}
		return res
	}
	r := w.act(spec.Restart.Op(), `{"name":"nx-svc-test"}`, ctlPoll)
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.log)
	}
	if !r.res.Changed || r.res.MainPID == nil || strconv.Itoa(*r.res.MainPID) == p0 {
		t.Errorf("result %+v, want changed with a main PID other than %s", r.res, p0)
	}
}

// CR-1: preflight does not report a masked unit as ok: systemd refuses
// start and restart of it.
func TestCR1_AMaskedUnitIsNotOKInTheAllowList(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n",
		ctlOptions{names: []string{"nx-svc-test"}, setup: func(fh *svcctltest.Host) {
			fh.Answer(loadStateArgv("nx-svc-test"), svcctl.Result{
				Output: []byte("Id=nx-svc-test.service\nLoadState=masked\n")}, nil)
		}})
	e := allowEntry(t, w.p.Preflight(context.Background()), "nx-svc-test")
	if e.Status == StatusOK || !strings.Contains(e.Reason, "masked") {
		t.Errorf("entry %+v, want a status other than ok whose reason says masked", e)
	}
}

// CR-4: the fake answers as the capture does. A masked unit is masked
// (show-masked.txt: LoadState=masked, UnitFileState=masked) even where the
// capture has no show of it, and start of it fails (systemd man page, as
// above).
func TestCR4_TheFakeRefusesToStartAMaskedUnit(t *testing.T) {
	for _, name := range []string{"cryptdisks-early", "hwclock"} { // a captured show; one only listed in list-unit-files
		t.Run(name, func(t *testing.T) {
			w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: "+name+"\n",
				ctlOptions{names: []string{name}})
			if kv := w.show(name); kv["LoadState"] != "masked" {
				t.Fatalf("show: %v, want LoadState=masked", kv)
			}
			res, err := w.fh.Run(context.Background(), systemdArgv("start", name))
			if err != nil || res.ExitCode == 0 {
				t.Errorf("start of a masked unit: %+v %v, want a non-zero exit", res, err)
			}
			if kv := w.show(name); kv["ActiveState"] != "inactive" {
				t.Errorf("after the refused start: %v, want inactive", kv)
			}
		})
	}
}

// CR-4: the captured nx-svc-fail start exits 1 (actions/start-nx-svc-fail.txt)
// and leaves the service failed (systemd) or stopped (OpenRC), with no
// FailsOnStart call.
func TestCR4_TheFakeReplaysTheCapturedFailingStart(t *testing.T) {
	for _, c := range []struct{ capture, want string }{{ctlSystemd, "failed"}, {ctlOpenRC, "stopped"}} {
		t.Run(c.capture, func(t *testing.T) {
			fh := svcctltest.NewHost(t, c.capture)
			argv := systemdArgv("start", "nx-svc-fail")
			if c.capture == ctlOpenRC {
				argv = openrcArgv("start", "nx-svc-fail")
			}
			captured, err := svcctltest.CaptureOutput(c.capture, "actions/start-nx-svc-fail.txt")
			if err != nil {
				t.Fatal(err)
			}
			res, err := fh.Run(context.Background(), argv)
			if err != nil || res.ExitCode != captured.ExitCode || res.ExitCode == 0 || !bytes.Equal(res.Output, captured.Output) {
				t.Errorf("start: %q exit %d, %v; want %q exit %d", res.Output, res.ExitCode, err, captured.Output,
					captured.ExitCode)
			}
			if c.capture == ctlSystemd {
				res, _ = fh.Run(context.Background(), showArgv("nx-svc-fail"))
				if !strings.Contains(string(res.Output), "ActiveState="+c.want) {
					t.Errorf("show after the failed start:\n%s", res.Output)
				}
			}
		})
	}
}

// CR-2: a dependency lookup that fails at preflight (the 1 s bound expiring
// while PID 1 is busy, a bus error) leaves nexusd's dependencies unknown.
// Control then fails closed, instead of admitting a stop that systemd may
// propagate to nexusd, and recovers when the next preflight resolves them.
func TestCR2_ControlFailsClosedWhileTheDependencyLookupFailed(t *testing.T) {
	depsArgv := []string{systemctlTool, "show", "-p", "Requires", "-p", "BindsTo", "-p", "Requisite", "-p", "PartOf",
		"--", "nexus-agent.service"}
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n",
		ctlOptions{names: []string{"nx-svc-test"}, agent: true, setup: func(fh *svcctltest.Host) {
			fh.Answer(depsArgv, svcctl.Result{}, fmt.Errorf("svcctl: %s: %w", depsArgv[0], context.DeadlineExceeded))
		}})
	for _, action := range spec.Actions {
		err := w.p.admitControl(context.Background(), "nx-svc-test", action)
		pe := wantProblem(t, err, problem.DependencyUnavailable)
		if pe.Status != 503 || !strings.Contains(pe.Detail, "deadline exceeded") {
			t.Errorf("%s: %d %q, want 503 naming the lookup's error", action, pe.Status, pe.Detail)
		}
	}
	// The next preflight resolves them.
	w.fh.Answer(depsArgv, svcctl.Result{Output: []byte("Requires=nexusd-db.service\nBindsTo=\nRequisite=\nPartOf=\n")}, nil)
	wantAvailable(t, w.p.Preflight(context.Background()))
	if err := w.p.admitControl(context.Background(), "nx-svc-test", spec.Stop.Op().Action); err != nil {
		t.Errorf("after a preflight that resolved the dependencies: %v, want admitted", err)
	}
	if err := w.p.admitControl(context.Background(), "nexusd-db", spec.Stop.Op().Action); err == nil {
		t.Error("nexusd-db, a dependency, was admitted")
	}
}

// CR-11: `actions:` with no value (null) grants none, as `actions: []` does;
// only an absent key grants all three.
func TestCR11_ANullActionsKeyGrantsNone(t *testing.T) {
	for _, c := range []struct {
		name, actions string
		want          bool // start is allowed
	}{
		{"absent", "", true},
		{"null", "    actions:\n", false},
		{"tilde", "    actions: ~\n", false},
		{"empty list", "    actions: []\n", false},
		{"start only", "    actions: [start]\n", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n"+c.actions,
				ctlOptions{names: []string{"nx-svc-test"}})
			err := w.p.admitControl(context.Background(), "nx-svc-test", spec.Start.Op().Action)
			if c.want && err != nil {
				t.Errorf("start: %v, want admitted", err)
			}
			if !c.want {
				wantProblem(t, err, problem.NotAllowed)
			}
			if got := allowEntry(t, w.p.Preflight(context.Background()), "nx-svc-test").Actions; c.want == (len(got) == 0) {
				t.Errorf("capabilities actions %v, want start allowed = %v", got, c.want)
			}
		})
	}
}

// CR-12: the helper's input.json and result.json are read bounded and only
// when regular: a FIFO never blocks nexusd, and an oversized file is an
// error, not a read of all of it.
func TestCR12_TheHelperFilesAreReadBoundedAndRegular(t *testing.T) {
	read := func(t *testing.T, dir string) (ok bool, err error) {
		t.Helper()
		type answer struct {
			ok  bool
			err error
		}
		c := make(chan answer, 1)
		go func() {
			_, ok, err := readHelperFile(dir, helperResultFile)
			c <- answer{ok, err}
		}()
		select {
		case a := <-c:
			return a.ok, a.err
		case <-time.After(3 * time.Second):
			// The reader is stuck in open(2) on the FIFO; unblock it so the
			// goroutine ends.
			if f, err := os.OpenFile(filepath.Join(dir, helperResultFile), os.O_WRONLY, 0); err == nil {
				f.Close()
			}
			t.Fatal("readHelperFile blocked")
			return false, nil
		}
	}
	t.Run("a FIFO", func(t *testing.T) {
		dir := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(dir, helperResultFile), 0o600); err != nil {
			t.Fatal(err)
		}
		if ok, err := read(t, dir); ok || err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("read = %v, %v; want an error: not a regular file", ok, err)
		}
	})
	t.Run("over the bound", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, helperResultFile), strings.Repeat("x", maxHelperFileBytes+1))
		if ok, err := read(t, dir); ok || err == nil || !strings.Contains(err.Error(), "is over") {
			t.Errorf("read = %v, %v; want an error: over the bound", ok, err)
		}
	})
	t.Run("a symlink", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "real"), "{}")
		if err := os.Symlink("real", filepath.Join(dir, helperResultFile)); err != nil {
			t.Fatal(err)
		}
		if ok, err := read(t, dir); ok || err == nil {
			t.Errorf("read = %v, %v; want an error", ok, err)
		}
	})
	t.Run("absent and regular", func(t *testing.T) {
		dir := t.TempDir()
		if ok, err := read(t, dir); ok || err != nil {
			t.Errorf("absent: %v, %v; want not found, no error", ok, err)
		}
		writeFile(t, filepath.Join(dir, helperResultFile), "{}")
		if ok, err := read(t, dir); !ok || err != nil {
			t.Errorf("regular: %v, %v; want found", ok, err)
		}
	})
}

// CR-16: CR-2's rule on OpenRC. When the deptree cannot be read, nexusd's
// needs are the names in its init script, and a virtual one (net) is not
// resolved to its provider (networking), whose stop would stop nexusd.
// Control fails closed, 503 dependency-unavailable, until a preflight reads
// the deptree.
func TestCR16_OpenRCControlFailsClosedWhileTheDeptreeIsUnusable(t *testing.T) {
	w := newCtlWorld(t, ctlOpenRC, "enabled: true\ncontrol:\n  - name: nx-svc-test\n  - name: crond\n", ctlOptions{})
	deptree := filepath.Join(w.root, "run", "openrc", "deptree")
	saved, err := os.ReadFile(deptree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deptree); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(w.root, "etc", "init.d", "nexusd"),
		"#!/sbin/openrc-run\ndepend() {\n\tneed cron\n}\n")
	wantAvailable(t, w.p.Preflight(context.Background()))
	for _, name := range []string{"crond", "nx-svc-test"} {
		for _, action := range spec.Actions {
			err := w.p.admitControl(context.Background(), name, action)
			pe := wantProblem(t, err, problem.DependencyUnavailable)
			if pe.Status != 503 || !strings.Contains(pe.Detail, "deptree") {
				t.Errorf("%s %s: %d %q, want 503 naming the deptree", action, name, pe.Status, pe.Detail)
			}
		}
	}
	// The next preflight reads the deptree.
	if err := os.WriteFile(deptree, saved, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(w.root, "etc", "init.d", "nexusd")); err != nil {
		t.Fatal(err)
	}
	wantAvailable(t, w.p.Preflight(context.Background()))
	if err := w.p.admitControl(context.Background(), "nx-svc-test", spec.Stop.Op().Action); err != nil {
		t.Errorf("after a preflight that read the deptree: %v, want admitted", err)
	}
}
