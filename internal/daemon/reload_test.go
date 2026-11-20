// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/provider"
)

// The authenticator is built once at startup, so a changed
// auth.failureLimitPerMinute must be reported as needing a restart rather
// than silently kept.
func TestRestartRequiredReportsAuthChanges(t *testing.T) {
	running := config.Default()
	same := config.Default()
	if keys := restartRequired(&running, &same); len(keys) != 0 {
		t.Fatalf("identical configurations: %v, want none", keys)
	}
	next := config.Default()
	next.Auth.FailureLimitPerMinute = 3
	if keys := restartRequired(&running, &next); !slices.Equal(keys, []string{"auth"}) {
		t.Fatalf("changed auth.failureLimitPerMinute: %v, want [auth]", keys)
	}
}

// The audit log is opened once at startup with its retention, so a changed
// audit.retentionDays must be reported as needing a restart.
func TestRestartRequiredReportsAuditChanges(t *testing.T) {
	running := config.Default()
	next := config.Default()
	next.Audit.RetentionDays = 30
	if keys := restartRequired(&running, &next); !slices.Equal(keys, []string{"audit"}) {
		t.Fatalf("changed audit.retentionDays: %v, want [audit]", keys)
	}
}

// sectionProvider records the last section it accepted. It takes the name
// of a real configuration section and decodes it into that section's type,
// so the real loader and the real reload path run. A docker section whose
// socket is rejectSocket is refused, as a provider may refuse what
// validate-config accepts.
type sectionProvider struct {
	name     string
	accepted atomic.Value // string: the field the test changes
}

const rejectSocket = "/refused-by-provider.sock"

func (p *sectionProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: p.name, ReadScope: p.name + ":read", Ops: []def.Op{probeOps[p.name].Op()}}
}

func (p *sectionProvider) Configure(decode func(any) error) error {
	switch p.name {
	case "apt":
		var c config.AptConfig
		if err := decode(&c); err != nil {
			return err
		}
		p.accepted.Store(c.LockWait.String())
	case "docker":
		var c config.DockerConfig
		if err := decode(&c); err != nil {
			return err
		}
		if c.Socket == rejectSocket {
			return fmt.Errorf("providers.docker.socket: refused")
		}
		p.accepted.Store(c.Socket)
	}
	return nil
}

func (p *sectionProvider) Preflight(context.Context) provider.Status { return ok() }

func (p *sectionProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Sync(probeOps[p.name], func(_ context.Context, in probeIn) (probeOut, error) {
		return probeOut(in), nil
	})}
}

// A reload is all or nothing across providers: when one provider refuses
// its section, a provider that accepted its new section first is put back
// to the configuration in force, which is the last one applied, not the
// one nexusd started with.
func TestReloadIsAllOrNothingAcrossProviders(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	write := func(lockWait, socket string) {
		t.Helper()
		body := fmt.Sprintf("listen: [\"127.0.0.1:7443\"]\ntls: {mode: off}\nproviders:\n"+
			"  apt: {enabled: true, lockWait: %s}\n  docker: {enabled: true, socket: %s}\n", lockWait, socket)
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("100s", "/run/docker.sock")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	apt, docker := &sectionProvider{name: "apt"}, &sectionProvider{name: "docker"}
	d, err := New(cfg, Options{Providers: []provider.Provider{apt, docker}, ConfigPath: cfgPath,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), StateDir: t.TempDir(), AuditDir: t.TempDir(),
		Detach: detachtest.Local()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.store.Close(); err != nil {
			t.Error(err)
		}
		if err := d.audit.Close(); err != nil {
			t.Error(err)
		}
	})
	check := func(step, wantApt, wantDocker string) {
		t.Helper()
		if got := apt.accepted.Load(); got != wantApt {
			t.Errorf("%s: apt section in force %v, want %s", step, got, wantApt)
		}
		if got := docker.accepted.Load(); got != wantDocker {
			t.Errorf("%s: docker section in force %v, want %s", step, got, wantDocker)
		}
	}
	check("startup", "1m40s", "/run/docker.sock")

	ctx := context.Background()
	write("200s", rejectSocket)
	d.reload(ctx)
	check("docker refused", "1m40s", "/run/docker.sock")

	write("300s", "/var/run/docker.sock")
	d.reload(ctx)
	check("accepted", "5m0s", "/var/run/docker.sock")

	write("400s", rejectSocket)
	d.reload(ctx)
	check("refused after an accepted reload", "5m0s", "/var/run/docker.sock")
}

