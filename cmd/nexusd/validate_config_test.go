package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/daemon"
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

// CR-9: validate-config applies the checks the providers make on their own
// sections, so a file it accepts cannot make nexusd exit at boot. The docker
// section is checked even while the provider is disabled, as at startup.
func TestValidateConfigRunsProviderConfigureChecks(t *testing.T) {
	for _, tc := range []struct {
		name, socket string
	}{
		{"path longer than sun_path", "/" + strings.Repeat("a", 200) + ".sock"},
		{"url inside an absolute path", "/run/docker.sock://x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "docker.yaml", fmt.Sprintf("providers:\n  docker:\n    enabled: false\n    socket: %q\n", tc.socket))
			out, code := runNexusd(t, "validate-config", "--config", path)
			if code == 0 || strings.Contains(out, "configuration is valid") {
				t.Fatalf("validate-config exited %d for an unusable socket:\n%s", code, out)
			}
			if !strings.Contains(out, "docker") {
				t.Errorf("output does not name the provider:\n%s", out)
			}
			// Startup must refuse the same file.
			if sout, scode := runNexusd(t, "serve", "--config", path, "--state-dir", t.TempDir(), "--audit-dir", t.TempDir()); scode == 0 {
				t.Errorf("serve accepted what validate-config rejects:\n%s", sout)
			}
		})
	}
}

// tcsm44Fixture is one configuration the tests above give validate-config.
// file is a fixed path; otherwise content is written to a temp file, or no
// file is written when missing is set.
type tcsm44Fixture struct {
	name, file, content string
	missing             bool
}

// tcsm44Fixtures are the fixtures of the validate-config tests above, with
// the listen address of TC-11 fixed so that the output is too. The docker
// fixtures of CR-9 are in TestTCSM44ProviderErrorAtItsPath.
func tcsm44Fixtures() []tcsm44Fixture {
	p := func(name string) string {
		return "          - {name: " + name + ", type: string, pattern: '^a$', arg: --k}"
	}
	return []tcsm44Fixture{
		{name: "default", file: "../../deploy/nexusd.default.yaml"},
		{name: "three-errors", content: "listen: [\"127.0.0.1:1\"]\ntls:\n  mode: bogus\nlimits:\n  maxBodyBytes: -1\naudit:\n  retentionDayz: 30\n"},
		{name: "unknown listn", content: "listn: [\"127.0.0.1:7443\"]\n"},
		{name: "unknown maxBodyByte", content: "limits:\n  maxBodyByte: 1024\n"},
		{name: "unknown sockt", content: "providers:\n  docker:\n    enabled: false\n    sockt: /run/docker.sock\n"},
		{name: "unknown timeouts", content: "providers:\n  scripts:\n    catalog:\n      - id: deploy\n        path: /opt/deploy.sh\n        timeouts: 1h\n"},
		{name: "unknown patern", content: "providers:\n  scripts:\n    catalog:\n      - id: deploy\n        path: /opt/deploy.sh\n        parameters:\n          - name: branch\n            type: string\n            patern: '^[a-z]+$'\n            arg: --branch\n"},
		{name: "syntax", content: "listen: [\"127.0.0.1:7443\"\ntls: {mode: auto}\n"},
		{name: "tls-mode", content: "tls:\n  mode: bogus\n"},
		{name: "duration", content: "providers:\n  apt:\n    lockWait: soon\n"},
		{name: "negative", content: "limits: {maxBodyBytes: -1}\n"},
		{name: "missing", missing: true},
		{name: "empty", content: ""},
		{name: "catalog ok", content: catalogYAML(0, "")},
		{name: "missing id", content: catalogYAML(4, "      - description: Deploy the API")},
		{name: "missing description", content: catalogYAML(5, "")},
		{name: "missing path", content: catalogYAML(6, "")},
		{name: "missing runAs", content: catalogYAML(7, "")},
		{name: "missing workingDirectory", content: catalogYAML(8, "")},
		{name: "relative path", content: catalogYAML(6, "        path: deploy.sh")},
		{name: "relative workingDirectory", content: catalogYAML(8, "        workingDirectory: opt")},
		{name: "bad id", content: catalogYAML(4, "      - id: ../x")},
		{name: "duplicate id", content: catalogYAML(0, "", "      - id: deploy", "        description: Again", "        path: /opt/b.sh",
			"        runAs: deploy", "        workingDirectory: /opt")},
		{name: "string parameter without pattern", content: catalogYAML(10, "          - {name: branch, type: string, arg: --branch}")},
		{name: "sensitive without env", content: catalogYAML(10, "          - {name: k, type: string, pattern: '^a$', sensitive: true}")},
		{name: "sensitive with arg", content: catalogYAML(10, "          - {name: k, type: string, pattern: '^a$', sensitive: true, env: K, arg: --k}")},
		{name: "invalid regex", content: catalogYAML(10, "          - {name: k, type: string, pattern: '(', arg: --k}")},
		{name: "bad parameter name", content: catalogYAML(10, p("-rf"))},
		{name: "bad env name", content: catalogYAML(10, "          - {name: k, type: string, pattern: '^a$', env: 'A=B'}")},
		{name: "bad catalog env name", content: catalogYAML(0, "", "        env: {'A B': x}")},
		{name: "unknown key in entry", content: catalogYAML(0, "", "        timeoutz: 1h")},
	}
}

