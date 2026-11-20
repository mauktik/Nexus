// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package ops_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
)

// tdOps are the detached test provider's declarations: a cancelable job
// with a lane from its input, and a scripts-style op on a per-script lane.
var tdOps = func() struct{ job, script def.Typed[thingIn, thingOut] } {
	job := def.Op{
		ID: "td.jobs.run", Provider: "td", Kind: def.KindResourceAction, ResourceType: "jobs", Action: "run",
		Summary: "s", Description: "Detached test operation for the operation manager tests.",
		Scope: def.Scope{Name: "td:write"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		Cancel: def.CancelRunning, Sensitive: []string{"secret"}, Refine: refineThing,
		CLI: def.CLIBinding{Noun: "job", Verb: "start", Positional: "name"},
	}
	script := def.Op{
		ID: "td.scripts.invoke", Provider: "td", Kind: def.KindResourceAction, ResourceType: "scripts", Action: "invoke",
		Summary: "s", Description: "Detached test script for the operation manager tests.",
		Scope: def.Scope{Name: "td:run", PerTarget: "td:run:{name}"}, Risk: def.RiskDestructive,
		Mode: def.ModeLongRunning, Refine: refineThing,
		CLI: def.CLIBinding{Noun: "tdscript", Verb: "invoke", Positional: "name"},
	}
	return struct{ job, script def.Typed[thingIn, thingOut] }{
		job: def.Define[thingIn, thingOut](job), script: def.Define[thingIn, thingOut](script),
	}
}()

// finalCall is one Finalize the detached test provider saw.
type finalCall struct {
	name string
	exit detach.Exit
}

// detProv is a provider of detached operations. Prepare is scripted per
// thing name; the default runs a short shell child.
type detProv struct {
	prepares atomic.Int64

	mu       sync.Mutex
	prepare  map[string]func(ctx context.Context, in thingIn) (detach.Spec, error)
	finals   []finalCall
	inputs   []thingIn // Inputs Finalize ran on
	finalErr error
}

func newDetProv() *detProv {
	return &detProv{prepare: map[string]func(context.Context, thingIn) (detach.Spec, error){}}
}

func (p *detProv) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "td", ReadScope: "td:read", Ops: []def.Op{tdOps.job.Op(), tdOps.script.Op()}}
}
func (p *detProv) Configure(func(any) error) error { return nil }
func (p *detProv) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (p *detProv) onPrepare(name string, f func(ctx context.Context, in thingIn) (detach.Spec, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prepare[name] = f
}

func (p *detProv) finalCalls() []finalCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]finalCall(nil), p.finals...)
}

// shJob is a detached spec running script with /bin/sh.
func shJob(script string) detach.Spec {
	return detach.Spec{
		Argv: []string{"/bin/sh", "-c", script}, Env: []string{"PATH=/usr/bin:/bin"},
		KillMode: detach.KillControlGroup, MaxRuntime: time.Hour, Grace: time.Second,
	}
}

func (p *detProv) Bindings() []provider.Binding {
	spec := provider.DetachedSpec[thingIn, thingOut]{
		Lane: func(_ context.Context, in thingIn) (provider.Lane, error) { return laneOf(in), nil },
		Prepare: func(ctx context.Context, in thingIn, _ *provider.Run) (detach.Spec, error) {
			p.prepares.Add(1)
			p.mu.Lock()
			f := p.prepare[in.Name]
			p.mu.Unlock()
			if f != nil {
				return f(ctx, in)
			}
			return shJob("echo hello " + in.Name), nil
		},
		Finalize: func(_ context.Context, in thingIn, x detach.Exit, _ *provider.Run) (thingOut, error) {
			p.mu.Lock()
			p.finals = append(p.finals, finalCall{name: in.Name, exit: x})
			p.inputs = append(p.inputs, in)
			err := p.finalErr
			p.mu.Unlock()
			if err != nil {
				return thingOut{}, err
			}
			if x.ExitCode != 0 {
				return thingOut{}, problem.New(problem.ScriptFailed, fmt.Sprintf("exit code %d", x.ExitCode))
			}
			return thingOut{ActionResult: apitypes.ActionResult{Changed: true}, Name: in.Name}, nil
		},
		LostDetail: func(_ *provider.Run, in thingIn) string { return "lost detail for " + in.Name },
	}
	script := spec
	script.Lane = func(_ context.Context, in thingIn) (provider.Lane, error) {
		return provider.Lane{Key: "script:" + in.Name, Policy: provider.LaneQueue}, nil
	}
	return []provider.Binding{provider.Detached(tdOps.job, spec), provider.Detached(tdOps.script, script)}
}

