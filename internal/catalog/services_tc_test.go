// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

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
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
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
		id, _, _ := strings.Cut(strings.TrimPrefix(r, `{"`), `"`)
		if strings.HasPrefix(id, "storage.") {
			continue // rc7 integration: the sibling storage provider's rows (its TC-STG-200 counts them)
		}
		added++
		if !strings.HasPrefix(id, "services.") {
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
	type want struct {
		kind       def.Kind
		route      string
		mode       def.Mode
		risk       def.Risk
		scope      string
		cli        string
		tool       string
		idempotent bool
		audited    bool
	}
	table := map[string]want{
		"services.services.list": {def.KindList, "GET /providers/services/services", def.ModeSync, def.RiskReadOnly,
			"services:read", "service get", "list_services", true, false},
		"services.services.get": {def.KindGet, "GET /providers/services/services/{name}", def.ModeSync, def.RiskReadOnly,
			"services:read", "service get name", "get_service", true, false},
		"services.services.logs": {def.KindSubresource, "GET /providers/services/services/{name}/logs", def.ModeStream,
			def.RiskReadOnly, "services:logs", "servicelog get name", "get_service_logs", true, true},
		"services.services.start": {def.KindResourceAction, "POST /providers/services/services/{name}/start",
			def.ModeLongRunning, def.RiskMutating, "services:write", "service start name", "", true, false},
		"services.services.stop": {def.KindResourceAction, "POST /providers/services/services/{name}/stop",
			def.ModeLongRunning, def.RiskDestructive, "services:write", "service stop name", "", true, false},
		"services.services.restart": {def.KindResourceAction, "POST /providers/services/services/{name}/restart",
			def.ModeLongRunning, def.RiskMutating, "services:write", "service restart name", "", true, false},
		"services.services.control": {def.KindResourceAction, "POST /providers/services/services/{name}/control",
			def.ModeLongRunning, def.RiskDestructive, "services:write", "service invoke name", "control_service", true, false},
		"services.actions.reboot": {def.KindProviderAction, "POST /providers/services/actions/reboot",
			def.ModeLongRunning, def.RiskDestructive, "services:reboot", "machine restart", "", false, false},
	}

	var ops []def.Op
	var spec def.ProviderSpec
	for _, s := range catalog.All() {
		for _, op := range s.Ops {
			if strings.HasPrefix(op.ID, "services.") {
				ops = append(ops, op)
				spec = s
			}
		}
	}
	if len(ops) != len(table) {
		t.Fatalf("catalog has %d services ops, want %d", len(ops), len(table))
	}
	scopes := map[string]bool{spec.ReadScope: true}
	for _, op := range ops {
		w, ok := table[op.ID]
		if !ok {
			t.Errorf("unexpected op %s", op.ID)
			continue
		}
		scopes[op.Scope.Name] = true
		method, path, err := openapi.Route(op)
		if err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		cli := op.CLI.Noun + " " + op.CLI.Verb
		if op.CLI.Positional != "" {
			cli += " " + op.CLI.Positional
		}
		tool := ""
		if op.MCP != nil {
			tool = op.MCP.Tool
		}
		got := want{op.Kind, method + " " + strings.TrimPrefix(path, "/v1"), op.Mode, op.Risk, op.Scope.Name, cli, tool,
			op.Idempotent, op.Audited}
		if got != w {
			t.Errorf("%s:\n got %+v\nwant %+v", op.ID, got, w)
		}
		if !op.Untrusted {
			t.Errorf("%s: not Untrusted", op.ID)
		}
		if op.Scope.PerTarget != "" || op.Scope.FromTarget {
			t.Errorf("%s: unexpected scope %+v", op.ID, op.Scope)
		}
		reboot := op.ID == "services.actions.reboot"
		if op.WhatIf != reboot || op.CompletesAcrossReboot != reboot || (op.Cancel == def.CancelRunning) != reboot {
			t.Errorf("%s: WhatIf %v, CompletesAcrossReboot %v, Cancel %q", op.ID, op.WhatIf, op.CompletesAcrossReboot, op.Cancel)
		}
		if reboot {
			if op.CLI.ReconnectWindow != 10*time.Minute || op.CLI.TypedConfirmation != "confirmHost" || op.MCP != nil {
				t.Errorf("reboot: ReconnectWindow %v, TypedConfirmation %q, MCP %+v",
					op.CLI.ReconnectWindow, op.CLI.TypedConfirmation, op.MCP)
			}
		} else if op.CLI.ReconnectWindow != 0 || op.CLI.TypedConfirmation != "" {
			t.Errorf("%s: CLI %+v", op.ID, op.CLI)
		}
	}
	if spec.Name != "services" || spec.ReadScope != "services:read" {
		t.Errorf("provider spec %q read scope %q", spec.Name, spec.ReadScope)
	}
	wantScopes := []string{"services:logs", "services:read", "services:reboot", "services:write"}
	if got := slices.Sorted(maps.Keys(scopes)); !slices.Equal(got, wantScopes) {
		t.Errorf("scopes %v, want %v", got, wantScopes)
	}
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
	verbs := map[string][]string{}
	for _, op := range catalog.Ops() {
		if !strings.HasPrefix(op.ID, "services.") {
			continue
		}
		if !def.IsApprovedVerb(op.CLI.Verb) {
			t.Errorf("%s: unapproved verb %q", op.ID, op.CLI.Verb)
		}
		if !slices.Contains(verbs[op.CLI.Noun], op.CLI.Verb) {
			verbs[op.CLI.Noun] = append(verbs[op.CLI.Noun], op.CLI.Verb)
		}
		if op.ID == "services.services.control" {
			r, err := op.InputSchema()
			if err != nil {
				t.Fatal(err)
			}
			p := r.Schema().Properties["action"]
			if p == nil || !slices.Equal(p.Enum, []any{"start", "stop", "restart"}) {
				t.Errorf("control's action is not the enum start, stop, restart: %+v", p)
			}
		}
	}
	for noun := range verbs {
		slices.Sort(verbs[noun])
	}
	want := map[string][]string{
		"service":    {"get", "invoke", "restart", "start", "stop"},
		"servicelog": {"get"},
		"machine":    {"restart"},
	}
	if !maps.EqualFunc(verbs, want, slices.Equal) {
		t.Errorf("nouns and verbs %v, want %v", verbs, want)
	}

	// No new verb: verbs.go is unchanged from the feature's base.
	const base = "43e5792"
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found: cannot compare verbs.go with base " + base)
	}
	if err := exec.Command("git", "cat-file", "-e", base+"^{commit}").Run(); err != nil {
		t.Skipf("base commit %s not available: %v", base, err)
	}
	baseSrc, err := exec.Command("git", "show", base+":internal/def/verbs.go").Output()
	if err != nil {
		t.Fatalf("git show %s:internal/def/verbs.go: %v", base, err)
	}
	headSrc, err := os.ReadFile("../def/verbs.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(baseSrc) != string(headSrc) {
		t.Error("internal/def/verbs.go changed since base: no new verb may be added")
	}
}
