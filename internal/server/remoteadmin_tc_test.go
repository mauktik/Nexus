// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-13, TC-RA-14.
// Owner WIs: 1.4. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package server_test

import "testing"

// TC-RA-13: A SecretOutput route answers with Cache-Control no-store, and other routes do not. Owner WI 1.4. Requirements: NFR-1, CON-3.
//
//   - Setup: `httptest` with the real server over a test provider that has the planted `SecretOutput` op (TC-RA-2) and a planted sync read op.
//   - Steps:
//     1. POST the secret op successfully.
//     2. POST it with an invalid body, which gives a 422.
//     3. GET the read op.
//   - Expected:
//   - Steps 1 and 2 both carry `Cache-Control: no-store`, because it is set before any write.
//   - Step 3 has no `Cache-Control: no-store`.
func TestTCRA13_ASecretOutputRouteAnswersWithCacheControlNoStore(t *testing.T) {
	t.Skip("TC-RA-13: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-RA-14: A 429 returned by a sync handler reaches the wire with Retry-After and a problem body. Owner WI 1.4. Requirements: FR-5.
//
//   - Setup: A planted sync mutation whose handler returns `problem.New(RateLimited, "x")` with `RetryAfter = 7300ms`.
//   - Steps:
//     1. POST.
//   - Expected: Status 429, `Retry-After: 8`, `Content-Type: application/problem+json`, a body slug of `rate-limited`, and no `retryAfter` member in the body.
func TestTCRA14_A429ReturnedByASyncHandlerReachesTheWireWith(t *testing.T) {
	t.Skip("TC-RA-14: SKIP-only stub; un-skip when WI 1.4 lands")
}
