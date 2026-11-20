// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-50 to TC-STG-59
// (WI 3.1): tool resolution, the runner, the in-flight table and the version
// parser. Captures are gated with capturegate.Require; this internal package
// never imports storagetest, which imports zfs.

package zfs

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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/storage/capturegate"
)

// writeScript plants an executable /bin/sh script named name in dir.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeInfo is a planted fs.FileInfo for Resolve's stat seam.
type fakeInfo struct {
	name string
	mode fs.FileMode
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

func statTable(m map[string]fs.FileInfo) func(string) (fs.FileInfo, error) {
	return func(name string) (fs.FileInfo, error) {
		if fi, ok := m[name]; ok {
			return fi, nil
		}
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
}

func TestTCSTG50_ResolveFindsZpoolAndZfsAtUsr(t *testing.T) {
	exe := func(name string) fs.FileInfo { return fakeInfo{name: name, mode: 0o755} }
	dir := fakeInfo{name: "zpool", mode: fs.ModeDir | 0o755}
	plain := fakeInfo{name: "zpool", mode: 0o644}
	tests := []struct {
		name    string
		files   map[string]fs.FileInfo
		want    Paths
		reasons map[string]string
	}{
		{
			name:  "both at /usr/sbin",
			files: map[string]fs.FileInfo{"/usr/sbin/zpool": exe("zpool"), "/usr/sbin/zfs": exe("zfs")},
			want:  Paths{Zpool: "/usr/sbin/zpool", Zfs: "/usr/sbin/zfs"},
		},
		{
			name:  "only at /sbin",
			files: map[string]fs.FileInfo{"/sbin/zpool": exe("zpool"), "/sbin/zfs": exe("zfs")},
			want:  Paths{Zpool: "/sbin/zpool", Zfs: "/sbin/zfs"},
		},
		{
			name: "at both, /usr/sbin wins",
			files: map[string]fs.FileInfo{
				"/usr/sbin/zpool": exe("zpool"), "/usr/sbin/zfs": exe("zfs"),
				"/sbin/zpool": exe("zpool"), "/sbin/zfs": exe("zfs"),
			},
			want: Paths{Zpool: "/usr/sbin/zpool", Zfs: "/usr/sbin/zfs"},
		},
		{
			// stat follows the link, so the seam answers with the target's info.
			name: "/usr/sbin/zpool a symlink to /sbin/zpool is followed",
			files: map[string]fs.FileInfo{
				"/usr/sbin/zpool": exe("zpool"), "/sbin/zpool": exe("zpool"), "/usr/sbin/zfs": exe("zfs"),
			},
			want: Paths{Zpool: "/usr/sbin/zpool", Zfs: "/usr/sbin/zfs"},
		},
		{
			name:  "/usr/sbin/zpool a directory, /sbin/zpool tried next",
			files: map[string]fs.FileInfo{"/usr/sbin/zpool": dir, "/sbin/zpool": exe("zpool"), "/usr/sbin/zfs": exe("zfs")},
			want:  Paths{Zpool: "/sbin/zpool", Zfs: "/usr/sbin/zfs"},
		},
		{
			name:    "/usr/sbin/zpool a directory and no /sbin/zpool",
			files:   map[string]fs.FileInfo{"/usr/sbin/zpool": dir, "/usr/sbin/zfs": exe("zfs")},
			want:    Paths{Zfs: "/usr/sbin/zfs"},
			reasons: map[string]string{"zpool": "zfs: /usr/sbin/zpool is a directory; /sbin/zpool not found"},
		},
		{
			name:  "/usr/sbin/zpool mode 0644, /sbin/zpool tried next",
			files: map[string]fs.FileInfo{"/usr/sbin/zpool": plain, "/sbin/zpool": exe("zpool"), "/usr/sbin/zfs": exe("zfs")},
			want:  Paths{Zpool: "/sbin/zpool", Zfs: "/usr/sbin/zfs"},
		},
		{
			name:    "/usr/sbin/zpool mode 0644 and no /sbin/zpool",
			files:   map[string]fs.FileInfo{"/usr/sbin/zpool": plain, "/usr/sbin/zfs": exe("zfs")},
			want:    Paths{Zfs: "/usr/sbin/zfs"},
			reasons: map[string]string{"zpool": "zfs: /usr/sbin/zpool is not executable (mode 0644); /sbin/zpool not found"},
		},
		{
			name:  "neither",
			files: map[string]fs.FileInfo{},
			want:  Paths{},
			reasons: map[string]string{
				"zpool": "zfs: /usr/sbin/zpool not found (nor /sbin/zpool)",
				"zfs":   "zfs: /usr/sbin/zfs not found (nor /sbin/zfs)",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reasons := Resolve(statTable(tc.files))
			if got != tc.want {
				t.Errorf("Paths = %+v, want %+v", got, tc.want)
			}
			if len(reasons) != len(tc.reasons) {
				t.Errorf("reasons = %q, want %q", reasons, tc.reasons)
			}
			for k, v := range tc.reasons {
				if reasons[k] != v {
					t.Errorf("reasons[%q] = %q, want %q", k, reasons[k], v)
				}
			}
		})
	}
}

// svcctlEnv extracts the literal list returned by env() in
// internal/svcctl/svcctl.go.
func svcctlEnv(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "svcctl", "svcctl.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var list []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "env" || fn.Recv != nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING {
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				list = append(list, s)
			}
			return true
		})
	}
	if len(list) == 0 {
		t.Fatalf("no env() string list found in %s", path)
	}
	return list
}

