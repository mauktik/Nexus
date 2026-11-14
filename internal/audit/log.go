// Package audit is the append-only audit log of nexusd (FR-24): one JSON
// object per line in audit.jsonl, appended under a mutex and fsynced before
// Write returns, rotated by size into gzip files that are deleted only once
// older than the retention period, and queried newest first.
package audit

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/opid"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/systemspec"
)

// DefaultDir is the audit directory of an installed nexusd.
const DefaultDir = "/var/log/nexus"

// FileName is the live audit file in the audit directory.
const FileName = "audit.jsonl"

// Tunables (A-54, A-47).
const (
	DefaultRotateBytes   = 16 << 20
	DefaultRetentionDays = 90
	// MaxRetentionDays bounds Options.RetentionDays (100 years): a larger
	// value is kept as this, which is "forever" for an audit log.
	MaxRetentionDays       = 36500
	DefaultAggregateWindow = time.Minute
	// MaxAggregates bounds the sources with an open aggregate; past it the
	// oldest is written early (NFR-4).
	MaxAggregates = 4096
)

// Record outcomes besides the operation states that 3.2 records.
const (
	OutcomeRequested = "requested"
	OutcomeDenied    = "denied"
	OutcomeRefused   = "refused"
	OutcomeReplayed  = "replayed"
	// OutcomeStored is an upload whose body was stored. It follows the
	// upload's requested record and names it in RequestID.
	OutcomeStored = "stored"
)

// Redacted replaces the value of every sensitive path.
const Redacted = "[REDACTED]"

// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("audit log closed")

// SensitivePaths is the redactor hook: it returns the parameter paths of an
// action that must be redacted beyond those the caller declares, such as the
// catalog-marked parameters of a script.
type SensitivePaths func(provider, action string, params json.RawMessage) []string

// Options configures Open.
type Options struct {
	Dir           string // Empty means DefaultDir
	Host          string // Written into every record
	RetentionDays int    // Zero means DefaultRetentionDays
	// RotateBytes is the live file size that triggers rotation. Zero means
	// DefaultRotateBytes.
	RotateBytes int64
	// AggregateWindow is how long one aggregate of rate-limited denials
	// collects. Zero means DefaultAggregateWindow, auth.FailureWindow.
	AggregateWindow time.Duration
	SensitivePaths  SensitivePaths   // Nil means only declared paths
	Now             func() time.Time // Timestamps and retention; nil means time.Now
	Logger          *slog.Logger     // Nil means slog.Default()
}

// Log is the audit log. It is safe for concurrent use and runs nothing in
// the background: rotation, retention and the flushing of aggregates happen
// inside the calls that need them (NFR-1).
type Log struct {
	dir         string
	host        string
	retention   time.Duration
	rotateBytes int64
	window      time.Duration
	sensitive   SensitivePaths
	now         func() time.Time
	log         *slog.Logger

	unhealthy atomic.Bool

	mu          sync.Mutex
	f           *os.File // Nil after Close
	size        int64
	damaged     bool       // A failed write may have left a partial line
	lastRotated time.Time  // Newest rotation timestamp, so names stay ordered
	aggs        *list.List // Front is newest; values are *aggregate
	bySource    map[string]*list.Element
}

// aggregate counts the rate-limited denials of one source in one window.
type aggregate struct {
	source  string
	surface string
	start   time.Time
	count   int
}

