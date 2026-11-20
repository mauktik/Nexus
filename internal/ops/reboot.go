package ops

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
)

// The manager rules of an op declaring def.Op.CompletesAcrossReboot (D-3).
// They are keyed only on that declaration and on the reboot record its
// helper writes with detach.RecordReboot; every other op ends as before.
//
//   - R1: the op is not launched when this boot's ID is unknown (runDetached).
//   - R2: a cancel is refused not-cancelable once the record exists, or when
//     it cannot be read (cancelDetached).
//   - R3: a unit that ended interrupted, lost or canceled with a record from
//     this boot is held Running, its lane slot kept, until nexusd stops or
//     the op's MaxRuntime from its start passes (holdForReboot).
//   - R4: the same end with a record from an earlier boot is decided by the
//     binding's AfterReboot (afterReboot).
//   - R5: a normal exit of the unit's own goes to Finalize, as before.
//
// A record that cannot be read ends the op outcome-unknown at once, with no
// hold and no AfterReboot.

// rebootVerdict is what R3 and R4 decide for the end of a detached
// operation.
type rebootVerdict int

const (
	// rebootNotApplicable: the op does not declare the flag, its unit
	// ended with a result of its own or overran, or there is no record.
	// The end is decided as for any detached op.
	rebootNotApplicable rebootVerdict = iota
	rebootHold                        // R3
	rebootComplete                    // R4
	// rebootUnknown: the record cannot be read, or this boot's ID is
	// unknown, so no reboot can be confirmed: outcome-unknown at once.
	rebootUnknown
)

// rebootDecision is acrossReboot's answer.
type rebootDecision struct {
	verdict rebootVerdict
	rec     detach.RebootRecord // For rebootHold and rebootComplete
	perr    *problem.Error      // For rebootUnknown
}

// acrossReboot decides R3 and R4 for operation e whose unit ended with o.
// It reads the reboot record only for an op declaring the flag whose unit
// ended interrupted, lost or canceled.
func (m *Manager) acrossReboot(e *entry, o detach.Outcome) rebootDecision {
	if !e.work.Op.CompletesAcrossReboot || m.detach == nil || !m.endedWithoutResult(e.id, o) {
		return rebootDecision{}
	}
	rec, ok, err := detach.ReadReboot(m.detach.Dir(e.id))
	switch {
	case err != nil:
		m.log.Error("cannot read the operation's reboot record", "operationId", e.id, "error", err.Error())
		return rebootDecision{verdict: rebootUnknown, perr: problem.New(problem.OutcomeUnknown,
			fmt.Sprintf("the reboot record (%s) cannot be read: %v; the host may or may not have rebooted", detach.RebootFile, err))}
	case !ok:
		return rebootDecision{}
	case m.boot == "":
		// R1 keeps such an op from launching in a boot whose ID is
		// unknown; this is a later start that cannot read it either.
		return rebootDecision{verdict: rebootUnknown, perr: problem.New(problem.OutcomeUnknown,
			fmt.Sprintf("the reboot was requested at %s in boot %s, but this boot's ID cannot be read, so the reboot could not be confirmed",
				rec.RequestedAt.UTC().Format(time.RFC3339), rec.BootID))}
	case rec.BootID == m.boot:
		return rebootDecision{verdict: rebootHold, rec: rec}
	}
	return rebootDecision{verdict: rebootComplete, rec: rec}
}

// endedWithoutResult reports whether operation id's unit ended without a
// result of its own: lost, canceled, or interrupted (interruption). An
// overrun unit has not ended.
func (m *Manager) endedWithoutResult(id string, o detach.Outcome) bool {
	switch {
	case o.Lost:
		return true
	case o.Exit == nil:
		return false
	case o.Exit.Canceled:
		return true
	}
	return m.interruption(id, *o.Exit) != ""
}

// holdForReboot is R3: nothing is committed while the reboot requested in
// this boot may still come. It returns at shutdown, leaving the record
// Running for the next start (R4 there), or once maxRuntime from started
// has passed, when the op ends outcome-unknown. The caller keeps e's lane
// slot until it returns.
func (m *Manager) holdForReboot(e *entry, rb detach.RebootRecord, started time.Time, maxRuntime time.Duration) {
	if maxRuntime <= 0 {
		// A hold with no end would keep the lane forever (A-63).
		maxRuntime = fallbackMaxRuntime
	}
	m.dropSpec(e.id)
	deadline := started.Add(maxRuntime)
	m.log.Info("operation held: its reboot was requested in this boot", "operationId", e.id,
		"requestedAt", rb.RequestedAt.UTC().Format(time.RFC3339), "bootId", rb.BootID, "until", deadline.UTC().Format(time.RFC3339))
	t := time.NewTimer(deadline.Sub(m.now()))
	defer t.Stop()
	select {
	case <-m.halt.Done():
		return
	case <-t.C:
	}
	m.commitEnd(e, apitypes.StatusFailed, nil, problem.New(problem.OutcomeUnknown,
		fmt.Sprintf("the reboot was requested at %s in boot %s, but the host did not restart within the operation's maximum runtime of %v; %s ended without reporting an outcome",
			rb.RequestedAt.UTC().Format(time.RFC3339), rb.BootID, maxRuntime, m.detach.Subject(e.id))))
}

