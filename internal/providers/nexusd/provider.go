// Package nexusd is the nexusd provider: the daemon managing itself on its
// own host. It reads its own state (get), checks its own configuration file
// (validate-config) and reloads it through the daemon's run loop (reload).
// Its declarations live in the spec subpackage.
//
// No operation takes a path, a command or a URL (NSM FR-25): the provider
// acts only on its own binary, unit and configuration file, which it learns
// from Options and from the provider.Daemon the daemon hands it.
package nexusd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/release"
)

// Paths are the host files the provider manages.
type Paths struct {
	Binary   string // The installed daemon
	Prev     string // The rollback binary
	UnitFile string // The installed unit file
	Unit     string // The unit's name
}

// DefaultPaths are where deploy/install.sh installs nexusd.
func DefaultPaths() Paths {
	return Paths{
		Binary:   "/usr/local/bin/nexusd",
		Prev:     "/usr/local/bin/nexusd.prev",
		UnitFile: "/etc/systemd/system/nexusd.service",
		Unit:     "nexusd.service",
	}
}

// withDefaults fills each empty path from DefaultPaths.
func (p Paths) withDefaults() Paths {
	d := DefaultPaths()
	if p.Binary == "" {
		p.Binary = d.Binary
	}
	if p.Prev == "" {
		p.Prev = d.Prev
	}
	if p.UnitFile == "" {
		p.UnitFile = d.UnitFile
	}
	if p.Unit == "" {
		p.Unit = d.Unit
	}
	return p
}

// RunResult is a finished command. A non-zero exit is a result, not an
// error: the caller decides what each code means.
type RunResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner runs a program: argv[0] is an absolute path, never looked up in
// PATH. It is the exec seam for "-version" and "validate-config" of a
// release binary. ctx bounds the run.
type Runner interface {
	Run(ctx context.Context, argv []string) (RunResult, error)
}

// Options configures New. Every seam has a production default.
type Options struct {
	Host  string // Reported in every resource and result
	Paths Paths  // Empty fields default to DefaultPaths
	// Systemd reports whether the host runs systemd. Nil means
	// detach.SystemdAvailable.
	Systemd func() detach.Availability
	// InService returns nil when this process runs as Paths.Unit. Nil means
	// the /proc/self/cgroup check (A-32).
	InService func() error
	// Executable is the running executable. Nil means os.Executable.
	Executable func() (string, error)
	Now        func() time.Time // Nil means time.Now
	Runner     Runner           // Nil means the real exec runner
}

// Details of the provider in capabilities (A-16).
const (
	DetailSelfManagement = "selfManagement" // "available" or "unavailable"
	DetailReason         = "reason"         // Why self-management is unavailable
)

// settings is the providers.nexusd section in force, with its trusted keys
// parsed once.
type settings struct {
	cfg  config.NexusdConfig
	keys []ed25519.PublicKey
}

// daemonRef boxes the provider.Daemon so it can be stored atomically.
type daemonRef struct{ d provider.Daemon }

// Provider is the nexusd provider.
type Provider struct {
	host       string
	paths      Paths
	systemd    func() detach.Availability
	inService  func() error
	executable func() (string, error)
	now        func() time.Time
	run        Runner
	// afterFunc runs f after d unless stopped first: time.AfterFunc outside
	// tests, a virtual clock in them.
	afterFunc func(d time.Duration, f func()) (stop func() bool)

	settings atomic.Pointer[settings]
	daemon   atomic.Pointer[daemonRef]

	prev prevCache
}

