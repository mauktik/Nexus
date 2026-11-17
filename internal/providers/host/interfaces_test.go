package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// ifaceList is host.interfaces.list in one page of 1000, checked by
// decodeList (assertExplicit included).
func (s *hostStack) ifaceList() []spec.Interface {
	s.t.Helper()
	out, err := s.p.interfaces(context.Background(), spec.ListInput{Limit: 1000})
	if err != nil {
		s.t.Fatalf("interfaces handler: %v", err)
	}
	l := decodeList[spec.InterfaceProperties](s.t, spec.InterfacesType, out, nil)
	if l.NextCursor != "" {
		s.t.Fatalf("limit 1000: nextCursor %q", l.NextCursor)
	}
	return l.Items
}

func ifaceNames(items []spec.Interface) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Name
	}
	return out
}

func ifaceAt(t *testing.T, items []spec.Interface, name string) spec.InterfaceProperties {
	t.Helper()
	for _, it := range items {
		if it.Name == name {
			return it.Properties
		}
	}
	t.Fatalf("interface %s is not listed: %v", name, ifaceNames(items))
	return spec.InterfaceProperties{}
}

// countAddrs wraps the stack's Addrs seam (F-ADDRS) with a call counter.
func countAddrs(s *hostStack) *int {
	n := new(int)
	orig := s.p.addrs
	s.p.addrs = func() (map[string][]netip.Prefix, error) {
		*n++
		return orig()
	}
	return n
}

// ipAddrIface is one interface of the capture's `ip -j addr`.
type ipAddrIface struct {
	IfName    string `json:"ifname"`
	Operstate string `json:"operstate"`
	MTU       int    `json:"mtu"`
	LinkType  string `json:"link_type"`
	Address   string `json:"address"`
	AddrInfo  []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// ipAddrTable reads the capture's tools/ip-addr.
func ipAddrTable(t *testing.T, capture string) []ipAddrIface {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", capture, "tools", "ip-addr"))
	if err != nil {
		t.Fatal(err)
	}
	var out []ipAddrIface
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ipAddrs is ip addr's addresses of one family ("inet" or "inet6") as
// the handler's address objects.
func ipAddrs(ifc ipAddrIface, family string) []spec.Address {
	var out []spec.Address
	for _, a := range ifc.AddrInfo {
		if a.Family == family {
			out = append(out, spec.Address{Address: a.Local, PrefixLength: a.PrefixLen})
		}
	}
	return out
}

// sysAttr is a capture's sys/class/net/<if>/<leaf> without its newline.
func sysAttr(t *testing.T, s *hostStack, name, leaf string) string {
	t.Helper()
	return strings.TrimSpace(string(s.ffs.rawRead("sys/class/net/" + name + "/" + leaf)))
}

// netDevByName is the interfaces of a /proc/net/dev by name.
func netDevByName(t *testing.T, b []byte) map[string]hostparse.NetDevStat {
	t.Helper()
	nd, err := hostparse.NetDev(b)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]hostparse.NetDevStat{}
	for _, n := range nd {
		out[n.Name] = n
	}
	return out
}

// wantCounters checks an item's cumulative counters against one net/dev
// line.
func wantCounters(t *testing.T, name string, got spec.InterfaceProperties, want hostparse.NetDevStat) {
	t.Helper()
	for _, c := range []struct {
		field string
		got   *uint64
		want  uint64
	}{
		{"rxBytes", got.RxBytes, want.RxBytes}, {"txBytes", got.TxBytes, want.TxBytes},
		{"rxErrors", got.RxErrors, want.RxErrs}, {"txErrors", got.TxErrors, want.TxErrs},
		{"rxDropped", got.RxDropped, want.RxDrop}, {"txDropped", got.TxDropped, want.TxDrop},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s %s = %v (%q), want %d", name, c.field, c.got, got.Unavailable[c.field], c.want)
		}
	}
}

