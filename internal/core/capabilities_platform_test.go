// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package core_test

import "testing"

// nginx-provider TC-4: Capabilities report os and initSystem, and follow the injected platform
// Owner WI 1.2. Requirements: FR-18.
//
//	Setup: Two platform.Info values:
//	  - A: {Init: OpenRC, OS: {ID: alpine, VersionID: 3.21.2, PrettyName:
//	    "Alpine Linux v3.21"}};
//	  - B: {Init: Systemd, OS: {ID: ubuntu, VersionID: 24.04}}.
//	Steps:
//	  1. Build a core registry with Options.Platform = A.
//	  2. GET /v1/capabilities through the real server.
//	  3. Build a second registry with B and GET again.
//	  4. Run nexus capability get --output json against a real nexusd started by
//	    daemon.New with Options.Platform = &A.
//	Expected:
//	  - Step 2 JSON has os.id == "alpine", os.versionId == "3.21.2",
//	    os.prettyName == "Alpine Linux v3.21" and initSystem == "openrc".
//	  - Step 3 has ubuntu and systemd, so neither result carries the other's
//	    value.
//	  - The CLI JSON carries the same fields as step 2.
//	  - The OpenAPI schema for Capabilities lists os and initSystem as required,
//	    and docs/api.md is current (make docs-check).
func TestCapabilitiesReportOSAndInitSystem(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-4 (WI 1.2)")
}
