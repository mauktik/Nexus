// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-42, TC-RA-69.
// Owner WIs: 2.4, 4.1. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package mcpadapter_test

import "testing"

// TC-RA-42: No MCP tool exists for any token op, and the tool budget is unchanged. Owner WI 2.4. Requirements: FR-10, OOS-1.
//
//   - Setup: The real catalog adapter. `baseTools` is the tool-name set at `521261c`, computed by the test from the catalog with the three token op IDs filtered out.
//   - Steps:
//     1. `tools/list`.
//   - Expected:
//   - No tool name maps to `nexusd.tokens.*`.
//   - The tool set equals `baseTools`.
//   - `TestToolBudget` and `TestTCSM96` (exactly two nexusd tools) pass unchanged.
func TestTCRA42_NoMCPToolExistsForAnyTokenOpAndTheToolBudgetIs(t *testing.T) {
	t.Skip("TC-RA-42: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-RA-69: No MCP tool exists for any of the six new ops. Owner WI 4.1. Requirements: FR-10, OOS-1.
//
//   - Setup: The real catalog adapter, after 4.1.
//   - Steps:
//     1. `tools/list`.
//     2. Count the nexusd tools.
//   - Expected:
//   - No tool maps to `nexusd.tokens.*`, `nexusd.config.*` or `nexusd.actions.update-config`.
//   - The tool count equals the count at `521261c` (computed by filtering the six new IDs from the catalog).
//   - `TestTCSM96` still sees exactly two nexusd tools.
func TestTCRA69_NoMCPToolExistsForAnyOfTheSixNewOps(t *testing.T) {
	t.Skip("TC-RA-69: SKIP-only stub; un-skip when WI 4.1 lands")
}
