// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package vzdump

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
)

// swapOpener reports every Stat and Lstat as the regular file "decoy", the
// state a FIFO swapped in after the check leaves behind.
type swapOpener struct{ *os.Root }

func (s swapOpener) Stat(string) (fs.FileInfo, error)  { return s.Root.Stat("decoy") }
func (s swapOpener) Lstat(string) (fs.FileInfo, error) { return s.Root.Lstat("decoy") }

// B-SEC-2: a FIFO where a notes file or a log should be is refused, not
// waited on.
func TestSecB2_FIFONotesAndLogDoNotBlock(t *testing.T) {
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir, storagetest.Node{Path: "decoy", Content: []byte("x")})
	for _, n := range []string{"a.notes", "a.log"} {
		if err := syscall.Mkfifo(filepath.Join(dir, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := swapOpener{openRoot(t, dir)}
	for name, read := range map[string]func() error{
		"notes": func() error { _, _, err := Notes(root, "a.notes", 1024); return err },
		"log":   func() error { _, err := ReadLog(root, "a.log", 4096, 10, 512); return err },
	} {
		done := make(chan error, 1)
		go func() { done <- read() }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("%s: err = %v, want a not-a-regular-file error", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: blocked in open(2) on a FIFO", name)
		}
	}
}

// SEC-9: os.Root.OpenFile ignores O_NOFOLLOW, so the check is the open
// file against the entry's Lstat. A symlink inside the root to another
// regular file in the root is refused, not read.
func TestSec9_SymlinkToARegularFileIsRefused(t *testing.T) {
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir,
		storagetest.Node{Path: "target", Content: []byte("INFO: Finished Backup of VM 101\n")},
		storagetest.Node{Path: "a.notes", Type: storagetest.Symlink, Target: "target"},
		storagetest.Node{Path: "a.log", Type: storagetest.Symlink, Target: "target"})
	root := openRoot(t, dir)
	if _, _, err := Notes(root, "a.notes", 1024); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("notes: err = %v, want a not-a-regular-file error", err)
	}
	if _, err := ReadLog(root, "a.log", 4096, 10, 512); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("log: err = %v, want a not-a-regular-file error", err)
	}
}

// SEC-9: a symlink swapped in after the check (the opener reports the
// regular file "decoy") opens a different file, which is refused.
func TestSec9_SymlinkSwappedInAfterTheCheckIsRefused(t *testing.T) {
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir,
		storagetest.Node{Path: "decoy", Content: []byte("x")},
		storagetest.Node{Path: "target", Content: []byte("y")},
		storagetest.Node{Path: "a.notes", Type: storagetest.Symlink, Target: "target"},
		storagetest.Node{Path: "a.log", Type: storagetest.Symlink, Target: "target"})
	root := swapOpener{openRoot(t, dir)}
	if _, _, err := Notes(root, "a.notes", 1024); err == nil {
		t.Error("notes: a swapped-in symlink was read")
	}
	if _, err := ReadLog(root, "a.log", 4096, 10, 512); err == nil {
		t.Error("log: a swapped-in symlink was read")
	}
}

// dirSwapOpener replaces the directory "dump" with a FIFO just before the
// root opens it: the state after the listing saw a directory.
type dirSwapOpener struct {
	*os.Root
	dir string
}

func (s dirSwapOpener) swap(name string) error {
	if name != "dump" {
		return nil
	}
	p := filepath.Join(s.dir, "dump")
	if err := os.Remove(p); err != nil {
		return err
	}
	return syscall.Mkfifo(p, 0o600)
}

func (s dirSwapOpener) Open(name string) (*os.File, error) {
	if err := s.swap(name); err != nil {
		return nil, err
	}
	return s.Root.Open(name)
}

func (s dirSwapOpener) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	if err := s.swap(name); err != nil {
		return nil, err
	}
	return s.Root.OpenFile(name, flag, perm)
}

// B-SEC-2: a FIFO where the dump directory was listed is refused, not
// waited on.
func TestSecB2_FIFODumpDirectoryDoesNotBlock(t *testing.T) {
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir, storagetest.Node{Path: "dump", Type: storagetest.Dir})
	t.Cleanup(func() { // release a reader a failing run left blocked
		if f, err := os.OpenFile(filepath.Join(dir, "dump"), os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	done := make(chan DirResult, 1)
	go func() { done <- Scan(dirSwapOpener{openRoot(t, dir), dir}, dir, 100) }()
	select {
	case res := <-done:
		if res.Err == nil || !strings.Contains(res.Err.Error(), "dump") {
			t.Errorf("Err = %v, want a refusal naming dump", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Scan is blocked in open(2) on a FIFO directory")
	}
}
