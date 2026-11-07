package store

import (
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
)

func countRecords(t *testing.T, s *Store) int {
	t.Helper()
	recs, err := s.ListNonTerminal()
	if err != nil {
		t.Fatal(err)
	}
	return len(recs)
}

func newRec() OpRecord {
	return OpRecord{Action: "scripts.run", Provider: "scripts", Exec: ExecDetached}
}

// createConcurrently releases n goroutines together; goroutine i uses
// tokenFor(i).
func createConcurrently(t *testing.T, s *Store, n int, tokenFor func(int) string) []CreateResult {
	t.Helper()
	results := make([]CreateResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = s.CreateWithIdempotency(newRec(), IdempotencyKey{TokenID: tokenFor(i), Key: "same-key", RequestHash: "same-hash"})
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	return results
}

// TC-14: concurrent creates with one key create exactly one record, and every
// caller gets its ID; with two tokens, exactly two records.
func TestCreateWithIdempotencyConcurrent(t *testing.T) {
	t.Run("same token", func(t *testing.T) {
		s := openTemp(t, Options{})
		results := createConcurrently(t, s, 100, func(int) string { return "tok-a" })
		if got := countRecords(t, s); got != 1 {
			t.Fatalf("records = %d, want 1", got)
		}
		created := 0
		for i, r := range results {
			if r.ID != results[0].ID {
				t.Fatalf("goroutine %d got ID %q, goroutine 0 got %q", i, r.ID, results[0].ID)
			}
			if r.RequestHash != "same-hash" {
				t.Fatalf("goroutine %d got hash %q", i, r.RequestHash)
			}
			if !r.Existing {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("%d callers created a record, want 1", created)
		}
	})

	t.Run("two tokens", func(t *testing.T) {
		s := openTemp(t, Options{})
		tokens := []string{"tok-a", "tok-b"}
		results := createConcurrently(t, s, 100, func(i int) string { return tokens[i%2] })
		if got := countRecords(t, s); got != 2 {
			t.Fatalf("records = %d, want 2", got)
		}
		ids := map[string]string{}
		for i, r := range results {
			tok := tokens[i%2]
			if prev, ok := ids[tok]; ok && prev != r.ID {
				t.Fatalf("token %s got IDs %q and %q", tok, prev, r.ID)
			}
			ids[tok] = r.ID
		}
		if ids["tok-a"] == ids["tok-b"] {
			t.Fatal("two tokens share one operation")
		}
	})
}

// A stored key returns the stored hash, so the caller can tell a replay from a
// reuse; nothing is created either way.
func TestCreateWithIdempotencyReuse(t *testing.T) {
	s := openTemp(t, Options{})
	first, err := s.CreateWithIdempotency(newRec(), IdempotencyKey{TokenID: "tok", Key: "k", RequestHash: "h1"})
	if err != nil || first.Existing {
		t.Fatalf("first create: %+v %v", first, err)
	}
	again, err := s.CreateWithIdempotency(newRec(), IdempotencyKey{TokenID: "tok", Key: "k", RequestHash: "h2"})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Existing || again.ID != first.ID || again.RequestHash != "h1" {
		t.Fatalf("reuse = %+v, want existing %s with stored hash h1", again, first.ID)
	}
	if got := countRecords(t, s); got != 1 {
		t.Fatalf("records = %d, want 1", got)
	}

	rec, err := s.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != apitypes.StatusQueued {
		t.Fatalf("new record status = %s, want Queued", rec.Status)
	}

	// No key: every call creates.
	for range 3 {
		r, err := s.CreateWithIdempotency(newRec(), IdempotencyKey{})
		if err != nil || r.Existing {
			t.Fatalf("keyless create: %+v %v", r, err)
		}
	}
	if got := countRecords(t, s); got != 4 {
		t.Fatalf("records = %d, want 4", got)
	}
}

// Options.Now drives expiry: the key answers until 24 hours after creation.
func TestIdempotencyExpiry(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	clock := &fixedClock{t: t0}
	s := openTemp(t, Options{Now: clock.Now})

	first, err := s.CreateWithIdempotency(newRec(), IdempotencyKey{TokenID: "tok", Key: "k", RequestHash: "h"})
	if err != nil {
		t.Fatal(err)
	}

	clock.Set(t0.Add(IdempotencyWindow - time.Nanosecond))
	e, ok, err := s.LookupIdempotency("tok", "k")
	if err != nil || !ok || e.OperationID != first.ID || !e.Expires.Equal(t0.Add(IdempotencyWindow)) {
		t.Fatalf("lookup inside window: %+v %v %v", e, ok, err)
	}
	if _, ok, err := s.LookupIdempotency("other-token", "k"); err != nil || ok {
		t.Fatalf("key leaked across tokens: ok=%v err=%v", ok, err)
	}

	clock.Set(t0.Add(IdempotencyWindow))
	if _, ok, err := s.LookupIdempotency("tok", "k"); err != nil || ok {
		t.Fatalf("lookup at expiry: ok=%v err=%v, want absent", ok, err)
	}
	second, err := s.CreateWithIdempotency(newRec(), IdempotencyKey{TokenID: "tok", Key: "k", RequestHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Existing || second.ID == first.ID {
		t.Fatalf("expired key still answered: %+v", second)
	}
	e, ok, err = s.LookupIdempotency("tok", "k")
	if err != nil || !ok || e.OperationID != second.ID {
		t.Fatalf("key not rebound to the new operation: %+v %v %v", e, ok, err)
	}
}

func TestCreateValidation(t *testing.T) {
	s := openTemp(t, Options{})
	cases := map[string]struct {
		rec  OpRecord
		idem IdempotencyKey
	}{
		"caller-set ID":     {OpRecord{ID: "x", Action: "a", Provider: "p"}, IdempotencyKey{}},
		"no action":         {OpRecord{Provider: "p"}, IdempotencyKey{}},
		"no provider":       {OpRecord{Action: "a"}, IdempotencyKey{}},
		"key without token": {OpRecord{Action: "a", Provider: "p"}, IdempotencyKey{Key: "k"}},
	}
	for name, c := range cases {
		if _, err := s.CreateWithIdempotency(c.rec, c.idem); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if got := countRecords(t, s); got != 0 {
		t.Fatalf("records = %d after refused creates", got)
	}
}
