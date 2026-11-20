// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-89 to TC-SV-101
// and TC-SV-ERR22 to TC-SV-ERR24 (WI 4.3): start, stop, restart and
// control, their admission, lane, Prepare and Finalize, and the
// services-helper's control mode, over the T1 captures through F-HOSTS
// (svcctltest.NewHost) and F-ROOTS (testRoot).
//
// The helper runs in process (runHelper) with F-HOSTS as its svcctl
// runner. The stack tests run the real Dispatcher, ops manager, store and
// audit log, with a detach launcher whose unit is that in-process helper,
// started on its own goroutine so a test can hold an operation Running.

package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
	"github.com/mauktik/nexus/internal/systemspec"
)

const (
	ctlSystemd = "t1-ubuntu-systemd"
	ctlOpenRC  = "t1-alpine-openrc"
	ctlExe     = "/usr/local/bin/nexusd"
	ctlToken   = "tokW"
	// ctlPoll is the helper's poll interval in the tests that do not time
	// the wait (the seam TC-SV-90 allows).
	ctlPoll = 10 * time.Millisecond
	// ctlAgentDepsReason is why nexusd-db is protected when the unit
	// running nexusd is nexus-agent.service and Requires= it (TC-SV-77):
	// WI 4.1's wording (protect.go dependencyReason).
	ctlAgentDepsReason = "Requires= of nexus-agent.service, the unit running nexusd"
)

// ctlOptions shape a ctlWorld.
type ctlOptions struct {
	// names are the allow-listed names preflight's `show -p LoadState,Id`
	// covers (systemd), in the allow-list's order.
	names []string
	// ids overrides a name's Id in that show (an alias).
	ids map[string]string
	// agent makes nexus-agent.service the unit running nexusd, with
	// Requires=nexusd-db.service: TC-SV-77's dynamic protection.
	agent bool
	// setup runs on the fake before the provider's first preflight.
	setup func(fh *svcctltest.Host)
}

// ctlWorld is the provider over one capture, configured with an
// allow-list and preflighted, with an operations directory.
type ctlWorld struct {
	t       *testing.T
	capture string
	fh      *svcctltest.Host
	root    string
	p       *Provider
	opsDir  string
	allow   string
	seq     int
	// post, when set, rewrites the fake's Result for the helper's calls
	// after the fake modeled them, so the model's state change stands.
	post func(argv []string, res svcctl.Result) svcctl.Result
}

func newCtlWorld(t *testing.T, capture, allow string, o ctlOptions) *ctlWorld {
	t.Helper()
	root := testRoot(t, capture)
	fh := svcctltest.NewHost(t, capture)
	if o.agent {
		cg := filepath.Join(root, "proc", strconv.Itoa(os.Getpid()), "cgroup")
		b, err := os.ReadFile(cg)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, cg, strings.Replace(string(b), "/nexusd.service", "/nexus-agent.service", 1))
		deps, err := svcctltest.CaptureOutput(capture, "deps-nexusd.txt")
		if err != nil {
			t.Fatal(err)
		}
		out := strings.Replace(string(deps.Output), "Requires=", "Requires=nexusd-db.service ", 1)
		fh.Answer([]string{systemctlTool, "show", "-p", "Requires", "-p", "BindsTo", "-p", "Requisite", "-p", "PartOf",
			"--", "nexus-agent.service"}, svcctl.Result{Output: []byte(out)}, nil)
	}
	if fh.Init() == platform.Systemd && len(o.names) > 0 {
		answerLoadStates(t, fh, capture, o.ids, o.names...)
	}
	if o.setup != nil {
		o.setup(fh)
	}
	p := newOn(t, root, fh)
	p.executable = ctlExe
	w := &ctlWorld{t: t, capture: capture, fh: fh, root: root, p: p, opsDir: t.TempDir(), allow: allow}
	p.UseOpsDir(w.opsDir)
	configure(t, p, section(t, allow))
	wantAvailable(t, p.Preflight(context.Background()))
	return w
}

// env is the helper's seams: F-HOSTS and the world's roots.
func (w *ctlWorld) env(poll time.Duration) helperEnv {
	run := w.fh.Run
	if w.post != nil {
		run = func(ctx context.Context, argv []string) (svcctl.Result, error) {
			res, err := w.fh.Run(ctx, argv)
			if err == nil {
				res = w.post(argv, res)
			}
			return res, err
		}
	}
	return helperEnv{control: controlHelperEnv{Run: run, Poll: poll, ProcRoot: filepath.Join(w.root, "proc"),
		RunRoot: filepath.Join(w.root, "run"), EtcRoot: filepath.Join(w.root, "etc")}}
}

// funcs is op's detached binding among the control bindings.
func (w *ctlWorld) funcs(op def.Op) (provider.Binding, provider.DetachedFuncs) {
	w.t.Helper()
	for _, b := range w.p.controlBindings() {
		if b.Op().ID == op.ID {
			f, ok := b.Detached()
			if !ok {
				w.t.Fatalf("%s is not detached", op.ID)
			}
			return b, f
		}
	}
	w.t.Fatalf("no control binding for %s", op.ID)
	return provider.Binding{}, provider.DetachedFuncs{}
}

func (w *ctlWorld) decode(op def.Op, input string) any {
	w.t.Helper()
	b, _ := w.funcs(op)
	in, err := b.Decode(json.RawMessage(input))
	if err != nil {
		w.t.Fatalf("decode %s %s: %v", op.ID, input, err)
	}
	return in
}

// ctlRun is one operation run through Prepare, the helper and Finalize.
type ctlRun struct {
	id, dir string
	spec    detach.Spec
	code    int
	log     string // The helper's stdout and stderr: the operation log
	res     spec.ServiceResult
	err     error // Finalize's
}

// prepare runs op's Prepare on input in a new operation directory.
func (w *ctlWorld) prepare(op def.Op, input string) ctlRun {
	w.t.Helper()
	w.seq++
	r := ctlRun{id: fmt.Sprintf("op-%d", w.seq)}
	_, f := w.funcs(op)
	sp, err := f.Prepare(context.Background(), w.decode(op, input), provider.NewRun(r.id, nil))
	if err != nil {
		w.t.Fatalf("Prepare %s %s: %v", op.ID, input, err)
	}
	r.spec, r.dir = sp, filepath.Join(w.opsDir, r.id)
	return r
}

// helper runs the helper as the spec's argv does, with poll.
func (w *ctlWorld) helper(r *ctlRun, poll time.Duration) {
	w.t.Helper()
	if len(r.spec.Argv) < 2 || r.spec.Argv[1] != HelperCommand {
		w.t.Fatalf("argv %q does not run %s", r.spec.Argv, HelperCommand)
	}
	var out bytes.Buffer
	r.code = runHelper(context.Background(), r.spec.Argv[2:], &out, &out, w.env(poll))
	r.log = out.String()
}

