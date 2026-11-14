package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/release"
)

const defaultConfigPath = "../../deploy/nexusd.default.yaml"

// TC-10: the shipped default config loads through the production loader with
// every provider disabled and the documented defaults.
func TestDefaultConfigLoads(t *testing.T) {
	cfg, err := Load(defaultConfigPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", defaultConfigPath, err)
	}

	if cfg.Providers.Docker.Settings.Enabled || cfg.Providers.Apt.Settings.Enabled || cfg.Providers.Scripts.Settings.Enabled ||
		cfg.Providers.Nexusd.Settings.Enabled {
		t.Errorf("providers enabled: docker=%v apt=%v scripts=%v nexusd=%v, want all false",
			cfg.Providers.Docker.Settings.Enabled, cfg.Providers.Apt.Settings.Enabled, cfg.Providers.Scripts.Settings.Enabled,
			cfg.Providers.Nexusd.Settings.Enabled)
	}
	if cfg.Providers.Pihole.Settings.Enabled {
		t.Error("providers.pihole.enabled = true, want false")
	}
	if want := []string{"127.0.0.1:7443"}; !reflect.DeepEqual(cfg.Listen, want) {
		t.Errorf("listen = %q, want %q", cfg.Listen, want)
	}
	if cfg.TLS.Mode != TLSAuto {
		t.Errorf("tls.mode = %q, want %q", cfg.TLS.Mode, TLSAuto)
	}
	if cfg.TLS.CertFile != "" || cfg.TLS.KeyFile != "" {
		t.Errorf("tls.certFile = %q, tls.keyFile = %q, want both unset", cfg.TLS.CertFile, cfg.TLS.KeyFile)
	}
	if len(cfg.TLS.ExtraSANs) != 0 {
		t.Errorf("tls.extraSANs = %q, want empty", cfg.TLS.ExtraSANs)
	}
	if cfg.Limits.MaxBodyBytes != 1048576 {
		t.Errorf("limits.maxBodyBytes = %d, want 1048576", cfg.Limits.MaxBodyBytes)
	}
	if cfg.Limits.OutputCapBytes != 10485760 {
		t.Errorf("limits.outputCapBytes = %d, want 10485760", cfg.Limits.OutputCapBytes)
	}
	if cfg.Auth.FailureLimitPerMinute != 10 {
		t.Errorf("auth.failureLimitPerMinute = %d, want 10", cfg.Auth.FailureLimitPerMinute)
	}
	if got := cfg.Providers.Docker.Settings.UpdateHealthTimeout; got != 60*time.Second {
		t.Errorf("providers.docker.updateHealthTimeout = %v, want 60s", got)
	}
	if got := cfg.Providers.Docker.Settings.UpdateMaxRuntime; got != time.Hour {
		t.Errorf("providers.docker.updateMaxRuntime = %v, want 1h", got)
	}
	if cfg.Host.Name == "" {
		t.Error("host.name is empty; want the OS hostname when the file leaves it empty")
	}
}

