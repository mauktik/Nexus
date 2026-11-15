// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-87, TC-HP-88, TC-HP-89, TC-HP-E23, TC-HP-E24, TC-HP-E25, TC-HP-ERR23, TC-HP-ERR24.
// Owner WI: 4.3. Split from hostprov_handlers_tc_test.go by the orchestrator so
// that Phase 4 work items can run in parallel. Each stub compiles today and
// skips; the developer of the owning work item replaces the skip and the
// comment with the test (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import "testing"

// TC-HP-87: Interfaces on both captures agree with ip addr. Owner WI 4.3. Requirements: FR-9, NFR-3, NFR-6, FR-15.
//
// - Setup: host stack per capture, ready snapshot.
// - Steps: interfaces handler.
// - Expected:
//   - names equal `tools/ip-addr`'s; `lo` has `loopback true` (type 772), others false;
//   - `state` = `Operstate(operstate)`; `mac` = `address`; `mtu` = `mtu`; `ipv4`/`ipv6` with prefix lengths equal ip addr's;
//   - `rxBytes`, `txBytes`, errors and drops equal `fs/proc/net/dev` (read live);
//   - rates equal TC-HP-63's; `assertExplicit` passes.
func TestTCHP87_InterfacesOnBothCapturesAgreeWithIp(t *testing.T) {
	t.Skip("TC-HP-87: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-88: Default gateways equal ip route. Owner WI 4.3. Requirements: FR-9.
//
// - Setup: host stack per capture.
// - Steps: netconfig handler.
// - Expected: one item `current`; `defaultGateways` = the `tools/ip-route` defaults with `family` (`ipv4`/`ipv6`), `address`, `interface`, `metric`, sorted by metric.
func TestTCHP88_DefaultGatewaysEqualIpRoute(t *testing.T) {
	t.Skip("TC-HP-88: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-89: DNS resolvers, with and without systemd-resolved. Owner WI 4.3. Requirements: FR-9.
//
// - Setup: UBU (resolved in use); ALP (plain).
// - Steps: netconfig handler.
// - Expected: UBU: `dns.source` = `/run/systemd/resolve/resolv.conf`; `servers` = that file's nameservers with `stub false`, plus `127.0.0.53` with `stub true`. ALP: `source` `/etc/resolv.conf`, servers in file order, `stub` true only for loopback addresses.
func TestTCHP89_DNSResolversWithAndWithoutSystemdResolved(t *testing.T) {
	t.Skip("TC-HP-89: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-E23: Interfaces are enumerated through symlink entries. Owner WI 4.3. Requirements: FR-9, CON-5.
//
// - Setup: F-FS(UBU) default shaping.
// - Steps: interfaces handler.
// - Expected: the same names as `tools/ip-addr`; zero interfaces is a failure.
func TestTCHPE23_InterfacesAreEnumeratedThroughSymlinkEntries(t *testing.T) {
	t.Skip("TC-HP-E23: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-E24: A host with no default route. Owner WI 4.3. Requirements: FR-9, FR-12.
//
// - Setup: UBU with `proc/net/route` and `ipv6_route` reduced to their non-default lines.
// - Steps: netconfig handler.
// - Expected: `defaultGateways []`; `assertExplicit` passes (an empty list is a value; if listed in `unavailable`, its reason is `not reported:`).
func TestTCHPE24_AHostWithNoDefaultRoute(t *testing.T) {
	t.Skip("TC-HP-E24: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-E25: Neighbour tables are never read. Owner WI 4.3. Requirements: NFR-6.
//
// - Setup: UBU with F-FS recording; F-ADDRS counting calls.
// - Steps: interfaces and netconfig handlers.
// - Expected: no recorded path under `proc/net/` other than `dev`, `route`, `ipv6_route`; the only netlink use is F-ADDRS (called once per request); `mac` fields are each interface's own `address` only.
func TestTCHPE25_NeighbourTablesAreNeverRead(t *testing.T) {
	t.Skip("TC-HP-E25: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-ERR23: A down or virtual link has no speed. Owner WI 4.3. Requirements: FR-9, FR-12.
//
// - Setup: UBU with `einval("sys/class/net/<a down interface>/speed")`; a virtual interface without `device` whose speed reads `-1`.
// - Steps: interfaces handler.
// - Expected: both `speedMbps` null with a `not reported:` reason; the call does not fail; other fields populated.
func TestTCHPERR23_ADownOrVirtualLinkHasNo(t *testing.T) {
	t.Skip("TC-HP-ERR23: SKIP-only stub; un-skip when WI 4.3 lands")
}

// TC-HP-ERR24: Unreadable route tables leave gateways explicitly unavailable. Owner WI 4.3. Requirements: FR-9, FR-12.
//
// - Setup: UBU with `deny("proc/net/route")` and `deny("proc/net/ipv6_route")`; separately `deny("etc/resolv.conf")`.
// - Steps: netconfig handler.
// - Expected: `defaultGateways []` listed in `unavailable` with `permission denied: /proc/net/route`; with resolv.conf denied: `dns.servers []` listed with `permission denied: /etc/resolv.conf`; no error.
func TestTCHPERR24_UnreadableRouteTablesLeaveGatewaysExplicitlyUnavailable(t *testing.T) {
	t.Skip("TC-HP-ERR24: SKIP-only stub; un-skip when WI 4.3 lands")
}
