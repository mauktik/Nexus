package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
)

// helperEnv makes the test binary act as a nexusd serve process whose
// providers are the test providers below. It is a real subprocess with real
// signals, real listeners and real stdout.
const (
	helperEnv       = "NEXUS_DAEMON_TEST_HELPER"
	helperConfigEnv = "NEXUS_DAEMON_TEST_CONFIG"
	helperStateEnv  = "NEXUS_DAEMON_TEST_STATE"
)

// testStateDir holds the token every daemon under test accepts, testToken.
var testStateDir, testToken string

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(helperMain(os.Getenv(helperConfigEnv)))
	}
	dir, err := os.MkdirTemp("", "nexus-daemon-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testStateDir = dir
	// The read scopes of the probe providers: the Dispatcher (2.2) enforces
	// them on every provider route.
	testToken, _, err = auth.Create(dir, auth.CreateRequest{Name: "daemon-tests",
		Scopes: []string{"apt:read", "docker:read", "scripts:read"}})
	if err == nil {
		// Helper subprocesses inherit the environment.
		err = os.Setenv(helperStateEnv, dir)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func helperMain(path string) int {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	d, err := New(cfg, Options{Providers: helperProviders(), ConfigPath: path, StateDir: os.Getenv(helperStateEnv)})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type probeIn struct {
	Name string `json:"name"`
}

type probeOut struct {
	Name string `json:"name"`
}

func probeOp(prov string) def.Op {
	return def.Op{
		ID: prov + ".probes.get", Provider: prov, Kind: def.KindGet, ResourceType: "probes",
		Summary: "s", Description: "Probe operation for the daemon tests.",
		Scope: def.Scope{Name: prov + ":read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "probe", Verb: "get", Positional: "name"},
	}
}

var probeOps = map[string]def.Typed[probeIn, probeOut]{
	"apt":     def.Define[probeIn, probeOut](probeOp("apt")),
	"docker":  def.Define[probeIn, probeOut](probeOp("docker")),
	"scripts": def.Define[probeIn, probeOut](probeOp("scripts")),
}

// probeProvider takes the name of a real config section, so the real config
// file enables it. It counts preflight and handler calls.
type probeProvider struct {
	name       string
	preflight  func() provider.Status
	preflights atomic.Int64
	handled    atomic.Int64
}

func (p *probeProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: p.name, ReadScope: p.name + ":read", Ops: []def.Op{probeOps[p.name].Op()}}
}

// Configure decodes into a type that knows only `enabled`, so the test
// config files hold only that key per section.
func (p *probeProvider) Configure(decode func(any) error) error {
	var c struct {
		Enabled bool `yaml:"enabled"`
	}
	return decode(&c)
}

func (p *probeProvider) Preflight(context.Context) provider.Status {
	p.preflights.Add(1)
	return p.preflight()
}

func (p *probeProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Sync(probeOps[p.name], func(_ context.Context, in probeIn) (probeOut, error) {
		p.handled.Add(1)
		return probeOut(in), nil
	})}
}

func ok() provider.Status { return provider.Status{State: apitypes.ProviderEnabled} }

// helperProviders: apt passes preflight, docker fails it, scripts panics.
func helperProviders() []provider.Provider {
	return []provider.Provider{
		&probeProvider{name: "apt", preflight: ok},
		&probeProvider{name: "docker", preflight: func() provider.Status {
			return provider.Status{State: apitypes.ProviderUnavailable, Reason: "docker socket not found"}
		}},
		&probeProvider{name: "scripts", preflight: func() provider.Status { panic("scripts preflight exploded") }},
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func writeConfig(t *testing.T, path, listen string, enabled ...string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "host: {name: daemon-test}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n", listen)
	for _, name := range enabled {
		fmt.Fprintf(&b, "  %s: {enabled: true}\n", name)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// helperProc is a running helper subprocess with its stdout collected.
type helperProc struct {
	cmd   *exec.Cmd
	base  string
	mu    sync.Mutex
	lines []string
	done  chan error
}

func startHelper(t *testing.T, cfgPath, listen string) *helperProc {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"=1", helperConfigEnv+"="+cfgPath)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helperProc{cmd: cmd, base: "http://" + listen, done: make(chan error, 1)}
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			h.mu.Lock()
			h.lines = append(h.lines, sc.Text())
			h.mu.Unlock()
		}
	}()
	go func() {
		<-scanned
		h.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-h.done
		}
	})
	h.waitHealthy(t)
	return h
}

