//go:build linux

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package detach

import "testing"

// nginx-provider TC-13: The process backend launches a session-leader supervisor and records its identity
// Owner WI 2.1. Requirements: FR-20 (items 1, 2), CON-11, NFR-8.
//
//	Setup: Linux. ProcessBackend(ProcessConfig{Executable:
//	detachtest.Supervisor(), CgroupRoot: "-"}) wired into detach.New. The op's
//	spec runs a child that writes its /proc/self/environ, cwd, stdin target
//	and the Umask: line of /proc/self/status to a marker, creates a file
//	umask-probe with os.OpenFile(..., O_CREATE|O_WRONLY, 0666), then sleeps 2
//	s. Before Launcher.Start the test sets its own umask to 077 with
//	syscall.Umask(0o077) (the value nexusd runs with under OpenRC) and
//	restores the old value in t.Cleanup. The umask is process-wide, so this
//	test must not call t.Parallel.
//	Steps:
//	  1. Launcher.Start.
//	  2. Read ops/<id>/supervisor.json.
//	  3. Read /proc/<pid>/stat, /proc/<pid>/cmdline and /proc/<pid>/status of
//	     the supervisor.
//	  4. Read the child's marker and stat the umask-probe file.
//	Expected:
//	  - The identity's pid is the supervisor's PID.
//	  - startTicks equals field 22 of its stat, parsed independently by the
//	    test.
//	  - bootId equals /proc/sys/kernel/random/boot_id trimmed.
//	  - The file mode is 0600.
//	  - The supervisor's session ID equals its PID.
//	  - cmdline is [<exe> op-supervise --dir <ops/id> --process].
//	  - The supervisor's own environment is empty, its cwd is /, and fd 0 is
//	    /dev/null.
//	  - The child's Umask: is 0022, although the launching process ran with
//	    0077. The supervisor sets umask(0022) before it starts the child,
//	    matching the default of systemd's transient units (L-47, NFR-8). The
//	    umask-probe file's mode is 0644 (0666 &^ 0022, computed in the test).
//	  - The test process's own umask is still 077 after Launcher.Start, so the
//	    backend does not change nexusd's umask (FR-23).
func TestProcessLaunchRecordsIdentityInOwnSession(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-13 (WI 2.1)")
}

// nginx-provider TC-14: A supervisor outlives the process that launched it and is finalized from exit.json
// Owner WI 2.1. Requirements: FR-20 (items 1, 3), FR-21.
//
//	Setup: Linux. A helper subprocess (the test binary re-executed with
//	GO_WANT_LAUNCHER=1) creates a ProcessBackend, launches an op whose child
//	sleeps 1.5 s then exits 7, and exits at once.
//	Steps:
//	  1. Wait for the helper to exit.
//	  2. Create a fresh ProcessBackend in the test process and call
//	    UnitState(Unit(id)) at once.
//	  3. Poll until exit.json exists, then call UnitState again.
//	Expected:
//	  - Step 2: the supervisor's PPID is not the helper's PID, and the state is
//	    {active, loaded}, which is Live.
//	  - Step 3: exit.json has exit code 7, and the state is {inactive, not-
//	    found}, which is Absent.
func TestSupervisorOutlivesLauncherProcess(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-14 (WI 2.1)")
}

// nginx-provider TC-15: Signal reaches only a verified supervisor, and cancel behaves as today
// Owner WI 2.1. Requirements: FR-20 (item 3).
//
//	Setup: Linux. An op whose child (in its own process group) traps SIGTERM,
//	writes term to a marker and exits, with grace 2 s.
//	Steps:
//	  1. Launch the op.
//	  2. Signal(ctx, Unit(id), SIGTERM).
//	  3. Wait for exit.json.
//	Expected:
//	  - The marker says term, and exit.json records the cancel outcome exactly
//	    as the detachtest.Local run of the same op does. The test runs both and
//	    compares the two exit.json files field by field, apart from timestamps and
//	    PIDs.
func TestSignalCancelsVerifiedSupervisor(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-15 (WI 2.1)")
}

// nginx-provider TC-16: ListUnits reports only live identities, as unit labels
// Owner WI 2.1. Requirements: FR-20 (item 3), FR-21.
//
//	Setup: Linux. Three op directories:
//	  - a: live supervisor;
//	  - b: exited (identity present, process gone);
//	  - c: forged identity with a different bootId.
//	Steps:
//	  1. ListUnits(ctx).
//	Expected: Exactly [detach.Unit("a")], which parses through the existing
//	recovery.go:511 orphan parsing to ID a.
func TestListUnitsReturnsLiveIdentitiesOnly(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-16 (WI 2.1)")
}

// nginx-provider TC-17: Supervisor diagnostics go to supervisor.log and are capped
// Owner WI 2.1. Requirements: FR-20 (item 5).
//
//	Setup: Linux. A diag writer test seam that produces more than
//	oplog.DefaultCapBytes of diagnostic lines (for example a spec whose child is
//	missing, repeated through the cap hook).
//	Steps:
//	  1. Run Supervise(dir, diag, ModeProcess) with a valid identity in place.
//	Expected:
//	  - ops/<id>/supervisor.log exists with mode 0600.
//	  - Its size is at most oplog.DefaultCapBytes plus one truncation marker
//	    line.
//	  - Nothing is written to the supervisor's stderr beyond the file.
func TestSupervisorLogIsCapped(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-17 (WI 2.1)")
}

// nginx-provider TC-ERR3: A supervisor that never sees its identity exits without running anything
// Owner WI 2.1. Requirements: FR-20 (item 2), L-51.
//
//	Setup: Linux. An op dir with spec.json whose child would create a marker,
//	and no supervisor.json. The launch timeout is injected at 500 ms.
//	Steps:
//	  1. Run nexusd op-supervise --dir D --process directly.
//	Expected:
//	  - Exit 1 within 2 s.
//	  - spec.json is still present, and there is no exit.json and no marker.
func TestHandshakeTimeoutRunsNothing(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR3 (WI 2.1)")
}

// nginx-provider TC-ERR4: A failed identity write kills the supervisor and fails the launch
// Owner WI 2.1. Requirements: FR-20 (item 2).
//
//	Setup: Linux. The hook writeIdentity returns EIO.
//	Steps:
//	  1. Launcher.Start.
//	Expected:
//	  - Start returns an error.
//	  - The spawned supervisor PID is gone within 1 s (SIGKILL).
//	  - spec.json is removed, and the child marker never appears.
func TestIdentityWriteFailureKillsSupervisor(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR4 (WI 2.1)")
}
