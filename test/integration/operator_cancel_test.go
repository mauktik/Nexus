//go:build integration

// T1: the host-side stop docs/security.md gives an operator who must stop
// an operation without the API, run as written on the harness's init
// system (nginx-provider, follow-up to the reboot-race fix).

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
)

// operatorStop is the command docs/security.md gives for operation id with
// signal sig ("INT" or "TERM"), without sudo: the harness runs as root. On
// systemd it is systemctl kill of the unit's main process; on OpenRC, kill
// of the supervisor whose pid supervisor.json records.
func operatorStop(t *testing.T, d *scriptsd, id, sig string) string {
	t.Helper()
	if detectedInit() == platform.Systemd {
		return fmt.Sprintf("systemctl kill --kill-whom=main --signal=SIG%s %s", sig, detach.Unit(id))
	}
	pid := supervisorOf(t, d, id).PID
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !strings.Contains(string(cmdline), "op-supervise") {
		t.Fatalf("pid %d from supervisor.json is not an op-supervise process: %q %v", pid, cmdline, err)
	}
	if sig == "TERM" {
		return "kill " + strconv.Itoa(pid)
	}
	return "kill -" + sig + " " + strconv.Itoa(pid)
}

// The operator's cancel is SIGINT: the script run ends Canceled, and its
// child got SIGTERM and is gone. A SIGTERM sent the same way, with no
// cancel from Nexus, stops the run too but ends it Failed outcome-unknown,
// as an operation running at a shutdown does, never Canceled.
func TestOperatorHostCancel(t *testing.T) {
	for _, c := range []struct {
		sig    string
		status apitypes.OperationStatus
		slug   string
	}{
		{"INT", apitypes.StatusCanceled, ""},
		{"TERM", apitypes.StatusFailed, string(problem.OutcomeUnknown)},
	} {
		t.Run("SIG"+c.sig, func(t *testing.T) {
			name := "opcancel-" + strings.ToLower(c.sig)
			m := markers(t, name, "root")
			script := installScript(t, filepath.Join(testDir(t, name), "trapper.sh"), fmt.Sprintf(
				"#!/bin/sh\ntrap 'echo TERM > %[1]s/child.term; exit 143' TERM\necho $$ > %[1]s/child\nwhile :; do sleep 0.2; done\n", m))
			catalog := catalogYAML(entry{id: "trapper", path: script})
			var d *scriptsd
			if detectedInit() == platform.Systemd {
				d = newScriptsd(t, name, catalog)
			} else {
				d = installedNexusd(t, catalog)
			}
			op := d.invoke("trapper", nil)
			child := waitPIDFile(t, filepath.Join(m, "child"))
			d.waitStatus(op.ID, apitypes.StatusRunning)

			cmd := operatorStop(t, d, op.ID, c.sig)
			if out, err := sh(time.Minute, "sh", "-c", cmd); err != nil {
				t.Fatalf("%s: %v\n%s", cmd, err, out)
			}
			end, took := d.waitTerminal(op.ID, time.Minute)
			t.Logf("%s: %s %s %v in %v", cmd, op.ID, end.Status, end.Error, took)
			if end.Status != c.status || problemSlug(end) != c.slug {
				t.Fatalf("after %s: %s %v, want %s %q", cmd, end.Status, end.Error, c.status, c.slug)
			}
			if b, err := os.ReadFile(filepath.Join(m, "child.term")); err != nil || strings.TrimSpace(string(b)) != "TERM" {
				t.Fatalf("the child recorded no SIGTERM: %q %v", b, err)
			}
			waitUntil(t, 15*time.Second, "the child to be gone", func() (bool, string) {
				return !pidAlive(child), strconv.Itoa(child)
			})
		})
	}
}
