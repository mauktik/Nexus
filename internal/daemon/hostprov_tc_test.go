// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-1, TC-HP-2, TC-HP-3, TC-HP-4, TC-HP-5, TC-HP-6, TC-HP-7, TC-HP-E1, TC-HP-E2, TC-HP-E3, TC-HP-ERR1, TC-HP-ERR2.
// Owner WIs: 1.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package daemon

import "testing"

// TC-HP-1: Startup starts an enabled background provider exactly once and never a disabled one. Owner WI 1.1. Requirements: FR-1, FR-11, NFR-2, CON-4.
//
// - Setup: F-BG in `normal` mode named `apt`; config via `writeAptConfig` with `providers.apt.enabled: true` (run A) and `false` (run B).
// - Steps:
//  1. Run A: `startInProcess(t, cfg, fbg)`; wait for READY (the `inProcess` helper's ready signal).
//  2. Run B: same with the provider disabled; let it serve one `GET /healthz`.
//
// - Expected:
//   - A: `fbg.starts == 1` within 1 s of READY; the recorded `ctx.Err()` is nil while serving.
//   - B: `fbg.starts == 0` after `/healthz` answered and after shutdown.
func TestTCHP1_StartupStartsAnEnabledBackgroundProviderExactly(t *testing.T) {
	t.Skip("TC-HP-1: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-2: A reload that disables the provider cancels its run before reload returns. Owner WI 1.1. Requirements: FR-1, FR-11.
//
// - Setup: as TC-HP-1 A.
// - Steps:
//  1. Rewrite the config with `providers.apt.enabled: false`.
//  2. Call `d.Reload(ctx)` (API path) and record when it returns; repeat the setup and use SIGHUP (`r.signal(t, syscall.SIGHUP)`) instead.
//
// - Expected: the first run's `ctx` is cancelled and its `RunBackground` has returned (F-BG `returned` channel closed) before `Reload` returns; for SIGHUP, within 1 s of the signal. `fbg.starts` stays 1.
func TestTCHP2_AReloadThatDisablesTheProviderCancels(t *testing.T) {
	t.Skip("TC-HP-2: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-3: A reload that re-enables the provider starts a fresh run with a new context. Owner WI 1.1. Requirements: FR-1, FR-11, FR-13.
//
// - Setup: TC-HP-2 end state (disabled after a run).
// - Steps: rewrite with `enabled: true`; `Reload`.
// - Expected: `fbg.starts == 2`; the second `ctx` is a different value from the first and is not done; the first stays done.
func TestTCHP3_AReloadThatReEnablesTheProvider(t *testing.T) {
	t.Skip("TC-HP-3: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-4: A reload rejected at apply leaves the running background untouched. Owner WI 1.1. Requirements: FR-1, NFR-4.
//
// - Setup: TC-HP-1 A; a second provider (the `sectionProvider` pattern) scripted to reject its next `Configure`, so `Apply` fails and restores.
// - Steps: rewrite the config with `apt.enabled: false` and the rejected section; `Reload`.
// - Expected: `Reload` returns the apply error; F-BG's first `ctx` is not done; `fbg.starts == 1`; no stop was attempted (manager log has no "stopping" line for `apt`).
func TestTCHP4_AReloadRejectedAtApplyLeavesThe(t *testing.T) {
	t.Skip("TC-HP-4: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-5: Shutdown stops the background run after the server and before the operations manager. Owner WI 1.1. Requirements: FR-11, NFR-2.
//
// - Setup: TC-HP-1 A, with a spy on `srv.Shutdown` completion and `ops.Shutdown` start (log lines the daemon already writes, read from the `syncBuffer` logger).
// - Steps: `r.signal(t, syscall.SIGTERM)`; `r.wait(t)`.
// - Expected: F-BG's run returned; the order in the log is server shutdown, background stop, operations shutdown; `Run` returns nil.
func TestTCHP5_ShutdownStopsTheBackgroundRunAfterThe(t *testing.T) {
	t.Skip("TC-HP-5: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-6: validate-config and CheckConfig never start a background run. Owner WI 1.1. Requirements: FR-1, NFR-2.
//
// - Setup: F-BG with `apt.enabled: true` in a fixture config.
// - Steps:
//  1. `core.ValidateProviders` on a provider set containing F-BG (the call `daemon.CheckConfig` makes).
//  2. `daemon.CheckConfig(path)` on the fixture file.
//
// - Expected: both succeed; `fbg.starts == 0`; `runtime.NumGoroutine()` is back to its pre-call value (`eventually`, 1 s).
func TestTCHP6_ValidateConfigAndCheckConfigNeverStartA(t *testing.T) {
	t.Skip("TC-HP-6: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-7: Goroutines return to the baseline after stop. Owner WI 1.1. Requirements: FR-11, NFR-2.
//
// - Setup: record `runtime.NumGoroutine()` before starting.
// - Steps: start enabled; reload disable; reload enable; reload disable; SIGTERM; wait.
// - Expected: `eventually(t, 5*time.Second, …)` `runtime.NumGoroutine() <= baseline` (the `reload_test.go:655` pattern).
func TestTCHP7_GoroutinesReturnToTheBaselineAfterStop(t *testing.T) {
	t.Skip("TC-HP-7: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-E1: A reload that keeps the provider enabled does not restart its run. Owner WI 1.1. Requirements: FR-1, FR-13.
//
// - Setup: TC-HP-1 A.
// - Steps: change an unrelated setting of the same section (or another section); `Reload` twice.
// - Expected: `fbg.starts == 1`; the first `ctx` is not done (history would otherwise be lost on every reload).
func TestTCHPE1_AReloadThatKeepsTheProviderEnabled(t *testing.T) {
	t.Skip("TC-HP-E1: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-E2: A listen failure never starts background work. Owner WI 1.1. Requirements: FR-1.
//
// - Setup: F-BG enabled; the listen address already bound by the test.
// - Steps: `Run`.
// - Expected: `Run` returns the listen error; `fbg.starts == 0`.
func TestTCHPE2_AListenFailureNeverStartsBackgroundWork(t *testing.T) {
	t.Skip("TC-HP-E2: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-E3: Providers without Background and unknown names are ignored. Owner WI 1.1. Requirements: CON-4.
//
// - Setup: `newBackgrounds([sectionProvider, F-BG], log)`.
// - Steps: `reconcile({"apt": true, "nope": true, "docker": true})`; `stopAll(ctx)`.
// - Expected: only F-BG starts; no panic or log error for `nope` or the non-Background provider; `stopAll` returns once F-BG returns.
func TestTCHPE3_ProvidersWithoutBackgroundAndUnknownNamesAre(t *testing.T) {
	t.Skip("TC-HP-E3: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-ERR1: A panicking RunBackground is logged and nexusd keeps serving. Owner WI 1.1. Requirements: NFR-4, FR-11.
//
// - Setup: F-BG in `panic` mode, enabled.
// - Steps: start; `GET /healthz`; `GET /v1/capabilities`.
// - Expected: both 200; the log has one error line naming provider `apt` with a stack (`goroutine` and `background.go` in it); the provider is still `enabled` in capabilities; a later reload disable/enable starts it again (`starts == 2`).
func TestTCHPERR1_APanickingRunBackgroundIsLoggedAndNexusd(t *testing.T) {
	t.Skip("TC-HP-ERR1: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-HP-ERR2: A runner that ignores cancellation cannot hold up reload or shutdown. Owner WI 1.1. Requirements: NFR-4, NFR-2.
//
// - Setup: F-BG `stubborn`; `backgroundStopTimeout = 200ms` (test seam; restored in cleanup); `ShutdownTimeout` 1 s.
// - Steps: reload disabling it; measure; re-enable and SIGTERM; measure; finally `release()` the runner.
// - Expected: the reload returns in 200–400 ms and logs a warning naming `apt`; shutdown completes within `ShutdownTimeout` + 200 ms and logs it still running; after `release`, goroutines return to baseline.
func TestTCHPERR2_ARunnerThatIgnoresCancellationCannotHold(t *testing.T) {
	t.Skip("TC-HP-ERR2: SKIP-only stub; un-skip when WI 1.1 lands")
}
