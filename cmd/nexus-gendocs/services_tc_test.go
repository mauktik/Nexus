// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-128, TC-SV-129, TC-SV-130, TC-SV-131, TC-SV-133.
// Owner WIs: 5.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package main

import "testing"

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
	t.Skip("TC-SV-128: SKIP-only stub; un-skip when WI 5.2 lands")
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
	t.Skip("TC-SV-129: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SV-130: docs/mcp.md lists the tools, the untrusted list and the measured budget, and says the reboot is not a tool. Owner WI 5.2. Requirements: FR-14, NFR-6.
//
// - Setup: `repoRoot`, and the token total computed as `TestToolBudget` does.
// - Steps:
//  1. Read the file.
//
// - Expected:
//   - the four tool names appear, and the untrusted list contains them;
//   - the recorded budget figure equals the computed token total and tool count (72);
//   - a sentence states that the reboot is not an MCP tool.
func TestTCSV130_DocsMcpMdListsTheToolsThe(t *testing.T) {
	t.Skip("TC-SV-130: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SV-131: docs/adding-a-provider.md §8 documents the four mechanisms. Owner WI 5.2. Requirements: CON-4, NFR-8.
//
// - Setup: `repoRoot`.
// - Steps:
//  1. Find section 8.
//
// - Expected: It mentions `CompletesAcrossReboot`, `AfterReboot`, `detach.RecordReboot`, `RetractReboot`, R1, R2, R3, R4 and R5, audited streams (`Audited` on `ModeStream`), `TypedConfirmation` and `OperationsUser`. The intro lists them.
func TestTCSV131_DocsAddingAProviderMd8Documents(t *testing.T) {
	t.Skip("TC-SV-131: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SV-133: Generated docs are current and include the services commands. Owner WI 5.2. Requirements: NFR-8, FR-15.
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
	t.Skip("TC-SV-133: SKIP-only stub; un-skip when WI 5.2 lands")
}
