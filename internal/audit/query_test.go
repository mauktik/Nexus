// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/systemspec"
)

// fixtureID is a well-formed UUIDv7 whose order is n's order.
func fixtureID(ms uint64, n int) string {
	return fmt.Sprintf("%08x-%04x-7000-8000-%012x", ms>>16, ms&0xffff, n)
}

func fixtureLines(t *testing.T, recs []systemspec.AuditRecord) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(line, '\n'))
	}
	return b.Bytes()
}

func writeGz(t *testing.T, dir string, at time.Time, recs []systemspec.AuditRecord) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(fixtureLines(t, recs)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, audit.RotatedName(at)), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// page follows NextCursor from the first page of q to the end.
func page(t *testing.T, l *audit.Log, q audit.Query) (ids []string, pages int) {
	t.Helper()
	for {
		res, err := l.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range res.Items {
			ids = append(ids, r.ID)
		}
		if res.NextCursor == "" {
			return ids, pages
		}
		if pages > 100 {
			t.Fatalf("paging never ended: %v", ids)
		}
		q.Cursor = res.NextCursor
	}
}

// CR-15: paging, filters and order across the live file, a compressed
// rotation, an uncompressed rotation, and an uncompressed twin that is
// dropped because a .gz holds the same records.
func TestQueryPagesAndFiltersAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := func(n int, token, outcome string) systemspec.AuditRecord {
		return systemspec.AuditRecord{ID: fixtureID(0x019000000000, n), Time: base.Add(time.Duration(n) * time.Minute),
			TokenName: token, Outcome: outcome, Action: "t.op.go", Provider: "t"}
	}
	var (
		oldGz    = []systemspec.AuditRecord{rec(1, "a", "denied"), rec(2, "b", "requested"), rec(3, "a", "requested")}
		midPlain = []systemspec.AuditRecord{rec(4, "b", "denied"), rec(5, "a", "requested"), rec(6, "b", "requested")}
		newGz    = []systemspec.AuditRecord{rec(7, "a", "requested"), rec(8, "b", "denied"), rec(9, "a", "denied")}
		twin     = []systemspec.AuditRecord{rec(7, "a", "requested"), rec(98, "zz", "twin")}
	)
	l := open(t, audit.Options{Dir: dir, Now: func() time.Time { return base.Add(time.Hour) }})
	writeGz(t, dir, base.Add(3*time.Minute+time.Second), oldGz)
	midName := strings.TrimSuffix(audit.RotatedName(base.Add(6*time.Minute+time.Second)), ".gz")
	if err := os.WriteFile(filepath.Join(dir, midName), fixtureLines(t, midPlain), 0o600); err != nil {
		t.Fatal(err)
	}
	writeGz(t, dir, base.Add(9*time.Minute+time.Second), newGz)
	twinName := strings.TrimSuffix(audit.RotatedName(base.Add(9*time.Minute+time.Second)), ".gz")
	if err := os.WriteFile(filepath.Join(dir, twinName), fixtureLines(t, twin), 0o600); err != nil {
		t.Fatal(err)
	}
	// The live file's records are the newest: IDs the generator issues.
	var live []string
	for range 3 {
		id, err := l.Write(systemspec.AuditRecord{Time: base.Add(time.Hour), TokenName: "a", Outcome: "requested", Action: "t.op.go"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		live = append([]string{id}, live...)
	}

	all := slices.Concat(live, []string{fixtureID(0x019000000000, 9), fixtureID(0x019000000000, 8), fixtureID(0x019000000000, 7)})
	for n := 6; n >= 1; n-- {
		all = append(all, fixtureID(0x019000000000, n))
	}
	for _, limit := range []int{1, 2, 5, 100} {
		got, _ := page(t, l, audit.Query{Limit: limit})
		if !slices.Equal(got, all) {
			t.Fatalf("limit %d: ids\n got %v\nwant %v", limit, got, all)
		}
	}
	if got, pages := page(t, l, audit.Query{Limit: 4}); len(got) != 12 || pages != 3 {
		t.Fatalf("limit 4: %d ids in %d pages, want 12 in 3", len(got), pages)
	}

	id := func(n int) string { return fixtureID(0x019000000000, n) }
	for _, tc := range []struct {
		name string
		q    audit.Query
		want []string
	}{
		{"token", audit.Query{TokenName: "b", Limit: 1}, []string{id(8), id(6), id(4), id(2)}},
		{"outcome", audit.Query{Outcome: "denied", Limit: 2}, []string{id(9), id(8), id(4), id(1)}},
		{"token and outcome", audit.Query{TokenName: "a", Outcome: "denied", Limit: 1}, []string{id(9), id(1)}},
		{"after", audit.Query{After: base.Add(7 * time.Minute), Limit: 1, TokenName: "b"}, []string{id(8)}},
		{"before", audit.Query{Before: base.Add(4 * time.Minute), Limit: 1}, []string{id(3), id(2), id(1)}},
		{"after and before", audit.Query{After: base.Add(2 * time.Minute), Before: base.Add(6 * time.Minute), Limit: 2}, []string{id(5), id(4), id(3)}},
		{"no match", audit.Query{TokenName: "nobody", Limit: 2}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := page(t, l, tc.q)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ids\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

// CR-12: records written after a restart sort after the records before it
// even when the clock now reads earlier than when those were written, so
// paging by ID skips and repeats nothing.
func TestIDsStayAboveThePersistedMaximumAfterAClockStepBack(t *testing.T) {
	dir := t.TempDir()
	ahead := uint64(time.Now().Add(time.Hour).UnixMilli())
	var before []systemspec.AuditRecord
	for n := 1; n <= 3; n++ {
		before = append(before, systemspec.AuditRecord{ID: fixtureID(ahead, n), Time: time.Now().UTC(), Outcome: "requested", Action: "t.op.go"})
	}
	if err := os.WriteFile(filepath.Join(dir, audit.FileName), fixtureLines(t, before), 0o600); err != nil {
		t.Fatal(err)
	}
	l := open(t, audit.Options{Dir: dir})
	var after []string
	for range 3 {
		id, err := l.Write(systemspec.AuditRecord{Outcome: "requested", Action: "t.op.go"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		after = append([]string{id}, after...)
	}
	want := slices.Concat(after, []string{before[2].ID, before[1].ID, before[0].ID})
	for _, limit := range []int{1, 2, 4} {
		if got, _ := page(t, l, audit.Query{Limit: limit}); !slices.Equal(got, want) {
			t.Fatalf("limit %d: ids\n got %v\nwant %v", limit, got, want)
		}
	}
}

// CR-14: a query whose context has ended stops.
func TestQueryStopsWhenItsContextEnds(t *testing.T) {
	l := open(t, audit.Options{})
	if _, err := l.Write(systemspec.AuditRecord{Outcome: "requested", Action: "t.op.go"}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.QueryContext(ctx, audit.Query{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("QueryContext on a canceled context: %v, want context.Canceled", err)
	}
}

// CR-14: a query that the newest file satisfies never reads an older one,
// and rotated files that ended at or before After are not read.
func TestQueryDoesNotReadFilesItDoesNotNeed(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	l := open(t, audit.Options{Dir: dir, Logger: newTextLogger(&logs), Now: func() time.Time { return base.Add(time.Hour) }})
	// A corrupt compressed file warns when it is read, and only then.
	bad := filepath.Join(dir, audit.RotatedName(base))
	if err := os.WriteFile(bad, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := l.Write(systemspec.AuditRecord{Time: base.Add(time.Hour), Outcome: "requested", Action: "t.op.go"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := l.Query(audit.Query{Limit: 2}); err != nil || len(res.Items) != 2 || res.NextCursor == "" {
		t.Fatalf("query: %+v %v", res, err)
	}
	if res, err := l.Query(audit.Query{After: base.Add(time.Minute)}); err != nil || len(res.Items) != 3 {
		t.Fatalf("query after: %+v %v", res, err)
	}
	if strings.Contains(logs.String(), "unreadable") {
		t.Fatalf("an unneeded file was read:\n%s", logs.String())
	}
	if _, err := l.Query(audit.Query{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "unreadable") {
		t.Fatalf("a query that needs every file did not read the corrupt one:\n%s", logs.String())
	}
}

// CR-4: an audit.retentionDays too large for a time.Duration must not wrap
// into a cutoff in the future that deletes the history.
func TestHugeRetentionDaysKeepsRecentRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	name := seedRotated(t, dir, clk.Now().Add(-time.Hour))
	old := seedRotated(t, dir, clk.Now().Add(-365*24*time.Hour))
	l := open(t, audit.Options{Dir: dir, Now: clk.Now, RetentionDays: 999999})
	for _, n := range []string{name, old} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("rotated file %s after Open with a huge retention: %v", n, err)
		}
	}
	if err := l.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("rotated file after Prune: %v", err)
	}
}

func newTextLogger(w *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

// CR-94: Open seeds IDs from the live file's last record, which holds the
// largest ID (file order is ID order), without parsing the rest of the
// file; a last line that does not parse falls back to reading every record.
func TestOpenSeedsIDsFromTheLastLiveRecord(t *testing.T) {
	ahead := uint64(time.Now().Add(2 * time.Hour).UnixMilli())
	rec := func(n int) systemspec.AuditRecord {
		return systemspec.AuditRecord{ID: fixtureID(ahead, n), Time: time.Now().UTC(), Outcome: "requested", Action: "t.op.go"}
	}
	for _, tc := range []struct {
		name    string
		body    []byte
		max     string
		skipped bool // Open logs an unparsable line: it read the whole file
	}{
		{"last line", append([]byte("not a record\n"), fixtureLines(t, []systemspec.AuditRecord{rec(1), rec(2)})...),
			fixtureID(ahead, 2), false},
		{"last line longer than one read", append([]byte("not a record\n"), fixtureLines(t, []systemspec.AuditRecord{rec(5),
			func() systemspec.AuditRecord { r := rec(6); r.Action = strings.Repeat("a", 10_000); return r }()})...),
			fixtureID(ahead, 6), false},
		{"last line cut short", append(fixtureLines(t, []systemspec.AuditRecord{rec(3), rec(4)}), `{"id":"`...),
			fixtureID(ahead, 4), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, audit.FileName), tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			l := open(t, audit.Options{Dir: dir, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			if skipped := strings.Contains(logs.String(), "skipping unparsable audit line"); skipped != tc.skipped {
				t.Fatalf("Open parsed every line: %v, want %v\n%s", skipped, tc.skipped, logs.String())
			}
			id, err := l.Write(systemspec.AuditRecord{Outcome: "requested", Action: "t.op.go"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if id <= tc.max {
				t.Fatalf("new ID %s is not above the persisted maximum %s", id, tc.max)
			}
		})
	}
}
