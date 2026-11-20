// Tests of planning/services-provider/test-cases.md TC-SV-8 to TC-SV-20,
// TC-SV-E2, TC-SV-E3, TC-SV-E4, TC-SV-E19 and TC-SV-ERR6 (WI 1.3): the
// operation manager's reboot rules R1 to R5 (D-3) and Manager.Active.

package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// F-BOOT: the boot IDs the manager's Options.BootID seam answers.
const (
	bootPrev = "11111111-1111-1111-1111-111111111111"
	bootCur  = "22222222-2222-2222-2222-222222222222"
)

// rbOut is the reboot test op's result: AfterReboot sets Rebooted (and
// UpAt when asked), Finalize sets Finalized.
type rbOut struct {
	apitypes.ActionResult
	Rebooted  bool       `json:"rebooted"`
	Finalized bool       `json:"finalized,omitempty"`
	UpAt      *time.Time `json:"upAt,omitempty"`
}

// rbOp is TC-SV-1's shape: a long-running destructive provider action with
// a reconnect window that declares CompletesAcrossReboot.
var rbOp = def.Define[thingIn, rbOut](def.Op{
	ID: "rb.actions.reboot", Provider: "rb", Kind: def.KindProviderAction, Action: "reboot",
	Summary: "s", Description: "Reboot test operation for the operation manager tests.",
	Scope: def.Scope{Name: "rb:reboot"}, Risk: def.RiskDestructive, Mode: def.ModeLongRunning,
	Cancel: def.CancelRunning, CompletesAcrossReboot: true, Refine: refineThing,
	CLI: def.CLIBinding{Noun: "machine", Verb: "restart", ReconnectWindow: 10 * time.Minute},
})

// rbProv is the planted provider of rbOp. Its lane is `reboot` with the
// reject policy unless the input names another; Prepare is scripted per
// thing name and by default runs until stopped.
type rbProv struct {
	mu        sync.Mutex
	prepare   map[string]func(in thingIn) detach.Spec
	afters    []provider.Rebooted
	finals    int
	afterErr  error
	copyUpAt  bool   // AfterReboot copies ReadyAt into UpAt
	duringAft func() // Runs inside AfterReboot
}

func newRBProv() *rbProv { return &rbProv{prepare: map[string]func(thingIn) detach.Spec{}} }

func (p *rbProv) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "rb", ReadScope: "rb:read", Ops: []def.Op{rbOp.Op()}}
}
func (p *rbProv) Configure(func(any) error) error { return nil }
func (p *rbProv) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (p *rbProv) onPrepare(name string, f func(in thingIn) detach.Spec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prepare[name] = f
}

func (p *rbProv) calls() (afters []provider.Rebooted, finals int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.afters), p.finals
}

// longJob prints its pid and runs until stopped.
func longJob() detach.Spec { return shJob("echo pid=$$; while :; do sleep 0.1; done") }

func (p *rbProv) Bindings() []provider.Binding {
	return []provider.Binding{provider.Detached(rbOp, provider.DetachedSpec[thingIn, rbOut]{
		Lane: func(_ context.Context, in thingIn) (provider.Lane, error) {
			if in.Lane != "" {
				return laneOf(in), nil
			}
			return provider.Lane{Key: "reboot", Policy: provider.LaneReject}, nil
		},
		Prepare: func(_ context.Context, in thingIn, _ *provider.Run) (detach.Spec, error) {
			p.mu.Lock()
			f := p.prepare[in.Name]
			p.mu.Unlock()
			if f != nil {
				return f(in), nil
			}
			return longJob(), nil
		},
		Finalize: func(context.Context, thingIn, detach.Exit, *provider.Run) (rbOut, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.finals++
			return rbOut{Finalized: true}, nil
		},
		LostDetail: func(_ *provider.Run, in thingIn) string { return "lost detail for " + in.Name },
		AfterReboot: func(_ context.Context, _ thingIn, rb provider.Rebooted, _ *provider.Run) (rbOut, error) {
			p.mu.Lock()
			p.afters = append(p.afters, rb)
			err, during, upAt := p.afterErr, p.duringAft, p.copyUpAt
			p.mu.Unlock()
			if during != nil {
				during()
			}
			if err != nil {
				return rbOut{}, err
			}
			out := rbOut{Rebooted: true}
			if upAt {
				out.UpAt = &rb.ReadyAt
			}
			return out, nil
		},
	})}
}

// rbWorld is a world whose registry also serves rbProv, and whose manager
// reads its boot ID through the F-BOOT seam: boot, or an error when empty.
type rbWorld struct {
	*world
	rb   *rbProv
	boot atomic.Pointer[string]
	// unbound registers the world without rbProv, as a restart with the
	// provider disabled does (CR-6).
	unbound bool
}

var rbWho = who("ops", "rb:reboot", "td:write", "test:write")

// newRBWorld builds the stack as newWorld does, with rbProv and the F-BOOT
// seam answering boot; tune, when given, adjusts the manager's options.
func newRBWorld(t *testing.T, boot string, tune ...func(*ops.Options)) *rbWorld {
	t.Helper()
	w := &world{t: t, stateDir: t.TempDir(), auditDir: t.TempDir(), tokDir: t.TempDir(), logs: &syncBuffer{}, seq: &seq{}}
	rw := &rbWorld{world: w, rb: newRBProv()}
	rw.setBoot(boot)
	w.tuneOpts = func(o *ops.Options) {
		o.BootID = func() (string, error) {
			if b := *rw.boot.Load(); b != "" {
				return b, nil
			}
			return "", errors.New("planted")
		}
		for _, f := range tune {
			f(o)
		}
	}
	w.log = slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w.tp = newTestProv("tp", "test:read", tpOps)
	w.td = newDetProv()
	w.tq = newTestProv("tq", "other:read", tqOps)
	alog, err := audit.Open(audit.Options{Dir: w.auditDir, Host: "testhost", Logger: w.log})
	if err != nil {
		t.Fatal(err)
	}
	w.alog = alog
	w.aud = &faultyAudit{Log: alog, seq: w.seq}
	rw.open()
	t.Cleanup(w.close)
	return rw
}

func (rw *rbWorld) setBoot(b string) { rw.boot.Store(&b) }

