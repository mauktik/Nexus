// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Code review B, CR-B2: `systemctl stop` of a failed unit exits 0 and leaves
// it failed (testdata/systemd/stop-failed.txt), so a stop of a failed nginx
// that is not running must end as stopped, not as "failed after stop".

package nginx

import (
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// nginx-provider CR-B2 A: stop of a failed nginx
func TestStopOfFailedNginx(t *testing.T) {
	fakeNginxBinary(t)
	t.Run("systemd: the unit stays failed, which is not running", func(t *testing.T) {
		h := newLC(t, ubuntu, svcctltest.Model{State: svcctl.Failed})
		writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})

		r := h.op(spec.Stop.Op()).succeeded(t)
		if r.Changed || r.State != string(svcctl.Failed) || r.MainPID != nil {
			t.Errorf("result %+v, want unchanged, state failed and no mainPid: nothing was running to stop", r)
		}
		if got, want := h.calls(), []string{"status", "stop", "status"}; !slices.Equal(got, want) {
			t.Errorf("calls %q, want %q", got, want)
		}
	})
	t.Run("openrc: a crashed service ends stopped", func(t *testing.T) {
		h := newLC(t, alpine, svcctltest.Model{State: svcctl.Failed})
		writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})

		r := h.op(spec.Stop.Op()).succeeded(t)
		if !r.Changed || r.State != string(svcctl.Stopped) {
			t.Errorf("result %+v, want changed and stopped: rc-service stop clears the crashed state", r)
		}
	})
}

// nginx-provider CR-B2 B: a stop whose tool exited 0 but that still reads running keeps polling to the bound
func TestStopStillRunningKeepsPolling(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
				h.setPID(700)
				writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
				h.set(func(h *lcHarness) {
					h.afterDo = func(verb string) {
						if verb == "stop" {
							h.f.svc.Set(func(m *svcctltest.Model) { m.State = svcctl.Running })
						}
					}
				})

				o := h.op(spec.Stop.Op())
				d := o.failed(t, problem.OutcomeUnknown)
				if !strings.Contains(d, "did not become stopped") {
					t.Errorf("detail %q, want nginx not stopped within the bound", d)
				}
				if o.elapsed != 30*time.Second {
					t.Errorf("reported after %v, want the state bound, 30s: a stale running read is not final", o.elapsed)
				}
			})
		})
	}
}

// nginx-provider CR-B2 C: a stop whose tool exited non-zero while nginx still runs is a definite failure at once
func TestDefiniteStopFailure(t *testing.T) {
	running := svcctl.Status{Known: true, State: svcctl.Running, MainPID: 7}
	if !definite(running, svcctl.Stopped, svcctl.Result{ExitCode: 1}) {
		t.Error("a running nginx after a stop that exited 1 is not definite")
	}
	if definite(running, svcctl.Stopped, svcctl.Result{ExitCode: 0}) {
		t.Error("a running nginx after a stop that exited 0 is definite: it may be a stale read")
	}
	if !definite(svcctl.Status{Known: true, State: svcctl.Stopped}, svcctl.Running, svcctl.Result{}) {
		t.Error("a stopped nginx after a start is not definite")
	}
}
