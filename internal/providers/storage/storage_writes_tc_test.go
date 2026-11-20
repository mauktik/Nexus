// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-175 to
// TC-STG-183, TC-STG-185, TC-STG-E2, E6 to E9 and TC-STG-ERR4 to ERR11,
// ERR13 and ERR14 (owner WI 4.5): scrub start and stop and snapshot create,
// their admission, Prepare, Finalize and LostDetail, and the storage helper.
//
// The bindings are composed in WI 5.1, so "through the Dispatcher" runs a
// real core.Dispatcher, ops manager, store and audit log over writesBound,
// the provider with p.writeBindings() bound (the zfsReadsBound precedent).
// The helper runs in-process on F-ZFS through its helperEnv seam, except in
// TC-STG-ERR11 and ERR13, which re-execute this test binary as the helper
// (TestMain) to stop it with a real SIGTERM.

package storage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// ---- test support (WI 4.5) ----

// helperModeEnv re-executes this test binary as the storage helper on
// F-ZFS (TC-STG-ERR11, ERR13): its value is the fake's mode, and
// helperCallsEnv names the file every argv the fake receives is appended
// to, before the fake answers it.
const (
	helperModeEnv  = "NEXUS_STORAGE_TEST_HELPER"
	helperCallsEnv = "NEXUS_STORAGE_TEST_HELPER_CALLS"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		os.Exit(subprocessHelper(mode, os.Getenv(helperCallsEnv), os.Args[1:]))
	}
	os.Exit(m.Run())
}

// procTB is the testing.TB of F-ZFS in a re-executed helper, which has no
// test: a fake's complaint goes to stderr. The fake calls nothing else.
type procTB struct{ testing.TB }

func (procTB) Helper() {}
func (procTB) Errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "storagetest: "+format+"\n", a...)
}
func (procTB) Fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "storagetest: "+format+"\n", a...)
	os.Exit(3)
}
func (procTB) Skipf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "storagetest: "+format+"\n", a...)
	os.Exit(4)
}

// subprocessHelper is the re-executed helper: F-ZFS with CAP_SYS_ADMIN,
// each argv logged to calls first. Mode hang-preread hangs the pre-read
// zpool status of z1 until the helper's ctx ends; confirm-stuck answers
// every zpool status after the write with the status read before it, so
// the scrub never shows.
func subprocessHelper(mode, calls string, args []string) int {
	fake := storagetest.NewFake(procTB{}, "home-server", storagetest.Options{CapSysAdmin: true})
	status := fake.Argv(zfs.CmdPoolStatus, "z1")
	if mode == "hang-preread" {
		fake.Hang(status)
	}
	var mu sync.Mutex
	var before *zfs.Result
	wrote := false
	run := func(ctx context.Context, argv []string) (zfs.Result, error) {
		f, err := os.OpenFile(calls, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return zfs.Result{}, err
		}
		_, werr := fmt.Fprintln(f, strings.Join(argv, " "))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return zfs.Result{}, werr
		}
		isStatus := slices.Equal(argv, status)
		mu.Lock()
		if mode == "confirm-stuck" && isStatus && wrote && before != nil {
			res := *before
			mu.Unlock()
			return res, nil
		}
		mu.Unlock()
		res, err := fake.Run(ctx, argv)
		mu.Lock()
		defer mu.Unlock()
		if isStatus && before == nil && err == nil {
			before = &res
		}
		if slices.Equal(argv, fake.Argv(zfs.CmdScrubStart, "z1")) {
			wrote = true
		}
		return res, err
	}
	return helperCLI(args, os.Stdout, os.Stderr, helperEnv{Run: run, Stat: toolStat(fake.Paths())})
}

// toolStat is a stat that finds the capture's zpool and zfs as executable
// files, and nothing else, so zfs.Resolve resolves them on any host.
func toolStat(p zfs.Paths) func(string) (fs.FileInfo, error) {
	root := fstest.MapFS{
		strings.TrimPrefix(p.Zpool, "/"): {Mode: 0o755},
		strings.TrimPrefix(p.Zfs, "/"):   {Mode: 0o755},
	}
	return func(name string) (fs.FileInfo, error) { return fs.Stat(root, strings.TrimPrefix(name, "/")) }
}

// tw0 is F-CLOCK's T0 of the write test cases: snapshot names are
// nexus-20261119T051000Z.
var tw0 = time.Date(2026, 11, 19, 5, 10, 0, 0, time.UTC)

// testOpID is the operation ID of TC-STG-182 and TC-STG-ERR10.
const testOpID = "op-0123456789abcdef"

// writesBound is the provider with the given bindings (by default
// p.writeBindings()), for the Dispatcher before WI 5.1 composes them.
type writesBound struct {
	*Provider
	bindings []provider.Binding
}

func (b writesBound) Bindings() []provider.Binding { return b.bindings }

// wopts configure a wstack.
type wopts struct {
	cfg      func(*config.StorageConfig)          // The providers.storage section over the defaults
	noCap    bool                                 // F-ZFS without CAP_SYS_ADMIN
	setup    func(*storagetest.Fake, *overlay)    // Before the startup preflight
	bindings func(p *Provider) []provider.Binding // Nil: p.writeBindings()
	seed     func(*store.Store)                   // Records the store holds before the manager starts
}

// wstack is the provider on home-server F-ZFS behind a real Dispatcher,
// ops manager, store and audit log, at F-CLOCK tw0. Its launcher runs the
// helper in-process on F-ZFS (launch), and records every launch.
type wstack struct {
	t        *testing.T
	fake     *storagetest.Fake
	ov       *overlay
	p        *Provider
	clk      *clock
	opsDir   string
	auditDir string
	st       *store.Store
	mgr      *ops.Manager
	d        *core.Dispatcher
	bindings []provider.Binding

	mu       sync.Mutex
	launch   func(inv detach.Invocation) // What a launch does; default runs the helper
	launched []string                    // Operation IDs, in launch order
	helperFn zfs.Runner                  // The helper's runner; default the fake's
	wg       sync.WaitGroup
}

func newWStack(t *testing.T, o wopts) *wstack {
	t.Helper()
	clk := &clock{}
	clk.set(tw0)
	fake := storagetest.NewFake(t, "home-server", storagetest.Options{CapSysAdmin: !o.noCap, Now: clk.Now})
	ov := newOverlay(fake)
	if o.setup != nil {
		o.setup(fake, ov)
	}
	p, _ := newTestProvider(homeFS(t), ov.run, func(op *Options) {
		op.Now = clk.Now
		op.Executable = func() (string, error) { return "/usr/bin/nexusd", nil }
	})
	stateDir := t.TempDir()
	s := &wstack{t: t, fake: fake, ov: ov, p: p, clk: clk, opsDir: filepath.Join(stateDir, "ops"), auditDir: t.TempDir()}
	s.launch = s.runHelperUnit
	p.UseOpsDir(s.opsDir)
	s.bindings = p.writeBindings()
	if o.bindings != nil {
		s.bindings = o.bindings(p)
	}
	alog, err := audit.Open(audit.Options{Dir: s.auditDir, Host: "h1", Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	s.st, err = store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if o.seed != nil {
		o.seed(s.st)
	}
	launcher, err := detach.New(detach.Config{OpsDir: s.opsDir, Executable: "/usr/bin/nexusd", Logger: discard},
		detach.Options{Launch: s.launchUnit, UnitState: s.unitState,
			Signal:    func(context.Context, string, syscall.Signal) error { return errors.New("no signals in this test") },
			ListUnits: func(context.Context) ([]string, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	s.mgr, err = ops.New(ops.Options{Store: s.st, Audit: alog, Dir: s.opsDir, Host: "h1", Logger: discard,
		PollInterval: 20 * time.Millisecond, Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := core.New([]provider.Provider{writesBound{p, s.bindings}}, core.Options{
		Host: "h1", Version: "v0.0.0-test", Logger: discard, Audit: alog, Ops: s.mgr,
		Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: func(into any) error {
			c := into.(*config.StorageConfig)
			c.Enabled = true
			if o.cfg != nil {
				o.cfg(c)
			}
			return nil
		}}},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	if st := p.feats.get(featureZFS); !st.Available {
		t.Fatalf("zfs unavailable after the preflight: %s", st.Reason)
	}
	if err := s.mgr.Restore(s.binding); err != nil {
		t.Fatal(err)
	}
	s.d = core.NewDispatcher(reg, core.DispatcherOptions{Logger: discard, Audit: alog, NoteLimitPerMinute: 1 << 20})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.mgr.Shutdown(ctx); err != nil {
			t.Errorf("ops shutdown: %v", err)
		}
		s.wg.Wait()
		if err := s.st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
	})
	<-s.mgr.Recover()
	return s
}

// binding finds a bound op by ID, as the daemon's restart recovery does.
func (s *wstack) binding(id string) (provider.Binding, bool) {
	i := slices.IndexFunc(s.bindings, func(b provider.Binding) bool { return b.Op().ID == id })
	if i < 0 {
		return provider.Binding{}, false
	}
	return s.bindings[i], true
}

func (s *wstack) launchUnit(_ context.Context, inv detach.Invocation) error {
	s.mu.Lock()
	s.launched = append(s.launched, inv.ID)
	f := s.launch
	s.mu.Unlock()
	f(inv)
	return nil
}

// unitState is active until exit.json exists, then gone.
func (s *wstack) unitState(_ context.Context, unit string) (detach.UnitState, error) {
	id := strings.TrimSuffix(strings.TrimPrefix(unit, detach.UnitPrefix), ".service")
	if _, ok, _ := detach.ReadExit(filepath.Join(s.opsDir, id)); ok {
		return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
	}
	return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
}

// runHelperUnit runs the operation's helper in a goroutine with helperEnv
// on F-ZFS and writes exit.json as the supervisor does.
func (s *wstack) runHelperUnit(inv detach.Invocation) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		started := time.Now()
		var out, errb syncBuf
		code := runHelper(context.Background(), inv.Dir, &out, &errb, s.env())
		if code != 0 {
			s.t.Errorf("helper for %s exited %d: %s", inv.ID, code, errb.String())
		}
		if err := detach.WriteExit(inv.Dir, detach.Exit{ExitCode: code, Started: started, Ended: time.Now()}); err != nil {
			s.t.Errorf("exit.json for %s: %v", inv.ID, err)
		}
	}()
}

// blockUnits makes every later launch a unit that never ends.
func (s *wstack) blockUnits() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.launch = func(detach.Invocation) {}
}

func (s *wstack) launches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.launched)
}