func (h *helperProc) waitHealthy(t *testing.T) {
	t.Helper()
	c := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.Get(h.base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-h.done:
			h.done <- err
			t.Fatalf("helper exited before becoming healthy: %v\n%s", err, h.output())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("helper not healthy within 15s\n%s", h.output())
}

func (h *helperProc) output() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.lines, "\n")
}

// waitLog waits for a JSON log line with the level whose msg matches.
func (h *helperProc) waitLog(t *testing.T, level string, msg *regexp.Regexp, after int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		lines := h.lines
		h.mu.Unlock()
		for i := after; i < len(lines); i++ {
			var rec map[string]any
			if json.Unmarshal([]byte(lines[i]), &rec) != nil {
				continue
			}
			if rec["level"] == level && msg.MatchString(fmt.Sprint(rec["msg"])) {
				return i + 1
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s log matching %v\n%s", level, msg, h.output())
	return 0
}

func (h *helperProc) lineCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.lines)
}

func (h *helperProc) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := h.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

func (h *helperProc) stop(t *testing.T) {
	t.Helper()
	h.signal(t, syscall.SIGTERM)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("helper exit: %v\n%s", err, h.output())
		}
	case <-time.After(11 * time.Second):
		t.Fatal("helper did not exit within 11s of SIGTERM")
	}
}

