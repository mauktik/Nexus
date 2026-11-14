// Cases: TC-54b, TC-66 to TC-73, TC-ERR11 and TC-ERR30 in
// planning/pihole-dns-zones/test-cases.md (WI 3.1): the detached binding
// builder, the helper, the real exec runner's exit contract, core files,
// and the v5 Mutator's wait for an external gravity run.
//
// The helper's actions here are test actions, registered under test op IDs
// only; the real actions are added by WIs 3.2 to 4.4. Every host is a
// v5test.FakeHost under t.TempDir().
package pihole

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
	"github.com/mauktik/nexus/internal/store"
)

// echoIn is the test ops' input. Secret is a value that must reach the
// helper only through input.json, never argv or env (CON-2).
type echoIn struct {
	Name   string `json:"name"`
	Secret string `json:"secret,omitempty"`
}

type echoOut struct {
	apitypes.ActionResult
	Name   string `json:"name"`
	Echoed string `json:"echoed"`
}

func testOp[In, Out any](action, summary string) def.Typed[In, Out] {
	return def.Define[In, Out](def.Op{
		ID: spec.Name + ".actions." + action, Provider: spec.Name, Kind: def.KindProviderAction, Action: action,
		Summary: summary, Description: summary + " (test only).",
		Scope: def.Scope{Name: spec.WriteScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI: def.CLIBinding{Noun: "piholetest", Verb: "start"},
	})
}

// The test ops and their helper actions.
var (
	echoOp     = testOp[echoIn, echoOut]("test-echo", "Echo the input")
	failOp     = testOp[echoIn, echoOut]("test-fail", "Fail with dns-config-test-failed")
	nilProbOp  = testOp[echoIn, echoOut]("test-nil-problem", "Return probErr of a nil problem")
	plainErrOp = testOp[echoIn, echoOut]("test-plain-error", "Fail with a plain error")
	wiringOp   = testOp[echoIn, echoOut]("test-wiring", "Record the helper's seams")
	blockingOp = testOp[spec.NoInput, blockingOut]("test-read-blocking", "Read the blocking state")
)

type blockingOut struct {
	apitypes.ActionResult
	State string `json:"state"`
}

// wired is what the wiring action saw.
var wired struct {
	mu   sync.Mutex
	call *helperCall
}

func init() {
	helperActions[echoOp.Op().ID] = func(_ context.Context, c *helperCall) (any, error) {
		var in echoIn
		if err := json.Unmarshal(c.In.Input, &in); err != nil {
			return nil, err
		}
		fmt.Fprintf(c.Out, "pihole: echo %s\n", in.Name)
		return echoOut{ActionResult: apitypes.ActionResult{Changed: true}, Name: in.Name, Echoed: c.In.DNSFile}, nil
	}
	helperActions[failOp.Op().ID] = func(context.Context, *helperCall) (any, error) {
		return nil, probErr(problem.New(problem.DNSConfigTestFailed, "the syntax test failed").
			With("exitCode", 139).With("rolledBack", true).With("preExisting", false))
	}
	helperActions[nilProbOp.Op().ID] = func(context.Context, *helperCall) (any, error) {
		var pe *problem.Error
		return echoOut{Name: "ok"}, probErr(pe)
	}
	helperActions[plainErrOp.Op().ID] = func(context.Context, *helperCall) (any, error) {
		return nil, errors.New("boom")
	}
	helperActions[wiringOp.Op().ID] = func(_ context.Context, c *helperCall) (any, error) {
		wired.mu.Lock()
		defer wired.mu.Unlock()
		wired.call = c
		return echoOut{}, nil
	}
	helperActions[blockingOp.Op().ID] = func(_ context.Context, c *helperCall) (any, error) {
		state := "unknown"
		if b := v5.ReadBlockingEnabled(c.Paths); b != nil {
			state = map[bool]string{true: "enabled", false: "disabled"}[*b]
		}
		fmt.Fprintf(c.Out, "pihole: blocking is %s\n", state)
		return blockingOut{State: state}, nil
	}
}

func enabledStatus(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

// helperWorld is a provider wired to a FakeHost through the test seams
// only, with its operations' directory under t.TempDir().
type helperWorld struct {
	t      *testing.T
	h      *v5test.FakeHost
	p      *Provider
	opsDir string
}

func newHelperWorld(t *testing.T) *helperWorld {
	t.Helper()
	h := v5test.NewFakeHost(t)
	p := New(Options{Host: "h", Preflight: enabledStatus, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	p.paths, p.runner = h.Paths(), h
	opsDir := filepath.Join(t.TempDir(), "state", "ops")
	if err := os.MkdirAll(opsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	p.UseOpsDir(opsDir)
	return &helperWorld{t: t, h: h, p: p, opsDir: opsDir}
}

func (w *helperWorld) inject() *helperInject {
	return &helperInject{Runner: w.h, Root: w.h.Root(), DNSServer: w.h.FTL().Addr(), Now: w.h.Now, Reload: v5test.FastReload(),
		PIDPoll: pidPoll{Interval: 10 * time.Millisecond, Limit: testPIDLimit}}
}

func (w *helperWorld) configure(dnsFile string) {
	w.t.Helper()
	err := w.p.Configure(func(into any) error {
		into.(*config.PiholeConfig).DNSFile = dnsFile
		return nil
	})
	if err != nil {
		w.t.Fatal(err)
	}
}

func lostDetail(r *provider.Run, in echoIn) string {
	return "check " + in.Name + " by hand (operation " + r.ID() + ")"
}

// prepare runs the Prepare of op's binding for operation id.
func prepare[In, Out any](w *helperWorld, op def.Typed[In, Out], id string, in In) (detach.Spec, string) {
	w.t.Helper()
	df, ok := detachedOp(w.p, op, nil, maxRuntimeDefault, nil).Detached()
	if !ok {
		w.t.Fatal("not a detached binding")
	}
	s, err := df.Prepare(context.Background(), in, provider.NewRun(id, nil))
	if err != nil {
		w.t.Fatalf("Prepare: %v", err)
	}
	return s, filepath.Join(w.opsDir, id)
}

// finalize runs the Finalize of op's binding (LostDetail lostDetail).
func finalize[Out any](w *helperWorld, op def.Typed[echoIn, Out], id string, in echoIn, x detach.Exit) (Out, error) {
	w.t.Helper()
	df, _ := detachedOp(w.p, op, nil, maxRuntimeDefault, lostDetail).Detached()
	out, err := df.Finalize(context.Background(), in, x, provider.NewRun(id, nil))
	if err != nil {
		var zero Out
		return zero, err
	}
	return out.(Out), nil
}

func readInput(t *testing.T, dir string) (map[string]json.RawMessage, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, inputFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m, b
}

func jsonString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TC-66 (FR-5, CON-1, CON-2): Prepare writes input.json and the exact unit
// spec; the input is only in the file.
func TestTC66_PrepareInputAndSpec(t *testing.T) {
	w := newHelperWorld(t)
	const secret = "sentinel-input-7c1d"
	before := time.Now()
	s, dir := prepare(w, echoOp, "op-66", echoIn{Name: "n", Secret: secret})
	after := time.Now()

	fi, err := os.Stat(filepath.Join(dir, inputFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("input.json mode %v, want 0600", fi.Mode().Perm())
	}
	m, raw := readInput(t, dir)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"backend", "deadline", "dnsFile", "input", "op", "opDir", "operationId", "stateDir"}
	if !slices.Equal(keys, want) {
		t.Errorf("input.json keys %v, want %v", keys, want)
	}
	if got := jsonString(t, m["operationId"]); got != "op-66" {
		t.Errorf("operationId %q", got)
	}
	if got := jsonString(t, m["op"]); got != echoOp.Op().ID {
		t.Errorf("op %q", got)
	}
	if got := jsonString(t, m["backend"]); got != spec.BackendV5CLI {
		t.Errorf("backend %q, want %q", got, spec.BackendV5CLI)
	}
	if got := jsonString(t, m["dnsFile"]); got != defaultDNSFile {
		t.Errorf("dnsFile %q", got)
	}
	if got, want := jsonString(t, m["stateDir"]), filepath.Join(filepath.Dir(w.opsDir), "pihole"); got != want {
		t.Errorf("stateDir %q, want %q", got, want)
	}
	if got := jsonString(t, m["opDir"]); got != dir {
		t.Errorf("opDir %q, want %q", got, dir)
	}
	var deadline time.Time
	if err := json.Unmarshal(m["deadline"], &deadline); err != nil {
		t.Fatal(err)
	}
	lo, hi := before.Add(maxRuntimeDefault-time.Minute), after.Add(maxRuntimeDefault-time.Minute)
	if deadline.Before(lo.Truncate(time.Second)) || deadline.After(hi) {
		t.Errorf("deadline %v, want now+10m-1m in [%v, %v]", deadline, lo, hi)
	}
	var in echoIn
	if err := json.Unmarshal(m["input"], &in); err != nil || in != (echoIn{Name: "n", Secret: secret}) {
		t.Errorf("input %s (%v)", m["input"], err)
	}
	if !bytes.Contains(raw, []byte(secret)) {
		t.Error("the input is not in input.json")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{exe, "pihole-helper", "--dir", dir}; !slices.Equal(s.Argv, want) {
		t.Errorf("argv %q, want %q", s.Argv, want)
	}
	wantEnv := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "HOME=/root"}
	if !slices.Equal(s.Env, wantEnv) {
		t.Errorf("env %q, want exactly %q (A-29)", s.Env, wantEnv)
	}
	if s.Dir != "/" || s.KillMode != detach.KillProcess || s.MaxRuntime != maxRuntimeDefault {
		t.Errorf("dir %q kill mode %q max runtime %v", s.Dir, s.KillMode, s.MaxRuntime)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("spec does not validate: %v", err)
	}
	for _, a := range slices.Concat(s.Argv, s.Env) {
		if strings.Contains(a, secret) || strings.Contains(a, "n\"") {
			t.Errorf("the input reached argv or env: %q", a)
		}
	}

	// MaxRuntime is the op's (A-3).
	df, _ := detachedOp(w.p, echoOp, nil, maxRuntimeGravity, nil).Detached()
	s, err = df.Prepare(context.Background(), echoIn{Name: "g"}, provider.NewRun("op-66g", nil))
	if err != nil || s.MaxRuntime != time.Hour {
		t.Errorf("gravity MaxRuntime %v (%v), want 1h", s.MaxRuntime, err)
	}
}

// TC-69 (FR-5): every pihole binding queues FIFO on the one pihole lane, and
// cancels queued only.
func TestTC69_OneFIFOLane(t *testing.T) {
	w := newHelperWorld(t)
	b := detachedOp(w.p, echoOp, nil, maxRuntimeDefault, nil)
	df, _ := b.Detached()
	l, err := df.Lane(context.Background(), echoIn{Name: "x"})
	if err != nil || l != (provider.Lane{Key: "pihole", Policy: provider.LaneQueue}) {
		t.Errorf("lane %+v (%v), want {pihole queue}", l, err)
	}
	if b.Op().Cancel != def.CancelQueuedOnly {
		t.Errorf("cancel %q, want queued-only", b.Op().Cancel)
	}
}

// TC-54b (FR-2): a reload leaves an already-prepared op's input.json on the
// old dnsFile, byte for byte; a new Prepare carries the new one.
func TestTC54b_ReloadKeepsPreparedDNSFile(t *testing.T) {
	w := newHelperWorld(t)
	const a, b = "/etc/dnsmasq.d/03-custom-dns-names.conf", "/etc/dnsmasq.d/04-nexus.conf"
	w.configure(a)
	_, dir1 := prepare(w, echoOp, "op-a", echoIn{Name: "a"})
	_, before := readInput(t, dir1)

	w.configure(b)
	m1, after := readInput(t, dir1)
	if !bytes.Equal(before, after) {
		t.Errorf("input.json changed by the reload:\n%s\n%s", before, after)
	}
	if got := jsonString(t, m1["dnsFile"]); got != a {
		t.Errorf("prepared op's dnsFile %q, want %q", got, a)
	}
	_, dir2 := prepare(w, echoOp, "op-b", echoIn{Name: "b"})
	if m2, _ := readInput(t, dir2); jsonString(t, m2["dnsFile"]) != b {
		t.Errorf("new op's dnsFile %s, want %q", m2["dnsFile"], b)
	}
}

// runPrepared prepares op and runs the helper on its directory.
func runPrepared[Out any](w *helperWorld, op def.Typed[echoIn, Out], id string, in echoIn) (string, int, string) {
	w.t.Helper()
	_, dir := prepare(w, op, id, in)
	var out, errb bytes.Buffer
	code := runHelper(context.Background(), dir, &out, &errb, w.inject())
	return dir, code, out.String() + errb.String()
}

func readResult(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, resultFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The input.json/result.json round trip: Prepare, the helper, Finalize.
// TC-67 (FR-5): a success decodes to the typed result; an error becomes the
// problem with its slug, detail and every declared extension.
func TestTC67_FinalizeMapsResult(t *testing.T) {
	w := newHelperWorld(t)
	dir, code, out := runPrepared(w, echoOp, "op-ok", echoIn{Name: "web"})
	if code != 0 || !strings.Contains(out, "pihole: echo web") {
		t.Fatalf("helper exit %d, output %q", code, out)
	}
	if fi, err := os.Stat(filepath.Join(dir, resultFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("result.json: %v %v, want mode 0600", fi, err)
	}
	if m := readResult(t, dir); m["error"] != nil {
		t.Errorf("a success wrote an error member: %s", m["error"])
	}
	got, err := finalize(w, echoOp, "op-ok", echoIn{Name: "web"}, detach.Exit{})
	want := echoOut{ActionResult: apitypes.ActionResult{Changed: true}, Name: "web", Echoed: defaultDNSFile}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Finalize = %+v, %v; want %+v", got, err, want)
	}

	_, code, _ = runPrepared(w, failOp, "op-fail", echoIn{Name: "web"})
	if code != 0 {
		t.Fatalf("a failed action: helper exit %d, want 0", code)
	}
	_, err = finalize(w, failOp, "op-fail", echoIn{Name: "web"}, detach.Exit{})
	var pe *problem.Error
	if !errors.As(err, &pe) {
		t.Fatalf("Finalize error %v is not a problem", err)
	}
	wantExt := map[string]any{"exitCode": 139, "rolledBack": true, "preExisting": false}
	if pe.Slug() != problem.DNSConfigTestFailed || pe.Detail != "the syntax test failed" || !reflect.DeepEqual(pe.Extensions, wantExt) {
		t.Errorf("problem %s %q %#v, want %s with %#v", pe.Slug(), pe.Detail, pe.Extensions, problem.DNSConfigTestFailed, wantExt)
	}

	// A plain error is internal-error.
	runPrepared(w, plainErrOp, "op-plain", echoIn{})
	_, err = finalize(w, plainErrOp, "op-plain", echoIn{}, detach.Exit{})
	if !errors.As(err, &pe) || pe.Slug() != problem.InternalError || !strings.Contains(pe.Detail, "boom") {
		t.Errorf("plain error: %v", err)
	}
}

// probErr never builds a non-nil error from a nil problem, and an action
// returning probErr(nil) writes no error member.
func TestProbErrNoTypedNil(t *testing.T) {
	var pe *problem.Error
	if err := probErr(pe); err != nil {
		t.Fatalf("probErr(nil) = %#v, want a nil error", err)
	}
	if err := probErr(problem.New(problem.PiholeBusy, "x")); err == nil {
		t.Fatal("probErr(problem) = nil")
	}
	w := newHelperWorld(t)
	dir, code, _ := runPrepared(w, nilProbOp, "op-nil", echoIn{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, ok := readResult(t, dir)["error"]; ok {
		t.Errorf("result.json has an error member for a nil problem")
	}
	if got, err := finalize(w, nilProbOp, "op-nil", echoIn{}, detach.Exit{}); err != nil || got.Name != "ok" {
		t.Errorf("Finalize = %+v, %v", got, err)
	}
}

// TC-ERR11 (FR-5): no result.json after a launch error is internal-error
// (nothing was changed); without a launch error it is outcome-unknown with
// the op's LostDetail.
func TestTCERR11_LostUnitAndLaunchFailure(t *testing.T) {
	w := newHelperWorld(t)
	in := echoIn{Name: "web"}
	prepare(w, echoOp, "op-launch", in)
	_, err := finalize(w, echoOp, "op-launch", in, detach.Exit{ExitCode: -1, Error: "fork/exec: permission denied"})
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.InternalError || !strings.Contains(pe.Detail, "nothing was changed") {
		t.Errorf("launch failure: %v", err)
	}

	prepare(w, echoOp, "op-lost", in)
	_, err = finalize(w, echoOp, "op-lost", in, detach.Exit{ExitCode: -1, Signal: "SIGKILL"})
	lost := lostDetail(provider.NewRun("op-lost", nil), in)
	if !errors.As(err, &pe) || pe.Slug() != problem.OutcomeUnknown || !strings.Contains(pe.Detail, lost) ||
		!strings.Contains(pe.Detail, "SIGKILL") {
		t.Errorf("lost unit: %v; want outcome-unknown with %q", err, lost)
	}
}

// TC-72 (FR-5): the helper writes result.json for a failing action (exit
// 0) and an unknown op (internal-error); only an unwritable result is a
// non-zero exit.
func TestTC72_HelperAlwaysWritesResult(t *testing.T) {
	w := newHelperWorld(t)
	dir, code, _ := runPrepared(w, failOp, "op-f", echoIn{})
	if code != 0 {
		t.Errorf("failing action: exit %d, want 0", code)
	}
	var res helperResult
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, resultFile)), &res); err != nil || res.Error == nil ||
		res.Error.Slug != problem.DNSConfigTestFailed {
		t.Errorf("failing action's result %+v (%v)", res, err)
	}

	_, dir = prepare(w, echoOp, "op-unknown", echoIn{})
	rewriteInput(t, dir, func(in *helperInput) { in.Op = "pihole.actions.no-such-op" })
	if code := runHelper(context.Background(), dir, io.Discard, io.Discard, w.inject()); code != 0 {
		t.Errorf("unknown op: exit %d", code)
	}
	res = helperResult{}
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, resultFile)), &res); err != nil || res.Error == nil ||
		res.Error.Slug != problem.InternalError {
		t.Errorf("unknown op's result %+v (%v)", res, err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	_, dir = prepare(w, echoOp, "op-ro", echoIn{})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var errb bytes.Buffer
	if code := runHelper(context.Background(), dir, io.Discard, &errb, w.inject()); code == 0 {
		t.Errorf("unwritable op dir: exit 0, want non-zero")
	}
	if !strings.Contains(errb.String(), dir) {
		t.Errorf("stderr %q does not name the directory", errb.String())
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rewriteInput(t *testing.T, dir string, f func(*helperInput)) {
	t.Helper()
	var in helperInput
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, inputFile)), &in); err != nil {
		t.Fatal(err)
	}
	f(&in)
	if err := writeJSONAtomic(dir, inputFile, in); err != nil {
		t.Fatal(err)
	}
}

// TC-68 (FR-1, NFR-7): Helper runs with the real exec runner, root "/",
// DNS server 127.0.0.1:53 and time.Now; the nexusd binary dispatches
// pihole-helper to it.
func TestTC68_ProductionWiring(t *testing.T) {
	s := helperSeams(nil)
	if _, ok := s.Runner.(v5.Exec); !ok || s.Root != "/" || s.DNSServer != netip.MustParseAddrPort("127.0.0.1:53") ||
		reflect.ValueOf(s.Now).Pointer() != reflect.ValueOf(time.Now).Pointer() {
		t.Errorf("production seams %+v", s)
	}
	// Every timing seam is pinned to its named constant (review CR-8): a
	// test-speed value leaking into production would pass the other tests.
	if s.ReadyWindow != dnscheck.ReadyWindow {
		t.Errorf("production readiness window %s, want dnscheck.ReadyWindow %s", s.ReadyWindow, dnscheck.ReadyWindow)
	}
	if s.PIDPoll != defaultPIDPoll || s.Reload != (v5.ReloadTiming{}) {
		t.Errorf("production PID poll %+v, reload timing %+v, want the defaults and the zero ReloadTiming (A-67)", s.PIDPoll, s.Reload)
	}
	if got := helperSeams(&helperInject{}).Reload; got != (v5.ReloadTiming{}) {
		t.Errorf("an empty injection's reload timing %+v, want the zero value", got)
	}

	w := newHelperWorld(t)
	_, dir := prepare(w, wiringOp, "op-wiring", echoIn{})
	var errb bytes.Buffer
	if code := Helper([]string{"--dir", dir}, io.Discard, &errb); code != 0 {
		t.Fatalf("Helper exit %d: %s", code, errb.String())
	}
	wired.mu.Lock()
	c := wired.call
	wired.mu.Unlock()
	if c == nil {
		t.Fatal("the wiring action did not run")
	}
	m, ok := c.Mutator.(*v5.Mutator)
	if !ok {
		t.Fatalf("mutator %T, want *v5.Mutator", c.Mutator)
	}
	if _, ok := m.Runner.(v5.Exec); !ok || m.Paths.Root != "/" || c.Paths.Root != "/" {
		t.Errorf("mutator runner %T root %q, call root %q", m.Runner, m.Paths.Root, c.Paths.Root)
	}
	if m.Paths != (v5.Paths{Root: "/"}) || m.Reload != (v5.ReloadTiming{}) || m.Sleep != nil || m.GravityPoll != 0 {
		t.Errorf("mutator paths %+v, reload %+v, sleep set %v, gravity poll %s: only the root may be set, and no timing override",
			m.Paths, m.Reload, m.Sleep != nil, m.GravityPoll)
	}
	// TC-E15: the list-reload barrier probes FTL on 127.0.0.1:53 and reads
	// /var/log/pihole/FTL.log (on a host whose pihole-FTL.conf has no
	// LOGFILE=, as the image's).
	if m.DNSServer != netip.MustParseAddrPort("127.0.0.1:53") {
		t.Errorf("mutator DNS server %v, want 127.0.0.1:53", m.DNSServer)
	}
	// Without a LOGFILE= in pihole-FTL.conf the log is the default one,
	// whatever the host's own conf says.
	root := t.TempDir()
	if got, want := (v5.Paths{Root: root}).FTLLogPath(), filepath.Join(root, "/var/log/pihole/FTL.log"); got != want {
		t.Errorf("FTL log %q with no pihole-FTL.conf, want %q", got, want)
	}
	if c.DNSServer != netip.MustParseAddrPort("127.0.0.1:53") ||
		reflect.ValueOf(c.Now).Pointer() != reflect.ValueOf(time.Now).Pointer() {
		t.Errorf("DNS server %v, Now %p", c.DNSServer, c.Now)
	}

	exe, err := detachtest.Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-op")
	cmd := exec.Command(exe, "pihole-helper", "--dir", missing)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("nexusd pihole-helper --dir missing: %v, want a non-zero exit\n%s", err, out)
	}
	if !strings.Contains(string(out), missing) || strings.Contains(string(out), "Usage:") {
		t.Errorf("output %q: want the missing directory named, not the nexusd usage", out)
	}
}

// After runHelper, the soft RLIMIT_CORE is 0 and the hard limit is
// unchanged; a child inherits the 0 (A-57).
func TestHelperDisablesCoreFiles(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &lim); err != nil {
		t.Fatal(err)
	}
	if lim.Max == 0 {
		t.Skip("hard RLIMIT_CORE is 0: the soft limit cannot be raised to observe the change")
	}
	raised := syscall.Rlimit{Cur: min(lim.Max, 1<<20), Max: lim.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &raised); err != nil {
		t.Fatal(err)
	}
	// The test process's own soft limit goes back as it was; runHelper
	// lowers it for the rest of the run (review CR-19).
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &lim); err != nil {
			t.Errorf("restoring RLIMIT_CORE: %v", err)
		}
	})
	w := newHelperWorld(t)
	runPrepared(w, echoOp, "op-core", echoIn{})
	var got syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cur != 0 || got.Max != lim.Max {
		t.Errorf("RLIMIT_CORE after runHelper = %+v, want soft 0, hard %d", got, lim.Max)
	}
	var out bytes.Buffer
	if code, err := (v5.Exec{}).Run(context.Background(), childArgv(t, "rlimit-core"), &out, io.Discard); code != 0 || err != nil {
		t.Fatalf("child: %d %v", code, err)
	}
	if strings.TrimSpace(out.String()) != "0" {
		t.Errorf("child's soft RLIMIT_CORE %q, want 0", out.String())
	}
}

