package nginx

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// renameat2 is unix.Renameat2. Only tests replace it, to force the
// fallback that a kernel or file system without RENAME_NOREPLACE takes.
var renameat2 = unix.Renameat2

// RenameNoReplace renames from to to in dirfd, and never replaces an
// existing to: that fails with EEXIST and changes nothing. The entry keeps
// its inode, so its bytes, mode and owner are unchanged. It uses
// renameat2(RENAME_NOREPLACE). Where that flag is not supported (EINVAL
// from the file system, ENOSYS from the kernel) it links to and then
// unlinks from, as linkat also refuses an existing to. The directory is
// fsynced afterwards.
func RenameNoReplace(dirfd int, from, to string) error {
	if err := checkBaseName(from); err != nil {
		return err
	}
	if err := checkBaseName(to); err != nil {
		return err
	}
	err := ignoringEINTR(func() error { return renameat2(dirfd, from, dirfd, to, unix.RENAME_NOREPLACE) })
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		err = linkThenUnlink(dirfd, from, to)
	}
	if err != nil {
		return fmt.Errorf("rename %s to %s: %w", from, to, err)
	}
	return syncDirFd(dirfd, to)
}

// linkThenUnlink is the RENAME_NOREPLACE fallback. linkat with no flags
// links from itself, not what a symlink there points to. If from cannot
// be unlinked, the new link is removed again, so the directory is left
// with from only, as before.
func linkThenUnlink(dirfd int, from, to string) error {
	if err := ignoringEINTR(func() error { return unix.Linkat(dirfd, from, dirfd, to, 0) }); err != nil {
		return err
	}
	if err := unlinkAt(dirfd, from); err != nil {
		if uerr := unlinkAt(dirfd, to); uerr != nil {
			return errors.Join(err, fmt.Errorf("both %s and %s now exist: remove %s: %w", from, to, to, uerr))
		}
		return err
	}
	return nil
}
