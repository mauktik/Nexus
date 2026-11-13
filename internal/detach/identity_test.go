//go:build linux

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package detach

import "testing"

// nginx-provider TC-E7: A forged identity with a reused PID is not live and cannot be signaled
// Owner WI 2.1. Requirements: FR-20 (item 2), L-3.
//
//	Setup: Linux. Start sleep 30 as an unrelated process. Write supervisor.json
//	with its PID, the correct bootId, and startTicks + 1.
//	Steps:
//	  1. UnitState.
//	  2. Signal(SIGTERM).
//	Expected:
//	  - Absent.
//	  - Signal returns an error naming the identity mismatch, and the sleep is
//	    still alive (kill(pid,0) == nil).
func TestForgedStartTicksIsAbsentAndUnsignalable(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E7 (WI 2.1)")
}

// nginx-provider TC-E8: An identity from another boot is never live
// Owner WI 2.1. Requirements: FR-20 (item 2), FR-21.
//
//	Setup: Linux. A live process's exact PID and startTicks, with bootId set to
//	a different UUID.
//	Steps:
//	  1. UnitState.
//	Expected: Absent.
func TestOtherBootIsAbsent(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E8 (WI 2.1)")
}

// nginx-provider TC-E9: A zombie supervisor is ended
// Owner WI 2.1. Requirements: FR-20, L-4, L-6.
//
//	Setup: Linux. A child that exits, with the test deliberately not reaping it,
//	so its state is Z. The identity matches.
//	Steps:
//	  1. UnitState.
//	Expected: Absent.
func TestZombieIsAbsent(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E9 (WI 2.1)")
}

// nginx-provider TC-E10: /proc stat parsing survives hostile comm values
// Owner WI 2.1. Requirements: FR-20 (item 2), L-3.
//
//	Setup: Captured stat lines with comm (nexusd), (a) (b c), (), and a line
//	truncated before field 22.
//	Steps:
//	  1. Parse each.
//	Expected:
//	  - State and startTicks match fields 3 and 22, counted after the last ),
//	    for each well-formed line.
//	  - The truncated line gives an error, not a zero value.
func TestProcStatParse(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E10 (WI 2.1)")
}
