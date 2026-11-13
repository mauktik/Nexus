package detach

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// ProcessConfig configures the process backend: detached operations run by
// a supervisor process in its own session instead of a systemd unit (FR-20).
type ProcessConfig struct {
	// OpsDir is the launcher's Config.OpsDir. UnitState, Signal and
	// ListUnits find an operation's supervisor.json there, so a backend
	// created after a restart of nexusd knows the supervisors of the one
	// before. Required.
	OpsDir string
	// Executable is the nexusd binary the supervisor runs. Empty means
	// os.Executable().
	Executable string
	// CgroupRoot is the cgroup v2 mount. Empty means /sys/fs/cgroup; "-"
	// disables containment (tests without root).
	CgroupRoot string
	ProcRoot   string       // Empty means /proc
	Logger     *slog.Logger // Nil means slog.Default()
}

// Files of a supervisor started by the process backend, in the operation's
// directory.
const (
	// IdentityFile names the operation's supervisor process (0600).
	IdentityFile = "supervisor.json"
	// SupervisorLog takes the supervisor's own stdout and stderr (0600), its
	// diagnostics, where a systemd unit has the journal.
	SupervisorLog = "supervisor.log"
)

// Identity is a supervisor process as the process backend records it. A PID
// alone can be reused: the start time, in clock ticks since boot, and the
// boot it belongs to make it a process that existed once only.
type Identity struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"startTicks"`
	BootID     string `json:"bootId"`
}

// WriteIdentity writes id as dir/supervisor.json, mode 0600, by atomic
// rename.
func WriteIdentity(dir string, id Identity) error {
	b, err := json.Marshal(id)
	if err != nil {
		return fmt.Errorf("detach: encode identity: %w", err)
	}
	return writeAtomic(dir, IdentityFile, b)
}

// ReadIdentity reads dir/supervisor.json. The error wraps fs.ErrNotExist
// when there is none.
func ReadIdentity(dir string) (Identity, error) {
	b, err := os.ReadFile(filepath.Join(dir, IdentityFile))
	if err != nil {
		return Identity{}, fmt.Errorf("detach: read identity: %w", err)
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return Identity{}, fmt.Errorf("detach: decode identity %s: %w", filepath.Join(dir, IdentityFile), err)
	}
	if id.PID <= 0 || id.BootID == "" {
		return Identity{}, fmt.Errorf("detach: identity %s has no pid or boot id", filepath.Join(dir, IdentityFile))
	}
	return id, nil
}

// bootID is the current boot's ID, which changes on every boot.
func bootID(procRoot string) (string, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", fmt.Errorf("detach: read boot id: %w", err)
	}
	id := string(bytes.TrimSpace(b))
	if id == "" {
		return "", errors.New("detach: empty boot id")
	}
	return id, nil
}

// statFields are the fields of /proc/<pid>/stat the process backend uses
// (proc_pid_stat(5)).
type statFields struct {
	State      byte   // Field 3: R, S, D, Z, X, ...
	PPID       int    // Field 4
	Session    int    // Field 6
	StartTicks uint64 // Field 22: start time in clock ticks after boot
}

// procStat reads process pid's state and start time. The error wraps
// fs.ErrNotExist when there is no such process.
func procStat(procRoot string, pid int) (statFields, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return statFields{}, fmt.Errorf("detach: read stat of %d: %w", pid, err)
	}
	f, err := parseStat(b)
	if err != nil {
		return statFields{}, fmt.Errorf("detach: stat of %d: %w", pid, err)
	}
	return f, nil
}

// parseStat parses a /proc/<pid>/stat line. The comm field (2) is in
// parentheses and may hold anything, spaces and parentheses included, so
// fields are counted from the last ')' of the line: the field after it is
// field 3.
func parseStat(b []byte) (statFields, error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return statFields{}, errors.New("no comm field")
	}
	rest := bytes.Fields(b[i+1:]) // rest[0] is field 3
	const need = 22 - 2
	if len(rest) < need {
		return statFields{}, fmt.Errorf("%d fields after comm, want at least %d", len(rest), need)
	}
	if len(rest[0]) != 1 {
		return statFields{}, fmt.Errorf("bad state %q", rest[0])
	}
	ppid, err := strconv.Atoi(string(rest[4-3]))
	if err != nil {
		return statFields{}, fmt.Errorf("bad ppid: %w", err)
	}
	session, err := strconv.Atoi(string(rest[6-3]))
	if err != nil {
		return statFields{}, fmt.Errorf("bad session: %w", err)
	}
	start, err := strconv.ParseUint(string(rest[22-3]), 10, 64)
	if err != nil {
		return statFields{}, fmt.Errorf("bad start time: %w", err)
	}
	return statFields{State: rest[0][0], PPID: ppid, Session: session, StartTicks: start}, nil
}

// checkIdentity reports whether operation directory dir's supervisor is
// alive. mismatch is empty when it is, and otherwise says why it is
// definitely not: no supervisor.json, another boot, no such process, another
// process with the same PID, or a process that has ended (zombie or dead).
// An error means unknown, never absent.
func checkIdentity(procRoot, dir string) (id Identity, mismatch string, err error) {
	id, err = ReadIdentity(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Identity{}, "no " + IdentityFile, nil
	}
	if err != nil {
		return Identity{}, "", err
	}
	boot, err := bootID(procRoot)
	if err != nil {
		return id, "", err
	}
	if id.BootID != boot {
		return id, fmt.Sprintf("boot id %s is not the current boot %s", id.BootID, boot), nil
	}
	st, err := procStat(procRoot, id.PID)
	if errors.Is(err, fs.ErrNotExist) {
		return id, fmt.Sprintf("process %d does not exist", id.PID), nil
	}
	if err != nil {
		return id, "", err
	}
	if st.StartTicks != id.StartTicks {
		return id, fmt.Sprintf("process %d started at tick %d, not %d", id.PID, st.StartTicks, id.StartTicks), nil
	}
	if st.State == 'Z' || st.State == 'X' {
		return id, fmt.Sprintf("process %d has ended (state %c)", id.PID, st.State), nil
	}
	return id, "", nil
}
