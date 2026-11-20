// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/cli"
)

// statement is one required statement of a page: every phrase must appear,
// compared case-insensitively with whitespace collapsed. A reviewer signs
// off the wording; this only guards against removal (TC-186).
type statement struct {
	what    string
	phrases []string
}

var requiredStatements = map[string][]statement{
	"security.md": {
		{"LAN only (ASM-5, A-1)", []string{"LAN only"}},
		{"warning against port forwarding", []string{"port forwarding"}},
		{"warning against the nginx gateway's public side", []string{"nginx", "public side"}},
		{"nexusd runs as root under a hardened unit (A-2)", []string{"runs as root", "hardened"}},
		{"mutating scopes are potentially root-equivalent (NFR-4)", []string{"potentially root-equivalent"}},
		{"the sandbox is not a boundary (D-5)", []string{"not a security boundary"}},
		{"repository write access equals code execution (FR-20)", []string{"write access", "code execution"}},
		{"audit limits (OOS-12)", []string{"append-only by convention", "not against root"}},
		{"forwarded headers are never honored (A-60)", []string{"X-Forwarded-For", "never honored"}},
		{"token handling", []string{"nexusd token create", "printed once", "revoke"}},
		{"operators stop operation units with systemctl kill (A-22)", []string{"systemctl kill", "RefuseManualStop"}},
		{"unit directives with omissions and reasons (10.1)", []string{"## Directives in force", "## Directives removed since WI 3.5", "Not applied in this release"}},
	},
	"scripts.md": {
		{"no TTY", []string{"no TTY"}},
		{"TERM=dumb", []string{"TERM=dumb"}},
		{"no .bashrc", []string{".bashrc", "not sourced"}},
		{"stdin at EOF", []string{"/dev/null", "end-of-file"}},
		{"block-buffered output", []string{"block-buffer"}},
		{"sudo cannot prompt", []string{"sudo", "cannot prompt"}},
		{"background children are cleaned up (A-10)", []string{"background", "KillMode=control-group"}},
		{"exit 0 means Succeeded", []string{"exit code 0", "Succeeded"}},
		{"run each script through Nexus, including a failing case", []string{"through Nexus", "failing case"}},
		{"umask 022 for checkouts (A-59)", []string{"umask 022"}},
		{"the private-group rule (A-59)", []string{"private group"}},
		{"the exact chmod each refusal names (A-59)", []string{"chmod g-w", "chmod o-w", "chmod u+x", "chown root"}},
		{"repository write access equals code execution (FR-20)", []string{"write access", "code execution"}},
		{"operators stop operation units with systemctl kill (A-22)", []string{"systemctl kill"}},
	},
	"mcp.md": {
		{"endpoint and token", []string{"/mcp", "Authorization: Bearer"}},
		{"certificate trust is one per-client step (A-15)", []string{"NODE_EXTRA_CA_CERTS", "--cacert"}},
		{"never the OS trust store (A-15)", []string{"operating system's trust store"}},
		{"loopback with tls.mode: off as the fallback (A-15)", []string{"tls.mode: off", "127.0.0.1"}},
		{"tool budget method (NFR-8)", []string{"ceil(total_chars / 4)", "TestToolBudget"}},
	},
	"adding-a-provider.md": {
		{"the spec package", []string{"def.Define", "def.ProviderSpec"}},
		{"the binding", []string{"provider.Sync", "provider.Detached", "Bindings()"}},
		{"the two composition lines", []string{"internal/catalog/catalog.go", "internal/daemon/providers.go"}},
	},
	"install.md": {
		{"installing nexusd", []string{"install.sh", "validate-config"}},
		{"tokens", []string{"nexusd token create"}},
		{"pinning the certificate", []string{"nexus host add", "nexusd tls fingerprint"}},
		{"plain HTTP needs 127.0.0.1, not localhost", []string{"127.0.0.1", "localhost"}},
		{"upgrades and needrestart", []string{"needrestart"}},
		{"uninstalling", []string{"uninstall.sh"}},
	},
}

var spaces = regexp.MustCompile(`\s+`)

func normalize(s string) string { return strings.ToLower(spaces.ReplaceAllString(s, " ")) }

// TC-186: every required statement is present in its page.
func TestRequiredStatements(t *testing.T) {
	docs := repoDocs(t)
	for page, stmts := range requiredStatements {
		b, err := os.ReadFile(filepath.Join(docs, page))
		if err != nil {
			t.Errorf("docs/%s: %v", page, err)
			continue
		}
		text := normalize(string(b))
		for _, s := range stmts {
			for _, p := range s.phrases {
				if !strings.Contains(text, normalize(p)) {
					t.Errorf("docs/%s: required statement %q: missing %q", page, s.what, p)
				}
			}
		}
	}
}

