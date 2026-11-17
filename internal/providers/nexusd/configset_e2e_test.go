// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-84, TC-RA-85, TC-RA-86, TC-RA-87.
// Owner WIs: 4.4. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package nexusd

import "testing"

// TC-RA-84: The T0 end-to-end matrix holds the FR-8 post-condition for every outcome. Owner WI 4.4. Requirements: FR-7, FR-8, NFR-4, NFR-7.
//
//   - Setup: The 4.4 config-set stack: the real ops manager, the detached process backend running the real `Helper` in process with F-SYS and F-RELOAD, and a temp `ConfigPath`.
//   - Steps: One subtest per row, each through REST and `awaitOp`.
//     | Row | Action | Outcome |
//     |---|---|---|
//     | (a) | `providers.host.enabled=true` | success |
//     | (b) | `tls.mode=auto` on a `tls.mode: off` loopback file | success with restart required |
//     | (c) | `audit.retentionDays=0` | 422 |
//     | (d) | the file changed after Prepare | conflict |
//     | (e) | `refuseApply(1)` | restored |
//     | (f) | `noAnswer(2)` | restore failed |
//     | (g) | the current value | no change |
//   - Expected:
//     | Row | Expected |
//     |---|---|
//     | (a) | `succeeded`, `restartRequired []` |
//     | (b) | `succeeded`, `restartRequired ["tls"]`, lock-out warning |
//     | (c) | 422 `config-invalid` with `errors[0].path == "audit.retentionDays"`, no operation, launches 0 |
//     | (d) | `failed config-conflict`, disk equals the changed file |
//     | (e) | `failed config-apply-failed`, `restored: true`, disk equals the original bytes |
//     | (f) | `failed config-restore-failed`, `backupPath`, detail with `docs/install.md#recovering-from-a-failed-configuration-change` |
//     | (g) | `succeeded changed:false`, `reloadCalls 0` |
//     PC holds in every row.
func TestTCRA84_TheT0EndToEndMatrixHoldsTheFR8PostConditionFor(t *testing.T) {
	t.Skip("TC-RA-84: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-RA-85: The lane serializes config set with reload, restart and update in both directions. Owner WI 4.4. Requirements: FR-9.
//
//   - Setup: A stack with the launch spy, so a launched op stays Running until its `exit.json` is written.
//   - Steps:
//     1. Start a `reload` and keep it running. Then run config set.
//     2. End it. Start a config set and keep it running. Then run `reload`, `restart`, `update` (upload stub) and a second config set.
//   - Expected:
//   - Step 1: config set gives 409 `conflict`, naming the reload's operation ID.
//   - Step 2: each of the four gives 409, naming the config set's operation ID.
//   - No side effects: no refused call created an operation or a launch.
func TestTCRA85_TheLaneSerializesConfigSetWithReloadRestartAnd(t *testing.T) {
	t.Skip("TC-RA-85: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-RA-86: A config set leaves a complete audit trail. Owner WI 4.4. Requirements: FR-13.
//
//   - Setup: The 4.4 stack.
//   - Steps:
//     1. Run TC-RA-84 rows (a) and (e).
//     2. Read `stack.auditOf(t, "actions.update-config")`.
//   - Expected:
//   - Outcomes: row (a) gives `requested`, `queued`, `running`, `succeeded`. Row (e) gives `requested`, `queued`, `running`, `failed` with slug `config-apply-failed`.
//   - Records: each `requested` has params with the new values and facts with `old.*` and `backupPath`. Each terminal record names the backup path.
func TestTCRA86_AConfigSetLeavesACompleteAuditTrail(t *testing.T) {
	t.Skip("TC-RA-86: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-RA-87: Through the full path, the shipped default file changes by one line. Owner WI 4.4. Requirements: NFR-3.
//
//   - Setup: The 4.4 stack with `ConfigPath` a copy of `deploy/nexusd.default.yaml` (with nexusd enabled).
//   - Steps:
//     1. Set `providers.host.enabled=true`.
//   - Expected:
//   - The line diff of the disk against `.bak` is exactly 1 line.
//   - The comment maps of the two are equal.
func TestTCRA87_ThroughTheFullPathTheShippedDefaultFileChangesBy(t *testing.T) {
	t.Skip("TC-RA-87: SKIP-only stub; un-skip when WI 4.4 lands")
}
