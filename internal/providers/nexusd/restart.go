// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

// restart (F9): a detached operation. Admit refuses a host without
// self-management and an on-disk configuration nexusd could not start with
// (A-23). Prepare writes input.json with the health target of that
// configuration; the service helper, outside nexusd.service, notes the
// running MainPID, restarts the unit with --no-block and runs the FR-10
// health check against any version, excluding that PID (A-41). There is no
// rollback: an unhealthy restart fails restart-unhealthy naming the
// condition. Finalize, in the nexusd running afterwards, reads result.json.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// modeRestart is the helper's mode for restart.
const modeRestart = "restart"

// restartRuntimeSlack is what restart's MaxRuntime allows beyond the health
// check: the helper's systemctl calls (30 s each), its -version runs and
// the supervisor (A-8).
const restartRuntimeSlack = 75 * time.Second

// restartMaxRuntime is the backstop of a restart whose health check runs
// for t.Timeout plus the stability window: healthTimeout + 5s + 75s (A-8).
func restartMaxRuntime(t healthTarget) time.Duration {
	return t.Timeout + t.Stability + restartRuntimeSlack
}

var _ provider.OpsDirUser = (*Provider)(nil)

// UseOpsDir sets the directory of the operations' directories. It
// implements provider.OpsDirUser.
func (p *Provider) UseOpsDir(dir string) { p.opsDir.Store(&dir) }

// opDir is the directory of operation id.
func (p *Provider) opDir(id string) (string, error) {
	dir := p.opsDir.Load()
	if dir == nil || *dir == "" {
		return "", errors.New("nexusd: no operations directory: UseOpsDir was not called")
	}
	return filepath.Join(*dir, id), nil
}

// restartBinding binds nexusd.actions.restart: detached, in the nexusd lane.
func (p *Provider) restartBinding() provider.Binding {
	return provider.Detached(spec.Restart, provider.DetachedSpec[spec.RestartInput, spec.RestartResult]{
		Admit:      p.admitRestart,
		Lane:       laneOf[spec.RestartInput],
		Prepare:    p.prepareRestart,
		Finalize:   p.finalizeRestart,
		LostDetail: p.lostRestartDetail,
	})
}

// admitRestart refuses, before an operation exists, a host without
// self-management (capability-unavailable with the reason), then a
// configuration file that fails the FR-3 check (config-invalid, A-23):
// nexusd would not start again with it.
func (p *Provider) admitRestart(context.Context, spec.RestartInput) error {
	if err := p.requireSelfManagement(); err != nil {
		return err
	}
	d, err := p.daemonOf()
	if err != nil {
		return err
	}
	return configValid(d.ConfigPath(), d.CheckConfig().Errors)
}

// prepareRestart writes input.json and returns the helper's spec. It checks
// the configuration again, since the file may have changed since Admit, and
// takes the health target from it (A-24).
func (p *Provider) prepareRestart(_ context.Context, _ spec.RestartInput, r *provider.Run) (detach.Spec, error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	d, err := p.daemonOf()
	if err != nil {
		return detach.Spec{}, err
	}
	check := d.CheckConfig()
	if err := configValid(d.ConfigPath(), check.Errors); err != nil {
		return detach.Spec{}, err
	}
	if check.Config == nil {
		return detach.Spec{}, fmt.Errorf("nexusd: %s passed its check but was not loaded", d.ConfigPath())
	}
	target, err := targetFrom(check.Config, d.StateDir())
	if err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: health target: %w", err)
	}
	exe, err := p.executable()
	if err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: locate nexusd: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: create %s: %w", dir, err)
	}
	in := helperInput{Mode: modeRestart, Health: target, Paths: p.paths}
	if err := writeJSONAtomic(dir, inputFileName, in, nil); err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: %w", err)
	}
	fmt.Fprintf(r.Output(), "restarting %s from unit %s; health check against %s within %s\n", p.paths.Unit,
		detach.Unit(r.ID()), target.URL, target.Timeout)
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "--dir", dir},
		Dir:        dir,
		KillMode:   detach.KillProcess,
		MaxRuntime: restartMaxRuntime(target),
	}, nil
}

