//go:build integration

package integration

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/apt"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
	"github.com/mauktik/nexus/test/harness/fixtures/aptrepo"
)

// NFR-6 and FR-14 (WI 7.5): the apt suite runs unchanged on every base
// make test-harness takes (BASE=ubuntu:latest and BASE=debian:stable), and
// security degrades to unknown, never to a failure, where it cannot be
// determined. Origins come from Release files the test writes, with suites
// that belong to no release, so no expectation depends on the base.

const portabilityList = "/etc/apt/sources.list.d/nexus-portability.list"

// releaseRepo is a flat repository holding name 2.0 with a Release file
// naming label and suite, and the SHA256 of its Packages index so apt
// accepts it under [trusted=yes].
func releaseRepo(t *testing.T, dir, label, suite, name string) {
	t.Helper()
	aptBuild(t, "add", dir, name, "2.0")
	idx, err := os.ReadFile(filepath.Join(dir, "Packages"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(idx)
	rel := fmt.Sprintf("Origin: %s\nLabel: %s\nSuite: %s\nCodename: %s\nVersion: 0\nDate: Thu, 01 Jan 2026 00:00:00 UTC\n"+
		"SHA256:\n %s %d Packages\n", label, label, suite, suite, hex.EncodeToString(sum[:]), len(idx))
	if err := os.WriteFile(filepath.Join(dir, "Release"), []byte(rel), 0o644); err != nil {
		t.Fatal(err)
	}
}

// simOrigins is the origins of name's Inst line in the test's own
// apt-get -s dist-upgrade, as apt prints them.
func simOrigins(t *testing.T, name string) string {
	t.Helper()
	out := mustSh(t, apt.AptGet, "-s", "-o", "APT::Color=0", "dist-upgrade")
	re := regexp.MustCompile(`(?m)^Inst ` + regexp.QuoteMeta(name) + ` \[[^]]*\] \(\S+ (.*) \[[^]]*\]\)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no Inst line for %s in apt-get -s dist-upgrade:\n%s", name, out)
	}
	return m[1]
}

// TC-141: on whatever base runs it, the upgradable list classifies the
// fixture candidates by their origins alone: a -security suite is yes, a
// known vendor's other suite is no, a third party and a flat repository
// with no Release file ("localhost") are unknown. When apt-get -s
// dist-upgrade fails (a real unmet dependency, checked by a control), the
// list still answers with every version and security unknown.
func TestAptSecurityDegradesOnThisBase(t *testing.T) {
	requireInit(t, platform.Systemd)
	osr, _ := os.ReadFile("/etc/os-release")
	for line := range strings.Lines(string(osr)) {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			t.Logf("base: %s; %s", strings.TrimSpace(line), strings.SplitN(mustSh(t, apt.AptGet, "--version"), "\n", 2)[0])
			break
		}
	}
	aptFixture(t, true)
	repos := []struct{ dir, label, suite, pkg, want string }{
		{"/srv/nexus-aptrepo-sec", "Debian-Security", "nxfixture-security", "nxsec", spec.SecurityYes},
		{"/srv/nexus-aptrepo-vendor", "Debian", "nxfixture", "nxvendor", spec.SecurityNo},
		{"/srv/nexus-aptrepo-third", "NexusThirdParty", "nxfixture", "nxthird", spec.SecurityUnknown},
	}
	pkgs := []string{"nxbroken"}
	for _, r := range repos {
		pkgs = append(pkgs, r.pkg)
	}
	aptPurgeAround(t, pkgs...)
	t.Cleanup(func() {
		_ = os.Remove(portabilityList)
		for _, r := range repos {
			_ = os.RemoveAll(r.dir)
		}
	})
	var list strings.Builder
	debs := filepath.Join(t.TempDir(), "debs")
	for _, r := range repos {
		_ = os.RemoveAll(r.dir)
		releaseRepo(t, r.dir, r.label, r.suite, r.pkg)
		mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", debs, r.pkg, "1.0"))
		fmt.Fprintf(&list, "deb [trusted=yes] file:%s ./\n", r.dir)
	}
	if err := os.WriteFile(portabilityList, []byte(list.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSh(t, apt.AptGet, "update", "-qq")
	n := aptd(t, "apt-portability")

	type want struct{ origins, security string }
	expect := map[string]want{aptPlain: {"localhost", spec.SecurityUnknown}}
	for _, r := range repos {
		expect[r.pkg] = want{r.label + ":0/" + r.suite, r.want}
	}
	for name, w := range expect {
		if got := simOrigins(t, name); got != w.origins {
			t.Fatalf("control: apt-get -s prints origins %q for %s, the test wrote %q", got, name, w.origins)
		}
	}
	valid := []string{spec.SecurityYes, spec.SecurityNo, spec.SecurityUnknown}
	check := func(step string, degraded bool) {
		t.Helper()
		seen := 0
		for _, u := range aptListAll[spec.UpgradableProperties](t, n, upgradableAt, 100) {
			p := u.Properties
			if !slices.Contains(valid, p.Security) {
				t.Errorf("%s: %s security %q is not one of %q", step, u.Name, p.Security, valid)
			}
			w, ok := expect[u.Name]
			if !ok {
				continue
			}
			seen++
			wantSec, wantOrig := w.security, w.origins
			if degraded {
				wantSec, wantOrig = spec.SecurityUnknown, ""
			}
			if p.Security != wantSec || strings.Join(p.Origins, ", ") != wantOrig || p.InstalledVersion != "1.0" ||
				p.CandidateVersion != "2.0" {
				t.Errorf("%s: %s %+v, want security %s origins %q", step, u.Name, p, wantSec, wantOrig)
			}
			t.Logf("%s: %s %s>%s origins %q security %s", step, u.Name, p.InstalledVersion, p.CandidateVersion,
				p.Origins, p.Security)
		}
		if seen != len(expect) {
			t.Errorf("%s: %d of the %d fixture packages listed", step, seen, len(expect))
		}
	}
	check("classified", false)

	// A real unmet dependency makes apt-get -s dist-upgrade fail.
	mustSh(t, apt.Dpkg, "-i", "--force-depends", aptBuild(t, "deb", debs, "nxbroken", "1.0", "--depends", "nxabsent"))
	out, err := sh(2*time.Minute, apt.AptGet, "-s", "-o", "APT::Color=0", "dist-upgrade")
	if err == nil {
		t.Fatalf("control: apt-get -s dist-upgrade succeeded with an unmet dependency, so this step tests nothing:\n%s", out)
	}
	t.Logf("control: apt-get -s dist-upgrade with nxbroken's unmet dependency: %v: %s", err,
		strings.ReplaceAll(strings.TrimSpace(out), "\n", " | "))
	check("simulation failing", true)

	mustSh(t, apt.Dpkg, "--purge", "nxbroken")
	check("after the purge", false)
}

// The suite's own sources, scanned for anything that ties a test to a
// release (TC-141, "nothing depends on an Ubuntu release").
//
//go:embed *.go
var suiteSources embed.FS

// releaseMarkers are release names and numbers and the ways to read them.
// The pattern is assembled, so this file does not match itself.
var releaseMarkers = regexp.MustCompile(`(?i)\b(` + strings.Join([]string{
	"focal", "jammy", "kinetic", "lunar", "mantic", "noble", "oracular", "plucky", "questing", "resolute",
	"bullseye", "bookworm", "trixie", "forky",
	`(1[4-9]|2[0-9])\.(04|10)`, "os" + "-release", "lsb" + "_release", "VERSION" + "_CODENAME", "VERSION" + "_ID",
	"UBUNTU" + "_CODENAME", "DISTRIB" + "_",
}, "|") + `)\b`)

// TestAptSuiteHasNoReleaseDependency: no test source of the T1 suite and
// not the fixture repository's build script names a release or reads the
// base's release, so the same suite runs on every BASE. This file is
// skipped: it reads /etc/os-release only to log the base.
func TestAptSuiteHasNoReleaseDependency(t *testing.T) {
	requireInit(t, platform.Systemd)
	files := map[string][]byte{"test/harness/fixtures/aptrepo/build.sh": aptrepo.BuildScript}
	err := fs.WalkDir(suiteSources, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "apt_portability_test.go" {
			return err
		}
		b, err := suiteSources.ReadFile(p)
		files["test/integration/"+p] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 10 {
		t.Fatalf("only %d sources embedded", len(files))
	}
	for name, b := range files {
		for i, line := range strings.Split(string(b), "\n") {
			if m := releaseMarkers.FindString(line); m != "" {
				t.Errorf("%s:%d names %q: %s", name, i+1, m, strings.TrimSpace(line))
			}
		}
	}
	t.Logf("%d sources scanned, no release marker", len(files))
}
