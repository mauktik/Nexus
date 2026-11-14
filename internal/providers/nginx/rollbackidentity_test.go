//go:build linux

// Code review B, CR-B3: a rollback undoes only what the operation did. A
// create must not replace an entry that appeared after the re-check, and
// the rollback of a create or of an overwrite must act on the inode the
// write made, never on whatever is at the name by then.

package nginx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

const intruderSite = "server {\n    listen 8299;\n}\n"

// replaceEntry makes name in dir a new file holding content, as a package
// manager or an operator replacing it would: a new inode renamed over it.
func replaceEntry(t *testing.T, dir, name, content string) {
	t.Helper()
	tmp := filepath.Join(dir, ".intruder")
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Error(err)
	}
}

// nginx-provider CR-B3 A: the rollback of a created file leaves an entry that replaced it
func TestCreateRollbackLeavesAReplacedEntry(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"s.conf": validSite})
	w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
	// After nginx -t read the created n.conf, something else replaces it.
	w.set(func(h *lcHarness) { h.afterTest = func() { replaceEntry(t, w.f.dir, "n.conf", intruderSite) } })

	o := w.write(spec.Add.Op(), spec.AddInput{Name: "n", Content: invalidSite})
	d := o.failed(t, problem.OutcomeUnknown)
	if !strings.Contains(d, "n.conf") {
		t.Errorf("detail %q does not name n.conf", d)
	}
	if got := readString(t, filepath.Join(w.f.dir, "n.conf")); got != intruderSite {
		t.Errorf("n.conf holds %q, want the entry that replaced it, %q: a rollback deleted a file it did not create",
			got, intruderSite)
	}
	if got, want := withoutStatus(w.calls()), []string{"nginx -t"}; !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q: no reload", got, want)
	}
}

// nginx-provider CR-B3 B: the rollback of an overwrite leaves an entry that replaced the written file
func TestOverwriteRollbackLeavesAReplacedEntry(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
	w.set(func(h *lcHarness) { h.afterTest = func() { replaceEntry(t, w.f.dir, "a.conf", intruderSite) } })

	o := w.write(spec.Update.Op(), spec.WriteInput{Name: "a", Content: invalidSite})
	d := o.failed(t, problem.OutcomeUnknown)
	if prev := filepath.Join(o.dir, "previous", "content"); !strings.Contains(d, prev) {
		t.Errorf("detail %q does not name the previous content at %s", d, prev)
	}
	if got := readString(t, filepath.Join(w.f.dir, "a.conf")); got != intruderSite {
		t.Errorf("a.conf holds %q, want the entry that replaced it, %q: the restore overwrote a file it did not write",
			got, intruderSite)
	}
}

// nginx-provider CR-B3 C: a create never replaces an entry that appeared at the name
func TestCreateNeverReplacesAnEntry(t *testing.T) {
	uid, gid := ownerIDs()
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		writeOwned(t, filepath.Join(dir, "n.conf"), "THEIRS\n", 0o640, uid, gid)
		ino := lstat(t, filepath.Join(dir, "n.conf")).Ino
		dirfd := testIncludeDir(t, dir)

		id, err := WriteAtomic(dirfd, "n.conf", []byte("OURS\n"), 0o644, uid, gid, ".n.nexus-01J.tmp", true)
		if !errors.Is(err, ErrAppeared) || !errors.Is(err, unix.EEXIST) || !id.IsZero() {
			t.Fatalf("WriteAtomic = %+v, %v, want ErrAppeared (EEXIST) and no identity", id, err)
		}
		checkFile(t, filepath.Join(dir, "n.conf"), "THEIRS\n", 0o640, uid, gid)
		if lstat(t, filepath.Join(dir, "n.conf")).Ino != ino {
			t.Error("the entry that appeared was replaced")
		}
		if got := dirNames(t, dir); !slices.Equal(got, []string{"n.conf"}) {
			t.Errorf("includeDir = %q, want only n.conf: our temp file is removed", got)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		dir, outside := filepath.Join(root, "conf.d"), filepath.Join(root, "outside")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeOwned(t, outside, "X\n", 0o644, uid, gid)
		if err := os.Symlink(outside, filepath.Join(dir, "n.conf")); err != nil {
			t.Fatal(err)
		}
		dirfd := testIncludeDir(t, dir)

		if _, err := WriteAtomic(dirfd, "n.conf", []byte("OURS\n"), 0o644, uid, gid, ".n.nexus-01J.tmp", true); !errors.Is(err, ErrAppeared) {
			t.Fatalf("WriteAtomic = %v, want ErrAppeared", err)
		}
		if l, err := os.Readlink(filepath.Join(dir, "n.conf")); err != nil || l != outside {
			t.Errorf("n.conf reads %q, %v, want the symlink to %s kept", l, err, outside)
		}
		checkFile(t, outside, "X\n", 0o644, uid, gid)
	})
	t.Run("absent", func(t *testing.T) {
		dir := t.TempDir()
		dirfd := testIncludeDir(t, dir)
		id, err := WriteAtomic(dirfd, "n.conf", []byte("OURS\n"), 0o644, uid, gid, ".n.nexus-01J.tmp", true)
		if err != nil {
			t.Fatal(err)
		}
		st := lstat(t, filepath.Join(dir, "n.conf"))
		if id != (Identity{Dev: uint64(st.Dev), Ino: st.Ino}) {
			t.Errorf("identity %+v, want the created file's (dev %d, inode %d)", id, st.Dev, st.Ino)
		}
		checkFile(t, filepath.Join(dir, "n.conf"), "OURS\n", 0o644, uid, gid)
	})
}

