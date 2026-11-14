package server_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
)

const (
	octetStream   = "application/octet-stream"
	fuCeiling     = 64 << 20
	fuUploadPath  = "/v1/providers/fu/uploads"
	fuApplyPath   = "/v1/providers/fu/actions/apply"
	fuEchoPath    = "/v1/providers/fu/actions/echo"
	uploadTimeout = 20 * time.Second
)

type fuIn struct {
	Tag string `json:"tag,omitempty"`
}

type fuOut struct {
	SizeBytes int64 `json:"sizeBytes"`
}

type fuNoteIn struct {
	Note string `json:"note,omitempty"`
}

// The F-UPL provider fu: an upload op, a JSON long-running op and a JSON
// sync op. def.Define registers globally, so each is declared once.
var (
	fuUpload = def.Define[fuIn, fuOut](def.Op{
		ID: "fu.uploads.add", Provider: "fu", Kind: def.KindUpload, ResourceType: "uploads",
		Summary: "s", Description: "Upload operation for the server upload tests.",
		Scope: def.Scope{Name: "fu:admin"}, Risk: def.RiskMutating, Mode: def.ModeSync,
		Upload: &def.Upload{ContentTypes: []string{octetStream}, MaxBytes: fuCeiling},
	})
	fuApply = def.Define[fuNoteIn, gadgetOut](def.Op{
		ID: "fu.actions.apply", Provider: "fu", Kind: def.KindProviderAction, Action: "apply",
		Summary: "s", Description: "Long-running JSON operation for the server upload tests.",
		Scope: def.Scope{Name: "fu:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI: def.CLIBinding{Noun: "fuapply", Verb: "update"},
	})
	fuEcho = def.Define[fuNoteIn, gadgetOut](def.Op{
		ID: "fu.actions.echo", Provider: "fu", Kind: def.KindProviderAction, Action: "echo",
		Summary: "s", Description: "Sync JSON operation for the server upload tests.",
		Scope: def.Scope{Name: "fu:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "fuecho", Verb: "test"},
	})
)

// fupl is the F-UPL provider. Its upload cap is the binding's MaxBytes;
// Receive streams the body to a temp file in dir, which it removes when the
// read fails, or to a discarding writer when dir is empty.
type fupl struct {
	maxBytes atomic.Int64
	dir      string
	admits   atomic.Int64
	receives atomic.Int64

	mu      sync.Mutex
	got     int64
	length  int64 // The ContentLength Receive was given
	recvErr error
	file    string
	done    chan struct{} // Closed when Receive returns
}

func newFupl(maxBytes int64, dir string) *fupl {
	f := &fupl{dir: dir, done: make(chan struct{})}
	f.maxBytes.Store(maxBytes)
	return f
}

func (*fupl) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "fu", ReadScope: "fu:read", Ops: []def.Op{fuUpload.Op(), fuApply.Op(), fuEcho.Op()}}
}
func (*fupl) Configure(func(any) error) error { return nil }
func (*fupl) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (f *fupl) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Upload(fuUpload, provider.UploadSpec[fuIn, fuOut]{
			MaxBytes: f.maxBytes.Load,
			Admit: func(context.Context, fuIn) (func(), error) {
				f.admits.Add(1)
				return nil, nil
			},
			Receive: f.receive,
		}),
		provider.InProcess(fuApply, provider.InProcessSpec[fuNoteIn, gadgetOut]{
			Run: func(_ context.Context, in fuNoteIn, _ *provider.Run) (gadgetOut, error) {
				return gadgetOut{Name: in.Note}, nil
			},
		}),
		provider.Sync(fuEcho, func(_ context.Context, in fuNoteIn) (gadgetOut, error) {
			return gadgetOut{Name: in.Note[:min(len(in.Note), 8)]}, nil
		}),
	}
}

// onlyWriter hides io.Discard's ReadFrom, so the copy uses the 32 KiB
// buffer Receive passes.
type onlyWriter struct{ io.Writer }

func (f *fupl) receive(_ context.Context, _ fuIn, b provider.UploadBody) (out fuOut, err error) {
	f.receives.Add(1)
	defer close(f.done)
	var n int64
	var name string
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.got, f.length, f.recvErr, f.file = n, b.ContentLength, err, name
	}()
	buf := make([]byte, 32<<10)
	if f.dir == "" {
		n, err = io.CopyBuffer(onlyWriter{io.Discard}, b.Reader, buf)
		return fuOut{SizeBytes: n}, err
	}
	tmp, err := os.CreateTemp(f.dir, "upload-*")
	if err != nil {
		return fuOut{}, err
	}
	name = tmp.Name()
	n, err = io.CopyBuffer(onlyWriter{tmp}, b.Reader, buf)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return fuOut{}, err
	}
	return fuOut{SizeBytes: n}, nil
}

