// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package apt

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

func slugOf(t *testing.T, err error) problem.Slug {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) {
		t.Fatalf("error %v is not a problem", err)
	}
	return p.Slug()
}

// A name apt would read as a regular expression never reaches apt-cache
// policy or show: apt-cache policy 'nxplai.' printed nxplain in the capture
// (testdata/*/fixture-policy-regex.txt), so get resolves names exactly.
func TestGetResolvesNamesExactly(t *testing.T) {
	for _, base := range bases {
		if !strings.HasPrefix(string(capture(t, base, "fixture-policy-regex.txt")), "nxplain:") {
			t.Fatalf("%s: the capture no longer shows apt's regex match; the premise changed", base)
		}
		for _, name := range []string{"nxplai.", "nxpla+", "nxplain.", "nxp"} {
			p, f := newFake(t, base, "fixture")
			_, err := p.get(context.Background(), spec.PackageInput{Name: name})
			if slugOf(t, err) != problem.NotFound {
				t.Errorf("%s get %q: %v, want not-found", base, name, err)
			}
			for _, c := range f.calls {
				if slices.Contains(c, "policy") || slices.Contains(c, "show") {
					t.Errorf("%s get %q ran %q", base, name, c)
				}
			}
		}
		// A foreign architecture apt has no such package for is not found,
		// not a failure of apt-cache show.
		p, _ := newFake(t, base, "fixture")
		if _, err := p.get(context.Background(), spec.PackageInput{Name: "nxplain:i386"}); slugOf(t, err) != problem.NotFound {
			t.Errorf("%s get nxplain:i386: %v, want not-found", base, err)
		}
		for _, name := range []string{"-o", "../x", "nx plain", "nx*", "~i", "?installed", "nxplain=1.0", "nxplain/stable", "A"} {
			p, f := newFake(t, base, "fixture")
			if _, err := p.get(context.Background(), spec.PackageInput{Name: name}); slugOf(t, err) != problem.ValidationFailed {
				t.Errorf("%s get %q: %v, want validation-failed", base, name, err)
			}
			if len(f.calls) != 0 {
				t.Errorf("%s get %q ran %q", base, name, f.calls)
			}
		}
	}
}

func TestGetReportsVersionsAndMetadata(t *testing.T) {
	for _, base := range bases {
		p, f := newFake(t, base, "fixture")
		f.files["dpkg-query nxplain"] = "fixture-dpkg-query-nxplain.txt"
		got, err := p.get(context.Background(), spec.PackageInput{Name: "nxplain"})
		if err != nil {
			t.Fatal(err)
		}
		pr := got.Properties
		if got.ID != "/providers/apt/packages/nxplain" || got.Type != "packages" || pr.Package != "nxplain" ||
			pr.InstalledVersion != "1.0" || pr.CandidateVersion != "2.0" || !pr.Upgradable || pr.Status != "installed" ||
			pr.Architecture != "all" || pr.Section != "misc" || pr.Priority != "optional" ||
			pr.Maintainer != "Nexus fixtures <fixtures@nexus.invalid>" || pr.Summary != "Nexus apt fixture package nxplain" ||
			pr.Description != "A package built by the Nexus T1 harness. It installs one file." {
			t.Errorf("%s get nxplain: %+v", base, got)
		}
		// The native architecture qualifier names the same package.
		p, f = newFake(t, base, "fixture")
		f.files["dpkg-query nxplain"] = "fixture-dpkg-query-nxplain.txt"
		if q, err := p.get(context.Background(), spec.PackageInput{Name: "nxplain:arm64"}); err != nil || q.Name != "nxplain" {
			t.Errorf("%s get nxplain:arm64: %+v %v", base, q, err)
		}
	}
	if !spec.Get.Op().Untrusted {
		t.Error("apt.packages.get is not marked Untrusted")
	}
}

