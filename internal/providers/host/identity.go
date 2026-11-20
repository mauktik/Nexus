// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"io/fs"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// The identity sources (FR-5). The OS and init system are not among them:
// they come from internal/platform, the same values system.capabilities.get
// reports (TC-HP-72).
const (
	pathHostname     = "proc/sys/kernel/hostname"
	pathOSRelease    = "proc/sys/kernel/osrelease" // the kernel release, uname -r
	pathUptime       = "proc/uptime"
	pathCPUInfo      = "proc/cpuinfo"
	pathSelfMounts   = "proc/self/mountinfo"
	pathLocaltime    = "etc/localtime"
	pathEtcTimezone  = "etc/timezone"
	pathDockerEnv    = ".dockerenv"
	pathContainerEnv = "run/.containerenv"
	pathSDContainer  = "run/systemd/container"
	pathProcVZ       = "proc/vz"
	pathProcBC       = "proc/bc"
	pathHypervisor   = "sys/hypervisor/type"
	dmiDir           = "sys/class/dmi/id/"
)

// dmiFields are the five allow-listed DMI files, by JSON field. Serial
// numbers, UUIDs and asset tags are never read (NFR-6).
var dmiFields = []struct{ json, file string }{
	{"systemVendor", "sys_vendor"},
	{"productName", "product_name"},
	{"productVersion", "product_version"},
	{"biosVendor", "bios_vendor"},
	{"biosVersion", "bios_version"},
}

// setWhy records that the field key of an object is null (or [], or the
// tri-state unknown) because of why (FR-12, D-5).
func setWhy(u *spec.Unavailable, key, why string) {
	if *u == nil {
		*u = spec.Unavailable{}
	}
	(*u)[key] = why
}

// currentList is the one-item list of a one-item read: the item current on
// this host.
func currentList[P any](p *Provider, typ string, props P) apitypes.List[apitypes.Resource[P]] {
	return apitypes.List[apitypes.Resource[P]]{
		Items: []apitypes.Resource[P]{apitypes.NewResource(p.host, spec.Name, typ, spec.CurrentName, props)},
	}
}

// identity is host.identity.get.
func (p *Provider) identity(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.Identity], error) {
	return currentList(p, spec.IdentityType, p.collectIdentity(ctx)), nil
}

// collectIdentity reads the host's identity live. Each source that fails
// leaves only its own fields null with the reason.
func (p *Provider) collectIdentity(ctx context.Context) spec.IdentityProperties {
	b := p.identityBasics()
	out := spec.IdentityProperties{
		Hostname:      b.hostname,
		OS:            b.os,
		KernelRelease: b.kernelRelease,
		Architecture:  b.architecture,
		InitSystem:    b.initSystem,
		BootTime:      b.bootTime,
		UptimeSeconds: b.uptimeSeconds,
		Unavailable:   b.un,
	}
	osrelease := ""
	if b.kernelRelease != nil {
		osrelease = *b.kernelRelease
	}

	dmi, vendor, product := p.dmi()
	out.DMI = dmi
	out.Virtualization = p.virtualization(osrelease, vendor, product)
	out.Timezone = p.timezone()
	out.Clock = p.clock(ctx)
	return out
}

// identityBasics is what the identity op and the summary's identity part
// both read, in one place so their values and reasons cannot drift apart:
// the hostname, OS, kernel release, architecture, init system, boot time
// and uptime.
type identityBasics struct {
	hostname, kernelRelease, architecture *string
	os                                    spec.OSRelease
	initSystem                            string
	bootTime                              *time.Time
	uptimeSeconds                         *float64
	un                                    spec.Unavailable
}

func (p *Provider) identityBasics() identityBasics {
	var out identityBasics
	un := &out.un

	// The hostname is the kernel's in nexusd's UTS namespace, never
	// os.Hostname or the host.name label (A-20, TC-HP-E19).
	if v, why := p.readString(pathHostname); why != "" {
		setWhy(un, "hostname", why)
	} else if v == "" {
		setWhy(un, "hostname", prefixUnrecognized+displayPath(pathHostname))
	} else {
		out.hostname = &v
	}

	out.os = p.osRelease()
	out.initSystem = string(p.plat.Init)

	if v, why := p.readString(pathOSRelease); why != "" {
		setWhy(un, "kernelRelease", why)
	} else if v == "" {
		setWhy(un, "kernelRelease", prefixUnrecognized+displayPath(pathOSRelease))
	} else {
		out.kernelRelease = &v
	}

	if m, err := p.uname(); err != nil {
		var unsupported *unsupportedError
		if errors.As(err, &unsupported) {
			setWhy(un, "architecture", reasonFor("uname", err))
		} else {
			setWhy(un, "architecture", prefixNotPresent+"uname(2) machine")
		}
	} else if m == "" {
		setWhy(un, "architecture", prefixUnrecognized+"uname(2) machine")
	} else {
		out.architecture = &m
	}

	if b, err := p.src.read(pathStat); err != nil {
		setWhy(un, "bootTime", reasonFor(pathStat, err))
	} else if st, err := hostparse.ProcStat(b); err != nil || !st.HasBootTime || st.BootTime > 1<<62 {
		setWhy(un, "bootTime", prefixUnrecognized+displayPath(pathStat))
	} else {
		bt := time.Unix(int64(st.BootTime), 0).UTC()
		out.bootTime = &bt
	}

	if b, err := p.src.read(pathUptime); err != nil {
		setWhy(un, "uptimeSeconds", reasonFor(pathUptime, err))
	} else if up, err := hostparse.Uptime(b); err != nil {
		setWhy(un, "uptimeSeconds", reasonFor(pathUptime, err))
	} else {
		out.uptimeSeconds = &up
	}
	return out
}

