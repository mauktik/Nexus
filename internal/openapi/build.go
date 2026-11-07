// Package openapi builds the OpenAPI 3.1 document of the REST API from the
// operation declarations (FR-26, D-6). It also derives every route, so the
// server registers exactly what the document describes: rest.go serves
// Route of each op, and the contract test compares the two sets.
//
// It depends on declarations only (def, apitypes, problem), so the
// documentation generator can build the document from the catalog without
// linking daemon code.
package openapi

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
)

// Version is the OpenAPI Specification version the document declares.
const Version = "3.1.1"

// Prefix is the path prefix of every API route.
const Prefix = "/" + apitypes.APIVersion

// Path is where the document is served.
const Path = Prefix + "/openapi.json"

// SecurityScheme names the bearer token scheme in components.
const SecurityScheme = "bearerAuth"

// IdempotencyKeyMaxLength is the longest Idempotency-Key the Dispatcher
// accepts; a longer one is a validation failure.
const IdempotencyKeyMaxLength = 255

// Media types of request and response bodies.
const (
	mediaJSON    = "application/json"
	mediaNDJSON  = "application/x-ndjson"
	mediaProblem = problem.ContentType
)

const (
	schemaRef     = "#/components/schemas/"
	problemName   = "Problem"
	frameName     = "Frame"
	operationName = "Operation"
)

// ProblemSchemaName is the component schema name of a problem type.
func ProblemSchemaName(slug problem.Slug) string { return problemName + "." + string(slug) }

// Route derives an op's method and path: from Op.Path for provider system,
// and from Kind otherwise. Nothing else decides a route.
func Route(op def.Op) (method, path string, err error) {
	method, err = methodFor(op.Kind)
	if err != nil {
		return "", "", fmt.Errorf("op %q: %w", op.ID, err)
	}
	if op.Provider == def.SystemProvider {
		if !strings.HasPrefix(op.Path, "/") {
			return "", "", fmt.Errorf("op %q: system op needs a Path starting with /", op.ID)
		}
		return method, Prefix + op.Path, nil
	}

	need := func(field, v string) error {
		if v == "" || strings.ContainsAny(v, "/{} ") {
			return fmt.Errorf("op %q: %s %q is not a route segment", op.ID, field, v)
		}
		return nil
	}
	if err := need("Provider", op.Provider); err != nil {
		return "", "", err
	}
	base := Prefix + "/providers/" + op.Provider
	switch op.Kind {
	case def.KindProviderAction:
		if err := need("Action", op.Action); err != nil {
			return "", "", err
		}
		return method, base + "/actions/" + op.Action, nil
	}
	if err := need("ResourceType", op.ResourceType); err != nil {
		return "", "", err
	}
	path = base + "/" + op.ResourceType
	if op.Kind != def.KindList {
		path += "/{name}"
	}
	if op.Kind == def.KindSubresource || op.Kind == def.KindResourceAction {
		if err := need("Action", op.Action); err != nil {
			return "", "", err
		}
		path += "/" + op.Action
	}
	return method, path, nil
}

func methodFor(k def.Kind) (string, error) {
	switch k {
	case def.KindList, def.KindGet, def.KindSubresource:
		return http.MethodGet, nil
	case def.KindResourceAction, def.KindProviderAction:
		return http.MethodPost, nil
	}
	return "", fmt.Errorf("unknown Kind %q", k)
}

var wildcardRE = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

// Wildcards returns a route path's wildcard names in order.
func Wildcards(path string) []string {
	var names []string
	for _, m := range wildcardRE.FindAllStringSubmatch(path, -1) {
		names = append(names, m[1])
	}
	return names
}

// Options configures Build.
type Options struct {
	// Version is info.version, the nexusd build version. Empty means the
	// API version.
	Version string
	// ReadScopes maps a provider name to its read scope, which an op that
	// declares no scope requires.
	ReadScopes map[string]string
}

