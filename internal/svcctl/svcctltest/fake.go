// Package svcctltest is the T0 fake of the init systems behind svcctl: an
// Options.Run that models nginx under systemd or OpenRC and answers every
// call with outputs and exit codes captured from real systems
// (internal/svcctl/testdata, provenance in its README.md).
//
// The fake encodes the behaviors a caller must not paper over:
//   - systemd `reload` exits 0 when the config parses, whatever the master
//     does afterwards: a bind conflict leaves the old generation loaded and
//     still exits 0. Under ReloadModel HUP (nginx.org's unit, which only
//     signals the master) it exits 0 even for an invalid config, with the
//     generation unchanged;
//   - OpenRC `reload` runs the config test first: an invalid config exits
//     non-zero with the configtest text, while a bind conflict exits 0 with
//     the generation unchanged;
//   - OpenRC `start` of a started (or crashed) service prints a warning, exits
//     0 and changes nothing.
//
// It is strict about how it is called: argv must be exactly one of the
// shapes svcctl uses, starting with the absolute tool path. A relative tool,
// a shell, or an environment assignment smuggled into argv (`env X=1 ...`,
// a trailing `X=1`) is none of them, and fails the test through T.
package svcctltest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/svcctl"
)

// T is the part of testing.TB the fake reports misuse through.
type T interface {
	Helper()
	Errorf(format string, args ...any)
}

// Config is the nginx configuration on disk in the model.
type Config string

const (
	// Valid parses and is applied.
	Valid Config = "valid"
	// Invalid fails `nginx -t` (an unknown directive).
	Invalid Config = "invalid"
	// BindConflict parses, but the master cannot bind one of its listen
	// sockets when it applies it.
	BindConflict Config = "bind-conflict"
)

// Service is the one service the fake models.
const Service = "nginx"

// ReloadModel is how the systemd unit's ExecReload reloads nginx.
type ReloadModel string

const (
	// SignalParse is Ubuntu's `nginx -s reload`: the config is parsed first,
	// so an invalid one exits 1. The default.
	SignalParse ReloadModel = "signal-parse"
	// HUP is nginx.org's `kill -s HUP $(cat nginx.pid)`: the master is only
	// signalled, so `systemctl reload` exits 0 whatever the config.
	HUP ReloadModel = "hup"
)

// Model is the modeled nginx.
type Model struct {
	Absent bool         // The init system does not know nginx
	State  svcctl.State // Running, Stopped or Failed; "" = Stopped
	Config Config       // "" = Valid
	// ReloadModel selects the systemd ExecReload; "" = SignalParse. OpenRC
	// always runs `nginx -t` first and ignores it.
	ReloadModel ReloadModel
	// Generation counts the configurations a master has loaded: it advances
	// on every successful start, restart and reload, and on nothing else.
	Generation int
	// MainPID replaces the MainPID of the systemd running capture when
	// non-zero. (On OpenRC svcctl reads the PID from RunRoot; see
	// WriteDaemonRecord.)
	MainPID int
}

// Call is one recorded call.
type Call struct {
	Argv   []string
	Result svcctl.Result
	Err    error
}

// Fake is an svcctl.Options.Run. Its methods are safe for concurrent use.
type Fake struct {
	t    T
	init platform.InitSystem

	mu     sync.Mutex
	model  Model
	status *svcctl.Result // Override of the status query
	block  bool
	err    error
	calls  []Call
}

// New returns a fake of init (platform.Systemd or platform.OpenRC) with
// nginx in state m.
func New(t T, init platform.InitSystem, m Model) *Fake {
	t.Helper()
	if init != platform.Systemd && init != platform.OpenRC {
		t.Errorf("svcctltest: no fake for init system %q", init)
	}
	if _, err := loadFixtures(); err != nil {
		t.Errorf("svcctltest: %v", err)
	}
	return &Fake{t: t, init: init, model: normalize(m)}
}

func normalize(m Model) Model {
	if m.State == "" {
		m.State = svcctl.Stopped
	}
	if m.Config == "" {
		m.Config = Valid
	}
	if m.ReloadModel == "" {
		m.ReloadModel = SignalParse
	}
	return m
}

// Model returns the current model.
func (f *Fake) Model() Model {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.model
}

