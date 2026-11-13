// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package nginx

import "testing"

// nginx-provider TC-93: Enable renames, tests and reloads
// Owner WI 6.2. Requirements: FR-9, FR-6.
//
//	Setup: a.conf.disabled (valid content). The fake is running, with a lockstep
//	ProcReader.
//	Steps:
//	  1. POST /sites/a/enable.
//	  2. Follow the op.
//	Expected:
//	  - The op Succeeded with {name: a, fileName: a.conf, enabled: true,
//	    reloaded: true, reload.verdict: confirmed}.
//	  - a.conf has the same inode, content, mode and owner as the old disabled
//	    file.
//	  - The recorded calls are nginx -t then reload.
//	  - The audit has requested and succeeded records with target a.
func TestEnable(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-93 (WI 6.2)")
}

// nginx-provider TC-94: Disable renames, tests and reloads
// Owner WI 6.2. Requirements: FR-10.
//
//	Setup: a.conf, running.
//	Steps:
//	  1. POST /sites/a/disable.
//	Expected:
//	  - a.conf.disabled exists with the same inode, and a.conf is absent.
//	  - The result has enabled: false and reloaded: true.
//	  - The op's risk is destructive.
func TestDisable(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-94 (WI 6.2)")
}

// nginx-provider TC-95: No nginx operation ever deletes a file
// Owner WI 6.2. Requirements: OOS-4, FR-10, NFR-3.
//
//	Setup: A tree of 3 sites.
//	Steps:
//	  1. Run every success path and every failure path of TC-93, TC-94, TC-
//	    ERR27, TC-ERR28 and TC-ERR31.
//	  2. After each, compute the set of inodes of non-dotfiles.
//	Expected:
//	  - After each path, the inode set is a superset of the inodes that existed
//	    before it. Only a create adds an inode, and only a failed create removes
//	    the one it added.
//	  - A grep of the helper source shows unlinkat used only in Restore of a
//	    created file and in temp cleanup.
func TestNoOperationDeletes(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-95 (WI 6.2)")
}

// nginx-provider TC-E26: Enabling an already-enabled config changes nothing
// Owner WI 6.2. Requirements: FR-9.
//
//	Setup: a.conf. Running.
//	Steps:
//	  1. Enable a.
//	Expected:
//	  - Succeeded with changed: false and reloaded: false.
//	  - No nginx -t and no reload were recorded. The hashes are unchanged.
func TestEnableAlreadyEnabled(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E26 (WI 6.2)")
}

// nginx-provider TC-E27: Enable or disable while nginx is stopped renames and tests, but does not start nginx
// Owner WI 6.2. Requirements: FR-9, FR-10.
//
//	Setup: a.conf.disabled and b.conf, both valid. Stopped.
//	Steps:
//	  1. Enable a.
//	  2. Disable b.
//	Expected:
//	  - Step 1: a.conf exists with the inode of the old a.conf.disabled, and
//	    reloaded is false.
//	  - Step 2: b.conf.disabled exists with the inode of the old b.conf,
//	    b.conf is absent, the result has enabled: false and reloaded: false,
//	    and the op is Succeeded.
//	  - For each step, the recorded calls are nginx -t (one line per step in
//	    fakenginx's call log <exe>.json.calls) and Status only, with no
//	    Do(start) or Do(reload).
//	  - nginx is still stopped after both steps.
func TestEnableWhileStopped(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E27 (WI 6.2)")
}

// nginx-provider TC-E28: Disabling an already-disabled config changes nothing
// Owner WI 6.2. Requirements: FR-10.
//
//	Setup: a.conf.disabled.
//	Steps:
//	  1. Disable a.
//	Expected: changed: false, with no test and no reload.
func TestDisableAlreadyDisabled(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E28 (WI 6.2)")
}

// nginx-provider TC-ERR27: Enable of an invalid config renames back and fails
// Owner WI 6.2. Requirements: FR-9, NFR-3, FR-17.
//
//	Setup: a.conf.disabled containing INVALID. Running.
//	Steps:
//	  1. Enable a.
//	Expected:
//	  - The op fails nginx-config-invalid with rolledBack: true and testOutput
//	    naming a.conf.
//	  - a.conf.disabled is restored with the same inode.
//	  - No reload was recorded. The includeDir snapshot (names, bytes, mode,
//	    owner) equals the pre-op snapshot.
func TestEnableInvalidRollsBack(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR27 (WI 6.2)")
}

