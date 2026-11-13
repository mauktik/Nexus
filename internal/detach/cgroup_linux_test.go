//go:build linux

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package detach

import "testing"

// nginx-provider TC-19: KillControlGroup operations run in a per-operation cgroup that is removed afterwards
// Owner WI 2.2. Requirements: FR-20 (item 4), CON-10.
//
//	Setup: Linux, root, and /sys/fs/cgroup/cgroup.controllers present and
//	writable (requireRoot, requireCgroup2). An op with KillControlGroup whose
//	child writes /proc/self/cgroup to a marker and sleeps 1 s.
//	Steps:
//	  1. Launch.
//	  2. While running, read the supervisor's /proc/<pid>/cgroup.
//	  3. After exit.json, stat the cgroup dirs.
//	Expected:
//	  - The supervisor is in 0::/nexus-ops/<id> (relative to the test's cgroup
//	    namespace root).
//	  - The child marker shows /nexus-ops/<id>/work.
//	  - nexus-ops/<id>/cgroup.subtree_control and nexus-
//	    ops/cgroup.subtree_control are empty.
//	  - After exit, nexus-ops/<id> does not exist.
func TestOperationCgroupContainment(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-19 (WI 2.2)")
}

// nginx-provider TC-20: Cancel kills a setsid grandchild through the cgroup
// Owner WI 2.2. Requirements: FR-20 (item 4), NFR-10.
//
//	Setup: Linux, root, cgroup v2. A KillControlGroup op whose child runs setsid
//	sleep 1000 &, writes the grandchild PID to a marker, and sleeps. Grace 1 s.
//	Steps:
//	  1. Launch.
//	  2. Read the grandchild PID.
//	  3. Signal(SIGTERM).
//	  4. Wait for exit.json.
//	Expected:
//	  - Within grace plus 2 s, /proc/<grandchild> is gone.
//	  - nexus-ops/<id>/work/cgroup.procs is empty or the directory is removed.
//	  - exit.json is the canceled outcome.
func TestCancelKillsSetsidGrandchild(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-20 (WI 2.2)")
}

// nginx-provider TC-21: Leftovers are swept for KillControlGroup and never for KillProcess
// Owner WI 2.2. Requirements: FR-20 (item 4), L-10.
//
//	Setup: Linux, root, cgroup v2. Two ops, each with a child that starts sleep
//	1000 & (same process group) and exits 0 at once. Op A uses KillControlGroup;
//	op B uses KillProcess.
//	Steps:
//	  1. Run both to exit.json.
//	  2. Check each background sleep.
//	Expected:
//	  - A's sleep is gone within grace plus 2 s after the child's exit.
//	  - B's sleep is still alive. The test kills it in t.Cleanup.
//	  - B never had a work cgroup.
func TestLeftoverSweepOnlyForControlGroup(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-21 (WI 2.2)")
}

// nginx-provider TC-E11: Without cgroup v2 the operation log records the CON-10 limitation once
// Owner WI 2.2. Requirements: CON-10, FR-20 (item 4).
//
//	Setup: Linux. CgroupRoot: "-". A KillControlGroup op whose child starts
//	setsid sleep 1000 & and a same-group sleep 1000 &, then sleeps.
//	Steps:
//	  1. Cancel.
//	  2. Read the op output frames.
//	Expected:
//	  - Exactly one stderr frame equal to cgroup v2 unavailable: descendants
//	    that leave the process group are not covered (CON-10).
//	  - The same-group sleep is killed. The setsid one survives (documented),
//	    and the test kills it in cleanup.
//	  - The op ends Canceled.
func TestNoCgroupFallbackNotesCON10(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E11 (WI 2.2)")
}

// nginx-provider TC-E12: Fallbacks when CLONE_INTO_CGROUP or cgroup.kill are unavailable
// Owner WI 2.2. Requirements: FR-20 (item 4), L-7, L-9.
//
//	Setup: Linux, root, cgroup v2. Internal hooks: cloneIntoCgroup returns
//	EINVAL, and hasCgroupKill returns false.
//	Steps:
//	  1. Launch a KillControlGroup op.
//	  2. Cancel it with a setsid grandchild.
//	Expected:
//	  - The supervisor is still in nexus-ops/<id> (through the cgroup.procs
//	    write).
//	  - The cancel kills the grandchild through freeze and SIGKILL, and
//	    cgroup.freeze is back to 0 afterwards.
func TestCgroupFallbacks(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E12 (WI 2.2)")
}
