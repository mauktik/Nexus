// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package oplog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/apitypes"
)

const segmentExt = ".seg"

// Page is the result of one Read.
type Page struct {
	Frames []apitypes.Frame
	// Next is the offset to read from next: after the last frame returned,
	// or the requested offset when nothing was returned.
	Next int64
	// More reports that maxBytes ended the page before the end of the log.
	More bool
}

// Reader reads the log in a directory. It only opens files for reading and
// keeps no lock, so it is safe while the Writer appends from another
// process.
type Reader struct {
	dir string
	// Poll is how often Follow looks for new output.
	Poll time.Duration

	onOpen func(seq int) // Test hook, called before a segment is opened for scanning
}

// NewReader returns a Reader of the log in dir that polls at
// DefaultPollInterval.
func NewReader(dir string) *Reader {
	return &Reader{dir: dir, Poll: DefaultPollInterval}
}

// cursor is where a scan stopped: the next unread line of segment seq starts
// at file position pos and holds raw offset off. It lets Follow resume a
// segment without parsing it again from the start.
type cursor struct {
	seq   int
	pos   int64
	off   int64
	valid bool
}

// Read returns the frames at and after offset. A frame that offset falls
// inside is returned from offset on. If offset precedes the oldest retained
// byte, the first frame is a truncation marker of the bytes lost. A partial
// final line is left out until it is complete. maxBytes, if positive, ends
// the page once that many data bytes have been returned.
func (r *Reader) Read(offset, maxBytes int64) (Page, error) {
	var p Page
	next, _, more, err := r.scan(context.Background(), offset, cursor{}, maxBytes, func(f apitypes.Frame) error {
		p.Frames = append(p.Frames, f)
		return nil
	})
	p.Next, p.More = next, more
	return p, err
}

// Follow emits the frames from offset on, then polls every r.Poll for more
// until terminal is closed, when it makes one last read and returns nil.
// The caller closes terminal once the operation is terminal and its writer
// closed. Follow also returns when ctx is done or emit fails. It returns the
// offset after the last frame emitted, from which a later Follow resumes
// with no gap and no repeat.
func (r *Reader) Follow(ctx context.Context, offset int64, terminal <-chan struct{}, emit func(apitypes.Frame) error) (int64, error) {
	poll := r.Poll
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	timer := time.NewTimer(poll)
	defer timer.Stop()
	var at cursor
	for {
		final := isClosed(terminal)
		next, c, _, err := r.scan(ctx, offset, at, 0, emit)
		offset, at = next, c
		if err != nil || final {
			return offset, err
		}
		timer.Reset(poll)
		select {
		case <-ctx.Done():
			return offset, ctx.Err()
		case <-terminal:
		case <-timer.C:
		}
	}
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// scan emits frames from offset on and returns the next offset, the cursor
// for a later scan, and whether maxBytes ended it.
func (r *Reader) scan(ctx context.Context, offset int64, at cursor, maxBytes int64, emit func(apitypes.Frame) error) (int64, cursor, bool, error) {
	if offset < 0 {
		return offset, cursor{}, false, fmt.Errorf("oplog: negative offset %d", offset)
	}
	cur := offset
	seq, pos := at.seq, at.pos
	if !at.valid || at.off != offset {
		var ok bool
		var err error
		if seq, ok, err = r.locate(offset); err != nil || !ok {
			return offset, cursor{}, false, err
		}
		pos = 0
	}
	var sent int64
	rechecked := false
	for {
		if r.onOpen != nil {
			r.onOpen(seq)
		}
		f, err := os.Open(segmentPath(r.dir, seq))
		if errors.Is(err, os.ErrNotExist) {
			// Evicted since it was listed. The gap check on the next
			// segment's first frame reports the loss.
			newer, ok, err := r.after(seq)
			if err != nil || !ok {
				return cur, cursor{}, false, err
			}
			seq, pos, rechecked = newer, 0, false
			continue
		}
		if err != nil {
			return cur, cursor{}, false, fmt.Errorf("oplog: %w", err)
		}
		stop, err := r.scanFile(ctx, f, &pos, &cur, &sent, maxBytes, emit)
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("oplog: %w", cerr)
		}
		here := cursor{seq: seq, pos: pos, off: cur, valid: true}
		if err != nil {
			return cur, here, false, err
		}
		if stop {
			return cur, here, true, nil
		}
		newer, ok, err := r.after(seq)
		if err != nil {
			return cur, here, false, err
		}
		if !ok {
			return cur, here, false, nil
		}
		if !rechecked {
			// The writer finishes a line before it starts the next
			// segment, so look once more for the end of this one.
			rechecked = true
			continue
		}
		seq, pos, rechecked = newer, 0, false
	}
}

