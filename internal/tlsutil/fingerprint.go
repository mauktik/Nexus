// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package tlsutil

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Fingerprint is the SHA-256 digest of a certificate's DER encoding, the
// value clients pin, written as colon-separated upper-case hex pairs, as
// openssl x509 -fingerprint -sha256 prints it.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	var b strings.Builder
	b.Grow(len(sum) * 3)
	for i, c := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", c)
	}
	return b.String()
}

// FileFingerprint reads the first certificate in the PEM file at path and
// returns its Fingerprint. It is what `nexusd tls fingerprint` prints.
func FileFingerprint(path string) (string, error) {
	cert, err := ReadCertificate(path)
	if err != nil {
		return "", err
	}
	return Fingerprint(cert.Raw), nil
}

// ReadCertificate parses the first CERTIFICATE block of the PEM file at path.
func ReadCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCertificatePEM(data, path)
}

func parseCertificatePEM(data []byte, path string) (*x509.Certificate, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, errors.New(path + ": no PEM CERTIFICATE block")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return cert, nil
	}
}
