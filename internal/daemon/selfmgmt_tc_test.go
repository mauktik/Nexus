// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package daemon

import "testing"

// TC-SM-45 (WI 3.3; FR-4): API reloads and SIGHUP reloads never overlap
func TestTCSM45_APIReloadsAndSIGHUPReloadsNever(t *testing.T) {
	// - Setup: a real `Daemon` over a temp config with a test provider whose `Configure` increments an
	//   atomic `inFlight`, fails the test if it was already 1, sleeps 2 ms, then decrements.
	// - Steps:
	//   1. Run 100 goroutine-launched `d.Reload(ctx)` calls while delivering 100 `SIGHUP`s (through
	//     the daemon's signal channel seam) concurrently.
	// - Expected:
	//   - Never `inFlight > 1`.
	//   - All 100 API calls return nil.
	//   - The total `Configure` calls is at least 100 plus the number of SIGHUPs processed.
	t.Skip("TC-SM-45: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-SM-46 (WI 3.3; FR-4): Reload reports restartRequired and typed failures
func TestTCSM46_ReloadReportsRestartRequiredAndTypedFailures(t *testing.T) {
	// - Setup: a real `Daemon` started from config C0.
	// - Steps:
	//   1. Rewrite the file with `limits.maxBodyBytes` and `listen` changed, then call `Reload`.
	//   2. Rewrite with a syntax error, then call `Reload`.
	//   3. Rewrite with a provider section that the provider's `Configure` refuses, then call
	//     `Reload`.
	//   4. Read the provider's configuration in force.
	// - Expected:
	//   - Step 1: `RestartRequired` is exactly `["limits", "listen"]` (sorted, the keys actually
	//     changed), and the error is nil.
	//   - Step 2: `*provider.ReloadError{Stage: "load"}`.
	//   - Step 3: `*provider.ReloadError{Stage: "apply"}`.
	//   - Step 4: after step 3, the configuration in force is still step 1's.
	t.Skip("TC-SM-46: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-SM-47 (WI 3.3; FR-2): UseDaemon is called once and exposes the daemon's values
func TestTCSM47_UseDaemonIsCalledOnceAndExposes(t *testing.T) {
	// - Setup: a test provider implementing `DaemonUser` that records calls; `daemon.New` with config
	//   path P and state dir S.
	// - Steps:
	//   1. Construct the daemon.
	//   2. Read `ConfigPath()`, `StateDir()`, `StartedAt()` and `CheckConfig()` from the recorded
	//     `Daemon`.
	// - Expected:
	//   - `UseDaemon` was called exactly once.
	//   - The values are P, S, and a time within 1 s of construction.
	//   - `CheckConfig().Errors` is empty for a valid P.
	t.Skip("TC-SM-47: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-SM-E18 (WI 3.3; FR-4): SIGHUP keeps working unchanged with the nexusd provider enabled
func TestTCSME18_SIGHUPKeepsWorkingUnchangedWithThe(t *testing.T) {
	// - Setup: a real `Daemon` with a config change written to disk; no API call.
	// - Steps:
	//   1. Deliver SIGHUP through the signal seam.
	// - Expected:
	//   - The new provider config is in force.
	//   - The log has the existing reload line.
	//   - `restartRequired` keys are logged as before, unchanged from base.
	t.Skip("TC-SM-E18: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-SM-ERR25 (WI 3.3; FR-4): Reload returns promptly after shutdown or cancellation
func TestTCSMERR25_ReloadReturnsPromptlyAfterShutdownOr(t *testing.T) {
	// - Setup: a real `Daemon`.
	// - Steps:
	//   1. Cancel the `Run` context; after `stopping` closes, call `Reload(ctx)`.
	//   2. With the daemon running and the loop blocked in a slow reload, call `Reload` with a context
	//     cancelled at 50 ms.
	//   3. Reply-path case: a fresh running `Daemon` whose reload apply is gated by a test hook that
	//     signals `entered` and then waits for `release`. Call `Reload(ctx)`; after `entered` (the Run
	//     loop has taken the request), cancel `ctx`; then close `release`.
	//   4. After step 3, send SIGHUP through the daemon harness's existing signal path (`h.signal(t,
	//     syscall.SIGHUP)`, `daemon_test.go`), with a changed config on disk.
	//   5. Cancel the `Run` context.
	// - Expected:
	//   - Step 1 returns an error within 1 s.
	//   - Step 2 returns `context.Canceled` within 200 ms.
	//   - Step 3: `Reload` returns `context.Canceled` within 200 ms of the cancel, while the apply is
	//     still gated. After `release`, the taken reload runs to completion (the applied config
	//     generation increments once) and its result is dropped: the Run loop's reply send does not
	//     block, because each request carries its own reply channel with a buffer of exactly 1.
	//   - Step 4: the SIGHUP is served, and the generation increments again within 2 s. This is the
	//     observable proof that the Run loop did not block on the abandoned reply.
	//   - Step 5: `Run` returns within `ShutdownTimeout`.
	//   - Neither goroutine leaks (`goleak`-style count check).
	t.Skip("TC-SM-ERR25: SKIP-only stub; un-skip when WI 3.3 lands")
}
