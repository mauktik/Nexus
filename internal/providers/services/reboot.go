package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/host"
	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// The guarded reboot (DF-4) runs detached, as `nexusd services-helper run
// --dir ops/<id>` in reboot mode, outside nexusd's sandbox (D-5). Admission
// runs the five guards of FR-10 in order and refuses on the first that
// fails, or, for whatIf, reports them all. Prepare runs them again, without
// the operation's own ID, and snapshots what the helper needs into
// input.json. The helper waits, records the request, re-checks and triggers
// (D-3, D-4, helper_reboot.go); the outcome is decided in the next boot by
// AfterReboot, or by Finalize when the helper reported one of its own.

// rebootConfigKey is the configuration that enables the reboot. Only the
// file on the host sets it (A-19).
const rebootConfigKey = "providers.services.reboot.enabled"

// rebootLaneKey is the lane of every reboot: one at a time, a second
// answering 409 conflict.
const rebootLaneKey = "reboot"

// The bounds of the reboot helper's spec (DF-4 step 3). Timeout is the
// delay plus the post-trigger wait plus rebootTimeoutMargin, MaxRuntime the
// same plus rebootMaxMargin. rebootGrace is above the trigger's own bound
// (svcctl.DefaultTimeout, S4) with a 10 s margin, so a stop's SIGTERM is
// never escalated to SIGKILL while the helper still decides whether to keep
// or retract the record (A-37).
const (
	rebootTimeoutMargin = 60 * time.Second
	rebootMaxMargin     = 120 * time.Second
	rebootGrace         = svcctl.DefaultTimeout + 10*time.Second
)

// The guards of FR-10, in the order admission checks them.
const (
	GuardConfirmHost   = "confirm-host"
	GuardRebootEnabled = "reboot-enabled"
	GuardNexusdAtBoot  = "nexusd-enabled-at-boot"
	GuardNoOperations  = "no-running-operations"
	GuardConfigValid   = "config-valid"
)

// Files the reboot reads under ProcRoot (DF-4 step 3).
const (
	procBootID    = "sys/kernel/random/boot_id"
	procOSRelease = "sys/kernel/osrelease"
	procUptime    = "uptime"
)

// rebootLane is the reboot's lane: LaneReject, so a second reboot answers
// 409 conflict naming the first.
func rebootLane(context.Context, spec.RebootInput) (provider.Lane, error) {
	return provider.Lane{Key: rebootLaneKey, Policy: provider.LaneReject}, nil
}

// rebootBindings binds the reboot.
func (p *Provider) rebootBindings() []provider.Binding {
	return []provider.Binding{provider.Detached(spec.Reboot, p.rebootSpec())}
}

// rebootSpec is the reboot's detached spec (DF-4).
func (p *Provider) rebootSpec() provider.DetachedSpec[spec.RebootInput, spec.RebootResult] {
	return provider.DetachedSpec[spec.RebootInput, spec.RebootResult]{
		Admit:       p.admitReboot,
		Lane:        rebootLane,
		Prepare:     p.prepareReboot,
		Finalize:    p.finalizeReboot,
		LostDetail:  p.rebootLostDetail,
		AfterReboot: p.afterReboot,
	}
}

// rebootGuardReport is every guard of one evaluation, in FR-10 order, with
// the problem each failed guard refuses with, and what whatIf reports
// beside the guards.
type rebootGuardReport struct {
	guards []spec.RebootGuard
	refuse []*problem.Error // Parallel to guards; nil for a guard that passed
	// nexusdAtBoot is whether the service running nexusd starts at boot;
	// nil when it could not be read.
	nexusdAtBoot *bool
	running      []string // The other Queued and Running operations
}

// first is the problem of the first guard that failed, or nil.
func (r rebootGuardReport) first() error {
	for _, e := range r.refuse {
		if e != nil {
			return e
		}
	}
	return nil
}

// passed reports whether every guard passed.
func (r rebootGuardReport) passed() bool { return r.first() == nil }

