// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests for planning/system-provider/test-cases.md TC-HP-1, TC-HP-2,
// TC-HP-3, TC-HP-4, TC-HP-5, TC-HP-6, TC-HP-7, TC-HP-E1, TC-HP-E2,
// TC-HP-E3, TC-HP-ERR1 and TC-HP-ERR2 (WI 1.1): the generic
// provider.Background mechanism, started and stopped on configuration
// enablement at startup, on reload and at shutdown.

package daemon

import (
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
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/provider"
)

// bgMode is how F-BG's RunBackground behaves.
type bgMode int

const (
	bgNormal   bgMode = iota // returns once ctx is done
	bgPanic                  // panics at once
	bgStubborn               // ignores ctx and returns only on releaseAll
)

// fakeBackground is F-BG: a provider named after a real section (the
// sectionProvider pattern, so the real loader and reload path run) that
// implements provider.Background and records every run.
type fakeBackground struct {
	sectionProvider
	mode    bgMode
	release chan struct{}
	once    sync.Once

	mu    sync.Mutex
	runs  []*bgRecord
	logTo *syncBuffer // when set, a normal run logs its return here
	dial  string      // when set, a normal run dials it once ctx is done
}

// bgRecord is one RunBackground call. dialErr is written before returned
// is closed.
type bgRecord struct {
	ctx      context.Context
	returned chan struct{}
	dialed   bool
	dialErr  error
}

var _ provider.Background = (*fakeBackground)(nil)

func newFakeBackground(mode bgMode) *fakeBackground {
	return &fakeBackground{sectionProvider: sectionProvider{name: "apt"}, mode: mode, release: make(chan struct{})}
}

func (f *fakeBackground) RunBackground(ctx context.Context) {
	rec := &bgRecord{ctx: ctx, returned: make(chan struct{})}
	f.mu.Lock()
	f.runs = append(f.runs, rec)
	f.mu.Unlock()
	defer close(rec.returned)
	switch f.mode {
	case bgPanic:
		panic("fake background exploded")
	case bgStubborn:
		<-f.release
		return
	}
	<-ctx.Done()
	f.mu.Lock()
	dial, logTo := f.dial, f.logTo
	f.mu.Unlock()
	if dial != "" {
		rec.dialed = true
		conn, err := net.DialTimeout("tcp", dial, time.Second)
		if err == nil {
			_ = conn.Close()
		}
		rec.dialErr = err
	}
	if logTo != nil {
		fmt.Fprintln(logTo, `{"level":"INFO","msg":"fake background returned"}`)
	}
}

func (f *fakeBackground) starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *fakeBackground) run(t *testing.T, i int) *bgRecord {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.runs) {
		t.Fatalf("run %d of F-BG never started (%d runs)", i, len(f.runs))
	}
	return f.runs[i]
}

// isClosed fails the test unless ch is already closed.
func isClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatalf("%s: not yet", what)
	}
}

// releaseAll lets every stubborn run return.
func (f *fakeBackground) releaseAll() { f.once.Do(func() { close(f.release) }) }

