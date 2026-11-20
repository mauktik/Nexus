// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package ops_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
)

// scriptedUnits answers UnitState per unit from a script that the test
// changes as it goes, and records the time of every call.
type scriptedUnits struct {
	mu     sync.Mutex
	answer map[string]func(ctx context.Context) (detach.UnitState, error)
	calls  map[string][]time.Time
}

func (s *scriptedUnits) set(id string, f func(ctx context.Context) (detach.UnitState, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer[detach.Unit(id)] = f
}

func (s *scriptedUnits) callTimes(id string) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.calls[detach.Unit(id)]...)
}

func (s *scriptedUnits) state(ctx context.Context, unit string) (detach.UnitState, error) {
	s.mu.Lock()
	s.calls[unit] = append(s.calls[unit], time.Now())
	f := s.answer[unit]
	s.mu.Unlock()
	if f == nil {
		return detach.UnitState{}, errors.New("no scripted answer")
	}
	return f(ctx)
}

func activeUnit(context.Context) (detach.UnitState, error) {
	return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
}

func inactiveUnit(context.Context) (detach.UnitState, error) {
	return detach.UnitState{ActiveState: "inactive", LoadState: "loaded"}, nil
}

func absentUnit(context.Context) (detach.UnitState, error) {
	return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
}

// offsets are call times relative to t0, in whole seconds.
func offsets(ts []time.Time, t0 time.Time) []time.Duration {
	out := make([]time.Duration, len(ts))
	for i, t := range ts {
		out[i] = t.Sub(t0).Truncate(100 * time.Millisecond)
	}
	return out
}

func durations(secs ...float64) []time.Duration {
	out := make([]time.Duration, len(secs))
	for i, s := range secs {
		out[i] = time.Duration(s * float64(time.Second))
	}
	return out
}

func sameDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TC-213: with nexusd up, the watcher checks each running detached
// operation's unit every 10 seconds. A definite inactive or absent answer is
// re-checked after 2 seconds before the operation ends outcome-unknown and
// frees its lane slot; exit.json written before the re-check wins. An error
// or a timeout is neither answer: it is logged, the operation stays Running
// with its slot held, and the next poll retries. With nothing running, no
// call is made and no watcher goroutine remains.
func TestWatcherLivenessPollingWhileTheDaemonIsUp(t *testing.T) {
	// Step 4 counts every watcher goroutine of the process: the watchers
	// of earlier tests' operations, outside this test's bubble, must have
	// ended first (seen on the T1 harnesses, where a whole package runs in
	// one process).
	waitNoWatcher(t)
	synctest.Test(t, func(t *testing.T) {
		units := &scriptedUnits{answer: map[string]func(context.Context) (detach.UnitState, error){},
			calls: map[string][]time.Time{}}
		spy := newUnitSpy()
		spy.onLaunch = func(detach.Invocation) error { return nil } // No child: the test decides the end
		spy.state = units.state
		w := spyWorld(t, spy)
		p := who("ops", "td:write")
		job := tdOps.job.Op()
		start := func(name, lane string) apitypes.Operation {
			t.Helper()
			op := w.start(p, job, `{"name":"`+name+`","lane":"`+lane+`","policy":"reject"}`, "")
			synctest.Wait() // Launched and watched
			if r, _ := w.store.Get(op.ID); r.Status != apitypes.StatusRunning {
				t.Fatalf("%s: %s after the launch", name, r.Status)
			}
			return op
		}
		status := func(id string) apitypes.OperationStatus {
			t.Helper()
			r, err := w.store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			return r.Status
		}
		laneHeld := func(lane string) bool {
			t.Helper()
			_, err := w.call(p, job, `{"name":"probe","lane":"`+lane+`","policy":"reject"}`, "")
			var perr *problem.Error
			if errors.As(err, &perr) && perr.Slug() == problem.Conflict {
				return true
			}
			if err != nil {
				t.Fatalf("probe on %s: %v", lane, err)
			}
			t.Fatalf("probe on %s was admitted: the lane was free", lane)
			return false
		}

		// Step 1: active for three intervals, one call per 10 seconds.
		a := start("a", "la")
		t0 := time.Now()
		units.set(a.ID, activeUnit)
		time.Sleep(35 * time.Second)
		synctest.Wait()
		if got := offsets(units.callTimes(a.ID), t0); !sameDurations(got, durations(10, 20, 30)) {
			t.Fatalf("step 1 calls at %v, want 10s, 20s, 30s", got)
		}
		if status(a.ID) != apitypes.StatusRunning || !laneHeld("la") {
			t.Fatal("step 1: the op must stay Running with its slot held")
		}

		// Step 2: inactive with no exit.json; re-checked after 2 seconds,
		// then outcome-unknown with the provider's detail, slot freed.
		units.set(a.ID, inactiveUnit)
		time.Sleep(10 * time.Second) // Past the check at 40s and its re-check at 42s
		synctest.Wait()
		if got := offsets(units.callTimes(a.ID), t0); !sameDurations(got, durations(10, 20, 30, 40, 42)) {
			t.Fatalf("step 2 calls at %v, want a re-check 2s after the check at 40s", got)
		}
		rec, err := w.store.Get(a.ID)
		if err != nil || rec.Status != apitypes.StatusFailed || rec.Error == nil ||
			rec.Error.Slug() != problem.OutcomeUnknown || !strings.Contains(rec.Error.Detail, "lost detail for a") ||
			!strings.Contains(rec.Error.Detail, detach.Unit(a.ID)) {
			t.Fatalf("step 2 record %+v (%v)", rec, err)
		}
		b0 := w.start(p, job, `{"name":"free","lane":"la","policy":"reject"}`, "")
		synctest.Wait()
		units.set(b0.ID, activeUnit)
		if err := detach.WriteExit(w.opDir(b0.ID), detach.Exit{Started: time.Now(), Ended: time.Now()}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if status(b0.ID) != apitypes.StatusSucceeded {
			t.Fatalf("an op on the freed lane: %s", status(b0.ID))
		}

		// Step 3: absent, then exit.json lands before the re-check: the
		// real outcome is reported.
		b := start("b", "lb")
		tb := time.Now()
		units.set(b.ID, absentUnit)
		time.Sleep(10*time.Second + 500*time.Millisecond)
		synctest.Wait()
		if n := len(units.callTimes(b.ID)); n != 1 {
			t.Fatalf("step 3: %d calls before exit.json, want 1", n)
		}
		if err := detach.WriteExit(w.opDir(b.ID), detach.Exit{ExitCode: 0, Started: tb, Ended: time.Now()}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if status(b.ID) != apitypes.StatusSucceeded {
			t.Fatalf("step 3: %s, want the real outcome Succeeded", status(b.ID))
		}
		if n := len(units.callTimes(b.ID)); n != 1 {
			t.Fatalf("step 3: %d calls, want no re-check once exit.json exists", n)
		}

		// Step 4: nothing running; several intervals make no call and leave
		// no watcher goroutine.
		before := len(units.callTimes(a.ID)) + len(units.callTimes(b.ID)) + len(units.callTimes(b0.ID))
		time.Sleep(60 * time.Second)
		synctest.Wait()
		after := len(units.callTimes(a.ID)) + len(units.callTimes(b.ID)) + len(units.callTimes(b0.ID))
		if after != before {
			t.Fatalf("step 4: %d calls with nothing running", after-before)
		}
		if n := watcherGoroutines(); n != 0 {
			t.Fatalf("step 4: %d watcher goroutines with nothing running", n)
		}

		// Step 5: an error for one interval, then a call blocked past the
		// 5-second timeout, then active. Both are logged, the op stays
		// Running with its slot held and no re-check, and each next poll
		// retries.
		c := start("c", "lc")
		tc := time.Now()
		var n int
		var mu sync.Mutex
		units.set(c.ID, func(ctx context.Context) (detach.UnitState, error) {
			mu.Lock()
			n++
			k := n
			mu.Unlock()
			switch k {
			case 1:
				return detach.UnitState{}, errors.New("systemctl show: exit status 1: Failed to connect to bus")
			case 2:
				<-ctx.Done()
				return detach.UnitState{}, ctx.Err()
			}
			return activeUnit(ctx)
		})
		for _, at := range []float64{10.5, 20.5, 25.5, 30.5} {
			time.Sleep(time.Duration(at*float64(time.Second)) - time.Since(tc))
			synctest.Wait()
			if status(c.ID) != apitypes.StatusRunning || !laneHeld("lc") {
				t.Fatalf("step 5 at %.1fs: %s; the op must stay Running with its slot held", at, status(c.ID))
			}
		}
		if got := offsets(units.callTimes(c.ID), tc); !sameDurations(got, durations(10, 20, 30)) {
			t.Fatalf("step 5 calls at %v, want 10s, 20s, 30s with no re-check", got)
		}
		logs := w.logs.String()
		for _, reason := range []string{`"reason":"error"`, `"reason":"timeout"`} {
			found := false
			for _, line := range strings.Split(logs, "\n") {
				if strings.Contains(line, "unit state unknown") && strings.Contains(line, c.ID) && strings.Contains(line, reason) {
					found = true
				}
			}
			if !found {
				t.Fatalf("step 5: no %s log for the op:\n%s", reason, logs)
			}
		}
		if strings.Contains(logs, `"msg":"operation abandoned`) {
			t.Fatal("step 5: unexpected abandonment")
		}
		if err := detach.WriteExit(w.opDir(c.ID), detach.Exit{Started: tc, Ended: time.Now()}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if status(c.ID) != apitypes.StatusSucceeded {
			t.Fatalf("step 5 end: %s", status(c.ID))
		}
	})
}
