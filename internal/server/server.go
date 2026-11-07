// Package server is the HTTP transport of nexusd: timeouts, bearer
// authentication, body limit, panic containment, problem+json for unmatched
// requests, /healthz, and the REST routes derived from the core registry.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
)

// HTTP server limits (NFR-4, A-54). WriteTimeout bounds every response;
// a streaming handler extends its own deadline before each write
// (StreamWriteDeadline), so a live follow is not cut while a stalled client is
// still dropped.
const (
	ReadHeaderTimeout   = 5 * time.Second
	ReadTimeout         = 30 * time.Second
	WriteTimeout        = 60 * time.Second
	IdleTimeout         = 120 * time.Second
	MaxHeaderBytes      = 64 << 10
	StreamWriteDeadline = 30 * time.Second
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
}

// Server serves the REST API and /healthz.
type Server struct {
	http       *http.Server
	reg        *core.Registry
	mux        *http.ServeMux
	routes     []Route
	log        *slog.Logger
	base       context.Context
	cancelBase context.CancelFunc
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
	s := &Server{reg: reg, mux: http.NewServeMux(), log: opts.Logger}
	// The base context outlives no shutdown: Shutdown cancels it first, so
	// long-lived handlers such as log followers end promptly.
	s.base, s.cancelBase = context.WithCancel(context.Background())

	s.mux.HandleFunc("GET /healthz", s.healthz)
	if err := s.registerREST(); err != nil {
		s.cancelBase()
		return nil, err
	}

	// Authentication, with its rate limit, runs before the body is touched
	// or a route is matched, so an unauthenticated client learns nothing
	// about routes and cannot make the server read a body.
	var h http.Handler = http.HandlerFunc(s.dispatch)
	h = limitBody(s.log, opts.MaxBodyBytes, h)
	h = opts.Auth.Middleware(h)
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
	return s, nil
}

// HTTP returns the configured *http.Server, for inspection of its limits.
func (s *Server) HTTP() *http.Server { return s.http }

// Handler returns the full handler chain.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Serve accepts connections on l until Shutdown. It returns nil after a
// shutdown and the accept error otherwise.
func (s *Server) Serve(l net.Listener) error {
	if err := s.http.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
