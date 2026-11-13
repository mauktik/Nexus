// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package core_test

import "testing"

// TC-SM-49 (WI 3.4; FR-16, FR-6): Facts are attached to refused and requested records, and an upload writes a stored record
func TestTCSM49_FactsAreAttachedToRefusedAnd(t *testing.T) {
	// - Setup:
	//   - real core with the real `audit.Log` in a temp dir;
	//   - an F-UPL provider whose `Receive` calls `audit.AddFact(ctx, "sha256", h)`;
	//   - a long-running test op whose `Admit` adds `k=v` and then refuses or admits on demand.
	// - Steps:
	//   1. Upload.
	//   2. Invoke the long-running op set to refuse.
	//   3. Invoke it again set to admit.
	//   4. Read `audit.jsonl`.
	// - Expected:
	//   - Upload: a `requested` record, then a `stored` record with the same `requestId`, where
	//     `stored.facts.sha256 == h`.
	//   - Refuse: a `refused` record with `facts: {k: v}` and the problem slug.
	//   - Admit: the `requested` record has `facts: {k: v}`.
	t.Skip("TC-SM-49: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SM-ERR26 (WI 3.4; FR-16, FR-6): A failed stored write is logged and the upload still answers 201
func TestTCSMERR26_AFailedStoredWriteIsLogged(t *testing.T) {
	// - Setup: an audit sink failing only on `stored`; a captured `slog`.
	// - Steps:
	//   1. Upload.
	// - Expected:
	//   - 201.
	//   - The log has an error entry naming the request ID and `stored`, at the same level the
	//     existing `note` failure uses.
	t.Skip("TC-SM-ERR26: SKIP-only stub; un-skip when WI 3.4 lands")
}
