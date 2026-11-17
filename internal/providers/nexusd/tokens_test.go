// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-30, TC-RA-31, TC-RA-32, TC-RA-33, TC-RA-34, TC-RA-35, TC-RA-36, TC-RA-E8, TC-RA-E9, TC-RA-ERR12.
// Owner WIs: 2.3. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package nexusd

import "testing"

// TC-RA-30: Token list matches auth.List, marks the caller's own token and reflects a second state. Owner WI 2.3. Requirements: FR-2, FR-4.
//
//   - Setup: A stack whose state dir holds the stack's own `admin`, `reader` and `nobody` tokens, plus `ci` (local, expires in 30 d) and `bot` (`CreatedBy` admin).
//   - Steps:
//     1. `GET /v1/providers/nexusd/tokens` as `admin`.
//     2. `auth.List(stateDir)`.
//     3. Run local `auth.Revoke(stateDir, "ci")` and `auth.Create(stateDir, {Name:"new"})`.
//     4. GET again.
//   - Expected:
//   - Step 1: the items equal `auth.List` field by field (`name`, `tokenId`, `scopes`, `created`, `expires`; `null` for never).
//   - Step 1, self: `self` is true only for the `admin` token.
//   - Step 1, createdBy: `bot` has `createdBy {tokenId, name}`.
//   - Step 1, no secrets: walked as `map[string]any`, no key is named `sha256`, `hash`, `secret` or `token`, and no value matches `^nxs_`.
//   - Step 4: `ci` is gone and `new` is present, so the list is not cached.
func TestTCRA30_TokenListMatchesAuthListMarksTheCallerSOwnToken(t *testing.T) {
	t.Skip("TC-RA-30: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-RA-31: Token add returns the secret once with metadata, and the new token authenticates on the next request. Owner WI 2.3. Requirements: FR-1, FR-4, ASM-6.
//
//   - Setup: A stack with F-CLK at t0.
//   - Steps:
//     1. `POST /v1/providers/nexusd/tokens/ci/add` with `{"name":"ci","scopes":["nexusd:read"],"expires":"90d"}` as `admin`.
//     2. `GET /v1/providers/nexusd/daemons` with the returned token.
//     3. `auth.List`.
//   - Expected:
//   - Step 1: 200 with `changed: true`. `token` matches `^nxs_<tokenId>_` and `name` is `ci`. `scopes` is `["nexusd:read"]`, `created` is t0, and `expires` is t0+90×24h.
//   - Step 2: 200.
//   - Step 3: the record exists, with `CreatedBy` equal to the admin token's ID and name.
//   - Expected values: computed from F-CLK and `auth.ParseLifetime("90d")`.
func TestTCRA31_TokenAddReturnsTheSecretOnceWithMetadataAndThe(t *testing.T) {
	t.Skip("TC-RA-31: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-RA-32: Token remove by name or ID deletes the record, and the token is refused from the next request on. Owner WI 2.3. Requirements: FR-3.
//
//   - Setup: Tokens `a` and `b` created through the API, holding `nexusd:read`.
//   - Steps:
//     1. Remove `a` by name.
//     2. Remove `b` by its 12-hex ID.
//     3. Use `a`'s and `b`'s token strings on `GET .../daemons`.
//   - Expected:
//   - Steps 1 and 2: 200 with `changed: true`, the right `tokenId` and `name`, and `revokedSelf: false`.
//   - Step 3: both get 401.
//   - `auth.List` has neither token.
func TestTCRA32_TokenRemoveByNameOrIDDeletesTheRecordAndTheToken(t *testing.T) {
	t.Skip("TC-RA-32: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-RA-33: Revoking the caller's own token answers 200 with revokedSelf and a warning. Owner WI 2.3. Requirements: FR-3, FR-13.
//
//   - Setup: A token `me` with `nexusd:admin`, minted through `stack.mint`.
//   - Steps:
//     1. `POST .../tokens/me/remove` using `me`'s own token.
//     2. Repeat any GET with it.
//   - Expected:
//   - Step 1: 200 with `revokedSelf: true`. `warnings` is exactly `["the token this request used (me, id <ID>) is revoked: this client can no longer authenticate to this host with it"]`.
//   - Step 1, audit: the `completed` record has the facts `tokenId=<ID>`, `tokenName=me` and `revokedSelf=true`.
//   - Step 2: 401.
func TestTCRA33_RevokingTheCallerSOwnTokenAnswers200With(t *testing.T) {
	t.Skip("TC-RA-33: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-RA-34: Ten successful mutations per rolling minute, then 429 with a computed Retry-After; only successes count. Owner WI 2.3. Requirements: FR-5, ASM-6.
//
//   - Setup: A stack with F-CLK at t0. A list of 10 successful operations: 6 adds `r0..r5` and 4 removes of `r0..r3`, each followed by `Advance(2s)`. Between them, the uncounted calls: a list, a duplicate add (409), an add with `expires:"0s"` (validation refusal), and a remove of `nope` (404).
//   - Steps:
//     1. Run the sequence.
//     2. Record the store's sha and the audit count.
//     3. Add `r6`.
//     4. `Advance` to t0+60s, where the oldest success is exactly 60 s old.
//     5. Add `r6` again.
//     6. `Advance(1ns)` and add `r6` again.
//   - Expected:
//   - Step 3: 429 `rate-limited` with `Retry-After` = ceil(t0 + 60s − now), computed from F-CLK (≥ 1), and a problem body. The store sha is unchanged. A `refused` record with slug `rate-limited`.
//   - Step 5: still 429, with `Retry-After: 1` (the window is inclusive).
//   - Step 6: 200.
//   - Accounting: the 4 uncounted calls changed nothing in the window. A variant with them removed gives the same 429 position.
func TestTCRA34_TenSuccessfulMutationsPerRollingMinuteThen429(t *testing.T) {
	t.Skip("TC-RA-34: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-RA-35: The 256-token cap counts local tokens, refuses remote adds, and leaves local creates working. Owner WI 2.3. Requirements: FR-5, ASM-6, CON-4.
//
//   - Setup: The stack's tokens plus local `auth.Create` calls until `len(auth.List) == spec.MaxRemoteTokens`.
//   - Steps:
//     1. Remote add `over`.
//     2. Local `auth.Create(stateDir, {Name:"local"})`.
//     3. Remote add `over2`.
//   - Expected:
//   - Step 1: 409 `token-limit-reached` with `maxTokens: 256`. The store sha is unchanged and no secret is in the body.
//   - Step 2: succeeds, giving 257 tokens.
//   - Step 3: still 409.
//   - Rate limit: the refusals are not counted, so 10 successful removes still go through.
func TestTCRA35_The256TokenCapCountsLocalTokensRefusesRemoteAdds(t *testing.T) {
	t.Skip("TC-RA-35: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-RA-36: Audit facts for token add and remove carry metadata and never the secret or hash. Owner WI 2.3. Requirements: FR-13, NFR-1.
//
//   - Setup: A stack.
//   - Steps:
//     1. Add `ci` with scopes `a:read` and `b:write` and expiry 1h.
//     2. Add `ne` without expiry.
//     3. Remove `ci`.
//     4. Read `stack.auditOf(t, "tokens.add")` and `"tokens.remove"`.
//   - Expected:
//   - add `ci`: `completed` has the facts `tokenId=<id>`, `tokenName=ci`, `scopes=a:read,b:write` and `expires=<RFC 3339 of t0+1h>`.
//   - add `ne`: `expires=never`.
//   - remove `ci`: `tokenId`, `tokenName=ci` and `revokedSelf=false`.
//   - Requested params: `name`, `scopes` and `expires` only.
//   - No secrets: the raw audit file has 0 occurrences of either secret or of either `sha256` hex. The hex is computed from `tokens.json` before the remove.
func TestTCRA36_AuditFactsForTokenAddAndRemoveCarryMetadataAnd(t *testing.T) {
	t.Skip("TC-RA-36: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-RA-E8: Concurrent remote adds serialize through tokenGuard, and exactly ten succeed. Owner WI 2.3. Requirements: FR-5, FR-4.
//
//   - Setup: A stack with F-CLK frozen.
//   - Steps:
//     1. Run 20 goroutines posting adds of distinct names, under `-race`.
//   - Expected: Exactly 10 answer 200 and 10 answer 429. `auth.List` gains exactly 10 records, and `tokens.json` is valid.
func TestTCRAE8_ConcurrentRemoteAddsSerializeThroughTokenGuard(t *testing.T) {
	t.Skip("TC-RA-E8: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-RA-E9: An admin may mint nexusd:admin, and an add with no expiry never expires. Owner WI 2.3. Requirements: ASM-2, ASM-6, FR-1.
//
//   - Setup: A stack.
//   - Steps:
//     1. Add `rot` with scopes `nexusd:admin` and `nexusd:read`, without `expires`.
//     2. Use `rot` to add `rot2`.
//   - Expected:
//   - Step 1: 200 with `expires: null`. The record has no `expires`.
//   - Step 2: 200, so the minted admin token holds `nexusd:admin`.
func TestTCRAE9_AnAdminMayMintNexusdAdminAndAnAddWithNoExpiry(t *testing.T) {
	t.Skip("TC-RA-E9: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-RA-ERR12: Token add and remove map store errors to problems without leaking a secret. Owner WI 2.3. Requirements: FR-1, FR-3, NFR-1.
//
//   - Setup: A stack with `a` present.
//   - Steps:
//     1. Add `a` again.
//     2. Add `b` with `expires:"0s"`.
//     3. Remove `nope`.
//   - Expected:
//   - Step 1 gives 409 `conflict` naming `a`.
//   - Step 2 gives 422 `validation-failed` at `/expires` with ParseLifetime's text.
//   - Step 3 gives 404 `not-found`.
//   - The store is unchanged after each, and each body has no `nxs_`, `sha256` or hash hex.
func TestTCRAERR12_TokenAddAndRemoveMapStoreErrorsToProblemsWithout(t *testing.T) {
	t.Skip("TC-RA-ERR12: SKIP-only stub; un-skip when WI 2.3 lands")
}
