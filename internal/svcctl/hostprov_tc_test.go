// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-16, TC-HP-17, TC-HP-18, TC-HP-19, TC-HP-20, TC-HP-ERR6, TC-HP-ERR7, TC-HP-ERR8, TC-HP-ERR9.
// Owner WIs: 1.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package svcctl

import "testing"

// TC-HP-16: Failed lists systemd failed units from the captured output. Owner WI 1.4. Requirements: FR-10, CON-3.
//
// - Setup: F-RUN answering `testdata/systemd/list-units-failed.txt` with its README `Exit`.
// - Steps: `Failed(ctx, platform.Systemd, Options{Run: frun, Timeout: time.Second})`.
// - Expected: one call with argv exactly `{"/usr/bin/systemctl","list-units","--state=failed","--plain","--no-legend","--no-pager","--full"}`; `Names` equals the first field of each capture line matching `^[A-Za-z0-9@._:\-]+\.[a-z]+$`, deduped and sorted (computed in-test from the file); `Count == len(Names)`; the planted unit named in the README is among them.
func TestTCHP16_FailedListsSystemdFailedUnitsFromThe(t *testing.T) {
	t.Skip("TC-HP-16: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-HP-17: Failed reports none from the captured empty systemd output. Owner WI 1.4. Requirements: FR-10.
//
// - Setup: F-RUN answering `list-units-failed-none.txt` with its README exit.
// - Steps: `Failed(ctx, platform.Systemd, …)`.
// - Expected: no error; `Count == 0`; `Names` is non-nil and empty.
func TestTCHP17_FailedReportsNoneFromTheCapturedEmpty(t *testing.T) {
	t.Skip("TC-HP-17: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-HP-18: Failed lists OpenRC crashed services from the captured output. Owner WI 1.4. Requirements: FR-10, CON-5.
//
// - Setup: F-RUN answering `testdata/openrc/rc-status-crashed.txt` with its README exit.
// - Steps: `Failed(ctx, platform.OpenRC, …)`.
// - Expected: argv exactly `{"/bin/rc-status","--crashed"}`; `Names` equals the capture's lines matching `ServiceNamePattern`, sorted; includes the README's planted service.
func TestTCHP18_FailedListsOpenRCCrashedServicesFromThe(t *testing.T) {
	t.Skip("TC-HP-18: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-HP-19: Failed treats the captured OpenRC "none crashed" result as none. Owner WI 1.4. Requirements: FR-10.
//
// - Setup: F-RUN answering `rc-status-crashed-none.txt` with its README exit (A-16 expects 1 with empty output; the README value is authoritative).
// - Steps: `Failed(ctx, platform.OpenRC, …)`; also with a synthetic `(exit 0, empty)`.
// - Expected: both give no error, `Count == 0`, empty `Names`.
func TestTCHP19_FailedTreatsTheCapturedOpenRCNoneCrashed(t *testing.T) {
	t.Skip("TC-HP-19: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-HP-20: Run is the exported real runner and remains the default. Owner WI 1.4. Requirements: CON-3.
//
// - Setup: none.
// - Steps: run `TestRealRunnerEnvCapAndTimeout`'s assertions against `svcctl.Run`; build a `base` with zero `Options.Run`.
// - Expected: env is exactly `PATH=/usr/sbin:/usr/bin:/sbin:/bin` and `LC_ALL=C`, output capped at `OutputCap`, the timeout kills the process group; `reflect.ValueOf(b.run).Pointer() == reflect.ValueOf(Run).Pointer()`; no symbol `realRun` remains (`go doc` / grep).
func TestTCHP20_RunIsTheExportedRealRunnerAnd(t *testing.T) {
	t.Skip("TC-HP-20: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-HP-ERR6: Failed rejects non-zero exits with output. Owner WI 1.4. Requirements: FR-10, FR-12.
//
// - Setup: F-RUN: OpenRC `(exit 1, "rc-status: some error\n")`; systemd `(exit 1, "Failed to connect to bus: No such file or directory\n")`; OpenRC exit 2 empty.
// - Steps: `Failed` for each.
// - Expected: each returns an error naming the exit status (`exit 1`, `exit 2`); no names.
func TestTCHPERR6_FailedRejectsNonZeroExitsWithOutput(t *testing.T) {
	t.Skip("TC-HP-ERR6: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-HP-ERR7: Failed drops lines that are not unit or service names. Owner WI 1.4. Requirements: FR-10, NFR-6.
//
// - Setup: the captured systemd output with lines added: `● x.service loaded failed failed X`, `Warning: something`, `a b c`, `../../etc.service`, a 300-character name; the OpenRC capture with ` * x` and `$(rm -rf /)` lines.
// - Steps: `Failed`.
// - Expected: only pattern-matching names are returned; the added junk lines produce no names; no error.
func TestTCHPERR7_FailedDropsLinesThatAreNotUnit(t *testing.T) {
	t.Skip("TC-HP-ERR7: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-HP-ERR8: Failed honours its timeout. Owner WI 1.4. Requirements: FR-12, NFR-1.
//
// - Setup: F-RUN `hang`; `Timeout: 200ms`.
// - Steps: `Failed(ctx, platform.Systemd, …)`; measure.
// - Expected: returns within 300 ms; `errors.Is(err, context.DeadlineExceeded)`.
func TestTCHPERR8_FailedHonoursItsTimeout(t *testing.T) {
	t.Skip("TC-HP-ERR8: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-HP-ERR9: Failed on an unknown init system does not run anything. Owner WI 1.4. Requirements: CON-5, FR-10.
//
// - Setup: F-RUN recording.
// - Steps: `Failed(ctx, platform.InitUnknown (the existing zero/unknown value), …)`.
// - Expected: error wrapping `ErrNotStarted`; F-RUN recorded no call.
func TestTCHPERR9_FailedOnAnUnknownInitSystemDoes(t *testing.T) {
	t.Skip("TC-HP-ERR9: SKIP-only stub; un-skip when WI 1.4 lands")
}
