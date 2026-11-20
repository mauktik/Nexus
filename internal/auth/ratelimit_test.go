// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
}

// fail records n failed attempts from a through Begin.
func fail(l *Limiter, a netip.Addr, n int) {
	for range n {
		if end, ok, _ := l.Begin(a); ok {
			end(true)
		}
	}
}

// allow is Begin for an attempt that does not fail: whether a may be
// evaluated, and if not, how long until its window ends.
func allow(l *Limiter, a netip.Addr) (bool, time.Duration) {
	end, ok, retry := l.Begin(a)
	if ok {
		end(false)
	}
	return ok, retry
}

// TC-E4: IPv6 sources group by /64 and limiter memory is bounded.
func TestLimiterGroupsIPv6By64(t *testing.T) {
	clk := newClock()
	l := NewLimiter(3, clk.Now)
	a1 := netip.MustParseAddr("2001:db8:1:2::1")
	a2 := netip.MustParseAddr("2001:db8:1:2:ffff:ffff:ffff:fffe")
	other := netip.MustParseAddr("2001:db8:1:3::1")

	fail(l, a1, 2)
	fail(l, a2, 1)
	if ok, _ := allow(l, a1); ok {
		t.Fatal("three failures across one /64 should block that /64")
	}
	if ok, _ := allow(l, a2); ok {
		t.Fatal("the second address in the same /64 shares the count")
	}
	if ok, _ := allow(l, other); !ok {
		t.Fatal("a different /64 must not share the count")
	}
	fail(l, other, 2)
	if ok, _ := allow(l, other); !ok {
		t.Fatal("two failures in the other /64 are under the limit")
	}
	if l.Len() != 2 {
		t.Fatalf("tracked sources = %d, want 2 (one per /64)", l.Len())
	}
}

func TestLimiterIPv4IsPerAddressAndMappedIsIPv4(t *testing.T) {
	clk := newClock()
	l := NewLimiter(1, clk.Now)
	fail(l, netip.MustParseAddr("192.0.2.1"), 1)
	if ok, _ := allow(l, netip.MustParseAddr("::ffff:192.0.2.1")); ok {
		t.Fatal("an IPv4-mapped IPv6 address is the same IPv4 source")
	}
	if ok, _ := allow(l, netip.MustParseAddr("192.0.2.2")); !ok {
		t.Fatal("IPv4 sources are not grouped")
	}
}

func TestLimiterIsBoundedAndEvictsLeastRecentlyUsed(t *testing.T) {
	clk := newClock()
	l := NewLimiter(1, clk.Now)
	first := netip.AddrFrom4([4]byte{10, 0, 0, 0})
	fail(l, first, 1)
	for i := 1; i < 5000; i++ {
		// Keep the first source recently used throughout.
		if i%1000 == 0 {
			fail(l, first, 1)
		}
		fail(l, netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), 1)
		if n := l.Len(); n > LimiterCapacity {
			t.Fatalf("tracker holds %d entries after %d sources, capacity %d", n, i+1, LimiterCapacity)
		}
	}
	if l.Len() != LimiterCapacity {
		t.Fatalf("tracker holds %d entries, want exactly %d", l.Len(), LimiterCapacity)
	}
	if ok, _ := allow(l, first); ok {
		t.Fatal("the recently used source was evicted")
	}
	// Source 1 was touched once, long ago: it is least recently used.
	if ok, _ := allow(l, netip.AddrFrom4([4]byte{10, 0, 0, 1})); !ok {
		t.Fatal("the least recently used source was not evicted")
	}
	if ok, _ := allow(l, netip.AddrFrom4([4]byte{10, 0, 0x13, 0x87})); ok {
		t.Fatal("the most recent source (4999) was evicted")
	}
}

func TestLimiterWindowAndRetryAfter(t *testing.T) {
	clk := newClock()
	l := NewLimiter(2, clk.Now)
	a := netip.MustParseAddr("192.0.2.9")
	fail(l, a, 2)
	clk.Advance(20*time.Second + 300*time.Millisecond)
	ok, retry := allow(l, a)
	if ok {
		t.Fatal("limit reached but allowed")
	}
	if retry != 40*time.Second {
		t.Fatalf("retry after %v, want 40s (rounded up to whole seconds)", retry)
	}
	clk.Advance(39*time.Second + 700*time.Millisecond)
	if ok, _ := allow(l, a); !ok {
		t.Fatal("still blocked after the window ended")
	}
}

func TestSourceAddrIsTheTCPPeerOnly(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/capabilities", nil)
	r.RemoteAddr = "[2001:db8::7]:4431"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	r.Header.Set("X-Real-IP", "198.51.100.2")
	r.Header.Set("Forwarded", "for=198.51.100.3")
	if got := SourceAddr(r); got != netip.MustParseAddr("2001:db8::7") {
		t.Fatalf("SourceAddr = %v, want the TCP peer 2001:db8::7", got)
	}
	r.RemoteAddr = "[::ffff:192.0.2.5]:80"
	if got := SourceAddr(r); got != netip.MustParseAddr("192.0.2.5") {
		t.Fatalf("SourceAddr = %v, want unmapped 192.0.2.5", got)
	}
	r.RemoteAddr = "garbage"
	if got := SourceAddr(r); got.IsValid() {
		t.Fatalf("SourceAddr = %v for an unparsable peer, want the invalid address", got)
	}
}

// CR-13: check and count are one step, so a concurrent burst of bad
// requests from one source gets exactly limit attempts through.
func TestLimiterBeginIsAtomicUnderABurst(t *testing.T) {
	const limit, burst = 10, 500
	l := NewLimiter(limit, newClock().Now)
	a := netip.MustParseAddr("192.0.2.9")
	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if end, ok, _ := l.Begin(a); ok {
				allowed.Add(1)
				end(true)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := allowed.Load(); got != limit {
		t.Fatalf("%d of %d concurrent attempts passed, want exactly %d", got, burst, limit)
	}
	if ok, retry := allow(l, a); ok || retry <= 0 {
		t.Fatalf("an attempt after the burst = %v, %v; want refused", ok, retry)
	}
}

// Valid attempts are never counted, however many arrive together, and leave
// no entry behind.
func TestLimiterBeginDoesNotCountValidAttempts(t *testing.T) {
	l := NewLimiter(2, newClock().Now)
	a := netip.MustParseAddr("192.0.2.10")
	var wg sync.WaitGroup
	var refused atomic.Int64
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			end, ok, _ := l.Begin(a)
			if !ok {
				refused.Add(1)
				return
			}
			end(false)
		}()
	}
	wg.Wait()
	if refused.Load() != 0 || l.Len() != 0 {
		t.Fatalf("%d valid attempts refused, %d sources tracked; want none of either", refused.Load(), l.Len())
	}
}

func TestLimiterBeginWindowsExpireAndFailuresAccumulate(t *testing.T) {
	clk := newClock()
	l := NewLimiter(2, clk.Now)
	a := netip.MustParseAddr("192.0.2.11")
	for i := range 2 {
		end, ok, _ := l.Begin(a)
		if !ok {
			t.Fatalf("attempt %d refused under the limit", i)
		}
		end(true)
	}
	if _, ok, retry := l.Begin(a); ok || retry <= 0 {
		t.Fatalf("third attempt: ok %v retry %v, want refused", ok, retry)
	}
	clk.Advance(FailureWindow)
	end, ok, _ := l.Begin(a)
	if !ok {
		t.Fatal("a new window refused its first attempt")
	}
	end(false)
}
