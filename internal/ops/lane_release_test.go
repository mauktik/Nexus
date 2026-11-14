package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
)

// laneRaceIters is the number of submit-on-terminal rounds of each loop
// subtest. Before the fix the first round that races fails the subtest;
// the measured race rate was 80 to 98 per 100 rounds. The round count
// keeps the test well under 30 seconds with -race.
const laneRaceIters = 100

// waitTerminal tight-polls Manager.Get, as a client polls GET
// /v1/operations/{id}, and returns the record once it reads terminal.
func (w *world) waitTerminal(id string) store.OpRecord {
	w.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rec, err := w.mgr.Get(id)
		if err != nil {
			w.t.Fatalf("get %s: %v", id, err)
		}
		if rec.Status.Terminal() {
			return rec
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("operation %s never read terminal: %s", id, rec.Status)
		}
	}
}

// refusedWhileTerminal fails the test when err is a 409: the caller only
// submits once every holder it knows of on the lane, other than those in
// running, reads terminal, so a 409 is a lane held by a finished operation.
func (w *world) refusedWhileTerminal(round int, err error, running ...string) {
	w.t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.Conflict {
		return
	}
	blk, _ := pe.Extensions["blockingOperationId"].(string)
	rec, gerr := w.mgr.Get(blk)
	w.t.Fatalf("round %d: 409 %q naming %s (reads %s, %v) while every holder other than %v reads terminal",
		round, pe.Detail, blk, rec.Status, gerr, running)
}

// logged reports whether the manager logged msg for operation id.
func (w *world) logged(msg, id string) bool {
	for _, line := range strings.Split(w.logs.String(), "\n") {
		if strings.Contains(line, `"msg":"`+msg+`"`) && strings.Contains(line, `"operationId":"`+id+`"`) {
			return true
		}
	}
	return false
}

