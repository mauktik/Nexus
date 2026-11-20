// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-73, TC-SV-74,
// TC-SV-75, TC-SV-76, TC-SV-77, TC-SV-78, TC-SV-79, TC-SV-E14 and
// TC-SV-ERR17 (WI 4.1): the provider skeleton, Configure, Preflight and the
// protected set, over the T1 captures through F-HOSTS
// (svcctltest.NewHost) and F-ROOTS (copies of each capture's proc, run and
// etc trees in one temp root).

package services

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

const systemctlTool = "/usr/bin/systemctl"

// captureNames are the capture directories under svcctl's testdata, each
// with its init system.
func captureNames(t *testing.T) map[string]platform.InitSystem {
	t.Helper()
	names, err := svcctltest.Captures()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]platform.InitSystem{}
	for _, n := range names {
		p, err := svcctltest.ReadProvenance(n)
		if err != nil {
			t.Fatal(err)
		}
		if init := p.Init(); init != "" {
			out[n] = init
		}
	}
	if len(out) == 0 {
		t.Fatal("no capture")
	}
	return out
}

func firstCaptureOf(t *testing.T, init platform.InitSystem, prefer string) string {
	t.Helper()
	all := captureNames(t)
	if all[prefer] == init {
		return prefer
	}
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for k := range all {
			if !yield(k) {
				return
			}
		}
	}) {
		if all[n] == init {
			return n
		}
	}
	t.Fatalf("no %s capture", init)
	return ""
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// toolPaths are the tools each init system's preflight checks, as the
// captures' PROVENANCE calls them (systemctl under /usr/bin, rc-status
// under /bin, rc-update and rc-service under /sbin).
var toolPaths = map[platform.InitSystem][]string{
	platform.Systemd: {"/usr/bin/systemctl", "/usr/bin/journalctl"},
	platform.OpenRC:  {"/sbin/rc-service", "/bin/rc-status", "/sbin/rc-update"},
}

// testRoot is F-ROOTS for capture: its proc, run and etc trees copied into
// one temp root, with this process's own PID put where the capture's
// running nexusd was (proc/<pid>/cgroup from proc/self/cgroup; OpenRC's
// options/nexusd/child_pid), the init system's tools planted, and /.dockerenv
// planted for a capture taken in a Docker container (PROVENANCE source:
// container, image nexus-harness), as Docker creates it in every container.
func testRoot(t *testing.T, capture string) string {
	t.Helper()
	dir, err := svcctltest.CaptureDir(capture)
	if err != nil {
		t.Fatal(err)
	}
	prov, err := svcctltest.ReadProvenance(capture)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, sub := range []string{"proc", "run", "etc"} {
		src := filepath.Join(dir, sub)
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.CopyFS(filepath.Join(root, sub), os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
	// PID 1's mount table, which the captures predate: the boot ID is not
	// readable without it (SEC-1). Unsandboxed, it is nexusd's own.
	if _, err := os.Stat(filepath.Join(root, "proc", "1", "mountinfo")); errors.Is(err, fs.ErrNotExist) {
		own, err := os.ReadFile(filepath.Join(root, "proc", "self", "mountinfo"))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "proc", "1", "mountinfo"), string(own))
	}
	pid := strconv.Itoa(os.Getpid())
	cg, err := os.ReadFile(filepath.Join(root, "proc", "self", "cgroup"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "proc", pid, "cgroup"), string(cg))
	init := prov.Init()
	if init == platform.OpenRC {
		writeFile(t, filepath.Join(root, "run", "openrc", "options", "nexusd", "child_pid"), pid+"\n")
	}
	for _, tool := range toolPaths[init] {
		writeFile(t, filepath.Join(root, tool), "")
	}
	if prov.Keys["source"] == "container" {
		writeFile(t, filepath.Join(root, ".dockerenv"), "")
	}
	return root
}

func newOn(t *testing.T, root string, fh *svcctltest.Host) *Provider {
	t.Helper()
	plat := platform.Info{Init: fh.Init(), OS: platform.OS{ID: "linux"}}
	return New(Options{Host: "h1", Logger: slog.New(slog.DiscardHandler), GOOS: "linux", Platform: &plat, Run: fh.Run, RunLines: fh.RunLines,
		ProcRoot: filepath.Join(root, "proc"), RunRoot: filepath.Join(root, "run"), EtcRoot: filepath.Join(root, "etc"),
		HostFS: os.DirFS(root)})
}

