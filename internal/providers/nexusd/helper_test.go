package nexusd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeInput writes dir/input.json.
func writeInput(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, inputFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dirNames lists a directory's entries.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// noopEnv is a helper environment over f with the test-only mode
// "noop-test", which logs two steps and reports a version.
func noopEnv(f *fakeSystem, systems *int) helperEnv {
	return helperEnv{
		newSystem: func(helperInput) system {
			*systems++
			return f
		},
		mode: func(name string) (helperMode, bool) {
			if name == "noop-test" {
				return func(_ context.Context, h *helperRun, _ helperInput) helperResult {
					step(h.out, "first step")
					step(h.out, "second step")
					return helperResult{Changed: true, Version: "v9.9.2"}
				}, true
			}
			return modeOf(name)
		},
	}
}

// TC-SM-60 (WI 4.1; FR-8, FR-9, FR-7): The helper framework reads input.json, writes result.json atomically and logs timestamped steps
func TestTCSM60_TheHelperFrameworkReadsInputJson(t *testing.T) {
	f := newFakeSystem(t, 200)
	systems := 0
	dir := t.TempDir()
	writeInput(t, dir, `{"mode":"noop-test"}`)
	var stdout, stderr bytes.Buffer
	if code := runHelper([]string{"--dir", dir}, &stdout, &stderr, noopEnv(f, &systems)); code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, stderr.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, resultFileName))
	if err != nil {
		t.Fatal(err)
	}
	var res helperResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("result.json is not complete JSON: %v: %s", err, b)
	}
	if !res.Changed || res.Version != "v9.9.2" {
		t.Errorf("result = %+v, want the mode's result", res)
	}
	fi, err := os.Stat(filepath.Join(dir, resultFileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("result.json mode = %v, want 0600", fi.Mode().Perm())
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{inputFileName, resultFileName}) {
		t.Errorf("dir holds %v, want only input.json and result.json (no temp file)", names)
	}
	lines := 0
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		lines++
		ts, _, _ := strings.Cut(sc.Text(), " ")
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("step line %q does not start with an RFC 3339 timestamp: %v", sc.Text(), err)
		}
	}
	if lines < 2 {
		t.Errorf("stdout has %d step lines, want at least the mode's 2:\n%s", lines, stdout.String())
	}

	// Step 4: a fault after the temp write and before the rename.
	dir = t.TempDir()
	writeInput(t, dir, `{"mode":"noop-test"}`)
	env := noopEnv(f, &systems)
	sawTemp := false
	env.beforeRename = func(tmp string) error {
		_, err := os.Stat(tmp)
		sawTemp = err == nil
		return errors.New("injected fault before the rename")
	}
	stderr.Reset()
	if code := runHelper([]string{"--dir", dir}, &stdout, &stderr, env); code == 0 {
		t.Error("exit 0 although result.json could not be written")
	}
	if !sawTemp {
		t.Error("the fault hook ran without a written temp file")
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{inputFileName}) {
		t.Errorf("after the fault the dir holds %v, want only input.json (no result.json, no temp file)", names)
	}
	if !strings.Contains(stderr.String(), "injected fault") {
		t.Errorf("stderr %q does not report the write failure", stderr.String())
	}
}

