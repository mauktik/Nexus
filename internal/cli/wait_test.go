// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
)

// The gate provider is the wait tests' provider: ops that end at once, end
// when released, print a counter, or fail, served by the real core and
// server.
const (
	gateName       = "gate"
	gateReadScope  = "gate:read"
	gateWriteScope = "gate:write"
)

type gateNameIn struct {
	Name string `json:"name" jsonschema:"Gadget name: quick, gated, counter or fail."`
}

type gateUpdateIn struct {
	WhatIf bool `json:"whatIf,omitempty" jsonschema:"Only report what would change."`
}

type gateOut struct {
	apitypes.ActionResult
	Name   string `json:"name,omitempty"`
	WhatIf bool   `json:"whatIf,omitempty"`
}

type gadget = apitypes.Resource[struct{}]

func refineGateName(s *jsonschema.Schema) {
	if p := s.Properties["name"]; p != nil {
		p.Pattern = `^[a-z][a-z0-9-]{0,30}$`
	}
}

var (
	gateGet = def.Define[gateNameIn, gadget](def.Op{
		ID: gateName + ".gadgets.get", Provider: gateName, Kind: def.KindGet, ResourceType: "gadgets",
		Summary: "Get a gadget", Description: "Returns one of the wait tests' gadgets. Read-only.",
		Scope: def.Scope{Name: gateReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI:    def.CLIBinding{Noun: "gadget", Verb: "get", Positional: "name"},
		Refine: refineGateName,
	})
	gateStart = def.Define[gateNameIn, gateOut](def.Op{
		ID: gateName + ".gadgets.start", Provider: gateName, Kind: def.KindResourceAction, ResourceType: "gadgets",
		Action: "start", Summary: "Start a gadget",
		Description: "Starts a wait test gadget. Side effects: none outside the test. " +
			"Long-running: answers 202 with an operation.",
		Scope: def.Scope{Name: gateWriteScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI:    def.CLIBinding{Noun: "gadget", Verb: "start", Positional: "name"},
		Refine: refineGateName,
	})
	gateRemove = def.Define[gateNameIn, gateOut](def.Op{
		ID: gateName + ".gadgets.remove", Provider: gateName, Kind: def.KindResourceAction, ResourceType: "gadgets",
		Action: "remove", Summary: "Remove a gadget",
		Description: "Removes a wait test gadget. Side effects: none outside the test. " +
			"Long-running: answers 202 with an operation.",
		Scope: def.Scope{Name: gateWriteScope}, Risk: def.RiskDestructive, Mode: def.ModeLongRunning,
		CLI:    def.CLIBinding{Noun: "gadget", Verb: "remove", Positional: "name"},
		Refine: refineGateName,
	})
	gateUpdate = def.Define[gateUpdateIn, gateOut](def.Op{
		ID: gateName + ".actions.update", Provider: gateName, Kind: def.KindProviderAction, Action: "update",
		Summary: "Update every gadget",
		Description: "Updates the wait tests' gadgets, or with whatIf only previews it. Side effects: none " +
			"outside the test. Long-running: answers 202 with an operation.",
		Scope: def.Scope{Name: gateWriteScope}, Risk: def.RiskDestructive, Mode: def.ModeLongRunning, WhatIf: true,
		CLI: def.CLIBinding{Noun: "gadget", Verb: "update"},
	})
)

func gateSpec() def.ProviderSpec {
	return def.ProviderSpec{Name: gateName, ReadScope: gateReadScope,
		Ops: []def.Op{gateGet.Op(), gateStart.Op(), gateRemove.Op(), gateUpdate.Op()}}
}

// gate is one server's gate provider. A gated gadget runs until release.
type gate struct {
	release chan struct{}
	once    sync.Once
	counter int // Lines the counter gadget prints, one write each
}

func newGate() *gate { return &gate{release: make(chan struct{}), counter: 40} }

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

func (g *gate) Spec() def.ProviderSpec          { return gateSpec() }
func (g *gate) Configure(func(any) error) error { return nil }
func (g *gate) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (g *gate) run(ctx context.Context, name string, out io.Writer) (gateOut, error) {
	switch name {
	case "gated":
		select {
		case <-g.release:
		case <-ctx.Done():
			return gateOut{}, ctx.Err()
		}
	case "counter":
		for i := 1; i <= g.counter; i++ {
			if _, err := fmt.Fprintf(out, "%d\n", i); err != nil {
				return gateOut{}, err
			}
			time.Sleep(15 * time.Millisecond)
		}
	case "fail":
		return gateOut{}, problem.New(problem.ScriptFailed, "the gadget jammed")
	}
	return gateOut{ActionResult: apitypes.ActionResult{Changed: true}, Name: name}, nil
}

func (g *gate) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(gateGet, func(_ context.Context, in gateNameIn) (gadget, error) {
			if in.Name == "missing" {
				return gadget{}, problem.New(problem.NotFound, "no gadget "+in.Name)
			}
			return apitypes.NewResource("gate-host", gateName, "gadgets", in.Name, struct{}{}), nil
		}),
		provider.InProcess(gateStart, provider.InProcessSpec[gateNameIn, gateOut]{
			Run: func(ctx context.Context, in gateNameIn, r *provider.Run) (gateOut, error) {
				return g.run(ctx, in.Name, r.Output())
			},
		}),
		provider.InProcess(gateRemove, provider.InProcessSpec[gateNameIn, gateOut]{
			Run: func(ctx context.Context, in gateNameIn, r *provider.Run) (gateOut, error) {
				return g.run(ctx, in.Name, r.Output())
			},
		}),
		provider.InProcess(gateUpdate, provider.InProcessSpec[gateUpdateIn, gateOut]{
			Run: func(context.Context, gateUpdateIn, *provider.Run) (gateOut, error) {
				return gateOut{WhatIf: true}, nil
			},
		}),
	}
}

