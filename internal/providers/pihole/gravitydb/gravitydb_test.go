// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Cases: TC-32 to TC-36, TC-E9 and TC-ERR5 in
// planning/pihole-dns-zones/test-cases.md, and TC-147 (WI 5.4). TC-37 (release binary size) is a
// build measurement, recorded in the WI notes, not a Go test.
//
// Every database is built from testdata/gravity.db.sql, Pi-hole core v5.18.4's
// template verbatim (see testdata/PROVENANCE), in t.TempDir(). There is no
// fake database.
package gravitydb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// buildDB creates path from the fixture schema, then runs seed. It uses its
// own read-write connection, not the package under test.
func buildDB(t *testing.T, path string, seed ...string) {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "gravity.db.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("load fixture schema: %v", err)
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertNoSidecar(t *testing.T, dir string) {
	t.Helper()
	for _, n := range dirNames(t, dir) {
		for _, suffix := range []string{"-journal", "-wal", "-shm"} {
			if strings.HasSuffix(n, suffix) {
				t.Errorf("sidecar file %s appeared in %s", n, dir)
			}
		}
	}
}

func readAll(t *testing.T, path string) ([]spec.DomainProperties, []spec.AdlistProperties, Info) {
	t.Helper()
	ctx := context.Background()
	d, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer d.Close()
	domains, err := d.Domains(ctx)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	adlists, err := d.Adlists(ctx)
	if err != nil {
		t.Fatalf("Adlists: %v", err)
	}
	info, err := d.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	return domains, adlists, info
}

// TC-32: the read-only open creates no sidecar file and refuses writes, and
// reads still work in a directory nexusd cannot write to.
func TestReadOnlyOpenNoSidecarRefusesWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "gravity.db")
	buildDB(t, path,
		`INSERT INTO domainlist (id, type, domain) VALUES (1, 1, 'ads.example')`,
		`INSERT INTO info VALUES ('updated', '1720203654'), ('gravity_count', '12')`)
	before := dirNames(t, dir)

	domains, _, info := readAll(t, path)
	if len(domains) != 1 || info.GravityCount == nil || *info.GravityCount != 12 {
		t.Fatalf("reads: domains %+v, info %+v", domains, info)
	}

	d, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	var queryOnly int
	if err := d.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	if mode != "delete" || queryOnly != 1 {
		t.Errorf("journal_mode %q query_only %d, want delete and 1", mode, queryOnly)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO domainlist (type, domain) VALUES (0, 'raw.example')`); err == nil {
		t.Error("raw INSERT through the read-only handle succeeded")
	}
	writeErr := func() error {
		tx, err := d.BeginImmediate(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		_, err = tx.InsertDomain(ctx, TypeDenyExact, "write.example", "")
		return err
	}()
	var se *sqlite.Error
	if !errors.As(writeErr, &se) || se.Code()&0xff != sqlite3.SQLITE_READONLY {
		t.Errorf("InsertDomain through the read-only handle: %v, want SQLITE_READONLY", writeErr)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoSidecar(t, dir)
	if after := dirNames(t, dir); !slices.Equal(before, after) {
		t.Errorf("directory changed: before %v, after %v", before, after)
	}
	domains, _, _ = readAll(t, path)
	if len(domains) != 1 {
		t.Errorf("a write landed: %d domains", len(domains))
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	domains, _, _ = readAll(t, path)
	if len(domains) != 1 {
		t.Errorf("read in a 0555 directory: %d domains", len(domains))
	}
	assertNoSidecar(t, dir)
	if after := dirNames(t, dir); !slices.Equal(before, after) {
		t.Errorf("directory changed in 0555: before %v, after %v", before, after)
	}
}

// TC-33: typed reads map Pi-hole's encodings.
func TestTypedReadsMapEncodings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gravity.db")
	// Inserted out of id order, so ordering by id is observable.
	buildDB(t, path,
		`INSERT INTO domainlist (id, type, domain, enabled, date_added, date_modified, comment) VALUES
			(4, 3, '(\.|^)track\.example$', 1, 1700000004, 1700000014, 'deny regex'),
			(2, 1, 'ads.example', 0, 1700000002, 1700000012, NULL),
			(3, 2, '^ok\.example$', 1, 1700000003, 1700000013, ''),
			(1, 0, 'allow.example', 1, 1700000001, 1700000011, 'allow exact')`,
		`INSERT INTO adlist (id, address, enabled, date_added, date_modified, comment, date_updated, number, status) VALUES
			(5, 'https://e.example/list', 1, 1600000005, 1600000015, 'e', 1710000005, 50, 4),
			(1, 'https://a.example/list', 1, 1600000001, 1600000011, NULL, NULL, 0, 0),
			(2, 'https://b.example/list', 0, 1600000002, 1600000012, 'b', 1710000002, 20, 1),
			(3, 'https://c.example/list', 1, 1600000003, 1600000013, 'c', 1710000003, 30, 2),
			(4, 'https://d.example/list', 1, 1600000004, 1600000014, 'd', 1710000004, 40, 3)`,
		`INSERT INTO info VALUES ('updated', '1720203654'), ('gravity_count', '123456')`)

	domains, adlists, info := readAll(t, path)

	u := func(s int64) time.Time { return time.Unix(s, 0).UTC() }
	wantDomains := []spec.DomainProperties{
		{PiholeID: 1, List: spec.ListAllow, Kind: spec.KindExact, Value: "allow.example", Enabled: true, Comment: "allow exact", Added: u(1700000001), Modified: u(1700000011)},
		{PiholeID: 2, List: spec.ListDeny, Kind: spec.KindExact, Value: "ads.example", Enabled: false, Comment: "", Added: u(1700000002), Modified: u(1700000012)},
		{PiholeID: 3, List: spec.ListAllow, Kind: spec.KindRegex, Value: `^ok\.example$`, Enabled: true, Comment: "", Added: u(1700000003), Modified: u(1700000013)},
		{PiholeID: 4, List: spec.ListDeny, Kind: spec.KindRegex, Value: `(\.|^)track\.example$`, Enabled: true, Comment: "deny regex", Added: u(1700000004), Modified: u(1700000014)},
	}
	if !slices.Equal(domains, wantDomains) {
		t.Errorf("Domains:\n got %+v\nwant %+v", domains, wantDomains)
	}
	for _, d := range domains {
		if d.Added.Location() != time.UTC || d.Modified.Location() != time.UTC {
			t.Errorf("domain %d times not UTC", d.PiholeID)
		}
	}

	wantStatus := []string{spec.AdlistNotDownloaded, spec.AdlistUpdated, spec.AdlistUnchanged, spec.AdlistFailedCached, spec.AdlistFailedNoCache}
	if len(adlists) != 5 {
		t.Fatalf("Adlists: %d rows, want 5", len(adlists))
	}
	for i, a := range adlists {
		id := int64(i + 1)
		if a.PiholeID != id {
			t.Errorf("adlist %d: id %d, not ordered by id", i, a.PiholeID)
		}
		if a.Status == nil || *a.Status != wantStatus[i] {
			t.Errorf("adlist %d: status %v, want %s", id, a.Status, wantStatus[i])
		}
		wantDomains := id * 10
		if id == 1 {
			wantDomains = 0
		}
		if a.Domains == nil || *a.Domains != wantDomains {
			t.Errorf("adlist %d: domains %v, want %d", id, a.Domains, wantDomains)
		}
		if !a.Added.Equal(u(1600000000+id)) || a.Added.Location() != time.UTC || !a.Modified.Equal(u(1600000010+id)) {
			t.Errorf("adlist %d: added %v modified %v", id, a.Added, a.Modified)
		}
		if a.URL != fmt.Sprintf("https://%c.example/list", 'a'+rune(i)) {
			t.Errorf("adlist %d: url %q", id, a.URL)
		}
	}
	if a := adlists[0]; a.Domains == nil || *a.Domains != 0 || a.LastUpdated != nil || a.Comment != "" || !a.Enabled {
		t.Errorf("adlist 1 (never downloaded): %+v", a)
	}
	if a := adlists[1]; a.Enabled || a.LastUpdated == nil || !a.LastUpdated.Equal(u(1710000002)) || a.LastUpdated.Location() != time.UTC || a.Comment != "b" {
		t.Errorf("adlist 2: %+v", a)
	}

	if info.GravityCount == nil || *info.GravityCount != 123456 {
		t.Errorf("Info.GravityCount %v, want 123456", info.GravityCount)
	}
	if info.Updated == nil || !info.Updated.Equal(u(1720203654)) || info.Updated.Location() != time.UTC {
		t.Errorf("Info.Updated %v, want %v UTC", info.Updated, u(1720203654))
	}
	if info.Version == nil || *info.Version != 15 {
		t.Errorf("Info.Version %v, want 15 (the fixture's)", info.Version)
	}
}

// Info reports unknown, not zero, when gravity.sh has not written its keys.
func TestInfoMissingKeysAreNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gravity.db")
	buildDB(t, path)
	_, _, info := readAll(t, path)
	if info.GravityCount != nil || info.Updated != nil {
		t.Errorf("Info on the bare template: %+v, want nil gravity_count and updated", info)
	}
}

func TestTypeMapping(t *testing.T) {
	cases := []struct {
		typ        int
		list, kind string
	}{
		{TypeAllowExact, spec.ListAllow, spec.KindExact},
		{TypeDenyExact, spec.ListDeny, spec.KindExact},
		{TypeAllowRegex, spec.ListAllow, spec.KindRegex},
		{TypeDenyRegex, spec.ListDeny, spec.KindRegex},
	}
	for i, c := range cases {
		if c.typ != i {
			t.Errorf("%s/%s is type %d, want %d", c.list, c.kind, c.typ, i)
		}
		got, err := TypeOf(c.list, c.kind)
		if err != nil || got != c.typ {
			t.Errorf("TypeOf(%s, %s) = %d, %v; want %d", c.list, c.kind, got, err, c.typ)
		}
	}
	if _, err := TypeOf("block", spec.KindExact); err == nil {
		t.Error("TypeOf accepted an unknown list")
	}
}

// TC-34: the fixture's insert trigger links a new domain to the default
// group (A-15).
func TestInsertTriggerLinksDefaultGroup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravity.db")
	buildDB(t, path)

	d, err := OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx, err := d.BeginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := tx.InsertDomain(ctx, TypeDenyExact, "x.example", "")
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := d.db.QueryContext(ctx, `SELECT group_id FROM domainlist_by_group WHERE domainlist_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var groups []int64
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			t.Fatal(err)
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups, []int64{0}) {
		t.Fatalf("domainlist_by_group for id %d: %v, want [0]; A-15 does not hold, escalate", id, groups)
	}

	var comment sql.NullString
	if err := d.db.QueryRowContext(ctx, `SELECT comment FROM domainlist WHERE id = ?`, id).Scan(&comment); err != nil {
		t.Fatal(err)
	}
	if comment.Valid {
		t.Errorf("empty comment stored as %q, want NULL (as list.sh stores it)", comment.String)
	}
}

