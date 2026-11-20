// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Enable and disable rename with renameat2(RENAME_NOREPLACE), which only
// Linux has (files_linux.go), so these tests run only there, as
// files_test.go does.

package nginx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"maps"
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
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	nexusserver "github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// The toggle tests run the helper in-process, as the lifecycle tests do
// (lcHarness): the svcctl fake is the init system, a fake /proc replays a
// captured reload in lockstep with it, and nginx -t is fakenginx. The files
// they create belong to the test's user, which ownedByTester makes the
// managed owner.

// toggleOutcome is one enable or disable run through its binding.
type toggleOutcome struct {
	res     spec.SiteToggleResult
	err     *problem.Error // Nil when the operation succeeded
	refused bool           // Admit refused it: no operation exists
	dir     string         // The operation's directory
	log     string
}

// newToggleLC is the lifecycle harness of init system plat, nginx in model
// m, with the test's files managed.
func newToggleLC(t *testing.T, plat platform.Info, m svcctltest.Model) *lcHarness {
	t.Helper()
	h := newLC(t, plat, m)
	ownedByTester(t)
	return h
}

// toggle runs o, enable or disable, of site name as the dispatcher and the
// supervisor would, with the helper in-process: Admit, Prepare, the helper,
// Finalize. As lcHarness.op, the calls Admit made are set aside, so
// h.calls() are the helper's.
func (h *lcHarness) toggle(o def.Op, name string) toggleOutcome {
	t := h.t
	t.Helper()
	b, d := h.binding(o)
	ctx := context.Background()
	raw, err := json.Marshal(spec.SiteInput{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	in, err := b.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	h.ops++
	if err := d.Admit(ctx, in); err != nil {
		var p *problem.Error
		if !errors.As(err, &p) {
			t.Fatalf("%s %s: Admit: %v, want a problem", o.ID, name, err)
		}
		return toggleOutcome{err: p, refused: true}
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
	sp, err := d.Prepare(ctx, in, r)
	if err != nil {
		t.Fatalf("%s %s: Prepare: %v", o.ID, name, err)
	}
	var stderr bytes.Buffer
	if code := runHelper(ctx, sp.Dir, &log, &stderr, h.build); code != 0 {
		t.Fatalf("%s %s: helper exited %d: %s", o.ID, name, code, stderr.String())
	}
	out, err := d.Finalize(ctx, in, detach.Exit{}, r)
	res := toggleOutcome{dir: sp.Dir, log: log.String()}
	if err != nil {
		if !errors.As(err, &res.err) {
			t.Fatalf("%s %s: Finalize: %v, want a problem", o.ID, name, err)
		}
		return res
	}
	res.res = out.(spec.SiteToggleResult)
	return res
}

func (o toggleOutcome) succeeded(t *testing.T) spec.SiteToggleResult {
	t.Helper()
	if o.err != nil {
		t.Fatalf("operation failed: %v (refused %v)\nlog:\n%s", o.err, o.refused, o.log)
	}
	return o.res
}

// failed requires the operation to have run and failed with slug, and
// returns its detail.
func (o toggleOutcome) failed(t *testing.T, slug problem.Slug) string {
	t.Helper()
	if o.refused {
		t.Fatalf("Admit refused the operation with %v; want an operation that fails %s", o.err, slug)
	}
	if o.err == nil || o.err.Slug() != slug {
		t.Fatalf("operation ended %v, result %+v; want Failed %s\nlog:\n%s", o.err, o.res, slug, o.log)
	}
	return o.err.Detail
}

// fileID is a file's identity and attributes: what a rename keeps.
type fileID struct {
	ino      uint64
	mode     fs.FileMode
	uid, gid uint32
	sum      string
}

func idOf(t *testing.T, path string) fileID {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	id := fileID{ino: st.Ino, mode: fi.Mode(), uid: st.Uid, gid: st.Gid}
	if fi.Mode().IsRegular() {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		id.sum = sha(string(b))
	}
	return id
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (lstat: %v), want absent", path, err)
	}
}

// ownedSnapshot is every entry of dir with its identity, mode, owner and
// bytes: includeDir's state as NFR-3 compares it.
func ownedSnapshot(t *testing.T, dir string) map[string]fileID {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := map[string]fileID{}
	for _, e := range entries {
		snap[e.Name()] = idOf(t, filepath.Join(dir, e.Name()))
	}
	return snap
}

// inodeSet is the inodes of dir's non-dotfiles.
func inodeSet(t *testing.T, dir string) map[uint64]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	set := map[uint64]string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		set[idOf(t, filepath.Join(dir, e.Name())).ino] = e.Name()
	}
	return set
}

// withoutStatus is calls without the state queries.
func withoutStatus(calls []string) []string {
	return slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return c == "status" })
}

// newLockstep is a lockstepProc over capture file, as useCapture makes, for
// a sequence of reloads.
func newLockstep(t *testing.T, file string, accepts bool, masterExe string) *lockstepProc {
	t.Helper()
	fx := loadCapture(t, file)
	p := &lockstepProc{t: t, name: file, accepts: accepts, master: fx.header.MasterPID,
		pre: snapshotOf(t, fx.pre[len(fx.pre)-1]), exe: map[int]string{fx.header.MasterPID: masterExe}}
	for _, s := range fx.post {
		p.post = append(p.post, snapshotOf(t, s))
	}
	return p
}