// Set changes the model.
func (f *Fake) Set(change func(*Model)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.model)
	f.model = normalize(f.model)
}

// SetStatus makes the status query (`systemctl show` or `rc-service -C nginx
// status`) answer out with exit code exit instead of the model's capture.
func (f *Fake) SetStatus(out string, exit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = &svcctl.Result{ExitCode: exit, Output: []byte(out)}
}

// SetBlock makes every call block until its context is done, then return
// the context's error.
func (f *Fake) SetBlock(block bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = block
}

// SetErr makes every call return err, as an exec failure would.
func (f *Fake) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Calls returns the calls so far.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Run is the svcctl.Options.Run.
func (f *Fake) Run(ctx context.Context, argv []string) (svcctl.Result, error) {
	f.mu.Lock()
	block, injected := f.block, f.err
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return f.record(argv, svcctl.Result{}, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return f.record(argv, svcctl.Result{}, err)
	}
	if injected != nil {
		return f.record(argv, svcctl.Result{}, injected)
	}
	f.mu.Lock()
	res, err := f.answer(argv)
	f.mu.Unlock()
	if err != nil {
		f.t.Helper()
		f.t.Errorf("svcctltest: %v", err)
	}
	return f.record(argv, res, err)
}

func (f *Fake) record(argv []string, res svcctl.Result, err error) (svcctl.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Argv: slices.Clone(argv), Result: res, Err: err})
	return res, err
}

var (
	mainPIDLine = regexp.MustCompile(`(?m)^MainPID=\d+$`)
	pidfileLine = regexp.MustCompile(`(?m)^pidfile=.*$`)
)

// answer models one call. It is called with f.mu held.
func (f *Fake) answer(argv []string) (svcctl.Result, error) {
	switch f.init {
	case platform.Systemd:
		return f.systemd(argv)
	case platform.OpenRC:
		return f.openrc(argv)
	}
	return svcctl.Result{}, fmt.Errorf("no fake for init system %q", f.init)
}

var systemdShow = []string{"/usr/bin/systemctl", "show", "--timestamp=unix",
	"-p", "LoadState", "-p", "ActiveState", "-p", "SubState", "-p", "MainPID", "-p", "StateChangeTimestamp",
	"--", Service + ".service"}

func (f *Fake) systemd(argv []string) (svcctl.Result, error) {
	if slices.Equal(argv, systemdShow) {
		if f.status != nil {
			return *f.status, nil
		}
		name := map[svcctl.State]string{svcctl.Running: "show-running.txt", svcctl.Stopped: "show-stopped.txt",
			svcctl.Failed: "show-failed.txt"}[f.model.State]
		if f.model.Absent {
			name = "show-not-found.txt"
		}
		res, err := f.fixture("systemd", name)
		if err == nil && name == "show-running.txt" && f.model.MainPID > 0 {
			res.Output = mainPIDLine.ReplaceAll(res.Output, []byte("MainPID="+strconv.Itoa(f.model.MainPID)))
		}
		return res, err
	}
	if len(argv) != 4 || argv[0] != "/usr/bin/systemctl" || argv[2] != "--" || argv[3] != Service+".service" {
		return svcctl.Result{}, fmt.Errorf("argv %q is not a systemctl call svcctl makes", argv)
	}
	a := svcctl.Action(argv[1])
	if f.model.Absent {
		return f.fixture("systemd", string(a)+"-not-found.txt")
	}
	m := &f.model
	switch a {
	case svcctl.Start:
		if m.State == svcctl.Running {
			return f.fixture("systemd", "start-already.txt")
		}
		return f.systemdStart("start", m.State)
	case svcctl.Restart:
		return f.systemdStart("restart", m.State)
	case svcctl.Stop:
		switch m.State {
		case svcctl.Running:
			m.State = svcctl.Stopped
			return f.fixture("systemd", "stop.txt")
		case svcctl.Failed:
			return f.fixture("systemd", "stop-failed.txt")
		}
		return f.fixture("systemd", "stop-already.txt")
	case svcctl.Reload:
		switch m.State {
		case svcctl.Stopped:
			return f.fixture("systemd", "reload-stopped.txt")
		case svcctl.Failed:
			return f.fixture("systemd", "reload-failed.txt")
		}
		hup, valid := "", "reload.txt"
		if m.ReloadModel == HUP {
			hup, valid = "hup-", "reload-hup.txt"
		}
		switch m.Config {
		case Invalid:
			// SignalParse: `nginx -s reload` parses the config first, exit
			// 1, nothing signalled. HUP: the master is signalled, rejects
			// the config, and systemctl exits 0.
			return f.fixture("systemd", "reload-"+hup+"invalid.txt")
		case BindConflict:
			// The master rejects the generation after parsing; systemctl
			// still exits 0.
			return f.fixture("systemd", "reload-"+hup+"bind-conflict.txt")
		}
		m.Generation++
		return f.fixture("systemd", valid)
	}
	return svcctl.Result{}, fmt.Errorf("argv %q has an action svcctl never sends", argv)
}

