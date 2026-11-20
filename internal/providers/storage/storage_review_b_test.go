// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Regression tests of planning/storage-provider/code-review-b.md, CR-1 to
// CR-10: each fails on the code the review found (PRE_FIX_SHA a54647f) and
// passes on its fix. CR-7 (a Configure wakes RunBackground) is documented,
// not changed: D-12 and TC-STG-E12 pin the tick as the only start of a
// round.

package storage

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
	"github.com/mauktik/nexus/internal/providers/storage/vzdump"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// pageAll lists every backup with limit 1, following the cursors.
func pageAll(t *testing.T, s *fstack, input func(cursor string) string) []spec.Backup {
	t.Helper()
	var out []spec.Backup
	cursor := ""
	for range 50 {
		l := fcall[apitypes.List[spec.Backup]](t, s, spec.BackupsList.Op(), input(cursor))
		out = append(out, l.Items...)
		if cursor = l.NextCursor; cursor == "" {
			return out
		}
	}
	t.Fatal("the cursors do not end")
	return nil
}

// CR-1 and CR-9: an archive of one name and mtime in a directory and in a
// directory that has the first as a prefix (its dump/, a mirror copy, a
// configured path that is a prefix of another) is returned once by a paged
// listing, and has an ID of its own.
func TestCR1_BackupsListPagingOverDirectoriesThatArePrefixesOfEachOther(t *testing.T) {
	at := t0.Add(-time.Hour)
	arch := func(dir string) vz {
		return vz{typ: spec.GuestQemu, vmid: 101, stamp: at, mtime: at, size: 10, dir: dir}
	}
	cases := map[string]func(t *testing.T) []string{
		"a path and its dump/": func(t *testing.T) []string {
			return []string{vzTree(t, arch(""), arch("dump"))}
		},
		"a path and a mirror whose name extends it": func(t *testing.T) []string {
			tree := vzTree(t, arch(""))
			mirror := tree + "-mirror"
			storagetest.Build(t, mirror, arch("").nodes()...)
			return []string{tree, mirror}
		},
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			s := newFStack(t, fopts{vzdump: paths(t)})
			all := s.backups(t, `{"limit":500}`)
			if len(all) < 2 {
				t.Fatalf("limit 500 listed %d items, want at least 2", len(all))
			}
			paged := pageAll(t, s, func(c string) string {
				if c == "" {
					return `{"limit":1}`
				}
				return fmt.Sprintf(`{"limit":1,"cursor":%q}`, c)
			})
			if len(paged) != len(all) {
				t.Fatalf("limit 1 paging returned %d items, limit 500 returned %d", len(paged), len(all))
			}
			ids := map[string]string{}
			for i := range all {
				if paged[i].Properties.Directory != all[i].Properties.Directory || paged[i].Name != all[i].Name {
					t.Errorf("item %d paged = %s in %s, want %s in %s", i, paged[i].Name, paged[i].Properties.Directory,
						all[i].Name, all[i].Properties.Directory)
				}
				if prev, dup := ids[all[i].ID]; dup {
					t.Errorf("items in %s and %s share the ID %s", prev, all[i].Properties.Directory, all[i].ID)
				}
				ids[all[i].ID] = all[i].Properties.Directory
			}
		})
	}
}

// A cursor whose key was cut at maxArchiveKey keeps the prefix comparison,
// and a key exactly maxArchiveKey long that was not cut compares whole.
func TestCR1_ACutCursorKeyIsMarked(t *testing.T) {
	long := "/" + strings.Repeat("d", maxArchiveKey)
	r := archiveRef{dir: long, a: sampleArchive(t0)}
	k := archiveKey(r)
	if len(k) > maxArchiveKey {
		t.Fatalf("key is %d bytes, want at most %d", len(k), maxArchiveKey)
	}
	c, ok := splitArchiveKey(k)
	if !ok {
		t.Fatalf("a cut key %q does not decode", k)
	}
	if !c.atOrBefore(r) {
		t.Errorf("the item the cut key was made from is after its own cursor")
	}
	// A directory that extends the cut one is not after it: the key lost
	// what told them apart.
	longer := archiveRef{dir: long + "x", a: r.a}
	if !c.atOrBefore(longer) {
		t.Errorf("a directory extending the cut key is after the cursor; the cut key can only compare by prefix")
	}
	// An uncut key of exactly maxArchiveKey bytes compares whole.
	file := r.a.File
	dir := "/" + strings.Repeat("e", maxArchiveKey-len(fmt.Sprintf("%d %s /", r.a.ModifiedAt.UnixNano(), file)))
	exact := archiveRef{dir: dir, a: r.a}
	if k := archiveKey(exact); len(k) != maxArchiveKey {
		t.Fatalf("test setup: key of %d bytes, want %d", len(k), maxArchiveKey)
	}
	c, _ = splitArchiveKey(archiveKey(exact))
	next := archiveRef{dir: dir + "x", a: r.a}
	if c.atOrBefore(next) {
		t.Errorf("an uncut key of exactly maxArchiveKey bytes drops the next directory")
	}
}

