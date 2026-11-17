// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-71, TC-RA-78, TC-RA-79, TC-RA-80, TC-RA-81, TC-RA-82, TC-RA-83, TC-RA-E22, TC-RA-E23, TC-RA-ERR26, TC-RA-ERR27, TC-RA-ERR28, TC-RA-ERR29, TC-RA-ERR30, TC-RA-ERR31.
// Owner WIs: 4.3. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package nexusd

import "testing"

// TC-RA-71: A config set that changes nothing is answered at admission with changed:false, with no operation and no helper unit. Owner WI 4.3. Requirements: FR-7.
//
//   - Setup: A stack with the launch spy, F-RELOAD and a temp `ConfigPath` holding the default file with `providers.nexusd.enabled: true`. Record the file's inode, mtime and sha, the operation count, the `ops/` directory listing and `launches`. The user decision of lead-review iteration 1 (`requirements.md` § User decisions): a no-op set is answered at admission with `changed:false` and no helper unit, keeping FR-7 as written.
//   - Steps:
//     1. `POST .../actions/update-config` with `{"changes":["providers.nexusd.enabled=true"]}` as `admin` (the value already in the file).
//     2. The same with two assignments that both equal the file: `["providers.nexusd.enabled=true","audit.retentionDays=<the file's value>"]`, the value read from the fixture with `yaml.v3`.
//   - Expected:
//   - Response: each answers `200`, never `202`, with a body that decodes into `spec.ConfigUpdateResult` with `changed: false`, an empty `configChanges`, `restartRequired []`, and no `operation` member and no `Location` header.
//   - No operation: the operation count and the `ops/` listing are unchanged, so no `input.json` was written.
//   - No unit: `launches == 0`.
//   - Nothing written or reloaded: the inode, mtime and sha are unchanged, no `.bak` exists, and F-RELOAD `reloadCalls == 0`.
func TestTCRA71_AConfigSetThatChangesNothingIsAnsweredAtAdmission(t *testing.T) {
	t.Skip("TC-RA-71: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-78: Admission accepts a valid change, and Prepare writes the config-set input. Owner WI 4.3. Requirements: FR-7, ASM-7.
//
//   - Setup: A stack with the launch spy and a temp `ConfigPath` (the default file with nexusd enabled).
//   - Steps:
//     1. `POST .../actions/update-config` with `{"changes":["providers.host.enabled=true"]}` as `admin`.
//     2. Read `ops/<id>/input.json`.
//   - Expected:
//   - Step 1: 202, and one launch with argv `[exe service-helper --dir D]`, `KillProcess`, and `MaxRuntime` 130 s.
//   - `input.json`: `mode config-set`, `configPath`, `backupPath = configPath+".bak"`, `changes` as sent, `baseSha256` equal to the sha of the bytes admission read and validated, `stateDir`, and `reloadTimeout` equal to the package constant (A-3). A no-change set never reaches Prepare (TC-RA-71), so the input needs no no-change flag.
//   - Audit: the `requested` record's params carry the `changes`.
func TestTCRA78_AdmissionAcceptsAValidChangeAndPrepareWritesThe(t *testing.T) {
	t.Skip("TC-RA-78: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-79: providers.nexusd.enabled=true is accepted although false is denied. Owner WI 4.3. Requirements: FR-12.
//
//   - Setup: As TC-RA-78.
//   - Steps:
//     1. `changes: ["providers.nexusd.enabled=true"]`.
//   - Expected: Not refused by the deny-list (the `Value: "false"` entry does not match). Because the value is already true, the call is a no-op answered at admission: `200` with `changed: false`, no operation and `launches == 0` (TC-RA-71).
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
//   - Expected:
//   - Facts:
//   - `old.audit.retentionDays=90`;
//   - `old.providers.nginx.testTimeout=<absent>`;
//   - `old.host.name` holds the first 256 bytes of the old value, cut by the audit bound;
//   - `backupPath=<configPath>.bak`.
//   - Params: they carry the three new values.
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
//   - FR-7 helper step 1: the helper refuses with `config-conflict` if the file "no longer matches what admission validated". The baseline is therefore admission's read, not Prepare's (user decision of lead-review iteration 1: FR-7 is kept as written).
//   - A temp `ConfigPath` holding the default file with `providers.nexusd.enabled: true`, F-SYS with F-RELOAD, and the real `CheckConfigDataForTest`. `orig := sha(file)`.
//   - The test calls WI 4.3's admission and Prepare functions directly, in package `nexusd`, so that it can rewrite the file between them deterministically. It does not race the ops manager's goroutine, and it does not queue behind a held `reload`, which the reject-policy lane would refuse with 409 instead.
//   - Steps:
//     1. Admit `["providers.host.enabled=true"]`.
//     2. Rewrite `ConfigPath` with other bytes: the default file with `audit.retentionDays: 45` (run A, valid), or the default file with `audit.retentionDays: 0` (run B, invalid). `other := sha(file)`.
//     3. Run Prepare on the admitted value, then the real `Helper` on the `input.json` Prepare wrote.
//   - Expected:
//   - Baseline: `input.json`'s `baseSha256 == orig`, not `other`.
//   - Outcome, runs A and B: `config-conflict`. Run B is reported as a conflict, not as `config-invalid`, because the base check comes first.
//   - Nothing written: the disk sha is still `other`, no `.bak` exists, and F-RELOAD `reloadCalls == 0`.
func TestTCRAE22_AFileChangedBetweenAdmissionAndPrepareEndsConfig(t *testing.T) {
	t.Skip("TC-RA-E22: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-E23: A no-change config set leaves requested and completed audit records and never takes the lane. Owner WI 4.3. Requirements: FR-7, FR-9, FR-13.
//
//   - Setup: A stack with the launch spy, as TC-RA-71.
//   - Steps:
//     1. Set `providers.nexusd.enabled=true` (already true).
//     2. Immediately `POST .../actions/reload`.
//     3. End the reload by writing its `exit.json`.
//     4. Set `["providers.nexusd.enabled=true","providers.host.enabled=true"]`: one assignment equals the file and one does not.
//     5. Read `stack.auditOf(t, "actions.update-config")`.
//   - Expected:
//   - Step 1: `200` with `changed: false`.
//   - Step 2: `202`, not 409, so the no-op set held no `nexusd` lane.
//   - Step 4: `202` with an operation, `launches` incremented by 1, and `input.json` carrying both assignments as sent. A no-op is decided for the whole edit, not per assignment.
//   - Step 5: step 1 has exactly `requested` then `completed`, linked, with outcome `completed` and no `queued` or `running` record, and no `backupPath` fact, because nothing is backed up. Step 4's `requested` record is followed by `queued`.
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
//   - Setup: As TC-RA-ERR26. The path-valued entries come from the user decision of lead-review iteration 1 (`requirements.md` § User decisions).
//   - Steps:
//     1. `providers.nexusd.update.trustedKeys=x`.
//     2. `providers.nexusd.enabled=false`.
//     3. `providers.scripts.catalog=x`.
//     4. `providers.scripts.catalog.foo=x`, a subtree.
//     5. `providers.nginx.includeDir=/etc/profile.d`, the review's root file-write example.
//     6. `["providers.nginx.includeDir=/etc/profile.d","providers.nginx.includeSuffix=.sh"]`, the two-key form of that example.
//     7. `providers.nginx.service=ssh`.
//     8. One subtest per remaining path-valued entry of `spec.ConfigDenyList` (those with `Value == ""` and not `Subtree`), each assigned a value that would otherwise pass every type and validation rule: an absolute root-owned path such as `/etc/nexus/x.conf` for path and directory keys, `.conf2` for a suffix key.
//   - Expected:
//   - Each gives 422 `config-invalid` at the key, with the message `"cannot be set remotely: " + spec.ConfigDenyList[i].Reason`, read from the table.
//   - For steps 1 and 3, the deny reason is reported and not the list or mapping reason (D-10 order).
//   - Step 6 reports both keys, each with its own deny reason, so a multi-key bypass is refused whole.
//   - Steps 5 to 8 never reach the validator: the refusal is the deny reason even where the value is otherwise valid.
//   - The operation count is unchanged, launches are 0, and the file sha is unchanged.
func TestTCRAERR27_AdmissionRefusesEachDenyListEntryWithItsReason(t *testing.T) {
	t.Skip("TC-RA-ERR27: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-ERR28: Admission refuses lists, mappings, wrong types, duplicates and too many changes. Owner WI 4.3. Requirements: FR-7, NFR-5, OOS-3, ASM-3.
//
//   - Setup: As TC-RA-ERR26. `listen` is not settable remotely, by the user decision of lead-review iteration 1 (`requirements.md` § User decisions, OOS-3).
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
//   - Both step 1 calls are refused at `listen` with the list message, and no element is parsed or applied: the file sha is unchanged and no lock-out warning is produced.
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
