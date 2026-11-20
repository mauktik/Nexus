// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-141 to
// TC-STG-159, TC-STG-E3, TC-STG-E5, TC-STG-E10 and TC-STG-E12 (owner WI
// 4.3), plus the handler-level half of TC-STG-119 carried forward from WI
// 4.1.
//
// The bindings land in WI 5.1, so "through the Dispatcher" runs a real
// core.Dispatcher over filesBound, the provider with the three backup and
// Time Machine reads bound here (WI 4.2's zfsReadsBound precedent). The
// storage.health.get steps of TC-STG-142, 143, 155 and 159 were added by
// WI 4.4 (health.go), carried forward from this WI.
//
// F-OPENER is fopener below. *os.File's ReadDir cannot be wrapped, so an
// Open of a directory through the root stands for its ReadDir: the call is
// recorded as both, and a ReadDir rule on that name applies to the Open.
// fs.WalkDir's ReadDir through the root's FS is wrapped directly.

package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
	"github.com/mauktik/nexus/internal/providers/storage/timemachine"
	"github.com/mauktik/nexus/internal/providers/storage/vzdump"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// ---- test support (WI 4.3) ----

// filesBound is the provider with the backup and Time Machine reads bound,
// and storage.health.get (WI 4.4), for the Dispatcher before the bindings of
// WI 5.1 exist.
type filesBound struct{ *Provider }

func (b filesBound) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.HealthGet, b.healthGet),
		provider.Sync(spec.BackupsList, b.backupsList),
		provider.Sync(spec.BackupsSummary, b.backupsSummary),
		provider.Sync(spec.TimeMachineList, b.timemachineList),
	}
}

// F-OPENER rule kinds.
const (
	ruleStuck = iota // ignores ctx until release closes
	ruleBlock        // until the gate opens or ctx ends
	rulePanic
)

type fRule struct {
	kind int
	gate <-chan struct{}
	ctx  context.Context
}

type fCall struct{ path, op, name string }

// fopener is F-OPENER over every root the provider opens: it records each
// call by configured path, op and name, and applies the rules of an exact
// (path, op, name).
type fopener struct {
	mu    sync.Mutex
	rules map[fCall]fRule
	calls []fCall
}

func newFOpener() *fopener { return &fopener{rules: map[fCall]fRule{}} }

func (f *fopener) set(p, op, name string, r fRule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules[fCall{p, op, path.Clean(name)}] = r
}

func (f *fopener) stuck(p, op, name string, release <-chan struct{}) {
	f.set(p, op, name, fRule{kind: ruleStuck, gate: release})
}

// hit records the call and applies its rule.
func (f *fopener) hit(p, op, name string) {
	c := fCall{p, op, path.Clean(name)}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	r, ok := f.rules[c]
	f.mu.Unlock()
	if !ok {
		return
	}
	switch r.kind {
	case ruleStuck:
		<-r.gate
	case ruleBlock:
		select {
		case <-r.gate:
		case <-r.ctx.Done():
		}
	case rulePanic:
		panic(fmt.Sprintf("F-OPENER panic on %s %s", op, name))
	}
}

// count is the number of recorded calls of op on path whose name match
// accepts.
func (f *fopener) count(p, op string, match func(name string) bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.path == p && c.op == op && match(c.name) {
			n++
		}
	}
	return n
}

// names is every recorded name of op on any path that match accepts.
func (f *fopener) names(op string, match func(name string) bool) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.op == op && match(c.name) {
			out = append(out, c.name)
		}
	}
	return out
}

func is(name string) func(string) bool { return func(n string) bool { return n == name } }

func suffix(s string) func(string) bool {
	return func(n string) bool { return strings.HasSuffix(n, s) }
}

func (f *fopener) openRoot(p string) (Root, error) {
	r, err := os.OpenRoot(p)
	if err != nil {
		return nil, err
	}
	return &fRoot{Root: r, f: f, path: p}, nil
}

type fRoot struct {
	*os.Root
	f    *fopener
	path string
}

func (r *fRoot) Open(name string) (*os.File, error) {
	r.f.hit(r.path, "Open", name)
	if fi, err := r.Root.Stat(name); err == nil && fi.IsDir() {
		r.f.hit(r.path, "ReadDir", name)
	}
	return r.Root.Open(name)
}

func (r *fRoot) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	r.f.hit(r.path, "Open", name)
	// A directory opened with O_DIRECTORY (B-SEC-2) is listed like one
	// opened with Open, so the ReadDir rules still apply to it.
	if flag&syscall.O_DIRECTORY != 0 {
		r.f.hit(r.path, "ReadDir", name)
	}
	return r.Root.OpenFile(name, flag, perm)
}

func (r *fRoot) Stat(name string) (fs.FileInfo, error) {
	r.f.hit(r.path, "Stat", name)
	return r.Root.Stat(name)
}

func (r *fRoot) Lstat(name string) (fs.FileInfo, error) {
	r.f.hit(r.path, "Lstat", name)
	return r.Root.Lstat(name)
}

func (r *fRoot) FS() fs.FS { return fRootFS{r} }

// fRootFS is the root's FS: Open (the file itself, so io.ReaderAt is kept),
// ReadDir and Stat, each recorded.
type fRootFS struct{ r *fRoot }

func (f fRootFS) Open(name string) (fs.File, error) {
	f.r.f.hit(f.r.path, "Open", name)
	return f.r.Root.FS().Open(name)
}

func (f fRootFS) ReadDir(name string) ([]fs.DirEntry, error) {
	f.r.f.hit(f.r.path, "ReadDir", name)
	return fs.ReadDir(f.r.Root.FS(), name)
}

func (f fRootFS) Stat(name string) (fs.FileInfo, error) {
	return fs.Stat(f.r.Root.FS(), name)
}

// fstatfs is F-STATFS: scripted usage or errors per configured share,
// counted per path, Stuck per path; any other path fails the test.
type fstatfs struct {
	t     *testing.T
	mu    sync.Mutex
	usage map[string]Usage
	errs  map[string]error
	stuck map[string]<-chan struct{}
	calls map[string]int
}

func newFStatfs(t *testing.T) *fstatfs {
	return &fstatfs{t: t, usage: map[string]Usage{}, errs: map[string]error{}, stuck: map[string]<-chan struct{}{},
		calls: map[string]int{}}
}

func (f *fstatfs) statfs(p string) (Usage, error) {
	f.mu.Lock()
	f.calls[p]++
	u, okU := f.usage[p]
	err, okE := f.errs[p]
	gate := f.stuck[p]
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if !okU && !okE {
		f.t.Errorf("statfs of %s, which is not a scripted share", p)
		return Usage{}, errors.New("not a scripted share")
	}
	return u, err
}

func (f *fstatfs) set(p string, u Usage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage[p] = u
}

func (f *fstatfs) n(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[p]
}

// fticker is the Tick seam (F-CLOCK): each wait is handed to the test on
// waits, so a tick is sent only once the loop waits.
type fticker struct {
	mu    sync.Mutex
	reqs  []time.Duration
	waits chan chan time.Time
}

func newFTicker() *fticker { return &fticker{waits: make(chan chan time.Time, 64)} }

func (k *fticker) tick(d time.Duration) (<-chan time.Time, func()) {
	ch := make(chan time.Time, 1)
	k.mu.Lock()
	k.reqs = append(k.reqs, d)
	k.mu.Unlock()
	k.waits <- ch
	return ch, func() {}
}

// next is the wait the loop is in, failing after 5 s.
func (k *fticker) next(t *testing.T) chan time.Time {
	t.Helper()
	select {
	case c := <-k.waits:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the Background loop did not wait within 5s")
		return nil
	}
}

func (k *fticker) drain() {
	for {
		select {
		case <-k.waits:
		default:
			return
		}
	}
}

func (k *fticker) last() time.Duration {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reqs[len(k.reqs)-1]
}

// fwalks is the walk observer: starts and ends per share.
type fwalks struct {
	mu            sync.Mutex
	starts, ended map[string]int
}

func newFWalks() *fwalks { return &fwalks{starts: map[string]int{}, ended: map[string]int{}} }

func (w *fwalks) observe(share string, start bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if start {
		w.starts[share]++
	} else {
		w.ended[share]++
	}
}

func (w *fwalks) get(share string) (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.starts[share], w.ended[share]
}

func (w *fwalks) total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, v := range w.starts {
		n += v
	}
	return n
}

func (w *fwalks) waitEnded(t *testing.T, share string, n int) {
	t.Helper()
	waitFor(t, func() bool { _, e := w.get(share); return e >= n })
}

// fstack is the provider over real fixture trees behind a Dispatcher, after
// the registry's startup preflight, at F-CLOCK t0.
type fstack struct {
	p   *Provider
	rec *records
	clk *clock
	op  *fopener
	sf  *fstatfs
	tk  *fticker
	wk  *fwalks
	d   *core.Dispatcher
}

type fopts struct {
	vzdump, tm []string
	mod        func(*config.StorageConfig)
	fsys       fs.FS      // Nil: no tools (zfs unavailable)
	run        zfs.Runner // Nil: noRun
	timeout    time.Duration
}

func newFStack(t *testing.T, o fopts) *fstack {
	t.Helper()
	s := &fstack{clk: &clock{}, op: newFOpener(), sf: newFStatfs(t), tk: newFTicker(), wk: newFWalks()}
	s.clk.set(t0)
	for _, p := range o.tm {
		s.sf.set(p, Usage{Blocks: 1000, Bfree: 600, Bavail: 600, Bsize: 4096})
	}
	fsys, run := o.fsys, o.run
	if fsys == nil {
		fsys = fstest.MapFS{}
	}
	if run == nil {
		run = noRun(t)
	}
	s.p, s.rec = newTestProvider(fsys, run, func(op *Options) {
		op.Now, op.OpenRoot, op.Statfs, op.Tick, op.ObserveWalk = s.clk.Now, s.op.openRoot, s.sf.statfs, s.tk.tick, s.wk.observe
	})
	reg, err := core.New([]provider.Provider{filesBound{s.p}}, core.Options{
		Host: "tc", Version: "v0.0.0-test", Logger: discard,
		Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: func(into any) error {
			c := into.(*config.StorageConfig)
			c.Enabled = true
			c.Backups.VzdumpPaths = o.vzdump
			c.TimeMachine.Paths = o.tm
			if o.mod != nil {
				o.mod(c)
			}
			return nil
		}}},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	if o.timeout != 0 {
		setCommandTimeout(s.p, o.timeout)
	}
	s.d = core.NewDispatcher(reg, core.DispatcherOptions{Logger: discard})
	return s
}

func (s *fstack) invoke(op def.Op, input string) (any, error) {
	res, err := s.d.Invoke(context.Background(), core.InvokeRequest{
		Caller:  auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "tc-stg-files", Scopes: []string{spec.ReadScope}},
		Surface: core.SurfaceREST, Op: op, Input: json.RawMessage(input),
	})
	if err != nil {
		return nil, err
	}
	if res.Status != 200 {
		return nil, fmt.Errorf("status %d", res.Status)
	}
	return res.Body, nil
}

func fcall[T any](t *testing.T, s *fstack, op def.Op, input string) T {
	t.Helper()
	body, err := s.invoke(op, input)
	if err != nil {
		t.Fatalf("%s %s: %v", op.ID, input, err)
	}
	v, ok := body.(T)
	if !ok {
		t.Fatalf("%s body %T", op.ID, body)
	}
	return v
}

func (s *fstack) backups(t *testing.T, input string) []spec.Backup {
	t.Helper()
	return fcall[apitypes.List[spec.Backup]](t, s, spec.BackupsList.Op(), input).Items
}

func (s *fstack) summary(t *testing.T) spec.BackupSummaryProperties {
	t.Helper()
	l := fcall[apitypes.List[spec.BackupSummary]](t, s, spec.BackupsSummary.Op(), "{}")
	if len(l.Items) != 1 || l.Items[0].Name != spec.CurrentName {
		t.Fatalf("summary items %+v, want one named current", l.Items)
	}
	return l.Items[0].Properties
}

func (s *fstack) shares(t *testing.T) []spec.Share {
	t.Helper()
	return fcall[apitypes.List[spec.Share]](t, s, spec.TimeMachineList.Op(), "{}").Items
}

