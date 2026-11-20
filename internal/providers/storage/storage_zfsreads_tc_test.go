// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-125 to
// TC-STG-140, TC-STG-E4, TC-STG-E11 and TC-STG-ERR1 to ERR3 and ERR12
// (owner WI 4.2), plus the handler-level halves of TC-STG-114 and
// TC-STG-116 carried forward from WI 4.1.
//
// The bindings land in WI 5.1, so "through the Dispatcher" runs a real
// core.Dispatcher over zfsReadsBound, the provider with the five ZFS reads
// bound here (the host provider's metricsOnly precedent): the schema
// validation, the panic containment and the problem mapping are the real
// ones. Expected values are computed in the test from the home-server text
// captures with minimal independent readers (test-strategy.md), never taken
// from the parsers under test.

package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// ---- test support (WI 4.2) ----

// t0 is F-CLOCK's time: after every snapshot and scrub of the capture.
var t0 = time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

// zfsReadsBound is the provider with the five ZFS reads bound, and
// storage.health.get (WI 4.4, TC-STG-139 step 2), for the Dispatcher before
// the bindings of WI 5.1 exist.
type zfsReadsBound struct{ *Provider }

func (b zfsReadsBound) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.HealthGet, b.healthGet),
		provider.Sync(spec.PoolsList, b.poolsList),
		provider.Sync(spec.PoolsGet, b.poolsGet),
		provider.Sync(spec.DatasetsList, b.datasetsList),
		provider.Sync(spec.DatasetsGet, b.datasetsGet),
		provider.Sync(spec.SnapshotsList, b.snapshotsList),
	}
}

// overlay is a runner over F-ZFS that answers some argv itself (synthetic
// variants of the capture) and records every argv. Like zfs.Exec it turns
// stdout over the cap into zfs.ErrStdoutTooLarge (TC-STG-52 pins Exec's).
type overlay struct {
	fake *storagetest.Fake

	mu      sync.Mutex
	answers map[string]func(ctx context.Context, argv []string) (zfs.Result, error)
	seen    [][]string
}

func newOverlay(fake *storagetest.Fake) *overlay {
	return &overlay{fake: fake, answers: map[string]func(context.Context, []string) (zfs.Result, error){}}
}

func argvKey(argv []string) string { return strings.Join(argv, "\x00") }

func (o *overlay) answer(argv []string, res zfs.Result) {
	o.answerFunc(argv, func(context.Context, []string) (zfs.Result, error) { return res, nil })
}

func (o *overlay) answerFunc(argv []string, fn func(context.Context, []string) (zfs.Result, error)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.answers[argvKey(argv)] = fn
}

func (o *overlay) run(ctx context.Context, argv []string) (zfs.Result, error) {
	o.mu.Lock()
	o.seen = append(o.seen, slices.Clone(argv))
	fn, ok := o.answers[argvKey(argv)]
	o.mu.Unlock()
	var res zfs.Result
	var err error
	if ok {
		res, err = fn(ctx, argv)
	} else {
		res, err = o.fake.Run(ctx, argv)
	}
	if err == nil && len(res.Stdout) > zfs.StdoutCap {
		return zfs.Result{}, fmt.Errorf("%w: %s wrote more than %d bytes", zfs.ErrStdoutTooLarge, argv[0], zfs.StdoutCap)
	}
	return res, err
}

func (o *overlay) argvs() [][]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.seen)
}

