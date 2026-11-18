// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-89, TC-SV-90, TC-SV-91, TC-SV-92, TC-SV-93, TC-SV-94, TC-SV-95, TC-SV-96, TC-SV-97, TC-SV-98, TC-SV-99, TC-SV-100, TC-SV-101, TC-SV-ERR22, TC-SV-ERR23, TC-SV-ERR24.
// Owner WIs: 4.3. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package services

import "testing"

// TC-SV-89: Admission refuses in the DF-3 order, creates nothing and sends nothing. Owner WI 4.3. Requirements: FR-7, FR-8, FR-9, NFR-4.
//
// - Setup:
//   - `t1-ubuntu-systemd` F-HOSTS;
//   - allow-list `[{name: nx-svc-test, actions: [restart]}, {name: nx-missing}, {name: nx-netalias}]`, where `nx-netalias`'s `show` answers `Id=systemd-networkd.service`;
//   - the dynamic protection of TC-SV-77 for `nexusd-db`;
//   - the real Dispatcher, store and audit log.
//
// - Steps:
//  1. `start ssh`.
//  2. `start nexusd-db`, which is protected and not allow-listed.
//  3. `start nx-missing`.
//  4. `start nx-netalias`.
//  5. `start cron`.
//  6. `start nx-svc-test`.
//  7. `restart nx-svc-test`, as the positive control.
//
// - Expected:
//
//   - (1) 409 `protected-service`, `service: ssh`;
//
//   - (2) 409 `protected-service`, not `not-allowed`;
//
//   - (3) 404 `not-found`;
//
//   - (4) 409 `protected-service`, with `reason` naming the canonical `systemd-networkd`;
//
//   - (5) 409 `not-allowed`, `configKey: providers.services.control`;
//
//   - (6) 409 `not-allowed`, `action: start`;
//
//   - (7) 202.
//
//     For each of (1) to (6): the store holds no operation, the audit log has `refused` with that slug, and `len(fh.Sent()) == 0`. Case (1) makes no F-HOSTS call at all (refused by name before `Describe`).
func TestTCSV89_AdmissionRefusesInTheDF3Order(t *testing.T) {
	t.Skip("TC-SV-89: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-90: start brings a stopped service to running and reports the final state. Owner WI 4.3. Requirements: FR-7, FR-14.
//
// - Setup: `nx-svc-test` stopped in F-HOSTS (settle 2), the helper run in-process on a temp `--dir` with Prepare's `input.json`, and the poll interval 250 ms (the seam may lower it to 10 ms).
// - Steps:
//  1. `Helper(["run","--dir",dir])`, then `Finalize`.
//
// - Expected:
//   - exit 0;
//   - `result.json` decodes to `ServiceResult{changed: true, service: nx-svc-test, action: start, state: running, rawState: "active (running)", mainPid: <fake's new PID>, since: <fake's ActiveEnterTimestamp>}`;
//   - `Sent()` = `[/usr/bin/systemctl start nx-svc-test.service]`;
//   - the operation log contains `services-op:` lines with the init output;
//   - the result carries `contentTrust: untrusted-host-data`.
func TestTCSV90_StartBringsAStoppedServiceToRunning(t *testing.T) {
	t.Skip("TC-SV-90: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-91: start on a running service and stop on a stopped one send nothing and report changed false. Owner WI 4.3. Requirements: FR-7.
//
// - Setup: F-HOSTS: `nx-svc-test` running in one subtest and stopped in another, on systemd and OpenRC.
// - Steps:
//  1. Run the helper with `start` (running) and with `stop` (stopped).
//
// - Expected: `changed: false`, the final state equal to the initial one, and `len(Sent()) == 0`.
func TestTCSV91_StartOnARunningServiceAndStop(t *testing.T) {
	t.Skip("TC-SV-91: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-92: stop and restart reach their target states, restart with a new main PID. Owner WI 4.3. Requirements: FR-7.
//
// - Setup: `nx-svc-test` running with `MainPID` P0 and `ActiveEnterTimestamp` T0.
// - Steps:
//  1. `stop`.
//  2. Reset the fake to running, then `restart`.
//
// - Expected:
//   - (1) `state: stopped`, `changed: true`, `mainPid` null;
//   - (2) `state: running`, `changed: true`, `mainPid ≠ P0`, `since > T0`;
//   - `Sent()` is exactly one argv each.
func TestTCSV92_StopAndRestartReachTheirTargetStates(t *testing.T) {
	t.Skip("TC-SV-92: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-93: A stop of a failed unit that stays failed with no main PID succeeds with changed false. Owner WI 4.3. Requirements: FR-7.
//
// - Setup: `nx-svc-fail` failed (`MainPID=0`), with F-HOSTS's captured `stop` of a failed unit.
// - Steps:
//  1. `stop nx-svc-fail`.
//
// - Expected: Succeeded with `changed: false`, `state: failed`, no `service-action-failed`, and one `Sent()` stop.
func TestTCSV93_AStopOfAFailedUnitThat(t *testing.T) {
	t.Skip("TC-SV-93: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-94: A second action on the same service answers 409 conflict, while another service runs in parallel. Owner WI 4.3. Requirements: FR-7.
//
// - Setup: The real ops manager with a gated launcher, holding the first op Running.
// - Steps:
//  1. `restart nx-svc-test`, held.
//  2. `stop nx-svc-test`.
//  3. `start nx-svc-fail`.
//
// - Expected:
//   - (2) 409 `conflict` with `blockingOperationId` = (1)'s ID, and no new operation;
//   - (3) 202, Running at the same time as (1);
//   - the binding declares the lane and `LaneReject` (the lane check TC-SV-32 dropped, Revision 1): for each of start, stop and restart, the binding's `DetachedFuncs.Lane` on the decoded input `{name: nx-svc-test}` returns `provider.Lane{Key: "service:nx-svc-test", Policy: provider.LaneReject}`.
func TestTCSV94_ASecondActionOnTheSameService(t *testing.T) {
	t.Skip("TC-SV-94: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-95: A reload during an operation keeps the operation's snapshot. Owner WI 4.3. Requirements: FR-1, FR-7.
//
// - Setup: Allow-list `[{name: nx-svc-test, timeout: 1s}]`. F-HOSTS keeps `nx-svc-test` `activating` forever.
// - Steps:
//  1. `start nx-svc-test`, and wait until Prepare has written `input.json`.
//  2. `Configure({control: [{name: nx-svc-test, timeout: 10m}]})`, then `Configure({control: []})`.
//  3. Wait for terminal.
//
// - Expected:
//   - `input.json` has `timeoutSeconds: 1`;
//   - the op ends `outcome-unknown` about 1 s after the action, not after 10 min;
//   - it was not refused by the later empty allow-list.
func TestTCSV95_AReloadDuringAnOperationKeepsThe(t *testing.T) {
	t.Skip("TC-SV-95: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-96: control with an action takes the same path and lane as the per-verb op. Owner WI 4.3. Requirements: FR-7, FR-15, NFR-6.
//
// - Setup: As TC-SV-90 and TC-SV-94.
// - Steps:
//  1. `control {name: nx-svc-test, action: start}` from stopped.
//  2. While a `restart nx-svc-test` is held, `control {name: nx-svc-test, action: stop}`.
//  3. `control {name: ssh, action: restart}`.
//
// - Expected:
//   - (1) the same `input.json` mode and fields, and the same result shape and values, as TC-SV-90;
//   - (2) 409 `conflict` (same lane);
//   - (3) 409 `protected-service`;
//   - the binding declares the lane and `LaneReject` (Revision 1): control's `DetachedFuncs.Lane` on `{name: nx-svc-test, action: stop}` returns `provider.Lane{Key: "service:nx-svc-test", Policy: provider.LaneReject}`, equal to the per-verb ops' lane for the same name.
func TestTCSV96_ControlWithAnActionTakesTheSame(t *testing.T) {
	t.Skip("TC-SV-96: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-97: The helper re-checks the name and its canonical Id against the snapshotted resolved protected set before any action. Owner WI 4.3. Requirements: FR-9.
//
// - Setup: Each subtest writes `input.json` by hand in the helper's `input.json` type (`helperio.go`: the resolved protected set `[{name, reason}]` and the canonical `Id`), as tampering or a race after admission would. The F-HOSTS fake records every argv (`Sent()`). Three subtests:
//   - (a) static: the request names `ssh`, the snapshotted set holds the static list with its reasons;
//   - (b) dynamic only: the snapshotted set holds `{name: nexusd-db, reason: "Requires= of the self unit"}` and the request names `nexusd-db`; `nexusd-db` is in no static list, so a helper that checks only `spec.Protected` would act;
//   - (c) alias: the request names `nx-netalias`, the snapshot's canonical `Id` is `systemd-networkd`, and the snapshotted set holds `systemd-networkd` with its static reason; `nx-netalias` itself is in no list.
//
// - Steps:
//  1. Run the helper, once per subtest.
//
// - Expected:
//   - (a) `result.json` holds the problem `protected-service` (`service: ssh`) with the snapshot's reason for `ssh`;
//   - (b) `result.json` holds `protected-service` (`service: nexusd-db`) whose reason is exactly `Requires= of the self unit`, taken from the snapshot;
//   - (c) `result.json` holds `protected-service` naming the canonical `systemd-networkd` with the snapshot's reason;
//   - in every subtest `len(Sent()) == 0`: no `systemctl` or `rc-service` argv was run, not even a status read.
func TestTCSV97_TheHelperReChecksTheStaticProtected(t *testing.T) {
	t.Skip("TC-SV-97: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-98: Prepare snapshots the entry and declares the helper argv and bounds. Owner WI 4.3. Requirements: FR-7, CON-1, CON-2.
//
// - Setup: `Options.Executable` = `/usr/local/bin/nexusd`, and the entry `{name: nx-svc-test, actions: [start, restart], timeout: 45s}`.
// - Steps:
//  1. Call Prepare for `start nx-svc-test` with a temp op directory.
//
// - Expected:
//   - `detach.Spec` argv is `[/usr/local/bin/nexusd services-helper run --dir <dir>]`, with `KillMode: process` and `MaxRuntime == 45s + 2*30s + 60s`;
//   - `input.json` is mode 0600 and holds `mode: control`, `service: nx-svc-test`, `action: start`, `timeoutSeconds: 45`, `actions: [start restart]`, `init: systemd` and the protected names.
func TestTCSV98_PrepareSnapshotsTheEntryAndDeclaresThe(t *testing.T) {
	t.Skip("TC-SV-98: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-99: OpenRC control through F-HOSTS uses rc-service and its captured outputs. Owner WI 4.3. Requirements: FR-7, CON-3.
//
// - Setup: `t1-alpine-openrc` F-HOSTS.
// - Steps:
//  1. `start nx-svc-test` (stopped), `restart`, `stop`, and `start nx-svc-fail`.
//
// - Expected:
//   - `Sent()` = `/sbin/rc-service nx-svc-test start`, `… restart`, `… stop` and `/sbin/rc-service nx-svc-fail start`;
//   - final states `running`, `running`, `stopped`, and `service-action-failed` with the captured output and exit code for `nx-svc-fail`.
func TestTCSV99_OpenRCControlThroughFHOSTSUsesRc(t *testing.T) {
	t.Skip("TC-SV-99: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-100: Idempotency-Key replays the same operation. Owner WI 4.3. Requirements: FR-7.
//
// - Setup: As TC-SV-90.
// - Steps:
//  1. `start nx-svc-test` with `Idempotency-Key: k1`, twice.
//  2. Then `stop` with the same key.
//
// - Expected:
//   - both starts return the same operation ID, with one launch and one `Sent()` start;
//   - the stop with the reused key answers as the existing idempotency rules do (409 `idempotency-key-reused`, or the slug the registry uses for a key reused with another input).
func TestTCSV100_IdempotencyKeyReplaysTheSameOperation(t *testing.T) {
	t.Skip("TC-SV-100: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-101: Start, stop and restart are audited requested then completed with their facts. Owner WI 4.3. Requirements: FR-16.
//
// - Setup: As TC-SV-90, with a token `tokW`.
// - Steps:
//  1. `start`, `stop` and `restart nx-svc-test`.
//
// - Expected: For each: a `requested` record at admission and a terminal `completed` record, both with `service=nx-svc-test`, `action=<verb>` and token `tokW`. `control` records `action` from its input.
func TestTCSV101_StartStopAndRestartAreAuditedRequested(t *testing.T) {
	t.Skip("TC-SV-101: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-ERR22: A failed start ends service-action-failed with the captured state, exit code and a 2 KiB quote. Owner WI 4.3. Requirements: FR-7, NFR-4.
//
// - Setup: F-HOSTS `nx-svc-fail` `FailsOnStart`, with its captured output padded to 5 KiB with captured lines.
// - Steps:
//  1. Run the helper with `start nx-svc-fail`, then Finalize.
//
// - Expected:
//   - Failed `service-action-failed`, with `service: nx-svc-fail`, `action: start`, `state: failed`, `rawState: "failed (failed)"` and `exitCode: 1`;
//   - the detail quotes the first 2 KiB of the output exactly;
//   - the operation log holds the full 5 KiB.
func TestTCSVERR22_AFailedStartEndsServiceActionFailed(t *testing.T) {
	t.Skip("TC-SV-ERR22: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-ERR23: A service still in transition at the timeout ends outcome-unknown. Owner WI 4.3. Requirements: FR-7.
//
// - Setup: F-HOSTS keeps `nx-svc-slow` `activating`, with the entry timeout 1 s.
// - Steps:
//  1. Start.
//
// - Expected: Failed `outcome-unknown` after about 1 s (within 1 s + 2 polls), with a detail naming `starting`, the service and the 1 s bound.
func TestTCSVERR23_AServiceStillInTransitionAtThe(t *testing.T) {
	t.Skip("TC-SV-ERR23: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SV-ERR24: A tool that never started ends dependency-unavailable and changes nothing. Owner WI 4.3. Requirements: FR-7, CON-2.
//
// - Setup: F-HOSTS `notStarted(ENOENT)` for the `start` argv only.
// - Steps:
//  1. Start `nx-svc-test`.
//
// - Expected: Failed `dependency-unavailable`, naming `/usr/bin/systemctl`, with the state unchanged (`stopped`) and `Sent()` recorded but the fake's state machine not advanced.
func TestTCSVERR24_AToolThatNeverStartedEndsDependency(t *testing.T) {
	t.Skip("TC-SV-ERR24: SKIP-only stub; un-skip when WI 4.3 lands")
}
