package host

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"math/bits"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// The mountinfo files (D-9): the host namespace's, the source of the mounts
// and their read-only flag, and nexusd's own, which decides visibility.
const (
	pathHostMountinfo = "proc/1/mountinfo"
	pathSelfMountinfo = "proc/self/mountinfo"
)

// reasonOwnNamespace ends the readOnly reason under the self fallback.
const reasonOwnNamespace = " (read-only flags in nexusd's own namespace would be wrong)"

// reasonNoInodes is a filesystem that reports no inode counts (btrfs, vfat).
var reasonNoInodes = reasonNotReported("the filesystem has no inode counts")

// usageFields are the JSON names of the fields a statfs fills.
var usageFields = []string{"sizeBytes", "usedBytes", "availableBytes", "usedPercent", "inodesTotal", "inodesUsed",
	"inodesFree"}

// excludedFSTypes are A-21's pseudo and virtual filesystem types.
var excludedFSTypes = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "cgroup": true, "cgroup2": true,
	"securityfs": true, "debugfs": true, "tracefs": true, "bpf": true, "pstore": true, "mqueue": true,
	"hugetlbfs": true, "configfs": true, "fusectl": true, "autofs": true, "binfmt_misc": true, "nsfs": true,
	"efivarfs": true, "rpc_pipefs": true, "nfsd": true, "selinuxfs": true, "ramfs": true, "fuse.lxcfs": true,
	"fuse.gvfsd-fuse": true, "fuse.portal": true,
}

// containerOverlayDirs hold the container runtimes' overlay mounts (A-21).
var containerOverlayDirs = []string{"/var/lib/docker/", "/var/lib/containerd/", "/run/containerd/",
	"/var/lib/containers/", "/run/containers/"}

// keptTmpfs are the tmpfs mount points that are listed (FR-7).
var keptTmpfs = map[string]bool{"/tmp": true, "/run": true, "/dev/shm": true}

// excludedMount reports whether A-21's fixed list drops m by its type and
// mount point. The non-directory exclusion is the Statfs seam's IsDir,
// decided when the group is picked.
func excludedMount(m hostparse.Mount) bool {
	switch {
	case excludedFSTypes[m.FSType]:
		return true
	case m.FSType == "overlay":
		for _, d := range containerOverlayDirs {
			if strings.HasPrefix(m.MountPoint, d) {
				return true
			}
		}
	case m.FSType == "squashfs":
		return strings.HasPrefix(m.MountPoint, "/snap/")
	case m.FSType == "tmpfs":
		return !keptTmpfs[m.MountPoint]
	}
	return false
}

// devOf is a mount's maj:min, the key of its filesystem.
type devOf struct{ major, minor uint32 }

func mountDev(m hostparse.Mount) devOf { return devOf{m.Major, m.Minor} }

// mountTable is the mount list of one request: the host's, or nexusd's own
// under the fallback, with the reason readOnly is unavailable then.
type mountTable struct {
	mounts   []hostparse.Mount
	fallback bool   // the mounts are proc/self/mountinfo's
	roWhy    string // set under the fallback
	hostWhy  string // proc/1/mountinfo could not be read
	self     []hostparse.Mount
	selfWhy  string // proc/self/mountinfo could not be read
}

// readMountTable reads proc/1/mountinfo and proc/self/mountinfo, each once
// (TC-HP-82). When the host's file cannot be read the self file is the
// mount list too. It fails only when neither can be read.
func (p *Provider) readMountTable() (mountTable, error) {
	var t mountTable
	if b, err := p.src.read(pathSelfMountinfo); err != nil {
		t.selfWhy = reasonFor(pathSelfMountinfo, err)
	} else if ms, err := hostparse.Mountinfo(b); err != nil {
		t.selfWhy = reasonFor(pathSelfMountinfo, err)
	} else {
		t.self = ms
	}
	if b, err := p.src.read(pathHostMountinfo); err != nil {
		t.hostWhy = reasonFor(pathHostMountinfo, err)
	} else if ms, err := hostparse.Mountinfo(b); err != nil {
		t.hostWhy = reasonFor(pathHostMountinfo, err)
	} else {
		t.mounts = ms
		return t, nil
	}
	if t.self == nil {
		return t, problem.New(problem.DependencyUnavailable,
			"no mount table: "+t.hostWhy+"; "+t.selfWhy).With("provider", spec.Name)
	}
	t.mounts, t.fallback, t.roWhy = t.self, true, t.hostWhy+reasonOwnNamespace
	return t, nil
}

