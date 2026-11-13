// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package nexusd

import "testing"

// TC-SM-67 (WI 4.3; FR-12, FR-9, CON-2): A rollback swaps the binary and unit files and verifies the target version
func TestTCSM67_ARollbackSwapsTheBinaryAnd(t *testing.T) {
	// - Setup:
	//   - temp-root Paths: `Binary` = `#!fake nexusd v9.9.2 ok` (bytes B2), `Prev` = `#!fake nexusd
	//     v9.9.1 ok` (bytes B1);
	//   - `UnitFile` holds U2, and `<state>/rollback/nexusd.service` holds U1;
	//   - F-SYS PID 100 runs B2;
	//   - Runner: prev `validate-config` exits 0, and prev `-version` gives `v9.9.1`.
	// - Steps:
	//   1. Admit.
	//   2. Prepare.
	//   3. Run the helper.
	//   4. Finalize.
	//   5. Inspect the files and the F-SYS call order.
	// - Expected:
	//   - `input.json` has `fromVersion: version.Version` (the running binary's), `toVersion:
	//     "v9.9.1"`, and `MaxRuntime == 2*(30s+20s)+60s`.
	//   - After the run:
	//     - `Binary == B1`, `Prev == B2`;
	//     - `UnitFile == U1`, saved unit `== U2`;
	//     - no `*.tmp` remains.
	//   - The F-SYS order is `DaemonReload`, `ResetFailed`, `Restart`, then `Show`… (health), with
	//     exactly 1 start.
	//   - The result is `{changed: true, fromVersion, toVersion: "v9.9.1"}`.
	t.Skip("TC-SM-67: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SM-68 (WI 4.3; FR-12, ASM-7): Rolling back a rollback restores the original pair byte for byte
func TestTCSM68_RollingBackARollbackRestoresThe(t *testing.T) {
	// - Setup: state after TC-SM-67.
	// - Steps:
	//   1. Run a second rollback (Admit, Prepare, helper, Finalize) with the expected version
	//     `v9.9.2`.
	// - Expected:
	//   - `Binary == B2`, `Prev == B1`, `UnitFile == U2`, saved unit `== U1`.
	//   - The SHA-256 of each file equals its original.
	//   - The result is `{fromVersion: v9.9.1, toVersion: v9.9.2}`.
	t.Skip("TC-SM-68: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SM-69 (WI 4.3; FR-12): With no saved unit, only the binary is swapped
func TestTCSM69_WithNoSavedUnitOnlyThe(t *testing.T) {
	// - Setup: as TC-SM-67, without `<state>/rollback/nexusd.service`.
	// - Steps:
	//   1. Run the full rollback pipeline.
	// - Expected:
	//   - The binaries are swapped.
	//   - The `UnitFile` hash is unchanged.
	//   - No `rollback/` directory is created.
	//   - The result is Succeeded.
	t.Skip("TC-SM-69: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SM-70 (WI 4.3; FR-12, NFR-5): Crash injection: the binary path exists after every intermediate swap step
func TestTCSM70_CrashInjectionTheBinaryPathExists(t *testing.T) {
	// - Setup: the swap sequence with a fault hook that aborts after step k, for k = 1..N (copy
	//   current to swap.tmp; rename prev to current; rename swap.tmp to prev; and the unit
	//   equivalents).
	// - Steps:
	//   1. For each k, run the swap in a fresh temp root with the fault at k.
	//   2. Inspect the files.
	// - Expected:
	//   - For every k, `Paths.Binary` exists and its content is B1 or B2.
	//   - Wherever `Prev` is missing, `nexusd.swap.tmp` exists and holds the other binary, so no
	//     binary content is ever lost.
	t.Skip("TC-SM-70: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SM-71 (WI 5.1; FR-6): An upload over REST stores the body with the right layout, modes and meta
func TestTCSM71_AnUploadOverRESTStoresThe(t *testing.T) {
	// - Setup: nexusd T0 stack (systemd available); `maxUploadBytes: 1048576`; body 200 KiB random;
	//   signature S (88 characters).
	// - Steps:
	//   1. `POST /v1/providers/nexusd/uploads?signature=S` with the `admin` token and `Content-Type:
	//     application/gzip`.
	//   2. Inspect `<state>/staging`.
	// - Expected:
	//   - 201 with `{uploadId matching ^[0-9a-f]{32}$, sha256 == hex(sha256(body)), sizeBytes ==
	//     204800, expiresAt == received+1h}`.
	//   - `staging/` is `0700`, `staging/<id>/` is `0700`, and `release.tar.gz` is `0400` with content
	//     equal to the body.
	//   - `meta.json` has `{uploadId, sha256, sizeBytes, signature: S, received, expires}`.
	//   - No `*.partial` remains.
	t.Skip("TC-SM-71: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-72 (WI 5.1; FR-6): Re-uploading the same bytes and signature returns the existing uploadId
func TestTCSM72_ReUploadingTheSameBytesAnd(t *testing.T) {
	// - Setup: as TC-SM-71.
	// - Steps:
	//   1. Upload (body, S) twice.
	//   2. Upload (body, S'), where S' is a different valid-pattern signature.
	// - Expected:
	//   - Both responses in step 1 carry the same `uploadId`, and exactly one staging dir exists.
	//   - Step 2 gives a new ID, so there are 2 dirs.
	//   - The audit log has a `stored` record for every request (three).
	t.Skip("TC-SM-72: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-73 (WI 5.1; FR-6, NFR-4): Expiry deletes the staged upload, and a later update is upload-not-found
func TestTCSM73_ExpiryDeletesTheStagedUploadAnd(t *testing.T) {
	// - Setup: nexusd T0 stack with F-CLK for staging `Now` and `afterFunc`; one trusted key.
	// - Steps:
	//   1. Upload.
	//   2. `clk.Advance(1h)`.
	//   3. List `staging/`.
	//   4. POST update `{uploadId}`.
	// - Expected:
	//   - Before the advance, the dir exists.
	//   - After it, the dir is gone.
	//   - The update gets 422 `upload-not-found` with `uploadId`, and no op is created.
	t.Skip("TC-SM-73: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-74 (WI 5.1; FR-6, NFR-4): The startup sweep removes invalid, expired and stray entries and re-arms live uploads
func TestTCSM74_TheStartupSweepRemovesInvalidExpired(t *testing.T) {
	// - Setup: `<state>/staging` pre-populated with:
	//   - `a/` with no `meta.json`;
	//   - `b/` with corrupt `meta.json`;
	//   - `c/` expired 1 s ago;
	//   - `d/` live, expiring in 10 m, holding a stray `probe-x1/` and a `release.tar.gz.partial`.
	// - Steps:
	//   1. Construct the provider and call `UseDaemon` (which sweeps).
	//   2. `clk.Advance(10m)`.
	// - Expected:
	//   - After step 1, only `d/` remains, holding only `release.tar.gz` and `meta.json`.
	//   - After step 2, `d/` is gone.
	t.Skip("TC-SM-74: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-75 (WI 5.1; FR-16, FR-6): The upload audit carries requested and stored records with facts and no key material
func TestTCSM75_TheUploadAuditCarriesRequestedAnd(t *testing.T) {
	// - Setup: nexusd T0 stack with trusted key K configured.
	// - Steps:
	//   1. Upload.
	//   2. Read `audit.jsonl`.
	// - Expected:
	//   - A `requested` record for `nexusd.uploads.add`, then `stored` with `facts == {uploadId,
	//     sha256, sizeBytes}` matching the 201 body.
	//   - Neither the audit file nor the server log contains K's base64.
	t.Skip("TC-SM-75: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-76 (WI 5.1; FR-6, CON-4): Upload IDs are server-generated, and no client-supplied name reaches the file system
func TestTCSM76_UploadIDsAreServerGeneratedAnd(t *testing.T) {
	// - Setup: nexusd T0 stack; a staging cap high enough for the test, by deleting after each upload.
	// - Steps:
	//   1. Upload 200 distinct bodies, collecting the IDs and removing each.
	//   2. Upload with `Content-Disposition: attachment; filename="../../etc/x"` and `X-Upload-Name:
	//     ../x`.
	//   3. Upload with the query `?signature=S&uploadId=../x`.
	// - Expected:
	//   - Step 1: 200 unique IDs, all matching the pattern.
	//   - Step 2: 201, and the only entries created are
	//     `staging/<serverId>/{release.tar.gz,meta.json}`. No path outside `staging/<serverId>` is
	//     touched (inspect the whole temp state tree).
	//   - Step 3: 400 `validation-failed` (unknown query parameter).
	t.Skip("TC-SM-76: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-82 (WI 5.3; FR-7, FR-9, CON-4): An admitted update creates an op, and Prepare extracts the package and writes input.json
func TestTCSM82_AnAdmittedUpdateCreatesAnOp(t *testing.T) {
	// - Setup:
	//   - nexusd T0 stack, running `version.Version` overridden to `v9.9.1` through ldflags in the
	//     test, or a package-level var seam;
	//   - trusted key K;
	//   - fixture `v9.9.2` (shell stand-in nexusd) signed by K and uploaded;
	//   - ops manager with a spy `detach.Options.Launch` that records the invocation and does not run
	//     it.
	// - Steps:
	//   1. `POST …/actions/update {uploadId}` with the `admin` token.
	//   2. Wait until the spy launch is called.
	//   3. Inspect `ops/<id>/`.
	// - Expected:
	//   - 202.
	//   - `ops/<id>/pkg/nexusd-v9.9.2-linux-<goarch>/` holds the 5 required files with normalized
	//     modes.
	//   - `input.json` has:
	//     - `{mode: "update", runningVersion: "v9.9.1", expectedVersion: "v9.9.2", noChange: false}`;
	//     - `packageDir` under `ops/<id>/pkg/`;
	//     - `installTimeout: 120s`.
	//   - The spec `Env` is exactly PATH and LC_ALL, and `KillMode` is process.
	//   - `MaxRuntime == 120s + 2*(30s+20s) + 60s`.
	//   - The `requested` audit record has facts `{uploadId, sha256, packageVersion: "v9.9.2",
	//     signature: "valid", allowDowngrade: "false"}`.
	t.Skip("TC-SM-82: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-83 (WI 5.3; FR-7, ASM-6): Updating to the running version yields a noChange input
func TestTCSM83_UpdatingToTheRunningVersionYields(t *testing.T) {
	// - Setup: as TC-SM-82, with the package version `v9.9.1` equal to the running version.
	// - Steps:
	//   1. POST update.
	// - Expected:
	//   - 202.
	//   - `input.json.noChange == true`.
	//   - It is not refused as a downgrade.
	t.Skip("TC-SM-83: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-84 (WI 5.3; FR-7, ASM-6): --allow-downgrade admits an older package
func TestTCSM84_AllowDowngradeAdmitsAnOlderPackage(t *testing.T) {
	// - Setup: running `v9.9.2`; package `v9.9.1` signed by K.
	// - Steps:
	//   1. POST update `{uploadId, allowDowngrade: true}`.
	// - Expected:
	//   - 202, and the op is created.
	//   - The audit fact `allowDowngrade == "true"`.
	//   - `input.json.expectedVersion == "v9.9.1"`.
	t.Skip("TC-SM-84: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-85 (WI 5.3; FR-6, FR-9, FR-8): Finalize deletes the staged upload idempotently and maps result.json
func TestTCSM85_FinalizeDeletesTheStagedUploadIdempotently(t *testing.T) {
	// - Setup: an admitted update op dir with `input.json`. The run writes `result.json` variants:
	//   - (a) `{changed: true, previousVersion, newVersion, rolledBack: false}`;
	//   - (b) error `update-failed` with `rolledBack: true`, `step: "install"`;
	//   - (c) error `update-rollback-failed`;
	//   - (d) no file.
	// - Steps:
	//   1. Before each `Finalize` of variants (a) to (c), plant `ops/<id>/pkg/` containing one file (a
	//     helper that stopped between `result.json` and its own removal).
	//   2. Call `Finalize` for each variant, twice for (a). The second (a) call runs with `pkg/`
	//     already gone.
	// - Expected:
	//   - (a) `UpdateResult{changed: true, previousVersion, newVersion, rolledBack: false}`.
	//   - (b) problem `update-failed` with extensions `rolledBack: true`, `step: "install"` and the
	//     versions.
	//   - (c) `update-rollback-failed`, `rolledBack: false`.
	//   - (d) `outcome-unknown`, whose `LostDetail` names `systemctl status nexusd.service`,
	//     `/usr/local/bin/nexusd -version` and `nexusd.prev`.
	//   - In every case `staging/<uploadId>` is absent afterwards, and the second Finalize does not
	//     error.
	//   - (a), (b) and (c): `ops/<id>/pkg/` is absent after `Finalize`. The second (a) call, with
	//     `pkg/` and the staged upload already absent, returns the same `UpdateResult` and no error.
	t.Skip("TC-SM-85: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-86 (WI 5.3; FR-11): The lane matrix: each of the four ops blocks the others, and other providers are unaffected
func TestTCSM86_TheLaneMatrixEachOfThe(t *testing.T) {
	// - Setup:
	//   - nexusd T0 stack plus a second test provider with a long-running op on its own lane;
	//   - F-DAEMON `Reload` blocks to hold the lane;
	//   - for the detached holders, a spy launch that never completes, so the op stays Running.
	// - Steps:
	//   1. For each holder H in {reload, restart, update, rollback}: start H, then POST each of
	//     {reload, restart, update, rollback} and the other provider's op.
	//   2. Release H, then repeat with the next holder.
	// - Expected:
	//   - All 16 nexusd pairs get 409 `conflict` with `blockingOperationId == H`.
	//   - The other provider's op gets 202 every time.
	//   - No refused request created an op.
	t.Skip("TC-SM-86: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-87 (WI 5.3; FR-16): An admitted update's audit facts carry no key material
func TestTCSM87_AnAdmittedUpdateSAuditFacts(t *testing.T) {
	// - Setup: two trusted keys K1 and K2 configured, so their base64 strings are known; an admitted
	//   update (TC-SM-82) and a refused one (bad signature).
	// - Steps:
	//   1. Read `audit.jsonl` and the captured `slog` output.
	// - Expected:
	//   - Neither K1's nor K2's base64 appears anywhere.
	//   - No PEM block appears.
	//   - The facts keys are a subset of `{uploadId, sha256, sizeBytes, packageVersion, signature,
	//     allowDowngrade}`.
	t.Skip("TC-SM-87: SKIP-only stub; un-skip when WI 5.3 lands")
}

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

// TC-SM-E3 (WI 5.1; FR-6): Re-uploading a staged upload while 2 are staged is refused 409 before the body is read
func TestTCSME3_ReUploadingAStagedUploadWhile(t *testing.T) {
	// - Setup: nexusd T0 stack with F-CLK as `Options.Now`. Upload A at virtual t0, advance 10 min,
	//   upload B. Keep each 201 response's `expiresAt` string (`spec.UploadResult.ExpiresAt`).
	// - Steps:
	//   1. Upload A's exact bytes and signature again.
	// - Expected:
	//   - 409 `conflict`.
	//   - The problem `detail` contains A's `expiresAt` exactly as A's upload response returned it (the
	//     earliest staged expiry, t0 + 1h), so the user knows when to retry. It does not contain B's
	//     `expiresAt`, which proves the earliest one is chosen.
	//   - The staging dirs are still exactly {A, B}.
	//   - The body was not read (a server-side counting wrapper shows 0 bytes).
	t.Skip("TC-SM-E3: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-E10 (WI 5.3; FR-7, ASM-6): A non-comparable running version counts as a downgrade
func TestTCSME10_ANonComparableRunningVersionCounts(t *testing.T) {
	// - Setup: running version `dev`; package `v9.9.2` signed by K.
	// - Steps:
	//   1. POST update.
	//   2. POST update with `allowDowngrade: true`.
	// - Expected:
	//   - Step 1 → 409 `version-downgrade` with `runningVersion: "dev"` and `packageVersion:
	//     "v9.9.2"`.
	//   - Step 2 → 202.
	t.Skip("TC-SM-E10: SKIP-only stub; un-skip when WI 5.3 lands")
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

// TC-SM-E16 (WI 5.1; FR-1, FR-19): A disabled nexusd provider refuses every op, including the upload
func TestTCSME16_ADisabledNexusdProviderRefusesEvery(t *testing.T) {
	// - Setup: nexusd T0 stack with `ProviderConfig{Enabled: false}`.
	// - Steps:
	//   1. Upload.
	//   2. GET daemons.
	//   3. POST validate-config.
	// - Expected:
	//   - Each gets the same disabled-provider refusal that the existing providers return (status,
	//     slug and `provider: nexusd`, compared against a docker stub disabled the same way).
	//   - Nothing is written under `staging/`.
	t.Skip("TC-SM-E16: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-E17 (WI 5.1; FR-6): Expiry boundary: an upload is found at 59:59 and gone at 60:00
func TestTCSME17_ExpiryBoundaryAnUploadIsFound(t *testing.T) {
	// - Setup: nexusd T0 stack with F-CLK; one trusted key; an upload of a validly signed same-version
	//   package.
	// - Steps:
	//   1. Advance 59m59s, then POST update with a spy launch.
	//   2. Upload again, advance 60m, then POST update.
	// - Expected:
	//   - Step 1 → 202.
	//   - Step 2 → 422 `upload-not-found`.
	t.Skip("TC-SM-E17: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-ERR35 (WI 4.3; FR-12): rollback-unavailable when the previous binary is missing or does not report a version
func TestTCSMERR35_RollbackUnavailableWhenThePreviousBinary(t *testing.T) {
	// - Setup: nexusd T0 stack with the real Runner. Prev is (a) missing, and (b) a script whose
	//   `validate-config` exits 0 and whose `-version` prints `oops`.
	// - Steps:
	//   1. POST rollback for each.
	// - Expected:
	//   - (a) 409 `rollback-unavailable` with `path == Paths.Prev`.
	//   - (b) 409 `rollback-unavailable` with a detail naming the unparsable version.
	//   - Neither creates an op.
	t.Skip("TC-SM-ERR35: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SM-ERR36 (WI 4.3; FR-12, FR-3): A previous binary that rejects the config refuses the rollback with config-invalid
func TestTCSMERR36_APreviousBinaryThatRejectsThe(t *testing.T) {
	// - Setup: Prev is a shell stand-in: `validate-config --config X` writes the captured real stderr
	//   (testdata/validate-config/unknown-key.stderr, 2 lines) and exits 1, and `-version` prints
	//   `nexusd v9.9.0`.
	// - Steps:
	//   1. POST rollback.
	// - Expected:
	//   - 422 `config-invalid` with `errors == [{message: line1}, {message: line2}]` and `configPath`.
	//   - No op is created.
	//   - Prev was invoked with exactly `validate-config --config <ConfigPath()>`.
	t.Skip("TC-SM-ERR36: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SM-ERR37 (WI 4.3; FR-12, FR-10): A rollback that fails health swaps back and reports rollback-failed
func TestTCSMERR37_ARollbackThatFailsHealthSwaps(t *testing.T) {
	// - Setup: as TC-SM-67, but the prev content B1 is `exit-at-start`. A variant also makes B2
	//   `exit-at-start` after the swap back.
	// - Steps:
	//   1. Run the helper.
	//   2. Finalize.
	//   3. Repeat with the variant.
	// - Expected:
	//   - Main run:
	//     - the files equal the originals (`Binary == B2`, `Prev == B1`, the units likewise);
	//     - `ResetFailed` is called before each `Restart`;
	//     - the result is `rollback-failed` with `rolledBack: false`, `condition` and both versions.
	//   - Variant: the detail additionally says that nexusd may be down and names the manual steps.
	t.Skip("TC-SM-ERR37: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-SM-ERR38 (WI 5.1; FR-6): A third concurrent or staged upload is refused 409
func TestTCSMERR38_AThirdConcurrentOrStagedUpload(t *testing.T) {
	// - Setup: nexusd T0 stack with F-CLK as `Options.Now`. In run A, two uploads are in flight (their
	//   bodies blocked mid-stream). In run B, two different uploads are staged at virtual t0 and t0 +
	//   10 min; keep each 201 response's `expiresAt`. In run C, the stack's audit-failure option (core
	//   `Options.Audit`, WI 5.1 `stack_test.go`) wraps the real audit log and fails the next two
	//   `requested` writes, then delegates to the real log. Run D works on the provider's staging
	//   directly (internal test package). Run E is on the T0 stack and reads the staging in-flight
	//   count after each request returns.
	// - Steps:
	//   1. Start a third upload in each of runs A and B.
	//   2. In run A, let the first blocked upload finish, advance F-CLK 10 min, let the second finish
	//     (keep both `expiresAt` values), then start a further upload.
	//   3. In run C, POST two uploads while the `requested` write fails. Then upload two different
	//     valid tarballs at virtual t0 and t0 + 10 min (keep both `expiresAt` values). Then start a
	//     fifth upload.
	//   4. In run D: (i) call `reserve()`, then call its `release` twice; (ii) with one upload stored
	//     (count 1), start two `reserve()` calls together behind a start barrier, 100 repetitions,
	//     releasing any success before the next repetition.
	//   5. In run E: a successful upload, then an upload identical to it (dedup), then a body aborted
	//     at 512 KiB; read the in-flight count after each request returns.
	// - Expected:
	//   - 409 `conflict` in both runs.
	//   - No new staging dir.
	//   - Run B: the `detail` contains the earlier upload's `expiresAt` exactly as its upload response
	//     returned it, and not the later one's.
	//   - Run A step 2: the further upload is still refused 409, because 2 are staged, and its `detail`
	//     contains the first-finished upload's `expiresAt` and not the second's.
	//   - Run A step 1 (nothing staged yet, only reservations in flight) asserts only the 409
	//     `conflict`; the detail has no staged expiry to name.
	//   - Run C: the first two uploads → 503 `audit-unavailable`, and `staging/` has no entries. Their
	//     reservations were released, so the next two uploads → 201 each.
	//   - Run C: the fifth upload → 409 `conflict`. Its `detail` contains the t0 upload's `expiresAt`
	//     and not the later one's, and does not contain `still receiving` (no reservation is in
	//     flight).
	//   - Run D (i): the in-flight count goes 0 → 1 → 0 and stays 0 after the second `release` (the
	//     `sync.Once` guard).
	//   - Run D (ii): in every repetition exactly one `reserve()` succeeds and the other returns 409
	//     `conflict`.
	//   - Run E: the in-flight count is 0 after each of the three requests; the dedup upload returns
	//     the first upload's ID.
	t.Skip("TC-SM-ERR38: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-ERR39 (WI 5.1; FR-6, NFR-4): Oversize and aborted uploads leave nothing in staging
func TestTCSMERR39_OversizeAndAbortedUploadsLeaveNothing(t *testing.T) {
	// - Setup: nexusd T0 stack with `maxUploadBytes` 1 MiB.
	// - Steps:
	//   1. Send a chunked 1 MiB + 1 body.
	//   2. Send 512 KiB of a declared 1 MiB, then close the connection.
	//   3. Use a `Receive` fault hook to fail after the rename but before `meta.json`.
	// - Expected:
	//   - Step 1 → 413.
	//   - After each step, `staging/` has no entries (no dir, no `.partial`, no `meta.json`).
	//   - The reservation is released: two subsequent uploads succeed.
	t.Skip("TC-SM-ERR39: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-ERR40 (WI 5.1; FR-6, FR-1): The upload requires nexusd:admin and working self-management
func TestTCSMERR40_TheUploadRequiresNexusdAdminAnd(t *testing.T) {
	// - Setup: nexusd T0 stack.
	// - Steps:
	//   1. Upload with no token.
	//   2. Upload with the `reader` token.
	//   3. Upload with the `admin` token while `Options.Systemd` is unavailable.
	// - Expected:
	//   - Step 1: 401.
	//   - Step 2: 403 with `requiredScope: "nexusd:admin"`.
	//   - Step 3: 503 `capability-unavailable` with the reason.
	//   - In every case nothing is written to `staging/`.
	t.Skip("TC-SM-ERR40: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SM-ERR45 (WI 5.3; FR-7, FR-16): Admission reports the first failing check in FR-7 order
func TestTCSMERR45_AdmissionReportsTheFirstFailingCheck(t *testing.T) {
	// - Setup: nexusd T0 stack, with a fixture failing several checks per row.
	// - Steps:
	//   1. POST update for each row:
	//
	//      | Row | Failing checks | Expected slug | Upload after |
	//      |---|---|---|---|
	//      | 1 | no keys, bad signature, traversal | `update-not-configured` | kept |
	//      | 2 | unknown uploadId, keys present | `upload-not-found` | n/a |
	//      | 3 | bad signature, traversal, downgrade | `update-signature-invalid` | deleted |
	//      | 4 | good signature, traversal, wrong-version binary, downgrade | `update-package-invalid`
	//        (`path`) | deleted |
	//      | 5 | good signature and layout, binary prints the wrong version, downgrade |
	//        `update-package-invalid` (`version-mismatch`) | deleted |
	//      | 6 | good everything, downgrade | `version-downgrade` | kept |
	// - Expected:
	//   - Each row returns the slug and upload state in the table.
	//   - No op is ever created.
	//   - The F-RUN exec count is 0 for rows 1–4.
	t.Skip("TC-SM-ERR45: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-ERR46 (WI 5.3; FR-7, FR-6): update-not-configured, upload-not-found and a malformed uploadId
func TestTCSMERR46_UpdateNotConfiguredUploadNotFound(t *testing.T) {
	// - Setup: nexusd T0 stack.
	// - Steps:
	//   1. With `trustedKeys: []`, POST update with any well-formed uploadId.
	//   2. With keys present, POST update with an unknown well-formed uploadId.
	//   3. POST update with `uploadId: "../../etc"`.
	// - Expected:
	//   - Step 1 → 409 `update-not-configured`.
	//   - Step 2 → 422 `upload-not-found` with `uploadId`.
	//   - Step 3 → 400 `validation-failed` (pattern), never 422.
	//   - No op in any case.
	t.Skip("TC-SM-ERR46: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-ERR47 (WI 5.3; FR-7, ASM-6): A downgrade without the flag is refused 409 with both versions
func TestTCSMERR47_ADowngradeWithoutTheFlagIs(t *testing.T) {
	// - Setup: running `v9.9.2`; package `v9.9.1` signed by K.
	// - Steps:
	//   1. POST update.
	//   2. POST update with `allowDowngrade: false` explicitly.
	// - Expected:
	//   - Both → 409 `version-downgrade` with `runningVersion: "v9.9.2"` and `packageVersion:
	//     "v9.9.1"`.
	//   - The refused audit record has facts `packageVersion`, `signature: valid` and `allowDowngrade:
	//     false`.
	//   - The upload is still staged.
	t.Skip("TC-SM-ERR47: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-ERR48 (WI 5.3; FR-1, FR-5, FR-7, FR-12): Without systemd, restart, update, rollback and the upload are refused capability-unavailable while the reads work
func TestTCSMERR48_WithoutSystemdRestartUpdateRollbackAnd(t *testing.T) {
	// - Setup: nexusd T0 stack with `Options.Systemd` returning `{Available: false, Reason:
	//   "systemd-run missing"}`.
	// - Steps:
	//   1. POST restart, update, rollback and upload.
	//   2. GET daemons.
	//   3. POST validate-config.
	//   4. POST reload.
	// - Expected:
	//   - The four mutating ops → 503 `capability-unavailable` with `reason` containing `systemd-run
	//     missing`. No op, and nothing staged.
	//   - The reads → 200.
	//   - Reload → 202, because reload is in-process and needs no systemd.
	t.Skip("TC-SM-ERR48: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-ERR49 (WI 5.3; FR-7, FR-9): An upload removed between Admit and Prepare fails the op with upload-not-found and no launch
func TestTCSMERR49_AnUploadRemovedBetweenAdmitAnd(t *testing.T) {
	// - Setup: nexusd T0 stack; a spy launch; a Prepare-entry hook (a test-only field of the provider)
	//   that runs `staging.remove(id)` after Admit returns and before Prepare reads the upload.
	// - Steps:
	//   1. POST update.
	//   2. Remove the upload in the hook.
	//   3. Poll the op.
	// - Expected:
	//   - The op is `Failed` with `upload-not-found`.
	//   - The spy launch count is 0.
	//   - `ops/<id>/pkg` does not exist.
	t.Skip("TC-SM-ERR49: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-ERR60 (WI 5.3; FR-7, FR-9): A Prepare that fails after extraction removes ops/<id>/pkg and launches nothing
func TestTCSMERR60_APrepareThatFailsAfterExtraction(t *testing.T) {
	// - Setup: nexusd T0 stack; a spy launch; running version `v9.9.1`, trusted key K, and fixture
	//   `v9.9.2` signed by K and uploaded (as TC-SM-82). A test-only write hook on the provider fails
	//   the `input.json` write with `errors.New("injected input.json write failure")` after
	//   `extractAll` has returned, so `ops/<id>/pkg/` is fully extracted when the error occurs.
	//   Background: the ops manager never calls `Finalize` after a Prepare failure
	//   (`internal/ops/manager.go:456` → `failQueued`), so only Prepare can remove `pkg/`.
	// - Steps:
	//   1. POST update with the staged `uploadId`.
	//   2. Poll the op until it is terminal.
	//   3. Assert that the hook fired after extraction: the hook records that `ops/<id>/pkg/` existed
	//     and was non-empty when it ran.
	// - Expected:
	//   - The op is `Failed` (from Queued), and no `Running` state was recorded.
	//   - The spy launch count is 0.
	//   - `ops/<id>/pkg` does not exist.
	//   - Step 3 holds, which proves the removal ran on the after-extraction path and not because
	//     extraction never happened (contrast TC-SM-ERR49, which fails before extraction).
	//   - `Finalize` was not called (a spy on the provider's `Finalize` counts 0), so the removal is
	//     Prepare's own.
	t.Skip("TC-SM-ERR60: SKIP-only stub; un-skip when WI 5.3 lands")
}

// TC-SM-ERR50 (WI 5.3; CON-4, FR-7): Update input with path, command or url fields is refused 400
func TestTCSMERR50_UpdateInputWithPathCommandOr(t *testing.T) {
	// - Setup: nexusd T0 stack.
	// - Steps:
	//   1. POST update with `{uploadId: valid, path: "/tmp/x"}`.
	//   2. POST with `{…, command: "id"}`.
	//   3. POST with `{…, url: "http://x"}`.
	// - Expected:
	//   - Each → 400 `validation-failed`, naming the unknown field.
	//   - No staging lookup is made, and no op is created.
	t.Skip("TC-SM-ERR50: SKIP-only stub; un-skip when WI 5.3 lands")
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
