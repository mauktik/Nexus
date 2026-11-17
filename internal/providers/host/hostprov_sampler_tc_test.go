// Tests of planning/system-provider/test-cases.md TC-HP-61, TC-HP-62, TC-HP-63, TC-HP-64, TC-HP-65, TC-HP-66, TC-HP-67, TC-HP-68, TC-HP-69, TC-HP-70, TC-HP-E14, TC-HP-E15, TC-HP-E16, TC-HP-E17, TC-HP-ERR14, TC-HP-ERR15, TC-HP-ERR16, TC-HP-ERR17.
// Owner WI: 3.3. The ready snapshot is the capture's fs/ sample then its t1/
// sample, 10 s apart on F-CLK; the sampler helpers are in sampler_test.go.

package host

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-HP-61: CPU percentages from the two captured samples follow A-6 and agree with vmstat. Owner WI 3.3. Requirements: FR-11, FR-6, NFR-3.
//
// - Setup: host stack per capture; ready snapshot (fs → t1, F-CLK 10 s).
// - Steps: read the snapshot's CPU fields.
// - Expected: total and each per-CPU `usagePercent` equal `(Δtotal − Δidle − Δiowait)/Δtotal × 100` computed in-test from `fs/proc/stat` and `t1/proc/stat` (|diff| < 1e-9); user/system/iowait/steal shares equal their deltas over Δtotal × 100; total within ±5 pp of `100 − id − wa` from the last line of `tools/vmstat`.
func TestTCHP61_CPUPercentagesFromTheTwoCapturedSamples(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newSamplerStack(t, capture)
			_, snap := s.ready()
			checkCPU(t, capture, snap, s.clk.Now())
		})
	}
}

// TC-HP-62: Disk throughput and IOPS from diskstats deltas. Owner WI 3.3. Requirements: FR-8, FR-11.
//
// - Setup: ready snapshot per capture.
// - Steps: read each disk's rates.
// - Expected: read/write bytes/s = Δsectors × 512 / 10 and IOPS = Δcompleted / 10, computed in-test from `fs/` and `t1/` diskstats; every value ≥ 0.
func TestTCHP62_DiskThroughputAndIOPSFromDiskstatsDeltas(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newSamplerStack(t, capture)
			_, snap := s.ready()
			checkDisks(t, capture, snap, s.clk.Now())
		})
	}
}

// TC-HP-63: Interface rates from net/dev deltas. Owner WI 3.3. Requirements: FR-9, FR-11.
//
// - Setup: ready snapshot per capture.
// - Steps: read each interface's rates.
// - Expected: rx/tx bytes/s = Δbytes / 10 computed in-test; ≥ 0; one entry per interface present in both samples with the same `ifindex`.
func TestTCHP63_InterfaceRatesFromNetDevDeltas(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newSamplerStack(t, capture)
			_, snap := s.ready()
			checkIfaces(t, capture, snap, s.clk.Now())
		})
	}
}

// TC-HP-64: Per-process CPU and the top-5 lists. Owner WI 3.3. Requirements: FR-10, FR-11.
//
// - Setup: ready snapshot per capture; `PageSize` 4096.
// - Steps: read `topByCpu`, `topByMemory`.
// - Expected:
//   - each `cpuPercent` = Δ(utime+stime)/(100 × 10) × 100 for pids in both `fs/` and `t1/` with equal starttime (computed in-test);
//   - `topByCpu` is the 5 highest, descending, ties by pid ascending; `topByMemory` the 5 highest `rss × 4096`;
//   - pids only in one sample are not ranked; the sum of per-pid deltas ≤ the total CPU delta.
func TestTCHP64_PerProcessCPUAndTheTop5(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newSamplerStack(t, capture)
			_, snap := s.ready()
			checkProcs(t, capture, snap, s.clk.Now())
		})
	}
}

// TC-HP-65: Values are warming up until two samples exist, then ready. Owner WI 3.3. Requirements: FR-11, FR-12.
//
// - Setup: host stack (UBU), `RunBackground` started, `waitSeq(1)` (the immediate sample).
// - Steps:
//  1. Read the snapshot (state A).
//  2. `F-FS.advance()`, `F-CLK.Advance(10s)`, `F-TICK.Fire`, `waitSeq(2)`; read again (state A′).
//
// - Expected: A: `state warming-up`, `sampledAt` set, `intervalSeconds` null with reason `warming up`, CPU %, rates and top lists null or `[]` with `warming up`, never 0. A′: `state ready`, `intervalSeconds` 10, the values of TC-HP-61..64.
func TestTCHP65_ValuesAreWarmingUpUntilTwoSamples(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	b := s.start()
	snap := s.waitSeq(1)
	now := s.clk.Now()

	// State A: one sample.
	if got := snap.state(now); got != spec.SamplerWarmingUp {
		t.Fatalf("A: state = %q, want %q", got, spec.SamplerWarmingUp)
	}
	sm := snap.sampler(now)
	if sm.SampledAt == nil || !sm.SampledAt.Equal(now) {
		t.Errorf("A: sampledAt = %v, want %v", sm.SampledAt, now)
	}
	if sm.IntervalSeconds != nil || sm.Unavailable["intervalSeconds"] != reasonWarmingUp {
		t.Errorf("A: intervalSeconds = %v with %q, want null with %q", sm.IntervalSeconds,
			sm.Unavailable["intervalSeconds"], reasonWarmingUp)
	}
	for name, v := range map[string]rateVal{"usage": snap.cpu.usage, "user": snap.cpu.user,
		"system": snap.cpu.system, "iowait": snap.cpu.iowait, "steal": snap.cpu.steal} {
		wantMissing(t, snap, now, "A: cpu "+name, v, reasonWarmingUp)
	}
	if len(snap.perCPU) == 0 {
		t.Error("A: no per-CPU entries")
	}
	for id, c := range snap.perCPU {
		wantMissing(t, snap, now, fmt.Sprintf("A: cpu%d usage", id), c.usage, reasonWarmingUp)
	}
	if len(snap.disks) == 0 || len(snap.ifaces) == 0 {
		t.Fatalf("A: %d disks, %d interfaces, want the capture's", len(snap.disks), len(snap.ifaces))
	}
	for name, d := range snap.disks {
		for _, v := range []rateVal{d.readBps, d.writeBps, d.readIOPS, d.writeIOPS} {
			wantMissing(t, snap, now, "A: disk "+name, v, reasonWarmingUp)
		}
	}
	for name, i := range snap.ifaces {
		wantMissing(t, snap, now, "A: rx "+name, i.rx, reasonWarmingUp)
		wantMissing(t, snap, now, "A: tx "+name, i.tx, reasonWarmingUp)
	}
	byCPU, byMem, secs, why := snap.processes(now)
	if len(byCPU) != 0 || len(byMem) != 0 || secs != nil || why != reasonWarmingUp {
		t.Errorf("A: processes = %v, %v, %v, %q; want [] [] null with %q", byCPU, byMem, secs, why, reasonWarmingUp)
	}

	// State A′: a second sample 10 s later, over t1/.
	s.ffs.advance()
	s.fire(b, 10*time.Second)
	snap = s.waitSeq(2)
	now = s.clk.Now()
	if got := snap.state(now); got != spec.SamplerReady {
		t.Fatalf("A′: state = %q, want %q", got, spec.SamplerReady)
	}
	if sm := snap.sampler(now); sm.IntervalSeconds == nil || *sm.IntervalSeconds != 10 || len(sm.Unavailable) != 0 {
		t.Errorf("A′: sampler = %+v, want intervalSeconds 10 and nothing unavailable", sm)
	}
	checkCPU(t, capUBU, snap, now)
	checkDisks(t, capUBU, snap, now)
	checkIfaces(t, capUBU, snap, now)
	checkProcs(t, capUBU, snap, now)
}

