// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package dnscheck is a minimal DNS client that asks one given server for A
// records over UDP. It is built on golang.org/x/net/dns/dnsmessage and never
// goes through net.Resolver, so /etc/hosts and resolv.conf are never
// consulted: the answer is exactly what the named server (Pi-hole FTL) says.
package dnscheck

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Timing defaults (A-1, A-2 in planning/pihole-dns-zones/architecture.md).
const (
	// ReadyName is the name WaitReady probes; FTL always answers it.
	ReadyName = "pi.hole"
	// ReadyInterval is the pacing between readiness probes and between
	// per-name retries.
	ReadyInterval = 200 * time.Millisecond
	// ReadyTryTimeout bounds each readiness probe.
	ReadyTryTimeout = 500 * time.Millisecond
	// ReadyWindow is how long the write pipeline waits for FTL after a restart.
	ReadyWindow = 15 * time.Second
	// NameTryTimeout bounds each per-name verification query.
	NameTryTimeout = 2 * time.Second
	// NameTries is how many queries Verify makes per name.
	NameTries = 3
	// ProbeTimeout bounds the status probe.
	ProbeTimeout = 300 * time.Millisecond
)

// maxReply is the read buffer. Queries carry no EDNS OPT record, so a
// conforming server keeps UDP replies within 512 bytes (RFC 1035 §4.2.1); the
// larger buffer only keeps an oversized reply from being cut by the read.
const maxReply = 4096

var (
	// ErrTimeout reports that the server sent no matching reply within the
	// per-query timeout.
	ErrTimeout = errors.New("no reply before timeout")
	// ErrTruncated reports a reply with the TC bit set.
	ErrTruncated = errors.New("truncated reply")
)

// Answer is the outcome of one A query.
type Answer struct {
	// RCode is the RFC 1035 §4.1.1 response code by its IANA name in upper
	// case: NOERROR, FORMERR, SERVFAIL, NXDOMAIN, NOTIMP, REFUSED, or
	// RCODE<n> for any other value.
	RCode string
	// A holds the IPv4 addresses of the A records in the answer section, in
	// the order received.
	A []netip.Addr
}

// WantKind selects which FR-24 rule Check applies.
type WantKind int

const (
	// WantSet is an add or an update: the name must resolve to exactly IP.
	WantSet WantKind = iota
	// WantRemove is a remove of the line for IP.
	WantRemove
)

// Want is the expected state of one changed name.
type Want struct {
	Kind WantKind
	// IP is the commanded address (WantSet) or the removed one (WantRemove).
	IP netip.Addr
	// CoveredIP, for WantRemove, is the address of a remaining line that still
	// covers the name (dnsfile.Expected); the zero Addr means uncovered.
	CoveredIP netip.Addr
}

var rcodeNames = map[dnsmessage.RCode]string{
	dnsmessage.RCodeSuccess:        "NOERROR",
	dnsmessage.RCodeFormatError:    "FORMERR",
	dnsmessage.RCodeServerFailure:  "SERVFAIL",
	dnsmessage.RCodeNameError:      "NXDOMAIN",
	dnsmessage.RCodeNotImplemented: "NOTIMP",
	dnsmessage.RCodeRefused:        "REFUSED",
}

func rcodeName(rc dnsmessage.RCode) string {
	if n, ok := rcodeNames[rc]; ok {
		return n
	}
	return "RCODE" + strconv.Itoa(int(rc))
}

// Query sends one A query for fqdn, with RD set and a random ID, to server
// over UDP and returns the reply's RCODE and A answers. Replies whose ID or
// question does not match are discarded (RFC 1035 §7.3); if no matching reply
// arrives within timeout the error wraps ErrTimeout. A reply with TC set is
// ErrTruncated. Cancellation of ctx returns ctx.Err().
func Query(ctx context.Context, server netip.AddrPort, fqdn string, timeout time.Duration) (Answer, error) {
	if !server.IsValid() {
		return Answer{}, fmt.Errorf("dnscheck: invalid server address %q", server)
	}
	name := fqdn
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	qname, err := dnsmessage.NewName(name)
	if err != nil {
		return Answer{}, fmt.Errorf("dnscheck: name %q: %w", fqdn, err)
	}
	q := dnsmessage.Question{Name: qname, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	var idb [2]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return Answer{}, fmt.Errorf("dnscheck: query id: %w", err)
	}
	id := binary.BigEndian.Uint16(idb[:])
	msg, err := buildQuery(id, q)
	if err != nil {
		return Answer{}, fmt.Errorf("dnscheck: build query for %s: %w", fqdn, err)
	}

	tryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// A connected UDP socket only delivers datagrams from server. Dialing an
	// IP literal involves no name lookup.
	var d net.Dialer
	conn, err := d.DialContext(tryCtx, "udp", server.String())
	if err != nil {
		return Answer{}, queryErr(ctx, tryCtx, fqdn, server, err)
	}
	defer conn.Close()
	deadline, _ := tryCtx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return Answer{}, fmt.Errorf("dnscheck: set deadline: %w", err)
	}
	// Unblock the read at once if the caller cancels before the deadline.
	stop := context.AfterFunc(tryCtx, func() {
		_ = conn.SetDeadline(time.Now()) // best effort; the deadline above still applies
	})
	defer stop()

	if _, err := conn.Write(msg); err != nil {
		return Answer{}, queryErr(ctx, tryCtx, fqdn, server, err)
	}
	buf := make([]byte, maxReply)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return Answer{}, queryErr(ctx, tryCtx, fqdn, server, err)
		}
		ans, ok, err := parseReply(buf[:n], id, q)
		if err != nil {
			return Answer{}, fmt.Errorf("dnscheck: %s @%s: %w", fqdn, server, err)
		}
		if ok {
			return ans, nil
		}
	}
}

