package apt

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

var wantEnv = []string{"LC_ALL=C", "LANG=C", "DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"}

// TC-122 step 1: no non-test Go under internal/ reads the release (D-7).
func TestNoReleaseSpecificLogic(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "providers", "apt", "exec.go")); err != nil {
		t.Fatalf("internal/ not found from the test's directory: %v", err)
	}
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, banned := range []string{"os-release", "lsb_release", "VERSION_CODENAME"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s mentions %s", path, banned)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files", scanned)
	}
}

// TC-122 step 2: every command is a fixed absolute binary, names follow
// "--", and the environment is exactly the four variables.
func TestCommandsUseFixedBinariesAndExactEnv(t *testing.T) {
	cmds := map[string]command{
		"dpkg-query all": cmdDpkgQueryAll(),
		"dpkg-query one": cmdDpkgQuery("nxplain"),
		"architecture":   cmdArchitecture(),
		"pkgnames":       cmdPkgNames("nxplain"),
		"policy":         cmdPolicy("nxplain", "curl"),
		"show":           cmdShow("nxplain"),
		"simulate":       cmdSimulate(),
		"refresh":        {Argv: RefreshArgv()},
	}
	for name, c := range cmds {
		if !filepath.IsAbs(c.Argv[0]) || !slices.Contains(Binaries, c.Argv[0]) {
			t.Errorf("%s: binary %q is not one of the fixed absolute paths", name, c.Argv[0])
		}
		for i, a := range c.Argv {
			if (a == "nxplain" || a == "curl") && !slices.Contains(c.Argv[:i], "--") {
				t.Errorf("%s: name %q is not after --: %q", name, a, c.Argv)
			}
		}
	}
	if !slices.Equal(Env(), wantEnv) {
		t.Errorf("Env() = %q, want %q", Env(), wantEnv)
	}

	// The real runner gives a child exactly that environment.
	envBin := "/usr/bin/env"
	if _, err := os.Stat(envBin); err != nil {
		t.Skipf("no %s: %v", envBin, err)
	}
	t.Setenv("NEXUS_LEAK_CHECK", "leaked")
	res, err := execRun(context.Background(), command{Argv: []string{envBin}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("env: %v %+v", err, res)
	}
	if got := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n"); !slices.Equal(got, wantEnv) {
		t.Errorf("child environment %q, want exactly %q", got, wantEnv)
	}

	// The refresh unit gets the same environment, and KillMode=process.
	p := New(Options{})
	s, err := p.prepareRefresh(context.Background(), spec.RefreshInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Env, wantEnv) || !slices.Equal(s.Argv, []string{AptGet, "-o", "APT::Color=0", "update"}) ||
		s.KillMode != detach.KillProcess || s.MaxRuntime != p.Settings().MaxRuntime || s.Credential != nil {
		t.Errorf("refresh spec %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("refresh spec: %v", err)
	}
}

func TestExecRunBoundsOutputAndReportsExit(t *testing.T) {
	sh := "/bin/sh"
	if _, err := os.Stat(sh); err != nil {
		t.Skip("no /bin/sh")
	}
	res, err := execRun(context.Background(), command{Argv: []string{sh, "-c", "echo out; echo err >&2; exit 3"}})
	if err != nil || res.ExitCode != 3 || string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" {
		t.Fatalf("exit 3: %v %+v", err, res)
	}
	if _, err := execRun(context.Background(), command{Argv: []string{"/nonexistent/apt-get"}}); err == nil {
		t.Fatal("a missing binary ran")
	}
}
