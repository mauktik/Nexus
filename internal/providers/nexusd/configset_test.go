// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-71, TC-RA-78, TC-RA-79, TC-RA-80, TC-RA-81, TC-RA-82, TC-RA-83, TC-RA-E22, TC-RA-E23, TC-RA-E30, TC-RA-ERR26, TC-RA-ERR27, TC-RA-ERR28, TC-RA-ERR29, TC-RA-ERR30, TC-RA-ERR31, TC-RA-ERR43.
// Owner WIs: 4.3. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package nexusd

import "testing"

// TC-RA-71: A config set that changes nothing is answered at admission with changed:false, with no operation and no helper unit. Owner WI 4.3. Requirements: FR-7.
//
//   - Setup: A stack with the launch spy, F-RELOAD and a temp `ConfigPath` holding the default file with `providers.nexusd.enabled: true`. Record the file's inode, mtime and sha, the operation count, the `ops/` directory listing and `launches`. FR-7 as written (A-14, D-7): a no-op set ends at admission with `200 {changed:false}`, and no operation, unit or idempotency entry exists.
//   - Steps:
//     1. `POST .../actions/update-config` with `{"changes":["providers.nexusd.enabled=true"]}` as `admin` (the value already in the file).
//     2. The same with two assignments that both equal the file: `["providers.nexusd.enabled=true","audit.retentionDays=<the file's value>"]`, the value read from the fixture with `yaml.v3`.
//     3. Step 1's body twice more with the header `Idempotency-Key: ra-noop-1`.
//   - Expected:
//   - Response: each answers `200`, never `202`, with a body that decodes into `spec.ConfigUpdateResult` with `changed: false`, `changes` holding one entry per assignment with `old` equal to `new` (the decoded file value), `restartRequired []`, no `backupPath`, and no `Location` or `Retry-After` header.
//   - No operation: the operation count and the `ops/` listing are unchanged, so no `input.json` was written.
//   - No unit: `launches == 0`.
//   - No idempotency entry (A-36): both step 3 calls answer `200` with the same body, and the ops store has no entry for `(admin's token ID, ra-noop-1)`.
//   - Nothing written or reloaded: the inode, mtime and sha are unchanged, no `.bak` exists, and F-RELOAD `reloadCalls == 0`.
func TestTCRA71_AConfigSetThatChangesNothingIsAnsweredAtAdmission(t *testing.T) {
	t.Skip("TC-RA-71: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-78: Admission accepts a valid change, and Prepare writes the config-set input. Owner WI 4.3. Requirements: FR-7, ASM-7.
//
//   - Setup: A stack with the launch spy and a temp `ConfigPath` (the default file with nexusd enabled). `orig := sha256(file bytes)` before the call.
//   - Steps:
//     1. `POST .../actions/update-config` with `{"changes":["providers.host.enabled=true"]}` as `admin`.
//     2. Read `ops/<id>/input.json`.
//   - Expected:
//   - Step 1: 202, and one launch with argv `[exe service-helper --dir D]`, `KillProcess`, and `MaxRuntime == configSetMaxRuntime`, which is `2*reloadTimeout + 30*time.Second` computed from the package constants and equals 54 s (A-5).
//   - `input.json`: `mode config-set`, `configPath`, `backupPath = configPath+".bak"`, `changes` as sent, `baseSha256 == orig` (admission's digest, handed through `ConfigUpdateInput.BaseSHA256`, D-7, A-15), `stateDir`, and `reloadTimeout` equal to the package constant (12 s, A-3). `noChange` is absent or false: a no-change set never reaches Prepare (TC-RA-71).
//   - Prepare does not re-edit: with `CheckConfigDataForTest` counting calls, admission validates once and Prepare adds no call (the helper re-validates, TC-RA-ERR20).
//   - Audit: the `requested` record's params carry the `changes` and no `baseSha256`.
func TestTCRA78_AdmissionAcceptsAValidChangeAndPrepareWritesThe(t *testing.T) {
	t.Skip("TC-RA-78: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-79: providers.nexusd.enabled=true is accepted although false is denied. Owner WI 4.3. Requirements: FR-12, ASM-3.
//
//   - Setup: As TC-RA-78. A file whose `host.name` is absent and whose `tls.mode` is `off` on a loopback `listen`.
//   - Steps:
//     1. `changes: ["providers.nexusd.enabled=true"]`.
//     2. `changes: ["host.name=x"]`.
//     3. `changes: ["tls.mode=auto"]`.
//   - Expected:
//   - Step 1: not refused by the deny-list (the `Value: "false"` entry does not match). Because the value is already true, the call is a no-op answered at admission: `200` with `changed: false`, no operation and `launches == 0` (TC-RA-71).
//   - Steps 2 and 3: the two settable string keys (D-10) are accepted: each answers `202` with one launch. Neither message contains "cannot be set remotely".
func TestTCRA79_ProvidersNexusdEnabledTrueIsAcceptedAlthough(t *testing.T) {
	t.Skip("TC-RA-79: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-80: A helper that dies without a result ends outcome-unknown with a detail naming both files. Owner WI 4.3. Requirements: NFR-4, FR-8.
//
//   - Setup: A stack with the launch spy. The test writes `exit.json` with exit 137 and no `result.json`.
//   - Steps:
//     1. Accept a config set.
//     2. End the unit.
//     3. Await.
//   - Expected: The operation ends `failed` with `outcome-unknown`. Its detail contains the config path, the `.bak` path, `nexusd validate-config` and `nexus nexusdconfig get`.
func TestTCRA80_AHelperThatDiesWithoutAResultEndsOutcomeUnknown(t *testing.T) {
	t.Skip("TC-RA-80: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-81: nexus nexusd get reports configSet next to selfManagement, and it follows availability. Owner WI 4.3. Requirements: FR-9.
//
//   - Setup: A stack with the availability seam set available.
//   - Steps:
//     1. `GET .../daemons`.
//     2. Flip the seam to `{false, "requires systemd (detected openrc)"}`.
//     3. GET again.
//   - Expected:
//   - Step 1: `configSet.available == true` and equals `selfManagement`.
//   - Step 3: `configSet.available == false`, with the same reason as `selfManagement`, so the field is recomputed on each call.
func TestTCRA81_NexusNexusdGetReportsConfigSetNextTo(t *testing.T) {
	t.Skip("TC-RA-81: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-82: The requested record carries the new values in params and the old values and backup path in facts. Owner WI 4.3. Requirements: FR-13, ASM-7.
//
//   - Setup: A file with `audit.retentionDays: 90`, without `providers.nginx.testTimeout`, and with `host.name` set to a 300-byte string. (The absent key is not path-valued: path-valued keys such as `providers.pihole.dnsFile` are denied, TC-RA-ERR27.)
//   - Steps:
//     1. Set `audit.retentionDays=30`, `providers.nginx.testTimeout=45s` and `host.name=short`.
//     2. Set `audit.retentionDays=30` again, now the file's value after step 1 has succeeded (a no-change set).
//   - Expected:
//   - Step 1 facts:
//   - `old.audit.retentionDays=90`;
//   - `old.providers.nginx.testTimeout=<absent>`;
//   - `old.host.name` holds the first 256 bytes of the old value, cut by the audit bound;
//   - `backupPath=<configPath>.bak`.
//   - Step 1 params: they carry the three new values.
//   - Step 2 facts (A-14): the `completed` record has `old.audit.retentionDays=30` and `changed=false`, and no `backupPath` fact, because nothing is backed up.
func TestTCRA82_TheRequestedRecordCarriesTheNewValuesInParamsAnd(t *testing.T) {
	t.Skip("TC-RA-82: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-83: A tls.* or auth.* change carries the lock-out warning, and restart-required keys get the restart note. Owner WI 4.3. Requirements: ASM-3, ASM-4, FR-8, FR-11.
//
//   - Setup: A stack with the real helper and F-RELOAD (the 4.4 wiring helper, or the launch spy with `result.json` written by the real helper).
//   - Steps:
//     1. Set `auth.failureLimitPerMinute=20`.
//     2. Set `providers.host.enabled=true`.
//   - Expected:
//   - Step 1, warnings: the lock-out sentence of WI 4.3 for `auth.failureLimitPerMinute`, plus `run nexus nexusd restart to apply: auth`.
//   - Step 1, result: `restartRequired == ["auth"]`.
//   - Step 1, no restart: F-SYS `starts` is unchanged (ASM-4).
//   - Step 2: no warnings and `restartRequired == []`.
func TestTCRA83_ATlsOrAuthChangeCarriesTheLockOutWarningAnd(t *testing.T) {
	t.Skip("TC-RA-83: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-E22: A file changed between admission and Prepare ends config-conflict with nothing written. Owner WI 4.3. Requirements: FR-7, FR-8.
//
//   - Setup:
//   - FR-7 helper step 1: a change refuses with `config-conflict` if the file "no longer matches what admission validated". Admission's digest is the baseline (D-7, A-15), and Prepare compares it before writing anything.
//   - A temp `ConfigPath` holding the default file with `providers.nexusd.enabled: true`, F-SYS with F-RELOAD, and the real `CheckConfigDataForTest`. `orig := sha(file)`.
//   - The test calls WI 4.3's Admission hook and Prepare directly, in package `nexusd`, so that it can rewrite the file between them deterministically. It does not race the ops manager's goroutine, and it does not queue behind a held `reload`, which the reject-policy lane would refuse with 409 instead.
//   - Steps:
//     1. Run the Admission hook on `["providers.host.enabled=true"]` and keep the returned `Admitted`.
//     2. Rewrite `ConfigPath` with other bytes: the default file with `audit.retentionDays: 45` (run A, valid), or the default file with `audit.retentionDays: 0` (run B, invalid). `other := sha(file)`.
//     3. Run Prepare with `Admitted.In` as the operation's input.
//   - Expected:
//   - Baseline: step 1 returns `Done == false` and `In.BaseSHA256 == orig` (hex SHA-256 of the bytes admission read).
//   - Outcome, runs A and B: Prepare fails the operation with `config-conflict`, nothing changed. Run B is reported as a conflict, not as `config-invalid`: Prepare compares digests and does not re-validate.
//   - Nothing written: no `input.json` exists in the operation directory, Prepare returns no launch spec, the disk sha is still `other`, no `.bak` exists, and F-RELOAD `reloadCalls == 0`.
func TestTCRAE22_AFileChangedBetweenAdmissionAndPrepareEndsConfig(t *testing.T) {
	t.Skip("TC-RA-E22: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-E23: A no-change config set leaves requested and completed audit records and never takes the lane. Owner WI 4.3. Requirements: FR-7, FR-9, FR-13.
//
//   - Setup: A stack with the launch spy, as TC-RA-71.
//   - Steps:
//     1. Set `providers.nexusd.enabled=true` (already true).
//     2. Immediately `POST .../actions/reload`, and keep it running.
//     3. While the reload runs, set `providers.nexusd.enabled=true` again.
//     4. End the reload by writing its `exit.json`.
//     5. Set `["providers.nexusd.enabled=true","providers.host.enabled=true"]`: one assignment equals the file and one does not.
//     6. Read `stack.auditOf(t, "actions.update-config")`.
//   - Expected:
//   - Step 1: `200` with `changed: false`.
//   - Step 2: `202`, not 409, so the no-op set held no `nexusd` lane.
//   - Step 3: 409 `conflict` naming the reload's operation ID (A-14: a no-change set still answers 409 under a busy lane, FR-9).
//   - Step 5: `202` with an operation, `launches` incremented by 1, and `input.json` carrying both assignments as sent. A no-op is decided for the whole edit, not per assignment.
//   - Step 6:
//   - step 1 has exactly `requested` then `completed`, linked, with outcome `completed`, no `queued` or `running` record, facts `changed=false` and `old.providers.nexusd.enabled=true`, and no `backupPath` fact;
//   - step 3 has one `refused` record with slug `conflict`, and no `requested` or `completed` record (the lane refusal comes before the requested record);
//   - step 5's `requested` record is followed by `queued`.
func TestTCRAE23_ANoChangeConfigSetLeavesRequestedAndCompletedAudit(t *testing.T) {
	t.Skip("TC-RA-E23: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-ERR26: Admission refuses an unknown key with its parent and creates no operation. Owner WI 4.3. Requirements: FR-7, FR-6.
//
//   - Setup: A stack with the launch spy. Record the operation count.
//   - Steps:
//     1. `changes: ["providers.host.enabeld=true"]`.
//   - Expected:
//   - 422 `config-invalid` with `configPath`, and `errors[0].path == "providers.host.enabeld"` with a message naming `providers.host`.
//   - The operation count is unchanged and launches are 0.
//   - A `refused` audit record.
func TestTCRAERR26_AdmissionRefusesAnUnknownKeyWithItsParentAnd(t *testing.T) {
	t.Skip("TC-RA-ERR26: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-ERR27: Admission refuses each deny-list entry with its reason. Owner WI 4.3. Requirements: FR-12, ASM-3, NFR-5.
//
//   - Setup: As TC-RA-ERR26. The twelve entries are FR-12 as amended by the user decision of 2026-11-17 (`requirements.md` § User decisions), with `providers.nginx.disabledSuffix` included by analogy (A-35, autonomous default).
//   - Steps:
//     1. `providers.nexusd.update.trustedKeys=x`.
//     2. `providers.nexusd.enabled=false`.
//     3. `providers.scripts.catalog=x`.
//     4. `providers.scripts.catalog.foo=x`, a subtree.
//     5. `providers.nginx.includeDir=/etc/profile.d`, the review's root file-write example.
//     6. `providers.nginx.includeSuffix=.sh`.
//     7. `["providers.nginx.includeDir=/etc/profile.d","providers.nginx.includeSuffix=.sh"]`, the two-key form of that example.
//     8. `providers.nginx.service=ssh`.
//     9. `providers.nginx.disabledSuffix=.sh`.
//     10. One subtest per remaining path-valued entry of `spec.ConfigDenyList` (those with `Value == ""` and not `Subtree`, not already covered), each assigned a value that would otherwise pass every type and validation rule: an absolute root-owned path such as `/etc/nexus/x.conf` for path and directory keys.
//   - Expected:
//   - Each gives 422 `config-invalid` at the key, with the message `"cannot be set remotely: " + spec.ConfigDenyList[i].Reason`, read from the table.
//   - Steps 1 to 10 together cover all twelve entries: the test fails if any entry of `spec.ConfigDenyList` was not refused at least once.
//   - For steps 1 and 3, the deny reason is reported and not the list or mapping reason (D-10 order).
//   - Step 7 reports both keys, each with its own deny reason, so a multi-key bypass is refused whole.
//   - Steps 5 to 10 never reach the validator (`CheckConfigDataForTest` call count unchanged): the refusal is the deny reason even where the value is otherwise valid.
//   - The operation count is unchanged, launches are 0, and the file sha is unchanged.
func TestTCRAERR27_AdmissionRefusesEachDenyListEntryWithItsReason(t *testing.T) {
	t.Skip("TC-RA-ERR27: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-ERR28: Admission refuses lists, mappings, wrong types, duplicates and too many changes. Owner WI 4.3. Requirements: FR-7, NFR-5, OOS-3, ASM-3.
//
//   - Setup: As TC-RA-ERR26. `listen` is not settable remotely, by the user decision of 2026-11-17 (`requirements.md` § User decisions, OOS-3, A-12). It is refused as a list, not through the deny-list.
//   - Steps:
//     1. `listen=127.0.0.1:9`, and in a second call the comma form `listen=127.0.0.1:9,127.0.0.1:10`.
//     2. `providers.host=x`.
//     3. `providers.host.enabled=yes`.
//     4. `["tls.mode=auto","tls.mode=off"]`.
//     5. 33 items.
//     6. `host.name=` followed by 4097 bytes. The item fits the schema's `maxLength` (4096 + 520), so the value bound is admission's.
//     7. One item longer than 4616 bytes.
//   - Expected:
//   - Steps 1 to 4 and 6 give 422 `config-invalid`, located at the key, with the list, mapping, type, duplicate and length messages. Step 6's message never contains the value.
//   - Both step 1 calls are refused at `listen` with the list message, not "cannot be set remotely" (`listen` is not on `spec.ConfigDenyList`). No element is parsed or applied: the file sha is unchanged and no lock-out warning is produced.
//   - Steps 5 and 7 give 422 `validation-failed` from the schema (`maxItems` and `maxLength`), before admission.
//   - Each has an operation count of 0 and launches of 0.
func TestTCRAERR28_AdmissionRefusesListsMappingsWrongTypes(t *testing.T) {
	t.Skip("TC-RA-ERR28: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-ERR29: Admission refuses an edit that makes the file invalid. Owner WI 4.3. Requirements: FR-7.
//
//   - Setup: A stack config with `listen: ["0.0.0.0:8443"]` and `tls.mode: auto`.
//   - Steps:
//     1. `tls.mode=off`.
//     2. `audit.retentionDays=0`.
//   - Expected:
//   - Step 1 gives 422 `config-invalid` with an error at `listen[0]`, whose message is the validator's loopback text.
//   - Step 2 gives 422 at `audit.retentionDays`.
//   - The errors equal `daemon.CheckConfigData(Edit(...))`'s list, and the operation count is 0.
func TestTCRAERR29_AdmissionRefusesAnEditThatMakesTheFileInvalid(t *testing.T) {
	t.Skip("TC-RA-ERR29: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-ERR30: Config set on a host without systemd self-management is refused while tokens and config get still work. Owner WI 4.3. Requirements: FR-9, OOS-2.
//
//   - Setup: A stack with the availability seam `{false, "requires systemd (detected openrc)"}`.
//   - Steps:
//     1. Run config set.
//     2. Run token list, add and remove.
//     3. Run config get.
//   - Expected:
//   - Step 1: 503 `config-set-unavailable`, with `provider: nexusd` and the reason, refused before any operation exists. The operation count is 0, launches are 0, the file sha is unchanged, and there is a `refused` record.
//   - Steps 2 and 3: 200.
func TestTCRAERR30_ConfigSetOnAHostWithoutSystemdSelfManagementIs(t *testing.T) {
	t.Skip("TC-RA-ERR30: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-ERR31: Config set refuses a nexusd:read-only token and an unauthenticated call. Owner WI 4.3. Requirements: FR-7, NFR-7.
//
//   - Setup: A stack.
//   - Steps:
//     1. Post as `reader`.
//     2. Post with no token.
//   - Expected: 403 with `requiredScope nexusd:admin` and a `denied` record, then 401. Launches are 0.
func TestTCRAERR31_ConfigSetRefusesANexusdReadOnlyTokenAndAn(t *testing.T) {
	t.Skip("TC-RA-ERR31: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-E30: The no-change rule compares decoded values, and an absent key is always a change. Owner WI 4.3. Requirements: FR-7.
//
//   - Setup: As TC-RA-71, with a file that has `providers.nginx.testTimeout: 90s`, `providers.host.enabled: True` (capitalized YAML boolean) and no `audit.retentionDays`. The schema default of `audit.retentionDays` is read with `config.LookupKey`.
//   - Steps:
//     1. `providers.nginx.testTimeout=1m30s`.
//     2. `providers.host.enabled=true`.
//     3. `audit.retentionDays=<the default>`.
//     4. `["providers.nginx.testTimeout=90s","providers.nexusd.enabled=true"]`, both equal.
//   - Expected:
//   - Steps 1, 2 and 4: `200 changed:false`, no operation (durations compare as `time.Duration`, booleans as decoded values, not bytes).
//   - Step 3: `202` with an operation: an assignment to an absent key is a change even when it equals the default.
func TestTCRAE30_TheNoChangeRuleComparesDecodedValuesAndAnAbsent(t *testing.T) {
	t.Skip("TC-RA-E30: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-ERR43: Prepare refuses an empty baseline, as after a restart, and the baseline never survives a JSON round trip. Owner WI 4.3. Requirements: FR-7, FR-8.
//
//   - Setup: As TC-RA-E22. Run the Admission hook on `["providers.host.enabled=true"]` and keep `Admitted.In`. `recovered` is `json.Unmarshal(json.Marshal(Admitted.In))`, which is what recovery decodes from a Queued record (`internal/ops/recovery.go`).
//   - Steps:
//     1. Check `recovered.BaseSHA256`.
//     2. Run Prepare with `recovered` as the operation's input.
//   - Expected:
//   - Step 1: `recovered.BaseSHA256 == ""` while `Admitted.In.BaseSHA256` is not empty.
//   - Step 2: the operation fails `config-conflict` with the detail "nexusd restarted before the change was prepared; nothing was changed; run the command again". No `input.json` is written, no launch spec is returned, the file sha is unchanged, no `.bak` exists, and `reloadCalls == 0`.
func TestTCRAERR43_PrepareRefusesAnEmptyBaselineAsAfterARestart(t *testing.T) {
	t.Skip("TC-RA-ERR43: SKIP-only stub; un-skip when WI 4.3 lands")
}
