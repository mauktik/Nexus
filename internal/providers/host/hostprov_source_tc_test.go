// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-21, TC-HP-22, TC-HP-23, TC-HP-24, TC-HP-25, TC-HP-26, TC-HP-27, TC-HP-E6, TC-HP-E7, TC-HP-ERR10, TC-HP-ERR11, TC-HP-ERR12, TC-HP-E30, TC-HP-135, TC-HP-E32.
// Owner WIs: 2.1, 2.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mauktik/nexus/internal/svcctl"
)

// releaseFileCaptured is the OS release file the captures hold as the
// expected-value source of identity.os. The allow-list refuses it: the
// provider takes the OS from internal/platform (orchestrator note on
// TestNoReleaseSpecificLogic).
const releaseFileCaptured = "etc/os-release"

// TC-HP-21: Both captures are complete and documented. Owner WI 2.1. Requirements: NFR-4, NFR-5, CON-5.
//
// - Setup: none.
// - Steps: for UBU and ALP, stat `fs/`, `t1/`, `tools/`, `PROVENANCE`; read `PROVENANCE`; stat `testdata/capture.sh` and `testdata/README.md`.
// - Expected:
//   - all exist; `PROVENANCE` has non-empty `host:`, `kernel:`, `date:` and `under-unit:` lines;
//   - `fs/proc/stat`, `fs/proc/meminfo`, `fs/proc/self/mountinfo`, `fs/proc/1/mountinfo` (or a documented absence), `t1/proc/stat`, `t1/proc/net/dev`, `t1/proc/diskstats` exist;
//   - `tools/` holds `free`, `df`, `vmstat`, `uname-rm`, `nproc`, `hostname`, `statfs.txt`, `stat-mounts.txt` (one `stat -c '%d %F %n'` line per mountinfo mount point; `%F` is the F-STATFS `IsDir` source, and `%d` is A-38 evidence only, never read by a test or by the fake), `ip-addr`, `ip-route`, and `lsblk` (UBU), and the failed-services output;
//   - README says the files are vectors, not proofs (NFR-5), and how passwd was reduced.
func TestTCHP21_BothCapturesAreCompleteAndDocumented(t *testing.T) {
	for _, name := range []string{"capture.sh", "README.md"} {
		if st, err := os.Stat(filepath.Join("testdata", name)); err != nil || !st.Mode().IsRegular() {
			t.Errorf("testdata/%s: want a regular file, got %v", name, err)
		}
	}
	readme := readCaptureFile(t, filepath.Join("testdata", "README.md"))
	for _, want := range []string{"vectors, not proofs", "/etc/passwd", "uid < 1000", "65534"} {
		if !strings.Contains(readme, want) {
			t.Errorf("testdata/README.md does not say %q (NFR-5 vectors, passwd reduction)", want)
		}
	}

	for _, c := range hostCaptures {
		t.Run(c.dir, func(t *testing.T) {
			root := filepath.Join("testdata", c.dir)
			for _, d := range []string{"fs", "t1", "tools"} {
				if st, err := os.Stat(filepath.Join(root, d)); err != nil || !st.IsDir() {
					t.Errorf("%s/%s: want a directory, got %v", c.dir, d, err)
				}
			}
			prov := readProvenance(t, root)
			for _, k := range []string{"host", "kernel", "date", "under-unit"} {
				if v := prov[k]; len(v) != 1 || v[0] == "" {
					t.Errorf("%s/PROVENANCE: want one non-empty %q line, got %q", c.dir, k+":", v)
				}
			}

			files := []string{"fs/proc/stat", "fs/proc/meminfo", "fs/proc/self/mountinfo", "t1/proc/stat", "t1/proc/net/dev", "t1/proc/diskstats"}
			mountinfo := "proc/1/mountinfo"
			if _, err := os.Stat(filepath.Join(root, "fs", mountinfo)); err != nil {
				// A documented absence: capture.sh then records the self
				// fallback as its mountinfo source.
				if v := prov["mountinfo"]; len(v) != 1 || v[0] != "proc/self/mountinfo" {
					t.Errorf("%s: fs/proc/1/mountinfo is absent (%v) and PROVENANCE does not document it (mountinfo: %q)", c.dir, err, v)
				}
				mountinfo = "proc/self/mountinfo"
			} else if v := prov["mountinfo"]; len(v) != 1 || v[0] != mountinfo {
				t.Errorf("%s/PROVENANCE: mountinfo: %q, want %q", c.dir, v, mountinfo)
			}
			files = append(files, "fs/"+mountinfo)
			for _, f := range files {
				if st, err := os.Stat(filepath.Join(root, f)); err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
					t.Errorf("%s/%s: want a non-empty regular file, got %v", c.dir, f, err)
				}
			}

			tools := []string{"free", "df", "vmstat", "uname-rm", "nproc", "hostname", "statfs.txt", "stat-mounts.txt", "ip-addr", "ip-route"}
			if c.lsblk {
				tools = append(tools, "lsblk")
			}
			for _, f := range tools {
				if st, err := os.Stat(filepath.Join(root, "tools", f)); err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
					t.Errorf("%s/tools/%s: want a non-empty regular file, got %v", c.dir, f, err)
				}
			}
			// The failed-services output may be empty (nothing failed).
			if st, err := os.Stat(filepath.Join(root, "tools", c.failed)); err != nil || !st.Mode().IsRegular() {
				t.Errorf("%s/tools/%s (failed services): want a regular file, got %v", c.dir, c.failed, err)
			}

			// One stat-mounts.txt and one statfs.txt line per mountinfo
			// mount point, unless UNREADABLE says stat could not reach it.
			mounts := captureMountPoints(t, filepath.Join(root, "fs", mountinfo))
			unreadable := readCaptureFile(t, filepath.Join(root, "UNREADABLE"))
			statMounts := map[string]string{}
			for _, line := range captureLines(readCaptureFile(t, filepath.Join(root, "tools", "stat-mounts.txt"))) {
				mp, typ, ok := parseStatMountsLine(line)
				if !ok {
					t.Errorf("%s/tools/stat-mounts.txt: malformed line %q", c.dir, line)
					continue
				}
				if _, dup := statMounts[mp]; dup {
					t.Errorf("%s/tools/stat-mounts.txt: %q twice", c.dir, mp)
				}
				statMounts[mp] = typ
			}
			statfs := map[string]bool{}
			for _, line := range captureLines(readCaptureFile(t, filepath.Join(root, "tools", "statfs.txt"))) {
				mp, ok := parseStatfsLine(line)
				if !ok {
					t.Errorf("%s/tools/statfs.txt: malformed line %q", c.dir, line)
					continue
				}
				if statfs[mp] {
					t.Errorf("%s/tools/statfs.txt: %q twice", c.dir, mp)
				}
				statfs[mp] = true
			}
			inMountinfo := map[string]bool{}
			for _, mp := range mounts {
				inMountinfo[mp] = true
				if _, ok := statMounts[mp]; !ok && !strings.Contains(unreadable, "stat "+mp+": ") {
					t.Errorf("%s: mount point %q has no tools/stat-mounts.txt line and no UNREADABLE entry", c.dir, mp)
				}
				if !statfs[mp] && !strings.Contains(unreadable, "statfs "+mp+": ") {
					t.Errorf("%s: mount point %q has no tools/statfs.txt line and no UNREADABLE entry", c.dir, mp)
				}
			}
			for mp := range statMounts {
				if !inMountinfo[mp] {
					t.Errorf("%s/tools/stat-mounts.txt: %q is not a mount point of fs/%s", c.dir, mp, mountinfo)
				}
			}
			for mp := range statfs {
				if !inMountinfo[mp] {
					t.Errorf("%s/tools/statfs.txt: %q is not a mount point of fs/%s", c.dir, mp, mountinfo)
				}
			}
			if statMounts["/"] != "directory" || !statfs["/"] {
				t.Errorf("%s: the root mount has stat-mounts type %q and statfs line %v, want directory and true", c.dir, statMounts["/"], statfs["/"])
			}
		})
	}
}

