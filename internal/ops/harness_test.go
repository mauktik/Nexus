package ops_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// thingIn is the input of every test op. Lane, Policy and Limit choose the
// lane, so one op exercises every lane policy.
type thingIn struct {
	Name   string    `json:"name"`
	Lane   string    `json:"lane,omitempty"`
	Policy string    `json:"policy,omitempty"`
	Limit  int       `json:"limit,omitempty"`
	Secret string    `json:"secret,omitempty"`
	Nested *nestedIn `json:"nested,omitempty"`
	Note   string    `json:"note,omitempty"`
	WhatIf bool      `json:"whatIf,omitempty"`
}

type nestedIn struct {
	Key   string `json:"key,omitempty"`
	Plain string `json:"plain,omitempty"`
}

type thingOut struct {
	apitypes.ActionResult
	Name string `json:"name"`
}

func refineThing(s *jsonschema.Schema) {
	set := func(name, pattern string) {
		if p := s.Properties[name]; p != nil {
			p.Pattern = pattern
		}
	}
	set("name", `^[a-z0-9-]+$`)
	set("lane", `^[a-z0-9:-]*$`)
	set("secret", `^[A-Za-z0-9_-]*$`)
	set("note", `^[A-Za-z0-9 _-]*$`)
	if p := s.Properties["policy"]; p != nil {
		p.Enum = []any{"", "queue", "reject"}
	}
}

// testOps are one test provider's declarations.
type testOps struct {
	run, cancelable, invoke, check def.Typed[thingIn, thingOut]
	peek, slow                     def.Typed[thingIn, thingOut]
	logs                           def.Typed[thingIn, apitypes.Frame]
}

func defineTestOps(prov, readScope, writeScope string) testOps {
	op := func(rt, action string, kind def.Kind, risk def.Risk, mode def.Mode, scope def.Scope, verb string) def.Op {
		return def.Op{
			ID: prov + "." + rt + "." + action, Provider: prov, Kind: kind, ResourceType: rt, Action: action,
			Summary: "s", Description: "Test operation for the operation manager tests.",
			Scope: scope, Risk: risk, Mode: mode, Refine: refineThing,
			CLI: def.CLIBinding{Noun: "thing", Verb: verb, Positional: "name"},
		}
	}
	run := op("things", "run", def.KindResourceAction, def.RiskMutating, def.ModeLongRunning, def.Scope{Name: writeScope}, "start")
	run.Sensitive = []string{"secret", "nested.key"}
	run.WhatIf = true
	cancelable := op("things", "stop", def.KindResourceAction, def.RiskMutating, def.ModeLongRunning, def.Scope{Name: writeScope}, "stop")
	cancelable.Cancel = def.CancelRunning
	invoke := op("scripts", "invoke", def.KindResourceAction, def.RiskDestructive, def.ModeLongRunning,
		def.Scope{Name: prov + ":run", PerTarget: prov + ":run:{name}"}, "invoke")
	invoke.CLI.Noun = "script"
	check := op("", "check", def.KindProviderAction, def.RiskReadOnly, def.ModeLongRunning, def.Scope{Name: readScope}, "test")
	check.ID, check.ResourceType, check.CLI.Positional = prov+".actions.check", "", ""
	peek := op("things", "", def.KindGet, def.RiskReadOnly, def.ModeSync, def.Scope{Name: readScope}, "get")
	peek.ID = prov + ".things.get"
	slow := op("things", "slow", def.KindSubresource, def.RiskReadOnly, def.ModeSync, def.Scope{Name: readScope}, "get")
	logs := op("things", "logs", def.KindSubresource, def.RiskReadOnly, def.ModeStream, def.Scope{Name: readScope}, "get")
	logs.CLI.Noun = "thinglog"
	return testOps{
		run:        def.Define[thingIn, thingOut](run),
		cancelable: def.Define[thingIn, thingOut](cancelable),
		invoke:     def.Define[thingIn, thingOut](invoke),
		check:      def.Define[thingIn, thingOut](check),
		peek:       def.Define[thingIn, thingOut](peek),
		slow:       def.Define[thingIn, thingOut](slow),
		logs:       def.Define[thingIn, apitypes.Frame](logs),
	}
}

