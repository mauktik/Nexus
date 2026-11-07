package conformance_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/conformance/testprovider"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/problem"
	scripts "github.com/mauktik/nexus/internal/providers/scripts/spec"
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

var sampleStrings = []string{"w1", "a", "red", "1", "x-1", "color=red"}

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
			for _, a := range attacks {
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
