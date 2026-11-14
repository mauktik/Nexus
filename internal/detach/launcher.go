package detach

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Paths of the systemd tools. They are absolute so the daemon never
// resolves them through PATH.
const (
	SystemdRun = "/usr/bin/systemd-run"
	Systemctl  = "/usr/bin/systemctl"
)

// UnitPrefix starts the name of every operation unit.
const UnitPrefix = "nexus-op-"

// LaunchTimeout bounds one launch: systemd-run with --service-type=exec
// returns once the supervisor has been executed.
const LaunchTimeout = 30 * time.Second

// Unit is the name of operation id's unit.
func Unit(id string) string { return UnitPrefix + id + ".service" }

// Argv is the exact systemd-run invocation of operation id (architecture,
// Transient unit invocation). It carries only the kill mode of spec: the
// environment and every other value travel in the 0600 spec.json, never in
// --setenv or Environment=, which systemctl show would expose (FR-18).
func Argv(id, executable, opsDir string, spec Spec) []string {
	return []string{
		SystemdRun,
		"--unit=" + UnitPrefix + id,
		"--collect",
		"--quiet",
		"--service-type=exec",
		"--description=Nexus operation " + id,
		"--property=RefuseManualStop=yes",
		"--property=KillMode=" + string(spec.KillMode),
		"--",
		executable, "op-supervise", "--dir", filepath.Join(opsDir, id),
	}
}

// Invocation is one launch: the unit to start and the exact command line
// that starts it.
type Invocation struct {
	ID   string
	Unit string   // nexus-op-<id>.service
	Dir  string   // The operation's directory, holding spec.json
	Argv []string // From Argv
}

// UnitState is a unit's ActiveState and LoadState, as systemctl show
// reports them.
type UnitState struct {
	ActiveState string
	LoadState   string
}

// Liveness is what a UnitState says about the operation's supervisor.
type Liveness int

const (
	Live     Liveness = iota + 1 // Active, or on its way in or out
	Inactive                     // Stopped or failed
	Absent                       // No such unit (LoadState not-found)
)

// Liveness classifies s. A state it does not know is an error, never a
// definite answer.
func (s UnitState) Liveness() (Liveness, error) {
	if s.LoadState == "not-found" {
		return Absent, nil
	}
	switch s.ActiveState {
	case "active", "activating", "deactivating", "reloading", "refreshing":
		return Live, nil
	case "inactive", "failed", "maintenance":
		return Inactive, nil
	}
	return 0, fmt.Errorf("detach: unknown unit state ActiveState=%q LoadState=%q", s.ActiveState, s.LoadState)
}

// Config is where operations live and what the unit runs.
type Config struct {
	// OpsDir holds one directory per operation. Required.
	OpsDir string
	// Executable is the nexusd binary the unit runs. Empty means
	// os.Executable().
	Executable string
	Logger     *slog.Logger // Nil means slog.Default()
}

// Options are the seams of New. Each nil field selects the real systemd
// implementation. A T0 test takes all three process seams from
// detachtest.Local, never Launch alone, so a nil UnitState can never reach
// a real systemctl on a development machine.
type Options struct {
	// Launch starts inv's unit. Nil runs inv.Argv: the real systemd-run.
	Launch func(ctx context.Context, inv Invocation) error
	// UnitState reports a unit's state. Nil runs systemctl show.
	UnitState func(ctx context.Context, unit string) (UnitState, error)
	// Signal sends sig to a unit's main process; CancelRunning calls it.
	// Nil runs systemctl kill --kill-whom=main --signal=<sig>.
	Signal func(ctx context.Context, unit string, sig syscall.Signal) error
	// ListUnits lists the operation units systemd has loaded, by name, for
	// restart recovery's orphan check. Nil runs systemctl list-units.
	ListUnits func(ctx context.Context) ([]string, error)
	// Now is the clock of MaxRuntime. Nil means time.Now.
	Now func() time.Time
	// PollInterval is how often the watcher reads exit.json. Zero, in
	// production, means the PollInterval constant; tests shorten it.
	PollInterval time.Duration
}

