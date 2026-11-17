// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-42, TC-RA-69.
// Owner WIs: 2.4, 4.1. TC-RA-42 is implemented (WI 2.4), TC-RA-69 (WI 4.1). Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package mcpadapter_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/conformance/testprovider"
	"github.com/mauktik/nexus/internal/def"
	nexusd "github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// TC-RA-42: No MCP tool exists for any token op, and the tool budget is unchanged. Owner WI 2.4. Requirements: FR-10, OOS-1.
//
// baseTools, the tool set at 521261c, is computed from the catalog with the
// three token op IDs filtered out. TestToolBudget and TestTCSM96 (exactly
// two nexusd tools) run unchanged beside it.
func TestTCRA42_NoMCPToolExistsForAnyTokenOpAndTheToolBudgetIs(t *testing.T) {
	tokenOps := []string{nexusd.TokensList.Op().ID, nexusd.TokensAdd.Op().ID, nexusd.TokensRemove.Op().ID}
	baseTools := map[string]bool{}
	for _, op := range catalog.Ops() {
		if op.MCP != nil && !slices.Contains(tokenOps, op.ID) {
			baseTools[op.MCP.Tool] = true
		}
	}
	// The tools of the test providers catalogWorld adds besides the catalog.
	w, j, _ := catalogWorld(t)
	extra := map[string]bool{}
	for _, op := range append(testprovider.Spec().Ops, j.Spec().Ops...) {
		if op.MCP != nil {
			extra[op.MCP.Tool] = true
		}
	}

	// Step 1.
	tools := listTools(t, w.session(t, w.full, nil))
	byTool := opsByTool()
	got := map[string]bool{}
	for _, tool := range tools {
		if op, ok := byTool[tool.Name]; ok && strings.HasPrefix(op.ID, "nexusd.tokens.") {
			t.Errorf("tool %s maps to %s", tool.Name, op.ID)
		}
		if strings.Contains(strings.ToLower(tool.Name), "token") {
			t.Errorf("tool %s names a token", tool.Name)
		}
		if !extra[tool.Name] {
			got[tool.Name] = true
		}
	}
	b, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range tokenOps {
		if strings.Contains(string(b), id) {
			t.Errorf("the tool list mentions %s", id)
		}
	}
	if !maps.Equal(got, baseTools) {
		t.Errorf("tool set %v, want baseTools %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(baseTools)))
	}
	// Non-vacuity: the token ops are in the catalog, none with an MCP binding.
	for _, id := range tokenOps {
		i := slices.IndexFunc(catalog.Ops(), func(op def.Op) bool { return op.ID == id })
		if i < 0 || catalog.Ops()[i].MCP != nil {
			t.Errorf("catalog op %s: index %d; want an op without a tool", id, i)
		}
	}
}

// TC-RA-69: No MCP tool exists for any of the six new ops. Owner WI 4.1. Requirements: FR-10, OOS-1.
//
//   - Setup: The real catalog adapter, after 4.1.
//   - Steps:
//     1. `tools/list`.
//     2. Count the nexusd tools.
//   - Expected:
//   - No tool maps to `nexusd.tokens.*`, `nexusd.config.*` or `nexusd.actions.update-config`.
//   - The tool count equals the count at `521261c` (computed by filtering the six new IDs from the catalog).
//   - `TestTCSM96` still sees exactly two nexusd tools.
func TestTCRA69_NoMCPToolExistsForAnyOfTheSixNewOps(t *testing.T) {
	newOps := []string{nexusd.TokensList.Op().ID, nexusd.TokensAdd.Op().ID, nexusd.TokensRemove.Op().ID,
		nexusd.ConfigList.Op().ID, nexusd.ConfigGet.Op().ID, nexusd.UpdateConfig.Op().ID}
	// Non-vacuity: all six are in the catalog, none with an MCP binding.
	for _, id := range newOps {
		i := slices.IndexFunc(catalog.Ops(), func(op def.Op) bool { return op.ID == id })
		if i < 0 || catalog.Ops()[i].MCP != nil {
			t.Errorf("catalog op %s: index %d; want an op without a tool", id, i)
		}
	}
	// baseTools is the tool set at 521261c: the catalog's tools without the
	// six new op IDs.
	baseTools := map[string]bool{}
	for _, op := range catalog.Ops() {
		if op.MCP != nil && !slices.Contains(newOps, op.ID) {
			baseTools[op.MCP.Tool] = true
		}
	}
	w, j, _ := catalogWorld(t)
	extra := map[string]bool{}
	for _, op := range append(testprovider.Spec().Ops, j.Spec().Ops...) {
		if op.MCP != nil {
			extra[op.MCP.Tool] = true
		}
	}

	// Step 1.
	tools := listTools(t, w.session(t, w.full, nil))
	byTool := opsByTool()
	got := map[string]bool{}
	nexusdTools := 0
	for _, tool := range tools {
		op, ok := byTool[tool.Name]
		if ok && (strings.HasPrefix(op.ID, "nexusd.tokens.") || strings.HasPrefix(op.ID, "nexusd.config.") ||
			op.ID == nexusd.UpdateConfig.Op().ID) {
			t.Errorf("tool %s maps to %s", tool.Name, op.ID)
		}
		// Step 2.
		if ok && op.Provider == nexusd.Name {
			nexusdTools++
		}
		if !extra[tool.Name] {
			got[tool.Name] = true
		}
	}
	b, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range newOps {
		if strings.Contains(string(b), id) {
			t.Errorf("the tool list mentions %s", id)
		}
	}
	if !maps.Equal(got, baseTools) {
		t.Errorf("tool set %v, want baseTools %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(baseTools)))
	}
	// TestTCSM96_TheToolBudgetStaysUnder4000 pins the same two, by name.
	if nexusdTools != 2 {
		t.Errorf("%d nexusd tools, want 2", nexusdTools)
	}
}
