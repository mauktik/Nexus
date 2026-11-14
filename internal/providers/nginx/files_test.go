//go:build linux

package nginx

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/config"
)

const tc90Temp = ".a.nexus-01J.tmp"

var testMaxConfigBytes = config.Default().Providers.Nginx.Settings.MaxConfigBytes

// testIncludeDir opens dir as the helper opens includeDir and returns its fd.
// The directory is the test's own, so that is the owner includeDir must have.
func testIncludeDir(t *testing.T, dir string) int {
	t.Helper()
	setIncludeDirUID(t, uint32(os.Getuid()))
	f, err := openIncludeDir(Settings{IncludeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return int(f.Fd())
}

// writeOwned creates path holding content with mode and owner uid:gid.
func writeOwned(t *testing.T, path, content string, mode os.FileMode, uid, gid int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
}

func lstat(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// checkFile asserts that path is a regular file holding content, with
// permission bits mode and owner uid:gid.
func checkFile(t *testing.T, path, content string, mode uint32, uid, gid int) {
	t.Helper()
	st := lstat(t, path)
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		t.Errorf("%s has type %#o, want a regular file", path, st.Mode&unix.S_IFMT)
	}
	if got := readString(t, path); got != content {
		t.Errorf("%s holds %q, want %q", path, got, content)
	}
	if got := st.Mode & 0o7777; got != mode {
		t.Errorf("%s has mode %#o, want %#o", path, got, mode)
	}
	if int(st.Uid) != uid || int(st.Gid) != gid {
		t.Errorf("%s is owned %d:%d, want %d:%d", path, st.Uid, st.Gid, uid, gid)
	}
}

// ownerIDs are the owner WriteAtomic is asked for: 0:0 as root, as in
// production, else the tester's own IDs, the only ones fchown accepts.
func ownerIDs() (int, int) { return os.Getuid(), os.Getgid() }

// foreignIDs are the owner a pre-existing file gets: a UID and GID other
// than the tester's when root can set them, so restoring the owner is
// proven rather than coincidental.
func foreignIDs() (int, int) {
	if os.Getuid() == 0 {
		return 4321, 4322
	}
	return ownerIDs()
}

type inotifyEvent struct {
	mask uint32
	name string
}

// watchDir starts an inotify watch of every event in dir.
func watchDir(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_ALL_EVENTS); err != nil {
		t.Fatal(err)
	}
	return fd
}

// drainInotify reads the queued events.
func drainInotify(t *testing.T, fd int) []inotifyEvent {
	t.Helper()
	var out []inotifyEvent
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			start := off + unix.SizeofInotifyEvent
			name := string(bytes.TrimRight(buf[start:start+int(ev.Len)], "\x00"))
			out = append(out, inotifyEvent{mask: ev.Mask, name: name})
			off = start + int(ev.Len)
		}
	}
}

// nginx-provider TC-90: WriteAtomic writes a root-owned 0644 file through a hidden temp and rename
// Owner WI 6.1. Requirements: FR-11, ASM-11, L-23.
func TestWriteAtomic(t *testing.T) {
	// nexusd runs under OpenRC with umask 077: the mode must not depend on it (L-54).
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	uid, gid := ownerIDs()
	for _, existing := range []bool{false, true} {
		name := map[bool]string{false: "new", true: "replace"}[existing]
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if existing {
				fuid, fgid := foreignIDs()
				writeOwned(t, filepath.Join(dir, "a.conf"), "old\n", 0o600, fuid, fgid)
			}
			dirfd := testIncludeDir(t, dir)
			wd := watchDir(t, dir)
			content := "server { listen 18080; }\n"

			if _, err := WriteAtomic(dirfd, "a.conf", []byte(content), 0o644, uid, gid, tc90Temp, false); err != nil {
				t.Fatal(err)
			}

			// Drained before the checks below read a.conf and add events of their own.
			events := drainInotify(t, wd)
			checkFile(t, filepath.Join(dir, "a.conf"), content, 0o644, uid, gid)
			create, moved := -1, -1
			for i, ev := range events {
				switch {
				case ev.name == tc90Temp && ev.mask&unix.IN_CREATE != 0 && create < 0:
					create = i
				case ev.name == "a.conf" && ev.mask == unix.IN_MOVED_TO:
					moved = i
				case ev.name == "a.conf":
					t.Errorf("event %#x on a.conf: it was touched other than by the rename", ev.mask)
				case ev.name != "" && ev.name != tc90Temp:
					t.Errorf("event %#x on unexpected entry %q", ev.mask, ev.name)
				}
			}
			if create < 0 || moved < 0 || create > moved {
				t.Errorf("events = %+v, want CREATE %s then MOVED_TO a.conf", events, tc90Temp)
			}
			if got := dirNames(t, dir); !slices.Equal(got, []string{"a.conf"}) {
				t.Errorf("includeDir = %q, want only a.conf (no temp file left)", got)
			}
		})
	}

	t.Run("refuses a path", func(t *testing.T) {
		dir := t.TempDir()
		dirfd := testIncludeDir(t, dir)
		for _, tc := range [][2]string{{"../a.conf", tc90Temp}, {"a.conf", "../.t.tmp"}, {"a.conf", "visible.tmp"}} {
			if _, err := WriteAtomic(dirfd, tc[0], []byte("x"), 0o644, uid, gid, tc[1], false); err == nil {
				t.Errorf("WriteAtomic(%q, temp %q) succeeded, want refused", tc[0], tc[1])
			}
		}
		if got := dirNames(t, dir); len(got) != 0 {
			t.Errorf("includeDir = %q, want empty", got)
		}
	})
}

