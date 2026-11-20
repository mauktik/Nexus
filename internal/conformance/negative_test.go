package conformance_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/conformance/testprovider"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/problem"
	nexusd "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	nginx "github.com/mauktik/nexus/internal/providers/nginx/spec"
	pihole "github.com/mauktik/nexus/internal/providers/pihole/spec"
	scripts "github.com/mauktik/nexus/internal/providers/scripts/spec"
	services "github.com/mauktik/nexus/internal/providers/services/spec"
	storage "github.com/mauktik/nexus/internal/providers/storage/spec"
)

// attacks is the FR-25 list sent through every string parameter: shell
// metacharacters, leading-dash values, path traversal, control characters
// and a 64 KiB value.
var attacks = []struct{ name, value string }{
	{"semicolon", "; rm -rf /"},
	{"substitution", "$(id)"},
	{"backticks", "`id`"},
	{"pipe", "|"},
	{"and", "&&"},
	{"long-flag", "--force"},
	{"short-flag", "-o"},
	{"traversal", "../../etc/passwd"},
	{"newline", "a\nid"},
	{"nul", "a\x00id"},
	{"64KiB", strings.Repeat("A", 64<<10)},
}

// piholeAttacks are sent, besides attacks, through every string parameter
// of the pihole ops: a path separator, the batch change's "=" and a quote,
// each of which a Pi-hole input could carry into a dnsmasq line or a shell
// argument (CON-2).
var piholeAttacks = []struct{ name, value string }{
	{"slash", "a/b"},
	{"equals", "a=b"},
	{"quote", "a'b"},
}

// servicesAttacks are sent, besides attacks, through the services ops'
// string parameters named here (services-provider FR-25, TC-SV-121): a
// service name that is a path, carries a separator or a space, is one
// character too long, starts with a dash or is the canonical .service name,
// and a confirmHost carrying a newline, a NUL or an escape sequence. Each
// must answer 400 with no handler call, confirmHost's inert acceptance
// notwithstanding.
var servicesAttacks = map[string][]struct{ name, value string }{
	"/name": {
		{"dotdot", "../x"},
		{"slash", "a/b"},
		{"semicolon", "a;b"},
		{"space", "a b"},
		{"65chars", strings.Repeat("a", services.MaxNameLength+1)},
		{"dash", "-x"},
		{"unit-suffix", "x.service"},
	},
	"/confirmHost": {
		{"newline", "h1\n"},
		{"nul", "h1\x00"},
		{"escape", "h1\x1b[31m"},
	},
}

// negative is one FR-25 input of a provider's own grammar: input sent to op
// must be refused with 400 validation-failed naming field, before any
// handler call.
type negative struct {
	op    def.Op
	field string
	input map[string]any
}

// storageNegatives are the storage ops' FR-25 inputs (TC-STG-202): pool
// and dataset names outside the OpenZFS grammar, labels, vmids and cursors
// out of bounds, and a body that names the snapshot.
func storageNegatives() []negative {
	var out []negative
	add := func(op def.Op, field string, base map[string]any, values ...any) {
		for _, v := range values {
			in := map[string]any{field: v}
			for k, b := range base {
				in[k] = b
			}
			out = append(out, negative{op: op, field: field, input: in})
		}
	}
	pools := []any{"../x", "a;b", "a b", "1pool", "-x", "p" + strings.Repeat("a", 255)}
	for _, op := range []def.Op{storage.PoolsGet.Op(), storage.PoolsScrub.Op(), storage.PoolsStopScrub.Op(), storage.PoolsList.Op()} {
		add(op, "name", nil, pools...)
	}
	add(storage.DatasetsList.Op(), "pool", nil, pools...)
	datasets := []any{"z1/../x", "z1//x", "z1/x@y", "z1/x#y", "/z1"}
	add(storage.DatasetsGet.Op(), "name", nil, datasets...)
	add(storage.DatasetsSnapshot.Op(), "name", nil, datasets...)
	add(storage.SnapshotsList.Op(), "dataset", nil, datasets...)
	add(storage.DatasetsSnapshot.Op(), "label", map[string]any{"name": "z1/ds/share"}, "Bad", "-x", strings.Repeat("a", 33))
	add(storage.BackupsList.Op(), "vmid", nil, 99, 1000000000)
	for _, op := range []def.Op{storage.PoolsList.Op(), storage.DatasetsList.Op(), storage.SnapshotsList.Op(),
		storage.BackupsList.Op(), storage.TimeMachineList.Op()} {
		add(op, "cursor", nil, strings.Repeat("a", storage.MaxCursorLength+1))
	}
	// The caller never names the snapshot (D-11).
	add(storage.DatasetsSnapshot.Op(), "snapshot", map[string]any{"name": "z1/ds/share"}, "nexus-x")
	return out
}

