// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-48, TC-HP-49, TC-HP-50, TC-HP-51, TC-HP-E11, TC-HP-ERR13.
// Owner WIs: 3.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-HP-48: Configure decodes strictly and starts nothing. Owner WI 3.1. Requirements: FR-1, FR-11.
//
// - Setup: `New(Options{FS: F-FS(UBU), GOOS: "linux"})`; goroutine baseline.
// - Steps: `Configure` with a decoder of `{enabled: true, sampleInterval: 5s}`; read `Settings()`.
// - Expected: `Settings{5s, 1h, 2s}` (unset keys take defaults); `p.run.Load() == nil`; goroutine count unchanged; calling `Configure` three times more changes nothing else.
func TestTCHP48_ConfigureDecodesStrictlyAndStartsNothing(t *testing.T) {
	t.Skip("TC-HP-48: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-HP-49: Settings swap atomically under the race detector. Owner WI 3.1. Requirements: FR-1.
//
// - Setup: as TC-HP-48.
// - Steps: 8 goroutines alternate `Configure` with `{sampleInterval: 2s, historyDuration: 2s}` and `{sampleInterval: 60s, historyDuration: 24h}` 500 times while 8 read `Settings()`.
// - Expected: no race report; every read is one of the two complete settings (never `2s` with `24h`).
func TestTCHP49_SettingsSwapAtomicallyUnderTheRaceDetector(t *testing.T) {
	t.Skip("TC-HP-49: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-HP-50: Preflight is unavailable off Linux and names the GOOS. Owner WI 3.1. Requirements: FR-3, CON-5.
//
// - Setup: `New(Options{GOOS: "darwin", FS: F-FS(UBU)})`.
// - Steps: `Preflight(ctx)`.
// - Expected: unavailable; reason exactly `the host provider reads Linux /proc and /sys; this host is darwin`; F-FS recorded no reads.
func TestTCHP50_PreflightIsUnavailableOffLinuxAndNames(t *testing.T) {
	t.Skip("TC-HP-50: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-HP-51: Preflight is available on both captures and reports its details. Owner WI 3.1. Requirements: FR-3, CON-5.
//
// - Setup: `GOOS: "linux"`, F-FS(UBU) with `Platform` init systemd; F-FS(ALP) with init openrc; default settings.
// - Steps: `Preflight(ctx)`.
// - Expected: available; details `initSystem` = `systemd` / `openrc`, `sampleIntervalSeconds` = 10, `historyDurationSeconds` = 3600; F-FS recorded `proc/stat`, `proc/meminfo`, `proc/self/mountinfo` and nothing outside the allow-list.
func TestTCHP51_PreflightIsAvailableOnBothCapturesAnd(t *testing.T) {
	t.Skip("TC-HP-51: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-HP-E11: A single unreadable source does not make the provider unavailable. Owner WI 3.1. Requirements: FR-3, FR-12.
//
// - Setup: F-FS(UBU) with SP-SANDBOX.
// - Steps: `Preflight`.
// - Expected: available.
func TestTCHPE11_ASingleUnreadableSourceDoesNotMake(t *testing.T) {
	t.Skip("TC-HP-E11: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-HP-ERR13: Preflight names the first missing required source. Owner WI 3.1. Requirements: FR-3.
//
// - Setup: F-FS(UBU) variants: `remove("proc/stat")`; `deny("proc/meminfo")`; `remove("proc/self/mountinfo")`; all three removed.
// - Steps: `Preflight` for each.
// - Expected: unavailable with a reason naming `/proc/stat`, `/proc/meminfo`, `/proc/self/mountinfo`, and `/proc/stat` (the first) respectively.
func TestTCHPERR13_PreflightNamesTheFirstMissingRequiredSource(t *testing.T) {
	t.Skip("TC-HP-ERR13: SKIP-only stub; un-skip when WI 3.1 lands")
}