// finalize runs op's Finalize on the run's directory.
func (w *ctlWorld) finalize(op def.Op, input string, r *ctlRun) {
	w.t.Helper()
	_, f := w.funcs(op)
	out, err := f.Finalize(context.Background(), w.decode(op, input), detach.Exit{ExitCode: r.code},
		provider.NewRun(r.id, nil))
	r.err = err
	if err == nil {
		res, ok := out.(spec.ServiceResult)
		if !ok {
			w.t.Fatalf("Finalize returned %T, want spec.ServiceResult", out)
		}
		r.res = res
	}
}

// act runs op on input end to end, with the helper polling every poll.
func (w *ctlWorld) act(op def.Op, input string, poll time.Duration) ctlRun {
	w.t.Helper()
	r := w.prepare(op, input)
	w.helper(&r, poll)
	w.finalize(op, input, &r)
	return r
}

// do sends a to name through the fake directly, as setup: it is not the
// code under test, and the tests count Sent() from after it.
func (w *ctlWorld) do(a svcctl.Action, name string) {
	w.t.Helper()
	c, err := svcctl.New(w.fh.Init(), svcctl.Options{Run: w.fh.Run})
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := c.Do(context.Background(), a, name); err != nil {
		w.t.Fatalf("setup %s %s: %v", a, name, err)
	}
}

// show is the fake's systemd unit properties for name now.
func (w *ctlWorld) show(name string) map[string]string {
	w.t.Helper()
	res, err := w.fh.Run(context.Background(), showArgv(name))
	if err != nil {
		w.t.Fatal(err)
	}
	kv := map[string]string{}
	for line := range strings.Lines(string(res.Output)) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func ctlUnixTime(t *testing.T, v string) *time.Time {
	t.Helper()
	s, ok := strings.CutPrefix(v, "@")
	if !ok {
		return nil
	}
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	tm := time.Unix(sec, 0).UTC()
	return &tm
}

// sentSince is the fake's Sent() after the first n.
func sentSince(fh *svcctltest.Host, n int) [][]string {
	s := fh.Sent()
	if n > len(s) {
		return nil
	}
	return s[n:]
}

func systemdArgv(verb, name string) []string {
	return []string{systemctlTool, verb, "--", name + ".service"}
}

func openrcArgv(verb, name string) []string {
	return []string{"/sbin/rc-service", "-C", name, verb}
}

func wantSent(t *testing.T, got [][]string, want ...[]string) {
	t.Helper()
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("Sent() = %q, want %q", got, want)
	}
}

// wantProblem requires err to be a *problem.Error of slug.
func wantProblem(t *testing.T, err error, slug problem.Slug) *problem.Error {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want problem %s", err, err, slug)
	}
	if pe.Slug() != slug {
		t.Fatalf("problem = %s (%s), want %s", pe.Slug(), pe.Detail, slug)
	}
	return pe
}

// readResult is the helper's result.json in dir.
func readResult(t *testing.T, dir string) helperResult {
	t.Helper()
	var res helperResult
	ok, err := readHelperJSON(dir, helperResultFile, &res)
	if err != nil || !ok {
		t.Fatalf("result.json: ok %v, %v", ok, err)
	}
	return res
}

func extString(t *testing.T, e *helperError, key string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(e.Ext[key], &s); err != nil {
		t.Fatalf("extension %s = %s: %v", key, e.Ext[key], err)
	}
	return s
}

// ---- The stack: real Dispatcher, ops manager, store and audit log. ----

// ctlProvider serves the provider's control bindings, which WI 5.1 adds to
// Bindings.
type ctlProvider struct{ *Provider }

func (c ctlProvider) Bindings() []provider.Binding { return c.Provider.controlBindings() }

// ctlBuffer is a bytes.Buffer safe for concurrent use.
type ctlBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *ctlBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *ctlBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type ctlStack struct {
	*ctlWorld
	st       *store.Store
	disp     *core.Dispatcher
	auditDir string
	out      ctlBuffer
	quit     chan struct{}
	wg       sync.WaitGroup

	mu       sync.Mutex
	gate     func(inv detach.Invocation) // Runs before each helper, on its goroutine
	launches int
}

func newCtlStack(t *testing.T, w *ctlWorld) *ctlStack {
	t.Helper()
	s := &ctlStack{ctlWorld: w, quit: make(chan struct{})}
	log := slog.New(slog.DiscardHandler)
	stateDir := t.TempDir()
	s.auditDir = t.TempDir()
	opsDir := filepath.Join(stateDir, "ops")
	w.opsDir = opsDir
	w.p.UseOpsDir(opsDir)
	launcher, err := detach.New(detach.Config{OpsDir: opsDir, Logger: log}, detach.Options{
		PollInterval: 10 * time.Millisecond,
		Launch:       s.launch,
		UnitState: func(context.Context, string) (detach.UnitState, error) {
			return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
		},
		Signal:    func(context.Context, string, syscall.Signal) error { return nil },
		ListUnits: func(context.Context) ([]string, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	alog, err := audit.Open(audit.Options{Dir: s.auditDir, Host: "h1", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.st = st
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: opsDir, Host: "h1", Logger: log, Detach: launcher})
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
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
	})
	// Runs first: no helper goroutine outlives the test.
	t.Cleanup(func() {
		close(s.quit)
		s.wg.Wait()
	})
	reg, err := core.New([]provider.Provider{ctlProvider{w.p}}, core.Options{Host: "h1", Logger: log, Audit: alog,
		Ops: mgr, Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: section(t, w.allow)}}})
	if err != nil {
		t.Fatal(err)
	}
	s.disp = core.NewDispatcher(reg, core.DispatcherOptions{Logger: log})
	return s
}

// launch is the operation's unit: the helper from spec.json's argv, run
// in process on its own goroutine after the gate, then exit.json.
func (s *ctlStack) launch(_ context.Context, inv detach.Invocation) error {
	b, err := os.ReadFile(filepath.Join(inv.Dir, detach.SpecFile))
	if err != nil {
		return err
	}
	var sp struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(b, &sp); err != nil {
		return err
	}
	if len(sp.Argv) < 2 || sp.Argv[1] != HelperCommand {
		return fmt.Errorf("argv %q does not run %s", sp.Argv, HelperCommand)
	}
	s.mu.Lock()
	s.launches++
	gate := s.gate
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if gate != nil {
			gate(inv)
		}
		started := time.Now()
		code := runHelper(context.Background(), sp.Argv[2:], &s.out, &s.out, s.env(ctlPoll))
		_ = detach.WriteExit(inv.Dir, detach.Exit{ExitCode: code, Started: started, Ended: time.Now()})
	}()
	return nil
}

func (s *ctlStack) setGate(g func(inv detach.Invocation)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = g
}

