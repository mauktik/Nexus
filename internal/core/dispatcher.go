package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/systemspec"
)

// Surface is the transport a request arrived on.
type Surface string

const (
	SurfaceREST Surface = "rest"
	SurfaceMCP  Surface = "mcp"
)

// ErrTargetNotFound is what a TargetLoader returns when no operation record
// has the ID.
var ErrTargetNotFound = errors.New("target operation not found")

// TargetRecord is what authorization needs from the operation record a
// Scope.FromTarget op names.
type TargetRecord struct {
	Provider   string // Its read scope authorizes get and logs
	StartScope string // Authorizes cancel (OperationControl)
}

// TargetLoader loads the operation record a FromTarget op names. It returns
// ErrTargetNotFound when there is none. 3.2 supplies it from the store.
type TargetLoader func(ctx context.Context, id string) (TargetRecord, error)

// DispatcherOptions configures NewDispatcher.
type DispatcherOptions struct {
	Logger *slog.Logger // Nil means slog.Default()
	// LoadTarget loads operation records for FromTarget ops. Nil means no
	// record exists, so every FromTarget op answers not-found.
	LoadTarget TargetLoader
}

// InvokeRequest is one call of an operation, from any transport.
type InvokeRequest struct {
	Caller  auth.Principal // From the authentication middleware; a zero Principal is refused
	Surface Surface
	// Op names the operation by its ID. The registered declaration, not
	// this copy, decides scope, mode and schema.
	Op def.Op
	// Target is the resource the transport addressed (a route's {name} or
	// {id}). Authorization uses the target named in Input; a Target that
	// differs from it is refused as invalid input.
	Target string
	Input  json.RawMessage // A JSON object; empty means {}
	// InputError is set by a transport that could not assemble Input, such
	// as a malformed body. It is reported at the validation step, so the
	// scope and capability checks still come first.
	InputError *problem.Error
	// IdempotencyKey is used by long-running ops (3.2).
	IdempotencyKey string
}

// InvokeResult is a successful call's answer.
type InvokeResult struct {
	Status      int // 200, or 202 for an accepted operation
	Body        any // Sync output, or apitypes.Operation for 202 and cancel
	OperationID string
	// Stream is non-nil for a ModeStream op; the transport drives it. Every
	// error it returns is a *problem.Error safe to send.
	Stream func(ctx context.Context, emit func(apitypes.Frame) error) error
}

// Dispatcher is the single path from a transport to a provider handler, and
// the only place scopes are enforced. Every call runs, in this order:
// authentication present, scope (loading the target record first for a
// FromTarget op), capability, input validation, then the handler with panic
// containment (Data Flow). Long-running ops continue with idempotency,
// storage, Admit and lanes, which arrive with the operation manager (3.2).
//
// It writes the audit records of these steps (FR-24): denied for every
// failed authentication or scope check, refused for a mutating request any
// later step refuses, and requested, durably, before a sync mutating
// handler runs. A failed requested write refuses the call with
// audit-unavailable; a failed denied or refused write changes nothing,
// because nothing executes, and is logged.
type Dispatcher struct {
	reg        *Registry
	log        *slog.Logger
	audit      *audit.Log // Nil: denials and refusals are logged only, mutations fail closed
	loadTarget TargetLoader
	entries    map[string]Entry
	validators sync.Map // op ID -> *validator
}

// NewDispatcher returns the Dispatcher over every binding in reg.
func NewDispatcher(reg *Registry, opts DispatcherOptions) *Dispatcher {
	d := &Dispatcher{reg: reg, log: opts.Logger, audit: reg.audit, loadTarget: opts.LoadTarget, entries: map[string]Entry{}}
	if d.log == nil {
		d.log = slog.Default()
	}
	for _, e := range reg.Entries() {
		d.entries[e.Binding.Op().ID] = e
	}
	return d
}

const genericDetail = "the server failed to handle the request"