// waitLogged waits for the manager to log msg for operation id.
func (w *world) waitLogged(msg, id string) {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !w.logged(msg, id) {
		if time.Now().After(deadline) {
			w.t.Fatalf("no %q log for %s", msg, id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// heldPrepare scripts name's Prepare to ignore its context and block until
// release is closed; entered is closed once it runs.
func (w *world) heldPrepare(name string) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	w.td.onPrepare(name, func(context.Context, thingIn) (detach.Spec, error) {
		close(entered)
		<-release
		return shJob("true"), nil
	})
	return entered, release
}

// cancelHeld starts the detached job name on lane with a Prepare that
// ignores its context, cancels it while Prepare runs and requires the
// record to read Canceled. Prepare stays blocked until release is closed.
func (w *world) cancelHeld(p auth.Principal, name, lane, policy string) (id string, release chan struct{}) {
	w.t.Helper()
	entered, release := w.heldPrepare(name)
	a := w.start(p, tdOps.job.Op(), fmt.Sprintf(`{"name":%q,"lane":%q,"policy":%q}`, name, lane, policy), "")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		w.t.Fatalf("Prepare of %s never ran", a.ID)
	}
	if got := w.mustCancel(p, a.ID, ""); got.Status != apitypes.StatusCanceled {
		w.t.Fatalf("cancel of queued %s answered %s", a.ID, got.Status)
	}
	if rec, err := w.mgr.Get(a.ID); err != nil || rec.Status != apitypes.StatusCanceled {
		w.t.Fatalf("Get(%s) after the cancel: %s %v", a.ID, rec.Status, err)
	}
	return a.ID, release
}

// TestLaneFreeWhenRecordReadsTerminal is the regression test of bug
// ops-lane-release-race: once a client reads an operation terminal, the
// operation no longer holds its lane. Each subtest submits the next
// operation at the moment the previous record reads terminal and fails on
// any 409 while every holder of the lane reads terminal.
func TestLaneFreeWhenRecordReadsTerminal(t *testing.T) {
	t.Run("http-limit1-submit-on-visible", func(t *testing.T) {
		w := newWorld(t)
		w.serve()
		tok := w.mint("racer", "test:write", "test:read")
		for i := range laneRaceIters {
			resp, b := w.do(http.MethodPost, fmt.Sprintf("/v1/providers/tp/things/h%d/run", i), tok, `{"lane":"h","policy":"reject"}`)
			if resp.StatusCode == http.StatusConflict {
				blk, _ := decodeProblem(t, b)["blockingOperationId"].(string)
				_, gb := w.do(http.MethodGet, "/v1/operations/"+blk, tok, "")
				t.Fatalf("round %d: POST -> 409 %s; GET /v1/operations/%s -> status %s", i, b, blk, decodeOp(t, gb).Status)
			}
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("round %d: POST -> %d %s", i, resp.StatusCode, b)
			}
			id := decodeOp(t, b).ID
			// Wait as the CLI's --wait does: the first GET that reads terminal.
			deadline := time.Now().Add(15 * time.Second)
			for {
				_, gb := w.do(http.MethodGet, "/v1/operations/"+id, tok, "")
				o := decodeOp(t, gb)
				if o.Status.Terminal() {
					if o.Status != apitypes.StatusSucceeded {
						t.Fatalf("round %d: %s ended %s", i, id, o.Status)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("round %d: %s never read terminal", i, id)
				}
			}
		}
	})

	t.Run("limit2-no-409-while-holder-terminal", func(t *testing.T) {
		w := newWorld(t)
		p := who("ops", "test:write", "test:read")
		g := w.tp.gated("holder")
		t.Cleanup(g.open)
		holder := w.start(p, tpOps.run.Op(), `{"name":"holder","lane":"two","policy":"reject","limit":2}`, "")
		g.waitStarted(t)
		for i := range laneRaceIters {
			res, err := w.call(p, tpOps.run.Op(), fmt.Sprintf(`{"name":"b%d","lane":"two","policy":"reject","limit":2}`, i), "")
			w.refusedWhileTerminal(i, err, holder.ID)
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
			if rec := w.waitTerminal(res.OperationID); rec.Status != apitypes.StatusSucceeded {
				t.Fatalf("round %d: %s ended %s", i, rec.ID, rec.Status)
			}
		}
		if rec, _ := w.mgr.Get(holder.ID); rec.Status != apitypes.StatusRunning {
			t.Fatalf("holder %s reads %s, want Running throughout", holder.ID, rec.Status)
		}
		g.open()
		w.waitTerminal(holder.ID)
	})

	t.Run("queued-cancel-prepare-ignores-ctx", func(t *testing.T) {
		spy := newUnitSpy()
		w := spyWorld(t, spy)
		p := who("ops", "td:write", "td:read", "test:write", "test:read")
		for i := range 5 {
			lane := fmt.Sprintf("cq%d", i)
			a, release := w.cancelHeld(p, fmt.Sprintf("cq-a%d", i), lane, "reject")
			// The next operation is in-process, so no watcher poll paces the
			// rounds; the lane is the same key.
			res, err := w.call(p, tpOps.run.Op(), fmt.Sprintf(`{"name":"cq-b%d","lane":%q,"policy":"reject"}`, i, lane), "")
			close(release)
			w.refusedWhileTerminal(i, err)
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
			if rec := w.waitTerminal(res.OperationID); rec.Status != apitypes.StatusSucceeded {
				t.Fatalf("round %d: %s ended %s", i, rec.ID, rec.Status)
			}
			w.waitLogged("operation abandoned: no longer queued", a)
			if n := spy.launched(a); n != 0 {
				t.Fatalf("round %d: canceled %s launched %d times", i, a, n)
			}
			if rec, _ := w.mgr.Get(a); rec.Status != apitypes.StatusCanceled {
				t.Fatalf("round %d: canceled %s now reads %s", i, a, rec.Status)
			}
		}
	})

	t.Run("recovery-terminal-path", func(t *testing.T) {
		w := spyWorld(t, newUnitSpy())
		p := who("ops", "test:write", "test:read")
		for i := range 10 {
			lane := fmt.Sprintf("rc%d", i)
			a := w.seed(tdRec(fmt.Sprintf("rc-a%d", i), store.Lane{Key: lane, Policy: store.LaneReject, Limit: 1}), apitypes.StatusRunning)
			now := time.Now()
			// The unit ended while nexusd was down.
			w.writeExit(a, detach.Exit{Started: now, Ended: now})
			w.restart()
			recovered := w.recoverNow()
			if rec := w.waitTerminal(a); rec.Status != apitypes.StatusSucceeded {
				t.Fatalf("round %d: recovered %s ended %s", i, a, rec.Status)
			}
			res, err := w.call(p, tpOps.run.Op(), fmt.Sprintf(`{"name":"rc-b%d","lane":%q,"policy":"reject"}`, i, lane), "")
			w.refusedWhileTerminal(i, err)
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
			waitClosed(t, recovered, 30*time.Second, "recovery")
			w.waitTerminal(res.OperationID)
		}
	})

	t.Run("queue-lane-starts-on-terminal", func(t *testing.T) {
		spy := newUnitSpy()
		w := spyWorld(t, spy)
		p := who("ops", "td:write", "td:read", "test:write", "test:read")
		for i := range 3 {
			lane := fmt.Sprintf("ql%d", i)
			a, release := w.cancelHeld(p, fmt.Sprintf("ql-a%d", i), lane, "queue")
			started := make(chan struct{})
			b := fmt.Sprintf("ql-b%d", i)
			w.tp.on(b, func(_ context.Context, in thingIn, _ *provider.Run) (thingOut, error) {
				close(started)
				return thingOut{Name: in.Name}, nil
			})
			next := w.start(p, tpOps.run.Op(), fmt.Sprintf(`{"name":%q,"lane":%q,"policy":"queue"}`, b, lane), "")
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatalf("round %d: %s still waits behind %s, which reads Canceled", i, next.ID, a)
			}
			close(release)
			if rec := w.waitTerminal(next.ID); rec.Status != apitypes.StatusSucceeded {
				t.Fatalf("round %d: %s ended %s", i, next.ID, rec.Status)
			}
			w.waitLogged("operation abandoned: no longer queued", a)
			if n := spy.launched(a); n != 0 {
				t.Fatalf("round %d: canceled %s launched %d times", i, a, n)
			}
		}
	})
}

// holdCommit wraps the real store and holds the worker of one operation in
// its CommitTerminal, after the real commit (or, with fail set, after an
// injected failure that commits nothing) and before the call returns: a
// crash at that point leaves the store exactly as it is while the worker
// is held.
type holdCommit struct {
	ops.Store
	id        atomic.Value // string; the held operation
	fail      bool
	committed chan struct{}
	release   chan struct{}
	freed     sync.Once
}

func newHoldCommit(s ops.Store) *holdCommit {
	return &holdCommit{Store: s, committed: make(chan struct{}), release: make(chan struct{})}
}

// free lets the held worker return.
func (h *holdCommit) free() { h.freed.Do(func() { close(h.release) }) }

func (h *holdCommit) CommitTerminal(id string, from, to apitypes.OperationStatus, result json.RawMessage, perr *problem.Error) (store.OpRecord, error) {
	if held, _ := h.id.Load().(string); held != id {
		return h.Store.CommitTerminal(id, from, to, result, perr)
	}
	h.id.Store("")
	var rec store.OpRecord
	err := errors.New("injected: the terminal commit failed and nothing was committed")
	if !h.fail {
		rec, err = h.Store.CommitTerminal(id, from, to, result, perr)
	}
	close(h.committed)
	<-h.release
	return rec, err
}

// holdWorld is a spy world whose store is a holdCommit; hold is the one of
// the current store, replaced on each restart.
func holdWorld(t *testing.T, spy *unitSpy) (*world, *atomic.Pointer[holdCommit]) {
	var hold atomic.Pointer[holdCommit]
	var opsDir atomic.Value
	w := newWorld(t, func(o *ops.Options) {
		opsDir.Store(o.Dir)
		withDetach(t, spy.options(func() string { return opsDir.Load().(string) }))(o)
		h := newHoldCommit(o.Store)
		hold.Store(h)
		o.Store = h
	})
	return w, &hold
}

// conflictNaming requires err to be a 409 naming id.
func conflictNaming(t *testing.T, err error, id string) {
	t.Helper()
	pe := wantProblem(t, err, problem.Conflict)
	if got := pe.Extensions["blockingOperationId"]; got != id {
		t.Fatalf("409 names %v, want %s", got, id)
	}
}

// TestLaneReapCrashSafety holds a worker between its terminal commit and
// its slot release, admits on the lane, and restarts from the store as a
// crash at that point would leave it. A holder is retired only once its
// record reads terminal; a restart then finds at most one holder on a
// limit-1 lane, never a terminal one.
func TestLaneReapCrashSafety(t *testing.T) {
	p := who("ops", "test:write", "test:read", "td:write", "td:read")
	runIn := func(name, lane string) string {
		return fmt.Sprintf(`{"name":%q,"lane":%q,"policy":"reject"}`, name, lane)
	}

	t.Run("crash-after-commit-before-release", func(t *testing.T) {
		spy := newUnitSpy()
		w, hold := holdWorld(t, spy)
		h := hold.Load()
		t.Cleanup(h.free)
		g := w.tp.gated("a")
		a := w.start(p, tpOps.run.Op(), runIn("a", "cs"), "")
		g.waitStarted(t)
		h.id.Store(a.ID)
		g.open()
		<-h.committed
		if rec, _ := w.mgr.Get(a.ID); rec.Status != apitypes.StatusSucceeded {
			h.free()
			t.Fatalf("A reads %s, want Succeeded", rec.Status)
		}
		// A's worker is held after its commit: its slot is not released.
		spy.mu.Lock()
		spy.onLaunch = func(detach.Invocation) error { return nil } // B keeps running
		spy.mu.Unlock()
		res, err := w.call(p, tdOps.job.Op(), runIn("b", "cs"), "")
		if err != nil {
			h.free()
			t.Fatalf("B on the lane of A, which reads Succeeded: %v", err)
		}
		b := res.OperationID
		w.waitStatus(b, apitypes.StatusRunning)
		_, err = w.call(p, tpOps.run.Op(), runIn("c0", "cs"), "")
		conflictNaming(t, err, b)
		h.free()

		// A crash now leaves A Succeeded and B Running in the store; the
		// release of A's worker changes nothing there.
		w.restart()
		if err := w.mgr.Restore(w.lookup()); err != nil {
			t.Fatal(err)
		}
		_, err = w.call(p, tpOps.run.Op(), runIn("c1", "cs"), "")
		conflictNaming(t, err, b)

		spy.mu.Lock()
		spy.onLaunch = newUnitSpy().onLaunch
		spy.mu.Unlock()
		now := time.Now()
		w.writeExit(b, detach.Exit{Started: now, Ended: now})
		waitClosed(t, w.mgr.Recover(), 30*time.Second, "recovery")
		if rec := w.waitTerminal(b); rec.Status != apitypes.StatusSucceeded {
			t.Fatalf("B ended %s", rec.Status)
		}
		if rec, _ := w.mgr.Get(a.ID); rec.Status != apitypes.StatusSucceeded {
			t.Fatalf("A reads %s after the restart", rec.Status)
		}
		res, err = w.call(p, tpOps.run.Op(), runIn("c2", "cs"), "")
		w.refusedWhileTerminal(0, err)
		if err != nil {
			t.Fatal(err)
		}
		w.waitTerminal(res.OperationID)
	})

	t.Run("commit-failure-keeps-holder", func(t *testing.T) {
		w, hold := holdWorld(t, newUnitSpy())
		h := hold.Load()
		t.Cleanup(h.free)
		h.fail = true
		g := w.tp.gated("a")
		a := w.start(p, tpOps.run.Op(), runIn("a", "cf"), "")
		g.waitStarted(t)
		h.id.Store(a.ID)
		g.open()
		<-h.committed
		// Nothing was committed: A reads Running and keeps its slot.
		rec, _ := w.mgr.Get(a.ID)
		_, err := w.call(p, tpOps.run.Op(), runIn("b", "cf"), "")
		h.free()
		if rec.Status != apitypes.StatusRunning {
			t.Fatalf("A reads %s, want Running", rec.Status)
		}
		conflictNaming(t, err, a.ID)
		w.waitLogged("cannot commit the operation's end", a.ID)

		// A restart finds A Running: in-process, so it ends outcome-unknown,
		// and the lane is then free.
		w.restart()
		waitClosed(t, w.recoverNow(), 30*time.Second, "recovery")
		got := w.waitTerminal(a.ID)
		wantOutcomeUnknown(t, got, "in-process operations are not recovered")
		res, err := w.call(p, tpOps.run.Op(), runIn("c", "cf"), "")
		w.refusedWhileTerminal(0, err)
		if err != nil {
			t.Fatal(err)
		}
		w.waitTerminal(res.OperationID)
	})
}
