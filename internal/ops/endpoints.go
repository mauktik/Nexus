package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// List is system.operations.list: operations newest first, restricted to
// providers, the providers whose read scope the caller holds (FR-22, A-50).
// An empty providers lists nothing. The schema has already bounded limit
// and the filter keys; the values are checked here.
func (m *Manager) List(_ context.Context, in systemspec.ListInput, providers []string) (apitypes.List[apitypes.Operation], error) {
	var none apitypes.List[apitypes.Operation]
	if providers == nil {
		providers = []string{}
	}
	q := store.ListQuery{Limit: in.Limit, Cursor: in.Cursor, Providers: providers}
	for i, f := range in.Filter {
		key, val, _ := strings.Cut(f, "=")
		path := fmt.Sprintf("/filter/%d", i)
		switch key {
		case "status":
			s := apitypes.OperationStatus(val)
			switch s {
			case apitypes.StatusQueued, apitypes.StatusRunning, apitypes.StatusSucceeded,
				apitypes.StatusFailed, apitypes.StatusCanceled:
				q.Status = s
			default:
				return none, invalid(path, "status must be one of Queued, Running, Succeeded, Failed, Canceled")
			}
		case "provider":
			q.Provider = val
		case "createdAfter", "createdBefore":
			t, err := time.Parse(time.RFC3339Nano, val)
			if err != nil {
				return none, invalid(path, key+" must be an RFC 3339 time")
			}
			if key == "createdAfter" {
				q.CreatedAfter = t
			} else {
				q.CreatedBefore = t
			}
		default:
			return none, invalid(path, "unknown filter key")
		}
	}
	page, err := m.store.List(q)
	if errors.Is(err, store.ErrInvalidCursor) {
		return none, invalid("/cursor", "is not a cursor this server issued")
	}
	if err != nil {
		return none, fmt.Errorf("list operations: %w", err)
	}
	items := make([]apitypes.Operation, len(page.Records))
	for i, r := range page.Records {
		items[i] = r.ToAPI(m.host)
	}
	return apitypes.List[apitypes.Operation]{Items: items, NextCursor: page.NextCursor}, nil
}

func invalid(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}

// GetOperation is system.operations.get. The Dispatcher has already loaded
// the record to authorize the call.
func (m *Manager) GetOperation(_ context.Context, in systemspec.OperationInput) (apitypes.Operation, error) {
	return m.getAPI(in.ID)
}

// Logs is system.operations.logs: the operation's output frames from
// in.Offset, as oplog frames them. Without follow it emits what is there
// now; with follow it keeps emitting until the operation is terminal and
// its output complete, or ctx ends. An offset before the oldest retained
// byte first emits the truncation marker.
func (m *Manager) Logs(ctx context.Context, in systemspec.OperationLogsInput, emit func(apitypes.Frame) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	terminal := closedChan()
	if in.Follow {
		t, err := m.terminal(ctx, in.ID)
		if err != nil {
			return err
		}
		terminal = t
	}
	r := oplog.NewReader(m.outputDir(in.ID))
	r.Poll = m.poll
	_, err := r.Follow(ctx, in.Offset, terminal, emit)
	if err != nil && ctx.Err() != nil {
		// The follower went away; nothing to report.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read operation output: %w", err)
	}
	return nil
}

func closedChan() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// terminal returns a channel closed once operation id is terminal with its
// output complete. For an operation this process runs, that is its done
// channel; otherwise the record is polled until ctx ends.
func (m *Manager) terminal(ctx context.Context, id string) (<-chan struct{}, error) {
	m.mu.Lock()
	e, ok := m.live[id]
	m.mu.Unlock()
	if ok {
		return e.done, nil
	}
	rec, err := m.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, fmt.Errorf("load operation: %w", err)
	}
	if rec.Status.Terminal() {
		return closedChan(), nil
	}
	c := make(chan struct{})
	go func() {
		t := time.NewTicker(m.poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			rec, err := m.store.Get(id)
			if err != nil || rec.Status.Terminal() {
				close(c)
				return
			}
		}
	}()
	return c, nil
}

// Cancel is the action of system.operations.cancel, after the Dispatcher
// has authorized the call against the record's StartScope and written the
// request's audit record (Data Flow, operation cancel). It creates no record
// and takes no lane. cancelPolicy reports the Cancel of an op ID.
//
//   - Queued: compare-and-set to Canceled, then audit, and cancel the
//     context of any Prepare in progress. A worker that already dequeued it
//     abandons it.
//   - Running with CancelRunning: cancel the run; it ends Canceled. A
//     detached operation is signaled through its unit (3.3).
//   - Canceled: the same body again; nothing else happens.
//   - Running with CancelQueuedOnly, Succeeded, Failed: not-cancelable.
func (m *Manager) Cancel(_ context.Context, id string, cancelPolicy func(action string) def.Cancel) (apitypes.Operation, error) {
	rec, err := m.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return apitypes.Operation{}, notFound()
	}
	if err != nil {
		return apitypes.Operation{}, fmt.Errorf("load operation: %w", err)
	}
	for range 2 {
		switch rec.Status {
		case apitypes.StatusCanceled:
			return rec.ToAPI(m.host), nil
		case apitypes.StatusQueued:
			cur, err := m.store.CompareAndSetStatus(id, apitypes.StatusQueued, apitypes.StatusCanceled)
			if errors.Is(err, store.ErrStatusConflict) {
				// A worker started it meanwhile: decide again on what it is now.
				rec = cur
				continue
			}
			if err != nil {
				return apitypes.Operation{}, fmt.Errorf("cancel operation: %w", err)
			}
			m.canceledQueued(cur)
			return cur.ToAPI(m.host), nil
		case apitypes.StatusRunning:
			if cancelPolicy == nil || cancelPolicy(rec.Action) != def.CancelRunning {
				return apitypes.Operation{}, notCancelable(rec)
			}
			m.cancelRunning(rec)
			return rec.ToAPI(m.host), nil
		default:
			return apitypes.Operation{}, notCancelable(rec)
		}
	}
	return apitypes.Operation{}, notCancelable(rec)
}

func notCancelable(rec store.OpRecord) *problem.Error {
	return problem.New(problem.NotCancelable, fmt.Sprintf("the operation is %s and cannot be canceled", rec.Status)).
		With("operationId", rec.ID)
}

// canceledQueued finishes a cancel whose compare-and-set to Canceled won:
// the audit record follows the compare-and-set, a still-queued entry leaves
// its lane without taking a slot, and a Prepare in progress sees its
// context canceled.
func (m *Manager) canceledQueued(rec store.OpRecord) {
	m.mu.Lock()
	e, ok := m.live[rec.ID]
	if ok && m.dequeue(e) {
		delete(m.live, rec.ID)
	}
	m.mu.Unlock()
	if !ok {
		// Not run by this process (for example, restored after a restart
		// and not yet re-enqueued): the record alone is canceled.
		m.stateChange(&entry{id: rec.ID, rec: rec}, apitypes.StatusCanceled, nil)
		return
	}
	e.cancel()
	m.stateChange(e, apitypes.StatusCanceled, nil)
	e.markDone()
}

// cancelRunning cancels the run of a CancelRunning operation this process
// runs. Its worker commits Canceled when Run returns.
func (m *Manager) cancelRunning(rec store.OpRecord) {
	m.mu.Lock()
	e, ok := m.live[rec.ID]
	if ok {
		e.userCanceled = true
	}
	m.mu.Unlock()
	if !ok {
		m.log.Warn("cancel of a running operation this process does not run", "operationId", rec.ID)
		return
	}
	e.cancel()
}
