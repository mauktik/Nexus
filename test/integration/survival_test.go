// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
)

// FR-6's bound: an operation whose supervisor is lost is terminal within
// this long.
const lostBound = 60 * time.Second

// The setsid control (TC-77) runs once per run. Until it has shown that
// the environment kills a process left in a killed unit's cgroup, no
// survival result means anything: a harness where nothing dies would pass
// every survival test.
var (
	controlOnce sync.Once
	controlErr  error // The setsid child survived: nothing discriminates
	controlOp   error // The op unit beside it did not survive (A-20)
)

// requireDiscriminating fails t, reporting the survival suite invalid,
// unless the setsid control passed.
func requireDiscriminating(t *testing.T) {
	t.Helper()
	controlOnce.Do(func() { controlErr, controlOp = setsidControl(t) })
	if controlErr != nil {
		t.Fatalf("survival suite INVALID: the environment does not discriminate: %v", controlErr)
	}
}

// setsidControl starts opsd with a setsid child in its own unit's cgroup
// and a gate op in a transient unit, kills the daemon's main process with
// SIGKILL. env is an error unless the setsid child dies; op is an error
// unless the op unit and its child live on (A-20). Only env invalidates the
// survival suite: op is a product result, which TC-77 asserts.
func setsidControl(t *testing.T) (env, op error) {
	t.Helper()
	o := newOpsd(t, "tc77", opsdConfig{flags: []string{"--setsid-child", filepath.Join(nexusdState, "opsd-tc77", "setsid.pid")}})
	o.start()
	setsid := waitPIDFile(t, filepath.Join(o.stateDir, "setsid.pid"))
	daemonCg := pidCgroup(t, o.mainPID())
	if cg := pidCgroup(t, setsid); cg != daemonCg || !strings.Contains(cg, nexusdUnit) {
		return fmt.Errorf("setsid child %d is in cgroup %q, not the daemon's %q", setsid, cg, daemonCg), nil
	}
	if sid, err := sessionID(setsid); err != nil || sid != setsid {
		return fmt.Errorf("child %d is not a session leader (sid %d, %v)", setsid, sid, err), nil
	}
	started := o.startJob("tc77", job{ExitCode: 42})
	child := waitPIDFile(t, filepath.Join(markerDir("tc77"), "child.pid"))
	unit := detach.Unit(started.ID)
	t.Logf("before kill -9: setsid child %d in %s; op child %d in %s", setsid, pidCgroup(t, setsid), child, pidCgroup(t, child))

	o.kill9()
	deadline := time.Now().Add(15 * time.Second)
	for pidAlive(setsid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	setsidDead := !pidAlive(setsid)
	opActive := activeState(t, unit)
	childAlive := pidAlive(child)
	t.Logf("after kill -9: setsid child alive=%v; %s %s; op child alive=%v", !setsidDead, unit, opActive, childAlive)
	release(t, "tc77")
	if opActive == "active" {
		waitUntil(t, 30*time.Second, "exit.json of "+started.ID, func() (bool, string) {
			return exists(filepath.Join(o.opDir(started.ID), detach.ExitFile)), ""
		})
	}
	if !setsidDead {
		env = fmt.Errorf("setsid child %d in the killed unit's cgroup survived", setsid)
	}
	if opActive != "active" || !childAlive {
		op = fmt.Errorf("the op unit did not survive the same kill (unit %s, child alive %v)", opActive, childAlive)
	}
	return env, op
}

// TC-77: kill -9 of the daemon's main process kills a setsid child left
// in the daemon's own cgroup, so this environment can tell surviving from
// not surviving; the transient op unit beside it survives (A-20).
func TestSetsidChildInDaemonCgroupDies(t *testing.T) {
	requireInit(t, platform.Systemd)
	requireDiscriminating(t)
	if controlOp != nil {
		t.Fatal(controlOp)
	}
}

// TC-72: kill -9 of the daemon leaves the op unit active and its child
// alive; restarted before or after the child ends, the daemon reports the
// child's real exit, 42, with no output lost.
func TestKill9LeavesOpRunningAndReportsTruthfully(t *testing.T) {
	requireInit(t, platform.Systemd)
	requireDiscriminating(t)
	for _, when := range []string{"before", "after"} {
		t.Run("restart-"+when+"-child-ends", func(t *testing.T) {
			name := "tc72-" + when
			o := newOpsd(t, name, opsdConfig{})
			o.start()
			op := o.startJob(name, job{ExitCode: 42})
			unit := detach.Unit(op.ID)
			child := waitPIDFile(t, filepath.Join(markerDir(name), "child.pid"))
			o.waitStatus(op.ID, apitypes.StatusRunning)

			o.kill9()
			// Survival is observed twice, the second time after systemd has
			// finished cleaning up the daemon's cgroup.
			for range 2 {
				if st := activeState(t, unit); st != "active" {
					t.Fatalf("%s is %s after kill -9 of the daemon", unit, st)
				}
				if !pidAlive(child) {
					t.Fatalf("op child %d died with the daemon", child)
				}
				time.Sleep(2 * time.Second)
			}
			t.Logf("after kill -9: %s active, child %d alive", unit, child)

			exitFile := filepath.Join(o.opDir(op.ID), detach.ExitFile)
			if when == "before" {
				o.start()
				release(t, name)
			} else {
				release(t, name)
				waitUntil(t, 30*time.Second, unit+" to end with the daemon down", func() (bool, string) {
					u := showUnit(t, unit, "LoadState", "ActiveState")
					return exists(exitFile) && u["ActiveState"] != "active", fmt.Sprint(u)
				})
				if activeState(t, nexusdUnit) == "active" {
					t.Fatal("the daemon came back before the child ended")
				}
				x, _, err := detach.ReadExit(o.opDir(op.ID))
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("child ended while the daemon was down: exit.json %+v", x)
				o.start()
			}
			end, took := o.waitTerminal(op.ID, lostBound)
			t.Logf("%s after restart: %s %s (%v)", op.ID, end.Status, end.Error, took)
			if end.Status != apitypes.StatusFailed || problemSlug(end) != string(problem.ScriptFailed) ||
				!strings.Contains(end.Error.Detail, "code 42") {
				t.Fatalf("op ended %s %v, want Failed script-failed with exit code 42", end.Status, end.Error)
			}
			out := o.logs(op.ID)
			for _, line := range []string{"gate: before\n", "gate: stderr before\n", "gate: after\n"} {
				if !strings.Contains(out, line) {
					t.Fatalf("output lost %q across the restart; output %q", line, out)
				}
			}
		})
	}
}

// TC-73: the supervisor's unit is killed while the daemon is down; after
// the restart the operation is Failed outcome-unknown within 60 seconds,
// and nothing stays Running.
func TestLostSupervisorWithDaemonDownIsOutcomeUnknown(t *testing.T) {
	requireInit(t, platform.Systemd)
	requireDiscriminating(t)
	o := newOpsd(t, "tc73", opsdConfig{})
	o.start()
	op := o.startJob("tc73", job{})
	unit := detach.Unit(op.ID)
	child := waitPIDFile(t, filepath.Join(markerDir("tc73"), "child.pid"))
	o.waitStatus(op.ID, apitypes.StatusRunning)

	o.kill9()
	// RefuseManualStop does not block kill; every process of the unit dies.
	mustSh(t, detach.Systemctl, "kill", "-s", "KILL", "--", unit)
	waitUntil(t, 15*time.Second, unit+" gone", func() (bool, string) {
		st := activeState(t, unit)
		return st != "active" && st != "deactivating" && !pidAlive(child), st
	})
	if exists(filepath.Join(o.opDir(op.ID), detach.ExitFile)) {
		t.Fatal("exit.json exists: the supervisor was not lost")
	}
	o.start()
	end, took := o.waitTerminal(op.ID, lostBound)
	t.Logf("%s after restart: %s %v in %v", op.ID, end.Status, end.Error, took)
	if end.Status != apitypes.StatusFailed || problemSlug(end) != string(problem.OutcomeUnknown) {
		t.Fatalf("op ended %s %v, want Failed outcome-unknown", end.Status, end.Error)
	}
	if took >= lostBound {
		t.Fatalf("terminal %v after the restart, bound %v", took, lostBound)
	}
	code, b := o.do(http.MethodGet, "/v1/operations?filter=status=Running", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, b)
	}
	var running apitypes.List[apitypes.Operation]
	if err := json.Unmarshal(b, &running); err != nil {
		t.Fatal(err)
	}
	if len(running.Items) != 0 {
		t.Fatalf("operations still Running: %+v", running.Items)
	}
}