// withDetach gives the world's manager a launcher with the seams d.
func withDetach(t *testing.T, d detach.Options) func(*ops.Options) {
	return func(o *ops.Options) {
		l, err := detach.New(detach.Config{OpsDir: o.Dir, Logger: o.Logger}, d)
		if err != nil {
			t.Fatal(err)
		}
		o.Detach = l
	}
}

// unitSpy is a full set of injected seams that start no process: Launch
// counts launches and runs onLaunch, UnitState answers through state, and
// Signal records each signal and runs onSignal.
type unitSpy struct {
	mu       sync.Mutex
	launches map[string]int
	signals  map[string][]syscall.Signal
	onLaunch func(inv detach.Invocation) error
	onSignal func(unit string, sig syscall.Signal) error
	state    func(ctx context.Context, unit string) (detach.UnitState, error)
}

// newUnitSpy returns a spy whose launches finish at once with exit code 0
// and whose signals finish the operation canceled, as the supervisor does.
func newUnitSpy() *unitSpy {
	s := &unitSpy{launches: map[string]int{}, signals: map[string][]syscall.Signal{}}
	s.onLaunch = func(inv detach.Invocation) error {
		now := time.Now()
		return detach.WriteExit(inv.Dir, detach.Exit{Started: now, Ended: now})
	}
	return s
}

func (s *unitSpy) options(opsDir func() string) detach.Options {
	return detach.Options{
		Launch: func(_ context.Context, inv detach.Invocation) error {
			s.mu.Lock()
			s.launches[inv.ID]++
			f := s.onLaunch
			s.mu.Unlock()
			return f(inv)
		},
		UnitState: func(ctx context.Context, unit string) (detach.UnitState, error) {
			s.mu.Lock()
			f := s.state
			s.mu.Unlock()
			if f != nil {
				return f(ctx, unit)
			}
			return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
		},
		Signal: func(_ context.Context, unit string, sig syscall.Signal) error {
			s.mu.Lock()
			s.signals[unit] = append(s.signals[unit], sig)
			f := s.onSignal
			s.mu.Unlock()
			if f != nil {
				return f(unit, sig)
			}
			id := strings.TrimSuffix(strings.TrimPrefix(unit, detach.UnitPrefix), ".service")
			now := time.Now()
			return detach.WriteExit(filepath.Join(opsDir(), id), detach.Exit{ExitCode: -1, Signal: "SIGTERM",
				Started: now, Ended: now, Canceled: true})
		},
	}
}

func (s *unitSpy) launched(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.launches[id]
}

func (s *unitSpy) totalLaunches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.launches {
		n += c
	}
	return n
}

func (s *unitSpy) signaled(id string) []syscall.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]syscall.Signal(nil), s.signals[detach.Unit(id)]...)
}

// spyWorld is a world whose launcher is spy.
func spyWorld(t *testing.T, spy *unitSpy) *world {
	var opsDir atomic.Value
	w := newWorld(t, func(o *ops.Options) {
		opsDir.Store(o.Dir)
		withDetach(t, spy.options(func() string { return opsDir.Load().(string) }))(o)
	})
	return w
}

// localWorld is a world whose detached operations run under the real
// supervisor through detachtest.Local; wrap, when given, adjusts the seams.
func localWorld(t *testing.T, wrap ...func(*detach.Options)) *world {
	d := detachtest.Local()
	for _, f := range wrap {
		f(&d)
	}
	return newWorld(t, withDetach(t, d))
}

// backendWorld is a world whose detached operations run under the real
// supervisor through backend's seams; wrap, when given, adjusts them.
func backendWorld(t *testing.T, backend detachtest.Backend, wrap ...func(*detach.Options)) *world {
	return newWorld(t, withBackend(t, backend, nil, wrap...))
}

func (w *world) opDir(id string) string { return filepath.Join(w.stateDir, "ops", id) }

// opOutput is the output so far of operation id.
func (w *world) opOutput(id string) string {
	w.t.Helper()
	p, err := oplog.NewReader(filepath.Join(w.opDir(id), "output")).Read(0, 0)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, f := range p.Frames {
		b.WriteString(f.D)
	}
	return b.String()
}

