// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package problem

import "testing"

// TC-SM-5 (WI 1.2; FR-16, FR-4, FR-5, FR-6, FR-7, FR-8, FR-12): The 14 new problem types are registered with status, exit and extensions
func TestTCSM5_The14NewProblemTypesAre(t *testing.T) {
	// - Setup: an expected table copied from architecture.md "Problem types":
	//
	//   | Slug | Status | Exit | Extensions | Operation-level |
	//   |---|---|---|---|---|
	//   | `config-invalid` | 422 | 1 | `configPath`, `errors` | no |
	//   | `upload-too-large` | 413 | 2 | `maxBytes` | no |
	//   | `upload-not-found` | 422 | 1 | `uploadId` | no |
	//   | `update-not-configured` | 409 | 1 | — | no |
	//   | `update-signature-invalid` | 422 | 1 | — | no |
	//   | `update-package-invalid` | 422 | 1 | `check` | no |
	//   | `version-downgrade` | 409 | 1 | `runningVersion`, `packageVersion` | no |
	//   | `rollback-unavailable` | 409 | 1 | `path` | no |
	//   | `config-apply-failed` | 0 | 1 | — | yes |
	//   | `restart-unhealthy` | 0 | 1 | `condition`, `version` | yes |
	//   | `update-failed` | 0 | 1 | `rolledBack`, `step`, `previousVersion`, `newVersion` | yes |
	//   | `update-rollback-failed` | 0 | 1 | `rolledBack`, `step`, `previousVersion`, `newVersion` |
	//     yes |
	//   | `rollback-failed` | 0 | 1 | `rolledBack`, `condition`, `fromVersion`, `toVersion` | yes |
	// - Steps:
	//   1. For each row, look up the slug's `Type`.
	//   2. Call `problem.New(slug, "detail")` for each slug.
	//   3. Count the registry rows per slug.
	// - Expected:
	//   - Every field matches the table.
	//   - `URI()` is `urn:nexus:problem:<slug>`.
	//   - `New` accepts each slug.
	//   - Each slug appears exactly once.
	//   - The rows for the existing slugs are unchanged.
	t.Skip("TC-SM-5: SKIP-only stub; un-skip when WI 1.2 lands")
}
