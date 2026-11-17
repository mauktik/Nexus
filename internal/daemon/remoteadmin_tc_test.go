// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-56, TC-RA-57, TC-RA-58, TC-RA-59, TC-RA-ERR16.
// Owner WIs: 3.2. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package daemon

import "testing"

// TC-RA-56: Every reload, SIGHUP or API, writes a status with the digest of the bytes read and the next seq. Owner WI 3.2. Requirements: FR-8, NFR-4.
//
//   - Setup: `startInProcess` with a valid config file. The test computes `sha256` of the file bytes it writes.
//   - Steps:
//     1. Rewrite the file with `providers.host.enabled` flipped, then send SIGHUP.
//     2. Rewrite it with `audit.retentionDays` changed, then call `d.Reload(ctx)`.
//   - Expected:
//   - After step 1: `reloadstatus.Read(stateDir)` gives `Seq == 1`, `SHA256 == sha(bytes1)`, `Applied == true` and `RestartRequired == []`.
//   - After step 2: `Seq == 2`, `SHA256 == sha(bytes2)`, and `RestartRequired == ["audit"]`, sorted and equal to the `ReloadResult` that `Reload` returned.
//   - `Boot` is the same non-empty 16-hex value in both.
func TestTCRA56_EveryReloadSIGHUPOrAPIWritesAStatusWithTheDigest(t *testing.T) {
	t.Skip("TC-RA-56: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-RA-57: A refused reload records its stage and digest, and the configuration in force is unchanged. Owner WI 3.2. Requirements: FR-8, NFR-4.
//
//   - Setup: As TC-RA-56, plus a second provider scripted to refuse its next `Configure` (the `sectionProvider` pattern).
//   - Steps:
//     1. Write a syntactically invalid YAML file and send SIGHUP.
//     2. Remove the file and send SIGHUP.
//     3. Write a valid file that the scripted provider refuses, then call `Reload`.
//   - Expected:
//   - Step 1: `Applied: false`, `Stage: load`, and `SHA256 == sha(invalid bytes)`.
//   - Step 2: `Stage: load` and `SHA256 == ""`.
//   - Step 3: `Stage: apply`, `SHA256 == sha(bytes)`, and a non-empty `Error`.
//   - Throughout: `seq` advances by 1 each time, and the configuration in force, read through the provider's settings, stays the pre-step value.
func TestTCRA57_ARefusedReloadRecordsItsStageAndDigestAndThe(t *testing.T) {
	t.Skip("TC-RA-57: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-RA-58: The boot ID differs per daemon start. Owner WI 3.2. Requirements: FR-8.
//
//   - Setup: Two `startInProcess` runs over the same state dir, one after the other.
//   - Steps:
//     1. Reload once in each run.
//   - Expected: The two statuses have different `Boot` values, and each has `Seq == 1`.
func TestTCRA58_TheBootIDDiffersPerDaemonStart(t *testing.T) {
	t.Skip("TC-RA-58: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-RA-59: CheckConfigData on a file's bytes equals CheckConfig on the file. Owner WI 3.2. Requirements: FR-7, CON-2.
//
//   - Setup: Every file in `internal/daemon/testdata/checkconfig/`.
//   - Steps:
//     1. Compare `CheckConfig(path)` with `CheckConfigData(os.ReadFile(path), path)`.
//   - Expected: Equal `ErrorList`s (path, line, column and message) and equal non-nil configs for the valid fixtures. `CheckConfig` is implemented as `ReadFile` + `CheckConfigData`, checked by a source grep for one per-provider loop.
func TestTCRA59_CheckConfigDataOnAFileSBytesEqualsCheckConfigOn(t *testing.T) {
	t.Skip("TC-RA-59: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-RA-ERR16: A reload-status write failure never changes the reload's outcome. Owner WI 3.2. Requirements: FR-8.
//
//   - Setup: `startInProcess`, with the state dir made unwritable to the status write (`reload-status.json` pre-created as a directory).
//   - Steps:
//     1. Call `Reload` with a valid change.
//   - Expected: `Reload` returns the normal `ReloadResult` with no error, and the change is applied. One Error-level log line names `reload-status`.
func TestTCRAERR16_AReloadStatusWriteFailureNeverChangesTheReloadS(t *testing.T) {
	t.Skip("TC-RA-ERR16: SKIP-only stub; un-skip when WI 3.2 lands")
}