// open is world.open with rbProv registered.
func (rw *rbWorld) open() {
	w, t := rw.world, rw.t
	t.Helper()
	st, err := store.Open(w.stateDir, store.Options{Now: w.now})
	if err != nil {
		t.Fatal(err)
	}
	w.store = st
	opts := ops.Options{Store: &spyStore{Store: st, seq: w.seq}, Audit: w.aud,
		Dir: filepath.Join(w.stateDir, "ops"), Host: "testhost", Logger: w.log, PollInterval: 20 * time.Millisecond}
	w.tuneOpts(&opts)
	if w.mgr, err = ops.New(opts); err != nil {
		t.Fatal(err)
	}
	provs := []provider.Provider{w.tp, w.tq, w.td, rw.rb}
	cfgs := map[string]core.ProviderConfig{"tp": {Enabled: true}, "tq": {Enabled: true}, "td": {Enabled: true}, "rb": {Enabled: true}}
	if rw.unbound {
		provs, cfgs = provs[:3], map[string]core.ProviderConfig{"tp": {Enabled: true}, "tq": {Enabled: true}, "td": {Enabled: true}}
	}
	w.reg, err = core.New(provs, core.Options{
		Host: "testhost", Logger: w.log, Audit: w.alog, Ops: w.mgr, Providers: cfgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.disp = core.NewDispatcher(w.reg, core.DispatcherOptions{Logger: w.log, Audit: w.aud, NoteLimitPerMinute: 1 << 20})
}

// reboot closes the world as a host shutdown stops nexusd, runs between
// with nexusd down, and reopens it in boot. Recovery is the caller's.
func (rw *rbWorld) reboot(boot string, between func()) {
	rw.closeState()
	if between != nil {
		between()
	}
	rw.setBoot(boot)
	rw.open()
}

func (rw *rbWorld) startRB(name string) apitypes.Operation {
	rw.t.Helper()
	return rw.start(rbWho, rbOp.Op(), `{"name":"`+name+`"}`, "")
}

// running waits for operation id's child to print its pid, and arranges for
// its supervisor and its child's group to be stopped after the test.
func (rw *rbWorld) running(seams *detach.Options, id string) int {
	rw.t.Helper()
	pid := rw.childPID(id)
	rw.t.Cleanup(func() {
		stopLocal(rw.t, *seams, id)
		_ = syscall.Kill(-pid, syscall.SIGKILL) // The group may be gone
	})
	return pid
}

func (rw *rbWorld) record(id, boot string, at time.Time) {
	rw.t.Helper()
	if err := detach.RecordReboot(rw.opDir(id), detach.RebootRecord{BootID: boot, RequestedAt: at}); err != nil {
		rw.t.Fatal(err)
	}
}

// corruptRecord writes a reboot record that cannot be read (TC-SV-ERR4).
func (rw *rbWorld) corruptRecord(id string) {
	rw.t.Helper()
	if err := os.WriteFile(filepath.Join(rw.opDir(id), detach.RebootFile), []byte(`{"bootId":`), 0o600); err != nil {
		rw.t.Fatal(err)
	}
}

// rewriteExitBoot rewrites exit.json's bootId raw, as the supervisor of
// that boot wrote it.
func (rw *rbWorld) rewriteExitBoot(id, boot string) {
	rw.t.Helper()
	path := filepath.Join(rw.opDir(id), detach.ExitFile)
	b, err := os.ReadFile(path)
	if err != nil {
		rw.t.Fatal(err)
	}
	var x map[string]any
	if err := json.Unmarshal(b, &x); err != nil {
		rw.t.Fatal(err)
	}
	x["bootId"] = boot
	if b, err = json.Marshal(x); err != nil {
		rw.t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		rw.t.Fatal(err)
	}
}

// waitExit waits for operation id's exit.json.
func (rw *rbWorld) waitExit(id string) detach.Exit {
	rw.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		x, ok, err := detach.ReadExit(rw.opDir(id))
		if err == nil && ok {
			return x
		}
		if time.Now().After(deadline) {
			rw.t.Fatalf("operation %s wrote no exit.json: %v", id, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitEnd polls the store until id is terminal, at most limit.
func (rw *rbWorld) waitEnd(id string, limit time.Duration) store.OpRecord {
	rw.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		rec, err := rw.store.Get(id)
		if err == nil && rec.Status.Terminal() {
			return rec
		}
		if time.Now().After(deadline) {
			rw.t.Fatalf("operation %s: %s (%v) after %v, want terminal", id, rec.Status, err, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// terminalRecord is the terminal state-change audit record of id.
func (rw *rbWorld) terminalRecord(id string) systemspec.AuditRecord {
	rw.t.Helper()
	var found []systemspec.AuditRecord
	for _, r := range rw.records() {
		if r.OperationID == id && apitypes.OperationStatus(r.Outcome).Terminal() {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		rw.t.Fatalf("terminal state-change records of %s: %+v, want one", id, found)
	}
	return found[0]
}

// wantRebootFacts checks the R4 facts of id's terminal audit record.
func (rw *rbWorld) wantRebootFacts(id string) systemspec.AuditRecord {
	rw.t.Helper()
	r := rw.terminalRecord(id)
	if r.Facts["previousBootId"] != bootPrev || r.Facts["bootId"] != bootCur {
		rw.t.Fatalf("terminal audit record of %s: facts %v, want previousBootId=%s bootId=%s", id, r.Facts, bootPrev, bootCur)
	}
	return r
}

// stillRunning checks that id is Running, with no terminal audit record.
func (rw *rbWorld) stillRunning(id string) {
	rw.t.Helper()
	rec, err := rw.store.Get(id)
	if err != nil || rec.Status != apitypes.StatusRunning || rec.Error != nil {
		rw.t.Fatalf("operation %s: %+v (%v), want Running", id, rec, err)
	}
	for _, r := range rw.records() {
		if r.OperationID == id && apitypes.OperationStatus(r.Outcome).Terminal() {
			rw.t.Fatalf("operation %s has a terminal audit record %+v", id, r)
		}
	}
}

func wantNoHooks(t *testing.T, rb *rbProv) {
	t.Helper()
	if afters, finals := rb.calls(); len(afters) != 0 || finals != 0 {
		t.Fatalf("AfterReboot calls %+v, Finalize calls %d; want none", afters, finals)
	}
}

// wantAfterReboot checks rec ended Succeeded through exactly one
// AfterReboot from bootPrev to bootCur, with no Finalize.
func wantAfterReboot(t *testing.T, rb *rbProv, rec store.OpRecord, requested time.Time) provider.Rebooted {
	t.Helper()
	if rec.Status != apitypes.StatusSucceeded || rec.Error != nil {
		t.Fatalf("operation %s: %s %+v, want Succeeded", rec.ID, rec.Status, rec.Error)
	}
	afters, finals := rb.calls()
	if len(afters) != 1 || finals != 0 {
		t.Fatalf("AfterReboot calls %+v, Finalize calls %d; want one and none", afters, finals)
	}
	got := afters[0]
	if got.PreviousBootID != bootPrev || got.BootID != bootCur || !got.RequestedAt.Equal(requested) {
		t.Fatalf("Rebooted %+v, want %s to %s requested at %v", got, bootPrev, bootCur, requested)
	}
	return got
}

// requestedAt is a fixed request time in the past, as the helper recorded it.
func requestedAt() time.Time { return time.Now().UTC().Truncate(time.Second).Add(-time.Minute) }

// interruptAcrossReboot runs TC-SV-8's steps 1 to 5: the op is started in
// bootPrev, its reboot recorded, nexusd stopped, then the unit interrupted
// (SIGTERM, or SIGKILL with lost) as a shutdown does; nexusd starts in
// bootCur and recovers. It returns the op, the request time, and the
// window in which recovery began.
func interruptAcrossReboot(t *testing.T, rw *rbWorld, seams *detach.Options, lost bool) (
	op apitypes.Operation, requested, before, after time.Time) {
	t.Helper()
	op = rw.startRB("r")
	rw.running(seams, op.ID)
	requested = requestedAt()
	rw.record(op.ID, bootPrev, requested)
	rw.reboot(bootCur, func() {
		unit := detach.Unit(op.ID)
		sig := syscall.SIGTERM
		if lost {
			sig = syscall.SIGKILL
		}
		if err := seams.Signal(context.Background(), unit, sig); err != nil {
			t.Fatal(err)
		}
		waitUnitGone(t, *seams, unit)
		if !lost {
			rw.rewriteExitBoot(op.ID, bootPrev)
		}
	})
	before = time.Now()
	done := rw.recoverNow()
	after = time.Now()
	waitClosed(t, done, 15*time.Second, "classification")
	return op, requested, before, after
}

// TC-SV-8 (FR-11, FR-16, CON-4, CON-5): an interrupted unit with a record
// from an earlier boot completes Succeeded through AfterReboot.
func TestTCSV8_R4AnInterruptedUnitWithARecord(t *testing.T) {
	perBackend(t, testTCSV8)
}

func testTCSV8(t *testing.T, backend detachtest.Backend) {
	var seams detach.Options
	rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
	op, requested, before, after := interruptAcrossReboot(t, rw, &seams, false)
	rec := rw.waitEnd(op.ID, 15*time.Second)
	got := wantAfterReboot(t, rw.rb, rec, requested)
	if string(rec.Result) != `{"changed":false,"rebooted":true}` {
		t.Fatalf("result %s, want rebooted", rec.Result)
	}
	if got.ReadyAt.Before(before) || got.ReadyAt.After(after) {
		t.Fatalf("ReadyAt %v, want when recovery began, in [%v, %v]", got.ReadyAt, before, after)
	}
	rw.wantRebootFacts(op.ID)
}

// TC-SV-9 (FR-11, CON-4): a lost unit with a record from an earlier boot
// completes Succeeded through AfterReboot after the 2 s lost re-check.
func TestTCSV9_R4ALostUnitWithARecord(t *testing.T) {
	perBackend(t, testTCSV9)
}

func testTCSV9(t *testing.T, backend detachtest.Backend) {
	var seams detach.Options
	rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
	op, requested, before, _ := interruptAcrossReboot(t, rw, &seams, true)
	rec := rw.waitEnd(op.ID, 15*time.Second)
	wantAfterReboot(t, rw.rb, rec, requested)
	if took := rec.Ended.Sub(before); took < detach.RecheckDelay-50*time.Millisecond {
		t.Fatalf("ended %v after recovery began, before the lost re-check", took)
	}
	rw.wantRebootFacts(op.ID)
}

// TC-SV-10 (FR-11): an AfterReboot error ends the operation Failed with
// that problem, or the problem problemOf gives a plain error.
func TestTCSV10_R4AnAfterRebootErrorEndsTheOperation(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		slug   problem.Slug
		detail string
	}{
		{"problem", problem.New(problem.ValidationFailed, "planted"), problem.ValidationFailed, "planted"},
		// problemOf keeps a plain error's text in the log only.
		{"plain error", errors.New("planted"), problem.InternalError, "the operation failed with an internal error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			perBackend(t, func(t *testing.T, backend detachtest.Backend) {
				var seams detach.Options
				rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
				rw.rb.afterErr = c.err
				op, _, _, _ := interruptAcrossReboot(t, rw, &seams, false)
				rec := rw.waitEnd(op.ID, 15*time.Second)
				if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != c.slug || rec.Error.Detail != c.detail {
					t.Fatalf("operation: %s %+v, want Failed %s %q", rec.Status, rec.Error, c.slug, c.detail)
				}
				if afters, finals := rw.rb.calls(); len(afters) != 1 || finals != 0 {
					t.Fatalf("AfterReboot calls %d, Finalize calls %d", len(afters), finals)
				}
				if r := rw.wantRebootFacts(op.ID); r.Problem != string(c.slug) {
					t.Fatalf("terminal audit record problem %q, want %s", r.Problem, c.slug)
				}
			})
		})
	}
}

// TC-SV-11 (FR-11, CON-4): a normal exit with a record present goes to
// Finalize, never AfterReboot, live and after a restart in a new boot.
func TestTCSV11_R5ANormalExitWithARecord(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "live"
		if recovery {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			perBackend(t, func(t *testing.T, backend detachtest.Backend) {
				var seams detach.Options
				rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
				goOn := filepath.Join(t.TempDir(), "go-on")
				rw.rb.onPrepare("fin", func(thingIn) detach.Spec {
					return shJob("echo pid=$$; while [ ! -e " + goOn + " ]; do sleep 0.05; done; exit 0")
				})
				op := rw.startRB("fin")
				rw.running(&seams, op.ID)
				rw.record(op.ID, bootPrev, requestedAt())
				release := func() {
					if err := os.WriteFile(goOn, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if recovery {
					rw.reboot(bootCur, func() {
						release()
						waitUnitGone(t, seams, detach.Unit(op.ID))
						rw.rewriteExitBoot(op.ID, bootPrev)
					})
					waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
				} else {
					release()
				}
				rec := rw.waitEnd(op.ID, 15*time.Second)
				afters, finals := rw.rb.calls()
				if len(afters) != 0 || finals != 1 {
					t.Fatalf("AfterReboot calls %d, Finalize calls %d; want none and one", len(afters), finals)
				}
				if rec.Status != apitypes.StatusSucceeded || string(rec.Result) != `{"changed":false,"rebooted":false,"finalized":true}` {
					t.Fatalf("operation: %s %s %+v, want Finalize's result", rec.Status, rec.Result, rec.Error)
				}
			})
		})
	}
}

// heldInThisBoot runs TC-SV-12's steps 1 to 3 in bootPrev with the world
// open: the op is started, its reboot recorded, and its supervisor
// SIGTERMed, so the watcher sees exit.json interrupted.
func heldInThisBoot(t *testing.T, rw *rbWorld, seams *detach.Options, name string) (apitypes.Operation, time.Time) {
	t.Helper()
	op := rw.startRB(name)
	rw.running(seams, op.ID)
	requested := requestedAt()
	rw.record(op.ID, bootPrev, requested)
	if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if x := rw.waitExit(op.ID); !x.Interrupted {
		t.Fatalf("exit.json %+v, want interrupted", x)
	}
	time.Sleep(3*detach.PollInterval + time.Second)
	return op, requested
}

// TC-SV-12 (FR-11, CON-4): an interrupted exit with a record from this boot
// is held Running, and the lane stays held.
func TestTCSV12_R3AnInterruptedExitWithARecord(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op, _ := heldInThisBoot(t, rw, &seams, "r")
		rw.stillRunning(op.ID)
		wantNoHooks(t, rw.rb)
		_, err := rw.call(rbWho, rbOp.Op(), `{"name":"second"}`, "")
		if pe := wantProblem(t, err, problem.Conflict); pe.Extensions["blockingOperationId"] != op.ID {
			t.Fatalf("409 %+v, want blockingOperationId %s", pe, op.ID)
		}
	})
}

// TC-SV-13 (FR-11, CON-4): the hold ends at shutdown with the record still
// Running, and the next boot completes it.
func TestTCSV13_R3TheHoldEndsAtShutdownWith(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		baseline := watcherGoroutines()
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op, requested := heldInThisBoot(t, rw, &seams, "r")
		begin := time.Now()
		rw.closeState() // Fails the test when Shutdown passes its deadline
		if took := time.Since(begin); took > 5*time.Second {
			t.Fatalf("Shutdown took %v with an operation held", took)
		}
		deadline := time.Now().Add(5 * time.Second)
		for watcherGoroutines() > baseline {
			if time.Now().After(deadline) {
				t.Fatalf("%d watcher goroutines remain after Shutdown, baseline %d", watcherGoroutines(), baseline)
			}
			time.Sleep(20 * time.Millisecond)
		}
		rw.setBoot(bootCur)
		rw.open()
		if rec, err := rw.store.Get(op.ID); err != nil || rec.Status != apitypes.StatusRunning {
			t.Fatalf("after Shutdown: %+v (%v), want Running", rec, err)
		}
		waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
		wantAfterReboot(t, rw.rb, rw.waitEnd(op.ID, 15*time.Second), requested)
		rw.wantRebootFacts(op.ID)
	})
}

// heldUntilMaxRuntime starts the op with a MaxRuntime of 2 s, records its
// reboot in bootPrev, and SIGTERMs its supervisor (TC-SV-14 step 1).
func heldUntilMaxRuntime(t *testing.T, rw *rbWorld, seams *detach.Options) (apitypes.Operation, time.Time) {
	t.Helper()
	rw.rb.onPrepare("short", func(thingIn) detach.Spec {
		s := longJob()
		s.MaxRuntime = 2 * time.Second
		return s
	})
	rw.rb.onPrepare("next", func(thingIn) detach.Spec { return shJob("true") })
	op := rw.startRB("short")
	rw.running(seams, op.ID)
	requested := requestedAt()
	rw.record(op.ID, bootPrev, requested)
	if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	return op, requested
}

// TC-SV-14 (FR-11, A-14): the hold ends outcome-unknown at MaxRuntime when
// the boot never changes, and the lane is then released.
func TestTCSV14_R3TheHoldEndsOutcomeUnknownAt(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op, requested := heldUntilMaxRuntime(t, rw, &seams)
		rec := rw.waitEnd(op.ID, 5*time.Second)
		wantOutcomeUnknown(t, rec, requested.Format(time.RFC3339))
		for _, want := range []string{bootPrev, "did not restart"} {
			if !strings.Contains(rec.Error.Detail, want) {
				t.Fatalf("detail %q lacks %q", rec.Error.Detail, want)
			}
		}
		if rec.Ended.Before(rec.Started.Add(2 * time.Second)) {
			t.Fatalf("committed at %v, before started %v + 2s", rec.Ended, rec.Started)
		}
		if afters, _ := rw.rb.calls(); len(afters) != 0 {
			t.Fatalf("AfterReboot called %d times", len(afters))
		}
		next := rw.startWhenFree(rbWho, rbOp.Op(), `{"name":"next"}`)
		rw.waitEnd(next.ID, 15*time.Second)
	})
}