// result waits for Receive to return and reports what it saw.
func (f *fupl) result(t *testing.T) (int64, error, string) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatal("Receive did not return")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got, f.recvErr, f.file
}

type uplServer struct {
	srv  *server.Server
	addr string
	tok  string
	lg   *audit.Log
}

type uplOptions struct {
	tls  *tls.Config
	tune func(*http.Server) // Applied before Serve
}

// startUploads runs the real server over fu on a loopback listener, with a
// token holding every fu scope and an audit log, which a mutating op needs.
func startUploads(t *testing.T, fu *fupl, o uplOptions) *uplServer {
	t.Helper()
	dir := t.TempDir()
	us := &uplServer{tok: mint(t, dir, "fu-all", "fu:read", "fu:admin")}
	copts := core.Options{Host: "testhost", Version: "v0.0.0-test", Logger: discardLog,
		Providers: map[string]core.ProviderConfig{"fu": {Enabled: true}}}
	lg, err := audit.Open(audit.Options{Dir: t.TempDir(), Host: "testhost", Logger: discardLog})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lg.Close(); err != nil {
			t.Error(err)
		}
	})
	us.lg, copts.Audit = lg, lg
	reg, err := core.New([]provider.Provider{fu}, copts)
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	srv, err := server.New(reg, server.Options{Logger: discardLog, Auth: auth.New(dir, auth.Options{Logger: discardLog}), TLS: o.tls})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if o.tune != nil {
		o.tune(srv.HTTP())
	}
	us.srv, us.addr = srv, serveOn(t, srv)
	return us
}

// zeroReader reads zeros forever.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func uploadRequest(t *testing.T, url, tok string, body io.Reader, length int64) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", octetStream)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req
}

func expectExtension(t *testing.T, p *problem.Error, key, want string) {
	t.Helper()
	got, err := json.Marshal(p.Extensions[key])
	if err != nil || string(got) != want {
		t.Fatalf("extension %s = %s (%v), want %s; problem %+v", key, got, err, want, p)
	}
}

// TC-SM-28: the upload route's cap is the binding's MaxBytes, not
// limits.maxBodyBytes, which still bounds every other route.
func TestTCSM28_TheUploadRouteCapIsIndependent(t *testing.T) {
	fu := newFupl(8<<20, "")
	us := startUploads(t, fu, uplOptions{})
	base := "http://" + us.addr

	resp, body := send(t, uploadRequest(t, base+fuUploadPath, us.tok, io.LimitReader(zeroReader{}, 2<<20), 2<<20))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("2 MiB upload: status %d, want 201; body %s", resp.StatusCode, body)
	}
	if got, err, _ := fu.result(t); got != 2<<20 || err != nil {
		t.Fatalf("Receive saw %d bytes, error %v; want %d and nil", got, err, 2<<20)
	}

	big := `{"note":"` + strings.Repeat("x", 2<<20) + `"}`
	resp, body = request(t, http.MethodPost, base+fuApplyPath, us.tok, strings.NewReader(big))
	expectProblem(t, "2 MiB JSON body", resp, body, http.StatusRequestEntityTooLarge, problem.PayloadTooLarge)

	before := fu.admits.Load()
	resp, body = send(t, uploadRequest(t, base+fuUploadPath, us.tok, io.LimitReader(zeroReader{}, 8<<20+1), 8<<20+1))
	expectProblem(t, "8 MiB + 1 upload", resp, body, http.StatusRequestEntityTooLarge, problem.UploadTooLarge)
	expectExtension(t, decodeProblem(t, resp, body), "maxBytes", "8388608")
	if fu.admits.Load() != before || fu.receives.Load() != 1 {
		t.Fatalf("a declared oversize upload reached Admit (%d) or Receive (%d)", fu.admits.Load()-before, fu.receives.Load()-1)
	}
}

// throttled sends size bytes at rate bytes per second, in 64 KiB pieces.
type throttled struct {
	left, rate int64
	start      time.Time
	sent       int64
}