// zstack is the provider on home-server F-ZFS behind a Dispatcher, after
// the registry's startup preflight, at F-CLOCK t0.
type zstack struct {
	fake *storagetest.Fake
	ov   *overlay
	p    *Provider
	rec  *records
	clk  *clock
	d    *core.Dispatcher
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// newZStack builds the stack; setup plants the overlay's answers before
// the preflight.
func newZStack(t *testing.T, setup func(*storagetest.Fake, *overlay), opts ...storagetest.Options) *zstack {
	t.Helper()
	fake := storagetest.NewFake(t, "home-server", opts...)
	ov := newOverlay(fake)
	if setup != nil {
		setup(fake, ov)
	}
	clk := &clock{}
	clk.set(t0)
	p, rec := newTestProvider(homeFS(t), ov.run, func(o *Options) { o.Now = clk.Now })
	reg, err := core.New([]provider.Provider{zfsReadsBound{p}}, core.Options{
		Host: "tc", Version: "v0.0.0-test", Logger: discard,
		Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: func(into any) error {
			into.(*config.StorageConfig).Enabled = true
			return nil
		}}},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	if st := p.feats.get(featureZFS); !st.Available {
		t.Fatalf("zfs unavailable after the preflight: %s", st.Reason)
	}
	return &zstack{fake: fake, ov: ov, p: p, rec: rec, clk: clk, d: core.NewDispatcher(reg, core.DispatcherOptions{Logger: discard})}
}

// invoke calls op through the Dispatcher with storage:read.
func (s *zstack) invoke(op def.Op, input string) (any, error) {
	res, err := s.d.Invoke(context.Background(), core.InvokeRequest{
		Caller:  auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "tc-stg-zfs", Scopes: []string{spec.ReadScope}},
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

func call[T any](t *testing.T, s *zstack, op def.Op, input string) T {
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

func listPools(t *testing.T, s *zstack, input string) apitypes.List[spec.Pool] {
	t.Helper()
	return call[apitypes.List[spec.Pool]](t, s, spec.PoolsList.Op(), input)
}

func listDatasets(t *testing.T, s *zstack, input string) apitypes.List[spec.Dataset] {
	t.Helper()
	return call[apitypes.List[spec.Dataset]](t, s, spec.DatasetsList.Op(), input)
}

func listSnapshots(t *testing.T, s *zstack, input string) apitypes.List[spec.Snapshot] {
	t.Helper()
	return call[apitypes.List[spec.Snapshot]](t, s, spec.SnapshotsList.Op(), input)
}

// walk follows nextCursor from input (a JSON object without cursor) to
// the end and returns every page.
func walk[T any](t *testing.T, s *zstack, op def.Op, input string) [][]T {
	t.Helper()
	var pages [][]T
	cursor := ""
	for range 1000 {
		in := input
		if cursor != "" {
			in = strings.TrimSuffix(input, "}") + fmt.Sprintf(`,"cursor":%q}`, cursor)
			in = strings.Replace(in, "{,", "{", 1)
		}
		l := call[apitypes.List[T]](t, s, op, in)
		pages = append(pages, l.Items)
		if l.NextCursor == "" {
			return pages
		}
		cursor = l.NextCursor
	}
	t.Fatalf("%s: no last page after 1000 pages", op.ID)
	return nil
}

// wantSlug is err's problem, failing unless it has slug.
func wantSlug(t *testing.T, err error, slug problem.Slug) *problem.Error {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != slug {
		t.Fatalf("err = %v, want %s", err, slug)
	}
	return pe
}

// wantInvalid fails unless err is validation-failed naming field.
func wantInvalid(t *testing.T, err error, field string) {
	t.Helper()
	pe := wantSlug(t, err, problem.ValidationFailed)
	if pe.Status != 400 {
		t.Errorf("status %d, want 400", pe.Status)
	}
	errs, _ := pe.Extensions["errors"].([]problem.FieldError)
	if !slices.ContainsFunc(errs, func(e problem.FieldError) bool { return e.Path == "/"+field }) {
		t.Errorf("errors %v, want one naming /%s", pe.Extensions["errors"], field)
	}
}

// home is a home-server capture file's stdout.
func home(t testing.TB, name string) []byte {
	t.Helper()
	out, _, err := storagetest.CaptureStdout(filepath.Join("testdata", "captures", "home-server", name))
	if err != nil {
		t.Fatalf("capture %s: %v", name, err)
	}
	return out
}

func homeStderr(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "captures", "home-server", name+".stderr"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func synthetic(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "captures", "synthetic", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// homeZone is the zone PROVENANCE records, which the text dates are in.
func homeZone(t *testing.T) *time.Location {
	t.Helper()
	prov, err := os.ReadFile(filepath.Join("testdata", "captures", "home-server", "PROVENANCE"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(prov), "\n") {
		if z, ok := strings.CutPrefix(l, "timezone: "); ok {
			loc, err := time.LoadLocation(z)
			if err != nil {
				t.Fatal(err)
			}
			return loc
		}
	}
	t.Fatal("PROVENANCE has no timezone")
	return nil
}

func tabRows(b []byte) [][]string {
	var out [][]string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if l != "" {
			out = append(out, strings.Split(l, "\t"))
		}
	}
	return out
}

func atoi(t *testing.T, s string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	return n
}

// jsonMembers is a document's container object, member by member, the
// bytes of each kept as the tool wrote them.
func jsonMembers(t testing.TB, doc []byte, container string) (json.RawMessage, map[string]json.RawMessage) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(top[container], &m); err != nil {
		t.Fatal(err)
	}
	return top["output_version"], m
}

// jsonDoc builds a document of the members in order: name, raw, ….
func jsonDoc(version json.RawMessage, container string, members ...any) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"output_version":%s,%q:{`, version, container)
	for i := 0; i < len(members); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%s", members[i], members[i+1])
	}
	b.WriteString("}}\n")
	return b.Bytes()
}

// spliced is doc with extra members first in its container.
func spliced(t testing.TB, doc []byte, container string, members ...any) []byte {
	t.Helper()
	open := []byte(fmt.Sprintf(`%q:{`, container))
	if !bytes.Contains(doc, open) {
		t.Fatalf("no %s object", container)
	}
	var add bytes.Buffer
	for i := 0; i < len(members); i += 2 {
		fmt.Fprintf(&add, "%q:%s,", members[i], members[i+1])
	}
	return bytes.Replace(doc, open, append(slices.Clone(open), add.Bytes()...), 1)
}

func rename(raw json.RawMessage, from, to string) json.RawMessage {
	return bytes.ReplaceAll(raw, []byte(fmt.Sprintf("%q", from)), []byte(fmt.Sprintf("%q", to)))
}

// setProp sets a {value, source} property's value in a raw item.
func setProp(t testing.TB, raw json.RawMessage, prop string, value any) json.RawMessage {
	t.Helper()
	re := regexp.MustCompile(fmt.Sprintf(`"%s":\{"value":("[^"]*"|[0-9]+)`, regexp.QuoteMeta(prop)))
	if !re.Match(raw) {
		t.Fatalf("no property %s", prop)
	}
	v, _ := json.Marshal(value)
	return re.ReplaceAll(raw, []byte(fmt.Sprintf(`"%s":{"value":%s`, prop, v)))
}

// ---- the text oracle ----

type oracleVdev struct {
	name, state, parent string
	depth               int
	read, write, cksum  uint64
}

type oraclePool struct {
	name, health          string
	size, alloc, free     uint64
	capacity, frag        int
	dedup                 float64
	readOnly              bool
	altroot               *string
	vdevs                 []oracleVdev
	status, action        *string
	scanFn, scanState     string
	start, end            time.Time
	errorsRepaired, bytes uint64
	permanentErrors       uint64
}

var (
	scanDoneRE = regexp.MustCompile(`^  scan: (scrub) repaired (\S+) in ((?:\d+ days )?\d+:\d\d:\d\d) with (\d+) errors on (.+)$`)
	niceRE     = regexp.MustCompile(`^([0-9.]+)([BKMGTPE])$`)
	hmsRE      = regexp.MustCompile(`^(?:(\d+) days )?(\d+):(\d\d):(\d\d)$`)
)

// homePools reads zpool list -H -p (tab split) and zpool status -p -v (the
// scan line by regexp, the vdev rows by strings.Fields) of the capture.
func homePools(t *testing.T) map[string]*oraclePool {
	t.Helper()
	pools := map[string]*oraclePool{}
	for _, r := range tabRows(home(t, "zfs/zpool-list.txt")) {
		p := &oraclePool{name: r[0], health: r[7], size: atoi(t, r[1]), alloc: atoi(t, r[2]), free: atoi(t, r[3]),
			capacity: int(atoi(t, r[4])), frag: int(atoi(t, r[5])), readOnly: r[9] == "on"}
		p.dedup, _ = strconv.ParseFloat(r[6], 64)
		if r[8] != "-" {
			p.altroot = &r[8]
		}
		pools[r[0]] = p
	}
	loc := homeZone(t)
	lines := strings.Split(string(home(t, "zfs/zpool-status-v.txt")), "\n")
	var cur *oraclePool
	cont := func(i *int, first string) *string {
		s := first
		for *i+1 < len(lines) && strings.HasPrefix(lines[*i+1], "\t") {
			*i++
			s += "\n" + lines[*i]
		}
		return &s
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "  pool: "):
			cur = pools[strings.TrimPrefix(l, "  pool: ")]
		case strings.HasPrefix(l, "status: "):
			cur.status = cont(&i, strings.TrimPrefix(l, "status: "))
		case strings.HasPrefix(l, "action: "):
			cur.action = cont(&i, strings.TrimPrefix(l, "action: "))
		case strings.HasPrefix(l, "  scan: "):
			m := scanDoneRE.FindStringSubmatch(l)
			if m == nil {
				t.Fatalf("scan line %q", l)
			}
			cur.scanFn, cur.scanState = m[1], "finished"
			cur.bytes = niceBytes(t, m[2])
			cur.errorsRepaired = atoi(t, m[4])
			end, err := time.ParseInLocation(time.ANSIC, m[5], loc)
			if err != nil {
				t.Fatal(err)
			}
			cur.end = end.UTC()
			h := hmsRE.FindStringSubmatch(m[3])
			days, _ := strconv.Atoi("0" + h[1])
			hh, _ := strconv.Atoi(h[2])
			mm, _ := strconv.Atoi(h[3])
			ss, _ := strconv.Atoi(h[4])
			cur.start = cur.end.Add(-time.Duration(((days*24+hh)*60+mm)*60+ss) * time.Second)
		case l == "config:":
			i += 3 // the blank line and the header
			var parents []string
			for ; i < len(lines) && lines[i] != ""; i++ {
				row := strings.TrimPrefix(lines[i], "\t")
				depth := (len(row) - len(strings.TrimLeft(row, " "))) / 2
				f := strings.Fields(row)
				v := oracleVdev{name: f[0], state: strings.ToLower(f[1]), depth: depth,
					read: atoi(t, f[2]), write: atoi(t, f[3]), cksum: atoi(t, f[4])}
				parents = append(parents[:depth], f[0])
				if depth > 0 {
					v.parent = parents[depth-1]
				}
				cur.vdevs = append(cur.vdevs, v)
			}
		case strings.HasPrefix(l, "errors: "):
			if l != "errors: No known data errors" {
				t.Fatalf("errors line %q", l)
			}
		}
	}
	return pools
}

// niceBytes reads a zfs_nicebytes size; the capture prints only exact ones.
func niceBytes(t *testing.T, s string) uint64 {
	t.Helper()
	m := niceRE.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("size %q", s)
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	return uint64(math.Round(f * math.Pow(1024, float64(strings.Index("BKMGTPE", m[2])))))
}

// vdevErrors is DF-2 step 3's rule over the oracle rows: the leaf sums,
// else the root's own count.
func (p *oraclePool) vdevErrors() spec.ErrorCounts {
	var leaves spec.ErrorCounts
	for i, v := range p.vdevs {
		if i+1 == len(p.vdevs) || p.vdevs[i+1].depth <= v.depth {
			leaves.Read += v.read
			leaves.Write += v.write
			leaves.Checksum += v.cksum
		}
	}
	root := p.vdevs[0]
	if leaves.Read == 0 {
		leaves.Read = root.read
	}
	if leaves.Write == 0 {
		leaves.Write = root.write
	}
	if leaves.Checksum == 0 {
		leaves.Checksum = root.cksum
	}
	return leaves
}

func eqU(t *testing.T, what string, got *uint64, want uint64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %d", what, fmtPtr(got), want)
	}
}

func fmtPtr[T any](p *T) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

// checkPool compares one pool with the oracle. dates is false on a text
// read, whose ctime dates are in time.Local (A-60, pinned in the zfs tests).
func checkPool(t *testing.T, got spec.Pool, want *oraclePool, dates bool) {
	t.Helper()
	g := got.Properties
	n := want.name
	if got.Name != n || g.Health != strings.ToLower(want.health) || g.RawHealth != want.health {
		t.Errorf("%s: name %q health %q raw %q", n, got.Name, g.Health, g.RawHealth)
	}
	eqU(t, n+" sizeBytes", g.SizeBytes, want.size)
	eqU(t, n+" allocatedBytes", g.AllocatedBytes, want.alloc)
	eqU(t, n+" freeBytes", g.FreeBytes, want.free)
	if g.CapacityPercent == nil || *g.CapacityPercent != want.capacity || g.FragmentationPercent == nil ||
		*g.FragmentationPercent != want.frag || g.DedupRatio == nil || *g.DedupRatio != want.dedup {
		t.Errorf("%s: capacity %s frag %s dedup %s", n, fmtPtr(g.CapacityPercent), fmtPtr(g.FragmentationPercent), fmtPtr(g.DedupRatio))
	}
	if g.ReadOnly == nil || *g.ReadOnly != want.readOnly || fmtPtr(g.Altroot) != fmtPtr(want.altroot) {
		t.Errorf("%s: readOnly %s altroot %s", n, fmtPtr(g.ReadOnly), fmtPtr(g.Altroot))
	}
	depth1 := 0
	for _, v := range want.vdevs {
		if v.depth == 1 {
			depth1++
		}
	}
	if g.VdevCount == nil || *g.VdevCount != depth1 {
		t.Errorf("%s: vdevCount %s, want %d", n, fmtPtr(g.VdevCount), depth1)
	}
	if g.Errors == nil || *g.Errors != want.vdevErrors() {
		t.Errorf("%s: errors %+v, want %+v", n, g.Errors, want.vdevErrors())
	}
	checkVdevs(t, n, g.Vdevs, want.vdevs)
	if fmtPtr(g.Status) != fmtPtr(want.status) || fmtPtr(g.Action) != fmtPtr(want.action) {
		t.Errorf("%s: status %q action %q, want %q %q", n, fmtPtr(g.Status), fmtPtr(g.Action), fmtPtr(want.status), fmtPtr(want.action))
	}
	eqU(t, n+" permanentErrors", g.PermanentErrors, want.permanentErrors)
	sc := g.Scan
	if sc == nil {
		t.Fatalf("%s: scan null (%v)", n, g.Unavailable)
	}
	if sc.Function != want.scanFn || sc.State != want.scanState {
		t.Errorf("%s: scan %s %s, want %s %s", n, sc.Function, sc.State, want.scanFn, want.scanState)
	}
	eqU(t, n+" errorsRepaired", sc.ErrorsRepaired, want.errorsRepaired)
	eqU(t, n+" bytesRepaired", sc.BytesRepaired, want.bytes)
	if sc.LastScrubSource != scrubSourceStatus {
		t.Errorf("%s: lastScrubSource %q, want status", n, sc.LastScrubSource)
	}
	if dates {
		if sc.StartedAt == nil || !sc.StartedAt.Equal(want.start) || sc.EndedAt == nil || !sc.EndedAt.Equal(want.end) {
			t.Errorf("%s: scan %s..%s, want %s..%s", n, fmtPtr(sc.StartedAt), fmtPtr(sc.EndedAt), want.start, want.end)
		}
		if sc.LastScrubFinishedAt == nil || !sc.LastScrubFinishedAt.Equal(want.end) {
			t.Errorf("%s: lastScrubFinishedAt %s, want %s", n, fmtPtr(sc.LastScrubFinishedAt), want.end)
		}
		if age := t0.Sub(want.end).Hours() / 24; sc.LastScrubAgeDays == nil || *sc.LastScrubAgeDays != age {
			t.Errorf("%s: lastScrubAgeDays %s, want %v", n, fmtPtr(sc.LastScrubAgeDays), age)
		}
	}
}

func checkVdevs(t *testing.T, pool string, got []spec.Vdev, want []oracleVdev) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d vdevs, want %d: %+v", pool, len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		typ := "disk"
		if w.depth == 0 {
			typ = "root"
		}
		if g.Name != w.name || g.State != w.state || g.Depth != w.depth || g.Parent != w.parent || g.Type != typ {
			t.Errorf("%s vdev %d = %+v, want %+v", pool, i, g, w)
		}
		eqU(t, pool+" "+w.name+" read", g.ReadErrors, w.read)
		eqU(t, pool+" "+w.name+" write", g.WriteErrors, w.write)
		eqU(t, pool+" "+w.name+" checksum", g.ChecksumErrors, w.cksum)
	}
}

// homeSnapshotStats groups the name,creation capture by dataset.
func homeSnapshotStats(t *testing.T) map[string]snapshotStats {
	t.Helper()
	out := map[string]snapshotStats{}
	for _, r := range tabRows(home(t, "zfs/zfs-list-snapshots-creation.txt")) {
		ds, _, _ := strings.Cut(r[0], "@")
		st := out[ds]
		st.count++
		c := time.Unix(int64(atoi(t, r[1])), 0).UTC()
		if st.newest == nil || c.After(*st.newest) {
			st.newest = &c
		}
		out[ds] = st
	}
	return out
}

// checkDataset compares one dataset with its text row (DatasetColumns);
// snaps false expects the two snapshot fields null with a reason.
func checkDataset(t *testing.T, got spec.Dataset, r []string, stats map[string]snapshotStats, snaps bool) {
	t.Helper()
	g := got.Properties
	n := r[0]
	pool, _, _ := strings.Cut(n, "/")
	if got.Name != n || g.Pool != pool || g.Type != r[1] {
		t.Errorf("%s: name %q pool %q type %q", n, got.Name, g.Pool, g.Type)
	}
	used, avail, refer := atoi(t, r[2]), atoi(t, r[3]), atoi(t, r[4])
	eqU(t, n+" usedBytes", g.UsedBytes, used)
	eqU(t, n+" availableBytes", g.AvailableBytes, avail)
	eqU(t, n+" referencedBytes", g.ReferencedBytes, refer)
	eqU(t, n+" usedBySnapshotsBytes", g.UsedBySnapshotsBytes, atoi(t, r[5]))
	optional := func(w string) string {
		if w == "0" || w == "-" || w == "none" {
			return "null"
		}
		return w
	}
	if fmtPtr(g.QuotaBytes) != optional(r[6]) || fmtPtr(g.RefquotaBytes) != optional(r[7]) {
		t.Errorf("%s: quota %s refquota %s, want %s %s", n, fmtPtr(g.QuotaBytes), fmtPtr(g.RefquotaBytes), r[6], r[7])
	}
	eqU(t, n+" reservationBytes", g.ReservationBytes, atoi(t, r[8]))
	ratio, _ := strconv.ParseFloat(r[10], 64)
	if fmtPtr(g.Compression) != r[9] || g.CompressRatio == nil || *g.CompressRatio != ratio {
		t.Errorf("%s: compression %s ratio %s", n, fmtPtr(g.Compression), fmtPtr(g.CompressRatio))
	}
	eqU(t, n+" recordsize", g.Recordsize, atoi(t, r[11]))
	dash := func(w string) string {
		if w == "-" {
			return "null"
		}
		return w
	}
	mounted := map[string]string{"yes": "true", "no": "false", "-": "null"}[r[13]]
	if fmtPtr(g.Mountpoint) != dash(r[12]) || fmtPtr(g.Mounted) != mounted || fmtPtr(g.Encryption) != dash(r[14]) ||
		fmtPtr(g.KeyStatus) != dash(r[15]) {
		t.Errorf("%s: mountpoint %s mounted %s encryption %s keyStatus %s", n, fmtPtr(g.Mountpoint), fmtPtr(g.Mounted),
			fmtPtr(g.Encryption), fmtPtr(g.KeyStatus))
	}
	var basis string
	var pct float64
	switch {
	case optional(r[6]) != "null":
		basis, pct = basisQuota, float64(used)/float64(atoi(t, r[6]))*100
	case optional(r[7]) != "null":
		basis, pct = basisRefquota, float64(refer)/float64(atoi(t, r[7]))*100
	default:
		basis, pct = basisAvailable, float64(used)/float64(used+avail)*100
	}
	if g.UsedPercentBasis != basis || g.UsedPercent == nil || *g.UsedPercent != pct {
		t.Errorf("%s: usedPercent %s basis %q, want %v %q", n, fmtPtr(g.UsedPercent), g.UsedPercentBasis, pct, basis)
	}
	if !snaps {
		if g.SnapshotCount != nil || g.NewestSnapshotAt != nil || g.Unavailable["snapshotCount"] == "" ||
			g.Unavailable["newestSnapshotAt"] == "" {
			t.Errorf("%s: snapshotCount %s newest %s unavailable %v, want both null with a reason", n,
				fmtPtr(g.SnapshotCount), fmtPtr(g.NewestSnapshotAt), g.Unavailable)
		}
		return
	}
	st := stats[n]
	if g.SnapshotCount == nil || *g.SnapshotCount != st.count || fmtPtr(g.NewestSnapshotAt) != fmtPtr(st.newest) {
		t.Errorf("%s: snapshotCount %s newest %s, want %d %s", n, fmtPtr(g.SnapshotCount), fmtPtr(g.NewestSnapshotAt),
			st.count, fmtPtr(st.newest))
	}
	if len(g.Unavailable) != 0 {
		t.Errorf("%s: unavailable %v, want none", n, g.Unavailable)
	}
}

// homeSnapshotOrder is the full listing's names sorted by creation
// descending, then name.
func homeSnapshotOrder(t *testing.T) ([]string, map[string]time.Time) {
	t.Helper()
	created := map[string]time.Time{}
	var names []string
	for _, r := range tabRows(home(t, "zfs/zfs-list-snapshots.txt")) {
		names = append(names, r[0])
		created[r[0]] = time.Unix(int64(atoi(t, r[1])), 0).UTC()
	}
	slices.SortFunc(names, func(a, b string) int {
		if c := created[b].Compare(created[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return names, created
}

func itemNames[P any](items []apitypes.Resource[P]) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Name
	}
	return out
}

// readsSince are the argv the overlay saw from index from.
func readsSince(s *zstack, from int) [][]string { return s.ov.argvs()[from:] }

func hasSnapshotListing(argvs [][]string) bool {
	return slices.ContainsFunc(argvs, func(a []string) bool { return strings.Contains(strings.Join(a, " "), "-t snapshot") })
}

// ---- TC-STG-125 to TC-STG-140 ----

// TC-STG-125: pools.list on the home-server capture equals the text oracle field by field. Owner WI 4.2. Requirements: FR-4, FR-6, NFR-4, FR-18.
func TestTCSTG125_PoolsListOnTheHomeServerCapture(t *testing.T) {
	s := newZStack(t, nil)
	from := len(s.ov.argvs())
	l := listPools(t, s, `{}`)
	oracle := homePools(t)
	if got := itemNames(l.Items); !slices.Equal(got, []string{"z1", "z2", "z3"}) || l.NextCursor != "" {
		t.Fatalf("pools %v next %q, want [z1 z2 z3]", got, l.NextCursor)
	}
	for _, it := range l.Items {
		checkPool(t, it, oracle[it.Name], true)
		g := it.Properties
		if len(g.PermanentErrorPaths) != 0 || g.FeaturesUpgradable != nil || len(g.Unavailable) != 0 {
			t.Errorf("%s: permanentErrorPaths %v featuresUpgradable %s unavailable %v, want [] null none",
				it.Name, g.PermanentErrorPaths, fmtPtr(g.FeaturesUpgradable), g.Unavailable)
		}
		// percentDone is examined / toExamine × 100 (test-strategy formula);
		// the text form prints neither for a finished scan (A-59).
		sc := g.Scan
		if sc.ExaminedBytes == nil || sc.ToExamineBytes == nil || sc.PercentDone == nil ||
			*sc.PercentDone != float64(*sc.ExaminedBytes)/float64(*sc.ToExamineBytes)*100 {
			t.Errorf("%s: percentDone %s of %s / %s", it.Name, fmtPtr(sc.PercentDone), fmtPtr(sc.ExaminedBytes), fmtPtr(sc.ToExamineBytes))
		}
	}
	b, _ := json.Marshal(l)
	if !bytes.Contains(b, []byte(`"permanentErrorPaths":[]`)) {
		t.Errorf("permanentErrorPaths not marshaled []")
	}
	reads := readsSince(s, from)
	want := [][]string{s.fake.Argv(zfs.CmdPoolList), s.fake.Argv(zfs.CmdPoolStatus)}
	if len(reads) != 2 || !slices.ContainsFunc(reads, func(a []string) bool { return slices.Equal(a, want[0]) }) ||
		!slices.ContainsFunc(reads, func(a []string) bool { return slices.Equal(a, want[1]) }) {
		t.Errorf("reads %q, want exactly %q", reads, want)
	}
	for _, a := range reads {
		if !slices.Contains(a, "-j") {
			t.Errorf("read %q is not the JSON form", a)
		}
	}
}

// TC-STG-126: pools.get returns the vdev list, status, permanent errors and featuresUpgradable, and 404 for a pool not imported. Owner WI 4.2. Requirements: FR-5, FR-18.
func TestTCSTG126_PoolsGetReturnsTheVdevListStatus(t *testing.T) {
	t.Run("z1", func(t *testing.T) {
		s := newZStack(t, nil)
		got := call[spec.Pool](t, s, spec.PoolsGet.Op(), `{"name":"z1"}`)
		oracle := homePools(t)["z1"]
		checkPool(t, got, oracle, true)
		// TC-STG-68's verdict: a feature@ property whose value is disabled.
		upgradable := false
		for _, r := range tabRows(home(t, "zfs/zpool-get-z1.txt")) {
			if strings.HasPrefix(r[1], "feature@") && r[2] == "disabled" {
				upgradable = true
			}
		}
		if fmtPtr(got.Properties.FeaturesUpgradable) != fmt.Sprint(upgradable) {
			t.Errorf("featuresUpgradable %s, want %v", fmtPtr(got.Properties.FeaturesUpgradable), upgradable)
		}
		if got.ID != "/providers/storage/pools/z1" || len(got.Properties.PermanentErrorPaths) != 0 {
			t.Errorf("id %q paths %v", got.ID, got.Properties.PermanentErrorPaths)
		}
	})
	t.Run("tank", func(t *testing.T) {
		vector := synthetic(t, "zpool-status-degraded-mirror.json")
		s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
			o.answer(f.Argv(zfs.CmdPoolNames), zfs.Result{Stdout: append(home(t, "zfs/zpool-list-names.txt"), "tank\n"...)})
			ver, pools := jsonMembers(t, home(t, "zfs/zpool-list.json"), "pools")
			tank := setProp(t, rename(pools["z1"], "z1", "tank"), "health", "DEGRADED")
			o.answer(f.Argv(zfs.CmdPoolList, "tank"), zfs.Result{Stdout: jsonDoc(ver, "pools", "tank", tank)})
			o.answer(f.Argv(zfs.CmdPoolStatusVerbose, "tank"), zfs.Result{Stdout: vector})
			o.answer(f.Argv(zfs.CmdPoolGet, "tank"), zfs.Result{Stdout: rename(home(t, "zfs/zpool-get-z1.json"), "z1", "tank")})
		})
		got := call[spec.Pool](t, s, spec.PoolsGet.Op(), `{"name":"tank"}`).Properties
		// Expected from a generic decode of the vector: the leaf sums.
		var doc struct {
			Pools map[string]map[string]any `json:"pools"`
		}
		if err := json.Unmarshal(vector, &doc); err != nil {
			t.Fatal(err)
		}
		pool := doc.Pools["tank"]
		var want spec.ErrorCounts
		var sum func(v map[string]any)
		sum = func(v map[string]any) {
			kids, _ := v["vdevs"].(map[string]any)
			if len(kids) == 0 {
				want.Read += uint64(v["read_errors"].(float64))
				want.Write += uint64(v["write_errors"].(float64))
				want.Checksum += uint64(v["checksum_errors"].(float64))
			}
			for _, k := range kids {
				sum(k.(map[string]any))
			}
		}
		for _, root := range pool["vdevs"].(map[string]any) {
			sum(root.(map[string]any))
		}
		if got.Errors == nil || *got.Errors != want || want.Checksum == 0 {
			t.Errorf("errors %+v, want %+v", got.Errors, want)
		}
		if got.Health != "degraded" || got.PermanentErrors == nil || *got.PermanentErrors != uint64(pool["error_count"].(float64)) ||
			len(got.PermanentErrorPaths) != 0 {
			t.Errorf("health %s permanentErrors %s paths %v", got.Health, fmtPtr(got.PermanentErrors), got.PermanentErrorPaths)
		}
		if wantStatus := strings.TrimRight(pool["status"].(string), "\n"); fmtPtr(got.Status) != wantStatus {
			t.Errorf("status %q, want %q", fmtPtr(got.Status), wantStatus)
		}
		if i := slices.IndexFunc(got.Vdevs, func(v spec.Vdev) bool { return v.Type == "mirror" }); i < 0 || got.Vdevs[i].State != "degraded" {
			t.Errorf("vdevs %+v, want a degraded mirror", got.Vdevs)
		}
	})
	t.Run("nexus-absent", func(t *testing.T) {
		s := newZStack(t, nil)
		from := len(s.ov.argvs())
		_, err := s.invoke(spec.PoolsGet.Op(), `{"name":"nexus-absent"}`)
		if pe := wantSlug(t, err, problem.NotFound); pe.Status != 404 {
			t.Errorf("status %d", pe.Status)
		}
		if reads := readsSince(s, from); len(reads) != 1 || !slices.Equal(reads[0], s.fake.Argv(zfs.CmdPoolNames)) {
			t.Errorf("reads %q, want only zpool list -H -o name", reads)
		}
	})
}

// TC-STG-127: pools.list name filter and paging. Owner WI 4.2. Requirements: FR-4, NFR-3.
func TestTCSTG127_PoolsListNameFilterAndPaging(t *testing.T) {
	s := newZStack(t, nil)
	if got := itemNames(listPools(t, s, `{"name":"z2"}`).Items); !slices.Equal(got, []string{"z2"}) {
		t.Errorf("name=z2: %v", got)
	}
	l := listPools(t, s, `{"name":"nope"}`)
	if b, _ := json.Marshal(l); len(l.Items) != 0 || !bytes.Contains(b, []byte(`"items":[]`)) {
		t.Errorf("name=nope: %s, want items []", b)
	}
	pages := walk[spec.Pool](t, s, spec.PoolsList.Op(), `{"limit":1}`)
	var got [][]string
	for _, pg := range pages {
		got = append(got, itemNames(pg))
	}
	if fmt.Sprint(got) != "[[z1] [z2] [z3]]" {
		t.Errorf("pages %v, want [z1] [z2] [z3]", got)
	}
}

// TC-STG-128: datasets.list fields and the usedPercent basis. Owner WI 4.2. Requirements: FR-7, NFR-4.
func TestTCSTG128_DatasetsListFieldsAndTheUsedPercentBasis(t *testing.T) {
	const rq = "z2/d2/refquota-only"
	const refquota = 100 << 30
	var rqRaw json.RawMessage
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		doc := home(t, "zfs/zfs-list-datasets.json")
		_, ds := jsonMembers(t, doc, "datasets")
		rqRaw = setProp(t, rename(ds["z2/d2/appdata"], "z2/d2/appdata", rq), "refquota", refquota)
		o.answer(f.Argv(zfs.CmdDatasetList), zfs.Result{Stdout: spliced(t, doc, "datasets", rq, rqRaw)})
	})
	l := listDatasets(t, s, `{"limit":500}`)
	stats := homeSnapshotStats(t)
	rows := tabRows(home(t, "zfs/zfs-list-datasets.txt"))
	byName := map[string]spec.Dataset{}
	for _, d := range l.Items {
		byName[d.Name] = d
	}
	if len(l.Items) != len(rows)+1 || l.NextCursor != "" {
		t.Fatalf("%d datasets next %q, want %d", len(l.Items), l.NextCursor, len(rows)+1)
	}
	if !slices.IsSortedFunc(l.Items, func(a, b spec.Dataset) int { return strings.Compare(a.Name, b.Name) }) {
		t.Errorf("not sorted by name: %v", itemNames(l.Items))
	}
	for _, r := range rows {
		checkDataset(t, byName[r[0]], r, stats, true)
	}
	vms := byName["z2/d1/vms"].Properties
	if fmtPtr(vms.Mountpoint) != "/media/vzdump" || vms.QuotaBytes == nil || vms.UsedPercentBasis != basisQuota ||
		*vms.UsedPercent != float64(*vms.UsedBytes)/float64(*vms.QuotaBytes)*100 {
		t.Errorf("z2/d1/vms: %+v", vms)
	}
	got := byName[rq].Properties
	if got.UsedPercentBasis != basisRefquota || got.QuotaBytes != nil || got.UsedPercent == nil ||
		*got.UsedPercent != float64(*got.ReferencedBytes)/float64(refquota)*100 || *got.SnapshotCount != 0 || got.NewestSnapshotAt != nil {
		t.Errorf("%s: %+v", rq, got)
	}
	if b, _ := json.Marshal(l); bytes.Contains(b, []byte(`"keylocation"`)) {
		t.Errorf("a keylocation key in %s", b)
	}
}

