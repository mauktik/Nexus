// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-52, TC-HP-53, TC-HP-54, TC-HP-55, TC-HP-56, TC-HP-57, TC-HP-58, TC-HP-59, TC-HP-60, TC-HP-E12, TC-HP-E13.
// Owner WIs: 3.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-HP-52: History capacity is historyDuration over sampleInterval and overwrites the oldest. Owner WI 3.2. Requirements: FR-13, NFR-2.
//
// - Setup: capacities from `(1h, 10s)`, `(24h, 2s)`, `(10s, 10s)`.
// - Steps: build each; append `capacity + 1` points with cpuPercent = i.
// - Expected: capacities 360, 43,200, 1; after the extra append the ring holds `capacity` points, the oldest has cpuPercent 1 (point 0 overwritten), the newest `capacity`.
func TestTCHP52_HistoryCapacityIsHistoryDurationOverSampleIntervalAnd(t *testing.T) {
	t.Skip("TC-HP-52: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-53: Resize keeps the newest points. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: capacity 360 with 360 points (cpu = i).
// - Steps: `Resize(100)`; then `Resize(500)` and append 10.
// - Expected: after 100: points 260..359 in order; after 500 + 10: 110 points, 260..359 then the 10 new ones; per-entity rings resized identically.
func TestTCHP53_ResizeKeepsTheNewestPoints(t *testing.T) {
	t.Skip("TC-HP-53: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-54: Query aligns to the step and averages each bucket. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: 360 points at 10 s from `T0 = 2026-01-01T00:00:05Z`, cpu = i.
// - Steps: `Query(now = T0+3595s, window 3600, step 60, [cpu])`.
// - Expected: `from` = `max(now−window, oldest)` aligned down to 60 s; `stepSeconds` 60; each value equals the mean of the points whose timestamp falls in `[from+i·60, from+(i+1)·60)`, computed in-test from the appended series; `points == len(values)`; `historyStart` = T0.
func TestTCHP54_QueryAlignsToTheStepAndAverages(t *testing.T) {
	t.Skip("TC-HP-54: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-55: Buckets without samples are null. Owner WI 3.2. Requirements: FR-13, FR-12.
//
// - Setup: points at 10 s with a 5-minute gap (no appends).
// - Steps: `Query(window 3600, step 60)`.
// - Expected: the buckets inside the gap are `null` (JSON `null` in `values`), the others numbers; `assertExplicit` accepts the result (the documented `series[].values` exemption).
func TestTCHP55_BucketsWithoutSamplesAreNull(t *testing.T) {
	t.Skip("TC-HP-55: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-56: At most 500 points per series. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: capacity 43,200 at 2 s, full.
// - Steps: `Query(window 86400, step 0, [cpu])`.
// - Expected: `stepSeconds ≥ ceil(86400/500) = 173` (the right side computed in-test); every series has ≤ 500 values.
func TestTCHP56_AtMost500PointsPerSeries(t *testing.T) {
	t.Skip("TC-HP-56: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-57: At most 20,000 values per response. Owner WI 3.2. Requirements: FR-13, NFR-1.
//
// - Setup: capacity 1,800 at 2 s (1 h), 32 interfaces and 32 disks, all points filled.
// - Steps: `Query(window 3600, step 2, [cpu, load, memory, swap, network, disk])`.
// - Expected: `len(series) × points ≤ 20,000`; `stepSeconds > 2`; and the raise is not excessive: with `minimal` = the smallest multiple of the 2 s interval for which `len(series) × ceil(3600/step) ≤ 20,000` (computed in-test), `minimal ≤ stepSeconds ≤ 2 × minimal`.
func TestTCHP57_AtMost20000ValuesPerResponse(t *testing.T) {
	t.Skip("TC-HP-57: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-58: Default metrics are cpu, load, memory and swap. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: a filled history with 2 interfaces and 1 disk.
// - Steps: `Query` with no metric; then with `[network]`; then `[disk]`.
// - Expected: default series metrics are exactly `cpu`, `load`, `memory` (used and available), `swap`, with units declared; no network or disk series; `[network]` gives rx and tx per interface with `entity` = interface name; `[disk]` gives read and write per disk.
func TestTCHP58_DefaultMetricsAreCpuLoadMemoryAnd(t *testing.T) {
	t.Skip("TC-HP-58: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-59: Entity slots go to devices first and free after a full window unseen. Owner WI 3.2. Requirements: FR-13, NFR-2.
//
// - Setup: capacity 10; 33 interfaces: `eth0..eth9` with a device, `veth00..veth22` without.
// - Steps: append one point with all 33; query `[network]`; then append 10 points without `veth21` and `veth22` and with `wlan0` (a device) added.
// - Expected: all 10 devices hold slots; the 33rd by order (`veth22`) is denied and `entitiesTruncated.interfaces == 1`; `veth21` is then unseen for 10 appends (a full window, it is dropped from the later points too), its slot frees, and `wlan0` holds a slot in the next query with `entitiesTruncated.interfaces` reflecting only entities still denied.
func TestTCHP59_EntitySlotsGoToDevicesFirstAnd(t *testing.T) {
	t.Skip("TC-HP-59: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-60: History memory stays within the NFR-2 bounds. Owner WI 3.2. Requirements: NFR-2, FR-13.
//
// - Setup: `runtime.GC()` and `runtime.ReadMemStats` before and after.
// - Steps: build capacity 43,200 with 32 interfaces and 32 disks and fill it; separately capacity 360 filled.
// - Expected: the `HeapAlloc` delta (history kept alive with `runtime.KeepAlive`) is under the live-history budget A-27 records, and under 1 MiB at capacity 360. The budget is 16 MiB if A-27 adopts Finding 6 option (a), because twice the live heap must stay under 32 MiB, and 28 MiB if it adopts option (b). The stub holds it in one constant, `liveHistoryBudget`, with a comment citing A-27. This TC bounds the live heap only; TC-HP-133 bounds the heap including GC headroom, which is what NFR-2's RSS bound needs.
func TestTCHP60_HistoryMemoryStaysWithinTheNFR2(t *testing.T) {
	t.Skip("TC-HP-60: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-E12: Querying beyond the start of history, and an empty history. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: 6 points at 10 s; and an empty ring.
// - Steps: `Query(window 86400, step 0)` on each.
// - Expected: 6 points: `from` = oldest aligned down to the step, `historyStart` = oldest, no leading nulls before `historyStart`; empty: each default metric series with `values []`, `points 0`, `historyStart` null with `warming up`.
func TestTCHPE12_QueryingBeyondTheStartOfHistoryAnd(t *testing.T) {
	t.Skip("TC-HP-E12: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-HP-E13: Concurrent appends and queries are race-free. Owner WI 3.2. Requirements: FR-13, NFR-4.
//
// - Setup: capacity 360.
// - Steps: one goroutine appends 10,000 points and resizes every 1,000 while 4 goroutines query.
// - Expected: no race report; each query result is internally consistent (`points == len(values)` per series, timestamps increasing).
func TestTCHPE13_ConcurrentAppendsAndQueriesAreRaceFree(t *testing.T) {
	t.Skip("TC-HP-E13: SKIP-only stub; un-skip when WI 3.2 lands")
}