// attacksFor returns the attack list for op.
func attacksFor(op def.Op) []struct{ name, value string } {
	if op.Provider == pihole.Name {
		return append(slices.Clip(attacks), piholeAttacks...)
	}
	return attacks
}

// paramAttacksFor returns the attack list for op's string parameter path:
// attacksFor, plus servicesAttacks for a services parameter.
func paramAttacksFor(op def.Op, path string) []struct{ name, value string } {
	if op.Provider == services.Name {
		return slices.Concat(attacksFor(op), servicesAttacks[path])
	}
	return attacksFor(op)
}

// inertAllowed lists the parameters, as "<op> <path>", that may accept a
// hostile value because their handler takes it only as data. An accepted
// value must reach the handler byte for byte. Providers add their
// parameters here, and prove inertness against real processes at T1 (5.3,
// 7.4).
var inertAllowed = map[string]bool{
	testprovider.WidgetsRestart.Op().ID + " /note": true,
	testprovider.ActionsEcho.Op().ID + " /text":    true,
	// Script parameter values reach the script only as one argv element or
	// one env value (5.3 proves it against real processes, TC-106).
	scripts.Invoke.Op().ID + " /parameters/{key}": true,
	// nginx site content is only written to the configuration file and read
	// by nginx, never passed to a shell or as an argument (FR-12).
	nginx.Add.Op().ID + " /content":    true,
	nginx.Update.Op().ID + " /content": true,
	// A regex domain value reaches gravity.db only as a bound SQL parameter,
	// never a shell or dnsmasq line; WI 5.2 proves it at T1 by storing a
	// regex with `\.` and ";querytype=A" byte for byte.
	pihole.AddDomain.Op().ID + " /value":    true,
	pihole.RemoveDomain.Op().ID + " /value": true,
	// confirmHost is only compared with the host's name and recorded as an
	// audit fact, never passed to a command; its schema refuses control
	// characters only (services-provider FR-10). WI 5.3 proves inertness on
	// real init systems (A-42).
	services.Reboot.Op().ID + " /confirmHost": true,
}

// stringParams lists the paths of op's string inputs.
func stringParams(t *testing.T, op def.Op) []string {
	t.Helper()
	var out []string
	visitSchema(inputSchema(t, op), "", func(path string, s *jsonschema.Schema) {
		if hasType(s, "string") && path != "" {
			out = append(out, path)
		}
	})
	return out
}

// sampler builds valid inputs from an op's schema: each required property
// gets a value its schema accepts. A FromTarget op's target is the fixture
// operation; any other required string takes the first candidate that
// validates.
type sampler struct {
	target string // ID of an existing operation
}

var sampleStrings = []string{"w1", "a", "red", "1", "x-1", "color=red",
	"a.b", "192.0.2.1", "a.b~A", "add-zone:a.b", "https://a.b/l.txt",
	strings.Repeat("a", 32),        // An upload ID: 32 lowercase hex digits
	strings.Repeat("A", 86) + "==", // A signature's form: 64 bytes in padded base64
}