// TC-STG-129: datasets.get by a slash-separated name, and 404 for a missing dataset. Owner WI 4.2. Requirements: FR-7, FR-18.
func TestTCSTG129_DatasetsGetByASlashSeparatedName(t *testing.T) {
	s := newZStack(t, nil)
	from := len(s.ov.argvs())
	got := call[spec.Dataset](t, s, spec.DatasetsGet.Op(), `{"name":"z1/ds/appdata"}`)
	reads := readsSince(s, from)
	if !slices.ContainsFunc(reads, func(a []string) bool { return slices.Equal(a, s.fake.Argv(zfs.CmdSnapshotTimes, "z1/ds/appdata")) }) {
		t.Errorf("reads %q, want the -d 1 z1/ds/appdata snapshot read", reads)
	}
	l := listDatasets(t, s, `{"limit":500}`)
	i := slices.IndexFunc(l.Items, func(d spec.Dataset) bool { return d.Name == "z1/ds/appdata" })
	gb, _ := json.Marshal(got)
	lb, _ := json.Marshal(l.Items[i])
	if !bytes.Equal(gb, lb) {
		t.Errorf("get %s\nlist %s", gb, lb)
	}
	if got.ID != "/providers/storage/datasets/z1%2Fds%2Fappdata" {
		t.Errorf("id %q", got.ID)
	}
	_, err := s.invoke(spec.DatasetsGet.Op(), `{"name":"z1/nexus-capture-absent"}`)
	if pe := wantSlug(t, err, problem.NotFound); pe.Status != 404 || !strings.Contains(pe.Detail, "does not exist") {
		t.Errorf("missing dataset: %d %q", pe.Status, pe.Detail)
	}
}

