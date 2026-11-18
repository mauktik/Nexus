// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-8, TC-RA-9,
// TC-RA-10, TC-RA-11, TC-RA-111, TC-RA-E4, TC-RA-E29, TC-RA-ERR5,
// TC-RA-ERR6, TC-RA-ERR7, TC-RA-ERR8 and TC-RA-ERR41 (WI 1.3): the
// Dispatcher's SyncMutation and Audited sync paths, and completion at
// admission of a detached op.

package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// raSentinel is the secret the SyncMutation handler returns: it may reach
// the response body only (NFR-1, CON-3).
const raSentinel = "RA-SENTINEL-1"

const idemMessage = "Idempotency-Key is not supported by this operation: it answers once and is never replayed"

type tcraIn struct {
	Name string `json:"name"`
}

type tcraOut struct {
	Changed bool   `json:"changed"`
	Secret  string `json:"secret"`
}

type tcraListIn struct{}

type tcraCount struct {
	Count int `json:"count"`
}

// tcraSetIn is the input of the detached ops. Carried is what admission
// derives: it is never persisted.
type tcraSetIn struct {
	X       string `json:"x,omitempty"`
	Carried string `json:"-"`
}

type tcraSetOut struct {
	Changed bool   `json:"changed"`
	Value   string `json:"value"`
}

var (
	// tcraAdd is the planted SyncMutation op.
	tcraAdd = def.Define[tcraIn, tcraOut](def.Op{
		ID: "tcra.things.add", Provider: "tcra", Kind: def.KindResourceAction, ResourceType: "things",
		Action: "add", Summary: "s", Description: "Sync mutation for the remote-admin dispatcher tests.",
		Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskMutating, Mode: def.ModeSync, SyncMutation: true,
		CLI: def.CLIBinding{Noun: "tcrathing", Verb: "install", Positional: "name"},
	})
	// tcraCounted is the planted Audited read.
	tcraCounted = def.Define[tcraListIn, tcraCount](def.Op{
		ID: "tcra.counts.list", Provider: "tcra", Kind: def.KindList, ResourceType: "counts",
		Summary: "s", Description: "Audited read for the remote-admin dispatcher tests.",
		Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Audited: true,
		CLI: def.CLIBinding{Noun: "tcracount", Verb: "get"},
	})
	// tcraPlain is a read that is not Audited.
	tcraPlain = def.Define[tcraListIn, tcraCount](def.Op{
		ID: "tcra.plains.list", Provider: "tcra", Kind: def.KindList, ResourceType: "plains",
		Summary: "s", Description: "Unaudited read for the remote-admin dispatcher tests.",
		Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "tcraplain", Verb: "get"},
	})
	// tcraSet is op D: detached, CompletesAtAdmission, in the tcra lane.
	tcraSet = def.Define[tcraSetIn, tcraSetOut](def.Op{
		ID: "tcra.actions.set", Provider: "tcra", Kind: def.KindProviderAction, Action: "set",
		Summary: "s", Description: "Detached op that may complete at admission.",
		Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CompletesAtAdmission: true,
		CLI:                  def.CLIBinding{Noun: "tcraconf", Verb: "update"},
	})
	// tcraHold is op B: detached, in D's lane.
	tcraHold = def.Define[tcraSetIn, tcraSetOut](def.Op{
		ID: "tcra.actions.hold", Provider: "tcra", Kind: def.KindProviderAction, Action: "hold",
		Summary: "s", Description: "Detached op that holds the tcra lane.",
		Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI: def.CLIBinding{Noun: "tcrahold", Verb: "invoke"},
	})
	// tcraUndeclared is op N: an Admission hook that answers Done on an op
	// that does not declare CompletesAtAdmission.
	tcraUndeclared = def.Define[tcraSetIn, tcraSetOut](def.Op{
		ID: "tcra.actions.undeclared", Provider: "tcra", Kind: def.KindProviderAction, Action: "undeclared",
		Summary: "s", Description: "Detached op whose admission answers Done without the declaration.",
		Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI: def.CLIBinding{Noun: "tcraund", Verb: "invoke"},
	})
)

// Admission modes of tcraSet's hook.
const (
	byInput    = iota // Done for {"x":"same"}, else the input with Carried set
	alwaysDone        // Done whatever the input
	neverDone         // The input, unchanged
)

