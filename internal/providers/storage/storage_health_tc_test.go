// Tests of planning/storage-provider/test-cases.md TC-STG-160 to
// TC-STG-172 (owner WI 4.4).
//
// The bindings land in WI 5.1, so "through the Dispatcher" runs a real
// core.Dispatcher over healthBound, the provider with storage.health.get and
// every read bound here (the zfsReadsBound and filesBound precedent). Synthetic
// ZFS variants are the home-server JSON captures decoded with exact numbers,
// changed member by member and encoded again; the expected signals are
// computed in the tests from FR-15's rules, never taken from health.go.
//
// The pihole capture (WI 1.2) is not committed, so "pihole F-ZFS" (zfs
// unavailable at preflight) is a host root with no zpool or zfs, the
// substitution WI 4.3's TC-STG-152 made; the observable, zfs unavailable with
// its preflight reason, is the same.

package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
	"github.com/mauktik/nexus/internal/providers/storage/timemachine"
	"github.com/mauktik/nexus/internal/providers/storage/vzdump"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
	"github.com/mauktik/nexus/internal/server"
)

// ---- test support (WI 4.4) ----

// healthBound is the provider with storage.health.get and every read bound,
// for the Dispatcher before the bindings of WI 5.1 exist.
type healthBound struct{ *Provider }

func (b healthBound) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.HealthGet, b.healthGet),
		provider.Sync(spec.PoolsList, b.poolsList),
		provider.Sync(spec.PoolsGet, b.poolsGet),
		provider.Sync(spec.DatasetsList, b.datasetsList),
		provider.Sync(spec.DatasetsGet, b.datasetsGet),
		provider.Sync(spec.SnapshotsList, b.snapshotsList),
		provider.Sync(spec.BackupsList, b.backupsList),
		provider.Sync(spec.BackupsSummary, b.backupsSummary),
		provider.Sync(spec.TimeMachineList, b.timemachineList),
	}
}

type hopts struct {
	zfs        bool // Home-server F-ZFS; false: a host with no zpool or zfs
	setup      func(*storagetest.Fake, *overlay)
	vzdump, tm []string
	mod        func(*config.StorageConfig)
	timeout    time.Duration
	now        time.Time // Zero: t0
}

// hstack is the provider behind a Dispatcher after the registry's startup
// preflight, at F-CLOCK.
type hstack struct {
	fake *storagetest.Fake // Nil without zfs
	ov   *overlay
	p    *Provider
	rec  *records
	clk  *clock
	op   *fopener
	sf   *fstatfs
	wk   *fwalks
	reg  *core.Registry
	d    *core.Dispatcher
}

