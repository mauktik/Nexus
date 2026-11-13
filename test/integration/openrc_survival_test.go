//go:build integration

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package integration

import "testing"

// nginx-provider TC-38: rc-service controls nexusd with readiness, reload, bounded stop and respawn
// Owner WI 3.4. Requirements: FR-22, FR-23, ASM-17, L-12, L-13.
//
//	Setup: nexusd installed by install.sh. The config enables the test provider.
//	Steps:
//	  1. rc-service nexusd start.
//	  2. Change a reloadable config value, such as a provider enabled flag, and
//	    run rc-service nexusd reload.
//	  3. kill -9 the nexusd PID.
//	  4. rc-service nexusd stop and time it.
//	  5. Check modes and syslog.
//	Expected:
//	  - Step 1: the pidfile exists and names a live nexusd, and /healthz answers
//	    within 2 s.
//	  - Step 2: the capability change is visible over the API with the same PID,
//	    and syslog has a nexusd reload line.
//	  - Step 3: a new nexusd PID answers within respawn_delay plus 3 s.
//	  - Step 4: it returns within 15 s and no nexusd is left.
//	  - /var/lib/nexus and /var/log/nexus are 0700 root:root, and state.db is
//	    0600.
//	  - /var/log/messages has lines tagged nexusd.
//	  - NOTIFY_SOCKET is unset in /proc/<pid>/environ, and no error mentions
//	    sd_notify.
func TestOpenRCServiceLifecycle(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-38 (WI 3.4)")
}

// nginx-provider TC-39: A detached operation survives rc-service nexusd restart and is finalized from exit.json
// Owner WI 3.4. Requirements: FR-20 (item 1), FR-21, NFR-10.
//
//	Setup: An opsd detached op whose child sleeps 20 s and then writes a marker.
//	Steps:
//	  1. Start the op.
//	  2. Record the supervisor's PID, session ID, PPID and cgroup.
//	  3. rc-service nexusd restart.
//	  4. Read the supervisor's PID, session and PPID again, and wait for the op.
//	Expected:
//	  - The supervisor's PID and startTicks are unchanged across the restart.
//	  - The session equals its own PID.
//	  - Its PPID is no longer the old nexusd.
//	  - The marker exists, and the op ends Succeeded with a duration of at least
//	    20 s.
//	  - The op log has no outcome-unknown.
func TestOpRunsAcrossNexusdRestart(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-39 (WI 3.4)")
}

// nginx-provider TC-40: A supervisor killed while nexusd is down ends outcome-unknown
// Owner WI 3.4. Requirements: FR-21, NFR-10.
//
//	Setup: As TC-39.
//	Steps:
//	  1. Start the op.
//	  2. rc-service nexusd stop.
//	  3. kill -9 the supervisor and its child.
//	  4. rc-service nexusd start.
//	Expected: Within 60 s the op is outcome-unknown, and ListUnits does not list
//	it.
func TestLostSupervisorWhileDownIsOutcomeUnknown(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-40 (WI 3.4)")
}

// nginx-provider TC-41: Cancel ends a script Canceled with no surviving descendant
// Owner WI 3.4. Requirements: FR-20 (item 4), NFR-10.
//
//	Setup: A root-owned catalog script (fixture) that starts setsid sleep 1000
//	&, records its PID, and sleeps.
//	Steps:
//	  1. Invoke the script.
//	  2. Read the grandchild PID.
//	  3. Cancel through the API.
//	Expected:
//	  - The op is Canceled.
//	  - ps -o pid (procps) has no grandchild.
//	  - /sys/fs/cgroup/nexus-ops/<id> is gone or empty.
func TestCancelLeavesNoDescendantOnOpenRC(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-41 (WI 3.4)")
}

// nginx-provider TC-44: apt reports unavailable on Alpine while the other providers answer
// Owner WI 3.4. Requirements: FR-26, NFR-10.
//
//	Setup: Config with apt, scripts and docker enabled.
//	Steps:
//	  1. nexus capability get --output json.
//	  2. Call one apt op.
//	  3. List scripts and containers.
//	Expected:
//	  - apt is unavailable with a reason naming apt and dpkg, and the apt op
//	    answers capability-unavailable.
//	  - scripts and docker are enabled and answer 200.
//	  - validate-config exits 0.
func TestAptUnavailableOnAlpine(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-44 (WI 3.4)")
}

// nginx-provider TC-E14: Stopping nexusd with rc_cgroup_cleanup=YES does not kill a supervisor (harness)
// Owner WI 3.4. Requirements: ASM-14, FR-20 (item 1), L-5.
//
//	Setup: /etc/conf.d/nexusd with rc_cgroup_cleanup="YES". A running op.
//	Steps:
//	  1. rc-service nexusd stop.
//	  2. Check the supervisor.
//	  3. Start nexusd again.
//	Expected:
//	  - The supervisor's PID and startTicks are alive and unchanged, and its
//	    cgroup is nexus-ops/<id>, not openrc.nexusd.
//	  - After step 3 the op completes normally.
//	  - The real-VM confirmation is TC-51 step 3 (L-16).
func TestCgroupCleanupDoesNotKillSupervisor(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E14 (WI 3.4)")
}
