// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package timemachine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
)

// Size is a bundle's allocated size: st_blocks × 512 summed over its bands/
// directory and every entry under it (as `du -s --block-size=1` counts,
// A-17), the number of entries under bands/, and whether the walk stopped
// at its entry or time bound before the end.
type Size struct {
	Bytes      uint64
	Entries    int
	Truncated  bool
	ComputedAt time.Time
}

// errNoBlocks: the platform's FileInfo carries no syscall.Stat_t, so the
// allocated size cannot be read.
var errNoBlocks = errors.New("allocated blocks are not available on this platform")

// walkBatch is how many directory entries WalkBands reads at a time: the
// entry, time and ctx bounds are checked between batches, so a huge bands/
// directory is never read, statted and sorted whole before they can act.
const walkBatch = 256

// WalkBands sums the allocated size of <bundle>/bands: bands itself and every
// entry under it, depth first by directory, reading each directory in batches
// of walkBatch entries with (*os.File).ReadDir(n). Each directory is opened
// in the root with O_DIRECTORY|O_NONBLOCK, so a FIFO swapped in is refused
// at once, and its DirEntry.Info is already the entry's lstat taken in the
// root, so no entry is statted twice and a regular file is never opened. A
// symlink entry is never walked; a directory replaced by an in-root symlink
// after the listing is followed, inside the root and within the entry and
// time bounds (CON-7, NFR-5). It counts at most
// maxEntries entries and stops, with Truncated, at that bound or once
// deadline has passed; it returns ctx's error, with the partial size, when
// ctx ends between entries or batches. bundle must be one directory name;
// bands must be a directory (a symlink named bands is refused). It starts no
// goroutine. The order of the walk is the directory's own, not sorted: a sum
// needs none.
func WalkBands(ctx context.Context, root Opener, bundle string, maxEntries int, deadline time.Time) (Size, error) {
	var size Size
	if bundle == "" || bundle == "." || bundle == ".." || strings.ContainsRune(bundle, '/') {
		return size, fmt.Errorf("bundle %q is not a directory name", bundle)
	}
	if err := ctx.Err(); err != nil {
		return size, err
	}
	bands := path.Join(bundle, "bands")
	fi, err := root.Lstat(bands)
	if err != nil {
		return size, err
	}
	if !fi.IsDir() {
		return size, fmt.Errorf("%s: not a directory", bands)
	}
	maxEntries = max(maxEntries, 0)
	blocks, err := allocatedBytes(fi)
	if err != nil {
		return size, err
	}
	size.Bytes += blocks
	// pending holds the directories still to read; every one is an entry
	// already counted, so it is bounded by maxEntries.
	pending := []string{bands}
	for len(pending) > 0 && err == nil && !size.Truncated {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		pending, err = walkDir(ctx, root, dir, &size, maxEntries, deadline, pending)
	}
	size.ComputedAt = time.Now().UTC()
	return size, err
}

// walkDir reads one directory in batches, adds each entry to size and appends
// its subdirectories to pending. It stops without error, with size.Truncated,
// at the entry or time bound.
func walkDir(ctx context.Context, root Opener, dir string, size *Size, maxEntries int, deadline time.Time, pending []string) ([]string, error) {
	// O_DIRECTORY|O_NONBLOCK: a FIFO swapped in after the Lstat answers
	// ENOTDIR at once instead of blocking open(2) (B-SEC-2).
	f, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return pending, err
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return pending, err
		}
		if !time.Now().Before(deadline) {
			size.Truncated = true
			return pending, nil
		}
		des, err := f.ReadDir(walkBatch)
		for _, d := range des {
			if err := ctx.Err(); err != nil {
				return pending, err
			}
			if !time.Now().Before(deadline) {
				size.Truncated = true
				return pending, nil
			}
			if size.Entries >= maxEntries {
				size.Truncated = true
				return pending, nil
			}
			size.Entries++
			info, ierr := d.Info()
			if ierr != nil {
				return pending, ierr
			}
			blocks, berr := allocatedBytes(info)
			if berr != nil {
				return pending, berr
			}
			size.Bytes += blocks
			if d.IsDir() {
				pending = append(pending, path.Join(dir, d.Name()))
			}
		}
		if errors.Is(err, io.EOF) {
			return pending, nil
		}
		if err != nil {
			return pending, err
		}
	}
}

// allocatedBytes is the st_blocks × 512 of an lstat result.
func allocatedBytes(info fs.FileInfo) (uint64, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errNoBlocks
	}
	return uint64(st.Blocks) * 512, nil
}
