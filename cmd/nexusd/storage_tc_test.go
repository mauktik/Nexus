// Tests of planning/storage-provider/test-cases.md TC-STG-184 (owner WI
// 4.5): the production wiring of the storage helper in the nexusd binary
// that TestMain builds.

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TC-STG-184: production wiring: nexusd storage-helper prints the helper's usage. Owner WI 4.5. Requirements: FR-13, FR-14, CON-1.
func TestTCSTG184_ProductionWiringNexusdStorageHelperPrintsThe(t *testing.T) {
	out, _ := runNexusd(t, "storage-helper", "--help")
	if !strings.Contains(out, "storage-helper run --dir") || strings.Contains(out, "validate-config") {
		t.Errorf("storage-helper --help printed %q, want the helper's usage", out)
	}

	missing := filepath.Join(t.TempDir(), "ops", "op-0123456789abcdef")
	out, code := runNexusd(t, "storage-helper", "run", "--dir", missing)
	if code == 0 || !strings.Contains(out, "input.json") {
		t.Errorf("storage-helper run --dir <missing> exited %d with %q, want non-zero naming input.json", code, out)
	}

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`case storageprov\.HelperCommand:\n\t+// Hidden: `).Match(src) {
		t.Error("main.go: the storage helper's case has no Hidden: comment like the other helper cases")
	}
}
