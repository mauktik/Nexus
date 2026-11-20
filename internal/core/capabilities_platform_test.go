// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package core_test

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
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
)

// platformCaps is the platform part of the capabilities JSON, decoded from
// the wire so the field names are checked too.
type platformCaps struct {
	OS         map[string]any `json:"os"`
	InitSystem *string        `json:"initSystem"`
}

func decodePlatformCaps(t *testing.T, b []byte) platformCaps {
	t.Helper()
	var pc platformCaps
	if err := json.Unmarshal(b, &pc); err != nil {
		t.Fatalf("decode capabilities: %v: %s", err, b)
	}
	if pc.OS == nil || pc.InitSystem == nil {
		t.Fatalf("capabilities lack os or initSystem: %s", b)
	}
	return pc
}

// serveCapabilities builds a registry on info and returns the raw body of GET
// /v1/capabilities through the real server.
func serveCapabilities(t *testing.T, info platform.Info) []byte {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	reg, err := core.New(nil, core.Options{Host: "testhost", Version: "v0.0.0-test", Logger: log, Platform: info})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	stateDir := t.TempDir()
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "caps"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(reg, server.Options{Logger: log, Auth: auth.New(stateDir, auth.Options{Logger: log})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/v1/capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/capabilities: %d %s", resp.StatusCode, body)
	}
	return body
}

// cliCapabilities starts a real nexusd by daemon.New on info and returns the
// stdout of `nexus capability get --output json` against it.
func cliCapabilities(t *testing.T, info platform.Info) []byte {
	t.Helper()
	dir := t.TempDir()
	stateDir, auditDir, cfgPath := filepath.Join(dir, "state"), filepath.Join(dir, "audit"), filepath.Join(dir, "nexusd.yaml")
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("listen: [%q]\ntls: {mode: off}\n", listen)), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(c, daemon.Options{
		Providers:  []provider.Provider{},
		ConfigPath: cfgPath, StateDir: stateDir, AuditDir: auditDir,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Platform: &info,
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

	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"capability", "get", "--output", "json"}, cli.Env{
		Stdout: &stdout, Stderr: &stderr,
		Getenv: func(k string) string {
			switch k {
			case clientconfig.EnvHost:
				return "http://" + listen
			case clientconfig.EnvToken:
				return tok
			case "HOME":
				return home
			}
			return ""
		},
	})
	if code != 0 {
		t.Fatalf("nexus capability get: exit %d: %s", code, stderr.String())
	}
	return stdout.Bytes()
}

// nginx-provider TC-4: Capabilities report os and initSystem, and follow the
// injected platform. Owner WI 1.2. Requirements: FR-18.
func TestCapabilitiesReportOSAndInitSystem(t *testing.T) {
	a := platform.Info{Init: platform.OpenRC, OS: platform.OS{ID: "alpine", VersionID: "3.21.2", PrettyName: "Alpine Linux v3.21"}}
	b := platform.Info{Init: platform.Systemd, OS: platform.OS{ID: "ubuntu", VersionID: "24.04"}}
	wantA := func(what string, pc platformCaps) {
		t.Helper()
		want := map[string]any{"id": "alpine", "versionId": "3.21.2", "prettyName": "Alpine Linux v3.21"}
		if !reflect.DeepEqual(pc.OS, want) || *pc.InitSystem != "openrc" {
			t.Errorf("%s: os = %v, initSystem = %q; want os %v, initSystem openrc", what, pc.OS, *pc.InitSystem, want)
		}
	}

	// Steps 1 and 2.
	bodyA := serveCapabilities(t, a)
	wantA("registry A", decodePlatformCaps(t, bodyA))

	// Step 3.
	bodyB := serveCapabilities(t, b)
	pcB := decodePlatformCaps(t, bodyB)
	if want := map[string]any{"id": "ubuntu", "versionId": "24.04"}; !reflect.DeepEqual(pcB.OS, want) ||
		*pcB.InitSystem != "systemd" {
		t.Errorf("registry B: os = %v, initSystem = %q; want os %v, initSystem systemd", pcB.OS, *pcB.InitSystem, want)
	}
	for _, v := range []string{"alpine", "openrc", "Alpine Linux"} {
		if strings.Contains(string(bodyB), v) {
			t.Errorf("registry B carries A's %q: %s", v, bodyB)
		}
	}
	for _, v := range []string{"ubuntu", "systemd", "24.04"} {
		if strings.Contains(string(bodyA), v) {
			t.Errorf("registry A carries B's %q: %s", v, bodyA)
		}
	}

	// Step 4. The json format is an array of one object per host.
	out := cliCapabilities(t, a)
	var objs []json.RawMessage
	if err := json.Unmarshal(out, &objs); err != nil || len(objs) != 1 {
		t.Fatalf("CLI output is not one JSON object in an array: %v: %s", err, out)
	}
	wantA("nexus capability get", decodePlatformCaps(t, objs[0]))

	// The OpenAPI schema requires both; docs/api.md currency is make docs-check.
	s, err := openapi.WireSchema(reflect.TypeFor[apitypes.Capabilities]())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"os", "initSystem"} {
		if !slices.Contains(s.Required, f) {
			t.Errorf("Capabilities schema required = %v, lacks %q", s.Required, f)
		}
	}
	if osSchema := s.Properties["os"]; osSchema == nil || !slices.Contains(osSchema.Required, "id") ||
		!slices.Contains(osSchema.Required, "versionId") {
		t.Errorf("Capabilities os schema = %+v, want id and versionId required", osSchema)
	}
}
