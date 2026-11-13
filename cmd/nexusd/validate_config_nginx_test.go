// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package main

import "testing"

// nginx-provider TC-ERR8: validate-config refuses invalid nginx settings with locations
// Owner WI 4.2. Requirements: FR-1, FR-2, CON-4.
//
//	Setup: A config file with all of these:
//	  - includeDir: etc/nginx (relative);
//	  - includeSuffix: conf;
//	  - disabledSuffix: .conf (ends in the include suffix);
//	  - service: "nginx;x";
//	  - testTimeout: 0s;
//	  - stateTimeout: -1s;
//	  - maxConfigBytes: 0;
//	  - an unknown key includeGlob.
//	  Separately: includeSuffix: ".co/nf", includeSuffix: "", disabledSuffix: ""
//	  and disabledSuffix: "-off".
//	Steps:
//	  1. nexusd validate-config --config f.
//	Expected:
//	  - Non-zero exit.
//	  - Every error is reported in one run, each naming providers.nginx.<key>
//	    with a line and column.
//	  - The unknown key error names includeGlob.
//	  - The server does not start.
func TestValidateConfigNginxRefusals(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR8 (WI 4.2)")
}
