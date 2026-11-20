// Package storage is the storage provider: ZFS pools, datasets and
// snapshots, Proxmox vzdump backup archives and Time Machine shares, read
// inside nexusd's sandbox. Its declarations live in the spec subpackage.
// Wherever internal/storage is also in scope, it is imported as
// storageprov (CON-5).
//
// Every file of the package keeps three rules:
//   - Only zpool and zfs. The only programs it runs are the zpool and zfs
//     that Preflight resolved, through zfs.Tools (fixed argv built by
//     zfs.Argv, bounded, at most four processes). No hypervisor tool and no
//     network client (FR-20, CON-6).
//   - Bounded filesystem access. Every access to a configured directory runs
//     under bounded (D-15), so a path on a suspended pool answers its reason
//     at once instead of holding a request.
//   - Configured paths only. The directories it reads are the configured
//     vzdumpPaths and timeMachine.paths, each opened as an os.Root (CON-7).
package storage

import (
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/vzdump"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// Usage is what statfs(2) reports of a Time Machine share: block counts in
// units of Bsize bytes (f_frsize).
type Usage struct {
	Blocks, Bfree, Bavail, Bsize uint64
}

// Root is an opened configured directory: the readers' Opener (vzdump's
// and timemachine's, which have the same methods) and its Close. *os.Root
// is one.
type Root interface {
	vzdump.Opener
	Close() error
}

// Options configures New. Every nil or zero seam means the real one.
type Options struct {
	Host   string       // Reported in every resource
	Logger *slog.Logger // Nil means slog.Default()
	// GOOS is the operating system Preflight checks. Empty means
	// runtime.GOOS; the provider is available on linux only.
	GOOS string
	// FS is the host's root, for the Proxmox markers and the stat of the
	// tools and /dev/zfs. Nil means os.DirFS("/").
	FS fs.FS
	// Run runs one zpool or zfs argv. Nil means zfs.Exec.
	Run zfs.Runner
	// Statfs is statfs(2) of a configured share. Nil means the real call on
	// linux and an error elsewhere.
	Statfs func(path string) (Usage, error)
	// OpenRoot opens a configured directory. Nil means os.OpenRoot. The
	// provider opens and closes a root inside each bounded flight.
	OpenRoot func(path string) (Root, error)
	// Now is the clock of the feature re-check (D-6). Nil means time.Now.
	Now func() time.Time
	// Executable is the running nexusd, which runs the storage helper. Nil
	// means os.Executable.
	Executable func() (string, error)
	// Tick is the Background's wait of d (sizeInterval, read before each
	// wait): it returns the channel that fires and the function that stops
	// it. Nil means a time.Timer.
	Tick func(d time.Duration) (<-chan time.Time, func())
	// ObserveWalk, when set, is called as each Background size walk of a
	// share starts (start true) and ends (D-12).
	ObserveWalk func(share string, start bool)
}

// Settings are the effective settings of the providers.storage section.
// The checks on them are config.validate's.
type Settings struct {
	config.StorageConfig
}

// Provider is the storage provider.
type Provider struct {
	host string
	log  *slog.Logger
	goos string

	// The resolved seams.
	fsys        fs.FS
	run         zfs.Runner
	statfs      func(path string) (Usage, error)
	openRoot    func(path string) (Root, error)
	now         func() time.Time
	executable  func() (string, error)
	tick        func(d time.Duration) (<-chan time.Time, func())
	observeWalk func(share string, start bool)

	settings atomic.Pointer[Settings]

	// fl is the in-flight table of bounded filesystem accesses (D-15).
	fl *flights
	// feats is the per-feature availability with its re-check (D-6).
	feats *features
	// zfs is what the last zfs check found: the tools, the format and the
	// pools. Nil until the first check.
	zfs atomic.Pointer[zfsState]
	// reads is what the ZFS reads keep between calls: the last-scrub memo,
	// the last dataset listing and the text-form fallbacks (pools.go).
	reads zfsReads
	// logs is the log-status memo of the backup reads (D-13).
	logs logMemo
	// walks is the Background size walks' table and their results (D-12).
	// It lives on the provider, so it outlives a RunBackground call.
	walks walkTable
	// walkEntries bounds the entries of one share's size walk; 0 means
	// spec.MaxSizeWalkEntries. Tests set it lower.
	walkEntries int
	// opsDir is the operations directory UseOpsDir injected (detached.go).
	opsDir atomic.Pointer[string]

	// toolsMu guards tools, which is kept while the resolved paths stay
	// the same, so its in-flight table outlives a preflight: a second
	// preflight joins a stuck zpool list instead of starting another.
	toolsMu    sync.Mutex
	tools      *zfs.Tools
	toolsPaths zfs.Paths
}

// New returns the provider with the default settings until Configure. It
// starts nothing and touches no host file.
func New(opts Options) *Provider {
	p := &Provider{host: opts.Host, log: opts.Logger, goos: opts.GOOS, fsys: opts.FS, run: opts.Run,
		statfs: opts.Statfs, openRoot: opts.OpenRoot, now: opts.Now, executable: opts.Executable,
		tick: opts.Tick, observeWalk: opts.ObserveWalk}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.goos == "" {
		p.goos = runtime.GOOS
	}
	if p.fsys == nil {
		p.fsys = os.DirFS("/")
	}
	if p.run == nil {
		p.run = zfs.Exec
	}
	if p.statfs == nil {
		p.statfs = defaultStatfs
	}
	if p.openRoot == nil {
		p.openRoot = openRoot
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.executable == nil {
		p.executable = os.Executable
	}
	s := Settings{config.Default().Providers.Storage.Settings}
	p.settings.Store(&s)
	p.fl = newFlights(p.commandTimeout)
	p.feats = newFeatures()
	return p
}

// openRoot is the default OpenRoot seam.
func openRoot(path string) (Root, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Spec returns the declarations.
func (p *Provider) Spec() def.ProviderSpec { return spec.Spec() }

// Configure strictly decodes the providers.storage section over the
// defaults and stores the settings in one atomic swap. A section it refuses
// leaves the running settings as they were. It starts nothing, so it is
// safe to call at any time next to running handlers: a request reads the
// settings once, at its start, and keeps that snapshot to its end. The
// section's checks are config.validate's, so validate-config reports them
// with locations.
func (p *Provider) Configure(decode func(into any) error) error {
	c := config.Default().Providers.Storage.Settings
	if err := decode(&c); err != nil {
		return err
	}
	p.settings.Store(&Settings{c})
	return nil
}

// Settings returns the settings in force.
func (p *Provider) Settings() Settings { return *p.settings.Load() }

// commandTimeout bounds one zpool or zfs call and one bounded filesystem
// access, read on each call so a Configure applies to the next one.
func (p *Provider) commandTimeout() time.Duration { return p.Settings().CommandTimeout }

// toolsFor returns the runner over paths, keeping the current one (and its
// in-flight table) while the paths stay the same.
func (p *Provider) toolsFor(paths zfs.Paths) *zfs.Tools {
	p.toolsMu.Lock()
	defer p.toolsMu.Unlock()
	if p.tools == nil || p.toolsPaths != paths {
		p.tools, p.toolsPaths = zfs.NewTools(paths, p.run, p.commandTimeout), paths
	}
	return p.tools
}

var _ provider.Provider = (*Provider)(nil)
