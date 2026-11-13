// Package nginx is the nginx provider: it reports the host's packaged nginx
// service and lists and reads the site configuration files of one include
// directory. Its declarations live in the spec subpackage.
//
// Everything here runs inside nexusd, which on systemd is sandboxed with a
// read-only file system, so this code only reads: the service state through
// svcctl, `nginx -v`, and lstat and reads of includeDir. It never writes
// includeDir and never runs `nginx -t`; changes run in detached operations.
// The service, the binary and the directory come only from code and the
// root-owned configuration (CON-3), never from a request.
package nginx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// The keys of the preflight's details, reported in capabilities.
const (
	DetailInitSystem     = "initSystem"
	DetailService        = "service"
	DetailIncludeDir     = "includeDir"
	DetailIncludeSuffix  = "includeSuffix"
	DetailDisabledSuffix = "disabledSuffix"
	DetailVersion        = "version"
)

// Options configures New. Every nil seam means the real one.
type Options struct {
	Host   string       // Reported in every resource
	Logger *slog.Logger // Nil means slog.Default()
	// Platform is the host's init system and OS. Nil means
	// platform.Current().
	Platform *platform.Info
	// Services controls the nginx service. Nil means svcctl for Platform's
	// init system, each call bounded by stateTimeout.
	Services svcctl.Controller
	// Detach reports whether detached operations can run here. Nil means
	// detach.Available.
	Detach func() detach.Availability
	// Nginx is the nginx binary. Empty means DefaultNginx.
	Nginx string
	// Preflight replaces the real check. T0 contract tests, on hosts
	// without nginx, set it to serve the provider's routes.
	Preflight func(context.Context) provider.Status
}

// Provider is the nginx provider.
type Provider struct {
	host      string
	log       *slog.Logger
	plat      platform.Info
	services  svcctl.Controller // Nil: built from plat per call
	detach    func() detach.Availability
	nginx     string
	preflight func(context.Context) provider.Status

	settings atomic.Pointer[Settings]
	opsDir   atomic.Pointer[string] // Operations' directories; set by UseOpsDir

	versions       versionCache
	versionTimeout time.Duration // VersionTimeout outside tests
}

// New returns the provider with the default settings until Configure.
func New(opts Options) *Provider {
	p := &Provider{host: opts.Host, log: opts.Logger, services: opts.Services, detach: opts.Detach,
		nginx: opts.Nginx, preflight: opts.Preflight, versionTimeout: VersionTimeout}
	if p.log == nil {
		p.log = slog.Default()
	}
	if opts.Platform != nil {
		p.plat = *opts.Platform
	} else {
		p.plat = platform.Current()
	}
	if p.detach == nil {
		p.detach = detach.Available
	}
	if p.nginx == "" {
		p.nginx = DefaultNginx
	}
	s := effectiveSettings(config.Default().Providers.Nginx.Settings, p.plat.OS)
	p.settings.Store(&s)
	return p
}

// Spec returns the declarations.
func (p *Provider) Spec() def.ProviderSpec { return spec.Spec() }

// Configure strictly decodes the nginx section and stores the effective
// settings, includeDir resolved, in one atomic swap. A section it refuses
// leaves the running settings as they were. The section's own checks (FR-2)
// are config.validate's, so validate-config reports them with locations.
func (p *Provider) Configure(decode func(into any) error) error {
	c := config.Default().Providers.Nginx.Settings
	if err := decode(&c); err != nil {
		return err
	}
	s := effectiveSettings(c, p.plat.OS)
	p.settings.Store(&s)
	return nil
}

// Settings returns the effective settings in force.
func (p *Provider) Settings() Settings { return *p.settings.Load() }

// UseOpsDir sets the directory of the operations' directories, where the
// detached operations keep their input and result. It implements
// provider.OpsDirUser.
func (p *Provider) UseOpsDir(dir string) { p.opsDir.Store(&dir) }

// controller is the service controller: the injected one, or svcctl for the
// host's init system with each call bounded by stateTimeout.
func (p *Provider) controller(s Settings) (svcctl.Controller, error) {
	if p.services != nil {
		return p.services, nil
	}
	return svcctl.New(p.plat.Init, svcctl.Options{Timeout: s.StateTimeout})
}

// Preflight reports the provider available only when, in order: the nginx
// binary is a regular executable file; detached operations can run, which
// every change needs; the init system knows the service; and includeDir is a
// directory itself, not a symlink to one (FR-2a). Each failure names what is
// missing. Nothing here is cached across calls, so a reload (SIGHUP) or a
// call-time re-check notices nginx installed after nexusd started.
func (p *Provider) Preflight(ctx context.Context) provider.Status {
	if p.preflight != nil {
		return p.preflight(ctx)
	}
	s := p.Settings()
	unavailable := func(format string, args ...any) provider.Status {
		return provider.Status{State: apitypes.ProviderUnavailable, Reason: fmt.Sprintf(format, args...)}
	}

	fi, err := os.Stat(p.nginx)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return unavailable("the nginx binary %s is missing: install the distribution's nginx package", p.nginx)
	case err != nil:
		return unavailable("the nginx binary %s cannot be checked: %v", p.nginx, err)
	case !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0:
		return unavailable("the nginx binary %s is not executable: it must be an executable regular file", p.nginx)
	}

	if a := p.detach(); !a.Available {
		return unavailable("nginx changes run only as detached operations: %s", a.Reason)
	}

	ctl, err := p.controller(s)
	if err != nil {
		return unavailable("nginx needs a supported init system: %v", err)
	}
	st, err := ctl.Status(ctx, s.Service)
	if err != nil {
		return unavailable("the state of service %s cannot be read from %s: %v", s.Service, ctl.Init(), err)
	}
	if !st.Known {
		return unavailable("service %s is not known to %s: install nginx's service, or set providers.nginx.service",
			s.Service, ctl.Init())
	}

	di, err := os.Lstat(s.IncludeDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return unavailable("includeDir %s does not exist", s.IncludeDir)
	case err != nil:
		return unavailable("includeDir %s cannot be checked: %v", s.IncludeDir, err)
	case di.Mode()&fs.ModeSymlink != 0:
		return unavailable("includeDir %s is a symlink: it must be the directory itself", s.IncludeDir)
	case !di.IsDir():
		return unavailable("includeDir %s is not a directory", s.IncludeDir)
	}

	return provider.Status{State: apitypes.ProviderEnabled, Details: map[string]any{
		DetailInitSystem:     string(ctl.Init()),
		DetailService:        s.Service,
		DetailIncludeDir:     s.IncludeDir,
		DetailIncludeSuffix:  s.IncludeSuffix,
		DetailDisabledSuffix: s.DisabledSuffix,
		DetailVersion:        p.versions.get(ctx, p.nginx, p.versionTimeout),
	}}
}

// Bindings binds the synchronous reads. Each detached operation's work item
// appends its binding.
func (p *Provider) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.Status, p.status),
		provider.Sync(spec.List, p.list),
		provider.Sync(spec.Get, p.get),
		provider.Sync(spec.Content, p.content),
	}
}
