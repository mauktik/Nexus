// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nginx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The shadow test (Data Flow 4, ASM-10, L-30) runs `nginx -t` with a
// candidate file presented in includeDir under a name nginx includes,
// without that name ever appearing in the live includeDir. The helper
// starts `nexusd nginx-op shadow-test --dir D` in a mount namespace of its
// own. That child makes every mount private, builds a copy of includeDir's
// top level on a tmpfs at D/shadow (subdirectories bind-mounted from the
// originals), places the candidate in it, binds it over includeDir and
// execs nginx -t. The namespace, and every mount in it, ends with nginx.

// Files of the shadow test in the operation's directory. The helper removes
// all three when the test ends.
const (
	shadowDirName       = "shadow"           // The tmpfs mountpoint, empty on the host
	shadowCandidateFile = "shadow-candidate" // The candidate's bytes (0600)
	shadowInputFile     = "shadow.json"      // What the child needs (0600)
)

// shadowStatusFd is the child's descriptor of the status pipe: the first of
// exec.Cmd.ExtraFiles.
const shadowStatusFd = 3

// The child's status lines. The last line the helper reads decides: "exec"
// with no line after it means nginx ran and its exit code is the verdict.
const (
	shadowExec     = "exec"
	shadowTooLarge = "too-large"
	shadowError    = "error"
)

// shadowStatusCap bounds what the helper reads from the status pipe.
const shadowStatusCap = 4 << 10

// shadowSizeCap is the size of the shadow tmpfs: the most top-level bytes of
// includeDir, with the candidate, a shadow test copies (L-31). Only tests
// change it.
var shadowSizeCap int64 = 64 << 20

// shadowExe is the nexusd binary the helper starts the child from: itself,
// even after an upgrade replaced the file. Tests that run the helper in the
// test binary point it at a built nexusd.
var shadowExe = "/proc/self/exe"

// errTooLarge marks a shadow tmpfs that ran out of room.
var errTooLarge = errors.New("includeDir too large to shadow")

// shadowInput is shadow.json.
type shadowInput struct {
	IncludeDir string `json:"includeDir"`
	Nginx      string `json:"nginx"`
	Name       string `json:"name"`      // The candidate's presented name
	SizeBytes  int64  `json:"sizeBytes"` // The tmpfs size
}

// shadowBin is the ShadowTester of an operation: it runs the child from
// exe, staging its files in the operation's directory dir. One helper runs
// at most one shadow test at a time in its directory.
type shadowBin struct {
	exe         string
	dir         string
	includeDir  string
	nginx       string
	testTimeout time.Duration
	log         io.Writer // The operation log; nil discards
}

// newShadowTester is the shadow tester of the operation in dir, with in's
// includeDir, nginx and testTimeout, logging to out.
func newShadowTester(dir string, in helperInput, out io.Writer) ShadowTester {
	return shadowBin{exe: shadowExe, dir: dir, includeDir: in.Settings.IncludeDir, nginx: in.Nginx,
		testTimeout: in.Settings.TestTimeout, log: out}
}

// Test runs `nginx -t` with candidatePath's bytes at includeDir/presentedName
// (0644, owned 0:0, as a write would leave them), in a private mount
// namespace, bounded by testTimeout. Nothing in the live includeDir or in
// the host's mounts changes. An invalid configuration is a TestOutput with
// Valid false; the error is for a test that could not run: an includeDir
// whose top level does not fit the shadow, a host without the privilege to
// make a mount namespace, or a run that did not finish.
func (s shadowBin) Test(ctx context.Context, presentedName, candidatePath string) (TestOutput, error) {
	if err := checkBaseName(presentedName); err != nil {
		return TestOutput{}, fmt.Errorf("shadow test: %w", err)
	}
	if !filepath.IsAbs(s.nginx) {
		return TestOutput{}, fmt.Errorf("refusing to run nginx %q: the path must be absolute", s.nginx)
	}
	if err := s.cleanup(); err != nil {
		return TestOutput{}, fmt.Errorf("shadow test: a previous test's files remain: %v", err)
	}
	defer func() {
		if err := s.cleanup(); err != nil && s.log != nil {
			say(s.log, "nginx-op: shadow test: %v", err)
		}
	}()
	if err := s.stage(presentedName, candidatePath); err != nil {
		return TestOutput{}, err
	}
	// The shadow keeps the host's /run, so the pid directory a live test
	// would make (piddir.go) is the one the shadow nginx needs too.
	nginxBin{path: s.nginx, testTimeout: s.testTimeout, log: s.log}.ensurePidDir(ctx)
	return s.run(ctx)
}

