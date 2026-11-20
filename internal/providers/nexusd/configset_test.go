// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-71, TC-RA-78 to
// TC-RA-83, TC-RA-E22, TC-RA-E23, TC-RA-E30, TC-RA-ERR26 to TC-RA-ERR31 and
// TC-RA-ERR43 (WI 4.3): config set's admission, Prepare, Finalize and
// availability. REST cases run on the T0 stack with the launch spy; the
// cases that must step between admission and Prepare, or need the real
// helper, call the erased binding directly over the WI 4.2 rig (csRig).

import (
	"context"
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
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/systemspec"
)

// csStack is the T0 stack with the launch spy and the daemon's ConfigPath
// on a temp file.
type csStack struct {
	*stack
	spy     *launchSpy
	cfgPath string
}

func newCSStack(t *testing.T, body []byte, cfg stackConfig) *csStack {
	t.Helper()
	spy := &launchSpy{}
	cfg.launch = spy.launch
	s := newStack(t, cfg)
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	s.daemon.setConfigPath(path)
	return &csStack{stack: s, spy: spy, cfgPath: path}
}

// set POSTs update-config with changes.
func (c *csStack) set(t *testing.T, token string, header http.Header, changes ...string) response {
	t.Helper()
	b, err := json.Marshal(map[string]any{"changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	return c.do(t, http.MethodPost, "/providers/nexusd/actions/update-config", token, string(b), header)
}

// noChange wants a 200 changed:false answer and returns its result.
func (c *csStack) noChange(t *testing.T, r response) spec.ConfigUpdateResult {
	t.Helper()
	r.want(t, http.StatusOK)
	res := decodeJSON[spec.ConfigUpdateResult](t, r.body)
	if res.Changed || res.RestartRequired == nil || len(res.RestartRequired) != 0 || res.BackupPath != "" {
		t.Errorf("result %s, want changed false, restartRequired [] and no backupPath", r.body)
	}
	if !strings.Contains(string(r.body), `"restartRequired":[]`) {
		t.Errorf("body %s, want restartRequired []", r.body)
	}
	if r.header.Get("Location") != "" || r.header.Get("Retry-After") != "" {
		t.Errorf("headers %v, want no Location or Retry-After", r.header)
	}
	return res
}

// invalid wants a 422 config-invalid naming the config path and returns
// its errors.
func (c *csStack) invalid(t *testing.T, r response) []spec.ConfigError {
	t.Helper()
	m := wantProblem(t, r, http.StatusUnprocessableEntity, problem.ConfigInvalid)
	if m["configPath"] != c.cfgPath {
		t.Errorf("configPath %v, want %s", m["configPath"], c.cfgPath)
	}
	return decodeJSON[struct {
		Errors []spec.ConfigError `json:"errors"`
	}](t, r.body).Errors
}

// validations is how many times the daemon's byte check ran.
func validations(d *fakeDaemon) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if c == "CheckConfigData" {
			n++
		}
	}
	return n
}

// opsEntries lists the ops directory, empty when it does not exist.
func opsEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// wantNoSideEffects asserts that no operation, launch, write or backup
// happened since the stack started.
func (c *csStack) wantNoSideEffects(t *testing.T, sha string) {
	t.Helper()
	if ops := c.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
	if n := c.spy.count(); n != 0 {
		t.Errorf("launches %d, want 0", n)
	}
	if got := shaHex(readBytes(t, c.cfgPath)); got != sha {
		t.Errorf("the config file changed: sha %s, want %s", got, sha)
	}
	if _, err := os.Lstat(c.cfgPath + ".bak"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s.bak exists (%v), want none", c.cfgPath, err)
	}
}

// endOp writes operation id's exit.json, and its result.json when res is
// set, and waits for it to end.
func (c *csStack) endOp(t *testing.T, id string, x detach.Exit, res *helperResult) apitypes.Operation {
	t.Helper()
	dir := filepath.Join(c.opsDir, id)
	if res != nil {
		if err := writeResult(dir, *res); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	x.Started, x.Ended = now, now
	if err := detach.WriteExit(dir, x); err != nil {
		t.Fatal(err)
	}
	return c.awaitOp(t, id)
}

// factsOf returns the facts of the last record of the update-config op
// with outcome.
func (c *csStack) factsOf(t *testing.T, outcome string) map[string]string {
	t.Helper()
	recs := c.auditOf(t, spec.UpdateConfig.Op().ID)
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Outcome == outcome {
			return recs[i].Facts
		}
	}
	t.Fatalf("no %s record in %v", outcome, outcomes(recs))
	return nil
}

// csDirect is the erased config-set binding of a provider whose daemon
// serves the csRig's file, with the rig's operation directory as the
// operation's (TC-RA-E22's direct-call method).
type csDirect struct {
	rig   *csRig
	p     *Provider
	d     *fakeDaemon
	funcs provider.DetachedFuncs
	run   *provider.Run
}

func newCSDirect(t *testing.T, changes ...string) *csDirect {
	t.Helper()
	rig := newCSRig(t, 0o600, changes...)
	d := newFakeDaemon(rig.cfgPath, rig.stateDir, time.Now().Add(-time.Minute))
	p := New(Options{Host: StackHost, Paths: rig.f.paths, Systemd: AvailableSystemd,
		InService: func() error { return nil }, Executable: func() (string, error) { return restartExe, nil }})
	p.UseDaemon(d)
	p.UseOpsDir(filepath.Dir(rig.dir))
	return &csDirect{rig: rig, p: p, d: d, funcs: detachedOf(t, p, spec.UpdateConfig.Op().ID),
		run: provider.NewRun(filepath.Base(rig.dir), nil)}
}

