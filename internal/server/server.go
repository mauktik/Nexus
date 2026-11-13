// Package server is the HTTP transport of nexusd: timeouts, bearer
// authentication, body limit, panic containment, problem+json for unmatched
// requests, /healthz, the REST routes derived from the core registry, and
// the MCP endpoint (internal/mcpadapter) behind its Origin guard.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/mcpadapter"
)

// HTTP server limits (NFR-4, A-54). WriteTimeout bounds every response;
// a streaming handler takes the deadline off while it waits for output and
// sets it around each write (StreamWriteDeadline), so a live follow is not
// cut while a stalled client is still dropped. Under HTTP/2 a write deadline
// is a timer that resets the stream when it fires, even with no write
// pending, which is why it must not stay armed between frames.
const (
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 30 * time.Second
	WriteTimeout      = 60 * time.Second
	IdleTimeout       = 120 * time.Second
	MaxHeaderBytes    = 64 << 10
)

// StreamWriteDeadline bounds one frame's write and flush. It is a variable
// so a test can shorten it.
var StreamWriteDeadline = 30 * time.Second

// Upload deadlines (A-5). ReadTimeout would cut a large upload on a slow
// link, so an upload route moves the request's read deadline, once the body
// is first read, to now + UploadDeadlineGrace + length/UploadMinRate (the
// declared length, bounded by the cap; the cap if undeclared), and its
// write deadline UploadDeadlineGrace past that.
const (
	UploadDeadlineGrace = 30 * time.Second
	UploadMinRate       = 1 << 20 // Bytes per second
)

// DefaultMaxBodyBytes is the request body limit when Options leaves it zero.
const DefaultMaxBodyBytes = 1 << 20

// Options configures New.
type Options struct {
	MaxBodyBytes int64        // Zero means DefaultMaxBodyBytes
	Logger       *slog.Logger // Nil means slog.Default()
	// Auth authenticates every request except GET /healthz. It is required:
	// there is no configuration in which the API is open.
	Auth *auth.Authenticator
	// TLS, when non-nil, holds the certificate to serve; its MinVersion is
	// raised to MinTLSVersion. Nil serves plain HTTP, which Serve allows on
	// loopback listeners only.
	TLS *tls.Config
	// MCPAllowedOrigins is mcp.allowedOrigins: the only Origin values /mcp
	// accepts. A request to /mcp with any other Origin header gets 403
	// before authentication. A request without one is not refused for it.
	MCPAllowedOrigins []string
	// NoteLimitPerMinute is core.DispatcherOptions.NoteLimitPerMinute: the
	// denied and refused audit records one token can cause per minute. Zero
	// means core.DefaultNoteLimitPerMinute.
	NoteLimitPerMinute int
}

// Server serves the REST API, /mcp and /healthz.
type Server struct {
	http   *http.Server
	reg    *core.Registry
	mcp    *mcpadapter.Adapter
	mux    *http.ServeMux
	routes []Route
	// uploadCaps maps the path of each upload route to its binding's
	// MaxBytes, the body limit limitBody applies to a POST there. It is
	// filled by registerREST and only read once serving starts.
	uploadCaps map[string]func() int64
	log        *slog.Logger
	base       context.Context
	cancelBase context.CancelFunc
	// tls records whether Options.TLS was set. Serve decides per listener
	// from it, not from http.Server.TLSConfig: net/http fills TLSConfig in
	// while configuring HTTP/2 on the first plain Serve, so a later
	// listener would otherwise be served as TLS with no certificate.
	tls bool
}

// New builds the server and registers every route the registry implies.
func New(reg *core.Registry, opts Options) (*Server, error) {
	if opts.Auth == nil {
		return nil, errors.New("no authenticator: every route except /healthz requires a token")
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{reg: reg, mux: http.NewServeMux(), log: opts.Logger, uploadCaps: map[string]func() int64{}}
	// The base context outlives no shutdown: Shutdown cancels it first, so
	// long-lived handlers such as log followers end promptly.
	s.base, s.cancelBase = context.WithCancel(context.Background())

	s.mux.HandleFunc("GET /healthz", s.healthz)
	// One Dispatcher serves REST and MCP alike.
	disp := core.NewDispatcher(reg, core.DispatcherOptions{Logger: s.log, NoteLimitPerMinute: opts.NoteLimitPerMinute})
	if err := s.registerREST(disp); err != nil {
		s.cancelBase()
		return nil, err
	}
	mcpHandler, err := mcpadapter.New(reg, disp, mcpadapter.Options{
		MaxBodyBytes: opts.MaxBodyBytes, Logger: s.log, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.cancelBase()
		return nil, err
	}
	s.mcp = mcpHandler
	s.mux.Handle("POST "+mcpadapter.Path, mcpHandler)

	// Authentication, with its rate limit, runs before the body is touched
	// or a route is matched, so an unauthenticated client learns nothing
	// about routes and cannot make the server read a body. The Origin guard
	// of /mcp runs before it (FR-23). closeUnread keeps net/http from
	// draining a body no handler read before it answers.
	var h http.Handler = http.HandlerFunc(s.dispatch)
	h = limitBody(s.log, opts.MaxBodyBytes, s.uploadCaps, h)
	h = opts.Auth.Middleware(h)
	h = mcpadapter.OriginGuard(opts.MCPAllowedOrigins, s.log, h)
	h = closeUnread(h)
	h = recoverPanics(s.log, h)
	s.http = &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return s.base },
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	if opts.TLS != nil {
		s.tls = true
		s.http.TLSConfig = serverTLS(opts.TLS)
	}
	return s, nil
}

// RefreshMCP rebuilds the MCP tool list from the registry's current
// provider enablement. A reload calls it after applying new provider
// sections; calls already in flight finish on the list they started with.
func (s *Server) RefreshMCP() error { return s.mcp.Refresh() }

// HTTP returns the configured *http.Server, for inspection of its limits.
func (s *Server) HTTP() *http.Server { return s.http }

// Handler returns the full handler chain.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Serve accepts connections on l until Shutdown, over TLS when Options.TLS
// was set. Without TLS it refuses a listener that is not loopback. It returns
// nil after a shutdown and the accept error otherwise.
func (s *Server) Serve(l net.Listener) error {
	var err error
	if s.tls {
		err = s.http.ServeTLS(l, "", "")
	} else if err = checkPlainHTTP(l.Addr()); err == nil {
		err = s.http.Serve(l)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s: %w", l.Addr(), err)
	}
	return nil
}

// Shutdown cancels the base context of every request, stops accepting, and
// waits for in-flight requests until ctx ends. When ctx ends first, the
// remaining connections are closed and ctx's error is returned.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancelBase()
	err := s.http.Shutdown(ctx)
	if err != nil {
		if cerr := s.http.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// healthz reveals liveness only: no version, host or provider data.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.writeBody(w, r, []byte(`{"status":"ok"}`))
}

// writeBody writes a response body. The status is already sent, so a failed
// write (normally a client that went away) can only be logged.
func (s *Server) writeBody(w http.ResponseWriter, r *http.Request, body []byte) {
	if _, err := w.Write(body); err != nil {
		s.log.Debug("response write failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
	}
}
