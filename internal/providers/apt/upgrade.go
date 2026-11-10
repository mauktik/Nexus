package apt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// apt.actions.upgrade (FR-15). A real run is an apt mutation: it queues on
// the apt lane and goes through PrepareMutation and FinalizeMutation. A
// whatIf run is apt-get -s on no lane (A-29: a simulation takes no lock),
// whose Finalize parses the Inst and Remv lines from the operation's
// output. Nothing here ever reboots or schedules a reboot.

// PreviewFile is what a whatIf run's Prepare writes into the operation's
// directory: the upgradable packages before the simulation, from which
// Finalize computes heldBack.
const PreviewFile = "preview.json"

// maxPreviewBytes bounds the simulation output Finalize reads.
const maxPreviewBytes = 64 << 20

// previewState is preview.json.
type previewState struct {
	Upgradable []spec.HeldBackPackage `json:"upgradable"`
	// Architectures maps each installed package, named as apt-get -s names
	// it, to its architecture, which a Remv line does not print.
	Architectures map[string]string `json:"architectures"`
}

// upgradeArgs is apt-get's command and arguments after the fixed options,
// for mode and the resolved names (none: every package). Standard never
// removes: apt-get upgrade never does, and a subset passes --no-remove,
// since install --only-upgrade may remove to satisfy a dependency.
func upgradeArgs(mode string, names []string) []string {
	switch {
	case len(names) == 0 && mode == spec.ModeFull:
		return []string{"dist-upgrade"}
	case len(names) == 0:
		return []string{"upgrade"}
	case mode == spec.ModeFull:
		return slices.Concat([]string{"install", "--only-upgrade", "--"}, names)
	default:
		return slices.Concat([]string{"install", "--only-upgrade", "--no-remove", "--"}, names)
	}
}

// previewArgv is the simulation of upgradeArgs: apt-get -s, with the
// package cache built in memory, so the run writes nothing.
func previewArgv(args []string) []string {
	return slices.Concat([]string{AptGet, "-s", "-y"}, noCacheFiles, []string{"-o", "APT::Color=0"}, args)
}

func (p *Provider) upgradeBinding() provider.Binding {
	return provider.Detached(spec.Upgrade, provider.DetachedSpec[spec.UpgradeInput, spec.UpgradeResult]{
		Admit:      p.admitUpgrade,
		Lane:       upgradeLane,
		Prepare:    p.prepareUpgrade,
		Finalize:   p.finalizeUpgrade,
		LostDetail: lostUpgrade,
	})
}

// admitUpgrade refuses a bad mode, and a name that does not resolve
// exactly to an installed package, before any operation exists: with
// whatIf too, and in both modes.
func (p *Provider) admitUpgrade(ctx context.Context, in spec.UpgradeInput) error {
	if !slices.Contains(spec.Modes, in.EffectiveMode()) {
		return invalid("/mode", "must be one of "+strings.Join(spec.Modes, ", "))
	}
	if len(in.Names) > spec.MaxUpgradeNames {
		return invalid("/names", fmt.Sprintf("must have at most %d names", spec.MaxUpgradeNames))
	}
	_, err := p.resolveInstalled(ctx, in.Names)
	return err
}

// upgradeLane is the apt lane, except for whatIf, which waits for nothing.
func upgradeLane(ctx context.Context, in spec.UpgradeInput) (provider.Lane, error) {
	if in.WhatIf {
		return provider.NoLane, nil
	}
	return aptLane(ctx, in)
}

func (p *Provider) prepareUpgrade(ctx context.Context, in spec.UpgradeInput, r *provider.Run) (detach.Spec, error) {
	if in.WhatIf {
		return p.preparePreview(ctx, in, r)
	}
	// The names resolve again once the wait is over: a package removed while
	// the operation queued would otherwise reach apt-get as a name apt reads
	// another way.
	resolve := func() ([]string, error) { return p.resolveInstalled(ctx, in.Names) }
	return p.preparePackages(ctx, r, false, resolve, func(names []string) []string {
		return upgradeArgs(in.EffectiveMode(), names)
	})
}

