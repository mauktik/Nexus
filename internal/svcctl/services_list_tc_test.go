// Tests of planning/services-provider/test-cases.md TC-SV-46 to TC-SV-60,
// TC-SV-E8, TC-SV-E9, TC-SV-ERR12 and TC-SV-ERR13 (WI 3.2), and the fuzz
// targets FuzzParseListUnits, FuzzParseListUnitFiles, FuzzParseShow,
// FuzzParseRCStatus and FuzzParseRCUpdate.
//
// The capture-backed cases iterate over every capture directory under
// testdata/services (svcctltest.Captures), so WI 6.0's host captures join
// without a code change. Expected values are computed from each capture's
// own files and oracle (test-strategy.md, Expected values), never written
// down. The package is svcctl_test because F-HOSTS (svcctltest) imports
// svcctl.

package svcctl_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// The fixed argv of DF-1 (architecture.md) that List, Describe and
// EnabledAtBoot send.
var (
	dfListUnits     = []string{"/usr/bin/systemctl", "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager", "--full"}
	dfListUnitFiles = []string{"/usr/bin/systemctl", "list-unit-files", "--type=service", "--plain", "--no-legend", "--no-pager", "--full"}
	dfRCStatus      = []string{"/bin/rc-status", "-s", "-C"}
	dfRCUpdate      = []string{"/sbin/rc-update", "show", "-v"}
)

func dfShow(name string) []string {
	return []string{"/usr/bin/systemctl", "show", "--timestamp=unix", "-p",
		"Id,LoadState,ActiveState,SubState,UnitFileState,Description,MainPID,ActiveEnterTimestamp,InactiveEnterTimestamp,Result,NRestarts,MemoryCurrent,FragmentPath",
		"--", name + ".service"}
}

func dfIsEnabled(name string) []string {
	return []string{"/usr/bin/systemctl", "is-enabled", "--", name + ".service"}
}

func dfRCServiceStatus(name string) []string {
	return []string{"/sbin/rc-service", "-C", name, "status"}
}

// enabledWords is the FR-4 enabled enum.
var enabledWords = []string{"enabled", "disabled", "static", "masked", "indirect", "generated", "alias", "unknown"}

// stateWords are the six State words.
var stateWords = []string{"running", "stopped", "failed", "starting", "stopping", "unknown"}

// fr4Enabled is word when it is in the FR-4 enum, else unknown.
func fr4Enabled(word string) string {
	if word != "unknown" && slices.Contains(enabledWords, word) {
		return word
	}
	return "unknown"
}

