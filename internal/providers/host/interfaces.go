// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"io/fs"
	"net/netip"
	"slices"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// netClassDir lists the network interfaces, as symlinks into
// /sys/devices.
const netClassDir = "sys/class/net"

// arphrdLoopback is ARPHRD_LOOPBACK (include/uapi/linux/if_arp.h), the
// sys/class/net/<if>/type of a loopback interface.
const arphrdLoopback = 772

// addrsWhat names the Addrs seam (netlink RTM_GETADDR on Linux) in a
// reason. It is not a path, so it is written as it is.
const addrsWhat = "netlink"

// counterFields are the JSON names of the fields /proc/net/dev fills.
var counterFields = []string{"rxBytes", "txBytes", "rxErrors", "txErrors", "rxDropped", "txDropped"}

// ifaceItem is one interface with its name.
type ifaceItem struct {
	name  string
	props spec.InterfaceProperties
}

func ifaceItemName(i ifaceItem) string { return i.name }

// ifaceNameKey is keyOfName for the interfaces list, sorted by name: only
// a name segPath accepts is one this list issues.
func ifaceNameKey(name string) (string, bool) { return name, validName(name) }

// interfaces is host.interfaces.list.
func (p *Provider) interfaces(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Interface], error) {
	after, err := decodeCursor(in.Cursor, ifaceNameKey)
	if err != nil {
		return apitypes.List[spec.Interface]{}, err
	}
	all, err := p.collectInterfaces(ctx, p.run.Load().snapshot())
	if err != nil {
		return apitypes.List[spec.Interface]{}, err
	}
	page, next := paginate(all, in.Limit, after, ifaceItemName, ifaceItemName)
	out := apitypes.List[spec.Interface]{Items: make([]spec.Interface, 0, len(page)), NextCursor: next}
	for _, i := range page {
		out.Items = append(out.Items, apitypes.NewResource(p.host, spec.Name, spec.InterfacesType, i.name, i.props))
	}
	return out, nil
}

// ifaceInputs are the per-request sources every interface shares, each
// read once: /proc/net/dev (live, not the sampler's copy) and the Addrs
// seam, the only netlink use. No neighbour table is ever read (NFR-6).
type ifaceInputs struct {
	now         time.Time
	snap        *snapshot
	counters    map[string]hostparse.NetDevStat
	countersWhy string
	addrs       map[string][]netip.Prefix
	addrsWhy    string
}

// collectInterfaces lists sys/class/net, sorted by name (WI 4.3, FR-9).
// The class directory holds symlinks, so entries are not filtered on
// IsDir; a regular file there (the bonding driver's bonding_masters) is not
// an interface. Each item's rates come from snap (nil when the sampler is
// not running), joined on its name. Only an unlistable sys/class/net fails
// the list; anything else leaves its own field null with the reason.
func (p *Provider) collectInterfaces(ctx context.Context, snap *snapshot) ([]ifaceItem, error) {
	entries, err := p.src.readDir(netClassDir)
	if err != nil {
		return nil, problem.New(problem.DependencyUnavailable, reasonFor(netClassDir, err)).With("provider", spec.Name)
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			continue
		}
		if _, err := segPath(segName, netClassDir, e.Name()); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	slices.Sort(names)

	in := ifaceInputs{now: p.now(), snap: snap}
	if b, err := p.src.read(pathNetDev); err != nil {
		in.countersWhy = reasonFor(pathNetDev, err)
	} else if nd, err := hostparse.NetDev(b); err != nil {
		in.countersWhy = reasonFor(pathNetDev, err)
	} else {
		in.counters = make(map[string]hostparse.NetDevStat, len(nd))
		for _, n := range nd {
			in.counters[n.Name] = n
		}
	}
	if in.addrs, err = p.addrs(); err != nil {
		in.addrsWhy = addrsReason(err)
	}

	speeds := p.linkSpeeds(ctx, names)
	out := make([]ifaceItem, len(names))
	for i, name := range names {
		out[i] = ifaceItem{name: name, props: p.collectInterface(name, in, speeds[i])}
	}
	return out, nil
}

// addrsReason classifies a failure of the Addrs seam. Its error text is
// never part of the reason.
func addrsReason(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return prefixPermission + addrsWhat
	}
	return prefixNotPresent + addrsWhat
}

// speedRead is one interface's link speed, or the reason there is none.
type speedRead struct {
	mbps int
	why  string
}

// linkSpeeds reads each interface's speed under bounded, all at once, so
// a driver stuck in get_link_ksettings holds up no other interface for
// longer than one sourceTimeout (D-7). A panic in a read is raised again
// here, in the request's goroutine.
func (p *Provider) linkSpeeds(ctx context.Context, names []string) []speedRead {
	out := make([]speedRead, len(names))
	fanOut(len(names), func(i int) { out[i] = p.linkSpeed(ctx, names[i]) })
	return out
}

