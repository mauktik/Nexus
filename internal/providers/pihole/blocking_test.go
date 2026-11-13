//go:build pihole_red

// Stub-style: SKIP-only (WI 3.3). Excluded from `make test` by the
// pihole_red build tag; the WI 3.3 developer deletes the //go:build line and
// these must pass. Cases: TC-77 to TC-81, TC-E10, TC-ERR14, TC-ERR15 in
// planning/pihole-dns-zones/test-cases.md. Shared helpers are in
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
	"slices"
	"testing"
	"time"

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
	h, m := newV5(t)
	ctx := context.Background()
	m.StopBlocking(ctx, 60*time.Second, &bytes.Buffer{})
	got, _ := m.StopBlocking(ctx, 120*time.Second, &bytes.Buffer{})
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
	m.StopBlocking(ctx, 60*time.Second, &bytes.Buffer{})
	if got, _ := m.StopBlocking(ctx, 0, &bytes.Buffer{}); got.ReenableAt != nil {
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
	h, m := newV5(t)
	ctx := context.Background()
	m.StopBlocking(ctx, 60*time.Second, &bytes.Buffer{})
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

// TC-81 (live-state, FR-6/FR-8): the blocking read reflects state A, then
// the changed state A', then the re-enabled state, without caching.
func TestTC81_BlockingReadFollowsState(t *testing.T) {
	h, m := newV5(t)
	r := &v5.Reader{Paths: h.Paths(), Runner: h, DNSServer: h.FTL().Addr()}
	ctx := context.Background()
	if b, err := r.Blocking(ctx); err != nil || b.State != "enabled" || b.ReenableAt != nil {
		t.Fatalf("state A: %+v %v", b, err)
	}
	stop, _ := m.StopBlocking(ctx, 60*time.Second, &bytes.Buffer{})
	if b, _ := r.Blocking(ctx); b.State != "disabled" || b.ReenableAt == nil || !b.ReenableAt.Equal(*stop.ReenableAt) {
		t.Fatalf("state A': %+v, want disabled until %v", b, stop.ReenableAt)
	}
	h.EndUnit()
	h.Advance(61 * time.Second)
	if b, _ := r.Blocking(ctx); b.State != "enabled" || b.ReenableAt != nil {
		t.Fatalf("after re-enable: %+v", b)
	}
}
