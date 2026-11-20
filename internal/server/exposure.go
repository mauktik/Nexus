// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
)

// Exposure rules (FR-23, ASM-5). nexusd is a LAN service: TLS whenever an
// address other than loopback is bound, TLS 1.2 at least, no CORS headers
// anywhere, and forwarded headers never read (auth.SourceAddr). None of these
// has an override.

// MinTLSVersion is the lowest TLS version the server negotiates.
const MinTLSVersion = tls.VersionTLS12

// serverTLS copies the caller's TLS configuration and raises its minimum
// version to MinTLSVersion, so no caller can lower it.
func serverTLS(c *tls.Config) *tls.Config {
	c = c.Clone()
	if c.MinVersion < MinTLSVersion {
		c.MinVersion = MinTLSVersion
	}
	return c
}

// checkPlainHTTP refuses plain HTTP on a listener that is not loopback.
// Configuration validation already refuses tls.mode off with such an address;
// this is the transport's own guard against a wiring mistake.
func checkPlainHTTP(addr net.Addr) error {
	if isLoopback(addr) {
		return nil
	}
	return fmt.Errorf("refusing plain HTTP on %s: without TLS only loopback addresses may be served", addr)
}

func isLoopback(addr net.Addr) bool {
	switch a := addr.(type) {
	case *net.TCPAddr:
		return a.IP.IsLoopback()
	case nil:
		return false
	}
	ap, err := netip.ParseAddrPort(addr.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}
