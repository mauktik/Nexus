// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-5, TC-SV-6, TC-SV-E1, TC-SV-ERR4.
// Owner WIs: 1.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package detach_test

import "testing"

// TC-SV-5: RecordReboot writes the record atomically with mode 0600 and ReadReboot reads it back. Owner WI 1.2. Requirements: FR-11, CON-5.
//
// - Setup: `dir := t.TempDir()`; `rec := detach.RebootRecord{BootID: PREV, RequestedAt: time.Date(2026,11,18,9,0,0,123456789,time.UTC)}`.
// - Steps:
//  1. `ReadReboot(dir)` before anything is written.
//  2. `RecordReboot(dir, rec)`.
//  3. `os.Stat(filepath.Join(dir, detach.RebootFile))` and `os.ReadDir(dir)`.
//  4. `ReadReboot(dir)`.
//  5. `RecordReboot` again with `BootID: CUR`, then `ReadReboot`.
//
// - Expected:
//   - step 1: `(RebootRecord{}, false, nil)`;
//   - `detach.RebootFile == "reboot-requested"`, and the file mode is `0600`;
//   - the directory holds exactly that one file, with no temp file left;
//   - the JSON has exactly the keys `bootId` and `requestedAt`;
//   - step 4 returns `(rec, true, nil)`, with `RequestedAt` equal to the nanosecond;
//   - step 5 returns the `CUR` record (the record is replaced, not appended).
func TestTCSV5_RecordRebootWritesTheRecordAtomicallyWithMode(t *testing.T) {
	t.Skip("TC-SV-5: SKIP-only stub; un-skip when WI 1.2 lands")
}

// TC-SV-6: RetractReboot removes the record and is idempotent. Owner WI 1.2. Requirements: FR-11, CON-5.
//
// - Setup: `dir := t.TempDir()`.
// - Steps:
//  1. `RetractReboot(dir)` with no record.
//  2. `RecordReboot(dir, rec)`, then `RetractReboot(dir)`, then `ReadReboot(dir)`.
//  3. `RetractReboot(dir)` again.
//
// - Expected: Steps 1 and 3 return nil. After step 2, `ReadReboot` gives `ok == false`, and `os.ReadDir(dir)` is empty.
func TestTCSV6_RetractRebootRemovesTheRecordAndIsIdempotent(t *testing.T) {
	t.Skip("TC-SV-6: SKIP-only stub; un-skip when WI 1.2 lands")
}

// TC-SV-E1: RecordReboot under a concurrent reader never exposes a partial record. Owner WI 1.2. Requirements: FR-11, CON-5.
//
// - Setup: `dir := t.TempDir()`. One goroutine writes records alternating `PREV` and `CUR` 1,000 times, while 4 goroutines call `ReadReboot` in a loop.
// - Steps:
//  1. Run under `-race` until the writer finishes.
//
// - Expected: Every successful read is exactly the `PREV` record or the `CUR` record. No read returns a JSON error, and no reader sees `ok == false` after the first write.
func TestTCSVE1_RecordRebootUnderAConcurrentReaderNeverExposes(t *testing.T) {
	t.Skip("TC-SV-E1: SKIP-only stub; un-skip when WI 1.2 lands")
}

// TC-SV-ERR4: ReadReboot on a corrupt record is an error, and RecordReboot to a missing directory fails. Owner WI 1.2. Requirements: FR-11, CON-5.
//
// - Setup: `reboot-requested` containing `{"bootId":`; separately, a file of 1 MiB of `a`; and a nonexistent directory.
// - Steps:
//  1. `ReadReboot`.
//  2. `RecordReboot(missingDir, rec)`.
//
// - Expected:
//   - (1) a non-nil error naming the file for both corrupt cases, with `ok` reported true (present but unreadable), never `(zero, false, nil)`;
//   - (2) an error, with nothing created.
func TestTCSVERR4_ReadRebootOnACorruptRecordIsAn(t *testing.T) {
	t.Skip("TC-SV-ERR4: SKIP-only stub; un-skip when WI 1.2 lands")
}
