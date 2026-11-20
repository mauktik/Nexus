// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package apt

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/provider"
)

// nginx-provider TC-E13 (FR-26): on a host without apt (Alpine), an enabled
// apt section passes validate-config, and the provider is unavailable for
// the missing binary: the binary check comes before the detach check, so
// the reason never mentions detached operations.
func TestEnabledAptWithoutBinariesIsNotConfigError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nexusd.yaml")
	if err := os.WriteFile(cfgPath, []byte("listen: [\"127.0.0.1:9\"]\ntls: {mode: off}\nproviders:\n  apt: {enabled: true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	p := New(Options{Host: "h"})
	p.root = t.TempDir() // No apt-get, no dpkg
	p.systemd = func() detach.Availability { return detach.Availability{Available: true} }

	// Step 1: what validate-config adds to config.Load (daemon.ValidateProviders).
	cfgs := map[string]core.ProviderConfig{"apt": {Enabled: cfg.Providers.Apt.Settings.Enabled, Decode: cfg.Providers.Apt.Decode}}
	if !cfgs["apt"].Enabled {
		t.Fatal("the apt section is not enabled")
	}
	if err := core.ValidateProviders([]provider.Provider{p}, cfgs); err != nil {
		t.Fatalf("validate-config: %v, want valid", err)
	}

	// Step 2.
	st := p.Preflight(context.Background())
	want := "/usr/bin/apt-get is missing or not executable; the apt provider needs apt and dpkg"
	if st.State != apitypes.ProviderUnavailable || st.Reason != want {
		t.Fatalf("Preflight %+v, want unavailable %q", st, want)
	}
	p.systemd = func() detach.Availability {
		return detach.Availability{Reason: "no supported init system (systemd or OpenRC)"}
	}
	if st := p.Preflight(context.Background()); st.Reason != want {
		t.Fatalf("Preflight without apt or detach: %q, want the binary reason %q", st.Reason, want)
	}
}

// nginx-provider TC-27 (apt part, FR-26, FR-18): with its binaries present,
// apt is enabled when detached operations are available on any init system
// and unavailable with the detach reason otherwise; the seam's default is
// the platform-neutral detach.Available.
func TestAptPreflightUsesDetachAvailable(t *testing.T) {
	if got, want := reflect.ValueOf(New(Options{}).systemd).Pointer(), reflect.ValueOf(detach.Available).Pointer(); got != want {
		t.Fatal("the availability seam's default is not detach.Available")
	}
	p, _ := newFake(t, "ubuntu-latest", "fixture")
	for _, bin := range Binaries {
		path := p.hostPath(bin)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// OpenRC's process backend: available, with no systemd anywhere.
	p.systemd = func() detach.Availability { return detach.Availability{Available: true} }
	if st := p.Preflight(context.Background()); st.State != apitypes.ProviderEnabled {
		t.Fatalf("available: %+v, want enabled", st)
	}
	p.systemd = func() detach.Availability {
		return detach.Availability{Reason: "no supported init system (systemd or OpenRC)"}
	}
	st := p.Preflight(context.Background())
	want := "apt operations run only as detached operations: no supported init system (systemd or OpenRC)"
	if st.State != apitypes.ProviderUnavailable || st.Reason != want {
		t.Fatalf("unavailable: %+v, want reason %q", st, want)
	}
}
