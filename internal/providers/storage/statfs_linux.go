// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package storage

import (
	"errors"
	"io/fs"
	"syscall"
)

// defaultStatfs is the Statfs seam: statfs(2) of a configured Time Machine
// share. It never opens or reads the path. The caller runs it under bounded
// keyed "statfs <path>" (D-15), because on a suspended pool the kernel holds
// it as it holds zpool. Blocks, Bfree and Bavail count blocks of f_frsize
// bytes (statfs(2) "Fragment size"), reported as Bsize.
func defaultStatfs(path string) (Usage, error) {
	var sf syscall.Statfs_t
	for {
		err := syscall.Statfs(path, &sf)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EINTR) {
			return Usage{}, &fs.PathError{Op: "statfs", Path: path, Err: err}
		}
	}
	// The field types of Statfs_t differ between architectures (Frsize is
	// int64 on amd64 and arm64, int32 on 386 and arm), hence the
	// conversions; the kernel never reports a negative size.
	return Usage{
		Blocks: uint64(sf.Blocks),
		Bfree:  uint64(sf.Bfree),
		Bavail: uint64(sf.Bavail),
		Bsize:  uint64(sf.Frsize),
	}, nil
}
