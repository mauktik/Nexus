// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nginx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	nexusserver "github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// fakenginx (testdata/fakenginx) is built once per test process, as
// detachtest.Supervisor builds nexusd, and copied to each test's nginx
// path, beside which it finds its sidecar <path>.json.

// FakeNginxEnv names a prebuilt fakenginx that fakeNginxBinary returns
// instead of building one, for runs without a Go toolchain.
const FakeNginxEnv = "NEXUS_TEST_FAKENGINX"

var fakeNginxBuild struct {
	once sync.Once
	path string
	err  error
}

func fakeNginxBinary(t testing.TB) string {
	t.Helper()
	fakeNginxBuild.once.Do(func() {
		if p := os.Getenv(FakeNginxEnv); p != "" {
			fakeNginxBuild.path = p
			if _, err := os.Stat(p); err != nil {
				fakeNginxBuild.err = fmt.Errorf("%s: %w", FakeNginxEnv, err)
			}
			return
		}
		fakeNginxBuild.path, fakeNginxBuild.err = buildFakeNginx()
	})
	if fakeNginxBuild.err != nil {
		t.Fatal(fakeNginxBuild.err)
	}
	return fakeNginxBuild.path
}

func buildFakeNginx() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("fakenginx: cannot locate the package")
	}
	parent := filepath.Join(os.TempDir(), "nexus-fakenginx")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	pruneFakeBuilds(parent)
	dir, err := os.MkdirTemp(parent, "build-*")
	if err != nil {
		return "", err
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("fakenginx: no go tool (set %s): %w", FakeNginxEnv, err)
	}
	out := filepath.Join(dir, "fakenginx")
	cmd := exec.Command(goTool, "build", "-o", out, "./testdata/fakenginx")
	cmd.Dir = filepath.Dir(file)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("fakenginx: build: %w\n%s", err, b)
	}
	return out, nil
}

// pruneFakeBuilds removes builds of earlier test runs, older than an hour.
func pruneFakeBuilds(parent string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
				fmt.Fprintf(os.Stderr, "fakenginx: prune %s: %v\n", e.Name(), err)
			}
		}
	}
}

// fakeConfig is fakenginx's sidecar.
type fakeConfig struct {
	IncludeDir    string         `json:"includeDir"`
	IncludeSuffix string         `json:"includeSuffix,omitempty"`
	ErrorLogDir   string         `json:"errorLogDir"`
	PidDir        string         `json:"pidDir"`
	TempDir       string         `json:"tempDir"`
	Port          int            `json:"port,omitempty"`
	Controls      map[string]any `json:"controls,omitempty"`
}

// fakeDirs is a sidecar over includeDir with writable log, pid and temp
// directories under root.
func fakeDirs(t *testing.T, root, includeDir string) fakeConfig {
	t.Helper()
	c := fakeConfig{IncludeDir: includeDir, ErrorLogDir: filepath.Join(root, "log"), PidDir: filepath.Join(root, "pid"),
		TempDir: filepath.Join(root, "tmp")}
	for _, d := range []string{c.ErrorLogDir, c.PidDir, c.TempDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// installFakeNginx copies fakenginx to path and writes its sidecar.
func installFakeNginx(t *testing.T, path string, c fakeConfig) {
	t.Helper()
	src, err := os.Open(fakeNginxBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(errors.Join(err, dst.Close()))
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	writeSidecar(t, path, c)
}

func writeSidecar(t *testing.T, path string, c fakeConfig) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".json", b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeCall is one line of <fakenginx>.json.calls.
type fakeCall struct {
	Argv          []string `json:"argv"`
	IncludedFiles []string `json:"includedFiles"`
	ExitCode      int      `json:"exitCode"`
}

func readFakeCalls(t *testing.T, path string) []fakeCall {
	t.Helper()
	b, err := os.ReadFile(path + ".json.calls")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []fakeCall
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var c fakeCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("calls line %q: %v", sc.Text(), err)
		}
		calls = append(calls, c)
	}
	return calls
}

func readDenied(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path + ".json.denied")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// plainSuccess is nginx -t's whole output for a valid configuration.
const plainSuccess = "nginx: the configuration file /etc/nginx/nginx.conf syntax is ok\n" +
	"nginx: configuration file /etc/nginx/nginx.conf test is successful\n"

type fakeRun struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
}

// runFake runs the fake at path as the helper runs nginx: by absolute path,
// with nginxEnv plus extra.
func runFake(t *testing.T, path string, extra []string, args ...string) fakeRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(nginxEnv(), extra...)
	cmd.Dir = "/"
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	r := fakeRun{stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start)}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	default:
		t.Fatalf("%s %v: %v", path, args, err)
	}
	return r
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func emptyDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// treeHashes is every regular file under dir with its SHA-256.
func treeHashes(t *testing.T, dir string) map[string]string {
	t.Helper()
	h := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		h[path] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// fakeProcesses counts the running processes started from path: from
// /proc where there is one (busybox ps has no -A), else from ps.
func fakeProcesses(t *testing.T, path string) int {
	t.Helper()
	if runtime.GOOS == "linux" {
		cmdlines, err := filepath.Glob("/proc/[0-9]*/cmdline")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range cmdlines {
			b, err := os.ReadFile(c)
			if err != nil {
				continue // The process has exited
			}
			if argv0, _, _ := bytes.Cut(b, []byte{0}); string(argv0) == path {
				n++
			}
		}
		return n
	}
	out, err := exec.Command("ps", "-A", "-o", "command=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, path+" ") || l == path {
			n++
		}
	}
	return n
}

