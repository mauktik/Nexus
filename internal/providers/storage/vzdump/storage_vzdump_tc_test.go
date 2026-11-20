// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-85 to TC-STG-95
// (WI 3.3): the vzdump name grammar, Scan, Notes and ReadLog over real temp
// trees opened through os.Root (F-TREE, storagetest/trees.go).

package vzdump

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

// recOpener wraps an os.Root and records every name opened, through Open or
// through FS, the bytes read from each file opened through FS, and the
// offset of each file's first positioned read or seek.
type recOpener struct {
	*os.Root
	mu      sync.Mutex
	opens   []string
	read    map[string]int64
	firstAt map[string]int64
}

func newRecOpener(r *os.Root) *recOpener {
	return &recOpener{Root: r, read: map[string]int64{}, firstAt: map[string]int64{}}
}

func (o *recOpener) record(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opens = append(o.opens, name)
}

func (o *recOpener) Open(name string) (*os.File, error) {
	o.record(name)
	return o.Root.Open(name)
}

func (o *recOpener) FS() fs.FS { return recFS{o} }

// OpenFile records the open the way Open and FS().Open do; WrapFile then
// counts the reads of the file.
func (o *recOpener) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	o.record(name)
	return o.Root.OpenFile(name, flag, perm)
}

func (o *recOpener) WrapFile(name string, f *os.File) fs.File {
	return &recFile{File: f, o: o, name: name}
}

func (o *recOpener) opened() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.opens)
}

type recFS struct{ o *recOpener }

func (f recFS) Open(name string) (fs.File, error) {
	f.o.record(name)
	file, err := f.o.Root.FS().Open(name)
	if err != nil {
		return nil, err
	}
	return &recFile{File: file, o: f.o, name: name}, nil
}

type recFile struct {
	fs.File
	o    *recOpener
	name string
}

func (f *recFile) count(n int) {
	f.o.mu.Lock()
	defer f.o.mu.Unlock()
	f.o.read[f.name] += int64(n)
}

func (f *recFile) at(off int64) {
	f.o.mu.Lock()
	defer f.o.mu.Unlock()
	if _, ok := f.o.firstAt[f.name]; !ok {
		f.o.firstAt[f.name] = off
	}
}

func (f *recFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.count(n)
	return n, err
}

func (f *recFile) ReadAt(p []byte, off int64) (int, error) {
	f.at(off)
	n, err := f.File.(io.ReaderAt).ReadAt(p, off)
	f.count(n)
	return n, err
}

func (f *recFile) Seek(off int64, whence int) (int64, error) {
	pos, err := f.File.(io.Seeker).Seek(off, whence)
	if err == nil {
		f.at(pos)
	}
	return pos, err
}

// subOpener presents the directory sub of an os.Root as the root's ".",
// whatever sub is: the way a configured path that is not a readable
// directory looks to a reader holding a root on it.
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

func archiveByFile(t *testing.T, res DirResult, file string) Archive {
	t.Helper()
	for _, a := range res.Archives {
		if a.File == file {
			return a
		}
	}
	t.Fatalf("no archive %s in %+v", file, res.Archives)
	return Archive{}
}