// The default file must spell out every key the loader knows, with exactly
// the built-in default, so the shipped file and Default() cannot drift.
func TestDefaultConfigMatchesDefaultsAndHasEveryKey(t *testing.T) {
	data, err := os.ReadFile(defaultConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data, defaultConfigPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Default()
	want.Host.Name = cfg.Host.Name // resolved from the OS hostname
	assertSameSettings(t, cfg, &want)

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, path := range structKeyPaths(reflect.TypeOf(Config{}), "") {
		if strings.Contains(path, "catalog.") {
			continue // the default catalog is empty, so entry keys cannot appear
		}
		if keyNode(doc.Content[0], path) == nil {
			t.Errorf("deploy/nexusd.default.yaml is missing key %s", path)
		}
	}
}

// TC-ERR3: malformed values each fail with a located error and never panic.
func TestMalformedConfiguration(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name     string
		content  *string // nil: the file does not exist
		wantPath string
		wantLine int
		wantCol  int
		anyLine  bool // the parser chooses the line; only require one
	}{
		{name: "invalid yaml syntax", content: ptr("listen: [\"127.0.0.1:7443\"\ntls: {mode: auto}\n"), anyLine: true},
		{name: "bogus tls mode", content: ptr("tls:\n  mode: bogus\n"), wantPath: "tls.mode", wantLine: 2, wantCol: 9},
		{name: "unparsable duration", content: ptr("providers:\n  apt:\n    lockWait: soon\n"), wantPath: "providers.apt.lockWait", wantLine: 3, wantCol: 15},
		{name: "negative body limit", content: ptr("limits: {maxBodyBytes: -1}\n"), wantPath: "limits.maxBodyBytes", wantLine: 1, wantCol: 24},
		{name: "missing file", content: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".yaml")
			if tc.content != nil {
				if err := os.WriteFile(path, []byte(*tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(path)
			if err == nil {
				t.Fatalf("Load succeeded with %+v, want an error", cfg)
			}
			var list ErrorList
			if !errors.As(err, &list) || len(list) == 0 {
				t.Fatalf("error %v (%T) is not a non-empty ErrorList", err, err)
			}
			e := list[0]
			if e.File != path {
				t.Errorf("error file = %q, want %q", e.File, path)
			}
			if !strings.HasPrefix(err.Error(), path) {
				t.Errorf("message %q does not start with the file path", err.Error())
			}
			if tc.wantPath != "" && e.Path != tc.wantPath {
				t.Errorf("error path = %q, want %q (%v)", e.Path, tc.wantPath, err)
			}
			if tc.anyLine {
				if e.Line <= 0 {
					t.Errorf("syntax error has no line: %v", err)
				}
			} else if e.Line != tc.wantLine || e.Column != tc.wantCol {
				t.Errorf("error at %d:%d, want %d:%d (%v)", e.Line, e.Column, tc.wantLine, tc.wantCol, err)
			}
		})
	}

	t.Run("empty file falls back to defaults", func(t *testing.T) {
		path := filepath.Join(dir, "empty.yaml")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(empty): %v", err)
		}
		want := Default()
		want.Host.Name = cfg.Host.Name
		assertSameSettings(t, cfg, &want)
		// Same exposure as the default config: loopback-only, TLS on.
		if !reflect.DeepEqual(cfg.Listen, []string{"127.0.0.1:7443"}) || cfg.TLS.Mode != TLSAuto {
			t.Errorf("empty config exposure: listen=%q tls.mode=%q", cfg.Listen, cfg.TLS.Mode)
		}
	})
}