// TC-HP-22: No sensitive file is committed in the captures. Owner WI 2.1. Requirements: NFR-6.
//
// - Setup: none.
// - Steps: walk `testdata/`; parse each capture's `fs/etc/passwd`.
// - Expected: no path whose base name is `product_serial`, `product_uuid`, `board_serial`, `chassis_serial`, `chassis_asset_tag`, `cmdline`, `environ`, `kmsg`, `shadow`, `gshadow` or `arp`; every passwd entry has uid < 1000 or uid 65534; `capture.sh` contains a guard that exits non-zero when any of those names appears in `OUTDIR` (grep for each name in the guard).
func TestTCHP22_NoSensitiveFileIsCommittedInThe(t *testing.T) {
	banned := []string{"product_serial", "product_uuid", "board_serial", "chassis_serial", "chassis_asset_tag", "cmdline", "environ", "kmsg", "shadow", "gshadow", "arp"}
	walked := 0
	err := filepath.WalkDir("testdata", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		walked++
		for _, b := range banned {
			if d.Name() == b {
				t.Errorf("sensitive path committed: %s", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk testdata: %v", err)
	}
	if walked < 100 {
		t.Fatalf("walked only %d paths under testdata: the captures are missing", walked)
	}

	for _, c := range hostCaptures {
		passwd := captureLines(readCaptureFile(t, filepath.Join("testdata", c.dir, "fs", "etc", "passwd")))
		if len(passwd) == 0 {
			t.Errorf("%s/fs/etc/passwd: no entries", c.dir)
		}
		for _, line := range passwd {
			f := strings.Split(line, ":")
			if len(f) != 7 {
				t.Errorf("%s/fs/etc/passwd: malformed entry %q", c.dir, line)
				continue
			}
			uid, err := strconv.Atoi(f[2])
			if err != nil || (uid >= 1000 && uid != 65534) {
				t.Errorf("%s/fs/etc/passwd: entry %q is not a system account (uid < 1000 or 65534)", c.dir, line)
			}
		}
	}

	script := readCaptureFile(t, filepath.Join("testdata", "capture.sh"))
	start := strings.Index(script, "\ncheck_sensitive() {\n")
	if start < 0 {
		t.Fatal("capture.sh has no check_sensitive guard")
	}
	end := strings.Index(script[start+1:], "\n}\n")
	if end < 0 {
		t.Fatal("capture.sh: check_sensitive has no closing brace")
	}
	guard := script[start : start+1+end]
	for _, b := range banned {
		if !strings.Contains(guard, "-name "+b+" ") {
			t.Errorf("capture.sh check_sensitive does not look for %q", b)
		}
	}
	if !strings.Contains(guard, "exit 1") || !strings.Contains(guard, "$OUT") {
		t.Errorf("capture.sh check_sensitive does not exit non-zero on a match in OUTDIR:\n%s", guard)
	}
	if n := strings.Count(script, "\ncheck_sensitive\n"); n < 2 {
		t.Errorf("capture.sh calls check_sensitive %d times, want it after copying and again at the end", n)
	}
}

// TC-HP-23: Every allow-listed capture file reads through the source layer, and nothing else was captured. Owner WI 2.2. Requirements: NFR-6, FR-3, CON-5.
//
// - Setup: `newSource(F-FS(capture), 250ms, …)` per capture.
// - Steps: walk each capture's `fs/` (unrecorded handle). For each regular file, look up its access kind (Finding 1). Call `src.read(path)` for a read entry, and `src.stat(path)` for a stat-only entry such as `usr/bin/chronyc` or `run/systemd/timesync`.
// - Expected: every file is allowed (the capture holds only allow-listed paths); each read entry reads with content byte-equal to the file; each stat-only entry stats without error; F-FS's recorder shows each path exactly once, with the matching access kind.
func TestTCHP23_EveryAllowListedCaptureFileReadsThrough(t *testing.T) {
	for _, c := range hostCaptures {
		t.Run(c.dir, func(t *testing.T) {
			ffs := fixtureFS(t, c.dir)
			src := newSource(ffs, 250*time.Millisecond, newFakeStatfs(t, c.dir).Statfs)
			capture := os.DirFS(filepath.Join("testdata", c.dir, "fs")) // the unrecorded handle
			var want []fsCall
			files := 0
			err := fs.WalkDir(capture, ".", func(name string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return err
				}
				files++
				kinds := allowedKinds(name)
				switch {
				case name == releaseFileCaptured:
					// Captured as the expected-value source of the OS fields,
					// which the provider takes from internal/platform: the
					// allow-list refuses it, so the provider cannot read the
					// release file itself.
					if _, err := src.read(name); !errors.Is(err, errNotAllowed) {
						t.Errorf("read %s: err = %v, want the allow-list's refusal", name, err)
					}
				case kinds&accRead != 0:
					want = append(want, fsCall{op: "open", name: name})
					got, err := src.read(name)
					if err != nil {
						t.Errorf("read %s: %v", name, err)
						return nil
					}
					raw, err := fs.ReadFile(capture, name)
					if err != nil {
						return err
					}
					if !bytes.Equal(got, raw) {
						t.Errorf("read %s: %d bytes differ from the capture's %d", name, len(got), len(raw))
					}
				case kinds&accStatOnly != 0:
					want = append(want, fsCall{op: "stat", name: name})
					if _, err := src.stat(name); err != nil {
						t.Errorf("stat %s: %v", name, err)
					}
				default:
					t.Errorf("%s/fs/%s is captured but not allow-listed for read or stat-only (kinds %v)", c.dir, name, kinds)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if files < 50 {
				t.Fatalf("walked only %d files", files)
			}
			if got := ffs.recorded(); !slices.Equal(got, want) {
				t.Errorf("F-FS recorded %d calls, want each allowed file once with its kind (%d):\ngot  %v\nwant %v", len(got), len(want), got, want)
			}
		})
	}
}

// TC-HP-24: Reads ignore a zero procfs size and cap at 4 MiB. Owner WI 2.2. Requirements: NFR-4, FR-12.
//
// - Setup: F-FS(UBU) (procfs `Stat` reports size 0); `replace("proc/stat", 5 MiB of valid-looking lines)`.
// - Steps: `src.read("proc/meminfo")`; `src.read("proc/stat")` after the replace.
// - Expected: `proc/meminfo` returns the full capture content despite size 0; the 5 MiB read returns an error (not a truncated success), and F-FS's byte counter shows at most 4 MiB + 1 byte read.
func TestTCHP24_ReadsIgnoreAZeroProcfsSizeAnd(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	src := newSource(ffs, 250*time.Millisecond, nil)
	fi, err := ffs.Stat("proc/meminfo")
	if err != nil || fi.Size() != 0 {
		t.Fatalf("F-FS Stat(proc/meminfo) = %v, %v; want procfs size 0", fi, err)
	}
	got, err := src.read("proc/meminfo")
	if want := ffs.rawRead("proc/meminfo"); err != nil || len(want) == 0 || !bytes.Equal(got, want) {
		t.Fatalf("read proc/meminfo = %d bytes, %v; want the capture's %d bytes", len(got), err, len(want))
	}

	line := strings.SplitAfter(string(ffs.rawRead("proc/stat")), "\n")[0] // the captured cpu line
	big := bytes.Repeat([]byte(line), 5<<20/len(line)+1)
	if len(big) < 5<<20 {
		t.Fatalf("synthetic proc/stat is %d bytes, want at least 5 MiB", len(big))
	}
	ffs.replace("proc/stat", big)
	ffs.resetRecorder()
	got, err = src.read("proc/stat")
	if err == nil || got != nil {
		t.Fatalf("read of a 5 MiB proc/stat = %d bytes, %v; want an error, not a truncated success", len(got), err)
	}
	if !errors.Is(err, errTooLarge) {
		t.Errorf("err = %v, want errTooLarge", err)
	}
	if r := reasonFor("proc/stat", err); r != "unrecognized format: /proc/stat" {
		t.Errorf("reason = %q", r)
	}
	if n := ffs.bytesRead("proc/stat"); n > maxReadBytes+1 || maxReadBytes != 4<<20 {
		t.Errorf("F-FS served %d bytes of proc/stat (bound %d), want at most 4 MiB + 1", n, maxReadBytes)
	}
}

// TC-HP-25: Reason classification uses the fixed vocabulary and never quotes content. Owner WI 2.2. Requirements: FR-12, NFR-6.
//
// - Setup: a table of injected errors.
// - Steps: `reasonFor(path, err)` for: `&fs.PathError{Op:"open", Path:"proc/1/mountinfo", Err: syscall.EACCES}`, the same with `EPERM`, with `ENOENT`, a `bounded` timeout of 2 s on `statfs /mnt/x`, and a parse error carrying the line `secret-token-xyz`.
// - Expected: `permission denied: /proc/1/mountinfo` (twice), `not present: /proc/1/mountinfo`, `timed out after 2s: statfs /mnt/x`, `unrecognized format: /proc/stat`; no reason contains `secret-token-xyz`; every reason starts with one of the prefixes of the architecture's Error Handling table (the test holds the table's prefix column as a list, with no count).
func TestTCHP25_ReasonClassificationUsesTheFixedVocabularyAnd(t *testing.T) {
	// The prefix column of architecture.md's Error Handling table.
	tablePrefixes := []string{
		"warming up", "stale: last sample ", "not running", "permission denied: ",
		"not present: ", "timed out after ", "still pending: ", "not visible: ",
		"unrecognized format: ", "counter reset: ", "not reported: ", "command failed: ",
		"not supported: ", "internal error: access not allow-listed",
	}
	const secret = "secret-token-xyz"
	pathErr := func(e syscall.Errno) error {
		return &fs.PathError{Op: "open", Path: "proc/1/mountinfo", Err: e}
	}
	cases := []struct {
		what string
		err  error
		want string
	}{
		{"proc/1/mountinfo", pathErr(syscall.EACCES), "permission denied: /proc/1/mountinfo"},
		{"proc/1/mountinfo", pathErr(syscall.EPERM), "permission denied: /proc/1/mountinfo"},
		{"proc/1/mountinfo", pathErr(syscall.ENOENT), "not present: /proc/1/mountinfo"},
		{"statfs /mnt/x", &timeoutError{what: "statfs /mnt/x", after: 2 * time.Second}, "timed out after 2s: statfs /mnt/x"},
		{"proc/stat", fmt.Errorf("hostparse: malformed line %q", secret), "unrecognized format: /proc/stat"},
		{"proc/stat", fmt.Errorf("parse: %w", &fs.PathError{Op: "parse", Path: secret, Err: errors.New(secret)}), "unrecognized format: /proc/stat"},
	}
	for _, c := range cases {
		got := reasonFor(c.what, c.err)
		if got != c.want {
			t.Errorf("reasonFor(%q, %v) = %q, want %q", c.what, c.err, got, c.want)
		}
		if strings.Contains(got, secret) {
			t.Errorf("reason %q quotes content", got)
		}
	}

	// Every reason the source layer can give starts with a table prefix.
	reasons := []string{
		reasonWarmingUp, reasonNotRunning, reasonStale(42 * time.Second),
		reasonNotVisible("/tmp/x"), reasonCounterReset("eth0"), reasonNotReported("no sensors"),
		reasonCommandExit("/usr/bin/timedatectl", 1),
		reasonFor("proc/1/cmdline", &fs.PathError{Op: "open", Path: "proc/1/cmdline", Err: errNotAllowed}),
		reasonFor("statfs /mnt/x", &pendingError{what: "statfs /mnt/x"}),
		reasonFor("statfs", errNotSupported("statfs on plan9")),
		reasonFor("/usr/bin/chronyc", fmt.Errorf("%w: %w", svcctl.ErrNotStarted, &fs.PathError{Op: "fork/exec", Path: "/usr/bin/chronyc", Err: syscall.ENOENT})),
		reasonFor("/usr/bin/chronyc", fmt.Errorf("%w: %w", svcctl.ErrNotStarted, &fs.PathError{Op: "fork/exec", Path: "/usr/bin/chronyc", Err: syscall.EACCES})),
		reasonFor("sys/class/net/lo/speed", &fs.PathError{Op: "read", Path: "sys/class/net/lo/speed", Err: syscall.EINVAL}),
	}
	for _, c := range cases {
		reasons = append(reasons, reasonFor(c.what, c.err))
	}
	for _, r := range reasons {
		if !slices.ContainsFunc(tablePrefixes, func(p string) bool { return strings.HasPrefix(r, p) }) {
			t.Errorf("reason %q starts with no prefix of the Error Handling table", r)
		}
	}
}

// TC-HP-26: No file read bypasses the allow-list. Owner WI 2.2. Requirements: NFR-6, CON-1.
//
// - Setup: none.
// - Steps: parse every non-test `.go` file of `internal/providers/host` (excluding `hostparse/`, which takes `[]byte`) with `go/parser`; find calls to `os.Open`, `os.OpenFile`, `os.ReadFile`, `os.ReadDir`, `os.Stat`, `os.Lstat`, `os.Readlink`, `os.DirFS`, `ioutil.*`, `syscall.Open`, `filepath.Walk*`.
// - Expected: the only hit is the default `os.DirFS("/")` in `provider.go`'s `New`; a planted file with `os.ReadFile("/etc/shadow")` in a temp copy of the package is caught (self-test).
func TestTCHP26_NoFileReadBypassesTheAllowList(t *testing.T) {
	hits := scanFileAccess(t, ".")
	for _, h := range hits {
		if h.file == "provider.go" && h.call == "os.DirFS" {
			continue // the default Options.FS in New, wrapped by the allow-list
		}
		t.Errorf("%s: %s bypasses the allow-list", h.pos, h.call)
	}
	if n := len(slices.DeleteFunc(slices.Clone(hits), func(h accessHit) bool { return h.call != "os.DirFS" })); n > 1 {
		t.Errorf("os.DirFS appears %d times, want only New's default", n)
	}

	// Self-test: a planted read, also under an import alias, is caught in a
	// copy of the package.
	dir := t.TempDir()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	copied := 0
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			b, err := os.ReadFile(e.Name())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
				t.Fatal(err)
			}
			copied++
		}
	}
	if copied == 0 {
		t.Fatal("no package files copied")
	}
	planted := "package host\n\nimport (\n\t\"os\"\n\txos \"os\"\n\t\"path/filepath\"\n)\n\n" +
		"func planted() {\n\t_, _ = os.ReadFile(\"/etc/shadow\")\n\t_, _ = xos.Open(\"/etc/gshadow\")\n" +
		"\t_ = filepath.WalkDir(\"/\", nil)\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	var caught []string
	for _, h := range scanFileAccess(t, dir) {
		if h.file == "planted.go" {
			caught = append(caught, h.call)
		}
	}
	if want := []string{"os.ReadFile", "os.Open", "filepath.WalkDir"}; !slices.Equal(caught, want) {
		t.Errorf("self-test caught %v in planted.go, want %v", caught, want)
	}
}

// accessHit is a direct file-system call found in a non-test source file.
type accessHit struct{ pos, file, call string }

// bannedAccess lists, per import path, the functions that reach the file
// system without the allow-list ("*": every function of the package).
var bannedAccess = map[string][]string{
	"os":            {"Open", "OpenFile", "ReadFile", "ReadDir", "Stat", "Lstat", "Readlink", "DirFS", "OpenRoot", "OpenInRoot"},
	"io/ioutil":     {"*"},
	"syscall":       {"Open", "Openat"},
	"path/filepath": {"Walk", "WalkDir"},
}

// scanFileAccess parses every non-test .go file of dir (hostparse/, which
// takes []byte, and testdata/ are other directories and not scanned) and
// returns its calls to bannedAccess functions, resolving import names.
func scanFileAccess(t *testing.T, dir string) []accessHit {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var hits []accessHit
	parsed := 0
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		parsed++
		names := map[string]string{} // local name -> import path
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			local := path.Base(p)
			if imp.Name != nil {
				local = imp.Name.Name
			}
			names[local] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			p, ok := names[id.Name]
			if !ok {
				return true
			}
			if banned := bannedAccess[p]; slices.Contains(banned, "*") || slices.Contains(banned, sel.Sel.Name) {
				hits = append(hits, accessHit{pos: fset.Position(sel.Pos()).String(), file: e.Name(), call: path.Base(p) + "." + sel.Sel.Name})
			}
			return true
		})
	}
	if parsed == 0 {
		t.Fatalf("%s: no non-test Go files parsed", dir)
	}
	return hits
}

