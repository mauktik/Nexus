// Package host is the host provider (the "system" provider): a read-only
// overview of the Linux host nexusd runs on, with a background sampler for
// rates and an in-memory metrics history. Its declarations live in the spec
// subpackage.
//
// Every file of the package keeps four rules:
//   - The allow-list. Every file access goes through Options.FS wrapped by
//     the allow-list (allowlist.go): each Open, ReadDir, Stat and ReadLink is
//     checked against a fixed pattern and its access kind (read, list,
//     stat-only, link) before it reaches the FS. The one path access outside
//     it is the Statfs seam, which makes only stat and statfs, only on mount
//     points from mountinfo, always under bounded. The OS comes from
//     internal/platform, never from a file read here.
//   - Never mutate. The provider only reads. It writes no file, signals no
//     process and changes no host setting; the only commands it runs are the
//     fixed read-only queries of the clock and of failed services, through
//     svcctl.Run, with no part of a request in their argv.
//   - No @clock. It never makes a syscall of systemd's @clock group: clock
//     synchronization comes from timedatectl on systemd and chronyc
//     elsewhere (D-8).
//   - /proc/1/mountinfo. Mounts and their read-only flag come from the host
//     namespace's /proc/1/mountinfo. When that is unreadable the provider
//     uses /proc/self/mountinfo and reports readOnly unavailable rather than
//     wrong. statfs runs on a mount point only when /proc/self/mountinfo
//     shows a lookup of it in nexusd's namespace reaches the same
//     filesystem (D-9).
package host

