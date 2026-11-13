//go:build integration

// T1 Alpine (WI 3.4): nexusd under real OpenRC, through the shipped
// /etc/init.d/nexusd (supervise-daemon), with the process backend running
// detached operations: TC-38 to TC-41, TC-44 and TC-E14 of
// planning/nginx-provider. Every test here is requireInit(OpenRC); the
// systemd survival proof is survival_test.go.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	aptspec "github.com/mauktik/nexus/internal/providers/apt/spec"
	dockerspec "github.com/mauktik/nexus/internal/providers/docker/spec"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

const (
	openrcConfD   = "/etc/conf.d/nexusd"
	openrcScript  = "/etc/init.d/nexusd"
	shippedOpenRC = "/harness/deploy/nexusd.openrc"
	cgroupRoot    = "/sys/fs/cgroup"
	opsCgroup     = cgroupRoot + "/nexus-ops"
)

// installedNexusd runs nexusd as an operator would on Alpine: the shipped
// /etc/init.d/nexusd, unchanged, with /etc/nexus/nexusd.yaml enabling the
// scripts provider with catalog over plain HTTP on 127.0.0.1:7443, and the
// default state and audit directories. It returns a scriptsd on that
// daemon; its cleanup stops nexusd and puts back the harness config.
func installedNexusd(t *testing.T, catalog string) *scriptsd {
	t.Helper()
	ensureUsers(t)
	lib(t, "stop-nexusd")
	d := &scriptsd{t: t, stateDir: nexusdState, auditDir: logDir, listen: exactAddr, tokens: map[string]string{},
		client: &http.Client{Timeout: 30 * time.Second}}
	for _, dir := range []string{nexusdState, logDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for tok, scopes := range map[string][]string{
		"run":  {spec.RunScope, spec.ReadScope},
		"read": {spec.ReadScope},
	} {
		v, _, err := auth.Create(d.stateDir, auth.CreateRequest{Name: tokenName(t) + "-" + tok, Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		d.tokens[tok] = v
	}
	cfg := filepath.Join(t.TempDir(), "nexusd.yaml")
	body := fmt.Sprintf("host: {name: harness}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n  scripts:\n    enabled: true\n    catalog:\n%s",
		exactAddr, catalog)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	lib(t, "install-nexusd", cfg)
	unitInstalled.Do(func() {})
	requireShippedScript(t)
	t.Cleanup(func() {
		lib(t, "stop-nexusd")
		harness := filepath.Join(t.TempDir(), "harness.yaml")
		if err := os.WriteFile(harness, []byte("listen: [\"127.0.0.1:7443\"]\ntls: {mode: auto}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		lib(t, "install-nexusd", harness)
	})
	lib(t, "start-nexusd")
	return d
}

// tokenName is the test's name as a token name: lower case, with the
// subtest separator and anything else outside [a-z0-9._-] replaced.
func tokenName(t *testing.T) string {
	n := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(t.Name()))
	return n[:min(len(n), 56)]
}

// requireShippedScript checks that /etc/init.d/nexusd is deploy/nexusd.openrc
// byte for byte.
func requireShippedScript(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(openrcScript)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(shippedOpenRC)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s differs from deploy/nexusd.openrc", openrcScript)
	}
}

// procStat is /proc/<pid>/stat's session, PPID and start time (fields 6, 4
// and 22), read after the last ')' as detach does.
type procStat struct {
	ppid, session int
	startTicks    uint64
}

func readProcStat(t *testing.T, pid int) procStat {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("stat of %d: %v", pid, err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:]) // f[0] is field 3
	ppid, err1 := strconv.Atoi(f[4-3])
	session, err2 := strconv.Atoi(f[6-3])
	start, err3 := strconv.ParseUint(f[22-3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("stat of %d: %q", pid, s)
	}
	return procStat{ppid: ppid, session: session, startTicks: start}
}

// cgroupPath is the path of pid's cgroup from the 0:: line of
// /proc/<pid>/cgroup, such as /openrc.nexusd.
func cgroupPath(t *testing.T, pid int) string {
	t.Helper()
	for line := range strings.Lines(pidCgroup(t, pid)) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			return p
		}
	}
	t.Fatalf("pid %d has no cgroup v2 line: %s", pid, pidCgroup(t, pid))
	return ""
}

// supervisorOf waits for operation id's supervisor.json and returns the
// identity it records.
func supervisorOf(t *testing.T, d *scriptsd, id string) detach.Identity {
	t.Helper()
	var ident detach.Identity
	waitUntil(t, 30*time.Second, "supervisor.json of "+id, func() (bool, string) {
		var err error
		ident, err = detach.ReadIdentity(d.opDir(id))
		return err == nil && ident.PID > 0, fmt.Sprint(err)
	})
	return ident
}

// waitHealthy waits until nexusd answers /healthz from a PID other than
// not, and returns that PID.
func waitHealthy(t *testing.T, timeout time.Duration, not int) int {
	t.Helper()
	var pid int
	c := &http.Client{Timeout: 2 * time.Second}
	waitUntil(t, timeout, "nexusd to answer /healthz", func() (bool, string) {
		pid = openrcMainPID(t)
		if pid == 0 || pid == not {
			return false, fmt.Sprintf("main pid %d", pid)
		}
		resp, err := c.Get("http://" + exactAddr + "/healthz")
		if err != nil {
			return false, err.Error()
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, resp.Status
	})
	return pid
}

// liveSupervisors is the process backend's ListUnits over the default ops
// directory: the units whose supervisor identity is alive.
func liveSupervisors(t *testing.T) []string {
	t.Helper()
	opts := detach.ProcessBackend(detach.ProcessConfig{OpsDir: filepath.Join(nexusdState, "ops")})
	units, err := opts.ListUnits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return units
}

// messagesSince is /var/log/messages after its first n lines.
func messagesSince(t *testing.T, n int) string {
	t.Helper()
	b, err := os.ReadFile("/var/log/messages")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(b), "\n")
	if n > len(lines) {
		return ""
	}
	return strings.Join(lines[n:], "")
}

func messagesLines(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/var/log/messages")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if fi.Mode().Perm() != want || st.Uid != 0 || st.Gid != 0 {
		t.Fatalf("%s: %v %d:%d, want root:root %v", path, fi.Mode().Perm(), st.Uid, st.Gid, want)
	}
}