func TestTCSTG85_TheVzdumpNameGrammar(t *testing.T) {
	recognized := []struct {
		name, guestType string
		vmid            int
		format, comp    string
		ts              string
	}{
		{"vzdump-qemu-101-2026_11_15-01_00_03.vma.zst", "qemu", 101, "vma", "zst", "2026-11-15T01:00:03"},
		{"vzdump-lxc-103-2026_11_15-01_10_00.tar.gz", "lxc", 103, "tar", "gz", "2026-11-15T01:10:00"},
		{"vzdump-lxc-103-2026_11_15-01_10_00.tar.lzo", "lxc", 103, "tar", "lzo", "2026-11-15T01:10:00"},
		{"vzdump-qemu-101-2026_11_15-01_00_03.vma", "qemu", 101, "vma", "", "2026-11-15T01:00:03"},
		{"vzdump-lxc-999999999-2026_11_15-01_10_00.tar.zst", "lxc", 999999999, "tar", "zst", "2026-11-15T01:10:00"},
		{"vzdump-qemu-100-2024_02_29-23_59_59.vma.gz", "qemu", 100, "vma", "gz", "2024-02-29T23:59:59"},
	}
	for _, c := range recognized {
		n := parseName(c.name)
		if n.kind != kindArchive {
			t.Errorf("%s: kind %d, want archive", c.name, n.kind)
			continue
		}
		got := fmt.Sprintf("%s %d %s %q %s", n.guestType, n.vmid, n.format, n.compression,
			n.timestamp.Format("2006-01-02T15:04:05"))
		want := fmt.Sprintf("%s %d %s %q %s", c.guestType, c.vmid, c.format, c.comp, c.ts)
		if got != want {
			t.Errorf("%s: got %s, want %s", c.name, got, want)
		}
	}
	unrecognized := []string{
		"vzdump-qemu-99-2026_11_15-01_00_03.vma.zst",
		"vzdump-qemu-1000000000-2026_11_15-01_00_03.vma.zst",
		"vzdump-qemu-0101-2026_11_15-01_00_03.vma.zst",
		"vzdump-openvz-101-2026_11_15-01_00_03.tar.gz",
		"vzdump-QEMU-101-2026_11_15-01_00_03.vma.zst",
		"vzdump-qemu-101-2026_13_40-25_61_61.vma",
		"vzdump-qemu-101-2026_02_30-01_00_03.vma",
		"vzdump-qemu-101-2026_11_15-01_00_03.vma.xz",
		"vzdump-qemu-101-2026_11_15-01_00_03.vma.zst.bak",
		"random.txt",
		"",
	}
	for _, name := range unrecognized {
		if n := parseName(name); n.kind != kindOther {
			t.Errorf("%q: kind %d, want unrecognized", name, n.kind)
		}
	}
	// Companions and in-progress names share the archive's stem (A-10, A-12).
	kinds := map[string]nameKind{
		"vzdump-qemu-101-2026_11_15-01_00_03.log":               kindLog,
		"vzdump-qemu-101-2026_11_15-01_00_03.notes":             kindNotes,
		"vzdump-qemu-101-2026_11_15-01_00_03.vma.zst.notes":     kindNotes,
		"vzdump-qemu-101-2026_11_15-01_00_03.vma.zst.protected": kindProtected,
		"vzdump-qemu-101-2026_11_15-01_00_03.vma.dat":           kindPartial,
		"vzdump-qemu-101-2026_11_15-01_00_03.dat":               kindPartial,
		"vzdump-lxc-103-2026_11_15-01_10_00.tar.dat":            kindPartial,
		"vzdump-lxc-103-2026_11_15-01_10_00.tmp":                kindTmpDir,
		"vzdump-lxc-103-2026_11_15-01_10_00.tar.gz.log":         kindOther,
		"vzdump-lxc-103-2026_11_15-01_10_00.tar.gz.dat":         kindOther,
		"vzdump-lxc-103-2026_11_15-01_10_00.protected":          kindOther,
	}
	for name, want := range kinds {
		if n := parseName(name); n.kind != want {
			t.Errorf("%s: kind %d, want %d", name, n.kind, want)
		}
	}
}

