// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package nexusd

import "testing"

// TC-SM-51 (WI 3.5; FR-2, FR-15): get over REST returns the daemon fields and reflects a second state
func TestTCSM51_GetOverRESTReturnsTheDaemon(t *testing.T) {
	// - Setup:
	//   - nexusd T0 stack, with F-DAEMON `StartedAt` at t0 and `ConfigPath` set to the fixture;
	//   - `Options.Now` = t0+90s;
	//   - `Executable` returns `/x/nexusd`;
	//   - `Paths.Prev` absent;
	//   - config with `trustedKeys: []`.
	// - Steps:
	//   1. `GET /v1/providers/nexusd/daemons` with the `reader` token.
	//   2. Write `Paths.Prev` as a shell stand-in printing `nexusd v9.9.0`, set `trustedKeys` to one
	//     key, and apply through `Configure` (the reload path).
	//   3. GET again.
	// - Expected:
	//   - Step 1:
	//     - `items[0].id == "/providers/nexusd/daemons/nexusd"`;
	//     - `properties`: `version == version.Version`, `startedAt == t0`, `uptimeSeconds == 90`,
	//       `configPath` equals the fixture path, `executablePath == "/x/nexusd"`;
	//     - `previous == {present: false}`, `updateConfigured == false`, `selfManagement.available ==
	//       true`.
	//   - Step 3: `previous == {present: true, path: Paths.Prev, version: "v9.9.0"}` and
	//     `updateConfigured == true`. This is not a stale snapshot.
	t.Skip("TC-SM-51: SKIP-only stub; un-skip when WI 3.5 lands")
}

// TC-SM-52 (WI 3.5; FR-2, NFR-2): The prev-version lookup is cached by file identity and reports unknown after 5 s
func TestTCSM52_ThePrevVersionLookupIsCached(t *testing.T) {
	// - Setup: F-RUN counting execs of `Paths.Prev`, returning `nexusd v9.9.0\n`; the nexusd T0 stack.
	// - Steps:
	//   1. GET twice.
	//   2. `os.Chtimes(Prev, now+1s)`, then GET.
	//   3. Make F-RUN block until `ctx.Done()`, rewrite Prev (new size), and GET with an F-CLK-driven
	//     5 s timeout.
	// - Expected:
	//   - Step 1 makes exactly 1 exec.
	//   - Step 2 makes exactly 1 more.
	//   - Step 3 returns `previous.version == "unknown"` and `present: true`, with the call returning
	//     within the injected 5 s and no error status.
	t.Skip("TC-SM-52: SKIP-only stub; un-skip when WI 3.5 lands")
}

// TC-SM-53 (WI 3.5; FR-3): validate-config over REST returns valid:false with status 200 for an invalid file
func TestTCSM53_ValidateConfigOverRESTReturnsValid(t *testing.T) {
	// - Setup: nexusd T0 stack; F-DAEMON `ConfigPath` points to `valid.yaml`, then to
	//   `unknown-key.yaml`.
	// - Steps:
	//   1. `POST /v1/providers/nexusd/actions/validate-config` with the `reader` token.
	//   2. Swap the path to `unknown-key.yaml`, then POST.
	//   3. POST with a query `?path=/etc/shadow`.
	// - Expected:
	//   - Step 1: 200 with `{host, valid: true, configPath, errors: []}`.
	//   - Step 2: 200 with `valid: false` and `errors[0] == {file: <path>, line: N, column: M, path:
	//     "bogus", message: <the CheckConfig message>}`, where N and M come from `CheckConfig` on the
	//     same file.
	//   - Location fields that are 0 are absent from the JSON.
	//   - Step 3: 400 `validation-failed`, because the op takes no input (NSM FR-25).
	t.Skip("TC-SM-53: SKIP-only stub; un-skip when WI 3.5 lands")
}

// TC-SM-54 (WI 3.5; FR-4): reload over REST applies a valid config and reports restartRequired
func TestTCSM54_ReloadOverRESTAppliesAValid(t *testing.T) {
	// - Setup: nexusd T0 stack; F-DAEMON `Reload` returns `{RestartRequired: ["limits"]}`; valid
	//   config.
	// - Steps:
	//   1. `POST …/actions/reload` with the `admin` token.
	//   2. Poll the op.
	// - Expected:
	//   - 202 with an op.
	//   - The op ends `Succeeded` with `result == {changed: true, applied: true, restartRequired:
	//     ["limits"]}`.
	//   - F-DAEMON saw exactly 1 `Reload`.
	//   - The audit log has `requested` and then the outcome.
	t.Skip("TC-SM-54: SKIP-only stub; un-skip when WI 3.5 lands")
}

