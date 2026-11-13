// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package release

import "testing"

// TC-SM-6 (WI 1.3; FR-15, CON-6): RFC 8032 §7.1 Ed25519 test vectors pass
func TestTCSM6_RFC803271Ed25519Test(t *testing.T) {
	// - Setup: `testdata/rfc8032.json` with the five §7.1 vectors (TEST 1, 2, 3, 1024 and SHA(abc)):
	//   secret key, public key, message and signature.
	// - Steps:
	//   1. For each vector, derive the key with `ed25519.NewKeyFromSeed(sk)`.
	//   2. Sign the raw message.
	//   3. Verify the vector's signature over the raw message with the vector's public key.
	// - Expected:
	//   - The derived public key equals the vector's.
	//   - The signature equals the vector's byte for byte.
	//   - Verification succeeds.
	//   - Flipping one signature bit fails verification.
	t.Skip("TC-SM-6: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-7 (WI 1.3; FR-15): Message is the domain-separated digest, and Sign/Verify round-trips
func TestTCSM7_MessageIsTheDomainSeparatedDigest(t *testing.T) {
	// - Setup: key from `seedTrusted`; `digest = sha256.Sum256([]byte("tarball bytes"))`.
	// - Steps:
	//   1. Compute `m := Message(digest)`.
	//   2. `sig := Sign(k, digest)`.
	//   3. Call `Verify([]ed25519.PublicKey{k.Public()}, digest, sig)`.
	// - Expected:
	//   - `m` equals `[]byte("nexus-release-signature-v1\x00")` followed by the 32 digest bytes, so
	//     `len(m) == 59`.
	//   - `len(sig) == 64`.
	//   - `ed25519.Verify(pub, m, sig)` is true.
	//   - `Verify` is true.
	t.Skip("TC-SM-7: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-8 (WI 1.3; CON-6, FR-15): An OpenSSL 3 signature over Message(digest) verifies
func TestTCSM8_AnOpenSSL3SignatureOverMessage(t *testing.T) {
	// - Setup: the committed `testdata/openssl-signed/`:
	//   - `fixture.tar.gz`;
	//   - `fixture.tar.gz.sig`, from `openssl pkeyutl -sign -rawin` over `Message(sha256(tarball))`
	//     and then base64;
	//   - `pubkey.b64`;
	//   - a README with the exact commands.
	// - Steps:
	//   1. `DigestFile(fixture)`.
	//   2. `DecodeSignature(.sig)`.
	//   3. `ParsePublicKey(pubkey.b64)`.
	//   4. Call `Verify`.
	// - Expected:
	//   - `Verify` is true.
	//   - Verifying over the raw digest (without the context) is false, which proves that the fixture
	//     was made over the domain-separated message.
	t.Skip("TC-SM-8: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-9 (WI 1.3; FR-15): Verify accepts a signature by any listed key (rotation)
func TestTCSM9_VerifyAcceptsASignatureByAny(t *testing.T) {
	// - Setup: keys `old` (`seedTrusted`) and `new` (`seedRotated`); one digest.
	// - Steps:
	//   1. Verify `sigOld` and `sigNew` against `[old, new]`.
	//   2. Verify `sigOld` against `[new]`.
	//   3. Verify `sigNew` against `[new, old]`.
	// - Expected: steps 1 and 3 are true (both signatures, either order); step 2 is false.
	t.Skip("TC-SM-9: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-10 (WI 1.3; FR-15): WritePrivateKey writes 0600 with O_EXCL, and LoadPrivateKey loads it
func TestTCSM10_WritePrivateKeyWrites0600WithOEXCL(t *testing.T) {
	// - Setup: temp dir with `umask` 022 in effect.
	// - Steps:
	//   1. `WritePrivateKey(dir/k, key)`.
	//   2. Stat the file.
	//   3. `LoadPrivateKey(dir/k)`.
	//   4. Read the file as text.
	// - Expected:
	//   - Mode is exactly `0600`.
	//   - The PEM block type is `PRIVATE KEY` (PKCS#8).
	//   - The loaded key equals the original (`Equal`).
	//   - A file at mode `0400` also loads.
	t.Skip("TC-SM-10: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-11 (WI 1.3; FR-15): Public-key and signature codecs round-trip
func TestTCSM11_PublicKeyAndSignatureCodecsRound(t *testing.T) {
	// - Setup: key from `seedTrusted`; a signature.
	// - Steps:
	//   1. `EncodePublicKey` then `ParsePublicKey`.
	//   2. `EncodeSignature` then `DecodeSignature`, with and without the trailing newline.
	// - Expected:
	//   - The encoded key is 44 characters of standard base64 of 32 raw bytes and round-trips.
	//   - The encoded signature is 88 characters, then `\n`, matching `^[A-Za-z0-9+/]{86}==\n$`.
	//   - Decoding with or without the newline gives the same 64 bytes.
	t.Skip("TC-SM-11: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-12 (WI 1.3; FR-7, ASM-6): CompareVersions follows SemVer 2.0 precedence
func TestTCSM12_CompareVersionsFollowsSemVer20Precedence(t *testing.T) {
	// - Setup: table.
	// - Steps:
	//   1. Compare these pairs:
	//      - `v1.2.3`/`v1.2.4`;
	//      - `v1.10.0`/`v1.9.0`;
	//      - `v2.0.0`/`v1.99.99`;
	//      - `v1.0.0-rc.1`/`v1.0.0`;
	//      - the SemVer §11 chain `v1.0.0-alpha < -alpha.1 < -alpha.beta < -beta < -beta.2 < -beta.11
	//        < -rc.1 < v1.0.0`;
	//      - `v9.9.2`/`v9.9.2`.
	//   2. Compare `dev`, `1.2.3`, `v1.2`, `v1.2.3.4`, `v01.2.3` and `""` against `v1.2.3`.
	// - Expected:
	//   - Step 1:
	//     - `-1` for the first argument older and `+1` for newer;
	//     - each adjacent pair in the chain compares `-1`, and the reverse compares `+1`;
	//     - `0` for equal versions;
	//     - `ok=true` everywhere.
	//   - Step 2: `ok=false` for each.
	//   - Every accepted string also matches the Makefile `release-version` regex
	//     `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`.
	t.Skip("TC-SM-12: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-13 (WI 1.3; FR-7): DigestFile streams and matches sha256
func TestTCSM13_DigestFileStreamsAndMatchesSha256(t *testing.T) {
	// - Setup: a 5 MiB file of `crypto/rand` bytes.
	// - Steps:
	//   1. `DigestFile(path)`.
	// - Expected:
	//   - The digest equals `sha256.Sum256(os.ReadFile(path))`.
	//   - The size is 5 MiB.
	//   - A missing path returns an error wrapping `fs.ErrNotExist`.
	t.Skip("TC-SM-13: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-ERR4 (WI 1.3; FR-15, FR-7, NFR-5): Verify rejects every malformed or replayed signature
func TestTCSMERR4_VerifyRejectsEveryMalformedOrReplayed(t *testing.T) {
	// - Setup: K (trusted), K2 (untrusted); `d = sha256(T)`, `d2 = sha256(T2)`.
	// - Steps:
	//   1. Run `Verify` on each row:
	//      - (1) keys `[]`, a valid signature;
	//      - (2) keys `[nil]`;
	//      - (3) a signature by K2;
	//      - (4) `d` with one bit flipped;
	//      - (5) a signature over `d2`, verified for `d`;
	//      - (6) a K signature over the raw `d` without the context;
	//      - (7) a K signature over `"nexus-release-signature-v2\x00" ‖ d`;
	//      - (8) a K signature over the raw tarball bytes T;
	//      - (9) a 63-byte signature;
	//      - (10) a 65-byte signature;
	//      - (11) 64 zero bytes;
	//      - (12) keys `[K2, K]`, a valid K signature.
	// - Expected:
	//   - Rows 1–11 are false, with no panic.
	//   - Row 12 is true.
	t.Skip("TC-SM-ERR4: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-ERR5 (WI 1.3; FR-15): LoadPrivateKey refuses group or other permission bits and non-ed25519 keys
func TestTCSMERR5_LoadPrivateKeyRefusesGroupOrOtherPermission(t *testing.T) {
	// - Setup: key files at modes 0640, 0604, 0644, 0660 and 0700; an RSA PKCS#8 PEM; an ECDSA PKCS#8
	//   PEM; garbage; a missing path.
	// - Steps:
	//   1. Call `LoadPrivateKey` on each.
	// - Expected:
	//   - Each returns an error naming the mode (`0640`…), `not an ed25519 key`, a parse failure, or
	//     not-exist.
	//   - No error string contains PEM content or base64 key bytes.
	t.Skip("TC-SM-ERR5: SKIP-only stub; un-skip when WI 1.3 lands")
}

// TC-SM-ERR6 (WI 1.3; FR-15): Codecs reject malformed public keys and signatures
func TestTCSMERR6_CodecsRejectMalformedPublicKeysAnd(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. `ParsePublicKey` on:
	//      - base64 of 31 bytes and of 33 bytes;
	//      - URL-safe base64 (`-`, `_`);
	//      - unpadded base64;
	//      - a string with surrounding spaces;
	//      - `""`.
	//   2. `DecodeSignature` on 87 characters, on 89 characters, and on base64 of 63 bytes.
	// - Expected: each returns an error.
	t.Skip("TC-SM-ERR6: SKIP-only stub; un-skip when WI 1.3 lands")
}