// env is the helper's seams on F-ZFS: F-CLOCK, whose Sleep advances it,
// and the capture's tools.
func (s *wstack) env() helperEnv {
	s.mu.Lock()
	run := s.helperFn
	s.mu.Unlock()
	if run == nil {
		run = s.fake.Run
	}
	return helperEnv{Run: run, Now: s.clk.Now,
		Sleep: func(ctx context.Context, d time.Duration) {
			if ctx.Err() == nil {
				s.clk.set(s.clk.Now().Add(d))
			}
		},
		Stat: toolStat(s.fake.Paths())}
}

// setHelperRun replaces the helper's runner.
func (s *wstack) setHelperRun(run zfs.Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.helperFn = run
}

// configure applies mod to the defaults with storage enabled.
func (s *wstack) configure(mod func(*config.StorageConfig)) { configure(s.t, s.p, mod) }

// zfsDown makes the zfs feature unavailable, as checked now; zfsUp restores it.
func (s *wstack) zfsDown() {
	s.p.feats.set(featureZFS, featureState{Reason: "zfs: /dev/zfs not found", CheckedAt: s.clk.Now()})
}
func (s *wstack) zfsUp() {
	s.p.feats.set(featureZFS, featureState{Available: true, CheckedAt: s.clk.Now()})
}

var wPrincipal = auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "tc-stg-writes",
	Scopes: []string{spec.ReadScope, spec.ScrubScope, spec.SnapshotScope}}

func (s *wstack) invoke(op def.Op, input string) (core.InvokeResult, error) {
	return s.d.Invoke(context.Background(), core.InvokeRequest{Caller: wPrincipal, Surface: core.SurfaceREST, Op: op,
		Input: json.RawMessage(input)})
}

// start invokes op and requires a 202; it returns the operation's ID.
func (s *wstack) start(op def.Op, input string) string {
	s.t.Helper()
	res, err := s.invoke(op, input)
	if err != nil {
		s.t.Fatalf("%s %s: %v", op.ID, input, err)
	}
	if res.Status != 202 || res.OperationID == "" {
		s.t.Fatalf("%s %s: status %d, id %q", op.ID, input, res.Status, res.OperationID)
	}
	return res.OperationID
}

// refused invokes op and requires a refusal with slug, before any
// operation exists, audited refused with the slug, and nothing sent to ZFS.
func (s *wstack) refused(op def.Op, input string, slug problem.Slug) *problem.Error {
	s.t.Helper()
	opsBefore, refusedBefore := s.opCount(), s.auditCount(op.ID, audit.OutcomeRefused, slug)
	res, err := s.invoke(op, input)
	if err == nil {
		s.t.Fatalf("%s %s: status %d, want %s", op.ID, input, res.Status, slug)
	}
	pe := wantSlug(s.t, err, slug)
	if n := s.opCount(); n != opsBefore {
		s.t.Errorf("%s %s: %d operations, want %d: a refusal creates none", op.ID, input, n, opsBefore)
	}
	if n := s.auditCount(op.ID, audit.OutcomeRefused, slug); n != refusedBefore+1 {
		s.t.Errorf("%s %s: %d refused records with %s, want %d", op.ID, input, n, slug, refusedBefore+1)
	}
	if w := s.fake.Writes(); len(w) != 0 {
		s.t.Errorf("%s %s: writes %q, want none", op.ID, input, w)
	}
	return pe
}

func (s *wstack) opCount() int {
	s.t.Helper()
	page, err := s.st.List(store.ListQuery{})
	if err != nil {
		s.t.Fatal(err)
	}
	return len(page.Records)
}

// wait polls the store until operation id is terminal.
func (s *wstack) wait(id string) store.OpRecord {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rec, err := s.st.Get(id)
		if err != nil {
			s.t.Fatal(err)
		}
		if rec.Status.Terminal() {
			return rec
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("operation %s still %s", id, rec.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *wstack) audits() []systemspec.AuditRecord {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.auditDir, audit.FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
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
		recs = append(recs, r)
	}
	return recs
}

func (s *wstack) auditCount(action, outcome string, slug problem.Slug) int {
	n := 0
	for _, r := range s.audits() {
		if r.Action == action && r.Outcome == outcome && r.Problem == string(slug) {
			n++
		}
	}
	return n
}

// requested is the last requested record of action.
func (s *wstack) requested(action string) systemspec.AuditRecord {
	s.t.Helper()
	var last *systemspec.AuditRecord
	for _, r := range s.audits() {
		if r.Action == action && r.Outcome == audit.OutcomeRequested {
			last = &r
		}
	}
	if last == nil {
		s.t.Fatalf("no requested record of %s", action)
	}
	return *last
}

// spec funcs called directly, as the core calls them.

func runOf(id string) *provider.Run { return provider.NewRun(id, nil) }

// prepareStart admits and prepares a scrub start of pool as operation id.
func (s *wstack) prepareStart(id, pool string, force bool) (spec.ScrubInput, detach.Spec) {
	s.t.Helper()
	in := spec.ScrubInput{Name: pool, Force: force}
	sp := s.p.scrubStartSpec()
	if err := sp.Admit(context.Background(), in); err != nil {
		s.t.Fatalf("admit %s: %v", pool, err)
	}
	ds, err := sp.Prepare(context.Background(), in, runOf(id))
	if err != nil {
		s.t.Fatalf("prepare %s: %v", pool, err)
	}
	return in, ds
}

func (s *wstack) prepareStop(id, pool string) spec.StopScrubInput {
	s.t.Helper()
	in := spec.StopScrubInput{Name: pool}
	sp := s.p.stopScrubSpec()
	if err := sp.Admit(context.Background(), in); err != nil {
		s.t.Fatalf("admit stop %s: %v", pool, err)
	}
	if _, err := sp.Prepare(context.Background(), in, runOf(id)); err != nil {
		s.t.Fatalf("prepare stop %s: %v", pool, err)
	}
	return in
}

// prepareSnapshot admits and prepares a snapshot of in as operation id.
func (s *wstack) prepareSnapshot(id string, in spec.SnapshotInput) spec.SnapshotInput {
	s.t.Helper()
	sp := s.p.snapshotSpec()
	a, err := sp.Admission(context.Background(), in)
	if err != nil {
		s.t.Fatalf("admit snapshot %s: %v", in.Name, err)
	}
	if _, err := sp.Prepare(context.Background(), a.In, runOf(id)); err != nil {
		s.t.Fatalf("prepare snapshot %s: %v", in.Name, err)
	}
	return a.In
}

// helper runs operation id's helper in-process and returns its exit code
// and result.json.
func (s *wstack) helper(id string) (int, helperResult) {
	s.t.Helper()
	dir := filepath.Join(s.opsDir, id)
	var out, errb syncBuf
	code := runHelper(context.Background(), dir, &out, &errb, s.env())
	var res helperResult
	if ok, err := readJSON(dir, resultFile, &res); err != nil || !ok {
		s.t.Fatalf("result.json of %s: %v %v (stderr %s)", id, ok, err, errb.String())
	}
	return code, res
}

// scrubOK is the helper's ScrubResult, failing unless it succeeded.
func scrubOK(t *testing.T, code int, res helperResult) spec.ScrubResult {
	t.Helper()
	if code != 0 || res.Error != nil {
		t.Fatalf("helper exited %d with %+v", code, res.Error)
	}
	var r spec.ScrubResult
	if err := decodeStrict(res.Result, &r); err != nil {
		t.Fatalf("result %s: %v", res.Result, err)
	}
	return r
}

// okScrub runs operation id's helper and returns its ScrubResult,
// failing unless it succeeded.
func (s *wstack) okScrub(id string) spec.ScrubResult {
	s.t.Helper()
	code, res := s.helper(id)
	return scrubOK(s.t, code, res)
}

// helperProblem is the helper's failure, failing unless it is one with slug.
func helperProblem(t *testing.T, res helperResult, slug problem.Slug) *helperError {
	t.Helper()
	if res.Error == nil || res.Error.Slug != slug {
		t.Fatalf("result %s / %+v, want %s", res.Result, res.Error, slug)
	}
	return res.Error
}

func (s *wstack) finalizeStart(id string, in spec.ScrubInput, x detach.Exit) (spec.ScrubResult, error) {
	return s.p.scrubStartSpec().Finalize(context.Background(), in, x, runOf(id))
}

// statusWithHealth is `zpool status -j --json-int z1` of the synthetic
// vector file's pool renamed z1 and with its state word health.
func statusWithHealth(t *testing.T, file, health string) []byte {
	t.Helper()
	version, pools := jsonMembers(t, synthetic(t, file), "pools")
	if len(pools) != 1 {
		t.Fatalf("%s: %d pools", file, len(pools))
	}
	var raw json.RawMessage
	for _, v := range pools {
		raw = v
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rename(raw, "tank", "z1"), &obj); err != nil {
		t.Fatal(err)
	}
	obj["state"], _ = json.Marshal(health)
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return jsonDoc(version, "pools", "z1", json.RawMessage(b))
}

// answerHealth makes admission's zpool status of z1 show health.
func answerHealth(t *testing.T, f *storagetest.Fake, o *overlay, file, health string) {
	o.answer(f.Argv(zfs.CmdPoolStatus, "z1"), zfs.Result{Stdout: statusWithHealth(t, file, health)})
}

func scrubCfg(pools ...string) func(*config.StorageConfig) {
	return func(c *config.StorageConfig) { c.Scrub.Enabled, c.Scrub.Pools = true, pools }
}

func snapCfg(datasets ...config.StorageSnapshotDataset) func(*config.StorageConfig) {
	return func(c *config.StorageConfig) { c.Snapshots.Enabled, c.Snapshots.Datasets = true, datasets }
}

func cfgAll(fns ...func(*config.StorageConfig)) func(*config.StorageConfig) {
	return func(c *config.StorageConfig) {
		for _, f := range fns {
			f(c)
		}
	}
}

var shareDS = config.StorageSnapshotDataset{Name: "z1/ds/share"}

func extOf(pe *problem.Error, key string) string { return fmt.Sprint(pe.Extensions[key]) }

func wantExt(t *testing.T, pe *problem.Error, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got := extOf(pe, k); got != v {
			t.Errorf("%s: %s = %q, want %q (%v)", pe.Slug(), k, got, v, pe.Extensions)
		}
	}
}

