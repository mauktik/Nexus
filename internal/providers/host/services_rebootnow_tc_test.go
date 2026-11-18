// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-115.
// Owner WIs: 4.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-SV-115: RebootRequiredNow equals the host provider's own rule. Owner WI 4.4. Requirements: FR-10, FR-11.
//
// - Setup: The host provider's own captures (`testdata/ubuntu-systemd`, `alpine-openrc`) with F-FS, plus a variant with `/var/run/reboot-required` and `.pkgs` added.
// - Steps:
//  1. `RebootRequiredNow(opts)`, and the host provider's `rebootRequired()` over the same options.
//
// - Expected: The two are equal in every variant. `TC-HP-93`'s apt parity still passes.
func TestTCSV115_RebootRequiredNowEqualsTheHostProvidersOwnRule(t *testing.T) {
	t.Skip("TC-SV-115: SKIP-only stub; un-skip when WI 4.4 lands")
}
