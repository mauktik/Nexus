// Package conformance_test is the conformance suite (4.2): rules every
// operation in the catalog must meet, checked generically so a provider
// added later is covered without editing the suite's logic. A catalog
// provider other than system needs a stub in catalogStubs before the
// integration tests can bind it; until it has one they fail and say so.
package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/conformance/testprovider"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	apt "github.com/mauktik/nexus/internal/providers/apt/spec"
	docker "github.com/mauktik/nexus/internal/providers/docker/spec"
	nexusd "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	pihole "github.com/mauktik/nexus/internal/providers/pihole/spec"
	scripts "github.com/mauktik/nexus/internal/providers/scripts/spec"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

const module = "github.com/mauktik/nexus"

// catalogStubs binds stub handlers for each catalog provider other than
// system, whose ops core binds itself. A provider added to catalog.All()
// adds one entry here, built with testprovider.NewStub and the spy helpers
// (testprovider.Sync, Stream, InProcess, Upload), one binding per op.
var catalogStubs = map[string]func(*testprovider.Spy) provider.Provider{
	docker.Name: func(s *testprovider.Spy) provider.Provider {
		return testprovider.NewStub(docker.Spec(),
			testprovider.Sync(s, docker.List),
			testprovider.Sync(s, docker.Get),
			testprovider.Stream(s, docker.Logs),
			testprovider.InProcess(s, docker.Start),
			testprovider.InProcess(s, docker.Stop),
			testprovider.InProcess(s, docker.Restart),
			testprovider.InProcess(s, docker.Remove),
			testprovider.InProcess(s, docker.Update),
			testprovider.InProcess(s, docker.CheckUpdates),
		)
	},
	apt.Name: func(s *testprovider.Spy) provider.Provider {
		return testprovider.NewStub(apt.Spec(),
			testprovider.Sync(s, apt.List),
			testprovider.Sync(s, apt.Get),
			testprovider.Sync(s, apt.UpgradableList),
			testprovider.InProcess(s, apt.Refresh),
			testprovider.InProcess(s, apt.Upgrade),
			testprovider.InProcess(s, apt.Install),
			testprovider.InProcess(s, apt.Remove),
		)
	},
	scripts.Name: func(s *testprovider.Spy) provider.Provider {
		return testprovider.NewStub(scripts.Spec(),
			testprovider.Sync(s, scripts.List),
			testprovider.Sync(s, scripts.Get),
			testprovider.InProcess(s, scripts.Invoke),
		)
	},
	// reload is InProcess and restart, update and rollback Detached in the
	// real provider; InProcess stubs both (testprovider.InProcess).
	nexusd.Name: func(s *testprovider.Spy) provider.Provider {
		return testprovider.NewStub(nexusd.Spec(),
			testprovider.Sync(s, nexusd.List),
			testprovider.Sync(s, nexusd.ValidateConfig),
			testprovider.InProcess(s, nexusd.Reload),
			testprovider.InProcess(s, nexusd.Restart),
			testprovider.InProcess(s, nexusd.Update),
			testprovider.InProcess(s, nexusd.Rollback),
			testprovider.Upload(s, nexusd.Upload),
		)
	},
	pihole.Name: func(s *testprovider.Spy) provider.Provider {
		return testprovider.NewStub(pihole.Spec(),
			testprovider.Sync(s, pihole.StatusGet),
			testprovider.Sync(s, pihole.SummaryGet),
			testprovider.Sync(s, pihole.BlockingGet),
			testprovider.InProcess(s, pihole.StopBlocking),
			testprovider.InProcess(s, pihole.StartBlocking),
			testprovider.Sync(s, pihole.DomainsList),
			testprovider.Sync(s, pihole.DomainsGet),
			testprovider.InProcess(s, pihole.AddDomain),
			testprovider.InProcess(s, pihole.RemoveDomain),
			testprovider.Sync(s, pihole.AdlistsList),
			testprovider.Sync(s, pihole.AdlistsGet),
			testprovider.InProcess(s, pihole.AddAdlist),
			testprovider.InProcess(s, pihole.RemoveAdlist),
			testprovider.InProcess(s, pihole.UpdateGravity),
			testprovider.InProcess(s, pihole.RestartDNS),
			testprovider.InProcess(s, pihole.FlushCache),
			testprovider.Sync(s, pihole.ZonesList),
			testprovider.Sync(s, pihole.ZonesGet),
			testprovider.InProcess(s, pihole.AddZone),
			testprovider.InProcess(s, pihole.ZonesRemove),
			testprovider.Sync(s, pihole.RecordsList),
			testprovider.Sync(s, pihole.RecordsGet),
			testprovider.InProcess(s, pihole.AddRecord),
			testprovider.InProcess(s, pihole.RecordsUpdate),
			testprovider.InProcess(s, pihole.RecordsRemove),
			testprovider.InProcess(s, pihole.ApplyDNSChanges),
		)
	},
}

// stack is the real server, Dispatcher, audit log and operation manager over
// the system ops, the conformance provider and a stub of every other catalog
// provider.
type stack struct {
	base      string
	reg       *core.Registry
	audit     *audit.Log
	spy       *testprovider.Spy
	full      string            // Holds every scope any op names
	none      string            // Holds no scope
	exact     map[string]string // Scope -> token holding only that scope
	exactName map[string]string // Scope -> that token's name
}

const noneToken = "none"

