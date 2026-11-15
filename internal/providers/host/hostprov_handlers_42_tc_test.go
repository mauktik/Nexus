// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-79, TC-HP-80, TC-HP-81, TC-HP-82, TC-HP-83, TC-HP-84, TC-HP-85, TC-HP-86, TC-HP-E20, TC-HP-E21, TC-HP-E22, TC-HP-ERR21, TC-HP-ERR22, TC-HP-E31, TC-HP-E33.
// Owner WI: 4.2. Split from hostprov_handlers_tc_test.go by the orchestrator so
// that Phase 4 work items can run in parallel. Each stub compiles today and
// skips; the developer of the owning work item replaces the skip and the
// comment with the test (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-HP-79: Filesystems on both captures agree with statfs and df. Owner WI 4.2. Requirements: FR-7, NFR-3, FR-15.
//
// - Setup: host stack per capture, F-STATFS from `tools/statfs.txt`. F-FS serves the capture's own `proc/1/mountinfo` and `proc/self/mountinfo`, with no `addMount`. When `PROVENANCE` says `under-unit: no`, both files list the same mounts, and every `tools/df` target is expected with numbers. When it says `yes`, targets at or under `/tmp` and `/var/tmp` are left out of this comparison; TC-HP-E31 and TC-HP-117 cover them.
// - Steps: filesystems handler with `limit 1000`.
// - Expected:
//   - every item's mount point is a `tools/df` target, and `/` is present;
//   - `sizeBytes` = `%b × %S`, `usedBytes` = `(%b − %f) × %S`, `availableBytes` = `%a × %S` from `statfs.txt` exactly; `sizeBytes` equals df's size exactly; `usedBytes` within one block of df's used; `usedPercent` = used/(used+available) × 100 and within 1 of df's `Use%`;
//   - inodes `%c`, `%d`, `%c − %d`; `assertExplicit` passes.
func TestTCHP79_FilesystemsOnBothCapturesAgreeWithStatfs(t *testing.T) {
	t.Skip("TC-HP-79: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-80: The exclusion list and bind-mount dedupe. Owner WI 4.2. Requirements: FR-7.
//
// - Setup:
//   - UBU F-FS. Every mount below is added with `addMount(…, both)` and has its own fresh `maj:min` unless stated otherwise. F-FS has no entry for any mount point (Finding 2).
//   - For each fstype of A-21, one line derived from a real line of that type in the capture, or from the root line with the fstype changed and a fresh `maj:min`. Also overlays under each container-runtime directory, `squashfs` at `/snap/core/1`, and tmpfs at `/tmp`, `/run`, `/dev/shm` and `/var/tmp` (the capture's own lines where it has them).
//   - `/mnt/bind`: a bind of the root filesystem, `sameDevAs("/")`, with root `/home` (the dedupe case).
//   - `/etc/hosts`: a file bind from another disk, as Docker makes it, with its own `maj:min` `8:33` (the host disk's, not the root's) and root `/var/lib/docker/containers/c1/hosts`. F-STATFS answers it with `IsDir=false`.
//
// - Steps: filesystems handler with `limit 1000`, in four subtests:
//  1. As set up.
//  2. F-STATFS answers `/etc/hosts` with `IsDir=true` and a row of numbers.
//  3. Add `/srv/appdata`, a directory mount with `sameDevAs("/etc/hosts")` (`8:33`) and root `/appdata`. Its mount point is longer than `/etc/hosts`, so WI 4.2's preference order (mountinfo root `/`, then the shortest mount point) tries the file bind first. (`/srv/data`, the architecture's example, is shorter than `/etc/hosts`, so it would be tried first and the skip would never run.)
//  4. The root line's fstype is `overlay` in both files (a container root).
//
// - Expected:
//   - Subtest 1:
//   - none of the excluded types or paths is listed;
//   - tmpfs at `/tmp`, `/run` and `/dev/shm` is listed, and the `/var/tmp` tmpfs is not;
//   - `/etc/hosts` is not listed. Its group is a singleton, and F-STATFS said `IsDir=false`, so the group is dropped. F-STATFS's count for `/etc/hosts` is 1;
//   - the root device is listed once, at `/` (root `/` is preferred over `/mnt/bind`), and F-STATFS's count for `/mnt/bind` is 0.
//   - Subtest 2: `/etc/hosts` is listed with the row's numbers. So the drop in subtest 1 came from `IsDir`, not from the path.
//   - Subtest 3: `/srv/appdata` is listed with its F-STATFS numbers and `/etc/hosts` is not. F-STATFS was called for `/etc/hosts` and then for `/srv/appdata`, once each. A file bind never displaces a directory bind of the same filesystem.
//   - Subtest 4: the overlay at `/` is listed.
//   - Every subtest: F-FS recorded no access to `etc/hosts`, `mnt/bind`, `srv/appdata` or any other mount point.
func TestTCHP80_TheExclusionListAndBindMountDedupe(t *testing.T) {
	t.Skip("TC-HP-80: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-81: A filesystem without inode counts reports them as not reported. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup: F-STATFS answers `Files = 0` for one captured mount (as btrfs/vfat do).
// - Steps: filesystems handler.
// - Expected: that item's `inodesTotal`, `inodesUsed`, `inodesFree` null, each in `unavailable` with `not reported: the filesystem has no inode counts`; sizes populated.
func TestTCHP81_AFilesystemWithoutInodeCountsReportsThem(t *testing.T) {
	t.Skip("TC-HP-81: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-82: The read-only flag comes from PID 1's mountinfo. Owner WI 4.2. Requirements: FR-7.
//
// - Setup: UBU F-FS. `proc/1/mountinfo` is the capture's plus one mount, `/srv/rosuper` (fresh `maj:min`, added with `addMount(…, both)`), whose per-mount options are `rw` and whose super options are `ro`. Then every entry of `proc/self/mountinfo` gets per-mount options `ro`, as under `ProtectSystem=strict`, with its mount ID, parent ID and `maj:min` unchanged.
// - Steps: filesystems handler; read F-FS's recorder.
// - Expected:
//   - F-FS recorded `Open`s of both files: `proc/1/mountinfo` (the mount list and the `readOnly` source) and `proc/self/mountinfo` (visibility only, D-9), the latter exactly once per handler call (WI 4.2 step 1).
//   - `readOnly` is false for `/` and true for `/srv/rosuper`. So `readOnly` comes from PID 1's view, and the self file's `ro` options change nothing.
//   - Every item has numbers: the self file decides only visibility, and every mount is visible in it.
func TestTCHP82_TheReadOnlyFlagComesFromPID(t *testing.T) {
	t.Skip("TC-HP-82: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-83: Filesystems are ordered by mount point and named by its escaped path. Owner WI 4.2. Requirements: FR-7, FR-14.
//
// - Setup: host stack (UBU).
// - Steps: filesystems handler.
// - Expected: items sorted by `mountPoint` byte order; each `name` = `url.PathEscape(mountPoint)` (`/` → `%2F`).
func TestTCHP83_FilesystemsAreOrderedByMountPointAnd(t *testing.T) {
	t.Skip("TC-HP-83: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-84: Paging round trip and limits. Owner WI 4.2. Requirements: FR-14.
//
// - Setup: host stack (UBU) with ≥ 3 filesystems.
// - Steps: page with `limit 2` following `nextCursor` until absent; then no `limit`.
// - Expected: concatenated pages equal the unpaged list exactly once each, in order; the last page has no `nextCursor`; no-limit returns up to 100; cursor format matches nginx's (unpadded base64url of the last name).
func TestTCHP84_PagingRoundTripAndLimits(t *testing.T) {
	t.Skip("TC-HP-84: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-85: Block devices on both captures agree with lsblk. Owner WI 4.2. Requirements: FR-8, NFR-3, FR-15.
//
// - Setup: host stack per capture, ready snapshot; `tools/lsblk` (UBU), and for ALP the capture's own `sys/block` (no lsblk).
// - Steps: disks handler.
// - Expected:
//   - one item per `sys/block` entry except `ram*`, `zram*`, and `loop*` without `loop/backing_file`; a loop with a backing file is listed;
//   - UBU: names equal lsblk `type: disk` (plus listed loops); `sizeBytes`, `rotational`, `removable` equal lsblk; `model` = trimmed `device/model`;
//   - each partition from `sys/block/<d>/<p>/partition`, with `sizeBytes` = size × 512 and `mountPoints` equal lsblk's `mountpoints` for it;
//   - rates equal TC-HP-62's for the same name and `maj:min`; `assertExplicit` passes.
func TestTCHP85_BlockDevicesOnBothCapturesAgreeWith(t *testing.T) {
	t.Skip("TC-HP-85: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-86: Disk rates report the sampler state. Owner WI 4.2. Requirements: FR-8, FR-11, FR-12.
//
// - Setup: host stack (UBU) warming up.
// - Steps: disks handler.
// - Expected: `readBytesPerSecond`, `writeBytesPerSecond`, `readIops`, `writeIops` null with `warming up`; model, size, flags populated; each item's `sampler.state` `warming-up`.
func TestTCHP86_DiskRatesReportTheSamplerState(t *testing.T) {
	t.Skip("TC-HP-86: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-E20: Block devices are enumerated through symlink entries. Owner WI 4.2. Requirements: FR-8, CON-5.
//
// - Setup: F-FS(UBU) with `sys/block/*` as `ModeSymlink` entries (the default shaping).
// - Steps: disks handler.
// - Expected: the same disk set as lsblk (TC-HP-85); zero disks is a failure.
func TestTCHPE20_BlockDevicesAreEnumeratedThroughSymlinkEntries(t *testing.T) {
	t.Skip("TC-HP-E20: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-E21: A disk without a model reports it as not reported. Owner WI 4.2. Requirements: FR-8, FR-12.
//
// - Setup: UBU F-FS with `remove("sys/block/<first disk>/device/model")` (as virtio disks have none).
// - Steps: disks handler.
// - Expected: that disk's `model` null with a `not reported:` reason; other fields populated.
func TestTCHPE21_ADiskWithoutAModelReportsIt(t *testing.T) {
	t.Skip("TC-HP-E21: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-E22: A mount point with a space round-trips. Owner WI 4.2. Requirements: FR-7, FR-15.
//
// - Setup: an extra ext4 mount whose mountinfo mount point is `/mnt/my\040disk`, with a fresh `maj:min` (`8:65`), added with `addMount(…, both)`. F-STATFS answers for `/mnt/my disk` with a row distinct from the root's.
// - Steps: filesystems handler.
// - Expected: the item has `mountPoint "/mnt/my disk"`, `name "%2Fmnt%2Fmy%20disk"` and the row's numbers; F-STATFS was called once, with the decoded path `/mnt/my disk`.
func TestTCHPE22_AMountPointWithASpaceRound(t *testing.T) {
	t.Skip("TC-HP-E22: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-ERR21: Without PID 1's mountinfo, read-only flags are unavailable, never wrong. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup: UBU with `deny("proc/1/mountinfo")` and `proc/self/mountinfo` marking every mount `ro`.
// - Steps: filesystems handler.
// - Expected: items come from `self/mountinfo`; every `readOnly` null with `permission denied: /proc/1/mountinfo (read-only flags in nexusd's own namespace would be wrong)`; no item has `readOnly true`; sizes populated.
func TestTCHPERR21_WithoutPID1SMountinfoReadOnly(t *testing.T) {
	t.Skip("TC-HP-ERR21: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-ERR22: One hung mount does not hold up the list. Owner WI 4.2. Requirements: FR-7, NFR-1.
//
// - Setup: UBU with SP-HUNG-NFS: `/mnt/nfs` (nfs4, fresh `maj:min` `0:250`) is added with `addMount(…, both)`, so it is visible and the seam is reached. F-STATFS `hang` covers the whole seam call, including the directory check (Finding 2). Timeout 250 ms.
// - Steps: filesystems handler; measure; call again at once (the call in flight is then `sourceTimeout` old: the stuck branch, A-42).
// - Expected: first returns within 450 ms (`sourceTimeout` + 200 ms); `/mnt/nfs` listed with size, used, available, usedPercent and inodes null with `timed out after 250ms: statfs /mnt/nfs`, never `not visible:` (which would mean the fixture, not the timeout, decided); all other mounts populated; the second call answers within 50 ms with `still pending: statfs /mnt/nfs`; F-FS recorded no access of any kind to `mnt/nfs` or a path under it, so the directory check did not run outside the bounded call; F-STATFS's count for `/mnt/nfs` is 1.
func TestTCHPERR22_OneHungMountDoesNotHoldUp(t *testing.T) {
	t.Skip("TC-HP-ERR22: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-E31: A mount point hidden in nexusd's mount namespace reports not visible, never numbers. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup:
//   - UBU F-FS. Mounts are added with `addMount`, and the root forms are set with `selfRoot` and `stackRoot` (test-strategy.md). Each writes mount IDs and parent IDs that fit its view, because D-9's reachability rule follows parent IDs. F-STATFS answers every path below with a distinct row, the hidden ones included, so a call that should not have been made would also leak numbers.
//   - Base (a): any `/tmp` line is removed from both files, so `/tmp` lives on `/`.
//   - Base (b): a host tmpfs at `/tmp` (`0:45`) is added with `both`.
//   - The hidden mount is `/tmp/hidden`: ext4, `8:99`, root `/`.
//   - A path-prefix control is in every variant: `/tmpdata` (ext4, `8:97`, `both`).
//
// - Steps: filesystems handler with `limit 1000`, once per variant:
//  1. Shape (a), PrivateTmp over a root-filesystem `/tmp` (D-9, Finding 14): `/tmp/hidden` with `underAncestor("/tmp")` on base (a). Self has `/`, the private bind at `/tmp` (root `/tmp/systemd-private-…/tmp`, the `maj:min` of `/`, parent `/`) and `/tmp/hidden` with parent `/`, the only entry at that path. Run it four times: the bind written before and after `/tmp/hidden`, each with mount IDs ascending and descending (`idOrder`).
//  2. The second line of defence: `/mnt/gone` (ext4, `8:96`) with `both`; F-STATFS returns `*fs.PathError{Err: ENOENT}` for it.
//  3. Over-mounted at its own path: `/srv/over` (ext4, `8:98`) with `overMounted("0:77")`. The over-mount is written first in self.
//  4. Shape (b), PrivateTmp over a host tmpfs `/tmp`: `/tmp/hidden` with `underAncestor("/tmp")` on base (b). Self has `/`, the copied tmpfs `/tmp`, the private bind stacked on it (`0:45`, parent the copied `/tmp`) and `/tmp/hidden`, whose parent is the copied `/tmp`. Run it in the same four orders as step 1.
//  5. Ambiguous: `/srv/twin` (ext4, `8:95`) with `ambiguous("0:78")`. Self has two entries at `/srv/twin`, both children of `/` and neither stacked on the other, one with `8:95` and one with `0:78`.
//  6. The fallback (`deny("proc/1/mountinfo")`, so the self entries are the candidates):
//     - 6a: `/tmp/hidden` with `both`, and F-STATFS ENOENT for it;
//     - 6b: `/tmp/hidden` with `underAncestor("/tmp")` on base (a).
//  7. Positive controls:
//     - 7a: `/tmp/hidden` with `both` on each base;
//     - 7b: a root stack (`stackRoot()`): a second `/` entry with the root's `maj:min`, stacked on the root entry. `/srv/upper` (ext4, `8:94`) is a child of the upper `/` entry, and `/srv/lower` (ext4, `8:93`) is a child of the covered lower one;
//     - 7c: the self root entry in proc(5)'s namespace-root form, with its own mount ID as its parent ID (`selfRoot(ownParent)`). Every other variant uses the unlisted-parent form (`selfRoot(unlisted)`), as a captured self file has it.
//
// - Expected:
//   - Steps 1, 3, 4, 5 and 6b: the hidden item (`/tmp/hidden`, `/srv/over` or `/srv/twin`) is listed with `fsType ext4`. In steps 1, 3, 4 and 5 its `readOnly` comes from PID 1's line.
//   - Its `sizeBytes`, `usedBytes`, `availableBytes`, `usedPercent`, `inodesTotal`, `inodesFree` and `inodesUsed` are null, with `not visible: <mount> is hidden in nexusd's mount namespace`.
//   - F-STATFS's count for it is 0 (D-9: no syscall on a hidden mount), so none of its configured numbers appears.
//   - Every order of steps 1 and 4 gives the same result.
//   - Step 2: `/mnt/gone` has null usage with `not visible: /mnt/gone is hidden in nexusd's mount namespace`, and F-STATFS's count for it is 1: the seam was asked and answered ENOENT.
//   - Step 4: `/tmp` itself (the private bind on the host's tmpfs, with the same `maj:min`, `0:45`) is listed with its F-STATFS numbers, count 1. This pins A-44: a stacked entry needs its parent only walked.
//   - Step 6a: `/tmp/hidden` has null usage with `not present: /tmp/hidden`, and `readOnly` is null with TC-HP-ERR21's reason. Step 6b: `not visible:`, count 0, so the rule also applies under the fallback.
//   - Step 7a: `/tmp/hidden` is listed with its F-STATFS numbers, count 1, on both bases. So the hidden results above come from the self view, not from the path, the `maj:min` or the fstype.
//   - Step 7b: `/` and `/srv/upper` are listed with numbers. `/srv/lower` is `not visible:` with count 0, because a child of a covered entry is not walked.
//   - Step 7c: `/` is listed with numbers.
//   - Every variant:
//   - `/tmpdata` and `/` keep their numbers. `/tmp` hides `/tmp/hidden` but is not a path-prefix ancestor of `/tmpdata`;
//   - `assertExplicit` passes;
//   - F-FS recorded no access to `tmp/hidden`, `srv/over`, `srv/twin`, `srv/upper`, `srv/lower` or `mnt/gone`.
func TestTCHPE31_AMountPointHiddenInNexusdsMountNamespace(t *testing.T) {
	t.Skip("TC-HP-E31: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-HP-E33: The subvolumes of one btrfs filesystem are listed once, with numbers, never not visible. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup:
//   - Variant (a): UBU F-FS with the root line replaced in both files by a btrfs line: `maj:min` `0:31`, root `/@`, source `/dev/sda2`, super options `rw,subvol=/@`. `/home` is added with `addMount(…, both)`: btrfs, `sameDevAs("/")` (`0:31`), root `/@home`, super options `rw,subvol=/@home`. F-STATFS answers `/` and `/home` with distinct rows.
//   - Variant (b): the capture's ext4 root is unchanged. `/home` is added with `both`: btrfs, a fresh `0:31`, root `/@home`.
//   - `Usage` has no device field (A-38), so `tools/stat-mounts.txt`'s `%d` cannot affect either variant.
//
// - Steps: filesystems handler with `limit 1000`, per variant.
// - Expected:
//   - Variant (a): one item for `0:31`, at `/`, with `fsType btrfs` and `/`'s F-STATFS numbers. There is no `/home` item (one filesystem is listed once; FR-7, A-21) and no `not visible:` reason anywhere. F-STATFS's count is 1 for `/` and 0 for `/home`: one seam call per filesystem.
//   - Variant (b): `/home` is listed with `fsType btrfs` and its F-STATFS numbers, count 1.
func TestTCHPE33_TheSubvolumesOfOneBtrfsFilesystem(t *testing.T) {
	t.Skip("TC-HP-E33: SKIP-only stub; un-skip when WI 4.2 lands")
}