// hold returns a gate that holds the operation whose input names name,
// signaling entered once it is held, until release is closed or the test
// ends; other operations pass.
func (s *ctlStack) hold(name string, entered chan<- string, release <-chan struct{}) func(detach.Invocation) {
	return func(inv detach.Invocation) {
		b, err := os.ReadFile(filepath.Join(inv.Dir, helperInputFile))
		if err != nil || !bytes.Contains(b, []byte(`"service":"`+name+`"`)) {
			return
		}
		entered <- inv.ID
		select {
		case <-release:
		case <-s.quit:
		}
	}
}

func (s *ctlStack) launched() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.launches
}

// invoke calls op through the Dispatcher as tokW.
func (s *ctlStack) invoke(op def.Op, input, key string) (core.InvokeResult, error) {
	return s.disp.Invoke(context.Background(), core.InvokeRequest{
		Caller:  auth.Principal{TokenID: ctlToken, Name: "writer", Scopes: []string{spec.ReadScope, spec.WriteScope}},
		Surface: core.SurfaceREST, Op: op, Input: json.RawMessage(input), IdempotencyKey: key,
	})
}

// accepted invokes op and requires 202.
func (s *ctlStack) accepted(op def.Op, input, key string) string {
	s.t.Helper()
	res, err := s.invoke(op, input, key)
	if err != nil || res.Status != 202 || res.OperationID == "" {
		s.t.Fatalf("%s %s = %d %q (%v), want 202", op.ID, input, res.Status, res.OperationID, err)
	}
	return res.OperationID
}

// wait polls operation id until ok holds of it.
func (s *ctlStack) wait(id string, ok func(store.OpRecord) bool) store.OpRecord {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rec, err := s.st.Get(id)
		if err == nil && ok(rec) {
			return rec
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("operation %s: %+v (%v)", id, rec, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func terminal(r store.OpRecord) bool { return r.Status.Terminal() }

func running(r store.OpRecord) bool { return r.Status == apitypes.StatusRunning }

func (s *ctlStack) operations() []store.OpRecord {
	s.t.Helper()
	page, err := s.st.List(store.ListQuery{Limit: 1000})
	if err != nil {
		s.t.Fatal(err)
	}
	return page.Records
}

func (s *ctlStack) auditRecords() []systemspec.AuditRecord {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.auditDir, audit.FileName))
	if err != nil {
		s.t.Fatal(err)
	}
	var recs []systemspec.AuditRecord
	for line := range strings.Lines(string(b)) {
		var r systemspec.AuditRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			s.t.Fatalf("audit line %q: %v", line, err)
		}
		recs = append(recs, r)
	}
	return recs
}

// ---- Tests ----

// TC-SV-89: Admission refuses in the DF-3 order, creates nothing and sends nothing. Owner WI 4.3. Requirements: FR-7, FR-8, FR-9, NFR-4.
//
// cron is in no T1 capture, so its show is planted as a loaded, inactive
// unit: the refusal under test is the allow-list's, which comes after the
// init system knows the name.
func TestTCSV89_AdmissionRefusesInTheDF3Order(t *testing.T) {
	names := []string{"nx-svc-test", "nx-missing", "nx-netalias"}
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n    actions: [restart]\n"+
		"  - name: nx-missing\n  - name: nx-netalias\n", ctlOptions{
		names: names, ids: map[string]string{"nx-netalias": "systemd-networkd.service"}, agent: true,
		setup: func(fh *svcctltest.Host) {
			fh.Answer(showArgv("nx-netalias"), svcctl.Result{Output: []byte("Id=systemd-networkd.service\n" +
				"Description=Network Management\nLoadState=loaded\nActiveState=inactive\nSubState=dead\n" +
				"FragmentPath=/usr/lib/systemd/system/systemd-networkd.service\nUnitFileState=disabled\n" +
				"ActiveEnterTimestamp=\nInactiveEnterTimestamp=\nMainPID=0\nResult=success\nNRestarts=0\n" +
				"MemoryCurrent=[not set]\n")}, nil)
			fh.Answer(showArgv("cron"), svcctl.Result{Output: []byte("Id=cron.service\nDescription=cron\n" +
				"LoadState=loaded\nActiveState=inactive\nSubState=dead\nFragmentPath=/usr/lib/systemd/system/cron.service\n" +
				"UnitFileState=enabled\nActiveEnterTimestamp=\nInactiveEnterTimestamp=\nMainPID=0\nResult=success\n" +
				"NRestarts=0\nMemoryCurrent=[not set]\n")}, nil)
		}})
	s := newCtlStack(t, w)
	for _, c := range []struct {
		step    string
		input   string
		slug    problem.Slug
		status  int
		ext     map[string]string
		reason  string // The reason extension contains it
		noCalls bool
	}{
		{"1 start ssh", `{"name":"ssh"}`, problem.ProtectedService, 409, map[string]string{"service": "ssh"}, "ssh", true},
		{"2 start nexusd-db", `{"name":"nexusd-db"}`, problem.ProtectedService, 409,
			map[string]string{"service": "nexusd-db"}, ctlAgentDepsReason, true},
		{"3 start nx-missing", `{"name":"nx-missing"}`, problem.NotFound, 404, nil, "", false},
		{"4 start nx-netalias", `{"name":"nx-netalias"}`, problem.ProtectedService, 409,
			map[string]string{"service": "nx-netalias"}, "systemd-networkd", false},
		{"5 start cron", `{"name":"cron"}`, problem.NotAllowed, 409,
			map[string]string{"service": "cron", "action": "start", "configKey": "providers.services.control"}, "", false},
		{"6 start nx-svc-test", `{"name":"nx-svc-test"}`, problem.NotAllowed, 409,
			map[string]string{"service": "nx-svc-test", "action": "start", "configKey": "providers.services.control"}, "", false},
	} {
		t.Run(c.step, func(t *testing.T) {
			calls, sent := len(w.fh.Calls()), len(w.fh.Sent())
			_, err := s.invoke(spec.Start.Op(), c.input, "")
			pe := wantProblem(t, err, c.slug)
			if pe.Status != c.status {
				t.Errorf("status %d, want %d", pe.Status, c.status)
			}
			for k, v := range c.ext {
				if got := pe.Extensions[k]; got != v {
					t.Errorf("%s = %v, want %q", k, got, v)
				}
			}
			if c.reason != "" {
				if r, _ := pe.Extensions["reason"].(string); !strings.Contains(r, c.reason) {
					t.Errorf("reason %q, want it to name %q", r, c.reason)
				}
			}
			if n := len(s.operations()); n != 0 {
				t.Errorf("the store holds %d operations", n)
			}
			if got := sentSince(w.fh, sent); len(got) != 0 {
				t.Errorf("sent %q", got)
			}
			if c.noCalls {
				if got := w.fh.Calls()[calls:]; len(got) != 0 {
					t.Errorf("refused by name, yet called %v", got)
				}
			}
			recs := s.auditRecords()
			last := recs[len(recs)-1]
			if last.Outcome != audit.OutcomeRefused || last.Problem != string(c.slug) || last.Action != spec.Start.Op().ID {
				t.Errorf("last audit record %+v, want refused %s", last, c.slug)
			}
		})
	}
	t.Run("7 restart nx-svc-test", func(t *testing.T) {
		id := s.accepted(spec.Restart.Op(), `{"name":"nx-svc-test"}`, "")
		if rec := s.wait(id, terminal); rec.Status != apitypes.StatusSucceeded {
			t.Errorf("restart %s: %v", rec.Status, rec.Error)
		}
	})
}

