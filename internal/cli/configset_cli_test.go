// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-88, TC-RA-89, TC-RA-E24.
// Owner WIs: 4.4. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package cli

import "testing"

// TC-RA-88: nexusdconfig update fans out with confirmation, prints per-host results and echoes warnings. Owner WI 4.4. Requirements: FR-11, FR-7, ASM-3.
//
//   - Setup: Two `startProviderServer` hosts `a` and `b` with the nexusd provider wired to the real helper and F-RELOAD over separate temp files.
//   - Steps:
//     1. `nexus nexusdconfig update --host a,b tls.mode=auto` with no TTY.
//     2. The same with `--yes`.
//   - Expected:
//   - Step 1: refused, with 0 POSTs.
//   - Step 2: waits like `reload`. stdout has per-host results with `changed`, `changes` (old and new), `restartRequired` and `backupPath`. stderr has the lock-out warning once per host through `a.warn`. Exit 0.
func TestTCRA88_NexusdconfigUpdateFansOutWithConfirmationPrints(t *testing.T) {
	t.Skip("TC-RA-88: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-RA-89: nexusdconfig get and get KEY print tables. Owner WI 4.4. Requirements: FR-6, FR-11.
//
//   - Setup: Host `a` over the catalog-env fixture.
//   - Steps:
//     1. `nexus nexusdconfig get`.
//     2. `nexus nexusdconfig get providers.host.enabled`.
//   - Expected:
//   - Both tables have the header `NAME VALUE EFFECTIVE SOURCE`, per the 3.3 columns.
//   - Step 1 has one row per leaf key, and the env values show `[REDACTED]`.
//   - Step 2 has one row.
//   - The sentinel occurs 0 times in stdout.
func TestTCRA89_NexusdconfigGetAndGetKEYPrintTables(t *testing.T) {
	t.Skip("TC-RA-89: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-RA-E24: A multi-host config set with one refused host prints both results and exits non-zero. Owner WI 4.4. Requirements: FR-11, OOS-6.
//
//   - Setup: Hosts `a` (systemd-ready) and `b` (availability seam unavailable).
//   - Steps:
//     1. `nexus nexusdconfig update --host a,b --yes providers.host.enabled=true`.
//   - Expected:
//   - `a` succeeds.
//   - `b` renders the `config-set-unavailable` problem with its reason.
//   - The exit code is the existing multi-host failure code.
//   - There is no cross-host rollback, so `a`'s file stays changed (OOS-6).
func TestTCRAE24_AMultiHostConfigSetWithOneRefusedHostPrintsBoth(t *testing.T) {
	t.Skip("TC-RA-E24: SKIP-only stub; un-skip when WI 4.4 lands")
}
