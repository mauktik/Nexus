package mcpadapter

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

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
