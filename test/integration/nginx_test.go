//go:build integration

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package integration

import "testing"

// nginx-provider TC-106: Lifecycle commanded state equals the init system's observed state
// Owner WI 7.1. Requirements: NFR-6, FR-5, FR-6.
//
//	Setup: Each harness, with distribution nginx installed and started. Ubuntu
//	includeDir: /etc/nginx/conf.d; Alpine default.
//	Steps:
//	  1. For each of stop, start, restart and reload, run it through the CLI.
//	  2. Read back with systemctl is-active nginx or rc-service nginx status,
//	    and with pidof nginx / /proc.
//	Expected:
//	  - The readback matches the commanded state: stopped after stop, running
//	    after start and restart.
//	  - After restart, the master PID changed.
//	  - After reload, the master PID is unchanged and the worker PIDs changed.
//	  - The Nexus result's state and mainPid equal the readback.
func TestNginxLifecycleObserved(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-106 (WI 7.1)")
}

// nginx-provider TC-107: Enable and disable are observed on disk and over HTTP
// Owner WI 7.1. Requirements: NFR-6, FR-9, FR-10.
//
//	Setup: site.conf.tmpl rendered with port 18081 and a marker, installed as
//	t1.conf.disabled by root in the container.
//	Steps:
//	  1. nexus nginxsite start t1, then ls includeDir and curl :18081.
//	  2. nexus nginxsite stop t1 --yes, then ls and curl.
//	Expected:
//	  - Step 1: t1.conf exists and curl returns the marker.
//	  - Step 2: t1.conf.disabled exists and curl fails to connect.
func TestNginxToggleObserved(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-107 (WI 7.1)")
}

// nginx-provider TC-108: A write is observed by hash and over HTTP
// Owner WI 7.1. Requirements: NFR-6, FR-11.
//
//	Setup: Port 18082, marker M1, and new content with marker M2.
//	Steps:
//	  1. Add t2 with M1.
//	  2. Update with M2 and --if-match.
//	  3. sha256sum and curl.
//	Expected:
//	  - sha256sum equals the result's sha256, which equals the hash of the
//	    content sent.
//	  - curl returns M2.
func TestNginxWriteObserved(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-108 (WI 7.1)")
}

// nginx-provider TC-109: Rolled-back writes leave the previous content served
// Owner WI 7.1. Requirements: NFR-3, NFR-6, FR-11.
//
//	Setup: t3 serving M1 on port 18083. nc -l 18090 (BusyBox nc on Alpine) holds
//	port 18090.
//	Steps:
//	  1. Update t3 with invalid.conf content.
//	  2. Update t3 with port-conflict.conf content (listens on 18090).
//	  3. After each, read includeDir hashes, ls -ln and curl :18083.
//	Expected:
//	  - Step 1 fails nginx-config-invalid with rolledBack: true. Step 2 fails
//	    nginx-reload-failed with rolledBack: true, rollbackReload confirmed, and
//	    observed.verdict: rejected.
//	  - After each, the hashes, names, modes and owners equal the pre-op
//	    snapshot, and curl returns M1.
//	  - ASM-7 on the real host: step 2's observed.initExitCode is recorded in
//	    the log, whatever it is.
func TestNginxRollbackObserved(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-109 (WI 7.1)")
}

// nginx-provider TC-110: Writes across a restart, with a killed supervisor, and queued across a restart
// Owner WI 7.1. Requirements: NFR-3, L-40, FR-15.
//
//	Setup: An nginx wrapper is not used. To widen the window, the test holds the
//	lane with a running test op whose nginx -t is slowed by an includeDir file
//	include /harness/fixtures/nginx/slow/*.conf; of 2,000 entries, or, if that
//	is too fast, the test uses the TC-115 wrapper for this case only.
//	Steps:
//	  1. Case A: start an update, wait for previous/meta.json, then systemctl
//	    restart nexusd or rc-service nexusd restart.
//	  2. Case B: start an update, wait for previous/meta.json, then kill -9 the
//	    supervisor.
//	  3. Case C: queue an update behind a running op, then restart nexusd.
//	Expected:
//	  - A: Succeeded, with the new hash on disk.
//	  - B: outcome-unknown. The op log contains previous content of t4.conf is
//	    kept at /var/lib/nexus/ops/<id>/previous/content, and that file's hash
//	    equals the pre-op hash.
//	  - C: outcome-unknown never started, with includeDir hashes unchanged.
func TestNginxWriteRestartCases(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-110 (WI 7.1)")
}

// nginx-provider TC-111: whatIf leaves the mount table and includeDir unchanged
// Owner WI 7.1. Requirements: FR-11, ASM-10.
//
//	Setup: t5 enabled.
//	Steps:
//	  1. Read /proc/1/mountinfo and the includeDir hashes.
//	  2. Update with whatIf (valid), then with whatIf (invalid).
//	  3. Read both again.
//	Expected:
//	  - The valid field matches the verdict of a real nginx -t run manually on a
//	    copy.
//	  - The mountinfo is byte-identical and the hashes are identical.
//	  - nginx's master PID and worker PIDs are unchanged (no reload).
func TestNginxWhatIfObserved(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-111 (WI 7.1)")
}