// Build returns the document for ops as JSON. The output depends only on
// the set of ops and opts: ops are taken in operationId order and every map
// is written with sorted keys, so equal inputs give identical bytes.
func Build(ops []def.Op, opts Options) ([]byte, error) {
	if opts.Version == "" {
		opts.Version = apitypes.APIVersion
	}
	ops = slices.Clone(ops)
	slices.SortFunc(ops, func(a, b def.Op) int { return cmp.Compare(a.ID, b.ID) })

	schemas, err := componentSchemas()
	if err != nil {
		return nil, err
	}
	d := document{
		OpenAPI: Version,
		Info: info{
			Title:   "Nexus API",
			Version: opts.Version,
			Description: "The REST API of one nexusd host. Every request needs a bearer token; each operation's " +
				"x-required-scope says which scope it needs. Errors are RFC 9457 problem details whose type is " +
				problem.TypePrefix + "<slug>. A long-running operation answers 202 with an operation to poll " +
				"at its Location.",
		},
		Paths: map[string]map[string]*operation{},
		Components: components{
			Schemas: schemas,
			SecuritySchemes: map[string]securityScheme{SecurityScheme: {
				Type: "http", Scheme: "bearer",
				Description: "A Nexus token, created on the host with nexusd token add: Authorization: Bearer <token>.",
			}},
		},
		Security: []map[string][]string{{SecurityScheme: {}}},
	}
	for _, op := range ops {
		method, path, err := Route(op)
		if err != nil {
			return nil, err
		}
		o, err := buildOperation(op, method, path, opts.ReadScopes)
		if err != nil {
			return nil, err
		}
		item := d.Paths[path]
		if item == nil {
			item = map[string]*operation{}
			d.Paths[path] = item
		}
		m := strings.ToLower(method)
		if prev := item[m]; prev != nil {
			return nil, fmt.Errorf("ops %q and %q share the route %s %s", prev.OperationID, op.ID, method, path)
		}
		item[m] = o
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("encode the OpenAPI document: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

type document struct {
	OpenAPI    string                           `json:"openapi"`
	Info       info                             `json:"info"`
	Paths      map[string]map[string]*operation `json:"paths"`
	Components components                       `json:"components"`
	Security   []map[string][]string            `json:"security"`
}

type info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type components struct {
	Schemas         map[string]*jsonschema.Schema `json:"schemas"`
	SecuritySchemes map[string]securityScheme     `json:"securitySchemes"`
}

type securityScheme struct {
	Type        string `json:"type"`
	Scheme      string `json:"scheme"`
	Description string `json:"description"`
}

type operation struct {
	OperationID   string              `json:"operationId"`
	Summary       string              `json:"summary,omitempty"`
	Description   string              `json:"description"`
	Tags          []string            `json:"tags"`
	Parameters    []parameter         `json:"parameters"`
	RequestBody   *requestBody        `json:"requestBody,omitempty"`
	Responses     map[string]response `json:"responses"`
	RequiredScope requiredScope       `json:"x-required-scope"`
	Risk          def.Risk            `json:"x-risk"`
	LongRunning   bool                `json:"x-long-running"`
}

type parameter struct {
	Name        string             `json:"name"`
	In          string             `json:"in"`
	Description string             `json:"description,omitempty"`
	Required    bool               `json:"required,omitempty"`
	Schema      *jsonschema.Schema `json:"schema"`
}

type requestBody struct {
	Required bool                 `json:"required"`
	Content  map[string]mediaType `json:"content"`
}

type mediaType struct {
	Schema *jsonschema.Schema `json:"schema"`
}

type response struct {
	Description string               `json:"description"`
	Headers     map[string]header    `json:"headers,omitempty"`
	Content     map[string]mediaType `json:"content,omitempty"`
}

type header struct {
	Description string             `json:"description"`
	Schema      *jsonschema.Schema `json:"schema"`
}

// requiredScope is x-required-scope: the scopes that authorize the op, any
// one of them sufficing, or for a FromTarget op the rule that picks the
// scope from the target operation. Empty scopes without fromTarget means
// any valid token.
type requiredScope struct {
	Scopes     []string `json:"scopes"`
	FromTarget bool     `json:"fromTarget,omitempty"`
	Rule       string   `json:"rule"`
}

func buildOperation(op def.Op, method, path string, readScopes map[string]string) (*operation, error) {
	if strings.TrimSpace(op.Description) == "" {
		return nil, fmt.Errorf("op %q: empty description", op.ID)
	}
	resolved, err := op.InputSchema()
	if err != nil {
		return nil, err
	}
	in := resolved.Schema()
	names := Wildcards(path)
	o := &operation{
		OperationID:   op.ID,
		Summary:       op.Summary,
		Description:   op.Description,
		Tags:          []string{op.Provider},
		Parameters:    []parameter{},
		RequiredScope: scopeOf(op, readScopes),
		Risk:          op.Risk,
		LongRunning:   op.Mode == def.ModeLongRunning,
	}

	for _, n := range names {
		prop := in.Properties[n]
		if prop == nil {
			return nil, fmt.Errorf("op %q: route wildcard {%s} is not an input field", op.ID, n)
		}
		o.Parameters = append(o.Parameters, parameter{Name: n, In: "path", Required: true,
			Description: paramDescription(op, n, prop), Schema: scalarSchema(prop)})
	}
	if method == http.MethodGet {
		for _, n := range slices.Sorted(maps.Keys(in.Properties)) {
			if slices.Contains(names, n) {
				continue
			}
			prop := in.Properties[n]
			o.Parameters = append(o.Parameters, parameter{Name: n, In: "query", Required: slices.Contains(in.Required, n),
				Description: paramDescription(op, n, prop), Schema: scalarSchema(prop)})
		}
	} else if body := bodySchema(in, names); body != nil {
		o.RequestBody = &requestBody{Required: len(body.Required) > 0,
			Content: map[string]mediaType{mediaJSON: {Schema: body}}}
	}
	if op.Mode == def.ModeLongRunning && op.Mutating() {
		n := IdempotencyKeyMaxLength
		o.Parameters = append(o.Parameters, parameter{Name: "Idempotency-Key", In: "header",
			Description: "Makes a retry safe: a repeat with the same key and request returns the original " +
				"operation, and the same key with a different request is refused with idempotency-key-reused.",
			Schema: &jsonschema.Schema{Type: "string", MaxLength: &n}})
	}

	o.Responses, err = responses(op, len(names) > 0)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// paramDescription is a parameter's description: the field's own, plus the
// declared keys for a list's filter parameter.
func paramDescription(op def.Op, name string, prop *jsonschema.Schema) string {
	desc := prop.Description
	if name == "filter" && len(op.Filters) > 0 {
		if desc == "" {
			desc = "Repeatable key=value filter; an undeclared key is refused."
		}
		keys := make([]string, len(op.Filters))
		for i, f := range op.Filters {
			keys[i] = f.Key + " (" + strings.TrimSuffix(f.Description, ".") + ")"
		}
		desc += " Keys: " + strings.Join(keys, "; ") + "."
	}
	if desc == "" && op.Scope.FromTarget {
		desc = "Operation ID."
	}
	return desc
}

// scalarSchema is a field schema without "null": a parameter is either
// given or absent.
func scalarSchema(prop *jsonschema.Schema) *jsonschema.Schema {
	s := prop.CloneSchemas()
	s.Description = ""
	if i := slices.Index(s.Types, "null"); i >= 0 {
		s.Types = slices.Delete(slices.Clone(s.Types), i, i+1)
		if len(s.Types) == 1 {
			s.Type, s.Types = s.Types[0], nil
		}
	}
	return s
}

// bodySchema is the POST body: the input without the fields the path
// supplies. Nil when nothing is left to send.
func bodySchema(in *jsonschema.Schema, wildcards []string) *jsonschema.Schema {
	s := in.CloneSchemas()
	for _, n := range wildcards {
		delete(s.Properties, n)
	}
	s.Required = slices.DeleteFunc(slices.Clone(s.Required), func(n string) bool { return slices.Contains(wildcards, n) })
	s.PropertyOrder = slices.DeleteFunc(slices.Clone(s.PropertyOrder), func(n string) bool { return slices.Contains(wildcards, n) })
	if len(s.Properties) == 0 {
		return nil
	}
	return s
}

// scopeOf states the authorization rule the Dispatcher applies to op.
func scopeOf(op def.Op, readScopes map[string]string) requiredScope {
	switch {
	case op.Scope.FromTarget && op.OperationControl:
		return requiredScope{Scopes: []string{}, FromTarget: true,
			Rule: "Requires the scope that started the operation named in the path (its start scope), or a scope that grants it."}
	case op.Scope.FromTarget:
		return requiredScope{Scopes: []string{}, FromTarget: true,
			Rule: "Requires the read scope of the provider of the operation named in the path."}
	case op.Scope.Name == "" && op.Scope.PerTarget == "":
		if op.Provider == def.SystemProvider {
			return requiredScope{Scopes: []string{}, Rule: "Any valid token; no scope is required."}
		}
		rs := readScopes[op.Provider]
		if rs == "" {
			return requiredScope{Scopes: []string{},
				Rule: fmt.Sprintf("Requires the read scope of provider %s, which declares none, so no token can call it.", op.Provider)}
		}
		return requiredScope{Scopes: []string{rs}, Rule: fmt.Sprintf("Requires %s, the provider's read scope.", rs)}
	case op.Scope.Name == "":
		return requiredScope{Scopes: []string{op.Scope.PerTarget},
			Rule: fmt.Sprintf("Requires %s for the named target.", op.Scope.PerTarget)}
	case op.Scope.PerTarget == "":
		return requiredScope{Scopes: []string{op.Scope.Name}, Rule: fmt.Sprintf("Requires %s.", op.Scope.Name)}
	}
	return requiredScope{Scopes: []string{op.Scope.Name, op.Scope.PerTarget},
		Rule: fmt.Sprintf("Requires %s, or %s for the named target.", op.Scope.Name, op.Scope.PerTarget)}
}

// applicableProblems lists the problem types op can answer with, beyond
// the ones every route can: the Dispatcher pipeline decides which apply.
// Anything a handler adds is covered by the default response.
func applicableProblems(op def.Op, hasTarget bool) []problem.Slug {
	slugs := []problem.Slug{
		problem.ValidationFailed, problem.Unauthorized, problem.RouteNotFound, problem.MethodNotAllowed,
		problem.PayloadTooLarge, problem.RateLimited, problem.InternalError,
	}
	if op.Scope.FromTarget || op.Scope.Name != "" || op.Scope.PerTarget != "" || op.Provider != def.SystemProvider {
		slugs = append(slugs, problem.Forbidden)
	}
	if hasTarget {
		slugs = append(slugs, problem.NotFound)
	}
	if op.Provider != def.SystemProvider {
		// A provider's dependency (the Docker Engine, apt, systemd) can be
		// down while its preflight last passed (NFR-3).
		slugs = append(slugs, problem.CapabilityUnavailable, problem.DependencyUnavailable)
	}
	if op.Mutating() {
		slugs = append(slugs, problem.AuditUnavailable)
	}
	if op.OperationControl {
		slugs = append(slugs, problem.NotCancelable)
	}
	if op.Mode == def.ModeLongRunning {
		slugs = append(slugs, problem.Conflict)
		if op.Mutating() {
			slugs = append(slugs, problem.IdempotencyKeyReused, problem.StorageLimitReached)
		}
	}
	return slugs
}

// problemHeaders are the response headers a problem status carries.
var problemHeaders = map[int]map[string]header{
	http.StatusUnauthorized: {"WWW-Authenticate": {Description: "The bearer challenge.",
		Schema: &jsonschema.Schema{Type: "string"}}},
	http.StatusMethodNotAllowed: {"Allow": {Description: "The methods the path accepts.",
		Schema: &jsonschema.Schema{Type: "string"}}},
	http.StatusTooManyRequests: {"Retry-After": {Description: "Seconds to wait before retrying.",
		Schema: &jsonschema.Schema{Type: "integer", Minimum: ptr(0.0)}}},
}

func responses(op def.Op, hasTarget bool) (map[string]response, error) {
	out := map[string]response{}
	switch op.Mode {
	case def.ModeStream:
		out["200"] = response{
			Description: "NDJSON: one Frame per line, until the output ends or the client disconnects.",
			Content:     map[string]mediaType{mediaNDJSON: {Schema: ref(frameName)}},
		}
	case def.ModeLongRunning:
		result, err := outputSchema(op.OutputType())
		if err != nil {
			return nil, fmt.Errorf("op %q: %w", op.ID, err)
		}
		accepted := &jsonschema.Schema{
			AllOf: []*jsonschema.Schema{ref(operationName)},
			Properties: map[string]*jsonschema.Schema{"result": {
				Description: "The op's output once the operation has Succeeded; null until then.",
				AnyOf:       []*jsonschema.Schema{result, {Type: "null"}},
			}},
		}
		out["202"] = response{
			Description: "Accepted: the operation was created, or an earlier one with the same Idempotency-Key " +
				"is returned. Poll it at Location until its status is terminal.",
			Headers: map[string]header{
				"Location": {Description: "The operation's URL path: " + Prefix + "/operations/<id>.",
					Schema: &jsonschema.Schema{Type: "string", Format: "uri-reference"}},
				"Retry-After": {Description: "Seconds to wait before the first poll.",
					Schema: &jsonschema.Schema{Type: "integer", Minimum: ptr(0.0)}},
			},
			Content: map[string]mediaType{mediaJSON: {Schema: accepted}},
		}
	default:
		s, err := outputSchema(op.OutputType())
		if err != nil {
			return nil, fmt.Errorf("op %q: %w", op.ID, err)
		}
		out["200"] = response{Description: "The result.", Content: map[string]mediaType{mediaJSON: {Schema: s}}}
	}

	byStatus := map[int][]problem.Type{}
	for _, slug := range applicableProblems(op, hasTarget) {
		t, ok := problem.Lookup(slug)
		if !ok || t.Status == 0 {
			return nil, fmt.Errorf("op %q: problem %q is not an HTTP-level type", op.ID, slug)
		}
		byStatus[t.Status] = append(byStatus[t.Status], t)
	}
	for status, types := range byStatus {
		slices.SortFunc(types, func(a, b problem.Type) int { return cmp.Compare(a.Slug, b.Slug) })
		var refs []*jsonschema.Schema
		var slugs []string
		for _, t := range types {
			refs = append(refs, ref(ProblemSchemaName(t.Slug)))
			slugs = append(slugs, string(t.Slug))
		}
		schema := refs[0]
		if len(refs) > 1 {
			schema = &jsonschema.Schema{OneOf: refs}
		}
		out[fmt.Sprint(status)] = response{
			Description: "Problem: " + strings.Join(slugs, ", ") + ".",
			Headers:     problemHeaders[status],
			Content:     map[string]mediaType{mediaProblem: {Schema: schema}},
		}
	}
	out["default"] = response{
		Description: "Any other problem, such as one the provider reports, as problem details of a registered type.",
		Content:     map[string]mediaType{mediaProblem: {Schema: ref(problemName)}},
	}
	return out, nil
}

func ref(name string) *jsonschema.Schema { return &jsonschema.Schema{Ref: schemaRef + name} }

func ptr[T any](v T) *T { return &v }

func constant(v any) *any { return &v }

// falseSchema is the JSON Schema false: nothing validates.
func falseSchema() *jsonschema.Schema { return &jsonschema.Schema{Not: &jsonschema.Schema{}} }

// problemSchema is RFC 9457 problem details as Nexus writes them.
func problemSchema() *jsonschema.Schema {
	str := func(desc string) *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Description: desc} }
	return &jsonschema.Schema{
		Type:        "object",
		Description: "RFC 9457 problem details. Members beyond the standard ones are the type's extensions.",
		Properties: map[string]*jsonschema.Schema{
			"type":     {Type: "string", Format: "uri", Description: problem.TypePrefix + "<slug>."},
			"title":    str("Short summary of the problem type."),
			"status":   {Type: "integer", Description: "HTTP status; absent inside an operation's error."},
			"detail":   str("Explanation of this occurrence."),
			"instance": str("Reference to this occurrence."),
		},
		Required: []string{"type", "title"},
	}
}

func extensionSchema(name string) *jsonschema.Schema {
	if name == "errors" {
		return &jsonschema.Schema{
			Type:        "array",
			Description: "The failing input fields.",
			Items: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"path":    {Type: "string", Description: "JSON Pointer into the input; empty for the input as a whole."},
					"message": {Type: "string"},
				},
				Required: []string{"path", "message"},
			},
		}
	}
	switch name {
	case "exitCode":
		return &jsonschema.Schema{Type: "integer", Description: "The script's exit code; -1 when a signal ended it or it never started."}
	case "durationSeconds":
		return &jsonschema.Schema{Type: "number", Description: "How long the script ran."}
	}
	return &jsonschema.Schema{Type: "string"}
}

