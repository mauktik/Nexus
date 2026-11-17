// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-37, TC-RA-38.
// Owner WIs: 2.4. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package daemon

import "testing"

// TC-RA-37: REST and CLI token creates leave the secret in no sink of a real daemon. Owner WI 2.4. Requirements: NFR-1, FR-1, FR-13.
//
//   - Setup:
//   - `startInProcess` with temp state, ops, audit and log dirs, `providers.nexusd.enabled: true`, an `slog` capture at Debug, and an admin token minted locally.
//   - The CLI is run in-process (`cli.Run`) against the daemon's URL, with captured stdout and stderr.
//   - Steps:
//     1. REST `POST .../tokens/rest1/add`, keeping the body.
//     2. CLI `nexus nexusdtoken add cli1 --yes`.
//     3. A duplicate REST add of `rest1`, which gives 409.
//     4. A replay of step 1 with an `Idempotency-Key`, which gives 422.
//     5. Collect the sinks: `state.db`, `ops/`, `audit.jsonl*`, the captured log, CLI stderr, the bodies of steps 3 and 4, the full `GET /v1/operations` paging, and `GET /v1/system/audit`.
//   - Expected:
//   - For both secrets, each sink has 0 occurrences of the token string, of its secret part, and of the hash hex taken from `tokens.json`.
//   - Non-vacuity: step 1's body and the CLI stdout each contain their secret, and `tokens.json` contains both hashes.
//   - Both tokens authenticate.
func TestTCRA37_RESTAndCLITokenCreatesLeaveTheSecretInNoSinkOfA(t *testing.T) {
	t.Skip("TC-RA-37: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-RA-38: Token ops leave no operation record and no operation-endpoint trace. Owner WI 2.4. Requirements: FR-1, NFR-1, CON-3.
//
//   - Setup: As TC-RA-37.
//   - Steps:
//     1. Record the operation count.
//     2. Add, list and remove a token.
//     3. `GET /v1/operations?provider=nexusd`.
//     4. `GET /v1/operations/{requested record ID of the add}`.
//   - Expected:
//   - The operation count is unchanged, and no operation has an op ID under `nexusd.tokens.*`.
//   - Step 4 gives 404 `not-found`.
//   - `ops/` has no new directory.
func TestTCRA38_TokenOpsLeaveNoOperationRecordAndNoOperation(t *testing.T) {
	t.Skip("TC-RA-38: SKIP-only stub; un-skip when WI 2.4 lands")
}