// reloadProvider takes the apt section. Its Configure counts overlapping
// calls, can be held by a gate, and records the lockWait it accepted, which
// stands for the configuration in force; applied counts the sections it
// accepted, the applied configuration's generation. It is also a
// DaemonUser and records each UseDaemon.
type reloadProvider struct {
	hold     time.Duration // Configure sleeps this long
	inFlight atomic.Int32
	overlaps atomic.Int64
	applied  atomic.Int64
	lockWait atomic.Value // string
	gate     atomic.Pointer[reloadGate]

	mu      sync.Mutex
	daemons []provider.Daemon
}

// reloadGate holds the next Configure: it closes entered, then waits for
// release.
type reloadGate struct{ entered, release chan struct{} }

func (p *reloadProvider) arm() *reloadGate {
	g := &reloadGate{entered: make(chan struct{}), release: make(chan struct{})}
	p.gate.Store(g)
	return g
}

func (p *reloadProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "apt", ReadScope: "apt:read", Ops: []def.Op{probeOps["apt"].Op()}}
}

func (p *reloadProvider) Configure(decode func(any) error) error {
	if p.inFlight.Add(1) > 1 {
		p.overlaps.Add(1)
	}
	defer p.inFlight.Add(-1)
	if g := p.gate.Swap(nil); g != nil {
		close(g.entered)
		<-g.release
	}
	time.Sleep(p.hold)
	var c config.AptConfig
	if err := decode(&c); err != nil {
		return err
	}
	p.lockWait.Store(c.LockWait.String())
	p.applied.Add(1)
	return nil
}

func (p *reloadProvider) Preflight(context.Context) provider.Status { return ok() }

func (p *reloadProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Sync(probeOps["apt"], func(_ context.Context, in probeIn) (probeOut, error) {
		return probeOut(in), nil
	})}
}

func (p *reloadProvider) UseDaemon(d provider.Daemon) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.daemons = append(p.daemons, d)
}

// writeAptConfig writes a configuration listening on listen with the apt
// section enabled at lockWait, followed by extra YAML.
func writeAptConfig(t *testing.T, path, listen, lockWait, extra string) {
	t.Helper()
	body := fmt.Sprintf("host: {name: daemon-test}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n"+
		"  apt: {enabled: true, lockWait: %s}\n%s", listen, lockWait, extra)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// logText is everything written to b so far (syncBuffer: mcp_test.go).
func logText(b *syncBuffer) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// inProcess is a Daemon running in this process, whose signals come through
// the Options.Signals seam.
type inProcess struct {
	d      *Daemon
	sigs   chan os.Signal
	cancel context.CancelFunc
	runErr chan error
	logs   *syncBuffer
	done   bool
}

func startInProcess(t *testing.T, cfgPath string, providers ...provider.Provider) *inProcess {
	t.Helper()
	return startInProcessIn(t, cfgPath, t.TempDir(), providers...)
}

// startInProcessIn is startInProcess over the given state directory, so two
// runs in turn can share it.
func startInProcessIn(t *testing.T, cfgPath, stateDir string, providers ...provider.Provider) *inProcess {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	r := &inProcess{sigs: make(chan os.Signal), runErr: make(chan error, 1), logs: &syncBuffer{}}
	r.d, err = New(cfg, Options{Providers: providers, ConfigPath: cfgPath,
		Logger: slog.New(slog.NewJSONHandler(r.logs, nil)), StateDir: stateDir, AuditDir: t.TempDir(),
		Detach: detachtest.Local(), Signals: r.sigs})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.runErr <- r.d.Run(ctx) }()
	t.Cleanup(func() {
		if r.done {
			return
		}
		cancel()
		select {
		case <-r.runErr:
		case <-time.After(ShutdownTimeout + 5*time.Second):
			t.Error("Run still running after the test")
		}
	})
	select {
	case <-r.d.Ready():
	case err := <-r.runErr:
		r.done = true
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready within 10s")
	}
	return r
}

