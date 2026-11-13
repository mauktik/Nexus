//go:build pihole_red

// Stub-style: SKIP-only (WI 2.3). Excluded from `make test` by the
// pihole_red build tag; the WI 2.3 developer un-skips it by deleting the
// //go:build line, and it must then pass. Until then,
// `go test -tags pihole_red ./internal/providers/pihole/dnscheck/` shows it RED.
// Cases: TC-48 to TC-51, TC-ERR8 in planning/pihole-dns-zones/test-cases.md.
// The responder below is a real UDP DNS server built with the same
// golang.org/x/net/dns/dnsmessage codec the production client uses; it is
// not a mock of the client.
package dnscheck_test

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
)

type behavior struct {
	a        []netip.Addr
	rcode    dnsmessage.RCode
	silent   bool
	wrongID  bool
	truncate bool
}

type responder struct {
	addr    netip.AddrPort
	queries atomic.Int64
}

// serve answers on a random loopback UDP port according to rules (keyed by
// FQDN with trailing dot); unknown names get NXDOMAIN.
func serve(t *testing.T, rules map[string]behavior) *responder {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	r := &responder{addr: pc.LocalAddr().(*net.UDPAddr).AddrPort()}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			r.queries.Add(1)
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b, ok := rules[q.Name.String()]
			if !ok {
				b = behavior{rcode: dnsmessage.RCodeNameError}
			}
			if b.silent {
				continue
			}
			id := h.ID
			if b.wrongID {
				id++
			}
			rh := dnsmessage.Header{ID: id, Response: true, RecursionDesired: true, RecursionAvailable: true, RCode: b.rcode, Truncated: b.truncate}
			out, err := buildReply(rh, q, b.a)
			if err != nil {
				// A reply the fake cannot build is never sent; the test under
				// way then fails on its query timeout rather than passing.
				continue
			}
			if _, err := pc.WriteTo(out, from); err != nil {
				return // the listener was closed by t.Cleanup
			}
		}
	}()
	return r
}

func buildReply(rh dnsmessage.Header, q dnsmessage.Question, as []netip.Addr) ([]byte, error) {
	bld := dnsmessage.NewBuilder(nil, rh)
	bld.EnableCompression()
	if err := bld.StartQuestions(); err != nil {
		return nil, err
	}
	if err := bld.Question(q); err != nil {
		return nil, err
	}
	if err := bld.StartAnswers(); err != nil {
		return nil, err
	}
	for _, ip := range as {
		if err := bld.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 0}, dnsmessage.AResource{A: ip.As4()}); err != nil {
			return nil, err
		}
	}
	return bld.Finish()
}

var (
	ip5 = netip.MustParseAddr("192.168.7.5")
	ip7 = netip.MustParseAddr("192.168.7.7")
	pub = netip.MustParseAddr("203.0.113.10")
)

// TC-48: Query sends one A question to exactly the given server and returns
// the RCODE and the A answers.
func TestTC48_QueryAnswers(t *testing.T) {
	r := serve(t, map[string]behavior{
		"kiwi.home.local.": {a: []netip.Addr{ip5}},
		"two.home.local.":  {a: []netip.Addr{ip5, ip7}},
	})
	ctx := context.Background()
	ans, err := dnscheck.Query(ctx, r.addr, "kiwi.home.local", time.Second)
	if err != nil || ans.RCode != "NOERROR" || len(ans.A) != 1 || ans.A[0] != ip5 {
		t.Errorf("kiwi: %+v, %v", ans, err)
	}
	ans, err = dnscheck.Query(ctx, r.addr, "two.home.local", time.Second)
	if err != nil || len(ans.A) != 2 {
		t.Errorf("two: %+v, %v", ans, err)
	}
	ans, err = dnscheck.Query(ctx, r.addr, "nope.home.local", time.Second)
	if err != nil || ans.RCode != "NXDOMAIN" || len(ans.A) != 0 {
		t.Errorf("nxdomain: %+v, %v", ans, err)
	}
}

