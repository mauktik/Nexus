package buildsys

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// dockerStub stands in for the Docker CLI on PATH: it logs every invocation,
// answers `context inspect` with $DOCKER_STUB_ENDPOINT and `info` with
// aarch64, and exits 99 at the first `docker build`. It never reaches a
// Docker daemon.
const dockerStub = `#!/bin/bash
echo "$*" >>"$DOCKER_STUB_LOG"
case $1 in
context)
	case ${2:-} in
	inspect) echo "$DOCKER_STUB_ENDPOINT" ;;
	show) echo stub ;;
	*) exit 98 ;;
	esac
	;;
info) echo aarch64 ;;
build) exit 99 ;;
*) exit 98 ;;
esac
`

// runHarnessStub runs test/harness/run.sh in the repository copy dir with
// the Docker stub first on PATH, and returns its stdout, stderr, exit code
// and the stub's invocation log.
func runHarnessStub(t *testing.T, dir, endpoint string, extraEnv ...string) (stdout, stderr string, code int, calls []string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(dockerStub), 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(bin, "docker.log")
	env := envWithout(makeGuardEnv, "PATH", "DOCKER_HOST", "DOCKER_CONTEXT", "NEXUS_HARNESS_ALLOW_REMOTE", "RUN", "BASE")
	env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DOCKER_STUB_LOG="+logFile, "DOCKER_STUB_ENDPOINT="+endpoint)
	env = append(env, extraEnv...)
	cmd := exec.Command("bash", "test/harness/run.sh")
	cmd.Dir = dir
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("start run.sh: %v", err)
	}
	b, err := os.ReadFile(logFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			calls = append(calls, l)
		}
	}
	return outBuf.String(), errBuf.String(), code, calls
}

// hasCall reports whether a logged docker invocation starts with sub.
func hasCall(calls []string, sub string) bool {
	for _, c := range calls {
		if c == sub || strings.HasPrefix(c, sub+" ") {
			return true
		}
	}
	return false
}

// TC-SM-106 (WI 7.1; CON-1, A-42): run.sh refuses a Docker context whose
// endpoint is not unix:// before it calls anything but `docker context`, so
// a context pointing at the live server never receives the privileged
// container; NEXUS_HARNESS_ALLOW_REMOTE=1 overrides, and a local socket
// passes. The stub stops a run that gets past the guard at `docker build`.
func TestTCSM106_TheHarnessRefusesANonLocal(t *testing.T) {
	skipUnderMake(t)
	dir := copyRepo(t)
	const remote = "tcp://192.168.7.5:2376"
	const local = "unix:///Users/someone/.colima/default/docker.sock"

	t.Run("a remote endpoint is refused before any docker call", func(t *testing.T) {
		stdout, stderr, code, calls := runHarnessStub(t, dir, remote)
		if code == 0 {
			t.Fatalf("run.sh exited 0 against %s:\nstdout:\n%s\nstderr:\n%s", remote, stdout, stderr)
		}
		if !strings.Contains(stderr, remote) {
			t.Errorf("stderr does not name the endpoint %s:\n%s", remote, stderr)
		}
		if !strings.Contains(stderr, "NEXUS_HARNESS_ALLOW_REMOTE=1") {
			t.Errorf("stderr does not name the override:\n%s", stderr)
		}
		for _, sub := range []string{"run", "build", "cp", "exec", "info"} {
			if hasCall(calls, sub) {
				t.Errorf("docker %s was called after the refusal; calls: %q", sub, calls)
			}
		}
		for _, c := range calls {
			if !strings.HasPrefix(c, "context ") {
				t.Errorf("docker %s was called; only `docker context` may run before the guard passes", c)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "build", "harness")); err == nil {
			t.Errorf("build/harness was written although the guard refused")
		}
	})

	t.Run("NEXUS_HARNESS_ALLOW_REMOTE=1 passes the guard", func(t *testing.T) {
		stdout, stderr, code, calls := runHarnessStub(t, dir, remote, "NEXUS_HARNESS_ALLOW_REMOTE=1")
		if code != 99 || !hasCall(calls, "build") {
			t.Fatalf("run.sh exited %d, calls %q; want 99 at docker build:\nstdout:\n%s\nstderr:\n%s", code, calls, stdout, stderr)
		}
		if !strings.Contains(stdout+stderr, remote) {
			t.Errorf("the override run does not name the remote endpoint %s", remote)
		}
	})

	t.Run("a local unix socket passes the guard", func(t *testing.T) {
		stdout, stderr, code, calls := runHarnessStub(t, dir, local)
		if code != 99 || !hasCall(calls, "build") {
			t.Fatalf("run.sh exited %d, calls %q; want 99 at docker build:\nstdout:\n%s\nstderr:\n%s", code, calls, stdout, stderr)
		}
	})
}