// signal hands sig to the Run loop; the send returns once the loop took it.
func (r *inProcess) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	select {
	case r.sigs <- sig:
	case <-time.After(2 * time.Second):
		t.Fatalf("the Run loop did not take %v within 2s", sig)
	}
}

// wait waits for Run to return, at most ShutdownTimeout.
func (r *inProcess) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.runErr:
		r.done = true
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(ShutdownTimeout):
		t.Fatalf("Run did not return within %s", ShutdownTimeout)
	}
}

// within waits for ch to be closed, at most d.
func within(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("%s: not within %s", what, d)
	}
}

// eventually polls cond for up to within.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TC-SM-45: Reload calls and SIGHUPs all run in the Run loop, one at a
// time.
func TestTCSM45_APIReloadsAndSIGHUPReloadsNever(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeAptConfig(t, cfgPath, freePort(t), "100s", "")
	p := &reloadProvider{hold: 2 * time.Millisecond}
	r := startInProcess(t, cfgPath, p)
	before := p.applied.Load()

	const n = 100
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_, err := r.d.Reload(ctx)
			errs <- err
		})
	}
	var sighups atomic.Int64
	wg.Go(func() {
		for range n {
			select {
			case r.sigs <- syscall.SIGHUP:
				sighups.Add(1)
			case <-ctx.Done():
				return
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Reload: %v", err)
		}
	}
	if got := p.overlaps.Load(); got != 0 {
		t.Fatalf("%d Configure calls overlapped another", got)
	}
	// Registry.Apply holds its own lock around Configure, so the log is the
	// witness for whole reloads: each one ends before the next one starts.
	open := false
	for line := range strings.Lines(logText(r.logs)) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch rec["msg"] {
		case "reloading configuration":
			if open {
				t.Fatal("a reload started before the previous one ended")
			}
			open = true
		case "configuration reloaded":
			open = false
		}
	}
	// A SIGHUP taken by the loop is served before the next request, and the
	// next Reload is answered only after it.
	if _, err := r.d.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := p.applied.Load()-before, n+sighups.Load()+1; got < want {
		t.Fatalf("%d Configure calls, want at least %d (%d SIGHUPs processed)", got, want, sighups.Load())
	}
	r.cancel()
	r.wait(t)
}

