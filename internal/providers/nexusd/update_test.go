package nexusd

// Tests for update admission, Prepare and Finalize (WI 5.3): TC-SM-82 to
// TC-SM-87, TC-SM-E10 and TC-SM-ERR45 to TC-SM-ERR50 and TC-SM-ERR60. The
// packages are generated with archive/tar, their nexusd a shell stand-in
// that prints its version, and signed with the fixed test keys.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/release"
	"github.com/mauktik/nexus/internal/systemspec"
)

// seedTrusted2 is the second trusted key, K2 (TC-SM-87).
var seedTrusted2 = bytes32(0x22)

func bytes32(b byte) []byte {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = b
	}
	return s
}

// keyB64 is k's public key as providers.nexusd.update.trustedKeys holds it.
func keyB64(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

// signBytes is the signature query value of b signed by k.
func signBytes(k ed25519.PrivateKey, b []byte) string {
	return strings.TrimSuffix(string(release.EncodeSignature(release.Sign(k, sha256.Sum256(b)))), "\n")
}

// updateTop is the top-level directory of a package of version v.
func updateTop(v string) string { return "nexusd-" + v + "-linux-" + runtime.GOARCH }

// updatePackage is a release tarball of version v whose nexusd prints
// "nexusd <prints>" for -version and refuses any other argv, with extra
// entries appended. Two files carry modes extraction normalizes.
func updatePackage(t *testing.T, v, prints string, extra ...pkgTE) []byte {
	t.Helper()
	top := updateTop(v)
	entries := []pkgTE{
		pkgDir(top + "/"),
		pkgFile(top+"/nexusd", "#!/bin/sh\n[ \"$1\" = -version ] || exit 2\necho 'nexusd "+prints+"'\n", 0o755),
		pkgFile(top+"/install.sh", "#!/bin/sh\nexit 0\n", 0o755),
		pkgFile(top+"/nexusd.service", "[Unit]\n", 0o644),
		pkgFile(top+"/nexusd.default.yaml", "listen: 127.0.0.1:8400\n", 0o600),
		pkgFile(top+"/uninstall.sh", "#!/bin/sh\nexit 0\n", 0o4711),
	}
	return pkgGzip(t, pkgTarBytes(t, nil, append(entries, extra...)))
}

// updateModes are the normalized modes of updatePackage's files (A-36).
var updateModes = map[string]fs.FileMode{
	"nexusd": 0o755, "install.sh": 0o755, "nexusd.service": 0o644, "nexusd.default.yaml": 0o644,
	"uninstall.sh": 0o755,
}

// launchSpy is the ops manager's detach Launch: it records each
// invocation and starts nothing, so a launched operation stays Running
// until the test writes its exit.json.
type launchSpy struct {
	mu   sync.Mutex
	invs []detach.Invocation
}

func (l *launchSpy) launch(_ context.Context, inv detach.Invocation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.invs = append(l.invs, inv)
	return nil
}

func (l *launchSpy) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.invs)
}

// await waits until operation id was launched.
func (l *launchSpy) await(t *testing.T, id string) detach.Invocation {
	t.Helper()
	var inv detach.Invocation
	waitFor(t, "the launch of "+id, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, v := range l.invs {
			if v.ID == id {
				inv = v
				return true
			}
		}
		return false
	})
	return inv
}

// updateStackConfig configures newUpdateStack.
type updateStackConfig struct {
	keys  []ed25519.PrivateKey // Trusted keys; none means update-not-configured
	opts  Options              // A nil Runner means F-RUN over the real runner
	clock *fakeClock
	extra []provider.Provider
}

// updateStack is the T0 stack with a launch spy and a counting Runner
// that runs each program for real (F-RUN).
type updateStack struct {
	*stack
	spy *launchSpy
	run *fakeRunner
}

func newUpdateStack(t *testing.T, cfg updateStackConfig) *updateStack {
	t.Helper()
	keys := make([]string, 0, len(cfg.keys))
	for _, k := range cfg.keys {
		keys = append(keys, keyB64(k))
	}
	run := newFakeRunner(func(ctx context.Context, argv []string) (RunResult, error) {
		return execRunner{}.Run(ctx, argv)
	})
	opts := cfg.opts
	if opts.Runner == nil {
		opts.Runner = run
	}
	section := "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: auto\nproviders:\n  docker:\n    enabled: false\n" +
		"  nexusd:\n    enabled: true\n    update:\n      trustedKeys: [" + strings.Join(quoted(keys), ", ") + "]\n"
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeFile(t, path, section)
	spy := &launchSpy{}
	s := newStack(t, stackConfig{opts: opts, clock: cfg.clock, section: path, launch: spy.launch, extra: cfg.extra})
	return &updateStack{stack: s, spy: spy, run: run}
}

// quoted returns each of ss as a YAML double-quoted string.
func quoted(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// stage uploads pkg signed by k and returns its upload ID.
func (u *updateStack) stage(t *testing.T, pkg []byte, k ed25519.PrivateKey) string {
	t.Helper()
	res, _ := stored(t, u.upload(t, u.admin, signBytes(k, pkg), pkg))
	return res.UploadID
}

// update POSTs nexusd.actions.update with body.
func (u *updateStack) update(t *testing.T, body string) response {
	t.Helper()
	return u.do(t, http.MethodPost, "/providers/nexusd/actions/update", u.admin, body, nil)
}

// updateOf is the JSON body naming uploadID, with allowDowngrade when set.
func updateOf(uploadID string, allowDowngrade bool) string {
	if allowDowngrade {
		return fmt.Sprintf(`{"uploadId":%q,"allowDowngrade":true}`, uploadID)
	}
	return fmt.Sprintf(`{"uploadId":%q}`, uploadID)
}

// accepted decodes a 202 update answer and returns the operation's ID.
func accepted(t *testing.T, r response) string {
	t.Helper()
	r.want(t, http.StatusAccepted)
	op := decodeJSON[apitypes.Operation](t, r.body)
	if op.ID == "" {
		t.Fatalf("202 body %s has no operation", r.body)
	}
	return op.ID
}

// staged reports whether staging/<id> exists.
func (u *updateStack) staged(id string) bool { return pkgExists(filepath.Join(u.stagingDir(), id)) }

// totalRuns is how many programs F-RUN ran.
func (r *fakeRunner) totalRuns() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.runs {
		n += c
	}
	return n
}

