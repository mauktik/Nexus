package apt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// The install testdata was captured from real apt and dpkg by
// testdata/capture-install.sh: apt-cache show of exact, hostile, qualified
// and unknown names, dpkg-query of the names remove resolves, the S-4
// controls, and for each step the provider's exact preview command and
// mutation command run one after the other on the same state, with
// dpkg-query around the run.
var installBases = []string{"install/ubuntu-latest", "install/debian-stable"}

func TestInstallTestdataIsCaptured(t *testing.T) {
	for _, base := range installBases {
		prov := string(capture(t, base, "PROVENANCE"))
		for _, want := range []string{"captured: ", "apt: apt ", "dpkg: ", "script: internal/providers/apt/testdata/capture-install.sh"} {
			if !strings.Contains(prov, want) {
				t.Errorf("%s/PROVENANCE lacks %q:\n%s", base, want, prov)
			}
		}
	}
}

// TC-ERR52: layer 1, through the resolved input schemas of install and
// remove, and the provider's own checks of the same rules: the accepted and
// rejected sets are exact, and a rejection is validation-failed.
func TestInstallNameAndVersionValidation(t *testing.T) {
	accept := []string{"g++", "libstdc++6", "python3.12", "nginx:amd64"}
	reject := []string{"-o", "--allow-unauthenticated", "../x", "/tmp/x.deb", "x.deb", "a/b", "UPPER", "",
		strings.Repeat("a", 300), "a b"}
	acceptVersion := []string{"2:1.0~rc1+dfsg-1ubuntu1"}
	rejectVersion := []string{"-1", "1;id"}

	for _, op := range []def.Op{spec.Install.Op(), spec.Remove.Op()} {
		r, err := op.InputSchema()
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range accept {
			if err := r.Validate(map[string]any{"names": []any{n}}); err != nil {
				t.Errorf("%s: schema refused %q: %v", op.ID, n, err)
			}
		}
		for _, n := range reject {
			if err := r.Validate(map[string]any{"names": []any{n}}); err == nil {
				t.Errorf("%s: schema accepted %q", op.ID, n)
			}
		}
	}
	r, err := spec.Install.Op().InputSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range acceptVersion {
		if err := r.Validate(map[string]any{"names": []any{"nxplain"}, "version": v}); err != nil {
			t.Errorf("schema refused version %q: %v", v, err)
		}
	}
	for _, v := range rejectVersion {
		if err := r.Validate(map[string]any{"names": []any{"nxplain"}, "version": v}); err == nil {
			t.Errorf("schema accepted version %q", v)
		}
	}

	// The provider's own layer 1 agrees, and runs nothing.
	for _, n := range accept {
		if err := checkInstall(spec.InstallInput{Names: []string{n}}); err != nil {
			t.Errorf("checkInstall refused %q: %v", n, err)
		}
		if err := checkNames([]string{n}); err != nil {
			t.Errorf("checkNames refused %q: %v", n, err)
		}
	}
	for _, n := range reject {
		if err := checkInstall(spec.InstallInput{Names: []string{n}}); slugOf(t, err) != problem.ValidationFailed {
			t.Errorf("checkInstall %q: %v, want validation-failed", n, err)
		}
		if err := checkNames([]string{n}); slugOf(t, err) != problem.ValidationFailed {
			t.Errorf("checkNames %q: %v, want validation-failed", n, err)
		}
	}
	for _, v := range acceptVersion {
		if err := checkInstall(spec.InstallInput{Names: []string{"nxplain"}, Version: v}); err != nil {
			t.Errorf("version %q: %v", v, err)
		}
	}
	for _, v := range slices.Concat(rejectVersion, []string{strings.Repeat("1", spec.MaxVersionLength+1), "1/x", "1=2", "1*"}) {
		if err := checkInstall(spec.InstallInput{Names: []string{"nxplain"}, Version: v}); slugOf(t, err) != problem.ValidationFailed {
			t.Errorf("version %q: %v, want validation-failed", v, err)
		}
	}
	// A pin names exactly one package; a request names at least one.
	for _, in := range []spec.InstallInput{{Names: []string{"nxplain", "nxg++"}, Version: "1.0"}, {}, {Names: []string{}}} {
		if err := checkInstall(in); slugOf(t, err) != problem.ValidationFailed {
			t.Errorf("%+v: %v, want validation-failed", in, err)
		}
	}
	many := make([]string, spec.MaxInstallNames+1)
	for i := range many {
		many[i] = "nxplain"
	}
	if err := checkNames(many); slugOf(t, err) != problem.ValidationFailed {
		t.Errorf("too many names: %v", err)
	}
}

