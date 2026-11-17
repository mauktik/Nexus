// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-19, TC-RA-E5, TC-RA-ERR9, TC-RA-ERR10.
// Owner WIs: 1.5. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package cli

import "testing"

// TC-RA-19: --save-as-host with one target stores the token with mode 0600 and keeps the secret off stdout and stderr. Owner WI 1.5. Requirements: FR-11, NFR-1, ASM-8.
//
//   - Setup:
//   - A planted `SecretOutput` op whose server returns `{"changed":true,"token":"nxs_aaaaaaaaaaaa_RASAVE","tokenId":"aaaaaaaaaaaa"}`.
//   - The client config (`writeClientConfig`) has host `h1` (URL U, fingerprint F) and no `defaultHost`.
//   - Steps:
//     1. `nexus tcra add ci --host h1 --save-as-host newh`.
//   - Expected:
//   - Exit 0. The client config file has mode 0600 and gains host `newh` with URL U, fingerprint F and token `nxs_aaaaaaaaaaaa_RASAVE`. `defaultHost` becomes `newh`, because it was unset.
//   - stdout is the result object without `token`.
//   - stderr is exactly `token saved as host newh in <path>`.
//   - The secret occurs 0 times in stdout and in stderr.
func TestTCRA19_SaveAsHostWithOneTargetStoresTheTokenWithMode(t *testing.T) {
	t.Skip("TC-RA-19: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-RA-E5: Two-host --save-as-host is refused before anything is sent, and an existing name with the same URL rotates in place. Owner WI 1.5. Requirements: FR-11, ASM-8.
//
//   - Setup: As TC-RA-19. The client config has `h1` (URL U, tags `[lab]`) and `defaultHost: other`.
//   - Steps:
//     1. `--save-as-host h1 --host h1`, which saves under the same name and URL.
//     2. `--save-as-host x --host h1,h2`.
//   - Expected:
//   - Step 1: `h1`'s token is replaced, its tags `[lab]` are kept, and `defaultHost` stays `other`.
//   - Step 2: a usage error, exit 2, with 0 requests counted.
func TestTCRAE5_TwoHostSaveAsHostIsRefusedBeforeAnythingIsSent(t *testing.T) {
	t.Skip("TC-RA-E5: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-RA-ERR9: --save-as-host refusals send nothing. Owner WI 1.5. Requirements: FR-11, ASM-8.
//
//   - Setup: As TC-RA-19, with an existing host `h2` at URL V ≠ U.
//   - Steps:
//     1. `--save-as-host h2 --host h1`, a URL mismatch.
//     2. `--save-as-host 'bad name'`.
//     3. `--save-as-host x --stdin`.
//   - Expected: Each is a usage error (exit 2) with a message naming the reason. Each counts 0 requests, and the client config is unchanged.
func TestTCRAERR9_SaveAsHostRefusalsSendNothing(t *testing.T) {
	t.Skip("TC-RA-ERR9: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-RA-ERR10: A failed save prints the secret on stdout only and exits 1. Owner WI 1.5. Requirements: FR-11, NFR-1, ASM-8.
//
//   - Setup: As TC-RA-19, with the client config directory made read-only (0500) after the pre-check (a hook between pre-check and save, or a config path whose parent becomes read-only).
//   - Steps:
//     1. `--save-as-host newh --host h1`.
//   - Expected:
//   - Exit 1.
//   - stdout has the full object, including `token`.
//   - stderr has the save error and 0 occurrences of the secret.
//   - The client config is unchanged.
func TestTCRAERR10_AFailedSavePrintsTheSecretOnStdoutOnlyAndExits1(t *testing.T) {
	t.Skip("TC-RA-ERR10: SKIP-only stub; un-skip when WI 1.5 lands")
}
