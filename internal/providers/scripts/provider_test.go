package scripts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/scripts"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// node is a real nexusd: daemon.New and Run in this process, with real
// listeners, configuration file, SIGHUP handling, Dispatcher and audit log.
type node struct {
	base     string
	cfgPath  string
	stateDir string
	auditDir string
	tokens   map[string]string // Scope set name -> token
}

// enabled is the test seam that stands in for systemd on T0, where there is
// none: the real preflight is exercised by TestMissingSystemdMakesScriptsUnavailable.
func enabled(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// configYAML is a loopback, plain-HTTP configuration with scripts enabled
// and catalog as the indented catalog entries.
func configYAML(listen, catalog string) string {
	return fmt.Sprintf("listen: [%q]\ntls: {mode: off}\nproviders:\n  scripts:\n    enabled: true\n    catalog:\n%s", listen, catalog)
}

// start runs a daemon over providers (nil: the compiled-in providers) with
// the scripts catalog given.
func start(t *testing.T, providers []provider.Provider, catalog string) *node {
	t.Helper()
	dir := t.TempDir()
	stateDir, auditDir := filepath.Join(dir, "state"), filepath.Join(dir, "audit")
	n := &node{cfgPath: filepath.Join(dir, "nexusd.yaml"), stateDir: stateDir, auditDir: auditDir, tokens: map[string]string{}}
	for name, scopes := range map[string][]string{
		"read": {spec.ReadScope},
		"run":  {spec.RunScope},
		"full": {spec.ReadScope, spec.RunScope, "audit:read", "containers:read", "containers:write",
			"packages:read", "packages:write", "packages:install"},
		"none": nil,
	} {
		tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: name, Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		n.tokens[name] = tok
	}
	writeFile(t, n.cfgPath, configYAML(freePort(t), catalog))
	cfg, err := config.Load(n.cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	// Detached runs go through detachtest.Local: T0 has no systemd.
	d, err := daemon.New(cfg, daemon.Options{Providers: providers, ConfigPath: n.cfgPath, StateDir: stateDir,
		AuditDir: auditDir, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Detach: detachtest.Local()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	select {
	case <-d.Ready():
	case err := <-done:
		t.Fatalf("Run returned before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon not ready")
	}
	n.base = "http://" + d.Addrs()[0].String()
	return n
}

type response struct {
	status      int
	contentType string
	body        []byte
}

func (n *node) do(t *testing.T, method, path, token, body string) response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, n.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: b}
}

func (r response) problem(t *testing.T) *problem.Error {
	t.Helper()
	var p problem.Error
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("status %d: not a problem: %v\n%s", r.status, err, r.body)
	}
	return &p
}

func (n *node) list(t *testing.T, token string) apitypes.List[spec.Script] {
	t.Helper()
	r := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts", token, "")
	if r.status != http.StatusOK {
		t.Fatalf("list: %d %s", r.status, r.body)
	}
	var l apitypes.List[spec.Script]
	if err := json.Unmarshal(r.body, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

const (
	sentinelEnv     = "sentinel-env-value-5d0c2a"
	sentinelPattern = "sentinel-pattern-8e41b7"
)

// sentinelCatalog has an env value and a sensitive parameter's pattern that
// must never appear in a response.
var sentinelCatalog = `      - id: deploy-api
        description: Pull main and redeploy the API container
        path: /opt/deploy/deploy-api.sh
        runAs: deploy
        workingDirectory: /opt/deploy
        timeout: 20m
        concurrency: 2
        terminationGrace: 10s
        env: {PATH: "/usr/bin:/bin", API_TOKEN: "` + sentinelEnv + `"}
        parameters:
          - {name: branch, type: string, pattern: '^[a-z]+$', arg: --branch}
          - {name: target, type: string, pattern: '^[a-z]+$'}
          - {name: apiKey, type: string, pattern: '^` + sentinelPattern + `$', sensitive: true, env: API_KEY}
      - id: backup
        description: Back up the database
        path: /opt/backup.sh
        runAs: root
        workingDirectory: /
`

// TC-89: list and get return every catalog field with env names only, the
// sentinels appear in no response byte, and an unknown ID is 404.
func TestCatalogReadsExposeNoEnvironmentValues(t *testing.T) {
	n := start(t, []provider.Provider{scripts.New(scripts.Options{Host: "testhost", Preflight: enabled})}, sentinelCatalog)
	tok := n.tokens["read"]

	lr := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts", tok, "")
	gr := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts/deploy-api", tok, "")
	for name, r := range map[string]response{"list": lr, "get": gr} {
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		for _, s := range []string{sentinelEnv, sentinelPattern} {
			if bytes.Contains(r.body, []byte(s)) {
				t.Errorf("%s response contains the sentinel %q:\n%s", name, s, r.body)
			}
		}
	}

	var l apitypes.List[spec.Script]
	if err := json.Unmarshal(lr.body, &l); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range l.Items {
		ids = append(ids, s.Name)
	}
	if !slices.Equal(ids, []string{"backup", "deploy-api"}) || l.NextCursor != "" {
		t.Fatalf("list names %q next %q, want both scripts sorted by ID on one page", ids, l.NextCursor)
	}

	var got spec.Script
	if err := json.Unmarshal(gr.body, &got); err != nil {
		t.Fatal(err)
	}
	want := spec.Script{
		ID: "/providers/scripts/scripts/deploy-api", Type: "scripts", Name: "deploy-api", Host: "testhost",
		Properties: spec.ScriptProperties{
			Description: "Pull main and redeploy the API container", Path: "/opt/deploy/deploy-api.sh",
			RunAs: "deploy", WorkingDirectory: "/opt/deploy", TimeoutSeconds: 1200, Concurrency: 2,
			TerminationGraceSeconds: 10, Env: []string{"API_TOKEN", "PATH"},
			Parameters: []spec.Parameter{
				{Name: "branch", Type: "string", Pattern: "^[a-z]+$", Arg: "--branch"},
				{Name: "target", Type: "string", Pattern: "^[a-z]+$", Position: 1},
				{Name: "apiKey", Type: "string", Sensitive: true, Env: "API_KEY"},
			},
		},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("get:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if !slices.ContainsFunc(l.Items, func(s spec.Script) bool {
		b, _ := json.Marshal(s)
		return bytes.Equal(b, wantJSON)
	}) {
		t.Errorf("list does not hold the same deploy-api as get:\n%s", lr.body)
	}
	backup := l.Items[0].Properties
	if backup.TimeoutSeconds != 3600 || backup.Concurrency != 1 || backup.TerminationGraceSeconds != 30 ||
		backup.Env == nil || backup.Parameters == nil {
		t.Errorf("backup defaults %+v, want 3600s, 1, 30s and empty lists", backup)
	}

	r := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts/nope", tok, "")
	if r.status != http.StatusNotFound || r.problem(t).Slug() != problem.NotFound {
		t.Errorf("unknown ID: %d %s, want 404 not-found", r.status, r.body)
	}
	if r := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts", n.tokens["none"], ""); r.status != http.StatusForbidden {
		t.Errorf("list without scripts:read: %d, want 403", r.status)
	}
}

// The list pages by ID with an opaque cursor, and refuses one it did not
// issue.
func TestCatalogListPages(t *testing.T) {
	n := start(t, []provider.Provider{scripts.New(scripts.Options{Host: "testhost", Preflight: enabled})}, sentinelCatalog)
	tok := n.tokens["read"]
	var names []string
	path := "/v1/providers/scripts/scripts?limit=1"
	for range 3 {
		r := n.do(t, http.MethodGet, path, tok, "")
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, r.status, r.body)
		}
		var l apitypes.List[spec.Script]
		if err := json.Unmarshal(r.body, &l); err != nil {
			t.Fatal(err)
		}
		for _, s := range l.Items {
			names = append(names, s.Name)
		}
		if l.NextCursor == "" {
			break
		}
		path = "/v1/providers/scripts/scripts?limit=1&cursor=" + l.NextCursor
	}
	if !slices.Equal(names, []string{"backup", "deploy-api"}) {
		t.Fatalf("pages gave %q", names)
	}
	for _, q := range []string{"cursor=zzzz", "limit=0", "limit=1001", "filter=a=b"} {
		r := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts?"+q, tok, "")
		if r.status != http.StatusBadRequest || r.problem(t).Slug() != problem.ValidationFailed {
			t.Errorf("%s: %d %s, want 400 validation-failed", q, r.status, r.body)
		}
	}
}

