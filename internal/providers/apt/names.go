package apt

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// Layer 2 name resolution (A-28). The schema's pattern (layer 1) admits
// every legal Debian name, and legal names may end in "+" or "-" and hold
// "." and "+": apt-get reads a trailing "-" as remove and "+" as install,
// and a name it has no package for, holding ".", "?" or "*", as a regular
// expression matched by substring (apt-get(8), install). So a name reaches
// apt-get only after it resolved to exactly that package: apt-get then
// finds the package by its exact name before any of those readings
// (apt-pkg/cacheset.cc tries the whole argument as a package name first).
// Upgrade and remove resolve against dpkg (installed packages), install
// against apt's indexes.

// resolveInstalled resolves each name to the installed package of exactly
// that name, and returns the names to pass to apt-get, in order and without
// duplicates: the bare name for a package of the native architecture or
// all, and name:arch for a foreign one. Any name that is not installed
// under exactly that name is not-found, naming it. dpkg-query reads its
// arguments as globs, and a valid name holds no glob character, so it
// matches exactly; each returned row is still compared with the name.
func (p *Provider) resolveInstalled(ctx context.Context, names []string) ([]string, error) {
	return p.resolveDpkg(ctx, names, listed, "a subset upgrade names installed packages only")
}

// resolveRemovable is resolveInstalled for remove: with purge, a package
// left with only its configuration files resolves too, since purging it is
// what deletes them.
func (p *Provider) resolveRemovable(ctx context.Context, names []string, purge bool) ([]string, error) {
	accept := listed
	if purge {
		accept = func(status string) bool { return listed(status) || status == "config-files" }
	}
	return p.resolveDpkg(ctx, names, accept, "remove names installed packages only")
}

// resolveDpkg resolves names against dpkg's database: each must name a
// package whose status accept takes. why ends the not-found detail.
func (p *Provider) resolveDpkg(ctx context.Context, names []string, accept func(status string) bool, why string) (
	[]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	for i, n := range names {
		if !nameRE.MatchString(n) || len(n) > spec.MaxNameLength {
			return nil, invalid(fmt.Sprintf("/names/%d", i), "must match "+spec.NamePattern)
		}
	}
	native, err := p.architecture(ctx)
	if err != nil {
		return nil, unavailable(err.Error())
	}
	res, err := p.run(ctx, cmdDpkgQueryNames(names))
	switch {
	case err != nil:
		return nil, p.failed("dpkg-query", res, err)
	case res.ExitCode != 0 && res.ExitCode != 1: // 1: some name matched nothing
		return nil, p.failed("dpkg-query", res, nil)
	}
	rows := parseDpkgQuery(res.Stdout)

	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		base, arch, _ := strings.Cut(n, ":")
		if arch == native {
			arch = ""
		}
		var found *dpkgPackage
		for i := range rows {
			d := &rows[i]
			if d.Package == base && accept(d.Status) && archMatches(d.Architecture, arch, native) {
				found = d
				break
			}
		}
		if found == nil {
			return nil, problem.New(problem.NotFound, fmt.Sprintf("no package is installed under exactly the name %q; "+
				"%s, and nothing was started", n, why))
		}
		// Named as the installed list and apt-get -s name it.
		name := qualifiedName(base, found.Architecture, native)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// qualifiedName is how apt-get and the installed list name a package of
// architecture arch on a host whose native architecture is native: the bare
// name, except for a foreign architecture, which needs name:arch.
func qualifiedName(name, arch, native string) string {
	if arch != native && arch != "all" {
		return name + ":" + arch
	}
	return name
}

// cmdDpkgQueryNames queries names that passed the name pattern.
func cmdDpkgQueryNames(names []string) command {
	return newCommand(DpkgQuery, append([]string{"-W", "-f=" + dpkgFormat, "--"}, names...)...)
}

var debFileRE = regexp.MustCompile(spec.DebFilePattern)

// validInstallName is layer 1 for install and remove: the schema's name
// pattern and bound, and no .deb file name (FR-16).
func validInstallName(n string) bool {
	return nameRE.MatchString(n) && len(n) <= spec.MaxNameLength && !debFileRE.MatchString(n)
}

// noPackagesFound is apt-cache show's error when no argument matched any
// package (exit status 100). An argument that matched nothing among others
// that did is skipped with exit status 0, so the caller compares every
// returned record with the names (captured in testdata/install, show-*).
var noPackagesFound = []byte("E: No packages found")

// resolveAvailable is layer 2 for install (A-28): each name must be exactly
// a package apt knows. apt-cache show reads an argument it has no package
// for as a regular expression when it holds ".", "?", "+", "*", "|", "[",
// "^" or "$" (apt-pkg/cacheset.cc, isregex), and then prints the records
// of other packages, so a name resolves only when a returned record's
// Package: equals the name without its ":arch", and its Architecture: is
// the one named (the native one when none is) or all. Otherwise it is
// not-found, naming it. It returns the names to pass to apt-get, in order
// and without duplicates: the bare name for the native architecture or
// all, name:arch for a foreign one.
func (p *Provider) resolveAvailable(ctx context.Context, names []string) ([]string, error) {
	for i, n := range names {
		if !validInstallName(n) {
			return nil, invalid(fmt.Sprintf("/names/%d", i), "must match "+spec.NamePattern+" and not name a .deb file")
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	native, err := p.architecture(ctx)
	if err != nil {
		return nil, unavailable(err.Error())
	}
	res, err := p.run(ctx, cmdShowAll(names))
	switch {
	case err != nil:
		return nil, p.failed("apt-cache show", res, err)
	case res.ExitCode == aptExitFailure && bytes.Contains(res.Stderr, noPackagesFound):
		res.Stdout = nil
	case res.ExitCode != 0:
		return nil, p.failed("apt-cache show", res, nil)
	}
	records := parseStanzas(res.Stdout)

	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		base, arch, _ := strings.Cut(n, ":")
		if arch == "" {
			arch = native
		}
		found := ""
		for _, st := range records {
			if a := st["Architecture"]; st["Package"] == base && (a == arch || a == "all") {
				found = a
				break
			}
		}
		if found == "" {
			return nil, problem.New(problem.NotFound, fmt.Sprintf("apt knows no package of exactly the name %q; "+
				"install names packages in the package indexes only, and nothing was started", n))
		}
		name := qualifiedName(base, found, native)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// cmdShowAll reads every version's record of names that passed the name
// pattern.
func cmdShowAll(names []string) command {
	return newCommand(AptCache, slices.Concat(noCacheFiles, []string{"show", "--"}, names)...)
}
