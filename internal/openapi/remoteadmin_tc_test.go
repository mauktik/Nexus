// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-15, TC-RA-113.
// Owner WIs: 1.4. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package openapi_test

import "testing"

// TC-RA-15: OpenAPI documents SyncMutation ops without Idempotency-Key, with 409, and a SecretOutput 200 with Cache-Control. Owner WI 1.4. Requirements: CON-3, FR-14, NFR-1.
//
//   - Setup: Build the document for a spec set holding the planted `SyncMutation`+`SecretOutput` op and a planted long-running mutating op.
//   - Steps:
//     1. Inspect both operations.
//   - Expected:
//   - The sync mutation has no `Idempotency-Key` parameter, its responses include `409` (`conflict`), and its `200` declares a `Cache-Control` header.
//   - The long-running op still has the `Idempotency-Key` parameter.
//   - `x-` metadata is unchanged from a document built without the new flags, apart from the added response members.
func TestTCRA15_OpenAPIDocumentsSyncMutationOpsWithout(t *testing.T) {
	t.Skip("TC-RA-15: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-RA-113: OpenAPI documents both 200 and 202 for a CompletesAtAdmission op. Owner WI 1.4. Requirements: FR-14, CON-3, FR-7.
//
//   - Setup: Build the document for a spec set holding TC-RA-110's op C and a planted long-running mutating op without the flag.
//   - Steps:
//     1. Inspect both operations.
//   - Expected:
//   - C's responses include `200`, whose schema is C's output schema, and `202`. C keeps the `Idempotency-Key` parameter (long-running mutating ops keep it).
//   - The op without the flag has `202` and no `200`.
//   - `x-` metadata is unchanged apart from the added response member.
func TestTCRA113_OpenAPIDocumentsBoth200And202ForACompletesAt(t *testing.T) {
	t.Skip("TC-RA-113: SKIP-only stub; un-skip when WI 1.4 lands")
}