func newHStack(t *testing.T, o hopts) *hstack {
	t.Helper()
	s := &hstack{clk: &clock{}, op: newFOpener(), sf: newFStatfs(t), wk: newFWalks()}
	s.clk.set(t0)
	if !o.now.IsZero() {
		s.clk.set(o.now)
	}
	for _, p := range o.tm {
		s.sf.set(p, Usage{Blocks: 100, Bfree: 60, Bavail: 60, Bsize: 4096})
	}
	var fsys fstest.MapFS = fstest.MapFS{}
	run := noRun(t)
	if o.zfs {
		s.fake = storagetest.NewFake(t, "home-server")
		s.ov = newOverlay(s.fake)
		if o.setup != nil {
			o.setup(s.fake, s.ov)
		}
		fsys, run = homeFS(t), s.ov.run
	}
	s.p, s.rec = newTestProvider(fsys, run, func(op *Options) {
		op.Now, op.OpenRoot, op.Statfs, op.ObserveWalk = s.clk.Now, s.op.openRoot, s.sf.statfs, s.wk.observe
	})
	reg, err := core.New([]provider.Provider{healthBound{s.p}}, core.Options{
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
	s.reg = reg
	s.d = core.NewDispatcher(reg, core.DispatcherOptions{Logger: discard})
	return s
}

// health is storage.health.get through d.
func healthOf(t *testing.T, d *core.Dispatcher) spec.HealthProperties {
	t.Helper()
	res, err := d.Invoke(context.Background(), core.InvokeRequest{
		Caller:  auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "tc-stg-health", Scopes: []string{spec.ReadScope}},
		Surface: core.SurfaceREST, Op: spec.HealthGet.Op(), Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("storage.health.get: %v", err)
	}
	l, ok := res.Body.(apitypes.List[spec.Health])
	if res.Status != 200 || !ok || len(l.Items) != 1 || l.Items[0].Name != spec.CurrentName {
		t.Fatalf("storage.health.get: status %d body %+v, want one item named current", res.Status, res.Body)
	}
	return l.Items[0].Properties
}

func (s *hstack) health(t *testing.T) spec.HealthProperties {
	t.Helper()
	return healthOf(t, s.d)
}

// sig is a signal's (id, severity, subject).
type sig struct{ id, sev, subject string }

func (g sig) String() string { return g.sev + " " + g.id + " " + g.subject }

func sigsOf(signals []apitypes.HealthSignal) []sig {
	out := []sig{}
	for _, s := range signals {
		out = append(out, sig{s.ID, s.Severity, s.Subject})
	}
	slices.SortFunc(out, func(a, b sig) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func sortedSigs(in ...sig) []sig {
	out := append([]sig{}, in...)
	slices.SortFunc(out, func(a, b sig) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// wantSigs fails unless h's signals are exactly want, as a set.
func wantSigs(t *testing.T, h spec.HealthProperties, want ...sig) {
	t.Helper()
	if got, w := sigsOf(h.Signals), sortedSigs(want...); !slices.Equal(got, w) {
		t.Errorf("signals %v, want %v", got, w)
	}
}

// signalOf is the one signal of h with id and subject.
func signalOf(t *testing.T, h spec.HealthProperties, id, subject string) apitypes.HealthSignal {
	t.Helper()
	var found []apitypes.HealthSignal
	for _, s := range h.Signals {
		if s.ID == id && s.Subject == subject {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s signals for %s in %v, want 1", len(found), id, subject, sigsOf(h.Signals))
	}
	return found[0]
}

func countSeverity(signals []apitypes.HealthSignal) apitypes.SignalCounts {
	var c apitypes.SignalCounts
	for _, s := range signals {
		switch s.Severity {
		case apitypes.SeverityCritical:
			c.Critical++
		case apitypes.SeverityWarning:
			c.Warning++
		case apitypes.SeverityInfo:
			c.Info++
		}
	}
	return c
}

// ---- synthetic ZFS variants from the home-server JSON captures ----

// homeJSON decodes a home-server JSON capture with its numbers kept exact.
func homeJSON(t testing.TB, name string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(home(t, name)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return m
}

func encodeJSON(t testing.TB, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func obj(v any) map[string]any { return v.(map[string]any) }

// docWith is a home-server JSON capture with its container's members
// changed by fn.
func docWith(t testing.TB, name, container string, fn func(members map[string]any)) []byte {
	t.Helper()
	doc := homeJSON(t, name)
	fn(obj(doc[container]))
	return encodeJSON(t, doc)
}

// setValue sets a {value, source} property of a zpool list or zfs list item.
func setValue(item map[string]any, prop string, v any) {
	obj(obj(item["properties"])[prop])["value"] = v
}

// clone deep-copies a decoded JSON value.
func clone(t testing.TB, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func n(v int64) json.Number { return json.Number(fmt.Sprint(v)) }

// answerList, answerStatus and answerDatasets plant a changed capture as
// the health read's zpool list, zpool status and zfs list.
func answerList(t testing.TB, f *storagetest.Fake, o *overlay, fn func(pools map[string]any)) {
	t.Helper()
	o.answer(f.Argv(zfs.CmdPoolList), zfs.Result{Stdout: docWith(t, "zfs/zpool-list.json", "pools", fn)})
}

func answerStatus(t testing.TB, f *storagetest.Fake, o *overlay, fn func(pools map[string]any)) {
	t.Helper()
	o.answer(f.Argv(zfs.CmdPoolStatus), zfs.Result{Stdout: docWith(t, "zfs/zpool-status.json", "pools", fn)})
}

func answerDatasets(t testing.TB, f *storagetest.Fake, o *overlay, fn func(ds map[string]any)) {
	t.Helper()
	o.answer(f.Argv(zfs.CmdDatasetList), zfs.Result{Stdout: docWith(t, "zfs/zfs-list-datasets.json", "datasets", fn)})
}

// z1Leaf is z1's one leaf vdev in a decoded zpool status.
func z1Leaf(pools map[string]any) map[string]any {
	leaves := obj(obj(obj(obj(pools["z1"])["vdevs"])["z1"])["vdevs"])
	for _, v := range leaves {
		return obj(v)
	}
	return nil
}

// syntheticScan is the scan_stats of a WI 1.1 synthetic vector.
func syntheticScan(t testing.TB, vector string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(synthetic(t, vector)))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range obj(doc["pools"]) {
		return obj(obj(p)["scan_stats"])
	}
	t.Fatalf("%s has no pool", vector)
	return nil
}

// finishedAt is a finished scrub's scan_stats ending at end.
func finishedAt(t testing.TB, end time.Time, errs, processed int64) map[string]any {
	sc := syntheticScan(t, storagetest.VectorScrubFinished)
	sc["start_time"], sc["pass_start"], sc["end_time"] = n(end.Unix()-600), n(end.Unix()-600), n(end.Unix())
	sc["errors"], sc["processed"] = n(errs), n(processed)
	return sc
}

// ---- TC-STG-160 to TC-STG-172 ----

// TC-STG-160: pool-not-online and pool-errors. Owner WI 4.4. Requirements: FR-15.
func TestTCSTG160_PoolNotOnlineAndPoolErrors(t *testing.T) {
	words := map[string]string{"ONLINE": zfs.HealthOnline, "DEGRADED": zfs.HealthDegraded, "FAULTED": zfs.HealthFaulted,
		"OFFLINE": zfs.HealthOffline, "UNAVAIL": zfs.HealthUnavail, "REMOVED": zfs.HealthRemoved,
		"SUSPENDED": zfs.HealthSuspended, "SPLIT": zfs.HealthUnknown}
	for raw, word := range words {
		t.Run(raw, func(t *testing.T) {
			s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
				answerList(t, f, o, func(pools map[string]any) { setValue(obj(pools["z1"]), "health", raw) })
			}})
			h := s.health(t)
			if word == zfs.HealthOnline {
				wantSigs(t, h)
				return
			}
			wantSigs(t, h, sig{signalPoolNotOnline, apitypes.SeverityCritical, "pool:z1"})
			if m := signalOf(t, h, signalPoolNotOnline, "pool:z1").Message; !strings.Contains(m, word) {
				t.Errorf("message %q, want it to name %s", m, word)
			}
		})
	}
	for name, fn := range map[string]func(pools map[string]any){
		"checksum error on a leaf": func(pools map[string]any) { z1Leaf(pools)["checksum_errors"] = n(1) },
		"permanent errors":         func(pools map[string]any) { obj(pools["z1"])["error_count"] = n(1) },
	} {
		t.Run(name, func(t *testing.T) {
			s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) { answerStatus(t, f, o, fn) }})
			wantSigs(t, s.health(t), sig{signalPoolErrors, apitypes.SeverityCritical, "pool:z1"})
		})
	}
	t.Run("clean capture", func(t *testing.T) {
		h := newHStack(t, hopts{zfs: true}).health(t)
		for _, sg := range h.Signals {
			if sg.ID == signalPoolNotOnline || sg.ID == signalPoolErrors {
				t.Errorf("clean capture raised %v", sg)
			}
		}
	})
}

// TC-STG-161: pool-capacity at its boundaries. Owner WI 4.4. Requirements: FR-15.
func TestTCSTG161_PoolCapacityAtItsBoundaries(t *testing.T) {
	for capacity, want := range map[int64]string{79: "", 80: apitypes.SeverityWarning, 81: apitypes.SeverityWarning,
		89: apitypes.SeverityWarning, 90: apitypes.SeverityCritical, 91: apitypes.SeverityCritical} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
				answerList(t, f, o, func(pools map[string]any) { setValue(obj(pools["z1"]), "capacity", n(capacity)) })
			}})
			if c := s.p.Settings().Thresholds; c.PoolCapacityWarn != 80 || c.PoolCapacityCrit != 90 {
				t.Fatalf("thresholds %+v, want the defaults 80/90", c)
			}
			h := s.health(t)
			if want == "" {
				wantSigs(t, h)
				return
			}
			wantSigs(t, h, sig{signalPoolCapacity, want, "pool:z1"})
		})
	}
}

// TC-STG-162: scrub-overdue, scrub-age-unknown and scrub-errors. Owner WI 4.4. Requirements: FR-15, FR-6.
func TestTCSTG162_ScrubOverdueScrubAgeUnknownAndScrub(t *testing.T) {
	day := 24 * time.Hour
	overdue := sig{signalScrubOverdue, apitypes.SeverityWarning, "pool:z1"}
	unknown := sig{signalScrubAgeUnknown, apitypes.SeverityInfo, "pool:z1"}
	repairs := sig{signalScrubErrors, apitypes.SeverityCritical, "pool:z1"}
	canceledAt := func(at time.Time) map[string]any {
		sc := syntheticScan(t, storagetest.VectorScrubCanceled)
		sc["end_time"] = n(at.Unix())
		return sc
	}
	cases := []struct {
		name  string
		memo  map[string]any // A scan health reads first, feeding the memo; nil: none
		shown map[string]any // Nil: no scan_stats (function none)
		want  []sig
	}{
		{"finished 34 days ago", nil, finishedAt(t, t0.Add(-34*day), 0, 0), nil},
		{"finished 35 days ago", nil, finishedAt(t, t0.Add(-35*day), 0, 0), nil},
		{"finished 36 days ago", nil, finishedAt(t, t0.Add(-36*day), 0, 0), []sig{overdue}},
		{"never scrubbed", nil, nil, []sig{overdue}},
		{"resilver, empty memo", nil, syntheticScan(t, storagetest.VectorResilverScanning), []sig{unknown}},
		{"repaired bytes", nil, finishedAt(t, t0.Add(-day), 0, 4096), []sig{repairs}},
		{"repaired errors", nil, finishedAt(t, t0.Add(-day), 2, 0), []sig{repairs}},
		{"canceled 1h ago, memo 40 days", finishedAt(t, t0.Add(-40*day), 0, 0), canceledAt(t0.Add(-time.Hour)),
			[]sig{overdue}},
		{"canceled, empty memo", nil, canceledAt(t0.Add(-time.Hour)), []sig{unknown}},
		{"paused, empty memo", nil, syntheticScan(t, storagetest.VectorScrubPaused), []sig{unknown}},
		{"canceled, memo 2 days", finishedAt(t, t0.Add(-2*day), 0, 0), canceledAt(t0.Add(-time.Hour)), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scan := func(sc map[string]any) func(pools map[string]any) {
				return func(pools map[string]any) {
					if sc == nil {
						delete(obj(pools["z1"]), "scan_stats")
						return
					}
					obj(pools["z1"])["scan_stats"] = clone(t, sc)
				}
			}
			s := newHStack(t, hopts{zfs: true})
			if c.memo != nil {
				answerStatus(t, s.fake, s.ov, scan(c.memo))
				s.health(t)
			}
			answerStatus(t, s.fake, s.ov, scan(c.shown))
			h := s.health(t)
			wantSigs(t, h, c.want...)
		})
	}
}

