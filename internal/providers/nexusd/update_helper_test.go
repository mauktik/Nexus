package nexusd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// The update helper rig's unit files: U1 is installed, U2 is in the
// package. B1 and B2 are the fake binaries of runningVersion and
// expectedVersion.
const (
	uhU1 = "[Service]\nExecStart=/usr/local/bin/nexusd serve # U1\n"
	uhU2 = "[Service]\nExecStart=/usr/local/bin/nexusd serve # U2\n"
	// uhImage marks /proc/100/exe, so a backup copied from it differs from
	// one copied from Paths.Binary.
	uhImage = "# the image PID 100 started from\n"
)

// uhHealth is the rig's health target: the defaults of a 30 s
// healthTimeout. F-SYS answers Healthz itself.
var uhHealth = healthTarget{URL: "https://127.0.0.1:7443", Timeout: 30 * time.Second, Stability: stabilityWindow,
	Poll: pollInterval}

// hookedSystem is F-SYS with a test's fault and observation points around
// install.sh, Restart and ResetFailed. Unset hooks are F-SYS's behaviour.
type hookedSystem struct {
	*fakeSystem
	beforeInstall func(ctx context.Context)
	afterInstall  func(ctx context.Context)
	restart       func(ctx context.Context) error
	// skipResetFailed is a planted helper bug: its reset-failed never
	// reaches systemd.
	skipResetFailed bool
}

func (s *hookedSystem) RunInstall(ctx context.Context, script, dir string, out io.Writer) (int, error) {
	if s.beforeInstall != nil {
		s.beforeInstall(ctx)
	}
	code, err := s.fakeSystem.RunInstall(ctx, script, dir, out)
	if s.afterInstall != nil {
		s.afterInstall(ctx)
	}
	return code, err
}

func (s *hookedSystem) Restart(ctx context.Context) error {
	if s.restart != nil {
		return s.restart(ctx)
	}
	return s.fakeSystem.Restart(ctx)
}

func (s *hookedSystem) ResetFailed(ctx context.Context) error {
	if s.skipResetFailed {
		return nil
	}
	return s.fakeSystem.ResetFailed(ctx)
}

// updateHelperSetup configures newUpdateHelperRig; zero fields are
// TC-SM-88's setup.
type updateHelperSetup struct {
	running, expected string // v9.9.1 and v9.9.2
	procVersion       string // What PID 100 runs; running when empty
	pkgBehaviour      string // The package binary's behaviour; ok when empty
	inactive          bool   // The unit is inactive
	opsDir            string // The ops directory; a temp one when empty
}

// updateHelperRig is the update helper against F-SYS (TC-SM-88's setup):
// PID 100 runs B1 from Paths.Binary, UnitFile holds U1, and
// ops/<id>/pkg/<top> holds B2 and U2.
type updateHelperRig struct {
	f           *fakeSystem
	sys         *hookedSystem
	id, dir     string // ops/<id>
	pkg         string // ops/<id>/pkg
	procDir     string // Stands for /proc
	procExe     string // <procDir>/100/exe
	savedUnit   string
	in          helperInput
	callTimeout time.Duration
}

func newUpdateHelperRig(t *testing.T, s updateHelperSetup) *updateHelperRig {
	t.Helper()
	if s.running == "" {
		s.running = "v9.9.1"
	}
	if s.expected == "" {
		s.expected = "v9.9.2"
	}
	if s.procVersion == "" {
		s.procVersion = s.running
	}
	if s.pkgBehaviour == "" {
		s.pkgBehaviour = "ok"
	}
	if s.opsDir == "" {
		s.opsDir = t.TempDir()
	}
	f := newFakeSystem(t, 100)
	if s.inactive {
		writeFakeBinary(t, f.paths.Binary, s.procVersion, "ok")
	} else {
		f.bootActive(t, s.procVersion, "ok")
	}
	writeFile(t, f.paths.UnitFile, uhU1)
	procDir := t.TempDir()
	procExe := filepath.Join(procDir, "100", "exe")
	writeFile(t, procExe, readFile(t, f.paths.Binary)+uhImage)
	if err := os.Chmod(procExe, 0o755); err != nil {
		t.Fatal(err)
	}

	const id = "op-uh"
	dir := filepath.Join(s.opsDir, id)
	pkg := filepath.Join(dir, pkgDirName)
	top := filepath.Join(pkg, updateTop(s.expected))
	writeFakeBinary(t, filepath.Join(top, "nexusd"), s.expected, s.pkgBehaviour)
	writeFile(t, filepath.Join(top, "nexusd.service"), uhU2)
	writeFile(t, filepath.Join(top, "install.sh"), "#!/bin/bash\nexit 0\n")
	saved := savedUnitPath(t.TempDir())
	return &updateHelperRig{
		f: f, sys: &hookedSystem{fakeSystem: f}, id: id, dir: dir, pkg: pkg, procDir: procDir, procExe: procExe,
		savedUnit: saved,
		in: helperInput{
			Mode:            modeUpdate,
			RunningVersion:  s.running,
			ExpectedVersion: s.expected,
			PackageDir:      top,
			Health:          uhHealth,
			InstallTimeout:  installTimeout,
			Paths:           f.paths,
			SavedUnit:       saved,
		},
	}
}