// background starts RunBackground and returns its cancel and done.
func (s *fstack) background(t *testing.T) (context.Context, context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.p.RunBackground(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ctx, cancel, done
}

// timed runs fn and returns how long it took.
func timed(fn func()) time.Duration {
	start := time.Now()
	fn()
	return time.Since(start)
}

// vz is one planted vzdump archive with its companions.
type vz struct {
	typ   string
	vmid  int
	stamp time.Time // The name's time
	mtime time.Time
	size  int64
	dir   string // "" or "dump"
	notes []byte // Nil: no notes file
	log   []byte // Nil: no log file
}

func (a vz) stem() string {
	return fmt.Sprintf("vzdump-%s-%d-%s", a.typ, a.vmid, a.stamp.Format("2006_01_02-15_04_05"))
}

func (a vz) file() string {
	if a.typ == spec.GuestLXC {
		return a.stem() + ".tar.zst"
	}
	return a.stem() + ".vma.zst"
}

func (a vz) rel(name string) string {
	if a.dir == "" {
		return name
	}
	return a.dir + "/" + name
}

func (a vz) logName() string { return a.stem() + ".log" }

func (a vz) nodes() []storagetest.Node {
	n := []storagetest.Node{{Path: a.rel(a.file()), Size: a.size, ModTime: a.mtime}}
	if a.notes != nil {
		n = append(n, storagetest.Node{Path: a.rel(a.file() + ".notes"), Content: a.notes, ModTime: a.mtime})
	}
	if a.log != nil {
		n = append(n, storagetest.Node{Path: a.rel(a.logName()), Content: a.log, ModTime: a.mtime})
	}
	return n
}

func okLog(vmid int) []byte {
	return []byte(fmt.Sprintf("2026-11-17 03:00:02 INFO: Starting Backup of VM %d (qemu)\n"+
		"2026-11-17 03:04:10 INFO: Finished Backup of VM %d (00:04:08)\n", vmid, vmid))
}

func errLog(vmid int) []byte {
	return append(okLog(vmid), []byte("2026-11-17 03:04:11 ERROR: Backup of VM "+strconv.Itoa(vmid)+" failed - x\n")...)
}

// vzTree plants archives in a new directory and returns it.
func vzTree(t *testing.T, archives ...vz) string {
	t.Helper()
	dir := storagetest.TempDir(t)
	var nodes []storagetest.Node
	for _, a := range archives {
		nodes = append(nodes, a.nodes()...)
	}
	storagetest.Build(t, dir, nodes...)
	return dir
}

// three is three qemu archives of vmid, a day apart before t0, each with
// notes and an ok log.
func three(vmid int) []vz {
	var out []vz
	for i := range 3 {
		at := t0.Add(-time.Duration(i+1) * 24 * time.Hour)
		out = append(out, vz{typ: spec.GuestQemu, vmid: vmid, stamp: at, mtime: at, size: int64(1000 + i),
			notes: []byte(fmt.Sprintf("guest-%d\n", vmid)), log: okLog(vmid)})
	}
	return out
}

func inDir(items []spec.Backup, dir string) int {
	n := 0
	for _, b := range items {
		if b.Properties.Directory == dir {
			n++
		}
	}
	return n
}

// bundleTree plants sparsebundles with XML history plists under a new
// share directory: name → completion date; each gets n band files.
func bundleTree(t *testing.T, bands int, dates map[string]time.Time) string {
	t.Helper()
	share := storagetest.TempDir(t)
	var nodes []storagetest.Node
	for name, at := range dates {
		dir := name + ".sparsebundle"
		nodes = append(nodes, storagetest.BundleNodes(dir)...)
		nodes = append(nodes, storagetest.Node{Path: dir + "/" + timemachine.SnapshotHistoryFile,
			Content: storagetest.SnapshotHistoryXML(at), ModTime: at})
		nodes = append(nodes, storagetest.BandNodes(dir, bands, bytes.Repeat([]byte("b"), 5000))...)
	}
	storagetest.Build(t, share, nodes...)
	return share
}

// alloc is st_blocks × 512 of p, not following a symlink.
func alloc(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("no syscall.Stat_t on %s", runtime.GOOS)
	}
	return uint64(st.Blocks) * 512
}

// allocTree is st_blocks × 512 over dir and everything under it, as du -s
// counts.
func allocTree(t *testing.T, dir string) uint64 {
	t.Helper()
	var sum uint64
	if err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sum += alloc(t, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return sum
}

func shareByPath(t *testing.T, shares []spec.Share, p string) spec.ShareProperties {
	t.Helper()
	for _, s := range shares {
		if s.Properties.Path == p {
			return s.Properties
		}
	}
	t.Fatalf("no share %s in %+v", p, shares)
	return spec.ShareProperties{}
}

func bundleByName(t *testing.T, sp spec.ShareProperties, name string) spec.BundleProperties {
	t.Helper()
	for _, b := range sp.Bundles {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("no bundle %q in %s: %+v", name, sp.Path, sp.Bundles)
	return spec.BundleProperties{}
}

func guestOf(t *testing.T, sum spec.BackupSummaryProperties, typ string, vmid int) spec.GuestBackups {
	t.Helper()
	for _, g := range sum.Guests {
		if g.GuestType == typ && g.VMID == vmid {
			return g
		}
	}
	t.Fatalf("no guest %s/%d in %+v", typ, vmid, sum.Guests)
	return spec.GuestBackups{}
}

func dirOf(t *testing.T, sum spec.BackupSummaryProperties, p string) spec.BackupDirectory {
	t.Helper()
	for _, d := range sum.Directories {
		if d.Path == p {
			return d
		}
	}
	t.Fatalf("no directory %s in %+v", p, sum.Directories)
	return spec.BackupDirectory{}
}

func wantReason(t *testing.T, u spec.Unavailable, field string, parts ...string) {
	t.Helper()
	r, ok := u[field]
	if !ok {
		t.Errorf("unavailable has no %s: %v", field, u)
		return
	}
	for _, p := range parts {
		if !strings.Contains(r, p) {
			t.Errorf("unavailable[%s] = %q, want it to contain %q", field, r, p)
		}
	}
}

// ---- TC-STG-141 to TC-STG-144: stalls (D-15) ----

// TC-STG-141: bounded runs one flight per key, answers a stuck key at once with PendingError, and keeps at most one goroutine per stuck key. Owner WI 4.3. Requirements: NFR-2, NFR-7.
func TestTCSTG141_BoundedRunsOneFlightPerKeyAnswers(t *testing.T) {
	const timeout = 300 * time.Millisecond
	newP := func() *Provider {
		p, _ := newTestProvider(fstest.MapFS{}, noRun(t))
		setCommandTimeout(p, timeout)
		return p
	}
	ctx := context.Background()

	// Step 2 first, on its own provider, so its stuck flight is gone before
	// the goroutine baseline.
	{
		p2 := newP()
		var calls atomic.Int32
		release := make(chan struct{})
		stuck := func() (int, error) { calls.Add(1); <-release; return 1, nil }
		var wg sync.WaitGroup
		errs := make([]error, 11)
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[0] = bounded(ctx, p2.fl, "/a", "scan /a", stuck) }()
		waitFor(t, func() bool { return p2.fl.count() == 1 })
		for i := 1; i <= 10; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, errs[i] = bounded(ctx, p2.fl, "/a", "scan /a", stuck) }()
		}
		wg.Wait()
		if n := calls.Load(); n != 1 {
			t.Errorf("step 2: fn invoked %d times, want 1 (every caller joins the flight)", n)
		}
		for i, err := range errs {
			var te zfs.TimeoutError
			if !errors.As(err, &te) {
				t.Errorf("step 2 caller %d: %v, want a TimeoutError", i, err)
			}
		}
		close(release)
		waitFor(t, func() bool { return p2.fl.count() == 0 })
	}

	baseline := runtime.NumGoroutine()
	p := newP()
	var stuckCalls, readCalls atomic.Int32
	release := make(chan struct{})
	stuck := func() (int, error) { stuckCalls.Add(1); <-release; return 1, nil }
	fastRead := func() (int, error) { readCalls.Add(1); return 2, nil }

	// Step 1.
	var err error
	el := timed(func() { _, err = bounded(ctx, p.fl, "/a", "scan /a", stuck) })
	if el > timeout+100*time.Millisecond {
		t.Errorf("step 1 took %s, want at most 400ms", el)
	}
	if err == nil || err.Error() != "scan /a did not finish within 300ms" {
		t.Errorf("step 1 err = %v, want %q", err, "scan /a did not finish within 300ms")
	}

	// Step 3.
	for i := range 20 {
		var err error
		el := timed(func() { _, err = bounded(ctx, p.fl, "/a", "scan /a", stuck) })
		var pe zfs.PendingError
		if el >= 50*time.Millisecond || !errors.As(err, &pe) || pe.What != "scan /a" {
			t.Errorf("step 3 call %d: %v after %s, want a PendingError naming scan /a at once", i, err, el)
		}
	}
	if n := stuckCalls.Load(); n != 1 {
		t.Errorf("step 3: fn invoked %d times, want 1", n)
	}
	if g := runtime.NumGoroutine(); g > baseline+1 {
		t.Errorf("step 3: %d goroutines, want at most baseline %d + 1", g, baseline)
	}
	if n := p.fl.count(); n != 1 {
		t.Errorf("step 3: %d keys in flight, want 1", n)
	}
	el = timed(func() { _, err = bounded(ctx, p.fl, "/a", "read /a/x", fastRead) })
	var pe zfs.PendingError
	if el >= 50*time.Millisecond || !errors.As(err, &pe) || pe.What != "scan /a" {
		t.Errorf("step 3 read /a/x: %v after %s, want a PendingError naming the stuck key scan /a", err, el)
	}
	if n := readCalls.Load(); n != 0 {
		t.Errorf("read /a/x fn invoked %d times on a stuck path, want 0", n)
	}

	// Step 4.
	var v int
	el = timed(func() { v, err = bounded(ctx, p.fl, "/b", "scan /b", func() (int, error) { return 3, nil }) })
	if err != nil || v != 3 || el >= 50*time.Millisecond {
		t.Errorf("step 4 scan /b = %d, %v after %s, want 3 at once", v, err, el)
	}

	// Step 5: the per-path cap on a fresh provider with a timeout the test
	// controls; one gate opens at 100 ms, so the fifth gets its slot.
	{
		p5 := newP()
		setCommandTimeout(p5, time.Second)
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		gates := make([]chan struct{}, 5)
		var running, peak atomic.Int32
		var wg sync.WaitGroup
		for i := range 5 {
			gates[i] = make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := bounded(cctx, p5.fl, "/a", fmt.Sprintf("read /a/%d", i), func() (int, error) {
					n := running.Add(1)
					for {
						if old := peak.Load(); n <= old || peak.CompareAndSwap(old, n) {
							break
						}
					}
					defer running.Add(-1)
					select {
					case <-gates[i]:
					case <-cctx.Done():
					}
					return i, nil
				})
				if err != nil {
					t.Errorf("step 5 key %d: %v", i, err)
				}
			}()
		}
		waitFor(t, func() bool { return running.Load() == 4 })
		time.Sleep(50 * time.Millisecond)
		if n := p5.fl.countUnder("/a"); n != 4 {
			t.Errorf("step 5: %d flights under /a, want 4 (the fifth waits for a slot)", n)
		}
		for i := range 5 {
			if i == 0 {
				time.Sleep(50 * time.Millisecond)
			}
			close(gates[i])
		}
		wg.Wait()
		if n := peak.Load(); n > int32(spec.MaxPathFlights) {
			t.Errorf("step 5: %d flights ran at once under /a, want at most %d", n, spec.MaxPathFlights)
		}
	}

	// Step 6.
	{
		cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		hold := make(chan struct{})
		el := timed(func() { _, err = bounded(cctx, p.fl, "/c", "scan /c", func() (int, error) { <-hold; return 0, nil }) })
		cancel()
		close(hold)
		var te zfs.TimeoutError
		if el > 150*time.Millisecond || !errors.As(err, &te) {
			t.Errorf("step 6: %v after %s, want a TimeoutError within 150ms", err, el)
		}
	}

	// Step 7.
	close(release)
	waitFor(t, func() bool { return p.fl.countUnder("/a") == 0 })
	if v, err := bounded(ctx, p.fl, "/a", "read /a/x", fastRead); err != nil || v != 2 || readCalls.Load() != 1 {
		t.Errorf("step 7 read /a/x = %d, %v (fn %d), want it run", v, err, readCalls.Load())
	}
	if _, err := bounded(ctx, p.fl, "/a", "scan /a", stuck); err != nil || stuckCalls.Load() != 2 {
		t.Errorf("step 7 scan /a: %v, fn invoked %d times, want a second invocation", err, stuckCalls.Load())
	}
}

