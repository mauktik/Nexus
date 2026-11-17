//go:build integration

// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-93, TC-RA-94, TC-RA-95, TC-RA-96, TC-RA-97, TC-RA-98, TC-RA-99, TC-RA-100, TC-RA-101, TC-RA-102, TC-RA-103, TC-RA-E26, TC-RA-ERR32, TC-RA-ERR33, TC-RA-ERR34, TC-RA-ERR35, TC-RA-ERR36, TC-RA-ERR39.
// Owner WIs: 5.2. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package integration

import "testing"

// TC-RA-93: The hardened unit is unchanged on the real host. Owner WI 5.2. Requirements: CON-1.
//
//   - Setup: `raHost`.
//   - Steps:
//     1. Compare `sha256sum /etc/systemd/system/nexusd.service` with the repository's `deploy/nexusd.service`.
//     2. `systemctl show nexusd -p ProtectSystem,ReadWritePaths,CapabilityBoundingSet,ReadOnlyPaths`.
//     3. `systemctl cat nexusd` (no drop-in).
//   - Expected:
//   - The sha is equal.
//   - `ProtectSystem=strict`, and `ReadWritePaths` does not contain `/etc` or `/etc/nexus`.
//   - `CapabilityBoundingSet` equals the value parsed from the unit file.
//   - `requireExactUnit` passes.
func TestTCRA93_TheHardenedUnitIsUnchangedOnTheRealHost(t *testing.T) {
	t.Skip("TC-RA-93: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-94: Config set on real systemd applies the change, writes the backup and reloads. Owner WI 5.2. Requirements: FR-7, FR-8, ASM-7, NFR-6.
//
//   - Setup: `raHost`. `orig := read /etc/nexus/nexusd.yaml`, `mainPID := unitPID`, `seq0 :=` the seq in `reload-status.json` (absent is 0).
//   - Steps:
//     1. `nexus nexusdconfig update providers.host.enabled=true`.
//     2. Wait for the operation.
//     3. `nexus hostsummary get`.
//     4. `nexus nexusdconfig get providers.host.enabled`.
//   - Expected:
//   - Operation: `succeeded`, with `changed: true`, `changes [{providers.host.enabled, false, true}]`, `restartRequired []` and `backupPath /etc/nexus/nexusd.yaml.bak`.
//   - File: `/etc/nexus/nexusd.yaml` is `root:root` 0600 and decodes `providers.host.enabled: true`.
//   - Backup: `.bak` is `root:root` 0600 with bytes equal to `orig`.
//   - Reload status: `reload-status.json` has `seq == seq0+1`, `applied: true` and `sha256 == sha256sum(file)` (PC).
//   - Unit: `MainPID` is unchanged.
//   - Journal: `journalctl -u nexusd` in the window shows the `Reloading` and `Reloaded` lines.
//   - Effect: step 3 exits 0, and step 4 shows `file true`.
//   - Units: a `nexus-op-<id>` unit ran and is gone (`waitOpUnitsIdle`).
func TestTCRA94_ConfigSetOnRealSystemdAppliesTheChangeWritesThe(t *testing.T) {
	t.Skip("TC-RA-94: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-95: Comments in the installed default file survive a real config set. Owner WI 5.2. Requirements: NFR-3, NFR-6.
//
//   - Setup: TC-RA-94's end state.
//   - Steps:
//     1. Diff `/etc/nexus/nexusd.yaml` against `.bak`.
//     2. Build the comment map of both with `yaml.v3` in the test.
//   - Expected:
//   - The diff is exactly one changed line, the `enabled` line under `providers.host`.
//   - The comment maps are equal.
//   - Every comment line in `.bak` (`grep -c '#'`) is present in the new file.
func TestTCRA95_CommentsInTheInstalledDefaultFileSurviveAReal(t *testing.T) {
	t.Skip("TC-RA-95: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-96: Adding an absent key to a hand-commented file on the real host keeps every comment attached. Owner WI 5.2. Requirements: NFR-3, FR-7.
//
//   - Setup:
//   - `raHost`. Install `test/harness/fixtures/nexusd-commented.yaml` as `/etc/nexus/nexusd.yaml` (root 0600), with `providers.pihole` removed.
//   - `systemctl reload nexusd` and wait for the status.
//   - Steps:
//     1. `nexus nexusdconfig update providers.pihole.enabled=false`.
//   - Expected:
//   - `succeeded`.
//   - The comment maps of the file and of `.bak` are equal for every pre-existing key.
//   - The new key is present, and PC holds.
//   - The differences outside the new lines are only the documented normalization (TC-RA-51's comparison).
func TestTCRA96_AddingAnAbsentKeyToAHandCommentedFileOnTheReal(t *testing.T) {
	t.Skip("TC-RA-96: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-97: A config set that changes nothing touches nothing on the real host. Owner WI 5.2. Requirements: FR-7.
//
//   - Setup: `raHost`. Record the inode, mtime and sha of the file, and the status seq. No `.bak` exists.
//   - Steps:
//     1. `nexus nexusdconfig update providers.nexusd.enabled=true`.
//   - Expected: `succeeded changed:false`. The inode, mtime and sha are unchanged, no `.bak` exists, and the status seq is unchanged.
func TestTCRA97_AConfigSetThatChangesNothingTouchesNothingOnThe(t *testing.T) {
	t.Skip("TC-RA-97: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-98: A restart-only key is written and reported, never applied by a restart. Owner WI 5.2. Requirements: FR-8, ASM-4, ASM-3.
//
//   - Setup: `raHost`. `pid0 := unitPID`.
//   - Steps:
//     1. `nexus nexusdconfig update auth.failureLimitPerMinute=20 audit.retentionDays=30`.
//   - Expected:
//   - Operation: `succeeded` with `restartRequired ["audit","auth"]`.
//   - Warnings: stderr has the lock-out warning for `auth.failureLimitPerMinute` and the `run nexus nexusd restart to apply` note.
//   - No restart: `unitPID == pid0`, and `NRestarts` is unchanged.
//   - File: it holds both values.
//   - Reload status: `restartRequired` lists both keys.
func TestTCRA98_ARestartOnlyKeyIsWrittenAndReportedNeverApplied(t *testing.T) {
	t.Skip("TC-RA-98: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-99: The nexusd lane refuses a reload while a config set runs on the real host. Owner WI 5.2. Requirements: FR-9.
//
//   - Setup: `raHost`.
//   - Steps:
//     1. Start `nexus nexusdconfig update providers.host.enabled=true --no-wait`, take the operation ID, and enter the held window.
//     2. At the new-digest point, `kill -STOP` the helper (the `MainPID` of `nexus-op-<id>`, from `systemctl show`) and check that its state is `T`.
//     3. `kill -CONT` the daemon, which handles the pending SIGHUP and writes the status. The API is now up while the operation is still Running.
//     4. Run `nexus nexusd reload`, `nexus nexusd restart --yes` and a second `nexus nexusdconfig update audit.retentionDays=30`.
//     5. `kill -CONT` the helper and await the operation.
//     6. `nexus nexusd reload`.
//     7. Do steps 2 to 5 within 10 s of the new-digest point, inside the helper's 20 s wait.
//   - Expected:
//   - Step 4: each call is refused with 409 `conflict`, naming the config set's operation ID, and creates no operation. The MainPID is unchanged.
//   - Step 5: `succeeded`, with PC.
//   - Step 6: succeeds.
func TestTCRA99_TheNexusdLaneRefusesAReloadWhileAConfigSetRunsOn(t *testing.T) {
	t.Skip("TC-RA-99: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-100: Token secrets never reach state.db, the audit log, the journal or the operation endpoints on the real host. Owner WI 5.2. Requirements: NFR-1, FR-1, FR-13, NFR-6.
//
//   - Setup: `raHost`. `since := smWindow()`.
//   - Steps:
//     1. REST `POST /v1/providers/nexusd/tokens/t1rest/add`, keeping the body and headers.
//     2. `nexus nexusdtoken add t1cli --yes`, keeping stdout and stderr.
//     3. A REST replay of step 1 with an `Idempotency-Key`.
//     4. Remove both tokens.
//     5. Sweep the sinks as root: `grep -c` over `/var/lib/nexus/state.db`, `grep -rc` over `/var/lib/nexus/ops/`, `grep -c` over `/var/log/nexus/audit.jsonl*`, and `journalctl -u nexusd --since $since`. Then the bodies of `GET /v1/operations` (all pages), `GET /v1/system/audit?limit=1000` and `nexus operation get` on every listed ID, plus CLI stderr.
//   - Expected:
//   - Each sink has 0 occurrences of each token string, its secret part and its hash hex. The hash is read from `tokens.json` before step 4.
//   - Step 1 carries `Cache-Control: no-store`.
//   - Step 3 gives 422 `validation-failed`, and the token count is unchanged.
//   - No operation has an op ID under `nexusd.tokens.*`.
//   - Non-vacuity: the step 1 body and the step 2 stdout contain their secret, and `tokens.json` contains both hashes before step 4.
func TestTCRA100_TokenSecretsNeverReachStateDbTheAuditLogThe(t *testing.T) {
	t.Skip("TC-RA-100: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-101: Remote add and local sudo create of one name race on the real host, and add and revoke apply on the next request. Owner WI 5.2. Requirements: FR-4, FR-1, FR-3, NFR-6.
//
//   - Setup: `raHost`.
//   - Steps:
//     1. Do 8 rounds, which keeps the successful remote mutations at most 8 + 2 within the window. Each round launches together a REST add of `race<i>` and a host `nexusd token create --name race<i>`, the existing CLI run as root.
//     2. Remote-add `nx`, then use it on `GET .../daemons`.
//     3. Remote-remove `nx`, then use it again.
//     4. `nexus nexusdtoken get` with `self`.
//   - Expected:
//   - Step 1: exactly one winner per round. `tokens.json` is valid JSON, `root:root` 0600, and has one `race<i>` record.
//   - Sandbox: the journal has no `EACCES` or sandbox denial for `tokens.json.lock`.
//   - Steps 2 and 3: 200 then 401.
//   - Step 4: `self` is true only for `ra`.
func TestTCRA101_RemoteAddAndLocalSudoCreateOfOneNameRaceOnThe(t *testing.T) {
	t.Skip("TC-RA-101: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-102: NFR-2 latencies on the real host. Owner WI 5.2. Requirements: NFR-2.
//
//   - Setup: `raHost`, with 100 tokens created on the host through `nexusd token create`, so the remote rate limit is not used.
//   - Steps:
//     1. Time 50 lists and 50 `config get` calls.
//     2. Time 50 adds and 50 removes, in batches of 5 add+remove pairs (10 mutations). Between batches, run `systemctl restart nexusd` and wait for `/healthz`, which resets the in-memory window (A-1). Only the request wall time counts.
//     3. Time one config set with a passing reload, from 202 to terminal.
//     4. Time one config set that restores (TC-RA-ERR39's method), from 202 to terminal, minus the deliberate hold.
//   - Expected:
//   - The p95 of each op type is ≤ 200 ms.
//   - Step 3 is ≤ 15 s + the reload's own time. The reload time is the difference of `at` between the baseline and new statuses, measured against the journal timestamps.
//   - Step 4 is ≤ 30 s after the hold.
//   - The `NFR` lines are logged.
func TestTCRA102_NFR2LatenciesOnTheRealHost(t *testing.T) {
	t.Skip("TC-RA-102: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-103: No sandbox denial, crash or restart during the T1 run. Owner WI 5.2. Requirements: CON-1, NFR-6.
//
//   - Setup: Runs last in the file, over the window since the first `TestTCRA` start.
//   - Steps:
//     1. Scan `journalctl -u nexusd -u 'nexus-op-*'` and the kernel log for `Permission denied`, `EACCES`, `EROFS`, `audit: type=1400` and `code=dumped`.
//     2. Read `NRestarts`.
//   - Expected:
//   - 0 matches. Non-vacuity: the window has at least one `Reloaded` line.
//   - `NRestarts` is unchanged from the run's start, except for the restart that TC-RA-98 avoided.
func TestTCRA103_NoSandboxDenialCrashOrRestartDuringTheT1Run(t *testing.T) {
	t.Skip("TC-RA-103: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-E26: On real systemd, an unconfirmed reload is restored and confirmed. Owner WI 5.2. Requirements: FR-8, NFR-6.
//
//   - Setup: `raHost`. `orig := sha(file)`.
//   - Steps:
//     1. `update --no-wait providers.host.enabled=true`, then enter the held window, up to the new-digest point.
//     2. Keep the daemon stopped.
//     3. Sleep 25 s, which is longer than the 20 s `reloadTimeout`, then `kill -CONT`.
//     4. Await.
//   - Expected:
//   - `failed config-apply-failed` with `restored: true`.
//   - `sha(file) == orig`, mode 0600.
//   - `reload-status.json` is `applied` with `sha256 == orig`.
//   - The daemon serves `GET /healthz`.
func TestTCRAE26_OnRealSystemdAnUnconfirmedReloadIsRestoredAnd(t *testing.T) {
	t.Skip("TC-RA-E26: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-ERR32: On real systemd, a validation refusal starts no unit and touches nothing. Owner WI 5.2. Requirements: FR-7, NFR-6.
//
//   - Setup: `raHost`. Record the file sha, the `.bak` presence, the status seq, and `systemctl list-units 'nexus-op-*' --all`.
//   - Steps:
//     1. `nexus nexusdconfig update audit.retentionDays=0`.
//     2. `nexus nexusdconfig update providers.host.enabeld=true`.
//     3. `nexus nexusdconfig update providers.host.enabled=yes`.
//   - Expected:
//   - Each is refused with 422 `config-invalid`, and the CLI exits with the scheme's code and prints the located error.
//   - The file sha, `.bak` absence and status seq are unchanged.
//   - No new `nexus-op-*` unit appears in the journal window.
//   - `GET /v1/operations` has no new operation.
func TestTCRAERR32_OnRealSystemdAValidationRefusalStartsNoUnitAnd(t *testing.T) {
	t.Skip("TC-RA-ERR32: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-ERR33: On real systemd, each deny-list entry is refused with its reason. Owner WI 5.2. Requirements: FR-12, NFR-6.
//
//   - Setup: As TC-RA-ERR32.
//   - Steps:
//     1. `providers.nexusd.update.trustedKeys=x`.
//     2. `providers.nexusd.enabled=false`.
//     3. `providers.scripts.catalog=x`.
//   - Expected:
//   - 422, with each message containing "cannot be set remotely" and the table's reason.
//   - The file sha, `.bak` and status are unchanged, and no `nexus-op-*` unit runs.
//   - `/var/log/nexus/audit.jsonl` has a `refused` record per call with slug `config-invalid`.
func TestTCRAERR33_OnRealSystemdEachDenyListEntryIsRefusedWithIts(t *testing.T) {
	t.Skip("TC-RA-ERR33: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-ERR34: On real systemd, a reload refused at load restores the backup and confirms the restore. Owner WI 5.2. Requirements: FR-8, NFR-6, ASM-7.
//
//   - Setup: `raHost`. `orig := sha(file)`.
//   - Steps:
//     1. `update --no-wait providers.host.enabled=true`, then enter the held window, up to the new-digest point.
//     2. Keep the daemon stopped.
//     3. `chmod 0644 /etc/nexus/nexusd.yaml`, so root's `ReadFile` will refuse it.
//     4. `kill -CONT`.
//     5. Await.
//   - Expected:
//   - Operation: `failed config-apply-failed`, with `restored: true` and stage `load`.
//   - Disk: `sha(file) == orig`, mode 0600 `root:root`. `.bak` equals the original.
//   - Reload status: the last `reload-status.json` has `applied: true` and `sha256 == orig`. The status before it has `applied: false`, `stage: load` and `sha256: ""`, observed in the journal.
//   - Daemon: `nexus nexusd get` succeeds, and `providers.host.enabled` is still false in force (`hostsummary get` is refused as disabled).
func TestTCRAERR34_OnRealSystemdAReloadRefusedAtLoadRestoresThe(t *testing.T) {
	t.Skip("TC-RA-ERR34: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-ERR35: On real systemd, a file replaced before the reload ends config-conflict without a restore. Owner WI 5.2. Requirements: FR-8, FR-7, NFR-6.
//
//   - Setup: `raHost`. `other` is the installed file with `audit.retentionDays: 45`, written to a temp path, root 0600.
//   - Steps:
//     1. `update --no-wait providers.host.enabled=true`.
//     2. Enter the held window, right after the 202, up to the new-digest point.
//     3. `install -m 0600 -o root other /etc/nexus/nexusd.yaml`.
//     4. `kill -CONT`.
//     5. Await.
//   - Expected:
//   - `failed config-conflict`, with `sha(file) == sha(other)` (not restored).
//   - `reload-status.json` has `applied: true` and `sha256 == sha(other)`, so PC (a) holds for the replacement.
func TestTCRAERR35_OnRealSystemdAFileReplacedBeforeTheReloadEnds(t *testing.T) {
	t.Skip("TC-RA-ERR35: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-ERR36: On real systemd, a restore that cannot be confirmed ends config-restore-failed, and the daemon later loads the restored file. Owner WI 5.2. Requirements: FR-8, NFR-6.
//
//   - Setup: `raHost`. `orig := sha(file)`.
//   - Steps:
//     1. `update --no-wait providers.host.enabled=true`.
//     2. Enter the held window, right after the 202, up to the new-digest point. Keep the daemon stopped for 50 s, which is more than two 20 s waits plus the retries.
//     3. `kill -CONT`.
//     4. Await.
//     5. Poll `reload-status.json` for 10 s.
//   - Expected:
//   - Operation: `failed config-restore-failed`. The `backupPath` extension is `/etc/nexus/nexusd.yaml.bak`, and the detail names the `docs/install.md` recovery anchor.
//   - Disk: `sha(file) == orig`, because the backup bytes were written back.
//   - After CONT: a status with `applied: true` and `sha256 == orig`, so the disk again equals the configuration in force.
//   - Unit: the `nexus-op-*` unit ended within `MaxRuntime` (130 s).
func TestTCRAERR36_OnRealSystemdARestoreThatCannotBeConfirmedEnds(t *testing.T) {
	t.Skip("TC-RA-ERR36: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-RA-ERR39: On real systemd, a refused reload during a held window is restored within the NFR-2 budget. Owner WI 5.2. Requirements: NFR-2, FR-8.
//
//   - Setup: As TC-RA-ERR34, with timestamps taken at the 202, at CONT and at terminal.
//   - Steps:
//     1. Run TC-RA-ERR34's steps with timing.
//   - Expected: `terminal − CONT ≤ 30 s`. The logged `NFR` line feeds TC-RA-102.
func TestTCRAERR39_OnRealSystemdARefusedReloadDuringAHeldWindowIs(t *testing.T) {
	t.Skip("TC-RA-ERR39: SKIP-only stub; un-skip when WI 5.2 lands")
}