// procSeq is the /proc of an operation that reloads more than once: each
// reload replays the next capture.
type procSeq struct {
	mu    sync.Mutex
	steps []*lockstepProc
	i     int
}

func (s *procSeq) cur() *lockstepProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.steps[s.i]
}

// next moves to the next capture, for the next reload.
func (s *procSeq) next() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.i++
}

func (s *procSeq) Snapshot() (map[int]Proc, error)      { return s.cur().Snapshot() }
func (s *procSeq) Exe(pid int) (string, error)          { return s.cur().Exe(pid) }
func (s *procSeq) RealUID(pid int) (uint32, error)      { return s.cur().RealUID(pid) }
func (s *procSeq) PIDNamespace(pid int) (string, error) { return s.cur().PIDNamespace(pid) }
func (s *procSeq) reload(accepted bool)                 { s.cur().reload(accepted) }

// useSequence makes steps the helper's /proc, the first one's master
// nginx's main PID.
func (h *lcHarness) useSequence(steps ...*lockstepProc) *procSeq {
	s := &procSeq{steps: steps}
	h.set(func(h *lcHarness) { h.proc, h.reloaded = s, s.reload })
	h.setPID(steps[0].master)
	return s
}

// toggleServer is a real nexus server whose detached operations run the
// helper in-process with h's env, so the svcctl fake and the fake /proc
// apply. It returns an opsServer with a token of the read and write scopes.
func toggleServer(t *testing.T, h *lcHarness) *opsServer {
	t.Helper()
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
	inproc := &inprocLauncher{h: h, done: map[string]chan struct{}{}}
	launcher, err := detach.New(detach.Config{OpsDir: opsDir, Executable: "/usr/sbin/nexusd", Logger: log},
		detach.Options{Launch: inproc.launch, UnitState: inproc.state, Signal: inproc.signal,
			ListUnits: func(context.Context) ([]string, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: db, Audit: alog, Dir: opsDir, Host: "h1", Logger: log,
		PollInterval: 20 * time.Millisecond, Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	h.f.p.UseOpsDir(opsDir)
	reg, err := core.New([]provider.Provider{h.f.p}, core.Options{Host: "h1", Version: "v0.0.0-test", Logger: log,
		Audit: alog, Ops: mgr, Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true,
			Decode: h.f.decode}},
		Platform: h.f.p.plat})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "writer",
		Scopes: []string{spec.ReadScope, spec.WriteScope}})
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
		inproc.wait()
		if err := db.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
	})
	return s
}

// inprocLauncher is detach's process seams with the helper run in a
// goroutine instead of `nexusd op-supervise`: it runs the operation's
// helper with h.build and writes exit.json as the supervisor does.
type inprocLauncher struct {
	h    *lcHarness
	mu   sync.Mutex
	done map[string]chan struct{} // By unit; closed once exit.json is written
	wg   sync.WaitGroup
}

func (l *inprocLauncher) launch(_ context.Context, inv detach.Invocation) error {
	ch := make(chan struct{})
	l.mu.Lock()
	l.done[inv.Unit] = ch
	l.mu.Unlock()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer close(ch)
		started := time.Now()
		var out, stderr syncBuffer
		code := runHelper(context.Background(), inv.Dir, &out, &stderr, l.h.build)
		if code != 0 {
			l.h.t.Errorf("helper for %s exited %d: %s", inv.Unit, code, stderr.String())
		}
		if err := detach.WriteExit(inv.Dir, detach.Exit{ExitCode: code, Started: started, Ended: time.Now()}); err != nil {
			l.h.t.Errorf("exit.json for %s: %v", inv.Unit, err)
		}
	}()
	return nil
}

func (l *inprocLauncher) state(_ context.Context, unit string) (detach.UnitState, error) {
	l.mu.Lock()
	ch, ok := l.done[unit]
	l.mu.Unlock()
	if ok {
		select {
		case <-ch:
		default:
			return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
		}
	}
	return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
}

func (l *inprocLauncher) signal(context.Context, string, syscall.Signal) error {
	return errors.New("the in-process helper cannot be signalled")
}

func (l *inprocLauncher) wait() { l.wg.Wait() }

// post POSTs o's action on site name and returns the accepted operation.
func (s *opsServer) post(o def.Op, name string) apitypes.Operation {
	s.t.Helper()
	code, b := s.do(http.MethodPost, "/providers/nginx/sites/"+name+"/"+o.Action, strings.NewReader("{}"))
	if code != http.StatusAccepted {
		s.t.Fatalf("POST sites/%s/%s = %d %s, want 202", name, o.Action, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		s.t.Fatalf("%s: %v", b, err)
	}
	return op
}

func toggleResultOf(t *testing.T, op apitypes.Operation) spec.SiteToggleResult {
	t.Helper()
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s: %s, error %+v", op.ID, op.Status, op.Error)
	}
	var r spec.SiteToggleResult
	if err := decodeStrict(op.Result, &r); err != nil {
		t.Fatalf("result %s: %v", op.Result, err)
	}
	return r
}

// auditOutcomes are the outcomes of action's audit records on target, the
// site's name.
func (s *opsServer) auditOutcomes(action, target string) []string {
	s.t.Helper()
	var out []string
	for _, r := range s.auditRecords(action) {
		if r.Target == target {
			out = append(out, r.Outcome)
		}
	}
	return out
}