// TC-STG-142: a stuck vzdump directory or file read makes only that path or field unavailable, and the next call answers at once. Owner WI 4.3. Requirements: NFR-2, NFR-7, FR-9, FR-10.
func TestTCSTG142_AStuckVzdumpDirectoryOrFileRead(t *testing.T) {
	const timeout = 300 * time.Millisecond
	t.Run("stuck directory read", func(t *testing.T) {
		a, b := vzTree(t, three(101)...), vzTree(t, three(102)...)
		releaseA := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(releaseA) }) })
		s := newFStack(t, fopts{vzdump: []string{a, b}, timeout: timeout})
		s.op.stuck(a, "ReadDir", ".", releaseA)

		var items []spec.Backup
		if el := timed(func() { items = s.backups(t, "{}") }); el > timeout+100*time.Millisecond {
			t.Errorf("step 1 took %s, want at most 400ms", el)
		}
		if len(items) != 3 || inDir(items, b) != 3 {
			t.Errorf("step 1: %d items (%d of B), want B's 3", len(items), inDir(items, b))
		}

		if el := timed(func() { items = s.backups(t, "{}") }); el >= 50*time.Millisecond {
			t.Errorf("step 2 list took %s, want under 50ms", el)
		}
		if inDir(items, b) != 3 || len(items) != 3 {
			t.Errorf("step 2: %d items, want B's 3", len(items))
		}
		var sum spec.BackupSummaryProperties
		if el := timed(func() { sum = s.summary(t) }); el >= 50*time.Millisecond {
			t.Errorf("step 2 summary took %s, want under 50ms", el)
		}
		if n := s.op.count(a, "ReadDir", is(".")); n != 1 {
			t.Errorf("%d ReadDir(.) of A, want exactly 1", n)
		}
		da, db := dirOf(t, sum, a), dirOf(t, sum, b)
		if da.Available || !strings.Contains(da.Reason, "scan "+a+" is still running since ") {
			t.Errorf("A = %+v, want unavailable naming the pending scan", da)
		}
		if !db.Available || db.Archives != 3 {
			t.Errorf("B = %+v, want available with 3 archives", db)
		}
		var h spec.HealthProperties
		if el := timed(func() { h = healthOf(t, s.d) }); el >= 50*time.Millisecond {
			t.Errorf("step 2 health took %s, want under 50ms", el)
		}
		if !slices.Contains(h.Checked, featureBackups) || !strings.Contains(h.Unavailable[featureBackups], "scan "+a) {
			t.Errorf("health checked %v unavailable %v, want backups checked (B evaluated) and A's reason", h.Checked,
				h.Unavailable)
		}
		if n := s.op.count(a, "ReadDir", is(".")); n != 1 {
			t.Errorf("%d ReadDir(.) of A after health, want still 1", n)
		}

		once.Do(func() { close(releaseA) })
		waitFor(t, func() bool { return s.p.fl.count() == 0 })
		if items = s.backups(t, "{}"); inDir(items, a) != 3 || inDir(items, b) != 3 {
			t.Errorf("step 3: %d of A, %d of B, want 3 each", inDir(items, a), inDir(items, b))
		}
	})

	t.Run("stuck log read", func(t *testing.T) {
		archB := three(102)
		a, b := vzTree(t, three(101)...), vzTree(t, archB...)
		newest := archB[0]
		releaseL := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(releaseL) }) })
		s := newFStack(t, fopts{vzdump: []string{a, b}, timeout: timeout})
		s.op.stuck(b, "Open", newest.logName(), releaseL)

		var items []spec.Backup
		if el := timed(func() { items = s.backups(t, "{}") }); el > timeout+100*time.Millisecond {
			t.Errorf("step 4 first list took %s, want at most 400ms", el)
		}
		if len(items) != 6 {
			t.Fatalf("step 4: %d items, want 6", len(items))
		}
		for _, it := range items {
			bp := it.Properties
			if bp.Directory == b && bp.File == newest.file() {
				if bp.LogStatus != vzdump.LogUnknown || bp.Notes == nil || bp.SizeBytes == nil {
					t.Errorf("affected archive %+v, want logStatus unknown and its other fields filled", bp)
				}
				wantReason(t, bp.Unavailable, "logStatus", "read "+b+"/"+newest.logName(), "did not finish within")
				continue
			}
			if bp.LogStatus != vzdump.LogOK || bp.Notes == nil || len(bp.Unavailable) != 0 {
				t.Errorf("archive %s/%s = %+v, want complete", bp.Directory, bp.File, bp)
			}
		}

		if el := timed(func() { items = s.backups(t, "{}") }); el >= 50*time.Millisecond {
			t.Errorf("later list took %s, want under 50ms", el)
		}
		if len(items) != 3 || inDir(items, a) != 3 {
			t.Errorf("later list: %d items, want A's 3 only", len(items))
		}
		var sum spec.BackupSummaryProperties
		if el := timed(func() { sum = s.summary(t) }); el >= 50*time.Millisecond {
			t.Errorf("later summary took %s, want under 50ms", el)
		}
		if db := dirOf(t, sum, b); db.Available || !strings.Contains(db.Reason, "read "+b+"/"+newest.logName()) {
			t.Errorf("B = %+v, want unavailable naming the stuck key", db)
		}
		if n := s.op.count(b, "Open", is(newest.logName())); n != 1 {
			t.Errorf("%d opens of the stuck log, want 1", n)
		}
		if n := s.op.count(b, "ReadDir", is(".")); n != 1 {
			t.Errorf("%d ReadDir(.) of B, want 1 (no new listing of a stuck path)", n)
		}
	})

	t.Run("stuck notes read", func(t *testing.T) {
		arch := three(103)
		dir := vzTree(t, arch...)
		release := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		s := newFStack(t, fopts{vzdump: []string{dir}, timeout: timeout})
		s.op.stuck(dir, "Open", arch[0].file()+".notes", release)
		for _, it := range s.backups(t, "{}") {
			bp := it.Properties
			if bp.File != arch[0].file() {
				continue
			}
			if bp.Notes != nil {
				t.Errorf("notes = %q, want null", *bp.Notes)
			}
			wantReason(t, bp.Unavailable, "notes", "read "+dir+"/"+arch[0].file()+".notes")
		}
	})
}

// TC-STG-143: a stuck statfs or plist read on one Time Machine share leaves the other share and the other fields serving. Owner WI 4.3. Requirements: NFR-2, NFR-7, FR-11, FR-15.
func TestTCSTG143_AStuckStatfsOrPlistReadOn(t *testing.T) {
	const timeout = 300 * time.Millisecond
	dates := map[string]time.Time{"mac-1": t0.Add(-24 * time.Hour), "mac-2": t0.Add(-48 * time.Hour)}
	statfsFields := []string{"sizeBytes", "usedBytes", "availableBytes", "usedPercent"}
	complete := func(t *testing.T, sp spec.ShareProperties) {
		t.Helper()
		if sp.SizeBytes == nil || sp.UsedBytes == nil || sp.AvailableBytes == nil || sp.UsedPercent == nil ||
			len(sp.Bundles) != 2 {
			t.Errorf("share %s = %+v, want complete", sp.Path, sp)
		}
		for _, b := range sp.Bundles {
			if b.LastBackupAt == nil || b.LastBackupSource != sourcePlist {
				t.Errorf("bundle %s/%s = %+v, want its plist date", sp.Path, b.Name, b)
			}
		}
	}

	t.Run("stuck statfs", func(t *testing.T) {
		tm1, tm2 := bundleTree(t, 1, dates), bundleTree(t, 1, dates)
		releaseS := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(releaseS) }) })
		s := newFStack(t, fopts{tm: []string{tm1, tm2}, timeout: timeout})
		s.sf.mu.Lock()
		s.sf.stuck[tm1] = releaseS
		s.sf.mu.Unlock()

		var shares []spec.Share
		if el := timed(func() { shares = s.shares(t) }); el > timeout+100*time.Millisecond {
			t.Errorf("first list took %s, want at most 400ms", el)
		}
		if el := timed(func() { shares = s.shares(t) }); el >= 50*time.Millisecond {
			t.Errorf("second list took %s, want under 50ms", el)
		}
		sp1 := shareByPath(t, shares, tm1)
		if sp1.SizeBytes != nil || sp1.UsedBytes != nil || sp1.AvailableBytes != nil || sp1.UsedPercent != nil {
			t.Errorf("tm1 usage %+v, want null", sp1)
		}
		for _, f := range statfsFields {
			wantReason(t, sp1.Unavailable, f, "statfs "+tm1)
		}
		complete(t, shareByPath(t, shares, tm2))
		if n := s.sf.n(tm1); n != 1 {
			t.Errorf("%d statfs calls for tm1, want 1", n)
		}
		h := healthOf(t, s.d)
		for _, sg := range h.Signals {
			if sg.ID == signalTMShareFull && sg.Subject == "share:"+tm1 {
				t.Errorf("health raised %v for tm1, whose usage is unknown", sg)
			}
		}
		if !strings.Contains(h.Unavailable[featureTimeMachine], "statfs "+tm1) {
			t.Errorf("health unavailable %v, want tm1's reason", h.Unavailable)
		}

		// Step 3: share usage of tm1 at 96% once released.
		s.sf.set(tm1, Usage{Blocks: 100, Bfree: 4, Bavail: 4, Bsize: 1 << 20})
		once.Do(func() { close(releaseS) })
		waitFor(t, func() bool { return s.p.fl.count() == 0 })
		sp1 = shareByPath(t, s.shares(t), tm1)
		if sp1.UsedPercent == nil || *sp1.UsedPercent != 96 {
			t.Errorf("tm1 usedPercent %v, want 96 once released", sp1.UsedPercent)
		}
		complete(t, sp1)
		h = healthOf(t, s.d)
		if g := signalOf(t, h, signalTMShareFull, "share:"+tm1); g.Severity != apitypes.SeverityCritical {
			t.Errorf("tm1 at 96%%: %+v, want critical", g)
		}
	})

	t.Run("stuck plist read", func(t *testing.T) {
		tm1, tm2 := bundleTree(t, 1, dates), bundleTree(t, 1, dates)
		releaseP := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(releaseP) }) })
		s := newFStack(t, fopts{tm: []string{tm1, tm2}, timeout: timeout})
		plist := "mac-1.sparsebundle/" + timemachine.SnapshotHistoryFile
		s.op.stuck(tm2, "Open", plist, releaseP)

		var shares []spec.Share
		if el := timed(func() { shares = s.shares(t) }); el > timeout+100*time.Millisecond {
			t.Errorf("first list took %s, want at most 400ms", el)
		}
		sp2 := shareByPath(t, shares, tm2)
		b1, b2 := bundleByName(t, sp2, "mac-1"), bundleByName(t, sp2, "mac-2")
		if b1.LastBackupAt != nil || b1.LastModifiedAt != nil {
			t.Errorf("bundle 1 = %+v, want its dates null", b1)
		}
		wantReason(t, b1.Unavailable, "lastBackupAt", "read "+tm2+"/mac-1.sparsebundle")
		wantReason(t, b1.Unavailable, "lastModifiedAt", "read "+tm2+"/mac-1.sparsebundle")
		if b2.LastBackupAt == nil || sp2.UsedPercent == nil {
			t.Errorf("bundle 2 %+v / tm2 usage %v, want complete", b2, sp2.UsedPercent)
		}
		complete(t, shareByPath(t, shares, tm1))

		if el := timed(func() { shares = s.shares(t) }); el >= 50*time.Millisecond {
			t.Errorf("second list took %s, want under 50ms", el)
		}
		sp2 = shareByPath(t, shares, tm2)
		if sp2.UsedPercent != nil || len(sp2.Bundles) != 0 {
			t.Errorf("tm2 = %+v, want usage null and no bundles on a stuck path", sp2)
		}
		for _, f := range append(statfsFields, "bundles") {
			wantReason(t, sp2.Unavailable, f, "read "+tm2+"/mac-1.sparsebundle")
		}
		complete(t, shareByPath(t, shares, tm1))
		if n := s.op.count(tm2, "Open", is(plist)); n != 1 {
			t.Errorf("%d opens of the stuck plist, want 1", n)
		}
	})
}

// TC-STG-144: a stuck Background walk does not hold RunBackground past cancel and never gets a second walk beside it. Owner WI 4.3. Requirements: NFR-2, NFR-7, FR-12.
func TestTCSTG144_AStuckBackgroundWalkDoesNotHold(t *testing.T) {
	s1 := bundleTree(t, 3, map[string]time.Time{"one": t0.Add(-time.Hour)})
	s2 := bundleTree(t, 3, map[string]time.Time{"two": t0.Add(-time.Hour)})
	releaseW := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(releaseW) }) })
	s := newFStack(t, fopts{tm: []string{s1, s2}})
	s.op.stuck(s1, "ReadDir", "one.sparsebundle/bands", releaseW)

	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.p.RunBackground(ctx) }()
	waitFor(t, func() bool { st, _ := s.wk.get(s1); return st == 1 })
	s.wk.waitEnded(t, s2, 1)

	// Step 2.
	s.tk.next(t) <- t0
	s.wk.waitEnded(t, s2, 2)
	if st, _ := s.wk.get(s1); st != 1 {
		t.Errorf("share 1 has %d walk starts, want 1 while its walk is stuck", st)
	}

	// Step 3.
	shares := s.shares(t)
	one := bundleByName(t, shareByPath(t, shares, s1), "one")
	if one.SizeBytes != nil || one.Unavailable["sizeBytes"] != reasonNotComputed {
		t.Errorf("share 1 bundle = %+v, want sizeBytes null, not computed yet", one)
	}
	if two := bundleByName(t, shareByPath(t, shares, s2), "two"); two.SizeBytes == nil ||
		*two.SizeBytes != allocTree(t, filepath.Join(s2, "two.sparsebundle", "bands")) {
		t.Errorf("share 2 bundle = %+v, want its walked size", two)
	}

	// Step 4.
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunBackground did not return within 1s of the cancel")
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline+1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > baseline+1 {
		t.Errorf("%d goroutines after the cancel, want at most baseline %d + 1 (the stuck walk)", g, baseline)
	}

	// Step 5: a restarted loop, as after a reload.
	s.tk.drain()
	s.rec.reset()
	s.background(t)
	s.wk.waitEnded(t, s2, 3)
	s.tk.next(t) <- t0
	s.wk.waitEnded(t, s2, 4)
	if st, _ := s.wk.get(s1); st != 1 {
		t.Errorf("share 1 has %d walk starts after the restart, want still 1", st)
	}
	skipped := 0
	for _, r := range s.rec.withMessage("storage size walk skipped: the share's previous walk is still running") {
		if attrs(r)["share"] == s1 {
			skipped++
		}
	}
	if skipped != 1 {
		t.Errorf("%d debug lines naming share 1 as skipped, want 1", skipped)
	}

	// Step 6.
	once.Do(func() { close(releaseW) })
	s.wk.waitEnded(t, s1, 1)
	if bs, ok := s.p.walks.size(s1, "one.sparsebundle"); !ok || bs.reason != "" {
		t.Errorf("share 1 size after the stuck walk ends = %+v, %v, want a recorded size", bs, ok)
	}
	s.tk.next(t) <- t0
	waitFor(t, func() bool { st, _ := s.wk.get(s1); return st == 2 })
}