// nginx-provider TC-91: Backup and Restore return a file to byte, mode and owner identity
// Owner WI 6.1. Requirements: NFR-3, ASM-6.
func TestBackupRestoreIdentity(t *testing.T) {
	uid, gid := ownerIDs()
	fuid, fgid := foreignIDs()
	dir, opDir := t.TempDir(), t.TempDir()
	orig := "server { listen 18080; }\n"
	writeOwned(t, filepath.Join(dir, "a.conf"), orig, 0o640, fuid, fgid)
	before := lstat(t, filepath.Join(dir, "a.conf"))
	dirfd := testIncludeDir(t, dir)

	m, err := Backup(dirfd, "a.conf", opDir, testMaxConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	want := Meta{FileName: "a.conf", Exists: true, SHA256: sha(orig), Size: int64(len(orig)), Mode: 0o640,
		UID: uint32(fuid), GID: uint32(fgid)}
	if m != want {
		t.Errorf("Backup = %+v, want %+v", m, want)
	}
	checkFile(t, PreviousContentPath(opDir), orig, 0o600, uid, gid)
	metaPath := filepath.Join(opDir, "previous", "meta.json")
	checkFile(t, metaPath, readString(t, metaPath), 0o600, uid, gid)
	var onDisk map[string]any
	if err := json.Unmarshal([]byte(readString(t, metaPath)), &onDisk); err != nil {
		t.Fatal(err)
	}
	wantDisk := map[string]any{"fileName": "a.conf", "exists": true, "sha256": sha(orig),
		"size": float64(len(orig)), "mode": float64(0o640), "uid": float64(fuid), "gid": float64(fgid)}
	for k, v := range wantDisk {
		if onDisk[k] != v {
			t.Errorf("meta.json %s = %v, want %v", k, onDisk[k], v)
		}
	}
	if got := dirNames(t, filepath.Join(opDir, "previous")); !slices.Equal(got, []string{"content", "meta.json"}) {
		t.Errorf("previous/ = %q, want content and meta.json", got)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"a.conf"}) {
		t.Errorf("includeDir = %q, want only a.conf: the backup is not kept there", got)
	}

	wrote, err := WriteAtomic(dirfd, "a.conf", []byte("server { listen 18081; }\n"), 0o644, uid, gid, tc90Temp, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(dirfd, m, wrote, opDir, tc90Temp); err != nil {
		t.Fatal(err)
	}
	checkFile(t, filepath.Join(dir, "a.conf"), orig, 0o640, fuid, fgid)
	if h, err := Hash(dirfd, "a.conf"); err != nil || h != sha(orig) || h != sha(readString(t, filepath.Join(dir, "a.conf"))) {
		t.Errorf("Hash = %q, %v, want %s", h, err, sha(orig))
	}
	after := lstat(t, filepath.Join(dir, "a.conf"))
	if after.Size != before.Size || after.Mode != before.Mode || after.Uid != before.Uid || after.Gid != before.Gid {
		t.Errorf("after restore: %+v, want the size, mode and owner of %+v", after, before)
	}

	// Step 4: a file that did not exist is removed by Restore.
	opDir2 := t.TempDir()
	n, err := Backup(dirfd, "n.conf", opDir2, testMaxConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	if n != (Meta{FileName: "n.conf"}) {
		t.Errorf("Backup of a missing file = %+v, want Exists false", n)
	}
	if got := dirNames(t, filepath.Join(opDir2, "previous")); !slices.Equal(got, []string{"meta.json"}) {
		t.Errorf("previous/ = %q, want meta.json only", got)
	}
	created, err := WriteAtomic(dirfd, "n.conf", []byte("server {}\n"), 0o644, uid, gid, ".n.nexus-01J.tmp", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(dirfd, n, created, opDir2, ".n.nexus-01J.tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "n.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("n.conf after restore: %v, want it gone", err)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"a.conf"}) {
		t.Errorf("includeDir = %q, want only a.conf", got)
	}
}

// checkRenameNoReplace runs TC-92 steps 1 and 2 in dir.
func checkRenameNoReplace(t *testing.T, dirfd int, dir string) {
	t.Helper()
	fuid, fgid := foreignIDs()
	content := "server { listen 18080; }\n"
	writeOwned(t, filepath.Join(dir, "a.conf.disabled"), content, 0o640, fuid, fgid)
	ino := lstat(t, filepath.Join(dir, "a.conf.disabled")).Ino

	if err := RenameNoReplace(dirfd, "a.conf.disabled", "a.conf"); err != nil {
		t.Fatal(err)
	}
	checkFile(t, filepath.Join(dir, "a.conf"), content, 0o640, fuid, fgid)
	if got := lstat(t, filepath.Join(dir, "a.conf")).Ino; got != ino {
		t.Errorf("a.conf has inode %d, want %d (the renamed entry)", got, ino)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a.conf.disabled")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a.conf.disabled after rename: %v, want gone", err)
	}

	writeOwned(t, filepath.Join(dir, "b.conf"), "B enabled\n", 0o644, fuid, fgid)
	writeOwned(t, filepath.Join(dir, "b.conf.disabled"), "B disabled\n", 0o640, fuid, fgid)
	inoB, inoBD := lstat(t, filepath.Join(dir, "b.conf")).Ino, lstat(t, filepath.Join(dir, "b.conf.disabled")).Ino
	for _, tc := range [][2]string{{"b.conf.disabled", "b.conf"}, {"b.conf", "b.conf.disabled"}} {
		if err := RenameNoReplace(dirfd, tc[0], tc[1]); !errors.Is(err, unix.EEXIST) {
			t.Errorf("RenameNoReplace(%s, %s) = %v, want EEXIST", tc[0], tc[1], err)
		}
	}
	checkFile(t, filepath.Join(dir, "b.conf"), "B enabled\n", 0o644, fuid, fgid)
	checkFile(t, filepath.Join(dir, "b.conf.disabled"), "B disabled\n", 0o640, fuid, fgid)
	if lstat(t, filepath.Join(dir, "b.conf")).Ino != inoB || lstat(t, filepath.Join(dir, "b.conf.disabled")).Ino != inoBD {
		t.Error("a refused rename changed an inode")
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"a.conf", "b.conf", "b.conf.disabled"}) {
		t.Errorf("includeDir = %q, want a.conf, b.conf and b.conf.disabled", got)
	}
}

