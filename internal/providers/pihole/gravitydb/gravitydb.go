// Package gravitydb reads and writes Pi-hole v5's gravity.db (schema version
// 15, V-5) with modernc.org/sqlite, a pure-Go SQLite with no cgo and no JIT,
// so it runs under nexusd's MemoryDenyWriteExecute sandbox.
//
// nexusd only ever opens the database read-only (OpenReadOnly), and opens it
// afresh for every read: gravity.sh replaces the file by renaming a new one
// over it (V-7), so a pooled handle would keep reading the old inode. The
// read-write open (OpenReadWrite) is for the privileged helper only.
//
// Every query is fixed SQL with ? parameters; no caller text is ever spliced
// into a statement.
package gravitydb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

var (
	// ErrUnavailable means gravity.db is missing, locked past the busy
	// timeout, or has a hot journal a read-only connection cannot roll back.
	// Callers report it as dependency-unavailable.
	ErrUnavailable = errors.New("gravity.db unavailable")
	// ErrInvalid means the file is not a usable gravity.db: not a SQLite
	// database, corrupt, or holding values outside Pi-hole's encodings.
	ErrInvalid = errors.New("gravity.db invalid")
	// ErrConflict means an insert hit UNIQUE(domain, type).
	ErrConflict = errors.New("gravity.db constraint conflict")
	// ErrNotFound means a delete matched no row.
	ErrNotFound = errors.New("gravity.db row not found")
)

// Busy timeouts in milliseconds: nexusd's reads give up sooner than the
// helper's writes.
const (
	readBusyTimeoutMS  = 2000
	writeBusyTimeoutMS = 5000
)

// domainlist.type values (V-5).
const (
	TypeAllowExact = 0
	TypeDenyExact  = 1
	TypeAllowRegex = 2
	TypeDenyRegex  = 3
)

var domainTypes = [...]struct{ list, kind string }{
	TypeAllowExact: {spec.ListAllow, spec.KindExact},
	TypeDenyExact:  {spec.ListDeny, spec.KindExact},
	TypeAllowRegex: {spec.ListAllow, spec.KindRegex},
	TypeDenyRegex:  {spec.ListDeny, spec.KindRegex},
}

// adlist.status values 0 to 4 (V-5).
var adlistStatuses = [...]string{
	spec.AdlistNotDownloaded,
	spec.AdlistUpdated,
	spec.AdlistUnchanged,
	spec.AdlistFailedCached,
	spec.AdlistFailedNoCache,
}

// TypeOf returns the domainlist.type for a list and kind.
func TypeOf(list, kind string) (int, error) {
	for t, lk := range domainTypes {
		if lk.list == list && lk.kind == kind {
			return t, nil
		}
	}
	return 0, fmt.Errorf("no domainlist type for list %q kind %q", list, kind)
}

const (
	domainColumns = `id, type, domain, enabled, date_added, date_modified, comment`
	domainsSQL    = `SELECT ` + domainColumns + ` FROM domainlist ORDER BY id`
	findDomainSQL = `SELECT ` + domainColumns + ` FROM domainlist WHERE domain = ? ORDER BY id`
	adlistsSQL    = `SELECT id, address, enabled, date_added, date_modified, comment, date_updated, number, status FROM adlist ORDER BY id`
	infoSQL       = `SELECT property, value FROM info WHERE property IN ('version', 'gravity_count', 'updated')`
	insertSQL     = `INSERT INTO domainlist (type, domain, comment) VALUES (?, ?, ?)`
	deleteSQL     = `DELETE FROM domainlist WHERE id = ?`
)

// DB is one open gravity.db. Close it when done; do not keep it across
// reads (V-7).
type DB struct {
	db *sql.DB
}

// Info is the gravity run summary from the info table. A key gravity.sh has
// not written is nil.
type Info struct {
	Version      *int64
	GravityCount *int64
	Updated      *time.Time
}

// OpenReadOnly opens path read-only for nexusd: mode=ro, query_only and a
// 2 s busy timeout, in the rollback-journal mode Pi-hole uses, so the open
// creates no -journal, -wal or -shm file.
func OpenReadOnly(ctx context.Context, path string) (*DB, error) {
	return open(ctx, path, "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout("+strconv.Itoa(readBusyTimeoutMS)+")")
}

