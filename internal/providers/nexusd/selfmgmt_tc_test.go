// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package nexusd

import "testing"

// TC-SM-88 (WI 5.4; FR-7, FR-9, FR-10, CON-2): Update helper success: backup, install, exactly one restart, health
func TestTCSM88_UpdateHelperSuccessBackupInstallExactly(t *testing.T) {
	// - Setup:
	//   - F-SYS with PID 100 running `#!fake nexusd v9.9.1 ok` (B1) from `Paths.Binary`;
	//   - `UnitFile` holds U1;
	//   - packageDir holds `nexusd` (B2, `v9.9.2 ok`) and `nexusd.service` U2;
	//   - F-SYS `RunInstall` uses the default install.sh model;
	//   - `input.json` with `runningVersion v9.9.1` and `expectedVersion v9.9.2`.
	// - Steps:
	//   1. Run the helper seam.
	//   2. Read `result.json`, the files and `fake.starts`.
	// - Expected:
	//   - `result.json` is `{changed: true, previousVersion: "v9.9.1", newVersion: "v9.9.2",
	//     rolledBack: false}`.
	//   - `Prev == B1`, copied from `/proc/100/exe`, which is verified to report `v9.9.1`.
	//   - `<state>/rollback/nexusd.service == U1`.
	//   - `Binary == B2`.
	//   - `fake.starts == 1`, done by install.
	//   - The new MainPID is not 100.
	//   - packageDir is removed.
	//   - stdout has the step lines `precheck`, `backup`, `install`, `restart-started <ts>` and
	//     `healthy <ts>`, in that order.
	//   - Exit 0.
	t.Skip("TC-SM-88: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-89 (WI 5.4; FR-7): When install.sh does not restart, the helper restarts exactly once
func TestTCSM89_WhenInstallShDoesNotRestart(t *testing.T) {
	// - Setup: as TC-SM-88, with F-SYS install option `noRestart`.
	// - Steps:
	//   1. Run the helper.
	// - Expected:
	//   - `fake.starts == 1`, done by the helper's `Restart` with `--no-block`.
	//   - The result is Succeeded with `newVersion: v9.9.2`.
	t.Skip("TC-SM-89: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-90 (WI 5.4; FR-7, ASM-6): The noChange path makes no system calls
func TestTCSM90_TheNoChangePathMakesNoSystem(t *testing.T) {
	// - Setup: `input.json {mode: "update", noChange: true}`; F-SYS.
	// - Steps:
	//   1. Run the helper.
	// - Expected:
	//   - `result.json == {changed: false}`.
	//   - `fake.calls` is empty: no Show, install or start.
	//   - The file hashes are unchanged.
	//   - Exit 0.
	t.Skip("TC-SM-90: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-91 (WI 5.4; FR-7, FR-9): T0 full detached loop: a same-version update through the real supervisor and helper
func TestTCSM91_T0FullDetachedLoopASame(t *testing.T) {
	// - Setup:
	//   - nexusd T0 stack with the ops manager `Detach: detachtest.Local()`;
	//   - `Options.Executable` returns `detachtest.Supervisor()`, the real built nexusd;
	//   - the running version is set equal to the package version `v9.9.1`;
	//   - an upload signed by K.
	// - Steps:
	//   1. POST update.
	//   2. Poll the op until it is terminal (timeout 30 s).
	// - Expected:
	//   - The op is `Succeeded` with `result.changed == false`.
	//   - `ops/<id>/exit.json` exists with code 0, and `result.json` is `{changed: false}`.
	//   - `staging/<uploadId>` is gone.
	//   - No systemctl was invoked: the supervisor's log has no systemctl lines, and the macOS run
	//     proves it.
	t.Skip("TC-SM-91: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-92 (WI 5.4; FR-8, FR-9): The helper writes result.json and exits 0 in every path after it starts changing the host
func TestTCSM92_TheHelperWritesResultJsonAnd(t *testing.T) {
	// - Setup: the scenario table:
	//   - TC-SM-88 and TC-SM-89;
	//   - TC-SM-ERR51 to TC-SM-ERR58;
	//   - TC-SM-ERR32;
	//   - TC-SM-ERR37.
	// - Steps:
	//   1. Run each scenario.
	//   2. Run the helper a second time on the same dir.
	// - Expected:
	//   - Every scenario exits 0, and `result.json` parses into `helperResult`.
	//   - Every second run exits 1 with `result.json already exists`, makes zero F-SYS calls and
	//     leaves `result.json` unchanged.
	t.Skip("TC-SM-92: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-97 (WI 6.1; FR-13, FR-2, FR-3, FR-4): CLI end to end: get, validate-config and reload through cli.Run against the nexusd stack
func TestTCSM97_CLIEndToEndGetValidate(t *testing.T) {
	// - Setup: nexusd T0 stack with a client config in a temp HOME (`writeClientConfig` pattern);
	//   `admin` token.
	// - Steps:
	//   1. `nexus nexusd get -o json`.
	//   2. `nexus nexusd get` (table).
	//   3. Write Prev and repeat step 1.
	//   4. `nexus nexusd validate-config` on a valid config, then an invalid one.
	//   5. `nexus nexusd reload` on a valid config, then an invalid one.
	// - Expected:
	//   - Step 1: exit 0, with the TC-SM-51 fields.
	//   - Step 2: table columns `name, version, uptimeSeconds, previous.version, updateConfigured`.
	//   - Step 3: `previous.version` is updated.
	//   - Step 4: exit 0 for both. The invalid one prints `valid: false` and each error with its
	//     location.
	//   - Step 5:
	//     - the valid reload exits 0 with `restartRequired`;
	//     - the invalid one exits 1, with stderr naming `config-invalid` and the first error's
	//       `file:line:column`;
	//     - no op is created by the invalid reload.
	t.Skip("TC-SM-97: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-98 (WI 6.1; FR-7, FR-13, FR-16): CLI end to end: update refusals map to problem types and exit codes
func TestTCSM98_CLIEndToEndUpdateRefusals(t *testing.T) {
	// - Setup: nexusd T0 stack; fixtures in a temp dir:
	//   - `good-v9.9.2.tar.gz` signed by K;
	//   - `untrusted.tar.gz` signed by K2;
	//   - `traversal.tar.gz` signed by K;
	//   - `old-v9.9.0.tar.gz` signed by K, while running `v9.9.1`.
	// - Steps:
	//   1. With `trustedKeys: []`, run `nexus nexusd update --file good…`.
	//   2. With K trusted, update `untrusted`.
	//   3. Update `traversal`.
	//   4. Update `old`.
	//   5. Update `old --allow-downgrade --no-wait`.
	// - Expected:
	//   - Step 1: exit 1, `update-not-configured`.
	//   - Step 2: exit 1, `update-signature-invalid`.
	//   - Step 3: exit 1, `update-package-invalid` with `check: path`.
	//   - Step 4: exit 1, `version-downgrade` naming `v9.9.1` and `v9.9.0`.
	//   - Step 5: exit 0, printing the queued op.
	//   - Steps 1–4 created no op (`nexus operation get` shows only the step-5 op).
	t.Skip("TC-SM-98: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-99 (WI 6.1; FR-13, FR-1, FR-6): nexus command get lists six nexusd commands, and capabilities and OpenAPI list the provider
func TestTCSM99_NexusCommandGetListsSixNexusd(t *testing.T) {
	// - Setup: nexusd T0 stack, plus the composed `daemon.New` with `providers.nexusd.enabled: true`.
	// - Steps:
	//   1. `nexus command get -o json`.
	//   2. `GET /v1/capabilities`.
	//   3. `GET /v1/openapi.json`.
	// - Expected:
	//   - nexusd commands are exactly `get`, `validate-config`, `reload`, `restart`, `update` and
	//     `rollback`.
	//   - nexusd appears in capabilities.
	//   - The OpenAPI document has the 7 nexusd routes, including `POST /v1/providers/nexusd/uploads`
	//     with a binary body and the problems `upload-too-large`, `conflict` and
	//     `storage-limit-reached`.
	t.Skip("TC-SM-99: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-E11 (WI 6.1; FR-11, FR-6): A CLI update retried with the same idempotency key replays the same op
func TestTCSME11_ACLIUpdateRetriedWithThe(t *testing.T) {
	// - Setup: nexusd T0 stack with a spy launch (the op stays Queued or Running); `good-v9.9.2`
	//   signed by K.
	// - Steps:
	//   1. `nexus nexusd update --file good --idempotency-key k1 --no-wait`, run twice.
	// - Expected:
	//   - Both runs print the same op ID.
	//   - There is one staging dir and one op.
	//   - Each run has a `stored` record for its upload, but only one `requested` record for the
	//     update op.
	t.Skip("TC-SM-E11: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-E15 (WI 6.1; FR-12, FR-13): CLI rollback is destructive and requires confirmation
func TestTCSME15_CLIRollbackIsDestructiveAndRequires(t *testing.T) {
	// - Setup: nexusd T0 stack with Prev present; no TTY (`OpenTerminal` errors); stdin empty.
	// - Steps:
	//   1. `nexus nexusd rollback`.
	//   2. `nexus nexusd rollback --yes --no-wait`.
	// - Expected:
	//   - Step 1: exit 2 with the existing "confirmation required" message, and zero mutating
	//     requests.
	//   - Step 2: a POST is sent and the op is created.
	t.Skip("TC-SM-E15: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-ERR51 (WI 5.4; FR-8, FR-9): install.sh exit 3 is rolled back with step install
func TestTCSMERR51_InstallShExit3IsRolled(t *testing.T) {
	// - Setup: as TC-SM-88, with F-SYS install option `exitAfterRename=3` (the v9.9.3 analogue).
	// - Steps:
	//   1. Run the helper.
	//   2. Finalize.
	// - Expected:
	//   - `result.json`: error `update-failed`, `rolledBack: true`, `step: "install"`, and a detail
	//     containing `exit code 3`.
	//   - `Binary == B1`, copied from prev, and prev still `== B1` (A-21).
	//   - `UnitFile == U1`.
	//   - The F-SYS call order after install is `DaemonReload`, `ResetFailed`, `Restart`.
	//   - Health was checked against `v9.9.1`, excluding pidR.
	//   - After the helper returns, `packageDir` (`ops/<id>/pkg`) does not exist and `result.json`
	//     does: the helper removes the extracted package on every terminal path once `result.json` is
	//     written.
	//   - Finalize cleanup: re-create `ops/<id>/pkg` with one file, call `Finalize`, and `ops/<id>/pkg`
	//     is gone; call `Finalize` again with it already absent, and it returns no error (idempotent
	//     removal). Prepare failures are covered by Prepare's own removal (TC-SM-ERR49, TC-SM-ERR60),
	//     and a lost unit's `pkg/` by op retention; see A-50.
	t.Skip("TC-SM-ERR51: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-ERR52 (WI 5.4; FR-8, FR-10): A binary that exits at start is rolled back after reset-failed clears the start limit
func TestTCSMERR52_ABinaryThatExitsAtStart(t *testing.T) {
	// - Setup: as TC-SM-88, with the package binary `exit-at-start` (the v9.9.4 analogue). The
	//   blocking restart in install fails, so install exits 1, and F-SYS reaches `start-limit-hit`.
	// - Steps:
	//   1. Run the helper.
	// - Expected:
	//   - `update-failed`, `rolledBack: true`, `step: "install"`.
	//   - The restore succeeds only because `ResetFailed` preceded `Restart`. A planted variant that
	//     skips `ResetFailed` makes F-SYS refuse with `start request repeated too quickly`, which the
	//     test asserts to prove the fake enforces it.
	//   - The final unit is `active` with B1.
	t.Skip("TC-SM-ERR52: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-ERR53 (WI 5.4; FR-8, FR-10): A wrong version is rolled back, naming both versions
func TestTCSMERR53_AWrongVersionIsRolledBack(t *testing.T) {
	// - Setup: as TC-SM-88, with install option `installAlt=v9.9.8` and an expected version of
	//   `v9.9.5`.
	// - Steps:
	//   1. Run the helper.
	// - Expected: `update-failed`, `rolledBack: true`, `step: "health"`, with a detail containing
	//   `v9.9.8` and `v9.9.5`, and `Binary == B1`.
	t.Skip("TC-SM-ERR53: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-ERR54 (WI 5.4; FR-8, FR-9, FR-18): A restore that fails its health check is update-rollback-failed
func TestTCSMERR54_ARestoreThatFailsItsHealth(t *testing.T) {
	// - Setup: as TC-SM-ERR51. In addition, F-SYS makes B1 `exit-at-start` after the restore (the prev
	//   image is corrupted by the fake install, the v9.9.7 analogue).
	// - Steps:
	//   1. Run the helper.
	// - Expected:
	//   - `result.json`: `update-rollback-failed`, `rolledBack: false`.
	//   - The detail contains `nexusd may be down` and
	//     `docs/install.md#recovering-from-a-failed-update`.
	//   - The helper exits 0.
	//   - After the helper returns, `packageDir` (`ops/<id>/pkg`) does not exist and `result.json`
	//     does.
	//   - Finalize maps the result to that problem, and `ops/<id>/pkg` is still absent afterwards.
	t.Skip("TC-SM-ERR54: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-ERR55 (WI 5.4; FR-8): An install.sh timeout is rolled back
func TestTCSMERR55_AnInstallShTimeoutIsRolled(t *testing.T) {
	// - Setup: as TC-SM-88, with `installTimeout: 300ms` in `input.json` and install option `hang`.
	// - Steps:
	//   1. Run the helper.
	// - Expected:
	//   - `update-failed`, `rolledBack: true`, `step: "install"`, with a detail containing `timed
	//     out`.
	//   - The fake install's context was cancelled.
	t.Skip("TC-SM-ERR55: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-ERR56 (WI 5.4; FR-7, FR-8): A backup version mismatch is update-failed at backup, with nothing changed
func TestTCSMERR56_ABackupVersionMismatchIsUpdate(t *testing.T) {
	// - Setup: as TC-SM-88, but `/proc/100/exe` reports `v9.9.0`, not the `runningVersion` `v9.9.1`.
	// - Steps:
	//   1. Run the helper.
	// - Expected:
	//   - `update-failed`, `step: "backup"`, `rolledBack: false`.
	//   - `Paths.Prev` was not created, and no `nexusd.prev.tmp` remains.
	//   - `Binary` and `UnitFile` are unchanged.
	//   - F-SYS has no install and no start.
	t.Skip("TC-SM-ERR56: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-ERR57 (WI 5.4; FR-7): An inactive service at precheck changes nothing
func TestTCSMERR57_AnInactiveServiceAtPrecheckChanges(t *testing.T) {
	// - Setup: as TC-SM-88, with the unit `inactive`.
	// - Steps:
	//   1. Run the helper.
	// - Expected:
	//   - `update-failed`, `step: "precheck"`, `rolledBack: false`.
	//   - No file changed, and F-SYS made 0 starts and 0 installs.
	t.Skip("TC-SM-ERR57: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SM-ERR58 (WI 5.4; FR-8, FR-5): A systemctl call that hangs is bounded, and a result is still written
func TestTCSMERR58_ASystemctlCallThatHangsIs(t *testing.T) {
	// - Setup: as TC-SM-88. F-SYS `Restart` during restore blocks until ctx is done. The per-call
	//   timeout is injected as 200 ms; the default is 30 s.
	// - Steps:
	//   1. Run the helper with `exitAfterRename=3`, which forces a restore.
	// - Expected:
	//   - The helper returns within the bounded time.
	//   - `result.json` is `update-rollback-failed` with a detail naming the restart timeout.
	//   - Exit 0.
	t.Skip("TC-SM-ERR58: SKIP-only stub; un-skip when WI 5.4 lands")
}
