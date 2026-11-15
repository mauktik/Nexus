package host

// Shared host-boundary fakes of planning/system-provider/test-strategy.md
// (Mocking Strategy, Fake Fidelity Contracts): F-FS (fixtureFS), F-STATFS
// (newFakeStatfs), the mount-view helpers (addMount, stackRoot, selfRoot)
// and the SP-* fault profiles. WI 2.2 creates them; the WI 3.x and 4.x
// tests use them.
//
// What F-FS encodes from real Linux, each item from a capture or a cited
// kernel behaviour:
//   - sysfs class directories list their entries as symlinks: ReadDir of
//     sys/class/net, sys/block, sys/class/hwmon and sys/class/thermal gives
//     fs.ModeSymlink entries whose IsDir() is false (sysfs-rules: class
//     directories hold "only flat lists of symlinks pointing to the unified
//     /sys/devices tree"; the captures store them as plain directories);
//   - procfs files report size 0 and sysfs attributes 4096 on Stat, so a
//     reader that sizes its buffer from Stat gets nothing;
//   - errors are *fs.PathError wrapping syscall.Errno, so errors.Is with
//     fs.ErrPermission and fs.ErrNotExist behaves as with os.DirFS("/");
//   - an attribute the capture could not read with EINVAL (UNREADABLE,
//     e.g. sys/class/net/lo/speed) exists, opens, and fails on read with
//     EINVAL, as the kernel does for a virtual NIC's speed;
//   - etc/localtime's link target comes from tools/readlink-localtime (the
//     capture stores a flat copy);
//   - the empty directories of PLACEHOLDERS, which git does not keep, are
//     recreated;
//   - advance() serves t1/ for proc/stat, proc/diskstats, proc/net/dev and
//     proc/<pid>/{stat,status} from then on (a pid absent in t1/ has
//     exited), and lists t1/'s pids under proc.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The two committed WI 2.1 captures.
const (
	capUBU = "ubuntu-systemd"
	capALP = "alpine-openrc"
)

// fsCall is one access F-FS saw: op is open, readdir, stat, lstat or
// readlink.
type fsCall struct{ op, name string }

func (c fsCall) String() string { return c.op + " " + c.name }

// fsFault is one configured fault. match decides the paths it covers.
type fsFault struct {
	match  func(name string) bool
	errno  syscall.Errno // returned by every op (deny, eperm, missing)
	onRead syscall.Errno // returned by Read of an opened file (einval)
	block  chan struct{} // Open waits for it to close
	panicN int           // Open panics on the panicN-th matching call
	seen   int
	hook   func() // slow: called on each matching Open
}

// fakeFS is F-FS: a capture's fs/ tree with Linux shaping, faults and a
// recorder. It implements fs.FS, fs.ReadDirFS, fs.StatFS and
// fs.ReadLinkFS, and deliberately not fs.ReadFileFS, so every read is an
// Open.
type fakeFS struct {
	t       *testing.T
	capture string
	root    string // testdata/<capture>
	base    fs.FS  // testdata/<capture>/fs
	t1      fs.FS  // testdata/<capture>/t1

	mu       sync.Mutex
	calls    []fsCall
	bytes    map[string]int64
	faults   []*fsFault
	files    map[string][]byte // replace, mountinfo views, UNREADABLE placeholders
	dirs     map[string]bool   // PLACEHOLDERS empty directories
	removed  map[string]bool
	links    map[string]string
	advanced bool
}

// fixtureFS returns F-FS over testdata/<capture>/fs.
func fixtureFS(t *testing.T, capture string) *fakeFS {
	t.Helper()
	root := filepath.Join("testdata", capture)
	f := &fakeFS{
		t:       t,
		capture: capture,
		root:    root,
		base:    os.DirFS(filepath.Join(root, "fs")),
		t1:      os.DirFS(filepath.Join(root, "t1")),
		bytes:   map[string]int64{},
		files:   map[string][]byte{},
		dirs:    map[string]bool{},
		removed: map[string]bool{},
		links:   map[string]string{},
	}
	for _, l := range fakeLines(t, filepath.Join(root, "PLACEHOLDERS"), true) {
		kind, p, ok := strings.Cut(l, " ")
		name, inFS := strings.CutPrefix(p, "fs/")
		if !ok || !inFS {
			t.Fatalf("%s/PLACEHOLDERS: unexpected line %q", root, l)
		}
		switch kind {
		case "dir":
			f.dirs[name] = true
		case "file":
			if _, err := fs.Stat(f.base, name); err != nil {
				f.files[name] = nil
			}
		default:
			t.Fatalf("%s/PLACEHOLDERS: unexpected kind in %q", root, l)
		}
	}
	for _, l := range fakeLines(t, filepath.Join(root, "UNREADABLE"), true) {
		name, msg, ok := strings.Cut(l, ": ")
		if !ok || strings.HasPrefix(name, "stat") {
			continue // "stat <mount>" and "statfs <mount>" lines are F-STATFS's
		}
		if strings.Contains(msg, "Invalid argument") {
			f.files[name] = nil
			f.faults = append(f.faults, &fsFault{match: exactly(name), onRead: syscall.EINVAL})
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "tools", "readlink-localtime")); err == nil {
		f.links["etc/localtime"] = strings.TrimSpace(string(b))
	}
	return f
}