func TestTCSTG51_ExecGivesTheChildExactlyTheFixed(t *testing.T) {
	t.Setenv("NEXUS_LEAK", "1")
	script := writeScript(t, t.TempDir(), "envcheck", "env\ncat\necho END\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	res, err := Exec(ctx, []string{script})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Exec took %v: cat did not see EOF at once", d)
	}
	lines := strings.Split(strings.TrimSuffix(string(res.Stdout), "\n"), "\n")
	if len(lines) == 0 || lines[len(lines)-1] != "END" {
		t.Fatalf("stdout does not end in END (cat read something or never ended): %q", res.Stdout)
	}
	var got []string
	for _, l := range lines[:len(lines)-1] {
		// Only what /bin/sh itself adds is allowed beyond the fixed list.
		if strings.HasPrefix(l, "PWD=") || strings.HasPrefix(l, "SHLVL=") || strings.HasPrefix(l, "_=") {
			continue
		}
		got = append(got, l)
	}
	want := svcctlEnv(t)
	if fixed := env(); strings.Join(fixed, "\n") != strings.Join(want, "\n") {
		t.Errorf("zfs env() = %q, svcctl env() = %q", fixed, want)
	}
	if strings.Join(sorted(got), "\n") != strings.Join(sorted(want), "\n") {
		t.Errorf("child environment = %q, want exactly %q", got, want)
	}
	if bytes.Contains(res.Stdout, []byte("NEXUS_LEAK")) {
		t.Errorf("NEXUS_LEAK reached the child: %q", res.Stdout)
	}

	_, err = Exec(ctx, []string{"zpool", "list"})
	if !errors.Is(err, ErrNotStarted) {
		t.Errorf("Exec(relative) = %v, want ErrNotStarted", err)
	}
}

