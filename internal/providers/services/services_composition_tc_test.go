// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-117, TC-SV-118,
// TC-SV-119. Owner WI: 5.1. This is an external test: the composed daemon
// imports this package. The injected hooks are read through export_test.go.

package services_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/services"
	"github.com/mauktik/nexus/internal/providers/services/spec"
)

// The production value builtinProviders returns for services
// (services.New) is a provider and takes the three daemon hooks.
var (
	_ provider.Provider       = (*services.Provider)(nil)
	_ provider.OpsDirUser     = (*services.Provider)(nil)
	_ provider.DaemonUser     = (*services.Provider)(nil)
	_ provider.OperationsUser = (*services.Provider)(nil)
)

// composedServices runs daemon.New from a configuration that enables only
// services, with providers (nil: the compiled-in ones), until the test ends.
// Detach is stubbed: nothing is launched. It returns the daemon, its base
// URL and its state directory.
func composedServices(t *testing.T, providers []provider.Provider) (d *daemon.Daemon, base, stateDir string) {
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
		"  services: {enabled: true}\n", listen)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	stateDir = filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	d, err = daemon.New(cfg, daemon.Options{
		Providers:  providers,
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
	return d, "http://" + listen, stateDir
}

// servicesCapabilities reads the capabilities and returns every entry
// named services.
func servicesCapabilities(t *testing.T, base, stateDir string) []apitypes.ProviderCapability {
	t.Helper()
	token, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "composed", Scopes: []string{spec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/capabilities?refresh=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities: %d %s (%v)", resp.StatusCode, b, err)
	}
	var caps apitypes.Capabilities
	if err := json.Unmarshal(b, &caps); err != nil {
		t.Fatal(err)
	}
	var out []apitypes.ProviderCapability
	for _, p := range caps.Providers {
		if p.Name == spec.Name {
			out = append(out, p)
		}
	}
	return out
}

// TC-SV-117: builtinProviders and providerConfigs include services, and the daemon injects its three hooks. Owner WI 5.1. Requirements: FR-1, CON-4.
//
// - Setup: The production `builtinProviders` (exported for tests by the existing pattern), and `startInProcess` with `providers.services.enabled: true`.
// - Steps:
//  1. Find `services` in `builtinProviders`.
//  2. Assert interface conformance.
//  3. After start, read the provider's injected fields through an `export_test.go` accessor.
//
// - Expected:
//   - exactly one `services` provider;
//   - `providerConfigs` has a `"services"` row;
//   - it implements `provider.OpsDirUser`, `provider.DaemonUser` and `provider.OperationsUser`;
//   - all three were injected non-nil, and the `Operations` is the daemon's manager.
func TestTCSV117_BuiltinProvidersAndProviderConfigsIncludeServicesAndThe(t *testing.T) {
	// The compiled-in providers (daemon.Options.Providers nil): exactly one
	// services provider, and its section's enabled reaches it through
	// providerConfigs, so it is not disabled. The daemon package checks
	// builtinProviders and providerConfigs directly
	// (TestTCSV117_BuiltinProvidersHoldOneServicesProvider).
	_, base, stateDir := composedServices(t, nil)
	caps := servicesCapabilities(t, base, stateDir)
	if len(caps) != 1 {
		t.Fatalf("capabilities list %d services providers, want exactly one: %+v", len(caps), caps)
	}
	if caps[0].Status == apitypes.ProviderDisabled {
		t.Errorf("services is %s with providers.services.enabled true: providerConfigs has no services row? %+v",
			caps[0].Status, caps[0])
	}

	// The value builtinProviders constructs, composed by daemon.New: each
	// hook was injected, non-nil, before serving.
	p := services.New(services.Options{Host: "composed", Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	d, _, stateDir := composedServices(t, []provider.Provider{p})
	opsDir, gotDaemon, gotOps := p.Injected()
	if want := filepath.Join(stateDir, "ops"); opsDir != want {
		t.Errorf("UseOpsDir injected %q, want %q", opsDir, want)
	}
	if dd, ok := gotDaemon.(*daemon.Daemon); !ok || dd != d {
		t.Errorf("UseDaemon injected %T %p, want the daemon %p", gotDaemon, gotDaemon, d)
	}
	mgr, ok := gotOps.(*ops.Manager)
	if !ok || mgr == nil {
		t.Fatalf("UseOperations injected %T, want the daemon's *ops.Manager", gotOps)
	}
	if active, err := mgr.Active(); err != nil || len(active) != 0 {
		t.Errorf("the injected manager's Active() = %v, %v; want none", active, err)
	}
}

// TC-SV-118: nexusd routes services-helper to the helper. Owner WI 5.1. Requirements: FR-1, CON-4.
//
// - Setup: `go build -o $TMP/nexusd ./cmd/nexusd` (skipped under `skipUnderMake`'s rule, as other binary tests are).
// - Steps:
//  1. `$TMP/nexusd services-helper --help`.
//  2. `$TMP/nexusd services-helper run --dir /nonexistent`.
//
// - Expected:
//   - (1) the helper's own usage, naming `run --dir`, and not the main usage;
//   - (2) a non-zero exit whose stderr names the directory.
func TestTCSV118_NexusdRoutesServicesHelperToTheHelper(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: builds the nexusd binary")
	}
	bin := filepath.Join(t.TempDir(), "nexusd")
	build := exec.Command("go", "build", "-o", bin, "../../../cmd/nexusd")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/nexusd: %v\n%s", err, out)
	}
	run := func(args ...string) (stdout, stderr string, code int) {
		t.Helper()
		var o, e strings.Builder
		cmd := exec.Command(bin, args...)
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit):
			code = exit.ExitCode()
		default:
			t.Fatalf("nexusd %v: %v", args, err)
		}
		return o.String(), e.String(), code
	}

	// (1) The helper's own usage, not the main usage.
	stdout, stderr, _ := run(services.HelperCommand, "--help")
	usage := stdout + stderr
	if !strings.Contains(usage, "nexusd services-helper run --dir") {
		t.Errorf("services-helper --help printed %q, want the helper usage naming run --dir", usage)
	}
	if strings.Contains(usage, "serve [--config path]") || strings.Contains(usage, "validate-config") {
		t.Errorf("services-helper --help printed the main usage: %q", usage)
	}

	// (2) A directory that does not exist: a non-zero exit naming it.
	_, stderr, code := run(services.HelperCommand, "run", "--dir", "/nonexistent")
	if code == 0 || !strings.Contains(stderr, "/nonexistent") {
		t.Errorf("services-helper run --dir /nonexistent: exit %d, stderr %q; want non-zero naming the directory", code, stderr)
	}
}

