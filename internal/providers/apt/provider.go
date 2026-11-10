// Package apt is the apt provider: it reads the host's packages through
// fixed apt and dpkg binaries in the C locale, and refreshes the package
// indexes in a detached systemd unit. Its declarations live in the spec
// subpackage.
//
// Nothing here reads the OS release or branches on it (D-7, ASM-9): the
// only inputs are the machine-stable interfaces apt and dpkg print under
// LC_ALL=C (parse.go). The synchronous reads run inside the nexusd sandbox
// (A-23), so apt builds its cache in memory rather than in /var/cache/apt.
package apt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// The reboot-required marker and its package list, as Ubuntu maintains
// them (FR-14).
const (
	RebootMarker       = "/var/run/reboot-required"
	RebootPackagesFile = "/var/run/reboot-required.pkgs"
)

// RebootMarkerWriters are the installed programs known to create
// RebootMarker. With the marker absent, the answer is false only when one
// of them is present; otherwise nothing on the host would ever create the
// marker, so its absence proves nothing and the answer is unknown.
var RebootMarkerWriters = []string{
	// update-notifier-common: called by kernel and library maintainer
	// scripts on Ubuntu.
	"/usr/share/update-notifier/notify-reboot-required",
	// needrestart's Ubuntu interface writes /run/reboot-required.
	"/usr/share/perl5/NeedRestart/UI/Ubuntu.pm",
}

// maxRebootPackages bounds the package list read from RebootPackagesFile.
const maxRebootPackages = 1000

// Options configures New.
type Options struct {
	Host   string       // Reported in every resource
	Logger *slog.Logger // Command failures are logged here; nil means slog.Default()
	// Preflight replaces the real check. Nil means the real one. T0 contract
	// tests, on hosts without apt, set it to serve the provider's routes.
	Preflight func(context.Context) provider.Status
}

// Provider is the apt provider.
type Provider struct {
	host      string
	log       *slog.Logger
	preflight func(context.Context) provider.Status
	run       runner
	root      string // Prefix of every host file the provider stats or reads; "/" outside tests
	systemd   func() detach.Availability
	settings  atomic.Pointer[config.AptConfig]
	opsDir    atomic.Pointer[string] // Operations' directories; set by UseOpsDir
	probe     lockProbe              // probeLock outside tests
	lockPoll  time.Duration          // Between probes of a held lock

	archMu sync.Mutex
	arch   string // dpkg --print-architecture, once known

	upgradable upgradableCache
}

// New returns the provider with the default settings until Configure.
func New(opts Options) *Provider {
	p := &Provider{host: opts.Host, log: opts.Logger, preflight: opts.Preflight, run: execRun, root: "/",
		systemd: detach.SystemdAvailable, probe: probeLock, lockPoll: lockPoll}
	if p.log == nil {
		p.log = slog.Default()
	}
	s := config.Default().Providers.Apt.Settings
	p.settings.Store(&s)
	return p
}

// Spec returns the declarations.
func (p *Provider) Spec() def.ProviderSpec { return spec.Spec() }

// Configure strictly decodes the apt section. A section it refuses leaves
// the running settings as they were.
func (p *Provider) Configure(decode func(into any) error) error {
	s := config.Default().Providers.Apt.Settings
	if err := decode(&s); err != nil {
		return err
	}
	p.settings.Store(&s)
	return nil
}

// Settings returns the apt section in force, defaults applied.
func (p *Provider) Settings() config.AptConfig { return *p.settings.Load() }

// UseOpsDir sets the directory of the operations' directories, where
// Prepare writes pre.json and Finalize reads it and the output. It
// implements provider.OpsDirUser.
func (p *Provider) UseOpsDir(dir string) { p.opsDir.Store(&dir) }

func (p *Provider) opDir(id string) (string, error) {
	dir := p.opsDir.Load()
	if dir == nil || *dir == "" {
		return "", errors.New("apt: no operations directory: UseOpsDir was not called")
	}
	return filepath.Join(*dir, id), nil
}

// Preflight checks the fixed binaries, then systemd, which refresh and every
// later mutation need: there is no fallback to a child of nexusd (CON-4).
// Its details carry the native architecture and the reboot-required state,
// so capabilities with refresh=true re-reads them.
func (p *Provider) Preflight(ctx context.Context) provider.Status {
	if p.preflight != nil {
		return p.preflight(ctx)
	}
	for _, bin := range Binaries {
		fi, err := os.Stat(p.hostPath(bin))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			return provider.Status{State: apitypes.ProviderUnavailable,
				Reason: fmt.Sprintf("%s is missing or not executable; the apt provider needs apt and dpkg", bin)}
		}
	}
	if a := p.systemd(); !a.Available {
		return provider.Status{State: apitypes.ProviderUnavailable,
			Reason: "apt operations run only under systemd: " + a.Reason}
	}
	arch, err := p.architecture(ctx)
	if err != nil {
		return provider.Status{State: apitypes.ProviderUnavailable, Reason: err.Error()}
	}
	state, pkgs := p.rebootState()
	return provider.Status{State: apitypes.ProviderEnabled, Details: map[string]any{
		spec.DetailArchitecture:   arch,
		spec.DetailRebootRequired: state,
		spec.DetailRebootPackages: pkgs,
	}}
}