import (
	"context"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/host/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// The keys of the preflight's details, reported in capabilities.
const (
	DetailInitSystem             = "initSystem"
	DetailSampleIntervalSeconds  = "sampleIntervalSeconds"
	DetailHistoryDurationSeconds = "historyDurationSeconds"
)

// preflightFiles are the sources the provider cannot work without, in the
// order Preflight reads them (FR-3).
var preflightFiles = []string{"proc/stat", "proc/meminfo", "proc/self/mountinfo"}

// Options configures New. Every nil or zero seam means the real one.
type Options struct {
	Host   string       // Reported in every resource
	Logger *slog.Logger // Nil means slog.Default()
	// Platform is the host's init system and OS. Nil means
	// platform.Current().
	Platform *platform.Info
	// GOOS is the operating system Preflight checks. Empty means
	// runtime.GOOS; the provider is available on linux only.
	GOOS string
	// FS is the host's root. Nil means os.DirFS("/"). It is always wrapped
	// by the allow-list.
	FS fs.FS
	// Statfs is stat then statfs of one mount point, in one call. Nil means
	// the real syscalls on linux and an error elsewhere.
	Statfs func(path string) (Usage, error)
	// Uname returns uname(2)'s machine field. Nil means the real call on
	// linux.
	Uname func() (machine string, err error)
	// Addrs returns every interface's addresses. Nil means net.Interfaces
	// and their Addrs.
	Addrs func() (map[string][]netip.Prefix, error)
	// Run runs one fixed command. Nil means svcctl.Run.
	Run func(ctx context.Context, argv []string) (svcctl.Result, error)
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Ticker makes the sampler's ticker. Nil means a time.Ticker.
	Ticker func(d time.Duration) Ticker
	// PageSize is the size of a memory page in bytes. Zero means
	// os.Getpagesize().
	PageSize int
}

// Ticker is the sampler's ticker: time.Ticker's methods, with its channel
// behind a method so a test can replace it.
type Ticker interface {
	C() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

// timeTicker is the real Ticker.
type timeTicker struct{ t *time.Ticker }

func newTimeTicker(d time.Duration) Ticker { return timeTicker{t: time.NewTicker(d)} }

func (t timeTicker) C() <-chan time.Time   { return t.t.C }
func (t timeTicker) Reset(d time.Duration) { t.t.Reset(d) }
func (t timeTicker) Stop()                 { t.t.Stop() }

// Settings are the effective settings of the providers.host section. The
// checks on them are config.validate's.
type Settings struct {
	SampleInterval  time.Duration // How often the sampler reads the counters
	HistoryDuration time.Duration // How far back the metrics history goes
	SourceTimeout   time.Duration // The bound on one blocking source
}

// Provider is the host provider.
type Provider struct {
	host string
	log  *slog.Logger
	plat platform.Info
	goos string

	// The resolved seams. The command runner is exec, not run: the
	// sampler's run is the provider's current sampler run.
	uname     func() (string, error)
	addrs     func() (map[string][]netip.Prefix, error)
	exec      func(ctx context.Context, argv []string) (svcctl.Result, error)
	now       func() time.Time
	newTicker func(d time.Duration) Ticker
	pageSize  int

	settings atomic.Pointer[Settings]

	src *source
}

// New returns the provider with the default settings until Configure. It
// starts nothing and touches no host file.
func New(opts Options) *Provider {
	p := &Provider{host: opts.Host, log: opts.Logger, goos: opts.GOOS, uname: opts.Uname, addrs: opts.Addrs,
		exec: opts.Run, now: opts.Now, newTicker: opts.Ticker, pageSize: opts.PageSize}
	if p.log == nil {
		p.log = slog.Default()
	}
	if opts.Platform != nil {
		p.plat = *opts.Platform
	} else {
		p.plat = platform.Current()
	}
	if p.goos == "" {
		p.goos = runtime.GOOS
	}
	if p.uname == nil {
		p.uname = defaultUname
	}
	if p.addrs == nil {
		p.addrs = defaultAddrs
	}
	if p.exec == nil {
		p.exec = svcctl.Run
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.newTicker == nil {
		p.newTicker = newTimeTicker
	}
	if p.pageSize == 0 {
		p.pageSize = os.Getpagesize()
	}
	fsys := opts.FS
	if fsys == nil {
		fsys = os.DirFS("/")
	}
	s := settingsOf(config.Default().Providers.Host.Settings)
	p.settings.Store(&s)
	p.src = newSource(fsys, s.SourceTimeout, opts.Statfs)
	return p
}

func settingsOf(c config.HostProviderConfig) Settings {
	return Settings{SampleInterval: c.SampleInterval, HistoryDuration: c.HistoryDuration, SourceTimeout: c.SourceTimeout}
}

// Spec returns the declarations.
func (p *Provider) Spec() def.ProviderSpec { return spec.Spec() }

// Configure strictly decodes the providers.host section over the defaults
// and stores the settings in one atomic swap. A section it refuses leaves
// the running settings as they were. It starts nothing, so it is safe to
// call again at any time next to running handlers; the sampler picks the
// new settings up on its next tick. The section's checks are
// config.validate's, so validate-config reports them with locations.
func (p *Provider) Configure(decode func(into any) error) error {
	c := config.Default().Providers.Host.Settings
	if err := decode(&c); err != nil {
		return err
	}
	s := settingsOf(c)
	p.settings.Store(&s)
	return nil
}

// Settings returns the settings in force.
func (p *Provider) Settings() Settings { return *p.settings.Load() }

// Preflight reports the provider available when the host is Linux and
// /proc/stat, /proc/meminfo and /proc/self/mountinfo are readable, and
// unavailable naming the GOOS or the first of them that is not (FR-3). Off
// Linux it reads nothing. Any other source that fails later makes only its
// own fields unavailable (FR-12), never the provider.
func (p *Provider) Preflight(context.Context) provider.Status {
	if p.goos != "linux" {
		return provider.Status{State: apitypes.ProviderUnavailable,
			Reason: "the host provider reads Linux /proc and /sys; this host is " + p.goos}
	}
	for _, name := range preflightFiles {
		if _, err := p.src.read(name); err != nil {
			return provider.Status{State: apitypes.ProviderUnavailable, Reason: reasonFor(name, err)}
		}
	}
	s := p.Settings()
	return provider.Status{State: apitypes.ProviderEnabled, Details: map[string]any{
		DetailInitSystem:             string(p.plat.Init),
		DetailSampleIntervalSeconds:  int64(s.SampleInterval / time.Second),
		DetailHistoryDurationSeconds: int64(s.HistoryDuration / time.Second),
	}}
}

// Bindings binds the operations. They come with WI 5.1; until then the
// provider serves none.
func (p *Provider) Bindings() []provider.Binding { return nil }
