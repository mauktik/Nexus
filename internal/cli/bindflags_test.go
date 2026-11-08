package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/def"
)

type capturedRequest struct {
	method, path, rawQuery string
	header                 http.Header
	body                   []byte
}

// captureServer records every request and answers 202 with an operation.
type captureServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []capturedRequest
}

func startCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.reqs = append(cs.reqs, capturedRequest{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), body})
		cs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"0190a5d2-7c3e-7f00-8a00-000000000001","status":"Queued"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *captureServer) take() []capturedRequest {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	r := cs.reqs
	cs.reqs = nil
	return r
}

// urlEnv targets srv by URL with a token, so no configuration is read.
func urlEnv(srv *captureServer) Env {
	return Env{Getenv: func(k string) string {
		switch k {
		case clientconfig.EnvHost:
			return srv.URL
		case clientconfig.EnvToken:
			return "tok"
		}
		return ""
	}}
}

func runSpecsCLI(env Env, specs []def.ProviderSpec, args ...string) runResult {
	var stdout, stderr strings.Builder
	env.Stdout, env.Stderr = &stdout, &stderr
	code := runWithSpecs(args, env, specs)
	return runResult{code, stdout.String(), stderr.String()}
}

// TC-160: flags come from the input schema; bad values exit 2 with nothing
// sent; script parameters and the Idempotency-Key header are bound.
func TestFlagsFromInputSchemas(t *testing.T) {
	srv := startCaptureServer(t)
	env := urlEnv(srv)
	specs := widgetSpecs()

	r := runSpecsCLI(env, specs, "widget", "update", "--label", "hello world", "--count", "3", "--force",
		"--color", "green", "--tags", "a", "--tags", "b,c", "--idempotency-key", "K-1")
	if r.code != 0 {
		t.Fatalf("widget update exited %d: %s", r.code, r.stderr)
	}
	reqs := srv.take()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if got.method != http.MethodPost || got.path != "/v1/providers/testprov/actions/frob" || got.rawQuery != "" {
		t.Errorf("request %s %s?%s", got.method, got.path, got.rawQuery)
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("body %q: %v", got.body, err)
	}
	want := map[string]any{"label": "hello world", "count": float64(3), "force": true, "color": "green", "tags": []any{"a", "b,c"}}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body %v, want %v", body, want)
	}
	if k := got.header.Get("Idempotency-Key"); k != "K-1" {
		t.Errorf("Idempotency-Key %q", k)
	}
	if !strings.Contains(r.stdout, `"status":"Queued"`) {
		t.Errorf("stdout %q does not hold the operation", r.stdout)
	}

	// Unset flags are not sent.
	if r := runSpecsCLI(env, specs, "widget", "update", "--count", "0"); r.code != 0 {
		t.Fatalf("widget update --count 0 exited %d: %s", r.code, r.stderr)
	}
	if reqs := srv.take(); len(reqs) != 1 || string(reqs[0].body) != `{"count":0}` {
		t.Errorf("explicit zero: %+v", reqs)
	}

	for _, bad := range [][]string{
		{"widget", "update", "--color", "purple"},
		{"widget", "update", "--count", "three"},
		{"widget", "update", "--bogus", "x"},
		{"widget", "update", "extra-arg"},
		{"widget", "update", "--idempotency-key", strings.Repeat("k", 256)},
		{"widget", "update", "--idempotency-key", "bad\x01key"},
		{"script", "invoke", "x", "--param", "noequals"},
		{"script", "invoke", "x", "--param", "a=1", "--param", "a=2"},
		{"script", "invoke", "bad/name"},
		{"container", "get", "--limit", "0"},
		{"container", "get", "web", "--limit", "5"},
		{"container", "get", "--idempotency-key", "K"},
	} {
		r := runSpecsCLI(env, specs, bad...)
		if r.code != ExitUsage {
			t.Errorf("nexus %q exited %d, want 2: %s", bad, r.code, r.stderr)
		}
		if reqs := srv.take(); len(reqs) != 0 {
			t.Errorf("nexus %q sent %d requests", bad, len(reqs))
		}
	}

	r = runSpecsCLI(env, specs, "script", "invoke", "x", "--param", "a=1", "--param", "b=2=3", "--idempotency-key", "K")
	if r.code != 0 {
		t.Fatalf("script invoke exited %d: %s", r.code, r.stderr)
	}
	reqs = srv.take()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	if reqs[0].method != http.MethodPost || reqs[0].path != "/v1/providers/scripts/scripts/x/invoke" {
		t.Errorf("script invoke: %s %s", reqs[0].method, reqs[0].path)
	}
	if string(reqs[0].body) != `{"parameters":{"a":"1","b":"2=3"}}` {
		t.Errorf("script invoke body %s", reqs[0].body)
	}
	if k := reqs[0].header.Get("Idempotency-Key"); k != "K" {
		t.Errorf("script invoke Idempotency-Key %q", k)
	}
	if a := reqs[0].header.Get("Authorization"); a != "Bearer tok" {
		t.Errorf("Authorization %q", a)
	}

	// A read: query parameters, repeatable for an array, and no key header.
	r = runSpecsCLI(env, specs, "container", "get", "--limit", "5", "--filter", "status=upToDate", "--filter", "status=unknown")
	if r.code != 0 {
		t.Fatalf("container get exited %d: %s", r.code, r.stderr)
	}
	reqs = srv.take()
	if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].path != "/v1/providers/docker/containers" ||
		reqs[0].rawQuery != "filter=status%3DupToDate&filter=status%3Dunknown&limit=5" || len(reqs[0].body) != 0 {
		t.Errorf("container get: %+v", reqs)
	}

	// Names bind as arguments where the op takes a names array.
	r = runSpecsCLI(env, specs, "package", "install", "curl", "jq", "--what-if")
	if r.code != 0 {
		t.Fatalf("package install exited %d: %s", r.code, r.stderr)
	}
	reqs = srv.take()
	if len(reqs) != 1 || string(reqs[0].body) != `{"names":["curl","jq"],"whatIf":true}` {
		t.Errorf("package install: %+v", reqs)
	}
}
