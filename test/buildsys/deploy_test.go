package buildsys

import (
	"debug/elf"
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
		for _, f := range []string{"nexusd", "install.sh", "uninstall.sh", "nexusd.service", "nexusd.default.yaml", "README.md", "docs/install.md"} {
			if _, err := os.Stat(filepath.Join(stage, f)); err != nil {
				t.Errorf("staged tree lacks %s: %v", f, err)
			}
		}
		for _, f := range []string{"nexusd.service", "nexusd.default.yaml", "install.sh", "uninstall.sh"} {
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
		for _, f := range []string{"nexusd", "install.sh", "uninstall.sh"} {
			fi, err := os.Stat(filepath.Join(stage, f))
			if err == nil && fi.Mode().Perm() != 0o755 {
				t.Errorf("staged %s has mode %v, want 0755", f, fi.Mode().Perm())
			}
		}
		bin, err := elf.Open(filepath.Join(stage, "nexusd"))
		if err != nil {
			t.Fatalf("staged nexusd is not an ELF binary: %v", err)
		}
		defer bin.Close()
		if bin.Machine != elf.EM_X86_64 {
			t.Errorf("staged nexusd is built for %v, want x86-64", bin.Machine)
		}
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
