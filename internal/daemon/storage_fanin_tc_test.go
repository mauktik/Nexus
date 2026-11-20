// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (storage-provider)
//
// Tests of planning/storage-provider/test-cases.md TC-STG-35, TC-STG-36, TC-STG-37, TC-STG-38, TC-STG-39, TC-STG-40, TC-STG-41, TC-STG-43.
// Owner WIs: 2.1 (the provider.HealthSource fan-in, internal/daemon/health.go).

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
)

// fHealth is F-HEALTH (test-strategy.md): a planted HealthSource whose
// HealthSignals runs a scripted function per call (a report, an error, a
// panic, or a block that ignores ctx) and whose Preflight is scripted per
// call. It counts both calls separately and reports each HealthSignals
// return on returned. When its name has a probe op (a real config section),
// it serves that op, so a daemon config file can enable it.
type fHealth struct {
	name string

	mu        sync.Mutex
	preflight func(n int64) provider.Status                // Nil: enabled
	health    func(n int64) (provider.HealthReport, error) // n counts calls from 1

	preflights atomic.Int64
	signals    atomic.Int64
	returned   chan int64
}

var _ provider.HealthSource = (*fHealth)(nil)

func newFHealth(name string) *fHealth {
	return &fHealth{name: name, returned: make(chan int64, 256)}
}

func (f *fHealth) Spec() def.ProviderSpec {
	s := def.ProviderSpec{Name: f.name, ReadScope: f.name + ":read"}
	if op, ok := probeOps[f.name]; ok {
		s.Ops = []def.Op{op.Op()}
	}
	return s
}

func (f *fHealth) Configure(decode func(any) error) error {
	var c struct {
		Enabled bool `yaml:"enabled"`
	}
	return decode(&c)
}

func (f *fHealth) Preflight(context.Context) provider.Status {
	n := f.preflights.Add(1)
	f.mu.Lock()
	pf := f.preflight
	f.mu.Unlock()
	if pf == nil {
		return ok()
	}
	return pf(n)
}

func (f *fHealth) Bindings() []provider.Binding {
	op, found := probeOps[f.name]
	if !found {
		return nil
	}
	return []provider.Binding{provider.Sync(op, func(_ context.Context, in probeIn) (probeOut, error) {
		return probeOut(in), nil
	})}
}

func (f *fHealth) HealthSignals(context.Context) (provider.HealthReport, error) {
	n := f.signals.Add(1)
	f.mu.Lock()
	h := f.health
	f.mu.Unlock()
	defer func() { f.returned <- n }()
	return h(n)
}

func (f *fHealth) script(h func(n int64) (provider.HealthReport, error)) {
	f.mu.Lock()
	f.health = h
	f.mu.Unlock()
}

func (f *fHealth) scriptPreflight(pf func(n int64) provider.Status) {
	f.mu.Lock()
	f.preflight = pf
	f.mu.Unlock()
}

// awaitReturn waits for the HealthSignals call numbered n to return.
func (f *fHealth) awaitReturn(t *testing.T, n int64) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-f.returned:
			if got == n {
				return
			}
		case <-timeout:
			t.Fatalf("HealthSignals call %d did not return within 5s", n)
		}
	}
}

func sig(id, severity string) apitypes.HealthSignal {
	return apitypes.HealthSignal{ID: id, Severity: severity, Subject: "planted:" + id, Message: id, ObservedAt: time.Now()}
}

// healthReport is a report at at whose Counts count sigs.
func healthReport(at time.Time, sigs ...apitypes.HealthSignal) provider.HealthReport {
	r := provider.HealthReport{ObservedAt: at, Signals: sigs}
	for _, s := range sigs {
		switch s.Severity {
		case apitypes.SeverityCritical:
			r.Counts.Critical++
		case apitypes.SeverityWarning:
			r.Counts.Warning++
		case apitypes.SeverityInfo:
			r.Counts.Info++
		}
	}
	return r
}