// homeServerListing returns the stdout of the capture listing dir, found
// by its argv in PROVENANCE's commands, and whether the listing exited 0.
func homeServerListing(t *testing.T, capDir, dir string) ([]byte, bool) {
	t.Helper()
	prov, err := os.ReadFile(filepath.Join(capDir, "PROVENANCE"))
	if err != nil {
		t.Fatalf("read PROVENANCE: %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(prov))
	for sc.Scan() {
		file, argv, ok := strings.Cut(strings.TrimSpace(sc.Text()), ": ")
		if !ok || !strings.HasPrefix(argv, "ls ") {
			continue
		}
		argv = strings.TrimSuffix(argv, " [noeol]")
		if !strings.HasSuffix(argv, " "+dir) {
			continue
		}
		out, exit, err := storagetest.CaptureStdout(filepath.Join(capDir, file))
		if err != nil {
			t.Fatalf("capture %s: %v", file, err)
		}
		return out, exit == 0
	}
	t.Fatalf("PROVENANCE names no listing of %s", dir)
	return nil, false
}

func TestTCSTG86_EveryNameInTheHomeServerListings(t *testing.T) {
	capDir := filepath.Join("..", "testdata", "captures", "home-server")
	capturegate.Require(t, filepath.Join(capDir, "PROVENANCE"))

	top, ok := homeServerListing(t, capDir, "/media/vzdump")
	if !ok {
		t.Fatalf("the home-server listing of /media/vzdump did not exit 0")
	}
	topEntries, err := storagetest.ParseListing(top)
	if err != nil {
		t.Fatal(err)
	}
	listings := map[string][]storagetest.Listed{".": topEntries}
	for _, e := range topEntries {
		if e.Name == "dump" && e.Type == 'l' {
			t.Fatalf("dump is a symlink on home-server (-> %s): revise TC-STG-86's tree", e.Target)
		}
		if e.Name == "dump" && e.Type == 'd' {
			out, ok := homeServerListing(t, capDir, "/media/vzdump/dump")
			if !ok {
				t.Fatalf("dump is a directory but its listing did not exit 0")
			}
			if listings["dump"], err = storagetest.ParseListing(out); err != nil {
				t.Fatal(err)
			}
		}
	}

	dir := storagetest.TempDir(t)
	for _, d := range []string{".", "dump"} {
		prefix := ""
		if d == "dump" {
			prefix = "dump"
		}
		nodes, err := storagetest.ListingNodes(listings[d], prefix)
		if err != nil {
			t.Fatal(err)
		}
		storagetest.Build(t, dir, nodes...)
	}

	// The oracle: FR-9's grammar and DF-3's companion rules, applied to the
	// listing here, independently of names.go.
	archRE := regexp.MustCompile(`^(vzdump-(?:qemu|lxc)-[1-9][0-9]{2,8}-([0-9]{4}_[0-9]{2}_[0-9]{2}-[0-9]{2}_[0-9]{2}_[0-9]{2}))\.(?:vma|tar)(?:\.(?:zst|gz|lzo))?$`)
	stemRE := regexp.MustCompile(`^(vzdump-(?:qemu|lxc)-[1-9][0-9]{2,8}-([0-9]{4}_[0-9]{2}_[0-9]{2}-[0-9]{2}_[0-9]{2}_[0-9]{2}))\.(dat|vma\.dat|tar\.dat|tmp)$`)
	validTS := func(s string) bool {
		_, err := time.Parse("2006_01_02-15_04_05", s)
		return err == nil
	}
	// In progress (A-12): <stem>.dat, <stem>.vma.dat or <stem>.tar.dat
	// files and <stem>.tmp directories.
	isPartial := func(name string, isDir, isFile bool) bool {
		m := stemRE.FindStringSubmatch(name)
		if m == nil || !validTS(m[2]) {
			return false
		}
		if m[3] == "tmp" {
			return isDir
		}
		return isFile
	}
	type want struct {
		size  int64
		mtime time.Time
		dir   bool
	}
	wantArchives := map[string]want{}
	wantUnrecognized := 0
	for d, entries := range listings {
		byName := map[string]storagetest.Listed{}
		for _, e := range entries {
			byName[e.Name] = e
		}
		// A symlink counts as its target when the target is relative,
		// stays inside the tree and is a regular file listed in the same
		// directory; otherwise it is unrecognized.
		resolve := func(e storagetest.Listed) (storagetest.Listed, bool) {
			if e.Type != 'l' {
				return e, true
			}
			if path.IsAbs(e.Target) || strings.Contains(e.Target, "/") {
				return e, false
			}
			tgt, ok := byName[e.Target]
			return tgt, ok && tgt.Type == '-'
		}
		archives, stems := map[string]bool{}, map[string]bool{}
		for _, e := range entries {
			r, ok := resolve(e)
			if m := archRE.FindStringSubmatch(e.Name); ok && r.Type == '-' && m != nil && validTS(m[2]) {
				archives[e.Name], stems[m[1]] = true, true
			}
		}
		for _, e := range entries {
			key := d + "/" + e.Name
			r, ok := resolve(e)
			switch {
			case d == "." && e.Name == "dump" && e.Type == 'd':
			case archives[e.Name]:
				wantArchives[key] = want{size: r.Size, mtime: r.ModTime}
			case ok && r.Type == '-' && isCompanion(e.Name, archives, stems):
			case isPartial(e.Name, e.Type == 'd', ok && r.Type == '-'):
				wantArchives[key] = want{size: r.Size, mtime: r.ModTime, dir: e.Type == 'd'}
			default:
				wantUnrecognized++
			}
		}
	}

	res := Scan(openRoot(t, dir), "/media/vzdump", spec.MaxDirEntries)
	if res.Err != nil {
		t.Fatalf("Scan: %v", res.Err)
	}
	if len(res.Archives) != len(wantArchives) || res.Unrecognized != wantUnrecognized {
		t.Errorf("archives %d, unrecognized %d; oracle %d and %d",
			len(res.Archives), res.Unrecognized, len(wantArchives), wantUnrecognized)
	}
	for _, a := range res.Archives {
		w, ok := wantArchives[a.Dir+"/"+a.File]
		if !ok {
			t.Errorf("archive %s/%s is not in the oracle", a.Dir, a.File)
			continue
		}
		if !w.dir && a.Size != w.size {
			t.Errorf("%s/%s: size %d, listing %d", a.Dir, a.File, a.Size, w.size)
		}
		if !a.ModifiedAt.Equal(w.mtime) {
			t.Errorf("%s/%s: mtime %s, listing %s", a.Dir, a.File, a.ModifiedAt, w.mtime)
		}
	}
	t.Logf("home-server: %d archives, %d unrecognized", len(res.Archives), res.Unrecognized)
}

// isCompanion is the oracle's companion rule (A-10): <stem>.log,
// <stem>.notes, <archive>.notes and <archive>.protected of an archive in the
// same directory.
func isCompanion(name string, archives, stems map[string]bool) bool {
	for _, suf := range []string{".notes", ".protected"} {
		if a, ok := strings.CutSuffix(name, suf); ok && archives[a] {
			return true
		}
	}
	for _, suf := range []string{".log", ".notes"} {
		if s, ok := strings.CutSuffix(name, suf); ok && stems[s] {
			return true
		}
	}
	return false
}

func TestTCSTG87_ScanReadsThePathAndItsDump(t *testing.T) {
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir,
		storagetest.Node{Path: "vzdump-qemu-101-2026_11_15-01_00_03.vma.zst", Size: 10},
		storagetest.Node{Path: "dump/vzdump-qemu-102-2026_11_15-01_00_03.vma.zst", Size: 20},
		storagetest.Node{Path: "dump/vzdump-lxc-103-2026_11_15-01_10_00.tar.zst", Size: 30},
		storagetest.Node{Path: "dump/old/vzdump-qemu-104-2026_11_14-01_00_03.vma.zst", Size: 40},
		storagetest.Node{Path: "other/vzdump-qemu-105-2026_11_14-01_00_03.vma.zst", Size: 50},
	)
	rec := newRecOpener(openRoot(t, dir))
	res := Scan(rec, dir, spec.MaxDirEntries)
	if res.Err != nil {
		t.Fatalf("Scan: %v", res.Err)
	}
	var got []string
	for _, a := range res.Archives {
		got = append(got, a.Dir+"/"+a.File)
	}
	slices.Sort(got)
	want := []string{
		"./vzdump-qemu-101-2026_11_15-01_00_03.vma.zst",
		"dump/vzdump-lxc-103-2026_11_15-01_10_00.tar.zst",
		"dump/vzdump-qemu-102-2026_11_15-01_00_03.vma.zst",
	}
	if !slices.Equal(got, want) {
		t.Errorf("archives %v, want %v", got, want)
	}
	if res.Unrecognized != 2 {
		t.Errorf("unrecognized %d, want 2 (dump/old and other)", res.Unrecognized)
	}
	if res.Path != dir || res.Truncated {
		t.Errorf("path %q truncated %v", res.Path, res.Truncated)
	}
	opens := rec.opened()
	if len(opens) == 0 {
		t.Fatal("Scan opened nothing through the Opener")
	}
	for _, o := range opens {
		if o != "." && o != "dump" {
			t.Errorf("Scan opened %q; only . and dump may be opened", o)
		}
	}
}

func TestTCSTG88_CompanionsAreAssociatedWithTheirArchiveAnd(t *testing.T) {
	const (
		archA = "vzdump-qemu-101-2026_11_15-01_00_03.vma.zst"
		stemA = "vzdump-qemu-101-2026_11_15-01_00_03"
		archB = "vzdump-lxc-103-2026_11_15-01_10_00.tar.gz"
		stemB = "vzdump-lxc-103-2026_11_15-01_10_00"
	)
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir,
		storagetest.Node{Path: "dump/" + archA, Size: 100},
		storagetest.Node{Path: "dump/" + stemA + ".log", Content: []byte("log\n")},
		storagetest.Node{Path: "dump/" + archA + ".notes", Content: []byte("web\n")},
		storagetest.Node{Path: "dump/" + archA + ".protected", Content: []byte{}},
		storagetest.Node{Path: "dump/" + archB, Size: 200},
		storagetest.Node{Path: "dump/" + stemB + ".notes", Content: []byte("db\n")},
		storagetest.Node{Path: "dump/vzdump-qemu-105-2026_11_14-01_00_00.log", Content: []byte("orphan\n")},
	)
	res := Scan(openRoot(t, dir), dir, spec.MaxDirEntries)
	if res.Err != nil {
		t.Fatalf("Scan: %v", res.Err)
	}
	if len(res.Archives) != 2 {
		t.Fatalf("archives %+v, want A and B only (no companion is an archive)", res.Archives)
	}
	a := archiveByFile(t, res, archA)
	if a.LogFile != stemA+".log" || a.NotesFile != archA+".notes" || !a.Protected || a.InProgress {
		t.Errorf("A: %+v", a)
	}
	b := archiveByFile(t, res, archB)
	if b.LogFile != "" || b.NotesFile != stemB+".notes" || b.Protected || b.InProgress {
		t.Errorf("B: %+v", b)
	}
	if a.Dir != "dump" || a.Size != 100 || b.Size != 200 {
		t.Errorf("A dir %q size %d, B size %d", a.Dir, a.Size, b.Size)
	}
	if res.Unrecognized != 1 {
		t.Errorf("unrecognized %d, want 1 (the orphan log)", res.Unrecognized)
	}
}

