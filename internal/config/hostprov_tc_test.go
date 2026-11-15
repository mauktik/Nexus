// Tests of planning/system-provider/test-cases.md TC-HP-15, TC-HP-E4,
// TC-HP-ERR4 and TC-HP-ERR5, owned by WI 1.3: the providers.host section.
// The daemon.CheckConfig half of TC-HP-ERR4 is in host_config_test.go,
// which is package config_test because package daemon imports this one.

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-HP-15: providers.host defaults, YAML and comments agree. Requirements:
// FR-1, FR-11, FR-13. The every-key check in
// TestDefaultConfigMatchesDefaultsAndHasEveryKey also covers its four keys.
func TestTCHP15_ProvidersHostDefaultsYAMLAndCommentsAgree(t *testing.T) {
	want := HostProviderConfig{
		Enabled:         false,
		SampleInterval:  10 * time.Second,
		HistoryDuration: time.Hour,
		SourceTimeout:   2 * time.Second,
	}
	if want.SampleInterval != hostspec.DefaultSampleInterval || want.HistoryDuration != hostspec.DefaultHistoryDuration ||
		want.SourceTimeout != hostspec.DefaultSourceTimeout {
		t.Errorf("spec defaults %s, %s, %s differ from the expected %+v", hostspec.DefaultSampleInterval,
			hostspec.DefaultHistoryDuration, hostspec.DefaultSourceTimeout, want)
	}
	if got := Default().Providers.Host.Settings; got != want {
		t.Errorf("Default().Providers.Host.Settings = %+v, want %+v", got, want)
	}

	cfg, err := Load(defaultConfigPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", defaultConfigPath, err)
	}
	if got := cfg.Providers.Host.Settings; got != want {
		t.Errorf("deploy default providers.host = %+v, want %+v", got, want)
	}

	data, err := os.ReadFile(defaultConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	key := keyNode(doc.Content[0], "providers.host")
	if key == nil {
		t.Fatal("deploy/nexusd.default.yaml has no providers.host section")
	}
	c := key.HeadComment
	if !strings.Contains(c, "changes nothing") && !strings.Contains(c, "read-only") {
		t.Errorf("providers.host comment does not say it changes nothing (or is read-only):\n%s", c)
	}
	for _, s := range []string{"sampler", "in memory", "restart", "system:read", "Linux"} {
		if !strings.Contains(c, s) {
			t.Errorf("providers.host comment does not contain %q:\n%s", s, c)
		}
	}

	keys := []struct {
		name     string
		mentions []string // the default, then the range
	}{
		{"enabled", []string{"false"}},
		{"sampleInterval", []string{"10s", "2s", "60s"}},
		{"historyDuration", []string{"1h", "sampleInterval", "24h"}},
		{"sourceTimeout", []string{"2s", "250ms", "5s"}},
	}
	for _, k := range keys {
		path := "providers.host." + k.name
		kn, vn := keyNode(doc.Content[0], path), valueNode(doc.Content[0], path)
		if kn == nil || vn == nil {
			t.Errorf("%s is missing from the default YAML", path)
			continue
		}
		if vn.Value != k.mentions[0] {
			t.Errorf("%s = %q, want the default %q", path, vn.Value, k.mentions[0])
		}
		comment := kn.HeadComment + "\n" + kn.LineComment + "\n" + vn.LineComment
		if strings.TrimSpace(comment) == "" {
			t.Errorf("%s has no comment", path)
			continue
		}
		for _, m := range k.mentions[1:] {
			if !strings.Contains(comment, m) {
				t.Errorf("%s comment %q does not name %q", path, strings.TrimSpace(comment), m)
			}
		}
	}
}

// TC-HP-E4: enabling with no other keys takes the defaults. Requirements:
// FR-1, FR-11.
func TestTCHPE4_EnablingWithNoOtherKeysTakesThe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(path, []byte("providers: {host: {enabled: true}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := HostProviderConfig{Enabled: true, SampleInterval: 10 * time.Second, HistoryDuration: time.Hour,
		SourceTimeout: 2 * time.Second}
	if got := cfg.Providers.Host.Settings; got != want {
		t.Errorf("providers.host = %+v, want %+v", got, want)
	}
}

// TC-HP-ERR4: an unknown key in providers.host fails Load with its location.
// Requirements: FR-1.
func TestTCHPERR4_AnUnknownKeyInProvidersHostFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(path, []byte(hostUnknownKeyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want an unknown-key error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "providers.host.interval") || !strings.Contains(msg, path+":4") {
		t.Errorf("Load error %q does not name providers.host.interval at %s:4", msg, path)
	}
}

// hostUnknownKeyYAML puts the unknown key interval on line 4.
const hostUnknownKeyYAML = "providers:\n  host:\n    enabled: true\n    interval: 5s\n"

// TC-HP-ERR5: each bound fails with its location and names the bound, and
// each bound value itself is accepted. Requirements: FR-11, FR-12, FR-13.
func TestTCHPERR5_EachBoundFailsWithItsLocationAnd(t *testing.T) {
	cases := []struct {
		name    string
		section string // the lines under host:, the last on line 3 + its index
		path    string // "" when accepted
		line    int
		bound   string
	}{
		{"sampleInterval 1s", "sampleInterval: 1s", "providers.host.sampleInterval", 3, "2s"},
		{"sampleInterval 61s", "sampleInterval: 61s", "providers.host.sampleInterval", 3, "60s"},
		{"historyDuration 25h", "historyDuration: 25h", "providers.host.historyDuration", 3, "24h"},
		{"historyDuration 5s with sampleInterval 10s", "sampleInterval: 10s\n    historyDuration: 5s",
			"providers.host.historyDuration", 4, "sampleInterval"},
		{"sourceTimeout 249ms", "sourceTimeout: 249ms", "providers.host.sourceTimeout", 3, "250ms"},
		{"sourceTimeout 5001ms", "sourceTimeout: 5001ms", "providers.host.sourceTimeout", 3, "5s"},

		{"sampleInterval 2s", "sampleInterval: 2s", "", 0, ""},
		{"sampleInterval 60s", "sampleInterval: 60s", "", 0, ""},
		{"historyDuration 24h", "historyDuration: 24h", "", 0, ""},
		{"historyDuration 10s with sampleInterval 10s", "sampleInterval: 10s\n    historyDuration: 10s", "", 0, ""},
		{"sourceTimeout 250ms", "sourceTimeout: 250ms", "", 0, ""},
		{"sourceTimeout 5s", "sourceTimeout: 5s", "", 0, ""},
	}
	dir := t.TempDir()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("bound-%d.yaml", i))
			if err := os.WriteFile(path, []byte("providers:\n  host:\n    "+tc.section+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if tc.path == "" {
				if err != nil {
					t.Fatalf("Load refused a bound value:\n%v", err)
				}
				return
			}
			var list ErrorList
			if !errors.As(err, &list) || len(list) != 1 {
				t.Fatalf("Load error = %v, want exactly one located error", err)
			}
			e := list[0]
			if e.Path != tc.path || e.Line != tc.line || e.Column == 0 {
				t.Errorf("error %v at %s %d:%d, want %s on line %d", e, e.Path, e.Line, e.Column, tc.path, tc.line)
			}
			if !strings.Contains(e.Error(), fmt.Sprintf("%s:%d", path, tc.line)) {
				t.Errorf("error %q does not name %s:%d", e.Error(), path, tc.line)
			}
			if !strings.Contains(e.Msg, tc.bound) {
				t.Errorf("message %q does not name the bound %s", e.Msg, tc.bound)
			}
		})
	}
}
