// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package nginx

import "testing"

// nginx-provider TC-84: Start brings nginx to running after a passing config test
// Owner WI 5.3. Requirements: FR-5, FR-19.
//
//	Setup: The svcctl fake (both modes, as a table), stopped, and fakenginx
//	valid. Inside synctest where possible (the helper runs in-process through
//	env injection).
//	Steps:
//	  1. Run the helper action start.
//	Expected:
//	  - Recorded calls, in order: nginx -t, Do(start), then Status polls.
//	  - The result is {changed: true, state: running, mainPid: <fake PID>}.
//	  - The poll interval is 250 ms.
func TestStartFromStopped(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-84 (WI 5.3)")
}

// nginx-provider TC-85: Stop brings nginx to stopped
// Owner WI 5.3. Requirements: FR-5.
//
//	Setup: The fake is running.
//	Steps:
//	  1. Run stop.
//	Expected:
//	  - Calls: Do(stop) then polls. There is no nginx -t.
//	  - The result is {changed: true, state: stopped, mainPid: null}.
//	  - The op's risk is destructive (the spec, TC-56).
func TestStopFromRunning(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-85 (WI 5.3)")
}

// nginx-provider TC-86: Restart tests first and ends running with a new master
// Owner WI 5.3. Requirements: FR-5.
//
//	Setup: Both init modes.
//	  - Case A: the fake is running with PID 100. Restart gives PID 200.
//	  - Case B: the fake is stopped. Restart gives PID 300.
//	Steps:
//	  1. Run restart in each case.
//	Expected:
//	  - A: calls are nginx -t, Do(restart), then polls. The result has
//	    changed: true, state: running and mainPid: 200.
//	  - B: calls are nginx -t, Do(restart), then polls, with no Do(start). The
//	    result has changed: true, state: running and mainPid: 300. Restart
//	    from stopped ends running (FR-5).
func TestRestart(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-86 (WI 5.3)")
}

// nginx-provider TC-87: Reload succeeds only on a confirmed new generation
// Owner WI 5.3. Requirements: FR-6, ASM-7, L-53.
//
//	Setup: The svcctl fake running, its nginx model valid, and the fake
//	ProcReader in lockstep (strategy). Both init modes.
//	  - Case A: the master's exe is /usr/sbin/nginx.
//	  - Case B (after an upgrade): the master's exe is /usr/sbin/nginx
//	    (deleted).
//	Steps:
//	  1. Run reload in each case.
//	Expected:
//	  - Calls: Status, nginx -t, Do(reload).
//	  - The result's reload.verdict is confirmed, its masterPid is unchanged,
//	    newWorkers is non-empty, and initExitCode is 0.
//	  - The op is Succeeded in both cases. Case B proves an upgraded but not
//	    restarted nginx can still be reloaded.
func TestReloadConfirmed(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-87 (WI 5.3)")
}

// nginx-provider TC-E20: Start of a running nginx is idempotent
// Owner WI 5.3. Requirements: FR-5.
//
//	Setup: The fake is running, both init modes.
//	  - Case A: valid config.
//	  - Case B: INVALID in a.conf on disk (the running nginx loaded an
//	    earlier, valid version).
//	Steps:
//	  1. Run start in each case.
//	Expected:
//	  - Both cases: Succeeded with changed: false and state: running.
//	  - No Do(start) was recorded.
//	  - No nginx -t ran: fakenginx's call log (<exe>.json.calls) has no -t
//	    invocation. start checks Status first and returns before the test when
//	    nginx is running, so an invalid on-disk config does not turn an
//	    idempotent start into nginx-config-invalid.
func TestStartIdempotent(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E20 (WI 5.3)")
}

// nginx-provider TC-E21: Stop of a stopped nginx is idempotent
// Owner WI 5.3. Requirements: FR-5.
//
//	Setup: The fake is stopped.
//	Steps:
//	  1. Run stop.
//	Expected: changed: false, and no Do(stop).
func TestStopIdempotent(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E21 (WI 5.3)")
}

// nginx-provider TC-E22: A running nginx with no readable master PID makes reload a definite refusal
// Owner WI 5.3. Requirements: FR-6, L-20, L-55, NFR-2.
//
//	Setup:
//	  - Case A: OpenRC fake, started, with no daemon record (MainPID 0).
//	  - Case B: systemd fake, ActiveState=active, MainPID=0.
//	Steps:
//	  1. Run reload.
//	Expected:
//	  - The op ends Failed (not outcome-unknown) with problem
//	    dependency-unavailable and a detail starting nginx master process
//	    could not be identified: followed by the reason, init system reports
//	    no main PID (the L-55 example text).
//	  - No Do(reload) was recorded. Nothing was sent to nginx, so the outcome
//	    is known (NFR-2 reserves outcome-unknown for a mutation whose result
//	    cannot be determined).
//	  - No nginx -t ran (no -t line in fakenginx's call log): resolveMaster
//	    runs first, and reload does nothing after it fails (Data Flow 3 steps
//	    1 and 2).
//	  - includeDir is byte-identical to before.
func TestReloadWithoutMasterPID(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E22 (WI 5.3)")
}

