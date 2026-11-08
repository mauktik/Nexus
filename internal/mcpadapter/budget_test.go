package mcpadapter_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/catalog"
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

// TC-151: the tool budget, measured on the live tool list of every
// enabled catalog spec.
func TestToolBudget(t *testing.T) {
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