// TC-90 (step 2): PUT, POST and DELETE to the list and item routes with a
// full-scope token are 404 or 405 problems, and the catalog is unchanged.
func TestCatalogCannotBeChangedOverHTTP(t *testing.T) {
	n := start(t, []provider.Provider{scripts.New(scripts.Options{Host: "testhost", Preflight: enabled})}, sentinelCatalog)
	full := n.tokens["full"]
	before := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts", full, "")
	body := `{"id":"evil","description":"x","path":"/bin/sh","runAs":"root","workingDirectory":"/"}`
	for _, path := range []string{
		"/v1/providers/scripts/scripts", "/v1/providers/scripts/scripts/deploy-api", "/v1/providers/scripts/scripts/evil",
	} {
		for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch} {
			r := n.do(t, method, path, full, body)
			if r.status != http.StatusNotFound && r.status != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d %s, want 404 or 405", method, path, r.status, r.body)
				continue
			}
			if r.contentType != "application/problem+json" {
				t.Errorf("%s %s: Content-Type %q", method, path, r.contentType)
			}
			if slug := r.problem(t).Slug(); slug != problem.RouteNotFound && slug != problem.MethodNotAllowed {
				t.Errorf("%s %s: problem %s", method, path, slug)
			}
		}
	}
	after := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts", full, "")
	if !bytes.Equal(before.body, after.body) {
		t.Fatalf("catalog changed:\nbefore %s\nafter  %s", before.body, after.body)
	}
	if r := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts/evil", full, ""); r.status != http.StatusNotFound {
		t.Fatalf("evil: %d", r.status)
	}
}

