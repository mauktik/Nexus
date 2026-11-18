// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-1, TC-SV-2, TC-SV-3, TC-SV-ERR1, TC-SV-ERR2, TC-SV-ERR3.
// Owner WIs: 1.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package def

import "testing"

// TC-SV-1: CompletesAcrossReboot is accepted on a long-running destructive provider action with a reconnect window. Owner WI 1.1. Requirements: FR-11, CON-4.
//
// - Setup: A planted op `pl.actions.reboot`: `ModeLongRunning`, `RiskDestructive`, `KindProviderAction`, `CLI.ReconnectWindow: 10*time.Minute`, `CompletesAcrossReboot: true`. A second one is the same with `RiskMutating` and `KindResourceAction`.
// - Steps:
//  1. `def.Define` each op inside a `recover`.
//
// - Expected: Neither panics. `op.CompletesAcrossReboot` is true on the returned value. `CLI.ReconnectWindow` of exactly `10m` (the maximum) is accepted.
func TestTCSV1_CompletesAcrossRebootIsAcceptedOnALongRunning(t *testing.T) {
	t.Skip("TC-SV-1: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SV-2: TypedConfirmation is accepted on a required, non-positional string field of a mutating op. Owner WI 1.1. Requirements: FR-10, FR-13, FR-15, CON-4.
//
// - Setup: A planted destructive long-running op whose input has a string field with JSON tag `confirmHost` (no `omitempty`) and a bool field with tag `force,omitempty`, declared with `CLI.TypedConfirmation: "confirmHost"`.
// - Steps:
//  1. `def.Define` inside a `recover`.
//
// - Expected: No panic. The generated JSON schema lists `confirmHost` as required.
func TestTCSV2_TypedConfirmationIsAcceptedOnARequiredNon(t *testing.T) {
	t.Skip("TC-SV-2: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SV-3: Audited is accepted on a read-only stream and still on a read-only sync op. Owner WI 1.1. Requirements: FR-6, FR-16, CON-4.
//
// - Setup: Planted ops: `RiskReadOnly` + `ModeStream` + `Audited`; `RiskReadOnly` + `ModeSync` + `Audited` (the nexusd-remote-admin precedent).
// - Steps:
//  1. `def.Define` each inside a `recover`.
//
// - Expected: Neither panics.
func TestTCSV3_AuditedIsAcceptedOnAReadOnly(t *testing.T) {
	t.Skip("TC-SV-3: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SV-ERR1: CompletesAcrossReboot refusals name their rule. Owner WI 1.1. Requirements: FR-11, CON-4.
//
// - Setup: The TC-SV-1 op, varied one field at a time.
// - Steps:
//  1. `def.Define` each inside `recover`.
//
// - Expected: Each of these panics, with text naming `CompletesAcrossReboot` and the rule:
//   - `ModeSync`;
//   - `ModeStream`;
//   - `RiskReadOnly`;
//   - `KindList`, `KindGet` and the subresource kind;
//   - `CLI.ReconnectWindow: 0`;
//   - `SyncMutation: true`;
//   - `CompletesAtAdmission: true`.
func TestTCSVERR1_CompletesAcrossRebootRefusalsNameTheirRule(t *testing.T) {
	t.Skip("TC-SV-ERR1: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SV-ERR2: TypedConfirmation refusals name their rule. Owner WI 1.1. Requirements: FR-10, CON-4.
//
// - Setup: The TC-SV-2 op, varied.
// - Steps:
//  1. Define with each of:
//     - `TypedConfirmation: "nope"`;
//     - naming the bool field `force`;
//     - naming a string field with `omitempty`;
//     - naming a field that is `Positional`;
//     - on a `RiskReadOnly` op.
//
// - Expected: Each panics, naming `TypedConfirmation` and the rule (unknown field, not a string, not required, positional, not mutating).
func TestTCSVERR2_TypedConfirmationRefusalsNameTheirRule(t *testing.T) {
	t.Skip("TC-SV-ERR2: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SV-ERR3: Audited on a mutating stream or a long-running op is refused. Owner WI 1.1. Requirements: FR-16, CON-4.
//
// - Setup: `Audited` on (a) `ModeStream` + `RiskMutating` and (b) `ModeLongRunning` + `RiskReadOnly`.
// - Steps:
//  1. Define each.
//
// - Expected:
//   - (a) panics with `Audited requires RiskReadOnly`;
//   - (b) panics with `Audited requires ModeSync or ModeStream`.
func TestTCSVERR3_AuditedOnAMutatingStreamOrA(t *testing.T) {
	t.Skip("TC-SV-ERR3: SKIP-only stub; un-skip when WI 1.1 lands")
}
