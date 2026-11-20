// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-100 to TC-STG-109
// (WI 3.4): Bundles, ReadBundle, LastCompletion and WalkBands over real temp
// trees opened through os.Root (F-TREE, storagetest/trees.go), with a
// recording opener (F-OPENER) where a test checks what is opened.

package timemachine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/storage/capturegate"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
)

// openRoot opens dir as an os.Root closed at cleanup.
func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("close root: %v", err)
		}
	})
	return r
}

// recOpener (F-OPENER) wraps an os.Root and forwards every call, recording
// each Open (direct or through FS) with its name, the bytes read from each
// file opened through FS, and each Lstat and Stat. onLstat, when set, runs
// before every Lstat is forwarded.
type recOpener struct {
	*os.Root
	mu      sync.Mutex
	opens   []string
	lstats  []string
	stats   []string
	read    map[string]int64
	onLstat func(name string)
}

func newRecOpener(r *os.Root) *recOpener {
	return &recOpener{Root: r, read: map[string]int64{}}
}

func (o *recOpener) add(list *[]string, name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	*list = append(*list, path.Clean(name))
}

func (o *recOpener) Open(name string) (*os.File, error) {
	o.add(&o.opens, name)
	return o.Root.Open(name)
}

// OpenFile records the open the way Open and FS().Open do; WrapFile then
// counts the reads of the file.
func (o *recOpener) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	o.add(&o.opens, name)
	return o.Root.OpenFile(name, flag, perm)
}

func (o *recOpener) WrapFile(name string, f *os.File) fs.File {
	return &recFile{File: f, o: o, name: path.Clean(name)}
}

func (o *recOpener) Lstat(name string) (fs.FileInfo, error) {
	o.add(&o.lstats, name)
	if o.onLstat != nil {
		o.onLstat(name)
	}
	return o.Root.Lstat(name)
}

func (o *recOpener) Stat(name string) (fs.FileInfo, error) {
	o.add(&o.stats, name)
	return o.Root.Stat(name)
}

func (o *recOpener) FS() fs.FS { return recFS{o} }

func (o *recOpener) opened() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.opens)
}

func (o *recOpener) bytesRead(name string) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.read[name]
}

// nonDirOpens is every recorded open whose target is not a directory, as
// the root sees it now (Lstat: a symlink counts as a non-directory).
func (o *recOpener) nonDirOpens(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, name := range o.opened() {
		fi, err := o.Root.Lstat(name)
		if err == nil && fi.IsDir() {
			continue
		}
		out = append(out, name)
	}
	return out
}

// recFS is the recording opener's FS: it records opens and counts reads,
// and forwards ReadDir so fs.WalkDir lists directories through it.
type recFS struct{ o *recOpener }

func (f recFS) Open(name string) (fs.File, error) {
	f.o.add(&f.o.opens, name)
	file, err := f.o.Root.FS().Open(name)
	if err != nil {
		return nil, err
	}
	return &recFile{File: file, o: f.o, name: path.Clean(name)}, nil
}

type recFile struct {
	fs.File
	o    *recOpener
	name string
}

func (f *recFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.o.mu.Lock()
	f.o.read[f.name] += int64(n)
	f.o.mu.Unlock()
	return n, err
}

func (f *recFile) ReadDir(n int) ([]fs.DirEntry, error) {
	d, ok := f.File.(fs.ReadDirFile)
	if !ok {
		return nil, errors.New("not a directory")
	}
	return d.ReadDir(n)
}

