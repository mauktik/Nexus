// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

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
	// reconnectGap is the retry gap inside an op's reconnect window (FR-14).
	reconnectGap = time.Second
	// maxFrameLine bounds one NDJSON frame line.
	maxFrameLine = 4 << 20
)

// watchdog is followWatchdog; tests shorten it.
var watchdog = followWatchdog

// clock is the wait path's time source. Every elapsed-time read and every
// sleep of await and followFrames goes through waitClock, so a test runs a
// reconnect window in virtual time (A-45). The followed stream's watchdog
// stays on real time, with its own seam.
type clock struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// waitClock is the real clock. Tests replace it, and must then not run in
// parallel.
var waitClock = clock{now: time.Now, sleep: sleepCtx}

// sleepCtx waits for d, or returns ctx.Err() if ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// reconnector is an op's reconnect window (FR-14): it measures how long
// the host has been continuously unreachable, and when that was last shown.
// A zero window retries nothing.
type reconnector struct {
	window, gap          time.Duration
	now                  func() time.Time
	sleep                func(context.Context, time.Duration) error
	firstFail, lastShown time.Time
}

func newReconnector(window time.Duration) *reconnector {
	return &reconnector{window: window, gap: reconnectGap, now: waitClock.now, sleep: waitClock.sleep}
}

func (rc *reconnector) enabled() bool { return rc.window > 0 }

// failed records a retryable failure. It returns how long the host has been
// unreachable, whether to show that (at the first failure and every
// progressEvery), and whether the window is spent.
func (rc *reconnector) failed() (elapsed time.Duration, show, spent bool) {
	now := rc.now()
	if rc.firstFail.IsZero() {
		rc.firstFail, rc.lastShown = now, time.Time{}
	}
	elapsed = now.Sub(rc.firstFail)
	if elapsed >= rc.window {
		return elapsed, false, true
	}
	if rc.lastShown.IsZero() || now.Sub(rc.lastShown) >= progressEvery {
		rc.lastShown, show = now, true
	}
	return elapsed, show, false
}

// reached ends a run of failures: the host answered.
func (rc *reconnector) reached() { rc.firstFail, rc.lastShown = time.Time{}, time.Time{} }

// spentError is the exit 5 of a spent window, naming operation id. It
// quotes err rather than wrapping it, so the message stays this one.
func (rc *reconnector) spentError(host, id string, err error) error {
	return &ExitError{Code: ExitConnection, Err: fmt.Errorf(
		"nexusd on host %s stayed unreachable for %s, the reconnect window (%v); the operation may still be running; "+
			"see it with `%s operation get %s --host %s`", hostOr(host), seconds(rc.window), err, programName, id, host)}
}

// retryable reports whether err is a failure to reach the host that a
// reconnect window rides out: a transport failure, or a 503. A certificate
// pin mismatch never is (NSM FR-35): it reaches errors.As through the
// transport error (A-34).
func retryable(err error) bool {
	var (
		pin    *client.PinMismatchError
		tr     *client.TransportError
		prob   *problem.Error
		status *client.StatusError
	)
	switch {
	case errors.As(err, &pin):
		return false
	case errors.As(err, &tr):
		return true
	case errors.As(err, &prob):
		return prob.Status == http.StatusServiceUnavailable
	case errors.As(err, &status):
		return status.Status == http.StatusServiceUnavailable
	}
	return false
}

// seconds renders d in whole seconds, such as 60s.
func seconds(d time.Duration) string { return fmt.Sprintf("%ds", wholeSeconds(d)) }

