// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package host

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// countSyscalls replaces the seam's syscalls with counting wrappers.
func countSyscalls(t *testing.T) (stats, statfses *int) {
	t.Helper()
	var nStat, nStatfs int
	oldStat, oldStatfs := sysStat, sysStatfs
	sysStat = func(p string, st *syscall.Stat_t) error { nStat++; return oldStat(p, st) }
	sysStatfs = func(p string, sf *syscall.Statfs_t) error { nStatfs++; return oldStatfs(p, sf) }
	t.Cleanup(func() { sysStat, sysStatfs = oldStat, oldStatfs })
	return &nStat, &nStatfs
}

// The Statfs seam on a non-directory (a file bind such as /etc/hosts)
// answers IsDir false with zero counts and makes no statfs call.
func TestDefaultStatfsNonDirectorySkipsStatfs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(file, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats, statfses := countSyscalls(t)
	u, err := defaultStatfs(file)
	if err != nil || u != (Usage{}) {
		t.Errorf("defaultStatfs(file) = %+v, %v; want Usage{IsDir: false} with zero counts", u, err)
	}
	if *stats != 1 || *statfses != 0 {
		t.Errorf("stat %d, statfs %d calls; want 1 and 0", *stats, *statfses)
	}
}

// On a directory both syscalls run in the one call and Usage carries
// statfs(2)'s counts.
func TestDefaultStatfsDirectory(t *testing.T) {
	dir := t.TempDir()
	stats, statfses := countSyscalls(t)
	u, err := defaultStatfs(dir)
	if err != nil || !u.IsDir {
		t.Fatalf("defaultStatfs(dir) = %+v, %v", u, err)
	}
	if *stats != 1 || *statfses != 1 {
		t.Errorf("stat %d, statfs %d calls; want 1 and 1", *stats, *statfses)
	}
	var sf syscall.Statfs_t
	if err := syscall.Statfs(dir, &sf); err != nil {
		t.Fatal(err)
	}
	if u.Blocks != uint64(sf.Blocks) || u.Frsize != uint64(sf.Frsize) || u.Files != uint64(sf.Files) || u.Blocks == 0 || u.Frsize == 0 {
		t.Errorf("Usage %+v disagrees with statfs %+v", u, sf)
	}
	if u.Bavail > u.Bfree || u.Bfree > u.Blocks {
		t.Errorf("Usage %+v: want Bavail <= Bfree <= Blocks", u)
	}
}

func TestDefaultStatfsMissing(t *testing.T) {
	_, err := defaultStatfs(filepath.Join(t.TempDir(), "gone"))
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Op != "stat" || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want a stat *fs.PathError with fs.ErrNotExist", err)
	}
	if r := reasonFor("/mnt/gone", err); r != "not present: /mnt/gone" {
		t.Errorf("reason %q", r)
	}
}

// uname(2)'s machine field names the running architecture.
func TestDefaultUnameMachine(t *testing.T) {
	m, err := defaultUname()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"amd64": {"x86_64"}, "arm64": {"aarch64", "aarch64_be"}, "386": {"i386", "i486", "i586", "i686", "x86_64"},
		"arm": {"armv6l", "armv7l", "armv8l", "aarch64"}, "riscv64": {"riscv64"}, "ppc64le": {"ppc64le"}, "s390x": {"s390x"},
	}[runtime.GOARCH]
	if want == nil {
		if m == "" {
			t.Errorf("machine is empty")
		}
		return
	}
	for _, w := range want {
		if m == w {
			return
		}
	}
	t.Errorf("machine %q, want one of %q for GOARCH %s", m, want, runtime.GOARCH)
}