func newStack(t *testing.T) *stack {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	spy := &testprovider.Spy{}
	providers := []provider.Provider{testprovider.New(spy)}
	enabled := map[string]core.ProviderConfig{testprovider.Name: {Enabled: true}}
	for _, spec := range catalog.All() {
		if spec.Name == def.SystemProvider {
			continue
		}
		stub, ok := catalogStubs[spec.Name]
		if !ok {
			t.Fatalf("catalog provider %q has no stub in catalogStubs, so the conformance suite cannot call its ops", spec.Name)
		}
		providers = append(providers, stub(spy))
		enabled[spec.Name] = core.ProviderConfig{Enabled: true}
	}

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
	reg, err := core.New(providers, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: log, Audit: alog, Ops: mgr, Providers: enabled,
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}

	scopes := allScopes(reg)
	s := &stack{reg: reg, audit: alog, spy: spy, exact: map[string]string{}, exactName: map[string]string{}}
	mint := func(name string, scopes ...string) string {
		tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: name, Scopes: scopes})
		if err != nil {
			t.Fatalf("token %s: %v", name, err)
		}
		return tok
	}
	s.full = mint("full", scopes...)
	s.none = mint(noneToken)
	for i, sc := range scopes {
		s.exactName[sc] = fmt.Sprintf("exact-%d", i)
		s.exact[sc] = mint(s.exactName[sc], sc)
	}

	// The walks over every op make one denial per scoped op with the same
	// token, and with the docker, apt, scripts, nexusd and pihole catalogs
	// that is more than core.DefaultNoteLimitPerMinute (60) in one minute;
	// past it the dispatcher collapses denials into a rate-limited aggregate
	// and the per-op denial records the tests count are not written.
	srv, err := server.New(reg, server.Options{Logger: log, Auth: auth.New(stateDir, auth.Options{Logger: log}),
		NoteLimitPerMinute: 1000})
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
	s.base = "http://" + ln.Addr().String()

	// Every catalog op must be served, or the generated tests would skip it.
	served := map[string]bool{}
	for _, e := range reg.Entries() {
		served[e.Binding.Op().ID] = true
	}
	for _, op := range catalog.Ops() {
		if !served[op.ID] {
			t.Fatalf("catalog op %s is not served by the conformance stack", op.ID)
		}
	}
	return s
}

// allScopes is every scope an op or a provider names, sorted.
func allScopes(reg *core.Registry) []string {
	var out []string
	add := func(s string) {
		if s != "" && !strings.Contains(s, "{") && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, e := range reg.Entries() {
		add(e.Binding.Op().Scope.Name)
	}
	for _, spec := range reg.Specs() {
		add(spec.ReadScope)
	}
	add("audit:read")
	slices.Sort(out)
	return out
}

func (s *stack) ops() []def.Op {
	var out []def.Op
	for _, e := range s.reg.Entries() {
		out = append(out, e.Binding.Op())
	}
	return out
}

func (s *stack) readScope(prov string) string {
	for _, spec := range s.reg.Specs() {
		if spec.Name == prov {
			return spec.ReadScope
		}
	}
	return ""
}

// response is one HTTP answer.
type response struct {
	status int
	body   []byte
}

func (r response) problem(t *testing.T) *problem.Error {
	t.Helper()
	var p problem.Error
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("status %d: body is not a problem: %v\n%.300s", r.status, err, r.body)
	}
	return &p
}

func (s *stack) do(t *testing.T, method, path, token string, body []byte) response {
	t.Helper()
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	return s.doBody(t, method, path, token, contentType, body)
}

// doBody is do with the body's content type; a nil body sends none.
func (s *stack) doBody(t *testing.T, method, path, token, contentType string, body []byte) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %.200s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, body: b}
}

// octetStream is the content type the suite sends an upload body as.
const octetStream = "application/octet-stream"

// uploadBody is the body the suite sends to a KindUpload op: well under
// testprovider.UploadMaxBytes.
var uploadBody = []byte("conformance upload body\n")

// call sends input to op over its REST route: route wildcards go in the
// path, the rest in the query for GET and for a KindUpload op, and in the
// JSON body for any other POST. A KindUpload op's body is uploadBody, sent
// as application/octet-stream.
func (s *stack) call(t *testing.T, op def.Op, token string, input map[string]any) response {
	t.Helper()
	method, path, err := openapi.Route(op)
	if err != nil {
		t.Fatal(err)
	}
	rest := map[string]any{}
	for k, v := range input {
		rest[k] = v
	}
	for _, w := range openapi.Wildcards(path) {
		v, ok := rest[w].(string)
		if !ok {
			t.Fatalf("op %s: no string value for route wildcard {%s}", op.ID, w)
		}
		path = strings.Replace(path, "{"+w+"}", url.PathEscape(v), 1)
		delete(rest, w)
	}
	if method == http.MethodGet || op.Kind == def.KindUpload {
		q := url.Values{}
		for k, v := range rest {
			switch v := v.(type) {
			case []any:
				for _, x := range v {
					q.Add(k, fmt.Sprint(x))
				}
			case map[string]any:
				t.Fatalf("op %s: object field %s cannot be sent as a query parameter", op.ID, k)
			default:
				q.Set(k, fmt.Sprint(v))
			}
		}
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		if op.Kind == def.KindUpload {
			return s.doBody(t, method, path, token, octetStream, uploadBody)
		}
		return s.do(t, method, path, token, nil)
	}
	body, err := json.Marshal(rest)
	if err != nil {
		t.Fatal(err)
	}
	return s.do(t, method, path, token, body)
}

