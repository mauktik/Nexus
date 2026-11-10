package apt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// The upgrade testdata was captured from real apt and dpkg by
// testdata/capture-upgrade.sh: for each mode, the fixture world's
// dpkg-query and apt-cache policy, the provider's exact preview command
// and the provider's exact mutation command, run one after the other on
// the same state, and dpkg-query after the run.
var upgradeBases = []string{"upgrade/ubuntu-latest", "upgrade/debian-stable"}

// upgradeModes maps each captured mode to the input that runs it: the
// subsets name nxplain and nxconfl (whose 2.0 conflicts with nxvictim).
var upgradeModes = map[string]spec.UpgradeInput{
	"upgrade":       {},
	"dist-upgrade":  {Mode: spec.ModeFull},
	"only-standard": {Names: []string{"nxplain", "nxconfl"}},
	"only-full":     {Mode: spec.ModeFull, Names: []string{"nxplain", "nxconfl"}},
}

func captureCode(t *testing.T, base, name string) int {
	t.Helper()
	code, err := strconv.Atoi(strings.TrimSpace(string(capture(t, base, name+".exit"))))
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestUpgradeTestdataIsCaptured(t *testing.T) {
	for _, base := range upgradeBases {
		prov := string(capture(t, base, "PROVENANCE"))
		for _, want := range []string{"captured: ", "apt: apt ", "dpkg: ", "script: internal/providers/apt/testdata/capture-upgrade.sh"} {
			if !strings.Contains(prov, want) {
				t.Errorf("%s/PROVENANCE lacks %q:\n%s", base, want, prov)
			}
		}
	}
}

// The commands: standard never removes (upgrade, or a subset with
// --no-remove), full is dist-upgrade or a subset without it, names follow
// "--", and the preview is the same command under apt-get -s with the
// cache in memory.
func TestUpgradeArgs(t *testing.T) {
	names := []string{"nxplain", "g++"}
	for _, tc := range []struct {
		mode  string
		names []string
		want  []string
	}{
		{spec.ModeStandard, nil, []string{"upgrade"}},
		{spec.ModeFull, nil, []string{"dist-upgrade"}},
		{spec.ModeStandard, names, []string{"install", "--only-upgrade", "--no-remove", "--", "nxplain", "g++"}},
		{spec.ModeFull, names, []string{"install", "--only-upgrade", "--", "nxplain", "g++"}},
	} {
		if got := upgradeArgs(tc.mode, tc.names); !slices.Equal(got, tc.want) {
			t.Errorf("%s %q: %q, want %q", tc.mode, tc.names, got, tc.want)
		}
	}
	got := previewArgv([]string{"upgrade"})
	want := []string{AptGet, "-s", "-y", "-o", "Dir::Cache::pkgcache=", "-o", "Dir::Cache::srcpkgcache=", "-o",
		"APT::Color=0", "upgrade"}
	if !slices.Equal(got, want) {
		t.Errorf("preview argv %q\nwant %q", got, want)
	}
	// The captures ran exactly these: the command lines in the script.
	script := string(capture(t, "", "capture-upgrade.sh"))
	for _, line := range []string{
		`SIM=(/usr/bin/apt-get -s -y "${NOCACHE[@]}" -o APT::Color=0)`,
		`args=(install --only-upgrade --no-remove -- nxplain nxconfl)`,
		`args=(install --only-upgrade -- nxplain nxconfl)`,
	} {
		if !strings.Contains(script, line) {
			t.Errorf("capture-upgrade.sh no longer runs %s", line)
		}
	}
}

// statusDiff is the test's own diff of two dpkg-query captures, by plain
// field splitting: each package whose version changed, as
// "name old>new", with an empty side for not installed.
func statusDiff(before, after []byte) (upgraded, installed, removed []string) {
	read := func(b []byte) map[string]string {
		m := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			f := strings.Split(line, "\t")
			if len(f) == 4 && f[3] != "not-installed" && f[3] != "config-files" {
				m[f[0]+":"+f[1]] = f[2]
			}
		}
		return m
	}
	b, a := read(before), read(after)
	for k, v := range b {
		switch nv, ok := a[k]; {
		case !ok:
			removed = append(removed, k+" "+v+">")
		case nv != v:
			upgraded = append(upgraded, k+" "+v+">"+nv)
		}
	}
	for k, v := range a {
		if _, ok := b[k]; !ok {
			installed = append(installed, k+" >"+v)
		}
	}
	slices.Sort(upgraded)
	slices.Sort(installed)
	slices.Sort(removed)
	return upgraded, installed, removed
}