// recordOf returns the update audit record with outcome.
func (u *updateStack) recordOf(t *testing.T, outcome string) systemspec.AuditRecord {
	t.Helper()
	recs := u.auditOf(t, spec.Update.Op().ID)
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Outcome == outcome {
			return recs[i]
		}
	}
	t.Fatalf("no %s update record in %v", outcome, outcomes(recs))
	return systemspec.AuditRecord{}
}

// wantProblem asserts r's status and slug and returns the problem.
func wantProblem(t *testing.T, r response, status int, slug problem.Slug) map[string]any {
	t.Helper()
	r.want(t, status)
	m := problemOf(t, r.body)
	if slugOf(m) != string(slug) {
		t.Fatalf("slug %s, want %s: %s", slugOf(m), slug, r.body)
	}
	return m
}

// specOnDisk is the part of ops/<id>/spec.json a test checks.
type specOnDisk struct {
	Argv         []string `json:"argv"`
	Env          []string `json:"env"`
	Dir          string   `json:"dir"`
	KillMode     string   `json:"killMode"`
	MaxRuntimeNS int64    `json:"maxRuntimeNs"`
}

func readSpecFile(t *testing.T, dir string) specOnDisk {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, detach.SpecFile))
	if err != nil {
		t.Fatal(err)
	}
	return decodeJSON[specOnDisk](t, b)
}

// TC-SM-82 (WI 5.3; FR-7, FR-9, CON-4): An admitted update creates an op, and Prepare extracts the package and writes input.json
func TestTCSM82_AnAdmittedUpdateCreatesAnOp(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	pkg := updatePackage(t, "v9.9.2", "v9.9.2")
	uploadID := u.stage(t, pkg, k)

	// Step 1.
	id := accepted(t, u.update(t, updateOf(uploadID, false)))

	// Step 2.
	u.spy.await(t, id)

	// Step 3.
	dir := filepath.Join(u.opsDir, id)
	top := filepath.Join(dir, pkgDirName, updateTop("v9.9.2"))
	for name, mode := range updateModes {
		if got := modeOfPath(t, filepath.Join(top, name)); got != mode {
			t.Errorf("%s mode %v, want %v", name, got, mode)
		}
	}
	if names := stagingEntries(t, top); len(names) != len(updateModes) {
		t.Errorf("package dir holds %v, want the 5 required files", names)
	}
	in, _ := readInput(t, dir)
	if in.Mode != "update" || in.RunningVersion != "v9.9.1" || in.ExpectedVersion != "v9.9.2" || in.NoChange {
		t.Errorf("input.json %+v, want update v9.9.1 -> v9.9.2 with noChange false", in)
	}
	if in.PackageDir != top || !strings.HasPrefix(in.PackageDir, filepath.Join(dir, pkgDirName)+"/") {
		t.Errorf("packageDir %q, want %q under ops/<id>/pkg/", in.PackageDir, top)
	}
	if in.InstallTimeout != 120*time.Second {
		t.Errorf("installTimeout %v, want 120s", in.InstallTimeout)
	}
	if in.Paths != u.paths || in.SavedUnit != savedUnitPath(u.stateDir) {
		t.Errorf("input.json paths %+v saved unit %q, want %+v and %q", in.Paths, in.SavedUnit, u.paths,
			savedUnitPath(u.stateDir))
	}
	sp := readSpecFile(t, dir)
	if want := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}; !slices.Equal(sp.Env, want) {
		t.Errorf("spec env %q, want exactly %q", sp.Env, want)
	}
	if sp.KillMode != string(detach.KillProcess) {
		t.Errorf("kill mode %q, want process", sp.KillMode)
	}
	if want := 120*time.Second + 2*(30*time.Second+20*time.Second) + 60*time.Second; time.Duration(sp.MaxRuntimeNS) != want {
		t.Errorf("MaxRuntime %v, want %v", time.Duration(sp.MaxRuntimeNS), want)
	}
	if want := []string{"/x/nexusd", HelperCommand, "--dir", dir}; !slices.Equal(sp.Argv, want) || sp.Dir != dir {
		t.Errorf("argv %q dir %q, want %q in %s", sp.Argv, sp.Dir, want, dir)
	}
	sum := sha256.Sum256(pkg)
	want := map[string]string{"uploadId": uploadID, "sha256": fmt.Sprintf("%x", sum), "packageVersion": "v9.9.2",
		"signature": "valid", "allowDowngrade": "false"}
	if got := u.recordOf(t, "requested").Facts; !reflect.DeepEqual(got, want) {
		t.Errorf("requested facts %v, want %v", got, want)
	}
}

// TC-SM-83 (WI 5.3; FR-7, ASM-6): Updating to the running version yields a noChange input
func TestTCSM83_UpdatingToTheRunningVersionYields(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	uploadID := u.stage(t, updatePackage(t, "v9.9.1", "v9.9.1"), k)

	id := accepted(t, u.update(t, updateOf(uploadID, false)))
	u.spy.await(t, id)
	in, _ := readInput(t, filepath.Join(u.opsDir, id))
	if !in.NoChange || in.RunningVersion != "v9.9.1" || in.ExpectedVersion != "v9.9.1" {
		t.Errorf("input.json %+v, want noChange v9.9.1", in)
	}
}