func (sm sampler) value(t *testing.T, op def.Op, path string, s *jsonschema.Schema) any {
	t.Helper()
	if len(s.Enum) > 0 {
		return s.Enum[0]
	}
	switch {
	case hasType(s, "object"):
		obj := map[string]any{}
		for _, name := range s.Required {
			obj[name] = sm.value(t, op, path+"/"+name, s.Properties[name])
		}
		return obj
	case hasType(s, "array"):
		// An array that must not be empty gets one sampled item.
		if s.MinItems != nil && *s.MinItems >= 1 {
			return []any{sm.value(t, op, path+"/*", s.Items)}
		}
		return []any{}
	case hasType(s, "boolean"):
		return false
	case hasType(s, "integer"), hasType(s, "number"):
		if s.Minimum != nil {
			return *s.Minimum
		}
		return 1
	case hasType(s, "string"):
		if op.Scope.FromTarget && path == "/id" {
			return sm.target
		}
		r, err := s.CloneSchemas().Resolve(nil)
		if err != nil {
			t.Fatalf("%s %s: %v", op.ID, path, err)
		}
		for _, c := range sampleStrings {
			if r.Validate(c) == nil {
				return c
			}
		}
	}
	t.Fatalf("%s %s: no sample value; add one to sampleStrings", op.ID, path)
	return nil
}

// withValue returns a copy of base with v at path, building the objects on
// the way from the schema.
func (sm sampler) withValue(t *testing.T, op def.Op, base map[string]any, path string, v any) map[string]any {
	t.Helper()
	root := cloneJSON(t, base).(map[string]any)
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	var set func(cur any, s *jsonschema.Schema, segs []string, at string) any
	set = func(cur any, s *jsonschema.Schema, segs []string, at string) any {
		if len(segs) == 0 {
			return v
		}
		switch seg := segs[0]; seg {
		case "*":
			return []any{set(nil, s.Items, segs[1:], at+"/*")}
		default:
			obj, _ := cur.(map[string]any)
			if obj == nil {
				obj, _ = sm.value(t, op, at, s).(map[string]any)
			}
			child := s.Properties[seg]
			key := seg
			if seg == "{key}" {
				child, key = s.AdditionalProperties, "k"
			}
			obj[key] = set(obj[key], child, segs[1:], at+"/"+seg)
			return obj
		}
	}
	return set(root, inputSchema(t, op), segs, "").(map[string]any)
}

func cloneJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// valueAt reads path from a decoded JSON value.
func valueAt(v any, path string) (any, bool) {
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		switch seg {
		case "*":
			arr, ok := v.([]any)
			if !ok || len(arr) != 1 {
				return nil, false
			}
			v = arr[0]
		default:
			obj, ok := v.(map[string]any)
			if !ok {
				return nil, false
			}
			if seg == "{key}" {
				seg = "k"
			}
			if v, ok = obj[seg]; !ok {
				return nil, false
			}
		}
	}
	return v, true
}