// nginx-provider TC-92: RenameNoReplace never overwrites
// Owner WI 6.1. Requirements: FR-9, FR-10, CON-5.
func TestRenameNoReplace(t *testing.T) {
	t.Run("renameat2", func(t *testing.T) {
		calls := 0
		orig := renameat2
		renameat2 = func(od int, op string, nd int, np string, flags uint) error {
			calls++
			return orig(od, op, nd, np, flags)
		}
		t.Cleanup(func() { renameat2 = orig })
		dir := t.TempDir()
		checkRenameNoReplace(t, testIncludeDir(t, dir), dir)
		if calls == 0 {
			t.Error("renameat2 was not called")
		}
	})
	for _, errno := range []unix.Errno{unix.EINVAL, unix.ENOSYS} {
		t.Run("fallback on "+unix.ErrnoName(errno), func(t *testing.T) {
			calls := 0
			orig := renameat2
			renameat2 = func(_ int, _ string, _ int, _ string, flags uint) error {
				calls++
				if flags != unix.RENAME_NOREPLACE {
					t.Errorf("renameat2 flags = %#x, want RENAME_NOREPLACE", flags)
				}
				return errno
			}
			t.Cleanup(func() { renameat2 = orig })
			dir := t.TempDir()
			checkRenameNoReplace(t, testIncludeDir(t, dir), dir)
			if calls != 3 {
				t.Errorf("renameat2 called %d times, want 3 (each falling back)", calls)
			}
		})
	}
}

