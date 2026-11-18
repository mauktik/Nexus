// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-73, TC-SV-74, TC-SV-75, TC-SV-76, TC-SV-77, TC-SV-78, TC-SV-79, TC-SV-E14, TC-SV-ERR17.
// Owner WIs: 4.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package services

import "testing"

// TC-SV-73: Preflight is unavailable off Linux and the package compiles there. Owner WI 4.1. Requirements: FR-3, CON-3.
//
// - Setup: `New(Options{GOOS: "darwin", Run: fh.Run})`, with F-HOSTS recording. A second subtest uses `Options{}` on the darwin dev machine (the real `runtime.GOOS`).
// - Steps:
//  1. `Preflight(ctx)`.
//
// - Expected:
//   - unavailable, with a reason naming `darwin`;
//   - zero F-HOSTS calls;
//   - `make vet` (darwin and linux) passes on the package, which is the compile half, checked by the WI's verification note.
func TestTCSV73_PreflightIsUnavailableOffLinuxAndThe(t *testing.T) {
	t.Skip("TC-SV-73: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SV-74: Preflight is available on every systemd and OpenRC capture with every capabilities detail. Owner WI 4.1. Requirements: FR-3, FR-8, FR-9, CON-3.
//
// - Setup:
//   - Each capture with `GOOS: "linux"`, `Platform` init from the capture, F-HOSTS and F-ROOTS.
//   - VM classification for `t1-ubuntu-systemd` (`hostparse.Classify` over the copied files, with the container markers removed).
//   - Settings `control: [{name: nx-svc-test}, {name: nx-not-installed}]`, `reboot.enabled: true`.
//
// - Steps:
//  1. `Configure`, then `Preflight`.
//
// - Expected: Available, with details:
//   - `initSystem` from the capture;
//   - `logSource` `journal` on systemd and `openrc-files` on OpenRC;
//   - `controlAllowList` = `[{name: nx-svc-test, actions: [start stop restart], timeoutSeconds: 90, status: ok}, {name: nx-not-installed, …, status: missing}]`;
//   - `rebootEnabled: true`;
//   - `rebootCommand` = `RebootArgv` joined;
//   - `selfService` = TC-SV-67's or TC-SV-68's value for the capture;
//   - `protected` ⊇ the static list for the init system plus `selfService`;
//   - `bootIdSource: kernel` on the VM variant.
func TestTCSV74_PreflightIsAvailableOnEverySystemdAnd(t *testing.T) {
	t.Skip("TC-SV-74: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SV-75: A container's reboot is available only with a boot-ID mount. Owner WI 4.1. Requirements: FR-3, FR-10, FR-11, A-3, ASM-7.
//
// - Setup: `t1-debian-systemd` (a Docker container) with `reboot.enabled: true`. Variant A: `proc/self/mountinfo` as captured (no boot-ID mount). Variant B: the same file plus one line derived from a captured line, with mount point `/proc/sys/kernel/random/boot_id`.
// - Steps:
//  1. `Preflight` on each variant.
//
// - Expected:
//   - A: the provider is available, but `rebootAvailable: false`, with `rebootUnavailableReason` stating that the container's boot ID is not per-container, so a reboot could not be confirmed (the ASM-7 reason);
//   - B: `rebootAvailable: true` and `bootIdSource: container-mount`.
func TestTCSV75_AContainersRebootIsAvailableOnlyWith(t *testing.T) {
	t.Skip("TC-SV-75: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SV-76: Preflight runs at most two execs and finishes inside the startup bound. Owner WI 4.1. Requirements: FR-3, A-26, NFR-1.
//
// - Setup: `t1-ubuntu-systemd` with three allow-listed names. F-HOSTS answers the `show -p LoadState,Id` over all of them in one call.
// - Steps:
//  1. `Preflight`, timing it.
//
// - Expected:
//   - exactly two F-HOSTS calls: `Dependencies`'s argv, and one `/usr/bin/systemctl show -p LoadState,Id -- nx-a.service nx-b.service nx-c.service`;
//   - it finishes in under 1 s;
//   - `len(Sent()) == 0`.
func TestTCSV76_PreflightRunsAtMostTwoExecsAnd(t *testing.T) {
	t.Skip("TC-SV-76: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SV-77: The protected set resolves the self unit, its dependencies and the OpenRC net provider. Owner WI 4.1. Requirements: FR-9.
//
// - Setup:
//   - systemd: F-ROOTS with the renamed cgroup `nexus-agent.service` and F-HOSTS `deps-nexus-agent.txt` (`Requires=nexusd-db.service`);
//   - OpenRC: `t1-alpine-openrc` (nexusd needs `net`);
//   - the allow-list contains `nexusd-db` and the OpenRC `net` provider.
//
// - Steps:
//  1. `Preflight`, then `p.protected(name, canonical)` for each name.
//
// - Expected:
//   - systemd: `nexus-agent`, `nexusd` and `nexusd-db` are protected, each with a reason (self unit, literal, `Requires=` of the self unit);
//   - OpenRC: the `net` provider is protected with reason `nexusd needs net`;
//   - capabilities `controlAllowList` shows `nexusd-db` (and the OpenRC provider) with `status: protected` and the reason;
//   - `nx-svc-test` is not protected.
func TestTCSV77_TheProtectedSetResolvesTheSelfUnit(t *testing.T) {
	t.Skip("TC-SV-77: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SV-78: Configure decodes strictly and a reload swaps the allow-list atomically. Owner WI 4.1. Requirements: FR-1, FR-8.
//
// - Setup: `t1-ubuntu-systemd`.
// - Steps:
//  1. `Configure({control: [{name: nx-svc-test}]})`, then a `list` read of `controllable` for `nx-svc-test`.
//  2. `Configure({control: []})`, then read again.
//  3. `Configure` with an unknown key `contorl`.
//  4. Under `-race`: 8 goroutines alternate two configs 500 times while 8 call `list`.
//
// - Expected:
//   - (1) `controllable: true`;
//   - (2) `false`;
//   - (3) an error naming `contorl`, with the previous settings kept;
//   - (4) no race, and every list sees one complete settings snapshot (`controllable` matches the protected set of the same snapshot).
func TestTCSV78_ConfigureDecodesStrictlyAndAReloadSwaps(t *testing.T) {
	t.Skip("TC-SV-78: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SV-79: No code path names a hypervisor tool or API. Owner WI 4.1. Requirements: FR-12, CON-6, OOS-1.
//
// - Setup: The non-test `.go` files of both packages.
// - Steps:
//  1. Scan the string literals with `go/parser`.
//
// - Expected: No literal matches `\b(pct|qm|pvesh|pveam|pvesm)\b`, `/api2/` or `:8006`. The only Proxmox literals are the two detection paths `/etc/pve` and `/var/lib/dpkg/info/pve-manager.list`.
func TestTCSV79_NoCodePathNamesAHypervisorTool(t *testing.T) {
	t.Skip("TC-SV-79: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SV-E14: Preflight with an unreadable deptree stays available with the static list. Owner WI 4.1. Requirements: FR-3, FR-9, NFR-3.
//
// - Setup: `t1-alpine-openrc` with the deptree removed.
// - Steps:
//  1. Preflight.
//
// - Expected: Available. `protected` ⊇ `ProtectedOpenRC`, and the details mark the dependency resolution `partial` with the reason. `networking` is still protected (it is in the static list).
func TestTCSVE14_PreflightWithAnUnreadableDeptreeStaysAvailable(t *testing.T) {
	t.Skip("TC-SV-E14: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-SV-ERR17: Preflight is unavailable, with a reason, for an unsupported init, missing tools and a Proxmox node. Owner WI 4.1. Requirements: FR-3, FR-12, CON-6, A-34.
//
// - Setup: `t1-ubuntu-systemd` F-ROOTS and F-HOSTS variants:
//   - `Platform.Init` `unknown`;
//   - `/usr/bin/systemctl` removed from the tools-exist check root;
//   - OpenRC capture without `/sbin/rc-update`;
//   - `etc/pve/` created;
//   - `var/lib/dpkg/info/pve-manager.list` created.
//
// - Steps:
//  1. Preflight.
//
// - Expected:
//   - unavailable, with reasons naming the init system, `/usr/bin/systemctl` and `/sbin/rc-update` respectively;
//   - both Proxmox variants give exactly `Proxmox VE node: the services provider never manages the hypervisor`, with zero F-HOSTS calls.
func TestTCSVERR17_PreflightIsUnavailableWithAReasonFor(t *testing.T) {
	t.Skip("TC-SV-ERR17: SKIP-only stub; un-skip when WI 4.1 lands")
}
