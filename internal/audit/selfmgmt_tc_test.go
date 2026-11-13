// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package audit_test

import "testing"

// TC-SM-50 (WI 3.4; FR-16): The facts collector is bounded, is a no-op without a collector, and read ops record no facts
func TestTCSM50_TheFactsCollectorIsBoundedIs(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. `AddFact(context.Background(), "a", "b")`.
	//   2. `ctx := WithFacts(bg)`; add a 100-byte key and a 1000-byte value.
	//   3. Add from 50 goroutines.
	//   4. Read through the collector accessor.
	//   5. A sync read op through the core with a handler calling `AddFact`.
	// - Expected:
	//   - Step 1 does not panic.
	//   - The stored key is at most 64 bytes and the value at most 256 bytes.
	//   - The concurrent adds are race-free under `-race`.
	//   - The read op writes no audit record (reads are not audited), so no facts appear.
	t.Skip("TC-SM-50: SKIP-only stub; un-skip when WI 3.4 lands")
}
