// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nginx

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
	"github.com/mauktik/nexus/internal/systemspec"
)

// The lifecycle tests run the helper in-process (test-strategy.md): the
// binding's Admit and Prepare, runHelper with an env built from input.json
// whose init system is the svcctl fake and whose /proc is a fake reader in
// lockstep with it, then the binding's Finalize. nginx -t is fakenginx.

// lcModes are the two init systems, each with its platform and the prefix
// of its reload captures in testdata/reload.
var lcModes = []struct {
	name    string
	plat    platform.Info
	capture string
}{
	{"systemd", ubuntu, "ubuntu"},
	{"openrc", alpine, "alpine"},
}

const (
	validSite   = "server {\n    listen 8181;\n}\n"
	invalidSite = "server {\n    listen 8181;\n    INVALID;\n}\n"
)

// lcEvent is one call the helper made: a service-state query ("status"),
// an init-system action by its verb, or "nginx -t".
type lcEvent struct {
	what string
	at   time.Time
}

// lcHarness is one provider over fakenginx and the svcctl fake of one init
// system, with every call the helper makes recorded in order.
type lcHarness struct {
	t        *testing.T
	f        *fixture
	ctl      svcctl.Controller
	runRoot  string
	procRoot string
	pidfile  string

	mu          sync.Mutex
	events      []lcEvent
	admitEvents []lcEvent
	blockOn     string              // An init action that blocks until its bound expires
	statusRes   svcctl.Result       // Answers the next statusLeft status queries
	statusLeft  int                 // -1: every one
	afterDo     func(verb string)   // After each init action
	afterTest   func()              // After each nginx -t
	afterAdmit  func()              // Between Admit and the helper
	nextPIDs    []int               // The main PID of each start or restart that ends running
	proc        ProcReader          // The helper's /proc
	reloaded    func(accepted bool) // After each reload the fake answered
	reloadFault reloadFault         // Makes the next reloads' tool fail
	ops         int
}

// reloadFault makes the init tool fail the next n actions named verb (""
// is reload) with err. When before is set the tool never started, so the
// fake does not see the action; otherwise it delivered the action and then
// died (a signal, a wait error), so the fake applied it and the caller
// still got err.
type reloadFault struct {
	verb   string
	n      int
	err    error
	before bool
}

func newLC(t *testing.T, plat platform.Info, m svcctltest.Model) *lcHarness {
	t.Helper()
	f := newFixture(t, fixtureOptions{plat: plat, model: m, noNginx: true, preflight: enabledStatus})
	root := filepath.Dir(filepath.Dir(f.nginx))
	installFakeNginx(t, f.nginx, fakeDirs(t, root, f.dir))
	h := &lcHarness{t: t, f: f, runRoot: filepath.Join(root, "lc-run"), procRoot: filepath.Join(root, "lc-proc")}
	h.pidfile = filepath.Join(h.runRoot, "nginx", "nginx.pid")
	for _, d := range []string{filepath.Dir(h.pidfile), h.procRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctl, err := svcctl.New(plat.Init, svcctl.Options{Run: h.run, RunRoot: h.runRoot, ProcRoot: h.procRoot,
		Timeout: f.p.Settings().StateTimeout})
	if err != nil {
		t.Fatal(err)
	}
	h.ctl = ctl
	f.p.services = ctl
	f.p.UseOpsDir(t.TempDir())
	f.p.executable = func() (string, error) { return "/usr/sbin/nexusd", nil }
	h.proc = unreadProc{t}
	return h
}

// initCall names an svcctl argv: "status", "exists" or the action's verb.
func initCall(argv []string) string {
	switch {
	case len(argv) > 1 && argv[1] == "show":
		return "status"
	case len(argv) > 1 && argv[1] == "--exists":
		return "exists"
	case len(argv) == 4 && argv[1] == "-C":
		return argv[3]
	case len(argv) > 1:
		return argv[1]
	}
	return strings.Join(argv, " ")
}

// run is the controller's Options.Run: it records the call and passes it
// to the svcctl fake, then plays the parts of nginx the fake does not
// model: the new master's PID after a start, and the /proc reader's view of
// a reload.
func (h *lcHarness) run(ctx context.Context, argv []string) (svcctl.Result, error) {
	what := initCall(argv)
	h.mu.Lock()
	if what != "exists" {
		h.events = append(h.events, lcEvent{what, time.Now()})
	}
	block := what == h.blockOn
	var override *svcctl.Result
	if what == "status" && h.statusLeft != 0 {
		res := h.statusRes
		override = &res
		if h.statusLeft > 0 {
			h.statusLeft--
		}
	}
	afterDo, reloaded := h.afterDo, h.reloaded
	var fault *reloadFault
	if h.reloadFault.n > 0 && what == cmp.Or(h.reloadFault.verb, "reload") {
		h.reloadFault.n--
		f := h.reloadFault
		fault = &f
	}
	h.mu.Unlock()
	if fault != nil && fault.before {
		return svcctl.Result{}, fault.err
	}
	if block {
		<-ctx.Done()
		return svcctl.Result{}, ctx.Err()
	}
	if override != nil {
		return *override, nil
	}
	before := h.f.svc.Model().Generation
	res, err := h.f.svc.Run(ctx, argv)
	after := h.f.svc.Model()
	switch what {
	case "start", "restart":
		if after.State == svcctl.Running && after.Generation > before {
			h.mu.Lock()
			pid := 0
			if len(h.nextPIDs) > 0 {
				pid, h.nextPIDs = h.nextPIDs[0], h.nextPIDs[1:]
			}
			h.mu.Unlock()
			if pid > 0 {
				h.setPID(pid)
			}
		}
	case "reload":
		if reloaded != nil {
			reloaded(after.Generation > before)
		}
	}
	if fault != nil {
		return svcctl.Result{}, fault.err
	}
	if afterDo != nil && what != "status" && what != "exists" {
		afterDo(what)
	}
	return res, err
}

// setPID makes pid nginx's main PID as the init system reports it: the
// systemd capture's MainPID, or an OpenRC daemon record naming a pidfile
// that holds pid, with pid present in the proc root (L-20).
func (h *lcHarness) setPID(pid int) {
	if h.f.p.plat.Init == platform.Systemd {
		h.f.svc.Set(func(m *svcctltest.Model) { m.MainPID = pid })
		return
	}
	if err := svcctltest.WriteDaemonRecord(h.runRoot, svcctltest.Service, h.pidfile, pid); err != nil {
		h.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.procRoot, strconv.Itoa(pid)), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

// setStatus answers the next n status queries (every one when n is -1)
// with out.
func (h *lcHarness) setStatus(out svcctl.Result, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statusRes, h.statusLeft = out, n
}

func (h *lcHarness) set(change func(h *lcHarness)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change(h)
}

func (h *lcHarness) event(what string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, lcEvent{what, time.Now()})
}

// calls are the helper's calls, in order.
func (h *lcHarness) calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.events {
		out = append(out, e.what)
	}
	return out
}

