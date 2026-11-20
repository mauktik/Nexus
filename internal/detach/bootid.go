// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package detach

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// BootIDPath is where the kernel publishes the current boot's ID. A
// container manager can mount a per-container boot ID over it (LXC mounts
// /dev/.lxc-boot-id there), which changes on the container's restart.
const BootIDPath = "/proc/sys/kernel/random/boot_id"

// maxBootMountinfo bounds a read of mountinfo, which grows with every mount
// (a container host has hundreds).
const maxBootMountinfo = 4 << 20

// maxBootIDBytes bounds the read of a boot-ID file: one UUID and a newline.
const maxBootIDBytes = 4 << 10

// bootUUID is a boot ID: the kernel's, or a container manager's per-container
// one, both random UUIDs.
var bootUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Boot is the current boot's ID as PID 1 and every process outside a
// sandbox see it.
type Boot struct {
	ID string
	// Mounted is whether a boot ID is mounted over BootIDPath: in this
	// process's mount namespace, or in PID 1's when this process's sandbox
	// hides it.
	Mounted bool
	// Via is the file a boot ID mounted in PID 1's namespace only was read
	// through, in this process's namespace; empty when ID is this process's
	// own BootIDPath.
	Via string
}

// ReadBoot reads the current boot's ID under procRoot ("" = /proc). path
// maps an absolute path of this mount namespace to the file to open (nil:
// the path itself); only the source of a hidden boot-ID mount goes through
// it.
//
// It is BootIDPath, unless PID 1's mount table has a mount over BootIDPath
// that this process's does not. systemd 252's ProtectKernelTunables=yes
// remounts /proc/sys read-only without its submounts, so nexusd's sandbox
// reads the host kernel's boot ID while PID 1, the reboot helper and every
// transient unit read the container's (systemd 257 and later keep the
// submount, read-only). The mount's source is then found through a mount of
// the same device in this process's table and read there, checked to be a
// regular file on that device holding a UUID. When that fails the error says
// why: a boot ID that cannot be trusted is never replaced by the kernel's.
//
// A mount table that cannot be read is an error, never BootIDPath
// (SEC-1). Without it a hidden or absent boot-ID mount cannot be told
// apart, so the ID could be the kernel's here and the container's for the
// reboot helper, which runs outside any sandbox and records what PID 1 sees:
// the ops manager would then take the difference for a reboot that never
// happened (R4). Every reader of the boot ID, nexusd's preflight and
// admission, the ops manager, the supervisors and the helper, goes through
// this function, so they agree or none of them has an ID.
func ReadBoot(procRoot string, path func(abs string) string) (Boot, error) {
	if procRoot == "" {
		procRoot = "/proc"
	}
	own, ownDev, err := readOwnBootID(procRoot)
	if err != nil {
		return Boot{}, err
	}
	self, err := readMountinfo(filepath.Join(procRoot, "self", "mountinfo"))
	if err != nil {
		return Boot{}, unverifiable("this process's", err)
	}
	if m, ok := lastMountOn(self, BootIDPath); ok &&
		unix.Major(ownDev) == m.major && unix.Minor(ownDev) == m.minor {
		return Boot{ID: own, Mounted: true}, nil
	}
	// Either no boot-ID mount, or the table lists one that the file read is
	// not on: mountinfo also lists mounts that a later mount over /proc/sys
	// shadows, and the path then reads procfs, the host kernel's ID. Neither
	// is a boot ID mounted for this process, so PID 1's table decides.
	pid1, err := readMountinfo(filepath.Join(procRoot, "1", "mountinfo"))
	if err != nil {
		return Boot{}, unverifiable("PID 1's", err)
	}
	m, ok := lastMountOn(pid1, BootIDPath)
	if !ok {
		return Boot{ID: own}, nil
	}
	via, id, err := readHiddenBootID(self, m, path)
	if err != nil {
		return Boot{}, fmt.Errorf("detach: PID 1 has a boot ID mounted over %s (%s on device %d:%d) that this process's "+
			"mount namespace hides, and it cannot be read here: %w", BootIDPath, m.root, m.major, m.minor, err)
	}
	return Boot{ID: id, Mounted: true, Via: via}, nil
}

// unverifiable is ReadBoot's error for a mount table, whose is named, that
// cannot be read.
func unverifiable(whose string, err error) error {
	return fmt.Errorf("detach: the boot ID at %s cannot be verified: %s mount table cannot be read (%w), "+
		"so whether a boot ID is mounted over it is unknown", BootIDPath, whose, err)
}