var (
	tpOps = defineTestOps("tp", "test:read", "test:write")
	tqOps = defineTestOps("tq", "other:read", "other:write")
)

// hook is a test's behavior for runs of one thing name.
type hook func(ctx context.Context, in thingIn, r *provider.Run) (thingOut, error)

// testProv is a provider whose runs are counted and scripted per name.
type testProv struct {
	name, readScope string
	ops             testOps

	runs     atomic.Int64
	admitted atomic.Int64
	refuse   atomic.Bool // Admit refuses with not-updatable

	mu      sync.Mutex
	hooks   map[string]hook
	order   []string // Names in Run order
	streams map[string]func(ctx context.Context, emit func(apitypes.Frame) error) error
	slowFor time.Duration
}

func newTestProv(name, readScope string, o testOps) *testProv {
	return &testProv{name: name, readScope: readScope, ops: o, hooks: map[string]hook{},
		streams: map[string]func(context.Context, func(apitypes.Frame) error) error{}, slowFor: 3 * time.Second}
}

func (p *testProv) Spec() def.ProviderSpec {
	o := p.ops
	return def.ProviderSpec{Name: p.name, ReadScope: p.readScope, Ops: []def.Op{
		o.run.Op(), o.cancelable.Op(), o.invoke.Op(), o.check.Op(), o.peek.Op(), o.slow.Op(), o.logs.Op(),
	}}
}
func (p *testProv) Configure(func(any) error) error { return nil }
func (p *testProv) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (p *testProv) on(name string, h hook) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hooks[name] = h
}

func (p *testProv) runOrder() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.order...)
}

func (p *testProv) run(ctx context.Context, in thingIn, r *provider.Run) (thingOut, error) {
	p.runs.Add(1)
	p.mu.Lock()
	p.order = append(p.order, in.Name)
	h := p.hooks[in.Name]
	p.mu.Unlock()
	if h != nil {
		return h(ctx, in, r)
	}
	return thingOut{ActionResult: apitypes.ActionResult{Changed: true}, Name: in.Name}, nil
}

func laneOf(in thingIn) provider.Lane {
	if in.Lane == "" {
		return provider.NoLane
	}
	return provider.Lane{Key: in.Lane, Policy: provider.LanePolicy(in.Policy), Limit: in.Limit}
}

func (p *testProv) Bindings() []provider.Binding {
	o := p.ops
	admit := func(context.Context, thingIn) error {
		p.admitted.Add(1)
		if p.refuse.Load() {
			return problem.New(problem.NotUpdatable, "admission refused by the test").With("reason", "test")
		}
		return nil
	}
	lane := func(_ context.Context, in thingIn) (provider.Lane, error) { return laneOf(in), nil }
	spec := provider.InProcessSpec[thingIn, thingOut]{Admit: admit, Lane: lane, Run: p.run}
	script := provider.InProcessSpec[thingIn, thingOut]{Admit: admit, Run: p.run,
		Lane: func(_ context.Context, in thingIn) (provider.Lane, error) {
			return provider.Lane{Key: "script:" + in.Name, Policy: provider.LaneQueue}, nil
		}}
	return []provider.Binding{
		provider.InProcess(o.run, spec),
		provider.InProcess(o.cancelable, spec),
		provider.InProcess(o.invoke, script),
		provider.InProcess(o.check, spec),
		provider.Sync(o.peek, func(_ context.Context, in thingIn) (thingOut, error) { return thingOut{Name: in.Name}, nil }),
		provider.Sync(o.slow, func(_ context.Context, in thingIn) (thingOut, error) {
			time.Sleep(p.slowFor)
			return thingOut{Name: in.Name}, nil
		}),
		provider.Stream(o.logs, func(ctx context.Context, in thingIn, emit func(apitypes.Frame) error) error {
			p.mu.Lock()
			s := p.streams[in.Name]
			p.mu.Unlock()
			if s == nil {
				return problem.New(problem.NotFound, "no such thing")
			}
			return s(ctx, emit)
		}),
	}
}

