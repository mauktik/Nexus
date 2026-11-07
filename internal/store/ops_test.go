package store

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
)

var allStatuses = []apitypes.OperationStatus{
	apitypes.StatusQueued, apitypes.StatusRunning, apitypes.StatusSucceeded,
	apitypes.StatusFailed, apitypes.StatusCanceled,
}

// pathTo is the CompareAndSetStatus chain from Queued to each status.
var pathTo = map[apitypes.OperationStatus][]apitypes.OperationStatus{
	apitypes.StatusQueued:    nil,
	apitypes.StatusRunning:   {apitypes.StatusRunning},
	apitypes.StatusSucceeded: {apitypes.StatusRunning, apitypes.StatusSucceeded},
	apitypes.StatusFailed:    {apitypes.StatusRunning, apitypes.StatusFailed},
	apitypes.StatusCanceled:  {apitypes.StatusCanceled},
}

// createAs creates a record for provider at the clock's time and moves it to
// status, returning the committed record.
func createAs(t *testing.T, s *Store, provider string, status apitypes.OperationStatus) OpRecord {
	t.Helper()
	res, err := s.CreateWithIdempotency(OpRecord{Action: provider + ".x", Provider: provider, Exec: ExecInProcess}, IdempotencyKey{})
	if err != nil {
		t.Fatal(err)
	}
	from := apitypes.StatusQueued
	for _, to := range pathTo[status] {
		if _, err := s.CompareAndSetStatus(res.ID, from, to); err != nil {
			t.Fatal(err)
		}
		from = to
	}
	rec, err := s.Get(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func ids(recs []OpRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

// newestFirst returns recs, which are in creation order, reversed.
func newestFirst(recs []OpRecord) []OpRecord {
	out := slices.Clone(recs)
	slices.Reverse(out)
	return out
}

// listAll pages through q and returns every record and each page's size.
func listAll(t *testing.T, s *Store, q ListQuery) ([]OpRecord, []int) {
	t.Helper()
	var all []OpRecord
	var sizes []int
	for {
		p, err := s.List(q)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, p.Records...)
		sizes = append(sizes, len(p.Records))
		if p.NextCursor == "" {
			return all, sizes
		}
		if len(sizes) > 1000 {
			t.Fatal("pagination does not terminate")
		}
		q.Cursor = p.NextCursor
	}
}

// TC-16: List is newest-first, filters match an in-memory filter, and cursors
// are stable while new records are inserted.
func TestListPagesFiltersAndCursors(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	clock := &fixedClock{t: t0}
	s := openTemp(t, Options{Now: clock.Now})

	providers := []string{"docker", "apt", "scripts"}
	var fixture []OpRecord // creation order
	for i := range 250 {
		clock.Set(t0.Add(time.Duration(i) * time.Minute))
		fixture = append(fixture, createAs(t, s, providers[i%3], allStatuses[(i/3)%5]))
	}
	clock.Set(t0.Add(1000 * time.Minute))
	var inserted []OpRecord

	t.Run("pages are stable under inserts", func(t *testing.T) {
		want := newestFirst(fixture)
		q := ListQuery{Limit: 100}
		var sizes []int
		var got []OpRecord
		for page := 0; ; page++ {
			p, err := s.List(q)
			if err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, len(p.Records))
			got = append(got, p.Records...)
			if p.NextCursor == "" {
				break
			}
			// New records between page fetches must not shift later pages.
			for range 5 {
				inserted = append(inserted, createAs(t, s, "docker", apitypes.StatusQueued))
			}
			q.Cursor = p.NextCursor
		}
		if !slices.Equal(sizes, []int{100, 100, 50}) {
			t.Fatalf("page sizes = %v, want [100 100 50]", sizes)
		}
		if !slices.Equal(ids(got), ids(want)) {
			t.Fatal("paged records are not exactly the pre-existing records newest-first")
		}
		for i := 1; i < len(got); i++ {
			if !got[i].Created.Before(got[i-1].Created) {
				t.Fatalf("record %d created %v is not older than record %d created %v", i, got[i].Created, i-1, got[i-1].Created)
			}
		}
	})

	// Everything in the store now, in creation order.
	all := append(slices.Clone(fixture), inserted...)

	queries := []ListQuery{
		{},
		{Status: apitypes.StatusSucceeded},
		{Provider: "apt"},
		{CreatedAfter: t0.Add(30 * time.Minute)},
		{CreatedBefore: t0.Add(30 * time.Minute)},
		{Status: apitypes.StatusQueued, Provider: "docker"},
		{Status: apitypes.StatusFailed, Provider: "scripts", CreatedAfter: t0.Add(20 * time.Minute), CreatedBefore: t0.Add(200 * time.Minute)},
		{Status: apitypes.StatusCanceled, Provider: "apt", CreatedAfter: t0.Add(249 * time.Minute)},
	}
	for _, limit := range []int{0, 1, 7, 100} {
		for i, q := range queries {
			t.Run(fmt.Sprintf("filter %d limit %d", i, limit), func(t *testing.T) {
				var want []OpRecord
				for _, r := range newestFirst(all) {
					if (q.Status == "" || r.Status == q.Status) &&
						(q.Provider == "" || r.Provider == q.Provider) &&
						(q.CreatedAfter.IsZero() || r.Created.After(q.CreatedAfter)) &&
						(q.CreatedBefore.IsZero() || r.Created.Before(q.CreatedBefore)) {
						want = append(want, r)
					}
				}
				q.Limit = limit
				got, sizes := listAll(t, s, q)
				if !slices.Equal(ids(got), ids(want)) {
					t.Fatalf("got %d records, want %d (in-memory filter)", len(got), len(want))
				}
				pageLimit := limit
				if pageLimit == 0 {
					pageLimit = DefaultListLimit
				}
				for j, n := range sizes {
					if n > pageLimit || (j < len(sizes)-1 && n != pageLimit) {
						t.Fatalf("page sizes %v with limit %d", sizes, pageLimit)
					}
				}
			})
		}
	}
}

func TestListEmptyAndBadCursor(t *testing.T) {
	s := openTemp(t, Options{})
	p, err := s.List(ListQuery{})
	if err != nil || len(p.Records) != 0 || p.NextCursor != "" {
		t.Fatalf("empty store: %+v %v", p, err)
	}
	for _, c := range []string{"!!!", "c2hvcnQ", "not-a-cursor"} {
		if _, err := s.List(ListQuery{Cursor: c}); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("cursor %q: err = %v, want ErrInvalidCursor", c, err)
		}
	}
}

