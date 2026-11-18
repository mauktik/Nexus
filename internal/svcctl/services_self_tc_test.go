// Tests of planning/services-provider/test-cases.md TC-SV-67, TC-SV-68,
// TC-SV-69, TC-SV-70, TC-SV-71, TC-SV-72, TC-SV-E12, TC-SV-E13 and
// TC-SV-ERR16 (WI 3.4), and the fuzz targets FuzzCgroupSelf and FuzzDeptree.
// The capture-backed cases iterate over every capture directory under
// testdata/services, so host captures (WI 6.0) join without a code change.

package svcctl

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/platform"
)

// copyCaptureRoot is F-ROOTS: a copy of capture's proc/, run/ and etc/
// subtrees in a new temp root, so a variant edits the copy and never the
// committed capture. The root's proc, run and etc are ProcRoot, RunRoot and
// the etc tree.
func copyCaptureRoot(t *testing.T, capture string) string {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"proc", "run", "etc"} {
		src := filepath.Join(capture, sub)
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.CopyFS(filepath.Join(root, sub), os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// capturesOf are the capture directories whose init system is init
// ("systemd" or "openrc"), with their parsed PROVENANCE.
func capturesOf(t *testing.T, init string) map[string]captureProvenance {
	t.Helper()
	m := map[string]captureProvenance{}
	for _, dir := range captureDirectories(t) {
		if p := readCaptureProvenance(t, dir); p.init() == init {
			m[dir] = p
		}
	}
	if len(m) == 0 {
		t.Fatalf("no %s capture under %s", init, servicesCaptures)
	}
	return m
}

var provenanceCgroupPID = regexp.MustCompile(`^  proc/self/cgroup: \S+ /proc/([0-9]+)/cgroup$`)

// capturedPID is the PID whose /proc/<pid>/cgroup the capture copied to
// proc/self/cgroup, from its PROVENANCE command line.
func capturedPID(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "PROVENANCE"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if m := provenanceCgroupPID.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil {
			pid, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
	}
	t.Fatalf("%s/PROVENANCE names no /proc/<pid>/cgroup", dir)
	return 0
}

func selfWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeProcCgroup is a proc tree whose <pid>/cgroup is content.
func fakeProcCgroup(t *testing.T, pid int, content string) string {
	t.Helper()
	proc := filepath.Join(t.TempDir(), "proc")
	selfWriteFile(t, filepath.Join(proc, strconv.Itoa(pid), "cgroup"), content)
	return proc
}

// TC-SV-67: SelfService on systemd takes the last .service element of the 0:: cgroup line. Owner WI 3.4. Requirements: FR-9, A-11.
//
// - Setup: F-ROOTS `proc/<pid>/cgroup` from each systemd capture (`0::/system.slice/nexusd.service`). Variants: `0::/system.slice/nexus-agent.service` (renamed), `0::/system.slice/nexusd.service/payload`, a hybrid file with `1:name=systemd:/…` lines before `0::`, and a file with no `.service` element (`0::/user.slice`).
// - Steps:
//  1. `SelfService(platform.Systemd, procRoot, "", pid)`.
//
// - Expected:
//   - `nexusd`, `nexus-agent`, and `nexusd` (the last `.service` element);
//   - the hybrid file resolves from the `0::` line;
//   - `/user.slice` gives `("", ErrNoSelfService)`.
func TestTCSV67_SelfServiceOnSystemdTakesTheLastService(t *testing.T) {
	for dir := range capturesOf(t, "systemd") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			root := copyCaptureRoot(t, dir)
			pid := capturedPID(t, dir)
			b, err := os.ReadFile(filepath.Join(root, "proc", "self", "cgroup"))
			if err != nil {
				t.Fatal(err)
			}
			selfWriteFile(t, filepath.Join(root, "proc", strconv.Itoa(pid), "cgroup"), string(b))
			got, err := SelfService(platform.Systemd, filepath.Join(root, "proc"), "", pid)
			if err != nil || got != "nexusd" {
				t.Errorf("SelfService = (%q, %v), want (nexusd, nil)", got, err)
			}
		})
	}
	const pid = 4242
	for _, c := range []struct {
		name, cgroup, want string
		err                error
	}{
		{"renamed", "0::/system.slice/nexus-agent.service\n", "nexus-agent", nil},
		{"sub-cgroup", "0::/system.slice/nexusd.service/payload\n", "nexusd", nil},
		{"nested", "0::/docker/abc/system.slice/nexusd.service\n", "nexusd", nil},
		{"no trailing newline", "0::/system.slice/nexusd.service", "nexusd", nil},
		{"hybrid", "12:pids:/system.slice/other.service\n1:name=systemd:/system.slice/legacy.service\n0::/system.slice/nexusd.service\n", "nexusd", nil},
		{"hybrid without a 0:: service", "1:name=systemd:/system.slice/legacy.service\n0::/user.slice\n", "", ErrNoSelfService},
		{"user slice", "0::/user.slice\n", "", ErrNoSelfService},
		{"bare suffix", "0::/system.slice/.service\n", "", ErrNoSelfService},
		{"invalid name", "0::/system.slice/a;b.service\n", "", ErrNoSelfService},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := SelfService(platform.Systemd, fakeProcCgroup(t, pid, c.cgroup), "", pid)
			if got != c.want || !errors.Is(err, c.err) || (c.err == nil && err != nil) {
				t.Errorf("SelfService = (%q, %v), want (%q, %v)", got, err, c.want, c.err)
			}
		})
	}
}

