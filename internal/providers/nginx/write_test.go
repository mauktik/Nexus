// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Add and update (WI 6.3) reuse the toggle tests' harness (toggle_test.go,
// Linux-only): the helper runs in-process with the svcctl fake as the init
// system, a fake /proc replaying a captured reload in lockstep with it, and
// fakenginx as nginx -t. A disabled target and whatIf use the shadow tester:
// fakeShadow below in non-root runs, the real one (shadow_linux.go) as root.

package nginx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	nexusserver "github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
	"github.com/mauktik/nexus/internal/systemspec"
)

func setManagedGID(t *testing.T, gid uint32) {
	t.Helper()
	old := managedGID
	managedGID = gid
	t.Cleanup(func() { managedGID = old })
}

// fakeShadow is the shadow tester of non-root runs. It implements
// ShadowTester as shadowBin does, without a mount namespace: it copies the
// regular files of includeDir's top level into a directory of its own,
// places the candidate there under the presented name, and runs a second
// fakenginx whose sidecar points at that copy. Its verdict is fakenginx's,
// it never writes includeDir, and its -t runs are in the second fakenginx's
// call log, never the live one's.
type fakeShadow struct {
	inc   string // The live includeDir
	copy  string // The shadow copy
	nginx string // The second fakenginx, over copy

	mu    sync.Mutex
	names []string // The presented names, in call order
}

func newFakeShadow(t *testing.T, inc string) *fakeShadow {
	t.Helper()
	root := t.TempDir()
	s := &fakeShadow{inc: inc, copy: filepath.Join(root, "shadow"), nginx: filepath.Join(root, "bin", "nginx")}
	for _, d := range []string{s.copy, filepath.Dir(s.nginx)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	installFakeNginx(t, s.nginx, fakeDirs(t, root, s.copy))
	return s
}

func (s *fakeShadow) Test(ctx context.Context, presentedName, candidatePath string) (TestOutput, error) {
	s.mu.Lock()
	s.names = append(s.names, presentedName)
	s.mu.Unlock()
	if err := os.RemoveAll(s.copy); err != nil {
		return TestOutput{}, err
	}
	if err := os.Mkdir(s.copy, 0o755); err != nil {
		return TestOutput{}, err
	}
	entries, err := os.ReadDir(s.inc)
	if err != nil {
		return TestOutput{}, err
	}
	for _, e := range entries {
		if e.Name() == presentedName || !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.inc, e.Name()))
		if err != nil {
			return TestOutput{}, err
		}
		if err := os.WriteFile(filepath.Join(s.copy, e.Name()), b, 0o644); err != nil {
			return TestOutput{}, err
		}
	}
	b, err := os.ReadFile(candidatePath)
	if err != nil {
		return TestOutput{}, err
	}
	if err := os.WriteFile(filepath.Join(s.copy, presentedName), b, 0o644); err != nil {
		return TestOutput{}, err
	}
	return nginxBin{path: s.nginx, testTimeout: 30 * time.Second}.Test(ctx)
}

// presented are the names the shadow tests were run with.
func (s *fakeShadow) presented() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.names)
}

// writeRig is the lifecycle harness with the test's files managed (owner
// and group), and the shadow tester the helper gets.
type writeRig struct {
	*lcHarness
	fake   *fakeShadow
	shadow ShadowTester              // Nil leaves the helper the real one
	tune   func(*config.NginxConfig) // Applied to the section the daemon configures
}

func newWriteLC(t *testing.T, plat platform.Info, m svcctltest.Model) *writeRig {
	t.Helper()
	h := newToggleLC(t, plat, m)
	setManagedGID(t, uint32(os.Getgid()))
	fs := newFakeShadow(t, h.f.dir)
	return &writeRig{lcHarness: h, fake: fs, shadow: fs}
}

// useRealShadow gives the helper shadow_linux.go's tester, which runs a
// built nexusd's shadow-test child. It needs root on Linux.
func (w *writeRig) useRealShadow(t *testing.T) {
	t.Helper()
	requireShadowRoot(t)
	exe, err := detachtest.Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	old := shadowExe
	shadowExe = exe
	t.Cleanup(func() { shadowExe = old })
	w.shadow = nil
}

// build is runHelper's env builder: lcHarness's, with w's shadow tester.
func (w *writeRig) build(in helperInput, out io.Writer) (env, error) {
	e, err := w.lcHarness.build(in, out)
	if err != nil {
		return env{}, err
	}
	if w.shadow != nil {
		e.shadow = w.shadow
	}
	return e, nil
}

// blockFirstTest makes the live fakenginx's first -t block on a FIFO until
// the gate is released.
func (w *writeRig) blockFirstTest(t *testing.T) *fifoGate {
	t.Helper()
	g := &fifoGate{t: t, path: filepath.Join(t.TempDir(), "block.fifo"), nginx: w.f.nginx}
	if err := syscall.Mkfifo(g.path, 0o600); err != nil {
		t.Fatal(err)
	}
	c := fakeDirs(t, filepath.Dir(filepath.Dir(w.f.nginx)), w.f.dir)
	c.Controls = map[string]any{"blockFifo": g.path, "blockCount": 1}
	writeSidecar(t, w.f.nginx, c)
	t.Cleanup(g.cleanup)
	return g
}

// fifoGate is a FIFO that blocks fakenginx's first -t.
type fifoGate struct {
	t        *testing.T
	path     string
	nginx    string
	released bool
}

// blocked waits until the -t is blocked on the FIFO.
func (g *fifoGate) blocked() { waitFile(g.t, g.nginx+".json.blockcount", 30*time.Second) }

func (g *fifoGate) release() {
	g.t.Helper()
	releaseFifo(g.t, g.path)
	g.released = true
}

