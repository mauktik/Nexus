// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package svcctl

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// SEC-2: the component walk, which is the whole of the check off Linux and
// the fallback where openat2(2) is missing or filtered.
func TestSEC2_WalkNoLinks(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "a", "b", "file"))
	if err := os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a", "b", "file"), filepath.Join(dir, "a", "filelink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "a", "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		wantErr    string // Substring; "" for success
		wantIs     error
	}{
		{"a file", filepath.Join(dir, "a", "b", "file"), "", nil},
		{"a missing file", filepath.Join(dir, "a", "missing"), "no such file", fs.ErrNotExist},
		{"a missing directory", filepath.Join(dir, "nope", "file"), "no such file", fs.ErrNotExist},
		{"a symbolic link directory", filepath.Join(dir, "link", "b", "file"), "a component of", syscall.ELOOP},
		{"a symbolic link file", filepath.Join(dir, "a", "filelink"), "a component of", syscall.ELOOP},
		{"a file as a directory", filepath.Join(dir, "a", "b", "file", "more"), "", syscall.ENOTDIR},
		{"a relative path", "a/b/file", "not an absolute path", nil},
		{"an unclean path", filepath.Join(dir, "a", "b") + "/../b/file", "not an absolute path", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd, err := walkNoLinks(tc.path)
			if err == nil {
				unix.Close(fd)
			}
			switch {
			case tc.wantErr == "" && tc.wantIs == nil:
				if err != nil {
					t.Fatalf("walkNoLinks(%s) = %v, want a descriptor", tc.path, err)
				}
			case err == nil:
				t.Fatalf("walkNoLinks(%s) succeeded, want an error", tc.path)
			case !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErr)):
				t.Fatalf("walkNoLinks(%s) = %v, want %q", tc.path, err, tc.wantErr)
			case tc.wantIs != nil && !errors.Is(err, tc.wantIs):
				t.Fatalf("walkNoLinks(%s) = %v, want it to wrap %v", tc.path, err, tc.wantIs)
			}
		})
	}
	t.Run("a FIFO is opened without blocking", func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			fd, err := walkNoLinks(filepath.Join(dir, "a", "fifo"))
			if err == nil {
				unix.Close(fd)
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("walkNoLinks blocked on a FIFO")
		}
	})
}

// SEC-5: the name is looked at again after the open. A name that was removed,
// replaced or given a second link no longer passes, whatever the link count
// of the descriptor says.
func TestSEC5_SoleName(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "out.log")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fd, err := openNoLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := soleName(fd, path); err != nil {
		t.Fatalf("soleName of a file with one name: %v", err)
	}
	other := filepath.Join(dir, "other.log")
	if err := os.Link(path, other); err != nil {
		t.Fatal(err)
	}
	if err := soleName(fd, path); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("soleName with a second link = %v, want a hard-link error", err)
	}
	// The original name is removed and replaced: the descriptor's file now
	// has one link, other.log, and the name is another file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := soleName(fd, path); err == nil || !strings.Contains(err.Error(), "no longer") {
		t.Fatalf("soleName with the name gone = %v, want a name error", err)
	}
	if err := os.WriteFile(path, []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := soleName(fd, path); err == nil || !strings.Contains(err.Error(), "no longer") {
		t.Fatalf("soleName with the name replaced = %v, want a name error", err)
	}
}

// SEC-5: a root-only file is not read from a directory that another user can
// write, whatever its link count says.
func TestSEC5_RootOnlyInUserDir(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		fileUID, fileMode, parentUID, parentMode uint32
		want                                     bool
	}{
		{"root-only file in the service user's directory", 0, 0o600, 1000, 0o755, true},
		{"root-only file in a root directory its group can write", 0, 0o600, 0, 0o775, true},
		{"root-only file in a root directory anyone can write", 0, 0o600, 0, 0o777, true},
		{"root-only file in a root directory only root can write", 0, 0o600, 0, 0o755, false},
		{"world-readable root file in the service user's directory", 0, 0o644, 1000, 0o755, false},
		{"the user's own file in its directory", 1000, 0o600, 1000, 0o755, false},
		{"the user's file in a root directory its group can write", 1000, 0o600, 0, 0o775, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unreadableInForeignDir(tc.fileUID, tc.fileMode, tc.parentUID, tc.parentMode); got != tc.want {
				t.Fatalf("unreadableInForeignDir = %v, want %v", got, tc.want)
			}
		})
	}
}