// nginx-provider TC-93: Enable renames, tests and reloads
// Owner WI 6.2. Requirements: FR-9, FR-6.
func TestEnable(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			h := newToggleLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
			writeTree(t, h.f.dir, map[string]string{"a.conf.disabled": validSite})
			p := h.useCapture(m.capture+"-valid.jsonl", true, h.f.nginx)
			was := idOf(t, filepath.Join(h.f.dir, "a.conf.disabled"))
			s := toggleServer(t, h)

			op := s.wait(s.post(spec.Enable.Op(), "a").ID, 30*time.Second)
			r := toggleResultOf(t, op)
			if r.Name != "a" || r.FileName != "a.conf" || !r.Enabled || !r.Reloaded || !r.Changed ||
				r.Reload == nil || r.Reload.Verdict != spec.VerdictConfirmed || r.Reload.MasterPID != p.master {
				t.Errorf("result %+v (reload %+v), want a, a.conf, enabled, reloaded, changed, verdict confirmed "+
					"under master %d", r, r.Reload, p.master)
			}
			if !strings.Contains(r.TestOutput, "test is successful") {
				t.Errorf("testOutput %q, want nginx -t's output", r.TestOutput)
			}
			if now := idOf(t, filepath.Join(h.f.dir, "a.conf")); now != was {
				t.Errorf("a.conf is %+v, want the old a.conf.disabled's inode, bytes, mode and owner %+v", now, was)
			}
			absent(t, filepath.Join(h.f.dir, "a.conf.disabled"))
			if got, want := withoutStatus(h.calls()), []string{"nginx -t", "reload"}; !slices.Equal(got, want) {
				t.Errorf("calls %q (all %q), want %q", got, h.calls(), want)
			}
			if n := h.nginxTests(); n != 1 {
				t.Errorf("fakenginx ran -t %d times, want 1", n)
			}
			outcomes := s.auditOutcomes(spec.Enable.Op().ID, "a")
			if !slices.Contains(outcomes, "requested") || !slices.Contains(outcomes, string(apitypes.StatusSucceeded)) {
				t.Errorf("audit outcomes on %s: %q, want requested and succeeded", "a", outcomes)
			}
		})
	}
}

// nginx-provider TC-94: Disable renames, tests and reloads
// Owner WI 6.2. Requirements: FR-10.
func TestDisable(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			h := newToggleLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
			writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
			h.useCapture(m.capture+"-valid.jsonl", true, h.f.nginx)
			was := idOf(t, filepath.Join(h.f.dir, "a.conf"))
			s := toggleServer(t, h)

			op := s.wait(s.post(spec.Disable.Op(), "a").ID, 30*time.Second)
			r := toggleResultOf(t, op)
			if r.Enabled || !r.Reloaded || !r.Changed || r.FileName != "a.conf.disabled" || r.Reload == nil ||
				r.Reload.Verdict != spec.VerdictConfirmed {
				t.Errorf("result %+v, want a.conf.disabled, not enabled, reloaded, changed, verdict confirmed", r)
			}
			if now := idOf(t, filepath.Join(h.f.dir, "a.conf.disabled")); now != was {
				t.Errorf("a.conf.disabled is %+v, want the old a.conf's %+v", now, was)
			}
			absent(t, filepath.Join(h.f.dir, "a.conf"))
			if got, want := withoutStatus(h.calls()), []string{"nginx -t", "reload"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q", got, want)
			}
			b, _ := h.binding(spec.Disable.Op())
			if risk := b.Op().Risk; risk != def.RiskDestructive {
				t.Errorf("disable's risk is %s, want destructive", risk)
			}
			if b, _ := h.binding(spec.Enable.Op()); b.Op().Risk != def.RiskMutating {
				t.Errorf("enable's risk is %s, want mutating", b.Op().Risk)
			}
		})
	}
}

// toggleCase is one path of an enable or disable: the files it starts
// from, nginx's model, and the /proc and hooks it runs with.
type toggleCase struct {
	name  string
	plat  platform.Info
	model svcctltest.Model
	files map[string]string
	op    def.Op
	site  string
	setup func(h *lcHarness)
}

func (c toggleCase) harness(t *testing.T, extra map[string]string) *lcHarness {
	t.Helper()
	h := newToggleLC(t, c.plat, c.model)
	writeTree(t, h.f.dir, c.files)
	writeTree(t, h.f.dir, extra)
	if c.setup != nil {
		c.setup(h)
	}
	return h
}

// validCapture is setup with the mode's capture of an accepted reload.
func validCapture(capture string) func(h *lcHarness) {
	return func(h *lcHarness) { h.useCapture(capture+"-valid.jsonl", true, h.f.nginx) }
}

const (
	upstreamSite = "upstream be {\n    server 127.0.0.1:9;\n}\n"
	useSite      = "server {\n    listen 8182;\n    location / {\n        proxy_pass http://be;\n    }\n}\n"
)

// invalidEnable is TC-ERR27's case, and breakingDisable TC-ERR28's.
// The running nginx's master is identified from capture, the mode's.
func invalidEnable(mode string, plat platform.Info, capture string) toggleCase {
	return toggleCase{name: "enable invalid/" + mode, plat: plat, model: svcctltest.Model{State: svcctl.Running},
		files: map[string]string{"a.conf.disabled": invalidSite}, op: spec.Enable.Op(), site: "a",
		setup: validCapture(capture)}
}

