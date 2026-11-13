// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package daemon

import "testing"

// nginx-provider TC-23: daemon.New on OpenRC launches through the process backend
// Owner WI 2.3. Requirements: FR-20, FR-21, CON-11.
//
//	Setup: Linux. daemon.New with Options.Platform = &platform.Info{Init:
//	OpenRC}, no injected Detach.Launch, a temp state dir,
//	ProcessConfig.CgroupRoot "-" through the test-only option, and the test
//	provider with one detached op.
//	Steps:
//	  1. Start the op through HTTP.
//	  2. Read ops/<id>/supervisor.json and the PID's cmdline.
//	Expected:
//	  - supervisor.json exists.
//	  - cmdline contains --process.
//	  - The op reaches Succeeded.
//	  - No process with argv[0] /usr/bin/systemd-run ran. The test sets PATH to
//	    an empty dir and checks that no systemd-run marker appears. Absolute paths
//	    would fail anyway on the dev host, so the check is for ENOENT in the
//	    daemon log. The completed op is the real proof.
func TestDaemonOnOpenRCUsesProcessBackend(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-23 (WI 2.3)")
}

// nginx-provider TC-26: Restart recovery classifies process-backend records as on systemd
// Owner WI 2.3. Requirements: FR-21, NFR-8.
//
//	Setup: Linux. Four Running records on lane t:
//	  - A: live supervisor (child sleeps 3 s);
//	  - B: exited with exit.json code 0;
//	  - C: supervisor gone, no exit.json;
//	  - D: identity with a forged bootId.
//	Steps:
//	  1. Start daemon.New on those records with the process backend.
//	  2. Wait.
//	  3. Repeat the setup with detachtest.Local and compare the lane and queue
//	    snapshot.
//	Expected:
//	  - A is reattached and ends Succeeded, B ends Succeeded from exit.json, and
//	    C and D end outcome-unknown, all within 60 s.
//	  - The lane and queue state after recovery is identical between the two
//	    backends.
func TestRecoveryWithProcessBackend(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-26 (WI 2.3)")
}

// nginx-provider TC-ERR6: On an unknown init system nexusd serves reads and refuses detached work
// Owner WI 2.4. Requirements: FR-18, FR-26.
//
//	Setup: daemon.New with Platform Unknown, the real availability, and scripts
//	and docker enabled (fake socket).
//	Steps:
//	  1. GET capabilities.
//	  2. Invoke a script.
//	  3. List scripts.
//	Expected:
//	  - Capabilities show initSystem: unknown, and scripts unavailable with a
//	    reason naming no supported init system (systemd or OpenRC).
//	  - The invoke gives capability-unavailable naming the same.
//	  - nexusd is serving: /healthz is 200.
func TestUnknownInitRefusesDetachedWork(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR6 (WI 2.4)")
}
