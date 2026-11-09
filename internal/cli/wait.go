package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/cli/output"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/systemspec"
)

// Flags of a long-running command (FR-33).
const (
	flagNoWait  = "no-wait"
	flagTimeout = "timeout"
	flagFollow  = "follow"
)

// Waiting and following (FR-33, A-54).
const (
	pollFirst = 250 * time.Millisecond // First poll gap; it doubles up to pollMax
	pollMax   = 2 * time.Second
	// progressAfter is how long a wait runs before progress is shown, so an
	// operation that ends at once leaves stderr empty.
	progressAfter = time.Second
	// progressEvery repeats an unchanged status this often.
	progressEvery = 10 * time.Second
	// followWatchdog is how long a followed stream may stay silent before
	// it is dropped and reopened from the last offset. A silent operation
	// therefore reconnects once a minute, which is harmless.
	followWatchdog = 60 * time.Second
	// followRetries bounds consecutive failures to reopen a stream.
	followRetries  = 5
	followRetryGap = time.Second
	// maxFrameLine bounds one NDJSON frame line.
	maxFrameLine = 4 << 20
)

// watchdog is followWatchdog; tests shorten it.
var watchdog = followWatchdog

// waitOptions are the flags of a long-running command.
type waitOptions struct {
	noWait  bool
	follow  bool
	timeout time.Duration // 0 means no bound
}

// registerWaitFlags declares --no-wait, --timeout and --follow on a command
// that runs a long-running op. An input that takes one of them is a
// declaration error.
func registerWaitFlags(cmd *cobra.Command, g *opCommand) error {
	long := false
	for _, op := range g.ops {
		long = long || op.Mode == def.ModeLongRunning
	}
	if !long {
		return nil
	}
	fs := cmd.Flags()
	for _, name := range []string{flagNoWait, flagTimeout, flagFollow} {
		if fs.Lookup(name) != nil {
			return fmt.Errorf("%s %s: an input takes the flag --%s, which a long-running command reserves", g.noun, g.verb, name)
		}
	}
	fs.Bool(flagNoWait, false, "print the operation as accepted and exit, without waiting for it to end")
	fs.Duration(flagTimeout, 0, "stop waiting after this long, such as 90s or 5m, and exit 7; the operation is not canceled")
	fs.Bool(flagFollow, false, "stream the operation's output to stderr while waiting")
	return nil
}

// waitOptionsOf reads the wait flags; a command without them never waits.
func waitOptionsOf(cmd *cobra.Command) (waitOptions, error) {
	fs := cmd.Flags()
	if fs.Lookup(flagNoWait) == nil {
		return waitOptions{noWait: true}, nil
	}
	var w waitOptions
	var err error
	if w.noWait, err = fs.GetBool(flagNoWait); err != nil {
		return w, err
	}
	if w.follow, err = fs.GetBool(flagFollow); err != nil {
		return w, err
	}
	if w.timeout, err = fs.GetDuration(flagTimeout); err != nil {
		return w, err
	}
	if w.timeout < 0 {
		return w, usagef("--%s must not be negative", flagTimeout)
	}
	if w.noWait && (w.follow || fs.Changed(flagTimeout)) {
		return w, usagef("--%s cannot be combined with --%s or --%s", flagNoWait, flagFollow, flagTimeout)
	}
	return w, nil
}

// operationError is a failure that concerns one accepted operation: it
// failed, the wait ended, or the operation cannot be read. Its message and
// structured error name the operation.
type operationError struct {
	id  string
	err error
}

func (e *operationError) Error() string { return "operation " + e.id + ": " + e.err.Error() }
func (e *operationError) Unwrap() error { return e.err }

// interrupter turns SIGINT into a detach while at least one wait is in
// progress (FR-33). Outside a wait, SIGINT keeps its default and ends the
// process: nothing accepted is then being waited for, and a pending read of
// stdin or the terminal must not swallow it. Once signal.Stop has removed
// the last channel, the Go runtime again exits on SIGINT (os/signal: "A
// SIGHUP, SIGINT, or SIGTERM signal causes the program to exit"). Once hit, it stays hit, so
// every later wait of the invocation detaches at once.
type interrupter struct {
	ctx    context.Context // Done once SIGINT arrives during a wait
	cancel context.CancelFunc

	signaled atomic.Bool // SIGINT arrived during a wait

	mu      sync.Mutex
	waiting int
	stop    func() // Ends the current signal registration
}

func newInterrupter() *interrupter {
	ctx, cancel := context.WithCancel(context.Background())
	return &interrupter{ctx: ctx, cancel: cancel}
}

