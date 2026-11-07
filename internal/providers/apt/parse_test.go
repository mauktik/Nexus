package apt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// The testdata directories were captured from real apt by
// testdata/capture.sh (date, image, apt and dpkg versions in each
// PROVENANCE); none of it is hand-written.
var bases = []string{"ubuntu-latest", "debian-stable"}

func capture(t *testing.T, base, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", base, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTestdataIsCaptured(t *testing.T) {
	for _, base := range bases {
		prov := string(capture(t, base, "PROVENANCE"))
		for _, want := range []string{"captured: ", "apt: apt ", "dpkg: ", "script: internal/providers/apt/testdata/capture.sh"} {
			if !strings.Contains(prov, want) {
				t.Errorf("%s/PROVENANCE lacks %q:\n%s", base, want, prov)
			}
		}
	}
}

// rawInst is the second, independent parse of the Inst lines: fields split
// on spaces, the old version in brackets, the new one after "(".
type rawInst struct{ name, old, new string }

func rawInsts(b []byte) []rawInst {
	var out []rawInst
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != "Inst" {
			continue
		}
		r := rawInst{name: f[1]}
		rest := f[2:]
		if strings.HasPrefix(rest[0], "[") {
			r.old = strings.Trim(rest[0], "[]")
			rest = rest[1:]
		}
		r.new = strings.TrimPrefix(rest[0], "(")
		out = append(out, r)
	}
	return out
}

// TC-120: Inst and Remv lines.
func TestParseSimulationCaptured(t *testing.T) {
	for _, base := range bases {
		for _, file := range []string{"archive-sim-dist-upgrade.txt", "fixture-sim-dist-upgrade.txt"} {
			b := capture(t, base, file)
			want := rawInsts(b)
			if len(want) == 0 {
				t.Fatalf("%s/%s has no Inst line, so the test checks nothing", base, file)
			}
			got := parseSimulation(b)
			if len(got) != len(want) {
				t.Fatalf("%s/%s: %d actions, want %d (Conf and text lines must be ignored): %+v", base, file, len(got), len(want), got)
			}
			for i, a := range got {
				w := want[i]
				if a.Op != "Inst" || a.Name != w.name || a.OldVersion != w.old || a.NewVersion != w.new || a.Architecture == "" {
					t.Errorf("%s/%s action %d: %+v, want %+v", base, file, i, a, w)
				}
			}
		}

		got := parseSimulation(capture(t, base, "fixture-sim-remove.txt"))
		want := []simAction{{Op: "Remv", Name: "nxlocal", OldVersion: "1.0"}, {Op: "Remv", Name: "nxplain", OldVersion: "1.0"}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s remove: %+v, want %+v", base, got, want)
		}
		fx := parseSimulation(capture(t, base, "fixture-sim-dist-upgrade.txt"))
		if len(fx) != 1 || !slices.Equal(fx[0].Origins, []string{"localhost"}) || fx[0].Architecture != "all" {
			t.Errorf("%s fixture Inst: %+v", base, fx)
		}
	}
}

// TC-120: Installed and Candidate, with (none).
func TestParsePolicyCaptured(t *testing.T) {
	for _, base := range bases {
		got := parsePolicy(capture(t, base, "fixture-policy.txt"))
		dpkgVersion := ""
		for _, d := range parseDpkgQuery(capture(t, base, "fixture-dpkg-query.txt")) {
			if d.Package == "dpkg" {
				dpkgVersion = d.Version
			}
		}
		want := map[string]policyEntry{
			"nxplain": {Installed: "1.0", Candidate: "2.0"},
			"nxavail": {Installed: "", Candidate: "1.0"}, // Installed: (none)
			"nxlocal": {Installed: "1.0", Candidate: "1.0"},
			"dpkg":    {Installed: dpkgVersion, Candidate: dpkgVersion},
		}
		if dpkgVersion == "" || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s fixture policy: %v, want %v", base, got, want)
		}

		// The archive: every Inst line's versions, read independently.
		arch := parsePolicy(capture(t, base, "archive-policy.txt"))
		insts := rawInsts(capture(t, base, "archive-sim-dist-upgrade.txt"))
		if len(arch) != len(insts) {
			t.Errorf("%s archive policy has %d packages, the simulation %d", base, len(arch), len(insts))
		}
		for _, r := range insts {
			if e := arch[r.name]; e.Installed != r.old || e.Candidate != r.new {
				t.Errorf("%s archive policy %s: %+v, want Installed %s Candidate %s", base, r.name, e, r.old, r.new)
			}
		}

		if got := parsePolicy(capture(t, base, "fixture-policy-unknown.txt")); len(got) != 0 {
			t.Errorf("%s: an unknown name parsed as %v", base, got)
		}
	}
}

