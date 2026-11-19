package svcctl

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// walkNoLinks opens the absolute, clean path from "/" one component at a
// time, each with O_NOFOLLOW (the directories also O_DIRECTORY), so that no
// component can be a symbolic link, whatever another process does to the
// path while it walks. The file is opened O_NONBLOCK, so a FIFO never hangs
// it.
func walkNoLinks(path string) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return -1, fmt.Errorf("%s is not an absolute path of a file", path)
	}
	dir, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK
		}
		fd, err := unix.Openat(dir, part, flags, 0)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
				var st unix.Stat_t
				if unix.Fstatat(dir, part, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
					err = syscall.ELOOP
				}
			}
			unix.Close(dir)
			return -1, noLinkError(path, err)
		}
		unix.Close(dir)
		dir = fd
	}
	return dir, nil
}

// noLinkError says why path could not be opened without following a link:
// ELOOP is a symbolic link in it.
func noLinkError(path string, err error) error {
	if errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("a component of %s is a symbolic link, which a log file's path may not contain: %w", path, err)
	}
	return err
}

// soleName checks that path, looked up again in its parent directory without
// following a link, is still the file fd holds and has no other name: the
// same device and inode, and a link count of 1. The parent is walked with
// O_NOFOLLOW at every component, as walkNoLinks does, whichever way the
// file was opened, so this holds off Linux as well. A name that is gone, that
// is another file, or that is one of several links to the file is an error.
func soleName(fd int, path string) error {
	var self unix.Stat_t
	if err := unix.Fstat(fd, &self); err != nil {
		return err
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	var dfd int
	var err error
	if dir == "/" {
		dfd, err = unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	} else {
		dfd, err = walkNoLinks(dir)
	}
	if err != nil {
		return fmt.Errorf("the log file's directory cannot be checked again: %w", err)
	}
	defer unix.Close(dfd)
	var named unix.Stat_t
	if err := unix.Fstatat(dfd, base, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("the log file's name no longer refers to the file that was opened: %w", err)
	}
	if uint64(named.Dev) != uint64(self.Dev) || uint64(named.Ino) != uint64(self.Ino) {
		return errors.New("the log file's name no longer refers to the file that was opened")
	}
	var parent unix.Stat_t
	if err := unix.Fstat(dfd, &parent); err != nil {
		return err
	}
	if rootOnlyInUserDir(self.Uid, uint32(self.Mode), parent.Uid, uint32(parent.Mode)) {
		return errors.New("the log file is root's and not world-readable, in a directory that another user can write")
	}
	if uint64(named.Nlink) != 1 || uint64(self.Nlink) != 1 {
		return fmt.Errorf("the log file has %d hard links; it is read only when it has one", max(uint64(named.Nlink), uint64(self.Nlink)))
	}
	return nil
}

// rootOnlyInUserDir reports whether a file with this owner and mode is one that only root can read, in a
// directory that a user other than root can write: owned by another user, or
// writable by its group or by anyone. Such a file cannot have been made there
// by that user; it is a hard link to a root-only file (CWE-62). It is the
// check that does not depend on a link count, which a user can lower in the
// instant between the open and the check by removing its own link (SEC-5).
func rootOnlyInUserDir(fileUID, fileMode, parentUID, parentMode uint32) bool {
	userWritable := parentUID != 0 || parentMode&0o022 != 0
	return userWritable && fileUID == 0 && fileMode&0o004 == 0
}
