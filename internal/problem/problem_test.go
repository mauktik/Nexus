package problem

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// Copied from the Problem types table in architecture.md (Interfaces).
var wantHTTP = map[Slug]int{
	"validation-failed":       400,
	"unauthorized":            401,
	"forbidden":               403,
	"not-found":               404,
	"route-not-found":         404,
	"method-not-allowed":      405,
	"conflict":                409,
	"not-cancelable":          409,
	"payload-too-large":       413,
	"not-updatable":           422,
	"script-integrity-failed": 422,
	"compose-files-missing":   422,
	"idempotency-key-reused":  422,
	"rate-limited":            429,
	"internal-error":          500,
	"capability-unavailable":  503,
	"compose-plugin-missing":  503,
	"dependency-unavailable":  503,
	"audit-unavailable":       503,
	"storage-limit-reached":   507,
	"dns-file-conflict":       409,
	"dns-name-unzoned":        422,
	"dns-batch-rejected":      422,
}

var wantOperationLevel = []Slug{
	"outcome-unknown", "package-manager-busy", "package-manager-broken",
	"apt-failed", "script-failed", "script-timeout", "update-health-failed",
	"dns-config-test-failed", "dns-verification-failed", "dns-rollback-failed",
	"gravity-failed", "pihole-command-failed", "pihole-busy",
}

// TC-7: the registry is complete and marshals as RFC 9457.
func TestRegistryMatchesTable(t *testing.T) {
	if len(wantHTTP) != 23 || len(wantOperationLevel) != 13 {
		t.Fatalf("test table drifted: %d HTTP, %d operation-level", len(wantHTTP), len(wantOperationLevel))
	}

	all := All()
	if len(all) != 36 {
		t.Errorf("registry has %d types, want 36 (23 HTTP + 13 operation-level)", len(all))
	}
	for _, typ := range all {
		if !strings.HasPrefix(typ.URI(), "urn:nexus:problem:") || typ.URI() != "urn:nexus:problem:"+string(typ.Slug) {
			t.Errorf("%s: type URI %q, want urn:nexus:problem:<slug>", typ.Slug, typ.URI())
		}
		if strings.Contains(typ.URI(), "://") || strings.Contains(typ.URI(), ".") {
			t.Errorf("%s: type URI %q depends on a domain", typ.Slug, typ.URI())
		}
		if typ.Title == "" {
			t.Errorf("%s: empty title", typ.Slug)
		}
		if typ.Exit < 1 || typ.Exit > 4 {
			t.Errorf("%s: CLI exit %d outside 1..4", typ.Slug, typ.Exit)
		}
	}

	for slug, status := range wantHTTP {
		typ, ok := Lookup(slug)
		if !ok {
			t.Errorf("%s: missing from registry", slug)
			continue
		}
		if typ.Status != status || typ.OperationLevel {
			t.Errorf("%s: status %d operationLevel %v, want %d false", slug, typ.Status, typ.OperationLevel, status)
		}
	}
	for _, slug := range wantOperationLevel {
		typ, ok := Lookup(slug)
		if !ok {
			t.Errorf("%s: operation-level slug missing from registry", slug)
			continue
		}
		if !typ.OperationLevel || typ.Exit != 1 {
			t.Errorf("%s: operationLevel %v exit %d, want true 1", slug, typ.OperationLevel, typ.Exit)
		}
	}

	ops := OperationLevel()
	for _, compose := range []Slug{"compose-files-missing", "compose-plugin-missing"} {
		if slices.ContainsFunc(ops, func(t Type) bool { return t.Slug == compose }) {
			t.Errorf("%s is in the operation-level list; it belongs to the HTTP table", compose)
		}
		if !slices.ContainsFunc(HTTP(), func(t Type) bool { return t.Slug == compose }) {
			t.Errorf("%s is missing from the HTTP table", compose)
		}
	}
	if len(HTTP()) != 23 || len(ops) != 13 {
		t.Errorf("HTTP() = %d, OperationLevel() = %d; want 23 and 13", len(HTTP()), len(ops))
	}

	if _, ok := Lookup("no-such-problem"); ok {
		t.Error("Lookup of an unknown slug succeeded")
	}
}