func breakingDisable(mode string, plat platform.Info, capture string) toggleCase {
	return toggleCase{name: "disable upstream/" + mode, plat: plat, model: svcctltest.Model{State: svcctl.Running},
		files: map[string]string{"up.conf": upstreamSite, "use.conf": useSite}, op: spec.Disable.Op(), site: "up",
		setup: validCapture(capture)}
}

// reloadRejected is TC-ERR31's case on systemd: enabling p makes the model
// bind-conflict, so systemctl exits 0 and the generation stays. The
// rollback's nginx -t (the second) makes the model valid again when
// rollbackConfirms, and its reload then confirms; otherwise the rollback
// reload is answered as rejected and the fake /proc shows a respawned
// worker, which is unconfirmed. The captures were taken under different
// masters, so the confirming one names its own as the main PID.
func reloadRejected(rollbackConfirms bool) toggleCase {
	name := "reload rejected, rollback reload unconfirmed"
	if rollbackConfirms {
		name = "reload rejected, rollback reload confirmed"
	}
	return toggleCase{name: name, plat: ubuntu, model: svcctltest.Model{State: svcctl.Running},
		files: map[string]string{"p.conf.disabled": validSite}, op: spec.Enable.Op(), site: "p",
		setup: func(h *lcHarness) {
			first := newLockstep(h.t, "ubuntu-bind-rejected.jsonl", false, h.f.nginx)
			second := newLockstep(h.t, "ubuntu-respawn.jsonl", false, h.f.nginx)
			if rollbackConfirms {
				second = newLockstep(h.t, "ubuntu-valid.jsonl", true, h.f.nginx)
			}
			seq := h.useSequence(first, second)
			tests := 0
			h.set(func(h *lcHarness) {
				h.afterTest = func() {
					tests++
					switch tests {
					case 1:
						h.f.svc.Set(func(m *svcctltest.Model) { m.Config = svcctltest.BindConflict })
					case 2:
						seq.next()
						if rollbackConfirms {
							h.f.svc.Set(func(m *svcctltest.Model) { m.Config = svcctltest.Valid })
							h.setPID(second.master)
						}
					}
				}
			})
		}}
}

// nginx-provider TC-95: No nginx operation ever deletes a file
// Owner WI 6.2. Requirements: OOS-4, FR-10, NFR-3.
//
// Every path runs twice: with renameat2(RENAME_NOREPLACE), and with its
// link-then-unlink fallback (renameat2 answering ENOSYS), whose unlink
// removes the old name of an inode the new name already holds.
func TestNoOperationDeletes(t *testing.T) {
	fakeNginxBinary(t)
	var cases []toggleCase
	for _, m := range lcModes {
		cases = append(cases,
			toggleCase{name: "enable/" + m.name, plat: m.plat, model: svcctltest.Model{State: svcctl.Running},
				files: map[string]string{"a.conf.disabled": validSite}, op: spec.Enable.Op(), site: "a",
				setup: validCapture(m.capture)},
			toggleCase{name: "disable/" + m.name, plat: m.plat, model: svcctltest.Model{State: svcctl.Running},
				files: map[string]string{"a.conf": validSite}, op: spec.Disable.Op(), site: "a",
				setup: validCapture(m.capture)},
			invalidEnable(m.name, m.plat, m.capture), breakingDisable(m.name, m.plat, m.capture))
	}
	cases = append(cases, reloadRejected(true), reloadRejected(false))
	// Three bystander sites, in both states.
	tree := map[string]string{"s1.conf": validSite, "s2.conf": validSite, "s3.conf.disabled": validSite}
	for _, fallback := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s/fallback=%v", c.name, fallback), func(t *testing.T) {
				if fallback {
					old := renameat2
					renameat2 = func(int, string, int, string, uint) error { return unix.ENOSYS }
					t.Cleanup(func() { renameat2 = old })
				}
				synctest.Test(t, func(t *testing.T) {
					h := c.harness(t, tree)
					before := inodeSet(t, h.f.dir)
					o := h.toggle(c.op, c.site)
					if o.refused {
						t.Fatalf("refused: %v", o.err)
					}
					after := inodeSet(t, h.f.dir)
					for ino, name := range before {
						if _, ok := after[ino]; !ok {
							t.Errorf("inode %d (%s) is gone after %s: after %v, before %v", ino, name, c.name,
								after, before)
						}
					}
					if len(after) != len(before) {
						t.Errorf("%d non-dotfiles after, %d before: a rename neither adds nor removes one",
							len(after), len(before))
					}
				})
			})
		}
	}
	t.Run("removal calls", testRemovalCalls)
}