// Rules from the Configuration section of the architecture that validate-config
// enforces, each located at the offending value.
func TestValidationRules(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantPath string
		wantMsg  string
	}{
		{"files mode needs cert", "tls: {mode: files}\n", "tls.certFile", "required"},
		{"cert rejected outside files mode", "tls: {mode: auto, certFile: /etc/c.pem}\n", "tls.certFile", "only allowed"},
		{"empty listen", "listen: []\n", "listen", "at least one"},
		{"bad listen address", "listen: [\"nonsense\"]\n", "listen[0]", "host:port"},
		{"zero failure limit", "auth: {failureLimitPerMinute: 0}\n", "auth.failureLimitPerMinute", "greater than zero"},
		{"retention days that would overflow a duration", "audit: {retentionDays: 999999}\n", "audit.retentionDays", "at most 36500"},
		{"retention days just above the bound", "audit: {retentionDays: 36501}\n", "audit.retentionDays", "at most 36500"},
		{"relative docker socket", "providers: {docker: {socket: docker.sock}}\n", "providers.docker.socket", "absolute"},
		{"string parameter without pattern", scriptsYAML("{name: branch, type: string, arg: --branch}"), "providers.scripts.catalog[0].parameters[0].pattern", "required"},
		{"sensitive without env", scriptsYAML("{name: k, type: string, pattern: '^a$', sensitive: true, arg: --k}"), "providers.scripts.catalog[0].parameters[0].env", "sensitive"},
		{"pattern does not compile", scriptsYAML("{name: k, type: string, pattern: '(', arg: --k}"), "providers.scripts.catalog[0].parameters[0].pattern", "regular expression"},
		// Valid only once wrapped for anchoring: the bare compile refuses it.
		{"pattern that only compiles when wrapped", scriptsYAML("{name: k, type: string, pattern: 'a)(b', arg: --k}"), "providers.scripts.catalog[0].parameters[0].pattern", "regular expression"},
		// A misspelled or missing type would otherwise skip the string rule.
		{"misspelled parameter type", scriptsYAML("{name: branch, type: strnig, arg: --branch}"), "providers.scripts.catalog[0].parameters[0].type", "must be one of string, integer or boolean"},
		{"missing parameter type", scriptsYAML("{name: branch, arg: --branch}"), "providers.scripts.catalog[0].parameters[0].type", "required"},
		{"enum value of the wrong type", scriptsYAML("{name: n, type: integer, enum: ['1', two], arg: --n}"), "providers.scripts.catalog[0].parameters[0].enum[1]", "integer"},
		{"enum value failing the pattern", scriptsYAML("{name: e, type: string, pattern: '^[a-z]+$', enum: [prod, Dev], arg: --e}"), "providers.scripts.catalog[0].parameters[0].enum[1]", "pattern"},
		// SEC-3: the pattern decides the whole value, so "Dev" does not pass a
		// pattern that only finds "ev" in it, which the scripts provider
		// would refuse at run time.
		{"enum value matching a pattern only in part", scriptsYAML("{name: e, type: string, pattern: '[a-z]+', enum: [prod, Dev], arg: --e}"), "providers.scripts.catalog[0].parameters[0].enum[1]", "pattern"},
		{"duplicate enum value", scriptsYAML("{name: e, type: string, enum: [prod, prod], arg: --e}"), "providers.scripts.catalog[0].parameters[0].enum[1]", "duplicate"},
		// SEC-1 relaxation: every trusted Compose owner must exist on the host.
		{"unknown trusted Compose owner", "providers:\n  docker:\n    compose:\n      trustedOwners: [root, nx-no-such-user-182]\n",
			"providers.docker.compose.trustedOwners[1]", `unknown user "nx-no-such-user-182"`},
		{"empty pihole dnsFile", "providers: {pihole: {dnsFile: \"\"}}\n", "providers.pihole.dnsFile", "absolute"},
		// dnsmasq skips a lone "#": its first and last characters are both "#".
		{"pihole dnsFile named #", "providers: {pihole: {dnsFile: \"/etc/dnsmasq.d/#\"}}\n", "providers.pihole.dnsFile", "dnsmasq skips"},
		{"empty trusted Compose owner", "providers: {docker: {compose: {trustedOwners: [\"\"]}}}\n",
			"providers.docker.compose.trustedOwners[0]", "must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml), "c.yaml")
			var list ErrorList
			if !errors.As(err, &list) {
				t.Fatalf("Parse error = %v, want an ErrorList", err)
			}
			for _, e := range list {
				if e.Path == tc.wantPath && strings.Contains(e.Msg, tc.wantMsg) {
					if e.Line == 0 {
						t.Errorf("error %v has no line", e)
					}
					return
				}
			}
			t.Errorf("no error at %s containing %q in:\n%v", tc.wantPath, tc.wantMsg, err)
		})
	}

	t.Run("trusted Compose owners that exist pass, root included", func(t *testing.T) {
		u, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		y := fmt.Sprintf("providers: {docker: {compose: {trustedOwners: [root, %s]}}}\n", u.Username)
		cfg, err := Parse([]byte(y), "c.yaml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := cfg.Providers.Docker.Settings.Compose.TrustedOwners; len(got) != 2 || got[1] != u.Username {
			t.Fatalf("trustedOwners %q", got)
		}
	})

	t.Run("valid catalog passes", func(t *testing.T) {
		y := scriptsYAML("{name: branch, type: string, pattern: '^[a-z]+$', arg: --branch}", "{name: apiKey, type: string, pattern: '^[A-Za-z0-9]+$', sensitive: true, env: API_KEY}",
			"{name: stage, type: string, enum: [prod, staging]}", "{name: count, type: integer, arg: --count}", "{name: dry, type: boolean, env: DRY}")
		if _, err := Parse([]byte(y), "c.yaml"); err != nil {
			t.Fatalf("Parse: %v", err)
		}
	})
}

