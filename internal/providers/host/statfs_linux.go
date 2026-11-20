// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package host

import (
	"errors"
	"io/fs"
	"syscall"
)

// The syscalls of the seam, as variables so the linux test can count them.
var (
	sysStat   = syscall.Stat
	sysStatfs = syscall.Statfs
)

// defaultStatfs is the Statfs seam: stat(2) of the mount point, then, only
// for a directory, statfs(2). Both syscalls run in the one call, so the
// caller's bounded covers the stat too: on a hung NFS server the stat
// blocks as the statfs would (review Finding 2). Neither opens or reads the
// path (D-3). Blocks, Bfree and Bavail count blocks of Frsize bytes
// (statfs(2) f_frsize, "Fragment size"): the captures' df size equals
// stat -f %b × %S (testdata/ubuntu-systemd/tools/df and statfs.txt).
func defaultStatfs(path string) (Usage, error) {
	var st syscall.Stat_t
	if err := retryEINTR(func() error { return sysStat(path, &st) }); err != nil {
		return Usage{}, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return Usage{IsDir: false}, nil
	}
	var sf syscall.Statfs_t
	if err := retryEINTR(func() error { return sysStatfs(path, &sf) }); err != nil {
		return Usage{}, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	// The field types of Statfs_t differ between architectures (Frsize is
	// int64 on amd64 and arm64, int32 on 386 and arm), hence the
	// conversions; the kernel never reports a negative size.
	return Usage{
		IsDir:  true,
		Blocks: uint64(sf.Blocks),
		Bfree:  uint64(sf.Bfree),
		Bavail: uint64(sf.Bavail),
		Files:  uint64(sf.Files),
		Ffree:  uint64(sf.Ffree),
		Frsize: uint64(sf.Frsize),
	}, nil
}

// retryEINTR repeats a syscall interrupted by a signal, as os.Stat does.
func retryEINTR(call func() error) error {
	for {
		if err := call(); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
