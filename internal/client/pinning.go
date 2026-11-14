package client

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Fingerprint is the SHA-256 digest of a certificate's DER encoding, written
// as colon-separated upper-case hex pairs: the format of
// `openssl x509 -fingerprint -sha256` and of `nexusd tls fingerprint`. It is
// computed here rather than imported from internal/tlsutil so the CLI links
// only the contract packages (D-1); a test keeps the two identical.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	b.Grow(len(sum) * 3)
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// NormalizeFingerprint accepts a SHA-256 fingerprint as Fingerprint writes
// it, in either case, with or without colons, and with or without the
// "sha256 Fingerprint=" prefix openssl prints. It returns the canonical form.
func NormalizeFingerprint(s string) (string, error) {
	v := strings.TrimSpace(s)
	if i := strings.LastIndexByte(v, '='); i >= 0 && strings.HasPrefix(strings.ToLower(v), "sha256") {
		v = v[i+1:]
	}
	v = strings.ReplaceAll(v, ":", "")
	raw, err := hex.DecodeString(v)
	if err != nil || len(raw) != sha256.Size {
		return "", fmt.Errorf("fingerprint %q is not a SHA-256 fingerprint (64 hex digits, colons optional)", s)
	}
	h := strings.ToUpper(hex.EncodeToString(raw))
	pairs := make([]string, 0, len(raw))
	for i := 0; i < len(h); i += 2 {
		pairs = append(pairs, h[i:i+2])
	}
	return strings.Join(pairs, ":"), nil
}

// PinMismatchError is a server whose leaf certificate does not have the
// pinned fingerprint. The handshake is aborted, so no HTTP request, and no
// token, is ever sent to it.
type PinMismatchError struct {
	Want string // The pinned fingerprint
	Got  string // The fingerprint the server presented
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("certificate fingerprint mismatch: pinned %s, server presented %s", e.Want, e.Got)
}

// pinnedTLS returns a TLS configuration that accepts exactly the leaf whose
// SHA-256 fingerprint is pin. Chain and name verification are replaced by the
// pin, not removed: VerifyConnection runs on every handshake, resumed ones
// included, and fails it before any application data is sent. An expired
// pinned leaf is accepted, because the pin, not the CA system, is the trust
// anchor (A-16).
func pinnedTLS(pin string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // Replaced by VerifyConnection below
		VerifyConnection:   func(cs tls.ConnectionState) error { return verifyPin(pin, cs) },
	}
}

// PinnedTLS is pinnedTLS for a fingerprint in any form NormalizeFingerprint
// accepts. A fingerprint it cannot normalize is an error, never a
// configuration that accepts every certificate. The nexusd health check
// pins the daemon's own certificate with it.
func PinnedTLS(fingerprint string) (*tls.Config, error) {
	pin, err := NormalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	return pinnedTLS(pin), nil
}

func verifyPin(pin string, cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("server presented no certificate")
	}
	got := Fingerprint(cs.PeerCertificates[0].Raw)
	if subtle.ConstantTimeCompare([]byte(got), []byte(pin)) != 1 {
		return &PinMismatchError{Want: pin, Got: got}
	}
	return nil
}

// systemTLS verifies the server's chain and name against roots, or against
// the system trust store when roots is nil.
func systemTLS(roots *x509.CertPool) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
}

// ProbeResult is what one TLS handshake revealed about a server, before any
// decision to trust it.
type ProbeResult struct {
	Fingerprint string            // SHA-256 of the leaf
	Leaf        *x509.Certificate // The presented leaf
	// SystemTrusted reports whether the chain verifies against the system
	// roots (or the test roots) for the URL's host name. SystemErr says why
	// it does not.
	SystemTrusted bool
	SystemErr     error
}

// errProbed aborts a probe handshake once the certificates are captured.
var errProbed = errors.New("certificate captured")

// probeTimeout bounds the dial and handshake of a probe.
const probeTimeout = 10 * time.Second

// Probe connects to an https URL and returns the server's certificate. It
// aborts the handshake as soon as the certificates arrive: no handshake with
// an unverified peer ever completes, and no HTTP request or token is sent.
// roots is the test seam; nil means the system trust store.
func Probe(ctx context.Context, u *url.URL, roots *x509.CertPool) (ProbeResult, error) {
	if u.Scheme != "https" {
		return ProbeResult{}, fmt.Errorf("probe needs an https URL, got %q", u.Scheme)
	}
	var chain []*x509.Certificate
	conf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // Replaced by VerifyConnection, which never accepts
		VerifyConnection: func(cs tls.ConnectionState) error {
			chain = cs.PeerCertificates
			return errProbed
		},
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	d := &tls.Dialer{NetDialer: &net.Dialer{}, Config: conf}
	conn, err := d.DialContext(ctx, "tcp", hostPort(u))
	if conn != nil {
		_ = conn.Close() // Unreachable: VerifyConnection always fails the handshake
	}
	if !errors.Is(err, errProbed) {
		if err == nil {
			err = errors.New("handshake completed without certificate capture")
		}
		return ProbeResult{}, &TransportError{Err: err}
	}
	if len(chain) == 0 {
		return ProbeResult{}, &TransportError{Err: errors.New("server presented no certificate")}
	}
	res := ProbeResult{Fingerprint: Fingerprint(chain[0].Raw), Leaf: chain[0]}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, res.SystemErr = chain[0].Verify(x509.VerifyOptions{DNSName: u.Hostname(), Roots: roots, Intermediates: inter})
	res.SystemTrusted = res.SystemErr == nil
	return res, nil
}

// hostPort is the URL's host with its scheme's default port filled in.
func hostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := "443"
	if u.Scheme == "http" {
		port = "80"
	}
	return net.JoinHostPort(u.Hostname(), port)
}