// Invoke runs one call. Every error it returns is a *problem.Error whose
// content is safe to send to the caller: any other handler error becomes a
// generic internal-error, and its cause goes only to the log.
func (d *Dispatcher) Invoke(ctx context.Context, req InvokeRequest) (InvokeResult, error) {
	e, ok := d.entries[req.Op.ID]
	if !ok {
		return InvokeResult{}, problem.New(problem.RouteNotFound, "no such operation")
	}
	op := e.Binding.Op()

	if req.Caller.TokenID == "" {
		d.denied(op, req, problem.Unauthorized, "")
		return InvokeResult{}, problem.New(problem.Unauthorized, "a valid bearer token is required")
	}
	if perr := d.authorize(ctx, op, req); perr != nil {
		return InvokeResult{}, perr
	}
	if ok, st := d.reg.availableForCall(ctx, e.Provider); !ok {
		return InvokeResult{}, d.refuse(op, req, problem.New(problem.CapabilityUnavailable,
			fmt.Sprintf("provider %s is %s", e.Provider, st.State)).
			With("provider", e.Provider).With("reason", st.Reason))
	}
	in, perr := d.validate(op, e.Binding, req)
	if perr != nil {
		return InvokeResult{}, d.refuse(op, req, perr)
	}

	b := e.Binding
	if k := b.Kind(); k == provider.BindSync || k == provider.BindStream {
		if perr := d.requested(op, req); perr != nil {
			return InvokeResult{}, perr
		}
	}
	switch b.Kind() {
	case provider.BindSync:
		out, err := d.callSync(ctx, b, in)
		if err != nil {
			return InvokeResult{}, d.sanitize(ctx, op, err)
		}
		return InvokeResult{Status: 200, Body: out}, nil
	case provider.BindStream:
		return InvokeResult{Status: 200, Stream: func(ctx context.Context, emit func(apitypes.Frame) error) error {
			if err := d.callStream(ctx, b, in, emit); err != nil {
				return d.sanitize(ctx, op, err)
			}
			return nil
		}}, nil
	}
	// Long-running bindings run through the operation manager (3.2), which
	// writes their requested record at Data Flow step 9.
	d.log.Error("no operation manager for a long-running op", "op", op.ID)
	return InvokeResult{}, d.refuse(op, req, problem.New(problem.InternalError, genericDetail))
}

// authorize is the scope check. It is default-deny: a call passes only when
// the caller holds a scope that satisfies the op's requirement, or the op is
// a system op that declares none (A-50). A FromTarget op's requirement comes
// from the record its input names, which is loaded first.
func (d *Dispatcher) authorize(ctx context.Context, op def.Op, req InvokeRequest) *problem.Error {
	held := req.Caller.Scopes
	target, _ := inputString(req.Input, targetField(op))

	if op.Scope.FromTarget {
		rec, perr := d.loadRecord(ctx, op, target)
		if perr != nil {
			return d.refuse(op, req, perr)
		}
		need := d.reg.readScope(rec.Provider)
		if op.OperationControl {
			need = rec.StartScope
		}
		if auth.HasScope(held, need) {
			return nil
		}
		return d.forbid(op, req, need)
	}

	if op.Scope.Name == "" && op.Scope.PerTarget == "" {
		if op.Provider == def.SystemProvider {
			return nil
		}
		// A provider op that declares no scope still needs its provider's
		// read scope; with none, nothing satisfies it.
		need := d.reg.readScope(op.Provider)
		if auth.HasScope(held, need) {
			return nil
		}
		return d.forbid(op, req, need)
	}

	if auth.HasScope(held, op.Scope.Name) {
		return nil
	}
	if perTarget, ok := expand(op.Scope.PerTarget, req.Input); ok && auth.HasScope(held, perTarget) {
		return nil
	}
	need := op.Scope.Name
	if need == "" {
		need = op.Scope.PerTarget
	}
	return d.forbid(op, req, need)
}

func (d *Dispatcher) forbid(op def.Op, req InvokeRequest, need string) *problem.Error {
	d.denied(op, req, problem.Forbidden, need)
	if need == "" {
		return problem.New(problem.Forbidden, "the operation declares no scope, so no token may call it")
	}
	return problem.New(problem.Forbidden, fmt.Sprintf("the token lacks the scope %s", need)).
		With("requiredScope", need)
}

// denied records a call that failed authentication or the scope check, for
// every op, read or mutating (FR-24).
func (d *Dispatcher) denied(op def.Op, req InvokeRequest, slug problem.Slug, need string) {
	d.log.Info("request denied", "op", op.ID, "tokenId", req.Caller.TokenID, "tokenName", req.Caller.Name,
		"surface", string(req.Surface), "problem", string(slug), "requiredScope", need)
	d.note(d.record(op, req, audit.OutcomeDenied, slug))
}

// refuse records a mutating call refused after authorization and returns
// perr. A validation failure is recorded with the failing field paths and
// no parameter values, because unvalidated input cannot be redacted
// reliably. Reads, and what-if requests, are not audited (Data Flow).
func (d *Dispatcher) refuse(op def.Op, req InvokeRequest, perr *problem.Error) *problem.Error {
	if !audited(op, req) {
		return perr
	}
	d.log.Info("request refused", "op", op.ID, "tokenId", req.Caller.TokenID, "tokenName", req.Caller.Name,
		"surface", string(req.Surface), "problem", string(perr.Slug()))
	rec := d.record(op, req, audit.OutcomeRefused, perr.Slug())
	if perr.Slug() == problem.ValidationFailed {
		rec.InvalidFields = fieldPaths(perr)
	}
	d.note(rec)
	return perr
}

