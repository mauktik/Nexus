// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package spec

import "testing"

// nginx-provider TC-56: The 13 nginx operations are declared exactly as the operations table
// Owner WI 4.4. Requirements: FR-1, FR-3 to FR-11, FR-14, FR-16, CON-3.
//
//	Setup: A table in the test copied from the architecture's operations table
//	(operationId, kind, route, scope, risk, mode, CLI noun and verb, MCP tool).
//	Steps:
//	  1. spec.Spec().
//	  2. Compare each op.
//	  3. Run TestCatalogMatchesGoldenTable.
//	Expected:
//	  - The 13 ops match field for field.
//	  - Every detached op is ModeLongRunning, lane nginx and Cancel: queued-
//	    only.
//	  - nginx.actions.test is read-only and Untrusted. The reads of sites are
//	    Untrusted.
//	  - stop and disable are destructive. start, stop, enable and disable are
//	    idempotent.
//	  - The golden table includes the 13 rows.
//	  - catalog.All() includes nginx once (TestAllListsUniqueOps).
func TestOperationsMatchTable(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-56 (WI 4.4)")
}

// nginx-provider TC-57: Input schemas constrain names, ifMatch, cursor and content, and mark content sensitive
// Owner WI 4.4. Requirements: FR-12, FR-15, ASM-9, CON-4.
//
//	Setup: Generated JSON schemas of every op input.
//	Steps:
//	  1. Inspect the name, ifMatch, cursor and content properties.
//	  2. Validate sample inputs through the schema validator.
//	Expected:
//	  - name has pattern ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ and rejects a..b.
//	  - ifMatch pattern accepts 64 lower-case hex characters and none, and
//	    rejects 64 upper-case hex characters and *.
//	  - content is marked free text with maxLength 1048576.
//	  - add has no ifMatch property.
//	  - add and update have Sensitive == ["content"] and WhatIf == true.
//	  - No input of any op has a property named path, service, command, binary
//	    or includeDir.
func TestInputSchemaRefinements(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-57 (WI 4.4)")
}

// nginx-provider TC-59: MCP tools stay within budget and carry the right hints
// Owner WI 4.4. Requirements: NFR-7, FR-16.
//
//	Setup: The full catalog.
//	Steps:
//	  1. go test ./internal/mcpadapter -run TestToolBudget -v.
//	  2. List the tools.
//	Expected:
//	  - The budget passes, and the log shows 37 tools.
//	  - stop_nginx and disable_nginx_site have destructiveHint: true.
//	  - get_nginx_status, list_nginx_sites, get_nginx_site,
//	    get_nginx_site_content and test_nginx_config have readOnlyHint: true.
//	  - Each Description of add and update contains root-equivalent, and each
//	    long-running op's description contains operation ID.
//	  - make docs-check passes. If the budget limit changed, docs/mcp.md records
//	    the new limit and the reason.
func TestMCPHints(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-59 (WI 4.4)")
}