// TC-STG-163: dataset-quota uses quota-relative use only. Owner WI 4.4. Requirements: FR-15, FR-7.
func TestTCSTG163_DatasetQuotaUsesQuotaRelativeUseOnly(t *testing.T) {
	for _, c := range []struct {
		used int64
		want bool
	}{{849, false}, {850, true}, {900, true}} {
		t.Run(fmt.Sprintf("%.1f%%", float64(c.used)/10), func(t *testing.T) {
			s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
				answerDatasets(t, f, o, func(ds map[string]any) {
					tm := obj(ds["z2/d1/timemachine"])
					setValue(tm, "quota", n(1000))
					setValue(tm, "used", n(c.used))
					// No quota, 99% of used + available.
					share := obj(ds["z1/ds/share"])
					setValue(share, "quota", n(0))
					setValue(share, "refquota", n(0))
					setValue(share, "used", n(990))
					setValue(share, "available", n(10))
				})
			}})
			h := s.health(t)
			if !c.want {
				wantSigs(t, h)
				return
			}
			wantSigs(t, h, sig{signalDatasetQuota, apitypes.SeverityWarning, "dataset:z2/d1/timemachine"})
		})
	}
}

// tc157Share plants TC-STG-157's four bundles: b1 backed up T0−6d, b2
// T0−8d, b3 with a binary plist and a Results.plist of T0−8d, b4 with no
// date at all.
func tc157Share(t *testing.T) string {
	t.Helper()
	share := storagetest.TempDir(t)
	old := t0.Add(-10 * 24 * time.Hour)
	plist := func(dir string, content []byte, mt time.Time) storagetest.Node {
		return storagetest.Node{Path: dir + "/" + timemachine.SnapshotHistoryFile, Content: content, ModTime: mt}
	}
	dated := func(nodes []storagetest.Node) []storagetest.Node {
		for i := range nodes {
			if nodes[i].ModTime.IsZero() {
				nodes[i].ModTime = old
			}
		}
		return nodes
	}
	var nodes []storagetest.Node
	nodes = append(nodes, dated(append(storagetest.BundleNodes("b1.sparsebundle"),
		plist("b1.sparsebundle", storagetest.SnapshotHistoryXML(t0.Add(-6*24*time.Hour)), old)))...)
	nodes = append(nodes, dated(append(storagetest.BundleNodes("b2.sparsebundle"),
		plist("b2.sparsebundle", storagetest.SnapshotHistoryXML(t0.Add(-8*24*time.Hour)), old)))...)
	nodes = append(nodes, dated(append(storagetest.BundleNodes("b3.sparsebundle"),
		plist("b3.sparsebundle", []byte(storagetest.BinaryPlistMagic+"\x00\x01"), t0.Add(-9*24*time.Hour)),
		storagetest.Node{Path: "b3.sparsebundle/com.apple.TimeMachine.Results.plist", Content: []byte("r"),
			ModTime: t0.Add(-8 * 24 * time.Hour)}))...)
	nodes = append(nodes, storagetest.Node{Path: "b4.sparsebundle", Type: storagetest.Dir},
		storagetest.Node{Path: "b4.sparsebundle/bands", Type: storagetest.Dir},
		storagetest.Node{Path: "b4.sparsebundle/lock", Content: []byte{}},
		storagetest.Node{Path: "b4.sparsebundle/token", Content: []byte{}})
	storagetest.Build(t, share, nodes...)
	return share
}

// TC-STG-164: backup-stale, backup-failed, timemachine-stale and timemachine-share-full. Owner WI 4.4. Requirements: FR-15, FR-10, FR-11.
func TestTCSTG164_BackupStaleBackupFailedTimemachineStaleAnd(t *testing.T) {
	tree := vzTree(t, summaryTree(errLog(106))...)
	share := tc157Share(t)
	s := newHStack(t, hopts{vzdump: []string{tree}, tm: []string{share}, mod: func(c *config.StorageConfig) {
		c.Backups.MaxAge = 192 * time.Hour
		c.Backups.ExpectedGuests = []int{101, 999}
		c.TimeMachine.MaxAge = 168 * time.Hour
	}})
	base := []sig{
		{signalBackupStale, apitypes.SeverityWarning, "guest:lxc/106"},
		{signalBackupStale, apitypes.SeverityWarning, "guest:999"},
		{signalBackupFailed, apitypes.SeverityCritical, "guest:lxc/106"},
		{signalTMStale, apitypes.SeverityWarning, "bundle:" + share + "/b2"},
		{signalTMStale, apitypes.SeverityWarning, "bundle:" + share + "/b3"},
		{signalTMStale, apitypes.SeverityWarning, "bundle:" + share + "/b4"},
	}
	for _, c := range []struct {
		used uint64
		sev  string
	}{{84, ""}, {85, apitypes.SeverityWarning}, {94, apitypes.SeverityWarning}, {95, apitypes.SeverityCritical}} {
		s.sf.set(share, Usage{Blocks: 100, Bfree: 100 - c.used, Bavail: 100 - c.used, Bsize: 1 << 20})
		h := s.health(t)
		want := slices.Clone(base)
		if c.sev != "" {
			want = append(want, sig{signalTMShareFull, c.sev, "share:" + share})
		}
		t.Logf("usage %d%%", c.used)
		wantSigs(t, h, want...)
		if !slices.Equal(h.Checked, spec.Array[string]{featureBackups, featureTimeMachine}) {
			t.Errorf("checked %v, want backups and timeMachine", h.Checked)
		}
	}
	h := s.health(t)
	if m := signalOf(t, h, signalBackupStale, "guest:999").Message; !strings.Contains(m, "999") {
		t.Errorf("guest 999 message %q", m)
	}
}

// TC-STG-165: an unavailable feature contributes no signals and is named. Owner WI 4.4. Requirements: FR-15, NFR-7.
func TestTCSTG165_AnUnavailableFeatureContributesNoSignalsAnd(t *testing.T) {
	stale := t0.Add(-10 * 24 * time.Hour)
	tree := vzTree(t, vz{typ: spec.GuestQemu, vmid: 101, stamp: stale, mtime: stale, size: 1, log: okLog(101)})

	s := newHStack(t, hopts{vzdump: []string{tree}})
	h := s.health(t)
	wantSigs(t, h, sig{signalBackupStale, apitypes.SeverityWarning, "guest:qemu/101"})
	if !slices.Equal(h.Checked, spec.Array[string]{featureBackups}) {
		t.Errorf("checked %v, want [backups]", h.Checked)
	}
	if want := s.p.feats.get(featureZFS).Reason; want == "" || h.Unavailable[featureZFS] != want {
		t.Errorf("unavailable.zfs %q, want the preflight reason %q", h.Unavailable[featureZFS], want)
	}
	if h.Counts != (apitypes.SignalCounts{Warning: 1}) {
		t.Errorf("counts %+v, want the one backup warning", h.Counts)
	}

	_, file, _ := dirs(t)
	z := newHStack(t, hopts{zfs: true, vzdump: []string{file}, setup: func(f *storagetest.Fake, o *overlay) {
		answerList(t, f, o, func(pools map[string]any) { setValue(obj(pools["z1"]), "health", "DEGRADED") })
	}})
	h = z.health(t)
	wantSigs(t, h, sig{signalPoolNotOnline, apitypes.SeverityCritical, "pool:z1"})
	if !slices.Equal(h.Checked, spec.Array[string]{featureZFS}) {
		t.Errorf("checked %v, want [zfs]", h.Checked)
	}
	if want := z.p.feats.get(featureBackups).Reason; want == "" || h.Unavailable[featureBackups] != want {
		t.Errorf("unavailable.backups %q, want %q", h.Unavailable[featureBackups], want)
	}
	if _, ok := h.Unavailable[featureZFS]; ok {
		t.Errorf("unavailable.zfs %q, want zfs read", h.Unavailable[featureZFS])
	}
}