// childArgv re-executes this test binary as a child that does mode. The
// runner gives a child only the fixed environment, so the mode travels as
// an argument after "--".
func childArgv(t *testing.T, mode string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, "-test.run=^TestExecChild$", "--", mode}
}

// TestExecChild is the child of the exec runner tests; run normally, it
// does nothing.
func TestExecChild(t *testing.T) {
	args := flag.Args()
	if len(args) != 1 {
		return
	}
	switch args[0] {
	case "exit0":
		os.Exit(0)
	case "exit1":
		os.Exit(1)
	case "env":
		fmt.Println(strings.Join(os.Environ(), "\n"))
		os.Exit(0)
	case "rlimit-core":
		var lim syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &lim); err != nil {
			os.Exit(3)
		}
		fmt.Println(lim.Cur)
		os.Exit(0)
	case "sigterm", "sigkill":
		sig := map[string]syscall.Signal{"sigterm": syscall.SIGTERM, "sigkill": syscall.SIGKILL}[args[0]]
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			os.Exit(3)
		}
		time.Sleep(10 * time.Second)
		os.Exit(4)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(5)
}

// The real runner's exit contract (A-19): a normal exit is (status, nil); a
// death by signal is (128+signal, nil) with the signal named on stderr; a
// missing argv[0] or an ended ctx is (-1, err).
func TestExecRunnerExitContract(t *testing.T) {
	// A crashing child must leave no core file in the package directory.
	if err := noCoreFiles(); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, argv []string) (int, error, string, string) {
		var out, errb bytes.Buffer
		code, err := v5.Exec{}.Run(ctx, argv, &out, &errb)
		return code, err, out.String(), errb.String()
	}
	ctx := context.Background()
	if code, err, _, _ := run(ctx, childArgv(t, "exit0")); code != 0 || err != nil {
		t.Errorf("exit 0: (%d, %v)", code, err)
	}
	if code, err, _, _ := run(ctx, childArgv(t, "exit1")); code != 1 || err != nil {
		t.Errorf("exit 1: (%d, %v), want (1, nil)", code, err)
	}
	if _, err, out, _ := run(ctx, childArgv(t, "env")); err != nil ||
		!slices.Equal(strings.Fields(out), v5.Env()) {
		t.Errorf("child env %q, want exactly %q", out, v5.Env())
	}
	for _, c := range []struct {
		mode string
		sig  syscall.Signal
		name string
	}{{"sigterm", syscall.SIGTERM, "SIGTERM"}, {"sigkill", syscall.SIGKILL, "SIGKILL"}} {
		code, err, _, errOut := run(ctx, childArgv(t, c.mode))
		if code != 128+int(c.sig) || err != nil || !strings.Contains(errOut, "killed by signal "+c.name) {
			t.Errorf("%s: (%d, %v) stderr %q, want (%d, nil) naming %s", c.mode, code, err, errOut, 128+int(c.sig), c.name)
		}
	}
	// A Go child cannot die by a self-sent SIGSEGV (the runtime ignores one
	// sent with kill), so the SIGSEGV case is a /bin/sh that sends one to
	// itself, as `pihole-FTL --test` died in WI 0.1 (exit 139).
	if _, err := os.Stat("/bin/sh"); err == nil {
		code, err, _, errOut := run(ctx, []string{"/bin/sh", "-c", "kill -SEGV $$"})
		if code != 139 || err != nil || !strings.Contains(errOut, "pihole: /bin/sh killed by signal SIGSEGV") {
			t.Errorf("SIGSEGV: (%d, %v) stderr %q, want (139, nil) naming SIGSEGV", code, err, errOut)
		}
	}
	if code, err, _, _ := run(ctx, []string{"/nonexistent/pihole-FTL", "--test"}); code != -1 || err == nil {
		t.Errorf("missing argv[0]: (%d, %v), want (-1, err)", code, err)
	}
	if code, err, _, _ := run(ctx, []string{"pihole", "enable"}); code != -1 || err == nil {
		t.Errorf("relative argv[0]: (%d, %v), want (-1, err)", code, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	code, err, _, _ := run(cctx, childArgv(t, "sleep"))
	if code != -1 || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: (%d, %v), want (-1, context.Canceled)", code, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("cancel took %v", time.Since(start))
	}
	cancel()
}

