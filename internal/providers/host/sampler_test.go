// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// logCapture is a slog.Handler that keeps every record.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (l *logCapture) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, r.Clone())
	return nil
}

func (l *logCapture) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logCapture) WithGroup(string) slog.Handler      { return l }

// records returns the records at level whose message contains msg.
func (l *logCapture) records(level slog.Level, msg string) []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Record
	for _, r := range l.recs {
		if r.Level == level && strings.Contains(r.Message, msg) {
			out = append(out, r)
		}
	}
	return out
}

// attr is the string value of a record's attribute key.
func attr(r slog.Record, key string) string {
	var v string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
			return false
		}
		return true
	})
	return v
}

// samplerStack is the host stack of the sampler tests: the provider over
// F-FS with F-CLK, F-TICK, a 4096-byte page and a capturing logger.
type samplerStack struct {
	t    *testing.T
	p    *Provider
	ffs  *fakeFS
	clk  *fakeClock
	tks  *fakeTickers
	logs *logCapture
	runs int // RunBackground calls started, so the n-th uses the n-th ticker
}

func newSamplerStack(t *testing.T, capture string) *samplerStack {
	t.Helper()
	ffs := fixtureFS(t, capture)
	return newSamplerStackFS(t, capture, ffs, "linux")
}

func newSamplerStackFS(t *testing.T, capture string, ffs *fakeFS, goos string) *samplerStack {
	t.Helper()
	plat := platform.Info{Init: platform.Systemd}
	if capture == capALP {
		plat.Init = platform.OpenRC
	}
	s := &samplerStack{t: t, ffs: ffs, clk: newFakeClock(), tks: newFakeTickers(t), logs: &logCapture{}}
	s.p = New(Options{Host: "h1", Logger: slog.New(s.logs), Platform: &plat, GOOS: goos, FS: ffs,
		Run: newFakeRun(t, capture).Run, Now: s.clk.Now, Ticker: s.tks.New, PageSize: 4096})
	return s
}

// background is one RunBackground call: its cancel, its exit and its
// ticker.
type background struct {
	cancel context.CancelFunc
	done   chan struct{}
	tk     *fakeTicker
}

// start runs RunBackground in a goroutine, waits for its immediate sample
// and returns it. The test's cleanup cancels it and waits for it.
func (s *samplerStack) start() *background {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b := &background{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(b.done)
		s.p.RunBackground(ctx)
	}()
	s.t.Cleanup(func() {
		cancel()
		<-b.done
	})
	s.runs++
	b.tk = s.tks.nth(s.runs)
	return b
}

// waitSeq waits until the current run has published snapshot seq n (or a
// later one) and returns that snapshot.
func (s *samplerStack) waitSeq(n uint64) *snapshot {
	s.t.Helper()
	var snap *snapshot
	eventually(s.t, 2*time.Second, func() bool {
		snap = s.p.run.Load().snapshot()
		return snap != nil && snap.seq >= n
	}, "snapshot seq %d", n)
	return snap
}

// fire advances F-CLK by d and delivers one tick of b at the new time. It
// returns once the sampler has finished the tick and is idle again: the
// snapshot is published before the history append, so a test that waited
// for the snapshot (waitSeq) alone could read the history before the
// tick's point is in it.
func (s *samplerStack) fire(b *background, d time.Duration) {
	s.t.Helper()
	s.clk.Advance(d)
	b.tk.Fire(s.clk.Now())
	b.tk.waitIdle()
}

// ready starts the sampler and drives it to a ready snapshot over the
// capture's fs/ then t1/ samples, 10 s apart.
func (s *samplerStack) ready() (*background, *snapshot) {
	s.t.Helper()
	b := s.start()
	s.waitSeq(1)
	s.ffs.advance()
	s.fire(b, 10*time.Second)
	return b, s.waitSeq(2)
}

// mustVal is a computed value that must be present.
func mustVal(t *testing.T, snap *snapshot, now time.Time, what string, v rateVal) float64 {
	t.Helper()
	x, why := snap.val(now, v)
	if x == nil {
		t.Fatalf("%s: null with %q, want a value", what, why)
	}
	return *x
}

// wantMissing checks a computed value is null with exactly why.
func wantMissing(t *testing.T, snap *snapshot, now time.Time, what string, v rateVal, why string) {
	t.Helper()
	x, got := snap.val(now, v)
	if x != nil {
		t.Errorf("%s = %v, want null with %q", what, *x, why)
		return
	}
	if got != why {
		t.Errorf("%s reason = %q, want %q", what, got, why)
	}
}