// writeBgConfig writes a configuration listening on listen with the apt
// section enabled or not at lockWait, followed by extra YAML.
func writeBgConfig(t *testing.T, path, listen string, enabled bool, lockWait, extra string) {
	t.Helper()
	body := fmt.Sprintf("host: {name: daemon-test}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n"+
		"  apt: {enabled: %t, lockWait: %s}\n%s", listen, enabled, lockWait, extra)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bgDaemon is an in-process daemon serving F-BG.
type bgDaemon struct {
	*inProcess
	cfgPath, listen string
}

func startBg(t *testing.T, enabled bool, extra string, providers ...provider.Provider) *bgDaemon {
	t.Helper()
	b := &bgDaemon{cfgPath: filepath.Join(t.TempDir(), "nexusd.yaml"), listen: freePort(t)}
	writeBgConfig(t, b.cfgPath, b.listen, enabled, "100s", extra)
	b.inProcess = startInProcess(t, b.cfgPath, providers...)
	return b
}

func (b *bgDaemon) rewrite(t *testing.T, enabled bool, lockWait, extra string) {
	t.Helper()
	writeBgConfig(t, b.cfgPath, b.listen, enabled, lockWait, extra)
}

// reload runs an API reload and fails the test unless it is applied.
func (b *bgDaemon) reload(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := b.d.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

// get answers GET path with token (none when empty).
func (b *bgDaemon) get(t *testing.T, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+b.listen+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	c.CloseIdleConnections()
	return resp.StatusCode, body
}

// logRecords decodes every JSON line written to b.
func logRecords(b *syncBuffer) []map[string]any {
	var recs []map[string]any
	for line := range strings.SplitSeq(logText(b), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			recs = append(recs, rec)
		}
	}
	return recs
}

// firstLog is the index of the first record whose msg is msg, or -1.
func firstLog(recs []map[string]any, msg string) int {
	for i, rec := range recs {
		if rec["msg"] == msg {
			return i
		}
	}
	return -1
}

// logTime is the time of the first record whose msg is msg.
func logTime(t *testing.T, recs []map[string]any, msg string) time.Time {
	t.Helper()
	i := firstLog(recs, msg)
	if i < 0 {
		t.Fatalf("no %q record", msg)
	}
	s, _ := recs[i]["time"].(string)
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%q record time %q: %v", msg, s, err)
	}
	return at
}

// stillRunningWarnings counts the warnings naming provider apt as still
// running after its stop bound.
func stillRunningWarnings(recs []map[string]any) int {
	n := 0
	for _, rec := range recs {
		if rec["level"] == "WARN" && rec["provider"] == "apt" &&
			strings.Contains(fmt.Sprint(rec["msg"]), "still running") {
			n++
		}
	}
	return n
}

// TC-HP-1: Startup starts an enabled background provider exactly once and
// never a disabled one.
func TestTCHP1_StartupStartsAnEnabledBackgroundProviderExactly(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		f := newFakeBackground(bgNormal)
		b := startBg(t, true, "", f)
		eventually(t, time.Second, "F-BG starts after READY", func() bool { return f.starts() == 1 })
		if code, body := b.get(t, "/healthz", ""); code != http.StatusOK {
			t.Fatalf("/healthz: %d %s", code, body)
		}
		first := f.run(t, 0)
		if err := first.ctx.Err(); err != nil {
			t.Fatalf("the run's ctx is done while serving: %v", err)
		}
		if n := f.starts(); n != 1 {
			t.Fatalf("F-BG started %d times, want 1", n)
		}
		b.signal(t, syscall.SIGTERM)
		b.wait(t)
		within(t, first.returned, time.Second, "the run returns at shutdown")
		if n := f.starts(); n != 1 {
			t.Fatalf("F-BG started %d times, want 1", n)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		f := newFakeBackground(bgNormal)
		b := startBg(t, false, "", f)
		if code, body := b.get(t, "/healthz", ""); code != http.StatusOK {
			t.Fatalf("/healthz: %d %s", code, body)
		}
		if n := f.starts(); n != 0 {
			t.Fatalf("disabled F-BG started %d times while serving", n)
		}
		b.signal(t, syscall.SIGTERM)
		b.wait(t)
		if n := f.starts(); n != 0 {
			t.Fatalf("disabled F-BG started %d times", n)
		}
	})
}

