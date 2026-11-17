// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-43, TC-RA-44, TC-RA-45, TC-RA-46, TC-RA-E10.
// Owner WIs: 2.4. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package cli

import "testing"

// TC-RA-43: nexusdtoken add prints one distinct secret per host on stdout, labelled, after confirmation. Owner WI 2.4. Requirements: FR-11, NFR-1, FR-1.
//
//   - Setup: Two `startProviderServer` hosts `a` and `b`, each with the real nexusd provider over its own state dir. `writeClientConfig` sets admin tokens for both.
//   - Steps:
//     1. `nexus nexusdtoken add ci --host a,b` with no TTY and no `--yes`.
//     2. The same with `--yes`.
//   - Expected:
//   - Step 1: refused by the multi-host confirmation rule, with exit as the existing scheme gives. Both servers count 0 POSTs.
//   - Step 2: exit 0. stdout has one result per host, labelled `a` and `b`, each with a `token`. The two tokens differ.
//   - Each token works: it authenticates only on its own host.
//   - No leak: stderr has 0 occurrences of either secret.
func TestTCRA43_NexusdtokenAddPrintsOneDistinctSecretPerHostOn(t *testing.T) {
	t.Skip("TC-RA-43: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-RA-44: nexusdtoken remove of the caller's own token warns on stderr and exits 0. Owner WI 2.4. Requirements: FR-3, FR-11.
//
//   - Setup: Host `a`, whose client token is named `me` in the store.
//   - Steps:
//     1. `nexus nexusdtoken remove me` with no TTY and no `--yes`.
//     2. `nexus nexusdtoken remove me --yes`.
//     3. `nexus nexusdtoken get`.
//   - Expected:
//   - Step 1: refused, because the op is destructive, and 0 POSTs.
//   - Step 2: exit 0. stdout has `revokedSelf: true`, and stderr has the warning line from TC-RA-33, through `a.warn`.
//   - Step 3: an authentication failure exit.
func TestTCRA44_NexusdtokenRemoveOfTheCallerSOwnTokenWarnsOn(t *testing.T) {
	t.Skip("TC-RA-44: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-RA-45: nexusdtoken get prints the token table with the self column. Owner WI 2.4. Requirements: FR-2, FR-11.
//
//   - Setup: Host `a` with 3 tokens.
//   - Steps:
//     1. `nexus nexusdtoken get`.
//     2. `nexus nexusdtoken get -o json`.
//   - Expected:
//   - The table header matches the WI 2.2 columns.
//   - There is one row per `auth.List` entry, and `self` is true only for the client's token.
//   - The JSON items match `auth.List`.
//   - Neither output contains `sha256` or `nxs_`.
func TestTCRA45_NexusdtokenGetPrintsTheTokenTableWithTheSelf(t *testing.T) {
	t.Skip("TC-RA-45: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-RA-46: --save-as-host stores the new token and the next command uses it. Owner WI 2.4. Requirements: FR-11, ASM-8.
//
//   - Setup: Host `a` with the real provider.
//   - Steps:
//     1. `nexus nexusdtoken add ci2 --scopes nexusd:read --host a --save-as-host a-ci2`.
//     2. `nexus nexusd get --host a-ci2`.
//   - Expected:
//   - Step 1: exit 0. stdout has no `token`. The secret occurs 0 times in stdout and in stderr.
//   - Step 2: 200, and the server's audit shows the actor `ci2`.
func TestTCRA46_SaveAsHostStoresTheNewTokenAndTheNextCommandUses(t *testing.T) {
	t.Skip("TC-RA-46: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-RA-E10: CLI token commands accept no token value as an argument. Owner WI 2.4. Requirements: FR-11, NFR-1.
//
//   - Setup: The generated `nexusdtoken` command tree.
//   - Steps:
//     1. List every flag and positional of `add`, `get` and `remove`.
//   - Expected:
//   - No flag is named `token`, `secret` or `value`.
//   - `remove`'s positional is `NAME` (an id or name), and passing a full `nxs_…` string is refused by the name pattern with 422, with the string never echoed in the problem detail or on stderr.
func TestTCRAE10_CLITokenCommandsAcceptNoTokenValueAsAnArgument(t *testing.T) {
	t.Skip("TC-RA-E10: SKIP-only stub; un-skip when WI 2.4 lands")
}
