package nginx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/providers/nginx/spec"
)

// Reload verification (FR-6, ASM-7). An init system's reload exit status
// does not say whether nginx applied the configuration: Ubuntu's unit runs
// `nginx -s reload`, which exits 0 when the master later fails to bind, and
// a HUP-style unit exits 0 whatever happens. The source of truth is the
// worker generation in /proc: a master that applied a configuration starts
// new workers and tells the old ones to shut down; one that rejected it
// keeps its workers as they were (testdata/reload).

// DefaultReloadPoll is how often Verify reads /proc (L-28).
const DefaultReloadPoll = 100 * time.Millisecond

// The process titles nginx sets, as captured on Ubuntu and Alpine, and the
// suffix the kernel appends to an exe link whose file was unlinked.
const (
	masterTitlePrefix = "nginx: master process"
	workerTitle       = "nginx: worker process"
	shuttingDownTitle = "nginx: worker process is shutting down"
	exeDeletedSuffix  = " (deleted)"
)

// The reload verdicts, as reported in spec.ReloadObservation.Verdict.
const (
	VerdictConfirmed   = "confirmed"
	VerdictRejected    = "rejected"
	VerdictUnconfirmed = "unconfirmed"
	VerdictMasterGone  = "master-gone"
)

// ErrMasterGone is Observe's error when the master no longer has the
// recorded (pid, startTicks): it exited, or its PID now names another
// process.
var ErrMasterGone = errors.New("the nginx master process is gone")

// Proc is one process as /proc/<pid>/stat and cmdline show it.
type Proc struct {
	PID        int
	PPID       int
	State      byte   // stat field 3: R, S, D, Z, X, ...
	StartTicks uint64 // stat field 22: start time in clock ticks after boot
	Title      string // cmdline with NULs as spaces, trailing spaces trimmed
}

// ended reports whether p has exited and is not yet reaped, or is dead.
func (p Proc) ended() bool { return p.State == 'Z' || p.State == 'X' }

// ProcReader reads the host's processes.
type ProcReader interface {
	// Snapshot returns every process, keyed by PID. Processes that exit
	// while it scans are left out, not reported as errors.
	Snapshot() (map[int]Proc, error)
	// Exe returns the target of /proc/<pid>/exe exactly as the kernel
	// reports it, including a " (deleted)" suffix. The error wraps
	// fs.ErrNotExist when there is no such process.
	Exe(pid int) (string, error)
	// Owner returns the UID that owns process pid: the owner of
	// /proc/<pid>. The error wraps fs.ErrNotExist when there is no such
	// process.
	Owner(pid int) (uint32, error)
}

// Master is an identified nginx master process.
type Master struct {
	PID        int
	StartTicks uint64
}

// Worker is one nginx worker process.
type Worker struct {
	PID        int
	StartTicks uint64
}

// Generation is a master and its workers at one moment.
type Generation struct {
	Master  Master
	Workers []Worker // sorted by PID
}

// ValidateMaster identifies pid, the init system's main PID, as the nginx
// master (Data Flow 3 step 2). Its exe must be nginxPath, or nginxPath with
// the kernel's " (deleted)" suffix, which is what the link shows after a
// package upgrade replaced the binary without restarting nginx (L-53), its
// title must start "nginx: master process", and it must be owned by root
// (managedUID): on OpenRC the pid comes from a pidfile in a directory the
// nginx user owns, and a master that user started itself must not be taken
// for the one the init system reloads. The error's text is the reason,
// which the caller puts in the dependency-unavailable detail.
func ValidateMaster(r ProcReader, pid int, nginxPath string) (Master, error) {
	if pid <= 0 {
		return Master{}, fmt.Errorf("the init system reports no main pid (%d)", pid)
	}
	snap, err := r.Snapshot()
	if err != nil {
		return Master{}, fmt.Errorf("cannot read /proc: %w", err)
	}
	p, ok := snap[pid]
	if !ok || p.ended() {
		return Master{}, fmt.Errorf("main pid %d is gone", pid)
	}
	exe, err := r.Exe(pid)
	if errors.Is(err, fs.ErrNotExist) {
		return Master{}, fmt.Errorf("main pid %d is gone", pid)
	}
	if err != nil {
		return Master{}, fmt.Errorf("cannot read the exe of main pid %d: %w", pid, err)
	}
	if exe != nginxPath && exe != nginxPath+exeDeletedSuffix {
		return Master{}, fmt.Errorf("the exe of main pid %d is %q, not %q", pid, exe, nginxPath)
	}
	if !strings.HasPrefix(p.Title, masterTitlePrefix) {
		return Master{}, fmt.Errorf("the title of main pid %d is %q, not an nginx master process", pid, p.Title)
	}
	uid, err := r.Owner(pid)
	if errors.Is(err, fs.ErrNotExist) {
		return Master{}, fmt.Errorf("main pid %d is gone", pid)
	}
	if err != nil {
		return Master{}, fmt.Errorf("cannot read the owner of main pid %d: %w", pid, err)
	}
	if uid != managedUID {
		return Master{}, fmt.Errorf("main pid %d is owned by uid %d, not uid %d (root): it is not the master the "+
			"init system started", pid, uid, managedUID)
	}
	return Master{PID: pid, StartTicks: p.StartTicks}, nil
}

