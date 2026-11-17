package host

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"regexp"
	"strings"

	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// The Debian reboot-required marker and its package list, relative to the
// root. They mirror apt.RebootMarker and apt.RebootPackagesFile, and
// rebootMarkerWriters (allowlist.go) mirrors apt.RebootMarkerWriters; the
// host package does not import the apt provider, so TC-HP-93 asserts the
// copies are equal. The marker and the writers are stat-only.
const (
	pathRebootMarker   = "var/run/reboot-required"
	pathRebootPackages = "var/run/reboot-required.pkgs"
	pathLibModules     = "lib/modules"
)

// The package list rule of the apt provider's rebootState
// (internal/providers/apt/provider.go:228): a name matching apt's pkgNameRE
// (parse.go:29), no duplicates, at most maxRebootPackages
// (provider.go:53). Both are unexported there, so
// TestRebootPackagesRuleMatchesAptSource pins them to apt's source.
var rebootPackagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)

const maxRebootPackages = 1000

// The checks of spec.RebootRequired.Check.
const (
	checkDebianMarker  = "debian-reboot-required-marker"
	checkAlpineModules = "alpine-running-kernel-modules"
	checkNone          = "none"
)

var (
	reasonNoMarkerWriter    = reasonNotReported("no reboot-required marker writer is installed")
	reasonRebootUnsupported = prefixNotSupported + "no reboot-required check for this OS family (Debian family and Alpine only)"
	reasonAlpineContainer   = prefixNotSupported + "reboot-required in an Alpine container, whose kernel is the container host's"
)

// rebootRequired is A-18, in precedence order:
//  1. Debian family, in a container or not: the apt provider's marker rule;
//  2. Alpine in a container: unknown, the kernel is the container host's;
//  3. Alpine on a VM or bare metal: the running kernel's modules directory;
//  4. any other family: unknown, not supported.
func (p *Provider) rebootRequired() spec.RebootRequired {
	switch {
	case p.plat.OS.IsDebianFamily():
		return p.debianReboot()
	case p.plat.OS.IsAlpine():
		return p.alpineReboot()
	}
	out := spec.RebootRequired{State: unknownState, Check: checkNone,
		Evidence: "no reboot-required check for this OS family"}
	setWhy(&out.Unavailable, "state", reasonRebootUnsupported)
	return out
}

// debianReboot is the apt provider's rule exactly: the marker present is
// true with its packages; absent, false when a marker writer is installed
// and unknown when none is, since nothing would ever create it. As in apt,
// a stat that fails for any reason counts as absent. The marker describes
// the packages of the root it is in, so a container gives the same answer
// as the apt provider in it (A-18).
func (p *Provider) debianReboot() spec.RebootRequired {
	out := spec.RebootRequired{Check: checkDebianMarker}
	marker := displayPath(pathRebootMarker)
	if _, err := p.src.stat(pathRebootMarker); err == nil {
		out.State, out.Evidence = spec.RebootTrue, marker+" exists"
		if b, err := p.src.read(pathRebootPackages); err != nil {
			setWhy(&out.Unavailable, "packages", reasonFor(pathRebootPackages, err))
		} else {
			out.Packages = rebootPackages(b)
		}
		return out
	}
	for _, w := range rebootMarkerWriters {
		if _, err := p.src.stat(w); err == nil {
			out.State = spec.RebootFalse
			out.Evidence = marker + " is absent; marker writer installed: " + displayPath(w)
			return out
		}
	}
	out.State = unknownState
	out.Evidence = marker + " is absent and no marker writer is installed"
	setWhy(&out.Unavailable, "state", reasonNoMarkerWriter)
	return out
}

// rebootPackages is apt's parse of the package list: each line, trimmed,
// kept when it matches the pattern and is new, up to the bound. Lines are
// scanned as apt's lines() does, with a 1 MiB line limit.
func rebootPackages(b []byte) []string {
	pkgs := []string{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		name := strings.TrimSpace(strings.TrimRight(sc.Text(), "\r"))
		if rebootPackagePattern.MatchString(name) && !seen[name] && len(pkgs) < maxRebootPackages {
			seen[name] = true
			pkgs = append(pkgs, name)
		}
	}
	return pkgs
}

// alpineReboot is rules 2 and 3 of A-18. Alpine's kernel package removes
// the old kernel's /lib/modules/<release> on upgrade, so the running
// kernel's directory missing means a newer kernel is installed and not yet
// booted. In a container the running kernel is the container host's, so
// the directory says nothing.
func (p *Provider) alpineReboot() spec.RebootRequired {
	out := spec.RebootRequired{Check: checkAlpineModules, State: unknownState}
	release, why := p.readString(pathOSRelease)
	if why == "" && release == "" {
		why = prefixUnrecognized + displayPath(pathOSRelease)
	}
	var modules string
	if why == "" {
		var err error
		if modules, err = segPath(segName, pathLibModules, release); err != nil {
			why = prefixUnrecognized + displayPath(pathOSRelease)
		}
	}
	if why != "" {
		out.Evidence = "the running kernel release could not be read"
		setWhy(&out.Unavailable, "state", why)
		return out
	}

	// DMI only tells VMs apart, so it is not read for the container test.
	if v := p.virtualization(release, "", ""); v.Kind == hostparse.VirtKindContainer {
		out.Check = checkNone
		out.Evidence = "in a " + v.Type + " container: the running kernel " + release +
			" is the container host's, so " + displayPath(modules) + " says nothing"
		setWhy(&out.Unavailable, "state", reasonAlpineContainer)
		return out
	}

	_, err := p.src.stat(modules)
	switch {
	case err == nil:
		out.State = spec.RebootFalse
		out.Evidence = displayPath(modules) + " exists for the running kernel"
	case errors.Is(err, fs.ErrNotExist):
		out.State = spec.RebootTrue
		out.Evidence = displayPath(modules) + " is missing: the running kernel is no longer installed"
	default:
		out.Evidence = displayPath(modules) + " could not be checked"
		setWhy(&out.Unavailable, "state", reasonFor(modules, err))
	}
	return out
}
