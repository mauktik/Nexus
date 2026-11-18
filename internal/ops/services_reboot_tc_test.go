// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-8, TC-SV-9, TC-SV-10, TC-SV-11, TC-SV-12, TC-SV-13, TC-SV-14, TC-SV-15, TC-SV-16, TC-SV-17, TC-SV-18, TC-SV-19, TC-SV-20, TC-SV-E2, TC-SV-E3, TC-SV-E4, TC-SV-E19, TC-SV-ERR6.
// Owner WIs: 1.3. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package ops_test

import "testing"

// TC-SV-8: R4: an interrupted unit with a record from an earlier boot completes Succeeded through AfterReboot. Owner WI 1.3. Requirements: FR-11, FR-16, CON-4, CON-5.
//
// - Setup:
//   - `perBackend`. A world whose `Options.BootID` is F-BOOT `PREV`.
//   - Planted detached provider `rb` with op `rb.actions.reboot` (TC-SV-1 shape, lane `reboot` reject, `MaxRuntime` 1 h). Its Prepare is `shJob("sleep 600")`. Its `AfterReboot` records `Rebooted` and returns `{"rebooted":true}`. Its `Finalize` records calls.
//
// - Steps:
//  1. Start the op and wait for Running.
//  2. `detach.RecordReboot(opDir, {PREV, T0})`.
//  3. Close the world as a shutdown does: `w.close()` first, so `nexusd` stops before the unit (recovery path).
//  4. SIGTERM the supervisor (F-UNIT-END interrupted). Then rewrite `exit.json` raw with `bootId: PREV`, as the supervisor of the earlier boot wrote it.
//  5. Reopen the world with F-BOOT `CUR`, then `recoverNow()` and `waitTerminal`.
//
// - Expected:
//   - Succeeded, with result `{"rebooted":true}`;
//   - `AfterReboot` called exactly once with `Rebooted{PreviousBootID: PREV, BootID: CUR, RequestedAt: T0, ReadyAt: <recovery start>}`;
//   - `Finalize` never called;
//   - the terminal `stateChange` audit record carries the facts `previousBootId=PREV` and `bootId=CUR`;
//   - never `outcome-unknown`.
func TestTCSV8_R4AnInterruptedUnitWithARecord(t *testing.T) {
	t.Skip("TC-SV-8: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-9: R4: a lost unit with a record from an earlier boot completes Succeeded after the recheck. Owner WI 1.3. Requirements: FR-11, CON-4.
//
// - Setup: As TC-SV-8.
// - Steps:
//  1. Start, wait for Running, and record with `PREV`.
//  2. Close the world, then SIGKILL the supervisor (F-UNIT-END lost: no `exit.json`).
//  3. Reopen with `CUR`, and `recoverNow()`.
//
// - Expected: Succeeded through `AfterReboot` after the existing 2 s lost recheck (within 15 s), with the same audit facts as TC-SV-8. `Finalize` is never called.
func TestTCSV9_R4ALostUnitWithARecord(t *testing.T) {
	t.Skip("TC-SV-9: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-10: R4: an AfterReboot error ends the operation Failed with that problem. Owner WI 1.3. Requirements: FR-11.
//
// - Setup: As TC-SV-8, but `AfterReboot` returns `problem.New(problem.ValidationFailed, "planted")` in one subtest, and a plain `errors.New("planted")` in another.
// - Steps:
//  1. The TC-SV-8 sequence.
//
// - Expected:
//   - Failed with slug `validation-failed` and detail `planted` in the first subtest;
//   - in the second, the slug `problemOf` gives for a plain error today (`internal-error`), with the same detail;
//   - the terminal audit record carries both boot IDs.
func TestTCSV10_R4AnAfterRebootErrorEndsTheOperation(t *testing.T) {
	t.Skip("TC-SV-10: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-11: R5: a normal exit with a record present goes to Finalize, never AfterReboot. Owner WI 1.3. Requirements: FR-11, CON-4.
//
// - Setup: As TC-SV-8, but Prepare is `shJob` writing `result.json` and exiting 0.
// - Steps:
//  1. Subtest live: record with `PREV`, release the job, and wait for terminal, all in the same boot.
//  2. Subtest recovery: record with `PREV`, close the world, let the job exit 0 (normal `exit.json`, `bootId: PREV`), reopen with `CUR` and recover.
//
// - Expected: In both subtests, `Finalize` is called exactly once, `AfterReboot` zero times, and the outcome is whatever `Finalize` returned.
func TestTCSV11_R5ANormalExitWithARecord(t *testing.T) {
	t.Skip("TC-SV-11: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-12: R3: an interrupted exit with a record from this boot is held Running and the lane stays held. Owner WI 1.3. Requirements: FR-11, CON-4.
//
// - Setup: As TC-SV-8, all in boot `PREV`, with the world left open (the watcher is alive, as in A-7).
// - Steps:
//  1. Start, wait for Running, and record with `PREV`.
//  2. SIGTERM the supervisor, so the watcher sees `exit.json` `interrupted`.
//  3. Wait 3 × `PollInterval` plus 1 s.
//  4. Start a second `rb.actions.reboot`.
//
// - Expected:
//   - the first op's record is still `Running`, with no terminal commit and no `outcome-unknown`;
//   - `AfterReboot` and `Finalize` are not called;
//   - step 4 answers 409 `conflict` with `blockingOperationId` = the first op (the lane slot is held).
func TestTCSV12_R3AnInterruptedExitWithARecord(t *testing.T) {
	t.Skip("TC-SV-12: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-13: R3: the hold ends at shutdown with the record still Running, and the next boot completes it. Owner WI 1.3. Requirements: FR-11, CON-4.
//
// - Setup: TC-SV-12 after step 3.
// - Steps:
//  1. `w.close()` (manager `Shutdown`).
//  2. Read the store record.
//  3. Reopen with F-BOOT `CUR` and recover.
//
// - Expected: After step 1, the record is `Running` and `Shutdown` returned within its deadline (the hold goroutine exits on `m.halt`, so no goroutine is leaked: `watcherGoroutines()` is back to its baseline). After step 3, Succeeded through `AfterReboot`, with both boot IDs (TC-SV-8's expected).
func TestTCSV13_R3TheHoldEndsAtShutdownWith(t *testing.T) {
	t.Skip("TC-SV-13: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-14: R3: the hold ends outcome-unknown at MaxRuntime when the boot never changes. Owner WI 1.3. Requirements: FR-11, A-14.
//
// - Setup: As TC-SV-12, with the op spec's `MaxRuntime` 2 s.
// - Steps:
//  1. Start, record with `PREV` at requested time `T0`, and SIGTERM the supervisor.
//  2. `waitTerminal` within 2 s + 3 s.
//
// - Expected:
//   - Failed `outcome-unknown`, committed at or after `started + 2s`;
//   - its detail names the request time `T0` (RFC 3339) and the boot ID `PREV`, and says the host did not restart;
//   - `AfterReboot` is not called;
//   - a new `rb.actions.reboot` is then admitted (the lane is released).
func TestTCSV14_R3TheHoldEndsOutcomeUnknownAt(t *testing.T) {
	t.Skip("TC-SV-14: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-15: R3: a canceled exit after the record is held, not Canceled. Owner WI 1.3. Requirements: FR-10, FR-11.
//
// - Setup: As TC-SV-12.
// - Steps:
//  1. Start and wait for Running. Write the cancel-request file in the op directory, as the manager's cancel path does when R2 accepted the cancel before the record existed (the DF-5 race in which the cancel lands only after the helper has started the trigger, A-28).
//  2. Record with `PREV`.
//  3. Stop the unit through the backend's cancel path (the cancel-request file plus SIGTERM), so the supervisor writes `exit.json` with `canceled: true`.
//
// - Expected: The record stays `Running` (held), not `Canceled`. After TC-SV-13's step 3 (new boot `CUR`), it ends Succeeded through `AfterReboot` (A-28).
func TestTCSV15_R3ACanceledExitAfterTheRecord(t *testing.T) {
	t.Skip("TC-SV-15: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-16: Recovery in the same boot holds a recorded operation outside the classification deadline. Owner WI 1.3. Requirements: FR-11, CON-4.
//
// - Setup:
//   - Boot `PREV` throughout.
//   - Two Running ops: `rb` with a record (`PREV`) whose supervisor was SIGKILLed (lost), and a plain `td` op whose supervisor was also SIGKILLed.
//   - `MaxRuntime` of `rb` is 4 s from its start.
//
// - Steps:
//  1. Close and reopen the world (a `nexusd` restart, same boot), then `recoverNow()`.
//  2. Wait for the recovery channel to close.
//
// - Expected:
//   - the recovery channel closes within 15 s, while `rb` is still Running (the hold runs in its own goroutine);
//   - the `td` op ends `outcome-unknown` as today;
//   - `rb` ends `outcome-unknown` at `start + 4s` (the remaining time, not a fresh 4 s), with the TC-SV-14 detail.
func TestTCSV16_RecoveryInTheSameBootHoldsA(t *testing.T) {
	t.Skip("TC-SV-16: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-17: Without a record, a declaring operation's interrupted and lost ends stay outcome-unknown. Owner WI 1.3. Requirements: FR-11, CON-4.
//
// - Setup: `rb` as TC-SV-8, never recording.
// - Steps:
//  1. Run four subtests: {interrupted, lost} × {same boot live, next boot recovery}.
//
// - Expected: Each subtest ends Failed `outcome-unknown`, with today's detail text (compared with the same scenario run on the `td` op), and calls neither `AfterReboot` nor `Finalize`.
func TestTCSV17_WithoutARecordADeclaringOperationsInterrupted(t *testing.T) {
	t.Skip("TC-SV-17: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-18: An operation that does not declare the flag ignores a stray record file. Owner WI 1.3. Requirements: CON-4.
//
// - Setup: The existing `td` op (no flag), with `detach.RecordReboot(opDir, {PREV,…})` written while it runs.
// - Steps:
//  1. Run the TC-SV-17 subtests on `td`, and a cancel of `td` after the record.
//
// - Expected: The outcomes and details are byte-identical to the same scenarios without the file: `outcome-unknown`, or `Canceled` for the cancel. Cancel is not refused `not-cancelable`.
func TestTCSV18_AnOperationThatDoesNotDeclareThe(t *testing.T) {
	t.Skip("TC-SV-18: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-19: R2: cancel answers 409 not-cancelable once the record exists, and cancels normally before. Owner WI 1.3. Requirements: FR-10, FR-11.
//
// - Setup: `rb` as TC-SV-8. A principal holding the op's scope.
// - Steps:
//  1. Op A: Running, no record; `w.cancel`.
//  2. Op B: Running, record at `T0`; `w.cancel`.
//  3. Op C: Running, with a `reboot-requested` file that cannot be read (`{"bootId":`, truncated, as TC-SV-ERR4); `w.cancel`.
//
// - Expected:
//   - A ends `Canceled`;
//   - B's cancel answers 409 `not-cancelable`, with extension `operationId` = B and a detail containing `the reboot was already requested at` followed by `T0` in RFC 3339;
//   - B stays `Running`, and its unit is not signaled (`unitSpy.signaled(B)` is empty);
//   - C's cancel also answers 409 `not-cancelable` (R2 fails closed on a read error, review Finding 10), with `operationId` = C and a detail containing `the reboot record cannot be read; the reboot may have been requested`; C stays `Running`, `unitSpy.signaled(C)` is empty, and no cancel-request file exists in C's directory;
//   - each refusal is audited `refused` with slug `not-cancelable`.
func TestTCSV19_R2CancelAnswers409NotCancelableOnce(t *testing.T) {
	t.Skip("TC-SV-19: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-20: Manager.Active lists exactly the Queued and Running operations. Owner WI 1.3. Requirements: FR-10.
//
// - Setup: Seeded records: two Running, one Queued (blocked in a lane behind a gated op), one each of Succeeded, Failed and Canceled.
// - Steps:
//  1. `mgr.Active()`.
//
// - Expected: Exactly the three non-terminal operations, each with its `ID`, `Op` and `Status` (`Queued` or `Running`). The error is nil. The terminal ones are absent.
func TestTCSV20_ManagerActiveListsExactlyTheQueuedAnd(t *testing.T) {
	t.Skip("TC-SV-20: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-E2: readyAt is the time recovery began, not the commit time. Owner WI 1.3. Requirements: FR-11.
//
// - Setup: TC-SV-8, with the world's clock set to `T1` before `recoverNow`, and the clock advanced to `T1+10s` from inside `AfterReboot`.
// - Steps:
//  1. Recover.
//
// - Expected: `Rebooted.ReadyAt == T1`, and the result's `upAt` (when the hook copies it) is `T1`, not `T1+10s`.
func TestTCSVE2_ReadyAtIsTheTimeRecoveryBeganNot(t *testing.T) {
	t.Skip("TC-SV-E2: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-E3: A cancel accepted after the record ends Succeeded after the reboot. Owner WI 1.3. Requirements: FR-11, A-28.
//
// - Setup: `rb` as TC-SV-8.
// - Steps:
//  1. Start and wait for Running. A cancel is accepted (no record yet), so the manager writes the cancel request.
//  2. The record is written immediately after (the helper had passed its second check).
//  3. Interrupt the unit, close, and reopen with `CUR`.
//
// - Expected: Step 1 answered the cancel as accepted, and the op is Succeeded via `AfterReboot` with both boot IDs. The result or audit records that a cancel was requested (the cancel record exists), so the outcome is explainable.
func TestTCSVE3_ACancelAcceptedAfterTheRecordEnds(t *testing.T) {
	t.Skip("TC-SV-E3: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-E4: The lane is released only when the held operation commits. Owner WI 1.3. Requirements: FR-10, FR-11.
//
// - Setup: TC-SV-14 (`MaxRuntime` 2 s).
// - Steps:
//  1. Start a second `rb` op every 200 ms until one is admitted.
//
// - Expected: Every attempt before the first op's terminal commit answers 409 `conflict`, and the first attempt after it is admitted. There is no window in which both are non-terminal.
func TestTCSVE4_TheLaneIsReleasedOnlyWhenThe(t *testing.T) {
	t.Skip("TC-SV-E4: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-E19: A record whose boot ID cannot be read ends outcome-unknown with the read error, never Succeeded. Owner WI 1.3. Requirements: FR-11.
//
// - Setup: TC-SV-8, but the `reboot-requested` file contains `{"bootId":` (truncated). Two subtests:
//   - (R4) the unit ended interrupted in boot `PREV`; recover in boot `CUR`;
//   - (R3) the live watcher sees the unit end interrupted in the same boot (TC-SV-12's setup with the corrupt file), with `MaxRuntime` 2 s.
//
// - Steps:
//  1. Recover (R4), or let the watcher observe the exit (R3).
//
// - Expected: In both subtests:
//   - Failed `outcome-unknown` at once, with a detail naming `reboot-requested` and the read error (review Finding 10);
//   - in R3, the operation is terminal within 500 ms of the exit, well before `MaxRuntime` (no hold), and the lane is released;
//   - `AfterReboot` is not called;
//   - never Succeeded.
func TestTCSVE19_ARecordWhoseBootIDCannotBe(t *testing.T) {
	t.Skip("TC-SV-E19: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-ERR6: R1: a declaring op is not launched when the boot ID is unknown. Owner WI 1.3. Requirements: FR-11.
//
// - Setup: `spyWorld` with F-BOOT returning `("", errors.New("planted"))`.
// - Steps:
//  1. Start `rb.actions.reboot`.
//  2. Start the plain `td` op.
//
// - Expected:
//   - (1) Failed `internal-error`, with the detail `the boot ID cannot be read, so a reboot could not be confirmed`, and `unitSpy.launched(id) == 0`;
//   - (2) launches and runs normally.
func TestTCSVERR6_R1ADeclaringOpIsNotLaunched(t *testing.T) {
	t.Skip("TC-SV-ERR6: SKIP-only stub; un-skip when WI 1.3 lands")
}
