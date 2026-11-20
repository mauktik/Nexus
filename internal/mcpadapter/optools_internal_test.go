// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
)

// The page rules of TC-221 for the cases a test provider cannot produce
// over the wire: a frame split at the limit, a line larger than a page, a
// time-addressed log, and replacement characters.
func TestPageBuilder(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

	t.Run("offset page splits a frame on a rune boundary", func(t *testing.T) {
		b := newPageBuilder(offsetPager, 10, json.RawMessage(`{"offset":100}`))
		if !b.add(apitypes.DataFrame(100, t0, "out", "abcdef")) {
			t.Fatal("first frame refused")
		}
		if b.add(apitypes.DataFrame(106, t0, "out", "gé€xyz")) {
			t.Fatal("frame past the limit accepted whole")
		}
		p := b.page().(offsetPage)
		// 6 bytes used, 4 left: "g" and "é" (2 bytes) fit, "€" (3) does not.
		if text := p.Frames[1].D; text != "gé" || p.NextOffset != 109 || !p.Truncated {
			t.Errorf("page %+v, want gé then nextOffset 109", p)
		}
	})

	t.Run("offset page ends before a frame with no room", func(t *testing.T) {
		b := newPageBuilder(offsetPager, 4, nil)
		b.add(apitypes.DataFrame(0, t0, "out", "abcd"))
		b.add(apitypes.DataFrame(4, t0, "out", "e"))
		if p := b.page().(offsetPage); len(p.Frames) != 1 || p.NextOffset != 4 || !p.Truncated {
			t.Errorf("page %+v, want one frame and nextOffset 4", p)
		}
	})

	t.Run("offset page over a truncation marker and to the end", func(t *testing.T) {
		b := newPageBuilder(offsetPager, 64, json.RawMessage(`{"offset":3}`))
		b.add(apitypes.TruncatedFrame(7))
		b.add(apitypes.DataFrame(10, t0, "out", "abc"))
		p := b.page().(offsetPage)
		if len(p.Frames) != 2 || !p.Frames[0].Truncated() || p.NextOffset != 13 || p.Truncated {
			t.Errorf("page %+v, want the marker, abc and nextOffset 13", p)
		}
		if e := newPageBuilder(offsetPager, 64, json.RawMessage(`{"offset":42}`)).page().(offsetPage); e.NextOffset != 42 ||
			e.Frames == nil || len(e.Frames) != 0 {
			t.Errorf("empty page %+v, want no frames and nextOffset 42", e)
		}
	})

	t.Run("replacement characters never overshoot", func(t *testing.T) {
		// "a\xffb" reads back as "a�b": 5 bytes of text for 3 log bytes.
		b := newPageBuilder(offsetPager, 64, nil)
		b.add(apitypes.DataFrame(0, t0, "out", "a�b"))
		if p := b.page().(offsetPage); p.NextOffset != 3 {
			t.Errorf("nextOffset %d, want 3", p.NextOffset)
		}
	})

	t.Run("since page continues from the last line", func(t *testing.T) {
		b := newPageBuilder(sincePager, 8, json.RawMessage(`{"since":"2026-01-01T00:00:00Z"}`))
		b.add(apitypes.Frame{T: t0, S: "out", D: "line1"})
		b.add(apitypes.Frame{T: t0.Add(time.Second), S: "out", D: "line2"})
		p := b.page().(sincePage)
		if len(p.Frames) != 1 || p.NextOffset != "2026-01-02T03:04:05.000000006Z" || !p.Truncated {
			t.Errorf("page %+v, want line1 and its time", p)
		}
		b = newPageBuilder(sincePager, 64, nil)
		b.add(apitypes.Frame{T: t0, S: "out", D: "line1"})
		if p := b.page().(sincePage); p.NextOffset != "2026-01-02T03:04:05.000000006Z" || p.Truncated {
			t.Errorf("page %+v, want the last line's time, not truncated", p)
		}
		if p := newPageBuilder(sincePager, 64, json.RawMessage(`{"since":"2026-01-01T00:00:00Z"}`)).page().(sincePage); p.NextOffset != "2026-01-01T00:00:00Z" {
			t.Errorf("empty page %+v, want the since it was asked for", p)
		}
	})

	t.Run("since page with one line larger than a page moves past it", func(t *testing.T) {
		b := newPageBuilder(sincePager, 4, nil)
		if b.add(apitypes.Frame{T: t0, S: "out", D: "longline"}) {
			t.Fatal("oversize line accepted whole")
		}
		p := b.page().(sincePage)
		if len(p.Frames) != 1 || p.Frames[0].D != "long" || p.NextOffset != "2026-01-02T03:04:05.000000007Z" {
			t.Errorf("page %+v, want its start and a since 1ns later", p)
		}
	})
}