// allocated is st_blocks × 512 of p (not following a symlink).
func allocated(t *testing.T, p string) uint64 {
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

// allocatedTree is st_blocks × 512 summed over dir and everything under it,
// never following a symlink, the way `du -s` counts.
func allocatedTree(t *testing.T, dir string) uint64 {
	t.Helper()
	var sum uint64
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sum += allocated(t, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestTCSTG100_BundlesListsSparsebundleAndBackupbundleDirectoriesDirectly(t *testing.T) {
	dir := storagetest.TempDir(t)
	var nodes []storagetest.Node
	nodes = append(nodes, storagetest.BundleNodes("Mac-A.sparsebundle")...)
	nodes = append(nodes, storagetest.BundleNodes("Mac B.backupbundle")...)
	nodes = append(nodes, storagetest.BundleNodes("user/Mac-C.sparsebundle")...)
	nodes = append(nodes,
		storagetest.Node{Path: "x.sparsebundle", Content: []byte("a file")},
		storagetest.Node{Path: ".DS_Store", Content: []byte{0, 0, 0, 1}},
		storagetest.Node{Path: ".sparsebundle", Type: storagetest.Dir},
		storagetest.Node{Path: "Mac-D.sparsebundle.old", Type: storagetest.Dir},
		storagetest.Node{Path: "link.sparsebundle", Type: storagetest.Symlink, Target: "Mac-A.sparsebundle"},
	)
	storagetest.Build(t, dir, nodes...)

	got, truncated, err := Bundles(openRoot(t, dir), spec.MaxBundles)
	if err != nil {
		t.Fatalf("Bundles: %v", err)
	}
	if truncated {
		t.Error("truncated with two bundles")
	}
	if want := []string{"Mac B.backupbundle", "Mac-A.sparsebundle"}; !slices.Equal(got, want) {
		t.Fatalf("bundle directories %q, want %q", got, want)
	}
	var names []string
	for _, d := range got {
		names = append(names, Name(d))
	}
	if want := []string{"Mac B", "Mac-A"}; !slices.Equal(names, want) {
		t.Errorf("names %q, want %q (suffix removed)", names, want)
	}

	// 300 bundle directories: 256 listed and truncated.
	many := storagetest.TempDir(t)
	for i := range 300 {
		storagetest.Build(t, many, storagetest.Node{Path: fmt.Sprintf("mac-%03d.sparsebundle", i), Type: storagetest.Dir})
	}
	got, truncated, err = Bundles(openRoot(t, many), spec.MaxBundles)
	if err != nil {
		t.Fatalf("Bundles: %v", err)
	}
	if len(got) != spec.MaxBundles || !truncated {
		t.Errorf("300 bundles: %d listed, truncated %v; want %d and true", len(got), truncated, spec.MaxBundles)
	}
	if !slices.IsSorted(got) {
		t.Error("bundles not sorted")
	}

	// Exactly the bound: not truncated.
	exact := storagetest.TempDir(t)
	for i := range spec.MaxBundles {
		storagetest.Build(t, exact, storagetest.Node{Path: fmt.Sprintf("mac-%03d.backupbundle", i), Type: storagetest.Dir})
	}
	got, truncated, err = Bundles(openRoot(t, exact), spec.MaxBundles)
	if err != nil || len(got) != spec.MaxBundles || truncated {
		t.Errorf("256 bundles: %d listed, truncated %v, err %v", len(got), truncated, err)
	}

	// A share path that is a file is an error.
	fileRoot := storagetest.TempDir(t)
	storagetest.Build(t, fileRoot, storagetest.Node{Path: "f", Content: []byte("x")})
	if _, _, err := Bundles(subOpener{openRoot(t, fileRoot), "f"}, spec.MaxBundles); err == nil {
		t.Error("Bundles of a file: no error")
	}
}

// subOpener presents the entry sub of an os.Root as the root's ".".
type subOpener struct {
	r   *os.Root
	sub string
}

func (s subOpener) Open(name string) (*os.File, error) { return s.r.Open(path.Join(s.sub, name)) }
func (s subOpener) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return s.r.OpenFile(path.Join(s.sub, name), flag, perm)
}
func (s subOpener) Stat(name string) (fs.FileInfo, error)  { return s.r.Stat(path.Join(s.sub, name)) }
func (s subOpener) Lstat(name string) (fs.FileInfo, error) { return s.r.Lstat(path.Join(s.sub, name)) }
func (s subOpener) FS() fs.FS {
	sub, err := fs.Sub(s.r.FS(), s.sub)
	if err != nil {
		panic(err)
	}
	return sub
}

func TestTCSTG101_LastCompletionReadsTheLatestCompletionDateFrom(t *testing.T) {
	d1 := time.Date(2026, 11, 10, 3, 4, 5, 0, time.UTC)
	d2 := time.Date(2026, 11, 18, 22, 1, 0, 0, time.UTC) // latest
	d3 := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

	t.Run("several dates out of order", func(t *testing.T) {
		got, reason := LastCompletion(storagetest.SnapshotHistoryXML(d1, d2, d3))
		if got == nil || !got.Equal(d2) || got.Location() != time.UTC || reason != "" {
			t.Fatalf("got %v %q, want %v in UTC", got, reason, d2)
		}
	})
	t.Run("date with an offset is returned in UTC", func(t *testing.T) {
		doc := `<plist><dict><key>com.apple.backupd.SnapshotCompletionDate</key><date>2026-11-18T23:00:00+02:00</date></dict></plist>`
		got, reason := LastCompletion([]byte(doc))
		want := time.Date(2026, 11, 18, 21, 0, 0, 0, time.UTC)
		if got == nil || !got.Equal(want) || got.Location() != time.UTC {
			t.Fatalf("got %v %q, want %v UTC", got, reason, want)
		}
	})
	t.Run("deeply nested pair is found", func(t *testing.T) {
		doc := `<?xml version="1.0"?><plist><array><dict><key>a</key><array><dict>` +
			`<key>com.apple.backupd.SnapshotCompletionDate</key>` + "\n\t" + `<date>2026-11-01T00:00:00Z</date>` +
			`</dict></array></dict></array></plist>`
		got, _ := LastCompletion([]byte(doc))
		if got == nil || !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("a date not after the key is not taken", func(t *testing.T) {
		doc := `<plist><dict><key>other</key><date>2026-11-01T00:00:00Z</date>` +
			`<key>com.apple.backupd.SnapshotCompletionDate</key><string>2026-11-02T00:00:00Z</string></dict></plist>`
		got, reason := LastCompletion([]byte(doc))
		if got != nil || reason != ReasonNoCompletionDate {
			t.Fatalf("got %v %q, want nil %q", got, reason, ReasonNoCompletionDate)
		}
	})
	t.Run("no such key", func(t *testing.T) {
		got, reason := LastCompletion(storagetest.SnapshotHistoryXML())
		if got != nil || reason != "no SnapshotCompletionDate" {
			t.Fatalf("got %v %q", got, reason)
		}
	})
	t.Run("malformed XML", func(t *testing.T) {
		for _, doc := range []string{
			`<plist><dict><key>com.apple.backupd.SnapshotCompletionDate</key><date>2026-11-01T00:00:00Z</date>`,
			`<plist><dict></array></plist>`,
			`<plist><dict><key>com.apple.backupd.SnapshotCompletionDate</key><date>not a date</date></dict></plist>`,
		} {
			got, reason := LastCompletion([]byte(doc))
			if got != nil || reason == "" || reason == ReasonNoCompletionDate {
				t.Errorf("%q: got %v %q, want nil with a parse reason", doc, got, reason)
			}
		}
	})
	t.Run("entity declarations are neither expanded nor fetched", func(t *testing.T) {
		secret := storagetest.TempDir(t)
		storagetest.Build(t, secret, storagetest.Node{Path: "secret", Content: []byte("NX-ENTITY-SECRET")})
		decl := `<?xml version="1.0"?>` + "\n" +
			`<!DOCTYPE plist [<!ENTITY ext SYSTEM "file://` + secret + `/secret"><!ENTITY lol "lollollol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;">]>` + "\n"
		body := `<key>com.apple.backupd.SnapshotCompletionDate</key><date>2026-11-01T00:00:00Z</date>`
		// Declared, unused: the document parses and gives its date.
		got, reason := LastCompletion([]byte(decl + `<plist><dict>` + body + `</dict></plist>`))
		if got == nil || !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("unused entity: got %v %q", got, reason)
		}
		// Used: a parse reason; nothing is expanded or fetched.
		for _, use := range []string{"&ext;", "&lol2;"} {
			got, reason := LastCompletion([]byte(decl + `<plist><dict>` + body + `<key>x</key><string>` + use + `</string></dict></plist>`))
			if got != nil || reason == "" || reason == ReasonNoCompletionDate {
				t.Errorf("%s: got %v %q, want nil with a parse reason", use, got, reason)
			}
			if strings.Contains(reason, "NX-ENTITY-SECRET") || strings.Contains(reason, "lollollol") {
				t.Errorf("%s: reason %q carries expanded entity text", use, reason)
			}
		}
	})
	t.Run("binary plist", func(t *testing.T) {
		for _, b := range [][]byte{
			append([]byte(storagetest.BinaryPlistMagic), 0xd1, 0x01, 0x02, 0x55, 0xff),
			[]byte(storagetest.BinaryPlistMagic),
			[]byte("bplist01\x00\x00"),
		} {
			got, reason := LastCompletion(b)
			if got != nil || reason != "binary plist not parsed" {
				t.Errorf("%q: got %v %q, want nil %q", b, got, reason, "binary plist not parsed")
			}
		}
	})
	t.Run("empty", func(t *testing.T) {
		got, reason := LastCompletion(nil)
		if got != nil || reason == "" {
			t.Errorf("got %v %q", got, reason)
		}
	})
}

func TestTCSTG102_APlistOver1MiBIsNot(t *testing.T) {
	const name = "Mac.sparsebundle"
	dates := make([]time.Time, 0, 20000)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range cap(dates) {
		dates = append(dates, base.Add(time.Duration(i)*time.Hour))
	}
	big := storagetest.SnapshotHistoryXML(dates...)
	for len(big) < 3<<20 {
		big = storagetest.SnapshotHistoryXML(append(dates, dates...)...)
		dates = append(dates, dates...)
	}
	if got, _ := LastCompletion(big); got == nil {
		t.Fatal("the 3 MiB fixture is not a valid history plist")
	}
	mt := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	dir := storagetest.TempDir(t)
	nodes := append(storagetest.BundleNodes(name),
		storagetest.Node{Path: name + "/" + SnapshotHistoryFile, Content: big, ModTime: mt},
		storagetest.Node{Path: name + "/Info.plist", Content: []byte("<plist/>"), ModTime: mt.Add(-24 * time.Hour)})
	storagetest.Build(t, dir, nodes...)

	rec := newRecOpener(openRoot(t, dir))
	b := ReadBundle(rec, name)
	if b.LastBackupAt != nil {
		t.Errorf("LastBackupAt %v, want nil", b.LastBackupAt)
	}
	if !strings.Contains(b.LastBackupReason, "1 MiB") {
		t.Errorf("reason %q does not name the 1 MiB bound", b.LastBackupReason)
	}
	if n := rec.bytesRead(name + "/" + SnapshotHistoryFile); n > MaxPlistBytes+1 {
		t.Errorf("read %d bytes of the plist, want at most %d", n, MaxPlistBytes+1)
	}
	if b.LastModifiedAt == nil || !b.LastModifiedAt.Equal(mt) {
		t.Errorf("LastModifiedAt %v, want the plist's mtime %v", b.LastModifiedAt, mt)
	}

	// A plist at exactly the bound is read and parsed.
	exact := SnapshotHistoryXMLPadded(t, MaxPlistBytes, base)
	dir2 := storagetest.TempDir(t)
	storagetest.Build(t, dir2, append(storagetest.BundleNodes(name),
		storagetest.Node{Path: name + "/" + SnapshotHistoryFile, Content: exact})...)
	b = ReadBundle(openRoot(t, dir2), name)
	if b.LastBackupAt == nil || !b.LastBackupAt.Equal(base) {
		t.Errorf("a plist of exactly 1 MiB: %v %q, want %v", b.LastBackupAt, b.LastBackupReason, base)
	}
}

// SnapshotHistoryXMLPadded is a history plist holding date, padded with
// an XML comment to exactly size bytes.
func SnapshotHistoryXMLPadded(t *testing.T, size int, date time.Time) []byte {
	t.Helper()
	doc := storagetest.SnapshotHistoryXML(date)
	pad := size - len(doc) - len("<!---->")
	if pad < 0 {
		t.Fatal("size too small")
	}
	out := append(doc, "<!--"+strings.Repeat("x", pad)+"-->"...)
	if len(out) != size {
		t.Fatalf("padded to %d, want %d", len(out), size)
	}
	return out
}

func TestTCSTG103_TheMtimeFallbackIsTheNewestOf(t *testing.T) {
	const name = "Mac.sparsebundle"
	T := time.Now().Truncate(time.Second)
	day := 24 * time.Hour
	dir := storagetest.TempDir(t)
	nodes := storagetest.BundleNodes(name)
	nodes = append(nodes,
		storagetest.Node{Path: name + "/" + SnapshotHistoryFile, Content: []byte(storagetest.BinaryPlistMagic + "\x00\x01"), ModTime: T.Add(-3 * day)},
		storagetest.Node{Path: name + "/com.apple.TimeMachine.Results.plist", Content: []byte("<plist/>"), ModTime: T.Add(-1 * day)},
		storagetest.Node{Path: name + "/Info.plist", Content: []byte("<plist/>"), ModTime: T.Add(-10 * day)},
		storagetest.Node{Path: name + "/MachineID.plist", Content: []byte("NX-MACHINE-UUID"), ModTime: T},
		storagetest.Node{Path: name + "/lock", Content: []byte{}, ModTime: T},
		storagetest.Node{Path: name + "/token", Content: []byte{}, ModTime: T},
		storagetest.Node{Path: name + "/Info.bckup", Content: []byte("<plist/>"), ModTime: T},
		storagetest.Node{Path: name + "/bands", Type: storagetest.Dir, ModTime: T},
		storagetest.Node{Path: name, Type: storagetest.Dir, ModTime: T},
	)
	storagetest.Build(t, dir, nodes...)
	rec := newRecOpener(openRoot(t, dir))
	b := ReadBundle(rec, name)
	if b.LastBackupAt != nil || b.LastBackupReason != "binary plist not parsed" {
		t.Errorf("got %v %q, want nil %q", b.LastBackupAt, b.LastBackupReason, "binary plist not parsed")
	}
	if b.LastModifiedAt == nil || !b.LastModifiedAt.Equal(T.Add(-day)) {
		t.Errorf("LastModifiedAt %v, want %v (Results.plist; MachineID.plist not considered)", b.LastModifiedAt, T.Add(-day))
	}
	if b.Name != "Mac" {
		t.Errorf("Name %q, want Mac", b.Name)
	}
	if got := rec.nonDirOpens(t); !slices.Equal(got, []string{name + "/" + SnapshotHistoryFile}) {
		t.Errorf("non-directory opens %q", got)
	}

	// Only Info.plist: its mtime, and the history plist's absence as the reason.
	only := storagetest.TempDir(t)
	storagetest.Build(t, only,
		storagetest.Node{Path: name + "/Info.plist", Content: []byte("<plist/>"), ModTime: T.Add(-10 * day)},
		storagetest.Node{Path: name + "/MachineID.plist", Content: []byte("x"), ModTime: T},
		storagetest.Node{Path: name + "/bands", Type: storagetest.Dir},
	)
	b = ReadBundle(openRoot(t, only), name)
	if b.LastModifiedAt == nil || !b.LastModifiedAt.Equal(T.Add(-10*day)) {
		t.Errorf("only Info.plist: LastModifiedAt %v, want %v", b.LastModifiedAt, T.Add(-10*day))
	}
	if b.LastBackupAt != nil || b.LastBackupReason != ReasonNoHistory {
		t.Errorf("only Info.plist: %v %q, want nil %q", b.LastBackupAt, b.LastBackupReason, ReasonNoHistory)
	}

	// A symlinked Results.plist is not followed for its mtime.
	link := storagetest.TempDir(t)
	storagetest.Build(t, link,
		storagetest.Node{Path: "target", Content: []byte("x"), ModTime: T},
		storagetest.Node{Path: name + "/Info.plist", Content: []byte("<plist/>"), ModTime: T.Add(-10 * day)},
		storagetest.Node{Path: name + "/com.apple.TimeMachine.Results.plist", Type: storagetest.Symlink, Target: "../target"},
	)
	b = ReadBundle(openRoot(t, link), name)
	if b.LastModifiedAt == nil || !b.LastModifiedAt.Equal(T.Add(-10*day)) {
		t.Errorf("symlinked Results.plist: LastModifiedAt %v, want %v", b.LastModifiedAt, T.Add(-10*day))
	}

	// An empty bundle, and a bundle with no plist and no fallback files:
	// no date and no mtime.
	for _, nodes := range [][]storagetest.Node{
		{{Path: name, Type: storagetest.Dir}},
		{{Path: name + "/bands/0", Content: []byte("x")}, {Path: name + "/MachineID.plist", Content: []byte("x")}, {Path: name + "/lock", Content: []byte{}}},
	} {
		d := storagetest.TempDir(t)
		storagetest.Build(t, d, nodes...)
		b := ReadBundle(openRoot(t, d), name)
		if b.LastBackupAt != nil || b.LastModifiedAt != nil || b.LastBackupReason != ReasonNoHistory {
			t.Errorf("no plist, no fallback: %+v", b)
		}
	}
}

func TestTCSTG104_ALockFileInTheBundleRoot(t *testing.T) {
	T0 := time.Now().Truncate(time.Second)
	dir := storagetest.TempDir(t)
	var nodes []storagetest.Node
	// 1: the at-rest layout with a zero-byte root lock.
	nodes = append(nodes, storagetest.BundleNodes("rest.sparsebundle")...)
	// 2: no lock at all.
	for _, n := range storagetest.BundleNodes("nolock.sparsebundle") {
		if path.Base(n.Path) != "lock" {
			nodes = append(nodes, n)
		}
	}
	// 3: only bands/lock.
	for _, n := range storagetest.BundleNodes("bandslock.sparsebundle") {
		if path.Base(n.Path) != "lock" {
			nodes = append(nodes, n)
		}
	}
	nodes = append(nodes, storagetest.Node{Path: "bandslock.sparsebundle/bands/lock", Content: []byte{}})
	// 4: a root lock with a fresh mtime.
	nodes = append(nodes, storagetest.BundleNodes("fresh.sparsebundle")...)
	nodes = append(nodes, storagetest.Node{Path: "fresh.sparsebundle/lock", Content: []byte{}, ModTime: T0})
	storagetest.Build(t, dir, nodes...)

	rec := newRecOpener(openRoot(t, dir))
	bundles, _, err := Bundles(rec, spec.MaxBundles)
	if err != nil || len(bundles) != 4 {
		t.Fatalf("Bundles: %q %v", bundles, err)
	}
	for _, name := range bundles {
		b := ReadBundle(rec, name)
		in, reason := b.InProgress()
		if in != nil {
			t.Errorf("%s: InProgress %v, want nil", name, *in)
		}
		if reason != "not determinable from the share" {
			t.Errorf("%s: reason %q", name, reason)
		}
	}
	for _, name := range rec.opened() {
		if path.Base(name) == "lock" {
			t.Errorf("lock opened: %s", name)
		}
	}
}

func TestTCSTG105_MachineIDPlistAndBandFileContentsAre(t *testing.T) {
	const name = "Mac.sparsebundle"
	dir := storagetest.TempDir(t)
	nodes := storagetest.BundleNodes(name)
	nodes = append(nodes, storagetest.BandNodes(name, 16, []byte("NX-BAND-DATA"))...)
	nodes = append(nodes,
		storagetest.Node{Path: name + "/MachineID.plist", Content: []byte("<plist><string>NX-MACHINE-UUID</string></plist>")},
		storagetest.Node{Path: name + "/MachineID.bckup", Content: []byte("NX-MACHINE-UUID")},
		storagetest.Node{Path: name + "/" + SnapshotHistoryFile, Content: storagetest.SnapshotHistoryXML(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))},
		storagetest.Node{Path: name + "/com.apple.TimeMachine.Results.plist", Content: []byte("NX-MACHINE-UUID")},
		storagetest.Node{Path: name + "/bands/sub/NX", Content: []byte("NX-BAND-DATA")},
	)
	storagetest.Build(t, dir, nodes...)

	rec := newRecOpener(openRoot(t, dir))
	bundles, truncated, err := Bundles(rec, spec.MaxBundles)
	if err != nil {
		t.Fatal(err)
	}
	var results []any
	results = append(results, bundles, truncated)
	for _, bd := range bundles {
		results = append(results, ReadBundle(rec, bd))
		size, err := WalkBands(t.Context(), rec, bd, spec.MaxSizeWalkEntries, time.Now().Add(spec.SizeWalkTimeout))
		if err != nil {
			t.Fatalf("WalkBands: %v", err)
		}
		if size.Entries != 18 {
			t.Errorf("Entries %d, want 18 (16 bands, sub, sub/NX)", size.Entries)
		}
		results = append(results, size)
	}

	nonDir := rec.nonDirOpens(t)
	if want := []string{name + "/" + SnapshotHistoryFile}; !slices.Equal(nonDir, want) {
		t.Errorf("non-directory opens %q, want exactly %q", nonDir, want)
	}
	dirOpens := 0
	for _, o := range rec.opened() {
		if !slices.Contains(nonDir, o) {
			dirOpens++
		}
	}
	if dirOpens == 0 {
		t.Error("no directory opens recorded: the walk did not go through the Opener")
	}
	dump := fmt.Sprintf("%+v", results)
	for _, s := range []string{"NX-MACHINE-UUID", "NX-BAND-DATA"} {
		if strings.Contains(dump, s) {
			t.Errorf("a returned value contains %s: %s", s, dump)
		}
	}
}

func TestTCSTG106_WalkBandsSumsAllocatedBlocksAndAgreesWith(t *testing.T) {
	const name = "Mac.sparsebundle"
	dir := storagetest.TempDir(t)
	nodes := storagetest.BundleNodes(name)
	for i := range 38 {
		nodes = append(nodes, storagetest.Node{Path: fmt.Sprintf("%s/bands/%x", name, i), Content: bytes.Repeat([]byte{byte(i + 1)}, 1000+i*37_000)})
	}
	storagetest.Build(t, dir, nodes...)
	for _, s := range []string{"26", "27"} {
		p := filepath.Join(dir, name, "bands", s)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(bytes.Repeat([]byte{0xab}, 4096)); err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(8 << 20); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	bands := filepath.Join(dir, name, "bands")
	start := time.Now()
	size, err := WalkBands(t.Context(), openRoot(t, dir), name, spec.MaxSizeWalkEntries, time.Now().Add(spec.SizeWalkTimeout))
	if err != nil {
		t.Fatalf("WalkBands: %v", err)
	}
	want := allocatedTree(t, bands)
	if size.Bytes != want {
		t.Errorf("Bytes %d, want %d (st_blocks × 512 over bands/)", size.Bytes, want)
	}
	if size.Entries != 40 || size.Truncated {
		t.Errorf("Entries %d, Truncated %v; want 40, false", size.Entries, size.Truncated)
	}
	if size.ComputedAt.Before(start) || size.ComputedAt.After(time.Now()) {
		t.Errorf("ComputedAt %v outside the walk", size.ComputedAt)
	}
	if sparse := allocated(t, filepath.Join(bands, "26")); sparse >= 8<<20 {
		t.Logf("the filesystem did not keep 26 sparse (%d bytes allocated)", sparse)
	}

	t.Run("du oracle", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skipf("GNU du -s --block-size=1 is checked on Linux only (GOOS %s)", runtime.GOOS)
		}
		ver, err := exec.Command("du", "--version").Output()
		if err != nil || !bytes.Contains(ver, []byte("GNU")) {
			t.Skip("GNU du not installed")
		}
		out, err := exec.Command("du", "-s", "--block-size=1", bands).Output()
		if err != nil {
			t.Fatalf("du: %v", err)
		}
		du, err := strconv.ParseUint(strings.Fields(string(out))[0], 10, 64)
		if err != nil {
			t.Fatalf("du output %q: %v", out, err)
		}
		if diff := float64(size.Bytes) - float64(du); diff > 0.01*float64(du) || -diff > 0.01*float64(du) {
			t.Errorf("Bytes %d differs from du %d by more than 1%%", size.Bytes, du)
		}
	})
}

func TestTCSTG107_WalkBandsStopsAtItsEntryBoundIts(t *testing.T) {
	const name = "Mac.sparsebundle"
	dir := storagetest.TempDir(t)
	nodes := storagetest.BundleNodes(name)
	nodes = append(nodes, storagetest.BandNodes(name, 150, bytes.Repeat([]byte{1}, 5000))...)
	storagetest.Build(t, dir, nodes...)
	root := openRoot(t, dir)
	bands := filepath.Join(dir, name, "bands")
	full := allocatedTree(t, bands)

	// (1) the entry bound.
	size, err := WalkBands(t.Context(), root, name, 100, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("WalkBands: %v", err)
	}
	if size.Entries != 100 || !size.Truncated {
		t.Errorf("maxEntries 100: Entries %d, Truncated %v", size.Entries, size.Truncated)
	}
	if size.Bytes == 0 || size.Bytes >= full {
		t.Errorf("maxEntries 100: Bytes %d, want a partial sum below %d", size.Bytes, full)
	}
	// Exactly at the bound: not truncated.
	size, err = WalkBands(t.Context(), root, name, 150, time.Now().Add(time.Minute))
	if err != nil || size.Entries != 150 || size.Truncated || size.Bytes != full {
		t.Errorf("maxEntries 150: %+v %v, want 150 entries, %d bytes, not truncated", size, err, full)
	}

	// (2) a deadline already past.
	size, err = WalkBands(t.Context(), root, name, spec.MaxSizeWalkEntries, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("WalkBands: %v", err)
	}
	if !size.Truncated || size.Entries > 1 {
		t.Errorf("past deadline: %+v, want truncated with few or no entries", size)
	}

	// (3) a ctx cancelled at the first Lstat.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rec := newRecOpener(root)
	var once sync.Once
	rec.onLstat = func(string) { once.Do(cancel) }
	start := time.Now()
	size, err = WalkBands(ctx, rec, name, spec.MaxSizeWalkEntries, time.Now().Add(time.Minute))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err %v, want context.Canceled", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("cancelled walk took %v", el)
	}
	if size.Entries > 1 {
		t.Errorf("cancelled walk counted %d entries", size.Entries)
	}

	// A ctx already done: nothing walked.
	done, cancelDone := context.WithCancel(t.Context())
	cancelDone()
	if _, err := WalkBands(done, root, name, spec.MaxSizeWalkEntries, time.Now().Add(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Errorf("done ctx: err %v", err)
	}
}

func TestTCSTG108_WalkBandsDoesNotFollowSymlinksOutOf(t *testing.T) {
	const name = "Mac.sparsebundle"
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir, append(storagetest.BundleNodes(name),
		storagetest.Node{Path: name + "/bands/link", Type: storagetest.Symlink, Target: "/usr"},
		storagetest.Node{Path: name + "/bands/f", Content: bytes.Repeat([]byte{7}, 4096)},
	)...)
	bands := filepath.Join(dir, name, "bands")
	size, err := WalkBands(t.Context(), openRoot(t, dir), name, spec.MaxSizeWalkEntries, time.Now().Add(spec.SizeWalkTimeout))
	if err != nil {
		t.Fatalf("WalkBands: %v", err)
	}
	if size.Entries != 2 {
		t.Errorf("Entries %d, want 2 (link and f)", size.Entries)
	}
	want := allocated(t, bands) + allocated(t, filepath.Join(bands, "link")) + allocated(t, filepath.Join(bands, "f"))
	if size.Bytes != want {
		t.Errorf("Bytes %d, want %d (the link itself, not /usr)", size.Bytes, want)
	}

	// bands itself a symlink (inside or outside the root) is not walked.
	for _, target := range []string{"/usr", "other"} {
		d := storagetest.TempDir(t)
		storagetest.Build(t, d,
			storagetest.Node{Path: name + "/other/f", Content: []byte("x")},
			storagetest.Node{Path: name + "/bands", Type: storagetest.Symlink, Target: target},
		)
		if size, err := WalkBands(t.Context(), openRoot(t, d), name, spec.MaxSizeWalkEntries, time.Now().Add(time.Minute)); err == nil || size.Entries != 0 {
			t.Errorf("bands -> %s: %+v %v, want an error and nothing walked", target, size, err)
		}
	}
	// A bundle name that is not one directory name is refused.
	for _, bad := range []string{"", ".", "..", "a/b", "../x"} {
		if _, err := WalkBands(t.Context(), openRoot(t, dir), bad, 10, time.Now().Add(time.Minute)); err == nil {
			t.Errorf("bundle %q: no error", bad)
		}
	}
}

