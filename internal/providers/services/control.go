package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// Start, stop, restart and control (DF-3) run detached, as `nexusd
// services-helper run --dir ops/<id>`, outside nexusd's sandbox (D-5).
// Admission refuses, before any operation exists and with nothing sent,
// a protected name, an unknown one, a protected canonical unit and a name
// or action the allow-list does not grant, in that order. Prepare
// snapshots the entry and the resolved protected set into input.json, the
// helper re-checks them before it acts, and Finalize only reads
// result.json.

// controlConfigKey is the configuration that grants start, stop and
// restart.
const controlConfigKey = "providers.services.control"

// The bounds of the helper's spec: MaxRuntime is the entry's timeout plus
// two svcctl calls (the read before the action and the action itself) plus
// controlMargin. The supervisor stops the helper controlSlack before it, so
// the watcher's backstop never races the supervisor's timeout.
const (
	controlMargin = 60 * time.Second
	controlSlack  = 30 * time.Second
	controlGrace  = 10 * time.Second
)

// controlLane is the lane of every action on name: one at a time per
// service, a second answering 409 conflict.
func controlLane(name string) provider.Lane {
	return provider.Lane{Key: "service:" + name, Policy: provider.LaneReject}
}

// controlBindings binds start, stop, restart and control. control is the
// per-verb ops with the action as input: the same admission, lane, helper
// mode and result.
func (p *Provider) controlBindings() []provider.Binding {
	verb := func(op def.Typed[spec.NameInput, spec.ServiceResult], action string) provider.Binding {
		return provider.Detached(op, provider.DetachedSpec[spec.NameInput, spec.ServiceResult]{
			Admit: func(ctx context.Context, in spec.NameInput) error { return p.admitControl(ctx, in.Name, action) },
			Lane: func(_ context.Context, in spec.NameInput) (provider.Lane, error) {
				return controlLane(in.Name), nil
			},
			Prepare: func(ctx context.Context, in spec.NameInput, r *provider.Run) (detach.Spec, error) {
				return p.prepareControl(ctx, in.Name, action, r)
			},
			Finalize: func(_ context.Context, in spec.NameInput, x detach.Exit, r *provider.Run) (spec.ServiceResult, error) {
				return p.finalizeControl(in.Name, action, x, r)
			},
			LostDetail: func(_ *provider.Run, in spec.NameInput) string { return controlLostDetail(in.Name, action) },
		})
	}
	return []provider.Binding{
		verb(spec.Start, spec.ActionStart),
		verb(spec.Stop, spec.ActionStop),
		verb(spec.Restart, spec.ActionRestart),
		provider.Detached(spec.Control, provider.DetachedSpec[spec.ControlInput, spec.ServiceResult]{
			Admit: func(ctx context.Context, in spec.ControlInput) error {
				return p.admitControl(ctx, in.Name, in.Action)
			},
			Lane: func(_ context.Context, in spec.ControlInput) (provider.Lane, error) {
				return controlLane(in.Name), nil
			},
			Prepare: func(ctx context.Context, in spec.ControlInput, r *provider.Run) (detach.Spec, error) {
				return p.prepareControl(ctx, in.Name, in.Action, r)
			},
			Finalize: func(_ context.Context, in spec.ControlInput, x detach.Exit, r *provider.Run) (spec.ServiceResult, error) {
				return p.finalizeControl(in.Name, in.Action, x, r)
			},
			LostDetail: func(_ *provider.Run, in spec.ControlInput) string {
				return controlLostDetail(in.Name, in.Action)
			},
		}),
	}
}

// admitControl is DF-3 step 1, on one settings snapshot. The facts service
// and action go on the audit records (FR-16).
func (p *Provider) admitControl(ctx context.Context, name, action string) error {
	audit.AddFact(ctx, "service", name)
	audit.AddFact(ctx, "action", action)
	snap := p.snapshot()
	_, _, err := p.checkControl(ctx, snap, name, action)
	return err
}