// showFake answers apt-cache show from the captures: the records of each
// name in argument order, a name with no package skipped, and exit 100
// with "E: No packages found" when none had one, as the captured batch
// shows. dpkg-query answers from the captured names or names-rc.
func showFake(t *testing.T, base, dpkgCapture string) (*Provider, *[][]string) {
	t.Helper()
	calls := &[][]string{}
	p := New(Options{Host: "testhost"})
	p.root = t.TempDir()
	p.run = func(_ context.Context, c command) (result, error) {
		*calls = append(*calls, slices.Clone(c.Argv))
		i := slices.Index(c.Argv, "--")
		switch {
		case c.Argv[0] == Dpkg:
			return result{Stdout: capture(t, base, "print-architecture.txt")}, nil
		case c.Argv[0] == AptCache && i > 0 && c.Argv[i-1] == "show":
			var out []byte
			for _, n := range c.Argv[i+1:] {
				if captureCode(t, base, "show-"+n) == 0 {
					out = append(out, capture(t, base, "show-"+n+".stdout")...)
				}
			}
			if out == nil {
				return result{Stderr: capture(t, base, "show-nxnone.stderr"), ExitCode: captureCode(t, base, "show-nxnone")}, nil
			}
			return result{Stdout: out}, nil
		case c.Argv[0] == DpkgQuery && i > 0:
			var out []string
			code := 0
			for _, n := range c.Argv[i+1:] {
				found := false
				for _, line := range strings.Split(strings.TrimSpace(string(capture(t, base, dpkgCapture+".stdout"))), "\n") {
					if strings.HasPrefix(line, strings.SplitN(n, ":", 2)[0]+"\t") {
						out, found = append(out, line), true
					}
				}
				if !found {
					code = 1
				}
			}
			return result{Stdout: []byte(strings.Join(out, "\n") + "\n"), ExitCode: code}, nil
		}
		t.Fatalf("unexpected command %q", c.Argv)
		return result{}, nil
	}
	return p, calls
}

// The fake's batch is the captured batch: the records of each name in
// argument order, the unknown one skipped, exit status 0.
func TestShowFakeMatchesTheCapturedBatch(t *testing.T) {
	for _, base := range installBases {
		p, _ := showFake(t, base, "names")
		res, err := p.run(context.Background(), cmdShowAll([]string{"nxplain", "nxnone", "nxg++", "nxplai."}))
		if err != nil || res.ExitCode != captureCode(t, base, "show-batch") ||
			string(res.Stdout) != string(capture(t, base, "show-batch.stdout")) {
			t.Errorf("%s: the fake's batch (exit %d) differs from the captured one:\n%s", base, res.ExitCode, res.Stdout)
		}
	}
}

