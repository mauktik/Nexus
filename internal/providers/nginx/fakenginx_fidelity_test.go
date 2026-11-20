// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Code review B, CR-B7: TC-113 compares the real nginx's error-log [alert]
// line with the constant fakenginxErrorLogLine of the T1 integration tests,
// which cannot run fakenginx (the harness container has no Go toolchain to
// build it). This test is the other half: it runs fakenginx and compares
// the line it prints with that same constant, so the fake, the constant and
// the real nginx cannot drift apart without a test failing.

package nginx

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// nginx-provider TC-113 (fake half): fakenginx's error-log [alert] line is the constant TC-113 checks real nginx against
func TestFakeNginxErrorLogAlertIsTheLineTC113ChecksAgainstRealNginx(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "integration", "nginx_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?m)^const fakenginxErrorLogLine = `([^`]+)`$").FindSubmatch(src)
	if m == nil {
		t.Fatal("test/integration/nginx_test.go has no `const fakenginxErrorLogLine = ...`")
	}
	want := string(m[1])

	root := t.TempDir()
	inc := filepath.Join(root, "conf.d")
	emptyDir(t, inc)
	base := fakeDirs(t, root, inc)
	// A log directory that is not there fails the probe for every user, root
	// included; only the errno differs from the captured one.
	base.ErrorLogDir = filepath.Join(root, "absent")
	nginx := filepath.Join(root, "nginx")
	installFakeNginx(t, nginx, base)
	r := runFake(t, nginx, nil, "-t")
	var got string
	for line := range strings.Lines(r.stderr) {
		if strings.Contains(line, "[alert]") {
			got = strings.TrimSpace(line)
		}
	}
	if got == "" {
		t.Fatalf("fakenginx -t printed no [alert] line for an error log directory that is not there:\n%s", r.stderr)
	}
	// The path and the errno are the parts a host decides.
	mask := regexp.MustCompile(`open\(\) "[^"]*" failed \([^)]*\)`)
	const masked = `open() "<error log>" failed (<errno>)`
	if mask.ReplaceAllString(got, masked) != mask.ReplaceAllString(want, masked) {
		t.Errorf("fakenginx prints %q, but TC-113 compares real nginx with %q", got, want)
	}
}
