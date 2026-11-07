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
