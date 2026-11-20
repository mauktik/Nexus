// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package v5

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// FTLAPITimeout is the total deadline of one FTL text-API request: dial,
// command and reply (A-2). On a timeout the fields are null, never an error
// to the caller of a read.
const FTLAPITimeout = 500 * time.Millisecond

// endOfMessage ends every FTL text-API reply (V-1).
const endOfMessage = "---EOM---"

// The bounds of one reply (security review SEC-1). FTLPORT is unprivileged,
// so while FTL is down any local user can bind it and answer Nexus with
// whatever they like; a reply is read, parsed and kept only up to these
// limits. A real >stats or >version reply is under 1 KiB and has under 20
// keys (test/harness/pihole/transcripts/ftl-v5.25.2-*.txt).
const (
	// MaxFTLReply is the most bytes read from one reply.
	MaxFTLReply = 64 << 10
	// maxFTLKeys is the most distinct keys kept from one reply.
	maxFTLKeys = 64
)

// FTLAddr is the text-API address for port: FTL listens on loopback only
// (V-4, socket_listenlocal).
func FTLAddr(port int) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(port))
}

// Stats is the part of the `>stats` reply the reader uses (V-2). A key FTL
// did not send, or sent with an unparsable value, is nil.
type Stats struct {
	DomainsBeingBlocked *int64
	DNSQueriesToday     *int64
	AdsBlockedToday     *int64
	AdsPercentageToday  *float64
	UniqueClients       *int64
	Status              *string // enabled or disabled
}

// FTLStats sends `>stats` to FTL's text API at addr. Unknown keys are
// ignored.
func FTLStats(ctx context.Context, addr netip.AddrPort, timeout time.Duration) (Stats, error) {
	kv, err := ftlRequest(ctx, addr, ">stats", timeout)
	if err != nil {
		return Stats{}, err
	}
	var s Stats
	s.DomainsBeingBlocked = parseInt(kv, "domains_being_blocked")
	s.DNSQueriesToday = parseInt(kv, "dns_queries_today")
	s.AdsBlockedToday = parseInt(kv, "ads_blocked_today")
	s.UniqueClients = parseInt(kv, "unique_clients")
	if v, ok := kv["ads_percentage_today"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.AdsPercentageToday = &f
		}
	}
	if v, ok := kv["status"]; ok && (v == "enabled" || v == "disabled") {
		s.Status = &v
	}
	return s, nil
}

// FTLVersion sends `>version` and returns the version line's value, cleaned
// to ^v?[0-9]+(\.[0-9]+)* (V-3), or nil when it is not a version.
func FTLVersion(ctx context.Context, addr netip.AddrPort, timeout time.Duration) (*string, error) {
	kv, err := ftlRequest(ctx, addr, ">version", timeout)
	if err != nil {
		return nil, err
	}
	v, ok := kv["version"]
	if !ok {
		return nil, nil
	}
	return CleanVersion(v), nil
}

func parseInt(kv map[string]string, key string) *int64 {
	v, ok := kv[key]
	if !ok {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// ftlRequest dials addr, sends command, reads `key value` lines up to
// ---EOM---, then sends >quit and closes (V-1). The whole exchange shares
// one deadline: timeout, or ctx's deadline when that is sooner.
func ftlRequest(ctx context.Context, addr netip.AddrPort, command string, timeout time.Duration) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, fmt.Errorf("ftl api %s: %w", command, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("ftl api %s: %w", command, err)
		}
	}
	// Close the connection if ctx is cancelled before the deadline.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	if _, err := io.WriteString(conn, command+"\n"); err != nil {
		return nil, fmt.Errorf("ftl api %s: %w", command, err)
	}
	kv := map[string]string{}
	capped := &io.LimitedReader{R: conn, N: MaxFTLReply}
	r := bufio.NewReader(capped)
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line == endOfMessage {
			break
		}
		if err != nil {
			if capped.N <= 0 {
				return nil, fmt.Errorf("ftl api %s: reply longer than %d bytes without %s", command, MaxFTLReply, endOfMessage)
			}
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("ftl api %s: reply without %s: %w", command, endOfMessage, err)
		}
		if k, v, ok := strings.Cut(line, " "); ok {
			if _, dup := kv[k]; !dup && len(kv) < maxFTLKeys {
				kv[k] = v
			}
		}
	}
	// The reply is complete. >quit only asks FTL to close its end (V-1);
	// if it cannot be sent, closing the socket ends the session the same
	// way, so the reply stands.
	if _, err := io.WriteString(conn, ">quit\n"); err != nil {
		return kv, nil
	}
	return kv, nil
}
