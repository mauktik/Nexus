// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

// Code review CR-10: the helper retries a reload only for the not-active
// refusal, which it recognizes by systemctl's text. These tests feed the
// classification the text a real systemd printed (the golden captured by the
// T1 run, which fails if a systemd says anything else), never a string this
// package composes.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const systemctlGolden = "../../../test/integration/testdata/systemctl-reload-inactive.txt"

// realNotActiveText is systemctl's stderr for `reload` of a stopped unit,
// as captured on a real systemd.
func realNotActiveText(t testing.TB) string {
	t.Helper()
	b, err := os.ReadFile(systemctlGolden)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "nexusd.service is not active") {
		t.Fatalf("%s holds %q, not a systemctl capture for nexusd.service", systemctlGolden, s)
	}
	return s
}

// stubSystemctl is a realSystem whose systemctl prints stderr and exits code.
func stubSystemctl(t *testing.T, stderr string, code int) *realSystem {
	t.Helper()
	dir := t.TempDir()
	txt := filepath.Join(dir, "stderr.txt")
	if err := os.WriteFile(txt, []byte(stderr), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "systemctl")
	writeScript(t, bin, "cat '"+txt+"' >&2\nexit "+string(rune('0'+code)))
	return &realSystem{systemctl: bin, unit: "nexusd.service", run: execRunner{}}
}

func TestRealSystemctlNotActiveTextIsClassifiedAsNotActive(t *testing.T) {
	sys := stubSystemctl(t, realNotActiveText(t), 1)
	err := sys.Reload(context.Background())
	if !errors.Is(err, errUnitNotActive) {
		t.Fatalf("Reload of an inactive unit: %v, want errUnitNotActive", err)
	}
	if !strings.Contains(err.Error(), "is not active, cannot reload") {
		t.Errorf("the classified error lost systemctl's text: %v", err)
	}
	// Anything else systemctl says is not retried.
	for _, other := range []string{
		"Unit nexusd.service not found.\n",
		"Failed to reload nexusd.service: Job type reload is not applicable for unit nexusd.service.\n",
		"Job for nexusd.service canceled.\n",
	} {
		if err := stubSystemctl(t, other, 1).Reload(context.Background()); err == nil || errors.Is(err, errUnitNotActive) {
			t.Errorf("systemctl saying %q: %v, want a plain error, not errUnitNotActive", other, err)
		}
	}
	if err := stubSystemctl(t, "", 0).Reload(context.Background()); err != nil {
		t.Errorf("a successful reload: %v", err)
	}
}