// times are when the helper made each call named what.
func (h *lcHarness) times(what string) []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []time.Time
	for _, e := range h.events {
		if e.what == what {
			out = append(out, e.at)
		}
	}
	return out
}

// nginxTests counts the -t runs in fakenginx's own call log.
func (h *lcHarness) nginxTests() int {
	n := 0
	for _, c := range readFakeCalls(h.t, h.f.nginx) {
		if slices.Contains(c.Argv, "-t") {
			n++
		}
	}
	return n
}

// build is runHelper's env builder: the real one, with the init system,
// /proc and a recording nginx -t swapped in.
func (h *lcHarness) build(in helperInput, out io.Writer) (env, error) {
	e, err := newEnv(in, out)
	if err != nil {
		return env{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e.nginx, e.svc, e.proc = lcTester{h: h, inner: e.nginx}, h.ctl, h.proc
	return e, nil
}

// lcTester records each nginx -t in the harness's call order.
type lcTester struct {
	h     *lcHarness
	inner Tester
}

func (x lcTester) Test(ctx context.Context) (TestOutput, error) {
	x.h.event("nginx -t")
	out, err := x.inner.Test(ctx)
	x.h.mu.Lock()
	after := x.h.afterTest
	x.h.mu.Unlock()
	if after != nil {
		after()
	}
	return out, err
}

// unreadProc is the /proc of a test that must not read it.
type unreadProc struct{ t *testing.T }

func (r unreadProc) Snapshot() (map[int]Proc, error) {
	r.t.Error("the helper read /proc")
	return nil, errors.New("unexpected /proc read")
}

func (r unreadProc) Exe(pid int) (string, error) {
	r.t.Errorf("the helper read the exe of %d", pid)
	return "", errors.New("unexpected /proc read")
}

func (r unreadProc) RealUID(pid int) (uint32, error) {
	r.t.Errorf("the helper read the uid of %d", pid)
	return 0, errors.New("unexpected /proc read")
}

func (r unreadProc) PIDNamespace(pid int) (string, error) {
	r.t.Errorf("the helper read the PID namespace of %d", pid)
	return "", errors.New("unexpected /proc read")
}

// lcOutcome is one operation run through its binding.
type lcOutcome struct {
	res     spec.ServiceResult
	err     *problem.Error // Nil when the operation succeeded
	refused bool           // Admit refused it: no operation exists
	dir     string         // The operation's directory
	log     string
	elapsed time.Duration // From Prepare to Finalize
}

func (h *lcHarness) binding(o def.Op) (provider.Binding, provider.DetachedFuncs) {
	h.t.Helper()
	for _, b := range h.f.p.Bindings() {
		if b.Op().ID == o.ID {
			if d, ok := b.Detached(); ok {
				return b, d
			}
		}
	}
	h.t.Fatalf("%s is not bound as a detached operation", o.ID)
	return provider.Binding{}, provider.DetachedFuncs{}
}

// op runs o as the dispatcher and the supervisor would, with the helper
// in-process: Admit, Prepare, the helper, Finalize.
func (h *lcHarness) op(o def.Op) lcOutcome {
	t := h.t
	t.Helper()
	b, d := h.binding(o)
	ctx := context.Background()
	in, err := b.Decode(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	h.ops++
	if err := d.Admit(ctx, in); err != nil {
		var p *problem.Error
		if !errors.As(err, &p) {
			t.Fatalf("%s: Admit: %v, want a problem", o.ID, err)
		}
		return lcOutcome{err: p, refused: true}
	}
	h.mu.Lock()
	h.admitEvents, h.events = h.events, nil
	afterAdmit := h.afterAdmit
	h.mu.Unlock()
	if afterAdmit != nil {
		afterAdmit()
	}
	var log syncBuffer
	r := provider.NewRun(fmt.Sprintf("op%d", h.ops), &log)
	start := time.Now()
	sp, err := d.Prepare(ctx, in, r)
	if err != nil {
		t.Fatalf("%s: Prepare: %v", o.ID, err)
	}
	var stderr bytes.Buffer
	if code := runHelper(ctx, sp.Dir, &log, &stderr, h.build); code != 0 {
		t.Fatalf("%s: helper exited %d: %s", o.ID, code, stderr.String())
	}
	out, err := d.Finalize(ctx, in, detach.Exit{}, r)
	res := lcOutcome{dir: sp.Dir, log: log.String(), elapsed: time.Since(start)}
	if err != nil {
		if !errors.As(err, &res.err) {
			t.Fatalf("%s: Finalize: %v, want a problem", o.ID, err)
		}
		return res
	}
	res.res = out.(spec.ServiceResult)
	return res
}

// succeeded requires the operation to have succeeded.
func (o lcOutcome) succeeded(t *testing.T) spec.ServiceResult {
	t.Helper()
	if o.err != nil {
		t.Fatalf("operation failed: %v\nlog:\n%s", o.err, o.log)
	}
	return o.res
}

// failed requires the operation to have failed with slug, not been
// refused, and returns its detail.
func (o lcOutcome) failed(t *testing.T, slug problem.Slug) string {
	t.Helper()
	if o.refused {
		t.Fatalf("Admit refused the operation with %v; want an operation that fails %s", o.err, slug)
	}
	if o.err == nil || o.err.Slug() != slug {
		t.Fatalf("operation ended %v, result %+v; want Failed %s\nlog:\n%s", o.err, o.res, slug, o.log)
	}
	return o.err.Detail
}

// ext decodes extension key of p.
func ext[T any](t *testing.T, p *problem.Error, key string) T {
	t.Helper()
	var v T
	raw, ok := p.Extensions[key]
	if !ok {
		t.Fatalf("%v has no %s extension", p, key)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s %s: %v", key, b, err)
	}
	return v
}

// helperResultOf reads the helper's result.json for o.
func helperResultOf(t *testing.T, o lcOutcome) helperResult {
	t.Helper()
	var res helperResult
	if ok, err := readJSON(o.dir, resultFile, &res); err != nil || !ok {
		t.Fatalf("result.json: %v, %v", ok, err)
	}
	return res
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startingStatus is the init system's answer while nginx is starting.
// OpenRC's exit code is not read when the output names a state.
func startingStatus(init platform.InitSystem) svcctl.Result {
	if init == platform.Systemd {
		return svcctl.Result{Output: []byte("LoadState=loaded\nActiveState=activating\nSubState=start\n" +
			"StateChangeTimestamp=@1794584529\nMainPID=0\n")}
	}
	return svcctl.Result{Output: []byte(" * status: starting\n")}
}

// lockstepProc is the fake /proc of a reload: one real capture, replayed
// in lockstep with the svcctl fake (test-strategy.md). Until the fake has
// answered a reload it shows the last scan before the captured reload; then
// it replays the scans after it, one per Snapshot. A capture records one
// outcome, so a reload the fake answered differently fails the test.
type lockstepProc struct {
	t       *testing.T
	name    string
	accepts bool // The capture shows nginx applying the configuration
	master  int
	pre     map[int]Proc
	post    []map[int]Proc
	exe     map[int]string

	mu    sync.Mutex
	sent  bool
	reads int
}

// useCapture makes file the helper's /proc, with the master's exe link
// reading masterExe, and makes its master nginx's main PID.
func (h *lcHarness) useCapture(file string, accepts bool, masterExe string) *lockstepProc {
	h.t.Helper()
	fx := loadCapture(h.t, file)
	p := &lockstepProc{t: h.t, name: file, accepts: accepts, master: fx.header.MasterPID,
		pre: snapshotOf(h.t, fx.pre[len(fx.pre)-1]), exe: map[int]string{fx.header.MasterPID: masterExe}}
	for _, s := range fx.post {
		p.post = append(p.post, snapshotOf(h.t, s))
	}
	h.set(func(h *lcHarness) { h.proc, h.reloaded = p, p.reload })
	h.setPID(fx.header.MasterPID)
	return p
}

func (p *lockstepProc) reload(accepted bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if accepted != p.accepts {
		p.t.Errorf("the svcctl fake answered a reload with accepted=%v, but capture %s shows accepted=%v",
			accepted, p.name, p.accepts)
		return
	}
	p.sent = true
}

func (p *lockstepProc) Snapshot() (map[int]Proc, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.sent {
		return p.pre, nil
	}
	s := p.post[min(p.reads, len(p.post)-1)]
	p.reads++
	return s, nil
}

func (p *lockstepProc) Exe(pid int) (string, error) {
	if e, ok := p.exe[pid]; ok {
		return e, nil
	}
	return "", fs.ErrNotExist
}

// RealUID is the managed owner (root, in production) of every captured
// process, as the captures were taken under root.
func (p *lockstepProc) RealUID(pid int) (uint32, error) {
	if _, ok := p.exe[pid]; ok {
		return managedUID, nil
	}
	return 0, fs.ErrNotExist
}

// PIDNamespace is the host's of every process, as the captures were taken
// on the host.
func (p *lockstepProc) PIDNamespace(int) (string, error) { return hostPIDNS, nil }

// generation is the master's (pid, startTicks) and its children's, as the
// next Snapshot would show them; reading it does not advance the replay.
func (p *lockstepProc) generation() map[Worker]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := p.pre
	if p.sent {
		snap = p.post[min(p.reads, len(p.post)-1)]
	}
	g := map[Worker]string{}
	for _, pr := range snap {
		if pr.PID == p.master || pr.PPID == p.master {
			g[Worker{PID: pr.PID, StartTicks: pr.StartTicks}] = pr.Title
		}
	}
	return g
}

// dirEntry is one entry of includeDir: its inode, mode and bytes.
type dirEntry struct {
	ino  uint64
	mode fs.FileMode
	sum  [32]byte
}

// includeSnapshot is every entry under dir by path: names, inodes, modes
// and the bytes of regular files.
func includeSnapshot(t *testing.T, dir string) map[string]dirEntry {
	t.Helper()
	snap := map[string]dirEntry{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: no stat_t", path)
		}
		e := dirEntry{ino: uint64(st.Ino), mode: fi.Mode()}
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			e.sum = sha256.Sum256(b)
		}
		snap[path] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// nginx-provider TC-84: Start brings nginx to running after a passing config test
// Owner WI 5.3. Requirements: FR-5, FR-19.
//
// The first three state reads after Do(start) see nginx still starting, so
// the 250 ms cadence of the polls is observable in the bubble.
func TestStartFromStopped(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Stopped})
				writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
				h.set(func(h *lcHarness) {
					h.nextPIDs = []int{4242}
					h.afterDo = func(verb string) {
						if verb == "start" {
							h.setStatus(startingStatus(m.plat.Init), 3)
						}
					}
				})

				r := h.op(spec.Start.Op()).succeeded(t)
				want := []string{"status", "nginx -t", "start", "status", "status", "status", "status"}
				if got := h.calls(); !slices.Equal(got, want) {
					t.Errorf("calls %q, want %q", got, want)
				}
				started := h.times("start")[0]
				polls := h.times("status")[1:]
				for i, at := range polls {
					if want := started.Add(time.Duration(i) * 250 * time.Millisecond); !at.Equal(want) {
						t.Errorf("poll %d at %v after start, want %v", i, at.Sub(started), want.Sub(started))
					}
				}
				if !r.Changed || r.State != string(svcctl.Running) || r.MainPID == nil || *r.MainPID != 4242 {
					t.Errorf("result %+v (mainPid %v), want changed, running, mainPid 4242", r, r.MainPID)
				}
				if !strings.Contains(r.TestOutput, "test is successful") {
					t.Errorf("testOutput %q, want nginx -t's output", r.TestOutput)
				}
				if n := h.nginxTests(); n != 1 {
					t.Errorf("fakenginx ran -t %d times, want 1", n)
				}
			})
		})
	}
}

