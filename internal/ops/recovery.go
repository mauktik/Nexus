package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
)

// RecoveryDeadline bounds restart classification: deciding which row of the
// Restart recovery table applies to each Running record. It leaves margin
// under FR-6's 60 seconds. Finalize is not under it: it reads files and
// never waits.
const RecoveryDeadline = 30 * time.Second

// Restart recovery timing.
const (
	// recoveryRetry is the pause after a UnitState error before the next
	// try; a timed-out call has already taken detach.StateTimeout.
	recoveryRetry = time.Second
	// recoveryCalls bounds the systemctl calls classification makes at
	// once, so a host with many reattaching operations sees no fork storm.
	recoveryCalls = 8
)

// maxRuntimeFile holds a detached operation's MaxRuntime in its directory,
// so a reattached operation keeps its backstop (A-63): spec.json, which
// carries it to the supervisor, is gone once the supervisor started.
const maxRuntimeFile = "max-runtime"

// Restore is the synchronous stage of restart recovery, run before the
// listener opens. It reads the non-terminal records and rebuilds lane
// occupancy and queue order from each record's stored Lane: a Running
// record holds its slot, and Queued records wait behind it in ID order, so
// from the first request a LaneReject lane held by a reattaching operation
// answers 409 and a LaneQueue lane runs new requests after the restored
// ones (FR-7, FR-17). It makes no systemctl or provider calls and starts
// nothing; Recover does the rest. binding finds an op's binding by ID,
// which re-enqueued operations run on and Finalize comes from.
func (m *Manager) Restore(binding func(opID string) (provider.Binding, bool)) error {
	recs, err := m.store.ListNonTerminal()
	if err != nil {
		return fmt.Errorf("ops: read the operations to recover: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restored {
		return errors.New("ops: operations already restored")
	}
	m.restored = true
	// Running records first: they hold their slots ahead of every queued one.
	for _, rec := range recs {
		if rec.Status != apitypes.StatusRunning {
			continue
		}
		e := m.restoredEntry(rec, binding)
		e.launched = rec.Exec == store.ExecDetached
		if e.lane = m.restoredLane(rec.Lane); e.lane != nil {
			e.lane.active = append(e.lane.active, e)
		}
		m.recovering = append(m.recovering, e)
	}
	for _, rec := range recs {
		if rec.Status != apitypes.StatusQueued {
			continue
		}
		e := m.restoredEntry(rec, binding)
		// A record with a sensitive value never runs again, and one that
		// cannot be rebuilt fails: neither waits on a lane.
		if !rec.HasSensitive && e.runnable() {
			if e.lane = m.restoredLane(rec.Lane); e.lane != nil {
				e.lane.queue = append(e.lane.queue, e)
			}
		}
		m.recovering = append(m.recovering, e)
	}
	return nil
}

// restoredEntry is the in-memory entry of a record found at startup; m.mu
// is held. Its Work is rebuilt from the binding and the record's redacted
// input, which is the whole input when no sensitive value was supplied.
func (m *Manager) restoredEntry(rec store.OpRecord, binding func(string) (provider.Binding, bool)) *entry {
	e := &entry{id: rec.ID, rec: rec, done: make(chan struct{})}
	parent := m.base
	if rec.Exec == store.ExecDetached {
		parent = m.halt
	}
	e.ctx, e.cancel = context.WithCancel(parent)
	e.work = m.restoredWork(rec, binding)
	m.live[e.id] = e
	return e
}

func (m *Manager) restoredWork(rec store.OpRecord, binding func(string) (provider.Binding, bool)) Work {
	var b provider.Binding
	ok := binding != nil
	if ok {
		b, ok = binding(rec.Action)
	}
	if !ok {
		m.log.Error("no binding for a recovered operation", "operationId", rec.ID, "op", rec.Action)
		return Work{}
	}
	w := Work{Op: b.Op()}
	if funcs, ok := b.Detached(); ok {
		w.Detached = &DetachedWork{Decode: b.Decode, Funcs: funcs}
	}
	if rec.Status != apitypes.StatusQueued || rec.HasSensitive {
		if w.Detached == nil {
			// A Running in-process record is only failed; nothing runs.
			if _, ok := b.InProcess(); !ok {
				m.log.Error("a recovered operation's binding is not long-running", "operationId", rec.ID, "op", rec.Action)
			}
		}
		return w
	}
	in, err := b.Decode(rec.Input)
	if err != nil {
		m.log.Error("cannot decode a recovered operation's input", "operationId", rec.ID, "error", err.Error())
		return Work{}
	}
	if w.Detached != nil {
		w.Detached.In = in
		return w
	}
	if funcs, ok := b.InProcess(); ok {
		w.Run = func(ctx context.Context, r *provider.Run) (any, error) { return funcs.Run(ctx, in, r) }
		return w
	}
	m.log.Error("a recovered operation's binding is not long-running", "operationId", rec.ID, "op", rec.Action)
	return Work{}
}

// runnable reports whether a Queued entry's Work can run.
func (e *entry) runnable() bool {
	return e.work.Run != nil || (e.work.Detached != nil && e.work.Detached.In != nil)
}

// restoredLane is the lane a stored Lane names, opened as Admit would open
// it; m.mu is held. The first record on a lane sets its policy and limit.
func (m *Manager) restoredLane(sl store.Lane) *lane {
	if sl.Key == "" {
		return nil
	}
	if l, ok := m.lanes[sl.Key]; ok {
		return l
	}
	policy := provider.LanePolicy(sl.Policy)
	if policy == "" {
		policy = provider.LaneQueue
	}
	l := &lane{key: sl.Key, policy: policy, limit: max(sl.Limit, 1)}
	m.lanes[sl.Key] = l
	return l
}

// Recover is the asynchronous stage of restart recovery, started once the
// listener is ready. It starts the re-enqueued operations and classifies
// every other record Restore found, per the Restart recovery table:
//
//   - Queued with a sensitive value: Failed, outcome-unknown, "never
//     started", because the value was never persisted (A-66).
//   - Running in process: Failed, outcome-unknown; there is no in-process
//     recovery (D-8).
//   - Running detached: exit.json goes to Finalize; an active unit is
//     reattached to the watcher; an inactive or absent unit is re-checked
//     after detach.RecheckDelay and then ends outcome-unknown with the
//     provider's LostDetail. A UnitState error or timeout is unknown, never
//     absent: it is retried until the deadline, which ends the record
//     outcome-unknown.
//
// Every outcome commits through CommitTerminal; ErrStatusConflict means the
// record was already finalized. Operation units with no record are logged
// and left alone. The returned channel is closed once classification is
// over: reattached operations are then the watcher's.
func (m *Manager) Recover() <-chan struct{} {
	done := make(chan struct{})
	m.mu.Lock()
	pending := m.recovering
	m.recovering = nil
	if m.closed {
		m.mu.Unlock()
		close(done)
		return done
	}
	m.wg.Add(1)
	for _, e := range pending {
		if e.rec.Status == apitypes.StatusQueued && !e.rec.HasSensitive && e.runnable() {
			if e.lane == nil {
				m.start(e)
			} else {
				m.pump(e.lane)
			}
		}
	}
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		defer close(done)
		ctx, cancel := context.WithTimeout(m.halt, m.deadline)
		defer cancel()
		var wg sync.WaitGroup
		known := make(map[string]bool, len(pending))
		for _, e := range pending {
			known[e.id] = true
			switch {
			case e.rec.Status == apitypes.StatusQueued && e.rec.HasSensitive:
				m.failQueued(e, problem.New(problem.OutcomeUnknown,
					"never started: its sensitive input was not persisted across the nexusd restart"))
				m.finish(e)
			case e.rec.Status == apitypes.StatusQueued && !e.runnable():
				m.failQueued(e, problem.New(problem.InternalError, "the operation could not be restarted after nexusd restarted"))
				m.finish(e)
			case e.rec.Status == apitypes.StatusQueued:
				// Re-enqueued above.
			case e.rec.Exec != store.ExecDetached:
				m.commitEnd(e, apitypes.StatusFailed, nil, problem.New(problem.OutcomeUnknown,
					"the operation was running in nexusd when it stopped; in-process operations are not recovered"))
				m.finish(e)
			default:
				wg.Go(func() { m.classify(ctx, e) })
			}
		}
		if m.detach != nil {
			wg.Go(func() { m.orphans(ctx, known) })
		}
		wg.Wait()
	}()
	return done
}