func wholeSeconds(d time.Duration) int64 { return int64(d.Round(time.Second) / time.Second) }

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

	rc := newReconnector(op.CLI.ReconnectWindow)
	if w.follow && !cur.Status.Terminal() {
		r := &resume{lastO: -1}
		for {
			err := a.followOperation(wctx, c, t, cur.ID, op.CLI.ReconnectWindow, r)
			if wctx.Err() != nil {
				return ended(wctx.Err())
			}
			if err != nil {
				if rc.enabled() && retryable(err) {
					// Inside a window, followFrames gives up on such a
					// failure only once the window is spent.
					err = rc.spentError(t.Name, cur.ID, err)
				}
				return body, a.readError(op, cur.ID, t, err)
			}
			// The stream ends when the operation does; read its end state.
			b, next, err := a.pollOperation(wctx, c, t, cur.ID, rc)
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
			if err := waitClock.sleep(wctx, pollFirst); err != nil {
				return ended(wctx.Err())
			}
		}
	}

	start := waitClock.now()
	gap := pollFirst
	var shown apitypes.OperationStatus
	var shownAt time.Time
	for !cur.Status.Terminal() {
		if err := waitClock.sleep(wctx, gap); err != nil {
			return ended(wctx.Err())
		}
		b, next, err := a.pollOperation(wctx, c, t, cur.ID, rc)
		if wctx.Err() != nil {
			return ended(wctx.Err())
		}
		if err != nil {
			return body, a.readError(op, cur.ID, t, err)
		}
		body, cur = b, next
		now := waitClock.now()
		if el := now.Sub(start); !cur.Status.Terminal() && el >= progressAfter &&
			(cur.Status != shown || now.Sub(shownAt) >= progressEvery) {
			a.progress(t.Name, cur, el)
			shown, shownAt = cur.Status, now
		}
		gap = min(gap*2, pollMax)
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

// resultWarnings are the warnings of the result of the operation in body,
// the last state await returned (EchoWarnings). An operation that has not
// succeeded has no result, and so none.
func resultWarnings(body []byte) []string {
	var op struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(body, &op) != nil || len(op.Result) == 0 {
		return nil
	}
	return outputWarnings(op.Result)
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

// pollOperation reads operation id. Inside the reconnect window of rc, a
// retryable failure is retried every rc.gap while the host has been
// unreachable for less than the window, with progress on stderr (FR-14).
// Without a window it is getOperation.
func (a *app) pollOperation(ctx context.Context, c *client.Client, t hostTarget, id string, rc *reconnector) ([]byte, apitypes.Operation, error) {
	for {
		b, op, err := getOperation(ctx, c, id)
		if err == nil {
			rc.reached()
			return b, op, nil
		}
		if ctx.Err() != nil || !rc.enabled() || !retryable(err) {
			return nil, apitypes.Operation{}, err
		}
		elapsed, show, spent := rc.failed()
		if spent {
			return nil, apitypes.Operation{}, rc.spentError(t.Name, id, err)
		}
		if show {
			a.reconnectProgress(t.Name, id, elapsed, rc.window)
		}
		if err := rc.sleep(ctx, rc.gap); err != nil {
			return nil, apitypes.Operation{}, err
		}
	}
}

// reconnectProgress writes one reconnect progress line to stderr: JSON in
// a machine format.
func (a *app) reconnectProgress(host, id string, elapsed, window time.Duration) {
	msg := fmt.Sprintf("nexusd on host %s is unreachable, likely restarting; retrying (%s of %s)",
		hostOr(host), seconds(elapsed), seconds(window))
	if !a.format.Machine() {
		fmt.Fprintf(a.env.Stderr, "%s: %s\n", programName, msg)
		return
	}
	// A failed write to stderr has nowhere to go.
	_ = output.WriteLine(a.env.Stderr, map[string]any{"level": "progress", "event": "reconnecting", "host": host,
		"id": id, "message": msg, "elapsedSeconds": wholeSeconds(elapsed), "windowSeconds": wholeSeconds(window)})
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
// each frame is a JSON line labeled with host and id, else its text. The
// stream rides out an outage within window, the reconnect window of the
// operation's own op, and shows it on stderr as a plain wait does (FR-14).
func (a *app) followOperation(ctx context.Context, c *client.Client, t hostTarget, id string, window time.Duration, r *resume) error {
	op := systemspec.OperationsLogs.Op()
	op.CLI.ReconnectWindow = window
	in := map[string]any{"id": id, "follow": true}
	// The same reconnect progress as a plain wait (FR-14).
	reconnecting := func(elapsed time.Duration) { a.reconnectProgress(t.Name, id, elapsed, window) }
	return followFrames(ctx, c, op, in, r, reconnecting, func(f map[string]any) error {
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

	// replaying is true from a reopen until the first frame later than lastT
	// arrives: the window the host repeats. Only there is a frame stamped
	// before lastT a repeat. On a live stream it is not: Docker stamps stdout
	// and stderr in separate goroutines, so frames can arrive slightly out of
	// order, and none may be dropped.
	replaying bool
}

// reopen starts a new stream: the frames at lastT come again.
func (r *resume) reopen() {
	r.skip = r.atLast
	r.replaying = r.lastT != ""
}

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
			// A repeat while replaying; otherwise a live frame that arrived
			// out of order. It is new, and lastT stays where it is: the
			// frames at lastT, not this one, are what a reopen repeats.
			return !r.replaying
		case t.Equal(last):
			if r.skip > 0 {
				r.skip--
				return false
			}
			r.atLast++
			return true
		}
	}
	r.lastT, r.atLast, r.skip, r.replaying = ts, 1, 0, false
	return true
}

// followFrames reads op's follow stream for in and calls emit for each new
// frame until the stream ends by itself. A stream that drops, or stays
// silent for the watchdog, is reopened after the last frame r saw, so emit
// sees no gap and no repeat. The stream client has no overall timeout; the
// watchdog, ctx and the transport's handshake and header timeouts bound
// it. Only failures to connect count toward giving up: a silent stream
// reopens every watchdog period for as long as it lasts. Without a
// reconnect window on op it gives up after followRetries consecutive
// failures to connect; with one, after the window of continuous failure, a
// 503 counting as one and a pin mismatch ending it at once (FR-14).
// reconnecting, when not nil, is called with how long the host has been
// unreachable at the first failure of an outage and every progressEvery
// after it, as the reconnector's failed reports.
func followFrames(ctx context.Context, c *client.Client, op def.Op, in map[string]any, r *resume, reconnecting func(elapsed time.Duration), emit func(map[string]any) error) error {
	rc := newReconnector(op.CLI.ReconnectWindow)
	gap := followRetryGap
	if rc.enabled() {
		gap = rc.gap
	}
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
		reopen := errors.Is(err, errStreamCut) || (rc.enabled() && retryable(err)) || (!rc.enabled() && errors.As(err, &tr))
		if !reopen {
			return err // A refusal, such as 403, or a bad frame
		}
		switch {
		case connected:
			failures = 0
			rc.reached()
		case rc.enabled():
			elapsed, show, spent := rc.failed()
			if spent {
				return err
			}
			if show && reconnecting != nil {
				reconnecting(elapsed)
			}
		default:
			if failures++; failures > followRetries {
				return err
			}
		}
		if got {
			continue
		}
		if err := rc.sleep(ctx, gap); err != nil {
			return err
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