// recordingOpen is openRegular that records every path it opens.
func recordingOpen(opened *[]string) func(string) (*os.File, os.FileInfo, error) {
	return func(path string) (*os.File, os.FileInfo, error) {
		*opened = append(*opened, path)
		return openRegular(path)
	}
}

// capturedChildPID is the PID in the capture's options/nexusd/child_pid.
func capturedChildPID(t *testing.T, root string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "run", "openrc", "options", "nexusd", "child_pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// TC-SV-68: SelfService on OpenRC finds the script whose child_pid is this process. Owner WI 3.4. Requirements: FR-9, A-8.
//
// - Setup: F-ROOTS `run/openrc/options/` from `t1-alpine-openrc`, plus `nginx/child_pid` = 4242 and a renamed `nexus-agent/child_pid` = the target PID.
// - Steps:
//  1. `SelfService(platform.OpenRC, "", runRoot, pid)` for the captured nexusd PID and for the renamed one.
//
// - Expected: `nexusd` and `nexus-agent` respectively. Only `child_pid` files are opened, with no other file read.
func TestTCSV68_SelfServiceOnOpenRCFindsTheScriptWhose(t *testing.T) {
	for dir := range capturesOf(t, "openrc") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			for _, rename := range []string{"", "nexus-agent"} {
				root := copyCaptureRoot(t, dir)
				run := filepath.Join(root, "run")
				options := filepath.Join(run, "openrc", "options")
				pid := capturedChildPID(t, root)
				want := "nexusd"
				if rename != "" {
					if err := os.Rename(filepath.Join(options, "nexusd"), filepath.Join(options, rename)); err != nil {
						t.Fatal(err)
					}
					want = rename
				}
				selfWriteFile(t, filepath.Join(options, "nginx", "child_pid"), "4242")
				// The other option files supervise-daemon writes, which must
				// never be opened.
				for _, svc := range []string{"nginx", want} {
					for _, f := range []string{"pidfile", "exec", "argv_0", "start_time"} {
						selfWriteFile(t, filepath.Join(options, svc, f), strconv.Itoa(pid))
					}
				}

				got, err := SelfService(platform.OpenRC, "", run, pid)
				if err != nil || got != want {
					t.Errorf("SelfService = (%q, %v), want (%q, nil)", got, err, want)
				}
				var opened []string
				got, err = selfServiceOpenRC(run, pid, recordingOpen(&opened))
				if err != nil || got != want {
					t.Errorf("selfServiceOpenRC = (%q, %v), want (%q, nil)", got, err, want)
				}
				if len(opened) == 0 {
					t.Error("no child_pid file was opened")
				}
				for _, p := range opened {
					if filepath.Base(p) != "child_pid" {
						t.Errorf("opened %s, want only child_pid files", p)
					}
				}
			}
		})
	}
}