func sorted(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

func TestTCSTG52_StdoutOver4MiBIsAnError(t *testing.T) {
	dir := t.TempDir()
	exact := writeScript(t, dir, "exact", fmt.Sprintf("head -c %d /dev/zero\n", StdoutCap))
	over := writeScript(t, dir, "over", fmt.Sprintf("head -c %d /dev/zero\n", StdoutCap+1))
	noisy := writeScript(t, dir, "noisy", "head -c 1048576 /dev/zero >&2\necho ok\nexit 3\n")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := Exec(ctx, []string{exact})
	if err != nil || len(res.Stdout) != StdoutCap {
		t.Errorf("exactly StdoutCap: len(Stdout) = %d, err = %v; want %d, nil", len(res.Stdout), err, StdoutCap)
	}

	res, err = Exec(ctx, []string{over})
	if !errors.Is(err, ErrStdoutTooLarge) {
		t.Errorf("StdoutCap+1: err = %v, want ErrStdoutTooLarge", err)
	}
	if len(res.Stdout) != 0 {
		t.Errorf("StdoutCap+1: a truncated stdout of %d bytes was returned", len(res.Stdout))
	}

	res, err = Exec(ctx, []string{noisy})
	if err != nil {
		t.Fatalf("noisy: %v", err)
	}
	if string(res.Stdout) != "ok\n" || len(res.Stderr) != StderrCap || res.ExitCode != 3 {
		t.Errorf("noisy: Stdout = %q, len(Stderr) = %d, ExitCode = %d; want \"ok\\n\", %d, 3",
			res.Stdout, len(res.Stderr), res.ExitCode, StderrCap)
	}
}

// alive reports whether pid names a live process; a zombie counts as dead.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return false
		}
		// The state follows the parenthesised command name.
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) && b[i+2] == 'Z' {
			return false
		}
	}
	return true
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestTCSTG53_ATimeoutKillsTheChildSWhole(t *testing.T) {
	dir := t.TempDir()
	childFile, grandFile := filepath.Join(dir, "child.pid"), filepath.Join(dir, "grandchild.pid")
	script := writeScript(t, dir, "hang", fmt.Sprintf(
		"echo $$ > %q\nsleep 60 &\necho $! > %q\nsleep 60\n", childFile, grandFile))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Exec(ctx, []string{script})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	// 300 ms + WaitDelay, with 250 ms of scheduling slack for -race.
	if limit := 300*time.Millisecond + waitDelay + 250*time.Millisecond; elapsed > limit {
		t.Errorf("Exec returned after %v, want within %v", elapsed, limit)
	}
	pids := []int{readPID(t, childFile), readPID(t, grandFile)}
	deadline := time.Now().Add(time.Second)
	for _, pid := range pids {
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if alive(pid) {
			t.Errorf("pid %d is still alive 1 s after Exec returned", pid)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func TestTCSTG54_AStartFailureWrapsErrNotStarted(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	_, err := Exec(ctx, []string{filepath.Join(dir, "missing"), "list"})
	if !errors.Is(err, ErrNotStarted) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing path: err = %v, want ErrNotStarted and fs.ErrNotExist", err)
	}

	noexec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Exec(ctx, []string{noexec})
	if !errors.Is(err, ErrNotStarted) {
		t.Errorf("non-executable file: err = %v, want ErrNotStarted", err)
	}
}

// recorder is a Runner that records every argv it is called with.
type recorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recorder) run(_ context.Context, argv []string) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), argv...))
	return Result{Stdout: []byte("ok\n")}, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func fixed(d time.Duration) func() time.Duration { return func() time.Duration { return d } }

func TestTCSTG55_ToolsRunRefusesAnyProgramOtherThan(t *testing.T) {
	rec := &recorder{}
	tools := NewTools(Paths{Zpool: "/usr/sbin/zpool", Zfs: "/usr/sbin/zfs"}, rec.run, fixed(time.Second))
	ctx := context.Background()
	for _, argv := range [][]string{
		{"/usr/sbin/pvesm", "status"},
		{"/usr/sbin/qm", "list"},
		{"/bin/sh", "-c", "id"},
		{"/sbin/zpool", "list"},
		{"zpool", "list"},
		{},
	} {
		if _, err := tools.Run(ctx, argv); err == nil {
			t.Errorf("Run(%q) = nil error, want a refusal", argv)
		}
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("the runner saw %d calls for refused programs: %q", n, rec.calls)
	}
	res, err := tools.Run(ctx, []string{"/usr/sbin/zpool", "list", "-H", "-o", "name"})
	if err != nil || string(res.Stdout) != "ok\n" {
		t.Errorf("Run(zpool list) = %q, %v", res.Stdout, err)
	}
	if n := rec.count(); n != 1 {
		t.Errorf("the runner saw %d calls, want 1", n)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (t *Tools) flightCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.flights)
}