// nginx-provider TC-E24: A symlink planted at the destination is replaced, not followed
// Owner WI 6.1. Requirements: FR-11, ASM-11.
func TestSymlinkAtDestinationReplaced(t *testing.T) {
	uid, gid := ownerIDs()
	root := t.TempDir()
	dir, outside := filepath.Join(root, "conf.d"), filepath.Join(root, "outside")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeOwned(t, outside, "X\n", 0o644, uid, gid)
	if err := os.Symlink(outside, filepath.Join(dir, "a.conf")); err != nil {
		t.Fatal(err)
	}
	dirfd := testIncludeDir(t, dir)

	if _, err := WriteAtomic(dirfd, "a.conf", []byte("N\n"), 0o644, uid, gid, tc90Temp, false); err != nil {
		t.Fatal(err)
	}
	checkFile(t, filepath.Join(dir, "a.conf"), "N\n", 0o644, uid, gid)
	checkFile(t, outside, "X\n", 0o644, uid, gid)
}

// nginx-provider TC-E25: A crash between temp and rename leaves a harmless hidden file
// Owner WI 6.1. Requirements: FR-7, L-23.
func TestLeftoverTempIsHarmless(t *testing.T) {
	ownedByTester(t)
	f, _ := helperFixture(t, false)
	writeTree(t, f.dir, map[string]string{"a.conf": "server {\n    listen 8181;\n}\n", tc90Temp: "INVALID\n"})
	s := startOps(t, f, nil)

	code, b := s.do(http.MethodGet, "/providers/nginx/sites", nil)
	if code != http.StatusOK {
		t.Fatalf("GET sites = %d %s", code, b)
	}
	var page listPage
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	if names := siteNames(decodeSites(t, page.Items)); !slices.Equal(names, []string{"a"}) {
		t.Errorf("list = %q, want only a: the leftover temp file is not a site", names)
	}

	r := testResultOf(t, s.wait(s.startTest().ID, 30*time.Second))
	if !r.Valid {
		t.Errorf("test op = %+v, want valid: the leftover is never included", r)
	}
	for _, c := range readFakeCalls(t, f.nginx) {
		for _, inc := range c.IncludedFiles {
			if filepath.Base(inc) == tc90Temp {
				t.Errorf("nginx -t included %s", inc)
			}
		}
	}
	if got := readString(t, filepath.Join(f.dir, tc90Temp)); got != "INVALID\n" {
		t.Errorf("the leftover holds %q: it was changed", got)
	}
}

