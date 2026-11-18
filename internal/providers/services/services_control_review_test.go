// Regression tests of planning/services-provider/code-review.md CR-1 and
// CR-4: a restart that fails while the old instance keeps running, a
// masked unit, and the F-HOSTS fake replaying the captured failures.

package services

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

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
