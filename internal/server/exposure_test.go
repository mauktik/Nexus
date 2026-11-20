// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/tlsutil"
)

// testCert is a generated nexusd certificate and a pool trusting only it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	certPEM, keyPEM, err := tlsutil.Generate("testhost", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("cannot add the certificate to a pool")
	}
	return cert, pool
}

func newGadgetServer(t *testing.T, opts server.Options) *server.Server {
	t.Helper()
	reg, err := core.New([]provider.Provider{gadgets{}}, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: discardLog,
		Providers: map[string]core.ProviderConfig{"gadgets": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	opts.Logger = discardLog
	if opts.Auth == nil {
		opts.Auth = freshAuth()
	}
	srv, err := server.New(reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// serveOn runs srv on a loopback listener until the test ends.
func serveOn(t *testing.T, srv *server.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return ln.Addr().String()
}

// TC-41: TLS 1.2 is the minimum version, whatever the caller's TLS config
// says.
func TestTLSMinimumVersion(t *testing.T) {
	cert, pool := testCert(t)
	for _, callerMin := range []uint16{0, tls.VersionTLS10} {
		t.Run(fmt.Sprintf("caller MinVersion %#x", callerMin), func(t *testing.T) {
			srv := newGadgetServer(t, server.Options{TLS: &tls.Config{
				Certificates: []tls.Certificate{cert}, MinVersion: callerMin,
			}})
			if got := srv.HTTP().TLSConfig.MinVersion; got != tls.VersionTLS12 {
				t.Errorf("server MinVersion = %#x, want TLS 1.2 (%#x)", got, tls.VersionTLS12)
			}
			addr := serveOn(t, srv)
			versions := []struct {
				name string
				v    uint16
				ok   bool
			}{
				{"TLS 1.1", tls.VersionTLS11, false},
				{"TLS 1.2", tls.VersionTLS12, true},
				{"TLS 1.3", tls.VersionTLS13, true},
			}
			for _, c := range versions {
				// Go clients refuse anything below 1.2 by default, so the
				// client range is pinned explicitly.
				conf := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: c.v, MaxVersion: c.v}
				d := &net.Dialer{Timeout: 5 * time.Second}
				conn, err := tls.DialWithDialer(d, "tcp", addr, conf)
				if !c.ok {
					if err == nil {
						_ = conn.Close()
						t.Errorf("%s handshake succeeded, want failure", c.name)
					}
					continue
				}
				if err != nil {
					t.Errorf("%s handshake: %v", c.name, err)
					continue
				}
				if got := conn.ConnectionState().Version; got != c.v {
					t.Errorf("%s negotiated %#x", c.name, got)
				}
				_ = conn.Close()
			}

			hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"},
			}}
			resp, err := hc.Get("https://" + addr + "/healthz")
			if err != nil {
				t.Fatalf("GET /healthz over TLS: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.TLS == nil {
				t.Fatalf("GET /healthz over TLS: %d, TLS %v", resp.StatusCode, resp.TLS != nil)
			}
		})
	}
}

// fakeListener reports addr and accepts nothing; it lets the plain-HTTP
// guard be tested without binding a non-loopback address.
type fakeListener struct {
	addr   net.Addr
	closed chan struct{}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *fakeListener) Addr() net.Addr { return l.addr }

// TC-ERR16 (transport half): the server itself refuses to serve plain HTTP
// on any address that is not loopback, so a wiring mistake cannot expose it.
func TestPlainHTTPOnlyOnLoopback(t *testing.T) {
	for _, ip := range []string{"0.0.0.0", "::", "192.168.1.20", "::ffff:192.168.1.20"} {
		srv := newGadgetServer(t, server.Options{})
		ln := &fakeListener{addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 7443}, closed: make(chan struct{})}
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ln) }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "loopback") {
				t.Errorf("plain HTTP on %s: %v, want a refusal naming loopback", ip, err)
			}
		case <-time.After(5 * time.Second):
			_ = ln.Close()
			<-done
			t.Errorf("plain HTTP on %s was served", ip)
		}
	}
	cert, _ := testCert(t)
	srv := newGadgetServer(t, server.Options{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}})
	ln := &fakeListener{addr: &net.TCPAddr{IP: net.IPv4zero, Port: 7443}, closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		t.Fatalf("TLS on 0.0.0.0 refused: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("serve after shutdown: %v", err)
	}
}

