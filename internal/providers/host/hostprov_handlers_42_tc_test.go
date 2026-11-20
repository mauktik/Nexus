// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/system-provider/test-cases.md TC-HP-79, TC-HP-80, TC-HP-81, TC-HP-82, TC-HP-83, TC-HP-84, TC-HP-85, TC-HP-86, TC-HP-E20, TC-HP-E21, TC-HP-E22, TC-HP-ERR21, TC-HP-ERR22, TC-HP-E31, TC-HP-E33.
// Owner WI: 4.2. Split from hostprov_handlers_tc_test.go by the orchestrator so
// that Phase 4 work items can run in parallel. The helpers they share are in
// filesystems_test.go and disks_test.go.

package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-HP-79: Filesystems on both captures agree with statfs and df. Owner WI 4.2. Requirements: FR-7, NFR-3, FR-15.
//
// - Setup: host stack per capture, F-STATFS from `tools/statfs.txt`. F-FS serves the capture's own `proc/1/mountinfo` and `proc/self/mountinfo`, with no `addMount`. When `PROVENANCE` says `under-unit: no`, both files list the same mounts, and every `tools/df` target is expected with numbers. When it says `yes`, targets at or under `/tmp` and `/var/tmp` are left out of this comparison; TC-HP-E31 and TC-HP-117 cover them.
// - Steps: filesystems handler with `limit 1000`.
// - Expected:
//   - every item's mount point is a `tools/df` target, and `/` is present;
//   - `sizeBytes` = `%b × %S`, `usedBytes` = `(%b − %f) × %S`, `availableBytes` = `%a × %S` from `statfs.txt` exactly; `sizeBytes` equals df's size exactly; `usedBytes` within one block of df's used; `usedPercent` = used/(used+available) × 100 and within 1 of df's `Use%`;
//   - inodes `%c`, `%d`, `%c − %d`; `assertExplicit` passes.
func TestTCHP79_FilesystemsOnBothCapturesAgreeWithStatfs(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s, st := newFSStack(t, capture)
			items := s.fsAll()
			df := dfTable(t, capture)
			host, err := hostparse.Mountinfo(s.ffs.rawRead(pathHostMountinfo))
			if err != nil {
				t.Fatal(err)
			}
			hostAt := map[string]hostparse.Mount{}
			for _, m := range host {
				hostAt[m.MountPoint] = m
			}
			underUnit := strings.Contains(string(capFile(t, capture, "", "PROVENANCE")), "under-unit: yes")
			privateTmp := func(mp string) bool {
				return underUnit && (mp == "/tmp" || mp == "/var/tmp" || strings.HasPrefix(mp, "/tmp/") ||
					strings.HasPrefix(mp, "/var/tmp/"))
			}
			fsAt(t, items, "/")
			listed := map[devOf]bool{}
			for _, f := range items {
				listed[mountDev(hostAt[f.MountPoint])] = true
				if privateTmp(f.MountPoint) {
					continue
				}
				// GNU df dedupes a filesystem by st_dev and may print it at a
				// file bind (UBU: /etc/hosts), which WI 4.2 never lists; the
				// item is then compared with the df row of its maj:min.
				target, row, ok := dfRowOf(df, hostAt, f.MountPoint)
				if !ok {
					t.Errorf("%s: neither a tools/df target nor of a df target's maj:min", f.MountPoint)
					continue
				}
				u := st.row(f.MountPoint)
				wantUsage(t, f, u)
				if f.SizeBytes == nil || f.UsedBytes == nil || f.UsedPercent == nil {
					continue
				}
				if *f.SizeBytes != row.size {
					t.Errorf("%s: sizeBytes %d, df %s size %d", f.MountPoint, *f.SizeBytes, target, row.size)
				}
				if d := absDiff(*f.UsedBytes, row.used); d > dfDriftBlocks*u.Frsize {
					t.Errorf("%s: usedBytes %d is %d bytes from df %s used %d", f.MountPoint, *f.UsedBytes, d, target, row.used)
				}
				if math.Abs(*f.UsedPercent-row.pct) > 1 {
					t.Errorf("%s: usedPercent %.2f, df %s Use%% %.0f", f.MountPoint, *f.UsedPercent, target, row.pct)
				}
				if f.ReadOnly == nil {
					t.Errorf("%s: readOnly null with %q", f.MountPoint, f.Unavailable["readOnly"])
				}
			}
			// Every df target is accounted for: listed, excluded by A-21, or
			// the same filesystem (maj:min) as a listed item.
			for target := range df {
				if _, ok := fsFind(items, target); ok || privateTmp(target) {
					continue
				}
				m, ok := hostAt[target]
				if !ok || !(excludedMount(m) || listed[mountDev(m)]) {
					t.Errorf("df target %s is not listed, not excluded and not the filesystem of a listed item", target)
				}
			}
		})
	}
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
	setup := func(t *testing.T) (*hostStack, *fakeStatfs) {
		t.Helper()
		s, st := newFSStack(t, capUBU)
		// The capture's own /etc/hosts (Docker's bind from 253:17) is
		// removed, so /etc/hosts is the 8:33 file bind alone.
		removeMounts(t, s.ffs, "/etc/hosts")
		for i, typ := range a21Types {
			mp := "/mnt/x-" + typ
			s.ffs.addMount(t, fmt.Sprintf("0 0 0:%d / %s rw,relatime - %s %s rw", 100+i, mp, typ, typ), viewBoth)
			st.set(mp, fsRow(uint64(i+1)))
		}
		for i, mp := range containerOverlays {
			s.ffs.addMount(t, fmt.Sprintf("0 0 0:%d / %s rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w",
				140+i, mp), viewBoth)
			st.set(mp, fsRow(uint64(40+i)))
		}
		s.ffs.addMount(t, "0 0 7:1 / /snap/core/1 ro,nodev,relatime - squashfs /dev/loop1 ro", viewBoth)
		st.set("/snap/core/1", fsRow(50))
		s.ffs.addMount(t, "0 0 0:150 / /var/tmp rw,nosuid,nodev - tmpfs tmpfs rw", viewBoth)
		st.set("/var/tmp", fsRow(51))
		s.ffs.addMount(t, "0 0 0:0 /home /mnt/bind rw,relatime - overlay overlay rw", viewBoth, sameDevAs("/"))
		st.set("/mnt/bind", fsRow(52))
		s.ffs.addMount(t, "0 0 8:33 /var/lib/docker/containers/c1/hosts /etc/hosts rw,relatime - ext4 /dev/sdc1 rw", viewBoth)
		st.set("/etc/hosts", Usage{}) // a regular file: IsDir false
		return s, st
	}
	excluded := slices.Concat(containerOverlays, []string{"/snap/core/1", "/var/tmp"})
	for _, typ := range a21Types {
		excluded = append(excluded, "/mnt/x-"+typ)
	}
	every := func(t *testing.T, s *hostStack, st *fakeStatfs, items []spec.FilesystemProperties) {
		t.Helper()
		for _, mp := range excluded {
			if _, ok := fsFind(items, mp); ok || st.count(mp) != 0 {
				t.Errorf("excluded %s: listed %v, F-STATFS count %d", mp, ok, st.count(mp))
			}
		}
		for _, f := range items {
			if slices.Contains(a21Types, f.FSType) {
				t.Errorf("%s of excluded type %s is listed", f.MountPoint, f.FSType)
			}
		}
		wantOnlyMountinfo(t, s.ffs)
	}

	t.Run("as set up", func(t *testing.T) {
		s, st := setup(t)
		items := s.fsAll()
		every(t, s, st, items)
		for _, mp := range []string{"/tmp", "/run", "/dev/shm"} {
			if f := fsAt(t, items, mp); f.FSType != "tmpfs" {
				t.Errorf("%s fsType %q, want tmpfs", mp, f.FSType)
			}
		}
		if _, ok := fsFind(items, "/etc/hosts"); ok || st.count("/etc/hosts") != 1 {
			t.Errorf("/etc/hosts listed %v, F-STATFS count %d; want dropped after one call", ok, st.count("/etc/hosts"))
		}
		wantUsage(t, fsAt(t, items, "/"), st.row("/"))
		if _, ok := fsFind(items, "/mnt/bind"); ok || st.count("/mnt/bind") != 0 {
			t.Errorf("/mnt/bind listed %v, F-STATFS count %d; want the root device once, at /", ok, st.count("/mnt/bind"))
		}
	})
	t.Run("hosts is a directory", func(t *testing.T) {
		s, st := setup(t)
		st.set("/etc/hosts", fsRow(7))
		items := s.fsAll()
		every(t, s, st, items)
		wantUsage(t, fsAt(t, items, "/etc/hosts"), fsRow(7))
	})
	t.Run("a directory bind of the file bind's filesystem", func(t *testing.T) {
		s, st := setup(t)
		s.ffs.addMount(t, "0 0 0:0 /appdata /srv/appdata rw,relatime - ext4 /dev/sdc1 rw", viewBoth, sameDevAs("/etc/hosts"))
		st.set("/srv/appdata", fsRow(8))
		items := s.fsAll()
		every(t, s, st, items)
		wantUsage(t, fsAt(t, items, "/srv/appdata"), fsRow(8))
		if _, ok := fsFind(items, "/etc/hosts"); ok {
			t.Error("/etc/hosts is listed; a file bind never displaces a directory bind")
		}
		var order []string
		for _, c := range st.recordedCalls() {
			if c == "/etc/hosts" || c == "/srv/appdata" {
				order = append(order, c)
			}
		}
		if !slices.Equal(order, []string{"/etc/hosts", "/srv/appdata"}) {
			t.Errorf("F-STATFS calls %v, want /etc/hosts then /srv/appdata, once each", order)
		}
	})
	t.Run("container root", func(t *testing.T) {
		s, st := setup(t)
		for _, name := range []string{pathHostMountinfo, pathSelfMountinfo} {
			rewriteMI(t, s.ffs, name, func(e *miEntry) bool {
				if e.mountPoint() == "/" {
					setFSType(t, e, "overlay")
				}
				return true
			})
		}
		items := s.fsAll()
		every(t, s, st, items)
		if f := fsAt(t, items, "/"); f.FSType != "overlay" {
			t.Errorf("/ fsType %q, want overlay", f.FSType)
		}
	})
}

