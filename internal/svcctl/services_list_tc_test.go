// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-46, TC-SV-47, TC-SV-48, TC-SV-49, TC-SV-50, TC-SV-51, TC-SV-52, TC-SV-53, TC-SV-54, TC-SV-55, TC-SV-56, TC-SV-57, TC-SV-58, TC-SV-59, TC-SV-60, TC-SV-E8, TC-SV-E9, TC-SV-ERR12, TC-SV-ERR13.
// Owner WIs: 3.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package svcctl

import "testing"

// TC-SV-46: List on systemd captures equals the union of the two listings, with states and enabled words from the oracle. Owner WI 3.2. Requirements: FR-4, NFR-2, NFR-3.
//
// - Setup: Every systemd capture, with F-HOSTS over it.
// - Steps:
//  1. `List(ctx, platform.Systemd, Options{Run: fh.Run})`.
//  2. Compute the expected values from the capture (test-strategy.md, Expected values).
//
// - Expected:
//   - names equal the union of the `.service` first column of `list-units.txt` (suffix stripped, instance names kept) and the first column of `list-unit-files.txt`;
//   - `State` equals svcctl's existing `ActiveState` mapping applied to `oracle/is-active.tsv`'s word (exact words only; anything else is `unknown`);
//   - `Enabled` equals `oracle/is-enabled.tsv`'s word when it is one of `enabled`, `disabled`, `static`, `masked`, `indirect`, `generated` or `alias`, else `unknown`, with `EnabledRaw` holding the oracle word;
//   - no duplicates.
func TestTCSV46_ListOnSystemdCapturesEqualsTheUnion(t *testing.T) {
	t.Skip("TC-SV-46: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-47: List keeps the description and raw words, and maps only exact enabled words. Owner WI 3.2. Requirements: FR-4, NFR-3.
//
// - Setup: `t1-ubuntu-systemd` plus synthetic `list-unit-files` lines derived from its `cron.service enabled enabled` line, with the STATE replaced by `enabled-runtime`, `linked`, `transient`, `bad-setting` and `Enabled`.
// - Steps:
//  1. `List`.
//
// - Expected:
//   - each synthetic unit has `Enabled: unknown` and `EnabledRaw` equal to the exact word;
//   - `RawState` of a loaded unit is `<ACTIVE> (<SUB>)`, for example `active (running)` or `inactive (dead)`;
//   - `Description` is the rest of the `list-units` line after SUB, verbatim, including inner spaces, with `DescriptionKnown: true`;
//   - a unit-file-only entry has `DescriptionKnown: false`.
func TestTCSV47_ListKeepsTheDescriptionAndRawWords(t *testing.T) {
	t.Skip("TC-SV-47: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-48: Template units appear only from unit files, as stopped. Owner WI 3.2. Requirements: FR-4.
//
// - Setup: Systemd captures (each has `getty@.service` in unit files, and `getty@tty1` or `console-getty` in units where present).
// - Steps:
//  1. `List`.
//
// - Expected:
//   - `getty@` (the template, with `@` kept) appears once, with `UnitFileOnly: true`, `State: stopped`, `RawState: inactive (dead)` and `Enabled` from the unit-file STATE (test-strategy Open Item 3);
//   - an instance such as `getty@tty1`, when loaded, appears separately with its loaded state;
//   - no `UnitFileOnly` entry is ever `running`.
func TestTCSV48_TemplateUnitsAppearOnlyFromUnitFiles(t *testing.T) {
	t.Skip("TC-SV-48: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-49: List on OpenRC captures matches rc-status, rc-update and the rc-service oracle. Owner WI 3.2. Requirements: FR-4, NFR-2, NFR-3, A-10.
//
// - Setup: Every OpenRC capture, F-HOSTS, and F-ROOTS for the static descriptions.
// - Steps:
//  1. `List(ctx, platform.OpenRC, …)`.
//
// - Expected:
//   - names equal the script names of `rc-update.txt` ∪ `rc-status.txt`;
//   - `State` maps the bracket word exactly (`started`→`running`, `stopped`→`stopped`, `crashed`→`failed`, `starting`→`starting`, `stopping`→`stopping`, anything else→`unknown`) and agrees with `oracle/rc-service-status.tsv`'s mapped word;
//   - `RawState` is the bracket word;
//   - `Runlevels` equals the right side of `rc-update show -v`;
//   - `Enabled` is `enabled` iff `Runlevels` is non-empty, else `disabled`;
//   - `Description` comes from a literal `description="…"` in `etc/init.d/<name>` when present, and is unknown otherwise.
func TestTCSV49_ListOnOpenRCCapturesMatchesRcStatus(t *testing.T) {
	t.Skip("TC-SV-49: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-50: The failed set equals the oracle's failed services and ListFailed's .service subset. Owner WI 3.2. Requirements: FR-4, NFR-2, A-29.
//
// - Setup: Every capture. Each `t1-*` capture is made with `nx-svc-fail` started once, so it is `failed` (systemd) or `crashed`/`failed` (OpenRC).
// - Steps:
//  1. `List`, keeping `State == failed`.
//  2. Run `ListFailed` over the capture's `oracle/failed.txt`, served as the `list-units --state=failed` output.
//
// - Expected:
//   - set (1) equals the names in `oracle/failed.txt`, which equals the `.service` subset of `ListFailed`'s names;
//   - in every `t1-*` capture the set contains `nx-svc-fail`, so the case is never vacuously empty.
func TestTCSV50_TheFailedSetEqualsTheOraclesFailed(t *testing.T) {
	t.Skip("TC-SV-50: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-51: Names outside ServiceNamePattern are listed verbatim. Owner WI 3.2. Requirements: FR-4, A-24.
//
// - Setup: `t1-ubuntu-systemd`, with lines added from `internal/svcctl/testdata/systemd/list-units-failed-escaped.txt` (a `\x2d` unit) and a synthetic 65-character name.
// - Steps:
//  1. `List`.
//
// - Expected: Both appear with their names byte-for-byte (`\x2d` kept), and neither matches `ServiceNamePattern`.
func TestTCSV51_NamesOutsideServiceNamePatternAreListedVerbatim(t *testing.T) {
	t.Skip("TC-SV-51: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-52: Describe on systemd returns every DF-1 field from the show capture. Owner WI 3.2. Requirements: FR-5, NFR-2.
//
// - Setup: Every `show-<name>.txt` of every systemd capture (`nexusd`, `ssh` where present, a failed unit, a template instance, a masked unit where present).
// - Steps:
//  1. `Describe(ctx, platform.Systemd, name, …)`.
//
// - Expected: With `kv` = the capture's `key=value` lines:
//   - `Known` is `LoadState != not-found`, and `CanonicalName` is `Id` without `.service`;
//   - `State` maps `ActiveState`, and `SubState` and `Result` are verbatim;
//   - `Since` is `time.Unix` of `ActiveEnterTimestamp=@…` when active, else of `InactiveEnterTimestamp`;
//   - `MainPID` is `strconv.Atoi(kv.MainPID)`, `Restarts` is `NRestarts`, `MemoryBytes` is `MemoryCurrent`, and `UnitFilePath` is `FragmentPath`;
//   - `Enabled` is from `UnitFileState`.
func TestTCSV52_DescribeOnSystemdReturnsEveryDF1(t *testing.T) {
	t.Skip("TC-SV-52: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-53: Describe reports an unknown unit as not known, without an error. Owner WI 3.2. Requirements: FR-5.
//
// - Setup: F-HOSTS over `t1-ubuntu-systemd`. The fake answers `show -- nx-nope.service` exit 0 with `LoadState=not-found`, as the capture's `show-not-found.txt` does. An OpenRC capture answers `rc-service -C nx-nope status` with the captured `absent.txt` exit code.
// - Steps:
//  1. `Describe("nx-nope")` on each.
//
// - Expected: `Known: false`, with a nil error on both.
func TestTCSV53_DescribeReportsAnUnknownUnitAsNot(t *testing.T) {
	t.Skip("TC-SV-53: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-54: Describe returns the canonical unit of an alias. Owner WI 3.2. Requirements: FR-9, A-33.
//
// - Setup: `t1-ubuntu-systemd`'s `show-sshd.txt` (`Id=ssh.service`, captured after `apt-get install openssh-server` in the harness image) and `show-nx-netalias.txt` (`Id=systemd-networkd.service`).
// - Steps:
//  1. `Describe("sshd")` and `Describe("nx-netalias")`.
//
// - Expected: `CanonicalName` is `ssh` and `systemd-networkd` respectively, while `Name` stays the requested name.
func TestTCSV54_DescribeReturnsTheCanonicalUnitOfAn(t *testing.T) {
	t.Skip("TC-SV-54: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-55: Describe on OpenRC returns status, runlevels, script path and a static description. Owner WI 3.2. Requirements: FR-5.
//
// - Setup: `t1-alpine-openrc` with F-HOSTS and F-ROOTS.
// - Steps:
//  1. `Describe("nexusd")` and `Describe("nx-svc-test")`.
//
// - Expected:
//   - `State` is from `rc-service -C NAME status`;
//   - `Runlevels` is from `rc-update show -v`;
//   - `ScriptPath` is `/etc/init.d/NAME`;
//   - `Since` is nil with `Unavailable["since"]` naming OpenRC;
//   - `SubState`, `Result`, `Restarts` and `MemoryBytes` are nil, each with a reason;
//   - `Description` is the literal `description=` value;
//   - a description built from a variable (`description="$x"`, made in an F-ROOTS copy) gives `DescriptionKnown: false`.
func TestTCSV55_DescribeOnOpenRCReturnsStatusRunlevelsScript(t *testing.T) {
	t.Skip("TC-SV-55: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-56: EnabledAtBoot follows is-enabled on systemd and the boot or default runlevel on OpenRC. Owner WI 3.2. Requirements: FR-10.
//
// - Setup: systemd `is-enabled nexusd` outputs from the captures (`enabled`, exit 0), plus `disabled` (exit 1), `static`, `masked`, `enabled-runtime`, `indirect` and `linked` derived from `oracle/is-enabled.tsv` lines. OpenRC `rc-update show -v` lines `nexusd | default`, `nexusd | boot`, `nexusd | sysinit` and `nexusd |`.
// - Steps:
//  1. `EnabledAtBoot` for each.
//
// - Expected:
//   - systemd is true only for `enabled`, and the returned word is the raw output in every case;
//   - OpenRC is true for `default` and `boot`, and false for `sysinit` and none, with the runlevels as the word.
func TestTCSV56_EnabledAtBootFollowsIsEnabledOnSystemdAnd(t *testing.T) {
	t.Skip("TC-SV-56: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-57: List and Describe send exactly the fixed argv and never request a sensitive property. Owner WI 3.2. Requirements: FR-4, FR-5, NFR-5, CON-2.
//
// - Setup: F-HOSTS recording calls.
// - Steps:
//  1. `List` and `Describe("nexusd")` on systemd and OpenRC captures.
//
// - Expected:
//   - the calls are exactly `/usr/bin/systemctl list-units --type=service --all --plain --no-legend --no-pager --full`, `/usr/bin/systemctl list-unit-files --type=service --plain --no-legend --no-pager --full`, and `/usr/bin/systemctl show --timestamp=unix -p Id,LoadState,ActiveState,SubState,UnitFileState,Description,MainPID,ActiveEnterTimestamp,InactiveEnterTimestamp,Result,NRestarts,MemoryCurrent,FragmentPath -- nexusd.service`;
//   - on OpenRC: `/bin/rc-status -s -C`, `/sbin/rc-update show -v` and `/sbin/rc-service -C nexusd status`;
//   - no argv contains `Environment`, `ExecStart`, `ExecStartPre` or `cat`;
//   - `len(fh.Sent()) == 0`.
func TestTCSV57_ListAndDescribeSendExactlyTheFixed(t *testing.T) {
	t.Skip("TC-SV-57: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-58: Callers that do not set OutputCap keep the old cap and behavior. Owner WI 3.2. Requirements: CON-2.
//
// - Setup: The real runner with `/bin/sh -c 'head -c N /dev/zero'` as argv (the `TestRealRunnerEnvCapAndTimeout` pattern).
// - Steps:
//  1. With `OutputCap` 0 and N = 100 KiB.
//  2. With `OutputCap` 4 MiB and N = 5 MiB.
//  3. With `OutputCap` 4 MiB and N = 1 MiB.
//
// - Expected:
//   - step 1: `len(Output) == 64 KiB`, and `Truncated` is false or unset exactly as before (no error);
//   - step 2: `len(Output) == 4 MiB` and `Truncated == true`;
//   - step 3: 1 MiB and `Truncated == false`;
//   - `go test ./internal/svcctl ./internal/providers/nginx ./internal/providers/host` passes unchanged.
func TestTCSV58_CallersThatDoNotSetOutputCapKeep(t *testing.T) {
	t.Skip("TC-SV-58: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-59: F-HOSTS encodes the captured init-system behavior. Owner WI 3.2. Requirements: NFR-4, NFR-3.
//
// - Setup: F-HOSTS over `t1-ubuntu-systemd` and `t1-alpine-openrc`, with `nx-svc-fail` modeled `FailsOnStart`.
// - Steps:
//  1. Drive `start nx-svc-test`, then poll `show` 3 times; `start nx-svc-fail`; `stop nx-svc-fail`; `show -- nx-nope.service`; an argv `/usr/bin/systemctl start nx-svc-test.service; reboot`; a relative argv `systemctl show`.
//  2. On OpenRC, `rc-service nx-svc-test start` twice.
//
// - Expected:
//   - `start` exits 0, then `ActiveState` is `activating` for 2 polls and then `active` with a new `MainPID`;
//   - the failed start exits 1 with the bytes of `actions/start-nx-svc-fail.txt`, then `ActiveState=failed`, `Result=exit-code`, `MainPID=0`;
//   - `stop` of the failed unit exits 0 and it stays `failed`;
//   - the unknown `show` exits 0 with `LoadState=not-found`;
//   - the malformed argv and the relative argv each record a test error (checked with a recording `T`) and return `ErrNotStarted`;
//   - OpenRC's second start exits 0 with the captured already-started warning and no state change;
//   - `Sent()` lists only the start and stop calls.
func TestTCSV59_FHOSTSEncodesTheCapturedInitSystem(t *testing.T) {
	t.Skip("TC-SV-59: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-60: The listing and show parsers survive fuzzing seeded from every capture. Owner WI 3.2. Requirements: NFR-3.
//
// - Setup: `FuzzParseListUnits`, `FuzzParseListUnitFiles`, `FuzzParseShow`, `FuzzParseRCStatus` and `FuzzParseRCUpdate`, each seeded with every capture's file.
// - Steps:
//  1. `go test -run Fuzz ./internal/svcctl` (seeds), plus `-fuzz` for 60 s per target by the developer.
//
// - Expected: No panic. Every produced `State` is one of the six words, and every `Enabled` is in the FR-4 enum. The function `TestTCSV60_TheListingAndShowParsersSurviveFuzzing` asserts that the five fuzz targets exist and that each has ≥ 1 seed per capture.
func TestTCSV60_TheListingAndShowParsersSurviveFuzzing(t *testing.T) {
	t.Skip("TC-SV-60: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-E8: Unset, maximal or missing show values become nulls with reasons. Owner WI 3.2. Requirements: FR-5, NFR-3.
//
// - Setup: Variants of a captured `show-nexusd.txt`: `MemoryCurrent=[not set]`, `MemoryCurrent=18446744073709551615`, `NRestarts` line removed (older systemd), `MainPID=0` on an inactive unit, `ActiveEnterTimestamp=` empty (never started).
// - Steps:
//  1. `Describe`.
//
// - Expected: Each corresponding field is nil, with a non-empty `Unavailable` reason (`not set`, `not reported`, `not running`, `never entered`), and the other fields are unaffected.
func TestTCSVE8_UnsetMaximalOrMissingShowValuesBecome(t *testing.T) {
	t.Skip("TC-SV-E8: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-E9: Listings with or without the PRESET column, and descriptions with runs of spaces, parse identically. Owner WI 3.2. Requirements: FR-4, A-23.
//
// - Setup: `t1-debian-systemd`'s `list-unit-files.txt` (with PRESET), the same with the PRESET column removed, and a `list-units` line with the description `A  B   C`.
// - Steps:
//  1. Parse both.
//
// - Expected: The same `(name, enabled)` pairs from both, and the description `A  B   C` exactly.
func TestTCSVE9_ListingsWithOrWithoutThePRESETColumn(t *testing.T) {
	t.Skip("TC-SV-E9: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-ERR12: An over-cap listing is an error, never a partial list. Owner WI 3.2. Requirements: FR-4, NFR-3.
//
// - Setup: F-HOSTS answers `list-units` with 4 MiB + 1 byte made of repeated captured lines, and reports it truncated as the real runner would with `OutputCap` 4 MiB.
// - Steps:
//  1. `List`.
//
// - Expected: A nil slice and an error naming the tool and the 4 MiB cap.
func TestTCSVERR12_AnOverCapListingIsAnError(t *testing.T) {
	t.Skip("TC-SV-ERR12: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-SV-ERR13: A failing, missing or hung listing tool is an error within the timeout. Owner WI 3.2. Requirements: NFR-1, NFR-3.
//
// - Setup: F-HOSTS variants: `list-units` exits 1 with `Failed to connect to bus: No such file or directory`; `notStarted(ENOENT)`; `HangOn(list-unit-files)` with `Options.Timeout` 200 ms.
// - Steps:
//  1. `List`.
//
// - Expected:
//   - exit 1: an error containing the first stderr line;
//   - not started: an error wrapping `ErrNotStarted` with `errors.Is(err, fs.ErrNotExist)`;
//   - hang: returns within 1 s with `context.DeadlineExceeded`, and the concurrent `list-units` goroutine does not leak (the goroutine count returns to baseline).
func TestTCSVERR13_AFailingMissingOrHungListingTool(t *testing.T) {
	t.Skip("TC-SV-ERR13: SKIP-only stub; un-skip when WI 3.2 lands")
}
