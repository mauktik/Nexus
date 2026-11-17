// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-78, TC-RA-79, TC-RA-80, TC-RA-81, TC-RA-82, TC-RA-83, TC-RA-E22, TC-RA-E23, TC-RA-ERR26, TC-RA-ERR27, TC-RA-ERR28, TC-RA-ERR29, TC-RA-ERR30, TC-RA-ERR31.
// Owner WIs: 4.3. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package nexusd

import "testing"

// TC-RA-78: Admission accepts a valid change, and Prepare writes the config-set input. Owner WI 4.3. Requirements: FR-7, ASM-7.
//
//   - Setup: A stack with the launch spy and a temp `ConfigPath` (the default file with nexusd enabled).
//   - Steps:
//     1. `POST .../actions/update-config` with `{"changes":["providers.host.enabled=true"]}` as `admin`.
//     2. Read `ops/<id>/input.json`.
//   - Expected:
//   - Step 1: 202, and one launch with argv `[exe service-helper --dir D]`, `KillProcess`, and `MaxRuntime` 130 s.
//   - `input.json`: `mode config-set`, `configPath`, `backupPath = configPath+".bak"`, `changes` as sent, `baseSha256 = sha(file)`, `noChange false`, `stateDir`, and `reloadTimeout` 20 s.
//   - Audit: the `requested` record's params carry the `changes`.
func TestTCRA78_AdmissionAcceptsAValidChangeAndPrepareWritesThe(t *testing.T) {
	t.Skip("TC-RA-78: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-79: providers.nexusd.enabled=true is accepted although false is denied. Owner WI 4.3. Requirements: FR-12.
//
//   - Setup: As TC-RA-78.
//   - Steps:
//     1. `changes: ["providers.nexusd.enabled=true"]`.
//   - Expected: 202. `NoChange` is true in `input.json`, because the value is already true.
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
//   - Setup: A file with `audit.retentionDays: 90`, without `providers.pihole.dnsFile`, and with `host.name` set to a 300-byte string.
//   - Steps:
//     1. Set `audit.retentionDays=30`, `providers.pihole.dnsFile=/etc/dnsmasq.d/99-nexus.conf` and `host.name=short`.
//   - Expected:
//   - Facts:
//   - `old.audit.retentionDays=90`;
//   - `old.providers.pihole.dnsFile=<absent>`;
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

// TC-RA-E22: A file changed between admission and Prepare is re-validated, not reported as a conflict. Owner WI 4.3. Requirements: FR-7.
//
//   - Setup: A stack whose ops manager queues the op behind a held `reload`, so the test can rewrite the file between Admit and Prepare.
//   - Steps:
//     1. Accept a config set.
//     2. Rewrite the file with another valid content (run A) or with an invalid content (run B).
//     3. Release the queue.
//   - Expected:
//   - Run A: Prepare's `baseSha256 == sha(rewritten)`, and the op proceeds.
//   - Run B: the op ends `failed` with `config-invalid` and launches 0 (A-15).
func TestTCRAE22_AFileChangedBetweenAdmissionAndPrepareIsRe(t *testing.T) {
	t.Skip("TC-RA-E22: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-E23: A no-change config set still runs a short-lived no-op unit. Owner WI 4.3. Requirements: FR-7.
//
//   - Setup: A stack with the launch spy.
//   - Steps:
//     1. Set the current value.
//   - Expected: 202. `input.json` has `noChange: true`, launches 1, and the operation succeeds with `changed:false` once the spy's `exit.json` and the helper's `result.json` are written.
func TestTCRAE23_ANoChangeConfigSetStillRunsAShortLivedNoOpUnit(t *testing.T) {
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

// TC-RA-ERR27: Admission refuses each deny-list entry with its reason. Owner WI 4.3. Requirements: FR-12, ASM-3.
//
//   - Setup: As TC-RA-ERR26.
//   - Steps:
//     1. `providers.nexusd.update.trustedKeys=x`.
//     2. `providers.nexusd.enabled=false`.
//     3. `providers.scripts.catalog=x`.
//     4. `providers.scripts.catalog.foo=x`, a subtree.
//   - Expected:
//   - Each gives 422 `config-invalid` at the key, with the message `"cannot be set remotely: " + spec.ConfigDenyList[i].Reason`, read from the table.
//   - For steps 1 and 3, the deny reason is reported and not the list or mapping reason (D-10 order).
//   - The operation count is unchanged, launches are 0, and the file sha is unchanged.
func TestTCRAERR27_AdmissionRefusesEachDenyListEntryWithItsReason(t *testing.T) {
	t.Skip("TC-RA-ERR27: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-RA-ERR28: Admission refuses lists, mappings, wrong types, duplicates and too many changes. Owner WI 4.3. Requirements: FR-7, NFR-5, OOS-3.
//
//   - Setup: As TC-RA-ERR26.
//   - Steps:
//     1. `listen=127.0.0.1:9`.
//     2. `providers.host=x`.
//     3. `providers.host.enabled=yes`.
//     4. `["tls.mode=auto","tls.mode=off"]`.
//     5. 33 items.
//     6. `host.name=` followed by 4097 bytes. The item fits the schema's `maxLength` (4096 + 520), so the value bound is admission's.
//     7. One item longer than 4616 bytes.
//   - Expected:
//   - Steps 1 to 4 and 6 give 422 `config-invalid`, located at the key, with the list, mapping, type, duplicate and length messages. Step 6's message never contains the value.
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
