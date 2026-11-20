// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Code review B, CR-B24 and security review SEC-8: includeDir's owner and
// mode are checked on the directory every read and operation opens, not only
// at preflight. A directory that was made group-writable or given to another
// user after the provider became available is refused, with nothing changed.

package nginx

import (
	"context"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// nginx-provider CR-B24 A: openIncludeDir refuses a directory that is group- or other-writable, or not root's
func TestOpenIncludeDirChecksOwnerAndMode(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, fixtureOptions{model: svcctltest.Model{State: svcctl.Running}})
	s := f.p.Settings()

	d, err := openIncludeDir(s)
	if err != nil {
		t.Fatalf("openIncludeDir of a directory the managed owner owns, mode 0755: %v", err)
	}
	d.Close()

	for _, mode := range []os.FileMode{0o775, 0o757, 0o777} {
		if err := os.Chmod(f.dir, mode); err != nil {
			t.Fatal(err)
		}
		d, err := openIncludeDir(s)
		if err == nil {
			d.Close()
			t.Errorf("openIncludeDir of a directory with mode %#o succeeded", mode)
			continue
		}
		if !strings.Contains(err.Error(), "writable by its group or by others") {
			t.Errorf("mode %#o: %v, want the reason Preflight gives", mode, err)
		}
	}
	if err := os.Chmod(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}

	setManagedUID(t, uint32(os.Getuid())+1) // the directory is now someone else's
	if d, err := openIncludeDir(s); err == nil {
		d.Close()
		t.Error("openIncludeDir of a directory another user owns succeeded")
	} else if !strings.Contains(err.Error(), "not by root") {
		t.Errorf("another owner: %v, want the reason Preflight gives", err)
	}
}

// nginx-provider CR-B24 B: reads refuse an includeDir that became group-writable after preflight
func TestReadsRefuseAnIncludeDirMadeWritable(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, fixtureOptions{model: svcctltest.Model{State: svcctl.Running}})
	writeTree(t, f.dir, map[string]string{"a.conf": validSite})
	if st := f.p.Preflight(context.Background()); st.State != "enabled" {
		t.Fatalf("Preflight = %+v, want enabled", st)
	}
	if err := os.Chmod(f.dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.list(context.Background(), spec.ListInput{}); err == nil || !strings.Contains(err.Error(), "writable by its group") {
		t.Errorf("list of a group-writable includeDir: %v, want dependency-unavailable naming the mode", err)
	}
}

// nginx-provider CR-B24 C: a write or a toggle refuses an includeDir made writable after preflight, and changes nothing
func TestOperationsRefuseAnIncludeDirMadeWritable(t *testing.T) {
	fakeNginxBinary(t)
	cases := []struct {
		name string
		run  func(w *writeRig) writeOutcome
	}{
		{"add", func(w *writeRig) writeOutcome {
			return w.write(spec.Add.Op(), spec.AddInput{Name: "n", Content: validSite})
		}},
		{"update", func(w *writeRig) writeOutcome {
			return w.write(spec.Update.Op(), spec.WriteInput{Name: "a", Content: validSite + "# x\n"})
		}},
	}
	for _, when := range []string{"before Admit", "after Admit"} {
		for _, c := range cases {
			t.Run(when+" "+c.name, func(t *testing.T) {
				w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
				writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
				w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
				before := contentSnapshot(t, w.f.dir)
				widen := func() {
					if err := os.Chmod(w.f.dir, 0o777); err != nil {
						t.Error(err)
					}
				}
				if when == "before Admit" {
					widen()
				} else {
					w.set(func(h *lcHarness) { h.afterAdmit = widen })
				}

				o := c.run(w)
				var detail string
				if when == "before Admit" {
					if !o.refused || o.err == nil || o.err.Slug() != problem.DependencyUnavailable {
						t.Fatalf("Admit: %v refused=%v, want dependency-unavailable", o.err, o.refused)
					}
					detail = o.err.Detail
				} else {
					detail = o.failed(t, problem.DependencyUnavailable)
				}
				if !strings.Contains(detail, "writable by its group or by others") {
					t.Errorf("detail %q does not give the reason", detail)
				}
				if err := os.Chmod(w.f.dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if after := contentSnapshot(t, w.f.dir); !maps.Equal(before, after) {
					t.Errorf("includeDir %v, was %v: nothing may change", after, before)
				}
				if got := withoutStatus(w.calls()); len(got) != 0 {
					t.Errorf("calls %q, want none", got)
				}
			})
		}
	}
	t.Run("toggle after Admit", func(t *testing.T) {
		w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
		writeTree(t, w.f.dir, map[string]string{"a.conf.disabled": validSite})
		w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
		before := contentSnapshot(t, w.f.dir)
		w.set(func(h *lcHarness) {
			h.afterAdmit = func() {
				if err := os.Chmod(w.f.dir, 0o777); err != nil {
					t.Error(err)
				}
			}
		})
		o := w.toggle(spec.Enable.Op(), "a")
		detail := o.failed(t, problem.DependencyUnavailable)
		if !strings.Contains(detail, "writable by its group or by others") {
			t.Errorf("detail %q does not give the reason", detail)
		}
		if err := os.Chmod(w.f.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if after := contentSnapshot(t, w.f.dir); !maps.Equal(before, after) {
			t.Errorf("includeDir %v, was %v: nothing may change", after, before)
		}
	})
}
