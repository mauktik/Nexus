// Tests of planning/system-provider/test-cases.md TC-HP-87, TC-HP-88, TC-HP-89, TC-HP-E23, TC-HP-E24, TC-HP-E25, TC-HP-ERR23, TC-HP-ERR24.
// Owner WI: 4.3. Split from hostprov_handlers_tc_test.go by the orchestrator so
// that Phase 4 work items can run in parallel. The helpers they share are in
// interfaces_test.go and netconfig_test.go.

package host

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-HP-87: Interfaces on both captures agree with ip addr. Owner WI 4.3. Requirements: FR-9, NFR-3, NFR-6, FR-15.
//
// - Setup: host stack per capture, ready snapshot.
// - Steps: interfaces handler.
// - Expected:
//   - names equal `tools/ip-addr`'s; `lo` has `loopback true` (type 772), others false;
//   - `state` = `Operstate(operstate)`; `mac` = `address`; `mtu` = `mtu`; `ipv4`/`ipv6` with prefix lengths equal ip addr's;
//   - `rxBytes`, `txBytes`, errors and drops equal `fs/proc/net/dev` (read live);
//   - rates equal TC-HP-63's; `assertExplicit` passes.
//
// After the ready snapshot F-FS serves the t1/ sample of proc/net/dev, so
// "read live" is checked against what F-FS serves at the call (t1/), which
// differs from the snapshot's first sample.
func TestTCHP87_InterfacesOnBothCapturesAgreeWithIp(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newHostStack(t, capture)
			s.ready()
			items := s.ifaceList() // assertExplicit runs in decodeList
			ip := ipAddrTable(t, capture)
			var want []string
			for _, ifc := range ip {
				want = append(want, ifc.IfName)
			}
			slices.Sort(want)
			if got := ifaceNames(items); !slices.Equal(got, want) {
				t.Fatalf("names %v, want ip addr's %v", got, want)
			}
			live := netDevByName(t, s.ffs.rawRead(pathNetDev))
			if string(s.ffs.rawRead(pathNetDev)) != string(capFile(t, capture, "t1", pathNetDev)) {
				t.Fatal("F-FS does not serve t1/proc/net/dev after the ready snapshot")
			}
			prev, cur := netCols(t, capFile(t, capture, "fs", pathNetDev)), netCols(t, capFile(t, capture, "t1", pathNetDev))
			for _, ifc := range ip {
				p := ifaceAt(t, items, ifc.IfName)
				name := ifc.IfName
				if wantLo := sysAttr(t, s, name, "type") == "772"; p.Loopback == nil || *p.Loopback != wantLo ||
					wantLo != (ifc.LinkType == "loopback") {
					t.Errorf("%s loopback = %v, want %v (type 772; ip link_type %s)", name, p.Loopback, wantLo, ifc.LinkType)
				}
				if st := hostparse.Operstate(s.ffs.rawRead("sys/class/net/" + name + "/operstate")); p.State != st ||
					p.State != strings.ToLower(ifc.Operstate) {
					t.Errorf("%s state %q, want Operstate %q and ip's %q", name, p.State, st, ifc.Operstate)
				}
				if p.MAC == nil || *p.MAC != sysAttr(t, s, name, "address") || *p.MAC != ifc.Address {
					t.Errorf("%s mac %v, want the address attribute and ip's %s", name, p.MAC, ifc.Address)
				}
				if p.MTU == nil || strconv.Itoa(*p.MTU) != sysAttr(t, s, name, "mtu") || *p.MTU != ifc.MTU {
					t.Errorf("%s mtu %v, want the mtu attribute and ip's %d", name, p.MTU, ifc.MTU)
				}
				if got, w := p.IPv4, ipAddrs(ifc, "inet"); !slices.EqualFunc(got, w, addrEq) {
					t.Errorf("%s ipv4 %+v, want ip addr's %+v", name, got, w)
				}
				if got, w := p.IPv6, ipAddrs(ifc, "inet6"); !slices.EqualFunc(got, w, addrEq) {
					t.Errorf("%s ipv6 %+v, want ip addr's %+v", name, got, w)
				}
				wantCounters(t, name, p, live[name])
				if p.Sampler.State != spec.SamplerReady {
					t.Errorf("%s sampler state %q, want ready", name, p.Sampler.State)
				}
				wrx, wtx := float64(cur[name][0]-prev[name][0])/10, float64(cur[name][1]-prev[name][1])/10
				if p.RxBytesPerSecond == nil || p.TxBytesPerSecond == nil || !near(*p.RxBytesPerSecond, wrx) ||
					!near(*p.TxBytesPerSecond, wtx) {
					t.Errorf("%s rx/tx per second %v/%v (%v), want %v/%v", name, p.RxBytesPerSecond, p.TxBytesPerSecond,
						p.Unavailable, wrx, wtx)
				}
			}
		})
	}
}

