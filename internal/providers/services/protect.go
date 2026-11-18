package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// The dependency resolution reported in capabilities (D-6, TC-SV-E14).
const (
	ResolutionComplete = "complete"
	ResolutionPartial  = "partial"
	// ResolutionSkipped: the service running nexusd is unknown, so no
	// dependency was resolved and only the static list and nexusd are
	// protected.
	ResolutionSkipped = "skipped"
)

// selfName is Nexus's own service, protected by name whatever unit runs it.
const selfName = "nexusd"

// maxDeptreeBytes bounds the provider's read of the OpenRC deptree, as
// svcctl bounds its own.
const maxDeptreeBytes = 1 << 20

// protectEntry is one entry of the resolved protected set: a service name,
// or a prefix ending in "*", and why it is protected.
type protectEntry struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// protectedSet is the resolved protected set of D-6, in the order its
// reasons are reported: the unit or script running nexusd, nexusd itself,
// that unit's dependencies, then the built-in list of the init system.
type protectedSet struct {
	entries []protectEntry
}

// resolution is what Preflight resolved about the host for the protected
// set: the service running nexusd and the services it depends on.
type resolution struct {
	self       string // "" when unknown
	selfReason string // Why self is unknown
	deps       []protectEntry
	status     string // ResolutionComplete, Partial or Skipped
	reason     string // Why the resolution is not complete
}

// newProtectedSet is init's protected set with res's dynamic protections
// (nil before the first preflight). An init system other than systemd or
// OpenRC gets both built-in lists, which only ever protects more.
func newProtectedSet(init platform.InitSystem, res *resolution) *protectedSet {
	var entries []protectEntry
	if res != nil && res.self != "" {
		entries = append(entries, protectEntry{Name: res.self, Reason: selfReason(init, res.self)})
	}
	entries = append(entries, protectEntry{Name: selfName, Reason: "nexusd is Nexus's own service"})
	if res != nil {
		entries = append(entries, res.deps...)
	}
	lists := map[string][]string{}
	switch init {
	case platform.Systemd:
		lists["systemd"] = spec.ProtectedSystemd
	case platform.OpenRC:
		lists["OpenRC"] = spec.ProtectedOpenRC
	default:
		lists["systemd"], lists["OpenRC"] = spec.ProtectedSystemd, spec.ProtectedOpenRC
	}
	for _, label := range slices.Sorted(func(yield func(string) bool) {
		for k := range lists {
			if !yield(k) {
				return
			}
		}
	}) {
		for _, e := range lists[label] {
			entries = append(entries, protectEntry{Name: e, Reason: fmt.Sprintf("the built-in %s list entry %q", label, e)})
		}
	}
	return &protectedSet{entries: entries}
}

func selfReason(init platform.InitSystem, self string) string {
	if init == platform.Systemd {
		return self + ".service is the unit running nexusd"
	}
	return self + " is the script running nexusd"
}

// match is the reason of the first entry protecting name.
func (s *protectedSet) match(name string) (bool, string) {
	for _, e := range s.entries {
		if ok, _ := spec.Protected(name, []string{e.Name}); ok {
			return true, e.Reason
		}
	}
	return false, ""
}

// protected reports whether name, or canonical (its unit Id without
// .service, "" when unknown), is protected, and why (D-6). Matching is
// case-sensitive, as service names are.
func (s *protectedSet) protected(name, canonical string) (bool, string) {
	if ok, why := s.match(name); ok {
		return true, why
	}
	if canonical != "" && canonical != name {
		if ok, why := s.match(canonical); ok {
			return true, fmt.Sprintf("%s is an alias of %s: %s", name, canonical, why)
		}
	}
	return false, ""
}

// names are the protected names and prefixes, each once, in entry order.
func (s *protectedSet) names() []string {
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		if !slices.Contains(out, e.Name) {
			out = append(out, e.Name)
		}
	}
	return out
}

