// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
	"github.com/mauktik/nexus/test/harness/fixtures/aptrepo"
)

// Real apt and dpkg in the harness (WI 7.1), against a fixture repository
// built with dpkg-deb and dpkg-scanpackages and nothing else as a source,
// so the tests need no network. Every expected value comes from apt-cache,
// dpkg-query or dpkg run here, never from Nexus.

const (
	aptRepo        = "/srv/nexus-aptrepo"
	aptFixtureList = "/etc/apt/sources.list.d/nexus-fixture.list"
	aptPlain       = "nxplain"
	packagesAt     = "/v1/providers/apt/packages"
	upgradableAt   = "/v1/providers/apt/upgradable"
	refreshAt      = "/v1/providers/apt/actions/refresh"
)

// aptBuild runs the fixture build script with args.
func aptBuild(t *testing.T, args ...string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "build.sh")
	if err := os.WriteFile(script, aptrepo.BuildScript, 0o700); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(mustSh(t, "bash", append([]string{script}, args...)...))
}

// aptFixture leaves the fixture repository as the only apt source, with
// nxplain 1.0 installed and, when withV2, 2.0 in the repository, and the
// indexes refreshed. Cleanup purges nxplain and restores the sources.
func aptFixture(t *testing.T, withV2 bool) {
	t.Helper()
	// On the same filesystem as the sources, so they move by rename.
	off := "/etc/apt/nexus-sources-off"
	if err := os.RemoveAll(off); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(off, 0o755); err != nil {
		t.Fatal(err)
	}
	moved := map[string]string{}
	entries, _ := os.ReadDir("/etc/apt/sources.list.d")
	var srcs []string
	for _, e := range entries {
		srcs = append(srcs, filepath.Join("/etc/apt/sources.list.d", e.Name()))
	}
	if exists("/etc/apt/sources.list") {
		srcs = append(srcs, "/etc/apt/sources.list")
	}
	for _, src := range srcs {
		dst := filepath.Join(off, strings.ReplaceAll(src, "/", "_"))
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
		moved[src] = dst
	}
	purge := func() {
		_, _ = sh(2*time.Minute, apt.Dpkg, "--purge", aptPlain)
		_ = os.RemoveAll(aptRepo)
		_ = os.Remove(aptFixtureList)
		for _, pin := range []string{"/etc/apt/preferences.d/nexus-pin"} {
			_ = os.Remove(pin)
		}
	}
	purge()
	t.Cleanup(func() {
		purge()
		for src, dst := range moved {
			if err := os.Rename(dst, src); err != nil {
				t.Errorf("restore %s: %v", src, err)
			}
		}
		if err := os.Remove(off); err != nil {
			t.Errorf("remove %s: %v", off, err)
		}
	})

	aptBuild(t, "init", aptRepo)
	debs := filepath.Join(t.TempDir(), "debs")
	mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", debs, aptPlain, "1.0"))
	if withV2 {
		aptBuild(t, "add", aptRepo, aptPlain, "2.0")
	}
	if err := os.WriteFile(aptFixtureList, []byte("deb [trusted=yes] file:"+aptRepo+" ./\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSh(t, apt.AptGet, "update", "-qq")
}

// aptd is a real nexusd under the shipped unit with apt enabled, and a
// token holding packages:read and packages:write.
func aptd(t *testing.T, name string) *nexusdInstance {
	t.Helper()
	n := newNexusd(t, name, "  apt:\n    enabled: true\n")
	tok, _, err := auth.Create(filepath.Join(nexusdState, "nexusd-"+name), auth.CreateRequest{Name: "apt",
		Scopes: []string{spec.ReadScope, spec.WriteScope}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	return n
}

func aptPost(n *nexusdInstance, path string) (int, []byte) {
	n.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+n.listen+path, strings.NewReader("{}"))
	if err != nil {
		n.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		n.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		n.t.Fatal(err)
	}
	return resp.StatusCode, b
}

// aptRefresh refreshes through the API and returns the terminal operation.
func aptRefresh(t *testing.T, n *nexusdInstance) apitypes.Operation {
	t.Helper()
	code, b := aptPost(n, refreshAt)
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil || code != http.StatusAccepted {
		t.Fatalf("refresh: %d %s", code, b)
	}
	waitUntil(t, 3*time.Minute, "refresh "+op.ID, func() (bool, string) {
		n.mustGet("/v1/operations/"+op.ID, &op)
		return op.Status.Terminal(), string(op.Status)
	})
	return op
}

// aptDpkgSnapshot is dpkg-query's view of every package, run in the test.
func aptDpkgSnapshot(t *testing.T) string {
	t.Helper()
	return mustSh(t, apt.DpkgQuery, "-W", "-f=${Package}\t${Architecture}\t${Version}\t${db:Status-Status}\n")
}

// aptPolicy is apt-cache policy's Installed and Candidate for names, read in
// the test by plain line scanning.
func aptPolicy(t *testing.T, names ...string) map[string][2]string {
	t.Helper()
	out := map[string][2]string{}
	for chunk := range slices.Chunk(names, 400) {
		cur := ""
		for _, line := range strings.Split(mustSh(t, apt.AptCache, append([]string{"policy", "--"}, chunk...)...), "\n") {
			switch {
			case !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":"):
				cur = strings.TrimSuffix(line, ":")
			case strings.HasPrefix(line, "  Installed: "):
				e := out[cur]
				e[0] = strings.TrimPrefix(line, "  Installed: ")
				out[cur] = e
			case strings.HasPrefix(line, "  Candidate: "):
				e := out[cur]
				e[1] = strings.TrimPrefix(line, "  Candidate: ")
				out[cur] = e
			}
		}
	}
	return out
}

func aptListAll[T any](t *testing.T, n *nexusdInstance, at string, limit int) []apitypes.Resource[T] {
	t.Helper()
	var all []apitypes.Resource[T]
	cursor := ""
	for range 10000 {
		q := url.Values{"limit": {fmt.Sprint(limit)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page apitypes.List[apitypes.Resource[T]]
		n.mustGet(at+"?"+q.Encode(), &page)
		all = append(all, page.Items...)
		if cursor = page.NextCursor; cursor == "" {
			return all
		}
	}
	t.Fatal("the list never ended")
	return nil
}

// TC-123: refresh is 202 and terminal and changes no package; upgradable,
// installed (paged) and get match apt-cache policy and dpkg-query run here.
func TestAptReadsMatchIndependentReadback(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	n := aptd(t, "apt-reads")

	before := aptDpkgSnapshot(t)
	if op := aptRefresh(t, n); op.Status != apitypes.StatusSucceeded {
		t.Fatalf("refresh: %+v", op)
	}
	if after := aptDpkgSnapshot(t); after != before {
		t.Fatal("refresh changed the installed packages")
	}

	// Installed, paged: exactly dpkg-query's set.
	installed := aptListAll[spec.PackageProperties](t, n, packagesAt, 37)
	var got, want []string
	for _, p := range installed {
		got = append(got, fmt.Sprintf("%s %s %s %s", p.Name, p.Properties.Architecture, p.Properties.InstalledVersion, p.Properties.Status))
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(aptDpkgSnapshot(t)), "\n") {
		f := strings.Split(line, "\t")
		if f[3] == "not-installed" || f[3] == "config-files" {
			continue
		}
		want = append(want, strings.Join(f, " "))
		names = append(names, f[0])
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("paged list (%d) differs from dpkg-query (%d):\ngot  %q\nwant %q", len(got), len(want), got, want)
	}

	// Upgradable: exactly the packages whose policy Candidate differs.
	pol := aptPolicy(t, names...)
	var wantUp []string
	for _, name := range names {
		if e := pol[name]; e[1] != "(none)" && e[1] != e[0] {
			wantUp = append(wantUp, fmt.Sprintf("%s %s %s", name, e[0], e[1]))
		}
	}
	slices.Sort(wantUp)
	var gotUp []string
	for _, u := range aptListAll[spec.UpgradableProperties](t, n, upgradableAt, 100) {
		gotUp = append(gotUp, fmt.Sprintf("%s %s %s", u.Name, u.Properties.InstalledVersion, u.Properties.CandidateVersion))
		if u.Name == aptPlain && (u.Properties.Security != spec.SecurityUnknown || !slices.Equal(u.Properties.Origins, []string{"localhost"})) {
			t.Errorf("fixture package: %+v", u)
		}
	}
	if !slices.Equal(gotUp, wantUp) || !slices.Contains(gotUp, aptPlain+" 1.0 2.0") {
		t.Fatalf("upgradable %q, want %q including nxplain 1.0 2.0", gotUp, wantUp)
	}

	// Get: versions from policy, details marked untrusted.
	var d spec.PackageDetail
	n.mustGet(packagesAt+"/"+aptPlain, &d)
	if d.Properties.InstalledVersion != pol[aptPlain][0] || d.Properties.CandidateVersion != pol[aptPlain][1] ||
		!d.Properties.Upgradable || d.Properties.Summary != "Nexus apt fixture package nxplain" || d.Properties.Status != "installed" {
		t.Errorf("get: %+v, policy %q", d, pol[aptPlain])
	}
	if op := spec.Get.Op(); !op.Untrusted || !strings.Contains(op.Description, "untrusted") {
		t.Errorf("get is not marked untrusted: %+v", op)
	}

	// apt would read these as regular expressions matching nxplain.
	for _, name := range []string{"nxplai.", "nxpla+", "nxplain.", "nxp"} {
		if code, slug, _ := n.problem(packagesAt + "/" + url.PathEscape(name)); code != http.StatusNotFound || slug != problem.NotFound {
			t.Errorf("get %q: %d %s, want 404 not-found", name, code, slug)
		}
	}
	if after := aptDpkgSnapshot(t); after != before {
		t.Fatal("the reads changed the installed packages")
	}
}

func aptUpgradableNamed(t *testing.T, n *nexusdInstance, name string) (spec.Upgradable, bool, time.Duration) {
	t.Helper()
	start := time.Now()
	var page apitypes.List[spec.Upgradable]
	n.mustGet(upgradableAt+"?limit=1000", &page)
	d := time.Since(start)
	for _, u := range page.Items {
		if u.Name == name {
			return u, true, d
		}
	}
	return spec.Upgradable{}, false, d
}

// TC-124: the cached list answers under 200 ms, and a new version after a
// refresh, a pin, and an install outside Nexus each show on the first read
// after them.
func TestAptUpgradableCacheMeetsTargetAndInvalidates(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	n := aptd(t, "apt-cache")

	u, ok, first := aptUpgradableNamed(t, n, aptPlain)
	if !ok || u.Properties.CandidateVersion != "2.0" {
		t.Fatalf("first read: %+v %v", u, ok)
	}
	_, _, second := aptUpgradableNamed(t, n, aptPlain)
	t.Logf("upgradable: first read %v, cached read %v", first, second)
	if second >= 200*time.Millisecond {
		t.Errorf("cached read took %v, want under 200ms", second)
	}

	// Step 2: v3 in the repository, refreshed through Nexus.
	aptBuild(t, "add", aptRepo, aptPlain, "3.0")
	if op := aptRefresh(t, n); op.Status != apitypes.StatusSucceeded {
		t.Fatalf("refresh: %+v", op)
	}
	if u, ok, _ := aptUpgradableNamed(t, n, aptPlain); !ok || u.Properties.CandidateVersion != "3.0" {
		t.Fatalf("after v3 and refresh: %+v %v", u, ok)
	}
	if got := aptPolicy(t, aptPlain)[aptPlain]; got != [2]string{"1.0", "3.0"} {
		t.Fatalf("apt-cache policy after v3: %q", got)
	}

	// Step 3: a pin, then an install outside Nexus.
	pin := "Package: nxplain\nPin: version 2.0\nPin-Priority: 990\n"
	if err := os.WriteFile("/etc/apt/preferences.d/nexus-pin", []byte(pin), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := aptPolicy(t, aptPlain)[aptPlain]; got != [2]string{"1.0", "2.0"} {
		t.Fatalf("apt-cache policy with the pin: %q", got)
	}
	if u, ok, _ := aptUpgradableNamed(t, n, aptPlain); !ok || u.Properties.CandidateVersion != "2.0" {
		t.Fatalf("after the pin: %+v %v", u, ok)
	}
	mustSh(t, apt.Dpkg, "-i", filepath.Join(aptRepo, aptPlain+"_3.0_all.deb"))
	if got := aptPolicy(t, aptPlain)[aptPlain]; got != [2]string{"3.0", "3.0"} {
		t.Fatalf("apt-cache policy after dpkg -i: %q", got)
	}
	if u, ok, _ := aptUpgradableNamed(t, n, aptPlain); ok {
		t.Fatalf("after dpkg -i 3.0 the package is still upgradable: %+v", u)
	}
	var d spec.PackageDetail
	n.mustGet(packagesAt+"/"+aptPlain, &d)
	if d.Properties.InstalledVersion != "3.0" || d.Properties.Upgradable {
		t.Fatalf("get after dpkg -i: %+v", d.Properties)
	}
}

func aptCapability(t *testing.T, n *nexusdInstance) apitypes.ProviderCapability {
	t.Helper()
	var caps apitypes.Capabilities
	n.mustGet("/v1/capabilities?refresh=true", &caps)
	for _, p := range caps.Providers {
		if p.Name == spec.Name {
			return p
		}
	}
	t.Fatal("no apt provider in capabilities")
	return apitypes.ProviderCapability{}
}

// aptMoveAside renames path out of the way. The returned restore puts it
// back; cleanup calls it too, so it is back however the test ends.
func aptMoveAside(t *testing.T, path string) (restore func()) {
	t.Helper()
	off := path + ".nexus-off"
	if err := os.Rename(path, off); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	restore = func() {
		once.Do(func() {
			if err := os.Rename(off, path); err != nil {
				t.Errorf("restore %s: %v", path, err)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}

// TC-125: false with no marker, true with the packages named, unknown once
// the marker writers the provider names in its source are gone.
func TestAptRebootRequiredState(t *testing.T) {
	requireInit(t, platform.Systemd)
	clear := func() {
		_ = os.Remove(apt.RebootMarker)
		_ = os.Remove(apt.RebootPackagesFile)
	}
	clear()
	t.Cleanup(clear)
	var writers []string
	for _, w := range apt.RebootMarkerWriters {
		if exists(w) {
			writers = append(writers, w)
		}
	}
	// A base with no writer installed (Debian's needrestart has no Ubuntu
	// interface) starts at unknown: the marker's absence proves nothing.
	step1 := spec.RebootFalse
	if len(writers) == 0 {
		t.Logf("none of %q is installed: step 1 is unknown on this base", apt.RebootMarkerWriters)
		step1 = spec.RebootUnknown
	}
	n := aptd(t, "apt-reboot")

	c := aptCapability(t, n)
	if c.Status != apitypes.ProviderEnabled || c.Details[spec.DetailRebootRequired] != step1 {
		t.Fatalf("step 1: %+v, want %s", c, step1)
	}

	if err := os.WriteFile(apt.RebootMarker, []byte("*** System restart required ***\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(apt.RebootPackagesFile, []byte("linux-image-generic\nlibc6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = aptCapability(t, n)
	pkgs, _ := c.Details[spec.DetailRebootPackages].([]any)
	if c.Details[spec.DetailRebootRequired] != spec.RebootTrue || fmt.Sprint(pkgs) != "[linux-image-generic libc6]" {
		t.Fatalf("step 2: %+v", c)
	}

	clear()
	for _, w := range writers {
		aptMoveAside(t, w)
	}
	if c = aptCapability(t, n); c.Details[spec.DetailRebootRequired] != spec.RebootUnknown {
		t.Fatalf("step 3: %+v", c)
	}
}

// TC-ERR45: an unknown package is 404, an empty upgradable list is
// items: [], and a missing apt-get makes apt unavailable naming it while
// nexusd stays up.
func TestAptPreflightAndAbsentPackages(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, false)
	n := aptd(t, "apt-err")

	if code, slug, _ := n.problem(packagesAt + "/nxnosuch"); code != http.StatusNotFound || slug != problem.NotFound {
		t.Errorf("unknown package: %d %s", code, slug)
	}
	code, b := n.get(upgradableAt)
	if code != http.StatusOK || !strings.Contains(string(b), `"items":[]`) {
		t.Errorf("empty upgradable list: %d %s", code, b)
	}

	restore := aptMoveAside(t, apt.AptGet)
	c := aptCapability(t, n)
	if c.Status != apitypes.ProviderUnavailable || !strings.Contains(c.Reason, apt.AptGet) {
		t.Errorf("apt-get missing: %+v", c)
	}
	if st := activeState(t, nexusdUnit); st != "active" {
		t.Fatalf("nexusd %s after the preflight failure", st)
	}
	if code, b := n.get(upgradableAt); code != http.StatusServiceUnavailable {
		t.Errorf("upgradable while unavailable: %d %s", code, b)
	}
	restore()
	if c := aptCapability(t, n); c.Status != apitypes.ProviderEnabled {
		t.Errorf("after restoring apt-get: %+v", c)
	}
}
