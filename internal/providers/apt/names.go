package apt

import (
	"context"
	"fmt"
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
// finds the package by its exact name before any of those readings.

// resolveInstalled resolves each name to the installed package of exactly
// that name, and returns the names to pass to apt-get, in order and without
// duplicates: the bare name for a package of the native architecture or
// all, and name:arch for a foreign one. Any name that is not installed
// under exactly that name is not-found, naming it. dpkg-query reads its
// arguments as globs, and a valid name holds no glob character, so it
// matches exactly; each returned row is still compared with the name.
func (p *Provider) resolveInstalled(ctx context.Context, names []string) ([]string, error) {
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
			if d.Package == base && listed(d.Status) && archMatches(d.Architecture, arch, native) {
				found = d
				break
			}
		}
		if found == nil {
			return nil, problem.New(problem.NotFound, fmt.Sprintf("no package is installed under exactly the name %q; "+
				"a subset upgrade names installed packages only, and nothing was started", n))
		}
		// Named as the installed list and apt-get -s name it.
		name := base
		if found.Architecture != native && found.Architecture != "all" {
			name = base + ":" + found.Architecture
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// cmdDpkgQueryNames queries names that passed the name pattern.
func cmdDpkgQueryNames(names []string) command {
	return newCommand(DpkgQuery, append([]string{"-W", "-f=" + dpkgFormat, "--"}, names...)...)
}
