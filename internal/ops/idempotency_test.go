package ops_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/systemspec"
)

// TC-49: the same key and body return the original operation, also after it
// is terminal and after a restart on the same state.db; it runs once.
func TestIdempotencyKeyReturnsTheOriginalOperation(t *testing.T) {
	w := newWorld(t)
	p := who("ops", "test:write")
	first := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "K")
	second := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "K")
	if second.ID != first.ID {
		t.Fatalf("second POST: %s, want %s", second.ID, first.ID)
	}
	w.waitStatus(first.ID, apitypes.StatusSucceeded)
	if again := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "K"); again.ID != first.ID || again.Status != apitypes.StatusSucceeded {
		t.Fatalf("after terminal: %+v", again)
	}
	w.restart()
	if again := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "K"); again.ID != first.ID {
		t.Fatalf("after restart: %s, want %s", again.ID, first.ID)
	}
	if n := w.tp.runs.Load(); n != 1 {
		t.Fatalf("executed %d times, want 1", n)
	}
	if n := w.countOps(); n != 1 {
		t.Fatalf("%d operations, want 1", n)
	}
}

// TC-205: replays are audited replayed with the original operation ID and
// redacted parameters, and change no state.
func TestReplaysAreAuditedReplayed(t *testing.T) {
	w := newWorld(t)
	g := w.tp.gated("web")
	p := who("ops", "test:write")
	const body = `{"name":"web","secret":"SENTINEL205"}`
	op := w.start(p, tpOps.run.Op(), body, "K")
	g.waitStarted(t)
	if r := w.start(p, tpOps.run.Op(), body, "K"); r.ID != op.ID {
		t.Fatalf("replay while running: %s", r.ID)
	}
	g.open()
	w.waitStatus(op.ID, apitypes.StatusSucceeded)
	if r := w.start(p, tpOps.run.Op(), body, "K"); r.ID != op.ID {
		t.Fatalf("replay after terminal: %s", r.ID)
	}

	recs := w.records()
	if req := filterRecs(recs, outcome(audit.OutcomeRequested)); len(req) != 1 {
		t.Fatalf("%d requested records, want 1", len(req))
	}
	replays := filterRecs(recs, outcome(audit.OutcomeReplayed))
	if len(replays) != 2 {
		t.Fatalf("%d replayed records, want 2", len(replays))
	}
	for _, r := range replays {
		if r.OperationID != op.ID || string(r.Params) != `{"name":"web","secret":"[REDACTED]"}` ||
			r.TokenName != "ops" || r.Action != "tp.things.run" || r.Target != "web" {
			t.Fatalf("replayed record = %+v", r)
		}
	}
	var changes []string
	for _, r := range recs {
		if r.OperationID == op.ID && r.Outcome != audit.OutcomeReplayed {
			changes = append(changes, r.Outcome)
		}
	}
	if fmt.Sprint(changes) != "[Queued Running Succeeded]" {
		t.Fatalf("state changes = %v", changes)
	}
	if n := w.tp.runs.Load(); n != 1 || w.countOps() != 1 {
		t.Fatalf("runs %d, operations %d; want 1 and 1", n, w.countOps())
	}
}

// TC-229 (and the WI 3.2 T0 test): concurrent same-key requests that resolve
// to different lanes pass the step 8 re-check together, and
// CreateWithIdempotency decides: exactly one operation exists per key, and
// no requested record stands alone. The scripts-style op is detached, with
// its launcher injected through detach.Options so no real unit starts.
func TestConcurrentSameKeyOnDifferentLanesCreatesOneOperation(t *testing.T) {
	w := spyWorld(t, newUnitSpy())
	p := who("runner", "td:run")
	op := tdOps.script.Op()
	type answer struct {
		res core.InvokeResult
		err error
	}
	race := func(inputs [2]string, key string) [2]answer {
		var out [2]answer
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				<-start
				out[i].res, out[i].err = w.call(p, op, inputs[i], key)
			})
		}
		close(start)
		wg.Wait()
		return out
	}
	const rounds = 50
	for round := range rounds {
		before := w.countOps()
		key := fmt.Sprintf("same-%d", round)
		a := race([2]string{`{"name":"a"}`, `{"name":"a"}`}, key)
		for i, ans := range a {
			if ans.err != nil {
				t.Fatalf("round %d (a) request %d: %v", round, i, ans.err)
			}
		}
		if a[0].res.OperationID != a[1].res.OperationID {
			t.Fatalf("round %d (a): %s and %s", round, a[0].res.OperationID, a[1].res.OperationID)
		}
		if n := w.countOps(); n != before+1 {
			t.Fatalf("round %d (a): %d new operations, want 1", round, n-before)
		}

		key = fmt.Sprintf("diff-%d", round)
		b := race([2]string{`{"name":"a"}`, `{"name":"b"}`}, key)
		accepted, reused := 0, 0
		var created string
		for _, ans := range b {
			switch {
			case ans.err == nil && ans.res.Status == http.StatusAccepted:
				accepted++
				created = ans.res.OperationID
			default:
				p := wantProblem(t, ans.err, problem.IdempotencyKeyReused)
				reused++
				if id, _ := p.Extensions["operationId"].(string); id == "" {
					t.Fatalf("round %d (b): reuse without operationId", round)
				} else if created != "" && id != created {
					t.Fatalf("round %d (b): reuse names %s, created %s", round, id, created)
				}
			}
		}
		if accepted != 1 || reused != 1 {
			t.Fatalf("round %d (b): %d accepted, %d reused", round, accepted, reused)
		}
		if n := w.countOps(); n != before+2 {
			t.Fatalf("round %d (b): %d new operations, want 1", round, n-before-1)
		}
	}

	// Every requested record belongs to a created operation (its Queued
	// record names it) or is named by a replayed or refused record.
	recs := w.records()
	named := map[string]bool{}
	for _, r := range recs {
		if r.RequestID != "" {
			named[r.RequestID] = true
		}
	}
	requested := filterRecs(recs, func(r systemspec.AuditRecord) bool {
		return r.Outcome == audit.OutcomeRequested && r.Action == op.ID
	})
	if len(requested) < 2*rounds {
		t.Fatalf("%d requested records, want at least %d", len(requested), 2*rounds)
	}
	for _, r := range requested {
		if !named[r.ID] {
			t.Fatalf("requested record %s stands alone: %+v", r.ID, r)
		}
	}
	arbitrated := 0
	for _, r := range recs {
		if r.RequestID == "" {
			continue
		}
		if r.Outcome == audit.OutcomeRefused || r.Outcome == audit.OutcomeReplayed {
			arbitrated++
		}
		switch r.Outcome {
		case "Queued", audit.OutcomeReplayed, audit.OutcomeRefused:
		default:
			t.Fatalf("record %+v names a requested record", r)
		}
	}
	if n := w.countOps(); n != 2*rounds {
		t.Fatalf("%d operations, want %d", n, 2*rounds)
	}
	// The step 10 path must actually be exercised, not only the step 8
	// re-check.
	if arbitrated == 0 {
		t.Fatal("no round reached CreateWithIdempotency with both requests past step 8")
	}
	t.Logf("%d of %d rounds were decided by CreateWithIdempotency after both requests wrote requested", arbitrated, rounds)
}