func sampleArchive(at time.Time) vzdump.Archive {
	return vzdump.Archive{File: "vzdump-qemu-101-2026_11_18_03_00_00.vma.zst", ModifiedAt: at}
}

// CR-2: the share-to-dataset match uses the last dataset listing only while
// it is fresh, and the health read keeps the listing fresh.
func TestCR2_TheShareDatasetMatchDoesNotUseAStaleListing(t *testing.T) {
	base := storagetest.TempDir(t)
	tm1, tm2 := base+"/tm1", base+"/tm2"
	storagetest.Build(t, base, storagetest.Node{Path: "tm1", Type: storagetest.Dir},
		storagetest.Node{Path: "tm2", Type: storagetest.Dir})
	fake := storagetest.NewFake(t, "home-server")
	ov := newOverlay(fake)
	form := "zfs/zfs-list-datasets.txt"
	if fake.Format() == zfs.JSON {
		form = "zfs/zfs-list-datasets.json"
	}
	orig := home(t, form)
	rewritten := bytes.ReplaceAll(bytes.ReplaceAll(orig, []byte("/media/timemachine2"), []byte(tm2)),
		[]byte("/media/timemachine"), []byte(tm1))
	ov.answer(fake.Argv(zfs.CmdDatasetList), zfs.Result{Stdout: rewritten})
	s := newFStack(t, fopts{tm: []string{tm1}, fsys: homeFS(t), run: ov.run})

	if _, err := s.p.datasetsList(context.Background(), spec.DatasetListInput{}); err != nil {
		t.Fatal(err)
	}
	if sp := shareByPath(t, s.shares(t), tm1); sp.Dataset == nil || *sp.Dataset != "z3/ds/timemachine" {
		t.Fatalf("with a fresh listing the share's dataset is %v, want z3/ds/timemachine", sp.Dataset)
	}

	// The host changes and 72 h pass: no dataset is mounted at the share.
	ov.answer(fake.Argv(zfs.CmdDatasetList), zfs.Result{Stdout: orig})
	s.clk.set(t0.Add(72 * time.Hour))
	sp := shareByPath(t, s.shares(t), tm1)
	if sp.Dataset != nil || sp.QuotaUsedPercent != nil {
		t.Errorf("72 h after the listing the share's dataset is %v with quotaUsedPercent %v, want null from a fresh read",
			sp.Dataset, sp.QuotaUsedPercent)
	}
	wantReason(t, sp.Unavailable, "dataset", reasonNoDataset)
}

func TestCR2_TheHealthReadKeepsTheDatasetListingFresh(t *testing.T) {
	base := storagetest.TempDir(t)
	tm1 := base + "/tm1"
	storagetest.Build(t, base, storagetest.Node{Path: "tm1", Type: storagetest.Dir})
	fake := storagetest.NewFake(t, "home-server")
	ov := newOverlay(fake)
	s := newFStack(t, fopts{tm: []string{tm1}, fsys: homeFS(t), run: ov.run})
	listings := func() int {
		n := 0
		for _, argv := range ov.argvs() {
			if slices.Equal(argv, fake.Argv(zfs.CmdDatasetList)) {
				n++
			}
		}
		return n
	}
	healthOf(t, s.d)
	before := listings()
	if before == 0 {
		t.Fatal("the health read did not list datasets")
	}
	s.shares(t)
	if after := listings(); after != before {
		t.Errorf("timemachine.list ran %d more dataset listings after a health read, want 0 from the fresh listing",
			after-before)
	}
}

