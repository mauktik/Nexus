package config

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// nginx-provider TC-54: the nginx section's built-in defaults, its spelling
// in deploy/nexusd.default.yaml, and its place in the daemon's provider
// configs. Requirements: FR-1, FR-2, ASM-3.
func TestNginxSectionDefaults(t *testing.T) {
	got := Default().Providers.Nginx.Settings
	want := NginxConfig{
		Enabled:             false,
		IncludeDir:          "",
		IncludeSuffix:       ".conf",
		DisabledSuffix:      ".disabled",
		Service:             "nginx",
		TestTimeout:         30 * time.Second,
		StateTimeout:        30 * time.Second,
		ReloadVerifyTimeout: 15 * time.Second,
		MaxConfigBytes:      262144,
	}
	if got != want {
		t.Errorf("Default().Providers.Nginx.Settings = %+v, want %+v", got, want)
	}

	cfg, err := Load(defaultConfigPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", defaultConfigPath, err)
	}
	if cfg.Providers.Nginx.Settings != want {
		t.Errorf("default YAML nginx section = %+v, want %+v", cfg.Providers.Nginx.Settings, want)
	}

	data, err := os.ReadFile(defaultConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	section := keyNode(doc.Content[0], "providers.nginx")
	if section == nil {
		t.Fatal("deploy/nexusd.default.yaml has no providers.nginx section")
	}
	if c := section.HeadComment; !strings.Contains(c, "include") || !strings.Contains(c, "*.conf") {
		t.Errorf("providers.nginx comment does not say how nginx.conf includes the directory (want include and *.conf):\n%s", c)
	}
	for _, key := range []string{"enabled", "includeDir", "includeSuffix", "disabledSuffix", "service",
		"testTimeout", "stateTimeout", "reloadVerifyTimeout", "maxConfigBytes"} {
		k := keyNode(doc.Content[0], "providers.nginx."+key)
		if k == nil {
			t.Errorf("providers.nginx.%s is missing from the default YAML", key)
			continue
		}
		v := valueNode(doc.Content[0], "providers.nginx."+key)
		if k.HeadComment == "" && k.LineComment == "" && v.LineComment == "" {
			t.Errorf("providers.nginx.%s has no comment", key)
		}
	}

	if names := providerConfigNames(t); !names["nginx"] {
		t.Errorf("providerConfigs in internal/daemon/providers.go has keys %v, want one named nginx", names)
	}
}

// nginx-provider TC-E15: the boundary values of the nginx section, with the
// default limits.maxBodyBytes. Requirements: FR-2, L-17.
func TestNginxConfigBoundaries(t *testing.T) {
	maxBody := Default().Limits.MaxBodyBytes
	cases := []struct {
		name    string
		section string
		refused bool
	}{
		{"maxConfigBytes equal to limits.maxBodyBytes", fmt.Sprintf("maxConfigBytes: %d", maxBody), false},
		{"maxConfigBytes one above limits.maxBodyBytes", fmt.Sprintf("maxConfigBytes: %d", maxBody+1), true},
		{"disabledSuffix .off", "disabledSuffix: .off", false},
		{"includeSuffix .site with disabledSuffix .disabled", "includeSuffix: .site\n    disabledSuffix: .disabled", false},
		{"testTimeout 1ns", "testTimeout: 1ns", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := "providers:\n  nginx:\n    " + tc.section + "\n"
			_, err := Parse([]byte(y), "c.yaml")
			if !tc.refused {
				if err != nil {
					t.Fatalf("Parse refused:\n%v", err)
				}
				return
			}
			var list ErrorList
			if !errors.As(err, &list) || len(list) != 1 {
				t.Fatalf("Parse error = %v, want one located error", err)
			}
			e := list[0]
			if e.Path != "providers.nginx.maxConfigBytes" || e.Line != 3 || e.Column != 21 {
				t.Errorf("error %v at %s %d:%d, want providers.nginx.maxConfigBytes 3:21", e, e.Path, e.Line, e.Column)
			}
			if !strings.Contains(e.Msg, "limits.maxBodyBytes") {
				t.Errorf("message %q does not name limits.maxBodyBytes", e.Msg)
			}
		})
	}
}

