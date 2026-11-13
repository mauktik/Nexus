//go:build integration

// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package integration

import "testing"

// TC-SM-103 (WI 6.2; FR-19): Uninstall removes nexusd.prev and rollback/; --purge also removes staging/
func TestTCSM103_UninstallRemovesNexusdPrevAndRollback(t *testing.T) {
	// - Setup: T1 clean host; install `v9.9.1`; create `/usr/local/bin/nexusd.prev`,
	//   `/var/lib/nexus/rollback/nexusd.service`, `/var/lib/nexus/staging/x/release.tar.gz` and one op
	//   dir.
	// - Steps:
	//   1. `uninstall.sh`, then inspect.
	//   2. Recreate the state, then run `uninstall.sh --purge` and inspect.
	// - Expected:
	//   - Step 1:
	//     - `nexusd`, `nexusd.prev`, the unit and `rollback/` are gone;
	//     - `staging/`, `ops/` and `/etc/nexus` remain;
	//     - `systemctl show nexusd.service -p LoadState` is `not-found`.
	//   - Step 2: `/var/lib/nexus` is gone, including `staging/`.
	t.Skip("TC-SM-103: SKIP-only stub; un-skip when WI 6.2 lands")
}

// TC-SM-104 (WI 6.2; FR-19, CON-5): A manual install.sh from an unpacked tree still works unchanged
func TestTCSM104_AManualInstallShFromAn(t *testing.T) {
	// - Setup: T1 wiped host; the `v9.9.1` tree from `/harness/selfupdate`.
	// - Steps:
	//   1. Unpack the tarball.
	//   2. `bash nexusd-v9.9.1-linux-<goarch>/install.sh`.
	//   3. Start the service.
	//   4. Rerun `install.sh` from the `v9.9.2` tree while the service is active.
	// - Expected:
	//   - Step 2 succeeds; `/usr/local/bin/nexusd -version` prints `nexusd v9.9.1`; the service is
	//     active.
	//   - Step 4: the binary is `v9.9.2`, the MainPID changed exactly once (the journal has one
	//     `Started` line), and the config is kept unchanged.
	t.Skip("TC-SM-104: SKIP-only stub; un-skip when WI 6.2 lands")
}

// TC-SM-107 (WI 7.1; NFR-6, FR-15, CON-1): Fixture tarballs are signed with a per-run key and verified on the host
func TestTCSM107_FixtureTarballsAreSignedWithA(t *testing.T) {
	// - Setup: inside T1, `/harness/selfupdate/`.
	// - Steps:
	//   1. List the tarballs, `.sig` files and `release-test.pub`.
	//   2. `release.Verify` each against the per-run public key.
	//   3. Read `build/harness/logs/<stamp>/run.log` on the host (the run.sh output is captured into
	//     the T1 log).
	// - Expected:
	//   - Tarballs `v9.9.1`…`v9.9.6` exist, plus `v9.9.7` if TC-SM-123's fixture is adopted, each with
	//     a `.sig`.
	//   - Verify is true for every version except `v9.9.6`, which is false.
	//   - Each top-level dir is `nexusd-<v>-linux-<goarch>`.
	//   - The log shows `nexus-release verify` passing for each trusted fixture before the copy-in.
	//   - The host's `~/.config/nexus/release-signing.key` mtime is unchanged, or the file is absent.
	t.Skip("TC-SM-107: SKIP-only stub; un-skip when WI 7.1 lands")
}