// readOwnBootID is BootIDPath under procRoot, trimmed, and the device of
// the file it was read from.
func readOwnBootID(procRoot string) (string, uint64, error) {
	b, dev, err := readBootFileDev(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"), nil)
	if err != nil {
		return "", 0, fmt.Errorf("detach: read boot id: %w", err)
	}
	id := string(bytes.TrimSpace(b))
	if id == "" {
		return "", 0, errors.New("detach: empty boot id")
	}
	return id, dev, nil
}

// readHiddenBootID reads the source of PID 1's boot-ID mount m through the
// mount in self of the same device whose root holds it, the longest such
// root first.
func readHiddenBootID(self []bootMount, m bootMount, path func(string) string) (via, id string, err error) {
	best, rel := -1, ""
	for i, s := range self {
		if s.major != m.major || s.minor != m.minor || s.mountPoint == BootIDPath {
			continue
		}
		r, ok := underRoot(m.root, s.root)
		if !ok || (best >= 0 && len(s.root) < len(self[best].root)) {
			continue
		}
		best, rel = i, r
	}
	if best < 0 {
		return "", "", fmt.Errorf("no mount of device %d:%d holding %s in this process's mount table", m.major, m.minor, m.root)
	}
	via = filepath.Join(self[best].mountPoint, rel)
	file := via
	if path != nil {
		file = path(via)
	}
	b, err := readBootFile(file, &m)
	if err != nil {
		return via, "", err
	}
	id = string(bytes.TrimSpace(b))
	if !bootUUID.MatchString(id) {
		return via, "", fmt.Errorf("%s does not hold a boot ID (a UUID): %q", via, truncate(id, 64))
	}
	return via, id, nil
}

// underRoot is the path of root-relative file under mount root, when the
// mount holds it.
func underRoot(file, root string) (string, bool) {
	switch {
	case root == "/":
		return file, true
	case file == root:
		return "", true
	case strings.HasPrefix(file, root+"/"):
		return strings.TrimPrefix(file, root), true
	}
	return "", false
}

// readBootFile is the content of the regular file at path, at most
// maxBootIDBytes, and on on's device when on is not nil. It is opened
// without following a final symlink and without blocking (a FIFO never
// hangs the read), and checked on the descriptor.
func readBootFile(path string, on *bootMount) ([]byte, error) {
	b, _, err := readBootFileDev(path, on)
	return b, err
}

// readBootFileDev is readBootFile and the device of the file read.
func readBootFileDev(path string, on *bootMount) ([]byte, uint64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, 0, fmt.Errorf("%s: no device number", path)
	}
	dev := uint64(st.Dev) // int32 on darwin, uint64 on Linux
	if on != nil && (unix.Major(dev) != on.major || unix.Minor(dev) != on.minor) {
		return nil, 0, fmt.Errorf("%s is on device %d:%d, not %d:%d", path, unix.Major(dev), unix.Minor(dev), on.major, on.minor)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBootIDBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(b) > maxBootIDBytes {
		return nil, 0, fmt.Errorf("%s is over %d bytes", path, maxBootIDBytes)
	}
	return b, dev, nil
}

// bootMount is the part of a mountinfo line ReadBoot uses
// (proc_pid_mountinfo(5)): field 3 major:minor, 4 root, 5 mount point.
type bootMount struct {
	major, minor uint32
	root         string
	mountPoint   string
}

// readMountinfo parses the mountinfo file at path. Lines it cannot parse
// are skipped.
func readMountinfo(path string) ([]bootMount, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBootMountinfo+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBootMountinfo {
		return nil, fmt.Errorf("%s is over %d bytes", path, maxBootMountinfo)
	}
	var out []bootMount
	for line := range strings.Lines(string(b)) {
		f := strings.Split(strings.TrimRight(line, "\n"), " ")
		if len(f) < 5 {
			continue
		}
		maj, min, ok := strings.Cut(f[2], ":")
		ma, err1 := strconv.ParseUint(maj, 10, 32)
		mi, err2 := strconv.ParseUint(min, 10, 32)
		if !ok || err1 != nil || err2 != nil {
			continue
		}
		out = append(out, bootMount{major: uint32(ma), minor: uint32(mi),
			root: unescapeMountinfo(f[3]), mountPoint: unescapeMountinfo(f[4])})
	}
	return out, nil
}

// lastMountOn is the last mount in ms on mountPoint: the top one.
func lastMountOn(ms []bootMount, mountPoint string) (bootMount, bool) {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].mountPoint == mountPoint {
			return ms[i], true
		}
	}
	return bootMount{}, false
}

// unescapeMountinfo undoes the kernel's octal escapes (\040 and the like)
// in a mountinfo path.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