func (r *throttled) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	if r.start.IsZero() {
		r.start = time.Now()
	}
	due := r.start.Add(time.Duration(r.sent) * time.Second / time.Duration(r.rate))
	time.Sleep(time.Until(due))
	n := int(min(int64(len(p)), r.left, 64<<10))
	clear(p[:n])
	r.left -= int64(n)
	r.sent += int64(n)
	return n, nil
}

// TC-SM-29 (A-5): with the base read and write timeouts at 1 s and 1.5 s, an upload
// that takes 3 s is not cut over HTTP/2-over-TLS or HTTP/1.1-over-TLS,
// while a JSON POST trickled for 2 s is.
func TestTCSM29_ASlowUploadLongerThanThe(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 10 s")
	}
	cert, pool := testCert(t)
	clients := map[string]*http.Client{
		"h2": {Timeout: uploadTimeout, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}, ForceAttemptHTTP2: true}},
		"h1": {Timeout: uploadTimeout, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"},
			TLSNextProto:    map[string]func(string, *tls.Conn) http.RoundTripper{}}},
	}
	for _, proto := range []string{"h2", "h1"} {
		t.Run(proto, func(t *testing.T) {
			hc := clients[proto]
			t.Cleanup(hc.CloseIdleConnections)
			fu := newFupl(4<<20, "")
			spy := &bodySpy{}
			us := startUploads(t, fu, uplOptions{tls: &tls.Config{Certificates: []tls.Certificate{cert}}, tune: func(hs *http.Server) {
				// Both base deadlines are below the 3 s upload, so it passes only
				// if both are extended; the write deadline comes later, so the
				// control is cut by the read deadline, not by a stream reset.
				hs.ReadTimeout, hs.WriteTimeout = time.Second, 1500*time.Millisecond
				hs.Handler = spy.wrap(fuEchoPath, hs.Handler)
			}})
			base := "https://" + us.addr
			wantMajor := map[string]int{"h2": 2, "h1": 1}[proto]

			start := time.Now()
			resp, err := hc.Do(uploadRequest(t, base+fuUploadPath, us.tok, &throttled{left: 3 << 20, rate: 1 << 20}, 3<<20))
			if err != nil {
				t.Fatalf("slow upload: %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			elapsed := time.Since(start)
			if err != nil || resp.StatusCode != http.StatusCreated || resp.ProtoMajor != wantMajor {
				t.Fatalf("slow upload: %s %d, read error %v, body %s; want HTTP/%d 201", resp.Proto, resp.StatusCode, err, body, wantMajor)
			}
			if elapsed < 2500*time.Millisecond || elapsed > 8*time.Second {
				t.Fatalf("slow upload took %v, want about 3s", elapsed)
			}
			if got, rerr, _ := fu.result(t); got != 3<<20 || rerr != nil {
				t.Fatalf("Receive saw %d bytes, error %v; want %d and nil", got, rerr, 3<<20)
			}

			// Control: a route that is not an upload route keeps the base
			// read timeout, so a body trickled for 2 s is cut at 1 s.
			note := `{"note":"` + strings.Repeat("x", 2<<10-11) + `"}`
			req, err := http.NewRequest(http.MethodPost, base+fuEchoPath,
				&slowCopy{src: strings.NewReader(note), per: 2 * time.Second / 32, n: len(note) / 32})
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+us.tok)
			req.Header.Set("Content-Type", "application/json")
			// The server's read of the body must fail on its read deadline;
			// the client then sees one of the shapes that cut produces: a
			// transport error (h1 closes the connection, h2 resets the
			// stream), or the 400 the unreadable body gives, whose body the
			// write deadline may cut in turn. Nothing else passes.
			start = time.Now()
			resp, err = hc.Do(req)
			if err == nil {
				body, rerr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("control: status %d, body %s; want a transport error or 400 from the read-deadline cut", resp.StatusCode, body)
				}
				if rerr == nil {
					expectProblem(t, "control", resp, body, http.StatusBadRequest, problem.ValidationFailed)
				}
			}
			if elapsed := time.Since(start); elapsed >= 2*time.Second {
				t.Fatalf("control: answered after %v, want the 1s read deadline to cut the 2s trickle", elapsed)
			}
			if serr := spy.err(); !errors.Is(serr, os.ErrDeadlineExceeded) {
				t.Fatalf("control: the server's body read ended with %v, want a read-deadline error (client error %v)", serr, err)
			}
			t.Logf("control request cut: client error %v, server read error %v", err, spy.err())
		})
	}
}