// admit runs the Admission hook and wants a change: the input Prepare runs on.
func (c *csDirect) admit(t *testing.T, changes ...string) spec.ConfigUpdateInput {
	t.Helper()
	if c.funcs.Admission == nil {
		t.Fatal("update-config has no Admission hook")
	}
	done, out, next, err := c.funcs.Admission(t.Context(), spec.ConfigUpdateInput{Changes: changes})
	if err != nil || done {
		t.Fatalf("Admission = done %t, %+v, %v; want a change", done, out, err)
	}
	in, ok := next.(spec.ConfigUpdateInput)
	if !ok {
		t.Fatalf("Admission's input is %T, want spec.ConfigUpdateInput", next)
	}
	return in
}

// wantNothingPrepared asserts Prepare's refusal left nothing behind: no
// input.json, no backup, the disk at sha, and no reload.
func (c *csDirect) wantNothingPrepared(t *testing.T, s detach.Spec, sha string) {
	t.Helper()
	if !reflect.DeepEqual(s, detach.Spec{}) {
		t.Errorf("Prepare returned spec %+v, want none", s)
	}
	if _, err := os.Lstat(filepath.Join(c.rig.dir, inputFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("input.json exists (%v), want none", err)
	}
	if got := shaHex(c.rig.disk(t)); got != sha {
		t.Errorf("disk sha %s, want %s", got, sha)
	}
	c.rig.wantNoBackup(t)
	if c.rig.rl.reloadCalls != 0 {
		t.Errorf("reloadCalls %d, want 0", c.rig.rl.reloadCalls)
	}
}

// TC-RA-71: A config set that changes nothing is answered at admission with changed:false, with no operation and no helper unit. Owner WI 4.3. Requirements: FR-7.
func TestTCRA71_AConfigSetThatChangesNothingIsAnsweredAtAdmission(t *testing.T) {
	data := defaultConfigWithNexusd(t)
	c := newCSStack(t, data, stackConfig{})
	fi0, err := os.Stat(c.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	sha := shaHex(data)
	var doc struct {
		Limits struct {
			OutputCapBytes int64 `yaml:"outputCapBytes"`
		} `yaml:"limits"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	capBytes := fmt.Sprint(doc.Limits.OutputCapBytes)

	// Step 1.
	res := c.noChange(t, c.set(t, c.admin, nil, "providers.nexusd.enabled=true"))
	if len(res.Changes) != 1 || res.Changes[0].Key != "providers.nexusd.enabled" || res.Changes[0].Old != true ||
		res.Changes[0].New != true {
		t.Errorf("changes %+v, want providers.nexusd.enabled true -> true", res.Changes)
	}

	// Step 2.
	res = c.noChange(t, c.set(t, c.admin, nil, "providers.nexusd.enabled=true", "limits.outputCapBytes="+capBytes))
	if len(res.Changes) != 2 || res.Changes[1].Key != "limits.outputCapBytes" ||
		res.Changes[1].Old != float64(doc.Limits.OutputCapBytes) || res.Changes[1].Old != res.Changes[1].New {
		t.Errorf("changes %+v, want old == new == %s for limits.outputCapBytes", res.Changes, capBytes)
	}

	// Step 3.
	key := http.Header{"Idempotency-Key": []string{"ra-noop-1"}}
	r1 := c.set(t, c.admin, key, "providers.nexusd.enabled=true")
	r2 := c.set(t, c.admin, key, "providers.nexusd.enabled=true")
	c.noChange(t, r1)
	c.noChange(t, r2)
	if string(r1.body) != string(r2.body) {
		t.Errorf("idempotent no-op bodies differ:\n%s\n%s", r1.body, r2.body)
	}
	// No idempotency entry: a stored key would answer the second call as a
	// replay of an operation, audited replayed (A-36).
	got := outcomes(c.auditOf(t, spec.UpdateConfig.Op().ID))
	if want := []string{"requested", "completed", "requested", "completed", "requested", "completed",
		"requested", "completed"}; !slices.Equal(got, want) {
		t.Errorf("audit %v, want %v", got, want)
	}

	// No operation, unit, write or reload.
	if names := opsEntries(t, c.opsDir); len(names) != 0 {
		t.Errorf("ops dir holds %v, want nothing", names)
	}
	c.wantNoSideEffects(t, sha)
	fi1, err := os.Stat(c.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if keyOf(fi0) != keyOf(fi1) {
		t.Errorf("file key %+v -> %+v: the file was rewritten", keyOf(fi0), keyOf(fi1))
	}
	if n := c.daemon.reloadCount(); n != 0 {
		t.Errorf("reloads %d, want 0", n)
	}
}

// TC-RA-78: Admission accepts a valid change, and Prepare writes the config-set input. Owner WI 4.3. Requirements: FR-7, ASM-7.
func TestTCRA78_AdmissionAcceptsAValidChangeAndPrepareWritesThe(t *testing.T) {
	data := defaultConfigWithNexusd(t)
	c := newCSStack(t, data, stackConfig{})
	orig := shaHex(data)
	checks := validations(c.daemon)

	// Step 1.
	id := accepted(t, c.set(t, c.admin, nil, "providers.host.enabled=true"))
	c.spy.await(t, id)
	if n := c.spy.count(); n != 1 {
		t.Errorf("launches %d, want 1", n)
	}
	dir := filepath.Join(c.opsDir, id)
	sp := readSpecFile(t, dir)
	if want := []string{"/x/nexusd", HelperCommand, "--dir", dir}; !slices.Equal(sp.Argv, want) {
		t.Errorf("argv %q, want %q", sp.Argv, want)
	}
	if sp.KillMode != string(detach.KillProcess) {
		t.Errorf("kill mode %q, want process", sp.KillMode)
	}
	if got := time.Duration(sp.MaxRuntimeNS); got != configSetMaxRuntime {
		t.Errorf("MaxRuntime %v, want configSetMaxRuntime %v", got, configSetMaxRuntime)
	}
	if configSetMaxRuntime != 2*reloadTimeout+30*time.Second {
		t.Errorf("configSetMaxRuntime %v, want 2*reloadTimeout + 30s (A-5)", configSetMaxRuntime)
	}
	if configSetMaxRuntime != 54*time.Second {
		t.Errorf("configSetMaxRuntime %v, want 54s (A-5)", configSetMaxRuntime)
	}

	// Step 2.
	in, raw := readInput(t, dir)
	want := helperInput{Mode: modeConfigSet, ConfigPath: c.cfgPath, BackupPath: c.cfgPath + ".bak",
		Changes: []string{"providers.host.enabled=true"}, BaseSHA256: orig, StateDir: c.stateDir,
		ReloadTimeout: reloadTimeout, Paths: c.paths}
	if !reflect.DeepEqual(in, want) {
		t.Errorf("input.json\n got %+v\nwant %+v", in, want)
	}
	if reloadTimeout != 12*time.Second {
		t.Errorf("reloadTimeout %v, want 12s", reloadTimeout)
	}
	if strings.Contains(string(raw), "noChange") {
		t.Errorf("input.json %s carries noChange", raw)
	}

	// Prepare does not re-edit or re-validate.
	if n := validations(c.daemon) - checks; n != 1 {
		t.Errorf("CheckConfigData ran %d times, want once, at admission", n)
	}

	// Audit.
	recs := c.auditOf(t, spec.UpdateConfig.Op().ID)
	if len(recs) == 0 || recs[0].Outcome != audit.OutcomeRequested {
		t.Fatalf("audit %v, want requested first", outcomes(recs))
	}
	var params map[string]any
	if err := json.Unmarshal(recs[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(params, map[string]any{"changes": []any{"providers.host.enabled=true"}}) {
		t.Errorf("requested params %s, want the changes alone", recs[0].Params)
	}
}

// TC-RA-79: providers.nexusd.enabled=true is accepted although false is denied. Owner WI 4.3. Requirements: FR-12, ASM-3.
func TestTCRA79_ProvidersNexusdEnabledTrueIsAcceptedAlthough(t *testing.T) {
	body := "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: \"off\"\nproviders:\n  nexusd:\n    enabled: true\n"
	c := newCSStack(t, []byte(body), stackConfig{})

	// Step 1.
	c.noChange(t, c.set(t, c.admin, nil, "providers.nexusd.enabled=true"))
	if ops := c.operations(t); len(ops) != 0 || c.spy.count() != 0 {
		t.Errorf("operations %d, launches %d; want none", len(ops), c.spy.count())
	}

	// Steps 2 and 3.
	for i, change := range []string{"host.name=x", "tls.mode=auto"} {
		r := c.set(t, c.admin, nil, change)
		if strings.Contains(string(r.body), "cannot be set remotely") {
			t.Errorf("%s: %s", change, r.body)
		}
		id := accepted(t, r)
		c.spy.await(t, id)
		if n := c.spy.count(); n != i+1 {
			t.Errorf("%s: launches %d, want %d", change, n, i+1)
		}
		c.endOp(t, id, detach.Exit{ExitCode: 137}, nil)
	}
}

// TC-RA-80: A helper that dies without a result ends outcome-unknown with a detail naming both files. Owner WI 4.3. Requirements: NFR-4, FR-8.
func TestTCRA80_AHelperThatDiesWithoutAResultEndsOutcomeUnknown(t *testing.T) {
	c := newCSStack(t, defaultConfigWithNexusd(t), stackConfig{})
	guidance := []string{c.cfgPath, c.cfgPath + ".bak", "nexusd validate-config", "nexus nexusdconfig get"}

	// Steps 1 to 3.
	id := accepted(t, c.set(t, c.admin, nil, "providers.host.enabled=true"))
	c.spy.await(t, id)
	op := c.endOp(t, id, detach.Exit{ExitCode: 137}, nil)
	if op.Status != apitypes.StatusFailed || op.Error == nil || op.Error.Slug() != problem.OutcomeUnknown {
		t.Fatalf("operation %s %+v, want failed outcome-unknown", op.Status, op.Error)
	}
	for _, g := range guidance {
		if !strings.Contains(op.Error.Detail, g) {
			t.Errorf("detail %q lacks %q", op.Error.Detail, g)
		}
	}

	// A unit lost without exit.json: the ops manager appends LostDetail.
	funcs := detachedOf(t, c.p, spec.UpdateConfig.Op().ID)
	if funcs.LostDetail == nil {
		t.Fatal("update-config has no LostDetail")
	}
	for _, run := range []*provider.Run{provider.NewRun(id, nil), provider.NewRun("op-none", nil)} {
		d := funcs.LostDetail(run, spec.ConfigUpdateInput{})
		for _, g := range guidance {
			if !strings.Contains(d, g) {
				t.Errorf("LostDetail(%s) %q lacks %q", run.ID(), d, g)
			}
		}
	}
}

// availSeam is a Systemd seam a test flips.
type availSeam struct {
	mu sync.Mutex
	a  detach.Availability
}

func (s *availSeam) get() detach.Availability {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.a
}

func (s *availSeam) set(a detach.Availability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.a = a
}

// openRC is the Systemd seam's answer on an OpenRC host.
const openRCReason = "requires systemd (detected openrc)"

// TC-RA-81: nexus nexusd get reports configSet next to selfManagement, and it follows availability. Owner WI 4.3. Requirements: FR-9.
func TestTCRA81_NexusNexusdGetReportsConfigSetNextTo(t *testing.T) {
	seam := &availSeam{a: detach.Availability{Available: true}}
	s := newStack(t, stackConfig{opts: Options{Systemd: seam.get}})

	// Step 1.
	p := s.getDaemon(t, s.reader).Properties
	if !p.ConfigSet.Available || p.ConfigSet != p.SelfManagement {
		t.Errorf("configSet %+v selfManagement %+v, want both available", p.ConfigSet, p.SelfManagement)
	}

	// Steps 2 and 3.
	seam.set(detach.Availability{Reason: openRCReason})
	p = s.getDaemon(t, s.reader).Properties
	if p.ConfigSet.Available || p.ConfigSet.Reason != openRCReason || p.ConfigSet != p.SelfManagement {
		t.Errorf("configSet %+v selfManagement %+v, want both unavailable with %q", p.ConfigSet, p.SelfManagement,
			openRCReason)
	}
}

// TC-RA-82: The requested record carries the new values in params and the old values and backup path in facts. Owner WI 4.3. Requirements: FR-13, ASM-7.
func TestTCRA82_TheRequestedRecordCarriesTheNewValuesInParamsAnd(t *testing.T) {
	long := strings.Repeat("n", 300)
	body := "host:\n  name: " + long + "\nlisten: [\"127.0.0.1:7443\"]\nlimits:\n  outputCapBytes: 10485760\n" +
		"providers:\n  nexusd:\n    enabled: true\n"
	c := newCSStack(t, []byte(body), stackConfig{})
	changes := []string{"limits.outputCapBytes=20971520", "providers.nginx.testTimeout=45s", "host.name=short"}

	// Step 1.
	id := accepted(t, c.set(t, c.admin, nil, changes...))
	c.spy.await(t, id)
	facts := c.factsOf(t, audit.OutcomeRequested)
	want := map[string]string{
		"old.limits.outputCapBytes":       "10485760",
		"old.providers.nginx.testTimeout": "<absent>",
		"old.host.name":                   long[:audit.MaxFactValueBytes],
		"backupPath":                      c.cfgPath + ".bak",
	}
	if audit.MaxFactValueBytes != 256 {
		t.Errorf("MaxFactValueBytes %d, want 256", audit.MaxFactValueBytes)
	}
	if !reflect.DeepEqual(facts, want) {
		t.Errorf("requested facts\n got %v\nwant %v", facts, want)
	}
	recs := c.auditOf(t, spec.UpdateConfig.Op().ID)
	var params struct {
		Changes []string `json:"changes"`
	}
	if err := json.Unmarshal(recs[0].Params, &params); err != nil || !slices.Equal(params.Changes, changes) {
		t.Errorf("requested params %s (%v), want the three new values", recs[0].Params, err)
	}

	// The helper's success, as it would leave the file and result.json.
	var as []config.Assignment
	for _, ch := range changes {
		a, err := config.ParseAssignment(ch)
		if err != nil {
			t.Fatal(err)
		}
		as = append(as, a)
	}
	out, cs, err := config.Edit([]byte(body), c.cfgPath, as)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.cfgPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	res := helperResult{Changed: true, BackupPath: c.cfgPath + ".bak", RestartRequired: []string{"host", "limits"}}
	for _, ch := range cs {
		res.ConfigChanges = append(res.ConfigChanges, spec.ConfigChange{Key: ch.Key, Old: ch.Old, New: ch.New})
	}
	op := c.endOp(t, id, detach.Exit{}, &res)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s %+v, want succeeded", op.Status, op.Error)
	}
	got := decodeJSON[spec.ConfigUpdateResult](t, op.Result)
	if !got.Changed || got.BackupPath != c.cfgPath+".bak" || len(got.Changes) != 3 ||
		!slices.Equal(got.RestartRequired, []string{"host", "limits"}) ||
		!slices.Contains(got.Warnings, "run nexus nexusd restart to apply: host, limits") {
		t.Errorf("result %s", op.Result)
	}

	// Step 2.
	c.noChange(t, c.set(t, c.admin, nil, "limits.outputCapBytes=20971520"))
	facts = c.factsOf(t, audit.OutcomeCompleted)
	if want := map[string]string{"old.limits.outputCapBytes": "20971520", "changed": "false"}; !reflect.DeepEqual(facts, want) {
		t.Errorf("completed facts %v, want %v", facts, want)
	}
}

// csResult runs a change through Admission, Prepare, the real helper on
// F-SYS with F-RELOAD, and Finalize.
func csResult(t *testing.T, change string) (spec.ConfigUpdateResult, *csDirect, int) {
	t.Helper()
	c := newCSDirect(t, change)
	in := c.admit(t, change)
	if _, err := c.funcs.Prepare(t.Context(), in, c.run); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	starts := c.rig.f.starts
	if res, ok := c.rig.exec(t); !ok || res.Error != nil {
		t.Fatalf("helper result %+v (written %t)\n%s\n%s", res, ok, c.rig.stdout.String(), c.rig.stderr.String())
	}
	out, err := c.funcs.Finalize(t.Context(), in, detach.Exit{}, c.run)
	if err != nil {
		t.Fatalf("Finalize = %v", err)
	}
	res, ok := out.(spec.ConfigUpdateResult)
	if !ok {
		t.Fatalf("Finalize = %T, want spec.ConfigUpdateResult", out)
	}
	return res, c, starts
}

// TC-RA-83: A tls.* or auth.* change carries the lock-out warning, and restart-required keys get the restart note. Owner WI 4.3. Requirements: ASM-3, ASM-4, FR-8, FR-11.
func TestTCRA83_ATlsOrAuthChangeCarriesTheLockOutWarningAnd(t *testing.T) {
	// Step 1. auth.failureLimitPerMinute, the only auth.* key, is on the
	// deny-list (SEC-2), so tls.mode is the settable key that warns.
	res, c, starts := csResult(t, "tls.mode=off")
	lockout := "tls.mode takes effect when nexusd restarts; if it changes how this client " +
		"connects (certificate or pin), you may be locked out until a root login on the host fixes it"
	if want := []string{lockout, "run nexus nexusd restart to apply: tls"}; !slices.Equal(res.Warnings, want) {
		t.Errorf("warnings\n got %q\nwant %q", res.Warnings, want)
	}
	if !res.Changed || !slices.Equal(res.RestartRequired, []string{"tls"}) || res.BackupPath != c.rig.cfgPath+".bak" {
		t.Errorf("result %+v, want changed, restartRequired [tls] and the backup", res)
	}
	if len(res.Changes) != 1 || res.Changes[0].Key != "tls.mode" {
		t.Errorf("changes %+v", res.Changes)
	}
	if c.rig.f.starts != starts {
		t.Errorf("F-SYS starts %d -> %d: config set restarted nexusd", starts, c.rig.f.starts)
	}

	// Step 2.
	res, _, _ = csResult(t, "providers.host.enabled=true")
	if len(res.Warnings) != 0 || res.RestartRequired == nil || len(res.RestartRequired) != 0 {
		t.Errorf("result %+v, want no warnings and restartRequired []", res)
	}

}

// TC-RA-E22: A file changed between admission and Prepare ends config-conflict with nothing written. Owner WI 4.3. Requirements: FR-7, FR-8.
func TestTCRAE22_AFileChangedBetweenAdmissionAndPrepareEndsConfig(t *testing.T) {
	for run, days := range map[string]string{"A valid": "45", "B invalid": "0"} {
		t.Run(run, func(t *testing.T) {
			c := newCSDirect(t)
			orig := shaHex(c.rig.orig)

			// Step 1.
			in := c.admit(t, "providers.host.enabled=true")
			if in.BaseSHA256 != orig {
				t.Errorf("BaseSHA256 %q, want %q", in.BaseSHA256, orig)
			}

			// Step 2.
			other, _, err := config.Edit(c.rig.orig, c.rig.cfgPath,
				[]config.Assignment{{Key: "audit.retentionDays", Value: days}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(c.rig.cfgPath, other, 0o600); err != nil {
				t.Fatal(err)
			}

			// Step 3.
			s, err := c.funcs.Prepare(t.Context(), in, c.run)
			if perr := problemOfErr(t, err); perr.Slug() != problem.ConfigConflict {
				t.Errorf("Prepare = %s %q, want config-conflict", perr.Slug(), perr.Detail)
			}
			c.wantNothingPrepared(t, s, shaHex(other))
		})
	}
}

// TC-RA-E23: A no-change config set leaves requested and completed audit records and never takes the lane. Owner WI 4.3. Requirements: FR-7, FR-9, FR-13.
func TestTCRAE23_ANoChangeConfigSetLeavesRequestedAndCompletedAudit(t *testing.T) {
	c := newCSStack(t, defaultConfigWithNexusd(t), stackConfig{})
	running, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.daemon.onReload(func(ctx context.Context) (provider.ReloadResult, error) {
		once.Do(func() { close(running) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return provider.ReloadResult{}, nil
	})

	// Step 1.
	c.noChange(t, c.set(t, c.admin, nil, "providers.nexusd.enabled=true"))

	// Step 2.
	rid := accepted(t, c.action(t, "reload", c.admin, nil))
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("the reload did not start")
	}

	// Step 3.
	m := wantProblem(t, c.set(t, c.admin, nil, "providers.nexusd.enabled=true"), http.StatusConflict,
		problem.Conflict)
	if m["blockingOperationId"] != rid {
		t.Errorf("blockingOperationId %v, want the reload %s", m["blockingOperationId"], rid)
	}

	// Step 4.
	close(release)
	if op := c.awaitOp(t, rid); op.Status != apitypes.StatusSucceeded {
		t.Fatalf("reload %s", op.Status)
	}

	// Step 5.
	both := []string{"providers.nexusd.enabled=true", "providers.host.enabled=true"}
	id := accepted(t, c.set(t, c.admin, nil, both...))
	c.spy.await(t, id)
	if n := c.spy.count(); n != 1 {
		t.Errorf("launches %d, want 1", n)
	}
	if in, _ := readInput(t, filepath.Join(c.opsDir, id)); !slices.Equal(in.Changes, both) {
		t.Errorf("input.json changes %q, want %q", in.Changes, both)
	}

	// Step 6.
	recs := c.auditOf(t, spec.UpdateConfig.Op().ID)
	if len(recs) < 5 {
		t.Fatalf("audit %v, want at least 5 records", outcomes(recs))
	}
	req, done := recs[0], recs[1]
	if req.Outcome != audit.OutcomeRequested || done.Outcome != audit.OutcomeCompleted ||
		done.RequestID != req.ID || done.OperationID != "" {
		t.Errorf("step 1 records %+v, %+v; want requested then completed, linked, with no operation", req, done)
	}
	if want := map[string]string{"changed": "false", "old.providers.nexusd.enabled": "true"}; !reflect.DeepEqual(
		done.Facts, want) {
		t.Errorf("step 1 completed facts %v, want %v", done.Facts, want)
	}
	if r := recs[2]; r.Outcome != audit.OutcomeRefused || r.Problem != string(problem.Conflict) || r.RequestID != "" {
		t.Errorf("step 3 record %+v, want one refused conflict and no requested", r)
	}
	if recs[3].Outcome != audit.OutcomeRequested || recs[4].Outcome != string(apitypes.StatusQueued) ||
		recs[4].RequestID != recs[3].ID {
		t.Errorf("step 5 records %v, want requested then queued", outcomes(recs[3:]))
	}
}

// TC-RA-E30: The no-change rule compares decoded values, and an absent key is always a change. Owner WI 4.3. Requirements: FR-7.
func TestTCRAE30_TheNoChangeRuleComparesDecodedValuesAndAnAbsent(t *testing.T) {
	body := "listen: [\"127.0.0.1:7443\"]\nproviders:\n  host:\n    enabled: True\n  nginx:\n    testTimeout: 90s\n" +
		"  nexusd:\n    enabled: true\n"
	c := newCSStack(t, []byte(body), stackConfig{})
	if _, err := config.LookupKey("limits.outputCapBytes"); err != nil {
		t.Fatal(err)
	}
	def := fmt.Sprint(config.Default().Limits.OutputCapBytes)

	// Steps 1, 2 and 4.
	for _, changes := range [][]string{
		{"providers.nginx.testTimeout=1m30s"},
		{"providers.host.enabled=true"},
		{"providers.nginx.testTimeout=90s", "providers.nexusd.enabled=true"},
	} {
		c.noChange(t, c.set(t, c.admin, nil, changes...))
	}
	if ops := c.operations(t); len(ops) != 0 {
		t.Errorf("operations %d after the no-change sets, want 0", len(ops))
	}

	// Step 3.
	id := accepted(t, c.set(t, c.admin, nil, "limits.outputCapBytes="+def))
	c.spy.await(t, id)
}

// TC-RA-ERR26: Admission refuses an unknown key with its parent and creates no operation. Owner WI 4.3. Requirements: FR-7, FR-6.
func TestTCRAERR26_AdmissionRefusesAnUnknownKeyWithItsParentAnd(t *testing.T) {
	data := defaultConfigWithNexusd(t)
	c := newCSStack(t, data, stackConfig{})

	errs := c.invalid(t, c.set(t, c.admin, nil, "providers.host.enabeld=true"))
	if len(errs) == 0 || errs[0].Path != "providers.host.enabeld" || !strings.Contains(errs[0].Message,
		"providers.host") {
		t.Errorf("errors %+v, want providers.host.enabeld naming providers.host", errs)
	}
	c.wantNoSideEffects(t, shaHex(data))
	if got := outcomes(c.auditOf(t, spec.UpdateConfig.Op().ID)); !slices.Equal(got,
		[]string{"refused:" + string(problem.ConfigInvalid)}) {
		t.Errorf("audit %v, want one refused config-invalid", got)
	}
}

// TC-RA-ERR27: Admission refuses each deny-list entry with its reason. Owner WI 4.3. Requirements: FR-12, ASM-3, NFR-5.
func TestTCRAERR27_AdmissionRefusesEachDenyListEntryWithItsReason(t *testing.T) {
	data := defaultConfigWithNexusd(t)
	c := newCSStack(t, data, stackConfig{})
	refused := make([]bool, len(spec.ConfigDenyList))
	entryOf := func(key, value string) int {
		for i, d := range spec.ConfigDenyList {
			if (key == d.Key || (d.Subtree && strings.HasPrefix(key, d.Key+"."))) && (d.Value == "" || d.Value == value) {
				return i
			}
		}
		t.Fatalf("%s=%s matches no deny-list entry", key, value)
		return -1
	}
	steps := [][]string{
		{"providers.nexusd.update.trustedKeys=x"},
		{"providers.nexusd.enabled=false"},
		{"providers.scripts.catalog=x"},
		{"providers.scripts.catalog.foo=x"},
		{"providers.nginx.includeDir=/etc/profile.d"},
		{"providers.nginx.includeSuffix=.sh"},
		{"providers.nginx.includeDir=/etc/profile.d", "providers.nginx.includeSuffix=.sh"},
		{"providers.nginx.service=ssh"},
		{"providers.nginx.disabledSuffix=.sh"},
		{"audit.retentionDays=1"},
		{"auth.failureLimitPerMinute=1000000"},
		{"providers.services.reboot.enabled=true"},
	}
	covered := map[string]bool{}
	for _, st := range steps {
		for _, ch := range st {
			k, _, _ := strings.Cut(ch, "=")
			covered[k] = true
		}
	}
	for _, d := range spec.ConfigDenyList {
		if d.Value == "" && !d.Subtree && !covered[d.Key] {
			steps = append(steps, []string{d.Key + "=/etc/nexus/x.conf"})
		}
	}
	checks := validations(c.daemon)
	for _, st := range steps {
		t.Run(strings.Join(st, ","), func(t *testing.T) {
			errs := c.invalid(t, c.set(t, c.admin, nil, st...))
			if len(errs) != len(st) {
				t.Fatalf("errors %+v, want one per assignment", errs)
			}
			for i, ch := range st {
				k, v, _ := strings.Cut(ch, "=")
				e := entryOf(k, v)
				want := "cannot be set remotely: " + spec.ConfigDenyList[e].Reason
				if errs[i].Path != k || errs[i].Message != want {
					t.Errorf("error %+v, want %s: %q", errs[i], k, want)
				}
				refused[e] = true
			}
		})
	}
	for i, ok := range refused {
		if !ok {
			t.Errorf("deny-list entry %+v was never refused", spec.ConfigDenyList[i])
		}
	}
	if n := validations(c.daemon) - checks; n != 0 {
		t.Errorf("the validator ran %d times for denied keys, want 0", n)
	}
	c.wantNoSideEffects(t, shaHex(data))
}

// TC-RA-ERR28: Admission refuses lists, mappings, wrong types, duplicates and too many changes. Owner WI 4.3. Requirements: FR-7, NFR-5, OOS-3, ASM-3.
func TestTCRAERR28_AdmissionRefusesListsMappingsWrongTypes(t *testing.T) {
	data := defaultConfigWithNexusd(t)
	c := newCSStack(t, data, stackConfig{})
	long := strings.Repeat("v", spec.MaxConfigValueBytes+1)

	for _, tc := range []struct {
		name    string
		changes []string
		path    string
		message string
	}{
		{"list", []string{"listen=127.0.0.1:9"}, "listen", "is a list"},
		{"list, comma form", []string{"listen=127.0.0.1:9,127.0.0.1:10"}, "listen", "is a list"},
		{"mapping", []string{"providers.host=x"}, "providers.host", "is a mapping"},
		{"type", []string{"providers.host.enabled=yes"}, "providers.host.enabled", "expected a boolean"},
		{"duplicate", []string{"tls.mode=auto", "tls.mode=off"}, "tls.mode", "more than once"},
		{"length", []string{"host.name=" + long}, "host.name", "4096 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := c.set(t, c.admin, nil, tc.changes...)
			errs := c.invalid(t, r)
			if len(errs) != 1 || errs[0].Path != tc.path || !strings.Contains(errs[0].Message, tc.message) {
				t.Errorf("errors %+v, want one at %s containing %q", errs, tc.path, tc.message)
			}
			if strings.Contains(string(r.body), "cannot be set remotely") || strings.Contains(string(r.body), long) ||
				strings.Contains(string(r.body), "locked out") {
				t.Errorf("body %.300s: a deny reason, the value or a lock-out warning", r.body)
			}
		})
	}
	// Steps 5 and 7: the schema's bounds, before admission.
	many := make([]string, spec.MaxConfigChanges+1)
	for i := range many {
		many[i] = fmt.Sprintf("host.name=n%d", i)
	}
	checks := validations(c.daemon)
	for _, changes := range [][]string{many, {"host.name=" + strings.Repeat("v", spec.MaxConfigValueBytes+521)}} {
		wantProblem(t, c.set(t, c.admin, nil, changes...), http.StatusBadRequest, problem.ValidationFailed)
	}
	if n := validations(c.daemon) - checks; n != 0 {
		t.Errorf("admission ran %d validations for schema refusals, want 0", n)
	}
	c.wantNoSideEffects(t, shaHex(data))
}

// TC-RA-ERR29: Admission refuses an edit that makes the file invalid. Owner WI 4.3. Requirements: FR-7.
func TestTCRAERR29_AdmissionRefusesAnEditThatMakesTheFileInvalid(t *testing.T) {
	body := []byte("listen: [\"0.0.0.0:8443\"]\ntls:\n  mode: auto\nproviders:\n  nexusd:\n    enabled: true\n")
	c := newCSStack(t, body, stackConfig{})

	for _, tc := range []struct{ change, path string }{
		{"tls.mode=off", "listen[0]"},
		{"limits.outputCapBytes=0", "limits.outputCapBytes"},
	} {
		r := c.set(t, c.admin, nil, tc.change)
		errs := c.invalid(t, r)
		if len(errs) == 0 || errs[0].Path != tc.path {
			t.Errorf("%s: errors %+v, want one at %s", tc.change, errs, tc.path)
		}
		a, err := config.ParseAssignment(tc.change)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := config.Edit(body, c.cfgPath, []config.Assignment{a})
		if err != nil {
			t.Fatal(err)
		}
		_, list := CheckConfigDataForTest(out, c.cfgPath)
		raw := decodeJSON[map[string]json.RawMessage](t, r.body)["errors"]
		sameJSON(t, tc.change+": errors", raw, configErrors(list))
	}
	if !strings.Contains(fmt.Sprint(c.invalid(t, c.set(t, c.admin, nil, "tls.mode=off"))), "loopback") {
		t.Error("the listen[0] error is not the validator's loopback text")
	}
	c.wantNoSideEffects(t, shaHex(body))
}

// TC-RA-ERR30: Config set on a host without systemd self-management is refused while tokens and config get still work. Owner WI 4.3. Requirements: FR-9, OOS-2.
func TestTCRAERR30_ConfigSetOnAHostWithoutSystemdSelfManagementIs(t *testing.T) {
	data := defaultConfigWithNexusd(t)
	c := newCSStack(t, data, stackConfig{opts: Options{Systemd: func() detach.Availability {
		return detach.Availability{Reason: openRCReason}
	}}})
	checks := validations(c.daemon)

	// Step 1.
	m := wantProblem(t, c.set(t, c.admin, nil, "providers.host.enabled=true"), http.StatusServiceUnavailable,
		problem.ConfigSetUnavailable)
	if m["provider"] != spec.Name || m["reason"] != openRCReason {
		t.Errorf("problem %v, want provider nexusd and reason %q", m, openRCReason)
	}
	c.wantNoSideEffects(t, shaHex(data))
	if n := validations(c.daemon) - checks; n != 0 {
		t.Errorf("the validator ran %d times, want 0", n)
	}
	if got := outcomes(c.auditOf(t, spec.UpdateConfig.Op().ID)); !slices.Equal(got,
		[]string{"refused:" + string(problem.ConfigSetUnavailable)}) {
		t.Errorf("audit %v, want one refused record", got)
	}

	// Steps 2 and 3.
	c.do(t, http.MethodGet, "/providers/nexusd/tokens", c.admin, "", nil).want(t, http.StatusOK)
	c.do(t, http.MethodPost, "/providers/nexusd/tokens/ci/add", c.admin, `{"scopes":["nexusd:read"]}`, nil).
		want(t, http.StatusOK)
	c.do(t, http.MethodPost, "/providers/nexusd/tokens/ci/remove", c.admin, "", nil).want(t, http.StatusOK)
	c.configGet(t, c.admin, "audit.retentionDays").want(t, http.StatusOK)
}

// TC-RA-ERR31: Config set refuses a nexusd:read-only token and an unauthenticated call. Owner WI 4.3. Requirements: FR-7, NFR-7.
func TestTCRAERR31_ConfigSetRefusesANexusdReadOnlyTokenAndAn(t *testing.T) {
	c := newCSStack(t, defaultConfigWithNexusd(t), stackConfig{})

	// Step 1.
	m := wantProblem(t, c.set(t, c.reader, nil, "providers.host.enabled=true"), http.StatusForbidden,
		problem.Forbidden)
	if m["requiredScope"] != spec.AdminScope {
		t.Errorf("requiredScope %v, want %s", m["requiredScope"], spec.AdminScope)
	}
	recs := c.auditOf(t, spec.UpdateConfig.Op().ID)
	if !slices.ContainsFunc(recs, func(r systemspec.AuditRecord) bool { return r.Outcome == audit.OutcomeDenied }) {
		t.Errorf("audit %v, want a denied record", outcomes(recs))
	}

	// Step 2.
	c.set(t, "", nil, "providers.host.enabled=true").want(t, http.StatusUnauthorized)
	if n := c.spy.count(); n != 0 {
		t.Errorf("launches %d, want 0", n)
	}
}

// TC-RA-ERR43: Prepare refuses an empty baseline, as after a restart, and the baseline never survives a JSON round trip. Owner WI 4.3. Requirements: FR-7, FR-8.
func TestTCRAERR43_PrepareRefusesAnEmptyBaselineAsAfterARestart(t *testing.T) {
	c := newCSDirect(t)
	in := c.admit(t, "providers.host.enabled=true")
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var recovered spec.ConfigUpdateInput
	if err := json.Unmarshal(b, &recovered); err != nil {
		t.Fatal(err)
	}

	// Step 1.
	if recovered.BaseSHA256 != "" || in.BaseSHA256 == "" {
		t.Fatalf("BaseSHA256 %q after a round trip of %q, want empty after a non-empty one", recovered.BaseSHA256,
			in.BaseSHA256)
	}

	// Step 2.
	s, err := c.funcs.Prepare(t.Context(), recovered, c.run)
	perr := problemOfErr(t, err)
	if perr.Slug() != problem.ConfigConflict || perr.Detail != "nexusd restarted before the change was prepared; "+
		"nothing was changed; run the command again" {
		t.Errorf("Prepare = %s %q", perr.Slug(), perr.Detail)
	}
	c.wantNothingPrepared(t, s, shaHex(c.rig.orig))
}