// NFR-4, A-67: running as root, the file must be root-owned with no group or
// other bit. The check is driven with an injected euid and file info because
// tests do not run as root.
func TestOwnershipCheck(t *testing.T) {
	cases := []struct {
		name    string
		euid    int
		uid     uint32
		mode    fs.FileMode
		wantErr string
	}{
		{"root, root-owned 0600", 0, 0, 0o600, ""},
		{"root, root-owned 0400", 0, 0, 0o400, ""},
		{"root, group readable", 0, 0, 0o640, "chmod 0600 /etc/nexus/nexusd.yaml"},
		{"root, world readable", 0, 0, 0o644, "chmod 0600 /etc/nexus/nexusd.yaml"},
		{"root, other execute only", 0, 0, 0o601, "chmod 0600 /etc/nexus/nexusd.yaml"},
		{"root, not root-owned", 0, 1000, 0o600, "owned by uid 1000"},
		{"not root, world readable", 1000, 1000, 0o644, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fi := fakeInfo{mode: tc.mode, sys: &syscall.Stat_t{Uid: tc.uid}}
			err := checkFile("/etc/nexus/nexusd.yaml", fi, tc.euid)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkFile: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkFile error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}

	t.Run("not a regular file", func(t *testing.T) {
		fi := fakeInfo{mode: fs.ModeDir | 0o700, sys: &syscall.Stat_t{}}
		if err := checkFile("/etc/nexus", fi, 0); err == nil {
			t.Fatal("checkFile accepted a directory")
		}
	})
}

// Section.Decode is the strict closure handed to Provider.Configure.
func TestProviderSectionDecode(t *testing.T) {
	cfg, err := Parse([]byte("providers:\n  docker:\n    enabled: true\n    socket: /var/run/docker.sock\n"), "c.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got := Default().Providers.Docker.Settings
	if err := cfg.Providers.Docker.Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.Enabled || got.Socket != "/var/run/docker.sock" || got.UpdateMaxRuntime != time.Hour {
		t.Errorf("decoded %+v", got)
	}

	var narrow struct {
		Enabled bool `yaml:"enabled"`
	}
	err = cfg.Providers.Docker.Decode(&narrow)
	var list ErrorList
	if !errors.As(err, &list) || len(list) != 1 || list[0].Path != "providers.docker.socket" || list[0].Line != 4 || list[0].Column != 5 {
		t.Fatalf("narrow Decode error = %v, want one unknown key at providers.docker.socket 4:5", err)
	}

	empty, err := Parse(nil, "c.yaml")
	if err != nil {
		t.Fatal(err)
	}
	keep := DockerConfig{Socket: "/x"}
	if err := empty.Providers.Docker.Decode(&keep); err != nil || keep.Socket != "/x" {
		t.Fatalf("Decode of an absent section = %v, %+v; want no error and the value untouched", err, keep)
	}
}

func scriptsYAML(params ...string) string {
	var b strings.Builder
	b.WriteString("providers:\n  scripts:\n    catalog:\n      - id: deploy\n        description: Deploy\n        path: /opt/deploy.sh\n" +
		"        runAs: deploy\n        workingDirectory: /opt\n        parameters:\n")
	for _, p := range params {
		b.WriteString("          - " + p + "\n")
	}
	return b.String()
}

func ptr(s string) *string { return &s }

func assertSameSettings(t *testing.T, got, want *Config) {
	t.Helper()
	pairs := []struct {
		name      string
		got, want any
	}{
		{"host", got.Host, want.Host},
		{"listen", got.Listen, want.Listen},
		{"tls", got.TLS, want.TLS},
		{"limits", got.Limits, want.Limits},
		{"auth", got.Auth, want.Auth},
		{"audit", got.Audit, want.Audit},
		{"mcp", got.MCP, want.MCP},
		{"providers.docker", got.Providers.Docker.Settings, want.Providers.Docker.Settings},
		{"providers.apt", got.Providers.Apt.Settings, want.Providers.Apt.Settings},
		{"providers.scripts", got.Providers.Scripts.Settings, want.Providers.Scripts.Settings},
		{"providers.nexusd", got.Providers.Nexusd.Settings, want.Providers.Nexusd.Settings},
		{"providers.pihole", got.Providers.Pihole.Settings, want.Providers.Pihole.Settings},
	}
	for _, p := range pairs {
		if !reflect.DeepEqual(p.got, p.want) {
			t.Errorf("%s = %+v, want %+v", p.name, p.got, p.want)
		}
	}
}

// structKeyPaths lists the dotted yaml key paths of every leaf and section.
func structKeyPaths(typ reflect.Type, prefix string) []string {
	if s, ok := sectionSettingsType(typ); ok {
		typ = s
	}
	var paths []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		name := yamlName(f)
		if name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		paths = append(paths, path)
		ft := f.Type
		if ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct {
			paths = append(paths, structKeyPaths(ft.Elem(), path)...)
		} else if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Duration(0)) {
			paths = append(paths, structKeyPaths(ft, path)...)
		}
	}
	return paths
}