// scanFile emits the complete lines of f from *pos on, advancing *pos past
// each line and *cur past each frame. It returns true when maxBytes stops it.
func (r *Reader) scanFile(ctx context.Context, f *os.File, pos, cur, sent *int64, maxBytes int64, emit func(apitypes.Frame) error) (bool, error) {
	if _, err := f.Seek(*pos, io.SeekStart); err != nil {
		return false, fmt.Errorf("oplog: %w", err)
	}
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return false, nil // Nothing more, or a partial final line
		}
		if err != nil {
			return false, fmt.Errorf("oplog: %w", err)
		}
		var df diskFrame
		if err := json.Unmarshal(line, &df); err != nil {
			return false, fmt.Errorf("oplog: corrupt frame in %s at %d: %w", f.Name(), *pos, err)
		}
		raw := df.raw()
		start, end := df.O, df.O+int64(len(raw))
		if start > *cur {
			if err := emitCtx(ctx, emit, apitypes.TruncatedFrame(start-*cur)); err != nil {
				return false, err
			}
			*cur = start
		}
		if end > *cur {
			if err := emitCtx(ctx, emit, apitypes.DataFrame(*cur, df.T, df.S, validText(raw[*cur-start:]))); err != nil {
				return false, err
			}
			*sent += end - *cur
			*cur = end
		}
		*pos += int64(len(line))
		if maxBytes > 0 && *sent >= maxBytes {
			return true, nil
		}
	}
}

func emitCtx(ctx context.Context, emit func(apitypes.Frame) error, f apitypes.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return emit(f)
}

// locate returns the segment holding offset: the newest whose first frame
// starts at or before it, else the oldest. ok is false for an empty log.
func (r *Reader) locate(offset int64) (int, bool, error) {
	seqs, err := listSegments(r.dir)
	if err != nil {
		return 0, false, err
	}
	found, chosen := false, 0
	for _, seq := range seqs {
		start, ok, err := r.firstOffset(seq)
		if err != nil {
			return 0, false, err
		}
		if !ok {
			continue
		}
		if !found || start <= offset {
			found, chosen = true, seq
		}
		if start > offset {
			break
		}
	}
	return chosen, found, nil
}

// firstOffset returns the offset of the first frame of segment seq. ok is
// false when the segment is gone or holds no complete line yet.
func (r *Reader) firstOffset(seq int) (int64, bool, error) {
	f, err := os.Open(segmentPath(r.dir, seq))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("oplog: %w", err)
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	if errors.Is(err, io.EOF) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("oplog: %w", err)
	}
	var df diskFrame
	if err := json.Unmarshal(line, &df); err != nil {
		return 0, false, fmt.Errorf("oplog: corrupt frame in %s: %w", f.Name(), err)
	}
	return df.O, true, nil
}

// after returns the oldest segment newer than seq.
func (r *Reader) after(seq int) (int, bool, error) {
	seqs, err := listSegments(r.dir)
	if err != nil {
		return 0, false, err
	}
	for _, s := range seqs {
		if s > seq {
			return s, true, nil
		}
	}
	return 0, false, nil
}

// validText returns raw as UTF-8, replacing each invalid byte with U+FFFD.
func validText(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	var b strings.Builder
	b.Grow(len(raw) + 8)
	for len(raw) > 0 {
		c, n := utf8.DecodeRune(raw)
		b.WriteRune(c) // An invalid byte decodes as utf8.RuneError, width 1
		raw = raw[n:]
	}
	return b.String()
}

func segmentPath(dir string, seq int) string {
	return filepath.Join(dir, fmt.Sprintf("%06d%s", seq, segmentExt))
}

// listSegments returns the segment numbers in dir, oldest first. A missing
// directory is an empty log.
func listSegments(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("oplog: %w", err)
	}
	var seqs []int
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), segmentExt)
		if !ok || e.IsDir() {
			continue
		}
		seq, err := strconv.Atoi(name)
		if err != nil || seq <= 0 {
			continue
		}
		seqs = append(seqs, seq)
	}
	slices.Sort(seqs)
	return seqs, nil
}