// ---- TC-STG-145 to TC-STG-151: backups ----

// vzOracle is TC-STG-145's expected item, computed from the listing and the
// planted files with an independent reader.
type vzOracle struct {
	file, dir, guestType, format, comp, ts string
	vmid                                   int
	mt                                     time.Time
	size                                   *int64
	inProgress                             bool
}

var (
	oracleArchiveRE = regexp.MustCompile(`^vzdump-(qemu|lxc)-([1-9][0-9]{2,8})-([0-9]{4})_([0-9]{2})_([0-9]{2})-([0-9]{2})_([0-9]{2})_([0-9]{2})\.(vma|tar)(?:\.(zst|gz|lzo))?$`)
	oracleDatRE     = regexp.MustCompile(`^vzdump-(qemu|lxc)-([1-9][0-9]{2,8})-([0-9]{4})_([0-9]{2})_([0-9]{2})-([0-9]{2})_([0-9]{2})_([0-9]{2})\.(?:(vma|tar)\.)?dat$`)
	oracleTmpRE     = regexp.MustCompile(`^vzdump-(qemu|lxc)-([1-9][0-9]{2,8})-([0-9]{4})_([0-9]{2})_([0-9]{2})-([0-9]{2})_([0-9]{2})_([0-9]{2})\.tmp$`)
)

func oracleItems(entries []storagetest.Listed, dir string) []vzOracle {
	var out []vzOracle
	for _, e := range entries {
		var m []string
		o := vzOracle{file: e.Name, dir: dir, mt: e.ModTime.UTC()}
		switch {
		case e.Type == '-' && oracleArchiveRE.MatchString(e.Name):
			m = oracleArchiveRE.FindStringSubmatch(e.Name)
			o.format, o.comp = m[9], m[10]
		case e.Type == '-' && oracleDatRE.MatchString(e.Name):
			m = oracleDatRE.FindStringSubmatch(e.Name)
			o.format, o.inProgress = m[9], true
		case e.Type == 'd' && oracleTmpRE.MatchString(e.Name):
			m = oracleTmpRE.FindStringSubmatch(e.Name)
			o.inProgress = true
		default:
			continue
		}
		o.guestType = m[1]
		o.vmid, _ = strconv.Atoi(m[2])
		o.ts = fmt.Sprintf("%s-%s-%sT%s:%s:%s", m[3], m[4], m[5], m[6], m[7], m[8])
		if e.Type == '-' {
			o.size = ptrTo(e.Size)
		}
		out = append(out, o)
	}
	return out
}

// oracleLog is TC-STG-94's rule over a log's bytes.
func oracleLog(b []byte) (string, []string) {
	lastFinish, lastErr := -1, -1
	var errs []string
	for i, line := range strings.Split(string(b), "\n") {
		text := line
		if len(text) > 20 && text[4] == '-' && text[10] == ' ' && text[19] == ' ' {
			text = text[20:]
		}
		switch {
		case strings.HasPrefix(text, "INFO: Finished Backup of VM"):
			lastFinish = i
		case strings.HasPrefix(text, "ERROR:"):
			lastErr = i
			errs = append(errs, line)
		}
	}
	switch {
	case lastErr >= 0 && lastErr > lastFinish:
		return vzdump.LogError, errs
	case lastFinish >= 0:
		return vzdump.LogOK, errs
	}
	return vzdump.LogUnknown, errs
}