// TC-SM-84 (WI 5.3; FR-7, ASM-6): --allow-downgrade admits an older package
func TestTCSM84_AllowDowngradeAdmitsAnOlderPackage(t *testing.T) {
	setRunningVersion(t, "v9.9.2")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	uploadID := u.stage(t, updatePackage(t, "v9.9.1", "v9.9.1"), k)

	id := accepted(t, u.update(t, updateOf(uploadID, true)))
	if ops := u.operations(t); len(ops) != 1 || ops[0].ID != id {
		t.Errorf("operations %+v, want the update %s", ops, id)
	}
	if f := u.recordOf(t, "requested").Facts["allowDowngrade"]; f != "true" {
		t.Errorf("allowDowngrade fact %q, want true", f)
	}
	u.spy.await(t, id)
	in, _ := readInput(t, filepath.Join(u.opsDir, id))
	if in.ExpectedVersion != "v9.9.1" || in.RunningVersion != "v9.9.2" || in.NoChange {
		t.Errorf("input.json %+v, want v9.9.2 -> v9.9.1", in)
	}
}

// updateRig is the provider pipeline of an update without the server:
// trusted key K, DefaultPaths, a temp state and ops directory, and the
// staging store, driven through the bound detached functions.
type updateRig struct {
	p      *Provider
	d      *fakeDaemon
	opsDir string
	stg    *staging
	funcs  provider.DetachedFuncs
}