// recorded is one request the proxy saw.
type recorded struct {
	method, path, query string
	at                  time.Time
}

// Log stream faults the proxy injects into the first operation log stream.
const (
	streamPass  int32 = iota
	streamDrop        // Cut the connection after cutAfter frames
	streamStall       // Stop forwarding after cutAfter frames, holding the connection open
)

// recProxy is a reverse proxy in front of a real server: it records every
// request, and can break the first operation log stream.
type recProxy struct {
	*httptest.Server
	fault    atomic.Int32
	cutAfter int

	mu       sync.Mutex
	reqs     []recorded
	streams  int   // Log streams opened
	lastSent int64 // Offset of the last frame forwarded before the fault
}

func startRecProxy(t *testing.T, upstream string) *recProxy {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	rp := &recProxy{cutAfter: 10, lastSent: -1}
	rev := httputil.NewSingleHostReverseProxy(u)
	rev.FlushInterval = -1
	rp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp.mu.Lock()
		rp.reqs = append(rp.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, time.Now()})
		first := false
		if strings.HasSuffix(r.URL.Path, "/logs") {
			rp.streams++
			first = rp.streams == 1
		}
		rp.mu.Unlock()
		if fault := rp.fault.Load(); first && fault != streamPass {
			rp.faulty(w, r, u, fault)
			return
		}
		rev.ServeHTTP(w, r)
	}))
	t.Cleanup(rp.Close)
	return rp
}

// faulty forwards cutAfter frames of a log stream, then drops or stalls it.
func (rp *recProxy) faulty(w http.ResponseWriter, r *http.Request, upstream *url.URL, fault int32) {
	out, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.String()+r.URL.RequestURI(), nil)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	out.Header = r.Header.Clone()
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	sc := bufio.NewScanner(resp.Body)
	sent := 0
	for sc.Scan() {
		if _, err := w.Write(append(sc.Bytes(), '\n')); err != nil {
			return
		}
		_ = rc.Flush()
		var f struct {
			O *int64 `json:"o"`
		}
		if json.Unmarshal(sc.Bytes(), &f) == nil && f.O != nil {
			rp.mu.Lock()
			rp.lastSent = *f.O
			rp.mu.Unlock()
		}
		if sent++; sent == rp.cutAfter {
			break
		}
	}
	if fault == streamStall {
		<-r.Context().Done() // Hold the connection open, silent
		return
	}
	panic(http.ErrAbortHandler) // Drop the connection mid-stream
}

