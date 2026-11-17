// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-16, TC-RA-17, TC-RA-18.
// Owner WIs: 1.5. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package cli

import "testing"

// TC-RA-16: CLI trailing arguments bind to the Variadic field. Owner WI 1.5. Requirements: FR-11, FR-7.
//
//   - Setup: A planted spec (TC-RA-4's op) through `runSpecsCLI`, and a counting `httptest` server that records request bodies.
//   - Steps:
//     1. `nexus tcra update a.b=1 c.d=x=y --yes`.
//     2. `nexus tcra update --help`.
//   - Expected:
//   - The recorded body is `{"changes":["a.b=1","c.d=x=y"]}`.
//   - The usage line shows `CHANGES...`, and the examples print the `CHANGES` placeholder.
//   - Existing commands whose trailing field is `names` keep `NAMES...`, checked against an existing spec.
func TestTCRA16_CLITrailingArgumentsBindToTheVariadicField(t *testing.T) {
	t.Skip("TC-RA-16: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-RA-17: The idempotency flag is absent on SyncMutation-only commands and present elsewhere. Owner WI 1.5. Requirements: FR-11, CON-3.
//
//   - Setup: A planted command whose only mutating op is a `SyncMutation`, plus the real `operation cancel` command and a planted long-running command.
//   - Steps:
//     1. Collect the flag sets.
//     2. Render the examples.
//   - Expected:
//   - The SyncMutation command has no `--idempotency-key`, has `--yes`, and its examples lack the "safe to retry" line.
//   - `operation cancel` and the long-running command still have `--idempotency-key`.
//   - `docs/cli/nexus_operation_cancel.md` is unchanged (`TestGeneratedDocsAreCurrent`).
func TestTCRA17_TheIdempotencyFlagIsAbsentOnSyncMutationOnly(t *testing.T) {
	t.Skip("TC-RA-17: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-RA-18: EchoWarnings prints result warnings on stderr after the result, in text and json formats, and never with --no-wait. Owner WI 1.5. Requirements: FR-3, FR-11, ASM-3.
//
//   - Setup:
//   - A planted sync op with `EchoWarnings`, whose server reply is `{"changed":true,"warnings":["w-one","w-two"]}` (the `json.Marshal` of the planted output).
//   - A planted long-running op whose operation `result.warnings` is `["w-three"]`.
//   - Steps:
//     1. Run the sync command with `-o text` and with `-o json`.
//     2. Run the long-running command waited.
//     3. Run it with `--no-wait`.
//   - Expected:
//   - Steps 1 and 2: stderr has one `a.warn` line per warning, prefixed with the host as `a.warn` formats it. stdout holds the unmodified result, and the stdout JSON still includes `warnings`.
//   - Step 3: stderr has no warning line.
func TestTCRA18_EchoWarningsPrintsResultWarningsOnStderrAfterThe(t *testing.T) {
	t.Skip("TC-RA-18: SKIP-only stub; un-skip when WI 1.5 lands")
}