// TC-HP-2: A reload that disables the provider cancels its run before
// reload returns.
func TestTCHP2_AReloadThatDisablesTheProviderCancels(t *testing.T) {
	t.Run("Reload", func(t *testing.T) {
		f := newFakeBackground(bgNormal)
		b := startBg(t, true, "", f)
		eventually(t, time.Second, "F-BG starts", func() bool { return f.starts() == 1 })
		first := f.run(t, 0)
		b.rewrite(t, false, "100s", "")
		b.reload(t)
		select {
		case <-first.returned:
		default:
			t.Fatal("RunBackground had not returned when Reload returned")
		}
		if first.ctx.Err() == nil {
			t.Fatal("the run's ctx is not done after the disabling reload")
		}
		if n := f.starts(); n != 1 {
			t.Fatalf("F-BG started %d times, want 1", n)
		}
		b.signal(t, syscall.SIGTERM)
		b.wait(t)
	})
	t.Run("SIGHUP", func(t *testing.T) {
		f := newFakeBackground(bgNormal)
		b := startBg(t, true, "", f)
		eventually(t, time.Second, "F-BG starts", func() bool { return f.starts() == 1 })
		first := f.run(t, 0)
		b.rewrite(t, false, "100s", "")
		b.signal(t, syscall.SIGHUP)
		within(t, first.returned, time.Second, "the run returns after SIGHUP disables it")
		if first.ctx.Err() == nil {
			t.Fatal("the run's ctx is not done after the disabling SIGHUP")
		}
		if n := f.starts(); n != 1 {
			t.Fatalf("F-BG started %d times, want 1", n)
		}
		b.signal(t, syscall.SIGTERM)
		b.wait(t)
	})
}

// TC-HP-3: A reload that re-enables the provider starts a fresh run with a
// new context.
func TestTCHP3_AReloadThatReEnablesTheProvider(t *testing.T) {
	f := newFakeBackground(bgNormal)
	b := startBg(t, true, "", f)
	eventually(t, time.Second, "F-BG starts", func() bool { return f.starts() == 1 })
	b.rewrite(t, false, "100s", "")
	b.reload(t)
	first := f.run(t, 0)
	isClosed(t, first.returned, "the first run returned before Reload")

	b.rewrite(t, true, "100s", "")
	b.reload(t)
	eventually(t, time.Second, "F-BG starts again", func() bool { return f.starts() == 2 })
	second := f.run(t, 1)
	if second.ctx == first.ctx {
		t.Fatal("the second run got the first run's ctx")
	}
	if err := second.ctx.Err(); err != nil {
		t.Fatalf("the second run's ctx is done: %v", err)
	}
	if first.ctx.Err() == nil {
		t.Fatal("the first run's ctx is no longer done")
	}
	b.signal(t, syscall.SIGTERM)
	b.wait(t)
}

