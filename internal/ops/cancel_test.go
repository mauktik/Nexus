package ops_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/systemspec"
)

const cancelOp = "system.operations.cancel"

func (w *world) cancel(p auth.Principal, id, key string) (core.InvokeResult, error) {
	return w.call(p, systemspec.OperationsCancel.Op(), `{"id":"`+id+`"}`, key)
}

// mustCancel cancels id and requires 200 with the operation.
func (w *world) mustCancel(p auth.Principal, id, key string) apitypes.Operation {
	w.t.Helper()
	res, err := w.cancel(p, id, key)
	if err != nil {
		w.t.Fatalf("cancel %s: %v", id, err)
	}
	if res.Status != http.StatusOK || res.OperationID != "" {
		w.t.Fatalf("cancel %s: status %d, operation id %q", id, res.Status, res.OperationID)
	}
	return res.Body.(apitypes.Operation)
}

// TC-55: canceling a queued operation answers 200 with it Canceled; the
// request record precedes the one Canceled record; the cancel creates no
// record and takes no lane; the canceled op never runs and the lane goes on.
func TestCancelAQueuedOperation(t *testing.T) { perBackend(t, testCancelAQueuedOperation) }

func testCancelAQueuedOperation(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	p := who("ops", "test:write")
	g := w.tp.gated("first")
	first := w.start(p, tpOps.run.Op(), `{"name":"first","lane":"q"}`, "")
	g.waitStarted(t)
	second := w.start(p, tpOps.run.Op(), `{"name":"second","lane":"q"}`, "")
	opsBefore := w.countOps()

	got := w.mustCancel(p, second.ID, "")
	if got.ID != second.ID || got.Status != apitypes.StatusCanceled || got.Ended == nil {
		t.Fatalf("cancel body = %+v", got)
	}
	g.open()
	w.waitStatus(first.ID, apitypes.StatusSucceeded)
	third := w.start(p, tpOps.run.Op(), `{"name":"third","lane":"q"}`, "")
	w.waitStatus(third.ID, apitypes.StatusSucceeded)

	if slices.Contains(w.tp.runOrder(), "second") {
		t.Fatal("the canceled operation ran")
	}
	if rec, _ := w.store.Get(second.ID); rec.Status != apitypes.StatusCanceled || rec.Started != nil {
		t.Fatalf("canceled record = %+v", rec)
	}
	if n := w.countOps(); n != opsBefore+1 {
		t.Fatalf("%d operations after cancel and one more start, want %d", n, opsBefore+1)
	}
	recs := w.records()
	reqAt, canceledAt, canceled := -1, -1, 0
	for i, r := range recs {
		switch {
		case r.Action == cancelOp && r.Outcome == audit.OutcomeRequested && r.OperationID == second.ID:
			reqAt = i
		case r.OperationID == second.ID && r.Outcome == string(apitypes.StatusCanceled):
			canceledAt = i
			canceled++
		}
	}
	if reqAt < 0 || canceled != 1 || canceledAt < reqAt {
		t.Fatalf("cancel request at %d, %d Canceled records (last at %d)", reqAt, canceled, canceledAt)
	}
}

// TC-204: a repeat cancel returns the same body, keys are ignored, and only
// actual cancels add state-change records.
func TestRepeatCancelReturnsTheSameResultAndIgnoresKeys(t *testing.T) {
	perBackend(t, testRepeatCancelReturnsTheSameResultAndIgnoresKeys)
}

