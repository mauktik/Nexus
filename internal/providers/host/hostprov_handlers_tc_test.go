// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-71, TC-HP-72, TC-HP-73, TC-HP-74, TC-HP-75, TC-HP-76, TC-HP-77, TC-HP-78, TC-HP-79, TC-HP-80, TC-HP-81, TC-HP-82, TC-HP-83, TC-HP-84, TC-HP-85, TC-HP-86, TC-HP-87, TC-HP-88, TC-HP-89, TC-HP-90, TC-HP-91, TC-HP-92, TC-HP-93, TC-HP-94, TC-HP-95, TC-HP-96, TC-HP-97, TC-HP-98, TC-HP-99, TC-HP-100, TC-HP-101, TC-HP-102, TC-HP-103, TC-HP-E18, TC-HP-E19, TC-HP-E20, TC-HP-E21, TC-HP-E22, TC-HP-E23, TC-HP-E24, TC-HP-E25, TC-HP-E26, TC-HP-ERR18, TC-HP-ERR19, TC-HP-ERR20, TC-HP-ERR21, TC-HP-ERR22, TC-HP-ERR23, TC-HP-ERR24, TC-HP-ERR25, TC-HP-ERR26, TC-HP-ERR27, TC-HP-ERR28.
// Owner WIs: 4.1, 4.2, 4.3, 4.4, 4.5. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

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

