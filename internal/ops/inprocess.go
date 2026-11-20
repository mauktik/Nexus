// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
)

const genericDetail = "the operation failed with an internal error"

// run is the in-process runner of one dequeued operation (Data Flow,
// operation lifecycle). The transition out of Queued is a compare-and-set
// first and an audit second, because only the compare-and-set decides
// whether the operation runs or was canceled; a worker whose
// compare-and-set fails abandons the operation. The terminal transition is
// audited first, then committed.
func (m *Manager) run(e *entry) {
	defer m.wg.Done()
	defer m.finish(e)

	if _, err := m.store.CompareAndSetStatus(e.id, apitypes.StatusQueued, apitypes.StatusRunning); err != nil {
		if errors.Is(err, store.ErrStatusConflict) {
			m.log.Debug("operation abandoned: no longer queued", "operationId", e.id)
		} else {
			m.log.Error("cannot start the operation", "operationId", e.id, "error", err.Error())
		}
		return
	}
	m.stateChange(e, apitypes.StatusRunning, nil)

	out, runErr := m.execute(e)
	to, result, perr := m.outcome(e, out, runErr)

	m.stateChange(e, to, perr)
	if _, err := m.store.CommitTerminal(e.id, apitypes.StatusRunning, to, result, perr); err != nil {
		m.log.Error("cannot commit the operation's end", "operationId", e.id, "status", string(to), "error", err.Error())
	}
}

// execute runs the operation's Run with an output handle on its output log
// and contains a panic to this operation (NFR-3): the panic is logged with
// its stack and never reaches the record.
func (m *Manager) execute(e *entry) (out any, err error) {
	var output io.Writer
	w, oerr := oplog.Open(m.outputDir(e.id), oplog.Options{CapBytes: m.capBytes, Now: m.now})
	if oerr != nil {
		// Output is a convenience; the operation still runs and reports its
		// result.
		m.log.Error("cannot open the operation's output log", "operationId", e.id, "error", oerr.Error())
	} else {
		output = w.Stream(apitypes.StreamStdout)
		defer func() {
			if cerr := w.Close(); cerr != nil {
				m.log.Error("cannot close the operation's output log", "operationId", e.id, "error", cerr.Error())
			}
		}()
	}
	defer func() {
		if v := recover(); v != nil {
			m.log.Error("operation panic", "operationId", e.id, "op", e.rec.Action,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			out, err = nil, problem.New(problem.InternalError, genericDetail)
		}
	}()
	return e.work.Run(e.ctx, provider.NewRun(e.id, output))
}

// outcome maps Run's return to the terminal status. Failed always carries a
// structured problem: a *problem.Error as returned, anything else a generic
// internal-error whose cause goes only to the log.
func (m *Manager) outcome(e *entry, out any, runErr error) (apitypes.OperationStatus, json.RawMessage, *problem.Error) {
	m.mu.Lock()
	canceled := e.userCanceled
	m.mu.Unlock()
	if runErr == nil {
		b, err := json.Marshal(out)
		if err != nil {
			m.log.Error("cannot encode the operation's result", "operationId", e.id, "error", err.Error())
			return apitypes.StatusFailed, nil, problem.New(problem.InternalError, genericDetail)
		}
		return apitypes.StatusSucceeded, b, nil
	}
	if canceled {
		return apitypes.StatusCanceled, nil, nil
	}
	if m.base.Err() != nil {
		return apitypes.StatusFailed, nil, problem.New(problem.OutcomeUnknown,
			"the operation was interrupted when nexusd stopped")
	}
	var p *problem.Error
	if errors.As(runErr, &p) {
		return apitypes.StatusFailed, nil, p
	}
	m.log.Error("operation failed", "operationId", e.id, "op", e.rec.Action, "error", runErr.Error())
	return apitypes.StatusFailed, nil, problem.New(problem.InternalError, genericDetail)
}