// classify decides the row of a Running detached record, then finalizes,
// fails or reattaches it.
func (m *Manager) classify(ctx context.Context, e *entry) {
	unit := detach.Unit(e.id)
	if m.detach == nil {
		m.log.Error("no launcher to recover a detached operation", "operationId", e.id)
		m.endRecovered(e, detach.Outcome{Lost: true})
		return
	}
	recheck := false
	for {
		if x, ok := m.readExit(e); ok {
			m.endRecovered(e, detach.Outcome{Exit: &x})
			return
		}
		lv, err := m.liveness(ctx, e.id)
		if ctx.Err() != nil {
			break
		}
		switch {
		case err != nil:
			reason := "error"
			if errors.Is(err, context.DeadlineExceeded) {
				reason = "timeout"
			}
			m.log.Warn("unit state unknown; recovery retries", "operationId", e.id, "unit", unit,
				"reason", reason, "error", err.Error())
			recheck = false
			pause(ctx, recoveryRetry)
		case lv == detach.Live:
			m.reattach(e)
			return
		case !recheck:
			recheck = true
			pause(ctx, detach.RecheckDelay)
		default:
			// Confirmed gone; exit.json may have landed since the last read.
			if x, ok := m.readExit(e); ok {
				m.endRecovered(e, detach.Outcome{Exit: &x})
				return
			}
			m.endRecovered(e, detach.Outcome{Lost: true})
			return
		}
		if ctx.Err() != nil {
			break
		}
	}
	if m.halt.Err() != nil {
		// Shutdown: the record stays Running for the next start.
		m.finish(e)
		return
	}
	if x, ok := m.readExit(e); ok {
		m.endRecovered(e, detach.Outcome{Exit: &x})
		return
	}
	m.log.Warn("operation not classified by the recovery deadline", "operationId", e.id, "unit", unit,
		"deadline", m.deadline.String())
	m.commitEnd(e, apitypes.StatusFailed, nil, problem.New(problem.OutcomeUnknown,
		fmt.Sprintf("the state of unit %s could not be determined within %v after nexusd restarted; it was not stopped and may still be running",
			unit, m.deadline)))
	m.finish(e)
}

