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
	if uint64(named.Nlink) != 1 || uint64(self.Nlink) != 1 {
		return fmt.Errorf("the log file has %d hard links; it is read only when it has one", max(uint64(named.Nlink), uint64(self.Nlink)))
	}
	return nil
}