// section is a decoder of the providers.services section body through
// config.Parse, so validate-config's rules and defaults apply.
func section(t *testing.T, body string) func(any) error {
	t.Helper()
	var doc strings.Builder
	doc.WriteString("host:\n  name: h1\nproviders:\n  services:\n")
	for _, l := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		doc.WriteString("    " + l + "\n")
	}
	cfg, err := config.Parse([]byte(doc.String()), "nexusd.yaml")
	if err != nil {
		t.Fatalf("config.Parse:\n%s\n%v", doc.String(), err)
	}
	return cfg.Providers.Services.Decode
}

// strictSection is a strict decoder of body for a section config.Parse
// refuses (a statically protected name, an unknown key): unknown keys are
// errors, as in Section.Decode.
func strictSection(body string) func(any) error {
	return func(into any) error {
		dec := yaml.NewDecoder(strings.NewReader(body))
		dec.KnownFields(true)
		return dec.Decode(into)
	}
}

func configure(t *testing.T, p *Provider, decode func(any) error) {
	t.Helper()
	if err := p.Configure(decode); err != nil {
		t.Fatal(err)
	}
}

func showArgv(name string) []string {
	return []string{systemctlTool, "show", "--timestamp=unix", "-p",
		"Id,LoadState,ActiveState,SubState,UnitFileState,Description,MainPID,ActiveEnterTimestamp,InactiveEnterTimestamp,Result,NRestarts,MemoryCurrent,FragmentPath",
		"--", name + ".service"}
}

func loadStateArgv(names ...string) []string {
	argv := []string{systemctlTool, "show", "-p", "LoadState,Id", "--"}
	for _, n := range names {
		argv = append(argv, n+".service")
	}
	return argv
}

// answerLoadStates makes fh answer preflight's one `show -p LoadState,Id`
// over names with what the capture's own model (a second F-HOSTS over the
// same capture) shows for each unit: its Id and LoadState lines, one
// block per unit in argument order, separated by an empty line, as
// systemctl's show_one prints them. ids overrides a unit's Id (an alias).
func answerLoadStates(t *testing.T, fh *svcctltest.Host, capture string, ids map[string]string, names ...string) {
	t.Helper()
	model := svcctltest.NewHost(t, capture)
	var blocks []string
	for _, n := range names {
		res, err := model.Run(context.Background(), showArgv(n))
		if err != nil {
			t.Fatal(err)
		}
		kv := map[string]string{}
		for line := range strings.Lines(string(res.Output)) {
			if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
				kv[k] = v
			}
		}
		if id, ok := ids[n]; ok {
			kv["Id"] = id
		}
		blocks = append(blocks, "Id="+kv["Id"]+"\nLoadState="+kv["LoadState"]+"\n")
	}
	fh.Answer(loadStateArgv(names...), svcctl.Result{Output: []byte(strings.Join(blocks, "\n"))}, nil)
}

func detail[T any](t *testing.T, st provider.Status, key string) T {
	t.Helper()
	v, ok := st.Details[key]
	if !ok {
		t.Fatalf("details have no %q: %v", key, st.Details)
	}
	got, ok := v.(T)
	if !ok {
		t.Fatalf("details[%q] = %#v (%T), want a %T", key, v, v, *new(T))
	}
	return got
}

