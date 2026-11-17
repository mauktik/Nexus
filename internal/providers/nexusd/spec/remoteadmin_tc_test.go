// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-26, TC-RA-27, TC-RA-60, TC-RA-61, TC-RA-67, TC-RA-68, TC-RA-E7, TC-RA-E17.
// Owner WIs: 2.2, 3.3, 4.1. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package spec_test

import "testing"

// TC-RA-26: The three token ops are declared as the catalog table states and match the golden rows. Owner WI 2.2. Requirements: FR-1, FR-2, FR-3, FR-10, FR-11, ASM-1, ASM-2.
//
//   - Setup: `spec.Spec()`.
//   - Steps:
//     1. Find `nexusd.tokens.list`, `nexusd.tokens.add` and `nexusd.tokens.remove`.
//   - Expected:
//   - Routes: `GET /providers/nexusd/tokens`, `POST /providers/nexusd/tokens/{name}/add` and `POST .../{name}/remove`.
//   - All three: scope `nexusd:admin`, `MCP == nil`, CLI noun `nexusdtoken`.
//   - list: read-only, `Audited`, verb `get`, columns `name`, `properties.tokenId`, `properties.scopes`, `properties.expires`, `properties.self`.
//   - add: mutating, `ModeSync`, `SyncMutation`, `SecretOutput == "token"`, verb `add`, positional `name`, columns `name`, `tokenId`, `token`, `expires`.
//   - remove: destructive, `SyncMutation`, `CLI.EchoWarnings`, verb `remove`, positional `name`.
//   - Order: the ops appear after `nexusd.uploads.add` in `Spec()` order.
//   - Descriptions: each contains "nexusd:admin" and "root-equivalent". add's also contains "returned once".
//   - Catalog: the golden table has these three rows, and its row count has risen by 3.
func TestTCRA26_TheThreeTokenOpsAreDeclaredAsTheCatalogTable(t *testing.T) {
	t.Skip("TC-RA-26: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-RA-27: Token name and scope patterns equal the auth package's regexes. Owner WI 2.2. Requirements: FR-1, NFR-5.
//
//   - Setup: The test imports `internal/auth` (test only) for its name and scope regex strings, which `auth` exports or exposes through a test accessor added by WI 2.1/2.2.
//   - Steps:
//     1. Compare the schema `pattern` strings of `name` and `scopes.items` with auth's.
//     2. Run an accept/reject table through both: `ci`, `a.b-c_d`, a 64-char name, a 65-char name, `-x`, `a b`, `abcdefabcdef` (a 12-hex ID), `containers:read`, `x:`, `X:read`.
//   - Expected: The strings are equal, and every table row gets the same verdict from both regexes. `go list -deps ./cmd/nexus` does not include `internal/auth`.
func TestTCRA27_TokenNameAndScopePatternsEqualTheAuthPackageS(t *testing.T) {
	t.Skip("TC-RA-27: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-RA-60: The config get ops are declared as the catalog table states. Owner WI 3.3. Requirements: FR-6, FR-10, FR-11, ASM-5, ASM-1.
//
//   - Setup: `spec.Spec()`.
//   - Steps:
//     1. Find `nexusd.config.list` and `nexusd.config.get`.
//   - Expected:
//   - Routes: `GET /providers/nexusd/config` and `GET /providers/nexusd/config/{name}`.
//   - Declarations: scope `nexusd:admin`, read-only, sync, `Audited`, `MCP == nil`, noun `nexusdconfig`, verb `get` (one merged command), positional `name` on get.
//   - Columns: `name`, `properties.value`, `properties.effective`, `properties.source`.
//   - Descriptions: each states "on-disk", `[REDACTED]`, and "404" with "nearest" parent.
//   - Catalog: the golden table has 2 more rows, and `catalogStubs` binds both.
func TestTCRA60_TheConfigGetOpsAreDeclaredAsTheCatalogTable(t *testing.T) {
	t.Skip("TC-RA-60: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-RA-61: The config key-name pattern accepts every schema key and rejects others. Owner WI 3.3. Requirements: FR-6, NFR-5.
//
//   - Setup: `config.LeafKeys()` and mapping keys (test-only import).
//   - Steps:
//     1. Match each schema path against the `name` pattern.
//     2. Match the rejects: `Providers.x`, `a..b`, `.a`, `a.`, `a/b`, `../etc`, 9 segments, a 65-char segment, `a b`.
//   - Expected: Every schema path matches and every reject fails. `TestEveryStringInputIsConstrained` passes.
func TestTCRA61_TheConfigKeyNamePatternAcceptsEverySchemaKeyAnd(t *testing.T) {
	t.Skip("TC-RA-61: SKIP-only stub; un-skip when WI 3.3 lands")
}

// TC-RA-67: The config set op is declared as the catalog table states. Owner WI 4.1. Requirements: FR-7, FR-9, FR-10, FR-11, NFR-5, ASM-1.
//
//   - Setup: `spec.Spec()`.
//   - Steps:
//     1. Find `nexusd.actions.update-config`.
//     2. Build its generated input JSON schema.
//     3. `json.Marshal(spec.ConfigUpdateInput{Changes: []string{"a=b"}, BaseSHA256: "x"})`.
//   - Expected:
//   - Declaration: `KindProviderAction`, action `update-config`, route `POST /providers/nexusd/actions/update-config`, mutating, long-running, `CancelQueuedOnly`, `CompletesAtAdmission` (D-7), scope `nexusd:admin`, `MCP == nil`.
//   - CLI: `{Noun: nexusdconfig, Verb: update, Variadic: changes, EchoWarnings: true}`.
//   - `changes` schema: `minItems` 1, `maxItems` `spec.MaxConfigChanges`, item `maxLength` `MaxConfigValueBytes + 520`.
//   - No baseline on the wire: the input schema has no `baseSha256` property (or any property other than `changes`), and step 3's JSON is exactly `{"changes":["a=b"]}`: `BaseSHA256` is tagged `json:"-"` (D-7, A-15).
//   - Wire types: `DaemonProperties` has `configSet` (json).
//   - Description: it names `config-set-unavailable`, `config-apply-failed`, `config-restore-failed`, `restartRequired`, the backup path, and `200` with `changed false` and no operation when nothing changes.
//   - Lane: the `LaneKey` doc mentions config set.
//   - Catalog: one more golden row, and `catalogStubs` has a `testprovider.InProcess` entry (which has no Admission hook; only `provider.Detached` requires one).
func TestTCRA67_TheConfigSetOpIsDeclaredAsTheCatalogTableStates(t *testing.T) {
	t.Skip("TC-RA-67: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-RA-68: The deny-list keys exist in the schema, and the table holds the FR-12 entries and every path-valued key. Owner WI 4.1. Requirements: FR-12, ASM-3, NFR-5.
//
//   - Setup: None. FR-12 as amended by the user decision of 2026-11-17 (`requirements.md` § User decisions): every key whose value is a file path, directory, file-name suffix or service name is on the deny-list. `providers.nginx.disabledSuffix` is included by analogy with `includeSuffix` (A-35, the autonomous default, awaiting confirmation). The test's list of reviewed settable strings is exactly `host.name` and `tls.mode` (D-10, A-13).
//   - Steps:
//     1. Run `config.LookupKey(d.Key)` for each `spec.ConfigDenyList` entry.
//     2. Walk `config.LeafKeys()` and take every key of kind `KindString`.
//   - Expected:
//   - Every key is found, and every entry has a non-empty `Reason`.
//   - The table holds exactly these 12 entries, in this order:
//   - the three original FR-12 entries: `providers.nexusd.update.trustedKeys`, `providers.nexusd.enabled` (with `Value: "false"`) and `providers.scripts.catalog` (with `Subtree: true`);
//   - the nine path-, file-name- and service-valued keys: `tls.certFile`, `tls.keyFile`, `providers.docker.socket`, `providers.docker.configDir`, `providers.nginx.includeDir`, `providers.nginx.includeSuffix`, `providers.nginx.disabledSuffix`, `providers.nginx.service` and `providers.pihole.dnsFile`, each with `Value: ""` (any value), `Subtree: false`, and the one shared path reason (each of the nine `Reason` strings is equal).
//   - String-key classification (drift guard, A-13): every `KindString` key from step 2 is either on `spec.ConfigDenyList` or in the reviewed settable list, and the settable remainder is exactly `{host.name, tls.mode}`. A string key added to the schema later fails this test until someone classifies it. The failure message names the unclassified key.
//   - `listen` is not in the table. It is refused as a list (OOS-3, TC-RA-ERR28), by the user decision that `listen` is not settable remotely.
//   - `spec.LockoutKeyPrefixes` is `["tls.", "auth."]`, with no `listen` entry.
func TestTCRA68_TheDenyListKeysExistInTheSchemaAndTheTableHolds(t *testing.T) {
	t.Skip("TC-RA-68: SKIP-only stub; un-skip when WI 4.1 lands")
}

// TC-RA-E7: The expires pattern accepts the local command's forms and bounds its length. Owner WI 2.2. Requirements: FR-1, NFR-5.
//
//   - Setup: The schema of `TokenAddInput.expires`.
//   - Steps:
//     1. Match `90d`, `99999d`, `1h30m`, `1.5h`, `300ms`, `0d`, `100000d`, `-1h`, `1y`, `""`, and a 33-char valid duration.
//   - Expected:
//   - Accepted: `90d`, `99999d`, `1h30m`, `1.5h` and `300ms`. `""` is allowed by `omitempty`.
//   - Rejected: `0d`, `100000d`, `-1h`, `1y` and the 33-char value (`maxLength` 32).
func TestTCRAE7_TheExpiresPatternAcceptsTheLocalCommandSFormsAnd(t *testing.T) {
	t.Skip("TC-RA-E7: SKIP-only stub; un-skip when WI 2.2 lands")
}

// TC-RA-E17: The changes item pattern and count bounds. Owner WI 4.1. Requirements: NFR-5, FR-7.
//
//   - Setup: The `ConfigUpdateInput` schema.
//   - Steps:
//     1. Validate these inputs:
//   - `["providers.host.enabled=true"]`;
//   - `["host.name="]`;
//   - `["=x"]`;
//   - `["Providers.x=1"]`;
//   - `["a.b=c\u0001"]`;
//   - `[]`;
//   - 32 items;
//   - 33 items;
//   - one item whose value has 4096 bytes;
//   - one item longer than `maxLength`.
//   - Expected:
//   - Accepted: `providers.host.enabled=true`, `host.name=` (the empty value is allowed by the pattern), 32 items, and the 4096-byte value.
//   - Rejected: `=x`, `Providers.x=1`, the control character, `[]`, 33 items, and the item over `maxLength`.
func TestTCRAE17_TheChangesItemPatternAndCountBounds(t *testing.T) {
	t.Skip("TC-RA-E17: SKIP-only stub; un-skip when WI 4.1 lands")
}
