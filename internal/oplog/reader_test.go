// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package oplog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/oplog"
)

// TC-46: follow polls until terminal and resumes without gaps.
func TestFollowPollsUntilTerminalAndResumesWithoutGaps(t *testing.T) {
	if oplog.DefaultPollInterval != 250*time.Millisecond {
		t.Fatalf("DefaultPollInterval = %v, want 250ms (A-54)", oplog.DefaultPollInterval)
	}
	dir := t.TempDir()
	w := openWriter(t, dir, oplog.Options{})

	var mu sync.Mutex
	written := 0
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			if err := w.Write(out, []byte(strconv.Itoa(i)+"\n")); err != nil {
				t.Errorf("Write: %v", err)
				return
			}
			mu.Lock()
			written++
			mu.Unlock()
		}
	}()

	r := oplog.NewReader(dir)
	if r.Poll != oplog.DefaultPollInterval {
		t.Fatalf("NewReader Poll = %v, want %v", r.Poll, oplog.DefaultPollInterval)
	}
	var combined strings.Builder
	var maxLatency time.Duration
	collect := func(f apitypes.Frame) {
		if f.Truncated() || f.O == nil {
			t.Errorf("unexpected frame %+v", f)
			return
		}
		if lat := time.Since(f.T); lat > maxLatency {
			maxLatency = lat
		}
		combined.WriteString(f.D)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var last apitypes.Frame
	n := 0
	_, err := r.Follow(ctx, 0, nil, func(f apitypes.Frame) error {
		collect(f)
		last = f
		if n++; n == 6 {
			cancel()
		}
		return nil
	})
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Follow err = %v, want context.Canceled", err)
	}
	resume := *last.O + int64(len(last.D))

	terminal := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := r.Follow(context.Background(), resume, terminal, func(f apitypes.Frame) error {
			collect(f)
			return nil
		})
		done <- err
	}()
	time.Sleep(700 * time.Millisecond)
	close(stop)
	<-writerDone
	closeWriter(t, w)
	close(terminal)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Follow err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Follow did not return after terminal")
	}

	mu.Lock()
	total := written
	mu.Unlock()
	var want strings.Builder
	for i := range total {
		want.WriteString(strconv.Itoa(i) + "\n")
	}
	if combined.String() != want.String() {
		t.Errorf("combined output = %q\nwant %q", combined.String(), want.String())
	}
	if maxLatency > 600*time.Millisecond {
		t.Errorf("max frame latency %v, want about 250ms", maxLatency)
	}
	t.Logf("%d lines, max latency %v", total, maxLatency)
}

const childDirEnv = "NEXUS_OPLOG_TEST_CHILD_DIR"

func childLine(i int) string {
	return fmt.Sprintf("line %07d %s\n", i, strings.Repeat("x", i%300))
}

// TestHelperChildWriter is the writer process of TC-E8; it runs only when
// re-executed by that test.
func TestHelperChildWriter(t *testing.T) {
	dir := os.Getenv(childDirEnv)
	if dir == "" {
		t.Skip("helper process for TC-E8")
	}
	w, err := oplog.Open(dir, oplog.Options{SegmentBytes: 16 << 10, CapBytes: 1 << 30})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for i := 0; ; i++ {
		if err := w.Write(out, []byte(childLine(i))); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if i%20 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
}

// checkChildFrames fails unless frames are exactly the child's lines from
// the start, contiguous and well formed, and returns how many there are.
func checkChildFrames(t *testing.T, p oplog.Page) int {
	t.Helper()
	var off int64
	for i, f := range p.Frames {
		if f.Truncated() || f.O == nil || f.S != out || f.T.IsZero() {
			t.Fatalf("frame %d malformed: %+v", i, f)
		}
		if *f.O != off || f.D != childLine(i) {
			t.Fatalf("frame %d = {o:%d d:%q}, want {o:%d d:%q}", i, *f.O, f.D, off, childLine(i))
		}
		off += int64(len(f.D))
	}
	if p.Next != off {
		t.Fatalf("Next = %d, want %d", p.Next, off)
	}
	return len(p.Frames)
}

// TC-E8: the reader works across processes and tolerates a partial final line.
func TestReaderAcrossProcessesAndPartialFinalLine(t *testing.T) {
	t.Run("child writer killed", func(t *testing.T) {
		dir := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChildWriter$", "-test.count=1")
		cmd.Env = append(os.Environ(), childDirEnv+"="+dir)
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		killed := false
		t.Cleanup(func() {
			if !killed {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})

		r := oplog.NewReader(dir)
		// Read until the child has crossed segment boundaries (event, not a
		// wall-clock window) so the property holds however slowly it starts.
		// The deadline only bounds a hung child.
		const wantSegments = 3
		reads, maxSeen := 0, 0
		for deadline := time.Now().Add(60 * time.Second); ; {
			p, err := r.Read(0, 0)
			if err != nil {
				t.Fatalf("Read while child writes: %v", err)
			}
			n := checkChildFrames(t, p)
			if n < maxSeen {
				t.Fatalf("frames went backwards: %d after %d", n, maxSeen)
			}
			maxSeen = n
			reads++
			if reads >= 20 && maxSeen > 0 && len(segments(t, dir)) >= wantSegments {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("child wrote %d segments and %d frames in 60s, want >= %d segments", len(segments(t, dir)), maxSeen, wantSegments)
			}
		}
		if err := cmd.Process.Signal(os.Kill); err != nil {
			t.Fatal(err)
		}
		killed = true
		_ = cmd.Wait()

		p, err := r.Read(0, 0)
		if err != nil {
			t.Fatalf("Read after SIGKILL: %v", err)
		}
		final := checkChildFrames(t, p)
		if maxSeen == 0 || final < maxSeen {
			t.Fatalf("final %d frames, max seen while writing %d", final, maxSeen)
		}
		if n := len(segments(t, dir)); n < wantSegments {
			t.Errorf("child wrote %d segments, want >= %d to cross segment boundaries", n, wantSegments)
		}
		t.Logf("%d reads while writing, %d frames after SIGKILL", reads, final)
	})

	t.Run("fixture ending in half a line", func(t *testing.T) {
		dir := t.TempDir()
		w := openWriter(t, dir, oplog.Options{})
		write(t, w, out, "first\n")
		write(t, w, er, "second\n")
		end := w.End()
		closeWriter(t, w)

		line, err := json.Marshal(apitypes.DataFrame(end, time.Now().UTC(), out, "third\n"))
		if err != nil {
			t.Fatal(err)
		}
		line = append(line, '\n')
		segs := segments(t, dir)
		if len(segs) == 0 {
			t.Fatal("writer left no segment")
		}
		seg := segs[len(segs)-1]
		half := len(line) / 2
		appendFile(t, seg, line[:half])

		p := readPage(t, dir, 0)
		if got := concat(t, p.Frames); got != "first\nsecond\n" {
			t.Fatalf("with half a line, got %q", got)
		}
		if p.Next != end {
			t.Errorf("Next = %d, want %d", p.Next, end)
		}
		if q := readPage(t, dir, end); len(q.Frames) != 0 || q.Next != end {
			t.Errorf("read at the partial line = %+v", q)
		}

		appendFile(t, seg, line[half:])
		p = readPage(t, dir, 0)
		if got := concat(t, p.Frames); got != "first\nsecond\nthird\n" {
			t.Fatalf("after completing the line, got %q", got)
		}
		if f := p.Frames[2]; *f.O != end || f.S != out {
			t.Errorf("completed frame = %+v", f)
		}
	})
}

func appendFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
