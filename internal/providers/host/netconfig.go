package host

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"slices"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// The routing tables and resolver files of host.netconfig.get (FR-9,
// A-25, A-30).
const (
	pathRouteV4      = "proc/net/route"
	pathRouteV6      = "proc/net/ipv6_route"
	pathResolvConf   = "etc/resolv.conf"
	pathResolvedConf = "run/systemd/resolve/resolv.conf"
)

// netconfig is host.netconfig.get.
func (p *Provider) netconfig(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.NetConfig], error) {
	return currentList(p, spec.NetConfigType, p.collectNetConfig(ctx)), nil
}

// collectNetConfig reads the default gateways and the DNS resolvers live.
// Every source is kernel memory or a small file, read without bounded.
func (p *Provider) collectNetConfig(_ context.Context) spec.NetConfigProperties {
	var out spec.NetConfigProperties
	if gws, why := p.defaultGateways(); why != "" {
		out.DefaultGateways = spec.Array[spec.Gateway]{}
		setWhy(&out.Unavailable, "defaultGateways", why)
	} else {
		out.DefaultGateways = gws
	}
	out.DNS = p.dns()
	return out
}

// defaultGateways is the default routes of proc/net/route and
// proc/net/ipv6_route, IPv4 first, then stably sorted by metric. A host
// without IPv6 (ipv6.disable=1) has no ipv6_route, and so no IPv6 default
// route. Any other failure of either table is the reason for the whole
// list: a list of one family only would read as complete.
func (p *Provider) defaultGateways() (spec.Array[spec.Gateway], string) {
	tables := []struct {
		path, family string
		parse        func([]byte) ([]hostparse.DefaultRoute, error)
		optional     bool
	}{
		{pathRouteV4, "ipv4", hostparse.RouteV4, false},
		{pathRouteV6, "ipv6", hostparse.RouteV6, true},
	}
	out := spec.Array[spec.Gateway]{}
	for _, t := range tables {
		b, err := p.src.read(t.path)
		if t.optional && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, reasonFor(t.path, err)
		}
		routes, err := t.parse(b)
		if err != nil {
			return nil, reasonFor(t.path, err)
		}
		for _, r := range routes {
			out = append(out, spec.Gateway{Family: t.family, Address: r.Gateway.String(), Interface: r.Interface,
				Metric: int(r.Metric)})
		}
	}
	slices.SortStableFunc(out, func(a, b spec.Gateway) int { return cmp.Compare(a.Metric, b.Metric) })
	return out, ""
}

// dns is the resolver configuration in effect (A-25). systemd-resolved is
// in use when every /etc/resolv.conf server is one of its stubs and its
// run file exists: the servers are then the run file's upstreams (stub
// false) followed by the stubs of /etc/resolv.conf (stub true), and the
// source is the run file. Otherwise the servers are /etc/resolv.conf's in
// file order, each a stub when it is a loopback address.
//
// An unreadable /etc/resolv.conf leaves both fields unavailable: which file
// is in effect cannot be told. A run file that exists but cannot be read
// under resolved's stubs leaves the servers [] with its reason.
func (p *Provider) dns() spec.DNS {
	var out spec.DNS
	etc, err := p.src.read(pathResolvConf)
	if err != nil {
		why := reasonFor(pathResolvConf, err)
		out.Servers = spec.Array[spec.DNSServer]{}
		setWhy(&out.Unavailable, "source", why)
		setWhy(&out.Unavailable, "servers", why)
		return out
	}
	etcServers := hostparse.ResolvConf(etc)
	// A present, empty run file is enough for ResolvedInUse to answer
	// whether every /etc server is one of resolved's stubs.
	stubsOnly := hostparse.ResolvedInUse(etc, []byte{})
	run, runErr := p.src.read(pathResolvedConf)

	switch {
	case stubsOnly && runErr == nil:
		out.Source = ptr(displayPath(pathResolvedConf))
		out.Servers = spec.Array[spec.DNSServer]{}
		for _, a := range hostparse.ResolvConf(run) {
			out.Servers = append(out.Servers, spec.DNSServer{Address: a.String()})
		}
		for _, a := range etcServers {
			out.Servers = append(out.Servers, spec.DNSServer{Address: a.String(), Stub: true})
		}
	case stubsOnly && !errors.Is(runErr, fs.ErrNotExist):
		out.Source = ptr(displayPath(pathResolvedConf))
		out.Servers = spec.Array[spec.DNSServer]{}
		setWhy(&out.Unavailable, "servers", reasonFor(pathResolvedConf, runErr))
	default:
		out.Source = ptr(displayPath(pathResolvConf))
		out.Servers = spec.Array[spec.DNSServer]{}
		for _, a := range etcServers {
			out.Servers = append(out.Servers, spec.DNSServer{Address: a.String(), Stub: hostparse.Stub(a)})
		}
	}
	return out
}
