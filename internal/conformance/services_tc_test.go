// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-121, TC-SV-122.
// Owner WIs: 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package conformance_test

import "testing"

// TC-SV-121: Conformance covers every services op with scopes, audit and the FR-25 negative inputs. Owner WI 5.1. Requirements: FR-2, FR-15, NFR-5, NFR-8.
//
// - Setup: `catalogStubs` with the services stubs (the reboot stub declares `AfterReboot`).
// - Steps:
//  1. Run `TestScopeAndAuditCoverEveryOp`, `TestEveryStringInputIsConstrained` and `TestAttackStringsAgainstEveryStringParameter` restricted to `services.*`.
//  2. For every op with `name`, send `../x`, `a/b`, `a;b`, `a b`, a 65-character name, `-x` and `x.service`.
//  3. For the reboot, send `confirmHost` values `h1\n`, `h1\x00` and `h1\x1b[31m`.
//
// - Expected: Every listed input answers 400 `validation-failed` naming the field, with no handler called (the stub records zero calls). Every services op appears in the scope and audit coverage.
func TestTCSV121_ConformanceCoversEveryServicesOpWithScopes(t *testing.T) {
	t.Skip("TC-SV-121: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-122: Each op refuses a token without its scope with 403 naming the scope. Owner WI 5.1. Requirements: FR-2.
//
// - Setup: Tokens: `read` (`services:read`), `logs` (`services:logs`), `write` (`services:write`) and `reboot` (`services:reboot`).
// - Steps:
//  1. Call every op with every token.
//
// - Expected:
//   - each op succeeds or passes scope with exactly its own scope;
//   - with any other token it answers 403 `forbidden`, with `requiredScope` = its scope;
//   - in particular, `read` cannot get logs, `write` cannot reboot, and `reboot` cannot start.
func TestTCSV122_EachOpRefusesATokenWithoutIts(t *testing.T) {
	t.Skip("TC-SV-122: SKIP-only stub; un-skip when WI 5.1 lands")
}
