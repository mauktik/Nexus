// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-128, TC-SV-129, TC-SV-130, TC-SV-131, TC-SV-133.
// Owner WIs: 5.2 (TC-SV-128 to TC-SV-131), 5.1 (TC-SV-133, moved from
// WI 5.2 in architecture Revision 1). Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	services "github.com/mauktik/nexus/internal/providers/services/spec"
)

// svDoc reads docs/<name>.
func svDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoDocs(t), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// svFold folds every run of white space to one space, so a phrase wrapped
// across lines still matches.
func svFold(s string) string { return strings.Join(strings.Fields(s), " ") }

// svRequire reports each want that the folded text lacks.
func svRequire(t *testing.T, where, text string, wants ...string) {
	t.Helper()
	folded := svFold(text)
	for _, w := range wants {
		if !strings.Contains(folded, svFold(w)) {
			t.Errorf("%s lacks %q", where, w)
		}
	}
}

// svHeadings returns the Markdown headings of doc outside fenced blocks,
// without their leading #s.
func svHeadings(doc string) []string {
	var out []string
	inFence := false
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
		}
		n := strings.IndexFunc(l, func(r rune) bool { return r != '#' })
		if inFence || n <= 0 || !strings.HasPrefix(l[n:], " ") {
			continue
		}
		out = append(out, strings.TrimSpace(l[n:]))
	}
	return out
}

// svSection returns the part of doc from the first heading whose text
// contains title to the next heading of the same or a higher level, or ""
// when there is none.
func svSection(doc, title string) string {
	lines := strings.Split(doc, "\n")
	start, level := -1, 0
	inFence := false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
		}
		n := strings.IndexFunc(l, func(r rune) bool { return r != '#' })
		if inFence || n <= 0 || !strings.HasPrefix(l[n:], " ") {
			continue
		}
		if start < 0 {
			if strings.Contains(l[n:], title) {
				start, level = i, n
			}
			continue
		}
		if n <= level {
			return strings.Join(lines[start:i], "\n")
		}
	}
	if start < 0 {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

// TC-SV-128: docs/services.md covers every required topic and lists every protected name. Owner WI 5.2. Requirements: NFR-8, FR-9, ASM-3, ASM-10, A-27.
//
// - Setup: `repoRoot`; `spec.ProtectedSystemd` and `ProtectedOpenRC`.
// - Steps:
//  1. Read the file and its headings.
//
// - Expected:
//   - headings (case-insensitive substrings) for configuration, allow-list, protected services, log sources, reboot guards, delay and cancel, recovery, CLI wait, LXC and measured timings;
//   - every protected name of both lists appears in backticks;
//   - the text names `docker`, `containerd` and `pihole-FTL` as warnings, and contains the delay-window limitation (A-27);
//   - `services.md` is in `handWritten`.
func TestTCSV128_DocsServicesMdCoversEveryRequiredTopic(t *testing.T) {
	doc := svDoc(t, "services.md")
	headings := svHeadings(doc)
	for _, topic := range []string{"configuration", "allow-list", "protected services", "log sources",
		"reboot guards", "delay and cancel", "recovery", "cli wait", "lxc", "measured timings"} {
		if !slices.ContainsFunc(headings, func(h string) bool { return strings.Contains(strings.ToLower(h), topic) }) {
			t.Errorf("docs/services.md has no heading containing %q; its headings are %q", topic, headings)
		}
	}
	for _, name := range append(slices.Clone(services.ProtectedSystemd), services.ProtectedOpenRC...) {
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("docs/services.md does not name the protected service `%s`", name)
		}
	}
	warnings := svSection(doc, "Warning")
	if warnings == "" {
		t.Error("docs/services.md has no warnings section (ASM-10)")
	}
	svRequire(t, "docs/services.md's warnings section", warnings, "`docker`", "`containerd`", "`pihole-FTL`",
		"nginx", "pihole")
	// A-27: a request admitted during the delay is not refused, and one
	// still running at the shutdown ends outcome-unknown.
	delay := svSection(doc, "Delay and cancel")
	svRequire(t, "docs/services.md's delay and cancel section (A-27)", delay,
		"admitted during the delay are not refused", "`outcome-unknown`")
	if !slices.Contains(handWritten, "services.md") {
		t.Errorf("handWritten %q does not list services.md", handWritten)
	}
}

