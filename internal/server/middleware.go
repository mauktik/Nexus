// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"

	"github.com/mauktik/nexus/internal/problem"
)

// writeProblem sends p as application/problem+json. A failed write is logged:
// the status is already on the wire.
func writeProblem(log *slog.Logger, w http.ResponseWriter, r *http.Request, p *problem.Error) {
	if err := problem.Write(w, p); err != nil {
		log.Debug("problem response write failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
	}
}

// recoverPanics contains a handler panic to its own request (NFR-3). The
// panic value and stack go to the log only; the client gets a generic
// internal-error if nothing was sent yet. http.ErrAbortHandler keeps its
// meaning of aborting the response silently.
func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			log.Error("handler panic", "method", r.Method, "path", r.URL.Path,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			if tw.wroteHeader {
				// Too late for a problem response: abort the connection so
				// the client sees a truncated response, not a complete one.
				panic(http.ErrAbortHandler)
			}
			writeProblem(log, w, r, problem.New(problem.InternalError, "the server failed to handle the request"))
		}()
		next.ServeHTTP(tw, r)
	})
}

// trackingWriter records whether the response has started. Unwrap keeps
// http.ResponseController working through it (Flush, write deadlines).
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wroteHeader = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wroteHeader = true
	return t.ResponseWriter.Write(b)
}

func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// closeUnread sends Connection: close on an HTTP/1 response to a request
// whose body the handler never read. Without it net/http, before it sends
// the headers, reads up to 256 KiB of the unread body to keep the connection
// open, so a refusal such as the authentication middleware's 401 would wait
// on a client that holds back its body (NFR-7). HTTP/2 needs nothing: an
// unread stream is reset, not drained.
func closeUnread(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 || r.ContentLength == 0 || r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		body := &readTracker{ReadCloser: r.Body}
		r.Body = body
		next.ServeHTTP(&unreadWriter{ResponseWriter: w, body: body}, r)
	})
}

// readTracker records whether a body was ever read.
type readTracker struct {
	io.ReadCloser
	read atomic.Bool
}

func (t *readTracker) Read(p []byte) (int, error) {
	t.read.Store(true)
	return t.ReadCloser.Read(p)
}

// unreadWriter adds Connection: close to the headers when they go out
// before the body was read. Unwrap keeps http.ResponseController working.
type unreadWriter struct {
	http.ResponseWriter
	body  *readTracker
	wrote bool
}

func (u *unreadWriter) WriteHeader(code int) {
	if !u.wrote {
		u.wrote = true
		if !u.body.read.Load() {
			u.ResponseWriter.Header().Set("Connection", "close")
		}
	}
	u.ResponseWriter.WriteHeader(code)
}

func (u *unreadWriter) Write(b []byte) (int, error) {
	if !u.wrote {
		u.WriteHeader(http.StatusOK)
	}
	return u.ResponseWriter.Write(b)
}

func (u *unreadWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// limitBody caps every body with http.MaxBytesReader, so a body that turns
// out to be too long fails its read with *http.MaxBytesError, which the
// route reports as payload-too-large through the Dispatcher (rest.go), or
// upload-too-large on an upload route. A declared oversize body is never
// read: the body's first read fails at once. GET and HEAD routes read no
// body and are not audited, so they are refused here. The limit is max,
// except for a POST to an upload route, where it is that route's cap in
// uploads instead (FR-6).
func limitBody(log *slog.Logger, max int64, uploads map[string]func() int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := max
		if r.Method == http.MethodPost {
			if c, ok := uploads[r.URL.Path]; ok {
				limit = c()
			}
		}
		if r.ContentLength > limit {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				writeProblem(log, w, r, payloadTooLarge(limit))
				return
			}
			r.Body = oversizeBody{limit: limit}
			next.ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// oversizeBody replaces a body whose Content-Length exceeds the limit. Its
// first read fails as MaxBytesReader's would, without reading a byte; the
// server still closes the real body.
type oversizeBody struct{ limit int64 }

func (b oversizeBody) Read([]byte) (int, error) { return 0, &http.MaxBytesError{Limit: b.limit} }
func (oversizeBody) Close() error               { return nil }

func payloadTooLarge(max int64) *problem.Error {
	return problem.New(problem.PayloadTooLarge, fmt.Sprintf("the request body exceeds %d bytes", max))
}

// dispatch routes through the mux, but answers the mux's own 404 and 405 with
// problem+json instead of Go's plain-text bodies (FR-8). ServeMux.Handler
// returns an empty pattern exactly when it has no route for the request; the
// handler it returns then is the mux's NotFoundHandler or its 405 handler,
// which sets Allow.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	h, pattern := s.mux.Handler(r)
	if pattern != "" {
		s.mux.ServeHTTP(w, r)
		return
	}
	rec := &statusRecorder{header: http.Header{}}
	h.ServeHTTP(rec, r)
	if rec.status == http.StatusMethodNotAllowed {
		if allow := rec.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		writeProblem(s.log, w, r, problem.New(problem.MethodNotAllowed,
			fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path)))
		return
	}
	writeProblem(s.log, w, r, problem.New(problem.RouteNotFound,
		fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path)))
}

// statusRecorder captures the status and headers of the mux's fallback
// handlers and discards their plain-text body.
type statusRecorder struct {
	header http.Header
	status int
}

func (s *statusRecorder) Header() http.Header { return s.header }

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return len(b), nil
}
