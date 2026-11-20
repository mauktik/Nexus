// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/release"
)

// result is one in-process run of the tool.
type result struct {
	code           int
	stdout, stderr string
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
}

// invoke runs the tool in-process with env as its whole environment.
func invoke(env map[string]string, args ...string) result {
	var out, errOut bytes.Buffer
	code := run(args, func(k string) string { return env[k] }, &out, &errOut)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

var trustedKeysLine = regexp.MustCompile(`^trustedKeys: \["([A-Za-z0-9+/=]+)"\]\n$`)

// newKey makes a key at path through the tool and returns its public key.
func newKey(t *testing.T, path string) string {
	t.Helper()
	r := invoke(map[string]string{"HOME": t.TempDir()}, "keygen", "--key", path)
	m := trustedKeysLine.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil {
		t.Fatalf("keygen --key %s: %v", path, r)
	}
	return m[1]
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TC-SM-14 (WI 1.4; FR-15): nexus-release keygen writes a 0600 key and
// prints a trustedKeys line that config accepts.
func TestTCSM14_NexusReleaseKeygenWritesA0600(t *testing.T) {
	home := t.TempDir()
	defaultKey := filepath.Join(home, ".config", "nexus", "release-signing.key")

	check := func(t *testing.T, env map[string]string, keyPath string, args ...string) {
		t.Helper()
		r := invoke(env, append([]string{"keygen"}, args...)...)
		if r.code != 0 {
			t.Fatalf("keygen: %v", r)
		}
		m := trustedKeysLine.FindStringSubmatch(r.stdout)
		if m == nil {
			t.Fatalf("keygen stdout = %q, want exactly trustedKeys: [\"<b64>\"]\\n", r.stdout)
		}
		pub, err := release.ParsePublicKey(m[1])
		if err != nil {
			t.Fatalf("keygen printed a key ParsePublicKey refuses: %v", err)
		}
		if got := mode(t, keyPath); got != 0o600 {
			t.Errorf("%s mode %04o, want 0600", keyPath, got)
		}
		if got := mode(t, filepath.Dir(keyPath)); got != 0o700 {
			t.Errorf("%s mode %04o, want 0700", filepath.Dir(keyPath), got)
		}
		k, err := release.LoadPrivateKey(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if !k.Public().(ed25519.PublicKey).Equal(pub) {
			t.Error("the printed public key is not the written key's")
		}

		p := invoke(env, append([]string{"pubkey"}, args...)...)
		if p.code != 0 || p.stdout != m[1]+"\n" {
			t.Fatalf("pubkey = %v, want %q", p, m[1]+"\n")
		}

		// The line is the YAML a host's providers.nexusd.update section
		// takes: it decodes into config.NexusdUpdateConfig, and a config
		// file holding it there passes the providers.nexusd validation.
		var update config.NexusdUpdateConfig
		dec := yaml.NewDecoder(strings.NewReader(r.stdout))
		dec.KnownFields(true)
		if err := dec.Decode(&update); err != nil {
			t.Fatalf("decode %q: %v", r.stdout, err)
		}
		if len(update.TrustedKeys) != 1 || update.TrustedKeys[0] != m[1] {
			t.Fatalf("decoded trustedKeys = %q", update.TrustedKeys)
		}
		cfg, err := config.Parse([]byte("providers:\n  nexusd:\n    update:\n      "+r.stdout), "nexusd.yaml")
		if err != nil {
			t.Fatalf("a config with the printed line is invalid: %v", err)
		}
		if got := cfg.Providers.Nexusd.Settings.Update.TrustedKeys; len(got) != 1 || got[0] != m[1] {
			t.Fatalf("configured trustedKeys = %q", got)
		}
	}

	t.Run("default path", func(t *testing.T) {
		check(t, map[string]string{"HOME": home}, defaultKey)
	})

	t.Run("NEXUS_RELEASE_KEY and --key", func(t *testing.T) {
		home := t.TempDir()
		def := filepath.Join(home, ".config", "nexus", "release-signing.key")
		tmp := t.TempDir()
		k2 := filepath.Join(tmp, "dir2", "k2")
		check(t, map[string]string{"HOME": home, "NEXUS_RELEASE_KEY": k2}, k2)
		k3 := filepath.Join(tmp, "dir3", "k3")
		check(t, map[string]string{"HOME": home, "NEXUS_RELEASE_KEY": k2}, k3, "--key", k3)
		if _, err := os.Lstat(filepath.Dir(def)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the default key directory was touched: %v", err)
		}
	})
}

// TC-SM-15 (WI 1.4; FR-15): sign, verify and check-key behave, and no
// subcommand prints private material.
func TestTCSM15_SignVerifyAndCheckKeyBehave(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOME": t.TempDir()}
	k := filepath.Join(dir, "k.key")
	k2 := filepath.Join(dir, "k2.key")
	pub := newKey(t, k)
	pub2 := newKey(t, k2)
	f := filepath.Join(dir, "f.bin")
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, data, 0o644); err != nil {
		t.Fatal(err)
	}

	var outputs []result
	step := func(wantCode int, args ...string) result {
		t.Helper()
		r := invoke(env, args...)
		outputs = append(outputs, r)
		if r.code != wantCode {
			t.Errorf("%q: exit %d, want %d: %v", args, r.code, wantCode, r)
		}
		return r
	}

	step(0, "sign", "--key", k, f)
	sigFile, err := os.ReadFile(f + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := release.DecodeSignature(sigFile)
	if err != nil || !bytes.Equal(sigFile, release.EncodeSignature(sig)) {
		t.Fatalf("%s is not EncodeSignature output: %q %v", f+".sig", sigFile, err)
	}
	pk, err := release.ParsePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if !release.Verify([]ed25519.PublicKey{pk}, sha256.Sum256(data), sig) {
		t.Fatal("the signature does not verify against the key's public half")
	}

	step(0, "verify", "--pubkey", pub, f)
	if r := step(1, "verify", "--pubkey", pub2, f); !strings.Contains(r.stderr, "does not verify") {
		t.Errorf("verify with the wrong key: %v", r)
	}
	step(0, "check-key", "--key", k)

	open := filepath.Join(dir, "open.key")
	newKey(t, open)
	if err := os.Chmod(open, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := step(1, "check-key", "--key", open); !strings.Contains(r.stderr, "mode 0644") {
		t.Errorf("check-key on a 0644 key does not name the mode: %v", r)
	}
	if r := step(1, "check-key", "--key", filepath.Join(dir, "missing.key")); !strings.Contains(r.stderr, "missing") {
		t.Errorf("check-key on a missing key does not say missing: %v", r)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	rsaPath := filepath.Join(dir, "rsa.key")
	if err := os.WriteFile(rsaPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := step(1, "check-key", "--key", rsaPath); !strings.Contains(r.stderr, "not an ed25519 key") {
		t.Errorf("check-key on an RSA key does not say not ed25519: %v", r)
	}
	step(0, "pubkey", "--key", k)

	priv, err := release.LoadPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{
		"PRIVATE KEY",
		base64.StdEncoding.EncodeToString(priv.Seed()),
		base64.StdEncoding.EncodeToString(priv),
		base64.RawStdEncoding.EncodeToString(priv.Seed()),
	}
	for _, r := range outputs {
		for _, s := range secrets {
			if strings.Contains(r.stdout, s) || strings.Contains(r.stderr, s) {
				t.Errorf("output carries private material %q: %v", s, r)
			}
		}
	}
}

// openssl3 returns an OpenSSL 3 binary, or "" when none is installed. macOS
// ships LibreSSL as /usr/bin/openssl, which is not 3.x.
func openssl3() string {
	candidates := []string{os.Getenv("OPENSSL")}
	if p, err := exec.LookPath("openssl"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "/opt/homebrew/opt/openssl@3/bin/openssl", "/usr/local/opt/openssl@3/bin/openssl")
	for _, c := range candidates {
		if c == "" {
			continue
		}
		out, err := exec.Command(c, "version").Output()
		if err == nil && strings.HasPrefix(string(out), "OpenSSL 3.") {
			return c
		}
	}
	return ""
}

// transcript is the committed OpenSSL 3 check of a nexus-release signature,
// for hosts without OpenSSL 3.
const transcript = "../../internal/release/testdata/openssl-signed/nexus-release-sign.transcript"

// TC-SM-16 (WI 1.4; CON-6, FR-15): a nexus-release signature verifies under
// OpenSSL 3.
func TestTCSM16_ANexusReleaseSignatureVerifiesUnder(t *testing.T) {
	ossl := openssl3()
	if ossl == "" {
		if _, err := os.Stat(transcript); err != nil {
			t.Fatalf("no OpenSSL 3, and the committed transcript is missing: %v", err)
		}
		t.Skipf("no OpenSSL 3 found (set OPENSSL); the committed check is %s", transcript)
	}
	dir := t.TempDir()
	k := filepath.Join(dir, "k.key")
	pub := newKey(t, k)
	f := filepath.Join(dir, "f.tar.gz")
	data := make([]byte, 4096)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := invoke(map[string]string{"HOME": t.TempDir()}, "sign", "--key", k, f); r.code != 0 {
		t.Fatalf("sign: %v", r)
	}

	digest := sha256.Sum256(data)
	msg := append([]byte("nexus-release-signature-v1\x00"), digest[:]...)
	sigB64, err := os.ReadFile(f + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(string(sigB64), "\n"))
	if err != nil {
		t.Fatal(err)
	}
	pk, err := release.ParsePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"msg":     msg,
		"sig.bin": sig,
		"pub.pem": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(ossl, "pkeyutl", "-verify", "-rawin", "-pubin", "-inkey", "pub.pem", "-in", "msg", "-sigfile", "sig.bin")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Signature Verified Successfully") {
		t.Fatalf("%s pkeyutl -verify: %v\n%s", ossl, err, out)
	}
	t.Logf("%s: %s", ossl, strings.TrimSpace(string(out)))
}

// TC-SM-ERR8 (WI 1.4; FR-15): keygen and make release-key refuse to
// overwrite an existing key.
func TestTCSMERR8_KeygenAndMakeReleaseKeyRefuse(t *testing.T) {
	p := filepath.Join(t.TempDir(), "existing.key")
	newKey(t, p)
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(t *testing.T) {
		t.Helper()
		after, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("%s changed: %v", p, err)
		}
		if m := mode(t, p); m != 0o600 {
			t.Fatalf("%s mode %04o, want 0600", p, m)
		}
	}

	t.Run("keygen", func(t *testing.T) {
		r := invoke(map[string]string{"HOME": t.TempDir()}, "keygen", "--key", p)
		if r.code != 1 || !strings.Contains(r.stderr, "exists") || r.stdout != "" {
			t.Fatalf("keygen over an existing key: %v", r)
		}
		unchanged(t)
	})

	t.Run("make release-key", func(t *testing.T) {
		if os.Getenv("NEXUS_IN_MAKE") != "" {
			t.Skip("NEXUS_IN_MAKE is set: this case runs make and must not recurse from make; run `go test ./cmd/nexus-release/` directly")
		}
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "NEXUS_IN_MAKE=") && !strings.HasPrefix(kv, "NEXUS_RELEASE_KEY=") && !strings.HasPrefix(kv, "RELEASE_KEY=") {
				env = append(env, kv)
			}
		}
		// release-key writes only the key path, so it runs against the
		// checkout itself.
		cmd := exec.Command("make", "--no-print-directory", "-C", "../..", "release-key", "NEXUS_RELEASE_KEY="+p)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		// make reports a failed recipe with its own status 2; the tool's 1
		// is in the recipe's error line.
		if !errors.As(err, &exitErr) || !strings.Contains(string(out), "exists") || !strings.Contains(string(out), "Error 1") {
			t.Fatalf("make release-key over an existing key: %v\n%s", err, out)
		}
		unchanged(t)
	})
}