// TC-STG-145: backups.list fields over a tree built from the home-server listing. Owner WI 4.3. Requirements: FR-9, NFR-4, FR-18.
func TestTCSTG145_BackupsListFieldsOverATreeBuilt(t *testing.T) {
	top, err := storagetest.ParseListing(home(t, "shares/1/ls.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dump, err := storagetest.ParseListing(home(t, "shares/2/ls.txt"))
	if err != nil {
		t.Fatal(err)
	}
	base := storagetest.TempDir(t)
	treeB, treeA := filepath.Join(base, "b"), filepath.Join(base, "a")
	nodes, err := storagetest.ListingNodes(top, "")
	if err != nil {
		t.Fatal(err)
	}
	dumpNodes, err := storagetest.ListingNodes(dump, "dump")
	if err != nil {
		t.Fatal(err)
	}
	// Notes and log of the newest archive from the captured heads; the
	// other companions are sparse at their listed sizes.
	const newest = "vzdump-lxc-103-2026_08_27-10_34_54"
	logHead := home(t, "vzdump/newest-log-head.txt")
	notes := bytes.TrimSuffix(home(t, "vzdump/newest-notes-line1.txt"), []byte("\n"))
	for i, n := range dumpNodes {
		switch n.Path {
		case "dump/" + newest + ".log":
			dumpNodes[i].Content = logHead
		case "dump/" + newest + ".tar.zst.notes":
			dumpNodes[i].Content = notes
		}
	}
	storagetest.Build(t, treeB, append(nodes, dumpNodes...)...)

	// Tree a: the same name and mtime as one of b's archives, and the same
	// mtime under another name.
	var twin storagetest.Listed
	for _, e := range dump {
		if e.Name == "vzdump-qemu-107-2026_08_27-10_28_13.vma.zst" {
			twin = e
		}
	}
	other := "vzdump-qemu-104-2026_08_27-10_28_13.vma.zst"
	aEntries := []storagetest.Listed{
		{Name: twin.Name, Type: '-', Size: 4096, ModTime: twin.ModTime},
		{Name: other, Type: '-', Size: 8192, ModTime: twin.ModTime},
	}
	storagetest.Build(t, treeA,
		storagetest.Node{Path: twin.Name, Size: 4096, ModTime: twin.ModTime},
		storagetest.Node{Path: other, Size: 8192, ModTime: twin.ModTime})

	s := newFStack(t, fopts{vzdump: []string{treeB, treeA}})

	want := append(oracleItems(dump, treeB+"/dump"), oracleItems(aEntries, treeA)...)
	want = append(want, oracleItems(top, treeB)...)
	slices.SortFunc(want, func(x, y vzOracle) int {
		if c := y.mt.Compare(x.mt); c != 0 {
			return c
		}
		if c := strings.Compare(x.file, y.file); c != 0 {
			return c
		}
		return strings.Compare(x.dir, y.dir)
	})

	l := fcall[apitypes.List[spec.Backup]](t, s, spec.BackupsList.Op(), `{"limit":500}`)
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	var decoded apitypes.List[spec.Backup]
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode into spec.BackupProperties: %v", err)
	}
	got := decoded.Items
	if len(got) != len(want) {
		t.Fatalf("%d items, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i].Properties
		if got[i].Name != w.file || g.File != w.file || g.Directory != w.dir || g.GuestType != w.guestType ||
			g.VMID != w.vmid || g.ArchiveFormat != w.format || g.Compression != w.comp || g.Timestamp != w.ts ||
			g.TimestampZone != spec.TimestampZoneNodeLocal || !g.ModifiedAt.Equal(w.mt) ||
			g.InProgress != w.inProgress || g.Protected {
			t.Errorf("item %d = %s %+v, want %+v", i, got[i].Name, g, w)
		}
		if g.ModifiedAt.Location() != time.UTC {
			t.Errorf("item %d modifiedAt %v, want UTC", i, g.ModifiedAt)
		}
		if (w.size == nil) != (g.SizeBytes == nil) || w.size != nil && *w.size != *g.SizeBytes {
			t.Errorf("item %d %s sizeBytes %v, want %v", i, w.file, g.SizeBytes, w.size)
		}
		// Notes and log as the planted files give them.
		root := w.dir
		if w.inProgress {
			if g.Notes != nil || g.LogStatus != vzdump.LogUnknown {
				t.Errorf("in-progress %s: notes %v logStatus %s, want none and unknown", w.file, g.Notes, g.LogStatus)
			}
			continue
		}
		if b, err := os.ReadFile(filepath.Join(root, w.file+".notes")); err == nil {
			wantNotes := string(b[:min(len(b), spec.NotesMaxBytes)])
			if g.Notes == nil || *g.Notes != wantNotes {
				t.Errorf("%s notes %v, want %q", w.file, g.Notes, wantNotes)
			}
		} else if g.Notes != nil {
			t.Errorf("%s notes %q, want null", w.file, *g.Notes)
		}
		logName := strings.SplitN(w.file, ".", 2)[0] + ".log"
		if b, err := os.ReadFile(filepath.Join(root, logName)); err == nil {
			st, errs := oracleLog(b)
			if g.LogStatus != st || !slices.Equal([]string(g.LogErrors), append([]string{}, errs...)) &&
				len(errs)+len(g.LogErrors) > 0 {
				t.Errorf("%s log %s %q, want %s %q", w.file, g.LogStatus, g.LogErrors, st, errs)
			}
		} else if g.LogStatus != vzdump.LogUnknown {
			t.Errorf("%s logStatus %s with no log, want unknown", w.file, g.LogStatus)
		}
	}
	// The captured newest archive's log and notes.
	for _, it := range got {
		if it.Name == newest+".tar.zst" {
			if it.Properties.LogStatus != vzdump.LogOK || it.Properties.Notes == nil || *it.Properties.Notes != string(notes) {
				t.Errorf("newest archive %+v, want ok and notes %q", it.Properties, notes)
			}
		}
	}
	// The same-named pair: a before b; the same-mtime pair in file order.
	ia := slices.IndexFunc(got, func(b spec.Backup) bool { return b.Name == twin.Name && b.Properties.Directory == treeA })
	ib := slices.IndexFunc(got, func(b spec.Backup) bool { return b.Name == twin.Name && b.Properties.Directory == treeB+"/dump" })
	io := slices.IndexFunc(got, func(b spec.Backup) bool { return b.Name == other })
	if !(io < ia && ia+1 == ib) {
		t.Errorf("order: %s at %d, a's twin at %d, b's twin at %d; want other, a, b adjacent", other, io, ia, ib)
	}

	// Step 2: a page that ends between the twins.
	var pages []spec.Backup
	cursor := ""
	for range 100 {
		in := fmt.Sprintf(`{"limit":%d}`, ia+1)
		if cursor != "" {
			in = fmt.Sprintf(`{"limit":%d,"cursor":%q}`, ia+1, cursor)
		}
		l := fcall[apitypes.List[spec.Backup]](t, s, spec.BackupsList.Op(), in)
		if len(pages) == 0 && l.Items[len(l.Items)-1].Properties.Directory != treeA {
			t.Errorf("the first page does not end on a's twin")
		}
		pages = append(pages, l.Items...)
		if cursor = l.NextCursor; cursor == "" {
			break
		}
	}
	if len(pages) != len(got) {
		t.Fatalf("pages hold %d items, want %d", len(pages), len(got))
	}
	for i := range pages {
		if pages[i].Name != got[i].Name || pages[i].Properties.Directory != got[i].Properties.Directory {
			t.Errorf("page item %d = %s in %s, want %s in %s", i, pages[i].Name, pages[i].Properties.Directory,
				got[i].Name, got[i].Properties.Directory)
		}
	}
}

// TC-STG-146: backups.list filters and their validation. Owner WI 4.3. Requirements: FR-9, NFR-5.
func TestTCSTG146_BackupsListFiltersAndTheirValidation(t *testing.T) {
	var arch []vz
	for i, g := range []struct {
		typ  string
		vmid int
	}{{spec.GuestQemu, 101}, {spec.GuestLXC, 103}, {spec.GuestLXC, 106}} {
		for d := range 4 {
			at := t0.Add(-time.Duration(d)*24*time.Hour - time.Duration(i)*time.Hour - time.Hour)
			arch = append(arch, vz{typ: g.typ, vmid: g.vmid, stamp: at, mtime: at, size: 10, log: okLog(g.vmid)})
		}
	}
	s := newFStack(t, fopts{vzdump: []string{vzTree(t, arch...)}})
	since := t0.Add(-48 * time.Hour)
	check := func(input string, keep func(vz) bool) {
		t.Helper()
		var want []string
		for _, a := range arch {
			if keep(a) {
				want = append(want, a.file())
			}
		}
		var got []string
		for _, b := range s.backups(t, input) {
			got = append(got, b.Name)
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", input, got, want)
		}
	}
	check(`{"vmid":103}`, func(a vz) bool { return a.vmid == 103 })
	check(`{"guestType":"qemu"}`, func(a vz) bool { return a.typ == spec.GuestQemu })
	check(fmt.Sprintf(`{"since":%q}`, since.Format(time.RFC3339)), func(a vz) bool { return !a.mtime.Before(since) })
	for input, field := range map[string]string{`{"vmid":99}`: "vmid", `{"guestType":"openvz"}`: "guestType",
		`{"since":"2d"}`: "since"} {
		_, err := s.invoke(spec.BackupsList.Op(), input)
		wantInvalid(t, err, field)
	}
	// The handler refuses them again for a direct caller.
	ctx := context.Background()
	_, err := s.p.backupsList(ctx, spec.BackupListInput{VMID: ptrTo(99)})
	wantInvalid(t, err, "vmid")
	_, err = s.p.backupsList(ctx, spec.BackupListInput{GuestType: "openvz"})
	wantInvalid(t, err, "guestType")
	_, err = s.p.backupsList(ctx, spec.BackupListInput{Since: "2d"})
	wantInvalid(t, err, "since")
}

// TC-STG-147: notes and logs are read only for the returned page. Owner WI 4.3. Requirements: FR-9, NFR-1.
func TestTCSTG147_NotesAndLogsAreReadOnlyFor(t *testing.T) {
	var arch []vz
	for i := range 30 {
		at := t0.Add(-time.Duration(i+1) * time.Hour)
		arch = append(arch, vz{typ: spec.GuestQemu, vmid: 200 + i, stamp: at, mtime: at, size: 10,
			notes: []byte("n"), log: okLog(200 + i)})
	}
	dir := vzTree(t, arch...)
	s := newFStack(t, fopts{vzdump: []string{dir}})
	items := s.backups(t, `{"limit":5}`)
	if len(items) != 5 {
		t.Fatalf("%d items, want 5", len(items))
	}
	var wantNotes, wantLogs []string
	for _, a := range arch[:5] {
		wantNotes, wantLogs = append(wantNotes, a.file()+".notes"), append(wantLogs, a.logName())
	}
	gotNotes, gotLogs := s.op.names("Open", suffix(".notes")), s.op.names("Open", suffix(".log"))
	slices.Sort(gotNotes)
	slices.Sort(gotLogs)
	slices.Sort(wantNotes)
	slices.Sort(wantLogs)
	if !slices.Equal(gotNotes, wantNotes) || !slices.Equal(gotLogs, wantLogs) {
		t.Errorf("opened notes %v and logs %v, want exactly those of the page: %v %v", gotNotes, gotLogs, wantNotes, wantLogs)
	}
}

// summaryTree is TC-STG-148's tree: name timestamps 7 h off the mtimes.
func summaryTree(log106 []byte) []vz {
	off := 7 * time.Hour
	mk := func(typ string, vmid int, mt time.Time, notes, log []byte) vz {
		return vz{typ: typ, vmid: vmid, stamp: mt.Add(off), mtime: mt, size: int64(vmid) * 1000, notes: notes, log: log}
	}
	return []vz{
		mk(spec.GuestQemu, 101, t0.Add(-24*time.Hour), []byte("home-server\nsecond line\n"), okLog(101)),
		mk(spec.GuestQemu, 101, t0.Add(-8*24*time.Hour), []byte("old-name\n"), okLog(101)),
		mk(spec.GuestLXC, 103, t0.Add(-192*time.Hour), []byte("guest-103"), okLog(103)),
		mk(spec.GuestLXC, 106, t0.Add(-192*time.Hour-time.Second), []byte("guest-106"), log106),
	}
}

// TC-STG-148: backups.summary groups by guest and computes age and staleness from the mtime. Owner WI 4.3. Requirements: FR-10, ASM-5.
func TestTCSTG148_BackupsSummaryGroupsByGuestAndComputes(t *testing.T) {
	arch := summaryTree(okLog(106))
	s := newFStack(t, fopts{vzdump: []string{vzTree(t, arch...)}, mod: func(c *config.StorageConfig) {
		c.Backups.MaxAge = 192 * time.Hour
	}})
	sum := s.summary(t)
	if sum.MaxAgeHours != 192 || sum.GuestCount != 3 || len(sum.Guests) != 3 {
		t.Fatalf("summary %+v, want maxAgeHours 192 and 3 guests", sum)
	}
	for _, c := range []struct {
		typ    string
		vmid   int
		latest vz
		all    []vz
		stale  bool
		name   string
	}{
		{spec.GuestQemu, 101, arch[0], arch[:2], false, "home-server"},
		{spec.GuestLXC, 103, arch[2], arch[2:3], false, "guest-103"},
		{spec.GuestLXC, 106, arch[3], arch[3:4], true, "guest-106"},
	} {
		g := guestOf(t, sum, c.typ, c.vmid)
		var total int64
		for _, a := range c.all {
			total += a.size
		}
		age := t0.Sub(c.latest.mtime).Hours()
		if g.LatestAt == nil || !g.LatestAt.Equal(c.latest.mtime) || g.LatestTimestamp == nil ||
			*g.LatestTimestamp != c.latest.stamp.Format("2006-01-02T15:04:05") || g.LatestSizeBytes == nil ||
			*g.LatestSizeBytes != c.latest.size || g.LatestLogStatus != vzdump.LogOK || g.AgeHours == nil ||
			*g.AgeHours != age || g.Count != len(c.all) || g.TotalBytes != total || g.Stale != c.stale ||
			g.GuestName == nil || *g.GuestName != c.name || g.LatestFile == nil || *g.LatestFile != c.latest.file() {
			t.Errorf("guest %s/%d = %+v, want latest %s age %v stale %v name %q", c.typ, c.vmid, g, c.latest.file(),
				age, c.stale, c.name)
		}
	}
	if sum.Guests[0].GuestType != spec.GuestLXC || sum.Guests[2].GuestType != spec.GuestQemu {
		t.Errorf("guests not sorted by guestType then vmid: %+v", sum.Guests)
	}
}

// TC-STG-149: an expected guest with no archive appears stale, and counters add up. Owner WI 4.3. Requirements: FR-10.
func TestTCSTG149_AnExpectedGuestWithNoArchiveAppears(t *testing.T) {
	arch := summaryTree(errLog(106))
	s := newFStack(t, fopts{vzdump: []string{vzTree(t, arch...)}, mod: func(c *config.StorageConfig) {
		c.Backups.MaxAge = 192 * time.Hour
		c.Backups.ExpectedGuests = []int{101, 999}
	}})
	sum := s.summary(t)
	g := guestOf(t, sum, "", 999)
	if g.LatestAt != nil || !g.Stale || !g.Expected || g.Count != 0 || g.GuestType != "" {
		t.Errorf("guest 999 = %+v, want latestAt null, stale, expected, count 0, guestType empty", g)
	}
	if g := guestOf(t, sum, spec.GuestQemu, 101); !g.Expected {
		t.Errorf("101 = %+v, want expected", g)
	}
	if g := guestOf(t, sum, spec.GuestLXC, 106); g.LatestLogStatus != vzdump.LogError {
		t.Errorf("106 latestLogStatus %s, want error", g.LatestLogStatus)
	}
	if sum.GuestCount != 4 || sum.StaleCount != 2 || sum.FailedCount != 1 {
		t.Errorf("counts %d/%d/%d, want guestCount 4, staleCount 2, failedCount 1", sum.GuestCount, sum.StaleCount,
			sum.FailedCount)
	}
}

// TC-STG-150: one configured path failing is reported in directories while the other serves. Owner WI 4.3. Requirements: FR-9, NFR-7, CON-7.
func TestTCSTG150_OneConfiguredPathFailingIsReportedIn(t *testing.T) {
	_, file, _ := dirs(t)
	tree := vzTree(t, three(101)...)
	storagetest.Build(t, tree, storagetest.Node{Path: "README.txt", Content: []byte("x")},
		storagetest.Node{Path: "vzdump-openvz-101-2026_01_01-00_00_00.tar", Size: 1})
	s := newFStack(t, fopts{vzdump: []string{file, tree}})
	sum := s.summary(t)
	if d := dirOf(t, sum, file); d.Available || d.Reason == "" {
		t.Errorf("file = %+v, want unavailable with a reason", d)
	}
	if d := dirOf(t, sum, tree); !d.Available || d.Archives != 3 || d.UnrecognizedFiles != 2 || d.Truncated {
		t.Errorf("tree = %+v, want available, 3 archives, 2 unrecognized, not truncated", d)
	}
	if items := s.backups(t, "{}"); len(items) != 3 {
		t.Errorf("list: %d items, want 3", len(items))
	}
}

// TC-STG-151: finished logs are read once, and a changed log is read again. Owner WI 4.3. Requirements: FR-9, NFR-1.
func TestTCSTG151_FinishedLogsAreReadOnceAndA(t *testing.T) {
	var arch []vz
	for i := range 5 {
		at := t0.Add(-time.Duration(i+1) * time.Hour)
		arch = append(arch, vz{typ: spec.GuestQemu, vmid: 300 + i, stamp: at, mtime: at, size: 1, log: okLog(300 + i)})
	}
	dir := vzTree(t, arch...)
	s := newFStack(t, fopts{vzdump: []string{dir}})
	logs := func() int { return s.op.count(dir, "Open", suffix(".log")) }
	s.summary(t)
	first := logs()
	if first != 5 {
		t.Errorf("first summary opened %d logs, want 5", first)
	}
	s.summary(t)
	if n := logs(); n != first {
		t.Errorf("second summary opened %d logs, want none", n-first)
	}
	target := arch[2]
	f, err := os.OpenFile(filepath.Join(dir, target.logName()), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("2026-11-17 03:05:00 ERROR: something failed\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, target.logName()), t0, t0); err != nil {
		t.Fatal(err)
	}
	sum := s.summary(t)
	if n := s.op.count(dir, "Open", is(target.logName())); n != 2 || logs() != first+1 {
		t.Errorf("after the change: %d new log opens (%d of the changed log), want exactly that one", logs()-first, n)
	}
	if g := guestOf(t, sum, spec.GuestQemu, target.vmid); g.LatestLogStatus != vzdump.LogError || sum.FailedCount != 1 {
		t.Errorf("guest %d %s, failedCount %d, want failed", target.vmid, g.LatestLogStatus, sum.FailedCount)
	}

	// The memo holds at most 10,000 entries.
	var m logMemo
	for i := range maxLogMemo + 1 {
		m.put(logKey{dir: "/d", file: strconv.Itoa(i)}, vzdump.LogVerdict{Status: vzdump.LogOK})
	}
	if n := m.len(); n != maxLogMemo {
		t.Errorf("memo holds %d entries, want %d", n, maxLogMemo)
	}
	if _, ok := m.get(logKey{dir: "/d", file: "0"}); ok {
		t.Error("the oldest entry was kept past the bound")
	}
	if _, ok := m.get(logKey{dir: "/d", file: strconv.Itoa(maxLogMemo)}); !ok {
		t.Error("the newest entry is missing")
	}
}

// ---- TC-STG-152 to TC-STG-159: Time Machine and the Background walk ----

// dfUsage is F-STATFS's numbers from a captured `df -B1`: 1B-blocks,
// used and available, with bsize 1.
func dfUsage(t *testing.T, name string) (Usage, int) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(home(t, name))), "\n")
	f := strings.Fields(lines[1])
	blocks, used, avail := atoi(t, f[1]), atoi(t, f[2]), atoi(t, f[3])
	pct, err := strconv.Atoi(strings.TrimSuffix(f[4], "%"))
	if err != nil {
		t.Fatal(err)
	}
	return Usage{Blocks: blocks, Bfree: blocks - used, Bavail: avail, Bsize: 1}, pct
}