// Interfaces page by name with the shared cursor: limit 1 walks every
// interface once, in order, and a cursor this list never issued is
// refused.
func TestInterfacesPageByName(t *testing.T) {
	s := newHostStack(t, capUBU)
	all := ifaceNames(s.ifaceList())
	var walked []string
	cursor := ""
	for range len(all) + 1 {
		out, err := s.p.interfaces(context.Background(), spec.ListInput{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		l := decodeList[spec.InterfaceProperties](t, spec.InterfacesType, out, nil)
		walked = append(walked, ifaceNames(l.Items)...)
		if cursor = l.NextCursor; cursor == "" {
			break
		}
	}
	if !slices.Equal(walked, all) || !slices.IsSorted(all) {
		t.Errorf("limit 1 walked %v; the whole list is %v, want the same names sorted", walked, all)
	}
	_, err := s.p.interfaces(context.Background(), spec.ListInput{Cursor: base64.RawURLEncoding.EncodeToString([]byte("a/b"))})
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.ValidationFailed {
		t.Errorf("a foreign cursor: err %v, want validation-failed", err)
	}
}

// The bonding driver adds a regular file, bonding_masters, beside the
// interface links of /sys/class/net; it is not an interface.
func TestInterfacesSkipTheBondingMastersFile(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/class/net/bonding_masters": {Data: []byte("bond0\n")},
		"sys/class/net/bond0/type":      {Data: []byte("1\n")},
		"sys/class/net/bond0/operstate": {Data: []byte("up\n")},
		"sys/class/net/bond0/address":   {Data: []byte("02:00:00:00:00:09\n")},
		"sys/class/net/bond0/mtu":       {Data: []byte("1500\n")},
		"sys/class/net/bond0/speed":     {Data: []byte("2000\n")},
		"proc/net/dev":                  {Data: capFile(t, capUBU, "fs", pathNetDev)},
	}
	p := New(Options{Host: "tc", Logger: slog.New(&logCapture{}), Platform: &platform.Info{Init: platform.Systemd},
		GOOS: "linux", FS: fsys, Addrs: func() (map[string][]netip.Prefix, error) { return nil, nil }})
	out, err := p.interfaces(context.Background(), spec.ListInput{})
	if err != nil {
		t.Fatal(err)
	}
	l := decodeList[spec.InterfaceProperties](t, spec.InterfacesType, out, nil)
	if got := ifaceNames(l.Items); !slices.Equal(got, []string{"bond0"}) {
		t.Fatalf("names %v, want [bond0]", got)
	}
	if sp := l.Items[0].Properties.SpeedMbps; sp == nil || *sp != 2000 {
		t.Errorf("bond0 speedMbps = %v, want 2000", sp)
	}
}

// An unreadable /sys/class/net fails the list; there is no item to carry
// the reason.
func TestInterfacesUnlistableClassDirFails(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny("sys/class/net")
	_, err := s.p.interfaces(context.Background(), spec.ListInput{})
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.DependencyUnavailable ||
		!strings.Contains(pe.Detail, "permission denied: /sys/class/net") {
		t.Fatalf("err = %v, want dependency-unavailable with permission denied: /sys/class/net", err)
	}
}

// A failing Addrs seam leaves both address lists [] with the reason, and
// is called once per request.
func TestInterfacesAddrsFailure(t *testing.T) {
	s := newHostStack(t, capUBU)
	n := new(int)
	s.p.addrs = func() (map[string][]netip.Prefix, error) {
		*n++
		return nil, &os.SyscallError{Syscall: "netlinkrib", Err: os.ErrPermission}
	}
	for _, it := range s.ifaceList() {
		p := it.Properties
		for _, k := range []string{"ipv4", "ipv6"} {
			if got := p.Unavailable[k]; got != "permission denied: netlink" {
				t.Errorf("%s %s reason %q, want permission denied: netlink", it.Name, k, got)
			}
		}
		if len(p.IPv4) != 0 || len(p.IPv6) != 0 {
			t.Errorf("%s addresses %v %v, want []", it.Name, p.IPv4, p.IPv6)
		}
	}
	if *n != 1 {
		t.Errorf("Addrs called %d times, want 1", *n)
	}
}

// An unreadable /proc/net/dev leaves the counters null with its reason; an
// interface missing from it is not reported. The rest is populated.
func TestInterfacesCounterFailures(t *testing.T) {
	t.Run("net/dev denied", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		s.ffs.deny(pathNetDev)
		eth0 := ifaceAt(t, s.ifaceList(), "eth0")
		for _, k := range []string{"rxBytes", "txBytes", "rxErrors", "txErrors", "rxDropped", "txDropped"} {
			if got := eth0.Unavailable[k]; got != "permission denied: /proc/net/dev" {
				t.Errorf("eth0 %s reason %q, want permission denied: /proc/net/dev", k, got)
			}
		}
		if eth0.MTU == nil || eth0.MAC == nil || len(eth0.IPv4) == 0 {
			t.Errorf("eth0 = %+v; the other fields should be populated", eth0)
		}
	})
	t.Run("interface not in net/dev", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		var kept []string
		for _, l := range strings.Split(string(s.ffs.rawRead(pathNetDev)), "\n") {
			if !strings.Contains(l, "docker0:") {
				kept = append(kept, l)
			}
		}
		s.ffs.replace(pathNetDev, []byte(strings.Join(kept, "\n")))
		d := ifaceAt(t, s.ifaceList(), "docker0")
		if d.RxBytes != nil || !strings.HasPrefix(d.Unavailable["rxBytes"], prefixNotReported) {
			t.Errorf("docker0 rxBytes = %v with %q, want null with not reported:", d.RxBytes, d.Unavailable["rxBytes"])
		}
	})
}

