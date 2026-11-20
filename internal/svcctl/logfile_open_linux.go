//go:build linux

package svcctl

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// noOpenat2 makes openNoLinks use the component walk only, for the test of
// the fallback.
var noOpenat2 bool

// openNoLinks opens the absolute, clean path for reading without blocking and
// without following a symbolic link in any component (CWE-59): openat2(2)
// with RESOLVE_NO_SYMLINKS where the kernel has it (Linux 5.6), the
// component walk where it does not, or where a filter such as a container's
// seccomp profile refuses it (ENOSYS, EPERM; EINVAL is a kernel that does not
// know the flag). The walk is as strict, so the fallback weakens nothing.
func openNoLinks(path string) (int, error) {
	if !noOpenat2 {
		fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
			Flags:   unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_NO_SYMLINKS,
		})
		switch {
		case err == nil:
			return fd, nil
		case !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EINVAL):
			return -1, noLinkError(path, err)
		}
	}
	return walkNoLinks(path)
}
