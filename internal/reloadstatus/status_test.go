// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-55, TC-RA-E15.
// Owner WIs: 3.2. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package reloadstatus

import "testing"

// TC-RA-55: reloadstatus writes atomically with mode 0600, reads back, bounds the read and orders statuses by boot and seq. Owner WI 3.2. Requirements: FR-8, CON-2.
//
//   - Setup: A temp dir.
//   - Steps:
//     1. `Read` on an empty dir.
//     2. `Write` S1 `{Boot:"b1",Seq:1,SHA256:h,Applied:true,RestartRequired:[]}`.
//     3. `Read`.
//     4. `Write` a 65 KiB file by hand, then `Read`.
//     5. Call `After` for these pairs: S2 `{b1,2}` after S1; `{b2,1}` after S1; S1 after S1; any status after `(_, hadBase=false)`.
//   - Expected:
//   - Step 1: `ok == false` and a nil error.
//   - Step 3: equals S1. The file mode is 0600, and the raw JSON has exactly the members `boot`, `seq`, `at`, `sha256`, `applied` and `restartRequired`, with `stage` and `error` omitted when empty.
//   - Step 4: an error, because the read is bounded at 64 KiB.
//   - Step 5: true, true, false, true.
//   - Cleanup: no temp file remains in the dir.
func TestTCRA55_ReloadstatusWritesAtomicallyWithMode0600Reads(t *testing.T) {
	t.Skip("TC-RA-55: SKIP-only stub; un-skip when WI 3.2 lands")
}

// TC-RA-E15: Concurrent reads of reload-status during writes never see partial JSON. Owner WI 3.2. Requirements: NFR-4, FR-8.
//
//   - Setup: A temp dir.
//   - Steps:
//     1. Run 1 writer making 1000 `Write`s with increasing seq while 4 readers `Read` in a loop, under `-race`.
//   - Expected: Every successful `Read` decodes. No read error other than "absent before first write" occurs. The seqs seen by each reader are non-decreasing.
func TestTCRAE15_ConcurrentReadsOfReloadStatusDuringWritesNever(t *testing.T) {
	t.Skip("TC-RA-E15: SKIP-only stub; un-skip when WI 3.2 lands")
}
