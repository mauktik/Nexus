// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-28, TC-HP-29, TC-HP-30, TC-HP-31, TC-HP-32, TC-HP-33, TC-HP-34, TC-HP-35, TC-HP-36, TC-HP-37, TC-HP-38, TC-HP-39, TC-HP-E8, TC-HP-E9.
// Owner WIs: 2.3. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package hostparse

import "testing"

// TC-HP-28: ProcStat parses CPU lines and btime from both captures. Owner WI 2.3. Requirements: FR-6, FR-11, NFR-4.
//
// - Setup: both captures' `fs/proc/stat`, `tools/nproc`.
// - Steps: `ProcStat(b)`.
// - Expected: the number of per-CPU entries equals `tools/nproc`; the total line's user/nice/system/idle/iowait/irq/softirq/steal equal the file's `cpu ` fields 1–8; guest and guest_nice are not added; `btime` equals the file's `btime` value.
func TestTCHP28_ProcStatParsesCPULinesAndBtimeFrom(t *testing.T) {
	t.Skip("TC-HP-28: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-29: Meminfo values are bytes and agree with free. Owner WI 2.3. Requirements: FR-6, NFR-3.
//
// - Setup: both captures' `fs/proc/meminfo`, `tools/free` (`free -b`).
// - Steps: `Meminfo(b)`.
// - Expected: each needed key equals its kB value × 1024; `MemTotal` equals `free -b` total exactly; `MemAvailable` within ±1% of `free -b` available; `SwapTotal` equals free's swap total.
func TestTCHP29_MeminfoValuesAreBytesAndAgreeWith(t *testing.T) {
	t.Skip("TC-HP-29: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-30: Loadavg and Uptime parse the captured lines. Owner WI 2.3. Requirements: FR-4, FR-6.
//
// - Setup: both captures.
// - Steps: parse `proc/loadavg`, `proc/uptime`.
// - Expected: load1/5/15 equal the first three fields as float64; uptime seconds equal the first field; `tools/vmstat`'s run time is irrelevant here (no cross-check).
func TestTCHP30_LoadavgAndUptimeParseTheCapturedLines(t *testing.T) {
	t.Skip("TC-HP-30: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-31: Vmstat distinguishes oom_kill absent from zero. Owner WI 2.3. Requirements: FR-10.
//
// - Setup: both captures' `proc/vmstat`; the same with the `oom_kill` line removed; one with `oom_kill 0`.
// - Steps: `Vmstat(b)`.
// - Expected: capture value and present=true; removed → present=false; `0` → value 0, present=true.
func TestTCHP31_VmstatDistinguishesOomKillAbsentFromZero(t *testing.T) {
	t.Skip("TC-HP-31: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-32: Pressure parses some and full, with full absent on older kernels. Owner WI 2.3. Requirements: FR-6.
//
// - Setup: captured `proc/pressure/{cpu,memory,io}` where present; a copy of `cpu` without its `full` line.
// - Steps: `Pressure(b)`.
// - Expected: avg10/avg60/avg300 equal the file's values; the copy without `full` returns `Full == nil`, `Some` set.
func TestTCHP32_PressureParsesSomeAndFullWithFull(t *testing.T) {
	t.Skip("TC-HP-32: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-33: Cpuinfo gives the model, per-processor MHz and the hypervisor flag. Owner WI 2.3. Requirements: FR-5, FR-6.
//
// - Setup: both captures' `proc/cpuinfo`.
// - Steps: `Cpuinfo(b)`.
// - Expected: model equals the first `model name` value; MHz count equals the number of `processor` stanzas (or zero when the arch has no `cpu MHz`); hypervisor flag equals whether the first `flags` line contains the word `hypervisor`.
func TestTCHP33_CpuinfoGivesTheModelPerProcessorMHz(t *testing.T) {
	t.Skip("TC-HP-33: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-34: Diskstats parses 14, 18 and 20 field lines. Owner WI 2.3. Requirements: FR-8, FR-11.
//
// - Setup: both captures' `proc/diskstats`; synthetic lines derived from a capture line truncated to 14 and extended to 20 fields.
// - Steps: `Diskstats(b)`.
// - Expected: one entry per line with name, major, minor, reads completed (field 4), sectors read (6), writes completed (8), sectors written (10); all three field counts parse identically for the shared fields.
func TestTCHP34_DiskstatsParses1418And20Field(t *testing.T) {
	t.Skip("TC-HP-34: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-35: NetDev parses counters with either colon spacing. Owner WI 2.3. Requirements: FR-9, FR-11.
//
// - Setup: both captures' `proc/net/dev`; a synthetic line `eth0:1234 5 …` (no space after the colon, as kernels print for large counters).
// - Steps: `NetDev(b)`.
// - Expected: the two header lines are skipped; one entry per interface; rx bytes/packets/errs/drop and tx bytes/packets/errs/drop equal the file's columns; the no-space line parses the same as the spaced one; interface names equal `tools/ip-addr` names.
func TestTCHP35_NetDevParsesCountersWithEitherColonSpacing(t *testing.T) {
	t.Skip("TC-HP-35: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-36: Default routes from route and ipv6_route equal ip route. Owner WI 2.3. Requirements: FR-9.
//
// - Setup: both captures' `proc/net/route`, `proc/net/ipv6_route`, `tools/ip-route`.
// - Steps: `RouteV4(b)`, `RouteV6(b)`.
// - Expected: the IPv4 default gateway(s) (destination 0, mask 0, `RTF_GATEWAY` set, little-endian hex decoded) equal `ip route`'s `default via X dev Y metric M`; IPv6 likewise against `ip -6 route` default (when the capture has one); non-default and non-gateway lines are excluded.
func TestTCHP36_DefaultRoutesFromRouteAndIpv6Route(t *testing.T) {
	t.Skip("TC-HP-36: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-37: Mountinfo decodes escapes, optional fields and options. Owner WI 2.3. Requirements: FR-7.
//
// - Setup: both captures' `proc/1/mountinfo` (or `self`); synthetic lines with `\040`, `\011`, `\012`, `\134` in the mount point and with zero, one and two optional fields (`shared:1 master:2`) before ` - `.
// - Steps: `Mountinfo(b)`.
// - Expected: mount id, `maj:min`, root, decoded mount point, per-mount options, fstype, source and super options per line; every `tools/df` target is among the parsed mount points.
func TestTCHP37_MountinfoDecodesEscapesOptionalFieldsAndOptions(t *testing.T) {
	t.Skip("TC-HP-37: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-38: PidStat and PidStatusUID handle hostile comm values. Owner WI 2.3. Requirements: FR-10, FR-11, NFR-6.
//
// - Setup: every `fs/proc/[0-9]*/stat` and `status` in both captures; synthetic stat lines derived from a capture line with comm `a) (b`, `) (`, `x y z`, and an empty comm `()`.
// - Steps: `PidStat(b)`, `PidStatusUID(b)`.
// - Expected: comm is the text between the first `(` and the last `)`; utime, stime, starttime and rss equal fields 14, 15, 22, 24 counted after the last `)`; UID is the first number of the `Uid:` line; every captured file parses.
func TestTCHP38_PidStatAndPidStatusUIDHandleHostileCommValues(t *testing.T) {
	t.Skip("TC-HP-38: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-39: Every parser has a fuzz target seeded from the captures. Owner WI 2.3. Requirements: NFR-4.
//
// - Setup: none.
// - Steps: list the `Fuzz*` functions of `hostparse` (go/parser over `fuzz_test.go`); `go test ./internal/providers/host/hostparse` runs their seeds.
// - Expected: one fuzz target per parser of WI 2.3 and 2.4 (names listed in the TC's table in the stub); each adds every matching capture file from both captures as a seed; no seed panics.
func TestTCHP39_EveryParserHasAFuzzTargetSeeded(t *testing.T) {
	t.Skip("TC-HP-39: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-E8: Parsers skip malformed lines and never panic. Owner WI 2.3. Requirements: NFR-4.
//
// - Setup: per parser, each capture file mutated: a new trailing column on every line, CRLF line endings, a truncated last line, one non-numeric field, an empty file; and the required anchor removed (`cpu ` from stat, `MemTotal` from meminfo).
// - Steps: parse each.
// - Expected: no panic; mutations yield the same values for unaffected lines and skip only the broken ones; an empty file or a missing anchor returns an error (not zero values).
func TestTCHPE8_ParsersSkipMalformedLinesAndNeverPanic(t *testing.T) {
	t.Skip("TC-HP-E8: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-HP-E9: CPU and per-process deltas from the captures are consistent. Owner WI 2.3. Requirements: NFR-3, FR-10.
//
// - Setup: both captures' `fs/` and `t1/`.
// - Steps: parse both samples; compute Δ per pid present in both with equal starttime and the total Δ over all CPUs.
// - Expected: every per-pid Δ ≥ 0; Σ per-pid Δ ≤ total CPU Δ (all CPUs); total CPU % within ±5 pp of `tools/vmstat` (this validates the capture itself before the sampler relies on it).
func TestTCHPE9_CPUAndPerProcessDeltasFromThe(t *testing.T) {
	t.Skip("TC-HP-E9: SKIP-only stub; un-skip when WI 2.3 lands")
}
