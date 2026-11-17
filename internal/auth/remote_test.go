// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-20, TC-RA-21, TC-RA-22, TC-RA-23, TC-RA-24, TC-RA-25, TC-RA-E6, TC-RA-ERR11.
// Owner WIs: 2.1. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package auth

import "testing"

// TC-RA-20: The local nexusd token command keeps its flags, output and exit codes. Owner WI 2.1. Requirements: CON-4, FR-14.
//
//   - Setup: Golden strings captured from `TokenCommand` at `521261c`, recorded in the test with that citation: the usage text of `create`, `list` and `revoke`, the `list` table header, the duplicate-name and unknown-ref stderr lines, and the exit codes 0, 1 and 2.
//   - Steps:
//     1. Run `TokenCommand` with `create --name a --scope x:read --expires 30d`.
//     2. `list`; `revoke a`; `revoke nope`; `create --name a` twice; `create --bogus`.
//   - Expected: The flags, stdout shape (the token on stdout for `create`), stderr text and exit codes are byte-identical to the goldens, except the generated ID and secret, which are matched by the `nxs_[0-9a-f]{12}_` pattern.
func TestTCRA20_TheLocalNexusdTokenCommandKeepsItsFlagsOutputAnd(t *testing.T) {
	t.Skip("TC-RA-20: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-RA-21: A tokens.json with createdBy decodes under the rc3 structs, and local creates write no createdBy. Owner WI 2.1. Requirements: CON-5, FR-4.
//
//   - Setup: A copy of the rc3 `record` and `tokenFile` structs from `internal/auth/tokens.go@521261c`.
//   - Steps:
//     1. `Create(dir, {Name:"r", CreatedBy:&TokenRef{ID:"abcdefabcdef", Name:"admin"}})`.
//     2. `json.Unmarshal` the file into the rc3 structs.
//     3. Run the new `readRecords`.
//     4. `Create(dir, {Name:"l"})` without `CreatedBy`.
//   - Expected:
//   - Step 2 succeeds, with both records and their IDs, names and hashes intact.
//   - Step 3 returns `CreatedBy` for `r` only.
//   - The raw JSON of `l` has no `createdBy` key (omitempty).
func TestTCRA21_ATokensJsonWithCreatedByDecodesUnderTheRc3(t *testing.T) {
	t.Skip("TC-RA-21: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-RA-22: CreatedBy is stored and returned by List. Owner WI 2.1. Requirements: FR-4.
//
//   - Setup: A temp dir.
//   - Steps:
//     1. Create `r` with `CreatedBy{ID:"abcdefabcdef", Name:"admin"}`.
//     2. `List(dir)`.
//   - Expected: `List` returns `r` with `CreatedBy == &TokenRef{"abcdefabcdef","admin"}`. A local token has `CreatedBy == nil`.
func TestTCRA22_CreatedByIsStoredAndReturnedByList(t *testing.T) {
	t.Skip("TC-RA-22: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-RA-23: MaxTokens refuses the create past the cap under the lock and leaves the file unchanged. Owner WI 2.1. Requirements: FR-5, ASM-6.
//
//   - Setup: 3 tokens, 2 local and 1 with `CreatedBy`. Record `sha(tokens.json)`.
//   - Steps:
//     1. `Create(dir, {Name:"x", MaxTokens:3})`.
//     2. `Create(dir, {Name:"y", MaxTokens:0})`.
//   - Expected:
//   - Step 1 returns `*LimitError{Max:3}` (`errors.As`), and the file's sha and mtime are unchanged.
//   - Step 2 succeeds, because 0 means no cap, and the store holds 4 tokens.
func TestTCRA23_MaxTokensRefusesTheCreatePastTheCapUnderTheLock(t *testing.T) {
	t.Skip("TC-RA-23: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-RA-24: Remote and local creates of the same name race safely across processes. Owner WI 2.1. Requirements: FR-4, CON-4.
//
//   - Setup: A temp dir. The test binary re-executes itself with `NEXUS_TCRA_HELPER=create` to run `TokenCommand create --name same --dir <dir>` as a second process.
//   - Steps:
//     1. Start 2 goroutines calling `Create(dir, {Name:"same", CreatedBy:...})` and 1 subprocess, released together on a barrier.
//     2. Repeat 20 times with fresh dirs under `-race`.
//   - Expected: In every round:
//   - exactly one succeeds;
//   - the others get `*NameExistsError`, or the subprocess exits 1 with the unchanged duplicate-name text;
//   - `tokens.json` decodes, has exactly 1 record named `same`, and has mode 0600;
//   - no `tokens.json.tmp*` remains.
func TestTCRA24_RemoteAndLocalCreatesOfTheSameNameRaceSafely(t *testing.T) {
	t.Skip("TC-RA-24: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-RA-25: ParseLifetime is exported with the local command's behavior. Owner WI 2.1. Requirements: FR-1, ASM-6, CON-4.
//
//   - Setup: A table taken from `parseLifetime` at `521261c`.
//   - Steps:
//     1. Call `ParseLifetime` on `""`, `"90d"`, `"36500d"`, `"36501d"`, `"0d"`, `"1h30m"`, `"0s"`, `"-1h"`, `"x"`.
//   - Expected:
//   - `""` gives `0` and nil. `"90d"` gives `90*24h`. `"36500d"` gives `36500*24h`. `"1h30m"` gives `90m`.
//   - `"36501d"` and `"0d"` give "is not a positive number of days".
//   - `"0s"` and `"-1h"` give "must be positive".
//   - `"x"` gives "is not a duration such as 24h or 30d".
//   - `tokencmd` calls `ParseLifetime` (source grep: no `parseLifetime(` remains).
func TestTCRA25_ParseLifetimeIsExportedWithTheLocalCommandS(t *testing.T) {
	t.Skip("TC-RA-25: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-RA-E6: The cap is checked before the duplicate-name check. Owner WI 2.1. Requirements: FR-5, ASM-6.
//
//   - Setup: The store is at `MaxTokens = 2` with names `a` and `b`.
//   - Steps:
//     1. `Create(dir, {Name:"a", MaxTokens:2})`.
//   - Expected: `*LimitError`, not `*NameExistsError`, and the file is unchanged.
func TestTCRAE6_TheCapIsCheckedBeforeTheDuplicateNameCheck(t *testing.T) {
	t.Skip("TC-RA-E6: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-RA-ERR11: Duplicate-name and unknown-reference errors are typed with unchanged text. Owner WI 2.1. Requirements: FR-3, FR-4, CON-4.
//
//   - Setup: The store has `a`.
//   - Steps:
//     1. `Create(dir, {Name:"a"})`.
//     2. `Revoke(dir, "nope")`.
//   - Expected:
//   - Step 1 gives `errors.As(*NameExistsError{Name:"a", ID:<a's id>})`. Step 2 gives `*NotFoundError{Ref:"nope"}`.
//   - The `Error()` texts are byte-identical to the strings at `521261c`, copied into the test with the citation, including `no token with id or name "nope"`.
//   - The file is unchanged.
func TestTCRAERR11_DuplicateNameAndUnknownReferenceErrorsAreTyped(t *testing.T) {
	t.Skip("TC-RA-ERR11: SKIP-only stub; un-skip when WI 2.1 lands")
}
