// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package main

import "testing"

// TC-SM-61 (WI 4.1; CON-2): The hidden service-helper command is dispatched by cmd/nexusd
func TestTCSM61_TheHiddenServiceHelperCommandIs(t *testing.T) {
	// - Setup: a temp dir with `input.json {mode: "bogus"}`.
	// - Steps:
	//   1. Run in-process `run(["service-helper", "--dir", D])`.
	//   2. Run `nexusd -h`.
	// - Expected:
	//   - Step 1: exit 1, stderr contains `unknown mode "bogus"`, and no `result.json` is written.
	//   - Step 2: the usage output does not mention `service-helper`.
	t.Skip("TC-SM-61: SKIP-only stub; un-skip when WI 4.1 lands")
}
