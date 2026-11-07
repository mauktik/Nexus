package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// nexusdBin is built once by TestMain; cmd/ is tested by executing the binary.
var nexusdBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nexusd-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	nexusdBin = filepath.Join(dir, "nexusd")
	build := exec.Command("go", "build", "-o", nexusdBin, ".")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build nexusd:", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func runNexusd(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(nexusdBin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), exitErr.ExitCode()
	}
	t.Fatalf("start nexusd %v: %v", args, err)
	return "", -1
}

func writeConfig(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TC-10 (CLI half): validate-config accepts the shipped default config.
func TestValidateConfigAcceptsDefault(t *testing.T) {
	out, code := runNexusd(t, "validate-config", "--config", "../../deploy/nexusd.default.yaml")
	if code != 0 {
		t.Fatalf("validate-config exited %d:\n%s", code, out)
	}
}

// TC-11: validate-config reports every error with its location, exits
// non-zero, and never opens the configured listener.
func TestValidateConfigReportsAllErrorsWithoutServing(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	content := fmt.Sprintf(`listen: [%q]
tls:
  mode: bogus
limits:
  maxBodyBytes: -1
audit:
  retentionDayz: 30
`, addr)
	path := writeConfig(t, "three-errors.yaml", content)

	var accepted atomic.Int32
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
				accepted.Add(1)
				_ = c.Close()
			}
			time.Sleep(time.Millisecond)
		}
	}()
	out, code := runNexusd(t, "validate-config", "--config", path)
	close(stop)
	wg.Wait()
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		accepted.Add(1)
		_ = c.Close()
	}

	if code == 0 {
		t.Errorf("validate-config exited 0, want non-zero:\n%s", out)
	}
	for _, want := range []string{
		path + ":3:9: tls.mode:",
		path + ":5:17: limits.maxBodyBytes:",
		path + ":7:3: audit.retentionDayz: unknown key",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := accepted.Load(); n != 0 {
		t.Errorf("port %s accepted %d connections during or after validate-config", addr, n)
	}
}

// TC-ERR2 (CLI): each misspelled key fails validate-config with the key named
// at exactly the node's line and column.
func TestValidateConfigUnknownKeys(t *testing.T) {
	fixtures := []struct{ path, key, yaml string }{
		{"listn", "listn", "listn: [\"127.0.0.1:7443\"]\n"},
		{"limits.maxBodyByte", "maxBodyByte", "limits:\n  maxBodyByte: 1024\n"},
		{"providers.docker.sockt", "sockt", "providers:\n  docker:\n    enabled: false\n    sockt: /run/docker.sock\n"},
		{"providers.scripts.catalog[0].timeouts", "timeouts", "providers:\n  scripts:\n    catalog:\n      - id: deploy\n        path: /opt/deploy.sh\n        timeouts: 1h\n"},
		{"providers.scripts.catalog[0].parameters[0].patern", "patern", "providers:\n  scripts:\n    catalog:\n      - id: deploy\n        path: /opt/deploy.sh\n        parameters:\n          - name: branch\n            type: string\n            patern: '^[a-z]+$'\n            arg: --branch\n"},
	}
	for _, f := range fixtures {
		t.Run(f.path, func(t *testing.T) {
			line, col := nodePosition(t, f.yaml, f.key)
			path := writeConfig(t, "typo.yaml", f.yaml)
			out, code := runNexusd(t, "validate-config", "--config", path)
			if code == 0 {
				t.Fatalf("validate-config exited 0:\n%s", out)
			}
			want := fmt.Sprintf("%s:%d:%d: %s: unknown key %q", path, line, col, f.path, f.key)
			if !strings.Contains(out, want) {
				t.Fatalf("output lacks %q:\n%s", want, out)
			}
		})
	}
}