func previewStrings(l []spec.PreviewPackage) []string {
	var out []string
	for _, p := range l {
		out = append(out, p.Package+":"+p.Architecture+" "+p.OldVersion+">"+p.NewVersion)
	}
	return out
}

// testUpgradable is the test's own Upgradable: installed packages whose
// captured policy Candidate differs from Installed, by line scanning.
func testUpgradable(t *testing.T, base, mode string, names []string) []string {
	t.Helper()
	var out []string
	cur, inst := "", ""
	for _, line := range strings.Split(string(capture(t, base, mode+".policy")), "\n") {
		switch {
		case !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":"):
			cur = strings.TrimSuffix(line, ":")
		case strings.HasPrefix(line, "  Installed: "):
			inst = strings.TrimPrefix(line, "  Installed: ")
		case strings.HasPrefix(line, "  Candidate: "):
			cand := strings.TrimPrefix(line, "  Candidate: ")
			if inst != "(none)" && cand != "(none)" && cand != inst && (len(names) == 0 || slices.Contains(names, cur)) {
				out = append(out, cur)
			}
		}
	}
	slices.Sort(out)
	return out
}

// A-26 at T0, on real apt: for every mode, the preview built from the
// provider's simulation equals the dpkg-query diff of the real run that
// followed on the same state, whatever the solver chose; standard removes
// nothing; and heldBack is the test's own Upgradable minus the Inst names
// of the raw simulation text. Mode only-standard needs a removal, so both
// the simulation and the run fail and nothing changes.
func TestPreviewEqualsCapturedOutcome(t *testing.T) {
	for _, base := range upgradeBases {
		for mode, in := range upgradeModes {
			simExit, runExit := captureCode(t, base, mode+".sim"), captureCode(t, base, mode+".run")
			up, ins, rem := statusDiff(capture(t, base, mode+".dpkg-before"), capture(t, base, mode+".dpkg-after"))
			st := previewState{Architectures: map[string]string{}}
			for _, d := range parseDpkgQuery(capture(t, base, mode+".dpkg-before")) {
				st.Architectures[d.Package] = d.Architecture
			}
			pol := parsePolicy(capture(t, base, mode+".policy"))
			for _, name := range testUpgradable(t, base, mode, in.Names) {
				st.Upgradable = append(st.Upgradable, spec.HeldBackPackage{Package: name,
					Architecture: st.Architectures[name], InstalledVersion: pol[name].Installed,
					CandidateVersion: pol[name].Candidate})
			}
			ops := t.TempDir()
			p := New(Options{Host: "testhost"})
			p.root = t.TempDir()
			p.UseOpsDir(ops)
			writePreview(t, ops, "pv", st, capture(t, base, mode+".sim.stdout"), capture(t, base, mode+".sim.stderr"))
			in.WhatIf = true
			res, err := p.finalizeUpgrade(context.Background(), in, detach.Exit{ExitCode: simExit}, provider.NewRun("pv", nil))
			if simExit != 0 {
				if runExit == 0 || len(up)+len(ins)+len(rem) != 0 {
					t.Errorf("%s %s: the simulation failed (%d) but the run exited %d with changes %q %q %q", base,
						mode, simExit, runExit, up, ins, rem)
				}
				if slugOf(t, err) != problem.AptFailed {
					t.Errorf("%s %s: a failed simulation is %v, want apt-failed", base, mode, err)
				}
				t.Logf("%s %s: simulation and run both exit %d, nothing changed: %v", base, mode, simExit, err)
				continue
			}
			if err != nil || runExit != 0 {
				t.Fatalf("%s %s: %v, run exit %d", base, mode, err, runExit)
			}
			pv := *res.Preview
			if got := previewStrings(pv.Upgraded); !slices.Equal(got, up) {
				t.Errorf("%s %s: upgraded %q, real %q", base, mode, got, up)
			}
			if got := previewStrings(pv.Installed); !slices.Equal(got, ins) {
				t.Errorf("%s %s: installed %q, real %q", base, mode, got, ins)
			}
			if got := previewStrings(pv.Removed); !slices.Equal(got, rem) {
				t.Errorf("%s %s: removed %q, real %q", base, mode, got, rem)
			}
			if in.EffectiveMode() == spec.ModeStandard && len(pv.Removed)+len(rem) != 0 {
				t.Errorf("%s %s: standard removed %q (real %q)", base, mode, pv.Removed, rem)
			}
			instNames := map[string]bool{}
			for _, r := range rawInsts(capture(t, base, mode+".sim.stdout")) {
				instNames[r.name] = true
			}
			var wantHeld, gotHeld []string
			for _, name := range testUpgradable(t, base, mode, in.Names) {
				if !instNames[name] {
					wantHeld = append(wantHeld, name)
				}
			}
			for _, h := range pv.HeldBack {
				gotHeld = append(gotHeld, h.Package)
			}
			if !slices.Equal(gotHeld, wantHeld) {
				t.Errorf("%s %s: heldBack %q, want %q", base, mode, gotHeld, wantHeld)
			}
			t.Logf("%s %s: upgraded %q installed %q removed %q heldBack %q", base, mode, up, ins, rem, gotHeld)
		}
	}
}