func newUpdateRig(t *testing.T) *updateRig {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeFile(t, cfgPath, restartConfig)
	d := newFakeDaemon(cfgPath, t.TempDir(), time.Now().Add(-time.Minute))
	p := New(Options{Host: StackHost, Paths: DefaultPaths(), Systemd: AvailableSystemd,
		InService: func() error { return nil }, Executable: func() (string, error) { return restartExe, nil }})
	if err := p.Configure(func(into any) error {
		into.(*config.NexusdConfig).Update.TrustedKeys = []string{keyB64(pkgKey(seedTrusted))}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.UseDaemon(d)
	opsDir := t.TempDir()
	p.UseOpsDir(opsDir)
	stg, err := p.stagingOf()
	if err != nil {
		t.Fatal(err)
	}
	return &updateRig{p: p, d: d, opsDir: opsDir, stg: stg, funcs: detachedOf(t, p, spec.Update.Op().ID)}
}

// stage stores pkg signed by K and returns its upload ID.
func (r *updateRig) stage(t *testing.T, pkg []byte) string {
	t.Helper()
	m, err := r.stg.receive(t.Context(), signBytes(pkgKey(seedTrusted), pkg), strings.NewReader(string(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	return m.UploadID
}

// prepare stages a v9.9.2 package, runs Admit and Prepare as operation id
// and returns the input.
func (r *updateRig) prepare(t *testing.T, id string) spec.UpdateInput {
	t.Helper()
	in := spec.UpdateInput{UploadID: r.stage(t, updatePackage(t, "v9.9.2", "v9.9.2"))}
	if err := r.funcs.Admit(t.Context(), in); err != nil {
		t.Fatalf("Admit = %v", err)
	}
	if _, err := r.funcs.Prepare(t.Context(), in, provider.NewRun(id, nil)); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	return in
}

// TC-SM-85 (WI 5.3; FR-6, FR-9, FR-8): Finalize deletes the staged upload idempotently and maps result.json
func TestTCSM85_FinalizeDeletesTheStagedUploadIdempotently(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	ok := helperResult{Changed: true, PreviousVersion: "v9.9.1", NewVersion: "v9.9.2"}
	failed := helperResult{RolledBack: true, PreviousVersion: "v9.9.1", NewVersion: "v9.9.2",
		Error: &helperError{Slug: problem.UpdateFailed, Detail: "install.sh exited 3", Step: "install"}}
	rbFailed := helperResult{PreviousVersion: "v9.9.1", NewVersion: "v9.9.2",
		Error: &helperError{Slug: problem.UpdateRollbackFailed, Detail: "nexusd may be down", Step: "health"}}
	for _, tc := range []struct {
		name string
		res  *helperResult
	}{{"a", &ok}, {"b", &failed}, {"c", &rbFailed}, {"d", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUpdateRig(t)
			id := "op-85" + tc.name
			in := r.prepare(t, id)
			dir := filepath.Join(r.opsDir, id)
			pkg := filepath.Join(dir, pkgDirName)
			run := provider.NewRun(id, nil)
			if tc.res != nil {
				if err := writeResult(dir, *tc.res); err != nil {
					t.Fatal(err)
				}
				// Step 1: a helper that stopped between result.json and
				// its own removal of the package.
				if err := os.RemoveAll(pkg); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(pkg, 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(pkg, "leftover"), "x")
			}

			// Step 2.
			out, err := r.funcs.Finalize(t.Context(), in, detach.Exit{}, run)
			switch tc.name {
			case "a":
				want := spec.UpdateResult{ActionResult: apitypes.ActionResult{Changed: true},
					PreviousVersion: "v9.9.1", NewVersion: "v9.9.2"}
				if err != nil || !reflect.DeepEqual(out, want) {
					t.Errorf("Finalize = %+v, %v; want %+v", out, err, want)
				}
				if pkgExists(pkg) {
					t.Error("ops/<id>/pkg survives Finalize")
				}
				again, err := r.funcs.Finalize(t.Context(), in, detach.Exit{}, run)
				if err != nil || !reflect.DeepEqual(again, want) {
					t.Errorf("second Finalize = %+v, %v; want %+v", again, err, want)
				}
			case "b":
				perr := problemOfErr(t, err)
				want := map[string]any{"rolledBack": true, "step": "install", "previousVersion": "v9.9.1",
					"newVersion": "v9.9.2"}
				if perr.Slug() != problem.UpdateFailed || !reflect.DeepEqual(perr.Extensions, want) {
					t.Errorf("problem %s %v, want update-failed %v", perr.Slug(), perr.Extensions, want)
				}
			case "c":
				perr := problemOfErr(t, err)
				if perr.Slug() != problem.UpdateRollbackFailed || perr.Extensions["rolledBack"] != false {
					t.Errorf("problem %s %v, want update-rollback-failed rolledBack false", perr.Slug(),
						perr.Extensions)
				}
			case "d":
				perr := problemOfErr(t, err)
				if perr.Slug() != problem.OutcomeUnknown {
					t.Errorf("problem %s, want outcome-unknown", perr.Slug())
				}
				detail := r.funcs.LostDetail(run, in)
				for _, g := range []string{"systemctl status nexusd.service", "/usr/local/bin/nexusd -version",
					"nexusd.prev"} {
					if !strings.Contains(detail, g) || !strings.Contains(perr.Detail, g) {
						t.Errorf("LostDetail %q / detail %q lacks %q", detail, perr.Detail, g)
					}
				}
			}
			if pkgExists(filepath.Join(r.stg.dir, in.UploadID)) {
				t.Errorf("staging/%s survives Finalize", in.UploadID)
			}
			if pkgExists(pkg) {
				t.Error("ops/<id>/pkg survives Finalize")
			}
			if tc.name != "a" {
				if _, err2 := r.funcs.Finalize(t.Context(), in, detach.Exit{}, run); !reflect.DeepEqual(
					problemOfErr(t, err2), problemOfErr(t, err)) {
					t.Errorf("second Finalize = %v, want the same %v", err2, err)
				}
			}
		})
	}
}

// A helper that never started changed nothing, and Finalize still
// removes the upload.
func TestUpdateFinalizeHelperNeverStarted(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	r := newUpdateRig(t)
	in := r.prepare(t, "op-ns")
	_, err := r.funcs.Finalize(t.Context(), in, detach.Exit{ExitCode: -1, Error: "exec: permission denied"},
		provider.NewRun("op-ns", nil))
	if perr := problemOfErr(t, err); perr.Slug() != problem.InternalError || !strings.Contains(perr.Detail,
		"nothing was changed") {
		t.Errorf("problem %s %q, want internal-error saying nothing was changed", perr.Slug(), perr.Detail)
	}
	if pkgExists(filepath.Join(r.stg.dir, in.UploadID)) || pkgExists(filepath.Join(r.opsDir, "op-ns", pkgDirName)) {
		t.Error("Finalize left the upload or the package")
	}
}

// laneTest is a second provider with a long-running op on its own lane
// (TC-SM-86).
type laneTest struct {
	op def.Typed[struct{}, laneTestOut]
}

type laneTestOut struct {
	Done bool `json:"done"`
}

var laneTestOp = sync.OnceValue(func() def.Typed[struct{}, laneTestOut] {
	return def.Define[struct{}, laneTestOut](def.Op{
		ID: "lanetest.actions.start", Provider: "lanetest", Kind: def.KindProviderAction, Action: "start",
		Summary: "Hold", Description: "Long-running operation on its own lane, for the nexusd lane matrix.",
		Scope: def.Scope{Name: "lanetest:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI: def.CLIBinding{Noun: "lanetest", Verb: "start"},
	})
})

func (l laneTest) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "lanetest", ReadScope: "lanetest:read", Ops: []def.Op{l.op.Op()}}
}

func (laneTest) Configure(func(any) error) error { return nil }

func (laneTest) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (l laneTest) Bindings() []provider.Binding {
	return []provider.Binding{provider.InProcess(l.op, provider.InProcessSpec[struct{}, laneTestOut]{
		Lane: func(context.Context, struct{}) (provider.Lane, error) {
			return provider.Lane{Key: "lanetest", Policy: provider.LaneReject}, nil
		},
		Run: func(context.Context, struct{}, *provider.Run) (laneTestOut, error) {
			return laneTestOut{Done: true}, nil
		},
	})}
}

// TC-SM-86 (WI 5.3; FR-11): The lane matrix: each of the four ops blocks the others, and other providers are unaffected
func TestTCSM86_TheLaneMatrixEachOfThe(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}, extra: []provider.Provider{
		laneTest{op: laneTestOp()}}})
	laneToken := u.mint("lane", "lanetest:admin", "lanetest:read")
	writeScript(t, u.paths.Prev, `case "$1" in validate-config) exit 0;; -version) echo "nexusd v9.9.0";; esac`)
	pkg := updatePackage(t, "v9.9.2", "v9.9.2")

	var mu sync.Mutex
	var release chan struct{}
	running := make(chan struct{}, 1)
	u.daemon.onReload(func(context.Context) (provider.ReloadResult, error) {
		mu.Lock()
		rel := release
		mu.Unlock()
		running <- struct{}{}
		<-rel
		return provider.ReloadResult{}, nil
	})
	post := func(name string) response {
		if name == "update" {
			return u.update(t, updateOf(u.stage(t, pkg, k), false)) // An existing upload's ID when still staged
		}
		return u.action(t, name, u.admin, nil)
	}
	holders := []string{"reload", "restart", "update", "rollback"}
	for _, h := range holders {
		mu.Lock()
		release = make(chan struct{})
		rel := release
		mu.Unlock()
		var once sync.Once
		unblock := func() { once.Do(func() { close(rel) }) }
		t.Cleanup(unblock)

		// Step 1.
		hid := accepted(t, post(h))
		if h == "reload" {
			select {
			case <-running:
			case <-time.After(10 * time.Second):
				t.Fatal("the reload did not start")
			}
		} else {
			u.spy.await(t, hid)
		}
		for _, req := range holders {
			body := problemOf(t, post(req).want(t, http.StatusConflict).body)
			if slugOf(body) != string(problem.Conflict) || body["blockingOperationId"] != hid {
				t.Errorf("holder %s, request %s: %v, want conflict naming %s", h, req, body, hid)
			}
		}
		oid := accepted(t, u.do(t, http.MethodPost, "/providers/lanetest/actions/start", laneToken, "", nil))
		var other apitypes.Operation
		waitFor(t, "the other provider's op", func() bool {
			other = decodeJSON[apitypes.Operation](t, u.do(t, http.MethodGet, "/operations/"+oid, laneToken, "",
				nil).want(t, http.StatusOK).body)
			return other.Status.Terminal()
		})
		if other.Status != apitypes.StatusSucceeded {
			t.Errorf("holder %s: the other provider's op %s", h, other.Status)
		}

		// Step 2.
		if h == "reload" {
			unblock()
		} else {
			now := time.Now()
			if err := detach.WriteExit(filepath.Join(u.opsDir, hid), detach.Exit{Started: now, Ended: now}); err != nil {
				t.Fatal(err)
			}
		}
		if op := u.awaitOp(t, hid); !op.Status.Terminal() {
			t.Fatalf("holder %s did not end", h)
		}
	}
	if ops := u.operations(t); len(ops) != len(holders) {
		t.Errorf("nexusd operations %d, want only the %d holders: %+v", len(ops), len(holders), ops)
	}
}

