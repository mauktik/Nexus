// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package apt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// apt.actions.install and apt.actions.remove (FR-16), under
// packages:install (FR-22). Like upgrade, a real run is an apt mutation on
// the apt lane through PrepareMutation and FinalizeMutation, and a whatIf
// run is the same command under apt-get -s on no lane (A-29).
//
// Two layers keep a name from being read as anything but that package.
// Layer 1 is the schema: the Debian name pattern, never a .deb file name,
// and a version pattern. Layer 2 resolves every name exactly, in Admit and
// again in Prepare once the lane and lock wait are over (names.go):
// against apt's indexes for install, against dpkg for remove. apt-get
// reads "nxplain-" given to install as a removal, "nxnewdep+" given to
// remove as an install, and "nxplai." as a regular expression (S-4;
// captured in testdata/install, hostile-*), and none of them resolves.

var versionInputRE = regexp.MustCompile(spec.VersionPattern)

// installCommand is apt-get's install command after the fixed options. It
// passes --no-remove: an install that needs a removal fails instead
// (A-51). A version pins the single name, as name=version.
func installCommand(names []string, version string) []string {
	args := slices.Concat([]string{"install", "--no-remove", "--"}, names)
	if version != "" && len(names) == 1 {
		args[len(args)-1] += "=" + version
	}
	return args
}

// removeCommand is apt-get's remove or purge command after the fixed options,
// with --autoremove when asked: purge and autoremove are off by default.
func removeCommand(names []string, purge, autoremove bool) []string {
	cmd := "remove"
	if purge {
		cmd = "purge"
	}
	args := []string{cmd}
	if autoremove {
		args = append(args, "--autoremove")
	}
	return slices.Concat(args, []string{"--"}, names)
}

func (p *Provider) installBinding() provider.Binding {
	return provider.Detached(spec.Install, provider.DetachedSpec[spec.InstallInput, spec.PackagesResult]{
		Admit:    p.admitInstall,
		Lane:     installLane,
		Prepare:  p.prepareInstall,
		Finalize: p.finalizeInstall,
		LostDetail: func(_ *provider.Run, in spec.InstallInput) string {
			return lostPackages(in.WhatIf, "installed")
		},
	})
}

func (p *Provider) removeBinding() provider.Binding {
	return provider.Detached(spec.Remove, provider.DetachedSpec[spec.RemoveInput, spec.PackagesResult]{
		Admit:    p.admitRemove,
		Lane:     removeLane,
		Prepare:  p.prepareRemove,
		Finalize: p.finalizeRemove,
		LostDetail: func(_ *provider.Run, in spec.RemoveInput) string {
			return lostPackages(in.WhatIf, "removed")
		},
	})
}

func installLane(ctx context.Context, in spec.InstallInput) (provider.Lane, error) {
	if in.WhatIf {
		return provider.NoLane, nil
	}
	return aptLane(ctx, in)
}

func removeLane(ctx context.Context, in spec.RemoveInput) (provider.Lane, error) {
	if in.WhatIf {
		return provider.NoLane, nil
	}
	return aptLane(ctx, in)
}

// checkNames is layer 1 for install's and remove's names, which the schema
// already enforces: at least one, at most MaxInstallNames, each a valid
// name that is no .deb file name.
func checkNames(names []string) error {
	if len(names) == 0 {
		return invalid("/names", "must name at least one package")
	}
	if len(names) > spec.MaxInstallNames {
		return invalid("/names", fmt.Sprintf("must have at most %d names", spec.MaxInstallNames))
	}
	for i, n := range names {
		if !validInstallName(n) {
			return invalid(fmt.Sprintf("/names/%d", i), "must match "+spec.NamePattern+" and not name a .deb file")
		}
	}
	return nil
}

func checkInstall(in spec.InstallInput) error {
	if err := checkNames(in.Names); err != nil {
		return err
	}
	if in.Version == "" {
		return nil
	}
	if !versionInputRE.MatchString(in.Version) || len(in.Version) > spec.MaxVersionLength {
		return invalid("/version", "must match "+spec.VersionPattern)
	}
	if len(in.Names) != 1 {
		return invalid("/version", "pins exactly one package: name one package with a version")
	}
	return nil
}

