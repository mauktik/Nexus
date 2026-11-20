// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// External tests of the provider's exported surface. This file is also
// where the internal tests get the real daemon.CheckConfig and
// daemon.CheckConfigData: it is the only test file here that may import
// internal/daemon, because daemon will import this package once nexusd is a
// built-in provider (WI 6.1).
package nexusd_test

import (
	"crypto/ed25519"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/release"
)

func init() {
	nexusd.CheckConfigForTest = daemon.CheckConfig
	nexusd.CheckConfigDataForTest = daemon.CheckConfigData
}

var (
	_ provider.Provider   = (*nexusd.Provider)(nil)
	_ provider.DaemonUser = (*nexusd.Provider)(nil)
)

// Preflight is enabled with and without systemd, and its details say
// whether self-management is available and why not (A-16).
func TestPreflightDetails(t *testing.T) {
	const noSystemd = "the host was not booted with systemd (/run/systemd/system is missing)"
	tests := []struct {
		name      string
		systemd   detach.Availability
		inService error
		want      map[string]any
	}{
		{"available", detach.Availability{Available: true}, nil,
			map[string]any{nexusd.DetailSelfManagement: "available"}},
		{"no systemd", detach.Availability{Reason: noSystemd}, nil,
			map[string]any{nexusd.DetailSelfManagement: "unavailable", nexusd.DetailReason: noSystemd}},
		{"not the unit", detach.Availability{Available: true}, errors.New("not running in nexusd.service (cgroup /user.slice/x.scope)"),
			map[string]any{nexusd.DetailSelfManagement: "unavailable",
				nexusd.DetailReason: "not running in nexusd.service (cgroup /user.slice/x.scope)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := nexusd.New(nexusd.Options{
				Systemd:   func() detach.Availability { return tc.systemd },
				InService: func() error { return tc.inService },
			})
			st := p.Preflight(t.Context())
			if st.State != apitypes.ProviderEnabled || st.Reason != "" {
				t.Errorf("state %q reason %q, want enabled with no reason", st.State, st.Reason)
			}
			if !reflect.DeepEqual(st.Details, tc.want) {
				t.Errorf("details %v, want %v", st.Details, tc.want)
			}
		})
	}
}

func TestSpecIsTheSpecPackage(t *testing.T) {
	if got, want := nexusd.New(nexusd.Options{}).Spec(), spec.Spec(); !reflect.DeepEqual(got.Name, want.Name) ||
		len(got.Ops) != len(want.Ops) {
		t.Errorf("Spec %s with %d ops, want %s with %d", got.Name, len(got.Ops), want.Name, len(want.Ops))
	}
}

// Configure refuses a malformed trusted key without quoting it, and keeps
// the settings in force.
func TestConfigureRefusesAMalformedKeyWithoutEchoingIt(t *testing.T) {
	good := release.EncodePublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey))
	const bad = "SECRET-LOOKING-NOT-A-KEY"
	p := nexusd.New(nexusd.Options{})
	err := p.Configure(func(into any) error {
		into.(*config.NexusdConfig).Update.TrustedKeys = []string{good, bad}
		return nil
	})
	if err == nil {
		t.Fatal("Configure accepted a malformed key")
	}
	if strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), good) {
		t.Errorf("the error quotes a key: %v", err)
	}
	if !strings.Contains(err.Error(), "trustedKeys[1]") {
		t.Errorf("the error does not name the key's index: %v", err)
	}

	decodeErr := errors.New("providers.nexusd.bogus: unknown key")
	if err := p.Configure(func(any) error { return decodeErr }); !errors.Is(err, decodeErr) {
		t.Errorf("Configure = %v, want the decode error", err)
	}
}
