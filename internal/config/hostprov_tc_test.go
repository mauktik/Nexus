// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-15, TC-HP-E4, TC-HP-ERR4, TC-HP-ERR5.
// Owner WIs: 1.3. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package config

import "testing"

// TC-HP-15: providers.host defaults, YAML and comments agree. Owner WI 1.3. Requirements: FR-1, FR-11, FR-13.
//
// - Setup: `Load(defaultConfigPath)`; `yaml.Node` of the file; `keyNode(doc, "providers.host")`.
// - Steps: compare `Default().Providers.Host.Settings`, the loaded section and the comments.
// - Expected:
//   - both equal `HostProviderConfig{Enabled: false, SampleInterval: 10s, HistoryDuration: 1h, SourceTimeout: 2s}`, and equal `spec.DefaultSampleInterval`, `spec.DefaultHistoryDuration`, `spec.DefaultSourceTimeout`;
//   - the section comment contains "changes nothing" (or "read-only"), "sampler", "in memory", "restart", `system:read` and "Linux";
//   - each of `enabled`, `sampleInterval`, `historyDuration`, `sourceTimeout` has a comment naming its default and its range (`2s`–`60s`, up to `24h`, `250ms`–`5s`).
func TestTCHP15_ProvidersHostDefaultsYAMLAndCommentsAgree(t *testing.T) {
	t.Skip("TC-HP-15: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-HP-E4: Enabling with no other keys takes the defaults. Owner WI 1.3. Requirements: FR-1, FR-11.
//
// - Setup: a YAML file with `providers: {host: {enabled: true}}`.
// - Steps: `Load`.
// - Expected: no error; settings `{true, 10s, 1h, 2s}`.
func TestTCHPE4_EnablingWithNoOtherKeysTakesThe(t *testing.T) {
	t.Skip("TC-HP-E4: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-HP-ERR4: An unknown key in providers.host fails with its location. Owner WI 1.3. Requirements: FR-1.
//
// - Setup: YAML with `providers:\n  host:\n    enabled: true\n    interval: 5s\n`.
// - Steps: `Load(path)`; `daemon.CheckConfig(path)`.
// - Expected: both fail; the message names `providers.host.interval` and `<path>:4`.
func TestTCHPERR4_AnUnknownKeyInProvidersHostFails(t *testing.T) {
	t.Skip("TC-HP-ERR4: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-HP-ERR5: Each bound fails with its location and names the bound. Owner WI 1.3. Requirements: FR-11, FR-12, FR-13.
//
// - Setup: a table of YAML sections.
// - Steps: `Load` each.
// - Expected:
//   - fail: `sampleInterval 1s`, `61s`; `historyDuration 25h`; `historyDuration 5s` with `sampleInterval 10s`; `sourceTimeout 249ms`, `5001ms`;
//   - pass: `sampleInterval 2s`, `60s`; `historyDuration 24h`; `historyDuration 10s` with `sampleInterval 10s`; `sourceTimeout 250ms`, `5s`;
//   - each failure names the key, its `file:line`, and the bound (`2s`, `60s`, `24h`, `sampleInterval`, `250ms`, `5s`).
func TestTCHPERR5_EachBoundFailsWithItsLocationAnd(t *testing.T) {
	t.Skip("TC-HP-ERR5: SKIP-only stub; un-skip when WI 1.3 lands")
}