// The composition lines adding-a-provider.md quotes are the lines in the
// composition roots, so the guide cannot drift from the code.
func TestProviderGuideQuotesTheCompositionLines(t *testing.T) {
	docs := repoDocs(t)
	guide, err := os.ReadFile(filepath.Join(docs, "adding-a-provider.md"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(docs)
	for file, line := range map[string]string{
		"internal/catalog/catalog.go":  "scripts.Spec(),",
		"internal/daemon/providers.go": "scripts.New(scripts.Options{Host: host}),",
	} {
		src, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), line) {
			t.Errorf("%s no longer holds %q: update adding-a-provider.md and this test", file, line)
		}
		if !strings.Contains(string(guide), line) {
			t.Errorf("adding-a-provider.md does not quote %s's line %q", file, line)
		}
	}
}

// Every `nexus <noun> <verb>` and `nexusd <command>` the hand-written pages
// and README.md name exists, so the prose cannot document a command that
// is not there.
func TestHandWrittenDocsNameRealCommands(t *testing.T) {
	tree, err := cli.CommandTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		known[c.CommandPath()] = true
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(tree)
	// nexusd's commands and their subcommands (cmd/nexusd, internal/auth).
	nexusdCommands := map[string][]string{"serve": nil, "validate-config": nil,
		"token": {"create", "list", "revoke"}, "tls": {"fingerprint"}}

	docs := repoDocs(t)
	pages := append(slices.Clone(handWritten), "../README.md")
	nexusRE := regexp.MustCompile(`\bnexus ([a-z]+)(?: ([a-z][a-z-]*))?`)
	nexusdRE := regexp.MustCompile(`\bnexusd ([a-z][a-z-]*)(?: ([a-z]+))?`)
	// OpenRC's commands name the nexusd service, not a nexusd command:
	// rc-service nexusd start, rc-update add nexusd default.
	serviceRE := regexp.MustCompile(`\b(rc-service|rc-update (?:add|del)) nexusd\b`)
	// Words that can follow "nexus" in a code span without naming a command,
	// such as a comment in an example.
	prose := map[string]bool{"is": true, "and": true, "or": true, "on": true, "to": true, "the": true}
	for _, page := range pages {
		b, err := os.ReadFile(filepath.Join(docs, page))
		if err != nil {
			t.Errorf("%s: %v", page, err)
			continue
		}
		for _, code := range codeSpans(string(b)) {
			for _, m := range nexusRE.FindAllStringSubmatch(code, -1) {
				if prose[m[1]] {
					continue
				}
				path := "nexus " + m[1]
				if !known[path] {
					t.Errorf("%s names %q, which is not a nexus command", page, m[0])
					continue
				}
				if m[2] != "" && known[path+" "+m[2]] {
					continue
				}
				if m[2] != "" && hasSubcommands(tree, m[1]) {
					t.Errorf("%s names %q, which is not a nexus command", page, m[0])
				}
			}
			// rc-service and rc-update name the nexusd service, not the binary.
			c := serviceRE.ReplaceAllString(code, "$1")
			for _, ix := range nexusdRE.FindAllStringSubmatchIndex(c, -1) {
				// "nexus nexusd <verb>" is the nexus CLI's nexusd noun,
				// checked above, not the nexusd binary.
				if strings.HasSuffix(c[:ix[0]], "nexus ") {
					continue
				}
				m := []string{c[ix[0]:ix[1]], c[ix[2]:ix[3]], ""}
				if ix[4] >= 0 {
					m[2] = c[ix[4]:ix[5]]
				}
				subs, ok := nexusdCommands[m[1]]
				if !ok || (len(subs) > 0 && m[2] != "" && !slices.Contains(subs, m[2])) {
					t.Errorf("%s names %q, which is not a nexusd command", page, m[0])
				}
			}
		}
	}
}

func hasSubcommands(root *cobra.Command, noun string) bool {
	for _, c := range root.Commands() {
		if c.Name() == noun {
			return c.HasSubCommands()
		}
	}
	return false
}

var fenceRE = regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")
var inlineRE = regexp.MustCompile("`([^`\n]+)`")

// codeSpans returns the fenced blocks and inline code spans of a Markdown
// page: where commands are written.
func codeSpans(md string) []string {
	var out []string
	for _, m := range fenceRE.FindAllStringSubmatch(md, -1) {
		out = append(out, m[1])
	}
	rest := fenceRE.ReplaceAllString(md, "")
	for _, m := range inlineRE.FindAllStringSubmatch(rest, -1) {
		out = append(out, m[1])
	}
	return out
}
