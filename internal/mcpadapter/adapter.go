// Package mcpadapter serves the operations as MCP tools at /mcp over
// Streamable HTTP (FR-27): a stateless adapter over the Dispatcher with no
// business logic of its own. Authentication, scopes, audit, validation and
// idempotency are the Dispatcher's and the shared middleware's, exactly as
// for REST; this package maps a tool call to core.InvokeRequest and its
// answer back to a CallToolResult.
//
// The handler runs the SDK (github.com/modelcontextprotocol/go-sdk) in
// stateless mode with JSON responses. Every request asks for the current
// *mcp.Server, which Refresh replaces when a reload changes provider
// enablement, so a reload needs no restart and a call already running
// finishes on the server it started with.
package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sync/atomic"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/problem"
)

// ServerName is the server name /mcp reports in serverInfo.
const ServerName = "nexusd"

// Options configures New.
type Options struct {
	// MaxBodyBytes is the request body limit (NFR-4). It is required: the
	// SDK's default when it is left zero is 4 MiB.
	MaxBodyBytes int64
	Logger       *slog.Logger // Nil means slog.Default()
	// IdempotencyKey reads the key of a call from its HTTP request headers,
	// as REST reads Idempotency-Key. Nil means calls carry no key.
	IdempotencyKey func(http.Header) string
}

// Adapter is the /mcp handler.
type Adapter struct {
	reg     *core.Registry
	disp    *core.Dispatcher
	log     *slog.Logger
	key     func(http.Header) string
	current atomic.Pointer[mcp.Server]
	handler http.Handler
}

// New builds the adapter and its first tool list. disp is the Dispatcher
// REST uses, so both surfaces share one pipeline.
func New(reg *core.Registry, disp *core.Dispatcher, opts Options) (*Adapter, error) {
	if opts.MaxBodyBytes <= 0 {
		return nil, errors.New("mcp: no request body limit")
	}
	a := &Adapter{reg: reg, disp: disp, log: opts.Logger, key: opts.IdempotencyKey}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.key == nil {
		a.key = func(http.Header) string { return "" }
	}
	if err := a.Refresh(); err != nil {
		return nil, err
	}
	// Stateless: no Mcp-Session-Id is read or minted, so no request can
	// lean on an earlier one's authentication. JSONResponse is safe because
	// no tool streams (8.2 bounds ModeStream tools to one page). Older
	// protocol versions stay negotiable per the SDK default (A-19); they
	// pass the same middleware and Dispatcher. A closed connection cancels
	// the call, as the 2026-07-28 binding requires and as a REST request's
	// context does; so does server shutdown, which cancels every request.
	a.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return a.current.Load() },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			MaxRequestBodyBytes:          opts.MaxBodyBytes,
			PropagateRequestCancellation: true,
		})
	return a, nil
}

// ServeHTTP serves /mcp. The Origin guard and authentication run before it.
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// Refresh builds the tool list for the registry's current provider
// enablement and swaps it in. On error the current list stays.
func (a *Adapter) Refresh() error {
	srv, err := a.build()
	if err != nil {
		return err
	}
	a.current.Store(srv)
	return nil
}

// build makes one *mcp.Server holding a tool for every exposed op of every
// provider that configuration does not disable. An unavailable provider's
// tools stay listed and answer capability-unavailable, as its REST routes
// do. The list does not depend on the caller: scopes are checked when a
// tool is called. The SDK sorts tools by name, so the order is stable.
func (a *Adapter) build() (*mcp.Server, error) {
	caps := a.reg.Capabilities(context.Background(), false)
	enabled := map[string]bool{def.SystemProvider: true}
	for _, p := range caps.Providers {
		if p.Status != apitypes.ProviderDisabled {
			enabled[p.Name] = true
		}
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: caps.Version}, &mcp.ServerOptions{
		// Tools only, and no listChanged: the list changes on a reload, and
		// a stateless server has no stream to announce it on.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	owner := map[string]string{}
	for _, e := range a.reg.Entries() {
		op := e.Binding.Op()
		if !enabled[e.Provider] || !exposed(op) {
			continue
		}
		tool, err := toolFor(op)
		if err != nil {
			return nil, err
		}
		if prev, dup := owner[tool.Name]; dup {
			return nil, fmt.Errorf("mcp: ops %q and %q are both tool %q", prev, op.ID, tool.Name)
		}
		owner[tool.Name] = op.ID
		srv.AddTool(tool, a.call(op))
	}
	return srv, nil
}

// exposed reports whether op is a tool. An op without an MCP binding is
// not. A ModeStream op is not yet: its tool returns one bounded page of
// output (Streaming, A-65), which 8.2 adds.
func exposed(op def.Op) bool {
	return op.MCP != nil && op.Mode != def.ModeStream
}

// toolFor declares op's tool: its MCP name and description, its input
// schema and its output schema, all from the definition.
func toolFor(op def.Op) (*mcp.Tool, error) {
	in, err := op.InputSchema()
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q: %w", op.MCP.Tool, err)
	}
	if in.Schema().Type != "object" {
		return nil, fmt.Errorf("mcp tool %q: input schema of op %q is not an object", op.MCP.Tool, op.ID)
	}
	out, err := outputSchema(op)
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q: %w", op.MCP.Tool, err)
	}
	t := &mcp.Tool{Name: op.MCP.Tool, Description: op.MCP.Description, InputSchema: in.Schema()}
	if out != nil {
		t.OutputSchema = out
	}
	return t, nil
}

