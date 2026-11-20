// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests for planning/system-provider/test-cases.md TC-HP-108, TC-HP-110,
// TC-HP-E27 and TC-HP-ERR29 (WI 5.1): the host provider composed into
// nexusd and the CLI. The harness is composition_test.go.

package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// startedBackground reports whether the daemon log has the background
// mechanism starting the host provider's RunBackground.
func startedBackground(t *testing.T, logs string) bool {
	t.Helper()
	for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
		var rec struct {
			Msg      string `json:"msg"`
			Provider string `json:"provider"`
		}
		if json.Unmarshal([]byte(l), &rec) == nil && rec.Msg == "starting background provider" &&
			rec.Provider == spec.Name {
			return true
		}
	}
	return false
}

// hostCapability is the host entry of GET /v1/capabilities?refresh=true.
func hostCapability(t *testing.T, c *composed, token string) apitypes.ProviderCapability {
	t.Helper()
	var caps apitypes.Capabilities
	if err := json.Unmarshal(getOK(t, c.base+"/v1/capabilities?refresh=true", token), &caps); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(caps.Providers, func(p apitypes.ProviderCapability) bool { return p.Name == spec.Name })
	if i < 0 {
		t.Fatalf("capabilities list no %s: %+v", spec.Name, caps.Providers)
	}
	return caps.Providers[i]
}