// TC-HP-66: A sample older than three intervals is stale. Owner WI 3.3. Requirements: FR-11, NFR-4.
//
// - Setup: ready snapshot (interval 10 s).
// - Steps: `F-CLK.Advance(30s)`, read; `F-CLK.Advance(1s)`, read.
// - Expected: at age 30 s (= 3 × interval, not over): `ready`, `ageSeconds` 30; at 31 s: `stale`, reason `stale: last sample 31s old`, computed values null, `ageSeconds` 31.
func TestTCHP66_ASampleOlderThanThreeIntervalsIs(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	_, snap := s.ready()

	s.clk.Advance(30 * time.Second)
	now := s.clk.Now()
	if got := snap.state(now); got != spec.SamplerReady {
		t.Errorf("age 30s: state = %q, want %q", got, spec.SamplerReady)
	}
	if sm := snap.sampler(now); sm.AgeSeconds == nil || *sm.AgeSeconds != 30 {
		t.Errorf("age 30s: ageSeconds = %v, want 30", sm.AgeSeconds)
	}
	mustVal(t, snap, now, "age 30s: cpu usage", snap.cpu.usage)

	s.clk.Advance(time.Second)
	now = s.clk.Now()
	if got := snap.state(now); got != spec.SamplerStale {
		t.Errorf("age 31s: state = %q, want %q", got, spec.SamplerStale)
	}
	sm := snap.sampler(now)
	if sm.State != spec.SamplerStale || sm.AgeSeconds == nil || *sm.AgeSeconds != 31 {
		t.Errorf("age 31s: sampler = %+v, want stale with ageSeconds 31", sm)
	}
	const stale = "stale: last sample 31s old"
	wantMissing(t, snap, now, "age 31s: cpu usage", snap.cpu.usage, stale)
	wantMissing(t, snap, now, "age 31s: eth0 rx", snap.ifaces["eth0"].rx, stale)
	wantMissing(t, snap, now, "age 31s: vdb read", snap.disks["vdb"].readBps, stale)
	if byCPU, _, secs, why := snap.processes(now); byCPU != nil || secs != nil || why != stale {
		t.Errorf("age 31s: processes = %v, %v, %q; want empty with %q", byCPU, secs, why, stale)
	}
}

// TC-HP-67: RunBackground samples at once, ticks, and stops within one tick of cancel. Owner WI 3.3. Requirements: FR-11, NFR-2.
//
// - Setup: host stack (UBU); goroutine baseline.
// - Steps: start `RunBackground(ctx)` in a goroutine; `waitSeq(1)`; fire 2 ticks; `cancel()`.
// - Expected: seq 1 without any tick; seq 3 after two; after cancel, `RunBackground` returns within 100 ms with no further tick fired; F-TICK recorded `Stop`; `p.run.Load() == nil`; goroutines back to baseline (`eventually`).
func TestTCHP67_RunBackgroundSamplesAtOnceTicksAndStops(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	baseline := runtime.NumGoroutine()
	b := s.start()
	if snap := s.waitSeq(1); snap.seq != 1 {
		t.Fatalf("immediate sample: seq %d, want 1", snap.seq)
	}
	if got := s.tks.intervals(); len(got) != 1 || got[0] != 10*time.Second {
		t.Fatalf("tickers made %v, want one at 10s", got)
	}
	s.fire(b, 10*time.Second)
	s.fire(b, 10*time.Second)
	run := s.p.run.Load()
	if snap := s.waitSeq(3); snap.seq != 3 {
		t.Fatalf("after two ticks: seq %d, want 3", snap.seq)
	}

	start := time.Now()
	b.cancel()
	select {
	case <-b.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("RunBackground did not return within 100ms of cancel")
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("RunBackground returned %v after cancel, want within 100ms", took)
	}
	if got := run.snapshot().seq; got != 3 {
		t.Errorf("seq after cancel = %d, want 3 (no further tick)", got)
	}
	if ev := b.tk.recorded(); len(ev) != 1 || ev[0] != "stop" {
		t.Errorf("F-TICK recorded %q, want [stop]", ev)
	}
	if s.p.run.Load() != nil {
		t.Error("p.run still set after RunBackground returned")
	}
	waitGoroutines(t, baseline)
}