func allowEntry(t *testing.T, st provider.Status, name string) AllowListEntry {
	t.Helper()
	for _, e := range detail[[]AllowListEntry](t, st, DetailControlAllowList) {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("controlAllowList has no %s: %v", name, st.Details[DetailControlAllowList])
	return AllowListEntry{}
}

func wantAvailable(t *testing.T, st provider.Status) {
	t.Helper()
	if st.State != apitypes.ProviderEnabled {
		t.Fatalf("Preflight = %s (%s), want enabled", st.State, st.Reason)
	}
}

func wantUnavailable(t *testing.T, st provider.Status, reason string) {
	t.Helper()
	if st.State != apitypes.ProviderUnavailable || !strings.Contains(st.Reason, reason) {
		t.Fatalf("Preflight = %s (%q), want unavailable naming %q", st.State, st.Reason, reason)
	}
}

// TC-SV-73: Preflight is unavailable off Linux and the package compiles there. Owner WI 4.1. Requirements: FR-3, CON-3.
func TestTCSV73_PreflightIsUnavailableOffLinuxAndThe(t *testing.T) {
	t.Run("darwin", func(t *testing.T) {
		fh := svcctltest.NewHost(t, firstCaptureOf(t, platform.Systemd, "t1-ubuntu-systemd"))
		p := New(Options{GOOS: "darwin", Run: fh.Run})
		wantUnavailable(t, p.Preflight(context.Background()), "darwin")
		if calls := fh.Calls(); len(calls) != 0 {
			t.Errorf("F-HOSTS calls = %v, want none", calls)
		}
	})
	t.Run("real GOOS", func(t *testing.T) {
		if runtime.GOOS == "linux" {
			t.Skip("the real GOOS is linux: this subtest is for the darwin dev machine")
		}
		wantUnavailable(t, New(Options{}).Preflight(context.Background()), runtime.GOOS)
	})
}

// TC-SV-74: Preflight is available on every systemd and OpenRC capture with every capabilities detail. Owner WI 4.1. Requirements: FR-3, FR-8, FR-9, CON-3.
func TestTCSV74_PreflightIsAvailableOnEverySystemdAnd(t *testing.T) {
	type variant struct{ capture, name string }
	var variants []variant
	for _, c := range slices.Sorted(func(yield func(string) bool) {
		for k := range captureNames(t) {
			if !yield(k) {
				return
			}
		}
	}) {
		variants = append(variants, variant{c, c})
	}
	vm := firstCaptureOf(t, platform.Systemd, "t1-ubuntu-systemd")
	variants = append(variants, variant{vm, vm + "/vm"})
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			root := testRoot(t, v.capture)
			if strings.HasSuffix(v.name, "/vm") {
				// A VM: Classify over the same files with the container
				// marker removed.
				if err := os.Remove(filepath.Join(root, ".dockerenv")); err != nil {
					t.Fatal(err)
				}
			}
			fh := svcctltest.NewHost(t, v.capture)
			init := fh.Init()
			if init == platform.Systemd {
				answerLoadStates(t, fh, v.capture, nil, "nx-svc-test", "nx-not-installed")
			}
			p := newOn(t, root, fh)
			configure(t, p, section(t, "enabled: true\ncontrol:\n  - name: nx-svc-test\n  - name: nx-not-installed\nreboot:\n  enabled: true\n"))
			st := p.Preflight(context.Background())
			wantAvailable(t, st)

			if got := detail[string](t, st, DetailInitSystem); got != string(init) {
				t.Errorf("initSystem = %q, want %q", got, init)
			}
			wantLog := map[platform.InitSystem]string{platform.Systemd: spec.LogSourceJournal, platform.OpenRC: spec.LogSourceOpenRCFiles}[init]
			if got := detail[string](t, st, DetailLogSource); got != wantLog {
				t.Errorf("logSource = %q, want %q", got, wantLog)
			}
			wantList := []AllowListEntry{
				{Name: "nx-svc-test", Actions: spec.Actions, TimeoutSeconds: 90, Status: StatusOK},
				{Name: "nx-not-installed", Actions: spec.Actions, TimeoutSeconds: 90, Status: StatusMissing},
			}
			got := detail[[]AllowListEntry](t, st, DetailControlAllowList)
			if len(got) != len(wantList) {
				t.Fatalf("controlAllowList = %+v, want %+v", got, wantList)
			}
			for i, w := range wantList {
				g := got[i]
				if g.Name != w.Name || !slices.Equal(g.Actions, w.Actions) || g.TimeoutSeconds != w.TimeoutSeconds || g.Status != w.Status {
					t.Errorf("controlAllowList[%d] = %+v, want %+v", i, g, w)
				}
			}
			if !detail[bool](t, st, DetailRebootEnabled) {
				t.Error("rebootEnabled = false, want true")
			}
			argv, err := svcctl.RebootArgv(init)
			if err != nil {
				t.Fatal(err)
			}
			if got := detail[string](t, st, DetailRebootCommand); got != strings.Join(argv, " ") {
				t.Errorf("rebootCommand = %q, want %q", got, strings.Join(argv, " "))
			}
			// TC-SV-67 and TC-SV-68: every T1 capture's nexusd runs as nexusd.
			if got := detail[string](t, st, DetailSelfService); got != "nexusd" {
				t.Errorf("selfService = %q, want nexusd", got)
			}
			static := map[platform.InitSystem][]string{platform.Systemd: spec.ProtectedSystemd, platform.OpenRC: spec.ProtectedOpenRC}[init]
			prot := detail[[]string](t, st, DetailProtected)
			for _, n := range append(slices.Clone(static), "nexusd") {
				if !slices.Contains(prot, n) {
					t.Errorf("protected = %q, missing %q", prot, n)
				}
			}
			if strings.HasSuffix(v.name, "/vm") {
				if got := detail[string](t, st, DetailBootIDSource); got != BootIDKernel {
					t.Errorf("bootIdSource = %q, want kernel", got)
				}
				if !detail[bool](t, st, DetailRebootAvailable) {
					t.Errorf("rebootAvailable = false (%v), want true on a VM", st.Details[DetailRebootUnavailableReason])
				}
			}
			if s := fh.Sent(); len(s) != 0 {
				t.Errorf("Preflight sent %q, want nothing", s)
			}
		})
	}
}

