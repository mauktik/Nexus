package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

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

// limitBody caps every body with http.MaxBytesReader, so a body that turns
// out to be too long fails its read with *http.MaxBytesError, which the
// route reports as payload-too-large through the Dispatcher (rest.go). A
// declared oversize body is never read: the body's first read fails at
// once. GET and HEAD routes read no body and are not audited, so they are
// refused here.
func limitBody(log *slog.Logger, max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > max {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				writeProblem(log, w, r, payloadTooLarge(max))
				return
			}
			r.Body = oversizeBody{limit: max}
			next.ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, max)
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