// restart starts the conformance provider's long-running op with token and
// returns its operation, after the spy saw it run.
func (s *stack) restart(t *testing.T, token string) apitypes.Operation {
	t.Helper()
	r := s.call(t, testprovider.WidgetsRestart.Op(), token, map[string]any{"name": "w1"})
	if r.status != http.StatusAccepted {
		t.Fatalf("restart: %d %s", r.status, r.body)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(r.body, &op); err != nil {
		t.Fatal(err)
	}
	s.waitCall(t, func(c testprovider.Call) bool { return c.OperationID == op.ID })
	return s.waitTerminal(t, op.ID)
}

func (s *stack) waitCall(t *testing.T, match func(testprovider.Call) bool) testprovider.Call {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := s.spy.Wait(ctx, match)
	if err != nil {
		t.Fatalf("handler call never arrived: %v", err)
	}
	return c
}

func (s *stack) waitTerminal(t *testing.T, id string) apitypes.Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := s.do(t, http.MethodGet, "/v1/operations/"+id, s.full, nil)
		if r.status != http.StatusOK {
			t.Fatalf("get operation %s: %d %s", id, r.status, r.body)
		}
		var op apitypes.Operation
		if err := json.Unmarshal(r.body, &op); err != nil {
			t.Fatal(err)
		}
		if op.Status.Terminal() {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s still %s", id, op.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *stack) records(t *testing.T, q audit.Query) []string {
	t.Helper()
	q.Limit = 1000
	page, err := s.audit.Query(q)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	ids := make([]string, len(page.Items))
	for i, r := range page.Items {
		ids[i] = r.ID + " " + r.Problem
	}
	return ids
}

// TC-82: FR-3 acceptance. The test provider reaches capabilities, the
// OpenAPI document, the MCP tool list (8.1) and the Dispatcher's scope and
// audit rules through core.New alone; core and the transports import
// nothing from this suite.
func TestTestProviderNeedsNoCoreOrTransportChange(t *testing.T) {
	// Setup: core and transports do not depend on the conformance packages.
	for _, pkg := range []string{"./internal/core", "./internal/server", "./internal/mcpadapter"} {
		cmd := exec.Command("go", "list", "-deps", pkg)
		cmd.Dir = filepath.Join("..", "..")
		out, err := cmd.Output()
		if err != nil {
			stderr := ""
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				stderr = string(ee.Stderr)
			}
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, stderr)
		}
		deps := strings.Fields(string(out))
		if !slices.Contains(deps, module+"/internal/def") {
			t.Fatalf("go list -deps %s looks wrong: no internal/def among %d deps", pkg, len(deps))
		}
		for _, d := range deps {
			if strings.HasPrefix(d, module+"/internal/conformance") {
				t.Errorf("%s imports %s", pkg, d)
			}
		}
	}

	s := newStack(t)
	spec := testprovider.Spec()

	// Step 1: capabilities.
	r := s.do(t, http.MethodGet, "/v1/capabilities", s.none, nil)
	if r.status != http.StatusOK {
		t.Fatalf("capabilities: %d %s", r.status, r.body)
	}
	var caps apitypes.Capabilities
	if err := json.Unmarshal(r.body, &caps); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(caps.Providers, func(p apitypes.ProviderCapability) bool { return p.Name == spec.Name })
	if i < 0 {
		t.Fatalf("provider %s missing from capabilities: %s", spec.Name, r.body)
	}
	got := caps.Providers[i]
	if got.Status != apitypes.ProviderEnabled || !slices.Equal(got.ResourceTypes, []string{"widgets"}) ||
		!slices.Equal(got.Actions, []string{"echo", "restart"}) {
		t.Errorf("capabilities entry = %+v", got)
	}

	// Step 1: the OpenAPI document has every op at its route.
	r = s.do(t, http.MethodGet, openapi.Path, s.none, nil)
	if r.status != http.StatusOK {
		t.Fatalf("openapi: %d %s", r.status, r.body)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatal(err)
	}
	for _, op := range spec.Ops {
		method, path, err := openapi.Route(op)
		if err != nil {
			t.Fatal(err)
		}
		if got := doc.Paths[path][strings.ToLower(method)].OperationID; got != op.ID {
			t.Errorf("OpenAPI %s %s: operationId %q, want %q", method, path, got, op.ID)
		}
	}

	// Step 1: the MCP tool list has a tool for every op with an MCP
	// binding, the test provider's included, each with object schemas.
	// Stream ops are tools too: each returns one bounded page (8.2).
	tools := s.mcpTools(t, s.none)
	for _, op := range s.ops() {
		if op.MCP == nil {
			continue
		}
		tool, ok := tools[op.MCP.Tool]
		if !ok {
			t.Errorf("MCP tool %s of op %s is not listed", op.MCP.Tool, op.ID)
			continue
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("MCP tool %s: inputSchema type %v, want object", op.MCP.Tool, tool.InputSchema["type"])
		}
		if tool.OutputSchema != nil && tool.OutputSchema["type"] != "object" {
			t.Errorf("MCP tool %s: outputSchema type %v, want object", op.MCP.Tool, tool.OutputSchema["type"])
		}
	}
	for _, op := range spec.Ops {
		if op.MCP != nil {
			if _, ok := tools[op.MCP.Tool]; !ok {
				t.Errorf("test provider tool %s is not listed", op.MCP.Tool)
			}
		}
	}

	// Step 2: without the scope, 403 naming it and a denial record.
	restart := testprovider.WidgetsRestart.Op()
	r = s.call(t, restart, s.none, map[string]any{"name": "w1"})
	if r.status != http.StatusForbidden {
		t.Fatalf("restart without scope: %d %s", r.status, r.body)
	}
	if p := r.problem(t); p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != testprovider.WriteScope {
		t.Errorf("problem = %s %v, want forbidden naming %s", p.Slug(), p.Extensions, testprovider.WriteScope)
	}
	if got := s.records(t, audit.Query{Action: restart.ID, Outcome: audit.OutcomeDenied, TokenName: noneToken}); len(got) != 1 {
		t.Errorf("denied records = %v, want 1", got)
	}
	if calls := s.spy.Calls(); len(calls) != 0 {
		t.Errorf("handler ran without the scope: %+v", calls)
	}

	// Step 2: with the scope, it runs, after a requested record.
	op := s.restart(t, s.exact[testprovider.WriteScope])
	if op.Status != apitypes.StatusSucceeded {
		t.Errorf("operation ended %s, want Succeeded", op.Status)
	}
	if got := s.records(t, audit.Query{Action: restart.ID, Outcome: audit.OutcomeRequested,
		TokenName: s.exactName[testprovider.WriteScope]}); len(got) != 1 {
		t.Errorf("requested records = %v, want 1", got)
	}
}

