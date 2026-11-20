// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-62 to TC-RA-66,
// TC-RA-E16, TC-RA-ERR17 and TC-RA-ERR18 (WI 3.4): nexusd.config.list and
// nexusd.config.get on the T0 stack, reading the file at the daemon's
// ConfigPath.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// configItem is one ConfigKey as the wire carries it, properties raw.
type configItem struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Host       string          `json:"host"`
	Properties json.RawMessage `json:"properties"`
}

// configProps is a ConfigKey's properties decoded generically.
type configProps struct {
	Value     any    `json:"value"`
	Effective any    `json:"effective"`
	Source    string `json:"source"`
}

// writeConfigFile writes body as a 0600 config file in a temp dir.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// configStackOn is a T0 stack whose daemon's ConfigPath is path.
func configStackOn(t *testing.T, path string, cfg stackConfig) *stack {
	t.Helper()
	s := newStack(t, cfg)
	s.daemon.setConfigPath(path)
	return s
}

func (s *stack) configList(t *testing.T, token string) response {
	t.Helper()
	return s.do(t, http.MethodGet, "/providers/nexusd/config", token, "", nil)
}

func (s *stack) configGet(t *testing.T, token, name string) response {
	t.Helper()
	return s.do(t, http.MethodGet, "/providers/nexusd/config/"+name, token, "", nil)
}

// listItems decodes a 200 config list.
func listItems(t *testing.T, r response) []configItem {
	t.Helper()
	return decodeJSON[struct {
		Items []configItem `json:"items"`
	}](t, r.want(t, http.StatusOK).body).Items
}

// gotKey decodes a 200 config get.
func gotKey(t *testing.T, r response) (configItem, configProps) {
	t.Helper()
	it := decodeJSON[configItem](t, r.want(t, http.StatusOK).body)
	return it, decodeJSON[configProps](t, it.Properties)
}

// sameJSON fails unless got and want are equal once both are marshaled and
// decoded generically.
func sameJSON(t *testing.T, what string, got json.RawMessage, want any) {
	t.Helper()
	wb, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v: %s", what, err, got)
	}
	if err := json.Unmarshal(wb, &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %s, want %s", what, got, wb)
	}
}

// entryJSON is a config.Entry in the wire shape of ConfigKeyProperties.
func entryJSON(e config.Entry) map[string]any {
	return map[string]any{"value": e.Value, "effective": e.Effective, "source": e.Source}
}

// defaultConfigWithNexusd is deploy/nexusd.default.yaml with
// providers.nexusd.enabled set to true.
func defaultConfigWithNexusd(t *testing.T) []byte {
	t.Helper()
	src := filepath.Join("..", "..", "..", "deploy", "nexusd.default.yaml")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := config.Edit(data, src, []config.Assignment{{Key: "providers.nexusd.enabled", Value: "true"}})
	if err != nil {
		t.Fatalf("edit %s: %v", src, err)
	}
	return out
}

// TC-RA-62: config list returns every leaf key once, in order, with value, effective and source. Owner WI 3.4. Requirements: FR-6, FR-13.
func TestTCRA62_ConfigListReturnsEveryLeafKeyOnceInOrderWith(t *testing.T) {
	data := defaultConfigWithNexusd(t)
	path := writeConfigFile(t, string(data))
	s := configStackOn(t, path, stackConfig{})

	items := listItems(t, s.configList(t, s.admin))

	// Items.
	leaves := config.LeafKeys()
	want := make([]string, len(leaves))
	for i, k := range leaves {
		want[i] = k.Path
	}
	got := make([]string, len(items))
	seen := map[string]bool{}
	for i, it := range items {
		got[i] = it.Name
		if seen[it.Name] {
			t.Errorf("duplicate item %s", it.Name)
		}
		seen[it.Name] = true
		if it.Type != spec.ConfigType || it.Host != StackHost {
			t.Errorf("item %s: type %q host %q, want %q %q", it.Name, it.Type, it.Host, spec.ConfigType, StackHost)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("names %v, want %v", got, want)
	}

	// Values.
	entries, err := config.InspectAll(data, path)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		sameJSON(t, "properties of "+e.Key, items[i].Properties, entryJSON(e))
	}

	// Audit.
	if outs := outcomes(s.auditOf(t, spec.ConfigList.Op().ID)); !slices.Equal(outs,
		[]string{audit.OutcomeRequested, audit.OutcomeCompleted}) {
		t.Errorf("audit %v, want requested, completed", outs)
	}

	// Scope.
	s.configList(t, s.reader).want(t, http.StatusForbidden)
}