// fakeLines reads a capture metadata file as non-empty lines.
func fakeLines(t *testing.T, name string, optional bool) []string {
	t.Helper()
	b, err := os.ReadFile(name)
	if optional && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimRight(l, " \r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// exactly matches one path.
func exactly(name string) func(string) bool {
	return func(n string) bool { return n == name }
}

// under matches paths that pattern (a path.Match glob) matches, and every
// path below one of them, as a fault on a directory covers its contents.
func under(pattern string) func(string) bool {
	return func(n string) bool {
		for p := n; p != "."; p = path.Dir(p) {
			if ok, _ := path.Match(pattern, p); ok {
				return true
			}
		}
		return false
	}
}

func (f *fakeFS) addFault(ft *fsFault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, ft)
}

// deny makes every access to pattern (a glob) and below fail with EACCES.
func (f *fakeFS) deny(pattern string) {
	f.addFault(&fsFault{match: under(pattern), errno: syscall.EACCES})
}

// eperm is deny with EPERM.
func (f *fakeFS) eperm(pattern string) {
	f.addFault(&fsFault{match: under(pattern), errno: syscall.EPERM})
}

// missing makes pattern and below fail with ENOENT.
func (f *fakeFS) missing(pattern string) {
	f.addFault(&fsFault{match: under(pattern), errno: syscall.ENOENT})
}

// einval makes reads of pattern fail with EINVAL; Stat and Open succeed.
func (f *fakeFS) einval(pattern string) {
	f.addFault(&fsFault{match: under(pattern), onRead: syscall.EINVAL})
}

// block makes Open of pattern wait, after it is recorded, until release
// (or the test's cleanup) is called. It ignores contexts, as a D-state read
// does.
func (f *fakeFS) block(pattern string) (release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	f.t.Cleanup(release)
	f.addFault(&fsFault{match: under(pattern), block: ch})
	return release
}

// panicOn makes the n-th Open of pattern (counting from 1) panic.
func (f *fakeFS) panicOn(pattern string, n int) {
	f.addFault(&fsFault{match: under(pattern), panicN: n})
}

// slow calls hook on each Open of a path matching glob, before it opens.
func (f *fakeFS) slow(glob string, hook func()) {
	f.addFault(&fsFault{match: under(glob), hook: hook})
}

// replace serves data at name from now on, creating it (and its parent
// directories) when the capture lacks it.
func (f *fakeFS) replace(name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = bytes.Clone(data)
	delete(f.removed, name)
}

// remove makes name and everything below it absent.
func (f *fakeFS) remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed[name] = true
	for k := range f.files {
		if k == name || strings.HasPrefix(k, name+"/") {
			delete(f.files, k)
		}
	}
}

// advance switches the dynamic files to the capture's t1/ sample.
func (f *fakeFS) advance() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advanced = true
}

// recorded returns every call so far, in order.
func (f *fakeFS) recorded() []fsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// resetRecorder clears the calls and byte counters.
func (f *fakeFS) resetRecorder() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.bytes = map[string]int64{}
}

// count is how many times op was called on name.
func (f *fakeFS) count(op, name string) int {
	n := 0
	for _, c := range f.recorded() {
		if c.op == op && c.name == name {
			n++
		}
	}
	return n
}

// bytesRead is how many bytes Read returned for name, over all opens.
func (f *fakeFS) bytesRead(name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bytes[name]
}

// rawRead is the unrecorded handle: the content F-FS serves for name now,
// without faults or recording, for computing expectations.
func (f *fakeFS) rawRead(name string) []byte {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := f.contentLocked(name)
	if err != nil {
		f.t.Fatalf("F-FS raw read %s: %v", name, err)
	}
	return b
}

// record notes a call and returns the faults that match name.
func (f *fakeFS) record(op, name string) []*fsFault {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fsCall{op: op, name: name})
	var out []*fsFault
	for _, ft := range f.faults {
		if ft.match(name) {
			out = append(out, ft)
		}
	}
	return out
}

func errnoOf(faults []*fsFault) syscall.Errno {
	for _, ft := range faults {
		if ft.errno != 0 {
			return ft.errno
		}
	}
	return 0
}

