// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-8, TC-RA-9, TC-RA-10, TC-RA-11, TC-RA-E4, TC-RA-ERR5, TC-RA-ERR6, TC-RA-ERR7, TC-RA-ERR8.
// Owner WIs: 1.3. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package core_test

import "testing"

// TC-RA-8: A SyncMutation success writes requested then completed with facts, links them, and creates no operation. Owner WI 1.3. Requirements: CON-3, FR-1, FR-13, NFR-1.
//
//   - Setup:
//   - A test registry with a planted `SyncMutation` op `tcra.things.add`. Its handler calls `audit.AddFact(ctx, "thingId", "t1")` and returns `{changed:true, secret:"RA-SENTINEL-1"}`.
//   - The real `audit.Log` over a temp dir (`registryWithAudit`).
//   - A store spy that counts every `CreateOp`/`Put` call.
//   - Steps:
//     1. `Invoke` with a token holding `tcra:admin`.
//   - Expected:
//   - The result is 200 sync, and `InvokeResult.Body` decodes to the handler output.
//   - The audit file has exactly two records for the call: `requested`, then `completed`. `completed.RequestID == requested.ID`, and `completed.Facts == {"thingId":"t1"}`.
//   - The store spy count is 0.
func TestTCRA8_ASyncMutationSuccessWritesRequestedThenCompleted(t *testing.T) {
	t.Skip("TC-RA-8: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-RA-9: An Audited read writes requested then completed. Owner WI 1.3. Requirements: FR-13, FR-2, FR-6.
//
//   - Setup: As TC-RA-8, with a planted `Audited` read-only op that adds the fact `count=3`.
//   - Steps:
//     1. `Invoke`.
//   - Expected: 200. The records are `requested` then `completed`, linked, and `completed` carries `count=3`.
func TestTCRA9_AnAuditedReadWritesRequestedThenCompleted(t *testing.T) {
	t.Skip("TC-RA-9: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-RA-10: A read-only op that is not Audited still writes no audit record. Owner WI 1.3. Requirements: FR-13.
//
//   - Setup: As TC-RA-9 with `Audited: false`.
//   - Steps:
//     1. `Invoke`.
//   - Expected: 200, and the audit file's record count is unchanged (0 new records).
func TestTCRA10_AReadOnlyOpThatIsNotAuditedStillWritesNoAudit(t *testing.T) {
	t.Skip("TC-RA-10: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-RA-11: The sync handler output reaches no log line and no store. Owner WI 1.3. Requirements: NFR-1, CON-3.
//
//   - Setup: As TC-RA-8, with an `slog` text handler at Debug level writing to a `syncBuffer` passed to the registry.
//   - Steps:
//     1. `Invoke` 3 times: a success, a handler error whose message is fixed, and a validation refusal.
//   - Expected:
//   - `strings.Count(logs, "RA-SENTINEL-1") == 0`.
//   - The audit file contains 0 occurrences.
//   - The store spy count is 0.
//   - Non-vacuity: the first call's `Body` contains `RA-SENTINEL-1`.
func TestTCRA11_TheSyncHandlerOutputReachesNoLogLineAndNoStore(t *testing.T) {
	t.Skip("TC-RA-11: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-RA-E4: A SyncMutation skips the storage check, and the audit outcome filter accepts completed. Owner WI 1.3. Requirements: CON-3, FR-13.
//
//   - Setup: A test registry whose storage check reports over budget, holding the planted `SyncMutation` op and a planted long-running mutating op.
//   - Steps:
//     1. Invoke both.
//     2. Query the audit log with `outcome=completed` through the system audit list op.
//   - Expected:
//   - The SyncMutation answers 200, as `cancel` does. The long-running op is refused with the existing storage problem.
//   - The filtered list returns the `completed` record, and the filter's description lists `completed`.
func TestTCRAE4_ASyncMutationSkipsTheStorageCheckAndTheAudit(t *testing.T) {
	t.Skip("TC-RA-E4: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-RA-ERR5: A handler error on a SyncMutation writes a refused record linked to its requested record. Owner WI 1.3. Requirements: FR-13, CON-3.
//
//   - Setup: The planted op returns `problem.New(Conflict, "dup")`. A second case uses `system.operations.cancel` on an unknown ID.
//   - Steps:
//     1. Invoke each.
//   - Expected:
//   - The response is 409, and 404 respectively.
//   - The records are `requested` then `refused`, with `refused.RequestID == requested.ID` and slug `conflict`. The existing cancel record gains the link additively.
//   - No `completed` record.
func TestTCRAERR5_AHandlerErrorOnASyncMutationWritesARefusedRecord(t *testing.T) {
	t.Skip("TC-RA-ERR5: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-RA-ERR6: A requested-record write failure refuses with audit-unavailable before the handler runs. Owner WI 1.3. Requirements: FR-13, NFR-1.
//
//   - Setup: An audit writer that fails the next `requested` write (the `failingAudit` pattern), with the planted `SyncMutation` op and the `Audited` op.
//   - Steps:
//     1. Invoke each.
//   - Expected: `audit-unavailable` (503). The handler call counter is 0 for both.
func TestTCRAERR6_ARequestedRecordWriteFailureRefusesWithAudit(t *testing.T) {
	t.Skip("TC-RA-ERR6: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-RA-ERR7: An Idempotency-Key on a SyncMutation is refused and the handler never runs. Owner WI 1.3. Requirements: FR-1, NFR-1, CON-3.
//
//   - Setup: The planted `SyncMutation` op.
//   - Steps:
//     1. Invoke with `IdempotencyKey: "k1"`.
//   - Expected:
//   - 422 `validation-failed`. One field error at path `""` with the message exactly "Idempotency-Key is not supported by this operation: it answers once and is never replayed".
//   - The handler counter is 0.
//   - A `refused` audit record.
//   - A long-running mutating op with the same key still works, as before.
func TestTCRAERR7_AnIdempotencyKeyOnASyncMutationIsRefusedAndThe(t *testing.T) {
	t.Skip("TC-RA-ERR7: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-RA-ERR8: A completed-record write failure keeps the 200 and marks the audit log unhealthy. Owner WI 1.3. Requirements: FR-13.
//
//   - Setup: An audit writer that succeeds on `requested` and fails on `completed`.
//   - Steps:
//     1. Invoke the planted `SyncMutation` op.
//   - Expected:
//   - 200 with the handler body.
//   - An Error-level log line naming the failed record, with no handler output in it.
//   - The audit health reports unhealthy, as for the upload's `stored` record.
func TestTCRAERR8_ACompletedRecordWriteFailureKeepsThe200AndMarks(t *testing.T) {
	t.Skip("TC-RA-ERR8: SKIP-only stub; un-skip when WI 1.3 lands")
}