// TC-HP-27: Default statfs, uname and addresses are unsupported off Linux. Owner WI 2.2. Requirements: CON-5, FR-3.
//
// - Setup: runs on darwin (`runtime.GOOS != "linux"`; skip on linux).
// - Steps: call the default statfs and uname functions.
// - Expected: both return an error whose reason is `not supported: …`; `GOOS=linux go vet ./internal/providers/host/...` passes (asserted by `make vet`, noted here).
func TestTCHP27_DefaultStatfsUnameAndAddressesAreUnsupported(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("TC-HP-27 checks the non-Linux defaults; statfs_test.go covers Linux")
	}
	if u, err := defaultStatfs("/"); err == nil || !strings.HasPrefix(reasonFor("statfs /", err), "not supported: ") {
		t.Errorf("defaultStatfs = %+v, %v; want a not supported error", u, err)
	} else if r := reasonFor("statfs /", err); r != "not supported: statfs on "+runtime.GOOS {
		t.Errorf("reason = %q", r)
	}
	if m, err := defaultUname(); err == nil || reasonFor("uname", err) != "not supported: uname on "+runtime.GOOS {
		t.Errorf("defaultUname = %q, %v; want not supported: uname on %s", m, err, runtime.GOOS)
	}
	// Addresses come from the portable net package, so the default works
	// here too (WI 2.2 addrs.go has no per-platform variant).
	addrs, err := defaultAddrs()
	if err != nil {
		t.Fatalf("defaultAddrs: %v", err)
	}
	if !slices.ContainsFunc(slices.Collect(maps.Values(addrs)), func(ps []netip.Prefix) bool {
		return slices.Contains(ps, netip.MustParsePrefix("127.0.0.1/8"))
	}) {
		t.Errorf("defaultAddrs = %v, want the loopback 127.0.0.1/8", addrs)
	}
}

