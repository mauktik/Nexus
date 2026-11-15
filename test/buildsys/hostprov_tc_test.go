// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-8, TC-HP-112, TC-HP-113, TC-HP-132.
// Owner WIs: 1.1, 5.2, 5.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package buildsys

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-HP-8: adding-a-provider.md documents the generic Background contract. Owner WI 1.1. Requirements: CON-4, FR-1.
//
// - Setup: none.
// - Steps: read `docs/adding-a-provider.md`; find the section titled "Generic mechanism added for the host provider: `provider.Background`".
// - Expected: the section exists and contains `RunBackground`, "enabled", "reload", "validate-config", and states it is keyed on configuration enablement, not on the provider's name; `internal/daemon/daemon.go` no longer contains the sentence "Nothing runs in the background between requests" unqualified (it names `Background`).
func TestTCHP8_AddingAProviderMdDocumentsTheGeneric(t *testing.T) {
	root := repoRoot(t)
	guide := readFile(t, filepath.Join(root, "docs", "adding-a-provider.md"))
	s := section(guide, "Generic mechanism added for the host provider: `provider.Background`")
	if s == "" {
		t.Fatal("docs/adding-a-provider.md has no \"Generic mechanism added for the host provider: " +
			"`provider.Background`\" section")
	}
	requireAll(t, "the provider.Background section", s, "`RunBackground`", "enabled", "reload",
		"validate-config", "keyed on configuration enablement, not on the provider's name")

	// The Run comment names the exception instead of saying, unqualified,
	// that nothing runs between requests.
	daemon := oneLine(readFile(t, filepath.Join(root, "internal", "daemon", "daemon.go")))
	const claim = "Nothing runs in the background between requests"
	if i := strings.Index(daemon, claim); i >= 0 {
		sentence := daemon[i:]
		if end := strings.Index(sentence, ". "); end >= 0 {
			sentence = sentence[:end]
		}
		if !strings.Contains(sentence, "Background") {
			t.Errorf("internal/daemon/daemon.go still says %q without naming Background: %q", claim, sentence)
		}
	}
}