func TestTCSTG56_ConcurrentIdenticalCallsShareOneProcess(t *testing.T) {
	dir := t.TempDir()
	counter, release := filepath.Join(dir, "counter"), filepath.Join(dir, "release")
	script := writeScript(t, dir, "tool", fmt.Sprintf(
		"echo \"$*\" >> %q\nwhile [ ! -e %q ]; do sleep 0.02; done\necho R\n", counter, release))
	tools := NewTools(Paths{Zpool: script, Zfs: script}, nil, fixed(10*time.Second))
	ctx := context.Background()

	// call runs argv on n goroutines and returns once each has entered Run.
	call := func(n int, argv []string) (*sync.WaitGroup, []Result, []error) {
		var entered, done sync.WaitGroup
		res, errs := make([]Result, n), make([]error, n)
		for i := range n {
			entered.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				entered.Done()
				res[i], errs[i] = tools.Run(ctx, argv)
			}()
		}
		entered.Wait()
		return &done, res, errs
	}
	check := func(step string, res []Result, errs []error) {
		t.Helper()
		for i := range res {
			if errs[i] != nil || string(res[i].Stdout) != "R\n" {
				t.Errorf("%s: caller %d got %q, %v; want \"R\\n\", nil", step, i, res[i].Stdout, errs[i])
			}
		}
	}

	// Step 1: eight identical calls.
	done, res, errs := call(8, []string{script, "list"})
	waitFor(t, "the first process", func() bool { return countLines(t, counter) == 1 })
	time.Sleep(200 * time.Millisecond) // let every caller reach the flight
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	done.Wait()
	check("step 1", res, errs)
	if n := countLines(t, counter); n != 1 {
		t.Errorf("step 1: %d processes started, want 1", n)
	}

	// Step 2: two different argv, four callers each.
	if err := os.Remove(release); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(counter); err != nil {
		t.Fatal(err)
	}
	doneA, resA, errsA := call(4, []string{script, "list", "a"})
	doneB, resB, errsB := call(4, []string{script, "list", "b"})
	waitFor(t, "two processes", func() bool { return countLines(t, counter) == 2 })
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	doneA.Wait()
	doneB.Wait()
	check("step 2 a", resA, errsA)
	check("step 2 b", resB, errsB)
	if n := countLines(t, counter); n != 2 {
		t.Errorf("step 2: %d processes started, want 2", n)
	}

	// A finished flight is removed: the next identical call starts anew.
	if n := tools.flightCount(); n != 0 {
		t.Errorf("%d flights left after every call finished", n)
	}
	r, err := tools.Run(ctx, []string{script, "list", "a"})
	if err != nil || string(r.Stdout) != "R\n" {
		t.Errorf("later call: %q, %v", r.Stdout, err)
	}
	if n := countLines(t, counter); n != 3 {
		t.Errorf("after a later identical call: %d processes, want 3", n)
	}
}

// stuck is a Runner whose calls ignore ctx and end only when the argv's
// release channel is closed, like a process in uninterruptible sleep that
// SIGKILL cannot end. An argv without a release channel answers at once.
type stuck struct {
	mu      sync.Mutex
	release map[string]chan struct{}
	calls   [][]string
}

func newStuck(argvs ...[]string) *stuck {
	s := &stuck{release: make(map[string]chan struct{})}
	for _, a := range argvs {
		s.release[strings.Join(a, " ")] = make(chan struct{})
	}
	return s
}

func (s *stuck) run(_ context.Context, argv []string) (Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), argv...))
	ch := s.release[strings.Join(argv, " ")]
	s.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return Result{Stdout: []byte("done\n")}, nil
}

func (s *stuck) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stuck) saw(argv []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.Join(c, " ") == strings.Join(argv, " ") {
			return true
		}
	}
	return false
}

// open closes argv's release channel once; it is safe to call again.
func (s *stuck) open(argv []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.Join(argv, " ")
	if ch, ok := s.release[k]; ok {
		close(ch)
		delete(s.release, k)
	}
}