// TC-HP-E6: Bounded blocking reads give up after the source timeout. Owner WI 2.2. Requirements: FR-12, NFR-1.
//
// - Setup: F-FS `block("sys/class/hwmon/hwmon0/temp1_input")`; source timeout 250 ms.
// - Steps: take a goroutine baseline; `src.bounded(ctx, read that path)`; measure; call it again 3 times at once with the same key; call `src.bounded` once for `sys/class/hwmon/hwmon0/temp2_input` (not blocked); `release()`; wait; call the first path again.
// - Expected:
//   - The first call returns an error classified `timed out after 250ms: /sys/class/hwmon/hwmon0/temp1_input` in ≤ 350 ms.
//   - The 3 repeated calls each return `still pending: /sys/class/hwmon/hwmon0/temp1_input` within 10 ms, start no goroutine (`runtime.NumGoroutine()` ≤ baseline + 1 throughout), and F-FS's `Open` count for that path stays 1 (Finding 3: the in-flight guard is part of `bounded`, keyed by the allow-listed path).
//   - The `temp2_input` call is not affected by the other key and returns its capture content.
//   - After `release()`, the abandoned goroutine exits (baseline restored), the key is cleared, and the last call reads the path again (`Open` count 2).
func TestTCHPE6_BoundedBlockingReadsGiveUpAfterThe(t *testing.T) {
	const (
		temp1 = "sys/class/hwmon/hwmon0/temp1_input"
		temp2 = "sys/class/hwmon/hwmon0/temp2_input"
	)
	ffs := fixtureFS(t, capUBU)
	// Both captures have no hwmon data (WI 2.1 note (e)), so the sensor
	// files are synthetic.
	ffs.replace(temp1, []byte("45000\n"))
	ffs.replace(temp2, []byte("47000\n"))
	release := ffs.block(temp1)
	src := newSource(ffs, 250*time.Millisecond, nil)
	ctx := t.Context()
	baseline := runtime.NumGoroutine()

	start := time.Now()
	_, err := src.readBounded(ctx, temp1)
	if el := time.Since(start); el > 350*time.Millisecond {
		t.Errorf("first call took %v, want at most 350ms", el)
	}
	if r := reasonFor(temp1, err); r != "timed out after 250ms: /sys/class/hwmon/hwmon0/temp1_input" {
		t.Errorf("first call: reason %q (err %v)", r, err)
	}
	if n := runtime.NumGoroutine(); n > baseline+1 {
		t.Errorf("goroutines %d after the first call, want at most baseline %d + 1", n, baseline)
	}

	var wg sync.WaitGroup
	errs := make([]error, 3)
	took := make([]time.Duration, 3)
	for i := range 3 {
		wg.Go(func() {
			s := time.Now()
			_, errs[i] = src.readBounded(ctx, temp1)
			took[i] = time.Since(s)
		})
	}
	wg.Wait()
	for i := range 3 {
		if r := reasonFor(temp1, errs[i]); r != "still pending: /sys/class/hwmon/hwmon0/temp1_input" {
			t.Errorf("repeat %d: reason %q", i, r)
		}
		if took[i] > 10*time.Millisecond {
			t.Errorf("repeat %d took %v, want at most 10ms", i, took[i])
		}
	}
	if n := runtime.NumGoroutine(); n > baseline+1 {
		t.Errorf("goroutines %d after the repeats, want at most baseline %d + 1", n, baseline)
	}
	if n := ffs.count("open", temp1); n != 1 {
		t.Errorf("F-FS opened %s %d times while in flight, want 1", temp1, n)
	}

	if b, err := src.readBounded(ctx, temp2); err != nil || string(b) != "47000\n" {
		t.Errorf("other key: %q, %v; want its content", b, err)
	}

	release()
	waitGoroutines(t, baseline)
	if src.inFlight("/" + temp1) {
		t.Error("the key is still in flight after the call returned")
	}
	if b, err := src.readBounded(ctx, temp1); err != nil || string(b) != "45000\n" {
		t.Errorf("after release: %q, %v; want a fresh read", b, err)
	}
	if n := ffs.count("open", temp1); n != 2 {
		t.Errorf("F-FS opened %s %d times, want 2", temp1, n)
	}
}

// TC-HP-E7: Source directory listing keeps sysfs symlink entries. Owner WI 2.2. Requirements: FR-8, FR-9, CON-5.
//
// - Setup: F-FS(UBU) (class entries reported as `ModeSymlink`).
// - Steps: `src.readDir("sys/class/net")`, `src.readDir("sys/block")`.
// - Expected: every capture entry is returned, including those whose `IsDir()` is false; names equal the capture's directory names.
func TestTCHPE7_SourceDirectoryListingKeepsSysfsSymlinkEntries(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	src := newSource(ffs, 250*time.Millisecond, nil)
	for _, dir := range []string{"sys/class/net", "sys/block"} {
		got, err := src.readDir(dir)
		if err != nil {
			t.Fatalf("readDir %s: %v", dir, err)
		}
		want := captureDirNames(t, capUBU, dir)
		if len(want) == 0 {
			t.Fatalf("capture %s/fs/%s is empty", capUBU, dir)
		}
		var names []string
		symlinks := 0
		for _, e := range got {
			names = append(names, e.Name())
			if !e.IsDir() && e.Type()&fs.ModeSymlink != 0 {
				symlinks++
			}
		}
		if !slices.Equal(names, want) {
			t.Errorf("readDir %s = %v, want the capture's %v", dir, names, want)
		}
		if symlinks != len(want) {
			t.Errorf("readDir %s: %d symlink entries (IsDir false), want all %d: F-FS shapes class entries as on a kernel", dir, symlinks, len(want))
		}
	}
}

