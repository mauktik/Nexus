package detach

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TC-67: on Linux with a fixture layout the helper reports available only
// when the host was booted with systemd and both tools are executable; any
// gap is unavailable with a reason, never a silent downgrade.
func TestSystemdAvailabilityFixtureLayout(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, mode os.FileMode) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	check := func(goos string, wantOK bool, wantReason string) {
		t.Helper()
		a := systemdAt(goos, root)
		if a.Available != wantOK || (!wantOK && !strings.Contains(a.Reason, wantReason)) {
			t.Fatalf("%s: %+v, want available %v with reason containing %q", goos, a, wantOK, wantReason)
		}
	}
	check("linux", false, "/run/systemd/system")
	if err := os.MkdirAll(filepath.Join(root, "run", "systemd", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	check("linux", false, SystemdRun)
	mk(SystemdRun, 0o755)
	check("linux", false, Systemctl)
	mk(Systemctl, 0o644)
	check("linux", false, Systemctl)
	if err := os.Chmod(filepath.Join(root, Systemctl), 0o755); err != nil {
		t.Fatal(err)
	}
	check("linux", true, "")
	check("darwin", false, "darwin")
}
