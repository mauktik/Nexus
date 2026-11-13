package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
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

// ErrUploadTooLarge is what reading an upload body past its cap returns,
// wrapped. The Dispatcher answers an upload whose Receive fails with it as
// upload-too-large.
var ErrUploadTooLarge = errors.New("upload exceeds the size limit")

// TargetRecord is what authorization needs from the operation record a
// Scope.FromTarget op names.
type TargetRecord struct {
	Provider   string // Its read scope authorizes get and logs
	StartScope string // Authorizes cancel (OperationControl)
}

// TargetLoader loads the operation record a FromTarget op names. It returns
// ErrTargetNotFound when there is none. 3.2 supplies it from the store.
type TargetLoader func(ctx context.Context, id string) (TargetRecord, error)

// AuditWriter is what the Dispatcher needs of the audit log. *audit.Log
// implements it; tests wrap it to inject write failures.
type AuditWriter interface {
	Write(rec systemspec.AuditRecord, sensitive []string) (string, error)
	Redact(provider, action string, params json.RawMessage, sensitive []string) (json.RawMessage, error)
}

// DispatcherOptions configures NewDispatcher.
type DispatcherOptions struct {
	Logger *slog.Logger // Nil means slog.Default()
	// LoadTarget loads operation records for FromTarget ops. Nil means the
	// registry's operation manager loads them; without one, no record
	// exists, so every FromTarget op answers not-found.
	LoadTarget TargetLoader
	// Audit, when non-nil, replaces the registry's audit log for every
	// record the Dispatcher writes.
	Audit AuditWriter
	// NoteLimitPerMinute bounds the denied and pre-request refused records
	// one token writes per minute (SEC-2); the rest of its window collapses
	// into one aggregate record. Zero means DefaultNoteLimitPerMinute.
	NoteLimitPerMinute int
	Now                func() time.Time // The limiter's clock; nil means time.Now
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
	// Upload is the request body of a KindUpload op; other ops ignore it.
	Upload *provider.UploadBody
}

// InvokeResult is a successful call's answer.
type InvokeResult struct {
	Status      int // 200, 201 for a stored upload, or 202 for an accepted operation
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
// containment (Data Flow). Long-running ops continue with idempotency, the
// storage check (the limit flag of internal/storage, through
// Options.StorageCheck), Admit and lanes. Only a new mutation meets the
// storage check: reads, replays, read-risk requests, whatIf and cancel
// continue at the limit (A-68).
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
	audit      AuditWriter // Nil: denials and refusals are logged only, mutations fail closed
	ops        *ops.Manager
	loadTarget TargetLoader
	entries    map[string]Entry
	validators sync.Map // op ID -> *validator
	notes      *noteLimiter
}

// NewDispatcher returns the Dispatcher over every binding in reg.
func NewDispatcher(reg *Registry, opts DispatcherOptions) *Dispatcher {
	d := &Dispatcher{reg: reg, log: opts.Logger, ops: reg.ops, loadTarget: opts.LoadTarget, entries: map[string]Entry{},
		notes: newNoteLimiter(opts.NoteLimitPerMinute, opts.Now)}
	if d.log == nil {
		d.log = slog.Default()
	}
	// No typed nil: a nil *audit.Log stays a nil interface.
	switch {
	case opts.Audit != nil:
		d.audit = opts.Audit
	case reg.audit != nil:
		d.audit = reg.audit
	}
	if d.loadTarget == nil && d.ops != nil {
		d.loadTarget = opsLoader(d.ops)
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
		if _, perr := d.requested(op, req); perr != nil {
			return InvokeResult{}, perr
		}
	}
	// Handlers that act for the caller, such as system.operations.list,
	// read the principal from the context.
	ctx = auth.WithPrincipal(ctx, req.Caller)
	switch b.Kind() {
	case provider.BindSync:
		out, err := d.callSync(ctx, b, in)
		if err != nil {
			// A mutating sync op (system.operations.cancel) that refuses
			// after its request record, such as not-cancelable, is audited
			// refused.
			return InvokeResult{}, d.refuse(op, req, d.sanitize(ctx, op, err))
		}
		return InvokeResult{Status: 200, Body: out}, nil
	case provider.BindStream:
		return InvokeResult{Status: 200, Stream: func(ctx context.Context, emit func(apitypes.Frame) error) error {
			if err := d.callStream(ctx, b, in, emit); err != nil {
				return d.sanitize(ctx, op, err)
			}
			return nil
		}}, nil
	case provider.BindUpload:
		return d.upload(ctx, op, req, b, in)
	}
	if funcs, ok := b.InProcess(); ok && d.ops != nil {
		return d.longRunning(ctx, op, e.Provider, req, in, funcs.Admit, funcs.Lane, ops.Work{
			Run: func(ctx context.Context, r *provider.Run) (any, error) { return funcs.Run(ctx, in, r) },
		})
	}
	if funcs, ok := b.Detached(); ok && d.ops != nil {
		return d.longRunning(ctx, op, e.Provider, req, in, funcs.Admit, funcs.Lane, ops.Work{
			Detached: &ops.DetachedWork{In: in, Decode: b.Decode, Funcs: funcs},
		})
	}
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
	d.noteLimited(req, d.record(op, req, audit.OutcomeDenied, slug))
}