// Every FR-2 refusal, each located at the offending value.
func TestNginxValidationRules(t *testing.T) {
	cases := []struct {
		name, section, path, msg string
	}{
		{"relative includeDir", "includeDir: etc/nginx", "providers.nginx.includeDir", "absolute"},
		{"includeSuffix without a dot", "includeSuffix: conf", "providers.nginx.includeSuffix", `^\.[A-Za-z0-9]+$`},
		{"includeSuffix with a slash", `includeSuffix: ".co/nf"`, "providers.nginx.includeSuffix", `^\.[A-Za-z0-9]+$`},
		{"empty includeSuffix", `includeSuffix: ""`, "providers.nginx.includeSuffix", "required"},
		{"empty disabledSuffix", `disabledSuffix: ""`, "providers.nginx.disabledSuffix", "required"},
		{"disabledSuffix not a dot form", `disabledSuffix: "-off"`, "providers.nginx.disabledSuffix", `^\.[A-Za-z0-9]+$`},
		{"disabledSuffix ending in includeSuffix", "disabledSuffix: .conf", "providers.nginx.disabledSuffix", "includeSuffix"},
		{"service with a shell metacharacter", `service: "nginx;x"`, "providers.nginx.service", "must match"},
		{"zero testTimeout", "testTimeout: 0s", "providers.nginx.testTimeout", "greater than zero"},
		{"negative stateTimeout", "stateTimeout: -1s", "providers.nginx.stateTimeout", "greater than zero"},
		{"zero reloadVerifyTimeout", "reloadVerifyTimeout: 0s", "providers.nginx.reloadVerifyTimeout", "greater than zero"},
		{"zero maxConfigBytes", "maxConfigBytes: 0", "providers.nginx.maxConfigBytes", "greater than zero"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("providers:\n  nginx:\n    "+tc.section+"\n"), "c.yaml")
			var list ErrorList
			if !errors.As(err, &list) {
				t.Fatalf("Parse error = %v, want an ErrorList", err)
			}
			for _, e := range list {
				if e.Path == tc.path && strings.Contains(e.Msg, tc.msg) {
					if e.Line != 3 || e.Column == 0 {
						t.Errorf("error %v is not located at the value on line 3", e)
					}
					return
				}
			}
			t.Errorf("no error at %s containing %q in:\n%v", tc.path, tc.msg, err)
		})
	}

	t.Run("an empty includeSuffix does not also fault disabledSuffix", func(t *testing.T) {
		_, err := Parse([]byte("providers:\n  nginx:\n    includeSuffix: \"\"\n"), "c.yaml")
		var list ErrorList
		if !errors.As(err, &list) || len(list) != 1 || list[0].Path != "providers.nginx.includeSuffix" {
			t.Fatalf("Parse error = %v, want only providers.nginx.includeSuffix", err)
		}
	})

	t.Run("an absolute includeDir is accepted", func(t *testing.T) {
		if _, err := Parse([]byte("providers:\n  nginx:\n    includeDir: /etc/nginx/http.d\n"), "c.yaml"); err != nil {
			t.Fatalf("Parse: %v", err)
		}
	})
}

// valueNode returns the value node of the dotted key path.
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

// providerConfigNames reads the keys of the map literal providerConfigs
// returns. It is unexported in package daemon, which imports this one.
func providerConfigNames(t *testing.T) map[string]bool {
	t.Helper()
	const src = "../daemon/providers.go"
	f, err := parser.ParseFile(token.NewFileSet(), src, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "providerConfigs" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					names[s] = true
				}
			}
			return false
		})
	}
	if len(names) == 0 {
		t.Fatalf("no providerConfigs map literal found in %s", src)
	}
	return names
}