func hasCORS(h http.Header) []string {
	var found []string
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			found = append(found, k)
		}
	}
	return found
}

// TC-42: no response carries any Access-Control-* header.
func TestNoCORSHeaders(t *testing.T) {
	ts := startServer(t, server.Options{})
	const evil = "https://evil.example"
	type probe struct {
		method, path string
		authed       bool
		preflight    string
	}
	probes := []probe{
		{http.MethodGet, "/healthz", false, ""},
		{http.MethodGet, "/v1/capabilities", true, ""},
		{http.MethodGet, "/v1/capabilities", false, ""},
		{http.MethodOptions, "/healthz", false, http.MethodGet},
		{http.MethodOptions, "/v1/capabilities", false, http.MethodGet},
		{http.MethodOptions, "/v1/capabilities", true, http.MethodGet},
	}
	routes := ts.srv.Routes()
	if len(routes) == 0 {
		t.Fatal("no routes; the preflight probes would be vacuous")
	}
	for _, r := range routes {
		path := strings.NewReplacer("{", "", "}", "").Replace(r.Path)
		probes = append(probes, probe{http.MethodOptions, path, false, r.Method}, probe{http.MethodOptions, path, true, r.Method})
	}
	for _, p := range probes {
		req, err := http.NewRequest(p.method, ts.base+p.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", evil)
		if p.preflight != "" {
			req.Header.Set("Access-Control-Request-Method", p.preflight)
			req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		}
		if p.authed {
			req.Header.Set("Authorization", "Bearer "+sharedToken)
		}
		resp, _ := send(t, req)
		if found := hasCORS(resp.Header); len(found) > 0 {
			t.Errorf("%s %s (authed %v): CORS headers %v", p.method, p.path, p.authed, found)
		}
	}
}

// TC-43: forwarded headers are never honored. The audit source and the rate
// limiter key are always the TCP peer, so spoofing evades neither.
func TestForwardedHeadersIgnored(t *testing.T) {
	tokens := t.TempDir()
	bad := "Bearer nxs_000000000000_" + strings.Repeat("A", 43)
	clk := &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	h, lg, _ := auditedServer(t, tokens, clk)
	spoof := func(i int) (string, string) {
		addr := fmt.Sprintf("203.0.113.%d", 10+i)
		switch i % 3 {
		case 0:
			return "X-Forwarded-For", addr
		case 1:
			return "X-Real-IP", addr
		default:
			return "Forwarded", "for=" + addr
		}
	}
	request := func(peer string, i int, authz string) int {
		r, err := http.NewRequest(http.MethodGet, "/v1/capabilities", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.RemoteAddr = peer
		k, v := spoof(i)
		r.Header.Set(k, v)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	const first = "192.0.2.60:4000"
	for i := range 3 {
		if code := request(first, i, ""); code != http.StatusUnauthorized {
			t.Fatalf("denied request %d: %d", i, code)
		}
	}
	recs := allRecords(t, lg)
	if len(recs) != 3 {
		t.Fatalf("%d records, want 3", len(recs))
	}
	for _, r := range recs {
		if r.Source != "192.0.2.60" || r.Outcome != audit.OutcomeDenied {
			t.Errorf("audit source %q, want the TCP peer 192.0.2.60", r.Source)
		}
	}

	const second = "192.0.2.61:4000"
	for i := range auth.DefaultFailureLimitPerMinute {
		if code := request(second, i+3, bad); code != http.StatusUnauthorized {
			t.Fatalf("bad request %d: %d, want 401", i+1, code)
		}
		clk.t = clk.t.Add(time.Second)
	}
	if code := request(second, 99, bad); code != http.StatusTooManyRequests {
		t.Fatalf("11th bad request with yet another spoofed source: %d, want 429", code)
	}
	clk.t = clk.t.Add(auth.FailureWindow)
	sawAggregate := false
	for _, r := range allRecords(t, lg) {
		if strings.HasPrefix(r.Source, "203.0.113.") {
			t.Errorf("a spoofed address reached the audit log: %+v", r)
		}
		if r.Problem == "rate-limited" {
			sawAggregate = true
			// The aggregate is keyed by the limiter's source group.
			if r.Source != "192.0.2.61" || r.Count != 1 {
				t.Errorf("rate-limit aggregate = %+v, want source 192.0.2.61 with count 1", r)
			}
		}
	}
	if !sawAggregate {
		t.Error("no rate-limited aggregate record")
	}
}
