// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-8, TC-HP-112, TC-HP-113, TC-HP-132.
// Owner WIs: 1.1, 5.2, 5.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package buildsys

import (
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
	requireAll(t, "docs/host.md's sandbox section", sandbox, "to be confirmed by WI 5.4")
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
func TestTCHP132_TheLiveCheckScriptIsPOSIXAnd(t *testing.T) {
	t.Skip("TC-HP-132: SKIP-only stub; un-skip when WI 5.4 lands")
}