// helperFixture is a provider over a temp includeDir with fakenginx as its
// nginx, unless noNginx, with preflight bypassed.
func helperFixture(t *testing.T, noNginx bool) (*fixture, fakeConfig) {
	t.Helper()
	f := newFixture(t, fixtureOptions{noNginx: true, preflight: enabledStatus})
	c := fakeDirs(t, filepath.Dir(filepath.Dir(f.nginx)), f.dir)
	if !noNginx {
		installFakeNginx(t, f.nginx, c)
	}
	return f, c
}

// opsServer is a real nexus server with an operation manager whose
// detached operations run through detachtest.Local: the real nexusd
// op-supervise, which runs the real nexusd nginx-op helper.
type opsServer struct {
	t        *testing.T
	url      string
	token    string
	auditDir string
	opsDir   string
	http     *http.Client
}

func startOps(t *testing.T, f *fixture, tune func(*config.NginxConfig)) *opsServer {
	t.Helper()
	exe, err := detachtest.Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	stateDir, auditDir := t.TempDir(), t.TempDir()
	opsDir := filepath.Join(stateDir, "ops")
	alog, err := audit.Open(audit.Options{Dir: auditDir, Host: "h1", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := detach.New(detach.Config{OpsDir: opsDir, Executable: exe, Logger: log}, detachtest.Local())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: db, Audit: alog, Dir: opsDir, Host: "h1", Logger: log,
		PollInterval: 20 * time.Millisecond, Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	f.p.UseOpsDir(opsDir)
	f.p.executable = func() (string, error) { return exe, nil }
	decode := func(into any) error {
		if err := f.decode(into); err != nil {
			return err
		}
		if tune != nil {
			tune(into.(*config.NginxConfig))
		}
		return nil
	}
	reg, err := core.New([]provider.Provider{f.p}, core.Options{Host: "h1", Version: "v0.0.0-test", Logger: log,
		Audit: alog, Ops: mgr, Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: decode}},
		Platform: ubuntu})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "reader", Scopes: []string{spec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := nexusserver.New(reg, nexusserver.Options{Logger: log,
		Auth: auth.New(stateDir, auth.Options{Logger: log, Audit: alog, FailureLimitPerMinute: 100000})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	s := &opsServer{t: t, url: "http://" + ln.Addr().String(), token: tok, auditDir: auditDir, opsDir: opsDir,
		http: &http.Client{Transport: &http.Transport{}, Timeout: 30 * time.Second}}
	t.Cleanup(func() {
		s.http.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("ops shutdown: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
		waitWatchLoopsEnd(t)
	})
	return s
}

// waitWatchLoopsEnd waits for detach.Launcher's watch loop to exit. The loop
// ends on the first PollInterval tick after the last watched operation is
// gone, so after mgr.Shutdown it outlives the test by up to a tick, and the
// next test of the package would see it as an extra goroutine.
func waitWatchLoopsEnd(t *testing.T) {
	t.Helper()
	const loop = "detach.(*Launcher).watchLoop"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if !strings.Contains(goroutineDump(), loop) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%s still running 10 s after the ops manager shut down", loop)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func goroutineDump() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}

func (s *opsServer) do(method, path string, body io.Reader) (int, []byte) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.url+"/v1"+path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.StatusCode, b
}

// startTest POSTs nginx.actions.test and returns the accepted operation.
func (s *opsServer) startTest() apitypes.Operation {
	s.t.Helper()
	code, b := s.do(http.MethodPost, "/providers/nginx/actions/test", strings.NewReader("{}"))
	if code != http.StatusAccepted {
		s.t.Fatalf("POST actions/test = %d %s, want 202", code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		s.t.Fatalf("%s: %v", b, err)
	}
	return op
}

func (s *opsServer) get(id string) apitypes.Operation {
	s.t.Helper()
	code, b := s.do(http.MethodGet, "/operations/"+id, nil)
	if code != http.StatusOK {
		s.t.Fatalf("GET operation %s = %d %s", id, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		s.t.Fatalf("%s: %v", b, err)
	}
	return op
}

// wait follows operation id until it ends.
func (s *opsServer) wait(id string, within time.Duration) apitypes.Operation {
	s.t.Helper()
	deadline := time.Now().Add(within)
	for {
		op := s.get(id)
		if op.Status.Terminal() {
			return op
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("operation %s still %s after %s", id, op.Status, within)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitStatus polls operation id until it has status.
func (s *opsServer) waitStatus(id string, status apitypes.OperationStatus, within time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(within)
	for s.get(id).Status != status {
		if time.Now().After(deadline) {
			s.t.Fatalf("operation %s never %s", id, status)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// log is the whole operation log of id, frames' data concatenated.
func (s *opsServer) log(id string) string {
	s.t.Helper()
	code, b := s.do(http.MethodGet, "/operations/"+id+"/logs", nil)
	if code != http.StatusOK {
		s.t.Fatalf("GET logs %s = %d %s", id, code, b)
	}
	var out strings.Builder
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		var fr apitypes.Frame
		if err := dec.Decode(&fr); err != nil {
			s.t.Fatalf("log frame: %v in %s", err, b)
		}
		out.WriteString(fr.D)
	}
	return out.String()
}

// auditRecords are the audit log's records of action.
func (s *opsServer) auditRecords(action string) []systemspec.AuditRecord {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.auditDir, audit.FileName))
	if err != nil {
		s.t.Fatal(err)
	}
	var recs []systemspec.AuditRecord
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r systemspec.AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			s.t.Fatalf("audit line %q: %v", sc.Text(), err)
		}
		if r.Action == action {
			recs = append(recs, r)
		}
	}
	return recs
}

func testResultOf(t *testing.T, op apitypes.Operation) spec.TestResult {
	t.Helper()
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s: %s, error %+v", op.ID, op.Status, op.Error)
	}
	var r spec.TestResult
	if err := json.Unmarshal(op.Result, &r); err != nil {
		t.Fatalf("result %s: %v", op.Result, err)
	}
	return r
}

// failedWith requires op to have failed with slug and returns its detail.
func failedWith(t *testing.T, op apitypes.Operation, slug string) string {
	t.Helper()
	if op.Status != apitypes.StatusFailed || op.Error == nil || string(op.Error.Slug()) != slug {
		t.Fatalf("operation %s: %s, error %+v, result %s; want Failed %s", op.ID, op.Status, op.Error, op.Result, slug)
	}
	if string(op.Result) != "null" && len(op.Result) != 0 {
		t.Errorf("failed operation has a result %s", op.Result)
	}
	return op.Error.Detail
}

// nginx-provider TC-74: The config test operation reports a valid configuration
// Owner WI 5.1. Requirements: FR-4, CON-2, ASM-8.
func TestConfigTestValid(t *testing.T) {
	f, _ := helperFixture(t, false)
	writeTree(t, f.dir, map[string]string{"a.conf": "server {\n    listen 8181;\n}\n"})
	before := treeHashes(t, f.dir)
	s := startOps(t, f, nil)

	op := s.wait(s.startTest().ID, 30*time.Second)
	r := testResultOf(t, op)
	if !r.Valid || !strings.Contains(r.Output, "syntax is ok") || !strings.Contains(r.Output, "test is successful") ||
		r.OutputTruncated || r.Changed {
		t.Errorf("result %+v, want valid, both success lines, not truncated, not changed", r)
	}
	if r.Output != plainSuccess {
		t.Errorf("output %q, want nginx -t's own %q", r.Output, plainSuccess)
	}
	for _, rec := range s.auditRecords(spec.Test.Op().ID) {
		if rec.Outcome == "requested" || rec.Outcome == "refused" {
			t.Errorf("read-only test audited as a mutation: %+v", rec)
		}
	}
	if l := s.log(op.ID); !strings.Contains(l, plainSuccess) {
		t.Errorf("operation log %q lacks nginx -t's output", l)
	}
	if after := treeHashes(t, f.dir); !mapsEqual(before, after) {
		t.Errorf("includeDir changed: %v, was %v", after, before)
	}
	if d := readDenied(t, f.nginx); d != nil {
		t.Errorf("denied nginx invocations %q", d)
	}
	calls := readFakeCalls(t, f.nginx)
	if len(calls) != 1 || !slices.Equal(calls[0].Argv[1:], []string{"-t"}) || calls[0].Argv[0] != f.nginx ||
		!slices.Equal(calls[0].IncludedFiles, []string{filepath.Join(f.dir, "a.conf")}) {
		t.Errorf("nginx calls %+v, want one -t by its absolute path that included a.conf", calls)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// nginx-provider TC-75: An invalid configuration is a successful operation with valid false
// Owner WI 5.1. Requirements: FR-4, FR-17.
func TestConfigTestInvalid(t *testing.T) {
	f, _ := helperFixture(t, false)
	writeTree(t, f.dir, map[string]string{"a.conf": "server {\n    listen 8181;\n    INVALID;\n}\n"})
	s := startOps(t, f, nil)

	op := s.wait(s.startTest().ID, 30*time.Second)
	if op.Error != nil {
		t.Fatalf("an invalid configuration failed the operation: %+v", op.Error)
	}
	r := testResultOf(t, op)
	want := fmt.Sprintf(`unknown directive "INVALID" in %s:3`, filepath.Join(f.dir, "a.conf"))
	if r.Valid || !strings.Contains(r.Output, want) || !strings.Contains(r.Output, "test failed") {
		t.Errorf("result %+v, want valid false with %q and test failed", r, want)
	}
}

// nginx-provider TC-76: fakenginx encodes nginx's real constraints
// Owner WI 5.1. Requirements: CON-2, NFR-6, FR-4.
func TestFakeNginxEncodesConstraints(t *testing.T) {
	root := t.TempDir()
	inc := filepath.Join(root, "conf.d")
	emptyDir(t, inc)
	base := fakeDirs(t, root, inc)
	nginx := filepath.Join(root, "nginx")
	installFakeNginx(t, nginx, base)
	type wantCall struct {
		args     []string
		code     int
		included []string // Checked when non-nil
	}
	var want []wantCall
	run := func(c fakeConfig, extra []string, included []string, args ...string) fakeRun {
		t.Helper()
		writeSidecar(t, nginx, c)
		r := runFake(t, nginx, extra, args...)
		want = append(want, wantCall{args: args, code: r.code, included: included})
		return r
	}
	nonRoot := os.Geteuid() != 0

	// 1 and 2: an unwritable log or pid directory fails -t. Root ignores
	// the mode bits this uses, so these run as non-root only.
	if nonRoot {
		chmodDir(t, base.ErrorLogDir, 0o500)
		r := run(base, nil, nil, "-t")
		chmodDir(t, base.ErrorLogDir, 0o700)
		if r.code != 1 || !strings.Contains(r.stderr, "could not open error log file") ||
			!(strings.Contains(r.stderr, "(30: Read-only file system)") || strings.Contains(r.stderr, "(13: Permission denied)")) {
			t.Errorf("step 1: %+v, want exit 1 with nginx's error log text", r)
		}
		chmodDir(t, base.PidDir, 0o500)
		r = run(base, nil, nil, "-t")
		chmodDir(t, base.PidDir, 0o700)
		if pid := filepath.Join(base.PidDir, "nginx.pid"); r.code != 1 || !strings.Contains(r.stderr, `open() "`+pid+`" failed`) {
			t.Errorf("step 2: %+v, want exit 1 naming %s", r, pid)
		}
	} else {
		t.Log("steps 1 and 2 skipped: root writes through mode 0500")
	}

	// 3 and 4: only <includeDir>/*.conf without dotfiles is included.
	writeTree(t, inc, map[string]string{".x.conf": "server {\n INVALID;\n}\n"})
	if r := run(base, nil, []string{}, "-t"); r.code != 0 || r.stderr != plainSuccess {
		t.Errorf("step 3: %+v, want exit 0: dotfiles are not included", r)
	}
	emptyDir(t, inc)
	writeTree(t, inc, map[string]string{"x.conf.disabled": "server {\n INVALID;\n}\n"})
	if r := run(base, nil, []string{}, "-t"); r.code != 0 || r.stderr != plainSuccess {
		t.Errorf("step 4: %+v, want exit 0", r)
	}

	// 5: a proxy_pass to an upstream no included file defines.
	emptyDir(t, inc)
	writeTree(t, inc, map[string]string{"use.conf": "server {\n listen 8182;\n location / { proxy_pass http://backend_x; }\n}\n"})
	r := run(base, nil, nil, "-t")
	if wantText := `host not found in upstream "backend_x" in ` + filepath.Join(inc, "use.conf") + ":3"; r.code != 1 ||
		!strings.Contains(r.stderr, wantText) {
		t.Errorf("step 5: %+v, want exit 1 with %q", r, wantText)
	}
	writeTree(t, inc, map[string]string{"def.conf": "upstream backend_x {\n server 127.0.0.1:9;\n}\n"})
	if r := run(base, nil, nil, "-t"); r.code != 0 {
		t.Errorf("step 5, with the upstream defined: %+v, want exit 0", r)
	}

	// 6: two default servers on one port.
	emptyDir(t, inc)
	writeTree(t, inc, map[string]string{"d1.conf": "server { listen 8183 default_server; }\n",
		"d2.conf": "server { listen 8183 default_server; }\n"})
	r = run(base, nil, nil, "-t")
	if wantText := "a duplicate default server for 0.0.0.0:8183 in " + filepath.Join(inc, "d2.conf") + ":1"; r.code != 1 ||
		!strings.Contains(r.stderr, wantText) {
		t.Errorf("step 6: %+v, want exit 1 with %q", r, wantText)
	}

	// 7 and 8: -T and -s are refused and recorded.
	emptyDir(t, inc)
	if r := run(base, nil, nil, "-T"); r.code != 2 {
		t.Errorf("step 7: %+v, want exit 2", r)
	}
	if r := run(base, nil, nil, "-s", "reload"); r.code != 2 {
		t.Errorf("step 8: %+v, want exit 2", r)
	}
	if d := readDenied(t, nginx); !slices.Equal(d, []string{"-T", "-s reload"}) {
		t.Errorf("steps 7 and 8: denied %q, want -T then -s reload", d)
	}

	// 9: the environment does not configure the fake.
	writeTree(t, inc, map[string]string{"a.conf": "server {\n listen 8184;\n}\n"})
	plain := run(base, nil, nil, "-t")
	withEnv := run(base, []string{"NGINX_INCLUDE_DIR=/nonexistent", "FAKENGINX=" + root}, nil, "-t")
	if plain.code != 0 || withEnv.code != plain.code || withEnv.stderr != plain.stderr || withEnv.stdout != plain.stdout {
		t.Errorf("step 9: %+v with extra env, %+v without", withEnv, plain)
	}

	// 10: warnLines prints one [warn] line per WARN file, on only.
	emptyDir(t, inc)
	writeTree(t, inc, map[string]string{"a.conf": "server {\n WARN;\n}\n", "b.conf": "WARN\n", "c.conf": "server {}\n"})
	three := []string{filepath.Join(inc, "a.conf"), filepath.Join(inc, "b.conf"), filepath.Join(inc, "c.conf")}
	on := base
	on.Controls = map[string]any{"warnLines": true}
	r = run(on, nil, three, "-t")
	lines := strings.Split(strings.TrimSuffix(r.stderr, "\n"), "\n")
	if r.code != 0 || len(lines) != 4 || !strings.HasSuffix(r.stderr, plainSuccess) ||
		!strings.HasPrefix(lines[0], "nginx: [warn] ") || !strings.HasSuffix(lines[0], " in "+three[0]+":2") ||
		!strings.HasPrefix(lines[1], "nginx: [warn] ") || !strings.HasSuffix(lines[1], " in "+three[1]+":1") {
		t.Errorf("step 10 on: %+v, want two [warn] lines then the success lines", r)
	}
	if r := run(base, nil, three, "-t"); r.code != 0 || r.stderr != plainSuccess {
		t.Errorf("step 10 off: %+v, want the plain success text", r)
	}

	// 11: list prints the tree as the fake sees it, on only.
	emptyDir(t, inc)
	writeTree(t, inc, map[string]string{"f.conf": "server {}\n", "sub/inner.conf": "# inner\n"})
	chmodFile(t, filepath.Join(inc, "f.conf"), 0o640)
	if err := os.Symlink("f.conf", filepath.Join(inc, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(inc, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	on = base
	on.Controls = map[string]any{"list": true}
	r = run(on, nil, nil, "-t")
	wantList := listLine(t, filepath.Join(inc, "f.conf")) + filepath.Join(inc, "link") + " -> f.conf\n" +
		listLine(t, filepath.Join(inc, "sub", "inner.conf"))
	if r.code != 0 || r.stdout != wantList || r.stderr != plainSuccess {
		t.Errorf("step 11 on: %+v, want stdout %q", r, wantList)
	}
	if r := run(base, nil, nil, "-t"); r.code != 0 || r.stdout != "" {
		t.Errorf("step 11 off: %+v, want no stdout", r)
	}

	// 12: blockFifo holds the first -t until the test writes a byte.
	emptyDir(t, inc)
	fifo := filepath.Join(root, "block.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	on = base
	on.Controls = map[string]any{"blockFifo": fifo, "blockCount": 1}
	writeSidecar(t, nginx, on)
	blocked := exec.Command(nginx, "-t")
	blocked.Env, blocked.Dir = nginxEnv(), "/"
	if err := blocked.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- blocked.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("step 12: the first -t exited before the FIFO was written: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	releaseFifo(t, fifo)
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("step 12: the released -t: %v, want exit 0", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("step 12: the released -t did not exit within 2s")
	}
	want = append(want, wantCall{args: []string{"-t"}, code: 0})
	if r := run(on, nil, nil, "-t"); r.code != 0 {
		t.Errorf("step 12, second -t: %+v, want exit 0 without blocking", r)
	}
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if r := run(base, nil, nil, "-t"); r.code != 0 {
		t.Errorf("step 12 off, no FIFO: %+v, want exit 0", r)
	}

	// 13: sleepMs delays the verdict only.
	on = base
	on.Controls = map[string]any{"sleepMs": 1000}
	slow := run(on, nil, nil, "-t")
	fast := run(base, nil, nil, "-t")
	if slow.code != 0 || slow.elapsed < time.Second || slow.stderr != fast.stderr || fast.code != slow.code {
		t.Errorf("step 13: %+v on, %+v off: want the same verdict, at least 1s later", slow, fast)
	}

	// 14: the call log has every invocation above, in order.
	calls := readFakeCalls(t, nginx)
	if len(calls) != len(want) {
		t.Fatalf("step 14: %d calls logged, want %d: %+v", len(calls), len(want), calls)
	}
	for i, c := range calls {
		w := want[i]
		if c.Argv[0] != nginx || !slices.Equal(c.Argv[1:], w.args) || c.ExitCode != w.code ||
			(w.included != nil && !slices.Equal(c.IncludedFiles, w.included)) {
			t.Errorf("step 14: call %d %+v, want args %q exit %d included %q", i, c, w.args, w.code, w.included)
		}
	}

	// 15: a misspelled control is refused, never ignored.
	bad := base
	bad.Controls = map[string]any{"lsit": true}
	writeSidecar(t, nginx, bad)
	r = runFake(t, nginx, nil, "-t")
	if r.code != 2 || strings.TrimSpace(r.stderr) != `fakenginx: unknown control "lsit"` {
		t.Errorf("step 15: %+v, want exit 2 naming the control", r)
	}
	if d := readDenied(t, nginx); len(d) == 0 || d[len(d)-1] != `unknown control "lsit"` {
		t.Errorf("step 15: denied %q, want the unknown control recorded", d)
	}
}

// listLine is fakenginx's list line of a regular file, computed from
// os.Lstat and the bytes.
func listLine(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%s %04o %d:%d %s\n", path, fi.Mode().Perm(), st.Uid, st.Gid, hex.EncodeToString(sum[:]))
}

func chmodDir(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
}

func chmodFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// releaseFifo writes one byte to the FIFO a blocked fakenginx reads.
func releaseFifo(t *testing.T, fifo string) {
	t.Helper()
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte{1}); err != nil {
		t.Fatal(errors.Join(err, w.Close()))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// nginxDetachedOps are the nine detached nginx operations.
var nginxDetachedOps = []def.Op{spec.Test.Op(), spec.Start.Op(), spec.Stop.Op(), spec.Restart.Op(), spec.Reload.Op(),
	spec.Enable.Op(), spec.Disable.Op(), spec.Add.Op(), spec.Update.Op()}

// nginx-provider TC-77: Every nginx operation shares one queued lane
// Owner WI 5.1. Requirements: FR-13, L-34.
func TestOneQueuedLane(t *testing.T) {
	f, c := helperFixture(t, false)
	writeTree(t, f.dir, map[string]string{"a.conf": "server {}\n"})

	// The lane of each detached operation the provider binds, by table.
	// Every bound detached binding must be one of the nine; one not bound
	// yet is bound by its own work item and checked here once it is.
	bound := 0
	for _, b := range f.p.Bindings() {
		d, ok := b.Detached()
		if !ok {
			continue
		}
		bound++
		if !slices.ContainsFunc(nginxDetachedOps, func(o def.Op) bool { return o.ID == b.Op().ID }) {
			t.Errorf("detached binding %s is not one of the nine nginx operations", b.Op().ID)
		}
		in, err := b.Decode(json.RawMessage(`{"name":"a"}`))
		if err != nil {
			in, err = b.Decode(json.RawMessage(`{}`))
		}
		if err != nil {
			t.Fatalf("%s: decode: %v", b.Op().ID, err)
		}
		l, err := d.Lane(context.Background(), in)
		if err != nil || l != (provider.Lane{Key: "nginx", Policy: provider.LaneQueue}) {
			t.Errorf("%s: Lane() = %+v, %v; want {nginx, queue}", b.Op().ID, l, err)
		}
	}
	if bound == 0 {
		t.Fatal("no detached nginx operation is bound")
	}
	for _, o := range nginxDetachedOps {
		if o.Mode != def.ModeLongRunning {
			t.Errorf("%s is %s, want long-running", o.ID, o.Mode)
		}
	}

	fifo := filepath.Join(t.TempDir(), "block.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	c.Controls = map[string]any{"blockFifo": fifo, "blockCount": 1}
	writeSidecar(t, f.nginx, c)
	s := startOps(t, f, nil)

	first := s.startTest()
	s.waitStatus(first.ID, apitypes.StatusRunning, 30*time.Second)
	waitFile(t, f.nginx+".json.blockcount", 30*time.Second) // Its -t is blocked on the FIFO
	second, third := s.startTest(), s.startTest()
	time.Sleep(300 * time.Millisecond) // Room for a lane that wrongly let them run
	for _, op := range []apitypes.Operation{second, third} {
		if st := s.get(op.ID).Status; st != apitypes.StatusQueued {
			t.Errorf("operation %s is %s while the first runs, want Queued", op.ID, st)
		}
	}
	if n := fakeProcesses(t, f.nginx); n != 1 {
		t.Errorf("%d nginx processes while the first test is blocked, want 1", n)
	}

	releaseFifo(t, fifo)
	ended := []apitypes.Operation{s.wait(first.ID, 30*time.Second), s.wait(second.ID, 30*time.Second),
		s.wait(third.ID, 30*time.Second)}
	for i, op := range ended {
		if r := testResultOf(t, op); !r.Valid {
			t.Errorf("operation %d: %+v, want valid", i, r)
		}
		if i > 0 && (op.Started == nil || ended[i-1].Ended == nil || op.Started.Before(*ended[i-1].Ended)) {
			t.Errorf("operation %d started %v, before operation %d ended %v", i, op.Started, i-1, ended[i-1].Ended)
		}
	}
	if calls := readFakeCalls(t, f.nginx); len(calls) != 3 {
		t.Errorf("%d nginx -t runs, want 3: %+v", len(calls), calls)
	}
}

func waitFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// nginx-provider TC-78: Prepare builds the helper spec with the documented bounds
// Owner WI 5.1. Requirements: NFR-2, CON-2, CON-4.
func TestPrepareSpec(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tune    func(*config.NginxConfig)
		literal time.Duration // The formula's value, stated
	}{
		{"defaults", nil, 240 * time.Second},
		{"tuned", func(c *config.NginxConfig) {
			c.TestTimeout, c.StateTimeout, c.ReloadVerifyTimeout = 5*time.Second, 7*time.Second, 3*time.Second
		}, 95 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := helperFixture(t, false)
			if err := f.p.Configure(func(into any) error {
				if err := f.decode(into); err != nil {
					return err
				}
				if tc.tune != nil {
					tc.tune(into.(*config.NginxConfig))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			opsDir := t.TempDir()
			f.p.UseOpsDir(opsDir)
			const exe = "/usr/local/bin/nexusd"
			f.p.executable = func() (string, error) { return exe, nil }
			s := f.p.Settings()

			sp, err := f.p.prepare(actionTest, helperInput{}, provider.NewRun("op-1", nil))
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(opsDir, "op-1")
			if !slices.Equal(sp.Argv, []string{exe, "nginx-op", "run", "--dir", dir}) {
				t.Errorf("Argv %q", sp.Argv)
			}
			if sp.KillMode != detach.KillProcess {
				t.Errorf("KillMode %q, want %q", sp.KillMode, detach.KillProcess)
			}
			wantMax := 3*s.TestTimeout + 2*s.StateTimeout + 2*s.ReloadVerifyTimeout + 60*time.Second
			if sp.MaxRuntime != wantMax || wantMax != tc.literal {
				t.Errorf("MaxRuntime %s, want %s (%s)", sp.MaxRuntime, wantMax, tc.literal)
			}
			if sp.Timeout != wantMax-30*time.Second || sp.Grace != 10*time.Second {
				t.Errorf("Timeout %s, Grace %s; want %s and 10s", sp.Timeout, sp.Grace, wantMax-30*time.Second)
			}
			if len(sp.Env) != 0 {
				t.Errorf("Env %q, want empty", sp.Env)
			}
			if err := sp.Validate(); err != nil {
				t.Error(err)
			}
			fi, err := os.Stat(filepath.Join(dir, inputFile))
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("input.json mode %v, want 0600", fi.Mode().Perm())
			}
			var in helperInput
			b, err := os.ReadFile(filepath.Join(dir, inputFile))
			if err != nil {
				t.Fatal(err)
			}
			if err := decodeStrict(b, &in); err != nil {
				t.Fatal(err)
			}
			want := helperInput{OperationID: "op-1", Action: "test", Settings: settingsSnapshot{
				IncludeDir: f.dir, IncludeSuffix: s.IncludeSuffix, DisabledSuffix: s.DisabledSuffix, Service: s.Service,
				TestTimeout: s.TestTimeout, StateTimeout: s.StateTimeout, ReloadVerifyTimeout: s.ReloadVerifyTimeout,
				MaxConfigBytes: s.MaxConfigBytes}, InitSystem: "systemd", Nginx: f.nginx}
			if in != want {
				t.Errorf("input.json %+v, want %+v", in, want)
			}
		})
	}
}

// nginx-provider TC-79: nexusd dispatches the hidden nginx-op command
// Owner WI 5.1. Requirements: CON-3, L-50.
func TestNexusdNginxOpCommand(t *testing.T) {
	exe, err := detachtest.Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	f, _ := helperFixture(t, false)
	dir := t.TempDir()
	if err := writeJSONAtomic(dir, inputFile, helperInput{OperationID: "op-1", Action: actionTest,
		Settings: snapshot(f.p.Settings()), InitSystem: "systemd", Nginx: f.nginx}); err != nil {
		t.Fatal(err)
	}
	nexusd := func(args ...string) fakeRun {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		r := fakeRun{stdout: stdout.String(), stderr: stderr.String()}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			r.code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if r := nexusd("nginx-op", "run", "--dir", dir); r.code != 0 {
		t.Fatalf("nginx-op run: %+v, want exit 0", r)
	}
	fi, err := os.Stat(filepath.Join(dir, resultFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("result.json mode %v, want 0600", fi.Mode().Perm())
	}
	var res helperResult
	if ok, err := readJSON(dir, resultFile, &res); !ok || err != nil || res.Error != nil {
		t.Fatalf("result.json %+v, %v, %v", res, ok, err)
	}
	var tr spec.TestResult
	if err := decodeStrict(res.Result, &tr); err != nil || !tr.Valid {
		t.Errorf("result %s: %v; want valid", res.Result, err)
	}

	if r := nexusd("nginx-op", "bogus"); r.code != 2 || !strings.Contains(r.stderr, "usage: nexusd nginx-op run --dir dir") {
		t.Errorf("nginx-op bogus: %+v, want exit 2 with usage", r)
	}
	help := nexusd("--help")
	if out := help.stdout + help.stderr; strings.Contains(out, "nginx-op") || strings.Contains(out, "docker-update") ||
		!strings.Contains(out, "Usage:") {
		t.Errorf("nexusd --help lists a hidden command: %q", out)
	}
}

// nginx-provider TC-E17: Test output over 64 KiB is truncated in the result and kept in the log
// Owner WI 5.1. Requirements: FR-4, L-32.
func TestTestOutputTruncation(t *testing.T) {
	f, c := helperFixture(t, false)
	const files = 2000
	tree := map[string]string{}
	for i := range files {
		tree[fmt.Sprintf("site%04d.conf", i)] = "server {\n    WARN;\n}\n"
	}
	writeTree(t, f.dir, tree)
	c.Controls = map[string]any{"warnLines": true}
	writeSidecar(t, f.nginx, c)
	s := startOps(t, f, nil)

	op := s.wait(s.startTest().ID, 60*time.Second)
	r := testResultOf(t, op)
	if len(r.Output) != 65536 || !r.OutputTruncated || !r.Valid {
		t.Errorf("output %d bytes, truncated %v, valid %v; want 65536, true, true", len(r.Output), r.OutputTruncated, r.Valid)
	}
	l := s.log(op.ID)
	if n := strings.Count(l, "nginx: [warn] "); n != files || !strings.Contains(l, plainSuccess) {
		t.Errorf("operation log has %d [warn] lines (want %d) and success lines %v", n, files, strings.Contains(l, plainSuccess))
	}
	if len(l) <= TestOutputCap {
		t.Errorf("operation log %d bytes, want all of nginx's output, over 64 KiB", len(l))
	}
}

// nginx-provider TC-ERR12: A config test that does not finish is dependency-unavailable
// Owner WI 5.1. Requirements: FR-4, NFR-2, FR-17.
func TestConfigTestTimeout(t *testing.T) {
	f, _ := helperFixture(t, false)
	writeTree(t, f.dir, map[string]string{"a.conf": "server {\n    HANG;\n}\n"})
	s := startOps(t, f, func(c *config.NginxConfig) { c.TestTimeout = time.Second })

	op := s.wait(s.startTest().ID, 30*time.Second)
	if d := failedWith(t, op, "dependency-unavailable"); d != "nginx -t did not finish within 1s" {
		t.Errorf("detail %q", d)
	}
	if op.Started == nil || op.Ended == nil || op.Ended.Sub(*op.Started) > 3*time.Second {
		t.Errorf("ran from %v to %v, want at most 3s", op.Started, op.Ended)
	}
	if n := fakeProcesses(t, f.nginx); n != 0 {
		t.Errorf("%d nginx processes left after the bound", n)
	}
	if strings.Contains(string(op.Result), "valid") {
		t.Errorf("result %s has a valid field", op.Result)
	}
}

// nginx-provider TC-ERR13: A missing nginx binary fails the test op rather than reporting it invalid
// Owner WI 5.1. Requirements: FR-4, FR-17.
func TestConfigTestMissingBinary(t *testing.T) {
	f, _ := helperFixture(t, true)
	s := startOps(t, f, nil)

	op := s.wait(s.startTest().ID, 30*time.Second)
	if d := failedWith(t, op, "dependency-unavailable"); !strings.Contains(d, f.nginx) {
		t.Errorf("detail %q does not name %s", d, f.nginx)
	}
}

// nginx-provider TC-ERR14: A helper that cannot write a result yields outcome-unknown with a lost detail
// Owner WI 5.1. Requirements: NFR-2, FR-17.
//
// The hook blocks result.json's temporary name with a directory instead of
// making the operation's directory 0500 (the test case's setup): the
// launcher writes spec.json into that directory after Prepare and the
// supervisor unlinks it, so a read-only directory would fail the launch,
// not the helper's write, and root writes through mode bits anyway.
func TestHelperWithoutResult(t *testing.T) {
	f, _ := helperFixture(t, false)
	f.p.afterPrepare = func(dir string) {
		if err := os.Mkdir(filepath.Join(dir, "."+resultFile+".tmp"), 0o700); err != nil {
			t.Error(err)
		}
	}
	s := startOps(t, f, nil)

	op := s.wait(s.startTest().ID, 30*time.Second)
	d := failedWith(t, op, "outcome-unknown")
	if !strings.Contains(d, "action test") || !strings.Contains(d, "exited with code 1") {
		t.Errorf("detail %q, want the action test and the helper's non-zero exit", d)
	}
	if _, err := os.Stat(filepath.Join(s.opsDir, op.ID, resultFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("result.json: %v, want none", err)
	}
	if d := f.p.lostDetail(actionTest, provider.NewRun(op.ID, nil)); !strings.Contains(d, "action test") ||
		strings.Contains(d, "previous content") {
		t.Errorf("lost detail %q, want the action and no backup", d)
	}

	// Once a change has backed up a file, the lost detail says where its
	// previous content is.
	prev := filepath.Join(s.opsDir, op.ID, "previous")
	if err := os.Mkdir(prev, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("ab", 32)
	if err := writeJSONAtomic(prev, "meta.json", map[string]any{"fileName": "example.com.conf", "exists": true,
		"sha256": digest, "size": 10, "mode": 0o644, "uid": 0, "gid": 0}); err != nil {
		t.Fatal(err)
	}
	d = f.p.lostDetail(actionTest, provider.NewRun(op.ID, nil))
	if want := "example.com.conf may be partly changed: its previous content (sha256 " + digest + ") is kept at " +
		filepath.Join(prev, "content"); !strings.Contains(d, "action test") || !strings.Contains(d, want) {
		t.Errorf("lost detail %q, want %q", d, want)
	}
}
