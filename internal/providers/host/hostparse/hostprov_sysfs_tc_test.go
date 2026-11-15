// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-40, TC-HP-41, TC-HP-42, TC-HP-43, TC-HP-44, TC-HP-45, TC-HP-46, TC-HP-47, TC-HP-E10.
// Owner WIs: 2.4. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package hostparse

import "testing"

// TC-HP-40: Operstate mapping. Owner WI 2.4. Requirements: FR-9.
//
// - Setup: a table.
// - Steps: `Operstate(s)` for `up\n`, `down`, `unknown`, `dormant`, `notpresent`, `lowerlayerdown`, `testing`, `""`, `garbage`.
// - Expected: `up`, `down`, `unknown`, `dormant`, `down`, `down`, `unknown`, `unknown`, `unknown`.
func TestTCHP40_OperstateMapping(t *testing.T) {
	t.Skip("TC-HP-40: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-HP-41: Link speed reports only positive values. Owner WI 2.4. Requirements: FR-9.
//
// - Setup: captured `sys/class/net/*/speed` values; a table.
// - Steps: parse `1000\n`, `-1`, `0`, `-5`, `abc`, `""`.
// - Expected: `1000`; the rest are "not reported" (no error leaks to a value of 0 or negative).
func TestTCHP41_LinkSpeedReportsOnlyPositiveValues(t *testing.T) {
	t.Skip("TC-HP-41: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-HP-42: Block sizes and flags agree with lsblk. Owner WI 2.4. Requirements: FR-8, NFR-3.
//
// - Setup: UBU `fs/sys/block/*/{size,queue/rotational,removable}`, `tools/lsblk` (`lsblk -b -J`).
// - Steps: parse each.
// - Expected: size × 512 equals lsblk `size` for every disk lsblk lists as `type: disk`; rotational and removable equal lsblk `rota` and `rm`.
func TestTCHP42_BlockSizesAndFlagsAgreeWithLsblk(t *testing.T) {
	t.Skip("TC-HP-42: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-HP-43: resolv.conf parsing, stub detection and resolved-in-use. Owner WI 2.4. Requirements: FR-9.
//
// - Setup: UBU `fs/etc/resolv.conf` and `fs/run/systemd/resolve/resolv.conf`; ALP `fs/etc/resolv.conf`; synthetic: 17 nameservers, `search`/`options`/comment lines, `127.0.0.53` plus `1.1.1.1`.
// - Steps: parse; `Stub(addr)`; `ResolvedInUse(etc, run)`.
// - Expected: nameservers in file order, at most 16 (the 17th dropped); comments and other keywords ignored; `Stub` true for `127.0.0.53`, `127.0.0.1`, `::1`, false for `192.168.7.1`, `fe80::1`; `ResolvedInUse` true for UBU (when its etc file lists only `127.0.0.53`/`127.0.0.54`), false for ALP, false for the mixed synthetic, false when the run file is nil.
func TestTCHP43_ResolvConfParsingStubDetectionAndResolved(t *testing.T) {
	t.Skip("TC-HP-43: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-HP-44: passwd maps uid to name and skips malformed lines. Owner WI 2.4. Requirements: FR-10.
//
// - Setup: both captures' `etc/passwd`; synthetic lines `x`, `a:b:notanumber:…`, `+::::::` (NIS).
// - Steps: parse.
// - Expected: `0 → root`, each capture line's uid → name; malformed lines skipped without error.
func TestTCHP44_PasswdMapsUidToNameAndSkips(t *testing.T) {
	t.Skip("TC-HP-44: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-HP-45: Timezone name and offset. Owner WI 2.4. Requirements: FR-5.
//
// - Setup: `testdata/tz/America_New_York`; inputs: (a) `/etc/timezone` = `Europe/Berlin` and a localtime link to `…/zoneinfo/America/New_York`; (b) no `/etc/timezone`, link `/usr/share/zoneinfo/America/New_York`; (c) no link target (a copied file) and no `/etc/timezone`; (d) no `/etc/localtime`.
// - Steps: resolve name and offset at `2026-01-15T12:00:00Z` and `2026-07-15T12:00:00Z`.
// - Expected: (a) name `Europe/Berlin` (timezone file wins); (b) `America/New_York`, offsets −18000 and −14400 (computed with `time.LoadLocationFromTZData` from the tzfile in-test, not literals in the assertion); (c) name not reported, offset still parsed; (d) `UTC`, 0, source `default (no /etc/localtime)`.
func TestTCHP45_TimezoneNameAndOffset(t *testing.T) {
	t.Skip("TC-HP-45: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-HP-46: Virtualization classification. Owner WI 2.4. Requirements: FR-5.
//
// - Setup: a facts table plus both captures' facts; `tools/systemd-detect-virt` where present.
// - Steps: `Classify(facts)`.
// - Expected:
//   - `/.dockerenv` → docker/container; `/run/.containerenv` → podman/container; `run/systemd/container` = `lxc` → lxc/container; `fuse.lxcfs` on `/proc/meminfo` → lxc; `/proc/vz` without `/proc/bc` → openvz; osrelease containing `microsoft` → wsl;
//   - a container marker plus DMI `QEMU` → the container (containers first);
//   - `sys/hypervisor/type` `xen` → xen/vm; each DMI table row → its type/vm (`innotek GmbH` → oracle; `Microsoft Corporation` + `Virtual Machine` → microsoft);
//   - hypervisor flag with no table match → unknown/vm; nothing → none/none;
//   - each capture equals its `tools/systemd-detect-virt` output; `evidence` names the marker used.
func TestTCHP46_VirtualizationClassification(t *testing.T) {
	t.Skip("TC-HP-46: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-HP-47: Time-sync output parsers. Owner WI 2.4. Requirements: FR-5.
//
// - Setup: captured `tools/timedatectl` and, where present, `tools/chronyc-tracking`; synthetic CSV lines derived from the capture with field 14 replaced.
// - Steps: `TimedatectlNTPSynchronized`, `ChronyTracking`.
// - Expected: `yes\n` → true, `no` → false, `maybe` → error; field 14 `Normal`, `Insert second`, `Delete second` → true, `Not synchronised` → false, other → error; a line with 13 fields → error.
func TestTCHP47_TimeSyncOutputParsers(t *testing.T) {
	t.Skip("TC-HP-47: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-HP-E10: The /sys, /etc and /run parsers survive the fuzz seeds. Owner WI 2.4. Requirements: NFR-4.
//
// - Setup: seeds from both captures plus the synthetic tables of TC-HP-40..47.
// - Steps: `go test` runs the seeds.
// - Expected: no panic; `ChronyTracking` and `TimedatectlNTPSynchronized` return errors, not values, for every non-matching seed.
func TestTCHPE10_TheSysEtcAndRunParsersSurvive(t *testing.T) {
	t.Skip("TC-HP-E10: SKIP-only stub; un-skip when WI 2.4 lands")
}
