// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package config

import "testing"

// nginx-provider TC-54: The nginx config section defaults and default YAML
// Owner WI 4.2. Requirements: FR-1, FR-2, ASM-3.
//
//	Setup: config.Default(), plus a load of deploy/nexusd.default.yaml.
//	Steps:
//	  1. Read Providers.Nginx.
//	  2. Load the YAML.
//	  3. Inspect providerConfigs in internal/daemon/providers.go.
//	Expected:
//	  - Step 1:
//	    - Enabled == false;
//	    - IncludeDir == "";
//	    - IncludeSuffix == ".conf";
//	    - DisabledSuffix == ".disabled";
//	    - Service == "nginx";
//	    - TestTimeout, StateTimeout and ReloadVerifyTimeout are 30s, 30s and
//	      15s;
//	    - MaxConfigBytes == 262144.
//	  - The YAML has an nginx: section with every key and its default, a comment
//	    per key, and a comment containing include and *.conf.
//	  - The existing TestDefaultConfigMatchesDefaultsAndHasEveryKey passes with
//	    the new section.
//	  - providerConfigs contains "nginx".
func TestNginxSectionDefaults(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-54 (WI 4.2)")
}

// nginx-provider TC-E15: Config boundary values
// Owner WI 4.2. Requirements: FR-2, L-17.
//
//	Setup: limits.maxBodyBytes default.
//	Steps:
//	  1. Validate:
//	     - maxConfigBytes == limits.maxBodyBytes;
//	     - == limits.maxBodyBytes + 1;
//	     - disabledSuffix: .off;
//	     - includeSuffix: .site with disabledSuffix: .disabled;
//	     - testTimeout: 1ns.
//	Expected:
//	  - Accepted, refused, accepted, accepted, accepted.
//	  - The refusal names providers.nginx.maxConfigBytes with its line and
//	    column.
func TestNginxConfigBoundaries(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E15 (WI 4.2)")
}