func addrEq(a, b spec.Address) bool {
	return a.Address == b.Address && a.PrefixLength == b.PrefixLength && len(a.Unavailable) == 0 && len(b.Unavailable) == 0
}

// TC-HP-88: Default gateways equal ip route. Owner WI 4.3. Requirements: FR-9.
//
// - Setup: host stack per capture.
// - Steps: netconfig handler.
// - Expected: one item `current`; `defaultGateways` = the `tools/ip-route` defaults with `family` (`ipv4`/`ipv6`), `address`, `interface`, `metric`, sorted by metric.
func TestTCHP88_DefaultGatewaysEqualIpRoute(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newHostStack(t, capture)
			nc := s.netconfig() // oneItem checks the one item current and assertExplicit
			want := ipRouteDefaults(t, capture)
			if len(want) == 0 {
				t.Fatal("tools/ip-route has no default route; the capture cannot test this")
			}
			slices.SortStableFunc(want, func(a, b spec.Gateway) int { return a.Metric - b.Metric })
			if !slices.EqualFunc(nc.DefaultGateways, want, gatewayEq) {
				t.Errorf("defaultGateways = %+v, want ip route's %+v", nc.DefaultGateways, want)
			}
		})
	}
}

// TC-HP-89: DNS resolvers, with and without systemd-resolved. Owner WI 4.3. Requirements: FR-9.
//
// - Setup: UBU (resolved in use); ALP (plain).
// - Steps: netconfig handler.
// - Expected: UBU: `dns.source` = `/run/systemd/resolve/resolv.conf`; `servers` = that file's nameservers with `stub false`, plus `127.0.0.53` with `stub true`. ALP: `source` `/etc/resolv.conf`, servers in file order, `stub` true only for loopback addresses.
//
// The UBU capture ran in a container without resolved, so its resolv.conf
// files are replaced with the shape resolved writes: the stub file in /etc
// and the upstreams in /run.
func TestTCHP89_DNSResolversWithAndWithoutSystemdResolved(t *testing.T) {
	t.Run("UBU resolved in use", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		s.ffs.replace(pathResolvConf, []byte("# This is /run/systemd/resolve/stub-resolv.conf managed by man:systemd-resolved(8).\n"+
			"nameserver 127.0.0.53\noptions edns0 trust-ad\nsearch .\n"))
		s.ffs.replace(pathResolvedConf, []byte("# This is /run/systemd/resolve/resolv.conf managed by man:systemd-resolved(8).\n"+
			"nameserver 192.168.5.1\nnameserver 2001:db8::53\nsearch lan\n"))
		dns := s.netconfig().DNS
		want := []spec.DNSServer{{Address: "192.168.5.1"}, {Address: "2001:db8::53"}, {Address: "127.0.0.53", Stub: true}}
		if dns.Source == nil || *dns.Source != "/run/systemd/resolve/resolv.conf" {
			t.Errorf("source %v, want /run/systemd/resolve/resolv.conf", dns.Source)
		}
		if !slices.EqualFunc(dns.Servers, want, dnsEq) {
			t.Errorf("servers %+v, want %+v", dns.Servers, want)
		}
	})
	t.Run("ALP plain", func(t *testing.T) {
		s := newHostStack(t, capALP)
		dns := s.netconfig().DNS
		if dns.Source == nil || *dns.Source != "/etc/resolv.conf" {
			t.Errorf("source %v, want /etc/resolv.conf", dns.Source)
		}
		var want []spec.DNSServer
		for _, a := range hostparse.ResolvConf(s.ffs.rawRead(pathResolvConf)) {
			want = append(want, spec.DNSServer{Address: a.String(), Stub: a.IsLoopback()})
		}
		if len(want) == 0 || !slices.EqualFunc(dns.Servers, want, dnsEq) {
			t.Errorf("servers %+v, want the file's %+v", dns.Servers, want)
		}
		// Loopback servers among upstreams, in file order.
		s.ffs.replace(pathResolvConf, []byte("nameserver 192.168.5.1\nnameserver 127.0.0.1\n# nameserver 10.0.0.1\n"+
			"nameserver ::1\nnameserver 2001:db8::1\n"))
		dns = s.netconfig().DNS
		want = []spec.DNSServer{{Address: "192.168.5.1"}, {Address: "127.0.0.1", Stub: true}, {Address: "::1", Stub: true},
			{Address: "2001:db8::1"}}
		if !slices.EqualFunc(dns.Servers, want, dnsEq) {
			t.Errorf("servers %+v, want %+v", dns.Servers, want)
		}
	})
}

