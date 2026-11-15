// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-8, TC-HP-112, TC-HP-113, TC-HP-132.
// Owner WIs: 1.1, 5.2, 5.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package buildsys

import (
	"path/filepath"
	"strings"
	"testing"
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
	t.Skip("TC-HP-112: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-HP-113: The hardened unit and the OpenRC script are unchanged. Owner WI 5.2. Requirements: CON-1, OOS-4.
//
// - Setup: `git` in `repoRoot`.
// - Steps: `git diff --quiet 09b05cff1518fef79c23a87615d70fdaebcfba83 -- deploy/nexusd.service deploy/nexusd.openrc`; run `TestSystemdUnitByteIdenticalToBase`.
// - Expected: exit 0 (no diff); the existing test passes.
func TestTCHP113_TheHardenedUnitAndTheOpenRCScript(t *testing.T) {
	t.Skip("TC-HP-113: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-HP-132: The live check script is POSIX and read-only by default. Owner WI 5.4. Requirements: NFR-5, CON-2.
//
// - Setup: none.
// - Steps: `sh -n test/live/host-check.sh`; `busybox sh -n` when `busybox` is on PATH (else skip that half); `shellcheck -s sh` when installed; grep the script.
// - Expected: syntax checks pass; without `--yes` the script only prints facts and a plan (no `touch`, `systemd-run`, `rc-service`, `kill` outside functions guarded by `--yes`/`--with-faults`); it never writes `NEXUS_TOKEN` to the transcript (grep for the token-to-fd pattern used by `nginx-check.sh`).
func TestTCHP132_TheLiveCheckScriptIsPOSIXAnd(t *testing.T) {
	t.Skip("TC-HP-132: SKIP-only stub; un-skip when WI 5.4 lands")
}
