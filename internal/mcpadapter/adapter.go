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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
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
	// as REST reads Idempotency-Key. Nil means calls carry no key. A
	// mutating tool's idempotencyKey argument is the other way to send it.
	IdempotencyKey func(http.Header) string
	// LogPageBytes bounds the text of one page of a log tool. Zero means
	// DefaultLogPageBytes (A-65).
	LogPageBytes int
}

// Adapter is the /mcp handler.
type Adapter struct {
	reg       *core.Registry
	disp      *core.Dispatcher
	log       *slog.Logger
	key       func(http.Header) string
	pageBytes int
	current   atomic.Pointer[mcp.Server]
	handler   http.Handler
}

// New builds the adapter and its first tool list. disp is the Dispatcher
// REST uses, so both surfaces share one pipeline.
func New(reg *core.Registry, disp *core.Dispatcher, opts Options) (*Adapter, error) {
	if opts.MaxBodyBytes <= 0 {
		return nil, errors.New("mcp: no request body limit")
	}
	if opts.LogPageBytes < 0 {
		return nil, errors.New("mcp: negative log page size")
	}
	a := &Adapter{reg: reg, disp: disp, log: opts.Logger, key: opts.IdempotencyKey, pageBytes: opts.LogPageBytes}
	if a.pageBytes == 0 {
		a.pageBytes = DefaultLogPageBytes
	}
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
	// no tool streams: a ModeStream op's tool returns one page. Older
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
	untrusted := map[string]bool{}
	for _, e := range a.reg.Entries() {
		if op := e.Binding.Op(); op.Untrusted {
			untrusted[op.ID] = true
		}
	}
	owner := map[string]string{}
	for _, e := range a.reg.Entries() {
		op := e.Binding.Op()
		if !enabled[e.Provider] || op.MCP == nil {
			continue
		}
		decl, t, err := newTool(op, a.pageBytes, untrusted)
		if err != nil {
			return nil, err
		}
		if prev, dup := owner[decl.Name]; dup {
			return nil, fmt.Errorf("mcp: ops %q and %q are both tool %q", prev, op.ID, decl.Name)
		}
		owner[decl.Name] = op.ID
		srv.AddTool(decl, a.call(t))
	}
	return srv, nil
}

// call is a tool's handler: the principal the authentication middleware put
// in the context, the arguments as the input and the key, through the
// Dispatcher with SurfaceMCP. The key is the idempotencyKey argument,
// removed from the input, or else the request's Idempotency-Key header.
// Nothing else decides anything.
func (a *Adapter) call(t *tool) mcp.ToolHandler {
	op := t.op
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// A call without a principal cannot pass the middleware; if one
		// ever did, the Dispatcher refuses the zero value.
		caller, _ := auth.PrincipalFrom(ctx)
		ireq := core.InvokeRequest{Caller: caller, Surface: core.SurfaceMCP, Op: op}
		header := ""
		if req.Extra != nil {
			header = a.key(req.Extra.Header)
		}
		var args json.RawMessage
		if req.Params != nil {
			args = req.Params.Arguments
		}
		ireq.Input, ireq.IdempotencyKey, ireq.InputError = t.arguments(args, header)
		res, err := a.disp.Invoke(ctx, ireq)
		if err != nil {
			return a.failure(op, err), nil
		}
		if res.Stream != nil {
			if t.pager == noPager {
				a.log.Error("stream result on a non-stream tool", "op", op.ID)
				return a.failure(op, problem.New(problem.InternalError, "the server failed to handle the request")), nil
			}
			return a.page(ctx, t, ireq.Input, res.Stream), nil
		}
		return a.success(t, res.Body), nil
	}
}

// success carries the result in structuredContent, and its JSON in a text
// block for clients that read only content (tools: Structured Content). A
// result carrying host text is labeled in both.
func (a *Adapter) success(t *tool, body any) *mcp.CallToolResult {
	op := t.op
	b, err := marshal(body)
	if err == nil && t.untrustedResult(b) {
		b, err = withContentTrust(b)
	}
	if err != nil {
		a.log.Error("operation failed", "op", op.ID, "error", fmt.Errorf("marshal result: %w", err).Error())
		return a.failure(op, problem.New(problem.InternalError, "the server failed to handle the request"))
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: json.RawMessage(b),
	}
}

// marshal is json.Marshal without HTML escaping, so the text block reads as
// the result's JSON. It does not change the bytes sent for <, > and &: the
// SDK's CallToolResult.MarshalJSON escapes them again, and the page limit
// counts them as the six bytes they take on the wire (runeCost, CR-93).
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
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