// TC-SV-129: docs/security.md has the four scopes, the Services and reboot section, the exception and the deny-list row. Owner WI 5.2. Requirements: FR-2, NFR-8, CON-4.
//
// - Setup: `repoRoot`.
// - Steps:
//  1. Parse the scope table and the sections.
//
// - Expected:
//   - the scope table has rows `services:read`, `services:logs`, `services:write` and `services:reboot`, and none of them is in the root-equivalent list;
//   - a `Services and reboot` section says `services:write` can take every allow-listed service down and `services:reboot` takes the host down;
//   - the Operation units section names `CompletesAcrossReboot` as the documented exception to `outcome-unknown`;
//   - the deny-list table has `providers.services.reboot.enabled`.
func TestTCSV129_DocsSecurityMdHasTheFourScopes(t *testing.T) {
	doc := svDoc(t, "security.md")
	scopes := svSection(doc, "Scopes")
	scopeNames := []string{services.ReadScope, services.LogsScope, services.WriteScope, services.RebootScope}
	for _, scope := range scopeNames {
		row := regexp.MustCompile("(?m)^\\|\\s*`" + regexp.QuoteMeta(scope) + "`\\s*\\|.*$").FindString(scopes)
		if row == "" {
			t.Errorf("docs/security.md's Scopes table has no row for `%s`", scope)
			continue
		}
		if strings.Contains(row, "**root-equivalent**") {
			t.Errorf("docs/security.md's `%s` row calls it root-equivalent: %s", scope, row)
		}
	}
	// The root-equivalent list is the opening paragraph of its section.
	rootEq := svSection(doc, "Mutating scopes are potentially root-equivalent")
	if rootEq == "" {
		t.Fatal(`docs/security.md has no "Mutating scopes are potentially root-equivalent" section`)
	}
	list, _, _ := strings.Cut(strings.SplitN(rootEq, "\n", 2)[1], "\n\n- ")
	for _, scope := range scopeNames {
		if strings.Contains(list, scope) {
			t.Errorf("docs/security.md lists `%s` among the potentially root-equivalent scopes", scope)
		}
	}

	section := svSection(doc, "Services and reboot")
	if section == "" {
		t.Error(`docs/security.md has no "Services and reboot" section`)
	}
	svRequire(t, `docs/security.md's "Services and reboot" section`, section,
		"`services:write` is not root-equivalent", "can take every allow-listed service down",
		"`services:reboot` takes the host down")

	units := svSection(doc, "Operation units")
	svRequire(t, `docs/security.md's "Operation units" section`, units,
		"`CompletesAcrossReboot`", "documented exception", "`outcome-unknown`")

	deny := svSection(doc, "The deny-list")
	if !regexp.MustCompile("(?m)^\\|\\s*`providers\\.services\\.reboot\\.enabled`\\s*\\|").MatchString(deny) {
		t.Error("docs/security.md's deny-list table has no `providers.services.reboot.enabled` row")
	}
}

// TC-SV-130: docs/mcp.md lists the tools, the untrusted list and the measured budget, and says the reboot is not a tool. Owner WI 5.2. Requirements: FR-14, NFR-6, FR-6, ASM-5, A-38.
//
// - Setup: `repoRoot`, and the token total computed as `TestToolBudget` does.
// - Steps:
//  1. Read the file.
//
// - Expected:
//   - the four tool names appear, and the untrusted list contains them;
//   - the recorded budget figure equals the computed token total and tool count (72);
//   - a sentence states that the reboot is not an MCP tool;
//   - `docs/mcp.md`'s `get_service_logs` row states that OpenRC logs come back as one page with no `nextOffset`, and that a `truncated` page is narrowed with a smaller `tail` (string match against WI 5.4's rule, A-38; moved from TC-SV-149 in Revision 1).
func TestTCSV130_DocsMcpMdListsTheToolsThe(t *testing.T) {
	doc := svDoc(t, "mcp.md")
	var tools []string
	for _, op := range services.Spec().Ops {
		if op.MCP != nil {
			tools = append(tools, "`"+op.MCP.Tool+"`")
		}
	}
	if len(tools) != 4 {
		t.Fatalf("the services spec has %d MCP tools, want 4", len(tools))
	}
	svRequire(t, "docs/mcp.md", doc, tools...)
	var untrusted, logsRow string
	for _, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, "**Untrusted content.**") {
			untrusted = line
		}
		if strings.Contains(line, "`get_service_logs`") && strings.Contains(line, "OpenRC") {
			logsRow = line
		}
	}
	svRequire(t, "docs/mcp.md's untrusted list", untrusted, tools...)

	// The budget figure, against the total TestToolBudget computes.
	root := filepath.Dir(repoDocs(t))
	cmd := exec.Command("go", "test", "-count=1", "-run", "^TestToolBudget$", "-v", "./internal/mcpadapter")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	m := regexp.MustCompile(`(\d+) tools, (\d+) characters, ceil\(\d+/4\) = (\d+) tokens`).FindStringSubmatch(string(out))
	if err != nil || m == nil {
		t.Fatalf("TestToolBudget failed or logged no total (%v):\n%s", err, out)
	}
	if m[1] != "72" {
		t.Errorf("TestToolBudget measures %s tools, want 72", m[1])
	}
	stated := regexp.MustCompile(`Current figure: \*\*(\d+) tools, ([0-9,]+) characters, ([0-9,]+) tokens\*\*`).
		FindStringSubmatch(svFold(doc))
	if stated == nil {
		t.Fatal(`docs/mcp.md does not state "Current figure: **N tools, C characters, T tokens**"`)
	}
	if stated[1] != m[1] || strings.ReplaceAll(stated[2], ",", "") != m[2] || strings.ReplaceAll(stated[3], ",", "") != m[3] {
		t.Errorf("docs/mcp.md states %s tools, %s characters, %s tokens; TestToolBudget measures %s, %s, %s",
			stated[1], stated[2], stated[3], m[1], m[2], m[3])
	}

	// The reboot is not a tool.
	found := false
	for _, sentence := range strings.Split(svFold(doc), ". ") {
		if strings.Contains(sentence, "reboot") && strings.Contains(sentence, "not an MCP tool") {
			found = true
		}
	}
	if !found {
		t.Error("docs/mcp.md has no sentence saying that the reboot is not an MCP tool")
	}

	// A-38: OpenRC logs are one page, and a truncated page is narrowed with
	// a smaller tail; an untimed frame carries the zero time.
	if logsRow == "" {
		t.Fatal("docs/mcp.md has no line naming `get_service_logs` and OpenRC")
	}
	svRequire(t, "docs/mcp.md's get_service_logs row", logsRow, "one page", "no `nextOffset`", "`truncated`",
		"smaller `tail`")
	svRequire(t, "docs/mcp.md", doc, "`0001-01-01T00:00:00Z`", "untimed")
}