// versionCatalog is a catalog whose every description names version v.
func versionCatalog(v string, ids ...string) string {
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "      - id: %s\n        description: %s %s\n        path: /opt/%s.sh\n        runAs: deploy\n"+
			"        workingDirectory: /opt\n", id, v, id, id)
	}
	return b.String()
}

// TC-92 (second-state): with a reader running throughout, a SIGHUP reload
// swaps the catalog whole: every read returns entirely the old or entirely
// the new catalog, then new entries appear and removed ones are 404.
func TestCatalogReloadSwapsAtomically(t *testing.T) {
	n := start(t, []provider.Provider{scripts.New(scripts.Options{Host: "testhost", Preflight: enabled})},
		versionCatalog("v1", "alpha", "bravo", "charlie"))
	tok := n.tokens["read"]
	versions := map[string][]string{
		"v1": {"alpha", "bravo", "charlie"},
		"v2": {"bravo", "charlie", "delta", "echo"},
	}
	// whole reports which version a list is, or why it is neither.
	whole := func(l apitypes.List[spec.Script]) (string, error) {
		if len(l.Items) == 0 {
			return "", fmt.Errorf("empty list")
		}
		v, _, _ := strings.Cut(l.Items[0].Properties.Description, " ")
		var ids []string
		for _, s := range l.Items {
			if s.Properties.Description != v+" "+s.Name {
				return "", fmt.Errorf("mixed versions: %q in a %s list", s.Properties.Description, v)
			}
			ids = append(ids, s.Name)
		}
		if !slices.Equal(ids, versions[v]) {
			return "", fmt.Errorf("%s list has %q, want %q", v, ids, versions[v])
		}
		return v, nil
	}

	// Step 1.
	if v, err := whole(n.list(t, tok)); err != nil || v != "v1" {
		t.Fatalf("step 1: %s %v", v, err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]int{}
	var torn []string
	wg.Go(func() {
		client := &http.Client{Timeout: 5 * time.Second}
		for {
			select {
			case <-stop:
				return
			default:
			}
			req, _ := http.NewRequest(http.MethodGet, n.base+"/v1/providers/scripts/scripts", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := client.Do(req)
			if err != nil {
				mu.Lock()
				torn = append(torn, err.Error())
				mu.Unlock()
				return
			}
			var l apitypes.List[spec.Script]
			err = json.NewDecoder(resp.Body).Decode(&l)
			_ = resp.Body.Close()
			v, verr := whole(l)
			mu.Lock()
			switch {
			case err != nil:
				torn = append(torn, err.Error())
			case verr != nil:
				torn = append(torn, verr.Error())
			default:
				seen[v]++
			}
			mu.Unlock()
		}
	})

	// Step 2: change the file and reload.
	writeFile(t, n.cfgPath, configYAML(strings.TrimPrefix(n.base, "http://"),
		versionCatalog("v2", "bravo", "charlie", "delta", "echo")))
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if v, _ := whole(n.list(t, tok)); v == "v2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reloaded catalog never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Keep reading across the swap for a moment longer.
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(torn) > 0 {
		t.Fatalf("%d reads were neither catalog, first: %s", len(torn), torn[0])
	}
	if seen["v1"] == 0 || seen["v2"] == 0 {
		t.Fatalf("the reader saw %v, want reads of both catalogs", seen)
	}

	// Step 3.
	if r := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts/alpha", tok, ""); r.status != http.StatusNotFound {
		t.Errorf("removed alpha: %d, want 404", r.status)
	}
	r := n.do(t, http.MethodGet, "/v1/providers/scripts/scripts/delta", tok, "")
	if r.status != http.StatusOK || !bytes.Contains(r.body, []byte(`"v2 delta"`)) {
		t.Errorf("new delta: %d %s", r.status, r.body)
	}
}

// TC-ERR25: on a host without systemd, the compiled-in scripts provider is
// unavailable with the reason, invoke answers 503 capability-unavailable,
// and nothing runs: no child, no operation.
func TestMissingSystemdMakesScriptsUnavailable(t *testing.T) {
	if detach.SystemdAvailable().Available {
		t.Skip("this host runs systemd; the T0 case needs a host without it")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "touch.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	n := start(t, nil, fmt.Sprintf("      - id: touch\n        description: Touch a marker\n        path: %s\n"+
		"        runAs: nobody\n        workingDirectory: %s\n", script, dir))

	r := n.do(t, http.MethodGet, "/v1/capabilities", n.tokens["none"], "")
	var caps apitypes.Capabilities
	if err := json.Unmarshal(r.body, &caps); err != nil {
		t.Fatalf("capabilities: %d %s", r.status, r.body)
	}
	i := slices.IndexFunc(caps.Providers, func(p apitypes.ProviderCapability) bool { return p.Name == spec.Name })
	if i < 0 {
		t.Fatalf("scripts is not compiled in: %s", r.body)
	}
	if p := caps.Providers[i]; p.Status != apitypes.ProviderUnavailable || !strings.Contains(p.Reason, "systemd") {
		t.Fatalf("scripts capability %+v, want unavailable naming systemd", p)
	}

	r = n.do(t, http.MethodPost, "/v1/providers/scripts/scripts/touch/invoke", n.tokens["run"], "{}")
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("invoke: %d %s, want 503", r.status, r.body)
	}
	p := r.problem(t)
	if p.Slug() != problem.CapabilityUnavailable || p.Extensions["provider"] != spec.Name ||
		!strings.Contains(fmt.Sprint(p.Extensions["reason"]), "systemd") {
		t.Fatalf("invoke problem %s", r.body)
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the script ran: stat marker: %v", err)
	}
	r = n.do(t, http.MethodGet, "/v1/operations", n.tokens["full"], "")
	var ops apitypes.List[apitypes.Operation]
	if err := json.Unmarshal(r.body, &ops); err != nil || len(ops.Items) != 0 {
		t.Fatalf("operations: %d %s, want none", r.status, r.body)
	}
}