// A full page stops the stream, and the stop is not an error of the call.
func TestPageStopsStream(t *testing.T) {
	a := &Adapter{pageBytes: 4}
	tl := &tool{pager: offsetPager}
	emitted := 0
	res := a.page(context.Background(), tl, nil, func(ctx context.Context, emit func(apitypes.Frame) error) error {
		for i := int64(0); ; i += 2 {
			if err := emit(apitypes.DataFrame(i, time.Time{}, "out", "ab")); err != nil {
				if ctx.Err() == nil {
					t.Error("stream not canceled when the page filled")
				}
				return err
			}
			emitted++
		}
	})
	if res.IsError {
		t.Fatalf("full page reported as an error: %+v", res)
	}
	var p offsetPage
	if err := json.Unmarshal(res.StructuredContent.(json.RawMessage), &p); err != nil {
		t.Fatal(err)
	}
	want := []apitypes.Frame{apitypes.DataFrame(0, time.Time{}, "out", "ab"), apitypes.DataFrame(2, time.Time{}, "out", "ab")}
	if emitted != 2 || !reflect.DeepEqual(p.Frames, want) || p.NextOffset != 4 || !p.Truncated {
		t.Errorf("emitted %d, page %+v", emitted, p)
	}
}

// sincePages pages lines with a page limit of limit, standing in for
// Docker: each call returns the lines at or after since. It returns the
// text of every frame of every page.
func sincePages(t *testing.T, lines []apitypes.Frame, limit int) []string {
	t.Helper()
	since := ""
	var got []string
	for pages := 0; ; pages++ {
		if pages > 6 {
			t.Fatalf("paging did not end after %d pages: %q", pages, got)
		}
		in := json.RawMessage(`{}`)
		if since != "" {
			in = json.RawMessage(`{"since":"` + since + `"}`)
		}
		b := newPageBuilder(sincePager, limit, in)
		from, _ := time.Parse(time.RFC3339Nano, since)
		for _, f := range lines {
			if f.T.Before(from) {
				continue
			}
			if !b.add(f) {
				break
			}
		}
		p := b.page().(sincePage)
		for _, f := range p.Frames {
			got = append(got, f.D)
		}
		if !p.Truncated {
			return got
		}
		if p.NextOffset == since {
			t.Fatalf("page %d did not advance past since %q: %+v", pages, since, p)
		}
		since = p.NextOffset
	}
}

// CR-5: with an inclusive since, a line that does not fit after a line that
// does must not return the same page forever.
func TestSincePagingAlwaysAdvances(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	lines := []apitypes.Frame{
		{T: t0, S: "out", D: "small"},
		{T: t0.Add(time.Second), S: "out", D: "a-line-that-does-not-fit-the-page"},
		{T: t0.Add(2 * time.Second), S: "out", D: "tail"},
	}
	got := sincePages(t, lines, 10)
	want := []string{"small", "small", "a-lin", "tail"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines %q, want %q (small repeats once: the since is inclusive)", got, want)
	}
}