// provenanceFile returns the capture file whose argv in PROVENANCE's
// commands is `<prog> … <arg>` with prog one of progs, or "".
func provenanceFile(t *testing.T, capDir, arg string, progs ...string) string {
	t.Helper()
	prov, err := os.ReadFile(filepath.Join(capDir, "PROVENANCE"))
	if err != nil {
		t.Fatalf("read PROVENANCE: %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(prov))
	for sc.Scan() {
		file, argv, ok := strings.Cut(strings.TrimSpace(sc.Text()), ": ")
		if !ok {
			continue
		}
		argv = strings.TrimSuffix(argv, " [noeol]")
		for _, p := range progs {
			if strings.HasPrefix(argv, p+" ") && strings.HasSuffix(argv, " "+arg) {
				return file
			}
		}
	}
	return ""
}

func TestTCSTG109_TheCapturedHomeServerShareListingsGive(t *testing.T) {
	capDir := filepath.Join("..", "testdata", "captures", "home-server")
	capturegate.Require(t, filepath.Join(capDir, "PROVENANCE"))
	checkCapturedShares(t, capDir, "/media/timemachine", "/media/timemachine2")
}

// TestTCSTG109Harness runs TC-STG-109's check on a synthetic capture in
// capture.sh's layout, so the check itself is exercised before WI 1.2's
// captures exist: an XML history with two dates, a binary header, an XML
// header whose file was not captured, and a bundle with no history plist.
func TestTCSTG109Harness(t *testing.T) {
	capDir := storagetest.TempDir(t)
	ls := func(lines ...string) []byte {
		return []byte("total 1\n" + strings.Join(lines, "\n") + "\nExit 0\n")
	}
	d := func(name string) string {
		return "drwxr-xr-x 3 0 0 4096 2026-11-18 10:00:00.000000000 +0000 " + name
	}
	f := func(name string, size int) string {
		return fmt.Sprintf("-rw-r--r-- 1 0 0 %d 2026-11-18 10:00:00.000000000 +0000 %s", size, name)
	}
	bundleRoot := ls(d("."), d(".."), f("Info.bckup", 500), f("Info.plist", 500), f("lock", 0), f("token", 0),
		f("MachineID.plist", 300), f(SnapshotHistoryFile, 900), d("bands"))
	files := map[string][]byte{
		"PROVENANCE": []byte("host: synthetic\ncommands:\n  shares/3/ls.txt: ls -lan --time-style=full-iso /media/timemachine\n" +
			"  shares/4/ls.txt: ls -lan --time-style=full-iso /media/timemachine2\n"),
		"shares/3/ls.txt":                                   ls(d("."), d(".."), d("mac-1.sparsebundle"), d("mac-2.sparsebundle"), f(".DS_Store", 6148), d(".TemporaryItems")),
		"shares/3/mac-1.sparsebundle/ls.txt":                bundleRoot,
		"shares/3/mac-1.sparsebundle/SnapshotHistory.head8": []byte("<?xml ve\nExit 0\n"),
		"shares/3/mac-1.sparsebundle/SnapshotHistory.plist": append(storagetest.SnapshotHistoryXML(
			time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC), time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)), "Exit 0\n"...),
		"shares/3/mac-2.sparsebundle/ls.txt":                bundleRoot,
		"shares/3/mac-2.sparsebundle/SnapshotHistory.head8": []byte("bplist00\nExit 0\n"),
		"shares/4/ls.txt":                                   ls(d("."), d(".."), d("mac-3.backupbundle"), d("mac-4.sparsebundle")),
		"shares/4/mac-3.backupbundle/ls.txt":                bundleRoot,
		"shares/4/mac-3.backupbundle/SnapshotHistory.head8": []byte("<?xml ve\nExit 0\n"),
		"shares/4/mac-4.sparsebundle/ls.txt":                ls(d("."), d(".."), f("Info.plist", 500), d("bands")),
	}
	for name, b := range files {
		storagetest.Build(t, capDir, storagetest.Node{Path: name, Content: b})
	}
	checkCapturedShares(t, capDir, "/media/timemachine", "/media/timemachine2")
}

