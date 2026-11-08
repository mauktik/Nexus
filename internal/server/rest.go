package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
)

// APIPrefix is the path prefix of every catalog route.
const APIPrefix = openapi.Prefix

// Route is one registered REST route. Path is a ServeMux pattern path, such
// as /v1/providers/docker/containers/{name}.
type Route struct {
	Method string
	Path   string
	OpID   string
}

// Routes returns every catalog route registered, sorted by op ID, so the
// OpenAPI contract test (4.1) can compare the document with what is served.
func (s *Server) Routes() []Route { return slices.Clone(s.routes) }

// RouteFor derives an op's route: from Op.Path for provider system, and from
// Kind otherwise. internal/openapi owns the derivation, so the document and
// the routes cannot disagree on one.
func RouteFor(op def.Op) (Route, error) {
	method, path, err := openapi.Route(op)
	if err != nil {
		return Route{}, err
	}
	return Route{Method: method, Path: path, OpID: op.ID}, nil
}

// registerREST registers one route per binding in the registry, and records
// each registration. Every route calls disp, the one Dispatcher New builds:
// it is the only path from a route to a handler.
func (s *Server) registerREST(disp *core.Dispatcher) error {
	for _, e := range s.reg.Entries() {
		op := e.Binding.Op()
		rt, err := RouteFor(op)
		if err != nil {
			return err
		}
		names := openapi.Wildcards(rt.Path)
		fields := inputFields(op.InputType())
		for _, n := range names {
			if _, ok := fields[n]; !ok {
				return fmt.Errorf("op %q: route wildcard {%s} is not an input field", op.ID, n)
			}
		}
		if err := handle(s.mux, rt.Method+" "+rt.Path, s.opHandler(disp, op, names, fields)); err != nil {
			return fmt.Errorf("op %q: %w", op.ID, err)
		}
		s.routes = append(s.routes, rt)
	}
	doc := &openAPIHandler{reg: s.reg, log: s.log}
	// Built once now, so a declaration the builder rejects stops startup.
	if _, err := doc.document(); err != nil {
		return fmt.Errorf("openapi: %w", err)
	}
	s.mux.Handle("GET "+openapi.Path, doc)
	return nil
}

// openAPIHandler serves /v1/openapi.json, outside the catalog: any valid
// token may read it (A-50), and the authentication middleware has already
// required one. The document covers the ops of enabled providers, so it is
// rebuilt when a reload changes provider enablement; between changes the
// same bytes are served.
type openAPIHandler struct {
	reg   *core.Registry
	log   *slog.Logger
	cache atomic.Pointer[openAPIDoc]
}

// openAPIDoc is the document built for one build version and set of
// enabled providers, its key.
type openAPIDoc struct {
	key  string
	body []byte
}

func (h *openAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := h.document()
	if err != nil {
		h.log.Error("building the OpenAPI document failed", "error", err.Error())
		writeProblem(h.log, w, r, problem.New(problem.InternalError, "the server failed to handle the request"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		h.log.Debug("response write failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
	}
}

// document returns the document for the current provider enablement,
// building it when that changed. A provider is in it unless configuration
// disables it: an unavailable provider's ops stay documented, as they
// answer capability-unavailable until a re-check finds it working.
func (h *openAPIHandler) document() ([]byte, error) {
	caps := h.reg.Capabilities(context.Background(), false)
	enabled := map[string]bool{def.SystemProvider: true}
	key := caps.Version
	for _, p := range caps.Providers {
		if p.Status != apitypes.ProviderDisabled {
			enabled[p.Name] = true
			key += "\x00" + p.Name
		}
	}
	if d := h.cache.Load(); d != nil && d.key == key {
		return d.body, nil
	}
	var ops []def.Op
	for _, e := range h.reg.Entries() {
		if enabled[e.Provider] {
			ops = append(ops, e.Binding.Op())
		}
	}
	readScopes := map[string]string{}
	for _, spec := range h.reg.Specs() {
		readScopes[spec.Name] = spec.ReadScope
	}
	body, err := openapi.Build(ops, openapi.Options{Version: caps.Version, ReadScopes: readScopes})
	if err != nil {
		return nil, err
	}
	h.cache.Store(&openAPIDoc{key: key, body: body})
	return body, nil
}

// handle registers a pattern, turning ServeMux's panic on a conflicting
// pattern into an error.
func handle(mux *http.ServeMux, pattern string, h http.Handler) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("register %q: %v", pattern, v)
		}
	}()
	mux.Handle(pattern, h)
	return nil
}

