// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-90, TC-RA-91 and
// TC-RA-92, owned by WI 5.1: the hand-written documentation of remote token
// and configuration management, the generated reference, and the shipped
// default configuration.
//
// TC-RA-E25, owned by WI 5.2, checks the T1 harness's copy of the
// hand-commented configuration fixture.

package nexusd

import (
	"errors"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// docsRoot is the repository root, three directories above this package.
func docsRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func docsRead(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docsRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// docsFold folds every run of white space to one space, so a phrase wrapped
// across lines still matches.
func docsFold(s string) string { return strings.Join(strings.Fields(s), " ") }

// docsRequire reports each of wants that text lacks, after folding both.
func docsRequire(t *testing.T, where, text string, wants ...string) {
	t.Helper()
	folded := docsFold(text)
	for _, w := range wants {
		if !strings.Contains(folded, docsFold(w)) {
			t.Errorf("%s lacks %q", where, w)
		}
	}
}

// docsHeadingLevel is the number of leading '#' of a Markdown heading line
// followed by a space, or 0.
func docsHeadingLevel(line string) int {
	n := strings.IndexFunc(line, func(r rune) bool { return r != '#' })
	if n <= 0 || !strings.HasPrefix(line[n:], " ") {
		return 0
	}
	return n
}

// docsSection returns the part of a Markdown document from the heading line
// that is exactly heading to the next heading of the same or a higher
// level, outside code fences, or "" when there is no such heading.
func docsSection(doc, heading string) string {
	level := docsHeadingLevel(heading)
	lines := strings.Split(doc, "\n")
	for i, l := range lines {
		if strings.TrimRight(l, " ") != heading {
			continue
		}
		end, fence := len(lines), false
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "```") {
				fence = !fence
			}
			if n := docsHeadingLevel(lines[j]); !fence && n > 0 && n <= level {
				end = j
				break
			}
		}
		return strings.Join(lines[i:end], "\n")
	}
	return ""
}

// docsAnchors returns the GitHub anchors of a Markdown document's headings:
// lower case, punctuation other than "-" and "_" dropped, spaces as "-".
func docsAnchors(doc string) map[string]bool {
	drop := regexp.MustCompile("[^a-z0-9 _-]")
	anchors := map[string]bool{}
	fence := false
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "```") {
			fence = !fence
		}
		n := docsHeadingLevel(l)
		if fence || n == 0 {
			continue
		}
		text := drop.ReplaceAllString(strings.ToLower(strings.TrimSpace(l[n:])), "")
		anchors[strings.ReplaceAll(text, " ", "-")] = true
	}
	return anchors
}