// CR-3: a bundle reached after its share's budget is spent keeps its last
// figure, and the next round starts with the bundles that were skipped.
func TestCR3_ABundleWalkedAfterTheBudgetIsSpentKeepsItsLastFigure(t *testing.T) {
	share := bundleTree(t, 30, map[string]time.Time{"a": t0, "b": t0})
	s := newFStack(t, fopts{tm: []string{share}})
	s.background(t)
	s.wk.waitEnded(t, share, 1)
	full, ok := s.p.walks.size(share, "b.sparsebundle")
	if !ok || full.reason != "" || full.size.Truncated || full.size.Bytes == 0 {
		t.Fatalf("round 1 of b = %+v, %v, want a complete figure", full, ok)
	}

	// Round 2: the budget goes to a.
	s.p.walkEntries = 40
	s.tk.next(t) <- t0
	s.wk.waitEnded(t, share, 2)
	b, ok := s.p.walks.size(share, "b.sparsebundle")
	if !ok || b.size != full.size || b.reason != "" {
		t.Errorf("round 2 replaced b's complete figure %+v with %+v", full.size, b.size)
	}

	// Round 3: b was skipped, so it is walked first, and sees a new file.
	storagetest.Build(t, share, storagetest.Node{Path: "b.sparsebundle/bands/ff", Content: bytes.Repeat([]byte("x"), 70000)})
	want := allocTree(t, share+"/b.sparsebundle/bands")
	s.tk.next(t) <- t0
	s.wk.waitEnded(t, share, 3)
	b, _ = s.p.walks.size(share, "b.sparsebundle")
	if b.size.Bytes != want || b.size.Truncated {
		t.Errorf("round 3 of b = %d bytes truncated %v, want the complete %d: the skipped bundle starts the round",
			b.size.Bytes, b.size.Truncated, want)
	}
}

// CR-4: zfs-unresponsive follows the kind of each failed read, not the
// words of its reason, and a read cut by the caller's deadline does not
// hide another read's own failure.
func TestCR4_UnresponsiveFollowsEachReadsKind(t *testing.T) {
	unresponsive := func(h spec.HealthProperties) []apitypes.HealthSignal {
		var out []apitypes.HealthSignal
		for _, sg := range h.Signals {
			if sg.ID == signalZFSUnresponsive {
				out = append(out, sg)
			}
		}
		return out
	}
	for _, c := range []struct {
		name string
		err  error
	}{
		{"every process slot is taken", fmt.Errorf("%w: waited", zfs.ErrBusy)},
		{"the tool could not be started", fmt.Errorf("%w: no zpool", zfs.ErrNotStarted)},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
				o.answerFunc(f.Argv(zfs.CmdPoolList), func(context.Context, []string) (zfs.Result, error) {
					return zfs.Result{}, c.err
				})
			}})
			u := unresponsive(s.health(t))
			if len(u) != 1 || u[0].Severity != apitypes.SeverityCritical || !strings.Contains(u[0].Message, "zpool list") {
				t.Errorf("zfs-unresponsive %+v, want one critical naming zpool list", u)
			}
		})
	}

	t.Run("one read fails fast and another is cut by the caller's deadline", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		stderr := homeStderr(t, "zfs/negative-zpool-list.json")
		s := newHStack(t, hopts{zfs: true, setup: func(f *storagetest.Fake, o *overlay) {
			o.answer(f.Argv(zfs.CmdPoolList), zfs.Result{ExitCode: 1, Stderr: stderr})
			f.Stuck(f.Argv(zfs.CmdDatasetList), release)
		}})
		setCommandTimeout(s.p, 5*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		rep, err := s.p.HealthSignals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, sg := range rep.Signals {
			if sg.ID == signalZFSUnresponsive {
				found = append(found, sg.Message)
			}
		}
		if len(found) != 1 || !strings.Contains(found[0], "zpool list exited 1") {
			t.Errorf("zfs-unresponsive messages %q, want one for zpool list exited 1 and none for the read the deadline cut", found)
		}
	})
}