// sectionSettingsType unwraps Section[T] to T.
func sectionSettingsType(typ reflect.Type) (reflect.Type, bool) {
	if typ.Kind() != reflect.Struct {
		return nil, false
	}
	if _, ok := reflect.New(typ).Interface().(sectionTarget); !ok {
		return nil, false
	}
	f, _ := typ.FieldByName("Settings")
	return f.Type, true
}

// keyNode returns the key node at a dotted path (no indexes), or nil.
func keyNode(n *yaml.Node, path string) *yaml.Node {
	var key *yaml.Node
	for _, part := range strings.Split(path, ".") {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == part {
				key, next = n.Content[i], n.Content[i+1]
				break
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return key
}

type fakeInfo struct {
	mode fs.FileMode
	sys  any
}

func (f fakeInfo) Name() string       { return "nexusd.yaml" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return f.sys }

// CR-4: the largest accepted audit.retentionDays is 36500.
func TestRetentionDaysAtTheBoundIsValid(t *testing.T) {
	cfg, err := Parse([]byte("audit: {retentionDays: 36500}\n"), "c.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Audit.RetentionDays != MaxAuditRetentionDays {
		t.Fatalf("retentionDays = %d", cfg.Audit.RetentionDays)
	}
}

// TC-SM-41 (FR-19, FR-1, FR-10, FR-6): providers.nexusd defaults to disabled
// with the spec's health timeout and upload cap and no trusted key, and the
// shipped default file spells out the same section. The every-key check in
// TestDefaultConfigMatchesDefaultsAndHasEveryKey covers its four keys.
func TestTCSM41_ProvidersNexusdDefaultsAndTheDeploy(t *testing.T) {
	want := NexusdConfig{
		Enabled:       false,
		HealthTimeout: 30 * time.Second,
		Update:        NexusdUpdateConfig{MaxUploadBytes: 67108864, TrustedKeys: []string{}},
	}
	if got := Default().Providers.Nexusd.Settings; !reflect.DeepEqual(got, want) {
		t.Errorf("Default().Providers.Nexusd.Settings = %+v, want %+v", got, want)
	}

	cfg, err := Load(defaultConfigPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", defaultConfigPath, err)
	}
	if got := cfg.Providers.Nexusd.Settings; !reflect.DeepEqual(got, want) {
		t.Errorf("deploy default providers.nexusd = %+v, want %+v", got, want)
	}

	data, err := os.ReadFile(defaultConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	section := valueNode(doc.Content[0], "providers.nexusd")
	if section == nil {
		t.Fatal("deploy/nexusd.default.yaml has no providers.nexusd section")
	}
	if n := valueNode(section, "enabled"); n == nil || n.Value != "false" {
		t.Errorf("providers.nexusd.enabled = %v, want false", n)
	}
	if n := valueNode(section, "update.trustedKeys"); n == nil || n.Kind != yaml.SequenceNode || len(n.Content) != 0 {
		t.Errorf("providers.nexusd.update.trustedKeys = %v, want []", n)
	}
	comments := keyNode(doc.Content[0], "providers.nexusd").HeadComment + "\n" + allComments(section)
	if !strings.Contains(comments, "make release-key") {
		t.Errorf("no providers.nexusd comment mentions make release-key:\n%s", comments)
	}
}

// TC-SM-42 (FR-15, FR-10, FR-6): the bounds themselves are valid, and each
// trusted key loads as written and parses to the key it encodes.
func TestTCSM42_BoundValuesAreAcceptedAndA(t *testing.T) {
	k1, k2 := newPublicKey(t), newPublicKey(t)
	cases := []struct {
		name string
		yaml string
		want NexusdConfig
	}{
		{"healthTimeout 5s", "providers:\n  nexusd:\n    healthTimeout: 5s\n",
			NexusdConfig{HealthTimeout: 5 * time.Second, Update: NexusdUpdateConfig{MaxUploadBytes: 64 << 20, TrustedKeys: []string{}}}},
		{"healthTimeout 10m", "providers:\n  nexusd:\n    healthTimeout: 10m\n",
			NexusdConfig{HealthTimeout: 10 * time.Minute, Update: NexusdUpdateConfig{MaxUploadBytes: 64 << 20, TrustedKeys: []string{}}}},
		{"maxUploadBytes 1 MiB", "providers:\n  nexusd:\n    update:\n      maxUploadBytes: 1048576\n",
			NexusdConfig{HealthTimeout: 30 * time.Second, Update: NexusdUpdateConfig{MaxUploadBytes: 1048576, TrustedKeys: []string{}}}},
		{"maxUploadBytes 256 MiB", "providers:\n  nexusd:\n    update:\n      maxUploadBytes: 268435456\n",
			NexusdConfig{HealthTimeout: 30 * time.Second, Update: NexusdUpdateConfig{MaxUploadBytes: 268435456, TrustedKeys: []string{}}}},
		{"no trusted key", "providers:\n  nexusd:\n    enabled: true\n    update:\n      trustedKeys: []\n",
			NexusdConfig{Enabled: true, HealthTimeout: 30 * time.Second, Update: NexusdUpdateConfig{MaxUploadBytes: 64 << 20, TrustedKeys: []string{}}}},
		{"one trusted key", "providers:\n  nexusd:\n    update:\n      trustedKeys: [\"" + k1 + "\"]\n",
			NexusdConfig{HealthTimeout: 30 * time.Second, Update: NexusdUpdateConfig{MaxUploadBytes: 64 << 20, TrustedKeys: []string{k1}}}},
		{"two distinct trusted keys", "providers:\n  nexusd:\n    update:\n      trustedKeys:\n        - " + k1 + "\n        - \"" + k2 + "\"\n",
			NexusdConfig{HealthTimeout: 30 * time.Second, Update: NexusdUpdateConfig{MaxUploadBytes: 64 << 20, TrustedKeys: []string{k1, k2}}}},
	}
	dir := t.TempDir()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("bound-%d.yaml", i))
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.Providers.Nexusd.Settings
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("providers.nexusd = %+v, want %+v", got, tc.want)
			}
			for j, s := range got.Update.TrustedKeys {
				pub, err := release.ParsePublicKey(s)
				if err != nil {
					t.Fatalf("trustedKeys[%d] loaded but release.ParsePublicKey refuses it: %v", j, err)
				}
				if release.EncodePublicKey(pub) != tc.want.Update.TrustedKeys[j] {
					t.Errorf("trustedKeys[%d] does not round-trip", j)
				}
			}
		})
	}
}