// noteLimited is note for a record any valid token can make the server
// write by failing a check (SEC-2). Past the token's allowance for the
// window the record is counted into one aggregate instead, so the audit
// log's growth from refusals is bounded per token, not per request.
func (d *Dispatcher) noteLimited(req InvokeRequest, rec systemspec.AuditRecord) {
	if d.notes.allow(req.Caller.TokenID) {
		d.note(rec, nil)
		return
	}
	if rl, ok := d.audit.(interface{ RateLimited(source, surface string) }); ok {
		rl.RateLimited("token:"+req.Caller.TokenID, string(req.Surface))
	}
}

// refuse records a mutating call refused after authorization and returns
// perr. A validation failure is recorded with the failing field paths and
// no parameter values, because unvalidated input cannot be redacted
// reliably. Reads, and what-if requests, are not audited (Data Flow).
func (d *Dispatcher) refuse(op def.Op, req InvokeRequest, perr *problem.Error) *problem.Error {
	return d.refuseAfter(op, req, perr, "")
}

// refuseAfter is refuse for a request that already wrote its requested
// record, requestID, which the refused record names so it does not stand
// alone. An empty requestID is a refusal before any requested record.
func (d *Dispatcher) refuseAfter(op def.Op, req InvokeRequest, perr *problem.Error, requestID string) *problem.Error {
	if !audited(op, req) {
		return perr
	}
	d.log.Info("request refused", "op", op.ID, "tokenId", req.Caller.TokenID, "tokenName", req.Caller.Name,
		"surface", string(req.Surface), "problem", string(perr.Slug()))
	rec := d.record(op, req, audit.OutcomeRefused, perr.Slug())
	rec.RequestID = requestID
	if perr.Slug() == problem.ValidationFailed {
		rec.InvalidFields = fieldPaths(perr)
	}
	if requestID == "" {
		d.noteLimited(req, rec)
	} else {
		d.note(rec, nil) // Tied to a requested record the call already wrote
	}
	return perr
}

// requested durably records a validated mutating call before its handler
// runs, and returns the record's id. When the record cannot be written the
// call is refused with audit-unavailable: the audit log fails closed
// (FR-24). This write is also the audit-health check: it is the only way a
// mutation proceeds, and its success clears the flag a failed state-change
// write set.
func (d *Dispatcher) requested(op def.Op, req InvokeRequest) (string, *problem.Error) {
	if !audited(op, req) {
		return "", nil
	}
	unavailable := problem.New(problem.AuditUnavailable, "the audit record could not be written, so the request was not carried out")
	if d.audit == nil {
		d.log.Error("audit write failed", "op", op.ID, "outcome", audit.OutcomeRequested, "error", "no audit log")
		return "", unavailable
	}
	rec := d.record(op, req, audit.OutcomeRequested, "")
	rec.Params = params(req)
	id, err := d.audit.Write(rec, op.Sensitive)
	if err != nil {
		d.log.Error("audit write failed", "op", op.ID, "outcome", audit.OutcomeRequested, "error", err.Error())
		return "", unavailable
	}
	return id, nil
}

// params is a call's input as recorded: validated, and redacted by the
// audit log's one redactor when written.
func params(req InvokeRequest) json.RawMessage {
	if len(bytes.TrimSpace(req.Input)) == 0 {
		return json.RawMessage("{}")
	}
	return req.Input
}