// cleanup releases a -t that is still blocked, so the helper that ran it
// ends before the test does. It never blocks on a FIFO with no reader.
func (g *fifoGate) cleanup() {
	deadline := time.Now().Add(5 * time.Second)
	for !g.released && time.Now().Before(deadline) {
		f, err := os.OpenFile(g.path, os.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			_, werr := f.Write([]byte{1})
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				g.t.Errorf("release %s: %v", g.path, werr)
			}
			g.released = true
			return
		}
		if !errors.Is(err, unix.ENXIO) {
			g.t.Errorf("release %s: %v", g.path, err)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// writeOutcome is one add or update run through its binding.
type writeOutcome struct {
	res     spec.WriteResult
	err     *problem.Error // Nil when the operation succeeded
	refused bool           // Admit refused it: no operation exists
	dir     string         // The operation's directory
	log     string
}

// write runs o, add or update, with input in, as the dispatcher and the
// supervisor would, with the helper in-process: Admit, Prepare, the helper,
// Finalize. As lcHarness.toggle, the calls Admit made are set aside.
func (w *writeRig) write(o def.Op, in any) writeOutcome {
	t := w.t
	t.Helper()
	b, d := w.binding(o)
	ctx := context.Background()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := b.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	w.ops++
	if err := d.Admit(ctx, dec); err != nil {
		var p *problem.Error
		if !errors.As(err, &p) {
			t.Fatalf("%s: Admit: %v, want a problem", o.ID, err)
		}
		return writeOutcome{err: p, refused: true}
	}
	w.mu.Lock()
	w.admitEvents, w.events = w.events, nil
	afterAdmit := w.afterAdmit
	w.mu.Unlock()
	if afterAdmit != nil {
		afterAdmit()
	}
	var log syncBuffer
	r := provider.NewRun(fmt.Sprintf("op%d", w.ops), &log)
	sp, err := d.Prepare(ctx, dec, r)
	if err != nil {
		t.Fatalf("%s: Prepare: %v", o.ID, err)
	}
	var stderr bytes.Buffer
	if code := runHelper(ctx, sp.Dir, &log, &stderr, w.build); code != 0 {
		t.Fatalf("%s: helper exited %d: %s", o.ID, code, stderr.String())
	}
	out, err := d.Finalize(ctx, dec, detach.Exit{}, r)
	res := writeOutcome{dir: sp.Dir, log: log.String()}
	if err != nil {
		if !errors.As(err, &res.err) {
			t.Fatalf("%s: Finalize: %v, want a problem", o.ID, err)
		}
		return res
	}
	res.res = out.(spec.WriteResult)
	return res
}

func (o writeOutcome) succeeded(t *testing.T) spec.WriteResult {
	t.Helper()
	if o.err != nil {
		t.Fatalf("operation failed: %v (refused %v)\nlog:\n%s", o.err, o.refused, o.log)
	}
	return o.res
}

// failed requires the operation to have run and failed with slug, and
// returns its detail.
func (o writeOutcome) failed(t *testing.T, slug problem.Slug) string {
	t.Helper()
	if o.refused {
		t.Fatalf("Admit refused the operation with %v; want an operation that fails %s", o.err, slug)
	}
	if o.err == nil || o.err.Slug() != slug {
		t.Fatalf("operation ended %v, result %+v; want Failed %s\nlog:\n%s", o.err, o.res, slug, o.log)
	}
	return o.err.Detail
}

func boolp(b bool) *bool { return &b }

// fileState is an entry's bytes, mode and owner: what a restore must give
// back. A restore writes a new inode, so the inode is not part of it.
type fileState struct {
	mode     fs.FileMode
	uid, gid uint32
	sum      string
}

// contentSnapshot is every entry of dir, dotfiles included, with its
// fileState: includeDir as NFR-3 compares it after a rolled-back write.
func contentSnapshot(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	snap := map[string]fileState{}
	for name, id := range ownedSnapshot(t, dir) {
		snap[name] = fileState{mode: id.mode, uid: id.uid, gid: id.gid, sum: id.sum}
	}
	return snap
}

// checkWritten requires path to be a regular file holding content at mode
// 0644, owned by the managed owner and group (root, or the test's own).
func checkWritten(t *testing.T, path, content string) {
	t.Helper()
	id := idOf(t, path)
	if id.sum != sha(content) || id.mode != 0o644 || id.uid != managedUID || id.gid != managedGID {
		t.Errorf("%s is %+v, want sha256 %s, mode 0644, owner %d:%d", path, id, sha(content), managedUID, managedGID)
	}
}

func readMeta(t *testing.T, opDir string) Meta {
	t.Helper()
	var m Meta
	if ok, err := readJSON(opDir, previousMetaFile, &m); err != nil || !ok {
		t.Fatalf("%s: %v, %v", previousMetaFile, ok, err)
	}
	return m
}

// noCall requires the svcctl fake to have recorded none of verbs.
func (w *writeRig) noCall(t *testing.T, verbs ...string) {
	t.Helper()
	for _, c := range w.f.svc.Calls() {
		if slices.Contains(verbs, initCall(c.Argv)) {
			t.Errorf("the svcctl fake recorded %q", c.Argv)
		}
	}
}

// liveTests counts the live fakenginx's -t runs.
func (w *writeRig) liveTests() int { return w.nginxTests() }

// wLauncher is detach's process seams with each operation's helper run in
// a goroutine with w's env, as inprocLauncher does, its output written to
// the operation log as the supervisor writes it. It outlives a daemon,
// as a supervisor outlives nexusd, so a restarted daemon finds its units.
// A unit marked lost answers as gone and writes no exit.json, as a killed
// supervisor would.
type wLauncher struct {
	w    *writeRig
	mu   sync.Mutex
	done map[string]chan struct{}   // By unit; closed when the helper ended
	lost map[string]bool            // Units reported gone
	spec map[string]json.RawMessage // spec.json as Launch saw it, by unit
	wg   sync.WaitGroup
}

func (l *wLauncher) launch(_ context.Context, inv detach.Invocation) error {
	b, err := os.ReadFile(filepath.Join(inv.Dir, detach.SpecFile))
	if err != nil {
		return err
	}
	ch := make(chan struct{})
	l.mu.Lock()
	l.done[inv.Unit], l.spec[inv.Unit] = ch, b
	l.mu.Unlock()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer close(ch)
		started := time.Now()
		var out io.Writer = io.Discard
		log, err := oplog.Open(filepath.Join(inv.Dir, detach.OutputDir), oplog.Options{})
		if err != nil {
			l.w.t.Errorf("output log of %s: %v", inv.Unit, err)
		} else {
			out = &lockedWriter{w: log.Stream(apitypes.StreamStdout)}
		}
		var stderr syncBuffer
		code := runHelper(context.Background(), inv.Dir, out, &stderr, l.w.build)
		if log != nil {
			if err := log.Close(); err != nil {
				l.w.t.Errorf("output log of %s: %v", inv.Unit, err)
			}
		}
		l.mu.Lock()
		lost := l.lost[inv.Unit]
		l.mu.Unlock()
		if lost {
			return
		}
		if code != 0 {
			l.w.t.Errorf("helper for %s exited %d: %s", inv.Unit, code, stderr.String())
		}
		if err := detach.WriteExit(inv.Dir, detach.Exit{ExitCode: code, Started: started, Ended: time.Now()}); err != nil {
			l.w.t.Errorf("exit.json for %s: %v", inv.Unit, err)
		}
	}()
	return nil
}

// lockedWriter serializes writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (l *wLauncher) state(_ context.Context, unit string) (detach.UnitState, error) {
	l.mu.Lock()
	ch, ok := l.done[unit]
	lost := l.lost[unit]
	l.mu.Unlock()
	if ok && !lost {
		select {
		case <-ch:
		default:
			return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
		}
	}
	return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
}

func (l *wLauncher) signal(context.Context, string, syscall.Signal) error {
	return errors.New("the in-process helper cannot be signalled")
}

// kill makes operation id's unit gone, as SIGKILL of its supervisor and
// helper would: it reports inactive and never writes exit.json.
func (l *wLauncher) kill(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lost[detach.Unit(id)] = true
}

func (l *wLauncher) specOf(id string) json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spec[detach.Unit(id)]
}

// writeDaemon is nexusd's components over one state directory, with
// detached operations run by a wLauncher, and a token of nginx's three
// scopes. restart closes them and opens new ones on the same state, as
// daemon.New and Run would; the internal/daemon package itself imports
// this one, so its tests cannot import it.
type writeDaemon struct {
	t        *testing.T
	w        *writeRig
	l        *wLauncher
	stateDir string
	auditDir string
	opsDir   string
	token    string
	s        *opsServer
	shut     func()
}

func startWriteDaemon(t *testing.T, w *writeRig) *writeDaemon {
	t.Helper()
	d := &writeDaemon{t: t, w: w, stateDir: t.TempDir(), auditDir: t.TempDir(),
		l: &wLauncher{w: w, done: map[string]chan struct{}{}, lost: map[string]bool{},
			spec: map[string]json.RawMessage{}}}
	d.opsDir = filepath.Join(d.stateDir, "ops")
	tok, _, err := auth.Create(d.stateDir, auth.CreateRequest{Name: "configurer",
		Scopes: []string{spec.ReadScope, spec.WriteScope, spec.ConfigScope}})
	if err != nil {
		t.Fatal(err)
	}
	d.token = tok
	d.open()
	t.Cleanup(func() {
		if d.shut != nil {
			d.shut()
		}
		d.l.wg.Wait()
	})
	return d
}

