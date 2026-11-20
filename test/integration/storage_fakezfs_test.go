// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// The T1 fake zpool and zfs (storage-provider WI 5.3; test-strategy.md, "T1
// fake tools"): TestStorageFakeZFS is the re-exec entry the wrappers at
// /usr/sbin/zpool and /usr/sbin/zfs call, and stgFakeTools installs them.
// stgStuckFUSE serves a FUSE mount whose one lookup never answers: a fake
// in hang mode that stats it sits in uninterruptible sleep, the kernel state
// A-31 is about, which SIGKILL does not end.

package integration

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
)

const (
	// stgFakeMarker is the wrappers' first argument after "--": only a call
	// that carries it runs the fake.
	stgFakeMarker = "nx-fakezfs"
	// stgStateDir is the fake's state directory: under the shipped unit it
	// is read-only inside nexusd's sandbox (ProtectSystem=strict).
	stgStateDir = "/run/nx-fakezfs"
	// stgReadsDir holds reads.log. /run is read-only inside the sandbox
	// and /tmp is private to it (PrivateTmp=yes), so the reads log lives on
	// /dev/shm, which the unit leaves shared and writable.
	stgReadsDir = "/dev/shm/nx-fakezfs"
	stgReadsLog = stgReadsDir + "/reads.log"
	// stgCaptures is where run.sh copies internal/providers/storage/testdata.
	stgCaptures = "/harness/src/github.com/mauktik/nexus/internal/providers/storage/testdata/captures"
	stgCapture  = stgCaptures + "/home-server"
	stgZpool    = "/usr/sbin/zpool"
	stgZfs      = "/usr/sbin/zfs"
	stgDevZFS   = "/dev/zfs"
	// stgDevZFSSaved is where a /dev/zfs the container already has (a
	// privileged container sees the VM's) is kept while a test needs none.
	stgDevZFSSaved = "/dev/.nx-zfs-saved"
)

// TestStorageFakeZFS is the fake tools' entry. Run by the harness it skips;
// run by a wrapper (`-- nx-fakezfs <state> <capture> <reads.log> <tool>
// args…`) it answers the call through storagetest.Main and exits with the
// tool's code before the test framework prints anything, so the process's
// output is the tool's alone.
func TestStorageFakeZFS(t *testing.T) {
	args := flag.Args()
	if len(args) < 6 || args[0] != stgFakeMarker {
		t.Skip("not invoked by the fake zpool or zfs wrapper")
	}
	code := storagetest.Main(t, storagetest.MainConfig{StateDir: args[1], Capture: args[2], ReadsLog: args[3]},
		args[4:], os.Stdout, os.Stderr)
	// syscall.Exit, not os.Exit: the testing package turns an os.Exit(0)
	// inside a test into a failure.
	syscall.Exit(code)
}

// stgWrapper is the two-line sh wrapper of tool. The unit gives the child
// only PATH and LC_ALL, so the capture root and the state directory are set
// here, as arguments; NEXUS_HARNESS=1 lets the test binary's TestMain run.
func stgWrapper(tool string) string {
	return fmt.Sprintf("#!/bin/sh\nNEXUS_HARNESS=1 exec /harness/integration.test -test.run='^TestStorageFakeZFS$' -- %s %s %s %s %s \"$@\"\n",
		stgFakeMarker, stgStateDir, stgCapture, stgReadsLog, tool)
}

// stgShellEnv are the variables the wrapper and its shell add to the
// environment the caller gave: NEXUS_HARNESS (the wrapper's), PWD (dash and
// BusyBox ash export it) and SHLVL (BusyBox ash). A write's recorded
// environment minus these is what zfs.Exec passed.
var stgShellEnv = []string{"NEXUS_HARNESS", "PWD", "SHLVL"}

