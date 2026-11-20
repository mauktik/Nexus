// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package oplog_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/oplog"
)

const (
	out = apitypes.StreamStdout
	er  = apitypes.StreamStderr
)

func openWriter(t *testing.T, dir string, opts oplog.Options) *oplog.Writer {
	t.Helper()
	w, err := oplog.Open(dir, opts)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func write(t *testing.T, w *oplog.Writer, stream, data string) {
	t.Helper()
	if err := w.Write(stream, []byte(data)); err != nil {
		t.Fatalf("Write(%s, %q): %v", stream, data, err)
	}
}

func closeWriter(t *testing.T, w *oplog.Writer) {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func readPage(t *testing.T, dir string, offset int64) oplog.Page {
	t.Helper()
	p, err := oplog.NewReader(dir).Read(offset, 0)
	if err != nil {
		t.Fatalf("Read(%d): %v", offset, err)
	}
	return p
}

// concat joins the data of p's frames and fails on any truncation marker.
func concat(t *testing.T, frames []apitypes.Frame) string {
	t.Helper()
	var b strings.Builder
	for i, f := range frames {
		if f.Truncated() {
			t.Fatalf("frame %d is an unexpected truncation marker %+v", i, f)
		}
		b.WriteString(f.D)
	}
	return b.String()
}

func segments(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.seg"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	return paths
}

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	for _, p := range segments(t, dir) {
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		n += st.Size()
	}
	return n
}

// TC-44: output frames preserve order and absolute offsets.
func TestFramesPreserveOrderAndAbsoluteOffsets(t *testing.T) {
	dir := t.TempDir()
	w := openWriter(t, dir, oplog.Options{})
	chunks := []struct{ s, d string }{
		{out, "hello "},
		{er, "warning: disk\n"},
		{out, "world\n"},
		{er, "é ünïcödé €\n"},
		{out, strings.Repeat("z", 100)},
		{out, "<&>\t\"quoted\"\n"},
		{er, "last\n"},
	}
	var want strings.Builder
	var offsets []int64
	var off int64
	for _, c := range chunks {
		offsets = append(offsets, off)
		write(t, w, c.s, c.d)
		want.WriteString(c.d)
		off += int64(len(c.d))
	}
	closeWriter(t, w)

	p := readPage(t, dir, 0)
	if len(p.Frames) != len(chunks) {
		t.Fatalf("got %d frames, want %d: %+v", len(p.Frames), len(chunks), p.Frames)
	}
	if got := concat(t, p.Frames); got != want.String() {
		t.Errorf("concatenated d = %q, want %q", got, want.String())
	}
	for i, f := range p.Frames {
		if f.O == nil || *f.O != offsets[i] {
			t.Errorf("frame %d o = %v, want %d", i, f.O, offsets[i])
		}
		if f.S != chunks[i].s {
			t.Errorf("frame %d s = %q, want %q", i, f.S, chunks[i].s)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(wire))
		for k := range wire {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"d", "o", "s", "t"}) {
			t.Errorf("frame %d wire keys = %v, want [d o s t]", i, keys)
		}
		var ts string
		if err := json.Unmarshal(wire["t"], &ts); err != nil {
			t.Fatalf("frame %d t: %v", i, err)
		}
		if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			t.Errorf("frame %d t %q does not parse as RFC3339Nano: %v", i, ts, err)
		}
	}
	if p.Next != off {
		t.Errorf("Next = %d, want %d", p.Next, off)
	}
}

// pattern returns n deterministic printable ASCII bytes, so a byte offset
// identifies its expected content.
func pattern(n int) []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 \n"
	rng := rand.New(rand.NewPCG(44, 45))
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return b
}

