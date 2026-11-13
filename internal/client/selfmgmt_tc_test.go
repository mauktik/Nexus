// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package client

import "testing"

// TC-SM-64 (WI 4.1; FR-10): client.PinnedTLS is exported and normalizes fingerprints
func TestTCSM64_ClientPinnedTLSIsExportedAndNormalizes(t *testing.T) {
	// - Setup: an `httptest` TLS server; its leaf SHA-256 fingerprint F in lowercase hex.
	// - Steps:
	//   1. `PinnedTLS(F)`, `PinnedTLS(upper(F))` and `PinnedTLS(colon-separated F)`, each followed by
	//     a GET.
	//   2. `PinnedTLS(wrong)` followed by a GET.
	//   3. Call `PinnedTLS("zz")`.
	//   4. Run the existing InsecureSkipVerify static check.
	// - Expected:
	//   - Step 1: all three GETs succeed.
	//   - Step 2 fails with `*PinMismatchError`.
	//   - Step 3 returns an error.
	//   - Step 4 passes: no bare `InsecureSkipVerify` exists without `VerifyConnection`.
	t.Skip("TC-SM-64: SKIP-only stub; un-skip when WI 4.1 lands")
}
