// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Code review B, CR-B1: an init tool that died after it started may have
// delivered the reload, so it is a reload that may have been sent: its
// effect is verified in /proc and a failed toggle or write is rolled back
// with a second reload. Only a tool that never started sent nothing. The
// tool's death is injected after the svcctl fake applied the reload
// (lcHarness.reloadFault), as a signal or a wait error would come after
// the HUP.

package nginx

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// toolDied is what svcctl returns for a reload tool that started and then
// ended without an exit status (a signal, or a wait or I/O error).
var toolDied = errors.New("svcctl: /bin/systemctl reload nginx: signal: killed")

func (h *lcHarness) toolDiesOnReload(n int) {
	h.set(func(h *lcHarness) { h.reloadFault = reloadFault{n: n, err: toolDied} })
}

// nginx-provider CR-B1 A: a reload op whose tool died after the reload was applied
func TestReloadToolDiedAfterAppliedReload(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
				writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
				p := h.useCapture(m.capture+"-valid.jsonl", true, h.f.nginx)
				h.toolDiesOnReload(1)

				r := h.op(spec.Reload.Op()).succeeded(t)
				if r.Reload == nil || r.Reload.Verdict != spec.VerdictConfirmed || r.Reload.MasterPID != p.master ||
					r.Reload.InitExitCode != -1 {
					t.Errorf("reload %+v, want confirmed under master %d with init exit -1 (no exit status)",
						r.Reload, p.master)
				}
				if got, want := h.calls(), []string{"status", "nginx -t", "reload"}; !slices.Equal(got, want) {
					t.Errorf("calls %q, want %q", got, want)
				}
			})
		})
	}
}

// nginx-provider CR-B1 B: a reload op whose tool died and whose reload nginx rejected
func TestReloadToolDiedAfterRejectedReload(t *testing.T) {
	fakeNginxBinary(t)
	synctest.Test(t, func(t *testing.T) {
		h := newLC(t, ubuntu, svcctltest.Model{State: svcctl.Running, Config: svcctltest.BindConflict})
		writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
		h.useCapture("ubuntu-bind-rejected.jsonl", false, h.f.nginx)
		h.toolDiesOnReload(1)

		o := h.op(spec.Reload.Op())
		d := o.failed(t, problem.NginxReloadFailed)
		obs := ext[spec.ReloadObservation](t, o.err, "observed")
		if obs.Verdict != spec.VerdictRejected || obs.InitExitCode != -1 {
			t.Errorf("observed %+v, want verdict rejected and init exit -1", obs)
		}
		if strings.Contains(d, "nothing was reloaded") {
			t.Errorf("detail %q says nothing was reloaded, but the reload may have been sent", d)
		}
		if o.elapsed != testReloadTimeout {
			t.Errorf("reported after %v, want the full %v: a tool with no exit status does not end the wait early",
				o.elapsed, testReloadTimeout)
		}
	})
}

// nginx-provider CR-B1 C: an update whose reload tool died after nginx applied the new content
func TestWriteReloadToolDiedAfterAppliedReload(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWriteLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
				writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
				w.useCapture(m.capture+"-valid.jsonl", true, w.f.nginx)
				w.toolDiesOnReload(1)
				const nc = "server {\n    listen 8231;\n}\n"

				r := w.write(spec.Update.Op(), spec.WriteInput{Name: "a", Content: nc}).succeeded(t)
				if !r.Reloaded || r.Reload == nil || r.Reload.Verdict != spec.VerdictConfirmed {
					t.Errorf("result %+v, want reloaded and confirmed", r)
				}
				checkWritten(t, filepath.Join(w.f.dir, "a.conf"), nc)
				if got, want := w.calls(), []string{"status", "nginx -t", "reload"}; !slices.Equal(got, want) {
					t.Errorf("calls %q, want %q: nginx serves the new content, so it is not rolled back", got, want)
				}
			})
		})
	}
}

// nginx-provider CR-B1 D: a write whose reload tool died and whose reload nginx rejected is reloaded again after the restore
func TestWriteReloadToolDiedRejectedRollsBackWithReload(t *testing.T) {
	fakeNginxBinary(t)
	synctest.Test(t, func(t *testing.T) {
		w := writeReloadRejected(t, true, nil)
		w.toolDiesOnReload(1)
		before := contentSnapshot(t, w.f.dir)

		o := w.write(spec.Update.Op(), spec.WriteInput{Name: "a", Content: "server {\n    listen 8232;\n}\n"})
		o.failed(t, problem.NginxReloadFailed)
		if !ext[bool](t, o.err, "rolledBack") {
			t.Errorf("rolledBack false, want true")
		}
		if back := ext[spec.ReloadObservation](t, o.err, "rollbackReload"); back.Verdict != spec.VerdictConfirmed {
			t.Errorf("rollbackReload %+v, want confirmed", back)
		}
		if got, want := withoutStatus(w.calls()), []string{"nginx -t", "reload", "nginx -t", "reload"}; !slices.Equal(got, want) {
			t.Errorf("calls %q, want %q: the reload may have been sent, so the restore is reloaded", got, want)
		}
		if after := contentSnapshot(t, w.f.dir); !maps.Equal(before, after) {
			t.Errorf("includeDir %v, was %v", after, before)
		}
	})
}

