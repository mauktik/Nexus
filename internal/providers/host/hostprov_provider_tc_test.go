// Stub-style: SKIP-only (system-provider)
//
// Tests of planning/system-provider/test-cases.md TC-HP-48, TC-HP-49, TC-HP-50, TC-HP-51, TC-HP-E11, TC-HP-ERR13.
// Owner WIs: 3.1.

package host

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/platform"
)

// newTCProvider is the provider of the TC-HP-48 to TC-HP-ERR13 setups: F-FS
// over capture, GOOS linux, the capture's init system, and F-RUN.
func newTCProvider(t *testing.T, capture string, ffs *fakeFS, run *fakeRun) *Provider {
	t.Helper()
	plat := platform.Info{Init: platform.Systemd}
	if capture == capALP {
		plat.Init = platform.OpenRC
	}
	return New(Options{Host: "h1", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Platform: &plat,
		GOOS: "linux", FS: ffs, Run: run.Run})
}

// TC-HP-48: Configure decodes strictly and starts nothing. Owner WI 3.1. Requirements: FR-1, FR-11.
//
// - Setup: `New(Options{FS: F-FS(UBU), GOOS: "linux"})`; goroutine baseline.
// - Steps: `Configure` with a decoder of `{enabled: true, sampleInterval: 5s}`; read `Settings()`.
// - Expected: `Settings{5s, 1h, 2s}` (unset keys take defaults); `p.run.Load() == nil`; goroutine count unchanged; calling `Configure` three times more changes nothing else.
func TestTCHP48_ConfigureDecodesStrictlyAndStartsNothing(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	run := newFakeRun(t, capUBU)
	p := newTCProvider(t, capUBU, ffs, run)
	baseline := runtime.NumGoroutine()

	decode := hostSection(t, "enabled: true\nsampleInterval: 5s\n")
	want := Settings{SampleInterval: 5 * time.Second, HistoryDuration: time.Hour, SourceTimeout: 2 * time.Second}
	for i := range 4 {
		if err := p.Configure(decode); err != nil {
			t.Fatalf("Configure #%d: %v", i+1, err)
		}
		if got := p.Settings(); got != want {
			t.Fatalf("Settings after Configure #%d = %+v, want %+v", i+1, got, want)
		}
		if n := runtime.NumGoroutine(); n > baseline {
			t.Errorf("Configure #%d: %d goroutines, baseline %d", i+1, n, baseline)
		}
		if p.run.Load() != nil {
			t.Errorf("Configure #%d started a sampler run", i+1)
		}
	}
	if calls := ffs.recorded(); len(calls) != 0 {
		t.Errorf("Configure touched the FS: %v", calls)
	}
	if calls := run.recorded(); len(calls) != 0 {
		t.Errorf("Configure ran commands: %q", calls)
	}
}

// TC-HP-49: Settings swap atomically under the race detector. Owner WI 3.1. Requirements: FR-1.
//
// - Setup: as TC-HP-48.
// - Steps: 8 goroutines alternate `Configure` with `{sampleInterval: 2s, historyDuration: 2s}` and `{sampleInterval: 60s, historyDuration: 24h}` 500 times while 8 read `Settings()`.
// - Expected: no race report; every read is one of the two complete settings (never `2s` with `24h`).
func TestTCHP49_SettingsSwapAtomicallyUnderTheRaceDetector(t *testing.T) {
	p := newTCProvider(t, capUBU, fixtureFS(t, capUBU), newFakeRun(t, capUBU))
	decoders := []func(any) error{
		hostSection(t, "sampleInterval: 2s\nhistoryDuration: 2s\n"),
		hostSection(t, "sampleInterval: 60s\nhistoryDuration: 24h\n"),
	}
	valid := []Settings{
		{SampleInterval: 2 * time.Second, HistoryDuration: 2 * time.Second, SourceTimeout: 2 * time.Second},
		{SampleInterval: 60 * time.Second, HistoryDuration: 24 * time.Hour, SourceTimeout: 2 * time.Second},
	}
	// Start from one of the two, so a reader that runs before any writer
	// does not see the defaults.
	if err := p.Configure(decoders[0]); err != nil {
		t.Fatal(err)
	}

	var writers, readers sync.WaitGroup
	done := make(chan struct{})
	errs := make(chan error, 8)
	for w := range 8 {
		writers.Go(func() {
			for i := range 500 {
				if err := p.Configure(decoders[(w+i)%2]); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	var torn sync.Map
	for range 8 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				if s := p.Settings(); !slices.Contains(valid, s) {
					torn.Store(s, true)
				}
			}
		})
	}
	writers.Wait()
	close(done)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Configure: %v", err)
	}
	torn.Range(func(k, _ any) bool {
		t.Errorf("Settings read a mixed value %+v", k)
		return true
	})
	if s := p.Settings(); !slices.Contains(valid, s) {
		t.Errorf("final Settings = %+v, want one of %+v", s, valid)
	}
}

// TC-HP-50: Preflight is unavailable off Linux and names the GOOS. Owner WI 3.1. Requirements: FR-3, CON-5.
//
// - Setup: `New(Options{GOOS: "darwin", FS: F-FS(UBU)})`.
// - Steps: `Preflight(ctx)`.
// - Expected: unavailable; reason exactly `the host provider reads Linux /proc and /sys; this host is darwin`; F-FS recorded no reads.
func TestTCHP50_PreflightIsUnavailableOffLinuxAndNames(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	p := New(Options{GOOS: "darwin", FS: ffs})
	st := p.Preflight(context.Background())
	if st.State != apitypes.ProviderUnavailable {
		t.Errorf("State = %q, want %q", st.State, apitypes.ProviderUnavailable)
	}
	if want := "the host provider reads Linux /proc and /sys; this host is darwin"; st.Reason != want {
		t.Errorf("Reason = %q, want %q", st.Reason, want)
	}
	if calls := ffs.recorded(); len(calls) != 0 {
		t.Errorf("F-FS recorded %v, want nothing", calls)
	}
}

