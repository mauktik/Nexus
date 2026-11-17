// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-ERR42.
// Owner WIs: 1.3. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package provider

import "testing"

// TC-RA-ERR42: provider.Detached panics on Admit plus Admission, and on CompletesAtAdmission without Admission. Owner WI 1.3. Requirements: CON-3.
//
//   - Setup: TC-RA-110's op C and an op without the flag.
//   - Steps:
//     1. `provider.Detached(C, {Admit: f, Admission: g, ...})` inside a `recover`.
//     2. `provider.Detached(C, {Admit: f, ...})`, with no `Admission`.
//     3. `provider.Detached(opWithoutFlag, {Admission: g, ...})`.
//   - Expected:
//   - Steps 1 and 2 panic, each naming the op ID and the rule.
//   - Step 3 does not panic: a hook on an undeclared op is allowed, and a `Done` from it is refused at run time (TC-RA-ERR41).
//   - The erased `DetachedFuncs.Admission` is nil for a spec without `Admission`.
func TestTCRAERR42_ProviderDetachedPanicsOnAdmitPlusAdmissionAnd(t *testing.T) {
	t.Skip("TC-RA-ERR42: SKIP-only stub; un-skip when WI 1.3 lands")
}