// TC-SM-55 (WI 3.5; FR-1): Preflight is always enabled with selfManagement details, and reads work without systemd
func TestTCSM55_PreflightIsAlwaysEnabledWithSelfManagement(t *testing.T) {
	// - Setup: nexusd T0 stack with `Options.Systemd` returning available in run A and `{Available:
	//   false, Reason: "systemd not running (/run/systemd/system missing)"}` in run B.
	// - Steps:
	//   1. `GET /v1/capabilities`.
	//   2. GET `daemons`.
	//   3. POST `validate-config`.
	// - Expected:
	//   - Run A: capabilities show nexusd `enabled` with `details.selfManagement == "available"`.
	//   - Run B: nexusd is still `enabled`, with `details.selfManagement == "unavailable"` and
	//     `details.reason` containing `/run/systemd/system`.
	//   - Run B: `get` returns 200 with `selfManagement == {available: false, reason}`, and
	//     `validate-config` returns 200.
	t.Skip("TC-SM-55: SKIP-only stub; un-skip when WI 3.5 lands")
}

// TC-SM-56 (WI 3.5; FR-11): A reload with an Idempotency-Key replays the same operation
func TestTCSM56_AReloadWithAnIdempotencyKey(t *testing.T) {
	// - Setup: nexusd T0 stack.
	// - Steps:
	//   1. Send reload with `Idempotency-Key: r1` twice, with the same empty body.
	// - Expected:
	//   - Both responses carry the same op ID.
	//   - F-DAEMON saw exactly 1 `Reload`.
	//   - There is exactly one `requested` audit record.
	t.Skip("TC-SM-56: SKIP-only stub; un-skip when WI 3.5 lands")
}