// nginx-provider TC-ERR28: A disable that breaks another file's upstream renames back
// Owner WI 6.2. Requirements: FR-10, NFR-3.
//
//	Setup: up.conf defines upstream be, and use.conf has proxy_pass http://be.
//	Running.
//	Steps:
//	  1. Disable up.
//	Expected:
//	  - The op fails nginx-config-invalid with host not found in upstream "be"
//	    and rolledBack: true.
//	  - up.conf is back, with no reload. The snapshot is equal.
func TestDisableBreakingUpstreamRollsBack(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR28 (WI 6.2)")
}

// nginx-provider TC-ERR29: Enable and disable refuse unmanaged, conflicting and missing entries at admission
// Owner WI 6.2. Requirements: FR-9, FR-10, FR-17.
//
//	Setup:
//	  - s.conf.disabled, a symlink;
//	  - c.conf and c.conf.disabled;
//	  - no m;
//	  - d.conf.disabled, a directory.
//	Steps:
//	  1. Enable s, enable c, enable m and enable d.
//	  2. Disable c.
//	Expected:
//	  - nginx-config-unmanaged (422) with reasons symlink, conflict, conflict
//	    and not-regular, and 404 not-found for m.
//	  - No operation is created, and audit records are refused.
//	  - The snapshot is unchanged.
func TestToggleAdmitRefusals(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR29 (WI 6.2)")
}

// nginx-provider TC-ERR30: An entry that becomes unmanaged after admission is refused by the helper
// Owner WI 6.2. Requirements: FR-9, FR-10, ASM-11.
//
//	Setup: a.conf.disabled, regular at Admit. The hook beforeHelper replaces it
//	with a symlink.
//	Steps:
//	  1. Enable a.
//	Expected:
//	  - The op fails nginx-config-unmanaged with reason: symlink.
//	  - No rename, no test and no reload were recorded.
//	  - The symlink is untouched.
func TestToggleHelperRecheck(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR30 (WI 6.2)")
}

// nginx-provider TC-ERR31: A reload rejected after a toggle renames back and reloads again
// Owner WI 6.2. Requirements: FR-9, FR-10, FR-6, NFR-3.
//
//	Setup:
//	  - Case A: enabling p makes the model bind-conflict (systemd fake, exit 0,
//	    generation unchanged), and the rollback reload confirms.
//	  - Case B: the same, but the rollback reload is also unconfirmed.
//	Steps:
//	  1. Enable p.
//	Expected:
//	  - A: nginx-reload-failed with rolledBack: true, rollbackReload.verdict
//	    confirmed and observed.verdict rejected. p.conf.disabled is back, and the
//	    calls were -t, reload, -t, reload.
//	  - B: outcome-unknown, with a detail naming p.conf.disabled.
func TestToggleReloadRejected(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR31 (WI 6.2)")
}

// nginx-provider TC-ERR42: Enable and disable refuse, before renaming, when a running nginx's master cannot be identified
// Owner WI 6.2. Requirements: FR-9, FR-10, FR-6, FR-17, NFR-2, L-20, L-55.
//
//	Setup: a.conf.disabled and b.conf, both valid, and a byte-identity
//	snapshot of includeDir (names, inodes, bytes, modes). nginx is running in
//	the fake's model, with an unidentifiable master, as a table:
//	  - Case A: OpenRC fake, started, no daemon record (MainPID 0).
//	  - Case B: systemd fake, active, MainPID whose exe is /usr/bin/sleep.
//	Steps:
//	  1. Enable a.
//	  2. Disable b.
//	Expected: for each case and each step:
//	  - The op ends Failed (not outcome-unknown) with dependency-unavailable.
//	    The detail starts nginx master process could not be identified: and
//	    names the reason (init system reports no main PID for A, exe for B).
//	  - includeDir equals the snapshot: a.conf.disabled and b.conf keep their
//	    names and inodes, and a.conf and b.conf.disabled do not exist. No
//	    rename was made and none was reverted.
//	  - fakenginx's call log has no -t invocation, and the svcctl fake
//	    recorded no Do(reload). Identification happens before any change, so
//	    there is nothing to test or roll back.
//	  - The result has no rolledBack: true.
func TestToggleRefusesUnidentifiableMaster(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR42 (WI 6.2)")
}
