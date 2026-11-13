//go:build integration

// Platform gating of the T1 suites (WI 3.4) and the Alpine harness's own
// checks: TC-37, TC-46, TC-49 and TC-50 of planning/nginx-provider.

package integration

import (
	"bufio"
	"debug/elf"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/platform"
)

// detectedInit is the harness container's init system, detected as nexusd
// detects it.
func detectedInit() platform.InitSystem { return platform.Current().Init }

// requireInit skips a test written for one init system on the other, with
// the reason TC-46 reads in test.log: "requires systemd (detected openrc)".
func requireInit(t testing.TB, want platform.InitSystem) {
	t.Helper()
	if skip, reason := initGate(want, detectedInit()); skip {
		t.Skip(reason)
	}
}

// requireApt skips a test that measures or drives the apt provider on a
// host without apt and dpkg (Alpine), with the reason TC-46 reads:
// "requires apt (detected alpine)".
func requireApt(t testing.TB) {
	t.Helper()
	if o := platform.Current().OS; !o.IsDebianFamily() {
		t.Skipf("requires apt (detected %s)", o.ID)
	}
}

// initGate is requireInit's decision and skip reason.
func initGate(want, got platform.InitSystem) (bool, string) {
	return got != want, fmt.Sprintf("requires %s (detected %s)", want, got)
}

// openrcStarted reports whether rc-status output lists service as started.
func openrcStarted(rcStatus, service string) bool {
	re := regexp.MustCompile(`(?m)^ ` + regexp.QuoteMeta(service) + ` +\[ +started`)
	return re.MatchString(rcStatus)
}

// nginx-provider TC-37: The Alpine harness boots real OpenRC with cgroup v2
// and no bind mounts. run.sh asserts the host side: no bind: mount, the
// container removed, dist/ untouched, rc.log and /var/log/messages collected.
func TestAlpineHarnessSelfCheck(t *testing.T) {
	requireInit(t, platform.OpenRC)
	cmdline, err := os.ReadFile("/proc/1/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimRight(string(cmdline), "\x00"); got != "/sbin/init" {
		t.Fatalf("PID 1 is %q, want /sbin/init", got)
	}
	if target, err := os.Readlink("/sbin/init"); err != nil || !strings.HasSuffix(target, "busybox") {
		t.Fatalf("/sbin/init -> %q (%v), want BusyBox init", target, err)
	}
	rc := mustSh(t, "rc-status", "default")
	if !openrcStarted(rc, "docker") {
		t.Fatalf("docker is not started in the default runlevel:\n%s", rc)
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		t.Fatalf("no cgroup v2 hierarchy: %v", err)
	}
	probe := "/sys/fs/cgroup/nexus-ops-probe"
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatalf("cgroup v2 is not writable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatalf("rmdir %s: %v", probe, err)
	}
	// ID=alpine, as platform parses the base's identification file; the
	// suite reads no release data itself (TC-141).
	p := platform.Current()
	if p.OS.ID != "alpine" {
		t.Fatalf("platform.Current().OS.ID = %q, want alpine (%+v)", p.OS.ID, p.OS)
	}
	if p.Init != platform.OpenRC || !p.OS.IsAlpine() {
		t.Fatalf("platform.Current() = %+v, want OpenRC on alpine", p)
	}
	t.Logf("PID 1 /sbin/init (BusyBox), docker started, cgroup v2 writable (controllers: %s), %s, platform %+v",
		strings.TrimSpace(string(controllers)), strings.TrimSpace(mustSh(t, "openrc", "--version")), p)
}

// testResult is one --- PASS/FAIL/SKIP line of a `go test -v` log, with the
// reason lines indented under a SKIP.
type testResult struct {
	name, verdict, reason string
}

var (
	resultLine = regexp.MustCompile(`^(\s*)--- (PASS|FAIL|SKIP): (\S+) \(`)
	headerLine = regexp.MustCompile(`^=== (?:RUN|CONT|NAME|PAUSE)\s+(\S+)`)
)

