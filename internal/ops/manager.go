// Package ops is the operation manager of nexusd: operation records, lanes,
// the in-process runner, cancel, and the system.operations.* endpoints
// (FR-4, FR-5, FR-7). The Dispatcher (internal/core) drives admission in the
// Data Flow order and calls into the manager for each step; the manager
// owns everything after creation.
//
// Sensitive input values live only on the in-memory queue entry: the record
// holds the redacted input.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// RetryAfterSeconds is the Retry-After of a 202 response (A-54).
const RetryAfterSeconds = 1

// LocationPrefix is the path of an operation; a 202 response's Location is
// LocationPrefix + id.
const LocationPrefix = "/v1/operations/"

// Store is what the manager needs of internal/store. *store.Store
// implements it; tests wrap it to observe the order of commits.
type Store interface {
	Get(id string) (store.OpRecord, error)
	LookupIdempotency(tokenID, key string) (store.IdempotencyEntry, bool, error)
	CreateWithIdempotency(rec store.OpRecord, idem store.IdempotencyKey) (store.CreateResult, error)
	CompareAndSetStatus(id string, from, to apitypes.OperationStatus) (store.OpRecord, error)
	CommitTerminal(id string, from, to apitypes.OperationStatus, result json.RawMessage, perr *problem.Error) (store.OpRecord, error)
	List(q store.ListQuery) (store.Page, error)
	ListNonTerminal() ([]store.OpRecord, error)
}

// Auditor writes one audit record durably. *audit.Log implements it.
type Auditor interface {
	Write(rec systemspec.AuditRecord, sensitive []string) (string, error)
}

// Options configures New. There is no second constructor.
type Options struct {
	Store Store // Required
	// Audit receives the state-change records. Nil means none: each state
	// change is then only logged.
	Audit Auditor
	// Dir is the ops directory; an operation's output lives in
	// Dir/<id>/output. Required.
	Dir            string
	Host           string // Reported in every operation
	OutputCapBytes int64  // Zero means oplog.DefaultCapBytes
	// PollInterval is how often a log follower of an operation run by
	// another process checks for its end. Zero means
	// oplog.DefaultPollInterval.
	PollInterval time.Duration
	Logger       *slog.Logger     // Nil means slog.Default()
	Now          func() time.Time // Output frame timestamps; nil means time.Now
	// Detach launches, signals and watches detached operations. Nil fails
	// every detached operation with an internal error.
	Detach *detach.Launcher
	// RecoveryDeadline bounds restart classification. Zero means
	// RecoveryDeadline.
	RecoveryDeadline time.Duration
}

// Manager runs long-running operations. It is safe for concurrent use and
// runs nothing between operations (NFR-1): a goroutine exists only while an
// operation runs.
type Manager struct {
	store    Store
	audit    Auditor
	dir      string
	host     string
	capBytes int64
	poll     time.Duration
	log      *slog.Logger
	now      func() time.Time
	detach   *detach.Launcher

	// base is the context of in-process operations. Shutdown cancels it
	// only when its grace period ends.
	base context.Context
	stop context.CancelFunc
	// halt is the context of detached work in this process: Prepare, the
	// wait for the watcher, and restart recovery. Shutdown cancels it at
	// once; the units themselves are never touched.
	halt     context.Context
	haltStop context.CancelFunc
	wg       sync.WaitGroup

	deadline time.Duration // Of restart classification
	calls    chan struct{} // Restart recovery's UnitState calls in flight

	mu         sync.Mutex
	closed     bool
	lanes      map[string]*lane
	live       map[string]*entry // Admitted in this process, or restored, and not yet finished
	restored   bool              // Restore ran
	recovering []*entry          // Restored, for Recover
}