// linkSpeed reads sys/class/net/<if>/speed. The kernel reports a link
// without a speed, virtual or down, as -1 or by failing the read with
// EINVAL (hostparse.LinkSpeed), and the architecture's table puts that
// under "not reported:"; a denied, timed-out or still-pending read keeps
// its own reason.
func (p *Provider) linkSpeed(ctx context.Context, name string) speedRead {
	path, _ := segPath(segName, netClassDir, name, "speed") // name passed segPath
	noSpeed := speedRead{why: reasonNotReported("no link speed at " + displayPath(path))}
	b, err := p.src.readBounded(ctx, path)
	if err != nil {
		var timeout *timeoutError
		var pending *pendingError
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, errNotAllowed) || errors.As(err, &timeout) ||
			errors.As(err, &pending) {
			return speedRead{why: reasonFor(path, err)}
		}
		return noSpeed
	}
	v, ok := hostparse.LinkSpeed(b)
	if !ok {
		return noSpeed
	}
	return speedRead{mbps: int(v)}
}

// collectInterface reads one interface. name has passed segPath.
func (p *Provider) collectInterface(name string, in ifaceInputs, speed speedRead) spec.InterfaceProperties {
	out := spec.InterfaceProperties{Sampler: in.snap.sampler(in.now)}
	un := &out.Unavailable
	attr := func(leaf string) string {
		n, _ := segPath(segName, netClassDir, name, leaf) // name is valid; leaf is fixed
		return n
	}

	if t, why := readAttr(p.src, attr("type"), hostparse.ScalarInt); why != "" {
		setWhy(un, "loopback", why)
	} else {
		out.Loopback = ptr(t == arphrdLoopback)
	}

	if b, err := p.src.read(attr("operstate")); err != nil {
		out.State = unknownState
		setWhy(un, "state", reasonFor(attr("operstate"), err))
	} else {
		out.State = hostparse.Operstate(b)
	}

	// The interface's own link-layer address. An interface without one
	// (tun, wireguard) prints an empty line.
	if b, err := p.src.read(attr("address")); err != nil {
		setWhy(un, "mac", reasonFor(attr("address"), err))
	} else if mac := hostparse.ScalarString(b); mac == "" {
		setWhy(un, "mac", reasonNotReported("no link-layer address"))
	} else {
		out.MAC = &mac
	}

	if v, why := readAttr(p.src, attr("mtu"), hostparse.ScalarInt); why != "" {
		setWhy(un, "mtu", why)
	} else {
		out.MTU = ptr(int(v))
	}

	if speed.why != "" {
		setWhy(un, "speedMbps", speed.why)
	} else {
		out.SpeedMbps = ptr(speed.mbps)
	}

	if in.addrsWhy != "" {
		setWhy(un, "ipv4", in.addrsWhy)
		setWhy(un, "ipv6", in.addrsWhy)
	} else {
		out.IPv4, out.IPv6 = splitAddrs(in.addrs[name])
	}

	ifaceCounters(&out, name, in)
	ifaceRateFields(&out, name, in.snap, in.now)
	return out
}

// splitAddrs splits an interface's prefixes by family, each in the
// kernel's order, as lists that marshal [] when empty.
func splitAddrs(prefixes []netip.Prefix) (v4, v6 spec.Array[spec.Address]) {
	v4, v6 = spec.Array[spec.Address]{}, spec.Array[spec.Address]{}
	for _, pf := range prefixes {
		a := spec.Address{Address: pf.Addr().String(), PrefixLength: pf.Bits()}
		if pf.Addr().Is4() {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	return v4, v6
}

// ifaceCounters fills the cumulative counters from the live
// /proc/net/dev.
func ifaceCounters(out *spec.InterfaceProperties, name string, in ifaceInputs) {
	why := in.countersWhy
	st, ok := in.counters[name]
	if why == "" && !ok {
		why = reasonNotReported("no /proc/net/dev line for the interface")
	}
	if why != "" {
		for _, k := range counterFields {
			setWhy(&out.Unavailable, k, why)
		}
		return
	}
	out.RxBytes, out.TxBytes = ptr(st.RxBytes), ptr(st.TxBytes)
	out.RxErrors, out.TxErrors = ptr(st.RxErrs), ptr(st.TxErrs)
	out.RxDropped, out.TxDropped = ptr(st.RxDrop), ptr(st.TxDrop)
}

// ifaceRateFields fills the sampler-derived rates: the snapshot's entry
// for name. An interface the sampler has not seen is warming up.
func ifaceRateFields(out *spec.InterfaceProperties, name string, snap *snapshot, now time.Time) {
	why := snap.blocked(now)
	var r ifaceRates
	if why == "" {
		switch rr, ok := snap.ifaces[name]; {
		case snap.ifaces == nil:
			why = snap.ifaceWhy
		case !ok:
			why = reasonWarmingUp
		default:
			r = rr
		}
	}
	for _, f := range []struct {
		json string
		dst  **float64
		val  rateVal
	}{
		{"rxBytesPerSecond", &out.RxBytesPerSecond, r.rx},
		{"txBytesPerSecond", &out.TxBytesPerSecond, r.tx},
	} {
		if why != "" {
			setWhy(&out.Unavailable, f.json, why)
			continue
		}
		v, w := snap.val(now, f.val)
		if v == nil {
			setWhy(&out.Unavailable, f.json, w)
			continue
		}
		*f.dst = v
	}
}