func TestTCSTG89_AnArchiveBeingWrittenIsAnIn(t *testing.T) {
	dir := storagetest.TempDir(t)
	mt := time.Date(2026, 11, 19, 1, 5, 0, 0, time.UTC)
	storagetest.Build(t, dir,
		storagetest.Node{Path: "dump/vzdump-qemu-101-2026_11_19-01_00_00.vma.dat", Size: 4096, ModTime: mt},
		storagetest.Node{Path: "dump/vzdump-lxc-103-2026_11_19-01_10_00.tmp/etc/hostname", Content: []byte("ct\n")},
		storagetest.Node{Path: "dump/vzdump-lxc-103-2026_11_19-01_10_00.tar.dat", Size: 8192},
	)
	rec := newRecOpener(openRoot(t, dir))
	res := Scan(rec, dir, spec.MaxDirEntries)
	if res.Err != nil {
		t.Fatalf("Scan: %v", res.Err)
	}
	if len(res.Archives) != 3 || res.Unrecognized != 0 {
		t.Fatalf("archives %+v, unrecognized %d; want 3 in-progress items", res.Archives, res.Unrecognized)
	}
	for _, a := range res.Archives {
		if !a.InProgress || a.Dir != "dump" {
			t.Errorf("%s: InProgress %v, dir %q", a.File, a.InProgress, a.Dir)
		}
		want := map[string]string{
			"vzdump-qemu-101-2026_11_19-01_00_00.vma.dat": "qemu 101",
			"vzdump-lxc-103-2026_11_19-01_10_00.tmp":      "lxc 103",
			"vzdump-lxc-103-2026_11_19-01_10_00.tar.dat":  "lxc 103",
		}[a.File]
		if got := fmt.Sprintf("%s %d", a.GuestType, a.VMID); got != want {
			t.Errorf("%s: %s, want %s", a.File, got, want)
		}
	}
	if a := archiveByFile(t, res, "vzdump-qemu-101-2026_11_19-01_00_00.vma.dat"); a.Size != 4096 || !a.ModifiedAt.Equal(mt) {
		t.Errorf("dat item size %d mtime %s", a.Size, a.ModifiedAt)
	}
	for _, o := range rec.opened() {
		if strings.Contains(o, ".tmp") {
			t.Errorf("Scan opened the in-progress directory: %q", o)
		}
	}
}