// CR-92: a line that fits a page by itself is never cut, even when the lines
// the inclusive since repeats leave too little room for it: the page ends
// before it, and the next page starts at its time.
func TestSincePagingKeepsALineThatFitsAPage(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	for _, tc := range []struct {
		name  string
		first string
	}{
		{"room left", "small"},
		{"no room left", "exactly10!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := []apitypes.Frame{
				{T: t0, S: "out", D: tc.first},
				{T: t0.Add(time.Second), S: "out", D: "1234567"},
				{T: t0.Add(2 * time.Second), S: "out", D: "tail"},
			}
			got := sincePages(t, lines, 10)
			want := []string{tc.first, tc.first, "1234567", "tail"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("lines %q, want %q", got, want)
			}
		})
	}
}

// wireBytes encodes res as the SDK sends it: jsonrpc2's encoder (HTML
// escaping off) over CallToolResult.MarshalJSON, which calls json.Marshal
// and so escapes <, > and & again (go-sdk v1.8.0 mcp/protocol.go,
// internal/jsonrpc2/messages.go jsonMarshal).
func wireBytes(t *testing.T, res *mcp.CallToolResult) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// wireText is the bytes of each "d" member of structuredContent's frames
// as sent, without the quotes.
func wireText(t *testing.T, res *mcp.CallToolResult) []string {
	t.Helper()
	var msg struct {
		StructuredContent struct {
			Frames []map[string]json.RawMessage `json:"frames"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(wireBytes(t, res), &msg); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range msg.StructuredContent.Frames {
		if d := f["d"]; len(d) >= 2 {
			out = append(out, string(d[1:len(d)-1]))
		}
	}
	return out
}

// CR-23, CR-93: the page limit counts the bytes the text takes on the wire,
// where a control character is six, and so are <, > and &, which the SDK
// escapes whatever the adapter's own encoder does.
func TestPageLimitCountsEncodedBytes(t *testing.T) {
	a := &Adapter{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"abc", 3}, {"<>&", 18}, {"\x1b", 6}, {"\n", 2}, {`"\`, 4}, {"é", 2}, {"\u2028", 6}, {"\ufffd", 3}, {"a\xffb", 8},
	} {
		if got := encodedLen(tc.in); got != tc.want {
			t.Errorf("encodedLen(%q) = %d, want %d", tc.in, got, tc.want)
		}
		res := a.success(&tool{}, offsetPage{Frames: []apitypes.Frame{{D: tc.in}}})
		if got := wireText(t, res); len(got) != 1 || len(got[0]) != tc.want {
			t.Errorf("%q is %q on the wire, encodedLen says %d bytes", tc.in, got, tc.want)
		}
	}
	if got := encodedPrefix("ab\x1b\x1bc", 13); got != "ab\x1b" {
		t.Errorf("encodedPrefix = %q, want ab and one ESC", got)
	}

	t0 := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	b := newPageBuilder(offsetPager, 13, nil)
	if b.add(apitypes.DataFrame(0, t0, "out", "\x1b\x1b\x1b\x1b")) {
		t.Fatal("24 encoded bytes fit a 13-byte page")
	}
	p := b.page().(offsetPage)
	if len(p.Frames) != 1 || p.Frames[0].D != "\x1b\x1b" || p.NextOffset != 2 || !p.Truncated {
		t.Errorf("page %+v, want two ESC bytes and nextOffset 2", p)
	}
}

// CR-93: a page of output heavy in <, > and & holds at most the page limit
// of text as the SDK sends it.
func TestPageLimitHoldsOnTheWire(t *testing.T) {
	const limit = 64
	a := &Adapter{pageBytes: limit, log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	res := a.page(context.Background(), &tool{pager: offsetPager}, nil,
		func(ctx context.Context, emit func(apitypes.Frame) error) error {
			for i := int64(0); ; i += 16 {
				if err := emit(apitypes.DataFrame(i, time.Time{}, "out", "<a href=x>&amp;")); err != nil {
					return err
				}
			}
		})
	n := 0
	for _, d := range wireText(t, res) {
		n += len(d)
	}
	if n == 0 || n > limit {
		t.Fatalf("the page holds %d bytes of text on the wire, limit %d", n, limit)
	}
}
