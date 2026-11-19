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
