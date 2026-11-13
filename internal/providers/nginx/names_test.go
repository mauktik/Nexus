// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package nginx

import "testing"

// nginx-provider TC-71: The FR-12 name rule
// Owner WI 4.5. Requirements: FR-12, CON-4.
//
//	Setup: Settings .conf and .disabled.
//	Steps:
//	  1. Check each name in the table.
//	Expected:
//	  - Accepted: a, example.com, my_site-2, A1, and a name of 128 characters.
//	  - Refused:
//	    - "", .hidden, -x, _x;
//	    - a..b;
//	    - a name of 129 characters;
//	    - a/b, a b, a\x00b, é;
//	    - example.com.conf (ends in the include suffix), x.disabled,
//	      x.conf.disabled;
//	    - .., ..
//	  - Each refusal error names the rule it broke.
//	  - With settings .site and .off, x.conf is accepted and x.site refused.
func TestValidName(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-71 (WI 4.5)")
}