// t1Path reports whether name is served from t1/ after advance.
func t1Path(name string) bool {
	switch name {
	case "proc/stat", "proc/diskstats", "proc/net/dev":
		return true
	}
	parts := strings.Split(name, "/")
	return len(parts) == 3 && parts[0] == "proc" && allDigits(parts[1]) && (parts[2] == "stat" || parts[2] == "status")
}

// pidDir reports whether name is proc/<pid>.
func pidDir(name string) bool {
	d, pid := path.Split(name)
	return d == "proc/" && allDigits(pid)
}

// gone reports whether name or an ancestor was removed. Callers hold mu.
func (f *fakeFS) goneLocked(name string) bool {
	for p := name; p != "."; p = path.Dir(p) {
		if f.removed[p] {
			return true
		}
	}
	if f.advanced && pidDir(name) {
		_, err := fs.Stat(f.t1, name)
		return err != nil
	}
	return false
}

// overlayDirLocked reports whether name is a directory made by the overlay:
// a placeholder, or the parent of a replaced file.
func (f *fakeFS) overlayDirLocked(name string) bool {
	if f.dirs[name] {
		return true
	}
	for k := range f.files {
		if strings.HasPrefix(k, name+"/") {
			return true
		}
	}
	for k := range f.dirs {
		if strings.HasPrefix(k, name+"/") {
			return true
		}
	}
	return false
}

// contentLocked returns the bytes of a regular file.
func (f *fakeFS) contentLocked(name string) ([]byte, error) {
	if f.goneLocked(name) {
		return nil, syscall.ENOENT
	}
	if b, ok := f.files[name]; ok {
		return b, nil
	}
	src := f.base
	if f.advanced && t1Path(name) {
		src = f.t1
	}
	b, err := fs.ReadFile(src, name)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return nil, pe.Err
		}
		return nil, err
	}
	return b, nil
}

// infoLocked returns the shaped FileInfo of name.
func (f *fakeFS) infoLocked(name string) (fs.FileInfo, error) {
	if f.goneLocked(name) {
		return nil, syscall.ENOENT
	}
	if b, ok := f.files[name]; ok {
		return shapedInfo(name, memInfo{name: path.Base(name), size: int64(len(b)), mode: 0o444}), nil
	}
	src := f.base
	if f.advanced && (t1Path(name) || pidDir(name)) {
		src = f.t1
	}
	fi, err := fs.Stat(src, name)
	if err == nil {
		return shapedInfo(name, fi), nil
	}
	if f.overlayDirLocked(name) {
		return memInfo{name: path.Base(name), mode: fs.ModeDir | 0o555}, nil
	}
	return nil, syscall.ENOENT
}

// symlinkClassDirs list their entries as symlinks on a real kernel.
var symlinkClassDirs = map[string]bool{
	"sys/class/net":     true,
	"sys/block":         true,
	"sys/class/hwmon":   true,
	"sys/class/thermal": true,
}

// shapedInfo gives procfs files size 0 and sysfs attributes 4096.
func shapedInfo(name string, fi fs.FileInfo) fs.FileInfo {
	if !fi.Mode().IsRegular() {
		return fi
	}
	switch {
	case strings.HasPrefix(name, "proc/"):
		return memInfo{name: fi.Name(), size: 0, mode: fi.Mode(), mod: fi.ModTime()}
	case strings.HasPrefix(name, "sys/"):
		return memInfo{name: fi.Name(), size: 4096, mode: fi.Mode(), mod: fi.ModTime()}
	}
	return fi
}

type memInfo struct {
	name string
	size int64
	mode fs.FileMode
	mod  time.Time
}

func (m memInfo) Name() string       { return m.name }
func (m memInfo) Size() int64        { return m.size }
func (m memInfo) Mode() fs.FileMode  { return m.mode }
func (m memInfo) ModTime() time.Time { return m.mod }
func (m memInfo) IsDir() bool        { return m.mode.IsDir() }
func (m memInfo) Sys() any           { return nil }

// symlinkEntry is a class directory entry as sysfs lists it.
type symlinkEntry struct{ name string }

func (e symlinkEntry) Name() string      { return e.name }
func (e symlinkEntry) IsDir() bool       { return false }
func (e symlinkEntry) Type() fs.FileMode { return fs.ModeSymlink }
func (e symlinkEntry) Info() (fs.FileInfo, error) {
	return memInfo{name: e.name, mode: fs.ModeSymlink | 0o777}, nil
}