// nginx-provider TC-85: Stop brings nginx to stopped
// Owner WI 5.3. Requirements: FR-5.
func TestStopFromRunning(t *testing.T) {
	if spec.Stop.Op().Risk != def.RiskDestructive {
		t.Errorf("stop's risk is %s, want destructive", spec.Stop.Op().Risk)
	}
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
			h.setPID(700)
			writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})

			r := h.op(spec.Stop.Op()).succeeded(t)
			if got, want := h.calls(), []string{"status", "stop", "status"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q", got, want)
			}
			if n := h.nginxTests(); n != 0 {
				t.Errorf("stop ran nginx -t %d times, want none", n)
			}
			if !r.Changed || r.State != string(svcctl.Stopped) || r.MainPID != nil {
				t.Errorf("result %+v, want changed, stopped, no mainPid", r)
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(b, []byte(`"mainPid":null`)) {
				t.Errorf("result %s, want mainPid null", b)
			}
			if st := h.f.svc.Model().State; st != svcctl.Stopped {
				t.Errorf("the fake is %s, want stopped", st)
			}
		})
	}
}

// nginx-provider TC-86: Restart tests first and ends running with a new master
// Owner WI 5.3. Requirements: FR-5.
func TestRestart(t *testing.T) {
	for _, m := range lcModes {
		for _, tc := range []struct {
			name      string
			from      svcctl.State
			pid, next int
		}{
			{"A running", svcctl.Running, 100, 200},
			{"B stopped", svcctl.Stopped, 0, 300},
		} {
			t.Run(m.name+"/"+tc.name, func(t *testing.T) {
				h := newLC(t, m.plat, svcctltest.Model{State: tc.from})
				if tc.pid > 0 {
					h.setPID(tc.pid)
				}
				h.set(func(h *lcHarness) { h.nextPIDs = []int{tc.next} })
				writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})

				r := h.op(spec.Restart.Op()).succeeded(t)
				if got, want := h.calls(), []string{"nginx -t", "restart", "status"}; !slices.Equal(got, want) {
					t.Errorf("calls %q, want %q (no start)", got, want)
				}
				if !r.Changed || r.State != string(svcctl.Running) || r.MainPID == nil || *r.MainPID != tc.next {
					t.Errorf("result %+v (mainPid %v), want changed, running, mainPid %d", r, r.MainPID, tc.next)
				}
				if st := h.f.svc.Model().State; st != svcctl.Running {
					t.Errorf("the fake is %s, want running", st)
				}
			})
		}
	}
}

