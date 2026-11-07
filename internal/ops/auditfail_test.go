package ops_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
)

// TC-ERR14: a requested record that cannot be written (failing write, or
// failing sync) refuses the mutation with audit-unavailable before any
// record exists or the handler runs; reads are unaffected.
func TestAuditFailureRefusesTheMutation(t *testing.T) {
	w := newWorld(t)
	p := who("ops", "test:write", "test:read")
	for _, mode := range []int32{failWrite, failSync} {
		w.aud.mode.Store(mode)
		_, err := w.call(p, tpOps.run.Op(), `{"name":"web"}`, "")
		if pe := wantProblem(t, err, problem.AuditUnavailable); pe.Status != http.StatusServiceUnavailable {
			t.Fatalf("mode %d: %+v", mode, pe)
		}
		res, err := w.call(p, tpOps.peek.Op(), `{"name":"web"}`, "")
		if err != nil || res.Status != http.StatusOK {
			t.Fatalf("mode %d: read %d %v", mode, res.Status, err)
		}
	}
	if n := w.countOps(); n != 0 {
		t.Fatalf("%d operation records, want none", n)
	}
	if n := w.tp.runs.Load(); n != 0 {
		t.Fatalf("handler ran %d times", n)
	}
}

// TC-ERR15 (second-state): a failed state-change write is logged and the
// work completes; the next mutation is refused until audit writes succeed
// again.
func TestStateChangeAuditFailureBlocksMutationsUntilHealthy(t *testing.T) {
	w := newWorld(t)
	p := who("ops", "test:write")
	// Write 1 is the requested record; from write 2 on (the Queued, Running
	// and Succeeded state changes) every write fails.
	w.aud.failFrom.Store(2)
	op := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "")
	w.waitStatus(op.ID, apitypes.StatusSucceeded)
	if !strings.Contains(w.logs.String(), `"msg":"audit write failed"`) {
		t.Fatal("the failed state-change write was not logged")
	}

	_, err := w.call(p, tpOps.run.Op(), `{"name":"next"}`, "")
	wantProblem(t, err, problem.AuditUnavailable)

	w.aud.failFrom.Store(0)
	next := w.start(p, tpOps.run.Op(), `{"name":"next"}`, "")
	w.waitStatus(next.ID, apitypes.StatusSucceeded)
	if n := w.tp.runs.Load(); n != 2 {
		t.Fatalf("runs %d, want 2", n)
	}
}
