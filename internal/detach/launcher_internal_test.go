// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package detach

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TC-67: on Linux with a fixture layout the helper reports available only
// when the host was booted with systemd and both tools are executable; any
// gap is unavailable with a reason, never a silent downgrade.
func TestSystemdAvailabilityFixtureLayout(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, mode os.FileMode) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	check := func(goos string, wantOK bool, wantReason string) {
		t.Helper()
		a := systemdAt(goos, root)
		if a.Available != wantOK || (!wantOK && !strings.Contains(a.Reason, wantReason)) {
			t.Fatalf("%s: %+v, want available %v with reason containing %q", goos, a, wantOK, wantReason)
		}
	}
	check("linux", false, "/run/systemd/system")
	if err := os.MkdirAll(filepath.Join(root, "run", "systemd", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	check("linux", false, SystemdRun)
	mk(SystemdRun, 0o755)
	check("linux", false, Systemctl)
	mk(Systemctl, 0o644)
	check("linux", false, Systemctl)
	if err := os.Chmod(filepath.Join(root, Systemctl), 0o755); err != nil {
		t.Fatal(err)
	}
	check("linux", true, "")
	check("darwin", false, "darwin")
}

// Review CR-18: Options.PollInterval is a test seam (the pihole tests
// shorten the watcher's poll). A zero Options, which production passes
// (internal/daemon), gives the PollInterval constant of 500 ms.
func TestLauncherPollIntervalDefaultsToTheConstant(t *testing.T) {
	cfg := Config{OpsDir: t.TempDir()}
	l, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if l.pollEvery != PollInterval || PollInterval != 500*time.Millisecond {
		t.Errorf("poll interval %s (constant %s), want 500ms", l.pollEvery, PollInterval)
	}
	if l, err = New(cfg, Options{PollInterval: -time.Second}); err != nil || l.pollEvery != PollInterval {
		t.Errorf("a negative interval gave %s, %v; want the constant", l.pollEvery, err)
	}
	if l, err = New(cfg, Options{PollInterval: 20 * time.Millisecond}); err != nil || l.pollEvery != 20*time.Millisecond {
		t.Errorf("an explicit interval gave %s, %v", l.pollEvery, err)
	}
}