// nginx-provider TC-87: Reload succeeds only on a confirmed new generation
// Owner WI 5.3. Requirements: FR-6, ASM-7, L-53.
func TestReloadConfirmed(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		for _, tc := range []struct {
			name   string
			suffix string
		}{
			{"A exe", ""},
			{"B exe deleted", " (deleted)"},
		} {
			t.Run(m.name+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
					writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
					p := h.useCapture(m.capture+"-valid.jsonl", true, h.f.nginx+tc.suffix)

					o := h.op(spec.Reload.Op())
					r := o.succeeded(t)
					if got, want := h.calls(), []string{"status", "nginx -t", "reload"}; !slices.Equal(got, want) {
						t.Errorf("calls %q, want %q", got, want)
					}
					obs := r.Reload
					if obs == nil {
						t.Fatalf("result %+v has no reload observation", r)
					}
					if obs.Verdict != spec.VerdictConfirmed || obs.MasterPID != p.master || len(obs.NewWorkers) == 0 ||
						obs.InitExitCode != 0 {
						t.Errorf("observation %+v, want confirmed, master %d, new workers, init exit 0", obs, p.master)
					}
					if !r.Changed || r.State != string(svcctl.Running) || r.MainPID == nil || *r.MainPID != p.master {
						t.Errorf("result %+v, want changed, running, mainPid %d", r, p.master)
					}
					if !strings.Contains(o.log, "confirmed") {
						t.Errorf("log %q does not report the verdict", o.log)
					}
				})
			})
		}
	}
}

