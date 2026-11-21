// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
)

// The window gate is the gate provider behind ops that declare a reconnect
// window (FR-14): start keeps retrying for 60 s, restart for 3 s.
const (
	wgateName       = "wgate"
	wgateReadScope  = "wgate:read"
	wgateWriteScope = "wgate:write"
)

var (
	wgateStart = def.Define[gateNameIn, gateOut](def.Op{
		ID: wgateName + ".gadgets.start", Provider: wgateName, Kind: def.KindResourceAction, ResourceType: "gadgets",
		Action: "start", Summary: "Start a gadget",
		Description: "Starts a reconnect test gadget. Side effects: none outside the test. " +
			"Long-running: answers 202 with an operation.",
		Scope: def.Scope{Name: wgateWriteScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI:    def.CLIBinding{Noun: "wgadget", Verb: "start", Positional: "name", ReconnectWindow: 60 * time.Second},
		Refine: refineGateName,
	})
	wgateRestart = def.Define[gateNameIn, gateOut](def.Op{
		ID: wgateName + ".gadgets.restart", Provider: wgateName, Kind: def.KindResourceAction, ResourceType: "gadgets",
		Action: "restart", Summary: "Restart a gadget",
		Description: "Restarts a reconnect test gadget. Side effects: none outside the test. " +
			"Long-running: answers 202 with an operation.",
		Scope: def.Scope{Name: wgateWriteScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI:    def.CLIBinding{Noun: "wgadget", Verb: "restart", Positional: "name", ReconnectWindow: 3 * time.Second},
		Refine: refineGateName,
	})
)

func wgateSpec() def.ProviderSpec {
	return def.ProviderSpec{Name: wgateName, ReadScope: wgateReadScope, Ops: []def.Op{wgateStart.Op(), wgateRestart.Op()}}
}

func reconnSpecs() []def.ProviderSpec { return append(fleetSpecs(), wgateSpec()) }

// wgate runs the gate's gadgets through the window ops.
type wgate struct{ *gate }

func (w wgate) Spec() def.ProviderSpec { return wgateSpec() }

func (w wgate) Bindings() []provider.Binding {
	run := func(ctx context.Context, in gateNameIn, r *provider.Run) (gateOut, error) {
		return w.run(ctx, in.Name, r.Output())
	}
	return []provider.Binding{
		provider.InProcess(wgateStart, provider.InProcessSpec[gateNameIn, gateOut]{Run: run}),
		provider.InProcess(wgateRestart, provider.InProcessSpec[gateNameIn, gateOut]{Run: run}),
	}
}

// fakeClock is F-CLK on the wait path: virtual time that moves only when
// the code under test sleeps, firing the events that fall due on the way.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	events []clockEvent // By time
	sleeps []time.Duration
}

type clockEvent struct {
	at time.Time
	f  func()
}

// useFakeClock installs a fake clock as waitClock until the test ends. The
// test must not call t.Parallel: waitClock is shared.
func useFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	fc := &fakeClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	old := waitClock
	waitClock = clock{now: fc.Now, sleep: fc.Sleep}
	t.Cleanup(func() { waitClock = old })
	return fc
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// at runs f when virtual time reaches at.
func (c *fakeClock) at(at time.Time, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i, _ := slices.BinarySearchFunc(c.events, at, func(e clockEvent, at time.Time) int {
		if e.at.After(at) {
			return 1
		}
		return -1 // Events at the same time run in the order added
	})
	c.events = slices.Insert(c.events, i, clockEvent{at, f})
}

// Sleep advances virtual time by d, running each event due by then at its
// own time, outside the lock.
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	end := c.now.Add(d)
	for len(c.events) > 0 && !c.events[0].at.After(end) {
		ev := c.events[0]
		c.events = c.events[1:]
		if ev.at.After(c.now) {
			c.now = ev.at
		}
		c.mu.Unlock()
		ev.f()
		c.mu.Lock()
	}
	c.now = end
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sleeps)
}

// stampWriter is stderr that stamps each line with the time it was written,
// on the clock the test reads.
type stampWriter struct {
	now func() time.Time

	mu    sync.Mutex
	all   strings.Builder
	part  string
	lines []stampedLine
}

type stampedLine struct {
	at   time.Time
	text string
}

func (s *stampWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all.Write(p)
	s.part += string(p)
	for {
		i := strings.IndexByte(s.part, '\n')
		if i < 0 {
			break
		}
		s.lines = append(s.lines, stampedLine{s.now(), s.part[:i]})
		s.part = s.part[i+1:]
	}
	return len(p), nil
}

func (s *stampWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all.String()
}

func (s *stampWriter) stamped() []stampedLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lines)
}

// attempt is one request the CLI logged with --verbose.
type attempt struct {
	at     time.Time
	path   string
	query  string
	failed bool // The CLI saw no HTTP response
	status string
}

