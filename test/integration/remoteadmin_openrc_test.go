//go:build integration

// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-104, TC-RA-105, TC-RA-106, TC-RA-107, TC-RA-ERR37, TC-RA-ERR38.
// Owner WIs: 5.3. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package integration

import "testing"

// TC-RA-104: On OpenRC, token add, get and remove round-trip and apply on the next request. Owner WI 5.3. Requirements: FR-9, FR-1, FR-2, FR-3, NFR-6.
//
//   - Setup:
//   - `requireInit(t, platform.OpenRC)`, `freshHost`, `runInstall`.
//   - Enable `providers.nexusd` in place with `sed`.
//   - `lib(t, "start-nexusd")`. `createToken(t, "ra", "nexusd:read", "nexusd:admin")`.
//   - Steps:
//     1. `nexus nexusdtoken add oc --scopes nexusd:read --yes`.
//     2. Use the new token.
//     3. `nexus nexusdtoken get`.
//     4. `nexus nexusdtoken remove oc --yes`.
//     5. Use the token again.
//   - Expected:
//   - Steps 1 and 2: 0 and 200.
//   - Step 3: lists `oc`, with `self` on `ra`.
//   - Steps 4 and 5: 0 then 401.
//   - Store: `/var/lib/nexus/tokens.json` is `root:root` 0600 and valid.
func TestTCRA104_OnOpenRCTokenAddGetAndRemoveRoundTripAndApplyOn(t *testing.T) {
	t.Skip("TC-RA-104: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-RA-105: On OpenRC, config get works. Owner WI 5.3. Requirements: FR-9, FR-6.
//
//   - Setup: As TC-RA-104.
//   - Steps:
//     1. `nexus nexusdconfig get`.
//     2. `nexus nexusdconfig get providers.nexusd.enabled`.
//   - Expected: Both exit 0. Step 2 shows `true file`. The row count equals the leaf-key count of the installed file.
func TestTCRA105_OnOpenRCConfigGetWorks(t *testing.T) {
	t.Skip("TC-RA-105: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-RA-106: On OpenRC, nexus nexusd get reports configSet unavailable with a reason. Owner WI 5.3. Requirements: FR-9.
//
//   - Setup: As TC-RA-104.
//   - Steps:
//     1. `nexus nexusd get -o json`.
//   - Expected: `configSet.available == false`, with a reason naming systemd and equal to `selfManagement.reason`.
func TestTCRA106_OnOpenRCNexusNexusdGetReportsConfigSet(t *testing.T) {
	t.Skip("TC-RA-106: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-RA-107: On OpenRC, token secrets reach no sink. Owner WI 5.3. Requirements: NFR-1, NFR-6.
//
//   - Setup: As TC-RA-104.
//   - Steps:
//     1. REST add, and keep the secret.
//     2. Grep `state.db`, `ops/`, `audit.jsonl*` and the OpenRC daemon log (`/var/log/nexus/nexusd.log`, as the init script writes it), and the bodies of `GET /v1/operations` and `GET /v1/system/audit`.
//   - Expected: 0 occurrences of the token, its secret part and its hash hex. Non-vacuity as in TC-RA-100.
func TestTCRA107_OnOpenRCTokenSecretsReachNoSink(t *testing.T) {
	t.Skip("TC-RA-107: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-RA-ERR37: On OpenRC, config set is refused with config-set-unavailable and the file is untouched. Owner WI 5.3. Requirements: FR-9, OOS-2, NFR-6.
//
//   - Setup: As TC-RA-104. Record the file sha and the operation count.
//   - Steps:
//     1. `nexus nexusdconfig update providers.host.enabled=true`.
//     2. REST POST with the same body.
//   - Expected:
//   - Status: 503 `config-set-unavailable`. The reason names systemd and equals `nexus nexusd get`'s `configSet.reason`.
//   - CLI: exits with code 4 (the type's exit code).
//   - Nothing changed: the operation count is unchanged, `/etc/nexus/nexusd.yaml` has the same sha, and no `.bak` exists.
//   - Audit: a `refused` audit record.
func TestTCRAERR37_OnOpenRCConfigSetIsRefusedWithConfigSet(t *testing.T) {
	t.Skip("TC-RA-ERR37: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-RA-ERR38: On OpenRC, the 429 path and the token cap behave as on systemd. Owner WI 5.3. Requirements: FR-5, NFR-6.
//
//   - Setup: As TC-RA-104.
//   - Steps:
//     1. 10 remote adds within a minute.
//     2. The 11th add.
//   - Expected: 429 with `Retry-After` between 1 and 60, a problem body, and an unchanged `tokens.json` sha.
func TestTCRAERR38_OnOpenRCThe429PathAndTheTokenCapBehaveAsOn(t *testing.T) {
	t.Skip("TC-RA-ERR38: SKIP-only stub; un-skip when WI 5.3 lands")
}
