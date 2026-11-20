// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package timemachine reads Time Machine shares (FR-11, FR-12, DF-4): the
// bundle directories directly under a share, each bundle's snapshot
// history plist and fallback mtimes, and the allocated size of its bands.
// Every read goes through an Opener (an *os.Root the provider opened inside
// its bounded flight, D-15), so nothing outside the configured share is
// ever opened (CON-7). The only file whose contents are ever read is
// com.apple.TimeMachine.SnapshotHistory.plist; MachineID.plist, lock and
// band files are at most Lstat'ed or listed (NFR-5).
package timemachine

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/providers/storage/spec"
)

// Bundle suffixes (A-14): Samba vfs_fruit shares hold `<name>.sparsebundle`
// directories; `.backupbundle` is the suffix some macOS releases use.
const (
	sparseSuffix = ".sparsebundle"
	backupSuffix = ".backupbundle"
)

// File names inside a bundle.
const (
	// SnapshotHistoryFile is the one file of a bundle whose contents are read.
	SnapshotHistoryFile = "com.apple.TimeMachine.SnapshotHistory.plist"
	resultsFile         = "com.apple.TimeMachine.Results.plist"
	infoFile            = "Info.plist"
)

// MaxPlistBytes bounds the SnapshotHistory.plist read (NFR-3): a larger
// file is not parsed.
const MaxPlistBytes = 1 << 20

// Reasons a Bundle carries.
const (
	// ReasonNoHistory: the bundle holds no SnapshotHistory.plist.
	ReasonNoHistory = "no " + SnapshotHistoryFile
	// ReasonInProgress is why a bundle's inProgress is null (A-16): a
	// sparsebundle keeps its zero-byte lock file whether or not a backup
	// runs, so nothing on the share marks a backup in progress.
	ReasonInProgress = "not determinable from the share"
)

// Opener is the subset of *os.Root the readers call; *os.Root satisfies it. A test
// wraps a real root to record opens or to stall one call (the D-15 tests).
//
// Bundles lists the share with Open("."); ReadBundle Lstats its named files
// and reads the history plist through FS; WalkBands Lstats bands/ and reads
// each directory under it with Open and (*os.File).ReadDir(n). Open must
// return a file opened in the root, as *os.Root.Open does: only then is a
// DirEntry's Info the entry's lstat taken in the root.
type Opener interface {
	Open(name string) (*os.File, error)
	OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error)
	Stat(name string) (fs.FileInfo, error)
	Lstat(name string) (fs.FileInfo, error)
	FS() fs.FS
}

// Bundle is what one bundle's read gives: the latest completion date of
// its snapshot history (or nil with the reason) and, as a best-effort
// fallback, the newest mtime of the history plist, Results.plist and
// Info.plist (nil when none of them is a regular file). It holds no
// in-progress marker (A-16).
type Bundle struct {
	// Name is the bundle directory's name without its suffix, usually the
	// Mac's computer name; untrusted.
	Name             string
	LastBackupAt     *time.Time
	LastBackupReason string
	LastModifiedAt   *time.Time
}

// InProgress is always nil with ReasonInProgress (A-16): the share shows
// no file that exists only while a backup runs.
func (Bundle) InProgress() (*bool, string) { return nil, ReasonInProgress }

// Name is a bundle directory name without its .sparsebundle or
// .backupbundle suffix.
func Name(dir string) string {
	if n, ok := strings.CutSuffix(dir, sparseSuffix); ok {
		return n
	}
	return strings.TrimSuffix(dir, backupSuffix)
}

func isBundleName(name string) bool {
	return Name(name) != name && Name(name) != ""
}

// Bundles lists the bundle directories directly under the share (A-14):
// entries that are directories (a symlink is not followed) named
// `<name>.sparsebundle` or `<name>.backupbundle`, sorted, at most limit of
// them. It reads at most spec.MaxDirEntries entries of the share; truncated
// is set when more bundles were there, or when the listing stopped at that
// bound. It returns directory names, suffix kept, for ReadBundle and
// WalkBands; Name gives the bundle's name.
func Bundles(root Opener, limit int) ([]string, bool, error) {
	limit = max(limit, 0)
	f, err := root.Open(".")
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !fi.IsDir() {
		return nil, false, errors.New("not a directory")
	}
	var out []string
	read, truncated := 0, false
	for {
		des, err := f.ReadDir(min(256, spec.MaxDirEntries-read))
		for _, de := range des {
			if de.IsDir() && isBundleName(de.Name()) {
				out = append(out, de.Name())
			}
		}
		read += len(des)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, err
		}
		if len(out) > limit {
			truncated = true
			break
		}
		if read >= spec.MaxDirEntries {
			// The bound is reached; the listing is cut only if more
			// entries follow.
			more, err := f.ReadDir(1)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, false, err
			}
			truncated = len(more) > 0
			break
		}
	}
	slices.Sort(out)
	if len(out) > limit {
		out, truncated = out[:limit], true
	}
	return out, truncated, nil
}

// ReadBundle reads the bundle directory dir: its SnapshotHistory.plist
// (at most MaxPlistBytes, parsed by LastCompletion) and the mtimes of that
// plist, Results.plist and Info.plist. Those three are only Lstat'ed, and
// only the history plist is opened; lock, MachineID.plist and bands/ are
// never touched.
func ReadBundle(root Opener, dir string) Bundle {
	b := Bundle{Name: Name(dir)}
	for _, name := range []string{SnapshotHistoryFile, resultsFile, infoFile} {
		fi, err := root.Lstat(path.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if mt := fi.ModTime().UTC(); b.LastModifiedAt == nil || mt.After(*b.LastModifiedAt) {
			b.LastModifiedAt = &mt
		}
	}
	plist, reason := readHistory(root, path.Join(dir, SnapshotHistoryFile))
	if reason != "" {
		b.LastBackupReason = reason
		return b
	}
	b.LastBackupAt, b.LastBackupReason = LastCompletion(plist)
	return b
}

// readHistory returns the history plist's bytes, or the reason it was not
// read. It opens the file only when Lstat shows a regular file of at most
// MaxPlistBytes, checks that the open file is the file Lstat saw, and reads at most one byte
// past the bound.
func readHistory(root Opener, name string) ([]byte, string) {
	tooLarge := fmt.Sprintf("%s is larger than %d bytes (1 MiB); not read", SnapshotHistoryFile, MaxPlistBytes)
	fi, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ReasonNoHistory
	case err != nil:
		return nil, fmt.Sprintf("%s: %v", SnapshotHistoryFile, err)
	case !fi.Mode().IsRegular():
		return nil, SnapshotHistoryFile + " is not a regular file"
	case fi.Size() > MaxPlistBytes:
		return nil, tooLarge
	}
	// O_NONBLOCK: a FIFO swapped in after the Lstat does not block open(2).
	// os.Root.OpenFile ignores O_NOFOLLOW, so a swapped-in symlink to a file
	// in the root is caught by os.SameFile below. The open file is what is
	// judged (B-SEC-2, SEC-9).
	of, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", SnapshotHistoryFile, err)
	}
	defer of.Close()
	if ofi, err := of.Stat(); err != nil || !ofi.Mode().IsRegular() || !os.SameFile(fi, ofi) {
		return nil, SnapshotHistoryFile + " is not a regular file"
	}
	var f fs.File = of
	if w, ok := root.(interface {
		WrapFile(name string, f *os.File) fs.File
	}); ok {
		f = w.WrapFile(name, of)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxPlistBytes+1))
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", SnapshotHistoryFile, err)
	}
	if len(b) > MaxPlistBytes {
		return nil, tooLarge
	}
	return b, ""
}