// TC-SV-15 (FR-10, FR-11): a canceled exit after the record is held, not
// Canceled, and the next boot completes it (A-28).
func TestTCSV15_R3ACanceledExitAfterTheRecord(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op := rw.startRB("r")
		rw.running(&seams, op.ID)
		// The cancel R2 accepted before the record existed.
		if err := detach.RequestCancel(rw.opDir(op.ID)); err != nil {
			t.Fatal(err)
		}
		requested := requestedAt()
		rw.record(op.ID, bootPrev, requested)
		if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if x := rw.waitExit(op.ID); !x.Canceled {
			t.Fatalf("exit.json %+v, want canceled", x)
		}
		time.Sleep(3*detach.PollInterval + time.Second)
		rw.stillRunning(op.ID)
		wantNoHooks(t, rw.rb)
		rw.reboot(bootCur, nil)
		waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
		wantAfterReboot(t, rw.rb, rw.waitEnd(op.ID, 15*time.Second), requested)
		rw.wantRebootFacts(op.ID)
	})
}

// TC-SV-16 (FR-11, CON-4): recovery in the same boot holds a recorded
// operation in its own goroutine, outside the classification deadline, for
// what is left of its MaxRuntime.
func TestTCSV16_RecoveryInTheSameBootHoldsA(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		rw.rb.onPrepare("r", func(thingIn) detach.Spec {
			s := longJob()
			s.MaxRuntime = 4 * time.Second
			return s
		})
		rw.td.onPrepare("t", func(context.Context, thingIn) (detach.Spec, error) { return longJob(), nil })
		rb := rw.startRB("r")
		td := rw.start(rbWho, tdOps.job.Op(), `{"name":"t"}`, "")
		rw.running(&seams, rb.ID)
		rw.running(&seams, td.ID)
		requested := requestedAt()
		rw.record(rb.ID, bootPrev, requested)
		rw.reboot(bootPrev, func() {
			for _, id := range []string{rb.ID, td.ID} {
				if err := seams.Signal(context.Background(), detach.Unit(id), syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				waitUnitGone(t, seams, detach.Unit(id))
			}
		})
		waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
		if rec, err := rw.store.Get(rb.ID); err != nil || rec.Status != apitypes.StatusRunning {
			t.Fatalf("rb once classification is over: %+v (%v), want Running (held)", rec, err)
		}
		wantOutcomeUnknown(t, rw.waitEnd(td.ID, 15*time.Second), "lost detail for t")
		rec := rw.waitEnd(rb.ID, 15*time.Second)
		wantOutcomeUnknown(t, rec, "did not restart")
		if !strings.Contains(rec.Error.Detail, requested.Format(time.RFC3339)) || !strings.Contains(rec.Error.Detail, bootPrev) {
			t.Fatalf("detail %q does not name the request time and boot", rec.Error.Detail)
		}
		if end := rec.Started.Add(4 * time.Second); rec.Ended.Before(end) || rec.Ended.After(end.Add(1500*time.Millisecond)) {
			t.Fatalf("rb ended at %v, want at its start %v + 4s, not 4s after recovery", rec.Ended, rec.Started)
		}
		wantNoHooks(t, rw.rb)
	})
}

