// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package tlsutil

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func fakeInterfaces(t *testing.T, cidrs ...string) {
	t.Helper()
	var addrs []net.Addr
	for _, c := range cidrs {
		ip, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		addrs = append(addrs, n)
	}
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) { return addrs, nil }
	t.Cleanup(func() { interfaceAddrs = old })
}

// TC-39 (unit half): the generated certificate is the A-15 plain leaf.
func TestGeneratePlainLeaf(t *testing.T) {
	fakeInterfaces(t, "127.0.0.1/8", "::1/128", "192.168.1.20/24", "fd00::20/64", "fe80::1/64")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	certPEM, keyPEM, err := Generate("nas", []string{"nas.lan", "10.0.0.9", "nas"}, now)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("certificate PEM: %q", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.IsCA || !cert.BasicConstraintsValid {
		t.Errorf("IsCA = %v, BasicConstraintsValid = %v; want a leaf with basic constraints cA=false", cert.IsCA, cert.BasicConstraintsValid)
	}
	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %v, want digital signature only", cert.KeyUsage)
	}
	if !slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Errorf("ExtKeyUsage = %v, want server auth only", cert.ExtKeyUsage)
	}
	if want := cert.NotBefore.AddDate(10, 0, 0); !cert.NotAfter.Equal(want) {
		t.Errorf("validity %s to %s, want 10 years", cert.NotBefore, cert.NotAfter)
	}
	if cert.NotBefore.After(now) {
		t.Errorf("NotBefore %s is after generation at %s", cert.NotBefore, now)
	}
	if cert.SerialNumber.Sign() <= 0 || len(cert.SerialNumber.Bytes()) > 20 {
		t.Errorf("serial %v is not a positive integer of at most 20 octets", cert.SerialNumber)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key %T, want ECDSA P-256", cert.PublicKey)
	}
	if want := []string{"nas", "localhost", "nas.lan"}; !slices.Equal(cert.DNSNames, want) {
		t.Errorf("DNS SANs = %q, want %q", cert.DNSNames, want)
	}
	var ips []string
	for _, ip := range cert.IPAddresses {
		ips = append(ips, ip.String())
	}
	if want := []string{"192.168.1.20", "fd00::20", "fe80::1", "10.0.0.9"}; !slices.Equal(ips, want) {
		t.Errorf("IP SANs = %q, want %q (non-loopback interfaces, then extras)", ips, want)
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("not self-signed: %v", err)
	}

	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "PRIVATE KEY" {
		t.Fatalf("key PEM type: %q", keyPEM)
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if ek, ok := k.(*ecdsa.PrivateKey); !ok || !ek.PublicKey.Equal(pub) {
		t.Errorf("key %T does not match the certificate", k)
	}
}

// TC-39 (unit half): the certificate is generated once, stored 0600, and the
// same certificate is returned afterwards.
func TestLoadOrCreateIsStable(t *testing.T) {
	fakeInterfaces(t, "192.168.1.20/24")
	dir := filepath.Join(t.TempDir(), "state", "tls")
	now := time.Now()
	first, created, err := LoadOrCreate(dir, "nas", []string{"nas.lan"}, now)
	if err != nil || !created {
		t.Fatalf("first LoadOrCreate: created=%v err=%v", created, err)
	}
	for _, name := range []string{CertFileName, KeyFileName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %04o, want 0600", name, fi.Mode().Perm())
		}
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("tls directory: %v %v, want mode 0700", fi.Mode(), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("tls directory holds %d entries, want only cert.pem and key.pem", len(entries))
	}

	again, created, err := LoadOrCreate(dir, "other-host", []string{"x.lan"}, now.Add(time.Hour))
	if err != nil || created {
		t.Fatalf("second LoadOrCreate: created=%v err=%v", created, err)
	}
	if !bytes.Equal(first.Certificate[0], again.Certificate[0]) {
		t.Fatal("the certificate changed on the second start")
	}
	fp, err := FileFingerprint(filepath.Join(dir, CertFileName))
	if err != nil {
		t.Fatal(err)
	}
	if fp != Fingerprint(first.Certificate[0]) {
		t.Fatalf("file fingerprint %s differs from the loaded certificate's", fp)
	}
}

// A certificate whose key is gone may already be pinned: it is an error, not
// a silent regeneration. A missing certificate was never announced, so a
// stray key is replaced.
func TestLoadOrCreatePartialState(t *testing.T) {
	fakeInterfaces(t)
	dir := t.TempDir()
	if _, _, err := LoadOrCreate(dir, "nas", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	certBefore, err := os.ReadFile(filepath.Join(dir, CertFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, KeyFileName)); err != nil {
		t.Fatal(err)
	}
	_, _, err = LoadOrCreate(dir, "nas", nil, time.Now())
	if err == nil || !strings.Contains(err.Error(), KeyFileName) {
		t.Fatalf("missing key: %v, want an error naming %s", err, KeyFileName)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, CertFileName)); !bytes.Equal(after, certBefore) {
		t.Fatal("the pinned certificate was replaced")
	}

	if err := os.Remove(filepath.Join(dir, CertFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := LoadOrCreate(dir, "nas", nil, time.Now()); err != nil || !created {
		t.Fatalf("key without certificate: created=%v err=%v, want a new pair", created, err)
	}
}

// The fingerprint is SHA-256 over the certificate DER, as colon-separated
// upper-case hex pairs.
func TestFingerprintFormat(t *testing.T) {
	der := []byte("any DER bytes")
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var pairs []string
	for i := 0; i < len(h); i += 2 {
		pairs = append(pairs, h[i:i+2])
	}
	if got, want := Fingerprint(der), strings.Join(pairs, ":"); got != want {
		t.Fatalf("Fingerprint = %s, want %s", got, want)
	}
}
