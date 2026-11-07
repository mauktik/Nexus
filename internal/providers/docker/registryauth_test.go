package docker

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/registry"
)

func decodeHeader(t *testing.T, s string) registry.AuthConfig {
	t.Helper()
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("X-Registry-Auth is not base64url: %v", err)
	}
	var ac registry.AuthConfig
	if err := json.Unmarshal(b, &ac); err != nil {
		t.Fatal(err)
	}
	return ac
}

// A-14, A-61: the auths entry for exactly this registry, in any of the key
// forms the Docker CLI writes; empty entries (written with a credential
// store) are no credential; the helper is credHelpers' entry, else
// credsStore.
func TestRegistryConfigLookup(t *testing.T) {
	dir := writeConfig(t, `{
		"auths": {
			"https://index.docker.io/v1/": {"auth": "`+basicAuth("hub", "p1")+`"},
			"http://reg.example:5000/v2/": {"auth": "`+basicAuth("reg", "p2")+`"},
			"tok.example": {"identitytoken": "tok-123"},
			"store.example": {},
			"REG.UPPER": {"username": "up", "password": "p3"}
		},
		"credHelpers": {"helped.example": "ecr-login", "https://index.docker.io/v1/": "hubhelper"},
		"credsStore": "desktop",
		"proxies": {"default": {"httpProxy": "http://ignored"}}
	}`)
	cfg, err := loadRegistryConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		domain, user, pass, token, server, helper, source string
	}{
		{domain: "docker.io", user: "hub", pass: "p1", server: hubAuthKey, helper: "docker-credential-hubhelper", source: "credHelpers"},
		{domain: "reg.example:5000", user: "reg", pass: "p2", server: "http://reg.example:5000/v2/", helper: "docker-credential-desktop", source: "credsStore"},
		{domain: "reg.example:5001", helper: "docker-credential-desktop", source: "credsStore"},
		{domain: "reg.example", helper: "docker-credential-desktop", source: "credsStore"},
		{domain: "tok.example", token: "tok-123", server: "tok.example", helper: "docker-credential-desktop", source: "credsStore"},
		{domain: "store.example", helper: "docker-credential-desktop", source: "credsStore"},
		{domain: "helped.example", helper: "docker-credential-ecr-login", source: "credHelpers"},
		{domain: "reg.upper", user: "up", pass: "p3", server: "REG.UPPER", helper: "docker-credential-desktop", source: "credsStore"},
	} {
		c, err := cfg.forRegistry(tc.domain)
		if err != nil {
			t.Fatalf("%s: %v", tc.domain, err)
		}
		if c.helper != tc.helper || c.helperSource != tc.source {
			t.Errorf("%s: helper %q from %q, want %q from %q", tc.domain, c.helper, c.helperSource, tc.helper, tc.source)
		}
		if tc.server == "" {
			if c.encoded != "" || c.key != "" {
				t.Errorf("%s: a credential was chosen from %q", tc.domain, c.key)
			}
			continue
		}
		ac := decodeHeader(t, c.encoded)
		if ac.Username != tc.user || ac.Password != tc.pass || ac.IdentityToken != tc.token || ac.ServerAddress != tc.server {
			t.Errorf("%s: %+v", tc.domain, ac)
		}
	}
}

// A missing file or directory is anonymous access; anything unreadable is
// an error that never quotes the file's content.
func TestRegistryConfigErrors(t *testing.T) {
	cfg, err := loadRegistryConfig(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	if c, err := cfg.forRegistry("docker.io"); err != nil || c.encoded != "" || c.helper != "" {
		t.Fatalf("missing dir lookup: %+v %v", c, err)
	}
	for _, tc := range []struct{ name, js, want string }{
		{"syntax", `{"auths": {"x": {"auth": "c2VjcmV0LXZhbHVl"`, "not valid JSON"},
		{"shape", `{"auths": ["c2VjcmV0LXZhbHVl"]}`, "config.json shape"},
	} {
		_, err := loadRegistryConfig(writeConfig(t, tc.js))
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "c2VjcmV0LXZhbHVl") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "config.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRegistryConfig(dir); err == nil {
		t.Error("a directory named config.json was accepted")
	}
	for _, bad := range []string{"%%%", base64.StdEncoding.EncodeToString([]byte("no-colon-secret")),
		base64.StdEncoding.EncodeToString([]byte(":onlypass"))} {
		cfg, err := loadRegistryConfig(writeConfig(t, `{"auths":{"r.example":{"auth":"`+bad+`"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = cfg.forRegistry("r.example")
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "onlypass") {
			t.Errorf("auth %q: %v", bad, err)
		}
	}
}

func TestHostnameOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://index.docker.io/v1/": "index.docker.io",
		"http://reg.example:5000/v2/": "reg.example:5000",
		"reg.example:5000":            "reg.example:5000",
		"reg.example/path":            "reg.example",
		"[::1]:5000":                  "[::1]:5000",
	} {
		if got := hostnameOf(in); got != want {
			t.Errorf("hostnameOf(%q) = %q, want %q", in, got, want)
		}
	}
}
