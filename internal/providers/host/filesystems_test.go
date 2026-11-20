// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// newFSStack is newHostStack with its F-STATFS kept, so a test can
// configure and count the seam.
func newFSStack(t *testing.T, capture string) (*hostStack, *fakeStatfs) {
	t.Helper()
	s := newHostStack(t, capture)
	st := newFakeStatfs(t, capture)
	s.p.src.statfsSeam = st.Statfs
	return s, st
}

// decodeList runs assertExplicit on a list output, decodes it from JSON
// (the wire shape) and checks each item's resource fields: type, host
// "tc", the ID and, when name is set, the name it gives.
func decodeList[P any](t *testing.T, typ string, out any, name func(P) string) apitypes.List[apitypes.Resource[P]] {
	t.Helper()
	assertExplicit(t, out)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var l apitypes.List[apitypes.Resource[P]]
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatalf("decode %s: %v\n%s", typ, err, raw)
	}
	for _, r := range l.Items {
		if r.Type != typ || r.Host != "tc" || (name != nil && r.Name != name(r.Properties)) ||
			r.ID != apitypes.ResourceID(spec.Name, typ, r.Name) {
			t.Errorf("%s item: id %q type %q name %q host %q", typ, r.ID, r.Type, r.Name, r.Host)
		}
	}
	return l
}

// fsPage is one page of host.filesystems.list.
func (s *hostStack) fsPage(in spec.ListInput) apitypes.List[spec.Filesystem] {
	s.t.Helper()
	out, err := s.p.filesystems(context.Background(), in)
	if err != nil {
		s.t.Fatalf("filesystems handler: %v", err)
	}
	return decodeList(s.t, spec.FilesystemsType, out,
		func(p spec.FilesystemProperties) string { return url.PathEscape(p.MountPoint) })
}

// fsAll is the whole list in one page of 1000.
func (s *hostStack) fsAll() []spec.FilesystemProperties {
	s.t.Helper()
	l := s.fsPage(spec.ListInput{Limit: 1000})
	if l.NextCursor != "" {
		s.t.Fatalf("limit 1000: nextCursor %q, want none", l.NextCursor)
	}
	out := make([]spec.FilesystemProperties, len(l.Items))
	for i, r := range l.Items {
		out[i] = r.Properties
	}
	return out
}

// fsFind is the item at mount point mp.
func fsFind(items []spec.FilesystemProperties, mp string) (spec.FilesystemProperties, bool) {
	for _, f := range items {
		if f.MountPoint == mp {
			return f, true
		}
	}
	return spec.FilesystemProperties{}, false
}