// Open opens or creates the audit log in opts.Dir (mode 0700), finishes any
// rotation a crash interrupted, and applies retention.
func Open(opts Options) (*Log, error) {
	l := &Log{
		dir: opts.Dir, host: opts.Host, rotateBytes: opts.RotateBytes, window: opts.AggregateWindow,
		sensitive: opts.SensitivePaths, now: opts.Now, log: opts.Logger,
		aggs: list.New(), bySource: map[string]*list.Element{},
	}
	if l.dir == "" {
		l.dir = DefaultDir
	}
	days := opts.RetentionDays
	if days <= 0 {
		days = DefaultRetentionDays
	}
	// A larger count would overflow the Duration and wrap negative, which
	// puts the retention cutoff in the future and deletes the whole history.
	days = min(days, MaxRetentionDays)
	l.retention = time.Duration(days) * 24 * time.Hour
	if l.rotateBytes <= 0 {
		l.rotateBytes = DefaultRotateBytes
	}
	if l.window <= 0 {
		l.window = DefaultAggregateWindow
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.log == nil {
		l.log = slog.Default()
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit directory: %w", err)
	}
	if err := l.recover(); err != nil {
		return nil, err
	}
	if err := l.openLive(); err != nil {
		return nil, err
	}
	l.observePersisted()
	if err := l.Prune(); err != nil {
		// Retention runs again after every rotation; an old file kept a
		// little longer loses nothing.
		l.log.Error("audit retention failed", "dir", l.dir, "error", err.Error())
	}
	return l, nil
}

// observePersisted seeds the ID generator past the largest ID the newest
// non-empty audit file holds, so records written after a restart sort after
// those before it even if the clock stepped back meanwhile (Query's cursor
// relies on that order). File order is ID order, so the live file's last
// record is enough; only when it cannot be read is every record of the
// newest non-empty file parsed (CR-94).
func (l *Log) observePersisted() {
	if id := l.lastLiveID(); id != "" {
		opid.Observe(id)
		return
	}
	srcs, err := l.sources()
	if err != nil {
		l.log.Warn("cannot read the audit log to seed record IDs", "error", err.Error())
		return
	}
	defer func() {
		for _, s := range srcs {
			_ = s.f.Close() // Read-only
		}
	}()
	for _, s := range srcs {
		var newest string
		if err := l.scan(context.Background(), s, func(r systemspec.AuditRecord) { newest = max(newest, r.ID) }); err != nil {
			l.log.Warn("cannot read an audit file to seed record IDs", "file", s.name, "error", err.Error())
			continue
		}
		if newest != "" {
			opid.Observe(newest)
			return
		}
	}
}

// maxTailBytes bounds the backward read for the live file's last line.
const maxTailBytes = 1 << 20

// lastLiveID is the ID of the record on the live file's last line, read
// backwards from the end. openLive has ended a line a crash cut short, so
// the file ends in a newline. It is "" when the file is empty or the line
// is not a record with an ID.
func (l *Log) lastLiveID() string {
	f, err := os.Open(filepath.Join(l.dir, FileName))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }() // Read-only
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	const step = 4 << 10
	var tail []byte
	for off := fi.Size(); off > 0 && len(tail) < maxTailBytes; {
		n := min(step, off)
		off -= n
		buf := make([]byte, n, int(n)+len(tail))
		if _, err := f.ReadAt(buf, off); err != nil {
			return ""
		}
		tail = append(buf, tail...)
		body := bytes.TrimRight(tail, "\n")
		i := bytes.LastIndexByte(body, '\n')
		if i < 0 && off > 0 {
			continue
		}
		var rec systemspec.AuditRecord
		if json.Unmarshal(body[i+1:], &rec) != nil {
			return ""
		}
		return rec.ID
	}
	return ""
}

// openLive opens audit.jsonl for appending with mode 0600, and ends a final
// line a crash cut short, so the next record starts on a line of its own.
func (l *Log) openLive() error {
	p := filepath.Join(l.dir, FileName)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("audit log mode: %w", err), f.Close())
	}
	fi, err := f.Stat()
	if err != nil {
		return errors.Join(fmt.Errorf("audit log: %w", err), f.Close())
	}
	l.f, l.size, l.damaged = f, fi.Size(), true
	if err := l.repair(); err != nil {
		l.f = nil
		return errors.Join(err, f.Close())
	}
	return nil
}

// repair appends a newline when the live file does not end in one. It runs
// after a failed write, which may have appended part of a line.
func (l *Log) repair() error {
	if !l.damaged {
		return nil
	}
	fi, err := l.f.Stat()
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	l.size = fi.Size()
	if l.size > 0 {
		last := make([]byte, 1)
		r, err := os.Open(l.f.Name())
		if err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		_, rerr := r.ReadAt(last, l.size-1)
		if err := errors.Join(rerr, r.Close()); err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		if last[0] != '\n' {
			n, err := l.f.Write([]byte{'\n'})
			l.size += int64(n)
			if err == nil {
				err = l.f.Sync()
			}
			if err != nil {
				return fmt.Errorf("audit log: %w", err)
			}
		}
	}
	l.damaged = false
	return nil
}

// Write assigns the record its ID, host and time, redacts its Params, and
// appends it durably: it returns only after fsync. It returns the record's
// ID. Params are redacted at the paths in sensitive plus those of the
// SensitivePaths hook; Params that cannot be parsed are an error, never
// written as they are.
//
// A failed write of any record other than a requested one sets the
// audit-health flag; a successful requested write clears it. The caller
// decides what a failure means: a requested record fails the request
// closed, any other is logged.
func (l *Log) Write(rec systemspec.AuditRecord, sensitive []string) (string, error) {
	id, err := l.write(rec, sensitive)
	switch {
	case err != nil && rec.Outcome != OutcomeRequested:
		l.unhealthy.Store(true)
	case err == nil && rec.Outcome == OutcomeRequested:
		l.unhealthy.Store(false)
	}
	return id, err
}

func (l *Log) write(rec systemspec.AuditRecord, sensitive []string) (string, error) {
	if len(rec.Params) > 0 {
		p, err := l.Redact(rec.Provider, rec.Action, rec.Params, sensitive)
		if err != nil {
			return "", err
		}
		rec.Params = p
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return "", ErrClosed
	}
	l.flushExpired(l.now())
	return l.append(rec)
}

