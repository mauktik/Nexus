package audit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/systemspec"
)

// ErrBadCursor means a Query cursor is not one the log handed out.
var ErrBadCursor = errors.New("invalid audit cursor")

// Query selects audit records. Zero fields do not filter.
type Query struct {
	After     time.Time // Only records strictly after
	Before    time.Time // Only records strictly before
	TokenName string
	Provider  string
	Action    string
	Outcome   string
	Limit     int    // Zero means systemspec.DefaultLimit
	Cursor    string // From a previous page's NextCursor
}

func (q Query) match(r *systemspec.AuditRecord) bool {
	return (q.After.IsZero() || r.Time.After(q.After)) &&
		(q.Before.IsZero() || r.Time.Before(q.Before)) &&
		(q.TokenName == "" || r.TokenName == q.TokenName) &&
		(q.Provider == "" || r.Provider == q.Provider) &&
		(q.Action == "" || r.Action == q.Action) &&
		(q.Outcome == "" || r.Outcome == q.Outcome)
}

// cursorRE matches the record IDs opid issues, which are the cursors.
var cursorRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Query returns matching records newest first, across the live file and
// every rotated one. The cursor is the ID of the last record of the
// previous page; IDs increase in write order, so the next page holds the
// matching records with smaller IDs. A line that does not parse, such as a
// final line a crash cut short, is skipped with a warning.
func (l *Log) Query(q Query) (apitypes.List[systemspec.AuditRecord], error) {
	if q.Cursor != "" && !cursorRE.MatchString(q.Cursor) {
		return apitypes.List[systemspec.AuditRecord]{}, ErrBadCursor
	}
	limit := q.Limit
	if limit <= 0 {
		limit = systemspec.DefaultLimit
	}
	srcs, err := l.sources()
	if err != nil {
		return apitypes.List[systemspec.AuditRecord]{}, err
	}
	defer func() {
		for _, s := range srcs {
			_ = s.f.Close() // Read-only
		}
	}()

	var out []systemspec.AuditRecord
	for _, s := range srcs {
		recs := l.readSource(s)
		for i := len(recs) - 1; i >= 0; i-- {
			r := &recs[i]
			if q.Cursor != "" && r.ID >= q.Cursor {
				continue
			}
			if !q.match(r) {
				continue
			}
			if len(out) == limit {
				return apitypes.List[systemspec.AuditRecord]{Items: out, NextCursor: out[limit-1].ID}, nil
			}
			out = append(out, *r)
		}
	}
	return apitypes.List[systemspec.AuditRecord]{Items: out}, nil
}

// source is one audit file opened for a query, newest first.
type source struct {
	f    *os.File
	name string
	size int64 // Bytes to read; for the live file, its size when listed
	gz   bool
}

// sources writes the expired aggregates, then opens the live file and every
// rotated file under the lock, so a rotation or prune during the read
// cannot hide records: an open descriptor survives rename and unlink.
func (l *Log) sources() ([]source, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.flushExpired(l.now())
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("audit query: %w", err)
	}
	var names []string
	gz := map[string]bool{}
	for _, e := range entries {
		if _, ok := rotatedTime(e.Name()); ok {
			names = append(names, e.Name())
			gz[e.Name()] = strings.HasSuffix(e.Name(), gzSuffix)
		}
	}
	// A compressed file is complete, so its uncompressed original, if a
	// crash left it, is skipped.
	names = slices.DeleteFunc(names, func(n string) bool { return !gz[n] && gz[n+".gz"] })
	slices.SortFunc(names, func(a, b string) int {
		return strings.Compare(strings.TrimSuffix(b, ".gz"), strings.TrimSuffix(a, ".gz"))
	})
	names = append([]string{FileName}, names...)

	var srcs []source
	for _, n := range names {
		f, err := os.Open(filepath.Join(l.dir, n))
		if errors.Is(err, os.ErrNotExist) && n != FileName {
			continue
		}
		if err == nil {
			var fi os.FileInfo
			if fi, err = f.Stat(); err == nil {
				srcs = append(srcs, source{f: f, name: n, size: fi.Size(), gz: gz[n]})
				continue
			}
			_ = f.Close() // Read-only
		}
		for _, s := range srcs {
			_ = s.f.Close() // Read-only
		}
		return nil, fmt.Errorf("audit query: %w", err)
	}
	return srcs, nil
}

// readSource parses one file's records in file order. A file that cannot be
// read to its end yields the records before the fault, with a warning: one
// damaged file must not hide the rest of the log.
func (l *Log) readSource(s source) []systemspec.AuditRecord {
	var r io.Reader = io.LimitReader(s.f, s.size)
	if s.gz {
		zr, err := gzip.NewReader(s.f)
		if err != nil {
			l.log.Warn("skipping unreadable audit file", "file", s.name, "error", err.Error())
			return nil
		}
		defer zr.Close()
		r = zr
	}
	var recs []systemspec.AuditRecord
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var rec systemspec.AuditRecord
			if uerr := json.Unmarshal(line, &rec); uerr != nil {
				l.log.Warn("skipping unparsable audit line", "file", s.name, "line", n,
					"final", errors.Is(err, io.EOF), "error", uerr.Error())
			} else {
				recs = append(recs, rec)
			}
		}
		if errors.Is(err, io.EOF) {
			return recs
		}
		if err != nil {
			l.log.Warn("audit file read stopped early", "file", s.name, "line", n, "error", err.Error())
			return recs
		}
	}
}