// TC-HP-E23: Interfaces are enumerated through symlink entries. Owner WI 4.3. Requirements: FR-9, CON-5.
//
// - Setup: F-FS(UBU) default shaping.
// - Steps: interfaces handler.
// - Expected: the same names as `tools/ip-addr`; zero interfaces is a failure.
func TestTCHPE23_InterfacesAreEnumeratedThroughSymlinkEntries(t *testing.T) {
	s := newHostStack(t, capUBU)
	entries, err := s.ffs.ReadDir("sys/class/net")
	if err != nil || len(entries) == 0 {
		t.Fatalf("F-FS sys/class/net: %v, %d entries", err, len(entries))
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("F-FS lists %s as a directory; the shaping under test is symlink entries", e.Name())
		}
	}
	got := ifaceNames(s.ifaceList())
	var want []string
	for _, ifc := range ipAddrTable(t, capUBU) {
		want = append(want, ifc.IfName)
	}
	slices.Sort(want)
	if len(got) == 0 || !slices.Equal(got, want) {
		t.Errorf("names %v, want ip addr's %v", got, want)
	}
}

// TC-HP-E24: A host with no default route. Owner WI 4.3. Requirements: FR-9, FR-12.
//
// - Setup: UBU with `proc/net/route` and `ipv6_route` reduced to their non-default lines.
// - Steps: netconfig handler.
// - Expected: `defaultGateways []`; `assertExplicit` passes (an empty list is a value; if listed in `unavailable`, its reason is `not reported:`).
func TestTCHPE24_AHostWithNoDefaultRoute(t *testing.T) {
	s := newHostStack(t, capUBU)
	var v4 []string
	for _, l := range strings.Split(string(s.ffs.rawRead(pathRouteV4)), "\n") {
		if f := strings.Fields(l); len(f) > 7 && f[1] == "00000000" && f[7] == "00000000" {
			continue
		}
		v4 = append(v4, l)
	}
	var v6 []string
	for _, l := range strings.Split(string(s.ffs.rawRead(pathRouteV6)), "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[0] == strings.Repeat("0", 32) && f[1] == "00" {
			continue
		}
		v6 = append(v6, l)
	}
	if len(v4) < 2 || len(v6) < 1 {
		t.Fatalf("reduced tables keep %d and %d lines; want the header and the non-default routes", len(v4), len(v6))
	}
	s.ffs.replace(pathRouteV4, []byte(strings.Join(v4, "\n")))
	s.ffs.replace(pathRouteV6, []byte(strings.Join(v6, "\n")))
	nc := s.netconfig() // assertExplicit runs in oneItem
	if nc.DefaultGateways == nil || len(nc.DefaultGateways) != 0 {
		t.Errorf("defaultGateways = %#v, want []", nc.DefaultGateways)
	}
	if why, ok := nc.Unavailable["defaultGateways"]; ok && !strings.HasPrefix(why, prefixNotReported) {
		t.Errorf("defaultGateways listed in unavailable with %q, want no reason or not reported:", why)
	}
}

// TC-HP-E25: Neighbour tables are never read. Owner WI 4.3. Requirements: NFR-6.
//
// - Setup: UBU with F-FS recording; F-ADDRS counting calls.
// - Steps: interfaces and netconfig handlers.
// - Expected: no recorded path under `proc/net/` other than `dev`, `route`, `ipv6_route`; the only netlink use is F-ADDRS (called once per request); `mac` fields are each interface's own `address` only.
func TestTCHPE25_NeighbourTablesAreNeverRead(t *testing.T) {
	s := newHostStack(t, capUBU)
	n := countAddrs(s)
	s.ffs.resetRecorder()
	items := s.ifaceList()
	if *n != 1 {
		t.Errorf("interfaces: F-ADDRS called %d times, want 1", *n)
	}
	s.netconfig()
	if *n != 1 {
		t.Errorf("netconfig: F-ADDRS called %d more times, want 0", *n-1)
	}
	allowed := []string{"proc/net/dev", "proc/net/route", "proc/net/ipv6_route"}
	for _, c := range s.ffs.recorded() {
		if strings.HasPrefix(c.name, "proc/net/") && !slices.Contains(allowed, c.name) {
			t.Errorf("recorded %s", c)
		}
		if strings.HasPrefix(c.name, "sys/class/net/") && strings.Count(c.name, "/") > 4 {
			t.Errorf("recorded %s, below an interface's own attributes", c)
		}
	}
	for _, it := range items {
		if it.Properties.MAC == nil || *it.Properties.MAC != sysAttr(t, s, it.Name, "address") {
			t.Errorf("%s mac %v, want its own address attribute", it.Name, it.Properties.MAC)
		}
	}
}

