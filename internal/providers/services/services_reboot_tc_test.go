// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-102, TC-SV-103, TC-SV-104, TC-SV-105, TC-SV-106, TC-SV-107, TC-SV-108, TC-SV-109, TC-SV-110, TC-SV-111, TC-SV-112, TC-SV-113, TC-SV-114, TC-SV-116, TC-SV-150, TC-SV-E16, TC-SV-E17, TC-SV-E18, TC-SV-E20, TC-SV-ERR25, TC-SV-ERR26, TC-SV-ERR27, TC-SV-ERR29, TC-SV-ERR30, TC-SV-ERR31, TC-SV-ERR32.
// Owner WIs: 4.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package services

import "testing"

// TC-SV-102: Admission checks the reboot guards in FR-10 order and reports the first failure. Owner WI 4.4. Requirements: FR-10.
//
// - Setup:
//   - Host `h1`, F-OPS, F-DAEMON, F-HOSTS (`is-enabled nexusd` scripted), `reboot.enabled: true`, `rebootAvailable: true` (VM);
//   - each guard can be failed independently.
//
// - Steps:
//  1. Fail all five guards and admit with `confirmHost: h2`.
//  2. Fix them one at a time in FR-10 order, admitting after each fix.
//
// - Expected: The successive answers are:
//   - 400 `validation-failed` on `confirmHost`;
//   - 409 `reboot-unavailable` (`configKey: providers.services.reboot.enabled`);
//   - 409 `nexusd-not-enabled-at-boot` (`service: nexusd`, `state: disabled`);
//   - 409 `operations-running` (`operationIds: ["01X"]`);
//   - 422 `config-invalid`;
//   - then 202.
func TestTCSV102_AdmissionChecksTheRebootGuardsInFR(t *testing.T) {
	t.Skip("TC-SV-102: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-103: force skips only the operations guard and is an audit fact. Owner WI 4.4. Requirements: FR-10, FR-16.
//
// - Setup: As TC-SV-102, with F-OPS returning `01X` Running.
// - Steps:
//  1. Admit `{confirmHost: h1, force: true}`.
//  2. Make F-DAEMON's check fail and admit with `force: true`.
//  3. Disable `nexusd` at boot and admit with `force: true`.
//
// - Expected:
//   - (1) 202, with audit fact `force=true`;
//   - (2) 422 `config-invalid`;
//   - (3) 409 `nexusd-not-enabled-at-boot`.
func TestTCSV103_ForceSkipsOnlyTheOperationsGuardAnd(t *testing.T) {
	t.Skip("TC-SV-103: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-104: whatIf reports every guard and reboots nothing. Owner WI 4.4. Requirements: FR-10, FR-12.
//
// - Setup: F-ROOTS with `proc/sys/kernel/random/boot_id` = `CUR`, `proc/uptime` `5640281.12 …` and `proc/sys/kernel/osrelease` `7.0.0-42-generic`, read under `ProcRoot`. The host rule's debian marker is planted in F-ROOTS' host subtree: `var/run/reboot-required` and `var/run/reboot-required.pkgs` listing `linux-image-7.0.0-43-generic`, reaching `host.RebootRequiredNow` through `services.Options.HostFS = os.DirFS(root)`, with `ProcRoot = root/proc` from the same root (test-strategy F-ROOTS). Two subtests: guards all passing, and `nexusd` disabled plus F-OPS `01X`.
// - Steps:
//  1. Admit `{confirmHost: h1, whatIf: true}` and run the op through the helper.
//
// - Expected:
//   - Succeeded with `RebootResult{whatIf: true, bootId: CUR, uptimeSeconds: 5640281.12, rebootRequired.state: "true", rebootRequired.packages ⊇ [linux-image-7.0.0-43-generic]}`;
//   - `guards` lists all five in order, each with `passed` and `detail`;
//   - in the failing subtest, `wouldReboot: false`, `nexusdEnabledAtBoot: false` and `runningOperations: ["01X"]`, and the admission did not refuse;
//   - in both subtests: no `reboot-requested` file ever existed (the hook observed none), and F-HELPER's trigger was called 0 times.
func TestTCSV104_WhatIfReportsEveryGuardAndRebootsNothing(t *testing.T) {
	t.Skip("TC-SV-104: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-105: Admission records confirmHost, force and bootId as audit facts. Owner WI 4.4. Requirements: FR-16.
//
// - Setup: As TC-SV-102, all guards passing, with token `tokB`.
// - Steps:
//  1. Admit `{confirmHost: h1}`.
//
// - Expected: The `requested` record carries `confirmHost=h1`, `force=false`, `bootId=CUR` and token `tokB`.
func TestTCSV105_AdmissionRecordsConfirmHostForceAndBootIdAs(t *testing.T) {
	t.Skip("TC-SV-105: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-106: Prepare re-runs the guards excluding its own operation and snapshots the input. Owner WI 4.4. Requirements: FR-10, FR-11, NFR-7, A-37.
//
// - Setup: F-OPS returns the op's own ID as Running. `reboot.timeout: 5m`, `delaySeconds` omitted.
// - Steps:
//  1. Run Prepare.
//
// - Expected:
//   - no refusal (its own ID is excluded);
//   - `input.json` holds `mode: reboot`, `delaySeconds: 10`, `timeoutSeconds: 300`, `init: systemd`, `bootId: CUR`, `kernelRelease: 7.0.0-42-generic` and `rebootRequired` (from `host.RebootRequiredNow`);
//   - the spec has `KillMode: process`, `Timeout == 10s + 5m + 60s` and `MaxRuntime == 10s + 5m + 120s`;
//   - the spec's `Grace == 40*time.Second` exactly (DF-4 step 3, A-37: S4's 30 s trigger bound `svcctl.DefaultTimeout` plus a 10 s margin), so a stop's SIGTERM is never escalated to SIGKILL while the helper still decides whether to keep or retract the record. The test also asserts `Grace > svcctl.DefaultTimeout`, so a change to the trigger bound that eats the margin fails here. TC-SV-ERR30's logged SIGTERM-to-exit time and TC-SV-150's trigger-bound exit are the margin's evidence.
func TestTCSV106_PrepareReRunsTheGuardsExcludingIts(t *testing.T) {
	t.Skip("TC-SV-106: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-107: DF-5: a cancel during the delay ends Canceled with no record and no trigger. Owner WI 4.4. Requirements: FR-10, FR-11.
//
// - Setup: F-HELPER. The cancel is the real cancel: the cancel-request file and the stop (the test cancels the ctx the helper runs under, the in-process stand-in for the SIGTERM `Launcher.Cancel` sends). Two subtests:
//   - (a) during the delay: `Sleep` blocks until its ctx is done, and the test injects the cancel while `Sleep` is blocked;
//   - (b) at the delay's end: `hook("after-delay")` injects the cancel.
//
// - Steps:
//  1. Run the helper in reboot mode.
//
// - Expected: In both subtests:
//   - the helper exits as canceled (the existing helper cancel convention, ending `Canceled` through the manager); in (a) `Sleep` returned the ctx error;
//   - `ReadReboot` gives `ok == false`, and no `after-record` hook fired;
//   - trigger calls: 0.
func TestTCSV107_DF5ACancelDuringTheDelay(t *testing.T) {
	t.Skip("TC-SV-107: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-108: DF-5: a cancel before the second check stops the trigger, and one after it does not. Owner WI 4.4. Requirements: FR-10, FR-11, A-28.
//
// - Setup: F-HELPER, with the trigger fake returning exit 0 at once and `Sleep` returning at once for the delay. The hook injects the real cancel (the cancel-request file and the ctx cancel that stands in for SIGTERM) at one stage per subtest. The helper checks for a stop exactly twice, at step 2 and step 4, so the stage decides the rule:
//
//   - `after-check-1` (step 2 found no stop; before the record): S2, not S1. Step 3 writes the record without a check, and step 4 sees the cancel. This is the DF-5 race in which R2 read no record just before the helper wrote it;
//
//   - `after-record` (between the record and the second check): S2;
//
//   - `after-check-2` (step 4 found no stop; before the trigger): S4. No check runs between this hook and the trigger, so the trigger runs once (A-28's window opens here).
//
//     The test's hook records every stage it is called at, and the `ReadReboot` result at that moment.
//
// - Steps:
//  1. Run `runReboot`, once per subtest.
//
// - Expected:
//   - `after-check-1` and `after-record` alike: trigger calls 0; the record was written and then removed: the `after-record` hook fired and saw `ReadReboot` ok with `BootID == CUR`, and at exit `ReadReboot` gives `ok == false` (retracted at step 4); `after-check-2` never fired; `runReboot` returns 143; no `result.json`;
//   - `after-check-2`: exactly one trigger call; the record is present at exit with `BootID == CUR` (kept); `runReboot` returns 143 as soon as the trigger returns, with no `Sleep` call for the post-trigger timeout (step 6 sees the stop); no `result.json`, so the manager's R3 and R4 decide (TC-SV-15).
func TestTCSV108_DF5ACancelBetweenTheTwo(t *testing.T) {
	t.Skip("TC-SV-108: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-109: DF-5: a cancel while the trigger runs or after it (S4, S5) never kills the trigger. Owner WI 4.4. Requirements: FR-10, FR-11, A-28, A-36.
//
// - Setup: F-HELPER, `timeoutSeconds: 300`, `Sleep` returning at once for the delay. Each injected cancel is the real cancel (the cancel-request file and the ctx cancel standing in for SIGTERM). Four subtests:
//   - (a) S4, trigger succeeds: the trigger fake, once called, injects the cancel, then records `ctx.Err()` of the ctx it was given, and returns exit 0;
//   - (b) S4, trigger fails: as (a), but it returns exit 1 with output `Failed to reboot system via logind: Access denied`;
//   - (c) S5 at the hook: the trigger returns exit 0, and `hook("after-trigger")` injects the cancel. That hook runs after step 6's stop check and immediately before the post-trigger `Sleep`, which blocks until its ctx is done;
//   - (d) S5 during the wait: the trigger returns exit 0, nothing is injected at the hook, `Sleep(300 s)` blocks until its ctx is done, and the test injects the cancel while it is blocked.
//
// - Steps:
//  1. Run `runReboot`, once per subtest.
//
// - Expected:
//   - in every subtest: exactly one trigger call, and no stop reached the trigger: the ctx the trigger fake was given still has `Err() == nil` after the cancel (the trigger runs under `context.WithoutCancel`, A-36's in-process half; TC-SV-150 shows the process-group half);
//   - (a) the record is present at exit with `BootID == CUR`; `runReboot` returns 143 as soon as the trigger returns, with no post-trigger `Sleep` call; no `result.json`, so the manager's R3 and R4 decide (TC-SV-15);
//   - (b) the record is absent at exit (retracted); `result.json` holds `reboot-did-not-happen` with `stage: trigger` and `exitCode: 1`; `runReboot` returns 0 (a normal exit: the supervisor's `canceled` report then ends the operation `Canceled`, TC-SV-150 (c));
//   - (c) `Sleep` was called with 300 s and returned the ctx error at once; the record is present at exit; `runReboot` returns 143; no `result.json`;
//   - (d) `Sleep` was called with 300 s and returned the ctx error; the record is present at exit; `runReboot` returns 143 at once; no `result.json` (in particular no `reboot-did-not-happen{stage: wait}`).
func TestTCSV109_DF5ACancelThatLandsAfter(t *testing.T) {
	t.Skip("TC-SV-109: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-110: A trigger that fails retracts the record and ends reboot-did-not-happen at stage trigger. Owner WI 4.4. Requirements: FR-11.
//
// - Setup: F-HELPER's trigger returns exit 1 with 5 KiB of output starting `Failed to reboot system via logind: Access denied` in one subtest, and `ErrNotStarted` (ENOENT) in another.
// - Steps:
//  1. Run the helper, then Finalize.
//
// - Expected:
//   - the record is absent;
//   - `result.json` holds `reboot-did-not-happen` with `stage: trigger` and `exitCode: 1`, and the output quoted ≤ 2 KiB (its prefix equals the first 2 KiB);
//   - the not-started subtest has the tool error and `exitCode` null;
//   - Finalize gives Failed with that problem, by a normal exit (R5).
func TestTCSV110_ATriggerThatFailsRetractsTheRecord(t *testing.T) {
	t.Skip("TC-SV-110: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-111: A trigger that returns but never takes the host down ends reboot-did-not-happen at stage wait. Owner WI 4.4. Requirements: FR-11.
//
// - Setup: F-HELPER's trigger returns exit 0, and nothing cancels the ctx. `Sleep` returns at once and records durations.
// - Steps:
//  1. Run the helper with `timeoutSeconds: 300`.
//
// - Expected:
//   - `Sleep` was called with 10 s (the delay) and then 300 s;
//   - the record is absent;
//   - `result.json` holds `reboot-did-not-happen` with `stage: wait` and `bootId: CUR`;
//   - exit 0 (a normal exit).
func TestTCSV111_ATriggerThatReturnsButNeverTakes(t *testing.T) {
	t.Skip("TC-SV-111: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-112: The trigger is RebootArgv through svcctl's runner. Owner WI 4.4. Requirements: FR-10, FR-12, CON-2.
//
// - Setup: F-HELPER `Run` recording argv; init systemd, then OpenRC.
// - Steps:
//  1. Run the helper (as TC-SV-109).
//
// - Expected:
//   - the trigger argv is exactly `[/usr/bin/systemctl reboot]` (systemd) or `[/sbin/reboot]` (OpenRC);
//   - it went through the `svcctl.Run` seam (its fixed environment is asserted by TC-SV-63's runner contract);
//   - no argv contains `-f`, `--force`, `poweroff`, `halt`, `kexec`, `soft-reboot`, `pct` or `qm`.
func TestTCSV112_TheTriggerIsRebootArgvThroughSvcctlsRunner(t *testing.T) {
	t.Skip("TC-SV-112: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-113: AfterReboot builds the result from Rebooted, the new kernel and the reboot-required state. Owner WI 4.4. Requirements: FR-11, A-41.
//
// - Setup: `input.json` from TC-SV-106 (kernel `7.0.0-42-generic`, reboot required true). One F-ROOTS tree `root` holds `proc/sys/kernel/osrelease` `7.0.0-43-generic`, no `var/run/reboot-required`, and `lib/modules/7.0.0-43-generic/`. The test sets both seams from it: `ProcRoot = root/proc` and `HostFS = os.DirFS(root)`, so the provider's kernel-release read and the host rule's own `proc/sys/kernel/osrelease` read (for its `lib/modules/<release>` check) see the same kernel (A-41). `Rebooted{PREV, CUR, T0, T1}`.
// - Steps:
//  1. `AfterReboot(ctx, in, rb, run)`.
//
// - Expected: `RebootResult` with:
//
//   - `changed: true`, `previousBootId: PREV`, `bootId: CUR`, `rebootRequestedAt: T0`, `upAt: T1`;
//
//   - `previousKernelRelease: 7.0.0-42-generic`, `kernelRelease: 7.0.0-43-generic`;
//
//   - `rebootRequired.state: "false"`.
//
//     It is idempotent: a second call returns an equal value. It runs no exec.
func TestTCSV113_AfterRebootBuildsTheResultFromRebootedThe(t *testing.T) {
	t.Skip("TC-SV-113: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-114: End to end through the real manager: the record is held through shutdown and the op is Succeeded in the next boot with both boot IDs. Owner WI 4.4. Requirements: FR-10, FR-11, FR-16, NFR-7, CON-4, CON-5.
//
// - Setup:
//   - The real Dispatcher, store, audit log and `ops.Manager` (F-BOOT `PREV`) over a temp state directory;
//   - the services provider with F-HOSTS, F-ROOTS and F-DAEMON passing;
//   - `UseOperations(mgr)` and `UseOpsDir`;
//   - the helper run by the local detach backend through a test executable that calls `services.Helper` with F-HELPER seams. Its trigger writes `<dir>/trigger-called` with the time and returns exit 0 at once, as the real `systemctl reboot` does once the job is queued (A-6). Its `Sleep` returns at once for the delay and blocks until its ctx is done for the post-trigger timeout, so the helper is in the S5 wait when the shutdown arrives. It never blocks in the trigger: a trigger that waits to be killed would hold the helper under `WithoutCancel` (S4) until the supervisor's SIGKILL after `Grace`.
//
// - Steps:
//  1. POST the reboot `{confirmHost: h1, delaySeconds: 5}` and record the 202 time.
//  2. Wait for `trigger-called`.
//  3. SIGTERM the supervisor (the shutdown), and wait for its `exit.json`, at most 2 s.
//  4. Shut the manager down.
//  5. Start a new manager and Dispatcher with F-BOOT `CUR`, and recover.
//  6. GET the operation.
//
// - Expected:
//   - the 202 arrived before the trigger, and the admission audit record and the Running operation record were durable before it (NFR-7 ordering);
//   - the record (`PREV`) was written before the trigger;
//   - after step 3 the helper exited 143 within 1 s of the SIGTERM, well before `Grace` (40 s): `exit.json` reports `interrupted` with the helper's own exit, not a SIGKILL, and the record is still present (S5);
//   - after step 3 the op is Running (R3);
//   - after step 5 it is Succeeded, with `previousBootId: PREV`, `bootId: CUR` and `upAt` = the new manager's recovery start;
//   - the terminal audit record carries `previousBootId=PREV` and `bootId=CUR`;
//   - the op never reported `outcome-unknown` or `Canceled`.
func TestTCSV114_EndToEndThroughTheRealManager(t *testing.T) {
	t.Skip("TC-SV-114: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-116: The reboot lane refuses a second reboot. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: TC-SV-114's stack, with the first reboot in its delay (`delaySeconds: 300`).
// - Steps:
//  1. POST a second reboot with `force: true`.
//
// - Expected:
//   - 409 `conflict` with `blockingOperationId` = the first op. `force` skips the operations guard, so the lane is what refuses. No second operation exists in the store;
//   - the binding declares the lane and `LaneReject` (the lane check TC-SV-32 dropped, Revision 1): the reboot binding's `DetachedFuncs.Lane` on a decoded reboot input returns `provider.Lane{Key: "reboot", Policy: provider.LaneReject}`.
func TestTCSV116_TheRebootLaneRefusesASecondReboot(t *testing.T) {
	t.Skip("TC-SV-116: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-150: Subprocess, real trigger process: a cancel while the trigger runs never kills it (S4, A-36). Owner WI 4.4. Requirements: FR-10, FR-11, CON-5, A-28, A-36, A-37.
//
// - Setup:
//   - The helper runs as a subprocess under `detachtest.Local()`'s real `op-supervise` (the TC-SV-ERR30 helper-process harness, `delaySeconds: 5`, `Sleep` returning at once for the delay and blocking until its ctx is done for the post-trigger timeout). Its spec has the production `Grace` (40 s, TC-SV-106).
//   - The `Run` seam calls the real `svcctl.Run` with the ctx it was given, but with a real child in place of the reboot argv: `/bin/sh -c 'touch <dir>/trigger-started; sleep 1; touch <dir>/trigger-done'`. `svcctl.Run` starts it in its own process group (`internal/svcctl/svcctl.go:207-209`), exactly as it would start `systemctl reboot`.
//   - Three subtests:
//   - (a) the child exits 0;
//   - (b) the child ends `touch <dir>/trigger-done; exit 1` after the sleep;
//   - (c) the (b) child through TC-SV-114's stack (the real Dispatcher, store, audit log and `ops.Manager` with F-BOOT `CUR`, the helper run by the local detach backend).
//
// - Steps:
//  1. Start the op (in (c): POST the reboot `{confirmHost: h1, delaySeconds: 5}`), and wait for `trigger-started`, at most 5 s.
//  2. Call the backend's `Launcher.Cancel` for the op's unit: the cancel-request file, then a real SIGTERM that the supervisor forwards to the helper's process group. Record the time. In (c) the test calls `Launcher.Cancel` directly instead of `POST /cancel`: the record exists while the trigger runs, so R2 would answer 409, and this call stands in for an R2 that read no record just before the helper wrote it (DF-5 bullet 4).
//  3. Wait for the supervisor's `exit.json` (in (c): for the operation to be terminal), at most 5 s.
//
// - Expected:
//
//   - in every subtest `trigger-done` exists: the child was reached neither by the group SIGTERM nor by `cmd.Cancel` (A-36), and the helper's exit is after `trigger-done`'s mtime;
//
//   - the helper ended by its own exit, not by the supervisor's SIGKILL, within 3 s of step 2 (the child's 1 s plus milliseconds, far below `Grace`, A-37);
//
//   - (a) `ReadReboot` gives `ok == true` with `BootID == CUR` (the record is kept); the helper's exit status is 143; `exit.json` reports `canceled`; no `result.json`;
//
//   - (b) `ReadReboot` gives `ok == false` (retracted); `result.json` holds `reboot-did-not-happen` with `stage: trigger` and `exitCode: 1`; the helper's exit status is 0; `exit.json` reports `canceled`;
//
//   - (c) the operation ends `Canceled` within 5 s of step 2, never held and never `outcome-unknown`; `reboot-requested` is absent; `mgr.Active()` no longer lists it, and a second reboot `{confirmHost: h1, whatIf: true}` is admitted (the `reboot` lane is free).
//
//     On a helper that runs the trigger under the stop context instead of `context.WithoutCancel`, the stop fires `cmd.Cancel` (SIGKILL to the child's group), `trigger-done` is missing and (a) fails: this is A-36's falsifying test.
func TestTCSV150_ARealTriggerProcessIsNeverKilledByA(t *testing.T) {
	t.Skip("TC-SV-150: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-E16: LostDetail names the service and action, and for the reboot the boot-ID comparison. Owner WI 4.4. Requirements: FR-7, FR-11.
//
// - Setup: Inputs for `restart nx-svc-test` and for a reboot.
// - Steps:
//  1. Call each binding's `LostDetail`.
//
// - Expected:
//   - the control detail names `nx-svc-test` and `restart`;
//   - the reboot detail contains `the host may or may not have rebooted` and `/proc/sys/kernel/random/boot_id` and `previousBootId`.
func TestTCSVE16_LostDetailNamesTheServiceAndActionAnd(t *testing.T) {
	t.Skip("TC-SV-E16: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-E17: A guard that passes at admission but fails in Prepare fails the operation before any trigger. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: F-OPS is empty at admission, and returns another op `01Y` (not the reboot's own ID) by the time Prepare runs.
// - Steps:
//  1. Admit, then run Prepare.
//
// - Expected: Prepare returns `operations-running` with `operationIds: ["01Y"]`. The op ends Failed with that slug, no `input.json` is written, and the helper is never launched.
func TestTCSVE17_AGuardThatPassesAtAdmissionBut(t *testing.T) {
	t.Skip("TC-SV-E17: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-E18: confirmHost differing only by case or surrounding space is refused. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: Host `home-server`.
// - Steps:
//  1. Admit with `Home-server`, `home-server ` and `home-server.lan`.
//
// - Expected: Each answers 400 `validation-failed`, with the field `confirmHost`, and creates nothing.
func TestTCSVE18_ConfirmHostDifferingOnlyByCaseOrSurrounding(t *testing.T) {
	t.Skip("TC-SV-E18: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-E20: delaySeconds defaults to 10 and accepts its bounds. Owner WI 4.4. Requirements: FR-10, NFR-7.
//
// - Setup: As TC-SV-102, with guards passing.
// - Steps:
//  1. Admit with `delaySeconds` absent, 5 and 300.
//
// - Expected: 202 each time, and Prepare's `input.json` has `delaySeconds` 10, 5 and 300.
func TestTCSVE20_DelaySecondsDefaultsTo10AndAcceptsIts(t *testing.T) {
	t.Skip("TC-SV-E20: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-ERR25: Each reboot guard's refusal has its slug and extensions, creates nothing and is audited refused. Owner WI 4.4. Requirements: FR-10, FR-16, A-15.
//
// - Setup: TC-SV-102's stack, one failing guard per subtest.
// - Steps:
//  1. POST.
//
// - Expected: The exact status, slug and extensions of TC-SV-102 per guard. For every refusal:
//   - the store holds no reboot op;
//   - no op directory exists;
//   - the audit log has `refused` with the slug and the facts `confirmHost` and `force`;
//   - F-HELPER's trigger is never called.
func TestTCSVERR25_EachRebootGuardsRefusalHasItsSlug(t *testing.T) {
	t.Skip("TC-SV-ERR25: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-ERR26: SIGTERM during the delay exits 143 with no record and no trigger. Owner WI 4.4. Requirements: FR-10, FR-11.
//
// - Setup: The test binary re-executed as the helper (`os.Args[0]` with an env switch, the nexusd helper-test precedent), with `delaySeconds: 30` and real `Sleep`. The trigger seam is a recorder writing a marker file.
// - Steps:
//  1. Start the process, wait 300 ms, and send SIGTERM.
//
// - Expected: Exit status 143 within 1 s, no `reboot-requested` in the directory, and no trigger marker.
func TestTCSVERR26_SIGTERMDuringTheDelayExits143With(t *testing.T) {
	t.Skip("TC-SV-ERR26: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-ERR27: The reboot answers 503 capability-unavailable when the provider is unavailable. Owner WI 4.4. Requirements: FR-3, FR-12, CON-6.
//
// - Setup: The TC-SV-ERR17 Proxmox variant.
// - Steps:
//  1. POST the reboot, and `start nx-svc-test`.
//
// - Expected: Both answer 503 `capability-unavailable`, with the Proxmox reason. No exec and no audit `requested` record.
func TestTCSVERR27_TheRebootAnswers503CapabilityUnavailableWhen(t *testing.T) {
	t.Skip("TC-SV-ERR27: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-ERR29: Out-of-range delaySeconds is refused with 400. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: As TC-SV-102.
// - Steps:
//  1. POST with `delaySeconds` 4, 301 and -1.
//
// - Expected: 400 `validation-failed`, naming `delaySeconds`, with no guard evaluated (F-OPS and F-DAEMON call counts 0).
func TestTCSVERR29_OutOfRangeDelaySecondsIsRefusedWith(t *testing.T) {
	t.Skip("TC-SV-ERR29: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-ERR30: A real cancel (request file plus SIGTERM) before the second check retracts the record and never triggers, and one after it lets the trigger run once. Owner WI 4.4. Requirements: FR-10, FR-11, CON-5, A-28, A-37.
//
// - Setup: The TC-SV-ERR26 subprocess (the test binary re-executed as the helper, real signal handling, `delaySeconds: 5`, `Sleep` returning at once). Its test-only hook, selected by an env switch, writes `<dir>/hook-<stage>` at the chosen stage and then polls every 10 ms for `<dir>/hook-<stage>.release` (10 s bound). The trigger seam writes `<dir>/trigger-called` and returns exit 0 at once; the post-trigger `Sleep` blocks until its ctx is done. One subtest per stage: `after-check-1` and `after-record` (both S2: step 4 sees the cancel) and `after-check-2` (S4: no check remains before the trigger).
// - Steps:
//  1. Start the process and wait for `hook-<stage>`.
//  2. Write the cancel-request file with `detach.RequestCancel(dir)`, then send the process a real SIGTERM, in that order (as `Launcher.Cancel` does).
//  3. Wait 200 ms, then write `hook-<stage>.release`.
//
// - Expected:
//
//   - the process is still alive at step 3 (the SIGTERM was handled, not fatal);
//
//   - it exits within 1 s of the release with status 143 (the TC-SV-ERR26 convention); under a supervisor that end is reported `canceled`, because the request file is present (TC-SV-ERR31 checks that through the real backend);
//
//   - `after-check-1` and `after-record`: `ReadReboot` gives `ok == false` at exit (the record was written at step 3 and retracted at step 4), and there is no `trigger-called`;
//
//   - `after-check-2`: `trigger-called` exists and was written exactly once, and `ReadReboot` gives `ok == true` with `BootID == CUR` at exit (the record is kept, A-28); the exit with 143 follows the trigger's return at once (step 6 sees the stop);
//
//   - the measured time from SIGTERM to exit is logged; with TC-SV-150's trigger-bound exit it is the evidence for the margin in `Grace` (TC-SV-106, A-37).
//
//     On a design with no SIGTERM handler after the delay, the default SIGTERM action kills the helper in the hook, so in the `after-record` subtest the record is left behind and this case fails: it is the regression test for review Finding 1.
func TestTCSVERR30_ARealCancelRequestFilePlusSIGTERM(t *testing.T) {
	t.Skip("TC-SV-ERR30: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-ERR31: Through the real manager, a cancel accepted before the record whose SIGTERM lands after it ends Canceled, not held. Owner WI 4.4. Requirements: FR-10, FR-11, CON-4, CON-5.
//
// - Setup:
//   - TC-SV-114's stack (the real Dispatcher, store, audit log and `ops.Manager` with F-BOOT `CUR`; the helper run as a subprocess under the local detach backend), with `delaySeconds: 5` and `Sleep` returning at once;
//   - the TC-SV-ERR30 hook at `after-check-1` and `after-record`, and the `trigger-called` marker;
//   - F-UNIT-END's delayed-signal wrapper on the backend's `Signal`: it returns at once and forwards the same SIGTERM to the real supervisor only after `hook-after-record` exists, then writes `<dir>/sigterm-forwarded`. It never drops, adds or changes a signal; it only reproduces the real ordering in which R2 ran before `RecordReboot` and the SIGTERM arrived after it.
//
// - Steps:
//  1. POST the reboot `{confirmHost: h1, delaySeconds: 5}` and wait for `hook-after-check-1`.
//  2. `POST /v1/operations/<id>/cancel` (R2 finds no record).
//  3. Wait for the cancel-request file, then write `hook-after-check-1.release`.
//  4. Wait for `sigterm-forwarded`, wait 200 ms, then write `hook-after-record.release`.
//  5. Wait for terminal, at most 5 s after step 4.
//
// - Expected:
//   - step 2 is accepted (not 409);
//   - the operation ends `Canceled` within 5 s, never `outcome-unknown`, and was never held to `MaxRuntime`;
//   - `reboot-requested` is absent at the end, and there is no `trigger-called`;
//   - `mgr.Active()` no longer lists it, and a second reboot `{confirmHost: h1, whatIf: true}` is admitted (the `reboot` lane is free);
//   - the audit log has `requested` then the cancel records, and no terminal record names a reboot.
func TestTCSVERR31_ThroughTheRealManagerACancelAccepted(t *testing.T) {
	t.Skip("TC-SV-ERR31: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-SV-ERR32: SIGTERM with no cancel request after the record leaves the record, and triggers only after the second check. Owner WI 4.4. Requirements: FR-10, FR-11.
//
// - Setup: The TC-SV-ERR30 subprocess, one subtest per stage: `after-record` (S3) and `after-check-2` (S4). No cancel-request file is written: this is an operator's `systemctl stop` of the op unit, or a shutdown that came from elsewhere (D-3 S3 and S4).
// - Steps:
//  1. Start the process and wait for `hook-<stage>`.
//  2. Send a real SIGTERM, wait 200 ms, and write `hook-<stage>.release`.
//
// - Expected:
//   - the process exits within 1 s of the release with status 143; under a supervisor that end is reported `interrupted`, because there is no request file;
//   - in both subtests `ReadReboot` gives `ok == true` with `BootID == CUR` (the record is left, so the manager's R3 holds and R4 completes if the host does reboot: TC-SV-12, TC-SV-14, TC-SV-8);
//   - `after-record`: no `trigger-called` (S3);
//   - `after-check-2`: `trigger-called` exists and was written exactly once (S4: the trigger runs, and the stop is acted on after it returns).
func TestTCSVERR32_SIGTERMWithNoCancelRequestBetweenThe(t *testing.T) {
	t.Skip("TC-SV-ERR32: SKIP-only stub; un-skip when WI 4.4 lands")
}