// TC-STG-130: dataset filters and cursor paging. Owner WI 4.2. Requirements: FR-7, NFR-3.
func TestTCSTG130_DatasetFiltersAndCursorPaging(t *testing.T) {
	s := newZStack(t, nil)
	rows := tabRows(home(t, "zfs/zfs-list-datasets.txt"))
	var z2 []string
	var all []string
	for _, r := range rows {
		all = append(all, r[0])
		if strings.HasPrefix(r[0], "z2/") || r[0] == "z2" {
			z2 = append(z2, r[0])
		}
	}
	slices.Sort(all)
	slices.Sort(z2)
	if got := itemNames(listDatasets(t, s, `{"pool":"z2"}`).Items); !slices.Equal(got, z2) {
		t.Errorf("pool=z2: %v, want %v", got, z2)
	}
	for _, d := range listDatasets(t, s, `{"type":"volume"}`).Items {
		if d.Properties.Type != spec.TypeVolume {
			t.Errorf("type=volume: %s is %s", d.Name, d.Properties.Type)
		}
	}
	_, err := s.invoke(spec.DatasetsList.Op(), `{"type":"snapshot"}`)
	wantInvalid(t, err, "type")
	_, err = s.invoke(spec.DatasetsList.Op(), `{"pool":"z1/ds"}`)
	wantInvalid(t, err, "pool")

	var walked []string
	for _, pg := range walk[spec.Dataset](t, s, spec.DatasetsList.Op(), `{"limit":3}`) {
		if len(pg) > 3 {
			t.Errorf("page of %d", len(pg))
		}
		walked = append(walked, itemNames(pg)...)
	}
	if !slices.Equal(walked, all) {
		t.Errorf("walk %v, want %v", walked, all)
	}

	tampered := base64.RawURLEncoding.EncodeToString([]byte("z1/../etc"))
	for _, in := range []string{`{"cursor":"` + tampered + `"}`, `{"cursor":"bm90IGEgY3Vyc29y!"}`} {
		_, err = s.invoke(spec.DatasetsList.Op(), in)
		wantInvalid(t, err, "cursor")
	}
	_, err = s.invoke(spec.DatasetsList.Op(), `{"limit":501}`)
	wantInvalid(t, err, "limit")
	_, err = s.invoke(spec.DatasetsList.Op(), `{"limit":0}`)
	wantInvalid(t, err, "limit")
	if l := listDatasets(t, s, `{"limit":500}`); len(l.Items) != len(all) {
		t.Errorf("limit=500: %d items", len(l.Items))
	}
	// The handler refuses the same inputs for a direct caller.
	_, err = s.p.datasetsList(context.Background(), spec.DatasetListInput{Limit: 501})
	wantInvalid(t, err, "limit")
	_, err = s.p.datasetsList(context.Background(), spec.DatasetListInput{Cursor: tampered})
	wantInvalid(t, err, "cursor")
	_, err = s.p.datasetsList(context.Background(), spec.DatasetListInput{Type: "snapshot"})
	wantInvalid(t, err, "type")
}

