// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-23, TC-SV-24, TC-SV-25, TC-SV-E5, TC-SV-ERR7.
// Owner WIs: 1.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package core

import "testing"

// TC-SV-23: An Audited stream that ends without error writes requested then completed, linked. Owner WI 1.4. Requirements: FR-6, FR-16, CON-4.
//
// - Setup:
//   - `auditFixture` with a planted `ModeStream`, `RiskReadOnly`, `Audited` op `pa.things.logs` (input `{name}`).
//   - Its `BindStream` handler emits three frames, adds the fact `service=x` with `audit.AddFact`, and returns nil.
//
// - Steps:
//  1. Invoke it as principal `tok1`.
//  2. Read `records(t, lg)`.
//
// - Expected:
//   - the three frames reach the caller;
//   - exactly two records, in order: `requested` (written before the first frame) and `completed`;
//   - `completed` names the `requested` record's ID;
//   - both carry op `pa.things.logs` and token `tok1`, and `completed` carries `service=x`.
func TestTCSV23_AnAuditedStreamThatEndsWithoutError(t *testing.T) {
	t.Skip("TC-SV-23: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-SV-24: An Audited stream that fails writes requested then refused with the sanitized slug. Owner WI 1.4. Requirements: FR-6, FR-16.
//
// - Setup: As TC-SV-23. The handler emits one frame and then returns `problem.New(problem.DependencyUnavailable, "journalctl: planted /secret/path")` in one subtest, and a plain error in another.
// - Steps:
//  1. Invoke it.
//
// - Expected:
//   - two records: `requested`, then `refused`;
//   - `refused` names `requested`, with slug `dependency-unavailable` in the first subtest and `internal-error` in the second;
//   - the audit record carries the sanitized problem, exactly what the client receives, never more.
func TestTCSV24_AnAuditedStreamThatFailsWritesRequested(t *testing.T) {
	t.Skip("TC-SV-24: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-SV-25: An unaudited stream still writes no audit record. Owner WI 1.4. Requirements: FR-16, CON-4.
//
// - Setup: The same planted op without `Audited`. Separately, the real `docker.containers.logs` op definition bound to a stub stream handler.
// - Steps:
//  1. Invoke each, once successfully and once failing.
//
// - Expected: Zero audit records in every case.
func TestTCSV25_AnUnauditedStreamStillWritesNoAudit(t *testing.T) {
	t.Skip("TC-SV-25: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-SV-E5: An Audited stream ended by a client disconnect has exactly one terminal record. Owner WI 1.4. Requirements: FR-16.
//
// - Setup: TC-SV-23's op, with a handler that emits frames until `ctx` is done. The real server runs over `httptest`.
// - Steps:
//  1. Start the request, read one frame, and close the client connection.
//  2. Wait 2 s.
//
// - Expected: The audit log holds `requested` and exactly one of `completed` or `refused` naming it, never zero and never two (test-strategy Open Item 5).
func TestTCSVE5_AnAuditedStreamEndedByAClient(t *testing.T) {
	t.Skip("TC-SV-E5: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-SV-ERR7: A failed requested-record write refuses the audited stream before the handler. Owner WI 1.4. Requirements: FR-6, FR-16.
//
// - Setup: TC-SV-23's op, with the audit writer failing (`faultyAudit`).
// - Steps:
//  1. Invoke it.
//
// - Expected: 503 `audit-unavailable`. The handler call count is 0, no frame is sent, and no record is written.
func TestTCSVERR7_AFailedRequestedRecordWriteRefusesThe(t *testing.T) {
	t.Skip("TC-SV-ERR7: SKIP-only stub; un-skip when WI 1.4 lands")
}