// TC-HP-4: A reload rejected at apply leaves the running background
// untouched.
func TestTCHP4_AReloadRejectedAtApplyLeavesThe(t *testing.T) {
	f := newFakeBackground(bgNormal)
	docker := &sectionProvider{name: "docker"}
	b := startBg(t, true, "  docker: {enabled: true, socket: /run/docker.sock}\n", f, docker)
	eventually(t, time.Second, "F-BG starts", func() bool { return f.starts() == 1 })
	first := f.run(t, 0)

	b.rewrite(t, false, "100s", "  docker: {enabled: true, socket: "+rejectSocket+"}\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := b.d.Reload(ctx)
	var re *provider.ReloadError
	if !errors.As(err, &re) || re.Stage != "apply" {
		t.Fatalf("Reload: %v, want an apply-stage ReloadError", err)
	}
	if err := first.ctx.Err(); err != nil {
		t.Fatalf("the run's ctx is done after a rejected reload: %v", err)
	}
	if n := f.starts(); n != 1 {
		t.Fatalf("F-BG started %d times, want 1", n)
	}
	for _, rec := range logRecords(b.logs) {
		if strings.HasPrefix(fmt.Sprint(rec["msg"]), "stopping") {
			t.Fatalf("a stop was attempted after a rejected reload: %v", rec)
		}
	}
	b.signal(t, syscall.SIGTERM)
	b.wait(t)
}

// TC-HP-5: Shutdown stops the background run after the server and before
// the operations manager.
func TestTCHP5_ShutdownStopsTheBackgroundRunAfterThe(t *testing.T) {
	f := newFakeBackground(bgNormal)
	b := startBg(t, true, "", f)
	eventually(t, time.Second, "F-BG starts", func() bool { return f.starts() == 1 })
	f.mu.Lock()
	f.logTo, f.dial = b.logs, b.listen
	f.mu.Unlock()

	b.signal(t, syscall.SIGTERM)
	b.wait(t) // fails unless Run returns nil
	first := f.run(t, 0)
	isClosed(t, first.returned, "the run returned before Run")
	if !first.dialed {
		t.Fatal("the run never dialled the listen address")
	}
	if first.dialErr == nil {
		t.Fatalf("a dial to %s succeeded once the run's ctx was done: the server was still listening", b.listen)
	}

	recs := logRecords(b.logs)
	order := []string{"stopping background providers", "fake background returned", "stopping operations",
		"nexusd stopped"}
	prev := -1
	for _, msg := range order {
		i := firstLog(recs, msg)
		if i < 0 {
			t.Fatalf("no %q record:\n%s", msg, logText(b.logs))
		}
		if i <= prev {
			t.Fatalf("%q is logged out of order %v:\n%s", msg, order, logText(b.logs))
		}
		prev = i
	}
}

// TC-HP-6: validate-config and CheckConfig never start a background run.
func TestTCHP6_ValidateConfigAndCheckConfigNeverStartA(t *testing.T) {
	f := newFakeBackground(bgNormal)
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeBgConfig(t, path, "127.0.0.1:7443", true, "100s", "")
	baseline := runtime.NumGoroutine()

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ValidateProviders([]provider.Provider{f}, providerConfigs(cfg)); err != nil {
		t.Fatalf("core.ValidateProviders: %v", err)
	}
	if _, errs := CheckConfig(path); len(errs) != 0 {
		t.Fatalf("CheckConfig: %v", errs)
	}
	// CheckConfig builds its own providers, so F-BG goes through the same
	// path with it behind the provider set; the real host provider returns
	// from RunBackground at once off Linux and would leave nothing to see.
	withF := func(string, *slog.Logger) []provider.Provider { return []provider.Provider{f} }
	if _, errs := checkConfigWith(path, withF); len(errs) != 0 {
		t.Fatalf("checkConfigWith: %v", errs)
	}
	if n := f.starts(); n != 0 {
		t.Fatalf("F-BG started %d times by validation", n)
	}
	eventually(t, time.Second, "no goroutine left behind", func() bool {
		return runtime.NumGoroutine() <= baseline
	})
}

// TC-HP-7: Goroutines return to the baseline after stop.
func TestTCHP7_GoroutinesReturnToTheBaselineAfterStop(t *testing.T) {
	baseline := runtime.NumGoroutine()
	f := newFakeBackground(bgNormal)
	b := startBg(t, true, "", f)
	eventually(t, time.Second, "F-BG starts", func() bool { return f.starts() == 1 })
	for _, enabled := range []bool{false, true, false} {
		b.rewrite(t, enabled, "100s", "")
		b.reload(t)
	}
	if n := f.starts(); n != 2 {
		t.Fatalf("F-BG started %d times, want 2", n)
	}
	for i := range 2 {
		isClosed(t, f.run(t, i).returned, fmt.Sprintf("run %d returned", i))
	}
	b.signal(t, syscall.SIGTERM)
	b.wait(t)
	eventually(t, 5*time.Second, "no goroutine left behind", func() bool {
		return runtime.NumGoroutine() <= baseline
	})
}

// TC-HP-E1: A reload that keeps the provider enabled does not restart its
// run.
func TestTCHPE1_AReloadThatKeepsTheProviderEnabled(t *testing.T) {
	f := newFakeBackground(bgNormal)
	b := startBg(t, true, "", f)
	eventually(t, time.Second, "F-BG starts", func() bool { return f.starts() == 1 })
	first := f.run(t, 0)
	for _, lockWait := range []string{"200s", "300s"} {
		b.rewrite(t, true, lockWait, "")
		b.reload(t)
	}
	if got := f.accepted.Load(); got != "5m0s" {
		t.Fatalf("apt section in force %v, want 5m0s: the reloads were not applied", got)
	}
	if err := first.ctx.Err(); err != nil {
		t.Fatalf("the run's ctx is done after reloads that kept it enabled: %v", err)
	}
	if n := f.starts(); n != 1 {
		t.Fatalf("F-BG started %d times, want 1", n)
	}
	b.signal(t, syscall.SIGTERM)
	b.wait(t)
}

// TC-HP-E2: A listen failure never starts background work.
func TestTCHPE2_AListenFailureNeverStartsBackgroundWork(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeBgConfig(t, path, ln.Addr().String(), true, "100s", "")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeBackground(bgNormal)
	d, err := New(cfg, Options{Providers: []provider.Provider{f}, ConfigPath: path,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), StateDir: t.TempDir(), AuditDir: t.TempDir(),
		Detach: detachtest.Local(), Signals: make(chan os.Signal)})
	if err != nil {
		t.Fatal(err)
	}
	err = d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "listen on "+ln.Addr().String()) {
		t.Fatalf("Run: %v, want the listen error", err)
	}
	if n := f.starts(); n != 0 {
		t.Fatalf("F-BG started %d times after a listen failure", n)
	}
}