// testRemovalCalls is TC-95's grep, done on the syntax tree: every call in
// the package's sources that removes a directory entry, by the function
// that makes it. Each one removes a name whose inode survives under
// another, or a file the same operation created:
//   - unlinkAt is the unlinkat(2) wrapper itself;
//   - WriteAtomic removes its own temporary file when the write fails;
//   - Restore removes a file the failed operation created (Meta.Exists
//     false);
//   - linkThenUnlink, RenameNoReplace's fallback, unlinks from after linking
//     to (the inode keeps the name to), or to again when from cannot be
//     unlinked (the inode keeps from);
//   - writeJSONAtomic, shadowBin.cleanup and removeContent remove the
//     helper's own files in the operation's directory, never in includeDir.
//
// Any other removal, enable and disable included, fails the test.
func testRemovalCalls(t *testing.T) {
	removers := map[string]bool{"unix.Unlinkat": true, "unix.Unlink": true, "unix.Rmdir": true,
		"syscall.Unlink": true, "syscall.Unlinkat": true, "syscall.Rmdir": true, "os.Remove": true,
		"os.RemoveAll": true, "unlinkAt": true}
	want := map[string]int{
		"files.go unlinkAt unix.Unlinkat":             1,
		"files.go WriteAtomic unlinkAt":               1,
		"files.go Restore unlinkAt":                   1,
		"files_linux.go linkThenUnlink unlinkAt":      2,
		"helperio.go writeJSONAtomic os.Remove":       1,
		"shadow_linux.go shadowBin.cleanup os.Remove": 1,
		"write.go removeContent os.Remove":            1,
	}
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	fset := token.NewFileSet()
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				if id, ok := fn.Recv.List[0].Type.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := ""
				switch f := call.Fun.(type) {
				case *ast.Ident:
					callee = f.Name
				case *ast.SelectorExpr:
					if x, ok := f.X.(*ast.Ident); ok {
						callee = x.Name + "." + f.Sel.Name
					}
				}
				if removers[callee] {
					got[src+" "+name+" "+callee]++
				}
				return true
			})
		}
	}
	if !maps.Equal(got, want) {
		t.Errorf("removal calls by function:\n got %v\nwant %v", got, want)
	}
}

// nginx-provider TC-E26: Enabling an already-enabled config changes nothing
// Owner WI 6.2. Requirements: FR-9.
func TestEnableAlreadyEnabled(t *testing.T) {
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			h := newToggleLC(t, m.plat, svcctltest.Model{State: svcctl.Running})
			h.setPID(900)
			writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
			hashes := treeHashes(t, h.f.dir)

			r := h.toggle(spec.Enable.Op(), "a").succeeded(t)
			if r.Changed || r.Reloaded || !r.Enabled || r.FileName != "a.conf" || r.Reload != nil {
				t.Errorf("result %+v, want unchanged, not reloaded, enabled a.conf", r)
			}
			if calls := h.calls(); len(calls) != 0 {
				t.Errorf("calls %q, want none: no nginx -t and no reload", calls)
			}
			if n := h.nginxTests(); n != 0 {
				t.Errorf("fakenginx ran -t %d times, want none", n)
			}
			if after := treeHashes(t, h.f.dir); !maps.Equal(hashes, after) {
				t.Errorf("hashes %v, were %v", after, hashes)
			}
		})
	}
}

// nginx-provider TC-E27: Enable or disable while nginx is stopped renames and tests, but does not start nginx
// Owner WI 6.2. Requirements: FR-9, FR-10.
func TestEnableWhileStopped(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			h := newToggleLC(t, m.plat, svcctltest.Model{State: svcctl.Stopped})
			writeTree(t, h.f.dir, map[string]string{"a.conf.disabled": validSite, "b.conf": validSite})
			a := idOf(t, filepath.Join(h.f.dir, "a.conf.disabled"))
			b := idOf(t, filepath.Join(h.f.dir, "b.conf"))
			gen := h.f.svc.Model().Generation

			r := h.toggle(spec.Enable.Op(), "a").succeeded(t)
			if now := idOf(t, filepath.Join(h.f.dir, "a.conf")); now.ino != a.ino {
				t.Errorf("a.conf has inode %d, want the old a.conf.disabled's %d", now.ino, a.ino)
			}
			if !r.Changed || !r.Enabled || r.Reloaded || r.Reload != nil {
				t.Errorf("enable result %+v, want changed, enabled, not reloaded", r)
			}
			if got, want := h.calls(), []string{"status", "nginx -t"}; !slices.Equal(got, want) {
				t.Errorf("enable calls %q, want %q: no start and no reload", got, want)
			}
			if n := h.nginxTests(); n != 1 {
				t.Errorf("fakenginx ran -t %d times after the enable, want 1", n)
			}

			o := h.toggle(spec.Disable.Op(), "b")
			r = o.succeeded(t)
			if now := idOf(t, filepath.Join(h.f.dir, "b.conf.disabled")); now.ino != b.ino {
				t.Errorf("b.conf.disabled has inode %d, want the old b.conf's %d", now.ino, b.ino)
			}
			absent(t, filepath.Join(h.f.dir, "b.conf"))
			if !r.Changed || r.Enabled || r.Reloaded || r.FileName != "b.conf.disabled" {
				t.Errorf("disable result %+v, want changed, not enabled, not reloaded, b.conf.disabled", r)
			}
			if got, want := h.calls(), []string{"status", "nginx -t"}; !slices.Equal(got, want) {
				t.Errorf("disable calls %q, want %q: no start and no reload", got, want)
			}
			if n := h.nginxTests(); n != 2 {
				t.Errorf("fakenginx ran -t %d times after both, want 2", n)
			}
			if !strings.Contains(o.log, "nginx is stopped; nothing was reloaded") {
				t.Errorf("log %q does not say nothing was reloaded", o.log)
			}
			if md := h.f.svc.Model(); md.State != svcctl.Stopped || md.Generation != gen {
				t.Errorf("nginx is %s at generation %d, want still stopped at %d", md.State, md.Generation, gen)
			}
			for _, c := range h.f.svc.Calls() {
				if what := initCall(c.Argv); what != "status" && what != "exists" {
					t.Errorf("init call %q, want status only", c.Argv)
				}
			}
		})
	}
}

