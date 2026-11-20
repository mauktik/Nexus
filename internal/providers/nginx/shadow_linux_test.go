// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nginx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/problem"
)

// The shadow tests mount, so they need root on Linux (T0-root). The
// harnesses run them as root; elsewhere they skip, saying why.
func requireShadowRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: the shadow test makes a mount namespace and mounts in it")
	}
}

// shadowFixture is an includeDir, an operation directory, fakenginx over
// that includeDir and a shadow tester that runs a built nexusd.
type shadowFixture struct {
	root, inc, opDir, nginx string
	log                     *syncBuffer
	tester                  shadowBin
}

func newShadowFixture(t *testing.T, controls map[string]any) *shadowFixture {
	t.Helper()
	exe, err := detachtest.Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f := &shadowFixture{root: root, inc: filepath.Join(root, "inc"), opDir: filepath.Join(root, "ops", "op-1"),
		nginx: filepath.Join(root, "bin", "nginx"), log: &syncBuffer{}}
	for _, d := range []string{f.inc, filepath.Dir(f.nginx)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(f.opDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := fakeDirs(t, root, f.inc)
	c.Controls = controls
	installFakeNginx(t, f.nginx, c)
	f.tester = shadowBin{exe: exe, dir: f.opDir, includeDir: f.inc, nginx: f.nginx, testTimeout: 30 * time.Second,
		log: f.log}
	return f
}

// candidate writes content as a candidate file outside includeDir.
func (f *shadowFixture) candidate(t *testing.T, name, content string) string {
	t.Helper()
	dir := filepath.Join(f.root, "candidates")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// shadowTestCtx bounds one test's shadow runs.
func shadowTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func readMountinfo(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// dirState is every entry under dir, recursively, without following a
// symlink: its path, type, mode, owner, and its SHA-256 or link target.
func dirState(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		line := fmt.Sprintf("%s %v %d:%d", path, fi.Mode(), st.Uid, st.Gid)
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			line += " -> " + target
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			line += " " + hex.EncodeToString(sum[:])
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// noShadowLeftovers checks that the operation directory holds none of the
// shadow test's files.
func noShadowLeftovers(t *testing.T, opDir string) {
	t.Helper()
	for _, name := range []string{shadowDirName, shadowCandidateFile, shadowInputFile, "." + shadowInputFile + ".tmp"} {
		if _, err := os.Lstat(filepath.Join(opDir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s remains in the operation directory (%v)", name, err)
		}
	}
}

// nginx-provider TC-88: The shadow test agrees with the live test and leaves the host unchanged
// Owner WI 5.4. Requirements: FR-11 (whatIf), ASM-10, L-30.
//
// The symlink is l.conf -> snips/x.inc rather than l.conf -> a.conf: a
// second include of a.conf would repeat its default_server and its upstream,
// which nginx and fakenginx both refuse, so the base tree would be invalid
// before any candidate. The fifth candidate uses the upstream that only the
// symlink, through the bind-mounted subdirectory, defines.
func TestShadowMatchesLiveAndLeavesHostUnchanged(t *testing.T) {
	requireShadowRoot(t)
	f := newShadowFixture(t, nil)
	writeTree(t, f.inc, map[string]string{
		"a.conf":      "upstream backend_a { server 127.0.0.1:9000; }\nserver { listen 8183 default_server; }\n",
		"snips/x.inc": "upstream backend_x { server 127.0.0.1:9001; }\n",
	})
	if err := os.Symlink("snips/x.inc", filepath.Join(f.inc, "l.conf")); err != nil {
		t.Fatal(err)
	}
	ctx := shadowTestCtx(t)
	mountsBefore := readMountinfo(t, "/proc/self/mountinfo")
	stateBefore := dirState(t, f.inc)

	live := nginxBin{path: f.nginx, testTimeout: 30 * time.Second}
	for _, c := range []struct {
		name, content string
		valid         bool
	}{
		{"valid", "server { listen 8184; }\n", true},
		{"invalid", "server {\n  INVALID;\n}\n", false},
		{"duplicate default_server", "server { listen 8183 default_server; }\n", false},
		{"upstream of a.conf", "server { listen 8185; location / { proxy_pass http://backend_a; } }\n", true},
		{"upstream through the symlink", "server { listen 8186; location / { proxy_pass http://backend_x; } }\n", true},
	} {
		path := f.candidate(t, strings.ReplaceAll(c.name, " ", "-"), c.content)
		shadow, err := f.tester.Test(ctx, "cand.conf", path)
		if err != nil {
			t.Fatalf("%s: shadow test: %v\nlog:\n%s", c.name, err, f.log)
		}
		writeTree(t, f.inc, map[string]string{"cand.conf": c.content})
		liveOut, err := live.Test(ctx)
		if rerr := os.Remove(filepath.Join(f.inc, "cand.conf")); rerr != nil {
			t.Fatal(rerr)
		}
		if err != nil {
			t.Fatalf("%s: live test: %v", c.name, err)
		}
		if shadow.Valid != c.valid || liveOut.Valid != c.valid {
			t.Errorf("%s: shadow valid %v, live valid %v, want %v\nshadow:\n%s\nlive:\n%s", c.name, shadow.Valid,
				liveOut.Valid, c.valid, shadow.Output, liveOut.Output)
		}
		// The shadow is at includeDir's own path, so nginx prints the same
		// paths, in the same order.
		if shadow.Output != liveOut.Output {
			t.Errorf("%s: shadow output differs from live:\nshadow:\n%s\nlive:\n%s", c.name, shadow.Output,
				liveOut.Output)
		}
	}

	// The -t runs alternate shadow and live. Every shadow run included the
	// candidate, and a valid one also the symlink after it.
	var tests []fakeCall
	for _, c := range readFakeCalls(t, f.nginx) {
		if slices.Equal(c.Argv[1:], []string{"-t"}) {
			tests = append(tests, c)
		}
	}
	if len(tests) != 10 {
		t.Fatalf("%d -t runs in the call log %+v, want 5 shadow and 5 live", len(tests), tests)
	}
	for i := 0; i < len(tests); i += 2 {
		c := tests[i]
		want := []string{filepath.Join(f.inc, "cand.conf")}
		if c.ExitCode == 0 {
			want = append(want, filepath.Join(f.inc, "l.conf"))
		}
		for _, w := range want {
			if !slices.Contains(c.IncludedFiles, w) {
				t.Errorf("shadow run %d included %v, not %s", i/2, c.IncludedFiles, w)
			}
		}
		if !slices.Equal(c.IncludedFiles, tests[i+1].IncludedFiles) {
			t.Errorf("shadow run %d included %v, the live run %v", i/2, c.IncludedFiles, tests[i+1].IncludedFiles)
		}
	}

	if after := readMountinfo(t, "/proc/self/mountinfo"); after != mountsBefore {
		t.Errorf("mountinfo changed:\nbefore:\n%s\nafter:\n%s", mountsBefore, after)
	}
	if after := dirState(t, f.inc); after != stateBefore {
		t.Errorf("includeDir changed:\nbefore:\n%s\nafter:\n%s", stateBefore, after)
	}
	if _, err := os.Lstat(filepath.Join(f.inc, "cand.conf")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("cand.conf remains: %v", err)
	}
	noShadowLeftovers(t, f.opDir)
}

// findProc is the PID of the process started from path, or 0.
func findProc(path string) int {
	cmdlines, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, c := range cmdlines {
		b, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		if argv0, _, _ := bytes.Cut(b, []byte{0}); string(argv0) == path {
			var pid int
			if _, err := fmt.Sscanf(c, "/proc/%d/cmdline", &pid); err == nil {
				return pid
			}
		}
	}
	return 0
}

// waitProc waits up to within for a process started from path.
func waitProc(t *testing.T, path string, within time.Duration) int {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if pid := findProc(path); pid != 0 {
			return pid
		}
	}
	t.Fatalf("no process of %s within %s", path, within)
	return 0
}

type shadowRun struct {
	out TestOutput
	err error
}

// nginx-provider TC-89: The shadow tree mirrors includeDir's top level
// Owner WI 5.4. Requirements: ASM-10.
//
// fakenginx also sleeps, so that the test can read the running nginx's
// environment and look into its namespace through /proc/<pid>/root, where
// the FIFO's absence is visible (fakenginx's list never prints a FIFO).
func TestShadowTreeConstruction(t *testing.T) {
	requireShadowRoot(t)
	f := newShadowFixture(t, map[string]any{"list": true, "sleepMs": 1500})
	writeOwned(t, filepath.Join(f.inc, "f.conf"), "server { listen 8190; }\n", 0o640, 0, 4)
	writeTree(t, f.inc, map[string]string{
		"cand.conf":        "server { listen 8191; }\n",
		"sub/inner.inc":    "# inner\n",
		"sub/deeper/z.inc": "# deeper\n",
	})
	if err := os.Chmod(filepath.Join(f.inc, "sub", "inner.inc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f.conf", filepath.Join(f.inc, "l.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/target", filepath.Join(f.inc, "abs.link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(f.inc, "p.fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidate := "server { listen 8192; }\n"
	path := f.candidate(t, "cand", candidate)
	sum := sha256.Sum256([]byte(candidate))
	line := func(path string) string { return strings.TrimSuffix(listLine(t, path), "\n") }
	want := []string{
		line(filepath.Join(f.inc, "f.conf")),
		filepath.Join(f.inc, "l.conf") + " -> f.conf",
		filepath.Join(f.inc, "abs.link") + " -> /nonexistent/target",
		line(filepath.Join(f.inc, "sub", "inner.inc")),
		line(filepath.Join(f.inc, "sub", "deeper", "z.inc")),
		fmt.Sprintf("%s 0644 0:0 %s", filepath.Join(f.inc, "cand.conf"), hex.EncodeToString(sum[:])),
	}
	if !strings.Contains(want[0], " 0640 0:4 ") {
		t.Fatalf("setup: f.conf is %q, want mode 0640 owned 0:4", want[0])
	}
	stateBefore := dirState(t, f.inc)

	done := make(chan shadowRun, 1)
	go func() {
		out, err := f.tester.Test(shadowTestCtx(t), "cand.conf", path)
		done <- shadowRun{out, err}
	}()
	pid := waitProc(t, f.nginx, 20*time.Second)
	environ, envErr := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	inside := fmt.Sprintf("/proc/%d/root%s", pid, f.inc)
	_, fifoErr := os.Lstat(filepath.Join(inside, "p.fifo"))
	insideCand, candErr := os.ReadFile(filepath.Join(inside, "cand.conf"))
	r := <-done

	if envErr != nil {
		t.Fatal(envErr)
	}
	if got, want := string(environ), "PATH=/usr/sbin:/usr/bin:/sbin:/bin\x00LC_ALL=C\x00"; got != want {
		t.Errorf("the shadow nginx's environment is %q, want %q", got, want)
	}
	if !errors.Is(fifoErr, fs.ErrNotExist) {
		t.Errorf("the FIFO in the shadow: %v, want it absent", fifoErr)
	}
	if candErr != nil || string(insideCand) != candidate {
		t.Errorf("cand.conf in the shadow is %q (%v), want the candidate", insideCand, candErr)
	}
	if r.err != nil || !r.out.Valid {
		t.Fatalf("shadow test: %+v, %v\nlog:\n%s", r.out, r.err, f.log)
	}
	var listed []string
	for _, l := range strings.Split(r.out.Output, "\n") {
		if strings.HasPrefix(l, f.inc+"/") {
			listed = append(listed, l)
		}
	}
	slices.Sort(listed)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Errorf("the shadow tree as nginx saw it:\n%s\nwant:\n%s", strings.Join(listed, "\n"), strings.Join(want, "\n"))
	}
	if after := dirState(t, f.inc); after != stateBefore {
		t.Errorf("includeDir changed:\nbefore:\n%s\nafter:\n%s", stateBefore, after)
	}
	noShadowLeftovers(t, f.opDir)
}

// shadowE23Inner marks the run of TestShadowPropagationPrivate inside the
// test mount namespace.
const shadowE23Inner = "NEXUS_TEST_SHADOW_E23_INNER"

// nginx-provider TC-E23: Shadow mounts never propagate to the host
// Owner WI 5.4. Requirements: ASM-10, L-30.
//
// The test runs itself again in a mount namespace of its own whose / it
// makes MS_SHARED, as `unshare -m --propagation shared` would, to model
// systemd's shared root: a mount that propagated would appear in that
// namespace's mountinfo.
func TestShadowPropagationPrivate(t *testing.T) {
	requireShadowRoot(t)
	if os.Getenv(shadowE23Inner) == "1" {
		shadowPropagationInner(t)
		return
	}
	exe, err := detachtest.Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShadowPropagationPrivate$", "-test.v",
		"-test.count=1")
	cmd.Env = append(os.Environ(), shadowE23Inner+"=1", detachtest.SupervisorEnv+"="+exe,
		FakeNginxEnv+"="+fakeNginxBinary(t))
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestShadowPropagationPrivate") {
		t.Fatalf("the run in the shared test namespace: %v\n%s", err, out)
	}
}

func shadowPropagationInner(t *testing.T) {
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		t.Fatalf("make / shared: %v", err)
	}
	if root := mountLine(readMountinfo(t, "/proc/self/mountinfo"), "/"); !strings.Contains(root, " shared:") {
		t.Fatalf("/ is not shared in the test namespace: %q", root)
	}
	f := newShadowFixture(t, map[string]any{"sleepMs": 1000})
	writeTree(t, f.inc, map[string]string{"a.conf": "server { listen 8183; }\n"})
	path := f.candidate(t, "cand", "server { listen 8184; }\n")
	shadowPath := filepath.Join(f.opDir, shadowDirName)

	done := make(chan shadowRun, 1)
	go func() {
		out, err := f.tester.Test(shadowTestCtx(t), "cand.conf", path)
		done <- shadowRun{out, err}
	}()
	var leaked []string
	samples, during := 0, 0
	for running := true; running; {
		select {
		case r := <-done:
			running = false
			if r.err != nil || !r.out.Valid {
				t.Fatalf("shadow test: %+v, %v\nlog:\n%s", r.out, r.err, f.log)
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
		mounts := readMountinfo(t, "/proc/self/mountinfo")
		samples++
		for _, l := range strings.Split(mounts, "\n") {
			if fields := strings.Fields(l); len(fields) > 4 && (fields[4] == f.inc || fields[4] == shadowPath) {
				leaked = append(leaked, l)
			}
		}
		// The shadow exists now: nginx runs and sees cand.conf, which the
		// test namespace's includeDir does not have.
		if pid := findProc(f.nginx); pid != 0 {
			_, inErr := os.Lstat(fmt.Sprintf("/proc/%d/root%s/cand.conf", pid, f.inc))
			_, hostErr := os.Lstat(filepath.Join(f.inc, "cand.conf"))
			if inErr == nil && errors.Is(hostErr, fs.ErrNotExist) {
				during++
			}
		}
	}
	if len(leaked) > 0 {
		t.Errorf("shadow mounts visible in the test namespace:\n%s", strings.Join(leaked, "\n"))
	}
	if during < 10 {
		t.Errorf("only %d of %d samples were taken while the shadow nginx ran, want at least 10", during, samples)
	}
}

// mountLine is the mountinfo line of the mount at mountPoint, the last one
// listed (the one on top).
func mountLine(mountinfo, mountPoint string) string {
	line := ""
	for _, l := range strings.Split(mountinfo, "\n") {
		if fields := strings.Fields(l); len(fields) > 4 && fields[4] == mountPoint {
			line = l
		}
	}
	return line
}

// nginx-provider TC-ERR22: An includeDir too large to shadow is refused
// Owner WI 5.4. Requirements: ASM-10, L-31, NFR-2.
func TestShadowSizeCap(t *testing.T) {
	requireShadowRoot(t)
	saved := shadowSizeCap
	shadowSizeCap = 1 << 20
	t.Cleanup(func() { shadowSizeCap = saved })
	f := newShadowFixture(t, nil)
	writeTree(t, f.inc, map[string]string{
		"a.conf":   "server { listen 8183; }\n",
		"big1.bin": strings.Repeat("x", 1<<20),
		"big2.bin": strings.Repeat("y", 1<<20),
	})
	path := f.candidate(t, "cand", "server { listen 8184; }\n")
	mountsBefore := readMountinfo(t, "/proc/self/mountinfo")
	stateBefore := dirState(t, f.inc)

	out, herr := shadowTest(shadowTestCtx(t), env{shadow: f.tester, out: f.log}, "cand.conf", path)
	if herr == nil {
		t.Fatalf("shadow test of a 2 MiB includeDir under a 1 MiB cap: %+v, want an error", out)
	}
	if herr.Slug != problem.DependencyUnavailable || !strings.Contains(herr.Detail, "includeDir too large to shadow") {
		t.Errorf("error %s %q, want dependency-unavailable with %q", herr.Slug, herr.Detail,
			"includeDir too large to shadow")
	}
	if after := readMountinfo(t, "/proc/self/mountinfo"); after != mountsBefore {
		t.Errorf("mountinfo changed:\nbefore:\n%s\nafter:\n%s", mountsBefore, after)
	}
	if after := dirState(t, f.inc); after != stateBefore {
		t.Errorf("includeDir changed:\nbefore:\n%s\nafter:\n%s", stateBefore, after)
	}
	for _, c := range readFakeCalls(t, f.nginx) {
		if slices.Contains(c.Argv, "-t") {
			t.Errorf("nginx -t ran: %+v", c)
		}
	}
	noShadowLeftovers(t, f.opDir)
}

// A non-root helper cannot make a mount namespace: the shadow test refuses,
// saying so, and changes nothing.
func TestShadowRefusesWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as non-root: root may make a mount namespace")
	}
	f := newShadowFixture(t, nil)
	writeTree(t, f.inc, map[string]string{"a.conf": "server { listen 8183; }\n"})
	stateBefore := dirState(t, f.inc)
	_, err := f.tester.Test(shadowTestCtx(t), "cand.conf", f.candidate(t, "cand", "server { listen 8184; }\n"))
	if err == nil || !strings.Contains(err.Error(), "the shadow test needs root") {
		t.Errorf("shadow test as uid %d: %v, want a refusal that needs root", os.Geteuid(), err)
	}
	if after := dirState(t, f.inc); after != stateBefore {
		t.Errorf("includeDir changed:\nbefore:\n%s\nafter:\n%s", stateBefore, after)
	}
	noShadowLeftovers(t, f.opDir)
}

// With nginx's pid directory missing, as on Alpine before nginx first
// starts after a boot, a shadow test makes it as a live test does
// (piddir.go) and reports a valid configuration valid. The shadow does not
// remount /run, so the directory the helper makes is the one nginx uses.
func TestShadowMakesMissingPidDir(t *testing.T) {
	requireShadowRoot(t)
	f := newShadowFixture(t, nil)
	run := filepath.Join(f.root, "piddir-run")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	c := fakeDirs(t, f.root, f.inc)
	c.PidDir = filepath.Join(run, "nginx")
	writeSidecar(t, f.nginx, c)
	setPidDirSeams(t, []string{run}, 0, 0)
	writeTree(t, f.inc, map[string]string{"a.conf": "server { listen 8183; }\n"})

	out, err := f.tester.Test(shadowTestCtx(t), "cand.conf", f.candidate(t, "cand", "server { listen 8184; }\n"))
	if err != nil || !out.Valid || out.Output != plainSuccess {
		t.Errorf("shadow test with the pid directory missing: %+v, %v; want valid\nlog:\n%s", out, err, f.log)
	}
	requireDir(t, c.PidDir, 0o755, 0, 0)
	if !strings.Contains(f.log.String(), createdLine(c.PidDir)) {
		t.Errorf("the log has no %q:\n%s", createdLine(c.PidDir), f.log)
	}
	noShadowLeftovers(t, f.opDir)
}

// The shadow test's child refuses to run in its parent's mount namespace,
// where its mounts would be the host's: started by hand, it mounts nothing.
func TestShadowChildNeedsOwnNamespace(t *testing.T) {
	requireShadowRoot(t)
	f := newShadowFixture(t, nil)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mountsBefore := readMountinfo(t, "/proc/self/mountinfo")
	cmd := exec.Command(f.tester.exe, HelperCommand, "shadow-test", "--dir", f.opDir)
	cmd.ExtraFiles = []*os.File{w}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	w.Close()
	status, _ := io.ReadAll(r)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 ||
		!strings.Contains(string(status), "not in a mount namespace of its own") {
		t.Errorf("shadow-test in the parent's namespace: %v, status %q, stderr %q", err, status, stderr.String())
	}
	if after := readMountinfo(t, "/proc/self/mountinfo"); after != mountsBefore {
		t.Errorf("mountinfo changed:\nbefore:\n%s\nafter:\n%s", mountsBefore, after)
	}
}

// Live check against the host's real nginx (WI 5.4 behavioral
// verification), run only with NEXUS_NGINX_SHADOW_LIVE=1 as root, nginx
// stopped, on a disposable host or harness: it adds files to includeDir
// (NEXUS_NGINX_SHADOW_INCLUDE_DIR) for the run and removes them after. With
// NEXUS_NGINX_SHADOW_PIDDIR set, that directory is removed before each
// shadow test, as on a fresh Alpine boot. For a valid file, an invalid one,
// a duplicate default_server with another file, an upstream from another
// file, a subdirectory include and a disabled site presented as enabled,
// the shadow verdict and nginx's message match the live ones, and
// /proc/1/mountinfo and includeDir are unchanged.
func TestShadowLiveParity(t *testing.T) {
	if os.Getenv("NEXUS_NGINX_SHADOW_LIVE") != "1" {
		t.Skip("set NEXUS_NGINX_SHADOW_LIVE=1 to run against the host's nginx")
	}
	requireShadowRoot(t)
	inc := os.Getenv("NEXUS_NGINX_SHADOW_INCLUDE_DIR")
	if !filepath.IsAbs(inc) {
		t.Fatalf("NEXUS_NGINX_SHADOW_INCLUDE_DIR %q must be an absolute includeDir", inc)
	}
	pidDir := os.Getenv("NEXUS_NGINX_SHADOW_PIDDIR")
	exe, err := detachtest.Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuffer{}
	opDir := filepath.Join(t.TempDir(), "op-live")
	if err := os.Mkdir(opDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tester := shadowBin{exe: exe, dir: opDir, includeDir: inc, nginx: DefaultNginx, testTimeout: 30 * time.Second,
		log: log}
	live := nginxBin{path: DefaultNginx, testTimeout: 30 * time.Second, log: log}
	base := map[string]string{
		"nx54-a.conf":          "upstream nx54_backend_a { server 127.0.0.1:9000; }\nserver { listen 8183 default_server; server_name a.nx54.test; }\n",
		"nx54-snips/x.inc":     "add_header X-Nx54 1;\n",
		"nx54-b.conf.disabled": "server { listen 8187; server_name b.nx54.test; }\n",
	}
	writeTree(t, inc, base)
	t.Cleanup(func() {
		for _, p := range []string{"nx54-a.conf", "nx54-snips", "nx54-b.conf.disabled", "nx54-cand.conf", "nx54-b.conf"} {
			if err := os.RemoveAll(filepath.Join(inc, p)); err != nil {
				t.Error(err)
			}
		}
	})
	ctx := shadowTestCtx(t)
	mountsBefore := readMountinfo(t, "/proc/1/mountinfo")
	stateBefore := dirState(t, inc)

	for _, c := range []struct {
		name, presented, content, path string
		valid                          bool
	}{
		{name: "valid", presented: "nx54-cand.conf", content: "server { listen 8184; server_name v.nx54.test; }\n",
			valid: true},
		{name: "invalid", presented: "nx54-cand.conf", content: "server {\n  INVALID;\n}\n"},
		{name: "duplicate default_server", presented: "nx54-cand.conf",
			content: "server { listen 8183 default_server; server_name d.nx54.test; }\n"},
		{name: "upstream of another file", presented: "nx54-cand.conf", valid: true,
			content: "server { listen 8185; location / { proxy_pass http://nx54_backend_a; } }\n"},
		{name: "subdirectory include", presented: "nx54-cand.conf", valid: true,
			content: fmt.Sprintf("server { listen 8186; include %s/nx54-snips/x.inc; }\n", inc)},
		{name: "disabled site presented as enabled", presented: "nx54-b.conf", valid: true,
			path: filepath.Join(inc, "nx54-b.conf.disabled")},
	} {
		path := c.path
		if path == "" {
			path = filepath.Join(t.TempDir(), "candidate")
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if pidDir != "" {
			if err := os.RemoveAll(pidDir); err != nil {
				t.Fatal(err)
			}
		}
		shadow, err := tester.Test(ctx, c.presented, path)
		if err != nil {
			t.Fatalf("%s: shadow test: %v\nlog:\n%s", c.name, err, log)
		}
		if err := os.WriteFile(filepath.Join(inc, c.presented), content, 0o644); err != nil {
			t.Fatal(err)
		}
		liveOut, err := live.Test(ctx)
		if rerr := os.Remove(filepath.Join(inc, c.presented)); rerr != nil {
			t.Fatal(rerr)
		}
		if err != nil {
			t.Fatalf("%s: live test: %v", c.name, err)
		}
		t.Logf("%s: shadow valid %v, live valid %v\nshadow:\n%s", c.name, shadow.Valid, liveOut.Valid, shadow.Output)
		if shadow.Valid != c.valid || liveOut.Valid != c.valid {
			t.Errorf("%s: shadow valid %v, live valid %v, want %v\nshadow:\n%s\nlive:\n%s", c.name, shadow.Valid,
				liveOut.Valid, c.valid, shadow.Output, liveOut.Output)
		}
		if s, l := stripNginxStamps(shadow.Output), stripNginxStamps(liveOut.Output); s != l {
			t.Errorf("%s: nginx's messages differ:\nshadow:\n%s\nlive:\n%s", c.name, s, l)
		}
	}
	if after := readMountinfo(t, "/proc/1/mountinfo"); after != mountsBefore {
		t.Errorf("/proc/1/mountinfo changed:\nbefore:\n%s\nafter:\n%s", mountsBefore, after)
	}
	if after := dirState(t, inc); after != stateBefore {
		t.Errorf("includeDir changed:\nbefore:\n%s\nafter:\n%s", stateBefore, after)
	}
	noShadowLeftovers(t, opDir)
	t.Logf("log:\n%s", log)
}

// nginxStampRE is the time and PID that Ubuntu's nginx puts before a
// message, which differ between two runs.
var nginxStampRE = regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} (\[\w+\]) \d+#\d+: `)

func stripNginxStamps(s string) string { return nginxStampRE.ReplaceAllString(s, "nginx: $1 ") }
