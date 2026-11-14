// TC-103 of planning/nginx-provider/test-cases.md. The review of each
// exec.Command call's first argument (never read from input) stays a
// reviewer check.

package nginx

import (
	"encoding/json"
	"os"
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
	// CON-7: no module beyond the checked-in allowlist (testdata/allowed-modules.txt).
	// The test fails, never skips, when it cannot list them.
	deps := exec.Command("go", "list", "-deps", "-test", "-f", "{{if and .Module (not .Standard)}}{{.Module.Path}}{{end}}",
		"./internal/providers/nginx/...", "./internal/svcctl/...")
	deps.Dir = root
	d, err := deps.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	allowed := map[string]bool{}
	b, err := os.ReadFile(filepath.Join("testdata", "allowed-modules.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			allowed[line] = true
		}
	}
	if len(allowed) == 0 {
		t.Fatal("testdata/allowed-modules.txt lists no module")
	}
	for mod := range strings.Lines(string(d)) {
		if mod = strings.TrimSpace(mod); mod != "" && !allowed[mod] {
			t.Errorf("the nginx and svcctl packages depend on %s, which is not in testdata/allowed-modules.txt "+
				"(CON-7: no new dependency): add it there only if the dependency is intended", mod)
		}
	}
	// And golang.org/x/sys, the one requirement the feature added, is a direct one.
	mods := exec.Command("go", "list", "-m", "-f", "{{.Indirect}}", "golang.org/x/sys")
	mods.Dir = root
	if out, err := mods.Output(); err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Errorf("go list -m golang.org/x/sys: Indirect %q, %v; want false (a direct requirement)", out, err)
	}
}