// New returns a manager over opts.Store.
func New(opts Options) (*Manager, error) {
	if opts.Store == nil {
		return nil, errors.New("ops: no store")
	}
	if opts.Dir == "" {
		return nil, errors.New("ops: no ops directory")
	}
	m := &Manager{
		store: opts.Store, audit: opts.Audit, dir: opts.Dir, host: opts.Host,
		capBytes: opts.OutputCapBytes, poll: opts.PollInterval, log: opts.Logger, now: opts.Now,
		detach: opts.Detach, deadline: opts.RecoveryDeadline, calls: make(chan struct{}, recoveryCalls),
		lanes: map[string]*lane{}, live: map[string]*entry{},
	}
	if m.deadline <= 0 {
		m.deadline = RecoveryDeadline
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.poll <= 0 {
		m.poll = oplog.DefaultPollInterval
	}
	m.base, m.stop = context.WithCancel(context.Background())
	m.halt, m.haltStop = context.WithCancel(context.Background())
	return m, nil
}

// Host is the host name every operation reports.
func (m *Manager) Host() string { return m.host }

// Work is what an operation runs once it holds its lane slot. It is kept in
// memory only: Run closes over the decoded input, sensitive values
// included, which never reach the record.
type Work struct {
	Op      def.Op
	TokenID string // Of the requesting token, for the state-change records
	// RequestID is the id of the request's requested audit record, which
	// the operation's Queued record names. Empty for a read-risk request.
	RequestID string
	// Run is an in-process operation; Detached a detached one. Exactly one
	// is set.
	Run      func(ctx context.Context, r *provider.Run) (any, error)
	Detached *DetachedWork
}

// DetachedWork is a detached operation's provider functions and the input
// Prepare runs on.
type DetachedWork struct {
	// In is the decoded request input, sensitive values included. It is
	// dropped once spec.json is written.
	In any
	// Decode decodes the record's redacted input, which Finalize and
	// LostDetail run on, as they do after a restart.
	Decode func(json.RawMessage) (any, error)
	Funcs  provider.DetachedFuncs
}

// entry is an admitted operation in memory.
type entry struct {
	id   string
	rec  store.OpRecord // As created
	work Work
	lane *lane // Nil for NoLane

	// ctx is canceled by a cancel of the queued operation (which a
	// Prepare in progress sees), by a cancel of a running CancelRunning
	// operation, and by Shutdown.
	ctx    context.Context
	cancel context.CancelFunc

	done     chan struct{} // Closed once the operation is terminal in the store
	doneOnce sync.Once

	userCanceled bool // Set under Manager.mu by a cancel of the running operation
	launched     bool // Set under Manager.mu once a detached operation's unit exists
}

func (e *entry) markDone() { e.doneOnce.Do(func() { close(e.done) }) }

// LookupIdempotency returns the stored, unexpired entry for a token's key
// (Data Flow steps 4 and 8).
func (m *Manager) LookupIdempotency(tokenID, key string) (store.IdempotencyEntry, bool, error) {
	return m.store.LookupIdempotency(tokenID, key)
}

// Get returns the record of id; the error wraps store.ErrNotFound when
// there is none.
func (m *Manager) Get(id string) (store.OpRecord, error) { return m.store.Get(id) }

// Create is Data Flow step 10 and the registration that follows it: it
// creates the record as Queued together with the idempotency key in one
// transaction, writes the Queued record naming the request's requested
// record, registers the operation on the admission's lane and starts it when
// a slot is free. When the key is already stored, CreateWithIdempotency is
// the final arbiter: nothing is created or registered, and the result says
// Existing so the caller answers as a replay or a reuse.
func (a *Admission) Create(rec store.OpRecord, idem store.IdempotencyKey, w Work) (store.CreateResult, error) {
	if a.released {
		return store.CreateResult{}, errReleased
	}
	m := a.m
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return store.CreateResult{}, errors.New("ops: the manager is shut down")
	}
	rec.Lane = a.Lane()
	if w.Detached != nil {
		rec.Exec = store.ExecDetached
	}
	res, err := m.store.CreateWithIdempotency(rec, idem)
	if err != nil || res.Existing {
		return res, err
	}
	rec.ID = res.ID
	rec.Status = apitypes.StatusQueued
	e := &entry{id: res.ID, rec: rec, work: w, lane: a.lane, done: make(chan struct{})}
	parent := m.base
	if w.Detached != nil {
		parent = m.halt
	}
	e.ctx, e.cancel = context.WithCancel(parent)

	// The Queued record precedes the Running record the worker writes, so
	// it is written before the operation can start.
	m.stateChange(e, apitypes.StatusQueued, nil)

	// A holder already terminal in the store frees its slot before e is
	// placed, so on a LaneQueue lane e starts at once rather than after
	// that holder's finish.
	if a.lane != nil {
		m.retireTerminal(a.lane)
	}
	m.mu.Lock()
	m.live[e.id] = e
	if m.closed {
		// Shut down meanwhile: the record stays Queued for recovery.
		m.mu.Unlock()
		return res, nil
	}
	m.enqueue(e)
	m.mu.Unlock()
	return res, nil
}

