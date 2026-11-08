package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
)

// The operation handle loop (FR-28). A long-running tool answers at once
// with the operation; get_operation, get_operation_logs and
// cancel_operation are the system ops' own tools. A ModeStream op cannot
// stream over a JSON response, so its tool returns one bounded page of
// frames (Streaming, A-65), and the agent pages with nextOffset.

// offsetPage is a page of an offset-addressed log: operation output.
type offsetPage struct {
	Frames     []apitypes.Frame `json:"frames" jsonschema:"Output frames, oldest first."`
	NextOffset int64            `json:"nextOffset" jsonschema:"Byte offset to pass back as offset for the next page."`
	Truncated  bool             `json:"truncated" jsonschema:"True when the page size ended the page: more output follows nextOffset."`
}

// sincePage is a page of a time-addressed log: container logs, whose frames
// have no byte offset.
type sincePage struct {
	Frames []apitypes.Frame `json:"frames" jsonschema:"Log lines, oldest first."`
	// NextOffset is the last frame's time. Docker's since may repeat lines
	// logged at that time.
	NextOffset string `json:"nextOffset,omitempty" jsonschema:"RFC 3339 time to pass back as since for the next page; lines at that time may repeat."`
	Truncated  bool   `json:"truncated" jsonschema:"True when the page size ended the page: more lines follow nextOffset."`
}

// errPageFull stops a stream once its page is full.
var errPageFull = errors.New("page full")

// pageBuilder collects one page of frames, at most limit bytes of text.
type pageBuilder struct {
	pager  pager
	limit  int
	used   int
	frames []apitypes.Frame
	full   bool

	// offsetPager: the offset after the last byte in the page.
	next int64
	// sincePager: the time to continue from, once set.
	since    string
	haveData bool
	lastT    time.Time
}

// newPageBuilder starts a page at the call's offset or since.
func newPageBuilder(p pager, limit int, input json.RawMessage) *pageBuilder {
	b := &pageBuilder{pager: p, limit: limit, frames: []apitypes.Frame{}}
	var in struct {
		Offset int64  `json:"offset"`
		Since  string `json:"since"`
	}
	// The Dispatcher has validated the input by now.
	_ = json.Unmarshal(input, &in)
	b.next, b.since = in.Offset, in.Since
	return b
}

// add takes f into the page. It returns false once the page is full: f is
// then in the page only in part, or not at all, and nextOffset says where
// the next page starts.
func (b *pageBuilder) add(f apitypes.Frame) bool {
	if f.Truncated() {
		// A marker of output lost to the cap carries no text.
		b.frames = append(b.frames, f)
		return true
	}
	if b.used+len(f.D) <= b.limit {
		b.take(f)
		return true
	}
	b.full = true
	room := b.limit - b.used
	switch b.pager {
	case offsetPager:
		if part := prefix(f.D, room); part != "" {
			g := f
			g.D = part
			b.take(g)
		} else if f.O != nil {
			b.next = *f.O
		}
	case sincePager:
		if b.haveData {
			b.since = formatTime(b.lastT)
			break
		}
		// One line larger than a page: its start, and the next page
		// starts after it, since its time would only repeat it.
		g := f
		g.D = prefix(f.D, room)
		b.frames = append(b.frames, g)
		b.since = formatTime(f.T.Add(time.Nanosecond))
	}
	return false
}

func (b *pageBuilder) take(f apitypes.Frame) {
	b.frames = append(b.frames, f)
	b.used += len(f.D)
	b.haveData, b.lastT = true, f.T
	if f.O != nil {
		b.next = *f.O + rawLen(f.D)
	}
}

// page is the finished page. A page the size did not end continues from
// its last frame.
func (b *pageBuilder) page() any {
	if b.pager == offsetPager {
		return offsetPage{Frames: b.frames, NextOffset: b.next, Truncated: b.full}
	}
	if !b.full && b.haveData {
		b.since = formatTime(b.lastT)
	}
	return sincePage{Frames: b.frames, NextOffset: b.since, Truncated: b.full}
}

// rawLen is the number of log bytes a frame's text stands for. The reader
// turns each invalid byte into U+FFFD (three bytes) and leaves a valid
// U+FFFD as it is, so the two cannot be told apart; counting each U+FFFD as
// one byte never overshoots. The next page then starts at or before the
// true end: at worst it repeats part of a line holding U+FFFD, and it never
// skips output.
func rawLen(d string) int64 {
	return int64(len(d) - 2*strings.Count(d, string(utf8.RuneError)))
}

// prefix is the longest prefix of s of at most n bytes that ends on a rune
// boundary.
func prefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// page drives a stream until it ends or a page is full, then stops it. A
// stream stopped for a full page reports the cancellation, which is not an
// error of the call.
func (a *Adapter) page(ctx context.Context, t *tool, input json.RawMessage,
	stream func(context.Context, func(apitypes.Frame) error) error) *mcp.CallToolResult {
	b := newPageBuilder(t.pager, a.pageBytes, input)
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	err := stream(sctx, func(f apitypes.Frame) error {
		if b.add(f) {
			return nil
		}
		stop()
		return errPageFull
	})
	if err != nil && !b.full {
		return a.failure(t.op, err)
	}
	return a.success(t, b.page())
}