func TestSnapshotNilIsNotRunning(t *testing.T) {
	var r *samplerRun
	snap := r.snapshot()
	now := time.Now()
	if got := snap.state(now); got != spec.SamplerNotRunning {
		t.Errorf("state = %q, want %q", got, spec.SamplerNotRunning)
	}
	if r.history() != nil {
		t.Error("nil run has a history")
	}
	x, why := snap.val(now, have(1))
	if x != nil || why != reasonNotRunning {
		t.Errorf("val = %v, %q; want nil, %q", x, why, reasonNotRunning)
	}
	sm := snap.sampler(now)
	if sm.SampledAt != nil || sm.IntervalSeconds != nil || sm.AgeSeconds != nil {
		t.Errorf("sampler = %+v, want every field null", sm)
	}
	for _, k := range []string{"sampledAt", "intervalSeconds", "ageSeconds"} {
		if sm.Unavailable[k] != reasonNotRunning {
			t.Errorf("unavailable[%s] = %q, want %q", k, sm.Unavailable[k], reasonNotRunning)
		}
	}
	if byCPU, byMem, secs, why := snap.processes(now); byCPU != nil || byMem != nil || secs != nil || why != reasonNotRunning {
		t.Errorf("processes = %v, %v, %v, %q; want empty with %q", byCPU, byMem, secs, why, reasonNotRunning)
	}
}