func TestTCSTG90_ASymlinkThatPointsOutsideTheConfigured(t *testing.T) {
	const (
		marker  = "NX-SECRET-MARKER"
		arch101 = "vzdump-qemu-101-2026_11_15-01_00_03.vma.zst"
		stem101 = "vzdump-qemu-101-2026_11_15-01_00_03"
		arch102 = "vzdump-qemu-102-2026_11_15-01_00_03.vma.zst"
		arch103 = "vzdump-qemu-103-2026_11_15-01_00_03.vma.zst"
	)
	base := storagetest.TempDir(t)
	x := filepath.Join(base, "X")
	storagetest.Build(t, base,
		storagetest.Node{Path: "X/secret.txt", Content: []byte(marker + "\nERROR: x\nINFO: Finished Backup of VM 101 (00:00:01)\n")},
		storagetest.Node{Path: "R/dump", Type: storagetest.Symlink, Target: x},
		storagetest.Node{Path: "R/" + arch101, Content: []byte("archive")},
		storagetest.Node{Path: "R/" + arch101 + ".notes", Type: storagetest.Symlink, Target: filepath.Join(x, "secret.txt")},
		storagetest.Node{Path: "R/" + stem101 + ".log", Type: storagetest.Symlink, Target: "../X/secret.txt"},
		storagetest.Node{Path: "R/" + arch102, Type: storagetest.Symlink, Target: filepath.Join(x, "secret.txt")},
		// A symlink that resolves inside the root to a regular file is read.
		storagetest.Node{Path: "R/" + arch103, Type: storagetest.Symlink, Target: arch101},
	)
	r := filepath.Join(base, "R")
	rec := newRecOpener(openRoot(t, r))
	res := Scan(rec, r, spec.MaxDirEntries)
	if res.Err != nil {
		t.Fatalf("Scan: %v", res.Err)
	}
	var files []string
	for _, a := range res.Archives {
		files = append(files, a.File)
		if a.Dir != "." {
			t.Errorf("%s listed in %q: dump -> X must not be listed", a.File, a.Dir)
		}
	}
	slices.Sort(files)
	if !slices.Equal(files, []string{arch101, arch103}) {
		t.Errorf("archives %v, want 101 and the inside symlink 103", files)
	}
	if a := archiveByFile(t, res, arch101); a.NotesFile != "" || a.LogFile != "" {
		t.Errorf("101 companions that escape the root were associated: %+v", a)
	}
	if a := archiveByFile(t, res, arch103); a.Size != int64(len("archive")) {
		t.Errorf("103 size %d, want its inside target's", a.Size)
	}
	if res.Unrecognized != 4 {
		t.Errorf("unrecognized %d, want 4 (dump, 102, 101's .notes and .log)", res.Unrecognized)
	}
	for _, o := range rec.opened() {
		if o != "." {
			t.Errorf("Scan opened %q through the escaping symlinks", o)
		}
	}

	notes, _, nerr := Notes(rec, arch101+".notes", spec.NotesMaxBytes)
	if nerr == nil {
		t.Errorf("Notes through an escaping symlink: no error, content %q", notes)
	}
	v, lerr := ReadLog(rec, stem101+".log", spec.LogTailBytes, spec.MaxLogErrorLines, spec.LogLineMaxBytes)
	if lerr == nil {
		t.Errorf("ReadLog through an escaping symlink: no error, verdict %+v", v)
	}
	all := []string{notes, v.Status, fmt.Sprint(res)}
	all = append(all, v.Errors...)
	if nerr != nil {
		all = append(all, nerr.Error())
	}
	if lerr != nil {
		all = append(all, lerr.Error())
	}
	for _, s := range all {
		if strings.Contains(s, marker) {
			t.Errorf("a returned string holds the outside file's content: %q", s)
		}
	}
}

