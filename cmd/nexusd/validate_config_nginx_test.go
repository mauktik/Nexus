package main

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// nginx-provider TC-ERR8: validate-config refuses invalid nginx settings,
// all of them in one run, each at its line and column, and never serves.
// Requirements: FR-1, FR-2, CON-4.
func TestValidateConfigNginxRefusals(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	content := fmt.Sprintf(`listen: [%q]
providers:
  nginx:
    enabled: true
    includeDir: etc/nginx
    includeSuffix: conf
    disabledSuffix: .conf
    service: "nginx;x"
    testTimeout: 0s
    stateTimeout: -1s
    maxConfigBytes: 0
    includeGlob: "*.conf"
`, addr)
	path := writeConfig(t, "nginx.yaml", content)
	out, code := runNexusd(t, "validate-config", "--config", path)
	if code == 0 {
		t.Fatalf("validate-config exited 0:\n%s", out)
	}
	for _, key := range []string{"includeDir", "includeSuffix", "disabledSuffix", "service", "testTimeout", "stateTimeout", "maxConfigBytes"} {
		line, col := valuePosition(t, content, key)
		want := fmt.Sprintf("%s:%d:%d: providers.nginx.%s: ", path, line, col, key)
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	line, col := nodePosition(t, content, "includeGlob")
	if want := fmt.Sprintf("%s:%d:%d: providers.nginx.includeGlob: unknown key %q", path, line, col, "includeGlob"); !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Errorf("something listens on %s after validate-config", addr)
	}

	for _, tc := range []struct{ key, value string }{
		{"includeSuffix", `".co/nf"`},
		{"includeSuffix", `""`},
		{"disabledSuffix", `""`},
		{"disabledSuffix", `"-off"`},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			content := "providers:\n  nginx:\n    " + tc.key + ": " + tc.value + "\n"
			path := writeConfig(t, "nginx.yaml", content)
			out, code := runNexusd(t, "validate-config", "--config", path)
			if code == 0 {
				t.Fatalf("validate-config exited 0:\n%s", out)
			}
			want := fmt.Sprintf("%s:3:%d: providers.nginx.%s: ", path, 5+len(tc.key)+2, tc.key)
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		})
	}
}

// valuePosition returns the line and column of the value of the only key
// named key.
func valuePosition(t *testing.T, fixture, key string) (int, int) {
	t.Helper()
	for i, line := range strings.Split(fixture, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, key+": ") {
			return i + 1, len(line) - len(trimmed) + len(key) + 3
		}
	}
	t.Fatalf("fixture has no key %q", key)
	return 0, 0
}
