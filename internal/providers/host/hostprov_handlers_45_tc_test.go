// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-98, TC-HP-99, TC-HP-100, TC-HP-101, TC-HP-102, TC-HP-103, TC-HP-E26, TC-HP-ERR26, TC-HP-ERR27, TC-HP-134.
// Owner WI: 4.5. Split from hostprov_handlers_tc_test.go by the orchestrator so
// that Phase 4 work items can run in parallel. Each stub compiles today and
// skips; the developer of the owning work item replaces the skip and the
// comment with the test (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-HP-98: Summary values equal the detail operations. Owner WI 4.5. Requirements: FR-4, FR-15.
//
// - Setup: host stack per capture, ready snapshot, F-CLK frozen so all calls see the same snapshot.
// - Steps: summary, identity, cpu, memory, filesystems, interfaces, netconfig, health handlers.
// - Expected: each summary field equals the same field of its detail op (identity subset, cpu `model`/`logicalCpus`/`usagePercent`/`load`, memory subset, each filesystem's `mountPoint`/`sizeBytes`/`usedPercent`, each listed interface's state/rates, gateways, reboot state, failed count, OOM kills); no process list in the summary; one top-level `sampler`; `assertExplicit` passes.
func TestTCHP98_SummaryValuesEqualTheDetailOperations(t *testing.T) {
	t.Skip("TC-HP-98: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-99: The summary is at most 16 KiB on a 32-mount, 32-interface host. Owner WI 4.5. Requirements: FR-4, NFR-7.
//
// - Setup: the synthetic large host of test-strategy.md (32 mounts with 64-byte mount points, each added with `addMount(…, both)` and its own fresh `maj:min`, so each is visible and is its own dedupe group; 32 interfaces with 15-byte names, IPv4 + IPv6 each), warming up (all sampler fields null with reasons, the largest shape).
// - Steps: summary handler; `json.Marshal` the full list response.
// - Expected: ≤ 16,384 bytes; `filesystemsTruncated` and `interfacesTruncated` false; totals 32 + the capture's own.
func TestTCHP99_TheSummaryIsAtMost16KiB(t *testing.T) {
	t.Skip("TC-HP-99: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-100: Summary interface order and primary addresses. Owner WI 4.5. Requirements: FR-4.
//
// - Setup: UBU plus F-ADDRS variants for one interface: [link-local, ULA, global] in that kernel order; [link-local, ULA]; [link-local]; no IPv4.
// - Steps: summary handler.
// - Expected: loopback excluded; interfaces with a `device` first, then by name; primary IPv6 = the global, then ULA, then link-local per variant; primary IPv4 = first in kernel order; no IPv4 → `ipv4` null with `not reported: no IPv4 address`.
func TestTCHP100_SummaryInterfaceOrderAndPrimaryAddresses(t *testing.T) {
	t.Skip("TC-HP-100: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-101: Metrics operation over a running sampler. Owner WI 4.5. Requirements: FR-13, FR-11.
//
// - Setup: host stack (UBU), 20 ticks fired (alternating fs/t1 via `F-FS.advance` toggles, F-CLK +10 s each).
// - Steps: metrics with `{}`; `{metric: [network, disk]}`; `{windowSeconds: 86400}`; then with no run (provider never started).
// - Expected: default → cpu, load, memory, swap series with `stepSeconds` 10 and 20 points; network/disk → per-entity series; 86400 window → `from` = `historyStart` aligned; no run → `series []`, `points 0`, `sampler.state not-running` with reason `not running`; `assertExplicit` passes.
func TestTCHP101_MetricsOperationOverARunningSampler(t *testing.T) {
	t.Skip("TC-HP-101: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-102: Nothing outside the allow-list is ever read, and no sensitive path is touched. Owner WI 4.5. Requirements: NFR-6, CON-1.
//
// - Setup: host stack per capture with F-FS recording (F-SPY-FS).
// - Steps: Preflight; `RunBackground` with 3 ticks; every one of the ten handlers once (lists with limit 1000); stop.
// - Expected:
//   - Every recorded access matches an allow-list entry of its access kind, checked after the fact with `allowedAs(path, kind)` (Finding 1): an `Open` needs a read entry, a `ReadDir` a list entry, and a `Stat` or `ReadLink` a read or stat-only entry.
//   - No recorded path matches `(?i)(cmdline|environ|serial|uuid|asset_tag|kmsg|shadow|/arp$|neigh|/mem$|kcore|/fd/|/maps$)`.
//   - No recorded path is a mount point of the capture's mountinfo, other than allow-listed files that happen to be mount points. Mount points are reached only through F-STATFS (Finding 2).
//   - The recording is non-vacuous. It includes an `Open` of `proc/1/mountinfo`, of `proc/self/mountinfo` (visibility, D-9), of `sys/class/dmi/id/sys_vendor` and of at least one `proc/<pid>/stat`, a `ReadDir` of `proc` (process scan), and a `Stat` of at least one clock-source marker (`run/systemd/timesync` on UBU, from the clock-source check; `usr/bin/chronyc` on ALP, from the chronyc check).
func TestTCHP102_NothingOutsideTheAllowListIsEver(t *testing.T) {
	t.Skip("TC-HP-102: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-103: Every operation answers with explicit unavailability under every fault profile. Owner WI 4.5. Requirements: FR-12, FR-3, NFR-4.
//
// - Setup: each capture × profiles {none, SP-SANDBOX, SP-DENY-ALL, SP-NO-PSI, SP-VM-NO-SENSORS} × sampler states {not-running, warming-up, ready, stale}.
// - Steps: call all ten handlers.
// - Expected: no handler returns an error; every output passes `assertExplicit`; under SP-DENY-ALL every non-Preflight field is null or `[]` with a `permission denied: /…` reason; under SP-SANDBOX the denied fields carry `permission denied: <their path>` and nothing else is unavailable that is available with no profile.
func TestTCHP103_EveryOperationAnswersWithExplicitUnavailabilityUnder(t *testing.T) {
	t.Skip("TC-HP-103: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-E26: Lists over the summary bound are cut with an indicator, never silently. Owner WI 4.5. Requirements: FR-4.
//
// - Setup: the synthetic large host with 40 extra mounts (each added with `addMount(…, both)` and its own fresh `maj:min`) and 40 extra interfaces.
// - Steps: summary handler.
// - Expected: 32 filesystems, `filesystemsTotal` = 40 + the capture's, `filesystemsTruncated true`; 32 interfaces, `interfacesTotal` likewise, `interfacesTruncated true`; the kept filesystems are the first 32 by mount point.
func TestTCHPE26_ListsOverTheSummaryBoundAreCut(t *testing.T) {
	t.Skip("TC-HP-E26: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-ERR26: One hung mount keeps the summary under its bound. Owner WI 4.5. Requirements: FR-4, NFR-1.
//
// - Setup: UBU with SP-HUNG-NFS (`/mnt/nfs`, fresh `maj:min` `0:250`, added with `addMount(…, both)`, so the seam is reached; the hang covers the whole seam call, including the directory check; Finding 2); timeout 250 ms; plus F-RUN `hang` on the failed-units argv.
// - Steps: summary handler; measure.
// - Expected: returns within 550 ms (`sourceTimeout` + 300 ms) even with two hung sources (collectors run concurrently); `/mnt/nfs` usage and `health.failedServicesCount` null with their `timed out` reasons (not `not visible:` for `/mnt/nfs`).
func TestTCHPERR26_OneHungMountKeepsTheSummaryUnder(t *testing.T) {
	t.Skip("TC-HP-ERR26: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-ERR27: Metrics input refusals in the handler. Owner WI 4.5. Requirements: FR-13, FR-14.
//
// - Setup: host stack with a run.
// - Steps: call the handler directly with `stepSeconds 1`; and through a test server (dispatcher validation) with `windowSeconds 10`.
// - Expected: both are `*problem.Error` with slug `validation-failed` naming the field (`stepSeconds`, `windowSeconds`); the history is unchanged afterwards.
func TestTCHPERR27_MetricsInputRefusalsInTheHandler(t *testing.T) {
	t.Skip("TC-HP-ERR27: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-134: The sampler with a summary and a 24 h metrics call on every tick keeps the heap under 32 MiB. Owner WI 4.5. Requirements: NFR-2, FR-13, FR-4.
//
// - Setup:
//   - Finding 16 splits this out of TC-HP-133, so the shape that needs WI 4.5's handlers is owned by WI 4.5.
//   - The synthetic large host of test-strategy.md, generated from UBU into `t.TempDir()`: 32 extra interfaces, 32 extra block devices with diskstats lines, and 300 extra pids. The pids are copies of one captured `proc/<pid>/{stat,status}` pair with the pid fields rewritten, so each tick's process scan reads and parses a realistic volume.
//   - `debug.SetGCPercent(100)` (restored in cleanup).
//   - Baseline: `runtime.GC()`, `debug.FreeOSMemory()`, then `runtime.ReadMemStats`.
//
// - Steps:
//  1. Configure `historyDuration: 24h`, `sampleInterval: 2s`.
//  2. Fill the history ring to capacity (21,600 stored points for every slot; A-40) through the history API, as TC-HP-60 does.
//  3. Start `RunBackground`. Fire 200 ticks with F-TICK, alternating `F-FS.advance()` between the two dynamic samples and advancing F-CLK 2 s per tick. After each tick, call the summary handler and `host.metrics.get` for the full window once.
//  4. Read `runtime.ReadMemStats` after every 20th tick, with no forced GC.
//
// - Expected:
//   - Every reading of `HeapSys − HeapReleased` is below baseline + 32 MiB.
//   - The maximum and every reading are logged with `t.Logf`, so WI 5.4 can compare them with the live RSS of TC-HP-130.
//   - The ring still holds 21,600 points per slot at the end: the ticks overwrote, not grew.
func TestTCHP134_TheSamplerWithASummaryAndA(t *testing.T) {
	t.Skip("TC-HP-134: SKIP-only stub; un-skip when WI 4.5 lands")
}