// syncBuf is a goroutine-safe buffer.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func inputKeys(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, inputFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func inputKeyList(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	slices.Sort(k)
	return k
}

func sortedKeys(s ...string) []string { slices.Sort(s); return s }

// ---- TC-STG-175 to TC-STG-185 ----

// TC-STG-175: scrub admission refuses in order, before any operation exists, and sends nothing. Owner WI 4.5. Requirements: FR-13, FR-19, NFR-5.
func TestTCSTG175_ScrubAdmissionRefusesInOrderBeforeAny(t *testing.T) {
	s := newWStack(t, wopts{cfg: func(c *config.StorageConfig) { c.Scrub.Enabled = false }})
	op := spec.PoolsScrub.Op()

	pe := s.refused(op, `{"name":"z1"}`, problem.NotAllowed)
	wantExt(t, pe, map[string]string{"configKey": keyScrubEnabled})

	s.configure(scrubCfg("z2"))
	pe = s.refused(op, `{"name":"z1"}`, problem.NotAllowed)
	wantExt(t, pe, map[string]string{"configKey": keyScrubPools, "pool": "z1"})

	s.configure(scrubCfg("z1"))
	s.zfsDown()
	if pe := s.refused(op, `{"name":"z1"}`, problem.CapabilityUnavailable); pe.Status != 503 {
		t.Errorf("zfs unavailable: status %d, want 503", pe.Status)
	}
	s.zfsUp()

	s.configure(scrubCfg("nexus-absent"))
	if pe := s.refused(op, `{"name":"nexus-absent"}`, problem.NotFound); pe.Status != 404 {
		t.Errorf("absent pool: status %d", pe.Status)
	}

	s.configure(scrubCfg("z1"))
	s.fake.SeedScan("z1", storagetest.VectorScrubScanning)
	pe = s.refused(op, `{"name":"z1"}`, problem.ScanInProgress)
	wantExt(t, pe, map[string]string{"pool": "z1", "function": "scrub", "state": "scanning"})

	s.fake.SeedScan("z1", storagetest.VectorResilverScanning)
	pe = s.refused(op, `{"name":"z1"}`, problem.ScanInProgress)
	wantExt(t, pe, map[string]string{"pool": "z1", "function": "resilver", "state": "scanning"})

	answerHealth(t, s.fake, s.ov, "zpool-status-degraded-mirror.json", "DEGRADED")
	pe = s.refused(op, `{"name":"z1"}`, problem.PoolUnhealthy)
	wantExt(t, pe, map[string]string{"pool": "z1", "health": "degraded"})
	if n := len(s.launches()); n != 0 {
		t.Errorf("%d launches, want none", n)
	}
}

// TC-STG-176: force admits a degraded pool only and is audited; a paused scrub is admitted. Owner WI 4.5. Requirements: FR-13, FR-19.
func TestTCSTG176_ForceAdmitsADegradedPoolOnlyAnd(t *testing.T) {
	op := spec.PoolsScrub.Op()
	t.Run("degraded with force", func(t *testing.T) {
		s := newWStack(t, wopts{cfg: scrubCfg("z1"), setup: func(f *storagetest.Fake, o *overlay) {
			answerHealth(t, f, o, "zpool-status-degraded-mirror.json", "DEGRADED")
		}})
		s.blockUnits()
		s.start(op, `{"name":"z1","force":true}`)
		if f := s.requested(op.ID).Facts; f["pool"] != "z1" || f["force"] != "true" {
			t.Errorf("requested facts %v, want pool z1, force true", f)
		}
	})
	t.Run("paused scrub", func(t *testing.T) {
		s := newWStack(t, wopts{cfg: scrubCfg("z1")})
		s.blockUnits()
		s.fake.SeedScan("z1", storagetest.VectorScrubPaused)
		s.start(op, `{"name":"z1"}`)
		if f := s.requested(op.ID).Facts; f["pool"] != "z1" || f["force"] != "false" {
			t.Errorf("requested facts %v, want pool z1, force false", f)
		}
	})
	t.Run("other health with force", func(t *testing.T) {
		s := newWStack(t, wopts{cfg: scrubCfg("z1")})
		for _, c := range []struct{ file, word, want string }{
			{"zpool-status-suspended.json", "SUSPENDED", "suspended"},
			{"zpool-status-degraded-mirror.json", "FAULTED", "faulted"},
			{"zpool-status-degraded-mirror.json", "UNAVAIL", "unavail"},
			{"zpool-status-degraded-mirror.json", "REMOVED", "removed"},
			{"zpool-status-degraded-mirror.json", "OFFLINE", "offline"},
			{"zpool-status-degraded-mirror.json", "NEXUS_SYNTHETIC_STATE", "unknown"},
		} {
			answerHealth(t, s.fake, s.ov, c.file, c.word)
			pe := s.refused(op, `{"name":"z1","force":true}`, problem.PoolUnhealthy)
			wantExt(t, pe, map[string]string{"pool": "z1", "health": c.want})
		}
	})
}

// TC-STG-177: stop-scrub admission refuses only on configuration, zfs and existence. Owner WI 4.5. Requirements: FR-13.
func TestTCSTG177_StopScrubAdmissionRefusesOnlyOnConfiguration(t *testing.T) {
	s := newWStack(t, wopts{cfg: func(c *config.StorageConfig) { c.Scrub.Enabled = false },
		setup: func(f *storagetest.Fake, o *overlay) {
			answerHealth(t, f, o, "zpool-status-degraded-mirror.json", "DEGRADED")
		}})
	s.blockUnits()
	op := spec.PoolsStopScrub.Op()
	wantExt(t, s.refused(op, `{"name":"z1"}`, problem.NotAllowed), map[string]string{"configKey": keyScrubEnabled})
	s.configure(scrubCfg("z2"))
	wantExt(t, s.refused(op, `{"name":"z1"}`, problem.NotAllowed),
		map[string]string{"configKey": keyScrubPools, "pool": "z1"})
	s.configure(scrubCfg("z1"))
	// z1 is degraded (the overlay) and idle: a stop is admitted.
	s.start(op, `{"name":"z1"}`)
	if f := s.requested(op.ID).Facts; f["pool"] != "z1" {
		t.Errorf("requested facts %v", f)
	}
}

