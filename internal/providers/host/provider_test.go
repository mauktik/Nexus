package host

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/host/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

var _ provider.Provider = (*Provider)(nil)

// hostSection is the decoder the daemon hands Configure: the strict
// Section.Decode of a configuration whose providers.host section is body.
func hostSection(t *testing.T, body string) func(any) error {
	t.Helper()
	var doc strings.Builder
	doc.WriteString("host:\n  name: h1\nproviders:\n  host:\n")
	for _, l := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		doc.WriteString("    " + l + "\n")
	}
	cfg, err := config.Parse([]byte(doc.String()), "nexusd.yaml")
	if err != nil {
		t.Fatalf("config.Parse:\n%s\n%v", doc.String(), err)
	}
	return cfg.Providers.Host.Decode
}

// strictYAML is a strict decoder of body, for a section config.Parse
// itself refuses: unknown keys are errors, as in Section.Decode.
func strictYAML(body string) func(any) error {
	return func(into any) error {
		dec := yaml.NewDecoder(strings.NewReader(body))
		dec.KnownFields(true)
		return dec.Decode(into)
	}
}

func TestNewDefaultsBeforeConfigure(t *testing.T) {
	p := New(Options{})
	want := Settings{SampleInterval: spec.DefaultSampleInterval, HistoryDuration: spec.DefaultHistoryDuration,
		SourceTimeout: spec.DefaultSourceTimeout}
	if got := p.Settings(); got != want {
		t.Errorf("Settings before Configure = %+v, want %+v", got, want)
	}
	if got := p.Spec(); got.Name != spec.Name || got.ReadScope != spec.ReadScope || len(got.Ops) != 10 {
		t.Errorf("Spec = %s %s with %d ops, want the spec package's", got.Name, got.ReadScope, len(got.Ops))
	}
	// WI 5.1: one sync binding per op, in the spec's order.
	b := p.Bindings()
	if len(b) != len(spec.Spec().Ops) {
		t.Fatalf("Bindings = %d bindings, want one per op (%d)", len(b), len(spec.Spec().Ops))
	}
	for i, op := range spec.Spec().Ops {
		if b[i].Op().ID != op.ID || b[i].Kind() != provider.BindSync {
			t.Errorf("binding %d = %s %s, want %s sync", i, b[i].Op().ID, b[i].Kind(), op.ID)
		}
	}
}

func TestConfigureRefusesAnUnknownKeyAndKeepsTheSettings(t *testing.T) {
	p := New(Options{GOOS: "linux", FS: fixtureFS(t, capUBU)})
	if err := p.Configure(hostSection(t, "sampleInterval: 30s\n")); err != nil {
		t.Fatal(err)
	}
	before := p.Settings()

	err := p.Configure(strictYAML("sampleInterval: 5s\nsampleIntervall: 4s\n"))
	if err == nil || !strings.Contains(err.Error(), "sampleIntervall") {
		t.Fatalf("Configure with an unknown key = %v, want an error naming it", err)
	}
	if got := p.Settings(); got != before {
		t.Errorf("Settings after a refused Configure = %+v, want %+v", got, before)
	}

	refused := errors.New("providers.host.bogus: unknown key")
	if err := p.Configure(func(any) error { return refused }); !errors.Is(err, refused) {
		t.Errorf("Configure = %v, want the decoder's error", err)
	}
	if got := p.Settings(); got != before {
		t.Errorf("Settings after a refused Configure = %+v, want %+v", got, before)
	}
}

// The real decoder refuses the unknown key too, so the strict decode the
// test above relies on is the one the daemon hands Configure.
func TestConfigSectionIsStrict(t *testing.T) {
	_, err := config.Parse([]byte("host:\n  name: h1\nproviders:\n  host:\n    sampleIntervall: 4s\n"), "nexusd.yaml")
	if err == nil || !strings.Contains(err.Error(), "sampleIntervall") {
		t.Errorf("config.Parse with an unknown host key = %v, want an error naming it", err)
	}
}