func (f *fakeFS) Open(name string) (fs.File, error) {
	faults := f.record("open", name)
	for _, ft := range faults {
		if ft.hook != nil {
			ft.hook()
		}
	}
	f.mu.Lock()
	for _, ft := range faults {
		if ft.panicN > 0 {
			ft.seen++
			if ft.seen == ft.panicN {
				f.mu.Unlock()
				panic("F-FS: planted panic on open " + name)
			}
		}
	}
	f.mu.Unlock()
	for _, ft := range faults {
		if ft.block != nil {
			<-ft.block
		}
	}
	if e := errnoOf(faults); e != 0 {
		return nil, &fs.PathError{Op: "open", Path: name, Err: e}
	}
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, err := f.infoLocked(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	var data []byte
	if !fi.IsDir() {
		if data, err = f.contentLocked(name); err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
	}
	var onRead syscall.Errno
	for _, ft := range faults {
		if ft.onRead != 0 {
			onRead = ft.onRead
		}
	}
	return &fakeFile{fs: f, name: name, info: fi, r: bytes.NewReader(data), onRead: onRead}, nil
}

// fakeFile serves an opened file and counts the bytes it returns.
type fakeFile struct {
	fs     *fakeFS
	name   string
	info   fs.FileInfo
	r      *bytes.Reader
	onRead syscall.Errno
}

func (ff *fakeFile) Stat() (fs.FileInfo, error) { return ff.info, nil }
func (ff *fakeFile) Close() error               { return nil }

func (ff *fakeFile) Read(p []byte) (int, error) {
	if ff.onRead != 0 {
		return 0, &fs.PathError{Op: "read", Path: ff.name, Err: ff.onRead}
	}
	if ff.info.IsDir() {
		return 0, &fs.PathError{Op: "read", Path: ff.name, Err: syscall.EISDIR}
	}
	n, err := ff.r.Read(p)
	ff.fs.mu.Lock()
	ff.fs.bytes[ff.name] += int64(n)
	ff.fs.mu.Unlock()
	return n, err
}

func (f *fakeFS) Stat(name string) (fs.FileInfo, error) {
	if e := errnoOf(f.record("stat", name)); e != 0 {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: e}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, err := f.infoLocked(name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return fi, nil
}

func (f *fakeFS) Lstat(name string) (fs.FileInfo, error) {
	if e := errnoOf(f.record("lstat", name)); e != 0 {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: e}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, err := f.infoLocked(name)
	if err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: err}
	}
	if _, ok := f.links[name]; ok || symlinkClassDirs[path.Dir(name)] {
		return memInfo{name: fi.Name(), mode: fs.ModeSymlink | 0o777}, nil
	}
	return fi, nil
}

func (f *fakeFS) ReadLink(name string) (string, error) {
	if e := errnoOf(f.record("readlink", name)); e != 0 {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: e}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.infoLocked(name); err != nil {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: err}
	}
	if target, ok := f.links[name]; ok {
		return target, nil
	}
	return "", &fs.PathError{Op: "readlink", Path: name, Err: syscall.EINVAL}
}

func (f *fakeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if e := errnoOf(f.record("readdir", name)); e != 0 {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: e}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.goneLocked(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: syscall.ENOENT}
	}
	entries := map[string]fs.DirEntry{}
	found := false
	if base, err := fs.ReadDir(f.base, name); err == nil {
		found = true
		for _, e := range base {
			if !(f.advanced && name == "proc" && allDigits(e.Name())) {
				entries[e.Name()] = e
			}
		}
	}
	if f.advanced && name == "proc" {
		if t1, err := fs.ReadDir(f.t1, name); err == nil {
			for _, e := range t1 {
				if allDigits(e.Name()) {
					entries[e.Name()] = e
				}
			}
		}
	}
	if f.overlayDirLocked(name) {
		found = true
		for k := range f.files {
			if rest, ok := strings.CutPrefix(k, name+"/"); ok {
				child, _, nested := strings.Cut(rest, "/")
				if nested {
					entries[child] = fs.FileInfoToDirEntry(memInfo{name: child, mode: fs.ModeDir | 0o555})
				} else {
					entries[child] = fs.FileInfoToDirEntry(memInfo{name: child, size: int64(len(f.files[k])), mode: 0o444})
				}
			}
		}
		for k := range f.dirs {
			if rest, ok := strings.CutPrefix(k, name+"/"); ok {
				child, _, _ := strings.Cut(rest, "/")
				entries[child] = fs.FileInfoToDirEntry(memInfo{name: child, mode: fs.ModeDir | 0o555})
			}
		}
	}
	if !found {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: syscall.ENOENT}
	}
	out := make([]fs.DirEntry, 0, len(entries))
	for _, n := range slices.Sorted(maps.Keys(entries)) {
		if f.goneLocked(name + "/" + n) {
			continue
		}
		e := entries[n]
		if symlinkClassDirs[name] {
			e = symlinkEntry{name: n}
		}
		out = append(out, e)
	}
	return out, nil
}

// Mount views (test-strategy.md, Mount views; architecture D-9, A-44).
// addMount is the only way a test adds a mount: it appends the mount to
// proc/1/mountinfo and writes proc/self/mountinfo to match the view, with
// mount IDs and parent IDs that fit it. The two files never share mount IDs
// (a new mount namespace copies the table with new IDs) and print the same
// maj:min for one superblock. The helpers write files only; visibility is
// production's job.

