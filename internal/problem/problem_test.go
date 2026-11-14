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

	// 27 types from nexus-server-manager, plus the 13 of nexusd
	// self-management (TC-SM-5: 8 HTTP-level, 5 operation-level), plus the 9
	// of pihole-dns-zones (3 HTTP-level, 6 operation-level): 31 HTTP-level
	// and 18 operation-level.
	all := All()
	if len(all) != 49 {
		t.Errorf("registry has %d types, want 49 (31 HTTP + 18 operation-level)", len(all))
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
	if len(HTTP()) != 31 || len(ops) != 18 {
		t.Errorf("HTTP() = %d, OperationLevel() = %d; want 31 and 18", len(HTTP()), len(ops))
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

// TC-SM-5 (FR-16, FR-4, FR-5, FR-6, FR-7, FR-8, FR-12): the nexusd
// self-management problem types, copied from the Problem types table in
// planning/nexusd-self-management/architecture.md.
var wantSelfManagement = []struct {
	slug           Slug
	status, exit   int
	extensions     []string
	operationLevel bool
}{
	{"config-invalid", 422, 1, []string{"configPath", "errors"}, false},
	{"upload-too-large", 413, 2, []string{"maxBytes"}, false},
	{"upload-not-found", 422, 1, []string{"uploadId"}, false},
	{"update-not-configured", 409, 1, nil, false},
	{"update-signature-invalid", 422, 1, nil, false},
	{"update-package-invalid", 422, 1, []string{"check"}, false},
	{"version-downgrade", 409, 1, []string{"runningVersion", "packageVersion"}, false},
	{"rollback-unavailable", 409, 1, []string{"path"}, false},
	{"config-apply-failed", 0, 1, nil, true},
	{"restart-unhealthy", 0, 1, []string{"condition", "version"}, true},
	{"update-failed", 0, 1, []string{"rolledBack", "step", "previousVersion", "newVersion"}, true},
	{"update-rollback-failed", 0, 1, []string{"rolledBack", "step", "previousVersion", "newVersion"}, true},
	{"rollback-failed", 0, 1, []string{"rolledBack", "condition", "fromVersion", "toVersion"}, true},
}

// The extensions of the types registered before nexusd self-management, and
// of the pihole-dns-zones types merged in beside it; TC-SM-5 requires their
// rows to be unchanged.
var wantExistingExtensions = map[Slug][]string{
	"validation-failed": {"errors"}, "unauthorized": nil, "forbidden": {"requiredScope"},
	"not-found": nil, "route-not-found": nil, "method-not-allowed": nil,
	"conflict": {"blockingOperationId"}, "not-cancelable": {"operationId"}, "payload-too-large": nil,
	"not-updatable": {"reason", "hint"}, "script-integrity-failed": {"check", "path"},
	"compose-files-missing": {"path"}, "idempotency-key-reused": {"operationId"}, "rate-limited": nil,
	"internal-error": nil, "capability-unavailable": {"provider", "reason"},
	"compose-plugin-missing": {"provider"},
	"dependency-unavailable": {"provider", "rolledBack", "previousDigest", "newDigest"},
	"audit-unavailable":      nil, "storage-limit-reached": nil,
	"outcome-unknown": nil, "package-manager-busy": nil, "package-manager-broken": nil,
	"apt-failed": {"changes", "conffilesKept", "conffilesComplete", "rebootRequired",
		"rebootRequiredPackages", "exitCode"},
	"script-failed":        {"exitCode", "signal", "durationSeconds"},
	"script-timeout":       {"exitCode", "signal", "durationSeconds"},
	"update-health-failed": {"rolledBack", "previousDigest", "newDigest"},
	// pihole-dns-zones, merged in from main.
	"dns-file-conflict": {"expectedEtag", "currentEtag"}, "dns-name-unzoned": {"name"},
	"dns-batch-rejected":      {"changeIndex", "cause"},
	"dns-config-test-failed":  {"exitCode", "rolledBack", "preExisting"},
	"dns-verification-failed": {"rolledBack", "name", "expected", "observed"},
	"dns-rollback-failed":     {"rollbackCopy"}, "gravity-failed": {"exitCode"},
	"pihole-command-failed": {"command", "exitCode", "rolledBack"}, "pihole-busy": {"waitedSeconds"},
}

func TestTCSM5_The13NewProblemTypesAre(t *testing.T) {
	if len(wantSelfManagement) != 13 {
		t.Fatalf("test table drifted: %d rows, want 13", len(wantSelfManagement))
	}

	rows := map[Slug]int{}
	for _, typ := range All() {
		rows[typ.Slug]++
	}

	for _, w := range wantSelfManagement {
		t.Run(string(w.slug), func(t *testing.T) {
			typ, ok := Lookup(w.slug)
			if !ok {
				t.Fatalf("%s: missing from registry", w.slug)
			}
			if typ.Status != w.status || typ.Exit != w.exit || typ.OperationLevel != w.operationLevel {
				t.Errorf("status %d exit %d operationLevel %v, want %d %d %v",
					typ.Status, typ.Exit, typ.OperationLevel, w.status, w.exit, w.operationLevel)
			}
			if typ.OperationLevel && typ.Status != 0 {
				t.Errorf("operation-level type has status %d, want 0", typ.Status)
			}
			if !slices.Equal(typ.Extensions, w.extensions) {
				t.Errorf("extensions %v, want %v", typ.Extensions, w.extensions)
			}
			if typ.Title == "" {
				t.Error("empty title")
			}
			if want := "urn:nexus:problem:" + string(w.slug); typ.URI() != want {
				t.Errorf("URI %q, want %q", typ.URI(), want)
			}
			if n := rows[w.slug]; n != 1 {
				t.Errorf("registered %d times, want exactly once", n)
			}

			p := New(w.slug, "detail")
			if p.Type != typ.URI() || p.Status != w.status || p.Detail != "detail" || p.Slug() != w.slug {
				t.Errorf("New = %+v", p)
			}
			for _, ext := range w.extensions {
				p.With(ext, "x")
			}
			if len(p.Extensions) != len(w.extensions) {
				t.Errorf("New(...).With accepted %d of %d declared extensions", len(p.Extensions), len(w.extensions))
			}
		})
	}

	inList := func(list []Type, slug Slug) bool {
		return slices.ContainsFunc(list, func(t Type) bool { return t.Slug == slug })
	}
	for _, w := range wantSelfManagement {
		if inList(OperationLevel(), w.slug) != w.operationLevel || inList(HTTP(), w.slug) == w.operationLevel {
			t.Errorf("%s: listed in the wrong table (operationLevel %v)", w.slug, w.operationLevel)
		}
	}

	// The rows registered before nexusd self-management are unchanged.
	if len(wantExistingExtensions) != len(wantHTTP)+len(wantOperationLevel) {
		t.Fatalf("existing-extensions table has %d rows, want %d", len(wantExistingExtensions), len(wantHTTP)+len(wantOperationLevel))
	}
	for slug, exts := range wantExistingExtensions {
		typ, ok := Lookup(slug)
		if !ok {
			t.Errorf("%s: existing type missing", slug)
			continue
		}
		if !slices.Equal(typ.Extensions, exts) {
			t.Errorf("%s: extensions %v, want unchanged %v", slug, typ.Extensions, exts)
		}
		if n := rows[slug]; n != 1 {
			t.Errorf("%s: registered %d times, want exactly once", slug, n)
		}
	}
}