// Bindings binds the read operations, refresh, upgrade, install and remove.
func (p *Provider) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.List, p.list),
		provider.Sync(spec.Get, p.get),
		provider.Sync(spec.UpgradableList, p.listUpgradable),
		provider.Detached(spec.Refresh, provider.DetachedSpec[spec.RefreshInput, spec.RefreshResult]{
			Lane:       aptLane[spec.RefreshInput],
			Prepare:    p.prepareRefresh,
			Finalize:   p.finalizeRefreshBusy,
			LostDetail: lostRefresh,
		}),
		p.upgradeBinding(),
		p.installBinding(),
		p.removeBinding(),
	}
}

// prepareRefresh builds apt-get update's unit. It skips the dpkg lock
// probe, the audit and the snapshot the other mutations take, because
// apt-get update does not use the dpkg frontend lock (S-2) and changes no
// package. KillMode=process, so a stop of the unit never cuts apt.
func (p *Provider) prepareRefresh(context.Context, spec.RefreshInput, *provider.Run) (detach.Spec, error) {
	return detach.Spec{
		Argv:       RefreshArgv(),
		Env:        Env(),
		Dir:        "/",
		KillMode:   detach.KillProcess,
		MaxRuntime: p.Settings().MaxRuntime,
	}, nil
}

// finalizeRefresh states the outcome from exit.json alone: exit 0 is
// success, anything else apt-failed with apt-get's status.
// finalizeRefreshBusy (lane.go) runs first, for a held lock.
func finalizeRefresh(_ context.Context, _ spec.RefreshInput, x detach.Exit, _ *provider.Run) (spec.RefreshResult, error) {
	// No stderr: a held lock is finalizeRefreshBusy's, which runs first.
	if perr := aptFailure("apt-get update", x, nil); perr != nil {
		return spec.RefreshResult{}, perr
	}
	return spec.RefreshResult{ActionResult: apitypes.ActionResult{Changed: true}, ExitCode: 0}, nil
}

func lostRefresh(*provider.Run, spec.RefreshInput) string {
	return "apt-get update may not have finished; the package indexes are as it left them, and a new refresh repairs them"
}

// architecture is dpkg's native architecture, read once.
func (p *Provider) architecture(ctx context.Context) (string, error) {
	p.archMu.Lock()
	defer p.archMu.Unlock()
	if p.arch != "" {
		return p.arch, nil
	}
	res, err := p.run(ctx, cmdArchitecture())
	if err != nil || res.ExitCode != 0 {
		return "", fmt.Errorf("%s --print-architecture failed: %s", Dpkg, failure(res, err))
	}
	arch := strings.TrimSpace(string(res.Stdout))
	if !archRE.MatchString(arch) {
		return "", fmt.Errorf("%s --print-architecture printed %q, not an architecture", Dpkg, arch)
	}
	p.arch = arch
	return arch, nil
}

// rebootState reads the reboot-required marker: true with the packages that
// asked for it, false when it is absent and a writer is installed, and
// unknown when no writer is.
func (p *Provider) rebootState() (string, []string) {
	pkgs := []string{}
	if _, err := os.Stat(p.hostPath(RebootMarker)); err == nil {
		b, err := os.ReadFile(p.hostPath(RebootPackagesFile))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			p.log.Warn("reading the reboot-required package list", "error", err.Error())
		}
		seen := map[string]bool{}
		for line := range lines(b) {
			name := strings.TrimSpace(line)
			if pkgNameRE.MatchString(name) && !seen[name] && len(pkgs) < maxRebootPackages {
				seen[name] = true
				pkgs = append(pkgs, name)
			}
		}
		return spec.RebootTrue, pkgs
	}
	for _, w := range RebootMarkerWriters {
		if _, err := os.Stat(p.hostPath(w)); err == nil {
			return spec.RebootFalse, pkgs
		}
	}
	return spec.RebootUnknown, pkgs
}

func (p *Provider) hostPath(path string) string {
	if p.root == "/" {
		return path
	}
	return filepath.Join(p.root, path)
}
