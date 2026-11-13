//go:build linux

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package nginx

import "testing"

// nginx-provider TC-88: The shadow test agrees with the live test and leaves the host unchanged
// Owner WI 5.4. Requirements: FR-11 (whatIf), ASM-10, L-30.
//
//	Setup: Linux, root. fakenginx. An includeDir with a.conf, a subdirectory
//	snips/ holding x.inc, and a symlink l.conf to a.conf. Four candidates:
//	  - valid;
//	  - INVALID;
//	  - a default_server duplicate of a.conf's port;
//	  - a candidate using an upstream that a.conf defines.
//	Steps:
//	  1. Record /proc/self/mountinfo and the includeDir listing plus hashes.
//	  2. For each candidate, run ShadowTester.Test(ctx, "cand.conf", path).
//	  3. Copy the candidate in as cand.conf, run live fakenginx -t, and remove
//	    it.
//	  4. Record again.
//	Expected:
//	  - The shadow verdict equals the live verdict for all four: valid, invalid,
//	    invalid, valid.
//	  - The mountinfo and the listing plus hashes are byte-identical before and
//	    after.
//	  - No cand.conf remains.
func TestShadowMatchesLiveAndLeavesHostUnchanged(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-88 (WI 5.4)")
}

// nginx-provider TC-89: The shadow tree mirrors includeDir's top level
// Owner WI 5.4. Requirements: ASM-10.
//
//	Setup: Linux, root. A fakenginx variant (-t with LIST=1 in its JSON) that
//	prints every file it sees with its mode, owner and hash. The includeDir
//	contains a file at mode 0640 owned by uid 0 gid 4, a symlink, a
//	subdirectory, a FIFO, and an existing cand.conf.
//	Steps:
//	  1. Run a shadow test of a new cand.conf.
//	Expected:
//	  - The listing shows the file with the same mode, owner and hash; the
//	    symlink with the same target; the subdirectory's contents (bind-mounted);
//	    no FIFO; and cand.conf with the candidate's hash (replaced).
//	  - The child's env is exactly {PATH=/usr/sbin:/usr/bin:/sbin:/bin,
//	    LC_ALL=C}.
func TestShadowTreeConstruction(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-89 (WI 5.4)")
}

// nginx-provider TC-E23: Shadow mounts never propagate to the host
// Owner WI 5.4. Requirements: ASM-10, L-30.
//
//	Setup: Linux, root. Make / MS_SHARED in a test mount namespace (the test
//	runs inside unshare -m with --propagation shared) to model systemd's shared
//	root.
//	Steps:
//	  1. Run a shadow test with a fakenginx variant that sleeps 1 s.
//	  2. During the sleep, read the test namespace's mountinfo.
//	Expected: No tmpfs or bind over includeDir is visible in the test namespace
//	at any time.
func TestShadowPropagationPrivate(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E23 (WI 5.4)")
}

// nginx-provider TC-ERR22: An includeDir too large to shadow is refused
// Owner WI 5.4. Requirements: ASM-10, L-31, NFR-2.
//
//	Setup: Linux, root. The tmpfs cap hook is set to 1 MiB, and includeDir holds
//	2 MiB of top-level files.
//	Steps:
//	  1. Run a shadow test.
//	Expected:
//	  - dependency-unavailable with includeDir too large to shadow.
//	  - The host mountinfo is unchanged.
func TestShadowSizeCap(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR22 (WI 5.4)")
}
