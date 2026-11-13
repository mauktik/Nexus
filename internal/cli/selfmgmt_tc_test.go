// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package cli

import "testing"

// TC-SM-31 (WI 2.4; FR-13, FR-7): The CLI uploads, then posts the op with the returned uploadId
func TestTCSM31_TheCLIUploadsThenPostsThe(t *testing.T) {
	// - Setup:
	//   - `startProviderServer` with a test-local provider declaring an upload op `tp.uploads.add` and
	//     a long-running `tp.actions.update` with `CLI.Upload` (TC-SM-3 shape);
	//   - the update op completes at once with its input echoed;
	//   - a `recProxy` in front recording requests;
	//   - file F of 3 MiB, and `F.sig` holding an 88-character signature plus `\n`.
	// - Steps:
	//   1. `cli.Run(["tp", "update", "--file", F, "--idempotency-key", "k1", "-o", "json"], env)`.
	// - Expected:
	//   - Exit 0.
	//   - Request 1 is `POST /v1/providers/tp/uploads?signature=<trimmed sig>`, with `Content-Type:
	//     application/octet-stream`, `Content-Length: 3145728`, a body equal to F, and no
	//     `Idempotency-Key`.
	//   - Request 2 is `POST /v1/providers/tp/actions/update`, whose JSON body has `uploadId` equal to
	//     request 1's response `uploadId`, and with `Idempotency-Key: k1`.
	//   - stderr contains `uploading F (3 MiB) to host`.
	//   - stdout is the final operation.
	t.Skip("TC-SM-31: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-SM-32 (WI 2.4; FR-13): The signature sidecar defaults to <file>.sig, and --signature overrides it
func TestTCSM32_TheSignatureSidecarDefaultsToFile(t *testing.T) {
	// - Setup: as TC-SM-31; `F.sig` holds S1; `other.sig` holds S2.
	// - Steps:
	//   1. Run `tp update --file F`.
	//   2. Run `tp update --file F --signature other.sig`.
	// - Expected:
	//   - The upload query `signature` is S1 in run 1 and S2 in run 2.
	//   - Whitespace and newlines are trimmed.
	t.Skip("TC-SM-32: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-SM-33 (WI 2.4; FR-13, OOS-1): Multi-host update uploads once per host and posts each host's own uploadId
func TestTCSM33_MultiHostUpdateUploadsOncePer(t *testing.T) {
	// - Setup: two `startProviderServer` hosts `h1` and `h2` with the TC-SM-31 provider; client config
	//   with both, tagged `fleet`.
	// - Steps:
	//   1. Run `tp update --file F --host fleet --yes -o json`.
	// - Expected:
	//   - Each host received exactly one upload and one op POST.
	//   - Each op body carries that host's own upload response `uploadId`.
	//   - The output has a per-host result for both, and the exit code is 0.
	t.Skip("TC-SM-33: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-SM-34 (WI 2.4; FR-13): Generated help shows --file, --signature and --allow-downgrade, and the upload op has no command
func TestTCSM34_GeneratedHelpShowsFileSignatureAnd(t *testing.T) {
	// - Setup: `testApp` with the TC-SM-31 specs.
	// - Steps:
	//   1. Build the command tree.
	//   2. Render `tp update --help`.
	//   3. Run `nexus command get -o json`.
	// - Expected:
	//   - Help contains `--file` (required), `--signature` and `--allow-downgrade`.
	//   - Help contains no `--upload-id` flag, and an example contains `--file`.
	//   - No command is bound to `tp.uploads.add`, and `groupOps` raises no error.
	//   - `command get` lists `tp update` and no upload command.
	t.Skip("TC-SM-34: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-SM-36 (WI 2.5; FR-14): FR-14 acceptance: a 20 s outage mid-operation exits 0 with the final operation
func TestTCSM36_FR14AcceptanceA20S(t *testing.T) {
	// - Setup:
	//   - the real server with a gate-style long-running test op declaring `ReconnectWindow: 60s`;
	//   - F-PROXY in `refuse` mode for virtual `[t+2s, t+22s)`;
	//   - F-CLK injected into the reconnector;
	//   - the op releases at virtual t+25s.
	// - Steps:
	//   1. Run `gate gadget start gated` (the gated op), which waits by default.
	// - Expected:
	//   - Exit 0, and stdout is the Succeeded operation.
	//   - stderr contains `is unreachable, likely restarting; retrying (0s of 60s)` and later progress
	//     lines at intervals of at most 10 s.
	//   - The number of failed polls is about 20, one per virtual second.
	//   - Wall time is under 5 s.
	t.Skip("TC-SM-36: SKIP-only stub; un-skip when WI 2.5 lands")
}

// TC-SM-37 (WI 2.5; FR-14): FR-14 acceptance: a 61 s outage exits 5 naming the operation
func TestTCSM37_FR14AcceptanceA61S(t *testing.T) {
	// - Setup: as TC-SM-36, with the outage `[t+2s, t+63s)`.
	// - Steps:
	//   1. Run the gated op.
	// - Expected:
	//   - Exit 5.
	//   - stderr names the op ID and the exact text `nexus operation get <id> --host <host>`.
	//   - No poll is attempted after virtual t+2s+60s plus one gap.
	t.Skip("TC-SM-37: SKIP-only stub; un-skip when WI 2.5 lands")
}

// TC-SM-38 (WI 2.5; FR-14): --follow tolerates the outage and resumes the log from the last offset
func TestTCSM38_FollowToleratesTheOutageAndResumes(t *testing.T) {
	// - Setup: a counter op printing lines `1`…`40`, one per virtual second, with `ReconnectWindow:
	//   60s`. F-PROXY refuses for 20 s after line 10, then 61 s in a second run.
	// - Steps:
	//   1. Run 1: `gate gadget start counter --follow` with the 20 s outage.
	//   2. Run 2: the same with the 61 s outage.
	// - Expected:
	//   - Run 1: exit 0. The printed lines are exactly `1`…`40`, with no duplicate and no gap.
	//   - Run 1: the number of streams opened after the outage is 1, from the stored offset.
	//   - Run 2: exit 5 with the op ID.
	t.Skip("TC-SM-38: SKIP-only stub; un-skip when WI 2.5 lands")
}

// TC-SM-39 (WI 2.5; FR-14): 503 counts as retryable, and a success resets the clock
func TestTCSM39_503CountsAsRetryableAndA(t *testing.T) {
	// - Setup: F-PROXY in `503` mode for `[t+2, t+42)`, then healthy for `[t+42, t+44)`, then `refuse`
	//   for `[t+44, t+84)`. The op releases at t+90.
	// - Steps:
	//   1. Run the gated op.
	// - Expected:
	//   - Exit 0: no continuous outage reached 60 s.
	//   - A second progress sequence restarts at `0s of 60s` after the healthy gap.
	t.Skip("TC-SM-39: SKIP-only stub; un-skip when WI 2.5 lands")
}

// TC-SM-40 (WI 2.5; FR-14): The real-time short window exercises real sleeps
func TestTCSM40_TheRealTimeShortWindowExercises(t *testing.T) {
	// - Setup: a test op with `ReconnectWindow: 3s` and the real clock; a real proxy outage.
	// - Steps:
	//   1. Run with a 1 s outage, then the op ends.
	//   2. Run with a 4 s outage.
	// - Expected:
	//   - Run 1 → exit 0.
	//   - Run 2 → exit 5 after 3 s to 4.5 s of continuous failure.
	t.Skip("TC-SM-40: SKIP-only stub; un-skip when WI 2.5 lands")
}

// TC-SM-E12 (WI 2.4; FR-13): An explicit --signature path that is missing does not fall back to <file>.sig
func TestTCSME12_AnExplicitSignaturePathThatIs(t *testing.T) {
	// - Setup: F and `F.sig` both exist; `/nonexistent.sig` does not.
	// - Steps:
	//   1. Run `tp update --file F --signature /nonexistent.sig`.
	// - Expected:
	//   - Exit 2, and the message names `/nonexistent.sig`.
	//   - Zero requests reached the server.
	t.Skip("TC-SM-E12: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-SM-E13 (WI 2.5; FR-14): In machine formats, reconnect progress is JSON on stderr
func TestTCSME13_InMachineFormatsReconnectProgressIs(t *testing.T) {
	// - Setup: as TC-SM-36.
	// - Steps:
	//   1. Run with `-o json`.
	// - Expected:
	//   - Every reconnect progress line on stderr is a JSON object with fields for the host, the
	//     elapsed seconds and the 60-second window.
	//   - stdout is exactly one JSON operation.
	t.Skip("TC-SM-E13: SKIP-only stub; un-skip when WI 2.5 lands")
}

// TC-SM-ERR18 (WI 2.4; FR-13, FR-15): CLI pre-check failures exit 2 with no network, and the private key is never read
func TestTCSMERR18_CLIPreCheckFailuresExit2(t *testing.T) {
	// - Setup:
	//   - TC-SM-31 server behind a counting proxy;
	//   - a FIFO created at `$HOME/.config/nexus/release-signing.key`, and `NEXUS_RELEASE_KEY`
	//     pointing to another FIFO, so any open-for-read would block the test;
	//   - files: missing F; a directory; a sparse file of `ceiling+1` bytes; F without `.sig`; F with
	//     a 4097-byte `.sig`.
	// - Steps:
	//   1. Run `tp update --file <each>`.
	// - Expected:
	//   - Each exits 2 with a message naming the file and the reason.
	//   - Zero requests reached the proxy.
	//   - Every run returns within 5 s (no FIFO read).
	t.Skip("TC-SM-ERR18: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-SM-ERR19 (WI 2.4; FR-13, FR-6): A server 413 or 403 on the upload stops the CLI before the op POST
func TestTCSMERR19_AServer413Or403On(t *testing.T) {
	// - Setup: TC-SM-31 server with the provider cap set to 1 KiB; a 2 KiB F, which is under the
	//   declared ceiling, so it passes the local pre-check (A-11). A second run uses a token without
	//   `tp:admin`.
	// - Steps:
	//   1. `tp update --file F`, once per setup.
	// - Expected:
	//   - Run 1: exit 2, `upload-too-large`, stderr naming `maxBytes 1024`.
	//   - Run 2: exit 3, `forbidden`.
	//   - Both: no POST to `/actions/update`.
	t.Skip("TC-SM-ERR19: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-SM-ERR20 (WI 2.5; FR-14): A certificate pin mismatch is never retried, even inside a reconnect window
func TestTCSMERR20_ACertificatePinMismatchIsNever(t *testing.T) {
	// - Setup: an op with a 60 s window over TLS; F-PROXY switches to `pin` mode after the POST.
	// - Steps:
	//   1. Run the gated op (plain wait).
	//   2. Run it with `--follow`.
	// - Expected:
	//   - Both exit 5 at once: virtual time elapsed under 1 s and exactly 1 failed poll.
	//   - stderr names the fingerprint mismatch and has no `likely restarting` line.
	t.Skip("TC-SM-ERR20: SKIP-only stub; un-skip when WI 2.5 lands")
}

// TC-SM-ERR21 (WI 2.5; FR-14, CON-4): A windowless op keeps today's behaviour
func TestTCSMERR21_AWindowlessOpKeepsTodayS(t *testing.T) {
	// - Setup: the existing gate op with no `ReconnectWindow`; F-PROXY refuse mode after the POST.
	// - Steps:
	//   1. Plain wait.
	//   2. `--follow`.
	// - Expected:
	//   - Plain wait fails on the first poll error with exit 5, after exactly 1 poll.
	//   - `--follow` gives up after `followRetries=5` failures 1 s apart, matching base behaviour byte
	//     for byte (output compared with a golden captured from base `d04583a`).
	t.Skip("TC-SM-ERR21: SKIP-only stub; un-skip when WI 2.5 lands")
}

// TC-SM-ERR22 (WI 2.5; FR-14): Non-retryable errors inside a window are not retried
func TestTCSMERR22_NonRetryableErrorsInsideAWindow(t *testing.T) {
	// - Setup: an op with a 60 s window; F-PROXY answers polls with real-problem-shaped 401, 404 and
	//   500 in three runs.
	// - Steps:
	//   1. Run the gated op in each run.
	// - Expected:
	//   - The exit codes follow the existing mapping: 3, 4 and 1.
	//   - Each run made exactly 1 poll and printed no reconnect progress.
	t.Skip("TC-SM-ERR22: SKIP-only stub; un-skip when WI 2.5 lands")
}