// TC-RA-90: The docs carry the deny-list, the recovery anchor and the reversed statements. Owner WI 5.1. Requirements: FR-14, FR-12, FR-7, CON-3, ASM-9, OOS-5, OOS-3.
func TestTCRA90_TheDocsCarryTheDenyListTheRecoveryAnchorAndThe(t *testing.T) {
	// security.md: every deny-list key, read from spec.ConfigDenyList, in a
	// table row; the reason path-valued keys are denied; lists; and neither
	// reversed statement.
	security := docsRead(t, "docs/security.md")
	var rows []string
	for _, l := range strings.Split(security, "\n") {
		if strings.HasPrefix(l, "|") {
			rows = append(rows, l)
		}
	}
	// The fourteen of FR-12, plus services-provider's
	// providers.services.reboot.enabled=true (A-19).
	if len(spec.ConfigDenyList) != 15 {
		t.Errorf("spec.ConfigDenyList has %d entries, want the fourteen of FR-12 and services-provider's one", len(spec.ConfigDenyList))
	}
	for _, d := range spec.ConfigDenyList {
		key := "`" + d.Key + "`"
		if !slices.ContainsFunc(rows, func(r string) bool { return strings.Contains(r, key) }) {
			t.Errorf("docs/security.md has no table row naming the deny-list key %s", key)
		}
	}
	why := docsSection(security, "### Why path-valued keys are denied")
	if why == "" {
		t.Error(`docs/security.md has no "### Why path-valued keys are denied" section`)
	}
	docsRequire(t, "docs/security.md's path-valued keys section", why, "NFR-5", "FR-25",
		"`providers.nginx.includeDir`", "`providers.nginx.includeSuffix`", "file name", "service")
	lists := docsSection(security, "### Lists and mappings are not settable")
	if lists == "" {
		t.Error(`docs/security.md has no "### Lists and mappings are not settable" section`)
	}
	docsRequire(t, "docs/security.md's lists section", lists, "`listen`", "OOS-3", "`tls.extraSANs`",
		"`mcp.allowedOrigins`")
	lower := strings.ToLower(docsFold(security))
	for _, gone := range []string{"No API or MCP call creates", "No API, MCP or CLI call writes"} {
		if strings.Contains(lower, strings.ToLower(gone)) {
			t.Errorf("docs/security.md still says %q", gone)
		}
	}

	// install.md: the three sections, the anchor the helper cites, the
	// normalization and no-change rules, and the bootstrap.
	install := docsRead(t, "docs/install.md")
	anchors := docsAnchors(install)
	cited := strings.TrimPrefix(configRecoveryDoc, "docs/install.md#")
	for _, a := range []string{"recovering-from-a-failed-configuration-change", cited, "manage-tokens-remotely",
		"change-the-configuration-remotely"} {
		if !anchors[a] {
			t.Errorf("docs/install.md has no heading with the anchor %s", a)
		}
	}
	change := docsSection(install, "## Change the configuration remotely")
	docsRequire(t, `docs/install.md's "Change the configuration remotely"`, change,
		"normalization", "only when a key is added", "blank lines", "`changed: false`", "no operation",
		"`restartRequired`", "/etc/nexus/nexusd.yaml.bak", "config-set-unavailable", "locked out")
	tokens := docsSection(install, "## Manage tokens remotely")
	docsRequire(t, `docs/install.md's "Manage tokens remotely"`, tokens,
		"providers:", "nexusd:", "enabled: true", "sudo nexusd token create", "--scope nexusd:admin",
		"--save-as-host", "nexus nexusdtoken add", "nexus nexusdtoken get", "nexus nexusdtoken remove",
		"locked out", "sudo nexusd token")
	recovery := docsSection(install, "## Recovering from a failed configuration change")
	docsRequire(t, "docs/install.md's configuration recovery section", recovery,
		"config-restore-failed", "sudo nexusd validate-config",
		"sudo cp /etc/nexus/nexusd.yaml.bak /etc/nexus/nexusd.yaml", "sudo systemctl reload nexusd.service",
		"journalctl -u nexusd.service")

	// mcp.md: the six op IDs, each named as not a tool.
	mcp := docsRead(t, "docs/mcp.md")
	ids := []string{spec.TokensList.Op().ID, spec.TokensAdd.Op().ID, spec.TokensRemove.Op().ID,
		spec.ConfigList.Op().ID, spec.ConfigGet.Op().ID, spec.UpdateConfig.Op().ID}
	found := false
	for _, l := range strings.Split(mcp, "\n") {
		if !strings.Contains(l, "not MCP tools") {
			continue
		}
		missing := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return strings.Contains(l, "`"+id+"`") })
		if len(missing) == 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("docs/mcp.md has no paragraph saying %q of all six ops %v", "not MCP tools", ids)
	}

	// adding-a-provider.md: § 7 names every generic mechanism.
	guide := docsSection(docsRead(t, "docs/adding-a-provider.md"),
		"## 7. Generic mechanisms added for remote administration")
	if guide == "" {
		t.Error(`docs/adding-a-provider.md has no "## 7. Generic mechanisms added for remote administration"`)
	}
	docsRequire(t, "docs/adding-a-provider.md § 7", guide, "SyncMutation", "SecretOutput", "Audited",
		"CompletesAtAdmission", "DetachedSpec.Admission", "Variadic", "EchoWarnings", "RetryAfter",
		"CheckConfigData", `json:"-"`, "reload-status.json")

	// internal/auth's package doc.
	fset := token.NewFileSet()
	dir := filepath.Join(docsRoot(t), "internal", "auth")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var pkgDoc strings.Builder
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if f.Doc != nil {
			pkgDoc.WriteString(f.Doc.Text())
		}
	}
	if pkgDoc.Len() == 0 {
		t.Fatal("internal/auth has no package doc")
	}
	if strings.Contains(strings.ToLower(docsFold(pkgDoc.String())), "no network path") {
		t.Errorf("internal/auth's package doc still says %q:\n%s", "no network path", pkgDoc.String())
	}
}

