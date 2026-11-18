// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-117, TC-SV-118, TC-SV-119.
// Owner WIs: 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package services

import "testing"

// TC-SV-117: builtinProviders and providerConfigs include services, and the daemon injects its three hooks. Owner WI 5.1. Requirements: FR-1, CON-4.
//
// - Setup: The production `builtinProviders` (exported for tests by the existing pattern), and `startInProcess` with `providers.services.enabled: true`.
// - Steps:
//  1. Find `services` in `builtinProviders`.
//  2. Assert interface conformance.
//  3. After start, read the provider's injected fields through an `export_test.go` accessor.
//
// - Expected:
//   - exactly one `services` provider;
//   - `providerConfigs` has a `"services"` row;
//   - it implements `provider.OpsDirUser`, `provider.DaemonUser` and `provider.OperationsUser`;
//   - all three were injected non-nil, and the `Operations` is the daemon's manager.
func TestTCSV117_BuiltinProvidersAndProviderConfigsIncludeServicesAndThe(t *testing.T) {
	t.Skip("TC-SV-117: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-118: nexusd routes services-helper to the helper. Owner WI 5.1. Requirements: FR-1, CON-4.
//
// - Setup: `go build -o $TMP/nexusd ./cmd/nexusd` (skipped under `skipUnderMake`'s rule, as other binary tests are).
// - Steps:
//  1. `$TMP/nexusd services-helper --help`.
//  2. `$TMP/nexusd services-helper run --dir /nonexistent`.
//
// - Expected:
//   - (1) the helper's own usage, naming `run --dir`, and not the main usage;
//   - (2) a non-zero exit whose stderr names the directory.
func TestTCSV118_NexusdRoutesServicesHelperToTheHelper(t *testing.T) {
	t.Skip("TC-SV-118: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-119: The reboot op declares CompletesAcrossReboot, its binding carries AfterReboot, and every op is bound once. Owner WI 5.1. Requirements: FR-11, FR-15, CON-4.
//
// - Setup: `New(Options{…})`.
// - Steps:
//  1. Collect `Bindings()` by op ID.
//
// - Expected:
//   - exactly the eight `servicesspec` op IDs, each once;
//   - `spec.Reboot.CompletesAcrossReboot` is true, and its binding's `DetachedFuncs.AfterReboot` is non-nil;
//   - the six control ops are detached, list and get are sync, and logs is a stream.
func TestTCSV119_TheRebootOpDeclaresCompletesAcrossRebootItsBinding(t *testing.T) {
	t.Skip("TC-SV-119: SKIP-only stub; un-skip when WI 5.1 lands")
}