// authed is a GET request carrying testToken.
func authed(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

func capabilities(t *testing.T, base string) map[string]apitypes.ProviderCapability {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(authed(t, base+"/v1/capabilities"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities: %d %s", resp.StatusCode, body)
	}
	var caps apitypes.Capabilities
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatal(err)
	}
	m := map[string]apitypes.ProviderCapability{}
	for _, p := range caps.Providers {
		m[p.Name] = p
	}
	return m
}

// TC-21: SIGHUP applies valid config and keeps it on failure (second-state).
func TestSIGHUPReload(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	writeConfig(t, cfgPath, listen)
	h := startHelper(t, cfgPath, listen)

	// Step 1.
	if s := capabilities(t, h.base)["apt"].Status; s != apitypes.ProviderDisabled {
		t.Fatalf("step 1 apt = %q, want disabled", s)
	}

	// Step 2.
	n := h.lineCount()
	writeConfig(t, cfgPath, listen, "apt")
	h.signal(t, syscall.SIGHUP)
	n = h.waitLog(t, "INFO", regexp.MustCompile(`^configuration reloaded`), n)
	if s := capabilities(t, h.base)["apt"].Status; s != apitypes.ProviderEnabled {
		t.Fatalf("step 2 apt = %q, want enabled", s)
	}

	// Step 3.
	if err := os.WriteFile(cfgPath, []byte("listen: [\"127.0.0.1:1\"]\nbogusKey: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.signal(t, syscall.SIGHUP)
	n = h.waitLog(t, "ERROR", regexp.MustCompile(`reload failed`), n)
	if !strings.Contains(h.output(), "bogusKey") {
		t.Errorf("validation error does not name the bad key:\n%s", h.output())
	}
	if s := capabilities(t, h.base)["apt"].Status; s != apitypes.ProviderEnabled {
		t.Fatalf("step 3 apt = %q, want enabled kept", s)
	}

	// Step 4.
	other := freePort(t)
	writeConfig(t, cfgPath, other, "apt")
	h.signal(t, syscall.SIGHUP)
	h.waitLog(t, "WARN", regexp.MustCompile(`restart`), n)
	h.waitHealthy(t)
	c := &http.Client{Timeout: time.Second}
	if resp, err := c.Get("http://" + other + "/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("new listen address %s is serving without a restart", other)
	}
	h.stop(t)
}

// TC-ERR5: a failed preflight never prevents startup.
func TestFailedPreflightNeverPreventsStartup(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	writeConfig(t, cfgPath, listen, "docker", "scripts")
	h := startHelper(t, cfgPath, listen)

	caps := capabilities(t, h.base)
	for _, name := range []string{"docker", "scripts"} {
		if caps[name].Status != apitypes.ProviderUnavailable || caps[name].Reason == "" {
			t.Errorf("%s = %+v, want unavailable with a reason", name, caps[name])
		}
	}
	if !strings.Contains(caps["docker"].Reason, "docker socket not found") {
		t.Errorf("docker reason = %q", caps["docker"].Reason)
	}
	select {
	case err := <-h.done:
		t.Fatalf("process exited: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	h.waitHealthy(t)
	h.stop(t)
}

// tickerFrame matches stack frames that mean a periodic wakeup.
var tickerFrame = regexp.MustCompile(`time\.(Sleep|NewTicker|Tick|\(\*Ticker\))`)

// pollingGoroutines returns the goroutine stacks that run Nexus code and
// either sleep or own a ticker, or are not one of the expected idle waits.
func pollingGoroutines(dump string, allowed []string) []string {
	var bad []string
	for _, g := range strings.Split(dump, "\n\n") {
		if !strings.Contains(g, "github.com/mauktik/nexus/") {
			continue
		}
		if tickerFrame.MatchString(g) {
			bad = append(bad, g)
			continue
		}
		known := false
		for _, a := range allowed {
			if strings.Contains(g, a) {
				known = true
				break
			}
		}
		if !known {
			bad = append(bad, g)
		}
	}
	return bad
}

func goroutineDump() string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// idleWaits are the only goroutines with Nexus frames allowed at idle: the
// daemon's signal loop, one accept loop per listener, and the test harness
// (TestMain's goroutine and this test).
var idleWaits = []string{
	"internal/daemon.(*Daemon).Run(",
	"internal/server.(*Server).Serve(",
	"internal/daemon.TestMain(",
	"internal/daemon.TestNoPollingAtIdle(",
}

func pollingProbe(stop <-chan struct{}, exited chan<- struct{}) {
	defer close(exited)
	tk := time.NewTicker(10 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			time.Sleep(time.Millisecond)
		}
	}
}

// TC-23: no polling loops at idle.
func TestNoPollingAtIdle(t *testing.T) {
	// Control: the detector flags a known polling goroutine.
	stop, exited := make(chan struct{}), make(chan struct{})
	go pollingProbe(stop, exited)
	deadline := time.Now().Add(2 * time.Second)
	for len(pollingGoroutines(goroutineDump(), []string{"internal/daemon.pollingProbe("})) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("control: the detector never flagged a goroutine that sleeps on a ticker")
		}
	}
	close(stop)
	<-exited

	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	writeConfig(t, cfgPath, listen, "apt")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := &probeProvider{name: "apt", preflight: ok}
	d, err := New(cfg, Options{
		Providers:  []provider.Provider{p},
		ConfigPath: cfgPath,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
		StateDir:   testStateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx) }()
	select {
	case <-d.Ready():
	case err := <-runErr:
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(authed(t, "http://"+listen+"/v1/providers/apt/probes/p1"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	c.CloseIdleConnections()

	pre, handled := p.preflights.Load(), p.handled.Load()
	if pre != 1 || handled != 1 {
		t.Fatalf("after ready: preflights %d, handled %d; want 1 and 1", pre, handled)
	}
	time.Sleep(3 * time.Second)
	if p.preflights.Load() != pre || p.handled.Load() != handled {
		t.Fatalf("idle counters moved: preflights %d->%d, handled %d->%d",
			pre, p.preflights.Load(), handled, p.handled.Load())
	}
	if bad := pollingGoroutines(goroutineDump(), idleWaits); len(bad) > 0 {
		t.Fatalf("unexpected Nexus goroutines at idle:\n%s", strings.Join(bad, "\n\n"))
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(11 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// The notifier writes one datagram per state to NOTIFY_SOCKET and is a no-op
// without it.
func TestNotifierSendsDatagrams(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "n.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := newNotifier("").notify("READY=1"); err != nil {
		t.Fatalf("no socket: %v", err)
	}
	if err := newNotifier(sock).notify("READY=1"); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "READY=1" {
		t.Fatalf("datagram = %q", buf[:n])
	}
	if err := newNotifier("vsock:2:1234").notify("READY=1"); err == nil {
		t.Error("vsock address accepted, want an unsupported error")
	}
}
