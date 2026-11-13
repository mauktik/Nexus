// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package spec

import "testing"

// nginx-provider TC-60: CLI grammar for nginx, including prompts and the worked examples
// Owner WI 4.4. Requirements: FR-16, FR-5, FR-10, ASM-4, ASM-9.
//
//	Setup: A real nexusd with the conformance stub provider, and the nexus
//	binary. expect is used for the prompt.
//	Steps:
//	  1. nexus nginx --help and nexus nginxsite --help.
//	  2. nexus nginxsite stop example.com with a TTY and no --yes, answering n.
//	  3. The same with --yes.
//	  4. nexus nginxsite update example.com --content "x" --if-match <64-hex>
//	    with no TTY.
//	  5. nexus nginx reload.
//	  6. echo '{"name":"a"}' | nexus nginxsite get --stdin.
//	Expected:
//	  - Help lists get, start, stop, restart and reload for nginx; get, start,
//	    stop, add and update for nginxsite; test for nginxconfig; and get for
//	    nginxsitecontent.
//	  - Step 2 prompts, and n leaves the stub uncalled. Step 3 calls it.
//	  - Step 4 calls the stub with no prompt, passing ifMatch.
//	  - Step 5 returns an operation ID.
//	  - Step 6 works.
//	  - grep -r nginx internal/cli finds nothing.
func TestNginxCLIGrammar(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-60 (WI 4.4)")
}
