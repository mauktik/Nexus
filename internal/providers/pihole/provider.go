// Package pihole is the Pi-hole provider: it reads a Pi-hole v5 host's
// status, lists and DNS records, and manages the one dnsmasq file Nexus owns
// (providers.pihole.dnsFile). Its declarations live in the spec subpackage;
// the version-specific halves live behind the backend seam.
//
// Preflight only reads: it stats the Pi-hole binaries without executing
// them, reads the versions file, asks FTL's text API for its version when
// that file has none, and lstats the DNS file (FR-3, A-26). It never starts
// a unit.
package pihole

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

// The capabilities details Preflight reports when enabled (FR-3, FR-25).
// They are a snapshot as of the last preflight: startup, the last reload or
// the last capabilities read with refresh=true (A-53). Nothing may rely on
// them for a decision; the DNS writes re-check conf-dir themselves.
const (
	DetailBackend              = "backend"
	DetailCoreVersion          = "coreVersion"
	DetailWebVersion           = "webVersion"
	DetailFTLVersion           = "ftlVersion"
	DetailDNSFile              = "dnsFile"
	DetailDNSWrites            = "dnsWrites"
	DetailPreNexusBackupPath   = "preNexusBackupPath"
	DetailPreNexusBackupExists = "preNexusBackupExists"
)

// DNSWritesAvailable is the dnsWrites detail when the DNS writes can run.
const DNSWritesAvailable = "available"

// ftlVersionTimeout bounds FTL's `>version` fallback, so a silent FTL keeps
// preflight well within core.DefaultStartupPreflightTimeout (1 s).
const ftlVersionTimeout = 300 * time.Millisecond

// defaultDNSServer is where FTL answers DNS on a Pi-hole host.
var defaultDNSServer = netip.MustParseAddrPort("127.0.0.1:53")

// Options configures New.
type Options struct {
	Host   string       // Reported in every resource
	Logger *slog.Logger // Nil means slog.Default()
	// Preflight replaces the real check. Nil means the real one. T0 contract
	// tests, on hosts without Pi-hole, set it to serve the provider's routes.
	Preflight func(context.Context) provider.Status
}

// Provider is the pihole provider.
type Provider struct {
	host      string
	log       *slog.Logger
	preflight func(context.Context) provider.Status
	settings  atomic.Pointer[config.PiholeConfig]
	opsDir    atomic.Pointer[string] // Operations' directories; set by UseOpsDir
	// reader is the backend Reader the last preflight selected; the reads
	// use it. Nil before a real preflight, or after one that selected none.
	reader atomic.Pointer[readerBox]

	// Test seams. The zero paths are the production ones under "/".
	paths     v5.Paths
	dnsServer netip.AddrPort
	systemd   func() detach.Availability
	// runner is the backend's Runner. Preflight never uses it; it is nil
	// until the exec runner exists, and a nil Runner makes the reader's
	// re-enable timer unknown rather than failing.
	runner v5.Runner
}

// New returns the provider with the default settings until Configure.
func New(opts Options) *Provider {
	p := &Provider{host: opts.Host, log: opts.Logger, preflight: opts.Preflight,
		dnsServer: defaultDNSServer, systemd: detach.SystemdAvailable}
	if p.log == nil {
		p.log = slog.Default()
	}
	s := config.Default().Providers.Pihole.Settings
	p.settings.Store(&s)
	return p
}

// Spec returns the declarations.
func (p *Provider) Spec() def.ProviderSpec { return spec.Spec() }

// Configure strictly decodes the pihole section. A section it refuses leaves
// the running settings as they were; an accepted one takes effect for every
// later call, so a reload's dnsFile is used without a restart (FR-2).
func (p *Provider) Configure(decode func(into any) error) error {
	s := config.Default().Providers.Pihole.Settings
	if err := decode(&s); err != nil {
		return err
	}
	p.settings.Store(&s)
	return nil
}

// Settings returns the pihole section in force, defaults applied.
func (p *Provider) Settings() config.PiholeConfig { return *p.settings.Load() }

// UseOpsDir sets the directory of the operations' directories. It
// implements provider.OpsDirUser.
func (p *Provider) UseOpsDir(dir string) { p.opsDir.Store(&dir) }

// stateDir is the provider's own state directory, beside the operations'
// directories: <stateDir>/pihole for opsDir <stateDir>/ops (A-34). It is
// empty before UseOpsDir.
func (p *Provider) stateDir() string {
	dir := p.opsDir.Load()
	if dir == nil || *dir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(*dir), "pihole")
}

// backupPath is where the first Nexus write of dnsFile saves the original
// file: <stateDir>/backups/<basename>.pre-nexus. It is empty before
// UseOpsDir.
func (p *Provider) backupPath(dnsFile string) string {
	dir := p.stateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "backups", filepath.Base(dnsFile)+".pre-nexus")
}

func unavailable(format string, args ...any) provider.Status {
	return provider.Status{State: apitypes.ProviderUnavailable, Reason: fmt.Sprintf(format, args...)}
}

