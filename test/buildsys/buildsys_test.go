// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package buildsys

import (
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TC-1: module identity and stub binaries.
func TestModuleIdentityAndStubBinaries(t *testing.T) {
	root := repoRoot(t)

	out, code := run(t, root, nil, "go", "list", "-m")
	if code != 0 {
		t.Fatalf("go list -m exited %d: %s", code, out)
	}
	if got := strings.TrimSpace(out); got != modulePath {
		t.Errorf("module path = %q, want %q", got, modulePath)
	}

	out, code = run(t, root, nil, "go", "mod", "edit", "-json")
	if code != 0 {
		t.Fatalf("go mod edit -json exited %d: %s", code, out)
	}
	var mod struct{ Go string }
	if err := json.Unmarshal([]byte(out), &mod); err != nil {
		t.Fatalf("decode go mod edit -json: %v", err)
	}
	if !regexp.MustCompile(`^1\.26(\.\d+)?$`).MatchString(mod.Go) {
		t.Errorf("go directive = %q, want 1.26 or 1.26.x", mod.Go)
	}

	const ldflag = "-X " + modulePath + "/internal/version.Version=v9.9.9"
	for _, name := range []string{"nexusd", "nexus"} {
		t.Run(name, func(t *testing.T) {
			pkg := "./cmd/" + name

			out, code := run(t, root, nil, "go", "run", "-ldflags", ldflag, pkg, "--version")
			if code != 0 {
				t.Fatalf("go run %s --version (with ldflag) exited %d: %s", pkg, code, out)
			}
			if got, want := strings.TrimSpace(out), name+" v9.9.9"; got != want {
				t.Errorf("version output with ldflag = %q, want %q", got, want)
			}

			out, code = run(t, root, nil, "go", "run", pkg, "--version")
			if code != 0 {
				t.Fatalf("go run %s --version exited %d: %s", pkg, code, out)
			}
			fields := strings.Fields(out)
			if len(fields) != 2 || fields[0] != name || fields[1] == "" {
				t.Errorf("version output without ldflag = %q, want %q followed by a non-empty development version", out, name)
			}
		})
	}
}

// TC-2: the release build produces exactly the required static artifacts.
func TestBuildReleaseArtifacts(t *testing.T) {
	skipUnderMake(t)
	dir := copyRepo(t)
	dist := filepath.Join(dir, "dist")
	if err := os.RemoveAll(dist); err != nil {
		t.Fatalf("empty dist: %v", err)
	}

	out, code := run(t, dir, envWithout(makeGuardEnv), "make", "build-release", "VERSION=v0.0.0-tc2")
	if code != 0 {
		t.Fatalf("make build-release exited %d:\n%s", code, out)
	}

	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatalf("read dist: %v", err)
	}

	type target struct{ pkg, goos, goarch string }
	want := []target{
		{modulePath + "/cmd/nexus", "darwin", "arm64"},
		{modulePath + "/cmd/nexus", "linux", "amd64"},
		{modulePath + "/cmd/nexusd", "linux", "amd64"},
	}
	var got []target
	const maxSize = 40 << 20

	for _, e := range entries {
		path := filepath.Join(dist, e.Name())
		if !e.Type().IsRegular() {
			t.Errorf("dist/%s is not a regular file", e.Name())
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat dist/%s: %v", e.Name(), err)
		}
		if info.Size() >= maxSize {
			t.Errorf("dist/%s is %d bytes, want under 40 MiB", e.Name(), info.Size())
		}

		bi, err := buildinfo.ReadFile(path)
		if err != nil {
			t.Errorf("dist/%s: read build info: %v", e.Name(), err)
			continue
		}
		settings := map[string]string{}
		for _, s := range bi.Settings {
			settings[s.Key] = s.Value
		}
		if settings["CGO_ENABLED"] != "0" {
			t.Errorf("dist/%s: CGO_ENABLED = %q, want 0", e.Name(), settings["CGO_ENABLED"])
		}
		if settings["-trimpath"] != "true" {
			t.Errorf("dist/%s: -trimpath = %q, want true", e.Name(), settings["-trimpath"])
		}
		// Go omits -ldflags from build info under -trimpath, so -s -w is
		// checked from the object file sections in checkStaticELF/checkMachO.
		tg := target{bi.Path, settings["GOOS"], settings["GOARCH"]}
		got = append(got, tg)

		switch tg.goos {
		case "linux":
			checkStaticELF(t, path, tg.goarch)
		case "darwin":
			checkMachO(t, path, tg.goarch)
		default:
			t.Errorf("dist/%s: unexpected GOOS %q", e.Name(), tg.goos)
		}
	}

	cmp := func(a, b target) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) }
	slices.SortFunc(got, cmp)
	slices.SortFunc(want, cmp)
	if !slices.Equal(got, want) {
		t.Errorf("dist/ artifacts = %v, want exactly %v (no linux/arm64)", got, want)
	}
}