// TC-35: UNIQUE(domain, type) is respected.
func TestUniqueDomainType(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravity.db")
	buildDB(t, path)

	d, err := OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx, err := d.BeginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	first, err := tx.InsertDomain(ctx, TypeDenyExact, "dup.example", "first")
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.InsertDomain(ctx, TypeDenyExact, "dup.example", "second")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second identical insert: %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Errorf("second identical insert: %v, want SQLite's constraint error", err)
	}
	other, err := tx.InsertDomain(ctx, TypeAllowExact, "dup.example", "")
	if err != nil {
		t.Fatalf("same domain, other type: %v", err)
	}
	found, err := tx.FindDomainAllTypes(ctx, "dup.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].PiholeID != first || found[1].PiholeID != other ||
		found[0].List != spec.ListDeny || found[1].List != spec.ListAllow ||
		found[0].Kind != spec.KindExact || found[1].Kind != spec.KindExact || found[0].Comment != "first" {
		t.Errorf("FindDomainAllTypes: %+v (ids %d, %d)", found, first, other)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx, err = d.BeginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.DeleteDomain(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := tx.DeleteDomain(ctx, first); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete of %d: %v, want ErrNotFound", first, err)
	}
	found, err = tx.FindDomainAllTypes(ctx, "dup.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].PiholeID != other {
		t.Errorf("after delete: %+v", found)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var links int
	if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM domainlist_by_group WHERE domainlist_id = ?`, first).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 0 {
		t.Errorf("group links of deleted id %d: %d, want 0 (tr_domainlist_delete)", first, links)
	}
}

func TestInsertDomainRejectsUnknownType(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravity.db")
	buildDB(t, path)
	d, err := OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx, err := d.BeginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.InsertDomain(ctx, 4, "x.example", ""); err == nil {
		t.Error("InsertDomain accepted type 4")
	}
}

// TC-36: 20 fresh opens reading 1,000 domains and 50 adlists, p95 under
// 50 ms; no query touches the gravity table (TestReadQueriesAvoidGravityTable,
// which also runs under the race detector).
func TestReadPerformance(t *testing.T) {
	if raceEnabled {
		t.Skip("the 50 ms p95 budget (NFR) is a wall-clock bound; race instrumentation makes it meaningless")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravity.db")
	buildDB(t, path,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000)
			INSERT INTO domainlist (type, domain, comment) SELECT i % 4, 'd' || i || '.example', 'c' || i FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 50)
			INSERT INTO adlist (address, number, status) SELECT 'https://l' || i || '.example/list', i * 100, i % 5 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
			INSERT INTO gravity (domain, adlist_id) SELECT 'g' || i || '.example', 1 + i % 50 FROM n`)

	// The 50 ms p95 is a wall-clock bound on a shared machine: a round that
	// misses it because the host was busy is repeated, and the test fails
	// only when every one of 3 rounds misses (review CR-15). NFR-1 itself is
	// measured on a real host at T1 (TC-122).
	var samples []time.Duration
	var p95 time.Duration
	for round := 1; round <= 3; round++ {
		samples = samples[:0]
		for range 20 {
			start := time.Now()
			d, err := OpenReadOnly(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			domains, err := d.Domains(ctx)
			if err != nil {
				t.Fatal(err)
			}
			adlists, err := d.Adlists(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			samples = append(samples, time.Since(start))
			if len(domains) != 1000 || len(adlists) != 50 {
				t.Fatalf("read %d domains, %d adlists", len(domains), len(adlists))
			}
		}
		slices.Sort(samples)
		p95 = samples[18] // the 19th of 20 samples
		t.Logf("round %d: p95 %v, max %v", round, p95, samples[19])
		if p95 < 50*time.Millisecond {
			break
		}
	}
	if p95 >= 50*time.Millisecond {
		t.Errorf("p95 %v in the last of 3 rounds, want under 50ms (all: %v)", p95, samples)
	}
}

// TC-36, second half: no read query's plan touches the gravity table.
func TestReadQueriesAvoidGravityTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravity.db")
	buildDB(t, path,
		`INSERT INTO domainlist (type, domain, comment) VALUES (1, 'd.example', 'c')`,
		`INSERT INTO adlist (address, number, status) VALUES ('https://l.example/list', 1, 1)`,
		`INSERT INTO gravity (domain, adlist_id) VALUES ('g.example', 1)`)
	d, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	gravity := regexp.MustCompile(`(?i)\bgravity\b`)
	for name, q := range map[string]string{"domains": domainsSQL, "adlists": adlistsSQL, "info": infoSQL, "find": findDomainSQL} {
		args := make([]any, strings.Count(q, "?"))
		for i := range args {
			args[i] = "x"
		}
		rows, err := d.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if len(plan) == 0 {
			t.Errorf("explain %s: empty plan", name)
		}
		for _, p := range plan {
			if gravity.MatchString(p) {
				t.Errorf("query %s touches the gravity table: %q", name, p)
			}
		}
	}
}

// TC-E9: no pooled handle survives a gravity swap.
func TestNoPooledHandleAcrossSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gravity.db")
	temp := filepath.Join(dir, "gravity.db_temp")
	buildDB(t, path, `INSERT INTO domainlist (domain) VALUES ('one.example')`)
	buildDB(t, temp, `INSERT INTO domainlist (domain) VALUES ('one.example'), ('two.example')`)

	if domains, _, _ := readAll(t, path); len(domains) != 1 {
		t.Fatalf("before the swap: %d domains, want 1", len(domains))
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
	if domains, _, _ := readAll(t, path); len(domains) != 2 {
		t.Fatalf("after the swap: %d domains, want 2", len(domains))
	}
}

// TC-ERR5: an unavailable gravity.db maps to ErrUnavailable, garbage to
// ErrInvalid, and nothing panics.
func TestUnavailableMapsToTypedError(t *testing.T) {
	ctx := context.Background()

	t.Run("missing", func(t *testing.T) {
		d, err := OpenReadOnly(ctx, filepath.Join(t.TempDir(), "gravity.db"))
		if err == nil {
			d.Close()
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("missing file: %v, want ErrUnavailable", err)
		}
		d, err = OpenReadWrite(ctx, filepath.Join(t.TempDir(), "gravity.db"))
		if err == nil {
			d.Close()
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("missing file, read-write: %v, want ErrUnavailable", err)
		}
	})

	t.Run("locked", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "gravity.db")
		buildDB(t, path, `INSERT INTO domainlist (domain) VALUES ('one.example')`)
		w, err := OpenReadWrite(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		conn, err := w.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
		defer conn.ExecContext(ctx, "ROLLBACK")

		start := time.Now()
		var readErr error
		d, err := OpenReadOnly(ctx, path)
		if err != nil {
			readErr = err
		} else {
			_, readErr = d.Domains(ctx)
			d.Close()
		}
		elapsed := time.Since(start)
		if !errors.Is(readErr, ErrUnavailable) {
			t.Errorf("locked file: %v, want ErrUnavailable", readErr)
		}
		if elapsed < 1900*time.Millisecond {
			t.Errorf("locked file failed after %v, before the 2 s busy timeout", elapsed)
		}
	})

	t.Run("hot journal", func(t *testing.T) {
		// A rollback journal left by an interrupted writer: a read-only
		// connection cannot roll it back (SQLITE_READONLY_ROLLBACK).
		src := filepath.Join(t.TempDir(), "gravity.db")
		buildDB(t, src, `INSERT INTO domainlist (domain) VALUES ('one.example')`)
		w, err := sql.Open("sqlite", "file:"+src+"?mode=rw&_pragma=cache_size(1)")
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		conn, err := w.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		defer conn.ExecContext(ctx, "ROLLBACK")
		if _, err := conn.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
			INSERT INTO domainlist (domain) SELECT 'hot' || i || '.example' FROM n`); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(t.TempDir(), "gravity.db")
		for _, suffix := range []string{"", "-journal"} {
			b, err := os.ReadFile(src + suffix)
			if err != nil {
				t.Fatalf("copy %s: %v", suffix, err)
			}
			if err := os.WriteFile(dst+suffix, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}

		d, err := OpenReadOnly(ctx, dst)
		if err == nil {
			_, err = d.Domains(ctx)
			d.Close()
		}
		var se *sqlite.Error
		if !errors.Is(err, ErrUnavailable) || !errors.As(err, &se) || se.Code() != sqlite3.SQLITE_READONLY_ROLLBACK {
			t.Errorf("hot journal: %v, want ErrUnavailable from SQLITE_READONLY_ROLLBACK", err)
		}
	})

	t.Run("garbage", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "gravity.db")
		if err := os.WriteFile(path, []byte(strings.Repeat("this is not a database\n", 200)), 0o644); err != nil {
			t.Fatal(err)
		}
		d, err := OpenReadOnly(ctx, path)
		if err == nil {
			_, err = d.Domains(ctx)
			d.Close()
		}
		if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrUnavailable) {
			t.Errorf("non-database file: %v, want ErrInvalid only", err)
		}
	})
}

// A path with URI metacharacters still opens the file it names.
func TestPathIsEscaped(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	buildDB(t, filepath.Join(plain, "gravity.db"), `INSERT INTO domainlist (domain) VALUES ('one.example')`)
	dir := filepath.Join(root, "a?b#c%25d")
	if err := os.Rename(plain, dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gravity.db")
	if domains, _, _ := readAll(t, path); len(domains) != 1 {
		t.Errorf("domains %v, want the one row", domains)
	}
}

// adlistSeed is two adlists, each with gravity rows; the schema's
// tr_adlist_add trigger links each to the default group.
var adlistSeed = []string{
	`INSERT INTO adlist (address) VALUES ('https://a.example/hosts'), ('https://b.example/hosts')`,
	`INSERT INTO gravity (domain, adlist_id) VALUES ('a1.example', 1), ('a2.example', 1), ('b1.example', 2)`,
}

// adlistRefs counts adlist, adlist_by_group and gravity rows for id.
func adlistRefs(t *testing.T, path string, id int64) [3]int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n [3]int
	err = db.QueryRow(`SELECT (SELECT count(*) FROM adlist WHERE id = ?), (SELECT count(*) FROM adlist_by_group WHERE adlist_id = ?),
		(SELECT count(*) FROM gravity WHERE adlist_id = ?)`, id, id, id).Scan(&n[0], &n[1], &n[2])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TC-147 steps 1-2 (FR-12, CON-2, A-58): a read-write handle enforces
// foreign keys, as pihole-FTL sqlite3 does on the v5 image, so a bare
// delete of a list gravity references fails, classified ErrInvalid, and
// rolls back with nothing changed.
func TestTC147_ReadWriteEnforcesForeignKeys(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravity.db")
	buildDB(t, path, adlistSeed...)
	d, err := OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var on int
	if err := d.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil || on != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, %v; want 1", on, err)
	}
	tx, err := d.BeginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.conn.ExecContext(ctx, `DELETE FROM adlist WHERE id = ?`, 1)
	if err == nil {
		tx.Rollback()
		t.Fatal("a bare adlist delete with gravity rows succeeded; foreign keys are not enforced")
	}
	if got := classify("delete adlist", err); !errors.Is(got, ErrInvalid) {
		t.Errorf("classify(%v) = %v, want ErrInvalid", err, got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := adlistRefs(t, path, 1); got != [3]int{1, 1, 2} {
		t.Errorf("after rollback adlist 1 refs %v, want [1 1 2]", got)
	}
}

// TC-147 steps 3-4 (FR-12, CON-2, A-58): DeleteAdlist removes the list's
// gravity rows, group links and the list, returns the gravity row count and
// leaves the other list intact; an unknown id is ErrNotFound with nothing
// deleted. AdlistByID reads one row, ErrNotFound when there is none.
func TestTC147_DeleteAdlist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravity.db")
	buildDB(t, path, adlistSeed...)
	d, err := OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	tx, err := d.BeginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, err := tx.AdlistByID(ctx, 1)
	if err != nil || a.PiholeID != 1 || a.URL != "https://a.example/hosts" || !a.Enabled {
		t.Errorf("AdlistByID(1) = %+v, %v", a, err)
	}
	if _, err := tx.AdlistByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("AdlistByID(999) = %v, want ErrNotFound", err)
	}
	n, err := tx.DeleteAdlist(ctx, 1)
	if err != nil || n != 2 {
		tx.Rollback()
		t.Fatalf("DeleteAdlist(1) = %d, %v; want 2 gravity rows", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := adlistRefs(t, path, 1); got != [3]int{} {
		t.Errorf("adlist 1 refs %v after delete, want none", got)
	}
	if got := adlistRefs(t, path, 2); got != [3]int{1, 1, 1} {
		t.Errorf("adlist 2 refs %v, want [1 1 1] (untouched)", got)
	}

	tx, err = d.BeginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := tx.DeleteAdlist(ctx, 999); !errors.Is(err, ErrNotFound) || n != 0 {
		t.Errorf("DeleteAdlist(999) = %d, %v; want ErrNotFound", n, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := adlistRefs(t, path, 2); got != [3]int{1, 1, 1} {
		t.Errorf("adlist 2 refs %v after the unknown-id delete, want [1 1 1]", got)
	}
}