// TC-SM-57 (WI 4.1; FR-10): waitHealthy passes when all four FR-10 conditions hold
func TestTCSM57_WaitHealthyPassesWhenAllFourFR(t *testing.T) {
	// - Setup: F-SYS with the unit active; MainPID 200 running `#!fake nexusd v9.9.2 ok`; F-CLK; target
	//   `{Timeout: 30s, Stability: 5s, Poll: 1s}`.
	// - Steps:
	//   1. Call `waitHealthy(ctx, sys, target, "v9.9.2", excludePID 100)`, assigning the result to `var
	//     version string; var err error` (an `error`-typed variable, as the helper modes do).
	// - Expected:
	//   - It returns version `v9.9.2`, and `err == nil` holds on the `error`-typed variable. This is
	//     the typed-nil guard (`go/typed-nil-interface`): `waitHealthy` returns `error`, never a nil
	//     `*conditionError` wrapped in a non-nil interface.
	//   - The virtual time elapsed is 5 s to 6 s (the stability window).
	//   - F-SYS recorded at least 6 `Show` calls, at most 1 s apart.
	//   - `ExeVersion("/proc/200/exe")` was called once, because it is cached per PID.
	t.Skip("TC-SM-57: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-58 (WI 4.1; FR-10): A PID change resets the stability window
func TestTCSM58_APIDChangeResetsTheStability(t *testing.T) {
	// - Setup: F-SYS: PID 200 `crash-after=3s`; after the auto-restart, PID 201 `ok`. F-CLK.
	// - Steps:
	//   1. Call `waitHealthy(expected "v9.9.2", exclude 100)` with a timeout of 30 s.
	// - Expected:
	//   - Success with PID 201.
	//   - The success time is at least 5 s after PID 201 first satisfied conditions 1–3.
	//   - No success is reported while PID 200 was current.
	t.Skip("TC-SM-58: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-59 (WI 4.1; FR-10): targetFrom maps listen and TLS settings to the health URL and pin source
func TestTCSM59_TargetFromMapsListenAndTLSSettings(t *testing.T) {
	// - Setup: table of `config.Config` values loaded from YAML strings.
	// - Steps:
	//   1. Call `targetFrom(cfg, "/var/lib/nexus")` for each row.
	// - Expected:
	//
	//   | `listen[0]` | TLS | URL | Cert file |
	//   |---|---|---|---|
	//   | `0.0.0.0:8443` | auto | `https://127.0.0.1:8443` | `/var/lib/nexus/tls/cert.pem` |
	//   | `:8443` | auto | `https://127.0.0.1:8443` | `/var/lib/nexus/tls/cert.pem` |
	//   | `[::]:8443` | auto | `https://[::1]:8443` | `/var/lib/nexus/tls/cert.pem` |
	//   | `192.168.1.2:8443` | files | `https://192.168.1.2:8443` | `tls.certFile` |
	//   | `127.0.0.1:8080` | off | `http://127.0.0.1:8080` | none |
	//
	//   Only the first listen entry is used when there are several.
	t.Skip("TC-SM-59: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-60 (WI 4.1; FR-8, FR-9, FR-7): The helper framework reads input.json, writes result.json atomically and logs timestamped steps
func TestTCSM60_TheHelperFrameworkReadsInputJson(t *testing.T) {
	// - Setup: a temp op dir with `input.json {mode: "noop-test"}` (a test-only mode registered by the
	//   test through the helper seam) and F-SYS.
	// - Steps:
	//   1. Run the helper seam with `--dir D`.
	//   2. Inspect `D/result.json`.
	//   3. Inspect stdout.
	//   4. Inject a fault in `writeResult` after the temp write and before the rename.
	// - Expected:
	//   - Exit 0.
	//   - `result.json` is complete JSON, mode `0600`, and no temp file remains.
	//   - Each step line on stdout starts with an RFC 3339 timestamp.
	//   - Step 4: `result.json` is absent rather than partial, and the temp file is removed.
	t.Skip("TC-SM-60: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-62 (WI 4.1; FR-7, CON-2, NFR-6): The real system implementation runs install.sh with a fixed argv, env and cwd, and parses systemctl and -version output
func TestTCSM62_TheRealSystemImplementationRunsInstall(t *testing.T) {
	// - Setup:
	//   - a temp package dir with `install.sh` that prints `$0 $# $(pwd)` and `env | sort`, writes 5
	//     lines to stderr and exits 0;
	//   - a variant that sleeps 10 s;
	//   - captured `systemctl show` fixtures (test-strategy.md, Mocking Strategy);
	//   - a shell stand-in binary printing `nexusd v9.9.2`.
	// - Steps:
	//   1. Call real `RunInstall(ctx, dir/install.sh, dir, &buf)`, then repeat with a 300 ms timeout
	//     on the sleeping script.
	//   2. Parse each `systemctl show` fixture.
	//   3. Call `ExeVersion(stand-in)`.
	//   4. Call `ExeVersion` on a stand-in printing `garbage`.
	// - Expected:
	//   - Step 1:
	//     - argv is `/bin/bash <dir>/install.sh` with no other arguments, and cwd is `dir`;
	//     - the env is exactly `LC_ALL=C` and
	//       `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`, plus the variables
	//       bash sets itself (`PWD`, `SHLVL`, `_`);
	//     - stdout and stderr are streamed into `buf`;
	//     - the sleeping variant is killed at about 300 ms with a timeout error.
	//   - Step 2 yields ActiveState, SubState, Result, MainPID and NRestarts matching the fixture
	//     text.
	//   - Step 3 returns `v9.9.2`.
	//   - Step 4 returns a parse error.
	t.Skip("TC-SM-62: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-63 (WI 4.1; NFR-6): The fake system honours its fidelity contract
func TestTCSM63_TheFakeSystemHonoursItsFidelity(t *testing.T) {
	// - Setup: F-SYS and F-CLK.
	// - Steps:
	//   1. `Restart()` with the unit active and an `ok` binary.
	//   2. Run `RunInstall` with the unit inactive.
	//   3. Use an `exit-at-start` binary, call `Restart()`, then advance 10 s.
	//   4. Call `Restart()`.
	//   5. `ResetFailed()`, then `Restart()`.
	//   6. Read `ExeVersion("/proc/<old>/exe")` after a rename over `Paths.Binary`.
	//   7. Call `clk.Advance` with no waiter.
	// - Expected:
	//   - Step 1: returns at once with the same MainPID; the next tick gives a new PID in
	//     `activating`, then `active` after 300 ms; `starts == 1`.
	//   - Step 2: `starts == 0` (it restarts only an active service).
	//   - Step 3: `Result == "start-limit-hit"`, `ActiveState == "failed"`, `NRestarts == 4`, and
	//     `starts` counts only the explicit start.
	//   - Step 4 returns `start request repeated too quickly`.
	//   - Step 5 succeeds.
	//   - Step 6 returns the old image's version.
	//   - Step 7 panics with `advance with no waiter`.
	t.Skip("TC-SM-63: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-65 (WI 4.2; FR-5, FR-9, CON-2): The restart pipeline succeeds and reports the running version
func TestTCSM65_TheRestartPipelineSucceedsAndReports(t *testing.T) {
	// - Setup:
	//   - provider pipeline harness: `Options.Systemd` available and `InService` ok;
	//   - F-DAEMON with a valid config (`listen 127.0.0.1:8443`, TLS auto) and `healthTimeout: 30s`;
	//   - F-SYS with PID 100 `ok` `v9.9.1`;
	//   - F-CLK;
	//   - a temp ops dir.
	// - Steps:
	//   1. `Admit`.
	//   2. `Prepare`.
	//   3. Read `input.json` and the returned `detach.Spec`.
	//   4. Run the helper seam.
	//   5. `Finalize(detach.Exit{Code: 0})`.
	// - Expected:
	//   - Admit returns nil.
	//   - `input.json` has `mode: "restart"` and a health target matching TC-SM-59's mapping.
	//   - The spec:
	//     - `Argv == [Options.Executable(), "service-helper", "--dir", D]`;
	//     - `KillMode == KillProcess`;
	//     - `MaxRuntime == 30s + 5s + 75s` (computed from the configured `healthTimeout` with the A-8
	//       formula).
	//   - F-SYS: exactly 1 start, by `Restart` with `--no-block`.
	//   - The result is `RestartResult{changed: true, version: "v9.9.1"}`.
	t.Skip("TC-SM-65: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-SM-66 (WI 4.2; FR-11): restart while a reload holds the lane gets 409 naming the blocking op
func TestTCSM66_RestartWhileAReloadHoldsThe(t *testing.T) {
	// - Setup: nexusd T0 stack; F-DAEMON `Reload` blocks until released.
	// - Steps:
	//   1. POST reload, giving op R in Running.
	//   2. POST restart.
	//   3. `POST /v1/operations/R/cancel`.
	//   4. Release R.
	// - Expected:
	//   - Step 2 → 409 `conflict` with `blockingOperationId == R`, no op created, and a refused audit
	//     record.
	//   - Step 3 → 409 `not-cancelable` (`CancelQueuedOnly`).
	//   - After release, R is `Succeeded`.
	t.Skip("TC-SM-66: SKIP-only stub; un-skip when WI 4.2 lands")
}

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

// TC-SM-77 (WI 5.2; FR-7, FR-15): Real GNU tar and bsdtar release fixtures pass signature verification and scan
func TestTCSM77_RealGNUTarAndBsdtarRelease(t *testing.T) {
	// - Setup: committed `testdata/pkg/gnu-nexusd-v9.9.2-linux-<goarch>.tar.gz` and `bsd-…`, signed
	//   in-test with `seedTrusted`.
	// - Steps:
	//   1. Call `verifySignature(path, [K], sigB64)`.
	//   2. `scan(runtime.GOARCH)` for each fixture.
	// - Expected:
	//   - Both succeed.
	//   - The manifest is `{top: "nexusd-v9.9.2-linux-<goarch>", version: "v9.9.2"}`.
	//   - The re-hash at EOF equals the verified digest.
	t.Skip("TC-SM-77: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-78 (WI 5.2; NFR-5, FR-7): Nothing is written or executed before the signature verifies
func TestTCSM78_NothingIsWrittenOrExecutedBefore(t *testing.T) {
	// - Setup: F-FS spy on the staging root; F-RUN counting execs; a valid tarball signed by an
	//   untrusted key; a second tarball that is malicious and also badly signed.
	// - Steps:
	//   1. Run admission steps 3–5 (as `admitUpdate` calls them) on each upload.
	// - Expected:
	//   - For both: `update-signature-invalid`.
	//   - Spy write count is 0 except the deletion of the upload dir, which the spy allows only as
	//     `RemoveAll(staging/<id>)`.
	//   - Exec count is 0, and no `probe-*` dir is ever created.
	t.Skip("TC-SM-78: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-79 (WI 5.2; NFR-5): verifiedArchive can be constructed only by verifySignature
func TestTCSM79_VerifiedArchiveCanBeConstructedOnlyBy(t *testing.T) {
	// - Setup: parse every non-test `.go` file in `internal/providers/nexusd` with `go/parser`.
	// - Steps:
	//   1. Find the composite literals and `new()` calls of type `verifiedArchive`.
	//   2. Find the methods `scan`, `extractFile` and `extractAll`.
	// - Expected:
	//   - Exactly one construction site, inside `func verifySignature`.
	//   - `verifiedArchive`'s fields are unexported.
	//   - `scan`, `extractFile` and `extractAll` are methods with the receiver `*verifiedArchive` (or
	//     the value type), and no package-level function takes a path and extracts.
	t.Skip("TC-SM-79: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-80 (WI 5.2; FR-7, NFR-5): extractAll confines writes to dst and normalizes modes
func TestTCSM80_ExtractAllConfinesWritesToDstAnd(t *testing.T) {
	// - Setup:
	//   - a signed tarball whose entries have modes `0755 nexusd`, `0700 install.sh`, `04755
	//     uninstall.sh` (setuid), `0666 nexusd.default.yaml`, `01777 docs/` (sticky dir) and `0640
	//     nexusd.service`;
	//   - for run 2, `dst/<top>/docs` pre-created as a symlink to `/tmp/outside`.
	// - Steps:
	//   1. `extractAll(dst)`.
	//   2. Run 2: `extractAll` into the poisoned dst.
	// - Expected:
	//   - Run 1: the file modes are 0755, 0755, 0755 (setuid dropped), 0644 and 0644; directories are
	//     0700; no file exists outside `dst`.
	//   - Run 2: an error (`O_NOFOLLOW` or a parent-is-symlink check); nothing is written under
	//     `/tmp/outside`; `dst` is removed.
	t.Skip("TC-SM-80: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-81 (WI 5.2; FR-7): probeVersion accepts a binary that prints the matching version
func TestTCSM81_ProbeVersionAcceptsABinaryThatPrints(t *testing.T) {
	// - Setup: a shell stand-in `nexusd` printing `nexusd v9.9.2`, extracted by `extractFile` from a
	//   signed fixture; the real Runner.
	// - Steps:
	//   1. Call `probeVersion(ctx, runner, bin, "v9.9.2")`.
	// - Expected:
	//   - nil.
	//   - The binary was executed with exactly the argument `-version`.
	t.Skip("TC-SM-81: SKIP-only stub; un-skip when WI 5.2 lands")
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
	//   1. Call `Finalize` for each variant, twice for (a).
	// - Expected:
	//   - (a) `UpdateResult{changed: true, previousVersion, newVersion, rolledBack: false}`.
	//   - (b) problem `update-failed` with extensions `rolledBack: true`, `step: "install"` and the
	//     versions.
	//   - (c) `update-rollback-failed`, `rolledBack: false`.
	//   - (d) `outcome-unknown`, whose `LostDetail` names `systemctl status nexusd.service`,
	//     `/usr/local/bin/nexusd -version` and `nexusd.prev`.
	//   - In every case `staging/<uploadId>` is absent afterwards, and the second Finalize does not
	//     error.
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

// TC-SM-E4 (WI 5.2; FR-7, NFR-5): Exactly 1024 entries and exactly 256 MiB are accepted, and one more of either is refused
func TestTCSME4_Exactly1024EntriesAndExactly256(t *testing.T) {
	// - Setup: in-test generated, signed tarballs:
	//   - (a) the top dir, the 5 required files and filler files totalling 1024 entries;
	//   - (b) 1025 entries;
	//   - (c) the required files plus one zero-filled file making the total exactly 268435456 bytes;
	//   - (d) 268435457 bytes.
	// - Steps:
	//   1. Run `verifySignature` and then `scan` on each.
	// - Expected:
	//   - (a) and (c) succeed.
	//   - (b) fails with `check: entries`.
	//   - (d) fails with `check: size`. Scan stops at the limit without reading the rest: bytes read ≤
	//     limit + 1 block.
	t.Skip("TC-SM-E4: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-E5 (WI 5.2; FR-7, NFR-5): Duplicate entry names are refused at scan, and extraction refuses them as a backstop
func TestTCSME5_DuplicateEntryNamesNeverWriteOutside(t *testing.T) {
	// - Setup: in-test generated tarballs, each an otherwise valid package (correct top dir, all 5
	//   required files) validly signed by K, so the duplicate is the only defect. The second occurrence's
	//   content is the marker `SECOND-COPY-5e1d`:
	//   - (i) `top/nexusd` twice, file/file, different contents;
	//   - (ii) `top/nexusd` and then `top/./nexusd` (equal after `path.Clean`);
	//   - (iii) a file `top/docs` and then a directory `top/docs/` (file/dir);
	//   - (iv) a directory `top/docs/` twice (dir/dir).
	// - Steps:
	//   1. For each, `verifySignature`, then `scan(runtime.GOARCH)`.
	//   2. For (i), (ii) and (iii), call `extractAll(dst)` directly on the same `verifiedArchive`
	//     (without the scan result), with `dst` a fresh path under `t.TempDir()`.
	// - Expected:
	//   - Step 1: each fails `update-package-invalid` with `check: duplicate-entry`, and scan writes
	//     nothing.
	//   - Step 2: each `extractAll` fails at the second entry; no file anywhere under the temp root
	//     contains `SECOND-COPY-5e1d`; `dst` does not exist afterwards; and nothing was created outside
	//     `dst`.
	t.Skip("TC-SM-E5: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-E6 (WI 5.2; FR-7, NFR-5): Archive header encodings: a PAX path override, a GNU long name and implicit directories
func TestTCSME6_ArchiveHeaderEncodingsAPAXPath(t *testing.T) {
	// - Setup: signed tarballs:
	//   - (a) a PAX record `path=top/../../etc/x` on an innocuous header name;
	//   - (b) a GNU long-name entry whose real name is `top/` followed by 200 `a`s and then
	//     `/../../x`;
	//   - (c) a valid package with no explicit directory entries (files only).
	// - Steps:
	//   1. Run `scan` on each.
	// - Expected:
	//   - (a) and (b) fail with `check: path`. The name `archive/tar` resolves after applying PAX and
	//     GNU records is the one validated.
	//   - (c) succeeds.
	t.Skip("TC-SM-E6: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-E7 (WI 3.5; FR-2): A prev binary that is not executable or prints garbage reports previous.version unknown
func TestTCSME7_APrevBinaryThatIsNot(t *testing.T) {
	// - Setup: nexusd T0 stack with the real Runner. `Paths.Prev` is (a) mode 0644 and (b) a script
	//   printing `hello`.
	// - Steps:
	//   1. GET daemons for each.
	// - Expected:
	//   - 200 with `previous == {present: true, path, version: "unknown"}` in both.
	//   - No error status.
	t.Skip("TC-SM-E7: SKIP-only stub; un-skip when WI 3.5 lands")
}

// TC-SM-E8 (WI 4.1; FR-10, FR-7): The excluded PID never passes condition 2, even while it still serves
func TestTCSME8_TheExcludedPIDNeverPassesCondition(t *testing.T) {
	// - Setup: F-SYS with PID 100 active and healthy, reporting the expected version, and no restart
	//   pending. F-CLK. Timeout 10 s.
	// - Steps:
	//   1. Call `waitHealthy(expected v, exclude 100)`.
	// - Expected:
	//   - It fails at the deadline with condition 2.
	//   - The reason names `MainPID 100 is the pre-restart process`.
	t.Skip("TC-SM-E8: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-E9 (WI 4.1; FR-10): The stability window can extend up to 5 s past healthTimeout, and no further
func TestTCSME9_TheStabilityWindowCanExtendUp(t *testing.T) {
	// - Setup: F-SYS and F-CLK; healthTimeout 30 s. The new PID becomes healthy at (a) t=29 s and (b)
	//   t=30.5 s.
	// - Steps:
	//   1. Run `waitHealthy` for each.
	// - Expected:
	//   - (a) succeeds at about t=34 s.
	//   - (b) fails, because conditions 1–3 did not hold before healthTimeout. The reason names the
	//     last failing condition.
	t.Skip("TC-SM-E9: SKIP-only stub; un-skip when WI 4.1 lands")
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

// TC-SM-E14 (WI 3.5; FR-1): Not running as nexusd.service makes self-management unavailable with a reason
func TestTCSME14_NotRunningAsNexusdServiceMakes(t *testing.T) {
	// - Setup: nexusd T0 stack with systemd available and `Options.InService` returning
	//   `errors.New("not running in nexusd.service (cgroup /user.slice/x.scope)")`.
	// - Steps:
	//   1. GET daemons.
	//   2. POST restart.
	// - Expected:
	//   - `selfManagement == {available: false, reason}`, where the reason contains `cgroup`.
	//   - Restart → 503 `capability-unavailable` with the same reason, and no op.
	t.Skip("TC-SM-E14: SKIP-only stub; un-skip when WI 3.5 lands")
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

// TC-SM-ERR27 (WI 3.5; FR-4, FR-3, FR-16): Reload with an invalid file is refused config-invalid with no op
func TestTCSMERR27_ReloadWithAnInvalidFileIs(t *testing.T) {
	// - Setup: nexusd T0 stack with F-DAEMON `ConfigPath` set to `unknown-key.yaml`.
	// - Steps:
	//   1. POST reload with the `admin` token.
	// - Expected:
	//   - 422 `config-invalid` with `configPath` and an `errors` array equal to `CheckConfig`'s
	//     mapping.
	//   - `GET /v1/operations` has no nexusd op.
	//   - F-DAEMON `Reload` 0.
	//   - The audit has a `refused` record with slug `config-invalid`.
	t.Skip("TC-SM-ERR27: SKIP-only stub; un-skip when WI 3.5 lands")
}

// TC-SM-ERR28 (WI 3.5; FR-4): A provider refusal during apply fails the op with config-apply-failed
func TestTCSMERR28_AProviderRefusalDuringApplyFails(t *testing.T) {
	// - Setup: F-DAEMON `Reload` returns `&provider.ReloadError{Stage: "apply", Err:
	//   errors.New("docker: socket missing")}`.
	// - Steps:
	//   1. POST reload.
	//   2. Poll the op.
	// - Expected: the op is `Failed` with `error.type` ending `config-apply-failed` and a detail
	//   containing `docker: socket missing`.
	t.Skip("TC-SM-ERR28: SKIP-only stub; un-skip when WI 3.5 lands")
}

// TC-SM-ERR29 (WI 4.1; FR-10): Each FR-10 condition failing alone is named in the reason
func TestTCSMERR29_EachFR10ConditionFailingAlone(t *testing.T) {
	// - Setup: F-SYS and F-CLK, timeout 10 s, with one scenario per condition:
	//   - (1) `hang-ready` (stays `activating`);
	//   - (2) the new PID reports `v9.9.8` while `v9.9.5` is expected;
	//   - (3) `no-healthz` (connection refused);
	//   - (4) `crash-after=3s` with auto-restarts.
	// - Steps:
	//   1. Run `waitHealthy` for each scenario.
	// - Expected:
	//   - Each returns a non-nil `error` from which `errors.As(err, &ce)` (with `var ce
	//     *conditionError`) extracts a condition whose number is 1, 2, 3 or 4 respectively.
	//   - Details:
	//     - (1) contains `ActiveState=activating`;
	//     - (2) contains `v9.9.8` and `v9.9.5`;
	//     - (3) contains the URL;
	//     - (4) mentions the PID or NRestarts change.
	t.Skip("TC-SM-ERR29: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-ERR30 (WI 4.1; FR-10, NFR-5): A TLS pin mismatch on healthz counts as condition 3 failing, never skipped
func TestTCSMERR30_ATLSPinMismatchOnHealthz(t *testing.T) {
	// - Setup:
	//   - an `httptest` TLS server answering `/healthz` 200;
	//   - `cert.pem` on disk holding a different certificate;
	//   - F-SYS for conditions 1, 2 and 4, with `Healthz` delegated to the real implementation;
	//   - F-CLK.
	// - Steps:
	//   1. Run `waitHealthy`.
	//   2. Swap `cert.pem` to the server's real certificate mid-run, then continue.
	// - Expected:
	//   - Before the swap, condition 3 fails, and the detail mentions a fingerprint mismatch.
	//   - After the swap, success follows within one poll plus the stability window, which proves the
	//     cert is re-read on each poll.
	//   - The code never builds a `tls.Config` with `InsecureSkipVerify` without `VerifyConnection`
	//     (static check).
	t.Skip("TC-SM-ERR30: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-ERR31 (WI 4.1; FR-8, CON-2): The helper refuses a missing input, an existing result and a bad --dir
func TestTCSMERR31_TheHelperRefusesAMissingInput(t *testing.T) {
	// - Setup: F-SYS.
	// - Steps:
	//   1. Run with no `--dir`.
	//   2. Run with a dir that has no `input.json`.
	//   3. Run with a corrupt `input.json`.
	//   4. Run with an existing `result.json`.
	// - Expected:
	//   - Each exits 1 with a specific stderr message.
	//   - Zero F-SYS calls are made.
	//   - No `result.json` is created, and an existing one is unchanged.
	t.Skip("TC-SM-ERR31: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SM-ERR32 (WI 4.2; FR-5, FR-10): A crash-looping restart fails with restart-unhealthy and no rollback
func TestTCSMERR32_ACrashLoopingRestartFailsWith(t *testing.T) {
	// - Setup: as TC-SM-65; the binary after the restart is `crash-after=3s`.
	// - Steps:
	//   1. Admit.
	//   2. Prepare.
	//   3. Run the helper.
	//   4. Finalize.
	// - Expected:
	//   - Problem `restart-unhealthy` with `condition` 4 (or 1 once the start limit is hit) and
	//     `version` empty or the last observed version.
	//   - F-SYS shows no DaemonReload, no file change and no second explicit start.
	//   - The helper exit is 0.
	t.Skip("TC-SM-ERR32: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-SM-ERR33 (WI 4.2; FR-5): restart is refused with config-invalid when the on-disk config is invalid
func TestTCSMERR33_RestartIsRefusedWithConfigInvalid(t *testing.T) {
	// - Setup: nexusd T0 stack; `ConfigPath` set to `bad-listen.yaml`.
	// - Steps:
	//   1. POST restart.
	// - Expected:
	//   - 422 `config-invalid` with the errors.
	//   - No op is created, and F-SYS has zero calls.
	t.Skip("TC-SM-ERR33: SKIP-only stub; un-skip when WI 4.2 lands")
}

// TC-SM-ERR34 (WI 4.2; FR-9, FR-5): A restart Finalize without result.json is outcome-unknown with recovery guidance
func TestTCSMERR34_ARestartFinalizeWithoutResultJson(t *testing.T) {
	// - Setup: an op dir with `input.json` and no `result.json`.
	// - Steps:
	//   1. `Finalize(detach.Exit{Code: 0})`.
	//   2. `Finalize(detach.Exit{Lost: true})`.
	// - Expected:
	//   - Both are `outcome-unknown`.
	//   - The `LostDetail` text contains `systemctl status nexusd.service`, `/usr/local/bin/nexusd
	//     -version` and `nexusd.prev`.
	t.Skip("TC-SM-ERR34: SKIP-only stub; un-skip when WI 4.2 lands")
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
	//   bodies blocked mid-stream). In run B, two different uploads are staged at virtual t0 and t0 + 10
	//   min; keep each 201 response's `expiresAt`.
	// - Steps:
	//   1. Start a third upload in each run.
	//   2. In run A, let the first blocked upload finish, advance F-CLK 10 min, let the second finish
	//     (keep both `expiresAt` values), then start a further upload.
	// - Expected:
	//   - 409 `conflict` in both runs.
	//   - No new staging dir.
	//   - Run B: the `detail` contains the earlier upload's `expiresAt` exactly as its upload response
	//     returned it, and not the later one's.
	//   - Run A step 2: the further upload is still refused 409, because 2 are staged, and its `detail`
	//     contains the first-finished upload's `expiresAt` and not the second's.
	//   - Run A step 1 (nothing staged yet, only reservations in flight) asserts only the 409
	//     `conflict`; the detail has no staged expiry to name.
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

// TC-SM-ERR41 (WI 5.2; FR-7, NFR-5, FR-15): Signature failures are refused before any read of the archive contents
func TestTCSMERR41_SignatureFailuresAreRefusedBeforeAny(t *testing.T) {
	// - Setup: a valid fixture; trusted key K.
	// - Steps:
	//   1. Call `verifySignature` on each row:
	//      - a signature by K2;
	//      - the fixture with one byte flipped at the middle;
	//      - the fixture with one byte appended;
	//      - a truncated signature (60 bytes, re-encoded);
	//      - keys `[]`;
	//      - a signature over a different tarball;
	//      - a signature without the context;
	//      - signature base64 with a bad character;
	//      - an empty signature.
	// - Expected:
	//   - Each returns an error with `check: signature` and a nil `*verifiedArchive`.
	//   - The gzip reader was never constructed (spy on the gzip constructor seam, or assert no gzip
	//     header read through F-FS).
	t.Skip("TC-SM-ERR41: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-ERR42 (WI 5.2; FR-7, NFR-5): Malicious and malformed archives are refused with the right check
func TestTCSMERR42_MaliciousAndMalformedArchivesAreRefused(t *testing.T) {
	// - Setup: every tarball is generated in-test with `archive/tar` and validly signed by K, so only
	//   scan rules apply.
	// - Steps:
	//   1. Run `verifySignature` and then `scan(runtime.GOARCH)` on each row, giving its `check`:
	//
	//      | Archive | `check` |
	//      |---|---|
	//      | `top/../evil` | `path` |
	//      | `../evil` | `path` |
	//      | `/etc/passwd` | `path` |
	//      | `top/a/../../x` | `path` |
	//      | `top/link` symlink to `/etc` | `entry-type` |
	//      | `top/hard` hard link to `top/nexusd` | `entry-type` |
	//      | char device | `entry-type` |
	//      | block device | `entry-type` |
	//      | FIFO | `entry-type` |
	//      | two top-level dirs `top/`, `other/` | `layout` |
	//      | a file at the top level | `layout` |
	//      | top named `nexusd-v9.9.2-linux-386` | `layout` |
	//      | top named `nexusd-v9.9.2-linux-<other arch>` (A-13 arch mismatch) | `layout` |
	//      | top named `nexusd-9.9.2-linux-<goarch>` (no `v`) | `layout` |
	//      | top named `foo` | `layout` |
	//      | each of the 5 required files missing, one row per file | `required-file` |
	//      | `top/nexusd` twice (variants in TC-SM-E5) | `duplicate-entry` |
	//      | a non-gzip body | `layout` or format |
	//      | a truncated gzip stream | `layout` or format |
	//      | a gzip that is valid but whose tar is truncated mid-header | `layout` or format |
	// - Expected:
	//   - Each row is `update-package-invalid` with the listed `check`.
	//   - No row leaves anything written (scan writes nothing).
	t.Skip("TC-SM-ERR42: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-ERR43 (WI 5.2; FR-7, NFR-5): A file swapped between verification and scan or extraction fails the EOF digest check
func TestTCSMERR43_AFileSwappedBetweenVerificationAnd(t *testing.T) {
	// - Setup:
	//   - a valid signed package P;
	//   - a malicious package M with the same layout but a different `install.sh`;
	//   - in the staging file, P is replaced by M through rename after `verifySignature` returns.
	// - Steps:
	//   1. `scan`.
	//   2. Restore P, run `verifySignature` again, swap to M, then call `extractAll(dst)`.
	//   3. Repeat with `extractFile("nexusd")`.
	// - Expected:
	//   - Step 1 errors (digest mismatch).
	//   - Steps 2 and 3 error, and `dst` is removed, so no extracted file remains.
	//   - An error is returned before any probe exec.
	t.Skip("TC-SM-ERR43: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SM-ERR44 (WI 5.2; FR-7): probeVersion failures are update-package-invalid
func TestTCSMERR44_ProbeVersionFailuresAreUpdatePackageInvalid(t *testing.T) {
	// - Setup: the real Runner with the probe timeout injected as 300 ms; stand-ins that:
	//   - sleep 5 s;
	//   - print `nexusd v9.9.8` while `v9.9.2` is expected;
	//   - exit 1;
	//   - print `garbage`;
	//   - are a 4-byte `\x7fELF` file whose exec fails with `ENOEXEC`.
	// - Steps:
	//   1. Call `probeVersion` on each.
	// - Expected:
	//   - The `check` values, in order, are:
	//     - `version-probe` (timeout, about 300 ms);
	//     - `version-mismatch`, naming both versions;
	//     - `version-probe`;
	//     - `version-probe` (the output does not parse as `nexusd <version>`);
	//     - `version-probe`.
	//   - The probe dir is removed in every case.
	t.Skip("TC-SM-ERR44: SKIP-only stub; un-skip when WI 5.2 lands")
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
	//     removal, which also covers Prepare failures and `outcome-unknown`).
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
