package host

import (
	"net"
	"net/netip"
)

// defaultAddrs is the Addrs seam: every interface's addresses as prefixes,
// keyed by interface name, each list in the order the kernel reports it
// (netlink on Linux).
func defaultAddrs() (map[string][]netip.Prefix, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]netip.Prefix, len(ifs))
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			return nil, err
		}
		prefixes := make([]netip.Prefix, 0, len(addrs))
		for _, a := range addrs {
			if p, ok := ipNetPrefix(a); ok {
				prefixes = append(prefixes, p)
			}
		}
		out[ifc.Name] = prefixes
	}
	return out, nil
}

// ipNetPrefix converts a *net.IPNet. An IPv4 address held in 16-byte form
// keeps its 4-byte mask, so it is unmapped to IPv4 by the mask's length.
func ipNetPrefix(a net.Addr) (netip.Prefix, bool) {
	ipn, ok := a.(*net.IPNet)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, bits := ipn.Mask.Size()
	addr, ok := netip.AddrFromSlice(ipn.IP)
	if !ok || bits == 0 {
		return netip.Prefix{}, false
	}
	if bits == 32 {
		addr = addr.Unmap()
	}
	if addr.BitLen() != bits {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr, ones), true
}
