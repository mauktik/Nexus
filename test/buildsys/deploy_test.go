// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package buildsys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeployScript covers ./deploy.sh, which builds nexusd from a checkout and
// installs it with deploy/install.sh. The install itself needs root and
// systemd and was exercised in the T1 harness image; here --stage-only checks
// that the staged tree is the one the release tarball carries, and the
// argument and guard paths are checked without installing anything.
func TestDeployScript(t *testing.T) {
	root := repoRoot(t)

	t.Run("stage-only builds the release tree for linux/amd64", func(t *testing.T) {
		dir := copyRepo(t)
		stage := filepath.Join(t.TempDir(), "stage")
		out, code := run(t, dir, nil, "./deploy.sh", "--stage-only", stage)
		if code != 0 {
			t.Fatalf("deploy.sh --stage-only exited %d:\n%s", code, out)
		}
		for _, f := range []string{"nexusd", "install.sh", "uninstall.sh", "nexusd.service", "nexusd.openrc", "nexusd.default.yaml", "README.md", "docs/install.md"} {
			if _, err := os.Stat(filepath.Join(stage, f)); err != nil {
				t.Errorf("staged tree lacks %s: %v", f, err)
			}
		}
		for _, f := range []string{"nexusd.service", "nexusd.openrc", "nexusd.default.yaml", "install.sh", "uninstall.sh"} {
			want, err := os.ReadFile(filepath.Join(root, "deploy", f))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(stage, f))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("staged %s differs from deploy/%s", f, f)
			}
		}
		for _, f := range []string{"nexusd", "nexusd.openrc", "install.sh", "uninstall.sh"} {
			fi, err := os.Stat(filepath.Join(stage, f))
			if err == nil && fi.Mode().Perm() != 0o755 {
				t.Errorf("staged %s has mode %v, want 0755", f, fi.Mode().Perm())
			}
		}
		// FR-38: the staged nexusd is a static x86-64 ELF, with no PT_INTERP
		// and no DT_NEEDED, so it runs on musl hosts as well as glibc ones.
		checkStaticELF(t, filepath.Join(stage, "nexusd"), "amd64")
	})

	t.Run("bad arguments print usage and exit 2", func(t *testing.T) {
		for _, args := range [][]string{{"--bogus"}, {"--stage-only"}, {"--uninstall", "--nope"}} {
			out, code := run(t, root, nil, "./deploy.sh", args...)
			if code != 2 || !strings.Contains(out, "usage:") {
				t.Errorf("deploy.sh %v: exit %d, output %q; want exit 2 and usage", args, code, out)
			}
		}
	})

	t.Run("install refuses to run as root", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("needs root to observe the refusal; covered in the T1 image")
		}
		out, code := run(t, root, nil, "./deploy.sh")
		if code == 0 || !strings.Contains(out, "normal user") {
			t.Errorf("deploy.sh as root: exit %d, output %q; want a refusal", code, out)
		}
	})
}