// overQuota is n datasets z1/q<i> (the name padded to width bytes when
// width > 0), each at 90% of a 1000-byte quota, from z2/d1/timemachine's item.
func overQuota(t testing.TB, count, width int) func(ds map[string]any) {
	return func(ds map[string]any) {
		tmpl := ds["z2/d1/timemachine"]
		clear(ds)
		for i := range count {
			name := fmt.Sprintf("z1/q%04d", i)
			if width > len(name) {
				name += strings.Repeat("x", width-len(name))
			}
			item := clone(t, tmpl)
			item["name"], item["pool"] = name, "z1"
			setValue(item, "quota", n(1000))
			setValue(item, "used", n(900))
			ds[name] = item
		}
	}
}

// TC-STG-166: signals are sorted, capped at 500 and counted in full. Owner WI 4.4. Requirements: FR-15, NFR-3.
func TestTCSTG166_SignalsAreSortedCappedAt500And(t *testing.T) {
	s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
		answerDatasets(t, f, o, overQuota(t, 600, 0))
		answerList(t, f, o, func(pools map[string]any) { setValue(obj(pools["z1"]), "health", "DEGRADED") })
	}})
	h := s.health(t)
	if len(h.Signals) != spec.MaxSignals || !h.SignalsTruncated {
		t.Fatalf("%d signals, truncated %v; want 500 and true", len(h.Signals), h.SignalsTruncated)
	}
	if h.Counts.Warning != 600 || h.Counts.Critical < 1 {
		t.Errorf("counts %+v, want warning 600 and critical at least 1", h.Counts)
	}
	if g := h.Signals[0]; g.Severity != apitypes.SeverityCritical || g.ID != signalPoolNotOnline || g.Subject != "pool:z1" {
		t.Errorf("first signal %+v, want the critical pool-not-online of z1", g)
	}
	rank := map[string]int{apitypes.SeverityCritical: 0, apitypes.SeverityWarning: 1, apitypes.SeverityInfo: 2}
	sorted := slices.IsSortedFunc(h.Signals, func(a, b apitypes.HealthSignal) int {
		if c := rank[a.Severity] - rank[b.Severity]; c != 0 {
			return c
		}
		if c := strings.Compare(a.ID, b.ID); c != 0 {
			return c
		}
		return strings.Compare(a.Subject, b.Subject)
	})
	if !sorted {
		t.Error("signals are not sorted critical first, then by id and subject")
	}
	if last := h.Signals[len(h.Signals)-1]; last.Subject != fmt.Sprintf("dataset:z1/q%04d", spec.MaxSignals-1-h.Counts.Critical) {
		t.Errorf("last signal %+v, want the lowest-named over-quota datasets kept", last)
	}

	rep, err := s.p.HealthSignals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts != h.Counts || !rep.Truncated || !slices.Equal(rep.Signals, []apitypes.HealthSignal(h.Signals)) {
		t.Errorf("HealthSignals counts %+v truncated %v (%d signals), want health.get's %+v, true and the same 500",
			rep.Counts, rep.Truncated, len(rep.Signals), h.Counts)
	}
}

