package server

import (
	"bytes"
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
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
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
// each registration.
func (s *Server) registerREST() error {
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
		if err := handle(s.mux, rt.Method+" "+rt.Path, s.opHandler(e, names, fields)); err != nil {
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

// opHandler serves one binding: availability, input assembly and decoding,
// then the call. Authorization and schema validation arrive with the
// Dispatcher (2.2), which this handler then calls instead.
func (s *Server) opHandler(e core.Entry, names []string, fields map[string]reflect.Type) http.Handler {
	op := e.Binding.Op()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, st := s.reg.Available(e.Provider); !ok {
			writeProblem(s.log, w, r, problem.New(problem.CapabilityUnavailable,
				fmt.Sprintf("provider %s is %s", e.Provider, st.State)).
				With("provider", e.Provider).With("reason", st.Reason))
			return
		}
		raw, perr := buildInput(r, names, fields)
		if perr != nil {
			writeProblem(s.log, w, r, perr)
			return
		}
		in, err := e.Binding.Decode(raw)
		if err != nil {
			writeProblem(s.log, w, r, problem.New(problem.ValidationFailed, err.Error()))
			return
		}
		switch e.Binding.Kind() {
		case provider.BindSync:
			out, err := e.Binding.CallSync(r.Context(), in)
			if err != nil {
				s.writeError(w, r, op, err)
				return
			}
			s.writeJSON(w, r, op, http.StatusOK, out)
		case provider.BindStream:
			s.stream(w, r, e.Binding, in)
		default:
			// Long-running bindings run through the operation manager (3.2).
			s.log.Error("no operation manager for a long-running op", "op", op.ID)
			writeProblem(s.log, w, r, problem.New(problem.InternalError, "the server failed to handle the request"))
		}
	})
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

// stream serves a ModeStream binding as NDJSON, one apitypes.Frame per line.
// Before each write it extends the write deadline, so the server's
// WriteTimeout never cuts a live follow while a stalled client is dropped.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, b provider.Binding, in any) {
	op := b.Op()
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
	err := b.CallStream(r.Context(), in, emit)
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
// object body for POST, and the route wildcards, which name input fields.
func buildInput(r *http.Request, names []string, fields map[string]reflect.Type) (json.RawMessage, *problem.Error) {
	obj := map[string]any{}
	switch r.Method {
	case http.MethodGet:
		for key, vals := range r.URL.Query() {
			t, ok := fields[key]
			if !ok || slices.Contains(names, key) {
				return nil, invalid("unknown query parameter %q", key)
			}
			v, err := queryValue(t, vals)
			if err != nil {
				return nil, invalid("query parameter %q: %v", key, err)
			}
			obj[key] = v
		}
	default:
		if r.URL.RawQuery != "" {
			return nil, invalid("query parameters are not accepted on %s; send the input as a JSON body", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, payloadTooLarge(mbe.Limit)
			}
			return nil, invalid("cannot read the request body")
		}
		if len(bytes.TrimSpace(body)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.UseNumber()
			if err := dec.Decode(&obj); err != nil || obj == nil {
				return nil, invalid("the request body must be a JSON object")
			}
			if _, err := dec.Token(); !errors.Is(err, io.EOF) {
				return nil, invalid("the request body must hold a single JSON object")
			}
		}
	}
	for _, n := range names {
		v := r.PathValue(n)
		if prev, ok := obj[n]; ok && prev != v {
			return nil, invalid("%q in the body differs from the path", n)
		}
		obj[n] = v
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, invalid("cannot encode the input")
	}
	return raw, nil
}

func invalid(format string, args ...any) *problem.Error {
	return problem.New(problem.ValidationFailed, fmt.Sprintf(format, args...))
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
