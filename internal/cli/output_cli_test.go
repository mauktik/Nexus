package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/problem"
)

const (
	fixtureHost = "fixture-host"
	// urlTargetHost is what a NEXUS_HOST URL target is called client-side
	// (CR-32): the host of its URL, whatever the server calls itself.
	urlTargetHost = "127.0.0.1"
	fixtureOpID   = "0190a5d2-7c3e-7f00-8a00-000000000001"
)

var fixtureTime = time.Date(2026, 11, 7, 12, 0, 0, 5, time.UTC)

// fixtureFor is a real-shape answer to op, built from its apitypes output
// type: a 202 operation for a long-running op, NDJSON frames for a stream,
// and otherwise the output type with its identity fields set.
func fixtureFor(t *testing.T, op def.Op) (status int, contentType string, body []byte) {
	t.Helper()
	marshal := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	switch {
	case op.Mode == def.ModeLongRunning:
		return http.StatusAccepted, "application/json", marshal(apitypes.Operation{ID: fixtureOpID, Host: fixtureHost,
			Action: op.ID, Provider: op.Provider, Status: apitypes.StatusQueued, Created: fixtureTime})
	case op.Mode == def.ModeStream:
		var lines []byte
		frames := []apitypes.Frame{apitypes.TruncatedFrame(7), apitypes.DataFrame(7, fixtureTime, apitypes.StreamStdout, "one\n")}
		if op.CLI.Positional == "name" { // Container logs carry no offset
			frames = []apitypes.Frame{{T: fixtureTime, S: apitypes.StreamStderr, D: "two\n"}}
		}
		for _, f := range frames {
			lines = append(append(lines, marshal(f)...), '\n')
		}
		return http.StatusOK, "application/x-ndjson", lines
	}
	out := op.OutputType()
	if out == reflect.TypeFor[apitypes.Capabilities]() {
		return http.StatusOK, "application/json", marshal(apitypes.NewCapabilities(fixtureHost, "1.0.0",
			apitypes.HostOS{ID: "ubuntu", VersionID: "24.04"}, "systemd", []apitypes.ProviderCapability{{Name: "docker", Status: apitypes.ProviderEnabled}}))
	}
	v := reflect.New(out).Elem()
	if items := v.FieldByName("Items"); items.IsValid() && items.Kind() == reflect.Slice {
		items.Set(reflect.MakeSlice(items.Type(), 1, 1))
		setIdentity(items.Index(0))
	} else {
		setIdentity(v)
	}
	return http.StatusOK, "application/json", marshal(v.Interface())
}

func setIdentity(v reflect.Value) {
	for name, val := range map[string]string{"ID": "/fixture/" + v.Type().Name(), "Host": fixtureHost, "Name": "x"} {
		if f := v.FieldByName(name); f.IsValid() && f.Kind() == reflect.String {
			f.SetString(val)
		}
	}
}

// startFixtureServer answers every route of specs with fixtureFor.
func startFixtureServer(t *testing.T, specs []def.ProviderSpec) *captureServer {
	t.Helper()
	mux := http.NewServeMux()
	for _, spec := range specs {
		for _, op := range spec.Ops {
			method, path, err := openapi.Route(op)
			if err != nil {
				t.Fatal(err)
			}
			status, ct, body := fixtureFor(t, op)
			mux.HandleFunc(method+" "+path, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ct)
				w.WriteHeader(status)
				_, _ = w.Write(body)
			})
		}
	}
	cs := &captureServer{}
	cs.Server = httptest.NewServer(mux)
	t.Cleanup(cs.Close)
	return cs
}

// cannedServer answers every request with status and body, and records it.
func cannedServer(t *testing.T, status int, body string) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		cs.reqs = append(cs.reqs, capturedRequest{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), nil})
		cs.mu.Unlock()
		ct := "application/json"
		if status >= 400 {
			ct = problem.ContentType
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(cs.Close)
	return cs
}

func jsonLines(t *testing.T, what, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("%s: line %q is not a JSON object: %v", what, l, err)
		}
		out = append(out, m)
	}
	return out
}