// nginx-provider TC-38: rc-service controls nexusd with readiness, reload,
// bounded stop and respawn, through the script install.sh installs.
func TestOpenRCServiceLifecycle(t *testing.T) {
	requireInit(t, platform.OpenRC)
	freshHost(t)
	runInstall(t)
	requireShippedScript(t)
	writeConfig(t, "host: {name: harness}\nlisten: [\"127.0.0.1:7443\"]\ntls: {mode: off}\nproviders:\n  scripts: {enabled: false}\n")
	tok := createToken(t, "tc38", spec.ReadScope)
	n := &nexusdInstance{t: t, listen: exactAddr, token: tok, client: &http.Client{Timeout: 30 * time.Second}}
	scriptsState := func() apitypes.ProviderState {
		var caps apitypes.Capabilities
		n.mustGet("/v1/capabilities?refresh=true", &caps)
		for _, p := range caps.Providers {
			if p.Name == "scripts" {
				return p.Status
			}
		}
		t.Fatalf("no scripts provider in %+v", caps.Providers)
		return ""
	}
	t.Cleanup(func() { _, _ = sh(time.Minute, "rc-service", "nexusd", "stop") })

	// Step 1: start; one answering nexusd; the pidfile names it or its
	// supervise-daemon (L-11).
	logBefore := messagesLines(t)
	start := time.Now()
	mustSh(t, "rc-service", "nexusd", "start")
	pid := waitHealthy(t, 30*time.Second, 0)
	t.Logf("rc-service nexusd start: /healthz answered after %v", time.Since(start).Round(time.Millisecond))
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("/healthz answered %v after the start, want within 2 s", took)
	}
	// An operation supervisor is also a process named nexusd (nexusd
	// op-supervise); the daemons are the ones running serve.
	pids := strings.Fields(mustSh(t, "pgrep", "-x", "-f", nexusdBin+" serve.*"))
	if len(pids) != 1 || pids[0] != strconv.Itoa(pid) {
		t.Fatalf("pgrep nexusd serve: %v, want exactly the answering %d", pids, pid)
	}
	b, err := os.ReadFile(openrcPidfile)
	if err != nil {
		t.Fatal(err)
	}
	pf, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || !pidAlive(pf) {
		t.Fatalf("pidfile %s names %q, not a live process", openrcPidfile, b)
	}
	switch comm := procComm(pf); {
	case comm == "nexusd" && pf == pid:
		t.Logf("pidfile names: nexusd (pid %d)", pf)
	case isSuperviseDaemon(pf) && procStatusField(pid, "PPid") == strconv.Itoa(pf):
		t.Logf("pidfile names: supervise-daemon (pid %d), parent of the answering nexusd %d", pf, pid)
	default:
		t.Fatalf("pidfile names pid %d (%s), neither the answering nexusd %d nor its supervise-daemon (nexusd PPid %s)",
			pf, comm, pid, procStatusField(pid, "PPid"))
	}
	if st := scriptsState(); st != apitypes.ProviderDisabled {
		t.Fatalf("scripts is %s before the reload, want disabled", st)
	}

	// Step 2: a reload applies a reloadable change in the same process.
	writeConfig(t, "host: {name: harness}\nlisten: [\"127.0.0.1:7443\"]\ntls: {mode: off}\nproviders:\n  scripts: {enabled: true}\n")
	reloadAt := messagesLines(t)
	mustSh(t, "rc-service", "nexusd", "reload")
	waitUntil(t, 15*time.Second, "scripts enabled after the reload", func() (bool, string) {
		st := scriptsState()
		return st == apitypes.ProviderEnabled, string(st)
	})
	if now := openrcMainPID(t); now != pid {
		t.Fatalf("nexusd pid %d after the reload, was %d", now, pid)
	}
	waitUntil(t, 10*time.Second, "a nexusd reload line in syslog", func() (bool, string) {
		return regexp.MustCompile(`nexusd: .*reload`).MatchString(messagesSince(t, reloadAt)), ""
	})

	// Step 3: supervise-daemon respawns a killed nexusd.
	killed := time.Now()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	respawned := waitHealthy(t, 2*time.Second+3*time.Second+5*time.Second, pid)
	took := time.Since(killed)
	t.Logf("kill -9 %d: nexusd %d answered after %v (respawn_delay=2)", pid, respawned, took.Round(time.Millisecond))
	if took > 2*time.Second+3*time.Second {
		t.Errorf("respawn took %v, want within respawn_delay plus 3 s", took)
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", respawned))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(environ), "NOTIFY_SOCKET=") {
		t.Errorf("nexusd %d has NOTIFY_SOCKET set", respawned)
	}
	if u := procStatusField(respawned, "Umask"); u != "0077" {
		t.Errorf("nexusd Umask: %s, want 0077 (FR-23)", u)
	}

	// Step 4: a bounded stop leaves no nexusd.
	stopAt := time.Now()
	mustSh(t, "rc-service", "nexusd", "stop")
	if took := time.Since(stopAt); took > 15*time.Second {
		t.Errorf("rc-service nexusd stop took %v, want within 15 s", took)
	}
	if out, err := sh(10*time.Second, "pgrep", "-x", "-f", nexusdBin+" serve.*"); err == nil {
		t.Fatalf("nexusd left after the stop: %s", out)
	}

	// Step 5: modes, syslog, no systemd notification.
	requireMode(t, nexusdState, 0o700)
	requireMode(t, logDir, 0o700)
	requireMode(t, filepath.Join(nexusdState, "state.db"), 0o600)
	logs := messagesSince(t, logBefore)
	tagged := regexp.MustCompile(`(?m)^.* nexusd: .*$`).FindAllString(logs, -1)
	if len(tagged) == 0 {
		t.Fatalf("/var/log/messages has no line tagged nexusd:\n%s", logs)
	}
	t.Logf("/var/log/messages: %d lines tagged nexusd, e.g. %s", len(tagged), tagged[0])
	if strings.Contains(strings.ToLower(logs), "sd_notify") || strings.Contains(logs, "NOTIFY_SOCKET") {
		t.Errorf("syslog mentions sd_notify:\n%s", logs)
	}
}