// TC-RA-63: Catalog env secrets are redacted, and the raw value appears in no response, log or audit record. Owner WI 3.4. Requirements: FR-6, NFR-1, FR-13.
func TestTCRA63_CatalogEnvSecretsAreRedactedAndTheRawValue(t *testing.T) {
	const sentinel = "RA_ENV_SENTINEL_9f3c"
	path := fixture(t, "catalog-env.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Non-vacuity.
	if !strings.Contains(string(raw), sentinel) {
		t.Fatalf("fixture %s does not hold the sentinel", path)
	}
	s := configStackOn(t, path, stackConfig{})

	// Step 1.
	listResp := s.configList(t, s.admin)
	var catalog *configItem
	for _, it := range listItems(t, listResp) {
		if it.Name == "providers.scripts.catalog" {
			catalog = &it
		}
	}
	if catalog == nil {
		t.Fatal("no providers.scripts.catalog item")
	}
	// Step 2.
	getResp := s.configGet(t, s.admin, "providers.scripts.catalog")
	_, got := gotKey(t, getResp)

	for what, props := range map[string]configProps{
		"list": decodeJSON[configProps](t, catalog.Properties),
		"get":  got,
	} {
		if props.Source != config.SourceFile {
			t.Errorf("%s: source %q, want file", what, props.Source)
		}
		for field, v := range map[string]any{"value": props.Value, "effective": props.Effective} {
			list, ok := v.([]any)
			if !ok || len(list) != 1 {
				t.Fatalf("%s %s: %#v, want one catalog entry", what, field, v)
			}
			entry, _ := list[0].(map[string]any)
			env, _ := entry["env"].(map[string]any)
			want := map[string]any{"API_TOKEN": config.Redacted, "DB_PASSWORD": config.Redacted}
			if !reflect.DeepEqual(env, want) {
				t.Errorf("%s %s env = %v, want %v", what, field, env, want)
			}
		}
	}

	// Step 3.
	auditBytes := readAuditDir(t, s.auditDir)
	for what, b := range map[string][]byte{
		"list body":  listResp.body,
		"get body":   getResp.body,
		"stack logs": []byte(s.logs.String()),
		"audit file": auditBytes,
	} {
		if n := strings.Count(string(b), sentinel); n != 0 {
			t.Errorf("%s holds the sentinel %d times", what, n)
		}
	}
	if len(auditBytes) == 0 {
		t.Error("the audit directory is empty: the audit check is vacuous")
	}
}

// readAuditDir concatenates every file under dir.
func readAuditDir(t *testing.T, dir string) []byte {
	t.Helper()
	var out []byte
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		out = append(out, b...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TC-RA-64: An absent key reports source default and the default effective value; an empty host.name reports the OS hostname. Owner WI 3.4. Requirements: FR-6.
func TestTCRA64_AnAbsentKeyReportsSourceDefaultAndTheDefault(t *testing.T) {
	body := "host:\n  name: \"\"\nlisten: [\"127.0.0.1:7443\"]\ntls:\n  mode: auto\nproviders:\n  nexusd:\n    enabled: true\n"
	path := writeConfigFile(t, body)
	s := configStackOn(t, path, stackConfig{})

	// Step 1.
	cfg, err := config.Parse([]byte(body), path)
	if err != nil {
		t.Fatal(err)
	}
	it, got := gotKey(t, s.configGet(t, s.admin, "providers.host.sampleInterval"))
	if it.Name != "providers.host.sampleInterval" {
		t.Errorf("name %q", it.Name)
	}
	if got.Value != nil || got.Source != config.SourceDefault {
		t.Errorf("value %#v source %q, want null and default", got.Value, got.Source)
	}
	eff, _ := got.Effective.(string)
	d, err := time.ParseDuration(eff)
	if err != nil || d != cfg.Providers.Host.Settings.SampleInterval {
		t.Errorf("effective %#v, want the short form of %s", got.Effective, cfg.Providers.Host.Settings.SampleInterval)
	}

	// Step 2.
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	_, got = gotKey(t, s.configGet(t, s.admin, "host.name"))
	if got.Value != "" || got.Source != config.SourceFile || got.Effective != hostname {
		t.Errorf("host.name = %+v, want value \"\", source file, effective %q", got, hostname)
	}
}

// TC-RA-65: Config get works where systemd self-management is unavailable. Owner WI 3.4. Requirements: FR-9, FR-6.
func TestTCRA65_ConfigGetWorksWhereSystemdSelfManagementIs(t *testing.T) {
	unavailable := func() detach.Availability {
		return detach.Availability{Available: false, Reason: "requires systemd"}
	}
	s := newStack(t, stackConfig{opts: Options{Systemd: unavailable}})
	if a := s.p.selfManagementReady(); a.Available {
		t.Fatal("self-management is available: the seam is not in effect")
	}

	// Step 1.
	items := listItems(t, s.configList(t, s.admin))
	var tlsMode *configItem
	for _, it := range items {
		if it.Name == "tls.mode" {
			tlsMode = &it
		}
	}
	if tlsMode == nil {
		t.Fatal("no tls.mode item")
	}
	if p := decodeJSON[configProps](t, tlsMode.Properties); p.Value != "auto" || p.Source != config.SourceFile {
		t.Errorf("list tls.mode = %+v, want auto from the file", p)
	}

	// Step 2.
	if _, p := gotKey(t, s.configGet(t, s.admin, "tls.mode")); p.Value != "auto" || p.Source != config.SourceFile {
		t.Errorf("get tls.mode = %+v, want auto from the file", p)
	}
}

// TC-RA-66: Config get re-reads the file on every call. Owner WI 3.4. Requirements: FR-6.
func TestTCRA66_ConfigGetReReadsTheFileOnEveryCall(t *testing.T) {
	const head = "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: auto\nproviders:\n  nexusd:\n    enabled: true\n  host:\n"
	path := writeConfigFile(t, head+"    enabled: false\n")
	s := configStackOn(t, path, stackConfig{})

	// Step 1.
	if _, p := gotKey(t, s.configGet(t, s.admin, "providers.host.enabled")); p.Value != false ||
		p.Source != config.SourceFile {
		t.Fatalf("step 1: %+v, want false from the file", p)
	}

	// Step 2.
	if err := os.WriteFile(path, []byte(head+"    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Step 3.
	if _, p := gotKey(t, s.configGet(t, s.admin, "providers.host.enabled")); p.Value != true ||
		p.Effective != true || p.Source != config.SourceFile {
		t.Errorf("step 3: %+v, want true read from disk", p)
	}
	if n := s.daemon.reloadCount(); n != 0 {
		t.Errorf("reloads %d, want 0", n)
	}
}

// TC-RA-E16: A config get for a mapping key returns its children. Owner WI 3.4. Requirements: FR-6.
func TestTCRAE16_AConfigGetForAMappingKeyReturnsItsChildren(t *testing.T) {
	const body = "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: auto\nproviders:\n  nexusd:\n    enabled: true\n" +
		"  host:\n    enabled: true\n    sampleInterval: 30s\n"
	path := writeConfigFile(t, body)
	s := configStackOn(t, path, stackConfig{})

	it, got := gotKey(t, s.configGet(t, s.admin, "providers.host"))
	if it.Name != "providers.host" {
		t.Errorf("name %q", it.Name)
	}
	key, err := config.LookupKey("providers.host")
	if err != nil {
		t.Fatal(err)
	}
	value, ok := got.Value.(map[string]any)
	if !ok {
		t.Fatalf("value %#v, want a mapping", got.Value)
	}
	keys := make([]string, 0, len(value))
	for k := range value {
		keys = append(keys, k)
	}
	if !reflect.DeepEqual(sorted(keys), sorted(key.Children)) {
		t.Errorf("keys %v, want %v", sorted(keys), sorted(key.Children))
	}
	for _, c := range key.Children {
		leaf, err := config.Inspect([]byte(body), path, "providers.host."+c)
		if err != nil {
			t.Fatal(err)
		}
		child, err := json.Marshal(value[c])
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "child "+c, child, entryJSON(leaf))
	}
	if got.Source != config.SourceFile {
		t.Errorf("source %q, want file", got.Source)
	}
	eff, ok := got.Effective.(map[string]any)
	if !ok || eff["enabled"] != true || eff["sampleInterval"] != "30s" {
		t.Errorf("effective %#v", got.Effective)
	}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// TC-RA-ERR17: Config get answers not-found for an unknown key and config-invalid for a broken file. Owner WI 3.4. Requirements: FR-6.
func TestTCRAERR17_ConfigGetAnswersNotFoundForAnUnknownKeyAndConfig(t *testing.T) {
	const head = "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: auto\nproviders:\n  nexusd:\n    enabled: true\n"
	path := writeConfigFile(t, head)
	s := configStackOn(t, path, stackConfig{})

	// Step 1.
	key, err := config.LookupKey("providers.host")
	if err != nil {
		t.Fatal(err)
	}
	m := wantTokenProblem(t, s.configGet(t, s.admin, "providers.host.enabeld"), http.StatusNotFound, problem.NotFound)
	want := "no key providers.host.enabeld; the nearest valid parent is providers.host, whose keys are: " +
		strings.Join(key.Children, ", ")
	if m["detail"] != want {
		t.Errorf("detail %q, want %q", m["detail"], want)
	}

	// Step 2.
	if err := os.WriteFile(path, []byte(head+"  host:\n    bogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Step 3.
	for _, r := range []response{s.configList(t, s.admin), s.configGet(t, s.admin, "tls.mode")} {
		m := wantTokenProblem(t, r, http.StatusUnprocessableEntity, problem.ConfigInvalid)
		if m["configPath"] != path {
			t.Errorf("configPath %v, want %s", m["configPath"], path)
		}
		errs, _ := m["errors"].([]any)
		located := false
		for _, e := range errs {
			fe, _ := e.(map[string]any)
			line, _ := fe["line"].(float64)
			col, _ := fe["column"].(float64)
			if fe["path"] == "providers.host.bogus" && line == 8 && col > 0 {
				located = true
			}
		}
		if !located {
			t.Errorf("no error located at providers.host.bogus line 8: %s", r.body)
		}
	}
}

// TC-RA-ERR18: Config get refuses a nexusd:read-only token. Owner WI 3.4. Requirements: ASM-5, FR-6.
func TestTCRAERR18_ConfigGetRefusesANexusdReadOnlyToken(t *testing.T) {
	s := newStack(t, stackConfig{})

	for _, c := range []struct {
		action string
		do     func(token string) response
	}{
		{spec.ConfigList.Op().ID, func(tok string) response { return s.configList(t, tok) }},
		{spec.ConfigGet.Op().ID, func(tok string) response { return s.configGet(t, tok, "tls.mode") }},
	} {
		m := wantTokenProblem(t, c.do(s.reader), http.StatusForbidden, problem.Forbidden)
		if m["requiredScope"] != spec.AdminScope {
			t.Errorf("%s: requiredScope %v, want %s", c.action, m["requiredScope"], spec.AdminScope)
		}
		denied := false
		for _, r := range s.auditOf(t, c.action) {
			denied = denied || r.Outcome == audit.OutcomeDenied
		}
		if !denied {
			t.Errorf("%s: audit %v, want a denied record", c.action, outcomes(s.auditOf(t, c.action)))
		}
		c.do("").want(t, http.StatusUnauthorized)
	}
}