// endScenario is one {interrupted, lost} × {live, recovery} case of
// TC-SV-17 and TC-SV-18.
type endScenario struct {
	lost, recovery bool
}

func (s endScenario) String() string {
	how, when := "interrupted", "same boot live"
	if s.lost {
		how = "lost"
	}
	if s.recovery {
		when = "next boot recovery"
	}
	return how + ", " + when
}

var endScenarios = []endScenario{{false, false}, {true, false}, {false, true}, {true, true}}

// endWithoutResult ends each op's unit as s says: SIGTERM (interrupted) or
// SIGKILL (lost), with the world open, or with nexusd stopped and restarted
// in bootCur.
func endWithoutResult(t *testing.T, rw *rbWorld, seams *detach.Options, s endScenario, ids ...string) {
	t.Helper()
	end := func() {
		sig := syscall.SIGTERM
		if s.lost {
			sig = syscall.SIGKILL
		}
		for _, id := range ids {
			if err := seams.Signal(context.Background(), detach.Unit(id), sig); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range ids {
			waitUnitGone(t, *seams, detach.Unit(id))
		}
	}
	if !s.recovery {
		end()
		return
	}
	rw.reboot(bootCur, end)
	waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
}

// normalized is an outcome-unknown detail with the operation's ID replaced,
// so two operations' details compare.
func normalized(rec store.OpRecord) string {
	if rec.Error == nil {
		return ""
	}
	return strings.ReplaceAll(rec.Error.Detail, rec.ID, "<id>")
}

// TC-SV-17 (FR-11, CON-4): without a record, a declaring operation's
// interrupted and lost ends stay outcome-unknown with today's detail,
// compared with the td op in the same scenario. TC-SV-18 (CON-4): a td op,
// which does not declare the flag, with a stray record ends the same.
func TestTCSV17_WithoutARecordADeclaringOperationsInterrupted(t *testing.T) {
	for _, s := range endScenarios {
		t.Run(s.String(), func(t *testing.T) {
			perBackend(t, func(t *testing.T, backend detachtest.Backend) {
				var seams detach.Options
				rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
				rw.td.onPrepare("x", func(context.Context, thingIn) (detach.Spec, error) { return longJob(), nil })
				rb := rw.startRB("x")
				td := rw.start(rbWho, tdOps.job.Op(), `{"name":"x"}`, "")
				rw.running(&seams, rb.ID)
				rw.running(&seams, td.ID)
				endWithoutResult(t, rw, &seams, s, rb.ID, td.ID)
				rbRec, tdRec := rw.waitEnd(rb.ID, 30*time.Second), rw.waitEnd(td.ID, 30*time.Second)
				wantOutcomeUnknown(t, rbRec, "lost detail for x")
				if normalized(rbRec) != normalized(tdRec) {
					t.Fatalf("rb detail %q, td detail %q: want today's detail", normalized(rbRec), normalized(tdRec))
				}
				wantNoHooks(t, rw.rb)
			})
		})
	}
}

// TC-SV-18 (CON-4): an operation that does not declare the flag ignores a
// stray record file: its ends are byte-identical to those of the same op
// without the file, and its cancel is not refused.
func TestTCSV18_AnOperationThatDoesNotDeclareThe(t *testing.T) {
	for _, s := range endScenarios {
		t.Run(s.String(), func(t *testing.T) {
			perBackend(t, func(t *testing.T, backend detachtest.Backend) {
				var seams detach.Options
				rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
				rw.td.onPrepare("x", func(context.Context, thingIn) (detach.Spec, error) { return longJob(), nil })
				stray := rw.start(rbWho, tdOps.job.Op(), `{"name":"x"}`, "")
				plain := rw.start(rbWho, tdOps.job.Op(), `{"name":"x"}`, "")
				rw.running(&seams, stray.ID)
				rw.running(&seams, plain.ID)
				rw.record(stray.ID, bootPrev, requestedAt())
				endWithoutResult(t, rw, &seams, s, stray.ID, plain.ID)
				strayRec, plainRec := rw.waitEnd(stray.ID, 30*time.Second), rw.waitEnd(plain.ID, 30*time.Second)
				wantOutcomeUnknown(t, strayRec, "lost detail for x")
				if normalized(strayRec) != normalized(plainRec) {
					t.Fatalf("with the stray record %q, without %q", normalized(strayRec), normalized(plainRec))
				}
			})
		})
	}
	t.Run("cancel", func(t *testing.T) {
		perBackend(t, func(t *testing.T, backend detachtest.Backend) {
			var seams detach.Options
			rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
			rw.td.onPrepare("x", func(context.Context, thingIn) (detach.Spec, error) { return longJob(), nil })
			op := rw.start(rbWho, tdOps.job.Op(), `{"name":"x"}`, "")
			rw.running(&seams, op.ID)
			rw.record(op.ID, bootPrev, requestedAt())
			if got := rw.mustCancel(rbWho, op.ID, ""); got.Status != apitypes.StatusRunning {
				t.Fatalf("cancel answered %s, want Running", got.Status)
			}
			if rec := rw.waitEnd(op.ID, 15*time.Second); rec.Status != apitypes.StatusCanceled || rec.Error != nil {
				t.Fatalf("after the cancel: %s %+v, want Canceled", rec.Status, rec.Error)
			}
		})
	})
}

// rbSpyWorld is an rbWorld whose launcher is spy, whose launches never
// finish on their own.
func rbSpyWorld(t *testing.T, spy *unitSpy, boot string) *rbWorld {
	var opsDir atomic.Value
	spy.onLaunch = func(detach.Invocation) error { return nil }
	return newRBWorld(t, boot, func(o *ops.Options) {
		opsDir.Store(o.Dir)
		withDetach(t, spy.options(func() string { return opsDir.Load().(string) }))(o)
	})
}

// launchedRB starts rbOp with input and waits for its launch.
func (rw *rbWorld) launchedRB(spy *unitSpy, input string) apitypes.Operation {
	rw.t.Helper()
	op := rw.startWhenFree(rbWho, rbOp.Op(), input)
	deadline := time.Now().Add(10 * time.Second)
	for spy.launched(op.ID) == 0 {
		if time.Now().After(deadline) {
			rw.t.Fatalf("operation %s was never launched", op.ID)
		}
		time.Sleep(5 * time.Millisecond)
	}
	rw.waitStatus(op.ID, apitypes.StatusRunning)
	return op
}

// TC-SV-19 (FR-10, FR-11): R2: a cancel is accepted before the record, and
// answers 409 not-cancelable once it exists or when it cannot be read,
// sending neither a cancel request nor a signal.
func TestTCSV19_R2CancelAnswers409NotCancelableOnce(t *testing.T) {
	spy := newUnitSpy()
	rw := rbSpyWorld(t, spy, bootPrev)

	a := rw.launchedRB(spy, `{"name":"a"}`)
	if got := rw.mustCancel(rbWho, a.ID, ""); got.Status != apitypes.StatusRunning {
		t.Fatalf("A: cancel answered %s, want Running", got.Status)
	}
	if rec := rw.waitEnd(a.ID, 15*time.Second); rec.Status != apitypes.StatusCanceled {
		t.Fatalf("A: %s %+v, want Canceled", rec.Status, rec.Error)
	}

	b := rw.launchedRB(spy, `{"name":"b"}`)
	requested := requestedAt()
	rw.record(b.ID, bootPrev, requested)
	c := rw.launchedRB(spy, `{"name":"c","lane":"other","policy":"reject"}`)
	rw.corruptRecord(c.ID)

	for _, x := range []struct {
		id, detail string
	}{
		{b.ID, "the reboot was already requested at " + requested.Format(time.RFC3339)},
		{c.ID, "the reboot record cannot be read; the reboot may have been requested"},
	} {
		_, err := rw.cancel(rbWho, x.id, "")
		pe := wantProblem(t, err, problem.NotCancelable)
		if pe.Status != http.StatusConflict || pe.Extensions["operationId"] != x.id || !strings.Contains(pe.Detail, x.detail) {
			t.Fatalf("cancel of %s: %+v, want 409 not-cancelable naming it with %q", x.id, pe, x.detail)
		}
		rw.stillRunning(x.id)
		if sigs := spy.signaled(x.id); len(sigs) != 0 {
			t.Fatalf("operation %s was signaled %v", x.id, sigs)
		}
		if ok, err := detach.CancelRequested(rw.opDir(x.id)); err != nil || ok {
			t.Fatalf("operation %s has a cancel request (%v, %v)", x.id, ok, err)
		}
	}
	refused := filterRecs(rw.records(), func(r systemspec.AuditRecord) bool {
		return r.Action == cancelOp && r.Outcome == audit.OutcomeRefused && r.Problem == string(problem.NotCancelable)
	})
	var ids []string
	for _, r := range refused {
		ids = append(ids, r.OperationID)
	}
	if !slices.Equal(ids, []string{b.ID, c.ID}) {
		t.Fatalf("refused not-cancelable records for %q, want B then C", ids)
	}
}

// TC-SV-20 (FR-10): Manager.Active lists exactly the Queued and Running
// operations.
func TestTCSV20_ManagerActiveListsExactlyTheQueuedAnd(t *testing.T) {
	w := newWorld(t)
	p := who("ops", "test:write")
	g := w.tp.gated("first")
	defer g.open()
	first := w.start(p, tpOps.run.Op(), `{"name":"first","lane":"q"}`, "")
	g.waitStarted(t)
	queued := w.start(p, tpOps.run.Op(), `{"name":"second","lane":"q"}`, "")
	seeded := w.seed(tdRec("seeded", store.Lane{}), apitypes.StatusRunning)
	done := w.start(p, tpOps.run.Op(), `{"name":"done"}`, "")
	w.waitStatus(done.ID, apitypes.StatusSucceeded)
	failed := w.seed(tpRec("failed", store.Lane{}), apitypes.StatusRunning)
	if _, err := w.store.CommitTerminal(failed, apitypes.StatusRunning, apitypes.StatusFailed, nil,
		problem.New(problem.InternalError, "seeded")); err != nil {
		t.Fatal(err)
	}
	canceled := w.seed(tpRec("canceled", store.Lane{}), apitypes.StatusQueued)
	if _, err := w.store.CompareAndSetStatus(canceled, apitypes.StatusQueued, apitypes.StatusCanceled); err != nil {
		t.Fatal(err)
	}

	got, err := w.mgr.Active()
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b provider.ActiveOperation) int { return strings.Compare(a.ID, b.ID) })
	want := []provider.ActiveOperation{
		{ID: first.ID, Op: tpOps.run.Op().ID, Status: apitypes.StatusRunning},
		{ID: queued.ID, Op: tpOps.run.Op().ID, Status: apitypes.StatusQueued},
		{ID: seeded, Op: tdOps.job.Op().ID, Status: apitypes.StatusRunning},
	}
	slices.SortFunc(want, func(a, b provider.ActiveOperation) int { return strings.Compare(a.ID, b.ID) })
	if !slices.Equal(got, want) {
		t.Fatalf("Active() = %+v, want %+v", got, want)
	}
}

