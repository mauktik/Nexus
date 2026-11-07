package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// maxLineBytes bounds a log line held while waiting for its newline; a
// longer one is sent in pieces.
const maxLineBytes = 1 << 20

// logs streams a container's log lines as frames. Docker writes each line
// with its RFC 3339 timestamp (timestamps=1); the frame carries it as t and
// the rest of the line, newline included, as d. A non-TTY container's
// stream is multiplexed and is demuxed with stdcopy into out and err; a TTY
// container's stream is raw and all of it is out, so demuxing never
// corrupts it.
//
// The inspection and the start of the log call are bounded by the call
// timeout. Without follow, every read is too, so a daemon that stops
// answering mid-stream ends the stream; with follow, a quiet container is
// normal and only the caller ends it, or the container stopping.
func (p *Provider) logs(ctx context.Context, in spec.LogsInput, emit func(apitypes.Frame) error) error {
	if !validName(in.Name) {
		return invalid("/name", "must match "+spec.NamePattern)
	}
	opts := client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Timestamps: true, Follow: in.Follow}
	if in.Tail != nil {
		if *in.Tail < 0 || *in.Tail > spec.MaxTail {
			return invalid("/tail", fmt.Sprintf("must be between 0 and %d", spec.MaxTail))
		}
		opts.Tail = strconv.Itoa(*in.Tail)
	}
	if in.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, in.Since)
		if err != nil {
			return invalid("/since", "must be an RFC 3339 time")
		}
		opts.Since = t.UTC().Format(time.RFC3339Nano)
	}
	cli, err := p.client()
	if err != nil {
		return err
	}

	callCtx, cancel := p.callCtx(ctx)
	res, err := cli.ContainerInspect(callCtx, in.Name, client.ContainerInspectOptions{})
	cancel()
	if err != nil {
		return p.engineError(ctx, err, "inspect container", notFound())
	}
	tty := res.Container.Config != nil && res.Container.Config.Tty

	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	idle := newIdleTimer(p.timeout, stop)
	defer idle.Stop()
	body, err := cli.ContainerLogs(streamCtx, res.Container.ID, opts)
	if err != nil {
		return p.streamError(ctx, idle, err)
	}
	defer body.Close()
	var src io.Reader = body
	if in.Follow {
		idle.Stop()
	} else {
		src = idle.reader(body)
	}

	out := &lineSink{stream: apitypes.StreamStdout, emit: emit}
	errs := &lineSink{stream: apitypes.StreamStderr, emit: emit}
	if tty {
		_, err = io.Copy(out, src)
	} else {
		_, err = stdcopy.StdCopy(out, errs, src)
	}
	if err != nil {
		var ee emitError
		if errors.As(err, &ee) {
			return ee.err
		}
		return p.streamError(ctx, idle, err)
	}
	if err := out.flush(); err != nil {
		return err
	}
	return errs.flush()
}

// streamError reports a failed or stalled log stream.
func (p *Provider) streamError(ctx context.Context, idle *idleTimer, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if idle.fired() {
		err = fmt.Errorf("%w: no data within %s", context.DeadlineExceeded, p.timeout)
	}
	return p.engineError(ctx, err, "container logs", notFound())
}

// emitError marks an error returned by emit, so it is passed back as is: it
// means the client went away.
type emitError struct{ err error }

func (e emitError) Error() string { return e.err.Error() }
func (e emitError) Unwrap() error { return e.err }

// lineSink turns one stream's bytes into one frame per line.
type lineSink struct {
	stream string
	emit   func(apitypes.Frame) error
	buf    []byte
	last   time.Time // Timestamp of the previous line
}

func (s *lineSink) Write(b []byte) (int, error) {
	s.buf = append(s.buf, b...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		if err := s.frame(s.buf[:i+1]); err != nil {
			return 0, err
		}
		s.buf = s.buf[i+1:]
	}
	if len(s.buf) >= maxLineBytes {
		if err := s.frame(s.buf); err != nil {
			return 0, err
		}
		s.buf = s.buf[:0]
	}
	// Release a large backing array once it is drained.
	if len(s.buf) == 0 && cap(s.buf) > 64<<10 {
		s.buf = nil
	}
	return len(b), nil
}

// flush sends a last line that had no newline.
func (s *lineSink) flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	err := s.frame(s.buf)
	s.buf = nil
	if err != nil {
		var ee emitError
		if errors.As(err, &ee) {
			return ee.err
		}
	}
	return err
}

// frame splits Docker's "<RFC3339Nano> " prefix off a line and emits it. A
// line without one (a piece of an over-long line) keeps the previous
// line's time.
func (s *lineSink) frame(line []byte) error {
	t := s.last
	if sp := bytes.IndexByte(line, ' '); sp > 0 {
		if ts, err := time.Parse(time.RFC3339Nano, string(line[:sp])); err == nil {
			t, line = ts.UTC(), line[sp+1:]
		}
	}
	s.last = t
	if err := s.emit(apitypes.Frame{T: t, S: s.stream, D: validText(line)}); err != nil {
		return emitError{err}
	}
	return nil
}

// validText is line as UTF-8, each invalid byte replaced by U+FFFD.
func validText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// idleTimer cancels a stream that receives nothing for its timeout.
type idleTimer struct {
	timeout time.Duration
	mu      sync.Mutex
	t       *time.Timer
	done    bool
	expired bool
}

func newIdleTimer(timeout time.Duration, cancel context.CancelFunc) *idleTimer {
	it := &idleTimer{timeout: timeout}
	it.t = time.AfterFunc(timeout, func() {
		it.mu.Lock()
		if !it.done {
			it.expired = true
		}
		it.mu.Unlock()
		cancel()
	})
	return it
}

// Stop disarms the timer for good.
func (it *idleTimer) Stop() {
	it.mu.Lock()
	defer it.mu.Unlock()
	if !it.expired {
		it.done = true
	}
	it.t.Stop()
}

func (it *idleTimer) fired() bool {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.expired
}

func (it *idleTimer) reset() {
	it.mu.Lock()
	defer it.mu.Unlock()
	if !it.done && !it.expired {
		it.t.Reset(it.timeout)
	}
}

// reader re-arms the timer after every read that returns data.
func (it *idleTimer) reader(r io.Reader) io.Reader { return idleReader{r: r, it: it} }

type idleReader struct {
	r  io.Reader
	it *idleTimer
}

func (r idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.it.reset()
	}
	return n, err
}