// mountView is how a host mount appears in nexusd's namespace.
type mountView struct {
	kind   string // both, hostOnly, overMounted, underAncestor, ambiguous
	majmin string // overMounted, ambiguous
	path   string // underAncestor
}

var (
	viewBoth     = mountView{kind: "both"}
	viewHostOnly = mountView{kind: "hostOnly"}
)

// viewOverMounted stacks a second mount with majmin on the mount, in self.
func viewOverMounted(majmin string) mountView {
	return mountView{kind: "overMounted", majmin: majmin}
}

// viewUnderAncestor adds a PrivateTmp-style private bind at ancestor, in
// self.
func viewUnderAncestor(ancestor string) mountView {
	return mountView{kind: "underAncestor", path: ancestor}
}

// viewAmbiguous adds a sibling with majmin at the same mount point and
// parent, in self.
func viewAmbiguous(majmin string) mountView {
	return mountView{kind: "ambiguous", majmin: majmin}
}

type mountOpts struct {
	hiddenFirst bool   // order(before): the mount's line before the bind or over-mount
	idDesc      bool   // idOrder(desc)
	sameDevAs   string // reuse that mount point's maj:min
}

// mountOpt is an option of addMount.
type mountOpt func(*mountOpts)

// orderBefore writes the mount's own line before the bind or over-mount (a
// mount made before nexusd started). The default is after (the over-mount
// is written first; a mount that propagated later).
func orderBefore() mountOpt { return func(o *mountOpts) { o.hiddenFirst = true } }

// orderAfter is the default order, stated explicitly.
func orderAfter() mountOpt { return func(o *mountOpts) { o.hiddenFirst = false } }

// idOrderDesc assigns the new self mount IDs in descending line order.
func idOrderDesc() mountOpt { return func(o *mountOpts) { o.idDesc = true } }

// idOrderAsc is the default, stated explicitly.
func idOrderAsc() mountOpt { return func(o *mountOpts) { o.idDesc = false } }

// sameDevAs reuses mountPoint's maj:min. Without it addMount refuses a
// maj:min already in either file (Finding 13 item 4).
func sameDevAs(mountPoint string) mountOpt { return func(o *mountOpts) { o.sameDevAs = mountPoint } }

// miEntry is one mountinfo line: mount ID, parent ID, maj:min, root, mount
// point (escaped as in the file) and the remaining fields.
type miEntry struct {
	id, parent int
	majmin     string
	root       string
	mp         string // escaped
	rest       []string
}

func (e miEntry) mountPoint() string { return unescapeMount(e.mp) }

func (e miEntry) String() string {
	return strings.Join(append([]string{strconv.Itoa(e.id), strconv.Itoa(e.parent), e.majmin, e.root, e.mp}, e.rest...), " ")
}

func parseMI(t *testing.T, line string) miEntry {
	t.Helper()
	f := strings.Fields(line)
	if len(f) < 7 {
		t.Fatalf("mountinfo line %q: too few fields", line)
	}
	id, err1 := strconv.Atoi(f[0])
	parent, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || !strings.Contains(f[2], ":") {
		t.Fatalf("mountinfo line %q: bad id, parent or maj:min", line)
	}
	return miEntry{id: id, parent: parent, majmin: f[2], root: f[3], mp: f[4], rest: f[5:]}
}

// unescapeMount decodes mountinfo's octal escapes (\040 and the like).
func unescapeMount(s string) string {
	return captureOctal.ReplaceAllStringFunc(s, func(e string) string {
		n, _ := strconv.ParseUint(e[1:], 8, 8)
		return string(rune(n))
	})
}

// escapeMount encodes a path as mountinfo prints it: show_mountinfo calls
// seq_path_root(..., " \t\n\\") (fs/proc_namespace.c), escaping space,
// tab, newline and backslash as octal.
func escapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t', '\n', '\\':
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (f *fakeFS) mountinfo(t *testing.T, name string) []miEntry {
	t.Helper()
	f.mu.Lock()
	b, err := f.contentLocked(name)
	f.mu.Unlock()
	if err != nil {
		t.Fatalf("F-FS %s: %v", name, err)
	}
	var out []miEntry
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, parseMI(t, l))
		}
	}
	return out
}

func (f *fakeFS) writeMountinfo(name string, entries []miEntry) {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.String())
		b.WriteByte('\n')
	}
	f.replace(name, []byte(b.String()))
}

func maxID(entries []miEntry) int {
	m := 0
	for _, e := range entries {
		m = max(m, e.id)
	}
	return m
}

// isAncestor reports whether a is a proper path ancestor of b.
func isAncestor(a, b string) bool {
	return a != b && (a == "/" || strings.HasPrefix(b, a+"/"))
}

