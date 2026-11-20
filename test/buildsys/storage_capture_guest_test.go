package buildsys

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runCaptureCopy runs a copy of capture.sh with only its root check removed
// (see the A-62 test) on the given directories and returns the output tree.
func runCaptureCopy(t *testing.T, tmp string, dirs ...string) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	text, _ := readCaptureScript(t)
	const rootCheck = `[ "$(id -u)" = 0 ] || die "run as root"`
	if strings.Count(text, rootCheck) != 1 {
		t.Fatalf("capture.sh: the root check %q is not exactly once; update this test", rootCheck)
	}
	script := filepath.Join(tmp, "capture.sh")
	if err := os.WriteFile(script, []byte(strings.Replace(text, rootCheck, ":", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out")
	args := append([]string{script, "--host-name", "t", "--out", out, "--map", filepath.Join(tmp, "map.txt")}, dirs...)
	if b, err := exec.Command("sh", args...).CombinedOutput(); err != nil {
		t.Fatalf("capture.sh: %v\n%s", err, b)
	}
	return out
}

// walkCapture calls fn with each file of the output tree: its path relative
// to out and its contents.
func walkCapture(t *testing.T, out string, fn func(rel, content string)) {
	t.Helper()
	err := filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		fn(filepath.ToSlash(rel), string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Code review CR-3: a guest name is replaced in its own field, and elsewhere
// only as a whole word in the vzdump files, and only when it is no short or
// structural word. Before the fix every `VM Name:` or `CT Name:` value was
// replaced as a substring in every captured file, so a guest named `xi`
// turned every `Exit 0` trailer into `Eguest-103t 0` and a guest named
// `zfs` rewrote `zfs-2.4.1` and every dataset name containing it.
func TestCaptureShRedactionGuestNamesAreFieldScopedAndWholeWord(t *testing.T) {
	const archive = "vzdump-lxc-103-2026_08_27-10_34_54"
	exit := regexp.MustCompile(`(?m)^Exit [0-9]+\n?$`)
	for _, c := range []struct {
		name string
		// everywhere: the name must be gone from every file (a name the
		// script replaces beyond its own field).
		everywhere bool
	}{
		{"xi", false},
		{"zfs", false},
		{"Alice Smith", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			dir := filepath.Join(tmp, "in", "vz")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			log := "INFO: starting new backup job: vzdump 103 --mode snapshot\n" +
				"INFO: Starting Backup of VM 103 (lxc)\n" +
				"INFO: CT Name: " + c.name + "\n" +
				"INFO: backup of " + c.name + " is running, zfs snapshot taken\n" +
				"INFO: Finished Backup of VM 103 (00:00:06)\n"
			for name, body := range map[string]string{
				archive + ".tar.zst":       "x",
				archive + ".log":           log,
				archive + ".tar.zst.notes": c.name + "\nsecond line\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out := runCaptureCopy(t, tmp, dir)

			prov, err := os.ReadFile(filepath.Join(out, "PROVENANCE"))
			if err != nil {
				t.Fatal(err)
			}
			if !regexp.MustCompile(`(?m)^redaction: mac: 0; guest: 1; `).Match(prov) {
				t.Errorf("PROVENANCE redaction does not count 1 guest:\n%s", prov)
			}
			walkCapture(t, out, func(rel, content string) {
				if rel == "PROVENANCE" || strings.HasSuffix(rel, ".stderr") {
					return
				}
				if !exit.MatchString(content) {
					t.Errorf("%s does not end in an Exit line: %q", rel, content)
				}
				if strings.Contains(content, "guest-103") && !strings.HasPrefix(rel, "vzdump/") {
					t.Errorf("%s holds a guest placeholder outside vzdump/", rel)
				}
				if c.everywhere && strings.Contains(content, c.name) {
					t.Errorf("%s still holds the guest name %q", rel, c.name)
				}
			})
			head, err := os.ReadFile(filepath.Join(out, "vzdump", "newest-log-head.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(head), "CT Name: guest-103\n") {
				t.Errorf("the log's name field is not guest-103:\n%s", head)
			}
			notes, err := os.ReadFile(filepath.Join(out, "vzdump", "newest-notes-line1.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(notes), "guest-103\n") {
				t.Errorf("the notes first line is not guest-103: %q", notes)
			}
			if !c.everywhere && !strings.Contains(string(head), "zfs snapshot taken") {
				t.Errorf("the log's own words were rewritten:\n%s", head)
			}
			// The reproduction of the review: `Exit 0` survives a short name.
			idu, err := os.ReadFile(filepath.Join(out, "host", "id-u.txt"))
			if err != nil || !strings.HasSuffix(string(idu), "\nExit 0\n") {
				t.Errorf("host/id-u.txt = %q, %v; want it to end in \"Exit 0\"", idu, err)
			}
			zv, err := os.ReadFile(filepath.Join(out, "zfs", "zfs-version.txt"))
			if err != nil || strings.Contains(string(zv), "guest") {
				t.Errorf("zfs/zfs-version.txt = %q, %v; want it untouched", zv, err)
			}
		})
	}
}

// Code review CR-7: the Time Machine marker names of a share root, and a
// <string> value that spans lines, are redacted too.
func TestCaptureShRedactionMarkerNamesAndMultiLinePlistStrings(t *testing.T) {
	tmp := t.TempDir()
	share := filepath.Join(tmp, "in", "tm")
	bd := filepath.Join(share, "Alice Mac.sparsebundle")
	if err := os.MkdirAll(filepath.Join(bd, "bands"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, mk := range []string{"31c17dad-ced8-43b7", "54de2ac0-5fe1-4863"} {
		if err := os.WriteFile(filepath.Join(share, ".com.apple.timemachine.supported-"+mk), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plist := "<?xml version=\"1.0\"?>\n<plist version=\"1.0\"><dict><key>k</key><string>line-one\nline-two-secret</string></dict></plist>\n"
	if err := os.WriteFile(filepath.Join(bd, "com.apple.TimeMachine.SnapshotHistory.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runCaptureCopy(t, tmp, share)
	found := 0
	walkCapture(t, out, func(rel, content string) {
		for _, real := range []string{"31c17dad", "54de2ac0", "line-one", "line-two-secret", "Alice"} {
			if strings.Contains(content, real) || strings.Contains(rel, real) {
				t.Errorf("%s: real value %q survived", rel, real)
			}
		}
		if strings.HasSuffix(rel, "SnapshotHistory.plist") && regexp.MustCompile(`<string>redacted-[0-9]+</string>`).MatchString(content) {
			found++
		}
		if rel == "shares/1/ls.txt" {
			for _, want := range []string{".com.apple.timemachine.supported-id-1\n", ".com.apple.timemachine.supported-id-2\n"} {
				if !strings.Contains(content, want) {
					t.Errorf("%s lacks a marker placeholder ending %q:\n%s", rel, want, content)
				}
			}
		}
	})
	if found != 1 {
		t.Errorf("%d plists hold a redacted-<n> string, want 1", found)
	}
}