// TC-STG-152: timemachine.list share usage from statfs and the matching dataset's quota. Owner WI 4.3. Requirements: FR-11, NFR-4.
func TestTCSTG152_TimemachineListShareUsageFromStatfsAnd(t *testing.T) {
	base := storagetest.TempDir(t)
	tm1, tm2 := filepath.Join(base, "tm1"), filepath.Join(base, "tm2")
	storagetest.Build(t, base, storagetest.Node{Path: "tm1", Type: storagetest.Dir},
		storagetest.Node{Path: "tm2", Type: storagetest.Dir})
	u1, pct1 := dfUsage(t, "shares/3/df.txt")
	u2, pct2 := dfUsage(t, "shares/4/df.txt")

	rewrite := func(b []byte) []byte {
		b = bytes.ReplaceAll(b, []byte("/media/timemachine2"), []byte(tm2))
		return bytes.ReplaceAll(b, []byte("/media/timemachine"), []byte(tm1))
	}
	fake := storagetest.NewFake(t, "home-server")
	ov := newOverlay(fake)
	form := "zfs/zfs-list-datasets.txt"
	if fake.Format() == zfs.JSON {
		form = "zfs/zfs-list-datasets.json"
	}
	ov.answer(fake.Argv(zfs.CmdDatasetList), zfs.Result{Stdout: rewrite(home(t, form))})
	s := newFStack(t, fopts{tm: []string{tm1, tm2}, fsys: homeFS(t), run: ov.run})
	s.sf.set(tm1, u1)
	s.sf.set(tm2, u2)

	quota := map[string][2]uint64{}
	for _, r := range tabRows(home(t, "zfs/zfs-list-datasets.txt")) {
		if strings.HasSuffix(r[0], "/timemachine") {
			quota[r[0]] = [2]uint64{atoi(t, r[2]), atoi(t, r[6])}
		}
	}
	checkUsage := func(sp spec.ShareProperties, u Usage) {
		t.Helper()
		used := u.Blocks - u.Bfree
		want := float64(used) / float64(used+u.Bavail) * 100
		if sp.SizeBytes == nil || *sp.SizeBytes != u.Blocks*u.Bsize || sp.UsedBytes == nil ||
			*sp.UsedBytes != used*u.Bsize || sp.AvailableBytes == nil || *sp.AvailableBytes != u.Bavail*u.Bsize ||
			sp.UsedPercent == nil || *sp.UsedPercent != want {
			t.Errorf("share %s usage %v %v %v %v, want df's figures from %+v", sp.Path, sp.SizeBytes, sp.UsedBytes,
				sp.AvailableBytes, sp.UsedPercent, u)
		}
	}

	shares := s.shares(t)
	if len(shares) != 2 {
		t.Fatalf("%d shares, want 2", len(shares))
	}
	for _, c := range []struct {
		path, dataset string
		u             Usage
		pct           int
	}{{tm1, "z3/ds/timemachine", u1, pct1}, {tm2, "z2/d1/timemachine", u2, pct2}} {
		sp := shareByPath(t, shares, c.path)
		i := slices.IndexFunc(shares, func(x spec.Share) bool { return x.Properties.Path == c.path })
		if shares[i].Name != url.PathEscape(c.path) {
			t.Errorf("name %q, want the path-escaped path", shares[i].Name)
		}
		checkUsage(sp, c.u)
		if int(math.Ceil(*sp.UsedPercent)) != c.pct {
			t.Errorf("%s ceil(usedPercent) %v, want df's Use%% %d", c.path, *sp.UsedPercent, c.pct)
		}
		q := quota[c.dataset]
		if sp.Dataset == nil || *sp.Dataset != c.dataset || sp.QuotaBytes == nil || *sp.QuotaBytes != q[1] ||
			sp.QuotaUsedPercent == nil || *sp.QuotaUsedPercent != float64(q[0])/float64(q[1])*100 {
			t.Errorf("%s dataset %v quota %v %v, want %s with quota %d", c.path, sp.Dataset, sp.QuotaBytes,
				sp.QuotaUsedPercent, c.dataset, q[1])
		}
		if n := s.sf.n(c.path); n != 1 {
			t.Errorf("%d statfs calls for %s, want 1 per request", n, c.path)
		}
	}
	for _, argv := range ov.argvs() {
		if strings.Contains(strings.Join(argv, " "), base) {
			t.Errorf("argv %q mentions a share: statfs must not go through zfs.Tools", argv)
		}
	}

	// zfs unavailable: the statfs fields stay.
	z := newFStack(t, fopts{tm: []string{tm1, tm2}})
	z.sf.set(tm1, u1)
	sp := shareByPath(t, z.shares(t), tm1)
	checkUsage(sp, u1)
	if sp.Dataset != nil || sp.QuotaBytes != nil || sp.QuotaUsedPercent != nil {
		t.Errorf("zfs unavailable: dataset %v quota %v, want null", sp.Dataset, sp.QuotaBytes)
	}
	for _, f := range []string{"dataset", "quotaBytes", "quotaUsedPercent"} {
		wantReason(t, sp.Unavailable, f, "zfs")
	}

	// Step 2: ext4-like, 5% reserved blocks (bfree > bavail).
	ext4 := Usage{Blocks: 1_000_000, Bfree: 300_000, Bavail: 250_000, Bsize: 4096}
	s.sf.set(tm1, ext4)
	sp = shareByPath(t, s.shares(t), tm1)
	checkUsage(sp, ext4)
	older := float64(ext4.Blocks-ext4.Bavail) / float64(ext4.Blocks) * 100
	if *sp.UsedPercent == older {
		t.Errorf("usedPercent %v equals the (blocks − bavail)/blocks formula", *sp.UsedPercent)
	}
	// df's Use%: used × 100 / (used + avail), rounded up, which the captured
	// df -B1 lines of shares/3 and shares/4 agree with (checked above).
	used := ext4.Blocks - ext4.Bfree
	dfPct := (used*100 + used + ext4.Bavail - 1) / (used + ext4.Bavail)
	if uint64(math.Ceil(*sp.UsedPercent)) != dfPct {
		t.Errorf("ceil(usedPercent) %v, want df's Use%% %d", math.Ceil(*sp.UsedPercent), dfPct)
	}
}

// TC-STG-153: bundle sizes are "not computed yet", then cached with computedAt, then recomputed on the next tick. Owner WI 4.3. Requirements: FR-12, NFR-1.
func TestTCSTG153_BundleSizesAreNotComputedYetThen(t *testing.T) {
	share := bundleTree(t, 4, map[string]time.Time{"mac": t0.Add(-time.Hour)})
	bands := filepath.Join(share, "mac.sparsebundle", "bands")
	s := newFStack(t, fopts{tm: []string{share}})
	b := bundleByName(t, shareByPath(t, s.shares(t), share), "mac")
	if b.SizeBytes != nil || b.Unavailable["sizeBytes"] != "not computed yet" {
		t.Errorf("before the walk: %+v, want sizeBytes null, not computed yet", b)
	}

	s.background(t)
	s.wk.waitEnded(t, share, 1)
	b = bundleByName(t, shareByPath(t, s.shares(t), share), "mac")
	s1 := allocTree(t, bands)
	if b.SizeBytes == nil || *b.SizeBytes != s1 || b.SizeComputedAt == nil || b.SizeTruncated {
		t.Fatalf("state A: %+v, want sizeBytes %d with sizeComputedAt", b, s1)
	}
	first := *b.SizeComputedAt

	storagetest.Build(t, share, storagetest.Node{Path: "mac.sparsebundle/bands/ff", Content: bytes.Repeat([]byte("x"), 70000)})
	s2 := allocTree(t, bands)
	s.tk.next(t) <- t0
	s.wk.waitEnded(t, share, 2)
	b = bundleByName(t, shareByPath(t, s.shares(t), share), "mac")
	if b.SizeBytes == nil || *b.SizeBytes != s2 || b.SizeComputedAt == nil || !b.SizeComputedAt.After(first) {
		t.Errorf("state A': %+v, want sizeBytes %d and a later sizeComputedAt than %v", b, s2, first)
	}
}

// TC-STG-154: a walk that hits its bound reports sizeTruncated. Owner WI 4.3. Requirements: FR-12, NFR-3.
func TestTCSTG154_AWalkThatHitsItsBoundReports(t *testing.T) {
	share := bundleTree(t, 150, map[string]time.Time{"mac": t0.Add(-time.Hour)})
	bands := filepath.Join(share, "mac.sparsebundle", "bands")
	s := newFStack(t, fopts{tm: []string{share}})
	s.p.walkEntries = 100
	_, cancel, done := s.background(t)
	s.wk.waitEnded(t, share, 1)
	cancel()
	<-done
	des, err := os.ReadDir(bands) // sorted by name, as fs.WalkDir visits
	if err != nil {
		t.Fatal(err)
	}
	want := alloc(t, bands)
	for _, de := range des[:100] {
		want += alloc(t, filepath.Join(bands, de.Name()))
	}
	b := bundleByName(t, shareByPath(t, s.shares(t), share), "mac")
	if !b.SizeTruncated || b.SizeBytes == nil || *b.SizeBytes != want {
		t.Errorf("bundle %+v, want sizeTruncated and the partial sum %d of 100 entries", b, want)
	}
}

// TC-STG-155: no request starts a walk. Owner WI 4.3. Requirements: FR-12, NFR-1.
func TestTCSTG155_NoRequestStartsAWalk(t *testing.T) {
	share := bundleTree(t, 2, map[string]time.Time{"mac": t0.Add(-time.Hour)})
	s := newFStack(t, fopts{tm: []string{share}})
	for range 50 {
		s.shares(t)
	}
	for range 10 {
		healthOf(t, s.d)
	}
	if n := s.wk.total(); n != 0 {
		t.Errorf("%d walks after 50 timemachine.list and 10 storage.health.get, want 0", n)
	}
}

// TC-STG-156: RunBackground walks at start and per sizeInterval, recovers per bundle, and stops promptly. Owner WI 4.3. Requirements: FR-12, NFR-7.
func TestTCSTG156_RunBackgroundWalksAtStartAndPerSizeInterval(t *testing.T) {
	sh1 := bundleTree(t, 2, map[string]time.Time{"boom": t0, "fine": t0})
	sh2 := bundleTree(t, 2, map[string]time.Time{"other": t0})
	paths := []string{sh1, sh2}
	s := newFStack(t, fopts{tm: paths, mod: func(c *config.StorageConfig) { c.TimeMachine.SizeInterval = time.Hour }})
	s.op.set(sh1, "Lstat", "boom.sparsebundle/bands", fRule{kind: rulePanic})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.p.RunBackground(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	s.wk.waitEnded(t, sh1, 1)
	s.wk.waitEnded(t, sh2, 1)
	if bs, ok := s.p.walks.size(sh2, "other.sparsebundle"); !ok || bs.reason != "" {
		t.Errorf("share 2 bundle = %+v, %v, want a size", bs, ok)
	}
	if bs, ok := s.p.walks.size(sh1, "fine.sparsebundle"); !ok || bs.reason != "" {
		t.Errorf("share 1's other bundle = %+v, %v, want a size", bs, ok)
	}
	boom := bundleByName(t, shareByPath(t, s.shares(t), sh1), "boom")
	if boom.SizeBytes != nil {
		t.Errorf("panicking bundle = %+v, want no size", boom)
	}
	warned := false
	for _, r := range s.rec.all() {
		if r.Level == slog.LevelWarn && attrs(r)["bundle"] == "boom.sparsebundle" {
			warned = true
		}
	}
	if !warned {
		t.Error("no warn record names the panicking bundle")
	}

	// Step 2.
	configure(t, s.p, func(c *config.StorageConfig) {
		c.TimeMachine.Paths, c.TimeMachine.SizeInterval = paths, 2*time.Hour
	})
	c := s.tk.next(t)
	if d := s.tk.last(); d != time.Hour {
		t.Errorf("first wait %s, want 1h", d)
	}
	c <- t0
	s.wk.waitEnded(t, sh2, 2)
	c = s.tk.next(t)
	if d := s.tk.last(); d != 2*time.Hour {
		t.Errorf("the loop requested the next tick at %s, want 2h", d)
	}

	// Step 3: cancel during a walk blocked in the opener (a block that
	// honours the ctx).
	gate := make(chan struct{})
	defer close(gate)
	blocked := "fine.sparsebundle/bands"
	before := s.op.count(sh1, "ReadDir", is(blocked))
	s.op.set(sh1, "ReadDir", blocked, fRule{kind: ruleBlock, gate: gate, ctx: ctx})
	c <- t0
	waitFor(t, func() bool { return s.op.count(sh1, "ReadDir", is(blocked)) > before })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunBackground did not return within 1s of the cancel")
	}
	s.wk.waitEnded(t, sh1, 3)

	// Step 4: no Time Machine path.
	configure(t, s.p, func(c *config.StorageConfig) { c.TimeMachine.Paths = nil })
	s.tk.drain()
	before = s.wk.total()
	ctx4, cancel4 := context.WithCancel(context.Background())
	done4 := make(chan struct{})
	go func() { defer close(done4); s.p.RunBackground(ctx4) }()
	s.tk.next(t)
	if n := s.wk.total(); n != before {
		t.Errorf("%d walks with no Time Machine path, want none", n-before)
	}
	cancel4()
	select {
	case <-done4:
	case <-time.After(time.Second):
		t.Fatal("RunBackground with no path did not return on ctx done")
	}
}

