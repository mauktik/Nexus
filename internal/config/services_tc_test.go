// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-37, TC-SV-38,
// TC-SV-39, TC-SV-40, TC-SV-E7, TC-SV-ERR11. Owner WI: 2.3.

package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

var allActions = []string{"start", "stop", "restart"}

// defaultServices is TC-SV-37's expected default section.
var defaultServices = ServicesConfig{
	Control: []ServiceControl{},
	Reboot:  ServicesReboot{Timeout: 5 * time.Minute},
}

func parseServices(t *testing.T, doc string) ServicesConfig {
	t.Helper()
	cfg, err := Parse([]byte(doc), "c.yaml")
	if err != nil {
		t.Fatalf("Parse:\n%s\n%v", doc, err)
	}
	return cfg.Providers.Services.Settings
}

// TC-SV-37: An absent or minimal services section takes every default. Owner WI 2.3. Requirements: FR-1, FR-8, FR-10.
func TestTCSV37_AnAbsentOrMinimalServicesSectionTakes(t *testing.T) {
	if got := Default().Providers.Services.Settings; !reflect.DeepEqual(got, defaultServices) {
		t.Errorf("Default() services = %+v, want %+v", got, defaultServices)
	}
	if got := parseServices(t, "listen: [\"127.0.0.1:7443\"]\n"); !reflect.DeepEqual(got, defaultServices) {
		t.Errorf("absent section = %+v, want %+v", got, defaultServices)
	}
	enabled := defaultServices
	enabled.Enabled = true
	if got := parseServices(t, "providers:\n  services:\n    enabled: true\n"); !reflect.DeepEqual(got, enabled) {
		t.Errorf("{enabled: true} = %+v, want %+v", got, enabled)
	}

	doc := "providers:\n  services:\n    control:\n      - name: nx-a\n" +
		"      - {name: nx-b, actions: [restart], timeout: 30s}\n"
	got := parseServices(t, doc)
	want := []ServiceControl{
		{Name: "nx-a", Actions: allActions, Timeout: 90 * time.Second},
		{Name: "nx-b", Actions: []string{"restart"}, Timeout: 30 * time.Second},
	}
	if !reflect.DeepEqual(got.Control, want) {
		t.Errorf("control = %+v, want %+v", got.Control, want)
	}
	if got.Enabled || got.Reboot != defaultServices.Reboot {
		t.Errorf("enabled %v reboot %+v, want the defaults", got.Enabled, got.Reboot)
	}

	// A provider decoding its section through Section.Decode gets the same
	// per-entry defaults.
	cfg, err := Parse([]byte(doc), "c.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := ServicesConfig{Reboot: ServicesReboot{Timeout: 5 * time.Minute}}
	if err := cfg.Providers.Services.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Control, want) {
		t.Errorf("Section.Decode control = %+v, want %+v", out.Control, want)
	}
}

var (
	nexusdOnce sync.Once
	nexusdBin  string
	nexusdErr  error
)

// validateConfig runs `nexusd validate-config --config path`, building the
// binary once per test run.
func validateConfig(t *testing.T, path string) (string, error) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: not building nexusd for validate-config")
	}
	nexusdOnce.Do(func() {
		gobin, err := exec.LookPath("go")
		if err != nil {
			nexusdErr = err
			return
		}
		dir, err := os.MkdirTemp("", "nexusd-validate-*")
		if err != nil {
			nexusdErr = err
			return
		}
		nexusdBin = filepath.Join(dir, "nexusd")
		cmd := exec.Command(gobin, "build", "-o", nexusdBin, "./cmd/nexusd")
		cmd.Dir = filepath.Join("..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			nexusdErr = fmt.Errorf("go build ./cmd/nexusd: %v\n%s", err, out)
		}
	})
	if errors.Is(nexusdErr, exec.ErrNotFound) {
		t.Skip("no go tool on PATH to build nexusd")
	}
	if nexusdErr != nil {
		t.Fatal(nexusdErr)
	}
	out, err := exec.Command(nexusdBin, "validate-config", "--config", path).CombinedOutput()
	return string(out), err
}

