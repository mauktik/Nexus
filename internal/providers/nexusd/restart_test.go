// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// restartConfig is TC-SM-65's configuration: listen 127.0.0.1:8443, TLS
// auto and healthTimeout 30s.
const restartConfig = `listen: ["127.0.0.1:8443"]
tls:
  mode: auto
providers:
  docker:
    enabled: false
  nexusd:
    enabled: true
    healthTimeout: 30s
`

// restartExe is the running executable the rig reports.
const restartExe = "/usr/local/bin/nexusd"

// restartRig is the provider pipeline harness of a restart: the provider
// with systemd available and running as its unit, F-DAEMON on a written
// configuration, F-SYS with PID 100 running v9.9.1 ok, F-CLK (the fake's
// virtual clock) and a temp ops dir.
type restartRig struct {
	p        *Provider
	f        *fakeSystem
	d        *fakeDaemon
	cfgPath  string
	stateDir string
	opsDir   string
	funcs    provider.DetachedFuncs
}

// newRestartRig builds the rig. Empty paths means F-SYS's temp-root paths;
// stateDir "" means a temp directory.
func newRestartRig(t *testing.T, paths Paths, stateDir string) *restartRig {
	t.Helper()
	f := newFakeSystem(t, 100)
	f.bootActive(t, "v9.9.1", "ok")
	if paths == (Paths{}) {
		paths = f.paths
	}
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(cfgPath, []byte(restartConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newFakeDaemon(cfgPath, stateDir, time.Now().Add(-time.Minute))
	p := New(Options{
		Host:       StackHost,
		Paths:      paths,
		Systemd:    AvailableSystemd,
		InService:  func() error { return nil },
		Executable: func() (string, error) { return restartExe, nil },
	})
	p.UseDaemon(d)
	opsDir := t.TempDir()
	p.UseOpsDir(opsDir)
	return &restartRig{p: p, f: f, d: d, cfgPath: cfgPath, stateDir: stateDir, opsDir: opsDir,
		funcs: detachedOf(t, p, spec.Restart.Op().ID)}
}

// detachedOf returns the detached functions bound to op id.
func detachedOf(t *testing.T, p *Provider, id string) provider.DetachedFuncs {
	t.Helper()
	for _, b := range p.Bindings() {
		if b.Op().ID != id {
			continue
		}
		funcs, ok := b.Detached()
		if !ok {
			t.Fatalf("%s is bound %s, want detached", id, b.Kind())
		}
		return funcs
	}
	t.Fatalf("%s is not bound", id)
	return provider.DetachedFuncs{}
}

// prepare runs Admit and Prepare of operation id and returns the spec.
func (r *restartRig) prepare(t *testing.T, id string) detach.Spec {
	t.Helper()
	if err := r.funcs.Admit(t.Context(), spec.RestartInput{}); err != nil {
		t.Fatalf("Admit = %v, want nil", err)
	}
	s, err := r.funcs.Prepare(t.Context(), spec.RestartInput{}, provider.NewRun(id, nil))
	if err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	return s
}

// runHelperSpec runs the helper seam with the spec's arguments against F-SYS.
func runHelperSpec(t *testing.T, f *fakeSystem, s detach.Spec) (code int, stdout, stderr string) {
	t.Helper()
	if len(s.Argv) < 2 || s.Argv[1] != HelperCommand {
		t.Fatalf("argv %q does not run the service helper", s.Argv)
	}
	var out, errOut bytes.Buffer
	code = runHelper(s.Argv[2:], &out, &errOut, helperEnv{
		newSystem: func(helperInput) system { return f },
		mode:      modeOf,
	})
	return code, out.String(), errOut.String()
}

// readInput reads and decodes dir/input.json.
func readInput(t *testing.T, dir string) (helperInput, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, inputFileName))
	if err != nil {
		t.Fatal(err)
	}
	var in helperInput
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("input.json: %v: %s", err, b)
	}
	return in, b
}

