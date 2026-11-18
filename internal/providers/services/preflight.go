package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// The keys of the preflight's details, reported in capabilities.
const (
	DetailInitSystem                 = "initSystem"
	DetailLogSource                  = "logSource"
	DetailControlAllowList           = "controlAllowList"
	DetailRebootEnabled              = "rebootEnabled"
	DetailRebootAvailable            = "rebootAvailable"
	DetailRebootUnavailableReason    = "rebootUnavailableReason"
	DetailRebootCommand              = "rebootCommand"
	DetailSelfService                = "selfService"
	DetailSelfServiceReason          = "selfServiceReason"
	DetailProtected                  = "protected"
	DetailDependencyResolution       = "dependencyResolution"
	DetailDependencyResolutionReason = "dependencyResolutionReason"
	DetailBootIDSource               = "bootIdSource"
)

// The status of an allow-list entry in capabilities.
const (
	StatusOK        = "ok"
	StatusMissing   = "missing"   // The init system has no such service
	StatusProtected = "protected" // By name or canonical Id; with the reason
	StatusUnknown   = "unknown"   // Could not be checked; with the reason
)

// Where the boot ID that confirms a reboot comes from.
const (
	BootIDKernel = "kernel"
	// BootIDContainerMount is a container with a per-container boot ID
	// mounted over /proc/sys/kernel/random/boot_id.
	BootIDContainerMount = "container-mount"
)

// AllowListEntry is one providers.services.control entry as capabilities
// report it.
type AllowListEntry struct {
	Name           string   `json:"name"`
	Actions        []string `json:"actions"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	Status         string   `json:"status"`
	Reason         string   `json:"reason,omitempty"`
}

// proxmoxReason is why the provider is unavailable on a Proxmox VE node
// (FR-12, CON-6): it never manages the hypervisor.
const proxmoxReason = "Proxmox VE node: the services provider never manages the hypervisor"

// proxmoxMarkers recognize a Proxmox VE node (A-34). They are only ever
// checked for existence.
var proxmoxMarkers = []string{"/etc/pve", "/var/lib/dpkg/info/pve-manager.list"}

// serviceName is what svcctl accepts as a service name.
var serviceName = regexp.MustCompile(spec.ServiceNamePattern)

// tools are the init system's tools svcctl calls, by absolute path.
var tools = map[platform.InitSystem][]string{
	platform.Systemd: {"/usr/bin/systemctl"},
	platform.OpenRC:  {"/sbin/rc-service", "/bin/rc-status", "/sbin/rc-update"},
}

const (
	bootIDPath        = detach.BootIDPath
	mountinfoPath     = "/proc/self/mountinfo"
	pid1MountinfoPath = "/proc/1/mountinfo"
	// maxMountinfoBytes bounds the read of mountinfo, which grows with
	// every mount (a container host has hundreds).
	maxMountinfoBytes = 4 << 20
	// maxSmallFileBytes bounds the read of a one-line /proc or /run file.
	maxSmallFileBytes = 4 << 10
)

// Preflight reports the provider available only on Linux, with systemd or
// OpenRC, with the init system's tools present, on a host that is not a
// Proxmox VE node. Those checks exec nothing. It then resolves the
// protected set (D-6) and the allow-list's statuses with at most two execs,
// both on systemd: svcctl.Dependencies, and one `systemctl show -p
// LoadState,Id` over every allow-listed name (A-26). A service running
// nexusd that cannot be identified, or a dependency resolution that is
// partial, keeps the provider available and is reported in the details.
// So is a reboot that this host could not confirm (ASM-7).
func (p *Provider) Preflight(ctx context.Context) provider.Status {
	unavailable := func(format string, args ...any) provider.Status {
		return provider.Status{State: apitypes.ProviderUnavailable, Reason: fmt.Sprintf(format, args...)}
	}
	if p.goos != "linux" {
		return unavailable("the services provider runs on Linux only, and this host runs %s", p.goos)
	}
	init := p.plat.Init
	if init != platform.Systemd && init != platform.OpenRC {
		return unavailable("the services provider needs systemd or OpenRC, and this host's init system is %s", init)
	}
	for _, tool := range tools[init] {
		fi, err := p.stat(tool)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return unavailable("%s is missing: the services provider needs %s's tools", tool, init)
		case err != nil:
			return unavailable("%s cannot be checked: %v", tool, err)
		case fi.IsDir():
			return unavailable("%s is a directory: the services provider needs %s's tools", tool, init)
		}
	}
	for _, m := range proxmoxMarkers {
		_, err := p.stat(m)
		if err == nil {
			return unavailable("%s", proxmoxReason)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return unavailable("%s cannot be checked, so this host may be a Proxmox VE node: %v", m, err)
		}
	}

	res := p.resolve(ctx)
	switch {
	case res.self == "":
		p.log.Warn("services: the service running nexusd cannot be identified; only the built-in list and nexusd are protected",
			"reason", res.selfReason)
	case res.status != ResolutionComplete:
		p.log.Warn("services: the protected dependencies are resolved partially", "reason", res.reason)
	}
	p.mu.Lock()
	p.res = res
	snap := newSnapshot(p.snapshot().cfg, res, init)
	p.snap.Store(snap)
	p.mu.Unlock()

	reboot := p.rebootAvailability(init)
	details := map[string]any{
		DetailInitSystem:           string(init),
		DetailLogSource:            logSource(init),
		DetailControlAllowList:     p.allowList(ctx, init, snap),
		DetailRebootEnabled:        snap.cfg.Reboot.Enabled,
		DetailRebootAvailable:      reboot.available,
		DetailRebootCommand:        reboot.command,
		DetailProtected:            snap.prot.names(),
		DetailDependencyResolution: res.status,
		DetailBootIDSource:         nil,
		DetailSelfService:          nil,
	}
	if reboot.source != "" {
		details[DetailBootIDSource] = reboot.source
	}
	if !reboot.available {
		details[DetailRebootUnavailableReason] = reboot.reason
	}
	if res.self != "" {
		details[DetailSelfService] = res.self
	} else {
		details[DetailSelfServiceReason] = res.selfReason
	}
	if res.status != ResolutionComplete {
		details[DetailDependencyResolutionReason] = res.reason
	}
	return provider.Status{State: apitypes.ProviderEnabled, Details: details}
}

