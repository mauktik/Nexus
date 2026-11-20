// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package scripts

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// MaxRuntimeMargin is added to a script's timeout and termination grace to
// give its maximum runtime (A-63): past it, the watcher reports
// outcome-unknown, so a supervisor that cannot enforce the timeout cannot
// leave the operation Running for ever.
const MaxRuntimeMargin = 60 * time.Second

// LaneKey is the lane of script id: its runs share the catalog's
// concurrency limit, and one more is refused with 409 (FR-19, FR-7).
func LaneKey(id string) string { return "script:" + id }

// MaxRuntime is the maximum runtime of a run of s.
func MaxRuntime(s Script) time.Duration { return s.Timeout + s.TerminationGrace + MaxRuntimeMargin }

// admitInvoke refuses, before an operation exists, an unknown script with
// not-found, parameters that fail their declarations with
// validation-failed, and a script that fails the integrity check (FR-20)
// with script-integrity-failed.
func (p *Provider) admitInvoke(_ context.Context, in spec.InvokeInput) error {
	s, ok := p.catalog.Load().Get(in.Name)
	if !ok {
		return notFound()
	}
	if _, _, err := s.arguments(in.Parameters); err != nil {
		return err
	}
	_, err := hostIntegrity.check(s.Path, s.RunAs)
	return err
}

// invokeLane is the script's reject lane, limited to its concurrency.
func (p *Provider) invokeLane(_ context.Context, in spec.InvokeInput) (provider.Lane, error) {
	s, ok := p.catalog.Load().Get(in.Name)
	if !ok {
		return provider.Lane{}, notFound()
	}
	return provider.Lane{Key: LaneKey(s.ID), Policy: provider.LaneReject, Limit: s.Concurrency}, nil
}

// prepareInvoke builds the process the supervisor runs: the script, by the
// path the integrity check resolved, executed directly with its mapped
// arguments and never through a shell (FR-25), as the run-as user with its
// supplementary groups (A-7), in its working directory, with the defined
// environment (A-8). The supervisor gives it stdin on /dev/null, no
// controlling terminal (Setsid) and piped output (A-5). Every Admit check
// runs again here, and the integrity check's resolved path is what runs, so
// a script changed or a link repointed after Admit is refused or not
// followed. The catalog entry is read now and travels in spec.json, so a
// later reload does not touch this run.
func (p *Provider) prepareInvoke(_ context.Context, in spec.InvokeInput, _ *provider.Run) (detach.Spec, error) {
	s, ok := p.catalog.Load().Get(in.Name)
	if !ok {
		return detach.Spec{}, notFound()
	}
	args, paramEnv, err := s.arguments(in.Parameters)
	if err != nil {
		return detach.Spec{}, err
	}
	resolved, err := hostIntegrity.check(s.Path, s.RunAs)
	if err != nil {
		return detach.Spec{}, err
	}
	// The supervisor's chdir failure would name only the script.
	if !p.machine.isDir(s.WorkingDirectory) {
		return detach.Spec{}, problem.New(problem.ScriptFailed, fmt.Sprintf("script %s was not started: its "+
			"working directory %s does not exist or is not a directory. Fix: create it, or correct the "+
			"catalog's workingDirectory", s.ID, s.WorkingDirectory)).
			With("exitCode", -1).With("signal", "").With("durationSeconds", 0.0)
	}
	id, err := p.machine.identify(s.RunAs)
	if err != nil {
		return detach.Spec{}, refuse(CheckRunAsUser, resolved, fmt.Sprintf("the run-as user %s cannot be resolved: %v. "+
			"Fix: create the user, or correct the catalog's runAs", s.RunAs, err))
	}
	var cred *detach.Credential
	// nexusd runs as root and switches to the run-as user. Only an
	// unprivileged nexusd running a script as itself, as on a development
	// machine, keeps its own identity, which it could not change.
	if euid := p.machine.geteuid(); euid == 0 || uint32(euid) != id.uid {
		cred = &detach.Credential{UID: id.uid, GID: id.gid, Groups: slices.Clone(id.groups)}
	}
	return detach.Spec{
		Argv:       append([]string{resolved}, args...),
		Env:        environment(id, p.machine.exists(runtimeDir(id.uid)), s.Env, paramEnv),
		Dir:        s.WorkingDirectory,
		Credential: cred,
		Timeout:    s.Timeout,
		Grace:      s.TerminationGrace,
		// Leftovers in the unit's cgroup are stopped when the script ends;
		// containers it started belong to Docker and are not (A-10).
		KillMode:   detach.KillControlGroup,
		MaxRuntime: MaxRuntime(s),
	}, nil
}