// mcpTool is one tools/list entry.
type mcpTool struct {
	Name         string         `json:"name"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema"`
}

// mcpTools lists the MCP tools with a raw 2026-07-28 tools/list.
func (s *stack) mcpTools(t *testing.T, token string) map[string]mcpTool {
	t.Helper()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"conformance","version":"1"},` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`)
	req, err := http.NewRequest(http.MethodPost, s.base+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Result struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		} `json:"result"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &msg) != nil {
		t.Fatalf("tools/list: %d %.300s", resp.StatusCode, b)
	}
	if msg.Result.NextCursor != "" {
		t.Fatalf("tools/list is paginated; the suite reads one page")
	}
	out := map[string]mcpTool{}
	for _, tool := range msg.Result.Tools {
		out[tool.Name] = tool
	}
	return out
}

// requiredScope is the scope a call of op needs, and false for an op any
// valid token may call. target is the operation a FromTarget op names.
func (s *stack) requiredScope(op def.Op, target apitypes.Operation, startScope string) (string, bool) {
	switch {
	case op.Scope.FromTarget && op.OperationControl:
		return startScope, true
	case op.Scope.FromTarget:
		return s.readScope(target.Provider), true
	case op.Scope.Name != "":
		return op.Scope.Name, true
	case op.Provider == def.SystemProvider:
		return "", false
	}
	return s.readScope(op.Provider), true
}

// TC-86: every op, generated from the registry, refuses a token without its
// scope with 403 naming the scope and a denial record, and lets a token
// holding exactly that scope past authorization.
func TestScopeAndAuditCoverEveryOp(t *testing.T) {
	s := newStack(t)
	target := s.restart(t, s.full)
	inputs := func(op def.Op) map[string]any {
		in := map[string]any{}
		_, path, _ := openapi.Route(op)
		for _, w := range openapi.Wildcards(path) {
			in[w] = "w1"
			if op.Scope.FromTarget {
				in[w] = target.ID
			}
		}
		return in
	}

	checked := 0
	for _, op := range s.ops() {
		need, scoped := s.requiredScope(op, target, testprovider.WriteScope)
		before := len(s.records(t, audit.Query{Action: op.ID, Outcome: audit.OutcomeDenied, TokenName: noneToken}))

		// Step 1.
		r := s.call(t, op, s.none, inputs(op))
		after := s.records(t, audit.Query{Action: op.ID, Outcome: audit.OutcomeDenied, TokenName: noneToken})
		if !scoped {
			if r.status == http.StatusForbidden || len(after) != before {
				t.Errorf("%s needs only a valid token, but a scope-free token got %d with %d denials",
					op.ID, r.status, len(after)-before)
			}
		} else {
			if r.status != http.StatusForbidden {
				t.Errorf("%s without %s: %d %.200s, want 403", op.ID, need, r.status, r.body)
			} else if p := r.problem(t); p.Extensions["requiredScope"] != need {
				t.Errorf("%s: requiredScope %v, want %s", op.ID, p.Extensions["requiredScope"], need)
			}
			if len(after) != before+1 {
				t.Errorf("%s: %d new denial records, want 1", op.ID, len(after)-before)
			}
		}

		// Step 2.
		tok := s.none
		if scoped {
			tok = s.exact[need]
		}
		if tok == "" {
			t.Fatalf("%s: no token for scope %q", op.ID, need)
		}
		if r := s.call(t, op, tok, inputs(op)); r.status == http.StatusForbidden {
			t.Errorf("%s with exactly %q: 403 %s", op.ID, need, r.body)
		}
		checked++
	}
	if want := len(catalog.Ops()) + len(testprovider.Spec().Ops); checked != want {
		t.Errorf("checked %d ops, want %d", checked, want)
	}
}

// visitSchema calls visit for s and every schema inside it, with a path of
// property names: "*" stands for an array item and "{key}" for any key of
// an object with additional properties.
func visitSchema(s *jsonschema.Schema, path string, visit func(path string, s *jsonschema.Schema)) {
	if s == nil {
		return
	}
	visit(path, s)
	for _, name := range sortedKeys(s.Properties) {
		visitSchema(s.Properties[name], path+"/"+name, visit)
	}
	visitSchema(s.Items, path+"/*", visit)
	for _, p := range s.PrefixItems {
		visitSchema(p, path+"/*", visit)
	}
	if !isFalse(s.AdditionalProperties) {
		visitSchema(s.AdditionalProperties, path+"/{key}", visit)
	}
	for _, sub := range slices.Concat(s.AllOf, s.AnyOf, s.OneOf) {
		visitSchema(sub, path, visit)
	}
	for _, name := range sortedKeys(s.Defs) {
		visitSchema(s.Defs[name], "#/$defs/"+name, visit)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// isFalse reports whether s is the schema false, as jsonschema.For writes
// for a struct's additionalProperties.
func isFalse(s *jsonschema.Schema) bool {
	if s == nil {
		return false
	}
	b, err := json.Marshal(s)
	return err == nil && string(b) == "false"
}

func hasType(s *jsonschema.Schema, typ string) bool {
	return s.Type == typ || slices.Contains(s.Types, typ)
}

func isFreeText(s *jsonschema.Schema) bool {
	v, ok := s.Extra[testprovider.FreeTextKeyword].(bool)
	return ok && v
}

func constrained(s *jsonschema.Schema) bool {
	return len(s.Enum) > 0 || s.Const != nil || s.Pattern != "" || isFreeText(s)
}

func inputSchema(t *testing.T, op def.Op) *jsonschema.Schema {
	t.Helper()
	r, err := op.InputSchema()
	if err != nil {
		t.Fatalf("op %s: %v", op.ID, err)
	}
	return r.Schema()
}

// unconstrained lists op's input strings that accept any value, as
// "<op> <path>": a string property without enum, pattern or the free-text
// marker, an object whose additional property names are unconstrained, and
// a schema that accepts any type, strings included.
func unconstrained(t *testing.T, op def.Op) []string {
	t.Helper()
	var out []string
	visitSchema(inputSchema(t, op), "", func(path string, s *jsonschema.Schema) {
		switch {
		case hasType(s, "string") && !constrained(s):
			out = append(out, op.ID+" "+path)
		case s.Type == "" && len(s.Types) == 0 && s.Ref == "" && !isFalse(s) && !constrained(s) &&
			len(s.AllOf)+len(s.AnyOf)+len(s.OneOf) == 0:
			out = append(out, op.ID+" "+path+" (any type)")
		case hasType(s, "object") && s.AdditionalProperties != nil && !isFalse(s.AdditionalProperties) &&
			(s.PropertyNames == nil || !constrained(s.PropertyNames)) && !isFreeText(s):
			out = append(out, op.ID+" "+path+"/{key} (property name)")
		}
	})
	return out
}

// declared is every op the suite checks without a server: the catalog and
// the conformance provider.
func declared() []def.Op {
	return slices.Concat(catalog.Ops(), testprovider.Spec().Ops)
}

type controlNested struct {
	Label string `json:"label"`
}

type controlIn struct {
	Name    string        `json:"name"`
	Options controlNested `json:"options"`
}

type commandIn struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

type nothing struct{}

func controlOp(id, rt, action string, kind def.Kind) def.Op {
	return def.Op{
		ID: id, Provider: "control", Kind: kind, ResourceType: rt, Action: action,
		Summary: "Control", Description: "A planted control op that the conformance rules must reject.",
		Scope: def.Scope{Name: "control:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "control", Verb: "get"},
		Refine: func(s *jsonschema.Schema) {
			if p := s.Properties["name"]; p != nil {
				p.Pattern = `^[a-z]+$`
			}
			if p := s.Properties["command"]; p != nil {
				p.Pattern = `^.*$`
			}
		},
	}
}

// Planted controls: declared through def.Define, never bound or catalogued.
var (
	controlUnconstrained = def.Define[controlIn, nothing](controlOp("control.things.get", "things", "", def.KindGet))
	controlCommand       = def.Define[commandIn, nothing](controlOp("control.jobs.get", "jobs", "", def.KindGet))
	controlCreate        = def.Define[nothing, nothing](controlOp("control.containers.create", "", "create", def.KindProviderAction))
)

// TC-83: every string input of every op has enum, pattern or the free-text
// marker, nested properties included; the planted control fails.
func TestEveryStringInputIsConstrained(t *testing.T) {
	ops := declared()
	if len(catalog.Ops()) == 0 {
		t.Fatal("the catalog is empty")
	}
	found := 0
	for _, op := range ops {
		visitSchema(inputSchema(t, op), "", func(_ string, s *jsonschema.Schema) {
			if hasType(s, "string") {
				found++
			}
		})
		for _, f := range unconstrained(t, op) {
			t.Errorf("unconstrained input string: %s", f)
		}
	}
	if found == 0 {
		t.Fatal("no string inputs were found, so the walk checked nothing")
	}

	got := unconstrained(t, controlUnconstrained.Op())
	if want := []string{"control.things.get /options/label"}; !slices.Equal(got, want) {
		t.Errorf("control: findings %q, want %q", got, want)
	}
}

// forbiddenFields are input field names, normalized, that would carry a
// command, a shell string, an executable or file path, or a container
// specification (FR-25, OOS-6). A name containing any of the first group
// matches; the second group matches exactly.
var (
	forbiddenFieldParts = []string{"command", "shell", "exec", "path"}
	forbiddenFieldNames = []string{
		"cmd", "argv", "entrypoint", "image", "binds", "mounts", "privileged", "hostconfig", "capadd",
		"devices", "portbindings", "networkmode", "pidmode", "ipcmode", "securityopt", "spec",
		"containerspec", "createspec",
	}
)

// forbiddenWords are op ID and route words outside v1: container create,
// exec and run (OOS-6), script definition changes (FR-21), reboot and
// shutdown, repositories and keys (OOS-7), desired state and reconcile
// (OOS-2, ASM-2), schedules (OOS-5), metrics (OOS-4) and tokens (no
// network path manages tokens). Self-update is no longer out of scope
// (OOS-11): it is in scope through the nexusd provider, whose op IDs and
// routes use none of these words. "self" and "selfupdate" stay as guards
// against any other self-modifying surface.
var forbiddenWords = []string{
	"create", "exec", "execute", "run", "shell", "reboot", "shutdown", "poweroff", "halt",
	"repository", "repositories", "repo", "repos", "sources", "key", "keys",
	"reconcile", "drift", "deploy", "deployment", "deployments", "template", "templates",
	"schedule", "schedules", "cron", "metrics", "self", "selfupdate", "token", "tokens",
}

func normalize(name string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(name))
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '.' || r == '/' || r == '-' || r == '_' || r == '{' || r == '}'
	})
}

// executionSurface lists what makes op an arbitrary-execution surface.
func executionSurface(t *testing.T, op def.Op) []string {
	t.Helper()
	var out []string
	visitSchema(inputSchema(t, op), "", func(path string, s *jsonschema.Schema) {
		for _, name := range sortedKeys(s.Properties) {
			n := normalize(name)
			if slices.Contains(forbiddenFieldNames, n) ||
				slices.ContainsFunc(forbiddenFieldParts, func(p string) bool { return strings.Contains(n, p) }) {
				out = append(out, fmt.Sprintf("%s: input field %s/%s", op.ID, path, name))
			}
		}
	})

	method, route, err := openapi.Route(op)
	if err != nil {
		t.Fatalf("op %s: %v", op.ID, err)
	}
	if method == http.MethodPut || method == http.MethodPatch {
		out = append(out, fmt.Sprintf("%s: %s route", op.ID, method))
	}
	for _, w := range slices.Concat(words(op.ID), words(route)) {
		if slices.Contains(forbiddenWords, w) {
			out = append(out, fmt.Sprintf("%s: %q in op ID or route %s", op.ID, w, route))
		}
	}
	// Scripts are invoked, never defined or changed through the API.
	if op.Provider == "scripts" || op.ResourceType == "scripts" {
		if slices.Contains([]string{"add", "update", "remove", "delete", "edit", "set"}, op.Action) {
			out = append(out, fmt.Sprintf("%s: script action %q", op.ID, op.Action))
		}
	}
	return out
}

// TC-85: no op takes a command, shell, exec, path or container spec field,
// and no op ID or route is an excluded capability or a PUT; the planted
// controls fail.
func TestNoArbitraryExecutionSurface(t *testing.T) {
	for _, op := range declared() {
		for _, f := range executionSurface(t, op) {
			t.Errorf("execution surface: %s", f)
		}
	}

	if got := executionSurface(t, controlCommand.Op()); !slices.Equal(got, []string{"control.jobs.get: input field /command"}) {
		t.Errorf("command control: %q", got)
	}
	got := executionSurface(t, controlCreate.Op())
	if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, `"create"`) }) {
		t.Errorf("create control: %q", got)
	}
}

// TC-SM-101 (WI 6.1; CON-4): the no-arbitrary-execution surface still holds
// with nexusd in the catalog, and the forbiddenWords comment says why
// "self" and "selfupdate" stay.
func TestTCSM101_TheNoArbitraryExecutionSurfaceStill(t *testing.T) {
	// Step 1: the nexusd ops are among those TestNoArbitraryExecutionSurface
	// walks, and none is a surface.
	var walked []string
	for _, op := range declared() {
		if op.Provider != nexusd.Name {
			continue
		}
		walked = append(walked, op.ID)
		for _, f := range executionSurface(t, op) {
			t.Errorf("execution surface: %s", f)
		}
		_, route, err := openapi.Route(op)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range slices.Concat(words(op.ID), words(route)) {
			if slices.Contains([]string{"create", "self", "selfupdate"}, w) {
				t.Errorf("%s: %q in op ID or route %s", op.ID, w, route)
			}
		}
	}
	if len(walked) != len(nexusd.Spec().Ops) {
		t.Fatalf("walked nexusd ops %v, want all %d", walked, len(nexusd.Spec().Ops))
	}

	// Step 2: the words stay as guards, and the comment says why.
	for _, w := range []string{"create", "self", "selfupdate"} {
		if !slices.Contains(forbiddenWords, w) {
			t.Errorf("forbiddenWords lost %q", w)
		}
	}
	src, err := os.ReadFile("conformance_test.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	end := strings.Index(text, "var forbiddenWords = ")
	start := strings.LastIndex(text[:end], "// forbiddenWords are ")
	if start < 0 || end < 0 {
		t.Fatal("the forbiddenWords declaration or its comment is missing")
	}
	comment := strings.Join(strings.Fields(strings.ReplaceAll(text[start:end], "//", "")), " ")
	for _, want := range []string{"Self-update is no longer out of scope", "through the nexusd provider",
		`"self" and "selfupdate" stay as guards`} {
		if !strings.Contains(comment, want) {
			t.Errorf("forbiddenWords comment lacks %q: %s", want, comment)
		}
	}
}

// TC-SM-94 (WI 6.1; NFR-7, FR-1, FR-16): the conformance suite covers nexusd
// through catalogStubs. Steps 1 and 2 are TestScopeAndAuditCoverEveryOp and
// TestAttackStringsAgainstEveryStringParameter, which walk every served op;
// this test shows the nexusd ops are among them, sends the attacks through
// uploadId and signature, and calls each mutating nexusd op with a read
// token.
func TestTCSM94_TheConformanceSuiteCoversNexusdThrough(t *testing.T) {
	s := newStack(t)
	served := map[string]def.Op{}
	for _, op := range s.ops() {
		if op.Provider == nexusd.Name {
			served[op.ID] = op
		}
	}
	if len(served) != 7 {
		t.Fatalf("the stack serves %d nexusd ops, want 7", len(served))
	}

	// Step 2: the hostile strings through uploadId and signature are 400
	// validation-failed, and no handler runs.
	sm := sampler{}
	for _, c := range []struct {
		op   def.Op
		path string
	}{
		{nexusd.Update.Op(), "/uploadId"},
		{nexusd.Upload.Op(), "/signature"},
	} {
		if !slices.Contains(stringParams(t, c.op), c.path) {
			t.Fatalf("%s %s is not a string parameter the attack walk visits", c.op.ID, c.path)
		}
		base, _ := sm.value(t, c.op, "", inputSchema(t, c.op)).(map[string]any)
		for _, a := range attacks {
			before := len(s.spy.Calls())
			r := s.call(t, c.op, s.full, sm.withValue(t, c.op, base, c.path, a.value))
			if r.status != http.StatusBadRequest {
				t.Errorf("%s %s %s: %d %.200s, want 400", c.op.ID, c.path, a.name, r.status, r.body)
			} else if p := r.problem(t); p.Slug() != problem.ValidationFailed {
				t.Errorf("%s %s %s: problem %s", c.op.ID, c.path, a.name, p.Slug())
			}
			if n := len(s.spy.Calls()); n != before {
				t.Errorf("%s %s %s: %d handler calls ran", c.op.ID, c.path, a.name, n-before)
			}
		}
	}

	// Step 3: every mutating nexusd op, the upload included, refuses the
	// read token with 403 naming nexusd:admin, and no handler runs.
	reader := s.exact[nexusd.ReadScope]
	var mutating []string
	for _, op := range s.ops() {
		if op.Provider != nexusd.Name || !op.Mutating() {
			continue
		}
		mutating = append(mutating, op.ID)
		base, _ := sm.value(t, op, "", inputSchema(t, op)).(map[string]any)
		r := s.call(t, op, reader, base)
		if r.status != http.StatusForbidden {
			t.Errorf("%s with %s: %d %.200s, want 403", op.ID, nexusd.ReadScope, r.status, r.body)
			continue
		}
		if p := r.problem(t); p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != nexusd.AdminScope {
			t.Errorf("%s: problem %s %v, want forbidden naming %s", op.ID, p.Slug(), p.Extensions, nexusd.AdminScope)
		}
	}
	want := []string{nexusd.Reload.Op().ID, nexusd.Restart.Op().ID, nexusd.Update.Op().ID,
		nexusd.Rollback.Op().ID, nexusd.Upload.Op().ID}
	slices.Sort(mutating)
	slices.Sort(want)
	if !slices.Equal(mutating, want) {
		t.Errorf("mutating nexusd ops %v, want %v", mutating, want)
	}
	if calls := s.spy.Calls(); len(calls) != 0 {
		t.Errorf("handlers ran without nexusd:admin: %+v", calls)
	}
}

// TC-11 (pihole-dns-zones): the pihole scopes split three ways. Reads need
// only pihole:read; the zone, record and batch mutations need pihole:dns and
// a pihole:write token is refused naming it, and the reverse for blocking,
// domains, adlists, gravity, restart and flush. Each accepted mutation has a
// requested record, with the validated input, written before its handler
// ran; an Idempotency-Key replays, and a reused key with another body is
// refused.
func TestPiholeScopesAuditAndIdempotency(t *testing.T) {
	s := newStack(t)
	sm := sampler{}
	read, dns, write := s.exact[pihole.ReadScope], s.exact[pihole.DNSScope], s.exact[pihole.WriteScope]
	if read == "" || dns == "" || write == "" {
		t.Fatalf("missing a pihole token: read %t, dns %t, write %t", read != "", dns != "", write != "")
	}

	checked := 0
	for _, op := range pihole.Spec().Ops {
		in, _ := sm.value(t, op, "", inputSchema(t, op)).(map[string]any)
		switch op.Scope.Name {
		case pihole.ReadScope:
			if r := s.call(t, op, read, in); r.status != http.StatusOK {
				t.Errorf("%s with only %s: %d %.200s, want 200", op.ID, pihole.ReadScope, r.status, r.body)
			}
		case pihole.DNSScope, pihole.WriteScope:
			own, other := dns, write
			if op.Scope.Name == pihole.WriteScope {
				own, other = write, dns
			}
			r := s.call(t, op, other, in)
			if r.status != http.StatusForbidden {
				t.Errorf("%s with the other mutation scope: %d %.200s, want 403", op.ID, r.status, r.body)
			} else if p := r.problem(t); p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != op.Scope.Name {
				t.Errorf("%s: problem %s requiredScope %v, want forbidden naming %s", op.ID, p.Slug(), p.Extensions["requiredScope"], op.Scope.Name)
			}
			s.assertAuditedBeforeRun(t, op, own, s.exactName[op.Scope.Name], in)
		default:
			t.Errorf("%s: scope %q is none of the three pihole scopes", op.ID, op.Scope.Name)
			continue
		}
		checked++
	}
	if checked != 26 {
		t.Errorf("checked %d pihole ops, want 26", checked)
	}

	// Idempotency: the same key and body replay the first operation; the
	// same key with another body answers 422 idempotency-key-reused.
	add := pihole.AddZone.Op()
	first := s.callKeyed(t, add, dns, "pihole-key-1", map[string]any{"zone": "a.b"})
	again := s.callKeyed(t, add, dns, "pihole-key-1", map[string]any{"zone": "a.b"})
	if first.status != http.StatusAccepted || again.status != http.StatusAccepted {
		t.Fatalf("keyed add-zone: %d then %d, want 202 twice: %s", first.status, again.status, again.body)
	}
	var o1, o2 apitypes.Operation
	if err := errors.Join(json.Unmarshal(first.body, &o1), json.Unmarshal(again.body, &o2)); err != nil {
		t.Fatal(err)
	}
	if o1.ID == "" || o1.ID != o2.ID {
		t.Errorf("replay answered operation %q, want the first, %q", o2.ID, o1.ID)
	}
	r := s.callKeyed(t, add, dns, "pihole-key-1", map[string]any{"zone": "c.d"})
	if r.status != http.StatusUnprocessableEntity {
		t.Fatalf("reused key, other body: %d %.200s, want 422", r.status, r.body)
	}
	if p := r.problem(t); p.Slug() != problem.IdempotencyKeyReused {
		t.Errorf("reused key: problem %s, want %s", p.Slug(), problem.IdempotencyKeyReused)
	}
}

// assertAuditedBeforeRun calls the long-running op with token and checks its
// requested record names the token, carries the validated input the handler
// received, and was written no later than the handler started.
func (s *stack) assertAuditedBeforeRun(t *testing.T, op def.Op, token, tokenName string, in map[string]any) {
	t.Helper()
	r := s.call(t, op, token, in)
	if r.status != http.StatusAccepted {
		t.Errorf("%s with exactly %s: %d %.200s, want 202", op.ID, op.Scope.Name, r.status, r.body)
		return
	}
	got := s.received(t, op, r, 0)
	var o apitypes.Operation
	if err := json.Unmarshal(r.body, &o); err != nil {
		t.Fatal(err)
	}
	o = s.waitTerminal(t, o.ID)
	page, err := s.audit.Query(audit.Query{Action: op.ID, TokenName: tokenName, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	// The operation's Queued record names the requested record it answers.
	requestID := ""
	for _, it := range page.Items {
		if it.OperationID == o.ID && it.RequestID != "" {
			requestID = it.RequestID
		}
	}
	var rec *systemspec.AuditRecord
	for i := range page.Items {
		if it := &page.Items[i]; requestID != "" && it.ID == requestID && it.Outcome == audit.OutcomeRequested {
			rec = it
		}
	}
	if rec == nil {
		t.Errorf("%s: no requested record for operation %s", op.ID, o.ID)
		return
	}
	if o.Started == nil || rec.Time.After(*o.Started) {
		t.Errorf("%s: requested record at %v, after the run started at %v", op.ID, rec.Time, o.Started)
	}
	var params any
	if err := json.Unmarshal(rec.Params, &params); err != nil {
		t.Fatalf("%s: requested record params %q: %v", op.ID, rec.Params, err)
	}
	if fmt.Sprint(params) != fmt.Sprint(got) {
		t.Errorf("%s: requested record params %v, the handler received %v", op.ID, params, got)
	}
}

// callKeyed is call for a POST op with an Idempotency-Key header.
func (s *stack) callKeyed(t *testing.T, op def.Op, token, key string, input map[string]any) response {
	t.Helper()
	method, path, err := openapi.Route(op)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, s.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, body: b}
}

// TC-13 (pihole-dns-zones): a record ID "<fqdn>~A" routes as one path
// segment and reaches the handler unchanged, on a get and on a resource
// action; "~AAAA" is no record ID.
func TestPiholeRecordIDRoutes(t *testing.T) {
	s := newStack(t)
	const id = "kiwi.home.local~A"
	tok := s.full

	before := len(s.spy.Calls())
	r := s.do(t, http.MethodGet, "/v1/providers/pihole/records/"+id, tok, nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET record %s: %d %.200s", id, r.status, r.body)
	}
	if got := s.received(t, pihole.RecordsGet.Op(), r, before); fmt.Sprint(got) != fmt.Sprint(map[string]any{"name": id}) {
		t.Errorf("get handler received %v, want name %q", got, id)
	}

	r = s.do(t, http.MethodPost, "/v1/providers/pihole/records/"+id+"/update", tok, []byte(`{"ipv4":"192.0.2.1"}`))
	if r.status != http.StatusAccepted {
		t.Fatalf("POST record %s update: %d %.200s", id, r.status, r.body)
	}
	got, _ := s.received(t, pihole.RecordsUpdate.Op(), r, 0).(map[string]any)
	if got["name"] != id {
		t.Errorf("update handler received name %v, want %q", got["name"], id)
	}

	before = len(s.spy.Calls())
	r = s.do(t, http.MethodGet, "/v1/providers/pihole/records/kiwi.home.local~AAAA", tok, nil)
	if r.status != http.StatusBadRequest {
		t.Errorf("GET record ~AAAA: %d %.200s, want 400", r.status, r.body)
	} else if p := r.problem(t); p.Slug() != problem.ValidationFailed {
		t.Errorf("~AAAA: problem %s, want %s", p.Slug(), problem.ValidationFailed)
	}
	if n := len(s.spy.Calls()); n != before {
		t.Errorf("~AAAA reached %d handler calls", n-before)
	}
}

// TC-10 (pihole-dns-zones): the served OpenAPI document lists exactly the 26
// pihole routes under /v1/providers/pihole, with the Refine patterns as
// schema patterns and the refused addresses as not.enum.
func TestPiholeOpenAPI(t *testing.T) {
	s := newStack(t)
	r := s.do(t, http.MethodGet, openapi.Path, s.none, nil)
	if r.status != http.StatusOK {
		t.Fatalf("openapi: %d %.200s", r.status, r.body)
	}
	type schema struct {
		Pattern    string             `json:"pattern"`
		Properties map[string]*schema `json:"properties"`
		Not        *struct {
			Enum []string `json:"enum"`
		} `json:"not"`
	}
	type operation struct {
		OperationID string `json:"operationId"`
		Parameters  []struct {
			Name, In string
			Schema   schema `json:"schema"`
		} `json:"parameters"`
		RequestBody *struct {
			Content map[string]struct {
				Schema schema `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
	}
	var doc struct {
		Paths map[string]map[string]operation `json:"paths"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatal(err)
	}

	served := map[string]operation{}
	for path, methods := range doc.Paths {
		if !strings.HasPrefix(path, "/v1/providers/pihole/") {
			continue
		}
		for method, o := range methods {
			served[strings.ToUpper(method)+" "+path] = o
		}
	}
	for _, op := range pihole.Spec().Ops {
		method, path, err := openapi.Route(op)
		if err != nil {
			t.Fatal(err)
		}
		if got := served[method+" "+path].OperationID; got != op.ID {
			t.Errorf("OpenAPI %s %s: operationId %q, want %q", method, path, got, op.ID)
		}
	}
	if len(served) != 26 {
		t.Errorf("OpenAPI lists %d routes under /v1/providers/pihole, want 26", len(served))
	}

	_, getPath, _ := openapi.Route(pihole.RecordsGet.Op())
	pattern := ""
	for _, p := range served[http.MethodGet+" "+getPath].Parameters {
		if p.In == "path" && p.Name == "name" {
			pattern = p.Schema.Pattern
		}
	}
	if pattern != pihole.RecordNamePattern {
		t.Errorf("records.get {name} pattern %q, want RecordNamePattern %q", pattern, pihole.RecordNamePattern)
	}

	_, addPath, _ := openapi.Route(pihole.AddRecord.Op())
	body := served[http.MethodPost+" "+addPath].RequestBody
	if body == nil {
		t.Fatal("add-record has no request body")
	}
	ipv4 := body.Content["application/json"].Schema.Properties["ipv4"]
	switch {
	case ipv4 == nil:
		t.Error("add-record body has no ipv4 property")
	case ipv4.Pattern != pihole.IPv4Pattern:
		t.Errorf("add-record ipv4 pattern %q, want IPv4Pattern", ipv4.Pattern)
	case ipv4.Not == nil || !slices.Equal(ipv4.Not.Enum, []string{"0.0.0.0", "255.255.255.255"}):
		t.Errorf("add-record ipv4 not = %+v, want enum 0.0.0.0 and 255.255.255.255", ipv4.Not)
	}
}