// TC-STG-131: snapshots.list order, paging and filters. Owner WI 4.2. Requirements: FR-8, NFR-3.
func TestTCSTG131_SnapshotsListOrderPagingAndFilters(t *testing.T) {
	s := newZStack(t, nil)
	order, created := homeSnapshotOrder(t)
	first := listSnapshots(t, s, `{}`)
	if want := order[:min(spec.DefaultLimit, len(order))]; !slices.Equal(itemNames(first.Items), want) {
		t.Errorf("first page %v, want %v", itemNames(first.Items), want)
	}
	var walked []string
	for _, pg := range walk[spec.Snapshot](t, s, spec.SnapshotsList.Op(), `{"limit":500}`) {
		walked = append(walked, itemNames(pg)...)
	}
	if !slices.Equal(walked, order) {
		t.Errorf("walk of %d, want the %d of the capture in order", len(walked), len(order))
	}

	from := len(s.ov.argvs())
	l := listSnapshots(t, s, `{"dataset":"z1/ds/appdata","limit":500}`)
	if !slices.ContainsFunc(readsSince(s, from), func(a []string) bool {
		return slices.Equal(a, s.fake.Argv(zfs.CmdSnapshotList, "z1/ds/appdata")) && slices.Contains(a, "-d")
	}) {
		t.Errorf("reads %q, want -d 1 z1/ds/appdata", readsSince(s, from))
	}
	n := 0
	for _, name := range order {
		if strings.HasPrefix(name, "z1/ds/appdata@") {
			n++
		}
	}
	for _, it := range l.Items {
		if it.Properties.Dataset != "z1/ds/appdata" {
			t.Errorf("dataset filter: %s", it.Name)
		}
	}
	if len(l.Items) != n || n == 0 {
		t.Errorf("dataset filter: %d items, want %d", len(l.Items), n)
	}

	from = len(s.ov.argvs())
	l = listSnapshots(t, s, `{"dataset":"z1/ds","recursive":true,"limit":500}`)
	if !slices.ContainsFunc(readsSince(s, from), func(a []string) bool {
		return slices.Equal(a, s.fake.Argv(zfs.CmdSnapshotListRecursive, "z1/ds")) && slices.Contains(a, "-r")
	}) {
		t.Errorf("reads %q, want -r z1/ds", readsSince(s, from))
	}
	n = 0
	for _, name := range order {
		if strings.HasPrefix(name, "z1/ds@") || strings.HasPrefix(name, "z1/ds/") {
			n++
		}
	}
	if len(l.Items) != n {
		t.Errorf("recursive: %d items, want %d", len(l.Items), n)
	}

	since := created[order[9]]
	l = listSnapshots(t, s, fmt.Sprintf(`{"since":%q,"limit":500}`, since.Format(time.RFC3339)))
	n = 0
	for _, name := range order {
		if !created[name].Before(since) {
			n++
		}
	}
	if len(l.Items) != n || n < 10 {
		t.Errorf("since: %d items, want %d", len(l.Items), n)
	}
	for _, it := range l.Items {
		if it.Properties.CreatedAt.Before(since) {
			t.Errorf("since: %s created %s", it.Name, it.Properties.CreatedAt)
		}
	}
	_, err := s.invoke(spec.SnapshotsList.Op(), `{"since":"yesterday"}`)
	wantInvalid(t, err, "since")
	_, err = s.p.snapshotsList(context.Background(), spec.SnapshotListInput{Since: "yesterday"})
	wantInvalid(t, err, "since")
}

// TC-STG-132: createdByNexus is true only for a property value that matches the operation-ID pattern. Owner WI 4.2. Requirements: FR-8, FR-14.
func TestTCSTG132_CreatedByNexusIsTrueOnlyForAProperty(t *testing.T) {
	values := []string{"op-0123456789abcdef", "evil; rm -rf /", "-"}
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		out := home(t, "zfs/zfs-list-snapshots.txt")
		for i, v := range values {
			out = append(out, fmt.Sprintf("z1/ds@tc132-%d\t%d\t0\t196608\t0\t%s\n", i, 1795000000+i, v)...)
		}
		o.answer(f.Argv(zfs.CmdSnapshotList), zfs.Result{Stdout: out})
	})
	total := len(tabRows(home(t, "zfs/zfs-list-snapshots.txt"))) + len(values)
	all := listSnapshots(t, s, `{"limit":500}`)
	var nexus []string
	count := len(all.Items)
	for _, pg := range walk[spec.Snapshot](t, s, spec.SnapshotsList.Op(), `{"limit":500}`)[1:] {
		count += len(pg)
	}
	for _, it := range all.Items {
		if it.Properties.CreatedByNexus {
			nexus = append(nexus, it.Name)
		}
	}
	if count != total || !slices.Equal(nexus, []string{"z1/ds@tc132-0"}) {
		t.Errorf("%d items, createdByNexus on %v, want %d and only tc132-0", count, nexus, total)
	}
	yes := listSnapshots(t, s, `{"createdByNexus":"true","limit":500}`)
	if got := itemNames(yes.Items); !slices.Equal(got, []string{"z1/ds@tc132-0"}) {
		t.Errorf("createdByNexus=true: %v", got)
	}
	no := 0
	for _, pg := range walk[spec.Snapshot](t, s, spec.SnapshotsList.Op(), `{"createdByNexus":"false","limit":500}`) {
		for _, it := range pg {
			if it.Properties.CreatedByNexus {
				t.Errorf("createdByNexus=false listed %s", it.Name)
			}
		}
		no += len(pg)
	}
	if no != total-1 {
		t.Errorf("createdByNexus=false: %d, want %d", no, total-1)
	}
	b, _ := json.Marshal(all)
	for _, v := range values[:2] {
		if bytes.Contains(b, []byte(v)) || bytes.Contains(b, []byte("rm -rf")) {
			t.Errorf("the property value %q is returned", v)
		}
	}
}

// TC-STG-133: a suspended pool is listed with null fields and reasons beside healthy pools. Owner WI 4.2. Requirements: FR-4, NFR-7.
func TestTCSTG133_ASuspendedPoolIsListedWithNull(t *testing.T) {
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		list := home(t, "zfs/zpool-list.json")
		_, pools := jsonMembers(t, list, "pools")
		z4 := rename(pools["z1"], "z1", "z4")
		for _, k := range []string{"size", "allocated", "free", "capacity", "fragmentation", "dedupratio", "readonly"} {
			z4 = setProp(t, z4, k, "-")
		}
		z4 = setProp(t, z4, "health", "SUSPENDED")
		o.answer(f.Argv(zfs.CmdPoolList), zfs.Result{Stdout: spliced(t, list, "pools", "z4", z4)})
		// The synthetic suspended pool with the vdev tree and error count
		// zpool status cannot report removed (TC-STG-66's derivation).
		_, vp := jsonMembers(t, synthetic(t, "zpool-status-suspended.json"), "pools")
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(vp["tank"], &obj); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"vdevs", "dedup", "special", "error_count"} {
			delete(obj, k)
		}
		obj["name"] = json.RawMessage(`"z4"`)
		raw, _ := json.Marshal(obj)
		o.answer(f.Argv(zfs.CmdPoolStatus), zfs.Result{Stdout: spliced(t, home(t, "zfs/zpool-status.json"), "pools", "z4", raw)})
	})
	l := listPools(t, s, `{}`)
	if got := itemNames(l.Items); !slices.Equal(got, []string{"z1", "z2", "z3", "z4"}) {
		t.Fatalf("pools %v", got)
	}
	oracle := homePools(t)
	for _, it := range l.Items[:3] {
		checkPool(t, it, oracle[it.Name], true)
		if len(it.Properties.Unavailable) != 0 {
			t.Errorf("%s unavailable %v", it.Name, it.Properties.Unavailable)
		}
	}
	z4 := l.Items[3].Properties
	if z4.Health != "suspended" || z4.RawHealth != "SUSPENDED" {
		t.Errorf("z4 health %q %q", z4.Health, z4.RawHealth)
	}
	nulls := map[string]bool{
		"sizeBytes": z4.SizeBytes == nil, "allocatedBytes": z4.AllocatedBytes == nil, "freeBytes": z4.FreeBytes == nil,
		"capacityPercent": z4.CapacityPercent == nil, "fragmentationPercent": z4.FragmentationPercent == nil,
		"dedupRatio": z4.DedupRatio == nil, "readOnly": z4.ReadOnly == nil, "vdevs": len(z4.Vdevs) == 0,
		"vdevCount": z4.VdevCount == nil, "errors": z4.Errors == nil, "permanentErrors": z4.PermanentErrors == nil,
	}
	for k, isNull := range nulls {
		if !isNull || z4.Unavailable[k] == "" {
			t.Errorf("z4 %s: null %v, reason %q", k, isNull, z4.Unavailable[k])
		}
	}
}

// TC-STG-134: a hung zpool status lets pools.list answer with scan null, and the next call answers at once. Owner WI 4.2. Requirements: NFR-2, FR-4.
func TestTCSTG134_AHungZpoolStatusLetsPoolsList(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s := newZStack(t, func(f *storagetest.Fake, _ *overlay) { f.Stuck(f.Argv(zfs.CmdPoolStatus), release) })
	setCommandTimeout(s.p, 300*time.Millisecond)
	ctx := context.Background()

	check := func(l apitypes.List[spec.Pool], want ...string) {
		t.Helper()
		if got := itemNames(l.Items); !slices.Equal(got, []string{"z1", "z2", "z3"}) {
			t.Fatalf("pools %v", got)
		}
		for _, it := range l.Items {
			g := it.Properties
			if g.SizeBytes == nil || g.CapacityPercent == nil || g.Health != "online" {
				t.Errorf("%s list fields missing: %+v", it.Name, g)
			}
			for _, k := range []string{"scan", "vdevCount", "errors"} {
				r := g.Unavailable[k]
				if !strings.Contains(r, "zpool status") || !slices.ContainsFunc(want, func(w string) bool { return strings.Contains(r, w) }) {
					t.Errorf("%s %s reason %q, want zpool status and %q", it.Name, k, r, want)
				}
			}
			if g.Scan != nil || g.VdevCount != nil || g.Errors != nil {
				t.Errorf("%s: scan %v vdevCount %v errors %v, want null", it.Name, g.Scan, g.VdevCount, g.Errors)
			}
		}
	}
	start := time.Now()
	l, err := s.p.poolsList(ctx, spec.PoolListInput{})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if took < 250*time.Millisecond || took > time.Second {
		t.Errorf("first call took %s, want about 300ms", took)
	}
	check(l, "did not finish within 300ms")
	start = time.Now()
	l, err = s.p.poolsList(ctx, spec.PoolListInput{})
	if took := time.Since(start); err != nil || took > 50*time.Millisecond {
		t.Errorf("second call: %v after %s, want under 50ms", err, took)
	}
	check(l, "is still running since")
	if n := countArgv(s.fake.Reads(), s.fake.Argv(zfs.CmdPoolStatus)); n != 1 {
		t.Errorf("%d zpool status calls, want 1", n)
	}
}