func (d *writeDaemon) open() {
	t := d.t
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	alog, err := audit.Open(audit.Options{Dir: d.auditDir, Host: "h1", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(d.stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := detach.New(detach.Config{OpsDir: d.opsDir, Executable: "/usr/sbin/nexusd", Logger: log},
		detach.Options{Launch: d.l.launch, UnitState: d.l.state, Signal: d.l.signal,
			ListUnits: func(context.Context) ([]string, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: db, Audit: alog, Dir: d.opsDir, Host: "h1", Logger: log,
		PollInterval: 20 * time.Millisecond, Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	p := d.w.f.p
	p.UseOpsDir(d.opsDir)
	decode := func(into any) error {
		if err := d.w.f.decode(into); err != nil {
			return err
		}
		if d.w.tune != nil {
			d.w.tune(into.(*config.NginxConfig))
		}
		return nil
	}
	reg, err := core.New([]provider.Provider{p}, core.Options{Host: "h1", Version: "v0.0.0-test", Logger: log,
		Audit: alog, Ops: mgr, Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: decode}},
		Platform: p.plat})
	if err != nil {
		t.Fatal(err)
	}
	bindings := map[string]provider.Binding{}
	for _, b := range p.Bindings() {
		bindings[b.Op().ID] = b
	}
	if err := mgr.Restore(func(id string) (provider.Binding, bool) { b, ok := bindings[id]; return b, ok }); err != nil {
		t.Fatal(err)
	}
	srv, err := nexusserver.New(reg, nexusserver.Options{Logger: log,
		Auth: auth.New(d.stateDir, auth.Options{Logger: log, Audit: alog, FailureLimitPerMinute: 100000})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	mgr.Recover()
	d.s = &opsServer{t: t, url: "http://" + ln.Addr().String(), token: d.token, auditDir: d.auditDir,
		opsDir: d.opsDir, http: &http.Client{Transport: &http.Transport{}, Timeout: 30 * time.Second}}
	s := d.s
	d.shut = func() {
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
	}
}

// restart closes the daemon's components and opens new ones on the same
// state directory, audit log and operations directory. Units keep running.
func (d *writeDaemon) restart() {
	d.t.Helper()
	d.shut()
	d.shut = nil
	d.open()
}

// sitePath is op's route for site name, with the name escaped byte by byte
// (dots included) so the route carries it as given.
func sitePath(t *testing.T, o def.Op, name string) string {
	t.Helper()
	rt, err := nexusserver.RouteFor(o)
	if err != nil {
		t.Fatal(err)
	}
	var esc strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isAlnum(c) || c == '-' || c == '_' {
			esc.WriteByte(c)
		} else {
			fmt.Fprintf(&esc, "%%%02X", c)
		}
	}
	return strings.Replace(strings.TrimPrefix(rt.Path, "/v1"), "{name}", esc.String(), 1)
}

// call sends o on site name with the raw JSON body (none for a GET).
func (d *writeDaemon) call(o def.Op, name string, body []byte) (int, []byte) {
	d.t.Helper()
	rt, err := nexusserver.RouteFor(o)
	if err != nil {
		d.t.Fatal(err)
	}
	var r io.Reader
	if rt.Method != http.MethodGet {
		r = bytes.NewReader(body)
	}
	return d.s.do(rt.Method, sitePath(d.t, o, name), r)
}

func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// accept POSTs o on site name with body and returns the accepted operation.
func (d *writeDaemon) accept(o def.Op, name string, body map[string]any) apitypes.Operation {
	d.t.Helper()
	code, b := d.call(o, name, jsonBody(d.t, body))
	if code != http.StatusAccepted {
		d.t.Fatalf("POST sites/%s/%s = %d %s, want 202", name, o.Action, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		d.t.Fatalf("%s: %v", b, err)
	}
	return op
}

// refused POSTs o on site name with body and requires status code and
// problem slug; it returns the problem.
func (d *writeDaemon) refused(o def.Op, name string, body []byte, code int, slug problem.Slug) problem.Error {
	d.t.Helper()
	got, b := d.call(o, name, body)
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil {
		d.t.Fatalf("%s sites/%s: %d %s: %v", o.Action, name, got, b, err)
	}
	if got != code || p.Slug() != slug {
		d.t.Errorf("%s sites/%q = %d %s, want %d %s", o.Action, name, got, b, code, slug)
	}
	return p
}

// operations is how many operations exist.
func (d *writeDaemon) operations() int {
	d.t.Helper()
	code, b := d.s.do(http.MethodGet, "/operations", nil)
	var list apitypes.List[apitypes.Operation]
	if code != http.StatusOK || json.Unmarshal(b, &list) != nil {
		d.t.Fatalf("GET /operations = %d %s", code, b)
	}
	return len(list.Items)
}

func writeResultOf(t *testing.T, op apitypes.Operation) spec.WriteResult {
	t.Helper()
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s: %s, error %+v", op.ID, op.Status, op.Error)
	}
	var r spec.WriteResult
	if err := decodeStrict(op.Result, &r); err != nil {
		t.Fatalf("result %s: %v", op.Result, err)
	}
	return r
}

// auditOutcomesOf are the outcomes of every audit record of op.
func (d *writeDaemon) auditOutcomesOf(o def.Op) []string {
	d.t.Helper()
	var out []string
	for _, r := range d.s.auditRecords(o.ID) {
		out = append(out, r.Outcome)
	}
	return out
}

// nginx-provider TC-96: Add creates a new enabled config, tests and reloads
// Owner WI 6.3. Requirements: FR-11.
func TestAddCreatesEnabledConfig(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			w := newWriteLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
			p := w.useCapture(m.capture+"-valid.jsonl", true, w.f.nginx)
			d := startWriteDaemon(t, w)
			const c = "server {\n    listen 8191;\n}\n"

			op := d.s.wait(d.accept(spec.Add.Op(), "new", map[string]any{"content": c}).ID, 30*time.Second)
			r := writeResultOf(t, op)
			want := spec.WriteResult{Name: "new", FileName: "new.conf", SHA256: sha(c), SizeBytes: int64(len(c)),
				Created: true, Enabled: true, Reloaded: true}
			if r.Name != want.Name || r.FileName != want.FileName || r.SHA256 != want.SHA256 ||
				r.SizeBytes != want.SizeBytes || !r.Created || !r.Enabled || !r.Reloaded || r.WhatIf || r.Valid != nil {
				t.Errorf("result %+v, want %+v", r, want)
			}
			if r.PreviousSHA256 != "" || bytes.Contains(op.Result, []byte("previousSha256")) {
				t.Errorf("result %s has a previousSha256", op.Result)
			}
			if r.Reload == nil || r.Reload.Verdict != spec.VerdictConfirmed || r.Reload.MasterPID != p.master {
				t.Errorf("reload %+v, want confirmed under master %d", r.Reload, p.master)
			}
			checkWritten(t, filepath.Join(w.f.dir, "new.conf"), c)
			opDir := filepath.Join(d.opsDir, op.ID)
			absent(t, filepath.Join(opDir, "content"))
			if meta := readMeta(t, opDir); meta.Exists || meta.FileName != "new.conf" || meta.SHA256 != "" {
				t.Errorf("previous/meta.json %+v, want new.conf recorded as not existing", meta)
			}
			absent(t, filepath.Join(opDir, "previous", "content"))

			// Update of a name with neither file creates it, enabled.
			w.set(func(h *lcHarness) { h.events = nil })
			w.useCapture(m.capture+"-valid.jsonl", true, w.f.nginx)
			const dc = "server {\n    listen 8192;\n}\n"
			op = d.s.wait(d.accept(spec.Update.Op(), "fresh", map[string]any{"content": dc}).ID, 30*time.Second)
			r = writeResultOf(t, op)
			if r.Name != "fresh" || r.FileName != "fresh.conf" || r.SHA256 != sha(dc) ||
				r.SizeBytes != int64(len(dc)) || !r.Created || !r.Enabled || !r.Reloaded || r.PreviousSHA256 != "" {
				t.Errorf("update result %+v, want fresh.conf created, enabled and reloaded", r)
			}
			checkWritten(t, filepath.Join(w.f.dir, "fresh.conf"), dc)
			absent(t, filepath.Join(w.f.dir, "fresh.conf.disabled"))
			if got, want := withoutStatus(w.calls()), []string{"nginx -t", "reload"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q", got, want)
			}
		})
	}
}

// nginx-provider TC-97: Update overwrites with a matching ifMatch
// Owner WI 6.3. Requirements: FR-11, ASM-9.
func TestUpdateWithIfMatch(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			w := newWriteLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
			const pc, nc = "server {\n    listen 8193;\n}\n", "server {\n    listen 8194;\n}\n"
			writeTree(t, w.f.dir, map[string]string{"a.conf": pc})
			chmodFile(t, filepath.Join(w.f.dir, "a.conf"), 0o640)
			w.useCapture(m.capture+"-valid.jsonl", true, w.f.nginx)
			d := startWriteDaemon(t, w)

			op := d.s.wait(d.accept(spec.Update.Op(), "a", map[string]any{"content": nc, "ifMatch": sha(pc)}).ID,
				30*time.Second)
			r := writeResultOf(t, op)
			checkWritten(t, filepath.Join(w.f.dir, "a.conf"), nc)
			if r.PreviousSHA256 != sha(pc) || r.Created || !r.Reloaded || !r.Enabled || r.FileName != "a.conf" {
				t.Errorf("result %+v, want previousSha256 %s, not created, reloaded", r, sha(pc))
			}
			if b, err := os.ReadFile(filepath.Join(d.opsDir, op.ID, "previous", "content")); err != nil ||
				string(b) != pc {
				t.Errorf("previous/content %q (%v), want %q", b, err, pc)
			}

			// Without ifMatch the write is unconditional.
			w.useCapture(m.capture+"-valid.jsonl", true, w.f.nginx)
			const n2 = "server {\n    listen 8195;\n}\n"
			op = d.s.wait(d.accept(spec.Update.Op(), "a", map[string]any{"content": n2}).ID, 30*time.Second)
			if r := writeResultOf(t, op); r.PreviousSHA256 != sha(nc) {
				t.Errorf("result %+v, want previousSha256 %s", r, sha(nc))
			}
			checkWritten(t, filepath.Join(w.f.dir, "a.conf"), n2)

			// nginx stopped in the fake's model, not through Nexus: the
			// write is tested and not reloaded, and nothing starts nginx.
			w.f.svc.Set(func(m *svcctltest.Model) { m.State = svcctl.Stopped })
			w.set(func(h *lcHarness) { h.events = nil })
			before := len(w.f.svc.Calls())
			const sc = "server {\n    listen 8196;\n}\n"
			op = d.s.wait(d.accept(spec.Update.Op(), "a", map[string]any{"content": sc}).ID, 30*time.Second)
			r = writeResultOf(t, op)
			if !r.Enabled || r.Reloaded || r.Reload != nil {
				t.Errorf("result %+v, want enabled and not reloaded", r)
			}
			checkWritten(t, filepath.Join(w.f.dir, "a.conf"), sc)
			if got, want := w.calls(), []string{"status", "nginx -t"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q", got, want)
			}
			for _, c := range w.f.svc.Calls()[before:] {
				if v := initCall(c.Argv); v != "status" && v != "exists" {
					t.Errorf("init call %q while nginx is stopped, want status only", c.Argv)
				}
			}
			if st := w.f.svc.Model().State; st != svcctl.Stopped {
				t.Errorf("nginx is %s in the fake's model, want still stopped", st)
			}
		})
	}
}

