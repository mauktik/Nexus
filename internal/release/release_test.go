// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

// Fixed test seeds (test-strategy.md "Signing keys"); no private key is
// committed.
var (
	seedTrusted   = bytes.Repeat([]byte{0x11}, ed25519.SeedSize)
	seedRotated   = bytes.Repeat([]byte{0x22}, ed25519.SeedSize)
	seedUntrusted = bytes.Repeat([]byte{0x33}, ed25519.SeedSize)
)

func pubOf(k ed25519.PrivateKey) ed25519.PublicKey {
	return k.Public().(ed25519.PublicKey)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

type rfcVector struct {
	Name      string `json:"name"`
	SecretKey string `json:"secretKey"`
	PublicKey string `json:"publicKey"`
	Message   string `json:"message"`
	Signature string `json:"signature"`
}

// TC-SM-6 (WI 1.3; FR-15, CON-6): RFC 8032 §7.1 Ed25519 test vectors pass.
// testdata/rfc8032.json was extracted mechanically from
// https://www.rfc-editor.org/rfc/rfc8032.txt §7.1.
func TestTCSM6_RFC803271Ed25519Test(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "rfc8032.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []rfcVector `json:"vectors"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	want := []string{"TEST 1", "TEST 2", "TEST 3", "TEST 1024", "TEST SHA(abc)"}
	if len(file.Vectors) != len(want) {
		t.Fatalf("got %d vectors, want %d", len(file.Vectors), len(want))
	}
	for i, v := range file.Vectors {
		if v.Name != want[i] {
			t.Fatalf("vector %d is %q, want %q", i, v.Name, want[i])
		}
		t.Run(v.Name, func(t *testing.T) {
			sk := mustHex(t, v.SecretKey)
			wantPub := ed25519.PublicKey(mustHex(t, v.PublicKey))
			msg := mustHex(t, v.Message)
			wantSig := mustHex(t, v.Signature)

			k := ed25519.NewKeyFromSeed(sk)
			if !pubOf(k).Equal(wantPub) {
				t.Fatalf("public key %x, want %x", pubOf(k), wantPub)
			}
			if sig := ed25519.Sign(k, msg); !bytes.Equal(sig, wantSig) {
				t.Fatalf("signature %x, want %x", sig, wantSig)
			}
			if !ed25519.Verify(wantPub, msg, wantSig) {
				t.Fatal("vector signature does not verify")
			}
			flipped := bytes.Clone(wantSig)
			flipped[0] ^= 0x01
			if ed25519.Verify(wantPub, msg, flipped) {
				t.Fatal("signature with one bit flipped verifies")
			}

			// The same key through the package API: Sign and Verify are
			// RFC 8032 Ed25519 over Message(digest).
			digest := sha256.Sum256(msg)
			sig := Sign(k, digest)
			if !bytes.Equal(sig, ed25519.Sign(k, Message(digest))) {
				t.Fatal("Sign is not ed25519.Sign over Message(digest)")
			}
			if !Verify([]ed25519.PublicKey{wantPub}, digest, sig) {
				t.Fatal("Verify rejects Sign's output under the vector key")
			}
			sig[len(sig)-1] ^= 0x80
			if Verify([]ed25519.PublicKey{wantPub}, digest, sig) {
				t.Fatal("Verify accepts a signature with one bit flipped")
			}
		})
	}
}

// TC-SM-7 (WI 1.3; FR-15): Message is the domain-separated digest, and
// Sign/Verify round-trips.
func TestTCSM7_MessageIsTheDomainSeparatedDigest(t *testing.T) {
	k := ed25519.NewKeyFromSeed(seedTrusted)
	digest := sha256.Sum256([]byte("tarball bytes"))

	m := Message(digest)
	want := append([]byte("nexus-release-signature-v1\x00"), digest[:]...)
	if !bytes.Equal(m, want) {
		t.Fatalf("Message = %q, want %q", m, want)
	}
	if len(m) != 59 {
		t.Fatalf("len(Message) = %d, want 59", len(m))
	}
	if SignatureContext != "nexus-release-signature-v1\x00" {
		t.Fatalf("SignatureContext = %q", SignatureContext)
	}

	sig := Sign(k, digest)
	if len(sig) != 64 {
		t.Fatalf("len(sig) = %d, want 64", len(sig))
	}
	if !ed25519.Verify(pubOf(k), m, sig) {
		t.Fatal("ed25519.Verify(pub, Message(digest), sig) is false")
	}
	if !Verify([]ed25519.PublicKey{pubOf(k)}, digest, sig) {
		t.Fatal("Verify is false")
	}
}

// TC-SM-8 (WI 1.3; CON-6, FR-15): an OpenSSL 3 signature over Message(digest)
// verifies. The fixture and the commands that made it are in
// testdata/openssl-signed/README.
func TestTCSM8_AnOpenSSL3SignatureOverMessage(t *testing.T) {
	dir := filepath.Join("testdata", "openssl-signed")
	digest, size, err := DigestFile(filepath.Join(dir, "fixture.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 {
		t.Fatal("fixture tarball is empty")
	}
	sigFile, err := os.ReadFile(filepath.Join(dir, "fixture.tar.gz.sig"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := DecodeSignature(sigFile)
	if err != nil {
		t.Fatal(err)
	}
	pubFile, err := os.ReadFile(filepath.Join(dir, "pubkey.b64"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(strings.TrimSuffix(string(pubFile), "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !Verify([]ed25519.PublicKey{pub}, digest, sig) {
		t.Fatal("OpenSSL signature does not verify")
	}
	if ed25519.Verify(pub, digest[:], sig) {
		t.Fatal("OpenSSL signature verifies over the raw digest; the fixture was not made over Message(digest)")
	}
}

// TC-SM-9 (WI 1.3; FR-15): Verify accepts a signature by any listed key.
func TestTCSM9_VerifyAcceptsASignatureByAny(t *testing.T) {
	oldK := ed25519.NewKeyFromSeed(seedTrusted)
	newK := ed25519.NewKeyFromSeed(seedRotated)
	digest := sha256.Sum256([]byte("rotation"))
	sigOld, sigNew := Sign(oldK, digest), Sign(newK, digest)

	both := []ed25519.PublicKey{pubOf(oldK), pubOf(newK)}
	if !Verify(both, digest, sigOld) || !Verify(both, digest, sigNew) {
		t.Fatal("step 1: a signature by a listed key is rejected")
	}
	if Verify([]ed25519.PublicKey{pubOf(newK)}, digest, sigOld) {
		t.Fatal("step 2: sigOld verifies against [new]")
	}
	if !Verify([]ed25519.PublicKey{pubOf(newK), pubOf(oldK)}, digest, sigNew) {
		t.Fatal("step 3: sigNew rejected against [new, old]")
	}
}

// TC-SM-10 (WI 1.3; FR-15): WritePrivateKey writes 0600 with O_EXCL, and
// LoadPrivateKey loads it.
func TestTCSM10_WritePrivateKeyWrites0600WithOEXCL(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	k := ed25519.NewKeyFromSeed(seedTrusted)
	path := filepath.Join(t.TempDir(), "k")
	if err := WritePrivateKey(path, k); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != 0o600 {
		t.Fatalf("mode %v, want -rw-------", fi.Mode())
	}
	loaded, err := LoadPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Equal(k) {
		t.Fatal("loaded key differs from the written key")
	}
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(text)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("file is not a single PEM PRIVATE KEY block:\n%s", text)
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		t.Fatalf("block is not PKCS#8: %v", err)
	}

	// O_EXCL: an existing path is refused and left unchanged.
	other := ed25519.NewKeyFromSeed(seedRotated)
	if err := WritePrivateKey(path, other); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second WritePrivateKey: err = %v, want fs.ErrExist", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, text) {
		t.Fatal("existing key file was modified")
	}

	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(path); err != nil {
		t.Fatalf("0400 key: %v", err)
	}
}

// TC-SM-11 (WI 1.3; FR-15): public-key and signature codecs round-trip.
func TestTCSM11_PublicKeyAndSignatureCodecsRound(t *testing.T) {
	k := ed25519.NewKeyFromSeed(seedTrusted)
	pub := pubOf(k)

	enc := EncodePublicKey(pub)
	if len(enc) != 44 {
		t.Fatalf("encoded key %q is %d characters, want 44", enc, len(enc))
	}
	if raw, err := base64.StdEncoding.DecodeString(enc); err != nil || !bytes.Equal(raw, pub) {
		t.Fatalf("encoded key is not std base64 of the raw key: %v", err)
	}
	got, err := ParsePublicKey(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(pub) {
		t.Fatal("public key does not round-trip")
	}

	sig := Sign(k, sha256.Sum256([]byte("codec")))
	encSig := EncodeSignature(sig)
	if !regexp.MustCompile(`^[A-Za-z0-9+/]{86}==\n$`).Match(encSig) {
		t.Fatalf("encoded signature %q does not match the .sig format", encSig)
	}
	for _, in := range [][]byte{encSig, bytes.TrimSuffix(encSig, []byte("\n"))} {
		dec, err := DecodeSignature(in)
		if err != nil {
			t.Fatalf("DecodeSignature(%q): %v", in, err)
		}
		if !bytes.Equal(dec, sig) {
			t.Fatalf("DecodeSignature(%q) does not round-trip", in)
		}
	}
}

// TC-SM-13 (WI 1.3; FR-7): DigestFile streams and matches sha256.
func TestTCSM13_DigestFileStreamsAndMatchesSha256(t *testing.T) {
	const size = 5 << 20
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, n, err := DigestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest != sha256.Sum256(onDisk) {
		t.Fatal("digest differs from sha256.Sum256 of the file")
	}
	if n != size {
		t.Fatalf("size %d, want %d", n, size)
	}

	if _, _, err := DigestFile(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing path: err = %v, want fs.ErrNotExist", err)
	}
}

// TC-SM-ERR4 (WI 1.3; FR-15, FR-7, NFR-5): Verify rejects every malformed or
// replayed signature.
func TestTCSMERR4_VerifyRejectsEveryMalformedOrReplayed(t *testing.T) {
	k := ed25519.NewKeyFromSeed(seedTrusted)
	k2 := ed25519.NewKeyFromSeed(seedUntrusted)
	tarball := []byte("tarball T")
	d := sha256.Sum256(tarball)
	d2 := sha256.Sum256([]byte("tarball T2"))
	valid := Sign(k, d)
	trusted := []ed25519.PublicKey{pubOf(k)}

	flippedD := d
	flippedD[7] ^= 0x10

	tests := []struct {
		name   string
		keys   []ed25519.PublicKey
		digest [32]byte
		sig    []byte
		want   bool
	}{
		{"1 empty key list", []ed25519.PublicKey{}, d, valid, false},
		{"2 nil key", []ed25519.PublicKey{nil}, d, valid, false},
		{"3 signature by an untrusted key", trusted, d, Sign(k2, d), false},
		{"4 digest with one bit flipped", trusted, flippedD, valid, false},
		{"5 signature over another tarball", trusted, d, Sign(k, d2), false},
		{"6 raw digest without the context", trusted, d, ed25519.Sign(k, d[:]), false},
		{"7 other context version", trusted, d, ed25519.Sign(k, append([]byte("nexus-release-signature-v2\x00"), d[:]...)), false},
		{"8 raw tarball bytes", trusted, d, ed25519.Sign(k, tarball), false},
		{"9 63-byte signature", trusted, d, valid[:63], false},
		{"10 65-byte signature", trusted, d, append(bytes.Clone(valid), 0), false},
		{"11 64 zero bytes", trusted, d, make([]byte, 64), false},
		{"12 trusted key listed second", []ed25519.PublicKey{pubOf(k2), pubOf(k)}, d, valid, true},
		{"nil key list", nil, d, valid, false},
		{"short key", []ed25519.PublicKey{pubOf(k)[:31]}, d, valid, false},
		{"nil signature", trusted, d, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Verify panicked: %v", r)
				}
			}()
			if got := Verify(tt.keys, tt.digest, tt.sig); got != tt.want {
				t.Fatalf("Verify = %v, want %v", got, tt.want)
			}
		})
	}
}

func writePEM(t *testing.T, path, typ string, der []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TC-SM-ERR5 (WI 1.3; FR-15): LoadPrivateKey refuses group or other
// permission bits and non-ed25519 keys.
func TestTCSMERR5_LoadPrivateKeyRefusesGroupOrOtherPermission(t *testing.T) {
	dir := t.TempDir()
	edDER, err := x509.MarshalPKCS8PrivateKey(ed25519.NewKeyFromSeed(seedTrusted))
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}

	// Every secret byte sequence an error must never echo.
	secrets := []string{"-----BEGIN", "-----END"}
	for _, der := range [][]byte{edDER, rsaDER, ecDER} {
		b64 := base64.StdEncoding.EncodeToString(der)
		for i := 0; i+16 <= len(b64); i += 16 {
			secrets = append(secrets, b64[i:i+16])
		}
	}

	type row struct {
		name string
		path string
		want func(error) bool
		desc string
	}
	contains := func(s string) func(error) bool {
		return func(err error) bool { return strings.Contains(err.Error(), s) }
	}
	var rows []row
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o700} {
		p := filepath.Join(dir, fmt.Sprintf("ed-%04o", mode))
		writePEM(t, p, "PRIVATE KEY", edDER, mode)
		m := fmt.Sprintf("%04o", mode)
		rows = append(rows, row{"mode " + m, p, contains(m), "names mode " + m})
	}
	rsaPath := filepath.Join(dir, "rsa")
	writePEM(t, rsaPath, "PRIVATE KEY", rsaDER, 0o600)
	rows = append(rows, row{"rsa", rsaPath, contains("not an ed25519 key"), "not an ed25519 key"})
	ecPath := filepath.Join(dir, "ecdsa")
	writePEM(t, ecPath, "PRIVATE KEY", ecDER, 0o600)
	rows = append(rows, row{"ecdsa", ecPath, contains("not an ed25519 key"), "not an ed25519 key"})
	garbage := filepath.Join(dir, "garbage")
	if err := os.WriteFile(garbage, []byte("this is not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, row{"garbage", garbage, contains("no PEM PRIVATE KEY block"), "a parse failure"})
	badDER := filepath.Join(dir, "bad-der")
	writePEM(t, badDER, "PRIVATE KEY", edDER[:len(edDER)-3], 0o600)
	rows = append(rows, row{"truncated PKCS#8", badDER, contains("parse PKCS#8"), "a parse failure"})
	rows = append(rows, row{"missing", filepath.Join(dir, "missing"), func(err error) bool { return errors.Is(err, fs.ErrNotExist) }, "fs.ErrNotExist"})

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			k, err := LoadPrivateKey(r.path)
			if err == nil {
				t.Fatalf("LoadPrivateKey succeeded (key %d bytes)", len(k))
			}
			if !r.want(err) {
				t.Fatalf("err = %q, want %s", err, r.desc)
			}
			for _, s := range secrets {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("err %q contains key material %q", err, s)
				}
			}
		})
	}
}