// bootIDMountLine is a mountinfo line with mount point
// /proc/sys/kernel/random/boot_id, derived from a captured line: the T1
// boot-ID captures hold none (their grep for boot_id is empty), so the
// line is the host provider's captured Docker bind mount of /etc/hostname
// (internal/providers/host/testdata/ubuntu-systemd) with its mount point
// changed, and its device the one the root's boot_id file is on: ReadBoot
// accepts a boot-ID mount only for the file it read (CR-3).
func bootIDMountLine(t *testing.T, root string) string {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(root, "proc", procBootID), &st); err != nil {
		t.Fatal(err)
	}
	d := uint64(st.Dev) // int32 on darwin, uint64 on Linux
	dev := fmt.Sprintf("%d:%d", unix.Major(d), unix.Minor(d))
	b, err := os.ReadFile(filepath.Join("..", "host", "testdata", "ubuntu-systemd", "fs", "proc", "self", "mountinfo"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		f := strings.Split(strings.TrimRight(line, "\n"), " ")
		if len(f) > 4 && f[4] == "/etc/hostname" {
			f[2], f[4] = dev, "/proc/sys/kernel/random/boot_id"
			return strings.Join(f, " ") + "\n"
		}
	}
	t.Fatal("no /etc/hostname mount in the captured mountinfo")
	return ""
}

// TC-SV-75: A container's reboot is available only with a boot-ID mount. Owner WI 4.1. Requirements: FR-3, FR-10, FR-11, A-3, ASM-7.
func TestTCSV75_AContainersRebootIsAvailableOnlyWith(t *testing.T) {
	const capture = "t1-debian-systemd"
	for _, mounted := range []bool{false, true} {
		t.Run(fmt.Sprintf("boot-ID mount %v", mounted), func(t *testing.T) {
			root := testRoot(t, capture)
			if mounted {
				mi := filepath.Join(root, "proc", "self", "mountinfo")
				b, err := os.ReadFile(mi)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, mi, string(b)+bootIDMountLine(t, root))
			}
			fh := svcctltest.NewHost(t, capture)
			p := newOn(t, root, fh)
			configure(t, p, section(t, "enabled: true\nreboot:\n  enabled: true\n"))
			st := p.Preflight(context.Background())
			wantAvailable(t, st)
			avail := detail[bool](t, st, DetailRebootAvailable)
			if !mounted {
				if avail {
					t.Fatal("rebootAvailable = true in a container without a boot-ID mount")
				}
				reason := detail[string](t, st, DetailRebootUnavailableReason)
				for _, w := range []string{"boot ID", "not per-container", "could not be confirmed"} {
					if !strings.Contains(reason, w) {
						t.Errorf("rebootUnavailableReason = %q, want it to name %q", reason, w)
					}
				}
				return
			}
			if !avail {
				t.Fatalf("rebootAvailable = false (%v), want true with a boot-ID mount", st.Details[DetailRebootUnavailableReason])
			}
			if got := detail[string](t, st, DetailBootIDSource); got != BootIDContainerMount {
				t.Errorf("bootIdSource = %q, want container-mount", got)
			}
		})
	}
}

// TC-SV-76: Preflight runs at most two execs and finishes inside the startup bound. Owner WI 4.1. Requirements: FR-3, A-26, NFR-1.
func TestTCSV76_PreflightRunsAtMostTwoExecsAnd(t *testing.T) {
	const capture = "t1-ubuntu-systemd"
	root := testRoot(t, capture)
	fh := svcctltest.NewHost(t, capture)
	answerLoadStates(t, fh, capture, nil, "nx-a", "nx-b", "nx-c")
	p := newOn(t, root, fh)
	configure(t, p, section(t, "enabled: true\ncontrol:\n  - name: nx-a\n  - name: nx-b\n  - name: nx-c\n"))
	start := time.Now()
	st := p.Preflight(context.Background())
	elapsed := time.Since(start)
	wantAvailable(t, st)
	want := [][]string{
		{systemctlTool, "show", "-p", "Requires", "-p", "BindsTo", "-p", "Requisite", "-p", "PartOf", "--", "nexusd.service"},
		loadStateArgv("nx-a", "nx-b", "nx-c"),
	}
	calls := fh.Calls()
	if len(calls) != len(want) {
		t.Fatalf("F-HOSTS calls = %v, want %q", calls, want)
	}
	for i, c := range calls {
		if !slices.Equal(c.Argv, want[i]) {
			t.Errorf("call %d = %q, want %q", i, c.Argv, want[i])
		}
	}
	if elapsed >= time.Second {
		t.Errorf("Preflight took %s, want under 1s", elapsed)
	}
	if s := fh.Sent(); len(s) != 0 {
		t.Errorf("Sent = %q, want nothing", s)
	}
}

