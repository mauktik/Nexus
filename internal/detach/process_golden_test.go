// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package detach

import "testing"

// nginx-provider TC-18: The systemd path is byte-identical
// Owner WI 2.1. Requirements: NFR-9, FR-20 (systemd unchanged), CON-11.
//
//	Setup: A golden file, testdata/golden/systemd-argv.json and spec.json,
//	produced by the base SHA's code from a fixed Spec and committed before the
//	WI starts. The test documents the git stash-free way to regenerate it: git
//	worktree add at d04583a, then go test -run
//	TestSystemdArgvAndSpecBytesUnchanged -update.
//	Steps:
//	  1. detach.Argv("01J…", "/usr/local/bin/nexusd", "/var/lib/nexus/ops",
//	    spec).
//	  2. WriteSpec to a temp dir.
//	Expected:
//	  - Both are byte-equal to the goldens.
//	  - Argv contains no --process.
//	  - Supervise(dir, diag, ModeUnit) on a ready spec behaves as the existing
//	    TestSupervisorRunsAChildAndWritesExitAtomically (that test calls ModeUnit
//	    and still passes unchanged).
func TestSystemdArgvAndSpecBytesUnchanged(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-18 (WI 2.1)")
}
