package auth

import (
	"container/list"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// LimiterCapacity bounds how many sources the limiter tracks; past it the
// least recently used source is forgotten (NFR-4).
const LimiterCapacity = 4096

// FailureWindow is the period auth.failureLimitPerMinute counts over.
const FailureWindow = time.Minute

// DefaultFailureLimitPerMinute is auth.failureLimitPerMinute's default.
const DefaultFailureLimitPerMinute = 10

// SourceAddr is the one place a request's source address is decided; audit
// and the rate limiter both use it. It is always the TCP peer: forwarded
// headers are never read, because no trusted proxy can be configured (A-60).
// An unparsable peer yields the invalid netip.Addr.
func SourceAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		a, err := netip.ParseAddr(r.RemoteAddr)
		if err != nil {
			return netip.Addr{}
		}
		return a.Unmap()
	}
	return ap.Addr().Unmap()
}

// sourceKey groups an address with its neighbours: an IPv4 address stands
// alone, and an IPv6 address counts as its /64, which one host or LAN
// usually controls in full.
func sourceKey(a netip.Addr) netip.Prefix {
	a = a.Unmap().WithZone("")
	switch {
	case a.Is4():
		return netip.PrefixFrom(a, 32)
	case a.Is6():
		return netip.PrefixFrom(a, 64).Masked()
	}
	return netip.Prefix{}
}

// Limiter counts authentication failures per source in fixed windows of
// FailureWindow that start at a source's first failure. It is safe for
// concurrent use.
type Limiter struct {
	limit int
	now   func() time.Time

	mu      sync.Mutex
	lru     *list.List // Front is most recently used; values are *window
	sources map[netip.Prefix]*list.Element
}

type window struct {
	key      netip.Prefix
	start    time.Time
	failures int
}

// NewLimiter returns a limiter that refuses a source once it has limit
// failures in the current window. A nil now means time.Now.
func NewLimiter(limit int, now func() time.Time) *Limiter {
	if limit <= 0 {
		limit = DefaultFailureLimitPerMinute
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{limit: limit, now: now, lru: list.New(), sources: make(map[netip.Prefix]*list.Element)}
}

// Allow reports whether a request from a may be evaluated. When it may not,
// it also returns how long until the source's window ends, rounded up to
// whole seconds and at least one second.
func (l *Limiter) Allow(a netip.Addr) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.current(sourceKey(a))
	if w == nil || w.failures < l.limit {
		return true, 0
	}
	left := w.start.Add(FailureWindow).Sub(l.now())
	secs := (left + time.Second - 1) / time.Second
	return false, max(secs, 1) * time.Second
}

// Fail records an authentication failure from a.
func (l *Limiter) Fail(a netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := sourceKey(a)
	w := l.current(key)
	if w == nil {
		w = &window{key: key, start: l.now()}
		l.sources[key] = l.lru.PushFront(w)
		for l.lru.Len() > LimiterCapacity {
			oldest := l.lru.Back()
			delete(l.sources, oldest.Value.(*window).key)
			l.lru.Remove(oldest)
		}
	}
	w.failures++
}

// Len returns how many sources are tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lru.Len()
}

// current returns key's live window, marking it recently used, or nil. An
// expired window is dropped.
func (l *Limiter) current(key netip.Prefix) *window {
	el, ok := l.sources[key]
	if !ok {
		return nil
	}
	w := el.Value.(*window)
	if !l.now().Before(w.start.Add(FailureWindow)) {
		delete(l.sources, key)
		l.lru.Remove(el)
		return nil
	}
	l.lru.MoveToFront(el)
	return w
}