// TC-SV-E2 (FR-11): ReadyAt is the time recovery began, not the commit
// time.
func TestTCSVE2_ReadyAtIsTheTimeRecoveryBeganNot(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		var rw *rbWorld // Set before the manager reads the clock
		rw = newRBWorld(t, bootPrev, withBackend(t, backend, &seams), func(o *ops.Options) {
			o.Now = func() time.Time { return rw.now() }
		})
		t1 := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
		rw.rb.copyUpAt = true
		rw.rb.duringAft = func() { rw.setClock(t1.Add(10 * time.Second)) }
		op := rw.startRB("r")
		rw.running(&seams, op.ID)
		requested := requestedAt()
		rw.record(op.ID, bootPrev, requested)
		rw.reboot(bootCur, func() {
			if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			waitUnitGone(t, seams, detach.Unit(op.ID))
		})
		rw.setClock(t1)
		waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
		got := wantAfterReboot(t, rw.rb, rw.waitEnd(op.ID, 15*time.Second), requested)
		if !got.ReadyAt.Equal(t1) {
			t.Fatalf("ReadyAt %v, want %v", got.ReadyAt, t1)
		}
		rec, _ := rw.store.Get(op.ID)
		var out rbOut
		if err := json.Unmarshal(rec.Result, &out); err != nil || out.UpAt == nil || !out.UpAt.Equal(t1) {
			t.Fatalf("result %s (%v): upAt want %v", rec.Result, err, t1)
		}
	})
}