// TC-HP-E3: Providers without Background and unknown names are ignored.
func TestTCHPE3_ProvidersWithoutBackgroundAndUnknownNamesAre(t *testing.T) {
	logs := &syncBuffer{}
	f := newFakeBackground(bgNormal)
	b := newBackgrounds([]provider.Provider{&sectionProvider{name: "docker"}, f},
		slog.New(slog.NewJSONHandler(logs, nil)))
	b.reconcile(map[string]bool{"apt": true, "nope": true, "docker": true})
	eventually(t, time.Second, "F-BG starts", func() bool { return f.starts() == 1 })
	if len(b.running) != 1 {
		t.Fatalf("%d runs, want only F-BG's", len(b.running))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	b.stopAll(ctx)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("stopAll took %s for a run that returns at once", el)
	}
	isClosed(t, f.run(t, 0).returned, "F-BG returned before stopAll")
	if n := f.starts(); n != 1 {
		t.Fatalf("F-BG started %d times, want 1", n)
	}
	for _, rec := range logRecords(logs) {
		if rec["level"] == "ERROR" || rec["level"] == "WARN" {
			t.Errorf("unexpected %v record: %v", rec["level"], rec)
		}
		if p := rec["provider"]; p == "nope" || p == "docker" {
			t.Errorf("a record names %v: %v", p, rec)
		}
	}
}

// TC-HP-ERR1: A panicking RunBackground is logged and nexusd keeps serving.
func TestTCHPERR1_APanickingRunBackgroundIsLoggedAndNexusd(t *testing.T) {
	f := newFakeBackground(bgPanic)
	b := startBg(t, true, "", f)
	eventually(t, time.Second, "the panic is logged", func() bool {
		return b.logs.count("background provider panicked") == 1
	})
	token, _, err := auth.Create(b.d.StateDir(), auth.CreateRequest{Name: "hostprov", Scopes: []string{"apt:read"}})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := b.get(t, "/healthz", ""); code != http.StatusOK {
		t.Fatalf("/healthz: %d %s", code, body)
	}
	code, body := b.get(t, "/v1/capabilities", token)
	if code != http.StatusOK {
		t.Fatalf("/v1/capabilities: %d %s", code, body)
	}
	var caps apitypes.Capabilities
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatal(err)
	}
	status := apitypes.ProviderState("")
	for _, p := range caps.Providers {
		if p.Name == "apt" {
			status = p.Status
		}
	}
	if status != apitypes.ProviderEnabled {
		t.Fatalf("apt is %q after its run panicked, want enabled", status)
	}

	var panics []map[string]any
	for _, rec := range logRecords(b.logs) {
		if rec["msg"] == "background provider panicked" {
			panics = append(panics, rec)
		}
	}
	if len(panics) != 1 {
		t.Fatalf("%d panic records, want 1:\n%s", len(panics), logText(b.logs))
	}
	rec := panics[0]
	stack := fmt.Sprint(rec["stack"])
	if rec["level"] != "ERROR" || rec["provider"] != "apt" ||
		!strings.Contains(stack, "goroutine") || !strings.Contains(stack, "background.go") {
		t.Fatalf("panic record %v, want ERROR naming apt with a stack", rec)
	}

	b.rewrite(t, false, "100s", "")
	b.reload(t)
	b.rewrite(t, true, "100s", "")
	b.reload(t)
	eventually(t, time.Second, "F-BG starts again", func() bool { return f.starts() == 2 })
	b.signal(t, syscall.SIGTERM)
	b.wait(t)
}