// stage creates the empty mountpoint, the candidate's copy and shadow.json.
func (s shadowBin) stage(presentedName, candidatePath string) error {
	if err := os.Mkdir(filepath.Join(s.dir, shadowDirName), 0o700); err != nil {
		return fmt.Errorf("shadow test: %w", err)
	}
	if err := copyCandidate(candidatePath, filepath.Join(s.dir, shadowCandidateFile)); err != nil {
		return err
	}
	return writeJSONAtomic(s.dir, shadowInputFile, shadowInput{IncludeDir: s.includeDir, Nginx: s.nginx,
		Name: presentedName, SizeBytes: shadowSizeCap})
}

// copyCandidate copies the regular file src, opened without following a
// symlink, to the new file dst (0600). A candidate larger than the shadow
// itself is too large.
func copyCandidate(src, dst string) (err error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("shadow test: the candidate: %w", err)
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil {
		return fmt.Errorf("shadow test: the candidate: %w", err)
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("shadow test: the candidate %s is not a regular file", src)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("shadow test: %w", err)
	}
	n, err := io.Copy(out, io.LimitReader(in, shadowSizeCap+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return fmt.Errorf("shadow test: copy the candidate: %w", err)
	case n > shadowSizeCap:
		return fmt.Errorf("%w: the candidate alone exceeds %s", errTooLarge, sizeText(shadowSizeCap))
	}
	return nil
}

// run starts the child and reads its verdict.
func (s shadowBin) run(ctx context.Context) (TestOutput, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return TestOutput{}, fmt.Errorf("shadow test: %w", err)
	}
	defer r.Close()
	out := &capWriter{max: TestOutputCap, tee: s.log}
	bounded, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, s.exe, HelperCommand, "shadow-test", "--dir", s.dir)
	cmd.Env = nginxEnv()
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = out, out
	cmd.ExtraFiles = []*os.File{w}
	// A mount namespace of the child's own: the runtime also marks / as
	// MS_REC|MS_PRIVATE in it before the exec, as unshare(1) does.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Unshareflags: syscall.CLONE_NEWNS}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	err = cmd.Start()
	if cerr := w.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			return TestOutput{}, fmt.Errorf("the shadow test needs root, with CAP_SYS_ADMIN, to make a private mount "+
				"namespace: %v", err)
		}
		return TestOutput{}, fmt.Errorf("the shadow test could not start: %v", err)
	}
	werr := cmd.Wait()
	status, rerr := io.ReadAll(io.LimitReader(r, shadowStatusCap))
	kind, detail := lastStatus(status)
	switch {
	case ctx.Err() != nil:
		return TestOutput{}, fmt.Errorf("the shadow test was stopped: %w", ctx.Err())
	case bounded.Err() != nil:
		return TestOutput{}, fmt.Errorf("the shadow nginx -t did not finish within %s", s.testTimeout)
	case rerr != nil:
		return TestOutput{}, fmt.Errorf("the shadow test's status cannot be read: %v", rerr)
	case kind == shadowTooLarge:
		return TestOutput{}, fmt.Errorf("%w: %s", errTooLarge, detail)
	case kind == shadowError:
		return TestOutput{}, fmt.Errorf("the shadow test could not run: %s", detail)
	case kind != shadowExec:
		return TestOutput{}, fmt.Errorf("the shadow test ended before it ran nginx (%v); its output is in the "+
			"operation log", exitText(cmd, werr))
	}
	var exitErr *exec.ExitError
	switch {
	case (werr == nil || errors.Is(werr, exec.ErrWaitDelay)) && cmd.ProcessState != nil:
	case errors.As(werr, &exitErr) && exitErr.ExitCode() >= 0:
	default:
		if werr == nil {
			werr = errors.New("it ended without a process state")
		}
		return TestOutput{}, fmt.Errorf("the shadow nginx -t did not finish: %v", werr)
	}
	return TestOutput{Valid: cmd.ProcessState.ExitCode() == 0, Output: string(validUTF8Prefix(out.buf)),
		Truncated: out.dropped}, nil
}