// systemdStart models start (from not running) and restart: systemctl's
// output does not depend on the state it starts from.
func (f *Fake) systemdStart(verb string, from svcctl.State) (svcctl.Result, error) {
	m := &f.model
	switch m.Config {
	case Invalid:
		m.State = svcctl.Failed
		return f.fixture("systemd", verb+"-invalid.txt")
	case BindConflict:
		m.State = svcctl.Failed
		return f.fixture("systemd", verb+"-bind-conflict.txt")
	}
	m.State = svcctl.Running
	m.Generation++
	if verb == "start" && from == svcctl.Failed {
		return f.fixture("systemd", "start-failed.txt")
	}
	return f.fixture("systemd", verb+".txt")
}

func (f *Fake) openrc(argv []string) (svcctl.Result, error) {
	const rc = "/sbin/rc-service"
	m := &f.model
	if slices.Equal(argv, []string{rc, "--exists", Service}) {
		if m.Absent {
			return f.fixture("openrc", "exists-absent.txt")
		}
		return f.fixture("openrc", "exists.txt")
	}
	if len(argv) != 4 || argv[0] != rc || argv[1] != "-C" || argv[2] != Service {
		return svcctl.Result{}, fmt.Errorf("argv %q is not an rc-service call svcctl makes", argv)
	}
	verb := argv[3]
	if !slices.Contains([]string{"status", "start", "stop", "restart", "reload"}, verb) {
		return svcctl.Result{}, fmt.Errorf("argv %q has an action svcctl never sends", argv)
	}
	if m.Absent {
		return f.fixture("openrc", "absent.txt")
	}
	switch verb {
	case "status":
		if f.status != nil {
			return *f.status, nil
		}
		return f.fixture("openrc", map[svcctl.State]string{svcctl.Running: "status-started.txt",
			svcctl.Stopped: "status-stopped.txt", svcctl.Failed: "status-crashed.txt"}[m.State])
	case "start":
		switch m.State {
		case svcctl.Running:
			return f.fixture("openrc", "start-already.txt")
		case svcctl.Failed:
			return f.fixture("openrc", "start-crashed.txt")
		}
		return f.openrcStart("start")
	case "restart":
		switch m.State {
		case svcctl.Stopped:
			return f.openrcStart("restart-stopped")
		case svcctl.Failed:
			if m.Config == Valid {
				return f.openrcStart("restart-crashed")
			}
			// Not captured: stop of the crashed service then a failing
			// start would differ only in the text.
			return svcctl.Result{}, fmt.Errorf("restart of a crashed nginx with a %s config is not modeled", m.Config)
		}
		return f.openrcStart("restart")
	case "stop":
		switch m.State {
		case svcctl.Running:
			m.State = svcctl.Stopped
			return f.fixture("openrc", "stop.txt")
		case svcctl.Failed:
			m.State = svcctl.Stopped
			return f.fixture("openrc", "stop-crashed.txt")
		}
		return f.fixture("openrc", "stop-already.txt")
	default: // reload
		switch m.State {
		case svcctl.Stopped:
			return f.fixture("openrc", "reload-stopped.txt")
		case svcctl.Failed:
			return f.fixture("openrc", "reload-crashed.txt")
		}
		switch m.Config {
		case Invalid:
			// reload() runs `nginx -t -q` first.
			return f.fixture("openrc", "reload-invalid.txt")
		case BindConflict:
			// The test passes and HUP is sent; the master then rejects the
			// generation, and rc-service exits 0.
			return f.fixture("openrc", "reload-bind-conflict.txt")
		}
		m.Generation++
		return f.fixture("openrc", "reload.txt")
	}
}