// nginx-provider TC-98: Update of a disabled config validates it as if enabled, and does not reload
// Owner WI 6.3. Requirements: FR-11, ASM-10.
//
// The fake shadow half runs everywhere; the real shadow half (a mount
// namespace) needs root and skips otherwise, saying so.
func TestUpdateDisabledTargetShadowValidated(t *testing.T) {
	fakeNginxBinary(t)
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprintf("real shadow=%v", real), func(t *testing.T) {
			w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
			if real {
				w.useRealShadow(t)
			}
			writeTree(t, w.f.dir, map[string]string{"b.conf.disabled": validSite, "a.conf": useSite,
				"up.conf": upstreamSite})
			const v2 = "server {\n    listen 8197;\n}\n"

			o := w.write(spec.Update.Op(), spec.WriteInput{Name: "b", Content: v2})
			r := o.succeeded(t)
			checkWritten(t, filepath.Join(w.f.dir, "b.conf.disabled"), v2)
			absent(t, filepath.Join(w.f.dir, "b.conf"))
			if r.Reloaded || r.Enabled || r.FileName != "b.conf.disabled" || r.Created {
				t.Errorf("result %+v, want b.conf.disabled, not enabled, not reloaded", r)
			}
			if !strings.Contains(o.log, "shadow test with the candidate as b.conf\n") {
				t.Errorf("log does not show a shadow test of b.conf:\n%s", o.log)
			}
			// The real shadow runs the configured fakenginx inside its
			// namespace, so its runs share the call log with live ones; the
			// helper's own calls show that none was live.
			if calls := w.calls(); len(calls) != 0 {
				t.Errorf("calls %q, want none: no live nginx -t, no master and no reload", calls)
			}
			if !real {
				if got := w.fake.presented(); !slices.Equal(got, []string{"b.conf"}) {
					t.Errorf("shadow tests presented %q, want [b.conf]", got)
				}
				for _, c := range readFakeCalls(t, w.f.nginx) {
					if slices.ContainsFunc(c.IncludedFiles, func(f string) bool { return filepath.Base(f) == "b.conf" }) {
						t.Errorf("a live nginx -t included b.conf: %+v", c)
					}
				}
			}
			w.noCall(t, "reload")

			o = w.write(spec.Update.Op(), spec.WriteInput{Name: "b", Content: invalidSite})
			o.failed(t, problem.NginxConfigInvalid)
			if !ext[bool](t, o.err, "rolledBack") {
				t.Errorf("rolledBack false, want true")
			}
			if out := ext[string](t, o.err, "testOutput"); !strings.Contains(out, `unknown directive "INVALID"`) {
				t.Errorf("testOutput %q, want the unknown directive", out)
			}
			checkWritten(t, filepath.Join(w.f.dir, "b.conf.disabled"), v2)
			absent(t, filepath.Join(w.f.dir, "b.conf"))
			if calls := w.calls(); len(calls) != 0 {
				t.Errorf("calls %q, want none", calls)
			}
			if n := w.liveTests(); !real && n != 0 {
				t.Errorf("the live fakenginx ran -t %d times, want none", n)
			}
			w.noCall(t, "reload")
		})
	}
}

// nginx-provider TC-99: whatIf validates without touching includeDir or reloading
// Owner WI 6.3. Requirements: FR-11.
func TestWhatIfChangesNothing(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
	before := ownedSnapshot(t, w.f.dir)
	d := startWriteDaemon(t, w)
	const valid2 = "server {\n    listen 8198;\n}\n"

	type step struct {
		op    def.Op
		name  string
		body  map[string]any
		valid bool
	}
	for _, s := range []step{
		{spec.Update.Op(), "a", map[string]any{"content": valid2, "whatIf": true}, true},
		{spec.Update.Op(), "a", map[string]any{"content": invalidSite, "whatIf": true}, false},
		{spec.Add.Op(), "z", map[string]any{"content": valid2, "whatIf": true}, true},
	} {
		op := d.s.wait(d.accept(s.op, s.name, s.body).ID, 30*time.Second)
		r := writeResultOf(t, op)
		if !r.WhatIf || r.Valid == nil || *r.Valid != s.valid || r.Reloaded || r.Created || r.Changed {
			t.Errorf("%s %s: result %s, want whatIf, valid %v, nothing changed", s.op.Action, s.name, op.Result, s.valid)
		}
		if !s.valid && !strings.Contains(r.TestOutput, `unknown directive "INVALID"`) {
			t.Errorf("%s %s: testOutput %q, want nginx -t's failure", s.op.Action, s.name, r.TestOutput)
		}
		if after := ownedSnapshot(t, w.f.dir); !maps.Equal(before, after) {
			t.Errorf("%s %s: includeDir %v, was %v", s.op.Action, s.name, after, before)
		}
		absent(t, filepath.Join(d.opsDir, op.ID, "previous"))
		absent(t, filepath.Join(d.opsDir, op.ID, "content"))
	}
	if got, want := w.fake.presented(), []string{"a.conf", "a.conf", "z.conf"}; !slices.Equal(got, want) {
		t.Errorf("shadow tests presented %q, want %q", got, want)
	}
	w.noCall(t, "reload")
	if n := w.liveTests(); n != 0 {
		t.Errorf("the live fakenginx ran -t %d times, want none", n)
	}
	for _, o := range []def.Op{spec.Update.Op(), spec.Add.Op()} {
		if outcomes := d.auditOutcomesOf(o); slices.Contains(outcomes, audit.OutcomeRequested) {
			t.Errorf("audit outcomes of %s %q: whatIf is not audited", o.ID, outcomes)
		}
	}
}

// nginx-provider TC-100: Content is redacted in the audit and records, and its digest is in the op log
// Owner WI 6.3. Requirements: FR-15, L-39.
func TestContentRedactionAndDigestLine(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
	gate := w.blockFirstTest(t)
	d := startWriteDaemon(t, w)
	const marker = "nexus-secret-7c1e"
	c := "server {\n    listen 8199;\n    # " + marker + "\n}\n"

	op := d.accept(spec.Update.Op(), "a", map[string]any{"content": c})
	gate.blocked()
	contentPath := filepath.Join(d.opsDir, op.ID, "content")
	fi, err := os.Lstat(contentPath)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("content while the op runs: %v, %v; want a 0600 regular file", fi, err)
	}
	gate.release()
	op = d.s.wait(op.ID, 30*time.Second)
	r := writeResultOf(t, op)
	if r.SHA256 != sha(c) {
		t.Errorf("result sha256 %s, want %s", r.SHA256, sha(c))
	}
	absent(t, contentPath)

	var requested []systemspec.AuditRecord
	for _, rec := range d.s.auditRecords(spec.Update.Op().ID) {
		if rec.Outcome == audit.OutcomeRequested {
			requested = append(requested, rec)
		}
	}
	if len(requested) != 1 {
		t.Fatalf("%d requested records, want 1", len(requested))
	}
	var params map[string]any
	if err := json.Unmarshal(requested[0].Params, &params); err != nil || params["content"] != "[REDACTED]" ||
		requested[0].Target != "a" {
		t.Errorf("requested record target %q params %s (%v), want target a and content [REDACTED]",
			requested[0].Target, requested[0].Params, err)
	}
	if line := fmt.Sprintf("content: %d bytes, sha256 %s\n", len(c), sha(c)); !strings.Contains(d.s.log(op.ID), line) {
		t.Errorf("operation log %q has no %q", d.s.log(op.ID), line)
	}
	_, opJSON := d.s.do(http.MethodGet, "/operations/"+op.ID, nil)
	if bytes.Contains(opJSON, []byte(marker)) {
		t.Errorf("the operation record carries the content: %s", opJSON)
	}
	// Closed, so state.db's pages are on disk.
	d.shut()
	d.shut = nil
	for _, dir := range []string{d.auditDir, d.stateDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte(marker)) {
				t.Errorf("%s holds the content's marker", filepath.Join(dir, e.Name()))
			}
		}
	}
	if _, err := os.Stat(filepath.Join(d.stateDir, store.FileName)); err != nil {
		t.Errorf("state.db: %v", err)
	}
}