// waitGoroutines waits up to d for the goroutine count to fall to at most
// limit and returns the last count.
func waitGoroutines(limit int, d time.Duration) int {
	deadline := time.Now().Add(d)
	n := runtime.NumGoroutine()
	for n > limit && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// TC-HP-108: Composition: capabilities, OpenAPI, CLI catalog and the Background wiring. Owner WI 5.1. Requirements: FR-1, FR-3, FR-14, FR-11, CON-4.
func TestTCHP108_CompositionCapabilitiesOpenAPICLICatalogAndThe(t *testing.T) {
	// The CLI's generated command tree has the ten host commands.
	var stdout, stderr bytes.Buffer
	if code := cli.Run([]string{"command", "get"}, cli.Env{Stdout: &stdout, Stderr: &stderr}); code != 0 {
		t.Fatalf("command get: exit %d: %s", code, stderr.String())
	}
	var commands []string
	for _, l := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var c struct {
			Noun string   `json:"noun"`
			Verb string   `json:"verb"`
			Ops  []string `json:"ops"`
		}
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		if slices.ContainsFunc(c.Ops, func(id string) bool { return strings.HasPrefix(id, spec.Name+".") }) {
			commands = append(commands, c.Noun+" "+c.Verb)
		}
	}
	var want []string
	for _, op := range spec.Spec().Ops {
		want = append(want, op.CLI.Noun+" "+op.CLI.Verb)
	}
	slices.Sort(commands)
	slices.Sort(want)
	if len(want) != 10 || !slices.Equal(commands, want) {
		t.Errorf("host commands %v, want the ten %v", commands, want)
	}

	listen := freeListen(t)
	c := startComposed(t, listen, hostOnlyConfig(listen))
	token := c.token(t, "composed", spec.ReadScope)

	// Capabilities: the YAML's enabled reaches the provider (providerConfigs),
	// and its preflight decides the state.
	got := hostCapability(t, c, token)
	switch runtime.GOOS {
	case "linux":
		if got.Status != apitypes.ProviderEnabled {
			t.Errorf("host capability %+v, want enabled on linux", got)
		}
	default:
		want := "the host provider reads Linux /proc and /sys; this host is " + runtime.GOOS
		if got.Status != apitypes.ProviderUnavailable || got.Reason != want {
			t.Errorf("host capability %+v, want unavailable: %q", got, want)
		}
	}

	// OpenAPI: exactly the ten GET routes, each requiring host:read.
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID   string `json:"operationId"`
			RequiredScope struct {
				Scopes []string `json:"scopes"`
			} `json:"x-required-scope"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(getOK(t, c.base+"/v1/openapi.json", token), &doc); err != nil {
		t.Fatal(err)
	}
	var routes []string
	for path, methods := range doc.Paths {
		for method, op := range methods {
			if !strings.HasPrefix(op.OperationID, spec.Name+".") && !strings.HasPrefix(path, "/v1/providers/host/") {
				continue
			}
			routes = append(routes, strings.ToUpper(method)+" "+path)
			if !slices.Equal(op.RequiredScope.Scopes, []string{spec.ReadScope}) {
				t.Errorf("%s %s: x-required-scope %v, want [%s]", method, path, op.RequiredScope.Scopes, spec.ReadScope)
			}
		}
	}
	slices.Sort(routes)
	wantRoutes := []string{
		"GET /v1/providers/host/cpu", "GET /v1/providers/host/disks", "GET /v1/providers/host/filesystems",
		"GET /v1/providers/host/health", "GET /v1/providers/host/identity", "GET /v1/providers/host/interfaces",
		"GET /v1/providers/host/memory", "GET /v1/providers/host/metrics", "GET /v1/providers/host/netconfig",
		"GET /v1/providers/host/summary",
	}
	if !slices.Equal(routes, wantRoutes) {
		t.Errorf("host routes %v, want %v", routes, wantRoutes)
	}

	// The Background wiring: the value builtinProviders returns under host
	// is a provider.Background (the static assertion in
	// composition_test.go), and the daemon starts it because the section is
	// enabled, whatever the preflight says.
	deadline := time.Now().Add(5 * time.Second)
	for !startedBackground(t, c.log.String()) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !startedBackground(t, c.log.String()) {
		t.Errorf("the daemon did not start the host provider's RunBackground; log:\n%s", c.log.String())
	}
}

// captureDefault routes slog.Default to a buffer until the test ends.
func captureDefault(t *testing.T) *logBuffer {
	t.Helper()
	logs := &logBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// TC-HP-110: validate-config with the host provider enabled never starts its sampler. Owner WI 5.1. Requirements: FR-1, NFR-2.
//
// CheckConfig builds the providers with a logger of its own that discards,
// so the goroutine count is the evidence that no sampler started; any log
// through slog.Default is captured as well.
func TestTCHP110_ValidateConfigWithTheHostProviderEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	body := "host: {name: h1}\nlisten: [\"127.0.0.1:7443\"]\ntls: {mode: off}\nproviders:\n" +
		"  host: {enabled: true, sampleInterval: 2s}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureDefault(t)
	base := runtime.NumGoroutine()

	cfg, errs := daemon.CheckConfig(path)
	if len(errs) != 0 || cfg == nil {
		t.Fatalf("CheckConfig: %v", errs)
	}
	if !cfg.Providers.Host.Settings.Enabled || cfg.Providers.Host.Settings.SampleInterval != 2*time.Second {
		t.Fatalf("the checked configuration has host %+v, want enabled at 2s", cfg.Providers.Host.Settings)
	}

	// Longer than one interval: a sampler started by the check would be
	// running now.
	time.Sleep(3 * time.Second)
	if n := waitGoroutines(base, time.Second); n > base {
		t.Errorf("goroutines %d after CheckConfig, baseline %d", n, base)
	}
	if l := logs.String(); strings.Contains(l, "sampler") || strings.Contains(l, "background") {
		t.Errorf("CheckConfig logged a sampler or background start:\n%s", l)
	}
}

// TC-HP-E27: The provider is disabled by default. Owner WI 5.1. Requirements: FR-1, NFR-2.
func TestTCHPE27_TheProviderIsDisabledByDefault(t *testing.T) {
	listen := freeListen(t)
	c := startComposed(t, listen, defaultConfig(t, listen))
	token := c.token(t, "default", spec.ReadScope)

	if got := hostCapability(t, c, token); got.Status != apitypes.ProviderDisabled {
		t.Errorf("host capability %+v, want disabled", got)
	}
	status, b := get(t, c.base+"/v1/providers/host/summary", token)
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil || p.Slug() != problem.CapabilityUnavailable {
		t.Errorf("GET summary of a disabled provider: %d %s, want capability-unavailable", status, b)
	}

	base := runtime.NumGoroutine()
	time.Sleep(2 * time.Second)
	if n := waitGoroutines(base, time.Second); n > base {
		t.Errorf("goroutines %d after 2s, %d after the calls", n, base)
	}
	if startedBackground(t, c.log.String()) {
		t.Errorf("the daemon started the disabled host provider's RunBackground; log:\n%s", c.log.String())
	}
}

// writeHome writes a client configuration whose default host is url with
// token, under a fresh HOME, and returns the CLI's environment.
func writeHome(t *testing.T, url, token string, stdout, stderr *bytes.Buffer) cli.Env {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "defaultHost: h1\nhosts:\n  h1:\n    url: " + url + "\n    token: " + token + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cli.Env{Stdout: stdout, Stderr: stderr, Getenv: func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}}
}

// mcpSession connects the SDK client to the daemon's /mcp with token.
func mcpSession(t *testing.T, base, token string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "host-composition", Version: "1.0.0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{
			Endpoint:             base + "/mcp",
			HTTPClient:           &http.Client{Timeout: 10 * time.Second, Transport: bearer{token}},
			DisableStandaloneSSE: true,
			MaxRetries:           -1,
		}, nil)
	if err != nil {
		t.Fatalf("MCP connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// TC-HP-ERR29: Host operations refuse tokens without host:read on every surface. Owner WI 5.1. Requirements: FR-2.
//
// The test case's "insufficient-scope" is the registered forbidden problem
// with requiredScope, the only problem a missing scope produces.
func TestTCHPERR29_HostOperationsRefuseTokensWithoutHostRead(t *testing.T) {
	listen := freeListen(t)
	c := startComposed(t, listen, hostOnlyConfig(listen))
	other := c.token(t, "containers", "containers:read")
	reader := c.token(t, "reader", spec.ReadScope)
	summary := c.base + "/v1/providers/host/summary"

	// REST.
	status, b := get(t, summary, other)
	var p problem.Error
	if status != http.StatusForbidden || json.Unmarshal(b, &p) != nil || p.Slug() != problem.Forbidden ||
		p.Extensions["requiredScope"] != spec.ReadScope {
		t.Errorf("GET summary with containers:read: %d %s, want 403 forbidden naming %s", status, b, spec.ReadScope)
	}
	// With the scope the call passes authorization. Off Linux the provider
	// is unavailable, so the answer is capability-unavailable, not 200.
	status, b = get(t, summary, reader)
	if runtime.GOOS == "linux" && status != http.StatusOK {
		t.Errorf("GET summary with %s: %d %s, want 200", spec.ReadScope, status, b)
	} else if status == http.StatusForbidden || status == http.StatusUnauthorized {
		t.Errorf("GET summary with %s: %d %s, want past authorization", spec.ReadScope, status, b)
	}
	// system.* needs no host:read.
	getOK(t, c.base+"/v1/capabilities", other)

	// MCP.
	call := func(token string) *mcp.CallToolResult {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := mcpSession(t, c.base, token).CallTool(ctx, &mcp.CallToolParams{Name: "get_host_summary",
			Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("tools/call get_host_summary: %v", err)
		}
		return res
	}
	res := call(other)
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	p = problem.Error{}
	if !res.IsError || json.Unmarshal(raw, &p) != nil || p.Slug() != problem.Forbidden ||
		p.Extensions["requiredScope"] != spec.ReadScope {
		t.Errorf("MCP get_host_summary with containers:read: isError %v %s, want forbidden naming %s", res.IsError, raw,
			spec.ReadScope)
	}
	res = call(reader)
	raw, err = json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && res.IsError {
		t.Errorf("MCP get_host_summary with %s: %s, want a result", spec.ReadScope, raw)
	} else if res.IsError && strings.Contains(string(raw), string(problem.Forbidden)) {
		t.Errorf("MCP get_host_summary with %s: %s, want past authorization", spec.ReadScope, raw)
	}

	// CLI.
	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"hostsummary", "get"}, writeHome(t, c.base, other, &stdout, &stderr))
	if code != cli.ExitAuth || !strings.Contains(stderr.String(), spec.ReadScope) {
		t.Errorf("nexus hostsummary get with containers:read: exit %d, stderr %q; want %d naming %s", code,
			stderr.String(), cli.ExitAuth, spec.ReadScope)
	}
}
