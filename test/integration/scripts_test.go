//go:build integration

// T1 tests of the script runner (WI 5.3): real nexusd under the shipped
// unit, real systemd transient units, real users deploy and other, real
// Docker and git. Fixtures come from test/harness/fixtures/scripts, which
// run.sh copies to /harness/fixtures; each test installs what it runs
// root-owned under /opt/nxtest, so the integrity check (FR-20) passes.
package integration

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
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
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/scripts"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
	"github.com/mauktik/nexus/internal/store"
)

const (
	fixtures   = "/harness/fixtures/scripts"
	scriptRoot = "/opt/nxtest"
	runRoot    = "/run/nxtest" // Markers scripts write; outside every sandbox
	sleeperEnv = "NEXUS_SCRIPTS_SLEEPER"
	sleeperImg = "nxtest-sleeper:latest"
)

// A copy of this test binary is the process of the containers the scripts
// start (TC-102): with sleeperEnv set it only sleeps, so no image is pulled.
func init() {
	if os.Getenv(sleeperEnv) == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
}

// users creates the harness accounts once: deploy, with a bash login
// shell, a home and the docker group, and other, a second user.
var users sync.Once

func ensureUsers(t *testing.T) {
	t.Helper()
	users.Do(func() {
		if _, err := sh(time.Minute, "id", "deploy"); err != nil {
			mustSh(t, "useradd", "--create-home", "--shell", "/bin/bash", "--user-group", "deploy")
		}
		if _, err := sh(time.Minute, "id", "other"); err != nil {
			mustSh(t, "useradd", "--create-home", "--shell", "/bin/sh", "--user-group", "other")
		}
		mustSh(t, "usermod", "--append", "--groups", "docker", "deploy")
	})
}