// Unreadable type and operstate: loopback is null and state is unknown,
// each with the reason.
func TestInterfacesUnreadableTypeAndState(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny("sys/class/net/eth0/type")
	s.ffs.deny("sys/class/net/eth0/operstate")
	eth0 := ifaceAt(t, s.ifaceList(), "eth0")
	if eth0.Loopback != nil || eth0.Unavailable["loopback"] != "permission denied: /sys/class/net/eth0/type" {
		t.Errorf("loopback = %v with %q", eth0.Loopback, eth0.Unavailable["loopback"])
	}
	if eth0.State != unknownState || eth0.Unavailable["state"] != "permission denied: /sys/class/net/eth0/operstate" {
		t.Errorf("state = %q with %q", eth0.State, eth0.Unavailable["state"])
	}
}

// A NIC speed read that hangs is bounded by sourceTimeout and reported as
// timed out; a denied one keeps its permission reason, not "not reported".
func TestInterfacesSpeedIsBounded(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.timeout(250 * time.Millisecond)
	s.ffs.block("sys/class/net/eth0/speed")
	s.ffs.deny("sys/class/net/docker0/speed")
	start := time.Now()
	items := s.ifaceList()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the list took %v with one hung speed read", d)
	}
	eth0 := ifaceAt(t, items, "eth0")
	if eth0.SpeedMbps != nil || eth0.Unavailable["speedMbps"] != "timed out after 250ms: /sys/class/net/eth0/speed" {
		t.Errorf("eth0 speedMbps = %v with %q", eth0.SpeedMbps, eth0.Unavailable["speedMbps"])
	}
	d := ifaceAt(t, items, "docker0")
	if d.Unavailable["speedMbps"] != "permission denied: /sys/class/net/docker0/speed" {
		t.Errorf("docker0 speedMbps reason %q", d.Unavailable["speedMbps"])
	}
}

// An interface without a link-layer address (tun, wireguard) prints an
// empty address attribute: mac is null, not "".
func TestInterfacesEmptyAddressIsNotReported(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("sys/class/net/eth0/address", []byte("\n"))
	eth0 := ifaceAt(t, s.ifaceList(), "eth0")
	if eth0.MAC != nil || !strings.HasPrefix(eth0.Unavailable["mac"], prefixNotReported) {
		t.Errorf("mac = %v with %q, want null with not reported:", eth0.MAC, eth0.Unavailable["mac"])
	}
}

// Without a sampler run the rates are null with "not running", and the
// counters are still read live.
func TestInterfacesWithoutSampler(t *testing.T) {
	s := newHostStack(t, capUBU)
	live := netDevByName(t, s.ffs.rawRead(pathNetDev))
	for _, it := range s.ifaceList() {
		p := it.Properties
		if p.Sampler.State != spec.SamplerNotRunning || p.RxBytesPerSecond != nil ||
			p.Unavailable["rxBytesPerSecond"] != reasonNotRunning || p.Unavailable["txBytesPerSecond"] != reasonNotRunning {
			t.Errorf("%s: sampler %q, rx/s %v with %q", it.Name, p.Sampler.State, p.RxBytesPerSecond,
				p.Unavailable["rxBytesPerSecond"])
		}
		wantCounters(t, it.Name, p, live[it.Name])
	}
}
