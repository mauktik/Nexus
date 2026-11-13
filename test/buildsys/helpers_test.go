// Package buildsys holds the T0 build-system cases (TC-1, TC-2, TC-3, TC-E1).
// They build and execute the binaries and run make in throwaway copies of the
// repository, so the real checkout is never written to.
package buildsys

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// makeGuardEnv is exported by the Makefile. Cases that run make skip when it
// is set so that `make test` never recurses into make.
const makeGuardEnv = "NEXUS_IN_MAKE"

const modulePath = "github.com/mauktik/nexus"

func skipUnderMake(t *testing.T) {
	t.Helper()
	if os.Getenv(makeGuardEnv) != "" {
		t.Skipf("%s is set: this case runs make and must not recurse from make; run `go test ./test/buildsys/` directly", makeGuardEnv)
	}
}

// repoRoot walks up from the package directory to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// copyRepo copies the source tree into a fresh temporary directory, leaving
// out VCS metadata, build output and planning documents.
func copyRepo(t *testing.T) string {
	t.Helper()
	src := repoRoot(t)
	dst := t.TempDir()
	skipDirs := map[string]bool{".git": true, "build": true, "dist": true, "planning": true}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && filepath.Dir(rel) == "." && skipDirs[rel] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
	if err != nil {
		t.Fatalf("copy repo: %v", err)
	}
	return dst
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); err == nil {
			err = cerr
		}
	}()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return errors.Join(err, out.Close())
	}
	return out.Close()
}

// envWithout returns the current environment minus the named variables.
func envWithout(names ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		drop := false
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				drop = true
				break
			}
		}
		if !drop {
			env = append(env, kv)
		}
	}
	return env
}

// run executes name with args in dir and returns combined output and the exit
// code. A failure to start the process fails the test.
func run(t *testing.T, dir string, env []string, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), exitErr.ExitCode()
	}
	t.Fatalf("start %s %v: %v", name, args, err)
	return "", -1
}

// releaseKeyEnvs select the release-signing key. Cases that run make release
// drop them from the environment and always name a temporary key, so the
// real key under $HOME is never read or written.
var releaseKeyEnvs = []string{"NEXUS_RELEASE_KEY", "RELEASE_KEY"}

// releaseEnv is the environment for running make release: the current one
// minus the make guard and any key selection.
func releaseEnv() []string {
	return envWithout(append([]string{makeGuardEnv}, releaseKeyEnvs...)...)
}

var trustedKeysLine = regexp.MustCompile(`(?m)^trustedKeys: \["([A-Za-z0-9+/=]{44})"\]$`)

// releaseKey makes a temporary signing key with `nexus-release keygen` from
// the repository copy dir, and returns its path and public key.
func releaseKey(t *testing.T, dir string) (path, pub string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "keys", "release-signing.key")
	out, code := run(t, dir, releaseEnv(), "go", "run", "./cmd/nexus-release", "keygen", "--key", path)
	m := trustedKeysLine.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("nexus-release keygen exited %d:\n%s", code, out)
	}
	return path, m[1]
}