// installScript writes body as path, root:root 0755, creating root-owned
// 0755 parents up to scriptRoot.
func installScript(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// installFixture copies fixture name into dir, root:root 0755.
func installFixture(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return installScript(t, filepath.Join(dir, name), string(b))
}

// testDir is a fresh root-owned 0755 directory under scriptRoot.
func testDir(t *testing.T, name string) string {
	t.Helper()
	for _, d := range []string{scriptRoot, filepath.Join(scriptRoot, name)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := filepath.Join(scriptRoot, name)
	entries, err := os.ReadDir(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(d, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// markers is a fresh directory scripts write markers to, owned by owner.
func markers(t *testing.T, name, owner string) string {
	t.Helper()
	d := filepath.Join(runRoot, name)
	if err := os.RemoveAll(d); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if owner != "root" {
		mustSh(t, "chown", owner+":"+owner, d)
	}
	return d
}

// entry is one catalog entry, as YAML.
type entry struct {
	id, path, runAs, dir string
	extra                string // More YAML lines, indented by 8
}

func catalogYAML(entries ...entry) string {
	var b strings.Builder
	for _, e := range entries {
		dir := e.dir
		if dir == "" {
			dir = filepath.Dir(e.path)
		}
		runAs := e.runAs
		if runAs == "" {
			runAs = "root"
		}
		fmt.Fprintf(&b, "      - id: %s\n        description: T1 %s\n        path: %s\n        runAs: %s\n        workingDirectory: %s\n%s",
			e.id, e.id, e.path, runAs, dir, e.extra)
	}
	return b.String()
}

// scriptsd is the real nexusd under the shipped unit, through a drop-in
// that replaces ExecStart and sets Restart=no, with the scripts provider
// enabled and the catalog given.
type scriptsd struct {
	t        *testing.T
	stateDir string
	auditDir string
	listen   string
	tokens   map[string]string
	client   *http.Client
}

func newScriptsd(t *testing.T, name, catalog string) *scriptsd {
	t.Helper()
	installShippedUnit(t)
	ensureUsers(t)
	nextPort++
	d := &scriptsd{
		t: t, stateDir: filepath.Join(nexusdState, "nxs-"+name), auditDir: filepath.Join("/var/log/nexus", "nxs-"+name),
		listen: fmt.Sprintf("127.0.0.1:%d", nextPort), tokens: map[string]string{},
		client: &http.Client{Timeout: 30 * time.Second},
	}
	for _, dir := range []string{d.stateDir, d.auditDir} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for tok, scopes := range map[string][]string{
		"run":       {spec.RunScope, spec.ReadScope},
		"runonly":   {spec.RunScope},
		"read":      {spec.ReadScope},
		"deployapi": {"scripts:run:deploy-api"},
		"other":     {"scripts:run:other"},
	} {
		v, _, err := auth.Create(d.stateDir, auth.CreateRequest{Name: tok, Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		d.tokens[tok] = v
	}
	tmp := t.TempDir()
	cfgSrc := filepath.Join(tmp, "nexusd.yaml")
	cfg := fmt.Sprintf("host: {name: harness}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n  scripts:\n    enabled: true\n    catalog:\n%s",
		d.listen, catalog)
	if err := os.WriteFile(cfgSrc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := "/etc/nexus/nxs-" + name + ".yaml"
	lib(t, "install-config", cfgSrc, cfgPath)
	if out, err := sh(time.Minute, nexusdBin, "validate-config", "--config", cfgPath); err != nil {
		t.Fatalf("validate-config: %v\n%s", err, out)
	}
	dropin := fmt.Sprintf("[Service]\nExecStart=\nExecStart=%s serve --config %s --state-dir %s --audit-dir %s\nRestart=no\n",
		nexusdBin, cfgPath, d.stateDir, d.auditDir)
	dropinSrc := filepath.Join(tmp, "nxs.conf")
	if err := os.WriteFile(dropinSrc, []byte(dropin), 0o644); err != nil {
		t.Fatal(err)
	}
	lib(t, "stop-nexusd")
	lib(t, "remove-dropins")
	lib(t, "dropin", "nxs", dropinSrc)
	t.Cleanup(func() {
		lib(t, "stop-nexusd")
		lib(t, "remove-dropins")
	})
	d.start()
	return d
}

func (d *scriptsd) start() {
	d.t.Helper()
	lib(d.t, "start-nexusd")
	if st := activeState(d.t, nexusdUnit); st != "active" {
		d.t.Fatalf("nexusd unit %s after start", st)
	}
}

func (d *scriptsd) kill9() {
	d.t.Helper()
	pid, err := strconv.Atoi(showUnit(d.t, nexusdUnit, "MainPID")["MainPID"])
	if err != nil || pid == 0 {
		d.t.Fatalf("nexusd has no main process (%v)", err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		d.t.Fatalf("kill -9 %d: %v", pid, err)
	}
	waitUntil(d.t, 15*time.Second, "nexusd to stop after kill -9", func() (bool, string) {
		st := activeState(d.t, nexusdUnit)
		return st == "failed" || st == "inactive", st
	})
	d.t.Logf("kill -9 nexusd pid %d", pid)
}

func (d *scriptsd) request(method, path, token, body string) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://"+d.listen+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.tokens[token])
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return d.client.Do(req)
}

func (d *scriptsd) do(method, path, token, body string) (int, []byte) {
	d.t.Helper()
	resp, err := d.request(method, path, token, body)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		d.t.Fatal(err)
	}
	return resp.StatusCode, b
}

func invokeBody(params map[string]string) string {
	if len(params) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(map[string]any{"parameters": params})
	return string(b)
}

func (d *scriptsd) post(id string, params map[string]string) (int, []byte) {
	d.t.Helper()
	return d.do(http.MethodPost, "/v1/providers/scripts/scripts/"+id+"/invoke", "run", invokeBody(params))
}

// invoke starts script id with params and requires 202.
func (d *scriptsd) invoke(id string, params map[string]string) apitypes.Operation {
	d.t.Helper()
	return d.invokeAs("run", id, params)
}

func (d *scriptsd) invokeAs(token, id string, params map[string]string) apitypes.Operation {
	d.t.Helper()
	code, b := d.do(http.MethodPost, "/v1/providers/scripts/scripts/"+id+"/invoke", token, invokeBody(params))
	if code != http.StatusAccepted {
		d.t.Fatalf("invoke %s: %d %s", id, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		d.t.Fatal(err)
	}
	d.t.Logf("invoked %s: operation %s, unit %s", id, op.ID, detach.Unit(op.ID))
	return op
}

func (d *scriptsd) get(id string) apitypes.Operation {
	d.t.Helper()
	code, b := d.do(http.MethodGet, "/v1/operations/"+id, "run", "")
	if code != http.StatusOK {
		d.t.Fatalf("get %s: %d %s", id, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		d.t.Fatal(err)
	}
	return op
}

func (d *scriptsd) waitStatus(id string, want apitypes.OperationStatus) {
	d.t.Helper()
	waitUntil(d.t, 30*time.Second, id+" "+string(want), func() (bool, string) {
		s := d.get(id).Status
		return s == want, string(s)
	})
}

func (d *scriptsd) waitTerminal(id string, timeout time.Duration) (apitypes.Operation, time.Duration) {
	d.t.Helper()
	start := time.Now()
	var op apitypes.Operation
	waitUntil(d.t, timeout, id+" to end", func() (bool, string) {
		op = d.get(id)
		return terminal(op.Status), string(op.Status)
	})
	return op, time.Since(start)
}

// logs is operation id's whole output, frame data concatenated.
func (d *scriptsd) logs(id string) string {
	d.t.Helper()
	code, b := d.do(http.MethodGet, "/v1/operations/"+id+"/logs", "run", "")
	if code != http.StatusOK {
		d.t.Fatalf("logs %s: %d %s", id, code, b)
	}
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			d.t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		out.WriteString(f.D)
	}
	return out.String()
}

func (d *scriptsd) cancel(token, id string) (int, []byte) {
	d.t.Helper()
	return d.do(http.MethodPost, "/v1/operations/"+id+"/cancel", token, "")
}

func (d *scriptsd) opDir(id string) string { return filepath.Join(d.stateDir, "ops", id) }

// result decodes a succeeded operation's result.
func result(t *testing.T, op apitypes.Operation) spec.InvokeResult {
	t.Helper()
	var r spec.InvokeResult
	if err := json.Unmarshal(op.Result, &r); err != nil {
		t.Fatalf("result %s: %v", op.Result, err)
	}
	return r
}

func extension(op apitypes.Operation, key string) any {
	if op.Error == nil {
		return nil
	}
	return op.Error.Extensions[key]
}

// releaseFile lets a gate script go on.
func releaseFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// gateScript prints its PID, waits for release to exist, and exits code.
func gateScript(release string, code int) string {
	return fmt.Sprintf("#!/bin/sh\necho \"gate: pid $$\"\nwhile [ ! -e %s ]; do sleep 0.1; done\necho \"gate: released\"\nexit %d\n",
		release, code)
}

// allOpUnits lists every operation unit systemd has loaded.
func allOpUnits(t *testing.T) []string {
	t.Helper()
	out := mustSh(t, detach.Systemctl, "list-units", "--all", "--no-legend", "--plain", detach.UnitPrefix+"*")
	var units []string
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) > 0 {
			units = append(units, f[0])
		}
	}
	return units
}

// opUnits returns which of the operations' units systemd has active.
func opUnits(t *testing.T, ids []string) []string {
	t.Helper()
	out := mustSh(t, detach.Systemctl, "list-units", "--no-legend", "--plain", "--state=active", detach.UnitPrefix+"*")
	var active []string
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		for _, id := range ids {
			if f[0] == detach.Unit(id) {
				active = append(active, id)
			}
		}
	}
	return active
}

// TC-95: ok.sh is Succeeded with exit code 0, a duration and all its
// output; fail.sh is Failed script-failed with exit code 3; a script that
// prints "error" and exits 0 is Succeeded, because Nexus does not interpret
// output (A-6).
func TestScriptOutcomes(t *testing.T) {
	dir := testDir(t, "tc95")
	ok := installFixture(t, dir, "ok.sh")
	fail := installFixture(t, dir, "fail.sh")
	liar := installScript(t, filepath.Join(dir, "liar.sh"), "#!/bin/sh\necho \"error: the deploy failed\"\nexit 0\n")
	d := newScriptsd(t, "tc95", catalogYAML(
		entry{id: "ok", path: ok, runAs: "deploy"},
		entry{id: "fail", path: fail, runAs: "deploy"},
		entry{id: "liar", path: liar, runAs: "deploy"},
	))

	op, _ := d.waitTerminal(d.invoke("ok", nil).ID, time.Minute)
	t.Logf("ok: %s result %s", op.Status, op.Result)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("ok.sh: %s %v", op.Status, op.Error)
	}
	// TC-95 requires a duration to be present, not positive: run.go rounds to
	// the millisecond, so a sub-0.5 ms run reports 0. Check the raw key so a
	// missing duration still fails, and reject a negative one.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(op.Result, &raw); err != nil {
		t.Fatalf("ok.sh result %s: %v", op.Result, err)
	}
	if _, has := raw["durationSeconds"]; !has {
		t.Errorf("ok.sh result %s lacks durationSeconds", op.Result)
	}
	if r := result(t, op); r.ExitCode != 0 || r.Signal != "" || r.DurationSeconds < 0 {
		t.Errorf("ok.sh result %+v, want exit 0, no signal and a non-negative durationSeconds", r)
	}
	out := d.logs(op.ID)
	for _, line := range []string{"ok: starting\n", "ok: step 1 of 2\n", "ok: step 2 of 2\n", "ok: done\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("ok.sh output lacks %q: %q", line, out)
		}
	}

	op, _ = d.waitTerminal(d.invoke("fail", nil).ID, time.Minute)
	t.Logf("fail: %s error %v %v", op.Status, op.Error, extension(op, "exitCode"))
	if op.Status != apitypes.StatusFailed || problemSlug(op) != string(problem.ScriptFailed) ||
		extension(op, "exitCode") != 3.0 || extension(op, "durationSeconds") == nil {
		t.Fatalf("fail.sh: %s %+v, want Failed script-failed with exitCode 3", op.Status, op.Error)
	}
	if out := d.logs(op.ID); !strings.Contains(out, "fail: something went wrong\n") {
		t.Errorf("fail.sh output %q", out)
	}

	op, _ = d.waitTerminal(d.invoke("liar", nil).ID, time.Minute)
	t.Logf("liar: %s result %s", op.Status, op.Result)
	if op.Status != apitypes.StatusSucceeded || result(t, op).ExitCode != 0 {
		t.Fatalf("exit 0 after printing error: %s %v, want Succeeded", op.Status, op.Error)
	}
}

// TC-96: a 2 s timeout ends Failed script-timeout with the signal and the
// duration recorded, and the unit and its processes are gone.
func TestScriptTimeout(t *testing.T) {
	dir := testDir(t, "tc96")
	m := markers(t, "tc96", "root")
	path := installScript(t, filepath.Join(dir, "slow.sh"),
		fmt.Sprintf("#!/bin/sh\necho $$ > %s/pid\nsleep 300\n", m))
	d := newScriptsd(t, "tc96", catalogYAML(entry{id: "slow", path: path,
		extra: "        timeout: 2s\n        terminationGrace: 2s\n"}))

	op := d.invoke("slow", nil)
	pid := waitPIDFile(t, filepath.Join(m, "pid"))
	end, took := d.waitTerminal(op.ID, time.Minute)
	t.Logf("slow: %s %+v after %v", end.Status, end.Error, took)
	if end.Status != apitypes.StatusFailed || problemSlug(end) != string(problem.ScriptTimeout) ||
		extension(end, "signal") != "SIGTERM" || extension(end, "durationSeconds") == nil {
		t.Fatalf("timeout: %s %+v, want Failed script-timeout with SIGTERM and a duration", end.Status, end.Error)
	}
	if d, ok := extension(end, "durationSeconds").(float64); !ok || d < 2 || d > 10 {
		t.Errorf("durationSeconds %v, want about 2", extension(end, "durationSeconds"))
	}
	if pidAlive(pid) {
		t.Errorf("script pid %d still alive", pid)
	}
	waitUntil(t, 30*time.Second, "the unit to be collected", func() (bool, string) {
		u := showUnit(t, detach.Unit(op.ID), "LoadState", "ActiveState")
		return u["LoadState"] == "not-found", fmt.Sprint(u)
	})
}

// TC-97: the environment is exactly HOME, USER, LOGNAME and SHELL from the
// passwd entry, the default PATH, TERM=dumb, LANG=C.UTF-8, and
// XDG_RUNTIME_DIR only if /run/user/<uid> exists. Nothing is inherited,
// profiles are not sourced, and catalog env overrides win.
func TestScriptEnvironmentIsExact(t *testing.T) {
	ensureUsers(t)
	dir := testDir(t, "tc97")
	path := installFixture(t, dir, "env-report.sh")
	// Profiles that would show if they were sourced.
	for _, f := range []string{"/home/deploy/.profile", "/home/deploy/.bashrc", "/home/deploy/.bash_profile"} {
		if err := os.WriteFile(f, []byte("export NXTEST_PROFILE_SOURCED=1\nexport PATH=\"$HOME/.local/bin:$PATH\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustSh(t, "chown", "deploy:deploy", f)
	}
	passwd := strings.Split(strings.TrimSpace(mustSh(t, "getent", "passwd", "deploy")), ":")
	uid := passwd[2]
	xdg := "/run/user/" + uid

	d := newScriptsd(t, "tc97", catalogYAML(
		entry{id: "env", path: path, runAs: "deploy"},
		entry{id: "env-override", path: path, runAs: "deploy",
			extra: "        env: {PATH: /home/deploy/.local/bin:/usr/bin:/bin, TERM: xterm, DEPLOY_ENV: production}\n"},
	))
	report := func(id string) map[string]string {
		t.Helper()
		op, _ := d.waitTerminal(d.invoke(id, nil).ID, time.Minute)
		if op.Status != apitypes.StatusSucceeded {
			t.Fatalf("%s: %s %v", id, op.Status, op.Error)
		}
		env := map[string]string{}
		out := d.logs(op.ID)
		t.Logf("%s environment:\n%s", id, out)
		for line := range strings.Lines(out) {
			k, v, _ := strings.Cut(strings.TrimRight(line, "\n"), "=")
			env[k] = v
		}
		return env
	}
	base := map[string]string{
		"HOME": passwd[5], "USER": "deploy", "LOGNAME": "deploy", "SHELL": passwd[6],
		"PATH": scripts.DefaultPath, "TERM": "dumb", "LANG": "C.UTF-8",
	}
	for _, withXDG := range []bool{false, true} {
		if withXDG {
			mustSh(t, "install", "-d", "-o", "deploy", "-g", "deploy", "-m", "0700", xdg)
			t.Cleanup(func() { _ = os.RemoveAll(xdg) })
		} else if err := os.RemoveAll(xdg); err != nil {
			t.Fatal(err)
		}
		want := maps(base)
		if withXDG {
			want["XDG_RUNTIME_DIR"] = xdg
		}
		if got := report("env"); !equalMaps(got, want) {
			t.Errorf("XDG dir present=%v: environment\n got %v\nwant %v", withXDG, got, want)
		}
	}
	want := maps(base)
	want["XDG_RUNTIME_DIR"] = xdg
	want["PATH"] = "/home/deploy/.local/bin:/usr/bin:/bin"
	want["TERM"] = "xterm"
	want["DEPLOY_ENV"] = "production"
	got := report("env-override")
	if !equalMaps(got, want) {
		t.Errorf("catalog overrides: environment\n got %v\nwant %v", got, want)
	}
	for _, k := range []string{"INVOCATION_ID", "JOURNAL_STREAM", "NOTIFY_SOCKET", "NXTEST_PROFILE_SOURCED", "SSH_AUTH_SOCK", "MAINPID"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s was inherited", k)
		}
	}
}

func maps(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TC-98: under real systemd no stream is a terminal, there is no
// controlling terminal, and a read of stdin gets EOF at once.
func TestScriptHasNoTTY(t *testing.T) {
	dir := testDir(t, "tc98")
	path := installFixture(t, dir, "tty-check.sh")
	d := newScriptsd(t, "tc98", catalogYAML(entry{id: "tty", path: path, runAs: "deploy",
		extra: "        timeout: 30s\n"}))
	op, took := d.waitTerminal(d.invoke("tty", nil).ID, time.Minute)
	out := d.logs(op.ID)
	t.Logf("tty-check in %v: %s\n%s", took, op.Status, out)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("tty-check: %s %v", op.Status, op.Error)
	}
	for _, line := range []string{"tty: not a tty\n", "fd0: not a terminal\n", "fd1: not a terminal\n",
		"fd2: not a terminal\n", "ctty: none\n", "read: EOF\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q", line)
		}
	}
	if r := result(t, op); r.DurationSeconds > 5 {
		t.Errorf("tty-check took %vs: the read did not return EOF at once", r.DurationSeconds)
	}
}

// TC-99: the script runs as deploy with its supplementary docker group, so
// docker ps works, and with HOME-based git configuration and credentials,
// so git ls-remote through a URL rewrite and a credential helper work,
// with no SSH_AUTH_SOCK.
func TestScriptIdentityAndCredentials(t *testing.T) {
	ensureUsers(t)
	dir := testDir(t, "tc99")
	// A local bare repository with one commit, owned by deploy.
	repo := "/srv/nxtest/repo.git"
	if err := os.RemoveAll("/srv/nxtest"); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	mustSh(t, "git", "init", "--quiet", "--bare", repo)
	mustSh(t, "git", "-C", work, "init", "--quiet")
	mustSh(t, "git", "-C", work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "--allow-empty", "-m", "c1")
	mustSh(t, "git", "-C", work, "push", "--quiet", repo, "HEAD:refs/heads/main")
	head := strings.TrimSpace(mustSh(t, "git", "-C", work, "rev-parse", "HEAD"))
	mustSh(t, "chown", "-R", "deploy:deploy", "/srv/nxtest")
	gitconfig := "[url \"file://" + repo + "\"]\n\tinsteadOf = https://git.nxtest.invalid/repo.git\n[credential]\n\thelper = store\n"
	for f, body := range map[string]string{
		"/home/deploy/.gitconfig":       gitconfig,
		"/home/deploy/.git-credentials": "https://nxuser:nxpass@git.nxtest.invalid\n",
	} {
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mustSh(t, "chown", "deploy:deploy", f)
	}
	path := installScript(t, filepath.Join(dir, "whoami.sh"), "#!/bin/sh\nset -e\necho \"id: $(id)\"\n"+
		"docker ps --format '{{.ID}}' >/dev/null && echo 'docker: ok'\n"+
		"git ls-remote https://git.nxtest.invalid/repo.git\n"+
		"printf 'protocol=https\\nhost=git.nxtest.invalid\\n\\n' | git credential fill\n"+
		"echo \"ssh-agent: ${SSH_AUTH_SOCK:-none}\"\n")
	d := newScriptsd(t, "tc99", catalogYAML(entry{id: "whoami", path: path, runAs: "deploy"}))
	op, _ := d.waitTerminal(d.invoke("whoami", nil).ID, time.Minute)
	out := d.logs(op.ID)
	t.Logf("whoami: %s %v\n%s", op.Status, op.Error, out)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("whoami: %s %v", op.Status, op.Error)
	}
	want := "id: " + strings.TrimSpace(mustSh(t, "id", "deploy")) + "\n"
	if !strings.Contains(out, want) || !strings.Contains(want, "(docker)") {
		t.Errorf("output lacks %q (with the docker group)", want)
	}
	for _, line := range []string{"docker: ok\n", head + "\trefs/heads/main\n", "username=nxuser\n", "password=nxpass\n", "ssh-agent: none\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q", line)
		}
	}
}

