// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package core_test

import "testing"

// TC-SM-26 (WI 2.2; FR-6, NFR-7, FR-16): Dispatcher upload path runs scope, then validation, then Admit, then requested audit, then Receive, and answers 201
func TestTCSM26_DispatcherUploadPathRunsScopeThen(t *testing.T) {
	// - Setup: an F-UPL provider bound with `provider.Upload(op, UploadSpec{Admit, Receive})`, on the
	//   real `core.New` with a spy audit sink recording order.
	// - Steps:
	//   1. `Invoke` with a principal holding `tp:admin`, input `{signature: <valid>}`, and `Upload:
	//     &UploadBody{Reader: bytes.NewReader(1 KiB), ContentType: "application/gzip", ContentLength:
	//     1024}`.
	// - Expected:
	//   - `InvokeResult.Status == 201`.
	//   - The body is the provider's output.
	//   - The recorded order is exactly `admit`, then `audit:requested`, then `receive`.
	//   - `Receive` read exactly the 1024 bytes sent, which are equal.
	t.Skip("TC-SM-26: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-27 (WI 2.2; FR-6): The binding's MaxBytes caps uploads, and a value above the op ceiling is clamped
func TestTCSM27_TheBindingSMaxBytesCapsUploads(t *testing.T) {
	// - Setup: op ceiling 1 MiB. Binding `MaxBytes` returns 4 KiB in run 1 and 8 MiB in run 2.
	// - Steps:
	//   1. Run 1: invoke with `ContentLength` 4096, then 4097.
	//   2. Run 2: invoke with `ContentLength` 1 MiB, then 1 MiB + 1.
	// - Expected:
	//   - Run 1: 4096 → 201; 4097 → 413 `upload-too-large` with `maxBytes: 4096`.
	//   - Run 2: 1 MiB → 201; 1 MiB + 1 → 413 with `maxBytes: 1048576`. The ceiling wins (`never above
	//     it`).
	t.Skip("TC-SM-27: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-49 (WI 3.4; FR-16, FR-6): Facts are attached to refused and requested records, and an upload writes a stored record
func TestTCSM49_FactsAreAttachedToRefusedAnd(t *testing.T) {
	// - Setup:
	//   - real core with the real `audit.Log` in a temp dir;
	//   - an F-UPL provider whose `Receive` calls `audit.AddFact(ctx, "sha256", h)`;
	//   - a long-running test op whose `Admit` adds `k=v` and then refuses or admits on demand.
	// - Steps:
	//   1. Upload.
	//   2. Invoke the long-running op set to refuse.
	//   3. Invoke it again set to admit.
	//   4. Read `audit.jsonl`.
	// - Expected:
	//   - Upload: a `requested` record, then a `stored` record with the same `requestId`, where
	//     `stored.facts.sha256 == h`.
	//   - Refuse: a `refused` record with `facts: {k: v}` and the problem slug.
	//   - Admit: the `requested` record has `facts: {k: v}`.
	t.Skip("TC-SM-49: SKIP-only stub; un-skip when WI 3.4 lands")
}

// TC-SM-E1 (WI 2.2; FR-6): A declared Content-Length exactly at the cap is accepted, and cap+1 is refused unread
func TestTCSME1_ADeclaredContentLengthExactlyAt(t *testing.T) {
	// - Setup: an F-UPL provider with cap 64 KiB.
	// - Steps:
	//   1. Invoke with `ContentLength` 65536 and a body of 65536 bytes.
	//   2. Invoke with 65537.
	// - Expected:
	//   - Step 1 → 201.
	//   - Step 2 → 413 `upload-too-large` with `maxBytes: 65536`. The counting reader shows 0 bytes
	//     read, and Admit was not called.
	t.Skip("TC-SM-E1: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-ERR9 (WI 2.2; FR-6, FR-1, NFR-7): Unauthorized and forbidden upload requests never reach Admit, Receive or the body
func TestTCSMERR9_UnauthorizedAndForbiddenUploadRequestsNever(t *testing.T) {
	// - Setup: an F-UPL provider with a counting reader.
	// - Steps:
	//   1. Invoke with no principal.
	//   2. Invoke with a principal holding `tp:read` only.
	// - Expected:
	//   - Step 1: 401.
	//   - Step 2: 403 with `requiredScope: "tp:admin"`.
	//   - Both: Admit 0, Receive 0, bytes read 0, and no `requested` record. The 403 is a `refused`
	//     audit record, as for other ops.
	t.Skip("TC-SM-ERR9: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-ERR10 (WI 2.2; FR-6): A wrong Content-Type is validation-failed and the body is unread
func TestTCSMERR10_AWrongContentTypeIsValidation(t *testing.T) {
	// - Setup: an F-UPL provider.
	// - Steps:
	//   1. Invoke with `ContentType` set to each of `application/json`, `text/plain` and `""`.
	// - Expected:
	//   - 400 `validation-failed`, with `errors[0].message == "Content-Type must be one of
	//     application/octet-stream, application/gzip"`.
	//   - 0 bytes read, and Admit not called.
	t.Skip("TC-SM-ERR10: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-ERR11 (WI 2.2; FR-6, NFR-4): A declared oversize body is refused 413 with zero reads and no Admit
func TestTCSMERR11_ADeclaredOversizeBodyIsRefused(t *testing.T) {
	// - Setup: cap 1 MiB; a reader that fails the test if read.
	// - Steps:
	//   1. Invoke with `ContentLength: 2 MiB`.
	// - Expected:
	//   - 413 `upload-too-large` with `maxBytes: 1048576`.
	//   - Reader untouched, Admit 0, and no `requested` record.
	t.Skip("TC-SM-ERR11: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-ERR12 (WI 2.2; FR-6): A streamed oversize body maps to upload-too-large
func TestTCSMERR12_AStreamedOversizeBodyMapsTo(t *testing.T) {
	// - Setup: `ContentLength` -1; a reader that returns `fmt.Errorf("read: %w",
	//   core.ErrUploadTooLarge)` after 10 bytes. The provider `Receive` returns the read error wrapped
	//   once more.
	// - Steps:
	//   1. Invoke.
	//   2. Repeat with a `Receive` returning `errors.New("disk full")`.
	// - Expected:
	//   - Step 1: 413 `upload-too-large`, and no `stored` record.
	//   - Step 2: the existing post-`requested` failure mapping (`refuseAfter`), and no 201.
	t.Skip("TC-SM-ERR12: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-ERR13 (WI 2.2; FR-16, FR-6): An audit write failure refuses the upload with audit-unavailable before Receive
func TestTCSMERR13_AnAuditWriteFailureRefusesThe(t *testing.T) {
	// - Setup: an audit sink that fails on the `requested` write.
	// - Steps:
	//   1. Invoke the upload.
	// - Expected:
	//   - 503 `audit-unavailable`.
	//   - Receive 0, and bytes read 0.
	t.Skip("TC-SM-ERR13: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-ERR14 (WI 2.2; FR-6, FR-16): An Admit refusal returns its problem with a refused audit record
func TestTCSMERR14_AnAdmitRefusalReturnsItsProblem(t *testing.T) {
	// - Setup: F-UPL `Admit` returns `problem.New(problem.Conflict, …)`.
	// - Steps:
	//   1. Invoke the upload.
	// - Expected:
	//   - 409 `conflict`.
	//   - Receive 0, and bytes read 0.
	//   - The audit has `refused` with slug `conflict` and no `requested` record.
	t.Skip("TC-SM-ERR14: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-ERR15 (WI 2.2; FR-13, CON-3): The registry rejects bad CLI.Upload references
func TestTCSMERR15_TheRegistryRejectsBadCLIUpload(t *testing.T) {
	// - Setup: a provider whose update op's `CLI.Upload.Op` is:
	//   - (a) a missing ID;
	//   - (b) a `KindList` op;
	//   - (c) another provider's upload op;
	//   - (d) a declared but unbound upload op;
	//   - (e) an upload op whose output lacks the `uploadId` string field.
	// - Steps:
	//   1. Call `core.New` for each.
	// - Expected: each returns an error naming the op and the reason, and no registry is built.
	t.Skip("TC-SM-ERR15: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-SM-ERR26 (WI 3.4; FR-16, FR-6): A failed stored write is logged and the upload still answers 201
func TestTCSMERR26_AFailedStoredWriteIsLogged(t *testing.T) {
	// - Setup: an audit sink failing only on `stored`; a captured `slog`.
	// - Steps:
	//   1. Upload.
	// - Expected:
	//   - 201.
	//   - The log has an error entry naming the request ID and `stored`, at the same level the
	//     existing `note` failure uses.
	t.Skip("TC-SM-ERR26: SKIP-only stub; un-skip when WI 3.4 lands")
}