// TC-84: every attack through every string parameter of every op is
// rejected with 400 before any handler runs, or the parameter is
// inert-allowed and the handler receives the exact bytes.
//
// One rejection is not a 400: a FromTarget op's target is loaded, to
// authorize, before input validation (Dispatcher order, TC-31), so a
// hostile operation ID is a lookup key that finds nothing and answers 404
// not-found. No handler runs.
func TestAttackStringsAgainstEveryStringParameter(t *testing.T) {
	s := newStack(t)
	sm := sampler{target: s.restart(t, s.full).ID}

	walked := map[string]bool{}
	accepted := 0
	for _, op := range s.ops() {
		baseAny := sm.value(t, op, "", inputSchema(t, op))
		base, _ := baseAny.(map[string]any)
		r := s.call(t, op, s.full, base)
		if r.status == http.StatusBadRequest {
			t.Fatalf("%s: the sample input %v is invalid: %s", op.ID, base, r.body)
		}
		if r.status == http.StatusAccepted {
			s.received(t, op, r, 0) // Its run must not land among an attack's calls
		}
		_, route, err := openapi.Route(op)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range stringParams(t, op) {
			key := op.ID + " " + path
			walked[key] = true
			isTarget := op.Scope.FromTarget && strings.TrimPrefix(path, "/") == targetWildcard(route)
			for _, a := range paramAttacksFor(op, path) {
				t.Run(fmt.Sprintf("%s%s/%s", op.ID, path, a.name), func(t *testing.T) {
					in := sm.withValue(t, op, base, path, a.value)
					before := len(s.spy.Calls())
					r := s.call(t, op, s.full, in)
					switch {
					case r.status == http.StatusBadRequest:
						if p := r.problem(t); p.Slug() != problem.ValidationFailed {
							t.Errorf("400 with problem %s", p.Slug())
						}
						if n := len(s.spy.Calls()); n != before {
							t.Errorf("rejected, yet %d handler calls ran", n-before)
						}
					case isTarget && r.status == http.StatusNotFound:
						if p := r.problem(t); p.Slug() != problem.NotFound {
							t.Errorf("404 with problem %s", p.Slug())
						}
					case r.status/100 == 2 && inertAllowed[key]:
						accepted++
						got := s.received(t, op, r, before)
						if v, ok := valueAt(got, path); !ok || v != a.value {
							t.Errorf("handler received %.80q at %s, want the exact attack bytes", fmt.Sprint(v), path)
						}
					default:
						t.Errorf("status %d %.300s: want 400, or an inert-allowed parameter", r.status, r.body)
					}
				})
			}
		}
	}

	if len(walked) == 0 || accepted == 0 {
		t.Fatalf("walked %d parameters with %d inert acceptances: the generator checked nothing", len(walked), accepted)
	}
	for key := range inertAllowed {
		if !walked[key] {
			t.Errorf("inertAllowed names %q, which is not a string parameter of any op", key)
		}
	}
	for _, op := range declared() {
		for _, path := range stringParams(t, op) {
			if !walked[op.ID+" "+path] {
				t.Errorf("%s %s was not attacked", op.ID, path)
			}
		}
	}
}

// targetWildcard is the first wildcard of a route, the field a FromTarget
// op loads its target by.
func targetWildcard(route string) string {
	i := strings.Index(route, "{")
	j := strings.Index(route, "}")
	if i < 0 || j < i {
		return ""
	}
	return route[i+1 : j]
}