// nginx-provider TC-ERR16: Start and restart refuse an invalid config and change nothing
// Owner WI 5.3. Requirements: FR-5, FR-17.
//
//	Setup: INVALID in a.conf. Start from stopped. Restart from running.
//	Steps:
//	  1. Run each.
//	Expected:
//	  - Both fail nginx-config-invalid with testOutput containing unknown
//	    directive.
//	  - Neither recorded Do(start) or Do(restart).
//	  - The state is unchanged.
func TestStartRestartRefuseInvalidConfig(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR16 (WI 5.3)")
}

// nginx-provider TC-ERR17: A systemd reload that exits 0 but leaves the old generation is a failure
// Owner WI 5.3. Requirements: FR-6, ASM-7, NFR-6.
//
//	Setup: svcctl fake systemd, with the nginx model bind-conflict (-t passes
//	and the master rejects the reload). Lockstep ProcReader. reloadVerifyTimeout
//	15s in synctest.
//	Steps:
//	  1. Run reload.
//	Expected:
//	  - The op fails nginx-reload-failed with observed.initExitCode == 0 and
//	    observed.verdict == rejected.
//	  - The failure is reported at bubble time 15s.
//	  - The op does not succeed.
func TestSystemdReloadExitZeroButRejected(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR17 (WI 5.3)")
}

// nginx-provider TC-ERR18: An OpenRC reload that exits non-zero is a failure
// Owner WI 5.3. Requirements: FR-6, ASM-7.
//
//	Setup: svcctl fake OpenRC. The config turns invalid after the pre-test: a
//	test hook flips the model between nginx -t and Do(reload).
//	Steps:
//	  1. Run reload.
//	Expected: nginx-reload-failed with initExitCode != 0 and verdict rejected,
//	reported at once (TC-E19).
func TestOpenRCReloadNonZero(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR18 (WI 5.3)")
}

// nginx-provider TC-ERR19: Reload of a stopped nginx is refused
// Owner WI 5.3. Requirements: FR-6, FR-17.
//
//	Setup: Case A: stopped at Admit. Case B: running at Admit, stopped before
//	the helper runs (the hook stops the fake between Admit and the helper).
//	Steps:
//	  1. POST reload in each case.
//	Expected:
//	  - A: 422 nginx-not-running with state: stopped. No operation is created,
//	    and the audit records refused.
//	  - B: the op fails nginx-not-running, and no Do(reload) and no Do(start)
//	    were recorded.
func TestReloadNotRunning(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR19 (WI 5.3)")
}

// nginx-provider TC-ERR20: An unconfirmed reload is outcome-unknown; an unidentifiable master is a definite refusal
// Owner WI 5.3. Requirements: FR-6, NFR-2, L-55.
//
//	Setup:
//	  - Case A: the TC-E18 synthetic sequence.
//	  - Case B: running, MainPID whose /proc exe is /usr/bin/sleep (not
//	    nginx).
//	Steps:
//	  1. Run reload.
//	Expected:
//	  - A: outcome-unknown with observed.verdict == unconfirmed. The reload
//	    was sent, so its result is genuinely unknown.
//	  - B: the op ends Failed (not outcome-unknown) with
//	    dependency-unavailable and a detail starting nginx master process
//	    could not be identified: that names exe. No nginx -t and no Do(reload)
//	    were recorded, and includeDir is byte-identical.
func TestReloadUnknownOutcomes(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR20 (WI 5.3)")
}

// nginx-provider TC-ERR21: State convergence failures and init timeouts are bounded and named
// Owner WI 5.3. Requirements: FR-5, NFR-2, FR-17.
//
//	Setup: synctest. Three cases:
//	  - A: start leads to failed (Raw "failed (Result: exit-code)").
//	  - B: start leaves the state starting forever.
//	  - C: the fake Blocks on Do(start).
//	  stateTimeout is 30s.
//	Steps:
//	  1. Run start.
//	Expected:
//	  - A: dependency-unavailable, with a detail naming failed and Raw.
//	  - B: outcome-unknown at bubble time 30s.
//	  - C: dependency-unavailable naming the init-system timeout, at 30s.
func TestConvergenceFailures(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR21 (WI 5.3)")
}

// nginx-provider TC-ERR41: Reload refuses an invalid on-disk config and changes nothing
// Owner WI 5.3. Requirements: FR-6, FR-17, NFR-2.
//
//	Setup: Both init modes, as a table. The svcctl fake running, with an
//	identifiable master (the TC-87 Case A fixture) and the fake ProcReader in
//	lockstep. a.conf on disk contains INVALID (the running nginx loaded an
//	earlier, valid version). Before the op, record the fake nginx model's
//	generation counter and the master (pid, startTicks) and worker (pid,
//	startTicks) set through the model's inspection accessor, which does not
//	advance the ProcReader replay. Record a byte-identity snapshot of
//	includeDir (names, inodes, bytes, modes).
//	Steps:
//	  1. Run reload.
//	Expected:
//	  - The op ends Failed (not outcome-unknown) with nginx-config-invalid.
//	    Its testOutput contains unknown directive "INVALID", the text of the
//	    fakenginx README's captured line.
//	  - Recorded calls: Status and one nginx -t. No Do(reload) in either mode.
//	    In OpenRC mode this matters because rc-service nginx reload runs its
//	    own configtest, and the helper must refuse before reaching it.
//	  - The model's generation counter is unchanged, and the master and worker
//	    (pid, startTicks) sets equal the recorded ones.
//	  - includeDir equals the snapshot.
//	  - The result has no reload observation.
func TestReloadRefusesInvalidConfig(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR41 (WI 5.3)")
}
