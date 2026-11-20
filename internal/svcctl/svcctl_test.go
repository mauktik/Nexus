// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/nginx-provider/test-cases.md for WI 1.4. They are in the
// external test package because they use svcctltest, which imports svcctl.

package svcctl_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fixtureExit is the exit code testdata/README.md records for name.
func fixtureExit(t *testing.T, name string) int {
	t.Helper()
	rows, err := svcctltest.Provenance()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.File == name {
			code, err := strconv.Atoi(r.Exit)
			if err != nil {
				t.Fatalf("README row %s: exit %q", name, r.Exit)
			}
			return code
		}
	}
	t.Fatalf("README has no row for %s", name)
	return 0
}

// prop is the value of key=... in systemctl show output.
func prop(t *testing.T, out, key string) string {
	t.Helper()
	for line := range strings.Lines(out) {
		if v, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), key+"="); ok {
			return v
		}
	}
	t.Fatalf("no %s= in %q", key, out)
	return ""
}

// withLine replaces the key= line of out with key=value, or drops it when
// value is "-".
func withLine(t *testing.T, out, key, value string) string {
	t.Helper()
	var b strings.Builder
	found := false
	for line := range strings.Lines(out) {
		if strings.HasPrefix(line, key+"=") {
			found = true
			if value != "-" {
				b.WriteString(key + "=" + value + "\n")
			}
			continue
		}
		b.WriteString(line)
	}
	if !found {
		t.Fatalf("no %s= line to edit", key)
	}
	return b.String()
}