func TestCompareAndSetStatus(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	clock := &fixedClock{t: t0}
	s := openTemp(t, Options{Now: clock.Now})

	res, err := s.CreateWithIdempotency(newRec(), IdempotencyKey{})
	if err != nil {
		t.Fatal(err)
	}

	clock.Set(t0.Add(time.Minute))
	rec, err := s.CompareAndSetStatus(res.ID, apitypes.StatusQueued, apitypes.StatusRunning)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != apitypes.StatusRunning || rec.Started == nil || !rec.Started.Equal(t0.Add(time.Minute)) || rec.Ended != nil {
		t.Fatalf("after Running: %+v", rec)
	}

	// A stale from changes nothing and reports the current record.
	cur, err := s.CompareAndSetStatus(res.ID, apitypes.StatusQueued, apitypes.StatusCanceled)
	if !errors.Is(err, ErrStatusConflict) || cur.Status != apitypes.StatusRunning {
		t.Fatalf("stale CAS: %+v %v", cur, err)
	}
	if got, err := s.Get(res.ID); err != nil || got.Status != apitypes.StatusRunning {
		t.Fatalf("stale CAS changed the record: %+v %v", got, err)
	}

	clock.Set(t0.Add(2 * time.Minute))
	rec, err = s.CompareAndSetStatus(res.ID, apitypes.StatusRunning, apitypes.StatusSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Ended == nil || !rec.Ended.Equal(t0.Add(2*time.Minute)) || !rec.Started.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after Succeeded: %+v", rec)
	}

	if _, err := s.CompareAndSetStatus(res.ID, apitypes.StatusSucceeded, apitypes.StatusRunning); err == nil {
		t.Fatal("CAS out of a terminal status succeeded")
	}
	if _, err := s.CompareAndSetStatus(res.ID, apitypes.StatusRunning, "Approved"); err == nil {
		t.Fatal("CAS to an unknown status succeeded")
	}
	if _, err := s.CompareAndSetStatus("missing", apitypes.StatusQueued, apitypes.StatusRunning); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing record: %v", err)
	}
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
}

// Of the two transitions out of Queued racing, exactly one wins.
func TestCompareAndSetStatusRace(t *testing.T) {
	s := openTemp(t, Options{})
	for round := range 50 {
		res, err := s.CreateWithIdempotency(newRec(), IdempotencyKey{})
		if err != nil {
			t.Fatal(err)
		}
		targets := []apitypes.OperationStatus{apitypes.StatusRunning, apitypes.StatusCanceled}
		errs := make([]error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, to := range targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = s.CompareAndSetStatus(res.ID, apitypes.StatusQueued, to)
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		var winner apitypes.OperationStatus
		for i, err := range errs {
			switch {
			case err == nil:
				wins++
				winner = targets[i]
			case !errors.Is(err, ErrStatusConflict):
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d winners", round, wins)
		}
		if got, err := s.Get(res.ID); err != nil || got.Status != winner {
			t.Fatalf("round %d: stored %+v, winner %s (%v)", round, got, winner, err)
		}
	}
}

// ListNonTerminal returns Queued and Running records in creation order.
func TestListNonTerminal(t *testing.T) {
	s := openTemp(t, Options{})
	var want []string
	for i := range 20 {
		st := allStatuses[i%5]
		r := createAs(t, s, "apt", st)
		if !st.Terminal() {
			want = append(want, r.ID)
		}
	}
	got, err := s.ListNonTerminal()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("ListNonTerminal = %v, want %v", ids(got), want)
	}
}