// begin marks a wait as started; end, which it returns, as ended.
func (it *interrupter) begin() (end func()) {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.waiting == 0 && it.ctx.Err() == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		quit := make(chan struct{})
		var once sync.Once
		it.stop = func() {
			once.Do(func() {
				signal.Stop(ch)
				close(quit)
			})
		}
		go func() {
			select {
			case <-ch:
				it.signaled.Store(true)
				it.cancel()
			case <-quit:
			}
		}()
	}
	it.waiting++
	var once sync.Once
	return func() {
		once.Do(func() {
			it.mu.Lock()
			defer it.mu.Unlock()
			it.waiting--
			if it.waiting == 0 && it.stop != nil {
				it.stop()
				it.stop = nil
			}
		})
	}
}

// hit reports whether SIGINT arrived during a wait.
func (it *interrupter) hit() bool { return it.signaled.Load() }

// close releases the interrupter at the end of an invocation.
func (it *interrupter) close() {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.stop != nil {
		it.stop()
		it.stop = nil
	}
	it.cancel()
}

// stderrLabel prefixes host-labeled stderr text when there are several
// hosts.
func (a *app) stderrLabel(host string) string {
	if a.multi {
		return "[" + host + "] "
	}
	return ""
}

// await waits for the operation in body, the 202 answer of op on t, until
// it ends, --timeout passes or SIGINT detaches (FR-33). It returns the last
// state of the operation, to print, and an error when it did not succeed:
// exit 1 when it failed or was canceled, 7 on timeout, 130 on SIGINT, and
// 3 when the token cannot read it. Ending the wait never cancels the
// operation.
func (a *app) await(ctx context.Context, c *client.Client, t hostTarget, op def.Op, w waitOptions, body []byte) ([]byte, error) {
	var cur apitypes.Operation
	if err := json.Unmarshal(body, &cur); err != nil || cur.ID == "" {
		return body, errNotJSON
	}
	end := a.intr.begin()
	defer end()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(a.intr.ctx, cancel)
	defer stop()
	if w.timeout > 0 {
		var tcancel context.CancelFunc
		wctx, tcancel = context.WithTimeout(wctx, w.timeout)
		defer tcancel()
	}
	ended := func(err error) ([]byte, error) {
		switch {
		case a.intr.hit():
			return body, &operationError{id: cur.ID, err: &ExitError{Code: ExitInterrupted, Err: fmt.Errorf(
				"interrupted: detached; the operation continues and was not canceled; see it with `%s operation get %s --host %s`",
				programName, cur.ID, t.Name)}}
		case errors.Is(wctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
			return body, &operationError{id: cur.ID, err: &ExitError{Code: ExitWaitTimeout, Err: fmt.Errorf(
				"still %s after --%s %s; the operation continues and was not canceled; see it with `%s operation get %s --host %s`",
				cur.Status, flagTimeout, w.timeout, programName, cur.ID, t.Name)}}
		}
		return body, &operationError{id: cur.ID, err: err}
	}

	if w.follow && !cur.Status.Terminal() {
		r := &resume{lastO: -1}
		for {
			err := a.followOperation(wctx, c, t, cur.ID, r)
			if wctx.Err() != nil {
				return ended(wctx.Err())
			}
			if err != nil {
				return body, a.readError(op, cur.ID, t, err)
			}
			// The stream ends when the operation does; read its end state.
			b, next, err := getOperation(wctx, c, cur.ID)
			if wctx.Err() != nil {
				return ended(wctx.Err())
			}
			if err != nil {
				return body, a.readError(op, cur.ID, t, err)
			}
			body, cur = b, next
			if cur.Status.Terminal() {
				break
			}
			// The stream ended early, as when nexusd shut down: reopen it.
			select {
			case <-wctx.Done():
				return ended(wctx.Err())
			case <-time.After(pollFirst):
			}
		}
	}

	start := time.Now()
	gap := pollFirst
	var shown apitypes.OperationStatus
	var shownAt time.Time
	timer := time.NewTimer(gap)
	defer timer.Stop()
	for !cur.Status.Terminal() {
		select {
		case <-wctx.Done():
			return ended(wctx.Err())
		case <-timer.C:
		}
		b, next, err := getOperation(wctx, c, cur.ID)
		if wctx.Err() != nil {
			return ended(wctx.Err())
		}
		if err != nil {
			return body, a.readError(op, cur.ID, t, err)
		}
		body, cur = b, next
		if el := time.Since(start); !cur.Status.Terminal() && el >= progressAfter &&
			(cur.Status != shown || time.Since(shownAt) >= progressEvery) {
			a.progress(t.Name, cur, el)
			shown, shownAt = cur.Status, time.Now()
		}
		gap = min(gap*2, pollMax)
		timer.Reset(gap)
	}
	return body, operationOutcome(cur)
}

// operationOutcome is the error of a terminal operation: nil when it
// succeeded, exit 1 with its structured error otherwise (FR-34).
func operationOutcome(op apitypes.Operation) error {
	switch op.Status {
	case apitypes.StatusSucceeded:
		return nil
	case apitypes.StatusFailed:
		var err error = errors.New("failed")
		if op.Error != nil {
			err = op.Error
		}
		return &operationError{id: op.ID, err: &ExitError{Code: ExitFailure, Err: err}}
	}
	return &operationError{id: op.ID, err: &ExitError{Code: ExitFailure, Err: fmt.Errorf("ended %s", op.Status)}}
}

// readError is a failure to read an accepted operation. A 403 means the
// token may start it but not read it, as with scripts:run:{id} without
// scripts:read: exit 3, naming the scope reading needs. The operation is
// unaffected either way.
func (a *app) readError(op def.Op, id string, t hostTarget, err error) error {
	var (
		prob   *problem.Error
		status *client.StatusError
	)
	forbidden := (errors.As(err, &prob) && (prob.Slug() == problem.Forbidden || prob.Status == http.StatusForbidden)) ||
		(errors.As(err, &status) && status.Status == http.StatusForbidden)
	if !forbidden {
		return &operationError{id: id, err: err}
	}
	scope := a.readScope(op.Provider)
	if prob != nil {
		if s, ok := prob.Extensions["requiredScope"].(string); ok && s != "" {
			scope = s
		}
	}
	if scope == "" {
		scope = "the read scope of provider " + op.Provider
	}
	return &operationError{id: id, err: &ExitError{Code: ExitAuth, Err: fmt.Errorf(
		"accepted, but this token cannot read it: waiting needs scope %s. The operation is unaffected; "+
			"see it with a token that holds %s: `%s operation get %s --host %s`", scope, scope, programName, id, t.Name)}}
}

// readScope is the scope that reads provider's operations (FR-22).
func (a *app) readScope(provider string) string {
	for _, s := range a.specs {
		if s.Name == provider {
			return s.ReadScope
		}
	}
	return ""
}

// getOperation reads operation id.
func getOperation(ctx context.Context, c *client.Client, id string) ([]byte, apitypes.Operation, error) {
	req, err := buildRequest(systemspec.OperationsGet.Op(), map[string]any{"id": id})
	if err != nil {
		return nil, apitypes.Operation{}, err
	}
	resp, err := c.Send(ctx, req)
	if err != nil {
		return nil, apitypes.Operation{}, err
	}
	var op apitypes.Operation
	if err := json.Unmarshal(resp.Body, &op); err != nil || op.ID != id {
		return nil, apitypes.Operation{}, errNotJSON
	}
	return resp.Body, op, nil
}

// progress writes one wait progress line to stderr: JSON in a machine
// format.
func (a *app) progress(host string, op apitypes.Operation, elapsed time.Duration) {
	el := elapsed.Round(time.Second)
	if !a.format.Machine() {
		fmt.Fprintf(a.env.Stderr, "%s: host %s: operation %s %s, %s\n", programName, hostOr(host), op.ID, op.Status, el)
		return
	}
	// A failed write to stderr has nowhere to go.
	_ = output.WriteLine(a.env.Stderr, map[string]any{"level": "progress", "host": host, "id": op.ID,
		"status": op.Status, "elapsedSeconds": int64(el / time.Second)})
}

// followOperation streams operation id's output to stderr until the stream
// ends, which it does when the operation does (FR-33). In a machine format
// each frame is a JSON line labeled with host and id, else its text.
func (a *app) followOperation(ctx context.Context, c *client.Client, t hostTarget, id string, r *resume) error {
	op := systemspec.OperationsLogs.Op()
	in := map[string]any{"id": id, "follow": true}
	return followFrames(ctx, c, op, in, r, func(f map[string]any) error {
		f["id"] = frameLocator(id, f)
		if a.format.Machine() {
			f["level"] = "output"
			return output.WriteLine(a.env.Stderr, label(f, t))
		}
		text := frameString(f)
		if p := a.stderrLabel(t.Name); p != "" {
			text = prefixLines(text, p)
		}
		_, err := io.WriteString(a.env.Stderr, text)
		return err
	})
}

// frameString is a frame's text for a terminal: its sanitized data, or a
// note for a truncation marker.
func frameString(f map[string]any) string {
	if typ, _ := f["type"].(string); typ == apitypes.FrameTypeTruncated {
		lost, _ := f["lostBytes"].(json.Number)
		return fmt.Sprintf("[%s bytes of output were not kept]\n", lost)
	}
	d, _ := f[frameText].(string)
	return output.Sanitize(d, true)
}

func prefixLines(text, prefix string) string {
	var b strings.Builder
	for line := range strings.Lines(text) {
		b.WriteString(prefix)
		b.WriteString(line)
	}
	return b.String()
}

// resume is where a followed stream reopens: after the last frame seen.
// Operation output frames are told apart by offset. Container log frames
// have none, so they are told apart by time and by how many were seen at
// the last time, as since repeats the lines logged at that time.
type resume struct {
	lastO  int64  // Offset of the last frame seen; -1 for none
	lastT  string // Time of the last frame seen, for frames without offset
	atLast int    // Frames seen at lastT
	skip   int    // Frames at lastT still to drop on this stream
}

// reopen starts a new stream: the frames at lastT come again.
func (r *resume) reopen() { r.skip = r.atLast }

// next reports whether f is new, and records it.
func (r *resume) next(f map[string]any) bool {
	if typ, _ := f["type"].(string); typ == apitypes.FrameTypeTruncated {
		return true
	}
	if n, ok := f["o"].(json.Number); ok {
		o, err := n.Int64()
		if err != nil {
			return true
		}
		if o <= r.lastO {
			return false
		}
		r.lastO = o
		return true
	}
	ts, _ := f["t"].(string)
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return true
	}
	if r.lastT != "" {
		last, err := time.Parse(time.RFC3339Nano, r.lastT)
		switch {
		case err != nil:
		case t.Before(last):
			return false
		case t.Equal(last):
			if r.skip > 0 {
				r.skip--
				return false
			}
			r.atLast++
			return true
		}
	}
	r.lastT, r.atLast, r.skip = ts, 1, 0
	return true
}