// topAt returns the entry at mount point mp that nothing at mp is stacked
// on (the last such entry when several are), and false when there is none.
func topAt(entries []miEntry, mp string) (miEntry, bool) {
	var top miEntry
	found := false
	for _, e := range entries {
		if e.mountPoint() != mp {
			continue
		}
		covered := false
		for _, o := range entries {
			if o.mountPoint() == mp && o.parent == e.id && o.id != e.id {
				covered = true
			}
		}
		if !covered {
			top, found = e, true
		}
	}
	return top, found
}

// parentFor is the entry a lookup reaches at the longest mount point that
// is a proper ancestor of mp.
func parentFor(t *testing.T, entries []miEntry, mp string) miEntry {
	t.Helper()
	best := ""
	for _, e := range entries {
		if p := e.mountPoint(); isAncestor(p, mp) && len(p) > len(best) {
			best = p
		}
	}
	top, ok := topAt(entries, best)
	if best == "" || !ok {
		t.Fatalf("addMount: no ancestor mount for %s", mp)
	}
	return top
}

// addMount appends line (a proc/1/mountinfo line; its IDs are replaced) to
// proc/1/mountinfo and writes proc/self/mountinfo to match view.
func (f *fakeFS) addMount(t *testing.T, line string, view mountView, opts ...mountOpt) {
	t.Helper()
	var o mountOpts
	for _, opt := range opts {
		opt(&o)
	}
	host := f.mountinfo(t, "proc/1/mountinfo")
	self := f.mountinfo(t, "proc/self/mountinfo")
	m := parseMI(t, line)
	mp := m.mountPoint()
	if o.sameDevAs != "" {
		src, ok := topAt(host, o.sameDevAs)
		if !ok {
			t.Fatalf("addMount: sameDevAs(%s): no such mount in proc/1/mountinfo", o.sameDevAs)
		}
		m.majmin = src.majmin
	} else {
		for _, e := range append(slices.Clone(host), self...) {
			if e.majmin == m.majmin {
				t.Fatalf("addMount %s: maj:min %s is already in the capture (%s); use sameDevAs", mp, m.majmin, e.mountPoint())
			}
		}
	}

	hm := m
	hm.id = maxID(host) + 1
	hm.parent = parentFor(t, host, mp).id
	f.writeMountinfo("proc/1/mountinfo", append(host, hm))

	if view.kind == "hostOnly" {
		return
	}
	sm := m
	sm.parent = parentFor(t, self, mp).id
	var extra miEntry
	switch view.kind {
	case "both":
		assignIDs(self, &o, &sm)
		f.writeMountinfo("proc/self/mountinfo", append(self, sm))
		return
	case "overMounted":
		extra = m
		extra.majmin = view.majmin
	case "ambiguous":
		extra = m
		extra.majmin = view.majmin
		extra.parent = sm.parent
	case "underAncestor":
		if !isAncestor(view.path, mp) {
			t.Fatalf("addMount: underAncestor(%s) is not an ancestor of %s", view.path, mp)
		}
		holder, stacked := topAt(self, view.path)
		if !stacked {
			holder = parentFor(t, self, view.path)
		}
		extra = holder
		extra.root = "/tmp/systemd-private-0123456789abcdef0123456789abcdef-nexusd.service-AbCdEf/tmp"
		extra.mp = escapeMount(view.path)
		extra.parent = holder.id // stacked on it (b), or its child at the ancestor (a)
	default:
		t.Fatalf("addMount: unknown view %q", view.kind)
	}
	// The over-mount, bind or sibling is written first unless orderBefore.
	lines := []*miEntry{&extra, &sm}
	if o.hiddenFirst {
		lines = []*miEntry{&sm, &extra}
	}
	assignIDs(self, &o, lines...)
	if view.kind == "overMounted" {
		extra.parent = sm.id // stacked on the mount
	}
	f.writeMountinfo("proc/self/mountinfo", append(self, *lines[0], *lines[1]))
}

// assignIDs gives the new self entries fresh mount IDs, ascending or
// descending in line order.
func assignIDs(self []miEntry, o *mountOpts, lines ...*miEntry) {
	next := maxID(self) + 1
	for i, e := range lines {
		if o.idDesc {
			e.id = next + len(lines) - 1 - i
		} else {
			e.id = next + i
		}
	}
}

// selfRootEntry is the index of self's root entry: at /, with a parent that
// is its own ID or no listed entry's.
func selfRootEntry(t *testing.T, self []miEntry) int {
	t.Helper()
	ids := map[int]bool{}
	for _, e := range self {
		ids[e.id] = true
	}
	for i, e := range self {
		if e.mountPoint() == "/" && (e.parent == e.id || !ids[e.parent]) {
			return i
		}
	}
	t.Fatal("proc/self/mountinfo has no root entry")
	return -1
}

