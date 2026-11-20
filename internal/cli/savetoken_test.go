// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-19, TC-RA-E5,
// TC-RA-ERR9 and TC-RA-ERR10 (WI 1.5): --save-as-host on the planted
// SecretOutput op tcra.tokens.add (remoteadmin_tc_test.go).

package cli

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
)

const tcraSecret = "nxs_aaaaaaaaaaaa_RASAVE"

// startTCRATLSServer is startTCRAServer over TLS, so a host can pin it.
func startTCRATLSServer(t *testing.T) (*tcraServer, string) {
	t.Helper()
	ts := &tcraServer{counts: map[string]int{}}
	ts.Server = httptest.NewTLSServer(tcraHandler(ts))
	t.Cleanup(ts.Close)
	return ts, client.Fingerprint(ts.Certificate().Raw)
}

// saveConfig writes cfg as the client configuration under a fresh HOME and
// returns that HOME and the file's path.
func saveConfig(t *testing.T, cfg *clientconfig.Config) (home, path string) {
	t.Helper()
	home = t.TempDir()
	path, err := clientconfig.Path(homeEnv(home).Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientconfig.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	return home, path
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// noSecret fails when the planted secret appears in s.
func noSecret(t *testing.T, what, s string) {
	t.Helper()
	if n := strings.Count(s, tcraSecret); n != 0 {
		t.Errorf("%s holds the secret %d times: %q", what, n, s)
	}
}

func TestTCRA19_SaveAsHostWithOneTargetStoresTheTokenWithMode(t *testing.T) {
	ts, fp := startTCRATLSServer(t)
	home, path := saveConfig(t, &clientconfig.Config{Hosts: map[string]clientconfig.Host{
		"h1": {URL: ts.URL, Fingerprint: fp, Token: "tok"},
	}})
	r := runSpecsCLI(homeEnv(home), tcraSpecs(), "tcra", "add", "ci", "--host", "h1", "--save-as-host", "newh")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	noSecret(t, "stdout", r.stdout)
	noSecret(t, "stderr", r.stderr)
	if want := "token saved as host newh in " + path + "\n"; r.stderr != want {
		t.Errorf("stderr %q, want %q", r.stderr, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %04o, want 0600", fi.Mode().Perm())
	}
	cfg, err := clientconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := clientconfig.Host{URL: ts.URL, Fingerprint: fp, Token: tcraSecret}
	if got := cfg.Hosts["newh"]; got.URL != want.URL || got.Fingerprint != want.Fingerprint || got.Token != want.Token || len(got.Tags) != 0 {
		t.Errorf("newh %+v, want %+v", got, want)
	}
	if cfg.DefaultHost != "newh" {
		t.Errorf("defaultHost %q, want newh (it was unset)", cfg.DefaultHost)
	}
	if h1 := cfg.Hosts["h1"]; h1.Token != "tok" {
		t.Errorf("h1 changed: %+v", h1)
	}
	lines := jsonLines(t, "stdout", r.stdout)
	if len(lines) != 1 {
		t.Fatalf("stdout %q, want one object", r.stdout)
	}
	if _, ok := lines[0]["token"]; ok || lines[0]["tokenId"] != "aaaaaaaaaaaa" || lines[0]["changed"] != true || lines[0]["host"] != "h1" {
		t.Errorf("stdout object %v, want the result without token", lines[0])
	}
}

func TestTCRAE5_TwoHostSaveAsHostIsRefusedBeforeAnythingIsSent(t *testing.T) {
	ts, fp := startTCRATLSServer(t)
	other, _ := startTCRATLSServer(t)
	home, path := saveConfig(t, &clientconfig.Config{DefaultHost: "other", Hosts: map[string]clientconfig.Host{
		"h1":    {URL: ts.URL, Fingerprint: fp, Token: "old", Tags: []string{"lab"}},
		"h2":    {URL: other.URL, Token: "tok2"},
		"other": {URL: other.URL, Token: "tok3"},
	}})
	var stderr strings.Builder

	// Step 1: the same name and URL rotates the token in place.
	r := runSpecsCLI(homeEnv(home), tcraSpecs(), "tcra", "add", "ci", "--host", "h1", "--save-as-host", "h1")
	stderr.WriteString(r.stderr)
	if r.code != 0 {
		t.Fatalf("rotate: exit %d: %s", r.code, r.stderr)
	}
	cfg, err := clientconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h1 := cfg.Hosts["h1"]
	if h1.Token != tcraSecret || !slices.Equal(h1.Tags, []string{"lab"}) || h1.URL != ts.URL || h1.Fingerprint != fp {
		t.Errorf("h1 after rotation %+v", h1)
	}
	if cfg.DefaultHost != "other" {
		t.Errorf("defaultHost %q, want other", cfg.DefaultHost)
	}
	noSecret(t, "rotate stdout", r.stdout)

	// Step 2: two targets are a usage error, with nothing sent.
	before := readFile(t, path)
	ts.take()
	other.take()
	r = runSpecsCLI(homeEnv(home), tcraSpecs(), "tcra", "add", "ci", "--host", "h1,h2", "--save-as-host", "x", "--yes")
	stderr.WriteString(r.stderr)
	if r.code != ExitUsage || !strings.Contains(r.stderr, "--save-as-host") || !strings.Contains(r.stderr, "one host") {
		t.Errorf("two hosts: exit %d, stderr %q; want 2 naming --save-as-host and one host", r.code, r.stderr)
	}
	if n := ts.total() + other.total(); n != 0 {
		t.Errorf("two hosts: %d requests sent", n)
	}
	if !bytes.Equal(readFile(t, path), before) {
		t.Error("two hosts: the client config changed")
	}
	noSecret(t, "stderr", stderr.String())
}

func TestTCRAERR9_SaveAsHostRefusalsSendNothing(t *testing.T) {
	ts, fp := startTCRATLSServer(t)
	home, path := saveConfig(t, &clientconfig.Config{Hosts: map[string]clientconfig.Host{
		"h1": {URL: ts.URL, Fingerprint: fp, Token: "tok"},
		"h2": {URL: "https://192.0.2.1:8443", Token: "tok2"},
	}})
	before := readFile(t, path)
	var stderr strings.Builder
	for _, tc := range []struct {
		name   string
		args   []string
		reason string
	}{
		{"url mismatch", []string{"--save-as-host", "h2", "--host", "h1"}, "https://192.0.2.1:8443"},
		{"bad name", []string{"--save-as-host", "bad name", "--host", "h1"}, "invalid host name"},
		{"stdin", []string{"--save-as-host", "x", "--host", "h1", "--stdin"}, "--stdin"},
	} {
		env := homeEnv(home)
		args := []string{"tcra", "add", "ci"}
		if tc.name == "stdin" {
			args = args[:2]
			env.Stdin = strings.NewReader("ci\n")
		}
		r := runSpecsCLI(env, tcraSpecs(), append(args, tc.args...)...)
		stderr.WriteString(r.stderr)
		if r.code != ExitUsage || !strings.Contains(r.stderr, tc.reason) {
			t.Errorf("%s: exit %d, stderr %q; want 2 naming %q", tc.name, r.code, r.stderr, tc.reason)
		}
		if n := ts.total(); n != 0 {
			t.Errorf("%s: %d requests sent", tc.name, n)
		}
		if !bytes.Equal(readFile(t, path), before) {
			t.Errorf("%s: the client config changed", tc.name)
		}
	}
	noSecret(t, "stderr", stderr.String())
}

func TestTCRAERR10_AFailedSavePrintsTheSecretOnStdoutOnlyAndExits1(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	ts, fp := startTCRATLSServer(t)
	home, path := saveConfig(t, &clientconfig.Config{Hosts: map[string]clientconfig.Host{
		"h1": {URL: ts.URL, Fingerprint: fp, Token: "tok"},
	}})
	before := readFile(t, path)
	// The pre-check only reads the configuration; the save then cannot
	// create its temporary file in the directory.
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	r := runSpecsCLI(homeEnv(home), tcraSpecs(), "tcra", "add", "ci", "--host", "h1", "--save-as-host", "newh", "-o", "json")
	if r.code != ExitFailure {
		t.Fatalf("exit %d, want 1: %s", r.code, r.stderr)
	}
	var printed []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &printed); err != nil || len(printed) != 1 {
		t.Fatalf("stdout %q: %v", r.stdout, err)
	}
	if printed[0]["token"] != tcraSecret || printed[0]["tokenId"] != "aaaaaaaaaaaa" {
		t.Errorf("stdout object %v, want the full result with its token", printed[0])
	}
	noSecret(t, "stderr", r.stderr)
	if !strings.Contains(r.stderr, "not saved") || !strings.Contains(r.stderr, "permission denied") {
		t.Errorf("stderr %q does not name the save error", r.stderr)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, path), before) {
		t.Error("the client config changed")
	}
}