// TC-71 (FR-10, A-16): with no gravity run WaitExternalGravity returns at
// once; with one it logs the PID, polls, and returns once it is gone.
func TestTC71_WaitExternalGravity(t *testing.T) {
	h := v5test.NewFakeHost(t)
	m := &v5.Mutator{Paths: h.Paths(), Runner: h, Now: h.Now, GravityPoll: 50 * time.Millisecond}
	start := time.Now()
	if err := m.WaitExternalGravity(context.Background(), io.Discard); err != nil || time.Since(start) > time.Second {
		t.Fatalf("no run: %v after %v", err, time.Since(start))
	}
	pid, stop := h.PlantGravityProcess()
	time.AfterFunc(300*time.Millisecond, stop)
	var out bytes.Buffer
	start = time.Now()
	if err := m.WaitExternalGravity(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Errorf("returned after %v, before the run ended", took)
	}
	if want := fmt.Sprintf("pihole: waiting for a gravity run (pid %d) to finish\n", pid); out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
	if len(h.Argvs()) != 0 {
		t.Errorf("commands ran: %q", h.Argvs())
	}
}

// TC-ERR30 (A-4): a gravity run that never ends gives pihole-busy after 30
// minutes of polling, and nothing else runs.
func TestTCERR30_PiholeBusyAfter30Minutes(t *testing.T) {
	h := v5test.NewFakeHost(t)
	polls := 0
	m := &v5.Mutator{Paths: h.Paths(), Runner: h, Now: h.Now, Sleep: func(_ context.Context, d time.Duration) error {
		if d != 2*time.Second {
			t.Errorf("poll interval %v, want 2s", d)
		}
		polls++
		h.Advance(d)
		return nil
	}}
	h.PlantGravityProcess()
	err := m.WaitExternalGravity(context.Background(), io.Discard)
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.PiholeBusy {
		t.Fatalf("err %v, want pihole-busy", err)
	}
	if s, ok := pe.Extensions["waitedSeconds"].(int); !ok || s < 1800 {
		t.Errorf("waitedSeconds %v, want >= 1800", pe.Extensions["waitedSeconds"])
	}
	if polls != 900 {
		t.Errorf("%d polls, want 900 (30 min every 2 s)", polls)
	}
	if len(h.Argvs()) != 0 {
		t.Errorf("commands ran: %q", h.Argvs())
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testBindings adds test bindings to the provider.
type testBindings struct {
	*Provider
	extra []provider.Binding
}

func (p testBindings) Bindings() []provider.Binding {
	return append(p.Provider.Bindings(), p.extra...)
}

// TC-73 (FR-29): an op run through the Dispatcher, the operation manager
// and the helper over a host whose setupVars.conf holds a WEBPASSWORD hash
// leaves the hash in no result.json, output or audit record; no pihole op
// declares Sensitive.
func TestTC73_NoSecretInHelperOutput(t *testing.T) {
	w := newHelperWorld(t)
	if !bytes.Contains(mustRead(t, filepath.Join(w.h.Root(), v5.SetupVarsFile)), []byte(v5test.WebPasswordHash)) {
		t.Fatal("the fake host has no WEBPASSWORD line")
	}
	stateDir, auditDir := filepath.Dir(w.opsDir), t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var stdout, stderr syncBuffer
	launcher, err := detach.New(detach.Config{OpsDir: w.opsDir, Logger: log}, detach.Options{
		PollInterval: 20 * time.Millisecond,
		// The unit: the helper runs in-process over the fake host, then
		// exit.json is written, as op-supervise does.
		Launch: func(_ context.Context, inv detach.Invocation) error {
			if err := os.Remove(filepath.Join(inv.Dir, detach.SpecFile)); err != nil {
				return err
			}
			started := time.Now()
			code := runHelper(context.Background(), inv.Dir, &stdout, &stderr, w.inject())
			return detach.WriteExit(inv.Dir, detach.Exit{ExitCode: code, Started: started, Ended: time.Now()})
		},
		UnitState: func(context.Context, string) (detach.UnitState, error) {
			return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
		},
		Signal:    func(context.Context, string, syscall.Signal) error { return errors.New("no signals") },
		ListUnits: func(context.Context) ([]string, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	alog, err := audit.Open(audit.Options{Dir: auditDir, Host: "h", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alog.Close() })
	st, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: w.opsDir, Host: "h", Logger: log, Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("manager shutdown: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})
	tp := testBindings{Provider: w.p, extra: []provider.Binding{
		detachedOp(w.p, blockingOp, nil, maxRuntimeDefault, nil),
	}}
	reg, err := core.New([]provider.Provider{tp}, core.Options{Host: "h", Logger: log, Audit: alog, Ops: mgr,
		Providers: pfConfigs(true, nil)})
	if err != nil {
		t.Fatal(err)
	}
	disp := core.NewDispatcher(reg, core.DispatcherOptions{Logger: log})
	res, err := disp.Invoke(context.Background(), core.InvokeRequest{
		Caller:  auth.Principal{TokenID: "t73", Name: "tc73", Scopes: []string{spec.ReadScope, spec.WriteScope}},
		Surface: core.SurfaceREST, Op: blockingOp.Op(), Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var rec store.OpRecord
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err = st.Get(res.OperationID)
		if err == nil && rec.Status.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s: %+v (%v)", res.OperationID, rec, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s: %v", rec.Status, rec.Error)
	}
	var out blockingOut
	if err := json.Unmarshal(rec.Result, &out); err != nil || out.State != "enabled" {
		t.Errorf("result %s (%v), want state enabled", rec.Result, err)
	}
	if !strings.Contains(stdout.String(), "pihole: blocking is enabled") {
		t.Errorf("helper output %q", stdout.String())
	}

	hash := []byte(v5test.WebPasswordHash)
	for what, b := range map[string][]byte{
		"result.json":   mustRead(t, filepath.Join(w.opsDir, res.OperationID, resultFile)),
		"input.json":    mustRead(t, filepath.Join(w.opsDir, res.OperationID, inputFile)),
		"stdout":        []byte(stdout.String()),
		"stderr":        []byte(stderr.String()),
		"audit":         mustRead(t, filepath.Join(auditDir, audit.FileName)),
		"stored result": rec.Result,
	} {
		if bytes.Contains(b, hash) {
			t.Errorf("the WEBPASSWORD hash is in %s", what)
		}
	}
	if !bytes.Contains(mustRead(t, filepath.Join(auditDir, audit.FileName)), []byte(blockingOp.Op().ID)) {
		t.Error("the audit file has no record of the op")
	}
	for _, op := range append(spec.Spec().Ops, blockingOp.Op()) {
		if len(op.Sensitive) != 0 {
			t.Errorf("op %s declares Sensitive %v", op.ID, op.Sensitive)
		}
	}
}

// CR-8: the provider's own seams, which the reads use, default to the
// production ones: FTL's DNS on 127.0.0.1:53, the real exec runner, paths
// under "/" and the real systemd check.
func TestNewProductionSeams(t *testing.T) {
	p := New(Options{})
	if p.dnsServer != netip.MustParseAddrPort("127.0.0.1:53") {
		t.Errorf("DNS server %v, want 127.0.0.1:53", p.dnsServer)
	}
	if _, ok := p.runner.(v5.Exec); !ok {
		t.Errorf("runner %T, want v5.Exec", p.runner)
	}
	if p.paths != (v5.Paths{}) {
		t.Errorf("paths %+v, want the zero Paths (production, under /)", p.paths)
	}
	if reflect.ValueOf(p.systemd).Pointer() != reflect.ValueOf(detach.SystemdAvailable).Pointer() {
		t.Error("systemd check is not detach.SystemdAvailable")
	}
	if d := p.deps(); d.DNSServer != p.dnsServer || d.Runner != p.runner || d.Reload != (v5.ReloadTiming{}) {
		t.Errorf("backend deps %+v", d)
	}
}