// b1 is the backup the helper should make: the image of PID 100.
func (r *updateHelperRig) b1(t *testing.T) string { return readFile(t, r.procExe) }

// env is the helper environment over the rig's system.
func (r *updateHelperRig) env() helperEnv {
	return helperEnv{
		newSystem:   func(helperInput) system { return r.sys },
		mode:        modeOf,
		procDir:     r.procDir,
		callTimeout: r.callTimeout,
	}
}

// run writes input.json and runs the helper seam once.
func (r *updateHelperRig) run(t *testing.T) (code int, stdout, stderr string) {
	t.Helper()
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(r.dir, inputFileName, r.in, nil); err != nil {
		t.Fatal(err)
	}
	return r.again(t)
}

// again runs the helper seam on the rig's directory as it is.
func (r *updateHelperRig) again(t *testing.T) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = runHelper([]string{"--dir", r.dir}, &out, &errOut, r.env())
	return code, out.String(), errOut.String()
}

// result reads result.json, which must exist.
func (r *updateHelperRig) result(t *testing.T) helperResult {
	t.Helper()
	res, ok, err := readResult(r.dir)
	if err != nil || !ok {
		t.Fatalf("result.json: ok %t, %v", ok, err)
	}
	return res
}

// runOK runs the helper, which must exit 0, and returns its result.
func (r *updateHelperRig) runOK(t *testing.T) (helperResult, string) {
	t.Helper()
	code, stdout, stderr := r.run(t)
	if code != 0 {
		t.Fatalf("helper exit %d, want 0; stderr: %s", code, stderr)
	}
	return r.result(t), stdout
}

// wantPackageGone fails unless ops/<id>/pkg is gone and result.json is
// there.
func (r *updateHelperRig) wantPackageGone(t *testing.T) {
	t.Helper()
	if pkgExists(r.pkg) {
		t.Errorf("%s survives the helper", r.pkg)
	}
	if !pkgExists(filepath.Join(r.dir, resultFileName)) {
		t.Error("result.json is missing")
	}
}

// wantFailure fails unless res is slug with rolledBack and step.
func wantFailure(t *testing.T, res helperResult, slug problem.Slug, rolledBack bool, step string) {
	t.Helper()
	if res.Error == nil {
		t.Fatalf("result %+v, want %s", res, slug)
	}
	if res.Error.Slug != slug || res.RolledBack != rolledBack || res.Error.Step != step {
		t.Errorf("result %s rolledBack %t step %q (%s), want %s rolledBack %t step %q", res.Error.Slug,
			res.RolledBack, res.Error.Step, res.Error.Detail, slug, rolledBack, step)
	}
	if res.PreviousVersion == "" || res.NewVersion == "" {
		t.Errorf("result %+v does not name both versions", res)
	}
}

// indexOf is the index of the first recorded call named name from from
// on, or -1.
func indexOf(f *fakeSystem, from int, name string) int {
	for i := from; i < len(f.calls); i++ {
		if f.calls[i].name == name {
			return i
		}
	}
	return -1
}

// systemctlAfter lists the DaemonReload, ResetFailed and Restart calls
// after call index i.
func systemctlAfter(f *fakeSystem, i int) []string {
	var out []string
	for _, c := range f.calls[i+1:] {
		switch c.name {
		case "DaemonReload", "ResetFailed", "Restart":
			out = append(out, c.name)
		}
	}
	return out
}