// TC-STG-157: bundles report staleness from the plist date or, failing that, the best-effort mtime. Owner WI 4.3. Requirements: FR-11.
func TestTCSTG157_BundlesReportStalenessFromThePlistDate(t *testing.T) {
	share := storagetest.TempDir(t)
	old := t0.Add(-10 * 24 * time.Hour)
	plist := func(dir string, content []byte, mt time.Time) storagetest.Node {
		return storagetest.Node{Path: dir + "/" + timemachine.SnapshotHistoryFile, Content: content, ModTime: mt}
	}
	at := func(dir string, nodes []storagetest.Node) []storagetest.Node {
		for i := range nodes {
			if nodes[i].ModTime.IsZero() {
				nodes[i].ModTime = old
			}
		}
		return nodes
	}
	var nodes []storagetest.Node
	nodes = append(nodes, at("b1", append(storagetest.BundleNodes("b1.sparsebundle"),
		plist("b1.sparsebundle", storagetest.SnapshotHistoryXML(t0.Add(-6*24*time.Hour)), old)))...)
	nodes = append(nodes, at("b2", append(storagetest.BundleNodes("b2.sparsebundle"),
		plist("b2.sparsebundle", storagetest.SnapshotHistoryXML(t0.Add(-8*24*time.Hour)), old)))...)
	nodes = append(nodes, at("b3", append(storagetest.BundleNodes("b3.sparsebundle"),
		plist("b3.sparsebundle", []byte(storagetest.BinaryPlistMagic+"\x00\x01"), t0.Add(-9*24*time.Hour)),
		storagetest.Node{Path: "b3.sparsebundle/com.apple.TimeMachine.Results.plist", Content: []byte("r"),
			ModTime: t0.Add(-8 * 24 * time.Hour)}))...)
	nodes = append(nodes, storagetest.Node{Path: "b4.sparsebundle", Type: storagetest.Dir},
		storagetest.Node{Path: "b4.sparsebundle/bands", Type: storagetest.Dir},
		storagetest.Node{Path: "b4.sparsebundle/lock", Content: []byte{}},
		storagetest.Node{Path: "b4.sparsebundle/token", Content: []byte{}})
	storagetest.Build(t, share, nodes...)
	s := newFStack(t, fopts{tm: []string{share}, mod: func(c *config.StorageConfig) {
		c.TimeMachine.MaxAge = 168 * time.Hour
	}})
	sp := shareByPath(t, s.shares(t), share)

	b1, b2, b3, b4 := bundleByName(t, sp, "b1"), bundleByName(t, sp, "b2"), bundleByName(t, sp, "b3"),
		bundleByName(t, sp, "b4")
	if b1.Stale || b1.LastBackupSource != sourcePlist || b1.LastBackupAt == nil ||
		!b1.LastBackupAt.Equal(t0.Add(-6*24*time.Hour)) {
		t.Errorf("bundle 1 = %+v, want fresh from the plist", b1)
	}
	if !b2.Stale || b2.LastBackupSource != sourcePlist {
		t.Errorf("bundle 2 = %+v, want stale from the plist", b2)
	}
	if b3.LastBackupAt != nil || b3.LastModifiedAt == nil || !b3.LastModifiedAt.Equal(t0.Add(-8*24*time.Hour)) ||
		b3.LastBackupSource != sourceMtime || !b3.Stale {
		t.Errorf("bundle 3 = %+v, want lastModifiedAt T0−8d, mtime-best-effort, stale", b3)
	}
	wantReason(t, b3.Unavailable, "lastBackupAt", timemachine.ReasonBinaryPlist)
	if !b4.Stale || b4.LastBackupSource != "" || b4.LastBackupAt != nil || b4.LastModifiedAt != nil {
		t.Errorf("bundle 4 = %+v, want stale with no date", b4)
	}
	wantReason(t, b4.Unavailable, "lastBackupAt", timemachine.ReasonNoHistory)
	wantReason(t, b4.Unavailable, "lastModifiedAt", "missing")
	for _, b := range sp.Bundles {
		if b.InProgress != nil || b.Unavailable["inProgress"] != timemachine.ReasonInProgress {
			t.Errorf("bundle %s inProgress %v %q, want null with %q", b.Name, b.InProgress, b.Unavailable["inProgress"],
				timemachine.ReasonInProgress)
		}
	}
	if sp.BundleCount != 4 || sp.NewestBackupAt == nil || !sp.NewestBackupAt.Equal(t0.Add(-6*24*time.Hour)) {
		t.Errorf("share bundleCount %d newestBackupAt %v, want 4 and T0−6d", sp.BundleCount, sp.NewestBackupAt)
	}
}

// TC-STG-158: one Time Machine share failing does not fail the other. Owner WI 4.3. Requirements: FR-11, NFR-7.
func TestTCSTG158_OneTimeMachineShareFailingDoesNot(t *testing.T) {
	_, file, _ := dirs(t)
	tree := bundleTree(t, 1, map[string]time.Time{"mac": t0.Add(-time.Hour)})
	s := newFStack(t, fopts{tm: []string{file, tree}})
	s.sf.mu.Lock()
	delete(s.sf.usage, file)
	s.sf.errs[file] = &fs.PathError{Op: "statfs", Path: file, Err: syscall.ENOTDIR}
	s.sf.mu.Unlock()
	shares := s.shares(t)
	if len(shares) != 2 {
		t.Fatalf("%d shares, want 2", len(shares))
	}
	f := shareByPath(t, shares, file)
	if f.SizeBytes != nil || f.UsedPercent != nil || f.Bundles == nil || len(f.Bundles) != 0 {
		t.Errorf("file share = %+v, want its fields null and bundles []", f)
	}
	for _, k := range []string{"sizeBytes", "usedBytes", "availableBytes", "usedPercent", "bundles"} {
		wantReason(t, f.Unavailable, k, file)
	}
	if raw, _ := json.Marshal(f); !bytes.Contains(raw, []byte(`"bundles":[]`)) {
		t.Errorf("file share marshals %s, want bundles []", raw)
	}
	tr := shareByPath(t, shares, tree)
	if tr.UsedPercent == nil || len(tr.Bundles) != 1 || tr.Bundles[0].LastBackupAt == nil {
		t.Errorf("tree share = %+v, want complete", tr)
	}
}

// TC-STG-159: no response carries MachineID values, band contents or files outside the named set. Owner WI 4.3. Requirements: NFR-5, FR-11.
func TestTCSTG159_NoResponseCarriesMachineIDValuesBandContents(t *testing.T) {
	markers := map[string]string{"machineid": "MARKER-MACHINEID-7f3a", "band": "MARKER-BAND-91c2",
		"info": "MARKER-INFO-55d0", "results": "MARKER-RESULTS-0b8e", "other": "MARKER-OTHER-e41f",
		"notes": "MARKER-NOTES-2ndKiB-c9a1"}
	arch := three(101)
	arch[0].notes = append(bytes.Repeat([]byte("n"), spec.NotesMaxBytes), []byte(markers["notes"])...)
	vzdir := vzTree(t, arch...)
	storagetest.Build(t, vzdir, storagetest.Node{Path: "notes-copy.txt", Content: []byte(markers["other"])})

	share := storagetest.TempDir(t)
	d := "mac.sparsebundle"
	nodes := storagetest.BundleNodes(d)
	for i := range nodes {
		if nodes[i].Path == d+"/Info.plist" {
			nodes[i].Content = []byte("<plist><string>" + markers["info"] + "</string></plist>")
		}
	}
	nodes = append(nodes,
		storagetest.Node{Path: d + "/com.apple.TimeMachine.MachineID.plist", Content: []byte(markers["machineid"])},
		storagetest.Node{Path: d + "/com.apple.TimeMachine.Results.plist", Content: []byte(markers["results"])},
		storagetest.Node{Path: d + "/" + timemachine.SnapshotHistoryFile, Content: storagetest.SnapshotHistoryXML(t0)},
		storagetest.Node{Path: d + "/bands/0", Content: []byte(markers["band"])})
	storagetest.Build(t, share, nodes...)

	s := newFStack(t, fopts{vzdump: []string{vzdir}, tm: []string{share}})
	_, cancel, done := s.background(t)
	s.wk.waitEnded(t, share, 1)
	cancel()
	<-done
	var bodies [][]byte
	for _, c := range []struct {
		op    def.Op
		input string
	}{{spec.BackupsList.Op(), "{}"}, {spec.BackupsSummary.Op(), "{}"}, {spec.TimeMachineList.Op(), "{}"},
		{spec.HealthGet.Op(), "{}"}} {
		body, err := s.invoke(c.op, c.input)
		if err != nil {
			t.Fatalf("%s: %v", c.op.ID, err)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, raw)
	}
	for _, raw := range bodies {
		for what, m := range markers {
			if bytes.Contains(raw, []byte(m)) {
				t.Errorf("a body carries the %s marker: %s", what, raw)
			}
		}
	}
	for _, b := range s.backups(t, "{}") {
		if b.Properties.Notes != nil && len(*b.Properties.Notes) > spec.NotesMaxBytes {
			t.Errorf("notes of %s are %d bytes, want at most 1 KiB", b.Name, len(*b.Properties.Notes))
		}
	}
	for _, name := range s.op.names("Open", func(string) bool { return true }) {
		if strings.Contains(name, "MachineID") || strings.Contains(name, "bands/") || name == "notes-copy.txt" {
			t.Errorf("opened %s", name)
		}
	}
}

// TC-STG-E3: the vzdump timestamp is returned verbatim whatever the process time zone. Owner WI 4.3. Requirements: FR-9, ASM-5.
func TestTCSTGE3_TheVzdumpTimestampIsReturnedVerbatimWhatever(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no time zone data: %v", err)
	}
	stamp := time.Date(2026, 3, 8, 2, 30, 0, 0, time.UTC)
	mt := time.Date(2026, 3, 8, 7, 45, 0, 0, time.UTC)
	dir := vzTree(t, vz{typ: spec.GuestQemu, vmid: 101, stamp: stamp, mtime: mt, size: 1, log: okLog(101)})
	s := newFStack(t, fopts{vzdump: []string{dir}})
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	var got []spec.BackupProperties
	for _, loc := range []*time.Location{ny, time.UTC} {
		time.Local = loc
		items := s.backups(t, "{}")
		if len(items) != 1 || items[0].Name != "vzdump-qemu-101-2026_03_08-02_30_00.vma.zst" {
			t.Fatalf("items %+v, want the one archive", items)
		}
		got = append(got, items[0].Properties)
	}
	for i, g := range got {
		if g.Timestamp != "2026-03-08T02:30:00" || g.TimestampZone != "node-local" || !g.ModifiedAt.Equal(mt) ||
			g.ModifiedAt.Location() != time.UTC {
			t.Errorf("zone %d: timestamp %q %q modifiedAt %v, want 2026-03-08T02:30:00 node-local and %v UTC", i,
				g.Timestamp, g.TimestampZone, g.ModifiedAt, mt)
		}
	}
	if got[0].ModifiedAt != got[1].ModifiedAt {
		t.Errorf("modifiedAt differs between zones: %v %v", got[0].ModifiedAt, got[1].ModifiedAt)
	}
}

// stringsAt collects the JSON path of every string in v that holds one of
// the markers.
func stringsAt(v any, at string, markers []string, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			stringsAt(e, at+"/"+k, markers, out)
		}
	case []any:
		for i, e := range x {
			stringsAt(e, at+"/"+strconv.Itoa(i), markers, out)
		}
	case string:
		for _, m := range markers {
			if strings.Contains(x, m) {
				out[at] = x
			}
		}
	}
}