// TC-SV-119: The reboot op declares CompletesAcrossReboot, its binding carries AfterReboot, and every op is bound once. Owner WI 5.1. Requirements: FR-11, FR-15, CON-4.
//
// - Setup: `New(Options{…})`.
// - Steps:
//  1. Collect `Bindings()` by op ID.
//
// - Expected:
//   - exactly the eight `servicesspec` op IDs, each once;
//   - `spec.Reboot.CompletesAcrossReboot` is true, and its binding's `DetachedFuncs.AfterReboot` is non-nil;
//   - the six control ops are detached, list and get are sync, and logs is a stream.
func TestTCSV119_TheRebootOpDeclaresCompletesAcrossRebootItsBinding(t *testing.T) {
	if !spec.Reboot.Op().CompletesAcrossReboot {
		t.Error("spec.Reboot does not declare CompletesAcrossReboot")
	}
	bindings := map[string][]provider.Binding{}
	for _, b := range services.New(services.Options{Host: "h1"}).Bindings() {
		bindings[b.Op().ID] = append(bindings[b.Op().ID], b)
	}
	var want []string
	for _, op := range spec.Spec().Ops {
		want = append(want, op.ID)
	}
	got := slices.Sorted(func(yield func(string) bool) {
		for id := range bindings {
			if !yield(id) {
				return
			}
		}
	})
	slices.Sort(want)
	if len(want) != 8 || !slices.Equal(got, want) {
		t.Fatalf("bound ops %v, want the eight spec ops %v", got, want)
	}
	kinds := map[string]provider.BindingKind{
		spec.List.Op().ID: provider.BindSync, spec.Get.Op().ID: provider.BindSync, spec.Logs.Op().ID: provider.BindStream,
	}
	for _, op := range []def.Op{spec.Start.Op(), spec.Stop.Op(), spec.Restart.Op(), spec.Control.Op(), spec.Reboot.Op()} {
		kinds[op.ID] = provider.BindDetached
	}
	for id, bs := range bindings {
		if len(bs) != 1 {
			t.Errorf("%s bound %d times, want once", id, len(bs))
			continue
		}
		b := bs[0]
		if b.Kind() != kinds[id] {
			t.Errorf("%s: binding kind %s, want %s", id, b.Kind(), kinds[id])
		}
		f, ok := b.Detached()
		if ok != (kinds[id] == provider.BindDetached) {
			t.Errorf("%s: Detached() ok %v", id, ok)
			continue
		}
		if ok && (f.AfterReboot != nil) != (id == spec.Reboot.Op().ID) {
			t.Errorf("%s: AfterReboot set %v, want it only on the reboot", id, f.AfterReboot != nil)
		}
	}
}