// stepNames are the step lines' text after the timestamp.
func stepNames(stdout string) []string {
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
		if _, rest, ok := strings.Cut(line, " "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// wantStepOrder fails unless lines has a line beginning with each prefix,
// in this order; "restart-started" and "healthy" must carry an RFC 3339
// timestamp.
func wantStepOrder(t *testing.T, stdout string, prefixes ...string) {
	t.Helper()
	lines := stepNames(stdout)
	at := 0
	for _, p := range prefixes {
		found := false
		for ; at < len(lines); at++ {
			if !strings.HasPrefix(lines[at], p) {
				continue
			}
			found = true
			if p == "restart-started " || p == "healthy " {
				ts := strings.Fields(strings.TrimPrefix(lines[at], p))
				if len(ts) == 0 {
					t.Errorf("line %q has no timestamp", lines[at])
				} else if _, err := time.Parse(time.RFC3339Nano, ts[0]); err != nil {
					t.Errorf("line %q: %v", lines[at], err)
				}
			}
			break
		}
		if !found {
			t.Errorf("no step %q in order %q:\n%s", p, prefixes, stdout)
			return
		}
	}
}

// TC-SM-88 (WI 5.4; FR-7, FR-9, FR-10, CON-2): Update helper success: backup, install, exactly one restart, health
func TestTCSM88_UpdateHelperSuccessBackupInstallExactly(t *testing.T) {
	r := newUpdateHelperRig(t, updateHelperSetup{})
	b1, b2 := r.b1(t), readFile(t, filepath.Join(r.in.PackageDir, "nexusd"))

	// Steps 1-2.
	res, stdout := r.runOK(t)
	want := helperResult{Changed: true, PreviousVersion: "v9.9.1", NewVersion: "v9.9.2"}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("result %+v, want %+v", res, want)
	}
	if got := readFile(t, r.f.paths.Prev); got != b1 {
		t.Errorf("Prev = %q, want the image of PID 100 %q", got, b1)
	}
	verified, install := indexOf(r.f, 0, "ExeVersion"), indexOf(r.f, 0, "RunInstall")
	if verified < 0 || r.f.calls[verified].arg != r.f.paths.Prev+".tmp" || verified > install {
		t.Errorf("the copy was not verified before install.sh: calls %+v", r.f.calls)
	}
	if got := readFile(t, r.savedUnit); got != uhU1 {
		t.Errorf("saved unit %q, want U1", got)
	}
	if got := readFile(t, r.f.paths.Binary); got != b2 {
		t.Errorf("Binary = %q, want B2 %q", got, b2)
	}
	if got := readFile(t, r.f.paths.UnitFile); got != uhU2 {
		t.Errorf("UnitFile = %q, want U2", got)
	}
	if r.f.starts != 1 {
		t.Errorf("starts = %d, want 1", r.f.starts)
	}
	for _, c := range r.f.callsOf("Restart") {
		if c.arg != "blocking" {
			t.Errorf("Restart %q, want only install.sh's blocking restart", c.arg)
		}
	}
	if r.f.mainPID == 100 || r.f.mainPID == 0 {
		t.Errorf("MainPID %d, want a new process", r.f.mainPID)
	}
	r.wantPackageGone(t)
	if tmp := tmpFiles(t, filepath.Dir(r.f.paths.Binary), filepath.Dir(r.savedUnit)); len(tmp) != 0 {
		t.Errorf("temporary files remain: %v", tmp)
	}
	wantStepOrder(t, stdout, "precheck", "backup", "install", "restart-started ", "healthy ")
}

// TC-SM-89 (WI 5.4; FR-7): When install.sh does not restart, the helper restarts exactly once
func TestTCSM89_WhenInstallShDoesNotRestart(t *testing.T) {
	r := newUpdateHelperRig(t, updateHelperSetup{})
	r.f.install.noRestart = true

	res, stdout := r.runOK(t)
	if res.Error != nil || !res.Changed || res.NewVersion != "v9.9.2" {
		t.Errorf("result %+v, want succeeded with newVersion v9.9.2", res)
	}
	if r.f.starts != 1 {
		t.Errorf("starts = %d, want 1", r.f.starts)
	}
	if got := r.f.callsOf("Restart"); len(got) != 1 || got[0].arg != "--no-block" {
		t.Errorf("Restart calls %+v, want the helper's one --no-block restart", got)
	}
	wantStepOrder(t, stdout, "precheck", "backup", "install", "restart-started ", "healthy ")
}

// TC-SM-90 (WI 5.4; FR-7, ASM-6): The noChange path makes no system calls
func TestTCSM90_TheNoChangePathMakesNoSystem(t *testing.T) {
	r := newUpdateHelperRig(t, updateHelperSetup{expected: "v9.9.1"})
	r.in = helperInput{Mode: modeUpdate, NoChange: true}
	before := hostFiles(t, r.f.paths)

	res, _ := r.runOK(t)
	if !reflect.DeepEqual(res, helperResult{}) {
		t.Errorf("result %+v, want only changed false", res)
	}
	var raw map[string]any
	b, err := os.ReadFile(filepath.Join(r.dir, resultFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["changed"] != false || len(raw) > 2 || (len(raw) == 2 && raw["rolledBack"] != false) {
		t.Errorf("result.json %s, want {changed: false}", b)
	}
	if len(r.f.calls) != 0 {
		t.Errorf("F-SYS calls %+v, want none", r.f.calls)
	}
	if after := hostFiles(t, r.f.paths); !reflect.DeepEqual(after, before) {
		t.Errorf("host files %v, were %v", after, before)
	}
	if pkgExists(r.savedUnit) {
		t.Error("the unit was saved")
	}
	r.wantPackageGone(t)
}

// TC-SM-91 (WI 5.4; FR-7, FR-9): T0 full detached loop: a same-version update through the real supervisor and helper
func TestTCSM91_T0FullDetachedLoopASame(t *testing.T) {
	if _, err := detachtest.Supervisor(); err != nil {
		t.Fatal(err)
	}
	setRunningVersion(t, "v9.9.1")
	k := pkgKey(seedTrusted)
	section := "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: auto\nproviders:\n  docker:\n    enabled: false\n" +
		"  nexusd:\n    enabled: true\n    update:\n      trustedKeys: [\"" + keyB64(k) + "\"]\n"
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeFile(t, path, section)
	// The stack's launcher reports every unit active, so the op ends when
	// the real supervisor writes exit.json; the local launcher never
	// reaches systemd.
	s := newStack(t, stackConfig{
		opts:    Options{Runner: execRunner{}, Executable: detachtest.Supervisor},
		section: path,
		launch:  detachtest.Local().Launch,
	})
	u := &updateStack{stack: s}
	uploadID := u.stage(t, updatePackage(t, "v9.9.1", "v9.9.1"), k)

	// Step 1.
	id := accepted(t, u.update(t, updateOf(uploadID, false)))

	// Step 2.
	var op apitypes.Operation
	deadline := time.Now().Add(30 * time.Second)
	for {
		op = decodeJSON[apitypes.Operation](t, s.do(t, http.MethodGet, "/operations/"+id, s.reader, "", nil).
			want(t, http.StatusOK).body)
		if op.Status.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s still %s after 30s", id, op.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	dir := filepath.Join(s.opsDir, id)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("op %s %v, want Succeeded; output:\n%s", op.Status, op.Error, opOutput(t, dir))
	}
	var out spec.UpdateResult
	if err := json.Unmarshal(op.Result, &out); err != nil || out.Changed {
		t.Errorf("result %s (%v), want changed false", op.Result, err)
	}
	x, ok, err := detach.ReadExit(dir)
	if err != nil || !ok || x.ExitCode != 0 {
		t.Errorf("exit.json %+v ok %t, %v; want code 0", x, ok, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, resultFileName))
	if err != nil {
		t.Fatal(err)
	}
	var res helperResult
	if err := json.Unmarshal(b, &res); err != nil || !reflect.DeepEqual(res, helperResult{}) {
		t.Errorf("result.json %s (%v), want {changed: false}", b, err)
	}
	if u.staged(uploadID) {
		t.Errorf("staging/%s survives", uploadID)
	}
	if pkgExists(filepath.Join(dir, pkgDirName)) {
		t.Error("ops/<id>/pkg survives")
	}
	log := opOutput(t, dir)
	if !strings.Contains(log, "service helper: update") {
		t.Errorf("the op log does not show the helper's update:\n%s", log)
	}
	if strings.Contains(log, "systemctl") {
		t.Errorf("the op log shows systemctl:\n%s", log)
	}
}

// opOutput is the content of every file under dir/output.
func opOutput(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.WalkDir(filepath.Join(dir, detach.OutputDir), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		sb.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

// helperScenario is one row of TC-SM-92: a helper run that has started
// changing the host.
type helperScenario struct {
	name string
	run  func(t *testing.T) (f *fakeSystem, dir string, env helperEnv)
}

// updateScenario adapts an update rig to a helperScenario.
func updateScenario(name string, setup func(t *testing.T) *updateHelperRig) helperScenario {
	return helperScenario{name: name, run: func(t *testing.T) (*fakeSystem, string, helperEnv) {
		r := setup(t)
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSONAtomic(r.dir, inputFileName, r.in, nil); err != nil {
			t.Fatal(err)
		}
		return r.f, r.dir, r.env()
	}}
}

// The update scenarios of TC-SM-88, TC-SM-89 and TC-SM-ERR51 to ERR58.
func scenario88(t *testing.T) *updateHelperRig { return newUpdateHelperRig(t, updateHelperSetup{}) }

func scenario89(t *testing.T) *updateHelperRig {
	r := newUpdateHelperRig(t, updateHelperSetup{})
	r.f.install.noRestart = true
	return r
}

func scenarioERR51(t *testing.T) *updateHelperRig {
	r := newUpdateHelperRig(t, updateHelperSetup{})
	r.f.install.exitAfterRename = 3
	return r
}

// scenarioERR52 installs a binary that exits at start. install.sh's
// blocking restart fails, and while it exits non-zero the unit keeps
// re-spawning until systemd's start limit; reached is what it got to.
// scenarioERR52 has B1 running for longer than StartLimitIntervalSec
// before install.sh, so its boot start is outside the start limit's window,
// as on a real host. It lets the exit-at-start binary re-spawn for a second
// after install.sh, so the unit reaches its start limit (at about 0.55 s),
// and the restore runs within the 10 s window, while the limit still holds.
func scenarioERR52(t *testing.T, reached *unitState) *updateHelperRig {
	r := newUpdateHelperRig(t, updateHelperSetup{pkgBehaviour: "exit-at-start"})
	r.sys.beforeInstall = func(ctx context.Context) {
		if err := r.f.Sleep(ctx, time.Minute); err != nil {
			t.Error(err)
		}
	}
	r.sys.afterInstall = func(ctx context.Context) {
		if err := r.f.Sleep(ctx, time.Second); err != nil {
			t.Error(err)
		}
		if reached != nil {
			*reached, _ = r.f.Show(ctx)
		}
	}
	return r
}

func scenarioERR53(t *testing.T) *updateHelperRig {
	r := newUpdateHelperRig(t, updateHelperSetup{expected: "v9.9.5"})
	r.f.install.installAlt = "v9.9.8"
	return r
}

// scenarioERR54 is ERR51 with nexusd.prev replaced by a binary that exits
// at start once the backup is made, before install.sh runs (the v9.9.7
// analogue, A-44).
func scenarioERR54(t *testing.T) *updateHelperRig {
	r := scenarioERR51(t)
	r.sys.beforeInstall = func(context.Context) {
		writeFakeBinary(t, r.f.paths.Prev, "v9.9.1", "exit-at-start")
	}
	return r
}

// scenarioERR55 is an install.sh that hangs past a 300 ms installTimeout;
// cancelled is set to its context's error once it returned.
func scenarioERR55(t *testing.T, cancelled *error) *updateHelperRig {
	r := newUpdateHelperRig(t, updateHelperSetup{})
	r.in.InstallTimeout = 300 * time.Millisecond
	r.f.install.hang = true
	r.sys.afterInstall = func(ctx context.Context) {
		if cancelled != nil {
			*cancelled = ctx.Err()
		}
	}
	return r
}

func scenarioERR56(t *testing.T) *updateHelperRig {
	return newUpdateHelperRig(t, updateHelperSetup{procVersion: "v9.9.0"})
}

func scenarioERR57(t *testing.T) *updateHelperRig {
	return newUpdateHelperRig(t, updateHelperSetup{inactive: true})
}

// scenarioERR58 is ERR51 with a 200 ms bound on systemctl calls and a
// Restart that blocks until its context ends (5 s at most, so a missing
// bound fails the test instead of hanging it).
func scenarioERR58(t *testing.T) *updateHelperRig {
	r := scenarioERR51(t)
	r.callTimeout = 200 * time.Millisecond
	r.sys.restart = func(ctx context.Context) error {
		r.f.record("Restart", "--no-block (hangs)")
		select {
		case <-ctx.Done():
			return fmt.Errorf("systemctl restart --no-block nexusd.service: %w", ctx.Err())
		case <-time.After(5 * time.Second):
			return errors.New("the helper did not bound Restart")
		}
	}
	return r
}

// TC-SM-92 (WI 5.4; FR-8, FR-9): The helper writes result.json and exits 0 in every path after it starts changing the host
func TestTCSM92_TheHelperWritesResultJsonAnd(t *testing.T) {
	scenarios := []helperScenario{
		updateScenario("TC-SM-88", scenario88),
		updateScenario("TC-SM-89", scenario89),
		updateScenario("TC-SM-ERR51", scenarioERR51),
		updateScenario("TC-SM-ERR52", func(t *testing.T) *updateHelperRig { return scenarioERR52(t, nil) }),
		updateScenario("TC-SM-ERR53", scenarioERR53),
		updateScenario("TC-SM-ERR54", scenarioERR54),
		updateScenario("TC-SM-ERR55", func(t *testing.T) *updateHelperRig { return scenarioERR55(t, nil) }),
		updateScenario("TC-SM-ERR56", scenarioERR56),
		updateScenario("TC-SM-ERR57", scenarioERR57),
		updateScenario("TC-SM-ERR58", scenarioERR58),
		{name: "TC-SM-ERR32", run: func(t *testing.T) (*fakeSystem, string, helperEnv) {
			f := newFakeSystem(t, 100)
			f.bootActive(t, "v9.9.1", "ok")
			writeFakeBinary(t, f.paths.Binary, "v9.9.1", "crash-after=3s")
			dir := t.TempDir()
			if err := writeJSONAtomic(dir, inputFileName, helperInput{Mode: modeRestart, Health: uhHealth,
				Paths: f.paths}, nil); err != nil {
				t.Fatal(err)
			}
			return f, dir, helperEnv{newSystem: func(helperInput) system { return f }, mode: modeOf}
		}},
		{name: "TC-SM-ERR37", run: func(t *testing.T) (*fakeSystem, string, helperEnv) {
			f := newFakeSystem(t, 100)
			f.bootActive(t, "v9.9.2", "ok")
			writeFakeBinary(t, f.paths.Prev, "v9.9.1", "exit-at-start")
			writeFile(t, f.paths.UnitFile, rbU2)
			saved := savedUnitPath(t.TempDir())
			writeFile(t, saved, rbU1)
			dir := t.TempDir()
			if err := writeJSONAtomic(dir, inputFileName, helperInput{Mode: modeRollback, FromVersion: "v9.9.2",
				ToVersion: "v9.9.1", Health: uhHealth, Paths: f.paths, SavedUnit: saved}, nil); err != nil {
				t.Fatal(err)
			}
			return f, dir, helperEnv{newSystem: func(helperInput) system { return f }, mode: modeOf}
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f, dir, env := sc.run(t)

			// Step 1.
			var out, errOut bytes.Buffer
			if code := runHelper([]string{"--dir", dir}, &out, &errOut, env); code != 0 {
				t.Fatalf("exit %d, want 0; stderr: %s", code, errOut.String())
			}
			first, err := os.ReadFile(filepath.Join(dir, resultFileName))
			if err != nil {
				t.Fatal(err)
			}
			var res helperResult
			if err := json.Unmarshal(first, &res); err != nil {
				t.Fatalf("result.json %s: %v", first, err)
			}

			// Step 2.
			calls := len(f.calls)
			out.Reset()
			errOut.Reset()
			if code := runHelper([]string{"--dir", dir}, &out, &errOut, env); code != 1 {
				t.Errorf("second run exit %d, want 1", code)
			}
			if !strings.Contains(errOut.String(), "result.json already exists") {
				t.Errorf("second run stderr %q, want result.json already exists", errOut.String())
			}
			if len(f.calls) != calls {
				t.Errorf("second run made F-SYS calls %+v", f.calls[calls:])
			}
			if again, err := os.ReadFile(filepath.Join(dir, resultFileName)); err != nil || !bytes.Equal(again,
				first) {
				t.Errorf("result.json changed by the second run: %s, was %s (%v)", again, first, err)
			}
		})
	}
}

// finalizeRig is the update pipeline whose ops directory holds a helper
// rig's operation, for Finalize.
func finalizeRig(t *testing.T) (*updateRig, spec.UpdateInput) {
	t.Helper()
	ur := newUpdateRig(t)
	return ur, spec.UpdateInput{UploadID: ur.stage(t, updatePackage(t, "v9.9.2", "v9.9.2"))}
}

// TC-SM-ERR51 (WI 5.4; FR-8, FR-9): install.sh exit 3 is rolled back with step install
func TestTCSMERR51_InstallShExit3IsRolled(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	ur, uin := finalizeRig(t)
	r := newUpdateHelperRig(t, updateHelperSetup{opsDir: ur.opsDir})
	r.f.install.exitAfterRename = 3
	b1 := r.b1(t)

	// Step 1.
	res, _ := r.runOK(t)
	wantFailure(t, res, problem.UpdateFailed, true, "install")
	if res.Error != nil && !strings.Contains(res.Error.Detail, "exit code 3") {
		t.Errorf("detail %q does not name exit code 3", res.Error.Detail)
	}
	if got := readFile(t, r.f.paths.Binary); got != b1 {
		t.Errorf("Binary = %q, want B1 %q", got, b1)
	}
	if got := readFile(t, r.f.paths.Prev); got != b1 {
		t.Errorf("Prev = %q, want it kept as B1 (A-21)", got)
	}
	if got := readFile(t, r.f.paths.UnitFile); got != uhU1 {
		t.Errorf("UnitFile = %q, want U1", got)
	}
	install := indexOf(r.f, 0, "RunInstall")
	if got, want := systemctlAfter(r.f, install), []string{"DaemonReload", "ResetFailed", "Restart"}; !slices.Equal(got,
		want) {
		t.Errorf("calls after install %v, want %v", got, want)
	}
	restart := indexOf(r.f, install, "Restart")
	for _, c := range r.f.calls[restart+1:] {
		if c.name == "ExeVersion" && c.arg == "/proc/100/exe" {
			t.Error("the restore's health check ran -version on the excluded PID 100")
		}
	}
	if p := r.f.procs[r.f.mainPID]; r.f.mainPID == 100 || p == nil || p.bin.version != "v9.9.1" ||
		r.f.active != "active" {
		t.Errorf("MainPID %d %s, want a new active process of v9.9.1", r.f.mainPID, r.f.active)
	}
	if n := len(r.f.callsOf("ExeVersion")); n < 2 {
		t.Errorf("ExeVersion calls %d, want the backup's and the health check's", n)
	}
	r.wantPackageGone(t)

	// Step 2.
	run := provider.NewRun(r.id, nil)
	_, err := ur.funcs.Finalize(t.Context(), uin, detach.Exit{}, run)
	perr := problemOfErr(t, err)
	if perr.Slug() != problem.UpdateFailed || perr.Extensions["rolledBack"] != true ||
		perr.Extensions["step"] != "install" {
		t.Errorf("Finalize = %s %v, want update-failed rolledBack install", perr.Slug(), perr.Extensions)
	}
	writeFile(t, filepath.Join(r.pkg, "leftover"), "x")
	if _, err := ur.funcs.Finalize(t.Context(), uin, detach.Exit{}, run); problemOfErr(t, err).Slug() !=
		problem.UpdateFailed {
		t.Errorf("Finalize with a leftover package = %v", err)
	}
	if pkgExists(r.pkg) {
		t.Error("Finalize left ops/<id>/pkg")
	}
	if _, err := ur.funcs.Finalize(t.Context(), uin, detach.Exit{}, run); problemOfErr(t, err).Slug() !=
		problem.UpdateFailed {
		t.Errorf("Finalize with the package already gone = %v", err)
	}
}

// TC-SM-ERR52 (WI 5.4; FR-8, FR-10): A binary that exits at start is rolled back after reset-failed clears the start limit
func TestTCSMERR52_ABinaryThatExitsAtStart(t *testing.T) {
	var reached unitState
	r := scenarioERR52(t, &reached)
	b1 := r.b1(t)

	res, stdout := r.runOK(t)
	wantFailure(t, res, problem.UpdateFailed, true, "install")
	// The start limit as systemd 259 reaches it (TC-SM-129): failed with the
	// last start's Result=exit-code and NRestarts 5.
	if reached.ActiveState != "failed" || reached.Result != "exit-code" || reached.NRestarts != 5 {
		t.Errorf("after install.sh the unit was %+v, want failed at the start limit, exit-code, NRestarts 5", reached)
	}
	wantStepOrder(t, stdout, "restore: nexusd v9.9.1", "restore: reset-failed nexusd.service",
		"restore: restarting nexusd.service")
	install := indexOf(r.f, 0, "RunInstall")
	reset := indexOf(r.f, install, "ResetFailed")
	restart := -1
	for i := install; i < len(r.f.calls); i++ {
		if c := r.f.calls[i]; c.name == "Restart" && c.arg == "--no-block" {
			restart = i
			break
		}
	}
	if reset < 0 || restart < reset {
		t.Errorf("ResetFailed (%d) does not precede the restore's Restart (%d): %v", reset, restart,
			callNames(r.f, install))
	}
	if p := r.f.procs[r.f.mainPID]; p == nil || p.bin.version != "v9.9.1" || r.f.active != "active" {
		t.Errorf("unit %s MainPID %d, want active on B1", r.f.active, r.f.mainPID)
	}
	if got := readFile(t, r.f.paths.Binary); got != b1 {
		t.Errorf("Binary = %q, want B1", got)
	}

	// The planted variant: without reset-failed, systemd accepts the
	// --no-block restart but refuses its start at the limit, so the unit
	// stays failed and the restore's health check fails on condition 1.
	t.Run("planted: no reset-failed", func(t *testing.T) {
		r := scenarioERR52(t, nil)
		r.sys.skipResetFailed = true
		res, _ := r.runOK(t)
		wantFailure(t, res, problem.UpdateRollbackFailed, false, "restore")
		if res.Error != nil && (!strings.Contains(res.Error.Detail, "condition 1") ||
			!strings.Contains(res.Error.Detail, "ActiveState=failed")) {
			t.Errorf("detail %q, want health condition 1 with ActiveState=failed", res.Error.Detail)
		}
	})
}

// TC-SM-ERR53 (WI 5.4; FR-8, FR-10): A wrong version is rolled back, naming both versions
func TestTCSMERR53_AWrongVersionIsRolledBack(t *testing.T) {
	r := scenarioERR53(t)
	b1 := r.b1(t)

	res, _ := r.runOK(t)
	wantFailure(t, res, problem.UpdateFailed, true, "health")
	if res.Error != nil && (!strings.Contains(res.Error.Detail, "v9.9.8") ||
		!strings.Contains(res.Error.Detail, "v9.9.5")) {
		t.Errorf("detail %q does not name v9.9.8 and v9.9.5", res.Error.Detail)
	}
	if got := readFile(t, r.f.paths.Binary); got != b1 {
		t.Errorf("Binary = %q, want B1", got)
	}
}

// TC-SM-ERR54 (WI 5.4; FR-8, FR-9, FR-18): A restore that fails its health check is update-rollback-failed
func TestTCSMERR54_ARestoreThatFailsItsHealth(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	ur, uin := finalizeRig(t)
	r := newUpdateHelperRig(t, updateHelperSetup{opsDir: ur.opsDir})
	r.f.install.exitAfterRename = 3
	r.sys.beforeInstall = func(context.Context) {
		writeFakeBinary(t, r.f.paths.Prev, "v9.9.1", "exit-at-start")
	}

	res, _ := r.runOK(t)
	wantFailure(t, res, problem.UpdateRollbackFailed, false, "restore")
	for _, s := range []string{"nexusd may be down", "docs/install.md#recovering-from-a-failed-update"} {
		if res.Error != nil && !strings.Contains(res.Error.Detail, s) {
			t.Errorf("detail %q lacks %q", res.Error.Detail, s)
		}
	}
	r.wantPackageGone(t)

	_, err := ur.funcs.Finalize(t.Context(), uin, detach.Exit{}, provider.NewRun(r.id, nil))
	perr := problemOfErr(t, err)
	if perr.Slug() != problem.UpdateRollbackFailed || perr.Extensions["rolledBack"] != false ||
		perr.Detail != res.Error.Detail {
		t.Errorf("Finalize = %s %v %q, want the helper's update-rollback-failed", perr.Slug(), perr.Extensions,
			perr.Detail)
	}
	if pkgExists(r.pkg) {
		t.Error("ops/<id>/pkg exists after Finalize")
	}
}

// TC-SM-ERR55 (WI 5.4; FR-8): An install.sh timeout is rolled back
func TestTCSMERR55_AnInstallShTimeoutIsRolled(t *testing.T) {
	var cancelled error
	r := scenarioERR55(t, &cancelled)

	start := time.Now()
	res, _ := r.runOK(t)
	wantFailure(t, res, problem.UpdateFailed, true, "install")
	if res.Error != nil && !strings.Contains(res.Error.Detail, "timed out") {
		t.Errorf("detail %q does not say timed out", res.Error.Detail)
	}
	if !errors.Is(cancelled, context.DeadlineExceeded) {
		t.Errorf("the install's context ended with %v, want its deadline", cancelled)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the helper took %s", d)
	}
}

// TC-SM-ERR56 (WI 5.4; FR-7, FR-8): A backup version mismatch is update-failed at backup, with nothing changed
func TestTCSMERR56_ABackupVersionMismatchIsUpdate(t *testing.T) {
	r := scenarioERR56(t)
	before := hostFiles(t, r.f.paths)

	res, _ := r.runOK(t)
	wantFailure(t, res, problem.UpdateFailed, false, "backup")
	if res.Error != nil && (!strings.Contains(res.Error.Detail, "v9.9.0") ||
		!strings.Contains(res.Error.Detail, "v9.9.1")) {
		t.Errorf("detail %q does not name both versions", res.Error.Detail)
	}
	if pkgExists(r.f.paths.Prev) || pkgExists(r.f.paths.Prev+".tmp") {
		t.Error("nexusd.prev or nexusd.prev.tmp exists")
	}
	if after := hostFiles(t, r.f.paths); !reflect.DeepEqual(after, before) {
		t.Errorf("host files %v, were %v", after, before)
	}
	if pkgExists(r.savedUnit) {
		t.Error("the unit was saved")
	}
	if n := len(r.f.callsOf("RunInstall")); n != 0 || r.f.starts != 0 {
		t.Errorf("installs %d starts %d, want none", n, r.f.starts)
	}
	r.wantPackageGone(t)
}

// TC-SM-ERR57 (WI 5.4; FR-7): An inactive service at precheck changes nothing
func TestTCSMERR57_AnInactiveServiceAtPrecheckChanges(t *testing.T) {
	r := scenarioERR57(t)
	before := hostFiles(t, r.f.paths)

	res, _ := r.runOK(t)
	wantFailure(t, res, problem.UpdateFailed, false, "precheck")
	if after := hostFiles(t, r.f.paths); !reflect.DeepEqual(after, before) {
		t.Errorf("host files %v, were %v", after, before)
	}
	if pkgExists(r.savedUnit) {
		t.Error("the unit was saved")
	}
	if n := len(r.f.callsOf("RunInstall")); n != 0 || r.f.starts != 0 {
		t.Errorf("installs %d starts %d, want none", n, r.f.starts)
	}
	r.wantPackageGone(t)
}

// TC-SM-ERR58 (WI 5.4; FR-8, FR-5): A systemctl call that hangs is bounded, and a result is still written
func TestTCSMERR58_ASystemctlCallThatHangsIs(t *testing.T) {
	r := scenarioERR58(t)

	start := time.Now()
	res, _ := r.runOK(t)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the helper took %s, want it bounded near 200ms", d)
	}
	wantFailure(t, res, problem.UpdateRollbackFailed, false, "restore")
	if res.Error != nil && (!strings.Contains(res.Error.Detail, "restart") ||
		!strings.Contains(res.Error.Detail, "200ms")) {
		t.Errorf("detail %q does not name the restart timeout", res.Error.Detail)
	}
}

// A package removal that fails is logged and changes neither the result
// nor the exit code (A-50).
func TestUpdateHelperPackageRemovalFailure(t *testing.T) {
	r := newUpdateHelperRig(t, updateHelperSetup{})
	r.in = helperInput{Mode: modeUpdate, NoChange: true}
	if os.Geteuid() == 0 {
		t.Skip("root removes a read-only directory's entries")
	}
	// ops/<id>/pkg made read-only: its entries cannot be removed.
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(r.dir, inputFileName, r.in, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.pkg, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(r.pkg, 0o700); err != nil {
			t.Error(err)
		}
	})
	code, _, stderr := r.again(t)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr %s", code, stderr)
	}
	if res := r.result(t); !reflect.DeepEqual(res, helperResult{}) {
		t.Errorf("result %+v, want {changed: false}", res)
	}
	if !strings.Contains(stderr, pkgDirName) {
		t.Errorf("stderr %q does not log the removal failure", stderr)
	}
}