// TC-HP-81: A filesystem without inode counts reports them as not reported. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup: F-STATFS answers `Files = 0` for one captured mount (as btrfs/vfat do).
// - Steps: filesystems handler.
// - Expected: that item's `inodesTotal`, `inodesUsed`, `inodesFree` null, each in `unavailable` with `not reported: the filesystem has no inode counts`; sizes populated.
func TestTCHP81_AFilesystemWithoutInodeCountsReportsThem(t *testing.T) {
	s, st := newFSStack(t, capUBU)
	u := st.row("/run")
	u.Files, u.Ffree = 0, 0
	st.set("/run", u)
	f := fsAt(t, s.fsAll(), "/run")
	wantUsage(t, f, u)
	if f.InodesTotal != nil || f.InodesUsed != nil || f.InodesFree != nil {
		t.Errorf("inodes %s/%s/%s, want null", show(f.InodesTotal), show(f.InodesUsed), show(f.InodesFree))
	}
	for _, k := range []string{"inodesTotal", "inodesUsed", "inodesFree"} {
		if got := f.Unavailable[k]; got != "not reported: the filesystem has no inode counts" {
			t.Errorf("%s reason %q", k, got)
		}
	}
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
	s, st := newFSStack(t, capUBU)
	s.ffs.addMount(t, "0 0 0:140 / /srv/rosuper rw,relatime - ext4 /dev/sdz1 ro", viewBoth)
	st.set("/srv/rosuper", fsRow(3))
	// ProtectSystem=strict: every self entry is ro, IDs and maj:min kept.
	rewriteMI(t, s.ffs, pathSelfMountinfo, func(e *miEntry) bool {
		e.rest[0] = "ro"
		return true
	})
	s.ffs.resetRecorder()
	items := s.fsAll()
	if n := s.ffs.count("open", pathHostMountinfo); n != 1 {
		t.Errorf("proc/1/mountinfo opened %d times, want 1", n)
	}
	if n := s.ffs.count("open", pathSelfMountinfo); n != 1 {
		t.Errorf("proc/self/mountinfo opened %d times per call, want exactly 1", n)
	}
	if f := fsAt(t, items, "/"); f.ReadOnly == nil || *f.ReadOnly {
		t.Errorf("/ readOnly %s, want false (PID 1's view)", show(f.ReadOnly))
	}
	if f := fsAt(t, items, "/srv/rosuper"); f.ReadOnly == nil || !*f.ReadOnly {
		t.Errorf("/srv/rosuper readOnly %s, want true (super options ro)", show(f.ReadOnly))
	}
	for _, f := range items {
		wantUsage(t, f, st.row(f.MountPoint))
	}
}