// TC-SV-90: start brings a stopped service to running and reports the final state. Owner WI 4.3. Requirements: FR-7, FR-14.
//
// The argv is svcctl's, with its "--" before the unit. contentTrust is the
// MCP adapter's label for an op that declares Untrusted (mcpadapter
// withContentTrust), so it is asserted on the declaration.
func TestTCSV90_StartBringsAStoppedServiceToRunning(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n",
		ctlOptions{names: []string{"nx-svc-test"}})
	w.do(svcctl.Stop, "nx-svc-test")
	w.fh.SetSettle(2)
	n := len(w.fh.Sent())
	r := w.act(spec.Start.Op(), `{"name":"nx-svc-test"}`, ctlPoll)
	if r.code != 0 || r.err != nil {
		t.Fatalf("exit %d, Finalize %v\n%s", r.code, r.err, r.log)
	}
	kv := w.show("nx-svc-test")
	pid, _ := strconv.Atoi(kv["MainPID"])
	since := ctlUnixTime(t, kv["ActiveEnterTimestamp"])
	got := r.res
	if !got.Changed || got.Service != "nx-svc-test" || got.Action != "start" || got.State != "running" ||
		got.RawState != "active (running)" {
		t.Errorf("result %+v", got)
	}
	if got.MainPID == nil || *got.MainPID != pid || pid == 0 {
		t.Errorf("mainPid %v, want the fake's %d", got.MainPID, pid)
	}
	if got.Since == nil || since == nil || !got.Since.Equal(*since) {
		t.Errorf("since %v, want the fake's ActiveEnterTimestamp %v", got.Since, since)
	}
	wantSent(t, sentSince(w.fh, n), systemdArgv("start", "nx-svc-test"))
	if !strings.Contains(r.log, "services-op: ") || !strings.Contains(r.log, "start nx-svc-test exited 0") {
		t.Errorf("operation log %q, want services-op: lines with the init system's answer", r.log)
	}
	if !spec.Start.Op().Untrusted || !spec.Control.Op().Untrusted {
		t.Error("start and control must declare Untrusted, which labels results contentTrust: untrusted-host-data")
	}
}

// TC-SV-91: start on a running service and stop on a stopped one send nothing and report changed false. Owner WI 4.3. Requirements: FR-7.
func TestTCSV91_StartOnARunningServiceAndStop(t *testing.T) {
	for _, capture := range []string{ctlSystemd, ctlOpenRC} {
		t.Run(capture, func(t *testing.T) {
			for _, c := range []struct {
				op      def.Op
				stopped bool
				state   string
			}{{spec.Start.Op(), false, "running"}, {spec.Stop.Op(), true, "stopped"}} {
				t.Run(c.op.Action, func(t *testing.T) {
					w := newCtlWorld(t, capture, "enabled: true\ncontrol:\n  - name: nx-svc-test\n",
						ctlOptions{names: []string{"nx-svc-test"}})
					if c.stopped {
						w.do(svcctl.Stop, "nx-svc-test")
					}
					before, err := svcctl.Describe(context.Background(), w.fh.Init(), "nx-svc-test", w.p.svcctlOptions())
					if err != nil || before.State != c.state {
						t.Fatalf("setup: %s (%v), want %s", before.State, err, c.state)
					}
					n := len(w.fh.Sent())
					r := w.act(c.op, `{"name":"nx-svc-test"}`, ctlPoll)
					if r.err != nil {
						t.Fatalf("%v\n%s", r.err, r.log)
					}
					if r.res.Changed || r.res.State != before.State || r.res.RawState != before.RawState {
						t.Errorf("result %+v, want changed false in %s (%s)", r.res, before.State, before.RawState)
					}
					wantSent(t, sentSince(w.fh, n))
				})
			}
		})
	}
}

// TC-SV-92: stop and restart reach their target states, restart with a new main PID. Owner WI 4.3. Requirements: FR-7.
//
// The fake is reset to running by a new world over the same capture.
func TestTCSV92_StopAndRestartReachTheirTargetStates(t *testing.T) {
	const allow = "enabled: true\ncontrol:\n  - name: nx-svc-test\n"
	o := ctlOptions{names: []string{"nx-svc-test"}}
	t.Run("stop", func(t *testing.T) {
		w := newCtlWorld(t, ctlSystemd, allow, o)
		n := len(w.fh.Sent())
		r := w.act(spec.Stop.Op(), `{"name":"nx-svc-test"}`, ctlPoll)
		if r.err != nil {
			t.Fatalf("%v\n%s", r.err, r.log)
		}
		if !r.res.Changed || r.res.State != "stopped" || r.res.MainPID != nil {
			t.Errorf("result %+v, want stopped, changed, no mainPid", r.res)
		}
		wantSent(t, sentSince(w.fh, n), systemdArgv("stop", "nx-svc-test"))
	})
	t.Run("restart", func(t *testing.T) {
		w := newCtlWorld(t, ctlSystemd, allow, o)
		kv := w.show("nx-svc-test")
		p0, _ := strconv.Atoi(kv["MainPID"])
		t0 := ctlUnixTime(t, kv["ActiveEnterTimestamp"])
		if kv["ActiveState"] != "active" || p0 == 0 {
			t.Fatalf("setup: %v", kv)
		}
		n := len(w.fh.Sent())
		r := w.act(spec.Restart.Op(), `{"name":"nx-svc-test"}`, ctlPoll)
		if r.err != nil {
			t.Fatalf("%v\n%s", r.err, r.log)
		}
		if !r.res.Changed || r.res.State != "running" || r.res.MainPID == nil || *r.res.MainPID == p0 {
			t.Errorf("result %+v, want running, changed, mainPid other than %d", r.res, p0)
		}
		if r.res.Since == nil || (t0 != nil && !r.res.Since.After(*t0)) {
			t.Errorf("since %v, want after %v", r.res.Since, t0)
		}
		wantSent(t, sentSince(w.fh, n), systemdArgv("restart", "nx-svc-test"))
	})
}

