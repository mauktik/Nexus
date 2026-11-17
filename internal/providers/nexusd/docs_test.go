// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-90, TC-RA-91, TC-RA-92, TC-RA-E25.
// Owner WIs: 5.1, 5.2. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package nexusd

import "testing"

// TC-RA-90: The docs carry the deny-list, the recovery anchor and the reversed statements. Owner WI 5.1. Requirements: FR-14, FR-12, FR-7, CON-3, ASM-9, OOS-5, OOS-3.
//
//   - Setup: The repository's `docs/`.
//   - Steps:
//     1. Read `docs/security.md`, `docs/install.md`, `docs/mcp.md`, `docs/adding-a-provider.md` and `internal/auth`'s package doc.
//   - Expected:
//   - security.md:
//   - every one of the twelve `spec.ConfigDenyList` keys appears (read from the table, never retyped);
//   - it explains why path-, file-name- and service-valued keys are denied, naming NFR-5 and FR-25 and the `includeDir` plus `includeSuffix` file-write path they close;
//   - it states that `listen` and other list-valued keys are not settable (OOS-3);
//   - it no longer contains "No API or MCP call creates" or "No API, MCP or CLI call writes".
//   - install.md:
//   - it has the heading anchor `recovering-from-a-failed-configuration-change`;
//   - it has sections titled "Manage tokens remotely" and "Change the configuration remotely";
//   - "Change the configuration remotely" names the normalization done when a key is added, and that a no-change set answers `changed: false` at once with no operation;
//   - "Manage tokens remotely" gives the one-login bootstrap: enable `providers.nexusd` and mint the first `nexusd:admin` token with `sudo nexusd token create` on the host (ASM-9, OOS-5), and names `sudo nexusd token` as the lock-out recovery path (CON-4).
//   - mcp.md: it names all six op IDs as not tools.
//   - adding-a-provider.md: it names `SyncMutation`, `SecretOutput`, `Audited`, `CompletesAtAdmission`, `DetachedSpec.Admission`, `Variadic`, `EchoWarnings`, `RetryAfter` and `CheckConfigData`.
//   - Package doc: the `internal/auth` package doc no longer says "no network path".
func TestTCRA90_TheDocsCarryTheDenyListTheRecoveryAnchorAndThe(t *testing.T) {
	t.Skip("TC-RA-90: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-RA-91: Generated docs and the build gate are current. Owner WI 5.1. Requirements: FR-14, NFR-7.
//
//   - Setup: The tree after 5.1.
//   - Steps:
//     1. Check that `docs/cli/nexus_nexusdtoken*.md` and `docs/cli/nexus_nexusdconfig*.md` exist.
//     2. Run `TestGeneratedDocsAreCurrent`.
//   - Expected:
//   - The files exist, and the test passes.
//   - The developer pastes the `make check` and `make docs-check` output, both passing, into the WI report.
func TestTCRA91_GeneratedDocsAndTheBuildGateAreCurrent(t *testing.T) {
	t.Skip("TC-RA-91: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-RA-92: The shipped default YAML gains comments only. Owner WI 5.1. Requirements: CON-5, FR-14.
//
//   - Setup: `git show 521261c:deploy/nexusd.default.yaml` (skip with a reason when git is unavailable).
//   - Steps:
//     1. Decode both versions generically.
//   - Expected: The decoded values are deep-equal, and the key path sets are equal. Only comment text differs.
func TestTCRA92_TheShippedDefaultYAMLGainsCommentsOnly(t *testing.T) {
	t.Skip("TC-RA-92: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-RA-E25: The harness commented fixture equals the unit-test fixture. Owner WI 5.2. Requirements: NFR-3.
//
//   - Setup: The repository.
//   - Steps:
//     1. Compare `test/harness/fixtures/nexusd-commented.yaml` with `internal/config/testdata/edit/commented.yaml`.
//   - Expected: Byte-equal.
func TestTCRAE25_TheHarnessCommentedFixtureEqualsTheUnitTest(t *testing.T) {
	t.Skip("TC-RA-E25: SKIP-only stub; un-skip when WI 5.2 lands")
}