// nginx-provider TC-ERR24: WriteAtomic refuses an existing temp name and changes nothing
// Owner WI 6.1. Requirements: FR-11.
func TestTempNameExists(t *testing.T) {
	uid, gid := ownerIDs()
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		writeOwned(t, filepath.Join(dir, "a.conf"), "P\n", 0o644, uid, gid)
		writeOwned(t, filepath.Join(dir, tc90Temp), "T\n", 0o600, uid, gid)
		inoA, inoT := lstat(t, filepath.Join(dir, "a.conf")).Ino, lstat(t, filepath.Join(dir, tc90Temp)).Ino
		dirfd := testIncludeDir(t, dir)

		_, err := WriteAtomic(dirfd, "a.conf", []byte("N\n"), 0o644, uid, gid, tc90Temp, false)
		if !errors.Is(err, unix.EEXIST) {
			t.Fatalf("WriteAtomic = %v, want EEXIST", err)
		}
		checkFile(t, filepath.Join(dir, "a.conf"), "P\n", 0o644, uid, gid)
		checkFile(t, filepath.Join(dir, tc90Temp), "T\n", 0o600, uid, gid)
		if lstat(t, filepath.Join(dir, "a.conf")).Ino != inoA || lstat(t, filepath.Join(dir, tc90Temp)).Ino != inoT {
			t.Error("a refused write changed an inode")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		dir, outside := filepath.Join(root, "conf.d"), filepath.Join(root, "outside")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeOwned(t, outside, "X\n", 0o644, uid, gid)
		writeOwned(t, filepath.Join(dir, "a.conf"), "P\n", 0o644, uid, gid)
		if err := os.Symlink(outside, filepath.Join(dir, tc90Temp)); err != nil {
			t.Fatal(err)
		}
		dirfd := testIncludeDir(t, dir)

		if _, err := WriteAtomic(dirfd, "a.conf", []byte("N\n"), 0o644, uid, gid, tc90Temp, false); !errors.Is(err, unix.EEXIST) {
			t.Fatalf("WriteAtomic = %v, want EEXIST", err)
		}
		checkFile(t, filepath.Join(dir, "a.conf"), "P\n", 0o644, uid, gid)
		checkFile(t, outside, "X\n", 0o644, uid, gid)
		if l, err := os.Readlink(filepath.Join(dir, tc90Temp)); err != nil || l != outside {
			t.Errorf("the planted symlink reads %q, %v, want %s", l, err, outside)
		}
	})
}

// nginx-provider TC-ERR25: Restore fails loudly when the re-hash does not match
// Owner WI 6.1. Requirements: NFR-3.
func TestRestoreHashMismatchFails(t *testing.T) {
	uid, gid := ownerIDs()
	dir, opDir := t.TempDir(), t.TempDir()
	writeOwned(t, filepath.Join(dir, "a.conf"), "P\n", 0o644, uid, gid)
	dirfd := testIncludeDir(t, dir)
	m, err := Backup(dirfd, "a.conf", opDir, testMaxConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	wrote, err := WriteAtomic(dirfd, "a.conf", []byte("N\n"), 0o644, uid, gid, tc90Temp, false)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte(readString(t, PreviousContentPath(opDir)))
	b[0] ^= 0x01
	if err := os.WriteFile(PreviousContentPath(opDir), b, 0o600); err != nil {
		t.Fatal(err)
	}

	err = Restore(dirfd, m, wrote, opDir, tc90Temp)
	if err == nil {
		t.Fatal("Restore of a corrupted backup succeeded")
	}
	for _, h := range []string{sha("P\n"), sha(string(b))} {
		if !strings.Contains(err.Error(), h) {
			t.Errorf("error %q does not name sha256 %s", err, h)
		}
	}
	// The corrupted bytes are never written into includeDir.
	checkFile(t, filepath.Join(dir, "a.conf"), "N\n", 0o644, uid, gid)
	if got := dirNames(t, dir); !slices.Equal(got, []string{"a.conf"}) {
		t.Errorf("includeDir = %q, want only a.conf", got)
	}
}

// nginx-provider TC-ERR26: A symlinked includeDir is refused by the file primitives
// Owner WI 6.1. Requirements: FR-2a, ASM-11.
func TestIncludeDirSymlinkRefused(t *testing.T) {
	root := t.TempDir()
	target, link := filepath.Join(root, "real"), filepath.Join(root, "conf.d")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "a.conf"), []byte("P\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	before := lstat(t, target)

	f, err := openIncludeDir(Settings{IncludeDir: link})
	if err == nil {
		f.Close()
		t.Fatal("openIncludeDir through a symlink succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "symlink") ||
		!strings.Contains(msg, unix.ELOOP.Error()) && !strings.Contains(msg, unix.ENOTDIR.Error()) {
		t.Errorf("error %q, want ELOOP or ENOTDIR naming symlink", msg)
	}
	after := lstat(t, target)
	if after.Mtim != before.Mtim || after.Ctim != before.Ctim {
		t.Error("the symlink's target directory changed")
	}
	if got := dirNames(t, target); !slices.Equal(got, []string{"a.conf"}) {
		t.Errorf("target = %q, want a.conf only", got)
	}
	checkFile(t, filepath.Join(target, "a.conf"), "P\n", 0o644, os.Getuid(), os.Getgid())
}
