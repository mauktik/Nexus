package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// assertExplicit is D-5's one test helper (architecture D-5, FR-12): it
// marshals v and walks every JSON object in it, requiring that
//   - each null field is a key of the object's unavailable map, with a
//     non-empty reason;
//   - each key of unavailable has a non-empty reason and names a field of
//     the object that is null, [] or the tri-state "unknown" (unknownState):
//     a true, false or unknown field such as clock.synchronized is never
//     null, so when a source fails it is unknown and named with the reason;
//   - no list item is null.
//
// The one documented exception is series[].values, whose null items mark
// history buckets without a sample. Every handler test of Phase 4 calls it
// on every output.
func assertExplicit(t testing.TB, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("assertExplicit: marshal: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("assertExplicit: unmarshal: %v", err)
	}
	walkExplicit(t, "$", doc, false)
}

// walkExplicit checks node at path. nullItemsOK is set for the items of a
// series' values list.
func walkExplicit(t testing.TB, path string, node any, nullItemsOK bool) {
	t.Helper()
	switch n := node.(type) {
	case map[string]any:
		checkObjectExplicit(t, path, n)
		for _, k := range slices.Sorted(maps.Keys(n)) {
			if k == "unavailable" {
				continue
			}
			walkExplicit(t, path+"."+k, n[k], k == "values" && isSeriesItem(path))
		}
	case []any:
		for i, item := range n {
			p := path + "[" + strconv.Itoa(i) + "]"
			if item == nil {
				if !nullItemsOK {
					t.Errorf("%s: a list item is null; only series[].values may hold null", p)
				}
				continue
			}
			walkExplicit(t, p, item, false)
		}
	}
}

// isSeriesItem reports whether path is an item of a list named series.
func isSeriesItem(path string) bool {
	i := strings.LastIndexByte(path, '[')
	return i >= 0 && strings.HasSuffix(path, "]") && strings.HasSuffix(path[:i], ".series")
}

func checkObjectExplicit(t testing.TB, path string, obj map[string]any) {
	t.Helper()
	reasons := map[string]string{}
	if u, ok := obj["unavailable"]; ok {
		m, isObj := u.(map[string]any)
		if !isObj {
			t.Errorf("%s.unavailable is %v, not an object", path, u)
		}
		for k, r := range m {
			s, isStr := r.(string)
			if !isStr {
				t.Errorf("%s.unavailable.%s is %v, not a string", path, k, r)
				continue
			}
			reasons[k] = s
		}
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if k == "unavailable" {
			continue
		}
		if obj[k] == nil && reasons[k] == "" {
			t.Errorf("%s.%s is null with no reason in unavailable", path, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(reasons)) {
		r := reasons[k]
		if r == "" {
			t.Errorf("%s.unavailable.%s has an empty reason", path, k)
		}
		val, ok := obj[k]
		switch {
		case !ok:
			t.Errorf("%s.unavailable names %s, which is not a field of the object", path, k)
		case val == nil:
		case isEmptyList(val):
		case val == unknownState:
		default:
			t.Errorf("%s.unavailable names %s (%q), which is %v, not null, [] or %q", path, k, r, val, unknownState)
		}
	}
}

func isEmptyList(v any) bool {
	l, ok := v.([]any)
	return ok && len(l) == 0
}

// explicitRecorder is a testing.TB that records Errorf and Fatalf instead
// of failing, for testing assertExplicit itself (TC-HP-ERR28).
type explicitRecorder struct {
	testing.TB
	errs []string
}

func (r *explicitRecorder) Helper() {}

func (r *explicitRecorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *explicitRecorder) Fatalf(format string, args ...any) {
	r.errs = append(r.errs, "fatal: "+fmt.Sprintf(format, args...))
}