// nginx-provider CR-B3 D: Restore acts only on the inode the write made
func TestRestoreActsOnlyOnTheWrittenInode(t *testing.T) {
	uid, gid := ownerIDs()
	swap := func(t *testing.T, dir, name, content string) {
		t.Helper()
		writeOwned(t, filepath.Join(dir, ".swap"), content, 0o644, uid, gid)
		if err := os.Rename(filepath.Join(dir, ".swap"), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("created file", func(t *testing.T) {
		dir, opDir := t.TempDir(), t.TempDir()
		dirfd := testIncludeDir(t, dir)
		m, err := Backup(dirfd, "n.conf", opDir, testMaxConfigBytes)
		if err != nil {
			t.Fatal(err)
		}
		wrote, err := WriteAtomic(dirfd, "n.conf", []byte("OURS\n"), 0o644, uid, gid, ".n.nexus-01J.tmp", true)
		if err != nil {
			t.Fatal(err)
		}
		swap(t, dir, "n.conf", "THEIRS\n")

		err = Restore(dirfd, m, wrote, opDir, ".n.nexus-01J.tmp")
		if err == nil || !strings.Contains(err.Error(), "not the file this operation wrote") {
			t.Fatalf("Restore = %v, want a refusal naming that it is not the file this operation wrote", err)
		}
		checkFile(t, filepath.Join(dir, "n.conf"), "THEIRS\n", 0o644, uid, gid)
	})
	t.Run("an identity that is not known", func(t *testing.T) {
		dir, opDir := t.TempDir(), t.TempDir()
		dirfd := testIncludeDir(t, dir)
		m, err := Backup(dirfd, "n.conf", opDir, testMaxConfigBytes)
		if err != nil {
			t.Fatal(err)
		}
		writeOwned(t, filepath.Join(dir, "n.conf"), "THEIRS\n", 0o644, uid, gid)

		if err := Restore(dirfd, m, Identity{}, opDir, ".n.nexus-01J.tmp"); err == nil {
			t.Fatal("Restore with no identity removed an entry it cannot show it wrote")
		}
		checkFile(t, filepath.Join(dir, "n.conf"), "THEIRS\n", 0o644, uid, gid)
	})
	t.Run("created file already gone", func(t *testing.T) {
		dir, opDir := t.TempDir(), t.TempDir()
		dirfd := testIncludeDir(t, dir)
		m, err := Backup(dirfd, "n.conf", opDir, testMaxConfigBytes)
		if err != nil {
			t.Fatal(err)
		}
		wrote, err := WriteAtomic(dirfd, "n.conf", []byte("OURS\n"), 0o644, uid, gid, ".n.nexus-01J.tmp", true)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "n.conf")); err != nil {
			t.Fatal(err)
		}
		if err := Restore(dirfd, m, wrote, opDir, ".n.nexus-01J.tmp"); err != nil {
			t.Errorf("Restore of a file that is already gone = %v, want nil", err)
		}
	})
	t.Run("overwritten file", func(t *testing.T) {
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
		swap(t, dir, "a.conf", "THEIRS\n")

		if err := Restore(dirfd, m, wrote, opDir, tc90Temp); err == nil {
			t.Fatal("Restore overwrote an entry that is not the file this operation wrote")
		}
		checkFile(t, filepath.Join(dir, "a.conf"), "THEIRS\n", 0o644, uid, gid)
	})
}
