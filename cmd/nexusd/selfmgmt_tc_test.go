// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TC-SM-61 (WI 4.1; CON-2): The hidden service-helper command is dispatched by cmd/nexusd
func TestTCSM61_TheHiddenServiceHelperCommandIs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.json"), []byte(`{"mode":"bogus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"service-helper", "--dir", dir}, &stdout, &stderr); code != 1 {
		t.Errorf("service-helper with mode bogus: exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `unknown mode "bogus"`) {
		t.Errorf("stderr %q, want unknown mode \"bogus\"", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("result.json: %v, want none written", err)
	}

	stdout.Reset()
	stderr.Reset()
	run([]string{"-h"}, &stdout, &stderr)
	usage := stdout.String() + stderr.String()
	if !strings.Contains(usage, "Usage") {
		t.Fatalf("nexusd -h printed no usage: %q", usage)
	}
	if strings.Contains(usage, "service-helper") {
		t.Errorf("the usage mentions the hidden service-helper:\n%s", usage)
	}
}
