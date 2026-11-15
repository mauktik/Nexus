//go:build integration

// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-114, TC-HP-115, TC-HP-116, TC-HP-117, TC-HP-118, TC-HP-119, TC-HP-120, TC-HP-121, TC-HP-122, TC-HP-123, TC-HP-124, TC-HP-125, TC-HP-126, TC-HP-131, TC-HP-E28, TC-HP-E29.
// Owner WIs: 5.3, 5.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package integration

import "testing"

// TC-HP-114: Every op under the exact hardened unit agrees with the host's own tools (systemd). Owner WI 5.3. Requirements: NFR-5, NFR-3, FR-4, FR-5, FR-6, FR-7, FR-8, FR-9, CON-1, CON-5.
//
// - Setup: `requireInit(t, platform.Systemd)`; `freshHost`; `runInstall`; `writeConfig` with `providers.host: {enabled: true, sampleInterval: 2s}`; `startExact`; `exactNexusd(t, "system:read")`; wait until `host.summary.get`'s `sampler.state` is `ready` (`waitUntil`, 10 s).
// - Steps: call all ten ops over REST (lists with `limit=1000`); run `hostname`, `cat /etc/os-release`, `uname -r`, `uname -m`, `nproc`, `free -b`, `df -B1 --output=target,size,used,avail,pcent`, `lsblk -b -J -o NAME,SIZE,ROTA,RM,TYPE`, `ip -j addr`, `ip -j route`, `ip -j -6 route` immediately before and after.
// - Expected: hostname, os ID/VERSION_ID/PRETTY_NAME, kernel, arch, logicalCpus exact; `availableBytes` within ±1% of free's available and `totalBytes` exact; each listed filesystem's size equals df exactly and used within one block (or the before/after df bracket); each lsblk disk's size/rota/rm equal; each interface's addresses with prefixes equal `ip -j addr`; default gateways equal `ip -j route`; every op answered 200.
func TestTCHP114_EveryOpUnderTheExactHardenedUnit(t *testing.T) {
	t.Skip("TC-HP-114: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-115: Every field is populated or explicitly unavailable under the unit (systemd). Owner WI 5.3. Requirements: FR-12, NFR-5.
//
// - Setup: as TC-HP-114.
// - Steps: decode each response into `map[string]any`; walk it with the inline D-5 walker; collect every `(op, json path, reason)` from `unavailable` maps; write them to `build/harness/logs/<stamp>/host-unavailable-systemd.txt`.
// - Expected: no null without a reason; no `unavailable` key naming a populated field; every reason starts with a vocabulary prefix; the list file exists and is logged.
func TestTCHP115_EveryFieldIsPopulatedOrExplicitlyUnavailable(t *testing.T) {
	t.Skip("TC-HP-115: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-116: No sandbox denial, crash or restart during the run (systemd). Owner WI 5.3. Requirements: CON-1, NFR-4, NFR-5.
//
// - Setup: as TC-HP-114, recording `since` and the kernel log line count before start.
// - Steps: after TC-HP-114's calls: `showUnit(nexusd, MainPID, NRestarts, ActiveState)`; `journalctl -u nexusd --since @<since>`; new kernel log lines.
// - Expected: MainPID equals `startExact`'s, `NRestarts 0`, `active`; the journal has none of `SIGSYS`, `Bad system call`, `seccomp`, `Permission denied`, `Operation not permitted`; no kernel `audit: type=1326` line for `nexusd`; the window is non-vacuous (it contains nexusd's start line and the sampler's info "sampler started" line).
func TestTCHP116_NoSandboxDenialCrashOrRestartDuring(t *testing.T) {
	t.Skip("TC-HP-116: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-117: PID 1's mountinfo is readable under the unit and later host mounts appear. Owner WI 5.3. Requirements: FR-7.
//
// - Setup: as TC-HP-114.
// - Steps: `host.filesystems.list`; then, with nexusd still running, mount a loop-backed ext4 image (`truncate -s 32M /var/tmp/tc-hp.img`, `mkfs.ext4 -q`, `mount -o loop` at `/mnt/tc-hp`; cleanup `umount` and `rm`) and list again; `df -B1 /mnt/tc-hp`. (A tmpfs is not used: tmpfs outside `/tmp`, `/run` and `/dev/shm` is excluded by FR-7.) Then the PrivateTmp case (Finding 2, A-13): mount a second loop-backed ext4 image at `/tmp/tc-hp-hidden` on the host (`mkdir -p`, `mount -o loop`; cleanup `umount`, `rmdir` and `rm`), list again, and run `df -B1 /tmp/tc-hp-hidden` on the host.
// - Expected:
//   - No item has `readOnly` in `unavailable`, and `/` has `readOnly false`.
//   - After the first mount, `/mnt/tc-hp` is listed with `sizeBytes` equal to df's and `fsType ext4`. This is A-13: a host mount made after start is visible, and statfs reaches it.
//   - After the second mount, the host's `df` reports numbers for `/tmp/tc-hp-hidden`, so the mount is real. The provider lists `/tmp/tc-hp-hidden` with `fsType ext4`. Its `sizeBytes`, `usedBytes`, `availableBytes`, `usedPercent` and inode fields are null, with the reason `not visible: /tmp/tc-hp-hidden is hidden in nexusd's mount namespace`. nexusd's private `/tmp` hides the mount, and the provider must never report the numbers of whatever filesystem the path resolves to there.
//   - The test runs `nsenter -m -t <MainPID> stat -c '%d' /tmp/tc-hp-hidden` and logs the result: ENOENT, or a device number different from the host's `stat -c '%d'`. That records which seam result produced the reason, as A-13's evidence.
func TestTCHP117_PID1SMountinfoIsReadableUnder(t *testing.T) {
	t.Skip("TC-HP-117: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-118: A failed transient unit appears in failedServices (systemd). Owner WI 5.3. Requirements: FR-10.
//
// - Setup: as TC-HP-114; `systemd-run --unit nx-host-failed /bin/false`; wait until `systemctl is-failed nx-host-failed` prints `failed`; cleanup `systemctl reset-failed nx-host-failed`.
// - Steps: `host.health.get`; `systemctl --failed --plain --no-legend`.
// - Expected: `failedServices.names` contains `nx-host-failed.service`; `count` equals the number of lines systemctl prints.
func TestTCHP118_AFailedTransientUnitAppearsInFailedServices(t *testing.T) {
	t.Skip("TC-HP-118: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-119: The reboot-required marker drives rebootRequired (systemd, Ubuntu). Owner WI 5.3. Requirements: FR-10.
//
// - Setup: as TC-HP-114; save and restore any existing marker files in cleanup.
// - Steps: write `/var/run/reboot-required` and `.pkgs` = `linux-image-tc-hp`; `host.health.get`; remove both; `host.health.get`.
// - Expected: first `state "true"` with `packages ["linux-image-tc-hp"]`; second `"false"` if any `apt.RebootMarkerWriters` path exists on the harness (`test -e`), else `"unknown"`; the apt provider's capabilities `rebootRequired` (if enabled) agrees. (Encodes open item 1.)
func TestTCHP119_TheRebootRequiredMarkerDrivesRebootRequiredSystemd(t *testing.T) {
	t.Skip("TC-HP-119: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-120: A CPU burner appears in topByCpu (systemd). Owner WI 5.3. Requirements: FR-10, FR-11, NFR-3.
//
// - Setup: as TC-HP-114; start `sh -c 'while :; do :; done'` (cleanup kill).
// - Steps: poll `host.health.get` for up to 3 intervals (6 s).
// - Expected: an entry with the burner's PID, `command "sh"`, `user "root"`, `cpuPercent ≥ 50`; `ps -o pcpu= -p <pid>` (or `/proc/<pid>/stat` delta) agrees within ±20 percentage points.
func TestTCHP120_ACPUBurnerAppearsInTopByCpuSystemd(t *testing.T) {
	t.Skip("TC-HP-120: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-121: Disable by reload stops the provider; re-enable starts empty and warming up (systemd). Owner WI 5.3. Requirements: FR-1, FR-11, FR-13, NFR-2.
//
// - Setup: as TC-HP-114 with ≥ 3 history points.
// - Steps:
//  1. `writeConfig` with `enabled: false`; `systemctl reload nexusd`; call each op.
//  2. `writeConfig` with `enabled: true`; reload; immediately call `host.metrics.get` and `host.cpu.get`; wait 2 intervals; call again.
//
// - Expected: 1: every op answers `capability-unavailable` and the journal shows "sampler stopped" within one interval of the reload. 2: first `points 0`, `sampler.state warming-up`, `usagePercent` null with `warming up`; then `ready` with ≥ 1 point. MainPID unchanged throughout.
func TestTCHP121_DisableByReloadStopsTheProviderRe(t *testing.T) {
	t.Skip("TC-HP-121: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-122: Every op under OpenRC agrees with BusyBox and the host's files. Owner WI 5.3. Requirements: NFR-5, NFR-3, CON-5, FR-4, FR-5, FR-6, FR-7, FR-9.
//
// - Setup: `requireInit(t, platform.OpenRC)`; `freshHost`; `runInstall`; `writeConfig` (as TC-HP-114); `lib(t, "start-nexusd")`; `createToken(t, "tc-hp", "system:read")`.
// - Steps: call all ten ops; compare with `hostname`, `/etc/os-release`, `uname -r -m`, `nproc` (or `grep -c ^processor /proc/cpuinfo`), `free -b`, `df -B1` (or `df -k` × 1024), `ip -o addr`, `ip route`, `ip -6 route`.
// - Expected: the same comparisons and tolerances as TC-HP-114 (no lsblk; disk sizes against `/sys/block/*/size` × 512 read by the test); the inline walker of TC-HP-115 passes and writes `host-unavailable-openrc.txt`; `rc-service nexusd status` stays `started` with the same PID (`openrcMainPID`).
func TestTCHP122_EveryOpUnderOpenRCAgreesWithBusyBox(t *testing.T) {
	t.Skip("TC-HP-122: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-123: A crashed OpenRC service appears in failedServices. Owner WI 5.3. Requirements: FR-10, CON-5.
//
// - Setup: TC-HP-122's daemon; write `/etc/init.d/nx-host-crash` (`command=/bin/sleep`, `command_args=3600`, `command_background=yes`, `pidfile=/run/nx-host-crash.pid`); `rc-service nx-host-crash start`; `kill -9` its pid; cleanup `rc-service nx-host-crash zap` and remove the script.
// - Steps: `rc-status --crashed`; `host.health.get`.
// - Expected: `rc-status --crashed` lists `nx-host-crash`; `failedServices.names` contains `nx-host-crash`; `count` equals rc-status's line count; `/bin/rc-status` exists (A-17).
func TestTCHP123_ACrashedOpenRCServiceAppearsInFailedServices(t *testing.T) {
	t.Skip("TC-HP-123: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-124: In the Alpine container, rebootRequired is unknown and virtualization is a container. Owner WI 5.3. Requirements: FR-10, FR-5.
//
// - Setup: TC-HP-122's daemon.
// - Steps: `host.health.get`; `host.identity.get`.
// - Expected: `rebootRequired.state "unknown"` with container evidence; `virtualization.kind "container"` and `type` naming the marker found (`docker` when `/.dockerenv` exists, checked by the test).
func TestTCHP124_InTheAlpineContainerRebootRequiredIsUnknown(t *testing.T) {
	t.Skip("TC-HP-124: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-125: TC-182 sweeps the ten host ops under the exact unit. Owner WI 5.3. Requirements: NFR-5, CON-1, FR-2.
//
// - Setup: TC-182 with `host: {enabled: true, sampleInterval: 2s}` added to its config and `system:read` to its token scopes.
// - Steps: run TC-182.
// - Expected: the count of exercised ops equals the catalog's op count including the ten host ops; each host op answered 200.
func TestTCHP125_TC182SweepsTheTenHostOps(t *testing.T) {
	t.Skip("TC-HP-125: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-126: Identity reports the harness's init system and OS on both targets. Owner WI 5.3. Requirements: CON-5, FR-5.
//
// - Setup: either target's daemon (no `requireInit`; runs on both).
// - Steps: `host.identity.get`; `detectedInit()`; parse `/etc/os-release`.
// - Expected: `initSystem` = `detectedInit()`; `os.id` = os-release ID; the same values as `GET /v1/capabilities`.
func TestTCHP126_IdentityReportsTheHarnessSInitSystem(t *testing.T) {
	t.Skip("TC-HP-126: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-131: The documented sandbox-unavailable list matches what T1 observed. Owner WI 5.4. Requirements: NFR-5, FR-12, NFR-7.
//
// - Setup: TC-HP-115's collected list; `/harness/src/docs/host.md`.
// - Steps: extract the "Unavailable under the systemd sandbox" section; for each observed `(field, reason prefix)` caused by the sandbox (`permission denied:` or `command failed:` reasons), look it up.
// - Expected: every observed sandbox-caused field is listed with its cause; the section no longer says "to be confirmed by WI 5.4".
func TestTCHP131_TheDocumentedSandboxUnavailableListMatchesWhat(t *testing.T) {
	t.Skip("TC-HP-131: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-HP-E28: Processes of other users are sampled under the unit (systemd). Owner WI 5.3. Requirements: FR-10, ASM-4.
//
// - Setup: TC-HP-114's daemon; a burner as `nobody` (`setpriv --reuid=65534 --regid=65534 --clear-groups sh -c 'while :; do :; done'`).
// - Steps: poll `host.health.get` for 3 intervals.
// - Expected: the burner is in `topByCpu` with `uid 65534`, `user "nobody"`, `cpuPercent ≥ 50` (A-28: no hidepid; `/proc/<pid>/status` readable).
func TestTCHPE28_ProcessesOfOtherUsersAreSampledUnder(t *testing.T) {
	t.Skip("TC-HP-E28: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-HP-E29: Clock synchronization under the unit matches timedatectl or is explained. Owner WI 5.3. Requirements: FR-5, CON-1.
//
// - Setup: either target's daemon.
// - Steps: `host.identity.get`; on systemd, `timedatectl show -p NTPSynchronized --value` run by the test.
// - Expected: systemd: `clock.synchronized` equals `"true"`/`"false"` for `yes`/`no`, or is unavailable with a `command failed:`/`timed out` reason (then listed in host.md, TC-HP-131); OpenRC without chrony: `"unknown"` with `not supported:`; no SIGSYS in either case (TC-HP-116's scan covers the run).
func TestTCHPE29_ClockSynchronizationUnderTheUnitMatchesTimedatectl(t *testing.T) {
	t.Skip("TC-HP-E29: SKIP-only stub; un-skip when WI 5.3 lands")
}