// masterAlive reports whether m is in snap with its recorded start time.
func masterAlive(m Master, snap map[int]Proc) bool {
	p, ok := snap[m.PID]
	return ok && p.StartTicks == m.StartTicks && !p.ended()
}

// workersOf returns m's direct children titled exactly "nginx: worker
// process", sorted by PID. Cache managers, cache loaders, workers that are
// shutting down and the workers of another master are left out.
func workersOf(m Master, snap map[int]Proc) []Worker {
	var ws []Worker
	for _, p := range snap {
		if p.PPID == m.PID && p.Title == workerTitle && !p.ended() {
			ws = append(ws, Worker{PID: p.PID, StartTicks: p.StartTicks})
		}
	}
	slices.SortFunc(ws, func(a, b Worker) int { return a.PID - b.PID })
	return ws
}

// Observe takes the generation of m before a reload (Data Flow 3 step 3).
// It returns ErrMasterGone when m no longer has its recorded (pid,
// startTicks), in which case no reload must be sent.
func Observe(r ProcReader, m Master) (Generation, error) {
	snap, err := r.Snapshot()
	if err != nil {
		return Generation{Master: m}, fmt.Errorf("cannot read /proc: %w", err)
	}
	if !masterAlive(m, snap) {
		return Generation{Master: m}, ErrMasterGone
	}
	return Generation{Master: m, Workers: workersOf(m, snap)}, nil
}

// Classification is the verdict of one snapshot against the generation
// taken before the reload.
type Classification struct {
	// Verdict is confirmed or master-gone, which are final at once, or
	// what the reload is if nothing changes any more: rejected when no new
	// worker exists and every old worker is unchanged, and unconfirmed for
	// anything else.
	Verdict string
	// NewWorkers are the master's workers that are not in the old
	// generation.
	NewWorkers []Worker
}

// Classify applies the rules of Data Flow 3 step 5 to one snapshot. It is
// pure: the same inputs give the same result.
//
//   - master-gone: the master exited or its PID names another process.
//   - confirmed: at least one new worker, and every old worker has exited
//     or is titled "nginx: worker process is shutting down".
//   - rejected: no new worker, and every old worker is unchanged.
//   - unconfirmed: anything else, such as a worker that crashed and was
//     respawned by the master while the others stayed.
func Classify(before Generation, snap map[int]Proc) Classification {
	m := before.Master
	if !masterAlive(m, snap) {
		return Classification{Verdict: VerdictMasterGone}
	}
	old := make(map[Worker]bool, len(before.Workers))
	for _, w := range before.Workers {
		old[w] = true
	}
	var fresh []Worker
	for _, w := range workersOf(m, snap) {
		if !old[w] {
			fresh = append(fresh, w)
		}
	}
	allRetired, allUnchanged := true, true
	for _, w := range before.Workers {
		p, ok := snap[w.PID]
		alive := ok && p.StartTicks == w.StartTicks && p.PPID == m.PID && !p.ended()
		switch {
		case !alive:
			allUnchanged = false
		case p.Title == shuttingDownTitle:
			allUnchanged = false
		case p.Title == workerTitle:
			allRetired = false
		default:
			allRetired, allUnchanged = false, false
		}
	}
	c := Classification{NewWorkers: fresh}
	switch {
	case len(fresh) > 0 && allRetired:
		c.Verdict = VerdictConfirmed
	case len(fresh) == 0 && allUnchanged:
		c.Verdict = VerdictRejected
	default:
		c.Verdict = VerdictUnconfirmed
	}
	return c
}

func workerPIDs(ws []Worker) []int {
	pids := make([]int, 0, len(ws))
	for _, w := range ws {
		pids = append(pids, w.PID)
	}
	return pids
}