// received waits for the handler call a 2xx answer produced and returns its
// input as generic JSON: the newest call of op for a sync or stream op, and
// the call of the accepted operation for a long-running one.
func (s *stack) received(t *testing.T, op def.Op, r response, before int) any {
	t.Helper()
	var c testprovider.Call
	if r.status == http.StatusAccepted {
		var o apitypes.Operation
		if err := json.Unmarshal(r.body, &o); err != nil {
			t.Fatal(err)
		}
		c = s.waitCall(t, func(c testprovider.Call) bool { return c.OperationID == o.ID })
	} else {
		calls := s.spy.Calls()
		i := slices.IndexFunc(calls[before:], func(c testprovider.Call) bool { return c.Op == op.ID })
		if i < 0 {
			t.Fatalf("%s answered %d but no handler call was recorded", op.ID, r.status)
		}
		c = calls[before+i]
	}
	v, err := testprovider.InputJSON(c)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// blockingBody hands out head, then blocks until release is closed: a
// client still holding the rest of its body.
type blockingBody struct {
	head    io.Reader
	release chan struct{}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if n, err := b.head.Read(p); n > 0 || !errors.Is(err, io.EOF) {
		return n, err
	}
	<-b.release
	return 0, io.EOF
}

// TC-SM-ERR59 (WI 6.1; NFR-7, FR-6, FR-1): the nexusd upload, bound by
// testprovider.Upload in catalogStubs, is refused four ways before Receive
// runs: a body over the stub's cap (413), a read-only token (403 naming
// nexusd:admin), no token (401, answered while the client still holds its
// body, so the server never read it) and a JSON content type (400).
func TestTCSMERR59_ConformanceNegativesOnTheUploadOversize(t *testing.T) {
	s := newStack(t)
	upload := nexusd.Upload.Op()
	_, path, err := openapi.Route(upload)
	if err != nil {
		t.Fatal(err)
	}
	sig := url.Values{"signature": {strings.Repeat("A", 86) + "=="}}.Encode()
	target := s.base + path + "?" + sig
	admin := s.exact[nexusd.AdminScope]

	send := func(t *testing.T, token, contentType string, body io.Reader, length int64) response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, target, body)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = length
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", contentType)
		hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
		defer hc.CloseIdleConnections()
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response{status: resp.StatusCode, body: b}
	}
	expect := func(t *testing.T, r response, status int, slug problem.Slug) *problem.Error {
		t.Helper()
		if r.status != status {
			t.Fatalf("status %d %.300s, want %d", r.status, r.body, status)
		}
		p := r.problem(t)
		if p.Slug() != slug {
			t.Fatalf("problem %s, want %s", p.Slug(), slug)
		}
		return p
	}

	// The control: the same request with the admin token and a body at the
	// cap is received, so the refusals below are not a broken route.
	atCap := bytes.Repeat([]byte("u"), testprovider.UploadMaxBytes)
	if r := send(t, admin, octetStream, bytes.NewReader(atCap), int64(len(atCap))); r.status != http.StatusCreated {
		t.Fatalf("control upload at the cap: %d %.300s", r.status, r.body)
	}
	calls := s.spy.Calls()
	if len(calls) != 1 || calls[0].Op != upload.ID || calls[0].BodyBytes != testprovider.UploadMaxBytes {
		t.Fatalf("control upload: spy calls %+v, want one Receive of %d bytes", calls, testprovider.UploadMaxBytes)
	}

	t.Run("over the cap", func(t *testing.T) {
		over := bytes.Repeat([]byte("u"), testprovider.UploadMaxBytes+1)
		r := send(t, admin, octetStream, bytes.NewReader(over), int64(len(over)))
		expect(t, r, http.StatusRequestEntityTooLarge, problem.UploadTooLarge)
	})
	t.Run("read-only token", func(t *testing.T) {
		r := send(t, s.exact[nexusd.ReadScope], octetStream, bytes.NewReader(uploadBody), int64(len(uploadBody)))
		if p := expect(t, r, http.StatusForbidden, problem.Forbidden); p.Extensions["requiredScope"] != nexusd.AdminScope {
			t.Errorf("requiredScope %v, want %s", p.Extensions["requiredScope"], nexusd.AdminScope)
		}
	})
	t.Run("no token", func(t *testing.T) {
		// A watchdog frees the body after 3 s, because the client cannot
		// give up while its body read blocks; an answer that came only
		// after it means the server waited for the body.
		release := make(chan struct{})
		var once sync.Once
		var fired atomic.Bool
		free := func() { once.Do(func() { close(release) }) }
		watchdog := time.AfterFunc(3*time.Second, func() { fired.Store(true); free() })
		defer watchdog.Stop()
		defer free()
		body := &blockingBody{head: bytes.NewReader(uploadBody), release: release}
		r := send(t, "", octetStream, body, -1)
		if fired.Load() {
			t.Fatal("the 401 came only after the client body was released: the server waited for the body")
		}
		free()
		expect(t, r, http.StatusUnauthorized, problem.Unauthorized)
	})
	t.Run("JSON content type", func(t *testing.T) {
		r := send(t, admin, "application/json", bytes.NewReader([]byte(`{}`)), 2)
		expect(t, r, http.StatusBadRequest, problem.ValidationFailed)
	})

	if got := s.spy.Calls(); len(got) != 1 {
		t.Errorf("Receive ran for a refused upload: spy calls %+v, want only the control", got)
	}
}
