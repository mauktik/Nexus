//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// apt.actions.install and apt.actions.remove against real apt and dpkg
// (WI 7.4), through the real nexusd under the shipped unit, each mutation
// in a real nexus-op-<id> unit. Every expected value comes from
// dpkg-query, apt-get -s, apt-mark, files and systemd read in the test,
// never from Nexus.

const (
	installAt = "/v1/providers/apt/actions/install"
	removeAt  = "/v1/providers/apt/actions/remove"
)

// aptdScopes is a real nexusd with apt enabled, extra apt settings, and a
// token holding scopes.
func aptdScopes(t *testing.T, name, settings string, scopes ...string) *nexusdInstance {
	t.Helper()
	n := newNexusd(t, name, "  apt:\n    enabled: true\n"+settings)
	n.token = aptToken(t, name, "apt", scopes...)
	return n
}

// aptToken mints a token of nexusd instance name.
func aptToken(t *testing.T, name, token string, scopes ...string) string {
	t.Helper()
	tok, _, err := auth.Create(filepath.Join(nexusdState, "nexusd-"+name), auth.CreateRequest{Name: token, Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

var allAptScopes = []string{spec.ReadScope, spec.WriteScope, spec.InstallScope}

// startPkg posts an apt action and requires 202.
func startPkg(t *testing.T, n *nexusdInstance, at, body string) apitypes.Operation {
	t.Helper()
	code, b := n.post(at, body)
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil || code != http.StatusAccepted {
		t.Fatalf("%s %s: %d %s", at, body, code, b)
	}
	return op
}

// pkgOp runs an install or remove to its end within timeout.
func pkgOp(t *testing.T, n *nexusdInstance, at, body string, timeout time.Duration) (apitypes.Operation, spec.PackagesResult) {
	t.Helper()
	op := n.waitOp(startPkg(t, n, at, body).ID, timeout)
	var res spec.PackagesResult
	if op.Status == apitypes.StatusSucceeded {
		if err := json.Unmarshal(op.Result, &res); err != nil {
			t.Fatalf("result %s: %v", op.Result, err)
		}
	}
	return op, res
}

// aptPurgeAround purges pkgs now and at cleanup.
func aptPurgeAround(t *testing.T, pkgs ...string) {
	t.Helper()
	purge := func() {
		for _, p := range pkgs {
			_, _ = sh(2*time.Minute, apt.Dpkg, "--purge", p)
		}
	}
	purge()
	t.Cleanup(purge)
}

// aptGate is a gate file for a --gate fixture; cleanup opens it, so no
// postinst is left waiting.
func aptGate(t *testing.T, pkg string) (path string, open func()) {
	t.Helper()
	path = fmt.Sprintf("/run/nexus-gate-%s-%d", pkg, time.Now().UnixNano())
	open = func() {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() {
		open()
		time.Sleep(time.Second)
		_ = os.Remove(path)
	})
	return path, open
}

// dpkgKnows reports whether dpkg-query lists pkg at all.
func dpkgKnows(pkg string) bool {
	_, err := sh(time.Minute, apt.DpkgQuery, "-W", "--", pkg)
	return err == nil
}

// aptStatusOrForgotten is dpkg-query's status word of pkg, or "forgotten" when
// dpkg no longer knows pkg at all: a package with no conffile and no other
// record naming it leaves no stanza once removed, so dpkg-query exits 1. The
// status read is fatal (aptStatus), so it must not run on a forgotten package.
func aptStatusOrForgotten(t *testing.T, pkg string) string {
	t.Helper()
	if !dpkgKnows(pkg) {
		return "forgotten"
	}
	return aptStatus(t, pkg)
}

// activeOpUnits are the ids among ids whose nexus-op unit is active,
// activating or deactivating now.
func activeOpUnits(t *testing.T, ids ...string) []string {
	t.Helper()
	out := mustSh(t, "systemctl", "list-units", "--plain", "--no-legend", "--state=active,activating,deactivating",
		"nexus-op-*")
	var active []string
	for _, id := range ids {
		if strings.Contains(out, "nexus-op-"+id+".service") {
			active = append(active, id)
		}
	}
	return active
}

// notBefore reports whether a is set and not before b.
func notBefore(a, b *time.Time) bool { return a != nil && b != nil && !a.Before(*b) }

// TC-133: an upgrade and two installs submitted together run one at a
// time in submission order: at every sample at most one of their units is
// active, each starts only after the one before ended, and all reach the
// versions dpkg-query reads back. Each postinst is gated, so every run is
// sampled while it holds dpkg.
func TestAptMutationsSerializeFIFO(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	aptPurgeAround(t, "nxslowa", "nxslowb")
	gU, openU := aptGate(t, "nxplain")
	gA, openA := aptGate(t, "nxslowa")
	gB, openB := aptGate(t, "nxslowb")
	aptBuild(t, "add", aptRepo, aptPlain, "3.0", "--gate", gU)
	aptBuild(t, "add", aptRepo, "nxslowa", "1.0", "--gate", gA)
	aptBuild(t, "add", aptRepo, "nxslowb", "1.0", "--gate", gB)
	mustSh(t, apt.AptGet, "update", "-qq")
	n := aptdScopes(t, "apt-install-fifo", "    lockWait: 300s\n", allAptScopes...)

	u := startPkg(t, n, upgradeAt, `{"names":["nxplain"]}`)
	a := startPkg(t, n, installAt, `{"names":["nxslowa"]}`)
	b := startPkg(t, n, installAt, `{"names":["nxslowb"]}`)
	ids := []string{u.ID, a.ID, b.ID}
	opens := map[string]func(){u.ID: openU, a.ID: openA, b.ID: openB}
	seen, opened, samples := map[string]int{}, map[string]bool{}, 0
	deadline := time.Now().Add(5 * time.Minute)
	for {
		active := activeOpUnits(t, ids...)
		samples++
		if len(active) > 1 {
			t.Errorf("sample %d: %d apt units active at once: %q", samples, len(active), active)
		}
		for _, id := range active {
			seen[id]++
			// Release a run once it was sampled while its dpkg holds the
			// frontend lock in the gated postinst.
			if seen[id] >= 5 && !opened[id] && frontendHolder(t) != 0 {
				opened[id] = true
				opens[id]()
			}
		}
		done := 0
		for _, id := range ids {
			if terminal(n.operation(id).Status) {
				done++
			}
		}
		if done == len(ids) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not all ended after 5 minutes; seen %v", seen)
		}
		time.Sleep(200 * time.Millisecond)
	}
	ops := make([]apitypes.Operation, len(ids))
	for i, id := range ids {
		ops[i] = n.operation(id)
		if ops[i].Status != apitypes.StatusSucceeded || !opened[id] {
			t.Errorf("op %s: %s %+v, sampled active %d times", id, ops[i].Status, ops[i].Error, seen[id])
		}
	}
	for i := 1; i < len(ops); i++ {
		if !notBefore(ops[i].Started, ops[i-1].Ended) {
			t.Errorf("op %s started %v, before op %s ended %v", ops[i].ID, ops[i].Started, ops[i-1].ID, ops[i-1].Ended)
		}
	}
	for pkg, want := range map[string]string{aptPlain: "3.0", "nxslowa": "1.0", "nxslowb": "1.0"} {
		if got := aptVersion(t, pkg); got != want || aptStatus(t, pkg) != "installed" {
			t.Errorf("%s %s %s, want %s installed", pkg, got, aptStatus(t, pkg), want)
		}
	}
	t.Logf("%d samples, at most one unit active; active per op %v; started %v, %v, %v", samples, seen,
		ops[0].Started, ops[1].Started, ops[2].Started)
}

// opAptGet is the PID of the apt-get in operation id's unit.
func opAptGet(t *testing.T, id string) int {
	t.Helper()
	for _, pid := range unitPIDs(id) {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err == nil && strings.SplitN(string(b), "\x00", 2)[0] == apt.AptGet {
			return pid
		}
	}
	t.Fatalf("no apt-get in the unit of %s", id)
	return 0
}

// TC-134: one install running (gated), two queued; nexusd is killed with
// SIGKILL and restarted, and a third install is sent as soon as the
// listener accepts. The running one continues in its unit; the restored
// queued ones run after it in order; the third starts only after both are
// terminal; all reach their real terminal states.
func TestAptQueueSurvivesRestart(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, false)
	aptPurgeAround(t, "nxslowa", "nxqb", "nxqc", "nxqd")
	gA, openA := aptGate(t, "nxslowa")
	aptBuild(t, "add", aptRepo, "nxslowa", "1.0", "--gate", gA)
	for _, p := range []string{"nxqb", "nxqc", "nxqd"} {
		aptBuild(t, "add", aptRepo, p, "1.0")
	}
	mustSh(t, apt.AptGet, "update", "-qq")
	const name = "apt-install-restart"
	n := aptdScopes(t, name, "    lockWait: 300s\n", allAptScopes...)

	a := startPkg(t, n, installAt, `{"names":["nxslowa"]}`)
	waitUntil(t, 2*time.Minute, "nxslowa's dpkg in the gated postinst", func() (bool, string) {
		return frontendHolder(t) != 0 && n.operation(a.ID).Status == apitypes.StatusRunning, ""
	})
	b := startPkg(t, n, installAt, `{"names":["nxqb"]}`)
	c := startPkg(t, n, installAt, `{"names":["nxqc"]}`)
	time.Sleep(time.Second)
	for _, id := range []string{b.ID, c.ID} {
		if st := n.operation(id).Status; st != apitypes.StatusQueued {
			t.Fatalf("op %s is %s before the kill, want Queued", id, st)
		}
	}
	aptGetPID := opAptGet(t, a.ID)

	kill9Nexusd(t, n)
	if got := activeOpUnits(t, a.ID); len(got) != 1 {
		t.Fatalf("the running install's unit is not active after the kill")
	}
	restartNexusd(t)
	// The third install goes in as soon as the listener accepts.
	var d apitypes.Operation
	client := &http.Client{Timeout: 10 * time.Second}
	waitUntil(t, time.Minute, "the listener to accept the third install", func() (bool, string) {
		req, err := http.NewRequest(http.MethodPost, "http://"+n.listen+installAt, strings.NewReader(`{"names":["nxqd"]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+n.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return false, err.Error()
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("third install: %d %s", resp.StatusCode, body)
		}
		return json.Unmarshal(body, &d) == nil, string(body)
	})
	if st := n.operation(a.ID).Status; st != apitypes.StatusRunning || frontendHolder(t) == 0 {
		t.Errorf("after the restart the running install is %s, frontend lock holder %d", st, frontendHolder(t))
	}
	if got := opAptGet(t, a.ID); got != aptGetPID {
		t.Errorf("the running install's apt-get is %d, was %d", got, aptGetPID)
	}
	time.Sleep(2 * time.Second)
	for _, id := range []string{b.ID, c.ID, d.ID} {
		if st := n.operation(id).Status; st != apitypes.StatusQueued {
			t.Errorf("op %s is %s while nxslowa runs, want Queued", id, st)
		}
	}
	openA()
	ops := []apitypes.Operation{}
	for _, id := range []string{a.ID, b.ID, c.ID, d.ID} {
		ops = append(ops, n.waitOp(id, 3*time.Minute))
	}
	for i, op := range ops {
		if op.Status != apitypes.StatusSucceeded {
			t.Errorf("op %s: %s %+v", op.ID, op.Status, op.Error)
		}
		if i > 0 && !notBefore(op.Started, ops[i-1].Ended) {
			t.Errorf("op %s started %v, before op %s ended %v", op.ID, op.Started, ops[i-1].ID, ops[i-1].Ended)
		}
	}
	for _, p := range []string{"nxslowa", "nxqb", "nxqc", "nxqd"} {
		if aptStatus(t, p) != "installed" || aptVersion(t, p) != "1.0" {
			t.Errorf("%s %s %s", p, aptStatus(t, p), aptVersion(t, p))
		}
	}
	t.Logf("apt-get %d of a kept running across kill -9 and restart", aptGetPID)
	t.Logf("after kill -9 and restart: a %s ended %v; b started %v; c started %v; third started %v (after c ended %v)",
		ops[0].Status, ops[0].Ended, ops[1].Started, ops[2].Started, ops[3].Started, ops[2].Ended)
}

// TC-136: a name holding "+" installs as itself, a pin installs the older
// of two versions while the newer is the candidate, and whatIf changes
// nothing; dpkg-query shows exactly the installed names and the pinned
// version, and each preview equals the real outcome that follows.
func TestAptInstallPinAndPlusNames(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	aptPurgeAround(t, "nxg++", "nxnewdep")
	mustSh(t, apt.Dpkg, "--purge", aptPlain)
	aptBuild(t, "add", aptRepo, aptPlain, "1.0")
	aptBuild(t, "add", aptRepo, "nxg++", "1.0")
	aptBuild(t, "add", aptRepo, "nxnewdep", "1.0")
	mustSh(t, apt.AptGet, "update", "-qq")
	if pol := aptPolicy(t, aptPlain)[aptPlain]; pol[1] != "2.0" {
		t.Fatalf("control: nxplain's candidate is %q, want 2.0", pol[1])
	}
	n := aptdScopes(t, "apt-install-pin", "", allAptScopes...)

	for _, tc := range []struct{ body, want string }{
		{`{"names":["nxg++"]}`, "nxg++:all >1.0"},
		{`{"names":["nxplain"],"version":"1.0"}`, "nxplain:all >1.0"},
	} {
		statusBefore, before := fileHash(t, apt.DpkgStatus), aptDpkgSnapshot(t)
		whatIf := strings.TrimSuffix(tc.body, "}") + `,"whatIf":true}`
		op, pv := pkgOp(t, n, installAt, whatIf, 2*time.Minute)
		if op.Status != apitypes.StatusSucceeded || pv.Preview == nil || !pv.WhatIf || pv.Changed {
			t.Fatalf("%s: %+v %s", whatIf, op, op.Result)
		}
		if fileHash(t, apt.DpkgStatus) != statusBefore || aptDpkgSnapshot(t) != before {
			t.Fatalf("%s changed dpkg's status", whatIf)
		}
		op, res := pkgOp(t, n, installAt, tc.body, 2*time.Minute)
		if op.Status != apitypes.StatusSucceeded || !res.Changed || res.ExitCode != 0 {
			t.Fatalf("%s: %+v", tc.body, op)
		}
		up, ins, rem := dpkgDiff(before, aptDpkgSnapshot(t))
		if !slices.Equal(ins, []string{tc.want}) || len(up)+len(rem) != 0 {
			t.Errorf("%s: dpkg diff %q %q %q, want only %s", tc.body, up, ins, rem, tc.want)
		}
		if got := previewList(pv.Preview.Installed); !slices.Equal(got, ins) || len(pv.Preview.Upgraded)+len(pv.Preview.Removed) != 0 {
			t.Errorf("%s: preview %+v, real %q", tc.body, pv.Preview, ins)
		}
		t.Logf("%s: %s", tc.body, op.Result)
	}
	if aptVersion(t, "nxg++") != "1.0" || aptVersion(t, aptPlain) != "1.0" || aptPolicy(t, aptPlain)[aptPlain][1] != "2.0" {
		t.Errorf("nxg++ %s, nxplain %s (candidate %s)", aptVersion(t, "nxg++"), aptVersion(t, aptPlain),
			aptPolicy(t, aptPlain)[aptPlain][1])
	}

	// whatIf of a new name: no change, and nothing installs it.
	statusBefore, before := fileHash(t, apt.DpkgStatus), aptDpkgSnapshot(t)
	op, pv := pkgOp(t, n, installAt, `{"names":["nxnewdep"],"whatIf":true}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded || pv.Preview == nil ||
		!slices.Equal(previewList(pv.Preview.Installed), []string{"nxnewdep:all >1.0"}) {
		t.Errorf("whatIf nxnewdep: %+v %s", op, op.Result)
	}
	if fileHash(t, apt.DpkgStatus) != statusBefore || aptDpkgSnapshot(t) != before || dpkgKnows("nxnewdep") {
		t.Error("whatIf changed dpkg's status")
	}
}

// TC-137: remove keeps the configuration files (config-files state) and
// the automatic dependency by default; purge deletes the configuration
// file; autoremove removes exactly the dependency apt's own preview
// reported, and nothing else.
func TestAptRemovePurgeAndAutoremove(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, false)
	aptPurgeAround(t, "nxapp", "nxdep")
	t.Cleanup(func() { _ = os.Remove("/etc/nxapp.conf") })
	aptBuild(t, "add", aptRepo, "nxdep", "1.0")
	aptBuild(t, "add", aptRepo, "nxapp", "1.0", "--conffile", "--depends", "nxdep")
	mustSh(t, apt.AptGet, "update", "-qq")
	// Control: autoremove would remove nothing before the test, so what it
	// removes later is the test's own dependency only.
	if out := mustSh(t, apt.AptGet, "-s", "autoremove"); strings.Contains(out, "Remv ") {
		t.Fatalf("control: the harness already has autoremovable packages:\n%s", out)
	}
	n := aptdScopes(t, "apt-remove", "", allAptScopes...)
	install := func() {
		t.Helper()
		if op, _ := pkgOp(t, n, installAt, `{"names":["nxapp"]}`, 2*time.Minute); op.Status != apitypes.StatusSucceeded {
			t.Fatalf("install nxapp: %+v", op)
		}
		auto := strings.Fields(mustSh(t, "apt-mark", "showauto"))
		if !slices.Contains(auto, "nxdep") || slices.Contains(auto, "nxapp") || aptStatus(t, "nxapp") != "installed" ||
			!exists("/etc/nxapp.conf") {
			t.Fatalf("after install: nxapp %s, auto %q", aptStatus(t, "nxapp"), auto)
		}
	}

	// Default: config-files state, the conffile and the dependency stay.
	install()
	if op, res := pkgOp(t, n, removeAt, `{"names":["nxapp"]}`, 2*time.Minute); op.Status != apitypes.StatusSucceeded || !res.Changed {
		t.Fatalf("remove: %+v", op)
	}
	if aptStatus(t, "nxapp") != "config-files" || !exists("/etc/nxapp.conf") || aptStatus(t, "nxdep") != "installed" {
		t.Errorf("default remove: nxapp %s, conffile %v, nxdep %s", aptStatus(t, "nxapp"), exists("/etc/nxapp.conf"),
			aptStatus(t, "nxdep"))
	}
	t.Logf("default: nxapp %s, /etc/nxapp.conf kept, nxdep %s", aptStatus(t, "nxapp"), aptStatus(t, "nxdep"))

	// Purge: the configuration file goes; the dependency stays.
	install()
	if op, _ := pkgOp(t, n, removeAt, `{"names":["nxapp"],"purge":true}`, 2*time.Minute); op.Status != apitypes.StatusSucceeded {
		t.Fatalf("purge: %+v", op)
	}
	if dpkgKnows("nxapp") || exists("/etc/nxapp.conf") || aptStatus(t, "nxdep") != "installed" {
		t.Errorf("purge: nxapp known %v, conffile %v, nxdep %s", dpkgKnows("nxapp"), exists("/etc/nxapp.conf"),
			aptStatus(t, "nxdep"))
	}

	// Autoremove: the preview's removals are exactly the real ones.
	install()
	before := aptDpkgSnapshot(t)
	op, pv := pkgOp(t, n, removeAt, `{"names":["nxapp"],"autoremove":true,"whatIf":true}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded || pv.Preview == nil || aptDpkgSnapshot(t) != before {
		t.Fatalf("whatIf autoremove: %+v", op)
	}
	op, res := pkgOp(t, n, removeAt, `{"names":["nxapp"],"autoremove":true}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("autoremove: %+v", op)
	}
	up, ins, rem := dpkgDiff(before, aptDpkgSnapshot(t))
	if got := previewList(pv.Preview.Removed); !slices.Equal(got, rem) || len(up)+len(ins) != 0 ||
		!slices.Equal(rem, []string{"nxapp:all 1.0>", "nxdep:all 1.0>"}) {
		t.Errorf("autoremove: preview removed %q, real %q (upgraded %q installed %q)", got, rem, up, ins)
	}
	if st := aptStatusOrForgotten(t, "nxdep"); st != "forgotten" && st != "not-installed" && st != "config-files" {
		t.Errorf("nxdep %s after autoremove", st)
	}
	t.Logf("autoremove: preview removed %q, real %q; result %s", previewList(pv.Preview.Removed), rem, op.Result)
	_ = res
}

// TC-138: install and remove need packages:install, upgrade needs
// packages:write; each mismatch is 403 naming the scope it needs, with
// whatIf too.
func TestAptInstallAndUpgradeScopes(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	const name = "apt-install-scopes"
	n := aptdScopes(t, name, "", allAptScopes...)
	full := n.token
	writeOnly := aptToken(t, name, "write-only", spec.WriteScope)
	installOnly := aptToken(t, name, "install-only", spec.InstallScope)

	for _, tc := range []struct {
		token, at, body, need string
	}{
		{writeOnly, installAt, `{"names":["nxplain"],"whatIf":true}`, spec.InstallScope},
		{writeOnly, removeAt, `{"names":["nxplain"],"whatIf":true}`, spec.InstallScope},
		{writeOnly, upgradeAt, `{"whatIf":true}`, ""},
		{installOnly, upgradeAt, `{"whatIf":true}`, spec.WriteScope},
		{installOnly, installAt, `{"names":["nxplain"],"whatIf":true}`, ""},
		{installOnly, removeAt, `{"names":["nxplain"],"whatIf":true}`, ""},
	} {
		n.token = tc.token
		code, b := n.post(tc.at, tc.body)
		n.token = full
		if tc.need == "" {
			var op apitypes.Operation
			if code != http.StatusAccepted || json.Unmarshal(b, &op) != nil {
				t.Errorf("%s %s: %d %s, want 202", tc.at, tc.body, code, b)
				continue
			}
			if got := n.waitOp(op.ID, 2*time.Minute); got.Status != apitypes.StatusSucceeded {
				t.Errorf("%s %s: %+v", tc.at, tc.body, got)
			}
			continue
		}
		var p struct {
			Type          string `json:"type"`
			Detail        string `json:"detail"`
			RequiredScope string `json:"requiredScope"`
		}
		if err := json.Unmarshal(b, &p); err != nil || code != http.StatusForbidden || p.RequiredScope != tc.need ||
			!strings.Contains(p.Detail, tc.need) {
			t.Errorf("%s %s: %d %s, want 403 naming %s", tc.at, tc.body, code, b, tc.need)
		} else {
			t.Logf("%s with the wrong token: 403 %s", tc.at, p.Detail)
		}
	}
}

// TC-ERR51: a running apt operation is not cancelable, and dpkg finishes
// cleanly; a queued one cancels and never runs; an upgrade waiting in
// Prepare on another process's lock cancels, and no unit or dpkg
// transaction of it ever appears.
func TestAptCancellationRules(t *testing.T) {
	requireInit(t, platform.Systemd)
	t.Run("running and queued", func(t *testing.T) {
		aptFixture(t, true)
		aptPurgeAround(t, "nxg++")
		gate, openGate := aptGate(t, aptPlain)
		aptBuild(t, "add", aptRepo, aptPlain, "3.0", "--gate", gate)
		aptBuild(t, "add", aptRepo, "nxg++", "1.0")
		mustSh(t, apt.AptGet, "update", "-qq")
		n := aptdScopes(t, "apt-cancel-running", "", allAptScopes...)
		u := startPkg(t, n, upgradeAt, `{"names":["nxplain"]}`)
		waitUntil(t, 2*time.Minute, "the upgrade's dpkg in the gated postinst", func() (bool, string) {
			return frontendHolder(t) != 0 && n.operation(u.ID).Status == apitypes.StatusRunning, ""
		})
		q := startPkg(t, n, installAt, `{"names":["nxg++"]}`)

		code, b := n.post("/v1/operations/"+u.ID+"/cancel", "{}")
		var p problem.Error
		if err := json.Unmarshal(b, &p); err != nil || code != http.StatusConflict || p.Slug() != problem.NotCancelable {
			t.Errorf("cancel of the running upgrade: %d %s, want 409 not-cancelable", code, b)
		}
		if code, b := n.post("/v1/operations/"+q.ID+"/cancel", "{}"); code/100 != 2 {
			t.Fatalf("cancel of the queued install: %d %s", code, b)
		}
		if got := n.waitOp(q.ID, time.Minute); got.Status != apitypes.StatusCanceled {
			t.Fatalf("queued install: %+v, want Canceled", got)
		}
		openGate()
		if got := n.waitOp(u.ID, 2*time.Minute); got.Status != apitypes.StatusSucceeded || aptVersion(t, aptPlain) != "3.0" {
			t.Fatalf("running upgrade: %+v, nxplain %s", got, aptVersion(t, aptPlain))
		}
		time.Sleep(2 * time.Second)
		if audit := mustSh(t, apt.Dpkg, "--audit"); strings.TrimSpace(audit) != "" {
			t.Errorf("dpkg --audit after the upgrade:\n%s", audit)
		}
		if dpkgKnows("nxg++") || strings.Contains(aptOpUnits(t), q.ID) || n.operation(q.ID).Started != nil {
			t.Errorf("the canceled install ran: nxg++ known %v, units:\n%s", dpkgKnows("nxg++"), aptOpUnits(t))
		}
		t.Logf("running: 409 %s; queued %s Canceled and never ran; upgrade Succeeded, dpkg --audit empty", p.Detail, q.ID)
	})

	t.Run("waiting in Prepare", func(t *testing.T) {
		aptFixture(t, true)
		n := aptdScopes(t, "apt-cancel-waiting", "    lockWait: 120s\n", allAptScopes...)
		hold := aptHold(t, "nxgate")
		logBefore, err := os.ReadFile(apt.DpkgLog)
		if err != nil {
			t.Fatal(err)
		}
		op := startPkg(t, n, upgradeAt, `{"names":["nxplain"]}`)
		time.Sleep(3 * time.Second)
		if st := n.operation(op.ID).Status; st != apitypes.StatusQueued {
			t.Fatalf("the waiting upgrade is %s, want Queued", st)
		}
		if code, b := n.post("/v1/operations/"+op.ID+"/cancel", "{}"); code/100 != 2 {
			t.Fatalf("cancel: %d %s", code, b)
		}
		if got := n.waitOp(op.ID, time.Minute); got.Status != apitypes.StatusCanceled {
			t.Fatalf("%+v, want Canceled", got)
		}
		hold.release()
		end := time.Now().Add(10 * time.Second)
		samples := 0
		for time.Now().Before(end) {
			if units := aptOpUnits(t); strings.Contains(units, op.ID) {
				t.Fatalf("a unit for the canceled upgrade appeared:\n%s", units)
			}
			samples++
			time.Sleep(500 * time.Millisecond)
		}
		logAfter, err := os.ReadFile(apt.DpkgLog)
		if err != nil {
			t.Fatal(err)
		}
		added := strings.TrimPrefix(string(logAfter), string(logBefore))
		for _, line := range strings.Split(added, "\n") {
			if strings.Contains(line, aptPlain) {
				t.Errorf("dpkg.log has a transaction of nxplain: %s", line)
			}
		}
		if aptVersion(t, aptPlain) != "1.0" || n.operation(op.ID).Status != apitypes.StatusCanceled {
			t.Errorf("nxplain %s, op %s", aptVersion(t, aptPlain), n.operation(op.ID).Status)
		}
		t.Logf("waiting upgrade Canceled; %d unit samples over 10 s after the release, none for it; dpkg.log gained %d "+
			"lines, none of nxplain", samples, strings.Count(added, "\n"))
	})
}

// aptFR25Attacks is the FR-25 list of internal/conformance/negative_test.go.
var aptFR25Attacks = []string{"; rm -rf /", "$(id)", "`id`", "|", "&&", "--force", "-o", "../../etc/passwd", "a\nid",
	"a\x00id", strings.Repeat("A", 64<<10)}

// nameAttacks and versionAttacks add FR-16's local files, options, globs
// and selectors for each input.
var (
	nameAttacks = append(slices.Clone(aptFR25Attacks), "x.deb", "/tmp/x.deb", "nxplain.deb:all", "--allow-unauthenticated",
		"nx*", "a/b", "UPPER", "", "a b", "nxplain=1.0", "nxplain/stable")
	versionAttacks = append(slices.Clone(aptFR25Attacks), "1;id", "-1", "1/x", "1*", "1=2", "/tmp/x.deb",
		"--allow-downgrades", "1 2", strings.Repeat("1", spec.MaxVersionLength+1))
)

// TC-ERR53: the S-4 names are not-found on install and remove, with and
// without whatIf, and the attack list is 400 validation-failed as names and
// as versions; no operation, no unit, no apt-get mutation, and dpkg's
// packages are identical. The controls prove apt-get still reads each name
// dangerously, so the guard is what stops them.
func TestAptInstallRejectsInjections(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	aptPurgeAround(t, "nxnewdep")
	aptBuild(t, "add", aptRepo, "nxnewdep", "1.0")
	mustSh(t, apt.AptGet, "update", "-qq")
	n := aptdScopes(t, "apt-install-injection", "", allAptScopes...)

	type attempt struct{ at, name, control string }
	attempts := []attempt{
		{installAt, "nxplain-", "Remv nxplain"},
		{removeAt, "nxnewdep+", "Inst nxnewdep"},
		{installAt, "nxplai.", "Inst nxplain"},
	}
	for _, a := range attempts {
		cmd := strings.TrimPrefix(a.at, "/v1/providers/apt/actions/")
		out := mustSh(t, apt.AptGet, "-s", "-y", cmd, "--", a.name)
		if !strings.Contains(out, a.control) {
			t.Errorf("control: apt-get -s %s %s no longer prints %q, so this case no longer tests the guard:\n%s",
				cmd, a.name, a.control, out)
		} else {
			t.Logf("control apt-get -s %s %s: %q", cmd, a.name, a.control)
		}
	}

	var ops apitypes.List[apitypes.Operation]
	n.mustGet("/v1/operations", &ops)
	opsBefore, snap, units, status := len(ops.Items), aptDpkgSnapshot(t), aptOpUnits(t), fileHash(t, apt.DpkgStatus)

	// Step 1: each S-4 name on both actions, with and without whatIf.
	for _, a := range attempts {
		for _, at := range []string{installAt, removeAt} {
			for _, whatIf := range []bool{false, true} {
				body := fmt.Sprintf(`{"names":[%q],"whatIf":%v}`, a.name, whatIf)
				code, b := n.post(at, body)
				var p problem.Error
				if err := json.Unmarshal(b, &p); err != nil || code != http.StatusNotFound || p.Slug() != problem.NotFound {
					t.Errorf("%s %s: %d %s, want 404 not-found", at, body, code, b)
				}
			}
		}
	}
	// Step 2: the attack list as names and as versions.
	rejected := 0
	type request struct{ at, body string }
	var requests []request
	for _, s := range nameAttacks {
		for _, at := range []string{installAt, removeAt} {
			b, err := json.Marshal(map[string]any{"names": []string{s}})
			if err != nil {
				t.Fatal(err)
			}
			requests = append(requests, request{at, string(b)})
		}
	}
	for _, s := range versionAttacks {
		b, err := json.Marshal(map[string]any{"names": []string{aptPlain}, "version": s})
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request{installAt, string(b)})
	}
	for _, x := range requests {
		code, b := n.post(x.at, x.body)
		var p problem.Error
		if err := json.Unmarshal(b, &p); err != nil || code != http.StatusBadRequest || p.Slug() != problem.ValidationFailed {
			t.Errorf("%s %.80q: %d %.300s, want 400 validation-failed", x.at, x.body, code, b)
		} else {
			rejected++
		}
	}
	t.Logf("%d attack requests rejected 400 validation-failed", rejected)

	n.mustGet("/v1/operations", &ops)
	if len(ops.Items) != opsBefore {
		t.Errorf("operations %d, were %d", len(ops.Items), opsBefore)
	}
	if aptDpkgSnapshot(t) != snap || fileHash(t, apt.DpkgStatus) != status {
		t.Error("dpkg's packages changed")
	}
	if got := aptOpUnits(t); got != units {
		t.Errorf("units changed:\n%s\nwere\n%s", got, units)
	}
}

// TC-ERR54: an install that needs a removal fails with a clear error and
// removes nothing. The control: without --no-remove apt would remove the
// installed package.
func TestAptInstallNeverRemoves(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, false)
	aptPurgeAround(t, "nxconfl", "nxvictim")
	mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", filepath.Join(t.TempDir(), "debs"), "nxvictim", "1.0"))
	aptBuild(t, "add", aptRepo, "nxconfl", "1.0", "--conflicts", "nxvictim")
	mustSh(t, apt.AptGet, "update", "-qq")
	if out := mustSh(t, apt.AptGet, "-s", "-y", "install", "--", "nxconfl"); !strings.Contains(out, "Remv nxvictim") {
		t.Fatalf("control: apt-get -s install nxconfl no longer removes nxvictim:\n%s", out)
	}
	n := aptdScopes(t, "apt-install-noremove", "", allAptScopes...)
	before := aptDpkgSnapshot(t)
	for _, whatIf := range []bool{true, false} {
		op, _ := pkgOp(t, n, installAt, fmt.Sprintf(`{"names":["nxconfl"],"whatIf":%v}`, whatIf), 2*time.Minute)
		if op.Status != apitypes.StatusFailed || problemSlug(op) != string(problem.AptFailed) ||
			!strings.Contains(op.Error.Detail, "needs packages removed, and install never removes one") {
			t.Errorf("whatIf %v: %+v, want apt-failed saying a removal is needed", whatIf, op)
		} else {
			t.Logf("whatIf %v: %s: %s", whatIf, op.Status, op.Error.Detail)
		}
		if logs := n.opLogs(op.ID); !strings.Contains(logs, "nxvictim") {
			t.Errorf("the operation log does not name nxvictim:\n%s", logs)
		}
	}
	if aptDpkgSnapshot(t) != before || aptStatus(t, "nxvictim") != "installed" || dpkgKnows("nxconfl") {
		t.Errorf("packages changed: nxvictim %s, nxconfl known %v", aptStatus(t, "nxvictim"), dpkgKnows("nxconfl"))
	}
}

// TC-ERR55: a valid and an unknown name together, or a remove of a package
// that is not installed, are not-found and start nothing; a version apt
// does not have is an operation that ends Failed with no changes.
func TestAptInstallUnknownNamesAndVersions(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	aptPurgeAround(t, "nxnewdep")
	aptBuild(t, "add", aptRepo, "nxnewdep", "1.0")
	mustSh(t, apt.AptGet, "update", "-qq")
	n := aptdScopes(t, "apt-install-unknown", "", allAptScopes...)
	var ops apitypes.List[apitypes.Operation]
	n.mustGet("/v1/operations", &ops)
	opsBefore, snap, units := len(ops.Items), aptDpkgSnapshot(t), aptOpUnits(t)
	for _, x := range []struct{ at, body string }{
		{installAt, `{"names":["nxplain","nxnone"]}`},
		{removeAt, `{"names":["nxnewdep"]}`},
	} {
		code, b := n.post(x.at, x.body)
		var p problem.Error
		if err := json.Unmarshal(b, &p); err != nil || code != http.StatusNotFound || p.Slug() != problem.NotFound {
			t.Errorf("%s %s: %d %s, want 404 not-found", x.at, x.body, code, b)
		} else {
			t.Logf("%s %s: 404 %s", x.at, x.body, p.Detail)
		}
	}
	n.mustGet("/v1/operations", &ops)
	if len(ops.Items) != opsBefore || aptOpUnits(t) != units {
		t.Errorf("operations %d (were %d) or units changed", len(ops.Items), opsBefore)
	}

	op, _ := pkgOp(t, n, installAt, `{"names":["nxplain"],"version":"9.9"}`, 2*time.Minute)
	if op.Status != apitypes.StatusFailed || problemSlug(op) != string(problem.AptFailed) {
		t.Errorf("version 9.9: %+v, want Failed apt-failed", op)
	} else {
		t.Logf("version 9.9: %s: %s", op.Status, op.Error.Detail)
	}
	if aptDpkgSnapshot(t) != snap || aptVersion(t, aptPlain) != "1.0" {
		t.Error("dpkg's packages changed")
	}
}

// CR-27 (S-4): apt-get reads a trailing "+" or "-" on a name as a modifier
// ("nxplain-" removes), and "nxg++" is both a package and such a name. Remove
// and a subset upgrade of nxg++ change exactly nxg++: the preview equals the
// real diff, and nothing else is installed, upgraded or removed.
func TestAptPlusNamesRemoveAndSubsetUpgrade(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	aptPurgeAround(t, "nxg++", "nxg+")
	aptBuild(t, "add", aptRepo, "nxg++", "1.0")
	mustSh(t, apt.AptGet, "update", "-qq")
	n := aptdScopes(t, "apt-plus-names", "", allAptScopes...)
	if op, _ := pkgOp(t, n, installAt, `{"names":["nxg++"]}`, 2*time.Minute); op.Status != apitypes.StatusSucceeded ||
		aptVersion(t, "nxg++") != "1.0" {
		t.Fatalf("install nxg++: %+v", op)
	}
	aptBuild(t, "add", aptRepo, "nxg++", "2.0")
	mustSh(t, apt.AptGet, "update", "-qq")

	// Subset upgrade: whatIf changes nothing and predicts the real diff.
	statusBefore, before := fileHash(t, apt.DpkgStatus), aptDpkgSnapshot(t)
	op, pv := upgrade(t, n, `{"names":["nxg++"],"whatIf":true}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded || pv.Preview == nil || fileHash(t, apt.DpkgStatus) != statusBefore ||
		aptDpkgSnapshot(t) != before {
		t.Fatalf("whatIf upgrade nxg++: %+v %s", op, op.Result)
	}
	op, res := upgrade(t, n, `{"names":["nxg++"]}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded || !res.Changed {
		t.Fatalf("upgrade nxg++: %+v", op)
	}
	up, ins, rem := dpkgDiff(before, aptDpkgSnapshot(t))
	if !slices.Equal(up, []string{"nxg++:all 1.0>2.0"}) || len(ins)+len(rem) != 0 ||
		!slices.Equal(previewList(pv.Preview.Upgraded), []string{"nxg++:all 1.0>2.0"}) {
		t.Errorf("upgrade nxg++: diff %q %q %q, preview %+v", up, ins, rem, pv.Preview)
	}

	// Remove: the package named is the one removed.
	statusBefore, before = fileHash(t, apt.DpkgStatus), aptDpkgSnapshot(t)
	op, pr := pkgOp(t, n, removeAt, `{"names":["nxg++"],"whatIf":true}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded || pr.Preview == nil || fileHash(t, apt.DpkgStatus) != statusBefore ||
		aptDpkgSnapshot(t) != before {
		t.Fatalf("whatIf remove nxg++: %+v %s", op, op.Result)
	}
	op, res2 := pkgOp(t, n, removeAt, `{"names":["nxg++"]}`, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded || !res2.Changed {
		t.Fatalf("remove nxg++: %+v", op)
	}
	up, ins, rem = dpkgDiff(before, aptDpkgSnapshot(t))
	if !slices.Equal(rem, []string{"nxg++:all 2.0>"}) || len(up)+len(ins) != 0 ||
		!slices.Equal(previewList(pr.Preview.Removed), rem) {
		t.Errorf("remove nxg++: diff %q %q %q, preview %+v", up, ins, rem, pr.Preview)
	}
	if st := aptStatusOrForgotten(t, "nxg++"); st != "forgotten" && st != "not-installed" && st != "config-files" {
		t.Errorf("nxg++ is %s after remove", st)
	}
}