// selfFakeRun is an Options.Run that records argv and answers out with exit.
func selfFakeRun(argvs *[][]string, out string, exit int) func(context.Context, []string) (Result, error) {
	return func(_ context.Context, argv []string) (Result, error) {
		*argvs = append(*argvs, slices.Clone(argv))
		return Result{ExitCode: exit, Output: []byte(out)}, nil
	}
}

// TC-SV-69: Dependencies on systemd keeps the .service units of Requires, BindsTo, Requisite and PartOf. Owner WI 3.4. Requirements: FR-9, A-39.
//
// - Setup: Each systemd capture's `deps-nexusd.txt` (captured with `show -p Requires,BindsTo,Requisite,PartOf`, WI 3.1), plus a variant `Requires=sysinit.target nexusd-db.service -.mount` / `BindsTo=nx-bound.service` / `Requisite=` / `PartOf=nx-part.service`, in the order and `Key=value` form systemd prints for that argv.
// - Steps:
//  1. `Dependencies(ctx, platform.Systemd, "nexusd", …)`.
//
// - Expected:
//   - for each capture: exactly its `.service` names without the suffix;
//   - for the variant: `[nexusd-db nx-bound nx-part]` (a `PartOf=` unit is kept exactly like a `Requires=` or `BindsTo=` unit; `sysinit.target` and `-.mount` are dropped as non-`.service`);
//   - the argv is exactly `/usr/bin/systemctl show -p Requires -p BindsTo -p Requisite -p PartOf -- nexusd.service` (no `StopPropagatedFrom`, A-39).
func TestTCSV69_DependenciesOnSystemdKeepsTheServiceUnits(t *testing.T) {
	wantArgv := []string{"/usr/bin/systemctl", "show", "-p", "Requires", "-p", "BindsTo", "-p", "Requisite", "-p", "PartOf",
		"--", "nexusd.service"}
	check := func(t *testing.T, out string, want []string) {
		t.Helper()
		var argvs [][]string
		got, err := Dependencies(context.Background(), platform.Systemd, "nexusd", Options{Run: selfFakeRun(&argvs, out, 0)})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Names, want) || got.Names == nil || got.Partial {
			t.Errorf("Dependencies = %+v, want Names %q, not partial", got, want)
		}
		if len(argvs) != 1 || !slices.Equal(argvs[0], wantArgv) {
			t.Errorf("argv = %q, want exactly one call %q", argvs, wantArgv)
		}
	}
	for dir := range capturesOf(t, "systemd") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			lines, exit := captureLines(t, filepath.Join(dir, "deps-nexusd.txt"))
			if exit != "Exit 0" {
				t.Fatalf("deps-nexusd.txt records %q, want Exit 0", exit)
			}
			// The expectation: every .service word of the four keys, by
			// hand from the capture.
			want := []string{}
			for _, l := range lines {
				_, v, _ := strings.Cut(l, "=")
				for _, u := range strings.Fields(v) {
					if n, ok := strings.CutSuffix(u, ".service"); ok {
						want = append(want, n)
					}
				}
			}
			slices.Sort(want)
			check(t, strings.Join(lines, "\n")+"\n", want)
		})
	}
	t.Run("variant with PartOf", func(t *testing.T) {
		check(t, "Requires=sysinit.target nexusd-db.service -.mount\nRequisite=\nBindsTo=nx-bound.service\nPartOf=nx-part.service\n",
			[]string{"nexusd-db", "nx-bound", "nx-part"})
	})
	t.Run("other keys are ignored", func(t *testing.T) {
		check(t, "Wants=nx-wanted.service\nRequires=nx-a.service nx-a.service\nStopPropagatedFrom=nx-stop.service\n", []string{"nx-a"})
	})
	t.Run("non-zero exit is an error", func(t *testing.T) {
		var argvs [][]string
		if _, err := Dependencies(context.Background(), platform.Systemd, "nexusd", Options{Run: selfFakeRun(&argvs, "Failed", 1)}); err == nil {
			t.Error("Dependencies with systemctl exit 1 = nil error, want an error")
		}
	})
	t.Run("invalid self is refused before any exec", func(t *testing.T) {
		var argvs [][]string
		_, err := Dependencies(context.Background(), platform.Systemd, "a b", Options{Run: selfFakeRun(&argvs, "", 0)})
		if !errors.Is(err, ErrNotStarted) || len(argvs) != 0 {
			t.Errorf("Dependencies(\"a b\") = %v after %d calls, want ErrNotStarted and none", err, len(argvs))
		}
	})
	t.Run("unsupported init", func(t *testing.T) {
		if _, err := Dependencies(context.Background(), platform.InitSystem("sysvinit"), "nexusd", Options{}); err == nil {
			t.Error("Dependencies(sysvinit) = nil error, want an error")
		}
	})
}