// filesystems is host.filesystems.list.
func (p *Provider) filesystems(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Filesystem], error) {
	after, err := decodeCursor(in.Cursor, mountPointOfName)
	if err != nil {
		return apitypes.List[spec.Filesystem]{}, err
	}
	all, err := p.collectFilesystems(ctx)
	if err != nil {
		return apitypes.List[spec.Filesystem]{}, err
	}
	page, next := paginate(all, in.Limit, after,
		func(f spec.FilesystemProperties) string { return f.MountPoint }, filesystemName)
	out := apitypes.List[spec.Filesystem]{Items: make([]spec.Filesystem, 0, len(page)), NextCursor: next}
	for _, f := range page {
		out.Items = append(out.Items, apitypes.NewResource(p.host, spec.Name, spec.FilesystemsType, filesystemName(f), f))
	}
	return out, nil
}

// filesystemName is an item's name: its path-escaped mount point.
func filesystemName(f spec.FilesystemProperties) string { return url.PathEscape(f.MountPoint) }

// mountPointOfName maps a filesystems cursor's name back to its mount
// point. Only the escaping of an absolute path is one.
func mountPointOfName(name string) (string, bool) {
	mp, err := url.PathUnescape(name)
	return mp, err == nil && strings.HasPrefix(mp, "/") && url.PathEscape(mp) == name
}

// collectFilesystems lists the real filesystems, sorted by mount point
// (WI 4.2, D-9, A-21):
//  1. the mounts of proc/1/mountinfo (proc/self/mountinfo under the
//     fallback) less A-21's fixed exclusions, grouped by maj:min;
//  2. each group, concurrently, gives at most one item: its visible
//     candidates are tried in preference order with one bounded seam call
//     each, a file bind is skipped and the first other answer is the item;
//     a group with no visible candidate is its preferred candidate with
//     usage not visible and no syscall; a group of file binds only is
//     dropped.
//
// A group costs at most one sourceTimeout, and the groups run at once, so a
// hung mount holds up nothing else (NFR-1). No mount point is ever passed
// to the FS (D-3).
func (p *Provider) collectFilesystems(ctx context.Context) ([]spec.FilesystemProperties, error) {
	t, err := p.readMountTable()
	if err != nil {
		return nil, err
	}
	return p.filesystemsOf(ctx, t), nil
}

// filesystemsOf is collectFilesystems for an already read mount table.
func (p *Provider) filesystemsOf(ctx context.Context, t mountTable) []spec.FilesystemProperties {
	var view *selfView
	if t.self != nil {
		view = newSelfView(t.self)
	}

	groups := map[devOf][]hostparse.Mount{}
	var order []devOf
	for _, m := range t.mounts {
		if excludedMount(m) {
			continue
		}
		d := mountDev(m)
		if _, ok := groups[d]; !ok {
			order = append(order, d)
		}
		groups[d] = append(groups[d], m)
	}

	items := make([]*spec.FilesystemProperties, len(order))
	visibleOf := make([][]hostparse.Mount, len(order))
	for i, d := range order {
		cands := groups[d]
		slices.SortStableFunc(cands, preferMount)
		var visible []hostparse.Mount
		if view != nil {
			for _, m := range cands {
				if view.visible(m) {
					visible = append(visible, m)
				}
			}
		}
		if len(visible) == 0 {
			why := reasonNotVisible(cands[0].MountPoint)
			if view == nil {
				why = t.selfWhy
			}
			f := t.item(cands[0])
			noUsage(&f, why)
			items[i] = &f
			continue
		}
		visibleOf[i] = visible
	}
	// One bounded statfs per group at once. fanOut raises a panic of a
	// read in this goroutine, where the request's recovery sees it.
	fanOut(len(order), func(i int) {
		if visibleOf[i] != nil {
			items[i] = p.pickFilesystem(ctx, t, visibleOf[i])
		}
	})

	out := make([]spec.FilesystemProperties, 0, len(items))
	for _, f := range items {
		if f != nil {
			out = append(out, *f)
		}
	}
	slices.SortStableFunc(out, func(a, b spec.FilesystemProperties) int { return strings.Compare(a.MountPoint, b.MountPoint) })
	return out
}