// bodySpy records the error that ends the server's read of the request body
// on one path, below every middleware.
type bodySpy struct {
	mu   sync.Mutex
	last error
}

func (s *bodySpy) wrap(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			r.Body = &spiedBody{ReadCloser: r.Body, spy: s}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *bodySpy) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

type spiedBody struct {
	io.ReadCloser
	spy *bodySpy
}

func (b *spiedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.spy.mu.Lock()
		if b.spy.last == nil {
			b.spy.last = err
		}
		b.spy.mu.Unlock()
	}
	return n, err
}

// slowCopy hands out src n bytes at a time, one piece per interval.
type slowCopy struct {
	src io.Reader
	per time.Duration
	n   int
}

func (s *slowCopy) Read(p []byte) (int, error) {
	time.Sleep(s.per)
	return s.src.Read(p[:min(len(p), s.n)])
}

// TC-SM-30 (NFR-4, A-40): streaming 64 MiB through a Receive that copies
// with a 32 KiB buffer grows the heap by less than 8 MiB.
func TestTCSM30_Streaming64MiBGrowsTheHeap(t *testing.T) {
	fu := newFupl(fuCeiling, "")
	us := startUploads(t, fu, uplOptions{})
	hc := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{}}
	t.Cleanup(hc.CloseIdleConnections)

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	baseline := ms.HeapInuse
	var peak atomic.Uint64
	peak.Store(baseline)
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
				}
			}
		}
	}()
	resp, err := hc.Do(uploadRequest(t, "http://"+us.addr+fuUploadPath, us.tok, io.LimitReader(zeroReader{}, fuCeiling), fuCeiling))
	close(stop)
	<-sampled
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201; body %s", resp.StatusCode, body)
	}
	if got, rerr, _ := fu.result(t); got != fuCeiling || rerr != nil {
		t.Fatalf("Receive saw %d bytes, error %v; want %d", got, rerr, fuCeiling)
	}
	growth := int64(peak.Load()) - int64(baseline)
	t.Logf("HeapInuse baseline %d, peak %d, growth %d", baseline, peak.Load(), growth)
	if growth >= 8<<20 {
		t.Fatalf("HeapInuse grew by %d bytes while streaming 64 MiB, want under 8 MiB", growth)
	}
}

// TC-SM-E2: a chunked body exactly at the cap is stored; one byte more
// fails mid-stream as upload-too-large, and Receive keeps nothing.
func TestTCSME2_AChunkedBodyExactlyAtThe(t *testing.T) {
	const capBytes = 64 << 10
	for _, c := range []struct {
		size   int64
		status int
	}{{capBytes, http.StatusCreated}, {capBytes + 1, http.StatusRequestEntityTooLarge}} {
		t.Run(fmt.Sprint(c.size), func(t *testing.T) {
			dir := t.TempDir()
			fu := newFupl(capBytes, dir)
			us := startUploads(t, fu, uplOptions{})
			req := uploadRequest(t, "http://"+us.addr+fuUploadPath, us.tok,
				io.NopCloser(io.LimitReader(zeroReader{}, c.size)), -1)
			resp, body := send(t, req)
			got, rerr, file := fu.result(t)
			fu.mu.Lock()
			length := fu.length
			fu.mu.Unlock()
			if length != -1 {
				t.Fatalf("Receive was given ContentLength %d, want -1 (chunked)", length)
			}
			if c.status == http.StatusCreated {
				if resp.StatusCode != http.StatusCreated || got != c.size || rerr != nil {
					t.Fatalf("at the cap: status %d, Receive saw %d bytes, error %v; body %s", resp.StatusCode, got, rerr, body)
				}
				return
			}
			expectProblem(t, "cap + 1", resp, body, http.StatusRequestEntityTooLarge, problem.UploadTooLarge)
			expectExtension(t, decodeProblem(t, resp, body), "maxBytes", fmt.Sprint(capBytes))
			if !errors.Is(rerr, core.ErrUploadTooLarge) {
				t.Fatalf("Receive error %v, want one wrapping core.ErrUploadTooLarge", rerr)
			}
			if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temp file %q still there: %v", file, err)
			}
			if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
				t.Fatalf("Receive kept %v", left)
			}
		})
	}
}

