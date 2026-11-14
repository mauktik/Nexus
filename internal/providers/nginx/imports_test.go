// Stub-style: SKIP-only (nginx-provider)
//
// Stub of planning/nginx-provider/test-cases.md TC-103. The body is
// complete: the developer of WI 6.3 deletes the t.Skip line. The review of
// each exec.Command call's first argument (never read from input) stays a
// reviewer check. An un-skipped passing test is the GREEN signal.

package nginx

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nginx-provider TC-103: The nginx and svcctl packages have no network
// client and no new dependency. Owner WI 6.3. Requirements: CON-1, CON-4,
// CON-7.
func TestNoNetworkClientOrNewDeps(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	list := exec.Command("go", "list", "-json", "./internal/providers/nginx/...", "./internal/svcctl/...")
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	forbidden := []string{"net/http", "net/rpc", "golang.org/x/crypto/ssh"}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p struct {
			ImportPath string
			Imports    []string
		}
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		for _, imp := range p.Imports {
			for _, bad := range forbidden {
				if imp == bad || strings.HasPrefix(imp, bad+"/") {
					t.Errorf("%s imports %s (CON-1: no network client)", p.ImportPath, imp)
				}
			}
		}
	}
	// CON-7: the only go.mod change since the base SHA is x/sys going direct.
	diff := exec.Command("git", "diff", "d04583a2d7b29f7b28f603cdb7a88812d8f1ecb3", "--", "go.mod")
	diff.Dir = root
	d, err := diff.Output()
	if err != nil {
		t.Skipf("git diff against the base SHA: %v", err)
	}
	for _, l := range strings.Split(string(d), "\n") {
		if (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")) && !strings.HasPrefix(l, "+++") && !strings.HasPrefix(l, "---") {
			if !strings.Contains(l, "golang.org/x/sys") && strings.TrimSpace(l[1:]) != "" && strings.TrimSpace(l[1:]) != ")" && strings.TrimSpace(l[1:]) != "require (" {
				t.Errorf("unexpected go.mod change: %s", l)
			}
		}
	}
}