var (
	deptreeService    = regexp.MustCompile(`^depinfo_([0-9]+)_service='([^']*)'$`)
	deptreeIneedIndex = regexp.MustCompile(`^depinfo_([0-9]+)_ineed_([0-9]+)=`)
)

// plantNeeds appends ineed lines for names to self's entry in the deptree
// at path, and returns the providers of the net virtual entry, planting one
// provided by networking when the deptree has none (OpenRC inside Docker
// has no net provider, WI 3.1).
func plantNeeds(t *testing.T, path, self string, names ...string) (netProviders []string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	selfIdx, netIdx, maxIdx := -1, -1, -1
	nextIneed := map[int]int{}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\n")
		if m := deptreeService.FindStringSubmatch(line); m != nil {
			i, _ := strconv.Atoi(m[1])
			maxIdx = max(maxIdx, i)
			switch m[2] {
			case self:
				selfIdx = i
			case "net":
				netIdx = i
			}
		}
		if m := deptreeIneedIndex.FindStringSubmatch(line); m != nil {
			i, _ := strconv.Atoi(m[1])
			j, _ := strconv.Atoi(m[2])
			nextIneed[i] = max(nextIneed[i], j+1)
		}
	}
	if selfIdx < 0 {
		t.Fatalf("%s has no entry for %s", path, self)
	}
	var add strings.Builder
	for _, n := range names {
		add.WriteString("depinfo_" + strconv.Itoa(selfIdx) + "_ineed_" + strconv.Itoa(nextIneed[selfIdx]) + "='" + n + "'\n")
		nextIneed[selfIdx]++
	}
	if netIdx < 0 {
		netIdx = maxIdx + 1
		add.WriteString("depinfo_" + strconv.Itoa(netIdx) + "_service='net'\n")
		add.WriteString("depinfo_" + strconv.Itoa(netIdx) + "_providedby_0='networking'\n")
		netProviders = []string{"networking"}
	} else {
		p := regexp.MustCompile(`^depinfo_` + strconv.Itoa(netIdx) + `_providedby_[0-9]+='([^']*)'$`)
		for line := range strings.Lines(string(b)) {
			if m := p.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil {
				netProviders = append(netProviders, m[1])
			}
		}
	}
	if err := os.WriteFile(path, append(b, add.String()...), 0o644); err != nil {
		t.Fatal(err)
	}
	return netProviders
}