// exitText describes how the child ended.
func exitText(cmd *exec.Cmd, werr error) string {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.String()
	}
	return werr.Error()
}

// lastStatus is the kind and detail of the last line of status.
func lastStatus(status []byte) (kind, detail string) {
	lines := strings.Split(strings.TrimRight(string(status), "\n"), "\n")
	kind, detail, _ = strings.Cut(lines[len(lines)-1], " ")
	return kind, detail
}

// cleanup removes the shadow test's files from the operation's directory.
// The mountpoint is empty on the host: the tmpfs lived only in the child's
// namespace.
func (s shadowBin) cleanup() error {
	var errs []error
	for _, name := range []string{shadowCandidateFile, shadowInputFile, shadowDirName} {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sizeText is n in MiB, or in bytes below one MiB.
func sizeText(n int64) string {
	if n >= 1<<20 {
		return strconv.FormatInt(n>>20, 10) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// shadowChild is `nexusd nginx-op shadow-test --dir D`, the shadow test's
// child. It runs only as the helper starts it: in a mount namespace of its
// own, with the status pipe on fd 3. It returns only when it failed before
// nginx ran; otherwise nginx -t replaced it.
func shadowChild(dir string, stderr io.Writer) int {
	var st unix.Stat_t
	if err := unix.Fstat(shadowStatusFd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFIFO {
		fmt.Fprintf(stderr, "nexusd %s shadow-test: runs only as the shadow test's child, started by the helper\n",
			HelperCommand)
		return 2
	}
	syscall.CloseOnExec(shadowStatusFd)
	status := os.NewFile(shadowStatusFd, "shadow-status")
	err := shadowExecNginx(dir, status)
	kind := shadowError
	if errors.Is(err, errTooLarge) {
		kind = shadowTooLarge
	}
	fmt.Fprintf(stderr, "nexusd %s shadow-test: %v\n", HelperCommand, err)
	if _, werr := fmt.Fprintf(status, "%s %s\n", kind, strings.ReplaceAll(err.Error(), "\n", " ")); werr != nil {
		fmt.Fprintf(stderr, "nexusd %s shadow-test: status: %v\n", HelperCommand, werr)
	}
	return 1
}

// shadowExecNginx builds the shadow and execs nginx -t over it, in the
// order of Data Flow 4. It returns only on failure.
func shadowExecNginx(dir string, status io.Writer) error {
	if err := ownMountNamespace(); err != nil {
		return err
	}
	// First, before any mount: nothing this process mounts propagates to
	// the host, even where / is shared, as systemd mounts it.
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make the mounts private: %w", err)
	}
	in, err := readShadowInput(dir)
	if err != nil {
		return err
	}
	inc, err := shadowOpenDir(unix.AT_FDCWD, in.IncludeDir, false)
	if err != nil {
		return fmt.Errorf("includeDir %s cannot be opened as a directory (not through a symlink): %w",
			in.IncludeDir, err)
	}
	var incSt unix.Stat_t
	if err := unix.Fstat(inc, &incSt); err != nil {
		return fmt.Errorf("includeDir %s: %w", in.IncludeDir, err)
	}
	shadowPath := filepath.Join(dir, shadowDirName)
	if err := checkEmptyDir(shadowPath); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", shadowPath, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV,
		fmt.Sprintf("size=%d,mode=0700", in.SizeBytes)); err != nil {
		return fmt.Errorf("mount the shadow tmpfs on %s: %w", shadowPath, err)
	}
	shadow, err := shadowOpenDir(unix.AT_FDCWD, shadowPath, false)
	if err != nil {
		return fmt.Errorf("open the shadow tmpfs: %w", err)
	}
	if err := unix.Fchown(shadow, int(incSt.Uid), int(incSt.Gid)); err != nil {
		return fmt.Errorf("the shadow's owner: %w", err)
	}
	if err := unix.Fchmod(shadow, incSt.Mode&0o7777); err != nil {
		return fmt.Errorf("the shadow's mode: %w", err)
	}
	if err := copyTopLevel(inc, shadow, in); err != nil {
		return shadowFull(err, in)
	}
	if err := placeCandidate(filepath.Join(dir, shadowCandidateFile), shadow, in.Name); err != nil {
		return shadowFull(err, in)
	}
	// Recursive, so the subdirectories' binds come along.
	if err := unix.Mount(fdPath(shadow), fdPath(inc), "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind the shadow over includeDir %s: %w", in.IncludeDir, err)
	}
	unix.Close(shadow)
	unix.Close(inc)
	if _, err := io.WriteString(status, shadowExec+"\n"); err != nil {
		return fmt.Errorf("status: %w", err)
	}
	err = syscall.Exec(in.Nginx, []string{in.Nginx, "-t"}, nginxEnv())
	return fmt.Errorf("the nginx binary %s cannot be run: %v", in.Nginx, err)
}

// ownMountNamespace refuses a child that shares its parent's mount
// namespace: run by hand from a shell, its mounts would be the host's.
func ownMountNamespace() error {
	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return fmt.Errorf("the mount namespace cannot be read: %w", err)
	}
	parent, err := os.Readlink("/proc/" + strconv.Itoa(os.Getppid()) + "/ns/mnt")
	if err != nil {
		return fmt.Errorf("the parent's mount namespace cannot be read: %w", err)
	}
	if self == parent {
		return fmt.Errorf("not in a mount namespace of its own (%s); nothing was mounted", self)
	}
	return nil
}

// readShadowInput reads and checks dir's shadow.json.
func readShadowInput(dir string) (shadowInput, error) {
	var in shadowInput
	b, err := os.ReadFile(filepath.Join(dir, shadowInputFile))
	if err != nil {
		return in, fmt.Errorf("read %s: %w", shadowInputFile, err)
	}
	if err := decodeStrict(b, &in); err != nil {
		return in, fmt.Errorf("decode %s: %w", shadowInputFile, err)
	}
	switch {
	case !filepath.IsAbs(in.IncludeDir):
		return in, fmt.Errorf("%s: includeDir %q is not absolute", shadowInputFile, in.IncludeDir)
	case !filepath.IsAbs(in.Nginx):
		return in, fmt.Errorf("%s: nginx %q is not absolute", shadowInputFile, in.Nginx)
	case in.SizeBytes <= 0:
		return in, fmt.Errorf("%s: sizeBytes %d is not positive", shadowInputFile, in.SizeBytes)
	}
	if err := checkBaseName(in.Name); err != nil {
		return in, fmt.Errorf("%s: %w", shadowInputFile, err)
	}
	return in, nil
}

// checkEmptyDir checks that path is an empty directory, not a symlink.
func checkEmptyDir(path string) error {
	fd, err := shadowOpenDir(unix.AT_FDCWD, path, false)
	if err != nil {
		return fmt.Errorf("the shadow mountpoint %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("the shadow mountpoint %s: %w", path, err)
	}
	if len(names) > 0 {
		return fmt.Errorf("the shadow mountpoint %s is not empty", path)
	}
	return nil
}

// shadowOpenDir opens name in dirfd as a directory, never through a
// symlink; with path set, as an O_PATH handle (enough for a mount).
func shadowOpenDir(dirfd int, name string, path bool) (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if path {
		flags |= unix.O_PATH
	}
	var fd int
	err := ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(dirfd, name, flags, 0)
		return err
	})
	return fd, err
}

