// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package svcctl

import "testing"

// nginx-provider TC-7: systemd Status maps captured `systemctl show` output
// Owner WI 1.4. Requirements: FR-19.
//
//	Setup: svcctltest fake in systemd mode, answering show with each
//	testdata/systemd/show-*.txt capture (running, stopped, failed, not-found)
//	plus synthetic rows for reloading, activating and deactivating, each made by
//	editing one line of the running capture.
//	Steps:
//	  1. Status(ctx, "nginx") for each fixture.
//	Expected:
//	  - running: {Known: true, State: running, MainPID: <fixture MainPID>,
//	    Since: unix(<fixture StateChangeTimestamp>)}.
//	  - stopped: stopped with MainPID 0.
//	  - failed: failed.
//	  - not-found: Known: false.
//	  - reloading → running, activating → starting, deactivating → stopping.
//	  - Raw contains the fixture's ActiveState/SubState words.
//	  - The recorded argv is exactly /usr/bin/systemctl show --timestamp=unix -p
//	    LoadState -p ActiveState -p SubState -p MainPID -p StateChangeTimestamp --
//	    nginx.service.
func TestSystemdStatusFromCapturedShow(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-7 (WI 1.4)")
}

// nginx-provider TC-8: OpenRC Status maps captured rc-service output and reads MainPID from the daemon record
// Owner WI 1.4. Requirements: FR-19.
//
//	Setup:
//	  - svcctltest in OpenRC mode with
//	    testdata/openrc/status-{started,stopped,crashed}.txt and synthetic
//	    starting/stopping captures.
//	  - RunRoot in a temp dir with openrc/daemons/nginx/001 copied from
//	    testdata/openrc/daemon-record, whose pidfile= points at a temp pidfile
//	    holding the PID of a live child process the test starts.
//	  - ProcRoot is the real /proc on Linux, or a fake dir with <pid>/ on macOS.
//	Steps:
//	  1. Status(ctx, "nginx") for each state.
//	Expected:
//	  - started → running with MainPID equal to the child's PID; stopped →
//	    stopped; crashed → failed; starting and stopping map to themselves.
//	  - Since == nil in every case.
//	  - Known comes from --exists exit 0.
//	  - The recorded calls are /sbin/rc-service --exists nginx and /sbin/rc-
//	    service -C nginx status.
func TestOpenRCStatusFromCapturedOutput(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-8 (WI 1.4)")
}

// nginx-provider TC-9: Do invokes exactly the absolute binary with the action, and never a shell
// Owner WI 1.4. Requirements: FR-19, CON-4.
//
//	Setup: svcctltest recording runner, both modes.
//	Steps:
//	  1. For each action (start, stop, restart, reload), call Do(ctx, a,
//	    "nginx") on systemd and on OpenRC.
//	Expected:
//	  - systemd argv: [/usr/bin/systemctl <a> -- nginx.service]. OpenRC argv:
//	    [/sbin/rc-service -C nginx <a>].
//	  - No argv element contains a space-joined command or sh/-c.
//	  - Result.ExitCode and Output equal what the fake returned.
//	  - An Action value outside the four is refused before any call.
func TestDoArgvIsExact(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-9 (WI 1.4)")
}

// nginx-provider TC-10: The real runner uses a fixed environment, caps output, and is bounded
// Owner WI 1.4. Requirements: FR-19, NFR-2.
//
//	Setup: Temp scripts, each invoked by absolute path through the internal real
//	runner:
//	  - envdump.sh prints env and reads stdin;
//	  - big.sh writes 200 KiB;
//	  - hang.sh runs sleep 60.
//	Steps:
//	  1. Run envdump.sh.
//	  2. Run big.sh.
//	  3. Run hang.sh with Timeout: 200ms.
//	Expected:
//	  - Step 1 prints exactly the two lines PATH=/usr/sbin:/usr/bin:/sbin:/bin
//	    and LC_ALL=C, with no other variable, and stdin reads EOF at once.
//	  - Step 2 Output is exactly 65536 bytes.
//	  - Step 3 returns within 2 s with errors.Is(err, context.DeadlineExceeded),
//	    and the sleep process is gone.
func TestRealRunnerEnvCapAndTimeout(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-10 (WI 1.4)")
}

