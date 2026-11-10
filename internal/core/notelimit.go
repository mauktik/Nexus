package core

import (
	"sync"
	"time"
)

// Limits of the per-token allowance of denied and refused audit records
// (SEC-2).
const (
	DefaultNoteLimitPerMinute = 60
	noteWindowLen             = time.Minute
	// noteCapacity bounds the tokens tracked; past it, expired windows go
	// first and then the table restarts (NFR-4).
	noteCapacity = 4096
)

// noteLimiter counts the records each token causes in fixed windows that
// start at the token's first record. It is safe for concurrent use.
type noteLimiter struct {
	limit int
	now   func() time.Time

	mu      sync.Mutex
	windows map[string]*noteWindow
}

type noteWindow struct {
	start time.Time
	n     int
}

func newNoteLimiter(limit int, now func() time.Time) *noteLimiter {
	if limit <= 0 {
		limit = DefaultNoteLimitPerMinute
	}
	if now == nil {
		now = time.Now
	}
	return &noteLimiter{limit: limit, now: now, windows: map[string]*noteWindow{}}
}

// allow counts one record of token and reports whether it is within the
// token's allowance for the current window.
func (l *noteLimiter) allow(token string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windows[token]
	if w == nil || !now.Before(w.start.Add(noteWindowLen)) {
		if w == nil && len(l.windows) >= noteCapacity {
			for k, v := range l.windows {
				if !now.Before(v.start.Add(noteWindowLen)) {
					delete(l.windows, k)
				}
			}
			if len(l.windows) >= noteCapacity {
				clear(l.windows)
			}
		}
		w = &noteWindow{start: now}
		l.windows[token] = w
	}
	w.n++
	return w.n <= l.limit
}