// nginx-provider TC-E28: Disabling an already-disabled config changes nothing
// Owner WI 6.2. Requirements: FR-10.
func TestDisableAlreadyDisabled(t *testing.T) {
	h := newToggleLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, h.f.dir, map[string]string{"a.conf.disabled": validSite})
	before := ownedSnapshot(t, h.f.dir)

	r := h.toggle(spec.Disable.Op(), "a").succeeded(t)
	if r.Changed || r.Reloaded || r.Enabled || r.FileName != "a.conf.disabled" {
		t.Errorf("result %+v, want unchanged, not reloaded, disabled a.conf.disabled", r)
	}
	if calls := h.calls(); len(calls) != 0 {
		t.Errorf("calls %q, want none: no test and no reload", calls)
	}
	if n := h.nginxTests(); n != 0 {
		t.Errorf("fakenginx ran -t %d times, want none", n)
	}
	if after := ownedSnapshot(t, h.f.dir); !maps.Equal(before, after) {
		t.Errorf("includeDir %v, was %v", after, before)
	}
}

// nginx-provider TC-ERR27: Enable of an invalid config renames back and fails
// Owner WI 6.2. Requirements: FR-9, NFR-3, FR-17.
func TestEnableInvalidRollsBack(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			c := invalidEnable(m.name, m.plat, m.capture)
			h := c.harness(t, nil)
			before := ownedSnapshot(t, h.f.dir)

			o := h.toggle(c.op, c.site)
			o.failed(t, problem.NginxConfigInvalid)
			if !ext[bool](t, o.err, "rolledBack") {
				t.Errorf("rolledBack false, want true")
			}
			if out := ext[string](t, o.err, "testOutput"); !strings.Contains(out, `unknown directive "INVALID"`) ||
				!strings.Contains(out, "a.conf:") {
				t.Errorf("testOutput %q, want the unknown directive in a.conf", out)
			}
			if got, want := h.calls(), []string{"status", "nginx -t"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q: no reload", got, want)
			}
			if after := ownedSnapshot(t, h.f.dir); !maps.Equal(before, after) {
				t.Errorf("includeDir %v, was %v: a.conf.disabled is not back as it was", after, before)
			}
		})
	}
}

// nginx-provider TC-ERR28: A disable that breaks another file's upstream renames back
// Owner WI 6.2. Requirements: FR-10, NFR-3.
func TestDisableBreakingUpstreamRollsBack(t *testing.T) {
	fakeNginxBinary(t)
	for _, m := range lcModes {
		t.Run(m.name, func(t *testing.T) {
			c := breakingDisable(m.name, m.plat, m.capture)
			h := c.harness(t, nil)
			before := ownedSnapshot(t, h.f.dir)

			o := h.toggle(c.op, c.site)
			o.failed(t, problem.NginxConfigInvalid)
			if out := ext[string](t, o.err, "testOutput"); !strings.Contains(out, `host not found in upstream "be"`) {
				t.Errorf("testOutput %q, want host not found in upstream \"be\"", out)
			}
			if !ext[bool](t, o.err, "rolledBack") {
				t.Errorf("rolledBack false, want true")
			}
			if got, want := h.calls(), []string{"status", "nginx -t"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q: no reload", got, want)
			}
			if after := ownedSnapshot(t, h.f.dir); !maps.Equal(before, after) {
				t.Errorf("includeDir %v, was %v: up.conf is not back as it was", after, before)
			}
		})
	}
}

