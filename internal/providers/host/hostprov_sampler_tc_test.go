// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-61, TC-HP-62, TC-HP-63, TC-HP-64, TC-HP-65, TC-HP-66, TC-HP-67, TC-HP-68, TC-HP-69, TC-HP-70, TC-HP-E14, TC-HP-E15, TC-HP-E16, TC-HP-E17, TC-HP-ERR14, TC-HP-ERR15, TC-HP-ERR16, TC-HP-ERR17.
// Owner WIs: 3.3. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-HP-61: CPU percentages from the two captured samples follow A-6 and agree with vmstat. Owner WI 3.3. Requirements: FR-11, FR-6, NFR-3.
//
// - Setup: host stack per capture; ready snapshot (fs → t1, F-CLK 10 s).
// - Steps: read the snapshot's CPU fields.
// - Expected: total and each per-CPU `usagePercent` equal `(Δtotal − Δidle − Δiowait)/Δtotal × 100` computed in-test from `fs/proc/stat` and `t1/proc/stat` (|diff| < 1e-9); user/system/iowait/steal shares equal their deltas over Δtotal × 100; total within ±5 pp of `100 − id − wa` from the last line of `tools/vmstat`.
func TestTCHP61_CPUPercentagesFromTheTwoCapturedSamples(t *testing.T) {
	t.Skip("TC-HP-61: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-62: Disk throughput and IOPS from diskstats deltas. Owner WI 3.3. Requirements: FR-8, FR-11.
//
// - Setup: ready snapshot per capture.
// - Steps: read each disk's rates.
// - Expected: read/write bytes/s = Δsectors × 512 / 10 and IOPS = Δcompleted / 10, computed in-test from `fs/` and `t1/` diskstats; every value ≥ 0.
func TestTCHP62_DiskThroughputAndIOPSFromDiskstatsDeltas(t *testing.T) {
	t.Skip("TC-HP-62: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-63: Interface rates from net/dev deltas. Owner WI 3.3. Requirements: FR-9, FR-11.
//
// - Setup: ready snapshot per capture.
// - Steps: read each interface's rates.
// - Expected: rx/tx bytes/s = Δbytes / 10 computed in-test; ≥ 0; one entry per interface present in both samples with the same `ifindex`.
func TestTCHP63_InterfaceRatesFromNetDevDeltas(t *testing.T) {
	t.Skip("TC-HP-63: SKIP-only stub; un-skip when WI 3.3 lands")
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
	t.Skip("TC-HP-64: SKIP-only stub; un-skip when WI 3.3 lands")
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
	t.Skip("TC-HP-65: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-66: A sample older than three intervals is stale. Owner WI 3.3. Requirements: FR-11, NFR-4.
//
// - Setup: ready snapshot (interval 10 s).
// - Steps: `F-CLK.Advance(30s)`, read; `F-CLK.Advance(1s)`, read.
// - Expected: at age 30 s (= 3 × interval, not over): `ready`, `ageSeconds` 30; at 31 s: `stale`, reason `stale: last sample 31s old`, computed values null, `ageSeconds` 31.
func TestTCHP66_ASampleOlderThanThreeIntervalsIs(t *testing.T) {
	t.Skip("TC-HP-66: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-67: RunBackground samples at once, ticks, and stops within one tick of cancel. Owner WI 3.3. Requirements: FR-11, NFR-2.
//
// - Setup: host stack (UBU); goroutine baseline.
// - Steps: start `RunBackground(ctx)` in a goroutine; `waitSeq(1)`; fire 2 ticks; `cancel()`.
// - Expected: seq 1 without any tick; seq 3 after two; after cancel, `RunBackground` returns within 100 ms with no further tick fired; F-TICK recorded `Stop`; `p.run.Load() == nil`; goroutines back to baseline (`eventually`).
func TestTCHP67_RunBackgroundSamplesAtOnceTicksAndStops(t *testing.T) {
	t.Skip("TC-HP-67: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-68: A changed interval resets the ticker and a changed duration resizes history. Owner WI 3.3. Requirements: FR-11, FR-13.
//
// - Setup: running sampler with 10 s / 1 h and 5 points.
// - Steps: `Configure({sampleInterval: 2s, historyDuration: 4s})`; fire one tick.
// - Expected: F-TICK recorded `Reset(2s)` once; history capacity 2 holding the 2 newest points; info log "sampler interval changed" once.
func TestTCHP68_AChangedIntervalResetsTheTickerAnd(t *testing.T) {
	t.Skip("TC-HP-68: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-69: An older overlapping run cannot clear a newer run's state. Owner WI 3.3. Requirements: FR-11, NFR-4.
//
// - Setup: host stack; two F-TICKs handed out in order.
// - Steps: start run A; start run B (A still running); cancel A and wait for it to return.
// - Expected: `p.run.Load()` is B's run; reads use B's snapshot; cancelling B then clears it.
func TestTCHP69_AnOlderOverlappingRunCannotClearA(t *testing.T) {
	t.Skip("TC-HP-69: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-70: The process scan is throttled by its measured cost. Owner WI 3.3. Requirements: FR-10, NFR-2.
//
// - Setup: host stack (UBU, N pids in the capture); F-FS `slow("proc/*/stat", F-CLK.Advance(d))` with `d` chosen so the scan costs 1.2% of the 10 s interval (120 ms total).
// - Steps: fire 6 ticks; read `processSampleSeconds` and the per-tick scan record.
// - Expected: `scanEvery = ceil(0.12 / 0.05) = 3` (computed in-test from the injected cost); scans happen on every 3rd tick; `processSampleSeconds` = 30; one info log line about throttling; with `d` making the cost 10%, `scanEvery` is capped at 6.
func TestTCHP70_TheProcessScanIsThrottledByIts(t *testing.T) {
	t.Skip("TC-HP-70: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-E14: Huge 64-bit counters and 32-bit wraps. Owner WI 3.3. Requirements: FR-11.
//
// - Setup: two samples derived from UBU's `proc/net/dev`: eth0 rx `2^64 − 1000` → `2^64 − 10` (Δ 990); eth1 rx `2^32 − 10` → `5` (a 32-bit wrap).
// - Steps: `compute(prev, cur, 10s)`.
// - Expected: eth0 rx 99 B/s exactly (no overflow); eth1 rx null with `counter reset: eth1` (A-9: no wrap arithmetic), never a huge or negative number.
func TestTCHPE14_Huge64BitCountersAnd32Bit(t *testing.T) {
	t.Skip("TC-HP-E14: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-E15: RunBackground returns at once off Linux. Owner WI 3.3. Requirements: CON-5, FR-11.
//
// - Setup: `GOOS: "darwin"`.
// - Steps: `RunBackground(ctx)` with a live ctx.
// - Expected: returns within 10 ms; `p.run` nil; F-TICK never created; F-FS recorded nothing.
func TestTCHPE15_RunBackgroundReturnsAtOnceOffLinux(t *testing.T) {
	t.Skip("TC-HP-E15: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-E16: A stopped provider holds no history and a new run starts empty. Owner WI 3.3. Requirements: NFR-2, FR-13, ASM-3.
//
// - Setup: a run with 10 points.
// - Steps: cancel; `runtime.GC()`; start a new run; read metrics.
// - Expected: after cancel no reference to the old history remains (a `runtime.SetFinalizer`/weak pointer on it fires after GC); the new run's history has 1 point (its immediate sample) and its state is `warming-up`.
func TestTCHPE16_AStoppedProviderHoldsNoHistoryAnd(t *testing.T) {
	t.Skip("TC-HP-E16: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-E17: One unreadable counter source affects only its own values. Owner WI 3.3. Requirements: FR-12, FR-11.
//
// - Setup: F-FS(UBU) `deny("proc/diskstats")`.
// - Steps: two samples.
// - Expected: state `ready`; CPU and interface rates populated; disk rates null with `permission denied: /proc/diskstats`.
func TestTCHPE17_OneUnreadableCounterSourceAffectsOnlyIts(t *testing.T) {
	t.Skip("TC-HP-E17: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-ERR14: A counter decrease gives no rate for that entity only. Owner WI 3.3. Requirements: FR-11.
//
// - Setup: UBU with `t1/proc/net/dev` replaced so one interface's rx bytes is below its `fs/` value (same ifindex), and one disk's sectors read below.
// - Steps: two samples.
// - Expected: that interface's rx rate and that disk's read rate null with `counter reset: <name>`; their tx/write rates and every other entity populated; no negative value anywhere.
func TestTCHPERR14_ACounterDecreaseGivesNoRateFor(t *testing.T) {
	t.Skip("TC-HP-ERR14: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-ERR15: Re-created, hot-plugged, new and vanished entities. Owner WI 3.3. Requirements: FR-11, FR-8, FR-9.
//
// - Setup: UBU with second-sample changes: an interface's `ifindex` changed (counters higher); a disk's `maj:min` changed; a new interface `veth9` only in the second sample; an interface present only in the first.
// - Steps: two samples; read rates.
// - Expected: re-created interface and re-plugged disk: null with `counter reset: …`; `veth9`: null with a non-empty reason (`warming up` or `counter reset:`), never 0 (open item 3); the vanished interface has no rate entry and causes no error.
func TestTCHPERR15_ReCreatedHotPluggedNewAndVanished(t *testing.T) {
	t.Skip("TC-HP-ERR15: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-ERR16: A reused pid is not ranked. Owner WI 3.3. Requirements: FR-10, FR-11.
//
// - Setup: UBU with `t1/proc/<pid>/stat` for the top-CPU pid rewritten with a different starttime and a large utime.
// - Steps: two samples; top lists.
// - Expected: that pid is absent from `topByCpu` for this interval; the rest of the list is unchanged in order.
func TestTCHPERR16_AReusedPidIsNotRanked(t *testing.T) {
	t.Skip("TC-HP-ERR16: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-HP-ERR17: A panic inside a tick is recovered and sampling resumes. Owner WI 3.3. Requirements: FR-11, NFR-4.
//
// - Setup: UBU F-FS `panicOn("proc/diskstats", 2)` (the second read only); logger capturing.
// - Steps: immediate sample; tick 1 (+10 s, panics); tick 2 (+10 s, after `advance`).
// - Expected: `RunBackground` still running after tick 1; one error log with a stack; the snapshot after tick 1 is unchanged (seq unchanged or state unchanged); after tick 2 `ready` with `intervalSeconds` 20 (deltas against the last good sample).
func TestTCHPERR17_APanicInsideATickIsRecovered(t *testing.T) {
	t.Skip("TC-HP-ERR17: SKIP-only stub; un-skip when WI 3.3 lands")
}
