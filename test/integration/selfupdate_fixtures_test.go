// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	nrelease "github.com/mauktik/nexus/internal/release"
)

// The self-update fixtures run.sh builds, signs and copies in (WI 7.1), and
// the copy of run.sh's own log it makes just before the tests run.
const (
	fixtureDir    = "/harness/selfupdate"
	harnessRunLog = "/harness/run.log"
	selfupdatePub = "release-test.pub"
	// shippedInstallSh is the unpatched install.sh of the harness release
	// tree (make nexusd-tree).
	shippedInstallSh  = "/harness/release/nexusd/install.sh"
	fixtureRunTimeout = 30 * time.Second
)

// selfupdateVersions are the fixture versions: v9.9.6 is signed by a second
// key the per-run public key does not trust.
var selfupdateVersions = []string{"v9.9.1", "v9.9.2", "v9.9.3", "v9.9.4", "v9.9.5", "v9.9.6", "v9.9.7"}

const untrustedFixture = "v9.9.6"

// selfupdateTarball is the path of a fixture tarball in the container.
func selfupdateTarball(v string) string {
	return filepath.Join(fixtureDir, "nexusd-"+v+"-linux-"+runtime.GOARCH+".tar.gz")
}

// tarTops returns the distinct first path elements of a .tar.gz's entries.
func tarTops(t *testing.T, file string) []string {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	tr := tar.NewReader(zr)
	var tops []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		top, _, _ := strings.Cut(strings.TrimSuffix(h.Name, "/"), "/")
		if !slices.Contains(tops, top) {
			tops = append(tops, top)
		}
	}
	return tops
}

// TC-SM-107 (WI 7.1; NFR-6, FR-15, CON-1): the fixture tarballs in
// /harness/selfupdate are signed with the per-run key, which verifies all of
// them but v9.9.6; each holds one nexusd-<v>-linux-<goarch> directory; each
// fixture's binaries behave as its case needs; and run.sh's log shows
// `nexus-release verify` passing for each trusted fixture before the
// copy-in, with the host's real signing key untouched.
func TestTCSM107_FixtureTarballsAreSignedWithA(t *testing.T) {
	// nexusd self-management is systemd-only: its restart, update and
	// rollback swap and restart nexusd.service.
	requireInit(t, platform.Systemd)
	ents, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	want := []string{selfupdatePub}
	for _, v := range selfupdateVersions {
		b := filepath.Base(selfupdateTarball(v))
		want = append(want, b, b+".sig")
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s holds %q, want %q", fixtureDir, got, want)
	}

	pubB64, err := os.ReadFile(filepath.Join(fixtureDir, selfupdatePub))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := nrelease.ParsePublicKey(strings.TrimSpace(string(pubB64)))
	if err != nil {
		t.Fatalf("%s: %v", selfupdatePub, err)
	}
	for _, v := range selfupdateVersions {
		file := selfupdateTarball(v)
		digest, _, err := nrelease.DigestFile(file)
		if err != nil {
			t.Fatal(err)
		}
		sigText, err := os.ReadFile(file + ".sig")
		if err != nil {
			t.Fatal(err)
		}
		sig, err := nrelease.DecodeSignature(sigText)
		if err != nil {
			t.Fatalf("%s.sig: %v", file, err)
		}
		ok := nrelease.Verify([]ed25519.PublicKey{pub}, digest, sig)
		if ok != (v != untrustedFixture) {
			t.Errorf("%s: Verify against the per-run key = %v, want %v", filepath.Base(file), ok, v != untrustedFixture)
		}
		top := "nexusd-" + v + "-linux-" + runtime.GOARCH
		if tops := tarTops(t, file); !slices.Equal(tops, []string{top}) {
			t.Errorf("%s: top-level entries %q, want only %s (A-13)", filepath.Base(file), tops, top)
		}
	}

	t.Run("fixture binaries", func(t *testing.T) { checkFixtureBehaviour(t) })

	t.Run("run.sh log", func(t *testing.T) {
		b, err := os.ReadFile(harnessRunLog)
		if err != nil {
			t.Fatal(err)
		}
		log := string(b)
		copyIn := strings.Index(log, "harness: copying the self-update fixtures into "+fixtureDir)
		if copyIn < 0 {
			t.Fatalf("%s has no copy-in line:\n%s", harnessRunLog, log)
		}
		for _, v := range selfupdateVersions {
			name := regexp.QuoteMeta(filepath.Base(selfupdateTarball(v)))
			var re *regexp.Regexp
			if v == untrustedFixture {
				re = regexp.MustCompile(`harness: fixture ` + regexp.QuoteMeta(v) + `: refused by the per-run key, as intended`)
			} else {
				re = regexp.MustCompile(`nexus-release: \S*/` + name + `: signature verified`)
			}
			loc := re.FindStringIndex(log)
			switch {
			case loc == nil:
				t.Errorf("%s has no line matching %s", harnessRunLog, re)
			case loc[0] > copyIn:
				t.Errorf("%s: the %s verify line comes after the copy-in", harnessRunLog, v)
			}
		}
		key := regexp.MustCompile(`harness: host signing key \S+/\.config/nexus/release-signing\.key unchanged \((absent|mtime \d+)\)`)
		if loc := key.FindStringIndex(log); loc == nil || loc[0] > copyIn {
			t.Errorf("%s does not show the host's release-signing.key unchanged before the copy-in", harnessRunLog)
		}
	})
}

