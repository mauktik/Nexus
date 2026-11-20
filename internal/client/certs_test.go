// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testCA is a throwaway certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nexus test root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key}
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// leaf issues a 127.0.0.1 server certificate valid from notBefore to
// notAfter. A nil ca makes it self-signed.
func leaf(t *testing.T, ca *testCA, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "nexus test server"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	parent, signer := tmpl, any(key)
	if ca != nil {
		parent, signer = ca.cert, ca.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func validLeaf(t *testing.T, ca *testCA) tls.Certificate {
	return leaf(t, ca, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
}

// countingServer is a real TLS listener that counts requests and records
// every header value it receives.
type countingServer struct {
	*httptest.Server
	requests atomic.Int64
	mu       sync.Mutex
	headers  []string
}

func startTLS(t *testing.T, cert tls.Certificate, h http.HandlerFunc) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.requests.Add(1)
		cs.mu.Lock()
		for k, vs := range r.Header {
			for _, v := range vs {
				cs.headers = append(cs.headers, k+": "+v)
			}
		}
		cs.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"/capabilities","host":"test","apiVersion":"v1","version":"v0","providers":[]}`))
	}))
	cs.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	cs.StartTLS()
	t.Cleanup(cs.Close)
	return cs
}

func (cs *countingServer) sawHeader(sub string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, h := range cs.headers {
		if strings.Contains(h, sub) {
			return true
		}
	}
	return false
}