func (r *rebootGuardReport) add(name string, detail string, refuse *problem.Error) {
	r.guards = append(r.guards, spec.RebootGuard{Name: name, Passed: refuse == nil, Detail: detail})
	r.refuse = append(r.refuse, refuse)
}

// rebootGuards evaluates the five guards of FR-10, in order, on one
// settings snapshot. exclude is an operation ID the operations guard
// ignores: Prepare's own. force makes the operations guard pass (it is
// still evaluated, so whatIf reports the operations). Every guard is
// evaluated, so whatIf can report them all; none of them changes anything.
func (p *Provider) rebootGuards(ctx context.Context, in spec.RebootInput, exclude string) rebootGuardReport {
	var r rebootGuardReport
	snap := p.snapshot()

	if in.ConfirmHost == p.host {
		r.add(GuardConfirmHost, fmt.Sprintf("confirmHost equals this host's name, %s", p.host), nil)
	} else {
		detail := fmt.Sprintf("confirmHost must equal this host's name, %q, exactly as system.capabilities.get reports "+
			"it", p.host)
		r.add(GuardConfirmHost, detail, problem.New(problem.ValidationFailed, detail).
			With("errors", []problem.FieldError{{Path: "/confirmHost", Message: detail}}))
	}

	avail := p.rebootAvailability(p.plat.Init)
	switch {
	case !snap.cfg.Reboot.Enabled:
		why := rebootConfigKey + " is false; only the configuration file on the host sets it"
		r.add(GuardRebootEnabled, why, problem.New(problem.RebootUnavailable, "the reboot is unavailable: "+why).
			With("reason", why).With("configKey", rebootConfigKey))
	case !avail.available:
		r.add(GuardRebootEnabled, avail.reason, problem.New(problem.RebootUnavailable,
			"the reboot is unavailable: "+avail.reason).With("reason", avail.reason).With("configKey", rebootConfigKey))
	default:
		r.add(GuardRebootEnabled, fmt.Sprintf("%s is true and the reboot can be confirmed by the boot ID (%s)",
			rebootConfigKey, avail.source), nil)
	}

	self := p.selfService()
	atBoot, word, err := svcctl.EnabledAtBoot(ctx, p.plat.Init, self, p.svcctlOptions())
	switch {
	case err != nil:
		detail := fmt.Sprintf("whether %s starts at boot cannot be read from %s: %v", self, p.plat.Init, err)
		r.add(GuardNexusdAtBoot, detail, problem.New(problem.DependencyUnavailable, detail).With("provider", spec.Name))
	case !atBoot:
		if word == "" {
			word = "no runlevel"
		}
		detail := fmt.Sprintf("%s is not enabled at boot (%s), so nexusd would not come back after the reboot", self, word)
		r.add(GuardNexusdAtBoot, detail, problem.New(problem.NexusdNotEnabledAtBoot, detail).
			With("service", self).With("state", word))
		r.nexusdAtBoot = &atBoot
	default:
		r.add(GuardNexusdAtBoot, fmt.Sprintf("%s is enabled at boot (%s)", self, word), nil)
		r.nexusdAtBoot = &atBoot
	}

	running, err := p.otherOperations(exclude)
	r.running = running
	switch {
	case err != nil:
		detail := fmt.Sprintf("the running operations cannot be listed: %v", err)
		r.add(GuardNoOperations, detail, problem.New(problem.DependencyUnavailable, detail).With("provider", spec.Name))
	case len(running) == 0:
		r.add(GuardNoOperations, "no other operation is queued or running", nil)
	case in.Force:
		r.add(GuardNoOperations, fmt.Sprintf("force: the reboot goes ahead although %d other operation(s) are queued "+
			"or running: %s", len(running), strings.Join(running, ", ")), nil)
	default:
		detail := fmt.Sprintf("%d other operation(s) are queued or running, and the reboot would end them "+
			"outcome-unknown: %s", len(running), strings.Join(running, ", "))
		r.add(GuardNoOperations, detail, problem.New(problem.OperationsRunning, detail).With("operationIds", running))
	}

	if d := p.daemon.Load(); d == nil || d.d == nil {
		detail := "the configuration cannot be checked: nexusd did not provide its configuration check"
		r.add(GuardConfigValid, detail, problem.New(problem.DependencyUnavailable, detail).With("provider", spec.Name))
	} else {
		path := d.d.ConfigPath()
		errs := configErrorsOf(d.d.CheckConfig())
		if len(errs) == 0 {
			r.add(GuardConfigValid, fmt.Sprintf("%s is valid, so nexusd starts after the boot", path), nil)
		} else {
			detail := fmt.Sprintf("%s has %d error(s), so nexusd would not start after the boot", path, len(errs))
			r.add(GuardConfigValid, detail, problem.New(problem.ConfigInvalid, detail).With("configPath", path).
				With("errors", errs))
		}
	}
	return r
}