// TC-SM-87 (WI 5.3; FR-16): An admitted update's audit facts carry no key material
func TestTCSM87_AnAdmittedUpdateSAuditFacts(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k1, k2 := pkgKey(seedTrusted), pkgKey(seedTrusted2)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k1, k2}})
	pkg := updatePackage(t, "v9.9.2", "v9.9.2")
	id := accepted(t, u.update(t, updateOf(u.stage(t, pkg, k1), false)))
	u.spy.await(t, id)
	bad := updatePackage(t, "v9.9.3", "v9.9.3")
	wantProblem(t, u.update(t, updateOf(u.stage(t, bad, pkgKey(seedUntrusted)), false)),
		http.StatusUnprocessableEntity, problem.UpdateSignatureInvalid)

	// Step 1.
	var all strings.Builder
	err := filepath.WalkDir(u.auditDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		all.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	all.WriteString(u.logs.String())
	text := all.String()
	if !strings.Contains(text, "signature") {
		t.Fatalf("the audit log and slog output hold no update record: %s", text)
	}
	for _, k := range []ed25519.PrivateKey{k1, k2} {
		if strings.Contains(text, keyB64(k)) {
			t.Errorf("key %s appears in the audit log or slog output", keyB64(k))
		}
	}
	if strings.Contains(text, "-----BEGIN") {
		t.Error("a PEM block appears in the audit log or slog output")
	}
	allowed := []string{"uploadId", "sha256", "sizeBytes", "packageVersion", "signature", "allowDowngrade"}
	for _, rec := range u.auditOf(t, spec.Update.Op().ID) {
		for key := range rec.Facts {
			if !slices.Contains(allowed, key) {
				t.Errorf("%s record has fact %s, not one of %v", rec.Outcome, key, allowed)
			}
		}
	}
	if f := u.recordOf(t, "refused").Facts["signature"]; f != "invalid" {
		t.Errorf("refused signature fact %q, want invalid", f)
	}
}

// TC-SM-E10 (WI 5.3; FR-7, ASM-6): A non-comparable running version counts as a downgrade
func TestTCSME10_ANonComparableRunningVersionCounts(t *testing.T) {
	setRunningVersion(t, "dev")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	uploadID := u.stage(t, updatePackage(t, "v9.9.2", "v9.9.2"), k)

	// Step 1.
	body := wantProblem(t, u.update(t, updateOf(uploadID, false)), http.StatusConflict, problem.VersionDowngrade)
	if body["runningVersion"] != "dev" || body["packageVersion"] != "v9.9.2" {
		t.Errorf("problem %v, want runningVersion dev and packageVersion v9.9.2", body)
	}

	// Step 2.
	accepted(t, u.update(t, updateOf(uploadID, true)))
}

// TC-SM-ERR45 (WI 5.3; FR-7, FR-16): Admission reports the first failing check in FR-7 order
func TestTCSMERR45_AdmissionReportsTheFirstFailingCheck(t *testing.T) {
	setRunningVersion(t, "v9.9.3")
	good, bad := pkgKey(seedTrusted), pkgKey(seedUntrusted)
	traversal := pkgFile(updateTop("v9.9.2")+"/../../evil", "x", 0o644)
	for _, tc := range []struct {
		row      int
		noKeys   bool
		pkg      func(t *testing.T) []byte
		key      ed25519.PrivateKey
		unknown  bool
		status   int
		slug     problem.Slug
		check    string
		kept     bool
		zeroRuns bool
	}{
		{row: 1, noKeys: true, key: bad, pkg: func(t *testing.T) []byte {
			return updatePackage(t, "v9.9.2", "v9.9.0", traversal)
		}, status: http.StatusConflict, slug: problem.UpdateNotConfigured, kept: true, zeroRuns: true},
		{row: 2, unknown: true, status: http.StatusUnprocessableEntity, slug: problem.UploadNotFound, zeroRuns: true},
		{row: 3, key: bad, pkg: func(t *testing.T) []byte {
			return updatePackage(t, "v9.9.2", "v9.9.2", traversal)
		}, status: http.StatusUnprocessableEntity, slug: problem.UpdateSignatureInvalid, zeroRuns: true},
		{row: 4, key: good, pkg: func(t *testing.T) []byte {
			return updatePackage(t, "v9.9.2", "v9.9.0", traversal)
		}, status: http.StatusUnprocessableEntity, slug: problem.UpdatePackageInvalid, check: "path", zeroRuns: true},
		{row: 5, key: good, pkg: func(t *testing.T) []byte {
			return updatePackage(t, "v9.9.2", "v9.9.0")
		}, status: http.StatusUnprocessableEntity, slug: problem.UpdatePackageInvalid, check: "version-mismatch"},
		{row: 6, key: good, pkg: func(t *testing.T) []byte {
			return updatePackage(t, "v9.9.2", "v9.9.2")
		}, status: http.StatusConflict, slug: problem.VersionDowngrade, kept: true},
	} {
		t.Run(fmt.Sprintf("row %d", tc.row), func(t *testing.T) {
			cfg := updateStackConfig{keys: []ed25519.PrivateKey{good}}
			if tc.noKeys {
				cfg.keys = nil
			}
			u := newUpdateStack(t, cfg)
			uploadID := "0123456789abcdef0123456789abcdef"
			if !tc.unknown {
				uploadID = u.stage(t, tc.pkg(t), tc.key)
			}

			// Step 1.
			body := wantProblem(t, u.update(t, updateOf(uploadID, false)), tc.status, tc.slug)
			if tc.check != "" && body["check"] != tc.check {
				t.Errorf("check %v, want %s: %v", body["check"], tc.check, body)
			}
			if tc.slug == problem.UploadNotFound && body["uploadId"] != uploadID {
				t.Errorf("problem %v, want uploadId %s", body, uploadID)
			}
			if !tc.unknown && u.staged(uploadID) != tc.kept {
				t.Errorf("upload staged %v, want %v", u.staged(uploadID), tc.kept)
			}
			if ops := u.operations(t); len(ops) != 0 {
				t.Errorf("operations %+v, want none", ops)
			}
			if n := u.run.totalRuns(); tc.zeroRuns && n != 0 {
				t.Errorf("F-RUN ran %d programs, want 0", n)
			}
			if probes, _ := filepath.Glob(filepath.Join(u.stagingDir(), "*", "probe-*")); len(probes) != 0 {
				t.Errorf("probe dirs %v remain", probes)
			}
			rec := u.recordOf(t, "refused")
			if rec.Problem != string(tc.slug) || rec.OperationID != "" {
				t.Errorf("refused record %+v, want %s and no operation", rec, tc.slug)
			}
		})
	}
}