// opHandler serves one route: it assembles the op's input from the request
// and calls the Dispatcher, which alone authorizes, checks availability,
// validates and runs the handler. Nothing here decides whether a call may
// proceed.
func (s *Server) opHandler(disp *core.Dispatcher, op def.Op, names []string, fields map[string]reflect.Type) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A request without a principal cannot pass the authentication
		// middleware; if one ever did, the Dispatcher refuses the zero value.
		caller, _ := auth.PrincipalFrom(r.Context())
		raw, inputErr := buildInput(r, names, fields)
		var target string
		if len(names) > 0 {
			target = r.PathValue(names[0])
		}
		res, err := disp.Invoke(r.Context(), core.InvokeRequest{
			Caller: caller, Surface: core.SurfaceREST, Op: op, Target: target,
			Input: raw, InputError: inputErr, IdempotencyKey: idempotencyKey(r.Header),
		})
		switch {
		case err != nil:
			s.writeError(w, r, op, err)
		case res.Stream != nil:
			s.stream(w, r, op, res.Stream)
		default:
			if res.Status == http.StatusAccepted && res.OperationID != "" {
				// RFC 9110 15.3.3: a 202 points to a status monitor.
				w.Header().Set("Location", ops.LocationPrefix+res.OperationID)
				w.Header().Set("Retry-After", strconv.Itoa(ops.RetryAfterSeconds))
			}
			s.writeJSON(w, r, op, res.Status, res.Body)
		}
	})
}

// idempotencyKey reads the Idempotency-Key header. The IETF draft makes its
// value a Structured Field String, so a quoted value is unquoted; a bare
// token is taken as it is.
func idempotencyKey(h http.Header) string {
	v := strings.TrimSpace(h.Get("Idempotency-Key"))
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
	}
	return v
}

// writeError sends a *problem.Error as is. Any other error is an internal
// error: its cause goes only to the log, so no detail reaches the client.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, op def.Op, err error) {
	var p *problem.Error
	if errors.As(err, &p) {
		writeProblem(s.log, w, r, p)
		return
	}
	s.log.Error("operation failed", "op", op.ID, "error", err.Error())
	writeProblem(s.log, w, r, problem.New(problem.InternalError, "the server failed to handle the request"))
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, op def.Op, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.writeError(w, r, op, fmt.Errorf("marshal response: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	s.writeBody(w, r, body)
}

// stream serves a ModeStream result as NDJSON, one apitypes.Frame per line.
// Before each write it extends the write deadline, so the server's
// WriteTimeout never cuts a live follow while a stalled client is dropped.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, op def.Op,
	run func(context.Context, func(apitypes.Frame) error) error) {
	rc := http.NewResponseController(w)
	started := false
	start := func() error {
		if err := rc.SetWriteDeadline(time.Now().Add(StreamWriteDeadline)); err != nil {
			return fmt.Errorf("extend write deadline: %w", err)
		}
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		return nil
	}
	emit := func(f apitypes.Frame) error {
		line, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("marshal frame: %w", err)
		}
		if err := start(); err != nil {
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return fmt.Errorf("write frame: %w", err)
		}
		if err := rc.Flush(); err != nil {
			return fmt.Errorf("flush frame: %w", err)
		}
		return nil
	}
	err := run(r.Context(), emit)
	switch {
	case err != nil && !started:
		s.writeError(w, r, op, err)
	case err != nil:
		// Headers are sent; the client sees the stream end.
		if r.Context().Err() == nil {
			s.log.Warn("stream ended with an error", "op", op.ID, "error", err.Error())
		}
	case !started:
		if err := start(); err != nil {
			s.log.Debug("stream start failed", "op", op.ID, "error", err.Error())
		}
	}
}