// nginx-provider TC-101: Content never reaches argv or a shell
// Owner WI 6.3. Requirements: FR-12, CON-4.
//
// The helper runs in this test process (wLauncher), so this process's
// cmdline stands for the helper's, and spec.json's argv for the
// supervisor's and the helper's.
func TestContentNeverInArgv(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
	gate := w.blockFirstTest(t)
	d := startWriteDaemon(t, w)
	marker := fmt.Sprintf("pwned-%d", rand.Uint64())
	pwned := filepath.Join("/tmp", marker)
	t.Cleanup(func() { _ = os.Remove(pwned) })
	prefix := "$(touch " + pwned + "); "
	c := prefix + "\n" + validSite

	op := d.accept(spec.Update.Op(), "a", map[string]any{"content": c})
	gate.blocked()
	cmdlines, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, path := range cmdlines {
		b, err := os.ReadFile(path)
		if err != nil {
			continue // Exited
		}
		if argv0, _, _ := bytes.Cut(b, []byte{0}); string(argv0) == w.f.nginx {
			seen++
		}
		if bytes.Contains(b, []byte(marker)) || bytes.Contains(b, []byte("$(touch")) {
			t.Errorf("%s holds the content: %q", path, b)
		}
	}
	if seen != 1 {
		t.Errorf("%d blocked fakenginx processes, want 1", seen)
	}
	raw := d.l.specOf(op.ID)
	var sp detach.Spec
	if err := json.Unmarshal(raw, &sp); err != nil || len(sp.Argv) == 0 {
		t.Fatalf("spec.json %s: %v", raw, err)
	}
	if want := []string{"/usr/sbin/nexusd", HelperCommand, "run", "--dir", filepath.Join(d.opsDir, op.ID)}; !slices.Equal(sp.Argv, want) {
		t.Errorf("spec.json argv %q, want %q", sp.Argv, want)
	}
	if bytes.Contains(raw, []byte(marker)) || len(sp.Env) != 0 {
		t.Errorf("spec.json carries the content or an environment: %s", raw)
	}
	gate.release()
	op = d.s.wait(op.ID, 30*time.Second)
	if r := writeResultOf(t, op); r.SHA256 != sha(c) {
		t.Errorf("result %+v, want sha256 %s", r, sha(c))
	}
	absent(t, pwned)
}

// nginx-provider TC-102: A restarted nexusd finalizes a write, and a lost helper names the previous content
// Owner WI 6.3. Requirements: NFR-3, ASM-6, L-40.
func TestWriteAcrossRestartAndLostHelper(t *testing.T) {
	fakeNginxBinary(t)
	const pc, nc = "server {\n    listen 8201;\n}\n", "server {\n    listen 8202;\n}\n"
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost=%v", lost), func(t *testing.T) {
			w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
			writeTree(t, w.f.dir, map[string]string{"a.conf": pc})
			w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
			gate := w.blockFirstTest(t)
			d := startWriteDaemon(t, w)

			op := d.accept(spec.Update.Op(), "a", map[string]any{"content": nc})
			opDir := filepath.Join(d.opsDir, op.ID)
			waitFile(t, filepath.Join(opDir, previousMetaFile), 30*time.Second)
			gate.blocked()
			if lost {
				d.l.kill(op.ID)
			}
			d.restart()
			if !lost {
				gate.release()
			}
			op = d.s.wait(op.ID, 30*time.Second)
			if !lost {
				if r := writeResultOf(t, op); r.PreviousSHA256 != sha(pc) || !r.Reloaded {
					t.Errorf("result %+v, want reloaded with previousSha256 %s", r, sha(pc))
				}
				checkWritten(t, filepath.Join(w.f.dir, "a.conf"), nc)
			} else {
				d := failedWith(t, op, string(problem.OutcomeUnknown))
				prev := filepath.Join(opDir, "previous", "content")
				if !filepath.IsAbs(prev) || !strings.Contains(d, "a.conf") || !strings.Contains(d, prev) {
					t.Errorf("detail %q, want a.conf and %s", d, prev)
				}
			}
			if b, err := os.ReadFile(filepath.Join(opDir, "previous", "content")); err != nil || string(b) != pc {
				t.Errorf("previous/content %q (%v), want %q", b, err, pc)
			}
			if names := dirNames(t, w.f.dir); !slices.Equal(names, []string{"a.conf"}) {
				t.Errorf("includeDir holds %q, want only a.conf: the previous content is kept outside it", names)
			}
		})
	}
}

// nginx-provider TC-E29: Content size boundaries
// Owner WI 6.3. Requirements: FR-12, FR-11.
func TestContentSizeBoundaries(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Stopped})
	w.tune = func(c *config.NginxConfig) { c.MaxConfigBytes = 1024 }
	d := startWriteDaemon(t, w)
	// "€" is three bytes, the last of them byte 1024.
	exact := strings.Repeat("#", 1021) + "€"
	if len(exact) != 1024 || !strings.HasSuffix(exact, "\xe2\x82\xac") {
		t.Fatalf("exact is %d bytes", len(exact))
	}

	op := d.s.wait(d.accept(spec.Add.Op(), "e1", map[string]any{"content": exact}).ID, 30*time.Second)
	if r := writeResultOf(t, op); r.SizeBytes != 1024 {
		t.Errorf("result %+v, want 1024 bytes", r)
	}
	ops := d.operations()
	p := d.refused(spec.Add.Op(), "e2", jsonBody(t, map[string]any{"content": exact + "#"}), http.StatusBadRequest,
		problem.ValidationFailed)
	if b, _ := json.Marshal(p); !strings.Contains(string(b), "maxConfigBytes") ||
		!strings.Contains(string(b), "1025 bytes") || !strings.Contains(string(b), "1024 bytes") {
		t.Errorf("problem %s does not name maxConfigBytes in bytes", b)
	}
	if n := d.operations(); n != ops {
		t.Errorf("%d operations, want %d: the refusal created one", n, ops)
	}
	absent(t, filepath.Join(w.f.dir, "e2.conf"))
	op = d.s.wait(d.accept(spec.Add.Op(), "e3", map[string]any{"content": ""}).ID, 30*time.Second)
	if r := writeResultOf(t, op); r.SizeBytes != 0 || r.SHA256 != sha("") {
		t.Errorf("result %+v, want an empty file", r)
	}
	checkWritten(t, filepath.Join(w.f.dir, "e3.conf"), "")
}

// nginx-provider TC-E30: Add with enabled false creates a disabled config validated as enabled
// Owner WI 6.3. Requirements: FR-11, ASM-10.
func TestAddDisabled(t *testing.T) {
	fakeNginxBinary(t)
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprintf("real shadow=%v", real), func(t *testing.T) {
			w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
			if real {
				w.useRealShadow(t)
			}
			o := w.write(spec.Add.Op(), spec.AddInput{Name: "n", Content: validSite, Enabled: boolp(false)})
			r := o.succeeded(t)
			checkWritten(t, filepath.Join(w.f.dir, "n.conf.disabled"), validSite)
			absent(t, filepath.Join(w.f.dir, "n.conf"))
			if r.Reloaded || !r.Created || r.Enabled || r.FileName != "n.conf.disabled" {
				t.Errorf("result %+v, want created, not enabled, not reloaded", r)
			}
			if !strings.Contains(o.log, "shadow test with the candidate as n.conf\n") {
				t.Errorf("log does not show a shadow test of n.conf:\n%s", o.log)
			}
			if !real {
				if got := w.fake.presented(); !slices.Equal(got, []string{"n.conf"}) {
					t.Errorf("shadow tests presented %q, want [n.conf]", got)
				}
			}
			if calls := w.calls(); len(calls) != 0 {
				t.Errorf("calls %q, want none: a disabled site needs no master and no reload", calls)
			}
		})
	}
}

// nginx-provider TC-ERR32: Add on an existing name is a conflict
// Owner WI 6.3. Requirements: FR-11, FR-17.
func TestAddExistingConflict(t *testing.T) {
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf.disabled": validSite})
	before := ownedSnapshot(t, w.f.dir)
	d := startWriteDaemon(t, w)

	d.refused(spec.Add.Op(), "a", jsonBody(t, map[string]any{"content": invalidSite}), http.StatusConflict,
		problem.Conflict)
	if n := d.operations(); n != 0 {
		t.Errorf("%d operations, want none", n)
	}
	if after := ownedSnapshot(t, w.f.dir); !maps.Equal(before, after) {
		t.Errorf("includeDir %v, was %v", after, before)
	}
	if outcomes := d.auditOutcomesOf(spec.Add.Op()); !slices.Equal(outcomes, []string{audit.OutcomeRefused}) {
		t.Errorf("audit outcomes %q, want refused only", outcomes)
	}
}

// nginx-provider TC-ERR33: A stale or must-not-exist ifMatch is a conflict
// Owner WI 6.3. Requirements: FR-11, ASM-9, FR-17.
func TestIfMatchConflicts(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	const pc, qc = "server {\n    listen 8203;\n}\n", "server {\n    listen 8204;\n}\n"
	writeTree(t, w.f.dir, map[string]string{"a.conf": pc})
	w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
	d := startWriteDaemon(t, w)

	for _, ifMatch := range []string{sha("other"), spec.IfMatchNone} {
		d.refused(spec.Update.Op(), "a", jsonBody(t, map[string]any{"content": validSite, "ifMatch": ifMatch}),
			http.StatusConflict, problem.Conflict)
	}
	if n := d.operations(); n != 0 {
		t.Errorf("%d operations, want none", n)
	}

	// a.conf changes after Admit, before the helper runs.
	w.f.p.afterPrepare = func(string) { writeTree(t, w.f.dir, map[string]string{"a.conf": qc}) }
	t.Cleanup(func() { w.f.p.afterPrepare = nil })
	op := d.s.wait(d.accept(spec.Update.Op(), "a", map[string]any{"content": validSite, "ifMatch": sha(pc)}).ID,
		30*time.Second)
	if detail := failedWith(t, op, string(problem.Conflict)); !strings.Contains(detail, "nothing was changed") {
		t.Errorf("detail %q, want nothing changed", detail)
	}
	if b, err := os.ReadFile(filepath.Join(w.f.dir, "a.conf")); err != nil || string(b) != qc {
		t.Errorf("a.conf holds %q (%v), want Q untouched", b, err)
	}
	if n := w.liveTests(); n != 0 {
		t.Errorf("fakenginx ran -t %d times, want none", n)
	}
	w.noCall(t, "reload")
}