// start runs e in its own goroutine; m.mu is held and m is not closed.
func (m *Manager) start(e *entry) {
	if m.closed {
		return
	}
	m.wg.Add(1)
	if e.work.Detached != nil {
		go m.runDetached(e)
		return
	}
	go m.run(e)
}

// finish forgets e once its worker is done, and frees its slot.
func (m *Manager) finish(e *entry) {
	e.markDone()
	e.cancel()
	m.mu.Lock()
	delete(m.live, e.id)
	m.releaseSlot(e)
	m.mu.Unlock()
}

// stateChange writes the audit record of an operation state change. A
// failed write cannot undo the change: it is logged, and the audit log
// marks itself unhealthy, so the next mutation proceeds only if its own
// requested record is written (FR-24).
func (m *Manager) stateChange(e *entry, to apitypes.OperationStatus, perr *problem.Error) {
	rec := systemspec.AuditRecord{
		TokenName: e.rec.TokenName, TokenID: e.work.TokenID,
		Provider: e.rec.Provider, Action: e.rec.Action, Target: e.rec.Target,
		Outcome: string(to), OperationID: e.id,
	}
	if to == apitypes.StatusQueued {
		rec.RequestID = e.work.RequestID
	}
	if perr != nil {
		rec.Problem = string(perr.Slug())
	}
	if m.audit == nil {
		m.log.Error("audit write failed", "operationId", e.id, "outcome", rec.Outcome, "error", "no audit log")
		return
	}
	if _, err := m.audit.Write(rec, nil); err != nil {
		m.log.Error("audit write failed", "operationId", e.id, "outcome", rec.Outcome, "error", err.Error())
	}
}

// outputDir is where operation id's output log lives.
func (m *Manager) outputDir(id string) string { return filepath.Join(m.dir, id, "output") }

// Shutdown stops starting operations and gives in-process operations until
// ctx ends to finish (FR-37); only then is their context canceled, and one
// still running is outcome-unknown after the restart. Detached work stops
// at once and leaves its units alone: a Prepare in progress is canceled,
// the record of a launched operation stays Running for restart recovery,
// and no unit is signaled or stopped. Queued operations stay Queued in the
// store for restart recovery.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.haltStop()
	defer m.stop()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ops: operations still running at shutdown: %w", ctx.Err())
	}
}

func notFound() *problem.Error { return problem.New(problem.NotFound, "the operation does not exist") }

// getAPI loads id as its wire form.
func (m *Manager) getAPI(id string) (apitypes.Operation, error) {
	rec, err := m.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return apitypes.Operation{}, notFound()
	}
	if err != nil {
		return apitypes.Operation{}, fmt.Errorf("load operation: %w", err)
	}
	return rec.ToAPI(m.host), nil
}

