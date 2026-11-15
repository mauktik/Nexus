// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-104.
// Owner WIs: 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package catalog_test

import "testing"

// TC-HP-104: The catalog golden table has the ten host rows. Owner WI 5.1. Requirements: FR-14, FR-2, NFR-7.
//
// - Setup: `catalog.All()`.
// - Steps: run `TestCatalogMatchesGoldenTable` with the ten rows added after the nginx rows; filter the catalog to provider `host`.
// - Expected: it passes; `len(golden)` is 81 (71 at 47d93bc plus 10) and the header comment states that count and names the host table; the host ops appear after `pihole` in `catalog.All()` order.
func TestTCHP104_TheCatalogGoldenTableHasTheTen(t *testing.T) {
	t.Skip("TC-HP-104: SKIP-only stub; un-skip when WI 5.1 lands")
}
