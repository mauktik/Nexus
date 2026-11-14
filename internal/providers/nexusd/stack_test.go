package nexusd

// The nexusd T0 stack (test-strategy.md): the real server, Dispatcher, ops
// manager, audit log and store around nexusd.New(Options{...}). The only
// seams are the host boundary: Systemd, InService, Executable, Runner, a
// temp-root Paths, the fake provider.Daemon (F-DAEMON), the version-probe
// timer (F-CLK) and a temp state directory. It binds what the provider binds
// so far (A-49); later work items' tests reuse it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// CheckConfigForTest is daemon.CheckConfig. provider_test.go, in the
// external test package, sets it in init. This package's own test files
// cannot import internal/daemon: once nexusd is a built-in provider (WI
// 6.1), daemon imports this package, and an internal test importing daemon
// is an import cycle.
var CheckConfigForTest func(path string) (*config.Config, config.ErrorList)

// checkConfig runs the real daemon.CheckConfig on path.
func checkConfig(t *testing.T, path string) (*config.Config, config.ErrorList) {
	t.Helper()
	if CheckConfigForTest == nil {
		t.Fatal("CheckConfigForTest is not set: provider_test.go's init did not run")
	}
	return CheckConfigForTest(path)
}

// fixture returns the absolute path of testdata/config/<name>.
func fixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "config", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeDaemon is F-DAEMON: fixed ConfigPath, StateDir and StartedAt, the real
// daemon.CheckConfig on ConfigPath, and a scripted Reload. It records every
// call in order and never fabricates config errors.
type fakeDaemon struct {
	stateDir  string
	startedAt time.Time

	mu         sync.Mutex
	configPath string
	reloadFn   func(context.Context) (provider.ReloadResult, error)
	reloads    int
	calls      []string
}

func newFakeDaemon(configPath, stateDir string, startedAt time.Time) *fakeDaemon {
	return &fakeDaemon{configPath: configPath, stateDir: stateDir, startedAt: startedAt}
}

func (d *fakeDaemon) record(call string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, call)
}

func (d *fakeDaemon) ConfigPath() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.configPath
}

func (d *fakeDaemon) StateDir() string     { return d.stateDir }
func (d *fakeDaemon) StartedAt() time.Time { return d.startedAt }

func (d *fakeDaemon) CheckConfig() provider.ConfigCheck {
	d.record("CheckConfig")
	if CheckConfigForTest == nil {
		panic("CheckConfigForTest is not set: provider_test.go's init did not run")
	}
	cfg, errs := CheckConfigForTest(d.ConfigPath())
	return provider.ConfigCheck{Config: cfg, Errors: errs}
}

func (d *fakeDaemon) Reload(ctx context.Context) (provider.ReloadResult, error) {
	d.mu.Lock()
	d.calls = append(d.calls, "Reload")
	d.reloads++
	fn := d.reloadFn
	d.mu.Unlock()
	if fn == nil {
		return provider.ReloadResult{}, nil
	}
	return fn(ctx)
}

// setConfigPath points the daemon at another file.
func (d *fakeDaemon) setConfigPath(p string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.configPath = p
}

// onReload scripts Reload: return a result, a *provider.ReloadError, or
// block until the test releases it (to hold the lane).
func (d *fakeDaemon) onReload(fn func(context.Context) (provider.ReloadResult, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reloadFn = fn
}

func (d *fakeDaemon) reloadCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reloads
}

var _ provider.Daemon = (*fakeDaemon)(nil)

// fakeRunner is F-RUN: it counts the runs of each program and answers with
// a scripted function, used only for exec counting and timeouts.
type fakeRunner struct {
	mu    sync.Mutex
	runs  map[string]int
	reply func(ctx context.Context, argv []string) (RunResult, error)
}

func newFakeRunner(reply func(ctx context.Context, argv []string) (RunResult, error)) *fakeRunner {
	return &fakeRunner{runs: map[string]int{}, reply: reply}
}

func (r *fakeRunner) Run(ctx context.Context, argv []string) (RunResult, error) {
	r.mu.Lock()
	r.runs[argv[0]]++
	reply := r.reply
	r.mu.Unlock()
	return reply(ctx, argv)
}

