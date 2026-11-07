package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
)

// APIPrefix is the path prefix of every catalog route.
const APIPrefix = "/" + apitypes.APIVersion

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
// Kind otherwise. Nothing else decides a route.
func RouteFor(op def.Op) (Route, error) {
	method, err := methodFor(op.Kind)
	if err != nil {
		return Route{}, fmt.Errorf("op %q: %w", op.ID, err)
	}
	if op.Provider == def.SystemProvider {
		if !strings.HasPrefix(op.Path, "/") {
			return Route{}, fmt.Errorf("op %q: system op needs a Path starting with /", op.ID)
		}
		return Route{Method: method, Path: APIPrefix + op.Path, OpID: op.ID}, nil
	}

	need := func(field, v string) error {
		if v == "" || strings.ContainsAny(v, "/{} ") {
			return fmt.Errorf("op %q: %s %q is not a route segment", op.ID, field, v)
		}
		return nil
	}
	if err := need("Provider", op.Provider); err != nil {
		return Route{}, err
	}
	base := APIPrefix + "/providers/" + op.Provider
	var path string
	switch op.Kind {
	case def.KindProviderAction:
		if err := need("Action", op.Action); err != nil {
			return Route{}, err
		}
		path = base + "/actions/" + op.Action
	default:
		if err := need("ResourceType", op.ResourceType); err != nil {
			return Route{}, err
		}
		path = base + "/" + op.ResourceType
		if op.Kind != def.KindList {
			path += "/{name}"
		}
		if op.Kind == def.KindSubresource || op.Kind == def.KindResourceAction {
			if err := need("Action", op.Action); err != nil {
				return Route{}, err
			}
			path += "/" + op.Action
		}
	}
	return Route{Method: method, Path: path, OpID: op.ID}, nil
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

// wildcards returns the path's wildcard names in order.
func wildcards(path string) []string {
	var names []string
	for _, m := range wildcardRE.FindAllStringSubmatch(path, -1) {
		names = append(names, m[1])
	}
	return names
}

// registerREST registers one route per binding in the registry, and records
// each registration. Every route calls the one Dispatcher built here: it is
// the only path from a route to a handler.
func (s *Server) registerREST() error {
	disp := core.NewDispatcher(s.reg, core.DispatcherOptions{Logger: s.log})
	for _, e := range s.reg.Entries() {
		op := e.Binding.Op()
		rt, err := RouteFor(op)
		if err != nil {
			return err
		}
		names := wildcards(rt.Path)
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
	return nil
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
		raw, inputErr, tooLarge := buildInput(r, names, fields)
		if tooLarge != nil {
			writeProblem(s.log, w, r, tooLarge)
			return
		}
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
// does not refuse anything except an oversize body: an input it cannot
// assemble comes back as inputErr for the Dispatcher to report at its
// validation step, after the scope and capability checks. The wildcards are
// always present in raw, so the Dispatcher can authorize by target.
func buildInput(r *http.Request, names []string, fields map[string]reflect.Type) (raw json.RawMessage, inputErr, tooLarge *problem.Error) {
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
				return nil, nil, payloadTooLarge(mbe.Limit)
			}
			inputErr = invalid("", "cannot read the request body")
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
		return nil, invalid("", "cannot encode the input"), nil
	}
	return raw, inputErr, nil
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
