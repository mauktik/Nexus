// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// netconfig is host.netconfig.get's one item, checked by oneItem
// (assertExplicit included).
func (s *hostStack) netconfig() spec.NetConfigProperties {
	s.t.Helper()
	out, err := s.p.netconfig(context.Background(), spec.NoInput{})
	return oneItem[spec.NetConfigProperties](s.t, spec.NetConfigType, out, err)
}

// ipRouteEntry is one route of the capture's `ip -j route`. ip omits the
// metric when it is 0.
type ipRouteEntry struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	Dev     string `json:"dev"`
	Metric  int    `json:"metric"`
}

// ipRouteDefaults is the default routes of tools/ip-route as gateways.
func ipRouteDefaults(t *testing.T, capture string) []spec.Gateway {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", capture, "tools", "ip-route"))
	if err != nil {
		t.Fatal(err)
	}
	var rs []ipRouteEntry
	if err := json.Unmarshal(b, &rs); err != nil {
		t.Fatal(err)
	}
	var out []spec.Gateway
	for _, r := range rs {
		if r.Dst == "default" && r.Gateway != "" {
			family := "ipv4"
			if strings.Contains(r.Gateway, ":") {
				family = "ipv6"
			}
			out = append(out, spec.Gateway{Family: family, Address: r.Gateway, Interface: r.Dev, Metric: r.Metric})
		}
	}
	return out
}

// The default ipv6_route line of a host with an IPv6 router: ::/0 via
// fe80::1 on eth0, metric 0x32 (50), flags RTF_UP|RTF_GATEWAY.
const v6DefaultLine = "00000000000000000000000000000000 00 00000000000000000000000000000000 00 " +
	"fe800000000000000000000000000001 00000032 00000001 00000000 00000003     eth0\n"

// Gateways of both families are sorted by metric, whatever the family.
func TestNetConfigGatewaysSortByMetricAcrossFamilies(t *testing.T) {
	s := newHostStack(t, capUBU)
	route := string(s.ffs.rawRead(pathRouteV4))
	route = strings.Replace(route, "eth0\t00000000\t010011AC\t0003\t0\t0\t0\t",
		"eth0\t00000000\t010011AC\t0003\t0\t0\t100\t", 1)
	s.ffs.replace(pathRouteV4, []byte(route))
	s.ffs.replace(pathRouteV6, append(s.ffs.rawRead(pathRouteV6), v6DefaultLine...))
	got := s.netconfig().DefaultGateways
	want := []spec.Gateway{
		{Family: "ipv6", Address: "fe80::1", Interface: "eth0", Metric: 50},
		{Family: "ipv4", Address: "172.17.0.1", Interface: "eth0", Metric: 100},
	}
	if !slices.EqualFunc(got, want, gatewayEq) {
		t.Errorf("defaultGateways = %+v, want %+v", got, want)
	}
}

func gatewayEq(a, b spec.Gateway) bool {
	return a.Family == b.Family && a.Address == b.Address && a.Interface == b.Interface && a.Metric == b.Metric &&
		len(a.Unavailable) == 0 && len(b.Unavailable) == 0
}

// A kernel booted with ipv6.disable=1 has no /proc/net/ipv6_route: it has
// no IPv6 routes, and the IPv4 gateways are still reported.
func TestNetConfigWithoutIPv6(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.remove(pathRouteV6)
	nc := s.netconfig()
	if !slices.EqualFunc(nc.DefaultGateways, ipRouteDefaults(t, capUBU), gatewayEq) || len(nc.Unavailable) != 0 {
		t.Errorf("defaultGateways = %+v with %v, want ip route's", nc.DefaultGateways, nc.Unavailable)
	}
}

// One family's table unreadable: the list is [] with that reason, never a
// partial list that reads as complete.
func TestNetConfigOneRouteTableUnreadable(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny(pathRouteV6)
	nc := s.netconfig()
	if len(nc.DefaultGateways) != 0 || nc.Unavailable["defaultGateways"] != "permission denied: /proc/net/ipv6_route" {
		t.Errorf("defaultGateways = %+v with %q", nc.DefaultGateways, nc.Unavailable["defaultGateways"])
	}
}

// A route file without its anchor is unrecognized.
func TestNetConfigUnrecognizedRouteTable(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace(pathRouteV4, []byte("garbage\n"))
	nc := s.netconfig()
	if nc.Unavailable["defaultGateways"] != "unrecognized format: /proc/net/route" {
		t.Errorf("defaultGateways reason %q", nc.Unavailable["defaultGateways"])
	}
}

// resolved's stubs in /etc/resolv.conf with the run file unreadable (not
// absent): resolved is in use (A-25: the file exists), so the source is the
// run file and its servers are [] with the reason.
func TestNetConfigResolvedRunFileDenied(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace(pathResolvConf, []byte("nameserver 127.0.0.53\n"))
	s.ffs.replace(pathResolvedConf, []byte("nameserver 9.9.9.9\n"))
	s.ffs.deny(pathResolvedConf)
	dns := s.netconfig().DNS
	if dns.Source == nil || *dns.Source != "/"+pathResolvedConf || len(dns.Servers) != 0 ||
		dns.Unavailable["servers"] != "permission denied: /run/systemd/resolve/resolv.conf" {
		t.Errorf("dns = source %v servers %+v unavailable %v", dns.Source, dns.Servers, dns.Unavailable)
	}
}

// resolved without upstreams: its run file lists no server, and the stub
// alone is reported.
func TestNetConfigResolvedWithoutUpstreams(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace(pathResolvConf, []byte("nameserver 127.0.0.53\noptions edns0 trust-ad\n"))
	s.ffs.replace(pathResolvedConf, []byte("# no servers\n"))
	dns := s.netconfig().DNS
	want := []spec.DNSServer{{Address: "127.0.0.53", Stub: true}}
	if dns.Source == nil || *dns.Source != "/"+pathResolvedConf || !slices.EqualFunc(dns.Servers, want, dnsEq) {
		t.Errorf("dns = source %v servers %+v, want the run file and the stub", dns.Source, dns.Servers)
	}
}

// A 127.0.0.53 server without the run file is a plain resolv.conf (resolved
// is not in use, A-25), with the loopback server marked stub.
func TestNetConfigStubWithoutRunFileIsPlain(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace(pathResolvConf, []byte("nameserver 127.0.0.53\n"))
	dns := s.netconfig().DNS
	want := []spec.DNSServer{{Address: "127.0.0.53", Stub: true}}
	if dns.Source == nil || *dns.Source != "/"+pathResolvConf || !slices.EqualFunc(dns.Servers, want, dnsEq) {
		t.Errorf("dns = source %v servers %+v", dns.Source, dns.Servers)
	}
}

// A resolv.conf with no nameserver line has no servers; [] is the value.
func TestNetConfigNoNameservers(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace(pathResolvConf, []byte("search lan\n"))
	dns := s.netconfig().DNS
	if dns.Source == nil || len(dns.Servers) != 0 || len(dns.Unavailable) != 0 {
		t.Errorf("dns = source %v servers %+v unavailable %v, want /etc/resolv.conf, [] and no reason", dns.Source,
			dns.Servers, dns.Unavailable)
	}
}

func dnsEq(a, b spec.DNSServer) bool {
	return a.Address == b.Address && a.Stub == b.Stub && len(a.Unavailable) == 0 && len(b.Unavailable) == 0
}