// TC-HP-112: Hand-written docs cover scope, ops, keys and the sandbox list. Owner WI 5.2. Requirements: FR-2, FR-15, NFR-5, NFR-7, CON-4.
//
// - Setup: none.
// - Steps: read the files; read `handWritten` in `cmd/nexus-gendocs/main_test.go`.
// - Expected:
//   - `security.md` has a Scopes-table row for `system:read` naming host identity, network addresses, gateway, DNS resolvers, mount points, process names and owners, and a note that `system:read` does not gate `system.*`; a section "What the host provider reads" naming `CAP_DAC_READ_SEARCH`, cmdline, environ, serial numbers, kernel log, clock syscalls, OpenRC root;
//   - `host.md` contains all ten op IDs, all four config keys, the twelve reason prefixes, the heading "Unavailable under the systemd sandbox", the exclusion fstypes, and the naming note (`host`, `system:read`);
//   - `mcp.md` lists the three tools as untrusted and states 68 tools and the measured token figure;
//   - `adding-a-provider.md` lists `host`; README links `docs/host.md` and lists `system:read`; `handWritten` contains `host.md`.
func TestTCHP112_HandWrittenDocsCoverScopeOpsKeys(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string { return readFile(t, filepath.Join(root, filepath.FromSlash(rel))) }

	// security.md: the Scopes row, the system.* note and the reads section.
	security := read("docs/security.md")
	row := regexp.MustCompile("(?m)^\\|\\s*`system:read`\\s*\\|.*$").FindString(security)
	if row == "" {
		t.Error("docs/security.md's Scopes table has no row for `system:read`")
	}
	requireAll(t, "docs/security.md's system:read row", row, "host identity", "network addresses", "gateway",
		"DNS resolvers", "mount points", "process names and owners")
	if !regexp.MustCompile("`system:read`[^.]*does not gate[^.]*`system\\.\\*`").MatchString(oneLine(security)) {
		t.Error("docs/security.md does not note that `system:read` does not gate the `system.*` operations")
	}
	reads := section(security, "What the host provider reads")
	if reads == "" {
		t.Error(`docs/security.md has no "What the host provider reads" section`)
	}
	requireAll(t, "docs/security.md's host reads section", reads, "allow-list", "`CAP_DAC_READ_SEARCH`",
		"cmdline", "environ", "serial numbers", "kernel log", "`@clock`", "OpenRC", "unconfined root")

	// host.md: every op, every key, every reason prefix, the sandbox heading,
	// the exclusion list and the naming note.
	host := read("docs/host.md")
	var ids []string
	for _, op := range hostspec.Spec().Ops {
		ids = append(ids, "`"+op.ID+"`")
	}
	if len(ids) != 10 {
		t.Errorf("the host spec declares %d operations, want 10", len(ids))
	}
	requireAll(t, "docs/host.md (op IDs)", host, ids...)
	requireAll(t, "docs/host.md (config keys)", host,
		"`enabled`", "`sampleInterval`", "`historyDuration`", "`sourceTimeout`")
	requireAll(t, "docs/host.md (reason prefixes)", host,
		"`warming up`", "`stale: last sample", "`not running`", "`permission denied: ", "`not present: ",
		"`timed out after ", "`still pending: ", "`not visible: ", "`unrecognized format: ", "`counter reset: ",
		"`not reported: ", "`command failed: ", "`not supported: ", "`internal error: access not allow-listed`")
	if !regexp.MustCompile(`(?m)^#+ Unavailable under the systemd sandbox\s*$`).MatchString(host) {
		t.Error(`docs/host.md has no "Unavailable under the systemd sandbox" heading`)
	}
	sandbox := section(host, "Unavailable under the systemd sandbox")
	// TC-HP-112 asks for the heading; WI 5.4 replaced the expected list
	// under it with the observed one, which TC-HP-131 checks against T1.
	if strings.TrimSpace(sandbox) == "" {
		t.Error(`docs/host.md's "Unavailable under the systemd sandbox" section is empty`)
	}
	requireAll(t, "docs/host.md (excluded filesystem types)", host,
		"`proc`", "`sysfs`", "`devtmpfs`", "`devpts`", "`cgroup`", "`cgroup2`", "`securityfs`", "`debugfs`",
		"`tracefs`", "`bpf`", "`pstore`", "`mqueue`", "`hugetlbfs`", "`configfs`", "`fusectl`", "`autofs`",
		"`binfmt_misc`", "`nsfs`", "`efivarfs`", "`rpc_pipefs`", "`nfsd`", "`selinuxfs`", "`ramfs`",
		"`fuse.lxcfs`", "`fuse.gvfsd-fuse`", "`fuse.portal`", "`overlay`", "`squashfs`", "`tmpfs`")
	naming := section(host, "Naming")
	if naming == "" {
		t.Error(`docs/host.md has no "Naming" section`)
	}
	requireAll(t, "docs/host.md's naming note", naming, "`host`", "`system:read`", "`system`", "ASM-1")

	// mcp.md: the three tools as untrusted, 68 tools and the token figure.
	mcp := read("docs/mcp.md")
	untrusted := ""
	for _, para := range strings.Split(mcp, "\n") {
		if strings.Contains(para, "**Untrusted content.**") {
			untrusted = para
		}
	}
	requireAll(t, "docs/mcp.md's untrusted list", untrusted,
		"`get_host_summary`", "`get_host_health`", "`get_host_metrics`")
	requireAll(t, "docs/mcp.md", mcp, "68 tools", "2,831 tokens")

	// adding-a-provider.md, README.md and handWritten.
	intro := read("docs/adding-a-provider.md")
	if i := strings.Index(intro, "\n## "); i >= 0 {
		intro = intro[:i]
	}
	if !regexp.MustCompile("`host`\\s+for\\s").MatchString(oneLine(intro)) {
		t.Error("docs/adding-a-provider.md's intro does not list `host` among the providers")
	}
	readme := read("README.md")
	requireAll(t, "README.md", readme, "(docs/host.md)", "`system:read`")
	gendocs := read("cmd/nexus-gendocs/main_test.go")
	hw := regexp.MustCompile(`(?m)^var handWritten = \[\]string\{(.*)\}$`).FindStringSubmatch(gendocs)
	if hw == nil || !strings.Contains(hw[1], `"host.md"`) {
		t.Error("handWritten in cmd/nexus-gendocs/main_test.go does not list host.md")
	}
}