// TC-SV-E3 (FR-11, A-28): a cancel accepted just before the record ends
// Succeeded after the reboot, and the audit record says a cancel was
// requested.
func TestTCSVE3_ACancelAcceptedAfterTheRecordEnds(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		// The helper handles the stop itself and goes on to the trigger;
		// the supervisor kills it after the spec's Grace.
		rw.rb.onPrepare("r", func(thingIn) detach.Spec {
			return shJob(`trap "" TERM; echo pid=$$; while :; do sleep 0.1; done`)
		})
		op := rw.startRB("r")
		rw.running(&seams, op.ID)
		if got := rw.mustCancel(rbWho, op.ID, ""); got.Status != apitypes.StatusRunning {
			t.Fatalf("cancel answered %s, want accepted (Running)", got.Status)
		}
		requested := requestedAt()
		rw.record(op.ID, bootPrev, requested)
		if x := rw.waitExit(op.ID); !x.Canceled {
			t.Fatalf("exit.json %+v, want canceled", x)
		}
		time.Sleep(3 * detach.PollInterval)
		rw.stillRunning(op.ID)
		rw.reboot(bootCur, nil)
		waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
		wantAfterReboot(t, rw.rb, rw.waitEnd(op.ID, 15*time.Second), requested)
		if r := rw.wantRebootFacts(op.ID); r.Facts["cancelRequested"] != "true" {
			t.Fatalf("terminal audit facts %v, want cancelRequested=true", r.Facts)
		}
	})
}

