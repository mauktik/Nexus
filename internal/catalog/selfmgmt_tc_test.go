// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package catalog_test

import "testing"

// TC-SM-93 (WI 6.1; FR-1, FR-17, FR-13): The catalog golden gains exactly the seven nexusd rows
func TestTCSM93_TheCatalogGoldenGainsExactlyThe(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. Run `TestCatalogMatchesGoldenTable` with the 7 new rows, copied from architecture.md's
	//     Operation catalog.
	// - Expected:
	//   - It passes.
	//   - The upload row has CLI `" "` (empty noun and verb) and tool `""`.
	//   - The golden count comment says 32 rows.
	t.Skip("TC-SM-93: SKIP-only stub; un-skip when WI 6.1 lands")
}