// plantNetNeed makes the self entry of the deptree at path ineed net and
// adds a net virtual entry provided by networking (OpenRC inside Docker has
// neither, WI 3.1 and TC-SV-70).
func plantNetNeed(t *testing.T, path, self string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := regexp.MustCompile(`^depinfo_([0-9]+)_service='([^']*)'$`)
	ineed := regexp.MustCompile(`^depinfo_([0-9]+)_ineed_([0-9]+)=`)
	selfIdx, maxIdx, next := -1, -1, map[int]int{}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\n")
		if m := svc.FindStringSubmatch(line); m != nil {
			i, _ := strconv.Atoi(m[1])
			maxIdx = max(maxIdx, i)
			if m[2] == self {
				selfIdx = i
			}
			if m[2] == "net" {
				t.Fatalf("%s already has a net entry", path)
			}
		}
		if m := ineed.FindStringSubmatch(line); m != nil {
			i, _ := strconv.Atoi(m[1])
			j, _ := strconv.Atoi(m[2])
			next[i] = max(next[i], j+1)
		}
	}
	if selfIdx < 0 {
		t.Fatalf("%s has no %s entry", path, self)
	}
	add := fmt.Sprintf("depinfo_%d_ineed_%d='net'\ndepinfo_%d_service='net'\ndepinfo_%d_providedby_0='networking'\n",
		selfIdx, next[selfIdx], maxIdx+1, maxIdx+1)
	writeFile(t, path, string(b)+add)
}

