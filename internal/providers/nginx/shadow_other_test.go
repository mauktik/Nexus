//go:build !linux

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package nginx

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// nginx-provider TC-ERR23: Shadow tests refuse on non-Linux hosts
// Owner WI 5.4. Requirements: ASM-10.
//
//	Setup: macOS.
//	Steps:
//	  1. ShadowTester.Test.
//	Expected: An error containing shadow tests need Linux.
func TestShadowNeedsLinux(t *testing.T) {
	dir := t.TempDir()
	var tester ShadowTester = newShadowTester(dir, helperInput{Nginx: DefaultNginx}, &bytes.Buffer{})
	if _, err := tester.Test(context.Background(), "cand.conf", "/nonexistent"); err == nil ||
		!strings.Contains(err.Error(), "shadow tests need Linux") {
		t.Errorf("ShadowTester.Test: %v, want an error containing %q", err, "shadow tests need Linux")
	}
	var stderr bytes.Buffer
	if code := Helper([]string{"shadow-test", "--dir", dir}, &bytes.Buffer{}, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "shadow tests need Linux") {
		t.Errorf("nginx-op shadow-test: exit %d, stderr %q; want 1 with %q", code, stderr.String(),
			"shadow tests need Linux")
	}
}