// captureDirNames lists testdata/<capture>/fs/<dir> directly (unrecorded).
func captureDirNames(t *testing.T, capture, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("testdata", capture, "fs", filepath.FromSlash(dir)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TC-HP-ERR10: The allow-list refuses sensitive and escaping paths without reading. Owner WI 2.2. Requirements: NFR-6, CON-1.
//
// - Setup: F-FS(UBU) recording.
// - Steps:
//  1. `src.read` each of: `sys/class/dmi/id/product_serial`, `product_uuid`, `board_serial`, `chassis_serial`, `chassis_asset_tag`, `proc/1/cmdline`, `proc/self/cmdline`, `proc/1/environ`, `proc/1234/environ`, `proc/kmsg`, `dev/kmsg`, `proc/net/arp`, `etc/shadow`, `../etc/shadow`, `/proc/stat`, `proc//stat`, `proc/./stat`, `proc/1/root/etc/shadow`, `proc/1/cwd/x`, `sys/class/net/../../../etc/shadow`, `proc/kcore`, `proc/1/mem`.
//  2. Wrong access kinds (Finding 1): `src.readDir("etc")` (no list entry); `src.stat("etc/shadow")` (no entry of any kind); `src.read("usr/bin/chronyc")` (a stat-only entry refuses `Open`); `src.readDir("usr/bin")`; `src.read("proc")` (a list entry refuses `Open`); `src.readDir("proc/1")` (only `proc` is listed, not a pid directory).
//
// - Expected: each returns an internal (programming-bug) error naming the path and the access kind; F-FS recorded none of them, of any kind.
func TestTCHPERR10_TheAllowListRefusesSensitiveAndEscaping(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	src := newSource(ffs, 250*time.Millisecond, nil)
	refused := func(name, kind string, err error) {
		t.Helper()
		if !errors.Is(err, errNotAllowed) {
			t.Errorf("%s %q: err = %v, want the allow-list's refusal", kind, name, err)
			return
		}
		if msg := err.Error(); !strings.Contains(msg, name) || !strings.Contains(msg, "("+kind+")") {
			t.Errorf("%s %q: error %q does not name the path and the access kind", kind, name, msg)
		}
		if r := reasonFor(name, err); r != "internal error: access not allow-listed" {
			t.Errorf("%s %q: reason %q", kind, name, r)
		}
	}
	for _, name := range []string{
		"sys/class/dmi/id/product_serial", "sys/class/dmi/id/product_uuid", "sys/class/dmi/id/board_serial",
		"sys/class/dmi/id/chassis_serial", "sys/class/dmi/id/chassis_asset_tag",
		"proc/1/cmdline", "proc/self/cmdline", "proc/1/environ", "proc/1234/environ",
		"proc/kmsg", "dev/kmsg", "proc/net/arp", "etc/shadow", "../etc/shadow", "/proc/stat",
		"proc//stat", "proc/./stat", "proc/1/root/etc/shadow", "proc/1/cwd/x",
		"sys/class/net/../../../etc/shadow", "proc/kcore", "proc/1/mem",
	} {
		_, err := src.read(name)
		refused(name, "read", err)
	}
	_, err := src.readDir("etc")
	refused("etc", "list", err)
	_, err = src.stat("etc/shadow")
	refused("etc/shadow", "stat", err)
	_, err = src.read("usr/bin/chronyc")
	refused("usr/bin/chronyc", "read", err)
	_, err = src.readDir("usr/bin")
	refused("usr/bin", "list", err)
	_, err = src.read("proc")
	refused("proc", "read", err)
	_, err = src.readDir("proc/1")
	refused("proc/1", "list", err)
	_, err = src.readLink("etc/passwd")
	refused("etc/passwd", "link", err)

	if got := ffs.recorded(); len(got) != 0 {
		t.Errorf("F-FS recorded %v; a refused access must never reach it", got)
	}
}

// TC-HP-ERR11: Dynamic path segments are validated. Owner WI 2.2. Requirements: NFR-6.
//
// - Setup: F-FS(UBU) recording.
// - Steps: build paths through the source's segment helper with interface/disk/pid/hwmon segments `.`, `..`, `a/b`, `""`, `eth0\x00`, `self` for a pid pattern other than `proc/self/mountinfo`.
// - Expected: each refused before any F-FS call; valid segments (`eth0`, `nvme0n1`, `1234`, `hwmon0`) accepted.
func TestTCHPERR11_DynamicPathSegmentsAreValidated(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	src := newSource(ffs, 250*time.Millisecond, nil)
	builders := []struct {
		what  string
		build func(seg string) (string, error)
	}{
		{"interface", func(s string) (string, error) { return segPath(segName, "sys/class/net", s, "mtu") }},
		{"disk", func(s string) (string, error) { return segPath(segName, "sys/block", s, "size") }},
		{"pid", func(s string) (string, error) { return segPath(segPID, "proc", s, "stat") }},
		{"hwmon", func(s string) (string, error) { return segPath(segName, "sys/class/hwmon", s, "name") }},
	}
	for _, b := range builders {
		bad := []string{".", "..", "a/b", "", "eth0\x00"}
		if b.what == "pid" {
			bad = append(bad, "self", "thread-self", "-1", "1a")
		}
		for _, seg := range bad {
			if name, err := b.build(seg); err == nil || !errors.Is(err, errNotAllowed) {
				t.Errorf("%s segment %q: built %q, %v; want a refusal", b.what, seg, name, err)
			}
		}
	}
	if got := ffs.recorded(); len(got) != 0 {
		t.Fatalf("F-FS recorded %v before any valid segment", got)
	}

	for _, v := range []struct {
		build int
		seg   string
		want  string
	}{
		{0, "eth0", "sys/class/net/eth0/mtu"},
		{1, "nvme0n1", "sys/block/nvme0n1/size"},
		{2, "1234", "proc/1234/stat"},
		{3, "hwmon0", "sys/class/hwmon/hwmon0/name"},
	} {
		name, err := builders[v.build].build(v.seg)
		if err != nil || name != v.want {
			t.Errorf("%s segment %q: %q, %v; want %q", builders[v.build].what, v.seg, name, err, v.want)
			continue
		}
		// The built path passes the allow-list and reaches F-FS (eth0
		// exists in the capture; the others are absent there).
		if _, err := src.read(name); errors.Is(err, errNotAllowed) {
			t.Errorf("read %s: refused by the allow-list", name)
		}
		if ffs.count("open", name) != 1 {
			t.Errorf("read %s did not reach F-FS", name)
		}
	}
}

// TC-HP-ERR12: A hung statfs times out, and a second call reports it still pending. Owner WI 2.2. Requirements: FR-7, FR-12, NFR-1.
//
// - Setup: F-STATFS `hang("/mnt/nfs")`, which blocks the whole seam call (stat and statfs) before it returns `Usage` (Finding 2); timeout 250 ms; goroutine baseline taken.
// - Steps: `src.statfs(ctx, "/mnt/nfs")` (WI 2.2) and time it; call again at once, when the call in flight is already `sourceTimeout` old (the stuck branch, A-42); `release()`; wait until the first goroutine finished; call a third time.
// - Expected: first: `timed out after 250ms: statfs /mnt/nfs` within 350 ms; second: `still pending: statfs /mnt/nfs` within 10 ms, F-STATFS count still 1, and `runtime.NumGoroutine()` at most baseline + 1 (Finding 3); third: a new call (count 2) that returns the release result, the `Usage` F-STATFS was configured with, `IsDir` included.
func TestTCHPERR12_AHungStatfsTimesOutAndA(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	st := newFakeStatfs(t, capUBU)
	want := Usage{IsDir: true, Blocks: 1000, Bfree: 400, Bavail: 300, Files: 50, Ffree: 20, Frsize: 4096}
	st.set("/mnt/nfs", want)
	release := st.hang("/mnt/nfs")
	src := newSource(ffs, 250*time.Millisecond, st.Statfs)
	ctx := t.Context()
	baseline := runtime.NumGoroutine()

	start := time.Now()
	_, err := src.statfs(ctx, "/mnt/nfs")
	if el := time.Since(start); el > 350*time.Millisecond {
		t.Errorf("first call took %v, want at most 350ms", el)
	}
	if r := reasonFor("/mnt/nfs", err); r != "timed out after 250ms: statfs /mnt/nfs" {
		t.Errorf("first call: reason %q", r)
	}

	start = time.Now()
	_, err = src.statfs(ctx, "/mnt/nfs")
	if el := time.Since(start); el > 10*time.Millisecond {
		t.Errorf("second call took %v, want at most 10ms", el)
	}
	if r := reasonFor("/mnt/nfs", err); r != "still pending: statfs /mnt/nfs" {
		t.Errorf("second call: reason %q", r)
	}
	if n := st.count("/mnt/nfs"); n != 1 {
		t.Errorf("F-STATFS count %d while hung, want 1", n)
	}
	if n := runtime.NumGoroutine(); n > baseline+1 {
		t.Errorf("goroutines %d, want at most baseline %d + 1", n, baseline)
	}

	release()
	waitGoroutines(t, baseline)
	eventually(t, time.Second, func() bool { return !src.inFlight("statfs /mnt/nfs") }, "key cleared")
	got, err := src.statfs(ctx, "/mnt/nfs")
	if err != nil || got != want {
		t.Errorf("third call = %+v, %v; want %+v", got, err, want)
	}
	if n := st.count("/mnt/nfs"); n != 2 {
		t.Errorf("F-STATFS count %d, want 2 (a new call)", n)
	}
	if got := ffs.recorded(); len(got) != 0 {
		t.Errorf("F-FS recorded %v; the statfs seam never goes through the FS", got)
	}
}

// TC-HP-E30: List and stat-only allow-list entries accept their own access kind. Owner WI 2.2. Requirements: NFR-6, FR-5, FR-6, FR-8, FR-11.
//
// - Setup: F-FS(UBU) recording. A variant uses `replace` to create the four clock-source paths, so each exists once and is absent once.
// - Steps:
//  1. `src.readDir` each list entry of Finding 1: `proc`, `sys/class/net`, `sys/block`, `sys/block/<each capture disk>`, `sys/class/hwmon`, `sys/class/hwmon/<each capture hwmon>` and `sys/class/thermal`.
//  2. `src.stat` each stat-only entry: `usr/bin/chronyc`, `run/systemd/timesync`, `run/chrony/chronyd.pid` and `run/ntpd.pid`, in the present and the absent variants.
//  3. `src.stat("proc/stat")`: a read entry also allows `Stat`.
//
// - Expected:
//   - No call returns the allow-list's internal error.
//   - Each listing returns the capture's entries (symlink entries included, as TC-HP-E7 requires). The `proc` listing includes the capture's numeric pid directories.
//   - Each stat returns a `fs.FileInfo` when the path is present, and a `*fs.PathError` with `fs.ErrNotExist` when it is absent.
//   - F-FS recorded every call with its access kind, so the allow-list let it through to the filesystem.
func TestTCHPE30_ListAndStatOnlyAllowListEntriesAccept(t *testing.T) {
	clock := []string{"usr/bin/chronyc", "run/systemd/timesync", "run/chrony/chronyd.pid", "run/ntpd.pid"}
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("clock sources present=%v", present), func(t *testing.T) {
			ffs := fixtureFS(t, capUBU)
			if present {
				for _, p := range clock {
					ffs.replace(p, nil)
				}
			}
			src := newSource(ffs, 250*time.Millisecond, nil)
			var want []fsCall

			// Step 1: every list entry.
			lists := []string{"proc", "sys/class/net", "sys/block"}
			for _, d := range captureDirNames(t, capUBU, "sys/block") {
				lists = append(lists, "sys/block/"+d)
			}
			lists = append(lists, "sys/class/hwmon")
			for _, h := range captureDirNames(t, capUBU, "sys/class/hwmon") {
				lists = append(lists, "sys/class/hwmon/"+h)
			}
			lists = append(lists, "sys/class/thermal")
			for _, dir := range lists {
				want = append(want, fsCall{op: "readdir", name: dir})
				entries, err := src.readDir(dir)
				if err != nil {
					t.Errorf("readDir %s: %v", dir, err)
					continue
				}
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				if wantNames := captureDirNames(t, capUBU, dir); !slices.Equal(names, wantNames) {
					t.Errorf("readDir %s = %v, want the capture's %v", dir, names, wantNames)
				}
				if dir == "proc" && !slices.ContainsFunc(names, func(n string) bool { return n != "1" && allDigits(n) }) {
					t.Errorf("readDir proc = %v, want the capture's pid directories", names)
				}
			}

			// Step 2: every clock stat-only entry, present or absent.
			for _, p := range clock {
				want = append(want, fsCall{op: "stat", name: p})
				fi, err := src.stat(p)
				var pe *fs.PathError
				switch {
				case errors.Is(err, errNotAllowed):
					t.Errorf("stat %s: refused by the allow-list", p)
				case present && (err != nil || fi == nil):
					t.Errorf("stat %s (present) = %v, %v; want a FileInfo", p, fi, err)
				case !present && (!errors.As(err, &pe) || !errors.Is(err, fs.ErrNotExist)):
					t.Errorf("stat %s (absent) err = %v, want a *fs.PathError with fs.ErrNotExist", p, err)
				}
			}

			// Step 3: a read entry also allows Stat.
			want = append(want, fsCall{op: "stat", name: "proc/stat"})
			if _, err := src.stat("proc/stat"); err != nil {
				t.Errorf("stat proc/stat: %v", err)
			}

			if got := ffs.recorded(); !slices.Equal(got, want) {
				t.Errorf("F-FS recorded\n%v\nwant\n%v", got, want)
			}
		})
	}
}

