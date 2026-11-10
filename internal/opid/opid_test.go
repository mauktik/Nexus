package opid

import (
	"regexp"
	"sync"
	"testing"
	"time"
)

// Canonical lowercase UUID with version 7 and variant 0b10 (RFC 9562 sections
// 4.1, 4.2 and 5.7).
var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TC-15: IDs are valid UUIDv7, unique, and strictly increasing per goroutine,
// even within one millisecond.
func TestNewValidUniqueAndOrdered(t *testing.T) {
	const total = 100_000
	const workers = 8
	per := total / workers

	results := make([][]string, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids := make([]string, per)
			for i := range ids {
				ids[i] = New()
			}
			results[w] = ids
		}()
	}
	close(start)
	wg.Wait()

	// A tight single-goroutine loop as well, which is certain to produce many
	// IDs in one millisecond.
	single := make([]string, total)
	for i := range single {
		single[i] = New()
	}
	results = append(results, single)

	seen := make(map[string]bool, 2*total)
	sameMillis := 0
	for w, ids := range results {
		for i, id := range ids {
			if !uuidV7.MatchString(id) {
				t.Fatalf("goroutine %d id %d: %q is not a canonical UUIDv7", w, i, id)
			}
			if seen[id] {
				t.Fatalf("duplicate id %q", id)
			}
			seen[id] = true
			if i > 0 {
				if id <= ids[i-1] {
					t.Fatalf("goroutine %d: id %d %q is not greater than id %d %q", w, i, id, i-1, ids[i-1])
				}
				if id[:13] == ids[i-1][:13] {
					sameMillis++
				}
			}
		}
	}
	if sameMillis == 0 {
		t.Fatal("no two consecutive IDs shared a millisecond; the same-tick path was not exercised")
	}
}

// The embedded timestamp is the generation time in Unix milliseconds.
func TestNewEmbedsTimestamp(t *testing.T) {
	before := time.Now().UnixMilli()
	id := New()
	after := time.Now().UnixMilli()
	ms := parseMillis(t, id)
	// A prior same-process ID may have pushed the counter one tick ahead.
	if ms < before || ms > after+1 {
		t.Fatalf("timestamp %d outside [%d, %d]", ms, before, after+1)
	}
}

// A clock that stands still or steps back never produces a smaller ID.
func TestFixedAndBackwardClock(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	g := &generator{now: func() time.Time { return now }}

	prev := g.next()
	for i := range 1000 {
		if i == 500 {
			now = now.Add(-time.Hour)
		}
		id := g.next()
		if id <= prev {
			t.Fatalf("step %d: %q not greater than %q", i, id, prev)
		}
		if !uuidV7.MatchString(id) {
			t.Fatalf("%q is not a canonical UUIDv7", id)
		}
		prev = id
	}
	if got := parseMillis(t, prev); got != 1_700_000_000_000 {
		t.Fatalf("timestamp moved to %d; it must hold at the last tick while the clock is behind", got)
	}
}

// Counter rollover advances the timestamp instead of wrapping to a smaller ID.
func TestCounterRollover(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	g := &generator{now: func() time.Time { return now }}
	first := g.next()
	g.mu.Lock()
	g.randA = randAMask
	g.randB = randBMask
	g.mu.Unlock()
	edge := format(g.ms, g.randA, g.randB)
	next := g.next()
	if next <= edge || next <= first {
		t.Fatalf("rollover produced %q, not greater than %q", next, edge)
	}
	if got := parseMillis(t, next); got != 1_700_000_000_001 {
		t.Fatalf("rollover timestamp = %d, want the next millisecond", got)
	}
}

func parseMillis(t *testing.T, id string) int64 {
	t.Helper()
	hexTS := id[0:8] + id[9:13]
	var ms int64
	for _, c := range hexTS {
		ms <<= 4
		switch {
		case c >= '0' && c <= '9':
			ms |= int64(c - '0')
		case c >= 'a' && c <= 'f':
			ms |= int64(c-'a') + 10
		default:
			t.Fatalf("bad hex in %q", id)
		}
	}
	return ms
}

// CR-12: after Observe(id), every ID is above id even when the clock reads
// earlier than id's timestamp; Observe never lowers the generator, and
// ignores what is not a UUIDv7.
func TestObserveRaisesTheGeneratorPastAPersistedID(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	g := &generator{now: func() time.Time { return now }}
	ahead := format(uint64(now.Add(time.Hour).UnixMilli()), 0xabc, 0x1234)
	ms, a, b, ok := parse(ahead)
	if !ok || ms != uint64(now.Add(time.Hour).UnixMilli()) || a != 0xabc || b != 0x1234 {
		t.Fatalf("parse(%q) = %d %x %x %v", ahead, ms, a, b, ok)
	}
	g.observe(ms, a, b)
	prev := ahead
	for range 1000 {
		id := g.next()
		if id <= prev {
			t.Fatalf("id %q is not above %q", id, prev)
		}
		prev = id
	}
	g.observe(uint64(now.UnixMilli()), 1, 1) // Older: no effect
	if id := g.next(); id <= prev {
		t.Fatalf("id %q after an older Observe is not above %q", id, prev)
	}
	for _, bad := range []string{"", "x", "01a1c3dc-cea2-4321-839c-8e9231d25edf", "01a1c3dc-cea2-7321-039c-8e9231d25edf", "zzzzzzzz-cea2-7321-839c-8e9231d25edf"} {
		if _, _, _, ok := parse(bad); ok {
			t.Errorf("parse(%q) accepted a value that is not a UUIDv7", bad)
		}
	}
}
