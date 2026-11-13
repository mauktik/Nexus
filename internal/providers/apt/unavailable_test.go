package apt_test

import (
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
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
	"github.com/mauktik/nexus/internal/providers/scripts"
	scriptspec "github.com/mauktik/nexus/internal/providers/scripts/spec"
)

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

// TC-ERR46: on a host without apt (this Mac), a real nexusd with apt
// enabled starts, reports apt unavailable with a reason, and serves the
// other providers.
func TestAptUnavailableOnNonDebianHost(t *testing.T) {
	present := 0
	for _, bin := range apt.Binaries {
		if _, err := os.Stat(bin); err == nil {
			present++
		}
	}
	if present == len(apt.Binaries) && detach.Available().Available {
		t.Skip("this host has apt and detached operations; the case needs a host without them")
	}
	dir := t.TempDir()
	stateDir, auditDir, cfgPath := filepath.Join(dir, "state"), filepath.Join(dir, "audit"), filepath.Join(dir, "nexusd.yaml")
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "t", Scopes: []string{spec.ReadScope, scriptspec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	listen := freePort(t)
	cfg := fmt.Sprintf("listen: [%q]\ntls: {mode: off}\nproviders:\n  apt: {enabled: true}\n  scripts: {enabled: true, catalog: []}\n", listen)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	enabled := func(context.Context) provider.Status { return provider.Status{State: apitypes.ProviderEnabled} }
	d, err := daemon.New(c, daemon.Options{
		Providers:  []provider.Provider{apt.New(apt.Options{Host: "h"}), scripts.New(scripts.Options{Host: "h", Preflight: enabled})},
		ConfigPath: cfgPath, StateDir: stateDir, AuditDir: auditDir,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("startup failed: %v", err)
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

	get := func(path string) (int, []byte) {
		req, err := http.NewRequest(http.MethodGet, "http://"+listen+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}
	for _, refresh := range []string{"", "?refresh=true"} {
		code, b := get("/v1/capabilities" + refresh)
		var caps apitypes.Capabilities
		if err := json.Unmarshal(b, &caps); err != nil || code != http.StatusOK {
			t.Fatalf("capabilities: %d %s", code, b)
		}
		i := slices.IndexFunc(caps.Providers, func(p apitypes.ProviderCapability) bool { return p.Name == spec.Name })
		if i < 0 {
			t.Fatalf("apt is not compiled in: %s", b)
		}
		if p := caps.Providers[i]; p.Status != apitypes.ProviderUnavailable || p.Reason == "" {
			t.Errorf("apt %+v, want unavailable with a reason", p)
		}
		j := slices.IndexFunc(caps.Providers, func(p apitypes.ProviderCapability) bool { return p.Name == scriptspec.Name })
		if j < 0 || caps.Providers[j].Status != apitypes.ProviderEnabled {
			t.Errorf("scripts affected: %s", b)
		}
	}
	if code, b := get("/v1/providers/scripts/scripts"); code != http.StatusOK {
		t.Errorf("scripts list: %d %s", code, b)
	}
	if code, b := get("/v1/providers/apt/packages"); code != http.StatusServiceUnavailable {
		t.Errorf("apt list on a host without apt: %d %s, want 503", code, b)
	}
}