// TC-STG-178: scrub and stop share a per-pool lane that rejects a second operation. Owner WI 4.5. Requirements: FR-13.
func TestTCSTG178_ScrubAndStopShareAPerPool(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1", "z2")})
	s.blockUnits()
	for _, op := range []def.Op{spec.PoolsScrub.Op(), spec.PoolsStopScrub.Op(), spec.DatasetsSnapshot.Op()} {
		if op.Cancel != def.CancelQueuedOnly {
			t.Errorf("%s: Cancel %q, want %q", op.ID, op.Cancel, def.CancelQueuedOnly)
		}
	}
	for _, b := range s.bindings[:2] {
		f, _ := b.Detached()
		var in any = spec.ScrubInput{Name: "z1"}
		if b.Op().ID == spec.PoolsStopScrub.Op().ID {
			in = spec.StopScrubInput{Name: "z1"}
		}
		l, err := f.Lane(context.Background(), in)
		if err != nil || l.Key != "storage:scrub:z1" || l.Policy != provider.LaneReject {
			t.Errorf("%s lane %+v, %v", b.Op().ID, l, err)
		}
	}
	first := s.start(spec.PoolsScrub.Op(), `{"name":"z1"}`)
	for _, op := range []def.Op{spec.PoolsScrub.Op(), spec.PoolsStopScrub.Op()} {
		_, err := s.invoke(op, `{"name":"z1"}`)
		pe := wantSlug(t, err, problem.Conflict)
		if pe.Status != 409 || extOf(pe, "blockingOperationId") != first {
			t.Errorf("%s: %d %v, want 409 blocked by %s", op.ID, pe.Status, pe.Extensions, first)
		}
	}
	s.start(spec.PoolsScrub.Op(), `{"name":"z2"}`)
}

