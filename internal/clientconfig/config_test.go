// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package clientconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const pin = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func sample() *Config {
	return &Config{DefaultHost: "a", Hosts: map[string]Host{
		"a": {URL: "https://a.lan:8443", Fingerprint: pin, Token: "tok-a"},
		"b": {URL: "https://b.example:443", Token: "tok-b", Tags: []string{"lab"}},
	}}
}

// TC-154: the XDG path, the HOME fallback, and mode 0600 under umask 000.
func TestConfigLocationAndMode(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	xdg := t.TempDir()
	home := t.TempDir()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"XDG_CONFIG_HOME set", map[string]string{"XDG_CONFIG_HOME": xdg, "HOME": home}, filepath.Join(xdg, "nexus", "config.yaml")},
		{"XDG_CONFIG_HOME unset", map[string]string{"HOME": home}, filepath.Join(home, ".config", "nexus", "config.yaml")},
		{"XDG_CONFIG_HOME relative", map[string]string{"XDG_CONFIG_HOME": "rel/cfg", "HOME": home}, filepath.Join(home, ".config", "nexus", "config.yaml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := Path(envOf(tc.env))
			if err != nil || path != tc.want {
				t.Fatalf("Path = %q, %v; want %q", path, err, tc.want)
			}
			if err := Save(path, sample()); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("config mode %04o, want 0600", fi.Mode().Perm())
			}
			di, err := os.Stat(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			if di.Mode().Perm() != 0o700 {
				t.Errorf("directory mode %04o, want 0700", di.Mode().Perm())
			}
			got, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Hosts["a"].Fingerprint != pin || got.Hosts["b"].Token != "tok-b" || got.DefaultHost != "a" {
				t.Errorf("round trip = %+v", got)
			}
		})
	}
	if _, err := Path(envOf(map[string]string{})); err == nil {
		t.Error("Path with neither XDG_CONFIG_HOME nor HOME succeeded")
	}
}

// Rewriting an existing file that was left 0644 makes it 0600.
func TestSaveTightensExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexus", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("hosts: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o after Save, want 0600", fi.Mode().Perm())
	}
}

// TC-ERR61: a token in a group- or world-readable file is refused, naming
// the file and its mode.
func TestReadableTokenFileRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o602} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		var ime *InsecureModeError
		if !errors.As(err, &ime) {
			t.Fatalf("mode %04o: Load = %v, want *InsecureModeError", mode, err)
		}
		msg := err.Error()
		if !strings.Contains(msg, path) || !strings.Contains(msg, fmt.Sprintf("%04o", mode)) {
			t.Errorf("mode %04o: message %q does not name the file and mode", mode, msg)
		}
		if strings.Contains(msg, "tok-a") || strings.Contains(msg, "tok-b") {
			t.Errorf("message leaks a token: %q", msg)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("mode 0600: %v", err)
	}

	// A readable file without tokens is not a secret and loads.
	noTok := filepath.Join(dir, "notoken.yaml")
	if err := os.WriteFile(noTok, []byte("hosts:\n  a:\n    url: https://a.lan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(noTok); err != nil {
		t.Errorf("token-free 0644 file: %v", err)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown key":      "hosts: {}\nbogus: 1\n",
		"plain http lan":   "hosts:\n  a:\n    url: http://192.168.1.2:8080\n",
		"bad fingerprint":  "hosts:\n  a:\n    url: https://a.lan\n    fingerprint: nope\n",
		"bad name":         "hosts:\n  A B:\n    url: https://a.lan\n",
		"dangling default": "defaultHost: z\nhosts:\n  a:\n    url: https://a.lan\n",
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load accepted it", name)
		}
	}
	if c, err := Load(filepath.Join(dir, "missing.yaml")); err != nil || len(c.Hosts) != 0 {
		t.Errorf("missing file: %+v, %v; want an empty config", c, err)
	}
}