// Preflight reports the provider unavailable, with the cause, when systemd
// is missing, a Pi-hole binary is missing or not executable, no Pi-hole
// major version is detected, dnsFile is not a regular file, or a v6 host
// does not load /etc/dnsmasq.d. Otherwise it is enabled, with the backend,
// the versions, the DNS write state and the pre-Nexus backup as details.
func (p *Provider) Preflight(ctx context.Context) provider.Status {
	if p.preflight != nil {
		return p.preflight(ctx)
	}
	if a := p.systemd(); !a.Available {
		return unavailable("pi-hole operations run only under systemd: %s", a.Reason)
	}
	for _, bin := range []struct{ name, path string }{
		{"pihole", p.paths.PiholeCmd()},
		{"pihole-FTL", p.paths.PiholeFTLCmd()},
	} {
		// Stat only: neither binary is ever executed by nexusd (A-26).
		fi, err := os.Stat(p.paths.Host(bin.path))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return unavailable("%s not found at %s", bin.name, bin.path)
		case err != nil:
			return unavailable("%s at %s cannot be checked: %v", bin.name, bin.path, err)
		case !fi.Mode().IsRegular():
			return unavailable("%s at %s is not a regular file", bin.name, bin.path)
		case fi.Mode().Perm()&0o111 == 0:
			return unavailable("%s at %s is not executable", bin.name, bin.path)
		}
	}

	v := backend.Detect(ctx, p.paths, ftlVersionTimeout)
	rd, _, name, err := backend.Select(v, p.paths, p.deps())
	if err != nil {
		p.reader.Store(nil)
		if v.Major != 0 {
			return unavailable("pi-hole major version %d is not supported", v.Major)
		}
		return unavailable("%s", err.Error())
	}
	p.reader.Store(&readerBox{r: rd})

	dnsFile := p.Settings().DNSFile
	if st, ok := p.checkDNSFile(dnsFile); !ok {
		return st
	}

	dnsWrites := DNSWritesAvailable
	switch name {
	case spec.BackendUnsupported:
		if ok, reason := backend.CheckV6(p.paths); !ok {
			return unavailable("%s", reason)
		}
		dnsWrites = backend.ReasonV6DNSWrites
	default:
		if !v5.ConfDirLoads(p.paths, dnsFile) {
			dnsWrites = fmt.Sprintf("dnsmasq does not load %s (no conf-dir in %s)", filepath.Dir(dnsFile), v5.DnsmasqConfFile)
		}
	}

	details := map[string]any{
		DetailBackend:              name,
		DetailCoreVersion:          version(v.Core),
		DetailWebVersion:           version(v.Web),
		DetailFTLVersion:           version(v.FTL),
		DetailDNSFile:              dnsFile,
		DetailDNSWrites:            dnsWrites,
		DetailPreNexusBackupPath:   nil,
		DetailPreNexusBackupExists: false,
	}
	if bk := p.backupPath(dnsFile); bk != "" {
		details[DetailPreNexusBackupPath] = bk
		_, err := os.Lstat(bk)
		details[DetailPreNexusBackupExists] = err == nil
	}
	return provider.Status{State: apitypes.ProviderEnabled, Details: details}
}

// checkDNSFile requires dnsFile to be a regular file, not followed through
// a symlink: the DNS writes replace it in place, and a link would redirect
// them.
func (p *Provider) checkDNSFile(dnsFile string) (provider.Status, bool) {
	fi, err := os.Lstat(p.paths.Host(dnsFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return unavailable("providers.pihole.dnsFile %s does not exist", dnsFile), false
	case err != nil:
		return unavailable("providers.pihole.dnsFile %s cannot be checked: %v", dnsFile, err), false
	case fi.Mode()&fs.ModeSymlink != 0:
		return unavailable("providers.pihole.dnsFile %s is a symlink, not a regular file", dnsFile), false
	case fi.IsDir():
		return unavailable("providers.pihole.dnsFile %s is a directory, not a regular file", dnsFile), false
	case !fi.Mode().IsRegular():
		return unavailable("providers.pihole.dnsFile %s is not a regular file", dnsFile), false
	}
	return provider.Status{}, true
}

// version is a detected version as a details value: the string, or nil
// when unknown.
func version(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// Bindings binds the provider's operations. The provider binds a subset of
// its declarations (A-35): the 11 reads; the mutations are added by their
// own work items, and until then are not served.
func (p *Provider) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.StatusGet, p.status),
		provider.Sync(spec.SummaryGet, p.summary),
		provider.Sync(spec.BlockingGet, p.blocking),
		provider.Sync(spec.DomainsList, p.listDomains),
		provider.Sync(spec.DomainsGet, p.getDomain),
		provider.Sync(spec.AdlistsList, p.listAdlists),
		provider.Sync(spec.AdlistsGet, p.getAdlist),
		provider.Sync(spec.ZonesList, p.listZones),
		provider.Sync(spec.ZonesGet, p.getZone),
		provider.Sync(spec.RecordsList, p.listRecords),
		provider.Sync(spec.RecordsGet, p.getRecord),
	}
}