// tcraHarness holds the handlers' counters and switches. Prepare and the
// launcher run on the manager's goroutines, so everything is locked.
type tcraHarness struct {
	mu         sync.Mutex
	addErr     error
	addCalls   int
	readCalls  int
	hookCalls  int
	hookMode   int
	prepCalls  int
	prepIn     []tcraSetIn
	launches   []string
	plainCalls int
}

func (h *tcraHarness) with(f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f()
}

func (h *tcraHarness) counts() (add, read, hook, prep, launches int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addCalls, h.readCalls, h.hookCalls, h.prepCalls, len(h.launches)
}

func tcraLane(context.Context, tcraSetIn) (provider.Lane, error) {
	return provider.Lane{Key: "tcra", Policy: provider.LaneReject}, nil
}

func (h *tcraHarness) prepare(_ context.Context, in tcraSetIn, _ *provider.Run) (detach.Spec, error) {
	h.with(func() { h.prepCalls++; h.prepIn = append(h.prepIn, in) })
	return detach.Spec{Argv: []string{"/bin/true"}, KillMode: detach.KillProcess, MaxRuntime: time.Hour}, nil
}

func finalizeSet(_ context.Context, _ tcraSetIn, x detach.Exit, _ *provider.Run) (tcraSetOut, error) {
	return tcraSetOut{Changed: x.ExitCode == 0}, nil
}

func (h *tcraHarness) admission(ctx context.Context, in tcraSetIn) (provider.Admitted[tcraSetIn, tcraSetOut], error) {
	var mode int
	h.with(func() { h.hookCalls++; mode = h.hookMode })
	done := in.X == "same"
	switch mode {
	case alwaysDone:
		done = true
	case neverDone:
		return provider.Admitted[tcraSetIn, tcraSetOut]{In: in}, nil
	}
	if done {
		audit.AddFact(ctx, "f", "1")
		return provider.Admitted[tcraSetIn, tcraSetOut]{Done: true, Out: tcraSetOut{Value: in.X}}, nil
	}
	in.Carried = "carried"
	return provider.Admitted[tcraSetIn, tcraSetOut]{In: in}, nil
}

type tcraProvider struct{ h *tcraHarness }