// TC-ERR3 (CLI): malformed values exit non-zero with a located message and no
// panic; only the empty file is accepted.
func TestValidateConfigMalformed(t *testing.T) {
	dir := t.TempDir()
	rows := []struct {
		name, content, want string
		exists              bool
	}{
		{"syntax", "listen: [\"127.0.0.1:7443\"\ntls: {mode: auto}\n", ":", true},
		{"tls-mode", "tls:\n  mode: bogus\n", ":2:9: tls.mode:", true},
		{"duration", "providers:\n  apt:\n    lockWait: soon\n", ":3:15: providers.apt.lockWait:", true},
		{"negative", "limits: {maxBodyBytes: -1}\n", ":1:24: limits.maxBodyBytes:", true},
		{"missing", "", ": ", false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			path := filepath.Join(dir, r.name+".yaml")
			if r.exists {
				if err := os.WriteFile(path, []byte(r.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, code := runNexusd(t, "validate-config", "--config", path)
			if code == 0 {
				t.Fatalf("exited 0:\n%s", out)
			}
			if strings.Contains(out, "panic") || strings.Contains(out, "goroutine ") {
				t.Fatalf("panicked:\n%s", out)
			}
			if !strings.Contains(out, path+r.want) {
				t.Fatalf("output lacks %q:\n%s", path+r.want, out)
			}
		})
	}

	t.Run("empty", func(t *testing.T) {
		path := filepath.Join(dir, "empty.yaml")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, code := runNexusd(t, "validate-config", "--config", path); code != 0 {
			t.Fatalf("empty config exited %d:\n%s", code, out)
		}
	})
}

// nodePosition returns the line and column of the only key node named key.
func nodePosition(t *testing.T, fixture, key string) (int, int) {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fixture), &doc); err != nil {
		t.Fatal(err)
	}
	var found *yaml.Node
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == key {
					found = n.Content[i]
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	if found == nil {
		t.Fatalf("fixture has no key %q", key)
	}
	return found.Line, found.Column
}

// catalogEntry is a valid catalog entry, one key per line from line 4, so a
// row can drop or replace one line. Its parameter is on line 10.
var catalogEntry = []string{
	"providers:",
	"  scripts:",
	"    catalog:",
	"      - id: deploy",
	"        description: Deploy the API",
	"        path: /opt/deploy/deploy.sh",
	"        runAs: deploy",
	"        workingDirectory: /opt/deploy",
	"        parameters:",
	"          - {name: branch, type: string, pattern: '^[a-z]+$', arg: --branch}",
}

// catalogYAML returns catalogEntry with line n (1-based) replaced by with,
// or dropped when with is empty, plus extra lines.
func catalogYAML(n int, with string, extra ...string) string {
	var lines []string
	for i, l := range catalogEntry {
		switch {
		case i+1 != n:
			lines = append(lines, l)
		case with != "":
			lines = append(lines, with)
		}
	}
	return strings.Join(append(lines, extra...), "\n") + "\n"
}

// at is ":<line>:<column>: " for the first occurrence of token in text,
// which is line n of a fixture.
func at(n int, text, token string) string {
	return fmt.Sprintf(":%d:%d: ", n, strings.Index(text, token)+1)
}

// TC-ERR24: each catalog rule fails validate-config, which names the entry
// and field and gives the line and column: of the value at fault, of the
// unknown key, or of the entry when a required key is absent.
func TestValidateConfigCatalogRules(t *testing.T) {
	if out, code := runNexusd(t, "validate-config", "--config", writeConfig(t, "ok.yaml", catalogYAML(0, ""))); code != 0 {
		t.Fatalf("the valid base entry exited %d:\n%s", code, out)
	}
	const entry = ":4:9: " // The entry's first key, where an absent key is reported
	p := func(name string) string {
		return "          - {name: " + name + ", type: string, pattern: '^a$', arg: --k}"
	}
	noPattern := "          - {name: branch, type: string, arg: --branch}"
	sensitiveNoEnv := "          - {name: k, type: string, pattern: '^a$', sensitive: true}"
	sensitiveArg := "          - {name: k, type: string, pattern: '^a$', sensitive: true, env: K, arg: --k}"
	badRegex := "          - {name: k, type: string, pattern: '(', arg: --k}"
	badEnv := "          - {name: k, type: string, pattern: '^a$', env: 'A=B'}"
	rows := []struct {
		name, yaml, want string
	}{
		{"missing id", catalogYAML(4, "      - description: Deploy the API"), entry + "providers.scripts.catalog[0].id: required"},
		{"missing description", catalogYAML(5, ""), entry + "providers.scripts.catalog[0].description: required"},
		{"missing path", catalogYAML(6, ""), entry + "providers.scripts.catalog[0].path: required"},
		{"missing runAs", catalogYAML(7, ""), entry + "providers.scripts.catalog[0].runAs: required"},
		{"missing workingDirectory", catalogYAML(8, ""), entry + "providers.scripts.catalog[0].workingDirectory: required"},
		{"relative path", catalogYAML(6, "        path: deploy.sh"),
			at(6, "        path: deploy.sh", "deploy.sh") + "providers.scripts.catalog[0].path: must be an absolute path"},
		{"relative workingDirectory", catalogYAML(8, "        workingDirectory: opt"),
			at(8, "        workingDirectory: opt", "opt") + "providers.scripts.catalog[0].workingDirectory: must be an absolute path"},
		{"bad id", catalogYAML(4, "      - id: ../x"), at(4, "      - id: ../x", "../x") + "providers.scripts.catalog[0].id: must match"},
		{"duplicate id", catalogYAML(0, "", "      - id: deploy", "        description: Again", "        path: /opt/b.sh",
			"        runAs: deploy", "        workingDirectory: /opt"),
			at(11, "      - id: deploy", "deploy") + `providers.scripts.catalog[1].id: duplicate script id "deploy"`},
		{"string parameter without pattern", catalogYAML(10, noPattern),
			at(10, noPattern, "{") + "providers.scripts.catalog[0].parameters[0].pattern: required for a string parameter"},
		{"sensitive without env", catalogYAML(10, sensitiveNoEnv),
			at(10, sensitiveNoEnv, "{") + "providers.scripts.catalog[0].parameters[0].env: required because the parameter is sensitive"},
		{"sensitive with arg", catalogYAML(10, sensitiveArg),
			at(10, sensitiveArg, "--k") + "providers.scripts.catalog[0].parameters[0].arg: not allowed because the parameter is sensitive"},
		{"invalid regex", catalogYAML(10, badRegex),
			at(10, badRegex, "'('") + "providers.scripts.catalog[0].parameters[0].pattern: not a valid regular expression"},
		{"bad parameter name", catalogYAML(10, p("-rf")),
			at(10, p("-rf"), "-rf") + "providers.scripts.catalog[0].parameters[0].name: must match"},
		{"bad env name", catalogYAML(10, badEnv),
			at(10, badEnv, "'A=B'") + "providers.scripts.catalog[0].parameters[0].env: must match"},
		{"bad catalog env name", catalogYAML(0, "", "        env: {'A B': x}"),
			at(11, "        env: {'A B': x}", "{") + `providers.scripts.catalog[0].env: variable name "A B" must match`},
		{"unknown key in entry", catalogYAML(0, "", "        timeoutz: 1h"),
			at(11, "        timeoutz: 1h", "timeoutz") + `providers.scripts.catalog[0].timeoutz: unknown key "timeoutz"`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			path := writeConfig(t, "catalog.yaml", r.yaml)
			out, code := runNexusd(t, "validate-config", "--config", path)
			if code == 0 {
				t.Fatalf("validate-config exited 0 for:\n%s", r.yaml)
			}
			if !strings.Contains(out, path+r.want) {
				t.Fatalf("output lacks %q:\n%s", path+r.want, out)
			}
		})
	}
}
