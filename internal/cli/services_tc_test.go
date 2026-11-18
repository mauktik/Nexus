// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-26, TC-SV-27, TC-SV-28, TC-SV-29, TC-SV-125, TC-SV-126, TC-SV-E6, TC-SV-ERR8, TC-SV-ERR9, TC-SV-ERR10, TC-SV-ERR28.
// Owner WIs: 1.5, 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package cli

import "testing"

// TC-SV-26: Without the flag, the CLI prompts on the terminal for the host name and sends the trimmed answer. Owner WI 1.5. Requirements: FR-10, FR-13, FR-15.
//
// - Setup:
//   - `startAPIFake` serving a planted destructive long-running op `pl.actions.reboot` with CLI noun `machine`, verb `restart` and `TypedConfirmation: "confirmHost"`.
//   - The client config has one host `h1`.
//   - F-TTY answers `h1\n`.
//
// - Steps:
//  1. `nexus machine restart --host h1 --no-wait`.
//
// - Expected:
//   - F-TTY received exactly one prompt, containing `Type the host name` and `h1`;
//   - no y/N question was written;
//   - exactly one POST, whose body has `"confirmHost":"h1"`;
//   - exit 0.
func TestTCSV26_WithoutTheFlagTheCLIPromptsOn(t *testing.T) {
	t.Skip("TC-SV-26: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-SV-27: An explicit --confirm-host is the confirmation: no prompt and no y/N question. Owner WI 1.5. Requirements: FR-10, FR-13.
//
// - Setup: As TC-SV-26. F-TTY is present but would fail the test if opened. A second subtest runs with no terminal at all (`noTTY`).
// - Steps:
//  1. `nexus machine restart --host h1 --confirm-host h1 --no-wait`.
//
// - Expected: In both subtests the terminal is never opened, one POST is sent with `confirmHost: h1`, and the exit is 0.
func TestTCSV27_AnExplicitConfirmHostIsTheConfirmation(t *testing.T) {
	t.Skip("TC-SV-27: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-SV-28: --yes neither fills nor skips the typed confirmation. Owner WI 1.5. Requirements: FR-10, FR-13.
//
// - Setup: As TC-SV-26.
// - Steps:
//  1. `nexus machine restart --host h1 --yes --no-wait` with F-TTY answering `h1`.
//  2. The same with no terminal.
//
// - Expected:
//   - step 1 prompts for the typed host name exactly as TC-SV-26 does, and sends `confirmHost: h1`;
//   - step 2 is a usage error, exit 2, naming `--confirm-host`, with zero POSTs.
func TestTCSV28_YesNeitherFillsNorSkipsTheTyped(t *testing.T) {
	t.Skip("TC-SV-28: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-SV-29: The typed field is never copied from --host. Owner WI 1.5. Requirements: FR-10.
//
// - Setup: As TC-SV-26. F-TTY answers `other`.
// - Steps:
//  1. `nexus machine restart --host h1 --no-wait`.
//
// - Expected: The POST body has `"confirmHost":"other"`, not `h1`. The CLI does not pre-validate the value against `--host`; the server compares it (FR-10).
func TestTCSV29_TheTypedFieldIsNeverCopiedFrom(t *testing.T) {
	t.Skip("TC-SV-29: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-SV-125: CLI commands, columns and the --host all rules. Owner WI 5.1. Requirements: FR-15, FR-10.
//
// - Setup: `startProviderServer` with the services provider over F-HOSTS. The client config has hosts `h1` and `h2` (two servers).
// - Steps:
//  1. `nexus service get --host h1`.
//  2. `nexus service get nexusd --host h1`.
//  3. `nexus servicelog get nexusd --tail 5 --host h1` (it names the host for the same reason as step 2).
//  4. `nexus service get --host all`.
//  5. `nexus service invoke nx-svc-test --action restart --host h1 --no-wait`.
//  6. `nexus service stop nx-svc-test --host h1` with F-TTY answering `n`.
//  7. `nexus machine restart --host all --confirm-host h1`.
//
// - Expected:
//   - (1) the columns `NAME STATE ENABLED CONTROLLABLE PROTECTED` (the header from `name, properties.state, …`);
//   - (2) the same header and exactly one row, `nexusd`, whose `STATE` and `ENABLED` equal the provider's `get` response for `nexusd` over F-HOSTS (decoded into `spec.ServiceProperties`, not hard-coded), with `CONTROLLABLE` false and `PROTECTED` true; one GET to `h1` only, none to `h2`;
//   - (3) 5 lines;
//   - (4) rows from both hosts;
//   - (5) one POST to `/v1/providers/services/services/nx-svc-test/control` with `{action: restart}`;
//   - (6) a y/N question, no POST, and the existing decline exit code;
//   - (7) a usage error, exit 2, with zero POSTs to either host.
func TestTCSV125_CLICommandsColumnsAndTheHostAll(t *testing.T) {
	t.Skip("TC-SV-125: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-126: machine restart waits across the outage and prints the final result. Owner WI 5.1. Requirements: FR-13, NFR-7, A-16, A-35.
//
// - Setup: F-RECONNECT. The server returns the op Running, closes for 3 min of fake time (connection refused, then 503s), and reopens with the same certificate and the op Succeeded with a `RebootResult` holding `previousBootId: PREV` and `bootId: CUR`.
// - Steps:
//  1. `nexus machine restart --host h1 --confirm-host h1` (waits by default).
//
// - Expected:
//   - exit 0;
//   - stdout shows the columns `BOOTID PREVIOUSBOOTID UPAT REBOOTREQUIRED.STATE` with `CUR` and `PREV`;
//   - the fake clock advanced about 3 min in 1 s sleeps, with no real waiting;
//   - `--no-wait` instead prints the operation and returns after one POST.
func TestTCSV126_MachineRestartWaitsAcrossTheOutageAnd(t *testing.T) {
	t.Skip("TC-SV-126: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-E6: The typed answer is trimmed and sent as typed, case included. Owner WI 1.5. Requirements: FR-10.
//
// - Setup: As TC-SV-26, with F-TTY answering `"  H1 \t\n"`.
// - Steps:
//  1. `nexus machine restart --host h1 --no-wait`.
//
// - Expected: The body has `"confirmHost":"H1"`, with no case folding.
func TestTCSVE6_TheTypedAnswerIsTrimmedAndSent(t *testing.T) {
	t.Skip("TC-SV-E6: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-SV-ERR8: No terminal and no flag is a usage error naming the flag, with nothing sent. Owner WI 1.5. Requirements: FR-10, FR-13.
//
// - Setup: As TC-SV-26, with `noTTY`.
// - Steps:
//  1. `nexus machine restart --host h1`.
//
// - Expected: Exit 2, stderr names `--confirm-host`, and the fake recorded zero POSTs.
func TestTCSVERR8_NoTerminalAndNoFlagIsA(t *testing.T) {
	t.Skip("TC-SV-ERR8: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-SV-ERR9: --host all and host lists are refused before anything is sent. Owner WI 1.5. Requirements: FR-10, FR-15.
//
// - Setup: Two hosts `h1` and `h2`. F-TTY would fail the test if opened.
// - Steps:
//  1. `--host all --confirm-host h1`.
//  2. `--host h1,h2 --confirm-host h1`.
//  3. `--host all` with no flag.
//
// - Expected: Each is exit 2, with a message saying the command targets exactly one host. Zero POSTs, and the terminal is never opened.
func TestTCSVERR9_HostAllAndHostListsAreRefused(t *testing.T) {
	t.Skip("TC-SV-ERR9: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-SV-ERR10: An empty typed answer is a usage error. Owner WI 1.5. Requirements: FR-10.
//
// - Setup: F-TTY answering `"\n"`, and then `"   \n"`.
// - Steps:
//  1. `nexus machine restart --host h1`.
//
// - Expected: Exit 2 and zero POSTs in both cases.
func TestTCSVERR10_AnEmptyTypedAnswerIsAUsage(t *testing.T) {
	t.Skip("TC-SV-ERR10: SKIP-only stub; un-skip when WI 1.5 lands")
}

// TC-SV-ERR28: The CLI exits 5 with the follow-up command when the window is spent, and never retries a pin mismatch. Owner WI 5.1. Requirements: FR-13, ASM-9.
//
// - Setup: F-RECONNECT. (a) The server never returns, for 10 min + 1 s of fake time. (b) It returns with a different certificate.
// - Steps:
//  1. `nexus machine restart --host h1 --confirm-host h1`.
//
// - Expected:
//   - (a) exit 5, with stderr containing the operation ID and exactly `nexus operation get <id> --host h1`, and the fake clock advanced ≥ 10 min;
//   - (b) the pin-mismatch error at once, with no further dial (dial count stops at the mismatch), and a non-zero exit that is not 5.
func TestTCSVERR28_TheCLIExits5WithTheFollow(t *testing.T) {
	t.Skip("TC-SV-ERR28: SKIP-only stub; un-skip when WI 5.1 lands")
}