// TC-SV-77: The protected set resolves the self unit, its dependencies and the OpenRC net provider. Owner WI 4.1. Requirements: FR-9, A-39.
func TestTCSV77_TheProtectedSetResolvesTheSelfUnit(t *testing.T) {
	ctx := context.Background()
	t.Run("systemd", func(t *testing.T) {
		const capture = "t1-ubuntu-systemd"
		root := testRoot(t, capture)
		cg := filepath.Join(root, "proc", strconv.Itoa(os.Getpid()), "cgroup")
		b, err := os.ReadFile(cg)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, cg, strings.Replace(string(b), "/nexusd.service", "/nexus-agent.service", 1))
		fh := svcctltest.NewHost(t, capture)
		// deps-nexus-agent.txt: the captured deps-nexusd.txt with
		// Requires=nexusd-db.service and PartOf=nx-part.service added.
		deps, err := svcctltest.CaptureOutput(capture, "deps-nexusd.txt")
		if err != nil {
			t.Fatal(err)
		}
		out := strings.Replace(string(deps.Output), "Requires=", "Requires=nexusd-db.service ", 1)
		out = strings.Replace(out, "PartOf=", "PartOf=nx-part.service", 1)
		fh.Answer([]string{systemctlTool, "show", "-p", "Requires", "-p", "BindsTo", "-p", "Requisite", "-p", "PartOf",
			"--", "nexus-agent.service"}, svcctl.Result{Output: []byte(out)}, nil)
		names := []string{"nexusd-db", "nx-part", "nx-svc-test", "nx-netalias"}
		answerLoadStates(t, fh, capture, map[string]string{"nx-netalias": "systemd-networkd.service"}, names...)
		p := newOn(t, root, fh)
		configure(t, p, section(t, "enabled: true\ncontrol:\n"+
			"  - name: nexusd-db\n  - name: nx-part\n  - name: nx-svc-test\n  - name: nx-netalias\n"))
		st := p.Preflight(ctx)
		wantAvailable(t, st)
		if got := detail[string](t, st, DetailSelfService); got != "nexus-agent" {
			t.Errorf("selfService = %q, want nexus-agent", got)
		}
		for name, why := range map[string]string{
			"nexus-agent": "running nexusd", "nexusd": "nexusd", "nexusd-db": "Requires=", "nx-part": "PartOf=",
		} {
			ok, reason := p.protected(name, "")
			if !ok || !strings.Contains(reason, why) {
				t.Errorf("protected(%s) = (%v, %q), want true naming %q", name, ok, reason, why)
			}
		}
		if ok, reason := p.protected("nx-svc-test", ""); ok {
			t.Errorf("nx-svc-test is protected (%q)", reason)
		}
		// An alias of systemd-networkd is protected by its canonical Id.
		if ok, reason := p.protected("nx-netalias", "systemd-networkd"); !ok || !strings.Contains(reason, "systemd-networkd") {
			t.Errorf("protected(nx-netalias, systemd-networkd) = (%v, %q)", ok, reason)
		}
		for name, why := range map[string]string{"nexusd-db": "Requires=", "nx-part": "PartOf=", "nx-netalias": "systemd-networkd"} {
			e := allowEntry(t, st, name)
			if e.Status != StatusProtected || !strings.Contains(e.Reason, why) {
				t.Errorf("controlAllowList %s = %+v, want protected naming %q", name, e, why)
			}
		}
		if e := allowEntry(t, st, "nx-svc-test"); e.Status != StatusOK {
			t.Errorf("controlAllowList nx-svc-test = %+v, want ok", e)
		}
		if got := detail[string](t, st, DetailDependencyResolution); got != ResolutionComplete {
			t.Errorf("dependencyResolution = %q, want complete", got)
		}
		snap := p.snapshot()
		if snap.controllable("nexusd-db", "") || !snap.controllable("nx-svc-test", "") {
			t.Error("controllable does not follow the protected set")
		}
	})
	t.Run("openrc", func(t *testing.T) {
		const capture = "t1-alpine-openrc"
		root := testRoot(t, capture)
		plantNetNeed(t, filepath.Join(root, "run", "openrc", "deptree"), "nexusd")
		fh := svcctltest.NewHost(t, capture)
		p := newOn(t, root, fh)
		// networking is statically protected, so config.Parse refuses it:
		// the section is decoded strictly without validate-config.
		configure(t, p, strictSection("enabled: true\ncontrol:\n  - name: networking\n  - name: nx-svc-test\n"))
		st := p.Preflight(ctx)
		wantAvailable(t, st)
		if ok, reason := p.protected("networking", ""); !ok || reason != "nexusd needs net" {
			t.Errorf("protected(networking) = (%v, %q), want (true, \"nexusd needs net\")", ok, reason)
		}
		if got := detail[string](t, st, DetailDependencyResolution); got != ResolutionComplete {
			t.Errorf("dependencyResolution = %q (%v), want complete", got, st.Details[DetailDependencyResolutionReason])
		}
		if e := allowEntry(t, st, "networking"); e.Status != StatusProtected || e.Reason != "nexusd needs net" {
			t.Errorf("controlAllowList networking = %+v, want protected: nexusd needs net", e)
		}
		if e := allowEntry(t, st, "nx-svc-test"); e.Status != StatusOK {
			t.Errorf("controlAllowList nx-svc-test = %+v, want ok", e)
		}
		if ok, _ := p.protected("nx-svc-test", ""); ok {
			t.Error("nx-svc-test is protected")
		}
		if calls := fh.Calls(); len(calls) != 0 {
			t.Errorf("OpenRC preflight ran %v, want no exec", calls)
		}
	})
}

// TC-SV-78: Configure decodes strictly and a reload swaps the allow-list atomically. Owner WI 4.1. Requirements: FR-1, FR-8.
//
// The list handler is WI 4.2's; the read here is the settings snapshot
// that list reads controllable and protected from.
func TestTCSV78_ConfigureDecodesStrictlyAndAReloadSwaps(t *testing.T) {
	const capture = "t1-ubuntu-systemd"
	root := testRoot(t, capture)
	fh := svcctltest.NewHost(t, capture)
	p := newOn(t, root, fh)

	configure(t, p, section(t, "control:\n  - name: nx-svc-test\n"))
	if !p.snapshot().controllable("nx-svc-test", "") {
		t.Error("(1) nx-svc-test not controllable")
	}
	configure(t, p, section(t, "control: []\n"))
	if p.snapshot().controllable("nx-svc-test", "") {
		t.Error("(2) nx-svc-test still controllable")
	}
	configure(t, p, section(t, "control:\n  - name: nx-svc-test\n"))
	before := p.snapshot()
	err := p.Configure(strictSection("contorl: []\n"))
	if err == nil || !strings.Contains(err.Error(), "contorl") {
		t.Fatalf("(3) Configure(contorl) = %v, want an error naming contorl", err)
	}
	if p.snapshot() != before || !p.snapshot().controllable("nx-svc-test", "") {
		t.Error("(3) a refused section changed the settings")
	}

	// (4) The self unit, once resolved, is in config A but never
	// controllable; every reader sees one whole snapshot.
	answerLoadStates(t, fh, capture, nil, "nx-svc-test")
	wantAvailable(t, p.Preflight(context.Background()))
	a := strictSection("control:\n  - name: nx-svc-test\n  - name: nexusd\n  - name: nx-other\n")
	b := strictSection("control: []\n")
	configure(t, p, b)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan string, 64)
	for i := range 8 {
		wg.Go(func() {
			for j := range 500 {
				d := a
				if (i+j)%2 == 1 {
					d = b
				}
				if err := p.Configure(d); err != nil {
					errs <- err.Error()
					return
				}
			}
		})
	}
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := p.snapshot()
				n := len(s.cfg.Control)
				if n != 0 && n != 3 {
					errs <- fmt.Sprintf("a snapshot with %d entries", n)
					return
				}
				for _, e := range s.cfg.Control {
					prot, _ := s.prot.protected(e.Name, "")
					if s.controllable(e.Name, "") == prot {
						errs <- fmt.Sprintf("%s: controllable and protected disagree in one snapshot", e.Name)
						return
					}
				}
				if n == 3 && s.controllable("nexusd", "") {
					errs <- "nexusd controllable"
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for e := range errs {
		t.Error("(4) " + e)
	}
}

