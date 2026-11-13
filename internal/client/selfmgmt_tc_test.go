// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package client

import "testing"

// TC-SM-35 (WI 2.4; FR-6, FR-13): client.Upload sets Content-Length, honours pinning and derives its deadline from size
func TestTCSM35_ClientUploadSetsContentLengthHonours(t *testing.T) {
	// - Setup: an `httptest` TLS server recording headers; a client pinned to its fingerprint; a
	//   second client pinned to a wrong fingerprint.
	// - Steps:
	//   1. `Upload(ctx, UploadRequest{Path, Query, ContentType: "application/gzip", Body, Size: 2
	//     MiB})`.
	//   2. Upload with the wrong pin.
	//   3. Call the deadline helper for sizes 0, 1 MiB and 64 MiB.
	// - Expected:
	//   - The server saw `Content-Length: 2097152`, the given Content-Type and the query.
	//   - A 201 problem-free response is returned.
	//   - Step 2 fails with an error where `errors.As(err, *client.PinMismatchError)` holds.
	//   - The deadlines are 30 s, 31 s and 94 s (`30s + size/1MiB`).
	t.Skip("TC-SM-35: SKIP-only stub; un-skip when WI 2.4 lands")
}

// TC-SM-64 (WI 4.1; FR-10): client.PinnedTLS is exported and normalizes fingerprints
func TestTCSM64_ClientPinnedTLSIsExportedAndNormalizes(t *testing.T) {
	// - Setup: an `httptest` TLS server; its leaf SHA-256 fingerprint F in lowercase hex.
	// - Steps:
	//   1. `PinnedTLS(F)`, `PinnedTLS(upper(F))` and `PinnedTLS(colon-separated F)`, each followed by
	//     a GET.
	//   2. `PinnedTLS(wrong)` followed by a GET.
	//   3. Call `PinnedTLS("zz")`.
	//   4. Run the existing InsecureSkipVerify static check.
	// - Expected:
	//   - Step 1: all three GETs succeed.
	//   - Step 2 fails with `*PinMismatchError`.
	//   - Step 3 returns an error.
	//   - Step 4 passes: no bare `InsecureSkipVerify` exists without `VerifyConnection`.
	t.Skip("TC-SM-64: SKIP-only stub; un-skip when WI 4.1 lands")
}
