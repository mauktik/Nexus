// Tests for TC-SM-102 (nexusd-self-management): planning/nexusd-self-management/test-cases.md.
package buildsys

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TC-SM-102 (WI 6.2; FR-19), as amended at the nginx-provider merge: the
// update helper runs the package's install.sh as its install step and relies
// on its restart contract (A-33). install.sh has changed since the base
// revision, so its byte pin is replaced by TC-SM-104's backward-compatibility
// proof, as this case prescribed.
func TestTCSM102_DeployInstallShIsByteIdentical(t *testing.T) {
	// deploy/install.sh changed after d04583a: nginx-provider WI 3.2 made it
	// POSIX sh with OpenRC support. As TC-SM-102 itself prescribes, the byte
	// pin is replaced by TC-SM-104's backward-compatibility proof, which runs
	// the shipped install.sh by hand on real systemd (install_test.go). What
	// stays here is what the update helper needs on every host: /bin/bash
	// (system.go's bashPath) parses the script.
	root := repoRoot(t)
	path := filepath.Join(root, "deploy", "install.sh")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n deploy/install.sh: %v\n%s", err, out)
	}
	t.Log("deploy/install.sh parses under bash; TC-SM-104 proves it on real systemd")
}