// sleepThenMark is a catalog script that sleeps secs and then writes
// marker.
func sleepThenMark(t *testing.T, path, marker string, secs int) string {
	t.Helper()
	return installScript(t, path, fmt.Sprintf("#!/bin/sh\necho \"sleeper: pid $$\"\nsleep %d\necho done > %s\necho \"sleeper: marked\"\n",
		secs, marker))
}

// nginx-provider TC-39: A detached operation survives rc-service nexusd
// restart and is finalized from exit.json.
func TestOpRunsAcrossNexusdRestart(t *testing.T) {
	requireInit(t, platform.OpenRC)
	dir := testDir(t, "tc39")
	m := markers(t, "tc39", "root")
	marker := filepath.Join(m, "marker")
	d := installedNexusd(t, catalogYAML(entry{id: "sleeper", path: sleepThenMark(t, filepath.Join(dir, "sleeper.sh"), marker, 20)}))
	oldNexusd := openrcMainPID(t)

	op := d.invoke("sleeper", nil)
	d.waitStatus(op.ID, apitypes.StatusRunning)
	ident := supervisorOf(t, d, op.ID)
	before := readProcStat(t, ident.PID)
	cgBefore := cgroupPath(t, ident.PID)
	t.Logf("supervisor %d before: session %d ppid %d startTicks %d cgroup %s; nexusd %d (cgroup %s)",
		ident.PID, before.session, before.ppid, before.startTicks, cgBefore, oldNexusd, cgroupPath(t, oldNexusd))

	mustSh(t, "rc-service", "nexusd", "restart")
	newNexusd := waitHealthy(t, 30*time.Second, oldNexusd)
	if !pidAlive(ident.PID) {
		t.Fatalf("supervisor %d died with the restart", ident.PID)
	}
	after := readProcStat(t, ident.PID)
	t.Logf("supervisor %d after restart (nexusd %d -> %d): session %d ppid %d startTicks %d cgroup %s",
		ident.PID, oldNexusd, newNexusd, after.session, after.ppid, after.startTicks, cgroupPath(t, ident.PID))
	if after.startTicks != ident.StartTicks || after.startTicks != before.startTicks {
		t.Errorf("startTicks %d after, %d before, %d recorded: not the same process", after.startTicks, before.startTicks, ident.StartTicks)
	}
	if after.session != ident.PID {
		t.Errorf("session %d, want the supervisor's own pid %d", after.session, ident.PID)
	}
	if after.ppid == oldNexusd {
		t.Errorf("ppid is still the old nexusd %d", oldNexusd)
	}

	end, _ := d.waitTerminal(op.ID, time.Minute)
	if end.Status != apitypes.StatusSucceeded {
		t.Fatalf("op ended %s %v, want Succeeded", end.Status, end.Error)
	}
	if end.Started == nil || end.Ended == nil || end.Ended.Sub(*end.Started) < 20*time.Second {
		t.Errorf("op ran %v to %v, want at least 20 s", end.Started, end.Ended)
	}
	if !exists(marker) {
		t.Errorf("marker %s missing", marker)
	}
	if out := d.logs(op.ID); strings.Contains(out, string(problem.OutcomeUnknown)) {
		t.Errorf("op log mentions outcome-unknown:\n%s", out)
	}
	t.Logf("op %s: %s after %v", op.ID, end.Status, end.Ended.Sub(*end.Started).Round(time.Millisecond))
}