// nginx-provider TC-ERR29: Enable and disable refuse unmanaged, conflicting and missing entries at admission
// Owner WI 6.2. Requirements: FR-9, FR-10, FR-17.
func TestToggleAdmitRefusals(t *testing.T) {
	h := newToggleLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	outside := filepath.Join(t.TempDir(), "elsewhere.conf")
	writeTree(t, filepath.Dir(outside), map[string]string{"elsewhere.conf": validSite})
	writeTree(t, h.f.dir, map[string]string{"c.conf": validSite, "c.conf.disabled": validSite})
	if err := os.Symlink(outside, filepath.Join(h.f.dir, "s.conf.disabled")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(h.f.dir, "d.conf.disabled"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := ownedSnapshot(t, h.f.dir)
	s := toggleServer(t, h)

	for _, tc := range []struct {
		op               def.Op
		name             string
		code             int
		slug             problem.Slug
		reason, fileName string
	}{
		{spec.Enable.Op(), "s", http.StatusUnprocessableEntity, problem.NginxConfigUnmanaged, spec.ReasonSymlink, "s.conf.disabled"},
		{spec.Enable.Op(), "c", http.StatusUnprocessableEntity, problem.NginxConfigUnmanaged, spec.ReasonConflict, "c.conf"},
		{spec.Enable.Op(), "m", http.StatusNotFound, problem.NotFound, "", ""},
		{spec.Enable.Op(), "d", http.StatusUnprocessableEntity, problem.NginxConfigUnmanaged, spec.ReasonNotRegular, "d.conf.disabled"},
		{spec.Disable.Op(), "c", http.StatusUnprocessableEntity, problem.NginxConfigUnmanaged, spec.ReasonConflict, "c.conf"},
	} {
		t.Run(tc.op.Action+" "+tc.name, func(t *testing.T) {
			code, b := s.do(http.MethodPost, "/providers/nginx/sites/"+tc.name+"/"+tc.op.Action, strings.NewReader("{}"))
			if code != tc.code {
				t.Fatalf("POST = %d %s, want %d", code, b, tc.code)
			}
			var p problem.Error
			if err := json.Unmarshal(b, &p); err != nil {
				t.Fatalf("%s: %v", b, err)
			}
			if p.Slug() != tc.slug {
				t.Errorf("problem %s, want %s", b, tc.slug)
			}
			if tc.reason != "" && (p.Extensions["reason"] != tc.reason || p.Extensions["fileName"] != tc.fileName) {
				t.Errorf("problem %s, want reason %s and fileName %s", b, tc.reason, tc.fileName)
			}
			if outcomes := s.auditOutcomes(tc.op.ID, tc.name); !slices.Contains(outcomes, audit.OutcomeRefused) {
				t.Errorf("audit outcomes %q, want refused", outcomes)
			}
		})
	}
	code, b := s.do(http.MethodGet, "/operations", nil)
	var list apitypes.List[apitypes.Operation]
	if code != http.StatusOK || json.Unmarshal(b, &list) != nil || len(list.Items) != 0 {
		t.Errorf("GET /operations = %d %s, want no operation", code, b)
	}
	if calls := h.calls(); len(calls) != 0 {
		t.Errorf("calls %q, want none", calls)
	}
	if after := ownedSnapshot(t, h.f.dir); !maps.Equal(before, after) {
		t.Errorf("includeDir %v, was %v", after, before)
	}
}

// nginx-provider TC-ERR30: An entry that becomes unmanaged after admission is refused by the helper
// Owner WI 6.2. Requirements: FR-9, FR-10, ASM-11.
func TestToggleHelperRecheck(t *testing.T) {
	h := newToggleLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, h.f.dir, map[string]string{"a.conf.disabled": validSite})
	outside := filepath.Join(t.TempDir(), "elsewhere.conf")
	writeTree(t, filepath.Dir(outside), map[string]string{"elsewhere.conf": validSite})
	link := filepath.Join(h.f.dir, "a.conf.disabled")
	var linked fileID
	h.set(func(h *lcHarness) {
		h.afterAdmit = func() {
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			linked = idOf(t, link)
		}
	})

	o := h.toggle(spec.Enable.Op(), "a")
	o.failed(t, problem.NginxConfigUnmanaged)
	if r := ext[string](t, o.err, "reason"); r != spec.ReasonSymlink {
		t.Errorf("reason %q, want symlink", r)
	}
	if calls := h.calls(); len(calls) != 0 {
		t.Errorf("calls %q, want none: no test and no reload", calls)
	}
	if n := h.nginxTests(); n != 0 {
		t.Errorf("fakenginx ran -t %d times, want none", n)
	}
	if now := idOf(t, link); now != linked {
		t.Errorf("the symlink is %+v, was %+v", now, linked)
	}
	if target, err := os.Readlink(link); err != nil || target != outside {
		t.Errorf("readlink: %q, %v; want %q", target, err, outside)
	}
	absent(t, filepath.Join(h.f.dir, "a.conf"))
}

// nginx-provider TC-ERR31: A reload rejected after a toggle renames back and reloads again
// Owner WI 6.2. Requirements: FR-9, FR-10, FR-6, NFR-3.
func TestToggleReloadRejected(t *testing.T) {
	fakeNginxBinary(t)
	t.Run("A rollback reload confirmed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := reloadRejected(true)
			h := c.harness(t, nil)
			before := ownedSnapshot(t, h.f.dir)

			o := h.toggle(c.op, c.site)
			o.failed(t, problem.NginxReloadFailed)
			if !ext[bool](t, o.err, "rolledBack") {
				t.Errorf("rolledBack false, want true")
			}
			if obs := ext[spec.ReloadObservation](t, o.err, "observed"); obs.Verdict != spec.VerdictRejected ||
				obs.InitExitCode != 0 {
				t.Errorf("observed %+v, want verdict rejected with init exit 0", obs)
			}
			if back := ext[spec.ReloadObservation](t, o.err, "rollbackReload"); back.Verdict != spec.VerdictConfirmed {
				t.Errorf("rollbackReload %+v, want verdict confirmed", back)
			}
			if got, want := withoutStatus(h.calls()), []string{"nginx -t", "reload", "nginx -t", "reload"}; !slices.Equal(got, want) {
				t.Errorf("calls %q (all %q), want %q", got, h.calls(), want)
			}
			if got, want := h.calls(), []string{"status", "nginx -t", "reload", "nginx -t", "status", "reload"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q: the master is identified again before the second reload", got, want)
			}
			if after := ownedSnapshot(t, h.f.dir); !maps.Equal(before, after) {
				t.Errorf("includeDir %v, was %v: p.conf.disabled is not back as it was", after, before)
			}
		})
	})
	t.Run("B rollback reload unconfirmed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := reloadRejected(false)
			h := c.harness(t, nil)
			before := ownedSnapshot(t, h.f.dir)

			o := h.toggle(c.op, c.site)
			d := o.failed(t, problem.OutcomeUnknown)
			if !strings.Contains(d, "p.conf.disabled") {
				t.Errorf("detail %q does not name p.conf.disabled", d)
			}
			res := helperResultOf(t, lcOutcome{dir: o.dir})
			var back spec.ReloadObservation
			if raw, ok := res.Error.Ext["rollbackReload"]; !ok || json.Unmarshal(raw, &back) != nil ||
				back.Verdict != spec.VerdictUnconfirmed {
				t.Errorf("result.json error %+v, want rollbackReload.verdict unconfirmed", res.Error)
			}
			if got, want := withoutStatus(h.calls()), []string{"nginx -t", "reload", "nginx -t", "reload"}; !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q", got, want)
			}
			if after := ownedSnapshot(t, h.f.dir); !maps.Equal(before, after) {
				t.Errorf("includeDir %v, was %v: p.conf.disabled is not back as it was", after, before)
			}
		})
	})
}

