// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package catalog_test

import "testing"

// nginx-provider TC-28: Provider descriptions no longer claim systemd, and the docs are regenerated
// Owner WI 2.4. Requirements: FR-26.
//
//	Setup: catalog.All().
//	Steps:
//	  1. Scan every op's Description and MCP description.
//	Expected:
//	  - No description contains systemd unit without also containing OpenRC.
//	  - The reworded sentence from WI 2.4 is present on the scripts run and
//	    docker update ops.
//	  - go run ./cmd/nexus-gendocs -dir docs -check exits 0.
//	---
//	## Phase 3: OpenRC deployment, documentation and Alpine verification
func TestDescriptionsAreInitNeutral(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-28 (WI 2.4)")
}