// admitInstall refuses bad input, and a name that is not exactly a package
// apt knows, before any operation exists: with whatIf too.
func (p *Provider) admitInstall(ctx context.Context, in spec.InstallInput) error {
	if err := checkInstall(in); err != nil {
		return err
	}
	_, err := p.resolveAvailable(ctx, in.Names)
	return err
}

// admitRemove refuses bad input, and a name not installed under exactly
// that name, before any operation exists: with whatIf too.
func (p *Provider) admitRemove(ctx context.Context, in spec.RemoveInput) error {
	if err := checkNames(in.Names); err != nil {
		return err
	}
	_, err := p.resolveRemovable(ctx, in.Names, in.Purge)
	return err
}

func (p *Provider) prepareInstall(ctx context.Context, in spec.InstallInput, r *provider.Run) (detach.Spec, error) {
	if err := checkInstall(in); err != nil {
		return detach.Spec{}, err
	}
	resolve := func() ([]string, error) { return p.resolveAvailable(ctx, in.Names) }
	return p.preparePackages(ctx, r, in.WhatIf, resolve, func(names []string) []string {
		return installCommand(names, in.Version)
	})
}

func (p *Provider) prepareRemove(ctx context.Context, in spec.RemoveInput, r *provider.Run) (detach.Spec, error) {
	if err := checkNames(in.Names); err != nil {
		return detach.Spec{}, err
	}
	resolve := func() ([]string, error) { return p.resolveRemovable(ctx, in.Names, in.Purge) }
	return p.preparePackages(ctx, r, in.WhatIf, resolve, func(names []string) []string {
		return removeCommand(names, in.Purge, in.Autoremove)
	})
}

// preparePackages resolves the names and builds the unit: the simulation
// for whatIf, otherwise the mutation, whose names resolve again once the
// lock wait is over. A name whose package changed meanwhile (a refresh
// queued before it, or another process) would reach apt-get as a name apt
// reads another way, so it is not-found and nothing starts.
func (p *Provider) preparePackages(ctx context.Context, r *provider.Run, whatIf bool, resolve func() ([]string, error),
	args func(names []string) []string) (detach.Spec, error) {
	names, err := resolve()
	if err != nil {
		return detach.Spec{}, err
	}
	if whatIf {
		return p.preparePackagesPreview(ctx, r, args(names))
	}
	s, err := p.PrepareMutation(ctx, r, args(names))
	if err != nil {
		return detach.Spec{}, err
	}
	again, err := resolve()
	if err != nil {
		return detach.Spec{}, err
	}
	if !slices.Equal(again, names) {
		return detach.Spec{}, problem.New(problem.NotFound, "the named packages changed while the operation waited; "+
			"nothing was started")
	}
	return s, nil
}

// preparePackagesPreview records every package dpkg knows with its
// architecture, which a Remv or Purg line does not print, and returns the
// simulation's unit. It takes no lock and runs no audit.
func (p *Provider) preparePackagesPreview(ctx context.Context, r *provider.Run, args []string) (detach.Spec, error) {
	res, err := p.run(ctx, cmdDpkgQueryAll())
	if err != nil || res.ExitCode != 0 {
		return detach.Spec{}, p.failed("dpkg-query", res, err)
	}
	native, err := p.architecture(ctx)
	if err != nil {
		return detach.Spec{}, unavailable(err.Error())
	}
	st := previewState{Upgradable: []spec.HeldBackPackage{}, Architectures: map[string]string{}}
	for _, d := range parseDpkgQuery(res.Stdout) {
		st.Architectures[qualifiedName(d.Package, d.Architecture, native)] = d.Architecture
	}
	return p.writePreviewSpec(r, st, args)
}

// writePreviewSpec records st as preview.json in the operation's directory
// and returns the unit of the simulation of args: apt-get -s, which takes
// no lock and changes nothing.
func (p *Provider) writePreviewSpec(r *provider.Run, st previewState, args []string) (detach.Spec, error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("apt: create %s: %w", dir, err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return detach.Spec{}, fmt.Errorf("apt: encode %s: %w", PreviewFile, err)
	}
	if err := writeFileAtomic(dir, PreviewFile, b); err != nil {
		return detach.Spec{}, err
	}
	return detach.Spec{
		Argv:       previewArgv(args),
		Env:        Env(),
		Dir:        "/",
		KillMode:   detach.KillProcess,
		MaxRuntime: p.Settings().MaxRuntime,
	}, nil
}

