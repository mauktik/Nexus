// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-30, TC-SV-31.
// Owner WIs: 2.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package problem

import "testing"

// TC-SV-30: The eight services problem types are registered with their status, exit code and extensions. Owner WI 2.1. Requirements: FR-6, FR-7, FR-9, FR-10, FR-11.
//
// - Setup: The architecture's Problem types table.
// - Steps:
//  1. Look up each slug in the registry.
//
// - Expected:
//   - `not-allowed`, `protected-service`, `reboot-unavailable`, `nexusd-not-enabled-at-boot` and `operations-running` are HTTP 409 with exit 1;
//   - `logs-unavailable` is HTTP 422 with exit 1;
//   - `service-action-failed` and `reboot-did-not-happen` are operation-level, by the same registry marker the existing op-level slugs (`outcome-unknown`, `script-failed`) carry, with exit 1;
//   - the extensions per row are exactly those of the table.
func TestTCSV30_TheEightServicesProblemTypesAreRegistered(t *testing.T) {
	t.Skip("TC-SV-30: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-SV-31: The new problem types round-trip through JSON with their extensions. Owner WI 2.1. Requirements: FR-10, FR-11.
//
// - Setup:
//   - `operations-running` with `operationIds: ["01A","01B"]`;
//   - `reboot-did-not-happen` with `stage: "trigger"`, `bootId: PREV` and `exitCode: 1`;
//   - `not-allowed` with `service`, `action` and `configKey`.
//
// - Steps:
//  1. `MarshalJSON`, then `UnmarshalJSON`.
//
// - Expected: Equal values. `operationIds` stays a JSON array of strings, and `exitCode` stays a number.
func TestTCSV31_TheNewProblemTypesRoundTripThrough(t *testing.T) {
	t.Skip("TC-SV-31: SKIP-only stub; un-skip when WI 2.1 lands")
}
