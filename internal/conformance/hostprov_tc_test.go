// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-105.
// Owner WIs: 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package conformance_test

import "testing"

// TC-HP-105: The conformance suite covers the host provider. Owner WI 5.1. Requirements: NFR-7, FR-2, FR-14, NFR-6.
//
// - Setup: `catalogStubs[host.Name]` with ten `testprovider.Sync` bindings.
// - Steps: run the suite (`TestScopeAndAuditCoverEveryOp`, `TestEveryStringInputIsConstrained`, `TestNoArbitraryExecutionSurface`) and a host filter of each.
// - Expected: all pass; each host op answers 403 `insufficient-scope` for a token without `system:read` and 200 with it; every string input of a host op (`cursor`, `metric[]`) has a pattern or enum and a max length; no host op accepts a path, device or command field.
func TestTCHP105_TheConformanceSuiteCoversTheHostProvider(t *testing.T) {
	t.Skip("TC-HP-105: SKIP-only stub; un-skip when WI 5.1 lands")
}