// TC-SV-79: No code path names a hypervisor tool or API. Owner WI 4.1. Requirements: FR-12, CON-6, OOS-1.
func TestTCSV79_NoCodePathNamesAHypervisorTool(t *testing.T) {
	tools := regexp.MustCompile(`\b(pct|qm|pvesh|pveam|pvesm)\b`)
	detection := []string{"/etc/pve", "/var/lib/dpkg/info/pve-manager.list"}
	var seen []string
	files := 0
	for _, dir := range []string{".", "spec", filepath.Join("..", "..", "svcctl")} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			files++
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Errorf("%s: %s: %v", path, lit.Value, err)
					return true
				}
				if tools.MatchString(s) || strings.Contains(s, "/api2/") || strings.Contains(s, ":8006") {
					t.Errorf("%s: literal %q names a hypervisor tool or API", path, s)
				}
				if strings.Contains(strings.ToLower(s), "pve") {
					if !slices.Contains(detection, s) {
						t.Errorf("%s: Proxmox literal %q is not a detection path", path, s)
					}
					seen = append(seen, s)
				}
				return true
			})
		}
	}
	if files == 0 {
		t.Fatal("no source file scanned")
	}
	for _, d := range detection {
		if !slices.Contains(seen, d) {
			t.Errorf("detection path %q is not in the provider's code", d)
		}
	}
}

// TC-SV-E14: Preflight with an unreadable deptree stays available with the static list. Owner WI 4.1. Requirements: FR-3, FR-9, NFR-3.
func TestTCSVE14_PreflightWithAnUnreadableDeptreeStaysAvailable(t *testing.T) {
	const capture = "t1-alpine-openrc"
	root := testRoot(t, capture)
	if err := os.Remove(filepath.Join(root, "run", "openrc", "deptree")); err != nil {
		t.Fatal(err)
	}
	fh := svcctltest.NewHost(t, capture)
	p := newOn(t, root, fh)
	configure(t, p, section(t, "enabled: true\n"))
	st := p.Preflight(context.Background())
	wantAvailable(t, st)
	prot := detail[[]string](t, st, DetailProtected)
	for _, n := range spec.ProtectedOpenRC {
		if !slices.Contains(prot, n) {
			t.Errorf("protected = %q, missing %q", prot, n)
		}
	}
	if got := detail[string](t, st, DetailDependencyResolution); got != ResolutionPartial {
		t.Errorf("dependencyResolution = %q, want partial", got)
	}
	deps, err := svcctl.Dependencies(context.Background(), platform.OpenRC, "nexusd",
		svcctl.Options{RunRoot: filepath.Join(root, "run"), EtcRoot: filepath.Join(root, "etc")})
	if err != nil || !deps.Partial {
		t.Fatalf("Dependencies = (%+v, %v), want partial", deps, err)
	}
	if got := detail[string](t, st, DetailDependencyResolutionReason); got != deps.Reason {
		t.Errorf("dependencyResolutionReason = %q, want Deps.Reason %q", got, deps.Reason)
	}
	for _, n := range deps.Names {
		if !slices.Contains(prot, n) {
			t.Errorf("protected = %q, missing the dependency %q", prot, n)
		}
	}
	if ok, _ := p.protected("networking", ""); !ok {
		t.Error("networking is not protected")
	}
}