// TC-SM-ERR6 (WI 1.3; FR-15): codecs reject malformed public keys and
// signatures.
func TestTCSMERR6_CodecsRejectMalformedPublicKeysAnd(t *testing.T) {
	pub := EncodePublicKey(pubOf(ed25519.NewKeyFromSeed(seedTrusted)))
	// A key whose std encoding contains '+' and '/', so the URL-safe form
	// differs.
	var urlRaw []byte
	for i := 0; ; i++ {
		seed := bytes.Repeat([]byte{byte(i)}, ed25519.SeedSize)
		raw := pubOf(ed25519.NewKeyFromSeed(seed))
		if s := base64.StdEncoding.EncodeToString(raw); strings.ContainsAny(s, "+/") {
			urlRaw = raw
			break
		}
	}
	urlSafe := base64.URLEncoding.EncodeToString(urlRaw)
	if !strings.ContainsAny(urlSafe, "-_") {
		t.Fatalf("test setup: %q has no URL-safe characters", urlSafe)
	}

	keys := map[string]string{
		"31 bytes":         base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"33 bytes":         base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"url-safe":         urlSafe,
		"unpadded":         strings.TrimRight(pub, "="),
		"leading space":    " " + pub,
		"trailing space":   pub + " ",
		"trailing newline": pub + "\n",
		"empty":            "",
	}
	for name, in := range keys {
		if _, err := ParsePublicKey(in); err == nil {
			t.Errorf("ParsePublicKey(%s %q) succeeded", name, in)
		}
	}

	good := EncodeSignature(make([]byte, 64))
	body := strings.TrimSuffix(string(good), "\n")
	sigs := map[string]string{
		"87 characters":      body[:87],
		"89 characters":      body + "A",
		"63 bytes":           base64.StdEncoding.EncodeToString(make([]byte, 63)),
		"63 bytes + newline": base64.StdEncoding.EncodeToString(make([]byte, 63)) + "\n",
		"two newlines":       body + "\n\n",
		"CRLF":               body + "\r\n",
		"embedded newline":   body[:44] + "\n" + body[44:],
		"url-safe alphabet":  strings.Replace(body, "A", "-", 1),
		"empty":              "",
	}
	for name, in := range sigs {
		if _, err := DecodeSignature([]byte(in)); err == nil {
			t.Errorf("DecodeSignature(%s %q) succeeded", name, in)
		}
	}
}