func (rp *recProxy) requests() []recorded {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return slices.Clone(rp.reqs)
}

// matching are the recorded requests whose method is method and whose path
// satisfies match.
func (rp *recProxy) matching(method string, match func(string) bool) []recorded {
	var out []recorded
	for _, r := range rp.requests() {
		if r.method == method && match(r.path) {
			out = append(out, r)
		}
	}
	return out
}

func (rp *recProxy) cancels() []recorded {
	return rp.matching(http.MethodPost, func(p string) bool { return strings.HasSuffix(p, "/cancel") })
}

func (rp *recProxy) polls(id string) []recorded {
	return rp.matching(http.MethodGet, func(p string) bool { return p == "/v1/operations/"+id })
}

// gateEnv is a real server over a fresh gate, behind a recording proxy,
// configured as the default host alpha.
type gateEnv struct {
	g    *gate
	srv  *providerServer
	px   *recProxy
	home string
	env  Env
}

func startGate(t *testing.T) *gateEnv {
	t.Helper()
	g := newGate()
	srv := startProviderServer(t, "gate-host", g, true, gateReadScope, gateWriteScope)
	// Registered after the server, so it runs before its shutdown: a gated
	// op left running must not hold the shutdown.
	t.Cleanup(g.open)
	px := startRecProxy(t, srv.url)
	home := writeClientConfig(t, "alpha", clientHost{"alpha", px.URL, srv.token, nil})
	env := homeEnv(home)
	env.OpenTerminal = noTerminal
	return &gateEnv{g: g, srv: srv, px: px, home: home, env: env}
}

// noTerminal is a process with no controlling terminal.
func noTerminal() (Terminal, error) { return nil, errors.New("open /dev/tty: device not configured") }

// opStatus reads operation id from the server directly.
func (ge *gateEnv) opStatus(t *testing.T, id string) apitypes.Operation {
	t.Helper()
	c, err := client.New(client.Options{URL: ge.srv.url, Token: ge.srv.token})
	if err != nil {
		t.Fatal(err)
	}
	var op apitypes.Operation
	if err := c.Do(context.Background(), http.MethodGet, "/operations/"+id, nil, &op); err != nil {
		t.Fatal(err)
	}
	return op
}

// waitStatus polls operation id until it has status want.
func (ge *gateEnv) waitStatus(t *testing.T, id string, want apitypes.OperationStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		op := ge.opStatus(t, id)
		if op.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s is %s, want %s", id, op.Status, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// onlyOp decodes stdout as exactly one operation.
func onlyOp(t *testing.T, stdout string) apitypes.Operation {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout has %d lines, want one operation:\n%s", len(lines), stdout)
	}
	var op apitypes.Operation
	if err := json.Unmarshal([]byte(lines[0]), &op); err != nil || op.ID == "" {
		t.Fatalf("stdout %q is not an operation: %v", stdout, err)
	}
	return op
}