// New returns the provider with the default settings until Configure.
func New(opts Options) *Provider {
	p := &Provider{
		host:       opts.Host,
		paths:      opts.Paths.withDefaults(),
		systemd:    opts.Systemd,
		inService:  opts.InService,
		executable: opts.Executable,
		now:        opts.Now,
		run:        opts.Runner,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
	}
	p.prev.sem = make(chan struct{}, 1)
	if p.systemd == nil {
		p.systemd = detach.SystemdAvailable
	}
	if p.inService == nil {
		unit := p.paths.Unit
		p.inService = func() error { return checkCgroup(procSelfCgroup, unit) }
	}
	if p.executable == nil {
		p.executable = os.Executable
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.run == nil {
		p.run = execRunner{}
	}
	p.settings.Store(&settings{cfg: config.Default().Providers.Nexusd.Settings})
	return p
}

// Spec returns the declarations.
func (p *Provider) Spec() def.ProviderSpec { return spec.Spec() }

// Configure strictly decodes the nexusd section and parses its trusted
// keys. A section it refuses leaves the settings in force as they were. An
// error names a key only by its index, never by its value.
func (p *Provider) Configure(decode func(into any) error) error {
	s := config.Default().Providers.Nexusd.Settings
	if err := decode(&s); err != nil {
		return err
	}
	keys := make([]ed25519.PublicKey, 0, len(s.Update.TrustedKeys))
	for i, k := range s.Update.TrustedKeys {
		pk, err := release.ParsePublicKey(k)
		if err != nil {
			return fmt.Errorf("update.trustedKeys[%d] is not a valid Ed25519 public key", i)
		}
		keys = append(keys, pk)
	}
	p.settings.Store(&settings{cfg: s, keys: keys})
	return nil
}

func (p *Provider) current() *settings { return p.settings.Load() }

// UseDaemon keeps the daemon serving this provider. It implements
// provider.DaemonUser.
func (p *Provider) UseDaemon(d provider.Daemon) { p.daemon.Store(&daemonRef{d: d}) }

// daemonOf returns the daemon, or an error when UseDaemon was not called.
func (p *Provider) daemonOf() (provider.Daemon, error) {
	ref := p.daemon.Load()
	if ref == nil || ref.d == nil {
		return nil, errors.New("nexusd: no daemon: UseDaemon was not called")
	}
	return ref.d, nil
}

// Preflight is always enabled, so the reads work on any host (A-16). Its
// details say whether self-management (restart, update, rollback and the
// upload) can run here, and why not.
func (p *Provider) Preflight(context.Context) provider.Status {
	a := p.selfManagementReady()
	details := map[string]any{DetailSelfManagement: "available"}
	if !a.Available {
		details[DetailSelfManagement] = "unavailable"
		details[DetailReason] = a.Reason
	}
	return provider.Status{State: apitypes.ProviderEnabled, Details: details}
}

// selfManagementReady reports whether the host runs systemd and this
// process runs as the nexusd unit: what restart, update, rollback and the
// upload need. It is shared by their admissions and by get.
func (p *Provider) selfManagementReady() spec.Availability {
	if a := p.systemd(); !a.Available {
		return spec.Availability{Reason: a.Reason}
	}
	if err := p.inService(); err != nil {
		return spec.Availability{Reason: err.Error()}
	}
	return spec.Availability{Available: true}
}

// requireSelfManagement refuses, with capability-unavailable and the
// reason, an operation that needs self-management on a host without it.
func (p *Provider) requireSelfManagement() error {
	a := p.selfManagementReady()
	if a.Available {
		return nil
	}
	return problem.New(problem.CapabilityUnavailable, "nexusd self-management is unavailable: "+a.Reason).
		With("provider", spec.Name).With("reason", a.Reason)
}

// Bindings binds get, validate-config and reload.
func (p *Provider) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.List, p.get),
		provider.Sync(spec.ValidateConfig, p.validate),
		provider.InProcess(spec.Reload, provider.InProcessSpec[spec.ReloadInput, spec.ReloadResult]{
			Admit: p.admitReload,
			Lane:  laneOf[spec.ReloadInput],
			Run:   p.reload,
		}),
	}
}

// nexusdLane is the lane reload, restart, update and rollback share: one
// of them queued or running at a time, and a second request answers 409
// naming it (FR-11).
var nexusdLane = provider.Lane{Key: spec.LaneKey, Policy: provider.LaneReject}

func laneOf[In any](context.Context, In) (provider.Lane, error) { return nexusdLane, nil }

// procSelfCgroup lists the cgroups of this process.
const procSelfCgroup = "/proc/self/cgroup"

// maxCgroupFile bounds what checkCgroup reads.
const maxCgroupFile = 64 << 10

// checkCgroup returns nil when a cgroup of this process, as listed in the
// file at path, is the unit's: its path ends in "/<unit>" (A-32). On the
// cgroup v2 unified layout that is the "0::" line; the name=systemd line of
// a v1 or hybrid host is accepted too.
func checkCgroup(path, unit string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot tell whether nexusd runs as %s: %w", unit, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCgroupFile))
	if err != nil {
		return fmt.Errorf("cannot tell whether nexusd runs as %s: %w", unit, err)
	}
	var own string
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		// hierarchy-ID:controller-list:cgroup-path; the path may hold ":".
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		cg := parts[2]
		if strings.HasSuffix(cg, "/"+unit) {
			return nil
		}
		if (parts[0] == "0" && parts[1] == "") || own == "" {
			own = cg
		}
	}
	if own == "" {
		return fmt.Errorf("not running in %s (no cgroup listed in %s)", unit, path)
	}
	return fmt.Errorf("not running in %s (cgroup %s)", unit, own)
}

// The real Runner's bounds and environment.
const (
	maxRunOutput = 64 << 10
	runWaitDelay = time.Second
)

var runEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "LANG=C"}

// execRunner is the real Runner: the fixed environment, stdin on
// /dev/null, cwd /, bounded output, and the process killed when ctx ends.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, argv []string) (RunResult, error) {
	if len(argv) == 0 {
		return RunResult{}, errors.New("run: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = runEnv
	cmd.Dir = "/"
	cmd.WaitDelay = runWaitDelay
	stdout := &limitedBuffer{max: maxRunOutput}
	stderr := &limitedBuffer{max: maxRunOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	res := RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.Exited()) {
		if ctx.Err() != nil {
			return res, fmt.Errorf("%s: %w", argv[0], context.Cause(ctx))
		}
		return res, fmt.Errorf("%s: %w", argv[0], err)
	}
	return res, nil
}

// limitedBuffer keeps at most max bytes and drops the rest.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
