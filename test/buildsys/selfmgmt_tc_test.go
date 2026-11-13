// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package buildsys

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TC-SM-102 (WI 6.2; FR-19): deploy/install.sh is byte-identical to the base
// revision. The update helper runs the package's install.sh as its install
// step and relies on its restart contract (A-33), so the script must not
// drift. If it is ever changed, this case is replaced by TC-SM-104's
// backward-compatibility proof.
func TestTCSM102_DeployInstallShIsByteIdentical(t *testing.T) {
	const base = "d04583a"
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for TC-SM-102: %v", err)
	}
	root := repoRoot(t)
	cmd := exec.Command("git", "-C", root, "show", base+":deploy/install.sh")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	want, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show %s:deploy/install.sh: %v %s", base, err, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "deploy", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("deploy/install.sh differs from %s (%d bytes, base %d bytes): FR-19 keeps it unchanged; "+
			"a change needs TC-SM-104's backward-compatibility proof in place of this case", base, len(got), len(want))
	}
	t.Logf("deploy/install.sh equals %s:deploy/install.sh (%d bytes)", base, len(got))
}