// TC-120: dpkg-query in the explicit format.
func TestParseDpkgQueryCaptured(t *testing.T) {
	for _, base := range bases {
		for _, file := range []string{"fixture-dpkg-query.txt", "archive-dpkg-query.txt"} {
			b := capture(t, base, file)
			// The second parse: every non-empty line is one package.
			var want []string
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				want = append(want, strings.SplitN(line, "\t", 2)[0])
			}
			got := parseDpkgQuery(b)
			var names []string
			for _, d := range got {
				names = append(names, d.Package)
				if d.Version == "" || d.Architecture == "" || d.Status != "installed" {
					t.Errorf("%s/%s: %+v", base, file, d)
				}
			}
			if !slices.Equal(names, want) {
				t.Errorf("%s/%s: %d names, want %d", base, file, len(names), len(want))
			}
		}
		one := parseDpkgQuery(capture(t, base, "fixture-dpkg-query-nxplain.txt"))
		if len(one) != 1 || one[0] != (dpkgPackage{Package: "nxplain", Architecture: "all", Version: "1.0", Status: "installed"}) {
			t.Errorf("%s nxplain: %+v", base, one)
		}
		if got := parseDpkgQuery(capture(t, base, "fixture-dpkg-query-unknown.txt")); len(got) != 0 {
			t.Errorf("%s: dpkg-query's no-match message parsed as %+v", base, got)
		}
	}
}

func TestParseShowCaptured(t *testing.T) {
	for _, base := range bases {
		st := parseStanzas(capture(t, base, "fixture-show-nxplain.txt"))
		if len(st) != 1 || st[0]["Package"] != "nxplain" || st[0]["Version"] != "2.0" ||
			st[0]["Description"] != "Nexus apt fixture package nxplain\nA package built by the Nexus T1 harness. It installs one file." {
			t.Errorf("%s show nxplain: %q", base, st)
		}
		all := parseStanzas(capture(t, base, "fixture-show-all-nxplain.txt"))
		if len(all) != 2 || all[0]["Version"] != "2.0" || all[1]["Version"] != "1.0" || all[1]["Status"] != "install ok installed" {
			t.Errorf("%s show all nxplain: %q", base, all)
		}
		if got := parsePkgNames(capture(t, base, "fixture-pkgnames-nxpl.txt")); !slices.Equal(got, []string{"nxplain"}) {
			t.Errorf("%s pkgnames: %q", base, got)
		}
	}
}

// TC-120: unknown line forms are ignored. Every prefix of every captured
// line, and some foreign lines, either parse as a package the full line
// names or not at all, and nothing panics.
func TestParsersIgnoreUnknownLines(t *testing.T) {
	foreign := []string{"", "Inst", "Inst ", "Inst x", "Remv", "Conf nxplain (2.0 localhost [all])",
		"E: Unable to locate package nxnosuch", "W: Symlinking failed", "Inst -o [1] (2 x [all])",
		"Inst ../../etc [1] (2 x [all])", "nxplain:", "  Installed: $(id)", "\x00\xff\t\t\t",
		"Inst nxplain [1.0] (2.0; rm -rf / [all])", "a\tb\tc\td\te"}
	for _, base := range bases {
		names := map[string]bool{}
		var all []string
		entries, err := os.ReadDir(filepath.Join("testdata", base))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			for _, line := range strings.Split(string(capture(t, base, e.Name())), "\n") {
				all = append(all, line)
				for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == ':' }) {
					names[f] = true
				}
			}
		}
		for _, line := range slices.Concat(all, foreign) {
			for i := 0; i <= len(line); i++ {
				b := []byte(line[:i])
				for _, a := range parseSimulation(b) {
					if !names[strings.SplitN(a.Name, ":", 2)[0]] || strings.ContainsAny(a.Name, " ;$/") {
						t.Fatalf("%q parsed as package %q", line[:i], a.Name)
					}
				}
				for _, d := range parseDpkgQuery(b) {
					if !names[d.Package] {
						t.Fatalf("%q parsed as package %q", line[:i], d.Package)
					}
				}
				for k, v := range parsePolicy(b) {
					if v != (policyEntry{}) {
						t.Fatalf("%q alone parsed as policy %s %+v", line[:i], k, v)
					}
				}
				_ = parseStanzas(b)
				_ = parsePkgNames(b)
			}
		}
		for _, line := range foreign {
			if got := parseSimulation([]byte(line)); len(got) != 0 {
				t.Errorf("foreign line %q parsed as %+v", line, got)
			}
		}
	}
}

// fakeApt answers the provider's commands from a captured directory. Its
// policy is the captured policy file whatever names are asked.
type fakeApt struct {
	t     *testing.T
	base  string
	files map[string]string // first distinguishing argument -> capture file
	calls [][]string
}

