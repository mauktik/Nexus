// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package oplog is the capped, offset-addressed output log of an operation
// (FR-5). The supervisor process writes it and nexusd reads it, so the only
// shared state is the directory: numbered segment files of one JSON frame
// per line, each line appended with a single write.
//
// Offsets are absolute positions in the raw byte stream of the operation and
// never reset. When the segments together exceed the cap, whole segments are
// dropped oldest first and a reader asking for a dropped offset gets a
// truncation marker instead.
package oplog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/apitypes"
)

// Tunables (A-54, limits.outputCapBytes).
const (
	DefaultCapBytes     = 10 << 20
	DefaultSegmentBytes = 1 << 20
	DefaultPollInterval = 250 * time.Millisecond
	// MaxFrameBytes bounds the raw bytes of one frame, so a single line never
	// outgrows a segment and a large chunk spreads over several segments.
	MaxFrameBytes = 32 << 10
)

// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("output log closed")

// Options configures Open.
type Options struct {
	// CapBytes bounds the total size of the segment files as stored: the
	// JSON-encoded frames, which are larger than the raw output (control
	// bytes cost up to six bytes each). The active segment is never dropped,
	// so the log can exceed the cap by up to one segment. Zero means
	// DefaultCapBytes.
	CapBytes int64
	// SegmentBytes is the size at which a new segment starts. Zero means
	// DefaultSegmentBytes, or a quarter of the cap when that is smaller, so
	// a small cap is honoured to within a quarter of itself plus one frame.
	SegmentBytes int64
	Now          func() time.Time // Frame timestamps; nil means time.Now
}

// diskFrame is one line of a segment. A frame whose raw bytes are valid UTF-8
// stores them in d, which is then exactly the wire frame. Otherwise the raw
// bytes go in b and the reader derives d, so a read can start at any raw
// offset inside the frame.
type diskFrame struct {
	O int64     `json:"o"`
	T time.Time `json:"t"`
	S string    `json:"s"`
	D string    `json:"d,omitempty"`
	B []byte    `json:"b,omitempty"`
}

func (f *diskFrame) raw() []byte {
	if f.B != nil {
		return f.B
	}
	return []byte(f.D)
}

func (f *diskFrame) rawLen() int64 {
	if f.B != nil {
		return int64(len(f.B))
	}
	return int64(len(f.D))
}

type segment struct {
	seq  int
	size int64
}

// Writer appends output to the log in dir. It is safe for concurrent use,
// so stdout and stderr can be copied from separate goroutines. Only one
// Writer may have a directory open at a time.
type Writer struct {
	mu       sync.Mutex
	dir      string
	capBytes int64
	segBytes int64
	now      func() time.Time

	segs  []segment // Oldest first; the last one is active
	total int64     // Sum of segs sizes
	f     *os.File  // Active segment, nil before the first frame
	end   int64     // Raw bytes accepted and framed

	// pending holds the trailing bytes of the last write when they are the
	// start of a multibyte rune, so a rune split across writes is framed
	// whole.
	pending       []byte
	pendingStream string

	closed bool
	err    error // Sticky I/O error
}

// Open opens the log in dir, creating dir if needed. An existing log is
// continued: offsets resume after its last complete frame, and a partial
// final line left by a killed writer is cut off.
func Open(dir string, opts Options) (*Writer, error) {
	if opts.CapBytes < 0 || opts.SegmentBytes < 0 {
		return nil, fmt.Errorf("oplog: negative cap %d or segment size %d", opts.CapBytes, opts.SegmentBytes)
	}
	w := &Writer{dir: dir, capBytes: opts.CapBytes, segBytes: opts.SegmentBytes, now: opts.Now}
	if w.capBytes == 0 {
		w.capBytes = DefaultCapBytes
	}
	if w.segBytes == 0 {
		w.segBytes = min(DefaultSegmentBytes, w.capBytes/4)
	}
	if w.now == nil {
		w.now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("oplog: %w", err)
	}
	if err := w.resume(); err != nil {
		return nil, err
	}
	return w, nil
}

// resume loads the segments already in dir.
func (w *Writer) resume() error {
	seqs, err := listSegments(w.dir)
	if err != nil {
		return err
	}
	if len(seqs) == 0 {
		return nil
	}
	for _, seq := range seqs {
		st, err := os.Stat(segmentPath(w.dir, seq))
		if err != nil {
			return fmt.Errorf("oplog: %w", err)
		}
		w.segs = append(w.segs, segment{seq: seq, size: st.Size()})
		w.total += st.Size()
	}
	for i := len(w.segs) - 1; i >= 0; i-- {
		end, found, err := w.lastFrameEnd(i, i == len(w.segs)-1)
		if err != nil {
			return err
		}
		if found {
			w.end = end
			break
		}
	}
	last := w.segs[len(w.segs)-1]
	f, err := os.OpenFile(segmentPath(w.dir, last.seq), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("oplog: %w", err)
	}
	w.f = f
	return nil
}

// lastFrameEnd returns the raw end offset of the last complete frame of
// segment i, first truncating a partial final line when cut is set.
func (w *Writer) lastFrameEnd(i int, cut bool) (int64, bool, error) {
	path := segmentPath(w.dir, w.segs[i].seq)
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false, fmt.Errorf("oplog: %w", err)
	}
	complete := bytes.LastIndexByte(b, '\n') + 1
	if cut && complete < len(b) {
		if err := os.Truncate(path, int64(complete)); err != nil {
			return 0, false, fmt.Errorf("oplog: %w", err)
		}
		w.total -= int64(len(b) - complete)
		w.segs[i].size = int64(complete)
	}
	b = b[:complete]
	if len(b) == 0 {
		return 0, false, nil
	}
	start := bytes.LastIndexByte(b[:len(b)-1], '\n') + 1
	var f diskFrame
	if err := json.Unmarshal(b[start:], &f); err != nil {
		return 0, false, fmt.Errorf("oplog: corrupt frame in %s: %w", path, err)
	}
	return f.O + f.rawLen(), true, nil
}

