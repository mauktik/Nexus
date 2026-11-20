// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// nexusd-self-management test cases owned by WI 2.1:
// planning/nexusd-self-management/test-cases.md. The function names keep
// the TC IDs so that `grep TestTCSM` traces a case back to test-cases.md.
package openapi_test

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/problem"
	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// TC-SM-23 (WI 2.1; FR-6): OpenAPI describes a KindUpload op as a POST with query parameters, a binary body and 201
func TestTCSM23_OpenAPIDescribesAKindUploadOpAs(t *testing.T) {
	op := tpUpload.Op()
	method, path, err := openapi.Route(op)
	if err != nil || method != http.MethodPost || path != "/v1/providers/tp/uploads" {
		t.Fatalf("Route = %s %s, %v; want POST /v1/providers/tp/uploads", method, path, err)
	}
	noType := op
	noType.ResourceType = ""
	if _, _, err := openapi.Route(noType); err == nil {
		t.Fatal("an upload op without ResourceType has a route")
	}

	b, err := openapi.Build([]def.Op{op, tpRestart.Op()}, openapi.Options{ReadScopes: map[string]string{"tp": "tp:read"}})
	if err != nil {
		t.Fatal(err)
	}
	d := parse(t, b)
	o := obj(obj(obj(d["paths"])["/v1/providers/tp/uploads"])["post"])
	if o["operationId"] != "tp.uploads.add" {
		t.Fatalf("paths[/v1/providers/tp/uploads].post = %v", o)
	}

	params, _ := o["parameters"].([]any)
	if len(params) != 1 {
		t.Fatalf("parameters = %v, want only signature", params)
	}
	p := obj(params[0])
	if p["name"] != "signature" || p["in"] != "query" || p["required"] != true {
		t.Errorf("parameter = %v, want signature in query, required", p)
	}
	if ml := obj(p["schema"])["maxLength"]; ml != 4096.0 {
		t.Errorf("signature schema %v lost the refined maxLength", p["schema"])
	}

	rb := obj(o["requestBody"])
	if rb["required"] != true {
		t.Errorf("requestBody.required = %v, want true", rb["required"])
	}
	content := obj(rb["content"])
	want := map[string]any{}
	for _, ct := range op.Upload.ContentTypes {
		want[ct] = map[string]any{"schema": map[string]any{"type": "string", "contentMediaType": ct}}
	}
	if !reflect.DeepEqual(content, want) {
		t.Errorf("requestBody.content = %v, want %v", content, want)
	}
	if desc, _ := rb["description"].(string); !strings.Contains(desc, fmt.Sprint(tpUploadMaxBytes)+" bytes") {
		t.Errorf("requestBody.description %q does not state the ceiling of %d bytes", desc, tpUploadMaxBytes)
	}

	responses := obj(o["responses"])
	if responses["200"] != nil {
		t.Error("upload op declares 200")
	}
	created := obj(obj(obj(obj(responses["201"])["content"])["application/json"])["schema"])
	props := obj(created["properties"])
	for name, typ := range map[string]string{"uploadId": "string", "sha256": "string", "sizeBytes": "integer", "expiresAt": "string"} {
		if obj(props[name])["type"] != typ {
			t.Errorf("201 schema %s: %v, want type %s", name, props[name], typ)
		}
	}
	if obj(props["expiresAt"])["format"] != "date-time" {
		t.Errorf("201 expiresAt %v is not a date-time", props["expiresAt"])
	}
	for status, slug := range map[string]problem.Slug{"413": problem.UploadTooLarge, "409": problem.Conflict,
		"507": problem.StorageLimitReached} {
		schema := obj(obj(obj(obj(responses[status])["content"])["application/problem+json"])["schema"])
		if !slices.Contains(problemRefs(t, "upload "+status, schema), slug) {
			t.Errorf("upload %s response does not list %s", status, slug)
		}
	}

	// The other POST kinds keep their JSON body and take no query parameters.
	r := obj(obj(obj(d["paths"])["/v1/providers/tp/things/{name}/restart"])["post"])
	if _, ok := obj(obj(r["requestBody"])["content"])["application/json"]; !ok {
		t.Errorf("resource action lost its JSON body: %v", r["requestBody"])
	}
	for _, rp := range r["parameters"].([]any) {
		if obj(rp)["in"] == "query" {
			t.Errorf("resource action has a query parameter %v", rp)
		}
	}
	if s := obj(obj(r["responses"])["413"])["description"].(string); strings.Contains(s, string(problem.UploadTooLarge)) {
		t.Errorf("resource action lists %s: %s", problem.UploadTooLarge, s)
	}
}

