// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package svcctl

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mauktik/nexus/internal/platform"
)

// ErrNoSelfService: neither the cgroup (systemd) nor a child_pid (OpenRC)
// names the unit or script running pid. Preflight stays available (D-6).
var ErrNoSelfService = errors.New("svcctl: the service running nexusd cannot be identified")

const (
	// maxCgroupBytes bounds the read of /proc/<pid>/cgroup.
	maxCgroupBytes = 64 << 10
	// maxChildPIDBytes bounds the read of a supervise-daemon child_pid: a
	// larger file is skipped, never matched by its prefix.
	maxChildPIDBytes = 64
)

// SelfService is the name of the service running pid (without .service).
// On systemd it is the last path element ending .service of the 0:: line
// (cgroup v2) of <procRoot>/<pid>/cgroup; the v1 lines of a hybrid
// hierarchy are ignored. On OpenRC it is the script whose
// <runRoot>/openrc/options/<script>/child_pid holds pid, which
// supervise-daemon writes. When neither names a service matching
// ServiceNamePattern the error is ErrNoSelfService. A file that cannot be
// read is an ordinary error (a missing cgroup file wraps fs.ErrNotExist).
func SelfService(init platform.InitSystem, procRoot, runRoot string, pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("svcctl: invalid pid %d", pid)
	}
	switch init {
	case platform.Systemd:
		return selfServiceSystemd(procRoot, pid)
	case platform.OpenRC:
		return selfServiceOpenRC(runRoot, pid, openRegular)
	}
	return "", fmt.Errorf("svcctl: no self-service lookup for init system %q", string(init))
}

func selfServiceSystemd(procRoot string, pid int) (string, error) {
	if procRoot == "" {
		procRoot = "/proc"
	}
	path := filepath.Join(procRoot, strconv.Itoa(pid), "cgroup")
	f, _, err := openRegular(path)
	if err != nil {
		return "", fmt.Errorf("svcctl: reading the cgroup of pid %d: %w", pid, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCgroupBytes))
	if err != nil {
		return "", fmt.Errorf("svcctl: reading %s: %w", path, err)
	}
	if name := parseCgroupSelf(b); name != "" {
		return name, nil
	}
	return "", fmt.Errorf("%w: %s has no .service element on its 0:: line", ErrNoSelfService, path)
}

// parseCgroupSelf is the service named by the first 0:: line of a
// /proc/<pid>/cgroup file: its last path element ending .service, without
// the suffix, or "" when there is none or it does not match
// ServiceNamePattern.
func parseCgroupSelf(b []byte) string {
	for line := range bytes.Lines(b) {
		path, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("0::"))
		if !ok {
			continue
		}
		elems := strings.Split(string(path), "/")
		for _, e := range slices.Backward(elems) {
			if name, ok := strings.CutSuffix(e, ".service"); ok {
				if serviceName.MatchString(name) {
					return name
				}
				return ""
			}
		}
		return ""
	}
	return ""
}

// selfServiceOpenRC scans <runRoot>/openrc/options/*/child_pid, in name
// order, for pid. Only child_pid files are opened, each through open (in
// production openRegular: no symlink, no FIFO, regular files only). A
// child_pid that is missing, not a regular file, over maxChildPIDBytes or
// not a number is skipped; one that cannot be read for lack of permission
// is remembered and returned when nothing matched.
func selfServiceOpenRC(runRoot string, pid int, open func(string) (*os.File, os.FileInfo, error)) (string, error) {
	if runRoot == "" {
		runRoot = "/run"
	}
	options := filepath.Join(runRoot, "openrc", "options")
	ents, err := os.ReadDir(options)
	if err != nil {
		return "", fmt.Errorf("svcctl: listing %s: %w", options, err)
	}
	var readErr error
	for _, e := range ents {
		if !e.IsDir() || !serviceName.MatchString(e.Name()) {
			continue
		}
		got, err := readChildPID(filepath.Join(options, e.Name(), "child_pid"), open)
		if err != nil {
			if readErr == nil && errors.Is(err, fs.ErrPermission) {
				readErr = err
			}
			continue
		}
		if got == pid {
			return e.Name(), nil
		}
	}
	if readErr != nil {
		return "", fmt.Errorf("svcctl: reading a child_pid under %s: %w", options, readErr)
	}
	return "", fmt.Errorf("%w: no child_pid under %s holds pid %d", ErrNoSelfService, options, pid)
}

// readChildPID is the positive PID a child_pid file holds, read with the
// maxChildPIDBytes bound. Any other content is an error.
func readChildPID(path string, open func(string) (*os.File, os.FileInfo, error)) (int, error) {
	f, fi, err := open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if fi.Size() > maxChildPIDBytes {
		return 0, fmt.Errorf("%s is over %d bytes", path, maxChildPIDBytes)
	}
	// One byte over the bound tells a file that grew after the stat.
	b, err := io.ReadAll(io.LimitReader(f, maxChildPIDBytes+1))
	if err != nil {
		return 0, err
	}
	if len(b) > maxChildPIDBytes {
		return 0, fmt.Errorf("%s is over %d bytes", path, maxChildPIDBytes)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	if !sc.Scan() {
		return 0, fmt.Errorf("%s is empty", path)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%s holds no pid", path)
	}
	return pid, nil
}