// TC-SM-ERR31 (WI 4.1; FR-8, CON-2): The helper refuses a missing input, an existing result and a bad --dir
func TestTCSMERR31_TheHelperRefusesAMissingInput(t *testing.T) {
	f := newFakeSystem(t, 200)
	systems := 0
	env := noopEnv(f, &systems)

	noInput := t.TempDir()
	corrupt := t.TempDir()
	writeInput(t, corrupt, `{"mode":`)
	existing := t.TempDir()
	writeInput(t, existing, `{"mode":"noop-test"}`)
	prior := []byte(`{"changed":false,"rolledBack":false}`)
	if err := os.WriteFile(filepath.Join(existing, resultFileName), prior, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		dir  string
		want string
	}{
		{"no --dir", nil, "", "--dir"},
		{"relative --dir", []string{"--dir", "ops/x"}, "", "absolute"},
		{"extra argument", []string{"--dir", noInput, "more"}, noInput, "usage"},
		{"no input.json", []string{"--dir", noInput}, noInput, "input.json"},
		{"corrupt input.json", []string{"--dir", corrupt}, corrupt, "decode input.json"},
		{"existing result.json", []string{"--dir", existing}, existing, "result.json already exists"},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		if code := runHelper(c.args, &stdout, &stderr, env); code != 1 {
			t.Errorf("%s: exit %d, want 1", c.name, code)
		}
		if !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%s: stderr %q, want it to contain %q", c.name, stderr.String(), c.want)
		}
		if c.dir != "" && c.dir != existing {
			if _, err := os.Stat(filepath.Join(c.dir, resultFileName)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: result.json exists (%v), want none", c.name, err)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(existing, resultFileName)); err != nil || !bytes.Equal(b, prior) {
		t.Errorf("the existing result.json changed: %q, %v", b, err)
	}
	if systems != 0 || len(f.calls) != 0 {
		t.Errorf("F-SYS: %d systems built, calls %v; want none", systems, f.calls)
	}
}

// TC-SM-62 (WI 4.1; FR-7, CON-2, NFR-6): The real system implementation runs install.sh with a fixed argv, env and cwd, and parses systemctl and -version output
func TestTCSM62_TheRealSystemImplementationRunsInstall(t *testing.T) {
	ctx := context.Background()
	sys := newRealSystem("nexusd.service")

	// Step 1: install.sh's argv, env, cwd and streamed output.
	dir := t.TempDir()
	script := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(script, []byte("echo \"ARGV $0 $# $(pwd)\"\nenv | sort\n"+
		"for i in 1 2 3 4 5; do echo \"stderr line $i\" >&2; done\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	code, err := sys.RunInstall(ctx, script, dir, &buf)
	if err != nil || code != 0 {
		t.Fatalf("RunInstall = %d, %v\n%s", code, err, buf.String())
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if want := "ARGV " + script + " 0 " + realDir + "\n"; !strings.HasPrefix(out, want) {
		t.Errorf("first line of %q, want %q", out, want)
	}
	env := map[string]string{}
	stderrLines := 0
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "stderr line ") {
			stderrLines++
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "ARGV ") {
			env[k] = v
		}
	}
	if stderrLines != 5 {
		t.Errorf("stderr lines streamed = %d, want 5:\n%s", stderrLines, out)
	}
	if env["LC_ALL"] != "C" || env["PATH"] != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Errorf("LC_ALL=%q PATH=%q, want C and the fixed PATH", env["LC_ALL"], env["PATH"])
	}
	for k := range env {
		switch k {
		case "LC_ALL", "PATH", "PWD", "SHLVL", "_":
		default:
			t.Errorf("unexpected variable %s=%s in install.sh's environment", k, env[k])
		}
	}

	// ... and the timeout kills a script that sleeps.
	sleeper := filepath.Join(dir, "sleep.sh")
	if err := os.WriteFile(sleeper, []byte("echo sleeping\nsleep 10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	buf.Reset()
	_, err = sys.RunInstall(tctx, sleeper, dir, &buf)
	took := time.Since(began)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("sleeping install.sh: err %v, want a timeout error", err)
	}
	if took > 2*time.Second {
		t.Errorf("sleeping install.sh took %v, want it killed at about 300ms", took)
	}

	// Step 2: each captured systemctl show fixture (real systemd 259
	// captures from T1, TC-SM-129; testdata/systemctl-show/README).
	want := map[string]unitState{
		"active.txt":             {ActiveState: "active", SubState: "running", Result: "success", MainPID: 17647, NRestarts: 0},
		"activating.txt":         {ActiveState: "activating", SubState: "start", Result: "success", MainPID: 17689, NRestarts: 0},
		"failed-start-limit.txt": {ActiveState: "failed", SubState: "failed", Result: "exit-code", MainPID: 0, NRestarts: 5},
		"auto-restart.txt":       {ActiveState: "activating", SubState: "auto-restart", Result: "exit-code", MainPID: 0, NRestarts: 0},
	}
	for name, w := range want {
		b, err := os.ReadFile(filepath.Join("testdata/systemctl-show", name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseShow(b)
		if err != nil || got != w {
			t.Errorf("parseShow(%s) = %+v, %v; want %+v", name, got, err, w)
		}
	}
	if _, err := parseShow([]byte("ActiveState=active\nSubState=running\nResult=success\nMainPID=7\n")); err == nil ||
		!strings.Contains(err.Error(), "NRestarts") {
		t.Errorf("parseShow without NRestarts: %v, want an error naming NRestarts (A-18)", err)
	}
	if _, err := parseShow([]byte("ActiveState=active\nSubState=running\nResult=success\nMainPID=x\nNRestarts=0\n")); err == nil {
		t.Error("parseShow with a non-numeric MainPID succeeded")
	}

	// Show and Restart through a systemctl stand-in: the exact argv.
	argvFile := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "systemctl")
	fixture, err := filepath.Abs("testdata/systemctl-show/active.txt")
	if err != nil {
		t.Fatal(err)
	}
	writeScript(t, stub, "echo \"$@\" >> "+argvFile+"\n[ \"$1\" = show ] && cat "+fixture+"\nexit 0")
	stubSys := newRealSystem("nexusd.service")
	stubSys.systemctl = stub
	st, err := stubSys.Show(ctx)
	if err != nil || st != want["active.txt"] {
		t.Errorf("Show through the stand-in = %+v, %v", st, err)
	}
	for _, call := range []func(context.Context) error{stubSys.Restart, stubSys.DaemonReload, stubSys.ResetFailed} {
		if err := call(ctx); err != nil {
			t.Error(err)
		}
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := "show nexusd.service -p ActiveState -p SubState -p Result -p MainPID -p NRestarts\n" +
		"restart --no-block nexusd.service\ndaemon-reload\nreset-failed nexusd.service\n"
	if string(argv) != wantArgv {
		t.Errorf("systemctl argv:\n%s\nwant:\n%s", argv, wantArgv)
	}
	failing := filepath.Join(dir, "systemctl-fail")
	writeScript(t, failing, "echo 'Unit nexusd.service not found.' >&2\nexit 5")
	stubSys.systemctl = failing
	if err := stubSys.Restart(ctx); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a failing systemctl: %v, want an error with its stderr", err)
	}

	// Steps 3 and 4: -version of a stand-in.
	good := filepath.Join(dir, "nexusd-good")
	writeScript(t, good, `echo "nexusd v9.9.2"`)
	if v, err := sys.ExeVersion(ctx, good); err != nil || v != "v9.9.2" {
		t.Errorf("ExeVersion(stand-in) = %q, %v; want v9.9.2", v, err)
	}
	bad := filepath.Join(dir, "nexusd-garbage")
	writeScript(t, bad, `echo garbage`)
	if v, err := sys.ExeVersion(ctx, bad); err == nil {
		t.Errorf("ExeVersion(garbage) = %q, want a parse error", v)
	}
}

func TestCopyAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "sub", "dst")
	if err := os.Mkdir(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old image"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyAtomic(src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "new image" {
		t.Fatalf("dst = %q, %v", b, err)
	}
	after, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != 0o755 {
		t.Errorf("dst mode = %v, want 0755", after.Mode().Perm())
	}
	if os.SameFile(before, after) {
		t.Error("dst kept its inode: copyAtomic wrote in place instead of renaming")
	}
	if err := copyAtomic(filepath.Join(dir, "missing"), dst, 0o755); err == nil {
		t.Error("copyAtomic from a missing source succeeded")
	}
	if names := dirNames(t, filepath.Dir(dst)); !slices.Equal(names, []string{"dst"}) {
		t.Errorf("dst dir holds %v, want only dst (no temp file)", names)
	}
}
