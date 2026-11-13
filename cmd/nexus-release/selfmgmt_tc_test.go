// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package main

import "testing"

// TC-SM-14 (WI 1.4; FR-15): nexus-release keygen writes a 0600 key and prints a trustedKeys line that config accepts
func TestTCSM14_NexusReleaseKeygenWritesA0600(t *testing.T) {
	// - Setup: temp `HOME` with no `~/.config/nexus`; `NEXUS_RELEASE_KEY` unset.
	// - Steps:
	//   1. Run `nexus-release keygen` (in-process `run(args, env, stdout, stderr)`, or `go run`).
	//   2. Run `nexus-release pubkey`.
	//   3. Unmarshal the YAML line into `config.NexusdConfig.Update`.
	//   4. Repeat steps 1 and 2 with `NEXUS_RELEASE_KEY=$tmp/k2` and with `--key $tmp/k3`.
	// - Expected:
	//   - `~/.config/nexus` is created at `0700`; the key is at `0600`.
	//   - stdout is exactly `trustedKeys: ["<b64>"]\n`.
	//   - `<b64>` passes `release.ParsePublicKey` and equals the `pubkey` output.
	//   - The decoded YAML validates under 3.1 rules.
	//   - In step 4, the key lands at the given path and the default path is untouched.
	t.Skip("TC-SM-14: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-SM-15 (WI 1.4; FR-15): sign, verify and check-key behave, and no subcommand prints private material
func TestTCSM15_SignVerifyAndCheckKeyBehave(t *testing.T) {
	// - Setup: a keygen'd key K; a file F (1 MiB random); another key K2.
	// - Steps:
	//   1. `sign --key K F`.
	//   2. `verify --pubkey <K pub> F`.
	//   3. `verify --pubkey <K2 pub> F`.
	//   4. `check-key --key K`.
	//   5. `check-key` with K chmod 0644.
	//   6. `check-key` with a missing path.
	//   7. `check-key` with an RSA PKCS#8 key.
	//   8. Collect stdout and stderr of every run.
	// - Expected:
	//   - `F.sig` exists and is a valid `EncodeSignature` output.
	//   - Exit codes: step 2 exits 0; step 3 exits 1; step 4 exits 0; steps 5, 6 and 7 each exit 1
	//     with a reason naming mode, missing or not-ed25519.
	//   - No output contains `PRIVATE KEY`, the base64 of the 32-byte seed, or the base64 of the
	//     64-byte private key.
	t.Skip("TC-SM-15: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-SM-16 (WI 1.4; CON-6, FR-15): A nexus-release signature verifies under OpenSSL 3
func TestTCSM16_ANexusReleaseSignatureVerifiesUnder(t *testing.T) {
	// - Setup: `openssl version` reports 3.x. If not, the test skips and names the committed
	//   transcript under `internal/release/testdata/openssl-signed/`, which must exist.
	// - Steps:
	//   1. keygen to K.
	//   2. `sign --key K F`.
	//   3. Build `msg = "nexus-release-signature-v1\x00" ‖ sha256(F)` into a file.
	//   4. Base64-decode `F.sig`.
	//   5. Write the public key as SPKI PEM.
	//   6. Run `openssl pkeyutl -verify -rawin -pubin -inkey pub.pem -in msg -sigfile sig.bin`.
	// - Expected: OpenSSL prints `Signature Verified Successfully` and exits 0.
	t.Skip("TC-SM-16: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-SM-ERR8 (WI 1.4; FR-15): keygen and make release-key refuse to overwrite an existing key
func TestTCSMERR8_KeygenAndMakeReleaseKeyRefuse(t *testing.T) {
	// - Setup: an existing key at P with content C.
	// - Steps:
	//   1. `nexus-release keygen --key P`.
	//   2. `make release-key NEXUS_RELEASE_KEY=P`, in buildsys style.
	// - Expected:
	//   - Both exit 1 with `exists`.
	//   - P's content is still C and its mode unchanged.
	t.Skip("TC-SM-ERR8: SKIP-only stub; un-skip when WI 1.4 lands")
}
