package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// lookup finds a binding of the world's registry by op ID, as the daemon
// wires restart recovery.
func (w *world) lookup() func(string) (provider.Binding, bool) {
	bindings := map[string]provider.Binding{}
	for _, e := range w.reg.Entries() {
		bindings[e.Binding.Op().ID] = e.Binding
	}
	return func(id string) (provider.Binding, bool) {
		b, ok := bindings[id]
		return b, ok
	}
}

// recoverNow runs both stages of restart recovery on the world's manager
// and returns the channel closed once classification is over.
func (w *world) recoverNow() <-chan struct{} {
	w.t.Helper()
	if err := w.mgr.Restore(w.lookup()); err != nil {
		w.t.Fatalf("Restore: %v", err)
	}
	return w.mgr.Recover()
}

// waitClosed waits for c, failing after limit.
func waitClosed(t *testing.T, c <-chan struct{}, limit time.Duration, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(limit):
		t.Fatalf("%s did not finish within %v", what, limit)
	}
}

// seed stores a record as a previous nexusd left it: created Queued, then
// moved to status.
func (w *world) seed(rec store.OpRecord, status apitypes.OperationStatus) string {
	w.t.Helper()
	if rec.TokenName == "" {
		rec.TokenName = "seeder"
	}
	if rec.Exec == "" {
		rec.Exec = store.ExecInProcess
	}
	res, err := w.store.CreateWithIdempotency(rec, store.IdempotencyKey{})
	if err != nil {
		w.t.Fatal(err)
	}
	if status == apitypes.StatusRunning {
		if _, err := w.store.CompareAndSetStatus(res.ID, apitypes.StatusQueued, apitypes.StatusRunning); err != nil {
			w.t.Fatal(err)
		}
	}
	return res.ID
}

func tpRec(name string, lane store.Lane) store.OpRecord {
	return store.OpRecord{Action: tpOps.run.Op().ID, Provider: "tp", Target: name,
		Input: json.RawMessage(`{"name":"` + name + `"}`), StartScope: "test:write", Lane: lane}
}

func tdRec(name string, lane store.Lane) store.OpRecord {
	return store.OpRecord{Action: tdOps.job.Op().ID, Provider: "td", Target: name, Exec: store.ExecDetached,
		Input: json.RawMessage(`{"name":"` + name + `"}`), StartScope: "td:write", Lane: lane}
}

// writeExit writes operation id's exit.json in the supervisor's format.
func (w *world) writeExit(id string, x detach.Exit) {
	w.t.Helper()
	if err := os.MkdirAll(w.opDir(id), 0o700); err != nil {
		w.t.Fatal(err)
	}
	if err := detach.WriteExit(w.opDir(id), x); err != nil {
		w.t.Fatal(err)
	}
}

// stateLog records the UnitState calls per unit.
type stateLog struct {
	mu    sync.Mutex
	calls map[string][]time.Time
}

func (s *stateLog) wrap(f func(context.Context, string) (detach.UnitState, error)) func(context.Context, string) (detach.UnitState, error) {
	return func(ctx context.Context, unit string) (detach.UnitState, error) {
		s.mu.Lock()
		if s.calls == nil {
			s.calls = map[string][]time.Time{}
		}
		s.calls[unit] = append(s.calls[unit], time.Now())
		s.mu.Unlock()
		return f(ctx, unit)
	}
}

func (s *stateLog) of(id string) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls[detach.Unit(id)])
}

func wantOutcomeUnknown(t *testing.T, rec store.OpRecord, detail string) {
	t.Helper()
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.OutcomeUnknown ||
		!strings.Contains(rec.Error.Detail, detail) {
		t.Fatalf("operation %s: status %s, error %+v; want Failed outcome-unknown with %q", rec.ID, rec.Status, rec.Error, detail)
	}
	if rec.Ended == nil {
		t.Fatalf("operation %s has no end time", rec.ID)
	}
}

// TC-68: every row of the Restart recovery table. Queued records
// re-enqueue in ID order; a Queued record with a sensitive value never
// runs; exit.json reaches Finalize; an active unit is reattached and its
// output continues at the next offset; an absent unit is re-checked after
// 2 seconds and ends outcome-unknown; an in-process Running record ends
// outcome-unknown.
func TestRecoveryClassifiesEveryTableRow(t *testing.T) {
	perBackend(t, testRecoveryClassifiesEveryTableRow)
}