func newController(t *testing.T, init platform.InitSystem, opts svcctl.Options) svcctl.Controller {
	t.Helper()
	c, err := svcctl.New(init, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func argvs(calls []svcctltest.Call) [][]string {
	out := make([][]string, len(calls))
	for i, c := range calls {
		out[i] = c.Argv
	}
	return out
}

var wantShowArgv = []string{"/usr/bin/systemctl", "show", "--timestamp=unix", "-p", "LoadState", "-p", "ActiveState",
	"-p", "SubState", "-p", "MainPID", "-p", "StateChangeTimestamp", "--", "nginx.service"}

// nginx-provider TC-7: systemd Status maps captured `systemctl show` output.
func TestSystemdStatusFromCapturedShow(t *testing.T) {
	running := readFixture(t, "systemd/show-running.txt")
	runPID, err := strconv.Atoi(prop(t, running, "MainPID"))
	if err != nil || runPID <= 0 {
		t.Fatalf("running capture MainPID %q", prop(t, running, "MainPID"))
	}
	runSec, err := strconv.ParseInt(strings.TrimPrefix(prop(t, running, "StateChangeTimestamp"), "@"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	runSince := time.Unix(runSec, 0)

	cases := []struct {
		name      string
		out       string
		known     bool
		state     svcctl.State
		mainPID   int
		since     *time.Time
		active    string
		substate  string
		checkTime bool
	}{
		{name: "running", out: running, known: true, state: svcctl.Running, mainPID: runPID, since: &runSince, checkTime: true},
		{name: "stopped", out: readFixture(t, "systemd/show-stopped.txt"), known: true, state: svcctl.Stopped},
		{name: "failed", out: readFixture(t, "systemd/show-failed.txt"), known: true, state: svcctl.Failed},
		{name: "not-found", out: readFixture(t, "systemd/show-not-found.txt"), known: false, state: svcctl.Stopped},
		{name: "reloading", out: withLine(t, running, "ActiveState", "reloading"), known: true, state: svcctl.Running, mainPID: runPID},
		{name: "activating", out: withLine(t, running, "ActiveState", "activating"), known: true, state: svcctl.Starting, mainPID: runPID},
		{name: "deactivating", out: withLine(t, running, "ActiveState", "deactivating"), known: true, state: svcctl.Stopping, mainPID: runPID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := svcctltest.New(t, platform.Systemd, svcctltest.Model{})
			fake.SetStatus(tc.out, 0)
			st, err := newController(t, platform.Systemd, svcctl.Options{Run: fake.Run}).Status(t.Context(), "nginx")
			if err != nil {
				t.Fatal(err)
			}
			if st.Known != tc.known || st.State != tc.state {
				t.Errorf("Known, State = %v, %q; want %v, %q", st.Known, st.State, tc.known, tc.state)
			}
			if tc.state != svcctl.Running && tc.state != svcctl.Starting && tc.state != svcctl.Stopping && st.MainPID != 0 {
				t.Errorf("MainPID = %d, want 0", st.MainPID)
			}
			if tc.mainPID != 0 && st.MainPID != tc.mainPID {
				t.Errorf("MainPID = %d, want %d", st.MainPID, tc.mainPID)
			}
			if tc.checkTime && (st.Since == nil || !st.Since.Equal(*tc.since)) {
				t.Errorf("Since = %v, want %v", st.Since, tc.since)
			}
			for _, w := range []string{prop(t, tc.out, "ActiveState"), prop(t, tc.out, "SubState")} {
				if !strings.Contains(st.Raw, w) {
					t.Errorf("Raw %q does not contain %q", st.Raw, w)
				}
			}
			if got := argvs(fake.Calls()); len(got) != 1 || !slices.Equal(got[0], wantShowArgv) {
				t.Errorf("calls = %q, want exactly %q", got, wantShowArgv)
			}
		})
	}

	// The model's own answer is the same capture.
	fake := svcctltest.New(t, platform.Systemd, svcctltest.Model{State: svcctl.Running})
	st, err := newController(t, platform.Systemd, svcctl.Options{Run: fake.Run}).Status(t.Context(), "nginx")
	if err != nil || st.State != svcctl.Running || st.MainPID != runPID {
		t.Errorf("model running: %+v, %v", st, err)
	}
}

// startChild starts a process that lives until the test ends and returns
// its PID.
func startChild(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// procRoot is the real /proc on Linux, or a fake one holding <pid>/ for
// each of pids elsewhere.
func procRoot(t *testing.T, pids ...int) string {
	t.Helper()
	if runtime.GOOS == "linux" {
		return "/proc"
	}
	dir := t.TempDir()
	for _, pid := range pids {
		if err := os.Mkdir(filepath.Join(dir, strconv.Itoa(pid)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// nginx-provider TC-8: OpenRC Status maps captured rc-service output and
// reads MainPID from the daemon record.
func TestOpenRCStatusFromCapturedOutput(t *testing.T) {
	pid := startChild(t)
	runRoot := t.TempDir()
	pidfile := filepath.Join(t.TempDir(), "nginx.pid")
	if err := svcctltest.WriteDaemonRecord(runRoot, "nginx", pidfile, pid); err != nil {
		t.Fatal(err)
	}
	rec, err := os.ReadFile(filepath.Join(runRoot, "openrc", "daemons", "nginx", "001"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(readFixture(t, "openrc/daemon-record"), "pidfile=/run/nginx/nginx.pid", "pidfile="+pidfile, 1)
	if string(rec) != want {
		t.Fatalf("daemon record %q is not the capture with the pidfile replaced: %q", rec, want)
	}
	proc := procRoot(t, pid)

	cases := []struct {
		name, out string
		exit      int
		state     svcctl.State
		mainPID   int
	}{
		{"started", readFixture(t, "openrc/status-started.txt"), fixtureExit(t, "openrc/status-started.txt"), svcctl.Running, pid},
		{"stopped", readFixture(t, "openrc/status-stopped.txt"), fixtureExit(t, "openrc/status-stopped.txt"), svcctl.Stopped, 0},
		{"crashed", readFixture(t, "openrc/status-crashed.txt"), fixtureExit(t, "openrc/status-crashed.txt"), svcctl.Failed, 0},
		// Synthetic: one word of the started capture changed.
		{"starting", strings.Replace(readFixture(t, "openrc/status-started.txt"), "started", "starting", 1), 0, svcctl.Starting, 0},
		{"stopping", strings.Replace(readFixture(t, "openrc/status-started.txt"), "started", "stopping", 1), 0, svcctl.Stopping, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := svcctltest.New(t, platform.OpenRC, svcctltest.Model{State: svcctl.Running})
			fake.SetStatus(tc.out, tc.exit)
			c := newController(t, platform.OpenRC, svcctl.Options{Run: fake.Run, RunRoot: runRoot, ProcRoot: proc})
			st, err := c.Status(t.Context(), "nginx")
			if err != nil {
				t.Fatal(err)
			}
			if !st.Known || st.State != tc.state || st.MainPID != tc.mainPID || st.Since != nil {
				t.Errorf("Status = %+v; want Known, State %q, MainPID %d, Since nil", st, tc.state, tc.mainPID)
			}
			want := [][]string{{"/sbin/rc-service", "--exists", "nginx"}, {"/sbin/rc-service", "-C", "nginx", "status"}}
			if got := argvs(fake.Calls()); !slices.EqualFunc(got, want, slices.Equal) {
				t.Errorf("calls = %q, want %q", got, want)
			}
		})
	}

	t.Run("absent", func(t *testing.T) {
		fake := svcctltest.New(t, platform.OpenRC, svcctltest.Model{Absent: true})
		st, err := newController(t, platform.OpenRC, svcctl.Options{Run: fake.Run, RunRoot: runRoot, ProcRoot: proc}).
			Status(t.Context(), "nginx")
		if err != nil || st.Known || st.State == svcctl.Running || st.State == svcctl.Stopped {
			t.Errorf("Status = %+v, %v; want not Known and no running/stopped claim", st, err)
		}
		if got := argvs(fake.Calls()); len(got) != 1 || !slices.Equal(got[0], []string{"/sbin/rc-service", "--exists", "nginx"}) {
			t.Errorf("calls = %q", got)
		}
	})
}

// nginx-provider TC-9: Do invokes exactly the absolute binary with the
// action, and never a shell.
func TestDoArgvIsExact(t *testing.T) {
	actions := []svcctl.Action{svcctl.Start, svcctl.Stop, svcctl.Restart, svcctl.Reload}
	for _, init := range []platform.InitSystem{platform.Systemd, platform.OpenRC} {
		for _, a := range actions {
			t.Run(string(init)+"/"+string(a), func(t *testing.T) {
				fake := svcctltest.New(t, init, svcctltest.Model{State: svcctl.Running})
				res, err := newController(t, init, svcctl.Options{Run: fake.Run}).Do(t.Context(), a, "nginx")
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"/usr/bin/systemctl", string(a), "--", "nginx.service"}
				if init == platform.OpenRC {
					want = []string{"/sbin/rc-service", "-C", "nginx", string(a)}
				}
				calls := fake.Calls()
				if len(calls) != 1 || !slices.Equal(calls[0].Argv, want) {
					t.Fatalf("calls = %q, want exactly %q", argvs(calls), want)
				}
				for _, el := range calls[0].Argv {
					if strings.ContainsAny(el, " \t\n") || el == "sh" || el == "-c" || strings.HasSuffix(el, "/sh") {
						t.Errorf("argv element %q looks like a shell or a joined command", el)
					}
				}
				if res.ExitCode != calls[0].Result.ExitCode || string(res.Output) != string(calls[0].Result.Output) {
					t.Errorf("Do = %d %q, the fake returned %d %q", res.ExitCode, res.Output, calls[0].Result.ExitCode, calls[0].Result.Output)
				}
			})
		}
		t.Run(string(init)+"/bad-action", func(t *testing.T) {
			fake := svcctltest.New(t, init, svcctltest.Model{State: svcctl.Running})
			c := newController(t, init, svcctl.Options{Run: fake.Run})
			for _, a := range []svcctl.Action{"", "enable", "status", "reload; reboot", "START"} {
				if _, err := c.Do(t.Context(), a, "nginx"); err == nil {
					t.Errorf("Do(%q) succeeded", a)
				}
			}
			if n := len(fake.Calls()); n != 0 {
				t.Errorf("the runner was called %d times", n)
			}
		})
	}
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// nginx-provider TC-10: The real runner uses a fixed environment, caps
// output, and is bounded.
func TestRealRunnerEnvCapAndTimeout(t *testing.T) {
	dir := t.TempDir()
	// awk, not sh: a shell adds PWD and SHLVL to the environment it was
	// given, and the point is what svcctl passes. Any stdin line would be
	// printed; a stdin that is not at EOF would hang until the timeout.
	envdump := writeScript(t, dir, "envdump.sh", "#!/usr/bin/awk -f\nBEGIN {\n"+
		"\twhile ((getline line < \"-\") > 0) print \"stdin: \" line\n"+
		"\tfor (k in ENVIRON) print k \"=\" ENVIRON[k]\n}\n")
	big := writeScript(t, dir, "big.sh", "#!/bin/sh\nhead -c 204800 /dev/zero | tr '\\0' x\n")
	sleepPID := filepath.Join(dir, "sleep.pid")
	// The pid is written to a temp file and renamed, so the pidfile is
	// either absent or complete, never empty; the timeout below is long
	// enough that the script has started its sleep and renamed the file
	// before the deadline fires.
	hang := writeScript(t, dir, "hang.sh", "#!/bin/sh\nsleep 60 &\necho $! > "+sleepPID+".tmp\nmv "+sleepPID+".tmp "+sleepPID+"\nwait\n")

	t.Setenv("NEXUS_SVCCTL_LEAK", "must-not-pass")
	res, err := svcctl.RealCall(t.Context(), 10*time.Second, []string{envdump})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("envdump: %v, exit %d, %q", err, res.ExitCode, res.Output)
	}
	lines := strings.Split(strings.TrimRight(string(res.Output), "\n"), "\n")
	slices.Sort(lines)
	if want := []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}; !slices.Equal(lines, want) {
		t.Errorf("envdump printed %q, want exactly %q", lines, want)
	}

	res, err = svcctl.RealCall(t.Context(), 10*time.Second, []string{big})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("big: %v, exit %d", err, res.ExitCode)
	}
	if len(res.Output) != svcctl.OutputCap || svcctl.OutputCap != 65536 {
		t.Errorf("big: %d bytes of output, want exactly 65536", len(res.Output))
	}

	const hangTimeout = 2 * time.Second
	start := time.Now()
	res, err = svcctl.RealCall(t.Context(), hangTimeout, []string{hang})
	if el := time.Since(start); el > hangTimeout+2*time.Second {
		t.Errorf("hang returned after %s, want within %s of its timeout", el, 2*time.Second)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hang: err = %v (result %+v), want context.DeadlineExceeded", err, res)
	}
	b, err := os.ReadFile(sleepPID)
	if err != nil {
		t.Fatalf("hang.sh did not start its sleep within %s: %v", hangTimeout, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	// The orphaned sleep is reaped by init; until then it is a zombie that
	// kill(pid, 0) still finds.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if out, _ := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output(); strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("sleep (pid %d) is still running after the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := svcctl.RealCall(t.Context(), time.Second, []string{"sh", "-c", "true"}); err == nil {
		t.Error("a relative program was run")
	}
}

// tRecorder is a svcctltest.T that records failures instead of failing,
// so a test can observe that the fake fails its caller.
type tRecorder struct{ errs []string }

func (*tRecorder) Helper() {}
func (r *tRecorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// nginx-provider TC-11: The svcctl fake encodes real init-system
// constraints.
func TestFakeEncodesInitSystemConstraints(t *testing.T) {
	type step struct {
		config   svcctltest.Config
		exit     int
		output   string // A fixture whose text the output must equal
		advanced bool
	}
	type reloadCase struct {
		init  platform.InitSystem
		model svcctltest.ReloadModel
		steps []step
	}
	cases := map[string]reloadCase{
		// Captured: Ubuntu's ExecReload is `nginx -s reload`, which parses
		// the config first, so an invalid config exits 1. A bind conflict
		// is rejected by the master after parsing and still exits 0.
		"systemd/signal-parse": {platform.Systemd, svcctltest.SignalParse, []step{
			{svcctltest.Invalid, 1, "systemd/reload-invalid.txt", false},
			{svcctltest.BindConflict, 0, "systemd/reload-bind-conflict.txt", false},
			{svcctltest.Valid, 0, "systemd/reload.txt", true},
		}},
		// Worst case (L-26): nginx.org's ExecReload only sends HUP, so
		// systemctl exits 0 for an invalid config too, and the generation
		// does not advance.
		"systemd/hup": {platform.Systemd, svcctltest.HUP, []step{
			{svcctltest.Invalid, 0, "systemd/reload-hup-invalid.txt", false},
			{svcctltest.BindConflict, 0, "systemd/reload-hup-bind-conflict.txt", false},
			{svcctltest.Valid, 0, "systemd/reload-hup.txt", true},
		}},
		"openrc": {platform.OpenRC, "", []step{
			{svcctltest.Invalid, 1, "openrc/reload-invalid.txt", false},
			{svcctltest.BindConflict, 0, "openrc/reload-bind-conflict.txt", false},
			{svcctltest.Valid, 0, "openrc/reload.txt", true},
		}},
	}
	for name, rc := range cases {
		init, steps := rc.init, rc.steps
		t.Run(name+"/reload", func(t *testing.T) {
			fake := svcctltest.New(t, init, svcctltest.Model{State: svcctl.Running, Generation: 1, ReloadModel: rc.model})
			c := newController(t, init, svcctl.Options{Run: fake.Run})
			for _, s := range steps {
				fake.Set(func(m *svcctltest.Model) { m.Config = s.config })
				before := fake.Model().Generation
				res, err := c.Do(t.Context(), svcctl.Reload, "nginx")
				if err != nil {
					t.Fatal(err)
				}
				if res.ExitCode != s.exit || res.ExitCode != fixtureExit(t, s.output) {
					t.Errorf("%s: exit %d, want %d (README %d)", s.config, res.ExitCode, s.exit, fixtureExit(t, s.output))
				}
				if string(res.Output) != readFixture(t, s.output) {
					t.Errorf("%s: output %q, want the capture %s", s.config, res.Output, s.output)
				}
				m := fake.Model()
				if advanced := m.Generation != before; advanced != s.advanced {
					t.Errorf("%s: generation %d -> %d, advanced %v, want %v", s.config, before, m.Generation, advanced, s.advanced)
				}
				if m.State != svcctl.Running {
					t.Errorf("%s: state %q after reload, want running", s.config, m.State)
				}
			}
		})
	}
	t.Run("reload-model-defaults-to-signal-parse", func(t *testing.T) {
		fake := svcctltest.New(t, platform.Systemd, svcctltest.Model{})
		if got := fake.Model().ReloadModel; got != svcctltest.SignalParse {
			t.Errorf("default ReloadModel = %q, want %q", got, svcctltest.SignalParse)
		}
	})
	t.Run("openrc/reload-invalid-text", func(t *testing.T) {
		out := readFixture(t, "openrc/reload-invalid.txt")
		if !strings.Contains(out, "configuration file /etc/nginx/nginx.conf test failed") {
			t.Errorf("the OpenRC invalid reload capture has no configtest text: %q", out)
		}
	})

	t.Run("openrc/start-while-started", func(t *testing.T) {
		fake := svcctltest.New(t, platform.OpenRC, svcctltest.Model{State: svcctl.Running, Generation: 3})
		res, err := newController(t, platform.OpenRC, svcctl.Options{Run: fake.Run}).Do(t.Context(), svcctl.Start, "nginx")
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 0 || !strings.Contains(string(res.Output), "already been started") {
			t.Errorf("start while started: exit %d, %q", res.ExitCode, res.Output)
		}
		if m := fake.Model(); m.State != svcctl.Running || m.Generation != 3 {
			t.Errorf("start while started changed the model: %+v", m)
		}
	})

	t.Run("misuse-fails-the-caller", func(t *testing.T) {
		for _, argv := range [][]string{
			{"systemctl", "reload", "--", "nginx.service"},
			{"/usr/bin/env", "LD_PRELOAD=/tmp/x.so", "/usr/bin/systemctl", "reload", "--", "nginx.service"},
			{"/usr/bin/systemctl", "reload", "--", "nginx.service", "SYSTEMD_LOG_LEVEL=debug"},
		} {
			rec := &tRecorder{}
			fake := svcctltest.New(rec, platform.Systemd, svcctltest.Model{State: svcctl.Running})
			if _, err := fake.Run(t.Context(), argv); err == nil {
				t.Errorf("%q: the fake answered", argv)
			}
			if len(rec.errs) == 0 {
				t.Errorf("%q: the fake did not fail its test", argv)
			}
		}
		rec := &tRecorder{}
		fake := svcctltest.New(rec, platform.OpenRC, svcctltest.Model{})
		if _, err := fake.Run(t.Context(), []string{"rc-service", "-C", "nginx", "start"}); err == nil || len(rec.errs) == 0 {
			t.Errorf("relative rc-service: err %v, failures %q", err, rec.errs)
		}
	})
}

// nginx-provider TC-12: svcctl fixtures carry their real-host provenance.
func TestFixtureProvenance(t *testing.T) {
	rows, err := svcctltest.Provenance()
	if err != nil {
		t.Fatal(err)
	}
	byFile := map[string]svcctltest.ProvenanceRow{}
	for _, r := range rows {
		if _, dup := byFile[r.File]; dup {
			t.Errorf("two README rows for %s", r.File)
		}
		byFile[r.File] = r
	}
	var files []string
	err = filepath.WalkDir("testdata", func(path string, d fs.DirEntry, err error) error {
		// The services captures carry their provenance in each
		// directory's PROVENANCE instead (services-provider TC-SV-42).
		if err == nil && d.IsDir() && path == filepath.Join("testdata", "services") {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() || path == filepath.Join("testdata", "README.md") {
			return err
		}
		rel, err := filepath.Rel("testdata", path)
		files = append(files, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		r, ok := byFile[f]
		if !ok {
			t.Errorf("fixture %s has no README row", f)
			continue
		}
		for name, v := range map[string]string{"host": r.Host, "distribution": r.Distribution, "init": r.Init,
			"nginx": r.Nginx, "command": r.Command, "exit": r.Exit, "date": r.Date} {
			if v == "" {
				t.Errorf("%s: no %s", f, name)
			}
		}
		if !strings.ContainsAny(r.Distribution, "0123456789") || !strings.ContainsAny(r.Init, "0123456789") ||
			!strings.ContainsAny(r.Nginx, "0123456789") {
			t.Errorf("%s: distribution, init and nginx must carry versions: %+v", f, r)
		}
		if _, err := time.Parse(time.DateOnly, r.Date); err != nil {
			t.Errorf("%s: date %q", f, r.Date)
		}
		if r.Exit != "-" {
			if _, err := strconv.Atoi(r.Exit); err != nil {
				t.Errorf("%s: exit %q", f, r.Exit)
			}
		}
	}
	for _, r := range rows {
		if !slices.Contains(files, r.File) {
			t.Errorf("README row for a missing fixture %s", r.File)
		}
	}
	readme := readFixture(t, "README.md")
	if strings.Contains(strings.ToUpper(readme), "UNCONFIRMED") {
		t.Error("README says UNCONFIRMED")
	}
	var ubuntu, alpine bool
	for _, r := range rows {
		ubuntu = ubuntu || strings.Contains(r.Distribution, "Ubuntu")
		alpine = alpine || strings.Contains(r.Distribution, "Alpine")
	}
	if !ubuntu || !alpine {
		t.Errorf("rows for Ubuntu %v, Alpine %v; want both", ubuntu, alpine)
	}
}

// nginx-provider TC-E5: Unknown or unparseable systemd values never become a
// guess.
func TestSystemdUnknownValues(t *testing.T) {
	running := readFixture(t, "systemd/show-running.txt")
	status := func(t *testing.T, out string) svcctl.Status {
		t.Helper()
		fake := svcctltest.New(t, platform.Systemd, svcctltest.Model{})
		fake.SetStatus(out, 0)
		st, err := newController(t, platform.Systemd, svcctl.Options{Run: fake.Run}).Status(t.Context(), "nginx")
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := status(t, withLine(t, running, "ActiveState", "maintenance")); st.State != svcctl.Unknown {
		t.Errorf("maintenance: State %q, want unknown", st.State)
	}
	for _, v := range []string{"n/a", "", "@", "@abc", "1794584529"} {
		if st := status(t, withLine(t, running, "StateChangeTimestamp", v)); st.Since != nil {
			t.Errorf("StateChangeTimestamp=%q: Since %v, want nil", v, st.Since)
		}
	}
	if st := status(t, withLine(t, running, "MainPID", "0")); st.MainPID != 0 || st.State != svcctl.Running {
		t.Errorf("MainPID=0 while active: %+v", st)
	}
	out := withLine(t, running, "ActiveState", "-")
	if st := status(t, out); st.State != svcctl.Unknown || st.Raw != strings.TrimSpace(out) {
		t.Errorf("no ActiveState: State %q Raw %q; want unknown with the output", st.State, st.Raw)
	}
}

// nginx-provider TC-E6: OpenRC MainPID failures leave 0 and never error.
func TestOpenRCMainPIDFailures(t *testing.T) {
	live := startChild(t)
	record := func(t *testing.T, runRoot, body string) {
		t.Helper()
		d := filepath.Join(runRoot, "openrc", "daemons", "nginx")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "001"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	captured := readFixture(t, "openrc/daemon-record")
	cases := []struct {
		name  string
		setup func(t *testing.T, runRoot string) (procRoot string)
	}{
		{"no daemons dir", func(t *testing.T, _ string) string { return procRoot(t, live) }},
		{"record without pidfile", func(t *testing.T, runRoot string) string {
			var b strings.Builder
			for line := range strings.Lines(captured) {
				if !strings.HasPrefix(line, "pidfile=") {
					b.WriteString(line)
				}
			}
			record(t, runRoot, b.String())
			return procRoot(t, live)
		}},
		{"pidfile abc", func(t *testing.T, runRoot string) string {
			pf := filepath.Join(t.TempDir(), "nginx.pid")
			if err := os.WriteFile(pf, []byte("abc\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			record(t, runRoot, strings.Replace(captured, "/run/nginx/nginx.pid", pf, 1))
			return procRoot(t, live)
		}},
		{"pid without a /proc entry", func(t *testing.T, runRoot string) string {
			if err := svcctltest.WriteDaemonRecord(runRoot, "nginx", filepath.Join(t.TempDir(), "nginx.pid"), live); err != nil {
				t.Fatal(err)
			}
			return t.TempDir() // A /proc with no entries at all
		}},
		{"unreadable pidfile", func(t *testing.T, runRoot string) string {
			pf := filepath.Join(t.TempDir(), "nginx.pid")
			if err := svcctltest.WriteDaemonRecord(runRoot, "nginx", pf, live); err != nil {
				t.Fatal(err)
			}
			if os.Geteuid() == 0 {
				// Root reads a mode-0 file; a directory is unreadable as a
				// pidfile for everyone.
				if err := os.Remove(pf); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(pf, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(pf, 0); err != nil {
				t.Fatal(err)
			}
			return procRoot(t, live)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runRoot := t.TempDir()
			proc := tc.setup(t, runRoot)
			fake := svcctltest.New(t, platform.OpenRC, svcctltest.Model{State: svcctl.Running})
			c := newController(t, platform.OpenRC, svcctl.Options{Run: fake.Run, RunRoot: runRoot, ProcRoot: proc})
			st, err := c.Status(t.Context(), "nginx")
			if err != nil || st.State != svcctl.Running || st.MainPID != 0 {
				t.Errorf("Status = %+v, %v; want running, MainPID 0, no error", st, err)
			}
		})
	}
}

// nginx-provider TC-ERR1: svcctl refuses invalid service names and an
// unknown init system.
func TestRefusesBadServiceNamesAndUnknownInit(t *testing.T) {
	names := []string{"", "nginx;rm -rf /", "../nginx", "-nginx", "nginx service", strings.Repeat("n", 65), "nginx\n"}
	for _, init := range []platform.InitSystem{platform.Systemd, platform.OpenRC} {
		fake := svcctltest.New(t, init, svcctltest.Model{State: svcctl.Running})
		c := newController(t, init, svcctl.Options{Run: fake.Run})
		for _, name := range names {
			if _, err := c.Status(t.Context(), name); err == nil || !strings.Contains(err.Error(), svcctl.ServiceNamePattern) {
				t.Errorf("%s Status(%q): err %v, want one naming %s", init, name, err, svcctl.ServiceNamePattern)
			}
			if _, err := c.Do(t.Context(), svcctl.Reload, name); err == nil || !strings.Contains(err.Error(), svcctl.ServiceNamePattern) {
				t.Errorf("%s Do(%q): err %v, want one naming %s", init, name, err, svcctl.ServiceNamePattern)
			}
		}
		if n := len(fake.Calls()); n != 0 {
			t.Errorf("%s: the runner was called %d times", init, n)
		}
		// The longest valid name reaches the runner (the fake models nginx
		// only, so a bare recorder stands in).
		var got [][]string
		rec := newController(t, init, svcctl.Options{Run: func(_ context.Context, argv []string) (svcctl.Result, error) {
			got = append(got, argv)
			return svcctl.Result{}, nil
		}})
		if _, err := rec.Do(t.Context(), svcctl.Reload, strings.Repeat("n", 64)); err != nil || len(got) != 1 {
			t.Errorf("%s: a 64-character name was refused: %v, %d calls", init, err, len(got))
		}
	}
	if _, err := svcctl.New(platform.Unknown, svcctl.Options{}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("New(Unknown): err %v, want one containing unknown", err)
	}
}

// nginx-provider TC-ERR2: svcctl reports runner failures and timeouts as
// errors, never as a state.
func TestRunnerFailuresAreErrors(t *testing.T) {
	notClaimed := func(t *testing.T, st svcctl.Status) {
		t.Helper()
		if st.State == svcctl.Running || st.State == svcctl.Stopped {
			t.Errorf("a failed Status claimed %q", st.State)
		}
	}
	for _, init := range []platform.InitSystem{platform.Systemd, platform.OpenRC} {
		t.Run(string(init)+"/blocked", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := svcctltest.New(t, init, svcctltest.Model{State: svcctl.Running})
				fake.SetBlock(true)
				c := newController(t, init, svcctl.Options{Run: fake.Run, Timeout: 30 * time.Second})
				start := time.Now()
				st, err := c.Status(t.Context(), "nginx")
				if el := time.Since(start); el != 30*time.Second {
					t.Errorf("Status returned after %s of bubble time, want 30s", el)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Status err = %v, want context.DeadlineExceeded", err)
				}
				notClaimed(t, st)
				start = time.Now()
				if _, err := c.Do(t.Context(), svcctl.Reload, "nginx"); !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Do err = %v, want context.DeadlineExceeded", err)
				}
				if el := time.Since(start); el != 30*time.Second {
					t.Errorf("Do returned after %s of bubble time, want 30s", el)
				}
			})
		})
		t.Run(string(init)+"/exec-error", func(t *testing.T) {
			fake := svcctltest.New(t, init, svcctltest.Model{State: svcctl.Running})
			execErr := &fs.PathError{Op: "fork/exec", Path: "/usr/bin/systemctl", Err: syscall.ENOENT}
			fake.SetErr(execErr)
			c := newController(t, init, svcctl.Options{Run: fake.Run})
			st, err := c.Status(t.Context(), "nginx")
			if !errors.Is(err, syscall.ENOENT) || !errors.Is(err, execErr) {
				t.Errorf("Status err = %v, want the exec error wrapped", err)
			}
			notClaimed(t, st)
			if _, err := c.Do(t.Context(), svcctl.Reload, "nginx"); !errors.Is(err, syscall.ENOENT) {
				t.Errorf("Do err = %v, want the exec error wrapped", err)
			}
		})
	}
	t.Run("show-exits-non-zero", func(t *testing.T) {
		fake := svcctltest.New(t, platform.Systemd, svcctltest.Model{})
		fake.SetStatus("Failed to connect to bus: No such file or directory\n", 1)
		st, err := newController(t, platform.Systemd, svcctl.Options{Run: fake.Run}).Status(t.Context(), "nginx")
		if err == nil {
			t.Error("a failed systemctl show was not an error")
		}
		notClaimed(t, st)
	})
}

// TestLiveMatrix drives the real nginx of this host through svcctl with the
// real runner: start, reload, restart and stop, each read back with Status
// and, for MainPID, against nginx's own pidfile. It runs only with
// NEXUS_SVCCTL_LIVE=1, as root, on a host where nginx is installed and may be
// restarted (the WI 1.4 cross-platform matrix; see testdata/README.md).
func TestLiveMatrix(t *testing.T) {
	if os.Getenv("NEXUS_SVCCTL_LIVE") != "1" {
		t.Skip("set NEXUS_SVCCTL_LIVE=1 to drive this host's nginx")
	}
	p := platform.DetectAt(runtime.GOOS, "/")
	c, err := svcctl.New(p.Init, svcctl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pidfile := "/run/nginx.pid"
	if p.Init == platform.OpenRC {
		pidfile = "/run/nginx/nginx.pid"
	}
	ctx := t.Context()
	do := func(a svcctl.Action, want svcctl.State) {
		t.Helper()
		res, err := c.Do(ctx, a, "nginx")
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: exit %d, %v: %s", a, res.ExitCode, err, res.Output)
		}
		st, err := c.Status(ctx, "nginx")
		if err != nil {
			t.Fatalf("Status after %s: %v", a, err)
		}
		t.Logf("%s %s: exit %d, output %q; Status %+v", p.Init, a, res.ExitCode, res.Output, st)
		if !st.Known || st.State != want {
			t.Fatalf("after %s: Status %+v, want %s", a, st, want)
		}
		if want == svcctl.Running {
			b, err := os.ReadFile(pidfile)
			if err != nil {
				t.Fatal(err)
			}
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid == 0 || st.MainPID != pid {
				t.Errorf("after %s: MainPID %d, pidfile %s says %q", a, st.MainPID, pidfile, b)
			}
		} else if st.MainPID != 0 {
			t.Errorf("after %s: MainPID %d, want 0", a, st.MainPID)
		}
	}
	do(svcctl.Stop, svcctl.Stopped)
	do(svcctl.Start, svcctl.Running)
	do(svcctl.Reload, svcctl.Running)
	do(svcctl.Restart, svcctl.Running)
	do(svcctl.Stop, svcctl.Stopped)
}