// TC-STG-E5: host text with control characters, invalid UTF-8 and markup stays data. Owner WI 4.3. Requirements: FR-18, NFR-3.
func TestTCSTGE5_HostTextWithControlCharactersInvalidUTF(t *testing.T) {
	a := three(101)[0]
	a.notes = []byte("\x1b[31mred\r\n\xff\xfe tail")
	a.log = append(okLog(101), []byte("2026-11-17 03:04:11 ERROR: </script><b>x</b>\n")...)
	dir := vzTree(t, a)
	share := storagetest.TempDir(t)
	bundle := "Mac‮evil.sparsebundle"
	storagetest.Build(t, share, append(storagetest.BundleNodes(bundle),
		storagetest.Node{Path: bundle + "/" + timemachine.SnapshotHistoryFile, Content: storagetest.SnapshotHistoryXML(t0)})...)
	s := newFStack(t, fopts{vzdump: []string{dir}, tm: []string{share}})

	markers := []string{"\x1b", "\r", "�", "</script>", "‮"}
	found := map[string]string{}
	for _, c := range []struct {
		op   def.Op
		name string
	}{{spec.BackupsList.Op(), "list"}, {spec.BackupsSummary.Op(), "summary"}, {spec.TimeMachineList.Op(), "tm"}} {
		body, err := s.invoke(c.op, "{}")
		if err != nil {
			t.Fatalf("%s: %v", c.op.ID, err)
		}
		raw, err := json.Marshal(body)
		if err != nil || !json.Valid(raw) {
			t.Fatalf("%s marshals invalid JSON: %v", c.op.ID, err)
		}
		if bytes.Contains(raw, []byte{0x1b}) || bytes.Contains(raw, []byte("</script>")) ||
			!utf8Valid(raw) {
			t.Errorf("%s: raw body carries an unescaped control character, markup or invalid UTF-8: %q", c.op.ID, raw)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		stringsAt(v, c.name, markers, found)
	}
	// vzdump.Notes replaces each run of invalid bytes with one U+FFFD
	// (TC-STG-93).
	wantNotes := strings.ToValidUTF8(string(a.notes), "\uFFFD")
	for at, val := range found {
		switch {
		case strings.HasSuffix(at, "/properties/notes"):
			if val != wantNotes {
				t.Errorf("%s = %q, want %q", at, val, wantNotes)
			}
		case strings.Contains(at, "/properties/logErrors/"):
			if !strings.Contains(val, "</script><b>x</b>") {
				t.Errorf("%s = %q, want the ERROR line kept as data", at, val)
			}
		case strings.HasSuffix(at, "/name") && strings.HasPrefix(at, "tm/") && strings.Contains(at, "/bundles/"):
			if val != "Mac‮evil" {
				t.Errorf("%s = %q, want the bundle name", at, val)
			}
		case strings.HasPrefix(at, "summary/") && strings.HasSuffix(at, "/guestName"):
			if val != "\x1b[31mred" {
				t.Errorf("%s = %q, want the notes' first line", at, val)
			}
		default:
			t.Errorf("host text at %s = %q, outside notes, logErrors, guestName and the bundle name", at, val)
		}
	}
	if len(found) < 3 {
		t.Errorf("host text found at %v, want notes, logErrors and the bundle name", found)
	}
}

func utf8Valid(b []byte) bool { return strings.ToValidUTF8(string(b), "\x00") == string(b) }

// TC-STG-E10: an expected guest that also has archives is not duplicated. Owner WI 4.3. Requirements: FR-10.
func TestTCSTGE10_AnExpectedGuestThatAlsoHasArchives(t *testing.T) {
	at := t0.Add(-time.Hour)
	dir := vzTree(t, vz{typ: spec.GuestQemu, vmid: 101, stamp: at, mtime: at, size: 1, log: okLog(101)},
		vz{typ: spec.GuestLXC, vmid: 101, stamp: at, mtime: at, size: 2, log: okLog(101)})
	s := newFStack(t, fopts{vzdump: []string{dir}, mod: func(c *config.StorageConfig) {
		c.Backups.ExpectedGuests = []int{101}
	}})
	sum := s.summary(t)
	if len(sum.Guests) != 2 || sum.GuestCount != 2 {
		t.Fatalf("guests %+v, want qemu/101 and lxc/101 only", sum.Guests)
	}
	for _, typ := range []string{spec.GuestQemu, spec.GuestLXC} {
		if g := guestOf(t, sum, typ, 101); !g.Expected || g.LatestAt == nil {
			t.Errorf("%s/101 = %+v, want expected with its archive", typ, g)
		}
	}
}

// TC-STG-E12: a configuration reload during a Background walk does not interrupt it. Owner WI 4.3. Requirements: FR-12, FR-1.
func TestTCSTGE12_AConfigurationReloadDuringABackgroundWalk(t *testing.T) {
	sh1 := bundleTree(t, 2, map[string]time.Time{"one": t0})
	sh3 := bundleTree(t, 2, map[string]time.Time{"three": t0})
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	s := newFStack(t, fopts{tm: []string{sh1}})
	s.op.stuck(sh1, "ReadDir", "one.sparsebundle/bands", release)
	s.background(t)
	waitFor(t, func() bool { st, _ := s.wk.get(sh1); return st == 1 })
	c := s.tk.next(t)

	configure(t, s.p, func(c *config.StorageConfig) {
		c.TimeMachine.Paths, c.TimeMachine.SizeInterval = []string{sh1, sh3}, 3*time.Hour
	})
	once.Do(func() { close(release) })
	s.wk.waitEnded(t, sh1, 1)
	if bs, ok := s.p.walks.size(sh1, "one.sparsebundle"); !ok || bs.reason != "" || bs.size.Truncated ||
		bs.size.Bytes != allocTree(t, filepath.Join(sh1, "one.sparsebundle", "bands")) {
		t.Errorf("the first walk = %+v, %v, want it completed with share 1's full size", bs, ok)
	}
	if st, _ := s.wk.get(sh3); st != 0 {
		t.Errorf("the added share was walked %d times in the first walk, want 0", st)
	}
	c <- t0
	s.wk.waitEnded(t, sh3, 1)
	s.wk.waitEnded(t, sh1, 2)
	s.tk.next(t)
	if d := s.tk.last(); d != 3*time.Hour {
		t.Errorf("next wait %s, want the reloaded 3h", d)
	}
}

// ---- carried forward and extra WI 4.3 checks ----

// TC-STG-119's handler-level half (carried forward from WI 4.1): through
// the Dispatcher, backups.list and backups.summary answer 503 with the
// backups reason and timemachine.list 503 with "not configured"; none
// answers 200 with an empty list.
func TestTCSTG119_BackupsAndTimeMachineAnswer503ThroughTheDispatcher(t *testing.T) {
	_, file, _ := dirs(t)
	fake := storagetest.NewFake(t, "home-server")
	s := newFStack(t, fopts{vzdump: []string{file}, fsys: homeFS(t), run: fake.Runner()})
	for _, op := range []def.Op{spec.BackupsList.Op(), spec.BackupsSummary.Op()} {
		_, err := s.invoke(op, "{}")
		pe := wantSlug(t, err, problem.CapabilityUnavailable)
		if pe.Status != 503 || !strings.Contains(pe.Detail, file+": not a directory") {
			t.Errorf("%s: %d %q, want 503 naming the backups reason", op.ID, pe.Status, pe.Detail)
		}
	}
	_, err := s.invoke(spec.TimeMachineList.Op(), "{}")
	if pe := wantSlug(t, err, problem.CapabilityUnavailable); pe.Status != 503 || !strings.Contains(pe.Detail, "not configured") {
		t.Errorf("timemachine.list: %d %q, want 503 not configured", pe.Status, pe.Detail)
	}
}

// More than 500 guests: guestsTruncated, and the counters count every
// guest (WI 4.3 Description, A-18).
func TestBackupsSummaryCapsGuestsAt500(t *testing.T) {
	var arch []vz
	for i := range spec.MaxSummaryGuests + 2 {
		at := t0.Add(-time.Hour)
		if i%2 == 0 {
			at = t0.Add(-30 * 24 * time.Hour)
		}
		arch = append(arch, vz{typ: spec.GuestLXC, vmid: 1000 + i, stamp: at, mtime: at, size: 1})
	}
	s := newFStack(t, fopts{vzdump: []string{vzTree(t, arch...)}})
	sum := s.summary(t)
	if len(sum.Guests) != spec.MaxSummaryGuests || !sum.GuestsTruncated || sum.GuestCount != spec.MaxSummaryGuests+2 ||
		sum.StaleCount != (spec.MaxSummaryGuests+2)/2 {
		t.Errorf("%d guests listed, truncated %v, guestCount %d, staleCount %d; want 500, true, 502, 251",
			len(sum.Guests), sum.GuestsTruncated, sum.GuestCount, sum.StaleCount)
	}
}

// An archive still being written is listed, and counts in its guest's
// count and totalBytes, but is never its guest's latest backup (A-63): the
// home-server leftover vzdump-qemu-101-2026_08_27-10_35_50.tmp and its
// .vma.dat from the committed capture (shares/2/ls.txt).
func TestInProgressArchiveIsListedButNotAGuestsLatest(t *testing.T) {
	dump, err := storagetest.ParseListing(home(t, "shares/2/ls.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var keep []storagetest.Listed
	var datSize int64
	for _, e := range dump {
		if strings.Contains(e.Name, "-101-") {
			keep = append(keep, e)
			if strings.HasSuffix(e.Name, ".vma.dat") {
				datSize = e.Size
			}
		}
	}
	if len(keep) != 2 || datSize == 0 {
		t.Fatalf("capture: %d qemu-101 entries (.vma.dat %d bytes), want the .tmp and the .vma.dat", len(keep), datSize)
	}
	leftover := func(t *testing.T) string {
		t.Helper()
		nodes, err := storagetest.ListingNodes(keep, "")
		if err != nil {
			t.Fatal(err)
		}
		dir := storagetest.TempDir(t)
		storagetest.Build(t, dir, nodes...)
		return dir
	}
	expect101 := func(c *config.StorageConfig) { c.Backups.ExpectedGuests = []int{101} }

	t.Run("only in-progress archives", func(t *testing.T) {
		s := newFStack(t, fopts{vzdump: []string{leftover(t)}, mod: expect101})
		items := s.backups(t, "{}")
		if len(items) != 2 {
			t.Fatalf("%d items, want the .tmp and the .vma.dat", len(items))
		}
		for _, it := range items {
			bp := it.Properties
			if !bp.InProgress {
				t.Errorf("%s inProgress false", it.Name)
			}
			if strings.HasSuffix(it.Name, ".tmp") {
				if bp.SizeBytes != nil {
					t.Errorf("%s sizeBytes %v, want null", it.Name, *bp.SizeBytes)
				}
				wantReason(t, bp.Unavailable, "sizeBytes", "being written")
			}
		}
		sum := s.summary(t)
		g := guestOf(t, sum, spec.GuestQemu, 101)
		if g.LatestAt != nil || g.LatestFile != nil || g.LatestSizeBytes != nil || g.AgeHours != nil ||
			g.GuestName != nil || g.LatestLogStatus != vzdump.LogUnknown || !g.Stale || !g.Expected ||
			g.Count != 2 || g.TotalBytes != datSize {
			t.Errorf("qemu/101 = %+v, want latestAt null, stale, expected, count 2, totalBytes %d", g, datSize)
		}
		if sum.GuestCount != 1 || sum.StaleCount != 1 {
			t.Errorf("guestCount %d staleCount %d, want 1 and 1 (no second, empty-type 101)", sum.GuestCount,
				sum.StaleCount)
		}
	})

	t.Run("in-progress beside a finished archive", func(t *testing.T) {
		dir := leftover(t)
		// A finished qemu-101 archive older than the leftover, a day before t0.
		done := t0.Add(-24 * time.Hour)
		fin := vz{typ: spec.GuestQemu, vmid: 101, stamp: done, mtime: done, size: 4242,
			notes: []byte("home-server\n"), log: okLog(101)}
		storagetest.Build(t, dir, fin.nodes()...)
		s := newFStack(t, fopts{vzdump: []string{dir}, mod: expect101})
		sum := s.summary(t)
		g := guestOf(t, sum, spec.GuestQemu, 101)
		age := t0.Sub(done).Hours()
		if g.LatestFile == nil || *g.LatestFile != fin.file() || g.LatestAt == nil || !g.LatestAt.Equal(done) ||
			g.LatestSizeBytes == nil || *g.LatestSizeBytes != fin.size || g.LatestLogStatus != vzdump.LogOK ||
			g.AgeHours == nil || *g.AgeHours != age || g.GuestName == nil || *g.GuestName != "home-server" ||
			g.Stale || !g.Expected || g.Count != 3 || g.TotalBytes != datSize+fin.size {
			t.Errorf("qemu/101 = %+v, want latest %s (not the leftover), count 3, totalBytes %d", g, fin.file(),
				datSize+fin.size)
		}
		if sum.GuestCount != 1 {
			t.Errorf("guestCount %d, want 1", sum.GuestCount)
		}
	})
}

// A-64: storage.backups.list pages are byte-budgeted as well as counted.
// With 1 KiB notes and ten 512-byte log error lines an archive takes about
// 6.8 KB, so limit=500 would be 3.4 MB; the page ends early under
// spec.BackupsPageBudgetBytes with nextCursor set, and the cursors walk
// every archive once, in order.
func TestA64_BackupsListPageEndsUnderTheByteBudget(t *testing.T) {
	var log bytes.Buffer
	for i := range spec.MaxLogErrorLines + 2 {
		log.WriteString(pad(fmt.Sprintf("2026-11-17 03:04:%02d ERROR: line %d ", i, i), 600) + "\n")
	}
	notes := []byte(strings.Repeat("€", spec.NotesMaxBytes/3) + "\n")
	const total = 300
	var arch []vz
	for i := range total {
		at := t0.Add(-time.Duration(i+1) * time.Minute) // Newest first is vmid order
		arch = append(arch, vz{typ: spec.GuestQemu, vmid: 100 + i, stamp: at, mtime: at, size: 1 << 40,
			notes: notes, log: log.Bytes()})
	}
	s := newFStack(t, fopts{vzdump: []string{vzTree(t, arch...)}})

	var got []string
	cursor, pages := "", 0
	for {
		in := `{"limit":500}`
		if cursor != "" {
			in = fmt.Sprintf(`{"limit":500,"cursor":%q}`, cursor)
		}
		l := fcall[apitypes.List[spec.Backup]](t, s, spec.BackupsList.Op(), in)
		pages++
		body, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > 1<<20 {
			t.Errorf("page %d is %d bytes, over 1 MiB", pages, len(body))
		}
		if pages == 1 {
			if len(l.Items) >= 500 || len(l.Items) == 0 || l.NextCursor == "" {
				t.Errorf("page 1 holds %d items with nextCursor %q; want fewer than 500 and a cursor", len(l.Items), l.NextCursor)
			}
		}
		for _, it := range l.Items {
			got = append(got, it.Name)
		}
		if l.NextCursor == "" {
			break
		}
		cursor = l.NextCursor
		if pages > total {
			t.Fatal("the cursors do not end")
		}
	}
	if pages < 2 {
		t.Errorf("%d pages, want the budget to split %d archives", pages, total)
	}
	var want []string
	for _, a := range arch {
		want = append(want, a.file())
	}
	if !slices.Equal(got, want) {
		t.Errorf("the pages hold %d archives, want every one of the %d once and in order", len(got), len(want))
	}
}

// A-64: a page always holds at least one item, and the item that would cross
// the budget starts the next page. One maximal item is below 64 KiB, so an
// item over spec.BackupsPageBudgetBytes cannot be built from a tree; the
// cut is exercised on its own with a small budget.
func TestA64_PageWithinBudgetKeepsAtLeastOneItem(t *testing.T) {
	big := strings.Repeat("a", 100)
	items := []string{big, big, big}
	enc := func(s string) int { b, _ := json.Marshal(s); return len(b) }
	for _, c := range []struct {
		name   string
		budget int
		want   int
	}{
		{"the first item alone is over the budget", enc(big) - 1, 1},
		{"exactly one item fits", enc(big), 1},
		{"two items and their comma fit", 2*enc(big) + 1, 2},
		{"one byte short of two", 2 * enc(big), 1},
		{"every item fits", 1 << 20, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := withinBudget(items, c.budget); got != c.want {
				t.Errorf("withinBudget = %d, want %d", got, c.want)
			}
		})
	}
	if got := withinBudget([]string{}, 10); got != 0 {
		t.Errorf("withinBudget of no items = %d, want 0", got)
	}
}
