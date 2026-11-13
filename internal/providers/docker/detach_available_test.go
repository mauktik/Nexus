// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package docker

import "testing"

// nginx-provider TC-27 (docker part): container updates gate on detach.Available
// Owner WI 2.4. Requirements: FR-26, FR-18.
//
//	Setup: the provider's availability seam returns available (OpenRC
//	process backend), then unavailable with reason "no supported init system
//	(systemd or OpenRC)".
//	Expected:
//	  - Available -> admitUpdate admits a standalone container update.
//	  - Unavailable -> admitUpdate refuses with "container updates run only
//	    as detached operations: no supported init system (systemd or
//	    OpenRC)".
//	  - No file under internal/providers/ references detach.SystemdAvailable.
func TestDockerUpdateGateUsesDetachAvailable(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-27 (WI 2.4)")
}