// stackRoot adds a second / entry to self, with the root's maj:min,
// stacked on the root entry.
func (f *fakeFS) stackRoot(t *testing.T) {
	t.Helper()
	self := f.mountinfo(t, "proc/self/mountinfo")
	top, _ := topAt(self, "/")
	e := self[selfRootEntry(t, self)]
	e.id = maxID(self) + 1
	e.parent = top.id
	f.writeMountinfo("proc/self/mountinfo", append(self, e))
}

// Self root forms for selfRoot.
const (
	rootUnlisted  = "unlisted"  // parent ID that no entry has (after systemd's chroot; A-44)
	rootOwnParent = "ownParent" // parent ID equal to its own (proc(5) namespace root)
)

// selfRoot rewrites the parent ID of self's root entry.
func (f *fakeFS) selfRoot(t *testing.T, form string) {
	t.Helper()
	self := f.mountinfo(t, "proc/self/mountinfo")
	i := selfRootEntry(t, self)
	switch form {
	case rootUnlisted:
		self[i].parent = maxID(self) + 1000
	case rootOwnParent:
		self[i].parent = self[i].id
	default:
		t.Fatalf("selfRoot: unknown form %q", form)
	}
	f.writeMountinfo("proc/self/mountinfo", self)
}

// fakeStatfs is F-STATFS, the Statfs seam over tools/statfs.txt (counts) and
// tools/stat-mounts.txt (IsDir from %F; %d is never read). It mirrors the
// Linux seam: a mount point that is not a directory answers IsDir false with
// zero counts, and one the capture could not stat answers the captured
// errno (UNREADABLE), else ENOENT.
type fakeStatfs struct {
	t      *testing.T
	mu     sync.Mutex
	rows   map[string]Usage
	errs   map[string]error
	hangs  map[string]chan struct{}
	delays map[string]time.Duration
	counts map[string]int
	calls  []string
}

// newFakeStatfs loads F-STATFS from testdata/<capture>/tools.
func newFakeStatfs(t *testing.T, capture string) *fakeStatfs {
	t.Helper()
	root := filepath.Join("testdata", capture)
	st := &fakeStatfs{
		t:      t,
		rows:   map[string]Usage{},
		errs:   map[string]error{},
		hangs:  map[string]chan struct{}{},
		delays: map[string]time.Duration{},
		counts: map[string]int{},
	}
	isDir := map[string]bool{}
	for _, l := range fakeLines(t, filepath.Join(root, "tools", "stat-mounts.txt"), false) {
		mp, typ, ok := parseStatMountsLine(l)
		if !ok {
			t.Fatalf("%s/tools/stat-mounts.txt: bad line %q", root, l)
		}
		isDir[mp] = typ == "directory"
	}
	for _, l := range fakeLines(t, filepath.Join(root, "tools", "statfs.txt"), false) {
		mp, ok := parseStatfsLine(l)
		if !ok {
			t.Fatalf("%s/tools/statfs.txt: bad line %q", root, l)
		}
		f := strings.SplitN(l, " ", 7)
		n := make([]uint64, 6)
		for i := range n {
			n[i], _ = strconv.ParseUint(f[i], 10, 64) // checked by parseStatfsLine
		}
		u := Usage{IsDir: true, Blocks: n[0], Bfree: n[1], Bavail: n[2], Frsize: n[3], Files: n[4], Ffree: n[5]}
		if d, ok := isDir[mp]; ok && !d {
			u = Usage{}
		}
		st.rows[mp] = u
	}
	for _, l := range fakeLines(t, filepath.Join(root, "UNREADABLE"), true) {
		what, msg, _ := strings.Cut(l, ": ")
		mp, ok := strings.CutPrefix(what, "stat ")
		if !ok {
			continue
		}
		op := "stat"
		var e syscall.Errno
		switch {
		case strings.Contains(msg, "Too many levels of symbolic links"):
			e = syscall.ELOOP
		case strings.Contains(msg, "No such file or directory"):
			e = syscall.ENOENT
		case strings.Contains(msg, "Permission denied"):
			e = syscall.EACCES
		default:
			continue
		}
		st.errs[mp] = &fs.PathError{Op: op, Path: mp, Err: e}
	}
	return st
}