// Write appends p as output of stream, apitypes.StreamStdout or
// apitypes.StreamStderr. It does not retain p.
func (w *Writer) Write(stream string, p []byte) error {
	if stream != apitypes.StreamStdout && stream != apitypes.StreamStderr {
		return fmt.Errorf("oplog: unknown stream %q", stream)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if w.err != nil {
		return w.err
	}
	if len(p) == 0 {
		return nil
	}
	if w.pending != nil && w.pendingStream != stream {
		if err := w.flushPending(); err != nil {
			return err
		}
	}
	buf := p
	if w.pending != nil {
		buf = append(w.pending, p...)
		w.pending = nil
	}
	if n := incompleteTail(buf); n > 0 {
		w.pending = bytes.Clone(buf[len(buf)-n:])
		w.pendingStream = stream
		buf = buf[:len(buf)-n]
	}
	return w.frame(stream, buf)
}

// Stream returns an io.Writer appending to stream, for exec.Cmd pipes.
func (w *Writer) Stream(stream string) io.Writer {
	return streamWriter{w: w, stream: stream}
}

type streamWriter struct {
	w      *Writer
	stream string
}

func (s streamWriter) Write(p []byte) (int, error) {
	if err := s.w.Write(s.stream, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// End returns the absolute offset after the last byte written, including
// bytes still held back as the start of an incomplete rune.
func (w *Writer) End() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.end + int64(len(w.pending))
}

// Close frames any held-back bytes, syncs the active segment and closes it.
// Calling Close again returns nil.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var errs []error
	if w.err == nil && w.pending != nil {
		errs = append(errs, w.flushPending())
	}
	if w.f != nil {
		if err := w.f.Sync(); err != nil {
			errs = append(errs, fmt.Errorf("oplog: %w", err))
		}
		if err := w.f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("oplog: %w", err))
		}
		w.f = nil
	}
	return errors.Join(errs...)
}

func (w *Writer) flushPending() error {
	p, s := w.pending, w.pendingStream
	w.pending = nil
	return w.frame(s, p)
}

// frame splits raw into frames of at most MaxFrameBytes, cut on rune
// boundaries where possible, and appends them.
func (w *Writer) frame(stream string, raw []byte) error {
	t := w.now().UTC()
	for len(raw) > 0 {
		n := min(len(raw), MaxFrameBytes)
		if n < len(raw) {
			for cut := n; cut > n-utf8.UTFMax && cut > 0; cut-- {
				if utf8.RuneStart(raw[cut]) {
					n = cut
					break
				}
			}
		}
		if err := w.appendFrame(stream, raw[:n], t); err != nil {
			return err
		}
		raw = raw[n:]
	}
	return nil
}

func (w *Writer) appendFrame(stream string, raw []byte, t time.Time) error {
	df := diskFrame{O: w.end, T: t, S: stream}
	if utf8.Valid(raw) {
		df.D = string(raw)
	} else {
		df.B = raw
	}
	// HTML escaping is for HTML: without it < > & cost one byte, not six.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(df); err != nil { // Encode ends the line
		return w.fail(err)
	}
	line := buf.Bytes()
	if w.f == nil || (w.segs[len(w.segs)-1].size > 0 && w.segs[len(w.segs)-1].size+int64(len(line)) > w.segBytes) {
		if err := w.roll(); err != nil {
			return err
		}
	}
	// One write per line: a concurrent reader sees at worst a partial
	// final line, which it leaves until the line is complete.
	n, err := w.f.Write(line)
	w.segs[len(w.segs)-1].size += int64(n)
	w.total += int64(n)
	if err != nil {
		return w.fail(err)
	}
	w.end += int64(len(raw))
	return w.evict()
}

// roll starts the next segment.
func (w *Writer) roll() error {
	seq := 1
	if len(w.segs) > 0 {
		seq = w.segs[len(w.segs)-1].seq + 1
	}
	f, err := os.OpenFile(segmentPath(w.dir, seq), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return w.fail(err)
	}
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			_ = f.Close()
			return w.fail(err)
		}
	}
	w.f = f
	w.segs = append(w.segs, segment{seq: seq})
	return nil
}

// evict drops the oldest segments while the log exceeds the cap, never the
// active one.
func (w *Writer) evict() error {
	for w.total > w.capBytes && len(w.segs) > 1 {
		old := w.segs[0]
		if err := os.Remove(segmentPath(w.dir, old.seq)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return w.fail(err)
		}
		w.segs = w.segs[1:]
		w.total -= old.size
	}
	return nil
}

func (w *Writer) fail(err error) error {
	w.err = fmt.Errorf("oplog: %w", err)
	return w.err
}

// incompleteTail returns how many trailing bytes of b are the valid start of
// a multibyte rune that the next write may complete.
func incompleteTail(b []byte) int {
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		c := b[len(b)-i]
		if !utf8.RuneStart(c) {
			continue
		}
		if c >= 0xC0 && !utf8.FullRune(b[len(b)-i:]) {
			return i
		}
		return 0
	}
	return 0
}