// capturesOf lists the capture directories of init, failing when there is
// none, so no table runs vacuously.
func capturesOf(t *testing.T, init platform.InitSystem) []string {
	t.Helper()
	all, err := svcctltest.Captures()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range all {
		p, err := svcctltest.ReadProvenance(c)
		if err != nil {
			t.Fatal(err)
		}
		if p.Init() == init {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s capture under testdata/services", init)
	}
	return out
}

// captureText is a captured command's output, without its Exit line.
func captureText(t testing.TB, capture, file string) string {
	t.Helper()
	res, err := svcctltest.CaptureOutput(capture, file)
	if err != nil {
		t.Fatal(err)
	}
	return string(res.Output)
}

// oracleTSV reads NAME<TAB>WORD<TAB>EXIT into NAME -> WORD.
func oracleTSV(t *testing.T, capture, file string) map[string]string {
	t.Helper()
	dir, err := svcctltest.CaptureDir(capture)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for line := range strings.Lines(string(b)) {
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(f) != 3 {
			t.Fatalf("%s/%s: want NAME<TAB>WORD<TAB>EXIT, got %q", capture, file, line)
		}
		m[f[0]] = f[1]
	}
	return m
}

// existingSystemdState is svcctl's existing ActiveState mapping (the
// Controller's Status) applied to word.
func existingSystemdState(t *testing.T, word string) string {
	t.Helper()
	run := func(context.Context, []string) (svcctl.Result, error) {
		return svcctl.Result{Output: []byte("LoadState=loaded\nActiveState=" + word + "\nSubState=x\n")}, nil
	}
	c, err := svcctl.New(platform.Systemd, svcctl.Options{Run: run})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(t.Context(), "x")
	if err != nil {
		t.Fatal(err)
	}
	return string(st.State)
}

// existingOpenRCState is svcctl's existing rc-service status mapping
// applied to word.
func existingOpenRCState(t *testing.T, word string) string {
	t.Helper()
	run := func(_ context.Context, argv []string) (svcctl.Result, error) {
		if argv[1] == "--exists" {
			return svcctl.Result{}, nil
		}
		return svcctl.Result{Output: []byte(" * status: " + word + "\n")}, nil
	}
	c, err := svcctl.New(platform.OpenRC, svcctl.Options{Run: run, RunRoot: t.TempDir(), ProcRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(t.Context(), "x")
	if err != nil {
		t.Fatal(err)
	}
	return string(st.State)
}

// unitColumns splits list-units and list-unit-files lines into fields,
// keeping only .service lines.
func unitColumns(out string) [][]string {
	var rows [][]string
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) > 0 && strings.HasSuffix(f[0], ".service") {
			rows = append(rows, f)
		}
	}
	return rows
}

func byName(t *testing.T, entries []svcctl.Entry) map[string]svcctl.Entry {
	t.Helper()
	m := map[string]svcctl.Entry{}
	for _, e := range entries {
		if _, dup := m[e.Name]; dup {
			t.Errorf("%s listed twice", e.Name)
		}
		m[e.Name] = e
	}
	return m
}

func list(t *testing.T, init platform.InitSystem, opts svcctl.Options) []svcctl.Entry {
	t.Helper()
	entries, err := svcctl.List(t.Context(), init, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSortedFunc(entries, func(a, b svcctl.Entry) int { return strings.Compare(a.Name, b.Name) }) {
		t.Error("List is not sorted by name")
	}
	return entries
}

func setOf(names ...string) []string {
	s := slices.Clone(names)
	slices.Sort(s)
	return slices.Compact(s)
}

// TC-SV-46: List on systemd captures equals the union of the two listings,
// with states and enabled words from the oracle.
func TestTCSV46_ListOnSystemdCapturesEqualsTheUnion(t *testing.T) {
	for _, c := range capturesOf(t, platform.Systemd) {
		t.Run(c, func(t *testing.T) {
			fh := svcctltest.NewHost(t, c)
			got := byName(t, list(t, platform.Systemd, svcctl.Options{Run: fh.Run}))
			units := unitColumns(captureText(t, c, "list-units.txt"))
			files := unitColumns(captureText(t, c, "list-unit-files.txt"))
			var want []string
			notFound := map[string]bool{}
			hasFile := map[string]bool{}
			for _, f := range units {
				name := strings.TrimSuffix(f[0], ".service")
				want = append(want, name)
				notFound[name] = f[1] == "not-found"
			}
			for _, f := range files {
				name := strings.TrimSuffix(f[0], ".service")
				want = append(want, name)
				hasFile[name] = true
			}
			want = setOf(want...)
			if names := slices.Sorted(func(yield func(string) bool) {
				for n := range got {
					if !yield(n) {
						return
					}
				}
			}); !slices.Equal(names, want) {
				t.Fatalf("names %q, want the union of the listings %q", names, want)
			}
			active := oracleTSV(t, c, "oracle/is-active.tsv")
			enabled := oracleTSV(t, c, "oracle/is-enabled.tsv")
			for name, e := range got {
				word, ok := active[name+".service"]
				if !ok {
					t.Errorf("%s: no oracle/is-active.tsv line", name)
					continue
				}
				wantState := existingSystemdState(t, word)
				switch {
				case strings.HasSuffix(name, "@"):
					// A template is not a unit is-active can name (its oracle
					// word is empty); TC-SV-48 and test-strategy Open Item 3
					// decide it is stopped, as systemctl show reports.
					wantState = "stopped"
				case e.UnitFileOnly && e.EnabledRaw == "alias":
					// is-active follows an alias to its target, which no
					// listing names (procps, kmod, dbus-org.*): List cannot
					// know the state and says unknown rather than guess.
					wantState = "unknown"
				}
				if e.State != wantState {
					t.Errorf("%s: State %q, want %q (is-active %q)", name, e.State, wantState, word)
				}
				ew := enabled[name+".service"]
				if e.Enabled != fr4Enabled(ew) {
					t.Errorf("%s: Enabled %q, want %q (is-enabled %q)", name, e.Enabled, fr4Enabled(ew), ew)
				}
				wantRaw := ew
				if notFound[name] && !hasFile[name] {
					// No listing carries is-enabled's word for a unit with no
					// unit file: systemd 259 says not-found, 252 nothing.
					wantRaw = ""
				}
				if e.EnabledRaw != wantRaw {
					t.Errorf("%s: EnabledRaw %q, want %q", name, e.EnabledRaw, wantRaw)
				}
			}
		})
	}
}

// listUnitsLine splits a list-units line into UNIT LOAD ACTIVE SUB and the
// verbatim rest.
var listUnitsLine = regexp.MustCompile(`^(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(.*?)\s*$`)

// TC-SV-47: List keeps the description and raw words, and maps only exact
// enabled words.
func TestTCSV47_ListKeepsTheDescriptionAndRawWords(t *testing.T) {
	const c = "t1-ubuntu-systemd"
	lf := captureText(t, c, "list-unit-files.txt")
	// The capture has no cron.service; the first unit file whose STATE and
	// PRESET are both enabled is the template line.
	var tmpl []string
	for _, f := range unitColumns(lf) {
		if len(f) == 3 && f[1] == "enabled" && f[2] == "enabled" {
			tmpl = f
			break
		}
	}
	if tmpl == nil {
		t.Fatal("no `enabled enabled` line in list-unit-files.txt")
	}
	synthetic := map[string]string{}
	var extra strings.Builder
	for i, w := range []string{"enabled-runtime", "linked", "transient", "bad-setting", "Enabled"} {
		name := fmt.Sprintf("nx-syn%d", i)
		synthetic[name] = w
		fmt.Fprintf(&extra, "%s.service %s %s\n", name, w, tmpl[2])
	}
	fh := svcctltest.NewHost(t, c)
	fh.Answer(dfListUnitFiles, svcctl.Result{Output: []byte(lf + extra.String())}, nil)
	got := byName(t, list(t, platform.Systemd, svcctl.Options{Run: fh.Run}))
	for name, w := range synthetic {
		e := got[name]
		if e.Enabled != "unknown" || e.EnabledRaw != w {
			t.Errorf("%s: Enabled %q EnabledRaw %q, want unknown and %q", name, e.Enabled, e.EnabledRaw, w)
		}
		if !e.UnitFileOnly || e.DescriptionKnown {
			t.Errorf("%s: UnitFileOnly %v DescriptionKnown %v, want true and false", name, e.UnitFileOnly, e.DescriptionKnown)
		}
	}
	lines := 0
	for line := range strings.Lines(captureText(t, c, "list-units.txt")) {
		m := listUnitsLine.FindStringSubmatch(strings.TrimRight(line, "\n"))
		if m == nil || !strings.HasSuffix(m[1], ".service") {
			continue
		}
		lines++
		e := got[strings.TrimSuffix(m[1], ".service")]
		if want := m[3] + " (" + m[4] + ")"; e.RawState != want {
			t.Errorf("%s: RawState %q, want %q", m[1], e.RawState, want)
		}
		if e.Description != m[5] || !e.DescriptionKnown {
			t.Errorf("%s: Description %q known %v, want %q known", m[1], e.Description, e.DescriptionKnown, m[5])
		}
		if e.UnitFileOnly {
			t.Errorf("%s: a loaded unit is UnitFileOnly", m[1])
		}
	}
	if lines == 0 {
		t.Fatal("no list-units line parsed")
	}
}

// TC-SV-48: Template units appear only from unit files, as stopped.
func TestTCSV48_TemplateUnitsAppearOnlyFromUnitFiles(t *testing.T) {
	for _, c := range capturesOf(t, platform.Systemd) {
		t.Run(c, func(t *testing.T) {
			fh := svcctltest.NewHost(t, c)
			got := byName(t, list(t, platform.Systemd, svcctl.Options{Run: fh.Run}))
			templates := 0
			for _, f := range unitColumns(captureText(t, c, "list-unit-files.txt")) {
				name := strings.TrimSuffix(f[0], ".service")
				if !strings.HasSuffix(name, "@") {
					continue
				}
				templates++
				e := got[name]
				if !e.UnitFileOnly || e.State != "stopped" || e.RawState != "inactive (dead)" ||
					e.Enabled != fr4Enabled(f[1]) || e.EnabledRaw != f[1] {
					t.Errorf("%s: %+v, want UnitFileOnly, stopped, inactive (dead), enabled from %q", name, e, f[1])
				}
			}
			if _, ok := got["getty@"]; !ok || templates == 0 {
				t.Errorf("getty@ not listed (%d templates)", templates)
			}
			instances := 0
			for _, f := range unitColumns(captureText(t, c, "list-units.txt")) {
				name := strings.TrimSuffix(f[0], ".service")
				if !strings.Contains(name, "@") {
					continue
				}
				instances++
				e := got[name]
				if e.UnitFileOnly || e.State != existingSystemdState(t, f[2]) || e.RawState != f[2]+" ("+f[3]+")" {
					t.Errorf("%s: %+v, want its loaded state %s (%s)", name, e, f[2], f[3])
				}
			}
			if instances == 0 {
				t.Error("no loaded template instance in list-units.txt")
			}
			for _, e := range got {
				if e.UnitFileOnly && e.State == "running" {
					t.Errorf("%s: a UnitFileOnly entry is running", e.Name)
				}
			}
		})
	}
}

// rcStatusLine is " NAME [ WORD ... ]".
var rcStatusLine = regexp.MustCompile(`^\s*(\S+)\s+\[\s*(\S+)[^\]]*\]\s*$`)

// literalDescription is the test's own reading of a static description: a
// single description="..." line whose value has no expansion.
var literalDescription = regexp.MustCompile(`^description="([^"$` + "`" + `\\]*)"$`)

func staticDescription(t *testing.T, etc, name string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(etc, "init.d", name))
	if err != nil {
		return "", false
	}
	var vals []string
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\n")
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "description=") {
			m := literalDescription.FindStringSubmatch(line)
			if m == nil {
				return "", false
			}
			vals = append(vals, m[1])
		}
	}
	if vals = setOf(vals...); len(vals) != 1 {
		return "", false
	}
	return vals[0], true
}