// TC-E10: the key lookup precedes the lane check and Admit, and a refused
// request stores no key.
func TestIdempotencyBeforeAdmissionAndValidation(t *testing.T) {
	w := newWorld(t)
	g := w.tp.gated("web")
	p := who("ops", "test:write")
	const body = `{"name":"web","lane":"r","policy":"reject"}`
	op := w.start(p, tpOps.run.Op(), body, "K")
	g.waitStarted(t)

	// Step 1: the lane is held by the original.
	if r := w.start(p, tpOps.run.Op(), body, "K"); r.ID != op.ID {
		t.Fatalf("replay on a busy lane: %s", r.ID)
	}
	// Step 2: Admit now refuses.
	w.tp.refuse.Store(true)
	if r := w.start(p, tpOps.run.Op(), body, "K"); r.ID != op.ID {
		t.Fatalf("replay with Admit refusing: %s", r.ID)
	}
	w.tp.refuse.Store(false)
	if n := len(filterRecs(w.records(), outcome(audit.OutcomeReplayed))); n != 2 {
		t.Fatalf("%d replayed records, want 2", n)
	}

	// Step 3: invalid input with a new key stores nothing.
	before := len(filterRecs(w.records(), outcome(audit.OutcomeRefused)))
	_, err := w.call(p, tpOps.run.Op(), `{"name":"web2","bogus":1}`, "K2")
	wantProblem(t, err, problem.ValidationFailed)
	if _, ok, err := w.store.LookupIdempotency(p.TokenID, "K2"); ok || err != nil {
		t.Fatalf("a refused request stored its key (%v)", err)
	}
	if n := len(filterRecs(w.records(), outcome(audit.OutcomeRefused))); n != before+1 {
		t.Fatalf("%d refused records, want %d", n, before+1)
	}
	fixed := w.start(p, tpOps.run.Op(), `{"name":"web2"}`, "K2")
	if fixed.ID == op.ID {
		t.Fatal("the fixed request replayed the original")
	}
	w.waitStatus(fixed.ID, apitypes.StatusSucceeded)
	g.open()
	w.waitStatus(op.ID, apitypes.StatusSucceeded)
}

// TC-ERR17: a reused key with a different body is refused and starts
// nothing; the same key from another token is a different key.
func TestKeyReuseWithADifferentRequest(t *testing.T) {
	w := newWorld(t)
	a, b := who("alice", "test:write"), who("bob", "test:write")
	op := w.start(a, tpOps.run.Op(), `{"name":"web"}`, "K")
	w.waitStatus(op.ID, apitypes.StatusSucceeded)

	_, err := w.call(a, tpOps.run.Op(), `{"name":"web","note":"other"}`, "K")
	pe := wantProblem(t, err, problem.IdempotencyKeyReused)
	if pe.Status != http.StatusUnprocessableEntity || pe.Extensions["operationId"] != op.ID {
		t.Fatalf("problem = %+v", pe)
	}
	if w.tp.runs.Load() != 1 || w.countOps() != 1 {
		t.Fatalf("the reuse started something: runs %d, operations %d", w.tp.runs.Load(), w.countOps())
	}
	refused := filterRecs(w.records(), outcome(audit.OutcomeRefused))
	if len(refused) != 1 || refused[0].Problem != string(problem.IdempotencyKeyReused) {
		t.Fatalf("refused records = %+v", refused)
	}

	other := w.start(b, tpOps.run.Op(), `{"name":"web"}`, "K")
	if other.ID == op.ID {
		t.Fatal("another token's key replayed alice's operation")
	}
	w.waitStatus(other.ID, apitypes.StatusSucceeded)
}
