// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package vzdump reads Proxmox vzdump backup directories (FR-9, DF-3): the
// name grammar, a bounded listing of a configured path and its dump/, the
// head of a notes file and the tail of a log. Every read goes through an
// Opener (an *os.Root the provider opened inside its bounded flight, D-15),
// so nothing outside the configured directory is ever opened (A-32).
package vzdump

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Archive is one vzdump archive, or an archive still being written
// (InProgress: a `.dat` file or a `.tmp` directory, A-12).
type Archive struct {
	// File is the entry's name; Dir is "." or "dump", the directory of
	// the configured path it was listed in.
	File, Dir, GuestType string
	VMID                 int
	// Format is vma or tar ("" for an in-progress item whose name does not
	// say); Compression is zst, gz, lzo or "".
	Format, Compression string
	// Timestamp is the time in the name, naive node-local time held as
	// UTC and never converted (A-13).
	Timestamp time.Time
	// ModifiedAt and Size are the file's (a symlink's target's, when it
	// resolves inside the root); a `.tmp` directory's Size is 0.
	ModifiedAt time.Time
	Size       int64
	Protected  bool
	InProgress bool
	// LogFile and NotesFile are companion names in the same directory, ""
	// when absent.
	LogFile, NotesFile string
}

// Opener is the subset of *os.Root the readers call; *os.Root satisfies it. A test
// wraps a real root to record opens or to stall one call (the D-15 tests).
//
// Scan opens every directory it lists, `.` included, with
// OpenFile(O_DIRECTORY|O_NONBLOCK), so a FIFO is refused without blocking, and resolves symlinks with Stat; Notes and ReadLog Lstat, open
// the file with OpenFile (never blocking) and check that the open file is
// the regular file Lstat saw, so a symlink is refused (os.Root.OpenFile
// ignores O_NOFOLLOW), B-SEC-2, SEC-9. A
// wrapper that also has WrapFile may wrap each file they read, to count the
// bytes and offsets read.
type Opener interface {
	Open(name string) (*os.File, error)
	OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error)
	Stat(name string) (fs.FileInfo, error)
	Lstat(name string) (fs.FileInfo, error)
	FS() fs.FS
}

// DirResult is one configured path's listing.
type DirResult struct {
	Path     string
	Archives []Archive
	// Unrecognized counts the entries that are neither an archive, an
	// in-progress item nor a companion of an archive in the same
	// directory: other files, directories, orphan companions, and symlinks
	// that do not resolve inside the root to a regular file.
	Unrecognized int
	// Truncated is set when `.` or `dump` held more than maxEntries
	// entries; only the first maxEntries read of each were classified.
	Truncated bool
	// Err names the path and why it could not be listed. When only dump/
	// failed, the archives of `.` are kept beside it.
	Err error
}

var errNotDir = errors.New("not a directory")

// Scan lists `.` and, when `dump` is a directory inside the root, `dump`,
// each up to maxEntries entries, and never opens anything deeper: other
// directories (including `.tmp` ones) are counted or reported, not read.
// path is the configured path, used to name the result and its errors.
func Scan(root Opener, path string, maxEntries int) DirResult {
	res := DirResult{Path: path}
	top, truncated, err := readDir(root, ".", maxEntries)
	if err != nil {
		res.Err = fmt.Errorf("%s: %w", path, err)
		return res
	}
	res.Truncated = truncated
	listDump := false
	for _, de := range top {
		if de.Name() == "dump" {
			listDump = isDirInside(root, de)
		}
	}
	skip := ""
	if listDump {
		skip = "dump"
	}
	res.classify(root, ".", top, skip)
	if listDump {
		ents, truncated, err := readDir(root, "dump", maxEntries)
		if err != nil {
			res.Err = fmt.Errorf("%s/dump: %w", strings.TrimSuffix(path, "/"), err)
		} else {
			res.Truncated = res.Truncated || truncated
			res.classify(root, "dump", ents, "")
		}
	}
	slices.SortFunc(res.Archives, func(a, b Archive) int {
		if c := strings.Compare(a.Dir, b.Dir); c != 0 {
			return c
		}
		return strings.Compare(a.File, b.File)
	})
	return res
}

