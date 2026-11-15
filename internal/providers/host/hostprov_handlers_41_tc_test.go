// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-71, TC-HP-72, TC-HP-73, TC-HP-74, TC-HP-75, TC-HP-76, TC-HP-77, TC-HP-78, TC-HP-E18, TC-HP-E19, TC-HP-ERR18, TC-HP-ERR19, TC-HP-ERR20, TC-HP-ERR28.
// Owner WI: 4.1. Split from hostprov_handlers_tc_test.go by the orchestrator so
// that Phase 4 work items can run in parallel. Each stub compiles today and
// skips; the developer of the owning work item replaces the skip and the
// comment with the test (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-HP-71: Identity on both captures matches the hosts' tools. Owner WI 4.1. Requirements: FR-5, FR-4, NFR-3, FR-15.
//
// - Setup: host stack per capture.
// - Steps: call the identity handler; decode into `apitypes.List[apitypes.Resource[spec.IdentityProperties]]`.
// - Expected:
//   - one item named `current`, `host: "tc"`;
//   - `hostname` = `tools/hostname`; `os` = the capture's os-release ID, VERSION_ID, PRETTY_NAME; `kernelRelease` and `architecture` = `tools/uname-rm`; `initSystem` = the platform's;
//   - `bootTime` = `btime` as RFC 3339; `uptimeSeconds` = `proc/uptime` first field;
//   - `virtualization.type` = `tools/systemd-detect-virt` (where captured);
//   - `dmi` fields equal the five files, trimmed; `timezone` per TC-HP-45; `assertExplicit` passes.
func TestTCHP71_IdentityOnBothCapturesMatchesTheHosts(t *testing.T) {
	t.Skip("TC-HP-71: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-72: Identity and capabilities never disagree on OS and init system. Owner WI 4.1. Requirements: FR-5.
//
// - Setup: host stack with `Platform: nil` (production default) on the dev machine.
// - Steps: call identity; compute `platform.Current()`.
// - Expected: `os.id`, `os.versionId`, `os.prettyName`, `initSystem` equal `platform.Current()`'s, which is what `internal/core/capabilities.go` reports.
func TestTCHP72_IdentityAndCapabilitiesNeverDisagreeOnOS(t *testing.T) {
	t.Skip("TC-HP-72: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-73: Clock synchronization on systemd comes from timedatectl. Owner WI 4.1. Requirements: FR-5, CON-1, CON-3.
//
// - Setup: host stack (UBU, init systemd); F-RUN answers `tools/timedatectl`; variants where F-FS has `run/systemd/timesync`, `run/chrony/chronyd.pid`, `run/ntpd.pid`, none.
// - Steps: identity handler.
// - Expected: exactly one F-RUN call `{"/usr/bin/timedatectl","show","--property=NTPSynchronized","--value"}`; `clock.synchronized` = `"true"` for `yes`, `"false"` for `no`; `clock.source` = `systemd-timesyncd`, `chrony`, `ntpd`, `kernel` per variant.
func TestTCHP73_ClockSynchronizationOnSystemdComesFromTimedatectl(t *testing.T) {
	t.Skip("TC-HP-73: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-74: Clock synchronization elsewhere comes from chronyc or is unknown. Owner WI 4.1. Requirements: FR-5.
//
// - Setup: host stack (ALP, init openrc); variant 1: F-FS has `usr/bin/chronyc`, F-RUN answers `tools/chronyc-tracking` (or a capture-derived line); variant 2: no chronyc.
// - Steps: identity handler.
// - Expected: v1: one call `{"/usr/bin/chronyc","-c","tracking"}`, `synchronized` per field 14, source `chrony`; v2: no F-RUN call, `synchronized "unknown"`, `unavailable.synchronized` (or `clock`) = `not supported: no supported time-sync query (systemd-timedated, chrony)`.
func TestTCHP74_ClockSynchronizationElsewhereComesFromChronycOr(t *testing.T) {
	t.Skip("TC-HP-74: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-75: The provider never makes a clock syscall. Owner WI 4.1. Requirements: CON-1, NFR-4.
//
// - Setup: none.
// - Steps: grep every non-test `.go` file under `internal/providers/host` (recursively) for `Adjtimex`, `ClockAdjtime`, `adjtimex`, `clock_adjtime`, `Settimeofday`, `ClockSettime`, `SYS_ADJTIMEX`, `SYS_CLOCK_ADJTIME`.
// - Expected: no match; a planted temp copy with `unix.Adjtimex` is caught.
func TestTCHP75_TheProviderNeverMakesAClockSyscall(t *testing.T) {
	t.Skip("TC-HP-75: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-76: CPU detail from both captures. Owner WI 4.1. Requirements: FR-6, FR-15.
//
// - Setup: host stack per capture, ready snapshot.
// - Steps: CPU handler; decode into `spec.CPUProperties`.
// - Expected:
//   - `model` = cpuinfo first model name; `logicalCpus` = `tools/nproc`; `physicalCores` = distinct (`physical_package_id`, `core_id`) pairs, or cpuinfo `cpu cores` when topology is absent;
//   - `load` = `proc/loadavg`; per-CPU `frequencyMhz` = `scaling_cur_freq`/1000 where present, else cpuinfo MHz; `frequencyMhz` = their mean;
//   - `temperatures`: one per hwmon `temp*_input` (label = `temp*_label`, else hwmon `name`) and per thermal zone (label = `type`), `celsius` = value/1000, `source` hwmon/thermal;
//   - usage fields equal TC-HP-61's; `sampler.state` ready; `assertExplicit` passes.
func TestTCHP76_CPUDetailFromBothCaptures(t *testing.T) {
	t.Skip("TC-HP-76: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-77: Memory detail follows the free definitions. Owner WI 4.1. Requirements: FR-6, NFR-3.
//
// - Setup: host stack per capture.
// - Steps: memory handler.
// - Expected:
//   - from the captured meminfo (×1024): `usedBytes` = MemTotal − MemAvailable; `usedPercent` = used/total × 100; `cachedBytes` = Cached + SReclaimable; `sharedBytes` = Shmem; `swapUsedBytes` = SwapTotal − SwapFree; `buffersBytes`, `freeBytes`, `availableBytes`, `swapFreeBytes` direct;
//   - `totalBytes` = `tools/free` total exactly; `availableBytes` within ±1% of free's available;
//   - `pressure.{cpu,memory,io}` equal the PSI files where present; `assertExplicit` passes; the resource is not marked untrusted.
func TestTCHP77_MemoryDetailFollowsTheFreeDefinitions(t *testing.T) {
	t.Skip("TC-HP-77: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-78: CPU sampler fields report the sampler state, other fields stay live. Owner WI 4.1. Requirements: FR-11, FR-12.
//
// - Setup: host stack (UBU) in three states: no run (not-running), one sample (warming-up), stale (TC-HP-66).
// - Steps: CPU handler in each state.
// - Expected: `usagePercent`, `times`, each `perCpu[].usagePercent` null with `not running`, `warming up`, or `stale: last sample Ns old` respectively; `model`, `logicalCpus`, `load`, `frequencyMhz` populated in all three; `sampler.state` matches; `assertExplicit` passes.
func TestTCHP78_CPUSamplerFieldsReportTheSamplerState(t *testing.T) {
	t.Skip("TC-HP-78: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-E18: A VM with no sensors reports an empty temperature list with a reason. Owner WI 4.1. Requirements: FR-6, FR-12.
//
// - Setup: UBU with SP-VM-NO-SENSORS.
// - Steps: CPU handler.
// - Expected: `temperatures []`; `unavailable.temperatures` = `not reported: no temperature sensors (common in VMs)`.
func TestTCHPE18_AVMWithNoSensorsReportsAn(t *testing.T) {
	t.Skip("TC-HP-E18: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-E19: Hostname comes from the kernel file, not os.Hostname. Owner WI 4.1. Requirements: FR-5, ASM-1.
//
// - Setup: UBU F-FS with `replace("proc/sys/kernel/hostname", "tc-hp-host\n")`.
// - Steps: identity handler.
// - Expected: `hostname "tc-hp-host"` (differs from the dev machine's `os.Hostname()`); F-FS recorded `proc/sys/kernel/hostname`.
func TestTCHPE19_HostnameComesFromTheKernelFileNot(t *testing.T) {
	t.Skip("TC-HP-E19: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-ERR18: Clock command failures are reported with a reason. Owner WI 4.1. Requirements: FR-5, FR-12.
//
// - Setup: UBU with F-RUN for timedatectl: (a) exit 1 with `Failed to connect to bus`; (b) an `ErrNotStarted` (ENOENT) error; (c) `hang` (timeout 250 ms); (d) exit 0 with `maybe`; (e) an `ErrNotStarted` whose cause is `EACCES` (not ENOENT). F-RUN builds (b) and (e) as `fmt.Errorf("%w: %w", svcctl.ErrNotStarted, &fs.PathError{Op: "fork/exec", Path: argv0, Err: cause})`, the shape `realRun` returns (`internal/svcctl/svcctl.go:205`).
// - Steps: identity handler.
// - Expected: `clock.synchronized` unavailable with (a) `command failed: /usr/bin/timedatectl exit 1`, (b) `not present: /usr/bin/timedatectl` (Finding 7), (c) `timed out after 250ms: /usr/bin/timedatectl`, (d) `unrecognized format: …`, (e) `command failed: /usr/bin/timedatectl not started` (Finding 7); the handler returns within 350 ms in (c); the rest of identity is populated; the output never contains the command's text (`Failed to connect`).
func TestTCHPERR18_ClockCommandFailuresAreReportedWithA(t *testing.T) {
	t.Skip("TC-HP-ERR18: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-ERR19: A blocked sensor read times out without holding the CPU op. Owner WI 4.1. Requirements: FR-6, FR-12, NFR-1.
//
// - Setup: UBU (or a capture-derived hwmon) with F-FS `block` on one `temp*_input`; timeout 250 ms; goroutine baseline taken with `runtime.NumGoroutine()` before the first call.
// - Steps: call the CPU handler 5 times in sequence while the sensor stays blocked; measure each; then `release()`.
// - Expected:
//   - Each call returns within 350 ms.
//   - Call 1: the blocked sensor is absent from `temperatures` (or has null `celsius`) with `timed out after 250ms: /sys/class/hwmon/…`. Other sensors are present.
//   - Calls 2 to 5: the same sensor reports `still pending: /sys/class/hwmon/…` (Finding 3).
//   - F-FS recorded exactly one `Open` of that sensor path across the 5 calls.
//   - After call 5, `runtime.NumGoroutine()` is at most baseline + 1: one goroutine per stuck source, not one per request.
//   - After `release`, the goroutine count returns to the baseline (`eventually`).
func TestTCHPERR19_ABlockedSensorReadTimesOutWithout(t *testing.T) {
	t.Skip("TC-HP-ERR19: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-ERR20: The sandbox profile yields precise reasons for identity, CPU and memory. Owner WI 4.1. Requirements: FR-12, CON-1, NFR-6.
//
// - Setup: UBU with SP-SANDBOX; separately SP-NO-PSI; separately `remove` every `cpufreq/scaling_cur_freq`.
// - Steps: identity, CPU, memory handlers.
// - Expected: each `dmi.*` null with `permission denied: /sys/class/dmi/id/<file>`; `pressure.cpu|memory|io` null with `permission denied: /proc/pressure/<x>` (SP-SANDBOX) or `not present: /proc/pressure/<x> (kernel without PSI)` (SP-NO-PSI); without cpufreq, per-CPU frequency falls back to cpuinfo MHz with no reason; `assertExplicit` passes for all.
func TestTCHPERR20_TheSandboxProfileYieldsPreciseReasonsFor(t *testing.T) {
	t.Skip("TC-HP-ERR20: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-ERR28: assertExplicit catches each kind of violation. Owner WI 4.1. Requirements: FR-12.
//
// - Setup: a fake `testing.TB` that records failures; planted JSON-marshalable values.
// - Steps: run `assertExplicit` on: `{a: null}` (no unavailable); `{a: 1, unavailable: {a: "x"}}`; `{a: null, unavailable: {a: ""}}`; `{l: [], unavailable: {l: "r"}}`; nested `{o: {b: null}}`; `{series: [{values: [null]}]}`; `{a: null, unavailable: {a: "permission denied: /x"}}`.
// - Expected: the first four violations and the nested one are reported (except `l: []` with a reason, which is valid); the `series.values` null and the last, well-formed object pass.
func TestTCHPERR28_AssertExplicitCatchesEachKindOfViolation(t *testing.T) {
	t.Skip("TC-HP-ERR28: SKIP-only stub; un-skip when WI 4.1 lands")
}
