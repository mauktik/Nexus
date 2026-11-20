// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Code review B, CR-B1: svcctl says whether the init system's tool was
// started, because only then can an action have reached the service.

package svcctl_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// A tool that was never started wraps ErrNotStarted; one that started and
// then died, or exited non-zero, or was stopped by its bound, does not.
func TestErrNotStarted(t *testing.T) {
	dir := t.TempDir()
	killed := writeScript(t, dir, "killed.sh", "#!/bin/sh\nkill -9 $$\n")
	plain := filepath.Join(dir, "plain.sh")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		argv       []string
		notStarted bool
	}{
		{"missing program", []string{filepath.Join(dir, "absent")}, true},
		{"relative program", []string{"sh", "-c", "true"}, true},
		{"no argv", nil, true},
		{"not executable", []string{plain}, true},
		{"killed by a signal after it started", []string{killed}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := svcctl.RealCall(t.Context(), 10*time.Second, tc.argv)
			if err == nil {
				t.Fatalf("RealCall = %+v, nil; want an error", res)
			}
			if got := errors.Is(err, svcctl.ErrNotStarted); got != tc.notStarted {
				t.Errorf("errors.Is(%v, ErrNotStarted) = %v, want %v", err, got, tc.notStarted)
			}
		})
	}

	t.Run("deadline", func(t *testing.T) {
		hang := writeScript(t, dir, "hang.sh", "#!/bin/sh\nexec sleep 30\n")
		_, err := svcctl.RealCall(t.Context(), 200*time.Millisecond, []string{hang})
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, svcctl.ErrNotStarted) {
			t.Errorf("err = %v, want a deadline that is not ErrNotStarted", err)
		}
	})

	t.Run("refused arguments", func(t *testing.T) {
		for _, init := range []platform.InitSystem{platform.Systemd, platform.OpenRC} {
			fake := svcctltest.New(t, init, svcctltest.Model{})
			c := newController(t, init, svcctl.Options{Run: fake.Run})
			if _, err := c.Do(t.Context(), "bogus", "nginx"); !errors.Is(err, svcctl.ErrNotStarted) {
				t.Errorf("%s: Do of an invalid action: %v, want ErrNotStarted", init, err)
			}
			if _, err := c.Do(t.Context(), svcctl.Reload, "not a service"); !errors.Is(err, svcctl.ErrNotStarted) {
				t.Errorf("%s: Do of an invalid service: %v, want ErrNotStarted", init, err)
			}
			if n := len(fake.Calls()); n != 0 {
				t.Errorf("%s: the runner was called %d times for refused arguments", init, n)
			}
		}
	})
}