// A Configure'd sourceTimeout reaches the source layer: a blocked
// hardware attribute times out after it, not after the 2 s default
// (orchestrator note on WI 3.1, binding for 3.3).
func TestConfiguredSourceTimeoutBoundsSources(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	if err := s.p.Configure(hostSection(t, "sourceTimeout: 250ms\n")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	const attr = "sys/class/net/eth0/speed"
	s.ffs.block(attr)
	start := time.Now()
	_, err := s.p.src.readBounded(context.Background(), attr)
	took := time.Since(start)
	if err == nil {
		t.Fatal("blocked read succeeded")
	}
	if got, want := reasonFor(attr, err), "timed out after 250ms: /"+attr; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
	if took >= time.Second {
		t.Errorf("blocked read took %v, want about the configured 250ms", took)
	}

	if err := s.p.Configure(hostSection(t, "sourceTimeout: 1s\n")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	// The flight of the first call is still in flight and now younger than
	// the new 1 s bound, so this call joins it until its start plus 1 s.
	_, err = s.p.src.readBounded(context.Background(), attr)
	if got, want := reasonFor(attr, err), "timed out after 1s: /"+attr; got != want {
		t.Errorf("after reconfiguring: reason = %q, want %q", got, want)
	}
}

// The sampler reads only the counters it documents, every one through the
// allow-list, and scans /proc only when the scan is due.
func TestReadSampleReadsTheDocumentedSources(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	src := newSource(ffs, time.Second, nil)
	clk := newFakeClock()
	cur := readSample(src, clk.Now, clk.Now(), false)
	for _, c := range ffs.recorded() {
		if strings.HasPrefix(c.name, "proc/") && c.name != pathStat && c.name != pathDiskstats &&
			c.name != pathNetDev && c.name != pathMeminfo && c.name != pathLoadavg {
			t.Errorf("unscanned sample touched %s", c)
		}
	}
	if cur.procs != nil || cur.scanned {
		t.Error("unscanned sample has a process scan")
	}
	if cur.cpu == nil || cur.disks == nil || cur.ifaces == nil || cur.mem == nil || cur.load == nil {
		t.Fatalf("a source is missing: %+v", cur)
	}
	if got := cur.ifaces["eth0"].ifindex; got != 2 {
		t.Errorf("eth0 ifindex = %d, want 2 (sys/class/net/eth0/ifindex)", got)
	}

	ffs.resetRecorder()
	cur = readSample(src, clk.Now, clk.Now(), true)
	pids, err := os.ReadDir(filepath.Join("testdata", capUBU, "fs", "proc"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range pids {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			n++
			if ffs.count("open", "proc/"+e.Name()+"/stat") != 1 {
				t.Errorf("scan did not read proc/%s/stat once", e.Name())
			}
		}
	}
	if !cur.scanned || cur.procs == nil || len(cur.procs.pids) != n {
		t.Fatalf("scan found %v, want %d pids", cur.procs, n)
	}
}

// A failed /proc listing makes the top lists unavailable with its reason.
func TestFailedProcessScanGivesItsReason(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	s.ffs.deny("proc/1")
	_, snap := s.ready()
	now := s.clk.Now()
	byCPU, _, _, why := snap.processes(now)
	if why != "" {
		t.Fatalf("one unreadable pid: processes unavailable with %q, want the rest ranked", why)
	}
	for _, p := range byCPU {
		if p.pid == 1 {
			t.Errorf("unreadable pid 1 ranked: %+v", p)
		}
	}

	s2 := newSamplerStack(t, capUBU)
	s2.ffs.addFault(&fsFault{match: exactly("proc"), errno: syscall.EACCES})
	_, snap = s2.ready()
	byCPU, byMem, secs, why := snap.processes(s2.clk.Now())
	if want := "permission denied: /proc"; why != want || byCPU != nil || byMem != nil || secs != nil {
		t.Errorf("processes = %v, %v, %v, %q; want empty with %q", byCPU, byMem, secs, why, want)
	}
	if mustVal(t, snap, s2.clk.Now(), "cpu usage", snap.cpu.usage) < 0 {
		t.Error("negative cpu usage")
	}
}

func TestHistSampleOf(t *testing.T) {
	s := newSamplerStack(t, capUBU)
	_, snap := s.ready()
	h := s.p.run.Load().history()
	h.mu.Lock()
	count := h.count
	h.mu.Unlock()
	if count != 2 {
		t.Fatalf("history holds %d points after two samples, want 2", count)
	}
	var cur sample
	func() {
		src := newSource(s.ffs, time.Second, nil)
		cur = readSample(src, s.clk.Now, s.clk.Now(), false)
	}()
	hs := histSampleOf(cur, snap, wholeDisks(s.p.src))
	mem := cur.mem
	if hs.MemUsed == nil || *hs.MemUsed != float64(mem["MemTotal"]-mem["MemAvailable"]) {
		t.Errorf("MemUsed = %v, want MemTotal − MemAvailable", hs.MemUsed)
	}
	if hs.SwapUsed == nil || *hs.SwapUsed != float64(mem["SwapTotal"]-mem["SwapFree"]) {
		t.Errorf("SwapUsed = %v, want SwapTotal − SwapFree", hs.SwapUsed)
	}
	if hs.Load1 == nil || *hs.Load1 != cur.load.Load1 {
		t.Errorf("Load1 = %v, want %v", hs.Load1, cur.load.Load1)
	}
	if hs.CPUPercent != snap.cpu.usage.v {
		t.Error("CPUPercent is not the snapshot's usage")
	}
	for _, d := range hs.Disks {
		if want := !strings.HasPrefix(d.Name, "loop"); d.Device != want {
			t.Errorf("disk %s Device = %v, want %v", d.Name, d.Device, want)
		}
	}
	// Disk entities are the whole block devices the disks op lists, never a
	// partition or an unbacked loop (orchestrator ruling after WI 4.2).
	var disks []string
	for _, d := range hs.Disks {
		disks = append(disks, d.Name)
	}
	slices.Sort(disks)
	if want := []string{"vda", "vdb", "vdc"}; len(hs.Interfaces) != len(snap.ifaces) || !slices.Equal(disks, want) {
		t.Errorf("history entities %d interfaces, disks %v; want %d interfaces, disks %v", len(hs.Interfaces), disks,
			len(snap.ifaces), want)
	}
}

// A partition never takes a history disk slot, even with slots free, and a
// diskstats line joins a whole device only on its name and its maj:min
// (orchestrator ruling after WI 4.2). The held disks are exactly the disks
// op's list.
func TestHistoryDiskSlotsAreWholeDevicesOnly(t *testing.T) {
	s := newHostStack(t, capUBU)
	// vdc's sys/block dev says 253:99, so its diskstats line (253:32) is a
	// different device that happens to carry the name.
	s.ffs.replace("sys/block/vdc/dev", []byte("253:99\n"))
	_, snap := s.ready()
	for _, part := range []string{"vda1", "vda15", "vda16", "vdb1", "vdc"} {
		if _, ok := snap.disks[part]; !ok {
			t.Fatalf("the snapshot lacks %s; the fixture no longer has the partition lines", part)
		}
	}
	h := s.p.run.Load().history()
	held := heldNames(h, entDisk)
	if want := []string{"vda", "vdb"}; !slices.Equal(held, want) {
		t.Errorf("held disk slots %v, want %v (no partition, unbacked loop or maj:min mismatch)", held, want)
	}
	var listed []string
	for _, d := range s.diskList() {
		if d.Name != "vdc" {
			listed = append(listed, d.Name)
		}
	}
	if !slices.Equal(held, listed) {
		t.Errorf("held disk slots %v, the disks op lists %v", held, listed)
	}
	p := h.Query(s.clk.Now(), 3600, 0, []string{spec.MetricDisk})
	if p.EntitiesTruncated.Disks != 0 {
		t.Errorf("entitiesTruncated.disks %d, want 0", p.EntitiesTruncated.Disks)
	}
}

// An unreadable sys/block gives no history disk entities: none of the
// diskstats lines can be told from a partition.
func TestHistoryDisksWithoutSysBlock(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny("sys/block")
	s.ready()
	if held := heldNames(s.p.run.Load().history(), entDisk); len(held) != 0 {
		t.Errorf("held disk slots %v with sys/block denied, want none", held)
	}
}
