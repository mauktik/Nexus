package host

import (
	"cmp"
	"context"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// summary is host.summary.get.
func (p *Provider) summary(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.Summary], error) {
	return currentList(p, spec.SummaryType, p.collectSummary(ctx, p.run.Load().snapshot())), nil
}

// summaryParts is what the summary's collectors read, each written by its
// own goroutine only.
type summaryParts struct {
	identity spec.SummaryIdentity
	cpu      spec.SummaryCPU
	memory   spec.MemoryProperties
	health   spec.SummaryHealth

	fs    []spec.FilesystemProperties
	fsWhy string

	ifaces   []ifaceItem
	devices  map[string]bool // sys/class/net/<if>/device exists
	ifaceWhy string

	gateways spec.Array[spec.Gateway]
	gwWhy    string
}

// collectSummary runs the identity-basic, CPU-basic, memory, filesystems,
// interfaces, gateways and health-flags collectors at once and waits for
// all (WI 4.5). Each is bounded on its own (D-7): a hung mount or command
// costs one sourceTimeout, not one per collector. The sampler-derived
// values all come from snap, read once with now, so the summary's single
// sampler object describes every one of them.
func (p *Provider) collectSummary(ctx context.Context, snap *snapshot) spec.SummaryProperties {
	now := p.now()
	var r summaryParts
	concurrently(
		func() { r.identity = p.summaryIdentity() },
		func() { r.cpu = p.summaryCPU(snap, now) },
		func() { r.memory = p.collectMemory() },
		func() { r.fs, r.fsWhy = p.summaryFilesystems(ctx) },
		func() { r.ifaces, r.devices, r.ifaceWhy = p.summaryInterfaces(ctx, snap) },
		func() { r.gateways, r.gwWhy = p.defaultGateways() },
		func() { r.health = p.summaryHealth(ctx) },
	)

	out := spec.SummaryProperties{Sampler: snap.sampler(now), Identity: r.identity, CPU: r.cpu, Health: r.health}
	out.Memory = projectMemory(r.memory)

	out.FilesystemsTotal = len(r.fs)
	out.FilesystemsTruncated = len(r.fs) > spec.SummaryMaxFilesystems
	out.Filesystems = make(spec.Array[spec.SummaryFilesystem], 0, min(len(r.fs), spec.SummaryMaxFilesystems))
	for _, f := range r.fs[:min(len(r.fs), spec.SummaryMaxFilesystems)] {
		out.Filesystems = append(out.Filesystems, projectFilesystem(f))
	}
	if r.fsWhy != "" {
		setWhy(&out.Unavailable, "filesystems", r.fsWhy)
	}

	out.Network = projectNetwork(r.ifaces, r.devices)
	if r.ifaceWhy != "" {
		setWhy(&out.Network.Unavailable, "interfaces", r.ifaceWhy)
	}
	out.Network.DefaultGateways = spec.Array[spec.Gateway]{}
	if r.gwWhy != "" {
		setWhy(&out.Network.Unavailable, "defaultGateways", r.gwWhy)
	} else {
		out.Network.DefaultGateways = r.gateways
	}
	return out
}

// concurrently runs each fn in its own goroutine and waits for all. A
// panic in one is raised again here, in the caller's goroutine, once all
// have returned, so the dispatcher's recovery sees it and no goroutine
// outlives the call.
func concurrently(fns ...func()) {
	panics := make([]any, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Go(func() {
			defer func() { panics[i] = recover() }()
			fn()
		})
	}
	wg.Wait()
	for _, v := range panics {
		if v != nil {
			panic(v)
		}
	}
}

// carry copies the reason of src's field from, if any, to dst's field to.
func carry(dst *spec.Unavailable, to string, src spec.Unavailable, from string) {
	if why, ok := src[from]; ok {
		setWhy(dst, to, why)
	}
}

// summaryIdentity is the identity-basic collector: the identity op's
// hostname, OS, kernel release, architecture, init system, boot time and
// uptime, read the same way (collectIdentity), without DMI,
// virtualization, time zone or the clock command.
func (p *Provider) summaryIdentity() spec.SummaryIdentity {
	var out spec.SummaryIdentity
	un := &out.Unavailable

	if v, why := p.readString(pathHostname); why != "" {
		setWhy(un, "hostname", why)
	} else if v == "" {
		setWhy(un, "hostname", prefixUnrecognized+displayPath(pathHostname))
	} else {
		out.Hostname = &v
	}

	out.OS = p.osRelease()
	out.InitSystem = string(p.plat.Init)

	if v, why := p.readString(pathOSRelease); why != "" {
		setWhy(un, "kernelRelease", why)
	} else if v == "" {
		setWhy(un, "kernelRelease", prefixUnrecognized+displayPath(pathOSRelease))
	} else {
		out.KernelRelease = &v
	}

	if m, err := p.uname(); err != nil {
		var unsupported *unsupportedError
		if errors.As(err, &unsupported) {
			setWhy(un, "architecture", reasonFor("uname", err))
		} else {
			setWhy(un, "architecture", prefixNotPresent+"uname(2) machine")
		}
	} else if m == "" {
		setWhy(un, "architecture", prefixUnrecognized+"uname(2) machine")
	} else {
		out.Architecture = &m
	}

	if b, err := p.src.read(pathStat); err != nil {
		setWhy(un, "bootTime", reasonFor(pathStat, err))
	} else if st, err := hostparse.ProcStat(b); err != nil || !st.HasBootTime || st.BootTime > 1<<62 {
		setWhy(un, "bootTime", prefixUnrecognized+displayPath(pathStat))
	} else {
		bt := time.Unix(int64(st.BootTime), 0).UTC()
		out.BootTime = &bt
	}

	if b, err := p.src.read(pathUptime); err != nil {
		setWhy(un, "uptimeSeconds", reasonFor(pathUptime, err))
	} else if up, err := hostparse.Uptime(b); err != nil {
		setWhy(un, "uptimeSeconds", reasonFor(pathUptime, err))
	} else {
		out.UptimeSeconds = &up
	}
	return out
}