// followFrames reads op's follow stream for in and calls emit for each new
// frame until the stream ends by itself. A stream that drops, or stays
// silent for the watchdog, is reopened after the last frame r saw, so emit
// sees no gap and no repeat. The stream client has no overall timeout; the
// watchdog, ctx and the transport's handshake and header timeouts bound
// it. Only failures to connect count toward giving up: a silent stream
// reopens every watchdog period for as long as it lasts.
func followFrames(ctx context.Context, c *client.Client, op def.Op, in map[string]any, r *resume, emit func(map[string]any) error) error {
	failures := 0
	for {
		req, err := buildRequest(op, resumeInput(in, r))
		if err != nil {
			return err
		}
		r.reopen()
		clean, connected, got, err := streamOnce(ctx, c, req, r, emit)
		if clean || ctx.Err() != nil {
			return ctx.Err()
		}
		var tr *client.TransportError
		if !errors.Is(err, errStreamCut) && !errors.As(err, &tr) {
			return err // A refusal, such as 403, or a bad frame
		}
		if connected {
			failures = 0
		} else if failures++; failures > followRetries {
			return err
		}
		if got {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(followRetryGap):
		}
	}
}

// errStreamCut is a stream that dropped or fell silent.
var errStreamCut = errors.New("the log stream dropped")

