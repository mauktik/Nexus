// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-4, TC-SV-32, TC-SV-33.
// Owner WIs: 1.1, 2.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package catalog_test

import "testing"

// TC-SV-4: Every existing catalog op still validates and the golden table changes only by the services rows. Owner WI 1.1. Requirements: CON-4.
//
// - Setup: `catalog.All()` at the WI's head; the golden table of `internal/catalog/golden_test.go` at base `43e5792`, read with `git show 43e5792:internal/catalog/golden_test.go` (skip with a reason under `skipUnderMake` if git is unavailable).
// - Steps:
//  1. Validate every op of `catalog.All()` (it panics at init if not).
//  2. Diff the golden rows at the head against base.
//
// - Expected: No panic. Every base row is present byte-for-byte. Rows added since base all have op IDs starting `services.`, and there are exactly eight after WI 2.2 (zero before it).
func TestTCSV4_EveryExistingCatalogOpStillValidatesAnd(t *testing.T) {
	t.Skip("TC-SV-4: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SV-32: The eight services ops are in the catalog with their golden rows. Owner WI 2.2. Requirements: FR-1, FR-2, FR-4, FR-5, FR-6, FR-7, FR-10, FR-14, FR-15.
//
// - Setup: `catalog.All()`.
// - Steps:
//  1. Collect the ops whose IDs start `services.`.
//
// - Expected: Exactly the eight op IDs of the architecture's Operation catalog, each with:
//
//   - its kind and route, mode, risk, scope, lane, CLI binding and MCP name as in the table;
//
//   - `Untrusted: true` on all eight;
//
//   - `Idempotent` on the seven non-reboot ops;
//
//   - `Audited` only on `services.services.logs`;
//
//   - on `services.actions.reboot`: `WhatIf`, `Cancel: running`, `CompletesAcrossReboot`, `CLI.ReconnectWindow == 10*time.Minute` and `CLI.TypedConfirmation == "confirmHost"`, with no MCP name.
//
//     The provider spec declares exactly the scopes `services:read`, `services:logs`, `services:write` and `services:reboot`.
func TestTCSV32_TheEightServicesOpsAreInThe(t *testing.T) {
	t.Skip("TC-SV-32: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SV-33: The CLI grammar uses only approved verbs on the nouns service, servicelog and machine. Owner WI 2.2. Requirements: FR-15.
//
// - Setup: The services ops.
// - Steps:
//  1. Read each op's `CLI.Noun` and `CLI.Verb`.
//
// - Expected:
//   - nouns ⊆ {`service`, `servicelog`, `machine`};
//   - every verb is in `def`'s approved list;
//   - `machine` has only `restart`, and `service` has `get`, `start`, `stop`, `restart` and `invoke`;
//   - `verbs.go` is unchanged from base (no new verb);
//   - the control op's `--action` flag is an enum of `start`, `stop` and `restart`.
func TestTCSV33_TheCLIGrammarUsesOnlyApprovedVerbs(t *testing.T) {
	t.Skip("TC-SV-33: SKIP-only stub; un-skip when WI 2.2 lands")
}