// readResults parses a `go test -v` log. A test's output, its skip reason
// included, streams between its === header and its --- result, so the
// reason is the output since the test's last header. A result whose output
// was not streamed has it after the --- line, indented further.
func readResults(t *testing.T, path string) []testResult {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var (
		out     []testResult
		current string
		trailer = -1 // Index in out of a SKIP whose reason may follow it
		indent  string
	)
	output := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := headerLine.FindStringSubmatch(line); m != nil {
			current, trailer = m[1], -1
			continue
		}
		if m := resultLine.FindStringSubmatch(line); m != nil {
			r := testResult{name: m[3], verdict: m[2]}
			if r.verdict == "SKIP" {
				r.reason = output[r.name]
			}
			out = append(out, r)
			trailer, indent = -1, m[1]
			if r.verdict == "SKIP" && r.reason == "" {
				trailer = len(out) - 1
			}
			current = ""
			continue
		}
		switch {
		case trailer >= 0 && strings.HasPrefix(line, indent+"    "):
			out[trailer].reason += strings.TrimSpace(line) + "\n"
		case current != "" && strings.HasPrefix(line, "    "):
			output[current] += strings.TrimSpace(line) + "\n"
		default:
			trailer = -1
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func verdictOf(rs []testResult, name string) (testResult, bool) {
	for _, r := range rs {
		if r.name == name {
			return r, true
		}
	}
	return testResult{}, false
}

// gateRE is requireInit's skip reason, after the file:line prefix;
// aptGateRE is requireApt's.
var (
	gateRE    = regexp.MustCompile(`requires (systemd|openrc) \(detected (\w+)\)`)
	aptGateRE = regexp.MustCompile(`requires apt \(detected (\w+)\)`)
)

// privilegedSkipRE is a skip reason that names root, cgroups or an init
// system: such a case must run on its own platform.
var privilegedSkipRE = regexp.MustCompile(`(?i)root|cgroup|systemd|openrc|init system`)

// rootOnlyCgroupTests are TC-19, TC-20 and TC-21 of internal/detach.
var rootOnlyCgroupTests = []string{"TestOperationCgroupContainment", "TestCancelKillsSetsidGrandchild",
	"TestLeftoverSweepOnlyForControlGroup"}

// perInitCases are parity cases whose verdict as root depends on the init
// system by design: TestSystemdAvailabilityRefusesSilentDowngrade's
// negative case needs a host without systemd.
var perInitCases = map[string]map[platform.InitSystem]string{
	"TestSystemdAvailabilityRefusesSilentDowngrade": {platform.Systemd: "SKIP", platform.OpenRC: "PASS"},
}

// nginx-provider TC-46: Parity suites run as root on both harnesses, and
// gated cases are not silently skipped. run.sh runs this test on its own,
// after integration.test, with the logs copied to $NEXUS_HARNESS_LOGS.
func TestParitySuitesRanAsRoot(t *testing.T) {
	dir := os.Getenv("NEXUS_HARNESS_LOGS")
	if dir == "" {
		t.Fatal("NEXUS_HARNESS_LOGS is unset: run.sh runs this after the parity suites and integration.test")
	}
	here := detectedInit()
	other := map[platform.InitSystem]platform.InitSystem{platform.Systemd: platform.OpenRC, platform.OpenRC: platform.Systemd}[here]
	if other == "" {
		t.Fatalf("harness init system %s", here)
	}

	// The parity suites as root: every process-backend subtest and every
	// root-only cgroup test passes, and nothing root, cgroup or
	// init-related is skipped.
	for _, log := range []string{"ops.log", "detach.log"} {
		rs := readResults(t, filepath.Join(dir, log))
		var process int
		for _, r := range rs {
			if r.verdict == "FAIL" {
				t.Errorf("%s: %s FAIL", log, r.name)
			}
			if slices.Contains(strings.Split(r.name, "/"), "process") {
				process++
				if r.verdict != "PASS" {
					t.Errorf("%s: process-backend subtest %s is %s %s", log, r.name, r.verdict, r.reason)
				}
			}
			if want, ok := perInitCases[r.name]; ok {
				if r.verdict != want[here] {
					t.Errorf("%s (root): %s is %s on %s, want %s %s", log, r.name, r.verdict, here, want[here], r.reason)
				}
				continue
			}
			if r.verdict == "SKIP" && privilegedSkipRE.MatchString(r.reason) {
				t.Errorf("%s (root): %s skipped: %s", log, r.name, strings.TrimSpace(r.reason))
			}
		}
		t.Logf("%s: %d results, %d process-backend subtests", log, len(rs), process)
		if log == "ops.log" && process == 0 {
			t.Errorf("ops.log has no process-backend subtest: the parity suites did not run per backend")
		}
		if log == "detach.log" {
			for _, name := range rootOnlyCgroupTests {
				if r, ok := verdictOf(rs, name); !ok || r.verdict != "PASS" {
					t.Errorf("detach.log: %s is %q %s, want PASS as root with cgroup v2", name, r.verdict, r.reason)
				}
			}
		}
	}
	// The same suites as an unprivileged user pass too; there the root-only
	// cases skip with "needs root" (CON-10 is the process backend without
	// a cgroup).
	for _, log := range []string{"ops-nonroot.log", "detach-nonroot.log"} {
		rs := readResults(t, filepath.Join(dir, log))
		var skips []string
		for _, r := range rs {
			switch r.verdict {
			case "FAIL":
				t.Errorf("%s: %s FAIL", log, r.name)
			case "SKIP":
				skips = append(skips, r.name+": "+strings.TrimSpace(r.reason))
			}
		}
		t.Logf("%s: %d results; skips:\n%s", log, len(rs), strings.Join(skips, "\n"))
		if len(rs) == 0 {
			t.Errorf("%s has no results", log)
		}
	}

	// integration.test: gated cases skip only on the other platform, with
	// requireInit's reason; any other skip naming root, cgroups or an init
	// system fails.
	rs := readResults(t, filepath.Join(dir, "test.log"))
	var gated []string
	for _, r := range rs {
		if r.verdict == "FAIL" {
			t.Errorf("test.log: %s FAIL", r.name)
		}
		if r.verdict != "SKIP" {
			continue
		}
		if m := gateRE.FindStringSubmatch(r.reason); m != nil {
			if m[1] != string(other) || m[2] != string(here) {
				t.Errorf("test.log: %s skipped on %s with %q: a gate must name the other init system", r.name, here, m[0])
			}
			gated = append(gated, r.name)
			continue
		}
		if m := aptGateRE.FindStringSubmatch(r.reason); m != nil {
			if platform.Current().OS.IsDebianFamily() {
				t.Errorf("test.log: %s skipped for apt on %s, which has apt", r.name, m[1])
			}
			gated = append(gated, r.name)
			continue
		}
		if privilegedSkipRE.MatchString(r.reason) {
			t.Errorf("test.log: %s skipped on its own platform: %s", r.name, strings.TrimSpace(r.reason))
		}
	}
	t.Logf("test.log: %d results; %d gated to %s or to apt: %s", len(rs), len(gated), other, strings.Join(gated, ", "))
	// TC-50: the systemd cgroup control runs on systemd and is gated on
	// OpenRC, and the reverse for the OpenRC survival suite.
	for name, on := range map[string]platform.InitSystem{
		"TestSetsidChildInDaemonCgroupDies": platform.Systemd,
		"TestOpenRCServiceLifecycle":        platform.OpenRC,
	} {
		r, ok := verdictOf(rs, name)
		if !ok {
			t.Logf("test.log: %s did not run (RUN narrowed the run)", name)
			continue
		}
		want := "PASS"
		if on != here {
			want = "SKIP"
		}
		if r.verdict != want {
			t.Errorf("test.log: %s is %s on %s, want %s", name, r.verdict, here, want)
		}
	}
}

// nginx-provider TC-49: The release nexusd is static and runs on musl. The
// harness nexusd is built CGO_ENABLED=0, as RELEASE_BUILD is.
func TestReleaseBinaryStaticOnMusl(t *testing.T) {
	requireInit(t, platform.OpenRC)
	if out, err := sh(time.Minute, "apk", "info", "-e", "gcompat"); err == nil {
		t.Fatalf("gcompat is installed (%s): the musl check would prove nothing", strings.TrimSpace(out))
	}
	f, err := elf.Open(nexusdBin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			t.Fatalf("%s has a PT_INTERP program header", nexusdBin)
		}
	}
	if f.Section(".dynamic") != nil {
		needed, err := f.DynString(elf.DT_NEEDED)
		if err != nil || len(needed) > 0 {
			t.Fatalf("%s: DT_NEEDED %v (%v)", nexusdBin, needed, err)
		}
	}
	t.Logf("%s: no PT_INTERP, no DT_NEEDED; %s", nexusdBin, strings.TrimSpace(mustSh(t, nexusdBin, "-version")))

	// serve, then GET /healthz, with the C library being musl only.
	startPlainNexusd(t, "tc49")

	// runAs resolves from /etc/passwd through pure-Go os/user. validate-config
	// accepts the catalog but does not look the account up; the scripts
	// provider does at invoke (integrity.go lookupAccount), so a real user's
	// script runs under its UID and an unknown user's is refused.
	if _, err := sh(time.Minute, "id", "nexusrun"); err != nil {
		mustSh(t, "adduser", "-D", "-H", "nexusrun")
	}
	uid := strings.TrimSpace(mustSh(t, "id", "-u", "nexusrun"))
	dir := testDir(t, "tc49")
	script := installScript(t, filepath.Join(dir, "id.sh"), "#!/bin/sh\necho \"uid=$(id -u)\"\n")
	catalog := catalogYAML(entry{id: "known", path: script, runAs: "nexusrun"}, entry{id: "unknown", path: script, runAs: "nexus-no-such-user"})
	cfg := filepath.Join(t.TempDir(), "nexusd.yaml")
	body := fmt.Sprintf("listen: [\"127.0.0.1:7443\"]\ntls: {mode: off}\nproviders:\n  scripts:\n    enabled: true\n    catalog:\n%s", catalog)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustSh(t, nexusdBin, "validate-config", "--config", cfg)
	t.Logf("validate-config: %s", strings.TrimSpace(out))

	d := newScriptsd(t, "tc49", catalog)
	op, _ := d.waitTerminal(d.invoke("known", nil).ID, time.Minute)
	logs := d.logs(op.ID)
	t.Logf("runAs nexusrun: %s %v %q", op.Status, op.Error, logs)
	if op.Status != apitypes.StatusSucceeded || !strings.Contains(logs, "uid="+uid+"\n") {
		t.Errorf("runAs nexusrun: %s %v, output %q, want uid=%s", op.Status, op.Error, logs, uid)
	}
	code, resp := d.post("unknown", nil)
	refused := code >= 400
	if code == http.StatusAccepted {
		var started apitypes.Operation
		if err := json.Unmarshal(resp, &started); err != nil {
			t.Fatal(err)
		}
		op, _ := d.waitTerminal(started.ID, time.Minute)
		refused = op.Status == apitypes.StatusFailed
		resp = fmt.Appendf(resp, " -> %s %v", op.Status, op.Error)
	}
	t.Logf("runAs nexus-no-such-user: %d %s", code, resp)
	if !refused || !strings.Contains(string(resp), "nexus-no-such-user") {
		t.Errorf("runAs nexus-no-such-user: %d %s, want the invoke refused naming the user", code, resp)
	}
}

// nginx-provider TC-50: The Ubuntu harness still passes every existing
// suite, with platform gating. The run as a whole, its log committed to
// docs/verification/, is the evidence that the existing suites pass; this
// test checks the gate itself.
func TestRequireInitGating(t *testing.T) {
	requireInit(t, platform.Systemd)
	ran := false
	gated := t.Run("openrc-gated", func(t *testing.T) {
		requireInit(t, platform.OpenRC)
		ran = true
	})
	if ran || !gated {
		t.Fatalf("requireInit(OpenRC) on systemd did not skip (ran %v)", ran)
	}
	// The reason test.log shows for that skip, which TC-46 reads.
	if _, reason := initGate(platform.OpenRC, detectedInit()); reason != "requires openrc (detected systemd)" {
		t.Fatalf("the gate's reason is %q", reason)
	}
}
