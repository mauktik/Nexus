package buildsys

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const releaseVersion = "v0.0.0-test"

// tarEntry is one regular file of a release tarball.
type tarEntry struct {
	mode     int64
	uid, gid int
	data     []byte
}

// readTarball returns the regular files of a .tar.gz by path, and fails on
// any entry that is not a directory or a regular file under top.
func readTarball(t *testing.T, file, top string) map[string]tarEntry {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	tr := tar.NewReader(zr)
	out := map[string]tarEntry{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name != top && !strings.HasPrefix(name, top+"/") || path.Clean(name) != name || strings.Contains(name, "/._") {
			t.Errorf("%s: entry %q is outside %s/ or is macOS metadata", filepath.Base(file), h.Name, top)
		}
		if h.Uid != 0 || h.Gid != 0 {
			t.Errorf("%s: %s owned by %d:%d, want 0:0", filepath.Base(file), h.Name, h.Uid, h.Gid)
		}
		for k := range h.PAXRecords {
			if strings.Contains(k, "xattr") {
				t.Errorf("%s: %s carries extended attribute record %s", filepath.Base(file), h.Name, k)
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
		case tar.TypeReg:
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			out[strings.TrimPrefix(name, top+"/")] = tarEntry{mode: h.Mode & 0o7777, uid: h.Uid, gid: h.Gid, data: b}
		default:
			t.Errorf("%s: %s has type %q, want a directory or regular file", filepath.Base(file), h.Name, h.Typeflag)
		}
	}
	return out
}

func sortedKeys(m map[string]tarEntry) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// TC-178: `make release VERSION=v0.0.0-test` writes one tarball per target
// and SHA256SUMS that match; the nexusd tarball has the binary, unit,
// default config, install and uninstall scripts and docs; govulncheck runs
// and reports nothing; a failing release step stops the build before any
// tarball. `-o test` skips only the test step, which `make test` already is
// (this case runs inside it); a dry run shows release runs it.
func TestReleaseTarballs(t *testing.T) {
	skipUnderMake(t)
	env := envWithout(makeGuardEnv)
	root := repoRoot(t)

	t.Run("release flow runs the whole gate", func(t *testing.T) {
		out, code := run(t, copyRepo(t), env, "make", "-n", "release", "VERSION="+releaseVersion)
		if code != 0 {
			t.Fatalf("make -n release exited %d:\n%s", code, out)
		}
		steps := []string{"go vet ./...", "go test -race ./...", "golang.org/x/vuln/cmd/govulncheck@v", "go build -trimpath", "SHA256SUMS"}
		last := -1
		for _, s := range steps {
			i := strings.Index(out, s)
			if i < 0 || i < last {
				t.Fatalf("make -n release: %q missing or out of order (want %q in order):\n%s", s, steps, out)
			}
			last = i
		}
	})

	t.Run("tarballs, checksums and vuln step", func(t *testing.T) {
		dir := copyRepo(t)
		out, code := run(t, dir, env, "make", "-o", "test", "release", "VERSION="+releaseVersion)
		if code != 0 {
			t.Fatalf("make release exited %d (network access is a prerequisite for govulncheck):\n%s", code, out)
		}
		if !strings.Contains(out, "go run golang.org/x/vuln/cmd/govulncheck@") || !strings.Contains(out, "No vulnerabilities found.") {
			t.Fatalf("release output lacks a clean govulncheck run:\n%s", out)
		}
		t.Logf("govulncheck in the release flow: No vulnerabilities found.")
		rel := filepath.Join(dir, "dist", "release")
		entries, err := os.ReadDir(rel)
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for _, e := range entries {
			files = append(files, e.Name())
		}
		nexusdTop := "nexusd-" + releaseVersion + "-linux-amd64"
		tops := []string{"nexus-" + releaseVersion + "-darwin-arm64", "nexus-" + releaseVersion + "-linux-amd64", nexusdTop}
		var want []string
		for _, top := range tops {
			want = append(want, top+".tar.gz")
		}
		want = append(want, "SHA256SUMS")
		slices.Sort(want)
		if !slices.Equal(files, want) {
			t.Fatalf("dist/release = %q, want %q", files, want)
		}

		// Step 1: SHA256SUMS lists each tarball once, with its real digest.
		sums, err := os.Open(filepath.Join(rel, "SHA256SUMS"))
		if err != nil {
			t.Fatal(err)
		}
		defer sums.Close()
		listed := map[string]bool{}
		sc := bufio.NewScanner(sums)
		for sc.Scan() {
			sum, name, ok := strings.Cut(sc.Text(), "  ")
			if !ok || listed[name] {
				t.Fatalf("SHA256SUMS line %q is malformed or repeated", sc.Text())
			}
			listed[name] = true
			b, err := os.ReadFile(filepath.Join(rel, name))
			if err != nil {
				t.Fatalf("SHA256SUMS names %s: %v", name, err)
			}
			got := sha256.Sum256(b)
			if hex.EncodeToString(got[:]) != sum {
				t.Errorf("%s: sha256 %x, SHA256SUMS says %s", name, got, sum)
			}
		}
		if len(listed) != len(tops) {
			t.Fatalf("SHA256SUMS lists %d files, want %d", len(listed), len(tops))
		}

		// Step 1: contents.
		readme, err := os.ReadFile(filepath.Join(root, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		// Every file under docs/, the generated docs/cli/ included.
		var docs []string
		err = filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				docs = append(docs, p)
			}
			return err
		})
		if err != nil || len(docs) == 0 {
			t.Fatalf("docs: %v %v", docs, err)
		}
		for _, top := range tops {
			got := readTarball(t, filepath.Join(rel, top+".tar.gz"), top)
			t.Logf("%s.tar.gz: %q", top, sortedKeys(got))
			bin := "nexus"
			wantFiles := map[string]string{"README.md": string(readme)}
			if top == nexusdTop {
				bin = "nexusd"
				for _, f := range []string{"nexusd.service", "nexusd.openrc", "nexusd.default.yaml", "install.sh", "uninstall.sh"} {
					b, err := os.ReadFile(filepath.Join(root, "deploy", f))
					if err != nil {
						t.Fatal(err)
					}
					wantFiles[f] = string(b)
				}
			}
			for _, d := range docs {
				b, err := os.ReadFile(d)
				if err != nil {
					t.Fatal(err)
				}
				r, err := filepath.Rel(filepath.Join(root, "docs"), d)
				if err != nil {
					t.Fatal(err)
				}
				wantFiles["docs/"+filepath.ToSlash(r)] = string(b)
			}
			for name, content := range wantFiles {
				e, ok := got[name]
				if !ok {
					t.Errorf("%s: no %s", top, name)
					continue
				}
				if string(e.data) != content {
					t.Errorf("%s: %s differs from the source tree", top, name)
				}
			}
			for _, exe := range []string{bin, "nexusd.openrc", "install.sh", "uninstall.sh"} {
				if e, ok := got[exe]; ok && e.mode != 0o755 {
					t.Errorf("%s: %s mode %04o, want 0755", top, exe, e.mode)
				}
			}
			e, ok := got[bin]
			if !ok {
				t.Fatalf("%s: no %s binary", top, bin)
			}
			if len(got) != len(wantFiles)+1 {
				t.Errorf("%s: %d files, want %d: %q", top, len(got), len(wantFiles)+1, sortedKeys(got))
			}
			checkReleaseBinary(t, top, bin, e.data)
		}
	})

	t.Run("a failing step stops the build", func(t *testing.T) {
		dir := copyRepo(t)
		planted := filepath.Join(dir, "internal", "version", "zz_planted_vet_failure.go")
		src := "package version\n\nimport \"fmt\"\n\nfunc plantedVetFailure() string { return fmt.Sprintf(\"%d\", \"not a number\") }\n"
		if err := os.WriteFile(planted, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := run(t, dir, env, "make", "-o", "test", "release", "VERSION="+releaseVersion)
		if code == 0 {
			t.Fatalf("make release with a planted vet failure exited 0:\n%s", out)
		}
		if !strings.Contains(out, "zz_planted_vet_failure.go") {
			t.Errorf("make release failed, but not on the planted file:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, "dist", "release")); !os.IsNotExist(err) {
			t.Fatalf("dist/release exists after a failed release: %v", err)
		}
		t.Logf("planted vet failure: make release exited %d, no dist/release", code)
	})

	t.Run("version is required in release form", func(t *testing.T) {
		out, code := run(t, copyRepo(t), env, "make", "release", "VERSION=dev")
		if code == 0 || !strings.Contains(out, "is not vMAJOR.MINOR.PATCH") {
			t.Fatalf("make release VERSION=dev exited %d:\n%s", code, out)
		}
	})
}

// checkReleaseBinary checks a tarball's binary is the release build of its
// target, stamped with the release version.
func checkReleaseBinary(t *testing.T, top, bin string, data []byte) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), bin)
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		t.Fatal(err)
	}
	bi, err := buildinfo.ReadFile(tmp)
	if err != nil {
		t.Fatalf("%s: %v", top, err)
	}
	settings := map[string]string{}
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	target := settings["GOOS"] + "-" + settings["GOARCH"]
	if !strings.HasSuffix(top, target) || bi.Path != modulePath+"/cmd/"+bin || settings["CGO_ENABLED"] != "0" {
		t.Errorf("%s: %s is %s %s CGO_ENABLED=%s", top, bin, bi.Path, target, settings["CGO_ENABLED"])
	}
	if !bytes.Contains(data, []byte(releaseVersion)) {
		t.Errorf("%s: %s is not stamped %s", top, bin, releaseVersion)
	}
	switch settings["GOOS"] {
	case "linux":
		f, err := elf.NewFile(bytes.NewReader(data))
		if err != nil || f.Machine != elf.EM_X86_64 {
			t.Errorf("%s: not an x86-64 ELF: %v", top, err)
		}
	case "darwin":
		f, err := macho.NewFile(bytes.NewReader(data))
		if err != nil || f.Cpu != macho.CpuArm64 {
			t.Errorf("%s: not an arm64 Mach-O: %v", top, err)
		}
	}
}