// TC-HP-135: The committed captures are redacted. Owner WI 2.1. Requirements: NFR-6, NFR-5.
//
// - Setup: a scanner written in the test, not shared with `capture.sh`, that finds:
//   - MAC tokens: `[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}`, bounded by characters that are neither hex digits nor colons;
//   - IPv4: dotted tokens of four 0–255 octets, bounded by characters other than digits and dots, plus the Destination and Gateway columns of `proc/net/route`, decoded from little-endian hex;
//   - IPv6: candidate hex-and-colon tokens parsed with `netip.ParseAddr`, plus the destination, source and next-hop 32-hex columns of `proc/net/ipv6_route`.
//
// - Steps:
//  1. Self-test the scanner on an in-memory tree with one planted value of each kind: `00:1a:2b:3c:4d:5e` in `sys/class/net/eth0/address`, `08080808` as a `proc/net/route` Gateway, `8.8.4.4` in `etc/resolv.conf`, `2a00145000000000000000000000200e` as a `proc/net/ipv6_route` destination, and `2a00:1450::200e` in `tools/ip-addr`.
//  2. Scan every file under `fs/`, `t1/` and `tools/` of both captures.
//  3. Read `fs/proc/sys/kernel/hostname`, `tools/hostname` and `PROVENANCE`.
//
// - Expected:
//
//   - Step 1 flags each planted value, so the scanner is not vacuous.
//
//   - Step 2 finds, in either capture:
//
//   - no universally administered MAC (first octet with bit 0x02 clear), other than `00:00:00:00:00:00` and `ff:ff:ff:ff:ff:ff`;
//
//   - no IPv4 outside 10/8, 172.16/12, 192.168/16, 127/8, 169.254/16, 100.64/10 and 192.0.2.0/24. `0.0.0.0`, `255.255.255.255` and contiguous netmasks are not counted as addresses;
//
//   - no address in 2000::/3 outside 2001:db8::/32.
//
//     Each failure names the file and the token.
//
//   - Step 2 is non-vacuous: in each capture it finds at least one MAC (a `02:00:00:00:00:NN` value) and decodes at least one default route from `proc/net/route`.
//
//   - The hostname files hold exactly `ubuntu-capture` (UBU) and `alpine-capture` (ALP), and `tools/hostname` equals `fs/proc/sys/kernel/hostname`.
//
//   - `PROVENANCE` has a `redaction:` line whose value is not `off`, so a capture made with `--no-redact` fails here (test-strategy.md Open Item 8).
func TestTCHP135_TheCommittedCapturesAreRedacted(t *testing.T) {
	// Step 1: the scanner flags one planted value of each kind.
	planted := fstest.MapFS{
		"sys/class/net/eth0/address": {Data: []byte("00:1a:2b:3c:4d:5e\n")},
		"proc/net/route": {Data: []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
			"eth0\t00000000\t08080808\t0003\t0\t0\t0\t00000000\t0\t0\t0\n")},
		"etc/resolv.conf": {Data: []byte("nameserver 8.8.4.4\n")},
		"proc/net/ipv6_route": {Data: []byte("2a00145000000000000000000000200e 80 00000000000000000000000000000000 00 " +
			"00000000000000000000000000000000 00000000 00000001 00000000 00000001     eth0\n")},
		"tools/ip-addr": {Data: []byte("    inet6 2a00:1450::200e/64 scope global\n")},
	}
	self := scanCaptureTree(t, planted, ".")
	want := map[string]string{
		"sys/class/net/eth0/address": "00:1a:2b:3c:4d:5e",
		"proc/net/route":             "08080808",
		"etc/resolv.conf":            "8.8.4.4",
		"proc/net/ipv6_route":        "2a00145000000000000000000000200e",
		"tools/ip-addr":              "2a00:1450::200e",
	}
	for file, token := range want {
		found := false
		for _, f := range self.findings {
			if f.file == file && strings.Contains(f.token, token) {
				found = true
			}
		}
		if !found {
			t.Errorf("scanner self-test: planted %q in %s was not flagged (findings %v)", token, file, self.findings)
		}
	}
	if len(self.findings) != len(want) {
		t.Errorf("scanner self-test: %d findings, want %d: %v", len(self.findings), len(want), self.findings)
	}
	if self.defaultRoutes != 1 {
		t.Errorf("scanner self-test: decoded %d default routes, want 1", self.defaultRoutes)
	}

	mapped := regexp.MustCompile(`^02:00:00:00:00:[0-9a-f]{2}$`)
	for _, c := range hostCaptures {
		t.Run(c.dir, func(t *testing.T) {
			root := filepath.Join("testdata", c.dir)
			fsys := os.DirFS(root)
			var scan captureScan
			for _, sub := range []string{"fs", "t1", "tools"} {
				s := scanCaptureTree(t, fsys, sub)
				scan.findings = append(scan.findings, s.findings...)
				scan.macs = append(scan.macs, s.macs...)
				scan.defaultRoutes += s.defaultRoutes
			}
			// Step 2.
			for _, f := range scan.findings {
				t.Errorf("%s/%s: unredacted %s", c.dir, f.file, f.token)
			}
			nMapped := 0
			for _, m := range scan.macs {
				if mapped.MatchString(m) {
					nMapped++
				}
			}
			if nMapped == 0 {
				t.Errorf("%s: no 02:00:00:00:00:NN MAC found (MACs seen: %v): the scan is vacuous", c.dir, scan.macs)
			}
			if scan.defaultRoutes == 0 {
				t.Errorf("%s: no default route decoded from fs/proc/net/route: the scan is vacuous", c.dir)
			}

			// Step 3.
			kernel := readCaptureFile(t, filepath.Join(root, "fs", "proc", "sys", "kernel", "hostname"))
			tool := readCaptureFile(t, filepath.Join(root, "tools", "hostname"))
			if strings.TrimSuffix(kernel, "\n") != c.hostname {
				t.Errorf("%s/fs/proc/sys/kernel/hostname = %q, want %q", c.dir, kernel, c.hostname)
			}
			if tool != kernel {
				t.Errorf("%s/tools/hostname = %q, want fs/proc/sys/kernel/hostname's %q", c.dir, tool, kernel)
			}
			if v := readProvenance(t, root)["redaction"]; len(v) != 1 || v[0] == "" || v[0] == "off" {
				t.Errorf("%s/PROVENANCE: redaction: %q, want one line that is not off", c.dir, v)
			}
		})
	}
}

