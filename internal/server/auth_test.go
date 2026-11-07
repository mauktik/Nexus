package server_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
)

// sharedAuth and sharedToken authenticate every test server started without
// its own authenticator; do sends sharedToken.
var (
	sharedAuth  *auth.Authenticator
	sharedToken string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nexus-server-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sharedToken, _, err = auth.Create(dir, auth.CreateRequest{Name: "server-tests", Scopes: []string{"gadgets:read"}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	sharedAuth = auth.New(dir, auth.Options{Logger: discardLog})
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

var discardLog = slog.New(slog.NewJSONHandler(io.Discard, nil))

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

func mint(t *testing.T, dir, name string, scopes ...string) string {
	t.Helper()
	tok, _, err := auth.Create(dir, auth.CreateRequest{Name: name, Scopes: scopes})
	if err != nil {
		t.Fatalf("auth.Create: %v", err)
	}
	return tok
}

func doAuth(t *testing.T, method, url, authz string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	return send(t, req)
}

func expectProblem(t *testing.T, what string, resp *http.Response, body []byte, status int, slug problem.Slug) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("%s: status %d, want %d; body %s", what, resp.StatusCode, status, body)
	}
	if p := decodeProblem(t, resp, body); p.Slug() != slug {
		t.Fatalf("%s: problem %q, want %q", what, p.Type, slug)
	}
}

// TC-26: bearer authentication accepts valid and rejects invalid credentials.
func TestBearerAuthentication(t *testing.T) {
	dir := t.TempDir()
	authn := auth.New(dir, auth.Options{Logger: discardLog})
	valid := mint(t, dir, "valid")
	ts := startServer(t, server.Options{Auth: authn})
	url := ts.base + "/v1/capabilities"

	resp, body := doAuth(t, http.MethodGet, url, "Bearer "+valid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: %d %s", resp.StatusCode, body)
	}
	resp, _ = doAuth(t, http.MethodGet, url, "bearer "+valid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the auth scheme is case-insensitive (RFC 9110 11.1): %d", resp.StatusCode)
	}

	id, secret := valid[4:16], valid[17:]
	other := make([]byte, 6)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	wrongSecret := []byte(secret)
	if wrongSecret[0] == 'A' {
		wrongSecret[0] = 'B'
	} else {
		wrongSecret[0] = 'A'
	}
	cases := []struct {
		name, url, authz string
	}{
		{"no header", url, ""},
		{"Basic scheme", url, "Basic " + valid},
		{"unknown ID", url, "Bearer nxs_" + hex.EncodeToString(other) + "_" + secret},
		{"wrong secret", url, "Bearer nxs_" + id + "_" + string(wrongSecret)},
		{"truncated", url, "Bearer " + valid[:len(valid)-5]},
		{"query parameter only", url + "?token=" + valid + "&access_token=" + valid, ""},
		{"empty bearer", url, "Bearer "},
		{"two tokens", url, "Bearer " + valid + " " + valid},
	}
	bodies := map[string][]byte{}
	for _, c := range cases {
		resp, body := doAuth(t, http.MethodGet, c.url, c.authz)
		expectProblem(t, c.name, resp, body, http.StatusUnauthorized, problem.Unauthorized)
		if wa := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(wa, "Bearer") {
			t.Errorf("%s: WWW-Authenticate = %q, want a Bearer challenge", c.name, wa)
		}
		bodies[c.name] = body
	}
	if !bytes.Equal(bodies["unknown ID"], bodies["wrong secret"]) {
		t.Errorf("unknown ID and wrong secret are distinguishable:\n%s\n%s", bodies["unknown ID"], bodies["wrong secret"])
	}
	if strings.Contains(string(bodies["wrong secret"]), id) {
		t.Error("the 401 body names the token ID")
	}

	// Two Authorization headers are ambiguous and refused.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add("Authorization", "Bearer "+valid)
	req.Header.Add("Authorization", "Bearer "+valid)
	resp, body = send(t, req)
	expectProblem(t, "two headers", resp, body, http.StatusUnauthorized, problem.Unauthorized)
}

// newClockedServer builds a server whose authenticator runs on clk, for tests
// that drive the handler directly with chosen peer addresses.
func newClockedServer(t *testing.T, dir string, clk *clock, limit int) http.Handler {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(&syncBuffer{}, nil))
	reg, err := core.New([]provider.Provider{gadgets{}}, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: log,
		Providers: map[string]core.ProviderConfig{"gadgets": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.New(dir, auth.Options{Now: clk.Now, FailureLimitPerMinute: limit, Logger: log})
	srv, err := server.New(reg, server.Options{Auth: authn, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

func serveFrom(h http.Handler, peer, path, authz string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = peer
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TC-28: failure rate limiting.
func TestFailureRateLimiting(t *testing.T) {
	dir := t.TempDir()
	valid := "Bearer " + mint(t, dir, "rl")
	bad := "Bearer nxs_000000000000_" + strings.Repeat("A", 43)
	clk := &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	h := newClockedServer(t, dir, clk, 10)
	const a, b = "192.0.2.10:5000", "192.0.2.11:5000"

	for i := 1; i <= 10; i++ {
		if w := serveFrom(h, a, "/v1/capabilities", bad); w.Code != http.StatusUnauthorized {
			t.Fatalf("bad request %d: %d, want 401", i, w.Code)
		}
		clk.t = clk.t.Add(time.Second)
	}
	w := serveFrom(h, a, "/v1/capabilities", bad)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th bad request: %d, want 429", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != problem.ContentType || !strings.Contains(w.Body.String(), "urn:nexus:problem:rate-limited") {
		t.Fatalf("429 is not a rate-limited problem: %s %s", ct, w.Body)
	}
	// The first failure was at 12:00:00 and it is now 12:00:10: 50 seconds
	// remain in the window.
	if ra := w.Header().Get("Retry-After"); ra != "50" {
		t.Fatalf("Retry-After = %q, want \"50\"", ra)
	}
	if w := serveFrom(h, a, "/v1/capabilities", valid); w.Code != http.StatusTooManyRequests {
		t.Fatalf("valid token from a limited source: %d, want 429 (limiting precedes token evaluation)", w.Code)
	}
	if w := serveFrom(h, b, "/v1/capabilities", bad); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token from source B: %d, want 401", w.Code)
	}
	if w := serveFrom(h, a, "/healthz", ""); w.Code != http.StatusOK {
		t.Fatalf("/healthz from a limited source: %d, want 200", w.Code)
	}
	clk.t = clk.t.Add(51 * time.Second)
	if w := serveFrom(h, a, "/v1/capabilities", valid); w.Code != http.StatusOK {
		t.Fatalf("valid token after the window: %d %s, want 200", w.Code, w.Body)
	}

	h3 := newClockedServer(t, dir, clk, 3)
	const c = "[2001:db8::c]:443"
	for i := 1; i <= 3; i++ {
		if w := serveFrom(h3, c, "/v1/capabilities", bad); w.Code != http.StatusUnauthorized {
			t.Fatalf("limit 3, bad request %d: %d, want 401", i, w.Code)
		}
	}
	if w := serveFrom(h3, "[2001:db8::d]:443", "/v1/capabilities", bad); w.Code != http.StatusTooManyRequests {
		t.Fatalf("limit 3, fourth bad request from the same /64: %d, want 429", w.Code)
	}
}

// TC-ERR8: secrets never reach logs or error bodies.
func TestSecretsNeverReachLogsOrBodies(t *testing.T) {
	dir := t.TempDir()
	valid := mint(t, dir, "sentinel-valid")
	authLogs := &syncBuffer{}
	ts := startServer(t, server.Options{Auth: auth.New(dir, auth.Options{
		FailureLimitPerMinute: 1000, Logger: slog.New(slog.NewJSONHandler(authLogs, nil)),
	})})
	const sentinel = "SENTINELsecretSENTINELsecretSENTINELsecret0"
	id := valid[4:16]
	attempts := []string{
		"Bearer " + valid,
		"Bearer nxs_" + id + "_" + sentinel,
		"Bearer nxs_ffffffffffff_" + sentinel,
		"Bearer " + sentinel,
		"Basic " + sentinel,
		"Bearer nxs_" + id + "_" + sentinel + "x",
	}
	var bodies bytes.Buffer
	for _, a := range attempts {
		for _, path := range []string{"/v1/capabilities", "/v1/nope", "/v1/providers/gadgets/gadgets/g1/explode"} {
			resp, body := doAuth(t, http.MethodGet, ts.base+path, a)
			bodies.Write(body)
			for k, vs := range resp.Header {
				bodies.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
			}
		}
	}
	logs := ts.logs.String() + authLogs.String()
	if !strings.Contains(logs, "authentication failed") {
		t.Fatalf("no authentication failure was logged; the check below would be vacuous:\n%s", logs)
	}
	for _, s := range []string{sentinel, valid[17:], valid} {
		if strings.Contains(logs, s) {
			t.Errorf("secret %q appears in the logs", s)
		}
		if strings.Contains(bodies.String(), s) {
			t.Errorf("secret %q appears in a response", s)
		}
	}
}

// TC-ERR9: no network path mints or lists tokens.
func TestNoNetworkPathForTokens(t *testing.T) {
	dir := t.TempDir()
	all := mint(t, dir, "root", "containers:read", "containers:write", "packages:read", "packages:write",
		"packages:install", "scripts:read", "scripts:run", "audit:read", "gadgets:read")
	ts := startServer(t, server.Options{Auth: auth.New(dir, auth.Options{Logger: discardLog})})
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		for _, p := range []string{"/v1/tokens", "/v1/tokens/x", "/v1/auth/tokens"} {
			resp, body := doAuth(t, m, ts.base+p, "Bearer "+all)
			expectProblem(t, m+" "+p, resp, body, http.StatusNotFound, problem.RouteNotFound)
		}
	}
	routes := ts.srv.Routes()
	if len(routes) == 0 {
		t.Fatal("no routes recorded; the search below would be vacuous")
	}
	for _, r := range routes {
		if strings.Contains(strings.ToLower(r.Path+" "+r.OpID), "token") {
			t.Errorf("route %s %s (%s) mentions tokens", r.Method, r.Path, r.OpID)
		}
	}
}

// TC-ERR10: a corrupt token file fails closed and the process stays up.
func TestCorruptTokenFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	valid := "Bearer " + mint(t, dir, "c")
	authLogs := &syncBuffer{}
	ts := startServer(t, server.Options{Auth: auth.New(dir, auth.Options{Logger: slog.New(slog.NewJSONHandler(authLogs, nil))})})
	url := ts.base + "/v1/capabilities"
	if resp, body := doAuth(t, http.MethodGet, url, valid); resp.StatusCode != http.StatusOK {
		t.Fatalf("before corruption: %d %s", resp.StatusCode, body)
	}
	path := filepath.Join(dir, auth.TokenFileName)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"tokens": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		resp, body := doAuth(t, http.MethodGet, url, valid)
		expectProblem(t, "corrupt file", resp, body, http.StatusUnauthorized, problem.Unauthorized)
	}
	logs := authLogs.String()
	if !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, "token file") {
		t.Fatalf("no error logged for the corrupt token file:\n%s", logs)
	}
	if resp, _ := doAuth(t, http.MethodGet, ts.base+"/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("server is not serving after the corrupt file: %d", resp.StatusCode)
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, body := doAuth(t, http.MethodGet, url, valid); resp.StatusCode != http.StatusOK {
		t.Fatalf("after restoring the file: %d %s", resp.StatusCode, body)
	}
}

// The server refuses to start without an authenticator: no configuration
// leaves the API open.
func TestServerRequiresAuthenticator(t *testing.T) {
	reg, err := core.New([]provider.Provider{gadgets{}}, core.Options{Host: "h", Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.New(reg, server.Options{}); err == nil {
		t.Fatal("server.New without an authenticator succeeded")
	}
}