// holdRecovered runs R3 for a recovered operation in its own goroutine,
// outside the classification deadline, with the time its MaxRuntime has
// left from its start. It finishes e once the hold ends.
func (m *Manager) holdRecovered(e *entry, rb detach.RebootRecord) {
	started := m.now()
	if e.rec.Started != nil {
		started = *e.rec.Started
	}
	maxRuntime := m.readMaxRuntime(e.id)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.finish(e)
		m.holdForReboot(e, rb, started, maxRuntime)
	}()
}

// afterReboot is R4: the binding's AfterReboot decides the outcome of an
// operation whose reboot was requested in an earlier boot. Its output makes
// the op Succeeded and its error Failed. The terminal state-change record
// carries both boot IDs, and whether a cancel had been asked for.
func (m *Manager) afterReboot(e *entry, d *DetachedWork, r *provider.Run, rb detach.RebootRecord) (
	to apitypes.OperationStatus, result json.RawMessage, perr *problem.Error) {
	e.facts = map[string]string{"previousBootId": rb.BootID, "bootId": m.boot}
	canceled, err := detach.CancelRequested(m.detach.Dir(e.id))
	if err != nil {
		m.log.Warn("cannot check the operation's cancel request", "operationId", e.id, "error", err.Error())
	} else if canceled {
		// A cancel accepted after the helper's last check does not stop
		// the reboot (A-28); the fact explains the Succeeded.
		e.facts["cancelRequested"] = "true"
	}
	defer func() {
		if v := recover(); v != nil {
			m.log.Error("operation panic", "operationId", e.id, "op", e.rec.Action,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			to, result, perr = apitypes.StatusFailed, nil, problem.New(problem.InternalError, genericDetail)
		}
	}()
	if d.Funcs.AfterReboot == nil {
		m.log.Error("no AfterReboot for an operation that completes across a reboot", "operationId", e.id, "op", e.rec.Action)
		return apitypes.StatusFailed, nil, problem.New(problem.InternalError, genericDetail)
	}
	in, err := d.Decode(e.rec.Input)
	if err != nil {
		m.log.Error("cannot decode the operation's input", "operationId", e.id, "error", err.Error())
		return apitypes.StatusFailed, nil, problem.New(problem.InternalError, genericDetail)
	}
	m.mu.Lock()
	ready := m.readyAt
	m.mu.Unlock()
	out, err := d.Funcs.AfterReboot(m.base, in, provider.Rebooted{
		PreviousBootID: rb.BootID, BootID: m.boot, RequestedAt: rb.RequestedAt, ReadyAt: ready,
	}, r)
	if err != nil {
		return apitypes.StatusFailed, nil, m.problemOf(e, "after-reboot", err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		m.log.Error("cannot encode the operation's result", "operationId", e.id, "error", err.Error())
		return apitypes.StatusFailed, nil, problem.New(problem.InternalError, genericDetail)
	}
	return apitypes.StatusSucceeded, b, nil
}

// refuseCancelAfterReboot is R2: a running op declaring the flag cannot be
// canceled once its reboot record exists, nor when the record cannot be
// read, since the reboot may have been requested. It is nil when the cancel
// may go on.
func (m *Manager) refuseCancelAfterReboot(rec store.OpRecord) *problem.Error {
	m.mu.Lock()
	e, ok := m.live[rec.ID]
	m.mu.Unlock()
	if !ok || !e.work.Op.CompletesAcrossReboot || m.detach == nil {
		return nil
	}
	rb, found, err := detach.ReadReboot(m.detach.Dir(rec.ID))
	switch {
	case err != nil:
		m.log.Error("cannot read the operation's reboot record; the cancel is refused", "operationId", rec.ID, "error", err.Error())
		return problem.New(problem.NotCancelable, "the reboot record cannot be read; the reboot may have been requested").
			With("operationId", rec.ID)
	case found:
		return problem.New(problem.NotCancelable,
			fmt.Sprintf("the reboot was already requested at %s; the operation completes after the reboot",
				rb.RequestedAt.UTC().Format(time.RFC3339))).
			With("operationId", rec.ID)
	}
	return nil
}

// Active lists the Queued and Running operations (provider.Operations).
func (m *Manager) Active() ([]provider.ActiveOperation, error) {
	recs, err := m.store.ListNonTerminal()
	if err != nil {
		return nil, fmt.Errorf("ops: list the active operations: %w", err)
	}
	out := make([]provider.ActiveOperation, 0, len(recs))
	for _, rec := range recs {
		out = append(out, provider.ActiveOperation{ID: rec.ID, Op: rec.Action, Status: rec.Status})
	}
	return out, nil
}

var _ provider.Operations = (*Manager)(nil)