// Statfs is the seam passed as Options.Statfs or to newSource.
func (s *fakeStatfs) Statfs(mount string) (Usage, error) {
	s.mu.Lock()
	s.counts[mount]++
	s.calls = append(s.calls, mount)
	hang, delay := s.hangs[mount], s.delays[mount]
	s.mu.Unlock()
	if hang != nil {
		<-hang // ignores contexts, as an uninterruptible NFS stat does
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.errs[mount]; ok {
		return Usage{}, err
	}
	if u, ok := s.rows[mount]; ok {
		return u, nil
	}
	return Usage{}, &fs.PathError{Op: "stat", Path: mount, Err: syscall.ENOENT}
}

// row is the configured answer for mount.
func (s *fakeStatfs) row(mount string) Usage {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.rows[mount]
	if !ok {
		s.t.Fatalf("F-STATFS: no row for %s", mount)
	}
	return u
}

// set configures mount's answer, clearing a configured error.
func (s *fakeStatfs) set(mount string, u Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[mount] = u
	delete(s.errs, mount)
}

// setErr configures mount to fail with err.
func (s *fakeStatfs) setErr(mount string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[mount] = err
}

// hang blocks every call for mount, stat and statfs alike, until release
// (or the test's cleanup).
func (s *fakeStatfs) hang(mount string) (release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(ch)
			s.mu.Lock()
			if s.hangs[mount] == ch {
				delete(s.hangs, mount)
			}
			s.mu.Unlock()
		})
	}
	s.mu.Lock()
	s.hangs[mount] = ch
	s.mu.Unlock()
	s.t.Cleanup(release)
	return release
}

// delay answers mount after d of real time, ignoring contexts.
func (s *fakeStatfs) delay(mount string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays[mount] = d
}

// count is how many calls mount received.
func (s *fakeStatfs) count(mount string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[mount]
}

// SP-* fault profiles (test-strategy.md, Test Data).

// spSandbox applies SP-SANDBOX's file denials: EACCES on proc/1/mountinfo,
// sys/class/dmi/id/*, proc/pressure/*, and on proc/<pid>/status of every
// pid whose real uid is not 0. Its command part (F-RUN timedatectl exit 1)
// belongs to F-RUN.
func spSandbox(t *testing.T, f *fakeFS) {
	t.Helper()
	f.deny("proc/1/mountinfo")
	f.deny("sys/class/dmi/id/*")
	f.deny("proc/pressure/*")
	entries, err := fs.ReadDir(f.base, "proc")
	if err != nil {
		t.Fatalf("SP-SANDBOX: %v", err)
	}
	for _, e := range entries {
		if !allDigits(e.Name()) {
			continue
		}
		name := "proc/" + e.Name() + "/status"
		if uid, ok := statusRealUID(f.rawRead(name)); !ok || uid != 0 {
			f.deny(name)
		}
	}
}

// statusRealUID is the first field of a status file's Uid: line.
func statusRealUID(b []byte) (int, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				return 0, false
			}
			n, err := strconv.Atoi(f[0])
			return n, err == nil
		}
	}
	return 0, false
}

// spPreflightFiles are the three files Preflight reads.
var spPreflightFiles = []string{"proc/stat", "proc/meminfo", "proc/self/mountinfo"}

// spDenyAll applies SP-DENY-ALL: EACCES on every path except the three
// Preflight files.
func spDenyAll(f *fakeFS) {
	f.addFault(&fsFault{
		match: func(n string) bool { return !slices.Contains(spPreflightFiles, n) },
		errno: syscall.EACCES,
	})
}

// spNoPSI applies SP-NO-PSI: ENOENT on proc/pressure.
func spNoPSI(f *fakeFS) { f.missing("proc/pressure") }

// spHungNFSMount is SP-HUNG-NFS's mount: nfs4 with a fresh maj:min 0:250.
const spHungNFSMount = "/mnt/nfs"

// spHungNFS applies SP-HUNG-NFS: /mnt/nfs added with view both, so it is
// visible and the seam is reached, and F-STATFS hanging on it (the whole
// seam call, the directory check included). F-FS has no mnt/nfs entry.
func spHungNFS(t *testing.T, f *fakeFS, st *fakeStatfs) (release func()) {
	t.Helper()
	f.addMount(t, "0 0 0:250 / /mnt/nfs rw,relatime shared:900 - nfs4 nfs.example:/export rw,vers=4.2", viewBoth)
	return st.hang(spHungNFSMount)
}

// spVMNoSensors applies SP-VM-NO-SENSORS: sys/class/hwmon and
// sys/class/thermal exist and are empty.
func spVMNoSensors(t *testing.T, f *fakeFS) {
	t.Helper()
	for _, dir := range []string{"sys/class/hwmon", "sys/class/thermal"} {
		if entries, err := fs.ReadDir(f.base, dir); err == nil {
			for _, e := range entries {
				f.remove(dir + "/" + e.Name())
			}
		}
		f.mu.Lock()
		f.dirs[dir] = true
		f.mu.Unlock()
	}
}

// eventually polls cond every 5 ms until it holds or d passes.
func eventually(t *testing.T, d time.Duration, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not reached within %v: "+format, append([]any{d}, args...)...)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitGoroutines waits until the goroutine count is back to baseline.
func waitGoroutines(t *testing.T, baseline int) {
	t.Helper()
	eventually(t, 2*time.Second, func() bool { return runtime.NumGoroutine() <= baseline },
		"goroutines back to baseline %d (now %d)", baseline, runtime.NumGoroutine())
}
