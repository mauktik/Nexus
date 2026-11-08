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
	scripts "github.com/mauktik/nexus/internal/providers/scripts/spec"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
)

const module = "github.com/mauktik/nexus"

// catalogStubs binds stub handlers for each catalog provider other than
// system, whose ops core binds itself. A provider added to catalog.All()
// adds one entry here, built with testprovider.NewStub and the spy helpers
// (testprovider.Sync, Stream, InProcess), one binding per op.
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

	srv, err := server.New(reg, server.Options{Logger: log, Auth: auth.New(stateDir, auth.Options{Logger: log})})
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
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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

// call sends input to op over its REST route: route wildcards go in the
// path, the rest in the query for GET and in the JSON body for POST.
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
	if method == http.MethodGet {
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
// OpenAPI document and the Dispatcher's scope and audit rules through
// core.New alone; core and the transport import nothing from this suite.
// The MCP tool list joins this test at 8.1.
func TestTestProviderNeedsNoCoreOrTransportChange(t *testing.T) {
	// Setup: core and transport do not depend on the conformance packages.
	for _, pkg := range []string{"./internal/core", "./internal/server"} {
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
// (OOS-2, ASM-2), schedules (OOS-5), metrics (OOS-4), self-update (OOS-11)
// and tokens (no network path manages tokens).
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
