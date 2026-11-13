// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package conformance_test

import "testing"

// nginx-provider TC-58: The conformance suite covers every nginx op
// Owner WI 4.4. Requirements: NFR-7, FR-14, FR-15, CON-4.
//
//	Setup: A catalogStubs entry: 4 Sync bindings, and 9 testprovider.InProcess
//	bindings.
//	Steps:
//	  1. Run go test ./internal/conformance.
//	Expected:
//	  - TestScopeAndAuditCoverEveryOp:
//	    - reads need nginx:read;
//	    - lifecycle and toggle ops need nginx:write;
//	    - add and update need nginx:config;
//	    - a token with only nginx:config cannot read status (403 naming
//	      nginx:read);
//	    - every mutation is audited.
//	  - TestEveryStringInputIsConstrained and TestNoArbitraryExecutionSurface
//	    pass.
//	  - The FR-25 negative inputs are refused with validation-failed for name,
//	    ifMatch and cursor. For content, a negative input is accepted only as free
//	    text and never reaches argv, which the stub spy shows.
//	  - The stub binds exactly the 13 IDs.
func TestNginxStubBindsEveryOp(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-58 (WI 4.4)")
}
