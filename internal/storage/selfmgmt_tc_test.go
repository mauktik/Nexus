// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package storage_test

import "testing"

// TC-SM-48 (WI 3.3; FR-6): Staging bytes count toward the storage budget
func TestTCSM48_StagingBytesCountTowardTheStorage(t *testing.T) {
	// - Setup: `storage.New` with `StagingDir` = temp dir holding 3 MiB in two files and a limit of 4
	//   MiB; ops dir holding 2 MiB.
	// - Steps:
	//   1. Measure.
	//   2. Run an eviction pass.
	// - Expected:
	//   - `Report.StagingBytes == 3 MiB`, and `Total` includes it.
	//   - The limit flag is set: 5 MiB > 4 MiB.
	//   - Eviction removes no staging file.
	t.Skip("TC-SM-48: SKIP-only stub; un-skip when WI 3.3 lands")
}