// TC-HP-113: The hardened unit and the OpenRC script are unchanged. Owner WI 5.2. Requirements: CON-1, OOS-4.
//
// - Setup: `git` in `repoRoot`.
// - Steps: `git diff --quiet 09b05cff1518fef79c23a87615d70fdaebcfba83 -- deploy/nexusd.service deploy/nexusd.openrc`; run `TestSystemdUnitByteIdenticalToBase`.
// - Expected: exit 0 (no diff); the existing test passes.
func TestTCHP113_TheHardenedUnitAndTheOpenRCScript(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const base = "09b05cff1518fef79c23a87615d70fdaebcfba83"
	root := repoRoot(t)
	if out, code := run(t, root, nil, "git", "cat-file", "-e", base+"^{commit}"); code != 0 {
		t.Skipf("base commit %s not reachable (shallow clone?): %s", base, out)
	}
	if out, code := run(t, root, nil, "git", "diff", "--quiet", base, "--",
		"deploy/nexusd.service", "deploy/nexusd.openrc"); code != 0 {
		t.Errorf("deploy/nexusd.service or deploy/nexusd.openrc differs from %s (CON-1, OOS-4): exit %d %s",
			base, code, out)
	}
	t.Run("TestSystemdUnitByteIdenticalToBase", TestSystemdUnitByteIdenticalToBase)
}

