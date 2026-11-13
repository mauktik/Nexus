//go:build linux

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package nginx

import "testing"

// nginx-provider TC-90: WriteAtomic writes a root-owned 0644 file through a hidden temp and rename
// Owner WI 6.1. Requirements: FR-11, ASM-11, L-23.
//
//	Setup: Linux. The includeDir fd. When not root, the uid and gid parameters
//	are the current user's (T0). The root run happens in T1 (TC-46 runs the
//	package binary as root).
//	Steps:
//	  1. WriteAtomic(dirfd, "a.conf", content, 0644, uid, gid,
//	    ".a.nexus-01J.tmp"), observing the directory with an inotify watcher.
//	Expected:
//	  - a.conf bytes equal the content, its mode is 0644 and its owner is
//	    uid:gid (0:0 as root).
//	  - The inotify events show CREATE .a.nexus-01J.tmp then MOVED_TO a.conf,
//	    and no write to a.conf directly.
//	  - No temp file remains.
func TestWriteAtomic(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-90 (WI 6.1)")
}

// nginx-provider TC-91: Backup and Restore return a file to byte, mode and owner identity
// Owner WI 6.1. Requirements: NFR-3, ASM-6.
//
//	Setup: Linux. a.conf at mode 0640, plus an op dir.
//	Steps:
//	  1. Backup.
//	  2. WriteAtomic new content at 0644.
//	  3. Restore.
//	  4. Separately: Backup of a missing n.conf, create it, then Restore.
//	Expected:
//	  - previous/content equals the original bytes, and previous/meta.json holds
//	    {fileName, sha256, size, mode: 0640, uid, gid}. Both files are 0600 under
//	    the op dir, not under includeDir.
//	  - After step 3, the hash, mode and owner equal the original.
//	  - After step 4, n.conf does not exist.
func TestBackupRestoreIdentity(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-91 (WI 6.1)")
}

// nginx-provider TC-92: RenameNoReplace never overwrites
// Owner WI 6.1. Requirements: FR-9, FR-10, CON-5.
//
//	Setup: Linux. a.conf.disabled.
//	Steps:
//	  1. RenameNoReplace(a.conf.disabled → a.conf).
//	  2. Create b.conf and b.conf.disabled, then rename one onto the other.
//	  3. Force the EINVAL fallback through the test hook.
//	Expected:
//	  - Step 1 keeps the inode (stat.Ino equal before and after), the content,
//	    mode and owner.
//	  - Step 2 returns an EEXIST error, and both files are unchanged.
//	  - Step 3 succeeds through linkat then unlinkat, with the same result as
//	    step 1. With an existing destination it still refuses.
func TestRenameNoReplace(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-92 (WI 6.1)")
}

// nginx-provider TC-E24: A symlink planted at the destination is replaced, not followed
// Owner WI 6.1. Requirements: FR-11, ASM-11.
//
//	Setup: Linux. a.conf, a symlink to <tmp>/outside, which holds bytes X.
//	Steps:
//	  1. WriteAtomic(a.conf, N).
//	Expected:
//	  - a.conf is a regular file holding N.
//	  - outside still holds X.
func TestSymlinkAtDestinationReplaced(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E24 (WI 6.1)")
}

// nginx-provider TC-E25: A crash between temp and rename leaves a harmless hidden file
// Owner WI 6.1. Requirements: FR-7, L-23.
//
//	Setup: A .a.nexus-01J.tmp file containing INVALID, left as a crash would
//	leave it.
//	Steps:
//	  1. List the sites.
//	  2. Run the test op.
//	Expected:
//	  - The list does not show it.
//	  - The test op gives valid: true.
func TestLeftoverTempIsHarmless(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E25 (WI 6.1)")
}

// nginx-provider TC-ERR24: WriteAtomic refuses an existing temp name and changes nothing
// Owner WI 6.1. Requirements: FR-11.
//
//	Setup: Linux. .a.nexus-01J.tmp already exists, and a.conf holds P.
//	Steps:
//	  1. WriteAtomic with that temp name.
//	Expected: An EEXIST error. a.conf holds P, and the existing temp file is
//	unchanged.
func TestTempNameExists(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR24 (WI 6.1)")
}

// nginx-provider TC-ERR25: Restore fails loudly when the re-hash does not match
// Owner WI 6.1. Requirements: NFR-3.
//
//	Setup: Linux. Backup, then corrupt previous/content by flipping one byte.
//	Steps:
//	  1. Restore.
//	Expected: An error naming both hashes. The caller (tested in TC-ERR36) maps
//	it to outcome-unknown, never to success.
func TestRestoreHashMismatchFails(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR25 (WI 6.1)")
}

// nginx-provider TC-ERR26: A symlinked includeDir is refused by the file primitives
// Owner WI 6.1. Requirements: FR-2a, ASM-11.
//
//	Setup: Linux. includeDir, a symlink to a real dir.
//	Steps:
//	  1. Open it.
//	Expected: An ELOOP or ENOTDIR error naming symlink, and the target dir is
//	unchanged.
func TestIncludeDirSymlinkRefused(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR26 (WI 6.1)")
}