// TC-ERR53 at T0, on real apt output: apt-get still reads nxplain- as a
// removal, nxnewdep+ given to remove as an install and nxplai. as a
// regular expression (the controls), and apt-cache show prints other
// packages' records for nxplai., nxnewdep+ and nxg+; no returned record
// has Package: equal to the name, so each is not-found, and only
// apt-cache show and dpkg ran.
func TestResolveAvailableRejectsHostileNames(t *testing.T) {
	controls := map[string]string{"install-nxplain-": "Remv nxplain [1.0]", "remove-nxnewdep+": "Inst nxnewdep (1.0",
		"install-nxplai.": "Inst nxplain [1.0] (2.0"}
	for _, base := range installBases {
		for name, want := range controls {
			if !strings.Contains(string(capture(t, base, "hostile-"+name+".stdout")), want) {
				t.Errorf("%s: apt-get -s %s no longer prints %q; the premise changed", base, name, want)
			}
		}
		for _, regex := range []string{"nxplai.", "nxnewdep+", "nxg+"} {
			if captureCode(t, base, "show-"+regex) != 0 || len(parseStanzas(capture(t, base, "show-"+regex+".stdout"))) == 0 {
				t.Errorf("%s: apt-cache show %s no longer regex-matches; the premise changed", base, regex)
			}
		}
		for _, names := range [][]string{{"nxplain-"}, {"nxnewdep+"}, {"nxplai."}, {"nxg+"}, {"nxnone"},
			{"nxplain", "nxplai."}, {"nxplain:i386"}} {
			p, calls := showFake(t, base, "names")
			_, err := p.resolveAvailable(context.Background(), names)
			if slugOf(t, err) != problem.NotFound || !strings.Contains(err.Error(), "nothing was started") {
				t.Errorf("%s %q: %v, want not-found", base, names, err)
			}
			for _, c := range *calls {
				if c[0] != AptCache && c[0] != Dpkg {
					t.Errorf("%s %q ran %q", base, names, c)
				}
			}
		}
		for _, names := range [][]string{{"nxnewdep+"}, {"nxplain-"}, {"nxplai."}, {"nxnewdep"}} {
			p, calls := showFake(t, base, "names")
			if _, err := p.resolveRemovable(context.Background(), names, false); slugOf(t, err) != problem.NotFound {
				t.Errorf("%s remove %q: %v, want not-found", base, names, err)
			}
			for _, c := range *calls {
				if c[0] != DpkgQuery && c[0] != Dpkg {
					t.Errorf("%s remove %q ran %q", base, names, c)
				}
			}
		}
		for _, bad := range []string{"-o", "nx*", "nxplain=1.0", "../x", "A", "x.deb", "x.deb:amd64"} {
			p, calls := showFake(t, base, "names")
			if _, err := p.resolveAvailable(context.Background(), []string{bad}); slugOf(t, err) != problem.ValidationFailed ||
				len(*calls) != 0 {
				t.Errorf("%s %q: %v, calls %q", base, bad, err, *calls)
			}
		}
	}
}

// Exact names resolve, "+" kept; a qualifier of the native architecture
// or all names the bare package; the result has no duplicates.
func TestResolveAvailableExactNames(t *testing.T) {
	for _, base := range installBases {
		native := strings.TrimSpace(string(capture(t, base, "print-architecture.txt")))
		p, calls := showFake(t, base, "names")
		got, err := p.resolveAvailable(context.Background(), []string{"nxg++", "nxplain", "nxplain:" + native, "nxplain:all", "nxnewdep"})
		if err != nil || !slices.Equal(got, []string{"nxg++", "nxplain", "nxnewdep"}) {
			t.Errorf("%s: %q %v", base, got, err)
		}
		shows := 0
		for _, c := range *calls {
			if c[0] == AptCache {
				shows++
				if !slices.Equal(c, cmdShowAll([]string{"nxg++", "nxplain", "nxplain:" + native, "nxplain:all", "nxnewdep"}).Argv) {
					t.Errorf("%s: %q", base, c)
				}
			}
		}
		if shows != 1 {
			t.Errorf("%s: %d apt-cache calls, want one for every name", base, shows)
		}

		// Remove resolves against dpkg: installed only, or config-files
		// with purge.
		p, _ = showFake(t, base, "names")
		if got, err := p.resolveRemovable(context.Background(), []string{"nxplain"}, false); err != nil ||
			!slices.Equal(got, []string{"nxplain"}) {
			t.Errorf("%s remove nxplain: %q %v", base, got, err)
		}
		p, _ = showFake(t, base, "names-rc")
		if _, err := p.resolveRemovable(context.Background(), []string{"nxapp"}, false); slugOf(t, err) != problem.NotFound {
			t.Errorf("%s remove of a config-files package: %v, want not-found", base, err)
		}
		p, _ = showFake(t, base, "names-rc")
		if got, err := p.resolveRemovable(context.Background(), []string{"nxapp"}, true); err != nil ||
			!slices.Equal(got, []string{"nxapp"}) {
			t.Errorf("%s purge of a config-files package: %q %v", base, got, err)
		}
	}
}

