// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package nginx

import "testing"

// nginx-provider TC-61: Preflight reports nginx available with details when every condition holds
// Owner WI 4.5. Requirements: FR-2a, FR-1.
//
//	Setup: New(Options{Nginx: <fakenginx path>, Services: svcctltest systemd
//	(nginx running), Detach: available, Platform: ubuntu}), configured with a
//	temp includeDir.
//	Steps:
//	  1. Preflight(ctx).
//	Expected:
//	  - State == enabled.
//	  - Details are {initSystem: systemd, service: nginx, includeDir: <tmp>,
//	    includeSuffix: .conf, disabledSuffix: .disabled, version: "1.26.3"}, with
//	    the version parsed from fakenginx's -v output.
func TestPreflightAvailable(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-61 (WI 4.5)")
}

// nginx-provider TC-62: A SIGHUP reload re-runs preflight and picks up nginx installed later
// Owner WI 4.5. Requirements: FR-2a.
//
//	Setup: daemon.New with nginx enabled, an Nginx path that does not exist yet,
//	and the svcctl fake answering Known: false.
//	Steps:
//	  1. GET capabilities.
//	  2. Call nginx.service.list.
//	  3. Copy fakenginx to the path and make the svcctl fake Known: true.
//	  4. Trigger the daemon's reload path (the same function SIGHUP calls, as in
//	    internal/daemon/reload_test.go).
//	  5. GET capabilities and call status again.
//	Expected:
//	  - Step 1: nginx is unavailable with a reason naming /usr/sbin/nginx (or
//	    the injected path).
//	  - Step 2: capability-unavailable.
//	  - Step 5: nginx is enabled and status answers 200.
//	  - The other providers' state is unchanged throughout.
func TestPreflightRecheckedOnReload(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-62 (WI 4.5)")
}

// nginx-provider TC-72: The effective includeDir follows the distribution (worked example)
// Owner WI 4.5. Requirements: FR-2, ASM-3, ASM-16, L-2.
//
//	Setup: Platform Alpine, Ubuntu and Debian-with-OpenRC (TC-3), each with
//	includeDir: "". Then Alpine with includeDir: /srv/nginx.
//	Steps:
//	  1. Configure.
//	  2. Read the effective settings and status.
//	Expected:
//	  - Alpine → /etc/nginx/http.d, so the site example.com is
//	    /etc/nginx/http.d/example.com.conf, and when disabled
//	    /etc/nginx/http.d/example.com.conf.disabled.
//	  - Ubuntu → /etc/nginx/conf.d. Debian with OpenRC → /etc/nginx/conf.d.
//	  - The explicit value → /srv/nginx.
//	  - The stored config section still has includeDir: "", because the value is
//	    not written back.
func TestEffectiveIncludeDir(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-72 (WI 4.5)")
}

// nginx-provider TC-ERR9: Preflight names exactly what is missing
// Owner WI 4.5. Requirements: FR-2a, NFR-4.
//
//	Setup: One case per condition:
//	  - the binary is missing;
//	  - the binary is not executable (0644);
//	  - the binary is a directory;
//	  - detach is unavailable (reason R);
//	  - the svcctl fake answers Known: false;
//	  - includeDir is missing;
//	  - includeDir is a regular file;
//	  - includeDir is a symlink to a real dir.
//	Steps:
//	  1. Preflight.
//	  2. Call status through the server.
//	Expected:
//	  - Each case is unavailable, with a reason containing, in order:
//	    - the binary path and missing;
//	    - not executable;
//	    - not executable;
//	    - R;
//	    - nginx and not known to <init>;
//	    - the includeDir path and does not exist;
//	    - not a directory;
//	    - symlink.
//	  - Status answers capability-unavailable with the same reason.
//	  - A second, scripts-like stub provider in the same daemon stays enabled.
func TestPreflightFailures(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR9 (WI 4.5)")
}