// checkControl checks name and action against snap in the DF-3 order and
// returns the allow-list entry and the service's canonical name. The
// service pattern is the schema's; it is checked again so svcctl never
// sees a name it refuses.
func (p *Provider) checkControl(ctx context.Context, snap *snapshot, name, action string) (entry controlEntry,
	canonical string, err error) {
	if !serviceName.MatchString(name) {
		return entry, "", problem.New(problem.ValidationFailed, fmt.Sprintf("%q is not a service name", name)).
			With("errors", []problem.FieldError{{Path: "/name", Message: "must match " + spec.ServiceNamePattern}})
	}
	if ok, why := snap.prot.protected(name, ""); ok {
		return entry, "", protectedProblem(name, why)
	}
	d, err := svcctl.Describe(ctx, p.plat.Init, name, p.svcctlOptions())
	if err != nil {
		return entry, "", problem.New(problem.DependencyUnavailable, fmt.Sprintf("service %s cannot be read from %s: %v",
			name, p.plat.Init, err)).With("provider", spec.Name)
	}
	if !d.Known {
		return entry, "", problem.New(problem.NotFound, fmt.Sprintf("%s has no service %s", p.plat.Init, name))
	}
	if ok, why := snap.prot.protected(name, d.CanonicalName); ok {
		return entry, "", protectedProblem(name, why)
	}
	e, ok := snap.allow[name]
	if !ok {
		return entry, "", notAllowedProblem(name, action, fmt.Sprintf("%s is not in %s, which only the configuration "+
			"file on the host changes", name, controlConfigKey))
	}
	if !slices.Contains(e.Actions, action) {
		return entry, "", notAllowedProblem(name, action, fmt.Sprintf("%s allows %v on %s, not %s", controlConfigKey,
			e.Actions, name, action))
	}
	return controlEntry{actions: slices.Clone(e.Actions), timeout: e.Timeout}, d.CanonicalName, nil
}

// controlEntry is the part of an allow-list entry the helper acts on.
type controlEntry struct {
	actions []string
	timeout time.Duration
}

func protectedProblem(name, why string) *problem.Error {
	return problem.New(problem.ProtectedService, fmt.Sprintf("%s is protected: %s", name, why)).
		With("service", name).With("reason", why)
}

func notAllowedProblem(name, action, detail string) *problem.Error {
	return problem.New(problem.NotAllowed, detail).With("service", name).With("action", action).
		With("configKey", controlConfigKey)
}

// prepareControl is DF-3 step 3. It runs admission again on the settings
// in force now, since a reload may have come since, and snapshots what it
// decided on into input.json: the entry, the init system, the canonical
// name and the resolved protected set. The argv is nexusd's absolute path
// and fixed words: no shell and nothing from the request.
func (p *Provider) prepareControl(ctx context.Context, name, action string, r *provider.Run) (detach.Spec, error) {
	dir, err := p.helperOpDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	exe, err := p.helperExecutable()
	if err != nil {
		return detach.Spec{}, fmt.Errorf("services: locate nexusd: %w", err)
	}
	snap := p.snapshot()
	e, canonical, err := p.checkControl(ctx, snap, name, action)
	if err != nil {
		return detach.Spec{}, err
	}
	in := controlHelperInput{Mode: helperModeControl, OperationID: r.ID(), Service: name, Action: action,
		Init: string(p.plat.Init), CanonicalName: canonical, TimeoutSeconds: e.timeout.Seconds(), Actions: e.actions,
		Protected: slices.Clone(snap.prot.entries)}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("services: create %s: %w", dir, err)
	}
	if err := writeHelperJSON(dir, helperInputFile, in); err != nil {
		p.log.Warn("services operation not prepared", "operationId", r.ID(), "service", name, "action", action,
			"error", err.Error())
		return detach.Spec{}, err
	}
	helperSay(r.Output(), "%s of %s runs in detached operation %s", action, name, r.ID())
	limit := e.timeout + 2*svcctl.DefaultTimeout + controlMargin
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "run", "--dir", dir},
		Dir:        dir,
		Timeout:    limit - controlSlack,
		Grace:      controlGrace,
		KillMode:   detach.KillProcess,
		MaxRuntime: limit,
	}, nil
}

// finalizeControl is DF-3 step 5: result.json's result or problem.
func (p *Provider) finalizeControl(name, action string, x detach.Exit, r *provider.Run) (spec.ServiceResult, error) {
	dir, err := p.helperOpDir(r.ID())
	if err != nil {
		return spec.ServiceResult{}, err
	}
	return finalizeHelper[spec.ServiceResult](dir, action+" of "+name, x)
}

// controlLostDetail is added to outcome-unknown when the helper's unit
// ended without exit.json.
func controlLostDetail(name, action string) string {
	return fmt.Sprintf("the services helper for the %s of %s stopped without reporting a result, so whether %s "+
		"changed is unknown; read its state with `nexus service get %s`", action, name, name, name)
}

// helperOpDir is operation id's directory.
func (p *Provider) helperOpDir(id string) (string, error) {
	dir := p.opsDir.Load()
	if dir == nil || *dir == "" {
		return "", errors.New("services: no operations directory: UseOpsDir was not called")
	}
	return filepath.Join(*dir, id), nil
}

// helperExecutable is the nexusd that runs the helper: Options.Executable,
// else this process's executable.
func (p *Provider) helperExecutable() (string, error) {
	if p.executable != "" {
		return p.executable, nil
	}
	return os.Executable()
}