// TC-STG-167: the worked example on today's home-server values with default thresholds. Owner WI 4.4. Requirements: FR-15, FR-16, ASM-7.
//
// The tree is TC-STG-145's tree b (the captured /media/vzdump listing), the
// shares TC-STG-152's (the captured df figures of /media/timemachine and
// /media/timemachine2), the clock the capture's date. The expected set is
// FR-15's rules applied to the text oracles of the capture.
func TestTCSTG167_TheWorkedExampleOnTodaySHome(t *testing.T) {
	now := time.Date(2026, 11, 19, 15, 35, 31, 0, time.UTC) // PROVENANCE date
	day := 24 * time.Hour
	d := config.Default().Providers.Storage.Settings
	th := d.Thresholds

	// The backup tree.
	top, err := storagetest.ParseListing(home(t, "shares/1/ls.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dump, err := storagetest.ParseListing(home(t, "shares/2/ls.txt"))
	if err != nil {
		t.Fatal(err)
	}
	tree := storagetest.TempDir(t)
	nodes, err := storagetest.ListingNodes(top, "")
	if err != nil {
		t.Fatal(err)
	}
	dumpNodes, err := storagetest.ListingNodes(dump, "dump")
	if err != nil {
		t.Fatal(err)
	}
	const newest = "vzdump-lxc-103-2026_08_27-10_34_54"
	logHead := home(t, "vzdump/newest-log-head.txt")
	for i, nd := range dumpNodes {
		if nd.Path == "dump/"+newest+".log" {
			dumpNodes[i].Content = logHead
		}
	}
	storagetest.Build(t, tree, append(nodes, dumpNodes...)...)

	// The shares.
	base := storagetest.TempDir(t)
	tm1, tm2 := filepath.Join(base, "tm1"), filepath.Join(base, "tm2")
	storagetest.Build(t, base, storagetest.Node{Path: "tm1", Type: storagetest.Dir},
		storagetest.Node{Path: "tm2", Type: storagetest.Dir})
	u1, _ := dfUsage(t, "shares/3/df.txt")
	u2, _ := dfUsage(t, "shares/4/df.txt")

	s := newHStack(t, hopts{zfs: true, vzdump: []string{tree}, tm: []string{tm1, tm2}, now: now})
	s.sf.set(tm1, u1)
	s.sf.set(tm2, u2)

	// FR-15 over the oracles.
	var want []sig
	for name, p := range homePools(t) {
		subject := "pool:" + name
		if p.health != "ONLINE" {
			want = append(want, sig{signalPoolNotOnline, apitypes.SeverityCritical, subject})
		}
		if e := p.vdevErrors(); e.Read+e.Write+e.Checksum+p.permanentErrors > 0 {
			want = append(want, sig{signalPoolErrors, apitypes.SeverityCritical, subject})
		}
		switch {
		case p.capacity >= th.PoolCapacityCrit:
			want = append(want, sig{signalPoolCapacity, apitypes.SeverityCritical, subject})
		case p.capacity >= th.PoolCapacityWarn:
			want = append(want, sig{signalPoolCapacity, apitypes.SeverityWarning, subject})
		}
		if now.Sub(p.end) > time.Duration(th.ScrubMaxAgeDays)*day {
			want = append(want, sig{signalScrubOverdue, apitypes.SeverityWarning, subject})
		}
		if p.errorsRepaired+p.bytes > 0 {
			want = append(want, sig{signalScrubErrors, apitypes.SeverityCritical, subject})
		}
	}
	for _, r := range tabRows(home(t, "zfs/zfs-list-datasets.txt")) {
		used, refer, quota, refquota := atoi(t, r[2]), atoi(t, r[4]), atoi(t, r[6]), atoi(t, r[7])
		var pct float64
		switch {
		case quota > 0:
			pct = float64(used) / float64(quota) * 100
		case refquota > 0:
			pct = float64(refer) / float64(refquota) * 100
		default:
			continue
		}
		if pct >= float64(th.QuotaWarn) {
			want = append(want, sig{signalDatasetQuota, apitypes.SeverityWarning, "dataset:" + r[0]})
		}
	}
	type guest struct {
		typ  string
		vmid int
	}
	latest := map[guest]*vzOracle{}
	seen := map[guest]bool{}
	for _, o := range append(oracleItems(dump, tree+"/dump"), oracleItems(top, tree)...) {
		g := guest{o.guestType, o.vmid}
		seen[g] = true
		if o.inProgress {
			continue
		}
		if l := latest[g]; l == nil || o.mt.After(l.mt) {
			latest[g] = &o
		}
	}
	for g := range seen {
		subject := fmt.Sprintf("guest:%s/%d", g.typ, g.vmid)
		l := latest[g]
		if l == nil || now.Sub(l.mt) > d.Backups.MaxAge {
			want = append(want, sig{signalBackupStale, apitypes.SeverityWarning, subject})
		}
		if l != nil && l.file == newest+".tar.zst" {
			if st, _ := oracleLog(logHead); st == vzdump.LogError {
				want = append(want, sig{signalBackupFailed, apitypes.SeverityCritical, subject})
			}
		}
	}
	for path, u := range map[string]Usage{tm1: u1, tm2: u2} {
		used := u.Blocks - u.Bfree
		pct := float64(used) / float64(used+u.Bavail) * 100
		switch {
		case pct >= float64(th.ShareCrit):
			want = append(want, sig{signalTMShareFull, apitypes.SeverityCritical, "share:" + path})
		case pct >= float64(th.ShareWarn):
			want = append(want, sig{signalTMShareFull, apitypes.SeverityWarning, "share:" + path})
		}
	}

	h := s.health(t)
	wantSigs(t, h, want...)
	for _, sg := range h.Signals {
		if strings.Contains(sg.Subject, "timemachine") || sg.Subject == "share:"+tm2 {
			t.Errorf("/media/timemachine2 at about 83%% raised %v", sg)
		}
	}
	if !slices.Equal(h.Checked, spec.Array[string]{featureZFS, featureBackups, featureTimeMachine}) || len(h.Unavailable) != 0 {
		t.Errorf("checked %v unavailable %v, want every feature read", h.Checked, h.Unavailable)
	}
	// The worked example, for the WI evidence (A-19 sign-off).
	for _, sg := range h.Signals {
		t.Logf("worked example: %s %s %s: %s", sg.Severity, sg.ID, sg.Subject, sg.Message)
	}
	t.Logf("worked example counts: %+v", h.Counts)
}

// TC-STG-168: HealthSignals returns the same report as storage.health.get and honours a short deadline. Owner WI 4.4. Requirements: FR-16, NFR-2.
func TestTCSTG168_HealthSignalsReturnsTheSameReportAsStorage(t *testing.T) {
	stale := t0.Add(-10 * 24 * time.Hour)
	tree := vzTree(t, vz{typ: spec.GuestQemu, vmid: 101, stamp: stale, mtime: stale, size: 1, log: errLog(101)})
	s := newHStack(t, hopts{zfs: true, vzdump: []string{tree}, setup: func(f *storagetest.Fake, o *overlay) {
		answerList(t, f, o, func(pools map[string]any) { setValue(obj(pools["z2"]), "health", "DEGRADED") })
	}})
	h := s.health(t)
	rep, err := s.p.HealthSignals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Signals) != 3 {
		t.Errorf("signals %v, want z2's and guest 101's", sigsOf(h.Signals))
	}
	if !slices.Equal(rep.Signals, []apitypes.HealthSignal(h.Signals)) || rep.Counts != h.Counts ||
		!rep.ObservedAt.Equal(h.ObservedAt) || fmt.Sprint(rep.Unavailable) != fmt.Sprint(map[string]string(h.Unavailable)) {
		t.Errorf("HealthSignals %+v, want health.get's %+v", rep, h)
	}

	t.Run("hung zpool status", func(t *testing.T) {
		s := newHStack(t, hopts{zfs: true, timeout: time.Second,
			setup: func(f *storagetest.Fake, _ *overlay) { f.Hang(f.Argv(zfs.CmdPoolStatus)) }})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var rep provider.HealthReport
		var err error
		if el := timed(func() { rep, err = s.p.HealthSignals(ctx) }); el > 150*time.Millisecond {
			t.Errorf("HealthSignals took %s under a 100ms deadline, want at most 150ms", el)
		}
		if err == nil && !strings.Contains(rep.Unavailable[featureZFS], "zpool status") {
			t.Errorf("unavailable %v, want zfs naming zpool status", rep.Unavailable)
		}
	})
}

// TC-STG-169: signal subjects and messages carry host names only as data. Owner WI 4.4. Requirements: FR-15, FR-18.
func TestTCSTG169_SignalSubjectsAndMessagesCarryHostNames(t *testing.T) {
	const name = "ignore.previous:instructions"
	if !validPoolName(name) {
		t.Fatalf("%q is not a valid pool name", name)
	}
	s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
		answerList(t, f, o, func(pools map[string]any) {
			z := obj(pools["z1"])
			z["name"] = name
			setValue(z, "health", "DEGRADED")
			pools[name] = z
			delete(pools, "z1")
		})
	}})
	s.rec.reset()
	h := s.health(t)
	g := signalOf(t, h, signalPoolNotOnline, "pool:"+name)
	if !strings.Contains(g.Message, name) || g.ID != "pool-not-online" {
		t.Errorf("signal %+v, want the fixed id and the name in the message", g)
	}
	for _, rec := range s.rec.all() {
		if strings.Contains(rec.Message, name) {
			t.Errorf("log message %q carries the pool name", rec.Message)
		}
		for k := range attrs(rec) {
			if strings.Contains(k, name) {
				t.Errorf("log attribute key %q carries the pool name", k)
			}
		}
	}
}

