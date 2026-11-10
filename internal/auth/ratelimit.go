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
	start    time.Time // Zero until the first failure
	failures int
	users    int        // Attempts in flight or waiting; guarded by Limiter.mu
	gate     sync.Mutex // Held for the duration of one attempt's evaluation
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
	l.count(sourceKey(a))
}

// Begin starts one authentication attempt from a. Attempts from one source
// are evaluated one at a time, so the check against the limit and the
// recording of the attempt's failure are one step: a concurrent burst of bad
// requests cannot pass the check before any of its failures is counted. The
// evaluation is microseconds, so valid concurrent requests from one source
// queue only briefly and are never counted against it.
//
// When the source is over its limit, ok is false, retry is as for Allow and
// end is nil. Otherwise the caller evaluates the credentials and calls end
// exactly once, with whether the attempt counts as a failure, before doing
// anything else with the request.
func (l *Limiter) Begin(a netip.Addr) (end func(failed bool), ok bool, retry time.Duration) {
	key := sourceKey(a)
	l.mu.Lock()
	w := l.current(key)
	if w == nil {
		// Registered so that concurrent attempts share one gate; its start
		// is set by the first failure, and it is dropped again if it ends
		// with none.
		w = l.register(&window{key: key})
	}
	w.users++
	l.mu.Unlock()

	w.gate.Lock()
	l.mu.Lock()
	if w.failures >= l.limit && l.now().Before(w.start.Add(FailureWindow)) {
		left := w.start.Add(FailureWindow).Sub(l.now())
		secs := (left + time.Second - 1) / time.Second
		l.release(w)
		l.mu.Unlock()
		w.gate.Unlock()
		return nil, false, max(secs, 1) * time.Second
	}
	l.mu.Unlock()
	return func(failed bool) {
		l.mu.Lock()
		if failed {
			if w.start.IsZero() || !l.now().Before(w.start.Add(FailureWindow)) {
				w.start, w.failures = l.now(), 0
			}
			w.failures++
		}
		l.release(w)
		l.mu.Unlock()
		w.gate.Unlock()
	}, true, 0
}

// release drops one user of w and keeps the table consistent: a window with
// failures is registered, and one with none and no users is not. l.mu is
// held.
func (l *Limiter) release(w *window) {
	w.users--
	el, registered := l.sources[w.key]
	if registered && el.Value.(*window) != w {
		return // A newer window replaced w; its failures are no longer counted
	}
	switch {
	case registered && w.users == 0 && w.failures == 0:
		delete(l.sources, w.key)
		l.lru.Remove(el)
	case !registered && w.failures > 0:
		l.register(w)
	}
}

// register makes w the live window of its source, forgetting the least
// recently used source past LimiterCapacity. l.mu is held.
func (l *Limiter) register(w *window) *window {
	l.sources[w.key] = l.lru.PushFront(w)
	for l.lru.Len() > LimiterCapacity {
		oldest := l.lru.Back()
		delete(l.sources, oldest.Value.(*window).key)
		l.lru.Remove(oldest)
	}
	return w
}

// count adds one failure to key's live window, opening it when there is
// none, and returns the window.
func (l *Limiter) count(key netip.Prefix) *window {
	w := l.current(key)
	if w == nil {
		w = l.register(&window{key: key, start: l.now()})
	} else if w.start.IsZero() {
		w.start = l.now()
	}
	w.failures++
	return w
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
	if !w.start.IsZero() && !l.now().Before(w.start.Add(FailureWindow)) {
		delete(l.sources, key)
		l.lru.Remove(el)
		return nil
	}
	l.lru.MoveToFront(el)
	return w
}
