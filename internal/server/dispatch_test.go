// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
)

type textIn struct {
	// Free text: no pattern or enum (the marker for free-text fields is
	// added with the conformance suite, 4.2).
	Text string `json:"text,omitempty"`
}

type widgetIn struct {
	Name string `json:"name"`
}

var (
	widgetEcho = def.Define[textIn, gadgetOut](def.Op{
		ID: "widgets.actions.echo", Provider: "widgets", Kind: def.KindProviderAction, Action: "echo",
		Summary: "s", Description: "Echo operation for the dispatcher REST tests.",
		Scope: def.Scope{Name: "widgets:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "widgetecho", Verb: "test"},
	})
	widgetGet = def.Define[widgetIn, gadgetOut](def.Op{
		ID: "widgets.widgets.get", Provider: "widgets", Kind: def.KindGet, ResourceType: "widgets",
		Summary: "s", Description: "Get operation for the dispatcher REST tests.",
		Scope: def.Scope{Name: "test:write"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "widget", Verb: "get", Positional: "name"},
	})
	widgetLogs = def.Define[widgetIn, apitypes.Frame](def.Op{
		ID: "widgets.widgets.logs", Provider: "widgets", Kind: def.KindSubresource, ResourceType: "widgets",
		Action: "logs", Summary: "s", Description: "Stream operation for the dispatcher REST tests.",
		Scope: def.Scope{Name: "widgets:read"}, Risk: def.RiskReadOnly, Mode: def.ModeStream,
		CLI: def.CLIBinding{Noun: "widgetlog", Verb: "get", Positional: "name"},
	})
)

type widgets struct{ handled atomic.Int64 }

func (*widgets) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "widgets", ReadScope: "widgets:read",
		Ops: []def.Op{widgetEcho.Op(), widgetGet.Op(), widgetLogs.Op()}}
}

func (*widgets) Configure(func(any) error) error { return nil }

func (*widgets) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (w *widgets) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(widgetEcho, func(_ context.Context, in textIn) (gadgetOut, error) {
			w.handled.Add(1)
			return gadgetOut{Name: in.Text[:min(len(in.Text), 8)]}, nil
		}),
		provider.Sync(widgetGet, func(_ context.Context, in widgetIn) (gadgetOut, error) {
			w.handled.Add(1)
			return gadgetOut(in), nil
		}),
		provider.Stream(widgetLogs, func(_ context.Context, in widgetIn, emit func(apitypes.Frame) error) error {
			w.handled.Add(1)
			return emit(apitypes.Frame{S: apitypes.StreamStdout, D: in.Name})
		}),
	}
}

type widgetServer struct {
	base string
	srv  *server.Server
	reg  *core.Registry
	w    *widgets
	dir  string // Token store
}