// runDetached is the worker of one dequeued detached operation (Data Flow,
// detached operation lifecycle). Prepare runs while the record is Queued,
// under the context a cancel cancels. The compare-and-set to Running then
// decides between launching and abandoning, so a canceled operation never
// gets a unit; Running is audited, spec.json written and the unit launched,
// in that order. The watcher reports the end, which is audited then
// committed through CommitTerminal. At shutdown the worker leaves the
// record Running and the unit alone, for restart recovery.
func (m *Manager) runDetached(e *entry) {
	defer m.wg.Done()
	defer m.finish(e)
	d := e.work.Detached
	r := provider.NewRun(e.id, nil)

	if m.detach == nil {
		m.log.Error("no launcher for a detached operation", "operationId", e.id, "op", e.rec.Action)
		m.failQueued(e, problem.New(problem.InternalError, genericDetail))
		return
	}
	spec, perr := m.prepare(e, d, r)
	if perr != nil {
		if m.halt.Err() != nil {
			// Shutdown canceled Prepare: the record stays Queued for
			// restart recovery.
			return
		}
		m.failQueued(e, perr)
		return
	}
	rec, err := m.store.CompareAndSetStatus(e.id, apitypes.StatusQueued, apitypes.StatusRunning)
	if err != nil {
		if errors.Is(err, store.ErrStatusConflict) {
			m.log.Debug("operation abandoned: no longer queued", "operationId", e.id)
		} else {
			m.log.Error("cannot start the operation", "operationId", e.id, "error", err.Error())
		}
		return
	}
	m.stateChange(e, apitypes.StatusRunning, nil)

	if spec.OutputCapBytes == 0 {
		// No provider sets a cap; limits.outputCapBytes applies to every
		// operation, detached ones included.
		spec.OutputCapBytes = m.capBytes
	}
	maxRuntime := spec.MaxRuntime
	if werr := m.writeMaxRuntime(e.id, maxRuntime); werr != nil {
		// Without the file a reattached operation would have no MaxRuntime
		// backstop after a restart (A-63), so nothing is launched.
		spec, d.In = detach.Spec{}, nil
		m.log.Error("cannot record the operation's maximum runtime", "operationId", e.id, "error", werr.Error())
		m.commitEnd(e, apitypes.StatusFailed, nil,
			problem.New(problem.InternalError, "the operation could not be launched"))
		return
	}
	lerr := m.detach.Start(context.WithoutCancel(m.base), e.id, spec)
	// spec.json holds the sensitive values from here on; memory does not.
	spec, d.In = detach.Spec{}, nil
	if lerr != nil {
		m.log.Error("cannot launch the operation", "operationId", e.id, "error", lerr.Error())
		m.commitEnd(e, apitypes.StatusFailed, nil,
			problem.New(problem.InternalError, "the operation could not be launched"))
		return
	}
	m.mu.Lock()
	e.launched = true
	canceled := e.userCanceled
	m.mu.Unlock()
	if canceled {
		// Canceled between the compare-and-set and the launch; a failure
		// is logged, and the cancel can be repeated.
		if err := m.signalCancel(e.id); err != nil {
			m.log.Error("cannot signal the operation's unit", "operationId", e.id, "unit", detach.Unit(e.id), "error", err.Error())
		}
	}

	started := m.now()
	if rec.Started != nil {
		started = *rec.Started
	}
	select {
	case o := <-m.detach.Watch(m.halt, e.id, started, maxRuntime):
		to, result, perr := m.detachedEnd(e, d, r, o)
		m.commitEnd(e, to, result, perr)
	case <-m.halt.Done():
	}
}