// TC-HP-83: Filesystems are ordered by mount point and named by its escaped path. Owner WI 4.2. Requirements: FR-7, FR-14.
//
// - Setup: host stack (UBU).
// - Steps: filesystems handler.
// - Expected: items sorted by `mountPoint` byte order; each `name` = `url.PathEscape(mountPoint)` (`/` → `%2F`).
func TestTCHP83_FilesystemsAreOrderedByMountPointAnd(t *testing.T) {
	s, st := newFSStack(t, capUBU)
	// Byte order of mount points and of their escaped names differ here:
	// '"' (0x22) sorts before '$' (0x24), but "%22" sorts after "$".
	s.ffs.addMount(t, `0 0 0:141 / /mnt/a" rw - ext4 /dev/sde1 rw`, viewBoth)
	s.ffs.addMount(t, `0 0 0:142 / /mnt/a$ rw - ext4 /dev/sdf1 rw`, viewBoth)
	st.set(`/mnt/a"`, fsRow(1))
	st.set(`/mnt/a$`, fsRow(2))
	l := s.fsPage(spec.ListInput{Limit: 1000})
	var mps []string
	for _, r := range l.Items {
		mps = append(mps, r.Properties.MountPoint)
		if r.Name != url.PathEscape(r.Properties.MountPoint) {
			t.Errorf("name %q for %q", r.Name, r.Properties.MountPoint)
		}
	}
	if !slices.IsSorted(mps) || len(mps) == 0 || mps[0] != "/" || l.Items[0].Name != "%2F" {
		t.Errorf("mount points %q are not in byte order from / (%%2F)", mps)
	}
	if slices.Index(mps, `/mnt/a"`) > slices.Index(mps, `/mnt/a$`) {
		t.Errorf("%q: sorted by escaped name, not by mount point", mps)
	}
}

