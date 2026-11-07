// Package tlsutil creates and reads the certificate nexusd serves.
//
// In tls.mode auto, nexusd generates one self-signed certificate on first
// start and keeps it: clients pin its SHA-256 fingerprint, so it is never
// regenerated while it is usable. The certificate is a plain leaf (A-15):
// IsCA is false, so its key cannot vouch for any other name.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"
)

// File names inside the TLS directory.
const (
	CertFileName = "cert.pem"
	KeyFileName  = "key.pem"
)

// ValidityYears is the lifetime of a generated certificate (A-16). Pinned
// clients ignore expiry; a client that trusts the certificate directly does
// not.
const ValidityYears = 10

// backdate covers client clocks slightly behind the server's.
const backdate = time.Hour

// interfaceAddrs is replaced in tests.
var interfaceAddrs = net.InterfaceAddrs

// Dir is the TLS directory under the state directory.
func Dir(stateDir string) string { return filepath.Join(stateDir, "tls") }

// Generate creates an ECDSA P-256 key and a self-signed leaf certificate for
// host, localhost, every non-loopback interface address and extra (each a
// DNS name or an IP address). It returns both PEM encoded; the key is PKCS #8.
func Generate(host string, extra []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	dns, ips, err := subjectAltNames(host, extra)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	notBefore := now.Add(-backdate).UTC().Truncate(time.Second)
	cn := host
	if cn == "" {
		cn = "localhost"
	}
	tmpl := &x509.Certificate{
		// A nil SerialNumber makes CreateCertificate draw a random positive
		// serial of at most 20 octets (RFC 5280 4.1.2.2).
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"nexusd"}},
		NotBefore:             notBefore,
		NotAfter:              notBefore.AddDate(ValidityYears, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), nil
}

// subjectAltNames lists the names a generated certificate covers, without
// duplicates. Interface addresses are those present at generation time.
func subjectAltNames(host string, extra []string) ([]string, []net.IP, error) {
	var dns []string
	var ips []net.IP
	seenDNS := map[string]bool{}
	seenIP := map[string]bool{}
	addIP := func(ip net.IP) {
		if k := ip.String(); !seenIP[k] {
			seenIP[k] = true
			ips = append(ips, ip)
		}
	}
	addName := func(n string) {
		if n == "" {
			return
		}
		if ip := net.ParseIP(n); ip != nil {
			addIP(ip)
			return
		}
		if !seenDNS[n] {
			seenDNS[n] = true
			dns = append(dns, n)
		}
	}
	addName(host)
	addName("localhost")
	addrs, err := interfaceAddrs()
	if err != nil {
		return nil, nil, fmt.Errorf("list interface addresses: %w", err)
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsUnspecified() {
			continue
		}
		ip := ipn.IP
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		addIP(ip)
	}
	for _, n := range extra {
		addName(n)
	}
	return dns, ips, nil
}

// LoadOrCreate returns the certificate kept in dir, generating it first when
// dir holds no certificate. created reports a generation. The key is written
// before the certificate, so a certificate on disk always has its key; a
// certificate whose key is missing is an error, never a silent regeneration,
// because clients may have pinned it.
func LoadOrCreate(dir, host string, extra []string, now time.Time) (cert tls.Certificate, created bool, err error) {
	certPath := filepath.Join(dir, CertFileName)
	keyPath := filepath.Join(dir, KeyFileName)
	switch _, err := os.Stat(certPath); {
	case err == nil:
		if _, err := os.Stat(keyPath); err != nil {
			return tls.Certificate{}, false, fmt.Errorf("%s exists but its key %s cannot be used: %w; "+
				"restore the key, or remove %s to generate a new certificate (clients must then pin the new fingerprint)",
				certPath, keyPath, unwrapPath(err), dir)
		}
		kept, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, false, fmt.Errorf("load %s: %w", certPath, err)
		}
		return kept, false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return tls.Certificate{}, false, fmt.Errorf("stat %s: %w", certPath, unwrapPath(err))
	}

	certPEM, keyPEM, err := Generate(host, extra, now)
	if err != nil {
		return tls.Certificate{}, false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, false, fmt.Errorf("create %s: %w", dir, unwrapPath(err))
	}
	if err := writeFileAtomic(dir, KeyFileName, keyPEM); err != nil {
		return tls.Certificate{}, false, err
	}
	if err := writeFileAtomic(dir, CertFileName, certPEM); err != nil {
		return tls.Certificate{}, false, err
	}
	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("load generated certificate: %w", err)
	}
	return cert, true, nil
}

// writeFileAtomic writes data to dir/name with mode 0600 through a temporary
// file and a rename, then syncs the directory.
func writeFileAtomic(dir, name string, data []byte) (err error) {
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", name, unwrapPath(err))
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, unwrapPath(err))
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return d.Close()
}

func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