// summaryCPU is the CPU-basic collector: the CPU op's model, logical CPUs,
// usage and load, read the same way (collectCPU), without topology,
// frequencies or sensors.
func (p *Provider) summaryCPU(snap *snapshot, now time.Time) spec.SummaryCPU {
	var out spec.SummaryCPU
	un := &out.Unavailable

	if b, err := p.src.read(pathCPUInfo); err != nil {
		setWhy(un, "model", reasonFor(pathCPUInfo, err))
	} else if ci, err := hostparse.Cpuinfo(b); err != nil {
		setWhy(un, "model", reasonFor(pathCPUInfo, err))
	} else if ci.Model == "" {
		setWhy(un, "model", reasonNoModel)
	} else {
		m := ci.Model
		out.Model = &m
	}

	if b, err := p.src.read(pathStat); err != nil {
		setWhy(un, "logicalCpus", reasonFor(pathStat, err))
	} else if st, err := hostparse.ProcStat(b); err != nil {
		setWhy(un, "logicalCpus", reasonFor(pathStat, err))
	} else {
		n := len(st.CPUs)
		out.LogicalCPUs = &n
	}

	var rates cpuRates
	if snap != nil {
		rates = snap.cpu
	}
	usage, why := snap.val(now, rates.usage)
	out.UsagePercent = usage
	if why != "" {
		setWhy(un, "usagePercent", why)
	}
	out.Load = p.load()
	return out
}

// projectMemory is the summary's subset of the memory op.
func projectMemory(m spec.MemoryProperties) spec.SummaryMemory {
	out := spec.SummaryMemory{TotalBytes: m.TotalBytes, AvailableBytes: m.AvailableBytes, UsedPercent: m.UsedPercent,
		SwapTotalBytes: m.SwapTotalBytes, SwapUsedBytes: m.SwapUsedBytes}
	for _, k := range []string{"totalBytes", "availableBytes", "usedPercent", "swapTotalBytes", "swapUsedBytes"} {
		carry(&out.Unavailable, k, m.Unavailable, k)
	}
	return out
}

// summaryFilesystems is the filesystems op's list, sorted by mount point,
// or the reason there is none: only a host without any readable mountinfo
// has none, and then the host file's reason is the summary's.
func (p *Provider) summaryFilesystems(ctx context.Context) ([]spec.FilesystemProperties, string) {
	fs, err := p.collectFilesystems(ctx) // sorted by mount point
	if err != nil {
		t, _ := p.readMountTable()
		return nil, t.hostWhy
	}
	return fs, ""
}

// projectFilesystem is one filesystem's summary entry.
func projectFilesystem(f spec.FilesystemProperties) spec.SummaryFilesystem {
	out := spec.SummaryFilesystem{MountPoint: f.MountPoint, SizeBytes: f.SizeBytes, UsedPercent: f.UsedPercent}
	carry(&out.Unavailable, "sizeBytes", f.Unavailable, "sizeBytes")
	carry(&out.Unavailable, "usedPercent", f.Unavailable, "usedPercent")
	return out
}

// summaryInterfaces is the interfaces op's list with, for each, whether it
// has sys/class/net/<if>/device (a physical NIC), or the reason there is
// none. The stat is kernel memory, as the sampler's (readIface).
func (p *Provider) summaryInterfaces(ctx context.Context, snap *snapshot) ([]ifaceItem, map[string]bool, string) {
	items, err := p.collectInterfaces(ctx, snap)
	if err != nil {
		var pe *problem.Error
		if errors.As(err, &pe) {
			return nil, nil, pe.Detail // reasonFor(netClassDir, …)
		}
		return nil, nil, prefixNotPresent + displayPath(netClassDir)
	}
	devices := make(map[string]bool, len(items))
	for _, it := range items {
		if name, err := segPath(segName, netClassDir, it.name, "device"); err == nil {
			_, err := p.src.stat(name)
			devices[it.name] = err == nil
		}
	}
	return items, devices, ""
}

