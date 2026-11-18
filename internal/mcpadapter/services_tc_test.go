// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-123, TC-SV-124, TC-SV-149, TC-SV-151, TC-SV-152.
// Owner WIs: 5.1 (TC-SV-123, TC-SV-124), 5.4 (TC-SV-149, TC-SV-151, TC-SV-152). Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package mcpadapter_test

import "testing"

// TC-SV-123: MCP lists exactly four services tools within the budget, and no reboot tool. Owner WI 5.1. Requirements: FR-15, NFR-6, ASM-6.
//
// - Setup: `catalogTools(t)` and `toolBudget`.
// - Steps:
//  1. Filter the tools that come from `services.*` ops.
//  2. Compute the budget with and without them.
//
// - Expected:
//   - names are exactly `list_services`, `get_service`, `get_service_logs` and `control_service`;
//   - the first three have `readOnlyHint: true`, and `control_service` has `readOnlyHint: false` and `destructiveHint: true`;
//   - each description ends with the untrusted suffix;
//   - no tool name contains `reboot` or `machine`;
//   - the services delta is ≤ 800 characters and ≤ 200 tokens, and the total is ≤ 3,100 tokens;
//   - `TestToolBudget`'s pinned count is 72.
func TestTCSV123_MCPListsExactlyFourServicesToolsWithin(t *testing.T) {
	t.Skip("TC-SV-123: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-124: get_service_logs is since-paged with the default tail. Owner WI 5.1. Requirements: FR-6, FR-15.
//
// - Setup: The services provider over F-HOSTS with TC-SV-85's 2,500-line journal.
// - Steps:
//  1. Call `get_service_logs {name: nexusd}`.
//  2. Call it with `since` from the response's continuation.
//
// - Expected:
//   - (1) the newest 200 lines, with a continuation value;
//   - (2) the lines after it, oldest first, at most 1,000;
//   - the input schema of the tool has `since` and `tail`, and its description carries the since-paged suffix.
func TestTCSV124_GetServiceLogsIsSincePagedWith(t *testing.T) {
	t.Skip("TC-SV-124: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-149: get_service_logs on OpenRC file logs never hands back a continuation that answers 400. Owner WI 5.4. Requirements: FR-6, FR-15, ASM-5, A-38.
//
// - Setup: The services provider over F-HOSTS and F-ROOTS from `t1-alpine-openrc`, with `nx-svc-test`'s `output_log` and `error_log` pointing at temp files holding TC-SV-86's 3,000 `out-*` lines (200 bytes each) and 10 `err-*` lines. The MCP adapter is the real one (`mcpadapter.New` over the real registry and Dispatcher). Subcase (2) builds it with `Options.LogPageBytes: 16 << 10`, so the page is cut by the page size whatever the exact count of the provider's 64 KiB bound: the provider returns about 327 lines (64 KiB) for `tail: 1000`, well over 16 KiB.
// - Steps:
//  1. Call `get_service_logs {name: nx-svc-test}` (default page size).
//  2. Call `get_service_logs {name: nx-svc-test, tail: 1000}` on the 16 KiB adapter.
//  3. Call `get_service_logs {name: nx-svc-test, tail: 50}` on the 16 KiB adapter (the narrowing `docs/mcp.md` tells the agent to use).
//
// - Expected:
//
//   - no call answers 400 or any other error, and no call is ever made with a `since` taken from a result: no page carries a continuation;
//
//   - (1) is a success with the newest 200 `out-*` lines and the 10 `err-*` lines (as TC-SV-86), `truncated: false`, and the result JSON has no `nextOffset` key;
//
//   - (2) has `truncated: true` and no `nextOffset` key; its `out-*` frames are a contiguous ascending run of lines from the provider's tail window (each `out-N` has N ≥ 2000), fewer than the provider returned;
//
//   - (3) has `truncated: false`, no `nextOffset` key, and its last `out-*` frame is `out-2999` (the newest lines).
//
//     The `docs/mcp.md` wording check moved to TC-SV-130 (WI 5.2) in Revision 1, because WI 5.4 lands before the docs.
func TestTCSV149_GetServiceLogsOnOpenRCFileLogs(t *testing.T) {
	t.Skip("TC-SV-149: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SV-151: The since-pager gives no nextOffset for a page that holds an untimed frame, cut or not. Owner WI 5.4. Requirements: FR-6, FR-15, A-38.
//
// - Setup: A planted `ModeStream` op whose input has `since` and `tail`, so the adapter treats it as since-paged (`internal/mcpadapter/tools.go:130-140`). Its handler emits a scripted `[]apitypes.Frame` (the real frame type: an untimed frame has the zero `T`, which `omitzero` leaves out). The real adapter (`mcpadapter.New` over a real registry and Dispatcher) with `Options.LogPageBytes: 64`. Each frame's `D` is 10 ASCII bytes. Subcases:
//   - (a) 3 untimed frames (30 bytes, not cut);
//   - (b) 20 untimed frames (200 bytes, cut by the 64-byte page);
//   - (c) 1 timed frame followed by 2 untimed frames (not cut);
//   - (d) 1 timed frame followed by 19 untimed frames (cut);
//   - (e) 3 timed frames at `t0`, `t0+1s`, `t0+2s` (not cut), the unchanged timed case.
//
// - Steps:
//  1. Call the op's tool once per subcase, with no `since`.
//
// - Expected:
//   - (a) to (d): the result JSON has no `nextOffset` key; (a) and (c) have `truncated: false` and every frame; (b) and (d) have `truncated: true` and a prefix of the emitted frames whose encoded text fits 64 bytes;
//   - (e): `nextOffset` equals `formatTime(t0+2s)` (the pager's own format) and `truncated: false`, exactly as before WI 5.4;
//   - the existing since-pager tests in `internal/mcpadapter/optools_internal_test.go` (`TestPageBuilder`, `TestPageStopsStream`, `TestSincePagingAlwaysAdvances`, `TestSincePagingKeepsALineThatFitsAPage`, `TestPageLimitCountsEncodedBytes`, `TestPageLimitHoldsOnTheWire`) and `TestLogToolsReturnOneBoundedPage` pass unchanged: they are the regression for timed frames.
func TestTCSV151_TheSincePagerGivesNoNextOffsetForUntimed(t *testing.T) {
	t.Skip("TC-SV-151: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SV-152: An MCP page that fills ends the audited stream completed with no clientDisconnected fact. Owner WI 5.4. Requirements: FR-16, CON-4, A-44.
//
// - Setup:
//   - A planted `ModeStream`, `RiskReadOnly`, `Audited` op whose input has `since` and `tail`, so the adapter treats it as since-paged (`internal/mcpadapter/tools.go:130-140`); the TC-SV-151 planted op with `Audited` added.
//   - The real adapter (`mcpadapter.New` over a real registry and Dispatcher) with `Options.LogPageBytes: 64`, and a real `audit.Log` over `t.TempDir()`, read back as TC-SV-23 reads it.
//   - Its handler emits `[]apitypes.Frame` (the real frame type) with `T` at `t0 + i*1s` and a 10-byte ASCII `D`. Subcases:
//   - (a) 20 frames: the 64-byte page fills. The handler stops at the first `emit` error and returns nil;
//   - (b) as (a), but after the `emit` error the handler waits for `ctx` to be done and returns `ctx.Err()` (what the handler returns after the stop does not matter);
//   - (c) 3 frames (30 bytes): the page does not fill. The handler returns nil.
//
// - Steps:
//  1. Call the op's tool once per subcase, with no `since`.
//  2. Read the audit records.
//  3. Separately, wrap the sentinel: `fmt.Errorf("%w: page full", core.ErrStreamStopped)`.
//
// - Expected:
//   - (a) and (b): the tool result has `truncated: true`; the audit log holds exactly two records, `requested` then `completed`, and `completed` names `requested`'s ID and carries no `clientDisconnected` fact;
//   - (c): `truncated: false` with all three frames, and the same two records with no `clientDisconnected` fact, exactly as before WI 5.4;
//   - step 3: `errors.Is(err, core.ErrStreamStopped)` is true;
//   - TC-SV-23, TC-SV-24, TC-SV-25 and TC-SV-E5 pass unchanged: a real client disconnect still records `clientDisconnected=true` (A-40), and REST never returns the sentinel.
func TestTCSV152_AnMCPPageThatFillsEndsTheAudited(t *testing.T) {
	t.Skip("TC-SV-152: SKIP-only stub; un-skip when WI 5.4 lands")
}