func checkStaticELF(t *testing.T, path, goarch string) {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Errorf("%s: open ELF: %v", filepath.Base(path), err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("%s: close ELF: %v", filepath.Base(path), err)
		}
	}()
	if goarch == "amd64" && f.Machine != elf.EM_X86_64 {
		t.Errorf("%s: ELF machine = %v, want EM_X86_64", filepath.Base(path), f.Machine)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			t.Errorf("%s: has a PT_INTERP segment, want a static binary", filepath.Base(path))
		}
	}
	for _, s := range f.Sections {
		if s.Name == ".symtab" || strings.HasPrefix(s.Name, ".debug_") || strings.HasPrefix(s.Name, ".zdebug_") {
			t.Errorf("%s: has section %s, want it stripped by -ldflags='-s -w'", filepath.Base(path), s.Name)
		}
	}
	needed, err := f.DynString(elf.DT_NEEDED)
	if err != nil {
		t.Errorf("%s: read DT_NEEDED: %v", filepath.Base(path), err)
	}
	if len(needed) != 0 {
		t.Errorf("%s: DT_NEEDED = %v, want none", filepath.Base(path), needed)
	}
}

func checkMachO(t *testing.T, path, goarch string) {
	t.Helper()
	f, err := macho.Open(path)
	if err != nil {
		t.Errorf("%s: open Mach-O: %v", filepath.Base(path), err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("%s: close Mach-O: %v", filepath.Base(path), err)
		}
	}()
	if goarch == "arm64" && f.Cpu != macho.CpuArm64 {
		t.Errorf("%s: Mach-O cpu = %v, want CpuArm64", filepath.Base(path), f.Cpu)
	}
	for _, s := range f.Sections {
		if s.Seg == "__DWARF" {
			t.Errorf("%s: has DWARF section %s, want it stripped by -ldflags=-w", filepath.Base(path), s.Name)
		}
	}
	if f.Symtab != nil {
		for _, sym := range f.Symtab.Syms {
			if strings.HasSuffix(sym.Name, "main.main") {
				t.Errorf("%s: symbol table has %s, want it stripped by -ldflags=-s", filepath.Base(path), sym.Name)
				break
			}
		}
	}
}