func TestConfigureTakesEverySetting(t *testing.T) {
	p := New(Options{GOOS: "linux"})
	if err := p.Configure(hostSection(t, "enabled: true\nsampleInterval: 15s\nhistoryDuration: 2h\nsourceTimeout: 500ms\n")); err != nil {
		t.Fatal(err)
	}
	want := Settings{SampleInterval: 15 * time.Second, HistoryDuration: 2 * time.Hour, SourceTimeout: 500 * time.Millisecond}
	if got := p.Settings(); got != want {
		t.Errorf("Settings = %+v, want %+v", got, want)
	}
}

// copyPreflightFiles copies the three Preflight files of capture into a
// temporary root, for os.DirFS: the real fs.FS, not F-FS.
func copyPreflightFiles(t *testing.T, capture string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range spPreflightFiles {
		b, err := os.ReadFile(filepath.Join("testdata", capture, "fs", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Preflight over os.DirFS of a copied capture, as production reads it: the
// copy is available, and a copy without one required file names it.
func TestPreflightOnACopiedCapture(t *testing.T) {
	for _, c := range []struct {
		capture string
		init    platform.InitSystem
	}{{capUBU, platform.Systemd}, {capALP, platform.OpenRC}} {
		t.Run(c.capture, func(t *testing.T) {
			plat := platform.Info{Init: c.init}
			root := copyPreflightFiles(t, c.capture)
			st := New(Options{GOOS: "linux", Platform: &plat, FS: os.DirFS(root)}).Preflight(context.Background())
			if st.State != apitypes.ProviderEnabled || st.Details[DetailInitSystem] != string(c.init) {
				t.Errorf("Preflight = %q %q %v, want enabled on %s", st.State, st.Reason, st.Details, c.init)
			}

			for _, name := range spPreflightFiles {
				root := copyPreflightFiles(t, c.capture)
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
					t.Fatal(err)
				}
				st := New(Options{GOOS: "linux", Platform: &plat, FS: os.DirFS(root)}).Preflight(context.Background())
				if want := "not present: /" + name; st.State != apitypes.ProviderUnavailable || st.Reason != want {
					t.Errorf("without %s: Preflight = %q %q, want unavailable %q", name, st.State, st.Reason, want)
				}
			}
		})
	}
}

// The unconfigured provider on the machine running the test: unavailable
// naming the GOOS off Linux, and, on Linux, the real /proc decides.
func TestPreflightOnThisHost(t *testing.T) {
	st := New(Options{}).Preflight(context.Background())
	if runtime.GOOS != "linux" {
		want := "the host provider reads Linux /proc and /sys; this host is " + runtime.GOOS
		if st.State != apitypes.ProviderUnavailable || st.Reason != want {
			t.Errorf("Preflight = %q %q, want unavailable %q", st.State, st.Reason, want)
		}
		return
	}
	if st.State != apitypes.ProviderEnabled {
		t.Errorf("Preflight on Linux = %q %q, want enabled", st.State, st.Reason)
	}
}

// A Preflight file larger than the read bound is not readable: the
// provider never accepts a truncated /proc file.
func TestPreflightRefusesAnOversizedFile(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	ffs.replace("proc/meminfo", bytes.Repeat([]byte("x"), maxReadBytes+1))
	st := newTCProvider(t, capUBU, ffs, newFakeRun(t, capUBU)).Preflight(context.Background())
	if want := "unrecognized format: /proc/meminfo"; st.State != apitypes.ProviderUnavailable || st.Reason != want {
		t.Errorf("Preflight = %q %q, want unavailable %q", st.State, st.Reason, want)
	}
}

// Self-tests of the seam fakes: each behaves as the real seam it stands
// for (test-strategy.md, Fake Fidelity Contracts).

func TestFakeRunMirrorsSvcctlRun(t *testing.T) {
	ctx := context.Background()
	r := newFakeRun(t, capUBU)

	res, err := r.Run(ctx, argvTimedatectl)
	if err != nil || res.ExitCode != 0 || strings.TrimSpace(string(res.Output)) != "yes" {
		t.Errorf("timedatectl = %d %q %v, want exit 0 \"yes\"", res.ExitCode, res.Output, err)
	}
	spSandboxRun(r)
	if res, err := r.Run(ctx, argvTimedatectl); err != nil || res.ExitCode != 1 {
		t.Errorf("SP-SANDBOX timedatectl = %d %v, want exit 1 and no error", res.ExitCode, err)
	}

	alp := newFakeRun(t, capALP)
	if res, err := alp.Run(ctx, argvRCStatusCrashed); err != nil || res.ExitCode != 1 || len(res.Output) != 0 {
		t.Errorf("rc-status --crashed = %d %q %v, want exit 1, no output", res.ExitCode, res.Output, err)
	}

	r.notStarted(argvChronyc, syscall.ENOENT)
	_, err = r.Run(ctx, argvChronyc)
	if !errors.Is(err, svcctl.ErrNotStarted) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing chronyc = %v, want ErrNotStarted wrapping ErrNotExist", err)
	}
	if got := reasonFor(argvChronyc[0], err); got != "not present: /usr/bin/chronyc" {
		t.Errorf("reason for a missing binary = %q", got)
	}
	r.notStarted(argvChronyc, syscall.EACCES)
	_, err = r.Run(ctx, argvChronyc)
	if got := reasonFor(argvChronyc[0], err); got != "command failed: /usr/bin/chronyc not started" {
		t.Errorf("reason for a binary that cannot run = %q", got)
	}

	if _, err := r.Run(ctx, []string{"timedatectl"}); !errors.Is(err, svcctl.ErrNotStarted) {
		t.Errorf("relative argv0 = %v, want ErrNotStarted", err)
	}

	r.hang(argvSystemctlFailed)
	hctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := r.Run(hctx, argvSystemctlFailed); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hung command = %v, want the context's error", err)
	}

	var unexpected []string
	r.errorf = func(format string, args ...any) { unexpected = append(unexpected, format) }
	if _, err := r.Run(ctx, []string{"/usr/bin/true"}); !errors.Is(err, svcctl.ErrNotStarted) || len(unexpected) != 1 {
		t.Errorf("unlisted argv = %v with %d reports, want ErrNotStarted and one report", err, len(unexpected))
	}
	if n := r.count(argvTimedatectl); n != 2 {
		t.Errorf("timedatectl ran %d times, want 2", n)
	}
}

func TestFakeTickerIsARendezvous(t *testing.T) {
	k := newFakeTickers(t)
	var tk Ticker = k.New(5 * time.Second)
	now := newFakeClock().Now()
	got := make(chan time.Time)
	go func() { got <- <-tk.C() }()
	k.nth(1).Fire(now)
	if v := <-got; !v.Equal(now) {
		t.Errorf("tick = %v, want %v", v, now)
	}
	tk.Reset(time.Second)
	tk.Stop()
	if ev := k.nth(1).recorded(); strings.Join(ev, ",") != "reset 1s,stop" {
		t.Errorf("recorded %q, want reset then stop", ev)
	}
	if iv := k.intervals(); len(iv) != 1 || iv[0] != 5*time.Second {
		t.Errorf("intervals = %v", iv)
	}
}

func TestFakeClockMovesOnlyWhenTold(t *testing.T) {
	c := newFakeClock()
	t0 := c.Now()
	if !c.Now().Equal(t0) {
		t.Error("F-CLK moved by itself")
	}
	c.Advance(90 * time.Second)
	if d := c.Now().Sub(t0); d != 90*time.Second {
		t.Errorf("Advance moved %v", d)
	}
}

func TestFakeAddrsAndUnamePassTheCaptureThrough(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		addrs, err := fakeAddrs(t, capture)()
		if err != nil {
			t.Fatal(err)
		}
		lo := addrs["lo"]
		if len(lo) != 2 || lo[0].String() != "127.0.0.1/8" || lo[1].String() != "::1/128" {
			t.Errorf("%s: lo = %v, want 127.0.0.1/8 then ::1/128", capture, lo)
		}
		if _, ok := addrs["eth0"]; !ok {
			t.Errorf("%s: no eth0 in %v", capture, addrs)
		}
		if m, err := fakeUname(t, capture)(); err != nil || m != "aarch64" {
			t.Errorf("%s: uname = %q %v, want aarch64", capture, m, err)
		}
	}
}