// scanStack is a stack whose z1 scan was seeded with vector ("" keeps the
// capture's finished scrub).
func scanStack(t *testing.T, vector string) *zstack {
	s := newZStack(t, nil)
	if vector != "" {
		s.fake.SeedScan("z1", vector)
	}
	return s
}

func z1Scan(t *testing.T, s *zstack) *spec.Scan {
	t.Helper()
	l := listPools(t, s, `{"name":"z1"}`)
	if len(l.Items) != 1 || l.Items[0].Properties.Scan == nil {
		t.Fatalf("z1: %+v", l.Items)
	}
	return l.Items[0].Properties.Scan
}

// vectorEnd is a synthetic vector's scan end_time.
func vectorEnd(t *testing.T, vector string) time.Time {
	t.Helper()
	var doc struct {
		Pools map[string]struct {
			Scan struct {
				End int64 `json:"end_time"`
			} `json:"scan_stats"`
		} `json:"pools"`
	}
	if err := json.Unmarshal(synthetic(t, vector), &doc); err != nil {
		t.Fatal(err)
	}
	return time.Unix(doc.Pools["tank"].Scan.End, 0).UTC()
}

func wantRemembered(t *testing.T, sc *spec.Scan, e time.Time) {
	t.Helper()
	if sc.LastScrubFinishedAt == nil || !sc.LastScrubFinishedAt.Equal(e) || sc.LastScrubSource != scrubSourceRemembered {
		t.Errorf("lastScrubFinishedAt %s source %q, want %s remembered", fmtPtr(sc.LastScrubFinishedAt), sc.LastScrubSource, e)
	}
	if age := t0.Sub(e).Hours() / 24; sc.LastScrubAgeDays == nil || *sc.LastScrubAgeDays != age {
		t.Errorf("lastScrubAgeDays %s, want %v", fmtPtr(sc.LastScrubAgeDays), age)
	}
}

func wantUnknown(t *testing.T, sc *spec.Scan, shown string) {
	t.Helper()
	want := "the last completed scrub is not shown (" + shown + ") and none was observed since nexusd started"
	if sc.LastScrubFinishedAt != nil || sc.LastScrubAgeDays != nil || sc.LastScrubSource != "" ||
		sc.Unavailable["lastScrubFinishedAt"] != want || sc.Unavailable["lastScrubAgeDays"] != want {
		t.Errorf("last scrub %s age %s source %q unavailable %v, want null with %q", fmtPtr(sc.LastScrubFinishedAt),
			fmtPtr(sc.LastScrubAgeDays), sc.LastScrubSource, sc.Unavailable, want)
	}
}

// TC-STG-135: lastScrubFinishedAt survives a shown resilver through the memo, then a fresh provider says why it has none. Owner WI 4.2. Requirements: FR-6.
func TestTCSTG135_LastScrubFinishedAtSurvivesAShownResilverThroughThe(t *testing.T) {
	e := homePools(t)["z1"].end
	s := scanStack(t, "")
	a := z1Scan(t, s)
	if a.Function != "scrub" || a.State != "finished" || !a.LastScrubFinishedAt.Equal(e) || a.LastScrubSource != scrubSourceStatus {
		t.Errorf("state A: %+v", a)
	}
	s.fake.SeedScan("z1", storagetest.VectorResilverScanning)
	b := z1Scan(t, s)
	if b.Function != "resilver" || b.State != "scanning" {
		t.Errorf("state A': %s %s", b.Function, b.State)
	}
	wantRemembered(t, b, e)
	wantUnknown(t, z1Scan(t, scanStack(t, storagetest.VectorResilverScanning)), "resilver scanning")
}

// TC-STG-136: 5,000 snapshots list in under 2 s, and stdout over 4 MiB is a 503 with its reason. Owner WI 4.2. Requirements: NFR-1, NFR-3, FR-8.
func TestTCSTG136_5000SnapshotsListInUnder2(t *testing.T) {
	listing := home(t, "zfs/zfs-list-snapshots.txt")
	rows := tabRows(listing)
	b := float64(len(listing)) / float64(len(rows))
	if 5000*b >= zfs.StdoutCap {
		t.Fatalf("A-39 falsified: 5,000 snapshot rows exceed the stdout cap (%v bytes per row)", b)
	}
	big := snapshotRows(rows, 5000)
	if len(big) >= zfs.StdoutCap {
		t.Fatalf("5,000 synthetic rows are %d bytes, over the cap", len(big))
	}
	over := snapshotRows(rows, int(math.Ceil(float64(zfs.StdoutCap)/b))+1)
	if len(over) <= zfs.StdoutCap {
		t.Fatalf("the N-row listing is %d bytes, not over the cap", len(over))
	}
	listing5000 := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		o.answer(f.Argv(zfs.CmdSnapshotList), zfs.Result{Stdout: big})
	})
	start := time.Now()
	l := listSnapshots(t, listing5000, `{"limit":500}`)
	if took := time.Since(start); took > 2*time.Second || len(l.Items) != 500 || l.NextCursor == "" {
		t.Errorf("5,000 rows: %d items in %s", len(l.Items), took)
	}
	if !slices.ContainsFunc(l.Items, func(s spec.Snapshot) bool { return len(s.Name) == spec.MaxSnapshotNameBytes }) {
		t.Errorf("no 255-byte name on the first page")
	}

	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		o.answer(f.Argv(zfs.CmdSnapshotList), zfs.Result{Stdout: over})
	})
	_, err := s.invoke(spec.SnapshotsList.Op(), `{}`)
	pe := wantSlug(t, err, problem.DependencyUnavailable)
	if pe.Status != 503 || !strings.Contains(pe.Detail, "zfs list") || !strings.Contains(pe.Detail, "4 MiB") {
		t.Errorf("over the cap: %d %q", pe.Status, pe.Detail)
	}
}

// snapshotRows is a -H -p snapshot listing of n rows in the captured row
// shape and length: distinct names and creation times, every 50th name
// 255 bytes.
func snapshotRows(captured [][]string, n int) []byte {
	var b bytes.Buffer
	for i := range n {
		r := captured[i%len(captured)]
		// The captured name with its last seven bytes replaced by i keeps
		// the captured row's length.
		ds, snap, _ := strings.Cut(r[0], "@")
		id := fmt.Sprintf("%07d", i)
		if len(snap) > len(id) {
			snap = snap[:len(snap)-len(id)] + id
		} else {
			snap = "s" + id
		}
		name := ds + "@" + snap
		if i%50 == 0 {
			name += strings.Repeat("x", spec.MaxSnapshotNameBytes-len(name))
		}
		fmt.Fprintf(&b, "%s\t%d\t%s\n", name, 1790000000+i, strings.Join(r[2:], "\t"))
	}
	return b.Bytes()
}

// BenchmarkTCSTG136 is one storage.snapshots.list call with limit 500 on
// the 5,000-row listing (NFR-1).
func BenchmarkTCSTG136(b *testing.B) {
	fake := storagetest.NewFake(b, "home-server")
	ov := newOverlay(fake)
	ov.answer(fake.Argv(zfs.CmdSnapshotList), zfs.Result{Stdout: snapshotRows(tabRows(home(b, "zfs/zfs-list-snapshots.txt")), 5000)})
	fsys := fstest.MapFS{
		"usr/sbin/zpool": {Mode: 0o755}, "usr/sbin/zfs": {Mode: 0o755},
		devZFS: {Mode: fs.ModeDevice | fs.ModeCharDevice | 0o666},
	}
	p := New(Options{Host: "bench", Logger: discard, GOOS: "linux", FS: fsys, Run: ov.run})
	if err := p.Configure(func(into any) error { into.(*config.StorageConfig).Enabled = true; return nil }); err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st := p.Preflight(ctx); st.State != apitypes.ProviderEnabled {
		b.Fatalf("preflight: %s", st.Reason)
	}
	for b.Loop() {
		l, err := p.snapshotsList(context.Background(), spec.SnapshotListInput{Limit: 500})
		if err != nil || len(l.Items) != 500 {
			b.Fatalf("%v, %d items", err, len(l.Items))
		}
	}
}

