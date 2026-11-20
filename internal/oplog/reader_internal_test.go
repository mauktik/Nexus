// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package oplog

import (
	"os"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
)

func mustOpen(t *testing.T, dir string, opts Options) *Writer {
	t.Helper()
	w, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func mustWrite(t *testing.T, w *Writer, s string) {
	t.Helper()
	if err := w.Write(apitypes.StreamStdout, []byte(s)); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func mustRead(t *testing.T, r *Reader, offset int64) Page {
	t.Helper()
	p, err := r.Read(offset, 0)
	if err != nil {
		t.Fatalf("Read(%d): %v", offset, err)
	}
	return p
}

func joinData(t *testing.T, frames []apitypes.Frame) string {
	t.Helper()
	var b strings.Builder
	for _, f := range frames {
		if f.Truncated() {
			t.Fatalf("unexpected truncation marker %+v", f)
		}
		b.WriteString(f.D)
	}
	return b.String()
}

// TC-E9: reader boundaries.
func TestReaderBoundaries(t *testing.T) {
	const full = "hello world\nsecond line\n"
	known := func(t *testing.T) string {
		dir := t.TempDir()
		w := mustOpen(t, dir, Options{})
		mustWrite(t, w, "hello world\n")
		mustWrite(t, w, "second line\n")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("at and beyond end", func(t *testing.T) {
		r := NewReader(known(t))
		end := int64(len(full))
		for _, off := range []int64{end, end + 1, end + 1000} {
			p := mustRead(t, r, off)
			if len(p.Frames) != 0 || p.Next != off {
				t.Errorf("Read(%d) = %+v, want no frames and Next %d", off, p, off)
			}
		}
	})

	t.Run("inside a frame", func(t *testing.T) {
		r := NewReader(known(t))
		for n := 0; n < len(full); n++ {
			p := mustRead(t, r, int64(n))
			if got := joinData(t, p.Frames); got != full[n:] {
				t.Errorf("Read(%d) = %q, want %q", n, got, full[n:])
			}
			if len(p.Frames) == 0 {
				t.Fatalf("Read(%d) returned no frames", n)
			}
			if o := p.Frames[0].O; o == nil || *o != int64(n) {
				t.Errorf("Read(%d) first o = %v", n, o)
			}
		}
	})

	t.Run("segment evicted mid-scan", func(t *testing.T) {
		dir := t.TempDir()
		w := mustOpen(t, dir, Options{SegmentBytes: 4096, CapBytes: 1 << 20})
		var all strings.Builder
		for i := range 40 {
			line := strings.Repeat(string(rune('a'+i%26)), 999) + "\n"
			mustWrite(t, w, line)
			all.WriteString(line)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		r := NewReader(dir)
		evicted := false
		r.onOpen = func(seq int) {
			if seq == 2 && !evicted {
				evicted = true
				if err := os.Remove(segmentPath(dir, 2)); err != nil {
					t.Fatal(err)
				}
			}
		}
		p, err := r.Read(0, 0)
		if err != nil {
			t.Fatalf("Read with an evicted segment: %v", err)
		}
		if !evicted {
			t.Fatal("the scan never opened segment 2")
		}
		var before, after strings.Builder
		var marker *apitypes.Frame
		for i, f := range p.Frames {
			switch {
			case f.Truncated():
				if marker != nil {
					t.Fatalf("second marker %+v", f)
				}
				marker = &p.Frames[i]
			case marker == nil:
				before.WriteString(f.D)
			default:
				after.WriteString(f.D)
			}
		}
		if marker == nil {
			t.Fatal("no truncation marker for the evicted segment")
		}
		if before.Len() == 0 || after.Len() == 0 {
			t.Fatalf("want data on both sides of the marker, got %d and %d bytes", before.Len(), after.Len())
		}
		lostStart := int64(before.Len())
		lostEnd := lostStart + marker.LostBytes
		if s := all.String(); before.String() != s[:lostStart] || after.String() != s[lostEnd:] {
			t.Errorf("data around the evicted range [%d,%d) does not match the input", lostStart, lostEnd)
		}
		if p.Next != int64(all.Len()) {
			t.Errorf("Next = %d, want %d", p.Next, all.Len())
		}
	})

	t.Run("empty log", func(t *testing.T) {
		for name, dir := range map[string]string{
			"missing directory": t.TempDir() + "/never-created",
			"no writes":         func() string { d := t.TempDir(); w := mustOpen(t, d, Options{}); _ = w.Close(); return d }(),
		} {
			p := mustRead(t, NewReader(dir), 0)
			if len(p.Frames) != 0 || p.Next != 0 {
				t.Errorf("%s: Read = %+v, want empty", name, p)
			}
		}
	})

	t.Run("large chunk", func(t *testing.T) {
		dir := t.TempDir()
		w := mustOpen(t, dir, Options{})
		var b strings.Builder
		for b.Len() < 3<<20 {
			b.WriteString("héllo wörld € 𝄞 ")
		}
		chunk := b.String()
		mustWrite(t, w, chunk)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		p := mustRead(t, NewReader(dir), 0)
		if got := joinData(t, p.Frames); got != chunk {
			t.Fatalf("3 MiB chunk did not round-trip: got %d bytes, want %d", len(got), len(chunk))
		}
		var off int64
		for i, f := range p.Frames {
			if *f.O != off || len(f.D) > MaxFrameBytes {
				t.Fatalf("frame %d: o %d (want %d), %d bytes", i, *f.O, off, len(f.D))
			}
			off += int64(len(f.D))
		}
		if n, err := listSegments(dir); err != nil || len(n) < 3 {
			t.Errorf("segments = %v, %v; want at least 3 for 3 MiB at 1 MiB", n, err)
		}
	})
}
