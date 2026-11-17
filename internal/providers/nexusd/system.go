package nexusd

import (
	"bytes"
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
)

// system is the host as the service helper sees it: nexusd.service, the
// binaries' -version, the daemon's /healthz, install.sh and the time. The
// real implementation shells out to /usr/bin/systemctl and execs binaries;
// T0 uses a fake that encodes systemd's behaviour (NFR-6). The clock is part
// of the seam because the fake's transitions run on virtual time.
type system interface {
	clock
	// Show reads the unit's ActiveState, SubState, Result, MainPID and
	// NRestarts.
	Show(ctx context.Context) (unitState, error)
	// Restart is `systemctl restart --no-block`: it enqueues the restart
	// and returns.
	Restart(ctx context.Context) error
	DaemonReload(ctx context.Context) error
	ResetFailed(ctx context.Context) error
	// ExeVersion runs `path -version` and returns the version it prints.
	ExeVersion(ctx context.Context, path string) (string, error)
	// Healthz is nil when GET <t.URL>/healthz answers 200.
	Healthz(ctx context.Context, t healthTarget) error
	// RunInstall runs install.sh with a fixed argv, environment and cwd,
	// streaming its output to out. A non-zero exit is a code, not an
	// error; ctx bounds the run.
	RunInstall(ctx context.Context, script, dir string, out io.Writer) (exitCode int, err error)
	// Reload is `systemctl reload <unit>`, bounded by systemctlTimeout and
	// by ctx (the attempt deadline, D-4). Under ExecReload=kill -HUP it
	// returns once the signal is sent, before the daemon has reloaded
	// (A-6). A unit that is not active is errUnitNotActive.
	Reload(ctx context.Context) error
}

// clock is the helper's time: real, or virtual in T0.
type clock interface {
	Now() time.Time
	// Sleep waits d, or until ctx ends, which it returns as an error.
	Sleep(ctx context.Context, d time.Duration) error
	// WithDeadline is ctx ending at t on this clock.
	WithDeadline(ctx context.Context, t time.Time) (context.Context, context.CancelFunc)
}

// unitState is what `systemctl show` reports about the unit.
type unitState struct {
	ActiveState string
	SubState    string
	Result      string
	MainPID     int
	NRestarts   int
}

// The real system's commands and bounds (every external call has a
// timeout).
const (
	systemctlPath    = "/usr/bin/systemctl"
	systemctlTimeout = 30 * time.Second
	bashPath         = "/bin/bash"
	installWaitDelay = time.Second
)

// installEnv is install.sh's whole environment.
var installEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}

// showProperties are the properties Show asks for, in this order (A-18).
var showProperties = []string{"ActiveState", "SubState", "Result", "MainPID", "NRestarts"}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (realClock) WithDeadline(ctx context.Context, t time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, t)
}

// realSystem is the host: systemctl, exec and HTTP.
type realSystem struct {
	realClock
	systemctl string // Absolute path of systemctl; a test points it at a stand-in
	unit      string
	run       Runner
}

var _ system = (*realSystem)(nil)

func newRealSystem(unit string) *realSystem {
	return &realSystem{systemctl: systemctlPath, unit: unit, run: execRunner{}}
}

// systemctlRun runs systemctl with args under its 30 s bound. A non-zero
// exit is an error carrying systemctl's stderr.
func (s *realSystem) systemctlRun(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, systemctlTimeout)
	defer cancel()
	argv := append([]string{s.systemctl}, args...)
	res, err := s.run.Run(ctx, argv)
	if err != nil {
		return nil, fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("systemctl %s: exit %d: %s", strings.Join(args, " "), res.ExitCode,
			trimOutput(res.Stderr))
	}
	return res.Stdout, nil
}

func (s *realSystem) Show(ctx context.Context) (unitState, error) {
	args := []string{"show", s.unit}
	for _, p := range showProperties {
		args = append(args, "-p", p)
	}
	out, err := s.systemctlRun(ctx, args...)
	if err != nil {
		return unitState{}, err
	}
	return parseShow(out)
}

func (s *realSystem) Restart(ctx context.Context) error {
	_, err := s.systemctlRun(ctx, "restart", "--no-block", s.unit)
	return err
}

func (s *realSystem) DaemonReload(ctx context.Context) error {
	_, err := s.systemctlRun(ctx, "daemon-reload")
	return err
}

// Reload is `systemctl reload <unit>`. systemctlRun's 30 s bound is a
// WithTimeout of ctx, so the call ends at the earlier of the two.
func (s *realSystem) Reload(ctx context.Context) error {
	_, err := s.systemctlRun(ctx, "reload", s.unit)
	return classifyReloadError(err)
}