// pause waits d or until ctx ends.
func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// readExit reads e's exit.json without waiting.
func (m *Manager) readExit(e *entry) (detach.Exit, bool) {
	x, ok, err := detach.ReadExit(m.detach.Dir(e.id))
	if err != nil {
		m.log.Error("cannot read the operation's exit.json", "operationId", e.id, "error", err.Error())
		return detach.Exit{}, false
	}
	return x, ok
}

// liveness asks for operation id's unit state, with at most recoveryCalls
// calls at once and detach.StateTimeout per call. A call that ignores its
// context is abandoned, not waited on.
func (m *Manager) liveness(ctx context.Context, id string) (detach.Liveness, error) {
	select {
	case m.calls <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-m.calls }()
	type answer struct {
		lv  detach.Liveness
		err error
	}
	c := make(chan answer, 1)
	go func() {
		lv, err := m.detach.State(ctx, id)
		c <- answer{lv, err}
	}()
	t := time.NewTimer(detach.StateTimeout)
	defer t.Stop()
	select {
	case a := <-c:
		return a.lv, a.err
	case <-t.C:
		return 0, context.DeadlineExceeded
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// endRecovered commits a recovered operation's end and frees its slot.
func (m *Manager) endRecovered(e *entry, o detach.Outcome) {
	defer m.finish(e)
	m.commitRecovered(e, o)
}

// commitRecovered commits a recovered detached operation's end through
// Finalize or LostDetail, as for one this process launched. An op that is
// no longer bound has neither, so its end is outcome-unknown.
func (m *Manager) commitRecovered(e *entry, o detach.Outcome) {
	if d := e.work.Detached; d != nil {
		to, result, perr := m.detachedEnd(e, d, provider.NewRun(e.id, nil), o)
		m.commitEnd(e, to, result, perr)
		return
	}
	m.dropSpec(e.id)
	unit := detach.Unit(e.id)
	detail := fmt.Sprintf("unit %s ended without reporting an outcome", unit)
	switch {
	case o.Overran:
		detail = fmt.Sprintf("the operation passed its maximum runtime; unit %s was not stopped and may still be running", unit)
	case o.Exit != nil:
		detail = "the operation ended, but its op is no longer available to report the outcome"
	}
	m.commitEnd(e, apitypes.StatusFailed, nil, problem.New(problem.OutcomeUnknown, detail))
}

// reattach hands a recovered operation whose unit is active to the watcher,
// which reports its end as for one this process launched. Its output
// continues in the same log, at the next offset.
func (m *Manager) reattach(e *entry) {
	m.log.Info("operation reattached", "operationId", e.id, "unit", detach.Unit(e.id))
	started := m.now()
	if e.rec.Started != nil {
		started = *e.rec.Started
	}
	maxRuntime := m.readMaxRuntime(e.id)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.finish(e)
		select {
		case o := <-m.detach.Watch(m.halt, e.id, started, maxRuntime):
			m.commitRecovered(e, o)
		case <-m.halt.Done():
		}
	}()
}

// fallbackMaxRuntime is the backstop of a reattached operation whose
// max-runtime file is missing or unreadable: a watcher with no maximum would
// let a hung unit hold its lane forever (A-63), so a generous fixed ceiling
// stands in. Passing it only reports outcome-unknown; it never signals the
// unit.
const fallbackMaxRuntime = 24 * time.Hour

// writeMaxRuntime records a detached operation's MaxRuntime before its
// launch, through a temporary file, fsync, rename and a directory fsync, so
// a reader sees no file or a complete one. A failure is returned: the caller
// must not launch an operation whose backstop would be lost on restart.
func (m *Manager) writeMaxRuntime(id string, d time.Duration) (err error) {
	dir := m.detach.Dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+maxRuntimeFile+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(tmp))
		}
	}()
	if _, err := f.WriteString(d.String()); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, maxRuntimeFile)); err != nil {
		return err
	}
	df, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(df.Sync(), df.Close())
}

