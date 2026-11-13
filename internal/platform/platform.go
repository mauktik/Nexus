// Package platform identifies the host's init system and operating system, so
// the daemon can choose a service controller and a detach backend (FR-18).
// Detection is by init system, not by distribution (ASM-16). It depends on
// nothing beyond the standard library.
package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
)

// InitSystem is the host's service manager.
type InitSystem string

const (
	Systemd InitSystem = "systemd"
	OpenRC  InitSystem = "openrc"
	// Unknown is any other host, including every non-Linux one.
	Unknown InitSystem = "unknown"
)

// OS is the identification data of os-release(5). Every field is empty when
// neither os-release file exists.
type OS struct {
	ID, VersionID, PrettyName string
	IDLike                    []string
}

// IsAlpine reports whether the OS is Alpine Linux.
func (o OS) IsAlpine() bool { return o.ID == "alpine" }

// IsDebianFamily reports whether the OS is Debian, Ubuntu, or derived from
// either, by ID or ID_LIKE.
func (o OS) IsDebianFamily() bool {
	isDebian := func(id string) bool { return id == "debian" || id == "ubuntu" }
	return isDebian(o.ID) || slices.ContainsFunc(o.IDLike, isDebian)
}

// Info is the detected platform.
type Info struct {
	Init InitSystem
	OS   OS
}

var (
	currentOnce sync.Once
	current     Info
	// detect is a seam for tests that count detections.
	detect = func() Info { return DetectAt(runtime.GOOS, "/") }
)

// Current returns the platform of this host. It is detected on the first call
// and cached for the life of the process, because the init system does not
// change while nexusd runs (L-46).
func Current() Info {
	currentOnce.Do(func() { current = detect() })
	return current
}

// DetectAt detects the platform of the file system tree at root, as if the
// host's GOOS were goos. It is uncached. In order:
//   - systemd, if <root>/run/systemd/system is a directory (the sd_booted(3)
//     test);
//   - OpenRC, if <root>/run/openrc exists and <root>/sbin/openrc-run and
//     <root>/sbin/rc-service are executable regular files;
//   - otherwise Unknown. A non-Linux goos is always Unknown.
//
// OS comes from <root>/etc/os-release, or <root>/usr/lib/os-release when the
// former is missing.
func DetectAt(goos, root string) Info {
	return Info{Init: detectInit(goos, root), OS: readOSRelease(root)}
}

func detectInit(goos, root string) InitSystem {
	if goos != "linux" {
		return Unknown
	}
	if isDir(filepath.Join(root, "run", "systemd", "system")) {
		return Systemd
	}
	if exists(filepath.Join(root, "run", "openrc")) &&
		isExecutableFile(filepath.Join(root, "sbin", "openrc-run")) &&
		isExecutableFile(filepath.Join(root, "sbin", "rc-service")) {
		return OpenRC
	}
	return Unknown
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