func buildQuery(id uint16, q dnsmessage.Question) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 0, 512), dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	return b.Finish()
}

// queryErr classifies a socket error: the caller's cancellation, the per-try
// timeout (ErrTimeout), or a transport failure such as ICMP port unreachable.
func queryErr(ctx, tryCtx context.Context, fqdn string, server netip.AddrPort, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ne net.Error
	if tryCtx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("dnscheck: %s @%s: %w", fqdn, server, ErrTimeout)
	}
	return fmt.Errorf("dnscheck: %s @%s: %w", fqdn, server, err)
}

// parseReply returns ok=false for a datagram that is not the reply to this
// query (not a response, other ID, other question, or unparseable header),
// which the caller discards and keeps waiting.
func parseReply(b []byte, id uint16, q dnsmessage.Question) (Answer, bool, error) {
	var p dnsmessage.Parser
	h, err := p.Start(b)
	if err != nil || !h.Response || h.ID != id {
		return Answer{}, false, nil
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 || !sameQuestion(qs[0], q) {
		return Answer{}, false, nil
	}
	if h.Truncated {
		return Answer{}, true, ErrTruncated
	}
	ans := Answer{RCode: rcodeName(h.RCode)}
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return Answer{}, true, fmt.Errorf("parse answer: %w", err)
		}
		if rh.Type != dnsmessage.TypeA || rh.Class != dnsmessage.ClassINET {
			if err := p.SkipAnswer(); err != nil {
				return Answer{}, true, fmt.Errorf("parse answer: %w", err)
			}
			continue
		}
		a, err := p.AResource()
		if err != nil {
			return Answer{}, true, fmt.Errorf("parse A record: %w", err)
		}
		ans.A = append(ans.A, netip.AddrFrom4(a.A))
	}
	return ans, true, nil
}

// sameQuestion compares names case-insensitively (RFC 4343).
func sameQuestion(got, want dnsmessage.Question) bool {
	return got.Type == want.Type && got.Class == want.Class &&
		strings.EqualFold(got.Name.String(), want.Name.String())
}

// WaitReady probes ReadyName on server every ReadyInterval, each probe bounded
// by ReadyTryTimeout, until any reply arrives (any RCODE, truncated included)
// and returns how long that took. After window without a reply it returns an
// error wrapping ErrTimeout.
func WaitReady(ctx context.Context, server netip.AddrPort, window time.Duration) (time.Duration, error) {
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	var last error
	for {
		tryStart := time.Now()
		_, err := Query(wctx, server, ReadyName, ReadyTryTimeout)
		if err == nil || errors.Is(err, ErrTruncated) {
			return time.Since(start), nil
		}
		if ctx.Err() != nil {
			return time.Since(start), ctx.Err()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			last = err
		}
		if err := pace(wctx, tryStart); err != nil {
			if ctx.Err() != nil {
				return time.Since(start), ctx.Err()
			}
			return time.Since(start), fmt.Errorf("dnscheck: %s @%s not ready after %v (last: %v): %w", ReadyName, server, window, last, ErrTimeout)
		}
	}
}

// pace waits until ReadyInterval has passed since tryStart, or ctx ends.
func pace(ctx context.Context, tryStart time.Time) error {
	wait := ReadyInterval - time.Since(tryStart)
	if wait <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Verify queries fqdn up to tries times, each bounded by perTry (callers pass
// NameTries and NameTryTimeout, A-1), and evaluates the reply with Check. Any
// error is retried after ReadyInterval pacing; a reply is final. When every
// try timed out, a remove passes (A-45: FTL has already answered readiness,
// and a remaining line would answer at once, so silence means the query went
// upstream) and an add or update fails with an error wrapping ErrTimeout.
func Verify(ctx context.Context, server netip.AddrPort, fqdn string, want Want, perTry time.Duration, tries int) (Answer, bool, error) {
	var err error
	for i := 0; i < tries; i++ {
		tryStart := time.Now()
		var ans Answer
		ans, err = Query(ctx, server, fqdn, perTry)
		if err == nil {
			return ans, Check(ans, want), nil
		}
		if ctx.Err() != nil {
			return Answer{}, false, ctx.Err()
		}
		if i < tries-1 {
			if perr := pace(ctx, tryStart); perr != nil {
				return Answer{}, false, perr
			}
		}
	}
	if errors.Is(err, ErrTimeout) && want.Kind == WantRemove {
		return Answer{}, true, nil
	}
	return Answer{}, false, err
}

// Check applies the FR-24 rules to one answer:
//   - WantSet: NOERROR and the A set equals {IP};
//   - WantRemove, uncovered: IP is absent (NXDOMAIN or an upstream answer
//     passes);
//   - WantRemove covered by another address X: IP is absent;
//   - WantRemove covered by X == IP: NOERROR and the A set equals {IP}.
//
// Answers are compared as sets, so a repeated record does not matter.
func Check(ans Answer, want Want) bool {
	switch want.Kind {
	case WantSet:
		return exactly(ans, want.IP)
	case WantRemove:
		if want.CoveredIP.IsValid() && want.CoveredIP == want.IP {
			return exactly(ans, want.IP)
		}
		return !slices.Contains(ans.A, want.IP)
	default:
		return false
	}
}

func exactly(ans Answer, ip netip.Addr) bool {
	if ans.RCode != "NOERROR" || len(ans.A) == 0 {
		return false
	}
	for _, a := range ans.A {
		if a != ip {
			return false
		}
	}
	return true
}