// TC-SV-70: Dependencies on OpenRC resolves net to its provider from the deptree. Owner WI 3.4. Requirements: FR-9, A-9.
//
// - Setup: `t1-alpine-openrc`'s `run/openrc/deptree`, plus a variant where nexusd also `ineed`s the real script `nx-svc-test`.
// - Steps:
//  1. `Dependencies(ctx, platform.OpenRC, "nexusd", …)`.
//
// - Expected: `net` resolves through the virtual entry's `providedby_*` to the capture's provider (`networking` in the image), and a real script resolves to itself. No `partial` marker.
func TestTCSV70_DependenciesOnOpenRCResolvesNetToIts(t *testing.T) {
	ctx := context.Background()
	for dir := range capturesOf(t, "openrc") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			// As captured: inside Docker OpenRC drops nexusd's `need net`
			// (WI 3.1), so the self entry has no ineed. Every name that does
			// come back is a deptree entry or provider, and nothing is partial.
			root := copyCaptureRoot(t, dir)
			deptree := filepath.Join(root, "run", "openrc", "deptree")
			b, err := os.ReadFile(deptree)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Dependencies(ctx, platform.OpenRC, "nexusd", Options{RunRoot: filepath.Join(root, "run")})
			if err != nil || got.Partial || got.Names == nil {
				t.Fatalf("Dependencies as captured = (%+v, %v), want complete, no error", got, err)
			}
			for _, n := range got.Names {
				if !strings.Contains(string(b), "='"+n+"'") {
					t.Errorf("dependency %q is not in the deptree", n)
				}
			}

			// Variant: nexusd needs net and the real script nx-svc-test.
			root = copyCaptureRoot(t, dir)
			deptree = filepath.Join(root, "run", "openrc", "deptree")
			providers := plantNeeds(t, deptree, "nexusd", "net", "nx-svc-test")
			want := append(slices.Clone(providers), "nx-svc-test")
			slices.Sort(want)
			got, err = Dependencies(ctx, platform.OpenRC, "nexusd", Options{RunRoot: filepath.Join(root, "run")})
			if err != nil || got.Partial || !slices.Equal(got.Names, want) {
				t.Errorf("Dependencies = (%+v, %v), want Names %q, not partial", got, err, want)
			}

			// A needed name with no entry and no provider is kept, and the
			// resolution is marked partial with the name in the reason.
			root = copyCaptureRoot(t, dir)
			plantNeeds(t, filepath.Join(root, "run", "openrc", "deptree"), "nexusd", "nx-nosuch")
			got, err = Dependencies(ctx, platform.OpenRC, "nexusd", Options{RunRoot: filepath.Join(root, "run")})
			if err != nil || !got.Partial || !slices.Contains(got.Names, "nx-nosuch") || !strings.Contains(got.Reason, "nx-nosuch") {
				t.Errorf("Dependencies with an unresolvable need = (%+v, %v), want nx-nosuch kept and partial naming it", got, err)
			}
		})
	}
}

// TC-SV-71: RebootArgv is the fixed command per init system. Owner WI 3.4. Requirements: FR-10, FR-12, A-5.
//
// - Setup: None.
// - Steps:
//  1. `RebootArgv(platform.Systemd)`, `(platform.OpenRC)` and `(platform.InitSystem("sysvinit"))`.
//
// - Expected:
//   - `[/usr/bin/systemctl reboot]`;
//   - `[/sbin/reboot]` (no `-f`, no other argument);
//   - an error naming the init system.
func TestTCSV71_RebootArgvIsTheFixedCommandPerInit(t *testing.T) {
	for init, want := range map[platform.InitSystem][]string{
		platform.Systemd: {"/usr/bin/systemctl", "reboot"},
		platform.OpenRC:  {"/sbin/reboot"},
	} {
		got, err := RebootArgv(init)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("RebootArgv(%s) = (%q, %v), want (%q, nil)", init, got, err, want)
		}
	}
	for _, init := range []platform.InitSystem{"sysvinit", platform.Unknown, ""} {
		got, err := RebootArgv(init)
		if err == nil || got != nil || !strings.Contains(err.Error(), `"`+string(init)+`"`) {
			t.Errorf("RebootArgv(%q) = (%q, %v), want (nil, an error naming it)", init, got, err)
		}
	}
	// The returned argv is a copy: a caller cannot change the next one.
	a, _ := RebootArgv(platform.OpenRC)
	a[0] = "/bin/sh"
	if b, _ := RebootArgv(platform.OpenRC); b[0] != "/sbin/reboot" {
		t.Errorf("RebootArgv after a caller's write = %q", b)
	}
}