// requested durably records a validated mutating call before its handler
// runs. When the record cannot be written the call is refused with
// audit-unavailable: the audit log fails closed (FR-24).
func (d *Dispatcher) requested(op def.Op, req InvokeRequest) *problem.Error {
	if !audited(op, req) {
		return nil
	}
	unavailable := problem.New(problem.AuditUnavailable, "the audit record could not be written, so the request was not carried out")
	if d.audit == nil {
		d.log.Error("audit write failed", "op", op.ID, "outcome", audit.OutcomeRequested, "error", "no audit log")
		return unavailable
	}
	rec := d.record(op, req, audit.OutcomeRequested, "")
	rec.Params = req.Input
	if len(bytes.TrimSpace(rec.Params)) == 0 {
		rec.Params = json.RawMessage("{}")
	}
	if _, err := d.audit.Write(rec, op.Sensitive); err != nil {
		d.log.Error("audit write failed", "op", op.ID, "outcome", audit.OutcomeRequested, "error", err.Error())
		return unavailable
	}
	return nil
}

// note writes a record whose failure cannot change the response: it is
// logged, and the audit log marks itself unhealthy.
func (d *Dispatcher) note(rec systemspec.AuditRecord) {
	if d.audit == nil {
		return
	}
	if _, err := d.audit.Write(rec, nil); err != nil {
		d.log.Error("audit write failed", "op", rec.Action, "outcome", rec.Outcome, "problem", rec.Problem,
			"error", err.Error())
	}
}

// record is the audit record of a call, without parameters. Target is set
// only when it passes the op's schema for that field.
func (d *Dispatcher) record(op def.Op, req InvokeRequest, outcome string, slug problem.Slug) systemspec.AuditRecord {
	rec := systemspec.AuditRecord{
		TokenName: req.Caller.Name, TokenID: req.Caller.TokenID, Surface: string(req.Surface),
		Provider: op.Provider, Action: op.ID, Target: d.validTarget(op, req),
		Outcome: outcome, Problem: string(slug),
	}
	if req.Caller.Source.IsValid() {
		rec.Source = req.Caller.Source.String()
	}
	if op.Scope.FromTarget {
		// The target of a FromTarget op is an operation ID.
		rec.OperationID = rec.Target
	}
	return rec
}

// audited reports whether a call writes requested and refused records:
// mutating ops, except a what-if request, which mutates nothing (Data
// Flow, read-risk requests).
func audited(op def.Op, req InvokeRequest) bool {
	if !op.Mutating() {
		return false
	}
	if op.WhatIf {
		var in struct {
			WhatIf json.RawMessage `json:"whatIf"`
		}
		if json.Unmarshal(req.Input, &in) == nil && string(bytes.TrimSpace(in.WhatIf)) == "true" {
			return false
		}
	}
	return true
}

// validTarget is the value of the op's target field when it passes the
// schema of that field, else "". It is never a sensitive field.
func (d *Dispatcher) validTarget(op def.Op, req InvokeRequest) string {
	f := targetField(op)
	if f == "" || slices.Contains(op.Sensitive, f) {
		return ""
	}
	v, ok := inputString(req.Input, f)
	if !ok || v == "" {
		return ""
	}
	val, err := d.validatorFor(op)
	if err != nil {
		return ""
	}
	if r := val.props[f]; r == nil || r.Validate(v) != nil {
		return ""
	}
	return v
}

// maxAuditedFields and maxAuditedPathLen bound what a validation refusal
// records: an unknown property's path is a caller-chosen key.
const (
	maxAuditedFields  = 32
	maxAuditedPathLen = 128
)

// fieldPaths lists the JSON Pointers of a validation failure's errors[].
func fieldPaths(p *problem.Error) []string {
	fe, _ := p.Extensions["errors"].([]problem.FieldError)
	paths := make([]string, 0, min(len(fe), maxAuditedFields))
	for _, e := range fe {
		if len(paths) == maxAuditedFields {
			break
		}
		path := e.Path
		if len(path) > maxAuditedPathLen {
			path = strings.ToValidUTF8(path[:maxAuditedPathLen], "") + "..."
		}
		paths = append(paths, path)
	}
	return paths
}