// TC-SV-131: docs/adding-a-provider.md §8 documents the four mechanisms. Owner WI 5.2. Requirements: CON-4, NFR-8.
//
// - Setup: `repoRoot`.
// - Steps:
//  1. Find section 8.
//
// - Expected: It mentions `CompletesAcrossReboot`, `AfterReboot`, `detach.RecordReboot`, `RetractReboot`, R1, R2, R3, R4 and R5, audited streams (`Audited` on `ModeStream`), `TypedConfirmation` and `OperationsUser`. The intro lists them.
func TestTCSV131_DocsAddingAProviderMd8Documents(t *testing.T) {
	doc := svDoc(t, "adding-a-provider.md")
	section := svSection(doc, "8. Generic mechanisms added for the services provider")
	if section == "" {
		t.Fatal(`docs/adding-a-provider.md has no "8. Generic mechanisms added for the services provider" section`)
	}
	svRequire(t, "docs/adding-a-provider.md §8", section,
		"`CompletesAcrossReboot`", "`AfterReboot`", "`detach.RecordReboot`", "`RetractReboot`",
		"R1", "R2", "R3", "R4", "R5", "S1", "S2", "S3", "S4", "S5", "SIGTERM",
		"`Audited`", "`ModeStream`", "`clientDisconnected`", "`core.ErrStreamStopped`",
		"`TypedConfirmation`", "`OperationsUser`", "untimed")
	intro, _, _ := strings.Cut(doc, "\n## ")
	svRequire(t, "docs/adding-a-provider.md's intro", intro,
		"`services`", "`CompletesAcrossReboot`", "audited streams", "`TypedConfirmation`", "`OperationsUser`",
		"untimed frames", "(#8-generic-mechanisms-added-for-the-services-provider)")
}

// TC-SV-133: Generated docs are current and include the services commands. Owner WI 5.1. Requirements: NFR-8, FR-15.
//
// - Setup: The existing `TestGeneratedDocsAreCurrent` mechanism.
// - Steps:
//  1. Regenerate to a temp directory and diff.
//
// - Expected:
//   - no diff;
//   - `docs/cli/` has pages for `nexus service`, `nexus servicelog` and `nexus machine`;
//   - `docs/api.md` has the eight operation IDs.
func TestTCSV133_GeneratedDocsAreCurrentAndIncludeThe(t *testing.T) {
	docs := repoDocs(t)
	files := mustGenerate(t, nil)
	if problems := check(docs, files); len(problems) > 0 {
		t.Fatalf("generated documentation is stale; run `make docs` and commit docs/:\n  %s", strings.Join(problems, "\n  "))
	}
	for _, page := range []string{"nexus_service.md", "nexus_servicelog.md", "nexus_machine.md",
		"nexus_machine_restart.md", "nexus_service_get.md", "nexus_service_invoke.md", "nexus_servicelog_get.md"} {
		if _, err := os.Stat(filepath.Join(docs, "cli", page)); err != nil {
			t.Errorf("docs/cli/%s: %v", page, err)
		}
	}
	api, err := os.ReadFile(filepath.Join(docs, "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	ops := services.Spec().Ops
	if len(ops) != 8 {
		t.Fatalf("the services spec has %d ops, want 8", len(ops))
	}
	for _, op := range ops {
		if !strings.Contains(string(api), op.ID) {
			t.Errorf("docs/api.md does not name %s", op.ID)
		}
	}
}
