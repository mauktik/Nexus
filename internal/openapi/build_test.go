// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package openapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt"
	aptspec "github.com/mauktik/nexus/internal/providers/apt/spec"
	"github.com/mauktik/nexus/internal/providers/docker"
	dockerspec "github.com/mauktik/nexus/internal/providers/docker/spec"
	"github.com/mauktik/nexus/internal/providers/scripts"
	scriptspec "github.com/mauktik/nexus/internal/providers/scripts/spec"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
)

// The test providers cover every Kind and Mode, every Scope form, a list op
// with filters, and an op with no input fields, so the document is checked
// against each way a route and its responses are derived.

type nameIn struct {
	Name string `json:"name" jsonschema:"Name of the thing."`
}

type restartIn struct {
	Name  string `json:"name"`
	Force bool   `json:"force,omitempty" jsonschema:"Restart even when the thing is busy."`
}

type listIn struct {
	Limit  int      `json:"limit,omitempty"`
	Filter []string `json:"filter,omitempty"`
}

type checkIn struct{}

type thing struct {
	ID      string    `json:"id"`
	Host    string    `json:"host"`
	Name    string    `json:"name"`
	Updated time.Time `json:"updated"`
}

type restartOut struct {
	apitypes.ActionResult
	Name string `json:"name"`
}

// uploadIn and uploadOut are a KindUpload op's input, read from the query,
// and its 201 body (TC-SM-2's shape).
type uploadIn struct {
	Signature string `json:"signature" jsonschema:"Detached signature of the body."`
}