func (d *Dispatcher) loadRecord(ctx context.Context, op def.Op, id string) (TargetRecord, *problem.Error) {
	notFound := problem.New(problem.NotFound, "the operation does not exist")
	if d.loadTarget == nil || id == "" {
		return TargetRecord{}, notFound
	}
	rec, err := d.loadTarget(ctx, id)
	switch {
	case errors.Is(err, ErrTargetNotFound):
		return TargetRecord{}, notFound
	case err != nil:
		d.log.Error("cannot load the target operation", "op", op.ID, "error", err.Error())
		return TargetRecord{}, problem.New(problem.InternalError, genericDetail)
	}
	return rec, nil
}

// readScope is a provider's read scope, or "" when it has none.
func (r *Registry) readScope(name string) string {
	for _, p := range r.providers {
		if s := p.Spec(); s.Name == name {
			return s.ReadScope
		}
	}
	return ""
}

var placeholderRE = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

// targetField is the input field that names an op's target: the wildcard
// of a system op's Path, or name for ops addressed by resource name.
func targetField(op def.Op) string {
	if op.Path != "" {
		if m := placeholderRE.FindStringSubmatch(op.Path); m != nil {
			return m[1]
		}
		return ""
	}
	switch op.Kind {
	case def.KindGet, def.KindSubresource, def.KindResourceAction:
		return "name"
	}
	return ""
}

// expand fills a PerTarget template such as scripts:run:{name} from the
// input's string fields. It fails when a field is absent, empty or not a
// string, so only a well-formed name can satisfy a per-target scope.
func expand(tmpl string, input json.RawMessage) (string, bool) {
	if tmpl == "" {
		return "", false
	}
	ok := true
	out := placeholderRE.ReplaceAllStringFunc(tmpl, func(m string) string {
		v, found := inputString(input, m[1:len(m)-1])
		if !found || v == "" {
			ok = false
		}
		return v
	})
	return out, ok
}

// inputString reads one top-level string field of unvalidated input, the
// way the strict decoder will read it: an exact key, the last one wins.
func inputString(input json.RawMessage, field string) (string, bool) {
	if field == "" {
		return "", false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(input, &obj) != nil {
		return "", false
	}
	raw, ok := obj[field]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// validate checks the input against the op's resolved schema and decodes
// it. Every failure is validation-failed with a non-empty errors[] whose
// messages never echo an input value.
func (d *Dispatcher) validate(op def.Op, b provider.Binding, req InvokeRequest) (any, *problem.Error) {
	if req.InputError != nil {
		return nil, withErrors(req.InputError)
	}
	raw := req.Input
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, invalidInput(problem.FieldError{Path: "", Message: "the input is not valid JSON"})
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, invalidInput(problem.FieldError{Path: "", Message: "the input must be a JSON object"})
	}
	val, err := d.validatorFor(op)
	if err != nil {
		d.log.Error("input schema unavailable", "op", op.ID, "error", err.Error())
		return nil, problem.New(problem.InternalError, genericDetail)
	}
	if errs := val.check(obj); len(errs) > 0 {
		return nil, invalidInput(errs...)
	}
	if f := targetField(op); req.Target != "" && f != "" {
		if s, _ := obj[f].(string); s != req.Target {
			return nil, invalidInput(problem.FieldError{Path: pointer(f), Message: "differs from the target the request addressed"})
		}
	}
	in, err := b.Decode(raw)
	if err != nil {
		return nil, invalidInput(problem.FieldError{Path: "", Message: "the input does not match the operation's input type"})
	}
	return in, nil
}

func invalidInput(errs ...problem.FieldError) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").With("errors", errs)
}

// withErrors gives a transport's validation problem the errors[] member
// every validation failure carries.
func withErrors(p *problem.Error) *problem.Error {
	if p.Slug() != problem.ValidationFailed {
		return p
	}
	if fe, ok := p.Extensions["errors"].([]problem.FieldError); ok && len(fe) > 0 {
		return p
	}
	q := *p
	q.Extensions = nil
	return q.With("errors", []problem.FieldError{{Path: "", Message: p.Detail}})
}

// validator holds an op's input schema split by top-level property, so a
// failure can name the field without quoting its value.
type validator struct {
	root     *jsonschema.Resolved
	props    map[string]*jsonschema.Resolved
	describe map[string]string
	required []string
	closed   bool
}

type validatorEntry struct {
	once sync.Once
	v    *validator
	err  error
}

func (d *Dispatcher) validatorFor(op def.Op) (*validator, error) {
	x, _ := d.validators.LoadOrStore(op.ID, &validatorEntry{})
	e := x.(*validatorEntry)
	e.once.Do(func() { e.v, e.err = newValidator(op) })
	return e.v, e.err
}