// retainedRaw sums the raw bytes held in the segment files, read straight
// from disk without the Reader.
func retainedRaw(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	for _, p := range segments(t, dir) {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			var fr struct {
				D string `json:"d"`
			}
			if err := json.Unmarshal(sc.Bytes(), &fr); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			n += int64(len(fr.D))
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	return n
}

// TC-45: the cap keeps the most recent output and reports truncation.
func TestCapKeepsMostRecentOutputAndReportsTruncation(t *testing.T) {
	const total = 25 << 20
	const chunk = 64 << 10
	data := pattern(total)
	for _, tc := range []struct {
		name    string
		capOpt  int64
		wantCap int64
	}{
		{"default 10 MiB", 0, 10 << 20},
		{"configured 2 MiB", 2 << 20, 2 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if oplog.DefaultCapBytes != 10<<20 || oplog.DefaultSegmentBytes != 1<<20 {
				t.Fatalf("defaults = cap %d, segment %d; want 10 MiB and 1 MiB",
					oplog.DefaultCapBytes, oplog.DefaultSegmentBytes)
			}
			dir := t.TempDir()
			w := openWriter(t, dir, oplog.Options{CapBytes: tc.capOpt})
			limit := tc.wantCap + oplog.DefaultSegmentBytes
			for i := 0; i < total; i += chunk {
				write(t, w, out, string(data[i:i+chunk]))
				if sz := dirSize(t, dir); sz > limit {
					t.Fatalf("after %d bytes on-disk size %d exceeds cap plus one segment %d", i+chunk, sz, limit)
				}
			}
			closeWriter(t, w)

			kept := retainedRaw(t, dir)
			lost := int64(total) - kept
			if lost <= 0 || kept <= 0 {
				t.Fatalf("retained %d of %d bytes; want some dropped and some kept", kept, total)
			}

			p := readPage(t, dir, 0)
			if len(p.Frames) == 0 || !p.Frames[0].Truncated() {
				t.Fatalf("first frame = %+v, want a truncation marker", p.Frames)
			}
			if p.Frames[0].LostBytes != lost {
				t.Errorf("lostBytes = %d, want %d", p.Frames[0].LostBytes, lost)
			}
			marker, err := json.Marshal(p.Frames[0])
			if err != nil {
				t.Fatal(err)
			}
			if want := `{"type":"truncated","lostBytes":` + itoa(lost) + `}`; string(marker) != want {
				t.Errorf("marker wire = %s, want %s", marker, want)
			}
			if got := concat(t, p.Frames[1:]); got != string(data[lost:]) {
				t.Errorf("retained text (%d bytes) is not the pattern tail (%d bytes)", len(got), total-lost)
			}
			if o := p.Frames[1].O; o == nil || *o != lost {
				t.Errorf("first retained o = %v, want %d", o, lost)
			}

			p2 := readPage(t, dir, lost)
			if got := concat(t, p2.Frames); got != string(data[lost:]) {
				t.Errorf("read from oldest retained offset: %d bytes, want the %d-byte tail", len(got), total-lost)
			}
			if p2.Next != total {
				t.Errorf("Next = %d, want %d", p2.Next, total)
			}
		})
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TC-E7: invalid UTF-8 is replaced in d while offsets count raw bytes.
func TestInvalidUTF8AndOffsetAccounting(t *testing.T) {
	dir := t.TempDir()
	w := openWriter(t, dir, oplog.Options{})
	euro := "€" // e2 82 ac
	writes := []struct{ s, d string }{
		{out, "\xff\xfe"},
		{out, "ok"},
		{out, "a" + euro[:2]}, // the rune is split across two writes
		{out, euro[2:] + "b"},
		{er, euro[:2]}, // an incomplete rune cut off by the other stream
		{out, "z"},
	}
	var rawTotal int64
	for _, c := range writes {
		write(t, w, c.s, c.d)
		rawTotal += int64(len(c.d))
	}
	closeWriter(t, w)

	p := readPage(t, dir, 0)
	type fr struct {
		o    int64
		s, d string
	}
	want := []fr{
		{0, out, "��"},
		{2, out, "ok"},
		{4, out, "a"},
		{5, out, "€b"},
		{9, er, "��"},
		{11, out, "z"},
	}
	var got []fr
	for _, f := range p.Frames {
		if f.Truncated() || f.O == nil {
			t.Fatalf("unexpected frame %+v", f)
		}
		if !utf8.ValidString(f.D) {
			t.Errorf("d %q is not valid UTF-8", f.D)
		}
		got = append(got, fr{*f.O, f.S, f.D})
	}
	if !slices.Equal(got, want) {
		t.Errorf("frames = %+v\nwant     %+v", got, want)
	}
	if len(got) > 1 && got[1].o != got[0].o+2 {
		t.Errorf("frame after \\xff\\xfe has o %d, want %d", got[1].o, got[0].o+2)
	}
	if !strings.Contains(concat(t, p.Frames), "a€b") {
		t.Errorf("split rune lost: %q", concat(t, p.Frames))
	}
	if p.Next != rawTotal {
		t.Errorf("Next = %d, want raw total %d", p.Next, rawTotal)
	}
	// The offset inside the split rune still addresses raw bytes.
	if q := readPage(t, dir, 6); len(q.Frames) == 0 || *q.Frames[0].O != 6 || !bytes.HasSuffix([]byte(q.Frames[0].D), []byte("b")) {
		t.Errorf("read inside the rune = %+v", q.Frames)
	}
}

// Supplementary to TC-44: a reopened log continues its offsets after cutting
// a partial line left by a killed writer, and Read pages by maxBytes.
func TestReopenContinuesOffsetsAndReadPages(t *testing.T) {
	dir := t.TempDir()
	w := openWriter(t, dir, oplog.Options{})
	if _, err := io.WriteString(w.Stream(out), "a\n"); err != nil {
		t.Fatal(err)
	}
	write(t, w, er, "\xffb\n")
	closeWriter(t, w)
	if err := w.Write(out, []byte("x")); !errors.Is(err, oplog.ErrClosed) {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
	segs := segments(t, dir)
	appendFile(t, segs[len(segs)-1], []byte(`{"o":5,"t":"2026-`))

	w2 := openWriter(t, dir, oplog.Options{})
	if got := w2.End(); got != 5 {
		t.Fatalf("reopened End = %d, want 5", got)
	}
	if err := w2.Write("tty", []byte("x")); err == nil {
		t.Error("Write to an unknown stream succeeded")
	}
	write(t, w2, out, "c\n")
	closeWriter(t, w2)

	p := readPage(t, dir, 0)
	if got := concat(t, p.Frames); got != "a\n�b\nc\n" || p.Next != 7 {
		t.Fatalf("after reopen got %q, Next %d", got, p.Next)
	}
	r := oplog.NewReader(dir)
	var paged strings.Builder
	for off, pages := int64(0), 0; ; pages++ {
		pg, err := r.Read(off, 2)
		if err != nil {
			t.Fatal(err)
		}
		if pages > 5 || len(pg.Frames) > 1 {
			t.Fatalf("page %d = %+v, want one frame per page", pages, pg)
		}
		paged.WriteString(concat(t, pg.Frames))
		off = pg.Next
		if !pg.More {
			break
		}
	}
	if paged.String() != "a\n�b\nc\n" {
		t.Errorf("paged read = %q", paged.String())
	}
}

// Supplementary: invalid arguments and a corrupt complete line are errors,
// never frames.
func TestInvalidInputsAreErrors(t *testing.T) {
	if _, err := oplog.Open(t.TempDir(), oplog.Options{CapBytes: -1}); err == nil {
		t.Error("Open with a negative cap succeeded")
	}
	dir := t.TempDir()
	if _, err := oplog.NewReader(dir).Read(-1, 0); err == nil {
		t.Error("Read at a negative offset succeeded")
	}
	w := openWriter(t, dir, oplog.Options{})
	write(t, w, out, "a\n")
	closeWriter(t, w)
	segs := segments(t, dir)
	appendFile(t, segs[0], []byte("not json\n"))
	if p, err := oplog.NewReader(dir).Read(0, 0); err == nil {
		t.Errorf("Read of a corrupt line = %+v, want an error", p)
	}
	if _, err := oplog.Open(dir, oplog.Options{}); err == nil {
		t.Error("Open of a log ending in a corrupt line succeeded")
	}
}

// CR-22: a cap below the default segment size is honoured to within its own
// size, not to 1 MiB.
func TestSmallCapIsHonouredBelowTheSegmentSize(t *testing.T) {
	const cap = 4096
	dir := t.TempDir()
	w := openWriter(t, dir, oplog.Options{CapBytes: cap})
	chunk := strings.Repeat("x", 100)
	for range 20000 {
		write(t, w, out, chunk)
	}
	closeWriter(t, w)
	if sz := dirSize(t, dir); sz > 2*cap {
		t.Fatalf("on-disk size %d with a cap of %d, want at most %d", sz, cap, 2*cap)
	}
}

// CR-22: HTML-significant characters are stored as one byte, so they do not
// shrink the output a cap keeps sixfold.
func TestHTMLCharactersAreNotEscapedOnDisk(t *testing.T) {
	dir := t.TempDir()
	w := openWriter(t, dir, oplog.Options{})
	const raw = 64 << 10
	write(t, w, out, strings.Repeat("<&>", raw/3))
	closeWriter(t, w)
	if sz := dirSize(t, dir); sz > raw+raw/10 {
		t.Fatalf("on-disk size %d for %d raw bytes of < & >; want close to the raw size", sz, raw)
	}
	if got := concat(t, readPage(t, dir, 0).Frames); got != strings.Repeat("<&>", raw/3) {
		t.Fatalf("read back %d bytes that differ from what was written", len(got))
	}
}
