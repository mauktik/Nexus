// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host_test

// The composition harness of WI 5.1, modelled on
// internal/providers/nexusd/composition_test.go: the host provider is in
// catalog.All(), so the CLI generates its commands, and in daemon's
// builtinProviders and providerConfigs, so a nexusd built from a config
// enabling it serves the provider and starts its sampler. This is an
// external test: the composed daemon imports this package. The tests are
// TC-HP-108, TC-HP-110, TC-HP-E27 and TC-HP-ERR29
// (hostprov_composition_tc_test.go).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/provider"
	hostprov "github.com/mauktik/nexus/internal/providers/host"
)

// The production value builtinProviders returns for host is a
// *hostprov.Provider (hostprov.New); it is a provider.Background, so the
// daemon runs its sampler. TC-HP-108 also observes the daemon starting it.
var (
	_ provider.Provider   = (*hostprov.Provider)(nil)
	_ provider.Background = (*hostprov.Provider)(nil)
)

// logBuffer is a daemon log safe to read while the daemon writes it.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// composed is a running nexusd built by daemon.New with the compiled-in
// providers.
type composed struct {
	base     string
	stateDir string
	log      *logBuffer
}

// token creates a token holding scopes.
func (c *composed) token(t *testing.T, name string, scopes ...string) string {
	t.Helper()
	tok, _, err := auth.Create(c.stateDir, auth.CreateRequest{Name: name, Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// freeListen is a loopback address nothing listens on.
func freeListen(t *testing.T) string {
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

// hostOnlyConfig is a configuration that enables only host, on listen.
func hostOnlyConfig(listen string) string {
	return fmt.Sprintf("host: {name: composed}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n"+
		"  docker: {enabled: false}\n  apt: {enabled: false}\n  scripts: {enabled: false}\n"+
		"  host: {enabled: true}\n", listen)
}

// defaultConfig is deploy/nexusd.default.yaml with its listen address
// rewritten to listen and tls.mode to off: the shipped defaults otherwise,
// host disabled among them. (TLS auto needs a certificate the test would
// have to load; it changes nothing the provider sees.)
func defaultConfig(t *testing.T, listen string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nexusd.default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	listenRE := regexp.MustCompile(`(?m)^listen: \[.*\]`)
	modeRE := regexp.MustCompile(`(?m)^  mode: auto\b`)
	if !listenRE.Match(b) || !modeRE.Match(b) {
		t.Fatal("deploy/nexusd.default.yaml has no listen line or tls mode auto to rewrite")
	}
	b = listenRE.ReplaceAll(b, []byte(fmt.Sprintf("listen: [%q]", listen)))
	b = modeRE.ReplaceAll(b, []byte("  mode: off"))
	return string(b)
}

// startComposed runs daemon.New with the compiled-in providers from the
// configuration body (whose listen address is listen), until the test
// ends. Detach is stubbed: nothing is launched.
func startComposed(t *testing.T, listen, body string) *composed {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nexusd.yaml")
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
	logs := &logBuffer{}
	d, err := daemon.New(cfg, daemon.Options{
		ConfigPath: cfgPath, StateDir: stateDir, AuditDir: filepath.Join(dir, "audit"),
		Logger:  slog.New(slog.NewJSONHandler(logs, nil)),
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
	return &composed{base: "http://" + listen, stateDir: stateDir, log: logs}
}

// client closes each connection after its request, so no idle connection
// holds a server goroutine between the counts of a goroutine check.
var client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

// get sends GET url with token and returns the status and body.
func get(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
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

// getOK is get that requires 200.
func getOK(t *testing.T, url, token string) []byte {
	t.Helper()
	status, b := get(t, url, token)
	if status != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, status, b)
	}
	return b
}