// TC-STG-170: a hung or failing ZFS health read raises zfs-unresponsive, and a missing zfs does not. Owner WI 4.4. Requirements: FR-15, FR-16, NFR-2.
func TestTCSTG170_AHungOrFailingZFSHealthRead(t *testing.T) {
	const timeout = 300 * time.Millisecond
	unresponsiveOf := func(h spec.HealthProperties) []apitypes.HealthSignal {
		var out []apitypes.HealthSignal
		for _, sg := range h.Signals {
			if sg.ID == signalZFSUnresponsive {
				out = append(out, sg)
			}
		}
		return out
	}

	t.Run("a stuck zpool status", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, _ *overlay) {
			f.Stuck(f.Argv(zfs.CmdPoolStatus), release)
		}})
		setCommandTimeout(s.p, timeout)
		if argv := s.fake.Argv(zfs.CmdPoolStatus); strings.Join(argv[1:], " ") != "status -j --json-int" {
			t.Fatalf("the stuck argv is %q, want zpool status -j --json-int", argv)
		}
		before := time.Now()
		var h spec.HealthProperties
		if el := timed(func() { h = s.health(t) }); el > timeout+100*time.Millisecond {
			t.Errorf("first call took %s, want at most 400ms", el)
		}
		after := time.Now()
		u := unresponsiveOf(h)
		if len(u) != 1 || u[0].Severity != apitypes.SeverityCritical || u[0].Subject != subjectZFS ||
			!strings.Contains(u[0].Message, "zpool status") || !strings.Contains(u[0].Message, "did not finish within 300ms") {
			t.Fatalf("zfs-unresponsive %+v, want one critical with subject zfs naming zpool status and 300ms", u)
		}
		if h.Counts.Critical < 1 || !strings.Contains(h.Unavailable[featureZFS], "zpool status did not finish within 300ms") {
			t.Errorf("counts %+v unavailable %v, want a critical and zfs's reason", h.Counts, h.Unavailable)
		}

		if el := timed(func() { h = s.health(t) }); el >= 50*time.Millisecond {
			t.Errorf("second call took %s, want under 50ms", el)
		}
		u = unresponsiveOf(h)
		if len(u) != 1 || u[0].Subject != subjectZFS || !strings.Contains(u[0].Message, "zpool status is still running since ") {
			t.Fatalf("second zfs-unresponsive %+v, want still running since", u)
		}
		_, since, _ := strings.Cut(u[0].Message, "still running since ")
		at, err := time.Parse(time.RFC3339, since)
		if err != nil || at.Before(before.Truncate(time.Second)) || at.After(after) {
			t.Errorf("still running since %q, want the first call's start (between %s and %s)", since, before, after)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		rep, err := s.p.HealthSignals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, sg := range rep.Signals {
			found = found || sg.ID == signalZFSUnresponsive && sg.Message == u[0].Message
		}
		if !found || rep.Counts.Critical < 1 {
			t.Errorf("HealthSignals %+v, want the same zfs-unresponsive counted critical", rep)
		}
	})

	// (b): the CmdPoolList argv, zpool list -j --json-int -o
	// name,size,…,readonly (A-58), exits 1.
	t.Run("zpool list exits 1", func(t *testing.T) {
		stderr := homeStderr(t, "zfs/negative-zpool-list.json")
		s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
			o.answer(f.Argv(zfs.CmdPoolList), zfs.Result{ExitCode: 1, Stderr: stderr})
		}})
		if argv := s.fake.Argv(zfs.CmdPoolList); !slices.Contains(argv, zfs.PoolListColumns) {
			t.Fatalf("CmdPoolList argv %q lacks -o %s (A-58)", argv, zfs.PoolListColumns)
		}
		h := s.health(t)
		first, _, _ := strings.Cut(string(stderr), "\n")
		u := unresponsiveOf(h)
		if len(u) != 1 || u[0].Severity != apitypes.SeverityCritical || !strings.Contains(u[0].Message, "zpool list") ||
			!strings.Contains(u[0].Message, "exited 1") || !strings.Contains(u[0].Message, first) || len(first) > 200 {
			t.Errorf("zfs-unresponsive %+v, want one naming zpool list, exited 1 and %q", u, first)
		}
	})

	t.Run("a long stderr line is cut at 200 bytes", func(t *testing.T) {
		line := strings.Repeat("é", 150) // 300 bytes
		s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
			o.answer(f.Argv(zfs.CmdPoolList), zfs.Result{ExitCode: 1, Stderr: []byte(line + "\nsecond line\n")})
		}})
		u := unresponsiveOf(s.health(t))
		if len(u) != 1 {
			t.Fatalf("zfs-unresponsive %+v, want one", u)
		}
		_, got, _ := strings.Cut(u[0].Message, "exited 1: ")
		if got != line[:200] || strings.Contains(u[0].Message, "second line") {
			t.Errorf("stderr in the message %q (%d bytes), want the first line's first 200 bytes", got, len(got))
		}
	})

	t.Run("zfs unavailable at preflight", func(t *testing.T) {
		h := newHStack(t, hopts{vzdump: []string{vzTree(t, three(101)...)}}).health(t)
		if u := unresponsiveOf(h); len(u) != 0 || h.Unavailable[featureZFS] == "" {
			t.Errorf("zfs-unresponsive %+v unavailable %v, want none and zfs named", u, h.Unavailable)
		}
	})

	t.Run("a parse failure", func(t *testing.T) {
		list := home(t, "zfs/zpool-list.json")
		s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
			o.answer(f.Argv(zfs.CmdPoolList), zfs.Result{Stdout: list[:len(list)/2]})
		}})
		h := s.health(t)
		if u := unresponsiveOf(h); len(u) != 0 || !strings.Contains(h.Unavailable[featureZFS], "could not be parsed") {
			t.Errorf("zfs-unresponsive %+v unavailable %v, want none and the parse failure as zfs's reason", u, h.Unavailable)
		}
	})

	t.Run("the subject rule", func(t *testing.T) {
		zp := "/usr/sbin/zpool"
		for _, c := range []struct {
			argv []string
			want string
		}{
			{[]string{zp, "status", "-j", "--json-int", "z1"}, "pool:z1"},
			{[]string{zp, "status", "-j", "--json-int"}, subjectZFS},
			{[]string{zp, "list", "-j", "--json-int", "-o", zfs.PoolListColumns}, subjectZFS},
			{[]string{zp, "list", "-H", "-o", "name"}, subjectZFS},
			{[]string{"/usr/sbin/zfs", "list", "-H", "-p", "-t", "snapshot", "-o", "name,creation", "-d", "1", "z2/d1/vms"},
				"pool:z2"},
			{[]string{"/usr/sbin/zfs", "version"}, subjectZFS},
		} {
			if got := unresponsiveSubject(c.argv); got != c.want {
				t.Errorf("subject of %q = %q, want %q", c.argv, got, c.want)
			}
		}
	})
}

// TC-STG-171: HealthSignals re-checks a failed zfs feature after 5 s, with no direct storage call. Owner WI 4.4. Requirements: FR-3, FR-16, NFR-7.
func TestTCSTG171_HealthSignalsReChecksAFailedZfsFeature(t *testing.T) {
	var hang atomic.Bool
	hang.Store(true)
	var hung atomic.Int32
	tree := vzTree(t, three(101)...)
	s := newHStack(t, hopts{zfs: true, vzdump: []string{tree},
		mod: func(c *config.StorageConfig) { c.CommandTimeout = 200 * time.Millisecond },
		setup: func(f *storagetest.Fake, o *overlay) {
			o.answerFunc(f.Argv(zfs.CmdPoolNames), func(ctx context.Context, argv []string) (zfs.Result, error) {
				if hang.Load() {
					<-ctx.Done()
					hung.Add(1)
					return zfs.Result{}, ctx.Err()
				}
				return f.Run(ctx, argv)
			})
			answerList(t, f, o, func(pools map[string]any) { setValue(obj(pools["z1"]), "health", "DEGRADED") })
		}})
	names := s.fake.Argv(zfs.CmdPoolNames)
	before := s.p.feats.get(featureZFS)
	if before.Available || !strings.Contains(before.Reason, "zpool list") {
		t.Fatalf("zfs after the preflight at T0: %+v, want unavailable with the hang's reason", before)
	}
	waitFor(t, func() bool { return hung.Load() == 1 })
	hang.Store(false)
	s.rec.reset()

	s.clk.set(t0.Add(4 * time.Second))
	rep, err := s.p.HealthSignals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unavailable[featureZFS] != before.Reason || countArgv(s.ov.argvs(), names) != 1 {
		t.Errorf("at T0+4s: unavailable.zfs %q and %d zpool list -H -o name, want the preflight reason and no new one",
			rep.Unavailable[featureZFS], countArgv(s.ov.argvs(), names))
	}

	s.clk.set(t0.Add(6 * time.Second))
	reps := make([]provider.HealthReport, 5)
	var wg sync.WaitGroup
	for i := range reps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reps[i], _ = s.p.HealthSignals(context.Background())
		}()
	}
	wg.Wait()
	if n := countArgv(s.ov.argvs(), names); n != 2 {
		t.Errorf("%d zpool list -H -o name in all, want exactly one new", n)
	}
	for i, r := range reps {
		if _, ok := r.Unavailable[featureZFS]; ok {
			t.Errorf("report %d: unavailable.zfs %q, want zfs read", i, r.Unavailable[featureZFS])
		}
		if got := sigsOf(r.Signals); !slices.Equal(got, []sig{{signalPoolNotOnline, apitypes.SeverityCritical, "pool:z1"}}) {
			t.Errorf("report %d signals %v, want the pool rules' result", i, got)
		}
	}
	var changed int
	for _, rec := range s.rec.withMessage("storage feature availability changed") {
		if a := attrs(rec); a["feature"] == featureZFS && a["available"] == true {
			changed++
		}
	}
	if changed != 1 {
		t.Errorf("%d info records that zfs became available, want 1", changed)
	}
}