func logSource(init platform.InitSystem) string {
	if init == platform.Systemd {
		return spec.LogSourceJournal
	}
	return spec.LogSourceOpenRCFiles
}

// unitLoad is one unit of `systemctl show -p LoadState,Id`.
type unitLoad struct{ id, load string }

// allowList is every allow-list entry with its status. A protected entry
// (by name, or on systemd by its canonical Id) says why. On systemd one
// `systemctl show -p LoadState,Id` covers every entry: LoadState not-found
// is missing, and the Id resolves an alias.
func (p *Provider) allowList(ctx context.Context, init platform.InitSystem, snap *snapshot) []AllowListEntry {
	out := make([]AllowListEntry, len(snap.cfg.Control))
	var valid []int // Indexes of entries whose name svcctl accepts
	for i, e := range snap.cfg.Control {
		out[i] = AllowListEntry{Name: e.Name, Actions: append([]string(nil), e.Actions...),
			TimeoutSeconds: int(e.Timeout / time.Second)}
		if serviceName.MatchString(e.Name) {
			valid = append(valid, i)
		}
	}
	var units []unitLoad
	var showErr error
	if init == platform.Systemd && len(valid) > 0 {
		units, showErr = p.loadStates(ctx, snap, valid)
	}
	for vi, i := range valid {
		e := &out[i]
		canonical := ""
		if units != nil {
			canonical, _ = strings.CutSuffix(units[vi].id, ".service")
		}
		if ok, why := snap.prot.protected(e.Name, canonical); ok {
			e.Status, e.Reason = StatusProtected, why
			continue
		}
		if init == platform.OpenRC {
			e.Status, e.Reason = p.scriptStatus(e.Name)
			continue
		}
		if showErr != nil {
			e.Status, e.Reason = StatusUnknown, fmt.Sprintf("the unit's load state cannot be read: %v", showErr)
			continue
		}
		switch load := units[vi].load; load {
		case "loaded", "masked":
			e.Status = StatusOK
		case "not-found":
			e.Status, e.Reason = StatusMissing, fmt.Sprintf("systemd has no unit %s.service", e.Name)
		case "":
			e.Status, e.Reason = StatusUnknown, fmt.Sprintf("systemctl show reported no LoadState for %s.service", e.Name)
		default:
			e.Status, e.Reason = StatusUnknown, fmt.Sprintf("%s.service has LoadState=%s", e.Name, load)
		}
	}
	for i := range out {
		if out[i].Status == "" {
			out[i].Status = StatusUnknown
			out[i].Reason = fmt.Sprintf("%q is not a service name: it must match %s", out[i].Name, spec.ServiceNamePattern)
		}
	}
	return out
}

