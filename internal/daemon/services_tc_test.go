// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-21, TC-SV-120, TC-SV-127.
// Owner WIs: 1.3, 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package daemon

import "testing"

// TC-SV-21: daemon.New injects the manager into every OperationsUser before serving. Owner WI 1.3. Requirements: FR-10, CON-4.
//
// - Setup: A fake provider registered under a real `providerConfigs` key (the `reloadProvider` pattern) that implements `provider.OperationsUser`, recording each `UseOperations` call and the time.
// - Steps:
//  1. `startInProcess`.
//  2. Start an operation of another provider.
//  3. Call the injected `Operations.Active()` from the fake.
//
// - Expected:
//   - `UseOperations` was called exactly once, before the listener accepted its first request;
//   - the injected value is the daemon's `*ops.Manager` (the same pointer used for operations);
//   - `Active()` returns the operation started in step 2.
func TestTCSV21_DaemonNewInjectsTheManagerIntoEvery(t *testing.T) {
	t.Skip("TC-SV-21: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SV-120: A SIGHUP reload applies a changed services section without a restart. Owner WI 5.1. Requirements: FR-1, FR-8.
//
// - Setup: `startInProcess` with `providers.services: {enabled: true, control: []}` and the provider's `Run` seam set to an F-HOSTS of `t1-ubuntu-systemd` (through the daemon's test hook for provider options).
// - Steps:
//  1. Read `controlAllowList` from capabilities.
//  2. Rewrite the file with `control: [{name: nx-svc-test}]` and send SIGHUP.
//  3. Read capabilities again, and the PID.
//
// - Expected: Before: `[]`. After: `[{name: nx-svc-test, status: ok, …}]`. The same daemon instance, with no restart.
func TestTCSV120_ASIGHUPReloadAppliesAChangedServices(t *testing.T) {
	t.Skip("TC-SV-120: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-127: OpenAPI documents the eight routes with their scopes and problem types. Owner WI 5.1. Requirements: FR-15, NFR-8.
//
// - Setup: The daemon's OpenAPI document (the `openapi_test.go` pattern).
// - Steps:
//  1. Find the eight paths under `/v1/providers/services`.
//
// - Expected:
//   - each path has its method, its security scope, and `x-untrusted` (or the existing untrusted marker);
//   - start, stop, restart and control list 409 (`not-allowed`, `protected-service`, `conflict`) and 404;
//   - the reboot lists 400, 409 (`reboot-unavailable`, `nexusd-not-enabled-at-boot`, `operations-running`, `conflict`) and 422 (`config-invalid`);
//   - logs lists 422 `logs-unavailable`;
//   - start, stop, restart and control accept `Idempotency-Key`.
func TestTCSV127_OpenAPIDocumentsTheEightRoutesWithTheir(t *testing.T) {
	t.Skip("TC-SV-127: SKIP-only stub; un-skip when WI 5.1 lands")
}
