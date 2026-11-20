// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
)

// The panic sentinel must never reach a response body (TC-ERR6).
const panicSentinel = "SENTINEL-7f3a-panic-value"

type gadgetIn struct {
	Name string `json:"name"`
}

type gadgetOut struct {
	Name string `json:"name"`
}

type pingIn struct {
	Note string `json:"note,omitempty"`
}

func gadgetOp(id string, kind def.Kind, action string) def.Op {
	return def.Op{
		ID: id, Provider: "gadgets", Kind: kind, ResourceType: "gadgets", Action: action,
		Summary: "s", Description: "Gadget operation for the server tests.",
		Scope: def.Scope{Name: "gadgets:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "gadget", Verb: "get", Positional: "name"},
	}
}

var (
	gadgetGet   = def.Define[gadgetIn, gadgetOut](gadgetOp("gadgets.gadgets.get", def.KindGet, ""))
	gadgetPanic = def.Define[gadgetIn, gadgetOut](gadgetOp("gadgets.gadgets.explode", def.KindSubresource, "explode"))
	gadgetPing  = def.Define[pingIn, gadgetOut](func() def.Op {
		op := gadgetOp("gadgets.actions.ping", def.KindProviderAction, "ping")
		op.ResourceType = ""
		op.CLI = def.CLIBinding{Noun: "gadgetping", Verb: "test"}
		return op
	}())
)

type gadgets struct{}

func (gadgets) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "gadgets", ReadScope: "gadgets:read",
		Ops: []def.Op{gadgetGet.Op(), gadgetPanic.Op(), gadgetPing.Op()}}
}

func (gadgets) Configure(func(any) error) error { return nil }

func (gadgets) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (gadgets) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(gadgetGet, func(_ context.Context, in gadgetIn) (gadgetOut, error) {
			return gadgetOut{Name: in.Name}, nil
		}),
		provider.Sync(gadgetPanic, func(context.Context, gadgetIn) (gadgetOut, error) {
			panic(panicSentinel)
		}),
		provider.Sync(gadgetPing, func(_ context.Context, in pingIn) (gadgetOut, error) {
			return gadgetOut{Name: in.Note}, nil
		}),
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testServer struct {
	srv  *server.Server
	base string
	addr string
	logs *syncBuffer
}

// startServer runs the real server over gadgets on a real loopback listener.
func startServer(t *testing.T, opts server.Options) *testServer {
	t.Helper()
	return startServerWith(t, opts, core.Options{
		Providers: map[string]core.ProviderConfig{"gadgets": {Enabled: true}},
	}, gadgets{})
}

// startServerWith runs the real server over ps on a real loopback
// listener. copts names the enabled providers and may carry an audit log
// and an operation manager; its host, version and logger are set here.
func startServerWith(t *testing.T, opts server.Options, copts core.Options, ps ...provider.Provider) *testServer {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	copts.Host, copts.Version, copts.Logger = "testhost", "v0.0.0-test", log
	reg, err := core.New(ps, copts)
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	opts.Logger = log
	if opts.Auth == nil {
		opts.Auth = freshAuth()
	}
	srv, err := server.New(reg, opts)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	addr := ln.Addr().String()
	return &testServer{srv: srv, base: "http://" + addr, addr: addr, logs: logs}
}

var client = &http.Client{Timeout: 10 * time.Second}

// do sends a request authenticated with the shared test token.
func do(t *testing.T, method, url string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sharedToken)
	return send(t, req)
}

// send sends req as is and returns the response with its whole body.
func send(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, b
}

func decodeProblem(t *testing.T, resp *http.Response, body []byte) *problem.Error {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != problem.ContentType {
		t.Fatalf("Content-Type = %q, want %q; body %q", ct, problem.ContentType, body)
	}
	var p problem.Error
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("body is not a problem: %v: %q", err, body)
	}
	return &p
}