// attempts are the requests of --verbose GET lines whose path ends with
// suffix, each with the response line that follows it.
func (s *stampWriter) attempts(t *testing.T, suffix string) []attempt {
	t.Helper()
	lines := s.stamped()
	var out []attempt
	for i, l := range lines {
		f := strings.Fields(l.text)
		if len(f) != 3 || f[0] != ">" || f[1] != http.MethodGet {
			continue
		}
		u, err := url.Parse(f[2])
		if err != nil {
			t.Fatalf("verbose line %q: %v", l.text, err)
		}
		if !strings.HasSuffix(u.Path, suffix) {
			continue
		}
		a := attempt{at: l.at, path: u.Path, query: u.RawQuery}
		for _, next := range lines[i+1:] {
			if strings.HasPrefix(next.text, "< ") {
				a.failed = strings.Contains(next.text, " failed after ")
				a.status = next.text
				break
			}
		}
		out = append(out, a)
	}
	return out
}

func failedOf(as []attempt) []attempt {
	var out []attempt
	for _, a := range as {
		if a.failed || strings.HasPrefix(a.status, "< 5") || strings.HasPrefix(a.status, "< 4") {
			out = append(out, a)
		}
	}
	return out
}

// reconnectLines are the reconnect progress lines on stderr, as text.
func (s *stampWriter) reconnectLines() []stampedLine {
	var out []stampedLine
	for _, l := range s.stamped() {
		if strings.Contains(l.text, "likely restarting") {
			out = append(out, l)
		}
	}
	return out
}

var verboseOpID = regexp.MustCompile(`^> GET \S+/v1/operations/([^/?\s]+)`)

// opID is the operation the CLI read first, from --verbose.
func (s *stampWriter) opID(t *testing.T) string {
	t.Helper()
	for _, l := range s.stamped() {
		if m := verboseOpID.FindStringSubmatch(l.text); m != nil {
			return m[1]
		}
	}
	t.Fatalf("no operation read in stderr:\n%s", s.String())
	return ""
}

// The outages F-PROXY injects.
type outage int32

const (
	upPass      outage = iota
	downRefuse         // The listener is closed: connection refused
	downProblem        // Every request is answered with fp.prob
	downPin            // TLS with another certificate
)

// flakyProxy is F-PROXY: a proxy in front of the recording proxy that can
// refuse connections, answer problem details, or present a certificate
// other than the pinned one. Changing the outage closes idle connections,
// so the next request dials anew and sees it.
type flakyProxy struct {
	t        *testing.T
	addr     string
	scheme   string
	upstream *url.URL
	rev      *httputil.ReverseProxy
	tlsCfg   *tls.Config
	pinned   tls.Certificate
	other    tls.Certificate

	mu        sync.Mutex
	mode      outage
	prob      *problem.Error
	ln        net.Listener
	srv       *http.Server
	servers   []*http.Server
	afterPost func()
	cutAfter  int    // Frames of the first log stream before onCut
	onCut     func() // Runs once the first log stream forwarded cutAfter frames
	streams   int
	lastSent  int64
}

func startFlakyProxy(t *testing.T, upstream string, useTLS bool) *flakyProxy {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	fp := &flakyProxy{t: t, scheme: "http", upstream: u, lastSent: -1}
	fp.rev = httputil.NewSingleHostReverseProxy(u)
	fp.rev.FlushInterval = -1
	fp.rev.ErrorLog = log.New(io.Discard, "", 0)
	if useTLS {
		fp.scheme = "https"
		fp.pinned, fp.other = issue(t, nil), issue(t, nil)
		fp.tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12, SessionTicketsDisabled: true,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				fp.mu.Lock()
				defer fp.mu.Unlock()
				if fp.mode == downPin {
					return &fp.other, nil
				}
				return &fp.pinned, nil
			}}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fp.addr = ln.Addr().String()
	fp.mu.Lock()
	fp.serve(ln)
	fp.mu.Unlock()
	t.Cleanup(fp.close)
	return fp
}

func (fp *flakyProxy) url() string { return fp.scheme + "://" + fp.addr }

// serve starts a server on ln. fp.mu is held.
func (fp *flakyProxy) serve(ln net.Listener) {
	if fp.tlsCfg != nil {
		ln = tls.NewListener(ln, fp.tlsCfg)
	}
	srv := &http.Server{Handler: fp, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	srv.SetKeepAlivesEnabled(fp.mode == upPass)
	fp.ln, fp.srv = ln, srv
	fp.servers = append(fp.servers, srv)
	go func() { _ = srv.Serve(ln) }()
}

// set switches to outage m, answering prob in downProblem.
func (fp *flakyProxy) set(m outage, prob *problem.Error) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	fp.mode, fp.prob = m, prob
	if fp.srv != nil {
		// Closes idle connections; one serving a request closes after it.
		fp.srv.SetKeepAlivesEnabled(m == upPass)
	}
	switch {
	case m == downRefuse && fp.ln != nil:
		_ = fp.ln.Close()
		fp.ln, fp.srv = nil, nil
	case m != downRefuse && fp.ln == nil:
		var ln net.Listener
		var err error
		for range 50 {
			if ln, err = net.Listen("tcp", fp.addr); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			fp.t.Errorf("relisten on %s: %v", fp.addr, err)
			return
		}
		fp.serve(ln)
	}
}

