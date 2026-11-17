// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-5, TC-RA-6, TC-RA-7, TC-RA-E3.
// Owner WIs: 1.2. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package problem

import "testing"

// TC-RA-5: The four new problem types and the extended config-apply-failed are registered as declared. Owner WI 1.2. Requirements: FR-5, FR-8, FR-9, FR-12, FR-11.
//
//   - Setup: The architecture's problem table, as a test table.
//   - Steps:
//     1. `problem.Lookup(slug)` for `config-set-unavailable`, `config-conflict`, `config-restore-failed`, `token-limit-reached` and `config-apply-failed`.
//     2. Check membership in `problem.OperationLevel()`.
//   - Expected:
//   - `config-set-unavailable`: 503, exit 4, extensions `provider` and `reason`, not OperationLevel.
//   - `config-conflict`: OperationLevel, exit 1.
//   - `config-restore-failed`: OperationLevel, exit 1, extension `backupPath`.
//   - `token-limit-reached`: 409, exit 1, extension `maxTokens`, not OperationLevel.
//   - `config-apply-failed`: its extensions now include `restored` and `backupPath`, and every previous extension is kept.
//   - `rate-limited` stays 429 with exit 3.
//   - `wantOperationLevel` and the registry count are updated.
func TestTCRA5_TheFourNewProblemTypesAndTheExtendedConfigApply(t *testing.T) {
	t.Skip("TC-RA-5: SKIP-only stub; un-skip when WI 1.2 lands")
}

// TC-RA-6: New types and extensions round-trip through MarshalJSON and UnmarshalJSON. Owner WI 1.2. Requirements: FR-8, FR-9.
//
//   - Setup: `problem.New(ConfigApplyFailed, "d").With("restored", true).With("backupPath", "/etc/nexus/nexusd.yaml.bak")`. `problem.New(ConfigSetUnavailable, "d").With("provider", "nexusd").With("reason", "requires systemd")`.
//   - Steps:
//     1. Marshal, unmarshal into `problem.Error`, and marshal again.
//   - Expected: Byte-identical JSON on the second marshal. `Slug()` is preserved, and the extensions decode to `true` and the path.
func TestTCRA6_NewTypesAndExtensionsRoundTripThroughMarshalJSON(t *testing.T) {
	t.Skip("TC-RA-6: SKIP-only stub; un-skip when WI 1.2 lands")
}

// TC-RA-7: Write sets Retry-After for a 429 with RetryAfter, and the body is unchanged. Owner WI 1.2. Requirements: FR-5.
//
//   - Setup: `e := problem.New(RateLimited, "too many")`. Table of `RetryAfter`: `1500ms`, `60s`, `1ns`, `59s+1ms`, `0`.
//   - Steps:
//     1. For each value, `problem.Write(httptest.NewRecorder(), e)`.
//     2. Also write the same error with `RetryAfter` unset.
//   - Expected:
//   - The header is `2`, `60`, `1`, `60`, and absent for `0`. The expected value is computed as `max(1, ceil(seconds))`.
//   - `rec.Result().Header` has it, so it was set before `WriteHeader`.
//   - Each body is byte-identical to the body without `RetryAfter`, and contains no `retryAfter` member.
func TestTCRA7_WriteSetsRetryAfterForA429WithRetryAfterAndThe(t *testing.T) {
	t.Skip("TC-RA-7: SKIP-only stub; un-skip when WI 1.2 lands")
}

// TC-RA-E3: Retry-After is not set on a non-429 problem even when RetryAfter is set. Owner WI 1.2. Requirements: FR-5.
//
//   - Setup: `problem.New(Conflict, "x")` with `RetryAfter = 5s`.
//   - Steps:
//     1. `Write`.
//   - Expected: Status 409, and no `Retry-After` header.
func TestTCRAE3_RetryAfterIsNotSetOnANon429ProblemEvenWhen(t *testing.T) {
	t.Skip("TC-RA-E3: SKIP-only stub; un-skip when WI 1.2 lands")
}
