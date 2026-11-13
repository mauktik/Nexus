// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package config

import "testing"

// TC-SM-41 (WI 3.1; FR-19, FR-1, FR-10, FR-6): providers.nexusd defaults and the deploy default YAML section
func TestTCSM41_ProvidersNexusdDefaultsAndTheDeploy(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. Call `config.Default()`.
	//   2. `config.Load("deploy/nexusd.default.yaml")`.
	//   3. Run the existing "default yaml lists every key" test.
	// - Expected:
	//   - `Providers.Nexusd.Settings` is `{Enabled: false, HealthTimeout: 30s, Update:
	//     {MaxUploadBytes: 67108864, TrustedKeys: []}}`.
	//   - The default YAML loads with the same values, and its `providers.nexusd` section has
	//     `enabled: false` and `trustedKeys: []`.
	//   - A comment mentions `make release-key`.
	//   - The every-key test passes, which includes the four new keys.
	t.Skip("TC-SM-41: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-SM-42 (WI 3.1; FR-15, FR-10, FR-6): Bound values are accepted and a valid key round-trips
func TestTCSM42_BoundValuesAreAcceptedAndA(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. Load configs with:
	//      - `healthTimeout` 5s and 10m;
	//      - `maxUploadBytes` 1048576 and 268435456;
	//      - `trustedKeys` with 0 keys, 1 key, and 2 distinct keys.
	// - Expected:
	//   - Every config loads without error.
	//   - The parsed keys equal `release.ParsePublicKey` of each entry.
	t.Skip("TC-SM-42: SKIP-only stub; un-skip when WI 3.1 lands")
}

// TC-SM-ERR23 (WI 3.1; FR-15, FR-10, FR-6, FR-3): Config validation errors are located and never echo a key value
func TestTCSMERR23_ConfigValidationErrorsAreLocatedAnd(t *testing.T) {
	// - Setup: YAML strings with:
	//   - `healthTimeout: 4s`, then `11m`;
	//   - `maxUploadBytes: 1048575`, then `268435457`;
	//   - `trustedKeys: ["@@@"]`;
	//   - `trustedKeys: [<b64 of 31 bytes>]`;
	//   - `trustedKeys: [K, K]`;
	//   - `providers.nexusd.bogus: 1`.
	// - Steps:
	//   1. `config.Load` each.
	// - Expected:
	//   - Each gives one `config.Error` with `File`, `Line` and `Column` set and `Path` like
	//     `providers.nexusd.update.trustedKeys[1]`.
	//   - The message names the bound or index.
	//   - No message contains `@@@`, K's base64 or the 31-byte value.
	t.Skip("TC-SM-ERR23: SKIP-only stub; un-skip when WI 3.1 lands")
}