func TestTCSTG57_ACallStuckPastItsTimeoutAnswers(t *testing.T) {
	argv := []string{"/usr/sbin/zpool", "status", "-p"}
	s := newStuck(argv)
	tools := NewTools(Paths{Zpool: "/usr/sbin/zpool", Zfs: "/usr/sbin/zfs"}, s.run, fixed(200*time.Millisecond))
	t.Cleanup(func() {
		s.open(argv)
		waitFor(t, "the flight to end", func() bool { return tools.flightCount() == 0 })
	})
	ctx := context.Background()

	start := time.Now()
	_, err := tools.Run(ctx, argv)
	var te TimeoutError
	if !errors.As(err, &te) || te.After != 200*time.Millisecond || te.What == "" {
		t.Fatalf("step 1: err = %#v, want TimeoutError{What, After: 200ms}", err)
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 500*time.Millisecond {
		t.Errorf("step 1 returned after %v, want at its 200 ms deadline", d)
	}

	start = time.Now()
	_, err = tools.Run(ctx, argv)
	d := time.Since(start)
	var pe PendingError
	if !errors.As(err, &pe) || pe.What == "" || pe.Since.IsZero() {
		t.Fatalf("step 2: err = %#v, want PendingError{What, Since}", err)
	}
	if d >= 20*time.Millisecond {
		t.Errorf("step 2 took %v, want under 20 ms", d)
	}
	if n := s.count(); n != 1 {
		t.Errorf("step 2: the runner was called %d times, want 1", n)
	}

	s.open(argv)
	waitFor(t, "the flight to clear", func() bool { return tools.flightCount() == 0 })
	res, err := tools.Run(ctx, argv)
	if err != nil || string(res.Stdout) != "done\n" {
		t.Errorf("step 3: %q, %v", res.Stdout, err)
	}
	if n := s.count(); n != 2 {
		t.Errorf("step 3: the runner was called %d times, want 2", n)
	}
}

func TestTCSTG58_AtMostFourToolProcessesAFifth(t *testing.T) {
	if MaxProcesses != 4 {
		t.Fatalf("MaxProcesses = %d, want 4", MaxProcesses)
	}
	var stuckArgv [][]string
	for i := range 4 {
		stuckArgv = append(stuckArgv, []string{"/usr/sbin/zfs", "list", strconv.Itoa(i)})
	}
	fifth := []string{"/usr/sbin/zpool", "list", "fifth"}
	s := newStuck(stuckArgv...)
	tools := NewTools(Paths{Zpool: "/usr/sbin/zpool", Zfs: "/usr/sbin/zfs"}, s.run, fixed(150*time.Millisecond))
	t.Cleanup(func() {
		for _, a := range stuckArgv {
			s.open(a)
		}
		waitFor(t, "every flight to end", func() bool { return tools.flightCount() == 0 })
	})

	// Step 1: four stuck calls, each times out and stays stuck.
	var wg sync.WaitGroup
	errs := make([]error, len(stuckArgv))
	for i, a := range stuckArgv {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = tools.Run(context.Background(), a)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		var te TimeoutError
		if !errors.As(err, &te) {
			t.Errorf("stuck call %d: err = %v, want TimeoutError", i, err)
		}
	}

	// Step 2: the fifth waits until its 100 ms deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := tools.Run(ctx, fifth)
	d := time.Since(start)
	if err == nil || err.Error() != "4 storage commands are still running" {
		t.Errorf("step 2: err = %v, want \"4 storage commands are still running\"", err)
	}
	if d < 90*time.Millisecond || d > 400*time.Millisecond {
		t.Errorf("step 2 returned after %v, want about 100 ms", d)
	}
	if s.saw(fifth) {
		t.Errorf("step 2: the fifth argv reached the runner")
	}

	// Step 3: one stuck process exits, freeing its slot.
	s.open(stuckArgv[0])
	waitFor(t, "the released flight to clear", func() bool { return tools.flightCount() == 3 })
	res, err := tools.Run(context.Background(), fifth)
	if err != nil || string(res.Stdout) != "done\n" {
		t.Errorf("step 3: %q, %v", res.Stdout, err)
	}
}

// captureVersion splits a capture's zfs-version.txt into the tool's stdout
// and exit code: host captures end in one "Exit <code>" line (testdata
// README), upstream vectors are stdout only with exit 0.
func captureVersion(b []byte) ([]byte, int) {
	s := strings.TrimSuffix(string(b), "\n")
	i := strings.LastIndexByte(s, '\n')
	last := s[i+1:]
	if code, ok := strings.CutPrefix(last, "Exit "); ok {
		n, err := strconv.Atoi(code)
		if err == nil {
			return []byte(s[:i+1]), n
		}
	}
	return b, 0
}

func TestTCSTG59_ParseVersionOverEveryCaptureSZfsVersion(t *testing.T) {
	root := filepath.Join("..", "testdata", "captures")

	t.Run("home-server capture present", func(t *testing.T) {
		capturegate.Require(t, filepath.Join(root, "home-server", "zfs", "zfs-version.txt"))
	})

	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "zfs-version.txt" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no zfs-version.txt under testdata/captures")
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			out, code := captureVersion(b)
			v, err := ParseVersion(out)
			if code != 0 && len(bytes.TrimSpace(out)) == 0 {
				if err == nil {
					t.Errorf("exit %d with no output parsed as %+v", code, v)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVersion: %v", err)
			}
			var user, kern string
			for _, l := range strings.Split(string(out), "\n") {
				if k, ok := strings.CutPrefix(l, "zfs-kmod-"); ok {
					kern = k
				} else if u, ok := strings.CutPrefix(l, "zfs-"); ok && user == "" {
					user = u
				}
			}
			if v.Userland != user || v.Kernel != kern {
				t.Errorf("Userland, Kernel = %q, %q; want %q, %q", v.Userland, v.Kernel, user, kern)
			}
			parts := strings.SplitN(user, ".", 3)
			major, _ := strconv.Atoi(parts[0])
			minor, _ := strconv.Atoi(parts[1])
			if v.Major != major || v.Minor != minor {
				t.Errorf("Major.Minor = %d.%d, want %d.%d", v.Major, v.Minor, major, minor)
			}
			want := Text
			if major > 2 || major == 2 && minor >= 3 {
				want = JSON
			}
			if v.Format() != want {
				t.Errorf("Format() = %v, want %v", v.Format(), want)
			}
		})
	}

	tests := []struct {
		in      string
		want    Version
		format  Format
		wantErr bool
	}{
		{in: "zfs-2.2.6-1\nzfs-kmod-2.2.6-1\n", want: Version{Userland: "2.2.6-1", Kernel: "2.2.6-1", Major: 2, Minor: 2, Patch: 6}, format: Text},
		{in: "zfs-2.3.0\nzfs-kmod-2.3.0\n", want: Version{Userland: "2.3.0", Kernel: "2.3.0", Major: 2, Minor: 3, Patch: 0}, format: JSON},
		{in: "zfs-2.4.1-1\nzfs-kmod-2.4.1-1\n", want: Version{Userland: "2.4.1-1", Kernel: "2.4.1-1", Major: 2, Minor: 4, Patch: 1}, format: JSON},
		{in: "zfs-2.2.6-1\n", want: Version{Userland: "2.2.6-1", Major: 2, Minor: 2, Patch: 6}, format: Text},
		{in: "garbage\n", wantErr: true},
		{in: "", wantErr: true},
		{in: "zfs-\n", wantErr: true},
		{in: "zfs-x.y\nzfs-kmod-x.y\n", wantErr: true},
		{in: "zfs-kmod-2.2.6-1\n", wantErr: true},
	}
	for _, tc := range tests {
		v, err := ParseVersion([]byte(tc.in))
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseVersion(%q) = %+v, want an error", tc.in, v)
			}
			continue
		}
		if err != nil || v != tc.want || v.Format() != tc.format {
			t.Errorf("ParseVersion(%q) = %+v (format %v), %v; want %+v (format %v)", tc.in, v, v.Format(), err, tc.want, tc.format)
		}
	}
}