func testRecoveryClassifiesEveryTableRow(t *testing.T, backend detachtest.Backend) {
	var states stateLog
	var seams detach.Options
	w := newWorld(t, withBackend(t, backend, &seams, func(d *detach.Options) {
		d.UnitState = states.wrap(d.UnitState)
	}))

	fifo := store.Lane{Key: "fifo", Policy: store.LaneQueue, Limit: 1}
	q1 := w.seed(tpRec("q1", fifo), apitypes.StatusQueued)
	sensitive := tpRec("sens", store.Lane{})
	sensitive.HasSensitive = true
	sens := w.seed(sensitive, apitypes.StatusQueued)
	q2 := w.seed(tpRec("q2", fifo), apitypes.StatusQueued)
	fin := w.seed(tdRec("fin", store.Lane{}), apitypes.StatusRunning)
	live := w.seed(tdRec("live", store.Lane{}), apitypes.StatusRunning)
	gone := w.seed(tdRec("gone", store.Lane{}), apitypes.StatusRunning)
	inproc := w.seed(tpRec("inproc", store.Lane{}), apitypes.StatusRunning)
	q3 := w.seed(tpRec("q3", fifo), apitypes.StatusQueued)

	// fin's supervisor finished while nexusd was down.
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	w.writeExit(fin, detach.Exit{ExitCode: 0, Started: started, Ended: started.Add(time.Second)})

	// live's supervisor still runs: it printed before the restart and
	// prints again once the test lets it.
	goOn := filepath.Join(t.TempDir(), "go-on")
	if err := os.MkdirAll(w.opDir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := detach.WriteSpec(w.opDir(live), shJob("echo before; while [ ! -e "+goOn+" ]; do sleep 0.05; done; echo after")); err != nil {
		t.Fatal(err)
	}
	inv := detach.Invocation{ID: live, Unit: detach.Unit(live), Dir: w.opDir(live)}
	if err := seams.Launch(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	defer stopLocal(t, seams, live)
	reader := oplog.NewReader(filepath.Join(w.opDir(live), "output"))
	var before oplog.Page
	deadline := time.Now().Add(10 * time.Second)
	for {
		// Up to and including "before": the process backend without a
		// cgroup writes its CON-10 note as a frame first.
		p, err := reader.Read(0, 0)
		var text strings.Builder
		for _, f := range p.Frames {
			text.WriteString(f.D)
		}
		if err == nil && strings.Contains(text.String(), "before\n") {
			before = p
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the live supervisor did not print before: %v %q", err, text.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	t0 := time.Now()
	done := w.recoverNow()

	// Queued, in ID order, each after the previous one on the FIFO lane.
	w.waitStatus(q3, apitypes.StatusSucceeded)
	w.waitStatus(q1, apitypes.StatusSucceeded)
	w.waitStatus(q2, apitypes.StatusSucceeded)
	if got := w.tp.runOrder(); !slices.Equal(got, []string{"q1", "q2", "q3"}) {
		t.Fatalf("run order %q, want q1 q2 q3", got)
	}

	// Queued with a sensitive value: never started.
	rec := w.waitStatus(sens, apitypes.StatusFailed)
	wantOutcomeUnknown(t, rec, "never started")
	if rec.Started != nil {
		t.Fatalf("the sensitive operation has a start time %v", rec.Started)
	}

	// exit.json: Finalize reports the real outcome.
	rec = w.waitStatus(fin, apitypes.StatusSucceeded)
	if string(rec.Result) != `{"changed":true,"name":"fin"}` {
		t.Fatalf("fin result %s", rec.Result)
	}
	var finals []finalCall
	for _, c := range w.td.finalCalls() {
		if c.name == "fin" {
			finals = append(finals, c)
		}
	}
	if len(finals) != 1 || !finals[0].exit.Started.Equal(started) {
		t.Fatalf("Finalize calls for fin: %+v", finals)
	}

	// In-process Running: there is no in-process recovery.
	wantOutcomeUnknown(t, w.waitStatus(inproc, apitypes.StatusFailed), "")

	// Absent: re-checked RecheckDelay after the first check, then lost.
	rec = w.waitStatus(gone, apitypes.StatusFailed)
	wantOutcomeUnknown(t, rec, "lost detail for gone")
	calls := states.of(gone)
	if len(calls) < 2 || calls[len(calls)-1].Sub(calls[0]) < detach.RecheckDelay-50*time.Millisecond {
		t.Fatalf("gone's unit state calls %v: want a re-check %v after the first", calls, detach.RecheckDelay)
	}
	if time.Since(t0) < detach.RecheckDelay {
		t.Fatalf("gone ended %v after recovery began, before the re-check", time.Since(t0))
	}

	// Active: still Running, reattached; the output continues after the
	// offset read before the restart, and the watcher reports the end.
	waitClosed(t, done, 30*time.Second, "classification")
	if rec, err := w.store.Get(live); err != nil || rec.Status != apitypes.StatusRunning {
		t.Fatalf("live: %+v %v, want Running", rec, err)
	}
	if err := os.WriteFile(goOn, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec = w.waitStatus(live, apitypes.StatusSucceeded)
	if string(rec.Result) != `{"changed":true,"name":"live"}` {
		t.Fatalf("live result %s", rec.Result)
	}
	after, err := reader.Read(before.Next, 0)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, f := range after.Frames {
		text.WriteString(f.D)
	}
	if text.String() != "after\n" || len(after.Frames) == 0 || after.Frames[0].O == nil || *after.Frames[0].O != before.Next {
		t.Fatalf("output after offset %d: %+v", before.Next, after.Frames)
	}
}

// TC-E13: exit.json written between the absent answer and the re-check
// wins: the real outcome, not outcome-unknown.
func TestRecoveryExitDuringRecheck(t *testing.T) { perBackend(t, testRecoveryExitDuringRecheck) }

func testRecoveryExitDuringRecheck(t *testing.T, _ detachtest.Backend) {
	var opsDir atomic.Value
	var calls atomic.Int64
	w := newWorld(t, func(o *ops.Options) {
		opsDir.Store(o.Dir)
		withDetach(t, detach.Options{
			Launch: func(context.Context, detach.Invocation) error { return errors.New("no launch in this test") },
			UnitState: func(_ context.Context, unit string) (detach.UnitState, error) {
				if calls.Add(1) == 1 {
					id := strings.TrimSuffix(strings.TrimPrefix(unit, detach.UnitPrefix), ".service")
					now := time.Now()
					if err := detach.WriteExit(filepath.Join(opsDir.Load().(string), id),
						detach.Exit{ExitCode: 0, Started: now, Ended: now}); err != nil {
						return detach.UnitState{}, err
					}
				}
				return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
			},
			Signal:    func(context.Context, string, syscall.Signal) error { return errors.New("no signal in this test") },
			ListUnits: func(context.Context) ([]string, error) { return nil, nil },
		})(o)
	})
	id := w.seed(tdRec("late", store.Lane{}), apitypes.StatusRunning)
	if err := os.MkdirAll(w.opDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, w.recoverNow(), 15*time.Second, "classification")
	rec := w.waitStatus(id, apitypes.StatusSucceeded)
	if string(rec.Result) != `{"changed":true,"name":"late"}` || calls.Load() < 1 {
		t.Fatalf("late: %+v (state calls %d)", rec, calls.Load())
	}
}

// commitCrash fails the next CommitTerminal of a Running record without
// committing, as a crash between Finalize and the commit would leave it.
type commitCrash struct {
	ops.Store
	crash *atomic.Bool
}

func (c *commitCrash) CommitTerminal(id string, from, to apitypes.OperationStatus, result json.RawMessage, perr *problem.Error) (store.OpRecord, error) {
	if from == apitypes.StatusRunning && c.crash.CompareAndSwap(true, false) {
		return store.OpRecord{}, errors.New("simulated crash before the commit")
	}
	return c.Store.CommitTerminal(id, from, to, result, perr)
}

// TC-E14: Finalize runs again after a crash between Finalize and the
// commit, with the same result, and the record ends in one terminal state.
func TestRecoveryFinalizeTwiceIsHarmless(t *testing.T) {
	perBackend(t, testRecoveryFinalizeTwiceIsHarmless)
}

func testRecoveryFinalizeTwiceIsHarmless(t *testing.T, backend detachtest.Backend) {
	crash := &atomic.Bool{}
	crash.Store(true)
	useBackend := withBackend(t, backend, nil, func(d *detach.Options) {
		d.ListUnits = func(context.Context) ([]string, error) { return nil, nil }
	})
	w := newWorld(t, func(o *ops.Options) {
		useBackend(o)
		o.Store = &commitCrash{Store: o.Store, crash: crash}
	})
	id := w.seed(tdRec("twice", store.Lane{}), apitypes.StatusRunning)
	ended := time.Now().UTC().Truncate(time.Second)
	w.writeExit(id, detach.Exit{ExitCode: 0, Started: ended.Add(-time.Second), Ended: ended})

	waitClosed(t, w.recoverNow(), 15*time.Second, "first classification")
	if rec, err := w.store.Get(id); err != nil || rec.Status != apitypes.StatusRunning || crash.Load() {
		t.Fatalf("after the crashed commit: %+v %v (crash pending %v)", rec, err, crash.Load())
	}

	w.restart()
	waitClosed(t, w.recoverNow(), 15*time.Second, "second classification")
	first := w.waitStatus(id, apitypes.StatusSucceeded)

	finals := w.td.finalCalls()
	if len(finals) != 2 || finals[0] != finals[1] {
		t.Fatalf("Finalize calls %+v, want the same exit twice", finals)
	}
	// Another restart finds nothing to recover, and reads stay the same.
	w.restart()
	waitClosed(t, w.recoverNow(), 15*time.Second, "third classification")
	again, err := w.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.td.finalCalls()) != 2 || again.Status != apitypes.StatusSucceeded ||
		string(again.Result) != string(first.Result) || !again.Ended.Equal(*first.Ended) {
		t.Fatalf("later read %+v, first %+v", again, first)
	}
	if n := len(filterRecs(w.records(), func(r systemspec.AuditRecord) bool {
		return r.OperationID == id && r.Outcome == string(apitypes.StatusSucceeded)
	})); n < 1 {
		t.Fatalf("no Succeeded audit record for %s", id)
	}
}

// TC-E15: a nexus-op-* unit with no record is logged by name and never
// signaled or stopped.
func TestRecoveryOrphanUnitsAreOnlyLogged(t *testing.T) {
	perBackend(t, testRecoveryOrphanUnitsAreOnlyLogged)
}

func testRecoveryOrphanUnitsAreOnlyLogged(t *testing.T, _ detachtest.Backend) {
	var mu sync.Mutex
	var signals []string
	w := newWorld(t, withDetach(t, detach.Options{
		Launch: func(context.Context, detach.Invocation) error { return errors.New("no launch in this test") },
		UnitState: func(context.Context, string) (detach.UnitState, error) {
			return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
		},
		Signal: func(_ context.Context, unit string, _ syscall.Signal) error {
			mu.Lock()
			defer mu.Unlock()
			signals = append(signals, unit)
			return nil
		},
		ListUnits: func(context.Context) ([]string, error) {
			return []string{"nexus-op-x.service"}, nil
		},
	}))
	waitClosed(t, w.recoverNow(), 15*time.Second, "classification")
	var warned bool
	for _, line := range strings.Split(w.logs.String(), "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, `"unit":"nexus-op-x.service"`) {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning names nexus-op-x.service:\n%s", w.logs.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(signals) != 0 {
		t.Fatalf("signals sent: %q", signals)
	}
}

// TC-ERR21: a UnitState that never answers, like a hung systemctl, leaves
// the record unclassified until the deadline, which ends it
// outcome-unknown. The default deadline is 30 seconds, under FR-6's 60.
func TestRecoveryBlockedClassificationHitsDeadline(t *testing.T) {
	perBackend(t, testRecoveryBlockedClassificationHitsDeadline)
}

func testRecoveryBlockedClassificationHitsDeadline(t *testing.T, _ detachtest.Backend) {
	if ops.RecoveryDeadline != 30*time.Second || ops.RecoveryDeadline >= 60*time.Second {
		t.Fatalf("RecoveryDeadline = %v, want 30s", ops.RecoveryDeadline)
	}
	hang := make(chan struct{})
	defer close(hang)
	const deadline = 1500 * time.Millisecond
	w := newWorld(t, func(o *ops.Options) {
		o.RecoveryDeadline = deadline
		withDetach(t, detach.Options{
			Launch: func(context.Context, detach.Invocation) error { return errors.New("no launch in this test") },
			UnitState: func(context.Context, string) (detach.UnitState, error) {
				<-hang // Ignores its context, as a wedged call can
				return detach.UnitState{}, errors.New("released")
			},
			Signal:    func(context.Context, string, syscall.Signal) error { return errors.New("no signal in this test") },
			ListUnits: func(context.Context) ([]string, error) { return nil, nil },
		})(o)
	})
	id := w.seed(tdRec("hung", store.Lane{}), apitypes.StatusRunning)
	t0 := time.Now()
	waitClosed(t, w.recoverNow(), 10*time.Second, "classification")
	took := time.Since(t0)
	rec, err := w.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	wantOutcomeUnknown(t, rec, detach.Unit(id))
	if took < deadline || took > deadline+2*time.Second {
		t.Fatalf("classification ended after %v, want at the %v deadline", took, deadline)
	}
}