// TC-RA-91: Generated docs and the build gate are current. Owner WI 5.1. Requirements: FR-14, NFR-7.
func TestTCRA91_GeneratedDocsAndTheBuildGateAreCurrent(t *testing.T) {
	root := docsRoot(t)
	for pattern, want := range map[string]int{"nexus_nexusdtoken*.md": 4, "nexus_nexusdconfig*.md": 3} {
		got, err := filepath.Glob(filepath.Join(root, "docs", "cli", pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != want {
			t.Errorf("docs/cli/%s matches %d files, want %d: %v", pattern, len(got), want, got)
		}
	}
	if testing.Short() {
		t.Skip("-short: TestGeneratedDocsAreCurrent is not run")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go command to run TestGeneratedDocsAreCurrent: %v", err)
	}
	cmd := exec.Command(goBin, "test", "-count=1", "-run", "^TestGeneratedDocsAreCurrent$", "./cmd/nexus-gendocs")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TestGeneratedDocsAreCurrent failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("TestGeneratedDocsAreCurrent did not report ok:\n%s", out)
	}
}

// docsBaseCommit is the commit this feature branched from (planning
// test-cases.md TC-RA-92).
const docsBaseCommit = "521261c"

// docsKeyPaths returns the dotted path of every mapping key in v, with list
// elements as their index.
func docsKeyPaths(prefix string, v any, out map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out[p] = true
			docsKeyPaths(p, sub, out)
		}
	case []any:
		for i, sub := range x {
			docsKeyPaths(prefix+"["+strconv.Itoa(i)+"]", sub, out)
		}
	}
}

func docsDecode(t *testing.T, what string, data []byte) (any, map[string]bool) {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", what, err)
	}
	paths := map[string]bool{}
	docsKeyPaths("", v, paths)
	return v, paths
}

// TC-RA-92: The shipped default YAML gains comments only. Owner WI 5.1. Requirements: CON-5, FR-14.
func TestTCRA92_TheShippedDefaultYAMLGainsCommentsOnly(t *testing.T) {
	root := docsRoot(t)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is unavailable, so the base version cannot be read: %v", err)
	}
	git := func(args ...string) ([]byte, error) {
		cmd := exec.Command(gitBin, append([]string{"-C", root}, args...)...)
		return cmd.Output()
	}
	if _, err := git("rev-parse", "--git-dir"); err != nil {
		t.Skipf("%s is not a git checkout (a repository copy), so the base version cannot be read", root)
	}
	if _, err := git("cat-file", "-e", docsBaseCommit+"^{commit}"); err != nil {
		t.Skipf("the base commit %s is not in this clone (shallow?)", docsBaseCommit)
	}
	base, err := git("show", docsBaseCommit+":deploy/nexusd.default.yaml")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("git show %s:deploy/nexusd.default.yaml: %v\n%s", docsBaseCommit, err, ee.Stderr)
		}
		t.Fatal(err)
	}
	now, err := os.ReadFile(filepath.Join(root, "deploy", "nexusd.default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	baseV, basePaths := docsDecode(t, docsBaseCommit+":deploy/nexusd.default.yaml", base)
	nowV, nowPaths := docsDecode(t, "deploy/nexusd.default.yaml", now)
	// Sections added by later features are not remote-admin's to judge:
	// providers.services (services-provider WI 2.3) and providers.storage
	// (storage-provider WI 1.4).
	for _, name := range []string{"services", "storage"} {
		if m, ok := nowV.(map[string]any); ok {
			if p, ok := m["providers"].(map[string]any); ok {
				delete(p, name)
			}
		}
		maps.DeleteFunc(nowPaths, func(k string, _ bool) bool {
			return k == "providers."+name || strings.HasPrefix(k, "providers."+name+".")
		})
	}
	if !reflect.DeepEqual(baseV, nowV) {
		t.Errorf("deploy/nexusd.default.yaml decodes differently from %s's:\n got %v\nwant %v", docsBaseCommit, nowV, baseV)
	}
	if !maps.Equal(basePaths, nowPaths) {
		t.Errorf("deploy/nexusd.default.yaml's key paths differ from %s's:\n got %v\nwant %v", docsBaseCommit,
			slices.Sorted(maps.Keys(nowPaths)), slices.Sorted(maps.Keys(basePaths)))
	}
}

// TC-RA-E25 (WI 5.2; NFR-3): the hand-commented file TC-RA-96 installs on
// the T1 host is byte-for-byte the unit-test fixture config.Edit's NFR-3
// cases run on, so T1 checks the same comments T0 does.
func TestTCRAE25_TheHarnessCommentedFixtureEqualsTheUnitTest(t *testing.T) {
	const harness, unit = "test/harness/fixtures/nexusd-commented.yaml", "internal/config/testdata/edit/commented.yaml"
	got, want := docsRead(t, harness), docsRead(t, unit)
	if got != want {
		t.Errorf("%s (%d bytes) differs from %s (%d bytes); copy %s over it", harness, len(got), unit, len(want), unit)
	}
}