// blockingBody sends head, then blocks until release is closed.
type blockingBody struct {
	head    io.Reader
	release chan struct{}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if n, err := b.head.Read(p); n > 0 || !errors.Is(err, io.EOF) {
		return n, err
	}
	<-b.release
	return 0, io.EOF
}

// TC-SM-ERR16 (NFR-7): an unauthenticated upload is answered 401 while the
// client is still holding the rest of its body, so the server never waited
// for it; Admit and Receive never run. A watchdog releases the body after
// 3 s, because the client cannot give up while its body read blocks; a
// response that only came after it is the failure.
func TestTCSMERR16_AnUnauthenticatedUploadIsAnswered401(t *testing.T) {
	fu := newFupl(fuCeiling, "")
	us := startUploads(t, fu, uplOptions{})
	for _, c := range []struct{ name, authz string }{{"no Authorization", ""}, {"bad token", "Bearer nexus_bogus_token"}} {
		t.Run(c.name, func(t *testing.T) {
			hc := &http.Client{Transport: &http.Transport{}}
			defer hc.CloseIdleConnections()
			release := make(chan struct{})
			var once sync.Once
			var watchdogFired atomic.Bool
			free := func() { once.Do(func() { close(release) }) }
			watchdog := time.AfterFunc(3*time.Second, func() { watchdogFired.Store(true); free() })
			defer watchdog.Stop()
			defer free()
			req := uploadRequest(t, "http://"+us.addr+fuUploadPath, "",
				&blockingBody{head: strings.NewReader(strings.Repeat("z", 1<<10)), release: release}, -1)
			if c.authz != "" {
				req.Header.Set("Authorization", c.authz)
			}
			resp, err := hc.Do(req)
			if watchdogFired.Load() {
				t.Fatalf("the response (error %v) came only after the client body was released", err)
			}
			if err != nil {
				t.Fatalf("no response while the body was blocked: %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			free()
			expectProblem(t, c.name, resp, body, http.StatusUnauthorized, problem.Unauthorized)
		})
	}
	if fu.admits.Load() != 0 || fu.receives.Load() != 0 {
		t.Fatalf("Admit ran %d times and Receive %d, want 0", fu.admits.Load(), fu.receives.Load())
	}
}

// TC-SM-ERR17: a client that sends half of a declared 1 MiB and closes the
// connection leaves no file, an audit with requested and no stored record,
// and no server goroutine behind.
func TestTCSMERR17_AClientThatDisconnectsMidUpload(t *testing.T) {
	dir := t.TempDir()
	fu := newFupl(fuCeiling, dir)
	us := startUploads(t, fu, uplOptions{})
	baseline := runtime.NumGoroutine()

	conn, err := net.Dial("tcp", us.addr)
	if err != nil {
		t.Fatal(err)
	}
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		fuUploadPath, us.addr, us.tok, octetStream, 1<<20)
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(conn, io.LimitReader(zeroReader{}, 512<<10)); err != nil {
		t.Fatal(err)
	}
	// Close only once Receive is reading, so the disconnect is mid-body.
	deadline := time.Now().Add(5 * time.Second)
	for fu.receives.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	got, rerr, file := fu.result(t)
	if rerr == nil {
		t.Fatalf("Receive returned no error after %d bytes of a disconnected upload", got)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file %q still there: %v", file, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("Receive kept %v", left)
	}

	// The refused record is written after Receive returns.
	var outcomes []string
	for end := time.Now().Add(2 * time.Second); ; {
		outcomes = outcomes[:0]
		for _, r := range allRecords(t, us.lg) {
			if r.Action == fuUpload.Op().ID {
				outcomes = append(outcomes, r.Outcome)
			}
		}
		if len(outcomes) >= 2 || time.Now().After(end) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Contains(outcomes, "requested") || slices.Contains(outcomes, "stored") {
		t.Fatalf("audit outcomes %v, want a requested record and no stored one", outcomes)
	}

	var n int
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if n = runtime.NumGoroutine(); n <= baseline {
			return
		}
	}
	buf := make([]byte, 1<<20)
	t.Fatalf("goroutines %d, baseline %d, after 2s:\n%s", n, baseline, buf[:runtime.Stack(buf, true)])
}

// trickle sends per bytes of zeros every gap, until total bytes are sent,
// and stops early when stop is closed.
type trickle struct {
	total, per int
	gap        time.Duration
	sent       int
	stop       <-chan struct{}
}

func (r *trickle) Read(p []byte) (int, error) {
	if r.sent >= r.total {
		return 0, io.EOF
	}
	select {
	case <-time.After(r.gap):
	case <-r.stop:
		return 0, io.ErrClosedPipe
	}
	n := min(r.per, len(p), r.total-r.sent)
	clear(p[:n])
	r.sent += n
	return n, nil
}

// SEC-2: an upload, chunked or declaring its length, that falls below the
// minimum rate after the grace is cut by the read deadline, well before its
// window; one that keeps the rate is not. The grace and the rate
// are shortened through the test seam: 300 ms and 100 KB/s, against a trickle
// of 100 bytes every 100 ms (1 KB/s).
func TestUploadMinimumRate(t *testing.T) {
	defer server.SetUploadFloor(300*time.Millisecond, 100_000)()
	do := func(t *testing.T, fu *fupl, body io.Reader, length int64) (*http.Response, []byte, error) {
		t.Helper()
		us := startUploads(t, fu, uplOptions{})
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer cancel()
		req := uploadRequest(t, "http://"+us.addr+fuUploadPath, us.tok, body, length).WithContext(ctx)
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return resp, b, err
	}

	t.Run("a chunked trickle is cut", func(t *testing.T) {
		dir := t.TempDir()
		fu := newFupl(fuCeiling, dir)
		stop := make(chan struct{})
		defer close(stop)
		start := time.Now()
		resp, rbody, err := do(t, fu, &trickle{total: 100 * 600, per: 100, gap: 100 * time.Millisecond, stop: stop}, -1)
		_, rerr, file := fu.result(t)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("the trickle ran %v, want it cut within about 1s of the 300ms grace", elapsed)
		}
		if !errors.Is(rerr, os.ErrDeadlineExceeded) {
			t.Fatalf("Receive error %v, want the read deadline's", rerr)
		}
		if resp != nil && resp.StatusCode == http.StatusCreated {
			t.Errorf("a cut upload was answered 201")
		}
		if resp != nil {
			t.Logf("cut after %v: client saw %s %s", time.Since(start), resp.Status, rbody)
		} else {
			t.Logf("cut after %v: client error %v", time.Since(start), err)
		}
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temp file %q still there: %v", file, err)
		}
	})

	t.Run("a chunked body above the floor is stored", func(t *testing.T) {
		fu := newFupl(fuCeiling, "")
		resp, body, err := do(t, fu, &throttled{left: 300_000, rate: 400_000}, -1)
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("a chunked body at 4x the floor: %v, %v %s; want 201", err, resp, body)
		}
		if got, rerr, _ := fu.result(t); got != 300_000 || rerr != nil {
			t.Errorf("Receive saw %d bytes, error %v; want 300000 and nil", got, rerr)
		}
	})

	t.Run("a declared-length trickle is cut", func(t *testing.T) {
		// The declared length is a window of 30s+, so only the floor can cut it.
		dir := t.TempDir()
		fu := newFupl(fuCeiling, dir)
		stop := make(chan struct{})
		defer close(stop)
		start := time.Now()
		resp, rbody, err := do(t, fu, &trickle{total: 100 * 600, per: 100, gap: 100 * time.Millisecond, stop: stop}, 100*600)
		_, rerr, file := fu.result(t)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("the declared trickle ran %v, want it cut within about 1s of the 300ms grace", elapsed)
		}
		if !errors.Is(rerr, os.ErrDeadlineExceeded) {
			t.Fatalf("Receive error %v, want the read deadline's", rerr)
		}
		if resp != nil && resp.StatusCode == http.StatusCreated {
			t.Errorf("a cut upload was answered 201")
		}
		if resp != nil {
			t.Logf("cut after %v: client saw %s %s", time.Since(start), resp.Status, rbody)
		} else {
			t.Logf("cut after %v: client error %v", time.Since(start), err)
		}
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temp file %q still there: %v", file, err)
		}
	})

	t.Run("a declared-length body above the floor is stored", func(t *testing.T) {
		fu := newFupl(fuCeiling, "")
		resp, body, err := do(t, fu, &throttled{left: 300_000, rate: 400_000}, 300_000)
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("a declared body at 4x the floor: %v, %v %s; want 201", err, resp, body)
		}
		if got, rerr, _ := fu.result(t); got != 300_000 || rerr != nil {
			t.Errorf("Receive saw %d bytes, error %v; want 300000 and nil", got, rerr)
		}
	})
}