// prepare runs Prepare with panic containment. A failure is the
// operation's problem.
func (m *Manager) prepare(e *entry, d *DetachedWork, r *provider.Run) (spec detach.Spec, perr *problem.Error) {
	defer func() {
		if v := recover(); v != nil {
			m.log.Error("operation panic", "operationId", e.id, "op", e.rec.Action,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			perr = problem.New(problem.InternalError, genericDetail)
		}
	}()
	spec, err := d.Funcs.Prepare(e.ctx, d.In, r)
	if err != nil {
		return detach.Spec{}, m.problemOf(e, "prepare", err)
	}
	return spec, nil
}

// problemOf is the structured problem of a provider error: a
// *problem.Error as returned, anything else a generic internal-error whose
// cause goes only to the log.
func (m *Manager) problemOf(e *entry, stage string, err error) *problem.Error {
	var p *problem.Error
	if errors.As(err, &p) {
		return p
	}
	m.log.Error("operation failed", "operationId", e.id, "op", e.rec.Action, "stage", stage, "error", err.Error())
	return problem.New(problem.InternalError, genericDetail)
}

// failQueued fails an operation that never left Queued. The commit comes
// first, because a cancel may have moved the record to Canceled meanwhile:
// then nothing more happens.
func (m *Manager) failQueued(e *entry, perr *problem.Error) {
	_, err := m.store.CommitTerminal(e.id, apitypes.StatusQueued, apitypes.StatusFailed, nil, perr)
	if errors.Is(err, store.ErrStatusConflict) {
		m.log.Debug("operation abandoned: no longer queued", "operationId", e.id)
		return
	}
	if err != nil {
		m.log.Error("cannot commit the operation's end", "operationId", e.id, "error", err.Error())
		return
	}
	m.stateChange(e, apitypes.StatusFailed, perr)
}

// commitEnd audits then commits the end of a Running operation. A record
// no longer Running was already finalized, which is not an error.
func (m *Manager) commitEnd(e *entry, to apitypes.OperationStatus, result json.RawMessage, perr *problem.Error) {
	m.stateChange(e, to, perr)
	_, err := m.store.CommitTerminal(e.id, apitypes.StatusRunning, to, result, perr)
	if errors.Is(err, store.ErrStatusConflict) {
		m.log.Debug("operation already finalized", "operationId", e.id)
		return
	}
	if err != nil {
		m.log.Error("cannot commit the operation's end", "operationId", e.id, "status", string(to), "error", err.Error())
	}
}

// detachedEnd maps the watcher's outcome to the terminal status: exit.json
// with canceled set is Canceled; any other exit.json goes through
// Finalize; a lost unit or a passed MaxRuntime is outcome-unknown naming
// the unit.
func (m *Manager) detachedEnd(e *entry, d *DetachedWork, r *provider.Run, o detach.Outcome) (
	to apitypes.OperationStatus, result json.RawMessage, perr *problem.Error) {
	m.dropSpec(e.id)
	unit := detach.Unit(e.id)
	switch {
	case o.Exit != nil && o.Exit.Canceled:
		return apitypes.StatusCanceled, nil, nil
	case o.Overran:
		return apitypes.StatusFailed, nil, problem.New(problem.OutcomeUnknown,
			fmt.Sprintf("the operation passed its maximum runtime; unit %s was not stopped and may still be running", unit))
	}
	defer func() {
		if v := recover(); v != nil {
			m.log.Error("operation panic", "operationId", e.id, "op", e.rec.Action,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			to, result, perr = apitypes.StatusFailed, nil, problem.New(problem.InternalError, genericDetail)
		}
	}()
	in, err := d.Decode(e.rec.Input)
	if err != nil {
		m.log.Error("cannot decode the operation's input", "operationId", e.id, "error", err.Error())
		in = nil
	}
	if o.Lost {
		detail := fmt.Sprintf("unit %s ended without reporting an outcome", unit)
		if in != nil {
			if extra := d.Funcs.LostDetail(r, in); extra != "" {
				detail += "; " + extra
			}
		}
		return apitypes.StatusFailed, nil, problem.New(problem.OutcomeUnknown, detail)
	}
	if in == nil {
		return apitypes.StatusFailed, nil, problem.New(problem.InternalError, genericDetail)
	}
	out, err := d.Funcs.Finalize(m.base, in, *o.Exit, r)
	if err != nil {
		return apitypes.StatusFailed, nil, m.problemOf(e, "finalize", err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		m.log.Error("cannot encode the operation's result", "operationId", e.id, "error", err.Error())
		return apitypes.StatusFailed, nil, problem.New(problem.InternalError, genericDetail)
	}
	return apitypes.StatusSucceeded, b, nil
}

// signalTimeout bounds one cancel signal through the unit.
const signalTimeout = 10 * time.Second

// cancelDetached cancels a running detached operation by signaling its
// unit; it ends Canceled when exit.json reports canceled. A cancel that
// lands between the compare-and-set to Running and the launch is sent by
// the worker once the unit exists. A record this process does not run,
// such as one reattached after a restart, is signaled directly.
func (m *Manager) cancelDetached(rec store.OpRecord) error {
	m.mu.Lock()
	if e, ok := m.live[rec.ID]; ok {
		e.userCanceled = true
		if !e.launched {
			m.mu.Unlock()
			return nil
		}
	}
	m.mu.Unlock()
	if err := m.signalCancel(rec.ID); err != nil {
		// The cause stays in the log; the caller gets the problem.
		m.log.Error("cannot signal the operation's unit", "operationId", rec.ID, "unit", detach.Unit(rec.ID), "error", err.Error())
		return problem.New(problem.InternalError, "the cancel signal could not be delivered to the operation")
	}
	return nil
}

// signalCancel sends the cancel signal to operation id's unit.
func (m *Manager) signalCancel(id string) error {
	if m.detach == nil {
		return errors.New("no launcher")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.base), signalTimeout)
	defer cancel()
	return m.detach.Cancel(ctx, id)
}