// stgFakeTools installs the fake zpool and zfs, empty state and logs, and
// /dev/zfs when devZFS (else it makes sure there is none); cleanup removes
// everything and puts back a /dev/zfs the container had.
func stgFakeTools(t *testing.T, devZFS bool) {
	t.Helper()
	for _, p := range []string{stgZpool, stgZfs, "/sbin/zpool", "/sbin/zfs"} {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			if !strings.Contains(string(b), stgFakeMarker) {
				t.Fatalf("%s exists and is not the fake: refusing to replace a real ZFS tool", p)
			}
		}
	}
	for _, d := range []string{stgStateDir, stgReadsDir} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(stgReadsLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stgStateDir, storagetest.CallsFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{stgZpool, stgZfs} {
		if err := os.WriteFile(tool, []byte(stgWrapper(tool)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stgDevZFSState(t, devZFS)
	t.Cleanup(func() {
		for _, p := range []string{stgZpool, stgZfs, stgStateDir, stgReadsDir} {
			if err := os.RemoveAll(p); err != nil {
				t.Errorf("cleanup %s: %v", p, err)
			}
		}
	})
}

// stgDevZFSState makes /dev/zfs present (a character device, planted with
// mknod c 10 249 when missing) or absent, and restores it at cleanup.
func stgDevZFSState(t *testing.T, want bool) {
	t.Helper()
	_, err := os.Stat(stgDevZFS)
	had := err == nil
	switch {
	case want && !had:
		mustSh(t, "mknod", "-m", "0600", stgDevZFS, "c", "10", "249")
		t.Cleanup(func() { _ = os.Remove(stgDevZFS) })
	case !want && had:
		if err := os.Rename(stgDevZFS, stgDevZFSSaved); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Rename(stgDevZFSSaved, stgDevZFS); err != nil {
				t.Errorf("restore /dev/zfs: %v", err)
			}
		})
	}
	if fi, err := os.Stat(stgDevZFS); want && (err != nil || fi.Mode()&os.ModeCharDevice == 0) {
		t.Fatalf("/dev/zfs: %v %v, want a character device", fi, err)
	} else if !want && err == nil {
		t.Fatal("/dev/zfs is still present")
	}
}

// ---- a FUSE mount that never answers one lookup ----

// The FUSE kernel ABI, checked field by field against include/uapi/linux/fuse.h
// at the Linux v6.8 tag (the kernel the harness runs, 6.8.0-100-generic):
// https://raw.githubusercontent.com/torvalds/linux/v6.8/include/uapi/linux/fuse.h
//
//	FUSE_KERNEL_VERSION 7 (:246), FUSE_KERNEL_MINOR_VERSION 39 (:249),
//	FUSE_ROOT_ID 1 (:252)
//	opcodes: LOOKUP 1 (:572), FORGET 2 (:573), GETATTR 3 (:574), STATFS 17
//	(:587), INIT 26 (:595), INTERRUPT 36 (:605), BATCH_FORGET 42 (:611)
//	fuse_in_header (:963) 4+4+8+8+4+4+4+2+2 = 40
//	fuse_out_header (:975) 4+4+8 = 16
//	fuse_init_out (:868) 4*4 + 2+2 + 4+4 + 2+2 + 4 + 7*4 = 64; max_write at
//	20, time_gran at 24
//	fuse_attr (:257) 6*8 + 10*4 = 88; mode at 60, nlink at 64, blksize at 80
//	fuse_attr_out (:679) 8+4+4 + fuse_attr 88 = 104; attr at 16
//	fuse_kstatfs (:310) 5*8 + 4*4 + 6*4 = 80; bsize at 40, namelen at 44,
//	frsize at 48; fuse_statfs_out (:810) is one fuse_kstatfs = 80
//
// The server answers INIT with minor 31 (<= the kernel's 39), which keeps
// the 64-byte fuse_init_out and 104-byte fuse_attr_out layouts.
const (
	fuseLookup      = 1
	fuseForget      = 2
	fuseGetattr     = 3
	fuseStatfs      = 17
	fuseInit        = 26
	fuseInterrupt   = 36
	fuseBatchForget = 42
	fuseInHeader    = 40
	fuseOutHeader   = 16
	fuseInitOut     = 64
	fuseAttrOut     = 104
	fuseStatfsOut   = 80
	fuseRootID      = 1
	fuseKernelMajor = 7
	fuseServerMinor = 31
)

// stgStuckName is the one name whose lookup is read and never answered.
const stgStuckName = "stuck"

// stuckFUSE is a FUSE file system at dir: its root answers getattr and
// statfs, every lookup but stgStuckName answers ENOENT, and a lookup of
// stgStuckName is read and never answered. A request the server has read
// waits uninterruptibly for its answer once a fatal signal arrived
// (fs/fuse/dev.c request_wait_answer), so the caller cannot be killed until
// the connection is aborted, which closing the device does.
type stuckFUSE struct {
	dir  string
	dev  *os.File
	done chan struct{}

	mu      sync.Mutex
	stuck   int // lookups of stgStuckName read and left unanswered
	readErr error
}

// stgStuckFUSE mounts a stuckFUSE at dir; cleanup aborts it (closing the
// device ends every unanswered request with ENOTCONN) and unmounts it.
func stgStuckFUSE(t *testing.T, dir string) *stuckFUSE {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open("/dev/fuse", syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open /dev/fuse: %v", err)
	}
	opts := fmt.Sprintf("fd=%d,rootmode=40000,user_id=0,group_id=0,allow_other", fd)
	if err := syscall.Mount("nx-stuck", dir, "fuse", syscall.MS_NOSUID|syscall.MS_NODEV, opts); err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("mount fuse at %s: %v", dir, err)
	}
	f := &stuckFUSE{dir: dir, dev: os.NewFile(uintptr(fd), "/dev/fuse"), done: make(chan struct{})}
	go f.serve()
	t.Cleanup(f.abort)
	return f
}