// cgroupSelfSeeds are every capture's proc/self/cgroup, plus the variants.
func cgroupSelfSeeds(tb testing.TB) [][]byte {
	tb.Helper()
	seeds := [][]byte{
		[]byte("0::/system.slice/nexus-agent.service\n"),
		[]byte("0::/system.slice/nexusd.service/payload\n"),
		[]byte("1:name=systemd:/system.slice/legacy.service\n0::/system.slice/nexusd.service\n"),
		[]byte("0::/user.slice\n"),
		[]byte("0::/init.scope\n"),
		[]byte("0::/system.slice/.service\n"),
	}
	dirs, err := filepath.Glob(filepath.Join(servicesCaptures, "*", "proc", "self", "cgroup"))
	if err != nil {
		tb.Fatal(err)
	}
	for _, p := range dirs {
		b, err := os.ReadFile(p)
		if err != nil {
			tb.Fatal(err)
		}
		seeds = append(seeds, b)
	}
	return seeds
}

// deptreeSeeds are every capture's run/openrc/deptree, plus the shipped
// OpenRC script (for the init-script fallback).
func deptreeSeeds(tb testing.TB) [][]byte {
	tb.Helper()
	paths, err := filepath.Glob(filepath.Join(servicesCaptures, "*", "run", "openrc", "deptree"))
	if err != nil {
		tb.Fatal(err)
	}
	paths = append(paths, filepath.Join("..", "..", "deploy", "nexusd.openrc"))
	var seeds [][]byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			tb.Fatal(err)
		}
		seeds = append(seeds, b)
	}
	seeds = append(seeds, []byte("depinfo_0_service='nexusd'\ndepinfo_0_ineed_0='net'\ndepinfo_1_service='net'\ndepinfo_1_providedby_0='networking'\n"),
		[]byte("depend() { need net }\n"))
	return seeds
}

func FuzzCgroupSelf(f *testing.F) {
	for _, s := range cgroupSelfSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if name := parseCgroupSelf(b); name != "" && !serviceName.MatchString(name) {
			t.Errorf("parseCgroupSelf(%q) = %q, which does not match ServiceNamePattern", b, name)
		}
	})
}

func FuzzDeptree(f *testing.F) {
	for _, s := range deptreeSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		d := resolveDeptree(parseDeptree(b), "nexusd")
		for _, n := range append(d.Names, initScriptNeeds(b)...) {
			if !serviceName.MatchString(n) {
				t.Errorf("deptree %q gives %q, which does not match ServiceNamePattern", b, n)
			}
		}
		for _, n := range systemdDependencyNames(string(b)) {
			if !serviceName.MatchString(n) {
				t.Errorf("show output %q gives %q, which does not match ServiceNamePattern", b, n)
			}
		}
	})
}

// TC-SV-72: The cgroup and deptree parsers survive fuzzing. Owner WI 3.4. Requirements: NFR-3.
//
// - Setup: `FuzzCgroupSelf` and `FuzzDeptree`, seeded from the captures.
// - Steps:
//  1. Seeds, plus 60 s `-fuzz` per target.
//
// - Expected: No panic, and every result is a name matching `ServiceNamePattern` or empty. `TestTCSV72_TheCgroupAndDeptreeParsersSurviveFuzzing` asserts that the targets exist and are seeded.
func TestTCSV72_TheCgroupAndDeptreeParsersSurviveFuzzing(t *testing.T) {
	// The targets exist (this does not compile without them).
	_, _ = FuzzCgroupSelf, FuzzDeptree
	// Every capture seeds its parser: one cgroup per capture, one deptree
	// per OpenRC capture, plus the variants.
	cg := cgroupSelfSeeds(t)
	if n := len(captureDirectories(t)); len(cg) < n+6 {
		t.Errorf("FuzzCgroupSelf has %d seeds, want every one of the %d captures plus 6 variants", len(cg), n)
	}
	dt := deptreeSeeds(t)
	if n := len(capturesOf(t, "openrc")); len(dt) < n+3 {
		t.Errorf("FuzzDeptree has %d seeds, want every one of the %d OpenRC captures plus 3", len(dt), n)
	}
	// The seeds themselves give a name or nothing.
	for _, s := range cg {
		if name := parseCgroupSelf(s); name != "" && !serviceName.MatchString(name) {
			t.Errorf("parseCgroupSelf(%q) = %q", s, name)
		}
	}
	if got := parseCgroupSelf(cg[0]); got != "nexus-agent" {
		t.Errorf("parseCgroupSelf(seed 0) = %q, want nexus-agent", got)
	}
}