// argsFor is a valid invocation of each op of a generated command.
func argsFor(c *opCommand, op def.Op) []string {
	args := []string{c.noun, c.verb}
	switch op.CLI.Positional {
	case "":
	case "id":
		args = append(args, fixtureOpID)
	default:
		args = append(args, "web")
	}
	b, _ := newBinding(c)
	if b != nil && b.variadic && b.required {
		args = append(args, "curl")
	}
	// A required string input bound to a flag gets a value.
	if r, err := op.InputSchema(); b != nil && err == nil {
		for _, f := range b.fields {
			if f.kind == kindString && b.opFields[op.ID][f.name] && slices.Contains(r.Schema().Required, f.name) {
				args = append(args, "--"+f.flag, "x")
			}
		}
	}
	// The fixture server answers with an operation as accepted: print it
	// without waiting (FR-33), and confirm a destructive op.
	if op.Mode == def.ModeLongRunning {
		args = append(args, "--no-wait")
	}
	if op.Mutating() {
		args = append(args, "--yes")
	}
	return args
}

// TC-166: every object every data command prints has a host and an id, log
// frames included, with the locator ids of the Interfaces section.
func TestEveryObjectHasHostAndID(t *testing.T) {
	specs := catalog.All()
	srv := startFixtureServer(t, specs)
	env := urlEnv(srv)
	groups, err := groupOps(specs)
	if err != nil {
		t.Fatal(err)
	}
	ran := map[string]bool{}
	for _, g := range groups {
		for _, op := range g.ops {
			args := append(argsFor(g, op), "-o", "jsonl")
			r := runSpecsCLI(env, specs, args...)
			if r.code != 0 {
				t.Errorf("nexus %v exited %d: %s", args, r.code, r.stderr)
				continue
			}
			objs := jsonLines(t, strings.Join(args, " "), r.stdout)
			if len(objs) == 0 {
				t.Errorf("nexus %v printed nothing", args)
			}
			for _, o := range objs {
				if h, _ := o["host"].(string); h == "" {
					t.Errorf("nexus %v: object without host: %v", args, o)
				}
				if id, _ := o["id"].(string); id == "" {
					t.Errorf("nexus %v: object without id: %v", args, o)
				}
			}
			ran[op.ID] = true
			switch op.ID {
			case "system.capabilities.get":
				if objs[0]["id"] != apitypes.CapabilitiesID || objs[0]["host"] != urlTargetHost {
					t.Errorf("capability get: %v", objs[0])
				}
			case "system.operations.logs":
				want := []string{fixtureOpID + "@truncated", fixtureOpID + "@7"}
				if len(objs) != 2 || objs[0]["id"] != want[0] || objs[1]["id"] != want[1] || objs[1]["d"] != "one\n" || objs[0]["lostBytes"] != float64(7) {
					t.Errorf("operation log frames: %v, want ids %v next to the wire fields", objs, want)
				}
			case "docker.containers.logs":
				if want := "web@" + fixtureTime.Format(time.RFC3339Nano); len(objs) != 1 || objs[0]["id"] != want || objs[0]["s"] != "err" {
					t.Errorf("container log frames: %v, want id %s", objs, want)
				}
			}
			// Frames are labeled with the host the CLI targeted.
			if op.Mode == def.ModeStream && objs[0]["host"] != "127.0.0.1" {
				t.Errorf("%s: frame host %v, want the target 127.0.0.1", op.ID, objs[0]["host"])
			}
		}
	}
	for _, spec := range specs {
		for _, op := range spec.Ops {
			if !ran[op.ID] {
				t.Errorf("op %s was not run", op.ID)
			}
		}
	}

	// An object the host sent without its host is labeled with the target.
	bare := cannedServer(t, http.StatusOK, `{"id":"/capabilities","apiVersion":"v1","version":"x","providers":[]}`)
	r := runSpecsCLI(urlEnv(bare), specs, "capability", "get")
	if objs := jsonLines(t, "bare", r.stdout); r.code != 0 || len(objs) != 1 || objs[0]["host"] != "127.0.0.1" {
		t.Errorf("bare capabilities: exit %d %q", r.code, r.stdout)
	}
}