// nginx-provider TC-E20: Start of a running nginx is idempotent
// Owner WI 5.3. Requirements: FR-5.
func TestStartIdempotent(t *testing.T) {
	for _, m := range lcModes {
		for _, tc := range []struct {
			name, site string
			config     svcctltest.Config
		}{
			{"A valid", validSite, svcctltest.Valid},
			{"B invalid on disk", invalidSite, svcctltest.Invalid},
		} {
			t.Run(m.name+"/"+tc.name, func(t *testing.T) {
				h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Running, Config: tc.config})
				h.setPID(900)
				writeTree(t, h.f.dir, map[string]string{"a.conf": tc.site})

				r := h.op(spec.Start.Op()).succeeded(t)
				if got, want := h.calls(), []string{"status"}; !slices.Equal(got, want) {
					t.Errorf("calls %q, want %q: no nginx -t and no start", got, want)
				}
				if n := h.nginxTests(); n != 0 {
					t.Errorf("fakenginx's call log has %d -t runs, want none", n)
				}
				if r.Changed || r.State != string(svcctl.Running) {
					t.Errorf("result %+v, want unchanged and running", r)
				}
			})
		}
	}
}

// nginx-provider TC-E21: Stop of a stopped nginx is idempotent
// Owner WI 5.3. Requirements: FR-5.
func TestStopIdempotent(t *testing.T) {
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Stopped})
			r := h.op(spec.Stop.Op()).succeeded(t)
			if got, want := h.calls(), []string{"status"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q: no stop", got, want)
			}
			if r.Changed || r.State != string(svcctl.Stopped) || r.MainPID != nil {
				t.Errorf("result %+v, want unchanged and stopped", r)
			}
		})
	}
}

// masterUnidentified is the start of the detail of a reload refused
// because nginx's master cannot be identified (L-55).
const masterUnidentified = "nginx master process could not be identified: "

// nginx-provider TC-E22: A running nginx with no readable master PID makes reload a definite refusal
// Owner WI 5.3. Requirements: FR-6, L-20, L-55, NFR-2.
//
// The reason's wording is ValidateMaster's (WI 5.2): "the init system
// reports no main pid (0)", the L-55 example in lower case.
func TestReloadWithoutMasterPID(t *testing.T) {
	for _, tc := range []struct {
		name string
		plat platform.Info
	}{
		{"A openrc without a daemon record", alpine},
		{"B systemd active with MainPID 0", ubuntu},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLC(t, tc.plat, svcctltest.Model{State: svcctl.Running})
			if tc.plat.Init == platform.Systemd {
				h.setStatus(svcctl.Result{Output: []byte("LoadState=loaded\nActiveState=active\nSubState=running\n" +
					"StateChangeTimestamp=@1794584529\nMainPID=0\n")}, -1)
			}
			writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
			before := includeSnapshot(t, h.f.dir)

			d := h.op(spec.Reload.Op()).failed(t, problem.DependencyUnavailable)
			if !strings.HasPrefix(d, masterUnidentified) ||
				!strings.Contains(strings.ToLower(d), "init system reports no main pid") {
				t.Errorf("detail %q, want %q and the reason init system reports no main PID", d, masterUnidentified)
			}
			if got, want := h.calls(), []string{"status"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q: no nginx -t and no reload", got, want)
			}
			if n := h.nginxTests(); n != 0 {
				t.Errorf("fakenginx ran -t %d times, want none", n)
			}
			if after := includeSnapshot(t, h.f.dir); !maps.Equal(before, after) {
				t.Errorf("includeDir changed: %v, was %v", after, before)
			}
		})
	}
}