// readString reads a one-value file as text without surrounding space, or
// returns the reason it could not.
func (p *Provider) readString(name string) (string, string) {
	b, err := p.src.read(name)
	if err != nil {
		return "", reasonFor(name, err)
	}
	return hostparse.ScalarString(b), ""
}

// osRelease is the OS as internal/platform reads it, so it never disagrees
// with system.capabilities.get (TC-HP-72). This package never reads the OS
// release file itself.
func (p *Provider) osRelease() spec.OSRelease {
	var out spec.OSRelease
	for _, f := range []struct {
		json, value string
		dst         **string
	}{
		{"id", p.plat.OS.ID, &out.ID},
		{"versionId", p.plat.OS.VersionID, &out.VersionID},
		{"prettyName", p.plat.OS.PrettyName, &out.PrettyName},
	} {
		if f.value == "" {
			setWhy(&out.Unavailable, f.json, reasonNotReported("the OS identification has no "+f.json))
			continue
		}
		v := f.value
		*f.dst = &v
	}
	return out
}

// dmi reads the five DMI files, and returns sys_vendor and product_name
// ("" when unread) for the virtualization check.
func (p *Provider) dmi() (out spec.DMI, vendor, product string) {
	dst := map[string]**string{
		"systemVendor": &out.SystemVendor, "productName": &out.ProductName, "productVersion": &out.ProductVersion,
		"biosVendor": &out.BIOSVendor, "biosVersion": &out.BIOSVersion,
	}
	for _, f := range dmiFields {
		name := dmiDir + f.file
		v, why := p.readString(name)
		switch {
		case why != "":
			setWhy(&out.Unavailable, f.json, why)
		case v == "":
			setWhy(&out.Unavailable, f.json, reasonNotReported("empty "+displayPath(name)))
		default:
			*dst[f.json] = &v
		}
		switch f.file {
		case "sys_vendor":
			vendor = v
		case "product_name":
			product = v
		}
	}
	return out, vendor, product
}

// virtualization classifies the host from files only (A-33): container
// markers, mountinfo, the hypervisor type, DMI and the cpuinfo hypervisor
// flag. A marker that cannot be read counts as absent.
func (p *Provider) virtualization(osrelease, vendor, product string) spec.Virtualization {
	exists := func(name string) bool {
		_, err := p.src.stat(name)
		return err == nil
	}
	text := func(name string) string {
		v, _ := p.readString(name)
		return v
	}
	facts := hostparse.VirtFacts{
		DockerEnv:        exists(pathDockerEnv),
		ContainerEnv:     exists(pathContainerEnv),
		SystemdContainer: text(pathSDContainer),
		ProcVZ:           exists(pathProcVZ),
		ProcBC:           exists(pathProcBC),
		OSRelease:        osrelease,
		HypervisorType:   text(pathHypervisor),
		SysVendor:        vendor,
		ProductName:      product,
	}
	if b, err := p.src.read(pathSelfMounts); err == nil {
		facts.Mounts, _ = hostparse.Mountinfo(b) // unparsable: no lxcfs marker
	}
	if b, err := p.src.read(pathCPUInfo); err == nil {
		if ci, err := hostparse.Cpuinfo(b); err == nil {
			facts.HypervisorFlag = ci.Hypervisor
		}
	}
	v := hostparse.Classify(facts)
	ev := v.Evidence
	return spec.Virtualization{Type: v.Type, Kind: v.Kind, Evidence: &ev}
}

// timezone is A-31: the name from /etc/timezone, else the /etc/localtime
// link target; the offset from /etc/localtime at now; UTC without
// /etc/localtime.
func (p *Provider) timezone() spec.Timezone {
	var out spec.Timezone
	localtime, ltErr := p.src.read(pathLocaltime)
	if errors.Is(ltErr, fs.ErrNotExist) {
		localtime, ltErr = nil, nil
	} else if ltErr != nil {
		localtime = []byte{} // present but unreadable: never the UTC default
	}
	etcTZ, err := p.src.read(pathEtcTimezone)
	if err != nil {
		etcTZ = nil
	}
	link, linkErr := p.src.readLink(pathLocaltime)
	if linkErr != nil {
		link = ""
	}

	tz, parseErr := hostparse.Timezone(localtime, etcTZ, link, p.now())
	if tz.Name != "" {
		name, source := tz.Name, tz.NameSource
		out.Name, out.Source = &name, &source
	} else {
		why := reasonNotReported("no zone name in /etc/timezone or the /etc/localtime link")
		// A link that could not be read for a reason other than not
		// being a link (EINVAL) is the more precise reason.
		if linkErr != nil && !errors.Is(linkErr, syscall.EINVAL) && !errors.Is(linkErr, fs.ErrNotExist) {
			why = reasonFor(pathLocaltime, linkErr)
		}
		setWhy(&out.Unavailable, "name", why)
		setWhy(&out.Unavailable, "source", why)
	}
	switch {
	case ltErr != nil:
		setWhy(&out.Unavailable, "offsetSeconds", reasonFor(pathLocaltime, ltErr))
	case parseErr != nil:
		setWhy(&out.Unavailable, "offsetSeconds", prefixUnrecognized+displayPath(pathLocaltime))
	default:
		off := tz.Offset
		out.OffsetSeconds = &off
	}
	return out
}
