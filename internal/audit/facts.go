package audit

import (
	"context"
	"maps"
	"strings"
	"sync"
)

// Bounds of the audit facts (NFR-4): a longer key or value is cut to these
// many bytes, and facts past MaxFacts are dropped, so provider code cannot
// grow one record without limit.
const (
	MaxFactKeyBytes   = 64
	MaxFactValueBytes = 256
	MaxFacts          = 32
)

type factsKey struct{}

// facts is the collector WithFacts puts in a context. Provider code may add
// to it from several goroutines.
type facts struct {
	mu sync.Mutex
	m  map[string]string
}

// WithFacts returns ctx carrying a new, empty facts collector. The
// Dispatcher installs one before provider code runs for a request, and
// copies what was collected into that request's audit records (FR-16).
func WithFacts(ctx context.Context) context.Context {
	return context.WithValue(ctx, factsKey{}, &facts{m: map[string]string{}})
}

// AddFact records key=value in the context's facts collector, for the
// request's audit records. It is a no-op without a collector. Facts are
// written to the audit log as they are, so they must never hold secrets or
// key material. A later value for a key replaces the earlier one; an empty
// key is ignored.
func AddFact(ctx context.Context, key, value string) {
	f, ok := ctx.Value(factsKey{}).(*facts)
	if !ok {
		return
	}
	key = truncateUTF8(key, MaxFactKeyBytes)
	if key == "" {
		return
	}
	value = truncateUTF8(value, MaxFactValueBytes)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.m[key]; !exists && len(f.m) >= MaxFacts {
		return
	}
	f.m[key] = value
}

// Facts returns a copy of the facts collected in ctx so far, or nil when
// there is no collector or it is empty.
func Facts(ctx context.Context) map[string]string {
	f, ok := ctx.Value(factsKey{}).(*facts)
	if !ok {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.m) == 0 {
		return nil
	}
	return maps.Clone(f.m)
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune; any
// invalid UTF-8 is dropped.
func truncateUTF8(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}
