// Package client is the nexus CLI's HTTP transport to one nexusd host. Every
// HTTPS connection is verified: against a pinned SHA-256 leaf fingerprint, or
// against system trust. Nothing in this package can turn verification off.
package client

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

// Timeout bounds every unary request, connection to last body byte. The
// --follow stream client (9.5) is separate.
const Timeout = 30 * time.Second

// APIPrefix is the versioned path every API route lives under.
const APIPrefix = "/v1"

// Body size limits: a success body is read whole; an error body only needs
// to hold a problem document.
const (
	maxBodyBytes  = 64 << 20
	maxErrorBytes = 64 << 10
)

// Options configures New.
type Options struct {
	URL string // https://host[:port], or http:// on a loopback IP only
	// Fingerprint, when set, pins the server's leaf certificate. Empty means
	// system trust for https.
	Fingerprint string
	Token       string    // Sent as a bearer token; never printed
	Verbose     io.Writer // When set, receives one line per request and response
	// RootCAs replaces the system trust store. It exists for tests: Go on
	// macOS ignores SSL_CERT_FILE. Nil means the system store.
	RootCAs *x509.CertPool
}

// Client sends API requests to one host.
type Client struct {
	base    *url.URL
	http    *http.Client
	token   string
	verbose io.Writer
}

// PlainHTTPError is a plain http:// URL for an address other than a loopback
// IP literal. It is refused before any connection is made.
type PlainHTTPError struct{ URL string }

func (e *PlainHTTPError) Error() string {
	return fmt.Sprintf("refusing plain HTTP to %s: http:// is allowed only for a loopback address such as 127.0.0.1 or [::1]; use https://", e.URL)
}

// URLError is a host URL that is not a usable base URL.
type URLError struct {
	URL    string
	Reason string
}

func (e *URLError) Error() string { return fmt.Sprintf("invalid host URL %q: %s", e.URL, e.Reason) }

// TransportError is a failure to exchange a request and response with the
// host: refused or reset connections, DNS failure, TLS failure (including a
// pin mismatch) and timeouts. The CLI exits 5 for it.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// StatusError is a non-2xx response without a problem details body, other
// than a bare 404, which is reported as route-not-found.
type StatusError struct {
	Status int
	Body   string // Truncated
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("unexpected HTTP status %d %s", e.Status, http.StatusText(e.Status))
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// ParseURL validates a host URL: http or https, a host, no credentials, query
// or fragment, and no path beyond "/". Plain http is accepted only for a
// loopback IP literal.
func ParseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &URLError{URL: raw, Reason: "not a URL"}
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, &URLError{URL: raw, Reason: "the scheme must be https (or http for loopback)"}
	case u.Hostname() == "":
		return nil, &URLError{URL: raw, Reason: "no host"}
	case u.User != nil:
		return nil, &URLError{URL: raw, Reason: "credentials do not belong in the URL"}
	case u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
		return nil, &URLError{URL: raw, Reason: "a query or fragment is not allowed"}
	case u.Path != "" && u.Path != "/":
		return nil, &URLError{URL: raw, Reason: "a path is not allowed"}
	}
	if u.Scheme == "http" && !IsLoopbackHost(u.Hostname()) {
		return nil, &PlainHTTPError{URL: raw}
	}
	u.Path = ""
	return u, nil
}

// IsLoopbackHost reports whether host is a loopback IP literal. Names, even
// "localhost", are not accepted: resolution is outside the client's control.
func IsLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// New builds a client for one host.
func New(o Options) (*Client, error) {
	u, err := ParseURL(o.URL)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if u.Scheme == "https" {
		if o.Fingerprint != "" {
			pin, err := NormalizeFingerprint(o.Fingerprint)
			if err != nil {
				return nil, err
			}
			tr.TLSClientConfig = pinnedTLS(pin)
		} else {
			tr.TLSClientConfig = systemTLS(o.RootCAs)
		}
	}
	return &Client{
		base: u,
		http: &http.Client{
			Transport: tr,
			Timeout:   Timeout,
			// The API never redirects; following one could carry the token
			// somewhere unverified.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		token:   o.Token,
		verbose: o.Verbose,
	}, nil
}

// HTTPTimeout returns the unary client's overall timeout.
func (c *Client) HTTPTimeout() time.Duration { return c.http.Timeout }

// Do sends method to APIPrefix+path with in as a JSON body (nil for none)
// and decodes a 2xx JSON response into out (nil to discard). A non-2xx
// response is returned as a *problem.Error when it carries problem details.
// A 404 without one is returned as route-not-found: the host's nexusd does
// not serve that route, so it lacks the capability (NFR-5).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		body = bytes.NewReader(b)
	}
	target := c.base.JoinPath(APIPrefix, path)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json, "+problem.ContentType)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	c.logf("> %s %s", method, target.Redacted())
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.logf("< %s %s failed after %s", method, target.Redacted(), time.Since(start).Round(time.Millisecond))
		return &TransportError{Err: err}
	}
	defer resp.Body.Close()
	c.logf("< %s (%s)", resp.Status, time.Since(start).Round(time.Millisecond))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
		if err != nil {
			return &TransportError{Err: fmt.Errorf("read response: %w", err)}
		}
		if len(data) > maxBodyBytes {
			return fmt.Errorf("response body exceeds %d bytes", maxBodyBytes)
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return nil
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if err != nil {
		return &TransportError{Err: fmt.Errorf("read error response: %w", err)}
	}
	return responseError(resp.StatusCode, resp.Header.Get("Content-Type"), data)
}

// responseError turns a non-2xx response into an error.
func responseError(status int, contentType string, data []byte) error {
	if p := decodeProblem(contentType, data); p != nil {
		if p.Status == 0 {
			p.Status = status
		}
		return p
	}
	if status == http.StatusNotFound {
		return problem.New(problem.RouteNotFound, "the host answered 404 with no problem details: its nexusd does not serve this route")
	}
	return &StatusError{Status: status, Body: strings.TrimSpace(truncate(string(data), 200))}
}

// decodeProblem returns the problem details in a response body, or nil when
// the body is not one.
func decodeProblem(contentType string, data []byte) *problem.Error {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil || (mt != problem.ContentType && mt != "application/json") {
		return nil
	}
	var p problem.Error
	if err := json.Unmarshal(data, &p); err != nil || p.Type == "" {
		return nil
	}
	return &p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (c *Client) logf(format string, args ...any) {
	if c.verbose != nil {
		fmt.Fprintf(c.verbose, format+"\n", args...)
	}
}

// IsTransport reports whether err is a transport failure.
func IsTransport(err error) bool {
	var te *TransportError
	return errors.As(err, &te)
}
