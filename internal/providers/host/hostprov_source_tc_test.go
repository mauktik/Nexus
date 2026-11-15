// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-21, TC-HP-22, TC-HP-23, TC-HP-24, TC-HP-25, TC-HP-26, TC-HP-27, TC-HP-E6, TC-HP-E7, TC-HP-ERR10, TC-HP-ERR11, TC-HP-ERR12, TC-HP-E30.
// Owner WIs: 2.1, 2.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-HP-21: Both captures are complete and documented. Owner WI 2.1. Requirements: NFR-4, NFR-5, CON-5.
//
// - Setup: none.
// - Steps: for UBU and ALP, stat `fs/`, `t1/`, `tools/`, `PROVENANCE`; read `PROVENANCE`; stat `testdata/capture.sh` and `testdata/README.md`.
// - Expected:
//   - all exist; `PROVENANCE` has non-empty `host:`, `kernel:`, `date:` and `under-unit:` lines;
//   - `fs/proc/stat`, `fs/proc/meminfo`, `fs/proc/1/mountinfo` (or a documented absence), `t1/proc/stat`, `t1/proc/net/dev`, `t1/proc/diskstats` exist;
//   - `tools/` holds `free`, `df`, `vmstat`, `uname-rm`, `nproc`, `hostname`, `statfs.txt`, `stat-mounts.txt` (one `stat -c '%d %F %n'` line per mountinfo mount point; the F-STATFS `Dev` and `IsDir` source, Finding 2), `ip-addr`, `ip-route`, and `lsblk` (UBU), and the failed-services output;
//   - README says the files are vectors, not proofs (NFR-5), and how passwd was reduced.
func TestTCHP21_BothCapturesAreCompleteAndDocumented(t *testing.T) {
	t.Skip("TC-HP-21: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-HP-22: No sensitive file is committed in the captures. Owner WI 2.1. Requirements: NFR-6.
//
// - Setup: none.
// - Steps: walk `testdata/`; parse each capture's `fs/etc/passwd`.
// - Expected: no path whose base name is `product_serial`, `product_uuid`, `board_serial`, `chassis_serial`, `chassis_asset_tag`, `cmdline`, `environ`, `kmsg`, `shadow`, `gshadow` or `arp`; every passwd entry has uid < 1000 or uid 65534; `capture.sh` contains a guard that exits non-zero when any of those names appears in `OUTDIR` (grep for each name in the guard).
func TestTCHP22_NoSensitiveFileIsCommittedInThe(t *testing.T) {
	t.Skip("TC-HP-22: SKIP-only stub; un-skip when WI 2.1 lands")
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
// - Expected: `permission denied: /proc/1/mountinfo` (twice), `not present: /proc/1/mountinfo`, `timed out after 2s: statfs /mnt/x`, `unrecognized format: /proc/stat`; no reason contains `secret-token-xyz`; every reason starts with one of the prefixes of the architecture's table, which has thirteen after Finding 2 adds `not visible:`.
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
// - Setup: F-STATFS `hang("/mnt/nfs")`, which blocks the whole seam call (stat and statfs) before it returns `Usage`, `Dev` or `IsDir` (Finding 2); timeout 250 ms; goroutine baseline taken.
// - Steps: `statfsBounded(ctx, "/mnt/nfs")` and time it; call again at once; `release()`; wait until the first goroutine finished; call a third time.
// - Expected: first: `timed out after 250ms: statfs /mnt/nfs` within 350 ms; second: `still pending: statfs /mnt/nfs` within 10 ms, F-STATFS count still 1, and `runtime.NumGoroutine()` at most baseline + 1 (Finding 3); third: a new call (count 2) that returns the release result, including its `Dev` and `IsDir`.
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