// TC-SV-E12: SelfService ignores an oversized child_pid file and a non-matching PID. Owner WI 3.4. Requirements: FR-9.
//
// - Setup: `options/a/child_pid` of 100 bytes, starting with the target PID; `options/b/child_pid` = `999999`; `options/nexusd/child_pid` = the target PID.
// - Steps:
//  1. `SelfService`.
//
// - Expected: `nexusd`. The oversized file is skipped (bounded read at 64 bytes), never matched by prefix.
func TestTCSVE12_SelfServiceIgnoresAnOversizedChildPidFile(t *testing.T) {
	const pid = 4242
	run := filepath.Join(t.TempDir(), "run")
	options := filepath.Join(run, "openrc", "options")
	oversized := "4242\n" + strings.Repeat(" ", 95)
	if len(oversized) != 100 {
		t.Fatalf("oversized child_pid is %d bytes", len(oversized))
	}
	selfWriteFile(t, filepath.Join(options, "a", "child_pid"), oversized)
	selfWriteFile(t, filepath.Join(options, "b", "child_pid"), "999999")
	selfWriteFile(t, filepath.Join(options, "nexusd", "child_pid"), "4242\n")
	got, err := SelfService(platform.OpenRC, "", run, pid)
	if err != nil || got != "nexusd" {
		t.Errorf("SelfService = (%q, %v), want (nexusd, nil)", got, err)
	}
	// With only the oversized file there is no match at all.
	if err := os.RemoveAll(filepath.Join(options, "nexusd")); err != nil {
		t.Fatal(err)
	}
	got, err = SelfService(platform.OpenRC, "", run, pid)
	if got != "" || !errors.Is(err, ErrNoSelfService) {
		t.Errorf("SelfService with only the oversized match = (%q, %v), want ErrNoSelfService", got, err)
	}
	// A 64-byte file is at the bound and still read.
	selfWriteFile(t, filepath.Join(options, "c", "child_pid"), "4242"+strings.Repeat(" ", 60))
	if got, err := SelfService(platform.OpenRC, "", run, pid); err != nil || got != "c" {
		t.Errorf("SelfService with a 64-byte child_pid = (%q, %v), want (c, nil)", got, err)
	}
}