// path returns the fixture's --config argument, writing it under dir.
func (f tcsm44Fixture) path(t *testing.T, dir string) string {
	t.Helper()
	if f.file != "" {
		return f.file
	}
	path := filepath.Join(dir, "fixture.yaml")
	if f.missing {
		return path
	}
	if err := os.WriteFile(path, []byte(f.content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tcsm44Golden is validate-config's output for each fixture, captured from
// base d04583a (`git archive d04583a`, with this file's fixtures run against
// the nexusd built from that tree). $CONFIG stands for the --config path.
var tcsm44Golden = map[string]struct {
	stdout, stderr string
	code           int
}{
	"default":                          {"$CONFIG: configuration is valid\n", "", 0},
	"three-errors":                     {"", "$CONFIG:3:9: tls.mode: must be one of auto, files or off, got \"bogus\"\n$CONFIG:5:17: limits.maxBodyBytes: must be greater than zero, got -1\n$CONFIG:7:3: audit.retentionDayz: unknown key \"retentionDayz\"\n", 1},
	"unknown listn":                    {"", "$CONFIG:1:1: listn: unknown key \"listn\"\n", 1},
	"unknown maxBodyByte":              {"", "$CONFIG:2:3: limits.maxBodyByte: unknown key \"maxBodyByte\"\n", 1},
	"unknown sockt":                    {"", "$CONFIG:4:5: providers.docker.sockt: unknown key \"sockt\"\n", 1},
	"unknown timeouts":                 {"", "$CONFIG:4:9: providers.scripts.catalog[0].description: required\n$CONFIG:4:9: providers.scripts.catalog[0].runAs: required\n$CONFIG:4:9: providers.scripts.catalog[0].workingDirectory: required\n$CONFIG:6:9: providers.scripts.catalog[0].timeouts: unknown key \"timeouts\"\n", 1},
	"unknown patern":                   {"", "$CONFIG:4:9: providers.scripts.catalog[0].description: required\n$CONFIG:4:9: providers.scripts.catalog[0].runAs: required\n$CONFIG:4:9: providers.scripts.catalog[0].workingDirectory: required\n$CONFIG:7:13: providers.scripts.catalog[0].parameters[0].pattern: required for a string parameter without an enum\n$CONFIG:9:13: providers.scripts.catalog[0].parameters[0].patern: unknown key \"patern\"\n", 1},
	"syntax":                           {"", "$CONFIG:1: invalid YAML: did not find expected ',' or ']'\n", 1},
	"tls-mode":                         {"", "$CONFIG:2:9: tls.mode: must be one of auto, files or off, got \"bogus\"\n", 1},
	"duration":                         {"", "$CONFIG:3:15: providers.apt.lockWait: invalid duration \"soon\": want a string such as \"30s\" or \"1h\"\n", 1},
	"negative":                         {"", "$CONFIG:1:24: limits.maxBodyBytes: must be greater than zero, got -1\n", 1},
	"missing":                          {"", "$CONFIG: cannot read configuration: no such file or directory\n", 1},
	"empty":                            {"$CONFIG: configuration is valid\n", "", 0},
	"catalog ok":                       {"$CONFIG: configuration is valid\n", "", 0},
	"missing id":                       {"", "$CONFIG:4:9: providers.scripts.catalog[0].id: required\n$CONFIG:5:9: providers.scripts.catalog[0].description: key \"description\" already defined at line 4\n", 1},
	"missing description":              {"", "$CONFIG:4:9: providers.scripts.catalog[0].description: required\n", 1},
	"missing path":                     {"", "$CONFIG:4:9: providers.scripts.catalog[0].path: required\n", 1},
	"missing runAs":                    {"", "$CONFIG:4:9: providers.scripts.catalog[0].runAs: required\n", 1},
	"missing workingDirectory":         {"", "$CONFIG:4:9: providers.scripts.catalog[0].workingDirectory: required\n", 1},
	"relative path":                    {"", "$CONFIG:6:15: providers.scripts.catalog[0].path: must be an absolute path, got \"deploy.sh\"\n", 1},
	"relative workingDirectory":        {"", "$CONFIG:8:27: providers.scripts.catalog[0].workingDirectory: must be an absolute path, got \"opt\"\n", 1},
	"bad id":                           {"", "$CONFIG:4:13: providers.scripts.catalog[0].id: must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$, got \"../x\"\n", 1},
	"duplicate id":                     {"", "$CONFIG:11:13: providers.scripts.catalog[1].id: duplicate script id \"deploy\"\n", 1},
	"string parameter without pattern": {"", "$CONFIG:10:13: providers.scripts.catalog[0].parameters[0].pattern: required for a string parameter without an enum\n", 1},
	"sensitive without env":            {"", "$CONFIG:10:13: providers.scripts.catalog[0].parameters[0].env: required because the parameter is sensitive\n", 1},
	"sensitive with arg":               {"", "$CONFIG:10:83: providers.scripts.catalog[0].parameters[0].arg: not allowed because the parameter is sensitive\n", 1},
	"invalid regex":                    {"", "$CONFIG:10:46: providers.scripts.catalog[0].parameters[0].pattern: not a valid regular expression: error parsing regexp: missing closing ): `(`\n", 1},
	"bad parameter name":               {"", "$CONFIG:10:20: providers.scripts.catalog[0].parameters[0].name: must match ^[A-Za-z][A-Za-z0-9_]{0,63}$, got \"-rf\"\n", 1},
	"bad env name":                     {"", "$CONFIG:10:58: providers.scripts.catalog[0].parameters[0].env: must match ^[A-Za-z_][A-Za-z0-9_]*$, got \"A=B\"\n", 1},
	"bad catalog env name":             {"", "$CONFIG:11:14: providers.scripts.catalog[0].env: variable name \"A B\" must match ^[A-Za-z_][A-Za-z0-9_]*$\n", 1},
	"unknown key in entry":             {"", "$CONFIG:11:9: providers.scripts.catalog[0].timeoutz: unknown key \"timeoutz\"\n", 1},
}

// TC-SM-44: on every fixture of the tests above, validate-config's stdout,
// stderr and exit code are byte-identical to base d04583a's.
func TestTCSM44_ExistingValidateConfigOutputStaysByte(t *testing.T) {
	for _, f := range tcsm44Fixtures() {
		t.Run(f.name, func(t *testing.T) {
			golden, ok := tcsm44Golden[f.name]
			if !ok {
				t.Fatalf("no golden for %q", f.name)
			}
			path := f.path(t, t.TempDir())
			stdout, stderr, code := runBounded(t, "validate-config", "--config", path)
			expand := func(s string) string { return strings.ReplaceAll(s, "$CONFIG", path) }
			if stdout != expand(golden.stdout) || stderr != expand(golden.stderr) || code != golden.code {
				t.Fatalf("got exit %d\nstdout %q\nstderr %q\nwant exit %d\nstdout %q\nstderr %q",
					code, stdout, stderr, golden.code, expand(golden.stdout), expand(golden.stderr))
			}
		})
	}
}

// TC-SM-44, CR-9 fixtures: a provider's error is the one line WI 3.2
// changes. It is now an entry at providers.docker like every other, so the
// base d04583a line gains that path and still names docker.
func TestTCSM44ProviderErrorAtItsPath(t *testing.T) {
	long := "/" + strings.Repeat("a", 200) + ".sock"
	for _, tc := range []struct {
		name, socket, base string // base: d04583a's stderr
	}{
		{"path longer than sun_path", long,
			fmt.Sprintf("$CONFIG: provider \"docker\": providers.docker.socket: unix socket path %q is too long\n", long)},
		{"url inside an absolute path", "/run/docker.sock://x",
			"$CONFIG: provider \"docker\": providers.docker.socket: must be the absolute path of the Docker Engine's Unix socket, not a URL; got \"/run/docker.sock://x\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "docker.yaml", fmt.Sprintf("providers:\n  docker:\n    enabled: false\n    socket: %q\n", tc.socket))
			stdout, stderr, code := runBounded(t, "validate-config", "--config", path)
			want := strings.ReplaceAll(strings.Replace(tc.base, "$CONFIG: ", "$CONFIG: providers.docker: ", 1), "$CONFIG", path)
			if code != 1 || stdout != "" || stderr != want {
				t.Fatalf("exit %d\nstdout %q\nstderr %q\nwant exit 1 and stderr %q", code, stdout, stderr, want)
			}
		})
	}
}

// checkconfigFixture is a fixture shared with internal/daemon's CheckConfig
// tests.
func checkconfigFixture(name string) string {
	return filepath.Join("..", "..", "internal", "daemon", "testdata", "checkconfig", name)
}

// TC-SM-43 (CLI half): validate-config prints exactly the e.Error() of each
// entry CheckConfig returns, one per line on stderr, and exits 1; for a
// valid file, the unchanged success line and exit 0. The CheckConfig half
// is in internal/daemon/validate_test.go.
func TestTCSM43_ValidateConfigPrintsCheckConfigEntries(t *testing.T) {
	for _, name := range []string{"valid.yaml", "unknown-key.yaml", "bad-listen.yaml", "provider-error.yaml"} {
		t.Run(name, func(t *testing.T) {
			path := checkconfigFixture(name)
			_, errs := daemon.CheckConfig(path)
			var stdout, stderr bytes.Buffer
			code := run([]string{"validate-config", "--config", path}, &stdout, &stderr)
			if len(errs) == 0 {
				if name != "valid.yaml" {
					t.Fatalf("CheckConfig found no problem in %s", name)
				}
				if code != 0 || stdout.String() != path+": configuration is valid\n" || stderr.Len() != 0 {
					t.Fatalf("exit %d\nstdout %q\nstderr %q", code, stdout.String(), stderr.String())
				}
				return
			}
			want := make([]string, len(errs))
			for i, e := range errs {
				if e.File != path {
					t.Errorf("entry %+v: File is not %q", *e, path)
				}
				want[i] = e.Error()
			}
			got := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			slices.Sort(want)
			slices.Sort(got)
			if code != 1 || stdout.Len() != 0 || !slices.Equal(got, want) {
				t.Fatalf("exit %d\nstdout %q\nstderr lines %q\nwant exit 1 and lines %q", code, stdout.String(), got, want)
			}
			switch name {
			case "unknown-key.yaml", "bad-listen.yaml":
				if errs[0].Line == 0 || errs[0].Column == 0 {
					t.Errorf("entry %+v has no line and column although the parser knows them", *errs[0])
				}
			case "provider-error.yaml":
				if errs[0].Path != "providers.docker" {
					t.Errorf("entry %+v: Path is not providers.docker", *errs[0])
				}
			}
		})
	}
}

// TC-SM-ERR24 (CLI half): validate-config reports a provider error at
// providers.docker and never prints a catalog env value, whether the file
// fails config.Load or only the providers' checks.
func TestTCSMERR24_ValidateConfigNeverEchoesCatalogEnv(t *testing.T) {
	const sentinel = "SENTINEL-7f3a9c-DO-NOT-ECHO"
	for _, tc := range []struct{ name, want string }{
		{"secret-sentinel.yaml", `listen[0]: "nope" is not a host:port address`},
		{"secret-sentinel-provider.yaml", `: providers.docker: provider "docker": `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := checkconfigFixture(tc.name)
			if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), sentinel) {
				t.Fatalf("fixture lacks the sentinel (err %v)", err)
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"validate-config", "--config", path}, &stdout, &stderr)
			out := stdout.String() + stderr.String()
			if code != 1 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, want 1 and %q in:\n%s", code, tc.want, out)
			}
			if strings.Contains(out, sentinel) {
				t.Fatalf("the catalog env value is echoed:\n%s", out)
			}
		})
	}
}
