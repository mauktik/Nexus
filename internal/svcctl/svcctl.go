// Package svcctl starts, stops, restarts and reloads a service and reports
// its state through the host's init system (FR-19). It has a systemd and an
// OpenRC implementation, chosen by platform.InitSystem. The init system's
// tool is always called by absolute path, with a fixed minimal environment,
// no shell and no stdin, and every call is bounded (CON-4).
package svcctl

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/platform"
)

// State is a service's state, normalized across init systems.
type State string

const (
	Running  State = "running"
	Stopped  State = "stopped"
	Failed   State = "failed"
	Starting State = "starting"
	Stopping State = "stopping"
	// Unknown is any state the init system reported in words this package
	// does not map. It is never a guess at one of the others.
	Unknown State = "unknown"
)

// Status is a service's state as the init system reports it.
type Status struct {
	Known   bool       // The init system knows the service
	State   State      //
	Since   *time.Time // Nil when the init system does not say (always nil on OpenRC)
	MainPID int        // 0 when not running or unknown
	Raw     string     // The init system's own words, for problem details
}

// Action is a service-control verb.
type Action string

const (
	Start   Action = "start"
	Stop    Action = "stop"
	Restart Action = "restart"
	Reload  Action = "reload"
)

func (a Action) valid() bool {
	switch a {
	case Start, Stop, Restart, Reload:
		return true
	}
	return false
}

// Result is what the init system's tool returned for an action. A non-zero
// ExitCode is a result, not an error.
type Result struct {
	ExitCode int
	Output   []byte // Combined stdout and stderr, capped at OutputCap
}

// ErrNotStarted is wrapped by every error that means the init system's tool
// was never started: an argument refused before any exec, a program that is
// not there or cannot be executed. An action whose error is not
// ErrNotStarted (and not a context error) may have reached the service
// before the tool failed, for example a tool killed by a signal or a wait
// that failed after it delivered the action, so the caller must not treat
// it as "nothing was sent".
var ErrNotStarted = errors.New("svcctl: the init system's tool was not started")

// Controller controls services through one init system.
type Controller interface {
	Init() platform.InitSystem
	Status(ctx context.Context, service string) (Status, error)
	Do(ctx context.Context, a Action, service string) (Result, error)
}

// Options are the seams and bounds of a Controller.
type Options struct {
	// Run executes argv. Nil means the real runner: argv[0] (absolute) is
	// executed directly, with the environment exactly
	// {PATH=/usr/sbin:/usr/bin:/sbin:/bin, LC_ALL=C}, no stdin, and combined
	// output capped at OutputCap.
	Run      func(ctx context.Context, argv []string) (Result, error)
	ProcRoot string        // "" = "/proc"
	RunRoot  string        // "" = "/run" (OpenRC state)
	Timeout  time.Duration // Per call; 0 = DefaultTimeout
}

// ServiceNamePattern is what Status and Do accept as a service name.
const ServiceNamePattern = `^[A-Za-z0-9][A-Za-z0-9@._-]{0,63}$`

var serviceName = regexp.MustCompile(ServiceNamePattern)

const (
	// DefaultTimeout bounds each call when Options.Timeout is 0.
	DefaultTimeout = 30 * time.Second
	// OutputCap is the most output kept from one call.
	OutputCap = 64 << 10
	// waitDelay bounds the wait for the output pipes after the tool exited
	// or was killed, in case a descendant outside its process group still
	// holds them.
	waitDelay = time.Second
)

// env is the whole environment of every call of the real runner (L-21).
func env() []string { return []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"} }

// New returns the Controller of init. It validates nothing about any
// service; an init system other than systemd or OpenRC is an error.
func New(init platform.InitSystem, opts Options) (Controller, error) {
	b := base{run: opts.Run, procRoot: opts.ProcRoot, runRoot: opts.RunRoot, timeout: opts.Timeout}
	if b.run == nil {
		b.run = realRun
	}
	if b.procRoot == "" {
		b.procRoot = "/proc"
	}
	if b.runRoot == "" {
		b.runRoot = "/run"
	}
	if b.timeout <= 0 {
		b.timeout = DefaultTimeout
	}
	switch init {
	case platform.Systemd:
		return &systemd{base: b}, nil
	case platform.OpenRC:
		return &openrc{base: b}, nil
	}
	return nil, fmt.Errorf("svcctl: no service control for init system %q", string(init))
}

// base is what both implementations share.
type base struct {
	run      func(ctx context.Context, argv []string) (Result, error)
	procRoot string
	runRoot  string
	timeout  time.Duration
}

func checkService(service string) error {
	if !serviceName.MatchString(service) {
		return fmt.Errorf("%w: invalid service name %q: it must match %s", ErrNotStarted, service, ServiceNamePattern)
	}
	return nil
}

func checkAction(a Action) error {
	if !a.valid() {
		return fmt.Errorf("%w: invalid action %q: it must be start, stop, restart or reload", ErrNotStarted, string(a))
	}
	return nil
}

// call runs argv bounded by the timeout. When the bound expired the call is
// an error wrapping context.DeadlineExceeded, whatever the runner returned.
func (b base) call(ctx context.Context, argv []string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	res, err := b.run(ctx, argv)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("svcctl: %s did not finish within %s: %w", strings.Join(argv, " "), b.timeout, ctxErr)
		}
		return Result{}, fmt.Errorf("svcctl: %s: %w", strings.Join(argv, " "), ctxErr)
	}
	if err != nil {
		return Result{}, fmt.Errorf("svcctl: %s: %w", strings.Join(argv, " "), err)
	}
	return res, nil
}

// realRun executes argv[0], which must be absolute, with argv[1:] as its
// arguments: no shell, no stdin, the environment exactly env(), and combined
// output capped at OutputCap. The tool runs in its own process group, which
// is killed when ctx is done. A failure before the tool started wraps
// ErrNotStarted; any other error came after it started.
func realRun(ctx context.Context, argv []string) (Result, error) {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return Result{}, fmt.Errorf("%w: refusing to run %q: the program must be an absolute path", ErrNotStarted, argv)
	}
	out := &capWriter{max: OutputCap}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env()
	cmd.Stdin = nil
	// The same writer for both streams makes exec share one pipe and one
	// copying goroutine, so the writer needs no lock.
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	res := Result{Output: out.buf}
	var exitErr *exec.ExitError
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		res.ExitCode = cmd.ProcessState.ExitCode()
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		res.ExitCode = exitErr.ExitCode()
	default:
		return Result{}, err
	}
	if res.ExitCode < 0 {
		return Result{}, fmt.Errorf("%s was terminated: %s", argv[0], cmd.ProcessState)
	}
	return res, nil
}

// capWriter keeps the first max bytes written to it and discards the rest,
// so the tool never blocks on a full pipe.
type capWriter struct {
	buf []byte
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