// TC-SV-93: A stop of a failed unit that stays failed with no main PID succeeds with changed false. Owner WI 4.3. Requirements: FR-7.
func TestTCSV93_AStopOfAFailedUnitThat(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-fail\n",
		ctlOptions{names: []string{"nx-svc-fail"}})
	if kv := w.show("nx-svc-fail"); kv["ActiveState"] != "failed" {
		t.Fatalf("setup: nx-svc-fail %v", kv)
	}
	n := len(w.fh.Sent())
	r := w.act(spec.Stop.Op(), `{"name":"nx-svc-fail"}`, ctlPoll)
	if r.err != nil {
		t.Fatalf("Finalize: %v, want success\n%s", r.err, r.log)
	}
	if r.res.Changed || r.res.State != "failed" || r.res.MainPID != nil {
		t.Errorf("result %+v, want failed, changed false, no mainPid", r.res)
	}
	wantSent(t, sentSince(w.fh, n), systemdArgv("stop", "nx-svc-fail"))
}

// exitCodeOf is e's exitCode extension as JSON.
func exitCodeOf(e *problem.Error) string {
	b, _ := json.Marshal(e.Extensions["exitCode"])
	return string(b)
}

// TC-SV-94: A second action on the same service answers 409 conflict, while another service runs in parallel. Owner WI 4.3. Requirements: FR-7.
func TestTCSV94_ASecondActionOnTheSameService(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n  - name: nx-svc-fail\n",
		ctlOptions{names: []string{"nx-svc-test", "nx-svc-fail"}})
	for _, op := range []def.Op{spec.Start.Op(), spec.Stop.Op(), spec.Restart.Op()} {
		_, f := w.funcs(op)
		lane, err := f.Lane(context.Background(), w.decode(op, `{"name":"nx-svc-test"}`))
		if err != nil || lane != (provider.Lane{Key: "service:nx-svc-test", Policy: provider.LaneReject}) {
			t.Errorf("%s lane = %+v (%v), want service:nx-svc-test reject", op.ID, lane, err)
		}
	}
	s := newCtlStack(t, w)
	entered, release := make(chan string, 1), make(chan struct{})
	s.setGate(s.hold("nx-svc-test", entered, release))
	first := s.accepted(spec.Restart.Op(), `{"name":"nx-svc-test"}`, "")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the restart never launched")
	}
	s.wait(first, running)

	_, err := s.invoke(spec.Stop.Op(), `{"name":"nx-svc-test"}`, "")
	pe := wantProblem(t, err, problem.Conflict)
	if pe.Status != 409 || pe.Extensions["blockingOperationId"] != first {
		t.Errorf("conflict %+v, want blockingOperationId %s", pe, first)
	}
	if n := len(s.operations()); n != 1 {
		t.Errorf("%d operations, want the held one only", n)
	}

	other := s.accepted(spec.Start.Op(), `{"name":"nx-svc-fail"}`, "")
	rec := s.wait(other, terminal)
	// The capture's start of nx-svc-fail exits 1 (actions/start-nx-svc-fail.txt),
	// so the parallel operation ends Failed: it ran, which is what this test
	// needs, and it was not blocked by the held lane.
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.ServiceActionFailed ||
		exitCodeOf(rec.Error) != "1" {
		t.Errorf("start nx-svc-fail: %s %v, want Failed service-action-failed with exitCode 1 (the capture's)",
			rec.Status, rec.Error)
	}
	if r, _ := s.st.Get(first); r.Status != apitypes.StatusRunning {
		t.Errorf("the held restart is %s while nx-svc-fail ran, want running", r.Status)
	}
	close(release)
	if rec := s.wait(first, terminal); rec.Status != apitypes.StatusSucceeded {
		t.Errorf("restart: %s %v", rec.Status, rec.Error)
	}
}

// TC-SV-95: A reload during an operation keeps the operation's snapshot. Owner WI 4.3. Requirements: FR-1, FR-7.
func TestTCSV95_AReloadDuringAnOperationKeepsThe(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n    timeout: 1s\n",
		ctlOptions{names: []string{"nx-svc-test"}})
	w.do(svcctl.Stop, "nx-svc-test")
	w.fh.SetSettle(1 << 30)
	s := newCtlStack(t, w)
	entered, release := make(chan string, 1), make(chan struct{})
	s.setGate(s.hold("nx-svc-test", entered, release))
	id := s.accepted(spec.Start.Op(), `{"name":"nx-svc-test"}`, "")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the start never launched")
	}
	var in map[string]any
	b, err := os.ReadFile(filepath.Join(w.opsDir, id, helperInputFile))
	if err != nil || json.Unmarshal(b, &in) != nil {
		t.Fatalf("input.json: %v %s", err, b)
	}
	if in["timeoutSeconds"] != float64(1) {
		t.Errorf("timeoutSeconds = %v, want 1", in["timeoutSeconds"])
	}
	configure(t, w.p, section(t, "enabled: true\ncontrol:\n  - name: nx-svc-test\n    timeout: 10m\n"))
	configure(t, w.p, section(t, "enabled: true\ncontrol: []\n"))
	start := time.Now()
	close(release)
	rec := s.wait(id, terminal)
	took := time.Since(start)
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.OutcomeUnknown {
		t.Fatalf("operation %s %v, want failed outcome-unknown", rec.Status, rec.Error)
	}
	if took < time.Second || took > 2500*time.Millisecond {
		t.Errorf("ended %s after the action, want about 1s", took)
	}
}