func TestExitCodesMatchTable(t *testing.T) {
	want := map[Slug]int{
		ValidationFailed: 2, Unauthorized: 3, Forbidden: 3, NotFound: 4, RouteNotFound: 4,
		MethodNotAllowed: 2, Conflict: 1, NotCancelable: 1, PayloadTooLarge: 2, NotUpdatable: 1,
		ScriptIntegrityFailed: 1, ComposeFilesMissing: 1, IdempotencyKeyReused: 1, RateLimited: 3,
		InternalError: 1, CapabilityUnavailable: 4, ComposePluginMissing: 4, DependencyUnavailable: 1,
		AuditUnavailable: 1, StorageLimitReached: 1,
		DNSFileConflict: 1, DNSNameUnzoned: 1, DNSBatchRejected: 1,
	}
	for slug, exit := range want {
		if typ, _ := Lookup(slug); typ.Exit != exit {
			t.Errorf("%s: exit %d, want %d", slug, typ.Exit, exit)
		}
	}
}

func TestForbiddenMarshalsAsRFC9457(t *testing.T) {
	p := New(Forbidden, "token lacks containers:write").With("requiredScope", "containers:write")

	rec := httptest.NewRecorder()
	if err := Write(rec, p); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	want := map[string]any{
		"type":          "urn:nexus:problem:forbidden",
		"title":         "Forbidden",
		"status":        float64(403),
		"detail":        "token lacks containers:write",
		"requiredScope": "containers:write",
	}
	if fmt.Sprint(body) != fmt.Sprint(want) {
		t.Errorf("body = %v, want %v", body, want)
	}
}

func TestOperationLevelErrorOmitsStatus(t *testing.T) {
	b, err := json.Marshal(New(ScriptFailed, "exit status 3"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"type":"urn:nexus:problem:script-failed","title":"Script failed","detail":"exit status 3"}`; string(b) != want {
		t.Errorf("operation-level error = %s, want %s", b, want)
	}
}

func TestUnmarshalRoundTrip(t *testing.T) {
	in := New(Conflict, "lane busy").With("blockingOperationId", "op-1")
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Error
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if out.Type != in.Type || out.Status != 409 || out.Slug() != Conflict || out.Extensions["blockingOperationId"] != "op-1" {
		t.Errorf("round trip = %+v", out)
	}

	var e error = in
	var pe *Error
	if !errors.As(fmt.Errorf("wrapped: %w", e), &pe) || pe.Slug() != Conflict {
		t.Error("errors.As does not find a wrapped *Error")
	}
	if !strings.Contains(e.Error(), "conflict") || !strings.Contains(e.Error(), "lane busy") {
		t.Errorf("Error() = %q, want slug and detail", e.Error())
	}
}

func TestRegistryIsTheOnlyPlaceTypesAreDefined(t *testing.T) {
	cases := map[string]func(){
		"unknown slug":            func() { New("made-up", "x") },
		"undeclared extension":    func() { New(Forbidden, "x").With("operationId", "y") },
		"reserved member as ext.": func() { New(Forbidden, "x").With("status", 200) },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("did not panic")
				}
			}()
			f()
		})
	}
}

func TestValidationFailedCarriesErrors(t *testing.T) {
	p := New(ValidationFailed, "input is invalid").With("errors", []FieldError{{Path: "/mode", Message: "not in enum"}})
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"errors":[{"path":"/mode","message":"not in enum"}]`) {
		t.Errorf("validation-failed body = %s", b)
	}
}

// CR-24: a failed apt run still reports what it changed, so apt-failed
// declares the members the apt provider attaches.
func TestAptFailedDeclaresTheOutcomeMembers(t *testing.T) {
	p := New(AptFailed, "apt-get exited with status 100")
	for _, k := range []string{"changes", "conffilesKept", "conffilesComplete", "rebootRequired",
		"rebootRequiredPackages", "exitCode"} {
		p.With(k, nil)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out Error
	if err := json.Unmarshal(b, &out); err != nil || len(out.Extensions) != 6 {
		t.Errorf("round trip %s: %+v %v", b, out, err)
	}
}