// note writes a record whose failure cannot change the response: it is
// logged, and the audit log marks itself unhealthy. sensitive names the
// paths to redact when the record carries params.
func (d *Dispatcher) note(rec systemspec.AuditRecord, sensitive []string) {
	if d.audit == nil {
		return
	}
	if _, err := d.audit.Write(rec, sensitive); err != nil {
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

// upload is the KindUpload path after authorization, capability and
// validation (Data Flow F4): the Content-Type and the declared length are
// checked before anything else runs, then the storage check, Admit, the
// requested record, and Receive, which streams the body. Nothing reads the
// body before the requested record is written.
func (d *Dispatcher) upload(ctx context.Context, op def.Op, req InvokeRequest, b provider.Binding, in any) (InvokeResult, error) {
	funcs, ok := b.Upload()
	body := req.Upload
	if !ok || op.Upload == nil || body == nil {
		d.log.Error("upload op called without an upload binding or body", "op", op.ID)
		return InvokeResult{}, d.refuse(op, req, problem.New(problem.InternalError, genericDetail))
	}
	if !acceptsContentType(op.Upload.ContentTypes, body.ContentType) {
		return InvokeResult{}, d.refuse(op, req, invalidInput(problem.FieldError{Path: "",
			Message: "Content-Type must be one of " + strings.Join(op.Upload.ContentTypes, ", ")}))
	}
	maxBytes := funcs.MaxBytes()
	if body.ContentLength > maxBytes {
		return InvokeResult{}, d.refuse(op, req, tooLarge(maxBytes))
	}
	if d.reg.storageCheck != nil {
		if perr := d.reg.storageCheck(); perr != nil {
			return InvokeResult{}, d.refuse(op, req, perr)
		}
	}
	var release func()
	err := d.provide(op, func() (err error) { release, err = funcs.Admit(ctx, in); return err })
	// Whatever Admit holds is freed exactly once, on every path from here,
	// after Receive returned or unwound (A-51).
	if release != nil {
		defer release()
	}
	if err != nil {
		return InvokeResult{}, d.refuse(op, req, d.sanitize(ctx, op, err))
	}
	requestID, perr := d.requested(op, req)
	if perr != nil {
		return InvokeResult{}, perr
	}
	if err := ctx.Err(); err != nil {
		return InvokeResult{}, d.refuseAfter(op, req, d.sanitize(ctx, op, err), requestID)
	}
	var out any
	err = d.provide(op, func() (err error) { out, err = funcs.Receive(ctx, in, *body); return err })
	switch {
	case errors.Is(err, ErrUploadTooLarge):
		return InvokeResult{}, d.refuseAfter(op, req, tooLarge(maxBytes), requestID)
	case err != nil:
		return InvokeResult{}, d.refuseAfter(op, req, d.sanitize(ctx, op, err), requestID)
	}
	return InvokeResult{Status: 201, Body: out}, nil
}

// acceptsContentType reports whether a Content-Type header names one of the
// accepted media types. Media types compare case-insensitively, and
// parameters are ignored (RFC 9110, section 8.3.1).
func acceptsContentType(accepted []string, header string) bool {
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(accepted, func(a string) bool { return strings.EqualFold(a, mt) })
}

func tooLarge(maxBytes int64) *problem.Error {
	return problem.New(problem.UploadTooLarge, fmt.Sprintf("the upload is larger than the limit of %d bytes", maxBytes)).
		With("maxBytes", maxBytes)
}

// maxIdempotencyKeyLen bounds an Idempotency-Key, which is stored hashed.
const maxIdempotencyKeyLen = 255

// longRunning is Data Flow steps 4 to 11 for an in-process op, after
// authorization, capability and validation:
//
//  4. idempotency lookup: a hit replays or refuses the reuse, ahead of the
//     storage check and Admit;
//  5. storage check;
//  6. Admit;
//  7. lane resolution, then the lane's mutex, under which run
//  8. the idempotency re-check and the lane conflict check,
//  9. the requested record, and
//  10. the record's creation with its key, where CreateWithIdempotency is
//     the final arbiter of the key;
//  11. 202 with the operation.
//
// A read-risk request (a read-only op such as check-updates, or whatIf)
// skips the storage check, ignores its key, and writes no requested,
// refused or replayed record; scope, Admit and lane rules still apply.
func (d *Dispatcher) longRunning(ctx context.Context, op def.Op, prov string, req InvokeRequest, in any,
	admit func(context.Context, any) error, laneOf func(context.Context, any) (provider.Lane, error),
	work ops.Work) (InvokeResult, error) {
	readRisk := !audited(op, req)
	key := req.IdempotencyKey
	if readRisk {
		key = ""
	}
	if len(key) > maxIdempotencyKeyLen {
		return InvokeResult{}, d.refuse(op, req, invalidInput(problem.FieldError{Path: "",
			Message: fmt.Sprintf("the Idempotency-Key must be at most %d characters", maxIdempotencyKeyLen)}))
	}
	hash := requestHash(op, req.Input)

	// Step 4.
	if key != "" {
		if res, hit, err := d.lookupKey(op, req, key, hash, ""); hit {
			return res, err
		}
	}
	// Step 5.
	if !readRisk && d.reg.storageCheck != nil {
		if perr := d.reg.storageCheck(); perr != nil {
			return InvokeResult{}, d.refuse(op, req, perr)
		}
	}
	// Step 6.
	if err := d.provide(op, func() error { return admit(ctx, in) }); err != nil {
		return InvokeResult{}, d.refuse(op, req, d.sanitize(ctx, op, err))
	}
	// Step 7.
	var lane provider.Lane
	if err := d.provide(op, func() (err error) { lane, err = laneOf(ctx, in); return err }); err != nil {
		return InvokeResult{}, d.refuse(op, req, d.sanitize(ctx, op, err))
	}
	adm := d.ops.Admit(lane)
	defer adm.Release()

	// Step 8.
	if key != "" {
		if res, hit, err := d.lookupKey(op, req, key, hash, ""); hit {
			return res, err
		}
	}
	if blocker, busy := adm.Blocker(); busy {
		return InvokeResult{}, d.refuse(op, req, problem.New(problem.Conflict,
			"another operation holds this operation's lane").With("blockingOperationId", blocker))
	}
	// Step 9.
	requestID, perr := d.requested(op, req)
	if perr != nil {
		return InvokeResult{}, perr
	}
	// Step 10.
	rec, perr := d.newRecord(op, prov, req)
	if perr != nil {
		return InvokeResult{}, d.refuseAfter(op, req, perr, requestID)
	}
	idem := store.IdempotencyKey{}
	if key != "" {
		idem = store.IdempotencyKey{TokenID: req.Caller.TokenID, Key: key, RequestHash: hash}
	}
	work.Op, work.TokenID, work.RequestID = op, req.Caller.TokenID, requestID
	created, err := adm.Create(rec, idem, work)
	if err != nil {
		d.log.Error("cannot create the operation", "op", op.ID, "error", err.Error())
		return InvokeResult{}, d.refuseAfter(op, req, problem.New(problem.InternalError, genericDetail), requestID)
	}
	adm.Release()
	if created.Existing {
		return d.answerExisting(op, req, created.ID, created.RequestHash, hash, requestID)
	}
	// Step 11.
	return d.accepted(op, created.ID)
}

// lookupKey is the idempotency lookup of steps 4 and 8. hit reports that
// the key is stored, and res and err are then the answer.
func (d *Dispatcher) lookupKey(op def.Op, req InvokeRequest, key, hash, requestID string) (res InvokeResult, hit bool, err error) {
	e, ok, lerr := d.ops.LookupIdempotency(req.Caller.TokenID, key)
	if lerr != nil {
		d.log.Error("idempotency lookup failed", "op", op.ID, "error", lerr.Error())
		return InvokeResult{}, true, d.refuseAfter(op, req, problem.New(problem.InternalError, genericDetail), requestID)
	}
	if !ok {
		return InvokeResult{}, false, nil
	}
	res, err = d.answerExisting(op, req, e.OperationID, e.RequestHash, hash, requestID)
	return res, true, err
}

// answerExisting answers a request whose key is already stored: the original
// operation when the request hashes match (a replay, audited replayed with
// the original operation ID), else idempotency-key-reused (audited
// refused). Either record names requestID, the request's own requested
// record when it wrote one before CreateWithIdempotency found the key.
func (d *Dispatcher) answerExisting(op def.Op, req InvokeRequest, opID, storedHash, hash, requestID string) (InvokeResult, error) {
	if storedHash != hash {
		return InvokeResult{}, d.refuseAfter(op, req, problem.New(problem.IdempotencyKeyReused,
			"the Idempotency-Key was already used for a different request").With("operationId", opID), requestID)
	}
	res, err := d.accepted(op, opID)
	if err != nil {
		return InvokeResult{}, d.refuseAfter(op, req, problem.New(problem.InternalError, genericDetail), requestID)
	}
	rec := d.record(op, req, audit.OutcomeReplayed, "")
	rec.Params, rec.OperationID, rec.RequestID = params(req), opID, requestID
	d.note(rec, op.Sensitive)
	return res, nil
}

// accepted is the 202 answer for operation id: the transport derives
// Location from OperationID (A-49).
func (d *Dispatcher) accepted(op def.Op, id string) (InvokeResult, error) {
	rec, err := d.ops.Get(id)
	if err != nil {
		d.log.Error("cannot load the operation", "op", op.ID, "operationId", id, "error", err.Error())
		return InvokeResult{}, problem.New(problem.InternalError, genericDetail)
	}
	return InvokeResult{Status: 202, Body: rec.ToAPI(d.ops.Host()), OperationID: id}, nil
}

// newRecord is the record of a validated request: its input as the one
// redactor leaves it, whether a sensitive value was supplied, and the scope
// that authorized it. Sensitive values stay in memory only.
func (d *Dispatcher) newRecord(op def.Op, prov string, req InvokeRequest) (store.OpRecord, *problem.Error) {
	raw := params(req)
	var redacted json.RawMessage
	var err error
	if d.audit != nil {
		redacted, err = d.audit.Redact(op.Provider, op.ID, raw, op.Sensitive)
	} else {
		redacted, err = audit.Redact(raw, op.Sensitive)
	}
	if err != nil {
		d.log.Error("cannot redact the operation input", "op", op.ID, "error", err.Error())
		return store.OpRecord{}, problem.New(problem.InternalError, genericDetail)
	}
	// Redact re-encodes only when it has paths, so compare canonical forms:
	// they differ exactly when a sensitive value was supplied.
	before, err := canonicalJSON(raw)
	var after json.RawMessage
	if err == nil {
		after, err = canonicalJSON(redacted)
	}
	if err != nil {
		d.log.Error("cannot encode the operation input", "op", op.ID, "error", err.Error())
		return store.OpRecord{}, problem.New(problem.InternalError, genericDetail)
	}
	return store.OpRecord{
		Action: op.ID, Provider: prov, Target: d.validTarget(op, req), TokenName: req.Caller.Name,
		Input: redacted, HasSensitive: !bytes.Equal(before, after),
		Exec: store.ExecInProcess, StartScope: d.startScope(op, req),
	}, nil
}

// startScope is the scope that authorizes the operation's cancel: the
// per-target scope when the op has one, whichever scope the caller held
// (scripts:run:<id>), else the op's scope.
func (d *Dispatcher) startScope(op def.Op, req InvokeRequest) string {
	if s, ok := expand(op.Scope.PerTarget, req.Input); ok {
		return s
	}
	if op.Scope.Name != "" {
		return op.Scope.Name
	}
	return d.reg.readScope(op.Provider)
}

// requestHash identifies a request for replay detection: the op and its
// canonical input, sensitive values included, so a different value is a
// different request. Only the hash is stored.
func requestHash(op def.Op, input json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(op.ID))
	h.Write([]byte{0})
	c, err := canonicalJSON(params(InvokeRequest{Input: input}))
	if err != nil {
		c = input
	}
	h.Write(c)
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON re-encodes validated input with sorted keys and no
// insignificant whitespace.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonical input: %w", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical input: %w", err)
	}
	return b, nil
}

// provide calls provider code at admission (Admit, Lane) with panic
// containment: a panic fails this request only (NFR-3).
func (d *Dispatcher) provide(op def.Op, f func() error) (err error) {
	defer d.contain(op, &err)
	return f()
}

// opsLoader loads FromTarget records from the operation manager.
func opsLoader(m *ops.Manager) TargetLoader {
	return func(_ context.Context, id string) (TargetRecord, error) {
		rec, err := m.Get(id)
		if errors.Is(err, store.ErrNotFound) {
			return TargetRecord{}, ErrTargetNotFound
		}
		if err != nil {
			return TargetRecord{}, err
		}
		return TargetRecord{Provider: rec.Provider, StartScope: rec.StartScope}, nil
	}
}