// TC-SV-96: control with an action takes the same path and lane as the per-verb op. Owner WI 4.3. Requirements: FR-7, FR-15, NFR-6.
func TestTCSV96_ControlWithAnActionTakesTheSame(t *testing.T) {
	const allow = "enabled: true\ncontrol:\n  - name: nx-svc-test\n"
	o := ctlOptions{names: []string{"nx-svc-test"}}
	inputOf := func(t *testing.T, dir string) map[string]any {
		t.Helper()
		var m map[string]any
		b, err := os.ReadFile(filepath.Join(dir, helperInputFile))
		if err != nil || json.Unmarshal(b, &m) != nil {
			t.Fatalf("input.json: %v %s", err, b)
		}
		delete(m, "operationId")
		return m
	}
	run := func(t *testing.T, op def.Op, input string) (map[string]any, ctlRun) {
		w := newCtlWorld(t, ctlSystemd, allow, o)
		w.do(svcctl.Stop, "nx-svc-test")
		r := w.prepare(op, input)
		m := inputOf(t, r.dir)
		w.helper(&r, ctlPoll)
		w.finalize(op, input, &r)
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", op.ID, r.err, r.log)
		}
		return m, r
	}
	inControl, rControl := run(t, spec.Control.Op(), `{"name":"nx-svc-test","action":"start"}`)
	inStart, rStart := run(t, spec.Start.Op(), `{"name":"nx-svc-test"}`)
	if fmt.Sprint(inControl) != fmt.Sprint(inStart) || inControl["mode"] != "control" {
		t.Errorf("control's input.json %v, want start's %v", inControl, inStart)
	}
	a, _ := json.Marshal(rControl.res)
	b, _ := json.Marshal(rStart.res)
	if !bytes.Equal(a, b) || rControl.res.State != "running" {
		t.Errorf("control's result %s, want start's %s", a, b)
	}

	w := newCtlWorld(t, ctlSystemd, allow, o)
	_, f := w.funcs(spec.Control.Op())
	lane, err := f.Lane(context.Background(), w.decode(spec.Control.Op(), `{"name":"nx-svc-test","action":"stop"}`))
	if err != nil || lane != (provider.Lane{Key: "service:nx-svc-test", Policy: provider.LaneReject}) {
		t.Errorf("control lane = %+v (%v), want service:nx-svc-test reject", lane, err)
	}
	s := newCtlStack(t, w)
	entered, release := make(chan string, 1), make(chan struct{})
	s.setGate(s.hold("nx-svc-test", entered, release))
	first := s.accepted(spec.Restart.Op(), `{"name":"nx-svc-test"}`, "")
	<-entered
	_, err = s.invoke(spec.Control.Op(), `{"name":"nx-svc-test","action":"stop"}`, "")
	if pe := wantProblem(t, err, problem.Conflict); pe.Extensions["blockingOperationId"] != first {
		t.Errorf("conflict %+v, want blockingOperationId %s", pe, first)
	}
	_, err = s.invoke(spec.Control.Op(), `{"name":"ssh","action":"restart"}`, "")
	if pe := wantProblem(t, err, problem.ProtectedService); pe.Status != 409 || pe.Extensions["service"] != "ssh" {
		t.Errorf("control ssh: %+v", pe)
	}
	close(release)
	s.wait(first, terminal)
}

// TC-SV-97: The helper re-checks the name and its canonical Id against the snapshotted resolved protected set before any action. Owner WI 4.3. Requirements: FR-9.
//
// (b)'s reason is the provider's own wording for a Requires= dependency of
// the unit running nexusd (protect.go), not the test case's paraphrase.
func TestTCSV97_TheHelperReChecksTheStaticProtected(t *testing.T) {
	static := newProtectedSet(platform.Systemd, nil).entries
	reasonOf := func(name string) string {
		for _, e := range static {
			if e.Name == name {
				return e.Reason
			}
		}
		t.Fatalf("no static entry %s", name)
		return ""
	}
	for _, c := range []struct {
		name, service, canonical string
		protected                []protectEntry
		reason                   string // The result's reason contains it
		exact                    bool
	}{
		{"static", "ssh", "ssh", static, reasonOf("ssh"), true},
		{"dynamic only", "nexusd-db", "nexusd-db", []protectEntry{{Name: "nexusd-db", Reason: ctlAgentDepsReason}},
			ctlAgentDepsReason, true},
		{"alias", "nx-netalias", "systemd-networkd", static, reasonOf("systemd-networkd"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fh := svcctltest.NewHost(t, ctlSystemd)
			dir := t.TempDir()
			in := controlHelperInput{Mode: helperModeControl, OperationID: "op-x", Service: c.service, Action: "restart",
				Init: string(platform.Systemd), CanonicalName: c.canonical, TimeoutSeconds: 90, Actions: spec.Actions,
				Protected: c.protected}
			if err := writeHelperJSON(dir, helperInputFile, in); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			code := runHelper(context.Background(), []string{"run", "--dir", dir}, &out, &out,
				helperEnv{control: controlHelperEnv{Run: fh.Run, Poll: ctlPoll}})
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out.String())
			}
			res := readResult(t, dir)
			if res.Error == nil || res.Error.Slug != problem.ProtectedService {
				t.Fatalf("result %+v, want protected-service", res)
			}
			if got := extString(t, res.Error, "service"); got != c.service {
				t.Errorf("service %q, want %q", got, c.service)
			}
			reason := extString(t, res.Error, "reason")
			if c.exact && reason != c.reason || !strings.Contains(reason, c.reason) {
				t.Errorf("reason %q, want %q", reason, c.reason)
			}
			if c.canonical != c.service && !strings.Contains(reason, c.canonical) {
				t.Errorf("reason %q does not name the canonical %s", reason, c.canonical)
			}
			if calls := fh.Calls(); len(calls) != 0 {
				t.Errorf("the helper ran %v, want nothing", calls)
			}
		})
	}
}

// TC-SV-98: Prepare snapshots the entry and declares the helper argv and bounds. Owner WI 4.3. Requirements: FR-7, CON-1, CON-2.
func TestTCSV98_PrepareSnapshotsTheEntryAndDeclaresThe(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n    actions: [start, restart]\n"+
		"    timeout: 45s\n", ctlOptions{names: []string{"nx-svc-test"}, agent: true})
	r := w.prepare(spec.Start.Op(), `{"name":"nx-svc-test"}`)
	if want := []string{ctlExe, "services-helper", "run", "--dir", r.dir}; !slices.Equal(r.spec.Argv, want) {
		t.Errorf("argv %q, want %q", r.spec.Argv, want)
	}
	if r.spec.KillMode != detach.KillProcess || r.spec.MaxRuntime != 45*time.Second+2*30*time.Second+60*time.Second {
		t.Errorf("KillMode %s, MaxRuntime %s", r.spec.KillMode, r.spec.MaxRuntime)
	}
	if err := r.spec.Validate(); err != nil {
		t.Error(err)
	}
	path := filepath.Join(r.dir, helperInputFile)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("input.json: %v %v, want mode 0600", fi, err)
	}
	var in map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]any{"mode": "control", "service": "nx-svc-test", "action": "start",
		"timeoutSeconds": float64(45), "init": "systemd", "canonicalName": "nx-svc-test"} {
		if in[k] != v {
			t.Errorf("input.json %s = %v, want %v", k, in[k], v)
		}
	}
	if fmt.Sprint(in["actions"]) != "[start restart]" {
		t.Errorf("actions %v", in["actions"])
	}
	var names []string
	prot, _ := in["protected"].([]any)
	for _, e := range prot {
		m, _ := e.(map[string]any)
		if r, _ := m["reason"].(string); r == "" {
			t.Errorf("protected entry %v has no reason", m)
		}
		n, _ := m["name"].(string)
		names = append(names, n)
		// The dynamic protection (TC-SV-77: nexus-agent.service runs
		// nexusd and Requires= nexusd-db) is in the snapshot, with its
		// reason (CR-15).
		if n == "nexusd-db" && m["reason"] != ctlAgentDepsReason {
			t.Errorf("nexusd-db's reason %q, want %q", m["reason"], ctlAgentDepsReason)
		}
	}
	for _, want := range append([]string{"nexusd", "nexus-agent", "nexusd-db"}, spec.ProtectedSystemd...) {
		if !slices.Contains(names, want) {
			t.Errorf("protected names %v lack %s", names, want)
		}
	}
}