// TC-17: /healthz reveals liveness only.
func TestHealthzRevealsLivenessOnly(t *testing.T) {
	ts := startServer(t, server.Options{})
	resp, body := do(t, http.MethodGet, ts.base+"/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("body = %q, want exactly {\"status\":\"ok\"}", body)
	}
	standard := map[string]bool{"Content-Type": true, "Content-Length": true, "Date": true}
	for k, vs := range resp.Header {
		if !standard[k] {
			t.Errorf("non-standard header %s: %v", k, vs)
		}
		for _, v := range vs {
			for _, leak := range []string{"testhost", "v0.0.0-test", "gadgets"} {
				if strings.Contains(v, leak) {
					t.Errorf("header %s leaks %q", k, leak)
				}
			}
		}
	}
}

// TC-19: server timeouts and header limits are set.
func TestServerTimeoutsAndHeaderLimits(t *testing.T) {
	ts := startServer(t, server.Options{})
	hs := ts.srv.HTTP()
	checks := []struct {
		name      string
		got, want time.Duration
	}{
		{"ReadHeaderTimeout", hs.ReadHeaderTimeout, 5 * time.Second},
		{"ReadTimeout", hs.ReadTimeout, 30 * time.Second},
		{"WriteTimeout", hs.WriteTimeout, 60 * time.Second},
		{"IdleTimeout", hs.IdleTimeout, 120 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if hs.MaxHeaderBytes != 64<<10 {
		t.Errorf("MaxHeaderBytes = %d, want %d", hs.MaxHeaderBytes, 64<<10)
	}
	if hs.BaseContext == nil {
		t.Fatal("BaseContext is nil")
	}

	t.Run("partial request is closed", func(t *testing.T) {
		conn, err := net.Dial("tcp", ts.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		start := time.Now()
		if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\n"); err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, conn)
		elapsed := time.Since(start)
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("connection still open after %v", elapsed)
		}
		if elapsed > 6*time.Second {
			t.Fatalf("connection closed after %v, want within 6s", elapsed)
		}
	})

	t.Run("oversize header is rejected", func(t *testing.T) {
		conn, err := net.Dial("tcp", ts.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		big := strings.Repeat("a", 65<<10+4096+1024)
		req := "GET /healthz HTTP/1.1\r\nHost: x\r\nX-Big: " + big + "\r\n\r\n"
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		// The server may stop reading and close before the whole header is
		// sent; only the response matters.
		go func() { _, _ = io.WriteString(conn, req) }()
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatalf("read status line: %v", err)
		}
		// Go's built-in 431 is a recorded FR-8 exemption.
		if !strings.Contains(line, "431") {
			t.Fatalf("status line = %q, want 431", line)
		}
		resp, body := do(t, http.MethodGet, ts.base+"/healthz", nil)
		if resp.StatusCode != http.StatusOK || string(body) != `{"status":"ok"}` {
			t.Fatalf("server stopped serving: %d %q", resp.StatusCode, body)
		}
	})

	t.Run("BaseContext ends on shutdown", func(t *testing.T) {
		ts2 := startServer(t, server.Options{})
		ctx := ts2.srv.HTTP().BaseContext(nil)
		if ctx.Err() != nil {
			t.Fatal("base context already done before shutdown")
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ts2.srv.Shutdown(sctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("base context not canceled by shutdown")
		}
	})
}

// TC-ERR6: a handler panic fails one request only.
func TestHandlerPanicFailsOneRequest(t *testing.T) {
	ts := startServer(t, server.Options{})
	resp, body := do(t, http.MethodGet, ts.base+"/v1/providers/gadgets/gadgets/g1/explode", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %q", resp.StatusCode, body)
	}
	p := decodeProblem(t, resp, body)
	if p.Slug() != problem.InternalError {
		t.Fatalf("slug = %q, want internal-error", p.Slug())
	}
	for _, leak := range []string{panicSentinel, "goroutine", ".go:"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("response body leaks %q: %s", leak, body)
		}
	}
	logs := ts.logs.String()
	if !strings.Contains(logs, panicSentinel) {
		t.Errorf("log lacks the panic value: %s", logs)
	}
	if !strings.Contains(logs, "goroutine") || !strings.Contains(logs, "server_test.go") {
		t.Errorf("log lacks a stack: %s", logs)
	}

	resp, body = do(t, http.MethodGet, ts.base+"/v1/providers/gadgets/gadgets/g2", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"g2"`) {
		t.Fatalf("next request: %d %q", resp.StatusCode, body)
	}
}

// TC-ERR7: unmatched requests use problem+json.
func TestUnmatchedRequestsUseProblemJSON(t *testing.T) {
	ts := startServer(t, server.Options{})
	cases := []struct {
		method, path string
		status       int
		slug         problem.Slug
	}{
		{http.MethodGet, "/v1/nope", http.StatusNotFound, problem.RouteNotFound},
		{http.MethodDelete, "/v1/capabilities", http.StatusMethodNotAllowed, problem.MethodNotAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed, problem.MethodNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			resp, body := do(t, c.method, ts.base+c.path, nil)
			if resp.StatusCode != c.status {
				t.Fatalf("status = %d, want %d; body %q", resp.StatusCode, c.status, body)
			}
			for _, plain := range []string{"404 page not found", "Method Not Allowed\n"} {
				if strings.Contains(string(body), plain) {
					t.Fatalf("Go's default plain-text body: %q", body)
				}
			}
			p := decodeProblem(t, resp, body)
			if p.Slug() != c.slug || p.Status != c.status {
				t.Fatalf("problem = %+v, want %s %d", p, c.slug, c.status)
			}
			if c.status == http.StatusMethodNotAllowed && resp.Header.Get("Allow") == "" {
				t.Error("405 without an Allow header")
			}
		})
	}
}

// An oversize body is payload-too-large as problem+json, whether the
// Content-Length announces it or the body only turns out to be too long.
func TestOversizeBodyIsPayloadTooLarge(t *testing.T) {
	ts := startServer(t, server.Options{MaxBodyBytes: 64})
	url := ts.base + "/v1/providers/gadgets/actions/ping"

	resp, body := do(t, http.MethodPost, url, strings.NewReader(`{"note":"ok"}`))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("small body: %d %q", resp.StatusCode, body)
	}

	big := `{"note":"` + strings.Repeat("x", 100) + `"}`
	resp, body = do(t, http.MethodPost, url, strings.NewReader(big))
	if p := decodeProblem(t, resp, body); resp.StatusCode != 413 || p.Slug() != problem.PayloadTooLarge {
		t.Fatalf("declared length: %d %+v", resp.StatusCode, p)
	}

	// No Content-Length: chunked, so only MaxBytesReader can catch it.
	resp, body = do(t, http.MethodPost, url, io.MultiReader(strings.NewReader(big)))
	if p := decodeProblem(t, resp, body); resp.StatusCode != 413 || p.Slug() != problem.PayloadTooLarge {
		t.Fatalf("chunked: %d %+v", resp.StatusCode, p)
	}
}

// Routes come only from the registry, one per binding, and are recorded.
func TestRoutesAreRecordedFromTheRegistry(t *testing.T) {
	ts := startServer(t, server.Options{})
	got := map[string]string{}
	for _, r := range ts.srv.Routes() {
		got[r.Method+" "+r.Path] = r.OpID
	}
	want := map[string]string{
		"GET /v1/audit":                                    "system.audit.list",
		"GET /v1/capabilities":                             "system.capabilities.get",
		"GET /v1/providers/gadgets/gadgets/{name}":         "gadgets.gadgets.get",
		"GET /v1/providers/gadgets/gadgets/{name}/explode": "gadgets.gadgets.explode",
		"POST /v1/providers/gadgets/actions/ping":          "gadgets.actions.ping",
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("route %s = %q, want %q", k, got[k], v)
		}
	}
}

// Unknown query parameters are validation failures, not silently ignored.
func TestUnknownQueryParameterIsValidationFailed(t *testing.T) {
	ts := startServer(t, server.Options{})
	resp, body := do(t, http.MethodGet, ts.base+"/v1/capabilities?bogus=1", nil)
	if p := decodeProblem(t, resp, body); resp.StatusCode != 400 || p.Slug() != problem.ValidationFailed {
		t.Fatalf("got %d %+v", resp.StatusCode, p)
	}
}