// OpenReadWrite opens an existing path for writing, with a 5 s busy timeout.
// Only the privileged helper calls it.
func OpenReadWrite(ctx context.Context, path string) (*DB, error) {
	return open(ctx, path, "mode=rw&_pragma=busy_timeout("+strconv.Itoa(writeBusyTimeoutMS)+")")
}

func open(ctx context.Context, path, query string) (*DB, error) {
	// mode=ro and mode=rw never create the file, but a missing one is only
	// reported at the first query; check up front.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	db, err := sql.Open("sqlite", "file:"+escapePath(path)+"?"+query)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, classify("open "+path, err)
	}
	return &DB{db: db}, nil
}

// escapePath percent-encodes the characters that would end or alter the path
// part of a SQLite URI filename; SQLite decodes %HH in the path.
func escapePath(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
}

// Close closes the database.
func (d *DB) Close() error {
	return d.db.Close()
}

// classify wraps a SQLite error in the package's typed errors.
func classify(op string, err error) error {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return fmt.Errorf("%s: %w", op, err)
	}
	code := se.Code()
	switch {
	case code == sqlite3.SQLITE_READONLY_ROLLBACK,
		code&0xff == sqlite3.SQLITE_BUSY,
		code&0xff == sqlite3.SQLITE_CANTOPEN:
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
	case code&0xff == sqlite3.SQLITE_NOTADB, code&0xff == sqlite3.SQLITE_CORRUPT:
		return fmt.Errorf("%w: %s: %w", ErrInvalid, op, err)
	case code == sqlite3.SQLITE_CONSTRAINT_UNIQUE:
		return fmt.Errorf("%w: %s: %w", ErrConflict, op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func utc(unix int64) time.Time {
	return time.Unix(unix, 0).UTC()
}

// querier is what both *sql.DB and *sql.Conn offer for reads.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryDomains(ctx context.Context, q querier, op, query string, args ...any) ([]spec.DomainProperties, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classify(op, err)
	}
	defer rows.Close()
	var out []spec.DomainProperties
	for rows.Next() {
		var (
			id, typ, added, modified int64
			domain                   string
			enabled                  bool
			comment                  sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &enabled, &added, &modified, &comment); err != nil {
			return nil, classify(op, err)
		}
		if typ < 0 || typ >= int64(len(domainTypes)) {
			return nil, fmt.Errorf("%w: %s: domainlist id %d has type %d", ErrInvalid, op, id, typ)
		}
		lk := domainTypes[typ]
		out = append(out, spec.DomainProperties{
			PiholeID: id,
			List:     lk.list,
			Kind:     lk.kind,
			Value:    domain,
			Enabled:  enabled,
			Comment:  comment.String,
			Added:    utc(added),
			Modified: utc(modified),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, classify(op, err)
	}
	return out, nil
}

// Domains returns every domainlist row, ordered by id.
func (d *DB) Domains(ctx context.Context) ([]spec.DomainProperties, error) {
	return queryDomains(ctx, d.db, "read domainlist", domainsSQL)
}

// Adlists returns every adlist row, ordered by id. A status outside 0 to 4
// is reported as nil (unknown).
func (d *DB) Adlists(ctx context.Context) ([]spec.AdlistProperties, error) {
	const op = "read adlist"
	rows, err := d.db.QueryContext(ctx, adlistsSQL)
	if err != nil {
		return nil, classify(op, err)
	}
	defer rows.Close()
	var out []spec.AdlistProperties
	for rows.Next() {
		var (
			id, added, modified  int64
			address              string
			enabled              bool
			comment              sql.NullString
			updated, number, sts sql.NullInt64
		)
		if err := rows.Scan(&id, &address, &enabled, &added, &modified, &comment, &updated, &number, &sts); err != nil {
			return nil, classify(op, err)
		}
		a := spec.AdlistProperties{
			PiholeID: id,
			URL:      address,
			Enabled:  enabled,
			Comment:  comment.String,
			Added:    utc(added),
			Modified: utc(modified),
		}
		if number.Valid {
			n := number.Int64
			a.Domains = &n
		}
		if sts.Valid && sts.Int64 >= 0 && sts.Int64 < int64(len(adlistStatuses)) {
			s := adlistStatuses[sts.Int64]
			a.Status = &s
		}
		if updated.Valid {
			u := utc(updated.Int64)
			a.LastUpdated = &u
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(op, err)
	}
	return out, nil
}

// Info returns the schema version, gravity_count and updated from the info
// table.
func (d *DB) Info(ctx context.Context) (Info, error) {
	const op = "read info"
	rows, err := d.db.QueryContext(ctx, infoSQL)
	if err != nil {
		return Info{}, classify(op, err)
	}
	defer rows.Close()
	var info Info
	for rows.Next() {
		var property, value string
		if err := rows.Scan(&property, &value); err != nil {
			return Info{}, classify(op, err)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return Info{}, fmt.Errorf("%w: %s: info %s %q is not an integer", ErrInvalid, op, property, value)
		}
		switch property {
		case "version":
			info.Version = &n
		case "gravity_count":
			info.GravityCount = &n
		case "updated":
			u := utc(n)
			info.Updated = &u
		}
	}
	if err := rows.Err(); err != nil {
		return Info{}, classify(op, err)
	}
	return info, nil
}

// Tx is a write transaction begun with BEGIN IMMEDIATE, so the conflict
// check and the write it guards see the same database (CON-7). End it with
// Commit or Rollback; Rollback after Commit is a no-op.
type Tx struct {
	conn *sql.Conn
	done bool
}

// BeginImmediate starts a write transaction on a dedicated connection.
func (d *DB) BeginImmediate(ctx context.Context) (*Tx, error) {
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return nil, classify("begin", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		return nil, classify("begin immediate", err)
	}
	return &Tx{conn: conn}, nil
}

// FindDomainAllTypes returns the domainlist rows whose domain is value, of
// any type, ordered by id.
func (t *Tx) FindDomainAllTypes(ctx context.Context, value string) ([]spec.DomainProperties, error) {
	return queryDomains(ctx, t.conn, "find domain", findDomainSQL, value)
}

// InsertDomain inserts a domainlist row and returns its id. An empty comment
// is stored as NULL, as list.sh stores it. The schema's tr_domainlist_add
// trigger links the row to the default group (A-15).
func (t *Tx) InsertDomain(ctx context.Context, typ int, value, comment string) (int64, error) {
	if typ < 0 || typ >= len(domainTypes) {
		return 0, fmt.Errorf("insert domain: unknown domainlist type %d", typ)
	}
	var c sql.NullString
	if comment != "" {
		c = sql.NullString{String: comment, Valid: true}
	}
	res, err := t.conn.ExecContext(ctx, insertSQL, typ, value, c)
	if err != nil {
		return 0, classify("insert domain", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, classify("insert domain", err)
	}
	return id, nil
}

// DeleteDomain deletes the domainlist row with id; ErrNotFound when there is
// none. The schema's tr_domainlist_delete trigger removes its group links.
func (t *Tx) DeleteDomain(ctx context.Context, id int64) error {
	res, err := t.conn.ExecContext(ctx, deleteSQL, id)
	if err != nil {
		return classify("delete domain", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return classify("delete domain", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: domainlist id %d", ErrNotFound, id)
	}
	return nil
}

// Commit commits the transaction and releases its connection.
func (t *Tx) Commit(ctx context.Context) error {
	if t.done {
		return errors.New("commit: transaction already ended")
	}
	if _, err := t.conn.ExecContext(ctx, "COMMIT"); err != nil {
		// The transaction is still open; leave it for Rollback.
		return classify("commit", err)
	}
	t.done = true
	return t.conn.Close()
}

// Rollback rolls the transaction back and releases its connection. If the
// ROLLBACK fails, the connection is discarded rather than returned to the
// pool with a transaction open.
func (t *Tx) Rollback() error {
	if t.done {
		return nil
	}
	t.done = true
	// Cleanup runs even when the caller's context is already cancelled.
	_, rbErr := t.conn.ExecContext(context.Background(), "ROLLBACK")
	if rbErr != nil {
		_ = t.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	closeErr := t.conn.Close()
	if rbErr != nil {
		return classify("rollback", rbErr)
	}
	return closeErr
}