// TC-HP-84: Paging round trip and limits. Owner WI 4.2. Requirements: FR-14.
//
// - Setup: host stack (UBU) with ≥ 3 filesystems.
// - Steps: page with `limit 2` following `nextCursor` until absent; then no `limit`.
// - Expected: concatenated pages equal the unpaged list exactly once each, in order; the last page has no `nextCursor`; no-limit returns up to 100; cursor format matches nginx's (unpadded base64url of the last name).
func TestTCHP84_PagingRoundTripAndLimits(t *testing.T) {
	s, st := newFSStack(t, capUBU)
	s.ffs.addMount(t, `0 0 0:141 / /mnt/a" rw - ext4 /dev/sde1 rw`, viewBoth)
	s.ffs.addMount(t, `0 0 0:142 / /mnt/a$ rw - ext4 /dev/sdf1 rw`, viewBoth)
	for i := range 100 {
		mp := fmt.Sprintf("/srv/m%03d", i)
		s.ffs.addMount(t, fmt.Sprintf("0 0 0:%d / %s rw - ext4 /dev/sdg%d rw", 300+i, mp, i), viewBoth)
		st.set(mp, fsRow(uint64(i+1)))
	}
	full := s.fsPage(spec.ListInput{Limit: 1000}).Items
	if len(full) <= spec.DefaultLimit {
		t.Fatalf("%d filesystems; the test needs more than %d", len(full), spec.DefaultLimit)
	}

	var got []spec.Filesystem
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > len(full) {
			t.Fatal("paging does not end")
		}
		l := s.fsPage(spec.ListInput{Limit: 2, Cursor: cursor})
		got = append(got, l.Items...)
		if l.NextCursor == "" {
			break
		}
		if want := base64.RawURLEncoding.EncodeToString([]byte(l.Items[len(l.Items)-1].Name)); l.NextCursor != want {
			t.Errorf("nextCursor %q, want %q (unpadded base64url of the last name, as nginx's)", l.NextCursor, want)
		}
		cursor = l.NextCursor
	}
	if !reflect.DeepEqual(got, full) {
		t.Errorf("limit-2 pages give %d items, want the %d of the unpaged list, once each, in order", len(got), len(full))
	}

	l := s.fsPage(spec.ListInput{})
	if len(l.Items) != spec.DefaultLimit || !reflect.DeepEqual(l.Items, full[:spec.DefaultLimit]) || l.NextCursor == "" {
		t.Errorf("no limit: %d items, nextCursor %q; want the first %d and a cursor", len(l.Items), l.NextCursor,
			spec.DefaultLimit)
	}

	for _, c := range []string{base64.RawURLEncoding.EncodeToString([]byte("vda")), "%%%"} {
		_, err := s.p.filesystems(context.Background(), spec.ListInput{Cursor: c})
		var p *problem.Error
		if !errors.As(err, &p) || p.Slug() != problem.ValidationFailed {
			t.Errorf("foreign cursor %q: %v, want validation-failed", c, err)
		}
	}
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
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newHostStack(t, capture)
			s.ffs.replace("sys/block/loop0/loop/backing_file", []byte("/var/lib/snapd/snaps/core_1.snap\n"))
			s.ffs.replace("sys/block/vdb/device/model", []byte("  QEMU HARDDISK  \n"))
			_, snap := s.ready()
			items := s.diskList()
			lsblk := lsblkDisks(t, capture)

			want := []string{"loop0"}
			for _, d := range lsblk {
				want = append(want, d.Name)
			}
			slices.Sort(want)
			if got := diskNames(items); !slices.Equal(got, want) {
				t.Fatalf("disks %v, want %v (lsblk's disks and the backed loop0)", got, want)
			}
			for _, d := range lsblk {
				p := diskAt(t, items, d.Name)
				if p.SizeBytes == nil || *p.SizeBytes != d.Size || p.Rotational == nil || *p.Rotational != d.Rota ||
					p.Removable == nil || *p.Removable != d.RM {
					t.Errorf("%s: size %s rota %s rm %s; lsblk %d %v %v", d.Name, show(p.SizeBytes), show(p.Rotational),
						show(p.Removable), d.Size, d.Rota, d.RM)
				}
				if d.Name == "vdb" {
					if !eqStr(p.Model, "QEMU HARDDISK") {
						t.Errorf("vdb model %s, want the trimmed device/model", show(p.Model))
					}
				} else if p.Model != nil || !strings.HasPrefix(p.Unavailable["model"], prefixNotReported) {
					t.Errorf("%s model %s with %q, want not reported", d.Name, show(p.Model), p.Unavailable["model"])
				}
				var names []string
				for _, c := range d.Children {
					names = append(names, c.Name)
				}
				if got := partNames(p.Partitions); !slices.Equal(got, names) {
					t.Errorf("%s partitions %v, lsblk %v", d.Name, got, names)
					continue
				}
				for i, c := range d.Children {
					part := p.Partitions[i]
					sectors := strings.TrimSpace(string(capFile(t, capture, "fs", "sys/block/"+d.Name+"/"+c.Name+"/size")))
					if part.SizeBytes == nil || *part.SizeBytes != c.Size || strconv.FormatUint(c.Size/512, 10) != sectors {
						t.Errorf("%s: sizeBytes %s, lsblk %d, size file %s sectors", c.Name, show(part.SizeBytes), c.Size, sectors)
					}
					if got, want := []string(part.MountPoints), c.mountPoints(); !slices.Equal(got, want) {
						t.Errorf("%s: mountPoints %q, lsblk %q", c.Name, got, want)
					}
				}
			}
			loop := diskAt(t, items, "loop0")
			if loop.SizeBytes == nil || len(loop.Partitions) != 0 {
				t.Errorf("loop0: size %s, %d partitions", show(loop.SizeBytes), len(loop.Partitions))
			}

			// The rates are TC-HP-62's for the same name and maj:min.
			prev, cur := diskCols(t, capFile(t, capture, "fs", pathDiskstats)), diskCols(t, capFile(t, capture, "t1", pathDiskstats))
			for _, name := range want {
				p, c := prev[name], cur[name]
				d := diskAt(t, items, name)
				if dev := strings.TrimSpace(string(capFile(t, capture, "fs", "sys/block/"+name+"/dev"))); dev !=
					fmt.Sprintf("%d:%d", c[0], c[1]) {
					t.Fatalf("%s: sys/block dev %s, diskstats %d:%d", name, dev, c[0], c[1])
				}
				for _, x := range []struct {
					what string
					got  *float64
					want float64
				}{
					{"readBytesPerSecond", d.ReadBytesPerSecond, float64(c[3]-p[3]) * 512 / 10},
					{"writeBytesPerSecond", d.WriteBytesPerSecond, float64(c[5]-p[5]) * 512 / 10},
					{"readIops", d.ReadIOPS, float64(c[2]-p[2]) / 10},
					{"writeIops", d.WriteIOPS, float64(c[4]-p[4]) / 10},
				} {
					if x.got == nil || !near(*x.got, x.want) {
						t.Errorf("%s %s = %s (%q), want %v", name, x.what, show(x.got), d.Unavailable[x.what], x.want)
					}
				}
				if d.Sampler.State != spec.SamplerReady {
					t.Errorf("%s sampler %q", name, d.Sampler.State)
				}
			}
			if snap.disks == nil {
				t.Fatal("no snapshot disks")
			}
		})
	}
}