// nginx-provider TC-40: A supervisor killed while nexusd is down ends
// outcome-unknown.
func TestLostSupervisorWhileDownIsOutcomeUnknown(t *testing.T) {
	requireInit(t, platform.OpenRC)
	dir := testDir(t, "tc40")
	m := markers(t, "tc40", "root")
	d := installedNexusd(t, catalogYAML(entry{id: "sleeper",
		path: sleepThenMark(t, filepath.Join(dir, "sleeper.sh"), filepath.Join(m, "marker"), 300)}))
	op := d.invoke("sleeper", nil)
	d.waitStatus(op.ID, apitypes.StatusRunning)
	ident := supervisorOf(t, d, op.ID)
	unit := detach.Unit(op.ID)
	if units := liveSupervisors(t); !slices.Contains(units, unit) {
		t.Fatalf("ListUnits %v lacks %s while it runs", units, unit)
	}

	mustSh(t, "rc-service", "nexusd", "stop")
	victims := append([]int{ident.PID}, childrenOf(ident.PID)...)
	if len(victims) < 2 {
		t.Fatalf("supervisor %d has no child", ident.PID)
	}
	for _, pid := range victims {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatalf("kill -9 %d: %v", pid, err)
		}
	}
	waitUntil(t, 15*time.Second, "the supervisor and its child to die", func() (bool, string) {
		return !slices.ContainsFunc(victims, pidAlive), fmt.Sprint(victims)
	})
	if exists(filepath.Join(d.opDir(op.ID), detach.ExitFile)) {
		t.Fatal("exit.json exists: the supervisor was not lost")
	}
	t.Logf("killed supervisor %d and child %v while nexusd was stopped", ident.PID, victims[1:])

	lib(t, "start-nexusd")
	end, took := d.waitTerminal(op.ID, lostBound)
	t.Logf("%s after the start: %s %v in %v", op.ID, end.Status, end.Error, took)
	if end.Status != apitypes.StatusFailed || problemSlug(end) != string(problem.OutcomeUnknown) {
		t.Fatalf("op ended %s %v, want Failed outcome-unknown", end.Status, end.Error)
	}
	if took >= lostBound {
		t.Fatalf("terminal %v after the start, bound %v", took, lostBound)
	}
	if units := liveSupervisors(t); slices.Contains(units, unit) {
		t.Fatalf("ListUnits still lists %s: %v", unit, units)
	}
}

