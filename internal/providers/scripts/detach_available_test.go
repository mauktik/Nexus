// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package scripts

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
)

// nginx-provider TC-27 (FR-26, FR-18): scripts are enabled when detached
// operations are available on any init system (the OpenRC process backend
// included) and unavailable with the detach reason otherwise. The apt and
// docker halves are in their packages. No Go file under internal/providers/
// gates on the systemd-only check any more.
func TestScriptsPreflightUsesDetachAvailable(t *testing.T) {
	if got, want := reflect.ValueOf(New(Options{}).available).Pointer(), reflect.ValueOf(detach.Available).Pointer(); got != want {
		t.Fatal("the availability seam's default is not detach.Available")
	}
	p := New(Options{Host: "h"})
	p.available = func() detach.Availability { return detach.Availability{Available: true} }
	if st := p.Preflight(context.Background()); st.State != apitypes.ProviderEnabled {
		t.Fatalf("available: %+v, want enabled", st)
	}
	p.available = func() detach.Availability {
		return detach.Availability{Reason: "no supported init system (systemd or OpenRC)"}
	}
	st := p.Preflight(context.Background())
	want := "scripts run only as detached operations: no supported init system (systemd or OpenRC)"
	if st.State != apitypes.ProviderUnavailable || st.Reason != want {
		t.Fatalf("unavailable: %+v, want reason %q", st, want)
	}

	needle := "detach." + "SystemdAvailable" // Split so this file does not match itself
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		// Pi-hole and nexusd's self-management stay systemd-only on purpose:
		// a timed stop-blocking creates a transient systemd timer, and
		// nexusd's restart, update and rollback swap and restart
		// nexusd.service. Their descriptions and Preflight reasons say so.
		if d := filepath.Base(filepath.Dir(path)); d == "pihole" || d == "nexusd" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), needle) {
			t.Errorf("%s gates on systemd alone", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