// TC-SM-46: Reload answers with the keys that need a restart, sorted, or a
// typed error naming the stage that failed, and the configuration in force
// stays the last one applied.
func TestTCSM46_ReloadReportsRestartRequiredAndTypedFailures(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	write := func(listen, lockWait, socket, extra string) {
		t.Helper()
		body := fmt.Sprintf("listen: [%q]\ntls: {mode: off}\nproviders:\n"+
			"  apt: {enabled: true, lockWait: %s}\n  docker: {enabled: true, socket: %s}\n%s",
			listen, lockWait, socket, extra)
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(listen, "100s", "/run/docker.sock", "")
	apt, docker := &sectionProvider{name: "apt"}, &sectionProvider{name: "docker"}
	r := startInProcess(t, cfgPath, apt, docker)
	ctx := context.Background()

	// Step 1.
	write(freePort(t), "300s", "/var/run/docker.sock", "limits: {maxBodyBytes: 2097152}\n")
	res, err := r.d.Reload(ctx)
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if !slices.Equal(res.RestartRequired, []string{"limits", "listen"}) {
		t.Fatalf("step 1: restartRequired %q, want [limits listen]", res.RestartRequired)
	}

	// Step 2.
	if err := os.WriteFile(cfgPath, []byte("listen: [\"127.0.0.1:1\"\n  bad: : yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = r.d.Reload(ctx)
	var re *provider.ReloadError
	if !errors.As(err, &re) || re.Stage != "load" {
		t.Fatalf("step 2: %v, want a *provider.ReloadError of stage load", err)
	}

	// Step 3.
	write(listen, "400s", rejectSocket, "")
	_, err = r.d.Reload(ctx)
	re = nil
	if !errors.As(err, &re) || re.Stage != "apply" {
		t.Fatalf("step 3: %v, want a *provider.ReloadError of stage apply", err)
	}

	// Step 4.
	if got := apt.accepted.Load(); got != "5m0s" {
		t.Errorf("step 4: apt section in force %v, want step 1's 5m0s", got)
	}
	if got := docker.accepted.Load(); got != "/var/run/docker.sock" {
		t.Errorf("step 4: docker section in force %v, want step 1's /var/run/docker.sock", got)
	}
	r.cancel()
	r.wait(t)
}

// TC-SM-47: New hands itself once to each DaemonUser, before Run.
func TestTCSM47_UseDaemonIsCalledOnceAndExposes(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeConfig(t, cfgPath, freePort(t))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	p := &reloadProvider{}
	constructed := time.Now()
	d, err := New(cfg, Options{Providers: []provider.Provider{p}, ConfigPath: cfgPath,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), StateDir: stateDir, AuditDir: t.TempDir(),
		Detach: detachtest.Local()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.store.Close(); err != nil {
			t.Error(err)
		}
		if err := d.audit.Close(); err != nil {
			t.Error(err)
		}
	})
	p.mu.Lock()
	daemons := slices.Clone(p.daemons)
	p.mu.Unlock()
	if len(daemons) != 1 {
		t.Fatalf("UseDaemon called %d times, want 1", len(daemons))
	}
	got := daemons[0]
	if got != provider.Daemon(d) {
		t.Fatalf("UseDaemon got %T %p, want the daemon New returned", got, got)
	}
	if got.ConfigPath() != cfgPath || got.StateDir() != stateDir {
		t.Fatalf("ConfigPath %q, StateDir %q; want %q, %q", got.ConfigPath(), got.StateDir(), cfgPath, stateDir)
	}
	if at := got.StartedAt(); at.Sub(constructed).Abs() > time.Second {
		t.Fatalf("StartedAt %v, constructed at %v", at, constructed)
	}
	check := got.CheckConfig()
	if len(check.Errors) != 0 || check.Config == nil {
		t.Fatalf("CheckConfig of a valid file: config %v, errors %v", check.Config, check.Errors)
	}
}

// TC-SM-E18: SIGHUP still reloads, and logs, as before the daemon hook,
// with a DaemonUser provider in place and no Reload call.
func TestTCSME18_SIGHUPKeepsWorkingUnchangedWithThe(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeAptConfig(t, cfgPath, freePort(t), "100s", "")
	p := &reloadProvider{}
	r := startInProcess(t, cfgPath, p)
	writeAptConfig(t, cfgPath, freePort(t), "200s", "")

	r.signal(t, syscall.SIGHUP)
	eventually(t, 5*time.Second, "configuration reloaded", func() bool {
		return r.logs.count(`"msg":"configuration reloaded"`) > 0
	})
	if got := p.lockWait.Load(); got != "3m20s" {
		t.Fatalf("apt section in force %v, want 3m20s", got)
	}
	var warned []string
	for line := range strings.Lines(logText(r.logs)) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["level"] == "WARN" &&
			rec["msg"] == "configuration change takes effect only after a restart; keeping the running value" &&
			rec["path"] == cfgPath {
			warned = append(warned, fmt.Sprint(rec["key"]))
		}
	}
	if !slices.Equal(warned, []string{"listen"}) {
		t.Fatalf("restart-required keys logged %v, want [listen]\n%s", warned, logText(r.logs))
	}
	r.cancel()
	r.wait(t)
}

// TC-SM-ERR25: Reload never outlives its caller's context or the Run loop,
// and the loop never waits on a caller that has gone.
func TestTCSMERR25_ReloadReturnsPromptlyAfterShutdownOr(t *testing.T) {
	goroutines := runtime.NumGoroutine()
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	writeAptConfig(t, cfgPath, listen, "100s", "")

	t.Run("after shutdown", func(t *testing.T) {
		r := startInProcess(t, cfgPath, &reloadProvider{})
		r.cancel()
		r.wait(t)
		within(t, r.d.stopping, time.Second, "stopping closed after Run returned")
		start := time.Now()
		_, err := r.d.Reload(context.Background())
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("Reload after shutdown: %v after %s, want an error within 1s", err, time.Since(start))
		}
		var re *provider.ReloadError
		if errors.As(err, &re) {
			t.Fatalf("Reload after shutdown ran a reload: %v", err)
		}
	})

	t.Run("canceled before the loop takes it", func(t *testing.T) {
		p := &reloadProvider{}
		r := startInProcess(t, cfgPath, p)
		base := p.applied.Load()

		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := r.d.Reload(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("Reload with a canceled context: %v", err)
		}

		// The loop is busy with a slow (gated) SIGHUP reload.
		g := p.arm()
		r.signal(t, syscall.SIGHUP)
		within(t, g.entered, 2*time.Second, "the loop takes the gated reload")
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(50*time.Millisecond, cancel)
		defer timer.Stop()
		start := time.Now()
		_, err := r.d.Reload(ctx)
		if !errors.Is(err, context.Canceled) || time.Since(start) > 200*time.Millisecond {
			t.Fatalf("Reload canceled at 50ms: %v after %s, want context.Canceled within 200ms",
				err, time.Since(start))
		}
		close(g.release)
		// Answered after the SIGHUP's reload: neither canceled call ran one.
		if _, err := r.d.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := p.applied.Load() - base; got != 2 {
			t.Fatalf("%d reloads ran, want 2: the SIGHUP's and the last Reload's", got)
		}
		r.cancel()
		r.wait(t)
	})

	t.Run("caller gone after the loop took it", func(t *testing.T) {
		p := &reloadProvider{}
		r := startInProcess(t, cfgPath, p)
		base := p.applied.Load()

		// Step 3.
		g := p.arm()
		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			err error
			at  time.Time
		}
		done := make(chan result, 1)
		go func() {
			_, err := r.d.Reload(ctx)
			done <- result{err, time.Now()}
		}()
		within(t, g.entered, 2*time.Second, "the loop takes the gated reload")
		canceledAt := time.Now()
		cancel()
		select {
		case res := <-done:
			if !errors.Is(res.err, context.Canceled) || res.at.Sub(canceledAt) > 200*time.Millisecond {
				t.Fatalf("Reload: %v after %s, want context.Canceled within 200ms of the cancel",
					res.err, res.at.Sub(canceledAt))
			}
		case <-time.After(time.Second):
			t.Fatal("Reload did not return while the apply was gated")
		}
		if got := p.applied.Load() - base; got != 0 {
			t.Fatalf("the gated apply finished early: %d", got)
		}
		close(g.release)
		eventually(t, 2*time.Second, "the taken reload completes", func() bool { return p.applied.Load()-base == 1 })

		// Step 4: the loop did not block on the dropped reply.
		writeAptConfig(t, cfgPath, listen, "200s", "")
		r.signal(t, syscall.SIGHUP)
		eventually(t, 2*time.Second, "the SIGHUP is served", func() bool { return p.applied.Load()-base == 2 })
		if got := p.lockWait.Load(); got != "3m20s" {
			t.Fatalf("apt section in force %v after SIGHUP, want 3m20s", got)
		}

		// Step 5.
		r.signal(t, syscall.SIGTERM)
		r.wait(t)
	})

	eventually(t, 5*time.Second, "no goroutine left behind", func() bool {
		return runtime.NumGoroutine() <= goroutines
	})
}