// readMaxRuntime is the MaxRuntime writeMaxRuntime recorded; when it is
// missing or unreadable, fallbackMaxRuntime, never none.
func (m *Manager) readMaxRuntime(id string) time.Duration {
	b, err := os.ReadFile(filepath.Join(m.detach.Dir(id), maxRuntimeFile))
	if err == nil {
		d, perr := time.ParseDuration(strings.TrimSpace(string(b)))
		if perr == nil && d > 0 {
			return d
		}
		err = perr
	}
	m.log.Warn("no maximum runtime recorded for a reattached operation; using the fallback",
		"operationId", id, "fallback", fallbackMaxRuntime.String(), "error", fmt.Sprint(err))
	return fallbackMaxRuntime
}

// dropSpec removes operation id's spec.json, which can hold sensitive
// values and is normally unlinked by the supervisor. It is still there when
// the supervisor died before reading it (A-66), and an operation that has
// ended needs it no more.
func (m *Manager) dropSpec(id string) {
	if m.detach == nil {
		return
	}
	if err := os.Remove(filepath.Join(m.detach.Dir(id), detach.SpecFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Error("cannot remove the operation's spec.json", "operationId", id, "error", err.Error())
	}
}

// orphans logs the operation units that no recovered record owns. They are
// never signaled or stopped: nexusd cannot know what they are doing.
func (m *Manager) orphans(ctx context.Context, known map[string]bool) {
	type answer struct {
		units []string
		err   error
	}
	c := make(chan answer, 1)
	go func() {
		units, err := m.detach.Units(ctx)
		c <- answer{units, err}
	}()
	var a answer
	t := time.NewTimer(detach.StateTimeout)
	defer t.Stop()
	select {
	case a = <-c:
	case <-t.C:
		a.err = context.DeadlineExceeded
	case <-ctx.Done():
		a.err = ctx.Err()
	}
	if a.err != nil {
		m.log.Warn("cannot list operation units; orphans are not reported", "error", a.err.Error())
		return
	}
	for _, unit := range a.units {
		id := strings.TrimSuffix(strings.TrimPrefix(unit, detach.UnitPrefix), ".service")
		if known[id] {
			continue
		}
		rec, err := m.store.Get(id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			m.log.Warn("operation unit has no record; left alone", "unit", unit)
		case err != nil:
			m.log.Warn("cannot check an operation unit's record; left alone", "unit", unit, "error", err.Error())
		default:
			m.log.Info("operation unit outlived its record's end; left alone", "unit", unit,
				"operationId", id, "status", string(rec.Status))
		}
	}
}
