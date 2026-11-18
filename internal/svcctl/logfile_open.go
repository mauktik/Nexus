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