// TC-HP-68: A changed interval resets the ticker and a changed duration resizes history. Owner WI 3.3. Requirements: FR-11, FR-13.
//
// - Setup: running sampler with 10 s / 1 h and 5 points.
// - Steps: `Configure({sampleInterval: 2s, historyDuration: 4s})`; fire one tick.
// - Expected: F-TICK recorded `Reset(2s)` once; history capacity 2 holding the 2 newest points; info log "sampler interval changed" once.
func TestTCHP68_AChangedIntervalResetsTheTickerAnd(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	b := s.start()
	s.waitSeq(1)
	for i := range 4 {
		s.fire(b, 10*time.Second)
		s.waitSeq(uint64(i + 2))
	}
	h := s.p.run.Load().history()
	if got := histTimes(h); len(got) != 5 {
		t.Fatalf("history holds %d points, want 5", len(got))
	}
	before := histTimes(h)

	if err := s.p.Configure(hostSection(t, "sampleInterval: 2s\nhistoryDuration: 4s\n")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	s.fire(b, 2*time.Second)
	s.waitSeq(6)
	if ev := b.tk.recorded(); len(ev) != 1 || ev[0] != "reset 2s" {
		t.Errorf("F-TICK recorded %q, want [reset 2s]", ev)
	}
	h.mu.Lock()
	capacity := h.capacity
	h.mu.Unlock()
	want := []int64{before[4], s.clk.Now().UnixMilli()}
	if got := histTimes(h); capacity != 2 || !slices.Equal(got, want) {
		t.Errorf("history capacity %d holding %v, want 2 holding the 2 newest %v", capacity, got, want)
	}
	if n := len(s.logs.records(slog.LevelInfo, "sampler interval changed")); n != 1 {
		t.Errorf("%d \"sampler interval changed\" logs, want 1", n)
	}

	// The next tick at the same settings changes nothing.
	s.fire(b, 2*time.Second)
	s.waitSeq(7)
	if ev := b.tk.recorded(); len(ev) != 1 {
		t.Errorf("F-TICK recorded %q after an unchanged tick, want one reset", ev)
	}
	if n := len(s.logs.records(slog.LevelInfo, "sampler interval changed")); n != 1 {
		t.Errorf("%d \"sampler interval changed\" logs after an unchanged tick, want 1", n)
	}
}

// TC-HP-69: An older overlapping run cannot clear a newer run's state. Owner WI 3.3. Requirements: FR-11, NFR-4.
//
// - Setup: host stack; two F-TICKs handed out in order.
// - Steps: start run A; start run B (A still running); cancel A and wait for it to return.
// - Expected: `p.run.Load()` is B's run; reads use B's snapshot; cancelling B then clears it.
func TestTCHP69_AnOlderOverlappingRunCannotClearA(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	a := s.start()
	s.waitSeq(1)
	runA := s.p.run.Load()

	bg := s.start()
	var runB *samplerRun
	eventually(t, 2*time.Second, func() bool {
		runB = s.p.run.Load()
		return runB != runA && runB.snapshot() != nil
	}, "run B published")

	a.cancel()
	<-a.done
	if s.p.run.Load() != runB {
		t.Fatal("cancelling the older run A cleared or replaced run B")
	}
	if ev := a.tk.recorded(); len(ev) != 1 || ev[0] != "stop" {
		t.Errorf("A's ticker recorded %q, want [stop]", ev)
	}
	s.fire(bg, 10*time.Second)
	snap := s.waitSeq(2)
	if snap != runB.snapshot() || snap.seq != 2 {
		t.Errorf("reads see %v, want run B's second snapshot", snap)
	}

	bg.cancel()
	<-bg.done
	if s.p.run.Load() != nil {
		t.Error("cancelling run B did not clear p.run")
	}
}

// TC-HP-70: The process scan is throttled by its measured cost. Owner WI 3.3. Requirements: FR-10, NFR-2.
//
// - Setup: host stack (UBU, N pids in the capture); F-FS `slow("proc/*/stat", F-CLK.Advance(d))` with `d` chosen so the scan costs 1.2% of the 10 s interval (120 ms total).
// - Steps: fire 6 ticks; read `processSampleSeconds` and the per-tick scan record.
// - Expected: `scanEvery = ceil(0.12 / 0.05) = 3` (computed in-test from the injected cost); scans happen on every 3rd tick; `processSampleSeconds` = 30; one info log line about throttling; with `d` making the cost 10%, `scanEvery` is capped at 6.
func TestTCHP70_TheProcessScanIsThrottledByIts(t *testing.T) {
	pids := capturePIDs(t, capUBU, "fs")
	for _, c := range []struct {
		name  string
		cost  time.Duration // of one scan of every pid
		ticks int
	}{
		{"1.2% of the interval", 120 * time.Millisecond, 6},
		{"10% of the interval", time.Second, 12},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSamplerStack(t, capUBU)
			d := c.cost / time.Duration(len(pids))
			if d*time.Duration(len(pids)) != c.cost {
				t.Fatalf("cost %v does not divide over %d pids", c.cost, len(pids))
			}
			s.ffs.slow("proc/*/stat", func() { s.clk.Advance(d) })
			interval := 10 * time.Second
			every := int(math.Ceil(c.cost.Seconds() / (0.005 * interval.Seconds())))
			every = min(every, 6)

			t0 := s.clk.Now()
			b := s.start()
			s.waitSeq(1)
			probe := "proc/" + strconv.Itoa(pids[0]) + "/stat"
			var scanned []int
			for i := 1; i <= c.ticks; i++ {
				n := s.ffs.count("open", probe)
				s.clk.Set(t0.Add(time.Duration(i) * interval))
				b.tk.Fire(s.clk.Now())
				s.waitSeq(uint64(i + 1))
				if s.ffs.count("open", probe) > n {
					scanned = append(scanned, i)
				}
			}
			var want []int
			for i := every; i <= c.ticks; i += every {
				want = append(want, i)
			}
			if !slices.Equal(scanned, want) {
				t.Errorf("scans on ticks %v, want every %d: %v", scanned, every, want)
			}
			snap := s.p.run.Load().snapshot()
			_, _, secs, why := snap.processes(s.clk.Now())
			if wantSecs := float64(every) * interval.Seconds(); secs == nil || *secs != wantSecs {
				t.Errorf("processSampleSeconds = %v (%q), want %v", secs, why, wantSecs)
			}
			if n := len(s.logs.records(slog.LevelInfo, "process scan throttled")); n != 1 {
				t.Errorf("%d throttling logs, want 1", n)
			}
		})
	}
}

