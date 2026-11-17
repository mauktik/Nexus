// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-70, TC-RA-72, TC-RA-73, TC-RA-74, TC-RA-75, TC-RA-76, TC-RA-E18, TC-RA-E19, TC-RA-E20, TC-RA-E21, TC-RA-E27, TC-RA-ERR19, TC-RA-ERR20, TC-RA-ERR21, TC-RA-ERR22, TC-RA-ERR23, TC-RA-ERR24, TC-RA-ERR25.
// Owner WIs: 4.2. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package nexusd

import "testing"

// TC-RA-70: A successful config set writes the edit, a 0600 backup and the result, and preserves mode and owner. Owner WI 4.2. Requirements: FR-7, ASM-7, NFR-5.
//
//   - Setup:
//   - F-SYS with F-RELOAD over a temp `ConfigPath` holding the default file with `providers.nexusd.enabled: true`. Mode variants: 0600, 0400 and 0640.
//   - `input.json` as `prepareConfigSet` writes it: `Changes ["providers.host.enabled=true"]`, `BaseSHA256 = sha(file)`.
//   - `check` is the real `CheckConfigDataForTest` wrapper.
//   - Steps:
//     1. `Helper([service-helper --dir D], ..., check)`.
//   - Expected:
//   - Disk: the file equals `config.Edit(orig, ...)`'s bytes. `.bak` equals the original bytes, with mode 0600.
//   - Mode: the file mode is `orig & 0600`: 0600 stays 0600, 0400 stays 0400, and 0640 becomes 0600.
//   - Result: `result.json` has `changed:true`, `configChanges [{key, old:false, new:true}]`, `restartRequired []` and `backupPath = ConfigPath+".bak"`.
//   - Reload: F-RELOAD `reloadCalls == 1`.
//   - PC: `sha(disk) == F-RELOAD.applied sha`.
func TestTCRA70_ASuccessfulConfigSetWritesTheEditA0600BackupAnd(t *testing.T) {
	t.Skip("TC-RA-70: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-72: An inactive unit is retried until the reload is confirmed. Owner WI 4.2. Requirements: FR-8.
//
//   - Setup: As TC-RA-70, with F-RELOAD `inactiveFor(3)`, on `virtClock`.
//   - Steps:
//     1. Run the helper.
//   - Expected:
//   - Success with PC.
//   - `reloadCalls == 4`, spaced by `reloadRetryInterval` (1 s) on the virtual clock.
//   - Total virtual time is under `reloadTimeout`.
func TestTCRA72_AnInactiveUnitIsRetriedUntilTheReloadIsConfirmed(t *testing.T) {
	t.Skip("TC-RA-72: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-73: Kill at every helper step leaves the file whole, either old or new. Owner WI 4.2. Requirements: NFR-4, FR-8.
//
//   - Setup: As TC-RA-70. `beforeStep` returns `errKilled` at one of `backup`, `after-backup`, `after-tmp`, `after-rename` or `after-reload`.
//   - Steps:
//     1. For each step, run the helper on a fresh copy.
//   - Expected:
//   - Disk: `sha(ConfigPath)` is `sha(old)` for the first three steps and `sha(new)` for the last two. It is never another value, and the file is never empty.
//   - No result: `result.json` is absent.
//   - Leftovers: any leftover `.nexusd.yaml.*.tmp` is removed by the next helper run (TC-RA-E20).
//   - Recovery: in the stack, the operation ends `outcome-unknown` with `LostDetail` (TC-RA-80).
func TestTCRA73_KillAtEveryHelperStepLeavesTheFileWholeEitherOld(t *testing.T) {
	t.Skip("TC-RA-73: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-74: The helper writes only the config file, its backup and their temporary files. Owner WI 4.2. Requirements: NFR-5, CON-1.
//
//   - Setup: As TC-RA-70, with the config dir, state dir and op dir listed recursively (name, size, mtime) before the run. One run succeeds, and one is refused by `refuseApply(1)` and restored.
//   - Steps:
//     1. Run the helper.
//     2. List again.
//   - Expected:
//   - Config dir: the only changed or new entries are `nexusd.yaml` and `nexusd.yaml.bak`. No `*.tmp` remains.
//   - Op dir: only `result.json` and the op log are new.
//   - State dir: only `reload-status.json` changed, written by F-RELOAD, not by the helper.
func TestTCRA74_TheHelperWritesOnlyTheConfigFileItsBackupAnd(t *testing.T) {
	t.Skip("TC-RA-74: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-75: The helper's step lines name keys and the backup path, never values. Owner WI 4.2. Requirements: NFR-1, FR-13.
//
//   - Setup: As TC-RA-70, with `host.name=RA-VALUE-SENTINEL` and its stderr captured.
//   - Steps:
//     1. Run the helper.
//   - Expected:
//   - stderr has `step` lines naming `host.name` and the `.bak` path.
//   - `RA-VALUE-SENTINEL` occurs 0 times in stderr.
//   - Non-vacuity: `result.json` `configChanges[0].new == "RA-VALUE-SENTINEL"`.
func TestTCRA75_TheHelperSStepLinesNameKeysAndTheBackupPathNever(t *testing.T) {
	t.Skip("TC-RA-75: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-76: problem() maps config-invalid, restored and backupPath from the helper result. Owner WI 4.2. Requirements: FR-8, CON-2.
//
//   - Setup: `helperResult` values with these slugs: `config-invalid` (with `configPath` and `errors`), `config-apply-failed` (`restored:true`, `backupPath`), `config-restore-failed` (`backupPath`), `config-conflict`, and an unregistered slug.
//   - Steps:
//     1. Call `problem()` on each.
//   - Expected:
//   - Each registered slug maps to itself, with exactly the extensions its type declares.
//   - The unregistered slug still maps to `internal-error`.
func TestTCRA76_ProblemMapsConfigInvalidRestoredAndBackupPath(t *testing.T) {
	t.Skip("TC-RA-76: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-E18: The coalesced-signal case is still judged by digest. Owner WI 4.2. Requirements: FR-8.
//
//   - Setup: As TC-RA-70, with F-RELOAD `coalesce` and a SIGHUP already pending, sent by the test before the helper's `Reload`.
//   - Steps:
//     1. Run the helper.
//   - Expected: Success, because the one status written covers the new bytes (`sha == sha(out)`), and PC holds. F-RELOAD handled exactly 1 reload for the 2 signals.
func TestTCRAE18_TheCoalescedSignalCaseIsStillJudgedByDigest(t *testing.T) {
	t.Skip("TC-RA-E18: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-E19: A change landing after the helper's backup ends config-conflict with only the backup written. Owner WI 4.2. Requirements: FR-7, ASM-7.
//
//   - Setup: As TC-RA-70. `beforeStep("after-backup")` rewrites `ConfigPath` with other valid bytes and returns nil.
//   - Steps:
//     1. Run the helper.
//   - Expected: `config-conflict`. The disk holds the other bytes, `.bak` holds the original bytes, and `reloadCalls == 0`.
func TestTCRAE19_AChangeLandingAfterTheHelperSBackupEndsConfig(t *testing.T) {
	t.Skip("TC-RA-E19: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-E20: Stale temporary files from an earlier crash are removed. Owner WI 4.2. Requirements: NFR-4, NFR-5.
//
//   - Setup: The config dir holds `.nexusd.yaml.123.tmp`, `.nexusd.yaml.bak.9.tmp` and an unrelated `notes.tmp`.
//   - Steps:
//     1. Run a successful helper.
//   - Expected: The two `.nexusd.yaml*.tmp` files are gone, and `notes.tmp` is still present.
func TestTCRAE20_StaleTemporaryFilesFromAnEarlierCrashAreRemoved(t *testing.T) {
	t.Skip("TC-RA-E20: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-E21: helperInput and helperResult stay additive. Owner WI 4.2. Requirements: CON-2.
//
//   - Setup: An update-mode `input.json` and `result.json` from `helper_test.go`'s existing fixtures.
//   - Steps:
//     1. Decode them with the new structs.
//     2. Re-marshal an update-mode result.
//   - Expected:
//   - Decoding succeeds.
//   - The re-marshalled JSON has no `configPath`, `configChanges`, `restartRequired`, `backupPath` or `restored` member (omitempty).
//   - `modeOf` still lists every previous mode plus `config-set`.
func TestTCRAE21_HelperInputAndHelperResultStayAdditive(t *testing.T) {
	t.Skip("TC-RA-E21: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-E27: The worst-case unconfirmed-then-restored path completes within NFR-2's 30 s. Owner WI 4.2. Requirements: NFR-2, FR-8.
//
//   - Setup:
//   - As TC-RA-70, on `virtClock`.
//   - F-RELOAD `noAnswer(1)`, so the first reload is unconfirmed.
//   - F-RELOAD `delay(1, reloadTimeout − reloadPollInterval)`, so the restore's reload is handled at the last poll before the second wait expires.
//   - `t0` is the virtual time when `Helper` starts.
//   - Steps:
//     1. Run the helper.
//   - Expected:
//   - `config-apply-failed` with `restored: true`, and PC holds. This is the slowest path that still restores.
//   - NFR-2: the virtual time from `t0` to `result.json` is ≤ 30 s. NFR-2 says "A config set that restores completes within 30 s", and helper time is the whole of that budget except Prepare and launch. With A-3's 20 s `reloadTimeout`, the path takes about 40 s and this case fails until the timeouts or NFR-2 are reconciled (review Finding 7).
//   - The bound is computed from the constants the helper uses, never typed. A second run adds F-RELOAD `inactiveFor(2)` before the first reload, and its time from `t0` to `result.json` is also ≤ 30 s, so the inactive retries cannot push the path past the budget either.
func TestTCRAE27_TheWorstCaseUnconfirmedThenRestoredPathCompletes(t *testing.T) {
	t.Skip("TC-RA-E27: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-ERR19: A config-base mismatch at the helper ends config-conflict with nothing written. Owner WI 4.2. Requirements: FR-7.
//
//   - Setup: As TC-RA-70, but the file is rewritten after `input.json` is written, so `BaseSHA256 ≠ sha(file)`.
//   - Steps:
//     1. Run the helper.
//   - Expected: `config-conflict`. The disk equals the rewritten bytes, no `.bak` exists, and `reloadCalls == 0`.
func TestTCRAERR19_AConfigBaseMismatchAtTheHelperEndsConfigConflict(t *testing.T) {
	t.Skip("TC-RA-ERR19: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-ERR20: A helper re-validation failure ends config-invalid with nothing written. Owner WI 4.2. Requirements: FR-7, ASM-10.
//
//   - Setup: As TC-RA-70, with `check` a stub returning one `ConfigError{Path:"x", Message:"refused by helper"}`. This is the only stubbed validator, used because admission and the helper must disagree (ASM-10).
//   - Steps:
//     1. Run the helper.
//   - Expected: `result.json` has slug `config-invalid` with the error. The disk is unchanged, no `.bak` exists, and `reloadCalls == 0`.
func TestTCRAERR20_AHelperReValidationFailureEndsConfigInvalidWith(t *testing.T) {
	t.Skip("TC-RA-ERR20: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-ERR21: A reload refused at stage apply restores the backup and ends config-apply-failed. Owner WI 4.2. Requirements: FR-8.
//
//   - Setup: As TC-RA-70, with F-RELOAD `refuseApply(1)`.
//   - Steps:
//     1. Run the helper.
//   - Expected:
//   - Result: `config-apply-failed`, `restored: true`, `backupPath`, and the first reload's `stage apply` and error.
//   - Disk: byte-equal to the original, mode as the original.
//   - Reloads: `reloadCalls == 2`.
//   - PC: `sha(disk) == F-RELOAD.applied sha`, where the original is still applied.
func TestTCRAERR21_AReloadRefusedAtStageApplyRestoresTheBackupAnd(t *testing.T) {
	t.Skip("TC-RA-ERR21: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-ERR22: A reload refused at stage load with an empty digest restores rather than reporting a conflict. Owner WI 4.2. Requirements: FR-8.
//
//   - Setup: As TC-RA-70, with F-RELOAD `loadRefuse(1)`, which writes `SHA256: ""`.
//   - Steps:
//     1. Run the helper.
//   - Expected: `config-apply-failed restored:true` with stage `load`, never `config-conflict`. The disk equals the original, and PC holds.
func TestTCRAERR22_AReloadRefusedAtStageLoadWithAnEmptyDigest(t *testing.T) {
	t.Skip("TC-RA-ERR22: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-ERR23: An unconfirmed reload is restored, and an unconfirmed restore ends config-restore-failed. Owner WI 4.2. Requirements: FR-8.
//
//   - Setup: As TC-RA-70 on `virtClock`. Run A uses `noAnswer(1)`. Run B uses `noAnswer(2)`.
//   - Steps:
//     1. Run the helper for each run.
//   - Expected:
//   - Run A: after `reloadTimeout` (the package constant, on the virtual clock), the backup is restored and confirmed. `config-apply-failed restored:true`, and PC holds.
//   - Run B: `config-restore-failed`, with `backupPath` and the detail naming `docs/install.md#recovering-from-a-failed-configuration-change`. The disk equals the backup bytes, written but unconfirmed. Total virtual time is ≤ `configSetMaxRuntime` (130 s).
func TestTCRAERR23_AnUnconfirmedReloadIsRestoredAndAnUnconfirmed(t *testing.T) {
	t.Skip("TC-RA-ERR23: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-ERR24: A file replaced before the reload ends config-conflict with no restore. Owner WI 4.2. Requirements: FR-8, FR-7.
//
//   - Setup: As TC-RA-70, with F-RELOAD `replaceBeforeHandle(otherValidBytes)`.
//   - Steps:
//     1. Run the helper.
//   - Expected: `config-conflict`. The disk equals `otherValidBytes`, which are not restored, and `reloadCalls == 1`.
func TestTCRAERR24_AFileReplacedBeforeTheReloadEndsConfigConflict(t *testing.T) {
	t.Skip("TC-RA-ERR24: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-RA-ERR25: A nil validator makes the config-set mode refuse to start. Owner WI 4.2. Requirements: FR-7, CON-2.
//
//   - Setup: A `config-set` `input.json`.
//   - Steps:
//     1. `Helper(args, out, errw, nil)`.
//   - Expected: Exit 1. stderr has the "no validator" text, no `result.json` exists, the config dir is unchanged, and `reloadCalls == 0`. An update-mode input with a nil check still runs, so the nil check is scoped to config-set.
func TestTCRAERR25_ANilValidatorMakesTheConfigSetModeRefuseToStart(t *testing.T) {
	t.Skip("TC-RA-ERR25: SKIP-only stub; un-skip when WI 4.2 lands")
}