// TC-3: `make check` is the gate and can fail.
func TestMakeCheckIsTheGate(t *testing.T) {
	skipUnderMake(t)
	env := envWithout(makeGuardEnv)

	t.Run("clean tree passes and runs vet, test and vuln", func(t *testing.T) {
		dir := copyRepo(t)
		out, code := run(t, dir, env, "make", "check")
		if code != 0 {
			t.Fatalf("make check on the clean tree exited %d (network access is a prerequisite for make vuln):\n%s", code, out)
		}
		for _, step := range []string{"go vet ./...", "go test ", "golang.org/x/vuln/cmd/govulncheck@"} {
			if !strings.Contains(out, step) {
				t.Errorf("make check output lacks %q:\n%s", step, out)
			}
		}
	})

	t.Run("planted failing test makes it exit non-zero", func(t *testing.T) {
		dir := copyRepo(t)
		planted := filepath.Join(dir, "internal", "version", "zz_planted_failure_test.go")
		src := "package version\n\nimport \"testing\"\n\nfunc TestPlantedFailure(t *testing.T) { t.Fatal(\"planted failure\") }\n"
		if err := os.WriteFile(planted, []byte(src), 0o644); err != nil {
			t.Fatalf("plant failing test: %v", err)
		}
		out, code := run(t, dir, env, "make", "check")
		if code == 0 {
			t.Fatalf("make check with a planted failing test exited 0, want non-zero:\n%s", out)
		}
		if !strings.Contains(out, "TestPlantedFailure") {
			t.Errorf("make check failed, but not on the planted test:\n%s", out)
		}
	})

	t.Run("documented gate, pinned vuln, no hosted CI", func(t *testing.T) {
		root := repoRoot(t)
		makefile := readFile(t, filepath.Join(root, "Makefile"))
		readme := readFile(t, filepath.Join(root, "README.md"))

		if strings.Contains(makefile, "@latest") {
			t.Error("Makefile uses @latest; make vuln must name an exact version")
		}
		pinned := regexp.MustCompile(`go run golang\.org/x/vuln/cmd/govulncheck@v\d+\.\d+\.\d+ \./\.\.\.`)
		if !pinned.MatchString(makefile) {
			t.Error("Makefile vuln target does not run govulncheck at a pinned vX.Y.Z version with go run")
		}

		if !strings.Contains(readme, "make check") {
			t.Error("README does not name make check")
		}
		lower := strings.ToLower(readme)
		if !strings.Contains(lower, "gate") {
			t.Error("README does not describe make check as the gate")
		}
		if !strings.Contains(lower, "network") {
			t.Error("README does not state the network prerequisite of make vuln")
		}

		hostedCI := []string{".github/workflows", "github actions", "circleci", "travis", "gitlab-ci", "buildkite", "jenkins"}
		for name, text := range map[string]string{"Makefile": makefile, "README.md": readme} {
			for _, k := range hostedCI {
				if strings.Contains(strings.ToLower(text), k) {
					t.Errorf("%s mentions hosted CI %q", name, k)
				}
			}
		}
		if _, err := os.Stat(filepath.Join(root, ".github")); err == nil {
			t.Error(".github exists; hosted CI is not assumed")
		}
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TC-E1: .gitignore excludes build output and planning/, which is a separate
// private repository cloned into the working tree. The repo's .gitignore is checked inside a scratch git repository with global and system
// git config disabled, so a user-level excludes file cannot change the result.
func TestGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for TC-E1: %v", err)
	}
	gitignore := readFile(t, filepath.Join(repoRoot(t), ".gitignore"))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, code := run(t, dir, env, "git", "init", "-q"); code != 0 {
		t.Fatalf("git init exited %d: %s", code, out)
	}

	cases := []struct {
		path    string
		ignored bool
	}{
		{"build/x", true},
		{"dist/x", true},
		{".idea/workspace.xml", true},
		{".vscode/settings.json", true},
		{"main.go.swp", true},
		{".DS_Store", true},
		{"planning/nexus-server-manager/requirements.md", true},
		{"cmd/nexusd/main.go", false},
	}
	for _, c := range cases {
		out, code := run(t, dir, env, "git", "check-ignore", "-q", "--no-index", c.path)
		switch {
		case code > 1:
			t.Fatalf("git check-ignore %s exited %d: %s", c.path, code, out)
		case c.ignored && code != 0:
			t.Errorf("%s is not ignored (exit %d), want ignored", c.path, code)
		case !c.ignored && code != 1:
			t.Errorf("%s exit %d, want 1 (not ignored)", c.path, code)
		}
	}
}