// TC-HP-E14: Huge 64-bit counters and 32-bit wraps. Owner WI 3.3. Requirements: FR-11.
//
// - Setup: two samples derived from UBU's `proc/net/dev`: eth0 rx `2^64 − 1000` → `2^64 − 10` (Δ 990); eth1 rx `2^32 − 10` → `5` (a 32-bit wrap).
// - Steps: `compute(prev, cur, 10s)`.
// - Expected: eth0 rx 99 B/s exactly (no overflow); eth1 rx null with `counter reset: eth1` (A-9: no wrap arithmetic), never a huge or negative number.
func TestTCHPE14_Huge64BitCountersAnd32Bit(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	src := newSource(ffs, time.Second, nil)
	clk := newFakeClock()
	netdev := func(eth0, eth1 uint64) []byte {
		base := string(ffs.rawRead(pathNetDev))
		var out []string
		for _, l := range strings.Split(strings.TrimRight(base, "\n"), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "eth0:") {
				l = fmt.Sprintf("  eth0: %d 20 0 0 0 0 0 0 126 3 0 0 0 0 0 0", eth0)
			}
			out = append(out, l)
		}
		out = append(out, fmt.Sprintf("  eth1: %d 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0", eth1))
		return []byte(strings.Join(out, "\n") + "\n")
	}
	const max64 = math.MaxUint64
	ffs.replace(pathNetDev, netdev(max64-999, 1<<32-10))
	prev := readSample(src, clk.Now, clk.Now(), false)
	clk.Advance(10 * time.Second)
	ffs.replace(pathNetDev, netdev(max64-9, 5))
	cur := readSample(src, clk.Now, clk.Now(), false)
	if got := cur.ifaces["eth0"].stat.RxBytes - prev.ifaces["eth0"].stat.RxBytes; got != 990 {
		t.Fatalf("fixture: eth0 Δrx = %d, want 990", got)
	}

	snap := compute(prev, cur, 10*time.Second, 4096)
	snap.interval = 10 * time.Second
	now := clk.Now()
	if got := mustVal(t, snap, now, "eth0 rx", snap.ifaces["eth0"].rx); got != 99 {
		t.Errorf("eth0 rx = %v B/s, want exactly 99", got)
	}
	wantMissing(t, snap, now, "eth1 rx", snap.ifaces["eth1"].rx, "counter reset: eth1")
	if got := mustVal(t, snap, now, "eth1 tx", snap.ifaces["eth1"].tx); got != 0 {
		t.Errorf("eth1 tx = %v, want 0", got)
	}
}

