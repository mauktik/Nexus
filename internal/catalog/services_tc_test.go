// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-4, TC-SV-32, TC-SV-33.
// Owner WIs: 1.1, 5.1 (TC-SV-32 and TC-SV-33 moved from WI 2.2 in
// architecture Revision 1, with the catalog line). Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package catalog_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/catalog"
)

// TC-SV-4: Every existing catalog op still validates and the golden table changes only by the services rows. Owner WI 1.1. Requirements: CON-4.
//
// - Setup: `catalog.All()` at the WI's head; the golden table of `internal/catalog/golden_test.go` at base `43e5792`, read with `git show 43e5792:internal/catalog/golden_test.go` (skip with a reason under `skipUnderMake` if git is unavailable).
// - Steps:
//  1. Validate every op of `catalog.All()` (it panics at init if not).
//  2. Diff the golden rows at the head against base.
//
// - Expected: No panic. Every base row is present byte-for-byte. Rows added since base all have op IDs starting `services.`, and there are exactly eight after WI 5.1 adds the catalog line (zero before it; Revision 1 moved the line from WI 2.2).
func TestTCSV4_EveryExistingCatalogOpStillValidatesAnd(t *testing.T) {
	const base = "43e5792"
	const servicesRows = 8

	// catalog.All() builds every spec, each op by def.Define, which panics on
	// a declaration that fails validate.
	var nServices int
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("catalog.All() panicked: %v", r)
			}
		}()
		for _, spec := range catalog.All() {
			for _, op := range spec.Ops {
				if op.InputType() == nil {
					t.Errorf("%s: not built by def.Define", op.ID)
				}
				if strings.HasPrefix(op.ID, "services.") {
					nServices++
				}
			}
		}
	}()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found: cannot read the golden table at base " + base)
	}
	if err := exec.Command("git", "cat-file", "-e", base+"^{commit}").Run(); err != nil {
		t.Skipf("base commit %s not available: %v", base, err)
	}
	baseSrc, err := exec.Command("git", "show", base+":internal/catalog/golden_test.go").Output()
	if err != nil {
		t.Fatalf("git show %s:internal/catalog/golden_test.go: %v", base, err)
	}
	headSrc, err := os.ReadFile("golden_test.go")
	if err != nil {
		t.Fatal(err)
	}
	baseRows, headRows := goldenRows(t, string(baseSrc)), goldenRows(t, string(headSrc))

	inHead := map[string]bool{}
	for _, r := range headRows {
		inHead[r] = true
	}
	for _, r := range baseRows {
		if !inHead[r] {
			t.Errorf("base row missing or changed at head:\n%s", r)
		}
	}

	inBase := map[string]bool{}
	for _, r := range baseRows {
		inBase[r] = true
	}
	var added int
	for _, r := range headRows {
		if inBase[r] {
			continue
		}
		added++
		if id, _, _ := strings.Cut(strings.TrimPrefix(r, `{"`), `"`); !strings.HasPrefix(id, "services.") {
			t.Errorf("row added since base is not a services row:\n%s", r)
		}
	}

	// Zero services ops before WI 5.1 adds the catalog line, exactly eight after.
	if nServices != 0 && nServices != servicesRows {
		t.Errorf("catalog has %d services ops, want 0 or %d", nServices, servicesRows)
	}
	if added != nServices {
		t.Errorf("golden table gained %d rows since %s, want %d (one per services op)", added, base, nServices)
	}
}

// goldenRows returns the rows of the golden table in the source of
// golden_test.go, each line trimmed of its indentation.
func goldenRows(t *testing.T, src string) []string {
	t.Helper()
	_, body, ok := strings.Cut(src, "var golden = []row{\n")
	if !ok {
		t.Fatal("golden_test.go has no golden table")
	}
	body, _, ok = strings.Cut(body, "\n}\n")
	if !ok {
		t.Fatal("golden_test.go: golden table is not closed")
	}
	var rows []string
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, `{"`) {
			rows = append(rows, line)
		}
	}
	if len(rows) == 0 {
		t.Fatal("golden_test.go: golden table has no rows")
	}
	return rows
}

// TC-SV-32: The eight services ops are in the catalog with their golden rows. Owner WI 5.1. Requirements: FR-1, FR-2, FR-4, FR-5, FR-6, FR-7, FR-10, FR-14, FR-15.
//
// - Setup: `catalog.All()` with WI 5.1's `servicesspec.Spec()` line, and the eight golden rows in `golden_test.go` (95 rows; `hostprov_tc_test.go` pins 95).
// - Steps:
//  1. Collect the ops whose IDs start `services.`.
//
// - Expected: Exactly the eight op IDs of the architecture's Operation catalog, each with:
//
//   - its kind and route, mode, risk, scope, CLI binding and MCP name as in the table (not its lane: `def.Op` has no lane field, so the lanes are asserted on the bindings by TC-SV-94 and TC-SV-96, WI 4.3, and TC-SV-116, WI 4.4);
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
	t.Skip("TC-SV-32: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-33: The CLI grammar uses only approved verbs on the nouns service, servicelog and machine. Owner WI 5.1. Requirements: FR-15.
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
	t.Skip("TC-SV-33: SKIP-only stub; un-skip when WI 5.1 lands")
}
