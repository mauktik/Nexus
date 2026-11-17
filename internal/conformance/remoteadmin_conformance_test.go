// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-39, TC-RA-40, TC-RA-41.
// Owner WIs: 2.4. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package conformance_test

import "testing"

// TC-RA-39: Every token op refuses an unauthenticated call and a nexusd:read-only call. Owner WI 2.4. Requirements: FR-1, FR-2, FR-3, NFR-7, ASM-5.
//
//   - Setup: The conformance server with the real nexusd provider over a temp state dir.
//   - Steps:
//     1. Call each of the three token ops with no token.
//     2. Call each with a `nexusd:read` token.
//   - Expected:
//   - Step 1: 401 and no handler call. The store sha is unchanged.
//   - Step 2: 403 with the extension `requiredScope: nexusd:admin`. A `denied` audit record and no `requested` record.
func TestTCRA39_EveryTokenOpRefusesAnUnauthenticatedCallAndA(t *testing.T) {
	t.Skip("TC-RA-39: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-RA-40: The FR-25 negative-input suite walks the token string fields. Owner WI 2.4. Requirements: NFR-5, NFR-7.
//
//   - Setup: `TestAttackStringsAgainstEveryStringParameter`'s walker.
//   - Steps:
//     1. Collect the set of `(op, field)` pairs the walker attacks.
//   - Expected:
//   - The set contains `nexusd.tokens.add/name`, `nexusd.tokens.add/scopes[]`, `nexusd.tokens.add/expires` and `nexusd.tokens.remove/name`.
//   - Every attack answers 422 `validation-failed`, and the store sha is unchanged.
func TestTCRA40_TheFR25NegativeInputSuiteWalksTheTokenString(t *testing.T) {
	t.Skip("TC-RA-40: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-RA-41: The 429 path through the real provider sets Retry-After, writes a refused record and changes nothing. Owner WI 2.4. Requirements: FR-5, FR-13, NFR-7.
//
//   - Setup: The conformance server with the real provider and its real clock, run in under 60 s.
//   - Steps:
//     1. Make 10 successful adds.
//     2. Make the 11th add.
//   - Expected:
//   - 429 with a `Retry-After` integer between 1 and 60 inclusive, `application/problem+json` and slug `rate-limited`.
//   - A `refused` audit record with slug `rate-limited`.
//   - `auth.List` length is unchanged after step 2.
func TestTCRA41_The429PathThroughTheRealProviderSetsRetryAfter(t *testing.T) {
	t.Skip("TC-RA-41: SKIP-only stub; un-skip when WI 2.4 lands")
}