// checkCapturedShares is TC-STG-109 over the capture directory capDir.
func checkCapturedShares(t *testing.T, capDir string, shares ...string) {
	t.Helper()
	dateRE := regexp.MustCompile(`<key>com\.apple\.backupd\.SnapshotCompletionDate</key>\s*<date>([^<]*)</date>`)
	readme, _ := os.ReadFile(filepath.Join(capDir, "README.md"))

	checked := 0
	for _, share := range shares {
		t.Run(share, func(t *testing.T) {
			lsFile := provenanceFile(t, capDir, share, "ls")
			if lsFile == "" {
				t.Fatalf("PROVENANCE names no listing of %s", share)
			}
			out, exit, err := storagetest.CaptureStdout(filepath.Join(capDir, lsFile))
			if err != nil || exit != 0 {
				t.Fatalf("listing %s: exit %d, %v", lsFile, exit, err)
			}
			entries, err := storagetest.ParseListing(out)
			if err != nil {
				t.Fatal(err)
			}
			shareDir := filepath.Dir(lsFile)
			tree := storagetest.TempDir(t)
			topNodes, err := storagetest.ListingNodes(entries, "")
			if err != nil {
				t.Fatal(err)
			}
			storagetest.Build(t, tree, topNodes...)

			var wantDirs []string
			otherDirs := 0
			for _, e := range entries {
				isBundle := strings.HasSuffix(e.Name, ".sparsebundle") || strings.HasSuffix(e.Name, ".backupbundle")
				switch {
				case e.Type == 'd' && isBundle:
					wantDirs = append(wantDirs, e.Name)
				case e.Type == 'd' && !strings.HasPrefix(e.Name, "."):
					otherDirs++
				}
			}
			if len(wantDirs) == 0 && otherDirs > 0 {
				t.Fatalf("A-14 falsified: %s holds %d directories and no bundle directly under it", share, otherDirs)
			}
			slices.Sort(wantDirs)

			wantDate := map[string]*time.Time{}
			for _, bd := range wantDirs {
				bundleLs := filepath.Join(shareDir, bd, "ls.txt")
				bout, bexit, err := storagetest.CaptureStdout(filepath.Join(capDir, bundleLs))
				if err != nil || bexit != 0 {
					t.Fatalf("bundle listing %s: exit %d, %v", bundleLs, bexit, err)
				}
				bents, err := storagetest.ParseListing(bout)
				if err != nil {
					t.Fatal(err)
				}
				hasLock := false
				for _, e := range bents {
					if e.Name == "lock" {
						hasLock = true
					}
					if e.Type == 'd' && (strings.HasSuffix(e.Name, ".sparsebundle") || strings.HasSuffix(e.Name, ".backupbundle")) {
						t.Fatalf("A-14 falsified: %s/%s holds a nested bundle %s", share, bd, e.Name)
					}
				}
				bnodes, err := storagetest.ListingNodes(bents, bd)
				if err != nil {
					t.Fatal(err)
				}
				// The captured plist bytes replace the sparse placeholder.
				hist := bd + "/" + SnapshotHistoryFile
				var content []byte
				if b, _, err := storagetest.CaptureStdout(filepath.Join(capDir, shareDir, bd, "SnapshotHistory.plist")); err == nil {
					content = b
				} else if b, _, err := storagetest.CaptureStdout(filepath.Join(capDir, shareDir, bd, "SnapshotHistory.head8")); err == nil {
					content = b[:min(len(b), 8)]
				}
				for i := range bnodes {
					if bnodes[i].Path == hist && content != nil {
						bnodes[i].Content = content
					}
				}
				storagetest.Build(t, tree, bnodes...)

				var latest *time.Time
				if !bytes.HasPrefix(content, []byte("bplist00")) {
					for _, m := range dateRE.FindAllSubmatch(content, -1) {
						d, err := time.Parse(time.RFC3339, string(m[1]))
						if err != nil {
							t.Fatalf("%s: captured date %q: %v", bd, m[1], err)
						}
						if latest == nil || d.After(*latest) {
							latest = &d
						}
					}
				}
				wantDate[bd] = latest
				t.Logf("A-16 falsifier: %s/%s root holds lock: %v; header %q", share, bd, hasLock, content[:min(len(content), 8)])
			}
			for line := range strings.Lines(string(readme)) {
				if l := strings.ToLower(line); strings.Contains(l, "running") || strings.Contains(l, "in progress") {
					t.Logf("A-16 falsifier, capture README: %s", strings.TrimSpace(line))
				}
			}

			root := openRoot(t, tree)
			got, _, err := Bundles(root, spec.MaxBundles)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, wantDirs) {
				t.Fatalf("bundles %q, want %q", got, wantDirs)
			}
			for _, bd := range got {
				b := ReadBundle(root, bd)
				if b.Name != strings.TrimSuffix(strings.TrimSuffix(bd, ".sparsebundle"), ".backupbundle") {
					t.Errorf("%s: Name %q", bd, b.Name)
				}
				want := wantDate[bd]
				switch {
				case want == nil && b.LastBackupAt != nil:
					t.Errorf("%s: LastBackupAt %v, want nil", bd, b.LastBackupAt)
				case want != nil && (b.LastBackupAt == nil || !b.LastBackupAt.Equal(*want)):
					t.Errorf("%s: LastBackupAt %v (%q), want %v", bd, b.LastBackupAt, b.LastBackupReason, want)
				}
				if in, reason := b.InProgress(); in != nil || reason != ReasonInProgress {
					t.Errorf("%s: InProgress %v %q", bd, in, reason)
				}
				if b.LastBackupAt != nil {
					checked++
				}
			}
		})
	}
	t.Logf("%d bundles with a completion date", checked)
}