// problemOfErr extracts the *problem.Error from err.
func problemOfErr(t *testing.T, err error) *problem.Error {
	t.Helper()
	var perr *problem.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v (%T), want a *problem.Error", err, err)
	}
	return perr
}

// hostFiles reads the managed files' contents; a missing file is "".
func hostFiles(t *testing.T, p Paths) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, path := range []string{p.Binary, p.Prev, p.UnitFile} {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		out[path] = string(b)
	}
	return out
}

// TC-SM-65 (WI 4.2; FR-5, FR-9, CON-2): The restart pipeline succeeds and reports the running version
func TestTCSM65_TheRestartPipelineSucceedsAndReports(t *testing.T) {
	r := newRestartRig(t, Paths{}, "")
	const id = "op-65"

	// Steps 1-3.
	s := r.prepare(t, id)
	dir := filepath.Join(r.opsDir, id)
	in, _ := readInput(t, dir)
	if in.Mode != "restart" {
		t.Errorf("input.json mode %q, want restart", in.Mode)
	}
	cfg, errs := checkConfig(t, r.cfgPath)
	if len(errs) != 0 {
		t.Fatalf("the rig's config is invalid: %v", errs)
	}
	wantTarget, err := targetFrom(cfg, r.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if in.Health != wantTarget || in.Health.URL != "https://127.0.0.1:8443" || in.Health.Timeout != 30*time.Second {
		t.Errorf("input.json health %+v, want targetFrom's %+v", in.Health, wantTarget)
	}
	if in.Paths != r.f.paths {
		t.Errorf("input.json paths %+v, want %+v", in.Paths, r.f.paths)
	}
	if want := []string{restartExe, "service-helper", "--dir", dir}; !slices.Equal(s.Argv, want) {
		t.Errorf("argv %q, want %q", s.Argv, want)
	}
	if s.KillMode != detach.KillProcess {
		t.Errorf("kill mode %q, want %q", s.KillMode, detach.KillProcess)
	}
	if want := 30*time.Second + 5*time.Second + 75*time.Second; s.MaxRuntime != want {
		t.Errorf("MaxRuntime %v, want %v", s.MaxRuntime, want)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("the spec does not validate: %v", err)
	}

	// Step 4.
	code, stdout, stderr := runHelperSpec(t, r.f, s)
	if code != 0 {
		t.Fatalf("helper exit %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "restart") {
		t.Errorf("helper steps do not mention the restart: %s", stdout)
	}
	if r.f.starts != 1 {
		t.Errorf("F-SYS starts %d, want 1", r.f.starts)
	}
	if calls := r.f.callsOf("Restart"); len(calls) != 1 || calls[0].arg != "--no-block" {
		t.Errorf("Restart calls %+v, want exactly one --no-block", calls)
	}
	if r.f.mainPID == 100 {
		t.Error("MainPID is still the pre-restart 100")
	}

	// Step 5.
	out, err := r.funcs.Finalize(t.Context(), spec.RestartInput{}, detach.Exit{ExitCode: 0}, provider.NewRun(id, nil))
	if err != nil {
		t.Fatalf("Finalize = %v", err)
	}
	want := spec.RestartResult{ActionResult: apitypes.ActionResult{Changed: true}, Version: "v9.9.1"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("result %+v, want %+v", out, want)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"changed":true,"version":"v9.9.1"}` {
		t.Errorf("result JSON %s", got)
	}
}

// TC-SM-66 (WI 4.2; FR-11): restart while a reload holds the lane gets 409 naming the blocking op
func TestTCSM66_RestartWhileAReloadHoldsThe(t *testing.T) {
	s := newStack(t, stackConfig{})
	running := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // Before the stack's cleanup: a failed test must not leave the reload blocked
	s.daemon.onReload(func(context.Context) (provider.ReloadResult, error) {
		running <- struct{}{}
		<-release
		return provider.ReloadResult{}, nil
	})

	// Step 1.
	rid := s.reloadAccepted(t, nil)
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("the reload did not start running")
	}

	// Step 2.
	resp := s.action(t, "restart", s.admin, nil).want(t, http.StatusConflict)
	body := problemOf(t, resp.body)
	if slug := slugOf(body); slug != string(problem.Conflict) || body["blockingOperationId"] != rid {
		t.Errorf("problem %v, want conflict with blockingOperationId %s", body, rid)
	}
	if ops := s.operations(t); len(ops) != 1 || ops[0].ID != rid {
		t.Errorf("operations %+v, want only the reload %s", ops, rid)
	}
	if outs := outcomes(s.auditOf(t, spec.Restart.Op().ID)); !slices.Contains(outs, "refused:conflict") {
		t.Errorf("restart audit outcomes %v, want a refused conflict record", outs)
	}

	// Step 3.
	resp = s.do(t, http.MethodPost, "/operations/"+rid+"/cancel", s.admin, "", nil).want(t, http.StatusConflict)
	if slug := slugOf(problemOf(t, resp.body)); slug != string(problem.NotCancelable) {
		t.Errorf("cancel slug %s, want not-cancelable", slug)
	}

	// Step 4.
	unblock()
	if op := s.awaitOp(t, rid); op.Status != apitypes.StatusSucceeded {
		t.Errorf("reload %s, error %+v, want Succeeded", op.Status, op.Error)
	}
}

// TC-SM-ERR32 (WI 4.2; FR-5, FR-10): A crash-looping restart fails with restart-unhealthy and no rollback
func TestTCSMERR32_ACrashLoopingRestartFailsWith(t *testing.T) {
	r := newRestartRig(t, Paths{}, "")
	writeFakeBinary(t, r.f.paths.Binary, "v9.9.1", "crash-after=3s")
	before := hostFiles(t, r.f.paths)
	const id = "op-err32"

	// Steps 1-2.
	s := r.prepare(t, id)

	// Step 3.
	code, _, stderr := runHelperSpec(t, r.f, s)
	if code != 0 {
		t.Fatalf("helper exit %d, want 0; stderr: %s", code, stderr)
	}

	// Step 4.
	_, err := r.funcs.Finalize(t.Context(), spec.RestartInput{}, detach.Exit{ExitCode: 0}, provider.NewRun(id, nil))
	perr := problemOfErr(t, err)
	if perr.Slug() != problem.RestartUnhealthy {
		t.Fatalf("problem %s (%s), want restart-unhealthy", perr.Slug(), perr.Detail)
	}
	if c := perr.Extensions["condition"]; c != 4 && c != 1 {
		t.Errorf("condition %v, want 4 (or 1 at the start limit); detail %s", c, perr.Detail)
	}
	if v, ok := perr.Extensions["version"]; ok && v != "" && v != "v9.9.1" {
		t.Errorf("version %v, want empty or the last observed version", v)
	}
	if !strings.Contains(perr.Detail, "condition") {
		t.Errorf("detail %q does not name the failed condition", perr.Detail)
	}
	if n := len(r.f.callsOf("DaemonReload")); n != 0 {
		t.Errorf("DaemonReload calls %d, want 0", n)
	}
	if n := len(r.f.callsOf("ResetFailed")); n != 0 {
		t.Errorf("ResetFailed calls %d, want 0", n)
	}
	if r.f.starts != 1 || len(r.f.callsOf("Restart")) != 1 {
		t.Errorf("starts %d, Restart calls %d; want one explicit start and no second one", r.f.starts,
			len(r.f.callsOf("Restart")))
	}
	if after := hostFiles(t, r.f.paths); !reflect.DeepEqual(after, before) {
		t.Errorf("the helper changed host files: %v, was %v", after, before)
	}
}

// A restart of a unit at its start limit: systemctl restart --no-block
// returns 0 and systemd refuses the start later, as systemd 259 does
// (TC-SM-129), so the unit stays failed and the health check fails with
// restart-unhealthy condition 1; the helper still exits 0 with a result.
func TestRestartRefusedBySystemd(t *testing.T) {
	r := newRestartRig(t, Paths{}, "")
	r.f.reachStartLimit(t)
	// A healthy binary again: only the start limit can keep it from running.
	writeFakeBinary(t, r.f.paths.Binary, "v9.9.1", "ok")
	const id = "op-limited"
	s := r.prepare(t, id)
	if code, _, stderr := runHelperSpec(t, r.f, s); code != 0 {
		t.Fatalf("helper exit %d, want 0; stderr: %s", code, stderr)
	}
	_, err := r.funcs.Finalize(t.Context(), spec.RestartInput{}, detach.Exit{}, provider.NewRun(id, nil))
	perr := problemOfErr(t, err)
	if perr.Slug() != problem.RestartUnhealthy || perr.Extensions["condition"] != 1 ||
		!strings.Contains(perr.Detail, "ActiveState=failed") {
		t.Errorf("problem %s %v %q, want restart-unhealthy condition 1 with ActiveState=failed", perr.Slug(),
			perr.Extensions, perr.Detail)
	}
	if n := len(r.f.callsOf("Restart")); n != 2 {
		t.Errorf("Restart calls %d, want 2 (reaching the limit, then the helper's)", n)
	}
	if n := len(r.f.callsOf("Healthz")); n != 0 {
		t.Errorf("the helper checked health %d times of a unit that never became active", n)
	}
}

// TC-SM-ERR33 (WI 4.2; FR-5): restart is refused with config-invalid when the on-disk config is invalid
func TestTCSMERR33_RestartIsRefusedWithConfigInvalid(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad-listen.yaml")
	if err := os.WriteFile(bad, []byte("listen: [\"not-an-address\"]\ntls: {mode: auto}\n"+
		"providers: {docker: {enabled: false}, nexusd: {enabled: true}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errs := checkConfig(t, bad); len(errs) == 0 {
		t.Fatal("bad-listen.yaml passes CheckConfig")
	}
	s := newStack(t, stackConfig{daemon: newFakeDaemon(bad, t.TempDir(), time.Now())})
	opsDir := t.TempDir()
	s.p.UseOpsDir(opsDir)

	// Step 1.
	resp := s.action(t, "restart", s.admin, nil).want(t, http.StatusUnprocessableEntity)
	body := problemOf(t, resp.body)
	if slug := slugOf(body); slug != string(problem.ConfigInvalid) {
		t.Fatalf("slug %s, want config-invalid", slug)
	}
	if errs, _ := body["errors"].([]any); len(errs) == 0 || body["configPath"] != bad {
		t.Errorf("problem %v, want the errors of %s", body, bad)
	}
	if ops := s.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
	if names := dirNames(t, opsDir); len(names) != 0 {
		t.Errorf("ops dir %v, want empty: nothing prepared or launched", names)
	}
	if outs := outcomes(s.auditOf(t, spec.Restart.Op().ID)); !slices.Contains(outs, "refused:config-invalid") {
		t.Errorf("audit outcomes %v, want a refused config-invalid record", outs)
	}
}

// A config that turns invalid between Admit and Prepare fails Prepare with
// config-invalid and writes no input.json.
func TestRestartPrepareRechecksTheConfig(t *testing.T) {
	r := newRestartRig(t, Paths{}, "")
	if err := r.funcs.Admit(t.Context(), spec.RestartInput{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.cfgPath, []byte("listen: [\"not-an-address\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.funcs.Prepare(t.Context(), spec.RestartInput{}, provider.NewRun("op-p", nil))
	if perr := problemOfErr(t, err); perr.Slug() != problem.ConfigInvalid {
		t.Errorf("Prepare problem %s, want config-invalid", perr.Slug())
	}
	if _, err := os.Stat(filepath.Join(r.opsDir, "op-p", inputFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("input.json after a refused Prepare: %v", err)
	}
}

// TC-SM-ERR34 (WI 4.2; FR-9, FR-5): A restart Finalize without result.json is outcome-unknown with recovery guidance
func TestTCSMERR34_ARestartFinalizeWithoutResultJson(t *testing.T) {
	r := newRestartRig(t, DefaultPaths(), "")
	const id = "op-err34"
	r.prepare(t, id) // input.json, and no result.json
	run := provider.NewRun(id, nil)
	guidance := []string{"systemctl status nexusd.service", "/usr/local/bin/nexusd -version", "nexusd.prev"}

	// Step 1.
	for _, x := range []detach.Exit{{ExitCode: 0}, {ExitCode: -1, Signal: "SIGKILL"}} {
		_, err := r.funcs.Finalize(t.Context(), spec.RestartInput{}, x, run)
		perr := problemOfErr(t, err)
		if perr.Slug() != problem.OutcomeUnknown {
			t.Errorf("exit %+v: problem %s, want outcome-unknown", x, perr.Slug())
		}
		for _, g := range guidance {
			if !strings.Contains(perr.Detail, g) {
				t.Errorf("exit %+v: detail %q lacks %q", x, perr.Detail, g)
			}
		}
	}

	// Step 2: a lost unit. The ops manager calls only LostDetail and
	// appends it to its outcome-unknown (ops.Manager.detachedEnd).
	detail := r.funcs.LostDetail(run, spec.RestartInput{})
	for _, g := range guidance {
		if !strings.Contains(detail, g) {
			t.Errorf("LostDetail %q lacks %q", detail, g)
		}
	}

	// Without input.json, the guidance names the provider's own paths.
	if d := r.funcs.LostDetail(provider.NewRun("op-none", nil), spec.RestartInput{}); !strings.Contains(d,
		"systemctl status nexusd.service") || !strings.Contains(d, "/usr/local/bin/nexusd.prev") {
		t.Errorf("LostDetail without input.json %q", d)
	}

	// A helper that never started changed nothing.
	_, err := r.funcs.Finalize(t.Context(), spec.RestartInput{}, detach.Exit{ExitCode: -1, Error: "exec: permission denied"}, run)
	if perr := problemOfErr(t, err); perr.Slug() != problem.InternalError ||
		!strings.Contains(perr.Detail, "not restarted") {
		t.Errorf("never started: %s %q, want internal-error saying nexusd was not restarted", perr.Slug(), perr.Detail)
	}
}

// TC-SM-E14 step 2 (carried from WI 3.5; FR-1): over REST, restart is
// refused 503 capability-unavailable, with the cgroup reason, when nexusd
// does not run as nexusd.service; no op is created and the configuration
// is not even checked.
func TestTCSME14_RESTRestartRefusedOutsideTheUnit(t *testing.T) {
	const reason = "not running in nexusd.service (cgroup /user.slice/x.scope)"
	s := newStack(t, stackConfig{opts: Options{InService: func() error { return errors.New(reason) }}})

	resp := s.action(t, "restart", s.admin, nil).want(t, http.StatusServiceUnavailable)
	body := problemOf(t, resp.body)
	if slugOf(body) != string(problem.CapabilityUnavailable) || body["reason"] != reason ||
		body["provider"] != spec.Name {
		t.Errorf("problem %v, want capability-unavailable with reason %q", body, reason)
	}
	if ops := s.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
	s.daemon.mu.Lock()
	calls := slices.Clone(s.daemon.calls)
	s.daemon.mu.Unlock()
	if slices.Contains(calls, "CheckConfig") {
		t.Errorf("daemon calls %v: the configuration was checked before the self-management refusal", calls)
	}
}

// Restart holds the nexusd lane with the reject policy (FR-11), and the
// provider takes the ops directory from the daemon.
func TestRestartLaneAndOpsDir(t *testing.T) {
	r := newRestartRig(t, Paths{}, "")
	l, err := r.funcs.Lane(t.Context(), spec.RestartInput{})
	if err != nil || l != (provider.Lane{Key: spec.LaneKey, Policy: provider.LaneReject}) {
		t.Errorf("lane %+v %v, want nexusd/reject", l, err)
	}
	var _ provider.OpsDirUser = r.p

	p := New(Options{Systemd: AvailableSystemd, InService: func() error { return nil }})
	p.UseDaemon(r.d)
	_, err = detachedOf(t, p, spec.Restart.Op().ID).Prepare(t.Context(), spec.RestartInput{},
		provider.NewRun("op-x", nil))
	if err == nil || !strings.Contains(err.Error(), "UseOpsDir") {
		t.Errorf("Prepare without an ops dir = %v, want an error naming UseOpsDir", err)
	}
}

// restartInputGolden is restart's input.json as release 1 writes it (A-30):
// a later release may add members but must keep each of these, with these
// names and meanings, so the helper of either release reads the other's
// file. Paths has no JSON tags: its Go field names are its wire names.
const restartInputGolden = `{
  "mode": "restart",
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

// restartResultGolden are restart's result.json files, success and failure.
const (
	restartResultGolden       = `{"changed": true, "rolledBack": false, "version": "v9.9.1"}`
	restartResultFailedGolden = `{"changed": false, "rolledBack": false, "error": {"slug": "restart-unhealthy",
		"detail": "nexusd was not healthy within 30s: condition 4", "condition": 4}}`
)

// wantWireSuperset fails unless got holds every member of golden, nested,
// with the same value: members may be added, never renamed or dropped.
func wantWireSuperset(t *testing.T, got []byte, golden string) {
	t.Helper()
	var g, w map[string]any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got: %v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(golden), &w); err != nil {
		t.Fatalf("golden: %v", err)
	}
	var walk func(path string, want, have map[string]any)
	walk = func(path string, want, have map[string]any) {
		for k, wv := range want {
			hv, ok := have[k]
			if !ok {
				t.Errorf("wire member %s%s is missing: %s", path, k, got)
				continue
			}
			if wm, ok := wv.(map[string]any); ok {
				hm, ok := hv.(map[string]any)
				if !ok {
					t.Errorf("wire member %s%s is %v, want an object", path, k, hv)
					continue
				}
				walk(path+k+".", wm, hm)
				continue
			}
			if !reflect.DeepEqual(hv, wv) {
				t.Errorf("wire member %s%s = %v, want %v", path, k, hv, wv)
			}
		}
	}
	walk("", w, g)
}

// The restart input.json wire format is pinned in both directions: what
// Prepare writes holds every golden member, and the golden decodes into the
// values a helper acts on.
func TestRestartInputWireFormat(t *testing.T) {
	r := newRestartRig(t, DefaultPaths(), "/var/lib/nexus")
	r.prepare(t, "op-wire")
	got, raw := readInput(t, filepath.Join(r.opsDir, "op-wire"))
	wantWireSuperset(t, raw, restartInputGolden)

	want := helperInput{
		Mode: "restart",
		Health: healthTarget{URL: "https://127.0.0.1:8443", CertFile: "/var/lib/nexus/tls/cert.pem",
			Timeout: 30 * time.Second, Stability: 5 * time.Second, Poll: time.Second},
		Paths: DefaultPaths(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("input.json decodes to %+v, want %+v", got, want)
	}
	var golden helperInput
	if err := json.Unmarshal([]byte(restartInputGolden), &golden); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(golden, want) {
		t.Errorf("the golden input.json decodes to %+v, want %+v", golden, want)
	}
}

// The restart result.json wire format is pinned the same way.
func TestRestartResultWireFormat(t *testing.T) {
	ok := helperResult{Changed: true, Version: "v9.9.1"}
	failed := helperResult{Error: &helperError{Slug: problem.RestartUnhealthy,
		Detail: "nexusd was not healthy within 30s: condition 4", Condition: 4}}
	for _, tc := range []struct {
		res    helperResult
		golden string
	}{{ok, restartResultGolden}, {failed, restartResultFailedGolden}} {
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
