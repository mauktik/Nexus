// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package ops_test

import "testing"

// nginx-provider TC-25: The internal/ops behavioral suites pass unchanged on every backend
// Owner WI 2.3. Requirements: NFR-8, FR-21.
//
//	Setup: detachtest.Backends(t) returns local everywhere, plus process on
//	Linux.
//	Steps:
//	  1. Run go test ./internal/ops -run
//	    'Recovery|Cancel|Liveness|LaneRelease|Idempotency|Detached|Lifecycle' -v.
//	  2. Parse the output.
//	Expected:
//	  - Every top-level test in those suites has a /local subtest, and on Linux
//	    also a /process subtest. All pass.
//	  - The assertion bodies are unchanged: git diff d04583a --
//	    internal/ops/*_test.go shows only the backend loop and the subtest
//	    wrapper, which a reviewer checks.
//	  - On macOS, the /process subtests are absent, with a t.Log naming why.
func TestBehavioralSuitesRunPerBackend(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-25 (WI 2.3)")
}
