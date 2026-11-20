// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package services is the services provider: it lists the host's system
// services, reads one service and its recent logs, starts, stops and
// restarts the services the root-owned providers.services.control allow-list
// names, and reboots the host under guards. Its declarations live in the
// spec subpackage.
//
// The Provider runs inside nexusd, which on systemd is sandboxed, so it only
// reads: through svcctl (systemctl, rc-status, rc-update, rc-service, the
// journal and static reads of OpenRC files) and through its own reads of
// /proc, /run and /etc. Actions run in detached operations, in the hidden
// `nexusd services-helper` (D-5). No service is ever acted on when the
// protected set names it (D-6, protect.go), and the provider never manages
// a hypervisor (FR-12, CON-6).
package services

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// Options configures New. Every nil or empty seam means the real one.
type Options struct {
	Host   string       // The capabilities host name, reported in every resource
	Logger *slog.Logger // Nil means slog.Default()
	// Platform is the host's init system and OS. Nil means
	// platform.Current().
	Platform *platform.Info
	// GOOS is the operating system Preflight checks. Empty means
	// runtime.GOOS; the provider is available on linux only.
	GOOS string
	// Run and RunLines are svcctl's seams (svcctl.Options.Run and RunLines).
	Run      func(ctx context.Context, argv []string) (svcctl.Result, error)
	RunLines func(ctx context.Context, argv []string, maxLine int, onLine func([]byte) bool) (svcctl.Result, error)
	// ProcRoot ("" = /proc) is where the provider and the reboot helper
	// read sys/kernel/random/boot_id, sys/kernel/osrelease, uptime,
	// self/mountinfo and <pid>/cgroup.
	ProcRoot string
	// RunRoot ("" = /run) is the OpenRC state and the container markers
	// under /run.
	RunRoot string
	// EtcRoot ("" = /etc) is the OpenRC init.d and conf.d tree and /etc/pve.
	EtcRoot string
	// HostFS (nil = os.DirFS("/")) is every other host file: the tools,
	// the container markers at the root, the Proxmox package list, and what
	// host.RebootRequiredNow reads (as host.Options{FS: HostFS}). A test
	// that sets it and ProcRoot derives both from one root.
	HostFS fs.FS
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Executable is the nexusd that runs the helper. Empty means
	// os.Executable.
	Executable string
}

// Provider is the services provider.
type Provider struct {
	host       string
	log        *slog.Logger
	plat       platform.Info
	goos       string
	run        func(ctx context.Context, argv []string) (svcctl.Result, error)
	runLines   func(ctx context.Context, argv []string, maxLine int, onLine func([]byte) bool) (svcctl.Result, error)
	procRoot   string
	runRoot    string
	etcRoot    string
	hostFS     fs.FS
	now        func() time.Time
	executable string

	// mu serializes the writers of snap and res: Configure and Preflight.
	// Readers take snap without it.
	mu   sync.Mutex
	res  *resolution // The host's last resolved protections; nil before Preflight
	snap atomic.Pointer[snapshot]

	opsDir atomic.Pointer[string]
	daemon atomic.Pointer[daemonRef]
	ops    atomic.Pointer[operationsRef]
}

type daemonRef struct{ d provider.Daemon }

type operationsRef struct{ o provider.Operations }

// snapshot is one settings section and the protected set resolved with it.
// A reader takes the whole snapshot once, so controllable and protected
// always come from the same settings and the same resolution.
type snapshot struct {
	cfg   config.ServicesConfig
	allow map[string]config.ServiceControl
	prot  *protectedSet
	// unresolved is why the services nexusd depends on could not be looked
	// up ("" when they were, or the lookup answered partially).
	unresolved string
}

// controllable reports whether start, stop and restart may act on name
// (canonical is its unit Id without .service, or ""): it is in the
// allow-list and not protected.
func (s *snapshot) controllable(name, canonical string) bool {
	if _, ok := s.allow[name]; !ok {
		return false
	}
	prot, _ := s.prot.protected(name, canonical)
	return !prot
}

