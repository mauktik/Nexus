// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/conformance/testprovider"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/mcpadapter"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
)

// Regenerate testdata/discover.json from a live exchange with:
//
//	go test ./internal/mcpadapter -run TestDiscoverMatchesCapture -update
var update = flag.Bool("update", false, "rewrite testdata/discover.json from a live exchange")

const (
	module     = "github.com/mauktik/nexus"
	sdkModule  = "github.com/modelcontextprotocol/go-sdk"
	sdkPinned  = "v1.8.0"
	modern     = "2026-07-28"
	legacy     = "2025-11-25"
	okOrigin   = "https://ok.example"
	maxBody    = 1 << 20
	sdkDefault = 4 << 20
)

// world is a real nexusd server, Dispatcher, audit log and operation
// manager over the conformance provider, with three tokens: full holds the
// provider's read and write scopes, read only its read scope, none no scope.
type world struct {
	base             string
	srv              *server.Server
	audit            *audit.Log
	spy              *testprovider.Spy
	full, read, none string
}

type worldOpts struct {
	origins      []string
	failureLimit int
	// extra providers are enabled next to the conformance provider, and
	// the full token also holds extraScopes (8.2).
	extra       []provider.Provider
	extraScopes []string
}

func newWorld(t *testing.T, o worldOpts) *world {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	spy := &testprovider.Spy{}
	stateDir, auditDir := t.TempDir(), t.TempDir()
	alog, err := audit.Open(audit.Options{Dir: auditDir, Host: "testhost", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: filepath.Join(stateDir, "ops"), Host: "testhost",
		Logger: log, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	providers := []provider.Provider{testprovider.New(spy)}
	enabled := map[string]core.ProviderConfig{testprovider.Name: {Enabled: true}}
	for _, p := range o.extra {
		providers = append(providers, p)
		enabled[p.Spec().Name] = core.ProviderConfig{Enabled: true}
	}
	reg, err := core.New(providers, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: log, Audit: alog, Ops: mgr, Providers: enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	mint := func(name string, scopes ...string) string {
		tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: name, Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	w := &world{audit: alog, spy: spy,
		full: mint("full", append([]string{testprovider.ReadScope, testprovider.WriteScope}, o.extraScopes...)...),
		read: mint("read", testprovider.ReadScope),
		none: mint("none"),
	}
	limit := o.failureLimit
	if limit == 0 {
		limit = 1000
	}
	w.srv, err = server.New(reg, server.Options{Logger: log, MaxBodyBytes: maxBody, MCPAllowedOrigins: o.origins,
		Auth: auth.New(stateDir, auth.Options{Logger: log, Audit: alog, FailureLimitPerMinute: limit})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("ops shutdown: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
	})
	w.base = "http://" + ln.Addr().String()
	return w
}

// rpc is one raw JSON-RPC exchange with /mcp.
type rpc struct {
	token, origin string
	version       string // MCP-Protocol-Version; empty sends none
	method        string
	id            any
	params        map[string]any
	header        map[string]string
	body          []byte // When set, sent instead of a message built from the fields above
}

// message builds the JSON-RPC request. A modern version carries the
// per-request _meta of the 2026-07-28 revision (basic/index#meta); an older
// one does not.
func (r rpc) message() []byte {
	params := map[string]any{}
	for k, v := range r.params {
		params[k] = v
	}
	if r.version >= modern {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    r.version,
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "nexus-wire-test", "version": "1.0.0"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
	}
	id := r.id
	if id == nil {
		id = 1
	}
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": r.method, "params": params})
	if err != nil {
		panic(err)
	}
	return b
}

// headers are the request headers Streamable HTTP requires (2026-07-28,
// transports/streamable-http: Sending Messages, Standard Request Headers).
func (r rpc) headers() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
	if r.version != "" {
		h.Set("MCP-Protocol-Version", r.version)
	}
	if r.version >= modern {
		h.Set("Mcp-Method", r.method)
		if name, ok := r.params["name"].(string); ok && r.method == "tools/call" {
			h.Set("Mcp-Name", name)
		}
	}
	if r.token != "" {
		h.Set("Authorization", "Bearer "+r.token)
	}
	if r.origin != "" {
		h.Set("Origin", r.origin)
	}
	for k, v := range r.header {
		h.Set(k, v)
	}
	return h
}

type exchange struct {
	status int
	header http.Header
	body   []byte
}

func (w *world) send(t *testing.T, r rpc) exchange {
	t.Helper()
	body := r.body
	if body == nil {
		body = r.message()
	}
	return post(t, w.base+"/mcp", r.headers(), bytes.NewReader(body))
}

func post(t *testing.T, url string, h http.Header, body io.Reader) exchange {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = h
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return exchange{status: resp.StatusCode, header: resp.Header, body: b}
}

// toolResult is a CallToolResult as it is on the wire.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// result decodes a JSON-RPC success response, failing on a protocol error.
func (e exchange) result(t *testing.T, into any) {
	t.Helper()
	if e.status != http.StatusOK {
		t.Fatalf("status %d: %s", e.status, e.body)
	}
	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(e.body, &msg); err != nil {
		t.Fatalf("not a JSON-RPC response: %v\n%s", err, e.body)
	}
	if msg.Error != nil || msg.Result == nil {
		t.Fatalf("protocol error, want a result: %s", e.body)
	}
	if err := json.Unmarshal(msg.Result, into); err != nil {
		t.Fatalf("result: %v\n%s", err, msg.Result)
	}
}

func (e exchange) problem(t *testing.T) *problem.Error {
	t.Helper()
	if ct := e.header.Get("Content-Type"); ct != problem.ContentType {
		t.Fatalf("status %d: Content-Type %q, want %s: %s", e.status, ct, problem.ContentType, e.body)
	}
	var p problem.Error
	if err := json.Unmarshal(e.body, &p); err != nil {
		t.Fatalf("status %d: not a problem: %v\n%s", e.status, err, e.body)
	}
	return &p
}

// structuredProblem is the problem a tool error carries in structuredContent.
func structuredProblem(t *testing.T, raw json.RawMessage) *problem.Error {
	t.Helper()
	var p problem.Error
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("structuredContent is not a problem: %v\n%s", err, raw)
	}
	return &p
}

// headerTransport adds fixed headers to every request the SDK client sends.
type headerTransport struct {
	header map[string]string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.header {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// session connects the real SDK client with token and any extra headers.
func (w *world) session(t *testing.T, token string, extra map[string]string) *mcp.ClientSession {
	t.Helper()
	h := map[string]string{"Authorization": "Bearer " + token}
	for k, v := range extra {
		h[k] = v
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "nexus-wire-test", Version: "1.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             w.base + "/mcp",
		HTTPClient:           &http.Client{Timeout: 10 * time.Second, Transport: headerTransport{header: h}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("SDK client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: protocol error %v, want a result", name, err)
	}
	return res
}

func structured(t *testing.T, res *mcp.CallToolResult) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func goCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return string(out)
}

// discoverCapture is testdata/discover.json: the request, built from the
// server/discover example of the 2026-07-28 revision, and the response the
// real server gave, with volatile fields normalized.
type discoverCapture struct {
	Regenerate string `json:"regenerate"`
	Request    struct {
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Status      int             `json:"status"`
		ContentType string          `json:"contentType"`
		Body        json.RawMessage `json:"body"`
	} `json:"response"`
}

// normalizeDiscover replaces the build version in serverInfo, which changes
// with every build, and re-indents.
func normalizeDiscover(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("discover response: %v\n%s", err, body)
	}
	if res, ok := v["result"].(map[string]any); ok {
		if meta, ok := res["_meta"].(map[string]any); ok {
			if info, ok := meta["io.modelcontextprotocol/serverInfo"].(map[string]any); ok {
				info["version"] = "normalized"
			}
		}
	}
	out, err := json.MarshalIndent(v, "    ", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TC-142: real SDK discovery and raw exchange.
func TestDiscoverMatchesCapture(t *testing.T) {
	// Setup: the SDK is the pinned release, v1.7.0 or later.
	if got := strings.TrimSpace(goCmd(t, "list", "-m", "-f", "{{.Version}}", sdkModule)); got != sdkPinned {
		t.Fatalf("%s is %s, want %s pinned (v1.7.0 or later)", sdkModule, got, sdkPinned)
	}
	w := newWorld(t, worldOpts{})

	// Step 1: raw server/discover with a bearer token.
	r := rpc{token: w.read, version: modern, method: "server/discover", id: "discover-1"}
	ex := w.send(t, r)
	if ex.status != http.StatusOK {
		t.Fatalf("server/discover: %d %s", ex.status, ex.body)
	}
	if ct := ex.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q, want application/json (JSONResponse)", ct)
	}
	if sid := ex.header.Get("Mcp-Session-Id"); sid != "" {
		t.Errorf("stateless server set Mcp-Session-Id %q", sid)
	}

	// The fields the revision defines (server/discover, DiscoverResult),
	// checked independently of the capture.
	var res struct {
		ResultType        string                     `json:"resultType"`
		SupportedVersions []string                   `json:"supportedVersions"`
		Capabilities      map[string]json.RawMessage `json:"capabilities"`
		Meta              map[string]struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"_meta"`
	}
	ex.result(t, &res)
	if res.ResultType != "complete" || !slices.Contains(res.SupportedVersions, modern) {
		t.Errorf("resultType %q supportedVersions %v, want complete with %s", res.ResultType, res.SupportedVersions, modern)
	}
	if _, ok := res.Capabilities["tools"]; !ok {
		t.Errorf("capabilities %v lack tools", res.Capabilities)
	}
	if info := res.Meta["io.modelcontextprotocol/serverInfo"]; info.Name != "nexusd" || info.Version != "v0.0.0-test" {
		t.Errorf("serverInfo = %+v, want nexusd v0.0.0-test", info)
	}

	var got discoverCapture
	got.Regenerate = "go test ./internal/mcpadapter -run TestDiscoverMatchesCapture -update"
	got.Request.Headers = map[string]string{}
	for k := range r.headers() {
		if k != "Authorization" {
			got.Request.Headers[k] = r.headers().Get(k)
		}
	}
	var reqBody any
	if err := json.Unmarshal(r.message(), &reqBody); err != nil {
		t.Fatal(err)
	}
	var err error
	if got.Request.Body, err = json.MarshalIndent(reqBody, "    ", "  "); err != nil {
		t.Fatal(err)
	}
	got.Response.Status = ex.status
	got.Response.ContentType = ex.header.Get("Content-Type")
	got.Response.Body = normalizeDiscover(t, ex.body)
	path := filepath.Join("testdata", "discover.json")
	if *update {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture (regenerate with %s): %v", got.Regenerate, err)
	}
	var want discoverCapture
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !reflect.DeepEqual(want.Request.Headers, got.Request.Headers) || !jsonEqual(t, want.Request.Body, got.Request.Body) {
		t.Errorf("request differs from the capture:\n got %v %s\nwant %v %s",
			got.Request.Headers, got.Request.Body, want.Request.Headers, want.Request.Body)
	}
	if want.Response.Status != got.Response.Status || want.Response.ContentType != got.Response.ContentType ||
		!jsonEqual(t, want.Response.Body, got.Response.Body) {
		t.Errorf("response differs from the capture:\n got %d %s %s\nwant %d %s %s",
			got.Response.Status, got.Response.ContentType, got.Response.Body,
			want.Response.Status, want.Response.ContentType, want.Response.Body)
	}

	// Step 2: the real SDK client lists tools and calls one.
	cs := w.session(t, w.read, nil)
	if v := cs.InitializeResult().ProtocolVersion; v != modern {
		t.Errorf("SDK client negotiated %s, want %s", v, modern)
	}
	names := toolNames(t, cs)
	for _, want := range []string{"get_widget", "list_widgets", "restart_widget"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools/list %v lacks %s", names, want)
		}
	}
	call := callTool(t, cs, "get_widget", map[string]any{"name": "w1"})
	if call.IsError {
		t.Fatalf("get_widget: isError: %s", structured(t, call))
	}
	var widget testprovider.Widget
	if err := json.Unmarshal(structured(t, call), &widget); err != nil {
		t.Fatalf("get_widget structuredContent: %v", err)
	}
	if len(call.Content) != 1 {
		t.Errorf("get_widget content = %d blocks, want the JSON as one text block", len(call.Content))
	} else if tc, ok := call.Content[0].(*mcp.TextContent); !ok || !jsonEqual(t, []byte(tc.Text), structured(t, call)) {
		t.Errorf("get_widget content %#v does not carry the structured result", call.Content[0])
	}
	calls := w.spy.Calls()
	if len(calls) != 1 || calls[0].Op != testprovider.WidgetsGet.Op().ID {
		t.Fatalf("handler calls = %+v, want one %s", calls, testprovider.WidgetsGet.Op().ID)
	}
	if in, ok := calls[0].Input.(testprovider.WidgetInput); !ok || in.Name != "w1" {
		t.Errorf("handler input = %#v, want name w1", calls[0].Input)
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%v: %s", err, a)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return reflect.DeepEqual(x, y)
}

// TC-144: MCP inherits scope, audit and idempotency from the Dispatcher.
func TestMCPInheritsScopeAuditAndIdempotency(t *testing.T) {
	w := newWorld(t, worldOpts{})
	restart := testprovider.WidgetsRestart.Op()

	// The test provider appears in the MCP list.
	none := w.session(t, w.none, nil)
	if names := toolNames(t, none); !slices.Contains(names, restart.MCP.Tool) {
		t.Fatalf("tools/list %v lacks %s", names, restart.MCP.Tool)
	}

	// Step 1: without the scope, a tool error carrying the problem, not a
	// protocol error.
	res := callTool(t, none, restart.MCP.Tool, map[string]any{"name": "w1"})
	if !res.IsError {
		t.Fatalf("restart without scope: isError false: %s", structured(t, res))
	}
	p := structuredProblem(t, structured(t, res))
	if p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != testprovider.WriteScope {
		t.Errorf("problem = %s %v, want forbidden naming %s", p.Slug(), p.Extensions, testprovider.WriteScope)
	}
	if calls := w.spy.Calls(); len(calls) != 0 {
		t.Fatalf("handler ran without the scope: %+v", calls)
	}

	// Step 1: with the scope. Step 2: through MCP with key K, then REST
	// with the same token, key and body.
	const key = "mcp-parity-key-1"
	full := w.session(t, w.full, map[string]string{"Idempotency-Key": key})
	res = callTool(t, full, restart.MCP.Tool, map[string]any{"name": "w1"})
	if res.IsError {
		t.Fatalf("restart with scope: %s", structured(t, res))
	}
	var viaMCP apitypes.Operation
	if err := json.Unmarshal(structured(t, res), &viaMCP); err != nil || viaMCP.ID == "" {
		t.Fatalf("restart structuredContent is not an operation: %v %s", err, structured(t, res))
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+w.full)
	h.Set("Content-Type", "application/json")
	h.Set("Idempotency-Key", key)
	rest := post(t, w.base+"/v1/providers/conformance/widgets/w1/restart", h, strings.NewReader(`{}`))
	if rest.status != http.StatusAccepted {
		t.Fatalf("REST replay: %d %s", rest.status, rest.body)
	}
	var viaREST apitypes.Operation
	if err := json.Unmarshal(rest.body, &viaREST); err != nil {
		t.Fatal(err)
	}
	if viaREST.ID != viaMCP.ID {
		t.Errorf("REST with the same key gave operation %s, MCP gave %s", viaREST.ID, viaMCP.ID)
	}

	// Step 3: audit records carry surface mcp for MCP calls.
	records := func(outcome, token string) []string {
		t.Helper()
		page, err := w.audit.Query(audit.Query{Action: restart.ID, Outcome: outcome, TokenName: token, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var surfaces []string
		for _, r := range page.Items {
			surfaces = append(surfaces, r.Surface)
		}
		return surfaces
	}
	if got := records(audit.OutcomeDenied, "none"); !slices.Equal(got, []string{"mcp"}) {
		t.Errorf("denied record surfaces = %v, want [mcp]", got)
	}
	if got := records(audit.OutcomeRequested, "full"); !slices.Equal(got, []string{"mcp"}) {
		t.Errorf("requested record surfaces = %v, want [mcp]", got)
	}
	if got := records(audit.OutcomeReplayed, "full"); !slices.Equal(got, []string{"rest"}) {
		t.Errorf("replayed record surfaces = %v, want [rest]", got)
	}
	n := 0
	for _, c := range w.spy.Calls() {
		if c.Op == restart.ID {
			n++
		}
	}
	if n > 1 {
		t.Errorf("restart ran %d times, want at most once", n)
	}
}

// TC-145: the adapter contains no business logic.
func TestAdapterImportsNoBusinessLogic(t *testing.T) {
	const pkg = "./internal/mcpadapter"
	direct := strings.Fields(strings.Trim(strings.TrimSpace(goCmd(t, "list", "-f", "{{.Imports}}", pkg)), "[]"))
	if !slices.Contains(direct, module+"/internal/core") || !slices.Contains(direct, sdkModule+"/mcp") {
		t.Fatalf("direct imports %v look wrong: no internal/core or the SDK", direct)
	}
	for _, d := range direct {
		for _, banned := range []string{"/internal/ops", "/internal/store", "/internal/audit"} {
			if d == module+banned {
				t.Errorf("mcpadapter imports %s directly", d)
			}
		}
		if strings.HasPrefix(d, module+"/internal/providers/") {
			t.Errorf("mcpadapter imports provider package %s directly", d)
		}
	}
	deps := strings.Fields(goCmd(t, "list", "-deps", pkg))
	if !slices.Contains(deps, module+"/internal/def") {
		t.Fatalf("go list -deps looks wrong: no internal/def among %d deps", len(deps))
	}
	for _, d := range deps {
		rest, ok := strings.CutPrefix(d, module+"/internal/providers/")
		if ok && !strings.HasSuffix(rest, "/spec") && rest != "" {
			t.Errorf("mcpadapter depends on provider implementation %s", d)
		}
	}
}

// TC-E25: an older protocol version, which the SDK negotiates statelessly,
// meets the same controls as 2026-07-28 and gets no session.
func TestOlderProtocolCannotBypassControls(t *testing.T) {
	w := newWorld(t, worldOpts{origins: []string{okOrigin}})
	restart := testprovider.WidgetsRestart.Op()
	for _, version := range []string{modern, legacy} {
		t.Run(version, func(t *testing.T) {
			call := rpc{version: version, method: "tools/call",
				params: map[string]any{"name": restart.MCP.Tool, "arguments": map[string]any{"name": "w1"}}}

			noToken := call
			if ex := w.send(t, noToken); ex.status != http.StatusUnauthorized || ex.problem(t).Slug() != problem.Unauthorized {
				t.Errorf("no token: %d %s, want 401 unauthorized", ex.status, ex.body)
			}
			hostile := call
			hostile.token, hostile.origin = w.full, "https://evil.example"
			if ex := w.send(t, hostile); ex.status != http.StatusForbidden || ex.problem(t).Slug() != problem.Forbidden {
				t.Errorf("hostile Origin: %d %s, want 403 forbidden", ex.status, ex.body)
			}
			scopeFree := call
			scopeFree.token = w.none
			ex := w.send(t, scopeFree)
			var res toolResult
			ex.result(t, &res)
			if p := structuredProblem(t, res.StructuredContent); !res.IsError || p.Slug() != problem.Forbidden ||
				p.Extensions["requiredScope"] != testprovider.WriteScope {
				t.Errorf("scope-free token: isError %v problem %s %v, want forbidden naming %s",
					res.IsError, p.Slug(), p.Extensions, testprovider.WriteScope)
			}
			if sid := ex.header.Get("Mcp-Session-Id"); sid != "" {
				t.Errorf("tools/call answered with Mcp-Session-Id %q", sid)
			}
		})
	}
	if calls := w.spy.Calls(); len(calls) != 0 {
		t.Fatalf("handler ran: %+v", calls)
	}

	// The legacy handshake creates no session that a later request could
	// use to skip authentication.
	init := rpc{token: w.full, version: legacy, method: "initialize", params: map[string]any{
		"protocolVersion": legacy, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "legacy-client", "version": "1.0.0"},
	}}
	ex := w.send(t, init)
	if ex.status != http.StatusOK {
		t.Fatalf("legacy initialize: %d %s", ex.status, ex.body)
	}
	if sid := ex.header.Get("Mcp-Session-Id"); sid != "" {
		t.Errorf("legacy initialize minted session %q", sid)
	}
	forged := rpc{version: legacy, method: "tools/call", header: map[string]string{"Mcp-Session-Id": "forged-session"},
		params: map[string]any{"name": "get_widget", "arguments": map[string]any{"name": "w1"}}}
	if ex := w.send(t, forged); ex.status != http.StatusUnauthorized {
		t.Errorf("session ID without a token: %d %s, want 401", ex.status, ex.body)
	}
	if calls := w.spy.Calls(); len(calls) != 0 {
		t.Fatalf("handler ran: %+v", calls)
	}
}

// TC-ERR57: the Origin guard runs before authentication and refuses every
// Origin outside mcp.allowedOrigins.
func TestOriginGuard(t *testing.T) {
	// Control: the unwrapped SDK handler accepts a hostile Origin, which is
	// why the guard exists.
	bare := mcp.NewServer(&mcp.Implementation{Name: "control", Version: "0"}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return bare },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	discover := rpc{version: modern, method: "server/discover", origin: "https://evil.example"}
	if ex := post(t, "http://"+ln.Addr().String()+"/mcp", discover.headers(), bytes.NewReader(discover.message())); ex.status != http.StatusOK {
		t.Fatalf("control: the bare SDK handler answered a hostile Origin with %d, want 200: %s", ex.status, ex.body)
	}

	w := newWorld(t, worldOpts{origins: []string{okOrigin}})
	for _, origin := range []string{"https://evil.example", "null", "https://ok.example:444"} {
		for _, token := range []string{"", w.read} {
			r := rpc{token: token, origin: origin, version: modern, method: "server/discover"}
			ex := w.send(t, r)
			if ex.status != http.StatusForbidden || ex.problem(t).Slug() != problem.Forbidden {
				t.Errorf("Origin %s, token %t: %d %s, want 403 forbidden", origin, token != "", ex.status, ex.body)
			}
			// The guard ran before authentication: no challenge.
			if ex.header.Get("WWW-Authenticate") != "" {
				t.Errorf("Origin %s, token %t: authentication ran before the Origin guard", origin, token != "")
			}
		}
	}
	for _, origin := range []string{okOrigin, ""} {
		r := rpc{origin: origin, version: modern, method: "server/discover"}
		if ex := w.send(t, r); ex.status != http.StatusUnauthorized {
			t.Errorf("Origin %q without a token: %d %s, want 401 from authentication", origin, ex.status, ex.body)
		}
		r.token = w.read
		var res struct {
			ResultType string `json:"resultType"`
		}
		w.send(t, r).result(t, &res)
		if res.ResultType != "complete" {
			t.Errorf("Origin %q with a token: resultType %q", origin, res.ResultType)
		}
	}
	// Two Origin headers are not one allowed Origin.
	r := rpc{token: w.read, version: modern, method: "server/discover"}
	h := r.headers()
	h.Add("Origin", okOrigin)
	h.Add("Origin", "https://evil.example")
	if ex := post(t, w.base+"/mcp", h, bytes.NewReader(r.message())); ex.status != http.StatusForbidden {
		t.Errorf("two Origin headers: %d %s, want 403", ex.status, ex.body)
	}
	// REST is not behind the guard.
	h = http.Header{}
	h.Set("Authorization", "Bearer "+w.read)
	h.Set("Origin", "https://evil.example")
	req, err := http.NewRequest(http.MethodGet, w.base+"/v1/capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = h
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("REST with a foreign Origin: %d, want 200", resp.StatusCode)
	}
}

// TC-ERR58: MCP authentication, rate limit and body limit.
func TestAuthenticationAndLimits(t *testing.T) {
	w := newWorld(t, worldOpts{failureLimit: 10})
	r := rpc{version: modern, method: "tools/list"}
	if ex := w.send(t, r); ex.status != http.StatusUnauthorized || ex.problem(t).Slug() != problem.Unauthorized {
		t.Errorf("no token: %d %s, want 401", ex.status, ex.body)
	}
	r.token = "nexus_bogus.bogus"
	if ex := w.send(t, r); ex.status != http.StatusUnauthorized || ex.problem(t).Slug() != problem.Unauthorized {
		t.Errorf("bad token: %d %s, want 401", ex.status, ex.body)
	}
	for i := 3; i <= 10; i++ {
		if ex := w.send(t, r); ex.status != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d, want 401", i, ex.status)
		}
	}
	ex := w.send(t, r)
	if ex.status != http.StatusTooManyRequests || ex.problem(t).Slug() != problem.RateLimited {
		t.Errorf("11th failure: %d %s, want 429", ex.status, ex.body)
	}
	page, err := w.audit.Query(audit.Query{Outcome: audit.OutcomeDenied, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range page.Items {
		if rec.Surface != "mcp" {
			t.Errorf("denied record %s has surface %q, want mcp", rec.ID, rec.Surface)
		}
	}

	// Oversize body, with a valid token, from a source that has not failed.
	big := newWorld(t, worldOpts{})
	for _, chunked := range []bool{false, true} {
		body := oversizeCall(maxBody + 1)
		var rd io.Reader = bytes.NewReader(body)
		if chunked {
			rd = io.MultiReader(rd) // Unknown length: sent chunked
		}
		call := rpc{token: big.read, version: modern, method: "tools/call", params: map[string]any{"name": "get_widget"}}
		ex := post(t, big.base+"/mcp", call.headers(), rd)
		if ex.status != http.StatusRequestEntityTooLarge {
			t.Errorf("oversize body (chunked %t): %d %.200s, want 413", chunked, ex.status, ex.body)
		}
	}
	if calls := big.spy.Calls(); len(calls) != 0 {
		t.Fatalf("handler ran for an oversize body: %d calls", len(calls))
	}
	// Control: the same call under the limit runs.
	call := rpc{token: big.read, version: modern, method: "tools/call", params: map[string]any{"name": "get_widget"}}
	var res toolResult
	post(t, big.base+"/mcp", call.headers(), bytes.NewReader(oversizeCall(maxBody-1024))).result(t, &res)
	if res.IsError || len(big.spy.Calls()) != 1 {
		t.Fatalf("under-limit call: isError %v, calls %d: %s", res.IsError, len(big.spy.Calls()), res.StructuredContent)
	}
}

// oversizeCall is a get_widget call of exactly n bytes: the padding sits in
// _meta, so the call is valid and would run if the body were accepted.
func oversizeCall(n int) []byte {
	build := func(pad int) []byte {
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
			"name": "get_widget", "arguments": map[string]any{"name": "w1"},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    modern,
				"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "nexus-wire-test", "version": "1.0.0"},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				"pad": strings.Repeat("x", pad),
			},
		}})
		if err != nil {
			panic(err)
		}
		return b
	}
	b := build(0)
	b = build(n - len(b))
	if len(b) != n {
		panic(fmt.Sprintf("oversizeCall: %d bytes, want %d", len(b), n))
	}
	return b
}

// TC-ERR58: the adapter sets the SDK's body limit itself, so /mcp keeps
// NFR-4's 1 MiB without the server's own limit in front of it. The SDK's
// default is 4 MiB, so a 2 MiB body tells them apart.
func TestAdapterSetsBodyLimitExplicitly(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	spy := &testprovider.Spy{}
	reg, err := core.New([]provider.Provider{testprovider.New(spy)}, core.Options{Logger: log,
		Providers: map[string]core.ProviderConfig{testprovider.Name: {Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	disp := core.NewDispatcher(reg, core.DispatcherOptions{Logger: log})
	if _, err := mcpadapter.New(reg, disp, mcpadapter.Options{Logger: log}); err == nil {
		t.Error("New without a body limit succeeded; the SDK would apply its 4 MiB default")
	}
	a, err := mcpadapter.New(reg, disp, mcpadapter.Options{MaxBodyBytes: maxBody, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	// Stands in for the authentication middleware.
	caller := auth.Principal{TokenID: "t1", Name: "direct", Scopes: []string{testprovider.ReadScope}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), caller)))
	})}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	url := "http://" + ln.Addr().String() + "/mcp"
	call := rpc{version: modern, method: "tools/call", params: map[string]any{"name": "get_widget"}}

	if n := sdkDefault / 2; n <= maxBody {
		t.Fatalf("test body %d does not exceed the limit %d", n, maxBody)
	}
	ex := post(t, url, call.headers(), io.MultiReader(bytes.NewReader(oversizeCall(sdkDefault/2))))
	if ex.status != http.StatusRequestEntityTooLarge {
		t.Errorf("2 MiB chunked body: %d %.200s, want 413", ex.status, ex.body)
	}
	if calls := spy.Calls(); len(calls) != 0 {
		t.Fatalf("handler ran for an oversize body: %d calls", len(calls))
	}
	// The principal reaches the handler through ctx.
	var res toolResult
	post(t, url, call.headers(), bytes.NewReader(oversizeCall(4096))).result(t, &res)
	if res.IsError || len(spy.Calls()) != 1 {
		t.Fatalf("small call: isError %v, calls %d: %s", res.IsError, len(spy.Calls()), res.StructuredContent)
	}
}