func TestTCSTG91_AConfiguredPathThatIsAFile(t *testing.T) {
	base := storagetest.TempDir(t)
	storagetest.Build(t, base,
		storagetest.Node{Path: "file", Content: []byte("not a directory")},
		storagetest.Node{Path: "locked/" + "vzdump-qemu-101-2026_11_15-01_00_03.vma.zst", Size: 1},
	)
	if os.Geteuid() != 0 {
		storagetest.Build(t, base, storagetest.Node{Path: "locked", Type: storagetest.Dir, SetMode: true})
	}
	root := openRoot(t, base)
	cases := []struct{ sub, reason string }{
		{"file", "not a directory"},
		{"missing", "no such file or directory"},
		{"locked", "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.sub, func(t *testing.T) {
			if c.sub == "locked" && os.Geteuid() == 0 {
				t.Skip("mode 000 does not stop uid 0")
			}
			p := filepath.Join(base, c.sub)
			res := Scan(subOpener{root, c.sub}, p, spec.MaxDirEntries)
			if res.Err == nil {
				t.Fatalf("no error for %s: %+v", p, res)
			}
			if msg := res.Err.Error(); !strings.Contains(msg, p) || !strings.Contains(msg, c.reason) {
				t.Errorf("error %q does not name %s with %q", msg, p, c.reason)
			}
			if len(res.Archives) != 0 || res.Path != p {
				t.Errorf("archives %+v, path %q", res.Archives, res.Path)
			}
		})
	}
}

func TestTCSTG92_ADirectoryOf10001EntriesStops(t *testing.T) {
	dir := storagetest.TempDir(t)
	nodes := make([]storagetest.Node, 0, spec.MaxDirEntries+1)
	for i := range spec.MaxDirEntries + 1 {
		nodes = append(nodes, storagetest.Node{
			Path: fmt.Sprintf("dump/vzdump-qemu-%d-2026_11_15-01_00_00.vma", 100+i), Content: []byte{}})
	}
	storagetest.Build(t, dir, nodes...)
	root := openRoot(t, dir)

	res := Scan(root, dir, spec.MaxDirEntries)
	if res.Err != nil {
		t.Fatalf("Scan: %v", res.Err)
	}
	if !res.Truncated {
		t.Error("10,001 entries: Truncated false")
	}
	if n := len(res.Archives) + res.Unrecognized; n > spec.MaxDirEntries || n == 0 {
		t.Errorf("archives+unrecognized %d, want 1..%d", n, spec.MaxDirEntries)
	}
	// One entry more of headroom reads them all and is not truncated.
	res = Scan(root, dir, spec.MaxDirEntries+1)
	if res.Truncated || len(res.Archives) != spec.MaxDirEntries+1 {
		t.Errorf("bound 10,001: truncated %v, %d archives", res.Truncated, len(res.Archives))
	}
}