// projectNetwork is A-23: the non-loopback interfaces (an interface whose
// type cannot be read may not be loopback, so it is kept), devices first,
// then by name, cut at spec.SummaryMaxInterfaces with the total.
func projectNetwork(items []ifaceItem, devices map[string]bool) spec.SummaryNetwork {
	kept := make([]ifaceItem, 0, len(items))
	for _, it := range items {
		if lb := it.props.Loopback; lb == nil || !*lb {
			kept = append(kept, it)
		}
	}
	slices.SortStableFunc(kept, func(a, b ifaceItem) int {
		if da, db := devices[a.name], devices[b.name]; da != db {
			if da {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.name, b.name)
	})
	out := spec.SummaryNetwork{
		InterfacesTotal:     len(kept),
		InterfacesTruncated: len(kept) > spec.SummaryMaxInterfaces,
		Interfaces:          make(spec.Array[spec.SummaryInterface], 0, min(len(kept), spec.SummaryMaxInterfaces)),
	}
	for _, it := range kept[:min(len(kept), spec.SummaryMaxInterfaces)] {
		out.Interfaces = append(out.Interfaces, projectInterface(it))
	}
	return out
}

// projectInterface is one interface's summary entry: its state and rates
// as the interfaces op has them, and its primary addresses (A-24).
func projectInterface(it ifaceItem) spec.SummaryInterface {
	ip := it.props
	out := spec.SummaryInterface{Name: it.name, State: ip.State, RxBytesPerSecond: ip.RxBytesPerSecond,
		TxBytesPerSecond: ip.TxBytesPerSecond}
	for _, k := range []string{"state", "rxBytesPerSecond", "txBytesPerSecond"} {
		carry(&out.Unavailable, k, ip.Unavailable, k)
	}
	for _, f := range []struct {
		json    string
		dst     **string
		primary func(spec.Array[spec.Address]) (*string, string)
		addrs   spec.Array[spec.Address]
	}{
		{"ipv4", &out.IPv4, primaryIPv4, ip.IPv4},
		{"ipv6", &out.IPv6, primaryIPv6, ip.IPv6},
	} {
		if why, ok := ip.Unavailable[f.json]; ok {
			setWhy(&out.Unavailable, f.json, why)
			continue
		}
		v, why := f.primary(f.addrs)
		if why != "" {
			setWhy(&out.Unavailable, f.json, why)
			continue
		}
		*f.dst = v
	}
	return out
}

// addrString is an address with its prefix length, e.g. 192.0.2.10/24.
func addrString(a spec.Address) *string {
	s := a.Address + "/" + strconv.Itoa(a.PrefixLength)
	return &s
}

// primaryIPv4 is the first IPv4 address in the kernel's order (A-24).
func primaryIPv4(addrs spec.Array[spec.Address]) (*string, string) {
	if len(addrs) == 0 {
		return nil, reasonNotReported("no IPv4 address")
	}
	return addrString(addrs[0]), ""
}

// primaryIPv6 is A-24: the first global unicast address in the kernel's
// order, else the first unique-local (fc00::/7), else the first
// link-local (fe80::/10). Any other kind (deprecated site-local, a
// multicast or loopback address) is never primary.
func primaryIPv6(addrs spec.Array[spec.Address]) (*string, string) {
	if len(addrs) == 0 {
		return nil, reasonNotReported("no IPv6 address")
	}
	const (
		global = iota
		uniqueLocal
		linkLocal
		none
	)
	classOf := func(a spec.Address) int {
		ip, err := netip.ParseAddr(a.Address)
		switch {
		case err != nil || !ip.Is6() || ip.Is4In6():
			return none
		case ip.IsLinkLocalUnicast():
			return linkLocal
		case ip.IsPrivate():
			return uniqueLocal
		case ip.IsGlobalUnicast() && !isSiteLocal(ip):
			return global
		}
		return none
	}
	best, bestClass := -1, none
	for i, a := range addrs {
		if c := classOf(a); c < bestClass {
			best, bestClass = i, c
		}
	}
	if best < 0 {
		return nil, reasonNotReported("no global, unique-local or link-local IPv6 address")
	}
	return addrString(addrs[best]), ""
}

// siteLocal is fec0::/10, deprecated by RFC 3879, which netip still counts
// as global unicast.
var siteLocal = netip.MustParsePrefix("fec0::/10")

func isSiteLocal(ip netip.Addr) bool { return siteLocal.Contains(ip) }

// summaryHealth is the health-flags collector: the health op's reboot
// state, failed-services count and OOM kills, read the same way
// (collectHealth), without the process lists. The failed-services command
// is bounded by sourceTimeout (D-7); the rest is kernel memory and markers.
func (p *Provider) summaryHealth(ctx context.Context) spec.SummaryHealth {
	var out spec.SummaryHealth
	rr := p.rebootRequired()
	out.RebootRequired = rr.State
	carry(&out.Unavailable, "rebootRequired", rr.Unavailable, "state")
	failed := p.failedServices(ctx)
	out.FailedServicesCount = failed.Count
	carry(&out.Unavailable, "failedServicesCount", failed.Unavailable, "count")
	if n, why := p.oomKills(); why != "" {
		setWhy(&out.Unavailable, "oomKills", why)
	} else {
		out.OOMKills = &n
	}
	return out
}