// TC-HP-E32: Concurrent calls join a healthy in-flight call, and a joiner times out at the first call's deadline. Owner WI 2.2. Requirements: FR-12, NFR-1.
//
// - Setup: `newSource(F-FS(UBU), 250ms, F-STATFS)`. F-STATFS has `delay("/mnt/slow", 50ms)` and `delay("/mnt/late", 400ms)`, both answering with the root row of `tools/statfs.txt`. A goroutine baseline is taken.
// - Steps:
//  1. Start two `src.statfs(ctx, "/mnt/slow")` calls at once (a barrier releases both).
//  2. Call `src.statfs(ctx, "/mnt/late")` at t0. Call it again 100 ms later, and a third time at t0 + 300 ms.
//  3. Wait (`eventually`) until F-STATFS's `/mnt/late` call has returned and the key is cleared. Set its delay to 0, and call it a fourth time.
//
// - Expected:
//   - Step 1 (the join branch, A-42): both calls return the same `Usage` with no error, at least 40 ms after they start and under the 250 ms timeout, and neither gets `still pending`. F-STATFS's count for `/mnt/slow` is 1.
//   - Step 2:
//   - The first call returns `timed out after 250ms: statfs /mnt/late` at least 250 ms after its start and under the 400 ms the seam takes.
//   - The second call joined a call aged 100 ms. It returns the same reason at the first call's deadline (the first call's start + `sourceTimeout`): no earlier than that deadline, and under 250 ms after its own start. A joiner that waits a fresh `sourceTimeout` of its own takes at least 250 ms, so that bound is the line between the two; the correct path ends near 150 ms, 100 ms inside it.
//   - The third call finds the call in flight for 300 ms, which is `sourceTimeout` or longer (the stuck branch). It returns `still pending: statfs /mnt/late` in under 100 ms, before the seam returns.
//   - F-STATFS's count for `/mnt/late` stays 1, and `runtime.NumGoroutine()` stays at or below baseline + 1 throughout.
//   - Step 3 (the start branch): the fourth call starts a new seam call (count 2) and returns the row.
//   - Goroutines return to the baseline.
func TestTCHPE32_ConcurrentCallsJoinAHealthyInFlight(t *testing.T) {
	st := newFakeStatfs(t, capUBU)
	rootRow := st.row("/")
	st.set("/mnt/slow", rootRow)
	st.set("/mnt/late", rootRow)
	st.delay("/mnt/slow", 50*time.Millisecond)
	st.delay("/mnt/late", 400*time.Millisecond)
	src := newSource(fixtureFS(t, capUBU), 250*time.Millisecond, st.Statfs)
	ctx := t.Context()
	baseline := runtime.NumGoroutine()

	// The test's own goroutines are counted apart from bounded's.
	var mine atomic.Int64
	checkGoroutines := func(when string) {
		t.Helper()
		if n := runtime.NumGoroutine() - int(mine.Load()); n > baseline+1 {
			t.Errorf("%s: %d goroutines besides the test's, want at most baseline %d + 1", when, n, baseline)
		}
	}
	type result struct {
		u          Usage
		err        error
		start, end time.Time
		took       time.Duration
	}
	call := func(mount string) result {
		s := time.Now()
		u, err := src.statfs(ctx, mount)
		e := time.Now()
		return result{u, err, s, e, e.Sub(s)}
	}
	async := func(mount string) <-chan result {
		ch := make(chan result, 1)
		mine.Add(1)
		go func() {
			defer mine.Add(-1)
			ch <- call(mount)
		}()
		return ch
	}

	// Step 1: two calls released together join one healthy seam call.
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	slow := make([]result, 2)
	for i := range 2 {
		mine.Add(1)
		wg.Go(func() {
			defer mine.Add(-1)
			<-barrier
			slow[i] = call("/mnt/slow")
		})
	}
	close(barrier)
	wg.Wait()
	for i, r := range slow {
		if r.err != nil || r.u != rootRow {
			t.Errorf("step 1 call %d = %+v, %v; want the root row", i, r.u, r.err)
		}
		// Only the lower bound proves the wait; the upper one is the next
		// deadline, not a tight window a loaded -race run could miss.
		if r.took < 40*time.Millisecond || r.took >= 250*time.Millisecond {
			t.Errorf("step 1 call %d took %v, want at least 40ms and under the 250ms timeout", i, r.took)
		}
	}
	if n := st.count("/mnt/slow"); n != 1 {
		t.Errorf("F-STATFS count for /mnt/slow = %d, want 1 (joined)", n)
	}
	waitGoroutines(t, baseline)

	// Step 2: a 400 ms call, joined at 100 ms and found stuck at 300 ms.
	t0 := time.Now()
	first := async("/mnt/late")
	time.Sleep(100*time.Millisecond - time.Since(t0))
	checkGoroutines("first call in flight")
	second := async("/mnt/late")
	time.Sleep(300*time.Millisecond - time.Since(t0))
	checkGoroutines("second call joined")
	third := call("/mnt/late")
	r1, r2 := <-first, <-second
	checkGoroutines("after the three calls")

	const timedOut = "timed out after 250ms: statfs /mnt/late"
	if r := reasonFor("/mnt/late", r1.err); r != timedOut {
		t.Errorf("first call: reason %q", r)
	}
	if r1.took < 250*time.Millisecond || r1.took >= 400*time.Millisecond {
		t.Errorf("first call took %v, want at least 250ms and under the 400ms the seam takes", r1.took)
	}
	if r := reasonFor("/mnt/late", r2.err); r != timedOut {
		t.Errorf("second call: reason %q", r)
	}
	// The joiner must end at the first call's deadline, never at a fresh
	// timeout of its own. Both bounds are exact, not tuned windows: the
	// first call's flight starts no earlier than r1.start, so its deadline
	// is no earlier than r1.start+250ms; and a joiner on its own timer
	// cannot fire before r2.start+250ms. The correct path ends about
	// 100ms (the join offset) inside the upper bound.
	if deadline := r1.start.Add(250 * time.Millisecond); r2.end.Before(deadline) {
		t.Errorf("second call ended %v before the first call's deadline", deadline.Sub(r2.end))
	}
	if r2.took >= 250*time.Millisecond {
		t.Errorf("second call took %v, want under 250ms: it joined at %v and must end at the first call's deadline, not a fresh 250ms timeout of its own", r2.took, r2.start.Sub(r1.start))
	}
	if r := reasonFor("/mnt/late", third.err); r != "still pending: statfs /mnt/late" {
		t.Errorf("third call: reason %q", r)
	}
	if third.took >= 100*time.Millisecond {
		t.Errorf("third call took %v, want under 100ms (it started 100ms before the seam returns)", third.took)
	}
	if n := st.count("/mnt/late"); n != 1 {
		t.Errorf("F-STATFS count for /mnt/late = %d, want 1", n)
	}

	// Step 3: once the seam returns and the key clears, a fresh call.
	eventually(t, time.Second, func() bool { return !src.inFlight("statfs /mnt/late") }, "the /mnt/late key cleared")
	st.delay("/mnt/late", 0)
	if r := call("/mnt/late"); r.err != nil || r.u != rootRow {
		t.Errorf("fourth call = %+v, %v; want the root row", r.u, r.err)
	}
	if n := st.count("/mnt/late"); n != 2 {
		t.Errorf("F-STATFS count for /mnt/late = %d, want 2", n)
	}
	waitGoroutines(t, baseline)
}