// TC-STG-179: Prepare re-checks the allow-list and writes the helper's input. Owner WI 4.5. Requirements: FR-13, FR-14, CON-1.
func TestTCSTG179_PrepareReChecksTheAllowListAnd(t *testing.T) {
	s := newWStack(t, wopts{cfg: cfgAll(scrubCfg("z1"), snapCfg(shareDS))})

	// (1) The allow-list changes between admission and Prepare.
	in := spec.ScrubInput{Name: "z1"}
	sp := s.p.scrubStartSpec()
	if err := sp.Admit(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	s.configure(cfgAll(scrubCfg(), snapCfg(shareDS)))
	_, err := sp.Prepare(context.Background(), in, runOf("op-0000000000000001"))
	wantExt(t, wantSlug(t, err, problem.NotAllowed), map[string]string{"configKey": keyScrubPools})
	if _, err := os.Stat(filepath.Join(s.opsDir, "op-0000000000000001", inputFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("input.json after a refused Prepare: %v", err)
	}

	// (2) A scrub start's input and spec.
	s.configure(cfgAll(scrubCfg("z1"), snapCfg(shareDS)))
	const id = "op-0000000000000002"
	_, ds := s.prepareStart(id, "z1", false)
	dir := filepath.Join(s.opsDir, id)
	m := inputKeys(t, dir)
	if got, want := inputKeyList(m), sortedKeys("operationId", "action", "pool", "force", "format", "commandTimeout", "commandedAt"); !slices.Equal(got, want) {
		t.Errorf("scrub input keys %v, want %v", got, want)
	}
	for k, v := range map[string]any{"operationId": id, "action": "scrub-start", "pool": "z1", "force": false,
		"format": "json", "commandTimeout": "30s", "commandedAt": tw0.Format(time.RFC3339)} {
		if m[k] != v {
			t.Errorf("input %s = %#v, want %#v", k, m[k], v)
		}
	}
	timeout := 2*30*time.Second + 60*time.Second + 30*time.Second
	if !slices.Equal(ds.Argv, []string{"/usr/bin/nexusd", "storage-helper", "run", "--dir", dir}) ||
		ds.KillMode != detach.KillProcess || ds.Timeout != timeout || ds.MaxRuntime != timeout+30*time.Second ||
		ds.Grace != 10*time.Second {
		t.Errorf("spec %+v", ds)
	}
	if err := ds.Validate(); err != nil {
		t.Errorf("spec: %v", err)
	}

	// (3) A snapshot's input.
	const sid = "op-0000000000000003"
	s.prepareSnapshot(sid, spec.SnapshotInput{Name: "z1/ds/share"})
	m = inputKeys(t, filepath.Join(s.opsDir, sid))
	if got, want := inputKeyList(m), sortedKeys("operationId", "action", "dataset", "snapshot", "recursive", "format", "commandTimeout"); !slices.Equal(got, want) {
		t.Errorf("snapshot input keys %v, want %v", got, want)
	}
	for k, v := range map[string]any{"operationId": sid, "action": "snapshot", "dataset": "z1/ds/share",
		"snapshot": "nexus-20261119T051000Z", "recursive": false, "format": "json", "commandTimeout": "30s"} {
		if m[k] != v {
			t.Errorf("snapshot input %s = %#v, want %#v", k, m[k], v)
		}
	}

	// Each input, as written, passes the helper's checks.
	for _, id := range []string{id, sid} {
		dir := filepath.Join(s.opsDir, id)
		raw, err := os.ReadFile(filepath.Join(dir, inputFile))
		if err != nil {
			t.Fatal(err)
		}
		if _, herr := checkInput(dir, raw); herr != nil {
			t.Errorf("%s: %s", id, herr.Detail)
		}
		if code, res := s.helper(id); code != 0 || res.Error != nil {
			t.Errorf("%s: helper exited %d with %+v", id, code, res.Error)
		}
	}
}

// TC-STG-180: the helper starts a scrub and confirms it from zpool status. Owner WI 4.5. Requirements: FR-13, FR-6.
func TestTCSTG180_TheHelperStartsAScrubAndConfirms(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1")})
	in, _ := s.prepareStart(testOpID, "z1", false)
	code, res := s.helper(testOpID)
	if w := s.fake.Writes(); !slices.EqualFunc(w, [][]string{{"/usr/sbin/zpool", "scrub", "z1"}}, slices.Equal) {
		t.Errorf("writes %q", w)
	}
	r := scrubOK(t, code, res)
	out, err := s.finalizeStart(testOpID, in, detach.Exit{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	for _, x := range []spec.ScrubResult{r, out} {
		if !x.Changed || x.Pool != "z1" || x.Scan.Function != "scrub" || x.Scan.State != "scanning" || x.Resumed {
			t.Errorf("result %+v", x)
		}
	}
}

// TC-STG-181: stop-scrub sends nothing when nothing runs, and cancels a running scrub. Owner WI 4.5. Requirements: FR-13.
func TestTCSTG181_StopScrubSendsNothingWhenNothingRuns(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1")})

	s.prepareStop("op-0000000000000011", "z1")
	r := s.okScrub("op-0000000000000011")
	if r.Changed || len(r.Warnings) != 0 || len(s.fake.Writes()) != 0 {
		t.Errorf("idle: %+v, writes %q", r, s.fake.Writes())
	}

	s.fake.SeedScan("z1", storagetest.VectorResilverScanning)
	s.prepareStop("op-0000000000000012", "z1")
	r = s.okScrub("op-0000000000000012")
	if r.Changed || !slices.Equal(r.Warnings, []string{"a resilver is running; stop-scrub does not cancel a resilver"}) ||
		len(s.fake.Writes()) != 0 {
		t.Errorf("resilver: %+v, writes %q", r, s.fake.Writes())
	}

	s.fake.SeedScan("z1", storagetest.VectorScrubScanning)
	s.prepareStop("op-0000000000000013", "z1")
	r = s.okScrub("op-0000000000000013")
	if w := s.fake.Writes(); !slices.EqualFunc(w, [][]string{{"/usr/sbin/zpool", "scrub", "-s", "z1"}}, slices.Equal) {
		t.Errorf("writes %q", w)
	}
	if !r.Changed || r.Scan.State != "canceled" {
		t.Errorf("scanning: %+v", r)
	}
}

// TC-STG-182: the snapshot helper runs the exact argv and returns the created item. Owner WI 4.5. Requirements: FR-14, FR-8.
func TestTCSTG182_TheSnapshotHelperRunsTheExactArgv(t *testing.T) {
	s := newWStack(t, wopts{cfg: snapCfg(shareDS, config.StorageSnapshotDataset{Name: "z1/ds", Recursive: true})})
	sp := s.p.snapshotSpec()
	run := func(id string, in spec.SnapshotInput) spec.SnapshotResult {
		t.Helper()
		if _, err := sp.Prepare(context.Background(), in, runOf(id)); err != nil {
			t.Fatal(err)
		}
		code, res := s.helper(id)
		if code != 0 || res.Error != nil {
			t.Fatalf("helper exited %d with %+v", code, res.Error)
		}
		out, err := sp.Finalize(context.Background(), in, detach.Exit{}, runOf(id))
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		return out
	}
	const name = "nexus-20261119T051000Z-pre"
	out := run(testOpID, spec.SnapshotInput{Name: "z1/ds/share", Label: "pre", Snapshot: name})
	want := [][]string{{"/usr/sbin/zfs", "snapshot", "-o", "nexus:created-by=" + testOpID, "z1/ds/share@" + name}}
	if w := s.fake.Writes(); !slices.EqualFunc(w, want, slices.Equal) {
		t.Errorf("writes %q, want %q", w, want)
	}
	sn := out.Snapshot
	if !out.Changed || sn.Name != "z1/ds/share@"+name || !sn.Properties.CreatedAt.Equal(tw0) ||
		!sn.Properties.CreatedByNexus || sn.Properties.Dataset != "z1/ds/share" || sn.Type != spec.SnapshotsType ||
		sn.Host != "test" || sn.ID == "" {
		t.Errorf("result %+v", out)
	}

	// A recursive snapshot of z1/ds would collide with z1/ds/share's above
	// under the same name, so it takes another label.
	const rid, rname = "op-0123456789abcde0", "nexus-20261119T051000Z-rec"
	out = run(rid, spec.SnapshotInput{Name: "z1/ds", Label: "rec", Recursive: true, Snapshot: rname})
	want = append(want, []string{"/usr/sbin/zfs", "snapshot", "-o", "nexus:created-by=" + rid, "-r", "z1/ds@" + rname})
	if out.Snapshot.Name != "z1/ds@"+rname || !out.Snapshot.Properties.CreatedByNexus {
		t.Errorf("recursive result %+v", out)
	}
	if w := s.fake.Writes(); !slices.EqualFunc(w, want, slices.Equal) {
		t.Errorf("writes %q, want %q", w, want)
	}
}

// TC-STG-183: snapshot admission names the snapshot in UTC and records the facts. Owner WI 4.5. Requirements: FR-14, FR-19.
func TestTCSTG183_SnapshotAdmissionNamesTheSnapshotInUTC(t *testing.T) {
	t.Setenv("TZ", "America/Los_Angeles")
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	s := newWStack(t, wopts{cfg: snapCfg(shareDS)})
	s.clk.set(tw0.In(la))
	op := spec.DatasetsSnapshot.Op()
	for _, c := range []struct{ input, name string }{
		{`{"name":"z1/ds/share","label":"pre"}`, "nexus-20261119T051000Z-pre"},
		{`{"name":"z1/ds/share"}`, "nexus-20261119T051000Z"},
	} {
		id := s.start(op, c.input)
		f := s.requested(op.ID).Facts
		if f["dataset"] != "z1/ds/share" || f["snapshot"] != c.name || f["recursive"] != "false" {
			t.Errorf("%s: requested facts %v, want snapshot %s", c.input, f, c.name)
		}
		if rec := s.wait(id); rec.Status != apitypes.StatusSucceeded {
			t.Errorf("%s: %s %v", c.input, rec.Status, rec.Error)
		}
	}
}

// TC-STG-185: a snapshot operation whose admission-time name was lost by a restart fails in Prepare with nothing sent. Owner WI 4.5. Requirements: FR-14, FR-19.
func TestTCSTG185_ASnapshotOperationWhoseAdmissionTimeName(t *testing.T) {
	op := spec.DatasetsSnapshot.Op()
	var queued string
	s := newWStack(t, wopts{cfg: snapCfg(shareDS), seed: func(st *store.Store) {
		// A queued snapshot as recovery finds it after a restart.
		res, err := st.CreateWithIdempotency(store.OpRecord{Action: op.ID, Provider: spec.Name, Target: "z1/ds/share",
			TokenName: wPrincipal.Name, Input: json.RawMessage(`{"name":"z1/ds/share"}`), Exec: store.ExecDetached,
			StartScope: spec.SnapshotScope,
			Lane:       store.Lane{Key: snapshotLanePrefix + "z1/ds/share", Policy: store.LaneReject, Limit: 1}},
			store.IdempotencyKey{})
		if err != nil {
			t.Fatal(err)
		}
		queued = res.ID
	}})

	// Directly: the admitted input as recovery decodes it.
	sp := s.p.snapshotSpec()
	a, err := sp.Admission(context.Background(), spec.SnapshotInput{Name: "z1/ds/share"})
	if err != nil || a.In.Snapshot == "" {
		t.Fatalf("admission: %+v, %v", a, err)
	}
	b, err := json.Marshal(a.In)
	if err != nil {
		t.Fatal(err)
	}
	var lost spec.SnapshotInput
	if err := json.Unmarshal(b, &lost); err != nil || lost.Snapshot != "" {
		t.Fatalf("round trip kept %q (%v)", lost.Snapshot, err)
	}
	_, err = sp.Prepare(context.Background(), lost, runOf("op-0000000000000021"))
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.InternalError || pe.Detail != lostSnapshotName {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.opsDir, "op-0000000000000021", inputFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("input.json: %v", err)
	}

	// Through the ops manager's recovery of the queued record.
	rec := s.wait(queued)
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.InternalError ||
		rec.Error.Detail != lostSnapshotName {
		t.Fatalf("recovered operation: %s %+v", rec.Status, rec.Error)
	}
	if _, err := os.Stat(filepath.Join(s.opsDir, queued, inputFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("input.json of the recovered operation: %v", err)
	}
	if l := s.launches(); len(l) != 0 {
		t.Errorf("launches %v, want none", l)
	}
	if w := s.fake.Writes(); len(w) != 0 {
		t.Errorf("writes %q", w)
	}

	// A new request is named afresh and succeeds.
	id := s.start(op, `{"name":"z1/ds/share"}`)
	done := s.wait(id)
	if done.Status != apitypes.StatusSucceeded {
		t.Fatalf("new snapshot: %s %+v", done.Status, done.Error)
	}
	var r spec.SnapshotResult
	if err := json.Unmarshal(done.Result, &r); err != nil || r.Snapshot.Name != "z1/ds/share@nexus-20261119T051000Z" {
		t.Errorf("result %s (%v)", done.Result, err)
	}
	if f := s.requested(op.ID).Facts; f["snapshot"] != "nexus-20261119T051000Z" {
		t.Errorf("facts %v", f)
	}
}

// ---- TC-STG-E2, E6 to E9 ----

// TC-STG-E2: a snapshot name that would exceed 255 bytes is refused at admission. Owner WI 4.5. Requirements: FR-14.
func TestTCSTGE2_ASnapshotNameThatWouldExceed255(t *testing.T) {
	long := "z1/" + strings.Repeat("a", 227)   // 230 bytes
	longer := "z1/" + strings.Repeat("b", 230) // 233 bytes: with no label, 256
	s := newWStack(t, wopts{cfg: snapCfg(config.StorageSnapshotDataset{Name: long}, config.StorageSnapshotDataset{Name: longer})})
	op := spec.DatasetsSnapshot.Op()
	for _, c := range []struct{ input, field string }{
		{fmt.Sprintf(`{"name":%q,"label":%q}`, long, strings.Repeat("x", 32)), "/label"},
		{fmt.Sprintf(`{"name":%q}`, longer), "/name"},
	} {
		pe := s.refused(op, c.input, problem.ValidationFailed)
		errs, _ := pe.Extensions["errors"].([]problem.FieldError)
		if pe.Status != 400 || len(errs) != 1 || errs[0].Path != c.field || !strings.Contains(errs[0].Message, "255-byte") {
			t.Errorf("%s: %d %v, want 400 naming %s and the 255-byte limit", c.input, pe.Status, pe.Extensions, c.field)
		}
	}
}

// TC-STG-E6: a paused scrub is resumed by start and reported as resumed. Owner WI 4.5. Requirements: FR-13.
func TestTCSTGE6_APausedScrubIsResumedByStart(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1")})
	s.fake.SeedScan("z1", storagetest.VectorScrubPaused)
	s.prepareStart(testOpID, "z1", false)
	r := s.okScrub(testOpID)
	if w := s.fake.Writes(); !slices.EqualFunc(w, [][]string{{"/usr/sbin/zpool", "scrub", "z1"}}, slices.Equal) {
		t.Errorf("writes %q", w)
	}
	if !r.Changed || r.Scan.State != "scanning" || !r.Resumed {
		t.Errorf("result %+v", r)
	}
}

// TC-STG-E7: a tiny pool whose scrub finishes at once is a success; a stale finished scan is not. Owner WI 4.5. Requirements: FR-13.
func TestTCSTGE7_ATinyPoolWhoseScrubFinishesAt(t *testing.T) {
	t.Run("finished at once", func(t *testing.T) {
		s := newWStack(t, wopts{cfg: scrubCfg("z1")})
		s.fake.FinishImmediately("z1")
		s.prepareStart(testOpID, "z1", false)
		r := s.okScrub(testOpID)
		if r.Scan.State != "finished" || r.Scan.StartedAt == nil || r.Scan.StartedAt.Before(tw0.Add(-5*time.Second)) {
			t.Errorf("result %+v", r)
		}
	})
	t.Run("stale finished", func(t *testing.T) {
		s := newWStack(t, wopts{cfg: scrubCfg("z1")})
		s.fake.StaleFinished("z1")
		start := s.fake.Argv(zfs.CmdScrubStart, "z1")
		s.setHelperRun(func(ctx context.Context, argv []string) (zfs.Result, error) {
			res, err := s.fake.Run(ctx, argv)
			if slices.Equal(argv, start) {
				// Accepted, and zpool status never changes.
				s.fake.StaleFinished("z1")
			}
			return res, err
		})
		s.prepareStart(testOpID, "z1", false)
		_, res := s.helper(testOpID)
		he := helperProblem(t, res, problem.OutcomeUnknown)
		if he.Detail != "zpool scrub exited 0 but zpool status did not show the scrub within 60 s" {
			t.Errorf("detail %q", he.Detail)
		}
		if waited := s.clk.Now().Sub(tw0); waited < 60*time.Second {
			t.Errorf("the fake clock moved %s, want at least 60 s", waited)
		}
	})
}

// TC-STG-E8: stop-scrub racing with completion succeeds with a warning. Owner WI 4.5. Requirements: FR-13.
func TestTCSTGE8_StopScrubRacingWithCompletionSucceedsWith(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1")})
	s.fake.SeedScan("z1", storagetest.VectorScrubScanning)
	stop := s.fake.Argv(zfs.CmdScrubStop, "z1")
	s.setHelperRun(func(ctx context.Context, argv []string) (zfs.Result, error) {
		if slices.Equal(argv, stop) {
			// The scrub finishes between the read and the stop.
			s.fake.SeedScan("z1", storagetest.VectorScrubFinished)
		}
		return s.fake.Run(ctx, argv)
	})
	s.prepareStop(testOpID, "z1")
	r := s.okScrub(testOpID)
	if r.Changed || r.Scan.State != "finished" ||
		!slices.Equal(r.Warnings, []string{"the scrub finished before it could be stopped"}) {
		t.Errorf("result %+v", r)
	}
	if w := s.fake.Writes(); !slices.EqualFunc(w, [][]string{stop}, slices.Equal) {
		t.Errorf("writes %q", w)
	}
}

// TC-STG-E9: cron starts a scrub between admission and the helper. Owner WI 4.5. Requirements: FR-13.
func TestTCSTGE9_CronStartsAScrubBetweenAdmissionAnd(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1")})
	s.prepareStart(testOpID, "z1", false) // z1 idle at admission
	s.fake.SeedScan("z1", storagetest.VectorScrubScanning)
	r := s.okScrub(testOpID)
	if r.Changed || !slices.Equal(r.Warnings, []string{"a scrub was already running"}) || r.Scan.State != "scanning" {
		t.Errorf("result %+v", r)
	}
	if w := s.fake.Writes(); len(w) != 0 {
		t.Errorf("writes %q, want none", w)
	}
}

// ---- TC-STG-ERR4 to ERR11, ERR13, ERR14 ----

// TC-STG-ERR4: a scrub command that fails reports zfs-command-failed with at most 2 KiB of output. Owner WI 4.5. Requirements: FR-13, NFR-3.
func TestTCSTGERR4_AScrubCommandThatFailsReportsZfs(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1")})
	start := s.fake.Argv(zfs.CmdScrubStart, "z1")
	// 5 KiB of stderr whose 2 KiB boundary falls inside a 3-byte rune.
	stderr := "cannot scrub z1: I/O error\n" + strings.Repeat("é", 1008) + strings.Repeat("€", 1100)
	s.setHelperRun(func(ctx context.Context, argv []string) (zfs.Result, error) {
		res, err := s.fake.Run(ctx, argv)
		if slices.Equal(argv, start) {
			return zfs.Result{ExitCode: 1, Stderr: []byte(stderr)}, nil
		}
		return res, err
	})
	in, _ := s.prepareStart(testOpID, "z1", false)
	s.helper(testOpID)
	_, err := s.finalizeStart(testOpID, in, detach.Exit{})
	pe := wantSlug(t, err, problem.ZFSCommandFailed)
	out, _ := pe.Extensions["output"].(string)
	if extOf(pe, "command") != "zpool scrub" || extOf(pe, "exitCode") != "1" || len(out) > 2048 || len(out) < 2040 ||
		!utf8.ValidString(out) || !strings.HasPrefix(out, "cannot scrub z1: I/O error") {
		t.Errorf("problem %v (output %d bytes)", pe.Extensions, len(out))
	}
	if len(stderr) < 5<<10 {
		t.Fatalf("stderr is %d bytes", len(stderr))
	}
}

// TC-STG-ERR5: the helper denied inside a sandbox reports the denial, not success. Owner WI 4.5. Requirements: CON-1, FR-13.
func TestTCSTGERR5_TheHelperDeniedInsideASandboxReports(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1"), noCap: true})
	in, _ := s.prepareStart(testOpID, "z1", false)
	_, res := s.helper(testOpID)
	if res.Result != nil {
		t.Fatalf("a success: %s", res.Result)
	}
	_, err := s.finalizeStart(testOpID, in, detach.Exit{})
	pe := wantSlug(t, err, problem.ZFSCommandFailed)
	if !strings.Contains(extOf(pe, "output"), "permission denied") {
		t.Errorf("output %q", extOf(pe, "output"))
	}
}

// TC-STG-ERR6: a snapshot of an allow-listed dataset is refused when its pool is too full. Owner WI 4.5. Requirements: FR-14.
func TestTCSTGERR6_ASnapshotOfAnAllowListedDataset(t *testing.T) {
	var capacity func(int)
	s := newWStack(t, wopts{cfg: cfgAll(snapCfg(shareDS), func(c *config.StorageConfig) {
		c.Snapshots.MaxPoolCapacityPercent = 90
	}), setup: func(f *storagetest.Fake, o *overlay) {
		version, pools := jsonMembers(t, home(t, "zfs/zpool-list.json"), "pools")
		capacity = func(c int) {
			o.answer(f.Argv(zfs.CmdPoolList, "z1"),
				zfs.Result{Stdout: jsonDoc(version, "pools", "z1", setProp(t, pools["z1"], "capacity", c))})
		}
	}})
	s.blockUnits()
	op := spec.DatasetsSnapshot.Op()
	capacity(89)
	s.start(op, `{"name":"z1/ds/share"}`)
	capacity(90)
	pe := s.refused(op, `{"name":"z1/ds/share"}`, problem.PoolLowSpace)
	wantExt(t, pe, map[string]string{"pool": "z1", "capacityPercent": "90", "maxCapacityPercent": "90"})
}

// TC-STG-ERR7: snapshot admission refusals in order. Owner WI 4.5. Requirements: FR-14, FR-19.
func TestTCSTGERR7_SnapshotAdmissionRefusalsInOrder(t *testing.T) {
	s := newWStack(t, wopts{})
	op := spec.DatasetsSnapshot.Op()
	wantExt(t, s.refused(op, `{"name":"z1/ds/share"}`, problem.NotAllowed),
		map[string]string{"configKey": keySnapshotsEnabled})

	appdata := config.StorageSnapshotDataset{Name: "z1/ds/appdata"}
	s.configure(snapCfg(appdata))
	wantExt(t, s.refused(op, `{"name":"z1/ds/share"}`, problem.NotAllowed),
		map[string]string{"configKey": keySnapshotsDatasets, "dataset": "z1/ds/share"})
	wantExt(t, s.refused(op, `{"name":"z1/ds/appdata","recursive":true}`, problem.NotAllowed),
		map[string]string{"configKey": "providers.storage.snapshots.datasets[0].recursive", "dataset": "z1/ds/appdata"})

	s.zfsDown()
	s.refused(op, `{"name":"z1/ds/appdata"}`, problem.CapabilityUnavailable)
	s.zfsUp()

	s.configure(snapCfg(appdata, config.StorageSnapshotDataset{Name: "z1/ds/missing"}))
	if pe := s.refused(op, `{"name":"z1/ds/missing"}`, problem.NotFound); pe.Status != 404 {
		t.Errorf("status %d", pe.Status)
	}
}

// TC-STG-ERR8: a snapshot name collision fails the second operation. Owner WI 4.5. Requirements: FR-14.
func TestTCSTGERR8_ASnapshotNameCollisionFailsTheSecond(t *testing.T) {
	s := newWStack(t, wopts{cfg: snapCfg(shareDS)})
	op := spec.DatasetsSnapshot.Op()
	first := s.wait(s.start(op, `{"name":"z1/ds/share","label":"pre"}`))
	if first.Status != apitypes.StatusSucceeded {
		t.Fatalf("first: %s %+v", first.Status, first.Error)
	}
	second := s.wait(s.start(op, `{"name":"z1/ds/share","label":"pre"}`))
	if second.Status != apitypes.StatusFailed || second.Error == nil || second.Error.Slug() != problem.ZFSCommandFailed ||
		!strings.Contains(extOf(second.Error, "output"), "dataset already exists") {
		t.Fatalf("second: %s %+v", second.Status, second.Error)
	}
}

// TC-STG-ERR9: a helper that stops without a result ends outcome-unknown with a LostDetail naming what to check. Owner WI 4.5. Requirements: FR-13, FR-14.
func TestTCSTGERR9_AHelperThatStopsWithoutAResult(t *testing.T) {
	s := newWStack(t, wopts{cfg: cfgAll(scrubCfg("z1"), snapCfg(shareDS))})

	in, _ := s.prepareStart("op-0000000000000031", "z1", false)
	const scrubLost = "the storage helper for scrub-start on pool z1 stopped without a result; check `zpool status z1`"
	_, err := s.finalizeStart("op-0000000000000031", in, detach.Exit{ExitCode: -1, Signal: "SIGKILL", TimedOut: true})
	if pe := wantSlug(t, err, problem.OutcomeUnknown); pe.Detail != scrubLost {
		t.Errorf("detail %q", pe.Detail)
	}
	if d := s.p.scrubStartSpec().LostDetail(runOf("op-0000000000000031"), in); d != scrubLost {
		t.Errorf("LostDetail %q", d)
	}

	sin := s.prepareSnapshot("op-0000000000000032", spec.SnapshotInput{Name: "z1/ds/share"})
	sp := s.p.snapshotSpec()
	const snapLost = "the storage helper for snapshot on dataset z1/ds/share stopped without a result; " +
		"check `zfs list -t snapshot z1/ds/share`"
	_, err = sp.Finalize(context.Background(), sin, detach.Exit{ExitCode: 1}, runOf("op-0000000000000032"))
	if pe := wantSlug(t, err, problem.OutcomeUnknown); pe.Detail != snapLost {
		t.Errorf("detail %q", pe.Detail)
	}
	if d := sp.LostDetail(runOf("op-0000000000000032"), sin); d != snapLost {
		t.Errorf("LostDetail %q", d)
	}

	in, _ = s.prepareStart("op-0000000000000033", "z1", false)
	_, err = s.finalizeStart("op-0000000000000033", in,
		detach.Exit{ExitCode: -1, Error: "fork/exec /usr/bin/nexusd: no such file or directory"})
	if pe := wantSlug(t, err, problem.InternalError); !strings.Contains(pe.Detail, "nothing was sent") {
		t.Errorf("detail %q", pe.Detail)
	}
	if w := s.fake.Writes(); len(w) != 0 {
		t.Errorf("writes %q", w)
	}
}

// helperRun runs the in-process helper on a fresh F-ZFS over dir, as
// `Helper([run --dir dir])` with the env seam, and returns its exit code,
// result.json, every argv it ran (answered by answer, when set, or by the
// fake) and the writes the fake recorded.
func helperRun(t *testing.T, dir string, answer func(argv []string) (zfs.Result, bool)) (int, helperResult, [][]string, [][]string) {
	t.Helper()
	fake := storagetest.NewFake(t, "home-server", storagetest.Options{CapSysAdmin: true, Now: func() time.Time { return tw0 }})
	var seen [][]string
	run := func(ctx context.Context, argv []string) (zfs.Result, error) {
		seen = append(seen, slices.Clone(argv))
		if answer != nil {
			if res, ok := answer(argv); ok {
				return res, nil
			}
		}
		return fake.Run(ctx, argv)
	}
	now := tw0
	e := helperEnv{Run: run, Now: func() time.Time { return now }, Stat: toolStat(fake.Paths()),
		Sleep: func(context.Context, time.Duration) { now = now.Add(time.Second) }}
	var out, errb syncBuf
	code := helperCLI([]string{"run", "--dir", dir}, &out, &errb, e)
	var res helperResult
	if ok, err := readJSON(dir, resultFile, &res); err != nil || !ok {
		t.Fatalf("result.json: %v %v (stderr %s)", ok, err, errb.String())
	}
	return code, res, seen, fake.Writes()
}

// TC-STG-ERR10: the helper refuses a malformed or tampered input.json before any exec. Owner WI 4.5. Requirements: FR-13, FR-14, NFR-5.
func TestTCSTGERR10_TheHelperRefusesAMalformedOrTampered(t *testing.T) {
	s := newWStack(t, wopts{cfg: cfgAll(scrubCfg("z1"), snapCfg(shareDS))})
	s.prepareStart("op-0000000000000041", "z1", false)
	s.prepareSnapshot("op-0000000000000042", spec.SnapshotInput{Name: "z1/ds/share"})
	scrubBase := inputKeys(t, filepath.Join(s.opsDir, "op-0000000000000041"))
	snapBase := inputKeys(t, filepath.Join(s.opsDir, "op-0000000000000042"))
	scrubBase["operationId"], snapBase["operationId"] = testOpID, testOpID

	// write is base changed by mod, as dirName's input.json.
	write := func(base map[string]any, dirName string, mod func(map[string]any)) string {
		t.Helper()
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		mod(m)
		dir := filepath.Join(t.TempDir(), "ops", dirName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, inputFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	ds233 := "z1/" + strings.Repeat("d", 230)
	for _, c := range []struct {
		name    string
		base    map[string]any
		dirName string
		mod     func(map[string]any)
		field   string
	}{
		{"unknown field", scrubBase, testOpID, set("zpool", "/usr/sbin/zpool"), "zpool"},
		{"bad action", scrubBase, testOpID, set("action", "destroy"), "action"},
		{"other operation", scrubBase, testOpID, set("operationId", "op-fedcba9876543210"), "operationId"},
		{"operation pattern", scrubBase, "op_1", set("operationId", "op_1"), "operationId"},
		{"pool", scrubBase, testOpID, set("pool", "z1; rm -rf /"), "pool"},
		{"dataset", snapBase, testOpID, set("dataset", "z1/../x"), "dataset"},
		{"snapshot path", snapBase, testOpID, set("snapshot", "../x"), "snapshot"},
		{"snapshot pattern", snapBase, testOpID, set("snapshot", "nexus-20261119T051000Z-Bad"), "snapshot"},
		{"256 bytes", snapBase, testOpID, set("dataset", ds233), "snapshot"},
		{"format", scrubBase, testOpID, set("format", "yaml"), "format"},
		{"timeout 4s", scrubBase, testOpID, set("commandTimeout", "4s"), "commandTimeout"},
		{"timeout 121s", scrubBase, testOpID, set("commandTimeout", "121s"), "commandTimeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := write(c.base, c.dirName, c.mod)
			code, res, calls, writes := helperRun(t, dir, nil)
			he := helperProblem(t, res, problem.InternalError)
			if code != 2 || !strings.HasPrefix(he.Detail, "the storage helper refused its input: "+c.field) ||
				!strings.HasSuffix(he.Detail, "; nothing was sent") {
				t.Errorf("exit %d, detail %q", code, he.Detail)
			}
			if c.name == "256 bytes" && !strings.Contains(he.Detail, "255-byte") {
				t.Errorf("detail %q does not name the 255-byte limit", he.Detail)
			}
			if len(calls) != 0 || len(writes) != 0 {
				t.Errorf("calls %q, writes %q, want none at all", calls, writes)
			}
		})
	}

	ds232 := "z1/" + strings.Repeat("d", 229)
	for _, c := range []struct {
		name string
		base map[string]any
		mod  func(map[string]any)
	}{
		{"timeout 5s", scrubBase, set("commandTimeout", "5s")},
		{"timeout 120s", scrubBase, set("commandTimeout", "120s")},
		{"255 bytes", snapBase, set("dataset", ds232)},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := write(c.base, testOpID, c.mod)
			// The 232-byte dataset is not in the capture: zfs answers as
			// it does for a dataset that does not exist.
			missing := func(argv []string) (zfs.Result, bool) {
				if strings.Contains(strings.Join(argv, " "), ds232+"@") {
					return zfs.Result{ExitCode: 1, Stderr: []byte(fmt.Sprintf("cannot open '%s': dataset does not exist\n", ds232))}, true
				}
				return zfs.Result{}, false
			}
			code, res, calls, writes := helperRun(t, dir, missing)
			if res.Error != nil && strings.HasPrefix(res.Error.Detail, "the storage helper refused its input") {
				t.Fatalf("refused: %s", res.Error.Detail)
			}
			if c.base["action"] == actionSnapshot {
				if code != 0 || res.Error == nil || res.Error.Slug != problem.ZFSCommandFailed {
					t.Errorf("exit %d, %+v", code, res.Error)
				}
				return
			}
			if len(calls) == 0 || code != 0 {
				t.Errorf("exit %d, calls %q", code, calls)
			}
			scrubOK(t, code, res)
			if !slices.EqualFunc(writes, [][]string{{"/usr/sbin/zpool", "scrub", "z1"}}, slices.Equal) {
				t.Errorf("writes %q", writes)
			}
		})
	}
}

// subprocess re-executes this binary as the helper of dir in mode and
// returns it running, with the file its F-ZFS logs each argv to.
func helperSubprocess(t *testing.T, dir, mode string) (*exec.Cmd, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls.log")
	cmd := exec.Command(exe, "run", "--dir", dir)
	cmd.Env = append(os.Environ(), helperModeEnv+"="+mode, helperCallsEnv+"="+calls)
	var errb syncBuf
	cmd.Stdout, cmd.Stderr = &errb, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if t.Failed() {
			t.Logf("helper output:\n%s", errb.String())
		}
	})
	return cmd, calls
}

// waitCall waits until the subprocess's F-ZFS received argv.
func waitCall(t *testing.T, calls string, argv []string) {
	t.Helper()
	line := strings.Join(argv, " ")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(calls)
		if err == nil && slices.Contains(strings.Split(string(b), "\n"), line) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the helper never ran %q", line)
}

// sigterm stops cmd with SIGTERM and returns how long it took to exit.
func sigtermHelper(t *testing.T, cmd *exec.Cmd) time.Duration {
	t.Helper()
	sent := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait() // Its exit status is not asserted (DF-7 step 7 decides from result.json)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(helperGrace + 5*time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("the helper did not exit within %s of SIGTERM", helperGrace+5*time.Second)
	}
	return time.Since(sent)
}

// writtenCalls are the write argv in a calls log.
func writtenCalls(t *testing.T, calls string) []string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "zpool scrub") || strings.Contains(l, "zfs snapshot") {
			out = append(out, l)
		}
	}
	return out
}

