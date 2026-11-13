package nexusd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/version"
)

// The rollback rig's file contents: B2 is installed and running, B1 is the
// rollback binary, U2 the installed unit and U1 its saved copy.
const (
	rbB2 = "#!fake nexusd v9.9.2 ok\n"
	rbB1 = "#!fake nexusd v9.9.1 ok\n"
	rbU2 = "[Service]\nExecStart=/usr/local/bin/nexusd serve # U2\n"
	rbU1 = "[Service]\nExecStart=/usr/local/bin/nexusd serve # U1\n"
)

// setRunningVersion sets version.Version, the running nexusd's version,
// for the test.
func setRunningVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

// fakeBinaryRunner is F-RUN for a rollback binary that is an F-SYS fake:
// validate-config exits 0 and -version prints the fake's version.
func fakeBinaryRunner() *fakeRunner {
	return newFakeRunner(func(_ context.Context, argv []string) (RunResult, error) {
		switch {
		case len(argv) == 4 && argv[1] == "validate-config" && argv[2] == "--config":
			return RunResult{}, nil
		case len(argv) == 2 && argv[1] == "-version":
			bin, err := readFakeBinary(argv[0])
			if err != nil {
				return RunResult{Stderr: []byte(err.Error()), ExitCode: 126}, nil
			}
			return RunResult{Stdout: []byte("nexusd " + bin.version + "\n")}, nil
		}
		return RunResult{}, fmt.Errorf("unexpected argv %q", argv)
	})
}

// rollbackRig is the provider pipeline harness of a rollback: F-SYS with
// PID 100 running B2, B1 as the rollback binary, U2 installed and U1 saved
// under the state directory, F-DAEMON on restartConfig and a temp ops dir.
type rollbackRig struct {
	p         *Provider
	f         *fakeSystem
	d         *fakeDaemon
	run       *fakeRunner
	stateDir  string
	savedUnit string
	opsDir    string
	funcs     provider.DetachedFuncs
}

func newRollbackRig(t *testing.T, savedUnit bool) *rollbackRig {
	t.Helper()
	setRunningVersion(t, "v9.9.2")
	f := newFakeSystem(t, 100)
	f.bootActive(t, "v9.9.2", "ok")
	writeFakeBinary(t, f.paths.Prev, "v9.9.1", "ok")
	writeFile(t, f.paths.UnitFile, rbU2)
	stateDir := t.TempDir()
	saved := filepath.Join(stateDir, "rollback", "nexusd.service")
	if savedUnit {
		writeFile(t, saved, rbU1)
	}
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeFile(t, cfgPath, restartConfig)
	d := newFakeDaemon(cfgPath, stateDir, time.Now().Add(-time.Minute))
	run := fakeBinaryRunner()
	p := New(Options{
		Host:       StackHost,
		Paths:      f.paths,
		Systemd:    AvailableSystemd,
		InService:  func() error { return nil },
		Executable: func() (string, error) { return restartExe, nil },
		Runner:     run,
	})
	p.UseDaemon(d)
	opsDir := t.TempDir()
	p.UseOpsDir(opsDir)
	return &rollbackRig{p: p, f: f, d: d, run: run, stateDir: stateDir, savedUnit: saved, opsDir: opsDir,
		funcs: detachedOf(t, p, spec.Rollback.Op().ID)}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readFile returns path's content, or "" when it does not exist.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// files are the four swapped files' contents: Binary, Prev, UnitFile and
// the saved unit.
func (r *rollbackRig) files(t *testing.T) [4]string {
	t.Helper()
	return [4]string{readFile(t, r.f.paths.Binary), readFile(t, r.f.paths.Prev), readFile(t, r.f.paths.UnitFile),
		readFile(t, r.savedUnit)}
}

// pipeline runs Admit, Prepare, the helper and Finalize of operation id.
func (r *rollbackRig) pipeline(t *testing.T, id string) (detach.Spec, spec.RollbackResult, error) {
	t.Helper()
	if err := r.funcs.Admit(t.Context(), spec.RollbackInput{}); err != nil {
		t.Fatalf("Admit = %v, want nil", err)
	}
	s, err := r.funcs.Prepare(t.Context(), spec.RollbackInput{}, provider.NewRun(id, nil))
	if err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	code, _, stderr := runHelperSpec(t, r.f, s)
	if code != 0 {
		t.Fatalf("helper exit %d, want 0; stderr: %s", code, stderr)
	}
	out, err := r.funcs.Finalize(t.Context(), spec.RollbackInput{}, detach.Exit{}, provider.NewRun(id, nil))
	if err != nil {
		return s, spec.RollbackResult{}, err
	}
	res, ok := out.(spec.RollbackResult)
	if !ok {
		t.Fatalf("Finalize returned %T, want spec.RollbackResult", out)
	}
	return s, res, nil
}

// tmpFiles lists the *.tmp files in dirs.
func tmpFiles(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, dir := range dirs {
		names, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
		if err != nil {
			t.Fatal(err)
		}
		hidden, err := filepath.Glob(filepath.Join(dir, ".*.tmp"))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append(names, hidden...)...)
	}
	return out
}