// nginx-provider TC-41: Cancel ends a script Canceled with no surviving
// descendant, a setsid grandchild included (cgroup v2).
func TestCancelLeavesNoDescendantOnOpenRC(t *testing.T) {
	requireInit(t, platform.OpenRC)
	dir := testDir(t, "tc41")
	m := markers(t, "tc41", "root")
	script := installScript(t, filepath.Join(dir, "escaper.sh"), fmt.Sprintf(
		"#!/bin/sh\nsetsid sleep 1000 &\necho $! > %[1]s/grandchild\necho $$ > %[1]s/child\nwhile :; do sleep 1; done\n", m))
	d := installedNexusd(t, catalogYAML(entry{id: "escaper", path: script}))
	op := d.invoke("escaper", nil)
	grandchild := waitPIDFile(t, filepath.Join(m, "grandchild"))
	child := waitPIDFile(t, filepath.Join(m, "child"))
	waitUntil(t, 10*time.Second, "the grandchild in its own session", func() (bool, string) {
		return pidAlive(grandchild) && readProcStat(t, grandchild).session == grandchild, ""
	})
	t.Logf("child %d, setsid grandchild %d (cgroup %s)", child, grandchild, cgroupPath(t, grandchild))

	if code, b := d.cancel("run", op.ID); code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, b)
	}
	end, took := d.waitTerminal(op.ID, time.Minute)
	t.Logf("%s: %s after %v", op.ID, end.Status, took)
	if end.Status != apitypes.StatusCanceled {
		t.Fatalf("op ended %s %v, want Canceled", end.Status, end.Error)
	}
	waitUntil(t, 15*time.Second, "no descendant left", func() (bool, string) {
		ps := mustSh(t, "ps", "-e", "-o", "pid=")
		alive := slices.ContainsFunc(strings.Fields(ps), func(p string) bool {
			return p == strconv.Itoa(grandchild) || p == strconv.Itoa(child)
		})
		return !alive && len(opCgroupPIDs(op.ID)) == 0, fmt.Sprintf("in ps %v, cgroup procs %v", alive, opCgroupPIDs(op.ID))
	})
	if _, err := os.Stat(filepath.Join(opsCgroup, op.ID)); err == nil {
		t.Logf("%s/%s remains, empty", opsCgroup, op.ID)
	} else {
		t.Logf("%s/%s is gone", opsCgroup, op.ID)
	}
}