// Launcher starts operation units, signals them and watches them to their
// end.
type Launcher struct {
	opsDir    string
	exe       string
	log       *slog.Logger
	launch    func(context.Context, Invocation) error
	unitState func(context.Context, string) (UnitState, error)
	signal    func(context.Context, string, syscall.Signal) error
	listUnits func(context.Context) ([]string, error)
	now       func() time.Time
	pollEvery time.Duration
	real      bool

	mu       sync.Mutex
	watched  map[string]*watched
	watching bool // The watch loop runs
}

// New returns the launcher. There is no second constructor: tests inject
// through opts.
func New(cfg Config, opts Options) (*Launcher, error) {
	if cfg.OpsDir == "" {
		return nil, errors.New("detach: no ops directory")
	}
	l := &Launcher{
		opsDir: cfg.OpsDir, exe: cfg.Executable, log: cfg.Logger,
		launch: opts.Launch, unitState: opts.UnitState, signal: opts.Signal, listUnits: opts.ListUnits, now: opts.Now,
		watched: map[string]*watched{},
	}
	if l.exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("detach: locate nexusd: %w", err)
		}
		l.exe = exe
	}
	if l.log == nil {
		l.log = slog.Default()
	}
	if l.launch == nil {
		l.launch, l.real = systemdRun, true
	}
	if l.unitState == nil {
		l.unitState = systemctlShow
	}
	if l.signal == nil {
		l.signal = systemctlKill
	}
	if l.listUnits == nil {
		l.listUnits = systemctlListUnits
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.pollEvery = opts.PollInterval; l.pollEvery <= 0 {
		l.pollEvery = PollInterval
	}
	return l, nil
}

// RealLauncher reports whether Launch is the real systemd-run, that is,
// Options.Launch was nil.
func (l *Launcher) RealLauncher() bool { return l.real }

// Dir is operation id's directory.
func (l *Launcher) Dir(id string) string { return filepath.Join(l.opsDir, id) }

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

// Start writes spec as the operation's 0600 spec.json and launches its
// unit. The caller commits Running first, so a unit never exists without a
// Running record, and drops its copy of spec afterwards. On failure no unit
// was started and spec.json is gone. A launch that timed out is not a
// failure while its unit may exist (see mayHaveStarted).
func (l *Launcher) Start(ctx context.Context, id string, spec Spec) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("detach: invalid operation id %q", id)
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	dir := l.Dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("detach: create %s: %w", dir, err)
	}
	if err := WriteSpec(dir, spec); err != nil {
		return err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, LaunchTimeout)
	defer cancel()
	inv := Invocation{ID: id, Unit: Unit(id), Dir: dir, Argv: Argv(id, l.exe, l.opsDir, spec)}
	if err := l.launch(ctx, inv); err != nil {
		err = fmt.Errorf("detach: launch %s: %w", inv.Unit, err)
		if ctx.Err() != nil && l.mayHaveStarted(parent, id) {
			// A systemd-run that timed out or was killed says nothing of
			// the job PID 1 holds, so the unit may run: the caller watches
			// it, and spec.json stays for the supervisor to read.
			l.log.Warn("launch did not report; the unit may be running, so it is watched",
				"operationId", id, "unit", inv.Unit, "error", err.Error())
			return nil
		}
		// The spec may hold sensitive values and nothing will read it.
		if rerr := os.Remove(filepath.Join(dir, SpecFile)); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		return err
	}
	return nil
}

// mayHaveStarted reports whether operation id's unit may exist after a
// launch that did not finish: its exit.json is there, its state is anything
// but definitely absent (a unit that already ended counts: the watcher
// reports it), or its state is unknown. Only a definite absent
// answer is a failed launch.
func (l *Launcher) mayHaveStarted(ctx context.Context, id string) bool {
	if _, ok, err := ReadExit(l.Dir(id)); ok || err != nil {
		return true
	}
	lv, err := l.State(context.WithoutCancel(ctx), id)
	return err != nil || lv != Absent
}

// Cancel sends SIGTERM, the supervisor's cancel signal, to operation id's
// unit. The supervisor forwards it to the child's process group and
// escalates to SIGKILL after the grace period, even if nexusd restarts
// meanwhile.
func (l *Launcher) Cancel(ctx context.Context, id string) error {
	return l.signal(ctx, Unit(id), syscall.SIGTERM)
}

// State reports operation id's unit state with the StateTimeout bound. An
// error or a timeout means unknown, never absent.
func (l *Launcher) State(ctx context.Context, id string) (Liveness, error) {
	ctx, cancel := context.WithTimeout(ctx, StateTimeout)
	defer cancel()
	st, err := l.unitState(ctx, Unit(id))
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return st.Liveness()
}