// TC-SV-49: List on OpenRC captures matches rc-status, rc-update and the
// rc-service oracle.
func TestTCSV49_ListOnOpenRCCapturesMatchesRcStatus(t *testing.T) {
	for _, c := range capturesOf(t, platform.OpenRC) {
		t.Run(c, func(t *testing.T) {
			dir, err := svcctltest.CaptureDir(c)
			if err != nil {
				t.Fatal(err)
			}
			etc := copyEtc(t, dir)
			fh := svcctltest.NewHost(t, c)
			got := byName(t, list(t, platform.OpenRC, svcctl.Options{Run: fh.Run, EtcRoot: etc}))
			bracket := map[string]string{}
			for line := range strings.Lines(captureText(t, c, "rc-status.txt")) {
				if m := rcStatusLine.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil {
					bracket[m[1]] = m[2]
				}
			}
			runlevels := map[string][]string{}
			for line := range strings.Lines(captureText(t, c, "rc-update.txt")) {
				if l, r, ok := strings.Cut(line, "|"); ok {
					runlevels[strings.TrimSpace(l)] = strings.Fields(r)
				}
			}
			var want []string
			for n := range bracket {
				want = append(want, n)
			}
			for n := range runlevels {
				want = append(want, n)
			}
			want = setOf(want...)
			var names []string
			for n := range got {
				names = append(names, n)
			}
			if slices.Sort(names); !slices.Equal(names, want) {
				t.Fatalf("names %q, want rc-update ∪ rc-status %q", names, want)
			}
			oracle := oracleTSV(t, c, "oracle/rc-service-status.tsv")
			described := 0
			for name, e := range got {
				word, inStatus := bracket[name]
				if !inStatus {
					// rc-status -s leaves out scripts excluded by keyword
					// (-docker); rc-service reports them stopped.
					word = "stopped"
				}
				if e.State != existingOpenRCState(t, word) || e.RawState != word {
					t.Errorf("%s: State %q RawState %q, want %q from %q", name, e.State, e.RawState, existingOpenRCState(t, word), word)
				}
				if ow, ok := oracle[name]; ok && existingOpenRCState(t, ow) != e.State {
					t.Errorf("%s: State %q disagrees with rc-service status %q", name, e.State, ow)
				}
				if !slices.Equal(e.Runlevels, runlevels[name]) && len(e.Runlevels)+len(runlevels[name]) > 0 {
					t.Errorf("%s: Runlevels %q, want %q", name, e.Runlevels, runlevels[name])
				}
				wantEnabled := "disabled"
				if len(runlevels[name]) > 0 {
					wantEnabled = "enabled"
				}
				if e.Enabled != wantEnabled {
					t.Errorf("%s: Enabled %q, want %q", name, e.Enabled, wantEnabled)
				}
				d, known := staticDescription(t, etc, name)
				if e.DescriptionKnown != known || e.Description != d {
					t.Errorf("%s: Description %q known %v, want %q known %v", name, e.Description, e.DescriptionKnown, d, known)
				}
				if known {
					described++
				}
			}
			if described == 0 {
				t.Error("no static description was read")
			}
		})
	}
}

