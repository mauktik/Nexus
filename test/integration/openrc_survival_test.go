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
// Owner WI 3.4. Requirements: FR-22, FR-23, ASM-17, L-11, L-12, L-13.
//
//	Setup: nexusd installed by install.sh. The config enables the test
//	provider.
//	Steps:
//	  1. rc-service nexusd start. Find the answering nexusd as the single PID
//	     from pgrep -x nexusd (the test fails if there is not exactly one),
//	     and read the pidfile.
//	  2. Change a reloadable config value, such as a provider enabled flag,
//	     and run rc-service nexusd reload.
//	  3. kill -9 the answering nexusd PID from step 1 (not the pidfile's PID,
//	     which may be the supervisor).
//	  4. rc-service nexusd stop and time it.
//	  5. Check modes and syslog.
//	Expected:
//	  - Step 1: the pidfile exists and names a live process that is either:
//	    - the answering nexusd itself (/proc/<pid>/comm is nexusd), or
//	    - a supervise-daemon (/proc/<pid>/comm is supervise-daemon) whose
//	      child is the answering nexusd (that nexusd's PPid: in
//	      /proc/<nexusd>/status equals the pidfile PID).
//	    Any other process fails the test. The test logs which of the two it
//	    found (pidfile names: nexusd or pidfile names: supervise-daemon), and
//	    the harness log line is the real-host evidence for ledger row L-11 (WI
//	    3.1's real-device exchange). /healthz answers within 2 s.
//	  - Step 2: the capability change is visible over the API with the same
//	    PID, and syslog has a nexusd reload line.
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
// Owner WI 3.4. Requirements: ASM-14, FR-20 (item 1), L-5, L-52, CON-10.
//
//	Setup: The installed /etc/init.d/nexusd (which assigns
//	rc_cgroup_cleanup="${rc_cgroup_cleanup:-NO}", TC-29). A canary helper:
//	read nexusd's cgroup from the 0:: line of /proc/<nexusd>/cgroup (never a
//	hard-coded openrc.nexusd path), start sh -c 'echo $$ >
//	/sys/fs/cgroup<path>/cgroup.procs; exec sleep 1000' &, and assert the
//	canary's /proc/<canary>/cgroup equals nexusd's before going on.
//	Steps:
//	  1. Case A, control (default): no rc_cgroup_cleanup line in
//	     /etc/conf.d/nexusd. rc-service nexusd start, place a canary,
//	     rc-service nexusd stop.
//	  2. Case B: write rc_cgroup_cleanup="YES" to /etc/conf.d/nexusd.
//	     rc-service nexusd start, place a canary, start an opsd op whose child
//	     sleeps 20 s and then writes a marker, and record the supervisor's
//	     PID, startTicks and cgroup. Then rc-service nexusd stop.
//	  3. Case B: check the canary, then the supervisor.
//	  4. Case B: start nexusd again and wait for the op.
//	  5. t.Cleanup removes the conf.d line and kills any canary left.
//	Expected:
//	  - Case A: the canary is still alive 5 s after the stop. The default is
//	    NO, so OpenRC did not clean up.
//	  - Case B, in this order:
//	    1. The canary is gone within 5 s of the stop. This proves the
//	       operator's YES took effect. If the canary is alive, the test fails
//	       with "rc_cgroup_cleanup=YES not in effect; supervisor survival
//	       would be vacuous", and does not go on to the supervisor assertion.
//	    2. Only then: the supervisor's PID and startTicks are alive and
//	       unchanged, and its cgroup is nexus-ops/<id>, not nexusd's cgroup.
//	    3. After step 4 the op ends Succeeded, the marker exists, and the op
//	       log has no outcome-unknown.
//	  - The real-VM confirmation is TC-51 step 3 (L-16).
//	Out of scope (no writable cgroup v2), documented instead: Without a
//	writable cgroup v2, supervisors stay in nexusd's OpenRC cgroup, and
//	rc_cgroup_cleanup="YES" kills them on stop. That is the expected,
//	documented outcome (CON-10). The harness cannot produce it: the container
//	shares the host's pure cgroup v2 hierarchy, and a cgroup v1 or hybrid
//	layout under OpenRC cannot be set up inside it. Making /sys/fs/cgroup
//	read-only would also stop OpenRC's own cleanup, so the case would prove
//	nothing. The behavior is covered by:
//	  - TC-35 and TC-36, which check that docs/security.md and docs/install.md
//	    state the kill;
//	  - TC-E11, which checks that the CON-10 limitation line is logged when
//	    cgroup v2 is unavailable.
func TestCgroupCleanupDoesNotKillSupervisor(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E14 (WI 3.4)")
}