// TC-SM-24 (WI 2.1; FR-6, NFR-7): The document with an upload route validates, and route-versus-document coverage includes it
func TestTCSM24_TheDocumentWithAnUploadRoute(t *testing.T) {
	s := newStack(t)
	body := s.fetch(t)
	if _, ok, msg := validate(t, body); !ok {
		t.Fatalf("served document with an upload route is invalid:\n%s", msg)
	}
	document, served := docRoutes(parse(t, body)), servedRoutes(s.srv)
	if diff := routeDiff(document, served); len(diff) != 0 {
		t.Fatalf("document and routes differ:\n%s", strings.Join(diff, "\n"))
	}
	const upload = "POST /v1/providers/tp/uploads"
	if document[upload] != "tp.uploads.add" || served[upload] != "tp.uploads.add" {
		t.Fatalf("%s: document %q, served %q; want tp.uploads.add in both", upload, document[upload], served[upload])
	}

	// The catalog, which lists the nexusd spec since WI 6.1, documents its
	// upload op validly too.
	if !slices.ContainsFunc(catalog.All(), func(s def.ProviderSpec) bool { return s.Name == nexusdspec.Name }) {
		t.Fatalf("the catalog does not list %s", nexusdspec.Name)
	}
	gen, err := openapi.Build(catalog.Ops(), openapi.Options{Version: "dev", ReadScopes: readScopes()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, msg := validate(t, gen); !ok {
		t.Fatalf("document with the nexusd spec is invalid:\n%s", msg)
	}
	if _, ok := docRoutes(parse(t, gen))["POST /v1/providers/nexusd/uploads"]; !ok {
		t.Fatal("nexusd upload route is not in the document")
	}
}

// TC-SM-25 (WI 2.1; FR-16): Slug-specific extension schemas
func TestTCSM25_SlugSpecificExtensionSchemas(t *testing.T) {
	b, err := openapi.Build(catalog.Ops(), openapi.Options{Version: "dev", ReadScopes: readScopes()})
	if err != nil {
		t.Fatal(err)
	}
	schemas := obj(obj(parse(t, b)["components"])["schemas"])
	props := func(slug problem.Slug) map[string]any {
		return obj(obj(schemas[openapi.ProblemSchemaName(slug)])["properties"])
	}
	typed := func(slug problem.Slug, members map[string]string) {
		t.Helper()
		p := props(slug)
		for m, typ := range members {
			if got := obj(p[m])["type"]; got != typ {
				t.Errorf("%s.%s: type %v, want %s", slug, m, got, typ)
			}
			if desc, _ := obj(p[m])["description"].(string); desc == "" {
				t.Errorf("%s.%s: no description", slug, m)
			}
		}
	}

	errs := obj(props(problem.ConfigInvalid)["errors"])
	items := obj(errs["items"])
	if errs["type"] != "array" || items["type"] != "object" {
		t.Fatalf("config-invalid.errors = %v, want an array of objects", errs)
	}
	ip := obj(items["properties"])
	for m, typ := range map[string]string{"file": "string", "line": "integer", "column": "integer", "path": "string",
		"message": "string"} {
		if got := obj(ip[m])["type"]; got != typ {
			t.Errorf("config-invalid.errors[].%s: type %v, want %s", m, got, typ)
		}
	}
	if req, _ := items["required"].([]any); !slices.Equal(req, []any{"message"}) {
		t.Errorf("config-invalid.errors[] required %v, want [message]", req)
	}
	// validation-failed keeps its FieldError shape.
	vf := obj(obj(props(problem.ValidationFailed)["errors"])["items"])
	if req, _ := vf["required"].([]any); !slices.Equal(req, []any{"path", "message"}) || obj(vf["properties"])["file"] != nil {
		t.Errorf("validation-failed.errors[] changed: %v", vf)
	}

	for _, slug := range []problem.Slug{problem.UpdateFailed, problem.UpdateRollbackFailed} {
		typed(slug, map[string]string{"rolledBack": "boolean", "step": "string", "previousVersion": "string",
			"newVersion": "string"})
	}
	typed(problem.RollbackFailed, map[string]string{"rolledBack": "boolean", "condition": "integer",
		"fromVersion": "string", "toVersion": "string"})
	typed(problem.RestartUnhealthy, map[string]string{"condition": "integer", "version": "string"})
	typed(problem.UploadTooLarge, map[string]string{"maxBytes": "integer"})
	typed(problem.VersionDowngrade, map[string]string{"runningVersion": "string", "packageVersion": "string"})

	// The container slugs keep the description they had before nexusd.
	const containerRolledBack = "Whether the previous container was restored."
	for _, slug := range []problem.Slug{problem.DependencyUnavailable, problem.UpdateHealthFailed} {
		rb := obj(props(slug)["rolledBack"])
		if rb["type"] != "boolean" || rb["description"] != containerRolledBack {
			t.Errorf("%s.rolledBack = %v, want the container description %q", slug, rb, containerRolledBack)
		}
	}
	if d := obj(props(problem.UpdateFailed)["rolledBack"])["description"]; d == containerRolledBack {
		t.Errorf("update-failed.rolledBack uses the container description")
	}
}
