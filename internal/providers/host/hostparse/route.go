package hostparse

import (
	"encoding/hex"
	"errors"
	"math"
	"net/netip"
	"strconv"
	"strings"
)

// rtfGateway is RTF_GATEWAY (include/uapi/linux/route.h), set in both
// route files' flags column when the route has a gateway.
const rtfGateway = 0x0002

// DefaultRoute is a default route with a gateway, in file order.
type DefaultRoute struct {
	Gateway   netip.Addr
	Interface string
	Metric    uint32
}

func parseHex32(s string) (uint32, bool) {
	if len(s) != 8 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	return uint32(v), err == nil
}

// RouteV4 returns the default routes of /proc/net/route: destination and
// mask 0 with RTF_GATEWAY set. The kernel prints the network-order address
// as a host integer (%08X, net/ipv4/fib_trie.c fib_route_seq_show), so on
// the little-endian hosts nexusd runs on (amd64, arm64) the hex is
// byte-reversed: 010011AC is 172.17.0.1. The Metric column is a u32 printed
// with %d, so a priority over 2^31-1 shows as negative and is read back as
// its u32 value. The Iface header line is the required anchor.
func RouteV4(b []byte) ([]DefaultRoute, error) {
	var out []DefaultRoute
	header := false
	for l := range lines(b) {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if f[0] == "Iface" {
			header = true
			continue
		}
		if len(f) < 8 {
			continue
		}
		dst, ok1 := parseHex32(f[1])
		gw, ok2 := parseHex32(f[2])
		flags, err := strconv.ParseUint(f[3], 16, 32)
		mask, ok3 := parseHex32(f[7])
		metric, ok4 := parseMetric(f[6])
		if !ok1 || !ok2 || err != nil || !ok3 || !ok4 || f[0] == "*" {
			continue
		}
		if dst != 0 || mask != 0 || flags&rtfGateway == 0 || gw == 0 {
			continue
		}
		addr := netip.AddrFrom4([4]byte{byte(gw), byte(gw >> 8), byte(gw >> 16), byte(gw >> 24)})
		out = append(out, DefaultRoute{Gateway: addr, Interface: f[0], Metric: metric})
	}
	if !header {
		return nil, errors.New("proc/net/route: no header line")
	}
	return out, nil
}

func parseMetric(s string) (uint32, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < math.MinInt32 || v > math.MaxUint32 {
		return 0, false
	}
	if v < 0 {
		return uint32(int32(v)), true
	}
	return uint32(v), true
}

// RouteV6 returns the default routes of /proc/net/ipv6_route: destination
// ::/0 with RTF_GATEWAY set. A line is destination (32 hex), prefix length
// (2 hex), source and its length, next hop (32 hex), metric, refcount, use
// and flags (8 hex each) and the device (net/ipv6/ip6_fib.c
// ipv6_route_native_seq_show). The file has no header, so any well-formed
// line is the required anchor.
func RouteV6(b []byte) ([]DefaultRoute, error) {
	var out []DefaultRoute
	anchored := false
	for l := range lines(b) {
		f := strings.Fields(l)
		if len(f) < 10 {
			continue
		}
		dst, ok1 := parseHex128(f[0])
		plen, err := strconv.ParseUint(f[1], 16, 8)
		gw, ok2 := parseHex128(f[4])
		metric, ok3 := parseHex32(f[5])
		flags, ok4 := parseHex32(f[8])
		if !ok1 || err != nil || len(f[1]) != 2 || !ok2 || !ok3 || !ok4 {
			continue
		}
		anchored = true
		if !dst.IsUnspecified() || plen != 0 || flags&rtfGateway == 0 || gw.IsUnspecified() {
			continue
		}
		out = append(out, DefaultRoute{Gateway: gw, Interface: f[9], Metric: metric})
	}
	if !anchored {
		return nil, errors.New("proc/net/ipv6_route: no route line")
	}
	return out, nil
}

func parseHex128(s string) (netip.Addr, bool) {
	if len(s) != 32 {
		return netip.Addr{}, false
	}
	var a [16]byte
	if _, err := hex.Decode(a[:], []byte(s)); err != nil {
		return netip.Addr{}, false
	}
	return netip.AddrFrom16(a), true
}
