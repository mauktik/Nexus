// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-123, TC-SV-124, TC-SV-149.
// Owner WIs: 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package mcpadapter_test

import "testing"

// TC-SV-123: MCP lists exactly four services tools within the budget, and no reboot tool. Owner WI 5.1. Requirements: FR-15, NFR-6, ASM-6.
//
// - Setup: `catalogTools(t)` and `toolBudget`.
// - Steps:
//  1. Filter the tools that come from `services.*` ops.
//  2. Compute the budget with and without them.
//
// - Expected:
//   - names are exactly `list_services`, `get_service`, `get_service_logs` and `control_service`;
//   - the first three have `readOnlyHint: true`, and `control_service` has `readOnlyHint: false` and `destructiveHint: true`;
//   - each description ends with the untrusted suffix;
//   - no tool name contains `reboot` or `machine`;
//   - the services delta is ≤ 800 characters and ≤ 200 tokens, and the total is ≤ 3,100 tokens;
//   - `TestToolBudget`'s pinned count is 72.
func TestTCSV123_MCPListsExactlyFourServicesToolsWithin(t *testing.T) {
	t.Skip("TC-SV-123: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-124: get_service_logs is since-paged with the default tail. Owner WI 5.1. Requirements: FR-6, FR-15.
//
// - Setup: The services provider over F-HOSTS with TC-SV-85's 2,500-line journal.
// - Steps:
//  1. Call `get_service_logs {name: nexusd}`.
//  2. Call it with `since` from the response's continuation.
//
// - Expected:
//   - (1) the newest 200 lines, with a continuation value;
//   - (2) the lines after it, oldest first, at most 1,000;
//   - the input schema of the tool has `since` and `tail`, and its description carries the since-paged suffix.
func TestTCSV124_GetServiceLogsIsSincePagedWith(t *testing.T) {
	t.Skip("TC-SV-124: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-149: get_service_logs on OpenRC file logs never hands back a continuation that answers 400. Owner WI 5.1. Requirements: FR-6, FR-15, ASM-5.
//
// - Setup: The services provider over F-HOSTS and F-ROOTS from `t1-alpine-openrc`, with `nx-svc-test`'s `output_log` and `error_log` pointing at temp files holding TC-SV-86's 3,000 `out-*` and 10 `err-*` lines. The MCP adapter is the real one (`catalogTools`).
// - Steps:
//  1. Call `get_service_logs {name: nx-svc-test}`.
//  2. If the result carries a continuation (`nextOffset`), call `get_service_logs {name: nx-svc-test, since: <nextOffset>}`, and repeat while a continuation is returned, at most 5 times.
//
// - Expected:
//   - (1) is a success with the newest 200 `out-*` lines and the 10 `err-*` lines (as TC-SV-86);
//   - no call in (2) answers 400 or any other error: every continuation the tool hands back is one the provider accepts on OpenRC;
//   - the result matches the DF-2 file-frame rule the architect records: either no continuation (`truncated: false`, no `nextOffset`), or a continuation whose follow-up page is empty or holds only lines not already returned (never a repeat of page 1);
//   - `docs/mcp.md`'s `get_service_logs` row states the OpenRC pager behavior (asserted by string match against DF-2's wording).
func TestTCSV149_GetServiceLogsOnOpenRCFileLogs(t *testing.T) {
	t.Skip("TC-SV-149: SKIP-only stub; un-skip when WI 5.1 lands")
}
