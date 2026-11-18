//go:build integration

// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-135, TC-SV-136, TC-SV-137, TC-SV-138, TC-SV-139, TC-SV-140, TC-SV-141, TC-SV-142, TC-SV-143, TC-SV-144, TC-SV-145, TC-SV-146, TC-SV-147.
// Owner WIs: 5.3. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package integration

import "testing"

// TC-SV-135: T1 systemd list and get agree with systemctl in the same container. Owner WI 5.3. Requirements: FR-4, FR-5, NFR-2, CON-1, CON-3.
//
// - Setup: `requireInit(systemd)`, the planted services, `startExact`, `requireExactUnit`, and the token of test-strategy.md.
// - Steps:
//  1. Page through `GET /v1/providers/services/services`.
//  2. At the same moment, run `systemctl list-units --type=service --all --plain --no-legend` and `systemctl list-unit-files --type=service --plain --no-legend`, and `systemctl is-active`/`is-enabled` per name.
//  3. `GET …/services/nexusd` and `…/nx-svc-test`, compared with `systemctl show -p …`.
//
// - Expected:
//   - names are equal, and state and enabled are equal per name by TC-SV-46's mapping;
//   - a name whose state changed between the two reads (a timer-driven unit) is retried once and must then agree;
//   - the failed set equals `systemctl list-units --state=failed --type=service`'s names and contains `nx-svc-fail` (failed once at setup);
//   - get fields equal `show`'s.
func TestTCSV135_T1SystemdListAndGetAgreeWith(t *testing.T) {
	t.Skip("TC-SV-135: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-136: T1 OpenRC list and get agree with rc-status, rc-update and rc-service. Owner WI 5.3. Requirements: FR-4, FR-5, NFR-2, CON-3.
//
// - Setup: `requireInit(openrc)`, the planted scripts, `nexusd` under supervise-daemon (`openrcMainPID`).
// - Steps:
//  1. List, then `rc-status -s -C` and `rc-update show -v`.
//  2. Get `nx-svc-test`, then `rc-service -C nx-svc-test status`.
//
// - Expected: Names, states (TC-SV-49's mapping), runlevels and enabled are equal. Get's state equals `rc-service`'s, and `scriptPath` is `/etc/init.d/nx-svc-test`.
func TestTCSV136_T1OpenRCListAndGetAgreeWith(t *testing.T) {
	t.Skip("TC-SV-136: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-137: T1 logs agree with journalctl and with the declared OpenRC log file. Owner WI 5.3. Requirements: FR-6, NFR-2, CON-1, ASM-8.
//
// - Setup: `nx-svc-test` started, run for ≥ 8 s, then stopped with the host's own tool (`systemctl stop` / `rc-service stop`), so its log no longer grows and the two reads see the same lines. `T_start` is when it was started.
// - Steps:
//  1. Systemd: `GET …/nx-svc-test/logs?tail=5`, then `journalctl -u nx-svc-test.service -n 5 -o cat`; and `since=<T_start>` with no tail.
//  2. OpenRC: `logs tail=5`, then `tail -n 5 /var/log/nx-svc-test.log`.
//  3. `logs?priority=err` on OpenRC.
//
// - Expected:
//   - the 5 frame messages equal the tool's 5 lines, in the same order;
//   - the since frames equal `journalctl -u nx-svc-test.service --since=@<T_start> -o cat`'s `nx-svc-line-*` lines, starting at `nx-svc-line-0`, with consecutive numbers;
//   - (3) 400 `validation-failed`.
func TestTCSV137_T1LogsAgreeWithJournalctlAndWith(t *testing.T) {
	t.Skip("TC-SV-137: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-138: T1 start, stop and restart of nx-svc-test match the host's own state readback. Owner WI 5.3. Requirements: FR-7, NFR-4, CON-1, CON-3.
//
// - Setup: `nx-svc-test` stopped and allow-listed.
// - Steps:
//  1. POST start, wait, then `systemctl is-active` / `rc-service -C nx-svc-test status`.
//  2. Note `MainPID` and `ActiveEnterTimestamp`, POST restart, wait, and read both again.
//  3. POST stop, wait, and read back.
//  4. POST stop again.
//
// - Expected:
//   - (1) the result `state: running` and the tool says `active` / `started`;
//   - (2) the result `running`, with a new MainPID and a later timestamp, matching the tool;
//   - (3) `stopped`, and the tool says `inactive` / `stopped`;
//   - (4) `changed: false`, with `ActiveEnterTimestamp`/`InactiveEnterTimestamp` unchanged;
//   - on systemd, each action ran in a `nexus-op-<id>` unit (journal `_SYSTEMD_UNIT`), not in `nexusd.service`.
func TestTCSV138_T1StartStopAndRestartOfNx(t *testing.T) {
	t.Skip("TC-SV-138: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-139: T1 a failed start reports service-action-failed and the host shows the unit failed. Owner WI 5.3. Requirements: FR-7, NFR-4.
//
// - Setup: `nx-svc-fail` allow-listed (timeout 15 s), after `reset-failed` / `zap`.
// - Steps:
//  1. POST start, wait.
//  2. `systemctl show -p ActiveState,Result nx-svc-fail` / `rc-service status`.
//
// - Expected:
//   - Failed `service-action-failed`, with `state: failed`, `exitCode` equal to `systemctl start`'s own exit code (1 on systemd; the OpenRC captured code), and the output quoted ≤ 2 KiB;
//   - the tool reports `failed` (`Result=exit-code`) / `crashed` or `stopped`, as the T1 capture of the same action shows.
func TestTCSV139_T1AFailedStartReportsServiceAction(t *testing.T) {
	t.Skip("TC-SV-139: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-140: T1 every FR-9 refusal sends nothing: the state is unchanged and no nexus-op unit appears. Owner WI 5.3. Requirements: FR-9, NFR-4, FR-16.
//
// - Setup:
//   - systemd names `nexusd`, `ssh`, `dbus`, `systemd-journald`, `getty@tty1` and `nx-netalias` (canonical `systemd-networkd`);
//   - OpenRC names `nexusd`, `sshd`, `networking` and the `net` provider;
//   - before each request, read `ActiveEnterTimestamp`/`MainPID` (or `rc-service status`) and count `nexus-op-*` units (`systemctl list-units --all 'nexus-op-*'`) or the op directories.
//
// - Steps:
//  1. POST start, stop and restart for each name, and `control` with each action.
//
// - Expected:
//   - each answers 409 `protected-service`. The static names are refused by name before any lookup, so this holds even where the container lacks the service (for example `ssh`);
//   - the readbacks are unchanged, and the `nexus-op-*` count and `/var/lib/nexus/ops/` entries are unchanged;
//   - the audit log file has one `refused` record per request with slug `protected-service`.
func TestTCSV140_T1EveryFR9RefusalSendsNothing(t *testing.T) {
	t.Skip("TC-SV-140: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-141: T1 not-allowed and unknown names create nothing. Owner WI 5.3. Requirements: FR-7, FR-8.
//
// - Setup: `nx-svc-other` installed but not allow-listed. `nx-missing` allow-listed but not installed (capabilities show `status: missing`).
// - Steps:
//  1. POST start `nx-svc-other` and start `nx-missing`.
//
// - Expected: 409 `not-allowed` (`configKey: providers.services.control`) and 404 `not-found` respectively, with no operation and `nx-svc-other` still inactive.
func TestTCSV141_T1NotAllowedAndUnknownNamesCreate(t *testing.T) {
	t.Skip("TC-SV-141: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-142: T1 the reboot is unavailable without a boot-ID mount and available with one. Owner WI 5.3. Requirements: FR-3, FR-10, A-3, ASM-7.
//
// - Setup: `reboot.enabled: true` written with `writeConfig`.
// - Steps:
//  1. Start without the bind mount, and read capabilities.
//  2. Stop, `mount --bind /run/nx-boot-id /proc/sys/kernel/random/boot_id`, start, and read capabilities again.
//
// - Expected:
//   - (1) `rebootAvailable: false`, with the ASM-7 reason, and a POST reboot answers 409 `reboot-unavailable` with that reason;
//   - (2) `bootIdSource: container-mount`, `rebootAvailable: true`, and `cat /proc/sys/kernel/random/boot_id` inside the container equals `/run/nx-boot-id`.
func TestTCSV142_T1TheRebootIsUnavailableWithoutA(t *testing.T) {
	t.Skip("TC-SV-142: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-143: T1 every reboot guard refuses on the real host state. Owner WI 5.3. Requirements: FR-10, FR-16.
//
// - Setup: TC-SV-142 step 2 state. The host name is read from `system.capabilities.get`.
// - Steps:
//  1. `reboot.enabled: false` (file edit and SIGHUP), then POST.
//  2. Re-enable it and POST `confirmHost` = the host name uppercased.
//  3. `systemctl disable nexusd` (or `rc-update del nexusd default`), POST, and restore.
//  4. Start `nx-svc-slow` (Running about 20 s) and POST.
//  5. POST `{force: true, whatIf: true}` while `nx-svc-slow` runs.
//  6. Append `bogus: 1` to `/etc/nexus/nexusd.yaml` without a reload, POST, and restore.
//
// - Expected:
//   - (1) 409 `reboot-unavailable`;
//   - (2) 400 `validation-failed`;
//   - (3) 409 `nexusd-not-enabled-at-boot`, with `state` equal to `systemctl is-enabled nexusd`'s output;
//   - (4) 409 `operations-running` listing the slow op's ID;
//   - (5) Succeeded whatIf with the operations guard passed by `force`;
//   - (6) 422 `config-invalid`;
//   - every refusal leaves no reboot op directory, and the audit log has `refused` with the slug;
//   - the container's PID 1 start time (`/proc/1/stat` field 22) is unchanged throughout.
func TestTCSV143_T1EveryRebootGuardRefusesOnThe(t *testing.T) {
	t.Skip("TC-SV-143: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-144: T1 whatIf reports the guards with the container's boot ID and writes no record. Owner WI 5.3. Requirements: FR-10.
//
// - Setup: The TC-SV-142 step 2 state, with all guards passing.
// - Steps:
//  1. POST `{confirmHost: <host>, whatIf: true}`, and wait.
//
// - Expected:
//   - Succeeded with `whatIf: true`, `wouldReboot: true`, five guards passed;
//   - `bootId` equals `/run/nx-boot-id`, and `uptimeSeconds` is within 5 s of `/proc/uptime`'s first field;
//   - `rebootRequired.state` equals the host provider's `GET /v1/providers/host/health` value;
//   - no `reboot-requested` file in the op directory;
//   - PID 1 is unchanged.
func TestTCSV144_T1WhatIfReportsTheGuardsWithThe(t *testing.T) {
	t.Skip("TC-SV-144: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-145: T1 a delay-window cancel ends Canceled with no record and no trigger. Owner WI 5.3. Requirements: FR-10, FR-11, CON-5.
//
// - Setup: The TC-SV-142 step 2 state.
// - Steps:
//  1. POST `{confirmHost: <host>, delaySeconds: 300}`, and wait for Running.
//  2. On systemd, check that `systemctl show -p ActiveState nexus-op-<id>.service` is `active`.
//  3. `POST /v1/operations/<id>/cancel`.
//  4. Wait for terminal.
//
// - Expected:
//   - Canceled within 10 s;
//   - no `reboot-requested` ever existed (checked every 100 ms during the run);
//   - systemd: `systemctl list-jobs` never showed `reboot.target`, and the journal has no `reboot` request from the op unit;
//   - OpenRC: no `openrc shutdown` in `/var/log/messages` or the op log;
//   - PID 1 is unchanged and `nexusd` keeps answering;
//   - the audit log has `requested` then the cancel records.
func TestTCSV145_T1ADelayWindowCancelEndsCanceled(t *testing.T) {
	t.Skip("TC-SV-145: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-146: T1 reads under the exact unit cause no sandbox denial. Owner WI 5.3. Requirements: CON-1, NFR-4, ASM-8, A-1, A-2.
//
// - Setup: `startUnderSandbox`/`startExact` with `requireExactUnit`, and the start time `since`.
// - Steps:
//  1. List, get `nexusd`, logs `nexusd tail=200` and capabilities.
//  2. Run `journalFaults(systemLog(t))` over the window, restricted to `sandboxComms` plus `comm="journalctl"`.
//
// - Expected:
//   - all four answer 200, and logs returns ≥ 1 frame (the journal was readable under `CAP_DAC_READ_SEARCH`);
//   - no `SECCOMP`, `audit: type=1326`, `Operation not permitted` or `Permission denied` line from `nexusd`, `systemctl` or `journalctl`;
//   - `deploy/nexusd.service` installed equals the repository file (`requireExactUnit`).
func TestTCSV146_T1ReadsUnderTheExactUnitCause(t *testing.T) {
	t.Skip("TC-SV-146: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SV-147: T1 the protected set and selfService are resolved from the running unit. Owner WI 5.3. Requirements: FR-3, FR-9, A-8, A-11.
//
// - Setup: Defaults.
// - Steps:
//  1. Read capabilities.
//  2. Compare with `cat /proc/$(MainPID)/cgroup` and `systemctl show -p Requires,BindsTo,Requisite nexusd`, or with `/run/openrc/options/nexusd/child_pid` and `rc-service --resolve`/the deptree.
//
// - Expected:
//   - `selfService` is `nexusd`;
//   - `protected` contains every `.service` dependency the tool lists (systemd), or the `net` provider (OpenRC);
//   - `initSystem` equals the host provider's `initSystem`.
func TestTCSV147_T1TheProtectedSetAndSelfServiceAre(t *testing.T) {
	t.Skip("TC-SV-147: SKIP-only stub; un-skip when WI 5.3 lands")
}