func TestValidateHostName(t *testing.T) {
	for _, ok := range []string{"a", "nas", "nas-1.lan", "x_y"} {
		if err := ValidateHostName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "all", "A", "a,b", "-a", "a b", strings.Repeat("a", 64), "https://x"} {
		if err := ValidateHostName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TC-155: --host, NEXUS_HOST as a name and as a URL, NEXUS_TOKEN and
// NEXUS_FINGERPRINT.
func TestResolveOverrides(t *testing.T) {
	loads := 0
	load := func() (*Config, error) { loads++; return sample(), nil }
	refuse := func() (*Config, error) { return nil, &InsecureModeError{Path: "/x", Mode: 0o644} }

	for _, tc := range []struct {
		name     string
		flag     string
		env      map[string]string
		load     func() (*Config, error)
		want     Target
		wantErr  bool
		wantLoad bool
	}{
		{name: "default host", load: load, wantLoad: true,
			want: Target{Name: "a", URL: "https://a.lan:8443", Fingerprint: pin, Token: "tok-a"}},
		{name: "NEXUS_HOST name", env: map[string]string{EnvHost: "b"}, load: load, wantLoad: true,
			want: Target{Name: "b", URL: "https://b.example:443", Token: "tok-b"}},
		{name: "flag beats NEXUS_HOST", flag: "a", env: map[string]string{EnvHost: "b"}, load: load, wantLoad: true,
			want: Target{Name: "a", URL: "https://a.lan:8443", Fingerprint: pin, Token: "tok-a"}},
		{name: "NEXUS_TOKEN overrides", env: map[string]string{EnvHost: "a", EnvToken: "env-tok"}, load: load, wantLoad: true,
			want: Target{Name: "a", URL: "https://a.lan:8443", Fingerprint: pin, Token: "env-tok"}},
		{name: "NEXUS_FINGERPRINT ignored for a name", env: map[string]string{EnvHost: "b", EnvFingerprint: pin}, load: load, wantLoad: true,
			want: Target{Name: "b", URL: "https://b.example:443", Token: "tok-b"}},
		{name: "URL with fingerprint, config unread", env: map[string]string{EnvHost: "https://10.0.0.5:8443", EnvToken: "env-tok", EnvFingerprint: strings.ToLower(strings.ReplaceAll(pin, ":", ""))},
			load: refuse, want: Target{Name: "10.0.0.5", URL: "https://10.0.0.5:8443", Fingerprint: pin, Token: "env-tok"}},
		{name: "URL without fingerprint is system trust", env: map[string]string{EnvHost: "https://10.0.0.5:8443"},
			load: refuse, want: Target{Name: "10.0.0.5", URL: "https://10.0.0.5:8443"}},
		{name: "URL plain http on LAN", env: map[string]string{EnvHost: "http://10.0.0.5:8080"}, load: refuse, wantErr: true},
		{name: "URL bad fingerprint", env: map[string]string{EnvHost: "https://10.0.0.5", EnvFingerprint: "xyz"}, load: refuse, wantErr: true},
		{name: "unknown name", env: map[string]string{EnvHost: "zz"}, load: load, wantErr: true, wantLoad: true},
		{name: "insecure file refused", env: map[string]string{EnvHost: "a", EnvToken: "env-tok"}, load: refuse, wantErr: true},
		{name: "no default", load: func() (*Config, error) { return &Config{}, nil }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loads = 0
			got, err := Resolve(tc.flag, envOf(tc.env), tc.load)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Resolve = %+v, want %+v", got, tc.want)
			}
			if tc.wantLoad != (loads > 0) {
				t.Errorf("config loaded %d times, want loaded=%v", loads, tc.wantLoad)
			}
		})
	}
	if _, err := Resolve("", envOf(nil), func() (*Config, error) { return &Config{}, nil }); !errors.Is(err, ErrNoHost) {
		t.Errorf("no host: %v, want ErrNoHost", err)
	}
}
