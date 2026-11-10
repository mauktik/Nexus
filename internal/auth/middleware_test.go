package auth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
)

func TestMiddlewarePassesPrincipalAndExemptsOnlyHealthz(t *testing.T) {
	dir := t.TempDir()
	tok, rec := mustCreate(t, dir, CreateRequest{Name: "mw", Scopes: []string{"audit:read"}})
	a := New(dir, Options{Logger: quietLog()})
	var got Principal
	var gotOK bool
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotOK = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	serve := func(method, path, authz string) int {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "[2001:db8::5]:9"
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if code := serve(http.MethodGet, "/v1/capabilities", "Bearer "+tok); code != http.StatusNoContent {
		t.Fatalf("valid token: %d", code)
	}
	want := Principal{TokenID: rec.ID, Name: "mw", Scopes: []string{"audit:read"}, Source: netip.MustParseAddr("2001:db8::5")}
	if !gotOK || got.TokenID != want.TokenID || got.Name != want.Name || !slices.Equal(got.Scopes, want.Scopes) || got.Source != want.Source {
		t.Fatalf("principal = %+v (%v), want %+v", got, gotOK, want)
	}

	gotOK = false
	if code := serve(http.MethodGet, "/healthz", ""); code != http.StatusNoContent || gotOK {
		t.Fatalf("GET /healthz: %d, principal set %v; want exempt with no principal", code, gotOK)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/healthz"},
		{http.MethodGet, "/healthz/"},
		{http.MethodGet, "/v1/healthz"},
		{http.MethodGet, "/healthzx"},
		{http.MethodGet, "//healthz"},
	} {
		if code := serve(c.method, c.path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d, want 401", c.method, c.path, code)
		}
	}
}

// CR-13: a concurrent burst of bad tokens from one peer reaches token
// evaluation exactly limit times; the rest are refused with 429 before it.
func TestMiddlewareBurstOfBadTokensIsLimitedExactly(t *testing.T) {
	const limit, burst = 10, 500
	dir := t.TempDir()
	a := New(dir, Options{Logger: quietLog(), FailureLimitPerMinute: limit})
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	codes := make(chan int, burst)
	start := make(chan struct{})
	for range burst {
		go func() {
			r := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
			r.RemoteAddr = "192.0.2.77:1234"
			r.Header.Set("Authorization", "Bearer not-a-token")
			w := httptest.NewRecorder()
			<-start
			h.ServeHTTP(w, r)
			codes <- w.Code
		}()
	}
	close(start)
	count := map[int]int{}
	for range burst {
		count[<-codes]++
	}
	if count[http.StatusUnauthorized] != limit || count[http.StatusTooManyRequests] != burst-limit {
		t.Fatalf("status counts %v, want %d x 401 and %d x 429", count, limit, burst-limit)
	}
}