// gate is a run that blocks until released.
type gate struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate() *gate { return &gate{started: make(chan struct{}), release: make(chan struct{})} }

// gated makes runs of name wait for g, or for their context to end.
func (p *testProv) gated(name string) *gate {
	g := newGate()
	p.on(name, func(ctx context.Context, in thingIn, _ *provider.Run) (thingOut, error) {
		g.once.Do(func() { close(g.started) })
		select {
		case <-g.release:
			return thingOut{ActionResult: apitypes.ActionResult{Changed: true}, Name: in.Name}, nil
		case <-ctx.Done():
			return thingOut{}, ctx.Err()
		}
	})
	return g
}

func (g *gate) open() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

func (g *gate) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the gated run never started")
	}
}

// syncBuffer is a log destination safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// seq orders the events of the audit and store spies.
type seq struct {
	mu     sync.Mutex
	events []string
}

func (s *seq) add(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, fmt.Sprintf(format, args...))
}

func (s *seq) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// Injected failure modes of faultyAudit, encoding the errors a real disk
// returns: ENOSPC from write(2) and EIO from fsync(2). The real-disk case is
// at 10.1.
const (
	failNone = iota
	failWrite
	failSync
)

// faultyAudit wraps the real audit log. From the failFrom-th write on (when
// set), or always while mode is failWrite or failSync, Write fails: a write
// failure writes nothing, a sync failure appends the line but reports EIO,
// as a failed fsync leaves the line's durability unknown.
type faultyAudit struct {
	*audit.Log
	mode     atomic.Int32
	failFrom atomic.Int64 // 0 means never
	writes   atomic.Int64
	seq      *seq
}

func (f *faultyAudit) Write(rec systemspec.AuditRecord, sensitive []string) (string, error) {
	n := f.writes.Add(1)
	mode := int(f.mode.Load())
	if from := f.failFrom.Load(); from > 0 && n >= from && mode == failNone {
		mode = failWrite
	}
	switch mode {
	case failWrite:
		return "", fmt.Errorf("write audit record: %w", syscall.ENOSPC)
	case failSync:
		if _, err := f.Log.Write(rec, sensitive); err != nil {
			return "", err
		}
		return "", fmt.Errorf("write audit record: %w", syscall.EIO)
	}
	id, err := f.Log.Write(rec, sensitive)
	if err == nil && f.seq != nil {
		f.seq.add("audit %s %s", rec.Outcome, rec.OperationID)
	}
	return id, err
}

// spyStore records the commits of the store in the shared sequence.
type spyStore struct {
	*store.Store
	seq *seq
}

func (s *spyStore) CreateWithIdempotency(rec store.OpRecord, idem store.IdempotencyKey) (store.CreateResult, error) {
	res, err := s.Store.CreateWithIdempotency(rec, idem)
	if err == nil && !res.Existing {
		s.seq.add("commit Queued %s", res.ID)
	}
	return res, err
}

func (s *spyStore) CompareAndSetStatus(id string, from, to apitypes.OperationStatus) (store.OpRecord, error) {
	rec, err := s.Store.CompareAndSetStatus(id, from, to)
	if err == nil {
		s.seq.add("commit %s %s", to, id)
	}
	return rec, err
}

func (s *spyStore) CommitTerminal(id string, from, to apitypes.OperationStatus, result json.RawMessage, perr *problem.Error) (store.OpRecord, error) {
	rec, err := s.Store.CommitTerminal(id, from, to, result, perr)
	if err == nil {
		s.seq.add("commit %s %s", to, id)
	}
	return rec, err
}