// nginx-provider TC-11: The svcctl fake encodes real init-system constraints
// Owner WI 1.4. Requirements: FR-19, ASM-7, NFR-6.
//
//	Setup: The fake's nginx model with config invalid, then bind-conflict, then
//	valid.
//	Steps:
//	  1. systemd mode: Do(reload) under each config.
//	  2. OpenRC mode: Do(reload) under each config.
//	  3. OpenRC: Do(start) while started.
//	  4. Call the fake with argv[0] = "systemctl" (relative) and with an extra
//	    env var.
//	Expected:
//	  - Step 1 exits 0 in all three cases, and the generation advances only for
//	    valid.
//	  - Step 2 exits non-zero with the configtest text for invalid, exits 0 for
//	    bind-conflict with the generation unchanged, and exits 0 with the
//	    generation advanced for valid.
//	  - Step 3 exits 0, prints the "already been started" warning, and does not
//	    change state.
//	  - Step 4 fails the inner test, which a testing.T wrapper (tRecorder)
//	    observes, so the fake cannot be called wrongly and still pass.
func TestFakeEncodesInitSystemConstraints(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-11 (WI 1.4)")
}

// nginx-provider TC-12: svcctl fixtures carry their real-host provenance
// Owner WI 1.4. Requirements: FR-19, NFR-6.
//
//	Setup: None.
//	Steps:
//	  1. Walk testdata/.
//	  2. Parse testdata/README.md.
//	Expected:
//	  - Every fixture file has a README row naming host, distribution and
//	    version, init version, nginx version, capture command and date.
//	  - No row says UNCONFIRMED.
//	  - Rows exist for both Ubuntu and Alpine.
//	---
//	## Phase 2: Detached operations without systemd
func TestFixtureProvenance(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-12 (WI 1.4)")
}

// nginx-provider TC-E5: Unknown or unparseable systemd values never become a guess
// Owner WI 1.4. Requirements: FR-19.
//
//	Setup: Edits of the running capture:
//	  - ActiveState=maintenance;
//	  - StateChangeTimestamp=n/a;
//	  - MainPID=0 while active;
//	  - output missing the ActiveState line.
//	Steps:
//	  1. Status.
//	Expected:
//	  - maintenance → unknown.
//	  - An unparseable timestamp → Since nil.
//	  - MainPID 0 → 0.
//	  - A missing ActiveState → unknown, with Raw holding the output.
func TestSystemdUnknownValues(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E5 (WI 1.4)")
}

// nginx-provider TC-E6: OpenRC MainPID failures leave 0 and never error
// Owner WI 1.4. Requirements: FR-19, L-20.
//
//	Setup: started, with:
//	  - no daemons dir;
//	  - a record with no pidfile=;
//	  - a pidfile with abc;
//	  - a pidfile with a PID that has no /proc entry;
//	  - an unreadable pidfile.
//	Steps:
//	  1. Status.
//	Expected: State running, MainPID 0 and err == nil in each case.
func TestOpenRCMainPIDFailures(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E6 (WI 1.4)")
}

// nginx-provider TC-ERR1: svcctl refuses invalid service names and an unknown init system
// Owner WI 1.4. Requirements: FR-19, CON-4.
//
//	Setup: The recording runner.
//	Steps:
//	  1. Status and Do with each of these names: "", nginx;rm -rf /, ../nginx,
//	    -nginx, nginx service, a 65-character name, and nginx\n.
//	  2. New(platform.Unknown, …).
//	Expected:
//	  - Each call in step 1 returns an error naming ServiceNamePattern, and the
//	    runner recorded zero calls.
//	  - Step 2 returns an error containing unknown.
func TestRefusesBadServiceNamesAndUnknownInit(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR1 (WI 1.4)")
}

// nginx-provider TC-ERR2: svcctl reports runner failures and timeouts as errors, never as a state
// Owner WI 1.4. Requirements: FR-19, NFR-2.
//
//	Setup: The fake with Block: true, inside synctest, with Timeout 30s. Then a
//	fake returning an exec error (ENOENT).
//	Steps:
//	  1. Status.
//	  2. Do(reload).
//	Expected:
//	  - The blocked call returns at bubble time 30s with
//	    context.DeadlineExceeded.
//	  - The exec error is returned wrapped.
//	  - In neither case is a Status with State running or stopped returned.
func TestRunnerFailuresAreErrors(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR2 (WI 1.4)")
}