// TC-HP-E15: RunBackground returns at once off Linux. Owner WI 3.3. Requirements: CON-5, FR-11.
//
// - Setup: `GOOS: "darwin"`.
// - Steps: `RunBackground(ctx)` with a live ctx.
// - Expected: returns within 10 ms; `p.run` nil; F-TICK never created; F-FS recorded nothing.
func TestTCHPE15_RunBackgroundReturnsAtOnceOffLinux(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	s := newSamplerStackFS(t, capUBU, ffs, "darwin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	s.p.RunBackground(ctx)
	if took := time.Since(start); took > 10*time.Millisecond {
		t.Errorf("RunBackground took %v off Linux, want under 10ms", took)
	}
	if s.p.run.Load() != nil {
		t.Error("p.run set off Linux")
	}
	if got := s.tks.intervals(); len(got) != 0 {
		t.Errorf("tickers made off Linux: %v", got)
	}
	if calls := ffs.recorded(); len(calls) != 0 {
		t.Errorf("F-FS touched off Linux: %v", calls)
	}
}

// TC-HP-E16: A stopped provider holds no history and a new run starts empty. Owner WI 3.3. Requirements: NFR-2, FR-13, ASM-3.
//
// - Setup: a run with 10 points.
// - Steps: cancel; `runtime.GC()`; start a new run; read metrics.
// - Expected: after cancel no reference to the old history remains (a `runtime.SetFinalizer`/weak pointer on it fires after GC); the new run's history has 1 point (its immediate sample) and its state is `warming-up`.
func TestTCHPE16_AStoppedProviderHoldsNoHistoryAnd(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	b := s.start()
	s.waitSeq(1)
	for i := range 9 {
		s.fire(b, 10*time.Second)
		s.waitSeq(uint64(i + 2))
	}
	old := weakHistory(t, s.p, 10)

	b.cancel()
	<-b.done
	eventually(t, 2*time.Second, func() bool {
		runtime.GC()
		return old.Value() == nil
	}, "the stopped run's history collected")

	s.start()
	snap := s.waitSeq(1)
	if got := snap.state(s.clk.Now()); got != spec.SamplerWarmingUp {
		t.Errorf("new run: state = %q, want %q", got, spec.SamplerWarmingUp)
	}
	h := s.p.run.Load().history()
	if got := histTimes(h); len(got) != 1 {
		t.Errorf("new run's history holds %d points, want 1 (its immediate sample)", len(got))
	}
	m := h.Query(s.clk.Now(), 3600, 0, []string{spec.MetricLoad})
	if m.HistoryStart == nil || m.Points == 0 {
		t.Errorf("new run's metrics: historyStart %v, %d points, want its one sample", m.HistoryStart, m.Points)
	}
}

// TC-HP-E17: One unreadable counter source affects only its own values. Owner WI 3.3. Requirements: FR-12, FR-11.
//
// - Setup: F-FS(UBU) `deny("proc/diskstats")`.
// - Steps: two samples.
// - Expected: state `ready`; CPU and interface rates populated; disk rates null with `permission denied: /proc/diskstats`.
func TestTCHPE17_OneUnreadableCounterSourceAffectsOnlyIts(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	s.ffs.deny(pathDiskstats)
	_, snap := s.ready()
	now := s.clk.Now()
	if got := snap.state(now); got != spec.SamplerReady {
		t.Fatalf("state = %q, want %q", got, spec.SamplerReady)
	}
	mustVal(t, snap, now, "cpu usage", snap.cpu.usage)
	for name, i := range snap.ifaces {
		mustVal(t, snap, now, "rx "+name, i.rx)
		mustVal(t, snap, now, "tx "+name, i.tx)
	}
	if len(snap.ifaces) != 3 {
		t.Errorf("%d interfaces, want 3", len(snap.ifaces))
	}
	if snap.disks != nil || snap.disksWhy != "permission denied: /proc/diskstats" {
		t.Errorf("disks = %v with %q, want none with %q", snap.disks, snap.disksWhy, "permission denied: /proc/diskstats")
	}
}

// TC-HP-ERR14: A counter decrease gives no rate for that entity only. Owner WI 3.3. Requirements: FR-11.
//
// - Setup: UBU with `t1/proc/net/dev` replaced so one interface's rx bytes is below its `fs/` value (same ifindex), and one disk's sectors read below.
// - Steps: two samples.
// - Expected: that interface's rx rate and that disk's read rate null with `counter reset: <name>`; their tx/write rates and every other entity populated; no negative value anywhere.
func TestTCHPERR14_ACounterDecreaseGivesNoRateFor(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	b := s.start()
	s.waitSeq(1)
	fsDev, fsDisk := netCols(t, capFile(t, capUBU, "fs", pathNetDev)), diskCols(t, capFile(t, capUBU, "fs", pathDiskstats))
	s.ffs.advance()
	// eth0 rx below its fs/ value (same ifindex); vdb sectors read below.
	s.ffs.replace(pathNetDev, setNetCol(t, s.ffs.rawRead(pathNetDev), "eth0", 0, fsDev["eth0"][0]-1))
	s.ffs.replace(pathDiskstats, setDiskCol(t, s.ffs.rawRead(pathDiskstats), "vdb", 5, fsDisk["vdb"][3]-1))
	s.fire(b, 10*time.Second)
	snap := s.waitSeq(2)
	now := s.clk.Now()

	wantMissing(t, snap, now, "eth0 rx", snap.ifaces["eth0"].rx, "counter reset: eth0")
	wantMissing(t, snap, now, "vdb read", snap.disks["vdb"].readBps, "counter reset: vdb")
	for name, i := range snap.ifaces {
		if name != "eth0" {
			checkNonNegative(t, snap, now, "rx "+name, i.rx)
		}
		checkNonNegative(t, snap, now, "tx "+name, i.tx)
	}
	for name, d := range snap.disks {
		if name != "vdb" {
			checkNonNegative(t, snap, now, "read "+name, d.readBps)
		}
		checkNonNegative(t, snap, now, "write "+name, d.writeBps)
		checkNonNegative(t, snap, now, "read IOPS "+name, d.readIOPS)
		checkNonNegative(t, snap, now, "write IOPS "+name, d.writeIOPS)
	}
	checkNonNegative(t, snap, now, "cpu usage", snap.cpu.usage)
}

// TC-HP-ERR15: Re-created, hot-plugged, new and vanished entities. Owner WI 3.3. Requirements: FR-11, FR-8, FR-9.
//
// - Setup: UBU with second-sample changes: an interface's `ifindex` changed (counters higher); a disk's `maj:min` changed; a new interface `veth9` only in the second sample; an interface present only in the first.
// - Steps: two samples; read rates.
// - Expected: re-created interface and re-plugged disk: null with `counter reset: …`; `veth9`: null with exactly `warming up`, never 0 and never `counter reset:` (Finding 7: an entity with fewer than two samples of its own is warming up); the vanished interface has no rate entry and causes no error.
func TestTCHPERR15_ReCreatedHotPluggedNewAndVanished(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	b := s.start()
	snap := s.waitSeq(1)
	if _, ok := snap.ifaces["docker0"]; !ok {
		t.Fatal("fixture: docker0 not in the first sample")
	}
	s.ffs.advance()
	// eth0 re-created: a new ifindex and higher counters; docker0 gone;
	// veth9 new; vdb re-plugged under a new major.
	s.ffs.replace("sys/class/net/eth0/ifindex", []byte("7\n"))
	s.ffs.replace(pathNetDev, []byte("Inter-|   Receive                                                |  Transmit\n"+
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"+
		"    lo:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0\n"+
		"  eth0:    9000      90    0    0    0     0          0         0      900       9    0    0    0     0       0          0\n"+
		"  veth9:    500       5    0    0    0     0          0         0       50       1    0    0    0     0       0          0\n"))
	s.ffs.replace(pathDiskstats, setDiskCol(t, s.ffs.rawRead(pathDiskstats), "vdb", 0, 254))
	s.fire(b, 10*time.Second)
	snap = s.waitSeq(2)
	now := s.clk.Now()

	wantMissing(t, snap, now, "eth0 rx", snap.ifaces["eth0"].rx, "counter reset: eth0")
	wantMissing(t, snap, now, "eth0 tx", snap.ifaces["eth0"].tx, "counter reset: eth0")
	d := snap.disks["vdb"]
	for _, v := range []rateVal{d.readBps, d.writeBps, d.readIOPS, d.writeIOPS} {
		wantMissing(t, snap, now, "vdb", v, "counter reset: vdb")
	}
	wantMissing(t, snap, now, "veth9 rx", snap.ifaces["veth9"].rx, reasonWarmingUp)
	wantMissing(t, snap, now, "veth9 tx", snap.ifaces["veth9"].tx, reasonWarmingUp)
	if _, ok := snap.ifaces["docker0"]; ok {
		t.Error("vanished docker0 has a rate entry")
	}
	if got := mustVal(t, snap, now, "lo rx", snap.ifaces["lo"].rx); got != 0 {
		t.Errorf("lo rx = %v, want 0", got)
	}
	checkNonNegative(t, snap, now, "vda read", snap.disks["vda"].readBps)
}

// TC-HP-ERR16: A reused pid is not ranked. Owner WI 3.3. Requirements: FR-10, FR-11.
//
// - Setup: UBU with `t1/proc/<pid>/stat` for the top-CPU pid rewritten with a different starttime and a large utime.
// - Steps: two samples; top lists.
// - Expected: that pid is absent from `topByCpu` for this interval; the rest of the list is unchanged in order.
func TestTCHPERR16_AReusedPidIsNotRanked(t *testing.T) {
	want := expectedTop(t, capUBU, nil)
	if len(want.byCPU) == 0 {
		t.Fatal("fixture: no ranked process")
	}
	top := want.byCPU[0].pid

	s := newSamplerStack(t, capUBU)
	b := s.start()
	s.waitSeq(1)
	s.ffs.advance()
	name := "proc/" + strconv.Itoa(top) + "/stat"
	f := strings.Fields(string(s.ffs.rawRead(name)))
	f[13] = "999999"                             // utime (field 14)
	f[21] = strconv.Itoa(atoiT(t, f[21]) + 5000) // starttime (field 22)
	s.ffs.replace(name, []byte(strings.Join(f, " ")+"\n"))
	s.fire(b, 10*time.Second)
	snap := s.waitSeq(2)

	byCPU, _, _, why := snap.processes(s.clk.Now())
	if why != "" {
		t.Fatalf("processes unavailable: %q", why)
	}
	for _, p := range byCPU {
		if p.pid == top {
			t.Errorf("reused pid %d ranked: %+v", top, p)
		}
	}
	rest := expectedTop(t, capUBU, map[int]bool{top: true})
	if !samePids(byCPU, rest.byCPU) {
		t.Errorf("topByCpu %v, want %v (the list without pid %d, order unchanged)", pidsOf(byCPU), pidsOf(rest.byCPU), top)
	}
	if got, wantRest := pidsOf(byCPU)[:len(want.byCPU)-1], pidsOf(want.byCPU)[1:]; !slices.Equal(got, wantRest) {
		t.Errorf("topByCpu starts %v, want the earlier order %v", got, wantRest)
	}
}

// TC-HP-ERR17: A panic inside a tick is recovered and sampling resumes. Owner WI 3.3. Requirements: FR-11, NFR-4.
//
// - Setup: UBU F-FS `panicOn("proc/diskstats", 2)` (the second read only); logger capturing.
// - Steps: immediate sample; tick 1 (+10 s, panics); tick 2 (+10 s, after `advance`).
// - Expected: `RunBackground` still running after tick 1; one error log with a stack; the snapshot after tick 1 is unchanged (seq unchanged or state unchanged); after tick 2 `ready` with `intervalSeconds` 20 (deltas against the last good sample).
func TestTCHPERR17_APanicInsideATickIsRecovered(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	s.ffs.panicOn(pathDiskstats, 2)
	b := s.start()
	first := s.waitSeq(1)

	s.fire(b, 10*time.Second)
	eventually(t, 2*time.Second, func() bool { return len(s.logs.records(slog.LevelError, "")) >= 1 },
		"the panic logged")
	select {
	case <-b.done:
		t.Fatal("RunBackground returned after a panicking tick")
	default:
	}
	errs := s.logs.records(slog.LevelError, "")
	if len(errs) != 1 {
		t.Fatalf("%d error logs, want 1", len(errs))
	}
	if st := attr(errs[0], "stack"); !strings.Contains(st, "goroutine") || !strings.Contains(st, "readSample") {
		t.Errorf("error log stack = %q, want the panicking goroutine's stack through readSample", st)
	}
	if got := s.p.run.Load().snapshot(); got != first || got.state(s.clk.Now()) != spec.SamplerWarmingUp {
		t.Errorf("snapshot after the panicking tick = %v, want the first one, still warming up", got)
	}

	s.ffs.advance()
	s.fire(b, 10*time.Second)
	snap := s.waitSeq(2)
	now := s.clk.Now()
	if got := snap.state(now); got != spec.SamplerReady {
		t.Errorf("after tick 2: state = %q, want %q", got, spec.SamplerReady)
	}
	if sm := snap.sampler(now); sm.IntervalSeconds == nil || *sm.IntervalSeconds != 20 {
		t.Errorf("after tick 2: intervalSeconds = %v, want 20", sm.IntervalSeconds)
	}
	checkNonNegative(t, snap, now, "cpu usage", snap.cpu.usage)
}

// The expectations below are computed in-test from the capture files, by
// field position, independently of rates.go.

// capFile reads testdata/<capture>/<sample>/<name>; sample is fs or t1.
func capFile(t *testing.T, capture, sample, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", capture, sample, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func u64T(t *testing.T, s string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// cpuCols maps each cpu line of /proc/stat to its first eight columns:
// user nice system idle iowait irq softirq steal.
func cpuCols(t *testing.T, b []byte) map[string][8]uint64 {
	t.Helper()
	out := map[string][8]uint64{}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 9 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		var c [8]uint64
		for i := range c {
			c[i] = u64T(t, f[i+1])
		}
		out[f[0]] = c
	}
	return out
}

func sum8(c [8]uint64) uint64 {
	var s uint64
	for _, v := range c {
		s += v
	}
	return s
}

// diskCols maps each /proc/diskstats device to major, minor, reads
// completed, sectors read, writes completed and sectors written.
func diskCols(t *testing.T, b []byte) map[string][6]uint64 {
	t.Helper()
	out := map[string][6]uint64{}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 14 {
			continue
		}
		out[f[2]] = [6]uint64{u64T(t, f[0]), u64T(t, f[1]), u64T(t, f[3]), u64T(t, f[5]), u64T(t, f[7]), u64T(t, f[9])}
	}
	return out
}