// TC-SV-99: OpenRC control through F-HOSTS uses rc-service and its captured outputs. Owner WI 4.3. Requirements: FR-7, CON-3.
//
// The argv is svcctl's, `rc-service -C NAME VERB`.
func TestTCSV99_OpenRCControlThroughFHOSTSUsesRc(t *testing.T) {
	w := newCtlWorld(t, ctlOpenRC, "enabled: true\ncontrol:\n  - name: nx-svc-test\n  - name: nx-svc-fail\n", ctlOptions{})
	w.do(svcctl.Stop, "nx-svc-test")
	n := len(w.fh.Sent())
	for _, c := range []struct {
		op    def.Op
		state string
	}{{spec.Start.Op(), "running"}, {spec.Restart.Op(), "running"}, {spec.Stop.Op(), "stopped"}} {
		r := w.act(c.op, `{"name":"nx-svc-test"}`, ctlPoll)
		if r.err != nil || r.res.State != c.state || !r.res.Changed {
			t.Errorf("%s: %+v %v, want %s\n%s", c.op.Action, r.res, r.err, c.state, r.log)
		}
	}
	r := w.act(spec.Start.Op(), `{"name":"nx-svc-fail"}`, ctlPoll)
	pe := wantProblem(t, r.err, problem.ServiceActionFailed)
	captured, err := svcctltest.CaptureOutput(ctlOpenRC, "actions/start-nx-svc-fail.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%s", pe.Extensions["exitCode"]) != "1" || !strings.Contains(pe.Detail, "ERROR: nx-svc-fail failed to start") ||
		captured.ExitCode != 1 {
		t.Errorf("problem %+v, want the captured output and exit code %d", pe, captured.ExitCode)
	}
	wantSent(t, sentSince(w.fh, n), openrcArgv("start", "nx-svc-test"), openrcArgv("restart", "nx-svc-test"),
		openrcArgv("stop", "nx-svc-test"), openrcArgv("start", "nx-svc-fail"))
}

// TC-SV-100: Idempotency-Key replays the same operation. Owner WI 4.3. Requirements: FR-7.
//
// The registry's slug for a key reused with another request is
// idempotency-key-reused, 422.
func TestTCSV100_IdempotencyKeyReplaysTheSameOperation(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n",
		ctlOptions{names: []string{"nx-svc-test"}})
	w.do(svcctl.Stop, "nx-svc-test")
	s := newCtlStack(t, w)
	n := len(w.fh.Sent())
	a := s.accepted(spec.Start.Op(), `{"name":"nx-svc-test"}`, "k1")
	b := s.accepted(spec.Start.Op(), `{"name":"nx-svc-test"}`, "k1")
	if a != b {
		t.Errorf("operations %s and %s, want one", a, b)
	}
	if rec := s.wait(a, terminal); rec.Status != apitypes.StatusSucceeded {
		t.Errorf("start: %s %v", rec.Status, rec.Error)
	}
	if got := s.launched(); got != 1 {
		t.Errorf("%d launches, want 1", got)
	}
	wantSent(t, sentSince(w.fh, n), systemdArgv("start", "nx-svc-test"))
	_, err := s.invoke(spec.Stop.Op(), `{"name":"nx-svc-test"}`, "k1")
	if pe := wantProblem(t, err, problem.IdempotencyKeyReused); pe.Status != 422 {
		t.Errorf("reused key: %+v", pe)
	}
}

// TC-SV-101: Start, stop and restart are audited requested then completed with their facts. Owner WI 4.3. Requirements: FR-16.
//
// A long-running op's terminal record is its operation state (succeeded),
// written by the ops manager with the op ID, target and token; the facts
// service and action are on the requested record, which admission fills.
func TestTCSV101_StartStopAndRestartAreAuditedRequested(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n",
		ctlOptions{names: []string{"nx-svc-test"}})
	w.do(svcctl.Stop, "nx-svc-test")
	s := newCtlStack(t, w)
	for _, c := range []struct {
		op     def.Op
		input  string
		action string
	}{
		{spec.Start.Op(), `{"name":"nx-svc-test"}`, "start"},
		{spec.Stop.Op(), `{"name":"nx-svc-test"}`, "stop"},
		{spec.Restart.Op(), `{"name":"nx-svc-test"}`, "restart"},
		{spec.Control.Op(), `{"name":"nx-svc-test","action":"stop"}`, "stop"},
	} {
		id := s.accepted(c.op, c.input, "")
		if rec := s.wait(id, terminal); rec.Status != apitypes.StatusSucceeded {
			t.Fatalf("%s: %s %v", c.op.ID, rec.Status, rec.Error)
		}
		var requested, ended *systemspec.AuditRecord
		recs := s.auditRecords()
		for i := range recs {
			r := &recs[i]
			if r.Action != c.op.ID || r.TokenID != ctlToken {
				continue
			}
			switch {
			case r.Outcome == audit.OutcomeRequested:
				requested = r
			case r.OperationID == id && r.Outcome == string(apitypes.StatusSucceeded):
				ended = r
			}
		}
		if requested == nil || requested.Facts["service"] != "nx-svc-test" || requested.Facts["action"] != c.action {
			t.Errorf("%s: requested record %+v, want facts service=nx-svc-test action=%s", c.op.ID, requested, c.action)
		}
		if ended == nil || ended.Target != "nx-svc-test" {
			t.Errorf("%s: terminal record %+v, want succeeded for nx-svc-test", c.op.ID, ended)
		}
	}
}

// TC-SV-ERR22: A failed start ends service-action-failed with the captured state, exit code and a 2 KiB quote. Owner WI 4.3. Requirements: FR-7, NFR-4.
func TestTCSVERR22_AFailedStartEndsServiceActionFailed(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-fail\n",
		ctlOptions{names: []string{"nx-svc-fail"}})
	captured, err := svcctltest.CaptureOutput(ctlSystemd, "actions/start-nx-svc-fail.txt")
	if err != nil {
		t.Fatal(err)
	}
	var padded []byte
	for i := 0; len(padded) < 5<<10; i++ {
		padded = append(padded, fmt.Sprintf("%04d %s", i, captured.Output)...)
	}
	// The fake models the failed start (state failed, exit 1, as captured);
	// only its output is replaced by the long one.
	w.post = func(argv []string, res svcctl.Result) svcctl.Result {
		if slices.Equal(argv, systemdArgv("start", "nx-svc-fail")) {
			res.Output = padded
		}
		return res
	}
	r := w.act(spec.Start.Op(), `{"name":"nx-svc-fail"}`, ctlPoll)
	pe := wantProblem(t, r.err, problem.ServiceActionFailed)
	for k, v := range map[string]string{"service": `"nx-svc-fail"`, "action": `"start"`, "state": `"failed"`,
		"rawState": `"failed (failed)"`, "exitCode": `1`} {
		if got := fmt.Sprintf("%s", pe.Extensions[k]); got != v {
			t.Errorf("%s = %s, want %s", k, got, v)
		}
	}
	if !strings.Contains(pe.Detail, string(padded[:2<<10])) || strings.Contains(pe.Detail, string(padded[:2<<10+1])) {
		t.Errorf("detail does not quote exactly the first 2 KiB of the output:\n%s", pe.Detail)
	}
	if !strings.Contains(r.log, strings.TrimSpace(string(padded))) {
		t.Errorf("the operation log lacks the full %d bytes of output", len(padded))
	}
}

