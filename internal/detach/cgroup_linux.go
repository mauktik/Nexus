//go:build linux

package detach

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The process backend's cgroup v2 groups (FR-20 item 4). nexusd creates
// <root>/nexus-ops/<id> and clones the operation's supervisor into it; the
// supervisor of a KillControlGroup operation runs its child in <id>/work,
// which it kills and sweeps as systemd kills a KillMode=control-group unit.
// No controller is ever enabled in a subtree_control under nexus-ops, so the
// no-internal-processes rule never stops a supervisor in <id> from having
// its child in <id>/work (L-8).
const (
	defaultCgroupRoot = "/sys/fs/cgroup"
	opsCgroup         = "nexus-ops"
	workCgroup        = "work"
)

// freezeWait bounds how long kill waits for a frozen group to report
// frozen before it reads its processes anyway.
const freezeWait = time.Second

var (
	// supervisorCgroupRoot is where a supervisor finds the group it was
	// started in: the mount the launcher uses by default.
	supervisorCgroupRoot = defaultCgroupRoot
	// cloneIntoCgroup starts cmd in the cgroup open as fd (clone3 with
	// CLONE_INTO_CGROUP); a test makes it fail.
	cloneIntoCgroup = func(cmd *exec.Cmd, fd int) error {
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = true, fd
		return cmd.Start()
	}
	// hasCgroupKill reports whether the group at path has cgroup.kill
	// (Linux 5.14); a test makes it false.
	hasCgroupKill = func(path string) bool {
		_, err := os.Stat(filepath.Join(path, "cgroup.kill"))
		return err == nil
	}
	// killCgroup is kill; a test observes the group after it.
	killCgroup = kill
)

func init() { ownCgroup = linuxOwnCgroup }

// available reports why the cgroup v2 hierarchy at root cannot hold
// operation groups: no cgroup.controllers (not cgroup v2), or nexus-ops
// cannot be created there.
func available(root string) error {
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return fmt.Errorf("no cgroup v2 hierarchy at %s: %w", root, err)
	}
	if err := os.Mkdir(filepath.Join(root, opsCgroup), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create the %s cgroup: %w", opsCgroup, err)
	}
	return nil
}

// create makes the group of operation id, nexus-ops/<id> under root, and
// returns its path. It leaves no controller enabled in nexus-ops's
// subtree_control, and enables none in the new group's.
func create(root, id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("%q is not an operation ID", id)
	}
	ops := filepath.Join(root, opsCgroup)
	if err := clearSubtreeControl(ops); err != nil {
		return "", err
	}
	path := filepath.Join(ops, id)
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("create cgroup %s: %w", path, err)
	}
	return path, nil
}

// clearSubtreeControl disables every controller enabled in path's
// subtree_control, normally none.
func clearSubtreeControl(path string) error {
	b, err := os.ReadFile(filepath.Join(path, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("read %s subtree_control: %w", path, err)
	}
	var off []string
	for _, c := range strings.Fields(string(b)) {
		off = append(off, "-"+c)
	}
	if len(off) == 0 {
		return nil
	}
	return writeCgroupFile(path, "cgroup.subtree_control", strings.Join(off, " "))
}

// openFD opens the group at path for clone3's CLONE_INTO_CGROUP.
func openFD(path string) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open cgroup %s: %w", path, err)
	}
	return fd, nil
}

// cloneInto starts the command newCmd builds, cloned into the group at
// path (clone3 with CLONE_INTO_CGROUP). An error means it did not start.
func cloneInto(newCmd func() *exec.Cmd, path string) (*exec.Cmd, error) {
	fd, err := openFD(path)
	if err != nil {
		return nil, err
	}
	cmd := newCmd()
	err = cloneIntoCgroup(cmd, fd)
	// A read-only directory descriptor: closing it cannot lose data, and
	// the child's placement no longer depends on it.
	_ = syscall.Close(fd)
	if err != nil {
		return nil, fmt.Errorf("clone into cgroup %s: %w", path, err)
	}
	return cmd, nil
}

// startInCgroup starts the command newCmd builds in the group at path:
// cloned into it, or, when that fails (clone3 or CLONE_INTO_CGROUP
// unsupported or refused), started outside and moved in through
// cgroup.procs. The move comes after the process runs, so it suits only a
// process that forks nothing before it is moved: the supervisor, which
// waits for its identity. It returns the started command, why it is not in
// the group (nil when it is), and why it did not start.
func startInCgroup(newCmd func() *exec.Cmd, path string) (cmd *exec.Cmd, notPlaced, err error) {
	cmd, cloneErr := cloneInto(newCmd, path)
	if cloneErr == nil {
		return cmd, nil, nil
	}
	cmd, notPlaced, err = startMoved(newCmd, path)
	if notPlaced != nil {
		notPlaced = errors.Join(cloneErr, notPlaced)
	}
	return cmd, notPlaced, err
}

// startMoved is startInCgroup without the clone: it starts the command
// outside the group at path and moves it in through cgroup.procs.
func startMoved(newCmd func() *exec.Cmd, path string) (cmd *exec.Cmd, notPlaced, err error) {
	cmd = newCmd()
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, moveTo(path, cmd.Process.Pid), nil
}

// moveTo moves process pid, with all its threads, into the group at path.
func moveTo(path string, pid int) error {
	return writeCgroupFile(path, "cgroup.procs", strconv.Itoa(pid))
}