// callNames lists the names of F-SYS's recorded calls from index from on.
func callNames(f *fakeSystem, from int) []string {
	var out []string
	for _, c := range f.calls[from:] {
		out = append(out, c.name)
	}
	return out
}

// wantResetFailedBeforeEachRestart fails unless every Restart has a
// ResetFailed since the previous Restart.
func wantResetFailedBeforeEachRestart(t *testing.T, f *fakeSystem) {
	t.Helper()
	reset, restarts := false, 0
	for _, c := range f.calls {
		switch c.name {
		case "ResetFailed":
			reset = true
		case "Restart":
			restarts++
			if !reset {
				t.Errorf("Restart %d was not preceded by a ResetFailed: %v", restarts, callNames(f, 0))
			}
			reset = false
		}
	}
	if restarts == 0 {
		t.Error("no Restart was called")
	}
}

// TC-SM-67 (WI 4.3; FR-12, FR-9, CON-2): A rollback swaps the binary and unit files and verifies the target version
func TestTCSM67_ARollbackSwapsTheBinaryAnd(t *testing.T) {
	r := newRollbackRig(t, true)
	const id = "op-67"

	// Steps 1-2.
	if err := r.funcs.Admit(t.Context(), spec.RollbackInput{}); err != nil {
		t.Fatalf("Admit = %v", err)
	}
	s, err := r.funcs.Prepare(t.Context(), spec.RollbackInput{}, provider.NewRun(id, nil))
	if err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	dir := filepath.Join(r.opsDir, id)
	in, _ := readInput(t, dir)
	if in.Mode != "rollback" || in.FromVersion != version.Version || in.FromVersion != "v9.9.2" ||
		in.ToVersion != "v9.9.1" {
		t.Errorf("input.json mode %q fromVersion %q toVersion %q, want rollback, %s, v9.9.1", in.Mode,
			in.FromVersion, in.ToVersion, version.Version)
	}
	if in.SavedUnit != r.savedUnit || in.Paths != r.f.paths {
		t.Errorf("input.json savedUnit %q paths %+v, want %q %+v", in.SavedUnit, in.Paths, r.savedUnit, r.f.paths)
	}
	if in.Health.URL != "https://127.0.0.1:8443" || in.Health.Timeout != 30*time.Second {
		t.Errorf("input.json health %+v", in.Health)
	}
	if want := 2*(30*time.Second+20*time.Second) + 60*time.Second; s.MaxRuntime != want {
		t.Errorf("MaxRuntime %v, want %v", s.MaxRuntime, want)
	}
	if want := []string{restartExe, "service-helper", "--dir", dir}; !slices.Equal(s.Argv, want) ||
		s.KillMode != detach.KillProcess || s.Dir != dir {
		t.Errorf("spec %+v, want argv %q, KillProcess, dir %s", s, want, dir)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("the spec does not validate: %v", err)
	}

	// Step 3.
	nCalls := len(r.f.calls)
	code, stdout, stderr := runHelperSpec(t, r.f, s)
	if code != 0 {
		t.Fatalf("helper exit %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "swap") {
		t.Errorf("helper steps do not mention the swap: %s", stdout)
	}
	wantStepOrder(t, stdout, "swapping ", "reset-failed nexusd.service", "restarting nexusd.service onto")

	// Step 4.
	out, err := r.funcs.Finalize(t.Context(), spec.RollbackInput{}, detach.Exit{}, provider.NewRun(id, nil))
	if err != nil {
		t.Fatalf("Finalize = %v", err)
	}

	// Step 5.
	if got, want := r.files(t), [4]string{rbB1, rbB2, rbU1, rbU2}; got != want {
		t.Errorf("files (binary, prev, unit, saved) = %q, want %q", got, want)
	}
	if tmps := tmpFiles(t, filepath.Dir(r.f.paths.Binary), filepath.Dir(r.f.paths.UnitFile),
		filepath.Dir(r.savedUnit)); len(tmps) != 0 {
		t.Errorf("temporary files remain: %v", tmps)
	}
	calls := callNames(r.f, nCalls)
	if len(calls) < 5 || calls[0] != "Show" || !slices.Equal(calls[1:4], []string{"DaemonReload", "ResetFailed",
		"Restart"}) || calls[4] != "Show" {
		t.Errorf("F-SYS calls %v, want Show (pid0), DaemonReload, ResetFailed, Restart, then Show... (health)", calls)
	}
	if r.f.starts != 1 {
		t.Errorf("F-SYS starts %d, want 1", r.f.starts)
	}
	want := spec.RollbackResult{ActionResult: apitypes.ActionResult{Changed: true}, FromVersion: "v9.9.2",
		ToVersion: "v9.9.1"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("result %+v, want %+v", out, want)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"changed":true,"fromVersion":"v9.9.2","toVersion":"v9.9.1"}` {
		t.Errorf("result JSON %s", got)
	}
}

// TC-SM-68 (WI 4.3; FR-12, ASM-7): Rolling back a rollback restores the original pair byte for byte
func TestTCSM68_RollingBackARollbackRestoresThe(t *testing.T) {
	r := newRollbackRig(t, true)
	sums := func() [4][32]byte {
		var out [4][32]byte
		for i, c := range r.files(t) {
			out[i] = sha256.Sum256([]byte(c))
		}
		return out
	}
	original := sums()
	// Setup: the state after TC-SM-67.
	if _, _, err := r.pipeline(t, "op-68a"); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	setRunningVersion(t, "v9.9.1") // nexusd now runs B1

	// Step 1.
	_, out, err := r.pipeline(t, "op-68b")
	if err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if in, _ := readInput(t, filepath.Join(r.opsDir, "op-68b")); in.ToVersion != "v9.9.2" {
		t.Errorf("second rollback's toVersion %q, want v9.9.2", in.ToVersion)
	}
	if got, want := r.files(t), [4]string{rbB2, rbB1, rbU2, rbU1}; got != want {
		t.Errorf("files (binary, prev, unit, saved) = %q, want %q", got, want)
	}
	if got := sums(); got != original {
		t.Errorf("SHA-256 of the files %x, want the originals %x", got, original)
	}
	want := spec.RollbackResult{ActionResult: apitypes.ActionResult{Changed: true}, FromVersion: "v9.9.1",
		ToVersion: "v9.9.2"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("result %+v, want %+v", out, want)
	}
	if v, err := r.f.ExeVersion(t.Context(), fmt.Sprintf("/proc/%d/exe", r.f.mainPID)); err != nil || v != "v9.9.2" {
		t.Errorf("the running process is %q, %v; want v9.9.2", v, err)
	}
}

// TC-SM-69 (WI 4.3; FR-12): With no saved unit, only the binary is swapped
func TestTCSM69_WithNoSavedUnitOnlyThe(t *testing.T) {
	r := newRollbackRig(t, false)
	unitBefore := sha256.Sum256([]byte(readFile(t, r.f.paths.UnitFile)))

	// Step 1.
	_, out, err := r.pipeline(t, "op-69")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if readFile(t, r.f.paths.Binary) != rbB1 || readFile(t, r.f.paths.Prev) != rbB2 {
		t.Errorf("binaries not swapped: binary %q, prev %q", readFile(t, r.f.paths.Binary),
			readFile(t, r.f.paths.Prev))
	}
	if got := sha256.Sum256([]byte(readFile(t, r.f.paths.UnitFile))); got != unitBefore {
		t.Error("the unit file changed")
	}
	if _, err := os.Lstat(filepath.Join(r.stateDir, "rollback")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rollback/ directory: %v, want it not created", err)
	}
	if !out.Changed || out.ToVersion != "v9.9.1" {
		t.Errorf("result %+v, want changed to v9.9.1", out)
	}
}

// errCrash is the crash injected into a swap.
var errCrash = errors.New("injected crash")

// TC-SM-70 (WI 4.3; FR-12, NFR-5): Crash injection: the binary path exists after every intermediate swap step
func TestTCSM70_CrashInjectionTheBinaryPathExists(t *testing.T) {
	setup := func(t *testing.T) (Paths, string) {
		root := t.TempDir()
		p := Paths{
			Binary:   filepath.Join(root, "bin", "nexusd"),
			Prev:     filepath.Join(root, "bin", "nexusd.prev"),
			UnitFile: filepath.Join(root, "etc", "nexusd.service"),
			Unit:     "nexusd.service",
		}
		saved := filepath.Join(root, "state", "rollback", "nexusd.service")
		writeFile(t, p.Binary, rbB2)
		writeFile(t, p.Prev, rbB1)
		writeFile(t, p.UnitFile, rbU2)
		writeFile(t, saved, rbU1)
		return p, saved
	}

	// Without a fault, the hook sees every step and the pairs are swapped.
	p, saved := setup(t)
	var seen []int
	if err := swapInstalled(p, saved, func(step int) error { seen = append(seen, step); return nil }); err != nil {
		t.Fatalf("swap: %v", err)
	}
	n := len(seen)
	if !slices.Equal(seen, []int{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("hook steps %v, want 1..6", seen)
	}
	if got := [4]string{readFile(t, p.Binary), readFile(t, p.Prev), readFile(t, p.UnitFile),
		readFile(t, saved)}; got != [4]string{rbB1, rbB2, rbU1, rbU2} {
		t.Errorf("after a whole swap: %q", got)
	}

	other := map[string]string{rbB1: rbB2, rbB2: rbB1, rbU1: rbU2, rbU2: rbU1}
	// pair checks one swapped pair after a crash: cur exists with one of
	// the two contents, and the other content is at prev or, when prev is
	// missing, at tmp.
	pair := func(t *testing.T, k int, cur, prev, tmp string, a, b string) {
		c := readFile(t, cur)
		if c != a && c != b {
			t.Errorf("k=%d: %s holds %q, want one of the pair", k, cur, c)
			return
		}
		// The units are swapped by copy, so for a moment the saved unit
		// equals the installed one and the other content is only at tmp.
		pv, tv := readFile(t, prev), readFile(t, tmp)
		switch {
		case pv != "" && pv != a && pv != b:
			t.Errorf("k=%d: %s holds %q, want one of the pair", k, prev, pv)
		case pv == "" && tv != other[c]:
			t.Errorf("k=%d: %s is missing and %s holds %q, want %q: content lost", k, prev, tmp, tv, other[c])
		case pv != other[c] && tv != other[c]:
			t.Errorf("k=%d: neither %s nor %s holds %q: content lost", k, prev, tmp, other[c])
		}
	}
	for k := 1; k <= n; k++ {
		t.Run(fmt.Sprintf("crash after step %d", k), func(t *testing.T) {
			// Step 1.
			p, saved := setup(t)
			err := swapInstalled(p, saved, func(step int) error {
				if step == k {
					return errCrash
				}
				return nil
			})
			if !errors.Is(err, errCrash) {
				t.Fatalf("swap = %v, want the injected crash", err)
			}
			// Step 2.
			if _, err := os.Stat(p.Binary); err != nil {
				t.Fatalf("k=%d: %s: %v", k, p.Binary, err)
			}
			pair(t, k, p.Binary, p.Prev, p.Binary+".swap.tmp", rbB1, rbB2)
			if _, err := os.Stat(p.UnitFile); err != nil {
				t.Fatalf("k=%d: %s: %v", k, p.UnitFile, err)
			}
			pair(t, k, p.UnitFile, saved, saved+".swap.tmp", rbU1, rbU2)
		})
	}
	if _, err := os.Stat(p.Binary + ".swap.tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a whole swap left %s.swap.tmp: %v", p.Binary, err)
	}
}

// A stale nexusd.swap.tmp from an interrupted swap is replaced, never
// followed, and a crash right after step 2 is found again by Admit, which
// names the leftover copy.
func TestRollbackStaleSwapTmp(t *testing.T) {
	r := newRollbackRig(t, true)
	tmp := r.f.paths.Binary + ".swap.tmp"
	target := filepath.Join(t.TempDir(), "elsewhere")
	writeFile(t, target, "do not touch\n")
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatal(err)
	}
	if err := swapInstalled(r.f.paths, r.savedUnit, nil); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := readFile(t, target); got != "do not touch\n" {
		t.Errorf("the swap wrote through the stale symlink: %q", got)
	}
	if readFile(t, r.f.paths.Binary) != rbB1 || readFile(t, r.f.paths.Prev) != rbB2 {
		t.Error("binaries not swapped")
	}

	// A crash after step 2: prev is missing, its content at swap.tmp.
	err := swapInstalled(r.f.paths, r.savedUnit, func(step int) error {
		if step == 2 {
			return errCrash
		}
		return nil
	})
	if !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	err = r.funcs.Admit(t.Context(), spec.RollbackInput{})
	perr := problemOfErr(t, err)
	if perr.Slug() != problem.RollbackUnavailable || perr.Extensions["path"] != r.f.paths.Prev ||
		!strings.Contains(perr.Detail, tmp) {
		t.Errorf("Admit after an interrupted swap: %s %v %q, want rollback-unavailable naming %s", perr.Slug(),
			perr.Extensions, perr.Detail, tmp)
	}
}

// TC-SM-ERR35 (WI 4.3; FR-12): rollback-unavailable when the previous binary is missing or does not report a version
func TestTCSMERR35_RollbackUnavailableWhenThePreviousBinary(t *testing.T) {
	s := newStack(t, stackConfig{})
	s.p.UseOpsDir(t.TempDir())

	// Step 1 (a): no rollback binary.
	resp := s.action(t, "rollback", s.admin, nil).want(t, http.StatusConflict)
	body := problemOf(t, resp.body)
	if slugOf(body) != string(problem.RollbackUnavailable) || body["path"] != s.paths.Prev {
		t.Errorf("(a) problem %v, want rollback-unavailable with path %s", body, s.paths.Prev)
	}

	// Step 1 (b): a rollback binary whose -version is not parsable.
	writeScript(t, s.paths.Prev, `case "$1" in validate-config) exit 0;; -version) echo oops;; esac`)
	resp = s.action(t, "rollback", s.admin, nil).want(t, http.StatusConflict)
	body = problemOf(t, resp.body)
	detail, _ := body["detail"].(string)
	if slugOf(body) != string(problem.RollbackUnavailable) || body["path"] != s.paths.Prev ||
		!strings.Contains(detail, `"oops"`) {
		t.Errorf("(b) problem %v, want rollback-unavailable naming the unparsable version", body)
	}

	if ops := s.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
	if outs := outcomes(s.auditOf(t, spec.Rollback.Op().ID)); len(outs) != 2 ||
		outs[0] != "refused:rollback-unavailable" || outs[1] != "refused:rollback-unavailable" {
		t.Errorf("audit outcomes %v, want two refused rollback-unavailable records", outs)
	}
}

// TC-SM-ERR36 (WI 4.3; FR-12, FR-3): A previous binary that rejects the config refuses the rollback with config-invalid
func TestTCSMERR36_APreviousBinaryThatRejectsThe(t *testing.T) {
	s := newStack(t, stackConfig{})
	opsDir := t.TempDir()
	s.p.UseOpsDir(opsDir)
	stderrFile, err := filepath.Abs(filepath.Join("testdata", "validate-config", "unknown-key.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	captured := readFile(t, stderrFile)
	lines := strings.Split(strings.TrimSuffix(captured, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%s has %d lines, want the 2 captured ones", stderrFile, len(lines))
	}
	argsFile := filepath.Join(t.TempDir(), "args")
	writeScript(t, s.paths.Prev, fmt.Sprintf(`if [ "$1" = validate-config ]; then
  for a in "$@"; do printf '%%s\n' "$a"; done > '%s'
  cat '%s' >&2
  exit 1
