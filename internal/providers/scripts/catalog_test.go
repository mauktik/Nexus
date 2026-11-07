package scripts

import (
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/config"
)

// section parses a configuration file holding y and returns the scripts
// section's strict decode, the closure the registry hands Configure.
func section(t *testing.T, y string) func(any) error {
	t.Helper()
	cfg, err := config.Parse([]byte(y), "nexusd.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg.Providers.Scripts.Decode
}

const minimal = `providers:
  scripts:
    catalog:
      - id: deploy-api
        description: Deploy the API
        path: /opt/deploy/deploy-api.sh
        runAs: deploy
        workingDirectory: /opt/deploy
`

// TC-91: a minimal entry decodes with the defaults, and a parameter with
// neither arg nor env is positional in declared order.
func TestCatalogDefaultsAndParameterMapping(t *testing.T) {
	p := New(Options{})
	if err := p.Configure(section(t, minimal)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	s, ok := p.Catalog().Get("deploy-api")
	if !ok {
		t.Fatal("deploy-api is not in the catalog")
	}
	if s.Timeout != time.Hour || s.Concurrency != 1 || s.TerminationGrace != 30*time.Second {
		t.Errorf("defaults: timeout %s concurrency %d terminationGrace %s, want 1h 1 30s",
			s.Timeout, s.Concurrency, s.TerminationGrace)
	}
	if s.Description != "Deploy the API" || s.Path != "/opt/deploy/deploy-api.sh" || s.RunAs != "deploy" ||
		s.WorkingDirectory != "/opt/deploy" {
		t.Errorf("entry %+v", s)
	}

	withParams := minimal + `        timeout: 2m
        concurrency: 3
        terminationGrace: 5s
        parameters:
          - {name: branch, type: string, pattern: '^[a-z]+$', arg: --branch}
          - {name: first, type: string, pattern: '^[a-z]+$'}
          - {name: apiKey, type: string, pattern: '^[A-Za-z0-9]+$', sensitive: true, env: API_KEY}
          - {name: second, type: string, pattern: '^[a-z]+$'}
`
	if err := p.Configure(section(t, withParams)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	s, _ = p.Catalog().Get("deploy-api")
	if s.Timeout != 2*time.Minute || s.Concurrency != 3 || s.TerminationGrace != 5*time.Second {
		t.Errorf("explicit values: timeout %s concurrency %d terminationGrace %s", s.Timeout, s.Concurrency, s.TerminationGrace)
	}
	want := map[string]int{"branch": 0, "first": 1, "apiKey": 0, "second": 2}
	var order []string
	for _, prm := range s.Parameters {
		order = append(order, prm.Name)
		if prm.Position != want[prm.Name] {
			t.Errorf("parameter %s: position %d, want %d", prm.Name, prm.Position, want[prm.Name])
		}
	}
	if got := strings.Join(order, ","); got != "branch,first,apiKey,second" {
		t.Errorf("parameters in order %s, want declared order", got)
	}
}

// An absent or empty section is an empty catalog, never a stale one.
func TestCatalogAbsentSectionIsEmpty(t *testing.T) {
	p := New(Options{})
	if err := p.Configure(section(t, minimal)); err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(func(any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := len(p.Catalog().Scripts()); n != 0 {
		t.Fatalf("catalog has %d scripts after an absent section, want 0", n)
	}
}

// TC-ERR24 (provider half): Configure applies the same rules to whatever the
// decode yields, naming the entry and field, and a refused section leaves
// the running catalog untouched.
func TestConfigureRefusesInvalidCatalog(t *testing.T) {
	p := New(Options{})
	if err := p.Configure(section(t, minimal)); err != nil {
		t.Fatal(err)
	}
	valid := config.Script{ID: "x", Description: "d", Path: "/x.sh", RunAs: "u", WorkingDirectory: "/"}
	rows := []struct {
		name   string
		mutate func(*config.ScriptsConfig)
		want   string
	}{
		{"missing id", func(c *config.ScriptsConfig) { c.Catalog[0].ID = "" }, "catalog[0].id: required"},
		{"missing description", func(c *config.ScriptsConfig) { c.Catalog[0].Description = "" }, "catalog[0].description: required"},
		{"missing runAs", func(c *config.ScriptsConfig) { c.Catalog[0].RunAs = "" }, "catalog[0].runAs: required"},
		{"missing workingDirectory", func(c *config.ScriptsConfig) { c.Catalog[0].WorkingDirectory = "" }, "catalog[0].workingDirectory: required"},
		{"missing path", func(c *config.ScriptsConfig) { c.Catalog[0].Path = "" }, "catalog[0].path: required"},
		{"relative path", func(c *config.ScriptsConfig) { c.Catalog[0].Path = "x.sh" }, "catalog[0].path: must be an absolute path"},
		{"duplicate id", func(c *config.ScriptsConfig) { c.Catalog = append(c.Catalog, c.Catalog[0]) }, `catalog[1].id: duplicate script id "x"`},
		{"string parameter without pattern", func(c *config.ScriptsConfig) {
			c.Catalog[0].Parameters = []config.Parameter{{Name: "b", Type: "string", Arg: "--b"}}
		}, "catalog[0].parameters[0].pattern: required"},
		{"sensitive without env", func(c *config.ScriptsConfig) {
			c.Catalog[0].Parameters = []config.Parameter{{Name: "k", Type: "string", Pattern: "^a$", Sensitive: true}}
		}, "catalog[0].parameters[0].env: required because the parameter is sensitive"},
		{"invalid regex", func(c *config.ScriptsConfig) {
			c.Catalog[0].Parameters = []config.Parameter{{Name: "k", Type: "string", Pattern: "(", Arg: "--k"}}
		}, "catalog[0].parameters[0].pattern: not a valid regular expression"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			err := p.Configure(func(into any) error {
				c := into.(*config.ScriptsConfig)
				c.Catalog = []config.Script{valid}
				r.mutate(c)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "providers.scripts."+r.want) {
				t.Fatalf("Configure error %v, want one naming %q", err, r.want)
			}
			if _, ok := p.Catalog().Get("deploy-api"); !ok || len(p.Catalog().Scripts()) != 1 {
				t.Fatal("a refused section replaced the running catalog")
			}
		})
	}

	t.Run("unknown key in entry", func(t *testing.T) {
		y := strings.Replace(minimal, "        runAs: deploy\n", "        runAs: deploy\n        timeoutz: 1h\n", 1)
		cfg, err := config.Parse([]byte(y), "nexusd.yaml")
		if err == nil {
			t.Fatalf("Parse accepted an unknown key: %+v", cfg)
		}
		if !strings.Contains(err.Error(), `providers.scripts.catalog[0].timeoutz: unknown key "timeoutz"`) {
			t.Fatalf("error %v does not name the key", err)
		}
	})
}

// The catalog hands out copies: a caller changing what it got does not
// change the catalog other readers see.
func TestCatalogReadsAreCopies(t *testing.T) {
	p := New(Options{})
	y := minimal + "        env: {TOKEN: secret}\n        parameters:\n          - {name: a, type: string, pattern: '^a$'}\n"
	if err := p.Configure(section(t, y)); err != nil {
		t.Fatal(err)
	}
	s, _ := p.Catalog().Get("deploy-api")
	s.Env["TOKEN"] = "changed"
	s.Parameters[0].Name = "changed"
	again, _ := p.Catalog().Get("deploy-api")
	if again.Env["TOKEN"] != "secret" || again.Parameters[0].Name != "a" {
		t.Fatalf("catalog changed through a read: %+v", again)
	}
}
