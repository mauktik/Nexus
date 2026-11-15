// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-21, TC-HP-22, TC-HP-23, TC-HP-24, TC-HP-25, TC-HP-26, TC-HP-27, TC-HP-E6, TC-HP-E7, TC-HP-ERR10, TC-HP-ERR11, TC-HP-ERR12, TC-HP-E30, TC-HP-135, TC-HP-E32.
// Owner WIs: 2.1, 2.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import (
	"encoding/hex"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

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
	t.Skip("TC-HP-23: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-HP-24: Reads ignore a zero procfs size and cap at 4 MiB. Owner WI 2.2. Requirements: NFR-4, FR-12.
//
// - Setup: F-FS(UBU) (procfs `Stat` reports size 0); `replace("proc/stat", 5 MiB of valid-looking lines)`.
// - Steps: `src.read("proc/meminfo")`; `src.read("proc/stat")` after the replace.
// - Expected: `proc/meminfo` returns the full capture content despite size 0; the 5 MiB read returns an error (not a truncated success), and F-FS's byte counter shows at most 4 MiB + 1 byte read.
func TestTCHP24_ReadsIgnoreAZeroProcfsSizeAnd(t *testing.T) {
	t.Skip("TC-HP-24: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-HP-25: Reason classification uses the fixed vocabulary and never quotes content. Owner WI 2.2. Requirements: FR-12, NFR-6.
//
// - Setup: a table of injected errors.
// - Steps: `reasonFor(path, err)` for: `&fs.PathError{Op:"open", Path:"proc/1/mountinfo", Err: syscall.EACCES}`, the same with `EPERM`, with `ENOENT`, a `bounded` timeout of 2 s on `statfs /mnt/x`, and a parse error carrying the line `secret-token-xyz`.
// - Expected: `permission denied: /proc/1/mountinfo` (twice), `not present: /proc/1/mountinfo`, `timed out after 2s: statfs /mnt/x`, `unrecognized format: /proc/stat`; no reason contains `secret-token-xyz`; every reason starts with one of the prefixes of the architecture's Error Handling table (the test holds the table's prefix column as a list, with no count).
func TestTCHP25_ReasonClassificationUsesTheFixedVocabularyAnd(t *testing.T) {
	t.Skip("TC-HP-25: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-HP-26: No file read bypasses the allow-list. Owner WI 2.2. Requirements: NFR-6, CON-1.
//
// - Setup: none.
// - Steps: parse every non-test `.go` file of `internal/providers/host` (excluding `hostparse/`, which takes `[]byte`) with `go/parser`; find calls to `os.Open`, `os.OpenFile`, `os.ReadFile`, `os.ReadDir`, `os.Stat`, `os.Lstat`, `os.Readlink`, `os.DirFS`, `ioutil.*`, `syscall.Open`, `filepath.Walk*`.
// - Expected: the only hit is the default `os.DirFS("/")` in `provider.go`'s `New`; a planted file with `os.ReadFile("/etc/shadow")` in a temp copy of the package is caught (self-test).
func TestTCHP26_NoFileReadBypassesTheAllowList(t *testing.T) {
	t.Skip("TC-HP-26: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-HP-27: Default statfs, uname and addresses are unsupported off Linux. Owner WI 2.2. Requirements: CON-5, FR-3.
//
// - Setup: runs on darwin (`runtime.GOOS != "linux"`; skip on linux).
// - Steps: call the default statfs and uname functions.
// - Expected: both return an error whose reason is `not supported: …`; `GOOS=linux go vet ./internal/providers/host/...` passes (asserted by `make vet`, noted here).
func TestTCHP27_DefaultStatfsUnameAndAddressesAreUnsupported(t *testing.T) {
	t.Skip("TC-HP-27: SKIP-only stub; un-skip when WI 2.2 lands")
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
	t.Skip("TC-HP-E6: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-HP-E7: Source directory listing keeps sysfs symlink entries. Owner WI 2.2. Requirements: FR-8, FR-9, CON-5.
//
// - Setup: F-FS(UBU) (class entries reported as `ModeSymlink`).
// - Steps: `src.readDir("sys/class/net")`, `src.readDir("sys/block")`.
// - Expected: every capture entry is returned, including those whose `IsDir()` is false; names equal the capture's directory names.
func TestTCHPE7_SourceDirectoryListingKeepsSysfsSymlinkEntries(t *testing.T) {
	t.Skip("TC-HP-E7: SKIP-only stub; un-skip when WI 2.2 lands")
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
	t.Skip("TC-HP-ERR10: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-HP-ERR11: Dynamic path segments are validated. Owner WI 2.2. Requirements: NFR-6.
//
// - Setup: F-FS(UBU) recording.
// - Steps: build paths through the source's segment helper with interface/disk/pid/hwmon segments `.`, `..`, `a/b`, `""`, `eth0\x00`, `self` for a pid pattern other than `proc/self/mountinfo`.
// - Expected: each refused before any F-FS call; valid segments (`eth0`, `nvme0n1`, `1234`, `hwmon0`) accepted.
func TestTCHPERR11_DynamicPathSegmentsAreValidated(t *testing.T) {
	t.Skip("TC-HP-ERR11: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-HP-ERR12: A hung statfs times out, and a second call reports it still pending. Owner WI 2.2. Requirements: FR-7, FR-12, NFR-1.
//
// - Setup: F-STATFS `hang("/mnt/nfs")`, which blocks the whole seam call (stat and statfs) before it returns `Usage` (Finding 2); timeout 250 ms; goroutine baseline taken.
// - Steps: `src.statfs(ctx, "/mnt/nfs")` (WI 2.2) and time it; call again at once, when the call in flight is already `sourceTimeout` old (the stuck branch, A-42); `release()`; wait until the first goroutine finished; call a third time.
// - Expected: first: `timed out after 250ms: statfs /mnt/nfs` within 350 ms; second: `still pending: statfs /mnt/nfs` within 10 ms, F-STATFS count still 1, and `runtime.NumGoroutine()` at most baseline + 1 (Finding 3); third: a new call (count 2) that returns the release result, the `Usage` F-STATFS was configured with, `IsDir` included.
func TestTCHPERR12_AHungStatfsTimesOutAndA(t *testing.T) {
	t.Skip("TC-HP-ERR12: SKIP-only stub; un-skip when WI 2.2 lands")
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
	t.Skip("TC-HP-E30: SKIP-only stub; un-skip when WI 2.2 lands")
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
//   - Step 1 (the join branch, A-42): both calls return the same `Usage` with no error in 40–200 ms, and neither gets `still pending`. F-STATFS's count for `/mnt/slow` is 1.
//   - Step 2:
//   - The first call returns `timed out after 250ms: statfs /mnt/late` 250–350 ms after t0.
//   - The second call joined a call aged 100 ms. It returns the same reason at the first call's deadline (start + `sourceTimeout`): 120–220 ms after its own start, never 250 ms after it.
//   - The third call finds the call in flight for 300 ms, which is `sourceTimeout` or longer (the stuck branch). It returns `still pending: statfs /mnt/late` within 10 ms.
//   - F-STATFS's count for `/mnt/late` stays 1, and `runtime.NumGoroutine()` stays at or below baseline + 1 throughout.
//   - Step 3 (the start branch): the fourth call starts a new seam call (count 2) and returns the row.
//   - Goroutines return to the baseline.
func TestTCHPE32_ConcurrentCallsJoinAHealthyInFlight(t *testing.T) {
	t.Skip("TC-HP-E32: SKIP-only stub; un-skip when WI 2.2 lands")
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
