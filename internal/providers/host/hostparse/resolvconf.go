// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"net/netip"
	"strings"
)

// MaxNameservers bounds ResolvConf. glibc uses only the first MAXNS (3)
// servers (resolv.conf(5)); the provider reports more so that a long file is
// visible, but not without limit.
const MaxNameservers = 16

// ResolvConf returns the addresses of a resolv.conf's nameserver lines in
// file order, at most MaxNameservers. Comment lines (";" or "#" in the first
// column, resolv.conf(5)), other keywords and lines whose address does not
// parse are skipped. A file without a nameserver line is not an error.
func ResolvConf(b []byte) []netip.Addr {
	var out []netip.Addr
	for l := range lines(b) {
		if len(out) == MaxNameservers {
			break
		}
		if strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(f[1])
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	return out
}

// Stub reports whether addr is a local stub resolver: any loopback address,
// 127.0.0.0/8 or ::1 (A-25).
func Stub(addr netip.Addr) bool {
	return addr.Unmap().IsLoopback()
}

// resolvedStubs are systemd-resolved's two listeners, the DNS stub
// 127.0.0.53 and the DNS proxy 127.0.0.54 (systemd-resolved.service(8)).
var resolvedStubs = []netip.Addr{
	netip.AddrFrom4([4]byte{127, 0, 0, 53}),
	netip.AddrFrom4([4]byte{127, 0, 0, 54}),
}

// ResolvedInUse reports whether systemd-resolved serves this host (A-25):
// /etc/resolv.conf (etc) lists at least one server and every one is
// 127.0.0.53 or 127.0.0.54, and /run/systemd/resolve/resolv.conf (run) was
// read. A nil run means that file is absent or unreadable. The run file may
// list no server (resolved without upstreams) and still counts.
func ResolvedInUse(etc, run []byte) bool {
	if run == nil {
		return false
	}
	servers := ResolvConf(etc)
	if len(servers) == 0 {
		return false
	}
	for _, s := range servers {
		if a := s.Unmap(); a != resolvedStubs[0] && a != resolvedStubs[1] {
			return false
		}
	}
	return true
}