// TC-HP-86: Disk rates report the sampler state. Owner WI 4.2. Requirements: FR-8, FR-11, FR-12.
//
// - Setup: host stack (UBU) warming up.
// - Steps: disks handler.
// - Expected: `readBytesPerSecond`, `writeBytesPerSecond`, `readIops`, `writeIops` null with `warming up`; model, size, flags populated; each item's `sampler.state` `warming-up`.
func TestTCHP86_DiskRatesReportTheSamplerState(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("sys/block/vda/device/model", []byte("QEMU HARDDISK\n"))
	s.start()
	s.waitSeq(1)
	items := s.diskList()
	if len(items) == 0 {
		t.Fatal("no disks")
	}
	for _, r := range items {
		d := r.Properties
		for _, k := range []string{"readBytesPerSecond", "writeBytesPerSecond", "readIops", "writeIops"} {
			if got := d.Unavailable[k]; got != "warming up" {
				t.Errorf("%s %s reason %q, want warming up", r.Name, k, got)
			}
		}
		if d.ReadBytesPerSecond != nil || d.WriteBytesPerSecond != nil || d.ReadIOPS != nil || d.WriteIOPS != nil {
			t.Errorf("%s: a rate is set while warming up", r.Name)
		}
		if d.Sampler.State != spec.SamplerWarmingUp {
			t.Errorf("%s sampler.state %q, want warming-up", r.Name, d.Sampler.State)
		}
		if d.SizeBytes == nil || d.Rotational == nil || d.Removable == nil {
			t.Errorf("%s: size or flags missing: %v", r.Name, d.Unavailable)
		}
	}
	if d := diskAt(t, items, "vda"); !eqStr(d.Model, "QEMU HARDDISK") {
		t.Errorf("vda model %s", show(d.Model))
	}
}

// TC-HP-E20: Block devices are enumerated through symlink entries. Owner WI 4.2. Requirements: FR-8, CON-5.
//
// - Setup: F-FS(UBU) with `sys/block/*` as `ModeSymlink` entries (the default shaping).
// - Steps: disks handler.
// - Expected: the same disk set as lsblk (TC-HP-85); zero disks is a failure.
func TestTCHPE20_BlockDevicesAreEnumeratedThroughSymlinkEntries(t *testing.T) {
	s := newHostStack(t, capUBU)
	entries, err := fs.ReadDir(s.ffs, "sys/block")
	if err != nil || len(entries) == 0 {
		t.Fatalf("sys/block: %v", err)
	}
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 || e.IsDir() {
			t.Fatalf("F-FS lists sys/block/%s as %v, want a symlink as sysfs does", e.Name(), e.Type())
		}
	}
	var want []string
	for _, d := range lsblkDisks(t, capUBU) {
		want = append(want, d.Name)
	}
	got := diskNames(s.diskList())
	if len(got) == 0 || !slices.Equal(got, want) {
		t.Errorf("disks %v, want lsblk's %v", got, want)
	}
}

// TC-HP-E21: A disk without a model reports it as not reported. Owner WI 4.2. Requirements: FR-8, FR-12.
//
// - Setup: UBU F-FS with `remove("sys/block/<first disk>/device/model")` (as virtio disks have none).
// - Steps: disks handler.
// - Expected: that disk's `model` null with a `not reported:` reason; other fields populated.
func TestTCHPE21_ADiskWithoutAModelReportsIt(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("sys/block/vdb/device/model", []byte("QEMU HARDDISK\n"))
	s.ffs.remove("sys/block/vda/device/model")
	items := s.diskList()
	vda := diskAt(t, items, "vda")
	if vda.Model != nil || vda.Unavailable["model"] != "not reported: no /sys/block/vda/device/model" {
		t.Errorf("vda model %s with %q, want null and not reported", show(vda.Model), vda.Unavailable["model"])
	}
	if vda.SizeBytes == nil || vda.Rotational == nil || vda.Removable == nil || len(vda.Partitions) != 3 {
		t.Errorf("vda: other fields missing: %v", vda.Unavailable)
	}
	if vdb := diskAt(t, items, "vdb"); !eqStr(vdb.Model, "QEMU HARDDISK") {
		t.Errorf("vdb model %s", show(vdb.Model))
	}
}