func (p *Provider) finalizeInstall(_ context.Context, in spec.InstallInput, x detach.Exit, r *provider.Run) (
	spec.PackagesResult, error) {
	return p.finalizePackages(in.WhatIf, x, r)
}

func (p *Provider) finalizeRemove(_ context.Context, in spec.RemoveInput, x detach.Exit, r *provider.Run) (
	spec.PackagesResult, error) {
	return p.finalizePackages(in.WhatIf, x, r)
}

// finalizePackages reads files only, so it answers the same after a
// restart of nexusd: the preview from the simulation's Inst, Remv and Purg
// lines for whatIf, otherwise FinalizeMutation's outcome.
func (p *Provider) finalizePackages(whatIf bool, x detach.Exit, r *provider.Run) (spec.PackagesResult, error) {
	if whatIf {
		if perr := p.packagesFailure("apt-get -s", x, r); perr != nil {
			return spec.PackagesResult{}, perr
		}
		st, acts, err := p.readSimulation(r)
		if err != nil {
			return spec.PackagesResult{}, err
		}
		pv := buildPreview(acts, st)
		reboot, pkgs := p.rebootState()
		return spec.PackagesResult{
			WhatIf: true, Preview: &spec.PackagesPreview{Installed: pv.Installed, Upgraded: pv.Upgraded, Removed: pv.Removed},
			Changes: []spec.PackageChange{}, ConffilesKept: []string{}, ConffilesComplete: true,
			RebootRequired: reboot, RebootRequiredPackages: pkgs, ExitCode: x.ExitCode,
		}, nil
	}
	out, err := p.finalizeMutation(r, x, p.packagesFailure)
	if err != nil {
		return spec.PackagesResult{}, withOutcome(err, out)
	}
	changes := packageChanges(out.Changes)
	return spec.PackagesResult{
		ActionResult:  apitypes.ActionResult{Changed: len(changes) > 0},
		Changes:       changes,
		ConffilesKept: out.Conffiles, ConffilesComplete: out.ConffilesComplete,
		RebootRequired: out.RebootRequired, RebootRequiredPackages: out.RebootPackages,
		ExitCode: out.ExitCode,
	}, nil
}

// apt-get's error lines (exit status 100) for an install it refused to
// run as asked, captured from apt 3.0 and 3.2 in testdata/install
// (install-conflict, install-downgrade). Nothing was changed when either
// is printed: apt-get stops before it runs dpkg.
var (
	removalRefused   = []byte("E: Packages need to be removed but remove is disabled.")
	downgradeRefused = []byte("E: Packages were downgraded and -y was used without --allow-downgrades.")
)

// packagesFailure is exitFailure, with a clear detail when apt-get refused
// an install because it would need to remove a package (--no-remove, A-51)
// or to downgrade one.
func (p *Provider) packagesFailure(what string, x detach.Exit, r *provider.Run) *problem.Error {
	stderr := p.exitStderr(x, r)
	perr := aptFailure(what, x, stderr)
	if perr == nil || perr.Slug() != problem.AptFailed || len(stderr) == 0 {
		return perr
	}
	for line := range lines(stderr) {
		switch l := bytes.TrimSpace([]byte(line)); {
		case bytes.Equal(l, removalRefused):
			return problem.New(problem.AptFailed, fmt.Sprintf("%s exited with status %d: installing these packages "+
				"needs packages removed, and install never removes one; nothing was changed. The operation log lists "+
				"the packages it would remove: remove them with apt.actions.remove first, if that is intended", what,
				x.ExitCode))
		case bytes.Equal(l, downgradeRefused):
			return problem.New(problem.AptFailed, fmt.Sprintf("%s exited with status %d: the version asked for is "+
				"older than the installed one, and install never downgrades a package; nothing was changed", what,
				x.ExitCode))
		}
	}
	return perr
}

func lostPackages(whatIf bool, verb string) string {
	if whatIf {
		return "the simulation changed nothing; run the preview again"
	}
	return "apt-get may not have finished, and packages may be partly " + verb + "; dpkg --audit on the host shows " +
		"an interrupted state, which Nexus does not repair"
}
