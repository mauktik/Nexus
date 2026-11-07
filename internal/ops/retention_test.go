package ops_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/store"
)

// fakeRetentionStore answers retention from a map of records.
type fakeRetentionStore struct {
	recs      map[string]store.OpRecord
	prune     []string // IDs PruneTerminal reports deleted
	cutoff    time.Time
	keep      int
	purged    int
	evictions []string
}

func (f *fakeRetentionStore) Get(id string) (store.OpRecord, error) {
	r, ok := f.recs[id]
	if !ok {
		return store.OpRecord{}, fmt.Errorf("operation %q: %w", id, store.ErrNotFound)
	}
	return r, nil
}

func (f *fakeRetentionStore) PruneTerminal(olderThan time.Time, keepNewest int) ([]string, error) {
	f.cutoff, f.keep = olderThan, keepNewest
	return f.prune, nil
}

func (f *fakeRetentionStore) SetOutputEvicted(id string) error {
	r, ok := f.recs[id]
	if !ok {
		return store.ErrNotFound
	}
	r.OutputEvicted = true
	f.recs[id] = r
	f.evictions = append(f.evictions, id)
	return nil
}

func (f *fakeRetentionStore) PurgeExpiredIdempotency() (int, error) {
	f.purged++
	return 0, nil
}

const (
	id1 = "01a1b000-0000-7000-8000-000000000001"
	id2 = "01a1b000-0000-7000-8000-000000000002"
	id3 = "01a1b000-0000-7000-8000-000000000003"
	id4 = "01a1b000-0000-7000-8000-000000000004"
)

func quietLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// Prune passes the FR-4 cutoff and count, removes the directory of each
// pruned operation and nothing else, and purges idempotency keys.
func TestRetentionPruneRemovesPrunedDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{id1, id2, id3} {
		if err := os.MkdirAll(filepath.Join(dir, id, "output"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	f := &fakeRetentionStore{prune: []string{id1, id3, "../escape"}}
	r, err := ops.NewRetention(ops.RetentionOptions{Store: f, Dir: dir, Now: func() time.Time { return now }, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prune(); err != nil {
		t.Fatal(err)
	}
	if !f.cutoff.Equal(now.Add(-30*24*time.Hour)) || f.keep != 1000 || f.purged != 1 {
		t.Fatalf("cutoff %v, keep %d, purged %d", f.cutoff, f.keep, f.purged)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != id2 {
		t.Fatalf("remaining %v, want only %s", entries, id2)
	}
}

// EvictOutput goes oldest first, skips non-terminal and unknown operations
// and those without output, marks each record before removing its output,
// and stops once enough is freed.
func TestRetentionEvictOutput(t *testing.T) {
	dir := t.TempDir()
	write := func(id string, n int) {
		p := filepath.Join(dir, id, "output", "000001.seg")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(id1, 100)                                                     // Running
	write(id2, 100)                                                     // Succeeded
	write(id4, 100)                                                     // No record
	if err := os.MkdirAll(filepath.Join(dir, id3), 0o700); err != nil { // Failed, no output
		t.Fatal(err)
	}
	f := &fakeRetentionStore{recs: map[string]store.OpRecord{
		id1: {ID: id1, Status: apitypes.StatusRunning},
		id2: {ID: id2, Status: apitypes.StatusSucceeded},
		id3: {ID: id3, Status: apitypes.StatusFailed},
	}}
	r, err := ops.NewRetention(ops.RetentionOptions{Store: f, Dir: dir, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	ids, freed, err := r.EvictOutput(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{id2}) || freed != 100 || !slices.Equal(f.evictions, []string{id2}) {
		t.Fatalf("evicted %v (%d bytes), marked %v", ids, freed, f.evictions)
	}
	for id, want := range map[string]bool{id1: true, id2: false, id4: true} {
		_, err := os.Stat(filepath.Join(dir, id, "output"))
		if (err == nil) != want {
			t.Fatalf("output of %s present = %v, want %v", id, err == nil, want)
		}
	}
	// Nothing left to evict.
	if ids, freed, err := r.EvictOutput(1); err != nil || len(ids) != 0 || freed != 0 {
		t.Fatalf("second eviction: %v %d %v", ids, freed, err)
	}
}

// OnTerminal notifies after each committed terminal transition only.
func TestOnTerminalNotifiesAfterTerminalCommits(t *testing.T) {
	w := newWorld(t)
	n := 0
	s := ops.OnTerminal(w.store, func() { n++ })
	create := func() string {
		res, err := w.store.CreateWithIdempotency(store.OpRecord{Action: "tp.things.run", Provider: "tp"}, store.IdempotencyKey{})
		if err != nil {
			t.Fatal(err)
		}
		return res.ID
	}
	a, b := create(), create()
	if _, err := s.CompareAndSetStatus(a, apitypes.StatusQueued, apitypes.StatusRunning); err != nil || n != 0 {
		t.Fatalf("Running: %v, %d notifications", err, n)
	}
	if _, err := s.CommitTerminal(a, apitypes.StatusRunning, apitypes.StatusFailed, nil, problem.New(problem.InternalError, "x")); err != nil || n != 1 {
		t.Fatalf("CommitTerminal: %v, %d notifications", err, n)
	}
	if _, err := s.CompareAndSetStatus(b, apitypes.StatusQueued, apitypes.StatusCanceled); err != nil || n != 2 {
		t.Fatalf("cancel: %v, %d notifications", err, n)
	}
	// A failed commit notifies nothing.
	if _, err := s.CommitTerminal(b, apitypes.StatusRunning, apitypes.StatusSucceeded, json.RawMessage(`{}`), nil); err == nil || n != 2 {
		t.Fatalf("conflicting commit: %v, %d notifications", err, n)
	}
}
