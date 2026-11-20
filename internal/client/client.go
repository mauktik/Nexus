// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

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
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

// Timeout bounds every unary request, connection to last body byte. The
// --follow stream client is separate: see Stream.
const Timeout = 30 * time.Second

// Bounds of the --follow stream client, which has no overall Timeout (9.5).
const (
	StreamTLSHandshakeTimeout   = 10 * time.Second
	StreamResponseHeaderTimeout = 30 * time.Second
)

// Upload deadline: UploadBaseTimeout plus one second per UploadRate bytes.
const (
	UploadBaseTimeout = 30 * time.Second
	UploadRate        = 1 << 20 // Bytes per second
)

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
	stream  *http.Client // For Stream: no overall Timeout
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
	// The API never redirects; following one could carry the token
	// somewhere unverified.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// The stream client shares the TLS verification above. It has Timeout 0,
	// a recorded waiver of go/custom-http-client-timeout: a followed log
	// stays open as long as the operation or container runs, so any overall
	// bound would cut a healthy stream. It is bounded instead by the TLS
	// handshake and response header timeouts here, the dial timeout, and the
	// caller's no-frame watchdog, which cancels a silent stream (9.5).
	// Upload sends through it too, under the same waiver for the same
	// reason, and always bounds itself with a size-derived deadline (A-39).
	str := tr.Clone()
	str.TLSHandshakeTimeout = StreamTLSHandshakeTimeout
	str.ResponseHeaderTimeout = StreamResponseHeaderTimeout
	return &Client{
		base:    u,
		http:    &http.Client{Transport: tr, Timeout: Timeout, CheckRedirect: noRedirect},
		stream:  &http.Client{Transport: str, Timeout: 0, CheckRedirect: noRedirect},
		token:   o.Token,
		verbose: o.Verbose,
	}, nil
}

// HTTPTimeout returns the unary client's overall timeout.
func (c *Client) HTTPTimeout() time.Duration { return c.http.Timeout }

// StreamSettings are the stream client's bounds, for tests.
type StreamSettings struct {
	Timeout, TLSHandshakeTimeout, ResponseHeaderTimeout time.Duration
}

// StreamSettings returns the stream client's bounds.
func (c *Client) StreamSettings() StreamSettings {
	tr, _ := c.stream.Transport.(*http.Transport)
	s := StreamSettings{Timeout: c.stream.Timeout}
	if tr != nil {
		s.TLSHandshakeTimeout, s.ResponseHeaderTimeout = tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout
	}
	return s
}

// Request is one API request.
type Request struct {
	Method string
	Path   string // Under APIPrefix, with each segment already escaped
	Query  url.Values
	Header http.Header // Added to the request; Authorization is always the client's
	Body   any         // Sent as JSON; nil for none
	Accept string      // Empty means application/json
}

