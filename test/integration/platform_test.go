//go:build integration

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package integration

import "testing"

// nginx-provider TC-37: The Alpine harness boots real OpenRC with cgroup v2 and no bind mounts
// Owner WI 3.4. Requirements: NFR-10.
//
//	Setup: Inside the Alpine harness.
//	Steps:
//	  1. Read /proc/1/cmdline, run rc-status default, and read
//	    /sys/fs/cgroup/cgroup.controllers.
//	  2. mkdir /sys/fs/cgroup/nexus-ops-probe, then rmdir it.
//	  3. Read /etc/os-release.
//	  4. Read platform.Current().
//	Expected:
//	  - PID 1 is /sbin/init, docker is started in the default runlevel, and the
//	    cgroup v2 controllers file exists and is writable.
//	  - ID=alpine.
//	  - platform.Current().Init == OpenRC.
//	  - The run.sh log (asserted by the run script, not the test) shows no bind:
//	    mount, the container removed, dist/ untouched, and logs collected (rc.log,
//	    /var/log/messages).
func TestAlpineHarnessSelfCheck(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-37 (WI 3.4)")
}

// nginx-provider TC-46: Parity suites run as root on both harnesses, and gated cases are not silently skipped
// Owner WI 3.4. Requirements: NFR-8, NFR-10.
//
//	Setup: run.sh copies go test -c binaries of ./internal/detach and
//	./internal/ops and runs them as root before integration.test, writing
//	detach.log and ops.log.
//	Steps:
//	  1. Parse both logs.
//	  2. Parse test.log for SKIP lines.
//	Expected:
//	  - Every /process subtest in ops.log is PASS on Alpine, and every root-only
//	    cgroup test in detach.log (TC-19, TC-20, TC-21) is PASS.
//	  - On Ubuntu, the systemd-gated tests are PASS. On Alpine, they are SKIP
//	    with requires systemd (detected openrc), and the reverse holds.
//	  - Any other SKIP of a root, cgroup or init-gated case on its own platform
//	    fails the run.
func TestParitySuitesRanAsRoot(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-46 (WI 3.4)")
}

// nginx-provider TC-49: The release nexusd is static and runs on musl
// Owner WI 3.4. Requirements: FR-38, NFR-11.
//
//	Setup: Alpine harness. No gcompat package installed (asserted with apk info
//	-e gcompat returning non-zero).
//	Steps:
//	  1. Open /usr/local/bin/nexusd with debug/elf.
//	  2. nexusd -version.
//	  3. serve, then GET /healthz.
//	Expected:
//	  - No PT_INTERP and no DT_NEEDED.
//	  - Both commands succeed.
//	  - validate-config resolves a runAs user from /etc/passwd (the pure-Go
//	    os/user path).
func TestReleaseBinaryStaticOnMusl(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-49 (WI 3.4)")
}

// nginx-provider TC-50: The Ubuntu harness still passes every existing suite, with platform gating
// Owner WI 3.4. Requirements: NFR-9, NFR-8.
//
//	Setup: Ubuntu harness.
//	Steps:
//	  1. make test-harness.
//	  2. In-test: requireInit(t, platform.OpenRC) in a subtest.
//	Expected:
//	  - Every existing test that passed at d04583a passes.
//	  - The OpenRC-gated subtest skips with requires openrc (detected systemd).
//	  - TestSetsidChildInDaemonCgroupDies runs (it is not skipped) on Ubuntu.
//	  - The log is committed to docs/verification/ with the SHA.
func TestRequireInitGating(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-50 (WI 3.4)")
}