// nginx-provider TC-112: Every nginx op runs under the exact hardened unit with no denial
// Owner WI 7.1. Requirements: CON-2, NFR-9, NFR-6, L-22.
//
//	Setup: requireExactUnit.
//	Steps:
//	  1. Call every nginx op.
//	  2. Read the journal for nexusd.service and the nexus-op-* units.
//	Expected:
//	  - nginx.actions.test returns valid: true for the stock config, proving -t
//	    ran outside the sandbox.
//	  - Status version is non-empty and matches nginx -v run as root (L-22).
//	  - There is no SECCOMP, audit: type=1326, Permission denied or Read-only
//	    file system line in the window.
func TestNginxUnderExactUnit(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-112 (WI 7.1)")
}

// nginx-provider TC-113: The fakes match the real init systems and nginx
// Owner WI 7.1. Requirements: ASM-7, NFR-6, CON-2.
//
//	Setup: The fake's scenario table is exported from svcctltest as Scenarios()
//	and embedded in the integration binary.
//	Steps:
//	  1. For each scenario (reload with an invalid config, reload with a bound
//	    port, reload valid, start when started, status of a stopped service,
//	    --exists / show of an unknown service), set up the real host state and run
//	    the real command.
//	  2. Record exit code, stdout and stderr.
//	  3. Run nginx -t with the error-log directory made read-only through a
//	    read-only bind mount over /var/log/nginx (in a private mount namespace
//	    through unshare -m).
//	Expected:
//	  - Each real exit code equals the fake's for that init system.
//	  - Each real output contains the fake's key phrase.
//	  - Step 3 fails with the error-log text the fakenginx README lists.
//	  - Any mismatch fails the test, printing both, so the fake must be fixed
//	    before the result is trusted.
//	  - The transcript is copied to docs/verification/nginx-t1.md.
func TestFakesMatchRealBehavior(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-113 (WI 7.1)")
}

// nginx-provider TC-114: Latency and footprint with real nginx
// Owner WI 7.1. Requirements: NFR-1, NFR-5.
//
//	Setup: 50 sites in includeDir.
//	Steps:
//	  1. 200 requests each of status, list and get.
//	  2. Time the 202 response of a test op.
//	  3. Measure idle RSS with nginx disabled, then enabled after a 60 s idle
//	    period each.
//	Expected:
//	  - p95 is under 200 ms for each read, and the 202 under 500 ms.
//	  - The RSS delta is under 5 MiB.
//	  - nexusd's thread count and CPU time are flat over the idle window.
func TestNginxLatencyAndFootprint(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-114 (WI 7.1)")
}

// nginx-provider TC-115: A broken or hung nginx affects only nginx operations
// Owner WI 7.1. Requirements: NFR-4, NFR-2.
//
//	Setup: Three states:
//	  - nginx stopped;
//	  - nginx config broken (invalid.conf placed by root);
//	  - /usr/sbin/nginx replaced by a wrapper that sleeps forever on -t and
//	    execs the real binary otherwise (restored in cleanup, with the hash
//	    checked).
//	Steps:
//	  1. In each state, call the nginx test op and status, plus scripts list,
//	    docker list and capabilities.
//	Expected:
//	  - Stopped: status is 200 stopped.
//	  - Broken: the test op gives valid: false.
//	  - Hung: the test op fails dependency-unavailable after testTimeout (set to
//	    5 s in this test's config).
//	  - In every state the other providers answer 200 within 200 ms, and the
//	    nginx provider's capability state does not flip because of a hung -t.
func TestNginxIsolation(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-115 (WI 7.1)")
}

// nginx-provider TC-116: nginx started through Nexus survives the operation's end
// Owner WI 7.1. Requirements: FR-5, FR-20, L-41.
//
//	Setup: nginx stopped.
//	Steps:
//	  1. nexus nginx start.
//	  2. Wait for Succeeded, then 5 s.
//	  3. Read the master PID, /proc/<pid>/cgroup, and rc-service nginx status or
//	    systemctl is-active.
//	Expected:
//	  - The master is alive and the init system reports it started.
//	  - The cgroup path is recorded in the log. On OpenRC, it is not under
//	    nexus-ops/ (L-41); if it is, the test fails, citing L-41.
//	  - nexus-ops/<id> does not exist after the op.
func TestNginxSurvivesOpEnd(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-116 (WI 7.1)")
}

// nginx-provider TC-117: Real root ownership decides managed
// Owner WI 7.1. Requirements: FR-7, ASM-11.
//
//	Setup: own.conf root:root and user.conf owned by nobody.
//	Steps:
//	  1. List the sites.
//	  2. Enable user.
//	Expected:
//	  - own is managed. user is managed: false with unmanagedReason: not-root-
//	    owned.
//	  - Enabling user is refused with nginx-config-unmanaged, and nothing
//	    changes.
func TestNginxManagedRequiresRootOwner(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-117 (WI 7.1)")
}