// fsAt is the item at mp, which must be listed exactly once.
func fsAt(t *testing.T, items []spec.FilesystemProperties, mp string) spec.FilesystemProperties {
	t.Helper()
	n := 0
	for _, f := range items {
		if f.MountPoint == mp {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%s listed %d times, want once; items %v", mp, n, mountPoints(items))
	}
	f, _ := fsFind(items, mp)
	return f
}

func mountPoints(items []spec.FilesystemProperties) []string {
	out := make([]string, len(items))
	for i, f := range items {
		out[i] = f.MountPoint
	}
	return out
}

// wantUsage checks f carries u's numbers, by WI 4.2's table.
func wantUsage(t *testing.T, f spec.FilesystemProperties, u Usage) {
	t.Helper()
	size, used, avail := u.Blocks*u.Frsize, (u.Blocks-u.Bfree)*u.Frsize, u.Bavail*u.Frsize
	pct := float64(used) / float64(used+avail) * 100
	if f.SizeBytes == nil || f.UsedBytes == nil || f.AvailableBytes == nil || f.UsedPercent == nil ||
		*f.SizeBytes != size || *f.UsedBytes != used || *f.AvailableBytes != avail || !near(*f.UsedPercent, pct) {
		t.Errorf("%s: size %s used %s avail %s pct %s; want %d %d %d %v (unavailable %v)", f.MountPoint,
			show(f.SizeBytes), show(f.UsedBytes), show(f.AvailableBytes), show(f.UsedPercent), size, used, avail, pct,
			f.Unavailable)
	}
	if u.Files == 0 {
		return
	}
	if f.InodesTotal == nil || f.InodesFree == nil || f.InodesUsed == nil || *f.InodesTotal != u.Files ||
		*f.InodesFree != u.Ffree || *f.InodesUsed != u.Files-u.Ffree {
		t.Errorf("%s: inodes %s/%s/%s, want %d/%d/%d", f.MountPoint, show(f.InodesTotal), show(f.InodesUsed),
			show(f.InodesFree), u.Files, u.Files-u.Ffree, u.Ffree)
	}
}

// wantNoUsage checks every usage field of f is null with why.
func wantNoUsage(t *testing.T, f spec.FilesystemProperties, why string) {
	t.Helper()
	if f.SizeBytes != nil || f.UsedBytes != nil || f.AvailableBytes != nil || f.UsedPercent != nil ||
		f.InodesTotal != nil || f.InodesUsed != nil || f.InodesFree != nil {
		t.Errorf("%s: usage %s %s %s %s, want all null", f.MountPoint, show(f.SizeBytes), show(f.UsedBytes),
			show(f.AvailableBytes), show(f.InodesTotal))
	}
	for _, k := range usageFields {
		if got := f.Unavailable[k]; got != why {
			t.Errorf("%s: %s reason %q, want %q", f.MountPoint, k, got, why)
		}
	}
}

// wantOnlyMountinfo checks F-FS saw nothing but the two mountinfo files:
// no mount point ever reaches the FS (D-3).
func wantOnlyMountinfo(t *testing.T, f *fakeFS) {
	t.Helper()
	for _, c := range f.recorded() {
		if c.name != pathHostMountinfo && c.name != pathSelfMountinfo {
			t.Errorf("F-FS access %s: the filesystems handler reads only the two mountinfo files", c)
		}
	}
}

// rewriteMI rewrites the entries of one mountinfo file: fn edits an entry
// and returns false to drop it.
func rewriteMI(t *testing.T, f *fakeFS, name string, fn func(e *miEntry) bool) {
	t.Helper()
	var keep []miEntry
	for _, e := range f.mountinfo(t, name) {
		if fn(&e) {
			keep = append(keep, e)
		}
	}
	f.writeMountinfo(name, keep)
}

// removeMounts drops every entry at mp from both files.
func removeMounts(t *testing.T, f *fakeFS, mp string) {
	t.Helper()
	for _, name := range []string{pathHostMountinfo, pathSelfMountinfo} {
		rewriteMI(t, f, name, func(e *miEntry) bool { return e.mountPoint() != mp })
	}
}

// setFSType sets an entry's filesystem type, the field after "-".
func setFSType(t *testing.T, e *miEntry, fstype string) {
	t.Helper()
	i := slices.Index(e.rest, "-")
	if i < 0 || i+1 >= len(e.rest) {
		t.Fatalf("mountinfo entry %s has no separator", e)
	}
	e.rest[i+1] = fstype
}

// statfsCalls is every mount point F-STATFS was asked for, in order.
func (s *fakeStatfs) recordedCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// row returns a distinct F-STATFS row per seed.
func fsRow(seed uint64) Usage {
	return Usage{IsDir: true, Blocks: 1000 * seed, Bfree: 400 * seed, Bavail: 300 * seed, Frsize: 4096,
		Files: 50 * seed, Ffree: 10 * seed}
}

func mount(id, parent int, dev, mp string) hostparse.Mount {
	var maj, mn uint32
	_, _ = fmt.Sscanf(dev, "%d:%d", &maj, &mn)
	return hostparse.Mount{ID: id, ParentID: parent, Major: maj, Minor: mn, Root: "/", MountPoint: mp, FSType: "ext4"}
}

// D-9's reachability on hand-made tables: a parent cycle, an unlisted
// parent that is not a root entry, and a child whose mount point is not
// below its parent's are never walked, so nothing at them is visible.
func TestSelfViewMalformedTables(t *testing.T) {
	v := newSelfView([]hostparse.Mount{
		mount(1, 99, "0:1", "/"),
		mount(2, 1, "8:1", "/a"),
		mount(3, 4, "8:2", "/b"), // 3 and 4 are a cycle
		mount(4, 3, "8:3", "/b/c"),
		mount(5, 77, "8:4", "/d"),    // unlisted parent, not at /
		mount(6, 2, "8:5", "/e/f"),   // not below its parent /a
		mount(7, 2, "8:6", "/a/g/h"), // walked: no child of /a at /a/g
		mount(8, 8, "8:7", "/x"),     // its own parent, not at /
	})
	for _, c := range []struct {
		m    hostparse.Mount
		want bool
	}{
		{mount(0, 0, "0:1", "/"), true},
		{mount(0, 0, "8:1", "/a"), true},
		{mount(0, 0, "8:2", "/b"), false},
		{mount(0, 0, "8:3", "/b/c"), false},
		{mount(0, 0, "8:4", "/d"), false},
		{mount(0, 0, "8:5", "/e/f"), false},
		{mount(0, 0, "8:6", "/a/g/h"), true},
		{mount(0, 0, "8:7", "/x"), false},
		{mount(0, 0, "9:9", "/a"), false}, // another maj:min at a visible path
		{mount(0, 0, "8:1", "/nothing"), false},
	} {
		if got := v.visible(c.m); got != c.want {
			t.Errorf("visible(%s %d:%d) = %v, want %v", c.m.MountPoint, c.m.Major, c.m.Minor, got, c.want)
		}
	}
}

// A deep parent chain is walked without recursion and each entry once.
func TestSelfViewDeepChain(t *testing.T) {
	const n = 20000
	ms := []hostparse.Mount{mount(1, 0, "0:1", "/")}
	mp := ""
	for i := 2; i <= n; i++ {
		mp += "/d"
		ms = append(ms, mount(i, i-1, fmt.Sprintf("8:%d", i), mp))
	}
	v := newSelfView(ms)
	if !v.visible(ms[n-1]) {
		t.Errorf("the deepest entry of a %d-long chain is not visible", n)
	}
}

// A-21's fixed list, by fstype and mount point.
func TestExcludedMount(t *testing.T) {
	for _, c := range []struct {
		fstype, mp string
		want       bool
	}{
		{"ext4", "/", false},
		{"overlay", "/", false},
		{"overlay", "/var/lib/docker/overlay2/x/merged", true},
		{"overlay", "/var/lib/dockerx/merged", false},
		{"overlay", "/run/containers/storage/x", true},
		{"squashfs", "/snap/core/1", true},
		{"squashfs", "/opt/image", false},
		{"tmpfs", "/tmp", false},
		{"tmpfs", "/run", false},
		{"tmpfs", "/dev/shm", false},
		{"tmpfs", "/var/tmp", true},
		{"tmpfs", "/run/lock", true},
		{"fuse.lxcfs", "/proc/cpuinfo", true},
		{"fuse.sshfs", "/mnt/remote", false},
		{"nfs4", "/mnt/nfs", false},
		{"btrfs", "/home", false},
	} {
		if got := excludedMount(hostparse.Mount{FSType: c.fstype, MountPoint: c.mp}); got != c.want {
			t.Errorf("excludedMount(%s at %s) = %v, want %v", c.fstype, c.mp, got, c.want)
		}
	}
}

// The usage arithmetic's edges: no blocks, Bfree above Blocks, an
// overflowing product and Ffree above Files never give a wrong number.
func TestFillUsageEdges(t *testing.T) {
	var f spec.FilesystemProperties
	fillUsage(&f, Usage{IsDir: true, Frsize: 4096, Files: 10, Ffree: 20})
	if f.UsedPercent != nil || !strings.HasPrefix(f.Unavailable["usedPercent"], prefixNotReported) ||
		f.InodesUsed == nil || *f.InodesUsed != 0 {
		t.Errorf("no blocks: %+v", f)
	}
	f = spec.FilesystemProperties{MountPoint: "/x"}
	fillUsage(&f, Usage{IsDir: true, Blocks: 1 << 60, Frsize: 1 << 10, Files: 1})
	if f.SizeBytes != nil || f.Unavailable["sizeBytes"] != "unrecognized format: statfs /x" {
		t.Errorf("overflow: %+v", f)
	}
	f = spec.FilesystemProperties{}
	fillUsage(&f, Usage{IsDir: true, Blocks: 10, Bfree: 20, Bavail: 5, Frsize: 1, Files: 1})
	if f.UsedBytes == nil || *f.UsedBytes != 0 || *f.UsedPercent != 0 {
		t.Errorf("Bfree > Blocks: %+v", f)
	}
}

// The filesystems cursor never exceeds spec.MaxCursorLength, whatever the
// mount point's length (orchestrator note from WI 1.2), and a long
// cursor pages on without skipping or repeating.
func TestFilesystemsCursorBound(t *testing.T) {
	s, st := newFSStack(t, capUBU)
	long := "/mnt/" + strings.Repeat("x y", 1300) // 3905 bytes, escaped 6505
	for i, mp := range []string{long + "1", long + "2", "/mnt/short"} {
		s.ffs.addMount(t, fmt.Sprintf("0 0 0:%d / %s rw - ext4 /dev/sd%c rw", 200+i, escapeMount(mp), 'a'+i), viewBoth)
		st.set(mp, fsRow(uint64(i+1)))
	}
	all := mountPoints(s.fsAll())
	var got []string
	cursor := ""
	for {
		l := s.fsPage(spec.ListInput{Limit: 1, Cursor: cursor})
		for _, r := range l.Items {
			got = append(got, r.Properties.MountPoint)
		}
		if l.NextCursor == "" {
			break
		}
		if len(l.NextCursor) > spec.MaxCursorLength || !cursorRE.MatchString(l.NextCursor) {
			t.Fatalf("cursor of %d characters %q breaks the schema", len(l.NextCursor), l.NextCursor[:20])
		}
		cursor = l.NextCursor
	}
	if !slices.Equal(got, all) {
		t.Errorf("pages %d items, want %d in order", len(got), len(all))
	}

	// A long cursor whose item is gone resumes at the first mount point
	// with its prefix: nothing after it is skipped.
	pos, err := decodeCursor(encodeCursor(url.PathEscape(long+"0"), long+"0"), mountPointOfName)
	if err != nil || pos.digest == nil {
		t.Fatalf("long cursor: %+v, %v", pos, err)
	}
	items := s.fsAll()
	page, _ := paginate(items, 1000, pos, func(f spec.FilesystemProperties) string { return f.MountPoint },
		filesystemName)
	if len(page) == 0 || page[0].MountPoint != long+"1" {
		t.Errorf("after a gone long item the page is %d items, want it to start at the next long mount point", len(page))
	}
}

// decodeCursor refuses what paginate never issues.
func TestDecodeCursorRefusesForeign(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	sum := sha256.Sum256([]byte("x"))
	for _, c := range []string{
		"!!", "-abc", "Zm9v=", enc([]byte("foo")), enc([]byte("%2F%zz")), enc([]byte("%2Fa/b")),
		enc([]byte{0}), enc(append([]byte{0}, sum[:5]...)), strings.Repeat("A", spec.MaxCursorLength+4),
	} {
		_, err := decodeCursor(c, mountPointOfName)
		var p *problem.Error
		if !errors.As(err, &p) || p.Slug() != problem.ValidationFailed {
			t.Errorf("decodeCursor(%.20q) = %v, want validation-failed", c, err)
		}
	}
	if pos, err := decodeCursor(enc([]byte("%2Fmnt%2Fmy%20disk")), mountPointOfName); err != nil ||
		pos.key != "/mnt/my disk" {
		t.Errorf("a filesystems cursor decodes to %+v, %v", pos, err)
	}
	if pos, err := decodeCursor(enc([]byte("vda")), nameIsKey); err != nil || pos.key != "vda" {
		t.Errorf("a disks cursor decodes to %+v, %v", pos, err)
	}
}

// Without either mountinfo file the list fails, as a dependency.
func TestFilesystemsNoMountTable(t *testing.T) {
	s, _ := newFSStack(t, capUBU)
	s.ffs.deny("proc/1/mountinfo")
	s.ffs.deny("proc/self/mountinfo")
	_, err := s.p.filesystems(context.Background(), spec.ListInput{})
	var p *problem.Error
	if !errors.As(err, &p) || p.Slug() != problem.DependencyUnavailable {
		t.Errorf("err = %v, want dependency-unavailable", err)
	}
}

// A seam error other than ENOENT leaves usage null with its reason.
func TestFilesystemsSeamPermissionDenied(t *testing.T) {
	s, st := newFSStack(t, capUBU)
	st.setErr("/run", &fs.PathError{Op: "statfs", Path: "/run", Err: syscall.EACCES})
	f := fsAt(t, s.fsAll(), "/run")
	wantNoUsage(t, f, "permission denied: /run")
}

// a21Types are A-21's excluded filesystem types, as WI 4.2 lists them.
var a21Types = []string{"proc", "sysfs", "devtmpfs", "devpts", "cgroup", "cgroup2", "securityfs", "debugfs", "tracefs",
	"bpf", "pstore", "mqueue", "hugetlbfs", "configfs", "fusectl", "autofs", "binfmt_misc", "nsfs", "efivarfs",
	"rpc_pipefs", "nfsd", "selinuxfs", "ramfs", "fuse.lxcfs", "fuse.gvfsd-fuse", "fuse.portal"}

// containerOverlays are one overlay mount point below each container
// runtime directory of A-21.
var containerOverlays = []string{
	"/var/lib/docker/overlay2/abc/merged",
	"/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/9/fs",
	"/run/containerd/io.containerd.runtime.v2.task/moby/abc/rootfs",
	"/var/lib/containers/storage/overlay/abc/merged",
	"/run/containers/storage/overlay-containers/abc/userdata/shm",
}

// dfDriftBlocks bounds how far df's used may be from statfs.txt's: the
// captures ran df and stat -f moments apart on live filesystems, and their
// used differ by up to 16 blocks (UBU / 14, its 253:17 filesystem 16, ALP /
// 9), so TC-HP-79's "within one block" is widened to this. sizeBytes and the
// statfs.txt numbers are still compared exactly.
const dfDriftBlocks = 32

// dfRow is one tools/df line.
type dfRow struct {
	size, used, avail uint64
	pct               float64
}

// dfTable parses a capture's tools/df by target: GNU df's --output form
// (target first) or BusyBox df's (target last).
func dfTable(t *testing.T, capture string) map[string]dfRow {
	t.Helper()
	lines := strings.Split(toolText(t, capture, "df"), "\n")
	gnu := strings.HasPrefix(lines[0], "Mounted on")
	out := map[string]dfRow{}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) < 6 {
			t.Fatalf("tools/df line %q", l)
		}
		target := f[5]
		if gnu {
			target = f[0]
		}
		u := func(s string) uint64 { return u64T(t, s) }
		pct, err := strconv.ParseFloat(strings.TrimSuffix(f[4], "%"), 64)
		if err != nil {
			t.Fatalf("tools/df Use%% %q", f[4])
		}
		out[target] = dfRow{size: u(f[1]), used: u(f[2]), avail: u(f[3]), pct: pct}
	}
	return out
}