// buildInput assembles an op's JSON input: query parameters for GET, the JSON
// object body for POST, and the route wildcards, which name input fields. It
// refuses nothing: an input it cannot assemble, an oversize body included
// (payload-too-large), comes back as inputErr for the Dispatcher to report
// at its validation step, after the scope and capability checks, so a
// refused mutating request is audited (FR-24). The wildcards are always
// present in raw, so the Dispatcher can authorize by target.
func buildInput(r *http.Request, names []string, fields map[string]reflect.Type) (raw json.RawMessage, inputErr *problem.Error) {
	var tooLarge *problem.Error
	obj := map[string]any{}
	switch r.Method {
	case http.MethodGet:
		for key, vals := range r.URL.Query() {
			t, ok := fields[key]
			if !ok || slices.Contains(names, key) {
				inputErr = invalid(key, "unknown query parameter")
				continue
			}
			v, err := queryValue(t, vals)
			if err != nil {
				inputErr = invalid(key, err.Error())
				continue
			}
			obj[key] = v
		}
	default:
		if r.URL.RawQuery != "" {
			inputErr = invalid("", fmt.Sprintf("query parameters are not accepted on %s; send the input as a JSON body", r.Method))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				tooLarge = payloadTooLarge(mbe.Limit)
			} else {
				inputErr = invalid("", "cannot read the request body")
			}
			body = nil
		}
		if len(bytes.TrimSpace(body)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.UseNumber()
			if err := dec.Decode(&obj); err != nil || obj == nil {
				obj, inputErr = map[string]any{}, invalid("", "the request body must be a JSON object")
			} else if _, err := dec.Token(); !errors.Is(err, io.EOF) {
				obj, inputErr = map[string]any{}, invalid("", "the request body must hold a single JSON object")
			}
		}
	}
	for _, n := range names {
		v := r.PathValue(n)
		if prev, ok := obj[n]; ok && prev != v {
			inputErr = invalid(n, "differs from the path")
		}
		obj[n] = v
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, invalid("", "cannot encode the input")
	}
	if tooLarge != nil {
		// It outranks any other input error: the body was never seen.
		inputErr = tooLarge
	}
	return raw, inputErr
}

// invalid is a validation failure of one input field; an empty field means
// the input as a whole. The message never quotes a value.
func invalid(field, msg string) *problem.Error {
	path := ""
	if field != "" {
		path = "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(field)
	}
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}

// inputFields maps each JSON field name of an input type to its Go type.
func inputFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return fields
	}
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || (f.Anonymous && f.Type.Kind() == reflect.Struct) {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		fields[name] = f.Type
	}
	return fields
}

// queryValue converts query values to the JSON value the field's type takes.
// A slice field is repeatable; any other field may appear once.
func queryValue(t reflect.Type, vals []string) (any, error) {
	if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
		out := make([]any, 0, len(vals))
		for _, v := range vals {
			x, err := scalar(t.Elem(), v)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	}
	if len(vals) != 1 {
		return nil, errors.New("given more than once")
	}
	return scalar(t, vals[0])
}

func scalar(t reflect.Type, v string) (any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%q is not a boolean", v)
		}
		return b, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", v)
		}
		return n, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a non-negative integer", v)
		}
		return n, nil
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", v)
		}
		return f, nil
	}
	// Strings, and types that decode from a JSON string such as time.Time.
	return v, nil
}