// netCols maps each /proc/net/dev interface to its rx and tx bytes.
func netCols(t *testing.T, b []byte) map[string][2]uint64 {
	t.Helper()
	out := map[string][2]uint64{}
	for _, l := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(l, ":")
		f := strings.Fields(rest)
		if !ok || len(f) < 16 {
			continue
		}
		out[strings.TrimSpace(name)] = [2]uint64{u64T(t, f[0]), u64T(t, f[8])}
	}
	return out
}

// setNetCol sets counter col (0 is rx bytes) of iface in a /proc/net/dev.
func setNetCol(t *testing.T, b []byte, iface string, col int, v uint64) []byte {
	t.Helper()
	ls := strings.Split(string(b), "\n")
	for i, l := range ls {
		name, rest, ok := strings.Cut(l, ":")
		if ok && strings.TrimSpace(name) == iface {
			f := strings.Fields(rest)
			f[col] = strconv.FormatUint(v, 10)
			ls[i] = "  " + iface + ": " + strings.Join(f, " ")
			return []byte(strings.Join(ls, "\n"))
		}
	}
	t.Fatalf("no interface %s", iface)
	return nil
}

// setDiskCol sets whitespace field i (0 is the major) of disk in a
// /proc/diskstats.
func setDiskCol(t *testing.T, b []byte, disk string, i int, v uint64) []byte {
	t.Helper()
	ls := strings.Split(string(b), "\n")
	for n, l := range ls {
		f := strings.Fields(l)
		if len(f) >= 14 && f[2] == disk {
			f[i] = strconv.FormatUint(v, 10)
			ls[n] = strings.Join(f, " ")
			return []byte(strings.Join(ls, "\n"))
		}
	}
	t.Fatalf("no disk %s", disk)
	return nil
}