// TC-SV-ERR23: A service still in transition at the timeout ends outcome-unknown. Owner WI 4.3. Requirements: FR-7.
func TestTCSVERR23_AServiceStillInTransitionAtThe(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-slow\n    timeout: 1s\n",
		ctlOptions{names: []string{"nx-svc-slow"}})
	w.fh.SetSettle(1 << 30)
	r := w.prepare(spec.Start.Op(), `{"name":"nx-svc-slow"}`)
	start := time.Now()
	w.helper(&r, 0) // The production poll, 250 ms
	took := time.Since(start)
	w.finalize(spec.Start.Op(), `{"name":"nx-svc-slow"}`, &r)
	pe := wantProblem(t, r.err, problem.OutcomeUnknown)
	for _, want := range []string{"starting", "nx-svc-slow", "1s"} {
		if !strings.Contains(pe.Detail, want) {
			t.Errorf("detail %q does not name %q", pe.Detail, want)
		}
	}
	if took < time.Second || took > time.Second+2*controlPoll+500*time.Millisecond {
		t.Errorf("ended after %s, want within 1s + 2 polls", took)
	}
}

// TC-SV-ERR24: A tool that never started ends dependency-unavailable and changes nothing. Owner WI 4.3. Requirements: FR-7, CON-2.
func TestTCSVERR24_AToolThatNeverStartedEndsDependency(t *testing.T) {
	w := newCtlWorld(t, ctlSystemd, "enabled: true\ncontrol:\n  - name: nx-svc-test\n",
		ctlOptions{names: []string{"nx-svc-test"}})
	w.do(svcctl.Stop, "nx-svc-test")
	start := systemdArgv("start", "nx-svc-test")
	w.fh.Answer(start, svcctl.Result{}, fmt.Errorf("%w: fork/exec %s: %w", svcctl.ErrNotStarted, systemctlTool,
		syscall.ENOENT))
	n := len(w.fh.Sent())
	r := w.act(spec.Start.Op(), `{"name":"nx-svc-test"}`, ctlPoll)
	pe := wantProblem(t, r.err, problem.DependencyUnavailable)
	if !strings.Contains(pe.Detail, systemctlTool) {
		t.Errorf("detail %q does not name %s", pe.Detail, systemctlTool)
	}
	wantSent(t, sentSince(w.fh, n), start)
	if kv := w.show("nx-svc-test"); kv["ActiveState"] != "inactive" {
		t.Errorf("nx-svc-test is %s after a start that never ran, want inactive", kv["ActiveState"])
	}
}

// TC-SV-ERR22 (quote bound): invalid UTF-8 in the tool output must not push
// the quote past 2 KiB once it is made valid.
func TestTCSVERR22_QuoteStaysWithin2KiBForInvalidUTF8(t *testing.T) {
	out := bytes.Repeat([]byte{0xff, 'a'}, 5<<10) // each invalid byte is its own run
	q := controlQuote("systemctl", "start", svcctl.Result{ExitCode: 1, Output: out})
	prefix := "; systemctl start exited 1: "
	body := strings.TrimPrefix(q, prefix)
	if len(body) > controlQuoteBytes {
		t.Fatalf("quote body is %d bytes, want at most %d", len(body), controlQuoteBytes)
	}
	if !utf8.ValidString(body) {
		t.Fatalf("quote body is not valid UTF-8")
	}
}

// SEC-4 (services-provider security review): rc-service runs the init
// script in the foreground, so its output is whatever the script's start_pre,
// checkconfig or daemon prints (a config test quoting the file, an echoed
// environment). The operation's problem detail and its log are read with
// services:read, which does not cover services:logs, where that text could
// hold secrets. On OpenRC only OpenRC's own verdict line is quoted and
// logged; systemctl's job message (no script output) is unchanged.
func TestSEC4_OpenRCInitScriptOutputIsNeitherQuotedNorLogged(t *testing.T) {
	const verdict = " * ERROR: nx-svc-fail failed to start"
	const secret = "DB_PASSWORD=hunter2-planted"
	w := newCtlWorld(t, ctlOpenRC, "enabled: true\ncontrol:\n  - name: nx-svc-fail\n", ctlOptions{names: []string{"nx-svc-fail"}})
	w.post = func(argv []string, res svcctl.Result) svcctl.Result {
		if slices.Equal(argv, openrcArgv("start", "nx-svc-fail")) {
			res.Output = []byte(" * Starting nx-svc-fail ...\n * checking /etc/app.conf: " + secret + "\n * start_pre: token=abc\n" +
				verdict + "\n")
		}
		return res
	}
	r := w.act(spec.Start.Op(), `{"name":"nx-svc-fail"}`, ctlPoll)
	pe := wantProblem(t, r.err, problem.ServiceActionFailed)
	if !strings.Contains(pe.Detail, "ERROR: nx-svc-fail failed to start") || fmt.Sprintf("%s", pe.Extensions["exitCode"]) != "1" {
		t.Errorf("problem %q, exitCode %v; want OpenRC's own verdict and the exit code", pe.Detail, pe.Extensions["exitCode"])
	}
	for _, leak := range []string{secret, "token=abc", "checking /etc/app.conf", "Starting nx-svc-fail"} {
		if strings.Contains(pe.Detail, leak) {
			t.Errorf("detail %q quotes the init script's output %q", pe.Detail, leak)
		}
		if strings.Contains(r.log, leak) {
			t.Errorf("operation log %q records the init script's output %q", r.log, leak)
		}
	}
	if !strings.Contains(r.log, "ERROR: nx-svc-fail failed to start") || !strings.Contains(r.log, "exited 1") {
		t.Errorf("operation log %q, want the exit code and the verdict line", r.log)
	}
	if !strings.Contains(r.log, "not recorded") {
		t.Errorf("operation log %q does not say that the rest of the output is not recorded", r.log)
	}
}