// TC-SM-ERR46 (WI 5.3; FR-7, FR-6): update-not-configured, upload-not-found and a malformed uploadId
func TestTCSMERR46_UpdateNotConfiguredUploadNotFound(t *testing.T) {
	const unknown = "fedcba9876543210fedcba9876543210"

	// Step 1.
	u := newUpdateStack(t, updateStackConfig{})
	wantProblem(t, u.update(t, updateOf(unknown, false)), http.StatusConflict, problem.UpdateNotConfigured)
	if ops := u.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}

	// Step 2.
	u = newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{pkgKey(seedTrusted)}})
	body := wantProblem(t, u.update(t, updateOf(unknown, false)), http.StatusUnprocessableEntity,
		problem.UploadNotFound)
	if body["uploadId"] != unknown {
		t.Errorf("problem %v, want uploadId %s", body, unknown)
	}

	// Step 3.
	wantProblem(t, u.update(t, updateOf("../../etc", false)), http.StatusBadRequest, problem.ValidationFailed)
	if ops := u.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
}

// TC-SM-ERR47 (WI 5.3; FR-7, ASM-6): A downgrade without the flag is refused 409 with both versions
func TestTCSMERR47_ADowngradeWithoutTheFlagIs(t *testing.T) {
	setRunningVersion(t, "v9.9.2")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	uploadID := u.stage(t, updatePackage(t, "v9.9.1", "v9.9.1"), k)

	for _, body := range []string{updateOf(uploadID, false),
		fmt.Sprintf(`{"uploadId":%q,"allowDowngrade":false}`, uploadID)} {
		p := wantProblem(t, u.update(t, body), http.StatusConflict, problem.VersionDowngrade)
		if p["runningVersion"] != "v9.9.2" || p["packageVersion"] != "v9.9.1" {
			t.Errorf("%s: problem %v, want both versions", body, p)
		}
		f := u.recordOf(t, "refused").Facts
		if f["packageVersion"] != "v9.9.1" || f["signature"] != "valid" || f["allowDowngrade"] != "false" {
			t.Errorf("%s: refused facts %v", body, f)
		}
	}
	if !u.staged(uploadID) {
		t.Error("the upload is no longer staged")
	}
	if ops := u.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
}

// TC-SM-ERR48 (WI 5.3; FR-1, FR-5, FR-7, FR-12): Without systemd, restart, update, rollback and the upload are refused capability-unavailable while the reads work
func TestTCSMERR48_WithoutSystemdRestartUpdateRollbackAnd(t *testing.T) {
	const reason = "systemd-run missing"
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{pkgKey(seedTrusted)}, opts: Options{
		Systemd: func() detach.Availability { return detach.Availability{Reason: reason} }}})

	// Step 1.
	pkg := updatePackage(t, "v9.9.2", "v9.9.2")
	for name, r := range map[string]response{
		"restart":  u.action(t, "restart", u.admin, nil),
		"update":   u.update(t, updateOf("0123456789abcdef0123456789abcdef", false)),
		"rollback": u.action(t, "rollback", u.admin, nil),
		"upload":   u.upload(t, u.admin, signBytes(pkgKey(seedTrusted), pkg), pkg),
	} {
		body := wantProblem(t, r, http.StatusServiceUnavailable, problem.CapabilityUnavailable)
		if reasonOf, _ := body["reason"].(string); !strings.Contains(reasonOf, reason) {
			t.Errorf("%s: reason %v, want %q", name, body["reason"], reason)
		}
	}
	if ops := u.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
	if names := stagingEntries(t, u.stagingDir()); len(names) != 0 {
		t.Errorf("staging holds %v", names)
	}

	// Steps 2-4.
	u.getDaemon(t, u.reader)
	u.action(t, "validate-config", u.reader, nil).want(t, http.StatusOK)
	u.reloadAccepted(t, nil)
}

