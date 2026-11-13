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
	// - Setup: for step 2, a spec-level check helper in `catalog_test` that takes `[]def.ProviderSpec`,
	//   so planted negative specs can be fed to it. These are the spec-level `CLI.Upload` consistency
	//   rules, which `core.New` does not check (it checks only bound ops; see TC-SM-ERR15).
	// - Steps:
	//   1. Run `TestCatalogMatchesGoldenTable` with the 7 new rows, copied from architecture.md's
	//     Operation catalog.
	//   2. Run the spec-level check over `catalog.All()`: for every op with `CLI.Upload`, the
	//     referenced op exists in the same `ProviderSpec`, is `KindUpload`, and its output type has a
	//     string field whose JSON name is `CLI.Upload.Field`.
	//   3. Run the same check over planted copies of the nexusd spec: (a) `CLI.Upload.Op` names an ID
	//     absent from the spec; (b) it names a `KindList` op; (c) the upload op's output lacks the
	//     `uploadId` field; (d) `uploadId` is declared but not a string.
	// - Expected:
	//   - Step 1 passes.
	//   - The upload row has CLI `" "` (empty noun and verb) and tool `""`.
	//   - The golden count comment says 32 rows.
	//   - Step 2 reports no violation, and it visited `nexusd.actions.update` (the check is not
	//     vacuous).
	//   - Step 3: each planted spec yields exactly one violation naming the op ID and the rule broken.
	t.Skip("TC-SM-93: SKIP-only stub; un-skip when WI 6.1 lands")
}