// TC-STG-ERR11: SIGTERM during the confirm poll ends the helper without a success result. Owner WI 4.5. Requirements: FR-13, CON-1.
func TestTCSTGERR11_SIGTERMDuringTheConfirmPollEndsThe(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1")})
	in, _ := s.prepareStart(testOpID, "z1", false)
	dir := filepath.Join(s.opsDir, testOpID)
	cmd, calls := helperSubprocess(t, dir, "confirm-stuck")
	waitCall(t, calls, s.fake.Argv(zfs.CmdScrubStart, "z1"))
	if took := sigtermHelper(t, cmd); took >= helperGrace {
		t.Errorf("the helper took %s to exit, want under %s", took, helperGrace)
	}
	const detail = "`zpool scrub` was sent; the helper was stopped before `zpool status` confirmed it; check `zpool status z1`"
	var res helperResult
	if ok, err := readJSON(dir, resultFile, &res); err != nil || !ok {
		t.Fatalf("result.json: %v %v", ok, err)
	}
	if res.Result != nil || helperProblem(t, res, problem.OutcomeUnknown).Detail != detail {
		t.Errorf("result %s / %+v", res.Result, res.Error)
	}
	x := detach.Exit{ExitCode: 0, TimedOut: true, Started: time.Now(), Ended: time.Now()}
	if err := detach.WriteExit(dir, x); err != nil {
		t.Fatal(err)
	}
	_, err := s.finalizeStart(testOpID, in, x)
	if pe := wantSlug(t, err, problem.OutcomeUnknown); pe.Detail != detail {
		t.Errorf("Finalize detail %q", pe.Detail)
	}
	if w := writtenCalls(t, calls); !slices.Equal(w, []string{"/usr/sbin/zpool scrub z1"}) {
		t.Errorf("writes %q, want one zpool scrub z1", w)
	}
}

