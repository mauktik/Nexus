package host

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// blockDir lists the whole block devices: disks, NVMe namespaces, loop,
// ram and zram devices; partitions are below their disk.
const blockDir = "sys/block"

// diskItem is one disk with its kernel name.
type diskItem struct {
	name  string
	props spec.DiskProperties
}

func diskItemName(d diskItem) string { return d.name }

// disks is host.disks.list.
func (p *Provider) disks(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Disk], error) {
	after, err := decodeCursor(in.Cursor, nameIsKey)
	if err != nil {
		return apitypes.List[spec.Disk]{}, err
	}
	all, err := p.collectDisks(ctx, p.run.Load().snapshot())
	if err != nil {
		return apitypes.List[spec.Disk]{}, err
	}
	page, next := paginate(all, in.Limit, after, diskItemName, diskItemName)
	out := apitypes.List[spec.Disk]{Items: make([]spec.Disk, 0, len(page)), NextCursor: next}
	for _, d := range page {
		out.Items = append(out.Items, apitypes.NewResource(p.host, spec.Name, spec.DisksType, d.name, d.props))
	}
	return out, nil
}

// collectDisks lists sys/block, sorted by name, leaving out ram*, zram*
// and loop* without loop/backing_file (FR-8). Each disk's rates come from
// snap (nil when the sampler is not running), joined on its name and the
// maj:min of sys/block/<d>/dev, so partitions and devices sys/block does
// not list never appear here. Partition mount points come from the host's
// mountinfo (D-9's file and fallback). Only an unreadable sys/block fails
// the list; anything else leaves its own field null with the reason. The
// attributes are kernel memory, read without bounded.
func (p *Provider) collectDisks(_ context.Context, snap *snapshot) ([]diskItem, error) {
	entries, err := p.src.readDir(blockDir)
	if err != nil {
		return nil, problem.New(problem.DependencyUnavailable, reasonFor(blockDir, err)).With("provider", spec.Name)
	}
	var mounts map[devOf][]string
	mountsWhy := ""
	if t, err := p.readMountTable(); err != nil {
		mountsWhy = t.hostWhy
	} else {
		mounts = mountsByDev(t.mounts)
	}
	now := p.now()

	var out []diskItem
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") {
			continue
		}
		if _, err := segPath(segName, blockDir, name); err != nil {
			continue
		}
		if strings.HasPrefix(name, "loop") {
			backing, _ := segPath(segName, blockDir, name, "loop", "backing_file")
			if _, err := p.src.stat(backing); errors.Is(err, fs.ErrNotExist) {
				continue // an unbacked loop device
			}
		}
		out = append(out, diskItem{name: name, props: p.collectDisk(name, snap, now, mounts, mountsWhy)})
	}
	slices.SortFunc(out, func(a, b diskItem) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// collectDisk reads one disk. name has passed segPath.
func (p *Provider) collectDisk(name string, snap *snapshot, now time.Time, mounts map[devOf][]string,
	mountsWhy string) spec.DiskProperties {
	out := spec.DiskProperties{Sampler: snap.sampler(now)}
	un := &out.Unavailable
	attr := func(rest ...string) string {
		n, _ := segPath(segName, blockDir, name, rest...) // name is valid; rest is fixed
		return n
	}

	model := attr("device", "model")
	switch b, err := p.src.read(model); {
	case errors.Is(err, fs.ErrNotExist):
		setWhy(un, "model", reasonNotReported("no "+displayPath(model)))
	case err != nil:
		setWhy(un, "model", reasonFor(model, err))
	default:
		if m := hostparse.ScalarString(b); m != "" {
			out.Model = &m
		} else {
			setWhy(un, "model", reasonNotReported("empty "+displayPath(model)))
		}
	}

	if v, why := readAttr(p.src, attr("size"), hostparse.BlockSize); why != "" {
		setWhy(un, "sizeBytes", why)
	} else {
		out.SizeBytes = &v
	}
	if v, why := readAttr(p.src, attr("queue", "rotational"), hostparse.Rotational); why != "" {
		setWhy(un, "rotational", why)
	} else {
		out.Rotational = &v
	}
	if v, why := readAttr(p.src, attr("removable"), hostparse.Removable); why != "" {
		setWhy(un, "removable", why)
	} else {
		out.Removable = &v
	}

	p.diskRates(&out, name, attr("dev"), snap, now)
	out.Partitions = p.partitions(un, attr(), mounts, mountsWhy)
	return out
}

// readAttr reads and parses one attribute, or gives the reason it cannot.
func readAttr[T any](src *source, name string, parse func([]byte) (T, error)) (T, string) {
	var zero T
	b, err := src.read(name)
	if err != nil {
		return zero, reasonFor(name, err)
	}
	v, err := parse(b)
	if err != nil {
		return zero, reasonFor(name, err)
	}
	return v, ""
}

// diskRates fills the sampler-derived rates: the snapshot's entry for name,
// only when its maj:min is the one sys/block/<d>/dev reports now. A device
// the sampler has not seen under that name and maj:min is warming up.
func (p *Provider) diskRates(out *spec.DiskProperties, name, devPath string, snap *snapshot, now time.Time) {
	fields := []struct {
		json string
		dst  **float64
		val  func(diskRates) rateVal
	}{
		{"readBytesPerSecond", &out.ReadBytesPerSecond, func(r diskRates) rateVal { return r.readBps }},
		{"writeBytesPerSecond", &out.WriteBytesPerSecond, func(r diskRates) rateVal { return r.writeBps }},
		{"readIops", &out.ReadIOPS, func(r diskRates) rateVal { return r.readIOPS }},
		{"writeIops", &out.WriteIOPS, func(r diskRates) rateVal { return r.writeIOPS }},
	}
	dev, why := readAttr(p.src, devPath, parseDev)
	var r diskRates
	if why == "" {
		why = snap.blocked(now)
	}
	if why == "" {
		switch rr, ok := snap.disks[name]; {
		case snap.disks == nil:
			why = snap.disksWhy
		case !ok || (devOf{rr.major, rr.minor}) != dev:
			why = reasonWarmingUp
		default:
			r = rr
		}
	}
	for _, f := range fields {
		if why != "" {
			setWhy(&out.Unavailable, f.json, why)
			continue
		}
		v, w := snap.val(now, f.val(r))
		if v == nil {
			setWhy(&out.Unavailable, f.json, w)
			continue
		}
		*f.dst = v
	}
}

// partitions lists the partitions below dir (sys/block/<d>): the
// subdirectories with a partition attribute, ordered by partition number.
// A partition's mount points are the host mounts with its maj:min.
func (p *Provider) partitions(un *spec.Unavailable, dir string, mounts map[devOf][]string,
	mountsWhy string) spec.Array[spec.Partition] {
	entries, err := p.src.readDir(dir)
	if err != nil {
		setWhy(un, "partitions", reasonFor(dir, err))
		return nil
	}
	type numbered struct {
		num int64
		p   spec.Partition
	}
	var parts []numbered
	for _, e := range entries {
		if !e.IsDir() {
			continue // attributes and the bdi, device and subsystem links
		}
		attr := func(leaf string) string {
			n, _ := segPath(segName, dir, e.Name(), leaf)
			return n
		}
		if _, err := segPath(segName, dir, e.Name()); err != nil {
			continue
		}
		num, why := readAttr(p.src, attr("partition"), hostparse.ScalarInt)
		if why != "" {
			continue // queue, holders, power and the like
		}
		part := spec.Partition{Name: e.Name()}
		if v, why := readAttr(p.src, attr("size"), hostparse.BlockSize); why != "" {
			setWhy(&part.Unavailable, "sizeBytes", why)
		} else {
			part.SizeBytes = &v
		}
		switch dev, why := readAttr(p.src, attr("dev"), parseDev); {
		case why != "":
			setWhy(&part.Unavailable, "mountPoints", why)
		case mounts == nil:
			setWhy(&part.Unavailable, "mountPoints", mountsWhy)
		default:
			part.MountPoints = spec.Array[string](slices.Clone(mounts[dev]))
		}
		parts = append(parts, numbered{num: num, p: part})
	}
	slices.SortFunc(parts, func(a, b numbered) int {
		if c := cmp.Compare(a.num, b.num); c != 0 {
			return c
		}
		return strings.Compare(a.p.Name, b.p.Name)
	})
	out := make(spec.Array[spec.Partition], 0, len(parts))
	for _, n := range parts {
		out = append(out, n.p)
	}
	return out
}

// mountsByDev indexes the mount points of a mount list by maj:min, each
// list sorted and without repeats.
func mountsByDev(ms []hostparse.Mount) map[devOf][]string {
	out := map[devOf][]string{}
	for _, m := range ms {
		d := mountDev(m)
		out[d] = append(out[d], m.MountPoint)
	}
	for d, l := range out {
		slices.Sort(l)
		out[d] = slices.Compact(l)
	}
	return out
}

// errBadDev is a dev attribute that is not maj:min.
var errBadDev = errors.New("sysfs: dev is not maj:min")

// parseDev parses a sysfs dev attribute, "maj:min\n".
func parseDev(b []byte) (devOf, error) {
	maj, mn, ok := strings.Cut(strings.TrimSpace(string(b)), ":")
	a, err1 := strconv.ParseUint(maj, 10, 32)
	c, err2 := strconv.ParseUint(mn, 10, 32)
	if !ok || err1 != nil || err2 != nil {
		return devOf{}, errBadDev
	}
	return devOf{uint32(a), uint32(c)}, nil
}