// path is the name whose stat never returns.
func (f *stuckFUSE) path() string { return filepath.Join(f.dir, stgStuckName) }

func (f *stuckFUSE) stuckLookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stuck
}

func (f *stuckFUSE) abort() {
	_ = f.dev.Close()
	<-f.done
	_ = syscall.Unmount(f.dir, syscall.MNT_DETACH)
	_ = os.Remove(f.dir)
}

func (f *stuckFUSE) serve() {
	defer close(f.done)
	buf := make([]byte, 1<<20)
	for {
		n, err := f.dev.Read(buf)
		switch {
		case errors.Is(err, syscall.EINTR), errors.Is(err, syscall.ENOENT):
			continue // interrupted, or a request aborted before it was read
		case err != nil:
			f.mu.Lock()
			f.readErr = err
			f.mu.Unlock()
			return
		case n < fuseInHeader:
			continue
		}
		f.handle(buf[:n])
	}
}

func (f *stuckFUSE) handle(req []byte) {
	le := binary.LittleEndian
	op := le.Uint32(req[4:])
	unique := le.Uint64(req[8:])
	body := req[fuseInHeader:]
	switch op {
	case fuseForget, fuseBatchForget, fuseInterrupt:
		// No reply: forgets never get one, and an unanswered INTERRUPT
		// leaves the interrupted request waiting.
	case fuseInit:
		out := make([]byte, fuseInitOut)
		le.PutUint32(out[0:], fuseKernelMajor)
		le.PutUint32(out[4:], min(le.Uint32(body[4:]), fuseServerMinor))
		le.PutUint32(out[8:], le.Uint32(body[8:])) // max_readahead
		le.PutUint32(out[20:], 4096)               // max_write
		le.PutUint32(out[24:], 1)                  // time_gran
		f.reply(unique, 0, out)
	case fuseGetattr:
		out := make([]byte, fuseAttrOut)
		attr := out[16:]
		le.PutUint64(attr[0:], fuseRootID)             // ino
		le.PutUint32(attr[60:], syscall.S_IFDIR|0o755) // mode
		le.PutUint32(attr[64:], 2)                     // nlink
		le.PutUint32(attr[80:], 4096)                  // blksize
		f.reply(unique, 0, out)
	case fuseStatfs:
		out := make([]byte, fuseStatfsOut)
		le.PutUint32(out[40:], 4096) // bsize
		le.PutUint32(out[44:], 255)  // namelen
		le.PutUint32(out[48:], 4096) // frsize
		f.reply(unique, 0, out)
	case fuseLookup:
		name, _, _ := strings.Cut(string(body), "\x00")
		if name == stgStuckName {
			f.mu.Lock()
			f.stuck++
			f.mu.Unlock()
			return
		}
		f.reply(unique, -int32(syscall.ENOENT), nil)
	default:
		f.reply(unique, -int32(syscall.ENOSYS), nil)
	}
}

func (f *stuckFUSE) reply(unique uint64, errno int32, payload []byte) {
	out := make([]byte, fuseOutHeader+len(payload))
	binary.LittleEndian.PutUint32(out[0:], uint32(len(out)))
	binary.LittleEndian.PutUint32(out[4:], uint32(errno))
	binary.LittleEndian.PutUint64(out[8:], unique)
	copy(out[fuseOutHeader:], payload)
	_, _ = f.dev.Write(out)
}