func (r *fakeRunner) setReply(reply func(ctx context.Context, argv []string) (RunResult, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reply = reply
}

// execs is how many times the program at path ran.
func (r *fakeRunner) execs(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[path]
}

// fakeClock is F-CLK for the provider's timers: AfterFunc registers f to run
// on the Advance that crosses its deadline. Advance with no timer pending
// fails the test ("advance with no waiter"), so a test cannot advance before
// the code under test has armed its timer.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, tm)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if tm.fired || tm.stopped {
			return false
		}
		tm.stopped = true
		return true
	}
}

// pending is how many armed timers have neither fired nor been stopped.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tm := range c.timers {
		if !tm.fired && !tm.stopped {
			n++
		}
	}
	return n
}

// Advance moves virtual time by d and runs, in deadline order and outside
// the clock's lock, every timer it crosses.
func (c *fakeClock) Advance(t *testing.T, d time.Duration) {
	t.Helper()
	c.mu.Lock()
	var due []*fakeTimer
	waiting := false
	c.now = c.now.Add(d)
	for _, tm := range c.timers {
		if tm.fired || tm.stopped {
			continue
		}
		waiting = true
		if !tm.at.After(c.now) {
			tm.fired = true
			due = append(due, tm)
		}
	}
	c.mu.Unlock()
	if !waiting {
		t.Fatal("fakeClock: advance with no waiter")
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, tm := range due {
		tm.f()
	}
}

// stackConfig configures newStack. Zero fields take the stack defaults:
// systemd available, running as nexusd.service, executable /x/nexusd, the
// real Runner, real time, and a daemon started a minute ago on
// testdata/config/valid.yaml.
type stackConfig struct {
	opts   Options
	daemon *fakeDaemon
	// section is a config file whose providers.nexusd section Configure
	// decodes at startup; empty means no section (the defaults).
	section string
	clock   *fakeClock // Drives the provider's timers; nil means real time
	// disabled registers nexusd with Enabled false (TC-SM-E16).
	disabled bool
	// disabledExtra are further providers registered disabled, to compare
	// refusals against (TC-SM-E16).
	disabledExtra []provider.Provider
	// failRequested, when positive, gives the stack a second Dispatcher,
	// stack.disp, over the same registry, whose audit writer fails the
	// next failRequested requested records and then delegates every
	// record to the real audit log (TC-SM-ERR38 run C). The REST server
	// builds its own Dispatcher over the registry's log, so a test drives
	// this one through stack.invoke.
	failRequested int
	// launch, when set, gives the ops manager a detach launcher whose
	// Launch is this spy and whose units always report active, so a
	// launched operation stays Running until the test writes its
	// exit.json; the provider's ops directory is then the manager's
	// (WI 5.3).
	launch func(ctx context.Context, inv detach.Invocation) error
	// extra are further providers registered enabled (TC-SM-86).
	extra []provider.Provider
}

// stack is a running nexusd T0 stack. reader holds nexusd:read, admin holds
// nexusd:admin and nexusd:read, and nobody holds no scope.
type stack struct {
	url                   string
	reader, admin, nobody string

	p      *Provider
	reg    *core.Registry
	alog   *audit.Log
	daemon *fakeDaemon
	paths  Paths
	http   *http.Client

	stateDir, auditDir string
	opsDir             string           // The ops manager's directory
	logs               *syncBuffer      // The server's and registry's log
	disp               *core.Dispatcher // Set by stackConfig.failRequested
	failing            *failingAudit    // disp's audit writer
	mint               func(name string, scopes ...string) string
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// failingAudit is the stack's audit-failure option: it fails the next n
// requested writes, then delegates every write to the real log.
type failingAudit struct {
	real *audit.Log

	mu     sync.Mutex
	n      int
	failed int
}

var errInjectedAudit = errors.New("injected requested-write failure")

func (a *failingAudit) Write(rec systemspec.AuditRecord, sensitive []string) (string, error) {
	if rec.Outcome == audit.OutcomeRequested {
		a.mu.Lock()
		fail := a.n > 0
		if fail {
			a.n--
			a.failed++
		}
		a.mu.Unlock()
		if fail {
			return "", errInjectedAudit
		}
	}
	return a.real.Write(rec, sensitive)
}

func (a *failingAudit) Redact(provider, action string, params json.RawMessage, sensitive []string) (json.RawMessage, error) {
	return a.real.Redact(provider, action, params, sensitive)
}

// failures is how many requested writes failed so far.
func (a *failingAudit) failures() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failed
}

// StackHost is the host name the stack reports.
const StackHost = "t0-host"

// AvailableSystemd is the Systemd seam of a host with systemd.
func AvailableSystemd() detach.Availability { return detach.Availability{Available: true} }

func newStack(t *testing.T, cfg stackConfig) *stack {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	root, stateDir, auditDir := t.TempDir(), t.TempDir(), t.TempDir()

	opts := cfg.opts
	if opts.Host == "" {
		opts.Host = StackHost
	}
	if opts.Paths == (Paths{}) {
		opts.Paths = Paths{
			Binary:   filepath.Join(root, "nexusd"),
			Prev:     filepath.Join(root, "nexusd.prev"),
			UnitFile: filepath.Join(root, "nexusd.service"),
			Unit:     "nexusd.service",
		}
	}
	if opts.Systemd == nil {
		opts.Systemd = AvailableSystemd
	}
	if opts.InService == nil {
		opts.InService = func() error { return nil }
	}
	if opts.Executable == nil {
		opts.Executable = func() (string, error) { return "/x/nexusd", nil }
	}
	d := cfg.daemon
	if d == nil {
		d = newFakeDaemon(fixture(t, "valid.yaml"), stateDir, time.Now().Add(-time.Minute))
	}

	p := New(opts)
	if cfg.clock != nil {
		p.afterFunc = cfg.clock.AfterFunc
	}
	p.UseDaemon(d)

	alog, err := audit.Open(audit.Options{Dir: auditDir, Host: opts.Host, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	opsDir := filepath.Join(stateDir, "ops")
	var launcher *detach.Launcher
	if cfg.launch != nil {
		launcher, err = detach.New(detach.Config{OpsDir: opsDir, Executable: "/x/nexusd", Logger: log}, detach.Options{
			Launch: cfg.launch,
			UnitState: func(context.Context, string) (detach.UnitState, error) {
				return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
			},
			Signal:    func(context.Context, string, syscall.Signal) error { return nil },
			ListUnits: func(context.Context) ([]string, error) { return nil, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		p.UseOpsDir(opsDir)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: opsDir, Host: opts.Host, Logger: log,
		Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	sections := sectionOf(t, cfg.section)
	if cfg.disabled {
		sections[spec.Name] = core.ProviderConfig{}
	}
	provs := []provider.Provider{p}
	for _, x := range cfg.extra {
		provs = append(provs, x)
		sections[x.Spec().Name] = core.ProviderConfig{Enabled: true}
	}
	for _, x := range cfg.disabledExtra {
		provs = append(provs, x)
		sections[x.Spec().Name] = core.ProviderConfig{}
	}
	reg, err := core.New(provs, core.Options{Host: opts.Host, Version: "v0.0.0-test", Logger: log,
		Audit: alog, Ops: mgr, Providers: sections})
	if err != nil {
		t.Fatal(err)
	}
	token := func(name string, scopes ...string) string {
		tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: name, Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	s := &stack{
		reader: token("reader", spec.ReadScope),
		admin:  token("admin", spec.AdminScope, spec.ReadScope),
		nobody: token("nobody"),
		p:      p, reg: reg, alog: alog, daemon: d, paths: opts.Paths,
		http:     &http.Client{Timeout: 30 * time.Second},
		stateDir: d.StateDir(), auditDir: auditDir, opsDir: opsDir, logs: logs, mint: token,
	}
	if cfg.failRequested > 0 {
		s.failing = &failingAudit{real: alog, n: cfg.failRequested}
		s.disp = core.NewDispatcher(reg, core.DispatcherOptions{Logger: log, Audit: s.failing})
	}
	srv, err := server.New(reg, server.Options{Logger: log,
		Auth: auth.New(stateDir, auth.Options{Logger: log, Audit: alog, FailureLimitPerMinute: 1000})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.url = "http://" + ln.Addr().String()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("ops shutdown: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
	})
	return s
}

// sectionOf returns the provider configuration for the nexusd section of
// the config file at path, enabled; an empty path means no section.
func sectionOf(t *testing.T, path string) map[string]core.ProviderConfig {
	t.Helper()
	if path == "" {
		return map[string]core.ProviderConfig{spec.Name: {Enabled: true}}
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: cfg.Providers.Nexusd.Decode}}
}

// apply re-applies the provider sections from the config file at path, the
// way a daemon reload does (Registry.Apply calls Configure).
func (s *stack) apply(t *testing.T, path string) {
	t.Helper()
	if err := s.reg.Apply(t.Context(), sectionOf(t, path)); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}
}

// response is one HTTP exchange's outcome.
type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends method path (under /v1) with token and an optional JSON body.
func (s *stack) do(t *testing.T, method, path, token, body string, header http.Header) response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, s.url+"/v1"+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

// want fails unless r has status.
func (r response) want(t *testing.T, status int) response {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	return r
}

// decodeJSON decodes b into a T.
func decodeJSON[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %T: %v: %s", v, err, b)
	}
	return v
}

// getDaemon GETs the daemons list and returns its one item.
func (s *stack) getDaemon(t *testing.T, token string) spec.Daemon {
	t.Helper()
	l := decodeJSON[apitypes.List[spec.Daemon]](t, s.do(t, http.MethodGet, "/providers/nexusd/daemons", token, "",
		nil).want(t, http.StatusOK).body)
	if len(l.Items) != 1 {
		t.Fatalf("items %d, want 1: %+v", len(l.Items), l)
	}
	return l.Items[0]
}

// action POSTs /providers/nexusd/actions/<name>.
func (s *stack) action(t *testing.T, name, token string, header http.Header) response {
	t.Helper()
	return s.do(t, http.MethodPost, "/providers/nexusd/actions/"+name, token, "", header)
}

// awaitOp polls the operation until it is terminal.
func (s *stack) awaitOp(t *testing.T, id string) apitypes.Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		op := decodeJSON[apitypes.Operation](t, s.do(t, http.MethodGet, "/operations/"+id, s.reader, "", nil).
			want(t, http.StatusOK).body)
		if op.Status.Terminal() {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s still %s after 10s", id, op.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// operations lists the provider's operations over REST.
func (s *stack) operations(t *testing.T) []apitypes.Operation {
	t.Helper()
	all := decodeJSON[apitypes.List[apitypes.Operation]](t, s.do(t, http.MethodGet, "/operations?limit=1000",
		s.admin, "", nil).want(t, http.StatusOK).body).Items
	var out []apitypes.Operation
	for _, op := range all {
		if op.Provider == spec.Name {
			out = append(out, op)
		}
	}
	return out
}

// auditOf returns the audit records of action, oldest first.
func (s *stack) auditOf(t *testing.T, action string) []systemspec.AuditRecord {
	t.Helper()
	l, err := s.alog.Query(audit.Query{Provider: spec.Name, Action: action, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	recs := l.Items
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs
}

// outcomes lists the outcome of each record, with the problem slug after a
// colon when there is one.
func outcomes(recs []systemspec.AuditRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		o := r.Outcome
		if r.Problem != "" {
			o += ":" + r.Problem
		}
		out = append(out, o)
	}
	return out
}

// writeScript writes an executable shell stand-in at path.
func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stdoutReply is a fakeRunner reply that prints out and exits 0.
func stdoutReply(out string) func(context.Context, []string) (RunResult, error) {
	return func(context.Context, []string) (RunResult, error) {
		return RunResult{Stdout: []byte(out)}, nil
	}
}

// problemOf decodes a problem body.
func problemOf(t *testing.T, b []byte) map[string]any {
	t.Helper()
	m := decodeJSON[map[string]any](t, b)
	if _, ok := m["type"]; !ok {
		t.Fatalf("not a problem: %s", b)
	}
	return m
}

// slugOf is the slug of a problem's type URI.
func slugOf(m map[string]any) string {
	typ, _ := m["type"].(string)
	if slug, ok := strings.CutPrefix(typ, problem.TypePrefix); ok {
		return slug
	}
	return fmt.Sprintf("<%s>", typ)
}