// TC-HP-E22: A mount point with a space round-trips. Owner WI 4.2. Requirements: FR-7, FR-15.
//
// - Setup: an extra ext4 mount whose mountinfo mount point is `/mnt/my\040disk`, with a fresh `maj:min` (`8:65`), added with `addMount(…, both)`. F-STATFS answers for `/mnt/my disk` with a row distinct from the root's.
// - Steps: filesystems handler.
// - Expected: the item has `mountPoint "/mnt/my disk"`, `name "%2Fmnt%2Fmy%20disk"` and the row's numbers; F-STATFS was called once, with the decoded path `/mnt/my disk`.
func TestTCHPE22_AMountPointWithASpaceRound(t *testing.T) {
	s, st := newFSStack(t, capUBU)
	s.ffs.addMount(t, `0 0 8:65 / /mnt/my\040disk rw,relatime - ext4 /dev/sdd1 rw`, viewBoth)
	st.set("/mnt/my disk", fsRow(9))
	l := s.fsPage(spec.ListInput{Limit: 1000})
	found := false
	for _, r := range l.Items {
		if r.Properties.MountPoint == "/mnt/my disk" {
			found = true
			if r.Name != "%2Fmnt%2Fmy%20disk" {
				t.Errorf("name %q", r.Name)
			}
			wantUsage(t, r.Properties, fsRow(9))
		}
	}
	if !found {
		t.Fatal("/mnt/my disk is not listed")
	}
	if st.count("/mnt/my disk") != 1 || slices.Contains(st.recordedCalls(), `/mnt/my\040disk`) {
		t.Errorf("F-STATFS calls %q, want one with the decoded path", st.recordedCalls())
	}
}

// TC-HP-ERR21: Without PID 1's mountinfo, read-only flags are unavailable, never wrong. Owner WI 4.2. Requirements: FR-7, FR-12.
//
// - Setup: UBU with `deny("proc/1/mountinfo")` and `proc/self/mountinfo` marking every mount `ro`.
// - Steps: filesystems handler.
// - Expected: items come from `self/mountinfo`; every `readOnly` null with `permission denied: /proc/1/mountinfo (read-only flags in nexusd's own namespace would be wrong)`; no item has `readOnly true`; sizes populated.
func TestTCHPERR21_WithoutPID1SMountinfoReadOnly(t *testing.T) {
	s, _ := newFSStack(t, capUBU)
	s.ffs.deny("proc/1/mountinfo")
	rewriteMI(t, s.ffs, pathSelfMountinfo, func(e *miEntry) bool {
		e.rest[0] = "ro"
		return true
	})
	self := map[string]bool{}
	for _, e := range s.ffs.mountinfo(t, pathSelfMountinfo) {
		self[e.mountPoint()] = true
	}
	items := s.fsAll()
	if len(items) == 0 {
		t.Fatal("no filesystems")
	}
	for _, f := range items {
		if !self[f.MountPoint] {
			t.Errorf("%s is not in proc/self/mountinfo", f.MountPoint)
		}
		if f.ReadOnly != nil || f.Unavailable["readOnly"] !=
			"permission denied: /proc/1/mountinfo (read-only flags in nexusd's own namespace would be wrong)" {
			t.Errorf("%s readOnly %s with %q", f.MountPoint, show(f.ReadOnly), f.Unavailable["readOnly"])
		}
		if f.SizeBytes == nil {
			t.Errorf("%s: sizeBytes null with %q", f.MountPoint, f.Unavailable["sizeBytes"])
		}
	}
}