// hostCaptures are the two committed WI 2.1 captures under testdata/.
var hostCaptures = []struct {
	dir      string // testdata/<dir>
	hostname string // the redacted hostname
	failed   string // the failed-services output in tools/
	lsblk    bool   // tools/lsblk is required
}{
	{dir: "ubuntu-systemd", hostname: "ubuntu-capture", failed: "systemctl-failed", lsblk: true},
	{dir: "alpine-openrc", hostname: "alpine-capture", failed: "rc-status-crashed"},
}

func readCaptureFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func captureLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// readProvenance parses a capture's PROVENANCE, one `key: value` per line.
func readProvenance(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, l := range captureLines(readCaptureFile(t, filepath.Join(root, "PROVENANCE"))) {
		k, v, ok := strings.Cut(l, ": ")
		if !ok {
			t.Errorf("%s/PROVENANCE: line %q is not key: value", root, l)
			continue
		}
		out[k] = append(out[k], strings.TrimSpace(v))
	}
	return out
}

var captureOctal = regexp.MustCompile(`\\[0-7]{3}`)

// captureMountPoints returns the distinct mount points (field 5, octal
// escapes decoded) of a mountinfo file, in file order.
func captureMountPoints(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	for _, l := range captureLines(readCaptureFile(t, name)) {
		f := strings.Fields(l)
		if len(f) < 5 {
			t.Fatalf("%s: malformed line %q", name, l)
		}
		mp := captureOctal.ReplaceAllStringFunc(f[4], func(e string) string {
			n, _ := strconv.ParseUint(e[1:], 8, 8)
			return string(rune(n))
		})
		if !seen[mp] {
			seen[mp] = true
			out = append(out, mp)
		}
	}
	return out
}

// parseStatMountsLine splits a `stat -c '%d %F %n'` line. %F can hold a
// space, so it is matched against stat's file type names.
func parseStatMountsLine(line string) (mountPoint, fileType string, ok bool) {
	dev, rest, ok := strings.Cut(line, " ")
	if _, err := strconv.ParseUint(dev, 10, 64); !ok || err != nil {
		return "", "", false
	}
	for _, typ := range []string{"regular empty file", "regular file", "directory", "symbolic link", "character special file", "block special file", "fifo", "socket"} {
		if mp, found := strings.CutPrefix(rest, typ+" "); found && strings.HasPrefix(mp, "/") {
			return mp, typ, true
		}
	}
	return "", "", false
}

// parseStatfsLine checks a `stat -f -c '%b %f %a %S %c %d %n'` line.
func parseStatfsLine(line string) (mountPoint string, ok bool) {
	f := strings.SplitN(line, " ", 7)
	if len(f) != 7 || !strings.HasPrefix(f[6], "/") {
		return "", false
	}
	for _, n := range f[:6] {
		if _, err := strconv.ParseUint(n, 10, 64); err != nil {
			return "", false
		}
	}
	return f[6], true
}

// The TC-HP-135 scanner. It is written here, independently of capture.sh's
// redaction, and flags identifiers that redaction must have removed.
type captureFinding struct{ file, token string }

type captureScan struct {
	findings      []captureFinding
	macs          []string
	defaultRoutes int
}

var (
	captureHexColonRun = regexp.MustCompile(`[0-9A-Fa-f:]+`)
	captureDottedRun   = regexp.MustCompile(`[0-9.]+`)
	captureMAC         = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`)
	captureIPv4Kept    = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.7.0/16"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.2.0/24"),
	}
	captureGlobal6 = netip.MustParsePrefix("2000::/3")
	captureDoc6    = netip.MustParsePrefix("2001:db8::/32")
)

// captureIPv4Public reports an IPv4 that redaction must have mapped:
// outside the kept ranges, and not 0.0.0.0, 255.255.255.255 or a
// contiguous netmask.
func captureIPv4Public(a netip.Addr) bool {
	for _, p := range captureIPv4Kept {
		if p.Contains(a) {
			return false
		}
	}
	b := a.As4()
	inv := ^(uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]))
	return inv&(inv+1) != 0
}

func captureIPv6Global(a netip.Addr) bool {
	return a.Is6() && !a.Is4In6() && captureGlobal6.Contains(a) && !captureDoc6.Contains(a)
}

func scanCaptureTree(t *testing.T, fsys fs.FS, root string) captureScan {
	t.Helper()
	var s captureScan
	err := fs.WalkDir(fsys, root, func(name string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		scanCaptureFile(name, string(b), &s)
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return s
}

func scanCaptureFile(name, text string, s *captureScan) {
	flag := func(token string) { s.findings = append(s.findings, captureFinding{file: name, token: token}) }
	for _, tok := range captureHexColonRun.FindAllString(text, -1) {
		if captureMAC.MatchString(tok) {
			s.macs = append(s.macs, tok)
			first, _ := strconv.ParseUint(tok[:2], 16, 8)
			l := strings.ToLower(tok)
			if first&0x02 == 0 && l != "00:00:00:00:00:00" && l != "ff:ff:ff:ff:ff:ff" {
				flag("MAC " + tok)
			}
			continue
		}
		if a, err := netip.ParseAddr(tok); err == nil && captureIPv6Global(a) {
			flag("IPv6 " + tok)
		}
	}
	for _, tok := range captureDottedRun.FindAllString(text, -1) {
		if a, err := netip.ParseAddr(tok); err == nil && a.Is4() && captureIPv4Public(a) {
			flag("IPv4 " + tok)
		}
	}
	switch {
	case name == "proc/net/route" || strings.HasSuffix(name, "/proc/net/route"):
		for _, l := range captureLines(text)[1:] {
			f := strings.Fields(l)
			if len(f) < 8 {
				continue
			}
			for _, col := range f[1:3] {
				b, err := hex.DecodeString(col)
				if err != nil || len(b) != 4 {
					continue
				}
				a := netip.AddrFrom4([4]byte{b[3], b[2], b[1], b[0]})
				if captureIPv4Public(a) {
					flag("IPv4 " + col + " (" + a.String() + ")")
				}
			}
			if f[1] == "00000000" && f[7] == "00000000" {
				s.defaultRoutes++
			}
		}
	case path.Base(name) == "ipv6_route":
		for _, l := range captureLines(text) {
			f := strings.Fields(l)
			for i := 0; i <= 4 && i < len(f); i += 2 {
				b, err := hex.DecodeString(f[i])
				if err != nil || len(b) != 16 {
					continue
				}
				if a := netip.AddrFrom16([16]byte(b)); captureIPv6Global(a) {
					flag("IPv6 " + f[i] + " (" + a.String() + ")")
				}
			}
		}
	}
}
