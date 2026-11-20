// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package mcpadapter

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/problem"
)

// Path is the MCP endpoint.
const Path = "/mcp"

// OriginGuard refuses, with 403, every request to Path whose Origin header
// is not one of allowed (FR-23). The 2026-07-28 Streamable HTTP binding
// requires it: "If the Origin header is present and invalid, servers MUST
// respond with HTTP 403 Forbidden." The SDK does not check Origin unless
// configured, and its option for it is deprecated in favor of wrapping, so
// Nexus wraps (A-18).
//
// It goes outside authentication, so a browser page learns nothing about
// tokens and spends none of the failure budget. A request without Origin is
// not a browser's cross-origin request and passes on to authentication.
// Comparison is exact apart from ASCII case, which RFC 6454 serialization
// does not use: a different scheme, host or port is a different origin, and
// the opaque origin "null" is never allowed. More than one Origin header is
// refused. Other paths pass untouched.
func OriginGuard(allowed []string, log *slog.Logger, next http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	allowed = slices.Clone(allowed)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path {
			next.ServeHTTP(w, r)
			return
		}
		vals := r.Header.Values("Origin")
		if len(vals) == 0 || (len(vals) == 1 && originAllowed(allowed, vals[0])) {
			next.ServeHTTP(w, r)
			return
		}
		log.Info("mcp request refused", "reason", "origin not allowed", "source", r.RemoteAddr)
		if err := problem.Write(w, problem.New(problem.Forbidden, "the request's Origin is not in mcp.allowedOrigins")); err != nil {
			log.Debug("problem response write failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
		}
	})
}

func originAllowed(allowed []string, origin string) bool {
	if origin == "" || origin == "null" {
		return false
	}
	return slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(a, origin) })
}