// nginx-provider TC-ERR16: Start and restart refuse an invalid config and change nothing
// Owner WI 5.3. Requirements: FR-5, FR-17.
func TestStartRestartRefuseInvalidConfig(t *testing.T) {
	for _, m := range lcModes {
		for _, tc := range []struct {
			op    def.Op
			from  svcctl.State
			calls []string
		}{
			{spec.Start.Op(), svcctl.Stopped, []string{"status", "nginx -t"}},
			{spec.Restart.Op(), svcctl.Running, []string{"nginx -t"}},
		} {
			t.Run(m.name+"/"+tc.op.Action, func(t *testing.T) {
				h := newLC(t, m.plat, svcctltest.Model{State: tc.from, Config: svcctltest.Invalid})
				h.setPID(500)
				writeTree(t, h.f.dir, map[string]string{"a.conf": invalidSite})
				gen := h.f.svc.Model().Generation

				o := h.op(tc.op)
				o.failed(t, problem.NginxConfigInvalid)
				if out := ext[string](t, o.err, "testOutput"); !strings.Contains(out, "unknown directive") {
					t.Errorf("testOutput %q, want unknown directive", out)
				}
				if got := h.calls(); !slices.Equal(got, tc.calls) {
					t.Errorf("calls %q, want %q: no start and no restart", got, tc.calls)
				}
				if m := h.f.svc.Model(); m.State != tc.from || m.Generation != gen {
					t.Errorf("the fake is %s at generation %d, want %s at %d", m.State, m.Generation, tc.from, gen)
				}
			})
		}
	}
}

// nginx-provider TC-ERR17: A systemd reload that exits 0 but leaves the old generation is a failure
// Owner WI 5.3. Requirements: FR-6, ASM-7, NFR-6.
//
// Both systemd reload forms exit 0 on a bind conflict: Ubuntu's `nginx -s
// reload` and nginx.org's HUP.
func TestSystemdReloadExitZeroButRejected(t *testing.T) {
	fakeNginxBinary(t)
	for _, rm := range []svcctltest.ReloadModel{svcctltest.SignalParse, svcctltest.HUP} {
		t.Run(string(rm), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newLC(t, ubuntu, svcctltest.Model{State: svcctl.Running, Config: svcctltest.BindConflict,
					ReloadModel: rm})
				writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
				h.useCapture("ubuntu-bind-rejected.jsonl", false, h.f.nginx)

				o := h.op(spec.Reload.Op())
				o.failed(t, problem.NginxReloadFailed)
				obs := ext[spec.ReloadObservation](t, o.err, "observed")
				if obs.InitExitCode != 0 || obs.Verdict != spec.VerdictRejected {
					t.Errorf("observed %+v, want init exit 0 and verdict rejected", obs)
				}
				if o.elapsed != testReloadTimeout {
					t.Errorf("reported after %v, want at reloadVerifyTimeout %v", o.elapsed, testReloadTimeout)
				}
				if got, want := h.calls(), []string{"status", "nginx -t", "reload"}; !slices.Equal(got, want) {
					t.Errorf("calls %q, want %q", got, want)
				}
			})
		})
	}
}

// nginx-provider TC-ERR18: An OpenRC reload that exits non-zero is a failure
// Owner WI 5.3. Requirements: FR-6, ASM-7.
func TestOpenRCReloadNonZero(t *testing.T) {
	fakeNginxBinary(t)
	synctest.Test(t, func(t *testing.T) {
		h := newLC(t, alpine, svcctltest.Model{State: svcctl.Running})
		writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
		h.useCapture("alpine-invalid.jsonl", false, h.f.nginx)
		// The configuration turns invalid after the pre-test passed.
		h.set(func(h *lcHarness) {
			h.afterTest = func() {
				h.f.svc.Set(func(m *svcctltest.Model) { m.Config = svcctltest.Invalid })
				writeTree(t, h.f.dir, map[string]string{"a.conf": invalidSite})
			}
		})

		o := h.op(spec.Reload.Op())
		o.failed(t, problem.NginxReloadFailed)
		obs := ext[spec.ReloadObservation](t, o.err, "observed")
		if obs.InitExitCode == 0 || obs.Verdict != spec.VerdictRejected {
			t.Errorf("observed %+v, want a non-zero init exit and verdict rejected", obs)
		}
		if o.elapsed >= DefaultReloadPoll {
			t.Errorf("reported after %v, want at once", o.elapsed)
		}
	})
}

// nginx-provider TC-ERR19: Reload of a stopped nginx is refused
// Owner WI 5.3. Requirements: FR-6, FR-17.
func TestReloadNotRunning(t *testing.T) {
	t.Run("A stopped at Admit", func(t *testing.T) {
		f, _ := helperFixture(t, false)
		s := startOps(t, f, nil)
		tok, _, err := auth.Create(filepath.Dir(s.opsDir), auth.CreateRequest{Name: "writer",
			Scopes: []string{spec.ReadScope, spec.WriteScope}})
		if err != nil {
			t.Fatal(err)
		}
		s.token = tok

		code, b := s.do(http.MethodPost, "/providers/nginx/actions/reload", strings.NewReader("{}"))
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("POST actions/reload = %d %s, want 422", code, b)
		}
		var p problem.Error
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if p.Slug() != problem.NginxNotRunning || p.Extensions["state"] != string(svcctl.Stopped) {
			t.Errorf("problem %s, want nginx-not-running with state stopped", b)
		}
		code, b = s.do(http.MethodGet, "/operations", nil)
		var ops apitypes.List[apitypes.Operation]
		if code != http.StatusOK || json.Unmarshal(b, &ops) != nil || len(ops.Items) != 0 {
			t.Errorf("GET /operations = %d %s, want no operation", code, b)
		}
		recs := s.auditRecords(spec.Reload.Op().ID)
		if !slices.ContainsFunc(recs, func(r systemspec.AuditRecord) bool { return r.Outcome == audit.OutcomeRefused }) {
			t.Errorf("audit records %+v, want one refused", recs)
		}
		if calls := f.svc.Calls(); slices.ContainsFunc(calls, func(c svcctltest.Call) bool {
			return initCall(c.Argv) == "reload" || initCall(c.Argv) == "start"
		}) {
			t.Errorf("init calls %+v include a reload or a start", calls)
		}
	})
	for _, m := range lcModes {
		t.Run("B stopped before the helper/"+m.name, func(t *testing.T) {
			h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
			h.setPID(800)
			writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
			h.set(func(h *lcHarness) {
				h.afterAdmit = func() { h.f.svc.Set(func(m *svcctltest.Model) { m.State = svcctl.Stopped }) }
			})

			o := h.op(spec.Reload.Op())
			o.failed(t, problem.NginxNotRunning)
			if st := ext[string](t, o.err, "state"); st != string(svcctl.Stopped) {
				t.Errorf("state %q, want stopped", st)
			}
			if got, want := h.calls(), []string{"status"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q: no reload and no start", got, want)
			}
			if n := h.nginxTests(); n != 0 {
				t.Errorf("fakenginx ran -t %d times, want none", n)
			}
		})
	}
}

