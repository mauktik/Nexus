// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package detach

import (
	"os/exec"
	"syscall"
	"testing"
)

// CR-A4: killedBySIGKILL runs on nexusd's launch path. A Wait that fails
// other than by an ExitError leaves ProcessState nil, and must be answered
// with false, not a panic.
func TestKilledBySIGKILLWithoutProcessState(t *testing.T) {
	cmd := exec.Command("/bin/true") // Never started: Wait fails with ProcessState nil
	if killedBySIGKILL(cmd) {
		t.Fatal("killedBySIGKILL is true for a command that never ran")
	}
}

func TestKilledBySIGKILLReportsHowTheProcessEnded(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*exec.Cmd)
		want bool
	}{
		{"SIGKILL", func(c *exec.Cmd) { _ = c.Process.Kill() }, true},
		{"SIGTERM", func(c *exec.Cmd) { _ = c.Process.Signal(syscall.SIGTERM) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			tc.end(cmd)
			// Wait inside killedBySIGKILL reaps it, so the exit is awaited there.
			if got := killedBySIGKILL(cmd); got != tc.want {
				t.Fatalf("killedBySIGKILL = %v, want %v", got, tc.want)
			}
		})
	}
}