// TC-SM-ERR23 (FR-15, FR-10, FR-6, FR-3): each out-of-bound value, malformed
// or duplicate key and unknown key is one error located at file, line and
// column, naming the bound or index and never the key value.
func TestTCSMERR23_ConfigValidationErrorsAreLocatedAnd(t *testing.T) {
	k := newPublicKey(t)
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xa7}, 31))
	secrets := []string{"@@@", k, short, string(bytes.Repeat([]byte{0xa7}, 31))}
	keys := func(list string) string {
		return "providers:\n  nexusd:\n    update:\n      trustedKeys: " + list + "\n"
	}
	cases := []struct {
		name      string
		yaml      string
		wantPath  string
		wantLine  int
		wantCol   int
		wantNamed string // the bound or index the error names
	}{
		{"healthTimeout below 5s", "providers:\n  nexusd:\n    healthTimeout: 4s\n", "providers.nexusd.healthTimeout", 3, 20, "5s"},
		{"healthTimeout above 10m", "providers:\n  nexusd:\n    healthTimeout: 11m\n", "providers.nexusd.healthTimeout", 3, 20, "10m"},
		{"maxUploadBytes below 1 MiB", "providers:\n  nexusd:\n    update:\n      maxUploadBytes: 1048575\n",
			"providers.nexusd.update.maxUploadBytes", 4, 23, "1048576"},
		{"maxUploadBytes above 256 MiB", "providers:\n  nexusd:\n    update:\n      maxUploadBytes: 268435457\n",
			"providers.nexusd.update.maxUploadBytes", 4, 23, "268435456"},
		{"key that is not base64", keys(`["@@@"]`), "providers.nexusd.update.trustedKeys[0]", 4, 20, "trustedKeys[0]"},
		{"key of 31 bytes", keys(`["` + short + `"]`), "providers.nexusd.update.trustedKeys[0]", 4, 20, "trustedKeys[0]"},
		{"duplicate key", keys(`["` + k + `", "` + k + `"]`), "providers.nexusd.update.trustedKeys[1]", 4, 20, "trustedKeys[0]"},
		{"unknown key", "providers:\n  nexusd:\n    bogus: 1\n", "providers.nexusd.bogus", 3, 5, "bogus"},
	}
	dir := t.TempDir()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("err-%d.yaml", i))
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err == nil {
				t.Fatalf("Load succeeded with %+v, want an error", cfg.Providers.Nexusd.Settings)
			}
			var list ErrorList
			if !errors.As(err, &list) || len(list) != 1 {
				t.Fatalf("Load error = %v, want exactly one config.Error", err)
			}
			e := list[0]
			if e.File != path || e.Line != tc.wantLine || e.Column != tc.wantCol || e.Path != tc.wantPath {
				t.Errorf("error at %s:%d:%d path %q, want %s:%d:%d path %q", e.File, e.Line, e.Column, e.Path,
					path, tc.wantLine, tc.wantCol, tc.wantPath)
			}
			if !strings.Contains(e.Error(), tc.wantNamed) {
				t.Errorf("error %q does not name %q", e.Error(), tc.wantNamed)
			}
			for _, s := range secrets {
				if strings.Contains(e.Msg, s) || strings.Contains(e.Error(), s) {
					t.Errorf("error %q echoes key material %q", e.Error(), s)
				}
			}
		})
	}
}

func newPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return release.EncodePublicKey(pub)
}

// valueNode returns the value node at a dotted path (no indexes), or nil.
func valueNode(n *yaml.Node, path string) *yaml.Node {
	for _, part := range strings.Split(path, ".") {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == part {
				next = n.Content[i+1]
				break
			}
		}
		n = next
	}
	return n
}

// allComments joins every comment on n and the nodes below it.
func allComments(n *yaml.Node) string {
	s := n.HeadComment + "\n" + n.LineComment + "\n" + n.FootComment
	for _, c := range n.Content {
		s += "\n" + allComments(c)
	}
	return s
}