// checkNonNegative checks a computed value is present and not negative.
func checkNonNegative(t *testing.T, snap *snapshot, now time.Time, what string, v rateVal) {
	t.Helper()
	if x := mustVal(t, snap, now, what, v); x < 0 || math.IsNaN(x) || math.IsInf(x, 0) {
		t.Errorf("%s = %v, want a finite value ≥ 0", what, x)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// checkCPU is TC-HP-61's expectation for a ready fs → t1 snapshot.
func checkCPU(t *testing.T, capture string, snap *snapshot, now time.Time) {
	t.Helper()
	p, c := cpuCols(t, capFile(t, capture, "fs", pathStat)), cpuCols(t, capFile(t, capture, "t1", pathStat))
	pct := func(p, c [8]uint64, cols ...int) float64 {
		var d uint64
		for _, i := range cols {
			d += c[i] - p[i]
		}
		return float64(d) / float64(sum8(c)-sum8(p)) * 100
	}
	busy := func(p, c [8]uint64) float64 {
		total := sum8(c) - sum8(p)
		return float64(total-(c[3]-p[3])-(c[4]-p[4])) / float64(total) * 100
	}
	check := func(what string, got rateVal, want float64) {
		t.Helper()
		if x := mustVal(t, snap, now, what, got); !near(x, want) {
			t.Errorf("%s = %v, want %v", what, x, want)
		}
	}
	check("cpu usage", snap.cpu.usage, busy(p["cpu"], c["cpu"]))
	check("cpu user", snap.cpu.user, pct(p["cpu"], c["cpu"], 0))
	check("cpu system", snap.cpu.system, pct(p["cpu"], c["cpu"], 2))
	check("cpu iowait", snap.cpu.iowait, pct(p["cpu"], c["cpu"], 4))
	check("cpu steal", snap.cpu.steal, pct(p["cpu"], c["cpu"], 7))
	if len(snap.perCPU) != len(c)-1 {
		t.Errorf("%d per-CPU entries, want %d", len(snap.perCPU), len(c)-1)
	}
	for id, r := range snap.perCPU {
		name := "cpu" + strconv.Itoa(id)
		check(name+" usage", r.usage, busy(p[name], c[name]))
	}

	// vmstat ran concurrently with the two samples: 100 − id − wa of its
	// last line is the same interval's busy share, within ±5 pp.
	ls := strings.Split(strings.TrimSpace(string(capFile(t, capture, "tools", "vmstat"))), "\n")
	head, last := strings.Fields(ls[1]), strings.Fields(ls[len(ls)-1])
	col := func(name string) float64 {
		i := slices.Index(head, name)
		if i < 0 {
			t.Fatalf("tools/vmstat has no %s column", name)
		}
		return float64(atoiT(t, last[i]))
	}
	vm := 100 - col("id") - col("wa")
	if got := *snap.cpu.usage.v; math.Abs(got-vm) > 5 {
		t.Errorf("cpu usage %v is more than 5 pp from vmstat's %v", got, vm)
	}
}

// checkDisks is TC-HP-62's expectation.
func checkDisks(t *testing.T, capture string, snap *snapshot, now time.Time) {
	t.Helper()
	p, c := diskCols(t, capFile(t, capture, "fs", pathDiskstats)), diskCols(t, capFile(t, capture, "t1", pathDiskstats))
	if len(snap.disks) != len(c) {
		t.Errorf("%d disks, want %d", len(snap.disks), len(c))
	}
	for name, d := range snap.disks {
		for _, x := range []struct {
			what string
			got  rateVal
			want float64
		}{
			{"read bytes/s", d.readBps, float64(c[name][3]-p[name][3]) * 512 / 10},
			{"write bytes/s", d.writeBps, float64(c[name][5]-p[name][5]) * 512 / 10},
			{"read IOPS", d.readIOPS, float64(c[name][2]-p[name][2]) / 10},
			{"write IOPS", d.writeIOPS, float64(c[name][4]-p[name][4]) / 10},
		} {
			v := mustVal(t, snap, now, name+" "+x.what, x.got)
			if !near(v, x.want) || v < 0 {
				t.Errorf("%s %s = %v, want %v", name, x.what, v, x.want)
			}
		}
	}
}

// checkIfaces is TC-HP-63's expectation.
func checkIfaces(t *testing.T, capture string, snap *snapshot, now time.Time) {
	t.Helper()
	p, c := netCols(t, capFile(t, capture, "fs", pathNetDev)), netCols(t, capFile(t, capture, "t1", pathNetDev))
	both := 0
	for name := range c {
		if _, ok := p[name]; ok {
			both++
		}
	}
	if len(snap.ifaces) != both {
		t.Errorf("%d interfaces, want %d (one per interface in both samples)", len(snap.ifaces), both)
	}
	for name, i := range snap.ifaces {
		rx := mustVal(t, snap, now, name+" rx", i.rx)
		tx := mustVal(t, snap, now, name+" tx", i.tx)
		if wrx, wtx := float64(c[name][0]-p[name][0])/10, float64(c[name][1]-p[name][1])/10; !near(rx, wrx) ||
			!near(tx, wtx) || rx < 0 || tx < 0 {
			t.Errorf("%s rx/tx = %v/%v, want %v/%v", name, rx, tx, wrx, wtx)
		}
	}
}

// capturePIDs lists the pids of a capture sample, ascending.
func capturePIDs(t *testing.T, capture, sample string) []int {
	t.Helper()
	es, err := os.ReadDir(filepath.Join("testdata", capture, sample, "proc"))
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, e := range es {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

type topLists struct{ byCPU, byMem []procRate }

// expectedTop ranks the pids in both fs/ and t1/ with the same starttime,
// 10 s apart, leaving out exclude.
func expectedTop(t *testing.T, capture string, exclude map[int]bool) topLists {
	t.Helper()
	var all []procRate
	for _, pid := range capturePIDs(t, capture, "t1") {
		name := filepath.Join("proc", strconv.Itoa(pid), "stat")
		if exclude[pid] {
			continue
		}
		pb, err := os.ReadFile(filepath.Join("testdata", capture, "fs", name))
		if err != nil {
			continue // only in t1/
		}
		p := strings.Fields(string(pb[bytes.LastIndexByte(pb, ')')+1:]))
		cb := capFile(t, capture, "t1", name)
		c := strings.Fields(string(cb[bytes.LastIndexByte(cb, ')')+1:]))
		// p[0] is field 3, so field n is p[n-3].
		if p[22-3] != c[22-3] {
			continue
		}
		dt := u64T(t, c[14-3]) + u64T(t, c[15-3]) - u64T(t, p[14-3]) - u64T(t, p[15-3])
		comm := string(cb[bytes.IndexByte(cb, '(')+1 : bytes.LastIndexByte(cb, ')')])
		all = append(all, procRate{pid: pid, comm: comm, cpuPercent: float64(dt) / (100 * 10) * 100,
			rssBytes: u64T(t, c[24-3]) * 4096})
	}
	rank := func(key func(procRate) float64) []procRate {
		s := slices.Clone(all)
		sort.Slice(s, func(i, j int) bool {
			if ki, kj := key(s[i]), key(s[j]); ki != kj {
				return ki > kj
			}
			return s[i].pid < s[j].pid
		})
		return s[:min(5, len(s))]
	}
	return topLists{
		byCPU: rank(func(p procRate) float64 { return p.cpuPercent }),
		byMem: rank(func(p procRate) float64 { return float64(p.rssBytes) }),
	}
}

func pidsOf(ps []procRate) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.pid
	}
	return out
}

func samePids(a, b []procRate) bool { return slices.Equal(pidsOf(a), pidsOf(b)) }

// checkProcs is TC-HP-64's expectation.
func checkProcs(t *testing.T, capture string, snap *snapshot, now time.Time) {
	t.Helper()
	byCPU, byMem, secs, why := snap.processes(now)
	if why != "" || secs == nil || *secs != 10 {
		t.Fatalf("processes: %v s, %q; want 10 s", secs, why)
	}
	want := expectedTop(t, capture, nil)
	for _, l := range []struct {
		name      string
		got, want []procRate
	}{{"topByCpu", byCPU, want.byCPU}, {"topByMemory", byMem, want.byMem}} {
		if len(l.got) != len(l.want) || len(l.got) != 5 {
			t.Errorf("%s has %d, want %d (5)", l.name, len(l.got), len(l.want))
			continue
		}
		for i := range l.got {
			g, w := l.got[i], l.want[i]
			if g.pid != w.pid || g.comm != w.comm || g.rssBytes != w.rssBytes || !near(g.cpuPercent, w.cpuPercent) {
				t.Errorf("%s[%d] = %+v, want %+v", l.name, i, g, w)
			}
		}
	}

	// Every pid's ticks together fit in the CPU time of the interval.
	p, c := cpuCols(t, capFile(t, capture, "fs", pathStat)), cpuCols(t, capFile(t, capture, "t1", pathStat))
	var pidTicks float64
	for _, pid := range capturePIDs(t, capture, "t1") {
		name := filepath.Join("proc", strconv.Itoa(pid), "stat")
		pb, err := os.ReadFile(filepath.Join("testdata", capture, "fs", name))
		if err != nil {
			continue
		}
		ps, pc := strings.Fields(string(pb[bytes.LastIndexByte(pb, ')')+1:])), capFile(t, capture, "t1", name)
		cs := strings.Fields(string(pc[bytes.LastIndexByte(pc, ')')+1:]))
		if ps[19] == cs[19] {
			pidTicks += float64(u64T(t, cs[11]) + u64T(t, cs[12]) - u64T(t, ps[11]) - u64T(t, ps[12]))
		}
	}
	if total := float64(sum8(c["cpu"]) - sum8(p["cpu"])); pidTicks > total {
		t.Errorf("per-pid ticks %v exceed the CPU's %v", pidTicks, total)
	}
}

// histTimes is the history's stored timestamps, oldest first.
func histTimes(h *history) []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int64, h.count)
	for i := range out {
		out[i] = h.ts[(h.head+i)%h.capacity]
	}
	return out
}

// weakHistory is a weak pointer to the current run's history, which must
// hold n points. It keeps no strong reference.
func weakHistory(t *testing.T, p *Provider, n int) weak.Pointer[history] {
	t.Helper()
	h := p.run.Load().history()
	if got := len(histTimes(h)); got != n {
		t.Fatalf("history holds %d points, want %d", got, n)
	}
	return weak.Make(h)
}