// TC-SV-E13: An unreadable deptree returns the needed names with the partial marker. Owner WI 3.4. Requirements: FR-9, A-9.
//
// - Setup: F-ROOTS with the deptree removed, and nexusd's init script captured as `depend() { need net }`.
// - Steps:
//  1. `Dependencies`.
//
// - Expected: `[net]` with the `partial` marker set, and no error.
func TestTCSVE13_AnUnreadableDeptreeReturnsTheNeededNames(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "deploy", "nexusd.openrc"))
	if err != nil {
		t.Fatal(err)
	}
	for dir := range capturesOf(t, "openrc") {
		for _, c := range []struct {
			name, script string
			want         []string
		}{
			{"one-line depend", "depend() { need net }\n", []string{"net"}},
			{"the shipped script", string(shipped), []string{"net"}},
			{"need outside depend is ignored", "start_pre() {\n\tneed nx-other\n}\ndepend() {\n\tneed net; use dns\n\tafter firewall\n}\n", []string{"net"}},
			{"no script", "", []string{}},
		} {
			t.Run(filepath.Base(dir)+"/"+c.name, func(t *testing.T) {
				root := copyCaptureRoot(t, dir)
				if err := os.Remove(filepath.Join(root, "run", "openrc", "deptree")); err != nil {
					t.Fatal(err)
				}
				script := filepath.Join(root, "etc", "init.d", "nexusd")
				if c.script == "" {
					if err := os.Remove(script); err != nil {
						t.Fatal(err)
					}
				} else {
					selfWriteFile(t, script, c.script)
				}
				got, err := Dependencies(context.Background(), platform.OpenRC, "nexusd", Options{RunRoot: filepath.Join(root, "run")})
				if err != nil || !got.Partial || got.Reason == "" || !slices.Equal(got.Names, c.want) {
					t.Errorf("Dependencies = (%+v, %v), want Names %q, partial with a reason, no error", got, err, c.want)
				}
			})
		}
	}
	t.Run("deptree without a self entry", func(t *testing.T) {
		root := t.TempDir()
		selfWriteFile(t, filepath.Join(root, "run", "openrc", "deptree"), "depinfo_0_service='net'\ndepinfo_0_providedby_0='networking'\n")
		selfWriteFile(t, filepath.Join(root, "etc", "init.d", "nexusd"), "depend() { need net }\n")
		got, err := Dependencies(context.Background(), platform.OpenRC, "nexusd", Options{RunRoot: filepath.Join(root, "run")})
		if err != nil || !got.Partial || !slices.Equal(got.Names, []string{"net"}) {
			t.Errorf("Dependencies = (%+v, %v), want [net], partial", got, err)
		}
	})
}

// TC-SV-ERR16: SelfService with no match returns ErrNoSelfService. Owner WI 3.4. Requirements: FR-9.
//
// - Setup: OpenRC options with no matching `child_pid`; a systemd cgroup `0::/init.scope`; a missing cgroup file.
// - Steps:
//  1. `SelfService`.
//
// - Expected: `("", err)` with `errors.Is(err, ErrNoSelfService)` for the first two. The missing file gives an error wrapping `fs.ErrNotExist` (test-strategy Open Item 4).
func TestTCSVERR16_SelfServiceWithNoMatchReturnsErrNoSelfService(t *testing.T) {
	const pid = 4242
	run := filepath.Join(t.TempDir(), "run")
	selfWriteFile(t, filepath.Join(run, "openrc", "options", "nginx", "child_pid"), "17")
	selfWriteFile(t, filepath.Join(run, "openrc", "options", "crond", "pidfile"), "4242")
	if got, err := SelfService(platform.OpenRC, "", run, pid); got != "" || !errors.Is(err, ErrNoSelfService) {
		t.Errorf("OpenRC without a match = (%q, %v), want ErrNoSelfService", got, err)
	}
	if got, err := SelfService(platform.Systemd, fakeProcCgroup(t, pid, "0::/init.scope\n"), "", pid); got != "" || !errors.Is(err, ErrNoSelfService) {
		t.Errorf("systemd 0::/init.scope = (%q, %v), want ErrNoSelfService", got, err)
	}
	got, err := SelfService(platform.Systemd, filepath.Join(t.TempDir(), "proc"), "", pid)
	if got != "" || !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNoSelfService) {
		t.Errorf("missing cgroup = (%q, %v), want an error wrapping fs.ErrNotExist, not ErrNoSelfService", got, err)
	}
	if _, err := SelfService(platform.OpenRC, "", filepath.Join(t.TempDir(), "run"), pid); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing options directory = %v, want an error wrapping fs.ErrNotExist", err)
	}
	if _, err := SelfService(platform.InitSystem("sysvinit"), "", "", pid); err == nil || errors.Is(err, ErrNoSelfService) {
		t.Errorf("sysvinit = %v, want an error that is not ErrNoSelfService", err)
	}
	if _, err := SelfService(platform.Systemd, "", "", 0); err == nil {
		t.Error("pid 0 = nil error, want an error")
	}
}
