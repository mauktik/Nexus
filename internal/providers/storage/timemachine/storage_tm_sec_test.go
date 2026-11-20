// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package timemachine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
)

// hostilePlist is a plist of exactly MaxPlistBytes whose one unclosed
// element name fills the file: encoding/xml quotes the name in its error.
func hostilePlist() []byte {
	const head, tail = "<plist><", "></plist>"
	return []byte(head + strings.Repeat("A", MaxPlistBytes-len(head)-len(tail)) + tail)
}

// B-SEC-1: a parse error never copies the share's text into the reason.
func TestSecB1_PlistParseReasonIsBounded(t *testing.T) {
	plist := hostilePlist()
	if len(plist) != MaxPlistBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(plist), MaxPlistBytes)
	}
	for name, doc := range map[string][]byte{
		"unclosed element":   plist,
		"bad entity":         []byte("<plist><key>x</key><string>&" + strings.Repeat("B", 1<<19) + ";</string></plist>"),
		"control characters": []byte("<plist><\x1b[31m\x07" + strings.Repeat("C", 1000) + "></plist>"),
	} {
		_, reason := LastCompletion(doc)
		if reason == "" {
			t.Errorf("%s: no reason", name)
		}
		if len(reason) > 256 {
			t.Errorf("%s: the reason is %d bytes, want at most 256", name, len(reason))
		}
		if strings.Contains(reason, "AAAA") || strings.Contains(reason, "BBBB") || strings.Contains(reason, "CCCC") {
			t.Errorf("%s: the reason holds the file's text: %.80q", name, reason)
		}
		for _, r := range reason {
			if r < ' ' || r == 0x7f {
				t.Errorf("%s: the reason holds control character %U", name, r)
			}
		}
	}
}

// swapOpener reports every Stat and Lstat as the regular file "decoy", the
// state a FIFO swapped in after the check leaves behind: the check passed,
// the open meets the real entry.
type swapOpener struct{ *os.Root }

func (s swapOpener) Stat(string) (fs.FileInfo, error)  { return s.Root.Stat("decoy") }
func (s swapOpener) Lstat(string) (fs.FileInfo, error) { return s.Root.Lstat("decoy") }

// B-SEC-2: a FIFO where the history plist should be is refused, not waited on.
func TestSecB2_FIFOHistoryDoesNotBlock(t *testing.T) {
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir, storagetest.Node{Path: "decoy", Content: []byte("x")},
		storagetest.Node{Path: "m.sparsebundle", Type: storagetest.Dir})
	if err := syscall.Mkfifo(filepath.Join(dir, "m.sparsebundle", SnapshotHistoryFile), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan Bundle, 1)
	go func() { done <- ReadBundle(swapOpener{openRoot(t, dir)}, "m.sparsebundle") }()
	select {
	case b := <-done:
		if b.LastBackupAt != nil || !strings.Contains(b.LastBackupReason, "not a regular file") {
			t.Errorf("got %+v, want no date and a not-a-regular-file reason", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadBundle is blocked in open(2) on a FIFO")
	}
}

// SEC-9: os.Root.OpenFile ignores O_NOFOLLOW, so the check is the open
// file against the entry's Lstat. A symlink swapped in after the check
// opens a different regular file, which is refused.
func TestSec9_SymlinkSwappedInAfterTheCheckIsRefused(t *testing.T) {
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir,
		storagetest.Node{Path: "decoy", Content: []byte("x")},
		storagetest.Node{Path: "target", Content: []byte("<plist></plist>")},
		storagetest.Node{Path: "m.sparsebundle", Type: storagetest.Dir},
		storagetest.Node{Path: "m.sparsebundle/" + SnapshotHistoryFile, Type: storagetest.Symlink, Target: "../target"})
	b := ReadBundle(swapOpener{openRoot(t, dir)}, "m.sparsebundle")
	if b.LastBackupAt != nil || !strings.Contains(b.LastBackupReason, "not a regular file") {
		t.Errorf("got %+v, want no date and a not-a-regular-file reason", b)
	}
}

// SEC-9: a symlink in the root to another regular file is refused.
func TestSec9_SymlinkToARegularFileIsRefused(t *testing.T) {
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir,
		storagetest.Node{Path: "target", Content: []byte("<plist></plist>")},
		storagetest.Node{Path: "m.sparsebundle", Type: storagetest.Dir},
		storagetest.Node{Path: "m.sparsebundle/" + SnapshotHistoryFile, Type: storagetest.Symlink, Target: "../target"})
	b := ReadBundle(openRoot(t, dir), "m.sparsebundle")
	if b.LastBackupAt != nil || !strings.Contains(b.LastBackupReason, "not a regular file") {
		t.Errorf("got %+v, want no date and a not-a-regular-file reason", b)
	}
}

// bandsSwapOpener replaces the directory "m.sparsebundle/bands" with a FIFO
// just before the root opens it: the state after Lstat saw a directory.
type bandsSwapOpener struct {
	*os.Root
	dir string
}

func (s bandsSwapOpener) swap(name string) error {
	if name != "m.sparsebundle/bands" {
		return nil
	}
	p := filepath.Join(s.dir, name)
	if err := os.Remove(p); err != nil {
		return err
	}
	return syscall.Mkfifo(p, 0o600)
}

func (s bandsSwapOpener) Open(name string) (*os.File, error) {
	if err := s.swap(name); err != nil {
		return nil, err
	}
	return s.Root.Open(name)
}

func (s bandsSwapOpener) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	if err := s.swap(name); err != nil {
		return nil, err
	}
	return s.Root.OpenFile(name, flag, perm)
}

// B-SEC-2: a FIFO where bands/ was checked is refused, not waited on.
func TestSecB2_FIFOBandsDirectoryDoesNotBlock(t *testing.T) {
	dir := storagetest.TempDir(t)
	storagetest.Build(t, dir, storagetest.Node{Path: "m.sparsebundle/bands", Type: storagetest.Dir})
	t.Cleanup(func() { // release a reader a failing run left blocked
		if f, err := os.OpenFile(filepath.Join(dir, "m.sparsebundle/bands"), os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := WalkBands(context.Background(), bandsSwapOpener{openRoot(t, dir), dir}, "m.sparsebundle", 100, time.Now().Add(time.Minute))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("WalkBands read a FIFO as a directory")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WalkBands is blocked in open(2) on a FIFO directory")
	}
}