// startWidgets runs the real server, with the default body limit, over the
// widgets and gadgets providers and a token store of its own.
func startWidgets(t *testing.T) *widgetServer {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	w := &widgets{}
	reg, err := core.New([]provider.Provider{w, gadgets{}}, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: log,
		Providers: map[string]core.ProviderConfig{"widgets": {Enabled: true}, "gadgets": {Enabled: true}},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	dir := t.TempDir()
	srv, err := server.New(reg, server.Options{Logger: log, Auth: auth.New(dir, auth.Options{Logger: log})})
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
	return &widgetServer{base: "http://" + ln.Addr().String(), srv: srv, reg: reg, w: w, dir: dir}
}

func request(t *testing.T, method, url, token string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return send(t, req)
}

// TC-32: discovery needs only a valid token.
func TestDiscoveryNeedsOnlyAValidToken(t *testing.T) {
	ws := startWidgets(t)
	bare := mint(t, ws.dir, "bare")
	resp, body := request(t, http.MethodGet, ws.base+"/v1/capabilities", bare, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	var caps apitypes.Capabilities
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	resp, body = doAuth(t, http.MethodGet, ws.base+"/v1/capabilities", "")
	if p := decodeProblem(t, resp, body); resp.StatusCode != http.StatusUnauthorized || p.Slug() != problem.Unauthorized {
		t.Fatalf("no token: %d %+v", resp.StatusCode, p)
	}
}

// TC-30 over REST: 403 problem+json naming the scope; the handler never runs.
func TestMissingScopeOverREST(t *testing.T) {
	ws := startWidgets(t)
	bare := mint(t, ws.dir, "bare")
	scoped := mint(t, ws.dir, "scoped", "test:write")
	url := ws.base + "/v1/providers/widgets/widgets/w1"

	resp, body := request(t, http.MethodGet, url, bare, nil)
	p := decodeProblem(t, resp, body)
	if resp.StatusCode != http.StatusForbidden || p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != "test:write" {
		t.Fatalf("unscoped: %d %s", resp.StatusCode, body)
	}
	if n := ws.w.handled.Load(); n != 0 {
		t.Fatalf("handler ran %d times", n)
	}
	resp, body = request(t, http.MethodGet, url, scoped, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"w1"`) {
		t.Fatalf("scoped: %d %s", resp.StatusCode, body)
	}
}

// The pipeline order holds over REST: an input REST cannot even assemble is
// still reported after the scope check.
func TestRESTScopeCheckPrecedesInputErrors(t *testing.T) {
	ws := startWidgets(t)
	bare := mint(t, ws.dir, "bare")
	scoped := mint(t, ws.dir, "scoped", "widgets:read", "test:write")
	echo := ws.base + "/v1/providers/widgets/actions/echo"
	get := ws.base + "/v1/providers/widgets/widgets/w1"
	cases := []struct {
		name, method, url, body string
	}{
		{"malformed body", http.MethodPost, echo, `{"text":`},
		{"body not an object", http.MethodPost, echo, `["x"]`},
		{"query on POST", http.MethodPost, echo + "?text=x", `{}`},
		{"wrong type", http.MethodPost, echo, `{"text":5}`},
		{"additional property", http.MethodPost, echo, `{"text":"x","bogus":1}`},
		{"unknown query parameter", http.MethodGet, get + "?bogus=1", ""},
		{"path field in query", http.MethodGet, get + "?name=w2", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := request(t, c.method, c.url, bare, strings.NewReader(c.body))
			if p := decodeProblem(t, resp, body); resp.StatusCode != http.StatusForbidden || p.Slug() != problem.Forbidden {
				t.Fatalf("unscoped: %d %s", resp.StatusCode, body)
			}
			resp, body = request(t, c.method, c.url, scoped, strings.NewReader(c.body))
			p := decodeProblem(t, resp, body)
			if resp.StatusCode != http.StatusBadRequest || p.Slug() != problem.ValidationFailed {
				t.Fatalf("scoped: %d %s", resp.StatusCode, body)
			}
			if fe, ok := p.Extensions["errors"].([]any); !ok || len(fe) == 0 {
				t.Fatalf("errors[] missing or empty: %s", body)
			}
		})
	}
	if n := ws.w.handled.Load(); n != 0 {
		t.Fatalf("handler ran %d times", n)
	}
}

// TC-E5: a body of exactly the default limit reaches validation (and here,
// being valid, the handler); one byte more is 413 problem+json and the
// handler is not invoked.
func TestBodyLimitBoundary(t *testing.T) {
	if limit := config.Default().Limits.MaxBodyBytes; limit != 1048576 {
		t.Fatalf("config default limit = %d, want 1048576", limit)
	}
	if server.DefaultMaxBodyBytes != 1048576 {
		t.Fatalf("server default limit = %d, want 1048576", server.DefaultMaxBodyBytes)
	}
	ws := startWidgets(t)
	tok := mint(t, ws.dir, "body", "widgets:read")
	url := ws.base + "/v1/providers/widgets/actions/echo"
	bodyOf := func(n int) string {
		const head, tail = `{"text":"`, `"}`
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}

	exact := bodyOf(1048576)
	if len(exact) != 1048576 {
		t.Fatalf("test body is %d bytes", len(exact))
	}
	resp, body := request(t, http.MethodPost, url, tok, strings.NewReader(exact))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exactly the limit: %d %.200s", resp.StatusCode, body)
	}
	if n := ws.w.handled.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
	// Exactly the limit with no Content-Length (chunked) is also accepted.
	resp, body = request(t, http.MethodPost, url, tok, io.MultiReader(strings.NewReader(exact)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exactly the limit, chunked: %d %.200s", resp.StatusCode, body)
	}

	over := bodyOf(1048577)
	for name, r := range map[string]io.Reader{
		"declared": strings.NewReader(over),
		"chunked":  io.MultiReader(strings.NewReader(over)),
	} {
		resp, body = request(t, http.MethodPost, url, tok, r)
		p := decodeProblem(t, resp, body)
		if resp.StatusCode != http.StatusRequestEntityTooLarge || p.Slug() != problem.PayloadTooLarge {
			t.Fatalf("%s, one byte over: %d %s", name, resp.StatusCode, body)
		}
	}
	if n := ws.w.handled.Load(); n != 2 {
		t.Fatalf("handler ran %d times, want 2", n)
	}
}

// Every route is served through the Dispatcher: a token without scopes is
// refused on every route except the token-only system ops (A-50). A route
// that called a handler directly would answer it.
func TestEveryRouteGoesThroughTheDispatcher(t *testing.T) {
	ws := startWidgets(t)
	bare := mint(t, ws.dir, "bare")
	ops := map[string]def.Op{}
	for _, e := range ws.reg.Entries() {
		ops[e.Binding.Op().ID] = e.Binding.Op()
	}
	routes := ws.srv.Routes()
	if len(routes) < 5 {
		t.Fatalf("only %d routes; the check would be vacuous", len(routes))
	}
	for _, rt := range routes {
		op := ops[rt.OpID]
		tokenOnly := op.Provider == def.SystemProvider && op.Scope.Name == "" && op.Scope.PerTarget == "" && !op.Scope.FromTarget
		path := strings.NewReplacer("{name}", "x", "{id}", "01890a5d-ac96-774b-bcce-b302099a8057").Replace(rt.Path)
		var body io.Reader
		if rt.Method == http.MethodPost {
			body = strings.NewReader(`{}`)
		}
		resp, b := request(t, rt.Method, ws.base+path, bare, body)
		switch {
		case tokenOnly && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized):
			t.Errorf("%s %s (token-only): %d %s", rt.Method, rt.Path, resp.StatusCode, b)
		case !tokenOnly && resp.StatusCode != http.StatusForbidden:
			t.Errorf("%s %s without scopes: %d, want 403; body %.200s", rt.Method, rt.Path, resp.StatusCode, b)
		}
	}
	if n := ws.w.handled.Load(); n != 0 {
		t.Fatalf("a handler ran %d times for a token without scopes", n)
	}
}

// The REST binding has no authorization and no way to run a handler: only
// the Dispatcher calls bindings and checks scopes or availability.
func TestRESTHasNoAuthorizationOrDirectCalls(t *testing.T) {
	forbidden := map[string]string{
		"CallSync":   "calls a sync handler directly",
		"CallStream": "calls a stream handler directly",
		"InProcess":  "reaches a long-running binding directly",
		"Available":  "checks availability itself",
		"Satisfies":  "checks scopes itself",
		"HasScope":   "checks scopes itself",
		"Has":        "checks scopes itself",
		"Scopes":     "reads the caller's scopes",
		"Scope":      "reads an op's scope",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if why, bad := forbidden[sel.Sel.Name]; bad {
				t.Errorf("%s uses .%s: it %s", name, sel.Sel.Name, why)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no server source files found")
	}
}
