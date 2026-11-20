package buildsys

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A-62: capture.sh keys a bundle placeholder by the bundle name. The same
// Mac backs up to both Time Machine shares under one name, so the name has
// one placeholder in every captured file name and in every file's contents.
//
// The script insists on root (its first check) and finds ZFS and systemd by
// absolute path; a copy with only the root check removed runs on any host:
// a program the host lacks is recorded as Exit 127, so the redaction path
// is exercised on a temp tree without root or ZFS.
func TestCaptureShRedactionOnePlaceholderPerBundleName(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	text, _ := readCaptureScript(t)
	const rootCheck = `[ "$(id -u)" = 0 ] || die "run as root"`
	if strings.Count(text, rootCheck) != 1 {
		t.Fatalf("capture.sh: the root check %q is not exactly once; update this test", rootCheck)
	}
	tmp := t.TempDir()
	script := filepath.Join(tmp, "capture.sh")
	if err := os.WriteFile(script, []byte(strings.Replace(text, rootCheck, ":", 1)), 0o755); err != nil {
		t.Fatal(err)
	}

	const plist = "<?xml version=\"1.0\"?>\n<plist version=\"1.0\"><dict><key>k</key><string>secret-value</string></dict></plist>\n"
	mk := func(share string, bundles ...string) string {
		dir := filepath.Join(tmp, "in", share)
		for _, b := range bundles {
			bd := filepath.Join(dir, b)
			if err := os.MkdirAll(filepath.Join(bd, "bands"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bd, "com.apple.TimeMachine.SnapshotHistory.plist"), []byte(plist), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	// Share 1 holds two names; share 2 repeats the first; share 3 repeats
	// the second and adds a third. First appearance: Alice 1, Bob 2, Carol 3.
	s1 := mk("s1", "Alice Mac.sparsebundle", "Bob Mac.backupbundle")
	s2 := mk("s2", "Alice Mac.sparsebundle")
	s3 := mk("s3", "Bob Mac.backupbundle", "Carol Mac.sparsebundle")

	out := filepath.Join(tmp, "out")
	cmd := exec.Command("sh", script, "--host-name", "t", "--out", out, "--map", filepath.Join(tmp, "map.txt"), s1, s2, s3)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("capture.sh: %v\n%s", err, b)
	}

	for _, want := range []string{
		"shares/1/mac-1.sparsebundle/ls.txt",
		"shares/1/mac-2.backupbundle/ls.txt",
		"shares/2/mac-1.sparsebundle/ls.txt",
		"shares/3/mac-2.backupbundle/ls.txt",
		"shares/3/mac-3.sparsebundle/ls.txt",
	} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("%s: %v", want, err)
		}
	}
	prov, err := os.ReadFile(filepath.Join(out, "PROVENANCE"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^redaction: mac: 3;`).Match(prov) {
		t.Errorf("PROVENANCE redaction does not count 3 distinct bundle names:\n%s", prov)
	}
	// Every directory placeholder is one the file contents use, and no
	// real name or plist string survives in any file name or content.
	err = filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		for _, real := range []string{"Alice", "Bob", "Carol", "secret-value"} {
			if strings.Contains(rel, real) {
				t.Errorf("%s: real value %q in the file name", rel, real)
			}
		}
		if fi.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, real := range []string{"Alice", "Bob", "Carol", "secret-value"} {
			if strings.Contains(string(b), real) {
				t.Errorf("%s: real value %q in the contents", rel, real)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The argv of share 2's bundle, as PROVENANCE records it, uses the
	// placeholder its directory is named by (not mac-3, the listing position).
	if !strings.Contains(string(prov), "shares/2/mac-1.sparsebundle/ls.txt: ") ||
		!strings.Contains(string(prov), "/s2/mac-1.sparsebundle") {
		t.Errorf("PROVENANCE does not use mac-1 for the repeated name in share 2:\n%s", prov)
	}
}
