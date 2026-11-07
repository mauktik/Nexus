package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/tlsutil"
)

const sentinelToken = "nexus_SENTINEL_9f3c1e7a_do_not_send"

// The CLI's fingerprint is byte-for-byte what nexusd prints (WI 2.4).
func TestFingerprintMatchesServerFormat(t *testing.T) {
	cert := validLeaf(t, nil)
	der := cert.Certificate[0]
	if got, want := Fingerprint(der), tlsutil.Fingerprint(der); got != want {
		t.Fatalf("Fingerprint = %q, nexusd prints %q", got, want)
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	der := validLeaf(t, nil).Certificate[0]
	canon := Fingerprint(der)
	bare := strings.ReplaceAll(canon, ":", "")
	for _, in := range []string{canon, strings.ToLower(canon), bare, strings.ToLower(bare),
		"sha256 Fingerprint=" + canon, "SHA256 Fingerprint=" + canon, "  " + canon + "\n"} {
		got, err := NormalizeFingerprint(in)
		if err != nil || got != canon {
			t.Errorf("NormalizeFingerprint(%q) = %q, %v; want %q", in, got, err, canon)
		}
	}
	for _, in := range []string{"", "AB:CD", canon[:len(canon)-3], canon + ":00", "ZZ" + canon[2:]} {
		if _, err := NormalizeFingerprint(in); err == nil {
			t.Errorf("NormalizeFingerprint(%q) accepted a malformed fingerprint", in)
		}
	}
}

// TC-ERR59: a pin mismatch fails the handshake before any request, so the
// token never reaches the server.
func TestPinMismatchSendsNothing(t *testing.T) {
	srv := startTLS(t, validLeaf(t, nil), nil)
	wrong := Fingerprint(validLeaf(t, nil).Certificate[0])

	c, err := New(Options{URL: srv.URL, Fingerprint: wrong, Token: sentinelToken})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Do(context.Background(), http.MethodGet, "/capabilities", nil, nil)
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("Do = %v, want a *TransportError", err)
	}
	var pm *PinMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("Do = %v, want a *PinMismatchError in the chain", err)
	}
	if pm.Want != wrong || pm.Got != Fingerprint(srv.Certificate().Raw) {
		t.Errorf("mismatch = %+v, want pinned %s and presented %s", pm, wrong, Fingerprint(srv.Certificate().Raw))
	}
	if n := srv.requests.Load(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
	if srv.sawHeader(sentinelToken) {
		t.Error("the sentinel token reached the server")
	}
}

func TestPinMatchWorks(t *testing.T) {
	srv := startTLS(t, validLeaf(t, nil), nil)
	c, err := New(Options{URL: srv.URL, Fingerprint: Fingerprint(srv.Certificate().Raw), Token: sentinelToken})
	if err != nil {
		t.Fatal(err)
	}
	var caps apitypes.Capabilities
	if err := c.Do(context.Background(), http.MethodGet, "/capabilities", nil, &caps); err != nil {
		t.Fatalf("pinned request: %v", err)
	}
	if caps.ID != apitypes.CapabilitiesID || srv.requests.Load() != 1 {
		t.Errorf("caps %+v after %d requests", caps, srv.requests.Load())
	}
	if !srv.sawHeader("Bearer " + sentinelToken) {
		t.Error("the token was not sent as a bearer token on the pinned connection")
	}
}

// TC-153: system trust, an untrusted server, and a pinned expired leaf.
func TestSystemTrustAndPinnedExpiry(t *testing.T) {
	ca := newCA(t)
	trusted := startTLS(t, validLeaf(t, ca), nil)
	untrusted := startTLS(t, validLeaf(t, nil), nil)
	expiredLeaf := leaf(t, nil, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	expired := startTLS(t, expiredLeaf, nil)

	t.Run("system trust, trusted server", func(t *testing.T) {
		c, err := New(Options{URL: trusted.URL, RootCAs: ca.pool()})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Do(context.Background(), http.MethodGet, "/capabilities", nil, nil); err != nil {
			t.Fatalf("trusted server: %v", err)
		}
	})
	t.Run("system trust, untrusted server", func(t *testing.T) {
		c, err := New(Options{URL: untrusted.URL, RootCAs: ca.pool(), Token: sentinelToken})
		if err != nil {
			t.Fatal(err)
		}
		err = c.Do(context.Background(), http.MethodGet, "/capabilities", nil, nil)
		if !IsTransport(err) {
			t.Fatalf("untrusted server: %v, want a transport error", err)
		}
		if untrusted.requests.Load() != 0 {
			t.Error("a request reached the untrusted server")
		}
	})
	t.Run("pinned expired leaf", func(t *testing.T) {
		c, err := New(Options{URL: expired.URL, Fingerprint: Fingerprint(expiredLeaf.Certificate[0])})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Do(context.Background(), http.MethodGet, "/capabilities", nil, nil); err != nil {
			t.Fatalf("pinned expired leaf: %v", err)
		}
	})
	t.Run("system trust, expired leaf", func(t *testing.T) {
		c, err := New(Options{URL: expired.URL})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Do(context.Background(), http.MethodGet, "/capabilities", nil, nil); !IsTransport(err) {
			t.Fatalf("expired leaf under system trust: %v, want a transport error", err)
		}
	})
}

// A probe aborts the handshake: no request, and it reports system trust.
func TestProbe(t *testing.T) {
	ca := newCA(t)
	trusted := startTLS(t, validLeaf(t, ca), nil)
	selfSigned := startTLS(t, validLeaf(t, nil), nil)

	for _, tc := range []struct {
		srv     *countingServer
		trusted bool
	}{{trusted, true}, {selfSigned, false}} {
		u, _ := url.Parse(tc.srv.URL)
		res, err := Probe(context.Background(), u, ca.pool())
		if err != nil {
			t.Fatalf("Probe(%s): %v", tc.srv.URL, err)
		}
		if res.Fingerprint != Fingerprint(tc.srv.Certificate().Raw) {
			t.Errorf("Probe fingerprint %s, want %s", res.Fingerprint, Fingerprint(tc.srv.Certificate().Raw))
		}
		if res.SystemTrusted != tc.trusted {
			t.Errorf("Probe(%s).SystemTrusted = %v (%v), want %v", tc.srv.URL, res.SystemTrusted, res.SystemErr, tc.trusted)
		}
		if n := tc.srv.requests.Load(); n != 0 {
			t.Errorf("probe sent %d requests", n)
		}
	}
}