// loadStates runs one `systemctl show -p LoadState,Id` over the entries at
// idx, and returns one unit per name, in argument order: systemctl prints
// each unit's properties in that order, separated by an empty line.
func (p *Provider) loadStates(ctx context.Context, snap *snapshot, idx []int) ([]unitLoad, error) {
	argv := []string{tools[platform.Systemd][0], "show", "-p", "LoadState,Id", "--"}
	for _, i := range idx {
		argv = append(argv, snap.cfg.Control[i].Name+".service")
	}
	r, err := svcctl.Call(ctx, argv, p.svcctlOptions())
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, fmt.Errorf("systemctl show exited %d: %s", r.ExitCode, firstLine(r.Output))
	}
	units := parseLoadStates(r.Output)
	if len(units) != len(idx) {
		return nil, fmt.Errorf("systemctl show printed %d units for %d names", len(units), len(idx))
	}
	return units, nil
}

// parseLoadStates splits `systemctl show` output into its units' Id and
// LoadState.
func parseLoadStates(out []byte) []unitLoad {
	var units []unitLoad
	var cur *unitLoad
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			cur = nil
			continue
		}
		if cur == nil {
			units = append(units, unitLoad{})
			cur = &units[len(units)-1]
		}
		switch k, v, _ := strings.Cut(line, "="); k {
		case "Id":
			cur.id = v
		case "LoadState":
			cur.load = v
		}
	}
	return units
}

func firstLine(b []byte) string {
	l, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return l
}

// scriptStatus is an OpenRC allow-list entry's status: whether
// /etc/init.d/<name> exists, read statically.
func (p *Provider) scriptStatus(name string) (string, string) {
	path := "/etc/init.d/" + name
	_, err := p.stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return StatusMissing, fmt.Sprintf("OpenRC has no script %s", path)
	case err != nil:
		return StatusUnknown, fmt.Sprintf("%s cannot be checked: %v", path, err)
	}
	return StatusOK, ""
}

// rebootInfo is whether this host could confirm a reboot by its boot ID.
type rebootInfo struct {
	available bool
	reason    string // Why not, when not available
	source    string // BootIDKernel, BootIDContainerMount or ""
	command   string // RebootArgv, joined
}

