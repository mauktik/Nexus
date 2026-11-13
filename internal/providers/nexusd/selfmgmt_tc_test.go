// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package nexusd

import "testing"

// TC-SM-97 (WI 6.1; FR-13, FR-2, FR-3, FR-4): CLI end to end: get, validate-config and reload through cli.Run against the nexusd stack
func TestTCSM97_CLIEndToEndGetValidate(t *testing.T) {
	// - Setup: nexusd T0 stack with a client config in a temp HOME (`writeClientConfig` pattern);
	//   `admin` token.
	// - Steps:
	//   1. `nexus nexusd get -o json`.
	//   2. `nexus nexusd get` (table).
	//   3. Write Prev and repeat step 1.
	//   4. `nexus nexusd validate-config` on a valid config, then an invalid one.
	//   5. `nexus nexusd reload` on a valid config, then an invalid one.
	// - Expected:
	//   - Step 1: exit 0, with the TC-SM-51 fields.
	//   - Step 2: table columns `name, version, uptimeSeconds, previous.version, updateConfigured`.
	//   - Step 3: `previous.version` is updated.
	//   - Step 4: exit 0 for both. The invalid one prints `valid: false` and each error with its
	//     location.
	//   - Step 5:
	//     - the valid reload exits 0 with `restartRequired`;
	//     - the invalid one exits 1, with stderr naming `config-invalid` and the first error's
	//       `file:line:column`;
	//     - no op is created by the invalid reload.
	t.Skip("TC-SM-97: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-98 (WI 6.1; FR-7, FR-13, FR-16): CLI end to end: update refusals map to problem types and exit codes
func TestTCSM98_CLIEndToEndUpdateRefusals(t *testing.T) {
	// - Setup: nexusd T0 stack; fixtures in a temp dir:
	//   - `good-v9.9.2.tar.gz` signed by K;
	//   - `untrusted.tar.gz` signed by K2;
	//   - `traversal.tar.gz` signed by K;
	//   - `old-v9.9.0.tar.gz` signed by K, while running `v9.9.1`.
	// - Steps:
	//   1. With `trustedKeys: []`, run `nexus nexusd update --file good…`.
	//   2. With K trusted, update `untrusted`.
	//   3. Update `traversal`.
	//   4. Update `old`.
	//   5. Update `old --allow-downgrade --no-wait`.
	// - Expected:
	//   - Step 1: exit 1, `update-not-configured`.
	//   - Step 2: exit 1, `update-signature-invalid`.
	//   - Step 3: exit 1, `update-package-invalid` with `check: path`.
	//   - Step 4: exit 1, `version-downgrade` naming `v9.9.1` and `v9.9.0`.
	//   - Step 5: exit 0, printing the queued op.
	//   - Steps 1–4 created no op (`nexus operation get` shows only the step-5 op).
	t.Skip("TC-SM-98: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-99 (WI 6.1; FR-13, FR-1, FR-6): nexus command get lists six nexusd commands, and capabilities and OpenAPI list the provider
func TestTCSM99_NexusCommandGetListsSixNexusd(t *testing.T) {
	// - Setup: nexusd T0 stack, plus the composed `daemon.New` with `providers.nexusd.enabled: true`.
	// - Steps:
	//   1. `nexus command get -o json`.
	//   2. `GET /v1/capabilities`.
	//   3. `GET /v1/openapi.json`.
	// - Expected:
	//   - nexusd commands are exactly `get`, `validate-config`, `reload`, `restart`, `update` and
	//     `rollback`.
	//   - nexusd appears in capabilities.
	//   - The OpenAPI document has the 7 nexusd routes, including `POST /v1/providers/nexusd/uploads`
	//     with a binary body and the problems `upload-too-large`, `conflict` and
	//     `storage-limit-reached`.
	t.Skip("TC-SM-99: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-E11 (WI 6.1; FR-11, FR-6): A CLI update retried with the same idempotency key replays the same op
func TestTCSME11_ACLIUpdateRetriedWithThe(t *testing.T) {
	// - Setup: nexusd T0 stack with a spy launch (the op stays Queued or Running); `good-v9.9.2`
	//   signed by K.
	// - Steps:
	//   1. `nexus nexusd update --file good --idempotency-key k1 --no-wait`, run twice.
	// - Expected:
	//   - Both runs print the same op ID.
	//   - There is one staging dir and one op.
	//   - Each run has a `stored` record for its upload, but only one `requested` record for the
	//     update op.
	t.Skip("TC-SM-E11: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-E15 (WI 6.1; FR-12, FR-13): CLI rollback is destructive and requires confirmation
func TestTCSME15_CLIRollbackIsDestructiveAndRequires(t *testing.T) {
	// - Setup: nexusd T0 stack with Prev present; no TTY (`OpenTerminal` errors); stdin empty.
	// - Steps:
	//   1. `nexus nexusd rollback`.
	//   2. `nexus nexusd rollback --yes --no-wait`.
	// - Expected:
	//   - Step 1: exit 2 with the existing "confirmation required" message, and zero mutating
	//     requests.
	//   - Step 2: a POST is sent and the op is created.
	t.Skip("TC-SM-E15: SKIP-only stub; un-skip when WI 6.1 lands")
}