// nginx-provider CR-B1 E: an enable whose reload tool died and whose reload nginx rejected is reloaded again after the rename back
func TestToggleReloadToolDiedRejectedRollsBackWithReload(t *testing.T) {
	fakeNginxBinary(t)
	synctest.Test(t, func(t *testing.T) {
		c := reloadRejected(true)
		h := c.harness(t, nil)
		h.toolDiesOnReload(1)

		o := h.toggle(c.op, c.site)
		o.failed(t, problem.NginxReloadFailed)
		if !ext[bool](t, o.err, "rolledBack") {
			t.Errorf("rolledBack false, want true")
		}
		if back := ext[spec.ReloadObservation](t, o.err, "rollbackReload"); back.Verdict != spec.VerdictConfirmed {
			t.Errorf("rollbackReload %+v, want confirmed", back)
		}
		if got, want := withoutStatus(h.calls()), []string{"nginx -t", "reload", "nginx -t", "reload"}; !slices.Equal(got, want) {
			t.Errorf("calls %q, want %q", got, want)
		}
	})
}

// nginx-provider CR-B1 F: an enable whose reload tool died after nginx applied it is a success
func TestToggleReloadToolDiedAfterAppliedReload(t *testing.T) {
	fakeNginxBinary(t)
	synctest.Test(t, func(t *testing.T) {
		h := newToggleLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
		writeTree(t, h.f.dir, map[string]string{"a.conf.disabled": validSite})
		h.useCapture("ubuntu-valid.jsonl", true, h.f.nginx)
		h.toolDiesOnReload(1)

		r := h.toggle(spec.Enable.Op(), "a").succeeded(t)
		if !r.Reloaded || r.Reload == nil || r.Reload.Verdict != spec.VerdictConfirmed {
			t.Errorf("result %+v, want reloaded and confirmed", r)
		}
		absent(t, filepath.Join(h.f.dir, "a.conf.disabled"))
		if _, err := os.Stat(filepath.Join(h.f.dir, "a.conf")); err != nil {
			t.Errorf("a.conf: %v, want it in place: nginx serves it", err)
		}
	})
}

// nginx-provider CR-B1 G: a reload tool that never started sent nothing: no verification, no second reload
func TestReloadToolNeverStarted(t *testing.T) {
	fakeNginxBinary(t)
	notStarted := errors.Join(svcctl.ErrNotStarted, errors.New("fork/exec /bin/systemctl: no such file or directory"))
	t.Run("reload", func(t *testing.T) {
		h := newLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
		writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
		h.useCapture("ubuntu-valid.jsonl", true, h.f.nginx)
		h.set(func(h *lcHarness) { h.reloadFault = reloadFault{n: 1, err: notStarted, before: true} })

		d := h.op(spec.Reload.Op()).failed(t, problem.DependencyUnavailable)
		if !strings.Contains(d, "nothing was reloaded") {
			t.Errorf("detail %q, want it to say nothing was reloaded", d)
		}
	})
	t.Run("update", func(t *testing.T) {
		w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
		writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
		w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
		w.set(func(h *lcHarness) { h.reloadFault = reloadFault{n: 1, err: notStarted, before: true} })
		before := contentSnapshot(t, w.f.dir)

		o := w.write(spec.Update.Op(), spec.WriteInput{Name: "a", Content: "server {\n    listen 8233;\n}\n"})
		o.failed(t, problem.DependencyUnavailable)
		if !ext[bool](t, o.err, "rolledBack") {
			t.Errorf("rolledBack false, want true")
		}
		if got, want := withoutStatus(w.calls()), []string{"nginx -t", "reload"}; !slices.Equal(got, want) {
			t.Errorf("calls %q, want %q: nothing was sent, so no second reload", got, want)
		}
		if after := contentSnapshot(t, w.f.dir); !maps.Equal(before, after) {
			t.Errorf("includeDir %v, was %v", after, before)
		}
	})
}

// nginx-provider CR-B1 H (the sweep): a start, stop or restart tool that
// died after it started may have acted too, so the state it left decides,
// as for a reload.
func TestStateChangeToolDiedAfterActing(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		for _, tc := range []struct {
			verb  string
			op    def.Op
			from  svcctl.State
			state svcctl.State
		}{
			{"start", spec.Start.Op(), svcctl.Stopped, svcctl.Running},
			{"stop", spec.Stop.Op(), svcctl.Running, svcctl.Stopped},
			{"restart", spec.Restart.Op(), svcctl.Running, svcctl.Running},
		} {
			t.Run(m.name+"/"+tc.verb, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					h := newLC(t, m.plat, svcctltest.Model{State: tc.from})
					writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
					h.set(func(h *lcHarness) {
						h.reloadFault = reloadFault{verb: tc.verb, n: 1, err: toolDied}
						h.nextPIDs = []int{4343}
					})

					r := h.op(tc.op).succeeded(t)
					if !r.Changed || r.State != string(tc.state) {
						t.Errorf("result %+v, want changed and %s: the tool acted before it died", r, tc.state)
					}
				})
			})
		}
	}
}

// nginx-provider CR-B1 I: a start tool that never started changed nothing and is a failure at once
func TestStateChangeToolNeverStarted(t *testing.T) {
	fakeNginxBinary(t)
	h := newLC(t, ubuntu, svcctltest.Model{State: svcctl.Stopped})
	writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
	h.set(func(h *lcHarness) {
		h.reloadFault = reloadFault{verb: "start", n: 1, before: true,
			err: errors.Join(svcctl.ErrNotStarted, errors.New("fork/exec /usr/bin/systemctl: no such file or directory"))}
	})

	d := h.op(spec.Start.Op()).failed(t, problem.DependencyUnavailable)
	if !strings.Contains(d, "did not complete") {
		t.Errorf("detail %q, want it to say the start did not complete", d)
	}
	if got, want := h.calls(), []string{"status", "nginx -t", "start"}; !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q: no state polling after a tool that never started", got, want)
	}
}