type uploadOut struct {
	UploadID  string    `json:"uploadId"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"sizeBytes"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// tpUploadMaxBytes is the test upload op's ceiling.
const tpUploadMaxBytes = 1 << 20

func refineName(s *jsonschema.Schema) {
	if p := s.Properties["name"]; p != nil {
		p.Pattern = `^[a-z0-9-]+$`
	}
}

func refineList(s *jsonschema.Schema) {
	if p := s.Properties["filter"]; p != nil && p.Items != nil {
		p.Items.Pattern = `^(status|owner)=[^\x00-\x1f]*$`
	}
}

func tpOp(id string, kind def.Kind, rt, action string, risk def.Risk, mode def.Mode, scope def.Scope, desc string) def.Op {
	return def.Op{
		ID: id, Provider: "tp", Kind: kind, ResourceType: rt, Action: action,
		Summary: "Test " + id, Description: desc,
		Scope: scope, Risk: risk, Mode: mode, Refine: refineName,
		CLI: def.CLIBinding{Noun: "thing", Verb: "get"},
	}
}

var (
	tpList = func() def.Typed[listIn, apitypes.List[thing]] {
		op := tpOp("tp.things.list", def.KindList, "things", "", def.RiskReadOnly, def.ModeSync,
			def.Scope{Name: "tp:read"}, "Lists things. Read-only.")
		op.Filters = []def.Filter{{Key: "status", Description: "Thing status."}, {Key: "owner", Description: "Thing owner."}}
		op.Refine = refineList
		return def.Define[listIn, apitypes.List[thing]](op)
	}()
	tpGet = def.Define[nameIn, thing](tpOp("tp.things.get", def.KindGet, "things", "", def.RiskReadOnly, def.ModeSync,
		def.Scope{Name: "tp:read"}, "Returns one thing. Read-only."))
	tpLogs = def.Define[nameIn, apitypes.Frame](tpOp("tp.things.logs", def.KindSubresource, "things", "logs",
		def.RiskReadOnly, def.ModeStream, def.Scope{Name: "tp:read"}, "Streams a thing's output as NDJSON frames. Read-only."))
	// No declared scope: the provider's read scope applies.
	tpStatus = def.Define[nameIn, thing](tpOp("tp.things.status", def.KindSubresource, "things", "status",
		def.RiskReadOnly, def.ModeSync, def.Scope{}, "Returns a thing's status. Read-only."))
	tpRestart = def.Define[restartIn, restartOut](tpOp("tp.things.restart", def.KindResourceAction, "things", "restart",
		def.RiskMutating, def.ModeLongRunning, def.Scope{Name: "tp:write"},
		"Restarts a thing. Side effects: the thing stops and starts again. Long-running: answers 202 with an operation."))
	tpInvoke = def.Define[nameIn, restartOut](tpOp("tp.scripts.invoke", def.KindResourceAction, "scripts", "invoke",
		def.RiskDestructive, def.ModeLongRunning, def.Scope{Name: "tp:run", PerTarget: "tp:run:{name}"},
		"Runs a script. Side effects: whatever the script does. Long-running: answers 202 with an operation."))
	tpCheck = func() def.Typed[checkIn, restartOut] {
		op := tpOp("tp.actions.check", def.KindProviderAction, "", "check", def.RiskReadOnly, def.ModeLongRunning,
			def.Scope{Name: "tp:read"}, "Checks every thing. Read-only. Long-running: answers 202 with an operation.")
		op.Refine = nil
		return def.Define[checkIn, restartOut](op)
	}()
	// A KindUpload op has no CLI binding of its own.
	tpUpload = def.Define[uploadIn, uploadOut](def.Op{
		ID: "tp.uploads.add", Provider: "tp", Kind: def.KindUpload, ResourceType: "uploads",
		Summary: "Stage an upload", Description: "Stages the request body for a later operation and answers 201. " +
			"Side effects: the body is stored on the host.",
		Scope: def.Scope{Name: "tp:admin"}, Risk: def.RiskMutating, Mode: def.ModeSync,
		Upload: &def.Upload{ContentTypes: []string{"application/octet-stream", "application/gzip"}, MaxBytes: tpUploadMaxBytes},
		Refine: func(s *jsonschema.Schema) {
			if p := s.Properties["signature"]; p != nil {
				n := 4096
				p.MaxLength = &n
			}
		},
	})
	tqGet = def.Define[nameIn, thing](def.Op{
		ID: "tq.items.get", Provider: "tq", Kind: def.KindGet, ResourceType: "items",
		Summary: "Get an item", Description: "Returns one item. Read-only.",
		Scope: def.Scope{Name: "tq:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Refine: refineName,
		CLI: def.CLIBinding{Noun: "item", Verb: "get", Positional: "name"},
	})
)

type tp struct{}

func (tp) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "tp", ReadScope: "tp:read", Ops: []def.Op{
		tpList.Op(), tpGet.Op(), tpLogs.Op(), tpStatus.Op(), tpRestart.Op(), tpInvoke.Op(), tpCheck.Op(), tpUpload.Op(),
	}}
}
func (tp) Configure(func(any) error) error { return nil }
func (tp) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (tp) Bindings() []provider.Binding {
	get := func(_ context.Context, in nameIn) (thing, error) {
		return thing{ID: in.Name, Host: "testhost", Name: in.Name, Updated: time.Unix(0, 0).UTC()}, nil
	}
	run := provider.InProcessSpec[restartIn, restartOut]{Run: func(_ context.Context, in restartIn, _ *provider.Run) (restartOut, error) {
		return restartOut{ActionResult: apitypes.ActionResult{Changed: true}, Name: in.Name}, nil
	}}
	return []provider.Binding{
		provider.Sync(tpList, func(context.Context, listIn) (apitypes.List[thing], error) {
			return apitypes.List[thing]{Items: []thing{{ID: "a", Host: "testhost", Name: "a"}}}, nil
		}),
		provider.Sync(tpGet, get),
		provider.Stream(tpLogs, func(_ context.Context, in nameIn, emit func(apitypes.Frame) error) error {
			if err := emit(apitypes.DataFrame(0, time.Unix(0, 0).UTC(), apitypes.StreamStdout, in.Name)); err != nil {
				return err
			}
			return emit(apitypes.TruncatedFrame(3))
		}),
		provider.Sync(tpStatus, get),
		provider.InProcess(tpRestart, run),
		provider.InProcess(tpInvoke, provider.InProcessSpec[nameIn, restartOut]{
			Run: func(context.Context, nameIn, *provider.Run) (restartOut, error) { return restartOut{}, nil }}),
		provider.InProcess(tpCheck, provider.InProcessSpec[checkIn, restartOut]{
			Run: func(context.Context, checkIn, *provider.Run) (restartOut, error) { return restartOut{}, nil }}),
		// The server requires an upload binding for a KindUpload op.
		provider.Upload(tpUpload, provider.UploadSpec[uploadIn, uploadOut]{
			Receive: func(context.Context, uploadIn, provider.UploadBody) (uploadOut, error) { return uploadOut{}, nil }}),
	}
}

type tq struct{}

func (tq) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "tq", ReadScope: "tq:read", Ops: []def.Op{tqGet.Op()}}
}
func (tq) Configure(func(any) error) error { return nil }
func (tq) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (tq) Bindings() []provider.Binding {
	return []provider.Binding{provider.Sync(tqGet, func(_ context.Context, in nameIn) (thing, error) {
		return thing{ID: in.Name, Host: "testhost", Name: in.Name}, nil
	})}
}

// stack is a real server over the test providers and the system ops, with
// an operation manager so every system op is bound.
type stack struct {
	base      string
	srv       *server.Server
	reg       *core.Registry
	full      string // Holds every scope the test ops name
	scopeFree string
}

func enabledAll() map[string]core.ProviderConfig {
	return map[string]core.ProviderConfig{"tp": {Enabled: true}, "tq": {Enabled: true},
		scriptspec.Name: {Enabled: true, Decode: scriptsSection},
		dockerspec.Name: {Enabled: true, Decode: dockerSection},
		aptspec.Name:    {Enabled: true}}
}

// aptProvider is the real apt provider, with its preflight forced enabled
// because T0 hosts have no apt.
func aptProvider() provider.Provider {
	return apt.New(apt.Options{Host: "testhost", Preflight: func(context.Context) provider.Status {
		return provider.Status{State: apitypes.ProviderEnabled}
	}})
}

// dockerSection points the real docker provider at a socket that does not
// exist, so its calls answer dependency-unavailable.
func dockerSection(into any) error {
	into.(*config.DockerConfig).Socket = "/nonexistent/nexus-contract-test/docker.sock"
	return nil
}

// dockerProvider is the real docker provider, with its preflight forced
// enabled because T0 hosts have no Docker Engine.
func dockerProvider() provider.Provider {
	return docker.New(docker.Options{Host: "testhost", Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Preflight: func(context.Context) provider.Status { return provider.Status{State: apitypes.ProviderEnabled} }})
}

// scriptsSection is the real scripts provider's configuration in the stack:
// one cataloged script.
func scriptsSection(into any) error {
	into.(*config.ScriptsConfig).Catalog = []config.Script{{
		ID: "deploy", Description: "Deploy the API", Path: "/opt/deploy.sh", RunAs: "deploy",
		WorkingDirectory: "/opt", Env: map[string]string{"TOKEN": "secret"},
		Parameters: []config.Parameter{{Name: "branch", Type: "string", Pattern: "^[a-z]+$", Arg: "--branch"}},
	}}
	return nil
}

// scriptsProvider is the real scripts provider, with its preflight forced
// enabled because T0 hosts have no systemd.
func scriptsProvider() provider.Provider {
	return scripts.New(scripts.Options{Host: "testhost", Preflight: func(context.Context) provider.Status {
		return provider.Status{State: apitypes.ProviderEnabled}
	}})
}

func newStack(t *testing.T) *stack {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
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
	reg, err := core.New([]provider.Provider{tp{}, tq{}, scriptsProvider(), dockerProvider(), aptProvider()}, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: log, Audit: alog, Ops: mgr, Providers: enabledAll(),
	})
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "full",
		Scopes: []string{"tp:read", "tp:write", "tp:run", "tp:admin", "tq:read", "audit:read", "scripts:read", "scripts:run",
			dockerspec.ReadScope, aptspec.ReadScope, aptspec.WriteScope}})
	if err != nil {
		t.Fatal(err)
	}
	scopeFree, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "scope-free"})
	if err != nil {
		t.Fatal(err)
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
	return &stack{base: "http://" + ln.Addr().String(), srv: srv, reg: reg, full: full, scopeFree: scopeFree}
}

func (s *stack) do(t *testing.T, method, path, token string, body io.Reader, hdr ...string) (*http.Request, *http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, s.base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return req, resp, b
}

// fetch returns the served document's bytes.
func (s *stack) fetch(t *testing.T) []byte {
	t.Helper()
	_, resp, body := s.do(t, http.MethodGet, openapi.Path, s.full, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", openapi.Path, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q", ct)
	}
	return body
}

// doc is the parsed document, walked generically so the test does not share
// the builder's types.
type doc map[string]any

func parse(t *testing.T, b []byte) doc {
	t.Helper()
	var d doc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("document is not JSON: %v", err)
	}
	return d
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// docRoutes returns "METHOD path" -> operationId for every operation.
func docRoutes(d doc) map[string]string {
	out := map[string]string{}
	for path, item := range obj(d["paths"]) {
		for m, op := range obj(item) {
			if slices.Contains(methods, m) {
				id, _ := obj(op)["operationId"].(string)
				out[strings.ToUpper(m)+" "+path] = id
			}
		}
	}
	return out
}

func servedRoutes(srv *server.Server) map[string]string {
	out := map[string]string{}
	for _, r := range srv.Routes() {
		out[r.Method+" "+r.Path] = r.OpID
	}
	return out
}

// routeDiff names every entry that is in one set and not the other, or
// whose operation ID differs.
func routeDiff(document, served map[string]string) []string {
	var diff []string
	for k, id := range document {
		sid, ok := served[k]
		switch {
		case !ok:
			diff = append(diff, "in the document but not served: "+k)
		case sid != id:
			diff = append(diff, fmt.Sprintf("%s: document operationId %q, served op %q", k, id, sid))
		}
	}
	for k := range served {
		if _, ok := document[k]; !ok {
			diff = append(diff, "served but not in the document: "+k)
		}
	}
	slices.Sort(diff)
	return diff
}

// TC-78: the document's (method, path) set equals the routes rest.go
// recorded, in both directions; a planted difference either way is named.
func TestDocumentMatchesRoutes(t *testing.T) {
	s := newStack(t)
	document, served := docRoutes(parse(t, s.fetch(t))), servedRoutes(s.srv)
	if diff := routeDiff(document, served); len(diff) != 0 {
		t.Fatalf("document and routes differ:\n%s", strings.Join(diff, "\n"))
	}
	// Every Kind, Mode and system op is covered, so equality is not vacuous.
	for _, want := range []string{
		"GET /v1/capabilities", "GET /v1/operations", "GET /v1/operations/{id}", "GET /v1/operations/{id}/logs",
		"POST /v1/operations/{id}/cancel", "GET /v1/audit",
		"GET /v1/providers/tp/things", "GET /v1/providers/tp/things/{name}", "GET /v1/providers/tp/things/{name}/logs",
		"POST /v1/providers/tp/things/{name}/restart", "POST /v1/providers/tp/actions/check",
		"POST /v1/providers/tp/uploads",
		"GET /v1/providers/tq/items/{name}",
		"GET /v1/providers/scripts/scripts", "GET /v1/providers/scripts/scripts/{name}",
		"POST /v1/providers/scripts/scripts/{name}/invoke",
		"GET /v1/providers/docker/containers", "GET /v1/providers/docker/containers/{name}",
		"GET /v1/providers/docker/containers/{name}/logs",
		"GET /v1/providers/apt/packages", "GET /v1/providers/apt/packages/{name}", "GET /v1/providers/apt/upgradable",
		"POST /v1/providers/apt/actions/refresh", "POST /v1/providers/apt/actions/upgrade",
		"POST /v1/providers/apt/actions/install", "POST /v1/providers/apt/actions/remove",
	} {
		if _, ok := document[want]; !ok {
			t.Errorf("document lacks %s", want)
		}
	}

	planted := maps.Clone(served)
	planted["GET /v1/providers/tp/planted"] = "tp.planted.get"
	if diff := routeDiff(document, planted); !slices.Equal(diff, []string{"served but not in the document: GET /v1/providers/tp/planted"}) {
		t.Fatalf("planted route: diff %q", diff)
	}
	plantedDoc := maps.Clone(document)
	plantedDoc["POST /v1/providers/tp/ghost"] = "tp.ghost.run"
	if diff := routeDiff(plantedDoc, served); !slices.Equal(diff, []string{"in the document but not served: POST /v1/providers/tp/ghost"}) {
		t.Fatalf("planted document path: diff %q", diff)
	}
}

// validate runs the independent validator over b.
func validate(t *testing.T, b []byte) (libopenapi.Document, bool, string) {
	t.Helper()
	d, err := libopenapi.NewDocument(b)
	if err != nil {
		return nil, false, err.Error()
	}
	v, errs := validator.NewValidator(d)
	if len(errs) > 0 {
		return d, false, fmt.Sprint(errs)
	}
	ok, verrs := v.ValidateDocument()
	var msgs []string
	for _, e := range verrs {
		msgs = append(msgs, e.Error())
		for _, se := range e.SchemaValidationErrors {
			msgs = append(msgs, "  "+se.KeywordLocation+" at "+se.FieldPath+": "+se.Reason)
		}
	}
	return d, ok, strings.Join(msgs, "\n")
}

// TC-79: the document passes an independent OpenAPI 3.1 validator, which
// rejects a deliberately invalid control, and the validator is test-only.
func TestDocumentPassesIndependentValidator(t *testing.T) {
	s := newStack(t)
	body := s.fetch(t)
	d, ok, msg := validate(t, body)
	if !ok {
		t.Fatalf("served document is invalid:\n%s", msg)
	}
	if info := d.GetSpecInfo(); info.Version != openapi.Version || !strings.HasPrefix(info.Version, "3.1.") {
		t.Fatalf("validator read version %q, want %s", info.Version, openapi.Version)
	}
	if _, err := d.BuildV3Model(); err != nil {
		t.Fatalf("validator cannot build the model: %v", err)
	}

	// The gendocs path: every catalog op, with no server.
	gen, err := openapi.Build(catalog.Ops(), openapi.Options{Version: "dev", ReadScopes: readScopes()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, msg := validate(t, gen); !ok {
		t.Fatalf("catalog document is invalid:\n%s", msg)
	}

	// Controls: each breaks one MUST of OpenAPI 3.1.1.
	controls := map[string]func(doc){
		"no info": func(d doc) { delete(d, "info") },
		"response without description": func(d doc) {
			op := obj(obj(obj(d["paths"])["/v1/capabilities"])["get"])
			delete(obj(obj(op["responses"])["200"]), "description")
		},
		"path parameter not required": func(d doc) {
			op := obj(obj(obj(d["paths"])["/v1/operations/{id}"])["get"])
			obj(op["parameters"].([]any)[0])["required"] = false
		},
	}
	for name, mutate := range controls {
		bad := parse(t, body)
		mutate(bad)
		b, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := validate(t, b); ok {
			t.Errorf("control %q passed the validator: it does not discriminate", name)
		}
	}

	out, err := exec.Command("go", "list", "-deps", "github.com/mauktik/nexus/cmd/nexusd").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	if strings.Contains(string(out), "github.com/pb33f/") {
		t.Fatal("nexusd links libopenapi: the validator must stay test-only")
	}
}

func readScopes() map[string]string {
	m := map[string]string{}
	for _, s := range catalog.All() {
		m[s.Name] = s.ReadScope
	}
	return m
}

// expectedScope is x-required-scope as the Dispatcher enforces it.
func expectedScope(op def.Op, readScope string) ([]any, bool) {
	switch {
	case op.Scope.FromTarget:
		return []any{}, true
	case op.Scope.Name == "" && op.Scope.PerTarget == "":
		if op.Provider == def.SystemProvider {
			return []any{}, false
		}
		return []any{readScope}, false
	}
	var out []any
	for _, s := range []string{op.Scope.Name, op.Scope.PerTarget} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out, false
}

var refRE = regexp.MustCompile(`^#/components/schemas/(.+)$`)

// problemRefs returns the problem slugs a response's schema references.
func problemRefs(t *testing.T, where string, schema map[string]any) []problem.Slug {
	t.Helper()
	refs := []any{schema}
	if any, ok := schema["anyOf"].([]any); ok {
		refs = any
	}
	var slugs []problem.Slug
	for _, r := range refs {
		ref, _ := obj(r)["$ref"].(string)
		if ref == "#/components/schemas/Problem" {
			continue // The generic problem every status allows besides its listed types
		}
		m := refRE.FindStringSubmatch(ref)
		if m == nil {
			t.Fatalf("%s: problem schema %v is not a component reference", where, r)
		}
		var found bool
		for _, pt := range problem.All() {
			if openapi.ProblemSchemaName(pt.Slug) == m[1] {
				slugs, found = append(slugs, pt.Slug), true
			}
		}
		if !found {
			t.Fatalf("%s: reference %s names no registered problem type", where, ref)
		}
	}
	return slugs
}

// TC-80: every operation is fully described.
func TestEveryOperationIsFullyDescribed(t *testing.T) {
	s := newStack(t)
	d := parse(t, s.fetch(t))
	byID := map[string]def.Op{}
	for _, e := range s.reg.Entries() {
		byID[e.Binding.Op().ID] = e.Binding.Op()
	}
	scopes := map[string]string{"tp": "tp:read", "tq": "tq:read"}
	components := obj(obj(d["components"])["schemas"])

	if sec, _ := d["security"].([]any); len(sec) != 1 || obj(sec[0])["bearerAuth"] == nil {
		t.Fatalf("top-level security = %v, want bearerAuth", d["security"])
	}
	scheme := obj(obj(obj(d["components"])["securitySchemes"])["bearerAuth"])
	if scheme["type"] != "http" || scheme["scheme"] != "bearer" {
		t.Fatalf("bearerAuth scheme = %v", scheme)
	}
	if d["openapi"] != openapi.Version {
		t.Fatalf("openapi = %v", d["openapi"])
	}

	seen := map[string]bool{}
	for path, item := range obj(d["paths"]) {
		if !strings.HasPrefix(path, "/v1/") {
			t.Errorf("path %s is not under /v1/", path)
		}
		for method, raw := range obj(item) {
			o := obj(raw)
			where := strings.ToUpper(method) + " " + path
			id, _ := o["operationId"].(string)
			op, ok := byID[id]
			if !ok {
				t.Errorf("%s: operationId %q is no bound op", where, id)
				continue
			}
			if seen[id] {
				t.Errorf("operationId %q is not unique", id)
			}
			seen[id] = true
			if _, ok := o["security"]; ok {
				t.Errorf("%s overrides the top-level bearerAuth security", where)
			}

			desc, _ := o["description"].(string)
			switch {
			case strings.TrimSpace(desc) == "":
				t.Errorf("%s: empty description", where)
			case desc != op.Description:
				t.Errorf("%s: description %q, want the op's", where, desc)
			}
			if op.Mutating() && !strings.Contains(desc, "Side effects:") {
				t.Errorf("%s: mutating op does not state its side effects (\"Side effects:\")", where)
			}
			if op.Mode == def.ModeLongRunning && !strings.Contains(strings.ToLower(desc), "long-running") {
				t.Errorf("%s: long-running op does not say so", where)
			}

			if o["x-risk"] != string(op.Risk) {
				t.Errorf("%s: x-risk %v, want %s", where, o["x-risk"], op.Risk)
			}
			if o["x-long-running"] != (op.Mode == def.ModeLongRunning) {
				t.Errorf("%s: x-long-running %v", where, o["x-long-running"])
			}
			rs := obj(o["x-required-scope"])
			want, fromTarget := expectedScope(op, scopes[op.Provider])
			if got, _ := rs["scopes"].([]any); !slices.Equal(got, want) {
				t.Errorf("%s: x-required-scope.scopes %v, want %v", where, got, want)
			}
			if ft, _ := rs["fromTarget"].(bool); ft != fromTarget {
				t.Errorf("%s: x-required-scope.fromTarget %v, want %v", where, ft, fromTarget)
			}
			if rule, _ := rs["rule"].(string); rule == "" {
				t.Errorf("%s: x-required-scope has no rule", where)
			}

			// Every path template has a required path parameter.
			params := map[string]map[string]any{}
			for _, p := range o["parameters"].([]any) {
				params[obj(p)["in"].(string)+":"+obj(p)["name"].(string)] = obj(p)
			}
			for _, w := range openapi.Wildcards(path) {
				if p := params["path:"+w]; p == nil || p["required"] != true {
					t.Errorf("%s: no required path parameter %s", where, w)
				}
			}
			if len(op.Filters) > 0 {
				fd, _ := params["query:filter"]["description"].(string)
				for _, f := range op.Filters {
					if !strings.Contains(fd, f.Key) {
						t.Errorf("%s: filter parameter does not document key %s", where, f.Key)
					}
				}
			}

			responses := obj(o["responses"])
			switch {
			case op.Kind == def.KindUpload:
				if obj(obj(obj(responses["201"])["content"])["application/json"]) == nil || responses["200"] != nil {
					t.Errorf("%s: upload op lacks a 201 application/json response, or declares 200", where)
				}
			case op.Mode == def.ModeLongRunning:
				r := obj(responses["202"])
				hs := obj(r["headers"])
				if r == nil || hs["Location"] == nil || hs["Retry-After"] == nil {
					t.Errorf("%s: long-running op lacks 202 with Location and Retry-After: %v", where, r)
				}
				// Only an op that may complete at admission answers 200 too.
				if (responses["200"] != nil) != op.CompletesAtAdmission {
					t.Errorf("%s: long-running op declares 200 = %v, CompletesAtAdmission = %v",
						where, responses["200"] != nil, op.CompletesAtAdmission)
				}
			case op.Mode == def.ModeStream:
				ct := obj(obj(responses["200"])["content"])
				nd := obj(ct["application/x-ndjson"])
				if nd == nil || obj(nd["schema"])["$ref"] != "#/components/schemas/Frame" {
					t.Errorf("%s: stream op lacks application/x-ndjson with the Frame schema: %v", where, ct)
				}
			default:
				if obj(obj(obj(responses["200"])["content"])["application/json"]) == nil {
					t.Errorf("%s: sync op lacks a 200 application/json response", where)
				}
			}
			// Idempotency-Key is for long-running mutating ops only: a
			// SyncMutation or any other sync op refuses it.
			keyed := op.Mode == def.ModeLongRunning && op.Mutating()
			if (params["header:Idempotency-Key"] != nil) != keyed {
				t.Errorf("%s: Idempotency-Key header parameter = %v, want %v",
					where, params["header:Idempotency-Key"] != nil, keyed)
			}
			if cc := obj(obj(obj(responses["200"])["headers"])["Cache-Control"]); (cc != nil) != (op.SecretOutput != "") {
				t.Errorf("%s: 200 Cache-Control header = %v, SecretOutput = %q", where, cc, op.SecretOutput)
			}

			declared := map[problem.Slug]bool{}
			for code, raw := range responses {
				if code == "default" || code[0] == '2' {
					continue
				}
				schema := obj(obj(obj(obj(raw)["content"])["application/problem+json"])["schema"])
				for _, slug := range problemRefs(t, where+" "+code, schema) {
					pt, _ := problem.Lookup(slug)
					if fmt.Sprint(pt.Status) != code {
						t.Errorf("%s: %s is under %s, but its status is %d", where, slug, code, pt.Status)
					}
					if components[openapi.ProblemSchemaName(slug)] == nil {
						t.Errorf("%s: %s has no component schema", where, slug)
					}
					declared[slug] = true
				}
			}
			need := []problem.Slug{problem.Unauthorized, problem.ValidationFailed, problem.RouteNotFound,
				problem.MethodNotAllowed, problem.PayloadTooLarge, problem.RateLimited, problem.InternalError}
			if fromTarget || len(want) > 0 {
				need = append(need, problem.Forbidden)
			}
			if len(openapi.Wildcards(path)) > 0 {
				need = append(need, problem.NotFound)
			}
			if op.Provider != def.SystemProvider {
				need = append(need, problem.CapabilityUnavailable)
			}
			if op.Mutating() {
				need = append(need, problem.AuditUnavailable)
			}
			if op.OperationControl {
				need = append(need, problem.NotCancelable)
			}
			if op.Kind == def.KindUpload {
				need = append(need, problem.UploadTooLarge, problem.Conflict, problem.StorageLimitReached)
			}
			if op.Mode == def.ModeLongRunning || op.SyncMutation {
				need = append(need, problem.Conflict)
			}
			if op.Mode == def.ModeLongRunning && op.Mutating() {
				need = append(need, problem.IdempotencyKeyReused, problem.StorageLimitReached)
			}
			for _, slug := range need {
				if !declared[slug] {
					t.Errorf("%s: no %s response", where, slug)
				}
			}
			if responses["default"] == nil {
				t.Errorf("%s: no default problem response", where)
			}
		}
	}
	for id := range byID {
		if !seen[id] {
			t.Errorf("op %s is bound but not in the document", id)
		}
	}
	for _, pt := range problem.All() {
		if components[openapi.ProblemSchemaName(pt.Slug)] == nil {
			t.Errorf("problem type %s has no component schema", pt.Slug)
		}
	}
}

// CR-24: apt-failed declares the outcome members a failed apt run carries,
// each with its wire type, and exitCode there is apt-get's, not a script's.
func TestAptFailedSchemaDeclaresTheOutcome(t *testing.T) {
	s := newStack(t)
	d := parse(t, s.fetch(t))
	schema := obj(obj(obj(d["components"])["schemas"])[openapi.ProblemSchemaName(problem.AptFailed)])
	props := obj(schema["properties"])
	for member, typ := range map[string]string{"changes": "array", "conffilesKept": "array", "conffilesComplete": "boolean",
		"rebootRequired": "string", "rebootRequiredPackages": "array", "exitCode": "integer"} {
		if got := obj(props[member])["type"]; got != typ {
			t.Errorf("apt-failed %s: type %v, want %s", member, got, typ)
		}
	}
	if desc, _ := obj(props["exitCode"])["description"].(string); !strings.Contains(desc, "apt-get") {
		t.Errorf("apt-failed exitCode description %q does not name apt-get", desc)
	}
	script := obj(obj(obj(d["components"])["schemas"])[openapi.ProblemSchemaName(problem.ScriptFailed)])
	if desc, _ := obj(obj(script["properties"])["exitCode"])["description"].(string); !strings.Contains(desc, "script") {
		t.Errorf("script-failed exitCode description %q", desc)
	}
}

// nginx-provider carry-forward (WI 4.3): nginx-reload-failed's observed and
// rollbackReload are reload observations, not strings, and rolledBack of
// the nginx types speaks of the include directory, not a container.
func TestNginxProblemSchemasDeclareTheirMembers(t *testing.T) {
	s := newStack(t)
	d := parse(t, s.fetch(t))
	schemas := obj(obj(d["components"])["schemas"])
	reload := obj(obj(schemas[openapi.ProblemSchemaName(problem.NginxReloadFailed)])["properties"])
	for _, member := range []string{"observed", "rollbackReload"} {
		m := obj(reload[member])
		if m["type"] != "object" {
			t.Errorf("nginx-reload-failed %s: type %v, want object", member, m["type"])
			continue
		}
		props := obj(m["properties"])
		for key, typ := range map[string]string{"masterPid": "integer", "oldWorkers": "array", "newWorkers": "array",
			"initExitCode": "integer", "verdict": "string"} {
			if got := obj(props[key])["type"]; got != typ {
				t.Errorf("nginx-reload-failed %s.%s: type %v, want %s", member, key, got, typ)
			}
		}
	}
	for _, slug := range []problem.Slug{problem.NginxReloadFailed, problem.NginxConfigInvalid} {
		props := obj(obj(schemas[openapi.ProblemSchemaName(slug)])["properties"])
		rb := obj(props["rolledBack"])
		if desc, _ := rb["description"].(string); rb["type"] != "boolean" || strings.Contains(desc, "container") {
			t.Errorf("%s rolledBack = %v", slug, rb)
		}
		if to := obj(props["testOutput"]); to["type"] != "string" || !strings.Contains(fmt.Sprint(to["description"]), "nginx -t") {
			t.Errorf("%s testOutput = %v", slug, to)
		}
	}
	dep := obj(obj(obj(schemas[openapi.ProblemSchemaName(problem.DependencyUnavailable)])["properties"])["rolledBack"])
	if desc, _ := dep["description"].(string); !strings.Contains(desc, "container") {
		t.Errorf("dependency-unavailable rolledBack description %q changed", desc)
	}
}

// The schemas describe the wire: real responses of every shape validate
// against the served document, by the independent validator.
func TestResponsesMatchTheDocument(t *testing.T) {
	s := newStack(t)
	ld, err := libopenapi.NewDocument(s.fetch(t))
	if err != nil {
		t.Fatal(err)
	}
	v, errs := validator.NewValidator(ld)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	check := func(method, path, token, body string, wantStatus int, hdr ...string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, resp, b := s.do(t, method, path, token, rd, hdr...)
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		if body != "" {
			req.Body = io.NopCloser(strings.NewReader(body))
		}
		resp.Body = io.NopCloser(bytes.NewReader(b))
		if ok, verrs := v.ValidateHttpResponse(req, resp); !ok {
			var msgs []string
			for _, e := range verrs {
				msgs = append(msgs, e.Error())
				for _, se := range e.SchemaValidationErrors {
					msgs = append(msgs, "  "+se.KeywordLocation+" at "+se.FieldPath+": "+se.Reason)
				}
			}
			t.Errorf("%s %s (%d) does not match the document:\n%s\n%s", method, path, resp.StatusCode,
				strings.Join(msgs, "\n"), b)
		}
	}
	check(http.MethodGet, "/v1/capabilities", s.scopeFree, "", 200)
	check(http.MethodGet, "/v1/providers/tp/things?filter=status=up", s.full, "", 200)
	check(http.MethodGet, "/v1/providers/tp/things/a", s.full, "", 200)
	check(http.MethodGet, "/v1/providers/tp/things/a/status", s.full, "", 200)
	check(http.MethodPost, "/v1/providers/tp/things/a/restart", s.full, `{"force":true}`, 202)
	check(http.MethodGet, "/v1/providers/scripts/scripts", s.full, "", 200)
	check(http.MethodGet, "/v1/providers/scripts/scripts/deploy", s.full, "", 200)
	check(http.MethodGet, "/v1/providers/scripts/scripts/nope", s.full, "", 404)
	check(http.MethodPost, "/v1/providers/scripts/scripts/deploy/invoke", s.full, `{}`, 422)
	// No Docker Engine at the configured socket: dependency-unavailable.
	check(http.MethodGet, "/v1/providers/docker/containers", s.full, "", 503)
	check(http.MethodGet, "/v1/providers/docker/containers/web", s.full, "", 503)
	check(http.MethodGet, "/v1/providers/docker/containers/web/logs?tail=5", s.full, "", 503)
	check(http.MethodGet, "/v1/providers/docker/containers/..%2Fx", s.full, "", 400)
	check(http.MethodGet, "/v1/operations", s.full, "", 200)
	check(http.MethodGet, "/v1/audit", s.full, "", 200)
	check(http.MethodGet, "/v1/providers/tp/things/a", s.scopeFree, "", 403)
	check(http.MethodGet, "/v1/providers/tp/things?bogus=1", s.full, "", 400)
	check(http.MethodGet, "/v1/operations/0190a5c4-1f2e-7abc-8def-0123456789ab", s.full, "", 404)
	check(http.MethodGet, "/v1/providers/tp/things/a", "", "", 401)
	// The documented Idempotency-Key bound is the one enforced.
	check(http.MethodPost, "/v1/providers/tp/things/a/restart", s.full, `{}`, 400,
		"Idempotency-Key", strings.Repeat("k", openapi.IdempotencyKeyMaxLength+1))

	_, resp, b := s.do(t, http.MethodPost, "/v1/providers/tp/things/a/restart", s.full, strings.NewReader(`{}`),
		"Idempotency-Key", strings.Repeat("k", openapi.IdempotencyKeyMaxLength))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("Idempotency-Key at the documented maximum: %d %s", resp.StatusCode, b)
	}
}