func answer(r provider.HealthReport) func(int64) (provider.HealthReport, error) {
	return func(int64) (provider.HealthReport, error) { return r, nil }
}

func unavailable(reason string) provider.Status {
	return provider.Status{State: apitypes.ProviderUnavailable, Reason: reason}
}

// fanClock is a settable clock for core.Options.Now.
type fanClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fanClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fanClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// fanInRig is a real core.Registry over providers and the fan-in over it.
type fanInRig struct {
	reg  *core.Registry
	h    *healthFanIn
	logs *syncBuffer
}

// newFanInRig enables every provider except those named in disabled. A nil
// now is the real clock.
func newFanInRig(t *testing.T, now func() time.Time, disabled []string, providers ...provider.Provider) *fanInRig {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	cfgs := map[string]core.ProviderConfig{}
	for _, p := range providers {
		name := p.Spec().Name
		cfgs[name] = core.ProviderConfig{Enabled: !slices.Contains(disabled, name)}
	}
	reg, err := core.New(providers, core.Options{Host: "fanin-test", Providers: cfgs, Logger: log, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return &fanInRig{reg: reg, h: newHealthFanIn(reg, providers, log), logs: logs}
}

// flightOf is the source's flight in progress, or nil.
func flightOf(h *healthFanIn, name string) *healthFlight {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.flights[name]
}

// joinedOf is how many collects started or joined the source's flight in
// progress (0 when none is in progress).
func joinedOf(h *healthFanIn, name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f := h.flights[name]; f != nil {
		return f.joined
	}
	return 0
}

// noFlight waits until the source has no flight in progress, so the next
// collect starts a new one.
func noFlight(t *testing.T, h *healthFanIn, name string) {
	t.Helper()
	eventually(t, 5*time.Second, name+"'s flight to end", func() bool { return flightOf(h, name) == nil })
}

// timedCollect runs Collect and fails when it takes longer than timeout plus
// 50 ms (NFR-7, D-7).
func timedCollect(t *testing.T, h *healthFanIn, timeout time.Duration) []apitypes.ProviderSignals {
	t.Helper()
	start := time.Now()
	blocks := h.Collect(context.Background(), timeout)
	if took := time.Since(start); took > timeout+50*time.Millisecond {
		t.Errorf("Collect(%s) took %s, want at most %s", timeout, took, timeout+50*time.Millisecond)
	}
	return blocks
}

// oneBlock is the only block of blocks, which must be source's.
func oneBlock(t *testing.T, blocks []apitypes.ProviderSignals, source string) apitypes.ProviderSignals {
	t.Helper()
	if len(blocks) != 1 || blocks[0].Source != source {
		t.Fatalf("blocks = %+v, want exactly one, of %q", blocks, source)
	}
	return blocks[0]
}

// wantNullBlock checks a block with counts null, no signals, not stale and
// unavailable.signals equal to reason.
func wantNullBlock(t *testing.T, b apitypes.ProviderSignals, reason string) {
	t.Helper()
	if b.Counts != nil {
		t.Errorf("%s: counts = %+v, want null", b.Source, *b.Counts)
	}
	if b.Signals == nil || len(b.Signals) != 0 {
		t.Errorf("%s: signals = %#v, want []", b.Source, b.Signals)
	}
	if b.ObservedAt != nil {
		t.Errorf("%s: observedAt = %v, want null", b.Source, *b.ObservedAt)
	}
	if b.Stale {
		t.Errorf("%s: stale, want false", b.Source)
	}
	if want := map[string]string{"signals": reason}; !reflect.DeepEqual(b.Unavailable, want) {
		t.Errorf("%s: unavailable = %q, want %q", b.Source, b.Unavailable, want)
	}
}

func wantCounts(t *testing.T, b apitypes.ProviderSignals, want apitypes.SignalCounts) {
	t.Helper()
	if b.Counts == nil || *b.Counts != want {
		t.Errorf("%s: counts = %v, want %+v", b.Source, b.Counts, want)
	}
}

func wantObservedAt(t *testing.T, b apitypes.ProviderSignals, want time.Time) {
	t.Helper()
	if b.ObservedAt == nil || !b.ObservedAt.Equal(want) {
		t.Errorf("%s: observedAt = %v, want %v", b.Source, b.ObservedAt, want)
	}
}

func signalIDs(b apitypes.ProviderSignals) []string {
	ids := make([]string, len(b.Signals))
	for i, s := range b.Signals {
		ids[i] = s.ID
	}
	return ids
}

// TC-STG-35: a planted HealthSource that is not named storage produces a block (keyed on the interface). Owner WI 2.1. Requirements: FR-16, CON-4.
func TestTCSTG35_APlantedHealthSourceThatIsNotNamed(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	src := newFHealth("zzplanted")
	// Given unsorted: the block re-sorts them critical first.
	r := healthReport(t0, sig("i1", apitypes.SeverityInfo), sig("w1", apitypes.SeverityWarning),
		sig("c1", apitypes.SeverityCritical))
	r.Unavailable = map[string]string{"backups": "not configured"}
	src.script(answer(r))
	other := &probeProvider{name: "apt", preflight: ok}
	rig := newFanInRig(t, nil, nil, src, other)

	b := oneBlock(t, timedCollect(t, rig.h, 2*time.Second), "zzplanted")
	wantObservedAt(t, b, t0)
	wantCounts(t, b, apitypes.SignalCounts{Critical: 1, Warning: 1, Info: 1})
	if got, want := signalIDs(b), []string{"c1", "w1", "i1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("signals = %v, want %v", got, want)
	}
	if b.SignalsTruncated || b.Stale {
		t.Errorf("signalsTruncated = %t, stale = %t, want both false", b.SignalsTruncated, b.Stale)
	}
	if want := map[string]string{"backups": "not configured"}; !reflect.DeepEqual(b.Unavailable, want) {
		t.Errorf("unavailable = %q, want %q", b.Unavailable, want)
	}

	code, err := os.ReadFile("health.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(code), `"storage"`) {
		t.Error(`internal/daemon/health.go compares a provider name to "storage": the fan-in must be keyed on the interface`)
	}
}

// TC-STG-36: a disabled source gives no block, and enabling it by reload adds one. Owner WI 2.1. Requirements: FR-16, NFR-7.
func TestTCSTG36_ADisabledSourceGivesNoBlockAnd(t *testing.T) {
	src := newFHealth("zzplanted")
	src.script(answer(healthReport(time.Now(), sig("w1", apitypes.SeverityWarning))))
	rig := newFanInRig(t, nil, []string{"zzplanted"}, src)

	blocks := timedCollect(t, rig.h, 2*time.Second)
	if blocks == nil || len(blocks) != 0 {
		t.Fatalf("disabled source: Collect = %#v, want []", blocks)
	}
	if n, p := src.signals.Load(), src.preflights.Load(); n != 0 || p != 0 {
		t.Fatalf("disabled source: HealthSignals %d, Preflight %d calls, want 0 and 0", n, p)
	}

	if err := rig.reg.Apply(context.Background(), map[string]core.ProviderConfig{"zzplanted": {Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	b := oneBlock(t, timedCollect(t, rig.h, 2*time.Second), "zzplanted")
	wantCounts(t, b, apitypes.SignalCounts{Warning: 1})
	if n := src.signals.Load(); n != 1 {
		t.Errorf("HealthSignals calls after enabling = %d, want 1", n)
	}
}

// TC-STG-37: an enabled but unavailable source gives a block with counts null and its reason, without a call. Owner WI 2.1. Requirements: FR-16, NFR-7.
func TestTCSTG37_AnEnabledButUnavailableSourceGivesA(t *testing.T) {
	const reason = "zfs: /usr/sbin/zpool not found (nor /sbin/zpool); backups: not configured; timeMachine: not configured"
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	clock := &fanClock{t: t0}
	src := newFHealth("zzplanted")
	src.scriptPreflight(func(int64) provider.Status { return unavailable(reason) })
	src.script(answer(healthReport(t0, sig("w1", apitypes.SeverityWarning))))
	rig := newFanInRig(t, clock.now, nil, src)

	clock.set(t0.Add(4 * time.Second))
	b := oneBlock(t, timedCollect(t, rig.h, 2*time.Second), "zzplanted")
	wantNullBlock(t, b, "capability-unavailable: "+reason)
	if n, p := src.signals.Load(), src.preflights.Load(); n != 0 || p != 1 {
		t.Errorf("HealthSignals %d, Preflight %d calls, want 0 and 1 (no re-check within RecheckInterval)", n, p)
	}
}

// TC-STG-38: a source that blocks forever cannot delay Collect past its timeout, a second Collect starts no second call, and a late report is kept. Owner WI 2.1. Requirements: FR-16, NFR-2, NFR-7.
func TestTCSTG38_ASourceThatBlocksForeverCannotDelay(t *testing.T) {
	const timeout = 200 * time.Millisecond
	const late = "the current read did not finish within 200ms"
	src := newFHealth("zzplanted")
	rig := newFanInRig(t, nil, nil, src)

	// Warm-up: an immediate error leaves no report behind.
	src.script(func(int64) (provider.HealthReport, error) { return provider.HealthReport{}, errors.New("warm-up") })
	rig.h.Collect(context.Background(), timeout)
	noFlight(t, rig.h, "zzplanted")
	baseline := runtime.NumGoroutine()
	base := src.signals.Load()

	var relMu sync.Mutex
	rels := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	released := make([]bool, len(rels))
	release := func(i int) {
		relMu.Lock()
		defer relMu.Unlock()
		if !released[i] {
			released[i] = true
			close(rels[i])
		}
	}
	t.Cleanup(func() {
		for i := range rels {
			release(i)
		}
	})
	t1 := time.Now().Add(-time.Second).UTC()
	r1 := healthReport(t1, sig("r1-warning", apitypes.SeverityWarning))
	t2 := time.Now().UTC()
	r2 := healthReport(t2, sig("r2-critical", apitypes.SeverityCritical))
	t3 := time.Now().Add(-6 * time.Minute).UTC()
	r3 := healthReport(t3, sig("r3-info", apitypes.SeverityInfo))
	src.script(func(n int64) (provider.HealthReport, error) {
		switch n - base {
		case 1: // Ignores ctx until released
			<-rels[0]
			return r1, nil
		case 2:
			<-rels[1]
			return r1, nil
		case 3:
			return r2, nil
		case 4:
			return r3, nil
		default:
			<-rels[2]
			return r1, nil
		}
	})

	// Steps 1 and 2: no earlier report, so counts null with the deadline.
	wantNullBlock(t, oneBlock(t, timedCollect(t, rig.h, timeout), "zzplanted"), late)
	wantNullBlock(t, oneBlock(t, timedCollect(t, rig.h, timeout), "zzplanted"), late)
	var wg sync.WaitGroup
	blocks := make([][]apitypes.ProviderSignals, 18)
	for i := range blocks {
		wg.Go(func() { blocks[i] = timedCollect(t, rig.h, timeout) })
	}
	wg.Wait()
	for _, bs := range blocks {
		wantNullBlock(t, oneBlock(t, bs, "zzplanted"), late)
	}
	if n := src.signals.Load() - base; n != 1 {
		t.Errorf("HealthSignals calls after 20 collects = %d, want 1 (every later collect joins the flight)", n)
	}
	eventually(t, 2*time.Second, fmt.Sprintf("goroutines at most baseline %d + 1", baseline), func() bool {
		return runtime.NumGoroutine() <= baseline+1
	})

	// Step 3: R1 arrives after its deadline and is kept; the next collect
	// starts a new flight, which blocks, and serves R1 stale.
	release(0)
	src.awaitReturn(t, base+1)
	noFlight(t, rig.h, "zzplanted")
	b := oneBlock(t, timedCollect(t, rig.h, timeout), "zzplanted")
	if n := src.signals.Load() - base; n != 2 {
		t.Errorf("HealthSignals calls = %d, want 2 (a new flight)", n)
	}
	wantObservedAt(t, b, t1)
	wantCounts(t, b, apitypes.SignalCounts{Warning: 1})
	if got := signalIDs(b); !reflect.DeepEqual(got, []string{"r1-warning"}) {
		t.Errorf("signals = %v, want [r1-warning]", got)
	}
	if !b.Stale {
		t.Error("stale = false, want true")
	}
	if got := b.Unavailable["signals"]; got != late {
		t.Errorf("unavailable.signals = %q, want %q", got, late)
	}

	// Step 4: a fresh report is served fresh.
	release(1)
	src.awaitReturn(t, base+2)
	noFlight(t, rig.h, "zzplanted")
	b = oneBlock(t, timedCollect(t, rig.h, 2*time.Second), "zzplanted")
	wantObservedAt(t, b, t2)
	wantCounts(t, b, apitypes.SignalCounts{Critical: 1})
	if b.Stale || b.Unavailable != nil {
		t.Errorf("stale = %t, unavailable = %q, want false and none", b.Stale, b.Unavailable)
	}
	if n := src.signals.Load() - base; n != 3 {
		t.Errorf("HealthSignals calls = %d, want 3", n)
	}

	// Step 5 (A-49): a kept report older than 5 minutes is not served.
	b = oneBlock(t, timedCollect(t, rig.h, 2*time.Second), "zzplanted")
	wantObservedAt(t, b, t3)
	if b.Stale {
		t.Error("R3 as the current read: stale = true, want false")
	}
	wantNullBlock(t, oneBlock(t, timedCollect(t, rig.h, timeout), "zzplanted"), late)
}

// TC-STG-39: an error or a panic in a source becomes a block with a reason and spares the others. Owner WI 2.1. Requirements: FR-16, NFR-7.
func TestTCSTG39_AnErrorOrAPanicInA(t *testing.T) {
	aa, bb, cc := newFHealth("aaerr"), newFHealth("bbpanic"), newFHealth("ccok")
	aa.script(func(int64) (provider.HealthReport, error) {
		return provider.HealthReport{}, errors.New("planted failure")
	})
	bb.script(func(int64) (provider.HealthReport, error) { panic("planted panic") })
	cc.script(answer(healthReport(time.Now(), sig("ok-info", apitypes.SeverityInfo))))
	rig := newFanInRig(t, nil, nil, cc, bb, aa)

	blocks := timedCollect(t, rig.h, 2*time.Second)
	var names []string
	for _, b := range blocks {
		names = append(names, b.Source)
	}
	if want := []string{"aaerr", "bbpanic", "ccok"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("blocks = %v, want %v", names, want)
	}
	if blocks[0].Counts != nil || !strings.Contains(blocks[0].Unavailable["signals"], "planted failure") {
		t.Errorf("aaerr: counts = %v, unavailable = %q, want null and a reason naming planted failure",
			blocks[0].Counts, blocks[0].Unavailable)
	}
	wantNullBlock(t, blocks[1], "internal error")
	wantCounts(t, blocks[2], apitypes.SignalCounts{Info: 1})
	if got := signalIDs(blocks[2]); !reflect.DeepEqual(got, []string{"ok-info"}) {
		t.Errorf("ccok: signals = %v, want [ok-info]", got)
	}

	errs := 0
	for _, rec := range logRecords(rig.logs) {
		if rec["level"] == "ERROR" && rec["provider"] == "bbpanic" {
			errs++
		}
	}
	if errs != 1 {
		t.Errorf("error-level records naming bbpanic = %d, want 1:\n%s", errs, logText(rig.logs))
	}
}

// TC-STG-40: a block holds the first 10 signals, critical first, with counts over all of them. Owner WI 2.1. Requirements: FR-16.
func TestTCSTG40_ABlockHoldsTheFirst10Signals(t *testing.T) {
	var twelve []apitypes.HealthSignal
	for i := 1; i <= 8; i++ {
		twelve = append(twelve, sig(fmt.Sprintf("w%d", i), apitypes.SeverityWarning))
	}
	for i := 1; i <= 3; i++ {
		twelve = append(twelve, sig(fmt.Sprintf("c%d", i), apitypes.SeverityCritical))
	}
	twelve = append(twelve, sig("i1", apitypes.SeverityInfo))
	aa := newFHealth("aatwelve")
	aa.script(answer(healthReport(time.Now(), twelve...)))

	// The source capped its list at 500 and counted all 601 signals.
	var capped []apitypes.HealthSignal
	for i := range 499 {
		capped = append(capped, sig(fmt.Sprintf("w%03d", i), apitypes.SeverityWarning))
	}
	capped = append(capped, sig("c-last", apitypes.SeverityCritical))
	bb := newFHealth("bbcapped")
	bb.script(answer(provider.HealthReport{ObservedAt: time.Now(), Signals: capped, Truncated: true,
		Counts: apitypes.SignalCounts{Critical: 1, Warning: 600}}))
	rig := newFanInRig(t, nil, nil, aa, bb)

	blocks := timedCollect(t, rig.h, 2*time.Second)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want 2", blocks)
	}
	a, b := blocks[0], blocks[1]
	wantCounts(t, a, apitypes.SignalCounts{Critical: 3, Warning: 8, Info: 1})
	if got, want := signalIDs(a), []string{"c1", "c2", "c3", "w1", "w2", "w3", "w4", "w5", "w6", "w7"}; !reflect.DeepEqual(got, want) {
		t.Errorf("aatwelve: signals = %v, want %v", got, want)
	}
	if !a.SignalsTruncated {
		t.Error("aatwelve: signalsTruncated = false, want true")
	}
	wantCounts(t, b, apitypes.SignalCounts{Critical: 1, Warning: 600})
	if !b.SignalsTruncated || len(b.Signals) != 10 || b.Signals[0].ID != "c-last" {
		t.Errorf("bbcapped: signalsTruncated = %t, %d signals, first %v; want true, 10, c-last",
			b.SignalsTruncated, len(b.Signals), signalIDs(b))
	}
}

// fHealthUser is a planted provider.HealthSourcesUser that records each
// injection.
type fHealthUser struct {
	probeProvider
	mu    sync.Mutex
	calls int
	got   provider.HealthSources
	at    time.Time
}

func (u *fHealthUser) UseHealthSources(s provider.HealthSources) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.got, u.at = s, time.Now()
}

// TC-STG-41: daemon.New injects the fan-in into every HealthSourcesUser before serving. Owner WI 2.1. Requirements: FR-16, CON-4.
//
// F-HEALTH takes the name of a real config section here ("scripts"), as
// probeProvider does, because only a configured section enables a provider
// in a daemon.
func TestTCSTG41_DaemonNewInjectsTheFanInInto(t *testing.T) {
	user := &fHealthUser{probeProvider: probeProvider{name: "apt", preflight: ok}}
	src := newFHealth("scripts")
	src.script(answer(healthReport(time.Now(), sig("w1", apitypes.SeverityWarning))))
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeConfig(t, cfgPath, freePort(t), "scripts")

	start := time.Now()
	startInProcess(t, cfgPath, user, src)
	ready := time.Now()

	user.mu.Lock()
	calls, got, at := user.calls, user.got, user.at
	user.mu.Unlock()
	if calls != 1 || got == nil {
		t.Fatalf("UseHealthSources: %d calls, value %v; want 1 call with a fan-in", calls, got)
	}
	if at.Before(start) || at.After(ready) {
		t.Errorf("UseHealthSources at %v, want between New (%v) and ready (%v)", at, start, ready)
	}
	b := oneBlock(t, got.Collect(context.Background(), 2*time.Second), "scripts")
	wantCounts(t, b, apitypes.SignalCounts{Warning: 1})
}

// TC-STG-43: a source unavailable at startup recovers on the health path after 5 s, with no direct call. Owner WI 2.1. Requirements: FR-16, FR-3, NFR-7.
func TestTCSTG43_ASourceUnavailableAtStartupRecoversOnThe(t *testing.T) {
	const reason = "zfs: zpool list did not finish within 1s"
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

	t.Run("re-check on the health path", func(t *testing.T) {
		clock := &fanClock{t: t0}
		src := newFHealth("zzplanted")
		src.scriptPreflight(func(n int64) provider.Status {
			if n == 1 {
				return unavailable(reason)
			}
			return ok()
		})
		gate := make(chan struct{})
		var gateOnce sync.Once
		openGate := func() { gateOnce.Do(func() { close(gate) }) }
		t.Cleanup(openGate)
		src.script(func(int64) (provider.HealthReport, error) {
			<-gate
			return healthReport(t0, sig("w1", apitypes.SeverityWarning)), nil
		})
		rig := newFanInRig(t, clock.now, nil, src) // Step 1: startup preflight at T0

		clock.set(t0.Add(4 * time.Second))
		wantNullBlock(t, oneBlock(t, timedCollect(t, rig.h, 2*time.Second), "zzplanted"),
			"capability-unavailable: "+reason)
		if n, p := src.signals.Load(), src.preflights.Load(); n != 0 || p != 1 {
			t.Fatalf("step 2: HealthSignals %d, Preflight %d calls, want 0 and 1", n, p)
		}

		clock.set(t0.Add(6 * time.Second))
		var wg sync.WaitGroup
		blocks := make([][]apitypes.ProviderSignals, 10)
		for i := range blocks {
			wg.Go(func() { blocks[i] = timedCollect(t, rig.h, 2*time.Second) })
		}
		// Every collect joins the one flight before the source answers.
		eventually(t, time.Second, "10 collects on one flight", func() bool { return joinedOf(rig.h, "zzplanted") == 10 })
		openGate()
		wg.Wait()
		for _, bs := range blocks {
			b := oneBlock(t, bs, "zzplanted")
			wantCounts(t, b, apitypes.SignalCounts{Warning: 1})
			if b.Stale {
				t.Error("step 3: stale = true, want false")
			}
		}
		if n, p := src.signals.Load(), src.preflights.Load(); n != 1 || p != 2 {
			t.Errorf("step 3: HealthSignals %d, Preflight %d calls, want 1 and 2", n, p)
		}
	})

	t.Run("a re-check slower than the deadline is not cancelled", func(t *testing.T) {
		clock := &fanClock{t: t0}
		src := newFHealth("zzplanted")
		src.scriptPreflight(func(n int64) provider.Status {
			if n == 1 {
				return unavailable(reason)
			}
			time.Sleep(300 * time.Millisecond)
			return ok()
		})
		src.script(answer(healthReport(t0, sig("w1", apitypes.SeverityWarning))))
		rig := newFanInRig(t, clock.now, nil, src)

		clock.set(t0.Add(6 * time.Second))
		b := oneBlock(t, timedCollect(t, rig.h, 100*time.Millisecond), "zzplanted")
		if b.Counts != nil || b.Unavailable["signals"] == "" {
			t.Errorf("collect during the re-check: counts = %v, unavailable = %q; want null and a reason",
				b.Counts, b.Unavailable)
		}
		eventually(t, 2*time.Second, "the re-check to complete", func() bool {
			return src.preflights.Load() == 2 && flightOf(rig.h, "zzplanted") == nil
		})
		b = oneBlock(t, timedCollect(t, rig.h, 2*time.Second), "zzplanted")
		wantCounts(t, b, apitypes.SignalCounts{Warning: 1})
		if p := src.preflights.Load(); p != 2 {
			t.Errorf("Preflight calls = %d, want 2", p)
		}
	})
}