// componentSchemas holds the shared schemas: problem details and one
// schema per registered problem type, the Frame of a stream, and the
// Operation of a 202.
func componentSchemas() (map[string]*jsonschema.Schema, error) {
	m := map[string]*jsonschema.Schema{problemName: problemSchema()}
	for _, t := range problem.All() {
		s := &jsonschema.Schema{
			Title: t.Title,
			AllOf: []*jsonschema.Schema{ref(problemName)},
			Properties: map[string]*jsonschema.Schema{
				"type": {Const: constant(t.URI())},
			},
			Required: []string{"type"},
		}
		if t.OperationLevel {
			s.Description = "Appears only inside a failed operation's error."
		} else {
			s.Properties["status"] = &jsonschema.Schema{Const: constant(t.Status)}
			s.Required = append(s.Required, "status")
		}
		for _, ext := range t.Extensions {
			s.Properties[ext] = extensionSchema(ext)
		}
		m[ProblemSchemaName(t.Slug)] = s
	}

	m[frameName] = &jsonschema.Schema{
		Description: "One line of output. Container log frames have no byte offset o.",
		OneOf: []*jsonschema.Schema{
			{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"o": {Type: "integer", Minimum: ptr(0.0), Description: "Absolute byte offset."},
					"t": {Type: "string", Format: "date-time"},
					"s": {Enum: []any{apitypes.StreamStdout, apitypes.StreamStderr}},
					"d": {Type: "string", Description: "Output text; host-originated and untrusted."},
				},
				Required:             []string{"t", "s", "d"},
				AdditionalProperties: falseSchema(),
			},
			{
				Type:        "object",
				Description: "Output lost to the output cap before this offset.",
				Properties: map[string]*jsonschema.Schema{
					"type":      {Const: constant(apitypes.FrameTypeTruncated)},
					"lostBytes": {Type: "integer", Minimum: ptr(0.0)},
				},
				Required:             []string{"type", "lostBytes"},
				AdditionalProperties: falseSchema(),
			},
		},
	}

	opSchema, err := outputSchema(reflect.TypeFor[apitypes.Operation]())
	if err != nil {
		return nil, err
	}
	m[operationName] = opSchema
	return m, nil
}

// wireSchemas are the schemas of types whose JSON is not what their Go
// fields suggest: jsonschema.For reads struct fields and ignores
// MarshalJSON.
var wireSchemas = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[json.RawMessage](): {Description: "Any JSON value."},
	reflect.TypeFor[time.Time]():       {Type: "string", Format: "date-time"},
	reflect.TypeFor[problem.Error]():   problemSchema(),
}

// outputSchema is the wire schema of an output type.
func outputSchema(t reflect.Type) (*jsonschema.Schema, error) {
	if t == nil {
		return nil, fmt.Errorf("output type unknown: op not built by def.Define")
	}
	s, err := jsonschema.ForType(t, &jsonschema.ForOptions{TypeSchemas: wireSchemas})
	if err != nil {
		return nil, fmt.Errorf("output schema of %v: %w", t, err)
	}
	return s, nil
}