// checkFixtureBehaviour unpacks each fixture and checks what its case relies
// on: the binary each install.sh installs, and the install.sh patches.
func checkFixtureBehaviour(t *testing.T) {
	root := t.TempDir()
	tree := map[string]string{}
	for _, v := range selfupdateVersions {
		mustSh(t, "tar", "-xzf", selfupdateTarball(v), "-C", root)
		tree[v] = filepath.Join(root, "nexusd-"+v+"-linux-"+runtime.GOARCH)
	}
	version := func(bin string) string {
		out, err := sh(fixtureRunTimeout, bin, "-version")
		if err != nil {
			t.Errorf("%s -version: %v\n%s", bin, err, out)
		}
		return strings.TrimSpace(out)
	}
	for _, v := range selfupdateVersions {
		if got := version(filepath.Join(tree[v], "nexusd")); got != "nexusd "+v {
			t.Errorf("%s/nexusd -version = %q, want %q", v, got, "nexusd "+v)
		}
	}
	if got := version(filepath.Join(tree["v9.9.5"], "nexusd-alt")); got != "nexusd v9.9.8" {
		t.Errorf("v9.9.5/nexusd-alt -version = %q, want nexusd v9.9.8", got)
	}
	// The serve-failing stubs: v9.9.4's nexusd, and v9.9.7's restore stub.
	stubs := map[string]string{
		filepath.Join(tree["v9.9.4"], "nexusd"):              "nexusd v9.9.4",
		filepath.Join(tree["v9.9.7"], "nexusd-restore-stub"): "nexusd v9.9.1",
	}
	for bin, want := range stubs {
		if got := version(bin); got != want {
			t.Errorf("%s -version = %q, want %q", bin, got, want)
		}
		if out, err := sh(fixtureRunTimeout, bin, "validate-config", "--config", nexusdConfig); err != nil {
			t.Errorf("%s validate-config: %v\n%s", bin, err, out)
		}
		if out, err := sh(fixtureRunTimeout, bin, "serve", "--config", nexusdConfig); err == nil {
			t.Errorf("%s serve exited 0, want 1:\n%s", bin, out)
		}
		if fi, err := os.Stat(bin); err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: mode %v (%v), want 0755", bin, fi.Mode(), err)
		}
	}

	read := func(v string) string {
		b, err := os.ReadFile(filepath.Join(tree[v], "install.sh"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const binMv = `mv -f "$BIN.new" "$BIN"`
	if s := read("v9.9.3"); !strings.Contains(s, binMv+"\nexit 3\n") {
		t.Errorf("v9.9.3 install.sh does not exit 3 right after the binary mv")
	}
	if s := read("v9.9.5"); !strings.Contains(s, `"$SRC/nexusd-alt" "$BIN.new"`) {
		t.Errorf("v9.9.5 install.sh does not install nexusd-alt")
	}
	s := read("v9.9.7")
	clobber := strings.Index(s, `"$SRC/nexusd-restore-stub" /usr/local/bin/nexusd.prev`)
	exit3 := strings.Index(s, "\nexit 3\n")
	mv := strings.Index(s, binMv)
	if clobber < 0 || exit3 < clobber || mv < exit3 {
		t.Errorf("v9.9.7 install.sh does not copy the restore stub over nexusd.prev, then exit 3, before the binary mv")
	}
	shipped, err := os.ReadFile(shippedInstallSh)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v9.9.1", "v9.9.2", "v9.9.4", "v9.9.6"} {
		if read(v) != string(shipped) {
			t.Errorf("%s install.sh differs from the shipped %s", v, shippedInstallSh)
		}
	}
}
