// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package mcpadapter_test

import "testing"

// TC-SM-95 (WI 6.1; FR-17, OOS-8): MCP lists exactly two read-only nexusd tools
func TestTCSM95_MCPListsExactlyTwoReadOnly(t *testing.T) {
	// - Setup: the MCP adapter over the full catalog with nexusd enabled, and a token with all scopes.
	// - Steps:
	//   1. `tools/list`.
	//   2. Search the tool names and the full schema JSON for `reload`, `restart`, `update` and
	//     `rollback` in a nexusd context, and for `uploads`.
	//   3. `tools/call` for a made-up name `nexusd_update`.
	// - Expected:
	//   - nexusd tools are exactly `get_nexusd` and `validate_nexusd_config`.
	//   - Both have `annotations.readOnlyHint == true` and no `destructiveHint`.
	//   - No other nexusd op appears.
	//   - Step 3 returns the unknown-tool error.
	t.Skip("TC-SM-95: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-96 (WI 6.1; NFR-3): The tool budget stays under 4000 and nexusd adds fewer than 100 tokens
func TestTCSM96_TheToolBudgetStaysUnder4000(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. Run `TestToolBudget`.
	//   2. Compute the budget with and without the nexusd tools, using the same `toolBudget` function.
	// - Expected:
	//   - Total < 4000.
	//   - The delta is under 100.
	//   - The tool count is 26 (24 + 2).
	t.Skip("TC-SM-96: SKIP-only stub; un-skip when WI 6.1 lands")
}
