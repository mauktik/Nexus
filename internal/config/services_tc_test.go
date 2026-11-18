// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-37, TC-SV-38, TC-SV-39, TC-SV-40, TC-SV-E7, TC-SV-ERR11.
// Owner WIs: 2.3. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package config

import "testing"

// TC-SV-37: An absent or minimal services section takes every default. Owner WI 2.3. Requirements: FR-1, FR-8, FR-10.
//
// - Setup: Config documents with no `providers.services`; with `{enabled: true}`; and with `control: [{name: nx-a}, {name: nx-b, actions: [restart], timeout: 30s}]`.
// - Steps:
//  1. Parse and validate each.
//
// - Expected:
//   - first document: `Enabled false`, `Control` empty, `Reboot.Enabled false`, `Reboot.Timeout 5m`;
//   - third document: `nx-a` has actions `[start stop restart]` and timeout `90s`; `nx-b` has `[restart]` and `30s`.
func TestTCSV37_AnAbsentOrMinimalServicesSectionTakes(t *testing.T) {
	t.Skip("TC-SV-37: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SV-38: Boundary values are accepted, and an entry for a service that is not installed is accepted. Owner WI 2.3. Requirements: FR-8, FR-10.
//
// - Setup: Entries `{name: nx-not-installed}`, `{name: a, timeout: 10m}`, a 64-character name, `{name: getty-like}`; reboot timeouts `1m` and `30m`.
// - Steps:
//  1. Validate.
//
// - Expected: No error. `nexusd validate-config` on the same file exits 0.
func TestTCSV38_BoundaryValuesAreAcceptedAndAnEntry(t *testing.T) {
	t.Skip("TC-SV-38: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SV-39: Remote config set refuses reboot.enabled=true and accepts the other services keys. Owner WI 2.3. Requirements: FR-8, FR-10, A-19.
//
// - Setup: The shipped default file copied to `t.TempDir()`, and the edit path of `internal/config/edit_test.go`.
// - Steps:
//  1. Set `providers.services.reboot.enabled=true`.
//  2. Set `providers.services.reboot.enabled=false`.
//  3. Set `providers.services.enabled=true`.
//  4. Set `providers.services.control=[x]`.
//
// - Expected:
//   - step 1 is refused with the deny-list refusal, whose reason contains `the reboot is switched on only by editing the file on the host`, and the file's SHA-256 is unchanged;
//   - steps 2 and 3 are accepted, with the comments of the file preserved;
//   - step 4 is refused as a list (the existing "Lists and mappings are not settable");
//   - the existing string-key classification test still passes.
func TestTCSV39_RemoteConfigSetRefusesRebootEnabledTrue(t *testing.T) {
	t.Skip("TC-SV-39: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SV-40: The shipped default file carries the section, every key with its default and a comment. Owner WI 2.3. Requirements: FR-1, NFR-8.
//
// - Setup: `defaultConfigPath`.
// - Steps:
//  1. Parse the file and find `providers.services` with `keyNode`.
//  2. Load the whole file as config.
//
// - Expected:
//   - keys `enabled: false`, `control: []`, `reboot.enabled: false` and `reboot.timeout: 5m` are present, and each has a non-empty head or line comment;
//   - the `reboot.enabled` comment mentions that config set refuses `true`;
//   - the loaded config equals TC-SV-37's defaults.
func TestTCSV40_TheShippedDefaultFileCarriesTheSection(t *testing.T) {
	t.Skip("TC-SV-40: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SV-E7: A duplicate is reported at the second entry's location, and a .service suffix names the canonical form. Owner WI 2.3. Requirements: FR-8.
//
// - Setup: `control:` with `nx-a` at line 10 and `nx-a` again at line 12, and separately `nx-b.service`.
// - Steps:
//  1. Validate.
//
// - Expected:
//   - the duplicate error is located at line 12 and names line 10;
//   - the suffix error says to use `nx-b`.
func TestTCSVE7_ADuplicateIsReportedAtTheSecond(t *testing.T) {
	t.Skip("TC-SV-E7: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SV-ERR11: validate-config refuses every invalid services value with its location. Owner WI 2.3. Requirements: FR-1, FR-8, FR-9, FR-10, A-30, A-31.
//
// - Setup: One file per case, each with the offending value at a known line and column:
//   - names `ssh`, `nexusd`, `getty@tty1`, `nexus-op-x`, `dropbear` (OpenRC list, refused on any host), `foo.service`, `a b` and `-x`;
//   - a duplicate `nx-a`;
//   - action `reload`;
//   - timeouts `0s`, `-1s` and `11m`;
//   - reboot timeouts `59s` and `31m`;
//   - the unknown keys `contorl` and `reboot.enabeld`.
//
// - Steps:
//  1. Validate each, and run `nexusd validate-config --config <file>`.
//
// - Expected:
//   - each is an error whose text starts `<file>:<line>:<col>` at the offending node and names the rule;
//   - `validate-config` exits non-zero and prints the same location;
//   - the protected ones name `protected` and the list entry matched.
func TestTCSVERR11_ValidateConfigRefusesEveryInvalidServicesValue(t *testing.T) {
	t.Skip("TC-SV-ERR11: SKIP-only stub; un-skip when WI 2.3 lands")
}