func (tcraProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "tcra", ReadScope: "tcra:read", Ops: []def.Op{
		tcraAdd.Op(), tcraCounted.Op(), tcraPlain.Op(), tcraSet.Op(), tcraHold.Op(), tcraUndeclared.Op(),
	}}
}
func (tcraProvider) Configure(func(any) error) error { return nil }
func (tcraProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (p tcraProvider) Bindings() []provider.Binding {
	h := p.h
	return []provider.Binding{
		provider.Sync(tcraAdd, func(ctx context.Context, in tcraIn) (tcraOut, error) {
			var err error
			h.with(func() { h.addCalls++; err = h.addErr })
			audit.AddFact(ctx, "thingId", in.Name)
			if err != nil {
				return tcraOut{}, err
			}
			return tcraOut{Changed: true, Secret: raSentinel}, nil
		}),
		provider.Sync(tcraCounted, func(ctx context.Context, _ tcraListIn) (tcraCount, error) {
			h.with(func() { h.readCalls++ })
			audit.AddFact(ctx, "count", "3")
			return tcraCount{Count: 3}, nil
		}),
		provider.Sync(tcraPlain, func(ctx context.Context, _ tcraListIn) (tcraCount, error) {
			h.with(func() { h.plainCalls++ })
			audit.AddFact(ctx, "count", "1")
			return tcraCount{Count: 1}, nil
		}),
		provider.Detached(tcraSet, provider.DetachedSpec[tcraSetIn, tcraSetOut]{
			Admission: h.admission, Lane: tcraLane, Prepare: h.prepare, Finalize: finalizeSet,
		}),
		provider.Detached(tcraHold, provider.DetachedSpec[tcraSetIn, tcraSetOut]{
			Lane: tcraLane, Prepare: h.prepare, Finalize: finalizeSet,
		}),
		provider.Detached(tcraUndeclared, provider.DetachedSpec[tcraSetIn, tcraSetOut]{
			Admission: func(context.Context, tcraSetIn) (provider.Admitted[tcraSetIn, tcraSetOut], error) {
				return provider.Admitted[tcraSetIn, tcraSetOut]{Done: true, Out: tcraSetOut{Value: raSentinel}}, nil
			},
			Prepare: h.prepare, Finalize: finalizeSet,
		}),
	}
}

// countingStore is the store spy: it counts every write of an operation
// record.
type countingStore struct {
	*store.Store
	writes atomic.Int64
}

func (s *countingStore) CreateWithIdempotency(rec store.OpRecord, idem store.IdempotencyKey) (store.CreateResult, error) {
	s.writes.Add(1)
	return s.Store.CreateWithIdempotency(rec, idem)
}

func (s *countingStore) CompareAndSetStatus(id string, from, to apitypes.OperationStatus) (store.OpRecord, error) {
	s.writes.Add(1)
	return s.Store.CompareAndSetStatus(id, from, to)
}

func (s *countingStore) CommitTerminal(id string, from, to apitypes.OperationStatus, result json.RawMessage, perr *problem.Error) (store.OpRecord, error) {
	s.writes.Add(1)
	return s.Store.CommitTerminal(id, from, to, result, perr)
}

// failOutcomeAudit wraps the real audit log and fails the writes of one
// outcome. With corrupt set, the failing write still reaches the real log
// with params it cannot parse, so the log's own health flag is exercised.
type failOutcomeAudit struct {
	*audit.Log
	outcome string
	corrupt bool
}

func (a failOutcomeAudit) Write(rec systemspec.AuditRecord, sensitive []string) (string, error) {
	if rec.Outcome != a.outcome {
		return a.Log.Write(rec, sensitive)
	}
	if a.corrupt {
		rec.Params = json.RawMessage(`{`)
		return a.Log.Write(rec, sensitive)
	}
	return "", fmt.Errorf("write audit record: %w", syscall.ENOSPC)
}

type raOpts struct {
	storage    func() *problem.Error
	loadTarget core.TargetLoader
	wrapAudit  func(*audit.Log) core.AuditWriter
}

type raFixture struct {
	h        *tcraHarness
	lg       *audit.Log
	auditDir string
	stateDir string
	st       *countingStore
	mgr      *ops.Manager
	reg      *core.Registry
	d        *core.Dispatcher
	logs     *logBuf
	log      *slog.Logger
	launcher *detach.Launcher
}

func newRAFixture(t *testing.T, o raOpts) *raFixture {
	t.Helper()
	f := &raFixture{h: &tcraHarness{}, logs: &logBuf{}, auditDir: t.TempDir(), stateDir: t.TempDir()}
	f.log = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lg, err := audit.Open(audit.Options{Dir: f.auditDir, Host: "testhost", Logger: f.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	f.lg = lg
	st, err := store.Open(f.stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.st = &countingStore{Store: st}
	opsDir := filepath.Join(f.stateDir, "ops")
	// The launch spy starts nothing: a launched operation stays Running
	// until the test writes its exit.json.
	f.launcher, err = detach.New(detach.Config{OpsDir: opsDir, Executable: "/bin/true", Logger: f.log}, detach.Options{
		Launch: func(_ context.Context, inv detach.Invocation) error {
			f.h.with(func() { f.h.launches = append(f.h.launches, inv.ID) })
			return nil
		},
		UnitState: func(context.Context, string) (detach.UnitState, error) {
			return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
		},
		Signal:       func(context.Context, string, syscall.Signal) error { return nil },
		ListUnits:    func(context.Context) ([]string, error) { return nil, nil },
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.mgr, err = ops.New(ops.Options{Store: f.st, Audit: lg, Dir: opsDir, Host: "testhost", Logger: f.log, Detach: f.launcher})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.mgr.Shutdown(ctx)
	})
	f.reg, err = core.New([]provider.Provider{tcraProvider{h: f.h}}, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: f.log, Audit: lg, Ops: f.mgr, StorageCheck: o.storage,
		Providers: map[string]core.ProviderConfig{"tcra": {Enabled: true}},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	dopts := core.DispatcherOptions{Logger: f.log, LoadTarget: o.loadTarget}
	if o.wrapAudit != nil {
		dopts.Audit = o.wrapAudit(lg)
	}
	f.d = core.NewDispatcher(f.reg, dopts)
	return f
}

func (f *raFixture) call(d *core.Dispatcher, op def.Op, input, key string) (core.InvokeResult, error) {
	return d.Invoke(context.Background(), core.InvokeRequest{
		Caller: caller("tcra:admin", "audit:read"), Surface: core.SurfaceREST, Op: op,
		Input: json.RawMessage(input), IdempotencyKey: key,
	})
}

func (f *raFixture) invoke(op def.Op, input, key string) (core.InvokeResult, error) {
	return f.call(f.d, op, input, key)
}

// records are the audit records of action, oldest first.
func (f *raFixture) records(t *testing.T, action string) []systemspec.AuditRecord {
	t.Helper()
	var out []systemspec.AuditRecord
	for _, r := range f.allRecords(t) {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

func (f *raFixture) allRecords(t *testing.T) []systemspec.AuditRecord {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.auditDir, audit.FileName)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return readAuditFile(t, f.auditDir)
}

func outcomesOf(recs []systemspec.AuditRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Outcome
	}
	return out
}

// wantRequestedThen asserts recs are exactly requested then outcome, the
// second naming the first, and returns them.
func wantRequestedThen(t *testing.T, recs []systemspec.AuditRecord, outcome string) (requested, second systemspec.AuditRecord) {
	t.Helper()
	if len(recs) != 2 || recs[0].Outcome != audit.OutcomeRequested || recs[1].Outcome != outcome {
		t.Fatalf("records = %v, want [requested %s]", outcomesOf(recs), outcome)
	}
	if recs[0].ID == "" || recs[1].RequestID != recs[0].ID {
		t.Fatalf("%s requestId = %q, want the requested record's id %q", outcome, recs[1].RequestID, recs[0].ID)
	}
	return recs[0], recs[1]
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *raFixture) status(t *testing.T, id string) apitypes.OperationStatus {
	t.Helper()
	rec, err := f.mgr.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Status
}

func decodeBody[T any](t *testing.T, body any) T {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("body %s: %v", b, err)
	}
	return v
}

// TC-RA-8 (CON-3, FR-1, FR-13, NFR-1): a SyncMutation success writes
// requested then completed with facts, links them, and creates no operation.
func TestTCRA8_ASyncMutationSuccessWritesRequestedThenCompleted(t *testing.T) {
	f := newRAFixture(t, raOpts{})
	res, err := f.invoke(tcraAdd.Op(), `{"name":"t1"}`, "")
	wantOK(t, res, err)
	if res.OperationID != "" {
		t.Fatalf("operationId = %q, want none for a SyncMutation", res.OperationID)
	}
	if out := decodeBody[tcraOut](t, res.Body); out != (tcraOut{Changed: true, Secret: raSentinel}) {
		t.Fatalf("body = %+v, want the handler output", out)
	}
	req, done := wantRequestedThen(t, f.records(t, tcraAdd.Op().ID), audit.OutcomeCompleted)
	if !maps.Equal(done.Facts, map[string]string{"thingId": "t1"}) {
		t.Fatalf("completed facts = %v, want thingId=t1", done.Facts)
	}
	if done.TokenID != "a1b2c3d4e5f6" || done.Target != "t1" || done.OperationID != "" || len(done.Params) != 0 {
		t.Fatalf("completed record = %+v, want the caller and target, no operation and no params", done)
	}
	if req.Target != "t1" {
		t.Fatalf("requested record = %+v, want target t1", req)
	}
	if n := f.st.writes.Load(); n != 0 {
		t.Fatalf("store writes = %d, want 0", n)
	}
}

// TC-RA-9 (FR-13, FR-2, FR-6): an Audited read writes requested then
// completed.
func TestTCRA9_AnAuditedReadWritesRequestedThenCompleted(t *testing.T) {
	f := newRAFixture(t, raOpts{})
	res, err := f.invoke(tcraCounted.Op(), `{}`, "")
	wantOK(t, res, err)
	if out := decodeBody[tcraCount](t, res.Body); out.Count != 3 {
		t.Fatalf("body = %+v, want count 3", out)
	}
	_, done := wantRequestedThen(t, f.records(t, tcraCounted.Op().ID), audit.OutcomeCompleted)
	if !maps.Equal(done.Facts, map[string]string{"count": "3"}) {
		t.Fatalf("completed facts = %v, want count=3", done.Facts)
	}
}

// TC-RA-10 (FR-13): a read-only op that is not Audited still writes no
// audit record.
func TestTCRA10_AReadOnlyOpThatIsNotAuditedStillWritesNoAudit(t *testing.T) {
	f := newRAFixture(t, raOpts{})
	before := len(f.allRecords(t))
	res, err := f.invoke(tcraPlain.Op(), `{}`, "")
	wantOK(t, res, err)
	var calls int
	f.h.with(func() { calls = f.h.plainCalls })
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if after := f.allRecords(t); len(after) != before {
		t.Fatalf("audit records = %+v, want %d (none new)", after, before)
	}
}

// TC-RA-11 (NFR-1, CON-3): the sync handler output reaches no log line and
// no store.
func TestTCRA11_TheSyncHandlerOutputReachesNoLogLineAndNoStore(t *testing.T) {
	f := newRAFixture(t, raOpts{})
	res, err := f.invoke(tcraAdd.Op(), `{"name":"t1"}`, "")
	wantOK(t, res, err)
	body, merr := json.Marshal(res.Body)
	if merr != nil {
		t.Fatal(merr)
	}
	if !strings.Contains(string(body), raSentinel) {
		t.Fatalf("body %s lacks the sentinel: the test would be vacuous", body)
	}

	f.h.with(func() { f.h.addErr = errors.New("tcra handler failed") })
	_, err = f.invoke(tcraAdd.Op(), `{"name":"t2"}`, "")
	wantProblem(t, err, http.StatusInternalServerError, problem.InternalError)
	f.h.with(func() { f.h.addErr = nil })

	_, err = f.invoke(tcraAdd.Op(), `{"name":5}`, "")
	wantProblem(t, err, http.StatusBadRequest, problem.ValidationFailed)

	if n := strings.Count(f.logs.String(), raSentinel); n != 0 {
		t.Fatalf("the sentinel is in %d log lines:\n%s", n, f.logs.String())
	}
	raw, rerr := os.ReadFile(filepath.Join(f.auditDir, audit.FileName))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if n := strings.Count(string(raw), raSentinel); n != 0 {
		t.Fatalf("the sentinel is %d times in the audit file", n)
	}
	if n := f.st.writes.Load(); n != 0 {
		t.Fatalf("store writes = %d, want 0", n)
	}
	if got := outcomesOf(f.records(t, tcraAdd.Op().ID)); strings.Join(got, ",") != "requested,completed,requested,refused,refused" {
		t.Fatalf("records = %v, want requested,completed,requested,refused,refused", got)
	}
}

// TC-RA-E4 (CON-3, FR-13): a SyncMutation skips the storage check, and the
// audit outcome filter accepts completed.
func TestTCRAE4_ASyncMutationSkipsTheStorageCheckAndTheAudit(t *testing.T) {
	var checks atomic.Int64
	f := newRAFixture(t, raOpts{storage: func() *problem.Error {
		checks.Add(1)
		return problem.New(problem.StorageLimitReached, "the state directory is over its budget")
	}})
	res, err := f.invoke(tcraAdd.Op(), `{"name":"t1"}`, "")
	wantOK(t, res, err)
	if n := checks.Load(); n != 0 {
		t.Fatalf("storage checks = %d after the SyncMutation, want 0", n)
	}
	_, err = f.invoke(tcraSet.Op(), `{"x":"diff"}`, "")
	wantProblem(t, err, http.StatusInsufficientStorage, problem.StorageLimitReached)
	if checks.Load() != 1 {
		t.Fatalf("storage checks = %d after the long-running op, want 1", checks.Load())
	}

	res, err = f.invoke(systemspec.AuditList.Op(), `{"filter":["outcome=completed"]}`, "")
	wantOK(t, res, err)
	page := res.Body.(apitypes.List[systemspec.AuditRecord])
	if len(page.Items) != 1 || page.Items[0].Outcome != audit.OutcomeCompleted || page.Items[0].Action != tcraAdd.Op().ID {
		t.Fatalf("outcome=completed items = %+v, want the one completed record", page.Items)
	}
	var desc string
	for _, flt := range systemspec.AuditList.Op().Filters {
		if flt.Key == "outcome" {
			desc = flt.Description
		}
	}
	if !strings.Contains(desc, "completed") {
		t.Fatalf("outcome filter description %q does not list completed", desc)
	}
}

// TC-RA-ERR5 (FR-13, CON-3): a handler error on a SyncMutation writes a
// refused record linked to its requested record; the cancel's refused
// record gains the same link.
func TestTCRAERR5_AHandlerErrorOnASyncMutationWritesARefusedRecord(t *testing.T) {
	// The target loader finds the operation, so the cancel passes the scope
	// check and its handler, which reads the store, answers not-found.
	f := newRAFixture(t, raOpts{loadTarget: func(context.Context, string) (core.TargetRecord, error) {
		return core.TargetRecord{Provider: "tcra", StartScope: "tcra:admin"}, nil
	}})
	f.h.with(func() { f.h.addErr = problem.New(problem.Conflict, "dup") })
	_, err := f.invoke(tcraAdd.Op(), `{"name":"t1"}`, "")
	wantProblem(t, err, http.StatusConflict, problem.Conflict)
	_, refused := wantRequestedThen(t, f.records(t, tcraAdd.Op().ID), audit.OutcomeRefused)
	if refused.Problem != string(problem.Conflict) {
		t.Fatalf("refused problem = %q, want conflict", refused.Problem)
	}

	unknown := "01234567-89ab-7cde-8f01-23456789abcd"
	_, err = f.invoke(systemspec.OperationsCancel.Op(), `{"id":"`+unknown+`"}`, "")
	wantProblem(t, err, http.StatusNotFound, problem.NotFound)
	_, refused = wantRequestedThen(t, f.records(t, systemspec.OperationsCancel.Op().ID), audit.OutcomeRefused)
	if refused.Problem != string(problem.NotFound) || refused.OperationID != unknown {
		t.Fatalf("cancel refused record = %+v, want not-found naming the operation", refused)
	}
	for _, r := range f.allRecords(t) {
		if r.Outcome == audit.OutcomeCompleted {
			t.Fatalf("completed record %+v after a refusal", r)
		}
	}
}

// TC-RA-ERR6 (FR-13, NFR-1): a requested-record write failure refuses with
// audit-unavailable before the handler runs.
func TestTCRAERR6_ARequestedRecordWriteFailureRefusesWithAudit(t *testing.T) {
	f := newRAFixture(t, raOpts{wrapAudit: func(lg *audit.Log) core.AuditWriter {
		return failOutcomeAudit{Log: lg, outcome: audit.OutcomeRequested}
	}})
	_, err := f.invoke(tcraAdd.Op(), `{"name":"t1"}`, "")
	wantProblem(t, err, http.StatusServiceUnavailable, problem.AuditUnavailable)
	_, err = f.invoke(tcraCounted.Op(), `{}`, "")
	wantProblem(t, err, http.StatusServiceUnavailable, problem.AuditUnavailable)
	if add, read, _, _, _ := f.h.counts(); add != 0 || read != 0 {
		t.Fatalf("handler calls = %d add, %d read; want 0 and 0", add, read)
	}
}

// TC-RA-ERR7 (FR-1, NFR-1, CON-3): an Idempotency-Key on a SyncMutation is
// refused and the handler never runs.
func TestTCRAERR7_AnIdempotencyKeyOnASyncMutationIsRefusedAndThe(t *testing.T) {
	f := newRAFixture(t, raOpts{})
	_, err := f.invoke(tcraAdd.Op(), `{"name":"t1"}`, "k1")
	p := wantProblem(t, err, http.StatusBadRequest, problem.ValidationFailed)
	fe := fieldErrors(t, p)
	if len(fe) != 1 || fe[0].Path != "" || fe[0].Message != idemMessage {
		t.Fatalf("errors = %+v, want one at path \"\" with %q", fe, idemMessage)
	}
	if add, _, _, _, _ := f.h.counts(); add != 0 {
		t.Fatalf("handler calls = %d, want 0", add)
	}
	recs := f.records(t, tcraAdd.Op().ID)
	if len(recs) != 1 || recs[0].Outcome != audit.OutcomeRefused || recs[0].Problem != string(problem.ValidationFailed) {
		t.Fatalf("records = %+v, want one refused validation-failed", recs)
	}

	// A long-running mutating op with the same key still works.
	res, err := f.invoke(tcraSet.Op(), `{"x":"diff"}`, "k1")
	if err != nil || res.Status != http.StatusAccepted {
		t.Fatalf("long-running op with k1 = %+v, %v; want 202", res, err)
	}
}

// TC-RA-ERR8 (FR-13): a completed-record write failure keeps the 200 and
// marks the audit log unhealthy.
func TestTCRAERR8_ACompletedRecordWriteFailureKeepsThe200AndMarks(t *testing.T) {
	f := newRAFixture(t, raOpts{wrapAudit: func(lg *audit.Log) core.AuditWriter {
		return failOutcomeAudit{Log: lg, outcome: audit.OutcomeCompleted, corrupt: true}
	}})
	if !f.lg.Healthy() {
		t.Fatal("audit log unhealthy before the call")
	}
	res, err := f.invoke(tcraAdd.Op(), `{"name":"t1"}`, "")
	wantOK(t, res, err)
	if out := decodeBody[tcraOut](t, res.Body); out.Secret != raSentinel {
		t.Fatalf("body = %+v, want the handler output", out)
	}
	var found bool
	for line := range strings.Lines(f.logs.String()) {
		if strings.Contains(line, `msg="audit write failed"`) && strings.Contains(line, "outcome="+audit.OutcomeCompleted) {
			found = true
			if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, tcraAdd.Op().ID) {
				t.Fatalf("log line %q, want level ERROR naming the op", line)
			}
		}
	}
	if !found {
		t.Fatalf("no log line for the failed completed write in:\n%s", f.logs.String())
	}
	if strings.Contains(f.logs.String(), raSentinel) {
		t.Fatal("the handler output reached the log")
	}
	if f.lg.Healthy() {
		t.Fatal("audit log healthy after a failed completed write, want unhealthy")
	}
}

// TC-RA-111 (CON-3, FR-7, FR-13, NFR-1): a Done answer from the Admission
// hook ends the call with 200, requested then completed, and no operation;
// a !Done answer hands its input to Prepare.
func TestTCRA111_ADoneAnswerFromTheAdmissionHookEndsTheCallWith200(t *testing.T) {
	f := newRAFixture(t, raOpts{})
	res, err := f.invoke(tcraSet.Op(), `{"x":"same"}`, "")
	wantOK(t, res, err)
	if res.OperationID != "" {
		t.Fatalf("operationId = %q, want none", res.OperationID)
	}
	if out, ok := res.Body.(tcraSetOut); !ok || out != (tcraSetOut{Value: "same"}) {
		t.Fatalf("body = %#v, want the hook's output", res.Body)
	}
	if _, _, hook, prep, launches := f.h.counts(); hook != 1 || prep != 0 || launches != 0 {
		t.Fatalf("hook %d, prepare %d, launches %d; want 1, 0, 0", hook, prep, launches)
	}
	if n := f.st.writes.Load(); n != 0 {
		t.Fatalf("store writes = %d, want 0", n)
	}
	req, done := wantRequestedThen(t, f.records(t, tcraSet.Op().ID), audit.OutcomeCompleted)
	if string(req.Params) != `{"x":"same"}` {
		t.Fatalf("requested params = %s, want {\"x\":\"same\"}", req.Params)
	}
	if !maps.Equal(done.Facts, map[string]string{"f": "1"}) || done.OperationID != "" {
		t.Fatalf("completed record = %+v, want fact f=1 and no operationId", done)
	}

	res, err = f.invoke(tcraSet.Op(), `{"x":"diff"}`, "")
	if err != nil || res.Status != http.StatusAccepted || res.OperationID == "" {
		t.Fatalf("diff = %+v, %v; want 202 with an operation", res, err)
	}
	waitFor(t, "Prepare", func() bool { _, _, _, prep, _ := f.h.counts(); return prep == 1 })
	f.h.with(func() {
		if got := f.h.prepIn[0]; got != (tcraSetIn{X: "diff", Carried: "carried"}) {
			t.Errorf("Prepare input = %+v, want the hook's input with Carried", got)
		}
	})
	rec, err := f.mgr.Get(res.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec.Input, []byte(`{"x":"diff"}`)) {
		t.Fatalf("stored input = %s, want the original request", rec.Input)
	}
	var params []string
	for _, r := range f.records(t, tcraSet.Op().ID) {
		if r.Outcome == audit.OutcomeRequested {
			params = append(params, string(r.Params))
		}
	}
	if len(params) != 2 || params[1] != `{"x":"diff"}` {
		t.Fatalf("requested params = %v, want the second to be the original request", params)
	}
}

// TC-RA-E29 (FR-7, CON-3): an Idempotency-Key on a Done call stores no
// entry, so a retry runs the hook again.
func TestTCRAE29_AnIdempotencyKeyOnADoneCallStoresNoEntrySo(t *testing.T) {
	f := newRAFixture(t, raOpts{})
	f.h.with(func() { f.h.hookMode = alwaysDone })
	for i := range 2 {
		res, err := f.invoke(tcraSet.Op(), `{"x":"same"}`, "k1")
		if err != nil || res.Status != http.StatusOK {
			t.Fatalf("call %d = %+v, %v; want 200", i+1, res, err)
		}
	}
	if _, _, hook, _, _ := f.h.counts(); hook != 2 {
		t.Fatalf("hook calls = %d, want 2", hook)
	}
	tokenID := caller().TokenID
	if _, ok, err := f.mgr.LookupIdempotency(tokenID, "k1"); err != nil || ok {
		t.Fatalf("LookupIdempotency(k1) = %v, %v; want nothing stored", ok, err)
	}

	f.h.with(func() { f.h.hookMode = neverDone })
	res, err := f.invoke(tcraSet.Op(), `{"x":"same"}`, "k1")
	if err != nil || res.Status != http.StatusAccepted || res.OperationID == "" {
		t.Fatalf("third call = %+v, %v; want 202 with an operation", res, err)
	}
	if _, _, hook, _, _ := f.h.counts(); hook != 3 {
		t.Fatalf("hook calls = %d, want 3", hook)
	}
	e, ok, err := f.mgr.LookupIdempotency(tokenID, "k1")
	if err != nil || !ok || e.OperationID != res.OperationID {
		t.Fatalf("LookupIdempotency(k1) = %+v, %v, %v; want operation %s", e, ok, err, res.OperationID)
	}
}

// TC-RA-ERR41 (FR-9, FR-13, CON-3, NFR-1): the Done path still answers 409
// on a busy lane, refuses when the requested record fails, and rejects an
// undeclared Done.
func TestTCRAERR41_TheDonePathStillAnswers409OnABusyLane(t *testing.T) {
	f := newRAFixture(t, raOpts{})

	// Step 1: B holds the lane, Running.
	res, err := f.invoke(tcraHold.Op(), `{}`, "")
	if err != nil || res.Status != http.StatusAccepted {
		t.Fatalf("B = %+v, %v; want 202", res, err)
	}
	bID := res.OperationID
	waitFor(t, "B to run", func() bool {
		_, _, _, _, launches := f.h.counts()
		return launches == 1 && f.status(t, bID) == apitypes.StatusRunning
	})
	creates := func() int {
		page, err := f.st.List(store.ListQuery{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Records)
	}
	_, err = f.invoke(tcraSet.Op(), `{"x":"same"}`, "")
	p := wantProblem(t, err, http.StatusConflict, problem.Conflict)
	if p.Extensions["blockingOperationId"] != bID {
		t.Fatalf("blockingOperationId = %v, want B %s", p.Extensions["blockingOperationId"], bID)
	}
	if got := outcomesOf(f.records(t, tcraSet.Op().ID)); len(got) != 1 || got[0] != audit.OutcomeRefused {
		t.Fatalf("D records = %v, want one refused", got)
	}
	if n := creates(); n != 1 {
		t.Fatalf("operations = %d, want B only", n)
	}

	// Step 2: B ends; with requested writes failing, D's Done is refused.
	if err := detach.WriteExit(f.launcher.Dir(bID), detach.Exit{Started: time.Now(), Ended: time.Now()}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B to end", func() bool { return f.status(t, bID).Terminal() })
	failing := core.NewDispatcher(f.reg, core.DispatcherOptions{Logger: f.log,
		Audit: failOutcomeAudit{Log: f.lg, outcome: audit.OutcomeRequested}})
	_, err = f.call(failing, tcraSet.Op(), `{"x":"same"}`, "")
	wantProblem(t, err, http.StatusServiceUnavailable, problem.AuditUnavailable)
	for _, r := range f.records(t, tcraSet.Op().ID) {
		if r.Outcome == audit.OutcomeCompleted || r.Outcome == audit.OutcomeRequested {
			t.Fatalf("D record %+v after a failed requested write", r)
		}
	}
	if n := creates(); n != 1 {
		t.Fatalf("operations = %d, want B only", n)
	}

	// Step 3: N answers Done without the declaration.
	_, err = f.invoke(tcraUndeclared.Op(), `{}`, "")
	p = wantProblem(t, err, http.StatusInternalServerError, problem.InternalError)
	if p.Detail != "the server failed to handle the request" {
		t.Fatalf("detail = %q, want the generic detail", p.Detail)
	}
	var logged bool
	for line := range strings.Lines(f.logs.String()) {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, "op="+tcraUndeclared.Op().ID) {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no error log line naming %s in:\n%s", tcraUndeclared.Op().ID, f.logs.String())
	}
	if strings.Contains(f.logs.String(), raSentinel) {
		t.Fatal("the hook's output reached the log")
	}
	if n := creates(); n != 1 {
		t.Fatalf("operations = %d, want B only", n)
	}
	if _, _, _, prep, launches := f.h.counts(); prep != 1 || launches != 1 {
		t.Fatalf("prepare %d, launches %d; want B's only", prep, launches)
	}
}