// TC-STG-58 (code review CR-6): a caller that cancels while it waits for a
// slot gets its own context error, not the "4 storage commands are still
// running" reason, which is for a wait that ran out its deadline.
func TestTCSTG58_ACancelledCallerWaitingForASlotGetsItsContextError(t *testing.T) {
	var stuckArgv [][]string
	for i := range MaxProcesses {
		stuckArgv = append(stuckArgv, []string{"/usr/sbin/zfs", "list", strconv.Itoa(i)})
	}
	fifth := []string{"/usr/sbin/zpool", "list", "fifth"}
	s := newStuck(stuckArgv...)
	tools := NewTools(Paths{Zpool: "/usr/sbin/zpool", Zfs: "/usr/sbin/zfs"}, s.run, fixed(5*time.Second))
	t.Cleanup(func() {
		for _, a := range stuckArgv {
			s.open(a)
		}
		waitFor(t, "every flight to end", func() bool { return tools.flightCount() == 0 })
	})
	bg, stop := context.WithCancel(context.Background())
	defer stop()
	for _, a := range stuckArgv {
		go func() { _, _ = tools.Run(bg, a) }()
	}
	waitFor(t, "four flights", func() bool { return tools.flightCount() == MaxProcesses })

	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		_, err := tools.Run(ctx, fifth)
		res <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBusy) {
			t.Errorf("cancelled waiter: err = %v, want context.Canceled and not ErrBusy", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled waiter did not return")
	}
	if s.saw(fifth) {
		t.Errorf("the fifth argv reached the runner")
	}
}