// TC-81 (in process): the document is byte-identical across fetches, and
// a provider disabled by a configuration change leaves it, while its routes
// still answer capability-unavailable. TestOpenAPITracksSIGHUPReload in
// internal/daemon covers the same through a real SIGHUP.
func TestDocumentIsDeterministicAndTracksEnablement(t *testing.T) {
	s := newStack(t)
	first, second := s.fetch(t), s.fetch(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two fetches differ")
	}
	again, err := openapi.Build(entriesOps(s.reg), openapi.Options{Version: "v0.0.0-test",
		ReadScopes: map[string]string{"tp": "tp:read", "tq": "tq:read", scriptspec.Name: scriptspec.ReadScope,
			dockerspec.Name: dockerspec.ReadScope, aptspec.Name: aptspec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Fatal("a fresh build differs from the served document")
	}
	if _, ok := docRoutes(parse(t, first))["GET /v1/providers/tq/items/{name}"]; !ok {
		t.Fatal("tq is enabled but not documented")
	}

	cfgs := enabledAll()
	cfgs["tq"] = core.ProviderConfig{Enabled: false}
	if err := s.reg.Apply(context.Background(), cfgs); err != nil {
		t.Fatal(err)
	}
	after := docRoutes(parse(t, s.fetch(t)))
	for k := range after {
		if strings.Contains(k, "/providers/tq/") {
			t.Errorf("disabled provider still documented: %s", k)
		}
	}
	if _, ok := after["GET /v1/providers/tp/things/{name}"]; !ok {
		t.Error("enabled provider tp lost from the document")
	}
	_, resp, b := s.do(t, http.MethodGet, "/v1/providers/tq/items/x", s.full, nil)
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil || resp.StatusCode != 503 || p.Slug() != problem.CapabilityUnavailable {
		t.Fatalf("disabled provider route: %d %s", resp.StatusCode, b)
	}

	if err := s.reg.Apply(context.Background(), enabledAll()); err != nil {
		t.Fatal(err)
	}
	if back := s.fetch(t); !bytes.Equal(back, first) {
		t.Fatal("re-enabling tq does not restore the original document")
	}
}

func entriesOps(reg *core.Registry) []def.Op {
	var out []def.Op
	for _, e := range reg.Entries() {
		out = append(out, e.Binding.Op())
	}
	return out
}

// TC-ERR22: the document needs a valid token, and no scope.
func TestOpenAPIRequiresAToken(t *testing.T) {
	s := newStack(t)
	_, resp, b := s.do(t, http.MethodGet, openapi.Path, "", nil)
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil || resp.StatusCode != 401 || p.Slug() != problem.Unauthorized {
		t.Fatalf("no token: %d %s", resp.StatusCode, b)
	}
	_, resp, b = s.do(t, http.MethodGet, openapi.Path, s.scopeFree, nil)
	if resp.StatusCode != http.StatusOK || !json.Valid(b) {
		t.Fatalf("scope-free token: %d %.200s", resp.StatusCode, b)
	}
}

// Build is deterministic regardless of the order ops are given in, and
// refuses two ops on one route.
func TestBuildOrderAndConflicts(t *testing.T) {
	ops := catalog.Ops()
	a, err := openapi.Build(ops, openapi.Options{Version: "x"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(ops)
	b, err := openapi.Build(ops, openapi.Options{Version: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("op order changes the document")
	}
	if _, err := openapi.Build([]def.Op{tpGet.Op(), tpGet.Op()}, openapi.Options{}); err == nil {
		t.Fatal("two ops on one route were accepted")
	}
}

// Every declared op, including providers this test does not run, says what
// FR-26 asks of its description.
func TestCatalogDescriptionsStateEffects(t *testing.T) {
	for _, op := range catalog.Ops() {
		if strings.TrimSpace(op.Description) == "" {
			t.Errorf("%s: empty description", op.ID)
		}
		if op.Mutating() && !strings.Contains(op.Description, "Side effects:") {
			t.Errorf("%s: mutating op does not state its side effects (\"Side effects:\")", op.ID)
		}
		if op.Mode == def.ModeLongRunning && !strings.Contains(strings.ToLower(op.Description), "long-running") {
			t.Errorf("%s: long-running op does not say so", op.ID)
		}
	}
}
