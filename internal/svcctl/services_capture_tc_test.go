// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-42, TC-SV-43, TC-SV-44, TC-SV-45.
// Owner WIs: 3.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package svcctl

import "testing"

// TC-SV-42: Every capture directory has a complete PROVENANCE and oracle. Owner WI 3.1. Requirements: NFR-2, NFR-3.
//
// - Setup: Glob the capture directories.
// - Steps:
//  1. Read each `PROVENANCE`.
//  2. Check the files by init system (`init-version` mentions systemd or OpenRC).
//
// - Expected:
//   - keys `host`, `source` (`container` or `host`), `date` (RFC 3339), `os-release`, `init-version`, `kernel`, `captured-as: root`, `commands` and `redaction` are present;
//   - systemd captures have `list-units.txt`, `list-unit-files.txt`, `show-nexusd.txt`, `is-enabled-nexusd.txt`, `deps-nexusd.txt`, `journal-nexusd-n20.jsonl`, `journal-nexusd-since.jsonl`, `journal-nexusd-perr.jsonl`, `proc/self/cgroup`, `proc/self/mountinfo`, `proc/sys/kernel/random/boot_id`, `oracle/is-active.tsv`, `oracle/is-enabled.tsv` and `oracle/failed.txt`;
//   - OpenRC captures have `rc-status.txt`, `rc-update.txt`, `rc-service-nexusd-status.txt`, `run/openrc/deptree`, `run/openrc/options/nexusd/child_pid` and `oracle/rc-service-status.tsv`;
//   - every file named in `commands` exists;
//   - `testdata/README.md` has a services section naming each directory.
func TestTCSV42_EveryCaptureDirectoryHasACompletePROVENANCE(t *testing.T) {
	t.Skip("TC-SV-42: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-SV-43: No capture carries environment, command lines or foreign conf.d values. Owner WI 3.1. Requirements: NFR-5.
//
// - Setup: Every file under `testdata/services/`.
// - Steps:
//  1. Scan each file.
//
// - Expected:
//   - no match for `Environment=`, `EnvironmentFile=`, `ExecStart=`, `"_CMDLINE"`, `"_EXE"`, `"_COMM"` or `password|passwd|token=|secret`;
//   - every line under `etc/conf.d/` and `etc/init.d/` matches `^\s*(output_log|error_log|output_logger|error_logger|description)=`;
//   - every journal line's `MESSAGE` comes from a `nexusd` capture file (the file name names only `nexusd`), and no journal line has a `_SYSTEMD_UNIT` other than `nexusd.service` when present.
func TestTCSV43_NoCaptureCarriesEnvironmentCommandLinesOr(t *testing.T) {
	t.Skip("TC-SV-43: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-SV-44: The three T1-container captures exist. Owner WI 3.1. Requirements: NFR-3, CON-3.
//
// - Setup: None.
// - Steps:
//  1. Stat `t1-ubuntu-systemd`, `t1-debian-systemd` and `t1-alpine-openrc`.
//
// - Expected:
//   - all three exist with `source: container`;
//   - their `init-version` lines show systemd 259 (Ubuntu), systemd 257 (Debian) and OpenRC;
//   - each systemd capture's `actions/` holds `start-nx-svc-test.txt`, `stop-nx-svc-test.txt`, `restart-nx-svc-test.txt` and `start-nx-svc-fail.txt` with an `Exit` line, and the Alpine capture holds `start-already-nx-svc-test.txt`.
func TestTCSV44_TheThreeT1ContainerCapturesExist(t *testing.T) {
	t.Skip("TC-SV-44: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-SV-45: The three real-host captures exist and carry the A-3, A-5, A-8, A-11 and A-22 evidence. Owner WI 3.1 (operator-deferred; un-skipped by TC-SV-L0). Requirements: NFR-3, NFR-4, CON-3, A-3, A-5, A-8, A-11, A-22.
//
// - Setup: The operator has run `capture.sh` on home-server, pihole and Public-Nginx and committed `home-server/`, `pihole/` and `public-nginx/`.
// - Steps:
//  1. For each missing directory: `t.Skip("operator capture pending (WI 3.1): <dir>")`.
//  2. Otherwise read the evidence files.
//
// - Expected:
//   - all three are `source: host`;
//   - pihole's and public-nginx's `boot_id` differ from each other, and each has a `mountinfo` line with mount point `/proc/sys/kernel/random/boot_id` (A-3);
//   - public-nginx's `readlink-sbin-reboot.txt` is `/bin/busybox`, and its `inittab-shutdown.txt` contains `/sbin/openrc shutdown` (A-5);
//   - public-nginx's `run/openrc/options/nexusd/child_pid` holds a decimal PID (A-8);
//   - home-server's `proc/self/cgroup` has a `0::` line ending `/nexusd.service` (A-11);
//   - pihole's `journal-nexusd-since.txt` records `Exit 0` (A-22: systemd 252 accepts `--since=@s.us`);
//   - pihole's `init-version` is systemd 252.
func TestTCSV45_TheThreeRealHostCapturesExistAnd(t *testing.T) {
	t.Skip("TC-SV-45: SKIP-only stub; un-skip when WI 3.1 lands")
}