// nginx-provider TC-44: apt reports unavailable on Alpine while the other
// providers answer.
func TestAptUnavailableOnAlpine(t *testing.T) {
	requireInit(t, platform.OpenRC)
	providers := "  apt:\n    enabled: true\n  scripts:\n    enabled: true\n  docker:\n    enabled: true\n"
	n := newNexusd(t, "tc44", providers)
	tok, _, err := auth.Create(filepath.Join(nexusdState, "nexusd-tc44"), auth.CreateRequest{Name: "tc44",
		Scopes: []string{aptspec.ReadScope, aptspec.WriteScope, dockerspec.ReadScope, spec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	cfgPath := "/etc/nexus/nexusd-tc44.yaml"
	if out, err := sh(time.Minute, nexusdBin, "validate-config", "--config", cfgPath); err != nil {
		t.Fatalf("validate-config: %v\n%s", err, out)
	}

	// Step 1: the CLI's view of the capabilities.
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cli := fmt.Sprintf("defaultHost: alpine\nhosts:\n  alpine: {url: \"http://%s\", token: %s}\n", n.listen, tok)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cli), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runNexus(home, "capability", "get", "--output", "json")
	t.Logf("nexus capability get --output json: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	if r.code != 0 {
		t.Fatalf("nexus capability get: exit %d", r.code)
	}
	var list []apitypes.Capabilities // -o json is a list, one item per host
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list) != 1 {
		t.Fatalf("capability get output: %v, %d items", err, len(list))
	}
	caps := list[0]
	if caps.InitSystem != string(platform.OpenRC) || caps.OS.ID != "alpine" {
		t.Errorf("capabilities report init %q os %q, want openrc on alpine", caps.InitSystem, caps.OS.ID)
	}
	byName := map[string]apitypes.ProviderCapability{}
	for _, p := range caps.Providers {
		byName[p.Name] = p
	}
	apt := byName["apt"]
	if apt.Status != apitypes.ProviderUnavailable || !strings.Contains(apt.Reason, "apt") || !strings.Contains(apt.Reason, "dpkg") {
		t.Errorf("apt: %s %q, want unavailable with a reason naming apt and dpkg", apt.Status, apt.Reason)
	}
	for _, name := range []string{"scripts", "docker"} {
		if p := byName[name]; p.Status != apitypes.ProviderEnabled {
			t.Errorf("%s: %s %q, want enabled", name, p.Status, p.Reason)
		}
	}

	// Step 2: an apt op answers capability-unavailable.
	code, b := n.post("/v1/providers/apt/actions/refresh", "{}")
	var pe problem.Error
	_ = json.Unmarshal(b, &pe)
	t.Logf("POST apt refresh: %d %s", code, b)
	if code != http.StatusServiceUnavailable || pe.Slug() != problem.CapabilityUnavailable {
		t.Errorf("apt refresh: %d %s, want 503 capability-unavailable", code, b)
	}

	// Step 3: scripts and containers list.
	for _, at := range []string{"/v1/providers/scripts/scripts", containersAt} {
		if code, b := n.get(at); code != http.StatusOK {
			t.Errorf("GET %s: %d %s", at, code, b)
		}
	}
}

// openrcCgroupBug reports whether the installed rc-cgroup.sh has the
// cgroup_running() of OpenRC issue #910: it looks for /sys/fs/cgroup/<svc>,
// not the openrc.<svc> group a unified host has, so rc_cgroup_cleanup=YES
// never runs. Upstream commit 1cf3944 fixed it.
const rcCgroupSh = "/usr/libexec/rc/sh/rc-cgroup.sh"

var (
	rcCgroupBuggy = `[ -d "/sys/fs/cgroup/${RC_SVCNAME}" ] ||`
	rcCgroupFixed = `[ -d "/sys/fs/cgroup/openrc.${RC_SVCNAME}" ] ||`
)

// applyOpenRC910Fix replaces cgroup_running()'s paths with upstream
// 1cf3944's for the test, and restores the file at cleanup.
func applyOpenRC910Fix(t *testing.T) {
	t.Helper()
	orig, err := os.ReadFile(rcCgroupSh)
	if err != nil {
		t.Fatal(err)
	}
	s := string(orig)
	if strings.Contains(s, rcCgroupFixed) {
		return
	}
	fixed := strings.NewReplacer(
		`[ -d "/sys/fs/cgroup/unified/${RC_SVCNAME}" ] ||`, `[ -d "/sys/fs/cgroup/unified/openrc.${RC_SVCNAME}" ] ||`,
		rcCgroupBuggy, rcCgroupFixed,
	).Replace(s)
	if !strings.Contains(fixed, rcCgroupFixed) || !strings.Contains(fixed, "unified/openrc.${RC_SVCNAME}") {
		t.Fatalf("%s: cgroup_running() is not the one 1cf3944 fixes", rcCgroupSh)
	}
	if err := os.WriteFile(rcCgroupSh, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(rcCgroupSh, orig, 0o644); err != nil {
			t.Errorf("restore %s: %v", rcCgroupSh, err)
		}
	})
}

