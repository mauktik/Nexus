// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-15 and
// TC-RA-113 (WI 1.4): how the document describes a SyncMutation op, a
// SecretOutput op and a CompletesAtAdmission op.

package openapi_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/problem"
)

type raIssueIn struct {
	Name string `json:"name"`
}

type raIssueOut struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

type raSetIn struct {
	X string `json:"x,omitempty"`
}

type raSetOut struct {
	Changed bool   `json:"changed"`
	Value   string `json:"value"`
}

var (
	// raIssue is the planted SyncMutation+SecretOutput op.
	raIssue = def.Define[raIssueIn, raIssueOut](def.Op{
		ID: "ra.tokens.issue", Provider: "ra", Kind: def.KindResourceAction, ResourceType: "tokens",
		Action: "issue", Summary: "Issue a token",
		Description: "Issues a token and returns it once. Side effects: a token is created.",
		Scope:       def.Scope{Name: "ra:admin"}, Risk: def.RiskMutating, Mode: def.ModeSync,
		SyncMutation: true, SecretOutput: "token",
		CLI: def.CLIBinding{Noun: "ratoken", Verb: "add", Positional: "name"},
	})
	// raApply is a planted long-running mutating op without any new flag.
	raApply = def.Define[raSetIn, raSetOut](def.Op{
		ID: "ra.actions.apply", Provider: "ra", Kind: def.KindProviderAction, Action: "apply",
		Summary: "Apply", Description: "Applies a change. Side effects: the host changes. Long-running.",
		Scope: def.Scope{Name: "ra:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI: def.CLIBinding{Noun: "raapply", Verb: "invoke"},
	})
	// raSet is TC-RA-110's op C: long-running, CompletesAtAdmission.
	raSet = def.Define[raSetIn, raSetOut](def.Op{
		ID: "ra.actions.set", Provider: "ra", Kind: def.KindProviderAction, Action: "set",
		Summary: "Set", Description: "Sets a value. Side effects: the host changes. Long-running unless unchanged.",
		Scope: def.Scope{Name: "ra:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CompletesAtAdmission: true,
		CLI:                  def.CLIBinding{Noun: "raset", Verb: "update"},
	})
)

// raOperation builds the document for ops and returns the operation of id,
// generically parsed.
func raOperation(t *testing.T, ops []def.Op, id string) map[string]any {
	t.Helper()
	b, err := openapi.Build(ops, openapi.Options{Version: "v0.0.0-test"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, item := range obj(parse(t, b)["paths"]) {
		for _, raw := range obj(item) {
			if o := obj(raw); o["operationId"] == id {
				return o
			}
		}
	}
	t.Fatalf("no operation %s in the document", id)
	return nil
}

func raHasParam(o map[string]any, in, name string) bool {
	for _, p := range o["parameters"].([]any) {
		if obj(p)["in"] == in && obj(p)["name"] == name {
			return true
		}
	}
	return false
}

// raSameApartFrom fails unless with equals without once the response
// members drop removes from with are gone: the flags add only those.
func raSameApartFrom(t *testing.T, with, without map[string]any, drop func(responses map[string]any)) {
	t.Helper()
	for k, v := range with {
		if strings.HasPrefix(k, "x-") && !reflect.DeepEqual(v, without[k]) {
			t.Errorf("%s = %v, want %v as without the flags", k, v, without[k])
		}
	}
	// A deep copy, so drop does not change the caller's operation.
	b, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	var trimmed map[string]any
	if err := json.Unmarshal(b, &trimmed); err != nil {
		t.Fatal(err)
	}
	drop(obj(trimmed["responses"]))
	if !reflect.DeepEqual(trimmed, without) {
		got, _ := json.Marshal(trimmed)
		want, _ := json.Marshal(without)
		t.Errorf("operation differs beyond the added response members:\n got %s\nwant %s", got, want)
	}
}

// TC-RA-15: OpenAPI documents SyncMutation ops without Idempotency-Key, with 409, and a SecretOutput 200 with Cache-Control. Owner WI 1.4. Requirements: CON-3, FR-14, NFR-1.
func TestTCRA15_OpenAPIDocumentsSyncMutationOpsWithout(t *testing.T) {
	ops := []def.Op{raIssue.Op(), raApply.Op()}
	issue := raOperation(t, ops, raIssue.Op().ID)
	where := "POST " + raIssue.Op().ID

	if raHasParam(issue, "header", "Idempotency-Key") {
		t.Errorf("%s: SyncMutation op documents the Idempotency-Key parameter", where)
	}
	responses := obj(issue["responses"])
	conflict := obj(responses["409"])
	if conflict == nil {
		t.Fatalf("%s: no 409 response; responses %v", where, slices.Sorted(maps.Keys(responses)))
	}
	schema := obj(obj(obj(conflict["content"])["application/problem+json"])["schema"])
	if !slices.Contains(problemRefs(t, where+" 409", schema), problem.Conflict) {
		t.Errorf("%s: 409 does not list conflict: %v", where, schema)
	}
	ok := obj(responses["200"])
	cc := obj(obj(ok["headers"])["Cache-Control"])
	if cc == nil {
		t.Fatalf("%s: 200 declares no Cache-Control header: %v", where, ok)
	}
	if s := obj(cc["schema"]); s["const"] != "no-store" || s["type"] != "string" {
		t.Errorf("%s: Cache-Control schema = %v, want the string const no-store", where, s)
	}
	if obj(obj(ok["content"])["application/json"]) == nil {
		t.Errorf("%s: 200 lost its application/json body", where)
	}

	apply := raOperation(t, ops, raApply.Op().ID)
	if !raHasParam(apply, "header", "Idempotency-Key") {
		t.Errorf("long-running mutating op %s lost the Idempotency-Key parameter", raApply.Op().ID)
	}

	// The same op without SyncMutation and SecretOutput differs only by
	// the 409 member and the 200's headers.
	plain := raIssue.Op()
	plain.SyncMutation, plain.SecretOutput = false, ""
	without := raOperation(t, []def.Op{plain, raApply.Op()}, plain.ID)
	raSameApartFrom(t, issue, without, func(r map[string]any) {
		delete(r, "409")
		delete(obj(r["200"]), "headers")
	})
}

// TC-RA-113: OpenAPI documents both 200 and 202 for a CompletesAtAdmission op. Owner WI 1.4. Requirements: FR-14, CON-3, FR-7.
func TestTCRA113_OpenAPIDocumentsBoth200And202ForACompletesAt(t *testing.T) {
	ops := []def.Op{raSet.Op(), raApply.Op()}
	set := raOperation(t, ops, raSet.Op().ID)
	where := "POST " + raSet.Op().ID

	responses := obj(set["responses"])
	if obj(responses["202"]) == nil {
		t.Errorf("%s: no 202 response", where)
	}
	got := obj(obj(obj(obj(responses["200"])["content"])["application/json"])["schema"])
	if got == nil {
		t.Fatalf("%s: no 200 application/json response: %v", where, responses["200"])
	}
	s, err := openapi.WireSchema(raSet.Op().OutputType())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: 200 schema = %v, want the output schema %v", where, got, want)
	}
	if !raHasParam(set, "header", "Idempotency-Key") {
		t.Errorf("%s: lost the Idempotency-Key parameter", where)
	}

	apply := obj(raOperation(t, ops, raApply.Op().ID)["responses"])
	if apply["202"] == nil || apply["200"] != nil {
		t.Errorf("op without the flag: 202 %v, 200 %v; want 202 and no 200", apply["202"] != nil, apply["200"] != nil)
	}

	plain := raSet.Op()
	plain.CompletesAtAdmission = false
	without := raOperation(t, []def.Op{plain, raApply.Op()}, plain.ID)
	raSameApartFrom(t, set, without, func(r map[string]any) { delete(r, "200") })
}

// An Audited read fails closed when its record cannot be written, so its 503
// lists audit-unavailable, as a mutation's does; a plain read's does not.
func TestAuditedReadDocumentsAuditUnavailable(t *testing.T) {
	audited := def.Define[raSetIn, raSetOut](def.Op{
		ID: "ra.tokens.audited", Provider: "ra", Kind: def.KindProviderAction,
		Action: "audited", Summary: "Read audited", Description: "Reads a value, audited.",
		Scope: def.Scope{Name: "ra:admin"}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Audited: true,
		CLI: def.CLIBinding{Noun: "raaudited", Verb: "get"},
	})
	plain := def.Define[raSetIn, raSetOut](def.Op{
		ID: "ra.tokens.plain", Provider: "ra", Kind: def.KindProviderAction,
		Action: "plain", Summary: "Read plain", Description: "Reads a value.",
		Scope: def.Scope{Name: "ra:admin"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "raplain", Verb: "get"},
	})
	ops := []def.Op{audited.Op(), plain.Op()}
	for id, want := range map[string]bool{audited.Op().ID: true, plain.Op().ID: false} {
		o := raOperation(t, ops, id)
		schema := obj(obj(obj(obj(o["responses"])["503"])["content"])["application/problem+json"])["schema"]
		got := slices.Contains(problemRefs(t, id+" 503", obj(schema)), problem.AuditUnavailable)
		if got != want {
			t.Errorf("%s: 503 lists audit-unavailable = %v, want %v", id, got, want)
		}
	}
}
