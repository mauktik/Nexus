package apt

import (
	"bufio"
	"bytes"
	"regexp"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// The parsers read only the machine-stable parts of apt and dpkg output
// under LC_ALL=C (D-7, A-27): dpkg-query lines in an explicit format, the
// Installed and Candidate lines of apt-cache policy, the Inst and Remv lines
// of apt-get -s, and deb822 fields of apt-cache show. Every other line is
// ignored, and a line that does not have the expected shape is skipped,
// never turned into a package.

// dpkgPackage is one line of dpkg-query in dpkgFormat.
type dpkgPackage struct {
	Package      string
	Architecture string
	Version      string
	Status       string // db:Status-Status
}

var (
	pkgNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	archRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// A Debian version: [epoch:]upstream[-revision] (Debian Policy 5.6.12).
	versionRE = regexp.MustCompile(`^([0-9]+:)?[A-Za-z0-9][A-Za-z0-9.+~:-]*$`)
)

// parseDpkgQuery parses dpkg-query output in dpkgFormat.
func parseDpkgQuery(b []byte) []dpkgPackage {
	var out []dpkgPackage
	for line := range lines(b) {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		p := dpkgPackage{Package: f[0], Architecture: f[1], Version: f[2], Status: f[3]}
		if !pkgNameRE.MatchString(p.Package) || !slices.Contains(dpkgStatuses, p.Status) ||
			(p.Architecture != "" && !archRE.MatchString(p.Architecture)) ||
			(p.Version != "" && !versionRE.MatchString(p.Version)) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// dpkgStatuses are the package status words of dpkg-query(1), field
// db:Status-Status.
var dpkgStatuses = []string{"not-installed", "config-files", "half-installed", "unpacked", "half-configured",
	"triggers-awaited", "triggers-pending", "installed"}

// listed reports whether the installed list shows a package in status s:
// every state dpkg keeps files for. not-installed is a package dpkg only
// remembers, and config-files one removed but not purged.
func listed(status string) bool {
	return status != "not-installed" && status != "config-files"
}

// policyEntry is one package of apt-cache policy. A version apt reports as
// "(none)" is empty.
type policyEntry struct {
	Installed string
	Candidate string
}

var policyHeaderRE = regexp.MustCompile(`^([a-z0-9][a-z0-9+.-]+(?::[a-z0-9][a-z0-9-]*)?):$`)

// parsePolicy parses apt-cache policy output for named packages. Its keys
// are the header names exactly as apt printed them.
func parsePolicy(b []byte) map[string]policyEntry {
	out := map[string]policyEntry{}
	cur := ""
	for line := range lines(b) {
		if m := policyHeaderRE.FindStringSubmatch(line); m != nil {
			cur = m[1]
			out[cur] = policyEntry{}
			continue
		}
		if cur == "" || !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok {
			continue
		}
		val = policyVersion(val)
		e := out[cur]
		switch key {
		case "Installed":
			e.Installed = val
		case "Candidate":
			e.Candidate = val
		default:
			continue
		}
		out[cur] = e
	}
	return out
}

func policyVersion(v string) string {
	if v == "(none)" || !versionRE.MatchString(v) {
		return ""
	}
	return v
}

// simAction is one Inst or Remv line of apt-get -s.
type simAction struct {
	Op           string // "Inst" or "Remv"
	Name         string // As apt prints it: with ":arch" only for a foreign architecture
	OldVersion   string // In brackets; empty for a new install
	NewVersion   string // Inst only
	Origins      []string
	Architecture string // Inst only
}

var (
	// Inst NAME [OLD] (NEW ORIGIN, ORIGIN [ARCH]) and a possible trailing
	// broken-package marker; OLD is absent for a new install, and ORIGINs
	// may be absent.
	instRE = regexp.MustCompile(`^Inst (\S+)(?: \[([^\]]*)\])? \((\S+)(?: (.*?))? \[([^\]]+)\]\)(?: .*)?$`)
	// Remv NAME [OLD] and a possible trailing marker. Only an installed
	// package is removed, so OLD is always there.
	remvRE = regexp.MustCompile(`^Remv (\S+) \[([^\]]*)\](?: .*)?$`)
	// Purg NAME [OLD], as apt-get -s purge prints a removal: OLD is absent
	// for a package left with only its configuration files (captured in
	// testdata/install, remove-purge-rc).
	purgRE = regexp.MustCompile(`^Purg (\S+)(?: \[([^\]]*)\])?(?: .*)?$`)
)

// parseSimulation parses the Inst, Remv and Purg lines of apt-get -s
// output. A Purg is a removal: its Op is Remv.
func parseSimulation(b []byte) []simAction {
	var out []simAction
	for line := range lines(b) {
		if m := instRE.FindStringSubmatch(line); m != nil {
			a := simAction{Op: "Inst", Name: m[1], OldVersion: m[2], NewVersion: m[3], Architecture: m[5]}
			if m[4] != "" {
				a.Origins = strings.Split(m[4], ", ")
			}
			if validSimName(a.Name) && versionRE.MatchString(a.NewVersion) &&
				(a.OldVersion == "" || versionRE.MatchString(a.OldVersion)) {
				out = append(out, a)
			}
			continue
		}
		if m := remvRE.FindStringSubmatch(line); m != nil {
			a := simAction{Op: "Remv", Name: m[1], OldVersion: m[2]}
			if validSimName(a.Name) && versionRE.MatchString(a.OldVersion) {
				out = append(out, a)
			}
		}
		if m := purgRE.FindStringSubmatch(line); m != nil {
			a := simAction{Op: "Remv", Name: m[1], OldVersion: m[2]}
			if validSimName(a.Name) && (a.OldVersion == "" || versionRE.MatchString(a.OldVersion)) {
				out = append(out, a)
			}
		}
	}
	return out
}

func validSimName(n string) bool {
	base, arch, qualified := strings.Cut(n, ":")
	return pkgNameRE.MatchString(base) && (!qualified || archRE.MatchString(arch))
}

// knownVendors are the archive labels of the distributions Nexus supports
// (Ubuntu and Debian, ASM-9). A candidate offered only by them, and by no
// -security suite, is not a security update. Any other origin, such as a
// third-party repository or a flat repository with no Release file (which
// apt names "localhost"), cannot say, so the answer is unknown.
var knownVendors = []string{"Ubuntu", "Debian", "Debian-Security"}

// origin is one origin of apt-get -s, "<label>[:<version>][/<suite>]".
type origin struct {
	Label string
	Suite string
}

func parseOrigin(s string) origin {
	left, suite, _ := strings.Cut(s, "/")
	label, _, _ := strings.Cut(left, ":")
	return origin{Label: label, Suite: suite}
}

// classifySecurity is spec.SecurityYes when any origin's suite ends in
// -security, spec.SecurityNo when every origin is a known vendor's, and
// spec.SecurityUnknown otherwise, including when there are none.
func classifySecurity(origins []string) string {
	if len(origins) == 0 {
		return spec.SecurityUnknown
	}
	known := true
	for _, s := range origins {
		o := parseOrigin(s)
		if strings.HasSuffix(o.Suite, "-security") {
			return spec.SecurityYes
		}
		if !slices.Contains(knownVendors, o.Label) {
			known = false
		}
	}
	if known {
		return spec.SecurityNo
	}
	return spec.SecurityUnknown
}

// parseStanzas parses deb822 records, as apt-cache show prints them, into
// one map per record. A continuation line is appended to its field with a
// newline, as written; "." as a continuation is an empty line (Debian
// Policy 5.6.13).
func parseStanzas(b []byte) []map[string]string {
	var out []map[string]string
	var cur map[string]string
	key := ""
	for line := range lines(b) {
		if strings.TrimSpace(line) == "" {
			if cur != nil {
				out = append(out, cur)
			}
			cur, key = nil, ""
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if cur == nil || key == "" {
				continue
			}
			cont := line[1:]
			if strings.TrimSpace(cont) == "." {
				cont = ""
			}
			cur[key] += "\n" + cont
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			key = ""
			continue
		}
		if cur == nil {
			cur = map[string]string{}
		}
		key = k
		cur[k] = strings.TrimSpace(v)
	}
	if cur != nil {
		out = append(out, cur)
	}
	return out
}

// parsePkgNames returns the lines of apt-cache pkgnames output.
func parsePkgNames(b []byte) []string {
	var out []string
	for line := range lines(b) {
		if pkgNameRE.MatchString(line) {
			out = append(out, line)
		}
	}
	return out
}

// lines yields each line of b without its line ending.
func lines(b []byte) func(func(string) bool) {
	return func(yield func(string) bool) {
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			if !yield(strings.TrimRight(sc.Text(), "\r")) {
				return
			}
		}
	}
}