// resumeInput is in for reopening after r: from the last frame's offset,
// or for container logs, from its time without tail.
func resumeInput(in map[string]any, r *resume) map[string]any {
	out := maps.Clone(in)
	switch {
	case r.lastO >= 0:
		out["offset"] = int(r.lastO)
	case r.lastT != "":
		out["since"] = r.lastT
		delete(out, "tail")
	}
	return out
}

// streamOnce reads one stream. clean reports that it ended by itself,
// connected that the host answered, and got that it delivered a new frame.
func streamOnce(ctx context.Context, c *client.Client, req client.Request, r *resume, emit func(map[string]any) error) (clean, connected, got bool, err error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	body, err := c.Stream(sctx, req)
	if err != nil {
		return false, false, false, err
	}
	defer body.Close()

	lines := make(chan []byte)
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64<<10), maxFrameLine)
		for sc.Scan() {
			select {
			case lines <- append([]byte(nil), sc.Bytes()...):
			case <-sctx.Done():
				done <- sctx.Err()
				return
			}
		}
		done <- sc.Err()
	}()

	dog := time.NewTimer(watchdog)
	defer dog.Stop()
	for {
		select {
		case line := <-lines:
			dog.Reset(watchdog)
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			f, derr := output.DecodeObject(line)
			if derr != nil {
				cancel()
				<-done
				return false, true, got, errNotJSON
			}
			if !r.next(f) {
				continue
			}
			got = true
			if err := emit(f); err != nil {
				cancel()
				<-done
				return false, true, got, err
			}
		case rerr := <-done:
			if rerr == nil {
				return true, true, got, nil
			}
			return false, true, got, errStreamCut
		case <-dog.C:
			// Silent for the watchdog: drop it and reopen.
			cancel()
			<-done
			return false, true, got, errStreamCut
		case <-ctx.Done():
			cancel()
			<-done
			return false, true, got, ctx.Err()
		}
	}
}