// TC-STG-137: every ZFS read is labelled untrusted and carries no forbidden property. Owner WI 4.2. Requirements: FR-18, NFR-5.
//
// The contentTrust label itself is the MCP adapter's, which labels a
// result by the op's Untrusted flag (internal/mcpadapter success); the two
// MCP tools among these reads are exercised end to end by TC-STG-205
// (WI 5.1). Here: the flag on each op, and the raw bodies.
func TestTCSTG137_EveryZFSReadIsLabelledUntrustedAnd(t *testing.T) {
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		doc := home(t, "zfs/zfs-list-datasets.json")
		_, ds := jsonMembers(t, doc, "datasets")
		extra := func(raw json.RawMessage, name, prop, value string) json.RawMessage {
			raw = rename(raw, "z1/ds/appdata", name)
			return bytes.Replace(raw, []byte(`"properties":{`),
				[]byte(fmt.Sprintf(`"properties":{%q:{"value":%q,"source":{"type":"LOCAL","data":"-"}},`, prop, value)), 1)
		}
		user := extra(ds["z1/ds/appdata"], "z1/ds/userprop", "com.example:secret", "NX-USERPROP")
		enc := extra(ds["z1/ds/appdata"], "z1/ds/enc", "keylocation", "file:///root/NX-KEYLOC")
		o.answer(f.Argv(zfs.CmdDatasetList), zfs.Result{Stdout: spliced(t, doc, "datasets", "z1/ds/userprop", user, "z1/ds/enc", enc)})
		ver, _ := jsonMembers(t, doc, "datasets")
		o.answer(f.Argv(zfs.CmdDatasetList, "z1/ds/enc"), zfs.Result{Stdout: jsonDoc(ver, "datasets", "z1/ds/enc", enc)})
		o.answer(f.Argv(zfs.CmdSnapshotTimes, "z1/ds/enc"), zfs.Result{})
	})
	reads := []struct {
		op    def.Op
		input string
	}{
		{spec.PoolsList.Op(), `{}`}, {spec.PoolsGet.Op(), `{"name":"z1"}`}, {spec.DatasetsList.Op(), `{"limit":500}`},
		{spec.DatasetsGet.Op(), `{"name":"z1/ds/enc"}`}, {spec.SnapshotsList.Op(), `{"limit":500}`},
	}
	for _, r := range reads {
		if !r.op.Untrusted {
			t.Errorf("%s is not Untrusted", r.op.ID)
		}
		body, err := s.invoke(r.op, r.input)
		if err != nil {
			t.Fatalf("%s: %v", r.op.ID, err)
		}
		raw, _ := json.Marshal(body)
		for _, marker := range []string{"NX-USERPROP", "NX-KEYLOC", "com.example:secret"} {
			if bytes.Contains(raw, []byte(marker)) {
				t.Errorf("%s carries %s", r.op.ID, marker)
			}
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		walkKeys(v, func(k string) {
			if strings.EqualFold(k, "keylocation") {
				t.Errorf("%s has a %s key", r.op.ID, k)
			}
		})
	}
	enc := call[spec.Dataset](t, s, spec.DatasetsGet.Op(), `{"name":"z1/ds/enc"}`).Properties
	if fmtPtr(enc.Encryption) != "aes-256-gcm" || fmtPtr(enc.KeyStatus) != "available" {
		t.Errorf("encryption %s keyStatus %s", fmtPtr(enc.Encryption), fmtPtr(enc.KeyStatus))
	}
}

func walkKeys(v any, fn func(string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			fn(k)
			walkKeys(e, fn)
		}
	case []any:
		for _, e := range x {
			walkKeys(e, fn)
		}
	}
}

// TC-STG-138: lastScrubFinishedAt comes only from a finished scrub; a canceled or paused scrub keeps the remembered date. Owner WI 4.2. Requirements: FR-6, FR-15.
func TestTCSTG138_LastScrubFinishedAtComesOnlyFromAFinished(t *testing.T) {
	e := homePools(t)["z1"].end
	c := vectorEnd(t, storagetest.VectorScrubCanceled)
	if !c.After(e) {
		t.Fatalf("C %s is not after E %s", c, e)
	}
	s := scanStack(t, "")
	a := z1Scan(t, s)
	if !a.LastScrubFinishedAt.Equal(e) || a.LastScrubSource != scrubSourceStatus || *a.LastScrubAgeDays != t0.Sub(e).Hours()/24 {
		t.Errorf("state A: %s %q %s", fmtPtr(a.LastScrubFinishedAt), a.LastScrubSource, fmtPtr(a.LastScrubAgeDays))
	}

	s.fake.SeedScan("z1", storagetest.VectorScrubCanceled)
	canceled := z1Scan(t, s)
	got := call[spec.Pool](t, s, spec.PoolsGet.Op(), `{"name":"z1"}`).Properties.Scan
	for _, sc := range []*spec.Scan{canceled, got} {
		if sc.Function != "scrub" || sc.State != "canceled" || sc.EndedAt == nil || !sc.EndedAt.Equal(c) {
			t.Errorf("canceled: %s %s ended %s, want scrub canceled %s", sc.Function, sc.State, fmtPtr(sc.EndedAt), c)
		}
		wantRemembered(t, sc, e)
	}

	s.fake.SeedScan("z1", storagetest.VectorScrubPaused)
	paused := z1Scan(t, s)
	if paused.State != "paused" {
		t.Errorf("paused: state %s", paused.State)
	}
	wantRemembered(t, paused, e)

	wantUnknown(t, z1Scan(t, scanStack(t, storagetest.VectorScrubCanceled)), "scrub canceled")
	wantUnknown(t, z1Scan(t, scanStack(t, storagetest.VectorScrubPaused)), "scrub paused")
}

// TC-STG-139: an oversized or failed snapshot listing leaves only the snapshot fields null; health never runs the snapshot listing. Owner WI 4.2. Requirements: FR-7, FR-15, NFR-1, NFR-3, NFR-7.
//
// Step 2 runs storage.health.get (WI 4.4) in each variant: its reads hold no
// snapshot listing and its signals equal the clean capture's.
func TestTCSTG139_AnOversizedOrFailedSnapshotListingLeavesOnly(t *testing.T) {
	clean := sigsOf(healthOf(t, newZStack(t, nil).d).Signals)
	rows := tabRows(home(t, "zfs/zfs-list-datasets.txt"))
	stats := homeSnapshotStats(t)
	var times bytes.Buffer
	for i := 0; times.Len() <= zfs.StdoutCap; i++ {
		fmt.Fprintf(&times, "z1/ds/appdata@tc139-%07d\t%d\n", i, 1790000000+i)
	}
	full := snapshotRows(tabRows(home(t, "zfs/zfs-list-snapshots.txt")), int(math.Ceil(float64(zfs.StdoutCap)/40)))
	negative := homeStderr(t, "zfs/negative-zfs-list.json")
	variants := map[string]struct {
		res    zfs.Result
		reason []string
	}{
		"oversized": {zfs.Result{Stdout: times.Bytes()}, []string{snapshotWhat, "4 MiB"}},
		"failed": {zfs.Result{ExitCode: 1, Stderr: negative},
			[]string{snapshotWhat, "exited 1", strings.TrimSuffix(string(negative), "\n")}},
		"clean": {},
	}
	for name, v := range variants {
		t.Run(name, func(t *testing.T) {
			s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
				if name == "clean" {
					return
				}
				o.answer(f.Argv(zfs.CmdSnapshotTimes), v.res)
				o.answer(f.Argv(zfs.CmdSnapshotTimes, "z1/ds/appdata"), v.res)
				if name == "oversized" {
					o.answer(f.Argv(zfs.CmdSnapshotList), zfs.Result{Stdout: full})
				}
			})
			if name != "clean" {
				l := listDatasets(t, s, `{"limit":500}`)
				byName := map[string]spec.Dataset{}
				for _, d := range l.Items {
					byName[d.Name] = d
				}
				get := call[spec.Dataset](t, s, spec.DatasetsGet.Op(), `{"name":"z1/ds/appdata"}`)
				for _, r := range rows {
					checkDataset(t, byName[r[0]], r, stats, false)
				}
				i := slices.IndexFunc(rows, func(r []string) bool { return r[0] == "z1/ds/appdata" })
				checkDataset(t, get, rows[i], stats, false)
				for _, d := range append(l.Items, get) {
					for _, k := range []string{"snapshotCount", "newestSnapshotAt"} {
						for _, w := range v.reason {
							if r := d.Properties.Unavailable[k]; !strings.Contains(r, w) {
								t.Errorf("%s %s reason %q, want %q", d.Name, k, r, w)
							}
						}
					}
				}
			}
			from := len(s.ov.argvs())
			z := s.p.zfs.Load()
			if _, err := s.p.readPools(context.Background(), z); err != nil {
				t.Fatal(err)
			}
			if _, err := s.p.readDatasets(context.Background(), z); err != nil {
				t.Fatal(err)
			}
			if reads := readsSince(s, from); hasSnapshotListing(reads) || len(reads) != 3 {
				t.Errorf("health's reads %q, want three and no snapshot listing", reads)
			}
			from = len(s.ov.argvs())
			h := healthOf(t, s.d)
			if reads := readsSince(s, from); hasSnapshotListing(reads) || len(reads) == 0 {
				t.Errorf("storage.health.get read %q, want no snapshot listing", reads)
			}
			if got := sigsOf(h.Signals); !slices.Equal(got, clean) || h.Unavailable[featureZFS] != "" {
				t.Errorf("storage.health.get signals %v unavailable.zfs %q, want the clean capture's %v and zfs read", got,
					h.Unavailable[featureZFS], clean)
			}
			if name == "oversized" {
				_, err := s.invoke(spec.SnapshotsList.Op(), `{}`)
				pe := wantSlug(t, err, problem.DependencyUnavailable)
				if !strings.Contains(pe.Detail, "4 MiB") {
					t.Errorf("snapshots.list detail %q", pe.Detail)
				}
			}
		})
	}
}