// TC-STG-ERR13: SIGTERM before the write is sent ends Failed dependency-unavailable and sends nothing. Owner WI 4.5. Requirements: FR-13, CON-1.
func TestTCSTGERR13_SIGTERMBeforeTheWriteIsSentEnds(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1")})
	for _, c := range []struct {
		id, command string
		prepare     func(id string) func(detach.Exit) error
	}{
		{"op-0000000000000051", "zpool scrub", func(id string) func(detach.Exit) error {
			in, _ := s.prepareStart(id, "z1", false)
			return func(x detach.Exit) error { _, err := s.finalizeStart(id, in, x); return err }
		}},
		{"op-0000000000000052", "zpool scrub -s", func(id string) func(detach.Exit) error {
			in := s.prepareStop(id, "z1")
			return func(x detach.Exit) error {
				_, err := s.p.stopScrubSpec().Finalize(context.Background(), in, x, runOf(id))
				return err
			}
		}},
	} {
		t.Run(c.command, func(t *testing.T) {
			finalize := c.prepare(c.id)
			dir := filepath.Join(s.opsDir, c.id)
			cmd, calls := helperSubprocess(t, dir, "hang-preread")
			waitCall(t, calls, s.fake.Argv(zfs.CmdPoolStatus, "z1"))
			if took := sigtermHelper(t, cmd); took >= helperGrace {
				t.Errorf("the helper took %s to exit, want under %s", took, helperGrace)
			}
			detail := "the storage helper was stopped before `" + c.command + "` was sent; nothing was sent"
			var res helperResult
			if ok, err := readJSON(dir, resultFile, &res); err != nil || !ok {
				t.Fatalf("result.json: %v %v", ok, err)
			}
			if helperProblem(t, res, problem.DependencyUnavailable).Detail != detail {
				t.Errorf("detail %q", res.Error.Detail)
			}
			x := detach.Exit{ExitCode: 0, TimedOut: true, Started: time.Now(), Ended: time.Now()}
			if err := detach.WriteExit(dir, x); err != nil {
				t.Fatal(err)
			}
			pe := wantSlug(t, finalize(x), problem.DependencyUnavailable)
			if pe.Detail != detail {
				t.Errorf("Finalize detail %q", pe.Detail)
			}
			if w := writtenCalls(t, calls); len(w) != 0 {
				t.Errorf("writes %q, want none", w)
			}
		})
	}
}