// nginx-provider TC-ERR42: Enable and disable refuse, before renaming, when a running nginx's master cannot be identified
// Owner WI 6.2. Requirements: FR-9, FR-10, FR-6, FR-17, NFR-2, L-20, L-55.
func TestToggleRefusesUnidentifiableMaster(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plat   platform.Info
		setup  func(h *lcHarness)
		reason string
	}{
		{"A openrc without a daemon record", alpine, nil, "init system reports no main pid"},
		{"B systemd with a foreign exe", ubuntu, func(h *lcHarness) {
			h.useCapture("ubuntu-valid.jsonl", true, "/usr/bin/sleep")
		}, "exe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newToggleLC(t, tc.plat, svcctltest.Model{State: svcctl.Running})
			if tc.setup != nil {
				tc.setup(h)
			}
			writeTree(t, h.f.dir, map[string]string{"a.conf.disabled": validSite, "b.conf": validSite})
			before := ownedSnapshot(t, h.f.dir)
			for _, step := range []struct {
				op   def.Op
				site string
			}{{spec.Enable.Op(), "a"}, {spec.Disable.Op(), "b"}} {
				o := h.toggle(step.op, step.site)
				d := o.failed(t, problem.DependencyUnavailable)
				if !strings.HasPrefix(d, masterUnidentified) || !strings.Contains(strings.ToLower(d), tc.reason) {
					t.Errorf("%s %s: detail %q, want %q naming %q", step.op.Action, step.site, d, masterUnidentified,
						tc.reason)
				}
				if rb, ok := o.err.Extensions["rolledBack"]; ok {
					t.Errorf("%s %s: rolledBack %v, want none: nothing was changed", step.op.Action, step.site, rb)
				}
				if got, want := h.calls(), []string{"status"}; !slices.Equal(got, want) {
					t.Errorf("%s %s: calls %q, want %q: no nginx -t and no reload", step.op.Action, step.site, got,
						want)
				}
				if after := ownedSnapshot(t, h.f.dir); !maps.Equal(before, after) {
					t.Errorf("%s %s: includeDir %v, was %v", step.op.Action, step.site, after, before)
				}
			}
			absent(t, filepath.Join(h.f.dir, "a.conf"))
			absent(t, filepath.Join(h.f.dir, "b.conf.disabled"))
			if n := h.nginxTests(); n != 0 {
				t.Errorf("fakenginx ran -t %d times, want none", n)
			}
			for _, c := range h.f.svc.Calls() {
				if initCall(c.Argv) == "reload" {
					t.Errorf("the svcctl fake recorded a reload: %q", c.Argv)
				}
			}
		})
	}
}

// plantingProc is a /proc whose first Snapshot runs plant: it lets a test
// act between the helper's re-check and its rename, while the master is
// identified.
type plantingProc struct {
	ProcReader
	once  sync.Once
	plant func()
}

func (p *plantingProc) Snapshot() (map[int]Proc, error) {
	p.once.Do(p.plant)
	return p.ProcReader.Snapshot()
}

// A name that appears between the helper's re-check and its rename is a
// conflict that changes nothing: RenameNoReplace never replaces it.
func TestToggleTargetAppearsBeforeRename(t *testing.T) {
	fakeNginxBinary(t)
	h := newToggleLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, h.f.dir, map[string]string{"a.conf.disabled": validSite})
	p := h.useCapture("ubuntu-valid.jsonl", true, h.f.nginx)
	h.set(func(h *lcHarness) {
		h.proc = &plantingProc{ProcReader: p, plant: func() {
			writeTree(t, h.f.dir, map[string]string{"a.conf": "planted\n"})
		}}
	})
	was := idOf(t, filepath.Join(h.f.dir, "a.conf.disabled"))

	o := h.toggle(spec.Enable.Op(), "a")
	o.failed(t, problem.NginxConfigUnmanaged)
	if r, f := ext[string](t, o.err, "reason"), ext[string](t, o.err, "fileName"); r != spec.ReasonConflict ||
		f != "a.conf" {
		t.Errorf("reason %q and fileName %q, want conflict and a.conf", r, f)
	}
	if now := idOf(t, filepath.Join(h.f.dir, "a.conf.disabled")); now != was {
		t.Errorf("a.conf.disabled is %+v, was %+v", now, was)
	}
	if b, err := os.ReadFile(filepath.Join(h.f.dir, "a.conf")); err != nil || string(b) != "planted\n" {
		t.Errorf("a.conf holds %q (%v), want the planted file untouched", b, err)
	}
	if got, want := h.calls(), []string{"status"}; !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q: no nginx -t and no reload", got, want)
	}
}