// kill SIGKILLs every process in the group at path and its descendants:
// through cgroup.kill where the kernel has it, else by freezing the group so
// nothing in it can fork, sending SIGKILL to every PID in it, and thawing
// it (a frozen process still dies of a fatal signal) (L-9).
func kill(path string) error {
	if hasCgroupKill(path) {
		return writeCgroupFile(path, "cgroup.kill", "1")
	}
	if err := writeCgroupFile(path, "cgroup.freeze", "1"); err != nil {
		return err
	}
	for deadline := time.Now().Add(freezeWait); !eventIs(path, "frozen") && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	var errs []error
	pids, err := procs(path)
	if err != nil {
		errs = append(errs, err)
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("kill %d: %w", pid, err))
		}
	}
	if err := writeCgroupFile(path, "cgroup.freeze", "0"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// signalAll sends sig to every process in the group at path and its
// descendants.
func signalAll(path string, sig syscall.Signal) error {
	pids, err := procs(path)
	errs := []error{err}
	for _, pid := range pids {
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("send %s to %d: %w", signalName(sig), pid, err))
		}
	}
	return errors.Join(errs...)
}

// procs lists the PIDs of the group at path and of its descendants. A PID
// of 0, a process outside this PID namespace, is left out.
func procs(path string) ([]int, error) {
	var pids []int
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != path && errors.Is(err, fs.ErrNotExist) {
				return nil // A descendant removed during the walk
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(p, "cgroup.procs"))
		if errors.Is(err, fs.ErrNotExist) && p != path {
			return nil
		}
		if err != nil {
			return err
		}
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
				pids = append(pids, pid)
			}
		}
		return nil
	})
	if err != nil {
		return pids, fmt.Errorf("list the processes of cgroup %s: %w", path, err)
	}
	return pids, nil
}

// eventIs reports whether key is 1 in the group's cgroup.events
// ("populated": it or a descendant has a live process; "frozen").
func eventIs(path, key string) bool {
	b, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(b)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && k == key {
			return v == "1"
		}
	}
	return false
}

// selfPath is this process's cgroup v2 path, relative to its cgroup
// namespace's root, from /proc/self/cgroup.
func selfPath() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	return parseCgroupFile(b)
}

// parseCgroupFile returns the path of the "0::<path>" line of a
// /proc/<pid>/cgroup file.
func parseCgroupFile(b []byte) (string, error) {
	for line := range bytes.Lines(b) {
		if p, ok := bytes.CutPrefix(bytes.TrimRight(line, "\n"), []byte("0::")); ok {
			return string(p), nil
		}
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
}

// cleanup removes the group at path and its work group, best effort: a
// group that still has a process stays.
func cleanup(path string) error {
	var errs []error
	for _, p := range []string{filepath.Join(path, workCgroup), path} {
		if err := syscall.Rmdir(p); err != nil && !errors.Is(err, syscall.ENOENT) {
			errs = append(errs, fmt.Errorf("remove cgroup %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

func writeCgroupFile(path, name, value string) error {
	f, err := os.OpenFile(filepath.Join(path, name), os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("write %s of cgroup %s: %w", name, path, err)
	}
	_, err = f.WriteString(value)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s of cgroup %s: %w", name, path, err)
	}
	return nil
}

// opGroup is a supervisor's nexus-ops/<id> group.
type opGroup struct {
	path string
	work string // path/work once made
}

// linuxOwnCgroup is the group this supervisor of dir was started in, or nil
// when it is not in nexus-ops/<id> of its operation.
func linuxOwnCgroup(dir string) processCgroup {
	self, err := selfPath()
	if err != nil || self != "/"+opsCgroup+"/"+filepath.Base(dir) {
		return nil
	}
	return &opGroup{path: filepath.Join(supervisorCgroupRoot, self)}
}

func (g *opGroup) makeWork() error {
	work := filepath.Join(g.path, workCgroup)
	if err := os.Mkdir(work, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create cgroup %s: %w", work, err)
	}
	g.work = work
	return nil
}

// start starts the child in the work group: cloned into it or, when that
// fails, forked while this supervisor is in the group itself, so the child
// is there before it runs anything, and the supervisor moves back after.
func (g *opGroup) start(newCmd func() *exec.Cmd) (cmd *exec.Cmd, notPlaced, err error) {
	cmd, cloneErr := cloneInto(newCmd, g.work)
	if cloneErr == nil {
		return cmd, nil, nil
	}
	if err := moveTo(g.work, os.Getpid()); err != nil {
		cmd = newCmd()
		return cmd, errors.Join(cloneErr, err), cmd.Start()
	}
	cmd = newCmd()
	err = cmd.Start()
	if berr := moveTo(g.path, os.Getpid()); berr != nil {
		// A supervisor left in work would be killed with the child's
		// leftovers, so the operation does not go ahead.
		if err == nil {
			_ = cmd.Process.Kill() // Wait below reports nothing useful about a SIGKILL we sent
			_ = cmd.Wait()
		}
		return nil, nil, fmt.Errorf("leave the work cgroup: %w", berr)
	}
	return cmd, nil, err
}

func (g *opGroup) kill() error { return killCgroup(g.work) }

func (g *opGroup) signal(sig syscall.Signal) error { return signalAll(g.work, sig) }

func (g *opGroup) populated() bool { return eventIs(g.work, "populated") }

// release removes work, moves this supervisor to nexus-ops so its own
// group has no process left, and removes that group.
func (g *opGroup) release() error {
	var errs []error
	if g.work != "" {
		if err := syscall.Rmdir(g.work); err != nil && !errors.Is(err, syscall.ENOENT) {
			errs = append(errs, fmt.Errorf("remove cgroup %s: %w", g.work, err))
		}
	}
	if err := moveTo(filepath.Dir(g.path), os.Getpid()); err != nil {
		return errors.Join(append(errs, err)...)
	}
	if err := syscall.Rmdir(g.path); err != nil && !errors.Is(err, syscall.ENOENT) {
		errs = append(errs, fmt.Errorf("remove cgroup %s: %w", g.path, err))
	}
	return errors.Join(errs...)
}