// The S-4 behavior the resolution exists for is still in the captures:
// apt-get install --only-upgrade reads nxplain- as a removal, nxnewdep+ as
// nxnewdep and nxplai. as a regular expression. dpkg-query matches none of
// them, so each is not-found and no apt command runs.
func TestResolveInstalledRejectsHostileNames(t *testing.T) {
	controls := map[string]string{"nxplain-": "Remv nxplain [1.0]", "nxnewdep+": "Inst nxnewdep [1.0]",
		"nxplai.": "Inst nxplain [1.0]"}
	for _, base := range upgradeBases {
		for name, want := range controls {
			if !strings.Contains(string(capture(t, base, "hostile-"+name+".stdout")), want) {
				t.Errorf("%s: apt-get -s install --only-upgrade %s no longer prints %q; the premise changed", base, name, want)
			}
		}
		for _, names := range [][]string{{"nxplain-"}, {"nxnewdep+"}, {"nxplai."}, {"nxplain", "nxplai."}} {
			p, calls := namesFake(t, base)
			_, err := p.resolveInstalled(context.Background(), names)
			if slugOf(t, err) != problem.NotFound {
				t.Errorf("%s %q: %v, want not-found", base, names, err)
			}
			for _, c := range *calls {
				if c[0] != DpkgQuery && c[0] != Dpkg {
					t.Errorf("%s %q ran %q", base, names, c)
				}
			}
		}
		p, _ := namesFake(t, base)
		got, err := p.resolveInstalled(context.Background(), []string{"nxplain", "nxplain:arm64", "nxplain:all"})
		if err != nil || !slices.Equal(got, []string{"nxplain"}) {
			t.Errorf("%s nxplain: %q %v", base, got, err)
		}
		p, _ = namesFake(t, base)
		if _, err := p.resolveInstalled(context.Background(), []string{"nxplain:i386"}); slugOf(t, err) != problem.NotFound {
			t.Errorf("%s nxplain:i386: %v, want not-found", base, err)
		}
		for _, bad := range []string{"-o", "nx*", "nxplain=1.0", "../x", "A"} {
			p, calls := namesFake(t, base)
			if _, err := p.resolveInstalled(context.Background(), []string{bad}); slugOf(t, err) != problem.ValidationFailed ||
				len(*calls) != 0 {
				t.Errorf("%s %q: %v, calls %q", base, bad, err, *calls)
			}
		}
	}
}