func (f *fakeApt) run(_ context.Context, c command) (result, error) {
	f.calls = append(f.calls, slices.Clone(c.Argv))
	sub := ""
	for _, a := range c.Argv[1:] {
		if !strings.HasPrefix(a, "-") && !strings.Contains(a, "::") {
			sub = a
			break
		}
	}
	key := filepath.Base(c.Argv[0]) + " " + sub
	if c.Argv[0] == DpkgQuery {
		key = "dpkg-query"
		if c.Argv[len(c.Argv)-2] == "--" {
			key = "dpkg-query " + c.Argv[len(c.Argv)-1]
		}
	}
	if c.Argv[0] == Dpkg {
		key = "dpkg arch"
	}
	file, ok := f.files[key]
	if !ok {
		if strings.HasPrefix(key, "dpkg-query ") {
			return result{Stderr: []byte("dpkg-query: no packages found matching x\n"), ExitCode: 1}, nil
		}
		f.t.Fatalf("unexpected command %q (key %q)", c.Argv, key)
	}
	return result{Stdout: capture(f.t, f.base, file)}, nil
}

func newFake(t *testing.T, base, set string) (*Provider, *fakeApt) {
	f := &fakeApt{t: t, base: base, files: map[string]string{
		"dpkg arch":            "print-architecture.txt",
		"dpkg-query":           set + "-dpkg-query.txt",
		"apt-cache policy":     set + "-policy.txt",
		"apt-get dist-upgrade": set + "-sim-dist-upgrade.txt",
		"apt-cache show":       "fixture-show-nxplain.txt",
		"apt-cache pkgnames":   "fixture-pkgnames-nxpl.txt",
	}}
	p := New(Options{Host: "testhost"})
	p.run = f.run
	p.root = t.TempDir()
	return p, f
}

// TC-120 and TC-121: upgradable is the set where Candidate differs from
// Installed, computed independently from the raw Inst lines, and each is
// classified by its origins.
func TestUpgradableFromCaptures(t *testing.T) {
	security := map[string]map[string]string{
		"ubuntu-latest": {"base-files": spec.SecurityNo, "libapt-pkg7.0": spec.SecurityYes, "apt": spec.SecurityYes,
			"libpcre2-8-0": spec.SecurityYes},
		"debian-stable": {"zlib1g": spec.SecurityYes},
	}
	for _, base := range bases {
		for _, set := range []string{"archive", "fixture"} {
			p, _ := newFake(t, base, set)
			got, err := p.upgradableAll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]rawInst{}
			for _, r := range rawInsts(capture(t, base, set+"-sim-dist-upgrade.txt")) {
				want[r.name] = r
			}
			if len(got) != len(want) {
				t.Errorf("%s %s: %d upgradable, want %d: %+v", base, set, len(got), len(want), got)
			}
			for _, u := range got {
				w, ok := want[u.Name]
				pr := u.Properties
				if !ok || pr.InstalledVersion != w.old || pr.CandidateVersion != w.new || u.ID != "/providers/apt/packages/"+u.Name {
					t.Errorf("%s %s: %+v, want %+v", base, set, u, w)
				}
				wantSec := spec.SecurityUnknown // The fixture's origin is "localhost"
				if set == "archive" {
					wantSec = security[base][u.Name]
				}
				if pr.Security != wantSec || len(pr.Origins) == 0 {
					t.Errorf("%s %s %s: security %q origins %q, want %q", base, set, u.Name, pr.Security, pr.Origins, wantSec)
				}
			}
			if !slices.IsSortedFunc(got, func(a, b spec.Upgradable) int { return strings.Compare(a.Name, b.Name) }) {
				t.Errorf("%s %s: not sorted", base, set)
			}
		}
	}
}

// TC-121.
func TestSecurityClassification(t *testing.T) {
	origins := map[string][]string{}
	for _, base := range bases {
		for _, file := range []string{"archive-sim-dist-upgrade.txt", "fixture-sim-dist-upgrade.txt"} {
			for _, a := range parseSimulation(capture(t, base, file)) {
				origins[base+" "+a.Name] = a.Origins
			}
		}
	}
	cases := []struct {
		name    string
		origins []string
		want    string
	}{
		{"Ubuntu -security among others", origins["ubuntu-latest libapt-pkg7.0"], spec.SecurityYes},
		{"Ubuntu -updates only", origins["ubuntu-latest base-files"], spec.SecurityNo},
		{"Debian-Security", origins["debian-stable zlib1g"], spec.SecurityYes},
		{"flat repository without a Release file", origins["ubuntu-latest nxplain"], spec.SecurityUnknown},
		{"same on Debian", origins["debian-stable nxplain"], spec.SecurityUnknown},
		{"unknown vendor", []string{"Docker:stable/noble"}, spec.SecurityUnknown},
		{"unknown vendor beside a known one", slices.Concat(origins["ubuntu-latest base-files"], []string{"Docker:stable/noble"}), spec.SecurityUnknown},
		{"unknown vendor -security still counts", []string{"Vendor:1/stable-security"}, spec.SecurityYes},
		{"no origin", nil, spec.SecurityUnknown},
	}
	for _, c := range cases {
		if c.origins == nil && c.name != "no origin" {
			t.Fatalf("%s: no captured origins", c.name)
		}
		if got := classifySecurity(c.origins); got != c.want {
			t.Errorf("%s %q: %s, want %s", c.name, c.origins, got, c.want)
		}
	}
}