// window schedules outage m over virtual [from, to) on fc.
func (fp *flakyProxy) window(fc *fakeClock, from, to time.Time, m outage, prob *problem.Error) {
	fc.at(from, func() { fp.set(m, prob) })
	fc.at(to, func() { fp.set(upPass, nil) })
}

// onPost runs f once, after the next POST is answered.
func (fp *flakyProxy) onPost(f func()) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	fp.afterPost = f
}

// cutLogs makes the first log stream stop after n frames and run f.
func (fp *flakyProxy) cutLogs(n int, f func()) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	fp.cutAfter, fp.onCut = n, f
}

func (fp *flakyProxy) close() {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	for _, s := range fp.servers {
		_ = s.Close()
	}
	fp.ln, fp.srv = nil, nil
}

func (fp *flakyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fp.mu.Lock()
	mode, prob := fp.mode, fp.prob
	cut := false
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs") && mode == upPass {
		fp.streams++
		cut = fp.streams == 1 && fp.onCut != nil
	}
	var after func()
	if r.Method == http.MethodPost {
		after, fp.afterPost = fp.afterPost, nil
	}
	fp.mu.Unlock()
	if mode == downProblem {
		_ = problem.Write(w, prob)
		return
	}
	if cut {
		fp.forwardThenCut(w, r)
		return
	}
	fp.rev.ServeHTTP(w, r)
	if after != nil {
		after()
	}
}

// forwardThenCut forwards cutAfter frames of a log stream, runs onCut, and
// drops the connection.
func (fp *flakyProxy) forwardThenCut(w http.ResponseWriter, r *http.Request) {
	out, err := http.NewRequestWithContext(r.Context(), r.Method, fp.upstream.String()+r.URL.RequestURI(), nil)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	out.Header = r.Header.Clone()
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	sc := bufio.NewScanner(resp.Body)
	sent := 0
	for sc.Scan() {
		if _, err := w.Write(append(sc.Bytes(), '\n')); err != nil {
			panic(http.ErrAbortHandler)
		}
		_ = rc.Flush()
		var f struct {
			O *int64 `json:"o"`
		}
		if json.Unmarshal(sc.Bytes(), &f) == nil && f.O != nil {
			fp.mu.Lock()
			fp.lastSent = *f.O
			fp.mu.Unlock()
		}
		if sent++; sent == fp.cutAfter {
			break
		}
	}
	fp.onCut()
	panic(http.ErrAbortHandler)
}

// reconnEnv is a real server over a fresh gate, behind the recording proxy
// and F-PROXY, configured as the default host alpha.
type reconnEnv struct {
	g   *gate
	srv *providerServer
	rec *recProxy
	fp  *flakyProxy
	env Env
}

