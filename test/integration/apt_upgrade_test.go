// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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
)

// apt.actions.upgrade against real apt and dpkg (WI 7.3), through the real
// nexusd under the shipped unit: Prepare (lock probe, audit, snapshot,
// name resolution, upgradable list) runs inside its sandbox, the upgrade
// in a real nexus-op-<id> unit. Every expected value comes from dpkg-query,
// apt-get -s, apt-cache policy, files and systemd read in the test, never
// from Nexus, and no assertion names the packages a mode upgrades (A-26):
// the preview is compared with the real outcome.

const upgradeAt = "/v1/providers/apt/actions/upgrade"

// upgradeFixtures are the packages of the fixture world, each at 1.0
// installed and 2.0 in the repository: plain upgrades, a new dependency, a
// Conflicts removal, a held package and a locally edited conffile.
var upgradeFixtures = []string{"nxother", "nxnewdep", "nxdep", "nxconfl", "nxvictim", "nxheld", "nxconf"}

// aptUpgradeWorld is aptFixture with the whole fixture world. Cleanup
// purges it.
func aptUpgradeWorld(t *testing.T) {
	t.Helper()
	aptFixture(t, true)
	purge := func() {
		_, _ = sh(time.Minute, "apt-mark", "unhold", "nxheld")
		for _, p := range upgradeFixtures {
			_, _ = sh(2*time.Minute, apt.Dpkg, "--purge", p)
		}
		_ = os.Remove("/etc/nxconf.conf")
	}
	purge()
	t.Cleanup(purge)
	debs := filepath.Join(t.TempDir(), "debs")
	for _, p := range []string{"nxother", "nxnewdep", "nxconfl", "nxvictim", "nxheld"} {
		mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", debs, p, "1.0"))
	}
	mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", debs, "nxconf", "1.0", "--conffile"))
	if err := os.WriteFile("/etc/nxconf.conf", []byte("nxconf local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSh(t, "apt-mark", "hold", "nxheld")
	aptBuild(t, "add", aptRepo, "nxother", "2.0")
	aptBuild(t, "add", aptRepo, "nxdep", "1.0")
	aptBuild(t, "add", aptRepo, "nxnewdep", "2.0", "--depends", "nxdep")
	aptBuild(t, "add", aptRepo, "nxconfl", "2.0", "--conflicts", "nxvictim")
	aptBuild(t, "add", aptRepo, "nxheld", "2.0")
	aptBuild(t, "add", aptRepo, "nxconf", "2.0", "--conffile")
	mustSh(t, apt.AptGet, "update", "-qq")
}

// aptdWith is aptd with extra apt settings, such as lockWait.
func aptdWith(t *testing.T, name, settings string) *nexusdInstance {
	t.Helper()
	n := newNexusd(t, name, "  apt:\n    enabled: true\n"+settings)
	tok, _, err := auth.Create(filepath.Join(nexusdState, "nexusd-"+name), auth.CreateRequest{Name: "apt",
		Scopes: []string{spec.ReadScope, spec.WriteScope}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	return n
}

// startUpgrade posts an upgrade and requires 202.
func startUpgrade(t *testing.T, n *nexusdInstance, body string) apitypes.Operation {
	t.Helper()
	code, b := n.post(upgradeAt, body)
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil || code != http.StatusAccepted {
		t.Fatalf("upgrade %s: %d %s", body, code, b)
	}
	return op
}

// upgrade runs an upgrade to its end within timeout.
func upgrade(t *testing.T, n *nexusdInstance, body string, timeout time.Duration) (apitypes.Operation, spec.UpgradeResult) {
	t.Helper()
	op := n.waitOp(startUpgrade(t, n, body).ID, timeout)
	var res spec.UpgradeResult
	if op.Status == apitypes.StatusSucceeded {
		if err := json.Unmarshal(op.Result, &res); err != nil {
			t.Fatalf("result %s: %v", op.Result, err)
		}
	}
	return op, res
}

// dpkgDiff is the test's own diff of two dpkg-query snapshots, by field
// splitting: "pkg:arch old>new", with an empty side for not installed.
func dpkgDiff(before, after string) (upgraded, installed, removed []string) {
	read := func(s string) map[string]string {
		m := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
			f := strings.Split(line, "\t")
			if len(f) == 4 && f[3] != "not-installed" && f[3] != "config-files" {
				m[f[0]+":"+f[1]] = f[2]
			}
		}
		return m
	}
	b, a := read(before), read(after)
	for k, v := range b {
		switch nv, ok := a[k]; {
		case !ok:
			removed = append(removed, k+" "+v+">")
		case nv != v:
			upgraded = append(upgraded, k+" "+v+">"+nv)
		}
	}
	for k, v := range a {
		if _, ok := b[k]; !ok {
			installed = append(installed, k+" >"+v)
		}
	}
	slices.Sort(upgraded)
	slices.Sort(installed)
	slices.Sort(removed)
	return upgraded, installed, removed
}

func previewList(l []spec.PreviewPackage) []string {
	var out []string
	for _, p := range l {
		out = append(out, p.Package+":"+p.Architecture+" "+p.OldVersion+">"+p.NewVersion)
	}
	return out
}

// testUpgradable is the test's own Upgradable: installed packages whose
// apt-cache policy Candidate differs from Installed.
func testUpgradable(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(aptDpkgSnapshot(t)), "\n") {
		if f := strings.Split(line, "\t"); f[3] != "not-installed" && f[3] != "config-files" {
			names = append(names, f[0])
		}
	}
	var out []string
	for name, e := range aptPolicy(t, names...) {
		if e[0] != "(none)" && e[1] != "(none)" && e[0] != e[1] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// testInsts is the Inst names of apt-get -s run by the test.
func testInsts(t *testing.T, args ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range strings.Split(mustSh(t, apt.AptGet, append([]string{"-s", "-y"}, args...)...), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "Inst" {
			out[f[1]] = true
		}
	}
	return out
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// aptOpUnits lists the nexus-op-* units systemd knows, loaded or not.
func aptOpUnits(t *testing.T) string {
	t.Helper()
	return mustSh(t, detach.Systemctl, "list-units", "--all", "--plain", "--no-legend", detach.UnitPrefix+"*")
}

// TC-128, TC-129, TC-132 and ASM-13: in each mode, on a fresh fixture
// world, whatIf leaves dpkg's status untouched and its preview equals the
// dpkg-query diff of the real run that follows, whatever the solver
// chooses; heldBack is the test's own Upgradable minus the Inst names of
// its own apt-get -s; standard removes nothing; the edited conffile keeps
// its local content and is listed, with no prompt, within 60 seconds.
func TestAptUpgradePreviewEqualsOutcome(t *testing.T) {
	requireInit(t, platform.Systemd)
	for _, tc := range []struct{ mode, aptCmd string }{{spec.ModeStandard, "upgrade"}, {spec.ModeFull, "dist-upgrade"}} {
		t.Run(tc.mode, func(t *testing.T) {
			aptUpgradeWorld(t)
			n := aptd(t, "apt-upgrade-"+tc.mode)
			body := fmt.Sprintf(`{"mode":%q}`, tc.mode)
			wantHeld := []string{}
			insts := testInsts(t, tc.aptCmd)
			for _, name := range testUpgradable(t) {
				if !insts[name] {
					wantHeld = append(wantHeld, name)
				}
			}
			confHash := fileHash(t, "/etc/nxconf.conf")

			// Step 1: whatIf.
			statusBefore, snapBefore := fileHash(t, apt.DpkgStatus), aptDpkgSnapshot(t)
			op, pv := upgrade(t, n, `{"mode":"`+tc.mode+`","whatIf":true}`, 2*time.Minute)
			if op.Status != apitypes.StatusSucceeded || pv.Preview == nil || !pv.WhatIf || pv.Changed {
				t.Fatalf("whatIf: %+v %+v", op, pv)
			}
			if h := fileHash(t, apt.DpkgStatus); h != statusBefore || aptDpkgSnapshot(t) != snapBefore {
				t.Fatal("whatIf changed dpkg's status")
			}
			var gotHeld []string
			for _, h := range pv.Preview.HeldBack {
				gotHeld = append(gotHeld, h.Package)
			}
			if !slices.Equal(gotHeld, wantHeld) {
				t.Errorf("heldBack %q, want Upgradable minus Inst %q", gotHeld, wantHeld)
			}
			t.Logf("preview %s", op.Result)

			// Step 2: the real run, bounded to 60 s (TC-132: no prompt hangs).
			start := time.Now()
			op, res := upgrade(t, n, body, 60*time.Second)
			if op.Status != apitypes.StatusSucceeded {
				t.Fatalf("upgrade: %+v", op)
			}
			t.Logf("upgrade %s in %s: %s", op.ID, time.Since(start).Round(time.Millisecond), op.Result)
			up, ins, rem := dpkgDiff(snapBefore, aptDpkgSnapshot(t))
			if got := previewList(pv.Preview.Upgraded); !slices.Equal(got, up) {
				t.Errorf("preview upgraded %q, real %q", got, up)
			}
			if got := previewList(pv.Preview.Installed); !slices.Equal(got, ins) {
				t.Errorf("preview installed %q, real %q", got, ins)
			}
			if got := previewList(pv.Preview.Removed); !slices.Equal(got, rem) {
				t.Errorf("preview removed %q, real %q", got, rem)
			}
			if tc.mode == spec.ModeStandard && len(rem) != 0 {
				t.Errorf("standard removed %q", rem)
			}
			if len(res.Changes) != len(up)+len(ins)+len(rem) || !res.Changed || res.ExitCode != 0 {
				t.Errorf("result changes %+v, diff %q %q %q", res.Changes, up, ins, rem)
			}
			// The Conflicts package is removed or kept back, as previewed.
			victimGone := slices.Contains(rem, "nxvictim:all 1.0>")
			conflKept := slices.Contains(gotHeld, "nxconfl")
			t.Logf("%s: nxvictim removed %v, nxconfl held back %v", tc.mode, victimGone, conflKept)
			if victimGone == conflKept {
				t.Errorf("the Conflicts pair is inconsistent: nxvictim removed %v, nxconfl held back %v", victimGone, conflKept)
			}

			if h := fileHash(t, "/etc/nxconf.conf"); h != confHash {
				t.Errorf("/etc/nxconf.conf changed")
			}
			if aptVersion(t, "nxconf") != "2.0" || !slices.Contains(res.ConffilesKept, "/etc/nxconf.conf") ||
				!res.ConffilesComplete {
				t.Errorf("nxconf %s, conffilesKept %q complete %v", aptVersion(t, "nxconf"), res.ConffilesKept, res.ConffilesComplete)
			}
			if logs := n.opLogs(op.ID); strings.Contains(logs, "What would you like to do about it") {
				t.Errorf("dpkg prompted:\n%s", logs)
			}
			// ASM-13: nothing classifies disruptive packages.
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(op.Result, &keys); err != nil {
				t.Fatal(err)
			}
			for k := range keys {
				if !slices.Contains([]string{"changed", "changes", "conffilesComplete", "conffilesKept", "exitCode", "mode",
					"rebootRequired", "rebootRequiredPackages", "whatIf", "warnings"}, k) {
					t.Errorf("unexpected result field %q", k)
				}
			}
		})
	}
}

// TC-130: a subset upgrade changes only the named package, and a standard
// subset whose upgrade needs a removal removes nothing.
func TestAptUpgradeSubset(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptUpgradeWorld(t)
	n := aptd(t, "apt-upgrade-subset")
	before := aptDpkgSnapshot(t)
	op, res := upgrade(t, n, `{"names":["nxplain"]}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("nxplain: %+v", op)
	}
	up, ins, rem := dpkgDiff(before, aptDpkgSnapshot(t))
	if !slices.Equal(up, []string{"nxplain:all 1.0>2.0"}) || len(ins)+len(rem) != 0 {
		t.Errorf("diff %q %q %q, want only nxplain", up, ins, rem)
	}
	if len(res.Changes) != 1 || res.Changes[0].Package != aptPlain || aptVersion(t, "nxother") != "1.0" {
		t.Errorf("result %+v, nxother %s", res.Changes, aptVersion(t, "nxother"))
	}

	before = aptDpkgSnapshot(t)
	op, _ = upgrade(t, n, `{"names":["nxconfl"]}`, 2*time.Minute)
	_, ins, rem = dpkgDiff(before, aptDpkgSnapshot(t))
	t.Logf("nxconfl in standard: %s %+v; diff installed %q removed %q", op.Status, op.Error, ins, rem)
	if len(rem) != 0 || aptStatus(t, "nxvictim") != "installed" {
		t.Errorf("standard removed %q; nxvictim %s", rem, aptStatus(t, "nxvictim"))
	}
	if op.Status == apitypes.StatusFailed && problemSlug(op) != string(problem.AptFailed) {
		t.Errorf("failed with %s, want apt-failed", problemSlug(op))
	}
}

// pid1Start is PID 1's start time in clock ticks, field 22 of /proc/1/stat.
func pid1Start(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/1/stat")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	return f[19]
}

// TC-131: the result lists the change, the reboot now required by the
// package's postinst and apt-get's exit status 0; nothing reboots: PID 1
// started at the same time and no shutdown job exists.
func TestAptUpgradeResultAndNoReboot(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, false)
	for _, f := range []string{apt.RebootMarker, apt.RebootPackagesFile} {
		if exists(f) {
			aptMoveAside(t, f)
		}
	}
	t.Cleanup(func() {
		_ = os.Remove(apt.RebootMarker)
		_ = os.Remove(apt.RebootPackagesFile)
		_, _ = sh(2*time.Minute, apt.Dpkg, "--purge", "nxboot")
	})
	mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", filepath.Join(t.TempDir(), "debs"), "nxboot", "1.0"))
	aptBuild(t, "add", aptRepo, "nxboot", "2.0", "--reboot-marker")
	mustSh(t, apt.AptGet, "update", "-qq")
	n := aptd(t, "apt-upgrade-reboot")
	pid1 := pid1Start(t)

	op, res := upgrade(t, n, `{"names":["nxboot"]}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("upgrade: %+v", op)
	}
	t.Logf("result %s", op.Result)
	if len(res.Changes) != 1 || res.Changes[0].Package != "nxboot" || res.Changes[0].OldVersion != "1.0" ||
		res.Changes[0].NewVersion != aptVersion(t, "nxboot") || res.Changes[0].NewVersion != "2.0" {
		t.Errorf("changes %+v", res.Changes)
	}
	if res.RebootRequired != spec.RebootTrue || !slices.Contains(res.RebootRequiredPackages, "nxboot") || res.ExitCode != 0 {
		t.Errorf("reboot %s %q, exit %d", res.RebootRequired, res.RebootRequiredPackages, res.ExitCode)
	}
	if !exists(apt.RebootMarker) {
		t.Error("the postinst did not create the marker: the test proved nothing")
	}
	time.Sleep(2 * time.Second)
	if got := pid1Start(t); got != pid1 {
		t.Errorf("PID 1 start time %s, was %s", got, pid1)
	}
	jobs := mustSh(t, "systemctl", "list-jobs", "--no-legend")
	for _, target := range []string{"reboot", "poweroff", "halt", "shutdown", "kexec"} {
		if strings.Contains(jobs, target) {
			t.Errorf("a %s job exists:\n%s", target, jobs)
		}
	}
	t.Logf("PID 1 start %s unchanged; jobs: %q", pid1, strings.TrimSpace(jobs))
}

// TC-217 step 1, TC-127, TC-126, TC-217 step 2 and the cancel of a waiting
// upgrade, each against a real dpkg -i holding the frontend lock in the
// gated postinst (S-1).
func TestAptUpgradeWaitsForTheLock(t *testing.T) {
	requireInit(t, platform.Systemd)
	t.Run("healthy transaction is waited on", func(t *testing.T) {
		aptFixture(t, true)
		n := aptdWith(t, "apt-upgrade-wait", "    lockWait: 120s\n")
		hold := aptHold(t, "nxgate")
		// Control (S-1): once dpkg is in the postinst, dpkg --audit reports
		// the healthy transaction. dpkg takes the lock before it unpacks,
		// so the audit is read until the package is half-configured.
		waitUntil(t, time.Minute, "dpkg --audit reporting the held transaction", func() (bool, string) {
			audit := mustSh(t, apt.Dpkg, "--audit")
			return strings.Contains(audit, "nxgate") && hold.running(), audit
		})
		op := startUpgrade(t, n, `{"names":["nxplain"]}`)
		time.Sleep(4 * time.Second)
		if got := n.operation(op.ID); got.Status != apitypes.StatusQueued {
			t.Fatalf("during the hold: %s %+v", got.Status, got.Error)
		}
		if units := aptOpUnits(t); strings.Contains(units, op.ID) {
			t.Errorf("a unit exists during the wait:\n%s", units)
		}
		if !hold.running() {
			t.Fatal("the holder ended early")
		}

		// TC-127: whatIf completes while the holder and the queued upgrade hold.
		wi, pv := upgrade(t, n, `{"names":["nxplain"],"whatIf":true}`, time.Minute)
		if wi.Status != apitypes.StatusSucceeded || pv.Preview == nil {
			t.Fatalf("whatIf during the hold: %+v", wi)
		}
		if got := previewList(pv.Preview.Upgraded); !slices.Equal(got, []string{"nxplain:all 1.0>2.0"}) ||
			!testInsts(t, "install", "--only-upgrade", "--no-remove", "--", aptPlain)[aptPlain] {
			t.Errorf("preview %q", got)
		}
		if st := n.operation(op.ID).Status; st != apitypes.StatusQueued || !hold.running() {
			t.Errorf("whatIf ended while the upgrade is %s and the holder running %v", st, hold.running())
		}
		t.Logf("whatIf %s Succeeded while dpkg %d holds and upgrade %s is Queued: %s", wi.ID, hold.pid, op.ID, wi.Result)

		hold.release()
		got := n.waitOp(op.ID, 2*time.Minute)
		if got.Status != apitypes.StatusSucceeded || aptVersion(t, aptPlain) != "2.0" {
			t.Fatalf("after the release: %+v, nxplain %s", got, aptVersion(t, aptPlain))
		}
	})

	t.Run("lock released during the wait", func(t *testing.T) {
		aptFixture(t, true)
		// The candidate is a gated 3.0, so the upgrade's apt-get stays
		// running long enough to read its real argv.
		gate := fmt.Sprintf("/run/nexus-gate-nxplain-%d", time.Now().UnixNano())
		aptBuild(t, "add", aptRepo, aptPlain, "3.0", "--gate", gate)
		mustSh(t, apt.AptGet, "update", "-qq")
		t.Cleanup(func() { _ = os.Remove(gate) })
		n := aptdWith(t, "apt-upgrade-release", "    lockWait: 20s\n")
		hold := aptHold(t, "nxgate")
		op := startUpgrade(t, n, `{"names":["nxplain"]}`)
		time.Sleep(4 * time.Second)
		hold.release()

		// The rest of lockWait goes to apt as DPkg::Lock::Timeout: the
		// argv of the apt-get running in the operation's unit.
		var argv []string
		waitUntil(t, 2*time.Minute, "apt-get in the upgrade's unit", func() (bool, string) {
			for _, pid := range unitPIDs(op.ID) {
				b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
				if a := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"); err == nil && a[0] == apt.AptGet {
					argv = a
					return true, ""
				}
			}
			return false, string(n.operation(op.ID).Status)
		})
		secs := -1
		for _, a := range argv {
			if v, ok := strings.CutPrefix(a, "DPkg::Lock::Timeout="); ok {
				secs, _ = strconv.Atoi(v)
			}
		}
		if secs < 1 || secs > 17 {
			t.Errorf("DPkg::Lock::Timeout=%d after a wait of about 4 s of 20 s; argv %q", secs, argv)
		}
		t.Logf("apt-get in the unit: %q", argv)
		if err := os.WriteFile(gate, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		got := n.waitOp(op.ID, 2*time.Minute)
		if got.Status != apitypes.StatusSucceeded || aptVersion(t, aptPlain) != "3.0" {
			t.Fatalf("%+v, nxplain %s", got, aptVersion(t, aptPlain))
		}
	})

	t.Run("busy at lockWait", func(t *testing.T) {
		aptFixture(t, true)
		n := aptdWith(t, "apt-upgrade-busy", "    lockWait: 5s\n")
		hold := aptHold(t, "nxgate")
		op, _ := upgrade(t, n, `{"names":["nxplain"]}`, time.Minute)
		if op.Status != apitypes.StatusFailed || problemSlug(op) != string(problem.PackageManagerBusy) {
			t.Fatalf("%+v, want package-manager-busy", op)
		}
		if !hold.running() || aptVersion(t, aptPlain) != "1.0" {
			t.Errorf("holder running %v, nxplain %s", hold.running(), aptVersion(t, aptPlain))
		}
		t.Logf("%s: %s", op.Status, op.Error.Error())
	})

	t.Run("cancel during the wait", func(t *testing.T) {
		aptFixture(t, true)
		n := aptdWith(t, "apt-upgrade-cancel", "    lockWait: 120s\n")
		hold := aptHold(t, "nxgate")
		op := startUpgrade(t, n, `{"names":["nxplain"]}`)
		time.Sleep(3 * time.Second)
		if code, b := n.post("/v1/operations/"+op.ID+"/cancel", "{}"); code/100 != 2 {
			t.Fatalf("cancel: %d %s", code, b)
		}
		got := n.waitOp(op.ID, time.Minute)
		if got.Status != apitypes.StatusCanceled {
			t.Fatalf("%+v, want Canceled", got)
		}
		time.Sleep(2 * time.Second)
		if units := aptOpUnits(t); strings.Contains(units, op.ID) {
			t.Errorf("a unit exists for the canceled upgrade:\n%s", units)
		}
		if exists(filepath.Join(aptOpDir("apt-upgrade-cancel", op.ID), apt.PreFile)) ||
			exists(filepath.Join(aptOpDir("apt-upgrade-cancel", op.ID), detach.SpecFile)) {
			t.Error("the canceled upgrade wrote pre.json or spec.json")
		}
		if !hold.running() || aptVersion(t, aptPlain) != "1.0" || n.operation(op.ID).Status != apitypes.StatusCanceled {
			t.Errorf("holder running %v, nxplain %s", hold.running(), aptVersion(t, aptPlain))
		}
		t.Logf("canceled while waiting on dpkg %d: %s, no unit", hold.pid, got.Status)
	})
}

// lockInodes are the inodes of dpkg's lock files.
func lockInodes(t *testing.T) string {
	t.Helper()
	var out []string
	for _, f := range []string{apt.FrontendLock, "/var/lib/dpkg/lock"} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s=%d", f, fi.Sys().(*syscall.Stat_t).Ino))
	}
	return strings.Join(out, " ")
}

// TC-ERR47: a real apt-get install outside Nexus holds the frontend lock;
// apt-get itself still fails rc 100 with the lock text (S-3), and the
// upgrade fails package-manager-busy after lockWait, logging the holder's
// PID from F_GETLK, which is apt-get's; the holder is untouched and the
// lock files keep their inodes.
func TestAptUpgradeExternalHolderIsBusy(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	gate := fmt.Sprintf("/run/nexus-gate-nxslow-%d", time.Now().UnixNano())
	aptBuild(t, "add", aptRepo, "nxslow", "1.0", "--gate", gate)
	mustSh(t, apt.AptGet, "update", "-qq")
	t.Cleanup(func() { _, _ = sh(2*time.Minute, apt.Dpkg, "--purge", "nxslow") })
	n := aptdWith(t, "apt-upgrade-holder", "    lockWait: 5s\n")
	since := time.Now().Add(-time.Second)

	holder := exec.Command(apt.AptGet, "-y", "install", "--", "nxslow")
	holder.Env = apt.Env()
	var out strings.Builder
	holder.Stdout, holder.Stderr = &out, &out
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if err := os.WriteFile(gate, nil, 0o644); err != nil {
			t.Error(err)
		}
		if err := holder.Wait(); err != nil {
			t.Errorf("the holder apt-get: %v\n%s", err, out.String())
		}
		_ = os.Remove(gate)
	}
	t.Cleanup(release)
	waitUntil(t, time.Minute, "apt-get holding the frontend lock", func() (bool, string) {
		pid := frontendHolder(t)
		return pid == holder.Process.Pid, fmt.Sprintf("holder %d, apt-get %d", pid, holder.Process.Pid)
	})
	inodes := lockInodes(t)

	// Control (S-3).
	ctl := exec.Command(apt.AptGet, "-y", "-o", "DPkg::Lock::Timeout=3", "upgrade")
	ctl.Env = apt.Env()
	var cout, cerr strings.Builder
	ctl.Stdout, ctl.Stderr = &cout, &cerr
	err := ctl.Run()
	if ctl.ProcessState.ExitCode() != 100 || !(strings.Contains(cerr.String(), "E: Unable to acquire the dpkg frontend lock") ||
		strings.Contains(cerr.String(), "E: Could not get lock")) {
		t.Fatalf("control: %v rc %d\n%s", err, ctl.ProcessState.ExitCode(), cerr.String())
	}
	t.Logf("control: apt-get upgrade rc %d: %s", ctl.ProcessState.ExitCode(), strings.TrimSpace(cerr.String()))

	start := time.Now()
	op, _ := upgrade(t, n, `{}`, time.Minute)
	took := op.Ended.Sub(op.Created)
	if op.Status != apitypes.StatusFailed || problemSlug(op) != string(problem.PackageManagerBusy) {
		t.Fatalf("%+v, want package-manager-busy", op)
	}
	if took < 5*time.Second || time.Since(start) > 40*time.Second {
		t.Errorf("busy after %s", took)
	}
	if frontendHolder(t) != holder.Process.Pid || holder.ProcessState != nil {
		t.Error("the holder is no longer holding the lock")
	}
	if got := lockInodes(t); got != inodes {
		t.Errorf("lock inodes %s, were %s", got, inodes)
	}
	journal := mustSh(t, "journalctl", "-u", nexusdUnit, "--no-pager", "-o", "cat", "--since", since.Format("2006-01-02 15:04:05"))
	if want := fmt.Sprintf(`"holderPid":%d`, holder.Process.Pid); !strings.Contains(journal, want) {
		t.Errorf("the journal lacks %s:\n%s", want, journal)
	}
	t.Logf("busy after %s: %s; holder apt-get %d logged; inodes %s", took.Round(time.Millisecond), op.Error.Error(),
		holder.Process.Pid, inodes)
	release()
	if !holder.ProcessState.Success() {
		t.Errorf("the holder ended %s, not by itself", holder.ProcessState)
	}
}

// TC-ERR49: a real interrupted dpkg state, no lock holder: the upgrade
// fails package-manager-broken and repairs nothing.
func TestAptUpgradeBrokenStateIsReported(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	n := aptd(t, "apt-upgrade-broken")
	aptUnpack(t, "nxhalf")
	audit, hash := mustSh(t, apt.Dpkg, "--audit"), fileHash(t, apt.DpkgStatus)
	op, _ := upgrade(t, n, `{}`, time.Minute)
	if op.Status != apitypes.StatusFailed || problemSlug(op) != string(problem.PackageManagerBroken) {
		t.Fatalf("%+v, want package-manager-broken", op)
	}
	if got := mustSh(t, apt.Dpkg, "--audit"); got != audit {
		t.Errorf("audit changed:\n%s\nwas\n%s", got, audit)
	}
	if got := fileHash(t, apt.DpkgStatus); got != hash || aptStatus(t, "nxhalf") != "unpacked" {
		t.Error("dpkg's status changed: something repaired it")
	}
	if units := aptOpUnits(t); strings.Contains(units, op.ID) {
		t.Errorf("a unit ran:\n%s", units)
	}
	t.Logf("%s: %s", op.Status, op.Error.Error())
}

// TC-ERR50: a failing postinst is apt-failed with apt-get's exit status;
// a bad mode is 400; nothing to do succeeds with no changes.
func TestAptUpgradeFailureModes(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, false)
	t.Cleanup(func() { _, _ = sh(2*time.Minute, apt.Dpkg, "--purge", "nxfail") })
	mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", filepath.Join(t.TempDir(), "debs"), "nxfail", "1.0"))
	aptBuild(t, "add", aptRepo, "nxfail", "2.0", "--fail-postinst")
	mustSh(t, apt.AptGet, "update", "-qq")
	n := aptd(t, "apt-upgrade-fail")

	op, _ := upgrade(t, n, `{"names":["nxfail"]}`, 2*time.Minute)
	x, ok, err := detach.ReadExit(aptOpDir("apt-upgrade-fail", op.ID))
	if err != nil || !ok {
		t.Fatalf("exit.json: %v %v", ok, err)
	}
	if op.Status != apitypes.StatusFailed || problemSlug(op) != string(problem.AptFailed) || x.ExitCode == 0 ||
		!strings.Contains(op.Error.Detail, fmt.Sprintf("status %d", x.ExitCode)) {
		t.Fatalf("%+v exit %+v, want apt-failed naming the status", op, x)
	}
	t.Logf("%s: %s (exit.json %d)", op.Status, op.Error.Error(), x.ExitCode)
	mustSh(t, apt.Dpkg, "--purge", "nxfail")

	code, b := n.post(upgradeAt, `{"mode":"bogus"}`)
	if code != http.StatusBadRequest {
		t.Errorf("mode bogus: %d %s", code, b)
	}

	op, res := upgrade(t, n, `{}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded || res.Changed || len(res.Changes) != 0 || res.ExitCode != 0 {
		t.Errorf("nothing to do: %+v %s", op, op.Result)
	}
}

// TC-E24: Finalize reads files only. nexusd is killed with SIGKILL while
// the upgrade's unit runs (its postinst gated), and restarted after the
// unit ended: the result lists the conffile with conffilesComplete true.
// A second run whose dpkg.log is rotated during the run has
// conffilesComplete false.
func TestAptUpgradeFinalizeAcrossRestartAndRotation(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, false)
	t.Cleanup(func() {
		_, _ = sh(2*time.Minute, apt.Dpkg, "--purge", "nxconf")
		_ = os.Remove("/etc/nxconf.conf")
	})
	mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", filepath.Join(t.TempDir(), "debs"), "nxconf", "1.0", "--conffile"))
	if err := os.WriteFile("/etc/nxconf.conf", []byte("nxconf local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const name = "apt-upgrade-restart"
	n := aptd(t, name)

	gated := func(version string) (gate string, op apitypes.Operation) {
		gate = fmt.Sprintf("/run/nexus-gate-nxconf-%d", time.Now().UnixNano())
		aptBuild(t, "add", aptRepo, "nxconf", version, "--conffile", "--gate", gate)
		mustSh(t, apt.AptGet, "update", "-qq")
		op = startUpgrade(t, n, `{"names":["nxconf"]}`)
		waitUntil(t, 2*time.Minute, "the upgrade's dpkg in the gated postinst", func() (bool, string) {
			return frontendHolder(t) != 0 && n.operation(op.ID).Status == apitypes.StatusRunning, ""
		})
		return gate, op
	}

	// Run 1: killed before finalization.
	gate, op := gated("2.0")
	kill9Nexusd(t, n)
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Minute, "exit.json", func() (bool, string) {
		_, ok, _ := detach.ReadExit(aptOpDir(name, op.ID))
		return ok, ""
	})
	restartNexusd(t)
	got := n.waitOp(op.ID, time.Minute)
	var res spec.UpgradeResult
	if got.Status != apitypes.StatusSucceeded || json.Unmarshal(got.Result, &res) != nil {
		t.Fatalf("after the restart: %+v", got)
	}
	if !slices.Equal(res.ConffilesKept, []string{"/etc/nxconf.conf"}) || !res.ConffilesComplete || len(res.Changes) != 1 ||
		res.Changes[0].NewVersion != aptVersion(t, "nxconf") {
		t.Errorf("result after the restart: %s", got.Result)
	}
	t.Logf("run 1 after kill -9 and restart: %s", got.Result)

	// Run 2: dpkg.log rotated during the run.
	gate, op = gated("3.0")
	rotated := apt.DpkgLog + ".nexus-rotated"
	if err := os.Rename(apt.DpkgLog, rotated); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Put the rotated lines back in front.
		cur, _ := os.ReadFile(apt.DpkgLog)
		old, _ := os.ReadFile(rotated)
		_ = os.WriteFile(apt.DpkgLog, append(old, cur...), 0o644)
		_ = os.Remove(rotated)
	})
	if err := os.WriteFile(apt.DpkgLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got = n.waitOp(op.ID, 2*time.Minute)
	res = spec.UpgradeResult{}
	if got.Status != apitypes.StatusSucceeded || json.Unmarshal(got.Result, &res) != nil {
		t.Fatalf("run 2: %+v", got)
	}
	if res.ConffilesComplete || aptVersion(t, "nxconf") != "3.0" {
		t.Errorf("run 2 with dpkg.log rotated: %s", got.Result)
	}
	t.Logf("run 2 with rotation: %s", got.Result)
}

// TC-ERR71: names apt reads as a removal, a suffix or a regular
// expression are not-found, in both modes, with and without whatIf, for a
// token holding packages:write only: no operation, no unit, no change. The
// controls prove apt still reads each one dangerously.
func TestAptUpgradeRejectsHostileNames(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	t.Cleanup(func() {
		for _, p := range []string{"nxnewdep", "nxdep"} {
			_, _ = sh(2*time.Minute, apt.Dpkg, "--purge", p)
		}
	})
	mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", filepath.Join(t.TempDir(), "debs"), "nxnewdep", "1.0"))
	aptBuild(t, "add", aptRepo, "nxdep", "1.0")
	aptBuild(t, "add", aptRepo, "nxnewdep", "2.0", "--depends", "nxdep")
	mustSh(t, apt.AptGet, "update", "-qq")
	const name = "apt-upgrade-hostile"
	n := aptd(t, name)
	writeOnly, _, err := auth.Create(filepath.Join(nexusdState, "nexusd-"+name), auth.CreateRequest{Name: "write-only",
		Scopes: []string{spec.WriteScope}})
	if err != nil {
		t.Fatal(err)
	}

	controls := map[string]string{"nxplain-": "Remv nxplain", "nxnewdep+": "Inst nxnewdep", "nxplai.": "Inst nxplain"}
	for h, want := range controls {
		out := mustSh(t, apt.AptGet, "-s", "-y", "install", "--only-upgrade", "--", h)
		if !strings.Contains(out, want) {
			t.Errorf("control: apt-get -s install --only-upgrade %s no longer prints %q, so this case no longer tests "+
				"the guard:\n%s", h, want, out)
		} else {
			t.Logf("control %s: %q", h, want)
		}
	}

	var ops apitypes.List[apitypes.Operation]
	n.mustGet("/v1/operations", &ops)
	opsBefore, snap, units := len(ops.Items), aptDpkgSnapshot(t), aptOpUnits(t)
	readToken := n.token
	n.token = writeOnly
	for h := range controls {
		for _, mode := range spec.Modes {
			for _, whatIf := range []bool{false, true} {
				body := fmt.Sprintf(`{"mode":%q,"names":[%q],"whatIf":%v}`, mode, h, whatIf)
				code, b := n.post(upgradeAt, body)
				var p problem.Error
				if err := json.Unmarshal(b, &p); err != nil || code != http.StatusNotFound || p.Slug() != problem.NotFound {
					t.Errorf("%s: %d %s, want 404 not-found", body, code, b)
				}
			}
		}
	}
	n.token = readToken
	n.mustGet("/v1/operations", &ops)
	if len(ops.Items) != opsBefore {
		t.Errorf("operations %d, were %d", len(ops.Items), opsBefore)
	}
	if aptDpkgSnapshot(t) != snap {
		t.Error("dpkg's packages changed")
	}
	if got := aptOpUnits(t); got != units {
		t.Errorf("units changed:\n%s\nwere\n%s", got, units)
	}
}