// TC-164: every format parses; YAML round-trips to JSON's data; piped
// output defaults to jsonl and holds no escape byte.
func TestOutputFormats(t *testing.T) {
	specs := catalog.All()
	srv := startFixtureServer(t, specs)
	env := urlEnv(srv)
	run := func(args ...string) runResult {
		t.Helper()
		r := runSpecsCLI(env, specs, args...)
		if r.code != 0 {
			t.Fatalf("nexus %v exited %d: %s", args, r.code, r.stderr)
		}
		if strings.ContainsRune(r.stdout, 0x1b) || strings.ContainsRune(r.stderr, 0x1b) {
			t.Errorf("nexus %v: piped output holds an escape byte: %q", args, r.stdout)
		}
		return r
	}
	for _, args := range [][]string{{"capability", "get"}, {"container", "get"}, {"operationlog", "get", fixtureOpID}} {
		jl := jsonLines(t, "jsonl", run(append(args, "-o", "jsonl")...).stdout)
		var asAny []any
		for _, o := range jl {
			asAny = append(asAny, any(o))
		}
		var js []any
		if err := json.Unmarshal([]byte(run(append(args, "-o", "json")...).stdout), &js); err != nil {
			t.Fatalf("%v -o json: %v", args, err)
		}
		if !reflect.DeepEqual(js, asAny) {
			t.Errorf("%v: json %v != jsonl %v", args, js, asAny)
		}
		var y any
		if err := yaml.Unmarshal([]byte(run(append(args, "-o", "yaml")...).stdout), &y); err != nil {
			t.Fatalf("%v -o yaml: %v", args, err)
		}
		if b, _ := json.Marshal(y); !reflect.DeepEqual(viaJSON(t, b), js) {
			t.Errorf("%v: yaml %v != json %v", args, y, js)
		}
		if def := run(args...).stdout; def != run(append(args, "-o", "jsonl")...).stdout {
			t.Errorf("%v: piped default %q is not jsonl", args, def)
		}
	}
	tbl := run("capability", "get", "-o", "table").stdout
	if !strings.HasPrefix(tbl, "HOST ") || !strings.Contains(tbl, urlTargetHost) || json.Valid([]byte(tbl)) {
		t.Errorf("table = %q", tbl)
	}
	if r := runSpecsCLI(env, specs, "capability", "get", "-o", "xml"); r.code != ExitUsage || r.stdout != "" {
		t.Errorf("-o xml: exit %d, stdout %q", r.code, r.stdout)
	}
}

func viaJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TC-165: results only on stdout; warnings and progress on stderr; errors
// on stderr are JSON with type and host in machine modes, text in a table.
func TestStreamsSeparatedAndErrorsStructured(t *testing.T) {
	specs := catalog.All()
	page := cannedServer(t, http.StatusOK, `{"items":[{"id":"/providers/docker/containers/web","host":"nas","name":"web","type":"containers","properties":{}}],"nextCursor":"NEXT"}`)
	r := runSpecsCLI(urlEnv(page), specs, "container", "get", "-o", "jsonl", "--verbose")
	if r.code != 0 {
		t.Fatalf("success exited %d: %s", r.code, r.stderr)
	}
	if objs := jsonLines(t, "stdout", r.stdout); len(objs) != 1 || objs[0]["name"] != "web" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if !strings.Contains(r.stderr, "> GET") {
		t.Errorf("no progress on stderr: %q", r.stderr)
	}
	var warned bool
	for _, l := range strings.Split(strings.TrimSpace(r.stderr), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["level"] == "warning" && m["nextCursor"] == "NEXT" && m["host"] == "127.0.0.1" {
			warned = true
		}
	}
	if !warned || strings.Contains(r.stdout, "NEXT") {
		t.Errorf("more-results warning not on stderr alone: stdout %q stderr %q", r.stdout, r.stderr)
	}

	fail := cannedServer(t, http.StatusNotFound, `{"type":"urn:nexus:problem:not-found","title":"Not found","status":404,"detail":"no container named web"}`)
	failEnv := urlEnv(fail)
	for _, args := range [][]string{
		{"container", "get", "web", "-o", "jsonl"},
		{"container", "get", "web"}, // Piped: jsonl by default
		{"container", "get", "web", "-o", "yaml"},
	} {
		r := runSpecsCLI(failEnv, specs, args...)
		if r.code != ExitNotFound || r.stdout != "" {
			t.Errorf("nexus %v: exit %d, stdout %q", args, r.code, r.stdout)
		}
		e := onlyJSONLine(t, r.stderr)
		if e["type"] != "urn:nexus:problem:not-found" || e["host"] != "127.0.0.1" || e["exit"] != float64(ExitNotFound) ||
			!strings.Contains(fmt.Sprint(e["message"]), "no container named web") {
			t.Errorf("nexus %v: error %v", args, e)
		}
	}
	r = runSpecsCLI(failEnv, specs, "container", "get", "web", "-o", "table")
	if r.code != ExitNotFound || r.stdout != "" || json.Valid([]byte(strings.TrimSpace(r.stderr))) ||
		!strings.Contains(r.stderr, "not found: no container named web") {
		t.Errorf("table error: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	// Errors raised before a request: usage and connection, also as JSON.
	for _, tc := range []struct {
		args []string
		code int
		typ  string
	}{
		{[]string{"container", "get", "--limit", "0"}, ExitUsage, "urn:nexus:cli:usage"},
		{[]string{"container", "get", "--bogus"}, ExitUsage, "urn:nexus:cli:usage"},
		{[]string{"container", "restart"}, ExitUsage, "urn:nexus:cli:usage"},
		{[]string{"container", "get", "--filter", "nokey"}, ExitUsage, "urn:nexus:cli:usage"},
	} {
		r := runSpecsCLI(failEnv, specs, tc.args...)
		e := onlyJSONLine(t, r.stderr)
		if r.code != tc.code || r.stdout != "" || e["type"] != tc.typ || e["level"] != "error" {
			t.Errorf("nexus %v: exit %d, stdout %q, error %v", tc.args, r.code, r.stdout, e)
		}
		if _, ok := e["host"]; !ok {
			t.Errorf("nexus %v: error without host: %v", tc.args, e)
		}
	}
	fail.Close()
	r = runSpecsCLI(failEnv, specs, "capability", "get")
	if e := onlyJSONLine(t, r.stderr); r.code != ExitConnection || e["type"] != "urn:nexus:cli:connection" || e["host"] != "127.0.0.1" {
		t.Errorf("connection failure: exit %d, error %v", r.code, e)
	}
}

func onlyJSONLine(t *testing.T, s string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	var m map[string]any
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &m) != nil {
		t.Errorf("stderr is not one JSON line: %q", s)
	}
	return m
}

type gadgetProps struct {
	State string `json:"state"`
	Foo   string `json:"foo"`
}

type gadgetListInput struct {
	Filter []string `json:"filter,omitempty"`
}

// gadgetList is a test-only list op that declares the filter key status.
var gadgetList = def.Define[gadgetListInput, apitypes.List[apitypes.Resource[gadgetProps]]](def.Op{
	ID: "testgadget.gadgets.list", Provider: "testgadget", Kind: def.KindList, ResourceType: "gadgets",
	Summary: "List gadgets", Description: "Lists gadgets. Test only.",
	Risk: def.RiskReadOnly, Mode: def.ModeSync, Filters: []def.Filter{{Key: "status", Description: "Status."}},
	CLI: def.CLIBinding{Noun: "gadget", Verb: "get"},
})

// badColumns declares a column its output does not have.
var badColumns = def.Define[gadgetListInput, apitypes.List[apitypes.Resource[gadgetProps]]](def.Op{
	ID: "testgadget.gadgets.badlist", Provider: "testgadget", Kind: def.KindList, ResourceType: "badgadgets",
	Summary: "x", Description: "Test only.", Risk: def.RiskReadOnly, Mode: def.ModeSync,
	CLI: def.CLIBinding{Noun: "badgadget", Verb: "get"}, Columns: []string{"properties.nope"},
})

func gadgetSpecs() []def.ProviderSpec {
	return append(catalog.All(), def.ProviderSpec{Name: "testgadget", Ops: []def.Op{gadgetList.Op()}})
}

// TC-167: --select keeps the paths plus host and id; a declared filter key
// is sent; an undeclared path filters client-side.
func TestSelectAndFilter(t *testing.T) {
	var items []apitypes.Resource[gadgetProps]
	for i, p := range []gadgetProps{{"on", "y"}, {"off", "n"}, {"on", "y"}, {"off", ""}} {
		items = append(items, apitypes.NewResource(fixtureHost, "testgadget", "gadgets", fmt.Sprintf("g%d", i), p))
	}
	body, err := json.Marshal(apitypes.List[apitypes.Resource[gadgetProps]]{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	srv := cannedServer(t, http.StatusOK, string(body))
	env, specs := urlEnv(srv), gadgetSpecs()

	r := runSpecsCLI(env, specs, "gadget", "get", "--select", "name,properties.state")
	if r.code != 0 {
		t.Fatalf("select exited %d: %s", r.code, r.stderr)
	}
	objs := jsonLines(t, "select", r.stdout)
	if len(objs) != len(items) {
		t.Fatalf("select printed %d objects", len(objs))
	}
	for i, o := range objs {
		want := map[string]any{"host": urlTargetHost, "id": items[i].ID, "name": items[i].Name,
			"properties": map[string]any{"state": items[i].Properties.State}}
		if !reflect.DeepEqual(o, want) {
			t.Errorf("select[%d] = %v, want %v", i, o, want)
		}
	}
	srv.take()

	r = runSpecsCLI(env, specs, "gadget", "get", "--filter", "status=x")
	reqs := srv.take()
	if r.code != 0 || len(reqs) != 1 || reqs[0].rawQuery != "filter=status%3Dx" {
		t.Errorf("server filter: exit %d, requests %+v: %s", r.code, reqs, r.stderr)
	}

	r = runSpecsCLI(env, specs, "gadget", "get", "--filter", "properties.foo=y")
	reqs = srv.take()
	if r.code != 0 || len(reqs) != 1 || strings.Contains(reqs[0].rawQuery, "filter") {
		t.Errorf("client filter: exit %d, requests %+v: %s", r.code, reqs, r.stderr)
	}
	var want, got []string
	for _, it := range items {
		if it.Properties.Foo == "y" {
			want = append(want, it.ID)
		}
	}
	for _, o := range jsonLines(t, "filter", r.stdout) {
		got = append(got, o["id"].(string))
	}
	if !slices.Equal(got, want) || len(want) != 2 {
		t.Errorf("client filter printed %v, want %v", got, want)
	}

	// Both at once: status goes to the host, properties.foo stays here.
	r = runSpecsCLI(env, specs, "gadget", "get", "--filter", "status=x", "--filter", "properties.foo=n", "-o", "json")
	reqs = srv.take()
	var arr []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &arr); err != nil || r.code != 0 || len(arr) != 1 || arr[0]["name"] != "g1" ||
		len(reqs) != 1 || reqs[0].rawQuery != "filter=status%3Dx" {
		t.Errorf("mixed filters: exit %d, out %q, requests %+v", r.code, r.stdout, reqs)
	}
}

// TC-E28: an empty list exits 0 in every format.
func TestEmptyResults(t *testing.T) {
	srv := cannedServer(t, http.StatusOK, `{"items":[]}`)
	env, specs := urlEnv(srv), catalog.All()
	for _, f := range []string{"table", "json", "jsonl", "yaml"} {
		r := runSpecsCLI(env, specs, "container", "get", "-o", f)
		if r.code != 0 || r.stderr != "" {
			t.Errorf("-o %s: exit %d, stderr %q", f, r.code, r.stderr)
		}
		switch f {
		case "jsonl":
			if r.stdout != "" {
				t.Errorf("jsonl = %q", r.stdout)
			}
		case "json":
			var v []any
			if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v == nil || len(v) != 0 {
				t.Errorf("json = %q (%v)", r.stdout, err)
			}
		case "yaml":
			var v []any
			if err := yaml.Unmarshal([]byte(r.stdout), &v); err != nil || len(v) != 0 {
				t.Errorf("yaml = %q (%v)", r.stdout, err)
			}
		case "table":
			if l := strings.ToLower(r.stdout); strings.Contains(l, "error") || !strings.HasPrefix(r.stdout, "HOST") {
				t.Errorf("table = %q", r.stdout)
			}
		}
	}
}

// Every column the catalog declares names a field of its op's output.
func TestCatalogColumnsResolve(t *testing.T) {
	a := testApp(nil)
	if _, err := a.buildRoot(); err != nil {
		t.Fatal(err)
	}
	declared := 0
	for _, op := range catalog.Ops() {
		declared += len(op.Columns)
	}
	if declared == 0 {
		t.Fatal("no op declares columns")
	}
	// An op that prints its result items when waited declares columns of
	// one item, and with --no-wait shows an operation's.
	items := 0
	for c, g := range a.generated {
		for _, op := range g.ops {
			if !op.CLI.WaitedResultItems {
				continue
			}
			items++
			item, _ := def.ResultItemType(op.OutputType())
			if len(op.Columns) == 0 || itemType(op) != item || len(g.columns[op.ID]) != len(op.Columns) {
				t.Errorf("%s: columns %v do not resolve against the result item %v", c.CommandPath(), g.columns[op.ID], item)
			}
			want, err := a.columnsOf(operationType, nil)
			if err != nil || !reflect.DeepEqual(g.operationColumns, want) {
				t.Errorf("%s: --no-wait columns %v, want an operation's %v (%v)", c.CommandPath(), g.operationColumns, want, err)
			}
		}
	}
	if items == 0 {
		t.Error("no op prints its result items")
	}
	a = testApp(append(catalog.All(), def.ProviderSpec{Name: "testgadget", Ops: []def.Op{badColumns.Op()}}))
	if _, err := a.buildRoot(); err == nil || !strings.Contains(err.Error(), "properties.nope") {
		t.Errorf("an unknown column was accepted: %v", err)
	}
}