// finalizeInvoke states the outcome from exit.json alone (A-66): exit 0 is
// success, a timeout script-timeout, and any other end script-failed. The
// exit code is the only rule: Nexus does not read the script's output, so a
// script that exits 0 on failure reports success (A-6). Each outcome carries
// exitCode, signal and durationSeconds: in the result on success, in the
// error otherwise. A canceled run never reaches Finalize; it ends Canceled.
func finalizeInvoke(_ context.Context, in spec.InvokeInput, x detach.Exit, _ *provider.Run) (spec.InvokeResult, error) {
	d := duration(x)
	fail := func(slug problem.Slug, detail string) error {
		return problem.New(slug, detail).With("exitCode", x.ExitCode).With("signal", x.Signal).With("durationSeconds", d)
	}
	switch {
	case x.Error != "":
		return spec.InvokeResult{}, fail(problem.ScriptFailed, fmt.Sprintf("script %s could not be started: %s", in.Name, x.Error))
	case x.TimedOut:
		how := "and was stopped"
		if x.Signal != "" {
			how = "and was stopped by " + x.Signal
		}
		return spec.InvokeResult{}, fail(problem.ScriptTimeout, fmt.Sprintf("script %s passed its timeout %s", in.Name, how))
	case x.Signal != "":
		return spec.InvokeResult{}, fail(problem.ScriptFailed, fmt.Sprintf("script %s was killed by %s", in.Name, x.Signal))
	case x.ExitCode != 0:
		return spec.InvokeResult{}, fail(problem.ScriptFailed, fmt.Sprintf("script %s exited with code %d", in.Name, x.ExitCode))
	}
	return spec.InvokeResult{ActionResult: apitypes.ActionResult{Changed: true}, ExitCode: 0, DurationSeconds: d}, nil
}

// duration is how long the script ran, in seconds to the millisecond.
func duration(x detach.Exit) float64 {
	return math.Round(x.Ended.Sub(x.Started).Seconds()*1000) / 1000
}

// lostDetail is added to outcome-unknown when the run's unit ended without
// an exit.json: the script may have run in part.
func lostDetail(_ *provider.Run, in spec.InvokeInput) string {
	return fmt.Sprintf("script %s may have run in part; check what it changes before running it again", in.Name)
}

// SensitivePaths is the audit redactor's hook for this provider: the input
// paths of the invoked script's sensitive parameters, which the audit log
// and OpRecord.Input replace and which make a queued run unable to restart
// (NFR-4, A-66). For a script not in the catalog every supplied parameter
// is named, so a value is never kept by mistake.
func (p *Provider) SensitivePaths(action string, params json.RawMessage) []string {
	if action != spec.Invoke.Op().ID {
		return nil
	}
	var in struct {
		Name       string                     `json:"name"`
		Parameters map[string]json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(params, &in); err != nil {
		// Redact rejects input it cannot parse; nothing is kept.
		return nil
	}
	var paths []string
	s, ok := p.catalog.Load().Get(in.Name)
	if !ok {
		for name := range in.Parameters {
			paths = append(paths, "parameters."+name)
		}
		slices.Sort(paths)
		return paths
	}
	for _, prm := range s.Parameters {
		if prm.Sensitive {
			paths = append(paths, "parameters."+prm.Name)
		}
	}
	return paths
}
