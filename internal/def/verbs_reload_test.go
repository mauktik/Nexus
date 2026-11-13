// Stub-style: SKIP-only (nginx-provider)
//
// Stub of planning/nginx-provider/test-cases.md TC-53. The body is complete:
// the developer of WI 4.1 deletes the t.Skip line and also updates
// TestApprovedVerbsAreExact (verbs_test.go) to list reload and to refuse
// refresh and reconfigure. An un-skipped passing test is the GREEN signal.

package def

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nginx-provider TC-53: reload is an approved verb and its synonyms are not.
// Owner WI 4.1. Requirements: FR-6, FR-16, ASM-4.
func TestReloadIsApprovedAndSynonymsAreNot(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-53 (WI 4.1)")

	if !IsApprovedVerb("reload") {
		t.Error(`IsApprovedVerb("reload") = false, want true (user decision 2026-11-13)`)
	}
	for _, v := range []string{"refresh", "reconfigure", "Reload", "enable", "disable"} {
		if IsApprovedVerb(v) {
			t.Errorf("IsApprovedVerb(%q) = true, want false", v)
		}
	}
	if n := len(ApprovedVerbs()); n != 12 {
		t.Errorf("len(ApprovedVerbs()) = %d, want 12", n)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "adding-a-provider.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "`reload`") {
		t.Error("docs/adding-a-provider.md does not list the reload verb")
	}
}