// CR-5: the helper refuses to send a scrub to a pool whose health, read
// immediately before the send, is not online (or degraded with force).
func TestCR5_TheHelperReChecksPoolHealthBeforeSending(t *testing.T) {
	for _, c := range []struct {
		name, file, word string
		force            bool
		refused          bool
		health           string
	}{
		{"suspended", "zpool-status-suspended.json", "SUSPENDED", false, true, "suspended"},
		{"suspended with force", "zpool-status-suspended.json", "SUSPENDED", true, true, "suspended"},
		{"degraded without force", "zpool-status-degraded-mirror.json", "DEGRADED", false, true, "degraded"},
		{"degraded with force", "zpool-status-degraded-mirror.json", "DEGRADED", true, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newWStack(t, wopts{cfg: scrubCfg("z1")})
			s.prepareStart(testOpID, "z1", c.force) // Admission saw it online
			status := s.fake.Argv(zfs.CmdPoolStatus, "z1")
			sick := statusWithHealth(t, c.file, c.word)
			s.setHelperRun(func(ctx context.Context, argv []string) (zfs.Result, error) {
				if slices.Equal(argv, status) {
					return zfs.Result{Stdout: sick}, nil
				}
				return s.fake.Run(ctx, argv)
			})
			_, res := s.helper(testOpID)
			if !c.refused {
				if res.Error != nil && res.Error.Slug == problem.PoolUnhealthy {
					t.Errorf("a degraded pool with force was refused: %+v", res.Error)
				}
				if len(s.fake.Writes()) == 0 {
					t.Errorf("a degraded pool with force was sent nothing")
				}
				return
			}
			he := helperProblem(t, res, problem.PoolUnhealthy)
			if !strings.Contains(he.Detail, "nothing was sent") {
				t.Errorf("detail %q, want it to say nothing was sent", he.Detail)
			}
			if w := s.fake.Writes(); len(w) != 0 {
				t.Errorf("writes %q, want none", w)
			}
			wantExtRaw(t, he, "health", c.health)
		})
	}
}

func wantExtRaw(t *testing.T, he *helperError, key, want string) {
	t.Helper()
	if got := string(he.Ext[key]); got != fmt.Sprintf("%q", want) {
		t.Errorf("extension %s = %s, want %q", key, got, want)
	}
}

// CR-6: the confirm poll bounds each status read by the confirm time that
// is left, so the helper ends within helperTimeout.
func TestCR6_EachConfirmReadIsBoundedByTheConfirmTimeLeft(t *testing.T) {
	s := newWStack(t, wopts{cfg: scrubCfg("z1")})
	s.fake.StaleFinished("z1")
	start := s.fake.Argv(zfs.CmdScrubStart, "z1")
	status := s.fake.Argv(zfs.CmdPoolStatus, "z1")
	type obs struct{ bound, left time.Duration }
	var mu sync.Mutex
	var seen []obs
	sent := false
	s.setHelperRun(func(ctx context.Context, argv []string) (zfs.Result, error) {
		res, err := s.fake.Run(ctx, argv)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case slices.Equal(argv, start):
			sent = true
			s.fake.StaleFinished("z1") // Accepted, and zpool status never changes
		case sent && slices.Equal(argv, status):
			dl, ok := ctx.Deadline()
			if !ok {
				dl = time.Now().Add(time.Hour)
			}
			seen = append(seen, obs{bound: time.Until(dl), left: tw0.Add(spec.ConfirmTimeout).Sub(s.clk.Now())})
		}
		return res, err
	})
	s.prepareStart(testOpID, "z1", false)
	_, res := s.helper(testOpID)
	helperProblem(t, res, problem.OutcomeUnknown)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 30 {
		t.Fatalf("%d confirm reads, want the poll's 60", len(seen))
	}
	for i, o := range seen {
		if o.bound > o.left+time.Second {
			t.Errorf("confirm read %d was bound by %s with %s of the confirm time left", i, o.bound.Round(time.Second), o.left)
		}
	}
}

