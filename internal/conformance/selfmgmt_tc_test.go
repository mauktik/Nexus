// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package conformance_test

import "testing"

// TC-SM-94 (WI 6.1; NFR-7, FR-1, FR-16): The conformance suite covers nexusd through catalogStubs, including scope coverage
func TestTCSM94_TheConformanceSuiteCoversNexusdThrough(t *testing.T) {
	// - Setup: `catalogStubs[nexusd]` with 7 bindings, including `testprovider.Upload`.
	// - Steps:
	//   1. Run `TestScopeAndAuditCoverEveryOp`.
	//   2. Run `TestAttackStringsAgainstEveryStringParameter`.
	//   3. For each mutating nexusd op and the upload, call with the `exact[nexusd:read]` token.
	// - Expected:
	//   - Steps 1 and 2 pass with nexusd included. The attack strings through `uploadId` and
	//     `signature` get 400 and never reach the spy.
	//   - Step 3 gets 403 with `requiredScope == "nexusd:admin"` for all five.
	t.Skip("TC-SM-94: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-101 (WI 6.1; CON-4): The no-arbitrary-execution surface still holds with nexusd
func TestTCSM101_TheNoArbitraryExecutionSurfaceStill(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. Run `TestNoArbitraryExecutionSurface` with nexusd in the catalog.
	//   2. Read the `forbiddenWords` list.
	// - Expected:
	//   - It passes. No nexusd op ID or route contains `create`, `self` or `selfupdate`, and no input
	//     field contains `command`, `shell`, `exec` or `path`.
	//   - The `forbiddenWords` comment mentions that self-update is in scope through `nexusd`, while
	//     the words stay as guards.
	t.Skip("TC-SM-101: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-ERR59 (WI 6.1; NFR-7, FR-6, FR-1): Conformance negatives on the upload: oversize, wrong scope, unauthenticated, wrong content type
func TestTCSMERR59_ConformanceNegativesOnTheUploadOversize(t *testing.T) {
	// - Setup: the conformance stack with `catalogStubs[nexusd]`.
	// - Steps:
	//   1. POST the nexusd upload four ways:
	//      - over the stub's cap;
	//      - with the `exact[nexusd:read]` token;
	//      - with no token;
	//      - with `Content-Type: application/json`.
	// - Expected:
	//   - Over the cap → 413 `upload-too-large`.
	//   - The read-only token → 403 with `requiredScope: "nexusd:admin"`.
	//   - No token → 401, and the spy shows the body unread.
	//   - The JSON Content-Type → 400 `validation-failed`.
	//   - The spy's `Receive` was never called in any case.
	t.Skip("TC-SM-ERR59: SKIP-only stub; un-skip when WI 6.1 lands")
}