// fdPath names the open file fd for mount(2), which resolves it to the file
// itself, not to a path that could have changed since it was opened.
func fdPath(fd int) string { return "/proc/self/fd/" + strconv.Itoa(fd) }

// shadowFull reports a full tmpfs as errTooLarge.
func shadowFull(err error, in shadowInput) error {
	if errors.Is(err, unix.ENOSPC) {
		return fmt.Errorf("%w: %s holds more than %s of top-level files", errTooLarge, in.IncludeDir,
			sizeText(in.SizeBytes))
	}
	return err
}

// copyTopLevel copies inc's top level into shadow: regular files with their
// bytes, mode and owner; symlinks recreated with the same target and owner;
// subdirectories created with the same mode and owner and bind-mounted from
// the originals. Other types (FIFOs, sockets, devices) are skipped, and so
// is the presented name, which the candidate takes.
func copyTopLevel(inc, shadow int, in shadowInput) error {
	dup, err := unix.Dup(inc)
	if err != nil {
		return fmt.Errorf("includeDir: %w", err)
	}
	d := os.NewFile(uintptr(dup), in.IncludeDir)
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("list includeDir %s: %w", in.IncludeDir, err)
	}
	for _, name := range names {
		if name == in.Name {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(inc, name, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			continue // Removed since the listing
		} else if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			err = copyRegular(inc, shadow, name)
		case unix.S_IFLNK:
			err = copySymlink(inc, shadow, name, st)
		case unix.S_IFDIR:
			err = bindSubdir(inc, shadow, name, st)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// copyRegular copies the regular file name in inc to shadow, with its bytes,
// then its owner, then its mode (chown clears set-ID bits).
func copyRegular(inc, shadow int, name string) error {
	var sfd int
	err := ignoringEINTR(func() (err error) {
		sfd, err = unix.Openat(inc, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		return err
	})
	if errors.Is(err, unix.ENOENT) {
		return nil // Removed since the listing
	}
	if err != nil {
		return err
	}
	src := os.NewFile(uintptr(sfd), name)
	defer src.Close()
	var st unix.Stat_t
	if err := unix.Fstat(sfd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil // Replaced by another type since the listing
	}
	var dfd int
	err = ignoringEINTR(func() (err error) {
		dfd, err = unix.Openat(shadow, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		return err
	})
	if err != nil {
		return err
	}
	dst := os.NewFile(uintptr(dfd), name)
	_, err = io.Copy(dst, src)
	if err == nil {
		err = dst.Chown(int(st.Uid), int(st.Gid))
	}
	if err == nil {
		err = dst.Chmod(fileMode(st.Mode))
	}
	return errors.Join(err, dst.Close())
}

// copySymlink recreates the symlink name with its target and owner.
func copySymlink(inc, shadow int, name string, st unix.Stat_t) error {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(inc, name, buf)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EINVAL) {
		return nil // Removed or replaced since the listing
	}
	if err != nil {
		return err
	}
	if err := unix.Symlinkat(string(buf[:n]), shadow, name); err != nil {
		return err
	}
	return unix.Fchownat(shadow, name, int(st.Uid), int(st.Gid), unix.AT_SYMLINK_NOFOLLOW)
}

// bindSubdir creates the directory name in shadow with the original's mode
// and owner and bind-mounts the original on it, recursively.
func bindSubdir(inc, shadow int, name string, st unix.Stat_t) error {
	src, err := shadowOpenDir(inc, name, true)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return nil // Removed or replaced since the listing
	}
	if err != nil {
		return err
	}
	defer unix.Close(src)
	if err := unix.Mkdirat(shadow, name, 0o700); err != nil {
		return err
	}
	if err := unix.Fchownat(shadow, name, int(st.Uid), int(st.Gid), unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fchmodat(shadow, name, st.Mode&0o7777, 0); err != nil {
		return err
	}
	dst, err := shadowOpenDir(shadow, name, true)
	if err != nil {
		return err
	}
	defer unix.Close(dst)
	if err := unix.Mount(fdPath(src), fdPath(dst), "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind-mount the subdirectory: %w", err)
	}
	return nil
}

// placeCandidate writes the candidate's bytes to shadow as name, 0644 and
// owned 0:0, as a write leaves a config file in includeDir.
func placeCandidate(path string, shadow int, name string) error {
	src, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("the candidate: %w", err)
	}
	defer src.Close()
	var dfd int
	err = ignoringEINTR(func() (err error) {
		dfd, err = unix.Openat(shadow, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		return err
	})
	if err != nil {
		return fmt.Errorf("the candidate as %s: %w", name, err)
	}
	dst := os.NewFile(uintptr(dfd), name)
	_, err = io.Copy(dst, src)
	if err == nil {
		err = dst.Chown(0, 0)
	}
	if err == nil {
		err = dst.Chmod(0o644)
	}
	if err = errors.Join(err, dst.Close()); err != nil {
		return fmt.Errorf("the candidate as %s: %w", name, err)
	}
	return nil
}

// fileMode is the permission and set-ID bits of a stat mode as an
// os.FileMode, for (*os.File).Chmod.
func fileMode(m uint32) os.FileMode {
	mode := os.FileMode(m & 0o777)
	if m&unix.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if m&unix.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	if m&unix.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}
	return mode
}