// nginx-provider TC-ERR20: An unconfirmed reload is outcome-unknown; an unidentifiable master is a definite refusal
// Owner WI 5.3. Requirements: FR-6, NFR-2, L-55.
//
// outcome-unknown declares no extensions, so the observation reaches the
// operation in its detail; the helper's result.json keeps it whole.
func TestReloadUnknownOutcomes(t *testing.T) {
	fakeNginxBinary(t)
	t.Run("A unconfirmed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// The respawn sequence is based on a rejected reload: the
			// generation stays, but a worker crashed and was respawned.
			h := newLC(t, ubuntu, svcctltest.Model{State: svcctl.Running, Config: svcctltest.BindConflict})
			writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
			h.useCapture("ubuntu-respawn.jsonl", false, h.f.nginx)

			o := h.op(spec.Reload.Op())
			d := o.failed(t, problem.OutcomeUnknown)
			if !strings.Contains(d, spec.VerdictUnconfirmed) {
				t.Errorf("detail %q does not name the verdict unconfirmed", d)
			}
			res := helperResultOf(t, o)
			var obs spec.ReloadObservation
			if raw, ok := res.Error.Ext["observed"]; !ok || json.Unmarshal(raw, &obs) != nil ||
				obs.Verdict != spec.VerdictUnconfirmed {
				t.Errorf("result.json error %+v, want observed.verdict unconfirmed", res.Error)
			}
			if o.elapsed != testReloadTimeout {
				t.Errorf("reported after %v, want %v", o.elapsed, testReloadTimeout)
			}
		})
	})
	for _, m := range lcModes {
		t.Run("B foreign exe/"+m.name, func(t *testing.T) {
			h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
			writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
			h.useCapture(m.capture+"-valid.jsonl", true, "/usr/bin/sleep")
			before := includeSnapshot(t, h.f.dir)

			d := h.op(spec.Reload.Op()).failed(t, problem.DependencyUnavailable)
			if !strings.HasPrefix(d, masterUnidentified) || !strings.Contains(d, "exe") ||
				!strings.Contains(d, "/usr/bin/sleep") {
				t.Errorf("detail %q, want %q naming the exe /usr/bin/sleep", d, masterUnidentified)
			}
			if got, want := h.calls(), []string{"status"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q: no nginx -t and no reload", got, want)
			}
			if n := h.nginxTests(); n != 0 {
				t.Errorf("fakenginx ran -t %d times, want none", n)
			}
			if after := includeSnapshot(t, h.f.dir); !maps.Equal(before, after) {
				t.Errorf("includeDir changed: %v, was %v", after, before)
			}
		})
	}
}

// nginx-provider TC-ERR21: State convergence failures and init timeouts are bounded and named
// Owner WI 5.3. Requirements: FR-5, NFR-2, FR-17.
//
// Case A's definite other state is the captured one: systemd's start of a
// config that cannot bind ends failed, OpenRC's ends stopped.
func TestConvergenceFailures(t *testing.T) {
	fakeNginxBinary(t)
	const stateTimeout = 30 * time.Second
	for _, m := range lcModes {
		t.Run("A definite other state/"+m.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Stopped, Config: svcctltest.BindConflict})
				writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})

				d := h.op(spec.Start.Op()).failed(t, problem.DependencyUnavailable)
				st, err := h.ctl.Status(context.Background(), svcctltest.Service)
				if err != nil {
					t.Fatal(err)
				}
				want := map[platform.InitSystem]svcctl.State{platform.Systemd: svcctl.Failed,
					platform.OpenRC: svcctl.Stopped}[m.plat.Init]
				if st.State != want || !strings.Contains(d, string(want)) || !strings.Contains(d, st.Raw) {
					t.Errorf("detail %q, want the state %s and %q (the fake is %s)", d, want, st.Raw, st.State)
				}
			})
		})
		t.Run("B indeterminate/"+m.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Stopped})
				writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
				h.set(func(h *lcHarness) {
					h.afterDo = func(verb string) {
						if verb == "start" {
							h.setStatus(startingStatus(m.plat.Init), -1)
						}
					}
				})

				o := h.op(spec.Start.Op())
				d := o.failed(t, problem.OutcomeUnknown)
				if !strings.Contains(d, string(svcctl.Starting)) {
					t.Errorf("detail %q does not name the state starting", d)
				}
				if o.elapsed != stateTimeout {
					t.Errorf("reported after %v, want %v", o.elapsed, stateTimeout)
				}
			})
		})
		t.Run("C init timeout/"+m.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Stopped})
				writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
				h.set(func(h *lcHarness) { h.blockOn = "start" })

				o := h.op(spec.Start.Op())
				d := o.failed(t, problem.DependencyUnavailable)
				if !strings.Contains(d, "did not finish within 30s") {
					t.Errorf("detail %q does not name the init system's timeout", d)
				}
				if o.elapsed != stateTimeout {
					t.Errorf("reported after %v, want %v", o.elapsed, stateTimeout)
				}
			})
		})
	}
}