// Units lists the loaded operation units with the StateTimeout bound.
func (l *Launcher) Units(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, StateTimeout)
	defer cancel()
	units, err := l.listUnits(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return units, nil
}

// systemdRun is the real launcher: it runs inv.Argv.
func systemdRun(ctx context.Context, inv Invocation) error {
	out, err := exec.CommandContext(ctx, inv.Argv[0], inv.Argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(inv.Argv[0]), err, bytes.TrimSpace(out))
	}
	return nil
}

// systemctlShow is the real unit-state query. systemctl show prints one
// Property=value line per property, and reports LoadState=not-found for a
// unit that does not exist, such as a collected one.
func systemctlShow(ctx context.Context, unit string) (UnitState, error) {
	out, err := exec.CommandContext(ctx, Systemctl, "show", "-p", "ActiveState", "-p", "LoadState", "--", unit).Output()
	if err != nil {
		return UnitState{}, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	var st UnitState
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			st.ActiveState = v
		case "LoadState":
			st.LoadState = v
		}
	}
	if err := sc.Err(); err != nil {
		return UnitState{}, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	if st.ActiveState == "" && st.LoadState == "" {
		return UnitState{}, fmt.Errorf("systemctl show %s: no state in %q", unit, out)
	}
	return st, nil
}

// systemctlListUnits is the real unit lister. list-units --all shows every
// unit systemd has in memory whose primary name matches the glob, and
// --plain omits the bullet circles; any header or footer line is skipped
// because no field of it is an operation unit name. A host without systemd
// has no units.
func systemctlListUnits(ctx context.Context) ([]string, error) {
	if !SystemdAvailable().Available {
		return nil, nil
	}
	out, err := exec.CommandContext(ctx, Systemctl, "list-units", "--all", "--plain", "--full", "--", UnitPrefix+"*").Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-units: %w", err)
	}
	return unitNames(out), nil
}

var unitName = regexp.MustCompile(`^` + UnitPrefix + `[A-Za-z0-9][A-Za-z0-9-]*\.service$`)

// unitNames picks the operation unit names out of list-units output.
func unitNames(out []byte) []string {
	var units []string
	for line := range strings.Lines(string(out)) {
		for _, f := range strings.Fields(line) {
			if unitName.MatchString(f) {
				units = append(units, f)
				break
			}
		}
	}
	return units
}

// systemctlKill is the real signal: it reaches only the unit's main
// process, the supervisor, which owns the escalation to the child's group.
func systemctlKill(ctx context.Context, unit string, sig syscall.Signal) error {
	name, ok := signalNames[sig]
	if !ok {
		return fmt.Errorf("systemctl kill %s: unsupported signal %d", unit, int(sig))
	}
	out, err := exec.CommandContext(ctx, Systemctl, "kill", "--kill-whom=main", "--signal="+name, "--", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl kill %s: %w: %s", unit, err, bytes.TrimSpace(out))
	}
	return nil
}

// Availability is whether detached operations can run on this host.
type Availability struct {
	Available bool
	Reason    string // Why not, when not available
}

// SystemdAvailable reports whether this host runs systemd with the tools
// the launcher needs. Providers whose operations are detached report
// unavailable with the reason when it is not: there is no fallback to a
// plain child process, which would die with nexusd (CON-4).
func SystemdAvailable() Availability { return systemdAt(runtime.GOOS, "/") }

// systemdAt checks under root, so a test can lay out a fake system. The
// booted-with-systemd test is the one sd_booted(3) uses: /run/systemd/system
// exists as a directory.
func systemdAt(goos, root string) Availability {
	if goos != "linux" {
		return Availability{Reason: fmt.Sprintf("detached operations need systemd, which %s does not have", goos)}
	}
	if fi, err := os.Stat(filepath.Join(root, "run", "systemd", "system")); err != nil || !fi.IsDir() {
		return Availability{Reason: "the host was not booted with systemd (/run/systemd/system is missing)"}
	}
	for _, tool := range []string{SystemdRun, Systemctl} {
		fi, err := os.Stat(filepath.Join(root, tool))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			return Availability{Reason: fmt.Sprintf("%s is missing or not executable", tool)}
		}
	}
	return Availability{Available: true}
}