// TC-SV-ERR17: Preflight is unavailable, with a reason, for an unsupported init, missing tools and a Proxmox node. Owner WI 4.1. Requirements: FR-3, FR-12, CON-6, A-34.
func TestTCSVERR17_PreflightIsUnavailableWithAReasonFor(t *testing.T) {
	const proxmox = "Proxmox VE node: the services provider never manages the hypervisor"
	cases := []struct {
		name, capture string
		init          platform.InitSystem // "" = the capture's
		edit          func(t *testing.T, root string)
		reason        string
		exact         bool
	}{
		{name: "unknown init", capture: "t1-ubuntu-systemd", init: platform.Unknown, reason: "unknown"},
		{name: "no systemctl", capture: "t1-ubuntu-systemd", reason: "/usr/bin/systemctl", edit: func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "usr", "bin", "systemctl")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no rc-update", capture: "t1-alpine-openrc", reason: "/sbin/rc-update", edit: func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "sbin", "rc-update")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "etc/pve", capture: "t1-ubuntu-systemd", reason: proxmox, exact: true, edit: func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "etc", "pve"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "pve-manager.list", capture: "t1-ubuntu-systemd", reason: proxmox, exact: true, edit: func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "var", "lib", "dpkg", "info", "pve-manager.list"), "/usr/bin/pvesh\n")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := testRoot(t, c.capture)
			if c.edit != nil {
				c.edit(t, root)
			}
			fh := svcctltest.NewHost(t, c.capture)
			init := fh.Init()
			if c.init != "" {
				init = c.init
			}
			plat := platform.Info{Init: init, OS: platform.OS{ID: "linux"}}
			p := New(Options{GOOS: "linux", Platform: &plat, Run: fh.Run, ProcRoot: filepath.Join(root, "proc"),
				RunRoot: filepath.Join(root, "run"), EtcRoot: filepath.Join(root, "etc"), HostFS: os.DirFS(root)})
			st := p.Preflight(context.Background())
			wantUnavailable(t, st, c.reason)
			if c.exact && st.Reason != c.reason {
				t.Errorf("reason = %q, want exactly %q", st.Reason, c.reason)
			}
			if calls := fh.Calls(); len(calls) != 0 {
				t.Errorf("F-HOSTS calls = %v, want none", calls)
			}
		})
	}
}

// WI 4.1, "no self match": when neither the cgroup nor a child_pid names
// the service running nexusd, preflight stays available, reports
// selfService null with the reason, resolves no dependency (no exec), and
// protects only the static list and nexusd (D-6).
func TestServicesPreflightWithoutSelfServiceProtectsTheStaticList(t *testing.T) {
	ctx := context.Background()
	for capture, init := range captureNames(t) {
		t.Run(capture, func(t *testing.T) {
			root := testRoot(t, capture)
			if init == platform.Systemd {
				writeFile(t, filepath.Join(root, "proc", strconv.Itoa(os.Getpid()), "cgroup"), "0::/init.scope\n")
			} else {
				writeFile(t, filepath.Join(root, "run", "openrc", "options", "nexusd", "child_pid"), "1\n")
			}
			fh := svcctltest.NewHost(t, capture)
			p := newOn(t, root, fh)
			configure(t, p, section(t, "enabled: true\n"))
			st := p.Preflight(ctx)
			wantAvailable(t, st)
			if v, ok := st.Details[DetailSelfService]; !ok || v != nil {
				t.Errorf("selfService = %#v (present %v), want null", v, ok)
			}
			if r := detail[string](t, st, DetailSelfServiceReason); !strings.Contains(r, svcctl.ErrNoSelfService.Error()) {
				t.Errorf("selfServiceReason = %q, want ErrNoSelfService's text", r)
			}
			if got := detail[string](t, st, DetailDependencyResolution); got != ResolutionSkipped {
				t.Errorf("dependencyResolution = %q, want skipped", got)
			}
			static := map[platform.InitSystem][]string{platform.Systemd: spec.ProtectedSystemd, platform.OpenRC: spec.ProtectedOpenRC}[init]
			want := append([]string{"nexusd"}, static...)
			got := detail[[]string](t, st, DetailProtected)
			slices.Sort(want)
			want = slices.Compact(want)
			sorted := slices.Sorted(slices.Values(got))
			if !slices.Equal(sorted, want) {
				t.Errorf("protected = %q, want exactly %q", sorted, want)
			}
			if calls := fh.Calls(); len(calls) != 0 {
				t.Errorf("F-HOSTS calls = %v, want none without an allow-list or a self service", calls)
			}
		})
	}
}