// TC-171: waiting polls from 250 ms rising to at most 2 s, shows progress
// on stderr, and prints only the final object on stdout.
func TestWaitPollsWithBackoffAndProgressOnStderr(t *testing.T) {
	t.Parallel()
	ge := startGate(t)
	time.AfterFunc(8*time.Second, ge.g.open)
	start := time.Now()
	r := runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "gated", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	op := onlyOp(t, r.stdout)
	if op.Status != apitypes.StatusSucceeded || op.Host != "alpha" {
		t.Errorf("stdout operation %+v, want the final, Succeeded, labeled alpha", op)
	}
	if el := time.Since(start); el < 8*time.Second {
		t.Errorf("returned after %s, before the operation ended", el)
	}

	accepted := ge.px.matching(http.MethodPost, func(p string) bool { return strings.HasSuffix(p, "/start") })
	polls := ge.px.polls(op.ID)
	if len(accepted) != 1 || len(polls) < 5 {
		t.Fatalf("%d starts and %d polls recorded", len(accepted), len(polls))
	}
	var gaps []time.Duration
	prev := accepted[0].at
	for _, p := range polls {
		gaps = append(gaps, p.at.Sub(prev))
		prev = p.at
	}
	t.Logf("poll gaps: %v", gaps)
	if gaps[0] < 200*time.Millisecond || gaps[0] > 700*time.Millisecond {
		t.Errorf("first poll gap %s, want near 250ms", gaps[0])
	}
	for i, g := range gaps {
		if g > 2500*time.Millisecond {
			t.Errorf("poll gap %d is %s, want at most 2s", i, g)
		}
	}
	if top := slices.Max(gaps); top < 1800*time.Millisecond {
		t.Errorf("largest poll gap %s, want the backoff to reach 2s", top)
	}

	progress := jsonLines(t, "stderr", r.stderr)
	if len(progress) == 0 {
		t.Fatal("no progress on stderr")
	}
	for _, p := range progress {
		if p["level"] != "progress" || p["id"] != op.ID || p["host"] != "alpha" || p["status"] != "Running" {
			t.Errorf("stderr line %v, want progress of %s on alpha", p, op.ID)
		}
	}
	if len(ge.px.cancels()) != 0 {
		t.Error("a cancel was sent")
	}
}

// TC-172: --follow streams the counter with no gap and no repeat through a
// stream that drops midway and one that falls silent, reopening from the
// last offset; the watchdog defaults to 60 s; the stream client has no
// overall timeout but bounded handshake and headers.
func TestFollowReconnectsFromLastOffset(t *testing.T) {
	if followWatchdog != 60*time.Second {
		t.Errorf("watchdog default %s, want 60s", followWatchdog)
	}
	const short = 700 * time.Millisecond
	old := watchdog
	watchdog = short
	t.Cleanup(func() { watchdog = old })

	want := ""
	for i := 1; i <= 40; i++ {
		want += strconv.Itoa(i) + "\n"
	}
	for _, fault := range []int32{streamDrop, streamStall} {
		name := map[int32]string{streamDrop: "drop", streamStall: "stall"}[fault]
		t.Run(name, func(t *testing.T) {
			ge := startGate(t)
			ge.px.fault.Store(fault)
			r := runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "counter", "--follow", "-o", "table")
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			if r.stderr != want {
				t.Errorf("followed output, want 1 to 40 once each:\n%q", r.stderr)
			}
			if !strings.Contains(r.stdout, "Succeeded") {
				t.Errorf("stdout %q does not hold the final operation", r.stdout)
			}
			streams := ge.px.matching(http.MethodGet, func(p string) bool { return strings.HasSuffix(p, "/logs") })
			if len(streams) < 2 {
				t.Fatalf("%d log streams, want a reconnect", len(streams))
			}
			ge.px.mu.Lock()
			last := ge.px.lastSent
			ge.px.mu.Unlock()
			q0, _ := url.ParseQuery(streams[0].query)
			q1, _ := url.ParseQuery(streams[1].query)
			t.Logf("streams: %q at %s, then %q after %s", streams[0].query, streams[0].at.Format(time.StampMilli),
				streams[1].query, streams[1].at.Sub(streams[0].at))
			if q0.Get("follow") != "true" || q0.Get("offset") != "" {
				t.Errorf("first stream %q, want follow from the start", streams[0].query)
			}
			if q1.Get("follow") != "true" || q1.Get("offset") != strconv.FormatInt(last, 10) {
				t.Errorf("second stream %q, want offset %d, the last frame forwarded", streams[1].query, last)
			}
			if gap := streams[1].at.Sub(streams[0].at); fault == streamStall && gap < short {
				t.Errorf("reconnect %s after a stall, before the %s watchdog", gap, short)
			}
		})
	}

	// The clients the CLI builds: unary 30 s, stream without an overall
	// timeout, with bounded TLS handshake and response headers.
	a := &app{env: Env{Getenv: func(string) string { return "" }}}
	c, err := a.newClient("https://127.0.0.1:1", "", "t")
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPTimeout() != 30*time.Second {
		t.Errorf("unary Timeout %s, want 30s", c.HTTPTimeout())
	}
	if s := c.StreamSettings(); s.Timeout != 0 || s.TLSHandshakeTimeout != 10*time.Second || s.ResponseHeaderTimeout != 30*time.Second {
		t.Errorf("stream client %+v, want Timeout 0, TLSHandshakeTimeout 10s, ResponseHeaderTimeout 30s", s)
	}
}

