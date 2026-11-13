package buildsys

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mdSection returns the part of a Markdown document from the heading line
// that is exactly heading to the next heading of the same or a higher level,
// or "" when there is no such heading.
func mdSection(doc, heading string) string {
	level := strings.IndexFunc(heading, func(r rune) bool { return r != '#' })
	lines := strings.Split(doc, "\n")
	for i, l := range lines {
		if strings.TrimRight(l, " ") != heading {
			continue
		}
		end := len(lines)
		inFence := false
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "```") {
				inFence = !inFence
			}
			if inFence {
				continue
			}
			if n := strings.IndexFunc(lines[j], func(r rune) bool { return r != '#' }); n > 0 && n <= level &&
				strings.HasPrefix(lines[j][n:], " ") {
				end = j
				break
			}
		}
		return strings.Join(lines[i:end], "\n")
	}
	return ""
}

// headingAnchors returns the GitHub anchors of a Markdown document's
// headings: lower case, punctuation other than "-" and "_" dropped, spaces
// as "-".
func headingAnchors(doc string) map[string]bool {
	drop := regexp.MustCompile("[^a-z0-9 _-]")
	anchors := map[string]bool{}
	inFence := false
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
		}
		n := strings.IndexFunc(l, func(r rune) bool { return r != '#' })
		if inFence || n <= 0 || !strings.HasPrefix(l[n:], " ") {
			continue
		}
		text := drop.ReplaceAllString(strings.ToLower(strings.TrimSpace(l[n:])), "")
		anchors[strings.ReplaceAll(text, " ", "-")] = true
	}
	return anchors
}

// oneLine folds every run of white space to one space, so a phrase wrapped
// across lines still matches.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func requireAll(t *testing.T, where, text string, wants ...string) {
	t.Helper()
	folded := oneLine(text)
	for _, w := range wants {
		if !strings.Contains(folded, oneLine(w)) {
			t.Errorf("%s lacks %q", where, w)
		}
	}
}

// TC-SM-105 (WI 6.3; FR-18, CON-5, NFR-5, FR-15): the documentation covers
// the scopes, the signing key, recovery and the verbs.
func TestTCSM105_TheDocumentationCoversTheScopesThe(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string { return readFile(t, filepath.Join(root, filepath.FromSlash(rel))) }

	// security.md: the scopes, root-equivalence and the release signing key.
	security := read("docs/security.md")
	requireAll(t, "docs/security.md", security, "`nexusd:read`", "`nexusd:admin`", "root-equivalent")
	key := mdSection(security, "## The release signing key")
	if key == "" {
		t.Error(`docs/security.md has no "## The release signing key" section`)
	}
	requireAll(t, "docs/security.md's release signing key section", key,
		"`~/.config/nexus/release-signing.key`", "0600", "`SHA256SUMS`", "integrity", "not authentication",
		"Rotation", "stolen key")

	// install.md: the update flow, the manual first install and the recovery
	// steps at the anchor the helper's update-rollback-failed detail names.
	install := read("docs/install.md")
	requireAll(t, "docs/install.md", install, "nexus nexusd update --file", "make release-key",
		"first install of a version that includes the `nexusd` provider is manual")
	if strings.Contains(oneLine(install), "does not update itself") {
		t.Error(`docs/install.md still says "Nexus does not update itself"`)
	}
	anchors := headingAnchors(install)
	if !anchors["recovering-from-a-failed-update"] {
		t.Error("docs/install.md has no heading with the anchor recovering-from-a-failed-update")
	}
	// Every docs/install.md anchor the nexusd provider's messages cite is a
	// heading there; the helper's recoveryDoc is one of them.
	cited := map[string]bool{}
	citeRE := regexp.MustCompile(`docs/install\.md#([a-z0-9_-]+)`)
	srcs, err := filepath.Glob(filepath.Join(root, "internal", "providers", "nexusd", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range srcs {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range citeRE.FindAllStringSubmatch(string(b), -1) {
			cited[m[1]] = true
		}
	}
	if !cited["recovering-from-a-failed-update"] {
		t.Errorf("the nexusd provider cites install.md anchors %v, not recovering-from-a-failed-update", cited)
	}
	for a := range cited {
		if !anchors[a] {
			t.Errorf("internal/providers/nexusd cites docs/install.md#%s, which is no heading there", a)
		}
	}
	recovery := mdSection(install, "## Recovering from a failed update")
	requireAll(t, "docs/install.md's recovery section", recovery,
		"sudo cp /usr/local/bin/nexusd.prev /usr/local/bin/nexusd",
		"sudo cp /var/lib/nexus/rollback/nexusd.service /etc/systemd/system/nexusd.service",
		"sudo systemctl daemon-reload",
		"sudo systemctl reset-failed nexusd.service",
		"sudo systemctl restart nexusd.service",
		"/usr/local/bin/nexusd -version",
		"nexusd.swap.tmp")

	// mcp.md: the two tools and the budget TC-SM-96 measures.
	mcp := read("docs/mcp.md")
	requireAll(t, "docs/mcp.md", mcp, "`get_nexusd`", "`validate_nexusd_config`")
	out, code := run(t, root, nil, "go", "test", "-count=1", "-run", "^TestTCSM96_", "-v", "./internal/mcpadapter")
	m := regexp.MustCompile(`(\d+) tools, (\d+) tokens;`).FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("TC-SM-96 exited %d without its measurement:\n%s", code, out)
	}
	stated := regexp.MustCompile(`measured at \*\*([0-9,]+) tokens\*\* across (\d+) tools`).
		FindStringSubmatch(oneLine(mcp))
	if stated == nil {
		t.Fatal(`docs/mcp.md does not state the budget as "measured at **N tokens** across M tools"`)
	}
	if got := strings.ReplaceAll(stated[1], ",", ""); got != m[2] || stated[2] != m[1] {
		t.Errorf("docs/mcp.md states %s tokens across %s tools; TC-SM-96 measures %s tokens across %s tools",
			stated[1], stated[2], m[2], m[1])
	}

	// adding-a-provider.md: the verbs and the four generic core mechanisms.
	guide := read("docs/adding-a-provider.md")
	requireAll(t, "docs/adding-a-provider.md", guide, "`reload`", "`rollback`", "`validate-config`",
		"`def.KindUpload`", "`CLI.Upload`", "`CLI.ReconnectWindow`", "`provider.DaemonUser`", "`audit.AddFact`")
	if strings.Contains(oneLine(guide), "Core, server, OpenAPI, MCP and CLI code do not change.") {
		t.Error("docs/adding-a-provider.md still says core code does not change, without the exceptions")
	}

	// The nexus-server-manager requirements: OOS-11, FR-22, FR-29 and NFR-4.
	nsm := read("planning/nexus-server-manager/requirements.md")
	for _, c := range []struct {
		heading string
		wants   []string
	}{
		{"### OOS-11: Self-update and fleet rollout", []string{"nexusd-self-management", "in scope", "fleet"}},
		{"### FR-22: Scoped authorization", []string{"`nexusd:read`", "`nexusd:admin`"}},
		{"### FR-29: Noun-verb command grammar", []string{"`reload`", "`rollback`", "`validate-config`"}},
		{"### NFR-4: Security baseline", []string{"`nexusd:admin`", "root-equivalent"}},
	} {
		s := mdSection(nsm, c.heading)
		if s == "" {
			t.Errorf("the NSM requirements lack %q", c.heading)
			continue
		}
		requireAll(t, "NSM "+c.heading, s, c.wants...)
	}
}