// CR-8: the health read leaves the notes alone, and a backups.list page
// reads the notes of the items it keeps and at most one chunk more.
func TestCR8_HealthDoesNotReadNotes(t *testing.T) {
	tree := vzTree(t, summaryTree(errLog(106))...)
	s := newHStack(t, hopts{vzdump: []string{tree}})
	s.health(t)
	if n := s.op.count(tree, "Open", suffix(".notes")); n != 0 {
		t.Errorf("the health read opened %d notes files, want 0", n)
	}
	// The summary still names the guests.
	f := newFStack(t, fopts{vzdump: []string{tree}})
	named := 0
	for _, g := range f.summary(t).Guests {
		if g.GuestName != nil {
			named++
		}
	}
	if named == 0 {
		t.Errorf("the summary names no guest: it must still read the notes")
	}
}

func TestCR8_ABackupsPageReadsNotesInChunks(t *testing.T) {
	var log bytes.Buffer
	for i := range spec.MaxLogErrorLines + 2 {
		log.WriteString(pad(fmt.Sprintf("2026-11-17 03:04:%02d ERROR: line %d ", i, i), 600) + "\n")
	}
	notes := []byte(strings.Repeat("€", spec.NotesMaxBytes/3) + "\n")
	var arch []vz
	for i := range 300 {
		at := t0.Add(-time.Duration(i+1) * time.Minute)
		arch = append(arch, vz{typ: spec.GuestQemu, vmid: 100 + i, stamp: at, mtime: at, size: 1 << 40,
			notes: notes, log: log.Bytes()})
	}
	tree := vzTree(t, arch...)
	s := newFStack(t, fopts{vzdump: []string{tree}})
	l := fcall[apitypes.List[spec.Backup]](t, s, spec.BackupsList.Op(), `{"limit":500}`)
	if len(l.Items) == 0 || len(l.Items) >= 300 || l.NextCursor == "" {
		t.Fatalf("page 1 holds %d items with nextCursor %q, want a page cut by the byte budget", len(l.Items), l.NextCursor)
	}
	read := s.op.count(tree, "Open", suffix(".notes"))
	const chunk = 50 // backupFillChunk
	if read > len(l.Items)+chunk {
		t.Errorf("page 1 of %d items read %d notes, want at most the page and one chunk of %d", len(l.Items), read, chunk)
	}
}

// CR-10: a caller whose ctx has ended starts no flight, and a request whose
// ctx ends stops taking jobs.
func TestCR10_ACallerWhoseCtxHasEndedStartsNoFlight(t *testing.T) {
	fl := newFlightsFor(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ran atomic.Int32
	_, err := bounded(ctx, fl, "/p", "scan /p", func() (int, error) { ran.Add(1); return 1, nil })
	var te zfs.TimeoutError
	if !asTimeout(err, &te) {
		t.Fatalf("err %v, want a TimeoutError", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := ran.Load(); n != 0 {
		t.Errorf("fn ran %d times for a caller whose ctx had ended, want 0", n)
	}
	if n := fl.count(); n != 0 {
		t.Errorf("%d flights in progress, want 0", n)
	}
}

func asTimeout(err error, te *zfs.TimeoutError) bool {
	t, ok := err.(zfs.TimeoutError)
	if ok {
		*te = t
	}
	return ok
}

func TestCR10_ARequestWhoseCtxEndsStopsReading(t *testing.T) {
	var arch []vz
	for i := range 40 {
		at := t0.Add(-time.Duration(i+1) * time.Minute)
		arch = append(arch, vz{typ: spec.GuestQemu, vmid: 100 + i, stamp: at, mtime: at, size: 10,
			notes: []byte("n\n"), log: okLog(100 + i)})
	}
	tree := vzTree(t, arch...)
	s := newFStack(t, fopts{vzdump: []string{tree}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var opens atomic.Int32
	inner := s.p.openRoot
	s.p.openRoot = func(path string) (Root, error) {
		// The scan is the first open; the first read ends the request.
		if opens.Add(1) == 2 {
			cancel()
		}
		return inner(path)
	}
	_, _ = s.p.backupsList(ctx, spec.BackupListInput{Limit: 40})
	time.Sleep(100 * time.Millisecond)
	// The scan, the read that ended the request and one on the other worker.
	if n := opens.Load(); n > 4 {
		t.Errorf("%d roots opened for a request whose ctx ended at the first read, want at most 4", n)
	}
}