// nginx-provider TC-ERR34: Changing enabled on an existing config through update is refused
// Owner WI 6.3. Requirements: FR-11, L-37.
func TestUpdateEnabledMismatchRefused(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"example.com.conf.disabled": validSite})
	d := startWriteDaemon(t, w)
	const v2 = "server {\n    listen 8205;\n}\n"

	p := d.refused(spec.Update.Op(), "example.com", jsonBody(t, map[string]any{"content": v2, "enabled": true}),
		http.StatusBadRequest, problem.ValidationFailed)
	if !strings.Contains(p.Detail, "nexus nginxsite start example.com") {
		t.Errorf("detail %q does not name nexus nginxsite start example.com", p.Detail)
	}
	if n := d.operations(); n != 0 {
		t.Errorf("%d operations, want none", n)
	}
	op := d.s.wait(d.accept(spec.Update.Op(), "example.com", map[string]any{"content": v2, "enabled": false}).ID,
		30*time.Second)
	if r := writeResultOf(t, op); r.Enabled || r.FileName != "example.com.conf.disabled" {
		t.Errorf("result %+v, want example.com.conf.disabled", r)
	}
	checkWritten(t, filepath.Join(w.f.dir, "example.com.conf.disabled"), v2)
}

// nginx-provider TC-ERR35: An invalid write restores the previous file, or removes a created one
// Owner WI 6.3. Requirements: FR-11, NFR-3, FR-17.
func TestInvalidWriteRestores(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			w := newWriteLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
			const pc = "server {\n    listen 8206;\n}\n"
			writeTree(t, w.f.dir, map[string]string{"a.conf": pc, "s.conf": validSite})
			chmodFile(t, filepath.Join(w.f.dir, "a.conf"), 0o640)
			w.useCapture(m.capture+"-valid.jsonl", true, w.f.nginx)
			before := contentSnapshot(t, w.f.dir)

			for _, step := range []struct {
				op   def.Op
				in   any
				file string
			}{
				{spec.Update.Op(), spec.WriteInput{Name: "a", Content: invalidSite}, "a.conf"},
				{spec.Add.Op(), spec.AddInput{Name: "n", Content: invalidSite}, "n.conf"},
			} {
				o := w.write(step.op, step.in)
				o.failed(t, problem.NginxConfigInvalid)
				if !ext[bool](t, o.err, "rolledBack") {
					t.Errorf("%s: rolledBack false, want true", step.file)
				}
				if out := ext[string](t, o.err, "testOutput"); !strings.Contains(out, step.file+":") {
					t.Errorf("%s: testOutput %q does not name it", step.file, out)
				}
				if got, want := w.calls(), []string{"status", "nginx -t"}; !slices.Equal(got, want) {
					t.Errorf("%s: calls %q, want %q: no reload", step.file, got, want)
				}
				if after := contentSnapshot(t, w.f.dir); !maps.Equal(before, after) {
					t.Errorf("%s: includeDir %v, was %v", step.file, after, before)
				}
				absent(t, filepath.Join(o.dir, "content"))
			}
			absent(t, filepath.Join(w.f.dir, "n.conf"))
			w.noCall(t, "reload")
		})
	}
}

// writeReloadRejected is TC-ERR36's harness on systemd: the write makes
// the model bind-conflict, so systemctl exits 0 and the generation stays.
// After the first nginx -t the rollback's /proc is the next capture; when
// rollbackConfirms the rollback's nginx -t (the second) makes the model
// valid again and its reload confirms, else it is the respawn capture,
// which is unconfirmed. corrupt, when set, runs after the first nginx -t.
func writeReloadRejected(t *testing.T, rollbackConfirms bool, corrupt func(w *writeRig)) *writeRig {
	t.Helper()
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	first := newLockstep(t, "ubuntu-bind-rejected.jsonl", false, w.f.nginx)
	second := newLockstep(t, "ubuntu-respawn.jsonl", false, w.f.nginx)
	if rollbackConfirms {
		second = newLockstep(t, "ubuntu-valid.jsonl", true, w.f.nginx)
	}
	seq := w.useSequence(first, second)
	tests := 0
	w.set(func(h *lcHarness) {
		h.afterTest = func() {
			tests++
			switch tests {
			case 1:
				h.f.svc.Set(func(m *svcctltest.Model) { m.Config = svcctltest.BindConflict })
				if corrupt != nil {
					corrupt(w)
				}
			case 2:
				seq.next()
				if rollbackConfirms {
					h.f.svc.Set(func(m *svcctltest.Model) { m.Config = svcctltest.Valid })
					h.setPID(second.master)
				}
			}
		}
	})
	return w
}

// nginx-provider TC-ERR36: A write whose reload is rejected is rolled back and reloaded again
// Owner WI 6.3. Requirements: FR-11, NFR-3, FR-6, FR-17.
func TestWriteReloadRejectedRollsBack(t *testing.T) {
	fakeNginxBinary(t)
	const nc = "server {\n    listen 8207;\n}\n"
	update := spec.WriteInput{Name: "a", Content: nc}
	t.Run("A rollback reload confirmed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := writeReloadRejected(t, true, nil)
			before := contentSnapshot(t, w.f.dir)

			o := w.write(spec.Update.Op(), update)
			o.failed(t, problem.NginxReloadFailed)
			if !ext[bool](t, o.err, "rolledBack") {
				t.Errorf("rolledBack false, want true")
			}
			if obs := ext[spec.ReloadObservation](t, o.err, "observed"); obs.Verdict != spec.VerdictRejected {
				t.Errorf("observed %+v, want verdict rejected", obs)
			}
			if back := ext[spec.ReloadObservation](t, o.err, "rollbackReload"); back.Verdict != spec.VerdictConfirmed {
				t.Errorf("rollbackReload %+v, want verdict confirmed", back)
			}
			if got, want := w.calls(), []string{"status", "nginx -t", "reload", "nginx -t", "status", "reload"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q", got, want)
			}
			if after := contentSnapshot(t, w.f.dir); !maps.Equal(before, after) {
				t.Errorf("includeDir %v, was %v: a.conf is not back as it was", after, before)
			}
		})
	})
	t.Run("B rollback reload unconfirmed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := writeReloadRejected(t, false, nil)
			before := contentSnapshot(t, w.f.dir)

			o := w.write(spec.Update.Op(), update)
			d := o.failed(t, problem.OutcomeUnknown)
			if prev := filepath.Join(o.dir, "previous", "content"); !filepath.IsAbs(prev) || !strings.Contains(d, prev) {
				t.Errorf("detail %q does not name %s", d, prev)
			}
			if got, want := withoutStatus(w.calls()), []string{"nginx -t", "reload", "nginx -t", "reload"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q", got, want)
			}
			if after := contentSnapshot(t, w.f.dir); !maps.Equal(before, after) {
				t.Errorf("includeDir %v, was %v: a.conf is not back as it was", after, before)
			}
		})
	})
	t.Run("C restore fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var corrupted string
			// TC-ERR25's hook: the backup is corrupted after it was taken.
			w := writeReloadRejected(t, true, func(w *writeRig) {
				dir, err := w.f.p.opDir(fmt.Sprintf("op%d", w.ops))
				if err != nil {
					t.Error(err)
					return
				}
				path := PreviousContentPath(dir)
				b, err := os.ReadFile(path)
				if err != nil {
					t.Error(err)
					return
				}
				b[0] ^= 0x01
				corrupted = string(b)
				if err := os.WriteFile(path, b, 0o600); err != nil {
					t.Error(err)
				}
			})

			o := w.write(spec.Update.Op(), update)
			d := o.failed(t, problem.OutcomeUnknown)
			prev := filepath.Join(o.dir, "previous", "content")
			if !strings.Contains(d, prev) || !strings.Contains(d, sha(validSite)) || !strings.Contains(d, sha(corrupted)) {
				t.Errorf("detail %q, want %s and the hash mismatch (%s, %s)", d, prev, sha(validSite), sha(corrupted))
			}
			if got, want := withoutStatus(w.calls()), []string{"nginx -t", "reload"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q: no rollback reload without a restore", got, want)
			}
			if b, err := os.ReadFile(filepath.Join(w.f.dir, "a.conf")); err != nil || string(b) == corrupted {
				t.Errorf("a.conf holds %q (%v): the corrupted bytes reached includeDir", b, err)
			}
		})
	})
}

