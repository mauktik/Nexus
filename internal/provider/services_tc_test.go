// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-7, TC-SV-ERR5.
// Owner WIs: 1.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package provider

import "testing"

// TC-SV-7: Detached accepts a declaring op with AfterReboot, and the type-erased hook decodes and returns. Owner WI 1.2. Requirements: FR-11, CON-4.
//
// - Setup: The TC-SV-1 op with `In = struct{ Delay int }` and `Out = struct{ Prev, Cur string }`. `AfterReboot` returns `Out{rb.PreviousBootID, rb.BootID}` and records the `In` it got.
// - Steps:
//  1. `provider.Detached(op, provider.DetachedSpec[In,Out]{…, AfterReboot: f})`.
//  2. Call the binding's erased `AfterReboot` with the input `{"delay":7}` (decoded as the binding decodes inputs), `provider.Rebooted{PreviousBootID: PREV, BootID: CUR}` and a `*provider.Run`.
//
// - Expected: No panic. The erased hook is non-nil. `f` received `In{Delay: 7}`, and the result is `Out{PREV, CUR}` as `any`. A binding of an op without the flag and without `AfterReboot` has a nil erased hook.
func TestTCSV7_DetachedAcceptsADeclaringOpWithAfterReboot(t *testing.T) {
	t.Skip("TC-SV-7: SKIP-only stub; un-skip when WI 1.2 lands")
}

// TC-SV-ERR5: Detached panics on a declaration and hook mismatch. Owner WI 1.2. Requirements: FR-11, CON-4.
//
// - Setup: (a) the TC-SV-1 op with no `AfterReboot`; (b) an op without the flag but with `AfterReboot` set.
// - Steps:
//  1. `provider.Detached` inside `recover`.
//
// - Expected: Both panic, naming the op ID, `CompletesAcrossReboot` and `AfterReboot`.
func TestTCSVERR5_DetachedPanicsOnADeclarationAndHook(t *testing.T) {
	t.Skip("TC-SV-ERR5: SKIP-only stub; un-skip when WI 1.2 lands")
}