// namesFake answers dpkg-query for any names from the captured
// names.stdout (nxplain installed) with the exit status dpkg-query has when
// some name matched nothing.
func namesFake(t *testing.T, base string) (*Provider, *[][]string) {
	calls := &[][]string{}
	p := New(Options{Host: "testhost"})
	p.root = t.TempDir()
	p.run = func(_ context.Context, c command) (result, error) {
		*calls = append(*calls, slices.Clone(c.Argv))
		switch c.Argv[0] {
		case Dpkg:
			return result{Stdout: capture(t, "ubuntu-latest", "print-architecture.txt")}, nil
		case DpkgQuery:
			i := slices.Index(c.Argv, "--")
			var out []string
			code := 0
			for _, n := range c.Argv[i+1:] {
				found := false
				for _, line := range strings.Split(strings.TrimSpace(string(capture(t, base, "names.stdout"))), "\n") {
					if strings.HasPrefix(line, strings.SplitN(n, ":", 2)[0]+"\t") {
						out, found = append(out, line), true
					}
				}
				if !found {
					code = captureCode(t, base, "names")
				}
			}
			return result{Stdout: []byte(strings.Join(out, "\n") + "\n"), ExitCode: code}, nil
		}
		t.Fatalf("unexpected command %q", c.Argv)
		return result{}, nil
	}
	return p, calls
}