// TC-SV-E4 (FR-10, FR-11): the lane is released only when the held
// operation commits: every earlier attempt is 409, and the first admitted
// attempt comes after the commit.
func TestTCSVE4_TheLaneIsReleasedOnlyWhenThe(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op, _ := heldUntilMaxRuntime(t, rw, &seams)
		deadline := time.Now().Add(10 * time.Second)
		refused := 0
		for {
			res, err := rw.call(rbWho, rbOp.Op(), `{"name":"next"}`, "")
			if err == nil {
				if res.Status != http.StatusAccepted {
					t.Fatalf("admitted with %d", res.Status)
				}
				rec, gerr := rw.store.Get(op.ID)
				if gerr != nil || !rec.Status.Terminal() {
					t.Fatalf("a second op was admitted while the first reads %s (%v)", rec.Status, gerr)
				}
				rw.waitEnd(res.OperationID, 15*time.Second)
				break
			}
			if pe := wantProblem(t, err, problem.Conflict); pe.Extensions["blockingOperationId"] != op.ID {
				t.Fatalf("409 %+v, want blockingOperationId %s", pe, op.ID)
			}
			refused++
			if time.Now().After(deadline) {
				t.Fatal("never admitted")
			}
			time.Sleep(200 * time.Millisecond)
		}
		if refused == 0 {
			t.Fatal("admitted at once: the lane was not held")
		}
		wantOutcomeUnknown(t, rw.waitEnd(op.ID, time.Second), "did not restart")
	})
}

// TC-SV-E19 (FR-11): a record that cannot be read ends the op
// outcome-unknown at once with the read error, never held, never
// Succeeded, in a new boot (R4) and in the same one (R3).
func TestTCSVE19_ARecordWhoseBootIDCannotBe(t *testing.T) {
	wantUnreadable := func(t *testing.T, rec store.OpRecord) {
		t.Helper()
		wantOutcomeUnknown(t, rec, detach.RebootFile)
		if !strings.Contains(rec.Error.Detail, "unexpected end of JSON input") {
			t.Fatalf("detail %q does not name the read error", rec.Error.Detail)
		}
	}
	t.Run("R4", func(t *testing.T) {
		perBackend(t, func(t *testing.T, backend detachtest.Backend) {
			var seams detach.Options
			rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
			op := rw.startRB("r")
			rw.running(&seams, op.ID)
			rw.corruptRecord(op.ID)
			rw.reboot(bootCur, func() {
				if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				waitUnitGone(t, seams, detach.Unit(op.ID))
			})
			waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
			wantUnreadable(t, rw.waitEnd(op.ID, 15*time.Second))
			wantNoHooks(t, rw.rb)
		})
	})
	t.Run("R3", func(t *testing.T) {
		perBackend(t, func(t *testing.T, backend detachtest.Backend) {
			var seams detach.Options
			rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
			rw.rb.onPrepare("short", func(thingIn) detach.Spec {
				s := longJob()
				s.MaxRuntime = 2 * time.Second
				return s
			})
			rw.rb.onPrepare("next", func(thingIn) detach.Spec { return shJob("true") })
			op := rw.startRB("short")
			rw.running(&seams, op.ID)
			rw.corruptRecord(op.ID)
			if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			x := rw.waitExit(op.ID)
			rec := rw.waitEnd(op.ID, 5*time.Second)
			wantUnreadable(t, rec)
			// The watcher reads exit.json every PollInterval; past that the
			// commit takes milliseconds, and a hold would last until
			// MaxRuntime.
			if lag := rec.Ended.Sub(x.Ended); lag > detach.PollInterval+500*time.Millisecond {
				t.Fatalf("terminal %v after the exit, want at once", lag)
			}
			if !rec.Ended.Before(rec.Started.Add(2 * time.Second)) {
				t.Fatalf("terminal at %v, not before MaxRuntime from %v", rec.Ended, rec.Started)
			}
			wantNoHooks(t, rw.rb)
			next := rw.startWhenFree(rbWho, rbOp.Op(), `{"name":"next"}`)
			rw.waitEnd(next.ID, 15*time.Second)
		})
	})
}