// preparePreview records the upgradable packages (of the named ones, for a
// subset) and returns the simulation's unit. It takes no lock and runs no
// audit: the simulation changes nothing.
func (p *Provider) preparePreview(ctx context.Context, in spec.UpgradeInput, r *provider.Run) (detach.Spec, error) {
	names, err := p.resolveInstalled(ctx, in.Names)
	if err != nil {
		return detach.Spec{}, err
	}
	ups, err := p.upgradableAll(ctx)
	if err != nil {
		return detach.Spec{}, err
	}
	installed, err := p.installed(ctx)
	if err != nil {
		return detach.Spec{}, err
	}
	st := previewState{Upgradable: []spec.HeldBackPackage{}, Architectures: make(map[string]string, len(installed))}
	for _, i := range installed {
		st.Architectures[i.Name] = i.Architecture
	}
	for _, u := range ups {
		if len(names) > 0 && !slices.Contains(names, u.Name) {
			continue
		}
		st.Upgradable = append(st.Upgradable, spec.HeldBackPackage{Package: u.Properties.Package,
			Architecture: u.Properties.Architecture, InstalledVersion: u.Properties.InstalledVersion,
			CandidateVersion: u.Properties.CandidateVersion})
	}
	return p.writePreviewSpec(r, st, upgradeArgs(in.EffectiveMode(), names))
}

// finalizeUpgrade reads files only, so it answers the same after a restart
// of nexusd. A failed real run still changed what dpkg's status shows; the
// operation log has apt-get's output.
func (p *Provider) finalizeUpgrade(_ context.Context, in spec.UpgradeInput, x detach.Exit, r *provider.Run) (
	spec.UpgradeResult, error) {
	if in.WhatIf {
		return p.finalizePreview(in, x, r)
	}
	out, err := p.FinalizeMutation(r, x)
	if err != nil {
		return spec.UpgradeResult{}, err
	}
	changes := make([]spec.PackageChange, len(out.Changes))
	for i, c := range out.Changes {
		changes[i] = spec.PackageChange(c)
	}
	return spec.UpgradeResult{
		ActionResult: apitypes.ActionResult{Changed: len(changes) > 0},
		Mode:         in.EffectiveMode(), Changes: changes,
		ConffilesKept: out.Conffiles, ConffilesComplete: out.ConffilesComplete,
		RebootRequired: out.RebootRequired, RebootRequiredPackages: out.RebootPackages,
		ExitCode: out.ExitCode,
	}, nil
}

// finalizePreview turns the simulation's Inst and Remv lines into the
// preview. An Inst of an installed package is an upgrade (or a downgrade a
// pin asks for), of a package not installed a new install. heldBack is
// what preview.json recorded as upgradable minus the Inst set.
func (p *Provider) finalizePreview(in spec.UpgradeInput, x detach.Exit, r *provider.Run) (spec.UpgradeResult, error) {
	if perr := aptFailure("apt-get -s", x, nil); perr != nil {
		return spec.UpgradeResult{}, perr
	}
	st, acts, err := p.readSimulation(r)
	if err != nil {
		return spec.UpgradeResult{}, err
	}
	preview := buildPreview(acts, st)
	reboot, pkgs := p.rebootState()
	return spec.UpgradeResult{
		Mode: in.EffectiveMode(), WhatIf: true, Preview: &preview,
		Changes: []spec.PackageChange{}, ConffilesKept: []string{}, ConffilesComplete: true,
		RebootRequired: reboot, RebootRequiredPackages: pkgs, ExitCode: x.ExitCode,
	}, nil
}

