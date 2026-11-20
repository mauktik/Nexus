// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
	scriptspec "github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// FR-6 acceptance against real apt and dpkg (WI 7.5): the real nexusd under
// the shipped unit, a real apt-get upgrade in its nexus-op-<id> unit, held
// mid-postinst by the fixture's --gate (the slow-postinst fixture), and the
// daemon, its supervisor or needrestart acting on it while dpkg runs.
// Every expected value comes from dpkg-query, dpkg --audit, dpkg.log,
// apt-cache policy, /proc and systemd read in the test, never from Nexus.

// dpkgLogFrom is dpkg.log from byte offset off on.
func dpkgLogFrom(t *testing.T, off int64) string {
	t.Helper()
	b, err := os.ReadFile(apt.DpkgLog)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) < off {
		t.Fatalf("dpkg.log shrank below %d bytes", off)
	}
	return string(b[off:])
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// unitDpkg is the dpkg in operation id's unit and whether one of pkgs'
// postinst scripts is running there: 0 and "" when not.
func unitDpkg(id string, pkgs ...string) (dpkg int, postinst string) {
	for _, pid := range unitPIDs(id) {
		if procComm(pid) == "dpkg" {
			dpkg = pid
		}
		argv := strings.Join(procArgv(pid), " ")
		for _, p := range pkgs {
			if strings.Contains(argv, "/var/lib/dpkg/info/"+p+".postinst") {
				postinst = p
			}
		}
	}
	return dpkg, postinst
}

// unitPS is ps's view of the processes in operation id's unit, for the
// evidence log.
func unitPS(t *testing.T, id string) string {
	t.Helper()
	pids := unitPIDs(id)
	if len(pids) == 0 {
		return "(no process in the unit's cgroup)"
	}
	var list []string
	for _, p := range pids {
		list = append(list, strconv.Itoa(p))
	}
	out, _ := sh(30*time.Second, "ps", "-o", "pid,ppid,stat,args", "-p", strings.Join(list, ","))
	return strings.TrimRight(out, "\n")
}

// waitDpkgInPostinst waits until dpkg runs in the operation's unit with one
// of pkgs' gated postinst scripts, dpkg.log after off records the
// transaction's start and pkg's upgrade, and dpkg --audit reports the
// package mid-transaction (the control that a clean audit later means dpkg
// finished). It returns dpkg's PID and the package held.
func waitDpkgInPostinst(t *testing.T, n *nexusdInstance, id string, off int64, pkgs ...string) (int, string) {
	t.Helper()
	var dpkg int
	var held string
	waitUntil(t, 2*time.Minute, "dpkg in the upgrade's unit running a gated postinst", func() (bool, string) {
		dpkg, held = unitDpkg(id, pkgs...)
		return dpkg != 0 && held != "", fmt.Sprintf("dpkg %d postinst %q op %s", dpkg, held, n.operation(id).Status)
	})
	log := dpkgLogFrom(t, off)
	if !strings.Contains(log, " startup archives unpack") ||
		!strings.Contains(log, fmt.Sprintf(" upgrade %s:all 1.0 2.0", held)) {
		t.Fatalf("dpkg.log does not show the transaction started:\n%s", log)
	}
	if audit := mustSh(t, apt.Dpkg, "--audit"); !strings.Contains(audit, held) {
		t.Fatalf("control: dpkg --audit does not report %s mid-postinst:\n%s", held, audit)
	}
	if st := n.operation(id).Status; st != apitypes.StatusRunning {
		t.Fatalf("operation %s while dpkg runs", st)
	}
	return dpkg, held
}

// aptVersionAndStatus is pkg's dpkg-query version and status word, or empty
// and "forgotten" when dpkg no longer knows pkg. The callers expect pkg to be
// installed, so a forgotten package is their own diagnostic, not a fatal read.
func aptVersionAndStatus(t *testing.T, pkg string) (version, status string) {
	t.Helper()
	if status = aptStatusOrForgotten(t, pkg); status == "forgotten" {
		return "", status
	}
	return aptVersion(t, pkg), status
}

