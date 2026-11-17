// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-28, TC-RA-29.
// Owner WIs: 2.2. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package conformance_test

import "testing"

// TC-RA-28: The conformance word guard exempts exactly the three token op IDs. Owner WI 2.2. Requirements: FR-10, NFR-7.
//
//   - Setup: The real catalog, plus a planted extra spec with the op `tcra.tokens.list` and the route `/providers/tcra/tokens`.
//   - Steps:
//     1. Run the forbidden-word check over the catalog.
//     2. Run it over the catalog plus the planted spec.
//   - Expected:
//   - `allowedTokenOps` equals exactly the three nexusd token IDs.
//   - Step 1 passes.
//   - Step 2 fails, naming `tcra.tokens.list` and the word `tokens`.
//   - `TestTCSM101` passes unchanged: its pinned phrases are still in the `forbiddenWords` comment, and `create` is still forbidden in nexusd op IDs.
func TestTCRA28_TheConformanceWordGuardExemptsExactlyTheThree(t *testing.T) {
	t.Skip("TC-RA-28: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-RA-29: Generic conformance treats SyncMutation and Audited ops by their declaration. Owner WI 2.2. Requirements: FR-13, CON-3, NFR-7.
//
//   - Setup: `catalogStubs` with `testprovider.Sync` entries for the three token ops.
//   - Steps:
//     1. Run the scope/audit expectations over the token ops.
//   - Expected:
//   - add and remove answer 200 with no `operation` member, and write `requested` then `completed`. They are never 202.
//   - list writes `requested` then `completed`.
//   - Each refuses a `nexusd:read`-only token with 403 and a `denied` record.
//   - `TestScopeAndAuditCoverEveryOp` is green over the full catalog.
func TestTCRA29_GenericConformanceTreatsSyncMutationAndAudited(t *testing.T) {
	t.Skip("TC-RA-29: SKIP-only stub; un-skip when WI 2.2 lands")
}
