// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package server_test

import "testing"

// TC-SM-28 (WI 2.3; FR-6): The upload route cap is independent of limits.maxBodyBytes
func TestTCSM28_TheUploadRouteCapIsIndependent(t *testing.T) {
	// - Setup:
	//   - real server with the default `maxBodyBytes` of 1 MiB;
	//   - an F-UPL provider with upload cap 8 MiB;
	//   - a JSON long-running test op;
	//   - a token with all scopes.
	// - Steps:
	//   1. POST 2 MiB to the upload route with `Content-Type: application/octet-stream`.
	//   2. POST a 2 MiB JSON body to the JSON op.
	//   3. POST 8 MiB + 1 to the upload route.
	// - Expected:
	//   - Step 1 → 201, and `Receive` saw 2 MiB.
	//   - Step 2 → 413 `payload-too-large`, the existing behaviour, unchanged.
	//   - Step 3 → 413 `upload-too-large` with `maxBytes: 8388608`.
	t.Skip("TC-SM-28: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SM-29 (WI 2.3; FR-6): A slow upload longer than the base read timeout is not cut, over HTTP/2-over-TLS and HTTP/1.1
func TestTCSM29_ASlowUploadLongerThanThe(t *testing.T) {
	// - Setup:
	//   - server with the injectable base read timeout set to 1 s, so the real 30 s constant is
	//     replaced in the test only;
	//   - TLS enabled, with an h2 client (`ForceAttemptHTTP2`) and an h1 client;
	//   - upload cap 4 MiB;
	//   - a throttled reader sending 3 MiB at 1 MiB/s.
	// - Steps:
	//   1. Upload over h2.
	//   2. Upload over h1.
	//   3. Control: with the extension disabled by an unregistered route, a JSON POST trickled for 2 s
	//     is cut.
	// - Expected:
	//   - Steps 1 and 2 each → 201, about 3 s elapsed, and `Receive` saw 3 MiB.
	//   - Step 3: the control request fails with a read-deadline error, which proves the base timeout
	//     is in force.
	t.Skip("TC-SM-29: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SM-30 (WI 2.3; NFR-4): Streaming 64 MiB grows the heap by less than 8 MiB
func TestTCSM30_Streaming64MiBGrowsTheHeap(t *testing.T) {
	// - Setup: an F-UPL `Receive` that copies to `io.Discard` with a 32 KiB buffer; upload cap 64 MiB;
	//   a client body from `io.LimitReader(zeroReader, 64<<20)`.
	// - Steps:
	//   1. `runtime.GC()` and read `HeapInuse` as the baseline.
	//   2. Upload, sampling `HeapInuse` every 10 ms in a goroutine.
	//   3. Take the maximum sample.
	// - Expected:
	//   - 201.
	//   - `max - baseline < 8 MiB`.
	t.Skip("TC-SM-30: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SM-E2 (WI 2.3; FR-6, NFR-4): A chunked body exactly at the cap is accepted, and cap+1 is refused mid-stream with nothing kept
func TestTCSME2_AChunkedBodyExactlyAtThe(t *testing.T) {
	// - Setup: real server; an F-UPL `Receive` writing to a temp file and removing it on error; cap 64
	//   KiB; a client using `Transfer-Encoding: chunked` (body wrapped in `io.NopCloser`, with
	//   `ContentLength` -1).
	// - Steps:
	//   1. Send 65536 bytes.
	//   2. Send 65537 bytes.
	// - Expected:
	//   - Step 1 → 201.
	//   - Step 2 → 413 `upload-too-large`. `Receive` saw an error where `errors.Is(err,
	//     core.ErrUploadTooLarge)` holds, and its temp file is removed.
	t.Skip("TC-SM-E2: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SM-ERR16 (WI 2.3; FR-6, NFR-7): An unauthenticated upload is answered 401 without reading the body
func TestTCSMERR16_AnUnauthenticatedUploadIsAnswered401(t *testing.T) {
	// - Setup: real server. The client body sends 1 KiB, then blocks on a channel that the test
	//   releases only after the response arrives.
	// - Steps:
	//   1. POST the upload without `Authorization`.
	//   2. POST with a bad token.
	// - Expected:
	//   - Both are 401, received while the client body is still blocked, which proves the server did
	//     not wait for the body.
	//   - Admit and Receive are 0.
	t.Skip("TC-SM-ERR16: SKIP-only stub; un-skip when WI 2.3 lands")
}

// TC-SM-ERR17 (WI 2.3; FR-6, NFR-4): A client that disconnects mid-upload leaves no stored record and no partial state
func TestTCSMERR17_AClientThatDisconnectsMidUpload(t *testing.T) {
	// - Setup: an F-UPL `Receive` writing to a temp file that it removes on error.
	// - Steps:
	//   1. Send 512 KiB of a declared 1 MiB, then close the connection.
	// - Expected:
	//   - `Receive` returned an error, and its temp file is gone.
	//   - The audit has `requested` and no `stored` record.
	//   - The server goroutine count returns to baseline within 2 s.
	t.Skip("TC-SM-ERR17: SKIP-only stub; un-skip when WI 2.3 lands")
}