// configErrorsOf is a configuration check's errors as config-invalid's
// errors extension carries them (the nexusd provider's shape).
func configErrorsOf(c provider.ConfigCheck) []nexusdspec.ConfigError {
	out := make([]nexusdspec.ConfigError, 0, len(c.Errors))
	for _, e := range c.Errors {
		if e == nil {
			continue
		}
		out = append(out, nexusdspec.ConfigError{File: e.File, Line: e.Line, Column: e.Column, Path: e.Path,
			Message: e.Msg})
	}
	return out
}

// selfService is the service whose start at boot brings nexusd back: the
// one running nexusd, as the last preflight resolved it, else nexusd.
func (p *Provider) selfService() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.res != nil && p.res.self != "" {
		return p.res.self
	}
	return selfName
}

// otherOperations are the IDs of the Queued and Running operations other
// than exclude, sorted.
func (p *Provider) otherOperations(exclude string) ([]string, error) {
	ref := p.ops.Load()
	if ref == nil || ref.o == nil {
		return nil, errors.New("the operation manager was not provided")
	}
	active, err := ref.o.Active()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(active))
	for _, a := range active {
		if a.ID != exclude {
			ids = append(ids, a.ID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// admitReboot is DF-4 step 1: every guard, refusing on the first that
// failed unless whatIf. The facts confirmHost, force and bootId go on the
// audit records (FR-16).
func (p *Provider) admitReboot(ctx context.Context, in spec.RebootInput) error {
	audit.AddFact(ctx, "confirmHost", in.ConfirmHost)
	audit.AddFact(ctx, "force", strconv.FormatBool(in.Force))
	boot := "unknown"
	if b, err := p.boot(); err == nil {
		boot = b.ID
	}
	audit.AddFact(ctx, "bootId", boot)
	if _, err := rebootDelay(in); err != nil {
		return err
	}
	r := p.rebootGuards(ctx, in, "")
	if in.WhatIf {
		return nil
	}
	return r.first()
}

// rebootDelay is the input's delay, the default when it is absent. The
// schema bounds it; this keeps a decoded input that bypassed the schema
// within them too.
func rebootDelay(in spec.RebootInput) (time.Duration, error) {
	if in.DelaySeconds == nil {
		return spec.DefaultDelaySeconds * time.Second, nil
	}
	d := *in.DelaySeconds
	if d < spec.MinDelaySeconds || d > spec.MaxDelaySeconds {
		msg := fmt.Sprintf("must be %d to %d", spec.MinDelaySeconds, spec.MaxDelaySeconds)
		return 0, problem.New(problem.ValidationFailed, "delaySeconds "+msg).
			With("errors", []problem.FieldError{{Path: "/delaySeconds", Message: msg}})
	}
	return time.Duration(d) * time.Second, nil
}

// rebootTimeout is how long the helper waits after the trigger for the host
// to go down: providers.services.reboot.timeout, else the default.
func rebootTimeout(snap *snapshot) time.Duration {
	if t := snap.cfg.Reboot.Timeout; t > 0 {
		return t
	}
	return spec.DefaultRebootTimeout
}

// prepareReboot is DF-4 step 3. It runs the guards again, without the
// operation's own ID, since time has passed in the lane, and refuses on the
// first that fails unless whatIf. It snapshots into input.json the delay,
// the wait, the init system, the boot ID, the kernel release and the
// reboot-required state, and for whatIf the report the helper returns.
// The argv is nexusd's absolute path and fixed words.
func (p *Provider) prepareReboot(ctx context.Context, in spec.RebootInput, r *provider.Run) (detach.Spec, error) {
	dir, err := p.helperOpDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	exe, err := p.helperExecutable()
	if err != nil {
		return detach.Spec{}, fmt.Errorf("services: locate nexusd: %w", err)
	}
	delay, err := rebootDelay(in)
	if err != nil {
		return detach.Spec{}, err
	}
	timeout := rebootTimeout(p.snapshot())
	report := p.rebootGuards(ctx, in, r.ID())
	if !in.WhatIf {
		if err := report.first(); err != nil {
			return detach.Spec{}, err
		}
	}
	boot, err := p.boot()
	if err != nil {
		return detach.Spec{}, problem.New(problem.DependencyUnavailable, fmt.Sprintf("the boot ID cannot be read "+
			"from %s: %v, so a reboot could not be confirmed; nothing was sent", bootIDPath, err)).
			With("provider", spec.Name)
	}
	release, err := p.readProc(procOSRelease)
	if err != nil {
		helperSay(r.Output(), "the kernel release cannot be read: %v", err)
	}
	hin := rebootHelperInput{Mode: helperModeReboot, OperationID: r.ID(), Init: string(p.plat.Init),
		DelaySeconds: int(delay / time.Second), TimeoutSeconds: int(timeout / time.Second), BootID: boot.ID,
		KernelRelease: release, RebootRequired: p.rebootRequiredNow(), WhatIf: in.WhatIf}
	if in.WhatIf {
		hin.Report = p.whatIfReport(report, hin)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("services: create %s: %w", dir, err)
	}
	if err := writeHelperJSON(dir, helperInputFile, hin); err != nil {
		p.log.Warn("services reboot not prepared", "operationId", r.ID(), "error", err.Error())
		return detach.Spec{}, err
	}
	if in.WhatIf {
		helperSay(r.Output(), "reboot whatIf runs in detached operation %s; nothing is rebooted", r.ID())
	} else {
		helperSay(r.Output(), "reboot of boot %s runs in detached operation %s in %s", boot, r.ID(), delay)
	}
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "run", "--dir", dir},
		Dir:        dir,
		Timeout:    delay + timeout + rebootTimeoutMargin,
		Grace:      rebootGrace,
		KillMode:   detach.KillProcess,
		MaxRuntime: delay + timeout + rebootMaxMargin,
	}, nil
}

// whatIfReport is the RebootResult whatIf returns: every guard, and the
// boot ID, uptime, kernel, reboot-required state, other operations and
// whether nexusd starts at boot, as Prepare read them.
func (p *Provider) whatIfReport(r rebootGuardReport, in rebootHelperInput) *spec.RebootResult {
	would := r.passed()
	out := &spec.RebootResult{WhatIf: true, WouldReboot: &would, Guards: slices.Clone(r.guards), BootID: in.BootID,
		KernelRelease: in.KernelRelease, RebootRequired: in.RebootRequired,
		RunningOperations: slices.Clone(r.running), NexusdEnabledAtBoot: r.nexusdAtBoot}
	if out.RunningOperations == nil {
		out.RunningOperations = []string{}
	}
	if b, err := p.readProc(procUptime); err == nil {
		if f, perr := strconv.ParseFloat(strings.Fields(b)[0], 64); perr == nil {
			out.UptimeSeconds = &f
		}
	}
	return out
}

// finalizeReboot reads result.json: a whatIf report, or the problem the
// helper ended with (R5).
func (p *Provider) finalizeReboot(_ context.Context, _ spec.RebootInput, x detach.Exit, r *provider.Run) (
	spec.RebootResult, error) {
	dir, err := p.helperOpDir(r.ID())
	if err != nil {
		return spec.RebootResult{}, err
	}
	return finalizeHelper[spec.RebootResult](dir, "reboot", x)
}

// rebootLostDetail is added to outcome-unknown when the helper's unit ended
// without exit.json and without a reboot record that R3 or R4 could act on.
func (p *Provider) rebootLostDetail(r *provider.Run, _ spec.RebootInput) string {
	prev := "previousBootId, the boot ID recorded when the reboot was prepared"
	if dir, err := p.helperOpDir(r.ID()); err == nil {
		var in rebootHelperInput
		if ok, err := readHelperJSON(dir, helperInputFile, &in); err == nil && ok && in.BootID != "" {
			prev = fmt.Sprintf("previousBootId %s, the boot ID recorded when the reboot was prepared", in.BootID)
		}
	}
	return fmt.Sprintf("the services helper for the reboot stopped without reporting a result, so the host may or "+
		"may not have rebooted; compare %s on the host with %s", bootIDPath, prev)
}

// afterReboot is DF-4 step 6: the result in the boot after the reboot,
// from the manager's boot IDs and times, the kernel release Prepare
// recorded and the one running now, and the reboot-required state now. It
// reads files only, so it is idempotent.
func (p *Provider) afterReboot(_ context.Context, _ spec.RebootInput, rb provider.Rebooted, r *provider.Run) (
	spec.RebootResult, error) {
	var in rebootHelperInput
	if dir, err := p.helperOpDir(r.ID()); err != nil {
		p.log.Warn("services reboot: no previous kernel release", "operationId", r.ID(), "error", err.Error())
	} else if ok, err := readHelperJSON(dir, helperInputFile, &in); err != nil || !ok {
		p.log.Warn("services reboot: no previous kernel release", "operationId", r.ID(), "found", ok, "error", err)
	}
	release, err := p.readProc(procOSRelease)
	if err != nil {
		p.log.Warn("services reboot: the kernel release cannot be read", "operationId", r.ID(), "error", err.Error())
	}
	requested, up := rb.RequestedAt, rb.ReadyAt
	return spec.RebootResult{ActionResult: apitypes.ActionResult{Changed: true}, PreviousBootID: rb.PreviousBootID,
		BootID: rb.BootID, RebootRequestedAt: &requested, UpAt: &up, PreviousKernelRelease: in.KernelRelease,
		KernelRelease: release, RebootRequired: p.rebootRequiredNow()}, nil
}

// rebootRequiredNow is the host provider's reboot-required rule over the
// provider's HostFS and platform (A-41).
func (p *Provider) rebootRequiredNow() spec.RebootRequired {
	plat := p.plat
	rr := host.RebootRequiredNow(host.Options{FS: p.hostFS, Platform: &plat, GOOS: p.goos, Logger: p.log})
	out := spec.RebootRequired{State: rr.State, Check: rr.Check, Evidence: rr.Evidence,
		Packages: append([]string{}, rr.Packages...)}
	if len(rr.Unavailable) > 0 {
		out.Unavailable = make(map[string]string, len(rr.Unavailable))
		for k, v := range rr.Unavailable {
			out.Unavailable[k] = v
		}
	}
	return out
}

// readProc is the first line of name under ProcRoot, trimmed.
func (p *Provider) readProc(name string) (string, error) {
	return readProcFile(p.procRoot, name)
}

// readProcFile is the content of name under procRoot ("" = /proc),
// trimmed; an empty file is an error.
func readProcFile(procRoot, name string) (string, error) {
	if procRoot == "" {
		procRoot = "/proc"
	}
	b, err := readRegular(filepath.Join(procRoot, name), maxSmallFileBytes)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", filepath.Join(procRoot, name))
	}
	return s, nil
}