// TC-SM-ERR49 (WI 5.3; FR-7, FR-9): An upload removed between Admit and Prepare fails the op with upload-not-found and no launch
func TestTCSMERR49_AnUploadRemovedBetweenAdmitAnd(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	uploadID := u.stage(t, updatePackage(t, "v9.9.2", "v9.9.2"), k)
	var hookErr error
	u.p.setUpdateHooks(updateHooks{beforePrepare: func(in spec.UpdateInput) {
		hookErr = u.stg(t).remove(in.UploadID) // Step 2
	}})

	// Steps 1 and 3.
	id := accepted(t, u.update(t, updateOf(uploadID, false)))
	op := u.awaitOp(t, id)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if op.Status != apitypes.StatusFailed || op.Error == nil || !strings.HasSuffix(op.Error.Type,
		string(problem.UploadNotFound)) {
		t.Errorf("op %s %+v, want Failed upload-not-found", op.Status, op.Error)
	}
	if n := u.spy.count(); n != 0 {
		t.Errorf("launches %d, want 0", n)
	}
	if pkgExists(filepath.Join(u.opsDir, id, pkgDirName)) {
		t.Error("ops/<id>/pkg exists")
	}
}

// TC-SM-ERR60 (WI 5.3; FR-7, FR-9): A Prepare that fails after extraction removes ops/<id>/pkg and launches nothing
func TestTCSMERR60_APrepareThatFailsAfterExtraction(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	uploadID := u.stage(t, updatePackage(t, "v9.9.2", "v9.9.2"), k)
	var mu sync.Mutex
	var sawPkg []string
	finalized := 0
	u.p.setUpdateHooks(updateHooks{
		beforeInput: func(tmp string) error {
			names, _ := filepath.Glob(filepath.Join(filepath.Dir(tmp), pkgDirName, "*", "*"))
			mu.Lock()
			sawPkg = names
			mu.Unlock()
			return errors.New("injected input.json write failure")
		},
		onFinalize: func() {
			mu.Lock()
			finalized++
			mu.Unlock()
		},
	})

	// Steps 1-2.
	id := accepted(t, u.update(t, updateOf(uploadID, false)))
	op := u.awaitOp(t, id)
	if op.Status != apitypes.StatusFailed {
		t.Errorf("op %s, want Failed", op.Status)
	}
	if outs := outcomes(u.auditOf(t, spec.Update.Op().ID)); slices.Contains(outs, string(apitypes.StatusRunning)) {
		t.Errorf("audit outcomes %v record Running", outs)
	}
	if n := u.spy.count(); n != 0 {
		t.Errorf("launches %d, want 0", n)
	}
	if pkgExists(filepath.Join(u.opsDir, id, pkgDirName)) {
		t.Error("ops/<id>/pkg exists")
	}

	// Step 3.
	mu.Lock()
	defer mu.Unlock()
	if len(sawPkg) != len(updateModes) {
		t.Errorf("the hook saw %v in ops/<id>/pkg, want the extracted package", sawPkg)
	}
	if finalized != 0 {
		t.Errorf("Finalize ran %d times, want 0", finalized)
	}
}

// TC-SM-ERR50 (WI 5.3; CON-4, FR-7): Update input with path, command or url fields is refused 400
func TestTCSMERR50_UpdateInputWithPathCommandOr(t *testing.T) {
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	uploadID := u.stage(t, updatePackage(t, "v9.9.2", "v9.9.2"), k)
	for _, field := range []string{`"path":"/tmp/x"`, `"command":"id"`, `"url":"http://x"`} {
		body := fmt.Sprintf(`{"uploadId":%q,%s}`, uploadID, field)
		r := u.update(t, body)
		p := wantProblem(t, r, http.StatusBadRequest, problem.ValidationFailed)
		name := strings.Trim(strings.SplitN(field, ":", 2)[0], `"`)
		want := []any{map[string]any{"path": "/" + name, "message": "unknown property"}}
		if !reflect.DeepEqual(p["errors"], want) {
			t.Errorf("%s: errors %v, want %v", body, p["errors"], want)
		}
		// Admit adds the uploadId fact first: none means no staging lookup.
		if rec := u.recordOf(t, "refused"); len(rec.Facts) != 0 {
			t.Errorf("%s: refused facts %v, want none: Admit ran", body, rec.Facts)
		}
	}
	if ops := u.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
	if u.run.totalRuns() != 0 {
		t.Error("a program ran")
	}
}

// Update holds the nexusd lane with the reject policy (FR-11).
func TestUpdateLane(t *testing.T) {
	r := newUpdateRig(t)
	l, err := r.funcs.Lane(t.Context(), spec.UpdateInput{})
	if err != nil || l != (provider.Lane{Key: spec.LaneKey, Policy: provider.LaneReject}) {
		t.Errorf("lane %+v %v, want nexusd/reject", l, err)
	}
}