func TestAdmitUpgrade(t *testing.T) {
	base := upgradeBases[0]
	p, calls := namesFake(t, base)
	if err := p.admitUpgrade(context.Background(), spec.UpgradeInput{Mode: "bogus"}); slugOf(t, err) != problem.ValidationFailed {
		t.Errorf("mode bogus: %v", err)
	}
	many := make([]string, spec.MaxUpgradeNames+1)
	for i := range many {
		many[i] = "nxplain"
	}
	if err := p.admitUpgrade(context.Background(), spec.UpgradeInput{Names: many}); slugOf(t, err) != problem.ValidationFailed {
		t.Errorf("too many names: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("a refused input ran %q", *calls)
	}
	for _, in := range []spec.UpgradeInput{{}, {Mode: spec.ModeFull}, {Names: []string{"nxplain"}, WhatIf: true}} {
		if err := p.admitUpgrade(context.Background(), in); err != nil {
			t.Errorf("%+v: %v", in, err)
		}
	}
	for _, mode := range spec.Modes {
		for _, whatIf := range []bool{false, true} {
			in := spec.UpgradeInput{Mode: mode, WhatIf: whatIf, Names: []string{"nxplain-"}}
			if err := p.admitUpgrade(context.Background(), in); slugOf(t, err) != problem.NotFound {
				t.Errorf("%+v: %v, want not-found", in, err)
			}
		}
	}
}

// whatIf runs on no lane: it waits behind nothing (A-29). A real run
// queues on the apt lane.
func TestUpgradeLane(t *testing.T) {
	l, err := upgradeLane(context.Background(), spec.UpgradeInput{WhatIf: true})
	if err != nil || l != provider.NoLane {
		t.Errorf("whatIf lane %+v %v", l, err)
	}
	for _, in := range []spec.UpgradeInput{{}, {Mode: spec.ModeFull, Names: []string{"nxplain"}}} {
		l, err := upgradeLane(context.Background(), in)
		if err != nil || l != (provider.Lane{Key: LaneKey, Policy: provider.LaneQueue}) {
			t.Errorf("%+v lane %+v %v", in, l, err)
		}
	}
}

// upgradeFake is a fakeMutation that also answers name resolution (from
// the 7.1 capture of dpkg-query nxplain, or nothing once gone is set) and
// apt-cache and apt-get (from the 7.1 fixture captures).
type upgradeFake struct {
	*fakeMutation
	queries int  // Name resolutions
	gone    bool // Resolutions after the first find nothing
}

func newUpgradeFake(t *testing.T, lockWait time.Duration) *upgradeFake {
	u := &upgradeFake{fakeMutation: newFakeMutation(t, lockWait)}
	mutation := u.p.run
	fix, _ := newFake(t, "ubuntu-latest", "fixture")
	u.p.run = func(ctx context.Context, c command) (result, error) {
		u.mu.Lock()
		switch {
		case c.Argv[0] == DpkgQuery && slices.Contains(c.Argv, "--"):
			u.queries++
			n := u.queries
			u.calls = append(u.calls, slices.Clone(c.Argv))
			u.mu.Unlock()
			if n > 1 && u.gone {
				return result{ExitCode: 1}, nil
			}
			return result{Stdout: capture(t, "ubuntu-latest", "fixture-dpkg-query-nxplain.txt")}, nil
		case c.Argv[0] == Dpkg && slices.Contains(c.Argv, "--print-architecture"):
			u.mu.Unlock()
			return result{Stdout: capture(t, "ubuntu-latest", "print-architecture.txt")}, nil
		case c.Argv[0] == AptCache || c.Argv[0] == AptGet:
			u.calls = append(u.calls, slices.Clone(c.Argv))
			u.mu.Unlock()
			return fix.run(ctx, c)
		}
		u.mu.Unlock()
		return mutation(ctx, c)
	}
	return u
}

// A real run: the names resolve before and again after the lock wait,
// then the unit runs apt-get install --only-upgrade --no-remove with the
// fixed mutation options; a package gone by then is not-found and no unit
// is built.
func TestPrepareUpgradeResolvesAroundTheWait(t *testing.T) {
	f := newUpgradeFake(t, time.Minute)
	f.locks = []lockState{{Held: true, PID: 4242}, {}}
	r := provider.NewRun("op-upgrade", nil)
	s, err := f.p.prepareUpgrade(context.Background(), spec.UpgradeInput{Names: []string{"nxplain"}}, r)
	if err != nil {
		t.Fatal(err)
	}
	if f.queries != 2 || f.probes < 2 {
		t.Errorf("names resolved %d times around %d probes, want before and after the wait", f.queries, f.probes)
	}
	if !slices.Equal(s.Argv[len(s.Argv)-5:], []string{"install", "--only-upgrade", "--no-remove", "--", "nxplain"}) ||
		s.Argv[0] != AptGet || !slices.Contains(s.Argv, "Dpkg::Options::=--force-confold") || s.KillMode != detach.KillProcess ||
		!f.hasPre(r.ID()) {
		t.Errorf("spec %+v, pre.json %v", s, f.hasPre(r.ID()))
	}

	f = newUpgradeFake(t, time.Minute)
	f.gone = true
	_, err = f.p.prepareUpgrade(context.Background(), spec.UpgradeInput{Names: []string{"nxplain"}}, provider.NewRun("op-gone", nil))
	if slugOf(t, err) != problem.NotFound {
		t.Errorf("package gone after the wait: %v, want not-found", err)
	}

	// Every package in full mode: dist-upgrade, no name resolution.
	f = newUpgradeFake(t, time.Minute)
	s, err = f.p.prepareUpgrade(context.Background(), spec.UpgradeInput{Mode: spec.ModeFull}, provider.NewRun("op-full", nil))
	if err != nil || s.Argv[len(s.Argv)-1] != "dist-upgrade" || f.queries != 0 {
		t.Errorf("full: %q %v, %d resolutions", s.Argv, err, f.queries)
	}
}

// Coordinator handoff from 7.2: a cancel during the lock wait ends Prepare
// with the context's error, before any audit, snapshot or spec; the
// manager then leaves the record Canceled (asserted at T1,
// TestAptUpgradeWaitsForTheLock/cancel). The op is CancelQueuedOnly.
func TestPrepareUpgradeCancelDuringTheWait(t *testing.T) {
	if c := spec.Upgrade.Op().Cancel; c != def.CancelQueuedOnly {
		t.Errorf("Cancel %q, want queued-only", c)
	}
	f := newUpgradeFake(t, time.Hour)
	f.locks = []lockState{{Held: true, PID: 4242}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	r := provider.NewRun("op-cancel", nil)
	_, err := f.p.prepareUpgrade(ctx, spec.UpgradeInput{}, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v, want context.Canceled", err)
	}
	var perr *problem.Error
	if errors.As(err, &perr) || f.hasPre(r.ID()) {
		t.Errorf("a cancel is not a problem (%v), and writes no pre.json (%v)", perr, f.hasPre(r.ID()))
	}
	for _, c := range f.commands() {
		if c[0] == Dpkg && slices.Contains(c, "--audit") {
			t.Errorf("audit ran after the cancel: %q", c)
		}
	}
}

// whatIf: no lock probe, no audit, no snapshot; preview.json records the
// upgradable packages (of the named ones), and the unit is the simulation.
func TestPreparePreview(t *testing.T) {
	f := newUpgradeFake(t, time.Minute)
	f.locks = []lockState{{Held: true, PID: 4242}} // Never probed
	r := provider.NewRun("op-preview", nil)
	s, err := f.p.prepareUpgrade(context.Background(), spec.UpgradeInput{WhatIf: true, Names: []string{"nxplain"}}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, previewArgv(upgradeArgs(spec.ModeStandard, []string{"nxplain"}))) || s.KillMode != detach.KillProcess ||
		s.MaxRuntime <= 0 {
		t.Errorf("spec %+v", s)
	}
	if f.probes != 0 || f.hasPre(r.ID()) {
		t.Errorf("probes %d, pre.json %v", f.probes, f.hasPre(r.ID()))
	}
	for _, c := range f.commands() {
		if c[0] == Dpkg && slices.Contains(c, "--audit") {
			t.Errorf("whatIf audited: %q", c)
		}
	}
	b, err := os.ReadFile(filepath.Join(f.ops, r.ID(), PreviewFile))
	if err != nil {
		t.Fatal(err)
	}
	var st previewState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	want := []spec.HeldBackPackage{{Package: "nxplain", Architecture: "all", InstalledVersion: "1.0", CandidateVersion: "2.0"}}
	if !slices.Equal(st.Upgradable, want) || st.Architectures["nxplain"] != "all" {
		t.Errorf("preview.json %+v", st)
	}
}

// writePreview lays out a finished whatIf operation: preview.json and the
// simulation's output in the operation's log.
func writePreview(t *testing.T, ops, id string, st previewState, stdout, stderr []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(ops, id), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ops, id, PreviewFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeOpOutput(t, ops, id, stdout, stderr)
}

// Finalize of a whatIf reads preview.json and the simulation's stdout from
// the operation's log, files only; stderr never counts as an action. A
// failed simulation is apt-failed; a log that lost output to its cap is an
// error, never a short preview.
func TestFinalizePreview(t *testing.T) {
	base := upgradeBases[1]
	ops := t.TempDir()
	p := New(Options{Host: "testhost"})
	p.root = t.TempDir()
	p.UseOpsDir(ops)
	st := previewState{Architectures: map[string]string{"nxvictim": "all", "nxconfl": "all", "nxplain": "all"},
		Upgradable: []spec.HeldBackPackage{{Package: "nxheld", Architecture: "all", InstalledVersion: "1.0", CandidateVersion: "2.0"},
			{Package: "nxplain", Architecture: "all", InstalledVersion: "1.0", CandidateVersion: "2.0"}}}
	writePreview(t, ops, "pv", st, capture(t, base, "only-full.sim.stdout"), []byte("Inst nxfake [1.0] (2.0 x [all])\n"))
	in := spec.UpgradeInput{Mode: spec.ModeFull, WhatIf: true}
	res, err := p.finalizeUpgrade(context.Background(), in, detach.Exit{}, provider.NewRun("pv", nil))
	if err != nil {
		t.Fatal(err)
	}
	pv := res.Preview
	if pv == nil || !res.WhatIf || res.Changed || res.Mode != spec.ModeFull || len(res.Changes) != 0 ||
		!slices.Equal(previewStrings(pv.Upgraded), []string{"nxconfl:all 1.0>2.0", "nxplain:all 1.0>2.0"}) ||
		!slices.Equal(previewStrings(pv.Removed), []string{"nxvictim:all 1.0>"}) || len(pv.Installed) != 0 ||
		len(pv.HeldBack) != 1 || pv.HeldBack[0].Package != "nxheld" {
		t.Errorf("result %+v preview %+v", res, pv)
	}
	restarted := New(Options{})
	restarted.UseOpsDir(ops)
	again, err := restarted.finalizeUpgrade(context.Background(), in, detach.Exit{}, provider.NewRun("pv", nil))
	if err != nil || fmt.Sprint(again.Preview) != fmt.Sprint(res.Preview) {
		t.Errorf("after restart: %+v %v", again.Preview, err)
	}

	writePreview(t, ops, "fail", st, capture(t, base, "only-standard.sim.stdout"), capture(t, base, "only-standard.sim.stderr"))
	_, err = p.finalizeUpgrade(context.Background(), in, detach.Exit{ExitCode: captureCode(t, base, "only-standard.sim")},
		provider.NewRun("fail", nil))
	if slugOf(t, err) != problem.AptFailed || !strings.Contains(err.Error(), "status 100") {
		t.Errorf("failed simulation: %v", err)
	}

	// A log that lost its start to the cap.
	if err := os.MkdirAll(filepath.Join(ops, "cut"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(ops, "cut", PreviewFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := oplog.Open(filepath.Join(ops, "cut", detach.OutputDir), oplog.Options{CapBytes: 4096, SegmentBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	for range 40 {
		if err := w.Write(apitypes.StreamStdout, []byte(strings.Repeat("Conf x (1 y [all])\n", 10))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.finalizeUpgrade(context.Background(), in, detach.Exit{}, provider.NewRun("cut", nil)); err == nil ||
		!strings.Contains(err.Error(), "truncated") {
		t.Errorf("truncated log: %v", err)
	}
}

// Finalize of a real run is FinalizeMutation's outcome on the wire:
// changes, kept conffiles, reboot state and exit status; a failed run is
// the problem with the exit status in its detail.
func TestFinalizeUpgradeRealRun(t *testing.T) {
	f, r := finalizeFixture(t)
	res, err := f.p.finalizeUpgrade(context.Background(), spec.UpgradeInput{}, detach.Exit{}, r)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.p.FinalizeMutation(r, detach.Exit{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.WhatIf || res.Preview != nil || res.Mode != spec.ModeStandard || len(res.Changes) != len(out.Changes) ||
		!slices.Equal(res.ConffilesKept, out.Conffiles) || res.ConffilesComplete != out.ConffilesComplete ||
		res.RebootRequired != out.RebootRequired || res.ExitCode != 0 {
		t.Errorf("result %+v\noutcome %+v", res, out)
	}
	for i, c := range out.Changes {
		if res.Changes[i] != spec.PackageChange(c) {
			t.Errorf("change %d: %+v, want %+v", i, res.Changes[i], c)
		}
	}
	_, err = f.p.finalizeUpgrade(context.Background(), spec.UpgradeInput{}, detach.Exit{ExitCode: 1}, r)
	if slugOf(t, err) != problem.AptFailed || !strings.Contains(err.Error(), "status 1") {
		t.Errorf("exit 1: %v", err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	// ASM-13: the result reports, and classifies nothing as disruptive.
	got := slices.Sorted(func(yield func(string) bool) {
		for k := range keys {
			if !yield(k) {
				return
			}
		}
	})
	want := []string{"changed", "changes", "conffilesComplete", "conffilesKept", "exitCode", "mode", "rebootRequired",
		"rebootRequiredPackages", "whatIf"}
	if !slices.Equal(got, want) {
		t.Errorf("result keys %q, want %q", got, want)
	}
}

// The declaration: packages:write, destructive (A-12), long-running with
// whatIf and queued-only cancel, its side effects stated, a closed mode
// enum, and names constrained by the name pattern with a bound.
func TestUpgradeDeclaration(t *testing.T) {
	op := spec.Upgrade.Op()
	if op.Scope.Name != spec.WriteScope || op.Risk != def.RiskDestructive || op.Mode != def.ModeLongRunning || !op.WhatIf ||
		op.Cancel != def.CancelQueuedOnly || op.Kind != def.KindProviderAction || op.Action != "upgrade" || op.MCP == nil {
		t.Errorf("op %+v", op)
	}
	for _, want := range []string{"Side effects:", "Long-running", "never reboots", "package-manager-busy",
		"package-manager-broken", "apt-failed", "not-found"} {
		if !strings.Contains(op.Description, want) {
			t.Errorf("description lacks %q", want)
		}
	}
	s, err := jsonschema.For[spec.UpgradeInput](nil)
	if err != nil {
		t.Fatal(err)
	}
	op.Refine(s)
	if m := s.Properties["mode"]; len(m.Enum) != 2 || m.Enum[0] != spec.ModeStandard || m.Enum[1] != spec.ModeFull {
		t.Errorf("mode %+v", m)
	}
	n := s.Properties["names"]
	if n.MaxItems == nil || *n.MaxItems != spec.MaxUpgradeNames || n.Items.Pattern != spec.NamePattern ||
		n.Items.MaxLength == nil || *n.Items.MaxLength != spec.MaxNameLength {
		t.Errorf("names %+v", n)
	}
	if !slices.ContainsFunc(spec.Spec().Ops, func(o def.Op) bool { return o.ID == op.ID }) {
		t.Error("upgrade is not in the provider's spec")
	}
	p := New(Options{})
	if !slices.ContainsFunc(p.Bindings(), func(b provider.Binding) bool {
		return b.Op().ID == op.ID && b.Kind() == provider.BindDetached
	}) {
		t.Error("upgrade is not bound as a detached operation")
	}
}

// outcomeMembers are the extension members a failed apt run carries.
func outcomeMembers(t *testing.T, err error) map[string]any {
	t.Helper()
	var perr *problem.Error
	if !errors.As(err, &perr) || perr.Slug() != problem.AptFailed {
		t.Fatalf("error %v is not apt-failed", err)
	}
	b, jerr := json.Marshal(perr)
	if jerr != nil {
		t.Fatal(jerr)
	}
	var body map[string]any
	if jerr := json.Unmarshal(b, &body); jerr != nil {
		t.Fatal(jerr)
	}
	return body
}

// CR-24: apt-get exited 100 after dpkg had changed packages: the problem
// carries the changes, kept conffiles, reboot state and exit status, which
// the operation would otherwise lose along with its result.
func TestFailedUpgradeCarriesTheOutcome(t *testing.T) {
	for name, finalize := range map[string]func(*fakeMutation, *provider.Run) error{
		"upgrade": func(f *fakeMutation, r *provider.Run) error {
			_, err := f.p.finalizeUpgrade(context.Background(), spec.UpgradeInput{}, detach.Exit{ExitCode: 100}, r)
			return err
		},
		"install": func(f *fakeMutation, r *provider.Run) error {
			_, err := f.p.finalizeInstall(context.Background(), spec.InstallInput{Names: []string{"nxplain"}},
				detach.Exit{ExitCode: 100}, r)
			return err
		},
		"remove": func(f *fakeMutation, r *provider.Run) error {
			_, err := f.p.finalizeRemove(context.Background(), spec.RemoveInput{Names: []string{"nxplain"}},
				detach.Exit{ExitCode: 100}, r)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, r := finalizeFixture(t)
			body := outcomeMembers(t, finalize(f, r))
			changes, _ := body["changes"].([]any)
			if len(changes) != 3 {
				t.Errorf("changes %v, want the 3 packages dpkg changed", body["changes"])
			}
			if kept, _ := body["conffilesKept"].([]any); len(kept) != 1 || kept[0] != "/etc/nxconf.conf" {
				t.Errorf("conffilesKept %v", body["conffilesKept"])
			}
			if body["conffilesComplete"] != true || body["rebootRequired"] != spec.RebootUnknown ||
				body["exitCode"] != float64(100) {
				t.Errorf("members %v", body)
			}
			if pkgs, ok := body["rebootRequiredPackages"].([]any); !ok || len(pkgs) != 0 {
				t.Errorf("rebootRequiredPackages %v, want []", body["rebootRequiredPackages"])
			}
		})
	}
}
