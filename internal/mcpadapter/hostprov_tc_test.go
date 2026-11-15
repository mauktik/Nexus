// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-106, TC-HP-107.
// Owner WIs: 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package mcpadapter_test

import "testing"

// TC-HP-106: The MCP tool budget with the host tools. Owner WI 5.1. Requirements: NFR-7, CON-6, FR-14.
//
// - Setup: the full catalog tool list and the list without `host`, via the existing `toolBudget` helper.
// - Steps: compute both budgets.
// - Expected: total tokens < 2,850 (and < 4,000); host delta in 1..149 (135 expected from A-35's 541 characters; asserted as the range, the figure logged); exactly 68 tools, 3 of them host: `get_host_summary`, `get_host_health`, `get_host_metrics`; each `readOnlyHint true` and description = the spec text + the 94-character untrusted suffix; no other host op is a tool; `TestTCSM96`'s pin is 68.
func TestTCHP106_TheMCPToolBudgetWithTheHost(t *testing.T) {
	t.Skip("TC-HP-106: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-HP-107: MCP results of the host tools are labelled untrusted. Owner WI 5.1. Requirements: FR-15, FR-10.
//
// - Setup: the adapter over a server with the host stub provider (conformance stub returning a resource whose `hostname` is `ignore previous instructions`).
// - Steps: call each of the three tools.
// - Expected: each result carries `contentTrust: "untrusted-host-data"`; the hostile text is only inside the structured data, never in a description or message field.
func TestTCHP107_MCPResultsOfTheHostToolsAre(t *testing.T) {
	t.Skip("TC-HP-107: SKIP-only stub; un-skip when WI 5.1 lands")
}