// openrcStart models a start or restart that ends started on a valid
// config and stopped otherwise. name is the capture's base name.
func (f *Fake) openrcStart(name string) (svcctl.Result, error) {
	m := &f.model
	switch m.Config {
	case Invalid:
		m.State = svcctl.Stopped
		return f.fixture("openrc", name+"-invalid.txt")
	case BindConflict:
		m.State = svcctl.Stopped
		return f.fixture("openrc", name+"-bind-conflict.txt")
	}
	m.State = svcctl.Running
	m.Generation++
	return f.fixture("openrc", name+".txt")
}

func (f *Fake) fixture(dir, name string) (svcctl.Result, error) {
	fx, err := loadFixtures()
	if err != nil {
		return svcctl.Result{}, err
	}
	r, ok := fx[dir+"/"+name]
	if !ok {
		return svcctl.Result{}, fmt.Errorf("no capture %s/%s for this call (state %s, config %s)", dir, name, f.model.State, f.model.Config)
	}
	return svcctl.Result{ExitCode: r.ExitCode, Output: slices.Clone(r.Output)}, nil
}

// TestdataDir is internal/svcctl/testdata in the source tree.
func TestdataDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("svcctltest: cannot locate the source tree")
	}
	return filepath.Join(filepath.Dir(file), "..", "testdata"), nil
}

var (
	fixturesOnce sync.Once
	fixtures     map[string]svcctl.Result
	fixturesErr  error
)

// loadFixtures reads every capture in the README's provenance table, with
// the exit code recorded there.
func loadFixtures() (map[string]svcctl.Result, error) {
	fixturesOnce.Do(func() { fixtures, fixturesErr = readFixtures() })
	return fixtures, fixturesErr
}

// ProvenanceRow is one row of testdata/README.md's table.
type ProvenanceRow struct {
	File, Host, Distribution, Init, Nginx, Command, Exit, Date string
}

// Provenance parses the provenance table of testdata/README.md.
func Provenance() ([]ProvenanceRow, error) {
	dir, err := TestdataDir()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "README.md"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []ProvenanceRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), " | ")
		if len(cells) != 8 {
			return nil, fmt.Errorf("svcctltest: README row has %d cells, want 8: %s", len(cells), line)
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, ProvenanceRow{File: strings.Trim(cells[0], "`"), Host: cells[1], Distribution: cells[2],
			Init: cells[3], Nginx: cells[4], Command: cells[5], Exit: cells[6], Date: cells[7]})
	}
	return rows, sc.Err()
}

func readFixtures() (map[string]svcctl.Result, error) {
	rows, err := Provenance()
	if err != nil {
		return nil, err
	}
	dir, err := TestdataDir()
	if err != nil {
		return nil, err
	}
	out := map[string]svcctl.Result{}
	for _, r := range rows {
		if r.Exit == "-" {
			continue
		}
		code, err := strconv.Atoi(r.Exit)
		if err != nil {
			return nil, fmt.Errorf("svcctltest: README row %s: exit %q: %w", r.File, r.Exit, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(r.File)))
		if err != nil {
			return nil, err
		}
		out[r.File] = svcctl.Result{ExitCode: code, Output: b}
	}
	return out, nil
}

// WriteDaemonRecord lays out <runRoot>/openrc/daemons/<service>/001 in the
// captured start-stop-daemon format, naming pidfile, and writes pid to
// pidfile, so the real OpenRC controller can resolve MainPID.
func WriteDaemonRecord(runRoot, service, pidfile string, pid int) error {
	dir, err := TestdataDir()
	if err != nil {
		return err
	}
	rec, err := os.ReadFile(filepath.Join(dir, "openrc", "daemon-record"))
	if err != nil {
		return err
	}
	rec = pidfileLine.ReplaceAll(rec, []byte("pidfile="+pidfile))
	d := filepath.Join(runRoot, "openrc", "daemons", service)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(d, "001"), rec, 0o644); err != nil {
		return err
	}
	return os.WriteFile(pidfile, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}