// TC-HP-132: The live check script is POSIX and read-only by default. Owner WI 5.4. Requirements: NFR-5, CON-2.
//
// - Setup: none.
// - Steps: `sh -n test/live/host-check.sh`; `busybox sh -n` when `busybox` is on PATH (else skip that half); `shellcheck -s sh` when installed; grep the script.
// - Expected: syntax checks pass; without `--yes` the script only prints facts and a plan (no `touch`, `systemd-run`, `rc-service`, `kill` outside functions guarded by `--yes`/`--with-faults`); it never writes `NEXUS_TOKEN` to the transcript (grep for the token-to-fd pattern used by `nginx-check.sh`).
//
// "Guarded" is checked where it is decided: every function that runs one
// of the host-changing commands is named mut_* and begins with
// `mutating_allowed || refuse ...`, and mutating_allowed is true only with
// --yes and an opt-in mode. The one exception is with_timeout, whose kill
// ends this script's own nexus CLI child on the machine it runs on, never a
// process on the host. Without --yes, main refuses right after the plan.
func TestTCHP132_TheLiveCheckScriptIsPOSIXAnd(t *testing.T) {
	root := repoRoot(t)
	rel := filepath.Join("test", "live", "host-check.sh")
	path := filepath.Join(root, rel)
	script := readFile(t, path)
	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Errorf("%s: first line is not #!/bin/sh", rel)
	}

	// Syntax: sh -n, busybox sh -n when present, shellcheck -s sh when
	// installed.
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Errorf("sh -n %s: %v\n%s", rel, err, out)
	}
	if _, err := exec.LookPath("busybox"); err == nil {
		if out, err := exec.Command("busybox", "sh", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("busybox sh -n %s: %v\n%s", rel, err, out)
		}
	} else {
		t.Logf("busybox not on PATH: busybox sh -n %s skipped", rel)
	}
	if _, err := exec.LookPath("shellcheck"); err == nil {
		if out, err := exec.Command("shellcheck", "-s", "sh", path).CombinedOutput(); err != nil {
			t.Errorf("shellcheck -s sh %s: %v\n%s", rel, err, out)
		}
	} else {
		t.Logf("shellcheck not on PATH: lint of %s skipped", rel)
	}
	forbidden := map[string]*regexp.Regexp{
		"[[ ]]":         regexp.MustCompile(`(^|[\s;&|!])\[\[\s`), // not [[:space:]] or \[[0-9]
		"pipefail":      regexp.MustCompile(`pipefail`),
		"local":         regexp.MustCompile(`(^|[;&|\s])local\s`),
		"$'...'":        regexp.MustCompile(`\$'`),
		"function":      regexp.MustCompile(`(^|\s)function\s+\w+`),
		"here-string":   regexp.MustCompile(`<<<`),
		"source":        regexp.MustCompile(`(^|[;&|\s])source\s`),
		"&>":            regexp.MustCompile(`&>`),
		"${v:off}":      regexp.MustCompile(`\$\{\w+:[0-9]`),
		"date +%N":      regexp.MustCompile(`%N`),
		"jq (not base)": regexp.MustCompile(`(^|[\s|(])jq\s`),
	}

	// Split the script into its top-level functions (name() { ... } up to a
	// "}" in column 0, or one line) and the code outside them.
	funcStart := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\) \{(.*)$`)
	type fn struct {
		name  string
		first string // the first statement of the body
		lines []string
	}
	var fns []fn
	var cur *fn
	var top []string
	heredoc := ""
	for i, line := range strings.Split(script, "\n") {
		code := line
		if heredoc == "" {
			if j := strings.Index(code, "#"); j >= 0 && (j == 0 || code[j-1] == ' ' || code[j-1] == '\t') {
				code = code[:j]
			}
			for name, re := range forbidden {
				if re.MatchString(code) {
					t.Errorf("%s:%d uses %s (not POSIX sh, or not on a BusyBox host): %s", rel, i+1, name, line)
				}
			}
		}
		switch {
		case heredoc != "":
			if strings.TrimSpace(line) == heredoc {
				heredoc = ""
			}
			continue // the usage text and the token's here-document
		case strings.Contains(code, "<<'EOF'") || strings.Contains(code, "<<EOF"):
			heredoc = "EOF"
		}
		if cur == nil {
			if m := funcStart.FindStringSubmatch(code); m != nil {
				rest := strings.TrimSpace(m[2])
				if strings.HasSuffix(rest, "}") { // a one-line function
					fns = append(fns, fn{name: m[1], first: strings.TrimSuffix(rest, "}"), lines: []string{code}})
					continue
				}
				fns = append(fns, fn{name: m[1]})
				cur = &fns[len(fns)-1]
				continue
			}
			top = append(top, code)
			continue
		}
		if line == "}" {
			cur = nil
			continue
		}
		if cur.first == "" && strings.TrimSpace(code) != "" {
			cur.first = strings.TrimSpace(code)
		}
		cur.lines = append(cur.lines, code)
	}
	if cur != nil {
		t.Fatalf("%s: function %s is not closed by a } in column 0", rel, cur.name)
	}

	// The host-changing commands run only in guarded mut_* functions.
	changers := map[string]*regexp.Regexp{
		"touch":       regexp.MustCompile(`(^|[^\w-])touch\s`),
		"systemd-run": regexp.MustCompile(`systemd-run`),
		"rc-service":  regexp.MustCompile(`rc-service`),
		"kill":        regexp.MustCompile(`(^|[^\w-])kill\s`),
	}
	const guard = `mutating_allowed || refuse`
	seen := map[string]bool{}
	for _, f := range fns {
		seen[f.name] = true
		guarded := strings.HasPrefix(f.name, "mut_")
		if guarded && !strings.HasPrefix(f.first, guard) {
			t.Errorf("%s: %s does not begin with %q (its first statement is %q)", rel, f.name, guard, f.first)
		}
		for _, line := range f.lines {
			for word, re := range changers {
				if !re.MatchString(line) {
					continue
				}
				if guarded || (f.name == "with_timeout" && word == "kill") {
					continue
				}
				t.Errorf("%s: %s in %s, which is not a guarded mut_* function: %s", rel, word, f.name, strings.TrimSpace(line))
			}
		}
	}
	for _, line := range top {
		for word, re := range changers {
			if re.MatchString(line) {
				t.Errorf("%s: %s outside any function: %s", rel, word, strings.TrimSpace(line))
			}
		}
	}
	for _, name := range []string{"mutating_allowed", "with_timeout", "mut_cleanup", "mut_fault_reboot", "mut_fault_service",
		"mut_overhead", "mut_rss_start", "mut_rss_finish", "token_check", "token_env_check"} {
		if !seen[name] {
			t.Errorf("%s: no function %s", rel, name)
		}
	}
	allowed := ""
	for _, f := range fns {
		if f.name == "mutating_allowed" {
			allowed = strings.Join(f.lines, "\n")
		}
	}
	if !strings.Contains(allowed, `[ "$YES" = 1 ] || return 1`) {
		t.Errorf("%s: mutating_allowed does not require --yes first:\n%s", rel, allowed)
	}
	main := strings.Join(top, "\n")
	planAt := strings.Index(main, "\nplan\n")
	refuseAt := strings.Index(main, `if [ "$YES" != 1 ]; then`)
	firstMut := strings.Index(main, "mut_")
	if planAt < 0 || refuseAt < planAt || firstMut < refuseAt {
		t.Errorf("%s: main must print the plan, then refuse without --yes, before any mut_* call (plan at %d, refusal at %d, first mut_ at %d)",
			rel, planAt, refuseAt, firstMut)
	}

	// NEXUS_TOKEN: dropped from the environment by a re-exec that hands it
	// over on fd 3 (nginx-check.sh's pattern), passed on stdin only, and
	// never written to the transcript.
	requireAll(t, rel, script, "unset NEXUS_TOKEN", `exec sh "$0" "$@" 3<<EOF`, `IFS= read -r CHECK_TOKEN <&3`,
		"exec 3<&-", "--token-stdin", "token_check", "token_env_check")
	for i, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "CHECK_TOKEN") && !strings.Contains(line, "NEXUS_TOKEN") {
			continue
		}
		for _, sink := range []string{"log ", "note ", "tee ", "TRANSCRIPT", "result ", "echo "} {
			if strings.Contains(line, sink) && !strings.Contains(line, `"$NEXUS_TOKEN"`) && strings.Contains(line, "$CHECK_TOKEN") {
				t.Errorf("%s:%d writes the token where the transcript could get it: %s", rel, i+1, line)
			}
		}
		if strings.Contains(line, "$NEXUS_TOKEN") && !strings.Contains(line, "CHECK_TOKEN=$NEXUS_TOKEN") {
			t.Errorf("%s:%d uses $NEXUS_TOKEN outside the hand-over to fd 3: %s", rel, i+1, line)
		}
	}

	// Behaviour that needs no host: --help, an unknown argument, and
	// mutually exclusive modes, each before any transcript is made.
	dir := t.TempDir()
	if out, code := run(t, dir, nil, "sh", path, "--help"); code != 0 || !strings.Contains(out, "--with-faults") ||
		!strings.Contains(out, "--rss-24h start") {
		t.Errorf("sh %s --help: exit %d\n%s", rel, code, out)
	}
	if out, code := run(t, dir, nil, "sh", path, "--bogus"); code != 2 {
		t.Errorf("sh %s --bogus: exit %d (want 2)\n%s", rel, code, out)
	}
	if out, code := run(t, dir, nil, "sh", path, "--with-faults", "--overhead"); code != 2 ||
		!strings.Contains(out, "pick one") {
		t.Errorf("sh %s --with-faults --overhead: exit %d (want 2, refused)\n%s", rel, code, out)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the refused runs left files in %s: %v %v", dir, entries, err)
	}
}