// rebootAvailability decides whether a reboot here could be confirmed. The
// boot ID must be readable as PID 1 sees it (p.boot); in a container it
// changes on the container's restart only when a per-container boot ID is
// mounted over it, which /proc/self/mountinfo shows, or /proc/1/mountinfo
// when nexusd's sandbox hides the mount (systemd 252's
// ProtectKernelTunables=yes). Otherwise the container would keep the host
// kernel's boot ID across its restart (ASM-7).
func (p *Provider) rebootAvailability(init platform.InitSystem) rebootInfo {
	var info rebootInfo
	argv, err := svcctl.RebootArgv(init)
	if err != nil {
		info.reason = err.Error()
		return info
	}
	info.command = strings.Join(argv, " ")
	boot, err := p.boot()
	if err != nil {
		info.reason = fmt.Sprintf("the boot ID cannot be read from %s (%v), so a reboot could not be confirmed", bootIDPath, err)
		return info
	}
	var mounts []hostparse.Mount
	if b, err := p.read(mountinfoPath, maxMountinfoBytes); err == nil {
		mounts, _ = hostparse.Mountinfo(b) // None or unparsable: no mount marker
	}
	v := p.virtualization(mounts)
	if v.Kind != hostparse.VirtKindContainer {
		info.available, info.source = true, BootIDKernel
		return info
	}
	if boot.Mounted {
		info.available, info.source = true, BootIDContainerMount
		return info
	}
	info.reason = fmt.Sprintf("this host is a %s container (%s) whose boot ID is the host kernel's, not per-container: "+
		"nothing is mounted over %s in %s or %s, so a reboot could not be confirmed",
		v.Type, v.Evidence, bootIDPath, mountinfoPath, pid1MountinfoPath)
	return info
}

// boot is this boot's ID as PID 1 sees it (detach.ReadBoot) under ProcRoot.
// The source of a boot-ID mount nexusd's sandbox hides is read under
// RunRoot or EtcRoot when it is under /run or /etc.
func (p *Provider) boot() (detach.Boot, error) {
	return detach.ReadBoot(p.procRoot, func(abs string) string {
		if path, ok := p.rooted(abs); ok {
			return path
		}
		return abs
	})
}

// virtualization classifies the host from its container markers only, as
// the host provider does (hostparse.Classify, A-33): a VM and bare metal
// both have a kernel boot ID, so the VM markers are not read.
func (p *Provider) virtualization(mounts []hostparse.Mount) hostparse.Virtualization {
	exists := func(abs string) bool {
		_, err := p.stat(abs)
		return err == nil
	}
	text := func(abs string) string {
		b, _ := p.read(abs, maxSmallFileBytes)
		return string(b)
	}
	return hostparse.Classify(hostparse.VirtFacts{
		DockerEnv:        exists("/.dockerenv"),
		ContainerEnv:     exists("/run/.containerenv"),
		SystemdContainer: text("/run/systemd/container"),
		Mounts:           mounts,
		ProcVZ:           exists("/proc/vz"),
		ProcBC:           exists("/proc/bc"),
		OSRelease:        text("/proc/sys/kernel/osrelease"),
	})
}

// rooted is abs under the provider's ProcRoot, RunRoot or EtcRoot, when it
// is under /proc, /run or /etc.
func (p *Provider) rooted(abs string) (string, bool) {
	for _, r := range []struct{ prefix, root string }{{"/proc", p.procRoot}, {"/run", p.runRoot}, {"/etc", p.etcRoot}} {
		if abs == r.prefix || strings.HasPrefix(abs, r.prefix+"/") {
			return filepath.Join(r.root, strings.TrimPrefix(abs, r.prefix)), true
		}
	}
	return "", false
}

// stat is os.Stat of abs under its root, or fs.Stat of it in HostFS.
func (p *Provider) stat(abs string) (fs.FileInfo, error) {
	if path, ok := p.rooted(abs); ok {
		return os.Stat(path)
	}
	return fs.Stat(p.hostFS, strings.TrimPrefix(abs, "/"))
}

// read is the content of abs, which must be under /proc or /run, read
// through readRegular.
func (p *Provider) read(abs string, max int64) ([]byte, error) {
	path, ok := p.rooted(abs)
	if !ok {
		return nil, fmt.Errorf("%s is not under /proc, /run or /etc", abs)
	}
	return readRegular(path, max)
}

// readRegular is the content of the regular file at path, at most max
// bytes. It is opened without following a final symlink and without
// blocking (a FIFO never hangs the read), and checked on the descriptor.
func readRegular(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over %d bytes", path, max)
	}
	return b, nil
}
