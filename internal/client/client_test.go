// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/server"
)

// TC-E27: plain HTTP only for loopback, refused before connecting.
func TestPlainHTTPOnlyOnLoopback(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8443", "http://[::1]:8443", "http://127.0.0.2:1"} {
		if _, err := ParseURL(raw); err != nil {
			t.Errorf("ParseURL(%q) = %v, want accepted", raw, err)
		}
	}
	for _, raw := range []string{"http://192.168.1.2:8443", "http://nas.lan:8443", "http://localhost:8443", "http://[::ffff:192.168.1.2]:1"} {
		_, err := ParseURL(raw)
		var pe *PlainHTTPError
		if !errors.As(err, &pe) {
			t.Errorf("ParseURL(%q) = %v, want *PlainHTTPError", raw, err)
		}
		if _, err := New(Options{URL: raw, Token: "x"}); !errors.As(err, &pe) {
			t.Errorf("New(%q) = %v, want *PlainHTTPError", raw, err)
		}
	}
	for _, raw := range []string{"ftp://nas.lan", "https://", "https://u:p@nas.lan", "https://nas.lan/v1", "https://nas.lan?x=1", "nas.lan:8443"} {
		if _, err := ParseURL(raw); err == nil {
			t.Errorf("ParseURL(%q) accepted a bad URL", raw)
		}
	}
	if u, err := ParseURL("https://nas.lan:8443/"); err != nil || u.String() != "https://nas.lan:8443" {
		t.Errorf("ParseURL with a trailing slash = %v, %v", u, err)
	}
}

func TestUnaryClientTimeoutAndRedirects(t *testing.T) {
	c, err := New(Options{URL: "https://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPTimeout() != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", c.HTTPTimeout())
	}

	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	c, err = New(Options{URL: srv.URL, Token: sentinelToken})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Do(context.Background(), http.MethodGet, "/capabilities", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusFound || hops != 1 {
		t.Fatalf("redirect: err %v after %d hops; want a 302 StatusError without following", err, hops)
	}
}

func TestTimeoutIsTransport(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	c, err := New(Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Do(ctx, http.MethodGet, "/capabilities", nil, nil); !IsTransport(err) {
		t.Fatalf("timed-out request: %v, want a transport error", err)
	}
}

// TC-ERR63: an older host. A real internal/server whose registry lacks the
// newer op answers route-not-found; a bare 404 means the same; a not-found
// problem is a missing resource.
func TestOlderHostLackingRoute(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dir := t.TempDir()
	token, _, err := auth.Create(dir, auth.CreateRequest{Name: "cli-test"})
	if err != nil {
		t.Fatal(err)
	}
	// A catalog with no providers: the system ops only, so the newer
	// provider op is not routed.
	reg, err := core.New(nil, core.Options{Host: "old", Version: "v0.0.1-old", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(reg, server.Options{Auth: auth.New(dir, auth.Options{Logger: log}), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	})
	const newer = "/providers/scripts/scripts"

	real, err := New(Options{URL: "http://" + ln.Addr().String(), Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if err := real.Do(context.Background(), http.MethodGet, "/capabilities", nil, nil); err != nil {
		t.Fatalf("capabilities on the old host: %v", err)
	}
	err = real.Do(context.Background(), http.MethodGet, newer, nil, nil)
	if p := asProblem(t, err); p.Slug() != problem.RouteNotFound {
		t.Errorf("real old server: slug %q, want route-not-found", p.Slug())
	}

	bare := httptest.NewServer(http.NotFoundHandler())
	defer bare.Close()
	c, err := New(Options{URL: bare.URL, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Do(context.Background(), http.MethodGet, newer, nil, nil)
	if p := asProblem(t, err); p.Slug() != problem.RouteNotFound {
		t.Errorf("bare 404: slug %q, want route-not-found", p.Slug())
	}

	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = problem.Write(w, problem.New(problem.NotFound, "no container named web"))
	}))
	defer missing.Close()
	c, err = New(Options{URL: missing.URL, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Do(context.Background(), http.MethodGet, "/providers/docker/containers/web", nil, nil)
	if p := asProblem(t, err); p.Slug() != problem.NotFound || p.Detail != "no container named web" {
		t.Errorf("not-found problem: %+v", p)
	}
}

func asProblem(t *testing.T, err error) *problem.Error {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) {
		t.Fatalf("error %v (%T) is not a *problem.Error", err, err)
	}
	return p
}