// canary places a sleep in nexusd's own cgroup, read from the 0:: line of
// /proc/<nexusd>/cgroup, and returns its PID once its cgroup equals
// nexusd's.
func canary(t *testing.T) int {
	t.Helper()
	nexusd := openrcMainPID(t)
	path := cgroupPath(t, nexusd)
	pidFile := filepath.Join(t.TempDir(), "canary.pid")
	mustSh(t, "sh", "-c", fmt.Sprintf("sh -c 'echo $$ > %s/cgroup.procs; echo $$ > %s; exec sleep 1000' >/dev/null 2>&1 &",
		filepath.Join(cgroupRoot, path), pidFile))
	pid := waitPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	waitUntil(t, 5*time.Second, "the canary in nexusd's cgroup", func() (bool, string) {
		return pidAlive(pid) && cgroupPath(t, pid) == path, ""
	})
	t.Logf("canary %d in nexusd %d's cgroup %s", pid, nexusd, path)
	return pid
}

// setCgroupCleanup writes /etc/conf.d/nexusd with rc_cgroup_cleanup="YES",
// or removes it; cleanup removes it.
func setCgroupCleanup(t *testing.T, yes bool) {
	t.Helper()
	if !yes {
		if err := os.Remove(openrcConfD); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(openrcConfD, []byte("rc_cgroup_cleanup=\"YES\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(openrcConfD) })
}

// nginx-provider TC-E14: Stopping nexusd with rc_cgroup_cleanup=YES does
// not kill a supervisor. Stock OpenRC has issue #910 (rc_cgroup_cleanup is a
// no-op on a unified host); the stock subtest records that, and the
// 1cf3944 subtest applies the upstream fix so that cleanup is proven in
// effect before supervisor survival is counted.
func TestCgroupCleanupDoesNotKillSupervisor(t *testing.T) {
	requireInit(t, platform.OpenRC)
	stock, err := os.ReadFile(rcCgroupSh)
	if err != nil {
		t.Fatal(err)
	}
	dir := testDir(t, "tce14")
	m := markers(t, "tce14", "root")
	marker := filepath.Join(m, "marker")
	catalog := catalogYAML(entry{id: "sleeper", path: sleepThenMark(t, filepath.Join(dir, "sleeper.sh"), marker, 20)})
	version := strings.TrimSpace(mustSh(t, "openrc", "--version"))

	t.Run("stock", func(t *testing.T) {
		// The stock rc-cgroup.sh, case B only: is YES in effect?
		installedNexusd(t, catalog)
		lib(t, "stop-nexusd")
		setCgroupCleanup(t, true)
		lib(t, "start-nexusd")
		c := canary(t)
		mustSh(t, "rc-service", "nexusd", "stop")
		time.Sleep(5 * time.Second)
		buggy := strings.Contains(string(stock), rcCgroupBuggy)
		t.Logf("%s, stock rc-cgroup.sh (issue #910 present: %v): canary alive 5 s after the stop with YES: %v",
			version, buggy, pidAlive(c))
		if pidAlive(c) && !buggy {
			t.Fatalf("rc_cgroup_cleanup=YES not in effect with a cgroup_running() that has the 1cf3944 fix")
		}
		if !pidAlive(c) && buggy {
			t.Fatalf("the canary died with a cgroup_running() that has issue #910: unexplained")
		}
	})

	t.Run("1cf3944", func(t *testing.T) {
		applyOpenRC910Fix(t)

		// Case A, control: the script's default is NO.
		d := installedNexusd(t, catalog)
		setCgroupCleanup(t, false)
		ca := canary(t)
		mustSh(t, "rc-service", "nexusd", "stop")
		time.Sleep(5 * time.Second)
		if !pidAlive(ca) {
			t.Fatalf("case A: the canary died with no rc_cgroup_cleanup line: the default is not NO")
		}
		t.Logf("case A: canary %d alive 5 s after the stop (default NO)", ca)
		_ = syscall.Kill(ca, syscall.SIGKILL)

		// Case B: YES, a canary and a running op.
		setCgroupCleanup(t, true)
		lib(t, "start-nexusd")
		cb := canary(t)
		nexusdCg := cgroupPath(t, openrcMainPID(t))
		op := d.invoke("sleeper", nil)
		d.waitStatus(op.ID, apitypes.StatusRunning)
		ident := supervisorOf(t, d, op.ID)
		supCg := cgroupPath(t, ident.PID)
		t.Logf("case B: supervisor %d startTicks %d cgroup %s; nexusd cgroup %s", ident.PID, ident.StartTicks, supCg, nexusdCg)
		mustSh(t, "rc-service", "nexusd", "stop")

		// 1: cleanup is in effect, or survival would be vacuous.
		deadline := time.Now().Add(5 * time.Second)
		for pidAlive(cb) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if pidAlive(cb) {
			t.Fatal("rc_cgroup_cleanup=YES not in effect; supervisor survival would be vacuous")
		}
		t.Logf("case B: canary %d gone after the stop: cleanup in effect", cb)

		// 2: only then, the supervisor.
		if !pidAlive(ident.PID) {
			t.Fatalf("supervisor %d was killed by the stop", ident.PID)
		}
		if st := readProcStat(t, ident.PID); st.startTicks != ident.StartTicks {
			t.Fatalf("supervisor startTicks %d, recorded %d", st.startTicks, ident.StartTicks)
		}
		if want := "/nexus-ops/" + op.ID; supCg != want || cgroupPath(t, ident.PID) != want || supCg == nexusdCg {
			t.Fatalf("supervisor cgroup %s (now %s), want %s, not nexusd's %s", supCg, cgroupPath(t, ident.PID), want, nexusdCg)
		}

		// 3: the op still ends Succeeded.
		lib(t, "start-nexusd")
		end, _ := d.waitTerminal(op.ID, time.Minute)
		if end.Status != apitypes.StatusSucceeded {
			t.Fatalf("op ended %s %v, want Succeeded", end.Status, end.Error)
		}
		if !exists(marker) {
			t.Errorf("marker %s missing", marker)
		}
		if out := d.logs(op.ID); strings.Contains(out, string(problem.OutcomeUnknown)) {
			t.Errorf("op log mentions outcome-unknown:\n%s", out)
		}
		t.Logf("case B: op %s %s; the supervisor survived a cleanup that killed the canary", op.ID, end.Status)
	})
}