// dfRowOf is mp's df row, or that of the df target with mp's maj:min in
// proc/1/mountinfo, the first by name.
func dfRowOf(df map[string]dfRow, hostAt map[string]hostparse.Mount, mp string) (string, dfRow, bool) {
	if r, ok := df[mp]; ok {
		return mp, r, true
	}
	m, ok := hostAt[mp]
	if !ok {
		return "", dfRow{}, false
	}
	var targets []string
	for target := range df {
		if o, ok := hostAt[target]; ok && mountDev(o) == mountDev(m) {
			targets = append(targets, target)
		}
	}
	if len(targets) == 0 {
		return "", dfRow{}, false
	}
	slices.Sort(targets)
	return targets[0], df[targets[0]], true
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// A group's mount whose mountinfo root is / is preferred over a shorter
// bind of a subdirectory of the same filesystem.
func TestFilesystemsPreferRootSlash(t *testing.T) {
	s, st := newFSStack(t, capUBU)
	s.ffs.addMount(t, "0 0 8:40 /data /a rw,relatime - ext4 /dev/sdh1 rw", viewBoth)
	s.ffs.addMount(t, "0 0 0:0 / /srv/data rw,relatime - ext4 /dev/sdh1 rw", viewBoth, sameDevAs("/a"))
	st.set("/a", fsRow(1))
	st.set("/srv/data", fsRow(2))
	items := s.fsAll()
	wantUsage(t, fsAt(t, items, "/srv/data"), fsRow(2))
	if _, ok := fsFind(items, "/a"); ok || st.count("/a") != 0 {
		t.Errorf("/a listed %v with %d seam calls; want the root-/ mount only", ok, st.count("/a"))
	}
}