// TC-SV-ERR6 (FR-11): R1: a declaring op is not launched when this boot's
// ID cannot be read; an op without the flag runs normally.
func TestTCSVERR6_R1ADeclaringOpIsNotLaunched(t *testing.T) {
	spy := newUnitSpy()
	var opsDir atomic.Value
	rw := newRBWorld(t, "", func(o *ops.Options) {
		opsDir.Store(o.Dir)
		withDetach(t, spy.options(func() string { return opsDir.Load().(string) }))(o)
	})
	op := rw.startRB("r")
	rec := rw.waitEnd(op.ID, 15*time.Second)
	const detail = "the boot ID cannot be read, so a reboot could not be confirmed"
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.InternalError || rec.Error.Detail != detail {
		t.Fatalf("operation: %s %+v, want Failed internal-error %q", rec.Status, rec.Error, detail)
	}
	if n := spy.launched(op.ID); n != 0 {
		t.Fatalf("%d launches of the declaring op", n)
	}
	plain := rw.start(rbWho, tdOps.job.Op(), `{"name":"plain"}`, "")
	if rec := rw.waitEnd(plain.ID, 15*time.Second); rec.Status != apitypes.StatusSucceeded || spy.launched(plain.ID) != 1 {
		t.Fatalf("plain op: %s %+v, %d launches; want Succeeded after one", rec.Status, rec.Error, spy.launched(plain.ID))
	}
}

// CR-6 (services-provider code review): the fail-closed branches of the
// manager's reboot rules that no TC reached.

// A record is present at a start whose boot ID cannot be read: the unit is
// lost, so the reboot cannot be confirmed. Without the m.boot == "" case the
// record's boot would differ from the empty one and AfterReboot would mark
// an unconfirmed reboot Succeeded.
func TestCR6_ARecordAtAStartWhoseBootIDCannotBeReadIsOutcomeUnknown(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op := rw.startRB("r")
		rw.running(&seams, op.ID)
		rw.record(op.ID, bootPrev, requestedAt())
		rw.reboot("", func() {
			if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			waitUnitGone(t, seams, detach.Unit(op.ID))
		})
		waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
		rec := rw.waitEnd(op.ID, 15*time.Second)
		wantOutcomeUnknown(t, rec, "this boot's ID cannot be read")
		wantNoHooks(t, rw.rb)
	})
}

// An operation whose binding is gone at the restart (its provider disabled)
// has no reboot rules; a record from an earlier boot must still not make it
// Succeeded: it ends outcome-unknown, with no AfterReboot.
func TestCR6_ARecoveredRebootWhoseBindingIsGoneIsOutcomeUnknown(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op := rw.startRB("r")
		rw.running(&seams, op.ID)
		rw.record(op.ID, bootPrev, requestedAt())
		rw.unbound = true
		rw.reboot(bootCur, func() {
			if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			waitUnitGone(t, seams, detach.Unit(op.ID))
		})
		waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
		wantOutcomeUnknown(t, rw.waitEnd(op.ID, 15*time.Second), "")
		wantNoHooks(t, rw.rb)
	})
}

// reattached starts the op in bootPrev, restarts nexusd in the same boot
// with the unit still live, and recovers: the manager's entry is a
// restored one.
func reattached(t *testing.T, rw *rbWorld, seams *detach.Options) (op apitypes.Operation, pid int) {
	t.Helper()
	op = rw.startRB("r")
	pid = rw.running(seams, op.ID)
	rw.reboot(bootPrev, nil)
	waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
	rw.stillRunning(op.ID)
	return op, pid
}

// R2 holds for an entry rebuilt by Restore: once the record exists a cancel
// answers 409 not-cancelable, and sends neither a cancel request nor a
// signal.
func TestCR6_R2CoversAReattachedReboot(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op, pid := reattached(t, rw, &seams)
		requested := requestedAt()
		rw.record(op.ID, bootPrev, requested)
		_, err := rw.cancel(rbWho, op.ID, "")
		pe := wantProblem(t, err, problem.NotCancelable)
		if pe.Status != http.StatusConflict || !strings.Contains(pe.Detail, "the reboot was already requested at "+requested.Format(time.RFC3339)) {
			t.Fatalf("cancel: %+v, want 409 not-cancelable naming the request", pe)
		}
		if ok, err := detach.CancelRequested(rw.opDir(op.ID)); err != nil || ok {
			t.Fatalf("a cancel request was written (%v, %v)", ok, err)
		}
		time.Sleep(3 * detach.PollInterval)
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("the operation's process was signaled: %v", err)
		}
		rw.stillRunning(op.ID)
	})
}

// A reattached operation whose unit ends interrupted with this boot's
// record is held Running, with its lane, not ended.
func TestCR6_AReattachedRebootThatEndsInterruptedIsHeld(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op, _ := reattached(t, rw, &seams)
		rw.record(op.ID, bootPrev, requestedAt())
		if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if x := rw.waitExit(op.ID); !x.Interrupted {
			t.Fatalf("exit.json %+v, want interrupted", x)
		}
		time.Sleep(3*detach.PollInterval + time.Second)
		rw.stillRunning(op.ID)
		wantNoHooks(t, rw.rb)
		_, err := rw.call(rbWho, rbOp.Op(), `{"name":"second"}`, "")
		if pe := wantProblem(t, err, problem.Conflict); pe.Extensions["blockingOperationId"] != op.ID {
			t.Fatalf("409 %+v, want blockingOperationId %s", pe, op.ID)
		}
	})
}

// A hold that recovery started (the unit ended before nexusd restarted, in
// the same boot) ends at shutdown with the record Running, and the next
// boot completes it through AfterReboot (R4).
func TestCR6_ARecoveredHoldEndsAtShutdownAndCompletesNextBoot(t *testing.T) {
	perBackend(t, func(t *testing.T, backend detachtest.Backend) {
		var seams detach.Options
		rw := newRBWorld(t, bootPrev, withBackend(t, backend, &seams))
		op := rw.startRB("r")
		rw.running(&seams, op.ID)
		requested := requestedAt()
		rw.record(op.ID, bootPrev, requested)
		if err := seams.Signal(context.Background(), detach.Unit(op.ID), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if x := rw.waitExit(op.ID); !x.Interrupted {
			t.Fatalf("exit.json %+v, want interrupted", x)
		}
		rw.reboot(bootPrev, nil) // nexusd restarts in the same boot
		waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
		time.Sleep(time.Second)
		rw.stillRunning(op.ID)
		wantNoHooks(t, rw.rb)
		rw.reboot(bootCur, nil) // The hold ends at shutdown; the host rebooted
		waitClosed(t, rw.recoverNow(), 15*time.Second, "classification")
		wantAfterReboot(t, rw.rb, rw.waitEnd(op.ID, 15*time.Second), requested)
		rw.wantRebootFacts(op.ID)
	})
}
