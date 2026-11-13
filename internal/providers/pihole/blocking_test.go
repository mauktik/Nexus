// Cases: TC-77 to TC-81, TC-E10, TC-ERR14, TC-ERR15 in
// planning/pihole-dns-zones/test-cases.md (WI 3.3): the v5 blocking
// methods, and the stop-blocking and start-blocking helper actions run
// through Prepare, the helper and Finalize. Shared helpers are in
// service_test.go (WI 3.2).
//
// Regression for the verified v5 hazard "timed disable re-enable lifetime"
// (V-13, FR-8): `pihole disable Ns` starts its re-enable child inside the op
// unit's cgroup, which KillMode dies with. FakeHost encodes that (TC-70b), so
// these tests fail if the provider ever relies on the native child.
package pihole

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

func indexOf(argvs [][]string, pred func([]string) bool) int {
	return slices.IndexFunc(argvs, pred)
}

func isDisable(a []string) bool { return len(a) >= 2 && a[0] == piholeBin && a[1] == "disable" }
func isTimer(a []string) bool   { return len(a) >= 1 && a[0] == systemdRun }

// TC-77: an indefinite stop runs `pihole disable` with no argument and
// creates no timer.
func TestTC77_IndefiniteStop(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	got, err := m.StopBlocking(context.Background(), 0, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "disabled" || got.ReenableAt != nil || h.BlockingEnabled() != "false" {
		t.Errorf("state %+v, BLOCKING_ENABLED=%s", got, h.BlockingEnabled())
	}
	if hasArgv(h, isTimer) {
		t.Error("indefinite stop created a timer")
	}
	if i := indexOf(h.Argvs(), isDisable); i < 0 || len(h.Argvs()[i]) != 2 {
		t.Errorf("argvs %q: want `pihole disable` with no argument", h.Argvs())
	}
}

// TC-78 (regression: timed-disable lifetime): a 60 s stop creates the
// transient timer before disabling, returns reenableAt, and blocking comes
// back on after the op unit has ended, with no nexusd involvement.
func TestTC78_TimedStopSurvivesUnitEnd(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	start := h.Now()
	got, err := m.StopBlocking(context.Background(), 60*time.Second, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "disabled" || got.ReenableAt == nil || !got.ReenableAt.Equal(start.Add(60*time.Second)) {
		t.Fatalf("result %+v, want disabled with reenableAt %v", got, start.Add(60*time.Second))
	}
	ti, di := indexOf(h.Argvs(), isTimer), indexOf(h.Argvs(), isDisable)
	if ti < 0 || di < 0 || ti > di {
		t.Errorf("argvs %q: want systemd-run before pihole disable", h.Argvs())
	}
	if !slices.Contains(h.Argvs()[ti], "--on-active=60s") || h.Argvs()[ti][len(h.Argvs()[ti])-1] != "enable" {
		t.Errorf("timer argv %q", h.Argvs()[ti])
	}
	h.EndUnit()
	h.Advance(59 * time.Second)
	if h.BlockingEnabled() != "false" {
		t.Fatal("re-enabled early")
	}
	h.Advance(2 * time.Second)
	if h.BlockingEnabled() != "true" {
		t.Fatal("blocking still off after the duration: the re-enable died with the op unit")
	}
}

// TC-E10: no code path ever passes a duration to `pihole disable`.
func TestTCE10_NeverNativeTimedDisable(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	for _, d := range []time.Duration{0, time.Second, 90 * time.Second, 24 * time.Hour} {
		if _, err := m.StopBlocking(context.Background(), d, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.StartBlocking(context.Background(), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range h.Argvs() {
		if isDisable(a) && len(a) != 2 {
			t.Errorf("recorded %q: a native timed disable dies with the op unit", a)
		}
	}
}

// TC-79: a new stop replaces the pending re-enable: a later time, or none for
// an indefinite stop.
func TestTC79_SecondStopReplacesTimer(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	ctx := context.Background()
	if _, err := m.StopBlocking(ctx, 60*time.Second, &bytes.Buffer{}); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	got, err := m.StopBlocking(ctx, 120*time.Second, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if got.ReenableAt == nil {
		t.Fatalf("second stop %+v: want reenableAt set", got)
	}
	h.EndUnit()
	if at, ok := h.TimerPending(); !ok || !at.Equal(*got.ReenableAt) {
		t.Fatalf("pending %v %v, want the second stop's %v", at, ok, got.ReenableAt)
	}
	h.Advance(61 * time.Second)
	if h.BlockingEnabled() != "false" {
		t.Fatal("the replaced 60 s timer still fired")
	}
	h.Advance(60 * time.Second)
	if h.BlockingEnabled() != "true" {
		t.Fatal("the 120 s timer did not fire")
	}

	h, m = newV5(t)
	if _, err := m.StopBlocking(ctx, 60*time.Second, &bytes.Buffer{}); err != nil {
		t.Fatalf("timed stop: %v", err)
	}
	got, err = m.StopBlocking(ctx, 0, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("indefinite stop: %v", err)
	}
	if got.ReenableAt != nil {
		t.Errorf("indefinite stop kept reenableAt %v", got.ReenableAt)
	}
	h.EndUnit()
	h.Advance(5 * time.Minute)
	if _, ok := h.TimerPending(); ok || h.BlockingEnabled() != "false" {
		t.Error("indefinite stop did not cancel the earlier timer")
	}
}

// TC-80: start cancels a pending re-enable and enables blocking; start with
// no timer tolerates systemctl's exit 5.
func TestTC80_StartCancelsTimer(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	ctx := context.Background()
	if _, err := m.StopBlocking(ctx, 60*time.Second, &bytes.Buffer{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	got, err := m.StartBlocking(ctx, &bytes.Buffer{})
	if err != nil || got.State != "enabled" || got.ReenableAt != nil {
		t.Fatalf("start = %+v, %v", got, err)
	}
	if _, ok := h.TimerPending(); ok {
		t.Error("timer still pending after start")
	}
	if _, err := m.StartBlocking(ctx, &bytes.Buffer{}); err != nil {
		t.Errorf("start with no timer: %v (systemctl stop exit 5 must be tolerated)", err)
	}
}

// TC-ERR14: a failed timer creation fails the stop with blocking still on and
// `pihole disable` never run.
func TestTCERR14_TimerFailureLeavesBlockingOn(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	h.FailCommand([]string{systemdRun}, 1)
	_, err := m.StopBlocking(context.Background(), 60*time.Second, &bytes.Buffer{})
	slug, pe := slugOf(t, err)
	if slug != "pihole-command-failed" || pe.Extensions["command"] != "systemd-run" {
		t.Errorf("got %s %+v", slug, pe.Extensions)
	}
	if h.BlockingEnabled() != "true" || hasArgv(h, isDisable) {
		t.Errorf("BLOCKING_ENABLED=%s argvs %q: blocking must stay on", h.BlockingEnabled(), h.Argvs())
	}
}

// TC-ERR15 (V-13: disable always exits 0): a read-back that is not false
// fails the stop and removes the timer it created.
func TestTCERR15_DisableReadBack(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	h.IgnoreCommand([]string{piholeBin, "disable"})
	_, err := m.StopBlocking(context.Background(), 60*time.Second, &bytes.Buffer{})
	slug, pe := slugOf(t, err)
	if slug != "pihole-command-failed" || pe.Extensions["command"] != "pihole disable" {
		t.Errorf("got %s %+v", slug, pe.Extensions)
	}
	if _, ok := h.TimerPending(); ok {
		t.Error("timer left pending after a failed stop")
	}
}

// A reset-failed that exits non-zero does not fail the stop, and is not
// silent: one op-log line names it (review CR-1; the lead's ruling is that no
// command's result is dropped with `_`).
func TestStopBlockingLogsAFailedResetFailed(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	h.FailCommand([]string{systemctlBin, "reset-failed"}, 1)
	var out bytes.Buffer
	if _, err := m.StopBlocking(context.Background(), 60*time.Second, &out); err != nil {
		t.Fatalf("stop: %v", err)
	}
	const want = "pihole: systemctl reset-failed nexus-pihole-reenable.timer nexus-pihole-reenable.service exited 1; continuing"
	if !strings.Contains(out.String(), want) {
		t.Errorf("op log lacks %q:\n%s", want, out.String())
	}
	if h.BlockingEnabled() != "false" {
		t.Errorf("BLOCKING_ENABLED=%s, want false: the failed reset must not fail the stop", h.BlockingEnabled())
	}
}

// TC-81 (live-state, FR-6/FR-8): the blocking read reflects state A, then
// the changed state A', then the re-enabled state, without caching.
func TestTC81_BlockingReadFollowsState(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	r := &v5.Reader{Paths: h.Paths(), Runner: h, DNSServer: h.FTL().Addr()}
	ctx := context.Background()
	if b, err := r.Blocking(ctx); err != nil || b.State != "enabled" || b.ReenableAt != nil {
		t.Fatalf("state A: %+v %v", b, err)
	}
	stop, err := m.StopBlocking(ctx, 60*time.Second, &bytes.Buffer{})
	if err != nil || stop.ReenableAt == nil {
		t.Fatalf("stop: %+v %v", stop, err)
	}
	if b, err := r.Blocking(ctx); err != nil || b.State != "disabled" || b.ReenableAt == nil || !b.ReenableAt.Equal(*stop.ReenableAt) {
		t.Fatalf("state A': %+v %v, want disabled until %v", b, err, stop.ReenableAt)
	}
	h.EndUnit()
	h.Advance(61 * time.Second)
	if b, err := r.Blocking(ctx); err != nil || b.State != "enabled" || b.ReenableAt != nil {
		t.Fatalf("after re-enable: %+v %v", b, err)
	}
}

// runBlockingOp runs op's binding as the core would: Prepare, the helper on
// the operation's directory, then Finalize.
func runBlockingOp[In any](w *helperWorld, op def.Typed[In, spec.BlockingResult], in In, id string) (spec.BlockingResult, string, error) {
	w.t.Helper()
	df := detachedBinding(w, op.Op().ID)
	ctx := context.Background()
	if _, err := df.Prepare(ctx, in, provider.NewRun(id, nil)); err != nil {
		w.t.Fatalf("Prepare: %v", err)
	}
	var out bytes.Buffer
	if code := runHelper(ctx, filepath.Join(w.opsDir, id), &out, &out, w.inject()); code != 0 {
		w.t.Fatalf("helper exit %d: %s", code, out.String())
	}
	res, err := df.Finalize(ctx, in, detach.Exit{}, provider.NewRun(id, nil))
	if err != nil {
		return spec.BlockingResult{}, out.String(), err
	}
	return res.(spec.BlockingResult), out.String(), nil
}

// detachedBinding is the provider's own detached binding of op ID id.
func detachedBinding(w *helperWorld, id string) provider.DetachedFuncs {
	w.t.Helper()
	for _, b := range w.p.Bindings() {
		if b.Op().ID == id {
			df, ok := b.Detached()
			if !ok {
				w.t.Fatalf("%s is not detached", id)
			}
			return df
		}
	}
	w.t.Fatalf("%s is not bound", id)
	return provider.DetachedFuncs{}
}

// TC-78, TC-80, action half (FR-8): stop-blocking with durationSeconds and
// start-blocking run through the helper return the state read back, with
// reenableAt and changed; a repeated indefinite stop and a start with
// blocking already on report changed false.
func TestBlockingActions(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	start := w.h.Now()
	res, out, err := runBlockingOp(w, spec.StopBlocking, spec.StopBlockingInput{DurationSeconds: 60}, "op-stop")
	if err != nil {
		t.Fatalf("stop-blocking: %v\n%s", err, out)
	}
	if !res.Changed || res.State != "disabled" || res.ReenableAt == nil || !res.ReenableAt.Equal(start.Add(time.Minute)) {
		t.Errorf("stop result %+v, want changed, disabled until %v", res, start.Add(time.Minute))
	}
	for _, a := range w.h.Argvs() {
		if isDisable(a) && len(a) != 2 {
			t.Errorf("recorded %q: a native timed disable", a)
		}
	}
	if !strings.Contains(out, "pihole: running pihole disable") {
		t.Errorf("output lacks the disable step:\n%s", out)
	}

	res, out, err = runBlockingOp(w, spec.StopBlocking, spec.StopBlockingInput{}, "op-stop-2")
	if err != nil || !res.Changed || res.State != "disabled" || res.ReenableAt != nil {
		t.Errorf("indefinite stop cancelling the timer: %+v, %v; want changed, disabled, no reenableAt\n%s", res, err, out)
	}
	res, out, err = runBlockingOp(w, spec.StopBlocking, spec.StopBlockingInput{}, "op-stop-3")
	if err != nil || res.Changed || res.State != "disabled" {
		t.Errorf("repeated indefinite stop: %+v, %v; want changed false\n%s", res, err, out)
	}

	res, out, err = runBlockingOp(w, spec.StartBlocking, spec.NoInput{}, "op-start")
	if err != nil || !res.Changed || res.State != "enabled" || res.ReenableAt != nil {
		t.Errorf("start: %+v, %v; want changed, enabled\n%s", res, err, out)
	}
	res, out, err = runBlockingOp(w, spec.StartBlocking, spec.NoInput{}, "op-start-2")
	if err != nil || res.Changed || res.State != "enabled" {
		t.Errorf("start with blocking on: %+v, %v; want changed false\n%s", res, err, out)
	}
}

// TC-ERR14, action half: a timer-creation failure reaches Finalize as
// pihole-command-failed{command: systemd-run}, with blocking still on.
func TestStopBlockingActionTimerFailure(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	w.h.FailCommand([]string{systemdRun}, 1)
	_, out, err := runBlockingOp(w, spec.StopBlocking, spec.StopBlockingInput{DurationSeconds: 60}, "op-fail")
	slug, pe := slugOf(t, err)
	if slug != problem.PiholeCommandFailed || pe.Extensions["command"] != "systemd-run" || pe.Extensions["exitCode"] != 1 {
		t.Errorf("got %v %+v\n%s", slug, pe.Extensions, out)
	}
	if w.h.BlockingEnabled() != "true" {
		t.Errorf("BLOCKING_ENABLED=%s, want true", w.h.BlockingEnabled())
	}
}

// stop-blocking and start-blocking are bound detached with MaxRuntime 10 min
// and the blocking LostDetail, which a lost unit's outcome-unknown carries.
func TestBlockingBindings(t *testing.T) {
	t.Parallel()
	const lost = "blocking may be off; a re-enable timer nexus-pihole-reenable.timer may exist; run `piholeblocking get`"
	check := func(id string, in any) {
		w := newHelperWorld(t)
		df := detachedBinding(w, id)
		run := provider.NewRun("op-lost", nil)
		s, err := df.Prepare(context.Background(), in, run)
		if err != nil || s.MaxRuntime != 10*time.Minute {
			t.Errorf("%s: Prepare MaxRuntime %s, %v; want 10m", id, s.MaxRuntime, err)
		}
		if df.LostDetail == nil || df.LostDetail(run, in) != lost {
			t.Errorf("%s: LostDetail is not %q", id, lost)
		}
		_, err = df.Finalize(context.Background(), in, detach.Exit{ExitCode: -1, Signal: "SIGKILL"}, run)
		if slug, pe := slugOf(t, err); slug != problem.OutcomeUnknown || !strings.Contains(pe.Detail, lost) {
			t.Errorf("%s: lost unit: %v %q; want outcome-unknown containing %q", id, slug, pe.Detail, lost)
		}
	}
	check(spec.StopBlocking.Op().ID, spec.StopBlockingInput{DurationSeconds: 60})
	check(spec.StartBlocking.Op().ID, spec.NoInput{})
}
