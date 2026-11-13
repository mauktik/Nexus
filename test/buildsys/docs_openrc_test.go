// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md TC-35, TC-36 (WI 3.3) and
// TC-104, TC-105 (WI 6.4). The bodies are complete: each owning developer
// deletes its t.Skip line. An un-skipped passing test is the GREEN signal
// (test-strategy.md, RED Stub Style).

package buildsys

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// section returns the Markdown section whose heading contains title, up to
// the next heading of the same or a higher level.
func section(doc, title string) string {
	lines := strings.Split(doc, "\n")
	start, level := -1, 0
	for i, l := range lines {
		m := regexp.MustCompile(`^(#+)\s+(.*)$`).FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if start < 0 {
			if strings.Contains(m[2], title) {
				start, level = i, len(m[1])
			}
			continue
		}
		if len(m[1]) <= level {
			return strings.Join(lines[start:i], "\n")
		}
	}
	if start < 0 {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

// unitDirectives returns every Key= of deploy/nexusd.service's [Service]
// section, read from the file itself so the expectation follows the unit.
func unitDirectives(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(repoRoot(t), "deploy", "nexusd.service"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var keys []string
	seen := map[string]bool{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "[") {
			in = l == "[Service]"
			continue
		}
		if !in || l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		if k, _, ok := strings.Cut(l, "="); ok && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		t.Fatal("no directives parsed from deploy/nexusd.service")
	}
	return keys
}

// nginx-provider TC-35: security.md lists every unit directive with its
// OpenRC outcome. Owner WI 3.3. Requirements: FR-27, CON-9, ASM-18, ASM-19.
func TestSecurityDocCoversEveryUnitDirective(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-35 (WI 3.3)")

	sec := section(readDoc(t, "docs/security.md"), "OpenRC hosts")
	if sec == "" {
		t.Fatal(`docs/security.md has no section headed "OpenRC hosts"`)
	}
	outcome := regexp.MustCompile(`(?i)\b(replaced|lost|not applicable)\b`)
	rows := map[string]string{}
	for _, l := range strings.Split(sec, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "|") {
			continue
		}
		for _, k := range unitDirectives(t) {
			if strings.Contains(l, "`"+k+"`") || strings.Contains(l, "| "+k+" ") {
				rows[k] = l
			}
		}
	}
	for _, k := range unitDirectives(t) {
		row, ok := rows[k]
		if !ok {
			t.Errorf("no row for unit directive %s", k)
			continue
		}
		if !outcome.MatchString(row) {
			t.Errorf("row for %s states no outcome (replaced, lost or not applicable): %s", k, row)
		}
	}
	want := map[string]string{
		"Restart":               "replaced.*supervise-daemon",
		"UMask":                 "replaced.*umask",
		"Type":                  "replaced.*pidfile",
		"RefuseManualStop":      "not applicable",
		"NoNewPrivileges":       "lost",
		"ProtectSystem":         "lost",
		"SystemCallFilter":      "lost",
		"CapabilityBoundingSet": "lost",
	}
	for k, re := range want {
		if row, ok := rows[k]; ok && !regexp.MustCompile("(?i)"+re).MatchString(row) {
			t.Errorf("row for %s does not match %q: %s", k, re, row)
		}
	}
	if !strings.Contains(sec, "unconfined root") {
		t.Error(`section does not say nexusd runs as "unconfined root"`)
	}
	if !regexp.MustCompile(`(?i)never a security boundary`).MatchString(sec) {
		t.Error("section does not restate that the sandbox was never a security boundary")
	}
}

// nginx-provider TC-36: install.md and README cover Alpine. Owner WI 3.3.
// Requirements: FR-27, ASM-20, CON-8.
func TestInstallDocCoversAlpine(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-36 (WI 3.3)")

	sec := section(readDoc(t, "docs/install.md"), "Alpine")
	if sec == "" {
		t.Fatal("docs/install.md has no Alpine section")
	}
	for _, s := range []string{"apk add", "rc-update add nexusd default", "rc-service nexusd start", "syslog"} {
		if !strings.Contains(sec, s) {
			t.Errorf("Alpine section lacks %q", s)
		}
	}
	if !regexp.MustCompile(`(?is)apt.{0,80}unavailable`).MatchString(sec) {
		t.Error("Alpine section does not say apt is unavailable")
	}
	if !regexp.MustCompile(`(?is)apk.{0,80}not managed|not managed.{0,80}apk`).MatchString(sec) {
		t.Error("Alpine section does not say apk packages are not managed (ASM-20)")
	}
	if !strings.Contains(readDoc(t, "README.md"), "OpenRC") {
		t.Error("README.md does not mention OpenRC")
	}
}

// nginx-provider TC-104: security.md lists the nginx scopes and the
// root-equivalence of nginx:config. Owner WI 6.4. Requirements: FR-14,
// CON-6, FR-15.
func TestSecurityDocNginxScopes(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-104 (WI 6.4)")

	doc := readDoc(t, "docs/security.md")
	for _, s := range []string{"nginx:read", "nginx:write", "nginx:config"} {
		if !regexp.MustCompile(`(?m)^\|.*` + "`" + regexp.QuoteMeta(s) + "`").MatchString(doc) {
			t.Errorf("scope table has no row for %s", s)
		}
	}
	root := section(doc, "potentially root-equivalent")
	if !strings.Contains(root, "nginx:config") {
		t.Fatal(`"Mutating scopes are potentially root-equivalent" does not name nginx:config`)
	}
	if !regexp.MustCompile(`(?i)perl|njs`).MatchString(root) || !regexp.MustCompile("(?i)alias|`root`").MatchString(root) {
		t.Error("nginx:config reason does not mention perl/njs and alias/root (CON-6)")
	}
	if !regexp.MustCompile(`(?is)REDACTED.{0,300}(sha-?256|digest)|(sha-?256|digest).{0,300}REDACTED`).MatchString(doc) {
		t.Error("no FR-15 note that content is redacted and its size and digest are in the operation log")
	}
}

// nginx-provider TC-105: docs/nginx.md documents the provider. Owner WI 6.4.
// Requirements: FR-16, ASM-12, CON-5.
func TestNginxDoc(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-105 (WI 6.4)")

	doc := readDoc(t, "docs/nginx.md")
	for _, s := range []string{"http.d", "conf.d", "include", ".conf.disabled", "ifMatch", "none", "whatIf",
		"outcome-unknown", "previous/content", "128 KiB", "root-equivalent"} {
		if !strings.Contains(doc, s) {
			t.Errorf("docs/nginx.md lacks %q", s)
		}
	}
	if !strings.Contains(readDoc(t, "README.md"), "nginx") {
		t.Error("README.md does not list the nginx provider")
	}
}
