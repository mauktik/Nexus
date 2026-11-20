// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Security review SEC-2: the OpenRC pidfile is nginx's, in a directory the
// nginx user owns, so it is read as untrusted: never through a symlink,
// never blocking on a FIFO, only a small regular file.

package svcctl_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// nginx-provider SEC-2 A: the pidfile must be a small regular file that is not a symlink
func TestOpenRCPidfileIsUntrusted(t *testing.T) {
	live := startChild(t)
	status := func(t *testing.T, setup func(runRoot, pf string)) (svcctl.Status, time.Duration) {
		t.Helper()
		runRoot, pf := t.TempDir(), filepath.Join(t.TempDir(), "nginx.pid")
		if err := svcctltest.WriteDaemonRecord(runRoot, "nginx", pf, live); err != nil {
			t.Fatal(err)
		}
		setup(runRoot, pf)
		fake := svcctltest.New(t, platform.OpenRC, svcctltest.Model{State: svcctl.Running})
		c := newController(t, platform.OpenRC, svcctl.Options{Run: fake.Run, RunRoot: runRoot, ProcRoot: procRoot(t, live)})
		start := time.Now()
		st, err := c.Status(t.Context(), "nginx")
		if err != nil {
			t.Fatal(err)
		}
		return st, time.Since(start)
	}

	if st, _ := status(t, func(string, string) {}); st.MainPID != live {
		t.Fatalf("control: a regular pidfile gave MainPID %d, want %d", st.MainPID, live)
	}
	t.Run("symlink", func(t *testing.T) {
		st, _ := status(t, func(_, pf string) {
			real := pf + ".real"
			if err := os.Rename(pf, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, pf); err != nil {
				t.Fatal(err)
			}
		})
		if st.MainPID != 0 {
			t.Errorf("MainPID = %d through a symlinked pidfile, want 0", st.MainPID)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		st, took := status(t, func(_, pf string) {
			if err := os.Remove(pf); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(pf, 0o644); err != nil {
				t.Fatal(err)
			}
		})
		if st.MainPID != 0 || took > 2*time.Second {
			t.Errorf("fifo pidfile: MainPID %d after %v, want 0 at once", st.MainPID, took)
		}
	})
	t.Run("larger than a pid", func(t *testing.T) {
		st, _ := status(t, func(_, pf string) {
			b, err := os.ReadFile(pf)
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, strings.Repeat("x", 1024)...)
			if err := os.WriteFile(pf, b, 0o644); err != nil {
				t.Fatal(err)
			}
		})
		if st.MainPID != 0 {
			t.Errorf("MainPID = %d from a 1 KiB pidfile, want 0: a pidfile is a pid and a newline", st.MainPID)
		}
	})
}
