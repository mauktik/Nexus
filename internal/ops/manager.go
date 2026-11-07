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
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
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

	base context.Context // Canceled by Shutdown
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu     sync.Mutex
	closed bool
	lanes  map[string]*lane
	live   map[string]*entry // Admitted in this process and not yet finished
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
		lanes: map[string]*lane{}, live: map[string]*entry{},
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
	Run       func(ctx context.Context, r *provider.Run) (any, error)
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
	res, err := m.store.CreateWithIdempotency(rec, idem)
	if err != nil || res.Existing {
		return res, err
	}
	rec.ID = res.ID
	rec.Status = apitypes.StatusQueued
	e := &entry{id: res.ID, rec: rec, work: w, lane: a.lane, done: make(chan struct{})}
	e.ctx, e.cancel = context.WithCancel(m.base)

	// The Queued record precedes the Running record the worker writes, so
	// it is written before the operation can start.
	m.stateChange(e, apitypes.StatusQueued, nil)

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

// Shutdown stops starting operations, cancels the context of every running
// one, and waits for them until ctx ends. Queued operations stay Queued in
// the store for restart recovery.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.stop()
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