// resolve finds the service running nexusd and its dependencies (D-6). On
// systemd that is one exec (svcctl.Dependencies); on OpenRC none. When the
// running service is unknown nothing else is resolved. A dependency lookup
// that fails leaves the resolution partial, with the error as its reason;
// the static list and nexusd stay protected.
func (p *Provider) resolve(ctx context.Context) *resolution {
	init := p.plat.Init
	self, err := svcctl.SelfService(init, p.procRoot, p.runRoot, os.Getpid())
	if err != nil {
		return &resolution{selfReason: err.Error(), status: ResolutionSkipped,
			reason: "the service running nexusd is unknown, so its dependencies are not resolved"}
	}
	res := &resolution{self: self, status: ResolutionComplete}

	// The systemd reasons name the property each unit came from, which
	// Deps does not carry: the one call's output is kept for them. Deps
	// alone decides which names are protected.
	opts := p.svcctlOptions()
	var out []byte
	if init == platform.Systemd {
		inner := opts.Run
		if inner == nil {
			inner = svcctl.Run
		}
		opts.Run = func(ctx context.Context, argv []string) (svcctl.Result, error) {
			r, err := inner(ctx, argv)
			if err == nil {
				out = slices.Clone(r.Output)
			}
			return r, err
		}
	}
	deps, err := svcctl.Dependencies(ctx, init, self, opts)
	if err != nil {
		res.status, res.reason = ResolutionPartial, err.Error()
		return res
	}
	var via map[string]string
	switch init {
	case platform.Systemd:
		via = systemdDependencyKeys(out)
	case platform.OpenRC:
		via = p.openrcNeeds(self)
	}
	for _, n := range deps.Names {
		res.deps = append(res.deps, protectEntry{Name: n, Reason: dependencyReason(init, self, n, via[n])})
	}
	if deps.Partial {
		res.status, res.reason = ResolutionPartial, deps.Reason
	}
	return res
}

// dependencyReason is why dependency name of self is protected; via is the
// systemd property it came from, or the OpenRC need it provides ("" when
// not known).
func dependencyReason(init platform.InitSystem, self, name, via string) string {
	if init == platform.Systemd {
		if via == "" {
			return fmt.Sprintf("a Requires=, BindsTo=, Requisite= or PartOf= dependency of %s.service, the unit running nexusd", self)
		}
		return fmt.Sprintf("%s= of %s.service, the unit running nexusd", via, self)
	}
	if via == "" {
		via = name
	}
	return fmt.Sprintf("%s needs %s", self, via)
}

// systemdDependencyKeys maps each .service unit of `systemctl show -p
// Requires -p BindsTo -p Requisite -p PartOf` output, without the suffix,
// to the first property naming it.
func systemdDependencyKeys(out []byte) map[string]string {
	via := map[string]string{}
	for line := range strings.Lines(string(out)) {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "=")
		if !ok || !slices.Contains([]string{"Requires", "BindsTo", "Requisite", "PartOf"}, k) {
			continue
		}
		for _, u := range strings.Fields(v) {
			if n, ok := strings.CutSuffix(u, ".service"); ok {
				if _, seen := via[n]; !seen {
					via[n] = k
				}
			}
		}
	}
	return via
}

// openrcNeeds maps each provider of a service self ineeds in the OpenRC
// deptree to that need (networking -> net). It only words the reasons:
// svcctl.Dependencies decides the names. An unreadable deptree gives none.
func (p *Provider) openrcNeeds(self string) map[string]string {
	via := map[string]string{}
	b, err := readRegular(filepath.Join(p.runRoot, "openrc", "deptree"), maxDeptreeBytes)
	if err != nil {
		return via
	}
	first := map[string]string{} // name -> index of its first entry
	values := map[string][]string{}
	for line := range strings.Lines(string(b)) {
		rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "depinfo_")
		if !ok {
			continue
		}
		lhs, rhs, ok := strings.Cut(rest, "=")
		if !ok || len(rhs) < 2 || rhs[0] != '\'' || rhs[len(rhs)-1] != '\'' {
			continue
		}
		val := rhs[1 : len(rhs)-1]
		parts := strings.Split(lhs, "_")
		switch {
		case len(parts) == 2 && parts[1] == "service":
			if _, dup := first[val]; !dup {
				first[val] = parts[0]
			}
		case len(parts) == 3 && (parts[1] == "ineed" || parts[1] == "providedby"):
			key := parts[0] + "_" + parts[1]
			values[key] = append(values[key], val)
		}
	}
	idx, ok := first[self]
	if !ok {
		return via
	}
	for _, need := range values[idx+"_ineed"] {
		nidx, ok := first[need]
		if !ok {
			continue
		}
		for _, prov := range values[nidx+"_providedby"] {
			if _, seen := via[prov]; !seen {
				via[prov] = need
			}
		}
	}
	return via
}