// TC-HP-ERR2: A runner that ignores cancellation cannot hold up reload or
// shutdown.
func TestTCHPERR2_ARunnerThatIgnoresCancellationCannotHold(t *testing.T) {
	saved := backgroundStopTimeout
	backgroundStopTimeout = 200 * time.Millisecond
	t.Cleanup(func() { backgroundStopTimeout = saved })
	baseline := runtime.NumGoroutine()

	f := newFakeBackground(bgStubborn)
	b := startBg(t, true, "", f)
	t.Cleanup(f.releaseAll) // before the daemon's cleanup, which runs later
	eventually(t, time.Second, "F-BG starts", func() bool { return f.starts() == 1 })

	// Step 1.
	b.rewrite(t, false, "100s", "")
	start := time.Now()
	b.reload(t)
	if el := time.Since(start); el < 200*time.Millisecond || el > 400*time.Millisecond {
		t.Errorf("the disabling reload took %s, want 200ms to 400ms", el)
	}
	if n := stillRunningWarnings(logRecords(b.logs)); n != 1 {
		t.Errorf("%d warnings name apt as still running after the reload, want 1:\n%s", n, logText(b.logs))
	}

	// Step 2.
	b.rewrite(t, true, "100s", "")
	b.reload(t)
	eventually(t, time.Second, "F-BG starts again", func() bool { return f.starts() == 2 })
	signalled := time.Now()
	b.signal(t, syscall.SIGTERM)
	b.wait(t)
	if el := time.Since(signalled); el > time.Second {
		t.Errorf("Run returned %s after SIGTERM, want within 1s", el)
	}
	recs := logRecords(b.logs)
	gap := logTime(t, recs, "stopping operations").Sub(logTime(t, recs, "stopping background providers"))
	if gap > backgroundStopTimeout+100*time.Millisecond {
		t.Errorf("stopping operations logged %s after stopping background providers, want at most %s",
			gap, backgroundStopTimeout+100*time.Millisecond)
	}
	if n := stillRunningWarnings(recs); n != 2 {
		t.Errorf("%d warnings name apt as still running, want 2 (reload and shutdown):\n%s", n, logText(b.logs))
	}

	// Step 3: one shared bound, the earlier of backgroundStopTimeout and the
	// deadline of ctx.
	backgroundStopTimeout = 5 * time.Second
	f2 := newFakeBackground(bgStubborn)
	t.Cleanup(f2.releaseAll)
	bg := newBackgrounds([]provider.Provider{f2}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	bg.reconcile(map[string]bool{"apt": true})
	eventually(t, time.Second, "the second set's F-BG starts", func() bool { return f2.starts() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	bg.stopAll(ctx)
	if el := time.Since(start); el < 100*time.Millisecond || el > 200*time.Millisecond {
		t.Errorf("stopAll with a 100ms deadline took %s, want 100ms to 200ms", el)
	}

	// Step 4.
	f.releaseAll()
	f2.releaseAll()
	eventually(t, 5*time.Second, "no goroutine left behind", func() bool {
		return runtime.NumGoroutine() <= baseline
	})
}