func writeConfig(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TC-SV-38: Boundary values are accepted, and an entry for a service that is not installed is accepted. Owner WI 2.3. Requirements: FR-8, FR-10.
func TestTCSV38_BoundaryValuesAreAcceptedAndAnEntry(t *testing.T) {
	long := strings.Repeat("a", 64)
	for _, rebootTimeout := range []string{"1m", "30m"} {
		doc := "providers:\n  services:\n    control:\n" +
			"      - name: nx-not-installed\n" +
			"      - {name: a, timeout: 10m}\n" +
			"      - name: " + long + "\n" +
			"      - name: getty-like\n" +
			"    reboot:\n      timeout: " + rebootTimeout + "\n"
		got := parseServices(t, doc)
		if len(got.Control) != 4 || got.Control[1].Timeout != 10*time.Minute || got.Control[2].Name != long {
			t.Errorf("control = %+v", got.Control)
		}
		if d, _ := time.ParseDuration(rebootTimeout); got.Reboot.Timeout != d {
			t.Errorf("reboot.timeout = %s, want %s", got.Reboot.Timeout, d)
		}
		if out, err := validateConfig(t, writeConfig(t, doc)); err != nil {
			t.Errorf("validate-config with reboot.timeout %s: %v\n%s", rebootTimeout, err, out)
		}
	}
}

// TC-SV-39: Remote config set refuses reboot.enabled=true and accepts the other services keys. Owner WI 2.3. Requirements: FR-8, FR-10, A-19.
//
// The nexusd provider applies nexusdspec.ConfigDenyList before Edit
// (configset.go, deniedChange); this test applies the list the same way.
func TestTCSV39_RemoteConfigSetRefusesRebootEnabledTrue(t *testing.T) {
	src, err := os.ReadFile(defaultConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	denied := func(a Assignment) (nexusdspec.ConfigDeny, bool) {
		for _, d := range nexusdspec.ConfigDenyList {
			if (a.Key == d.Key || (d.Subtree && strings.HasPrefix(a.Key, d.Key+"."))) && (d.Value == "" || a.Value == d.Value) {
				return d, true
			}
		}
		return nexusdspec.ConfigDeny{}, false
	}
	comments := func(b []byte) []string {
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if i := strings.Index(l, "#"); i >= 0 {
				out = append(out, strings.TrimSpace(l[i:]))
			}
		}
		return out
	}

	// Step 1: refused by the deny-list, nothing written.
	before := sha256.Sum256(data)
	a := Assignment{Key: "providers.services.reboot.enabled", Value: "true"}
	d, ok := denied(a)
	if !ok {
		t.Fatal("providers.services.reboot.enabled=true is not on the deny-list")
	}
	if d.Key != a.Key || d.Value != "true" || d.Subtree ||
		!strings.Contains(d.Reason, "the reboot is switched on only by editing the file on the host") {
		t.Errorf("deny entry %+v", d)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != before {
		t.Error("the file changed")
	}

	// Steps 2 and 3: accepted, comments kept.
	for _, s := range []string{"providers.services.reboot.enabled=false", "providers.services.enabled=true"} {
		a, err := ParseAssignment(s)
		if err != nil {
			t.Fatal(err)
		}
		if d, ok := denied(a); ok {
			t.Errorf("%s denied: %+v", s, d)
			continue
		}
		out, changes, err := Edit(data, path, []Assignment{a})
		if err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		if len(changes) != 1 || changes[0].Key != a.Key {
			t.Errorf("%s: changes %+v", s, changes)
		}
		if !reflect.DeepEqual(comments(out), comments(data)) {
			t.Errorf("%s: comments not preserved", s)
		}
		if _, err := Parse(out, path); err != nil {
			t.Errorf("%s: edited file invalid: %v", s, err)
		}
	}

	// Step 4: a list is never settable.
	_, _, err = Edit(data, path, []Assignment{{Key: "providers.services.control", Value: "[x]"}})
	if err == nil || !strings.Contains(err.Error(), "is a list, which cannot be set") {
		t.Errorf("control=[x]: %v, want the list refusal", err)
	}
}

// TC-SV-40: The shipped default file carries the section, every key with its default and a comment. Owner WI 2.3. Requirements: FR-1, NFR-8.
func TestTCSV40_TheShippedDefaultFileCarriesTheSection(t *testing.T) {
	data, err := os.ReadFile(defaultConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	if keyNode(root, "providers.services") == nil {
		t.Fatal("deploy/nexusd.default.yaml has no providers.services section")
	}
	for path, want := range map[string]string{
		"providers.services.enabled":        "false",
		"providers.services.control":        "[]",
		"providers.services.reboot.enabled": "false",
		"providers.services.reboot.timeout": "5m",
	} {
		k, v := keyNode(root, path), valueNode(root, path)
		if k == nil || v == nil {
			t.Errorf("%s missing", path)
			continue
		}
		got := v.Value
		if v.Kind == yaml.SequenceNode {
			got = fmt.Sprintf("[%s]", strings.Repeat("x", len(v.Content)))
		}
		if got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
		if strings.TrimSpace(k.HeadComment+v.LineComment+k.LineComment) == "" {
			t.Errorf("%s has no head or line comment", path)
		}
	}
	k, v := keyNode(root, "providers.services.reboot.enabled"), valueNode(root, "providers.services.reboot.enabled")
	if c := k.HeadComment + v.LineComment + k.LineComment; !strings.Contains(c, "config set refuses true") {
		t.Errorf("reboot.enabled comment %q does not say config set refuses true", c)
	}

	cfg, err := Load(defaultConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers.Services.Settings; !reflect.DeepEqual(got, defaultServices) {
		t.Errorf("deploy default providers.services = %+v, want %+v", got, defaultServices)
	}
}

// TC-SV-E7: A duplicate is reported at the second entry's location, and a .service suffix names the canonical form. Owner WI 2.3. Requirements: FR-8.
func TestTCSVE7_ADuplicateIsReportedAtTheSecond(t *testing.T) {
	// nx-a at line 10 and again at line 12.
	doc := strings.Repeat("# filler\n", 6) +
		"providers:\n" + // 7
		"  services:\n" + // 8
		"    control:\n" + // 9
		"      - name: nx-a\n" + // 10
		"        actions: [stop]\n" + // 11
		"      - name: nx-a\n" // 12
	_, err := Parse([]byte(doc), "c.yaml")
	if err == nil {
		t.Fatal("duplicate accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "c.yaml:12:15: providers.services.control[1].name: ") || !strings.Contains(msg, "line 10") {
		t.Errorf("duplicate error %q, want it at line 12 naming line 10", msg)
	}

	_, err = Parse([]byte("providers:\n  services:\n    control:\n      - name: nx-b.service\n"), "c.yaml")
	if err == nil || !strings.Contains(err.Error(), `use "nx-b"`) || !strings.Contains(err.Error(), "c.yaml:4:15:") {
		t.Errorf("suffix error %v, want it at 4:15 saying to use \"nx-b\"", err)
	}
}

// TC-SV-ERR11: validate-config refuses every invalid services value with its location. Owner WI 2.3. Requirements: FR-1, FR-8, FR-9, FR-10, A-30, A-31.
func TestTCSVERR11_ValidateConfigRefusesEveryInvalidServicesValue(t *testing.T) {
	const head = "providers:\n  services:\n" // lines 1 and 2
	entry := func(name string) string { return head + "    control:\n      - name: " + name + "\n" }
	cases := []struct {
		label     string
		doc       string
		line, col int
		path      string
		want      []string
	}{
		{"ssh", entry("ssh"), 4, 15, "providers.services.control[0].name", []string{"protected", `"ssh"`}},
		{"nexusd", entry("nexusd"), 4, 15, "providers.services.control[0].name", []string{"protected", `"nexusd"`}},
		{"getty@tty1", entry("getty@tty1"), 4, 15, "providers.services.control[0].name", []string{"protected", `"getty@*"`}},
		{"nexus-op-x", entry("nexus-op-x"), 4, 15, "providers.services.control[0].name", []string{"protected", `"nexus-op-*"`}},
		{"dropbear", entry("dropbear"), 4, 15, "providers.services.control[0].name", []string{"protected", "OpenRC", `"dropbear"`}},
		{"foo.service", entry("foo.service"), 4, 15, "providers.services.control[0].name", []string{".service", `use "foo"`}},
		{"a b", entry("a b"), 4, 15, "providers.services.control[0].name", []string{"must match"}},
		{"-x", entry("-x"), 4, 15, "providers.services.control[0].name", []string{"must match"}},
		{"duplicate", head + "    control:\n      - name: nx-a\n      - name: nx-a\n", 5, 15,
			"providers.services.control[1].name", []string{"duplicate", "line 4"}},
		{"reload", head + "    control:\n      - name: nx-a\n        actions: [start, reload]\n", 5, 18,
			"providers.services.control[0].actions[1]", []string{"start, stop or restart", `"reload"`}},
		{"0s", head + "    control:\n      - {name: nx-a, timeout: 0s}\n", 4, 31,
			"providers.services.control[0].timeout", []string{"greater than zero", "10m"}},
		{"-1s", head + "    control:\n      - {name: nx-a, timeout: -1s}\n", 4, 31,
			"providers.services.control[0].timeout", []string{"greater than zero", "10m"}},
		{"11m", head + "    control:\n      - {name: nx-a, timeout: 11m}\n", 4, 31,
			"providers.services.control[0].timeout", []string{"at most 10m"}},
		{"reboot 59s", head + "    reboot:\n      timeout: 59s\n", 4, 16,
			"providers.services.reboot.timeout", []string{"between 1m and 30m"}},
		{"reboot 31m", head + "    reboot:\n      timeout: 31m\n", 4, 16,
			"providers.services.reboot.timeout", []string{"between 1m and 30m"}},
		{"contorl", head + "    contorl: []\n", 3, 5, "providers.services.contorl", []string{`unknown key "contorl"`}},
		{"reboot.enabeld", head + "    reboot:\n      enabeld: true\n", 4, 7,
			"providers.services.reboot.enabeld", []string{`unknown key "enabeld"`}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			path := writeConfig(t, c.doc)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("accepted:\n%s", c.doc)
			}
			var list ErrorList
			if !errors.As(err, &list) || len(list) != 1 {
				t.Fatalf("errors %v, want exactly one", err)
			}
			prefix := fmt.Sprintf("%s:%d:%d: %s: ", path, c.line, c.col, c.path)
			msg := list[0].Error()
			if !strings.HasPrefix(msg, prefix) {
				t.Errorf("error %q, want it to start %q", msg, prefix)
			}
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q lacks %q", msg, w)
				}
			}

			out, err := validateConfig(t, path)
			if err == nil {
				t.Errorf("validate-config exited 0:\n%s", out)
			}
			if !bytes.Contains([]byte(out), []byte(prefix)) {
				t.Errorf("validate-config output lacks %q:\n%s", prefix, out)
			}
		})
	}
}
