// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/provider"
)

// mcpProbeOps are read ops with MCP bindings, one per real config section,
// so the real config file enables and disables their tools.
var mcpProbeOps = map[string]def.Typed[probeIn, probeOut]{}

func init() {
	for _, p := range []string{"apt", "docker"} {
		mcpProbeOps[p] = def.Define[probeIn, probeOut](def.Op{
			ID: p + ".mcpprobes.get", Provider: p, Kind: def.KindGet, ResourceType: "mcpprobes",
			Summary: "s", Description: "MCP probe operation for the daemon tests.",
			Scope: def.Scope{Name: p + ":read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
			CLI: def.CLIBinding{Noun: "mcpprobe", Verb: "get", Positional: "name"},
			MCP: &def.MCPBinding{Tool: "get_" + p + "_probe", Description: "Get a probe."},
		})
	}
}

// mcpProbeProvider's handler, when gate is set, reports on entered and
// waits for gate before answering.
type mcpProbeProvider struct {
	name    string
	gate    chan struct{}
	entered chan struct{}
}

func (p *mcpProbeProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: p.name, ReadScope: p.name + ":read", Ops: []def.Op{mcpProbeOps[p.name].Op()}}
}

func (p *mcpProbeProvider) Configure(decode func(any) error) error {
	var c struct {
		Enabled bool `yaml:"enabled"`
	}
	return decode(&c)
}

func (p *mcpProbeProvider) Preflight(context.Context) provider.Status { return ok() }

func (p *mcpProbeProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Sync(mcpProbeOps[p.name], func(ctx context.Context, in probeIn) (probeOut, error) {
		if p.gate != nil {
			p.entered <- struct{}{}
			select {
			case <-p.gate:
			case <-ctx.Done():
				return probeOut{}, ctx.Err()
			}
		}
		return probeOut(in), nil
	})}
}

// syncBuffer collects log output written from the daemon's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

// mcpDaemon is an in-process daemon, so a test can gate its handlers, with
// a token per scope set.
type mcpDaemon struct {
	base    string
	cfgPath string
	listen  string
	logs    *syncBuffer
	tokens  map[string]string
}