var pidLine = regexp.MustCompile(`pid=(\d+)`)

// childPID waits for operation id's child to print pid=<pid>.
func (w *world) childPID(id string) int {
	w.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m := pidLine.FindStringSubmatch(w.opOutput(id)); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				w.t.Fatal(err)
			}
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.t.Fatalf("operation %s never printed its pid; output %q", id, w.opOutput(id))
	return 0
}

// startWhenFree starts op on a LaneReject lane, retrying while the lane is
// still held: a slot is released just after the terminal commit.
func (w *world) startWhenFree(p auth.Principal, op def.Op, input string) apitypes.Operation {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := w.call(p, op, input, "")
		var perr *problem.Error
		if errors.As(err, &perr) && perr.Slug() == problem.Conflict && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil {
			w.t.Fatalf("%s %s: %v", op.ID, input, err)
		}
		return res.Body.(apitypes.Operation)
	}
}

// stopLocal sends SIGTERM to operation id's supervisor and waits for its
// unit to be gone, so it never writes into a removed test directory.
func stopLocal(t *testing.T, d detach.Options, id string) {
	t.Helper()
	ctx := context.Background()
	unit := detach.Unit(id)
	if st, err := d.UnitState(ctx, unit); err != nil || st.LoadState == "not-found" {
		return
	}
	if err := d.Signal(ctx, unit, syscall.SIGTERM); err != nil {
		t.Errorf("stop the supervisor of %s: %v", id, err)
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := d.UnitState(ctx, unit); err == nil && st.LoadState == "not-found" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the supervisor of %s did not stop", id)
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// watcherGoroutines counts the goroutines of the detach watch loop.
func watcherGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "detach.(*Launcher).watchLoop")
		}
		buf = make([]byte, 2*len(buf))
	}
}