func TestListPagesTheDpkgSet(t *testing.T) {
	for _, base := range bases {
		p, _ := newFake(t, base, "fixture")
		var want []string
		for _, d := range parseDpkgQuery(capture(t, base, "fixture-dpkg-query.txt")) {
			want = append(want, d.Package) // All native or all: no qualifier
		}
		slices.Sort(want)
		var got []string
		cursor := ""
		for range 100 {
			page, err := p.list(context.Background(), spec.ListInput{Limit: 7, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range page.Items {
				got = append(got, it.Name)
				if it.Properties.InstalledVersion == "" || it.Properties.Status != "installed" {
					t.Errorf("%+v", it)
				}
			}
			if cursor = page.NextCursor; cursor == "" {
				break
			}
			if len(page.Items) != 7 {
				t.Fatalf("a non-final page has %d items", len(page.Items))
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: pages concatenate to %d names, want %d", base, len(got), len(want))
		}
		bad := base64.RawURLEncoding.EncodeToString([]byte("../x"))
		if _, err := p.list(context.Background(), spec.ListInput{Cursor: bad}); slugOf(t, err) != problem.ValidationFailed {
			t.Errorf("forged cursor: %v", err)
		}
	}
}

func TestEmptyUpgradableListIsEmptyArray(t *testing.T) {
	p, f := newFake(t, "ubuntu-latest", "fixture")
	f.files["apt-cache policy"] = "fixture-policy-unknown.txt" // Nothing upgradable
	got, err := p.listUpgradable(context.Background(), spec.ListInput{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := got.MarshalJSON()
	if err != nil || string(b) != `{"items":[],"nextCursor":""}` {
		t.Fatalf("empty list: %s %v", b, err)
	}
}

// The upgradable cache: a second read runs no command, and each watched
// file's change makes the next read recompute.
func TestUpgradableCacheInvalidates(t *testing.T) {
	p, f := newFake(t, "ubuntu-latest", "fixture")
	write := func(rel, content string) {
		path := p.hostPath(rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("/var/lib/dpkg/status", "a")
	write("/var/lib/apt/lists/x_Packages", "a")
	write("/etc/apt/sources.list.d/x.list", "a")
	if err := os.MkdirAll(p.hostPath("/etc/apt/preferences.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	read := func() int {
		before := len(f.calls)
		if _, err := p.listUpgradable(context.Background(), spec.ListInput{}); err != nil {
			t.Fatal(err)
		}
		return len(f.calls) - before
	}
	if n := read(); n == 0 {
		t.Fatal("the first read ran no command")
	}
	if n := read(); n != 0 {
		t.Fatalf("the cached read ran %d commands", n)
	}
	changes := map[string]func(){
		"dpkg status":       func() { write("/var/lib/dpkg/status", "bb") },
		"a new index":       func() { write("/var/lib/apt/lists/y_Packages", "a") },
		"a new pin":         func() { write("/etc/apt/preferences.d/pin", "Package: x") },
		"an edited pin":     func() { write("/etc/apt/preferences.d/pin", "Package: xy") },
		"preferences":       func() { write("/etc/apt/preferences", "a") },
		"an edited source":  func() { write("/etc/apt/sources.list.d/x.list", "bb") },
		"sources.list":      func() { write("/etc/apt/sources.list", "a") },
		"a removed pin":     func() { _ = os.Remove(p.hostPath("/etc/apt/preferences.d/pin")) },
		"a replaced status": func() { _ = os.Rename(p.hostPath("/etc/apt/sources.list"), p.hostPath("/var/lib/dpkg/status")) },
	}
	for _, name := range []string{"dpkg status", "a new index", "a new pin", "an edited pin", "preferences",
		"an edited source", "sources.list", "a removed pin", "a replaced status"} {
		time.Sleep(10 * time.Millisecond) // Distinct modification times on coarse clocks
		changes[name]()
		if n := read(); n == 0 {
			t.Errorf("after %s the read used the cache", name)
		}
		if n := read(); n != 0 {
			t.Errorf("after %s the second read ran %d commands", name, n)
		}
	}
}

func TestRebootState(t *testing.T) {
	p := New(Options{})
	p.root = t.TempDir()
	put := func(rel, content string) {
		path := p.hostPath(rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if s, pkgs := p.rebootState(); s != spec.RebootUnknown || len(pkgs) != 0 {
		t.Errorf("no writer: %s %q", s, pkgs)
	}
	for _, w := range RebootMarkerWriters {
		put(w, "")
		if s, _ := p.rebootState(); s != spec.RebootFalse {
			t.Errorf("writer %s present, no marker: %s", w, s)
		}
		if err := os.Remove(p.hostPath(w)); err != nil {
			t.Fatal(err)
		}
	}
	put(RebootMarker, "*** System restart required ***\n")
	put(RebootPackagesFile, "linux-image-generic\nlibc6\nlibc6\n$(id)\n\n")
	if s, pkgs := p.rebootState(); s != spec.RebootTrue || !slices.Equal(pkgs, []string{"linux-image-generic", "libc6"}) {
		t.Errorf("marker: %s %q", s, pkgs)
	}
}

func TestPreflightNamesTheMissingBinary(t *testing.T) {
	p, _ := newFake(t, "ubuntu-latest", "fixture")
	p.systemd = func() detach.Availability { return detach.Availability{Available: true} }
	for _, bin := range Binaries {
		path := p.hostPath(bin)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st := p.Preflight(context.Background())
	if st.State != apitypes.ProviderEnabled || st.Details[spec.DetailArchitecture] != "arm64" ||
		st.Details[spec.DetailRebootRequired] != spec.RebootUnknown {
		t.Fatalf("all present: %+v", st)
	}
	if err := os.Rename(p.hostPath(AptGet), p.hostPath(AptGet)+".off"); err != nil {
		t.Fatal(err)
	}
	if st := p.Preflight(context.Background()); st.State != apitypes.ProviderUnavailable || !strings.Contains(st.Reason, AptGet) {
		t.Fatalf("apt-get missing: %+v", st)
	}
	if err := os.Rename(p.hostPath(AptGet)+".off", p.hostPath(AptGet)); err != nil {
		t.Fatal(err)
	}
	p.systemd = func() detach.Availability { return detach.Availability{Reason: "no systemd here"} }
	if st := p.Preflight(context.Background()); st.State != apitypes.ProviderUnavailable || !strings.Contains(st.Reason, "systemd") {
		t.Fatalf("no systemd: %+v", st)
	}
}

func TestFinalizeRefresh(t *testing.T) {
	ok, err := finalizeRefresh(context.Background(), spec.RefreshInput{}, detach.Exit{ExitCode: 0}, nil)
	if err != nil || !ok.Changed {
		t.Fatalf("exit 0: %+v %v", ok, err)
	}
	for _, x := range []detach.Exit{{ExitCode: 100}, {ExitCode: -1, Signal: "SIGKILL"}, {ExitCode: -1, Error: "exec failed"}} {
		if _, err := finalizeRefresh(context.Background(), spec.RefreshInput{}, x, nil); slugOf(t, err) != problem.AptFailed {
			t.Errorf("%+v: %v", x, err)
		}
	}
	if l, err := aptLane(context.Background(), spec.RefreshInput{}); err != nil || l.Key != LaneKey || l.Policy != "queue" {
		t.Errorf("lane %+v %v", l, err)
	}
}