// TC-100: concurrency 1 and 2. Starts past the limit are 409 conflict
// naming a blocking operation; exactly the limit run at once, as the unit
// list shows; after they finish, a retry is accepted.
func TestScriptConcurrencyLimit(t *testing.T) {
	dir := testDir(t, "tc100")
	m := markers(t, "tc100", "root")
	one := installScript(t, filepath.Join(dir, "one.sh"), gateScript(filepath.Join(m, "release"), 0))
	two := installScript(t, filepath.Join(dir, "two.sh"), gateScript(filepath.Join(m, "release"), 0))
	d := newScriptsd(t, "tc100", catalogYAML(
		entry{id: "one", path: one, extra: "        concurrency: 1\n"},
		entry{id: "two", path: two, extra: "        concurrency: 2\n"},
	))
	var started []string
	try := func(id string, want int) {
		t.Helper()
		code, b := d.post(id, nil)
		var body struct {
			ID                  string `json:"id"`
			Type                string `json:"type"`
			BlockingOperationID string `json:"blockingOperationId"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
		t.Logf("start %s: %d %s", id, code, b)
		if code != want {
			t.Fatalf("start %s: %d, want %d: %s", id, code, want, b)
		}
		if code == http.StatusAccepted {
			started = append(started, body.ID)
			return
		}
		if body.Type != problem.TypePrefix+string(problem.Conflict) || !slices.Contains(started, body.BlockingOperationID) {
			t.Fatalf("refusal %s does not name a running operation of %v", b, started)
		}
	}
	try("one", http.StatusAccepted)
	try("one", http.StatusConflict)
	try("two", http.StatusAccepted)
	try("two", http.StatusAccepted)
	try("two", http.StatusConflict)
	// Running is recorded before the unit is launched, so wait for the
	// units themselves.
	waitUntil(t, 30*time.Second, "3 units (1 + 2)", func() (bool, string) {
		active := opUnits(t, started)
		return len(active) == 3, fmt.Sprint(active)
	})
	if active := opUnits(t, started); len(active) != 3 {
		t.Fatalf("units active for %v: %v, want 3 (1 + 2)", started, active)
	}
	releaseFile(t, filepath.Join(m, "release"))
	for _, id := range started {
		if op, _ := d.waitTerminal(id, time.Minute); op.Status != apitypes.StatusSucceeded {
			t.Fatalf("%s: %s %v", id, op.Status, op.Error)
		}
	}
	try("one", http.StatusAccepted)
	try("two", http.StatusAccepted)
	for _, id := range started[3:] {
		d.waitTerminal(id, time.Minute)
	}
}

// TC-101: cancel of a tree that handles TERM ends Canceled promptly with no
// process left; a tree that ignores TERM is killed around the 2 s grace,
// Canceled, with SIGKILL recorded in exit.json.
func TestScriptCancelStopsTreeAndHonorsGrace(t *testing.T) {
	dir := testDir(t, "tc101")
	m := markers(t, "tc101", "root")
	handles := installScript(t, filepath.Join(dir, "handles.sh"), fmt.Sprintf(
		"#!/bin/bash\ntrap 'echo got TERM; exit 143' TERM\nsleep 300 &\necho $! > %[1]s/handles-child\necho $$ > %[1]s/handles\nwait\n", m))
	ignores := installScript(t, filepath.Join(dir, "ignores.sh"), fmt.Sprintf(
		"#!/bin/bash\ntrap '' TERM\nsleep 300 &\necho $! > %[1]s/ignores-child\necho $$ > %[1]s/ignores\nwhile :; do wait; done\n", m))
	d := newScriptsd(t, "tc101", catalogYAML(
		entry{id: "handles", path: handles, extra: "        terminationGrace: 2s\n"},
		entry{id: "ignores", path: ignores, extra: "        terminationGrace: 2s\n"},
	))
	for _, tc := range []struct {
		id       string
		min, max time.Duration
		signal   string
	}{
		{"handles", 0, 2 * time.Second, ""},
		{"ignores", 2 * time.Second, 8 * time.Second, "SIGKILL"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			op := d.invoke(tc.id, nil)
			pids := []int{waitPIDFile(t, filepath.Join(m, tc.id)), waitPIDFile(t, filepath.Join(m, tc.id+"-child"))}
			if code, b := d.cancel("run", op.ID); code != http.StatusOK {
				t.Fatalf("cancel: %d %s", code, b)
			}
			end, took := d.waitTerminal(op.ID, time.Minute)
			x, ok, err := detach.ReadExit(d.opDir(op.ID))
			t.Logf("%s: %s after %v; exit.json %+v (%v %v)", tc.id, end.Status, took, x, ok, err)
			if end.Status != apitypes.StatusCanceled {
				t.Fatalf("%s: %s %v, want Canceled", tc.id, end.Status, end.Error)
			}
			if !ok || !x.Canceled || x.Signal != tc.signal {
				t.Errorf("exit.json %+v, want canceled with signal %q", x, tc.signal)
			}
			if span := x.Ended.Sub(x.Started); took > tc.max+time.Second || x.Ended.Sub(x.Started) < tc.min {
				t.Errorf("took %v (child ran %v), want between %v and %v", took, span, tc.min, tc.max)
			}
			for _, pid := range pids {
				if pidAlive(pid) {
					t.Errorf("pid %d of the tree is still alive", pid)
				}
			}
		})
	}
}

// sleeperImage builds an image whose process is a copy of this test binary
// that only sleeps, so TC-102 starts a real container without a pull.
var sleeper sync.Once

func sleeperImage(t *testing.T) {
	t.Helper()
	sleeper.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		bin, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{Name: "sleeper", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(bin); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("docker", "import", "--change", `ENTRYPOINT ["/sleeper"]`, "--change", "ENV "+sleeperEnv+"=1", "-", sleeperImg)
		cmd.Stdin = &buf
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("docker import: %v\n%s", err, out)
		}
	})
}

// TC-102: a script that starts `sleep 300 &` holding its stdout, starts a
// real container, and exits 0 completes without waiting for the
// background process, which the cgroup cleanup stops; the container keeps
// running. This is also TC-E11 under real systemd.
func TestScriptLeftoversStoppedContainersKept(t *testing.T) {
	ensureUsers(t)
	sleeperImage(t)
	const name = "nxtest-tc102"
	_, _ = sh(time.Minute, "docker", "rm", "-f", name)
	t.Cleanup(func() { _, _ = sh(time.Minute, "docker", "rm", "-f", name) })
	dir := testDir(t, "tc102")
	m := markers(t, "tc102", "deploy")
	path := installScript(t, filepath.Join(dir, "leave.sh"), fmt.Sprintf(
		"#!/bin/sh\nset -e\nsleep 300 &\necho $! > %s/bg\ndocker run -d --name %s %s >/dev/null\necho started\nexit 0\n", m, name, sleeperImg))
	d := newScriptsd(t, "tc102", catalogYAML(entry{id: "leave", path: path, runAs: "deploy"}))

	op := d.invoke("leave", nil)
	end, took := d.waitTerminal(op.ID, time.Minute)
	t.Logf("leave: %s %v after %v; output %q", end.Status, end.Error, took, d.logs(op.ID))
	if end.Status != apitypes.StatusSucceeded {
		t.Fatalf("leave: %s %v", end.Status, end.Error)
	}
	if took > 30*time.Second {
		t.Errorf("took %v: the background process held the operation open", took)
	}
	bg := waitPIDFile(t, filepath.Join(m, "bg"))
	waitUntil(t, 30*time.Second, "the background sleep to be stopped", func() (bool, string) {
		return !pidAlive(bg), "alive"
	})
	running := strings.TrimSpace(mustSh(t, "docker", "inspect", "-f", "{{.State.Running}}", name))
	if running != "true" {
		t.Errorf("container %s running=%s, want true", name, running)
	}
}

// TC-103: lines printed every 200 ms arrive while the script runs, not in
// one burst at the end, and the follow ends when the operation does.
func TestScriptLiveOutput(t *testing.T) {
	dir := testDir(t, "tc103")
	path := installScript(t, filepath.Join(dir, "ticks.sh"),
		"#!/bin/sh\ni=0\nwhile [ $i -lt 25 ]; do echo \"tick $i\"; i=$((i+1)); sleep 0.2; done\n")
	d := newScriptsd(t, "tc103", catalogYAML(entry{id: "ticks", path: path}))
	op := d.invoke("ticks", nil)
	start := time.Now()
	resp, err := d.request(http.MethodGet, "/v1/operations/"+op.ID+"/logs?follow=true", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("follow: %d %s", resp.StatusCode, b)
	}
	var arrivals []time.Duration
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		if strings.HasPrefix(f.D, "tick ") {
			arrivals = append(arrivals, time.Since(start))
		}
	}
	ended := time.Since(start)
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d lines; first at %v, last at %v, follow ended at %v", len(arrivals), arrivals[0], arrivals[len(arrivals)-1], ended)
	if len(arrivals) != 25 {
		t.Fatalf("%d tick lines, want 25", len(arrivals))
	}
	if spread := arrivals[len(arrivals)-1] - arrivals[0]; spread < 3*time.Second {
		t.Errorf("lines arrived within %v: a burst, not live", spread)
	}
	if op := d.get(op.ID); !terminal(op.Status) {
		t.Errorf("follow ended with the operation %s", op.Status)
	}
}

// TC-104: nexusd killed with SIGKILL mid-run; the script runs to the end,
// and after the restart the operation is Failed with exit code 5 and its
// whole output.
func TestScriptSurvivesDaemonKill9(t *testing.T) {
	dir := testDir(t, "tc104")
	m := markers(t, "tc104", "root")
	path := installScript(t, filepath.Join(dir, "five.sh"), fmt.Sprintf(
		"#!/bin/sh\necho before\nwhile [ ! -e %s/release ]; do sleep 0.1; done\necho after\nexit 5\n", m))
	d := newScriptsd(t, "tc104", catalogYAML(entry{id: "five", path: path}))
	op := d.invoke("five", nil)
	d.waitStatus(op.ID, apitypes.StatusRunning)
	waitUntil(t, 30*time.Second, "the first line", func() (bool, string) {
		out := d.logs(op.ID)
		return strings.Contains(out, "before\n"), out
	})
	d.kill9()
	releaseFile(t, filepath.Join(m, "release"))
	waitUntil(t, 30*time.Second, "the script to end with the daemon down", func() (bool, string) {
		return exists(filepath.Join(d.opDir(op.ID), detach.ExitFile)), "no exit.json"
	})
	if st := activeState(t, nexusdUnit); st == "active" {
		t.Fatal("nexusd came back before the script ended")
	}
	d.start()
	end, _ := d.waitTerminal(op.ID, lostBound)
	out := d.logs(op.ID)
	t.Logf("after restart: %s %+v; output %q", end.Status, end.Error, out)
	if end.Status != apitypes.StatusFailed || problemSlug(end) != string(problem.ScriptFailed) || extension(end, "exitCode") != 5.0 {
		t.Fatalf("%s %+v, want Failed script-failed exit code 5", end.Status, end.Error)
	}
	if out != "before\nafter\n" {
		t.Errorf("output %q, want both lines", out)
	}
}

// TC-105: a sensitive parameter and a catalog env value never appear in
// the unit's properties, the script's command line, the operation's
// directory, the audit log, the API, the journal or state.db's raw bytes;
// the script got both, as its hashes show; the persisted input keeps the
// non-sensitive parameter and replaces the sensitive one.
func TestScriptSensitiveValuesStayOut(t *testing.T) {
	const paramSentinel, envSentinel = "SentinelParam5d0c2a", "sentinel-env-8e41b7"
	dir := testDir(t, "tc105")
	m := markers(t, "tc105", "root")
	path := installScript(t, filepath.Join(dir, "secret.sh"), fmt.Sprintf(
		"#!/bin/sh\necho $$ > %[1]s/pid\nprintf %%s \"$API_KEY\" | sha256sum | cut -c1-64\nprintf %%s \"$SECRET_ENV\" | sha256sum | cut -c1-64\n"+
			"echo \"branch=$1\"\nwhile [ ! -e %[1]s/release ]; do sleep 0.1; done\n", m))
	d := newScriptsd(t, "tc105", catalogYAML(entry{id: "secret", path: path,
		extra: "        env: {SECRET_ENV: " + envSentinel + "}\n        parameters:\n" +
			"          - {name: apiKey, type: string, pattern: '^[A-Za-z0-9]+$', sensitive: true, env: API_KEY}\n" +
			"          - {name: branch, type: string, pattern: '^[a-z]+$'}\n"}))
	sentinels := [][]byte{[]byte(paramSentinel), []byte(envSentinel)}
	clean := func(what string, b []byte) {
		t.Helper()
		for _, s := range sentinels {
			if bytes.Contains(b, s) {
				t.Errorf("%s holds the sentinel %q", what, s)
			}
		}
	}

	op := d.invoke("secret", map[string]string{"apiKey": paramSentinel, "branch": "main"})
	pid := waitPIDFile(t, filepath.Join(m, "pid"))
	// Step 1, during the run.
	clean("systemctl show", []byte(mustSh(t, detach.Systemctl, "show", detach.Unit(op.ID))))
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Fatal(err)
	}
	clean("/proc/<pid>/cmdline", cmdline)
	if exists(filepath.Join(d.opDir(op.ID), detach.SpecFile)) {
		t.Errorf("spec.json still exists while the script runs")
	}
	walkClean := func(root string) {
		t.Helper()
		err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			clean(p, b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	walkClean(d.opDir(op.ID))

	releaseFile(t, filepath.Join(m, "release"))
	end, _ := d.waitTerminal(op.ID, time.Minute)
	if end.Status != apitypes.StatusSucceeded {
		t.Fatalf("secret: %s %v", end.Status, end.Error)
	}
	hash := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	out := d.logs(end.ID)
	if want := hash(paramSentinel) + "\n" + hash(envSentinel) + "\nbranch=main\n"; out != want {
		t.Fatalf("output %q, want %q: the script did not get the values", out, want)
	}
	// Step 2, after the run.
	_, body := d.do(http.MethodGet, "/v1/operations/"+end.ID, "run", "")
	clean("GET operation", body)
	walkClean(d.auditDir)
	walkClean(d.opDir(op.ID))
	clean("journal", []byte(mustSh(t, "journalctl", "--no-pager", "-o", "cat", "--since", "-10min")))
	db, err := os.ReadFile(filepath.Join(d.stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	clean("state.db", db)

	// Step 3: the persisted record, read with nexusd stopped (state.db is
	// locked while open).
	lib(t, "stop-nexusd")
	s, err := store.Open(d.stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Get(end.ID)
	if cerr := s.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("persisted input %s, hasSensitive %v", rec.Input, rec.HasSensitive)
	var in spec.InvokeInput
	if err := json.Unmarshal(rec.Input, &in); err != nil {
		t.Fatal(err)
	}
	if in.Parameters["branch"] != "main" || in.Parameters["apiKey"] != "[REDACTED]" || !rec.HasSensitive {
		t.Errorf("persisted input %s hasSensitive %v, want branch kept, apiKey redacted, hasSensitive", rec.Input, rec.HasSensitive)
	}
}

// fr25Attacks is the conformance generator's list (internal/conformance),
// minus NUL and newline, with each expansion aimed at a marker file, and
// the long value cut to the 4096-byte parameter limit.
func fr25Attacks(marker string) []string {
	return []string{
		"; touch " + marker + "-semicolon",
		"$(touch " + marker + "-substitution)",
		"`touch " + marker + "-backticks`",
		"| touch " + marker + "-pipe",
		"&& touch " + marker + "-and",
		"--force",
		"-o",
		"../../etc/passwd",
		"$HOME ${PATH} ~ * ? [a] 'q' \"d\" \\ > " + marker + "-redirect",
		strings.Repeat("A", spec.MaxParameterValueLength),
	}
}

// TC-106: each hostile value of a permissive `^.*$` parameter arrives as
// one argv entry, byte for byte, and no expansion happens.
func TestScriptHostileValuesAreInertArgv(t *testing.T) {
	dir := testDir(t, "tc106")
	m := markers(t, "tc106", "root")
	path := installScript(t, filepath.Join(dir, "argv.sh"), fmt.Sprintf("#!/bin/sh\nprintf '%%s\\0' \"$@\" > %s/argv-$N\n", m))
	d := newScriptsd(t, "tc106", catalogYAML(entry{id: "argv", path: path,
		extra: "        concurrency: 16\n        parameters:\n          - {name: value, type: string, pattern: '^.*$'}\n" +
			"          - {name: n, type: integer, env: N}\n"}))
	marker := filepath.Join(m, "pwned")
	for i, v := range fr25Attacks(marker) {
		op, _ := d.waitTerminal(d.invoke("argv", map[string]string{"value": v, "n": strconv.Itoa(i)}).ID, time.Minute)
		if op.Status != apitypes.StatusSucceeded {
			t.Fatalf("value %d: %s %v", i, op.Status, op.Error)
		}
		got, err := os.ReadFile(filepath.Join(m, "argv-"+strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}
		if want := v + "\x00"; string(got) != want {
			t.Errorf("value %d: argv bytes %.80q, want %.80q", i, got, want)
		}
	}
	matches, err := filepath.Glob(marker + "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > 0 {
		t.Errorf("expansion happened: %v", matches)
	}
}

// TC-208: a run started with scripts:run:deploy-api is canceled only by a
// scope that covers it: scripts:run:other and scripts:read are 403 naming
// scripts:run:deploy-api, and the unit keeps running; reading it needs
// scripts:read; scripts:run cancels it, and the tree gets SIGTERM.
func TestScriptCancelScopeFollowsStartScope(t *testing.T) {
	dir := testDir(t, "tc208")
	m := markers(t, "tc208", "root")
	path := installScript(t, filepath.Join(dir, "deploy-api.sh"), fmt.Sprintf(
		"#!/bin/sh\ntrap 'touch %[1]s/term; exit 143' TERM\necho $$ > %[1]s/pid\nsleep 300 &\nwait\n", m))
	d := newScriptsd(t, "tc208", catalogYAML(entry{id: "deploy-api", path: path}))
	op := d.invokeAs("deployapi", "deploy-api", nil)
	waitPIDFile(t, filepath.Join(m, "pid"))
	forbidden := func(code int, b []byte, scope string) {
		t.Helper()
		var p problem.Error
		if err := json.Unmarshal(b, &p); err != nil || code != http.StatusForbidden || p.Extensions["requiredScope"] != scope {
			t.Fatalf("%d %s, want 403 naming %s", code, b, scope)
		}
	}
	// Step 1.
	for _, tok := range []string{"other", "read"} {
		code, b := d.cancel(tok, op.ID)
		t.Logf("cancel with %s: %d %s", tok, code, b)
		forbidden(code, b, "scripts:run:deploy-api")
	}
	if st := activeState(t, detach.Unit(op.ID)); st != "active" {
		t.Fatalf("unit %s after refused cancels", st)
	}
	// Step 2.
	for _, path := range []string{"/v1/operations/" + op.ID, "/v1/operations/" + op.ID + "/logs"} {
		code, b := d.do(http.MethodGet, path, "deployapi", "")
		forbidden(code, b, spec.ReadScope)
		if code, b := d.do(http.MethodGet, path, "read", ""); code != http.StatusOK {
			t.Fatalf("GET %s with scripts:read: %d %s", path, code, b)
		}
	}
	// Step 3.
	if code, b := d.cancel("runonly", op.ID); code != http.StatusOK {
		t.Fatalf("cancel with scripts:run: %d %s", code, b)
	}
	end, _ := d.waitTerminal(op.ID, time.Minute)
	if end.Status != apitypes.StatusCanceled || !exists(filepath.Join(m, "term")) {
		t.Fatalf("%s, term marker %v: want Canceled after SIGTERM", end.Status, exists(filepath.Join(m, "term")))
	}
}

// TC-212: with a concurrency-1 run in progress and nexusd killed with
// SIGKILL, every start after the restart, from the first accepted
// connection on, is 409 naming the first run, and one unit exists; once
// the run ends, a start is accepted.
func TestScriptRestartAdmitsNoSecondRun(t *testing.T) {
	dir := testDir(t, "tc212")
	m := markers(t, "tc212", "root")
	path := installScript(t, filepath.Join(dir, "once.sh"), gateScript(filepath.Join(m, "release"), 0))
	d := newScriptsd(t, "tc212", catalogYAML(entry{id: "once", path: path, extra: "        concurrency: 1\n"}))
	first := d.invoke("once", nil)
	d.waitStatus(first.ID, apitypes.StatusRunning)
	// Units of earlier tests are not this test's business.
	earlier := map[string]bool{}
	for _, u := range allOpUnits(t) {
		if u != detach.Unit(first.ID) {
			earlier[u] = true
		}
	}
	d.kill9()
	mustSh(t, detach.Systemctl, "start", "--no-block", nexusdUnit)

	deadline := time.Now().Add(lostBound)
	var answers []int
	for len(answers) < 30 {
		if time.Now().After(deadline) {
			t.Fatalf("after %d answers the listener never settled", len(answers))
		}
		c, err := net.DialTimeout("tcp", d.listen, 200*time.Millisecond)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		_ = c.Close()
		resp, err := d.request(http.MethodPost, "/v1/providers/scripts/scripts/once/invoke", "run", "{}")
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		answers = append(answers, resp.StatusCode)
		var p problem.Error
		if resp.StatusCode != http.StatusConflict || json.Unmarshal(b, &p) != nil || p.Extensions["blockingOperationId"] != first.ID {
			t.Fatalf("POST %d after the restart: %d %s, want 409 naming %s", len(answers), resp.StatusCode, b, first.ID)
		}
		var units []string
		for _, u := range allOpUnits(t) {
			if !earlier[u] {
				units = append(units, u)
			}
		}
		if len(units) != 1 || units[0] != detach.Unit(first.ID) {
			t.Fatalf("sample %d: op units %v, want only %s", len(answers), units, detach.Unit(first.ID))
		}
	}
	t.Logf("%d starts after the restart, all 409 naming %s", len(answers), first.ID)
	releaseFile(t, filepath.Join(m, "release"))
	if end, _ := d.waitTerminal(first.ID, time.Minute); end.Status != apitypes.StatusSucceeded {
		t.Fatalf("first: %s %v", end.Status, end.Error)
	}
	if code, b := d.post("once", nil); code != http.StatusAccepted {
		t.Fatalf("start after the run ended: %d %s", code, b)
	} else {
		var op apitypes.Operation
		_ = json.Unmarshal(b, &op)
		d.waitTerminal(op.ID, time.Minute)
	}
}

// TC-215: with the supervisor stopped by SIGSTOP, so it cannot enforce its
// own 2 s timeout, the watcher ends the run Failed outcome-unknown naming
// the unit, no earlier than timeout + grace + 60 s and within 15 s after;
// the unit is still active and the script got no signal from nexusd.
func TestScriptMaxRuntimeUnderStuckSupervisor(t *testing.T) {
	dir := testDir(t, "tc215")
	m := markers(t, "tc215", "root")
	path := installScript(t, filepath.Join(dir, "stuck.sh"), fmt.Sprintf(
		"#!/bin/sh\ntrap 'touch %[1]s/term; exit 143' TERM\necho $$ > %[1]s/pid\nsleep 300 &\nwait\n", m))
	d := newScriptsd(t, "tc215", catalogYAML(entry{id: "stuck", path: path,
		extra: "        timeout: 2s\n        terminationGrace: 1s\n"}))
	limit := scripts.MaxRuntime(scripts.Script{Timeout: 2 * time.Second, TerminationGrace: time.Second})
	op := d.invoke("stuck", nil)
	unit := detach.Unit(op.ID)
	waitPIDFile(t, filepath.Join(m, "pid"))
	sup, err := strconv.Atoi(showUnit(t, unit, "MainPID")["MainPID"])
	if err != nil || sup == 0 {
		t.Fatalf("no supervisor: %v", err)
	}
	if err := syscall.Kill(sup, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(sup, syscall.SIGCONT)
		_, _ = sh(time.Minute, detach.Systemctl, "kill", "--signal=SIGKILL", unit)
	})
	d.waitStatus(op.ID, apitypes.StatusRunning)
	started := *d.get(op.ID).Started
	end, _ := d.waitTerminal(op.ID, limit+time.Minute)
	elapsed := end.Ended.Sub(started)
	t.Logf("stuck: %s %+v after %v (limit %v)", end.Status, end.Error, elapsed, limit)
	if end.Status != apitypes.StatusFailed || problemSlug(end) != string(problem.OutcomeUnknown) || !strings.Contains(end.Error.Detail, unit) {
		t.Fatalf("%s %+v, want Failed outcome-unknown naming %s", end.Status, end.Error, unit)
	}
	if elapsed < limit || elapsed > limit+15*time.Second {
		t.Errorf("ended %v after start, want within [%v, %v]", elapsed, limit, limit+15*time.Second)
	}
	if st := activeState(t, unit); st != "active" {
		t.Errorf("unit %s, want still active", st)
	}
	if exists(filepath.Join(m, "term")) {
		t.Error("the script got SIGTERM")
	}
}

// TC-223: two Queued records seeded with nexusd stopped: the one with a
// sensitive value ends Failed outcome-unknown "never started" with no
// marker and no unit; the control without one runs and succeeds.
func TestQueuedSensitiveScriptNeverStartsAfterRestart(t *testing.T) {
	dir := testDir(t, "tc223")
	m := markers(t, "tc223", "root")
	sens := installScript(t, filepath.Join(dir, "sens.sh"), fmt.Sprintf("#!/bin/sh\ntouch %s/sens\n", m))
	plain := installScript(t, filepath.Join(dir, "plain.sh"), fmt.Sprintf("#!/bin/sh\ntouch %s/plain\n", m))
	d := newScriptsd(t, "tc223", catalogYAML(
		entry{id: "sens", path: sens, extra: "        parameters:\n          - {name: apiKey, type: string, pattern: '^[A-Za-z0-9]+$', sensitive: true, env: API_KEY}\n"},
		entry{id: "plain", path: plain},
	))
	lib(t, "stop-nexusd")
	s, err := store.Open(d.stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	seed := func(id string, input string, sensitive bool) string {
		t.Helper()
		res, err := s.CreateWithIdempotency(store.OpRecord{
			Action: spec.Invoke.Op().ID, Provider: spec.Name, Target: id, TokenName: "run",
			Input: json.RawMessage(input), HasSensitive: sensitive, Exec: store.ExecDetached, StartScope: spec.RunScope,
			Lane: store.Lane{Key: scripts.LaneKey(id), Policy: "reject", Limit: 1},
		}, store.IdempotencyKey{})
		if err != nil {
			t.Fatal(err)
		}
		return res.ID
	}
	sensID := seed("sens", `{"name":"sens","parameters":{"apiKey":"[REDACTED]"}}`, true)
	plainID := seed("plain", `{"name":"plain"}`, false)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	d.start()
	sensEnd, _ := d.waitTerminal(sensID, lostBound)
	plainEnd, _ := d.waitTerminal(plainID, lostBound)
	t.Logf("sens: %s %+v; plain: %s %v", sensEnd.Status, sensEnd.Error, plainEnd.Status, plainEnd.Error)
	if sensEnd.Status != apitypes.StatusFailed || problemSlug(sensEnd) != string(problem.OutcomeUnknown) ||
		!strings.Contains(sensEnd.Error.Detail, "never started") {
		t.Errorf("sensitive: %s %+v, want Failed outcome-unknown never started", sensEnd.Status, sensEnd.Error)
	}
	if exists(filepath.Join(m, "sens")) {
		t.Error("the sensitive script ran")
	}
	if out := mustSh(t, "journalctl", "--no-pager", "-o", "cat", "-u", detach.Unit(sensID)); strings.TrimSpace(out) != "" && !strings.Contains(out, "No entries") {
		t.Errorf("the sensitive run's unit has journal entries:\n%s", out)
	}
	if plainEnd.Status != apitypes.StatusSucceeded || !exists(filepath.Join(m, "plain")) {
		t.Errorf("control: %s %v, marker %v; want Succeeded and the marker", plainEnd.Status, plainEnd.Error, exists(filepath.Join(m, "plain")))
	}
}

// TC-ERR27: the integrity matrix's ownership rows as real root with real
// users: each failing row is 422 script-integrity-failed naming the check
// and the path, with no unit; each control starts and succeeds. These are
// the rows T0 could only stage through the lookup seam (5.2).
func TestScriptIntegrityOwnershipAsRoot(t *testing.T) {
	ensureUsers(t)
	base := "/opt/nxi"
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	mkdir := func(p, owner string, mode os.FileMode) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		mustSh(t, "chown", owner, p)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	put := func(p, owner string, mode os.FileMode) string {
		t.Helper()
		installScript(t, p, "#!/bin/sh\necho ran\n")
		mustSh(t, "chown", owner, p)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkdir(base, "root:root", 0o755)
	// S-6 control: deploy's own 0775 directory, made by deploy under umask
	// 0002, before other joins group deploy.
	mkdir(base+"/s6", "deploy:deploy", 0o755)
	mustSh(t, "su", "-", "deploy", "-c", "umask 0002 && mkdir "+base+"/s6/made")
	rows := []struct {
		id, path, check, failing string // failing is the path named; "" for a control
	}{
		{"script-other", put(mkdir(base+"/a", "root:root", 0o755)+"/s.sh", "other:other", 0o755), scripts.CheckScriptOwner, base + "/a/s.sh"},
		{"parent-other", put(mkdir(base+"/b", "other:other", 0o755)+"/s.sh", "root:root", 0o755), scripts.CheckDirectoryOwner, base + "/b"},
		{"ancestor-other", put(mkdir(mkdir(base+"/c", "other:other", 0o755)+"/sub", "root:root", 0o755)+"/s.sh", "root:root", 0o755),
			scripts.CheckDirectoryOwner, base + "/c"},
		{"group-shared", put(mkdir(base+"/d", "deploy:deploy", 0o775)+"/s.sh", "root:root", 0o755), scripts.CheckDirectoryGroupWritable, base + "/d"},
		{"umask-0002", "", scripts.CheckScriptWritable, base + "/e/s.sh"},
		{"root-script", put(mkdir(base+"/f", "root:root", 0o755)+"/s.sh", "root:root", 0o755), "", ""},
		{"deploy-script", put(mkdir(base+"/g", "root:root", 0o755)+"/s.sh", "deploy:deploy", 0o755), "", ""},
		{"deploy-parent", put(mkdir(base+"/h", "deploy:deploy", 0o755)+"/s.sh", "root:root", 0o755), "", ""},
		{"s6-private-group", put(base+"/s6/made/s.sh", "root:root", 0o755), "", ""},
	}
	// Script mode 0775 as created under umask 0002.
	mkdir(base+"/e", "root:root", 0o755)
	mustSh(t, "sh", "-c", "umask 0002 && printf '#!/bin/sh\\necho ran\\n' > "+base+"/e/s.sh && chmod a+x "+base+"/e/s.sh")
	rows[4].path = base + "/e/s.sh"
	var es []entry
	for _, r := range rows {
		es = append(es, entry{id: r.id, path: r.path, runAs: "deploy", dir: "/"})
	}
	d := newScriptsd(t, "err27", catalogYAML(es...))
	// The S-6 control runs while group deploy has no other member.
	_, _ = sh(time.Minute, "gpasswd", "--delete", "other", "deploy") // Not a member: exit 3
	if g := mustSh(t, "getent", "group", "deploy"); strings.Contains(g, "other") {
		t.Fatalf("group deploy still lists other: %s", g)
	}
	op, _ := d.waitTerminal(d.invoke("s6-private-group", nil).ID, time.Minute)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("s6-private-group control: %s %v", op.Status, op.Error)
	}
	mustSh(t, "usermod", "--append", "--groups", "deploy", "other")
	t.Cleanup(func() { _, _ = sh(time.Minute, "gpasswd", "--delete", "other", "deploy") })

	operations := func() int {
		t.Helper()
		code, b := d.do(http.MethodGet, "/v1/operations?limit=1000", "run", "")
		var l apitypes.List[apitypes.Operation]
		if err := json.Unmarshal(b, &l); err != nil || code != http.StatusOK {
			t.Fatalf("operations: %d %s", code, b)
		}
		return len(l.Items)
	}
	for _, r := range rows {
		if r.id == "s6-private-group" {
			continue
		}
		t.Run(r.id, func(t *testing.T) {
			before := operations()
			code, b := d.post(r.id, nil)
			t.Logf("%s: %d %s", r.id, code, b)
			if r.check == "" {
				if code != http.StatusAccepted {
					t.Fatalf("control %s: %d %s", r.id, code, b)
				}
				var op apitypes.Operation
				_ = json.Unmarshal(b, &op)
				if end, _ := d.waitTerminal(op.ID, time.Minute); end.Status != apitypes.StatusSucceeded || d.logs(op.ID) != "ran\n" {
					t.Fatalf("control %s: %s %v", r.id, end.Status, end.Error)
				}
				return
			}
			var p problem.Error
			if err := json.Unmarshal(b, &p); err != nil || code != http.StatusUnprocessableEntity ||
				p.Slug() != problem.ScriptIntegrityFailed || p.Extensions["check"] != r.check || p.Extensions["path"] != r.failing {
				t.Fatalf("%d %s, want 422 script-integrity-failed check %s path %s", code, b, r.check, r.failing)
			}
			// No operation, so no unit.
			if after := operations(); after != before {
				t.Errorf("operations %d after the refusal, want %d", after, before)
			}
		})
	}
}

// TC-ERR28: a run-as user that does not exist is refused naming it, and a
// missing working directory ends Failed naming it; neither runs anything,
// as root or otherwise.
func TestScriptUnresolvableRunAsNeverRunsAsRoot(t *testing.T) {
	dir := testDir(t, "err28")
	m := markers(t, "err28", "root")
	path := installScript(t, filepath.Join(dir, "who.sh"), fmt.Sprintf("#!/bin/sh\nid -u > %s/ran-$(id -u)\n", m))
	d := newScriptsd(t, "err28", catalogYAML(
		entry{id: "nouser", path: path, runAs: "nosuchuser"},
		entry{id: "nodir", path: path, runAs: "deploy", dir: "/opt/nxtest/err28/missing"},
	))
	code, b := d.post("nouser", nil)
	t.Logf("nouser: %d %s", code, b)
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil || code != http.StatusUnprocessableEntity ||
		p.Extensions["check"] != scripts.CheckRunAsUser || !strings.Contains(p.Detail, "nosuchuser") {
		t.Fatalf("nouser: %d %s, want 422 run-as-user naming nosuchuser", code, b)
	}
	op, _ := d.waitTerminal(d.invoke("nodir", nil).ID, time.Minute)
	t.Logf("nodir: %s %+v", op.Status, op.Error)
	if op.Status != apitypes.StatusFailed || op.Error == nil || !strings.Contains(op.Error.Detail, "/opt/nxtest/err28/missing") {
		t.Fatalf("nodir: %s %+v, want Failed naming the directory", op.Status, op.Error)
	}
	if entries, _ := os.ReadDir(m); len(entries) > 0 {
		t.Errorf("a script ran: %v", entries)
	}
}
