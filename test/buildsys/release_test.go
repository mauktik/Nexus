package buildsys

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/release"
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
// (this case runs inside it); a dry run shows release runs it. Every run
// names a temporary signing key (WI 1.4).
func TestReleaseTarballs(t *testing.T) {
	skipUnderMake(t)
	env := releaseEnv()
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
		key, _ := releaseKey(t, dir)
		out, code := run(t, dir, env, "make", "-o", "test", "release", "VERSION="+releaseVersion, "NEXUS_RELEASE_KEY="+key)
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
		want = append(want, "SHA256SUMS", nexusdTop+".tar.gz.sig")
		slices.Sort(want)
		if !slices.Equal(files, want) {
			t.Fatalf("dist/release = %q, want %q", files, want)
		}

		// Step 1: SHA256SUMS lists each tarball and the signature once, with
		// its real digest.
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
		if len(listed) != len(tops)+1 {
			t.Fatalf("SHA256SUMS lists %d files, want %d", len(listed), len(tops)+1)
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
				for _, f := range []string{"nexusd.service", "nexusd.default.yaml", "install.sh", "uninstall.sh"} {
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
			for _, exe := range []string{bin, "install.sh", "uninstall.sh"} {
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
		key, _ := releaseKey(t, dir)
		planted := filepath.Join(dir, "internal", "version", "zz_planted_vet_failure.go")
		src := "package version\n\nimport \"fmt\"\n\nfunc plantedVetFailure() string { return fmt.Sprintf(\"%d\", \"not a number\") }\n"
		if err := os.WriteFile(planted, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := run(t, dir, env, "make", "-o", "test", "release", "VERSION="+releaseVersion, "NEXUS_RELEASE_KEY="+key)
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
		out, code := run(t, copyRepo(t), env, "make", "release", "VERSION=dev", "NEXUS_RELEASE_KEY=/nonexistent")
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

// TC-SM-17 (WI 1.4; FR-15): make -n release runs the key check before check
// and the build.
func TestTCSM17_MakeNReleaseRunsTheKey(t *testing.T) {
	skipUnderMake(t)
	out, code := run(t, copyRepo(t), releaseEnv(), "make", "-n", "release", "VERSION="+releaseVersion, "NEXUS_RELEASE_KEY=/nonexistent")
	if code != 0 {
		t.Fatalf("make -n release exited %d:\n%s", code, out)
	}
	keyCheck := strings.Index(out, "nexus-release check-key --key '/nonexistent'")
	if keyCheck < 0 {
		t.Fatalf("make -n release has no key check:\n%s", out)
	}
	if i := strings.Index(out, "/nonexistent"); i < keyCheck {
		t.Errorf("the key is referenced before the key check:\n%s", out)
	}
	for _, later := range []string{"go vet ./...", "go test -race ./...", "go build "} {
		if i := strings.Index(out, later); i < keyCheck {
			t.Errorf("%q is missing or runs before the key check:\n%s", later, out)
		}
	}
	sign := strings.Index(out, "nexus-release sign --key '/nonexistent' dist/release/nexusd-"+releaseVersion+"-linux-amd64.tar.gz")
	sums := strings.Index(out, "*.tar.gz *.sig >SHA256SUMS")
	if sign < 0 || sums < sign {
		t.Errorf("make -n release does not sign the nexusd tarball and then list *.tar.gz *.sig in SHA256SUMS:\n%s", out)
	}
}

// TC-SM-18 (WI 1.4; FR-15): make release with a temp key signs only the
// nexusd tarball and lists the .sig in SHA256SUMS. `-o test` as in TC-178.
func TestTCSM18_MakeReleaseWithATempKey(t *testing.T) {
	skipUnderMake(t)
	dir := copyRepo(t)
	key, pubB64 := releaseKey(t, dir)
	out, code := run(t, dir, releaseEnv(), "make", "-o", "test", "release", "VERSION="+releaseVersion, "NEXUS_RELEASE_KEY="+key)
	if code != 0 {
		t.Fatalf("make release exited %d (network access is a prerequisite for govulncheck):\n%s", code, out)
	}
	rel := filepath.Join(dir, "dist", "release")
	entries, err := os.ReadDir(rel)
	if err != nil {
		t.Fatal(err)
	}
	nexusd := "nexusd-" + releaseVersion + "-linux-amd64.tar.gz"
	var sigs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sig") {
			sigs = append(sigs, e.Name())
		}
	}
	if !slices.Equal(sigs, []string{nexusd + ".sig"}) {
		t.Fatalf("signatures in dist/release = %q, want exactly %s.sig", sigs, nexusd)
	}

	sums, err := os.ReadFile(filepath.Join(rel, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^[0-9a-f]{64}  ` + regexp.QuoteMeta(nexusd+".sig") + `$`).Match(sums) {
		t.Errorf("SHA256SUMS has no line for %s.sig:\n%s", nexusd, sums)
	}
	sha := []string{"sha256sum"}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		sha = []string{"shasum", "-a", "256"}
	}
	if out, code := run(t, rel, nil, sha[0], append(sha[1:], "-c", "SHA256SUMS")...); code != 0 {
		t.Errorf("%q -c SHA256SUMS exited %d:\n%s", sha, code, out)
	}

	pub, err := release.ParsePublicKey(pubB64)
	if err != nil {
		t.Fatal(err)
	}
	sigFile, err := os.ReadFile(filepath.Join(rel, nexusd+".sig"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := release.DecodeSignature(sigFile)
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := release.DigestFile(filepath.Join(rel, nexusd))
	if err != nil {
		t.Fatal(err)
	}
	if !release.Verify([]ed25519.PublicKey{pub}, digest, sig) {
		t.Fatalf("%s.sig does not verify against the temporary key", nexusd)
	}
	t.Logf("%s.sig verifies against %s", nexusd, pubB64)
}

// TC-SM-ERR7 (WI 1.4; FR-15): make release with a missing or 0644 key fails
// before any tarball and before build-release.
func TestTCSMERR7_MakeReleaseWithAMissingOr(t *testing.T) {
	skipUnderMake(t)
	cases := []struct {
		name   string
		key    func(t *testing.T, dir string) string
		reason string
	}{
		{"missing", func(*testing.T, string) string { return "/nonexistent" }, "missing"},
		{"mode 0644", func(t *testing.T, dir string) string {
			key, _ := releaseKey(t, dir)
			if err := os.Chmod(key, 0o644); err != nil {
				t.Fatal(err)
			}
			return key
		}, "mode 0644 is too open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := copyRepo(t)
			key := c.key(t, dir)
			out, code := run(t, dir, releaseEnv(), "make", "release", "VERSION="+releaseVersion, "NEXUS_RELEASE_KEY="+key)
			if code == 0 {
				t.Fatalf("make release with a %s key exited 0:\n%s", c.name, out)
			}
			if !strings.Contains(out, c.reason) {
				t.Errorf("make release output does not name the reason %q:\n%s", c.reason, out)
			}
			if strings.Contains(out, "go vet") {
				t.Errorf("the gate ran despite the bad key:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(dir, "dist", "nexusd-linux-amd64")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("build-release ran: dist/nexusd-linux-amd64: %v", err)
			}
			tarballs, err := filepath.Glob(filepath.Join(dir, "dist", "release", "*.tar.gz"))
			if err != nil || len(tarballs) != 0 {
				t.Errorf("tarballs after a refused key: %q %v", tarballs, err)
			}
			t.Logf("%s key: make release exited %d", c.name, code)
		})
	}
}