func testRepeatCancelReturnsTheSameResultAndIgnoresKeys(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	p := who("ops", "test:write")
	g := w.tp.gated("held")
	defer g.open()
	w.start(p, tpOps.run.Op(), `{"name":"held","lane":"q"}`, "")
	g.waitStarted(t)
	one := w.start(p, tpOps.run.Op(), `{"name":"one","lane":"q"}`, "")
	two := w.start(p, tpOps.run.Op(), `{"name":"two","lane":"q"}`, "")
	opsBefore := w.countOps()

	body := func(o apitypes.Operation) string {
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	b1 := body(w.mustCancel(p, one.ID, ""))
	b2 := body(w.mustCancel(p, one.ID, ""))
	b3 := body(w.mustCancel(p, one.ID, "K"))
	if b1 != b2 || b1 != b3 {
		t.Fatalf("repeat cancels differ:\n%s\n%s\n%s", b1, b2, b3)
	}
	if o := w.mustCancel(p, two.ID, "K"); o.Status != apitypes.StatusCanceled || o.ID != two.ID {
		t.Fatalf("second op with key K: %+v", o)
	}
	if _, ok, _ := w.store.LookupIdempotency(p.TokenID, "K"); ok {
		t.Fatal("a cancel stored its key")
	}
	if n := w.countOps(); n != opsBefore {
		t.Fatalf("operations %d, want %d", n, opsBefore)
	}
	recs := w.records()
	requests := filterRecs(recs, func(r systemspec.AuditRecord) bool {
		return r.Action == cancelOp && r.Outcome == audit.OutcomeRequested
	})
	if len(requests) != 4 {
		t.Fatalf("%d cancel request records, want 4", len(requests))
	}
	canceled := filterRecs(recs, outcome(string(apitypes.StatusCanceled)))
	if len(canceled) != 2 || canceled[0].OperationID != one.ID || canceled[1].OperationID != two.ID {
		t.Fatalf("Canceled records = %+v", canceled)
	}
	for _, r := range recs {
		if r.Problem == string(problem.IdempotencyKeyReused) {
			t.Fatalf("a cancel was refused as a key reuse: %+v", r)
		}
	}
}

// TC-ERR18: cancel refusals leave state unchanged; scope comes from the
// loaded record, which loads before the scope check; denials are audited.
func TestCancelRefusalsAndAuthorization(t *testing.T) {
	perBackend(t, testCancelRefusalsAndAuthorization)
}

func testCancelRefusalsAndAuthorization(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	p := who("ops", "test:write", "test:read")
	held := w.tp.gated("held")
	defer held.open()
	running := w.start(p, tpOps.run.Op(), `{"name":"held"}`, "")
	held.waitStarted(t)
	ok := w.start(p, tpOps.run.Op(), `{"name":"ok"}`, "")
	w.tp.on("bad", func(context.Context, thingIn, *provider.Run) (thingOut, error) {
		return thingOut{}, problem.New(problem.DependencyUnavailable, "down").With("provider", "tp")
	})
	bad := w.start(p, tpOps.run.Op(), `{"name":"bad"}`, "")
	w.waitStatus(ok.ID, apitypes.StatusSucceeded)
	w.waitStatus(bad.ID, apitypes.StatusFailed)

	stop := make(chan struct{})
	canceledCtx := make(chan struct{})
	w.tp.on("long", func(ctx context.Context, in thingIn, _ *provider.Run) (thingOut, error) {
		close(stop)
		<-ctx.Done()
		close(canceledCtx)
		return thingOut{}, ctx.Err()
	})
	long := w.start(p, tpOps.cancelable.Op(), `{"name":"long"}`, "")
	<-stop

	// Step 1.
	for _, id := range []string{running.ID, ok.ID, bad.ID} {
		before, _ := w.store.Get(id)
		_, err := w.cancel(p, id, "")
		pe := wantProblem(t, err, problem.NotCancelable)
		if pe.Status != http.StatusConflict || pe.Extensions["operationId"] != id {
			t.Fatalf("not-cancelable = %+v", pe)
		}
		if after, _ := w.store.Get(id); after.Status != before.Status {
			t.Fatalf("%s changed from %s to %s", id, before.Status, after.Status)
		}
	}
	// Step 2: a token lacking the record's StartScope.
	reader := who("reader", "test:read")
	_, err := w.cancel(reader, long.ID, "")
	if pe := wantProblem(t, err, problem.Forbidden); pe.Extensions["requiredScope"] != "test:write" {
		t.Fatalf("requiredScope = %v", pe.Extensions["requiredScope"])
	}
	select {
	case <-canceledCtx:
		t.Fatal("a forbidden cancel reached the running operation")
	case <-time.After(50 * time.Millisecond):
	}
	if rec, _ := w.store.Get(long.ID); rec.Status != apitypes.StatusRunning {
		t.Fatalf("after a forbidden cancel: %s", rec.Status)
	}
	_, err = w.cancel(reader, "01890a5d-ac96-774b-bcce-b302099a8059", "")
	wantProblem(t, err, problem.NotFound)

	recs := w.records()
	if d := filterRecs(recs, func(r systemspec.AuditRecord) bool {
		return r.Action == cancelOp && r.Outcome == audit.OutcomeDenied && r.Problem == string(problem.Forbidden) &&
			r.TokenName == "reader" && r.OperationID == long.ID
	}); len(d) != 1 {
		t.Fatalf("denied cancel records = %+v", d)
	}
	if r := filterRecs(recs, func(r systemspec.AuditRecord) bool {
		return r.Action == cancelOp && r.Outcome == audit.OutcomeRefused && r.Problem == string(problem.NotCancelable)
	}); len(r) != 3 {
		t.Fatalf("%d not-cancelable refusals audited, want 3", len(r))
	}
	if r := filterRecs(recs, func(r systemspec.AuditRecord) bool {
		return r.Action == cancelOp && r.Outcome == audit.OutcomeRefused && r.Problem == string(problem.NotFound)
	}); len(r) != 1 {
		t.Fatalf("%d not-found refusals audited, want 1", len(r))
	}

	// With the StartScope the running CancelRunning op is canceled.
	if o := w.mustCancel(p, long.ID, ""); o.Status != apitypes.StatusRunning {
		t.Fatalf("cancel of a running op answers %s, want the op as it is (Running)", o.Status)
	}
	<-canceledCtx
	w.waitStatus(long.ID, apitypes.StatusCanceled)
}

// TC-ERR69: a cancel whose audit record cannot be written is refused, with
// no state change and no signal to the running operation. The running
// CancelRunning op is detached, its child run by detachtest.Local, with a
// spy as detach.Options.Signal.
func TestCancelIsRefusedWhenAuditFails(t *testing.T) {
	perBackend(t, testCancelIsRefusedWhenAuditFails)
}

func testCancelIsRefusedWhenAuditFails(t *testing.T, backend detachtest.Backend) {
	signaled := make(chan struct{})
	var once sync.Once
	var local detach.Options
	w := backendWorld(t, backend, func(d *detach.Options) {
		local = *d
		d.Signal = func(ctx context.Context, unit string, sig syscall.Signal) error {
			once.Do(func() { close(signaled) })
			return local.Signal(ctx, unit, sig)
		}
	})
	p := who("ops", "td:write")
	w.td.onPrepare("long", func(context.Context, thingIn) (detach.Spec, error) {
		return shJob("echo pid=$$; while :; do sleep 0.1; done"), nil
	})
	running := w.start(p, tdOps.job.Op(), `{"name":"long","lane":"c"}`, "")
	w.childPID(running.ID)
	t.Cleanup(func() { stopLocal(t, local, running.ID) })
	queued := w.start(p, tdOps.job.Op(), `{"name":"later","lane":"c"}`, "")

	for _, mode := range []int32{failWrite, failSync} {
		w.aud.mode.Store(mode)
		for _, id := range []string{queued.ID, running.ID} {
			_, err := w.cancel(p, id, "")
			wantProblem(t, err, problem.AuditUnavailable)
		}
	}
	w.aud.mode.Store(failNone)
	select {
	case <-signaled:
		t.Fatal("a refused cancel reached the running operation")
	case <-time.After(50 * time.Millisecond):
	}
	if r, _ := w.store.Get(queued.ID); r.Status != apitypes.StatusQueued {
		t.Fatalf("queued op is %s", r.Status)
	}
	if r, _ := w.store.Get(running.ID); r.Status != apitypes.StatusRunning {
		t.Fatalf("running op is %s", r.Status)
	}

	if o := w.mustCancel(p, queued.ID, ""); o.Status != apitypes.StatusCanceled {
		t.Fatalf("queued cancel: %s", o.Status)
	}
	w.mustCancel(p, running.ID, "")
	<-signaled
	w.waitStatus(running.ID, apitypes.StatusCanceled)
}