// nginx-provider TC-ERR37: Invalid content is refused before an operation is created
// Owner WI 6.3. Requirements: FR-12, FR-17.
func TestContentValidation(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	d := startWriteDaemon(t, w)

	// \u0000 decodes to a NUL byte, which reaches Admit.
	d.refused(spec.Add.Op(), "nul", []byte(`{"content":"server {}\u0000\n"}`), http.StatusBadRequest,
		problem.ValidationFailed)
	// JSON cannot carry invalid UTF-8 into a string, so only an in-process
	// Admit can be given it.
	b, dt := w.binding(spec.Add.Op())
	in, err := b.Decode(jsonBody(t, spec.AddInput{Name: "u8"}))
	if err != nil {
		t.Fatal(err)
	}
	ai := in.(spec.AddInput)
	ai.Content = "ab\xff\xfecd"
	var p *problem.Error
	if err := dt.Admit(context.Background(), ai); !errors.As(err, &p) || p.Slug() != problem.ValidationFailed {
		t.Errorf("Admit of invalid UTF-8: %v, want validation-failed", err)
	}
	if n := d.operations(); n != 0 {
		t.Errorf("%d operations, want none", n)
	}
	if outcomes := d.auditOutcomesOf(spec.Add.Op()); slices.Contains(outcomes, audit.OutcomeRequested) {
		t.Errorf("audit outcomes %q: a refusal before the operation has no requested record", outcomes)
	}

	// Over HTTP, encoding/json replaces invalid bytes and a lone surrogate
	// with U+FFFD when it decodes into a string: the decoded content is
	// valid UTF-8, so these are admitted. This replacement is expected;
	// only the in-process Admit above can present invalid UTF-8.
	for _, tc := range []struct{ name, body string }{
		{"u8", "{\"content\":\"ab\xff\xfecd\"}"},
		{"u9", `{"content":"ab\udcffcd"}`},
	} {
		var decoded struct{ Content string }
		if err := json.Unmarshal([]byte(tc.body), &decoded); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(decoded.Content, "\xef\xbf\xbd") || strings.Contains(decoded.Content, "\xff") {
			t.Fatalf("%s decodes to %q, want U+FFFD and no 0xff", tc.name, decoded.Content)
		}
		w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
		code, body := d.call(spec.Add.Op(), tc.name, []byte(tc.body))
		if code != http.StatusAccepted {
			t.Fatalf("POST sites/%s/add = %d %s, want 202", tc.name, code, body)
		}
		var op apitypes.Operation
		if err := json.Unmarshal(body, &op); err != nil {
			t.Fatal(err)
		}
		writeResultOf(t, d.s.wait(op.ID, 30*time.Second))
		if got, err := os.ReadFile(filepath.Join(w.f.dir, tc.name+".conf")); err != nil || string(got) != decoded.Content {
			t.Errorf("%s.conf holds %q (%v), want %q", tc.name, got, err, decoded.Content)
		}
	}
}