// readDir reads at most limit entries of dir, reporting whether more were
// there. It reads limit+1 at most, never the whole directory.
func readDir(root Opener, dir string, limit int) ([]fs.DirEntry, bool, error) {
	limit = max(limit, 0)
	// O_DIRECTORY|O_NONBLOCK: a FIFO swapped in after the listing answers
	// ENOTDIR at once instead of blocking open(2) (B-SEC-2).
	f, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !fi.IsDir() {
		return nil, false, errNotDir
	}
	var out []fs.DirEntry
	for len(out) <= limit {
		des, err := f.ReadDir(limit + 1 - len(out))
		out = append(out, des...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, err
		}
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// isDirInside reports whether the entry `dump` is a directory, or a
// symlink that resolves inside the root to one; os.Root refuses a symlink
// that leaves the root, so an escaping `dump` is never listed.
func isDirInside(root Opener, de fs.DirEntry) bool {
	if de.IsDir() {
		return true
	}
	if de.Type()&fs.ModeSymlink == 0 {
		return false
	}
	fi, err := root.Stat(de.Name())
	return err == nil && fi.IsDir()
}

type vzFile struct {
	name string
	info fs.FileInfo
	n    vzName
}

// classify adds the entries of dir (one of "." and "dump") to res.
func (res *DirResult) classify(root Opener, dir string, des []fs.DirEntry, skip string) {
	var files []vzFile
	for _, de := range des {
		name := de.Name()
		if name == skip {
			continue
		}
		n := parseName(name)
		switch {
		case n.kind == kindTmpDir && de.IsDir():
			info, err := de.Info()
			if err != nil {
				res.Unrecognized++
				continue
			}
			res.Archives = append(res.Archives, item(dir, name, n, info, true))
		case n.kind == kindOther || n.kind == kindTmpDir:
			res.Unrecognized++
		case de.Type().IsRegular() || de.Type()&fs.ModeSymlink != 0:
			info, err := regularInfo(root, dir, de)
			if err != nil {
				res.Unrecognized++
				continue
			}
			files = append(files, vzFile{name: name, info: info, n: n})
		default:
			res.Unrecognized++
		}
	}

	// Archives and in-progress items first, then their companions.
	byArchive := map[string]int{}
	byStem := map[string][]int{}
	for _, f := range files {
		switch f.n.kind {
		case kindArchive:
			res.Archives = append(res.Archives, item(dir, f.name, f.n, f.info, false))
			i := len(res.Archives) - 1
			byArchive[f.name] = i
			byStem[f.n.stem] = append(byStem[f.n.stem], i)
		case kindPartial:
			res.Archives = append(res.Archives, item(dir, f.name, f.n, f.info, true))
		}
	}
	// <archive>.notes wins over <stem>.notes, so it is placed first.
	for _, f := range files {
		switch {
		case f.n.kind == kindNotes && f.n.archive != "":
			if i, ok := byArchive[f.n.archive]; ok {
				res.Archives[i].NotesFile = f.name
			} else {
				res.Unrecognized++
			}
		case f.n.kind == kindProtected:
			if i, ok := byArchive[f.n.archive]; ok {
				res.Archives[i].Protected = true
			} else {
				res.Unrecognized++
			}
		}
	}
	for _, f := range files {
		switch {
		case f.n.kind == kindLog:
			for _, i := range byStem[f.n.stem] {
				res.Archives[i].LogFile = f.name
			}
		case f.n.kind == kindNotes && f.n.archive == "":
			for _, i := range byStem[f.n.stem] {
				if res.Archives[i].NotesFile == "" {
					res.Archives[i].NotesFile = f.name
				}
			}
		default:
			continue
		}
		if len(byStem[f.n.stem]) == 0 {
			res.Unrecognized++
		}
	}
}

// regularInfo is a regular file's Lstat, or, for a symlink, its target's
// Stat through the root (which refuses a target outside it); anything but
// a regular file is an error.
func regularInfo(root Opener, dir string, de fs.DirEntry) (fs.FileInfo, error) {
	var info fs.FileInfo
	var err error
	if de.Type()&fs.ModeSymlink != 0 {
		info, err = root.Stat(join(dir, de.Name()))
	} else {
		info, err = de.Info()
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", join(dir, de.Name()))
	}
	return info, nil
}

func item(dir, name string, n vzName, info fs.FileInfo, inProgress bool) Archive {
	a := Archive{
		File: name, Dir: dir, GuestType: n.guestType, VMID: n.vmid,
		Format: n.format, Compression: n.compression, Timestamp: n.timestamp,
		ModifiedAt: info.ModTime(), InProgress: inProgress,
	}
	if info.Mode().IsRegular() {
		a.Size = info.Size()
	}
	return a
}

func join(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}
