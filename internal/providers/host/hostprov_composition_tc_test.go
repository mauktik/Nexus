// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-108, TC-HP-110, TC-HP-E27, TC-HP-ERR29.
// Owner WIs: 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package host_test

import "testing"

// TC-HP-108: Composition: capabilities, OpenAPI, CLI catalog and the Background wiring. Owner WI 5.1. Requirements: FR-1, FR-3, FR-14, FR-11, CON-4.
//
// - Setup: an in-process daemon from a config enabling only `host` (external test package `host_test`, modelled on `internal/providers/nexusd/composition_test.go`).
// - Steps: `GET /v1/capabilities?refresh=true`; `GET /v1/openapi.json`; the CLI's generated command tree; inspect `builtinProviders("h")` (through the exported composition path the nexusd test uses) for name `host`.
// - Expected: capabilities list `host`: on linux `enabled`, on darwin `unavailable` with the TC-HP-50 reason; OpenAPI has exactly the ten `GET /v1/providers/host/*` paths, each requiring `system:read`; the CLI has `hostsummary get` … `hostmetric get`; the `host` provider value implements `provider.Background`; `providerConfigs` has a `host` entry whose `Enabled` follows the YAML.
func TestTCHP108_CompositionCapabilitiesOpenAPICLICatalogAndThe(t *testing.T) {
	t.Skip("TC-HP-108: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-HP-110: validate-config with the host provider enabled never starts its sampler. Owner WI 5.1. Requirements: FR-1, NFR-2.
//
// - Setup: a config file with `providers.host: {enabled: true, sampleInterval: 2s}`; goroutine baseline.
// - Steps: `daemon.CheckConfig(path)`; wait 3 s (longer than one interval).
// - Expected: no error; goroutines at baseline; no "sampler started" log line in the captured logger.
func TestTCHP110_ValidateConfigWithTheHostProviderEnabled(t *testing.T) {
	t.Skip("TC-HP-110: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-HP-E27: The provider is disabled by default. Owner WI 5.1. Requirements: FR-1, NFR-2.
//
// - Setup: an in-process daemon from `deploy/nexusd.default.yaml` (listen rewritten to a free port); goroutine baseline after READY.
// - Steps: capabilities; `GET /v1/providers/host/summary`.
// - Expected: `host` status `disabled`; the op answers `capability-unavailable`; no "sampler started" log; goroutines stable over 2 s.
func TestTCHPE27_TheProviderIsDisabledByDefault(t *testing.T) {
	t.Skip("TC-HP-E27: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-HP-ERR29: Host operations refuse tokens without system:read on every surface. Owner WI 5.1. Requirements: FR-2.
//
// - Setup: in-process daemon with host enabled; tokens with `containers:read` only and with `system:read`.
// - Steps: `GET /v1/providers/host/summary` with each; MCP `tools/call get_host_summary` with each; `nexus hostsummary get` with the first.
// - Expected: without the scope: REST 403 `insufficient-scope`, MCP tool error with the same slug, CLI non-zero exit naming `system:read`; with it: 200 and a result; `system.capabilities.get` is still callable with the `containers:read` token (`system:read` does not gate `system.*`).
func TestTCHPERR29_HostOperationsRefuseTokensWithoutSystemRead(t *testing.T) {
	t.Skip("TC-HP-ERR29: SKIP-only stub; un-skip when WI 5.1 lands")
}
