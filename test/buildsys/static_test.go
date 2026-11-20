// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package buildsys

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// staticcheckMsg is the phrase staticcheck prints for every rejected ELF.
const staticcheckMsg = "is not statically linked"

// runSplit is run with stdout and stderr kept apart.
func runSplit(t *testing.T, dir string, env []string, name string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var o, e bytes.Buffer
	cmd.Stdout = &o
	cmd.Stderr = &e
	err := cmd.Run()
	if err == nil {
		return o.String(), e.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return o.String(), e.String(), exitErr.ExitCode()
	}
	t.Fatalf("start %s %v: %v", name, args, err)
	return "", "", -1
}

// buildStaticcheck builds test/buildsys/staticcheck for the host into dir.
func buildStaticcheck(t *testing.T, root, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "staticcheck")
	if out, code := run(t, root, nil, "go", "build", "-o", bin, "./test/buildsys/staticcheck"); code != 0 {
		t.Fatalf("build staticcheck exited %d:\n%s", code, out)
	}
	return bin
}

// nginx-provider TC-5: staticcheck rejects a dynamically linked ELF and
// accepts a static one (FR-38).
func TestStaticcheckRejectsDynamicELF(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	checker := buildStaticcheck(t, root, tmp)

	static := filepath.Join(tmp, "nexusd")
	env := append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, code := run(t, root, env, "go", "build", "-o", static, "./cmd/nexusd"); code != 0 {
		t.Fatalf("static build of nexusd exited %d:\n%s", code, out)
	}
	dynamic := filepath.Join(root, "test", "buildsys", "testdata", "dynamic.elf")

	t.Run("static ELF passes", func(t *testing.T) {
		_, stderr, code := runSplit(t, root, nil, checker, static)
		if code != 0 {
			t.Errorf("staticcheck %s exited %d, want 0; stderr:\n%s", static, code, stderr)
		}
	})

	t.Run("dynamic ELF fails naming the file, PT_INTERP and DT_NEEDED", func(t *testing.T) {
		_, stderr, code := runSplit(t, root, nil, checker, dynamic)
		if code == 0 {
			t.Fatalf("staticcheck %s exited 0, want non-zero", dynamic)
		}
		for _, want := range []string{dynamic, "PT_INTERP", "DT_NEEDED", "libc.so.6", staticcheckMsg} {
			if !strings.Contains(stderr, want) {
				t.Errorf("staticcheck stderr lacks %q:\n%s", want, stderr)
			}
		}
	})

	t.Run("any failing ELF fails the run", func(t *testing.T) {
		_, stderr, code := runSplit(t, root, nil, checker, static, dynamic)
		if code == 0 {
			t.Fatalf("staticcheck <static> <dynamic> exited 0, want non-zero")
		}
		if strings.Contains(stderr, static+":") {
			t.Errorf("staticcheck reported the static ELF as failing:\n%s", stderr)
		}
	})

	t.Run("no arguments or an unreadable file fail", func(t *testing.T) {
		if _, _, code := runSplit(t, root, nil, checker); code == 0 {
			t.Error("staticcheck with no arguments exited 0, want non-zero")
		}
		missing := filepath.Join(tmp, "missing")
		if _, stderr, code := runSplit(t, root, nil, checker, missing); code == 0 || !strings.Contains(stderr, missing) {
			t.Errorf("staticcheck %s: exit %d, stderr %q; want non-zero naming the file", missing, code, stderr)
		}
	})
}

var releaseBuildLine = regexp.MustCompile(`(?m)^RELEASE_BUILD[ \t]*:=.*$`)

// nginx-provider TC-6: make build-release fails on a dynamically linked
// release binary (FR-38, L-15).
func TestBuildReleaseRunsStaticcheck(t *testing.T) {
	skipUnderMake(t)
	root := repoRoot(t)
	makefile := readFile(t, filepath.Join(root, "Makefile"))
	env := envWithout(makeGuardEnv)

	t.Run("RELEASE_BUILD comment names the pure-Go os/user and net paths", func(t *testing.T) {
		lines := strings.Split(makefile, "\n")
		at := -1
		for i, l := range lines {
			if releaseBuildLine.MatchString(l) {
				at = i
				break
			}
		}
		if at < 0 {
			t.Fatal("Makefile has no RELEASE_BUILD assignment")
		}
		var comment []string
		for i := at - 1; i >= 0 && strings.HasPrefix(lines[i], "#"); i-- {
			comment = append(comment, lines[i])
		}
		text := strings.Join(comment, " ")
		for _, want := range []string{"pure-Go", "os/user", "net", "CGO_ENABLED=0"} {
			if !strings.Contains(text, want) {
				t.Errorf("the comment above RELEASE_BUILD lacks %q:\n%s", want, text)
			}
		}
	})

	t.Run("static release passes", func(t *testing.T) {
		dir := copyRepo(t)
		out, code := run(t, dir, env, "make", "build-release", "VERSION=v0.0.0-tc6")
		if code != 0 {
			t.Fatalf("make build-release exited %d:\n%s", code, out)
		}
		for _, name := range []string{"nexusd-linux-amd64", "nexus-linux-amd64"} {
			checkStaticELF(t, filepath.Join(dir, "dist", name), "amd64")
		}
	})

	t.Run("dynamic release fails", func(t *testing.T) {
		// A cgo, externally linked linux/amd64 build needs a C toolchain that
		// targets linux/amd64; probe for one with a small package.
		probeEnv := append(os.Environ(), "CGO_ENABLED=1", "GOOS=linux", "GOARCH=amd64")
		probe := filepath.Join(t.TempDir(), "probe")
		if out, code := run(t, root, probeEnv, "go", "build", "-ldflags=-linkmode=external", "-o", probe, "./test/buildsys/staticcheck"); code != 0 {
			t.Skipf("no C toolchain links cgo binaries for linux/amd64 on this host, so a dynamic release cannot be built:\n%s", out)
		}

		dir := copyRepo(t)
		mk := filepath.Join(dir, "Makefile")
		src := readFile(t, mk)
		dyn := releaseBuildLine.ReplaceAllLiteralString(src, "RELEASE_BUILD   := CGO_ENABLED=1 go build -ldflags=-linkmode=external")
		if dyn == src {
			t.Fatal("could not rewrite RELEASE_BUILD in the copied Makefile")
		}
		if err := os.WriteFile(mk, []byte(dyn), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := run(t, dir, env, "make", "build-release", "VERSION=v0.0.0-tc6")
		if code == 0 {
			t.Fatalf("make build-release with a dynamic RELEASE_BUILD exited 0, want non-zero:\n%s", out)
		}
		if !strings.Contains(out, staticcheckMsg) {
			t.Errorf("make build-release failed without the staticcheck message %q:\n%s", staticcheckMsg, out)
		}
	})
}