// TC-214: with the daemon up throughout, the supervisor's unit is killed;
// the operation is Failed outcome-unknown within 60 seconds of the kill,
// the daemon's PID is unchanged, and the reject lane admits new work.
func TestSupervisorDeathWithDaemonUp(t *testing.T) {
	requireInit(t, platform.Systemd)
	requireDiscriminating(t)
	for _, mode := range []detach.KillMode{detach.KillControlGroup, detach.KillProcess} {
		t.Run(string(mode), func(t *testing.T) {
			name := "tc214-" + string(mode)
			lane := "tc214:" + string(mode)
			o := newOpsd(t, name, opsdConfig{})
			o.start()
			j := job{KillMode: string(mode), Lane: lane, Policy: "reject"}
			op := o.startJob(name, j)
			unit := detach.Unit(op.ID)
			waitPIDFile(t, filepath.Join(markerDir(name), "child.pid"))
			o.waitStatus(op.ID, apitypes.StatusRunning)
			if got := showUnit(t, unit, "KillMode")["KillMode"]; got != string(mode) {
				t.Fatalf("%s KillMode=%s", unit, got)
			}
			if code, b := o.post(name+"-busy", j); code != http.StatusConflict {
				t.Fatalf("second op on the busy reject lane: %d %s, want 409", code, b)
			}
			pid := o.mainPID()

			mustSh(t, detach.Systemctl, "kill", "-s", "KILL", "--", unit)
			killed := time.Now()
			end, _ := o.waitTerminal(op.ID, lostBound)
			took := time.Since(killed)
			t.Logf("%s: %s %v, %v after the kill", op.ID, end.Status, end.Error, took)
			if end.Status != apitypes.StatusFailed || problemSlug(end) != string(problem.OutcomeUnknown) {
				t.Fatalf("op ended %s %v, want Failed outcome-unknown", end.Status, end.Error)
			}
			if took >= lostBound {
				t.Fatalf("terminal %v after the kill, bound %v", took, lostBound)
			}
			if exists(filepath.Join(o.opDir(op.ID), detach.ExitFile)) {
				t.Fatal("exit.json exists: the supervisor was not lost")
			}
			if now := o.mainPID(); now != pid || activeState(t, nexusdUnit) != "active" {
				t.Fatalf("daemon PID %d -> %d: the daemon did not stay up", pid, now)
			}
			next := o.startJob(name+"-next", j)
			t.Logf("the lane admitted %s with 202", next.ID)
			waitPIDFile(t, filepath.Join(markerDir(name+"-next"), "child.pid"))
			release(t, name+"-next")
			if end, _ := o.waitTerminal(next.ID, lostBound); end.Status != apitypes.StatusSucceeded {
				t.Fatalf("next op ended %s %v", end.Status, end.Error)
			}
		})
	}
}