// New returns the provider with the default settings until Configure.
func New(opts Options) *Provider {
	p := &Provider{host: opts.Host, log: opts.Logger, goos: opts.GOOS, run: opts.Run, runLines: opts.RunLines,
		procRoot: opts.ProcRoot, runRoot: opts.RunRoot, etcRoot: opts.EtcRoot, hostFS: opts.HostFS, now: opts.Now,
		executable: opts.Executable}
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
	if p.procRoot == "" {
		p.procRoot = "/proc"
	}
	if p.runRoot == "" {
		p.runRoot = "/run"
	}
	if p.etcRoot == "" {
		p.etcRoot = "/etc"
	}
	if p.hostFS == nil {
		p.hostFS = os.DirFS("/")
	}
	if p.now == nil {
		p.now = time.Now
	}
	p.snap.Store(newSnapshot(config.Default().Providers.Services.Settings, nil, p.plat.Init))
	return p
}

// Spec returns the declarations.
func (p *Provider) Spec() def.ProviderSpec { return spec.Spec() }

// Configure strictly decodes the services section, with its defaults, and
// swaps the settings and the protected set re-resolved with them in one
// atomic store. It runs nothing: the dynamic protections are the last
// preflight's, and the reload's own preflight resolves them again. A
// section it refuses leaves the running settings as they were. The
// section's checks are config.validate's, so validate-config reports them
// with locations.
func (p *Provider) Configure(decode func(into any) error) error {
	c := config.Default().Providers.Services.Settings
	if err := decode(&c); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snap.Store(newSnapshot(c, p.res, p.plat.Init))
	return nil
}

// snapshot is the settings and protected set in force.
func (p *Provider) snapshot() *snapshot { return p.snap.Load() }

// protected reports whether name, or its canonical unit Id (without
// .service; "" when unknown), is protected, and why (D-6).
func (p *Provider) protected(name, canonical string) (bool, string) {
	return p.snapshot().prot.protected(name, canonical)
}

// newSnapshot is c, its allow-list by name, and the protected set of init
// with res's dynamic protections. An entry with no actions or no timeout
// takes the defaults, as Section.Decode fills them.
func newSnapshot(c config.ServicesConfig, res *resolution, init platform.InitSystem) *snapshot {
	control := make([]config.ServiceControl, len(c.Control))
	allow := make(map[string]config.ServiceControl, len(c.Control))
	for i, e := range c.Control {
		if e.Actions == nil {
			e.Actions = spec.Actions
		}
		// Clone, not append to nil: an empty list must stay non-nil, or the
		// next snapshot (Preflight rebuilds it) would default it to all three.
		e.Actions = slices.Clone(e.Actions)
		if e.Timeout == 0 {
			e.Timeout = spec.DefaultControlTimeout
		}
		control[i] = e
		allow[e.Name] = e
	}
	c.Control = control
	snap := &snapshot{cfg: c, allow: allow, prot: newProtectedSet(init, res)}
	if res != nil {
		snap.unresolved = res.lookupErr
	}
	return snap
}

// UseOpsDir sets the directory of the operations' directories. It
// implements provider.OpsDirUser.
func (p *Provider) UseOpsDir(dir string) { p.opsDir.Store(&dir) }

// UseDaemon keeps the daemon serving this provider, for the reboot's
// config check. It implements provider.DaemonUser.
func (p *Provider) UseDaemon(d provider.Daemon) { p.daemon.Store(&daemonRef{d: d}) }

// UseOperations keeps the operation manager's view of the non-terminal
// operations, for the reboot's guard. It implements provider.OperationsUser.
func (p *Provider) UseOperations(o provider.Operations) { p.ops.Store(&operationsRef{o: o}) }

// svcctlOptions are svcctl's options: the provider's seams and roots, so
// every svcctl read sees the same tree as the provider's own reads.
func (p *Provider) svcctlOptions() svcctl.Options {
	return svcctl.Options{Run: p.run, RunLines: p.runLines, ProcRoot: p.procRoot, RunRoot: p.runRoot,
		EtcRoot: p.etcRoot}
}