// TC-HP-ERR23: A down or virtual link has no speed. Owner WI 4.3. Requirements: FR-9, FR-12.
//
// - Setup: UBU with `einval("sys/class/net/<a down interface>/speed")`; a virtual interface without `device` whose speed reads `-1`.
// - Steps: interfaces handler.
// - Expected: both `speedMbps` null with a `not reported:` reason; the call does not fail; other fields populated.
//
// docker0 is the capture's down interface; eth0 is a veth (no device link)
// whose speed is set to -1.
func TestTCHPERR23_ADownOrVirtualLinkHasNo(t *testing.T) {
	s := newHostStack(t, capUBU)
	if sysAttr(t, s, "docker0", "operstate") != "down" {
		t.Fatal("docker0 is not down in the capture")
	}
	if _, err := s.ffs.Stat("sys/class/net/eth0/device"); err == nil {
		t.Fatal("eth0 has a device link in the capture; it is not virtual")
	}
	s.ffs.einval("sys/class/net/docker0/speed")
	s.ffs.replace("sys/class/net/eth0/speed", []byte("-1\n"))
	items := s.ifaceList() // the call does not fail; assertExplicit runs in decodeList
	for _, name := range []string{"docker0", "eth0"} {
		p := ifaceAt(t, items, name)
		if p.SpeedMbps != nil || !strings.HasPrefix(p.Unavailable["speedMbps"], prefixNotReported) {
			t.Errorf("%s speedMbps = %v with %q, want null with not reported:", name, p.SpeedMbps, p.Unavailable["speedMbps"])
		}
		if p.Loopback == nil || p.MAC == nil || p.MTU == nil || p.RxBytes == nil || len(p.IPv4) == 0 ||
			p.State == "" || len(p.Unavailable) != 3 { // speed and the two rates (no sampler)
			t.Errorf("%s = %+v; the other fields should be populated", name, p)
		}
	}
}

// TC-HP-ERR24: Unreadable route tables leave gateways explicitly unavailable. Owner WI 4.3. Requirements: FR-9, FR-12.
//
// - Setup: UBU with `deny("proc/net/route")` and `deny("proc/net/ipv6_route")`; separately `deny("etc/resolv.conf")`.
// - Steps: netconfig handler.
// - Expected: `defaultGateways []` listed in `unavailable` with `permission denied: /proc/net/route`; with resolv.conf denied: `dns.servers []` listed with `permission denied: /etc/resolv.conf`; no error.
func TestTCHPERR24_UnreadableRouteTablesLeaveGatewaysExplicitlyUnavailable(t *testing.T) {
	t.Run("route tables denied", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		s.ffs.deny(pathRouteV4)
		s.ffs.deny(pathRouteV6)
		nc := s.netconfig() // no error; assertExplicit runs in oneItem
		if nc.DefaultGateways == nil || len(nc.DefaultGateways) != 0 ||
			nc.Unavailable["defaultGateways"] != "permission denied: /proc/net/route" {
			t.Errorf("defaultGateways = %#v with %q, want [] with permission denied: /proc/net/route", nc.DefaultGateways,
				nc.Unavailable["defaultGateways"])
		}
		if len(nc.DNS.Servers) == 0 {
			t.Error("dns servers are empty; only the gateways should be unavailable")
		}
	})
	t.Run("resolv.conf denied", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		s.ffs.deny(pathResolvConf)
		nc := s.netconfig()
		if nc.DNS.Servers == nil || len(nc.DNS.Servers) != 0 ||
			nc.DNS.Unavailable["servers"] != "permission denied: /etc/resolv.conf" {
			t.Errorf("dns.servers = %#v with %q, want [] with permission denied: /etc/resolv.conf", nc.DNS.Servers,
				nc.DNS.Unavailable["servers"])
		}
		if len(nc.DefaultGateways) == 0 {
			t.Error("defaultGateways are empty; only the DNS servers should be unavailable")
		}
	})
}