// TC-HP-51: Preflight is available on both captures and reports its details. Owner WI 3.1. Requirements: FR-3, CON-5.
//
// - Setup: `GOOS: "linux"`, F-FS(UBU) with `Platform` init systemd; F-FS(ALP) with init openrc; default settings.
// - Steps: `Preflight(ctx)`.
// - Expected: available; details `initSystem` = `systemd` / `openrc`, `sampleIntervalSeconds` = 10, `historyDurationSeconds` = 3600; F-FS recorded `proc/stat`, `proc/meminfo`, `proc/self/mountinfo` and nothing outside the allow-list.
func TestTCHP51_PreflightIsAvailableOnBothCapturesAnd(t *testing.T) {
	for _, c := range []struct{ capture, init string }{{capUBU, "systemd"}, {capALP, "openrc"}} {
		t.Run(c.capture, func(t *testing.T) {
			ffs := fixtureFS(t, c.capture)
			run := newFakeRun(t, c.capture)
			st := newTCProvider(t, c.capture, ffs, run).Preflight(context.Background())
			if st.State != apitypes.ProviderEnabled || st.Reason != "" {
				t.Fatalf("Preflight = %q %q, want enabled", st.State, st.Reason)
			}
			want := map[string]any{
				DetailInitSystem:             c.init,
				DetailSampleIntervalSeconds:  int64(10),
				DetailHistoryDurationSeconds: int64(3600),
			}
			if len(st.Details) != len(want) {
				t.Errorf("Details = %v, want %v", st.Details, want)
			}
			for k, v := range want {
				if st.Details[k] != v {
					t.Errorf("Details[%s] = %#v, want %#v", k, st.Details[k], v)
				}
			}
			assertPreflightReads(t, ffs.recorded(), spPreflightFiles)
			if calls := run.recorded(); len(calls) != 0 {
				t.Errorf("Preflight ran commands: %q", calls)
			}
		})
	}
}

// assertPreflightReads checks that F-FS saw exactly one open of each of
// want, in order, every one allow-listed for reading, and nothing else.
func assertPreflightReads(t *testing.T, calls []fsCall, want []string) {
	t.Helper()
	var got []string
	for _, c := range calls {
		if c.op != "open" || !permits(c.name, accRead) {
			t.Errorf("F-FS recorded %s, outside the Preflight reads", c)
			continue
		}
		got = append(got, c.name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("F-FS opened %q, want %q", got, want)
	}
}

// TC-HP-E11: A single unreadable source does not make the provider unavailable. Owner WI 3.1. Requirements: FR-3, FR-12.
//
// - Setup: F-FS(UBU) with SP-SANDBOX.
// - Steps: `Preflight`.
// - Expected: available.
func TestTCHPE11_ASingleUnreadableSourceDoesNotMake(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			ffs := fixtureFS(t, capture)
			spSandbox(t, ffs)
			run := newFakeRun(t, capture)
			spSandboxRun(run)
			st := newTCProvider(t, capture, ffs, run).Preflight(context.Background())
			if st.State != apitypes.ProviderEnabled {
				t.Errorf("Preflight under SP-SANDBOX = %q %q, want enabled", st.State, st.Reason)
			}
			assertPreflightReads(t, ffs.recorded(), spPreflightFiles)
		})
	}
}

// TC-HP-ERR13: Preflight names the first missing required source. Owner WI 3.1. Requirements: FR-3.
//
// - Setup: F-FS(UBU) variants: `remove("proc/stat")`; `deny("proc/meminfo")`; `remove("proc/self/mountinfo")`; all three removed.
// - Steps: `Preflight` for each.
// - Expected: unavailable with a reason naming `/proc/stat`, `/proc/meminfo`, `/proc/self/mountinfo`, and `/proc/stat` (the first) respectively.
func TestTCHPERR13_PreflightNamesTheFirstMissingRequiredSource(t *testing.T) {
	cases := []struct {
		name   string
		fault  func(*fakeFS)
		reason string
	}{
		{"remove proc/stat", func(f *fakeFS) { f.remove("proc/stat") }, "not present: /proc/stat"},
		{"deny proc/meminfo", func(f *fakeFS) { f.deny("proc/meminfo") }, "permission denied: /proc/meminfo"},
		{"remove proc/self/mountinfo", func(f *fakeFS) { f.remove("proc/self/mountinfo") },
			"not present: /proc/self/mountinfo"},
		{"remove all three", func(f *fakeFS) {
			for _, name := range spPreflightFiles {
				f.remove(name)
			}
		}, "not present: /proc/stat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ffs := fixtureFS(t, capUBU)
			c.fault(ffs)
			st := newTCProvider(t, capUBU, ffs, newFakeRun(t, capUBU)).Preflight(context.Background())
			if st.State != apitypes.ProviderUnavailable {
				t.Errorf("State = %q, want %q", st.State, apitypes.ProviderUnavailable)
			}
			if st.Reason != c.reason {
				t.Errorf("Reason = %q, want %q", st.Reason, c.reason)
			}
		})
	}
}
