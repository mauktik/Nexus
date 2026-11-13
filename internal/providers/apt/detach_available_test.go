// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package apt

import "testing"

// nginx-provider TC-E13: An enabled apt section on a host without apt is not a config error
// Owner WI 2.4. Requirements: FR-26.
//
//	Setup: A fake root without apt-get. Config with apt.enabled: true. Detach
//	available.
//	Steps:
//	  1. validate-config.
//	  2. Preflight.
//	Expected:
//	  - Step 1 exits 0.
//	  - The Preflight reason is /usr/bin/apt-get is missing or not executable;
//	    the apt provider needs apt and dpkg: the binary check comes before the
//	    detach check, so the reason never mentions detach.
func TestEnabledAptWithoutBinariesIsNotConfigError(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E13 (WI 2.4)")
}

// nginx-provider TC-27 (apt part): apt gates on detach.Available
// Owner WI 2.4. Requirements: FR-26, FR-18.
//
//	Setup: apt binaries present in a fake root; the availability seam returns
//	available, then unavailable with reason "no supported init system
//	(systemd or OpenRC)".
//	Expected:
//	  - Available -> Preflight enabled.
//	  - Unavailable -> reason "apt operations run only as detached
//	    operations: no supported init system (systemd or OpenRC)".
//	  - The seam's default is detach.Available, not detach.SystemdAvailable.
func TestAptPreflightUsesDetachAvailable(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-27 (WI 2.4)")
}
