// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package detach

import "testing"

// nginx-provider TC-22: PlatformOptions selects real systemd seams or the process backend
// Owner WI 2.3. Requirements: FR-20, CON-11, NFR-9.
//
//	Setup: platform.Info values for Systemd, Unknown and OpenRC.
//	Steps:
//	  1. PlatformOptions(info, ProcessConfig{CgroupRoot: "-"}) for each.
//	Expected:
//	  - Systemd and Unknown return the zero Options{}: all four seams nil, so
//	    detach.New uses real systemd.
//	  - OpenRC returns non-nil Launch, UnitState, Signal and ListUnits.
func TestPlatformOptionsSelection(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-22 (WI 2.3)")
}

// nginx-provider TC-24: Available is platform-neutral
// Owner WI 2.3. Requirements: FR-18, FR-26.
//
//	Setup: An internal seam availableAt(info, procRoot).
//	Steps:
//	  1. systemd info.
//	  2. OpenRC info with a readable procRoot/sys/kernel/random/boot_id.
//	  3. OpenRC with boot_id unreadable.
//	  4. Unknown.
//	Expected:
//	  1. Equals SystemdAvailable()'s answer.
//	  2. Available.
//	  3. Unavailable, with a reason naming boot_id.
//	  4. Unavailable, with a reason containing no supported init system (systemd
//	    or OpenRC).
func TestAvailableIsPlatformNeutral(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-24 (WI 2.3)")
}