func newValidator(op def.Op) (*validator, error) {
	root, err := op.InputSchema()
	if err != nil {
		return nil, err
	}
	s := root.Schema()
	v := &validator{
		root: root, props: map[string]*jsonschema.Resolved{}, describe: map[string]string{},
		required: slices.Clone(s.Required),
	}
	if s.AdditionalProperties != nil {
		b, err := json.Marshal(s.AdditionalProperties)
		v.closed = err == nil && string(b) == "false"
	}
	for name, ps := range s.Properties {
		r, err := ps.CloneSchemas().Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("op %q: property %q: %w", op.ID, name, err)
		}
		v.props[name] = r
		v.describe[name] = describe(ps)
	}
	return v, nil
}

// check returns one error per failing top-level field, then falls back to
// the whole schema for anything the per-field checks cannot see.
func (v *validator) check(obj map[string]any) []problem.FieldError {
	var errs []problem.FieldError
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		r, known := v.props[k]
		switch {
		case !known && v.closed:
			errs = append(errs, problem.FieldError{Path: pointer(k), Message: "unknown property"})
		case known && r.Validate(obj[k]) != nil:
			errs = append(errs, problem.FieldError{Path: pointer(k), Message: v.describe[k]})
		}
	}
	for _, k := range v.required {
		if _, ok := obj[k]; !ok {
			errs = append(errs, problem.FieldError{Path: pointer(k), Message: "required property is missing"})
		}
	}
	if len(errs) == 0 && v.root.Validate(obj) != nil {
		errs = append(errs, problem.FieldError{Path: "", Message: "the input does not match the operation's schema"})
	}
	return errs
}

// describe states what a property accepts, from its schema alone.
func describe(s *jsonschema.Schema) string {
	var parts []string
	switch {
	case s.Type != "":
		parts = append(parts, "must be of type "+s.Type)
	case len(s.Types) > 0:
		parts = append(parts, "must be one of the types "+strings.Join(s.Types, ", "))
	default:
		parts = append(parts, "does not match the schema")
	}
	if len(s.Enum) > 0 {
		if b, err := json.Marshal(s.Enum); err == nil {
			parts = append(parts, "one of "+string(b))
		}
	}
	if s.Pattern != "" {
		parts = append(parts, "matching "+s.Pattern)
	}
	if s.Minimum != nil {
		parts = append(parts, fmt.Sprintf("at least %v", *s.Minimum))
	}
	if s.Maximum != nil {
		parts = append(parts, fmt.Sprintf("at most %v", *s.Maximum))
	}
	if s.MinLength != nil {
		parts = append(parts, fmt.Sprintf("at least %d characters", *s.MinLength))
	}
	if s.MaxLength != nil {
		parts = append(parts, fmt.Sprintf("at most %d characters", *s.MaxLength))
	}
	if s.Items != nil {
		parts = append(parts, "with items that "+describe(s.Items))
	}
	return strings.Join(parts, ", ")
}

// pointer is the JSON Pointer (RFC 6901) of a top-level field.
func pointer(field string) string {
	return "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(field)
}

// callSync runs a sync handler. A panic fails this call only: it is logged
// with its stack and answered with a generic internal-error (NFR-3).
func (d *Dispatcher) callSync(ctx context.Context, b provider.Binding, in any) (out any, err error) {
	defer d.contain(b.Op(), &err)
	return b.CallSync(ctx, in)
}

func (d *Dispatcher) callStream(ctx context.Context, b provider.Binding, in any, emit func(apitypes.Frame) error) (err error) {
	defer d.contain(b.Op(), &err)
	return b.CallStream(ctx, in, emit)
}

func (d *Dispatcher) contain(op def.Op, err *error) {
	if v := recover(); v != nil {
		d.log.Error("handler panic", "op", op.ID, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
		*err = problem.New(problem.InternalError, genericDetail)
	}
}

// sanitize passes a handler's *problem.Error through and turns any other
// error into a generic internal-error, logging the cause (FR-8).
func (d *Dispatcher) sanitize(ctx context.Context, op def.Op, err error) *problem.Error {
	var p *problem.Error
	if errors.As(err, &p) {
		return p
	}
	if ctx.Err() != nil {
		// The caller went away; the error is a consequence, not a fault.
		d.log.Debug("operation ended after the caller left", "op", op.ID, "error", err.Error())
	} else {
		d.log.Error("operation failed", "op", op.ID, "error", err.Error())
	}
	return problem.New(problem.InternalError, genericDetail)
}
