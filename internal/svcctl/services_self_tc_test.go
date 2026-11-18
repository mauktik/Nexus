// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-67, TC-SV-68, TC-SV-69, TC-SV-70, TC-SV-71, TC-SV-72, TC-SV-E12, TC-SV-E13, TC-SV-ERR16.
// Owner WIs: 3.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package svcctl

import "testing"

// TC-SV-67: SelfService on systemd takes the last .service element of the 0:: cgroup line. Owner WI 3.4. Requirements: FR-9, A-11.
//
// - Setup: F-ROOTS `proc/<pid>/cgroup` from each systemd capture (`0::/system.slice/nexusd.service`). Variants: `0::/system.slice/nexus-agent.service` (renamed), `0::/system.slice/nexusd.service/payload`, a hybrid file with `1:name=systemd:/…` lines before `0::`, and a file with no `.service` element (`0::/user.slice`).
// - Steps:
//  1. `SelfService(platform.Systemd, procRoot, "", pid)`.
//
// - Expected:
//   - `nexusd`, `nexus-agent`, and `nexusd` (the last `.service` element);
//   - the hybrid file resolves from the `0::` line;
//   - `/user.slice` gives `("", ErrNoSelfService)`.
func TestTCSV67_SelfServiceOnSystemdTakesTheLastService(t *testing.T) {
	t.Skip("TC-SV-67: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SV-68: SelfService on OpenRC finds the script whose child_pid is this process. Owner WI 3.4. Requirements: FR-9, A-8.
//
// - Setup: F-ROOTS `run/openrc/options/` from `t1-alpine-openrc`, plus `nginx/child_pid` = 4242 and a renamed `nexus-agent/child_pid` = the target PID.
// - Steps:
//  1. `SelfService(platform.OpenRC, "", runRoot, pid)` for the captured nexusd PID and for the renamed one.
//
// - Expected: `nexusd` and `nexus-agent` respectively. Only `child_pid` files are opened, with no other file read.
func TestTCSV68_SelfServiceOnOpenRCFindsTheScriptWhose(t *testing.T) {
	t.Skip("TC-SV-68: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SV-69: Dependencies on systemd keeps the .service units of Requires, BindsTo, Requisite and PartOf. Owner WI 3.4. Requirements: FR-9, A-39.
//
// - Setup: Each systemd capture's `deps-nexusd.txt` (captured with `show -p Requires,BindsTo,Requisite,PartOf`, WI 3.1), plus a variant `Requires=sysinit.target nexusd-db.service -.mount` / `BindsTo=nx-bound.service` / `Requisite=` / `PartOf=nx-part.service`, in the order and `Key=value` form systemd prints for that argv.
// - Steps:
//  1. `Dependencies(ctx, platform.Systemd, "nexusd", …)`.
//
// - Expected:
//   - for each capture: exactly its `.service` names without the suffix;
//   - for the variant: `[nexusd-db nx-bound nx-part]` (a `PartOf=` unit is kept exactly like a `Requires=` or `BindsTo=` unit; `sysinit.target` and `-.mount` are dropped as non-`.service`);
//   - the argv is exactly `/usr/bin/systemctl show -p Requires -p BindsTo -p Requisite -p PartOf -- nexusd.service` (no `StopPropagatedFrom`, A-39).
func TestTCSV69_DependenciesOnSystemdKeepsTheServiceUnits(t *testing.T) {
	t.Skip("TC-SV-69: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SV-70: Dependencies on OpenRC resolves net to its provider from the deptree. Owner WI 3.4. Requirements: FR-9, A-9.
//
// - Setup: `t1-alpine-openrc`'s `run/openrc/deptree`, plus a variant where nexusd also `ineed`s the real script `nx-svc-test`.
// - Steps:
//  1. `Dependencies(ctx, platform.OpenRC, "nexusd", …)`.
//
// - Expected: `net` resolves through the virtual entry's `providedby_*` to the capture's provider (`networking` in the image), and a real script resolves to itself. No `partial` marker.
func TestTCSV70_DependenciesOnOpenRCResolvesNetToIts(t *testing.T) {
	t.Skip("TC-SV-70: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SV-71: RebootArgv is the fixed command per init system. Owner WI 3.4. Requirements: FR-10, FR-12, A-5.
//
// - Setup: None.
// - Steps:
//  1. `RebootArgv(platform.Systemd)`, `(platform.OpenRC)` and `(platform.InitSystem("sysvinit"))`.
//
// - Expected:
//   - `[/usr/bin/systemctl reboot]`;
//   - `[/sbin/reboot]` (no `-f`, no other argument);
//   - an error naming the init system.
func TestTCSV71_RebootArgvIsTheFixedCommandPerInit(t *testing.T) {
	t.Skip("TC-SV-71: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SV-72: The cgroup and deptree parsers survive fuzzing. Owner WI 3.4. Requirements: NFR-3.
//
// - Setup: `FuzzCgroupSelf` and `FuzzDeptree`, seeded from the captures.
// - Steps:
//  1. Seeds, plus 60 s `-fuzz` per target.
//
// - Expected: No panic, and every result is a name matching `ServiceNamePattern` or empty. `TestTCSV72_TheCgroupAndDeptreeParsersSurviveFuzzing` asserts that the targets exist and are seeded.
func TestTCSV72_TheCgroupAndDeptreeParsersSurviveFuzzing(t *testing.T) {
	t.Skip("TC-SV-72: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SV-E12: SelfService ignores an oversized child_pid file and a non-matching PID. Owner WI 3.4. Requirements: FR-9.
//
// - Setup: `options/a/child_pid` of 100 bytes, starting with the target PID; `options/b/child_pid` = `999999`; `options/nexusd/child_pid` = the target PID.
// - Steps:
//  1. `SelfService`.
//
// - Expected: `nexusd`. The oversized file is skipped (bounded read at 64 bytes), never matched by prefix.
func TestTCSVE12_SelfServiceIgnoresAnOversizedChildPidFile(t *testing.T) {
	t.Skip("TC-SV-E12: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SV-E13: An unreadable deptree returns the needed names with the partial marker. Owner WI 3.4. Requirements: FR-9, A-9.
//
// - Setup: F-ROOTS with the deptree removed, and nexusd's init script captured as `depend() { need net }`.
// - Steps:
//  1. `Dependencies`.
//
// - Expected: `[net]` with the `partial` marker set, and no error.
func TestTCSVE13_AnUnreadableDeptreeReturnsTheNeededNames(t *testing.T) {
	t.Skip("TC-SV-E13: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SV-ERR16: SelfService with no match returns ErrNoSelfService. Owner WI 3.4. Requirements: FR-9.
//
// - Setup: OpenRC options with no matching `child_pid`; a systemd cgroup `0::/init.scope`; a missing cgroup file.
// - Steps:
//  1. `SelfService`.
//
// - Expected: `("", err)` with `errors.Is(err, ErrNoSelfService)` for the first two. The missing file gives an error wrapping `fs.ErrNotExist` (test-strategy Open Item 4).
func TestTCSVERR16_SelfServiceWithNoMatchReturnsErrNoSelfService(t *testing.T) {
	t.Skip("TC-SV-ERR16: SKIP-only stub; un-skip when WI 3.4 lands")
}