func waitNoWatcher(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for watcherGoroutines() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("a watcher goroutine remains with no detached operation running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TC-64: inside Launch the record is already Running and its Running audit
// record exists; spec.json is in place, mode 0600.
func TestRunningIsCommittedBeforeLaunch(t *testing.T) {
	perBackend(t, testRunningIsCommittedBeforeLaunch)
}

func testRunningIsCommittedBeforeLaunch(t *testing.T, backend detachtest.Backend) {
	type seen struct {
		status  apitypes.OperationStatus
		audited bool
		mode    os.FileMode
		err     error
	}
	got := make(chan seen, 1)
	var w *world // Set before the first launch
	// The backend's own launch runs after the checks: its supervisor really
	// starts and ends the operation.
	w = backendWorld(t, backend, func(d *detach.Options) {
		launch := d.Launch
		d.Launch = func(ctx context.Context, inv detach.Invocation) error {
			var s seen
			rec, err := w.store.Get(inv.ID)
			s.status, s.err = rec.Status, err
			for _, r := range w.records() {
				if r.OperationID == inv.ID && r.Outcome == string(apitypes.StatusRunning) {
					s.audited = true
				}
			}
			if fi, err := os.Stat(filepath.Join(inv.Dir, detach.SpecFile)); err == nil {
				s.mode = fi.Mode().Perm()
			}
			got <- s
			return launch(ctx, inv)
		}
	})
	op := w.start(who("ops", "td:write"), tdOps.job.Op(), `{"name":"one"}`, "")
	s := <-got
	if s.err != nil || s.status != apitypes.StatusRunning || !s.audited {
		t.Fatalf("inside Launch: status %s (%v), Running audited %v", s.status, s.err, s.audited)
	}
	if s.mode != 0o600 {
		t.Fatalf("spec.json mode %v at launch, want 0600", s.mode)
	}
	rec := w.waitStatus(op.ID, apitypes.StatusSucceeded)
	if rec.Exec != store.ExecDetached {
		t.Fatalf("record exec %q, want %q", rec.Exec, store.ExecDetached)
	}
}

// TC-65: with the real supervisor, Finalize receives exit.json's fields and
// the operation is terminal about a second after the child exits; once no
// detached operation runs, no watcher goroutine remains.
func TestWatcherFinalizesAndStopsWhenIdle(t *testing.T) {
	perBackend(t, testWatcherFinalizesAndStopsWhenIdle)
}

func testWatcherFinalizesAndStopsWhenIdle(t *testing.T, backend detachtest.Backend) {
	w := backendWorld(t, backend)
	w.td.onPrepare("seven", func(context.Context, thingIn) (detach.Spec, error) {
		return shJob("echo bye; exit 7"), nil
	})
	p := who("ops", "td:write")
	ok := w.start(p, tdOps.job.Op(), `{"name":"zero"}`, "")
	bad := w.start(p, tdOps.job.Op(), `{"name":"seven"}`, "")
	okRec := w.waitStatus(ok.ID, apitypes.StatusSucceeded)
	badRec := w.waitStatus(bad.ID, apitypes.StatusFailed)
	if badRec.Error == nil || badRec.Error.Slug() != problem.ScriptFailed {
		t.Fatalf("failed op error %+v", badRec.Error)
	}
	calls := w.td.finalCalls()
	byName := map[string]detach.Exit{}
	for _, c := range calls {
		byName[c.name] = c.exit
	}
	for name, rec := range map[string]store.OpRecord{"zero": okRec, "seven": badRec} {
		x, found := byName[name]
		want, _, err := detach.ReadExit(w.opDir(rec.ID))
		if !found || err != nil || x != want {
			t.Fatalf("%s: Finalize got %+v (%v), exit.json %+v (%v)", name, x, found, want, err)
		}
		if lag := rec.Ended.Sub(x.Ended); lag > 1500*time.Millisecond {
			t.Fatalf("%s: terminal %s after the child ended, want about 1s at most", name, lag)
		}
	}
	if byName["seven"].ExitCode != 7 || byName["zero"].ExitCode != 0 {
		t.Fatalf("exit codes %+v", byName)
	}
	if !strings.Contains(w.opOutput(ok.ID), "hello zero") {
		t.Fatalf("output %q", w.opOutput(ok.ID))
	}
	waitNoWatcher(t)
}

// TC-66: past MaxRuntime the operation ends Failed with outcome-unknown
// naming its unit, without a signal: the child is still alive.
func TestMaximumRuntimeReportsWithoutSignaling(t *testing.T) {
	perBackend(t, testMaximumRuntimeReportsWithoutSignaling)
}

func testMaximumRuntimeReportsWithoutSignaling(t *testing.T, backend detachtest.Backend) {
	var offset atomic.Int64
	var signals atomic.Int64
	var local detach.Options
	w := backendWorld(t, backend, func(d *detach.Options) {
		local = *d
		d.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
		d.Signal = func(ctx context.Context, unit string, sig syscall.Signal) error {
			signals.Add(1)
			return local.Signal(ctx, unit, sig)
		}
	})
	w.td.onPrepare("long", func(context.Context, thingIn) (detach.Spec, error) {
		s := shJob("echo pid=$$; exec sleep 300")
		s.MaxRuntime, s.Grace = time.Hour, 100*time.Millisecond
		return s, nil
	})
	op := w.start(who("ops", "td:write"), tdOps.job.Op(), `{"name":"long"}`, "")
	pid := w.childPID(op.ID)
	t.Cleanup(func() { stopLocal(t, local, op.ID) })
	time.Sleep(700 * time.Millisecond)
	if r, _ := w.store.Get(op.ID); r.Status != apitypes.StatusRunning {
		t.Fatalf("before max runtime: %s", r.Status)
	}
	offset.Store(int64(2 * time.Hour))
	rec := w.waitStatus(op.ID, apitypes.StatusFailed)
	// The process backend has no unit: the detail names its supervisor.
	name := "nexus-op-" + op.ID
	if backend.Name == "process" {
		name = "the supervisor process of operation " + op.ID
	}
	if rec.Error == nil || rec.Error.Slug() != problem.OutcomeUnknown || !strings.Contains(rec.Error.Detail, name) {
		t.Fatalf("error %+v, want outcome-unknown naming %s", rec.Error, name)
	}
	if !alive(pid) {
		t.Fatal("the child died: max runtime must not signal")
	}
	if n := signals.Load(); n != 0 {
		t.Fatalf("%d signals sent", n)
	}
}

// TC-ERR20: a failed launch fails the operation with a structured error,
// leaves nothing Running, removes spec.json and releases the lane.
func TestLaunchFailureFailsTheOperation(t *testing.T) {
	spy := newUnitSpy()
	spy.onLaunch = func(detach.Invocation) error {
		return errors.New("systemd-run: exit status 1: Failed to start transient service unit")
	}
	w := spyWorld(t, spy)
	p := who("ops", "td:write")
	op := w.start(p, tdOps.job.Op(), `{"name":"one","lane":"solo","policy":"reject"}`, "")
	rec := w.waitStatus(op.ID, apitypes.StatusFailed)
	if rec.Error == nil || rec.Error.Slug() != problem.InternalError || rec.Ended == nil {
		t.Fatalf("record %+v, want Failed with internal-error", rec)
	}
	if strings.Contains(rec.Error.Detail, "systemd-run") {
		t.Fatalf("the launcher's message reached the client: %q", rec.Error.Detail)
	}
	if _, err := os.Stat(filepath.Join(w.opDir(op.ID), detach.SpecFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spec.json after a failed launch: %v", err)
	}
	spy.mu.Lock()
	spy.onLaunch = newUnitSpy().onLaunch
	spy.mu.Unlock()
	next := w.startWhenFree(p, tdOps.job.Op(), `{"name":"two","lane":"solo","policy":"reject"}`)
	w.waitStatus(next.ID, apitypes.StatusSucceeded)
}

// TC-224: a cancel cannot lose a race with a dequeued operation. A cancel
// during Prepare cancels Prepare's context and no unit is launched; a
// Prepare that ignores its context and succeeds afterwards is abandoned;
// with Prepare returning concurrently, each run either ends Canceled with no
// launch or is launched and then canceled under the Running rules.
func TestCancelCannotLoseARaceWithADequeuedOperation(t *testing.T) {
	spy := newUnitSpy()
	w := spyWorld(t, spy)
	p := who("ops", "td:write")

	for _, honorCtx := range []bool{true, false} {
		name := fmt.Sprintf("gated-%v", honorCtx)
		entered := make(chan struct{})
		release := make(chan struct{})
		ctxCanceled := make(chan struct{})
		w.td.onPrepare(name, func(ctx context.Context, in thingIn) (detach.Spec, error) {
			close(entered)
			if honorCtx {
				<-ctx.Done()
				close(ctxCanceled)
				return detach.Spec{}, ctx.Err()
			}
			<-release
			return shJob("echo late"), nil
		})
		lane := "q-" + name
		op := w.start(p, tdOps.job.Op(), `{"name":"`+name+`","lane":"`+lane+`"}`, "")
		behind := w.start(p, tdOps.job.Op(), `{"name":"behind","lane":"`+lane+`"}`, "")
		<-entered
		got := w.mustCancel(p, op.ID, "")
		if got.Status != apitypes.StatusCanceled {
			t.Fatalf("honorCtx %v: cancel answered %s", honorCtx, got.Status)
		}
		if honorCtx {
			select {
			case <-ctxCanceled:
			case <-time.After(5 * time.Second):
				t.Fatal("Prepare's context was not canceled")
			}
		} else {
			close(release)
		}
		w.waitStatus(behind.ID, apitypes.StatusSucceeded)
		if r := w.waitStatus(op.ID, apitypes.StatusCanceled); r.Error != nil {
			t.Fatalf("canceled op carries %+v", r.Error)
		}
		if n := spy.launched(op.ID); n != 0 {
			t.Fatalf("honorCtx %v: %d launches of a canceled operation", honorCtx, n)
		}
	}

	// Step 3: launches are not finished by the spy, so a launched
	// operation ends only through the cancel signal.
	spy.mu.Lock()
	spy.onLaunch = func(detach.Invocation) error { return nil }
	spy.mu.Unlock()
	const rounds = 200
	type round struct{ id, answer string }
	results := make([]round, rounds)
	for i := range rounds {
		name := fmt.Sprintf("race-%d", i)
		entered := make(chan struct{})
		release := make(chan struct{})
		w.td.onPrepare(name, func(context.Context, thingIn) (detach.Spec, error) {
			close(entered)
			<-release
			return shJob("echo race"), nil
		})
		op := w.start(p, tdOps.job.Op(), `{"name":"`+name+`"}`, "")
		<-entered
		var wg sync.WaitGroup
		// Jitter spreads Prepare's return across the cancel's pipeline
		// (authorization, the fsynced audit record, the compare-and-set),
		// so both sides of the race are taken.
		jitter := time.Duration(i%25) * time.Millisecond
		wg.Go(func() {
			time.Sleep(jitter)
			close(release)
		})
		got := w.mustCancel(p, op.ID, "")
		wg.Wait()
		results[i] = round{id: op.ID, answer: string(got.Status)}
	}
	queuedWins, runningWins := 0, 0
	for i, r := range results {
		rec := w.waitStatus(r.id, apitypes.StatusCanceled)
		launches, signals := spy.launched(r.id), spy.signaled(r.id)
		switch apitypes.OperationStatus(r.answer) {
		case apitypes.StatusCanceled:
			queuedWins++
			if launches != 0 || len(signals) != 0 {
				t.Fatalf("round %d: Canceled while queued, yet %d launches and signals %v", i, launches, signals)
			}
		case apitypes.StatusRunning:
			runningWins++
			if launches != 1 || len(signals) != 1 || signals[0] != syscall.SIGTERM {
				t.Fatalf("round %d: canceled while running with %d launches and signals %v", i, launches, signals)
			}
		default:
			t.Fatalf("round %d: cancel answered %s", i, r.answer)
		}
		if rec.Error != nil {
			t.Fatalf("round %d: %+v", i, rec.Error)
		}
	}
	t.Logf("%d cancels won while queued, %d after the launch", queuedWins, runningWins)
}

// Cancel through the unit: a running detached operation is canceled by
// SIGTERM to its supervisor, which stops the child's group and reports
// canceled, so the operation ends Canceled.
func TestCancelARunningDetachedOperationThroughItsUnit(t *testing.T) {
	perBackend(t, testCancelARunningDetachedOperationThroughItsUnit)
}

func testCancelARunningDetachedOperationThroughItsUnit(t *testing.T, backend detachtest.Backend) {
	var sent atomic.Int64
	w := backendWorld(t, backend, func(d *detach.Options) {
		inner := d.Signal
		d.Signal = func(ctx context.Context, unit string, sig syscall.Signal) error {
			sent.Add(1)
			return inner(ctx, unit, sig)
		}
	})
	w.td.onPrepare("long", func(context.Context, thingIn) (detach.Spec, error) {
		return shJob("echo pid=$$; while :; do sleep 0.1; done"), nil
	})
	p := who("ops", "td:write")
	op := w.start(p, tdOps.job.Op(), `{"name":"long"}`, "")
	pid := w.childPID(op.ID)
	if got := w.mustCancel(p, op.ID, ""); got.Status != apitypes.StatusRunning {
		t.Fatalf("cancel answered %s, want the op as it is (Running)", got.Status)
	}
	rec := w.waitStatus(op.ID, apitypes.StatusCanceled)
	if rec.Error != nil || sent.Load() != 1 {
		t.Fatalf("record %+v, %d signals", rec, sent.Load())
	}
	x, ok, err := detach.ReadExit(w.opDir(op.ID))
	if err != nil || !ok || !x.Canceled {
		t.Fatalf("exit.json %+v, %v, %v", x, ok, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatal("the child survived the cancel")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(w.td.finalCalls()) != 0 {
		t.Fatal("Finalize ran for a canceled operation")
	}
}

// Sensitive values reach the child only through spec.json, and Finalize
// runs on the record's redacted input.
func TestDetachedSensitiveInputStaysOutOfFinalize(t *testing.T) {
	perBackend(t, testDetachedSensitiveInputStaysOutOfFinalize)
}

func testDetachedSensitiveInputStaysOutOfFinalize(t *testing.T, backend detachtest.Backend) {
	w := backendWorld(t, backend)
	var specEnv []string
	w.td.onPrepare("secretive", func(_ context.Context, in thingIn) (detach.Spec, error) {
		s := shJob("true")
		s.Env = append(s.Env, "SECRET="+in.Secret)
		specEnv = s.Env
		return s, nil
	})
	op := w.start(who("ops", "td:write"), tdOps.job.Op(), `{"name":"secretive","secret":"hunter2"}`, "")
	w.waitStatus(op.ID, apitypes.StatusSucceeded)
	if !strings.Contains(strings.Join(specEnv, " "), "SECRET=hunter2") {
		t.Fatalf("Prepare saw env %v", specEnv)
	}
	w.td.mu.Lock()
	inputs := append([]thingIn(nil), w.td.inputs...)
	w.td.mu.Unlock()
	if len(inputs) != 1 || inputs[0].Secret == "hunter2" {
		t.Fatalf("Finalize inputs %+v: the sensitive value must not reach it", inputs)
	}
}
