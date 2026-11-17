// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-62, TC-RA-63, TC-RA-64, TC-RA-65, TC-RA-66, TC-RA-E16, TC-RA-ERR17, TC-RA-ERR18.
// Owner WIs: 3.4. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package nexusd

import "testing"

// TC-RA-62: config list returns every leaf key once, in order, with value, effective and source. Owner WI 3.4. Requirements: FR-6, FR-13.
//
//   - Setup: A stack whose `ConfigPath` is a copy of `deploy/nexusd.default.yaml` with `providers.nexusd.enabled: true`.
//   - Steps:
//     1. `GET /v1/providers/nexusd/config` as `admin`.
//   - Expected:
//   - Items: the item names equal `config.LeafKeys()` paths in order, with no duplicate.
//   - Values: each `properties` equals `config.InspectAll(file bytes)`, compared as JSON.
//   - Audit: `requested` then `completed` records.
//   - Scope: `reader` gets 403 (ASM-5).
func TestTCRA62_ConfigListReturnsEveryLeafKeyOnceInOrderWith(t *testing.T) {
	t.Skip("TC-RA-62: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-RA-63: Catalog env secrets are redacted, and the raw value appears in no response, log or audit record. Owner WI 3.4. Requirements: FR-6, NFR-1, FR-13.
//
//   - Setup: A stack over `testdata/config/catalog-env.yaml`, whose sentinel is `RA_ENV_SENTINEL_9f3c`.
//   - Steps:
//     1. Run `config list`.
//     2. Run `config get providers.scripts.catalog`.
//     3. Read the stack's logs and the audit file.
//   - Expected:
//   - The env values show `[REDACTED]` in `value` and in `effective`.
//   - The sentinel occurs 0 times in either response body, in `stack.logs` and in the audit file.
//   - Non-vacuity: the fixture file contains the sentinel.
func TestTCRA63_CatalogEnvSecretsAreRedactedAndTheRawValue(t *testing.T) {
	t.Skip("TC-RA-63: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-RA-64: An absent key reports source default and the default effective value; an empty host.name reports the OS hostname. Owner WI 3.4. Requirements: FR-6.
//
//   - Setup: A stack config with `providers.host` removed and `host.name: ""`.
//   - Steps:
//     1. `GET .../config/providers.host.sampleInterval`.
//     2. `GET .../config/host.name`.
//   - Expected:
//   - Step 1: `value: null`, `source: default`, and `effective` equal to the parsed default's short form (computed with `config.Parse` of the file).
//   - Step 2: `value: ""`, `source: file`, and `effective` equal to `os.Hostname()`.
func TestTCRA64_AnAbsentKeyReportsSourceDefaultAndTheDefault(t *testing.T) {
	t.Skip("TC-RA-64: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-RA-65: Config get works where systemd self-management is unavailable. Owner WI 3.4. Requirements: FR-9, FR-6.
//
//   - Setup: A stack with the availability seam set unavailable (`detach.Availability{Available:false, Reason:"requires systemd"}`), as on OpenRC.
//   - Steps:
//     1. `config list`.
//     2. `config get tls.mode`.
//   - Expected: Both answer 200 with values from the file.
func TestTCRA65_ConfigGetWorksWhereSystemdSelfManagementIs(t *testing.T) {
	t.Skip("TC-RA-65: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-RA-66: Config get re-reads the file on every call. Owner WI 3.4. Requirements: FR-6.
//
//   - Setup: A stack with `providers.host.enabled: false`.
//   - Steps:
//     1. `GET .../config/providers.host.enabled`.
//     2. Rewrite the file with the value `true`, without reloading the daemon.
//     3. GET again.
//   - Expected: Step 1 gives `value false, source file`. Step 3 gives `value true`, read from disk (A-25), not a cached first answer and not the configuration in force.
func TestTCRA66_ConfigGetReReadsTheFileOnEveryCall(t *testing.T) {
	t.Skip("TC-RA-66: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-RA-E16: A config get for a mapping key returns its children. Owner WI 3.4. Requirements: FR-6.
//
//   - Setup: A stack.
//   - Steps:
//     1. `GET .../config/providers.host`.
//   - Expected: 200. The value is a mapping whose keys are `config.LookupKey("providers.host").Children`, each with the value, effective and source of the leaf.
func TestTCRAE16_AConfigGetForAMappingKeyReturnsItsChildren(t *testing.T) {
	t.Skip("TC-RA-E16: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-RA-ERR17: Config get answers not-found for an unknown key and config-invalid for a broken file. Owner WI 3.4. Requirements: FR-6.
//
//   - Setup: A stack.
//   - Steps:
//     1. `GET .../config/providers.host.enabeld`.
//     2. Rewrite the file with a strict-decode error (an unknown key `providers.host.bogus: 1`).
//     3. `GET .../config`.
//   - Expected:
//   - Step 1: 404 `not-found`. The detail is exactly `no key providers.host.enabeld; the nearest valid parent is providers.host, whose keys are: <Children joined>`, computed from `LookupKey`.
//   - Step 3: 422 `config-invalid`, with `configPath` and an error located at `providers.host.bogus` (line and column).
func TestTCRAERR17_ConfigGetAnswersNotFoundForAnUnknownKeyAndConfig(t *testing.T) {
	t.Skip("TC-RA-ERR17: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-RA-ERR18: Config get refuses a nexusd:read-only token. Owner WI 3.4. Requirements: ASM-5, FR-6.
//
//   - Setup: A stack.
//   - Steps:
//     1. `GET .../config` and `.../config/tls.mode` as `reader`, then as no token.
//   - Expected: 403 with `requiredScope nexusd:admin` and a `denied` record, then 401.
func TestTCRAERR18_ConfigGetRefusesANexusdReadOnlyToken(t *testing.T) {
	t.Skip("TC-RA-ERR18: SKIP-only stub; un-skip when WI 3.4 lands")
}