// Admit refuses before anything exists, with whatIf too: layer 1 runs no
// command, layer 2 is not-found.
func TestAdmitInstallAndRemove(t *testing.T) {
	base := installBases[0]
	p, calls := showFake(t, base, "names")
	for _, in := range []spec.InstallInput{{}, {Names: []string{"a", "b"}, Version: "1"}, {Names: []string{"x.deb"}},
		{Names: []string{"nxplain"}, Version: "-1"}} {
		if err := p.admitInstall(context.Background(), in); slugOf(t, err) != problem.ValidationFailed {
			t.Errorf("%+v: %v", in, err)
		}
	}
	for _, in := range []spec.RemoveInput{{}, {Names: []string{"-o"}, Purge: true}} {
		if err := p.admitRemove(context.Background(), in); slugOf(t, err) != problem.ValidationFailed {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("a refused input ran %q", *calls)
	}
	for _, whatIf := range []bool{false, true} {
		for _, n := range []string{"nxplain-", "nxplai.", "nxnewdep+"} {
			if err := p.admitInstall(context.Background(), spec.InstallInput{Names: []string{n}, WhatIf: whatIf}); slugOf(t, err) != problem.NotFound {
				t.Errorf("install %s whatIf %v: %v", n, whatIf, err)
			}
			if err := p.admitRemove(context.Background(), spec.RemoveInput{Names: []string{n}, WhatIf: whatIf}); slugOf(t, err) != problem.NotFound {
				t.Errorf("remove %s whatIf %v: %v", n, whatIf, err)
			}
		}
	}
	if err := p.admitInstall(context.Background(), spec.InstallInput{Names: []string{"nxg++"}, WhatIf: true}); err != nil {
		t.Error(err)
	}
	if err := p.admitInstall(context.Background(), spec.InstallInput{Names: []string{"nxplain"}, Version: "1.0"}); err != nil {
		t.Error(err)
	}
	if err := p.admitRemove(context.Background(), spec.RemoveInput{Names: []string{"nxplain"}, Autoremove: true}); err != nil {
		t.Error(err)
	}
}

// The commands: install never removes (--no-remove) and pins one name as
// name=version; remove is remove or purge, with --autoremove only when
// asked; names follow "--". The captures ran exactly these.
func TestInstallAndRemoveCommands(t *testing.T) {
	for _, tc := range []struct{ got, want []string }{
		{installCommand([]string{"nxg++"}, ""), []string{"install", "--no-remove", "--", "nxg++"}},
		{installCommand([]string{"nxplain", "nxg++"}, ""), []string{"install", "--no-remove", "--", "nxplain", "nxg++"}},
		{installCommand([]string{"nxplain"}, "1.0"), []string{"install", "--no-remove", "--", "nxplain=1.0"}},
		{removeCommand([]string{"nxapp"}, false, false), []string{"remove", "--", "nxapp"}},
		{removeCommand([]string{"nxapp"}, true, false), []string{"purge", "--", "nxapp"}},
		{removeCommand([]string{"nxapp"}, false, true), []string{"remove", "--autoremove", "--", "nxapp"}},
		{removeCommand([]string{"nxapp"}, true, true), []string{"purge", "--autoremove", "--", "nxapp"}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%q, want %q", tc.got, tc.want)
		}
	}
	script := string(capture(t, "", "capture-install.sh"))
	for _, line := range []string{
		`SIM=(/usr/bin/apt-get -s -y "${NOCACHE[@]}" -o APT::Color=0)`,
		`step install-plus install --no-remove -- nxg++`,
		`step install-pin install --no-remove -- nxplain=1.0`,
		`step "remove-$mode" remove -- nxapp`,
		`step "remove-$mode" purge -- nxapp`,
		`step "remove-$mode" remove --autoremove -- nxapp`,
		`step "remove-$mode" purge --autoremove -- nxapp`,
		`show -- "$n"`,
	} {
		if !strings.Contains(script, line) {
			t.Errorf("capture-install.sh no longer runs %s", line)
		}
	}
}

// TC-135: every mutation's apt-get, as Prepare builds it, never prompts
// and keeps changed conffiles: -y, force-confdef, force-confold, a lock
// timeout and no color.
func TestEveryMutationUsesTheRequiredOptions(t *testing.T) {
	builds := map[string]func(f *installFake, r *provider.Run) (detach.Spec, error){
		"upgrade": func(f *installFake, r *provider.Run) (detach.Spec, error) {
			return f.p.prepareUpgrade(context.Background(), spec.UpgradeInput{}, r)
		},
		"upgrade full": func(f *installFake, r *provider.Run) (detach.Spec, error) {
			return f.p.prepareUpgrade(context.Background(), spec.UpgradeInput{Mode: spec.ModeFull}, r)
		},
		"install": func(f *installFake, r *provider.Run) (detach.Spec, error) {
			return f.p.prepareInstall(context.Background(), spec.InstallInput{Names: []string{"nxg++"}}, r)
		},
		"install pinned": func(f *installFake, r *provider.Run) (detach.Spec, error) {
			return f.p.prepareInstall(context.Background(), spec.InstallInput{Names: []string{"nxplain"}, Version: "1.0"}, r)
		},
	}
	for _, purge := range []bool{false, true} {
		for _, auto := range []bool{false, true} {
			in := spec.RemoveInput{Names: []string{"nxplain"}, Purge: purge, Autoremove: auto}
			builds["remove "+strings.Join(removeCommand(nil, purge, auto), " ")] = func(f *installFake, r *provider.Run) (detach.Spec, error) {
				return f.p.prepareRemove(context.Background(), in, r)
			}
		}
	}
	for name, build := range builds {
		f := newInstallFake(t, time.Minute)
		s, err := build(f, provider.NewRun("op-"+strings.ReplaceAll(name, " ", "-"), nil))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.Argv[0] != AptGet || s.Argv[1] != "-y" {
			t.Errorf("%s: %q", name, s.Argv)
		}
		for _, opt := range []string{"Dpkg::Options::=--force-confdef", "Dpkg::Options::=--force-confold", "APT::Color=0"} {
			if i := slices.Index(s.Argv, opt); i < 1 || s.Argv[i-1] != "-o" {
				t.Errorf("%s lacks -o %s: %q", name, opt, s.Argv)
			}
		}
		if !slices.ContainsFunc(s.Argv, func(a string) bool { return strings.HasPrefix(a, "DPkg::Lock::Timeout=") }) {
			t.Errorf("%s lacks the lock timeout: %q", name, s.Argv)
		}
		named := !strings.HasPrefix(name, "upgrade") // A whole upgrade names no package
		if i := slices.Index(s.Argv, "--"); (named && i < 0) || (i >= 0 && slices.ContainsFunc(s.Argv[i+1:],
			func(a string) bool { return strings.HasPrefix(a, "-") })) {
			t.Errorf("%s: names must follow -- and none starts with -: %q", name, s.Argv)
		}
	}
}

// installFake is a fakeMutation that also answers apt-cache show and the
// names dpkg-query from the install captures, and the snapshot from the
// lock captures; once gone is set, resolutions after the first find
// nothing.
type installFake struct {
	*fakeMutation
	resolutions int
	gone        bool
}

func newInstallFake(t *testing.T, lockWait time.Duration) *installFake {
	f := &installFake{fakeMutation: newFakeMutation(t, lockWait)}
	mutation := f.p.run
	names, _ := showFake(t, installBases[0], "names")
	f.p.run = func(ctx context.Context, c command) (result, error) {
		resolve := slices.Contains(c.Argv, "--") && (c.Argv[0] == DpkgQuery || c.Argv[0] == AptCache)
		switch {
		case resolve:
			f.mu.Lock()
			f.resolutions++
			n := f.resolutions
			f.calls = append(f.calls, slices.Clone(c.Argv))
			f.mu.Unlock()
			if n > 1 && f.gone {
				if c.Argv[0] == AptCache {
					return result{Stderr: noPackagesFound, ExitCode: aptExitFailure}, nil
				}
				return result{ExitCode: 1}, nil
			}
			return names.run(ctx, c)
		case c.Argv[0] == Dpkg && slices.Contains(c.Argv, "--print-architecture"):
			return names.run(ctx, c)
		}
		return mutation(ctx, c)
	}
	return f
}

// A real run resolves the names before and again after the lock wait, then
// builds apt-get with the fixed options; a package gone by then is
// not-found and nothing starts. A cancel during the wait ends Prepare with
// the context's error before any audit or pre.json.
func TestPreparePackagesResolveAroundTheWait(t *testing.T) {
	for name, prep := range map[string]func(f *installFake, ctx context.Context, r *provider.Run) (detach.Spec, error){
		"install": func(f *installFake, ctx context.Context, r *provider.Run) (detach.Spec, error) {
			return f.p.prepareInstall(ctx, spec.InstallInput{Names: []string{"nxplain"}}, r)
		},
		"remove": func(f *installFake, ctx context.Context, r *provider.Run) (detach.Spec, error) {
			return f.p.prepareRemove(ctx, spec.RemoveInput{Names: []string{"nxplain"}}, r)
		},
	} {
		f := newInstallFake(t, time.Minute)
		f.locks = []lockState{{Held: true, PID: 4242}, {}}
		r := provider.NewRun("op-"+name, nil)
		s, err := prep(f, context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if f.resolutions != 2 || f.probes < 2 || !f.hasPre(r.ID()) || s.KillMode != detach.KillProcess {
			t.Errorf("%s: %d resolutions around %d probes, pre.json %v, spec %+v", name, f.resolutions, f.probes,
				f.hasPre(r.ID()), s)
		}
		if got := s.Argv[len(s.Argv)-2:]; !slices.Equal(got, []string{"--", "nxplain"}) {
			t.Errorf("%s: %q", name, s.Argv)
		}

		f = newInstallFake(t, time.Minute)
		f.gone = true
		if _, err := prep(f, context.Background(), provider.NewRun("op-gone-"+name, nil)); slugOf(t, err) != problem.NotFound {
			t.Errorf("%s: package gone after the wait: %v, want not-found", name, err)
		}

		f = newInstallFake(t, time.Hour)
		f.locks = []lockState{{Held: true, PID: 4242}}
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		r = provider.NewRun("op-cancel-"+name, nil)
		_, err = prep(f, ctx, r)
		cancel()
		if !errors.Is(err, context.Canceled) || f.hasPre(r.ID()) {
			t.Errorf("%s: cancel during the wait: %v, pre.json %v", name, err, f.hasPre(r.ID()))
		}
		for _, c := range f.commands() {
			if c[0] == Dpkg && slices.Contains(c, "--audit") {
				t.Errorf("%s: audit ran after the cancel", name)
			}
		}
	}
	for _, op := range []def.Op{spec.Install.Op(), spec.Remove.Op()} {
		if op.Cancel != def.CancelQueuedOnly {
			t.Errorf("%s: Cancel %q, want queued-only", op.ID, op.Cancel)
		}
	}
}

// whatIf takes no lane, no lock, no audit and no snapshot: the unit is the
// simulation of the same command, and preview.json records every package
// dpkg knows with its architecture.
func TestPreparePackagesPreview(t *testing.T) {
	for _, l := range []func() (provider.Lane, error){
		func() (provider.Lane, error) {
			return installLane(context.Background(), spec.InstallInput{WhatIf: true})
		},
		func() (provider.Lane, error) { return removeLane(context.Background(), spec.RemoveInput{WhatIf: true}) },
	} {
		if got, err := l(); err != nil || got != provider.NoLane {
			t.Errorf("whatIf lane %+v %v", got, err)
		}
	}
	for _, l := range []func() (provider.Lane, error){
		func() (provider.Lane, error) { return installLane(context.Background(), spec.InstallInput{}) },
		func() (provider.Lane, error) { return removeLane(context.Background(), spec.RemoveInput{}) },
	} {
		if got, err := l(); err != nil || got != (provider.Lane{Key: LaneKey, Policy: provider.LaneQueue}) {
			t.Errorf("lane %+v %v", got, err)
		}
	}

	f := newInstallFake(t, time.Minute)
	f.locks = []lockState{{Held: true, PID: 4242}} // Never probed
	r := provider.NewRun("op-preview", nil)
	s, err := f.p.prepareRemove(context.Background(), spec.RemoveInput{Names: []string{"nxplain"}, Purge: true, WhatIf: true}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, previewArgv(removeCommand([]string{"nxplain"}, true, false))) || f.probes != 0 || f.hasPre(r.ID()) {
		t.Errorf("argv %q, probes %d, pre.json %v", s.Argv, f.probes, f.hasPre(r.ID()))
	}
	b, err := os.ReadFile(filepath.Join(f.ops, r.ID(), PreviewFile))
	if err != nil {
		t.Fatal(err)
	}
	var st previewState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Architectures) < 50 || st.Architectures["dpkg"] == "" {
		t.Errorf("preview.json architectures: %d", len(st.Architectures))
	}
}

// A-26 at T0, on real apt: for each captured step, the preview built from
// the provider's simulation equals the dpkg-query diff of the real run
// that followed on the same state. Default remove leaves the dependency
// and config-files state; purge leaves nothing; autoremove removes exactly
// the dependency apt reported. The failing steps change nothing.
func TestPackagesPreviewEqualsCapturedOutcome(t *testing.T) {
	for _, base := range installBases {
		for _, step := range []string{"install-plus", "install-pin", "remove-remove", "remove-purge", "remove-autoremove",
			"remove-purge-autoremove"} {
			before, after := capture(t, base, step+".dpkg-before"), capture(t, base, step+".dpkg-after")
			st := previewState{Architectures: map[string]string{}}
			for _, d := range parseDpkgQuery(before) {
				st.Architectures[d.Package] = d.Architecture
			}
			pv := buildPreview(parseSimulation(capture(t, base, step+".sim.stdout")), st)
			up, ins, rem := statusDiff(before, after)
			if got := previewStrings(pv.Upgraded); !slices.Equal(got, up) {
				t.Errorf("%s %s: preview upgraded %q, real %q", base, step, got, up)
			}
			if got := previewStrings(pv.Installed); !slices.Equal(got, ins) {
				t.Errorf("%s %s: preview installed %q, real %q", base, step, got, ins)
			}
			if got := previewStrings(pv.Removed); !slices.Equal(got, rem) {
				t.Errorf("%s %s: preview removed %q, real %q", base, step, got, rem)
			}
			t.Logf("%s %s: upgraded %q installed %q removed %q", base, step, up, ins, rem)
		}
		after := func(step, pkg string) string {
			for _, d := range parseDpkgQuery(capture(t, base, step+".dpkg-after")) {
				if d.Package == pkg {
					return d.Status
				}
			}
			return "absent"
		}
		for _, tc := range []struct{ step, nxapp, nxdep string }{
			{"remove-remove", "config-files", "installed"},
			{"remove-purge", "absent", "installed"},
			{"remove-autoremove", "config-files", "absent"},
			{"remove-purge-autoremove", "absent", "absent"},
		} {
			if a, d := after(tc.step, "nxapp"), after(tc.step, "nxdep"); a != tc.nxapp || d != tc.nxdep {
				t.Errorf("%s %s: nxapp %s nxdep %s, want %s %s", base, tc.step, a, d, tc.nxapp, tc.nxdep)
			}
		}
		// A purge of a package left with its configuration files: a Purg
		// line without a version.
		acts := parseSimulation(capture(t, base, "remove-purge-rc.sim.stdout"))
		if len(acts) != 1 || acts[0].Op != "Remv" || acts[0].Name != "nxapp" || acts[0].OldVersion != "" {
			t.Errorf("%s purge of config-files: %+v", base, acts)
		}
		for _, step := range []string{"install-conflict", "install-badversion", "install-downgrade"} {
			if captureCode(t, base, step+".run") != aptExitFailure || captureCode(t, base, step+".sim") != aptExitFailure ||
				string(capture(t, base, step+".dpkg-before")) != string(capture(t, base, step+".dpkg-after")) {
				t.Errorf("%s %s: the refused install changed packages or did not fail", base, step)
			}
		}
	}
}

// TC-ERR54 and TC-ERR55 at T0: apt-get's own refusals, read from the
// operation's log, are apt-failed with a detail that says why: a removal
// install never does, a downgrade it never does; a version apt does not
// have is apt-failed naming the status. The simulation says the same.
func TestPackagesFailureExplainsRefusals(t *testing.T) {
	for _, base := range installBases {
		for step, want := range map[string]string{
			"install-conflict":   "needs packages removed, and install never removes one; nothing was changed",
			"install-downgrade":  "install never downgrades a package; nothing was changed",
			"install-badversion": "exited with status 100; its output is in the operation log",
		} {
			for _, whatIf := range []bool{false, true} {
				f, r := finalizeFixture(t)
				run := ".run"
				if whatIf {
					run = ".sim"
				}
				writeOpOutput(t, f.ops, r.ID(), capture(t, base, step+run+".stdout"), capture(t, base, step+run+".stderr"))
				x := detach.Exit{ExitCode: captureCode(t, base, step+run)}
				_, err := f.p.finalizeInstall(context.Background(), spec.InstallInput{Names: []string{"nxconfl"}, WhatIf: whatIf}, x, r)
				if slugOf(t, err) != problem.AptFailed || !strings.Contains(err.Error(), want) {
					t.Errorf("%s %s whatIf %v: %v, want apt-failed with %q", base, step, whatIf, err, want)
				}
			}
		}
	}
}

// Finalize of a whatIf reads preview.json and the simulation output, files
// only: a Purg is a removal, with its architecture from preview.json. A
// real run is FinalizeMutation's outcome.
func TestFinalizePackages(t *testing.T) {
	base := installBases[0]
	f := newInstallFake(t, time.Minute)
	r := provider.NewRun("op-whatif", nil)
	if _, err := f.p.prepareRemove(context.Background(), spec.RemoveInput{Names: []string{"nxplain"}, WhatIf: true}, r); err != nil {
		t.Fatal(err)
	}
	writeOpOutput(t, f.ops, r.ID(), capture(t, base, "remove-purge-autoremove.sim.stdout"), nil)
	res, err := f.p.finalizeRemove(context.Background(), spec.RemoveInput{WhatIf: true}, detach.Exit{}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !res.WhatIf || res.Changed || res.Preview == nil || len(res.Changes) != 0 ||
		!slices.Equal(previewStrings(res.Preview.Removed), []string{"nxapp: 1.0>", "nxdep: 1.0>"}) ||
		len(res.Preview.Installed)+len(res.Preview.Upgraded) != 0 {
		t.Errorf("whatIf result %+v %+v", res, res.Preview)
	}

	f2, r2 := finalizeFixture(t)
	got, err := f2.p.finalizeInstall(context.Background(), spec.InstallInput{Names: []string{"nxplain"}}, detach.Exit{}, r2)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f2.p.FinalizeMutation(r2, detach.Exit{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Changed || got.WhatIf || got.Preview != nil || len(got.Changes) != len(out.Changes) ||
		!slices.Equal(got.ConffilesKept, out.Conffiles) || got.RebootRequired != out.RebootRequired || got.ExitCode != 0 {
		t.Errorf("result %+v\noutcome %+v", got, out)
	}
}

// The declarations: packages:install for both (FR-22); install mutating and
// remove destructive; long-running with whatIf; side effects stated; names
// and version constrained; both bound as detached operations.
func TestInstallAndRemoveDeclarations(t *testing.T) {
	p := New(Options{})
	for _, tc := range []struct {
		op   def.Op
		risk def.Risk
		tool string
	}{{spec.Install.Op(), def.RiskMutating, "install_packages"}, {spec.Remove.Op(), def.RiskDestructive, "remove_packages"}} {
		op := tc.op
		if op.Scope.Name != spec.InstallScope || spec.InstallScope != "packages:install" || op.Risk != tc.risk ||
			op.Mode != def.ModeLongRunning || !op.WhatIf || op.Kind != def.KindProviderAction || op.MCP == nil ||
			op.MCP.Tool != tc.tool || op.CLI.Noun != "package" {
			t.Errorf("op %+v", op)
		}
		for _, want := range []string{"Side effects:", "Long-running", "never reboots", "never prompts", "package-manager-busy",
			"package-manager-broken", "apt-failed", "not-found", "whatIf"} {
			if !strings.Contains(op.Description, want) {
				t.Errorf("%s description lacks %q", op.ID, want)
			}
		}
		r, err := op.InputSchema()
		if err != nil {
			t.Fatal(err)
		}
		n := r.Schema().Properties["names"]
		if n == nil || n.MaxItems == nil || *n.MaxItems != spec.MaxInstallNames || n.Items.Pattern != spec.NamePattern ||
			n.Items.Not == nil || n.Items.Not.Pattern != spec.DebFilePattern || !slices.Contains(r.Schema().Required, "names") {
			t.Errorf("%s names %+v", op.ID, n)
		}
		if !slices.ContainsFunc(spec.Spec().Ops, func(o def.Op) bool { return o.ID == op.ID }) {
			t.Errorf("%s is not in the provider's spec", op.ID)
		}
		if !slices.ContainsFunc(p.Bindings(), func(b provider.Binding) bool {
			return b.Op().ID == op.ID && b.Kind() == provider.BindDetached
		}) {
			t.Errorf("%s is not bound as a detached operation", op.ID)
		}
	}
	v := spec.Install.Op()
	r, err := v.InputSchema()
	if err != nil {
		t.Fatal(err)
	}
	if ver := r.Schema().Properties["version"]; ver == nil || ver.Pattern != spec.VersionPattern || ver.MaxLength == nil {
		t.Errorf("version %+v", ver)
	}
	for _, f := range []string{"purge", "autoremove"} {
		if _, ok := spec.Remove.Op().InputType().FieldByNameFunc(func(n string) bool { return strings.EqualFold(n, f) }); !ok {
			t.Errorf("remove lacks %s", f)
		}
	}
}
