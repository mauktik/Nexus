//go:build !linux

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package detach

import "testing"

// nginx-provider TC-ERR5: The process backend refuses on non-Linux hosts
// Owner WI 2.1. Requirements: FR-20, OOS-12.
//
//	Setup: macOS (the dev host).
//	Steps:
//	  1. Call each seam of ProcessBackend(ProcessConfig{}).
//	Expected: Each returns an error containing detached processes need Linux.
func TestProcessBackendNeedsLinux(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR5 (WI 2.1)")
}