// A followed stream op (operationlog get --follow) goes through the same
// stream client and reconnects from the last offset when it falls silent.
func TestFollowStreamOpReconnects(t *testing.T) {
	old := watchdog
	watchdog = 700 * time.Millisecond
	t.Cleanup(func() { watchdog = old })
	ge := startGate(t)
	r := runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "counter", "--no-wait", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("start: exit %d: %s", r.code, r.stderr)
	}
	id := onlyOp(t, r.stdout).ID
	ge.px.fault.Store(streamStall)
	r = runSpecsCLI(ge.env, fleetSpecs(), "operationlog", "get", id, "--follow", "-o", "table")
	if want := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"; r.code != 0 || !strings.HasPrefix(r.stdout, want) {
		t.Fatalf("exit %d, stdout %q: %s", r.code, r.stdout, r.stderr)
	}
	var got []string
	for line := range strings.Lines(r.stdout) {
		got = append(got, strings.TrimSpace(line))
	}
	for i, l := range got {
		if l != strconv.Itoa(i+1) {
			t.Fatalf("line %d is %q: gap or repeat in %q", i+1, l, r.stdout)
		}
	}
	if len(got) != 40 {
		t.Errorf("%d lines, want 40", len(got))
	}
}

// TC-173: --no-wait prints the operation and exits 0 at once; --timeout
// exits 7. Neither cancels the operation.
func TestNoWaitAndTimeout(t *testing.T) {
	t.Parallel()
	ge := startGate(t)

	start := time.Now()
	r := runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "gated", "--no-wait", "-o", "jsonl")
	if r.code != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("--no-wait: exit %d after %s: %s", r.code, time.Since(start), r.stderr)
	}
	first := onlyOp(t, r.stdout)
	if first.Status.Terminal() {
		t.Errorf("--no-wait printed %s, want the operation as accepted", first.Status)
	}
	if len(ge.px.polls(first.ID)) != 0 {
		t.Error("--no-wait polled")
	}

	start = time.Now()
	r = runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "gated", "--timeout", "1s", "-o", "jsonl")
	el := time.Since(start)
	if r.code != ExitWaitTimeout {
		t.Fatalf("--timeout 1s: exit %d, want 7: %s", r.code, r.stderr)
	}
	if el < time.Second || el > 4*time.Second {
		t.Errorf("--timeout 1s returned after %s", el)
	}
	second := onlyOp(t, r.stdout)
	errs := jsonLines(t, "stderr", r.stderr)
	if e := errs[len(errs)-1]; e["level"] != "error" || e["exit"] != float64(ExitWaitTimeout) || e["operationId"] != second.ID ||
		e["type"] != "urn:nexus:cli:wait-timeout" {
		t.Errorf("timeout error %v, want exit 7 naming %s", e, second.ID)
	}
	for _, id := range []string{first.ID, second.ID} {
		if op := ge.opStatus(t, id); op.Status != apitypes.StatusRunning {
			t.Errorf("operation %s is %s after the CLI stopped waiting, want Running", id, op.Status)
		}
	}
	ge.g.open()
	ge.waitStatus(t, first.ID, apitypes.StatusSucceeded)
	ge.waitStatus(t, second.ID, apitypes.StatusSucceeded)
	if c := ge.px.cancels(); len(c) != 0 {
		t.Errorf("cancel requests %v, want none", c)
	}
	if r := runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "quick", "--no-wait", "--timeout", "1s"); r.code != ExitUsage {
		t.Errorf("--no-wait with --timeout: exit %d, want 2", r.code)
	}
}