// SEC-7 (CWE-367, CWE-62): a log file that is not world-readable is refused
// when a user other than its owner and root can write the directory it is
// in, whoever owns it. Only the owner, root, or a directory only they can
// write leaves no one who could have planted a link to it.
func TestSEC7_UnreadableFileInForeignWritableDir(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		fileUID, fileMode, parentUID, parentMode uint32
		want                                     bool
	}{
		{"a file of uid 1000 in a directory of uid 2000", 1000, 0o600, 2000, 0o755, true},
		{"a file of uid 1000 in a directory of uid 2000 that is 0700", 1000, 0o600, 2000, 0o700, true},
		{"a file of uid 1000 in a directory its group can write", 1000, 0o600, 1000, 0o775, true},
		{"a file of uid 1000 in a directory anyone can write", 1000, 0o600, 1000, 0o777, true},
		{"a file of uid 1000 in a sticky world-writable root directory", 1000, 0o600, 0, 0o1777, true},
		{"a file of uid 1000 in a root directory its group can write", 1000, 0o600, 0, 0o775, true},
		{"a root file in a directory of uid 2000", 0, 0o600, 2000, 0o755, true},
		{"a root file in a root directory its group can write", 0, 0o600, 0, 0o775, true},
		{"a root file in a root directory anyone can write", 0, 0o600, 0, 0o777, true},
		// What supervise-daemon makes and what stays readable.
		{"a user's 0600 file in the user's own directory", 1000, 0o600, 1000, 0o755, false},
		{"a user's 0600 file in the user's own 0700 directory", 1000, 0o600, 1000, 0o700, false},
		{"a user's 0600 file in /var/log, which only root can write", 1000, 0o600, 0, 0o755, false},
		{"a root 0600 file in /var/log, which only root can write", 0, 0o600, 0, 0o755, false},
		{"a root 0600 file in a root 0700 directory", 0, 0o600, 0, 0o700, false},
		{"a world-readable file of uid 1000 in a directory of uid 2000", 1000, 0o644, 2000, 0o755, false},
		{"a world-readable file in a directory anyone can write", 1000, 0o644, 0, 0o777, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unreadableInForeignDir(tc.fileUID, tc.fileMode, tc.parentUID, tc.parentMode); got != tc.want {
				t.Fatalf("unreadableInForeignDir(file %d %o, dir %d %o) = %v, want %v", tc.fileUID, tc.fileMode, tc.parentUID, tc.parentMode, got, tc.want)
			}
		})
	}
}

// SEC-7 through soleName: a file of one user in another user's directory is
// refused even with a link count of 1. Needs root to give the file and the
// directory their owners.
func TestSEC7_SoleNameRefusesAnotherUsersFileInAUsersDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give a file and its directory different owners")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for d := root; d != "/"; d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o755); err != nil && !os.IsPermission(err) {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "svc")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "out.log")
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const fileUID, dirUID = 65533, 65534
	if err := os.Chown(dir, dirUID, dirUID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		fileUID  int
		wantFail bool
	}{
		{"a file of another user", fileUID, true},
		{"the directory owner's own file", dirUID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chown(path, tc.fileUID, tc.fileUID); err != nil {
				t.Fatal(err)
			}
			fd, err := walkNoLinks(path)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			err = soleName(fd, path)
			switch {
			case tc.wantFail && err == nil:
				t.Fatal("soleName accepted another user's unreadable file in a user's directory")
			case !tc.wantFail && err != nil:
				t.Fatalf("soleName refused the directory owner's own file: %v", err)
			}
		})
	}
}