// TC-STG-140: pools.list and datasets.list answer within 1 s on the home-server capture. Owner WI 4.2. Requirements: NFR-1.
func TestTCSTG140_PoolsListAndDatasetsListAnswerWithin(t *testing.T) {
	s := newZStack(t, nil)
	ctx := context.Background()
	for name, fn := range map[string]func() error{
		"pools.list":    func() error { _, err := s.p.poolsList(ctx, spec.PoolListInput{}); return err },
		"pools.get z1":  func() error { _, err := s.p.poolsGet(ctx, spec.PoolNameInput{Name: "z1"}); return err },
		"datasets.list": func() error { _, err := s.p.datasetsList(ctx, spec.DatasetListInput{}); return err },
	} {
		var took []time.Duration
		for range 20 {
			start := time.Now()
			if err := fn(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			took = append(took, time.Since(start))
		}
		slices.Sort(took)
		if p95 := took[18]; p95 >= 100*time.Millisecond {
			t.Errorf("%s p95 %s, want under 100ms", name, p95)
		}
	}
}

// TC-STG-E4: snapshot paging is stable when a snapshot appears between pages. Owner WI 4.2. Requirements: FR-8, NFR-3.
func TestTCSTGE4_SnapshotPagingIsStableWhenASnapshot(t *testing.T) {
	s := newZStack(t, nil, storagetest.Options{CapSysAdmin: true, Now: func() time.Time { return t0 }})
	order, _ := homeSnapshotOrder(t)
	ctx := context.Background()
	first, err := s.p.snapshotsList(ctx, spec.SnapshotListInput{Limit: 10})
	if err != nil || !slices.Equal(itemNames(first.Items), order[:10]) {
		t.Fatalf("page 1: %v %v", err, itemNames(first.Items))
	}
	const created = "z1/ds@nexus-tce4"
	if res, err := s.fake.Run(ctx, s.fake.Argv(zfs.CmdSnapshotCreate, "op-tce4", created)); err != nil || res.ExitCode != 0 {
		t.Fatalf("create: %v exit %d %s", err, res.ExitCode, res.Stderr)
	}
	var rest []string
	cursor := first.NextCursor
	for pages := 0; cursor != ""; pages++ {
		if pages > len(order) {
			t.Fatal("no last page: the cursor does not advance")
		}
		l, err := s.p.snapshotsList(ctx, spec.SnapshotListInput{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		rest = append(rest, itemNames(l.Items)...)
		cursor = l.NextCursor
	}
	if !slices.Equal(rest, order[10:]) || slices.Contains(rest, created) {
		t.Errorf("later pages: %d items (new one present: %v), want the %d older ones once each",
			len(rest), slices.Contains(rest, created), len(order)-10)
	}
	if l, _ := s.p.snapshotsList(ctx, spec.SnapshotListInput{Limit: 1}); itemNames(l.Items)[0] != created {
		t.Errorf("the new snapshot is not the newest: %v", itemNames(l.Items))
	}
}

// TC-STG-E11: pool and dataset names with dots and colons work everywhere. Owner WI 4.2. Requirements: FR-5, FR-7.
func TestTCSTGE11_PoolAndDatasetNamesWithDotsAnd(t *testing.T) {
	const pool, ds = "tank.v2:a", "tank.v2:a/x.y:z"
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		o.answer(f.Argv(zfs.CmdPoolNames), zfs.Result{Stdout: []byte(pool + "\n")})
		ver, pools := jsonMembers(t, home(t, "zfs/zpool-list.json"), "pools")
		o.answer(f.Argv(zfs.CmdPoolList, pool), zfs.Result{Stdout: jsonDoc(ver, "pools", pool, rename(pools["z1"], "z1", pool))})
		sver, spools := jsonMembers(t, home(t, "zfs/zpool-status-v.json"), "pools")
		o.answer(f.Argv(zfs.CmdPoolStatusVerbose, pool), zfs.Result{Stdout: jsonDoc(sver, "pools", pool, rename(spools["z1"], "z1", pool))})
		o.answer(f.Argv(zfs.CmdPoolGet, pool), zfs.Result{Stdout: rename(home(t, "zfs/zpool-get-z1.json"), "z1", pool)})
		dver, dss := jsonMembers(t, home(t, "zfs/zfs-list-datasets.json"), "datasets")
		o.answer(f.Argv(zfs.CmdDatasetList, ds), zfs.Result{Stdout: jsonDoc(dver, "datasets", ds, rename(dss["z1/ds/appdata"], "z1/ds/appdata", ds))})
		o.answer(f.Argv(zfs.CmdSnapshotTimes, ds), zfs.Result{Stdout: []byte(ds + "@s1\t1795000000\n")})
	})
	p := call[spec.Pool](t, s, spec.PoolsGet.Op(), fmt.Sprintf(`{"name":%q}`, pool))
	d := call[spec.Dataset](t, s, spec.DatasetsGet.Op(), fmt.Sprintf(`{"name":%q}`, ds))
	if p.Name != pool || p.Properties.Health != "online" || d.Name != ds || d.Properties.Pool != pool || *d.Properties.SnapshotCount != 1 {
		t.Errorf("pool %s %s, dataset %s %s", p.Name, p.Properties.Health, d.Name, d.Properties.Pool)
	}
	for _, name := range []string{pool, ds} {
		if !slices.ContainsFunc(s.ov.argvs(), func(a []string) bool { return a[len(a)-1] == name }) {
			t.Errorf("no argv ends with %q as one element: %q", name, s.ov.argvs())
		}
	}
}

// TC-STG-ERR1: a read command that exits non-zero answers 503 naming the tool and its first stderr line. Owner WI 4.2. Requirements: FR-4, FR-18, NFR-1.
func TestTCSTGERR1_AReadCommandThatExitsNonZero(t *testing.T) {
	line := strings.Repeat("e", 400)
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		o.answer(f.Argv(zfs.CmdDatasetList), zfs.Result{ExitCode: 2, Stderr: []byte(line + "\nsecond line\n")})
	})
	_, err := s.invoke(spec.DatasetsList.Op(), `{}`)
	pe := wantSlug(t, err, problem.DependencyUnavailable)
	typ, _ := problem.Lookup(problem.DependencyUnavailable)
	if pe.Status != 503 || pe.Title != typ.Title || pe.Detail != "zfs list exited 2: "+line[:200] {
		t.Errorf("problem %d %q %q", pe.Status, pe.Title, pe.Detail)
	}
}

// TC-STG-ERR2: a JSON parse failure is a 503, never a partial or empty list. Owner WI 4.2. Requirements: FR-3, FR-4.
func TestTCSTGERR2_AJSONParseFailureIsA503(t *testing.T) {
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		doc := home(t, "zfs/zpool-list.json")
		o.answer(f.Argv(zfs.CmdPoolList), zfs.Result{Stdout: doc[:len(doc)/2]})
	})
	body, err := s.invoke(spec.PoolsList.Op(), `{}`)
	pe := wantSlug(t, err, problem.DependencyUnavailable)
	if body != nil || pe.Status != 503 || !strings.Contains(pe.Detail, "zpool list output could not be parsed") {
		t.Errorf("body %v problem %d %q", body, pe.Status, pe.Detail)
	}
}

// TC-STG-ERR3: an unknown output_version major falls back to the text form and logs once. Owner WI 4.2. Requirements: FR-4, ASM-2.
func TestTCSTGERR3_AnUnknownOutputVersionMajorFallsBack(t *testing.T) {
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		o.answer(f.Argv(zfs.CmdPoolList), zfs.Result{Stdout: synthetic(t, "zpool-list-unknown-major.json")})
	})
	oracle := homePools(t)
	for range 3 {
		l := listPools(t, s, `{}`)
		if got := itemNames(l.Items); !slices.Equal(got, []string{"z1", "z2", "z3"}) {
			t.Fatalf("pools %v", got)
		}
		for _, it := range l.Items {
			checkPool(t, it, oracle[it.Name], true)
		}
	}
	text := zfs.Argv(s.fake.Paths(), zfs.Text, zfs.CmdPoolList)
	if n := countArgv(s.fake.Reads(), text); n != 3 {
		t.Errorf("%d text-form zpool list reads, want 3", n)
	}
	if n := countArgv(s.ov.argvs(), s.fake.Argv(zfs.CmdPoolList)); n != 1 {
		t.Errorf("%d JSON zpool list reads, want 1 (then the text form is kept)", n)
	}
	var logged int
	for _, r := range s.rec.all() {
		if strings.Contains(r.Message, "unknown zfs JSON output version") {
			logged++
		}
	}
	if logged != 1 {
		t.Errorf("%d log records about the unknown version, want 1", logged)
	}
}

// TC-STG-ERR12: a panic in a storage handler is recovered as an internal error and the provider keeps serving. Owner WI 4.2. Requirements: NFR-7.
func TestTCSTGERR12_APanicInAStorageHandlerIs(t *testing.T) {
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		o.answerFunc(f.Argv(zfs.CmdSnapshotList), func(context.Context, []string) (zfs.Result, error) {
			panic("tc-stg-err12")
		})
	})
	_, err := s.invoke(spec.SnapshotsList.Op(), `{}`)
	if pe := wantSlug(t, err, problem.InternalError); pe.Status != 500 || strings.Contains(pe.Detail, "tc-stg-err12") {
		t.Errorf("problem %d %q", pe.Status, pe.Detail)
	}
	if l := listPools(t, s, `{}`); len(l.Items) != 3 {
		t.Errorf("pools.list after the panic: %d items", len(l.Items))
	}
}

// ---- carried forward from WI 4.1 (handler level) ----

// TC-STG-114 (WI 4.1, the read's half): with zero imported pools,
// pools.list answers 200 with items [].
func TestTCSTG114_ZeroImportedPoolsListAnswersEmptyItems(t *testing.T) {
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		o.answer(f.Argv(zfs.CmdPoolNames), zfs.Result{})
		o.answer(f.Argv(zfs.CmdPoolList), zfs.Result{Stdout: []byte(`{"output_version":{"command":"zpool list","vers_major":0,"vers_minor":1},"pools":{}}` + "\n")})
		o.answer(f.Argv(zfs.CmdPoolStatus), zfs.Result{Stdout: []byte(`{"output_version":{"command":"zpool status","vers_major":0,"vers_minor":1},"pools":{}}` + "\n")})
	})
	l := listPools(t, s, `{}`)
	if b, _ := json.Marshal(l); string(b) != `{"items":[],"nextCursor":""}` {
		t.Errorf("pools.list = %s, want items []", b)
	}
}

// TC-STG-116 (WI 4.1, the values' half): with zfs version unreadable, the
// text form's pools equal TC-STG-125's oracle; the finished scan's values
// the text does not print are null with A-59's reason. Dates are in
// time.Local (A-60), which the zfs tests pin, so they are not compared.
func TestTCSTG116_TextFormPoolsEqualTheOracle(t *testing.T) {
	s := newZStack(t, func(f *storagetest.Fake, o *overlay) {
		o.answer(f.Argv(zfs.CmdVersion), zfs.Result{Stdout: []byte("garbage\n")})
		// zpool status -p is captured with -v only; the pools read asks
		// for it without -v, whose output differs only in the errors
		// section, which has no error on home-server.
		o.answer(zfs.Argv(f.Paths(), zfs.Text, zfs.CmdPoolStatus), zfs.Result{Stdout: home(t, "zfs/zpool-status-v.txt")})
	})
	if z := s.p.zfs.Load(); z.format != zfs.Text {
		t.Fatalf("format %v, want text", z.format)
	}
	l := listPools(t, s, `{}`)
	oracle := homePools(t)
	if len(l.Items) != 3 {
		t.Fatalf("pools %v", itemNames(l.Items))
	}
	for _, it := range l.Items {
		checkPool(t, it, oracle[it.Name], false)
		sc := it.Properties.Scan
		want := "not printed by zpool status (text form) for a scrub finished"
		for _, k := range []string{"examinedBytes", "toExamineBytes"} {
			if sc.Unavailable[k] != want {
				t.Errorf("%s %s reason %q, want %q", it.Name, k, sc.Unavailable[k], want)
			}
		}
		if sc.ExaminedBytes != nil || sc.PercentDone != nil || sc.Unavailable["percentDone"] == "" {
			t.Errorf("%s: examined %s percentDone %s reason %q", it.Name, fmtPtr(sc.ExaminedBytes), fmtPtr(sc.PercentDone),
				sc.Unavailable["percentDone"])
		}
	}
	for _, a := range s.ov.argvs() {
		if slices.Contains(a, "-j") {
			t.Errorf("a JSON read on the text form: %q", a)
		}
	}
}