// readSimulation reads a finished whatIf run's preview.json and the
// actions of its simulation output, files only.
func (p *Provider) readSimulation(r *provider.Run) (previewState, []simAction, error) {
	var st previewState
	dir, err := p.opDir(r.ID())
	if err != nil {
		return st, nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, PreviewFile))
	if err != nil {
		return st, nil, fmt.Errorf("apt: read %s of %s: %w", PreviewFile, r.ID(), err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, nil, fmt.Errorf("apt: decode %s of %s: %w", PreviewFile, r.ID(), err)
	}
	stdout, complete, err := p.opOutput(r.ID(), apitypes.StreamStdout, maxPreviewBytes)
	if err != nil {
		return st, nil, err
	}
	if !complete {
		return st, nil, fmt.Errorf("apt: the simulation output of %s was truncated; the preview would "+
			"be incomplete", r.ID())
	}
	return st, parseSimulation(stdout), nil
}

// buildPreview classifies the simulation's actions. Remv lines name no
// architecture: it is the installed package's that preview.json recorded.
func buildPreview(acts []simAction, st previewState) spec.UpgradePreview {
	pv := spec.UpgradePreview{Upgraded: []spec.PreviewPackage{}, Installed: []spec.PreviewPackage{},
		Removed: []spec.PreviewPackage{}, HeldBack: []spec.HeldBackPackage{}}
	inst := map[string]bool{} // package:architecture
	for _, a := range acts {
		base, _, _ := strings.Cut(a.Name, ":")
		switch a.Op {
		case "Inst":
			pp := spec.PreviewPackage{Package: base, Architecture: a.Architecture, OldVersion: a.OldVersion,
				NewVersion: a.NewVersion}
			inst[base+":"+a.Architecture] = true
			if a.OldVersion == "" {
				pv.Installed = append(pv.Installed, pp)
			} else {
				pv.Upgraded = append(pv.Upgraded, pp)
			}
		case "Remv":
			pv.Removed = append(pv.Removed, spec.PreviewPackage{Package: base, Architecture: st.Architectures[a.Name],
				OldVersion: a.OldVersion})
		}
	}
	for _, u := range st.Upgradable {
		if !inst[u.Package+":"+u.Architecture] {
			pv.HeldBack = append(pv.HeldBack, u)
		}
	}
	for _, l := range [][]spec.PreviewPackage{pv.Upgraded, pv.Installed, pv.Removed} {
		slices.SortFunc(l, func(x, y spec.PreviewPackage) int {
			return cmpPackage(x.Package, x.Architecture, y.Package, y.Architecture)
		})
	}
	slices.SortFunc(pv.HeldBack, func(x, y spec.HeldBackPackage) int {
		return cmpPackage(x.Package, x.Architecture, y.Package, y.Architecture)
	})
	return pv
}

func cmpPackage(p1, a1, p2, a2 string) int {
	if c := strings.Compare(p1, p2); c != 0 {
		return c
	}
	return strings.Compare(a1, a2)
}

func lostUpgrade(_ *provider.Run, in spec.UpgradeInput) string {
	if in.WhatIf {
		return "the simulation changed nothing; run the preview again"
	}
	return "apt-get may not have finished, and packages may be partly upgraded; dpkg --audit on the host shows " +
		"an interrupted state, which Nexus does not repair"
}

// opOutput reads the frames of stream from operation id's log, up to max
// bytes from its start. complete is false when the log lost output to its
// cap or holds more than max. It reads files only, so it works after a
// restart.
func (p *Provider) opOutput(id, stream string, max int) (b []byte, complete bool, err error) {
	dir, err := p.opDir(id)
	if err != nil {
		return nil, false, err
	}
	r := oplog.NewReader(filepath.Join(dir, detach.OutputDir))
	var buf bytes.Buffer
	var offset int64
	complete = true
	for {
		page, err := r.Read(offset, 256<<10)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("apt: read the output of %s: %w", id, err)
		}
		for _, f := range page.Frames {
			if f.Truncated() {
				complete = false
				continue
			}
			if f.S != stream {
				continue
			}
			if buf.Len()+len(f.D) > max {
				buf.WriteString(f.D[:max-buf.Len()])
				return buf.Bytes(), false, nil
			}
			buf.WriteString(f.D)
		}
		if !page.More || page.Next == offset {
			break
		}
		offset = page.Next
	}
	return buf.Bytes(), complete, nil
}