fi
echo "nexusd v9.9.0"`, argsFile, stderrFile))

	// Step 1.
	resp := s.action(t, "rollback", s.admin, nil).want(t, http.StatusUnprocessableEntity)
	body := problemOf(t, resp.body)
	if slugOf(body) != string(problem.ConfigInvalid) || body["configPath"] != s.daemon.ConfigPath() {
		t.Errorf("problem %v, want config-invalid with configPath %s", body, s.daemon.ConfigPath())
	}
	want := []any{map[string]any{"message": lines[0]}, map[string]any{"message": lines[1]}}
	if !reflect.DeepEqual(body["errors"], want) {
		t.Errorf("errors %v, want %v", body["errors"], want)
	}
	if ops := s.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
	if names := dirNames(t, opsDir); len(names) != 0 {
		t.Errorf("ops dir %v, want empty", names)
	}
	if got, want := readFile(t, argsFile), "validate-config\n--config\n"+s.daemon.ConfigPath()+"\n"; got != want {
		t.Errorf("the rollback binary was run with %q, want %q", got, want)
	}
}

// TC-SM-ERR37 (WI 4.3; FR-12, FR-10): A rollback that fails health swaps back and reports rollback-failed
func TestTCSMERR37_ARollbackThatFailsHealthSwaps(t *testing.T) {
	for _, variant := range []bool{false, true} {
		t.Run(fmt.Sprintf("B2 also fails=%t", variant), func(t *testing.T) {
			r := newRollbackRig(t, true)
			writeFakeBinary(t, r.f.paths.Prev, "v9.9.1", "exit-at-start")
			if variant {
				// PID 100 already runs the healthy image; the file B2 now
				// exits at start, so the swap back is unhealthy too.
				writeFakeBinary(t, r.f.paths.Binary, "v9.9.2", "exit-at-start")
			}
			before := r.files(t)

			// Steps 1-2.
			_, _, err := r.pipeline(t, "op-err37")
			perr := problemOfErr(t, err)
			if perr.Slug() != problem.RollbackFailed {
				t.Fatalf("problem %s (%s), want rollback-failed", perr.Slug(), perr.Detail)
			}
			if got := r.files(t); got != before {
				t.Errorf("files %q, want the originals %q", got, before)
			}
			wantResetFailedBeforeEachRestart(t, r.f)
			if n := len(r.f.callsOf("Restart")); n != 2 {
				t.Errorf("Restart calls %d, want 2 (the rollback and the swap back)", n)
			}
			ext := perr.Extensions
			if ext["rolledBack"] != false || ext["fromVersion"] != "v9.9.2" || ext["toVersion"] != "v9.9.1" {
				t.Errorf("extensions %v, want rolledBack false and both versions", ext)
			}
			if c, ok := ext["condition"].(int); !ok || c < 1 || c > 4 {
				t.Errorf("condition %v, want the FR-10 condition", ext["condition"])
			}
			down := strings.Contains(perr.Detail, "may be down")
			manual := strings.Contains(perr.Detail, "systemctl reset-failed nexusd.service") &&
				strings.Contains(perr.Detail, "systemctl restart nexusd.service")
			if variant != down || variant != manual {
				t.Errorf("detail %q: says may be down %t, names the manual steps %t; want %t", perr.Detail, down,
					manual, variant)
			}
		})
	}
}

// A Finalize without result.json is outcome-unknown, guiding a check of
// which binary is where; LostDetail says the same, and a helper that never
// started changed nothing.
func TestRollbackFinalizeWithoutResult(t *testing.T) {
	r := newRollbackRig(t, true)
	const id = "op-lost"
	if err := r.funcs.Admit(t.Context(), spec.RollbackInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.funcs.Prepare(t.Context(), spec.RollbackInput{}, provider.NewRun(id, nil)); err != nil {
		t.Fatal(err)
	}
	run := provider.NewRun(id, nil)
	guidance := []string{"systemctl status nexusd.service", r.f.paths.Binary + " -version", r.f.paths.Prev,
		r.f.paths.Binary + ".swap.tmp"}
	_, err := r.funcs.Finalize(t.Context(), spec.RollbackInput{}, detach.Exit{ExitCode: -1, Signal: "SIGKILL"}, run)
	perr := problemOfErr(t, err)
	if perr.Slug() != problem.OutcomeUnknown {
		t.Errorf("problem %s, want outcome-unknown", perr.Slug())
	}
	for _, g := range guidance {
		if !strings.Contains(perr.Detail, g) {
			t.Errorf("detail %q lacks %q", perr.Detail, g)
		}
		if d := r.funcs.LostDetail(run, spec.RollbackInput{}); !strings.Contains(d, g) {
			t.Errorf("LostDetail %q lacks %q", d, g)
		}
	}
	_, err = r.funcs.Finalize(t.Context(), spec.RollbackInput{}, detach.Exit{ExitCode: -1, Error: "exec: denied"}, run)
	if perr := problemOfErr(t, err); perr.Slug() != problem.InternalError || !strings.Contains(perr.Detail,
		"nothing was changed") {
		t.Errorf("never started: %s %q", perr.Slug(), perr.Detail)
	}
}

// Rollback is refused without self-management before the rollback binary
// is run, a rollback binary that cannot run is rollback-unavailable, and
// rollback holds the nexusd lane.
func TestRollbackAdmission(t *testing.T) {
	r := newRollbackRig(t, true)
	l, err := r.funcs.Lane(t.Context(), spec.RollbackInput{})
	if err != nil || l != (provider.Lane{Key: spec.LaneKey, Policy: provider.LaneReject}) {
		t.Errorf("lane %+v %v, want nexusd/reject", l, err)
	}

	r.p.inService = func() error { return errors.New("not running in nexusd.service") }
	if perr := problemOfErr(t, r.funcs.Admit(t.Context(), spec.RollbackInput{})); perr.Slug() !=
		problem.CapabilityUnavailable {
		t.Errorf("outside the unit: %s, want capability-unavailable", perr.Slug())
	}
	if n := r.run.execs(r.f.paths.Prev); n != 0 {
		t.Errorf("the rollback binary ran %d times before the self-management refusal", n)
	}
	r.p.inService = func() error { return nil }

	r.run.setReply(func(context.Context, []string) (RunResult, error) {
		return RunResult{ExitCode: -1}, errors.New("fork/exec: exec format error")
	})
	perr := problemOfErr(t, r.funcs.Admit(t.Context(), spec.RollbackInput{}))
	if perr.Slug() != problem.RollbackUnavailable || !strings.Contains(perr.Detail, "exec format error") {
		t.Errorf("unrunnable prev: %s %q, want rollback-unavailable with the reason", perr.Slug(), perr.Detail)
	}
}

// rollbackInputGolden is rollback's input.json as release 1 writes it
// (A-30): members may be added, never renamed or dropped.
const rollbackInputGolden = `{
  "mode": "rollback",
  "fromVersion": "v9.9.2",
  "toVersion": "v9.9.1",
  "savedUnit": "/var/lib/nexus/rollback/nexusd.service",
  "health": {
    "url": "https://127.0.0.1:8443",
    "certFile": "/var/lib/nexus/tls/cert.pem",
    "timeout": 30000000000,
    "stability": 5000000000,
    "poll": 1000000000
  },
  "paths": {
    "Binary": "/usr/local/bin/nexusd",
    "Prev": "/usr/local/bin/nexusd.prev",
    "UnitFile": "/etc/systemd/system/nexusd.service",
    "Unit": "nexusd.service"
  }
}`

// rollbackResultGolden are rollback's result.json files, success and
// failure.
const (
	rollbackResultGolden       = `{"changed": true, "rolledBack": false, "fromVersion": "v9.9.2", "toVersion": "v9.9.1"}`
	rollbackResultFailedGolden = `{"changed": false, "rolledBack": false, "fromVersion": "v9.9.2",
		"toVersion": "v9.9.1", "error": {"slug": "rollback-failed", "detail": "nexusd v9.9.1 was not healthy",
		"condition": 1}}`
)

// The rollback input.json wire format is pinned in both directions.
func TestRollbackInputWireFormat(t *testing.T) {
	setRunningVersion(t, "v9.9.2")
	run := newFakeRunner(func(_ context.Context, argv []string) (RunResult, error) {
		if argv[len(argv)-1] == "-version" {
			return RunResult{Stdout: []byte("nexusd v9.9.1\n")}, nil
		}
		return RunResult{}, nil
	})
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeFile(t, cfgPath, restartConfig)
	p := New(Options{Systemd: AvailableSystemd, InService: func() error { return nil },
		Executable: func() (string, error) { return restartExe, nil }, Runner: run})
	p.UseDaemon(newFakeDaemon(cfgPath, "/var/lib/nexus", time.Now()))
	opsDir := t.TempDir()
	p.UseOpsDir(opsDir)
	// The default Prev need not exist for Prepare's stat: point the
	// provider at a stand-in file there is.
	prev := filepath.Join(t.TempDir(), "nexusd.prev")
	writeFile(t, prev, rbB1)
	p.paths.Prev = prev
	if _, err := detachedOf(t, p, spec.Rollback.Op().ID).Prepare(t.Context(), spec.RollbackInput{},
		provider.NewRun("op-wire", nil)); err != nil {
		t.Fatal(err)
	}
	got, raw := readInput(t, filepath.Join(opsDir, "op-wire"))
	golden := strings.Replace(rollbackInputGolden, `"/usr/local/bin/nexusd.prev"`, fmt.Sprintf("%q", prev), 1)
	wantWireSuperset(t, raw, golden)

	want := helperInput{
		Mode: "rollback", FromVersion: "v9.9.2", ToVersion: "v9.9.1",
		SavedUnit: "/var/lib/nexus/rollback/nexusd.service",
		Health: healthTarget{URL: "https://127.0.0.1:8443", CertFile: "/var/lib/nexus/tls/cert.pem",
			Timeout: 30 * time.Second, Stability: 5 * time.Second, Poll: time.Second},
		Paths: DefaultPaths(),
	}
	want.Paths.Prev = prev
	if got != want {
		t.Errorf("input.json decodes to %+v, want %+v", got, want)
	}
	var back helperInput
	if err := json.Unmarshal([]byte(rollbackInputGolden), &back); err != nil {
		t.Fatal(err)
	}
	want.Paths.Prev = DefaultPaths().Prev
	if back != want {
		t.Errorf("the golden input.json decodes to %+v, want %+v", back, want)
	}
}

// The rollback result.json wire format is pinned the same way.
func TestRollbackResultWireFormat(t *testing.T) {
	ok := helperResult{Changed: true, FromVersion: "v9.9.2", ToVersion: "v9.9.1"}
	failed := helperResult{FromVersion: "v9.9.2", ToVersion: "v9.9.1", Error: &helperError{
		Slug: problem.RollbackFailed, Detail: "nexusd v9.9.1 was not healthy", Condition: 1}}
	for _, tc := range []struct {
		res    helperResult
		golden string
	}{{ok, rollbackResultGolden}, {failed, rollbackResultFailedGolden}} {
		b, err := json.Marshal(tc.res)
		if err != nil {
			t.Fatal(err)
		}
		wantWireSuperset(t, b, tc.golden)
		var back helperResult
		if err := json.Unmarshal([]byte(tc.golden), &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, tc.res) {
			t.Errorf("the golden result.json decodes to %+v, want %+v", back, tc.res)
		}
	}
	// The failure maps to rollback-failed with each declared member.
	perr := failed.problem()
	if perr.Slug() != problem.RollbackFailed || perr.Extensions["condition"] != 1 ||
		perr.Extensions["rolledBack"] != false || perr.Extensions["fromVersion"] != "v9.9.2" ||
		perr.Extensions["toVersion"] != "v9.9.1" {
		t.Errorf("problem %s %v", perr.Slug(), perr.Extensions)
	}
}

// A caller that leaves during rollback admission (CR-1): the rollback
// binary's failed run is the cancellation's, so it is not
// rollback-unavailable.
func TestRollbackAdmissionCancelledByTheCaller(t *testing.T) {
	r := newRollbackRig(t, true)
	r.run.setReply(func(ctx context.Context, argv []string) (RunResult, error) { return execRunner{}.Run(ctx, argv) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := r.funcs.Admit(ctx, spec.RollbackInput{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Admit on a cancelled context = %v, want context.Canceled", err)
	}
	var perr *problem.Error
	if errors.As(err, &perr) {
		t.Errorf("Admit = problem %s, want the cancellation", perr.Slug())
	}
}