// nginx-provider TC-ERR38: An entry that becomes unmanaged before the write helper runs is refused
// Owner WI 6.3. Requirements: FR-11, ASM-11.
func TestWriteHelperRecheck(t *testing.T) {
	fakeNginxBinary(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.conf")
	writeTree(t, filepath.Dir(outside), map[string]string{"elsewhere.conf": validSite})
	for _, tc := range []struct {
		name   string
		reason string
		change func(t *testing.T, w *writeRig)
	}{
		{"A symlink", spec.ReasonSymlink, func(t *testing.T, w *writeRig) {
			link := filepath.Join(w.f.dir, "a.conf")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
		}},
		// T0 cannot chown to another user: the managed owner changes instead.
		{"B not root-owned", spec.ReasonNotRootOwned, func(t *testing.T, _ *writeRig) {
			setIncludeDirUID(t, uint32(os.Getuid())) // includeDir stays trusted: the file is the foreign one
			setManagedUID(t, uint32(os.Getuid())+1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
			writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
			w.set(func(h *lcHarness) { h.afterAdmit = func() { tc.change(t, w) } })
			o := w.write(spec.Update.Op(), spec.WriteInput{Name: "a", Content: invalidSite})
			after := ownedSnapshot(t, w.f.dir)

			o.failed(t, problem.NginxConfigUnmanaged)
			if r, f := ext[string](t, o.err, "reason"), ext[string](t, o.err, "fileName"); r != tc.reason ||
				f != "a.conf" {
				t.Errorf("reason %q and fileName %q, want %s and a.conf", r, f, tc.reason)
			}
			absent(t, filepath.Join(o.dir, "previous"))
			if names := dirNames(t, w.f.dir); !slices.Equal(names, []string{"a.conf"}) {
				t.Errorf("includeDir holds %q, want a.conf only: no temporary file", names)
			}
			if now := ownedSnapshot(t, w.f.dir); !maps.Equal(now, after) {
				t.Errorf("includeDir %v, was %v", now, after)
			}
			if b, err := os.ReadFile(outside); err != nil || string(b) != validSite {
				t.Errorf("the symlink's target holds %q (%v), want it unchanged", b, err)
			}
			if calls := w.calls(); len(calls) != 0 {
				t.Errorf("calls %q, want none", calls)
			}
		})
	}
}

// nginx-provider TC-ERR39: A queued sensitive write across a restart never runs
// Owner WI 6.3. Requirements: FR-15, NFR-3.
func TestQueuedWriteAcrossRestart(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	before := ownedSnapshot(t, w.f.dir)
	gate := w.blockFirstTest(t)
	d := startWriteDaemon(t, w)

	test := d.s.startTest()
	gate.blocked()
	upd := d.accept(spec.Update.Op(), "a", map[string]any{"content": invalidSite})
	if st := d.s.get(upd.ID).Status; st != apitypes.StatusQueued {
		t.Fatalf("the update is %s behind the running test, want Queued", st)
	}
	d.restart()
	op := d.s.wait(upd.ID, 30*time.Second)
	if detail := failedWith(t, op, string(problem.OutcomeUnknown)); !strings.Contains(detail, "never started") {
		t.Errorf("detail %q, want never started", detail)
	}
	gate.release()
	if r := testResultOf(t, d.s.wait(test.ID, 30*time.Second)); !r.Valid {
		t.Errorf("the test op: %+v, want valid", r)
	}
	absent(t, filepath.Join(d.opsDir, upd.ID, "content"))
	if strings.Contains(d.s.log(upd.ID), "content:") {
		t.Errorf("the update's log shows a prepared content: %q", d.s.log(upd.ID))
	}
	if after := ownedSnapshot(t, w.f.dir); !maps.Equal(before, after) {
		t.Errorf("includeDir %v, was %v", after, before)
	}
}

// nginx-provider TC-ERR40: Invalid names are refused before an operation exists, on every name-taking op
// Owner WI 6.3. Requirements: FR-12, CON-4.
func TestEveryNameOpRefusesBadNames(t *testing.T) {
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	// Files a name refused only by the suffix rule would reach.
	writeTree(t, w.f.dir, map[string]string{"example.com.conf.conf": validSite, "x.disabled.conf": validSite,
		"x.conf.disabled.conf": validSite})
	before := ownedSnapshot(t, w.f.dir)
	d := startWriteDaemon(t, w)
	names := []string{"", ".hidden", "-x", "_x", "a..b", strings.Repeat("a", 129), "a/b", "a b", "a\x00b", "é",
		"example.com.conf", "x.disabled", "x.conf.disabled", "..", "."}
	content := jsonBody(t, map[string]any{"content": validSite})
	for _, o := range []def.Op{spec.Get.Op(), spec.Content.Op(), spec.Enable.Op(), spec.Disable.Op(), spec.Add.Op(),
		spec.Update.Op()} {
		body := []byte("{}")
		if o.ID == spec.Add.Op().ID || o.ID == spec.Update.Op().ID {
			body = content
		}
		for _, name := range names {
			if name != "" {
				d.refused(o, name, body, http.StatusBadRequest, problem.ValidationFailed)
				continue
			}
			// No route carries an empty segment: the server answers
			// without reaching the op (the in-process check is below).
			if code, b := d.call(o, name, body); code < 400 || code >= 500 {
				t.Errorf("%s of an empty name = %d %s, want a refusal", o.ID, code, b)
			}
		}
	}
	if n := d.operations(); n != 0 {
		t.Errorf("%d operations, want none", n)
	}
	if after := ownedSnapshot(t, w.f.dir); !maps.Equal(before, after) {
		t.Errorf("includeDir %v, was %v", after, before)
	}
	if calls := w.calls(); len(calls) != 0 {
		t.Errorf("calls %q, want none", calls)
	}
	// In process, past the schema: the suffix rule is the configured
	// suffixes', which only Admit and the reads know, and an empty name is
	// refused there too.
	for _, name := range []string{"a.conf", ""} {
		for _, o := range []def.Op{spec.Get.Op(), spec.Content.Op(), spec.Enable.Op(), spec.Disable.Op(),
			spec.Add.Op(), spec.Update.Op()} {
			var b provider.Binding
			for _, x := range w.f.p.Bindings() {
				if x.Op().ID == o.ID {
					b = x
				}
			}
			in, err := b.Decode(jsonBody(t, map[string]any{"name": name}))
			if err != nil {
				t.Fatal(err)
			}
			if d, ok := b.Detached(); ok {
				err = d.Admit(context.Background(), in)
			} else {
				_, err = b.CallSync(context.Background(), in)
			}
			var p *problem.Error
			if !errors.As(err, &p) || p.Slug() != problem.ValidationFailed {
				t.Errorf("%s of %q in process: %v, want validation-failed", o.ID, name, err)
			}
		}
	}
}

// nginx-provider TC-ERR43: A write refuses, before writing, when a running nginx's master cannot be identified
// Owner WI 6.3. Requirements: FR-11, FR-6, FR-17, NFR-2, NFR-3, L-20, L-55.
func TestWriteRefusesUnidentifiableMaster(t *testing.T) {
	fakeNginxBinary(t)
	const pc, nc, qc, rc = "server {\n    listen 8211;\n}\n", "server {\n    listen 8212;\n}\n",
		"server {\n    listen 8213;\n}\n", "server {\n    listen 8214;\n}\n"
	for _, tc := range []struct {
		name   string
		plat   platform.Info
		setup  func(w *writeRig)
		reason string
	}{
		{"A openrc without a daemon record", alpine, nil, "init system reports no main pid"},
		{"B systemd with a foreign exe", ubuntu, func(w *writeRig) {
			w.useCapture("ubuntu-valid.jsonl", true, "/usr/bin/sleep")
		}, "exe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWriteLC(t, tc.plat, svcctltest.Model{State: svcctl.Running})
			if tc.setup != nil {
				tc.setup(w)
			}
			writeTree(t, w.f.dir, map[string]string{"a.conf": pc, "b.conf.disabled": qc})
			before := ownedSnapshot(t, w.f.dir)
			for _, step := range []struct {
				op def.Op
				in any
			}{
				{spec.Update.Op(), spec.WriteInput{Name: "a", Content: nc, IfMatch: sha(pc)}},
				{spec.Add.Op(), spec.AddInput{Name: "new", Content: nc}},
			} {
				o := w.write(step.op, step.in)
				d := o.failed(t, problem.DependencyUnavailable)
				if !strings.HasPrefix(d, masterUnidentified) || !strings.Contains(strings.ToLower(d), tc.reason) {
					t.Errorf("%s: detail %q, want %q naming %q", step.op.Action, d, masterUnidentified, tc.reason)
				}
				if _, ok := o.err.Extensions["rolledBack"]; ok {
					t.Errorf("%s: problem %v has rolledBack: nothing was changed", step.op.Action, o.err)
				}
				if got, want := w.calls(), []string{"status"}; !slices.Equal(got, want) {
					t.Errorf("%s: calls %q, want %q: no nginx -t and no reload", step.op.Action, got, want)
				}
				if after := ownedSnapshot(t, w.f.dir); !maps.Equal(before, after) {
					t.Errorf("%s: includeDir %v, was %v", step.op.Action, after, before)
				}
				absent(t, filepath.Join(o.dir, "previous"))
				absent(t, filepath.Join(o.dir, "content"))
			}
			absent(t, filepath.Join(w.f.dir, "new.conf"))
			if n := w.liveTests(); n != 0 {
				t.Errorf("fakenginx ran -t %d times, want none", n)
			}

			o := w.write(spec.Update.Op(), spec.WriteInput{Name: "b", Content: rc})
			r := o.succeeded(t)
			if r.Enabled || r.Reloaded {
				t.Errorf("result %+v, want not enabled, not reloaded", r)
			}
			checkWritten(t, filepath.Join(w.f.dir, "b.conf.disabled"), rc)
			if got := w.fake.presented(); !slices.Equal(got, []string{"b.conf"}) {
				t.Errorf("shadow tests presented %q, want [b.conf]", got)
			}
			if calls := w.calls(); len(calls) != 0 {
				t.Errorf("calls %q, want none: a disabled target skips the master", calls)
			}
			w.noCall(t, "reload")
		})
	}
}

// A master whose exe link reads the configured nginx with " (deleted)"
// (a package upgrade replaced the binary, L-53) is identified, and the
// write proceeds to its reload.
func TestWriteWithReplacedBinaryProceeds(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, alpine, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	w.useCapture("alpine-valid.jsonl", true, w.f.nginx+" (deleted)")
	const nc = "server {\n    listen 8215;\n}\n"

	r := w.write(spec.Update.Op(), spec.WriteInput{Name: "a", Content: nc}).succeeded(t)
	if !r.Reloaded || r.Reload == nil || r.Reload.Verdict != spec.VerdictConfirmed {
		t.Errorf("result %+v, want reloaded and confirmed", r)
	}
	checkWritten(t, filepath.Join(w.f.dir, "a.conf"), nc)
}

// failingProc fails every Snapshot after its first n.
type failingProc struct {
	ProcReader
	mu sync.Mutex
	n  int
}

func (p *failingProc) Snapshot() (map[int]Proc, error) {
	p.mu.Lock()
	p.n--
	left := p.n
	p.mu.Unlock()
	if left < 0 {
		return nil, errors.New("planted /proc failure")
	}
	return p.ProcReader.Snapshot()
}

// A reload that fails before it is sent (the workers cannot be observed)
// restores the file and is dependency-unavailable, with no second reload:
// nginx never loaded the new content (Data Flow 2 step 5g, as WI 6.2's
// toggles do).
func TestWriteReloadNotSentRestores(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	chmodFile(t, filepath.Join(w.f.dir, "a.conf"), 0o640)
	p := w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
	// The master is identified (one Snapshot); observing its workers fails.
	w.set(func(h *lcHarness) { h.proc = &failingProc{ProcReader: p, n: 1} })
	before := contentSnapshot(t, w.f.dir)

	o := w.write(spec.Update.Op(), spec.WriteInput{Name: "a", Content: "server {\n    listen 8216;\n}\n"})
	d := o.failed(t, problem.DependencyUnavailable)
	if !strings.Contains(d, "no reload was sent") || !strings.Contains(d, "a.conf was restored") {
		t.Errorf("detail %q, want no reload sent and a.conf restored", d)
	}
	res := helperResultOf(t, lcOutcome{dir: o.dir})
	if string(res.Error.Ext["rolledBack"]) != "true" {
		t.Errorf("result.json error %+v, want rolledBack true", res.Error)
	}
	if got, want := w.calls(), []string{"status", "nginx -t"}; !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q: no reload", got, want)
	}
	if after := contentSnapshot(t, w.f.dir); !maps.Equal(before, after) {
		t.Errorf("includeDir %v, was %v", after, before)
	}
}

// The rest of TC-77 (WI 5.1's TestOneQueuedLane checks the lane of each
// bound operation): every one of the nine detached nginx operations is
// bound, now that add and update are.
func TestEveryDetachedOpBound(t *testing.T) {
	f, _ := helperFixture(t, true)
	bound := map[string]bool{}
	for _, b := range f.p.Bindings() {
		d, ok := b.Detached()
		if !ok {
			continue
		}
		bound[b.Op().ID] = true
		in, err := b.Decode(json.RawMessage(`{"name":"a"}`))
		if err != nil {
			in, err = b.Decode(json.RawMessage(`{}`))
		}
		if err != nil {
			t.Fatalf("%s: decode: %v", b.Op().ID, err)
		}
		if l, err := d.Lane(context.Background(), in); err != nil || l != nginxLane {
			t.Errorf("%s: Lane() = %+v, %v; want {nginx, queue}", b.Op().ID, l, err)
		}
	}
	if len(bound) != len(nginxDetachedOps) {
		t.Errorf("%d detached operations bound, want %d: %v", len(bound), len(nginxDetachedOps), bound)
	}
	for _, o := range nginxDetachedOps {
		if !bound[o.ID] {
			t.Errorf("%s is not bound as a detached operation", o.ID)
		}
	}
}

// A lost add of a file that did not exist names no previous content: its
// previous/meta.json says the file did not exist (WI 6.1's exists).
func TestLostDetailOfCreatedFile(t *testing.T) {
	f, _ := helperFixture(t, true)
	opsDir := t.TempDir()
	f.p.UseOpsDir(opsDir)
	prev := filepath.Join(opsDir, "op-1", "previous")
	if err := os.MkdirAll(prev, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(prev, "meta.json", Meta{FileName: "n.conf"}); err != nil {
		t.Fatal(err)
	}
	d := f.p.lostDetail("add", provider.NewRun("op-1", nil))
	if !strings.Contains(d, "action add") || !strings.Contains(d, "n.conf did not exist") ||
		strings.Contains(d, "previous content (") || strings.Contains(d, "is kept at") {
		t.Errorf("lost detail %q, want n.conf named as not existing before and no previous content", d)
	}
}