// helperRestartMode is the helper's restart: note the running MainPID,
// restart the unit with --no-block, and wait for a new, healthy process of
// any version. Nothing is rolled back.
func helperRestartMode(ctx context.Context, h *helperRun, in helperInput) helperResult {
	unit := in.Paths.withDefaults().Unit
	unhealthy := func(cond int, detail string) helperResult {
		_, _ = fmt.Fprintln(h.errOut, detail) // A lost log line never changes the outcome, as in step
		return helperResult{Error: &helperError{Slug: problem.RestartUnhealthy, Detail: detail, Condition: cond}}
	}
	st, err := h.sys.Show(ctx)
	if err != nil {
		return unhealthy(0, fmt.Sprintf("cannot read %s before restarting it (%v); it was not restarted", unit, err))
	}
	pid0 := st.MainPID
	step(h.out, fmt.Sprintf("restarting %s (MainPID %d, %s)", unit, pid0, st.ActiveState))
	if err := h.sys.Restart(ctx); err != nil {
		return unhealthy(0, fmt.Sprintf("systemd refused to restart %s: %v", unit, err))
	}
	step(h.out, fmt.Sprintf("health check: %s within %s", in.Health.URL, in.Health.Timeout))
	version, err := waitHealthy(ctx, h.sys, in.Health, "", pid0)
	if err != nil {
		var ce *conditionError
		if errors.As(err, &ce) {
			return unhealthy(ce.Condition, ce.Error())
		}
		return unhealthy(0, err.Error())
	}
	step(h.out, fmt.Sprintf("%s is healthy, running nexusd %s", unit, version))
	return helperResult{Changed: true, Version: version}
}

// finalizeRestart states the outcome from result.json. It only reads files.
func (p *Provider) finalizeRestart(_ context.Context, in spec.RestartInput, x detach.Exit, r *provider.Run) (spec.RestartResult, error) {
	var none spec.RestartResult
	dir, err := p.opDir(r.ID())
	if err != nil {
		return none, err
	}
	res, ok, err := readResult(dir)
	if err != nil {
		return none, err
	}
	if !ok {
		if x.Error != "" {
			return none, problem.New(problem.InternalError, fmt.Sprintf("the service helper could not be started "+
				"(%s); nexusd was not restarted", x.Error))
		}
		how := fmt.Sprintf("exited with code %d", x.ExitCode)
		if x.Signal != "" {
			how = "was killed by " + x.Signal
		}
		return none, problem.New(problem.OutcomeUnknown, fmt.Sprintf("the service helper %s without reporting a "+
			"result; %s", how, p.lostRestartDetail(r, in)))
	}
	if res.Error != nil {
		return none, res.problem()
	}
	return spec.RestartResult{ActionResult: apitypes.ActionResult{Changed: true}, Version: res.Version}, nil
}

// lostRestartDetail is what to check by hand after a restart that stopped
// without a result, naming the paths of input.json when it can be read, and
// the provider's own otherwise. It only reads a file.
func (p *Provider) lostRestartDetail(r *provider.Run, _ spec.RestartInput) string {
	paths := p.paths
	if dir, err := p.opDir(r.ID()); err == nil {
		var in helperInput
		if b, err := os.ReadFile(filepath.Join(dir, inputFileName)); err == nil && json.Unmarshal(b, &in) == nil {
			paths = in.Paths.withDefaults()
		}
	}
	return fmt.Sprintf("%s may or may not have restarted. Check by hand: `systemctl status %s` shows whether it is "+
		"active and since when, `%s -version` the installed version, and `ls -l %s` whether the rollback binary "+
		"exists (a restart never changes it). Nothing was rolled back", paths.Unit, paths.Unit, paths.Binary, paths.Prev)
}