// startSignaled runs the CLI as a real process waiting for a gated op and
// sends it SIGINT once it polls.
func startSignaled(t *testing.T, ge *gateEnv, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := cliProcess(t, ge.home, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	isPoll := func(p string) bool { return strings.HasPrefix(p, "/v1/operations/") }
	before := len(ge.px.matching(http.MethodGet, isPoll))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(ge.px.matching(http.MethodGet, isPoll)) == before {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("the CLI never polled: %s", errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exit interface{ ExitCode() int }
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errb.String()
}

// TC-174: SIGINT while waiting detaches: exit 130, stderr names the
// operation, nothing is canceled and the operation later completes.
func TestInterruptDetaches(t *testing.T) {
	t.Parallel()
	ge := startGate(t)
	code, stdout, stderr := startSignaled(t, ge, "gadget", "start", "gated", "-o", "jsonl")
	t.Logf("exit %d\nstdout: %sstderr: %s", code, stdout, stderr)
	if code != ExitInterrupted {
		t.Fatalf("exit %d, want 130", code)
	}
	op := onlyOp(t, stdout)
	if !strings.Contains(stderr, op.ID) || !strings.Contains(stderr, "not canceled") {
		t.Errorf("stderr %q does not name operation %s as detached", stderr, op.ID)
	}
	if st := ge.opStatus(t, op.ID).Status; st != apitypes.StatusRunning {
		t.Errorf("operation %s after SIGINT, want Running", st)
	}
	ge.g.open()
	ge.waitStatus(t, op.ID, apitypes.StatusSucceeded)
	if c := ge.px.cancels(); len(c) != 0 {
		t.Errorf("cancel requests %v, want none", c)
	}
}

// TC-ERR66: a failed operation exits 1 with its structured error on
// stderr, beside the final operation on stdout.
func TestFailedOperationExits1(t *testing.T) {
	ge := startGate(t)
	r := runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "fail", "-o", "jsonl")
	if r.code != ExitFailure {
		t.Fatalf("exit %d, want 1: %s", r.code, r.stderr)
	}
	op := onlyOp(t, r.stdout)
	if op.Status != apitypes.StatusFailed {
		t.Errorf("stdout operation %s, want Failed", op.Status)
	}
	errs := jsonLines(t, "stderr", r.stderr)
	if len(errs) != 1 {
		t.Fatalf("stderr %q, want one error", r.stderr)
	}
	e := errs[0]
	if e["type"] != "urn:nexus:problem:script-failed" || e["detail"] != "the gadget jammed" || e["exit"] != float64(ExitFailure) ||
		e["operationId"] != op.ID || e["host"] != "alpha" {
		t.Errorf("stderr error %v, want the operation's problem, exit 1, naming %s", e, op.ID)
	}
	r = runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "fail", "-o", "table")
	if r.code != ExitFailure || !strings.Contains(r.stderr, "operation "+onlyOpID(t, ge, 2)) || !strings.Contains(r.stderr, "the gadget jammed") {
		t.Errorf("text: exit %d, stderr %q", r.code, r.stderr)
	}
}

// onlyOpID is the ID of the n-th operation started on ge, from the proxy's
// record of its first poll.
func onlyOpID(t *testing.T, ge *gateEnv, n int) string {
	t.Helper()
	seen := []string{}
	for _, r := range ge.px.matching(http.MethodGet, func(p string) bool { return strings.HasPrefix(p, "/v1/operations/") }) {
		id := strings.TrimPrefix(r.path, "/v1/operations/")
		if !slices.Contains(seen, id) {
			seen = append(seen, id)
		}
	}
	if len(seen) < n {
		t.Fatalf("%d operations polled, want %d", len(seen), n)
	}
	return seen[n-1]
}

// A token that may start an op but not read it: the wait's 403 exits 3,
// naming the operation and the read scope, and the operation runs on.
func TestForbiddenWhileWaiting(t *testing.T) {
	ge := startGate(t)
	tok, _, err := auth.Create(ge.srv.stateDir, auth.CreateRequest{Name: "runner", Scopes: []string{gateWriteScope}})
	if err != nil {
		t.Fatal(err)
	}
	env := homeEnv(writeClientConfig(t, "alpha", clientHost{"alpha", ge.px.URL, tok, nil}))
	env.OpenTerminal = noTerminal
	for i, format := range []string{"jsonl", "table"} {
		r := runSpecsCLI(env, fleetSpecs(), "gadget", "start", "quick", "-o", format)
		if r.code != ExitAuth {
			t.Fatalf("%s: exit %d, want 3: %s", format, r.code, r.stderr)
		}
		id := onlyOpID(t, ge, i+1)
		if !strings.Contains(r.stderr, id) || !strings.Contains(r.stderr, gateReadScope) {
			t.Errorf("%s: stderr %q does not name operation %s and scope %s", format, r.stderr, id, gateReadScope)
		}
		ge.waitStatus(t, id, apitypes.StatusSucceeded)
	}
}

// TC-177: the FR-34 exit code table end to end, against real servers.
func TestExitCodeTable(t *testing.T) {
	ge := startGate(t)
	disabled := startProviderServer(t, "off-host", newGate(), false, gateReadScope, gateWriteScope)
	reader := startProviderServer(t, "ro-host", newGate(), true, gateReadScope)
	tlsSrv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(tlsSrv.Close)
	home := writeClientConfig(t, "alpha",
		clientHost{"alpha", ge.px.URL, ge.srv.token, nil},
		clientHost{"badtoken", ge.px.URL, "nx_not-a-token", nil},
		clientHost{"reader", reader.url, reader.token, nil},
		clientHost{"off", disabled.url, disabled.token, nil},
		clientHost{"down", closedURL(t), "tok", nil})
	// A pin that is not the server's certificate.
	cfgPath := home + "/.config/nexus/config.yaml"
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg = append(cfg, fmt.Sprintf("  pinned:\n    url: %s\n    token: tok\n    fingerprint: %s\n", tlsSrv.URL, strings.Repeat("ab", 32))...)
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	env := homeEnv(home)
	env.OpenTerminal = noTerminal

	for _, c := range []struct {
		what string
		args []string
		want int
	}{
		{"success", []string{"gadget", "start", "quick"}, ExitOK},
		{"operation failure", []string{"gadget", "start", "fail"}, ExitFailure},
		{"usage error", []string{"gadget", "start"}, ExitUsage},
		{"bad token", []string{"gadget", "start", "quick", "--host", "badtoken"}, ExitAuth},
		{"missing scope", []string{"gadget", "start", "quick", "--host", "reader"}, ExitAuth},
		{"unknown resource", []string{"gadget", "get", "missing"}, ExitNotFound},
		{"disabled provider", []string{"gadget", "get", "x", "--host", "off"}, ExitNotFound},
		{"connection refused", []string{"gadget", "get", "x", "--host", "down"}, ExitConnection},
		{"pin mismatch", []string{"gadget", "get", "x", "--host", "pinned"}, ExitConnection},
		{"partial failure", []string{"gadget", "start", "quick", "--host", "alpha,down", "--yes"}, ExitPartial},
		{"wait timeout", []string{"gadget", "start", "gated", "--timeout", "300ms"}, ExitWaitTimeout},
	} {
		r := runSpecsCLI(env, fleetSpecs(), append(c.args, "-o", "jsonl")...)
		t.Logf("%-18s nexus %s -> exit %d", c.what, strings.Join(c.args, " "), r.code)
		if r.code != c.want {
			t.Errorf("%s: exit %d, want %d: %s", c.what, r.code, c.want, r.stderr)
		}
	}
	code, _, stderr := startSignaled(t, ge, "gadget", "start", "gated")
	t.Logf("%-18s nexus gadget start gated + SIGINT -> exit %d", "SIGINT", code)
	if code != ExitInterrupted {
		t.Errorf("SIGINT: exit %d, want 130: %s", code, stderr)
	}
}