// TC-76: a cancel through the API reaches the child's whole process group
// by way of systemctl kill --kill-whom=main and the supervisor, and only
// that group; systemctl stop of an op unit is refused.
func TestCancelReachesProcessGroup(t *testing.T) {
	requireInit(t, platform.Systemd)
	requireDiscriminating(t)
	o := newOpsd(t, "tc76", opsdConfig{})
	o.start()

	// KillMode=process, so the unit's own cleanup sends nothing to the
	// bystander: a SIGTERM it records could only come from the cancel.
	op := o.startJob("tc76", job{Behavior: "trap", KillMode: string(detach.KillProcess)})
	unit := detach.Unit(op.ID)
	dir := markerDir("tc76")
	child := waitPIDFile(t, filepath.Join(dir, "child.pid"))
	grandchild := waitPIDFile(t, filepath.Join(dir, "grandchild.pid"))
	bystander := waitPIDFile(t, filepath.Join(dir, "bystander.pid"))
	o.waitStatus(op.ID, apitypes.StatusRunning)
	sup, err := strconv.Atoi(showUnit(t, unit, "MainPID")["MainPID"])
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{child, grandchild, bystander} {
		if cg := pidCgroup(t, pid); !strings.Contains(cg, unit) {
			t.Fatalf("pid %d is in %s, not %s", pid, cg, unit)
		}
	}
	pgid := func(pid int) int {
		g, err := syscall.Getpgid(pid)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if pgid(grandchild) != pgid(child) || pgid(bystander) == pgid(child) || pgid(sup) == pgid(child) {
		t.Fatalf("process groups: supervisor %d, child %d, grandchild %d, bystander %d",
			pgid(sup), pgid(child), pgid(grandchild), pgid(bystander))
	}
	t.Logf("%s: supervisor %d, child %d and grandchild %d in group %d, bystander %d in group %d",
		unit, sup, child, grandchild, pgid(child), bystander, pgid(bystander))

	code, b := o.do(http.MethodPost, "/v1/operations/"+op.ID+"/cancel", "")
	if code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, b)
	}
	end, took := o.waitTerminal(op.ID, 30*time.Second)
	t.Logf("%s canceled through the API: %s in %v", op.ID, end.Status, took)
	if end.Status != apitypes.StatusCanceled {
		t.Fatalf("op ended %s %v, want Canceled", end.Status, end.Error)
	}
	for _, who := range []string{"child", "grandchild"} {
		if b, err := os.ReadFile(filepath.Join(dir, who+".term")); err != nil || strings.TrimSpace(string(b)) != "TERM" {
			t.Fatalf("%s recorded no SIGTERM: %q %v", who, b, err)
		}
	}
	if exists(filepath.Join(dir, "bystander.term")) {
		t.Fatal("a process outside the child's group got SIGTERM: the cancel was not --kill-whom=main")
	}
	if !pidAlive(bystander) {
		t.Fatal("the bystander died: the cancel reached past the child's process group")
	}
	if err := syscall.Kill(bystander, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	t.Log("child and grandchild recorded SIGTERM; the bystander in its own session did not")

	// Step 2: systemctl stop is refused and the unit stays active.
	second := o.startJob("tc76-stop", job{})
	sunit := detach.Unit(second.ID)
	schild := waitPIDFile(t, filepath.Join(markerDir("tc76-stop"), "child.pid"))
	o.waitStatus(second.ID, apitypes.StatusRunning)
	out, err := sh(30*time.Second, detach.Systemctl, "stop", "--", sunit)
	t.Logf("systemctl stop %s: %v: %s", sunit, err, strings.TrimSpace(out))
	if err == nil || !strings.Contains(strings.ToLower(out), "refuse") {
		t.Fatalf("systemctl stop %s was not refused: %v %s", sunit, err, out)
	}
	time.Sleep(time.Second)
	if st := activeState(t, sunit); st != "active" || !pidAlive(schild) {
		t.Fatalf("%s is %s after the refused stop (child alive %v)", sunit, st, pidAlive(schild))
	}
	release(t, "tc76-stop")
	if end, _ := o.waitTerminal(second.ID, 30*time.Second); end.Status != apitypes.StatusSucceeded {
		t.Fatalf("second op ended %s %v", end.Status, end.Error)
	}
}