// updateInputGolden is update's input.json as release 1 writes it (A-30):
// members may be added, never renamed or dropped.
const updateInputGolden = `{
  "mode": "update",
  "runningVersion": "v9.9.1",
  "expectedVersion": "v9.9.2",
  "installTimeout": 120000000000,
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

// updateResultGolden are update's result.json files: success, noChange and
// a rolled-back failure.
const (
	updateResultGolden         = `{"changed": true, "rolledBack": false, "previousVersion": "v9.9.1", "newVersion": "v9.9.2"}`
	updateResultNoChangeGolden = `{"changed": false, "rolledBack": false}`
	updateResultFailedGolden   = `{"changed": false, "rolledBack": true, "previousVersion": "v9.9.1",
		"newVersion": "v9.9.2", "error": {"slug": "update-failed", "detail": "install.sh exited 3", "step": "install"}}`
)

// The update input.json and result.json wire formats are pinned in both
// directions.
func TestUpdateWireFormat(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	r := newUpdateRig(t)
	stateDir := r.d.StateDir()
	r.prepare(t, "op-wire")
	got, raw := readInput(t, filepath.Join(r.opsDir, "op-wire"))
	wantWireSuperset(t, raw, strings.ReplaceAll(updateInputGolden, "/var/lib/nexus", stateDir))
	pkgDir := filepath.Join(r.opsDir, "op-wire", pkgDirName, updateTop("v9.9.2"))
	want := helperInput{
		Mode: "update", RunningVersion: "v9.9.1", ExpectedVersion: "v9.9.2", InstallTimeout: 120 * time.Second,
		SavedUnit: "/var/lib/nexus/rollback/nexusd.service",
		Health: healthTarget{URL: "https://127.0.0.1:8443", CertFile: "/var/lib/nexus/tls/cert.pem",
			Timeout: 30 * time.Second, Stability: 5 * time.Second, Poll: time.Second},
		Paths: DefaultPaths(),
	}
	var back helperInput
	if err := json.Unmarshal([]byte(updateInputGolden), &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("the golden input.json decodes to %+v, want %+v", back, want)
	}
	want.PackageDir = pkgDir
	want.SavedUnit = savedUnitPath(stateDir)
	want.Health.CertFile = filepath.Join(stateDir, "tls", "cert.pem")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("input.json decodes to %+v, want %+v", got, want)
	}

	for _, tc := range []struct {
		res    helperResult
		golden string
	}{
		{helperResult{Changed: true, PreviousVersion: "v9.9.1", NewVersion: "v9.9.2"}, updateResultGolden},
		{helperResult{}, updateResultNoChangeGolden},
		{helperResult{RolledBack: true, PreviousVersion: "v9.9.1", NewVersion: "v9.9.2", Error: &helperError{
			Slug: problem.UpdateFailed, Detail: "install.sh exited 3", Step: "install"}}, updateResultFailedGolden},
	} {
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
}

// Prepare re-verifies the upload: a body replaced after Admit fails the
// signature check, and nothing is extracted.
func TestUpdatePrepareReverifies(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	r := newUpdateRig(t)
	in := spec.UpdateInput{UploadID: r.stage(t, updatePackage(t, "v9.9.2", "v9.9.2"))}
	if err := r.funcs.Admit(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	_, dir, ok := r.stg.get(in.UploadID)
	if !ok {
		t.Fatal("the upload is gone")
	}
	body := filepath.Join(dir, stagedFileName)
	if err := os.Chmod(body, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, body, string(updatePackage(t, "v9.9.3", "v9.9.3")))
	_, err := r.funcs.Prepare(t.Context(), in, provider.NewRun("op-swap", nil))
	if perr := problemOfErr(t, err); perr.Slug() != problem.UpdateSignatureInvalid {
		t.Errorf("Prepare problem %s, want update-signature-invalid", perr.Slug())
	}
	if pkgExists(filepath.Join(r.opsDir, "op-swap", pkgDirName)) {
		t.Error("the package was extracted")
	}
}

// A caller that leaves during update admission (CR-1): the probe's failure
// is the cancellation's, not the package's. The upload must survive and
// the audit log must not record a refusal as an invalid package.
func TestUpdateAdmissionCancelledByTheCallerKeepsTheUpload(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	uploadID := u.stage(t, updatePackage(t, "v9.9.2", "v9.9.2"), k)

	started, returned := make(chan struct{}), make(chan struct{})
	var once sync.Once
	u.run.setReply(func(ctx context.Context, argv []string) (RunResult, error) {
		// The real runner on a program that outlives the request, so the
		// error is the one a cancelled -version run returns in production.
		once.Do(func() { close(started) })
		res, err := execRunner{}.Run(ctx, []string{"/bin/sleep", "30"})
		close(returned)
		return res, err
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url+"/v1/providers/nexusd/actions/update",
		strings.NewReader(updateOf(uploadID, false)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+u.admin)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := u.http.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the version probe never started")
	}
	cancel()
	<-done
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the version probe never returned")
	}

	// Wait for the Dispatcher's refusal record: the admission has ended.
	waitFor(t, "the update's refused audit record", func() bool { return len(u.auditOf(t, spec.Update.Op().ID)) >= 1 })
	outs := outcomes(u.auditOf(t, spec.Update.Op().ID))
	if slices.Contains(outs, "refused:"+string(problem.UpdatePackageInvalid)) {
		t.Errorf("audit outcomes %v record the caller's cancellation as an invalid package", outs)
	}
	if !u.staged(uploadID) {
		t.Error("the staged upload was deleted by a cancellation")
	}
}

// Prepare re-run after a crash (CR-2): nexusd died during an extraction, so
// ops/<id>/pkg holds part of the package, and recovery runs Prepare again
// for the same operation. It extracts afresh instead of refusing the valid
// package as having duplicate entries.
func TestUpdatePrepareRerunsOverAPartialExtraction(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	r := newUpdateRig(t)
	in := spec.UpdateInput{UploadID: r.stage(t, updatePackage(t, "v9.9.2", "v9.9.2"))}
	if err := r.funcs.Admit(t.Context(), in); err != nil {
		t.Fatalf("Admit = %v", err)
	}
	id := "op-crashed"
	partial := filepath.Join(r.opsDir, id, pkgDirName, updateTop("v9.9.2"), "nexusd")
	writeFile(t, partial, "a half-written file from the first attempt\n")
	stale := filepath.Join(r.opsDir, id, pkgDirName, "stale-from-first-attempt")
	writeFile(t, stale, "x")

	if _, err := r.funcs.Prepare(t.Context(), in, provider.NewRun(id, nil)); err != nil {
		t.Fatalf("Prepare after a partial extraction = %v, want it to extract afresh", err)
	}
	if got := readFile(t, partial); !strings.HasPrefix(got, "#!/bin/sh") {
		t.Errorf("pkg/<top>/nexusd is %q, want the package's file", got)
	}
	if pkgExists(stale) {
		t.Error("a file of the first attempt survived the second extraction")
	}
}
