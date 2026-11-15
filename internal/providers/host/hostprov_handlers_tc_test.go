// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-71, TC-HP-72, TC-HP-73, TC-HP-74, TC-HP-75, TC-HP-76, TC-HP-77, TC-HP-78, TC-HP-79, TC-HP-80, TC-HP-81, TC-HP-82, TC-HP-83, TC-HP-84, TC-HP-85, TC-HP-86, TC-HP-87, TC-HP-88, TC-HP-89, TC-HP-90, TC-HP-91, TC-HP-92, TC-HP-93, TC-HP-94, TC-HP-95, TC-HP-96, TC-HP-97, TC-HP-98, TC-HP-99, TC-HP-100, TC-HP-101, TC-HP-102, TC-HP-103, TC-HP-E18, TC-HP-E19, TC-HP-E20, TC-HP-E21, TC-HP-E22, TC-HP-E23, TC-HP-E24, TC-HP-E25, TC-HP-E26, TC-HP-ERR18, TC-HP-ERR19, TC-HP-ERR20, TC-HP-ERR21, TC-HP-ERR22, TC-HP-ERR23, TC-HP-ERR24, TC-HP-ERR25, TC-HP-ERR26, TC-HP-ERR27, TC-HP-ERR28, TC-HP-E31, TC-HP-134, TC-HP-E33.
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
// - Setup: host stack per capture, F-STATFS from `tools/statfs.txt`. F-FS serves the capture's own `proc/1/mountinfo` and `proc/self/mountinfo`, with no `addMount`. When `PROVENANCE` says `under-unit: no`, both files list the same mounts, and every `tools/df` target is expected with numbers. When it says `yes`, targets at or under `/tmp` and `/var/tmp` are left out of this comparison; TC-HP-E31 and TC-HP-117 cover them.
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
// - Setup:
//   - UBU F-FS. Every mount below is added with `addMount(…, both)` and has its own fresh `maj:min` unless stated otherwise. F-FS has no entry for any mount point (Finding 2).
//   - For each fstype of A-21, one line derived from a real line of that type in the capture, or from the root line with the fstype changed and a fresh `maj:min`. Also overlays under each container-runtime directory, `squashfs` at `/snap/core/1`, and tmpfs at `/tmp`, `/run`, `/dev/shm` and `/var/tmp` (the capture's own lines where it has them).
//   - `/mnt/bind`: a bind of the root filesystem, `sameDevAs("/")`, with root `/home` (the dedupe case).
//   - `/etc/hosts`: a file bind from another disk, as Docker makes it, with its own `maj:min` `8:33` (the host disk's, not the root's) and root `/var/lib/docker/containers/c1/hosts`. F-STATFS answers it with `IsDir=false`.
//
// - Steps: filesystems handler with `limit 1000`, in four subtests:
//  1. As set up.
//  2. F-STATFS answers `/etc/hosts` with `IsDir=true` and a row of numbers.
//  3. Add `/srv/appdata`, a directory mount with `sameDevAs("/etc/hosts")` (`8:33`) and root `/appdata`. Its mount point is longer than `/etc/hosts`, so WI 4.2's preference order (mountinfo root `/`, then the shortest mount point) tries the file bind first. (`/srv/data`, the architecture's example, is shorter than `/etc/hosts`, so it would be tried first and the skip would never run.)
//  4. The root line's fstype is `overlay` in both files (a container root).
//
// - Expected:
//   - Subtest 1:
//   - none of the excluded types or paths is listed;
//   - tmpfs at `/tmp`, `/run` and `/dev/shm` is listed, and the `/var/tmp` tmpfs is not;
//   - `/etc/hosts` is not listed. Its group is a singleton, and F-STATFS said `IsDir=false`, so the group is dropped. F-STATFS's count for `/etc/hosts` is 1;
//   - the root device is listed once, at `/` (root `/` is preferred over `/mnt/bind`), and F-STATFS's count for `/mnt/bind` is 0.
//   - Subtest 2: `/etc/hosts` is listed with the row's numbers. So the drop in subtest 1 came from `IsDir`, not from the path.
//   - Subtest 3: `/srv/appdata` is listed with its F-STATFS numbers and `/etc/hosts` is not. F-STATFS was called for `/etc/hosts` and then for `/srv/appdata`, once each. A file bind never displaces a directory bind of the same filesystem.
//   - Subtest 4: the overlay at `/` is listed.
//   - Every subtest: F-FS recorded no access to `etc/hosts`, `mnt/bind`, `srv/appdata` or any other mount point.
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
// - Setup: UBU F-FS. `proc/1/mountinfo` is the capture's plus one mount, `/srv/rosuper` (fresh `maj:min`, added with `addMount(…, both)`), whose per-mount options are `rw` and whose super options are `ro`. Then every entry of `proc/self/mountinfo` gets per-mount options `ro`, as under `ProtectSystem=strict`, with its mount ID, parent ID and `maj:min` unchanged.
// - Steps: filesystems handler; read F-FS's recorder.
// - Expected:
//   - F-FS recorded `Open`s of both files: `proc/1/mountinfo` (the mount list and the `readOnly` source) and `proc/self/mountinfo` (visibility only, D-9), the latter exactly once per handler call (WI 4.2 step 1).
//   - `readOnly` is false for `/` and true for `/srv/rosuper`. So `readOnly` comes from PID 1's view, and the self file's `ro` options change nothing.
//   - Every item has numbers: the self file decides only visibility, and every mount is visible in it.
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
// - Setup: an extra ext4 mount whose mountinfo mount point is `/mnt/my\040disk`, with a fresh `maj:min` (`8:65`), added with `addMount(…, both)`. F-STATFS answers for `/mnt/my disk` with a row distinct from the root's.
// - Steps: filesystems handler.
// - Expected: the item has `mountPoint "/mnt/my disk"`, `name "%2Fmnt%2Fmy%20disk"` and the row's numbers; F-STATFS was called once, with the decoded path `/mnt/my disk`.
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
// - Setup: the synthetic large host with 40 extra mounts (each added with `addMount(…, both)` and its own fresh `maj:min`) and 40 extra interfaces.
// - Steps: summary handler.
// - Expected: 32 filesystems, `filesystemsTotal` = 40 + the capture's, `filesystemsTruncated true`; 32 interfaces, `interfacesTotal` likewise, `interfacesTruncated true`; the kept filesystems are the first 32 by mount point.
func TestTCHPE26_ListsOverTheSummaryBoundAreCut(t *testing.T) {
	t.Skip("TC-HP-E26: SKIP-only stub; un-skip when WI 4.5 lands")
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
// - Setup: UBU with SP-HUNG-NFS: `/mnt/nfs` (nfs4, fresh `maj:min` `0:250`) is added with `addMount(…, both)`, so it is visible and the seam is reached. F-STATFS `hang` covers the whole seam call, including the directory check (Finding 2). Timeout 250 ms.
// - Steps: filesystems handler; measure; call again at once (the call in flight is then `sourceTimeout` old: the stuck branch, A-42).
// - Expected: first returns within 450 ms (`sourceTimeout` + 200 ms); `/mnt/nfs` listed with size, used, available, usedPercent and inodes null with `timed out after 250ms: statfs /mnt/nfs`, never `not visible:` (which would mean the fixture, not the timeout, decided); all other mounts populated; the second call answers within 50 ms with `still pending: statfs /mnt/nfs`; F-FS recorded no access of any kind to `mnt/nfs` or a path under it, so the directory check did not run outside the bounded call; F-STATFS's count for `/mnt/nfs` is 1.
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
// - Setup: UBU with F-RUN for the failed-units argv: `hang` (timeout 250 ms); `ErrNotStarted` whose cause is ENOENT (the `realRun` shape of TC-HP-ERR18); exit 1 with bus error text.
// - Steps: health handler.
// - Expected: `failedServices.count` null and `names []`, both in `unavailable`, with `timed out after 250ms: /usr/bin/systemctl`, `not present: /usr/bin/systemctl` (Finding 7), `command failed: /usr/bin/systemctl exit 1`; the rest of health populated; no command output text in the response.
func TestTCHPERR25_FailedServiceQueryFailuresLeaveTheCount(t *testing.T) {
	t.Skip("TC-HP-ERR25: SKIP-only stub; un-skip when WI 4.4 lands")
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

// TC-HP-ERR28: assertExplicit catches each kind of violation. Owner WI 4.1. Requirements: FR-12.
//
// - Setup: a fake `testing.TB` that records failures; planted JSON-marshalable values.
// - Steps: run `assertExplicit` on: `{a: null}` (no unavailable); `{a: 1, unavailable: {a: "x"}}`; `{a: null, unavailable: {a: ""}}`; `{l: [], unavailable: {l: "r"}}`; nested `{o: {b: null}}`; `{series: [{values: [null]}]}`; `{a: null, unavailable: {a: "permission denied: /x"}}`.
// - Expected: the first four violations and the nested one are reported (except `l: []` with a reason, which is valid); the `series.values` null and the last, well-formed object pass.
func TestTCHPERR28_AssertExplicitCatchesEachKindOfViolation(t *testing.T) {
	t.Skip("TC-HP-ERR28: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-HP-E31: A mount point hidden in nexusd's mount namespace reports not visible, never numbers. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup:
//   - UBU F-FS. Mounts are added with `addMount`, and the root forms are set with `selfRoot` and `stackRoot` (test-strategy.md). Each writes mount IDs and parent IDs that fit its view, because D-9's reachability rule follows parent IDs. F-STATFS answers every path below with a distinct row, the hidden ones included, so a call that should not have been made would also leak numbers.
//   - Base (a): any `/tmp` line is removed from both files, so `/tmp` lives on `/`.
//   - Base (b): a host tmpfs at `/tmp` (`0:45`) is added with `both`.
//   - The hidden mount is `/tmp/hidden`: ext4, `8:99`, root `/`.
//   - A path-prefix control is in every variant: `/tmpdata` (ext4, `8:97`, `both`).
//
// - Steps: filesystems handler with `limit 1000`, once per variant:
//  1. Shape (a), PrivateTmp over a root-filesystem `/tmp` (D-9, Finding 14): `/tmp/hidden` with `underAncestor("/tmp")` on base (a). Self has `/`, the private bind at `/tmp` (root `/tmp/systemd-private-…/tmp`, the `maj:min` of `/`, parent `/`) and `/tmp/hidden` with parent `/`, the only entry at that path. Run it four times: the bind written before and after `/tmp/hidden`, each with mount IDs ascending and descending (`idOrder`).
//  2. The second line of defence: `/mnt/gone` (ext4, `8:96`) with `both`; F-STATFS returns `*fs.PathError{Err: ENOENT}` for it.
//  3. Over-mounted at its own path: `/srv/over` (ext4, `8:98`) with `overMounted("0:77")`. The over-mount is written first in self.
//  4. Shape (b), PrivateTmp over a host tmpfs `/tmp`: `/tmp/hidden` with `underAncestor("/tmp")` on base (b). Self has `/`, the copied tmpfs `/tmp`, the private bind stacked on it (`0:45`, parent the copied `/tmp`) and `/tmp/hidden`, whose parent is the copied `/tmp`. Run it in the same four orders as step 1.
//  5. Ambiguous: `/srv/twin` (ext4, `8:95`) with `ambiguous("0:78")`. Self has two entries at `/srv/twin`, both children of `/` and neither stacked on the other, one with `8:95` and one with `0:78`.
//  6. The fallback (`deny("proc/1/mountinfo")`, so the self entries are the candidates):
//     - 6a: `/tmp/hidden` with `both`, and F-STATFS ENOENT for it;
//     - 6b: `/tmp/hidden` with `underAncestor("/tmp")` on base (a).
//  7. Positive controls:
//     - 7a: `/tmp/hidden` with `both` on each base;
//     - 7b: a root stack (`stackRoot()`): a second `/` entry with the root's `maj:min`, stacked on the root entry. `/srv/upper` (ext4, `8:94`) is a child of the upper `/` entry, and `/srv/lower` (ext4, `8:93`) is a child of the covered lower one;
//     - 7c: the self root entry in proc(5)'s namespace-root form, with its own mount ID as its parent ID (`selfRoot(ownParent)`). Every other variant uses the unlisted-parent form (`selfRoot(unlisted)`), as a captured self file has it.
//
// - Expected:
//   - Steps 1, 3, 4, 5 and 6b: the hidden item (`/tmp/hidden`, `/srv/over` or `/srv/twin`) is listed with `fsType ext4`. In steps 1, 3, 4 and 5 its `readOnly` comes from PID 1's line.
//   - Its `sizeBytes`, `usedBytes`, `availableBytes`, `usedPercent`, `inodesTotal`, `inodesFree` and `inodesUsed` are null, with `not visible: <mount> is hidden in nexusd's mount namespace`.
//   - F-STATFS's count for it is 0 (D-9: no syscall on a hidden mount), so none of its configured numbers appears.
//   - Every order of steps 1 and 4 gives the same result.
//   - Step 2: `/mnt/gone` has null usage with `not visible: /mnt/gone is hidden in nexusd's mount namespace`, and F-STATFS's count for it is 1: the seam was asked and answered ENOENT.
//   - Step 4: `/tmp` itself (the private bind on the host's tmpfs, with the same `maj:min`, `0:45`) is listed with its F-STATFS numbers, count 1. This pins A-44: a stacked entry needs its parent only walked.
//   - Step 6a: `/tmp/hidden` has null usage with `not present: /tmp/hidden`, and `readOnly` is null with TC-HP-ERR21's reason. Step 6b: `not visible:`, count 0, so the rule also applies under the fallback.
//   - Step 7a: `/tmp/hidden` is listed with its F-STATFS numbers, count 1, on both bases. So the hidden results above come from the self view, not from the path, the `maj:min` or the fstype.
//   - Step 7b: `/` and `/srv/upper` are listed with numbers. `/srv/lower` is `not visible:` with count 0, because a child of a covered entry is not walked.
//   - Step 7c: `/` is listed with numbers.
//   - Every variant:
//   - `/tmpdata` and `/` keep their numbers. `/tmp` hides `/tmp/hidden` but is not a path-prefix ancestor of `/tmpdata`;
//   - `assertExplicit` passes;
//   - F-FS recorded no access to `tmp/hidden`, `srv/over`, `srv/twin`, `srv/upper`, `srv/lower` or `mnt/gone`.
func TestTCHPE31_AMountPointHiddenInNexusdsMountNamespace(t *testing.T) {
	t.Skip("TC-HP-E31: SKIP-only stub; un-skip when WI 4.2 lands")
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

// TC-HP-E33: The subvolumes of one btrfs filesystem are listed once, with numbers, never not visible. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup:
//   - Variant (a): UBU F-FS with the root line replaced in both files by a btrfs line: `maj:min` `0:31`, root `/@`, source `/dev/sda2`, super options `rw,subvol=/@`. `/home` is added with `addMount(…, both)`: btrfs, `sameDevAs("/")` (`0:31`), root `/@home`, super options `rw,subvol=/@home`. F-STATFS answers `/` and `/home` with distinct rows.
//   - Variant (b): the capture's ext4 root is unchanged. `/home` is added with `both`: btrfs, a fresh `0:31`, root `/@home`.
//   - `Usage` has no device field (A-38), so `tools/stat-mounts.txt`'s `%d` cannot affect either variant.
//
// - Steps: filesystems handler with `limit 1000`, per variant.
// - Expected:
//   - Variant (a): one item for `0:31`, at `/`, with `fsType btrfs` and `/`'s F-STATFS numbers. There is no `/home` item (one filesystem is listed once; FR-7, A-21) and no `not visible:` reason anywhere. F-STATFS's count is 1 for `/` and 0 for `/home`: one seam call per filesystem.
//   - Variant (b): `/home` is listed with `fsType btrfs` and its F-STATFS numbers, count 1.
func TestTCHPE33_TheSubvolumesOfOneBtrfsFilesystem(t *testing.T) {
	t.Skip("TC-HP-E33: SKIP-only stub; un-skip when WI 4.2 lands")
}