// serveStorage serves s's registry through the real REST server and returns
// a GET that reads a route's raw body with a storage:read token.
func serveStorage(t *testing.T, s *hstack) func(path string) []byte {
	t.Helper()
	stateDir := t.TempDir()
	token, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "reader", Scopes: []string{spec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(s.reg, server.Options{Logger: discard, Auth: auth.New(stateDir, auth.Options{Logger: discard})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 2 * time.Minute}
	return func(path string) []byte {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, res.StatusCode, raw)
		}
		return raw
	}
}

// routeOf is op's REST path with its {wildcard} set to name.
func routeOf(t *testing.T, op def.Op, name string) string {
	t.Helper()
	rt, err := server.RouteFor(op)
	if err != nil {
		t.Fatal(err)
	}
	if i := strings.Index(rt.Path, "{"); i >= 0 {
		j := strings.Index(rt.Path, "}")
		return rt.Path[:i] + name + rt.Path[j+1:]
	}
	return rt.Path
}

// decodeItem is the properties of a raw list body's one item.
func decodeItem[P any](t *testing.T, raw []byte) P {
	t.Helper()
	var l apitypes.List[apitypes.Resource[P]]
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 {
		t.Fatalf("%d items, want 1", len(l.Items))
	}
	return l.Items[0].Properties
}

// within1MiB fails, naming the case, when a raw body is over NFR-3's bound:
// a finding for the architect (a missing cap), not a test to relax.
func within1MiB(t *testing.T, name string, raw []byte) {
	t.Helper()
	t.Logf("case %s: %d bytes", name, len(raw))
	if len(raw) > 1<<20 {
		t.Errorf("case %s: the body is %d bytes, over 1 MiB with every documented cap applied (a missing cap)",
			name, len(raw))
	}
}

// pad is s followed by x up to n bytes.
func pad(s string, n int) string { return s + strings.Repeat("x", max(n-len(s), 0)) }