// append writes one record; l.mu is held.
func (l *Log) append(rec systemspec.AuditRecord) (string, error) {
	if err := l.repair(); err != nil {
		return "", err
	}
	// The ID is assigned under the lock, so file order is ID order.
	rec.ID = opid.New()
	rec.Host = l.host
	if rec.Time.IsZero() {
		rec.Time = l.now()
	}
	rec.Time = rec.Time.UTC()
	line, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("encode audit record: %w", err)
	}
	line = append(line, '\n')
	if l.size > 0 && l.size+int64(len(line)) > l.rotateBytes {
		if err := l.rotate(); err != nil {
			// The record is still written durably; the live file just
			// grows past the rotation size until a rotation succeeds.
			l.log.Error("audit rotation failed", "dir", l.dir, "error", err.Error())
		}
	}
	n, err := l.f.Write(line)
	l.size += int64(n)
	if err == nil {
		err = l.f.Sync()
	}
	if err != nil {
		l.damaged = true
		return "", fmt.Errorf("write audit record: %w", err)
	}
	return rec.ID, nil
}

// RateLimited counts one denial of a request refused because its source is
// rate limited (A-47). The denials of a source collapse into one record per
// window, with outcome denied, problem rate-limited and the count, so a
// flood from one host costs one record per window, not one per attempt.
// The record is written once its window has ended, by the next call that
// touches the log, or by Close. source names the group the limiter counts:
// an IPv4 address or an IPv6 /64.
func (l *Log) RateLimited(source, surface string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	now := l.now()
	l.flushExpired(now)
	if el, ok := l.bySource[source]; ok {
		el.Value.(*aggregate).count++
		return
	}
	l.bySource[source] = l.aggs.PushFront(&aggregate{source: source, surface: surface, start: now, count: 1})
	for l.aggs.Len() > MaxAggregates {
		l.flush(l.aggs.Back())
	}
}

// flushExpired writes every aggregate whose window has ended. Windows have
// one length and start in call order, so the oldest are at the back.
func (l *Log) flushExpired(now time.Time) {
	for el := l.aggs.Back(); el != nil; el = l.aggs.Back() {
		if now.Before(el.Value.(*aggregate).start.Add(l.window)) {
			return
		}
		l.flush(el)
	}
}

// flush writes one aggregate and forgets it. Nothing waits on it, so a
// failure is logged and sets the audit-health flag.
func (l *Log) flush(el *list.Element) {
	a := el.Value.(*aggregate)
	l.aggs.Remove(el)
	delete(l.bySource, a.source)
	_, err := l.append(systemspec.AuditRecord{
		Time: a.start, Source: a.source, Surface: a.surface,
		Outcome: OutcomeDenied, Problem: string(problem.RateLimited), Count: a.count,
	})
	if err != nil {
		l.unhealthy.Store(true)
		l.log.Error("audit write failed", "outcome", OutcomeDenied, "problem", string(problem.RateLimited),
			"count", a.count, "error", err.Error())
	}
}

// Healthy reports whether every record since the last successful requested
// record was written. A failed state-change, refused, replayed or denied
// write clears it until the next requested record is written.
func (l *Log) Healthy() bool { return !l.unhealthy.Load() }

// Close writes the open aggregates and closes the file. Later writes fail
// with ErrClosed; closing twice is a no-op.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	for el := l.aggs.Back(); el != nil; el = l.aggs.Back() {
		l.flush(el)
	}
	err := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("close audit log: %w", err)
	}
	return nil
}

// Redact returns params with the value at each sensitive path, and at each
// path the SensitivePaths hook names, replaced by Redacted. It is the one
// redactor: the audit record and OpRecord.Input (3.2) both come from it.
func (l *Log) Redact(provider, action string, params json.RawMessage, sensitive []string) (json.RawMessage, error) {
	paths := sensitive
	if l.sensitive != nil {
		paths = append(paths[:len(paths):len(paths)], l.sensitive(provider, action, params)...)
	}
	return Redact(params, paths)
}

// Redact replaces the value at each dot-separated path of params ("secret",
// "nested.key") with Redacted. The key stays, so a reader sees that a value
// was given. A path through an array applies to each element. Params that
// are not valid JSON are an error.
func Redact(params json.RawMessage, paths []string) (json.RawMessage, error) {
	if len(bytes.TrimSpace(params)) == 0 {
		return params, nil
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("redact parameters: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("redact parameters: trailing data after the JSON value")
	}
	if len(paths) == 0 {
		return params, nil
	}
	for _, p := range paths {
		if p != "" {
			redactPath(v, strings.Split(p, "."))
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("redact parameters: %w", err)
	}
	return out, nil
}

func redactPath(v any, path []string) {
	switch t := v.(type) {
	case map[string]any:
		child, ok := t[path[0]]
		if !ok {
			return
		}
		if len(path) == 1 {
			t[path[0]] = Redacted
			return
		}
		redactPath(child, path[1:])
	case []any:
		for _, e := range t {
			redactPath(e, path)
		}
	}
}