// nginx-provider TC-ERR41: Reload refuses an invalid on-disk config and changes nothing
// Owner WI 5.3. Requirements: FR-6, FR-17, NFR-2.
func TestReloadRefusesInvalidConfig(t *testing.T) {
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			h := newLC(t, m.plat, svcctltest.Model{State: svcctl.Running, Config: svcctltest.Invalid})
			writeTree(t, h.f.dir, map[string]string{"a.conf": invalidSite})
			p := h.useCapture(m.capture+"-valid.jsonl", true, h.f.nginx)
			gen, procs := h.f.svc.Model().Generation, p.generation()
			before := includeSnapshot(t, h.f.dir)

			o := h.op(spec.Reload.Op())
			o.failed(t, problem.NginxConfigInvalid)
			if out := ext[string](t, o.err, "testOutput"); !strings.Contains(out, `unknown directive "INVALID"`) {
				t.Errorf("testOutput %q, want unknown directive \"INVALID\"", out)
			}
			if got, want := h.calls(), []string{"status", "nginx -t"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q: no reload", got, want)
			}
			if n := h.nginxTests(); n != 1 {
				t.Errorf("fakenginx ran -t %d times, want 1", n)
			}
			if g := h.f.svc.Model().Generation; g != gen {
				t.Errorf("generation %d, want %d", g, gen)
			}
			if after := p.generation(); !maps.Equal(after, procs) {
				t.Errorf("master and workers %v, want %v", after, procs)
			}
			if after := includeSnapshot(t, h.f.dir); !maps.Equal(before, after) {
				t.Errorf("includeDir changed: %v, was %v", after, before)
			}
			if res := helperResultOf(t, o); res.Result != nil || res.Error == nil || res.Error.Ext["observed"] != nil {
				t.Errorf("result.json %+v, want an error with no reload observation", res)
			}
		})
	}
}

// nginx-provider TC-77 (WI 5.3's part): test, reload and test run one at a time in submission order
// Owner WI 5.1. Requirements: FR-13, L-34.
//
// The reload's own outcome does not matter here: its helper runs as a real
// process, with the host's init system.
func TestOneQueuedLaneReload(t *testing.T) {
	f, c := helperFixture(t, false)
	writeTree(t, f.dir, map[string]string{"a.conf": "server {}\n"})
	f.svc.Set(func(m *svcctltest.Model) { m.State = svcctl.Running }) // Admit sees nginx running
	fifo := filepath.Join(t.TempDir(), "block.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	c.Controls = map[string]any{"blockFifo": fifo, "blockCount": 1}
	writeSidecar(t, f.nginx, c)
	s := startOps(t, f, nil)
	tok, _, err := auth.Create(filepath.Dir(s.opsDir), auth.CreateRequest{Name: "writer",
		Scopes: []string{spec.ReadScope, spec.WriteScope}})
	if err != nil {
		t.Fatal(err)
	}
	s.token = tok

	first := s.startTest()
	s.waitStatus(first.ID, apitypes.StatusRunning, 30*time.Second)
	waitFile(t, f.nginx+".json.blockcount", 30*time.Second) // Its -t is blocked on the FIFO
	code, b := s.do(http.MethodPost, "/providers/nginx/actions/reload", strings.NewReader("{}"))
	if code != http.StatusAccepted {
		t.Fatalf("POST actions/reload = %d %s, want 202", code, b)
	}
	var reload apitypes.Operation
	if err := json.Unmarshal(b, &reload); err != nil {
		t.Fatal(err)
	}
	third := s.startTest()
	time.Sleep(300 * time.Millisecond) // Room for a lane that wrongly let them run
	for _, op := range []apitypes.Operation{reload, third} {
		if st := s.get(op.ID).Status; st != apitypes.StatusQueued {
			t.Errorf("operation %s is %s while the first runs, want Queued", op.ID, st)
		}
	}
	if n := fakeProcesses(t, f.nginx); n != 1 {
		t.Errorf("%d nginx processes while the first test is blocked, want 1", n)
	}

	releaseFifo(t, fifo)
	ended := []apitypes.Operation{s.wait(first.ID, 30*time.Second), s.wait(reload.ID, 60*time.Second),
		s.wait(third.ID, 30*time.Second)}
	for i, op := range ended {
		if i > 0 && (op.Started == nil || ended[i-1].Ended == nil || op.Started.Before(*ended[i-1].Ended)) {
			t.Errorf("operation %d started %v, before operation %d ended %v", i, op.Started, i-1, ended[i-1].Ended)
		}
	}
	if r := testResultOf(t, ended[2]); !r.Valid {
		t.Errorf("third operation %+v, want valid", r)
	}
}