// TC-74: real needrestart -r a with two outdated units of the same shape,
// the op unit as the production launcher makes it and a control without
// RefuseManualStop. The control must restart, or the run proves nothing;
// the op unit must not.
func TestNeedrestartSkipsOpUnitsWithControl(t *testing.T) {
	requireInit(t, platform.Systemd)
	requireDiscriminating(t)
	// The units run a copy of nexusd, replaced on disk below, so their
	// supervisors map a deleted file (the spike's method). The copy keeps
	// the real nexusd and the daemon under test out of it.
	exeDir := t.TempDir()
	exe := filepath.Join(exeDir, "nexusd")
	copyFile(t, nexusdBin, exe)
	opsDir := t.TempDir()
	l, err := detach.New(detach.Config{OpsDir: opsDir, Executable: exe}, detach.Options{})
	if err != nil {
		t.Fatal(err)
	}
	spec := detach.Spec{Argv: []string{"/usr/bin/sleep", "600"}, Env: []string{}, Grace: 2 * time.Second,
		KillMode: detach.KillControlGroup, MaxRuntime: time.Hour}
	ctx := context.Background()

	const opID, ctlID = "tc74-op", "tc74-control"
	if err := l.Start(ctx, opID, spec); err != nil {
		t.Fatal(err)
	}
	ctlDir := l.Dir(ctlID)
	if err := os.MkdirAll(ctlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := detach.WriteSpec(ctlDir, spec); err != nil {
		t.Fatal(err)
	}
	argv := slices.DeleteFunc(detach.Argv(ctlID, exe, opsDir, spec), func(a string) bool {
		return a == "--property=RefuseManualStop=yes"
	})
	mustSh(t, argv[0], argv[1:]...)
	opUnit, ctlUnit := detach.Unit(opID), detach.Unit(ctlID)
	t.Cleanup(func() {
		for _, u := range []string{opUnit, ctlUnit} {
			if out, err := sh(30*time.Second, detach.Systemctl, "kill", "-s", "KILL", "--", u); err != nil {
				t.Logf("cleanup %s: %v %s", u, err, out)
			}
		}
	})
	waitUntil(t, 30*time.Second, "both supervisors to take their spec", func() (bool, string) {
		return !exists(filepath.Join(l.Dir(opID), detach.SpecFile)) && !exists(filepath.Join(ctlDir, detach.SpecFile)) &&
			activeState(t, opUnit) == "active" && activeState(t, ctlUnit) == "active", ""
	})
	// A restarted control re-runs op-supervise; a fresh spec gives it a
	// child, so the restart shows as a new live main process (A-21's
	// danger, made visible).
	if err := detach.WriteSpec(ctlDir, spec); err != nil {
		t.Fatal(err)
	}
	props := []string{"MainPID", "InvocationID", "ActiveState", "RefuseManualStop"}
	opBefore, ctlBefore := showUnit(t, opUnit, props...), showUnit(t, ctlUnit, props...)
	t.Logf("before: %s %v; %s %v", opUnit, opBefore, ctlUnit, ctlBefore)
	if ctlBefore["RefuseManualStop"] != "no" {
		t.Fatalf("run INVALID: the control has RefuseManualStop=%s", ctlBefore["RefuseManualStop"])
	}

	// Replace the binary: a new inode under the same name.
	copyFile(t, nexusdBin, exe+".new")
	if err := os.Rename(exe+".new", exe); err != nil {
		t.Fatal(err)
	}
	listed := mustSh(t, "needrestart", "-b")
	t.Logf("needrestart -b:\n%s", listed)
	for _, u := range []string{opUnit, ctlUnit} {
		if !strings.Contains(listed, "NEEDRESTART-SVC: "+u) {
			t.Fatalf("run INVALID: needrestart does not list %s as outdated, so the method did not apply", u)
		}
	}
	out, err := sh(2*time.Minute, "needrestart", "-r", "a")
	t.Logf("needrestart -r a: %v\n%s", err, out)
	if err != nil {
		t.Fatal(err)
	}

	var ctlAfter map[string]string
	waitUntil(t, 30*time.Second, "the control to come back", func() (bool, string) {
		ctlAfter = showUnit(t, ctlUnit, props...)
		return ctlAfter["ActiveState"] == "active" && ctlAfter["InvocationID"] != ctlBefore["InvocationID"], fmt.Sprint(ctlAfter)
	})
	opAfter := showUnit(t, opUnit, props...)
	t.Logf("after: %s %v; %s %v", opUnit, opAfter, ctlUnit, ctlAfter)
	if ctlAfter["MainPID"] == ctlBefore["MainPID"] || ctlAfter["MainPID"] == "0" {
		t.Fatalf("run INVALID: the control was not restarted (MainPID %s -> %s)", ctlBefore["MainPID"], ctlAfter["MainPID"])
	}
	if opAfter["MainPID"] != opBefore["MainPID"] || opAfter["InvocationID"] != opBefore["InvocationID"] ||
		opAfter["ActiveState"] != "active" {
		t.Fatalf("the op unit was restarted: before %v after %v", opBefore, opAfter)
	}
	if opBefore["RefuseManualStop"] != "yes" {
		t.Fatalf("the op unit has RefuseManualStop=%s", opBefore["RefuseManualStop"])
	}
	pid, err := strconv.Atoi(opBefore["MainPID"])
	if err != nil || !pidAlive(pid) {
		t.Fatalf("op supervisor %s is gone (%v)", opBefore["MainPID"], err)
	}
	if !strings.Contains(out, opUnit) {
		t.Fatalf("needrestart output does not mention %s", opUnit)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	if out, err := exec.Command("cp", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("cp %s %s: %v %s", src, dst, err, out)
	}
}

// sessionID is field 6 of /proc/<pid>/stat.
func sessionID(pid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 4 {
		return 0, fmt.Errorf("short stat %q", s)
	}
	return strconv.Atoi(f[3])
}