func TestTCSTG93_NotesAreTheFirst1KiBValid(t *testing.T) {
	long := strings.Repeat("a", 1023) + "€" + strings.Repeat("b", 5*1024-1026)
	files := map[string][]byte{
		"short.notes":   []byte("web-server"),
		"exact.notes":   bytes.Repeat([]byte("n"), 1024),
		"long.notes":    []byte(long),
		"invalid.notes": []byte("ab\xff\xfecd"),
		"nul.notes":     []byte("a\x00b"),
	}
	dir := storagetest.TempDir(t)
	for name, b := range files {
		storagetest.Build(t, dir, storagetest.Node{Path: name, Content: b})
	}
	rec := newRecOpener(openRoot(t, dir))
	check := func(name, want string, wantTrunc bool) {
		t.Helper()
		got, trunc, err := Notes(rec, name, spec.NotesMaxBytes)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want || trunc != wantTrunc {
			t.Errorf("%s: %q truncated %v; want %q truncated %v", name, got, trunc, want, wantTrunc)
		}
		if !utf8.ValidString(got) || len(got) > spec.NotesMaxBytes {
			t.Errorf("%s: %d bytes, valid UTF-8 %v", name, len(got), utf8.ValidString(got))
		}
	}
	check("short.notes", "web-server", false)
	check("exact.notes", strings.Repeat("n", 1024), false)
	// The rune at bytes 1023..1025 straddles the bound and is dropped whole.
	check("long.notes", strings.Repeat("a", 1023), true)
	check("invalid.notes", "ab�cd", false)
	check("nul.notes", "a\x00b", false)
	for name := range files {
		if n := rec.read[name]; n > spec.NotesMaxBytes+3 {
			t.Errorf("%s: read %d bytes, more than 1 KiB + 3", name, n)
		}
	}
	if rec.read["long.notes"] == 0 {
		t.Error("the byte counter saw no read: Notes must read through FS")
	}
}

func TestTCSTG94_ReadLogReadsOnlyTheLast256KiB(t *testing.T) {
	const ts = "2026-11-15 01:00:03 "
	lines := func(ls ...string) []byte {
		var b bytes.Buffer
		for _, l := range ls {
			b.WriteString(ts + l + "\n")
		}
		return b.Bytes()
	}
	finish := "INFO: Finished Backup of VM 101 (00:01:02)"
	var filler bytes.Buffer
	filler.Write(lines("INFO: Starting Backup of VM 101 (qemu)"))
	for filler.Len() < 50<<10 {
		filler.Write(lines("INFO: transferred 1.0 GiB of 32.0 GiB (3.12%)"))
	}
	filler.Write(lines("ERROR: early failure outside the tail"))
	for filler.Len() < 1<<20 {
		filler.Write(lines("INFO: transferred 1.0 GiB of 32.0 GiB (3.12%)"))
	}
	filler.Write(lines(finish))
	var errs []string
	for i := range 15 {
		l := fmt.Sprintf("ERROR: failure %d", i)
		if i == 7 {
			l += " " + strings.Repeat("é", 1024) + "\xff"
		}
		errs = append(errs, l)
	}
	logs := map[string][]byte{
		"a.log": lines("INFO: Starting Backup of VM 101 (qemu)", "INFO: archive file size: 1.2GB", finish),
		"b.log": lines("INFO: Starting Backup of VM 101 (qemu)", finish, "ERROR: Backup of VM 101 failed - x"),
		"c.log": lines("INFO: Starting Backup of VM 101 (qemu)", "ERROR: vma_queue_write: write error", "ERROR: Backup of VM 101 failed - vma_queue_write: write error"),
		"d.log": lines("INFO: Starting Backup of VM 101 (qemu)", "INFO: status = running"),
		"e.log": filler.Bytes(),
		"f.log": lines(errs...),
		// Without the timestamp prefix, and an INFO line quoting ERROR: text.
		"g.log": []byte("INFO: command output: ERROR: not a level\n" + finish + "\n"),
	}
	dir := storagetest.TempDir(t)
	for name, b := range logs {
		storagetest.Build(t, dir, storagetest.Node{Path: name, Content: b})
	}
	rec := newRecOpener(openRoot(t, dir))
	read := func(name string) LogVerdict {
		t.Helper()
		v, err := ReadLog(rec, name, spec.LogTailBytes, spec.MaxLogErrorLines, spec.LogLineMaxBytes)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return v
	}
	for name, want := range map[string]string{"a.log": "ok", "b.log": "error", "c.log": "error", "d.log": "unknown", "e.log": "ok", "g.log": "ok"} {
		if v := read(name); v.Status != want {
			t.Errorf("%s: %q, want %q (errors %q)", name, v.Status, want, v.Errors)
		}
	}
	if v := read("c.log"); len(v.Errors) != 2 || !strings.HasSuffix(v.Errors[1], "failed - vma_queue_write: write error") {
		t.Errorf("c.log errors %q", v.Errors)
	}
	if v := read("e.log"); len(v.Errors) != 0 {
		t.Errorf("e.log: errors %q from outside the tail", v.Errors)
	}
	size := int64(len(logs["e.log"]))
	if off, ok := rec.firstAt["e.log"]; !ok || off != size-spec.LogTailBytes {
		t.Errorf("e.log: first read at %d (recorded %v), want %d", off, ok, size-spec.LogTailBytes)
	}
	if n := rec.read["e.log"]; n > 2*spec.LogTailBytes {
		t.Errorf("e.log: read %d bytes over two reads, more than the tail each time", n)
	}
	v := read("f.log")
	if v.Status != "error" || len(v.Errors) != spec.MaxLogErrorLines {
		t.Fatalf("f.log: %q with %d lines", v.Status, len(v.Errors))
	}
	for _, l := range v.Errors {
		if len(l) > spec.LogLineMaxBytes || !utf8.ValidString(l) {
			t.Errorf("f.log line of %d bytes, valid %v: %.60q", len(l), utf8.ValidString(l), l)
		}
		if !strings.Contains(l, "ERROR: failure") {
			t.Errorf("f.log kept a line that is not an ERROR line: %.60q", l)
		}
	}
}

