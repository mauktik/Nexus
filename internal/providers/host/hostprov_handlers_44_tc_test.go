// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-90, TC-HP-91, TC-HP-92, TC-HP-93, TC-HP-94, TC-HP-95, TC-HP-96, TC-HP-97, TC-HP-ERR25.
// Owner WI: 4.4. Split from hostprov_handlers_tc_test.go by the orchestrator so
// that Phase 4 work items can run in parallel. Each stub compiles today and
// skips; the developer of the owning work item replaces the skip and the
// comment with the test (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-HP-90: Reboot required on the Debian family follows the apt rule. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: UBU F-FS variants: (a) `var/run/reboot-required` and `.pkgs` with `linux-image-6.8.0-50-generic\nlinux-base\n`; (b) no marker, one `apt.RebootMarkerWriters` path present; (c) no marker, no writer; (d) as (a) plus the container marker `run/systemd/container`.
// - Steps: health handler.
// - Expected: (a) and (d) `state "true"`, `packages` = the two names, `check debian-reboot-required-marker`, `evidence` names `/var/run/reboot-required`; (b) `"false"`; (c) `"unknown"` with evidence that no marker writer is installed. (d) encodes open item 1 of test-strategy.md.
func TestTCHP90_RebootRequiredOnTheDebianFamilyFollows(t *testing.T) {
	t.Skip("TC-HP-90: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-91: Reboot required on Alpine compares the running kernel with /lib/modules. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: ALP F-FS, no container markers (VM capture); variants with `lib/modules/<osrelease>` present and removed.
// - Steps: health handler.
// - Expected: present → `"false"`, missing → `"true"`; `check alpine-running-kernel-modules`; evidence names `/lib/modules/<release>` with the capture's release.
func TestTCHP91_RebootRequiredOnAlpineComparesTheRunning(t *testing.T) {
	t.Skip("TC-HP-91: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-92: Reboot required is unknown in a non-Debian container. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: ALP F-FS plus `.dockerenv`, `lib/modules/<release>` missing; a platform with an unknown OS family and no container.
// - Steps: health handler.
// - Expected: container: `"unknown"`, evidence says the kernel belongs to the container host; unknown family: `"unknown"` with `not supported: …`.
func TestTCHP92_RebootRequiredIsUnknownInANon(t *testing.T) {
	t.Skip("TC-HP-92: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-93: Reboot constants mirror the apt provider. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: import `internal/providers/apt`.
// - Steps: compare.
// - Expected: the host constants equal `strings.TrimPrefix(apt.RebootMarker, "/")`, `…RebootPackagesFile`, and each of `apt.RebootMarkerWriters` with the leading `/` removed, in order; every one is allow-listed.
func TestTCHP93_RebootConstantsMirrorTheAptProvider(t *testing.T) {
	t.Skip("TC-HP-93: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-94: Failed services from the captured outputs, cut at 20 names. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: UBU with F-RUN answering `list-units-failed.txt`; ALP with `rc-status-crashed.txt`; UBU with a capture-derived output of 21 unit lines.
// - Steps: health handler.
// - Expected: `failedServices.count` and `names` equal `svcctl.Failed`'s on the same output, `source` names the tool; with 21: `count 21`, 20 names (the first 20 sorted), `namesTruncated true`.
func TestTCHP94_FailedServicesFromTheCapturedOutputsCut(t *testing.T) {
	t.Skip("TC-HP-94: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-95: OOM kills come from vmstat only. Owner WI 4.4. Requirements: FR-10, NFR-6.
//
// - Setup: UBU; variant with the `oom_kill` line removed.
// - Steps: health handler; read F-FS's recorder.
// - Expected: `oomKills` = the captured `oom_kill`; removed → null with `not present: /proc/vmstat oom_kill`; no recorded path contains `kmsg` or `log`.
func TestTCHP95_OOMKillsComeFromVmstatOnly(t *testing.T) {
	t.Skip("TC-HP-95: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-96: Top processes carry owner names and only comm. Owner WI 4.4. Requirements: FR-10, NFR-6, FR-15.
//
// - Setup: ready snapshot per capture; variant where one top pid's uid has no passwd entry; variant where one top pid's `status` is removed after the scan.
// - Steps: health handler.
// - Expected:
//   - `topByCpu`/`topByMemory` equal TC-HP-64's; `uid` from `t1/proc/<pid>/status`; `user` from `etc/passwd`;
//   - no passwd entry → `user` null with `not reported: uid N has no /etc/passwd entry`; vanished → the sampled values kept, `uid` and `user` null with `not present: /proc/<pid>/status`;
//   - `processSampleSeconds` = 10; `assertExplicit` passes.
func TestTCHP96_TopProcessesCarryOwnerNamesAndOnly(t *testing.T) {
	t.Skip("TC-HP-96: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-97: Health output never carries command lines or environment. Owner WI 4.4. Requirements: NFR-6, FR-10.
//
// - Setup: ready snapshot (UBU).
// - Steps: marshal the health output; walk every key.
// - Expected: process objects have exactly `pid`, `command`, `uid`, `user`, `cpuPercent`, `rssBytes` (plus `unavailable` when set); no key contains `cmdline`, `args`, `argv`, `env`; each `command` is ≤ 15 bytes and equals that pid's captured comm.
func TestTCHP97_HealthOutputNeverCarriesCommandLinesOr(t *testing.T) {
	t.Skip("TC-HP-97: SKIP-only stub; un-skip when WI 4.4 lands")
}

// TC-HP-ERR25: Failed-service query failures leave the count unavailable. Owner WI 4.4. Requirements: FR-10, FR-12.
//
// - Setup: UBU with F-RUN for the failed-units argv: `hang` (timeout 250 ms); `ErrNotStarted` whose cause is ENOENT (the `realRun` shape of TC-HP-ERR18); exit 1 with bus error text.
// - Steps: health handler.
// - Expected: `failedServices.count` null and `names []`, both in `unavailable`, with `timed out after 250ms: /usr/bin/systemctl`, `not present: /usr/bin/systemctl` (Finding 7), `command failed: /usr/bin/systemctl exit 1`; the rest of health populated; no command output text in the response.
func TestTCHPERR25_FailedServiceQueryFailuresLeaveTheCount(t *testing.T) {
	t.Skip("TC-HP-ERR25: SKIP-only stub; un-skip when WI 4.4 lands")
}