// world is a full in-process stack: store, audit log, operation manager,
// registry and Dispatcher over two test providers, plus a real HTTP server
// on demand.
type world struct {
	t        *testing.T
	stateDir string
	auditDir string
	tokDir   string
	logs     *syncBuffer
	log      *slog.Logger
	seq      *seq

	store *store.Store
	aud   *faultyAudit
	alog  *audit.Log
	mgr   *ops.Manager
	reg   *core.Registry
	disp  *core.Dispatcher
	tp    *testProv
	tq    *testProv

	srv  *server.Server
	base string

	clock    atomic.Pointer[time.Time] // Store time when set
	overFull atomic.Bool               // The storage hook refuses new mutations
	tuneOpts func(*ops.Options)
}

// now is the store's clock: the set time, else the real one.
func (w *world) now() time.Time {
	if t := w.clock.Load(); t != nil {
		return *t
	}
	return time.Now()
}

func (w *world) setClock(t time.Time) { w.clock.Store(&t) }

// newWorld builds the stack. tune, when given, adjusts the manager's options.
func newWorld(t *testing.T, tune ...func(*ops.Options)) *world {
	t.Helper()
	w := &world{t: t, stateDir: t.TempDir(), auditDir: t.TempDir(), tokDir: t.TempDir(), logs: &syncBuffer{}, seq: &seq{}}
	if len(tune) > 0 {
		w.tuneOpts = tune[0]
	}
	w.log = slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w.tp = newTestProv("tp", "test:read", tpOps)
	w.tq = newTestProv("tq", "other:read", tqOps)
	alog, err := audit.Open(audit.Options{Dir: w.auditDir, Host: "testhost", Logger: w.log})
	if err != nil {
		t.Fatal(err)
	}
	w.alog = alog
	w.aud = &faultyAudit{Log: alog, seq: w.seq}
	w.open()
	t.Cleanup(w.close)
	return w
}

