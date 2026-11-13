// Tests for TC-SM-102 (nexusd-self-management): planning/nexusd-self-management/test-cases.md.
package buildsys

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TC-SM-102 (WI 6.2; FR-19): deploy/install.sh is byte-identical to the base
// revision. The update helper runs the package's install.sh as its install
// step and relies on its restart contract (A-33), so the script must not
// drift. If it is ever changed, this case is replaced by TC-SM-104's
// backward-compatibility proof.
func TestTCSM102_DeployInstallShIsByteIdentical(t *testing.T) {
	// The sha256 of d04583a:deploy/install.sh, pinned so the check also runs in
	// the .git-less repo copy that TestMakeCheckIsTheGate builds. Recompute with
	// `git show d04583a:deploy/install.sh | shasum -a 256`.
	const base, baseSHA256 = "d04583a", "a85a4d8d4669a074ee3a925856b4185a66cee6d7759e55a4bafacef6e6b6ea34"
	root := repoRoot(t)
	got, err := os.ReadFile(filepath.Join(root, "deploy", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != baseSHA256 {
		t.Fatalf("deploy/install.sh differs from %s (sha256 %x, want %s): FR-19 keeps it unchanged; "+
			"a change needs TC-SM-104's backward-compatibility proof in place of this case", base, sum, baseSHA256)
	}
	t.Logf("deploy/install.sh equals %s:deploy/install.sh (%d bytes)", base, len(got))
}