// TC-STG-172: every maximal response body stays within 1 MiB. Owner WI 4.4. Requirements: NFR-3, FR-5, FR-8, FR-10, FR-11, FR-15.
//
// (a): spec.MaxDirEntries reads at most 10,000 entries of `.` and of dump/,
// so 10,000 archives each with its .notes cannot sit in dump/ alone, nor in
// the two directories (20,000 files and dump itself are 20,001 entries):
// each tree holds 5,000 archives in `.` and 5,000 in dump/, every one with
// its notes but the first, 10,000 recognized archives per path with no
// listing truncated. The summary lists
// guests by guestType then vmid, so its first 500 are the expected guests
// with no archive (guestType ""); (a2) is the same without expected guests,
// whose first 500 carry the 1 KiB names.
func TestTCSTG172_EveryMaximalResponseBodyStaysWithin1(t *testing.T) {
	name1K := strings.Repeat("€", spec.NotesMaxBytes/3) // 1023 bytes of 3-byte runes
	at := t0.Add(-time.Hour)
	stamp := at.Format("2006_01_02-15_04_05")

	t.Run("a backups.summary", func(t *testing.T) {
		if testing.Short() {
			t.Skip("40,000 archives")
		}
		var trees []string
		vmid := 100
		for range 4 {
			tree := storagetest.TempDir(t)
			nodes := make([]storagetest.Node, 0, 20000)
			for i := range 10000 {
				dir := ""
				if i >= 5000 {
					dir = "dump/"
				}
				file := fmt.Sprintf("vzdump-lxc-%d-%s.tar.zst", vmid, stamp)
				nodes = append(nodes, storagetest.Node{Path: dir + file, Size: 1, ModTime: at})
				if i != 0 {
					nodes = append(nodes, storagetest.Node{Path: dir + file + ".notes", Content: []byte(name1K + "\n"),
						ModTime: at})
				}
				vmid++
			}
			storagetest.Build(t, tree, nodes...)
			trees = append(trees, tree)
		}
		var expected []int
		for i := range 1000 {
			expected = append(expected, 900000+i)
		}
		s := newHStack(t, hopts{vzdump: trees, mod: func(c *config.StorageConfig) { c.Backups.ExpectedGuests = expected }})
		get := serveStorage(t, s)
		raw := get(routeOf(t, spec.BackupsSummary.Op(), ""))
		within1MiB(t, "a", raw)
		sum := decodeItem[spec.BackupSummaryProperties](t, raw)
		if len(sum.Guests) != spec.MaxSummaryGuests || !sum.GuestsTruncated || sum.GuestCount != 41000 ||
			sum.StaleCount != 1000 || sum.FailedCount != 0 {
			t.Errorf("%d guests, truncated %v, guestCount %d, staleCount %d, failedCount %d; want 500, true, 41000, 1000, 0",
				len(sum.Guests), sum.GuestsTruncated, sum.GuestCount, sum.StaleCount, sum.FailedCount)
		}
		if !slices.IsSortedFunc(sum.Guests, func(x, y spec.GuestBackups) int {
			if c := strings.Compare(x.GuestType, y.GuestType); c != 0 {
				return c
			}
			return x.VMID - y.VMID
		}) {
			t.Error("guests are not sorted by guestType then vmid")
		}
		if len(sum.Directories) != 4 {
			t.Errorf("directories %+v, want the 4 paths", sum.Directories)
		}
		for _, d := range sum.Directories {
			if !d.Available || d.Archives != 10000 || d.Truncated {
				t.Errorf("directory %+v, want 10,000 archives and no truncation", d)
			}
		}
	})

	t.Run("a2 backups.summary with 1 KiB names shown", func(t *testing.T) {
		tree := storagetest.TempDir(t)
		var nodes []storagetest.Node
		for i := range spec.MaxSummaryGuests + 1 {
			file := fmt.Sprintf("vzdump-lxc-%d-%s.tar.zst", 100+i, stamp)
			nodes = append(nodes, storagetest.Node{Path: "dump/" + file, Size: 1, ModTime: at},
				storagetest.Node{Path: "dump/" + file + ".notes", Content: []byte(name1K + "\n"), ModTime: at})
		}
		storagetest.Build(t, tree, nodes...)
		s := newHStack(t, hopts{vzdump: []string{tree}})
		raw := serveStorage(t, s)(routeOf(t, spec.BackupsSummary.Op(), ""))
		within1MiB(t, "a2", raw)
		sum := decodeItem[spec.BackupSummaryProperties](t, raw)
		if len(sum.Guests) != spec.MaxSummaryGuests || !sum.GuestsTruncated || sum.Guests[0].GuestName == nil ||
			*sum.Guests[0].GuestName != name1K {
			t.Errorf("%d guests, truncated %v, first name %v; want 500, true and the 1 KiB name", len(sum.Guests),
				sum.GuestsTruncated, sum.Guests[0].GuestName)
		}
	})

	t.Run("b storage.health.get", func(t *testing.T) {
		s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
			answerDatasets(t, f, o, overQuota(t, 600, spec.MaxNameLength))
			answerList(t, f, o, func(pools map[string]any) {
				tmpl := pools["z1"]
				clear(pools)
				for i := range 50 {
					name := pad(fmt.Sprintf("p%02d", i), spec.MaxNameLength)
					item := clone(t, tmpl)
					item["name"] = name
					setValue(item, "health", "DEGRADED")
					pools[name] = item
				}
			})
		}})
		raw := serveStorage(t, s)(routeOf(t, spec.HealthGet.Op(), ""))
		within1MiB(t, "b", raw)
		h := decodeItem[spec.HealthProperties](t, raw)
		if len(h.Signals) != spec.MaxSignals || !h.SignalsTruncated || h.Counts.Critical != 50 || h.Counts.Warning != 600 {
			t.Errorf("%d signals, truncated %v, counts %+v; want 500, true, 50 critical and 600 warnings",
				len(h.Signals), h.SignalsTruncated, h.Counts)
		}
	})

	t.Run("c snapshots.list", func(t *testing.T) {
		var listing bytes.Buffer
		for i := range 600 {
			name := pad(fmt.Sprintf("z1/ds/appdata@s%04d-", i), spec.MaxSnapshotNameBytes)
			fmt.Fprintf(&listing, "%s\t%d\t%d\t%d\t%d\t%s\n", name, 1790000000+i, uint64(1)<<60, uint64(1)<<60, 1<<30,
				"op-0123456789abcdef")
		}
		s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
			o.answer(f.Argv(zfs.CmdSnapshotList), zfs.Result{Stdout: listing.Bytes()})
		}})
		raw := serveStorage(t, s)(routeOf(t, spec.SnapshotsList.Op(), "") + "?limit=500")
		within1MiB(t, "c", raw)
		var l apitypes.List[spec.Snapshot]
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		if len(l.Items) != 500 || l.NextCursor == "" {
			t.Errorf("%d snapshots, next %q; want 500 and a cursor", len(l.Items), l.NextCursor)
		}
	})

	t.Run("d pools.get", func(t *testing.T) {
		text := strings.Repeat("a long status line.\n\t", 200) // Over 2 KiB
		s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
			dec := json.NewDecoder(bytes.NewReader(synthetic(t, "zpool-status-permanent-errors-60.json")))
			dec.UseNumber()
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				t.Fatal(err)
			}
			pools := obj(doc["pools"])
			tank := obj(pools["tank"])
			delete(pools, "tank")
			tank["name"], tank["status"], tank["action"] = "z1", text, text
			var errlist []any
			for i := range 60 {
				errlist = append(errlist, pad(fmt.Sprintf("/z1/data/file-%02d-", i), 4096))
			}
			tank["errlist"] = errlist
			vdevs := obj(tank["vdevs"])
			for k, v := range vdevs {
				delete(vdevs, k)
				obj(v)["name"] = "z1"
				vdevs["z1"] = v
			}
			pools["z1"] = tank
			o.answer(f.Argv(zfs.CmdPoolStatusVerbose, "z1"), zfs.Result{Stdout: encodeJSON(t, doc)})
		}})
		raw := serveStorage(t, s)(routeOf(t, spec.PoolsGet.Op(), "z1"))
		within1MiB(t, "d", raw)
		var p spec.Pool
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		pp := p.Properties
		if len(pp.PermanentErrorPaths) != spec.MaxErrorPaths || !pp.PermanentErrorsTruncated || pp.Status == nil ||
			!strings.HasSuffix(*pp.Status, "…[truncated]") || pp.Action == nil || !strings.HasSuffix(*pp.Action, "…[truncated]") {
			t.Errorf("paths %d truncated %v status %d bytes; want 50, true and both texts truncated",
				len(pp.PermanentErrorPaths), pp.PermanentErrorsTruncated, len(ptrOr(pp.Status)))
		}
	})

	t.Run("e timemachine.list", func(t *testing.T) {
		var shares []string
		for range 4 {
			share := storagetest.TempDir(t)
			var nodes []storagetest.Node
			for i := range 300 {
				dir := pad(fmt.Sprintf("mac-%03d-", i), spec.MaxNameLength-len(".sparsebundle")) + ".sparsebundle"
				nodes = append(nodes, storagetest.Node{Path: dir, Type: storagetest.Dir},
					storagetest.Node{Path: dir + "/" + timemachine.SnapshotHistoryFile,
						Content: storagetest.SnapshotHistoryXML(at), ModTime: at})
			}
			storagetest.Build(t, share, nodes...)
			shares = append(shares, share)
		}
		s := newHStack(t, hopts{tm: shares})
		raw := serveStorage(t, s)(routeOf(t, spec.TimeMachineList.Op(), ""))
		within1MiB(t, "e", raw)
		var l apitypes.List[spec.Share]
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		if len(l.Items) != 4 {
			t.Fatalf("%d shares, want 4", len(l.Items))
		}
		for _, sh := range l.Items {
			if len(sh.Properties.Bundles) != spec.MaxBundles || !sh.Properties.BundlesTruncated {
				t.Errorf("share %s: %d bundles, truncated %v; want 256 and true", sh.Properties.Path,
					len(sh.Properties.Bundles), sh.Properties.BundlesTruncated)
			}
		}
	})

	t.Run("f backups.list", func(t *testing.T) {
		var log bytes.Buffer
		for i := range spec.MaxLogErrorLines + 2 {
			log.WriteString(pad(fmt.Sprintf("2026-11-17 03:04:%02d ERROR: line %d ", i, i), 600) + "\n")
		}
		tree := storagetest.TempDir(t)
		var nodes []storagetest.Node
		for i := range 600 {
			a := vz{typ: spec.GuestQemu, vmid: 100 + i, stamp: at, mtime: at, size: 1 << 40,
				notes: []byte(name1K + "\n"), log: log.Bytes()}
			nodes = append(nodes, a.nodes()...)
		}
		storagetest.Build(t, tree, nodes...)
		s := newHStack(t, hopts{vzdump: []string{tree}})
		raw := serveStorage(t, s)(routeOf(t, spec.BackupsList.Op(), "") + "?limit=500")
		within1MiB(t, "f", raw)
		var l apitypes.List[spec.Backup]
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		// A-64: the page ends early under spec.BackupsPageBudgetBytes, so it
		// holds at least one and at most 500 items, with a cursor to the rest.
		if len(l.Items) == 0 || len(l.Items) > 500 || l.NextCursor == "" ||
			len(l.Items[0].Properties.LogErrors) != spec.MaxLogErrorLines {
			t.Errorf("%d backups, next %q, %d log errors on the first; want 1..500, a cursor and 10",
				len(l.Items), l.NextCursor, len(l.Items[0].Properties.LogErrors))
		}
	})
}
