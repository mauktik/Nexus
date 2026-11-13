package mcpadapter_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/catalog"
	nexusd "github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// toolBudget is the documented estimate (docs/mcp.md, Tool budget): the sum
// of the character counts of every tool's name and description, divided by
// four and rounded up.
func toolBudget(tools []*mcp.Tool) (chars, tokens int) {
	for _, tool := range tools {
		chars += utf8.RuneCountInString(tool.Name) + utf8.RuneCountInString(tool.Description)
	}
	return chars, (chars + 3) / 4
}

// budgetLimit is NFR-8's target for the initial three providers.
const budgetLimit = 4000

// catalogTools lists, as the real SDK client receives them, the tools of
// every enabled catalog spec. It fails when a catalog tool is not listed,
// which would make a budget total low.
func catalogTools(t *testing.T) []*mcp.Tool {
	t.Helper()
	w, _, _ := catalogWorld(t)
	cs := w.session(t, w.full, nil)
	inCatalog := map[string]bool{}
	for _, op := range catalog.Ops() {
		if op.MCP != nil {
			inCatalog[op.MCP.Tool] = true
		}
	}
	var tools []*mcp.Tool
	for _, tool := range listTools(t, cs) {
		if inCatalog[tool.Name] {
			tools = append(tools, tool)
			delete(inCatalog, tool.Name)
		}
	}
	if len(inCatalog) > 0 {
		t.Fatalf("catalog tools not listed, so the total would be low: %v", inCatalog)
	}
	return tools
}

// TC-151: the tool budget, measured on the live tool list of every
// enabled catalog spec.
func TestToolBudget(t *testing.T) {
	tools := catalogTools(t)

	// Step 1.
	chars, tokens := toolBudget(tools)
	t.Logf("%d tools, %d characters, ceil(%d/4) = %d tokens (limit %d)", len(tools), chars, chars, tokens, budgetLimit)
	if tokens >= budgetLimit {
		t.Errorf("tool names and descriptions estimate %d tokens, want under %d", tokens, budgetLimit)
	}

	// The control: a planted long description fails the same check.
	planted := append(tools, &mcp.Tool{Name: "planted_tool", Description: strings.Repeat("x", 4*budgetLimit)})
	if _, n := toolBudget(planted); n < budgetLimit {
		t.Errorf("planted description estimates %d tokens, the check would not catch it", n)
	}
	if _, n := toolBudget([]*mcp.Tool{{Name: "ab", Description: "cde"}}); n != 2 {
		t.Errorf("ceil(5/4) = %d, want 2", n)
	}

	// The method is documented.
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatalf("docs/mcp.md: %v", err)
	}
	for _, want := range []string{"ceil(", "/ 4", "4,000", "TestToolBudget"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("docs/mcp.md does not document the budget method: lacks %q", want)
		}
	}
}

// nexusdTools are the only nexusd ops exposed over MCP (FR-17): both
// read-only. Restart, update, rollback, reload and the upload are not tools
// (OOS-8).
var nexusdTools = []string{"get_nexusd", "validate_nexusd_config"}

// TC-SM-96 (WI 6.1; NFR-3): the tool budget stays under 4,000 tokens with
// nexusd, which adds fewer than 100 tokens and exactly two tools.
func TestTCSM96_TheToolBudgetStaysUnder4000(t *testing.T) {
	tools := catalogTools(t)
	without := slices.DeleteFunc(slices.Clone(tools), func(tool *mcp.Tool) bool {
		return slices.Contains(nexusdTools, tool.Name)
	})
	_, total := toolBudget(tools)
	_, base := toolBudget(without)
	t.Logf("%d tools, %d tokens; without nexusd %d tools, %d tokens; delta %d", len(tools), total, len(without),
		base, total-base)
	if total >= budgetLimit {
		t.Errorf("budget %d tokens, want under %d", total, budgetLimit)
	}
	if d := total - base; d <= 0 || d >= 100 {
		t.Errorf("nexusd adds %d tokens, want 1 to 99", d)
	}
	if len(tools) != 26 || len(tools)-len(without) != 2 {
		t.Errorf("%d tools, %d of them nexusd; want 26 with 2", len(tools), len(tools)-len(without))
	}
}

// TC-SM-95 (WI 6.1; FR-17, OOS-8): MCP lists exactly two read-only nexusd
// tools, and no other nexusd op is reachable as a tool.
func TestTCSM95_MCPListsExactlyTwoReadOnly(t *testing.T) {
	w, _, _ := catalogWorld(t)
	cs := w.session(t, w.full, nil)

	// Step 1: the nexusd tools are exactly the two reads.
	tools := listTools(t, cs)
	nexusdOps := map[string]bool{}
	for _, op := range nexusd.Spec().Ops {
		if op.MCP != nil {
			nexusdOps[op.MCP.Tool] = true
		}
	}
	var got []string
	for _, tool := range tools {
		if !nexusdOps[tool.Name] && !strings.Contains(tool.Name, "nexusd") {
			continue
		}
		got = append(got, tool.Name)
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint {
			t.Errorf("%s: annotations %+v, want readOnlyHint true", tool.Name, a)
		} else if a.DestructiveHint != nil && *a.DestructiveHint {
			t.Errorf("%s: destructiveHint true on a read-only tool", tool.Name)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, nexusdTools) {
		t.Errorf("nexusd tools %v, want %v", got, nexusdTools)
	}

	// Step 2: no tool name or schema names a nexusd lane op or the upload.
	b, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	all := string(b)
	for _, op := range nexusd.Spec().Ops {
		if op.MCP == nil && strings.Contains(all, op.ID) {
			t.Errorf("the tool list mentions %s, which is not a tool", op.ID)
		}
	}
	for _, tool := range tools {
		if !strings.Contains(tool.Name, "nexusd") {
			continue
		}
		for _, verb := range []string{"reload", "restart", "update", "rollback", "upload"} {
			if strings.Contains(tool.Name, verb) {
				t.Errorf("tool %s names %q", tool.Name, verb)
			}
		}
	}
	if strings.Contains(all, "/providers/nexusd/uploads") || strings.Contains(all, "nexusd.uploads") {
		t.Error("the tool list mentions the nexusd upload")
	}

	// Step 3: a made-up nexusd_update is the unknown-tool error.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "nexusd_update", Arguments: map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), `unknown tool "nexusd_update"`) {
		t.Errorf("tools/call nexusd_update: result %+v error %v, want the unknown-tool error", res, err)
	}
}