func startMCPDaemon(t *testing.T, providers []provider.Provider, scopes map[string][]string) *mcpDaemon {
	t.Helper()
	m := &mcpDaemon{cfgPath: filepath.Join(t.TempDir(), "nexusd.yaml"), listen: freePort(t), logs: &syncBuffer{},
		tokens: map[string]string{}}
	writeConfig(t, m.cfgPath, m.listen, "apt", "docker")
	cfg, err := config.Load(m.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	for name, sc := range scopes {
		tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: name, Scopes: sc})
		if err != nil {
			t.Fatal(err)
		}
		m.tokens[name] = tok
	}
	d, err := New(cfg, Options{Providers: providers, ConfigPath: m.cfgPath,
		Logger: slog.New(slog.NewJSONHandler(m.logs, nil)), StateDir: stateDir, AuditDir: t.TempDir(),
		Detach: detachtest.Local()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
	select {
	case <-d.Ready():
	case err := <-done:
		t.Fatalf("daemon stopped before ready: %v", err)
	}
	m.base = "http://" + m.listen
	return m
}

// reload rewrites the config with enabled providers, sends this process
// SIGHUP, which the running daemon's Run loop receives, and waits until
// the reload is logged.
func (m *mcpDaemon) reload(t *testing.T, enabled ...string) {
	t.Helper()
	before := m.logs.count("configuration reloaded")
	writeConfig(t, m.cfgPath, m.listen, enabled...)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for m.logs.count("configuration reloaded") == before {
		if time.Now().After(deadline) {
			t.Fatalf("reload not logged:\n%s", m.logs.buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rpc posts one 2026-07-28 JSON-RPC request to /mcp and returns its result.
func (m *mcpDaemon) rpc(t *testing.T, token, method string, params map[string]any) json.RawMessage {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "daemon-test", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	// It may run on a goroutine of its own, so it reports with Errorf only.
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Errorf("%s: %v", method, err)
		return nil
	}
	req, err := http.NewRequest(http.MethodPost, m.base+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Errorf("%s: %v", method, err)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		req.Header.Set("Mcp-Name", name)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Errorf("%s: %v", method, err)
		return nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("%s: %v", method, err)
		return nil
	}
	var msg struct {
		Result json.RawMessage `json:"result"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &msg) != nil || msg.Result == nil {
		t.Errorf("%s: %d %s", method, resp.StatusCode, b)
	}
	return msg.Result
}

func (m *mcpDaemon) tools(t *testing.T, token string) []string {
	t.Helper()
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(m.rpc(t, token, "tools/list", nil), &res); err != nil {
		t.Fatal(err)
	}
	if res.NextCursor != "" {
		t.Fatalf("tools/list is paginated (%q); the test reads one page", res.NextCursor)
	}
	names := make([]string, len(res.Tools))
	for i, tool := range res.Tools {
		names[i] = tool.Name
	}
	return names
}

// TC-143: the tool list is deterministic, the same for every token, and
// follows provider enablement on SIGHUP without a restart (second-state).
func TestMCPToolListDeterministicAndReloaded(t *testing.T) {
	m := startMCPDaemon(t, []provider.Provider{&mcpProbeProvider{name: "apt"}, &mcpProbeProvider{name: "docker"}},
		map[string][]string{"apt": {"apt:read"}, "docker": {"docker:read"}, "none": nil})

	// Step 1.
	var first []string
	for _, name := range []string{"apt", "docker", "none"} {
		for i := range 2 {
			got := m.tools(t, m.tokens[name])
			if first == nil {
				first = got
			}
			if !slices.Equal(got, first) {
				t.Errorf("token %s, list %d: %v, want %v", name, i+1, got, first)
			}
		}
	}
	if !slices.IsSorted(first) {
		t.Errorf("tool list %v is not sorted", first)
	}
	if !slices.Contains(first, "get_apt_probe") || !slices.Contains(first, "get_docker_probe") ||
		!slices.Contains(first, "get_capabilities") {
		t.Fatalf("tool list %v lacks a provider's or the system tools", first)
	}

	// Step 2: disable docker (B).
	m.reload(t, "apt")
	got := m.tools(t, m.tokens["apt"])
	if slices.Contains(got, "get_docker_probe") || !slices.Contains(got, "get_apt_probe") {
		t.Fatalf("after disabling docker: %v", got)
	}
	want := slices.DeleteFunc(slices.Clone(first), func(s string) bool { return s == "get_docker_probe" })
	if !slices.Equal(got, want) {
		t.Errorf("after disabling docker: %v, want %v", got, want)
	}
}

// TC-E26: a reload during an in-flight tool call lets the call finish, and
// the next list reflects the change.
func TestReloadDuringInFlightToolCall(t *testing.T) {
	apt := &mcpProbeProvider{name: "apt", gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	m := startMCPDaemon(t, []provider.Provider{apt, &mcpProbeProvider{name: "docker"}},
		map[string][]string{"apt": {"apt:read"}})
	tok := m.tokens["apt"]
	if !slices.Contains(m.tools(t, tok), "get_docker_probe") {
		t.Fatal("docker tool missing before the reload")
	}

	// Step 1: start a call, reload with a changed tool list, release.
	result := make(chan json.RawMessage, 1)
	go func() {
		result <- m.rpc(t, tok, "tools/call", map[string]any{"name": "get_apt_probe", "arguments": map[string]any{"name": "inflight"}})
	}()
	select {
	case <-apt.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the call never reached its handler")
	}
	m.reload(t, "apt")
	if got := m.tools(t, tok); slices.Contains(got, "get_docker_probe") {
		t.Errorf("list during the call still has docker: %v", got)
	}
	close(apt.gate)
	var res struct {
		IsError           bool     `json:"isError"`
		StructuredContent probeOut `json:"structuredContent"`
	}
	select {
	case raw := <-result:
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("in-flight call: %v %s", err, raw)
		}
		if res.IsError || res.StructuredContent.Name != "inflight" {
			t.Errorf("in-flight call: %s", raw)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight call did not complete")
	}
	if got := m.tools(t, tok); slices.Contains(got, "get_docker_probe") || !slices.Contains(got, "get_apt_probe") {
		t.Errorf("next list: %v, want apt without docker", got)
	}
}

// mcp.allowedOrigins is fixed at startup with the Origin guard, so a change
// is reported as needing a restart rather than silently kept.
func TestRestartRequiredReportsMCPChanges(t *testing.T) {
	running := config.Default()
	next := config.Default()
	next.MCP.AllowedOrigins = []string{"https://ok.example"}
	if keys := restartRequired(&running, &next); !slices.Equal(keys, []string{"mcp"}) {
		t.Fatalf("changed mcp.allowedOrigins: %v, want [mcp]", keys)
	}
	if keys := restartRequired(&next, &next); len(keys) != 0 {
		t.Fatalf("identical configurations: %v, want none", keys)
	}
}
