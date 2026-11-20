// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// uploadSeen is what the upload test server saw of one request.
type uploadSeen struct {
	method, path, query    string
	contentType, idemKey   string
	contentLength, bodyLen int64
}

// recordingTransport answers every request with a 201 upload result and
// keeps the request, so its context can be inspected after Upload returns.
type recordingTransport struct {
	mu  sync.Mutex
	req *http.Request
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return nil, err
		}
		if err := r.Body.Close(); err != nil {
			return nil, err
		}
	}
	rt.mu.Lock()
	rt.req = r
	rt.mu.Unlock()
	return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created",
		Header:  http.Header{"Content-Type": []string{"application/json"}},
		Body:    io.NopCloser(strings.NewReader(`{"uploadId":"u1"}`)),
		Request: r}, nil
}

func (rt *recordingTransport) last() *http.Request {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.req
}

// TC-SM-35 (WI 2.4; FR-6, FR-13): client.Upload sets Content-Length, honours
// pinning and derives its deadline from size.
func TestTCSM35_ClientUploadSetsContentLengthHonours(t *testing.T) {
	const size = 2 << 20
	var (
		mu   sync.Mutex
		seen []uploadSeen
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Errorf("read upload body: %v", err)
		}
		mu.Lock()
		seen = append(seen, uploadSeen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"),
			r.Header.Get("Idempotency-Key"), r.ContentLength, n})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"uploadId":"u1","sizeBytes":2097152}`))
	}))
	t.Cleanup(srv.Close)

	body := bytes.Repeat([]byte{0x5a}, size)
	query := url.Values{"signature": []string{"c2ln"}}
	req := func() UploadRequest {
		return UploadRequest{Path: "/providers/tp/uploads", Query: query, ContentType: "application/gzip",
			Body: bytes.NewReader(body), Size: size}
	}

	// Step 1: a client pinned to the server's certificate.
	c, err := New(Options{URL: srv.URL, Fingerprint: Fingerprint(srv.Certificate().Raw), Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Upload(context.Background(), req())
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if resp.Status != http.StatusCreated || !strings.Contains(string(resp.Body), `"uploadId":"u1"`) {
		t.Errorf("Upload = %d %s, want 201 with the upload result", resp.Status, resp.Body)
	}
	mu.Lock()
	got := seen
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(got))
	}
	want := uploadSeen{http.MethodPost, "/v1/providers/tp/uploads", "signature=c2ln", "application/gzip", "", size, size}
	if got[0] != want {
		t.Errorf("server saw %+v, want %+v", got[0], want)
	}

	// Step 2: a client pinned to another certificate sends nothing.
	wrong, err := New(Options{URL: srv.URL, Fingerprint: Fingerprint(validLeaf(t, nil).Certificate[0]), Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrong.Upload(context.Background(), req())
	var pm *PinMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("Upload with a wrong pin = %v, want a *PinMismatchError", err)
	}
	mu.Lock()
	if len(seen) != 1 {
		t.Errorf("server saw %d requests after the pin mismatch, want still 1", len(seen))
	}
	mu.Unlock()

	// Step 3: the deadline helper.
	for _, tc := range []struct {
		size int64
		want time.Duration
	}{{0, 30 * time.Second}, {1 << 20, 31 * time.Second}, {64 << 20, 94 * time.Second}} {
		if got := UploadTimeout(tc.size); got != tc.want {
			t.Errorf("UploadTimeout(%d) = %v, want %v", tc.size, got, tc.want)
		}
	}

	// Step 4: a caller context with no deadline still gets the derived one.
	rt := &recordingTransport{}
	c.stream.Transport = rt
	start := time.Now()
	if _, err := c.Upload(context.Background(), req()); err != nil {
		t.Fatalf("Upload through the recording transport: %v", err)
	}
	r := rt.last()
	dl, ok := r.Context().Deadline()
	if !ok {
		t.Fatal("the upload request context has no deadline (A-39)")
	}
	if lo, hi := start.Add(32*time.Second-time.Second), start.Add(32*time.Second+time.Second); dl.Before(lo) || dl.After(hi) {
		t.Errorf("deadline %v from start, want 32s ± 1s", dl.Sub(start))
	}
	if r.Context().Err() == nil {
		t.Error("the derived context is not canceled after Upload returned")
	}

	// Step 5: the caller's earlier deadline wins.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start = time.Now()
	if _, err := c.Upload(ctx, req()); err != nil {
		t.Fatalf("Upload with a caller deadline: %v", err)
	}
	r = rt.last()
	dl, ok = r.Context().Deadline()
	if !ok {
		t.Fatal("the upload request context has no deadline")
	}
	if lo, hi := start.Add(4*time.Second), start.Add(6*time.Second); dl.Before(lo) || dl.After(hi) {
		t.Errorf("deadline %v from start, want the caller's 5s ± 1s", dl.Sub(start))
	}
	if r.Context().Err() == nil {
		t.Error("the derived context is not canceled after Upload returned")
	}

	// The upload never goes through the unary client.
	if c.http.Transport == rt {
		t.Error("the unary client shares the stream transport")
	}
}

// A 413 problem is returned as a problem, with the status.
func TestUploadProblem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"type":"urn:nexus:problem:upload-too-large","title":"Upload too large","status":413,"maxBytes":1024}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Upload(context.Background(), UploadRequest{Path: "/providers/tp/uploads", ContentType: "application/octet-stream",
		Body: bytes.NewReader(make([]byte, 2048)), Size: 2048})
	p := asProblem(t, err)
	if p.Status != http.StatusRequestEntityTooLarge || p.Extensions["maxBytes"] != float64(1024) {
		t.Errorf("problem = %+v, want 413 with maxBytes 1024", p)
	}
	if _, err := c.Upload(context.Background(), UploadRequest{Path: "/x", Size: -1, Body: bytes.NewReader(nil)}); err == nil {
		t.Error("Upload accepted a negative size")
	}
}