// Verify watches the generation after the init system's reload returned
// exitCode (Data Flow 3 steps 4 to 6). It classifies a snapshot at once and
// then every poll (DefaultReloadPoll when poll is not positive) until the
// verdict is confirmed or master-gone, or until timeout has passed, when
// the last snapshot's verdict, rejected or unconfirmed, is final. When
// exitCode is positive (the tool reported a failure) and the first snapshot
// shows an unchanged generation, the verdict is rejected at once, without
// waiting. A negative exitCode is a tool that ended without an exit status
// (it may have delivered the reload before it died): it does not end the
// wait early.
//
// The observation is complete whatever the outcome. The error is set only
// when /proc could not be read or ctx ended; the verdict is then
// unconfirmed, because a reload was sent and its effect is unknown.
func Verify(ctx context.Context, r ProcReader, before Generation, exitCode int, timeout, poll time.Duration) (spec.ReloadObservation, error) {
	if poll <= 0 {
		poll = DefaultReloadPoll
	}
	obs := spec.ReloadObservation{
		MasterPID:    before.Master.PID,
		OldWorkers:   workerPIDs(before.Workers),
		NewWorkers:   []int{},
		InitExitCode: exitCode,
	}
	deadline := time.Now().Add(timeout)
	for first := true; ; first = false {
		snap, err := r.Snapshot()
		if err != nil {
			obs.Verdict = VerdictUnconfirmed
			return obs, fmt.Errorf("cannot read /proc: %w", err)
		}
		c := Classify(before, snap)
		obs.NewWorkers = workerPIDs(c.NewWorkers)
		switch {
		case c.Verdict == VerdictConfirmed, c.Verdict == VerdictMasterGone:
			obs.Verdict = c.Verdict
			return obs, nil
		case first && exitCode > 0 && c.Verdict == VerdictRejected:
			obs.Verdict = VerdictRejected
			return obs, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			obs.Verdict = c.Verdict
			return obs, nil
		}
		t := time.NewTimer(min(poll, left))
		select {
		case <-ctx.Done():
			t.Stop()
			obs.Verdict = VerdictUnconfirmed
			return obs, ctx.Err()
		case <-t.C:
		}
	}
}

// procDir reads a /proc-shaped directory: /proc itself on Linux
// (procfs_linux.go), or a fake root in tests. It only reads files, so it
// builds on every platform.
type procDir string

func (d procDir) Snapshot() (map[int]Proc, error) {
	ents, err := os.ReadDir(string(d))
	if err != nil {
		return nil, err
	}
	snap := make(map[int]Proc, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, err := d.proc(pid)
		if vanished(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		snap[pid] = p
	}
	return snap, nil
}

// vanished reports whether err is what reading a process that exited
// during the scan gives.
func vanished(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

func (d procDir) proc(pid int) (Proc, error) {
	dir := filepath.Join(string(d), strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return Proc{}, err
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return Proc{}, err
	}
	p, err := parseProcStat(stat)
	if err != nil {
		return Proc{}, fmt.Errorf("stat of %d: %w", pid, err)
	}
	if p.PID != pid {
		return Proc{}, fmt.Errorf("stat of %d names pid %d", pid, p.PID)
	}
	p.Title = procTitle(cmdline)
	return p, nil
}

func (d procDir) Exe(pid int) (string, error) {
	return os.Readlink(filepath.Join(string(d), strconv.Itoa(pid), "exe"))
}

// Owner is the owner of <dir>/<pid>, which /proc sets to the process's
// effective UID.
func (d procDir) Owner(pid int) (uint32, error) {
	fi, err := os.Stat(filepath.Join(string(d), strconv.Itoa(pid)))
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no owner in the stat of process %d", pid)
	}
	return st.Uid, nil
}

// parseProcStat parses a /proc/<pid>/stat line (proc_pid_stat(5)). The comm
// field (2) is in parentheses and may hold anything, spaces and
// parentheses included, so fields are counted from the last ')' of the
// line: the field after it is field 3.
func parseProcStat(b []byte) (Proc, error) {
	open := bytes.IndexByte(b, '(')
	i := bytes.LastIndexByte(b, ')')
	if open < 0 || i < open {
		return Proc{}, errors.New("no comm field")
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(b[:open])))
	if err != nil {
		return Proc{}, fmt.Errorf("bad pid: %w", err)
	}
	rest := bytes.Fields(b[i+1:]) // rest[0] is field 3
	const need = 22 - 2
	if len(rest) < need {
		return Proc{}, fmt.Errorf("%d fields after comm, want at least %d", len(rest), need)
	}
	if len(rest[0]) != 1 {
		return Proc{}, fmt.Errorf("bad state %q", rest[0])
	}
	ppid, err := strconv.Atoi(string(rest[4-3]))
	if err != nil {
		return Proc{}, fmt.Errorf("bad ppid: %w", err)
	}
	start, err := strconv.ParseUint(string(rest[22-3]), 10, 64)
	if err != nil {
		return Proc{}, fmt.Errorf("bad start time: %w", err)
	}
	return Proc{PID: pid, PPID: ppid, State: rest[0][0], StartTicks: start}, nil
}

// procTitle turns a raw cmdline into a title. nginx overwrites its argv
// with its title and pads the rest of that memory with NULs, so its
// cmdline is "nginx: worker process\x00\x00…"; another process's is its
// arguments separated by NULs.
func procTitle(cmdline []byte) string {
	return strings.TrimRight(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})), " ")
}