// outputSchema describes the structuredContent of op's successful result:
// the operation for a long-running op, which answers with it at once, and
// the output otherwise. It is the wire schema the OpenAPI document uses.
// MCP revisions before 2026-07-28 require an object schema, and those stay
// negotiable, so an op whose output is not an object declares none (the
// field is optional) rather than one an older client rejects.
func outputSchema(op def.Op) (*jsonschema.Schema, error) {
	t := op.OutputType()
	if op.Mode == def.ModeLongRunning {
		t = reflect.TypeFor[apitypes.Operation]()
	}
	s, err := openapi.WireSchema(t)
	if err != nil {
		return nil, err
	}
	if s.Type != "object" {
		return nil, nil
	}
	return s, nil
}

// call is a tool's handler: the principal the authentication middleware put
// in the context, the arguments as the input and the request's key, through
// the Dispatcher with SurfaceMCP. Nothing else decides anything.
func (a *Adapter) call(op def.Op) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// A call without a principal cannot pass the middleware; if one
		// ever did, the Dispatcher refuses the zero value.
		caller, _ := auth.PrincipalFrom(ctx)
		ireq := core.InvokeRequest{Caller: caller, Surface: core.SurfaceMCP, Op: op}
		if req.Params != nil {
			ireq.Input = req.Params.Arguments
		}
		if req.Extra != nil {
			ireq.IdempotencyKey = a.key(req.Extra.Header)
		}
		res, err := a.disp.Invoke(ctx, ireq)
		if err != nil {
			return a.failure(op, err), nil
		}
		if res.Stream != nil {
			// exposed keeps ModeStream ops off the list.
			a.log.Error("stream result on a non-stream tool", "op", op.ID)
			return a.failure(op, problem.New(problem.InternalError, "the server failed to handle the request")), nil
		}
		return a.success(op, res.Body), nil
	}
}

// success carries the result in structuredContent, and its JSON in a text
// block for clients that read only content (tools: Structured Content).
func (a *Adapter) success(op def.Op, body any) *mcp.CallToolResult {
	b, err := json.Marshal(body)
	if err != nil {
		a.log.Error("operation failed", "op", op.ID, "error", fmt.Errorf("marshal result: %w", err).Error())
		return a.failure(op, problem.New(problem.InternalError, "the server failed to handle the request"))
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: json.RawMessage(b),
	}
}

// failure turns a problem into a tool execution error, so the model sees
// it and can correct the call: isError with the problem in
// structuredContent and as text. It is never a protocol error. Any other
// error is an internal error whose cause goes to the log only.
func (a *Adapter) failure(op def.Op, err error) *mcp.CallToolResult {
	var p *problem.Error
	if !errors.As(err, &p) {
		a.log.Error("operation failed", "op", op.ID, "error", err.Error())
		p = problem.New(problem.InternalError, "the server failed to handle the request")
	}
	b, merr := json.Marshal(p)
	if merr != nil {
		a.log.Error("marshal problem", "op", op.ID, "error", merr.Error())
		b = []byte(`{"type":"` + problem.TypePrefix + string(problem.InternalError) + `","title":"Internal error"}`)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: json.RawMessage(b),
		IsError:           true,
	}
}
