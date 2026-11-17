package nexusd_test

// TC-SM-99 (WI 6.1): the composition. nexusd is in catalog.All(), so the
// CLI generates its commands, and in daemon's builtinProviders and
// providerConfigs, so a nexusd built from a config enabling it serves the
// provider in capabilities and the OpenAPI document. This is an external
// test: the composed daemon imports this package.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// composedDaemon runs daemon.New with the compiled-in providers from a
// config that enables only nexusd, and returns its base URL and a token
// holding nexusd:read. Detach is stubbed: nothing is launched.
func composedDaemon(t *testing.T) (base, token string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nexusd.yaml")
	body := fmt.Sprintf("host: {name: composed}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n"+
		"  docker: {enabled: false}\n  apt: {enabled: false}\n  scripts: {enabled: false}\n"+
		"  nexusd: {enabled: true}\n", listen)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	token, _, err = auth.Create(stateDir, auth.CreateRequest{Name: "composed", Scopes: []string{spec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(cfg, daemon.Options{
		ConfigPath: cfgPath, StateDir: stateDir, AuditDir: filepath.Join(dir, "audit"),
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Signals: make(chan os.Signal),
		Detach: detach.Options{
			Launch: func(context.Context, detach.Invocation) error { return errors.New("not launched in this test") },
			UnitState: func(context.Context, string) (detach.UnitState, error) {
				return detach.UnitState{}, errors.New("no systemd in this test")
			},
			Signal:    func(context.Context, string, syscall.Signal) error { return nil },
			ListUnits: func(context.Context) ([]string, error) { return nil, nil },
		},
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("daemon run: %v", err)
		}
	})
	select {
	case <-d.Ready():
	case err := <-done:
		t.Fatalf("daemon stopped before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon not ready after 10s")
	}
	return "http://" + listen, token
}

func getJSON(t *testing.T, url, token string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	return b
}

// TC-SM-99 (WI 6.1; FR-13, FR-1, FR-6): nexus command get lists six nexusd
// commands, and the composed daemon's capabilities and OpenAPI document
// list the provider.
func TestTCSM99_NexusCommandGetListsSixNexusd(t *testing.T) {
	// Step 1. `command get` prints JSON lines and takes no -o.
	var stdout, stderr bytes.Buffer
	if code := cli.Run([]string{"command", "get"}, cli.Env{Stdout: &stdout, Stderr: &stderr}); code != 0 {
		t.Fatalf("command get: exit %d: %s", code, stderr.String())
	}
	var verbs []string
	for _, l := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var c struct {
			Noun string   `json:"noun"`
			Verb string   `json:"verb"`
			Ops  []string `json:"ops"`
		}
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		if c.Noun == spec.Name {
			verbs = append(verbs, c.Verb)
		}
		if slices.Contains(c.Ops, spec.Upload.Op().ID) {
			t.Errorf("command %s %s runs the upload op", c.Noun, c.Verb)
		}
	}
	slices.Sort(verbs)
	if want := []string{"get", "reload", "restart", "rollback", "update", "validate-config"}; !slices.Equal(verbs, want) {
		t.Errorf("nexusd commands %v, want %v", verbs, want)
	}

	base, token := composedDaemon(t)

	// Step 2.
	var caps apitypes.Capabilities
	if err := json.Unmarshal(getJSON(t, base+"/v1/capabilities", token), &caps); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(caps.Providers, func(p apitypes.ProviderCapability) bool { return p.Name == spec.Name })
	if i < 0 {
		t.Fatalf("capabilities list no %s: %+v", spec.Name, caps.Providers)
	}
	if got := caps.Providers[i]; got.Status != apitypes.ProviderEnabled {
		t.Errorf("nexusd capability %+v, want enabled", got)
	}

	// Step 3.
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			RequestBody struct {
				Content map[string]struct {
					Schema map[string]any `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
			Responses map[string]struct {
				Description string `json:"description"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(getJSON(t, base+"/v1/openapi.json", token), &doc); err != nil {
		t.Fatal(err)
	}
	var routes []string
	for path, methods := range doc.Paths {
		for method, op := range methods {
			if strings.HasPrefix(op.OperationID, spec.Name+".") {
				routes = append(routes, strings.ToUpper(method)+" "+path)
			}
		}
	}
	slices.Sort(routes)
	want := []string{
		"GET /v1/providers/nexusd/daemons",
		"GET /v1/providers/nexusd/tokens",
		"POST /v1/providers/nexusd/actions/reload",
		"POST /v1/providers/nexusd/actions/restart",
		"POST /v1/providers/nexusd/actions/rollback",
		"POST /v1/providers/nexusd/actions/update",
		"POST /v1/providers/nexusd/actions/validate-config",
		"POST /v1/providers/nexusd/tokens/{name}/add",
		"POST /v1/providers/nexusd/tokens/{name}/remove",
		"POST /v1/providers/nexusd/uploads",
	}
	if !slices.Equal(routes, want) {
		t.Errorf("nexusd routes %v, want %v", routes, want)
	}
	upload := doc.Paths["/v1/providers/nexusd/uploads"]["post"]
	// OpenAPI 3.1 describes a binary body as a string with its
	// contentMediaType, not 3.0's format: binary.
	media, ok := upload.RequestBody.Content["application/octet-stream"]
	if !ok || media.Schema["type"] != "string" || media.Schema["contentMediaType"] != "application/octet-stream" {
		t.Errorf("upload request body %+v, want a binary application/octet-stream", upload.RequestBody)
	}
	var described strings.Builder
	for _, r := range upload.Responses {
		described.WriteString(r.Description + "\n")
	}
	for _, slug := range []problem.Slug{problem.UploadTooLarge, problem.Conflict, problem.StorageLimitReached} {
		if !strings.Contains(described.String(), string(slug)) {
			t.Errorf("the upload's responses do not list %s:\n%s", slug, described.String())
		}
	}
}