// Response is a 2xx response, its body read whole.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Do sends method to APIPrefix+path with in as a JSON body (nil for none)
// and decodes a 2xx JSON response into out (nil to discard). A non-2xx
// response is returned as a *problem.Error when it carries problem details.
// A 404 without one is returned as route-not-found: the host's nexusd does
// not serve that route, so it lacks the capability (NFR-5).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	resp, err := c.Send(ctx, Request{Method: method, Path: path, Body: in})
	if err != nil {
		return err
	}
	if out == nil || len(resp.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// Send sends r and returns a 2xx response. Any other response is an error,
// as for Do.
func (c *Client) Send(ctx context.Context, r Request) (*Response, error) {
	resp, err := c.do(ctx, c.http, r, nil)
	if err != nil {
		return nil, err
	}
	return readResponse(resp)
}

// readResponse reads and closes a 2xx response.
func readResponse(resp *http.Response) (*Response, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, &TransportError{Err: fmt.Errorf("read response: %w", err)}
	}
	if len(data) > maxBodyBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxBodyBytes)
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// Stream sends r through the stream client and returns the body of a 2xx
// response unread, for the caller to read as it arrives and close. Its
// context is the only bound once the headers are in: the caller cancels it
// when the stream falls silent. Any other response is an error, as for Do.
func (c *Client) Stream(ctx context.Context, r Request) (io.ReadCloser, error) {
	resp, err := c.do(ctx, c.stream, r, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// UploadRequest is one raw-body upload.
type UploadRequest struct {
	Path        string // Under APIPrefix, with each segment already escaped
	Query       url.Values
	ContentType string
	Body        io.Reader // Read up to Size bytes; Upload does not close it
	Size        int64     // Sent as Content-Length; Body must hold exactly this many bytes
}

// UploadTimeout is the deadline Upload gives a body of size bytes:
// UploadBaseTimeout plus one second per UploadRate bytes, rounded up.
func UploadTimeout(size int64) time.Duration {
	secs := max(size, 0) / UploadRate
	if size > 0 && size%UploadRate != 0 {
		secs++
	}
	const maxSecs = int64(math.MaxInt64/time.Second) - int64(UploadBaseTimeout/time.Second)
	return UploadBaseTimeout + time.Duration(min(secs, maxSecs))*time.Second
}

// Upload POSTs r.Body as is to APIPrefix+r.Path with Content-Length r.Size,
// through the stream client, so TLS verification and pinning are those of
// every request. It returns a 2xx response read whole; any other response
// is an error, as for Do.
//
// The stream client has no overall Timeout, because any fixed bound would
// cut a legitimate large upload: a recorded exception to
// go/custom-http-client-timeout (A-39), beside the stream client's own in
// New. It is safe only because Upload always derives the request context
// itself, whether or not ctx has a deadline (the earlier applies), so no
// upload is ever sent without UploadTimeout(r.Size). The dial, TLS
// handshake and response header timeouts of the stream transport still
// apply, the last counted once the body is written. Upload never goes
// through the unary client, whose Timeout would cut it.
func (c *Client) Upload(ctx context.Context, r UploadRequest) (*Response, error) {
	if r.Size < 0 {
		return nil, fmt.Errorf("upload size %d is negative", r.Size)
	}
	ctx, cancel := context.WithTimeout(ctx, UploadTimeout(r.Size))
	defer cancel()
	resp, err := c.do(ctx, c.stream, Request{Method: http.MethodPost, Path: r.Path, Query: r.Query},
		&rawBody{body: r.Body, contentType: r.ContentType, size: r.Size})
	if err != nil {
		return nil, err
	}
	return readResponse(resp)
}

// rawBody is a request body sent as is, with a known length.
type rawBody struct {
	body        io.Reader
	contentType string
	size        int64
}

// do sends r through hc, with raw as the body instead of r.Body when it is
// not nil, and returns a 2xx response with its body unread. It reads and
// closes any other response, and returns its error.
func (c *Client) do(ctx context.Context, hc *http.Client, r Request, raw *rawBody) (*http.Response, error) {
	var body io.Reader
	switch {
	case raw != nil && r.Body != nil:
		return nil, errors.New("a request has either a JSON or a raw body")
	case raw != nil && raw.size == 0:
		body = http.NoBody
	case raw != nil:
		// The caller owns the reader: the transport must not close it.
		body = io.NopCloser(raw.body)
	case r.Body != nil:
		b, err := json.Marshal(r.Body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		body = bytes.NewReader(b)
	}
	target := c.base.JoinPath(APIPrefix, r.Path)
	if len(r.Query) > 0 {
		target.RawQuery = r.Query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if raw != nil {
		req.ContentLength = raw.size
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	accept := r.Accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept+", "+problem.ContentType)
	switch {
	case raw != nil:
		req.Header.Set("Content-Type", raw.contentType)
	case r.Body != nil:
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Del("Authorization")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	c.logf("> %s %s", r.Method, target.Redacted())
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		c.logf("< %s %s failed after %s", r.Method, target.Redacted(), time.Since(start).Round(time.Millisecond))
		return nil, &TransportError{Err: err}
	}
	c.logf("< %s (%s)", resp.Status, time.Since(start).Round(time.Millisecond))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}

	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if err != nil {
		return nil, &TransportError{Err: fmt.Errorf("read error response: %w", err)}
	}
	return nil, responseError(resp.StatusCode, resp.Header.Get("Content-Type"), data)
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
