package scripts

import (
	"os"
	"path/filepath"
	"testing"
)

// SEC-1: a path owned by someone other than root is refused, whether it is
// the file, or the directory that holds it.
func TestCheckRootOwnedRefusesNonRootOwners(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("files made by root are root-owned")
	}
	base := cleanBase(t)
	f := script(t, filepath.Join(base, "compose.yaml"), 0o644)
	refusal(t, CheckRootOwned(f, false), CheckScriptOwner, f, "chown root "+f)
	refusal(t, CheckRootOwned(base, true), CheckDirectoryOwner, base, "chown root")
	l := symlink(t, f, filepath.Join(base, "link.yaml"))
	refusal(t, CheckRootOwned(l, false), CheckScriptOwner, f)
	if err := CheckRootOwned(filepath.Join(base, "absent.yaml"), false); err == nil {
		t.Error("a missing file passed")
	}
}

// A root-owned file in a root-owned tree passes: the control for the above.
func TestCheckRootOwnedAcceptsRootOwnedPaths(t *testing.T) {
	if err := CheckRootOwned("/etc/hosts", false); err != nil {
		t.Skipf("this host's /etc/hosts is not a clean root-owned file: %v", err)
	}
	if err := CheckRootOwned("/etc", true); err != nil {
		t.Errorf("/etc: %v", err)
	}
}