// open builds everything above the audit log on state.db.
func (w *world) open() {
	t := w.t
	t.Helper()
	st, err := store.Open(w.stateDir, store.Options{Now: w.now})
	if err != nil {
		t.Fatal(err)
	}
	w.store = st
	opts := ops.Options{Store: &spyStore{Store: st, seq: w.seq}, Audit: w.aud,
		Dir: filepath.Join(w.stateDir, "ops"), Host: "testhost", Logger: w.log, PollInterval: 20 * time.Millisecond}
	if w.tuneOpts != nil {
		w.tuneOpts(&opts)
	}
	w.mgr, err = ops.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	w.reg, err = core.New([]provider.Provider{w.tp, w.tq}, core.Options{
		Host: "testhost", Logger: w.log, Audit: w.alog, Ops: w.mgr,
		StorageCheck: func() *problem.Error {
			if w.overFull.Load() {
				return problem.New(problem.StorageLimitReached, "the storage budget is used up")
			}
			return nil
		},
		Providers: map[string]core.ProviderConfig{"tp": {Enabled: true}, "tq": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w.disp = core.NewDispatcher(w.reg, core.DispatcherOptions{Logger: w.log, Audit: w.aud})
}

func (w *world) closeState() {
	if w.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.srv.Shutdown(ctx); err != nil {
			w.t.Errorf("server shutdown: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.mgr.Shutdown(ctx); err != nil {
		w.t.Errorf("manager shutdown: %v", err)
	}
	if err := w.store.Close(); err != nil {
		w.t.Errorf("store close: %v", err)
	}
}

func (w *world) close() {
	w.closeState()
	_ = w.alog.Close()
	if w.t.Failed() {
		w.t.Logf("logs:\n%s", w.logs.String())
	}
}

// restart closes and reopens everything on the same state.db.
func (w *world) restart() {
	w.closeState()
	w.open()
}

// who is a principal for direct Dispatcher calls.
func who(name string, scopes ...string) auth.Principal {
	sum := sha256.Sum256([]byte(name))
	return auth.Principal{TokenID: hex.EncodeToString(sum[:6]), Name: name, Scopes: scopes}
}

func (w *world) call(p auth.Principal, op def.Op, input, key string) (core.InvokeResult, error) {
	return w.disp.Invoke(context.Background(), core.InvokeRequest{
		Caller: p, Surface: core.SurfaceREST, Op: op, Input: json.RawMessage(input), IdempotencyKey: key,
	})
}

// start invokes op and requires a 202, returning the operation.
func (w *world) start(p auth.Principal, op def.Op, input, key string) apitypes.Operation {
	w.t.Helper()
	res, err := w.call(p, op, input, key)
	if err != nil {
		w.t.Fatalf("%s %s: %v", op.ID, input, err)
	}
	if res.Status != http.StatusAccepted || res.OperationID == "" {
		w.t.Fatalf("%s: status %d, id %q", op.ID, res.Status, res.OperationID)
	}
	o := res.Body.(apitypes.Operation)
	if o.ID != res.OperationID {
		w.t.Fatalf("body id %q != operation id %q", o.ID, res.OperationID)
	}
	return o
}

func wantProblem(t *testing.T, err error, slug problem.Slug) *problem.Error {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) || p.Slug() != slug {
		t.Fatalf("err = %v, want %s", err, slug)
	}
	return p
}

// waitStatus polls the store until id has status.
func (w *world) waitStatus(id string, status apitypes.OperationStatus) store.OpRecord {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := w.store.Get(id)
		if err == nil && rec.Status == status {
			return rec
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("operation %s: %+v (%v), want %s", id, rec, err, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// records reads every audit record back from disk, oldest first.
func (w *world) records() []systemspec.AuditRecord {
	w.t.Helper()
	f, err := os.Open(filepath.Join(w.auditDir, audit.FileName))
	if err != nil {
		w.t.Fatal(err)
	}
	defer f.Close()
	var out []systemspec.AuditRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r systemspec.AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			w.t.Fatalf("audit line %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		w.t.Fatal(err)
	}
	return out
}

func filterRecs(recs []systemspec.AuditRecord, keep func(systemspec.AuditRecord) bool) []systemspec.AuditRecord {
	var out []systemspec.AuditRecord
	for _, r := range recs {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

func outcome(o string) func(systemspec.AuditRecord) bool {
	return func(r systemspec.AuditRecord) bool { return r.Outcome == o }
}

// countOps counts every stored operation.
func (w *world) countOps() int {
	w.t.Helper()
	n, cursor := 0, ""
	for {
		p, err := w.store.List(store.ListQuery{Limit: store.MaxListLimit, Cursor: cursor})
		if err != nil {
			w.t.Fatal(err)
		}
		n += len(p.Records)
		if p.NextCursor == "" {
			return n
		}
		cursor = p.NextCursor
	}
}

// serve starts the real HTTP server over the world's registry on a loopback
// listener, with tokens from w.tokDir. configure, when given, adjusts the
// *http.Server before it serves.
func (w *world) serve(configure ...func(*http.Server)) {
	t := w.t
	t.Helper()
	srv, err := server.New(w.reg, server.Options{Logger: w.log, Auth: auth.New(w.tokDir, auth.Options{Logger: w.log, Audit: w.alog})})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range configure {
		c(srv.HTTP())
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	w.srv = srv
	w.base = "http://" + ln.Addr().String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("server shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
}

// mint creates a real token in w.tokDir.
func (w *world) mint(name string, scopes ...string) string {
	w.t.Helper()
	tok, _, err := auth.Create(w.tokDir, auth.CreateRequest{Name: name, Scopes: scopes})
	if err != nil {
		w.t.Fatal(err)
	}
	return tok
}

var client = &http.Client{Timeout: 15 * time.Second}

// do sends an authenticated request to the world's server.
func (w *world) do(method, path, tok, body string, header ...string) (*http.Response, []byte) {
	w.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, w.base+path, rd)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	return resp, b
}

func decodeOp(t *testing.T, b []byte) apitypes.Operation {
	t.Helper()
	var o apitypes.Operation
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatalf("operation body %s: %v", b, err)
	}
	return o
}

func decodeProblem(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("problem body %s: %v", b, err)
	}
	return m
}
