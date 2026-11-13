// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package scripts_test

import "testing"

// nginx-provider TC-27: apt, scripts and docker gate on detach.Available
// Owner WI 2.4. Requirements: FR-26, FR-18.
//
//	Setup: Each provider constructed with its availability seam returning:
//	  - available (OpenRC process backend);
//	  - unavailable with the reason no supported init system (systemd or
//	    OpenRC).
//	  apt has its binaries present in a fake root.
//	Steps:
//	  1. Preflight for each.
//	  2. docker admitUpdate with unavailable.
//	Expected:
//	  - Available → enabled.
//	  - Unavailable → scripts reason scripts run only as detached operations: no
//	    supported init system…, apt reason apt operations run only as detached
//	    operations: …, and docker update refused with container updates run only
//	    as detached operations: ….
//	  - No source file under internal/providers/ references
//	    detach.SystemdAvailable (grep in the test).
func TestScriptsPreflightUsesDetachAvailable(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-27 (WI 2.4)")
}