// startReconn serves the gate through the window ops, or with windowed
// false through the gate's own ops, which have none.
func startReconn(t *testing.T, windowed, useTLS bool) *reconnEnv {
	t.Helper()
	g := newGate()
	var p provider.Provider = g
	scopes := []string{gateReadScope, gateWriteScope}
	if windowed {
		p, scopes = wgate{g}, []string{wgateReadScope, wgateWriteScope}
	}
	srv := startProviderServer(t, "gate-host", p, true, scopes...)
	// Registered after the server, so it runs before its shutdown.
	t.Cleanup(g.open)
	rec := startRecProxy(t, srv.url)
	fp := startFlakyProxy(t, rec.URL, useTLS)
	home := writeClientConfig(t, "alpha", clientHost{"alpha", fp.url(), srv.token, nil})
	if useTLS {
		path := filepath.Join(home, ".config", "nexus", "config.yaml")
		cfg, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg = fmt.Appendf(cfg, "    fingerprint: %s\n", client.Fingerprint(fp.pinned.Certificate[0]))
		if err := os.WriteFile(path, cfg, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := homeEnv(home)
	env.OpenTerminal = noTerminal
	return &reconnEnv{g: g, srv: srv, rec: rec, fp: fp, env: env}
}

type reconnRun struct {
	code   int
	stdout string
	stderr *stampWriter
	wall   time.Duration
}

// run runs the CLI, stamping stderr lines on now.
func (re *reconnEnv) run(now func() time.Time, args ...string) reconnRun {
	var stdout strings.Builder
	sw := &stampWriter{now: now}
	env := re.env
	env.Stdout, env.Stderr = &stdout, sw
	start := time.Now()
	code := runWithSpecs(args, env, reconnSpecs())
	return reconnRun{code, stdout.String(), sw, time.Since(start)}
}

var reconnectSecs = regexp.MustCompile(`retrying \((\d+)s of (\d+)s\)`)

// pollT is the virtual time of the third regular poll, 0.25 + 0.5 + 1 s
// after the POST: t in the outage windows of TC-SM-36 to 39, so that t+2 s
// is the next regular poll (pollFirst doubling to pollMax).
func pollT(fc *fakeClock) time.Time { return fc.Now().Add(1750 * time.Millisecond) }

// checkReconnectCadence checks that reconnect progress lines come at most
// 10 virtual seconds apart and print the virtual time since the first.
func checkReconnectCadence(t *testing.T, lines []stampedLine, window string) {
	t.Helper()
	for i, l := range lines {
		m := reconnectSecs.FindStringSubmatch(l.text)
		if m == nil || m[2] != window {
			t.Fatalf("progress line %q, want `retrying (Ns of %ss)`", l.text, window)
		}
		n, _ := strconv.Atoi(m[1])
		if want := int(l.at.Sub(lines[0].at) / time.Second); n != want {
			t.Errorf("progress line %q at virtual +%s: prints %ds, want %ds", l.text, l.at.Sub(lines[0].at), n, want)
		}
		if i > 0 && l.at.Sub(lines[i-1].at) > 10*time.Second {
			t.Errorf("progress lines %s apart, want at most 10s", l.at.Sub(lines[i-1].at))
		}
	}
}

// TC-SM-36 (FR-14): a 20 s outage inside the 60 s window exits 0 with the
// final operation, on the virtual clock.
func TestTCSM36_FR14AcceptanceA20S(t *testing.T) {
	fc := useFakeClock(t)
	re := startReconn(t, true, false)
	tt := pollT(fc)
	re.fp.window(fc, tt.Add(2*time.Second), tt.Add(22*time.Second), downRefuse, nil)
	fc.at(tt.Add(25*time.Second), re.g.open)

	r := re.run(fc.Now, "wgadget", "start", "gated", "--verbose", "-o", "table")
	t.Logf("exit %d after %s, virtual %s\nstdout:\n%sstderr:\n%s", r.code, r.wall, fc.Now().Sub(tt), r.stdout, r.stderr)
	if r.code != 0 {
		t.Fatalf("exit %d, want 0", r.code)
	}
	id := r.stderr.opID(t)
	if !strings.Contains(r.stdout, id) || !strings.Contains(r.stdout, "Succeeded") {
		t.Errorf("stdout %q, want the Succeeded operation %s", r.stdout, id)
	}
	if st := re.srvStatus(t, id); st != "Succeeded" {
		t.Errorf("operation %s is %s, want Succeeded", id, st)
	}
	lines := r.stderr.reconnectLines()
	if len(lines) < 2 || !strings.Contains(lines[0].text, "is unreachable, likely restarting; retrying (0s of 60s)") ||
		!strings.Contains(lines[1].text, "retrying (10s of 60s)") {
		t.Fatalf("reconnect progress %v, want 0s of 60s then 10s of 60s", lines)
	}
	if !lines[0].at.Equal(tt.Add(2 * time.Second)) {
		t.Errorf("first reconnect line at virtual t+%s, want t+2s", lines[0].at.Sub(tt))
	}
	checkReconnectCadence(t, lines, "60")
	failed := failedOf(r.stderr.attempts(t, "/v1/operations/"+id))
	if n := len(failed); n < 19 || n > 21 {
		t.Errorf("%d failed polls, want 20 ± 1", n)
	}
	for i := 1; i < len(failed); i++ {
		if gap := failed[i].at.Sub(failed[i-1].at); gap != time.Second {
			t.Errorf("failed polls %d and %d are %s apart, want 1s", i-1, i, gap)
		}
	}
	if r.wall >= 5*time.Second {
		t.Errorf("wall time %s, want under 5s: a real sleep is on the poll or retry path", r.wall)
	}
}

// srvStatus reads operation id's status from the server directly.
func (re *reconnEnv) srvStatus(t *testing.T, id string) string {
	t.Helper()
	c, err := client.New(client.Options{URL: re.srv.url, Token: re.srv.token})
	if err != nil {
		t.Fatal(err)
	}
	var op struct {
		Status string `json:"status"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := c.Do(context.Background(), http.MethodGet, "/operations/"+id, nil, &op); err != nil {
			t.Fatal(err)
		}
		if op.Status != "Running" && op.Status != "Pending" || time.Now().After(deadline) {
			return op.Status
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TC-SM-37 (FR-14): a 61 s outage exits 5 naming the operation, and no poll
// comes later than one gap past the window.
func TestTCSM37_FR14AcceptanceA61S(t *testing.T) {
	fc := useFakeClock(t)
	re := startReconn(t, true, false)
	tt := pollT(fc)
	re.fp.window(fc, tt.Add(2*time.Second), tt.Add(63*time.Second), downRefuse, nil)

	r := re.run(fc.Now, "wgadget", "start", "gated", "--verbose", "-o", "table")
	t.Logf("exit %d after %s, virtual %s\nstderr:\n%s", r.code, r.wall, fc.Now().Sub(tt), r.stderr)
	if r.code != ExitConnection {
		t.Fatalf("exit %d, want 5", r.code)
	}
	id := r.stderr.opID(t)
	if want := "nexus operation get " + id + " --host alpha"; !strings.Contains(r.stderr.String(), want) {
		t.Errorf("stderr does not name %q", want)
	}
	polls := r.stderr.attempts(t, "/v1/operations/"+id)
	last := polls[len(polls)-1]
	if limit := tt.Add(2*time.Second + 60*time.Second + time.Second); last.at.After(limit) {
		t.Errorf("last poll at virtual t+%s, want none after t+63s", last.at.Sub(tt))
	}
	if n := len(failedOf(polls)); n != 61 {
		t.Errorf("%d failed polls, want 61: t+2s to t+62s, 1 s apart", n)
	}
	if r.wall >= 5*time.Second {
		t.Errorf("wall time %s, want under 5s", r.wall)
	}
}

// TC-SM-38 (FR-14): --follow rides out a 20 s outage after line 10 and
// resumes from the last offset with no gap and no repeat; a 61 s outage
// exits 5.
func TestTCSM38_FollowToleratesTheOutageAndResumes(t *testing.T) {
	var want []string
	for i := 1; i <= 40; i++ {
		want = append(want, strconv.Itoa(i))
	}
	digits := regexp.MustCompile(`^\d+$`)
	for _, c := range []struct {
		name   string
		outage time.Duration
		code   int
	}{{"20s", 20 * time.Second, ExitOK}, {"61s", 61 * time.Second, ExitConnection}} {
		t.Run(c.name, func(t *testing.T) {
			fc := useFakeClock(t)
			re := startReconn(t, true, false)
			var cutAt time.Time
			re.fp.cutLogs(10, func() {
				cutAt = fc.Now()
				fc.at(cutAt.Add(c.outage), func() { re.fp.set(upPass, nil) })
				re.fp.set(downRefuse, nil)
			})
			r := re.run(fc.Now, "wgadget", "start", "counter", "--follow", "--verbose", "-o", "table")
			t.Logf("exit %d after %s\nstderr:\n%s", r.code, r.wall, r.stderr)
			if r.code != c.code {
				t.Fatalf("exit %d, want %d", r.code, c.code)
			}
			if r.wall >= 5*time.Second {
				t.Errorf("wall time %s, want under 5s", r.wall)
			}
			// --follow shows the same reconnect progress as a plain wait: a line
			// at the first failure and then at most 10 s apart, in the same format.
			lines := r.stderr.reconnectLines()
			if len(lines) < 2 || !strings.Contains(lines[0].text, "is unreachable, likely restarting; retrying (0s of 60s)") ||
				!strings.Contains(lines[1].text, "retrying (10s of 60s)") {
				t.Fatalf("reconnect progress %v, want 0s of 60s then 10s of 60s", lines)
			}
			if !lines[0].at.Equal(cutAt) {
				t.Errorf("first reconnect line at virtual +%s after the cut, want +0s", lines[0].at.Sub(cutAt))
			}
			checkReconnectCadence(t, lines, "60")
			streams := r.stderr.attempts(t, "/logs")
			failed := failedOf(streams)
			if c.code != ExitOK {
				id := strings.TrimSuffix(strings.TrimPrefix(streams[0].path, "/v1/operations/"), "/logs")
				if !strings.Contains(r.stderr.String(), id) || !strings.Contains(r.stderr.String(), "nexus operation get "+id+" --host alpha") {
					t.Errorf("stderr does not name operation %s", id)
				}
				if last := failed[len(failed)-1].at; last.After(cutAt.Add(61 * time.Second)) {
					t.Errorf("last stream attempt at virtual +%s after the cut, want none past the window plus a gap", last.Sub(cutAt))
				}
				return
			}
			var got []string
			for _, l := range r.stderr.stamped() {
				if digits.MatchString(l.text) {
					got = append(got, l.text)
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("followed lines %v, want 1 to 40 once each", got)
			}
			if len(failed) != 20 {
				t.Errorf("%d failed stream attempts, want 20, one per virtual second", len(failed))
			}
			reached := re.rec.matching(http.MethodGet, func(p string) bool { return strings.HasSuffix(p, "/logs") })
			if len(reached) != 2 {
				t.Fatalf("%d log streams reached the server, want 2: the cut one and one after the outage", len(reached))
			}
			re.fp.mu.Lock()
			last := re.fp.lastSent
			re.fp.mu.Unlock()
			q, _ := url.ParseQuery(reached[1].query)
			if q.Get("offset") != strconv.FormatInt(last, 10) {
				t.Errorf("stream after the outage %q, want offset %d, the last frame forwarded", reached[1].query, last)
			}
		})
	}
}

// TC-SM-39 (FR-14): a 503 counts as retryable, and a success resets the
// window, so two outages of 40 s each never reach 60 s.
func TestTCSM39_503CountsAsRetryableAndA(t *testing.T) {
	fc := useFakeClock(t)
	re := startReconn(t, true, false)
	tt := pollT(fc)
	unavailable := problem.New(problem.AuditUnavailable, "the audit log is unavailable")
	re.fp.window(fc, tt.Add(2*time.Second), tt.Add(42*time.Second), downProblem, unavailable)
	re.fp.window(fc, tt.Add(44*time.Second), tt.Add(84*time.Second), downRefuse, nil)
	fc.at(tt.Add(90*time.Second), re.g.open)

	r := re.run(fc.Now, "wgadget", "start", "gated", "--verbose", "-o", "table")
	t.Logf("exit %d after %s\nstderr:\n%s", r.code, r.wall, r.stderr)
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: no continuous outage reached 60s", r.code)
	}
	var starts []stampedLine
	for _, l := range r.stderr.reconnectLines() {
		if strings.Contains(l.text, "retrying (0s of 60s)") {
			starts = append(starts, l)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("%d reconnect sequences, want 2: %v", len(starts), r.stderr.reconnectLines())
	}
	if !starts[0].at.Equal(tt.Add(2*time.Second)) || !starts[1].at.Equal(tt.Add(44*time.Second)) {
		t.Errorf("sequences start at virtual t+%s and t+%s, want t+2s and t+44s", starts[0].at.Sub(tt), starts[1].at.Sub(tt))
	}
	id := r.stderr.opID(t)
	var unavailablePolls int
	for _, a := range r.stderr.attempts(t, "/v1/operations/"+id) {
		if strings.HasPrefix(a.status, "< 503") {
			unavailablePolls++
		}
	}
	if unavailablePolls != 40 {
		t.Errorf("%d polls answered 503, want 40", unavailablePolls)
	}
}

// TC-SM-40 (FR-14): with the real clock, a 3 s window rides out a 1 s
// outage and gives up on a longer one after 3 s, and the default sleep
// returns as soon as its context ends.
func TestTCSM40_TheRealTimeShortWindowExercises(t *testing.T) {
	t.Run("1s outage", func(t *testing.T) {
		re := startReconn(t, true, false)
		re.fp.onPost(func() {
			re.fp.set(downRefuse, nil)
			up := time.AfterFunc(time.Second, func() { re.fp.set(upPass, nil) })
			open := time.AfterFunc(1500*time.Millisecond, re.g.open)
			t.Cleanup(func() { up.Stop(); open.Stop() })
		})
		r := re.run(time.Now, "wgadget", "restart", "gated", "-o", "table")
		t.Logf("exit %d after %s\nstderr:\n%s", r.code, r.wall, r.stderr)
		if r.code != 0 {
			t.Fatalf("exit %d, want 0", r.code)
		}
		if l := r.stderr.reconnectLines(); len(l) == 0 || !strings.Contains(l[0].text, "retrying (0s of 3s)") {
			t.Errorf("reconnect progress %v, want 0s of 3s", l)
		}
	})
	t.Run("4s outage", func(t *testing.T) {
		re := startReconn(t, true, false)
		re.fp.onPost(func() {
			re.fp.set(downRefuse, nil)
			up := time.AfterFunc(4*time.Second, func() { re.fp.set(upPass, nil) })
			t.Cleanup(func() { up.Stop() })
		})
		r := re.run(time.Now, "wgadget", "restart", "gated", "-o", "table")
		end := time.Now()
		t.Logf("exit %d after %s\nstderr:\n%s", r.code, r.wall, r.stderr)
		if r.code != ExitConnection {
			t.Fatalf("exit %d, want 5", r.code)
		}
		l := r.stderr.reconnectLines()
		if len(l) == 0 {
			t.Fatal("no reconnect progress")
		}
		if d := end.Sub(l[0].at); d < 3*time.Second || d > 4500*time.Millisecond {
			t.Errorf("gave up %s after the first failure, want 3s to 4.5s", d)
		}
	})
	t.Run("default sleep ends with its context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()
		err := waitClock.sleep(ctx, 10*time.Second)
		if el := time.Since(start); !errors.Is(err, context.Canceled) || el > 200*time.Millisecond {
			t.Errorf("sleep returned %v after %s, want context.Canceled within 200ms", err, el)
		}
	})
}

// TC-SM-E13 (FR-14): in a machine format (jsonl: one JSON object per line on
// stdout) each reconnect progress line is a
// JSON object with the host, the elapsed seconds and the window.
func TestTCSME13_InMachineFormatsReconnectProgressIs(t *testing.T) {
	fc := useFakeClock(t)
	re := startReconn(t, true, false)
	tt := pollT(fc)
	re.fp.window(fc, tt.Add(2*time.Second), tt.Add(22*time.Second), downRefuse, nil)
	fc.at(tt.Add(25*time.Second), re.g.open)

	r := re.run(fc.Now, "wgadget", "start", "gated", "-o", "jsonl")
	t.Logf("exit %d\nstdout: %sstderr:\n%s", r.code, r.stdout, r.stderr)
	if r.code != 0 {
		t.Fatalf("exit %d, want 0", r.code)
	}
	if op := onlyOp(t, r.stdout); op.Status != "Succeeded" {
		t.Errorf("stdout operation %s, want Succeeded", op.Status)
	}
	// jsonLines fails on any stderr line that is not a JSON object.
	var reconnect []map[string]any
	for _, m := range jsonLines(t, "stderr", r.stderr.String()) {
		if m["event"] == "reconnecting" {
			reconnect = append(reconnect, m)
		}
	}
	if len(reconnect) != 2 {
		t.Fatalf("%d reconnect progress lines, want 2 (0s and 10s)", len(reconnect))
	}
	for i, m := range reconnect {
		if m["level"] != "progress" || m["host"] != "alpha" || m["windowSeconds"] != float64(60) ||
			m["elapsedSeconds"] != float64(10*i) {
			t.Errorf("reconnect line %v, want progress for alpha at %ds of a 60s window", m, 10*i)
		}
	}
}

// TC-SM-ERR20 (FR-14, NSM FR-35): a certificate pin mismatch inside the
// window fails at once, on the poll and on the stream.
func TestTCSMERR20_ACertificatePinMismatchIsNever(t *testing.T) {
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait", true: "follow"}[follow], func(t *testing.T) {
			fc := useFakeClock(t)
			start := fc.Now()
			re := startReconn(t, true, true)
			re.fp.onPost(func() { re.fp.set(downPin, nil) })
			args := []string{"wgadget", "start", "gated", "--verbose", "-o", "table"}
			if follow {
				args = append(args, "--follow")
			}
			r := re.run(fc.Now, args...)
			t.Logf("exit %d\nstderr:\n%s", r.code, r.stderr)
			if r.code != ExitConnection {
				t.Fatalf("exit %d, want 5", r.code)
			}
			if el := fc.Now().Sub(start); el >= time.Second {
				t.Errorf("virtual %s elapsed, want under 1s", el)
			}
			failed := failedOf(r.stderr.attempts(t, ""))
			if len(failed) != 1 {
				t.Errorf("%d failed requests, want 1", len(failed))
			}
			if s := r.stderr.String(); !strings.Contains(s, "fingerprint mismatch") || strings.Contains(s, "likely restarting") {
				t.Errorf("stderr %q, want the fingerprint mismatch and no reconnect", s)
			}
		})
	}
}

// windowlessGolden is the stderr of a windowless op whose host refuses
// connections after the POST, captured by this test from the base wait.go
// (2953509, unchanged to 2482f06) and normalized by normalizeStderr.
var windowlessGolden = map[string]string{
	"wait": "> POST http://127.0.0.1:<port>/v1/providers/gate/gadgets/gated/start\n" +
		"< 202 Accepted (<d>)\n" +
		"> GET http://127.0.0.1:<port>/v1/operations/<id>\n" +
		"< GET http://127.0.0.1:<port>/v1/operations/<id> failed after <d>\n" +
		"nexus: host alpha: operation <id>: connection failed: Get \"http://127.0.0.1:<port>/v1/operations/<id>\": " +
		"dial tcp 127.0.0.1:<port>: connect: connection refused\n",
	"follow": "> POST http://127.0.0.1:<port>/v1/providers/gate/gadgets/gated/start\n" +
		"< 202 Accepted (<d>)\n" +
		strings.Repeat("> GET http://127.0.0.1:<port>/v1/operations/<id>/logs?follow=true\n"+
			"< GET http://127.0.0.1:<port>/v1/operations/<id>/logs?follow=true failed after <d>\n", followRetries+1) +
		"nexus: host alpha: operation <id>: connection failed: Get \"http://127.0.0.1:<port>/v1/operations/<id>/logs?follow=true\": " +
		"dial tcp 127.0.0.1:<port>: connect: connection refused\n",
}

var (
	verboseDuration = regexp.MustCompile(`(failed after |\()[0-9.]+[µnm]?s\)?`)
	loopbackPort    = regexp.MustCompile(`127\.0\.0\.1:\d+`)
)

// normalizeStderr replaces the operation ID, ports and timings.
func normalizeStderr(s, id string) string {
	s = strings.ReplaceAll(s, id, "<id>")
	s = loopbackPort.ReplaceAllString(s, "127.0.0.1:<port>")
	return verboseDuration.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "(") {
			return "(<d>)"
		}
		return "failed after <d>"
	})
}

// TC-SM-ERR21 (FR-14, CON-4): an op without a window keeps today's
// behaviour: the plain wait fails on its first poll error, and --follow
// gives up after followRetries failures 1 s apart.
func TestTCSMERR21_AWindowlessOpKeepsTodayS(t *testing.T) {
	for _, follow := range []bool{false, true} {
		name := map[bool]string{false: "wait", true: "follow"}[follow]
		t.Run(name, func(t *testing.T) {
			fc := useFakeClock(t)
			re := startReconn(t, false, false)
			re.fp.onPost(func() { re.fp.set(downRefuse, nil) })
			args := []string{"gadget", "start", "gated", "--verbose", "-o", "table"}
			if follow {
				args = append(args, "--follow")
			}
			r := re.run(fc.Now, args...)
			t.Logf("exit %d\nstderr:\n%s", r.code, r.stderr)
			if r.code != ExitConnection {
				t.Fatalf("exit %d, want 5", r.code)
			}
			if strings.Contains(r.stderr.String(), "likely restarting") {
				t.Error("a windowless op printed reconnect progress")
			}
			all := r.stderr.attempts(t, "")
			var id string
			if follow {
				logs := r.stderr.attempts(t, "/logs")
				if len(logs) != followRetries+1 || len(failedOf(logs)) != len(logs) || len(all) != len(logs) {
					t.Fatalf("%d stream attempts (%d failed) of %d requests, want %d failed and nothing else",
						len(logs), len(failedOf(logs)), len(all), followRetries+1)
				}
				for i := 1; i < len(logs); i++ {
					if gap := logs[i].at.Sub(logs[i-1].at); gap != followRetryGap {
						t.Errorf("stream attempts %d and %d are %s apart, want %s", i-1, i, gap, followRetryGap)
					}
				}
				id = strings.TrimSuffix(strings.TrimPrefix(logs[0].path, "/v1/operations/"), "/logs")
			} else {
				if len(all) != 1 || !all[0].failed {
					t.Fatalf("%d requests after the POST, want exactly 1 failed poll", len(all))
				}
				id = r.stderr.opID(t)
			}
			if got := normalizeStderr(r.stderr.String(), id); got != windowlessGolden[name] {
				t.Errorf("stderr differs from base:\n got: %q\nwant: %q", got, windowlessGolden[name])
			}
		})
	}
}

// TC-SM-ERR22 (FR-14): errors that are not a lost connection or a 503 are
// not retried inside the window and keep their exit codes.
func TestTCSMERR22_NonRetryableErrorsInsideAWindow(t *testing.T) {
	for _, c := range []struct {
		prob *problem.Error
		code int
	}{
		{problem.New(problem.Unauthorized, "the token was revoked"), ExitAuth},
		{problem.New(problem.NotFound, "no such operation"), ExitNotFound},
		{problem.New(problem.InternalError, "the server failed"), ExitFailure},
	} {
		t.Run(strconv.Itoa(c.prob.Status), func(t *testing.T) {
			fc := useFakeClock(t)
			re := startReconn(t, true, false)
			re.fp.onPost(func() { re.fp.set(downProblem, c.prob) })
			r := re.run(fc.Now, "wgadget", "start", "gated", "--verbose", "-o", "table")
			t.Logf("exit %d\nstderr:\n%s", r.code, r.stderr)
			if r.code != c.code {
				t.Fatalf("exit %d, want %d", r.code, c.code)
			}
			if polls := r.stderr.attempts(t, "/v1/operations/"+r.stderr.opID(t)); len(polls) != 1 {
				t.Errorf("%d polls, want 1", len(polls))
			}
			if strings.Contains(r.stderr.String(), "likely restarting") {
				t.Error("a non-retryable error printed reconnect progress")
			}
		})
	}
}

// retryable is a lost connection or a 503, never a pin mismatch, which
// reaches errors.As through the transport error (A-34).
func TestReconnectRetryable(t *testing.T) {
	pin := &client.PinMismatchError{Want: "AA", Got: "BB"}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"refused", &client.TransportError{Err: errors.New("connect: connection refused")}, true},
		{"pin", &client.TransportError{Err: &url.Error{Op: "Get", URL: "https://h", Err: pin}}, false},
		{"bare pin", pin, false},
		{"503 problem", problem.New(problem.AuditUnavailable, ""), true},
		{"503 status", &client.StatusError{Status: http.StatusServiceUnavailable}, true},
		{"502 status", &client.StatusError{Status: http.StatusBadGateway}, false},
		{"500 problem", problem.New(problem.InternalError, ""), false},
		{"401 problem", problem.New(problem.Unauthorized, ""), false},
		{"not JSON", errNotJSON, false},
	} {
		if got := retryable(c.err); got != c.want {
			t.Errorf("%s: retryable %v, want %v", c.name, got, c.want)
		}
	}
}