// TC-STG-ERR14: the core ends a stopped storage operation from exit.json without Finalize: interrupted is outcome-unknown with LostDetail, canceled is Canceled. Owner WI 4.5. Requirements: FR-13, FR-14, FR-19.
func TestTCSTGERR14_TheCoreEndsAStoppedStorageOperation(t *testing.T) {
	var mu sync.Mutex
	finalized := 0
	count := func() int { mu.Lock(); defer mu.Unlock(); return finalized }
	s := newWStack(t, wopts{cfg: cfgAll(scrubCfg("z1"), snapCfg(shareDS)), bindings: func(p *Provider) []provider.Binding {
		scrub, snap := p.scrubStartSpec(), p.snapshotSpec()
		fs, fn := scrub.Finalize, snap.Finalize
		scrub.Finalize = func(ctx context.Context, in spec.ScrubInput, x detach.Exit, r *provider.Run) (spec.ScrubResult, error) {
			mu.Lock()
			finalized++
			mu.Unlock()
			return fs(ctx, in, x, r)
		}
		snap.Finalize = func(ctx context.Context, in spec.SnapshotInput, x detach.Exit, r *provider.Run) (spec.SnapshotResult, error) {
			mu.Lock()
			finalized++
			mu.Unlock()
			return fn(ctx, in, x, r)
		}
		return []provider.Binding{provider.Detached(spec.PoolsScrub, scrub), provider.Detached(spec.DatasetsSnapshot, snap)}
	}})

	success := map[string]any{
		spec.PoolsScrub.Op().ID: spec.ScrubResult{ActionResult: apitypes.ActionResult{Changed: true}, Pool: "z1",
			Scan: spec.Scan{Function: "scrub", State: "scanning"}},
		spec.DatasetsSnapshot.Op().ID: spec.SnapshotResult{ActionResult: apitypes.ActionResult{Changed: true},
			Snapshot: spec.Snapshot{Name: "z1/ds/share@nexus-20261119T051000Z"}},
	}
	for _, c := range []struct {
		op         def.Op
		input      string
		lost       string
		x          detach.Exit
		wantStatus apitypes.OperationStatus
	}{
		{spec.PoolsScrub.Op(), `{"name":"z1"}`,
			"; the storage helper for scrub-start on pool z1 stopped without a result; check `zpool status z1`",
			detach.Exit{Interrupted: true, ExitCode: 143}, apitypes.StatusFailed},
		{spec.PoolsScrub.Op(), `{"name":"z1"}`, "", detach.Exit{Canceled: true}, apitypes.StatusCanceled},
		{spec.DatasetsSnapshot.Op(), `{"name":"z1/ds/share"}`,
			"; the storage helper for snapshot on dataset z1/ds/share stopped without a result; check `zfs list -t snapshot z1/ds/share`",
			detach.Exit{Interrupted: true, ExitCode: 143}, apitypes.StatusFailed},
		{spec.DatasetsSnapshot.Op(), `{"name":"z1/ds/share"}`, "", detach.Exit{Canceled: true}, apitypes.StatusCanceled},
	} {
		s.mu.Lock()
		s.launch = func(inv detach.Invocation) {
			b, err := json.Marshal(success[c.op.ID])
			if err == nil {
				err = writeJSONAtomic(inv.Dir, resultFile, helperResult{Result: b})
			}
			x := c.x
			x.Started, x.Ended = time.Now(), time.Now()
			if err == nil {
				err = detach.WriteExit(inv.Dir, x)
			}
			if err != nil {
				t.Errorf("plant: %v", err)
			}
		}
		s.mu.Unlock()
		id := s.start(c.op, c.input)
		reads := len(s.fake.Reads())
		rec := s.wait(id)
		if _, err := os.Stat(filepath.Join(s.opsDir, id, inputFile)); err != nil {
			t.Errorf("%s: input.json: %v", id, err)
		}
		switch {
		case rec.Status != c.wantStatus:
			t.Errorf("%s %+v: %s %+v, want %s", c.op.ID, c.x, rec.Status, rec.Error, c.wantStatus)
		case c.lost != "" && (rec.Error == nil || rec.Error.Slug() != problem.OutcomeUnknown || !strings.HasSuffix(rec.Error.Detail, c.lost)):
			t.Errorf("%s: error %+v, want outcome-unknown ending %q", c.op.ID, rec.Error, c.lost)
		case c.lost == "" && rec.Error != nil:
			t.Errorf("%s: canceled with error %+v", c.op.ID, rec.Error)
		}
		if rec.Result != nil {
			t.Errorf("%s: the planted success was used: %s", c.op.ID, rec.Result)
		}
		if n := len(s.fake.Reads()); n != reads {
			t.Errorf("%s: %d ZFS reads after admission", c.op.ID, n-reads)
		}
	}
	if n := count(); n != 0 {
		t.Errorf("Finalize called %d times, want 0", n)
	}
	if w := s.fake.Writes(); len(w) != 0 {
		t.Errorf("writes %q", w)
	}
}