// TC-SM-109 (WI 7.2; FR-7, FR-9, FR-10, FR-14, NFR-6): T1 (a): an update from v9.9.1 to v9.9.2 succeeds with exactly one restart
func TestTCSM109_T1AAnUpdateFromV9(t *testing.T) {
	// - Setup: T1 clean host with `v9.9.1` installed through `install.sh`, the provider enabled, the
	//   per-run key trusted, TLS auto, and an admin token. Record `pid0 = systemctl show -p MainPID`.
	// - Steps:
	//   1. `nexus nexusd update --file /harness/selfupdate/nexusd-v9.9.2-….tar.gz` (plain wait,
	//     default).
	//   2. `nexus operation get <id> -o json`.
	// - Expected:
	//   - Exit 0.
	//   - The op is `Succeeded`, with `{previousVersion: v9.9.1, newVersion: v9.9.2, changed: true,
	//     rolledBack: false}`.
	//   - Read-backs:
	//     - `/usr/local/bin/nexusd -version` is `nexusd v9.9.2`;
	//     - `/usr/local/bin/nexusd.prev -version` is `nexusd v9.9.1`;
	//     - `/var/lib/nexus/rollback/nexusd.service` equals the pre-update unit.
	//   - The journal for `nexusd.service` since the start shows exactly one `Started` line, and the
	//     new MainPID is not pid0.
	//   - The op log contains the `install.sh` output (`installed nexusd v9.9.2`).
	//   - The real tree passes `scan`: the tarball is a real `make nexusd-tree` + `make nexusd-tarball`
	//     output (7.1, more than 64 entries per TC-SM-108), its upload answered 201, and the update op
	//     was created rather than refused `update-package-invalid`.
	//   - `staging/` is empty.
	//   - The `nexus-op-<id>` unit ran (`journalctl -u nexus-op-<id>` is non-empty).
	//   - The op never passed through `outcome-unknown`.
	t.Skip("TC-SM-109: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-110 (WI 7.2; FR-8, FR-9): T1 (b): a failing install.sh is rolled back
func TestTCSM110_T1BAFailingInstallSh(t *testing.T) {
	// - Setup: clean `v9.9.1` host.
	// - Steps:
	//   1. Update to `v9.9.3`, whose `install.sh` runs `exit 3` after the binary `mv`.
	// - Expected:
	//   - Exit 1.
	//   - The op is `Failed` with `update-failed`, `rolledBack: true`, `step: "install"`, and a detail
	//     naming exit code 3.
	//   - `/usr/local/bin/nexusd -version` is `v9.9.1`, and the unit file equals the original.
	//   - The service is `active`, and the MainPID differs from the one during install.
	t.Skip("TC-SM-110: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-111 (WI 7.2; FR-8, FR-9, FR-10): T1 (c): a binary that exits at start is rolled back after reset-failed
func TestTCSM111_T1CABinaryThatExits(t *testing.T) {
	// - Setup: clean `v9.9.1` host.
	// - Steps:
	//   1. Update to `v9.9.4`, a stub that exits 1 for `serve`.
	// - Expected:
	//   - Exit 1, with `update-failed` and `rolledBack: true`.
	//   - The op log contains a `reset-failed` step line.
	//   - The binary is `v9.9.1`, and `systemctl show -p ActiveState` is `active`.
	//   - `Result` is `success` after recovery.
	t.Skip("TC-SM-111: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-112 (WI 7.2; FR-8, FR-9, FR-10): T1 (d): a healthy binary that reports the wrong version is rolled back
func TestTCSM112_T1DAHealthyBinaryThat(t *testing.T) {
	// - Setup: clean `v9.9.1` host.
	// - Steps:
	//   1. Update to `v9.9.5`, whose `install.sh` installs a `v9.9.8` binary.
	// - Expected:
	//   - `update-failed`, `rolledBack: true`, `step: "health"`, with a detail naming `v9.9.8` and
	//     `v9.9.5`.
	//   - The binary is `v9.9.1`.
	t.Skip("TC-SM-112: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-113 (WI 7.2; FR-7, FR-16, NFR-5): T1 (e): a tarball signed by an untrusted key is refused, with no op and the upload deleted
func TestTCSM113_T1EATarballSignedBy(t *testing.T) {
	// - Setup: clean `v9.9.1` host.
	// - Steps:
	//   1. Update to `v9.9.6`.
	//   2. `nexus operation get -o json`.
	//   3. `ls /var/lib/nexus/staging`.
	//   4. Read `/var/log/nexus/audit.jsonl`.
	// - Expected:
	//   - Exit 1, `update-signature-invalid`.
	//   - No nexusd op exists.
	//   - `staging/` is empty.
	//   - The last audit record is `refused` with problem `update-signature-invalid` and
	//     `facts.signature == "invalid"`.
	//   - The MainPID is unchanged.
	t.Skip("TC-SM-113: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-114 (WI 7.2; FR-5, FR-9, FR-10): T1 (f): restart produces a new MainPID and reports the version
func TestTCSM114_T1FRestartProducesANew(t *testing.T) {
	// - Setup: clean `v9.9.1` host; pid0.
	// - Steps:
	//   1. `nexus nexusd restart`.
	// - Expected:
	//   - Exit 0.
	//   - The op is `Succeeded {version: v9.9.1, changed: true}`.
	//   - The MainPID is not pid0, and the service is active.
	//   - The op is recorded by the restarted nexusd (`operation get` after the restart shows the
	//     terminal state).
	t.Skip("TC-SM-114: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-115 (WI 7.2; FR-12, FR-9, ASM-7): T1 (g): rollback after (a), then rollback again
func TestTCSM115_T1GRollbackAfterAThen(t *testing.T) {
	// - Setup: the state after TC-SM-109's steps, run inside this case.
	// - Steps:
	//   1. `nexus nexusd rollback --yes`.
	//   2. Check the binaries.
	//   3. `nexus nexusd rollback --yes` again.
	// - Expected:
	//   - Step 1: `Succeeded {fromVersion: v9.9.2, toVersion: v9.9.1}`.
	//   - Step 2: the binary is `v9.9.1` and prev is `v9.9.2`.
	//   - Step 3: `{fromVersion: v9.9.1, toVersion: v9.9.2}`, the binary is `v9.9.2` again, and the
	//     unit files are swapped back (hashes equal their originals).
	t.Skip("TC-SM-115: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-116 (WI 7.2; FR-12): T1 (h): rollback on a fresh install is rollback-unavailable
func TestTCSM116_T1HRollbackOnAFresh(t *testing.T) {
	// - Setup: clean `v9.9.1` host with no prev.
	// - Steps:
	//   1. `nexus nexusd rollback --yes`.
	// - Expected:
	//   - Exit 1, `rollback-unavailable` with `path: /usr/local/bin/nexusd.prev`.
	//   - No op exists, and the MainPID is unchanged.
	t.Skip("TC-SM-116: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-117 (WI 7.2; FR-4, FR-3): T1 (i): reload refuses an unknown key, then applies a valid change
func TestTCSM117_T1IReloadRefusesAnUnknown(t *testing.T) {
	// - Setup: clean `v9.9.1` host; pid0.
	// - Steps:
	//   1. Append `bogusKey: 1` to `/etc/nexus/nexusd.yaml`, then run `nexus nexusd reload`.
	//   2. Restore the file, change `limits.maxBodyBytes`, then run `nexus nexusd reload`.
	// - Expected:
	//   - Step 1: exit 1, `config-invalid`, with `errors[0]` naming `/etc/nexus/nexusd.yaml` and the
	//     line and column of `bogusKey`. No op exists, and the MainPID equals pid0.
	//   - Step 2: exit 0, `Succeeded {applied: true, restartRequired: ["limits"]}`, and the MainPID
	//     still equals pid0.
	t.Skip("TC-SM-117: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-118 (WI 7.2; FR-11, FR-9): T1 (j): a concurrent request during an update gets 409, including right after the restart
func TestTCSM118_T1JAConcurrentRequestDuring(t *testing.T) {
	// - Setup: clean `v9.9.1` host.
	// - Steps:
	//   1. `nexus nexusd update --file v9.9.2 --no-wait`, giving op U.
	//   2. Loop every 200 ms until U is terminal, POSTing `nexus nexusd restart --no-wait` and
	//     recording each status, and the MainPID at each attempt.
	// - Expected:
	//   - Every attempt while U is non-terminal and the server answers gets 409 `conflict` with
	//     `blockingOperationId == U`.
	//   - At least one 409 comes from the new MainPID, which proves the lane was restored before the
	//     listener opened.
	//   - No restart op is created.
	t.Skip("TC-SM-118: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-119 (WI 7.2; FR-14): T1 (k): --follow prints log output after reconnecting across the restart
func TestTCSM119_T1KFollowPrintsLogOutput(t *testing.T) {
	// - Setup: clean `v9.9.1` host.
	// - Steps:
	//   1. `nexus nexusd update --file v9.9.2 --follow`, capturing stdout and stderr with timestamps.
	// - Expected:
	//   - Exit 0.
	//   - stderr contains `likely restarting`.
	//   - stdout contains log lines emitted after the restart (`healthy <ts>`), with no duplicated
	//     line.
	t.Skip("TC-SM-119: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-120 (WI 7.2; FR-7, ASM-6): T1: updating to the running version changes nothing
func TestTCSM120_T1UpdatingToTheRunningVersion(t *testing.T) {
	// - Setup: clean `v9.9.1` host; pid0; the binary hash.
	// - Steps:
	//   1. `nexus nexusd update --file v9.9.1`.
	// - Expected:
	//   - Exit 0, `Succeeded {changed: false}`.
	//   - The MainPID equals pid0, and the binary hash is unchanged.
	//   - No `nexusd.prev` was created.
	//   - `staging/` is empty after finalize.
	t.Skip("TC-SM-120: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-121 (WI 7.2; FR-7, ASM-6): T1: a downgrade is refused without the flag and installed with it
func TestTCSM121_T1ADowngradeIsRefusedWithout(t *testing.T) {
	// - Setup: host updated to `v9.9.2` (TC-SM-109 steps).
	// - Steps:
	//   1. `nexus nexusd update --file v9.9.1`.
	//   2. Repeat with `--allow-downgrade`.
	// - Expected:
	//   - Step 1: exit 1, `version-downgrade` (runningVersion `v9.9.2`, packageVersion `v9.9.1`), the
	//     upload is still staged, and there is no op.
	//   - Step 2: exit 0, `Succeeded {previousVersion: v9.9.2, newVersion: v9.9.1}`, and `-version`
	//     gives `v9.9.1`.
	t.Skip("TC-SM-121: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-122 (WI 7.2; FR-7, NFR-5): T1: a tarball with one byte flipped after signing is refused
func TestTCSM122_T1ATarballWithOneByte(t *testing.T) {
	// - Setup: copy `v9.9.2.tar.gz` and its `.sig`; flip one byte at offset `size/2` of the copy.
	// - Steps:
	//   1. Update with the tampered copy and the original signature.
	// - Expected:
	//   - Exit 1, `update-signature-invalid`.
	//   - No op exists, `staging/` is empty, and the binary is unchanged.
	t.Skip("TC-SM-122: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-123 (WI 7.2; FR-8, FR-9, FR-18): T1: update-rollback-failed is recorded by the next nexusd to start
func TestTCSM123_T1UpdateRollbackFailedIsRecorded(t *testing.T) {
	// - Setup: clean `v9.9.1` host; the known-good `v9.9.1` fixture tree from 7.1 available in the
	//   container.
	// - Steps:
	//   1. `nexus nexusd update --file …/nexusd-v9.9.7-….tar.gz`. Record the op ID (from the CLI output
	//     or `nexus operation list`). The CLI may exit 5 here, because nexusd stays down past the
	//     reconnect window (A-6); the test accepts exit 0, 1 or 5 and asserts nothing else about this
	//     exit.
	//   2. Wait (`waitUntil`, bounded by the op's MaxRuntime) until `systemctl show nexus-op-<id> -p
	//     ActiveState` is `inactive` or `failed`.
	//   3. Recover by hand: `lib.sh install-release` from the known-good `v9.9.1` tree, then `systemctl
	//     reset-failed nexusd.service`, then `systemctl start nexusd.service`.
	//   4. `nexus operation get <id> -o json`.
	// - Expected:
	//   - Before step 3, `/usr/local/bin/nexusd.prev -version` prints `nexusd v9.9.1` (the clobbering
	//     stub) and `nexusd.service` is not `active`, proving the restore really failed on systemd.
	//   - `/var/lib/nexus/ops/<id>/exit.json` and `result.json` exist before step 3.
	//   - After step 3, `/usr/local/bin/nexusd -version` is `nexusd v9.9.1` and the unit is `active`.
	//   - Step 4 shows `Failed` with `error.type` ending `update-rollback-failed`, `rolledBack: false`,
	//     and a detail containing "may be down" and `docs/install.md#recovering-from-a-failed-update`.
	//   - The op is never `outcome-unknown`: neither in step 4 nor in any `nexus operation get` poll
	//     made between steps 2 and 4.
	t.Skip("TC-SM-123: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-124 (WI 7.2; FR-12): T1: rollback is refused when the previous binary rejects the current config
func TestTCSM124_T1RollbackIsRefusedWhenThe(t *testing.T) {
	// - Setup: clean `v9.9.1` host. As root, install a stub `/usr/local/bin/nexusd.prev`: `-version`
	//   prints `nexusd v9.9.0`; `validate-config` prints two lines to stderr and exits 1.
	// - Steps:
	//   1. `nexus nexusd rollback --yes`.
	// - Expected:
	//   - Exit 1, `config-invalid`, whose `errors[].message` are the two stub lines in order.
	//   - No op exists, and the binaries are unchanged.
	t.Skip("TC-SM-124: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-125 (WI 7.2; FR-2, FR-3, FR-1): T1: get and validate-config read back the real host state
func TestTCSM125_T1GetAndValidateConfigRead(t *testing.T) {
	// - Setup: clean `v9.9.1` host, then update to `v9.9.2`.
	// - Steps:
	//   1. `nexus nexusd get -o json` before the update.
	//   2. The same after the update.
	//   3. `nexus nexusd validate-config` with a valid config.
	//   4. Use a token with only `nexusd:read` for `update` and `restart`.
	// - Expected:
	//   - Step 1:
	//     - `version: v9.9.1`, `previous.present: false`;
	//     - `configPath: /etc/nexus/nexusd.yaml`, `executablePath: /usr/local/bin/nexusd`;
	//     - `selfManagement.available: true`.
	//   - Step 2: `version: v9.9.2`, `previous: {present: true, version: v9.9.1}`, and `uptimeSeconds`
	//     reset (below the step-1 value plus elapsed).
	//   - Step 3: `valid: true`.
	//   - Step 4: exit 3 with 403 naming `nexusd:admin`.
	t.Skip("TC-SM-125: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-126 (WI 7.2; NFR-1): T1: NFR-1 update downtime p95 under 15 s, and rollback within healthTimeout + 20 s
func TestTCSM126_T1NFR1UpdateDowntimeP95(t *testing.T) {
	// - Setup: 5 clean-host runs of the TC-SM-109 steps, plus the rollback cases TC-SM-110 to
	//   TC-SM-112.
	// - Steps:
	//   1. Parse `restart-started <ts>` and `healthy <ts>` from each op log.
	// - Expected:
	//   - p95 (the maximum of 5) is under 15 s.
	//   - Each rollback case reaches its post-restore `healthy` within 30 s + 20 s of
	//     `restart-started`.
	//   - The figures are written to the test log for `docs/verification/measurements.md`.
	t.Skip("TC-SM-126: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-127 (WI 7.2; NFR-2): T1: NFR-2 get and validate-config p95 under 200 ms
func TestTCSM127_T1NFR2GetAndValidate(t *testing.T) {
	// - Setup: host with a prev present (after an update).
	// - Steps:
	//   1. Make 50 REST `get` and 50 `validate-config` calls on loopback, timing each.
	// - Expected:
	//   - The p95 of each is under 200 ms.
	//   - Prev is executed at most once (journal and audit show no per-call exec; measured by the
	//     `-version` exec count through `strace -f -e execve -p <MainPID>` or the cache counter in
	//     debug logs).
	t.Skip("TC-SM-127: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-128 (WI 7.2; NFR-4, FR-6): T1: NFR-4 upload RSS bound, and staging cleanup after a refusal and after an expiry sweep
func TestTCSM128_T1NFR4UploadRSSBound(t *testing.T) {
	// - Setup: clean host; a 64 MiB random file plus a signature (the content is irrelevant to the
	//   upload).
	// - Steps:
	//   1. Sample `VmRSS` of the MainPID idle, then every 50 ms during a 64 MiB REST upload.
	//   2. Upload, then update with a bad signature, then list `staging/`.
	//   3. Upload, edit its `meta.json` `expires` to the past, `systemctl restart nexusd`, then list
	//     `staging/`.
	// - Expected:
	//   - Step 1: max RSS minus idle is under 8 MiB.
	//   - Step 2: `staging/` is empty.
	//   - Step 3: `staging/` is empty after the restart (the sweep).
	t.Skip("TC-SM-128: SKIP-only stub; un-skip when WI 7.2 lands")
}

// TC-SM-129 (WI 7.2; NFR-6): T1: real systemd confirms the fake's encoded behaviours
func TestTCSM129_T1RealSystemdConfirmsTheFake(t *testing.T) {
	// - Setup: clean host. A transient copy of the shipped unit (`nexusd-fidelity.service`, same
	//   `Type`, `Restart` and `ExecStart` pattern) pointed at the `v9.9.4` stub, to avoid touching
	//   `nexusd.service`.
	// - Steps:
	//   1. `systemctl restart --no-block` against a healthy unit, timing the return and reading
	//     MainPID before and after.
	//   2. Blocking `systemctl restart` on the stub that exits at start; read the exit code and
	//     `systemctl show -p Result -p NRestarts -p ActiveState`.
	//   3. `systemctl restart` again.
	//   4. `reset-failed`, then restart.
	//   5. Run `install.sh` while the unit is inactive and count `Started` lines.
	// - Expected:
	//   - Step 1 returns in under 1 s, and a new MainPID appears later.
	//   - Step 2: a non-zero exit; within 10 s `Result=start-limit-hit` and `ActiveState=failed`.
	//   - Step 3 fails with `start request repeated too quickly`.
	//   - Step 4 starts again.
	//   - Step 5: 0 `Started` lines.
	//   - Each observation is written next to F-SYS's encoded expectation in the test log. A mismatch
	//     fails the test and means F-SYS must be corrected.
	t.Skip("TC-SM-129: SKIP-only stub; un-skip when WI 7.2 lands")
}