// TC-STG-107 (code review CR-2): the entry bound acts while the directory is
// read, not after it. A bands/ directory of 4,000 files walked with
// maxEntries 10 must not read and stat all of them first: fs.WalkDir (and
// os.ReadDir) read, lstat and sort every entry before the first callback,
// which allocates a DirEntry and a FileInfo per file. The walk allocates
// within a few batches' worth, far below the directory's size.
func TestTCSTG107_WalkBandsReadsAHugeDirectoryInBatches(t *testing.T) {
	const name = "Mac.sparsebundle"
	const files = 4000
	dir := storagetest.TempDir(t)
	nodes := storagetest.BundleNodes(name)
	nodes = append(nodes, storagetest.BandNodes(name, files, []byte{1})...)
	storagetest.Build(t, dir, nodes...)
	root := openRoot(t, dir)

	size, err := WalkBands(t.Context(), root, name, 10, time.Now().Add(time.Minute))
	if err != nil || size.Entries != 10 || !size.Truncated {
		t.Fatalf("maxEntries 10: %+v, %v; want 10 entries, truncated", size, err)
	}
	allocs := testing.AllocsPerRun(5, func() {
		if _, err := WalkBands(t.Context(), root, name, 10, time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	})
	// Reading all the entries costs at least 2 allocations each (a
	// DirEntry and its FileInfo): over 8,000. One batch of 256 costs about
	// 800.
	if allocs > 2000 {
		t.Errorf("a walk bounded to 10 entries over %d files made %.0f allocations (limit 2000): the directory was read whole", files, allocs)
	}

	// A deadline already past reads no batch at all.
	allocs = testing.AllocsPerRun(5, func() {
		if _, err := WalkBands(t.Context(), root, name, spec.MaxSizeWalkEntries, time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 100 {
		t.Errorf("a walk past its deadline made %.0f allocations: the directory was read before the deadline was checked", allocs)
	}
}