// copyEtc is F-ROOTS for the etc tree: a copy of the capture's etc/ in a
// temp root, so a variant edits the copy and never the committed capture.
func copyEtc(t *testing.T, captureDir string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "etc")
	src := filepath.Join(captureDir, "etc")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(root, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TC-SV-50: The failed set equals the oracle's failed services and
// ListFailed's .service subset.
func TestTCSV50_TheFailedSetEqualsTheOraclesFailed(t *testing.T) {
	failedOf := func(entries []svcctl.Entry) []string {
		var out []string
		for _, e := range entries {
			if e.State == "failed" {
				out = append(out, e.Name)
			}
		}
		return setOf(out...)
	}
	for _, c := range capturesOf(t, platform.Systemd) {
		t.Run(c, func(t *testing.T) {
			fh := svcctltest.NewHost(t, c)
			got := failedOf(list(t, platform.Systemd, svcctl.Options{Run: fh.Run}))
			var oracle []string
			for _, f := range unitColumns(captureText(t, c, "oracle/failed.txt")) {
				oracle = append(oracle, strings.TrimSuffix(f[0], ".service"))
			}
			oracle = setOf(oracle...)
			lf, err := svcctl.ListFailed(t.Context(), platform.Systemd, svcctl.Options{Run: fh.Run})
			if err != nil {
				t.Fatal(err)
			}
			var subset []string
			for _, n := range lf.Names {
				if s, ok := strings.CutSuffix(n, ".service"); ok {
					subset = append(subset, s)
				}
			}
			if !slices.Equal(got, oracle) || !slices.Equal(oracle, setOf(subset...)) {
				t.Errorf("failed %q, oracle %q, ListFailed .service %q: want all equal", got, oracle, subset)
			}
			if strings.HasPrefix(c, "t1-") && !slices.Contains(got, "nx-svc-fail") {
				t.Errorf("failed %q has no nx-svc-fail", got)
			}
		})
	}
	// OpenRC: a failed start-stop-daemon start leaves the script stopped,
	// not crashed (actions/start-nx-svc-fail.txt and the oracle), so the
	// failed set is the oracle's crashed set, which is empty in T1.
	for _, c := range capturesOf(t, platform.OpenRC) {
		t.Run(c, func(t *testing.T) {
			fh := svcctltest.NewHost(t, c)
			got := failedOf(list(t, platform.OpenRC, svcctl.Options{Run: fh.Run, EtcRoot: t.TempDir()}))
			var oracle []string
			for n, w := range oracleTSV(t, c, "oracle/rc-service-status.tsv") {
				if w == "crashed" {
					oracle = append(oracle, n)
				}
			}
			lf, err := svcctl.ListFailed(t.Context(), platform.OpenRC, svcctl.Options{Run: fh.Run})
			if err != nil {
				t.Fatal(err)
			}
			if oracle = setOf(oracle...); !slices.Equal(got, oracle) || !slices.Equal(oracle, setOf(lf.Names...)) {
				t.Errorf("failed %q, oracle crashed %q, ListFailed %q: want all equal", got, oracle, lf.Names)
			}
		})
	}
}

// TC-SV-51: Names outside ServiceNamePattern are listed verbatim.
func TestTCSV51_NamesOutsideServiceNamePatternAreListedVerbatim(t *testing.T) {
	const c = "t1-ubuntu-systemd"
	td, err := svcctltest.TestdataDir()
	if err != nil {
		t.Fatal(err)
	}
	escaped, err := os.ReadFile(filepath.Join(td, "systemd", "list-units-failed-escaped.txt"))
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("n", 65)
	lu := captureText(t, c, "list-units.txt") + string(escaped) + long + ".service loaded active running long name\n"
	fh := svcctltest.NewHost(t, c)
	fh.Answer(dfListUnits, svcctl.Result{Output: []byte(lu)}, nil)
	got := byName(t, list(t, platform.Systemd, svcctl.Options{Run: fh.Run}))
	pattern := regexp.MustCompile(svcctl.ServiceNamePattern)
	for _, name := range []string{`tc-hp\x2dfail`, long} {
		if _, ok := got[name]; !ok {
			t.Errorf("%q is not listed", name)
		}
		if pattern.MatchString(name) {
			t.Errorf("%q matches ServiceNamePattern; the case is vacuous", name)
		}
	}
}

// showCaptures are the capture's DF-1 show files with the name each was
// captured for.
func showCaptures(t *testing.T, c string) map[string]string {
	t.Helper()
	p, err := svcctltest.ReadProvenance(c)
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.Join(dfShow("")[:6], " ") + " "
	out := map[string]string{}
	for file, argv := range p.Commands {
		if rest, ok := strings.CutPrefix(argv, prefix); ok && strings.HasPrefix(file, "show-") {
			out[file] = strings.TrimSuffix(strings.TrimPrefix(rest, "-- "), ".service")
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no DF-1 show capture", c)
	}
	return out
}

func kvOf(out string) map[string]string {
	kv := map[string]string{}
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func unixAt(v string) *time.Time {
	s, ok := strings.CutPrefix(v, "@")
	if !ok {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	tm := time.Unix(n, 0)
	return &tm
}

func sameTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

// TC-SV-52: Describe on systemd returns every DF-1 field from the show
// capture.
func TestTCSV52_DescribeOnSystemdReturnsEveryDF1(t *testing.T) {
	for _, c := range capturesOf(t, platform.Systemd) {
		for file, name := range showCaptures(t, c) {
			t.Run(c+"/"+file, func(t *testing.T) {
				fh := svcctltest.NewHost(t, c)
				d, err := svcctl.Describe(t.Context(), platform.Systemd, name, svcctl.Options{Run: fh.Run})
				if err != nil {
					t.Fatal(err)
				}
				kv := kvOf(captureText(t, c, file))
				if d.Name != name || d.Known != (kv["LoadState"] != "not-found") ||
					d.CanonicalName != strings.TrimSuffix(kv["Id"], ".service") {
					t.Errorf("Name %q Known %v CanonicalName %q, want %q, LoadState %q, Id %q", d.Name, d.Known,
						d.CanonicalName, name, kv["LoadState"], kv["Id"])
				}
				if d.State != existingSystemdState(t, kv["ActiveState"]) || d.SubState != kv["SubState"] || d.Result != kv["Result"] {
					t.Errorf("State %q SubState %q Result %q, want from %q %q %q", d.State, d.SubState, d.Result,
						kv["ActiveState"], kv["SubState"], kv["Result"])
				}
				since := unixAt(kv["InactiveEnterTimestamp"])
				if kv["ActiveState"] == "active" {
					since = unixAt(kv["ActiveEnterTimestamp"])
				}
				if !sameTime(d.Since, since) {
					t.Errorf("Since %v, want %v", d.Since, since)
				}
				if pid, _ := strconv.Atoi(kv["MainPID"]); d.MainPID != pid {
					t.Errorf("MainPID %d, want %d", d.MainPID, pid)
				}
				if n, err := strconv.Atoi(kv["NRestarts"]); err != nil || d.Restarts == nil || *d.Restarts != n {
					t.Errorf("Restarts %v, want %q", d.Restarts, kv["NRestarts"])
				}
				if m, err := strconv.ParseInt(kv["MemoryCurrent"], 10, 64); err == nil {
					if d.MemoryBytes == nil || *d.MemoryBytes != m {
						t.Errorf("MemoryBytes %v, want %d", d.MemoryBytes, m)
					}
				} else if d.MemoryBytes != nil || d.Unavailable["memoryBytes"] == "" {
					t.Errorf("MemoryBytes %v (%q), want nil with a reason for %q", d.MemoryBytes, d.Unavailable["memoryBytes"], kv["MemoryCurrent"])
				}
				if d.UnitFilePath != kv["FragmentPath"] || d.Enabled != fr4Enabled(kv["UnitFileState"]) ||
					d.EnabledRaw != kv["UnitFileState"] || d.Description != kv["Description"] || !d.DescriptionKnown {
					t.Errorf("UnitFilePath %q Enabled %q/%q Description %q, want %q %q %q", d.UnitFilePath, d.Enabled,
						d.EnabledRaw, d.Description, kv["FragmentPath"], kv["UnitFileState"], kv["Description"])
				}
			})
		}
	}
}

// TC-SV-53: Describe reports an unknown unit as not known, without an error.
func TestTCSV53_DescribeReportsAnUnknownUnitAsNot(t *testing.T) {
	for _, tc := range []struct {
		capture string
		init    platform.InitSystem
		argv    []string
	}{
		{"t1-ubuntu-systemd", platform.Systemd, dfShow("nx-nope")},
		{"t1-alpine-openrc", platform.OpenRC, dfRCServiceStatus("nx-nope")},
	} {
		t.Run(tc.capture, func(t *testing.T) {
			fh := svcctltest.NewHost(t, tc.capture)
			d, err := svcctl.Describe(t.Context(), tc.init, "nx-nope", svcctl.Options{Run: fh.Run, EtcRoot: t.TempDir()})
			if err != nil || d.Known {
				t.Fatalf("Describe: %+v, %v; want not known and no error", d, err)
			}
			calls := fh.Calls()
			i := slices.IndexFunc(calls, func(c svcctltest.Call) bool { return slices.Equal(c.Argv, tc.argv) })
			if i < 0 {
				t.Fatalf("Describe did not send %q", tc.argv)
			}
			wantExit := 0
			if tc.init == platform.OpenRC {
				rows, err := svcctltest.Provenance()
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rows {
					if r.File == "openrc/absent.txt" {
						wantExit, _ = strconv.Atoi(r.Exit)
					}
				}
			}
			if calls[i].Result.ExitCode != wantExit {
				t.Errorf("the fake answered exit %d, want the captured %d", calls[i].Result.ExitCode, wantExit)
			}
		})
	}
}

// TC-SV-54: Describe returns the canonical unit of an alias.
func TestTCSV54_DescribeReturnsTheCanonicalUnitOfAn(t *testing.T) {
	for _, c := range capturesOf(t, platform.Systemd) {
		t.Run(c, func(t *testing.T) {
			aliases := 0
			for file, name := range showCaptures(t, c) {
				id := strings.TrimSuffix(kvOf(captureText(t, c, file))["Id"], ".service")
				if id == name {
					continue
				}
				aliases++
				fh := svcctltest.NewHost(t, c)
				d, err := svcctl.Describe(t.Context(), platform.Systemd, name, svcctl.Options{Run: fh.Run})
				if err != nil {
					t.Fatal(err)
				}
				if d.CanonicalName != id || d.Name != name {
					t.Errorf("%s: Name %q CanonicalName %q, want %q and %q", file, d.Name, d.CanonicalName, name, id)
				}
			}
			if strings.HasPrefix(c, "t1-") && aliases == 0 {
				t.Error("no alias show capture (show-alias.txt)")
			}
		})
	}
}

// TC-SV-55: Describe on OpenRC returns status, runlevels, script path and a
// static description.
func TestTCSV55_DescribeOnOpenRCReturnsStatusRunlevelsScript(t *testing.T) {
	const c = "t1-alpine-openrc"
	dir, err := svcctltest.CaptureDir(c)
	if err != nil {
		t.Fatal(err)
	}
	etc := copyEtc(t, dir)
	oracle := oracleTSV(t, c, "oracle/rc-service-status.tsv")
	runlevels := map[string][]string{}
	for line := range strings.Lines(captureText(t, c, "rc-update.txt")) {
		if l, r, ok := strings.Cut(line, "|"); ok {
			runlevels[strings.TrimSpace(l)] = strings.Fields(r)
		}
	}
	for _, name := range []string{"nexusd", "nx-svc-test"} {
		fh := svcctltest.NewHost(t, c)
		d, err := svcctl.Describe(t.Context(), platform.OpenRC, name, svcctl.Options{Run: fh.Run, EtcRoot: etc,
			RunRoot: filepath.Join(dir, "run"), ProcRoot: filepath.Join(dir, "proc")})
		if err != nil {
			t.Fatal(err)
		}
		if !d.Known || d.State != existingOpenRCState(t, oracle[name]) || d.RawState != oracle[name] {
			t.Errorf("%s: Known %v State %q RawState %q, want %q", name, d.Known, d.State, d.RawState, oracle[name])
		}
		if len(d.Runlevels)+len(runlevels[name]) > 0 && !slices.Equal(d.Runlevels, runlevels[name]) {
			t.Errorf("%s: Runlevels %q, want %q", name, d.Runlevels, runlevels[name])
		}
		if d.ScriptPath != "/etc/init.d/"+name {
			t.Errorf("%s: ScriptPath %q", name, d.ScriptPath)
		}
		if d.Since != nil || !strings.Contains(d.Unavailable["since"], "OpenRC") {
			t.Errorf("%s: Since %v (%q), want nil with a reason naming OpenRC", name, d.Since, d.Unavailable["since"])
		}
		if d.SubState != "" || d.Result != "" || d.Restarts != nil || d.MemoryBytes != nil {
			t.Errorf("%s: SubState %q Result %q Restarts %v MemoryBytes %v, want none", name, d.SubState, d.Result, d.Restarts, d.MemoryBytes)
		}
		for _, k := range []string{"subState", "result", "restarts", "memoryBytes"} {
			if d.Unavailable[k] == "" {
				t.Errorf("%s: no reason for %s", name, k)
			}
		}
		want, known := staticDescription(t, etc, name)
		if !known || d.Description != want || !d.DescriptionKnown {
			t.Errorf("%s: Description %q known %v, want the literal %q (%v)", name, d.Description, d.DescriptionKnown, want, known)
		}
	}
	if err := os.WriteFile(filepath.Join(etc, "init.d", "nx-svc-test"), []byte("x=1\ndescription=\"$x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fh := svcctltest.NewHost(t, c)
	d, err := svcctl.Describe(t.Context(), platform.OpenRC, "nx-svc-test", svcctl.Options{Run: fh.Run, EtcRoot: etc})
	if err != nil {
		t.Fatal(err)
	}
	if d.DescriptionKnown || d.Description != "" {
		t.Errorf("a description from a variable: %q known %v, want unknown", d.Description, d.DescriptionKnown)
	}
}

// describeMainPID runs Describe on the t1-alpine-openrc capture's started
// nexusd with a fresh run and proc root, so MainPID comes only from what
// plant lays out there, and returns the MainPID.
func describeMainPID(t *testing.T, plant func(run, proc string)) int {
	t.Helper()
	const c = "t1-alpine-openrc"
	dir, err := svcctltest.CaptureDir(c)
	if err != nil {
		t.Fatal(err)
	}
	run, proc := t.TempDir(), t.TempDir()
	plant(run, proc)
	fh := svcctltest.NewHost(t, c)
	d, err := svcctl.Describe(t.Context(), platform.OpenRC, "nexusd", svcctl.Options{Run: fh.Run,
		EtcRoot: copyEtc(t, dir), RunRoot: run, ProcRoot: proc})
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "running" {
		t.Fatalf("State %q, want running", d.State)
	}
	return d.MainPID
}

func plantProcs(t *testing.T, proc string, pids ...int) {
	t.Helper()
	for _, p := range pids {
		if err := os.MkdirAll(filepath.Join(proc, strconv.Itoa(p)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TC-SV-55 (supervise-daemon): a service under supervise-daemon has a
// daemons record whose pidfile is the supervisor's (live: `pidfile=/var/run/
// supervise-<name>.pid` beside options/<name>/child_pid), and MainPID is the
// service's own process, the child_pid, not the supervisor.
func TestTCSV55_DescribeOnOpenRCSupervisedServiceReportsTheChildPID(t *testing.T) {
	const supervisor, child = 2125, 2126
	got := describeMainPID(t, func(run, proc string) {
		plantProcs(t, proc, supervisor, child)
		pidfile := filepath.Join(run, "supervise-nexusd.pid")
		if err := svcctltest.WriteDaemonRecord(run, "nexusd", pidfile, supervisor); err != nil {
			t.Fatal(err)
		}
		opts := filepath.Join(run, "openrc", "options", "nexusd")
		if err := os.MkdirAll(opts, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(opts, "child_pid"), []byte(strconv.Itoa(child)), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got != child {
		t.Errorf("MainPID %d, want the child %d (not the supervisor %d)", got, child, supervisor)
	}
}

// A supervised service whose child is not live reports no PID, never the
// supervisor's.
func TestTCSV55_DescribeOnOpenRCSupervisedServiceWithADeadChildHasNoPID(t *testing.T) {
	const supervisor, child = 2125, 2126
	got := describeMainPID(t, func(run, proc string) {
		plantProcs(t, proc, supervisor)
		if err := svcctltest.WriteDaemonRecord(run, "nexusd", filepath.Join(run, "supervise-nexusd.pid"), supervisor); err != nil {
			t.Fatal(err)
		}
		opts := filepath.Join(run, "openrc", "options", "nexusd")
		if err := os.MkdirAll(opts, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(opts, "child_pid"), []byte(strconv.Itoa(child)), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got != 0 {
		t.Errorf("MainPID %d, want 0", got)
	}
}

// A start-stop-daemon service has a daemons record and no child_pid:
// MainPID is the pidfile's PID.
func TestTCSV55_DescribeOnOpenRCStartStopDaemonServiceReportsThePidfilePID(t *testing.T) {
	const pid = 3131
	got := describeMainPID(t, func(run, proc string) {
		plantProcs(t, proc, pid)
		if err := svcctltest.WriteDaemonRecord(run, "nexusd", filepath.Join(run, "nexusd.pid"), pid); err != nil {
			t.Fatal(err)
		}
	})
	if got != pid {
		t.Errorf("MainPID %d, want the pidfile's %d", got, pid)
	}
}

// TC-SV-56: EnabledAtBoot follows is-enabled on systemd and the boot or
// default runlevel on OpenRC.
func TestTCSV56_EnabledAtBootFollowsIsEnabledOnSystemdAnd(t *testing.T) {
	// Each word with the exit code a capture recorded for it, from the
	// captured is-enabled-nexusd.txt files and every oracle/is-enabled.tsv.
	exits := map[string]int{}
	for _, c := range capturesOf(t, platform.Systemd) {
		res, err := svcctltest.CaptureOutput(c, "is-enabled-nexusd.txt")
		if err != nil {
			t.Fatal(err)
		}
		exits[strings.TrimSpace(string(res.Output))] = res.ExitCode
		dir, _ := svcctltest.CaptureDir(c)
		b, err := os.ReadFile(filepath.Join(dir, "oracle", "is-enabled.tsv"))
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.Lines(string(b)) {
			if f := strings.Split(strings.TrimRight(line, "\n"), "\t"); len(f) == 3 && f[1] != "" {
				exits[f[1]], _ = strconv.Atoi(f[2])
			}
		}
	}
	// Words no T1 image has: the exit codes of systemctl(1)'s is-enabled
	// table (indirect 0, linked > 0).
	for w, code := range map[string]int{"indirect": 0, "linked": 1} {
		if _, ok := exits[w]; !ok {
			exits[w] = code
		}
	}
	for _, w := range []string{"enabled", "disabled", "static", "masked", "enabled-runtime", "indirect", "linked"} {
		code, ok := exits[w]
		if !ok {
			t.Fatalf("no exit code for %q", w)
		}
		fh := svcctltest.NewHost(t, "t1-ubuntu-systemd")
		fh.Answer(dfIsEnabled("nexusd"), svcctl.Result{ExitCode: code, Output: []byte(w + "\n")}, nil)
		on, word, err := svcctl.EnabledAtBoot(t.Context(), platform.Systemd, "nexusd", svcctl.Options{Run: fh.Run})
		if err != nil || on != (w == "enabled") || word != w {
			t.Errorf("is-enabled %q (exit %d): %v %q %v, want %v %q", w, code, on, word, err, w == "enabled", w)
		}
	}
	for _, tc := range []struct {
		line, word string
		on         bool
	}{
		{"default", "default", true}, {"boot", "boot", true}, {"sysinit", "sysinit", false}, {"", "", false},
	} {
		fh := svcctltest.NewHost(t, "t1-alpine-openrc")
		fh.Answer(dfRCUpdate, svcctl.Result{Output: []byte("              crond | \n             nexusd | " + tc.line + "\n")}, nil)
		on, word, err := svcctl.EnabledAtBoot(t.Context(), platform.OpenRC, "nexusd", svcctl.Options{Run: fh.Run})
		if err != nil || on != tc.on || word != tc.word {
			t.Errorf("nexusd | %s: %v %q %v, want %v %q", tc.line, on, word, err, tc.on, tc.word)
		}
	}
	// The captured state of each capture agrees with its own files.
	for _, c := range capturesOf(t, platform.Systemd) {
		fh := svcctltest.NewHost(t, c)
		raw := strings.TrimSpace(captureText(t, c, "is-enabled-nexusd.txt"))
		on, word, err := svcctl.EnabledAtBoot(t.Context(), platform.Systemd, "nexusd", svcctl.Options{Run: fh.Run})
		if err != nil || word != raw || on != (raw == "enabled") {
			t.Errorf("%s: %v %q %v, want %q", c, on, word, err, raw)
		}
	}
}

// TC-SV-57: List and Describe send exactly the fixed argv and never request
// a sensitive property.
func TestTCSV57_ListAndDescribeSendExactlyTheFixed(t *testing.T) {
	for _, tc := range []struct {
		init platform.InitSystem
		want [][]string
	}{
		{platform.Systemd, [][]string{dfListUnits, dfListUnitFiles, dfShow("nexusd")}},
		{platform.OpenRC, [][]string{dfRCStatus, dfRCUpdate, dfRCServiceStatus("nexusd")}},
	} {
		for _, c := range capturesOf(t, tc.init) {
			t.Run(c, func(t *testing.T) {
				fh := svcctltest.NewHost(t, c)
				opts := svcctl.Options{Run: fh.Run, EtcRoot: t.TempDir(), RunRoot: t.TempDir(), ProcRoot: t.TempDir()}
				list(t, tc.init, opts)
				if _, err := svcctl.Describe(t.Context(), tc.init, "nexusd", opts); err != nil {
					t.Fatal(err)
				}
				var got [][]string
				for _, call := range fh.Calls() {
					if !slices.ContainsFunc(got, func(a []string) bool { return slices.Equal(a, call.Argv) }) {
						got = append(got, call.Argv)
					}
					for _, a := range call.Argv {
						if a == "cat" || strings.Contains(a, "Environment") || strings.Contains(a, "ExecStart") {
							t.Errorf("argv %q requests a sensitive property", call.Argv)
						}
					}
				}
				cmp := func(a, b []string) int { return strings.Compare(strings.Join(a, " "), strings.Join(b, " ")) }
				want := slices.Clone(tc.want)
				slices.SortFunc(got, cmp)
				slices.SortFunc(want, cmp)
				if !slices.EqualFunc(got, want, slices.Equal) {
					t.Errorf("calls %q, want exactly %q", got, want)
				}
				if len(fh.Sent()) != 0 {
					t.Errorf("sent %q", fh.Sent())
				}
			})
		}
	}
}

// TC-SV-58: Callers that do not set OutputCap keep the old cap and behavior.
func TestTCSV58_CallersThatDoNotSetOutputCapKeep(t *testing.T) {
	zeros := func(n int) []string { return []string{"/bin/sh", "-c", fmt.Sprintf("head -c %d /dev/zero", n)} }
	for _, tc := range []struct {
		cap, n, want int
		truncated    bool
	}{
		{0, 100 << 10, svcctl.OutputCap, false},
		{4 << 20, 5 << 20, 4 << 20, true},
		{4 << 20, 1 << 20, 1 << 20, false},
		{4 << 20, 4 << 20, 4 << 20, false},
	} {
		res, err := svcctl.Call(t.Context(), zeros(tc.n), svcctl.Options{OutputCap: tc.cap, Timeout: 30 * time.Second})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("cap %d, %d bytes: %v, exit %d", tc.cap, tc.n, err, res.ExitCode)
		}
		if len(res.Output) != tc.want || res.Truncated != tc.truncated {
			t.Errorf("cap %d, %d bytes: %d bytes, Truncated %v; want %d, %v", tc.cap, tc.n, len(res.Output), res.Truncated, tc.want, tc.truncated)
		}
	}
	// The exported runner is unchanged: 64 KiB, Truncated never reported.
	res, err := svcctl.Run(t.Context(), zeros(100<<10))
	if err != nil || len(res.Output) != 64<<10 || svcctl.OutputCap != 64<<10 || res.Truncated {
		t.Errorf("Run: %v, %d bytes, Truncated %v; want 64 KiB, false", err, len(res.Output), res.Truncated)
	}
}

// TC-SV-59: F-HOSTS encodes the captured init-system behavior.
func TestTCSV59_FHOSTSEncodesTheCapturedInitSystem(t *testing.T) {
	ctx := t.Context()
	const sc = "t1-ubuntu-systemd"
	fh := svcctltest.NewHost(t, sc)
	fh.FailsOnStart("nx-svc-fail")
	opts := svcctl.Options{Run: fh.Run}
	ctl, err := svcctl.New(platform.Systemd, opts)
	if err != nil {
		t.Fatal(err)
	}
	describe := func(name string) svcctl.Details {
		t.Helper()
		d, err := svcctl.Describe(ctx, platform.Systemd, name, opts)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	do := func(a svcctl.Action, name string) svcctl.Result {
		t.Helper()
		res, err := ctl.Do(ctx, a, name)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// nx-svc-test is running in the capture: stop it first.
	if res := do(svcctl.Stop, "nx-svc-test"); res.ExitCode != 0 {
		t.Fatalf("stop nx-svc-test exited %d", res.ExitCode)
	}
	before := describe("nx-svc-test")
	if before.State != "stopped" {
		t.Fatalf("after stop: %q", before.State)
	}
	prevPID := describe("nexusd").MainPID
	if res := do(svcctl.Start, "nx-svc-test"); res.ExitCode != 0 || len(res.Output) != 0 {
		t.Errorf("start: exit %d %q, want 0 and no output", res.ExitCode, res.Output)
	}
	for i := range 2 {
		if d := describe("nx-svc-test"); d.State != "starting" || d.RawState != "activating (start)" {
			t.Errorf("poll %d: %q %q, want activating", i+1, d.State, d.RawState)
		}
	}
	after := describe("nx-svc-test")
	if after.State != "running" || after.MainPID <= 0 || after.MainPID == prevPID || after.Since == nil ||
		before.Since != nil && !after.Since.After(*before.Since) {
		t.Errorf("poll 3: %+v, want active with a new MainPID and a later ActiveEnterTimestamp", after)
	}
	failOut := captureText(t, sc, "actions/start-nx-svc-fail.txt")
	if res := do(svcctl.Start, "nx-svc-fail"); res.ExitCode != 1 || string(res.Output) != failOut {
		t.Errorf("failed start: exit %d %q, want 1 %q", res.ExitCode, res.Output, failOut)
	}
	if d := describe("nx-svc-fail"); d.State != "failed" || d.SubState != "failed" || d.Result != "exit-code" || d.MainPID != 0 {
		t.Errorf("after the failed start: %+v", d)
	}
	if res := do(svcctl.Stop, "nx-svc-fail"); res.ExitCode != 0 {
		t.Errorf("stop of the failed unit exited %d", res.ExitCode)
	}
	if d := describe("nx-svc-fail"); d.State != "failed" {
		t.Errorf("after stop the failed unit is %q, want failed", d.State)
	}
	if res, err := fh.Run(ctx, dfShow("nx-nope")); err != nil || res.ExitCode != 0 ||
		kvOf(string(res.Output))["LoadState"] != "not-found" {
		t.Errorf("show nx-nope: %v exit %d %q, want exit 0 with LoadState=not-found", err, res.ExitCode, res.Output)
	}
	for _, argv := range [][]string{{"/usr/bin/systemctl", "start", "nx-svc-test.service; reboot"}, {"systemctl", "show"}} {
		rec := &tRecorder{}
		bad := svcctltest.NewHost(rec, sc)
		if _, err := bad.Run(ctx, argv); !errors.Is(err, svcctl.ErrNotStarted) || len(rec.errs) != 1 {
			t.Errorf("%q: %v, %d test errors; want ErrNotStarted and one test error", argv, err, len(rec.errs))
		}
	}
	wantSent := [][]string{
		{"/usr/bin/systemctl", "stop", "--", "nx-svc-test.service"},
		{"/usr/bin/systemctl", "start", "--", "nx-svc-test.service"},
		{"/usr/bin/systemctl", "start", "--", "nx-svc-fail.service"},
		{"/usr/bin/systemctl", "stop", "--", "nx-svc-fail.service"},
	}
	if !slices.EqualFunc(fh.Sent(), wantSent, slices.Equal) {
		t.Errorf("Sent %q, want %q", fh.Sent(), wantSent)
	}

	const oc = "t1-alpine-openrc"
	oh := svcctltest.NewHost(t, oc)
	octl, err := svcctl.New(platform.OpenRC, svcctl.Options{Run: oh.Run, RunRoot: t.TempDir(), ProcRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	already := captureText(t, oc, "actions/start-already-nx-svc-test.txt")
	for i := range 2 {
		res, err := octl.Do(ctx, svcctl.Start, "nx-svc-test")
		if err != nil || res.ExitCode != 0 || string(res.Output) != already {
			t.Errorf("OpenRC start %d: %v exit %d %q, want 0 %q", i+1, err, res.ExitCode, res.Output, already)
		}
	}
	if st, err := octl.Status(ctx, "nx-svc-test"); err != nil || st.State != svcctl.Running {
		t.Errorf("after the second start: %+v %v, want running unchanged", st, err)
	}
	if want := [][]string{{"/sbin/rc-service", "-C", "nx-svc-test", "start"}, {"/sbin/rc-service", "-C", "nx-svc-test", "start"}}; !slices.EqualFunc(oh.Sent(), want, slices.Equal) {
		t.Errorf("OpenRC Sent %q, want %q", oh.Sent(), want)
	}
}

// fuzzSeedFiles are the seed files of each fuzz target, by capture: every
// capture of the parser's init system contributes its files.
var fuzzSeedFiles = map[string]struct {
	init  platform.InitSystem
	files func(t testing.TB, capture string) []string
}{
	"FuzzParseListUnits":     {platform.Systemd, fixedSeed("list-units.txt")},
	"FuzzParseListUnitFiles": {platform.Systemd, fixedSeed("list-unit-files.txt")},
	"FuzzParseShow":          {platform.Systemd, showSeeds},
	"FuzzParseRCStatus":      {platform.OpenRC, fixedSeed("rc-status.txt")},
	"FuzzParseRCUpdate":      {platform.OpenRC, fixedSeed("rc-update.txt")},
}

func fixedSeed(file string) func(testing.TB, string) []string {
	return func(t testing.TB, capture string) []string {
		dir, err := svcctltest.CaptureDir(capture)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			return nil
		}
		return []string{file}
	}
}

func showSeeds(t testing.TB, capture string) []string {
	dir, err := svcctltest.CaptureDir(capture)
	if err != nil {
		t.Fatal(err)
	}
	m, err := filepath.Glob(filepath.Join(dir, "show-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range m {
		out = append(out, filepath.Base(p))
	}
	return out
}

// seed adds every capture's seed files of target to f.
func seed(f *testing.F, target string) {
	all, err := svcctltest.Captures()
	if err != nil {
		f.Fatal(err)
	}
	s := fuzzSeedFiles[target]
	for _, c := range all {
		p, err := svcctltest.ReadProvenance(c)
		if err != nil {
			f.Fatal(err)
		}
		if p.Init() != s.init {
			continue
		}
		for _, file := range s.files(f, c) {
			f.Add(captureText(f, c, file))
		}
	}
}

// checkEntries fails on a State or Enabled word outside its enum.
func checkEntries(t *testing.T, entries []svcctl.Entry) {
	for _, e := range entries {
		if !slices.Contains(stateWords, e.State) || !slices.Contains(enabledWords, e.Enabled) {
			t.Fatalf("%q: State %q Enabled %q outside the enums", e.Name, e.State, e.Enabled)
		}
	}
}

// fuzzRun answers argv with data and every other call with no output.
func fuzzRun(argv []string, data string) func(context.Context, []string) (svcctl.Result, error) {
	return func(_ context.Context, a []string) (svcctl.Result, error) {
		if slices.Equal(a, argv) {
			return svcctl.Result{Output: []byte(data)}, nil
		}
		return svcctl.Result{}, nil
	}
}

const fuzzEtc = "/nonexistent-nexus-fuzz-etc"

func FuzzParseListUnits(f *testing.F) {
	seed(f, "FuzzParseListUnits")
	f.Fuzz(func(t *testing.T, data string) {
		entries, err := svcctl.List(context.Background(), platform.Systemd, svcctl.Options{Run: fuzzRun(dfListUnits, data)})
		if err == nil {
			checkEntries(t, entries)
		}
	})
}

func FuzzParseListUnitFiles(f *testing.F) {
	seed(f, "FuzzParseListUnitFiles")
	f.Fuzz(func(t *testing.T, data string) {
		entries, err := svcctl.List(context.Background(), platform.Systemd, svcctl.Options{Run: fuzzRun(dfListUnitFiles, data)})
		if err == nil {
			checkEntries(t, entries)
		}
	})
}

func FuzzParseShow(f *testing.F) {
	seed(f, "FuzzParseShow")
	f.Fuzz(func(t *testing.T, data string) {
		d, err := svcctl.Describe(context.Background(), platform.Systemd, "nexusd", svcctl.Options{Run: fuzzRun(dfShow("nexusd"), data)})
		if err == nil && d.Known {
			checkEntries(t, []svcctl.Entry{d.Entry})
		}
	})
}

func FuzzParseRCStatus(f *testing.F) {
	seed(f, "FuzzParseRCStatus")
	f.Fuzz(func(t *testing.T, data string) {
		entries, err := svcctl.List(context.Background(), platform.OpenRC, svcctl.Options{Run: fuzzRun(dfRCStatus, data), EtcRoot: fuzzEtc})
		if err == nil {
			checkEntries(t, entries)
		}
	})
}

func FuzzParseRCUpdate(f *testing.F) {
	seed(f, "FuzzParseRCUpdate")
	f.Fuzz(func(t *testing.T, data string) {
		entries, err := svcctl.List(context.Background(), platform.OpenRC, svcctl.Options{Run: fuzzRun(dfRCUpdate, data), EtcRoot: fuzzEtc})
		if err == nil {
			checkEntries(t, entries)
		}
		on, _, err := svcctl.EnabledAtBoot(context.Background(), platform.OpenRC, "nexusd", svcctl.Options{Run: fuzzRun(dfRCUpdate, data)})
		if err != nil && on {
			t.Fatal("EnabledAtBoot is true with an error")
		}
	})
}

// TC-SV-60: The listing and show parsers survive fuzzing seeded from every
// capture.
func TestTCSV60_TheListingAndShowParsersSurviveFuzzing(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "services_list_tc_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && strings.HasPrefix(fn.Name.Name, "Fuzz") {
			targets = append(targets, fn.Name.Name)
		}
	}
	want := []string{"FuzzParseListUnitFiles", "FuzzParseListUnits", "FuzzParseRCStatus", "FuzzParseRCUpdate", "FuzzParseShow"}
	if slices.Sort(targets); !slices.Equal(targets, want) {
		t.Errorf("fuzz targets %q, want %q", targets, want)
	}
	for _, target := range want {
		s, ok := fuzzSeedFiles[target]
		if !ok {
			t.Errorf("%s has no seeds", target)
			continue
		}
		for _, c := range capturesOf(t, s.init) {
			if len(s.files(t, c)) == 0 {
				t.Errorf("%s: no seed from %s", target, c)
			}
		}
	}
}

// TC-SV-E8: Unset, maximal or missing show values become nulls with reasons.
func TestTCSVE8_UnsetMaximalOrMissingShowValuesBecome(t *testing.T) {
	const c = "t1-ubuntu-systemd"
	base := captureText(t, c, "show-nexusd.txt")
	replace := func(key, line string) string {
		re := regexp.MustCompile(`(?m)^` + key + `=.*\n`)
		if !re.MatchString(base) {
			t.Fatalf("show-nexusd.txt has no %s line", key)
		}
		return re.ReplaceAllLiteralString(base, line)
	}
	describe := func(out string) svcctl.Details {
		t.Helper()
		fh := svcctltest.NewHost(t, c)
		fh.Answer(dfShow("nexusd"), svcctl.Result{Output: []byte(out)}, nil)
		d, err := svcctl.Describe(t.Context(), platform.Systemd, "nexusd", svcctl.Options{Run: fh.Run})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	want := describe(base)
	if want.MemoryBytes == nil || want.Restarts == nil || want.MainPID == 0 || want.Since == nil {
		t.Fatalf("the base capture lacks a value: %+v", want)
	}
	inactive := strings.NewReplacer("ActiveState=active\n", "ActiveState=inactive\n", "SubState=running\n", "SubState=dead\n")
	for _, tc := range []struct {
		name, out, key string
		check          func(d svcctl.Details) bool
		zero           func(d *svcctl.Details)
	}{
		{"memory not set", replace("MemoryCurrent", "MemoryCurrent=[not set]\n"), "memoryBytes",
			func(d svcctl.Details) bool { return d.MemoryBytes == nil }, func(d *svcctl.Details) { d.MemoryBytes = nil }},
		{"memory maximal", replace("MemoryCurrent", "MemoryCurrent=18446744073709551615\n"), "memoryBytes",
			func(d svcctl.Details) bool { return d.MemoryBytes == nil }, func(d *svcctl.Details) { d.MemoryBytes = nil }},
		{"no NRestarts", replace("NRestarts", ""), "restarts",
			func(d svcctl.Details) bool { return d.Restarts == nil }, func(d *svcctl.Details) { d.Restarts = nil }},
		{"never entered", replace("ActiveEnterTimestamp", "ActiveEnterTimestamp=\n"), "since",
			func(d svcctl.Details) bool { return d.Since == nil }, func(d *svcctl.Details) { d.Since = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := describe(tc.out)
			if !tc.check(d) || d.Unavailable[tc.key] == "" {
				t.Errorf("%s: %+v, want nil with a reason", tc.key, d)
			}
			w := want
			tc.zero(&w)
			if !sameDetails(d, w, tc.key) {
				t.Errorf("other fields changed:\n%+v\nwant\n%+v", d, w)
			}
		})
	}
	t.Run("not running", func(t *testing.T) {
		d := describe(inactive.Replace(replace("MainPID", "MainPID=0\n")))
		if d.MainPID != 0 || d.Unavailable["mainPid"] == "" || d.State != "stopped" {
			t.Errorf("MainPID %d (%q) State %q, want 0 with a reason, stopped", d.MainPID, d.Unavailable["mainPid"], d.State)
		}
		if d.Description != want.Description || d.UnitFilePath != want.UnitFilePath || d.Enabled != want.Enabled {
			t.Errorf("other fields changed: %+v", d)
		}
	})
}

// sameDetails compares a and b, ignoring Unavailable[key].
func sameDetails(a, b svcctl.Details, key string) bool {
	ua, ub := map[string]string{}, map[string]string{}
	for k, v := range a.Unavailable {
		if k != key {
			ua[k] = v
		}
	}
	for k, v := range b.Unavailable {
		if k != key {
			ub[k] = v
		}
	}
	a.Unavailable, b.Unavailable = nil, nil
	ptrEq := func(x, y *int) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	ptr64Eq := func(x, y *int64) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	if !ptrEq(a.Restarts, b.Restarts) || !ptr64Eq(a.MemoryBytes, b.MemoryBytes) || !sameTime(a.Since, b.Since) {
		return false
	}
	a.Restarts, b.Restarts, a.MemoryBytes, b.MemoryBytes, a.Since, b.Since = nil, nil, nil, nil, nil, nil
	return fmt.Sprint(a) == fmt.Sprint(b) && fmt.Sprint(ua) == fmt.Sprint(ub)
}

// TC-SV-E9: Listings with or without the PRESET column, and descriptions
// with runs of spaces, parse identically.
func TestTCSVE9_ListingsWithOrWithoutThePRESETColumn(t *testing.T) {
	const c = "t1-debian-systemd"
	with := captureText(t, c, "list-unit-files.txt")
	preset := regexp.MustCompile(`(?m)^(\S+\s+\S+)\s+\S+[ \t]*$`)
	without := preset.ReplaceAllString(with, "$1")
	if without == with {
		t.Fatal("the capture has no PRESET column to remove")
	}
	pairs := func(lf string) []string {
		fh := svcctltest.NewHost(t, c)
		fh.Answer(dfListUnitFiles, svcctl.Result{Output: []byte(lf)}, nil)
		fh.Answer(dfListUnits, svcctl.Result{}, nil)
		var out []string
		for _, e := range list(t, platform.Systemd, svcctl.Options{Run: fh.Run}) {
			out = append(out, e.Name+" "+e.Enabled+" "+e.EnabledRaw)
		}
		return out
	}
	if a, b := pairs(with), pairs(without); len(a) == 0 || !slices.Equal(a, b) {
		t.Errorf("with PRESET %q\nwithout %q", a, b)
	}
	fh := svcctltest.NewHost(t, c)
	fh.Answer(dfListUnits, svcctl.Result{Output: []byte("nx-spaces.service loaded active running A  B   C\n")}, nil)
	got := byName(t, list(t, platform.Systemd, svcctl.Options{Run: fh.Run}))
	if e := got["nx-spaces"]; e.Description != "A  B   C" || !e.DescriptionKnown {
		t.Errorf("Description %q, want %q", e.Description, "A  B   C")
	}
}

// TC-SV-ERR12: An over-cap listing is an error, never a partial list.
func TestTCSVERR12_AnOverCapListingIsAnError(t *testing.T) {
	const c = "t1-ubuntu-systemd"
	lines := captureText(t, c, "list-units.txt")
	var big bytes.Buffer
	for big.Len() <= svcctl.ListOutputCap {
		big.WriteString(lines)
	}
	over := big.Bytes()[:svcctl.ListOutputCap+1]
	for _, tc := range []struct {
		name string
		res  svcctl.Result
	}{
		{"truncated by the runner", svcctl.Result{Output: over[:svcctl.ListOutputCap], Truncated: true}},
		{"over the cap", svcctl.Result{Output: over}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fh := svcctltest.NewHost(t, c)
			fh.Answer(dfListUnits, tc.res, nil)
			entries, err := svcctl.List(t.Context(), platform.Systemd, svcctl.Options{Run: fh.Run})
			if entries != nil || err == nil || !strings.Contains(err.Error(), "list-units") || !strings.Contains(err.Error(), "4 MiB") {
				t.Errorf("List: %d entries, %v; want nil and an error naming list-units and the 4 MiB cap", len(entries), err)
			}
		})
	}
}

// TC-SV-ERR13: A failing, missing or hung listing tool is an error within
// the timeout.
func TestTCSVERR13_AFailingMissingOrHungListingTool(t *testing.T) {
	const c = "t1-ubuntu-systemd"
	t.Run("exit 1", func(t *testing.T) {
		const msg = "Failed to connect to bus: No such file or directory"
		fh := svcctltest.NewHost(t, c)
		fh.Answer(dfListUnits, svcctl.Result{ExitCode: 1, Output: []byte(msg + "\nsecond line\n")}, nil)
		entries, err := svcctl.List(t.Context(), platform.Systemd, svcctl.Options{Run: fh.Run})
		if entries != nil || err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("List: %v, %v; want an error with %q", entries, err, msg)
		}
		fh.Answer(dfShow("nexusd"), svcctl.Result{ExitCode: 1, Output: []byte(msg + "\n")}, nil)
		if _, err := svcctl.Describe(t.Context(), platform.Systemd, "nexusd", svcctl.Options{Run: fh.Run}); err == nil ||
			!strings.Contains(err.Error(), msg) {
			t.Errorf("Describe: %v, want an error with %q", err, msg)
		}
	})
	t.Run("not started", func(t *testing.T) {
		_, notStarted := svcctl.Run(t.Context(), []string{filepath.Join(t.TempDir(), "systemctl")})
		if !errors.Is(notStarted, svcctl.ErrNotStarted) {
			t.Fatalf("the real runner's error %v does not wrap ErrNotStarted", notStarted)
		}
		fh := svcctltest.NewHost(t, c)
		fh.Answer(dfListUnits, svcctl.Result{}, notStarted)
		_, err := svcctl.List(t.Context(), platform.Systemd, svcctl.Options{Run: fh.Run})
		if !errors.Is(err, svcctl.ErrNotStarted) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("List: %v, want ErrNotStarted and fs.ErrNotExist", err)
		}
	})
	t.Run("hang", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		fh := svcctltest.NewHost(t, c)
		fh.HangOn(dfListUnitFiles)
		start := time.Now()
		_, err := svcctl.List(t.Context(), platform.Systemd, svcctl.Options{Run: fh.Run, Timeout: 200 * time.Millisecond})
		if el := time.Since(start); el > time.Second {
			t.Errorf("List returned after %s, want within 1 s", el)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("List: %v, want context.DeadlineExceeded", err)
		}
		deadline := time.Now().Add(time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := runtime.NumGoroutine(); n > baseline {
			t.Errorf("%d goroutines after List, baseline %d", n, baseline)
		}
	})
}
