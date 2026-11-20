// Tests of planning/services-provider/test-cases.md TC-SV-30, TC-SV-31.
// Owner WI: 2.1.

package problem

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// TC-SV-30: The eight services problem types are registered with their status, exit code and extensions. Owner WI 2.1. Requirements: FR-6, FR-7, FR-9, FR-10, FR-11.
//
// The table is the Problem types table of
// planning/services-provider/architecture.md (Interfaces).
func TestTCSV30_TheEightServicesProblemTypesAreRegistered(t *testing.T) {
	want := []struct {
		slug           Slug
		status, exit   int
		extensions     []string
		operationLevel bool
	}{
		// rc7 integration: not-allowed is shared with storage, its extensions the union (M-1).
		{"not-allowed", 409, 1, []string{"service", "action", "configKey", "pool", "dataset"}, false},
		{"protected-service", 409, 1, []string{"service", "reason"}, false},
		{"reboot-unavailable", 409, 1, []string{"reason", "configKey"}, false},
		{"nexusd-not-enabled-at-boot", 409, 1, []string{"service", "state"}, false},
		{"operations-running", 409, 1, []string{"operationIds"}, false},
		{"logs-unavailable", 422, 1, []string{"service", "logSource", "reason"}, false},
		{"service-action-failed", 0, 1, []string{"service", "action", "state", "rawState", "exitCode"}, true},
		{"reboot-did-not-happen", 0, 1, []string{"stage", "bootId", "exitCode"}, true},
	}
	// The existing operation-level slugs carry the marker the new ones must.
	for _, ref := range []Slug{OutcomeUnknown, ScriptFailed} {
		if typ, _ := Lookup(ref); !typ.OperationLevel || typ.Status != 0 {
			t.Fatalf("reference %s: operationLevel %v status %d", ref, typ.OperationLevel, typ.Status)
		}
	}
	inOps := func(slug Slug) bool {
		return slices.ContainsFunc(OperationLevel(), func(t Type) bool { return t.Slug == slug })
	}
	inHTTP := func(slug Slug) bool {
		return slices.ContainsFunc(HTTP(), func(t Type) bool { return t.Slug == slug })
	}
	for _, w := range want {
		t.Run(string(w.slug), func(t *testing.T) {
			typ, ok := Lookup(w.slug)
			if !ok {
				t.Fatalf("%s: missing from registry", w.slug)
			}
			if typ.Status != w.status || typ.Exit != w.exit || typ.OperationLevel != w.operationLevel {
				t.Errorf("status %d exit %d operationLevel %v, want %d %d %v",
					typ.Status, typ.Exit, typ.OperationLevel, w.status, w.exit, w.operationLevel)
			}
			if !slices.Equal(typ.Extensions, w.extensions) {
				t.Errorf("extensions %v, want %v", typ.Extensions, w.extensions)
			}
			if typ.Title == "" {
				t.Error("empty title")
			}
			if typ.URI() != "urn:nexus:problem:"+string(w.slug) {
				t.Errorf("URI %q", typ.URI())
			}
			if inOps(w.slug) != w.operationLevel || inHTTP(w.slug) == w.operationLevel {
				t.Errorf("in OperationLevel() %v, in HTTP() %v; want operation-level %v",
					inOps(w.slug), inHTTP(w.slug), w.operationLevel)
			}
			p := New(w.slug, "d")
			for _, ext := range w.extensions {
				p.With(ext, "x")
			}
			if len(p.Extensions) != len(w.extensions) {
				t.Errorf("With accepted %d of %d declared extensions", len(p.Extensions), len(w.extensions))
			}
		})
	}

	for _, slug := range []Slug{ServiceActionFailed, RebootDidNotHappen} {
		if !slices.Contains(wantOperationLevel, slug) {
			t.Errorf("wantOperationLevel lacks %s", slug)
		}
	}
	for _, slug := range []Slug{NotAllowed, ProtectedService, RebootUnavailable, NexusdNotEnabledAtBoot,
		OperationsRunning, LogsUnavailable} {
		if _, ok := wantHTTP[slug]; !ok {
			t.Errorf("wantHTTP lacks %s", slug)
		}
	}
}

// TC-SV-31: The new problem types round-trip through JSON with their extensions. Owner WI 2.1. Requirements: FR-10, FR-11.
func TestTCSV31_TheNewProblemTypesRoundTripThrough(t *testing.T) {
	const prev = "8f2d4b0e-6c1a-4f3e-9b7d-2a5c8e1f0d39"
	cases := []struct {
		in   *Error
		slug Slug
		want map[string]any
	}{
		{
			New(OperationsRunning, "2 operations are running").With("operationIds", []string{"01A", "01B"}),
			OperationsRunning,
			map[string]any{"operationIds": []any{"01A", "01B"}},
		},
		{
			New(RebootDidNotHappen, "the reboot command failed").
				With("stage", "trigger").With("bootId", prev).With("exitCode", 1),
			RebootDidNotHappen,
			map[string]any{"stage": "trigger", "bootId": prev, "exitCode": float64(1)},
		},
		{
			New(NotAllowed, "nx-a may not stop").
				With("service", "nx-a").With("action", "stop").With("configKey", "providers.services.control"),
			NotAllowed,
			map[string]any{"service": "nx-a", "action": "stop", "configKey": "providers.services.control"},
		},
	}
	for _, c := range cases {
		t.Run(string(c.slug), func(t *testing.T) {
			first, err := c.in.MarshalJSON()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out Error
			if err := out.UnmarshalJSON(first); err != nil {
				t.Fatalf("unmarshal %s: %v", first, err)
			}
			second, err := out.MarshalJSON()
			if err != nil {
				t.Fatalf("second marshal: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Errorf("second marshal differs:\n first %s\nsecond %s", first, second)
			}
			if out.Slug() != c.slug || out.Status != c.in.Status || out.Title != c.in.Title || out.Detail != c.in.Detail {
				t.Errorf("standard members %+v, want those of %+v", out, *c.in)
			}
			if !reflect.DeepEqual(out.Extensions, c.want) {
				t.Errorf("extensions %#v, want %#v", out.Extensions, c.want)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(first, &raw); err != nil {
				t.Fatal(err)
			}
			if ids, ok := raw["operationIds"]; ok && string(ids) != `["01A","01B"]` {
				t.Errorf("operationIds on the wire = %s, want a JSON array of strings", ids)
			}
			if code, ok := raw["exitCode"]; ok && string(code) != `1` {
				t.Errorf("exitCode on the wire = %s, want the number 1", code)
			}
		})
	}
}