// errUnitNotActive is a reload refused because the unit is not active
// (activating, deactivating, inactive or failed): one worth retrying.
var errUnitNotActive = errors.New("the unit is not active")

// notActiveText is systemctl's message for a reload job whose result is
// "invalid", which unit_reload gives a unit that is not active
// (systemd src/libsystemd/sd-bus/bus-wait-for-jobs.c: "%s is not active,
// cannot reload.").
const notActiveText = "is not active, cannot reload"

// classifyReloadError marks systemctl's not-active refusal with
// errUnitNotActive, keeping systemctl's text; any other error is returned
// as it is.
func classifyReloadError(err error) error {
	if err != nil && strings.Contains(err.Error(), notActiveText) {
		return fmt.Errorf("%w: %w", errUnitNotActive, err)
	}
	return err
}

func (s *realSystem) ResetFailed(ctx context.Context) error {
	_, err := s.systemctlRun(ctx, "reset-failed", s.unit)
	return err
}

// ExeVersion runs `path -version` for at most versionTimeout. For the
// running daemon, path is /proc/<MainPID>/exe, which names the image the
// process started from even after a rename over the binary (A-17).
func (s *realSystem) ExeVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	res, err := s.run.Run(ctx, []string{path, "-version"})
	if err != nil {
		return "", fmt.Errorf("%s -version: %w", path, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s -version: exit %d: %s", path, res.ExitCode, trimOutput(res.Stderr))
	}
	v, ok := parseVersion(res.Stdout)
	if !ok {
		return "", fmt.Errorf("%s -version printed %q, not \"nexusd <version>\"", path, trimOutput(res.Stdout))
	}
	return v, nil
}

func (s *realSystem) Healthz(ctx context.Context, t healthTarget) error { return checkHealthz(ctx, t) }

// RunInstall runs `/bin/bash script` in dir with the fixed environment.
// Its stdout and stderr both stream to out. When ctx ends, the script's
// whole process group is killed, so a child such as sleep cannot keep the
// output open.
func (s *realSystem) RunInstall(ctx context.Context, script, dir string, out io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, bashPath, script)
	cmd.Dir = dir
	cmd.Env = installEnv
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = installWaitDelay
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, fmt.Errorf("%s: %w", script, ctx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("%s: %w", script, err)
	}
	return 0, nil
}

// parseShow parses `systemctl show -p ...` output: one NAME=VALUE line per
// property, in systemctl's order, not the order of the -p flags. Every
// property of showProperties must be present; NRestarts needs systemd 235
// or later (A-18).
func parseShow(out []byte) (unitState, error) {
	vals := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if ok {
			vals[k] = v
		}
	}
	for _, p := range showProperties {
		if _, ok := vals[p]; !ok {
			return unitState{}, fmt.Errorf("systemctl show printed no %s (NRestarts needs systemd 235 or later)", p)
		}
	}
	st := unitState{ActiveState: vals["ActiveState"], SubState: vals["SubState"], Result: vals["Result"]}
	var err error
	if st.MainPID, err = parseCount("MainPID", vals["MainPID"]); err != nil {
		return unitState{}, err
	}
	if st.NRestarts, err = parseCount("NRestarts", vals["NRestarts"]); err != nil {
		return unitState{}, err
	}
	return st, nil
}

// parseCount parses an unsigned D-Bus property ("u").
func parseCount(name, v string) (int, error) {
	n, err := strconv.ParseUint(v, 10, 31)
	if err != nil {
		return 0, fmt.Errorf("systemctl show: %s=%q is not a number", name, v)
	}
	return int(n), nil
}

// copyAtomic replaces dst with a copy of src, with mode, through a
// temporary file in dst's directory, fsync, rename and a directory fsync:
// dst is the old file or the complete new one at every instant.
func copyAtomic(src, dst string, mode fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dir := filepath.Dir(dst)
	f, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if _, err := io.Copy(f, in); err != nil {
		return errors.Join(fmt.Errorf("copy %s to %s: %w", src, tmp, err), f.Close())
	}
	if err := f.Chmod(mode); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory, making a rename in it durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	if serr != nil {
		return fmt.Errorf("sync %s: %w", dir, serr)
	}
	return nil
}

// trimOutput bounds text quoted in a detail.
func trimOutput(b []byte) string {
	const limit = 512
	b = bytes.TrimSpace(b)
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}
