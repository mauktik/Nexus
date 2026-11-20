// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/opid"
	"github.com/mauktik/nexus/internal/problem"
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

// CommitTerminal moves a record from Queued or Running to a terminal status
// in one transaction, storing Result or Error and Ended, and refuses a stale
// from without changing anything (WI 3.2 store addition).
func TestCommitTerminal(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	clock := &fixedClock{t: t0}
	s := openTemp(t, Options{Now: clock.Now})

	ok := createAs(t, s, "apt", apitypes.StatusRunning)
	clock.Set(t0.Add(time.Minute))
	rec, err := s.CommitTerminal(ok.ID, apitypes.StatusRunning, apitypes.StatusSucceeded, json.RawMessage(`{"changed":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != apitypes.StatusSucceeded || string(rec.Result) != `{"changed":true}` || rec.Error != nil ||
		rec.Ended == nil || !rec.Ended.Equal(t0.Add(time.Minute)) || rec.Started == nil {
		t.Fatalf("committed %+v", rec)
	}
	if got, err := s.Get(ok.ID); err != nil || string(got.Result) != `{"changed":true}` || got.Status != apitypes.StatusSucceeded {
		t.Fatalf("stored %+v %v", got, err)
	}

	failed := createAs(t, s, "apt", apitypes.StatusRunning)
	perr := problem.New(problem.InternalError, "the operation failed")
	rec, err = s.CommitTerminal(failed.ID, apitypes.StatusRunning, apitypes.StatusFailed, nil, perr)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(failed.ID); got.Error == nil || got.Error.Slug() != problem.InternalError || got.Result != nil {
		t.Fatalf("failed record %+v", got)
	}

	// From Queued: no Started.
	q := createAs(t, s, "apt", apitypes.StatusQueued)
	rec, err = s.CommitTerminal(q.ID, apitypes.StatusQueued, apitypes.StatusFailed, nil,
		problem.New(problem.OutcomeUnknown, "never started"))
	if err != nil || rec.Started != nil || rec.Ended == nil {
		t.Fatalf("from Queued: %+v %v", rec, err)
	}

	// A stale from changes nothing and reports the current record.
	cur, err := s.CommitTerminal(ok.ID, apitypes.StatusRunning, apitypes.StatusFailed, nil, perr)
	if !errors.Is(err, ErrStatusConflict) || cur.Status != apitypes.StatusSucceeded {
		t.Fatalf("stale commit: %+v %v", cur, err)
	}
	if got, _ := s.Get(ok.ID); got.Status != apitypes.StatusSucceeded || got.Error != nil {
		t.Fatalf("stale commit changed the record: %+v", got)
	}

	r := createAs(t, s, "apt", apitypes.StatusRunning)
	bad := []struct {
		name     string
		from, to apitypes.OperationStatus
		perr     *problem.Error
	}{
		{"from terminal", apitypes.StatusSucceeded, apitypes.StatusFailed, perr},
		{"to non-terminal", apitypes.StatusRunning, apitypes.StatusQueued, nil},
		{"unknown status", apitypes.StatusRunning, "Approved", nil},
		{"failed without a problem", apitypes.StatusRunning, apitypes.StatusFailed, nil},
		{"succeeded with a problem", apitypes.StatusRunning, apitypes.StatusSucceeded, perr},
	}
	for _, c := range bad {
		if _, err := s.CommitTerminal(r.ID, c.from, c.to, nil, c.perr); err == nil || errors.Is(err, ErrStatusConflict) {
			t.Errorf("%s: err = %v, want a usage error", c.name, err)
		}
	}
	if got, _ := s.Get(r.ID); got.Status != apitypes.StatusRunning {
		t.Fatalf("a refused commit changed the record: %+v", got)
	}
	if _, err := s.CommitTerminal("missing", apitypes.StatusRunning, apitypes.StatusCanceled, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

// Of a cancel and a terminal commit racing out of Queued, exactly one wins.
func TestCommitTerminalRace(t *testing.T) {
	s := openTemp(t, Options{})
	for round := range 50 {
		rec := createAs(t, s, "apt", apitypes.StatusQueued)
		errs := make([]error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			_, errs[0] = s.CompareAndSetStatus(rec.ID, apitypes.StatusQueued, apitypes.StatusCanceled)
		})
		wg.Go(func() {
			<-start
			_, errs[1] = s.CommitTerminal(rec.ID, apitypes.StatusQueued, apitypes.StatusFailed, nil,
				problem.New(problem.OutcomeUnknown, "never started"))
		})
		close(start)
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: errs %v", round, errs)
		}
	}
}

// ListQuery.Providers restricts the scan to a set of providers: nil means
// no restriction and a non-nil empty set matches nothing. Page sizes and
// cursors stay correct because the filter applies inside the scan.
func TestListProvidersFilter(t *testing.T) {
	s := openTemp(t, Options{})
	var fixture []OpRecord
	for i := range 60 {
		fixture = append(fixture, createAs(t, s, []string{"docker", "apt", "scripts"}[i%3], apitypes.StatusQueued))
	}
	want := func(keep func(OpRecord) bool) []string {
		var out []string
		for _, r := range newestFirst(fixture) {
			if keep(r) {
				out = append(out, r.ID)
			}
		}
		return out
	}

	got, sizes := listAll(t, s, ListQuery{Providers: []string{"docker", "scripts"}, Limit: 7})
	if !slices.Equal(ids(got), want(func(r OpRecord) bool { return r.Provider != "apt" })) {
		t.Fatalf("docker+scripts = %v", ids(got))
	}
	for i, n := range sizes[:len(sizes)-1] {
		if n != 7 {
			t.Fatalf("page %d has %d records, want 7", i, n)
		}
	}
	got, _ = listAll(t, s, ListQuery{Providers: []string{"docker", "scripts"}, Provider: "scripts"})
	if !slices.Equal(ids(got), want(func(r OpRecord) bool { return r.Provider == "scripts" })) {
		t.Fatalf("scripts within docker+scripts = %v", ids(got))
	}
	got, _ = listAll(t, s, ListQuery{Providers: []string{"docker"}, Provider: "apt"})
	if len(got) != 0 {
		t.Fatalf("apt outside the allowed set = %v", ids(got))
	}
	p, err := s.List(ListQuery{Providers: []string{}})
	if err != nil || len(p.Records) != 0 || p.NextCursor != "" {
		t.Fatalf("empty set = %+v %v, want nothing", p, err)
	}
	got, _ = listAll(t, s, ListQuery{Providers: nil})
	if len(got) != len(fixture) {
		t.Fatalf("nil set = %d records, want %d", len(got), len(fixture))
	}
}

// seedAt creates n records moved to status with the clock at at, so a
// terminal one has Ended == at, and returns their IDs in creation order.
func seedAt(t *testing.T, s *Store, clock *fixedClock, at time.Time, n int, status apitypes.OperationStatus) []string {
	t.Helper()
	clock.Set(at)
	out := make([]string, n)
	for i := range out {
		out[i] = createAs(t, s, "p", status).ID
	}
	return out
}

// bulkTerminal writes n terminal records created and ended at at, in one
// transaction: thousands of fsynced commits would dominate the test. The
// records are what CreateWithIdempotency and CommitTerminal would leave:
// opid IDs in creation order, Created and Ended set.
func bulkTerminal(t *testing.T, s *Store, at time.Time, n int, status apitypes.OperationStatus) []string {
	t.Helper()
	out := make([]string, n)
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketOps)
		for i := range out {
			ended := at
			rec := OpRecord{ID: opid.New(), Action: "p.x", Provider: "p", Status: status, Created: at,
				Started: &ended, Ended: &ended, Exec: ExecInProcess}
			enc, err := encodeRecord(rec)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(rec.ID), enc); err != nil {
				return err
			}
			out[i] = rec.ID
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func allIDs(t *testing.T, s *Store) []string {
	t.Helper()
	recs, _ := listAll(t, s, ListQuery{Limit: MaxListLimit})
	out := ids(recs)
	slices.Sort(out)
	return out
}

// TC-87: retention keeps whichever retains more of the records younger than
// 30 days and the newest 1,000, and never removes a non-terminal record.
// The expected keep-set is computed independently of PruneTerminal:
// keep = newest max(count(age < 30d), min(1000, total)).
func TestPruneTerminalKeepsWhicheverRetainsMore(t *testing.T) {
	const (
		maxAge = 30 * 24 * time.Hour
		keep   = 1000
	)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-40*24*time.Hour), now.Add(-5*24*time.Hour)
	cases := []struct {
		name          string
		nOld, nRecent int
		want          int
	}{
		{"1500 old", 1500, 0, 1000},
		{"1500 recent", 0, 1500, 1500},
		{"1100 old and 100 recent", 1100, 100, 1000},
		{"900 old", 900, 0, 900},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock := &fixedClock{t: old}
			s := openTemp(t, Options{Now: clock.Now})
			// Old non-terminal records come first, so they are the oldest IDs
			// and outside the newest 1,000.
			live := append(seedAt(t, s, clock, old.Add(-time.Hour), 3, apitypes.StatusQueued),
				seedAt(t, s, clock, old.Add(-time.Hour), 3, apitypes.StatusRunning)...)
			terminal := append(bulkTerminal(t, s, old, c.nOld, apitypes.StatusSucceeded),
				bulkTerminal(t, s, recent, c.nRecent, apitypes.StatusCanceled)...)

			// Expected: the newest max(young, min(keep, total)) terminal
			// records, plus every non-terminal one.
			young := c.nRecent
			n := max(young, min(keep, len(terminal)))
			want := append(slices.Clone(terminal[len(terminal)-n:]), live...)
			slices.Sort(want)
			if len(want)-len(live) != c.want {
				t.Fatalf("fixture: computed keep-set has %d terminal records, table says %d", len(want)-len(live), c.want)
			}

			clock.Set(now)
			deleted, err := s.PruneTerminal(now.Add(-maxAge), keep)
			if err != nil {
				t.Fatal(err)
			}
			got := allIDs(t, s)
			if !slices.Equal(got, want) {
				t.Fatalf("remaining %d records, want %d", len(got), len(want))
			}
			if len(deleted) != len(terminal)-n || !slices.IsSorted(deleted) {
				t.Fatalf("deleted %d IDs (sorted %v), want %d oldest first", len(deleted), slices.IsSorted(deleted), len(terminal)-n)
			}
			for _, id := range deleted {
				if slices.Contains(want, id) {
					t.Fatalf("deleted %s, which is in the keep-set", id)
				}
			}
			for _, id := range live {
				if _, err := s.Get(id); err != nil {
					t.Fatalf("non-terminal record %s removed: %v", id, err)
				}
			}
			// A second pass finds nothing more.
			if again, err := s.PruneTerminal(now.Add(-maxAge), keep); err != nil || len(again) != 0 {
				t.Fatalf("second prune deleted %d (%v)", len(again), err)
			}
		})
	}
}

// Retention is measured from Ended, not Created: an operation created long
// ago that ended recently is young.
func TestPruneTerminalMeasuresFromEnded(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	clock := &fixedClock{t: now.Add(-60 * 24 * time.Hour)}
	s := openTemp(t, Options{Now: clock.Now})
	res, err := s.CreateWithIdempotency(newRec(), IdempotencyKey{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompareAndSetStatus(res.ID, apitypes.StatusQueued, apitypes.StatusRunning); err != nil {
		t.Fatal(err)
	}
	clock.Set(now.Add(-24 * time.Hour))
	if _, err := s.CommitTerminal(res.ID, apitypes.StatusRunning, apitypes.StatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	clock.Set(now)
	if deleted, err := s.PruneTerminal(now.Add(-30*24*time.Hour), 0); err != nil || len(deleted) != 0 {
		t.Fatalf("deleted %v (%v); the record ended a day ago", deleted, err)
	}
	if deleted, err := s.PruneTerminal(now, 0); err != nil || !slices.Equal(deleted, []string{res.ID}) {
		t.Fatalf("deleted %v (%v), want %s", deleted, err, res.ID)
	}
	if _, err := s.Get(res.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after prune: %v", err)
	}
}

// SetOutputEvicted sets only OutputEvicted, and a gone record is
// ErrNotFound.
func TestSetOutputEvicted(t *testing.T) {
	s := openTemp(t, Options{})
	rec := createAs(t, s, "p", apitypes.StatusSucceeded)
	if rec.OutputEvicted {
		t.Fatal("a new record is evicted")
	}
	if err := s.SetOutputEvicted(rec.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := rec
	want.OutputEvicted = true
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after SetOutputEvicted:\n got %+v\nwant %+v", got, want)
	}
	if !got.ToAPI("h").OutputEvicted {
		t.Fatal("outputEvicted does not reach the API")
	}
	if err := s.SetOutputEvicted(rec.ID); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if err := s.SetOutputEvicted("0190a1b2-0000-7000-8000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing record: %v, want ErrNotFound", err)
	}
}