// maxPackageEntries is the package entry bound of the nexusd update
// pipeline. A-43 replaced FR-7's 64 with 1024 because the real tree already
// holds more than 64 entries; it is hard-coded here rather than imported
// from the unexported scan constant.
const maxPackageEntries = 1024

// TC-SM-108 (WI 7.1; FR-7, A-43): a real nexusd tree packed by
// `make nexusd-tarball` holds more than 64 and at most 1024 entries, under
// one top-level directory nexusd-<v>-linux-<goarch>, packed with TAR_FLAGS
// (root-owned, no extended attributes or macOS metadata).
func TestTCSM108_TheRealNexusdTreeSEntry(t *testing.T) {
	skipUnderMake(t)
	dir := copyRepo(t)
	tmp := t.TempDir()
	const v = "v9.9.2"
	bin := filepath.Join(tmp, "nexusd")
	env := append(envWithout(makeGuardEnv, "GOOS", "GOARCH", "CGO_ENABLED"), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, code := run(t, dir, env, "go", "build", "-trimpath", "-ldflags", "-X "+modulePath+"/internal/version.Version="+v, "-o", bin, "./cmd/nexusd"); code != 0 {
		t.Fatalf("go build exited %d:\n%s", code, out)
	}
	top := "nexusd-" + v + "-linux-" + runtime.GOARCH
	tree := filepath.Join(tmp, top)
	tarball := filepath.Join(tmp, "t.tar.gz")
	makeEnv := envWithout(makeGuardEnv)
	if out, code := run(t, dir, makeEnv, "make", "--no-print-directory", "nexusd-tree", "TREE_BIN="+bin, "TREE_DIR="+tree); code != 0 {
		t.Fatalf("make nexusd-tree exited %d:\n%s", code, out)
	}
	if out, code := run(t, dir, makeEnv, "make", "--no-print-directory", "nexusd-tarball", "TREE_DIR="+tree, "TARBALL="+tarball); code != 0 {
		t.Fatalf("make nexusd-tarball exited %d:\n%s", code, out)
	}

	f, err := os.Open(tarball)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	entries := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries++
		if first, _, _ := strings.Cut(strings.TrimSuffix(h.Name, "/"), "/"); first != top {
			t.Errorf("entry %q is not below the single top-level directory %s", h.Name, top)
		}
	}
	if entries <= 64 || entries > maxPackageEntries {
		t.Errorf("the tarball has %d entries, want more than 64 and at most %d (A-43)", entries, maxPackageEntries)
	}
	t.Logf("%s: %d entries", top, entries)

	// The TAR_FLAGS checks: root-owned, no xattr records, no macOS
	// metadata, only directories and regular files.
	files := readTarball(t, tarball, top)
	for _, f := range []string{"nexusd", "install.sh", "uninstall.sh"} {
		if e, ok := files[f]; !ok || e.mode != 0o755 {
			t.Errorf("%s/%s: present %v, mode %o; want 0755", top, f, ok, e.mode)
		}
	}
	for _, f := range []string{"nexusd.service", "nexusd.default.yaml", "README.md", "docs/install.md"} {
		if _, ok := files[f]; !ok {
			t.Errorf("the tarball has no %s/%s", top, f)
		}
	}
}