// TC-ERR8: silence is a timeout error within the timeout; a reply with the
// wrong ID is not accepted as the answer; a truncated reply is an error.
func TestTCERR8_QueryFailures(t *testing.T) {
	r := serve(t, map[string]behavior{
		"silent.lan.":  {silent: true},
		"wrongid.lan.": {a: []netip.Addr{ip5}, wrongID: true},
		"trunc.lan.":   {a: []netip.Addr{ip5}, truncate: true},
	})
	ctx := context.Background()
	start := time.Now()
	if _, err := dnscheck.Query(ctx, r.addr, "silent.lan", 300*time.Millisecond); err == nil {
		t.Error("silent server: want a timeout error")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("silent query took %v, want about the 300ms timeout", el)
	}
	if ans, err := dnscheck.Query(ctx, r.addr, "wrongid.lan", 300*time.Millisecond); err == nil {
		t.Errorf("wrong ID accepted: %+v", ans)
	}
	if ans, err := dnscheck.Query(ctx, r.addr, "trunc.lan", 300*time.Millisecond); err == nil {
		t.Errorf("truncated reply accepted: %+v", ans)
	}
}

// TC-49: WaitReady treats any reply, NXDOMAIN or SERVFAIL included, as ready;
// it times out after the window when nothing answers.
func TestTC49_WaitReady(t *testing.T) {
	r := serve(t, map[string]behavior{"pi.hole.": {rcode: dnsmessage.RCodeServerFailure}})
	d, err := dnscheck.WaitReady(context.Background(), r.addr, 2*time.Second)
	if err != nil || d > time.Second {
		t.Errorf("WaitReady = %v, %v; want ready at once", d, err)
	}
	silent := serve(t, map[string]behavior{"pi.hole.": {silent: true}})
	start := time.Now()
	if _, err := dnscheck.WaitReady(context.Background(), silent.addr, 1200*time.Millisecond); err == nil {
		t.Error("WaitReady on a silent server: want a timeout error")
	}
	if el := time.Since(start); el < time.Second || el > 2500*time.Millisecond {
		t.Errorf("WaitReady gave up after %v, want about the 1.2s window", el)
	}
	if n := silent.queries.Load(); n < 3 {
		t.Errorf("silent server saw %d probes in 1.2s, want one about every 200ms", n)
	}
}

// TC-50: Check implements the FR-24 rules.
func TestTC50_CheckRules(t *testing.T) {
	ans := func(rc string, ips ...netip.Addr) dnscheck.Answer { return dnscheck.Answer{RCode: rc, A: ips} }
	set := func(ip netip.Addr) dnscheck.Want { return dnscheck.Want{Kind: dnscheck.WantSet, IP: ip} }
	rm := func(ip, covered netip.Addr) dnscheck.Want {
		return dnscheck.Want{Kind: dnscheck.WantRemove, IP: ip, CoveredIP: covered}
	}
	var none netip.Addr
	cases := []struct {
		name string
		a    dnscheck.Answer
		w    dnscheck.Want
		pass bool
	}{
		{"add exact", ans("NOERROR", ip5), set(ip5), true},
		{"add old IP still served", ans("NOERROR", ip7), set(ip5), false},
		{"add extra IP", ans("NOERROR", ip5, ip7), set(ip5), false},
		{"add nxdomain", ans("NXDOMAIN"), set(ip5), false},
		{"remove uncovered nxdomain", ans("NXDOMAIN"), rm(ip5, none), true},
		{"remove uncovered upstream answer", ans("NOERROR", pub), rm(ip5, none), true},
		{"remove uncovered still local", ans("NOERROR", ip5), rm(ip5, none), false},
		{"remove covered by other IP", ans("NOERROR", ip7), rm(ip5, ip7), true},
		{"remove covered by other IP, old served", ans("NOERROR", ip5), rm(ip5, ip7), false},
		{"remove covered by same IP", ans("NOERROR", ip5), rm(ip5, ip5), true},
		{"remove covered by same IP, nxdomain", ans("NXDOMAIN"), rm(ip5, ip5), false},
	}
	for _, c := range cases {
		if got := dnscheck.Check(c.a, c.w); got != c.pass {
			t.Errorf("%s: Check = %v, want %v", c.name, got, c.pass)
		}
	}
}

// TC-51: Query never consults /etc/hosts or resolv.conf: "localhost" sent to
// a responder that does not know it is NXDOMAIN, not 127.0.0.1.
func TestTC51_NoSystemResolver(t *testing.T) {
	r := serve(t, nil)
	ans, err := dnscheck.Query(context.Background(), r.addr, "localhost.localdomain", time.Second)
	if err != nil || ans.RCode != "NXDOMAIN" || len(ans.A) != 0 {
		t.Errorf("localhost.localdomain = %+v, %v; want NXDOMAIN from the given server", ans, err)
	}
	if r.queries.Load() != 1 {
		t.Errorf("server saw %d queries, want 1", r.queries.Load())
	}
}
