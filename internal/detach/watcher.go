// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package detach

import (
	"context"
	"errors"
	"time"
)

// Watcher timing (Data Flow, detached operation lifecycle step 6).
const (
	PollInterval     = 500 * time.Millisecond // exit.json and MaxRuntime
	LivenessInterval = 10 * time.Second       // UnitState, per operation
	RecheckDelay     = 2 * time.Second        // Before an inactive or absent unit is believed
	StateTimeout     = 5 * time.Second        // One UnitState call; past it the answer is unknown
)

// Outcome is how a watched operation ended. Exactly one field is set.
type Outcome struct {
	Exit *Exit // exit.json appeared
	// Lost means the unit was inactive or absent on a check and again on
	// the re-check RecheckDelay later, with no exit.json.
	Lost bool
	// Overran means MaxRuntime passed. The unit was not signaled and may
	// still run.
	Overran bool
}

// watched is one operation the watcher follows. Its fields belong to the
// watch loop.
type watched struct {
	ctx     context.Context
	id      string
	unit    string
	dir     string
	started time.Time     // On the Options.Now clock
	max     time.Duration // MaxRuntime
	out     chan Outcome  // Buffered: the loop never blocks on it

	next      time.Time   // Next liveness check
	recheckAt time.Time   // Non-zero while a re-check is due
	pending   chan answer // A UnitState call in flight
	asked     time.Time   // When the call in flight started
	deadline  time.Time   // Of the call in flight
	exitErr   error       // Last exit.json read error, logged once
}

// answer is the result of one UnitState call.
type answer struct {
	liveness Liveness
	err      error
	recheck  bool
}

// Watch follows operation id, whose unit started at started (on the
// Options.Now clock), until it ends: exit.json appears, the unit is
// confirmed gone without one, or maxRuntime passes. The returned channel
// receives that one Outcome. Nothing is delivered once ctx ends, and the
// operation is no longer watched.
//
// The watcher reads exit.json every PollInterval and the unit's state
// every LivenessInterval. A UnitState error or a call longer than
// StateTimeout is neither answer: it is logged and the next check retries.
// Only a definite inactive or absent answer starts the re-check, and an
// error during the re-check cancels it. One goroutine serves every watched
// operation, and it exists only while one is watched (NFR-1).
func (l *Launcher) Watch(ctx context.Context, id string, started time.Time, maxRuntime time.Duration) <-chan Outcome {
	w := &watched{
		ctx: ctx, id: id, unit: Unit(id), dir: l.Dir(id), started: started, max: maxRuntime,
		out: make(chan Outcome, 1), next: time.Now().Add(LivenessInterval),
	}
	l.mu.Lock()
	l.watched[id] = w
	if !l.watching {
		l.watching = true
		go l.watchLoop()
	}
	l.mu.Unlock()
	return w.out
}

// watchLoop polls every watched operation and returns once none is left.
func (l *Launcher) watchLoop() {
	t := time.NewTicker(l.pollEvery)
	defer t.Stop()
	for range t.C {
		l.mu.Lock()
		if len(l.watched) == 0 {
			l.watching = false
			l.mu.Unlock()
			return
		}
		ws := make([]*watched, 0, len(l.watched))
		for _, w := range l.watched {
			ws = append(ws, w)
		}
		l.mu.Unlock()

		for _, w := range ws {
			if l.poll(w) {
				l.mu.Lock()
				if l.watched[w.id] == w {
					delete(l.watched, w.id)
				}
				l.mu.Unlock()
			}
		}
	}
}

// poll advances one operation and reports whether it is finished with.
func (l *Launcher) poll(w *watched) bool {
	if w.ctx.Err() != nil {
		return true
	}
	if l.exited(w) {
		return true
	}
	if w.max > 0 && l.now().Sub(w.started) > w.max {
		w.out <- Outcome{Overran: true}
		return true
	}
	now := time.Now()
	if w.pending != nil {
		select {
		case a := <-w.pending:
			w.pending = nil
			if l.settle(w, a, now) {
				return true
			}
		default:
			if now.Before(w.deadline) {
				return false
			}
			w.pending = nil
			l.unknown(w, context.DeadlineExceeded, now)
		}
	}
	switch {
	case w.pending != nil:
	case !w.recheckAt.IsZero() && !now.Before(w.recheckAt):
		l.query(w, true, now)
	case w.recheckAt.IsZero() && !now.Before(w.next):
		l.query(w, false, now)
	}
	return false
}

// exited delivers exit.json when it exists.
func (l *Launcher) exited(w *watched) bool {
	x, ok, err := ReadExit(w.dir)
	if err != nil {
		if w.exitErr == nil || w.exitErr.Error() != err.Error() {
			l.log.Error("cannot read the operation's exit.json", "operationId", w.id, "error", err.Error())
		}
		w.exitErr = err
		return false
	}
	if !ok {
		return false
	}
	w.out <- Outcome{Exit: &x}
	return true
}

// settle acts on a UnitState answer and reports whether the operation is
// finished with.
func (l *Launcher) settle(w *watched, a answer, now time.Time) bool {
	if a.err != nil {
		l.unknown(w, a.err, now)
		return false
	}
	if a.liveness == Live {
		l.resume(w, now)
		return false
	}
	if !a.recheck {
		// RecheckDelay after the check that saw it, not after the answer
		// was collected.
		w.recheckAt = w.asked.Add(RecheckDelay)
		return false
	}
	// Confirmed gone. exit.json may have landed since the last read.
	if l.exited(w) {
		return true
	}
	w.out <- Outcome{Lost: true}
	return true
}

// unknown handles a UnitState error or timeout: the operation stays as it
// is, a re-check in progress is canceled, and the next check retries.
func (l *Launcher) unknown(w *watched, err error, now time.Time) {
	reason := "error"
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "timeout"
	}
	l.log.Warn("unit state unknown; the operation stays running", "operationId", w.id, "unit", w.unit,
		"reason", reason, "error", err.Error())
	l.resume(w, now)
}

// resume ends any re-check and returns to the regular schedule: the next
// check is LivenessInterval after the last regular one.
func (l *Launcher) resume(w *watched, now time.Time) {
	w.recheckAt = time.Time{}
	if w.next.Before(now) {
		w.next = now.Add(LivenessInterval)
	}
}

// query starts one UnitState call bounded by StateTimeout. Its goroutine
// never blocks on the answer, so an operation dropped meanwhile leaves
// nothing behind once the call returns.
func (l *Launcher) query(w *watched, recheck bool, now time.Time) {
	c := make(chan answer, 1)
	w.pending, w.asked, w.deadline = c, now, now.Add(StateTimeout)
	if !recheck {
		w.next = now.Add(LivenessInterval)
	}
	go func() {
		lv, err := l.State(context.Background(), w.id)
		c <- answer{liveness: lv, err: err, recheck: recheck}
	}()
}