// preferMount orders a group's candidates: mountinfo root "/" first, then
// the shortest mount point, then by mount point.
func preferMount(a, b hostparse.Mount) int {
	if ar, br := a.Root == "/", b.Root == "/"; ar != br {
		if ar {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(len(a.MountPoint), len(b.MountPoint)); c != 0 {
		return c
	}
	return strings.Compare(a.MountPoint, b.MountPoint)
}

// pickFilesystem tries the visible candidates in order, one bounded seam
// call each, and returns the group's item, or nil when every one is a file
// bind.
func (p *Provider) pickFilesystem(ctx context.Context, t mountTable, visible []hostparse.Mount) *spec.FilesystemProperties {
	for _, m := range visible {
		u, err := p.src.statfs(ctx, m.MountPoint)
		if err == nil && !u.IsDir {
			continue
		}
		f := t.item(m)
		switch {
		case err == nil:
			fillUsage(&f, u)
		case isNotExist(err) && t.fallback:
			noUsage(&f, prefixNotPresent+m.MountPoint)
		case isNotExist(err):
			// The second line of defence (D-9): the path is missing in
			// nexusd's namespace, so a statfs there measured nothing of it.
			noUsage(&f, reasonNotVisible(m.MountPoint))
		default:
			noUsage(&f, reasonFor(m.MountPoint, err))
		}
		return &f
	}
	return nil
}

// isNotExist is an ENOENT from the seam itself, not a timeout.
func isNotExist(err error) bool {
	var timeout *timeoutError
	var pending *pendingError
	return errors.Is(err, fs.ErrNotExist) && !errors.As(err, &timeout) && !errors.As(err, &pending)
}

// item is m's filesystem without usage: its identity and readOnly, which
// comes from the host's line, so it is reported even for a hidden or failed
// mount.
func (t mountTable) item(m hostparse.Mount) spec.FilesystemProperties {
	f := spec.FilesystemProperties{MountPoint: m.MountPoint, Source: m.Source, FSType: m.FSType}
	if t.fallback {
		setWhy(&f.Unavailable, "readOnly", t.roWhy)
		return f
	}
	ro := slices.Contains(m.Options, "ro") || slices.Contains(strings.Split(m.SuperOptions, ","), "ro")
	f.ReadOnly = &ro
	return f
}

// noUsage leaves every usage field null with why.
func noUsage(f *spec.FilesystemProperties, why string) {
	for _, k := range usageFields {
		setWhy(&f.Unavailable, k, why)
	}
}

// fillUsage is WI 4.2's table: size = Blocks × Frsize, used = (Blocks −
// Bfree) × Frsize, available = Bavail × Frsize, used % = used / (used +
// available) × 100 (df's Use%, A-22), and the inode counts. A product that
// overflows 64 bits is not a real answer and is reported unrecognized.
func fillUsage(f *spec.FilesystemProperties, u Usage) {
	bad := prefixUnrecognized + "statfs " + f.MountPoint
	size, ok1 := mulU64(u.Blocks, u.Frsize)
	used, ok2 := mulU64(u.Blocks-min(u.Bfree, u.Blocks), u.Frsize)
	avail, ok3 := mulU64(u.Bavail, u.Frsize)
	if ok1 && ok2 && ok3 {
		f.SizeBytes, f.UsedBytes, f.AvailableBytes = &size, &used, &avail
		if total := float64(used) + float64(avail); total > 0 {
			f.UsedPercent = ptr(float64(used) / total * 100)
		} else {
			setWhy(&f.Unavailable, "usedPercent", reasonNotReported("the filesystem has no blocks"))
		}
	} else {
		for _, k := range usageFields[:4] {
			setWhy(&f.Unavailable, k, bad)
		}
	}
	if u.Files == 0 {
		for _, k := range usageFields[4:] {
			setWhy(&f.Unavailable, k, reasonNoInodes)
		}
		return
	}
	files, free := u.Files, min(u.Ffree, u.Files)
	inUse := files - free
	f.InodesTotal, f.InodesFree, f.InodesUsed = &files, &free, &inUse
}

// mulU64 is a × b, and false when it overflows.
func mulU64(a, b uint64) (uint64, bool) {
	hi, lo := bits.Mul64(a, b)
	return lo, hi == 0
}

// selfView is D-9's reachability over proc/self/mountinfo, computed once
// per request: the maj:min of the reachable entries at each decoded mount
// point.
type selfView struct {
	entries []hostparse.Mount
	byID    map[int]int
	// children counts, by (parent ID, mount point), the entries with that
	// parent at that mount point, leaving out an entry that is its own
	// parent (proc(5)'s namespace root).
	children map[parentAt]int
	state    []walkState
	at       map[string]map[devOf]bool
}

type parentAt struct {
	parent int
	mp     string
}

type walkState uint8

const (
	walkUnknown walkState = iota
	walkVisiting
	walkYes
	walkNo
)

func newSelfView(entries []hostparse.Mount) *selfView {
	v := &selfView{
		entries:  entries,
		byID:     make(map[int]int, len(entries)),
		children: make(map[parentAt]int, len(entries)),
		state:    make([]walkState, len(entries)),
		at:       make(map[string]map[devOf]bool, len(entries)),
	}
	for i, e := range entries {
		if _, dup := v.byID[e.ID]; !dup {
			v.byID[e.ID] = i
		}
		if e.ParentID != e.ID {
			v.children[parentAt{e.ParentID, e.MountPoint}]++
		}
	}
	for i, e := range entries {
		if v.walked(i) && !v.covered(i) {
			devs := v.at[e.MountPoint]
			if devs == nil {
				devs = map[devOf]bool{}
				v.at[e.MountPoint] = devs
			}
			devs[mountDev(e)] = true
		}
	}
	return v
}

// visible is D-9's rule for a candidate: at least one reachable self entry
// at its mount point, and every one with its maj:min. Only mountinfo's
// maj:min is compared, never a stat st_dev, so btrfs subvolumes are
// treated as any other mount (A-38).
func (v *selfView) visible(m hostparse.Mount) bool {
	devs := v.at[m.MountPoint]
	return len(devs) == 1 && devs[mountDev(m)]
}

// covered: another entry at the same mount point is stacked on entry i.
func (v *selfView) covered(i int) bool {
	e := v.entries[i]
	return v.children[parentAt{e.ID, e.MountPoint}] > 0
}

// root: at / with a parent ID that is its own or no listed entry's.
func (v *selfView) root(i int) bool {
	e := v.entries[i]
	if e.MountPoint != "/" {
		return false
	}
	_, listed := v.byID[e.ParentID]
	return e.ParentID == e.ID || !listed
}

// walked reports whether a path lookup passes through entry i, memoized
// along the parent chain. The chain is followed iteratively, and an entry
// met again before it is resolved (a parent cycle) is not walked, nor is
// anything below it.
func (v *selfView) walked(i int) bool {
	var chain []int
	result := false
	for j := i; ; {
		if s := v.state[j]; s != walkUnknown {
			result = s == walkYes // walkVisiting: a cycle
			break
		}
		if v.root(j) {
			v.state[j], result = walkYes, true
			break
		}
		p, ok := v.byID[v.entries[j].ParentID]
		if !ok || p == j {
			v.state[j], result = walkNo, false
			break
		}
		v.state[j] = walkVisiting
		chain = append(chain, j)
		j = p
	}
	for k := len(chain) - 1; k >= 0; k-- {
		j := chain[k]
		result = result && v.step(j)
		v.state[j] = walkNo
		if result {
			v.state[j] = walkYes
		}
	}
	return result
}

// step is the condition on entry j given that its parent P is walked: j is
// stacked on P, or P is not covered and no other child of P sits at a
// proper ancestor of j's mount point strictly below P's (the lookup would
// leave P there first). Each such ancestor costs one map lookup.
func (v *selfView) step(j int) bool {
	e := v.entries[j]
	pi := v.byID[e.ParentID]
	pm := v.entries[pi].MountPoint
	if e.MountPoint == pm {
		return true
	}
	if v.covered(pi) || !below(e.MountPoint, pm) {
		return false
	}
	for a := path.Dir(e.MountPoint); a != pm && a != "/"; a = path.Dir(a) {
		if v.children[parentAt{e.ParentID, a}] > 0 {
			return false
		}
	}
	return true
}

// below reports whether mount point c is strictly below p.
func below(c, p string) bool {
	if p == "/" {
		return c != "/" && strings.HasPrefix(c, "/")
	}
	return strings.HasPrefix(c, p+"/")
}
