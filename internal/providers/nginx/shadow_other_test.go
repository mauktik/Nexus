//go:build !linux

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package nginx

import "testing"

// nginx-provider TC-ERR23: Shadow tests refuse on non-Linux hosts
// Owner WI 5.4. Requirements: ASM-10.
//
//	Setup: macOS.
//	Steps:
//	  1. ShadowTester.Test.
//	Expected: An error containing shadow tests need Linux.
func TestShadowNeedsLinux(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR23 (WI 5.4)")
}