// TC-HP-ERR22: One hung mount does not hold up the list. Owner WI 4.2. Requirements: FR-7, NFR-1.
//
// - Setup: UBU with SP-HUNG-NFS: `/mnt/nfs` (nfs4, fresh `maj:min` `0:250`) is added with `addMount(…, both)`, so it is visible and the seam is reached. F-STATFS `hang` covers the whole seam call, including the directory check (Finding 2). Timeout 250 ms.
// - Steps: filesystems handler; measure; call again at once (the call in flight is then `sourceTimeout` old: the stuck branch, A-42).
// - Expected: first returns within 450 ms (`sourceTimeout` + 200 ms); `/mnt/nfs` listed with size, used, available, usedPercent and inodes null with `timed out after 250ms: statfs /mnt/nfs`, never `not visible:` (which would mean the fixture, not the timeout, decided); all other mounts populated; the second call answers within 50 ms with `still pending: statfs /mnt/nfs`; F-FS recorded no access of any kind to `mnt/nfs` or a path under it, so the directory check did not run outside the bounded call; F-STATFS's count for `/mnt/nfs` is 1.
func TestTCHPERR22_OneHungMountDoesNotHoldUp(t *testing.T) {
	s, st := newFSStack(t, capUBU)
	s.timeout(250 * time.Millisecond)
	spHungNFS(t, s.ffs, st)
	s.ffs.resetRecorder()

	start := time.Now()
	items := s.fsAll()
	if d := time.Since(start); d > 450*time.Millisecond {
		t.Errorf("first call took %v, want within sourceTimeout + 200ms", d)
	}
	wantNoUsage(t, fsAt(t, items, "/mnt/nfs"), "timed out after 250ms: statfs /mnt/nfs")
	for _, f := range items {
		if f.MountPoint != "/mnt/nfs" {
			wantUsage(t, f, st.row(f.MountPoint))
		}
	}

	start = time.Now()
	items = s.fsAll()
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("second call took %v, want within 50ms", d)
	}
	wantNoUsage(t, fsAt(t, items, "/mnt/nfs"), "still pending: statfs /mnt/nfs")
	wantOnlyMountinfo(t, s.ffs)
	if n := st.count("/mnt/nfs"); n != 1 {
		t.Errorf("F-STATFS count for /mnt/nfs %d, want 1", n)
	}
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
	const hiddenLine = "0 0 8:99 / /tmp/hidden rw,relatime - ext4 /dev/sdy1 rw"
	rows := map[string]uint64{"/tmp/hidden": 99, "/tmpdata": 97, "/srv/over": 98, "/srv/twin": 95, "/srv/upper": 94,
		"/srv/lower": 93, "/mnt/gone": 96}
	// build is a variant: base (a) or (b), the self root form, pre run
	// before the /tmpdata control is added.
	build := func(t *testing.T, base, rootForm string, pre func(s *hostStack)) (*hostStack, *fakeStatfs) {
		t.Helper()
		s, st := newFSStack(t, capUBU)
		removeMounts(t, s.ffs, "/tmp")
		s.ffs.selfRoot(t, rootForm)
		if base == "b" {
			s.ffs.addMount(t, "0 0 0:45 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw,size=1024k", viewBoth)
			st.set("/tmp", fsRow(45))
		}
		if pre != nil {
			pre(s)
		}
		s.ffs.addMount(t, "0 0 8:97 / /tmpdata rw,relatime - ext4 /dev/sdx1 rw", viewBoth)
		for mp, seed := range rows {
			st.set(mp, fsRow(seed))
		}
		return s, st
	}
	every := func(t *testing.T, s *hostStack, st *fakeStatfs, items []spec.FilesystemProperties) {
		t.Helper()
		wantUsage(t, fsAt(t, items, "/tmpdata"), st.row("/tmpdata"))
		wantUsage(t, fsAt(t, items, "/"), st.row("/"))
		wantOnlyMountinfo(t, s.ffs)
	}
	hidden := func(t *testing.T, st *fakeStatfs, items []spec.FilesystemProperties, mp string, roFromHost bool) string {
		t.Helper()
		f := fsAt(t, items, mp)
		if f.FSType != "ext4" {
			t.Errorf("%s fsType %q", mp, f.FSType)
		}
		wantNoUsage(t, f, "not visible: "+mp+" is hidden in nexusd's mount namespace")
		if n := st.count(mp); n != 0 {
			t.Errorf("F-STATFS count for hidden %s is %d, want 0", mp, n)
		}
		if roFromHost && (f.ReadOnly == nil || *f.ReadOnly) {
			t.Errorf("%s readOnly %s, want PID 1's false", mp, show(f.ReadOnly))
		}
		b, _ := json.Marshal(f)
		return string(b)
	}
	orders := []struct {
		name string
		opts []mountOpt
	}{
		{"bind after, ids asc", []mountOpt{orderAfter(), idOrderAsc()}},
		{"bind after, ids desc", []mountOpt{orderAfter(), idOrderDesc()}},
		{"bind before, ids asc", []mountOpt{orderBefore(), idOrderAsc()}},
		{"bind before, ids desc", []mountOpt{orderBefore(), idOrderDesc()}},
	}

	for _, shape := range []string{"a", "b"} {
		step := map[string]string{"a": "1 shape (a)", "b": "4 shape (b)"}[shape]
		t.Run(step, func(t *testing.T) {
			results := map[string]bool{}
			for _, o := range orders {
				t.Run(o.name, func(t *testing.T) {
					s, st := build(t, shape, rootUnlisted, nil)
					s.ffs.addMount(t, hiddenLine, viewUnderAncestor("/tmp"), o.opts...)
					items := s.fsAll()
					results[hidden(t, st, items, "/tmp/hidden", true)] = true
					every(t, s, st, items)
					if shape == "b" {
						wantUsage(t, fsAt(t, items, "/tmp"), fsRow(45))
						if n := st.count("/tmp"); n != 1 {
							t.Errorf("F-STATFS count for /tmp %d, want 1", n)
						}
					}
				})
			}
			if len(results) != 1 {
				t.Errorf("the four orders give %d different results", len(results))
			}
		})
	}
	t.Run("2 seam ENOENT", func(t *testing.T) {
		s, st := build(t, "a", rootUnlisted, nil)
		s.ffs.addMount(t, "0 0 8:96 / /mnt/gone rw,relatime - ext4 /dev/sdw1 rw", viewBoth)
		st.setErr("/mnt/gone", &fs.PathError{Op: "stat", Path: "/mnt/gone", Err: syscall.ENOENT})
		items := s.fsAll()
		wantNoUsage(t, fsAt(t, items, "/mnt/gone"), "not visible: /mnt/gone is hidden in nexusd's mount namespace")
		if n := st.count("/mnt/gone"); n != 1 {
			t.Errorf("F-STATFS count for /mnt/gone %d, want 1", n)
		}
		every(t, s, st, items)
	})
	t.Run("3 over-mounted", func(t *testing.T) {
		s, st := build(t, "a", rootUnlisted, nil)
		s.ffs.addMount(t, "0 0 8:98 / /srv/over rw,relatime - ext4 /dev/sdv1 rw", viewOverMounted("0:77"))
		items := s.fsAll()
		hidden(t, st, items, "/srv/over", true)
		every(t, s, st, items)
	})
	t.Run("5 ambiguous", func(t *testing.T) {
		s, st := build(t, "a", rootUnlisted, nil)
		s.ffs.addMount(t, "0 0 8:95 / /srv/twin rw,relatime - ext4 /dev/sdu1 rw", viewAmbiguous("0:78"))
		items := s.fsAll()
		hidden(t, st, items, "/srv/twin", true)
		every(t, s, st, items)
	})
	t.Run("6a fallback ENOENT", func(t *testing.T) {
		s, st := build(t, "a", rootUnlisted, nil)
		s.ffs.addMount(t, hiddenLine, viewBoth)
		s.ffs.deny("proc/1/mountinfo")
		st.setErr("/tmp/hidden", &fs.PathError{Op: "stat", Path: "/tmp/hidden", Err: syscall.ENOENT})
		items := s.fsAll()
		f := fsAt(t, items, "/tmp/hidden")
		wantNoUsage(t, f, "not present: /tmp/hidden")
		if f.ReadOnly != nil || f.Unavailable["readOnly"] !=
			"permission denied: /proc/1/mountinfo (read-only flags in nexusd's own namespace would be wrong)" {
			t.Errorf("readOnly %s with %q", show(f.ReadOnly), f.Unavailable["readOnly"])
		}
		every(t, s, st, items)
	})
	t.Run("6b fallback shape (a)", func(t *testing.T) {
		s, st := build(t, "a", rootUnlisted, nil)
		s.ffs.addMount(t, hiddenLine, viewUnderAncestor("/tmp"))
		s.ffs.deny("proc/1/mountinfo")
		items := s.fsAll()
		hidden(t, st, items, "/tmp/hidden", false)
		every(t, s, st, items)
	})
	for _, base := range []string{"a", "b"} {
		t.Run("7a both on base "+base, func(t *testing.T) {
			s, st := build(t, base, rootUnlisted, nil)
			s.ffs.addMount(t, hiddenLine, viewBoth)
			items := s.fsAll()
			wantUsage(t, fsAt(t, items, "/tmp/hidden"), fsRow(99))
			if n := st.count("/tmp/hidden"); n != 1 {
				t.Errorf("F-STATFS count for /tmp/hidden %d, want 1", n)
			}
			every(t, s, st, items)
		})
	}
	t.Run("7b root stack", func(t *testing.T) {
		s, st := build(t, "a", rootUnlisted, func(s *hostStack) { s.ffs.stackRoot(t) })
		s.ffs.addMount(t, "0 0 8:94 / /srv/upper rw,relatime - ext4 /dev/sdt1 rw", viewBoth)
		s.ffs.addMount(t, "0 0 8:93 / /srv/lower rw,relatime - ext4 /dev/sds1 rw", viewBoth)
		// /srv/lower is a child of the covered lower root entry.
		self := s.ffs.mountinfo(t, pathSelfMountinfo)
		lower := self[selfRootEntry(t, self)].id
		rewriteMI(t, s.ffs, pathSelfMountinfo, func(e *miEntry) bool {
			if e.mountPoint() == "/srv/lower" {
				e.parent = lower
			}
			return true
		})
		items := s.fsAll()
		wantUsage(t, fsAt(t, items, "/srv/upper"), fsRow(94))
		hidden(t, st, items, "/srv/lower", true)
		every(t, s, st, items)
	})
	t.Run("7c namespace-root form", func(t *testing.T) {
		s, st := build(t, "a", rootOwnParent, nil)
		items := s.fsAll()
		every(t, s, st, items)
	})
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
	btrfsRoot := func(e *miEntry) {
		e.majmin, e.root = "0:31", "/@"
		i := slices.Index(e.rest, "-")
		e.rest = append(e.rest[:i:i], "-", "btrfs", "/dev/sda2", "rw,subvol=/@")
	}
	t.Run("a: root and home subvolumes", func(t *testing.T) {
		s, st := newFSStack(t, capUBU)
		for _, name := range []string{pathHostMountinfo, pathSelfMountinfo} {
			rewriteMI(t, s.ffs, name, func(e *miEntry) bool {
				if e.mountPoint() == "/" {
					btrfsRoot(e)
				}
				return true
			})
		}
		s.ffs.addMount(t, "0 0 0:0 /@home /home rw,relatime - btrfs /dev/sda2 rw,subvol=/@home", viewBoth, sameDevAs("/"))
		st.set("/", fsRow(31))
		st.set("/home", fsRow(32))
		items := s.fsAll()
		f := fsAt(t, items, "/")
		if f.FSType != "btrfs" {
			t.Errorf("/ fsType %q", f.FSType)
		}
		wantUsage(t, f, fsRow(31))
		if _, ok := fsFind(items, "/home"); ok {
			t.Error("/home is listed; one btrfs filesystem is listed once")
		}
		for _, f := range items {
			for k, why := range f.Unavailable {
				if strings.HasPrefix(why, prefixNotVisible) {
					t.Errorf("%s.%s: %q", f.MountPoint, k, why)
				}
			}
		}
		if st.count("/") != 1 || st.count("/home") != 0 {
			t.Errorf("F-STATFS counts / %d, /home %d; want 1 and 0", st.count("/"), st.count("/home"))
		}
	})
	t.Run("b: home only", func(t *testing.T) {
		s, st := newFSStack(t, capUBU)
		s.ffs.addMount(t, "0 0 0:31 /@home /home rw,relatime - btrfs /dev/sda2 rw,subvol=/@home", viewBoth)
		st.set("/home", fsRow(32))
		items := s.fsAll()
		f := fsAt(t, items, "/home")
		if f.FSType != "btrfs" {
			t.Errorf("/home fsType %q", f.FSType)
		}
		wantUsage(t, f, fsRow(32))
		if n := st.count("/home"); n != 1 {
			t.Errorf("F-STATFS count for /home %d, want 1", n)
		}
	})
}