func TestTCSTG95_TheCapturedHomeServerLogHeadsAnd(t *testing.T) {
	capDir := filepath.Join("..", "testdata", "captures", "home-server", "vzdump")
	head := capturegate.Require(t, filepath.Join(capDir, "newest-log-head.txt"))
	tail := capturegate.Require(t, filepath.Join(capDir, "newest-log-tail.txt"))
	var log []byte
	for _, p := range []string{head, tail} {
		out, _, err := storagetest.CaptureStdout(p)
		if err != nil {
			t.Fatal(err)
		}
		log = append(log, out...)
	}

	// The oracle (A-11): the last finish line against the last ERROR: line,
	// each after an optional `YYYY-MM-DD HH:MM:SS ` prefix.
	prefix := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} `)
	lastFinish, lastErr := -1, -1
	for i, l := range strings.Split(string(log), "\n") {
		l = prefix.ReplaceAllString(l, "")
		switch {
		case strings.HasPrefix(l, "INFO: Finished Backup of VM"):
			lastFinish = i
		case strings.HasPrefix(l, "ERROR:"):
			lastErr = i
		}
	}
	want := "error"
	switch {
	case lastFinish < 0 && lastErr < 0:
		t.Fatalf("A-11 falsified: the captured log has neither an `INFO: Finished Backup of VM` nor an `ERROR:` line")
	case lastFinish > lastErr:
		want = "ok"
	}

	const arch = "vzdump-qemu-100-2026_01_01-00_00_00.vma.zst"
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir,
		storagetest.Node{Path: arch, Size: 1},
		storagetest.Node{Path: "vzdump-qemu-100-2026_01_01-00_00_00.log", Content: log},
	)
	root := openRoot(t, dir)
	v, err := ReadLog(root, "vzdump-qemu-100-2026_01_01-00_00_00.log", spec.LogTailBytes, spec.MaxLogErrorLines, spec.LogLineMaxBytes)
	if err != nil || v.Status != want {
		t.Errorf("ReadLog: %+v, %v; oracle %q", v, err, want)
	}

	notesCap := filepath.Join(capDir, "newest-notes-line1.txt")
	if _, err := os.Stat(notesCap); err != nil {
		t.Logf("no captured notes (%v): the newest archive had no readable .notes", err)
		return
	}
	line, _, err := storagetest.CaptureStdout(notesCap)
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Build(t, dir, storagetest.Node{Path: arch + ".notes", Content: line})
	got, _, err := Notes(root, arch+".notes", spec.NotesMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(got, "\n")
	if wantLine, _, _ := strings.Cut(string(line), "\n"); first != wantLine {
		t.Errorf("Notes first line %q, captured %q", first, wantLine)
	}
}