// TC-HP-79: Filesystems on both captures agree with statfs and df. Owner WI 4.2. Requirements: FR-7, NFR-3, FR-15.
//
// - Setup: host stack per capture, F-STATFS from `tools/statfs.txt`.
// - Steps: filesystems handler with `limit 1000`.
// - Expected:
//   - every item's mount point is a `tools/df` target, and `/` is present;
//   - `sizeBytes` = `%b × %S`, `usedBytes` = `(%b − %f) × %S`, `availableBytes` = `%a × %S` from `statfs.txt` exactly; `sizeBytes` equals df's size exactly; `usedBytes` within one block of df's used; `usedPercent` = used/(used+available) × 100 and within 1 of df's `Use%`;
//   - inodes `%c`, `%d`, `%c − %d`; `assertExplicit` passes.
func TestTCHP79_FilesystemsOnBothCapturesAgreeWithStatfs(t *testing.T) {
	t.Skip("TC-HP-79: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-80: The exclusion list and bind-mount dedupe. Owner WI 4.2. Requirements: FR-7.
//
// - Setup: UBU F-FS with `replace("proc/1/mountinfo", …)` = the capture's lines plus, for each fstype of A-21, one synthetic line derived from a real line of that type (or from the root line with the fstype changed), plus: overlays under each container-runtime dir, `squashfs` at `/snap/core/1`, tmpfs at `/tmp`, `/run`, `/dev/shm`, `/var/tmp`, a file bind at `/etc/hosts` (F-FS: not a directory), and a bind of the root device at `/mnt/bind` (same `maj:min`, root `/home`).
// - Steps: filesystems handler.
// - Expected: none of the excluded types or paths listed; tmpfs at `/tmp`, `/run`, `/dev/shm` listed, `/var/tmp` tmpfs not; `/etc/hosts` not; the root device listed once at `/` (root `/` preferred over `/mnt/bind`); an overlay at `/` itself (container root) is listed.
func TestTCHP80_TheExclusionListAndBindMountDedupe(t *testing.T) {
	t.Skip("TC-HP-80: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-81: A filesystem without inode counts reports them as not reported. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup: F-STATFS answers `Files = 0` for one captured mount (as btrfs/vfat do).
// - Steps: filesystems handler.
// - Expected: that item's `inodesTotal`, `inodesUsed`, `inodesFree` null, each in `unavailable` with `not reported: the filesystem has no inode counts`; sizes populated.
func TestTCHP81_AFilesystemWithoutInodeCountsReportsThem(t *testing.T) {
	t.Skip("TC-HP-81: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-82: The read-only flag comes from PID 1's mountinfo. Owner WI 4.2. Requirements: FR-7.
//
// - Setup: UBU F-FS where `proc/self/mountinfo` marks every mount `ro` (as under `ProtectSystem=strict`) and `proc/1/mountinfo` is the capture's, plus one line `ro` only in super options.
// - Steps: filesystems handler.
// - Expected: F-FS recorded `proc/1/mountinfo` and not `proc/self/mountinfo`; `readOnly` false for `/`, true for the super-options `ro` mount.
func TestTCHP82_TheReadOnlyFlagComesFromPID(t *testing.T) {
	t.Skip("TC-HP-82: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-83: Filesystems are ordered by mount point and named by its escaped path. Owner WI 4.2. Requirements: FR-7, FR-14.
//
// - Setup: host stack (UBU).
// - Steps: filesystems handler.
// - Expected: items sorted by `mountPoint` byte order; each `name` = `url.PathEscape(mountPoint)` (`/` → `%2F`).
func TestTCHP83_FilesystemsAreOrderedByMountPointAnd(t *testing.T) {
	t.Skip("TC-HP-83: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-84: Paging round trip and limits. Owner WI 4.2. Requirements: FR-14.
//
// - Setup: host stack (UBU) with ≥ 3 filesystems.
// - Steps: page with `limit 2` following `nextCursor` until absent; then no `limit`.
// - Expected: concatenated pages equal the unpaged list exactly once each, in order; the last page has no `nextCursor`; no-limit returns up to 100; cursor format matches nginx's (unpadded base64url of the last name).
func TestTCHP84_PagingRoundTripAndLimits(t *testing.T) {
	t.Skip("TC-HP-84: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-85: Block devices on both captures agree with lsblk. Owner WI 4.2. Requirements: FR-8, NFR-3, FR-15.
//
// - Setup: host stack per capture, ready snapshot; `tools/lsblk` (UBU), and for ALP the capture's own `sys/block` (no lsblk).
// - Steps: disks handler.
// - Expected:
//   - one item per `sys/block` entry except `ram*`, `zram*`, and `loop*` without `loop/backing_file`; a loop with a backing file is listed;
//   - UBU: names equal lsblk `type: disk` (plus listed loops); `sizeBytes`, `rotational`, `removable` equal lsblk; `model` = trimmed `device/model`;
//   - each partition from `sys/block/<d>/<p>/partition`, with `sizeBytes` = size × 512 and `mountPoints` equal lsblk's `mountpoints` for it;
//   - rates equal TC-HP-62's for the same name and `maj:min`; `assertExplicit` passes.
func TestTCHP85_BlockDevicesOnBothCapturesAgreeWith(t *testing.T) {
	t.Skip("TC-HP-85: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-86: Disk rates report the sampler state. Owner WI 4.2. Requirements: FR-8, FR-11, FR-12.
//
// - Setup: host stack (UBU) warming up.
// - Steps: disks handler.
// - Expected: `readBytesPerSecond`, `writeBytesPerSecond`, `readIops`, `writeIops` null with `warming up`; model, size, flags populated; each item's `sampler.state` `warming-up`.
func TestTCHP86_DiskRatesReportTheSamplerState(t *testing.T) {
	t.Skip("TC-HP-86: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-87: Interfaces on both captures agree with ip addr. Owner WI 4.3. Requirements: FR-9, NFR-3, NFR-6, FR-15.
//
// - Setup: host stack per capture, ready snapshot.
// - Steps: interfaces handler.
// - Expected:
//   - names equal `tools/ip-addr`'s; `lo` has `loopback true` (type 772), others false;
//   - `state` = `Operstate(operstate)`; `mac` = `address`; `mtu` = `mtu`; `ipv4`/`ipv6` with prefix lengths equal ip addr's;
//   - `rxBytes`, `txBytes`, errors and drops equal `fs/proc/net/dev` (read live);
//   - rates equal TC-HP-63's; `assertExplicit` passes.
func TestTCHP87_InterfacesOnBothCapturesAgreeWithIp(t *testing.T) {
	t.Skip("TC-HP-87: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-88: Default gateways equal ip route. Owner WI 4.3. Requirements: FR-9.
//
// - Setup: host stack per capture.
// - Steps: netconfig handler.
// - Expected: one item `current`; `defaultGateways` = the `tools/ip-route` defaults with `family` (`ipv4`/`ipv6`), `address`, `interface`, `metric`, sorted by metric.
func TestTCHP88_DefaultGatewaysEqualIpRoute(t *testing.T) {
	t.Skip("TC-HP-88: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-89: DNS resolvers, with and without systemd-resolved. Owner WI 4.3. Requirements: FR-9.
//
// - Setup: UBU (resolved in use); ALP (plain).
// - Steps: netconfig handler.
// - Expected: UBU: `dns.source` = `/run/systemd/resolve/resolv.conf`; `servers` = that file's nameservers with `stub false`, plus `127.0.0.53` with `stub true`. ALP: `source` `/etc/resolv.conf`, servers in file order, `stub` true only for loopback addresses.
func TestTCHP89_DNSResolversWithAndWithoutSystemdResolved(t *testing.T) {
	t.Skip("TC-HP-89: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-90: Reboot required on the Debian family follows the apt rule. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: UBU F-FS variants: (a) `var/run/reboot-required` and `.pkgs` with `linux-image-6.8.0-50-generic\nlinux-base\n`; (b) no marker, one `apt.RebootMarkerWriters` path present; (c) no marker, no writer; (d) as (a) plus the container marker `run/systemd/container`.
// - Steps: health handler.
// - Expected: (a) and (d) `state "true"`, `packages` = the two names, `check debian-reboot-required-marker`, `evidence` names `/var/run/reboot-required`; (b) `"false"`; (c) `"unknown"` with evidence that no marker writer is installed. (d) encodes open item 1 of test-strategy.md.
func TestTCHP90_RebootRequiredOnTheDebianFamilyFollows(t *testing.T) {
	t.Skip("TC-HP-90: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-91: Reboot required on Alpine compares the running kernel with /lib/modules. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: ALP F-FS, no container markers (VM capture); variants with `lib/modules/<osrelease>` present and removed.
// - Steps: health handler.
// - Expected: present → `"false"`, missing → `"true"`; `check alpine-running-kernel-modules`; evidence names `/lib/modules/<release>` with the capture's release.
func TestTCHP91_RebootRequiredOnAlpineComparesTheRunning(t *testing.T) {
	t.Skip("TC-HP-91: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-92: Reboot required is unknown in a non-Debian container. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: ALP F-FS plus `.dockerenv`, `lib/modules/<release>` missing; a platform with an unknown OS family and no container.
// - Steps: health handler.
// - Expected: container: `"unknown"`, evidence says the kernel belongs to the container host; unknown family: `"unknown"` with `not supported: …`.
func TestTCHP92_RebootRequiredIsUnknownInANon(t *testing.T) {
	t.Skip("TC-HP-92: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-93: Reboot constants mirror the apt provider. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: import `internal/providers/apt`.
// - Steps: compare.
// - Expected: the host constants equal `strings.TrimPrefix(apt.RebootMarker, "/")`, `…RebootPackagesFile`, and each of `apt.RebootMarkerWriters` with the leading `/` removed, in order; every one is allow-listed.
func TestTCHP93_RebootConstantsMirrorTheAptProvider(t *testing.T) {
	t.Skip("TC-HP-93: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-94: Failed services from the captured outputs, cut at 20 names. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: UBU with F-RUN answering `list-units-failed.txt`; ALP with `rc-status-crashed.txt`; UBU with a capture-derived output of 21 unit lines.
// - Steps: health handler.
// - Expected: `failedServices.count` and `names` equal `svcctl.Failed`'s on the same output, `source` names the tool; with 21: `count 21`, 20 names (the first 20 sorted), `namesTruncated true`.
func TestTCHP94_FailedServicesFromTheCapturedOutputsCut(t *testing.T) {
	t.Skip("TC-HP-94: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-95: OOM kills come from vmstat only. Owner WI 4.4. Requirements: FR-10, NFR-6.
//
// - Setup: UBU; variant with the `oom_kill` line removed.
// - Steps: health handler; read F-FS's recorder.
// - Expected: `oomKills` = the captured `oom_kill`; removed → null with `not present: /proc/vmstat oom_kill`; no recorded path contains `kmsg` or `log`.
func TestTCHP95_OOMKillsComeFromVmstatOnly(t *testing.T) {
	t.Skip("TC-HP-95: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-96: Top processes carry owner names and only comm. Owner WI 4.4. Requirements: FR-10, NFR-6, FR-15.
//
// - Setup: ready snapshot per capture; variant where one top pid's uid has no passwd entry; variant where one top pid's `status` is removed after the scan.
// - Steps: health handler.
// - Expected:
//   - `topByCpu`/`topByMemory` equal TC-HP-64's; `uid` from `t1/proc/<pid>/status`; `user` from `etc/passwd`;
//   - no passwd entry → `user` null with `not reported: uid N has no /etc/passwd entry`; vanished → the sampled values kept, `uid` and `user` null with `not present: /proc/<pid>/status`;
//   - `processSampleSeconds` = 10; `assertExplicit` passes.
func TestTCHP96_TopProcessesCarryOwnerNamesAndOnly(t *testing.T) {
	t.Skip("TC-HP-96: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-97: Health output never carries command lines or environment. Owner WI 4.4. Requirements: NFR-6, FR-10.
//
// - Setup: ready snapshot (UBU).
// - Steps: marshal the health output; walk every key.
// - Expected: process objects have exactly `pid`, `command`, `uid`, `user`, `cpuPercent`, `rssBytes` (plus `unavailable` when set); no key contains `cmdline`, `args`, `argv`, `env`; each `command` is ≤ 15 bytes and equals that pid's captured comm.
func TestTCHP97_HealthOutputNeverCarriesCommandLinesOr(t *testing.T) {
	t.Skip("TC-HP-97: SKIP-only stub; un-skip when WI 4.4 lands")
}

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
// - Setup: the synthetic large host of test-strategy.md (32 mounts with 64-byte mount points, 32 interfaces with 15-byte names, IPv4 + IPv6 each), warming up (all sampler fields null with reasons, the largest shape).
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
// - Expected: every recorded path matches the allow-list (`allowed(path)` true, checked after the fact); no recorded path matches `(?i)(cmdline|environ|serial|uuid|asset_tag|kmsg|shadow|/arp$|neigh|/mem$|kcore|/fd/|/maps$)`; recorded paths include `proc/1/mountinfo`, `sys/class/dmi/id/sys_vendor` and at least one `proc/<pid>/stat` (non-vacuous).
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

// TC-HP-E20: Block devices are enumerated through symlink entries. Owner WI 4.2. Requirements: FR-8, CON-5.
//
// - Setup: F-FS(UBU) with `sys/block/*` as `ModeSymlink` entries (the default shaping).
// - Steps: disks handler.
// - Expected: the same disk set as lsblk (TC-HP-85); zero disks is a failure.
func TestTCHPE20_BlockDevicesAreEnumeratedThroughSymlinkEntries(t *testing.T) {
	t.Skip("TC-HP-E20: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-E21: A disk without a model reports it as not reported. Owner WI 4.2. Requirements: FR-8, FR-12.
//
// - Setup: UBU F-FS with `remove("sys/block/<first disk>/device/model")` (as virtio disks have none).
// - Steps: disks handler.
// - Expected: that disk's `model` null with a `not reported:` reason; other fields populated.
func TestTCHPE21_ADiskWithoutAModelReportsIt(t *testing.T) {
	t.Skip("TC-HP-E21: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-E22: A mount point with a space round-trips. Owner WI 4.2. Requirements: FR-7, FR-15.
//
// - Setup: an extra mountinfo line (derived from the root line) with mount point `/mnt/my\040disk`; F-STATFS answering for `/mnt/my disk`.
// - Steps: filesystems handler.
// - Expected: `mountPoint "/mnt/my disk"`, `name "%2Fmnt%2Fmy%20disk"`; F-STATFS was called with the decoded path.
func TestTCHPE22_AMountPointWithASpaceRound(t *testing.T) {
	t.Skip("TC-HP-E22: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-E23: Interfaces are enumerated through symlink entries. Owner WI 4.3. Requirements: FR-9, CON-5.
//
// - Setup: F-FS(UBU) default shaping.
// - Steps: interfaces handler.
// - Expected: the same names as `tools/ip-addr`; zero interfaces is a failure.
func TestTCHPE23_InterfacesAreEnumeratedThroughSymlinkEntries(t *testing.T) {
	t.Skip("TC-HP-E23: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-E24: A host with no default route. Owner WI 4.3. Requirements: FR-9, FR-12.
//
// - Setup: UBU with `proc/net/route` and `ipv6_route` reduced to their non-default lines.
// - Steps: netconfig handler.
// - Expected: `defaultGateways []`; `assertExplicit` passes (an empty list is a value; if listed in `unavailable`, its reason is `not reported:`).
func TestTCHPE24_AHostWithNoDefaultRoute(t *testing.T) {
	t.Skip("TC-HP-E24: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-E25: Neighbour tables are never read. Owner WI 4.3. Requirements: NFR-6.
//
// - Setup: UBU with F-FS recording; F-ADDRS counting calls.
// - Steps: interfaces and netconfig handlers.
// - Expected: no recorded path under `proc/net/` other than `dev`, `route`, `ipv6_route`; the only netlink use is F-ADDRS (called once per request); `mac` fields are each interface's own `address` only.
func TestTCHPE25_NeighbourTablesAreNeverRead(t *testing.T) {
	t.Skip("TC-HP-E25: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-E26: Lists over the summary bound are cut with an indicator, never silently. Owner WI 4.5. Requirements: FR-4.
//
// - Setup: the synthetic large host with 40 extra mounts and 40 extra interfaces.
// - Steps: summary handler.
// - Expected: 32 filesystems, `filesystemsTotal` = 40 + the capture's, `filesystemsTruncated true`; 32 interfaces, `interfacesTotal` likewise, `interfacesTruncated true`; the kept filesystems are the first 32 by mount point.
func TestTCHPE26_ListsOverTheSummaryBoundAreCut(t *testing.T) {
	t.Skip("TC-HP-E26: SKIP-only stub; un-skip when WI 4.5 lands")
}

// TC-HP-ERR18: Clock command failures are reported with a reason. Owner WI 4.1. Requirements: FR-5, FR-12.
//
// - Setup: UBU with F-RUN for timedatectl: (a) exit 1 with `Failed to connect to bus`; (b) an `ErrNotStarted` (ENOENT) error; (c) `hang` (timeout 250 ms); (d) exit 0 with `maybe`.
// - Steps: identity handler.
// - Expected: `clock.synchronized` unavailable with (a) `command failed: /usr/bin/timedatectl exit 1`, (b) `not present: /usr/bin/timedatectl` (open item 2), (c) `timed out after 250ms: /usr/bin/timedatectl`, (d) `unrecognized format: …`; the handler returns within 350 ms in (c); the rest of identity is populated; the output never contains the command's text (`Failed to connect`).
func TestTCHPERR18_ClockCommandFailuresAreReportedWithA(t *testing.T) {
	t.Skip("TC-HP-ERR18: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-ERR19: A blocked sensor read times out without holding the CPU op. Owner WI 4.1. Requirements: FR-6, FR-12, NFR-1.
//
// - Setup: UBU (or a capture-derived hwmon) with F-FS `block` on one `temp*_input`; timeout 250 ms.
// - Steps: CPU handler; measure.
// - Expected: returns within 350 ms; the blocked sensor is absent from `temperatures` (or null `celsius`) with `timed out after 250ms: /sys/class/hwmon/…`; other sensors present.
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

// TC-HP-ERR21: Without PID 1's mountinfo, read-only flags are unavailable, never wrong. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup: UBU with `deny("proc/1/mountinfo")` and `proc/self/mountinfo` marking every mount `ro`.
// - Steps: filesystems handler.
// - Expected: items come from `self/mountinfo`; every `readOnly` null with `permission denied: /proc/1/mountinfo (read-only flags in nexusd's own namespace would be wrong)`; no item has `readOnly true`; sizes populated.
func TestTCHPERR21_WithoutPID1SMountinfoReadOnly(t *testing.T) {
	t.Skip("TC-HP-ERR21: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-ERR22: One hung mount does not hold up the list. Owner WI 4.2. Requirements: FR-7, NFR-1.
//
// - Setup: UBU with SP-HUNG-NFS; timeout 250 ms.
// - Steps: filesystems handler; measure; call again at once.
// - Expected: first returns within 450 ms (`sourceTimeout` + 200 ms); `/mnt/nfs` listed with size, used, available, usedPercent and inodes null with `timed out after 250ms: statfs /mnt/nfs`; all other mounts populated; the second call answers within 50 ms with `still pending: statfs /mnt/nfs`.
func TestTCHPERR22_OneHungMountDoesNotHoldUp(t *testing.T) {
	t.Skip("TC-HP-ERR22: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-ERR23: A down or virtual link has no speed. Owner WI 4.3. Requirements: FR-9, FR-12.
//
// - Setup: UBU with `einval("sys/class/net/<a down interface>/speed")`; a virtual interface without `device` whose speed reads `-1`.
// - Steps: interfaces handler.
// - Expected: both `speedMbps` null with a `not reported:` reason; the call does not fail; other fields populated.
func TestTCHPERR23_ADownOrVirtualLinkHasNo(t *testing.T) {
	t.Skip("TC-HP-ERR23: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-ERR24: Unreadable route tables leave gateways explicitly unavailable. Owner WI 4.3. Requirements: FR-9, FR-12.
//
// - Setup: UBU with `deny("proc/net/route")` and `deny("proc/net/ipv6_route")`; separately `deny("etc/resolv.conf")`.
// - Steps: netconfig handler.
// - Expected: `defaultGateways []` listed in `unavailable` with `permission denied: /proc/net/route`; with resolv.conf denied: `dns.servers []` listed with `permission denied: /etc/resolv.conf`; no error.
func TestTCHPERR24_UnreadableRouteTablesLeaveGatewaysExplicitlyUnavailable(t *testing.T) {
	t.Skip("TC-HP-ERR24: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-ERR25: Failed-service query failures leave the count unavailable. Owner WI 4.4. Requirements: FR-10, FR-12.
//
// - Setup: UBU with F-RUN for the failed-units argv: `hang` (timeout 250 ms); `ErrNotStarted`; exit 1 with bus error text.
// - Steps: health handler.
// - Expected: `failedServices.count` null and `names []`, both in `unavailable`, with `timed out after 250ms: /usr/bin/systemctl`, `not present: /usr/bin/systemctl`, `command failed: /usr/bin/systemctl exit 1`; the rest of health populated; no command output text in the response.
func TestTCHPERR25_FailedServiceQueryFailuresLeaveTheCount(t *testing.T) {
	t.Skip("TC-HP-ERR25: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-ERR26: One hung mount keeps the summary under its bound. Owner WI 4.5. Requirements: FR-4, NFR-1.
//
// - Setup: UBU with SP-HUNG-NFS; timeout 250 ms; plus F-RUN `hang` on the failed-units argv.
// - Steps: summary handler; measure.
// - Expected: returns within 550 ms (`sourceTimeout` + 300 ms) even with two hung sources (collectors run concurrently); `/mnt/nfs` usage and `health.failedServicesCount` null with their `timed out` reasons.
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

// TC-HP-ERR28: assertExplicit catches each kind of violation. Owner WI 4.1. Requirements: FR-12.
//
// - Setup: a fake `testing.TB` that records failures; planted JSON-marshalable values.
// - Steps: run `assertExplicit` on: `{a: null}` (no unavailable); `{a: 1, unavailable: {a: "x"}}`; `{a: null, unavailable: {a: ""}}`; `{l: [], unavailable: {l: "r"}}`; nested `{o: {b: null}}`; `{series: [{values: [null]}]}`; `{a: null, unavailable: {a: "permission denied: /x"}}`.
// - Expected: the first four violations and the nested one are reported (except `l: []` with a reason, which is valid); the `series.values` null and the last, well-formed object pass.
func TestTCHPERR28_AssertExplicitCatchesEachKindOfViolation(t *testing.T) {
	t.Skip("TC-HP-ERR28: SKIP-only stub; un-skip when WI 4.1 lands")
}