// TC-139 (FR-6, NFR-7): kill -9 of nexusd while dpkg runs a real apt-get
// upgrade's gated postinst. dpkg goes on with the same PID, nexusd is
// restarted while dpkg still runs and reports Running, and once the gate
// opens the operation reaches its real result: Succeeded, apt-get's exit
// status, every package at the version apt-cache policy named as the
// candidate before the run, dpkg --audit empty.
func TestAptUpgradeSurvivesNexusdKill9DuringDpkg(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	const slow = "nxslow6"
	aptPurgeAround(t, slow)
	gate, open := aptGate(t, slow)
	mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", filepath.Join(t.TempDir(), "debs"), slow, "1.0"))
	aptBuild(t, "add", aptRepo, slow, "2.0", "--gate", gate)
	mustSh(t, apt.AptGet, "update", "-qq")
	want := aptPolicy(t, aptPlain, slow)
	for _, p := range []string{aptPlain, slow} {
		if want[p] != [2]string{"1.0", "2.0"} {
			t.Fatalf("apt-cache policy %s: %q, want installed 1.0, candidate 2.0", p, want[p])
		}
	}
	const name = "apt-fr6"
	n := aptd(t, name)
	off := fileSize(t, apt.DpkgLog)

	op := startUpgrade(t, n, `{}`)
	dpkg, held := waitDpkgInPostinst(t, n, op.ID, off, slow)
	unit := detach.Unit(op.ID)
	aptGet := opAptGet(t, op.ID)
	daemon := n.mainPID()
	t.Logf("before kill -9: nexusd %d; %s active, apt-get %d, dpkg %d in %s's postinst; dpkg --audit:\n%s\n%s",
		daemon, unit, aptGet, dpkg, held, strings.TrimSpace(mustSh(t, apt.Dpkg, "--audit")), unitPS(t, op.ID))

	kill9Nexusd(t, n)
	killed := time.Now()
	for range 2 {
		if st := activeState(t, unit); st != "active" {
			t.Fatalf("%s is %s after kill -9 of nexusd", unit, st)
		}
		if !pidAlive(dpkg) || !pidAlive(aptGet) {
			t.Fatalf("dpkg %d alive %v, apt-get %d alive %v after kill -9 of nexusd", dpkg, pidAlive(dpkg), aptGet, pidAlive(aptGet))
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("kill -9 nexusd %d; %s after: %s active, dpkg %d and apt-get %d alive:\n%s", daemon,
		time.Since(killed).Round(time.Millisecond), unit, dpkg, aptGet, unitPS(t, op.ID))

	restarted := restartNexusd(t)
	if got := n.operation(op.ID); got.Status != apitypes.StatusRunning || !pidAlive(dpkg) {
		t.Fatalf("after the restart, with dpkg %d alive %v: %s %+v", dpkg, pidAlive(dpkg), got.Status, got.Error)
	}
	t.Logf("nexusd restarted as %d; %s Running while dpkg %d still holds %s's postinst", n.mainPID(), op.ID, dpkg, held)

	open()
	var x detach.Exit
	waitUntil(t, 2*time.Minute, "exit.json", func() (bool, string) {
		var ok bool
		x, ok, _ = detach.ReadExit(aptOpDir(name, op.ID))
		return ok, ""
	})
	got := n.waitOp(op.ID, lostBound)
	took := time.Since(x.Ended)
	t.Logf("%s %s %s after apt-get ended (%s after the restart): %s", op.ID, got.Status, took.Round(time.Millisecond),
		time.Since(restarted).Round(time.Millisecond), got.Result)
	if got.Status != apitypes.StatusSucceeded {
		t.Fatalf("after the restart: %s %+v", got.Status, got.Error)
	}
	var res spec.UpgradeResult
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatal(err)
	}
	if x.ExitCode != 0 || res.ExitCode != x.ExitCode || !res.Changed {
		t.Errorf("exit.json %+v, result exitCode %d changed %v", x, res.ExitCode, res.Changed)
	}
	if pidAlive(dpkg) || pidAlive(aptGet) {
		t.Errorf("dpkg %d or apt-get %d still runs", dpkg, aptGet)
	}
	audit := mustSh(t, apt.Dpkg, "--audit")
	if strings.TrimSpace(audit) != "" {
		t.Errorf("dpkg --audit is not empty:\n%s", audit)
	}
	var changes []string
	for _, c := range res.Changes {
		changes = append(changes, c.Package+" "+c.OldVersion+">"+c.NewVersion)
	}
	for _, p := range []string{aptPlain, slow} {
		if v, st := aptVersionAndStatus(t, p); v != want[p][1] || st != "installed" {
			t.Errorf("dpkg-query %s %s %s, want %s installed", p, v, st, want[p][1])
		}
		if !slices.Contains(changes, p+" 1.0>"+want[p][1]) {
			t.Errorf("result changes %q lack %s 1.0>%s", changes, p, want[p][1])
		}
	}
	log := dpkgLogFrom(t, off)
	if !strings.Contains(log, " status installed "+slow+":all 2.0") {
		t.Errorf("dpkg.log lacks %s installed at 2.0", slow)
	}
	t.Logf("dpkg --audit after: %q; dpkg-query: %s %s, %s %s; dpkg.log of the run:\n%s", strings.TrimSpace(audit),
		aptPlain, aptVersion(t, aptPlain), slow, aptVersion(t, slow), strings.TrimSpace(log))
}

// TC-ERR56 (FR-6, FR-17, A-52): with nexusd up throughout, the upgrade's
// op-supervise process alone is killed with SIGKILL while dpkg runs a gated
// postinst of a KillMode=process unit. Two packages share the gate, so
// dpkg has to write to the broken pipe after the kill. The operation ends
// Failed outcome-unknown within 60 seconds, nexusd keeps its PID, a later
// mutation on the apt lane is accepted and runs once dpkg is done, and
// dpkg --audit is empty with every package installed. Any other result is
// an A-52 falsification, reported as such and not loosened.
func TestAptSupervisorDeathMidDpkg(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	pkgs := []string{"nxsupa", "nxsupb"}
	aptPurgeAround(t, pkgs...)
	gate, open := aptGate(t, "nxsup")
	debs := filepath.Join(t.TempDir(), "debs")
	for _, p := range pkgs {
		mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", debs, p, "1.0"))
		aptBuild(t, "add", aptRepo, p, "2.0", "--gate", gate)
	}
	mustSh(t, apt.AptGet, "update", "-qq")
	n := aptdWith(t, "apt-sup-death", "    lockWait: 180s\n")
	off := fileSize(t, apt.DpkgLog)

	op := startUpgrade(t, n, `{"names":["nxsupa","nxsupb"]}`)
	dpkg, held := waitDpkgInPostinst(t, n, op.ID, off, pkgs...)
	unit := detach.Unit(op.ID)
	if km := showUnit(t, unit, "KillMode")["KillMode"]; km != string(detach.KillProcess) {
		t.Fatalf("%s KillMode=%s, want process", unit, km)
	}
	sup, err := strconv.Atoi(showUnit(t, unit, "MainPID")["MainPID"])
	if err != nil || sup == 0 || !slices.Contains(procArgv(sup), "op-supervise") {
		t.Fatalf("%s MainPID %d (%v) is not op-supervise: %q", unit, sup, err, procArgv(sup))
	}
	aptGet := opAptGet(t, op.ID)
	daemon := n.mainPID()
	t.Logf("before the kill: nexusd %d; %s KillMode=process, op-supervise %d, apt-get %d, dpkg %d in %s's postinst:\n%s",
		daemon, unit, sup, aptGet, dpkg, held, unitPS(t, op.ID))

	if err := syscall.Kill(sup, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	time.Sleep(2 * time.Second)
	if !pidAlive(dpkg) {
		t.Fatalf("A-52 falsified: dpkg %d died with its supervisor before the gate opened", dpkg)
	}
	t.Logf("SIGKILL op-supervise %d; 2 s later %s is %s, dpkg %d alive, apt-get %d alive %v:\n%s", sup, unit,
		activeState(t, unit), dpkg, aptGet, pidAlive(aptGet), unitPS(t, op.ID))

	got := n.waitOp(op.ID, lostBound)
	took := time.Since(killed)
	t.Logf("%s: %s %v, %s after the kill", op.ID, got.Status, got.Error, took.Round(time.Millisecond))
	if got.Status != apitypes.StatusFailed || problemSlug(got) != string(problem.OutcomeUnknown) {
		t.Fatalf("op ended %s %v, want Failed outcome-unknown", got.Status, got.Error)
	}
	if took >= lostBound {
		t.Fatalf("terminal %s after the kill, bound %s", took, lostBound)
	}
	if now := n.mainPID(); now != daemon || activeState(t, nexusdUnit) != "active" {
		t.Fatalf("nexusd PID %d -> %d: it did not stay up", daemon, now)
	}

	// The same lane accepts the next mutation; it waits for the frontend
	// lock while the orphaned dpkg still holds it.
	next := startUpgrade(t, n, `{"names":["nxplain"]}`)
	time.Sleep(3 * time.Second)
	nextSt := n.operation(next.ID).Status
	t.Logf("next upgrade %s accepted with 202, %s while dpkg %d alive %v holds the frontend lock (holder %d)", next.ID,
		nextSt, dpkg, pidAlive(dpkg), frontendHolder(t))

	open()
	opened := time.Now()
	waitUntil(t, 2*time.Minute, "the orphaned apt-get and dpkg to finish", func() (bool, string) {
		return len(unitPIDs(op.ID)) == 0, unitPS(t, op.ID)
	})
	log := dpkgLogFrom(t, off)
	t.Logf("dpkg.log of the killed run (gate opened %s after the kill):\n%s", opened.Sub(killed).Round(time.Millisecond),
		strings.TrimSpace(log))
	audit := mustSh(t, apt.Dpkg, "--audit")
	for _, p := range pkgs {
		v, st := aptVersionAndStatus(t, p)
		t.Logf("dpkg-query %s %s %s", p, v, st)
		if st != "installed" {
			t.Errorf("A-52 falsified: %s is %s %s after dpkg finished", p, st, v)
		}
	}
	if strings.TrimSpace(audit) != "" {
		t.Errorf("A-52 falsified: dpkg --audit is not empty after dpkg finished:\n%s", audit)
	}
	// The second package was configured after the kill: dpkg wrote
	// "Setting up" to the dead supervisor's pipe and went on.
	other := pkgs[0]
	if other == held {
		other = pkgs[1]
	}
	if !strings.Contains(log, " status installed "+other+":all 2.0") {
		t.Errorf("dpkg.log lacks %s installed at 2.0 after the kill", other)
	}

	end := n.waitOp(next.ID, 3*time.Minute)
	if end.Status != apitypes.StatusSucceeded || aptVersion(t, aptPlain) != "2.0" {
		t.Fatalf("next upgrade on the lane: %s %+v, nxplain %s", end.Status, end.Error, aptVersion(t, aptPlain))
	}
	t.Logf("next upgrade %s Succeeded (%s): nxplain %s; dpkg --audit %q; nexusd %d throughout", next.ID, end.Result,
		aptVersion(t, aptPlain), strings.TrimSpace(mustSh(t, apt.Dpkg, "--audit")), n.mainPID())
}

// pollOp reads operation id, tolerating a daemon that is restarting: ok is
// false while the listener refuses or answers other than 200.
func pollOp(n *nexusdInstance, id string) (apitypes.Operation, bool) {
	var op apitypes.Operation
	req, err := http.NewRequest(http.MethodGet, "http://"+n.listen+"/v1/operations/"+id, nil)
	if err != nil {
		return op, false
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	resp, err := n.client.Do(req)
	if err != nil {
		return op, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&op) != nil {
		return op, false
	}
	return op, true
}

// waitOpAcrossRestart waits for id to be terminal through a daemon restart.
func waitOpAcrossRestart(t *testing.T, n *nexusdInstance, id string, timeout time.Duration) apitypes.Operation {
	t.Helper()
	var op apitypes.Operation
	waitUntil(t, timeout, id+" to end", func() (bool, string) {
		got, ok := pollOp(n, id)
		if !ok {
			return false, "daemon not answering"
		}
		op = got
		return terminal(op.Status), string(op.Status)
	})
	return op
}

const needrestartConf = "/etc/needrestart/conf.d/zz-nexus-t1.conf"

// TC-140 (FR-6, A-21, A-57): needrestart, run by apt's DPkg::Post-Invoke
// hook inside a real upgrade with $nrconf{restart} = 'a', restarts
// nexusd.service, whose binary was replaced on disk first because a static
// Go binary maps no shared library (A-57). The control: nexusd's MainPID
// changes. The upgrade still reaches its real terminal state, and a script
// operation's unit running at the same time, which needrestart lists as
// outdated too, keeps its MainPID and InvocationID and completes
// truthfully. If needrestart does not list nexusd.service, that is a G-15
// finding and the case fails.
func TestAptNeedrestartRestartsNexusdNotScriptUnit(t *testing.T) {
	requireInit(t, platform.Systemd)
	if !exists("/usr/sbin/needrestart") {
		t.Fatal("needrestart is not installed: the harness image (test/harness/Dockerfile) installs it on every base")
	}
	aptFixture(t, true)
	want := aptPolicy(t, aptPlain)[aptPlain]

	// Automatic restarts, and no state left over from earlier dpkg runs:
	// apt-pinvoke skips needrestart while /run/needrestart/errored exists.
	if err := os.WriteFile(needrestartConf, []byte("$nrconf{restart} = 'a';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(needrestartConf) })
	for _, f := range []string{"/run/needrestart/errored", "/run/needrestart/unpacked"} {
		if exists(f) {
			t.Logf("removing stale %s", f)
			_ = os.Remove(f)
		}
	}

	dir := testDir(t, "needrestart")
	release := filepath.Join(markers(t, "needrestart", "root"), "release")
	script := installScript(t, filepath.Join(dir, "gate.sh"), gateScript(release, 0))
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) })
	const name = "apt-needrestart"
	n := newNexusd(t, name, "  apt:\n    enabled: true\n  scripts:\n    enabled: true\n    catalog:\n"+
		catalogYAML(entry{id: "nrgate", path: script}))
	tok, _, err := auth.Create(filepath.Join(nexusdState, "nexusd-"+name), auth.CreateRequest{Name: "apt-scripts",
		Scopes: []string{spec.ReadScope, spec.WriteScope, scriptspec.RunScope, scriptspec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok

	code, b := n.post("/v1/providers/scripts/scripts/nrgate/invoke", "{}")
	var sop apitypes.Operation
	if code != http.StatusAccepted || json.Unmarshal(b, &sop) != nil {
		t.Fatalf("invoke: %d %s", code, b)
	}
	sunit := detach.Unit(sop.ID)
	waitUntil(t, 30*time.Second, sunit+" running the script", func() (bool, string) {
		return n.operation(sop.ID).Status == apitypes.StatusRunning && activeState(t, sunit) == "active" &&
			len(unitPIDs(sop.ID)) >= 2, unitPS(t, sop.ID)
	})

	// Replace nexusd on disk: a new inode under the same name, so nexusd and
	// the script unit's op-supervise both map a deleted file.
	copyFile(t, nexusdBin, nexusdBin+".nexus-new")
	if err := os.Rename(nexusdBin+".nexus-new", nexusdBin); err != nil {
		t.Fatal(err)
	}
	listed := mustSh(t, "needrestart", "-b")
	t.Logf("needrestart -b after the binary replacement:\n%s", strings.TrimSpace(listed))
	if !strings.Contains(listed, "NEEDRESTART-SVC: "+nexusdUnit) {
		t.Fatalf("G-15 finding: needrestart -b does not list %s after its binary was replaced (A-57)", nexusdUnit)
	}
	if !strings.Contains(listed, "NEEDRESTART-SVC: "+sunit) {
		t.Fatalf("run INVALID: needrestart -b does not list %s, so its staying up would prove nothing", sunit)
	}

	props := []string{"MainPID", "InvocationID", "ActiveState"}
	daemonBefore, scriptBefore := showUnit(t, nexusdUnit, props...), showUnit(t, sunit, props...)
	// The long-lived processes of the script unit: its op-supervise and the
	// shell running gate.sh (the loop's sleeps come and go).
	var scriptPIDs []int
	for _, pid := range unitPIDs(sop.ID) {
		if argv := procArgv(pid); slices.Contains(argv, "op-supervise") || slices.Contains(argv, script) {
			scriptPIDs = append(scriptPIDs, pid)
		}
	}
	if len(scriptPIDs) != 2 {
		t.Fatalf("script unit processes:\n%s", unitPS(t, sop.ID))
	}
	t.Logf("before the upgrade: %s %v; %s %v, op-supervise and gate.sh %v", nexusdUnit, daemonBefore, sunit, scriptBefore,
		scriptPIDs)

	op := startUpgrade(t, n, `{}`)
	got := waitOpAcrossRestart(t, n, op.ID, 4*time.Minute)
	daemonAfter, scriptAfter := showUnit(t, nexusdUnit, props...), showUnit(t, sunit, props...)
	t.Logf("after the upgrade: %s %v; %s %v", nexusdUnit, daemonAfter, sunit, scriptAfter)
	logs := n.opLogs(op.ID)
	t.Logf("upgrade %s %s: %s\noutput:\n%s", op.ID, got.Status, got.Result, logs)

	if daemonAfter["MainPID"] == daemonBefore["MainPID"] || daemonAfter["MainPID"] == "0" ||
		daemonAfter["InvocationID"] == daemonBefore["InvocationID"] || daemonAfter["ActiveState"] != "active" {
		t.Fatalf("control failed: needrestart did not restart %s (before %v, after %v)", nexusdUnit, daemonBefore, daemonAfter)
	}
	journal := mustSh(t, "journalctl", "-u", nexusdUnit, "--no-pager", "-o", "short-precise", "-n", "12")
	t.Logf("%s journal tail:\n%s", nexusdUnit, journal)
	// needrestart's own account, in the upgrade's output: it restarted
	// nexusd and systemd refused the script unit's restart.
	for _, want := range []string{"systemctl restart " + nexusdUnit, "Service restarts being refused by systemd", sunit} {
		if !strings.Contains(logs, want) {
			t.Errorf("the upgrade's output lacks needrestart's %q", want)
		}
	}

	if got.Status != apitypes.StatusSucceeded {
		t.Fatalf("upgrade: %s %+v", got.Status, got.Error)
	}
	var res spec.UpgradeResult
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatal(err)
	}
	x, ok, err := detach.ReadExit(aptOpDir(name, op.ID))
	if err != nil || !ok || x.ExitCode != 0 || res.ExitCode != 0 {
		t.Errorf("exit.json %+v %v %v, result exitCode %d", x, ok, err, res.ExitCode)
	}
	if v := aptVersion(t, aptPlain); v != want[1] || len(res.Changes) != 1 || res.Changes[0].NewVersion != v {
		t.Errorf("dpkg-query nxplain %s, candidate before %s, changes %+v", v, want[1], res.Changes)
	}
	if audit := mustSh(t, apt.Dpkg, "--audit"); strings.TrimSpace(audit) != "" {
		t.Errorf("dpkg --audit:\n%s", audit)
	}

	// A-21: the outdated script unit was not restarted.
	if scriptAfter["MainPID"] != scriptBefore["MainPID"] || scriptAfter["InvocationID"] != scriptBefore["InvocationID"] ||
		scriptAfter["ActiveState"] != "active" {
		t.Fatalf("A-21: %s was restarted: before %v after %v", sunit, scriptBefore, scriptAfter)
	}
	for _, pid := range scriptPIDs {
		if !pidAlive(pid) {
			t.Fatalf("A-21: process %d of %s died", pid, sunit)
		}
	}
	releaseFile(t, release)
	send := n.waitOp(sop.ID, time.Minute)
	slogs := n.opLogs(sop.ID)
	if send.Status != apitypes.StatusSucceeded || !strings.Contains(slogs, "gate: released") {
		t.Fatalf("script op %s %+v, output %q", send.Status, send.Error, slogs)
	}
	t.Logf("script op %s Succeeded with its one run: %s", sop.ID, strings.ReplaceAll(strings.TrimSpace(slogs), "\n", " | "))
}
