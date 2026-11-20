package storage

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// Scrub start and stop (DF-7). Admission runs inside the sandbox and
// refuses before any operation exists; the helper, outside it, re-reads the
// pool and sends zpool scrub [-s] (D-4, D-10).

// The providers.storage keys that gate scrub start and stop, as not-allowed
// names them.
const (
	keyScrubEnabled = "providers.storage.scrub.enabled"
	keyScrubPools   = "providers.storage.scrub.pools"
)

// scrubLanePrefix starts the lane key of a pool's scrub start and stop,
// which share it (DF-7 step 2).
const scrubLanePrefix = "storage:scrub:"

// scrubLane is pool's lane: one scrub start or stop at a time, a second
// answering 409 conflict.
func scrubLane(pool string) provider.Lane {
	return provider.Lane{Key: scrubLanePrefix + pool, Policy: provider.LaneReject}
}

// scrubStartSpec is storage.pools.scrub's detached binding.
func (p *Provider) scrubStartSpec() provider.DetachedSpec[spec.ScrubInput, spec.ScrubResult] {
	return provider.DetachedSpec[spec.ScrubInput, spec.ScrubResult]{
		Admit: p.admitScrub,
		Lane:  func(_ context.Context, in spec.ScrubInput) (provider.Lane, error) { return scrubLane(in.Name), nil },
		Prepare: func(_ context.Context, in spec.ScrubInput, r *provider.Run) (detach.Spec, error) {
			return p.prepareScrub(actionScrubStart, in.Name, in.Force, r)
		},
		Finalize: func(_ context.Context, in spec.ScrubInput, x detach.Exit, r *provider.Run) (spec.ScrubResult, error) {
			return finalizeHelper[spec.ScrubResult](p, scrubSubject(actionScrubStart, in.Name), scrubCheck(in.Name), x, r)
		},
		LostDetail: func(_ *provider.Run, in spec.ScrubInput) string { return scrubLost(actionScrubStart, in.Name) },
	}
}

// stopScrubSpec is storage.pools.stop-scrub's detached binding.
func (p *Provider) stopScrubSpec() provider.DetachedSpec[spec.StopScrubInput, spec.ScrubResult] {
	return provider.DetachedSpec[spec.StopScrubInput, spec.ScrubResult]{
		Admit: p.admitStopScrub,
		Lane:  func(_ context.Context, in spec.StopScrubInput) (provider.Lane, error) { return scrubLane(in.Name), nil },
		Prepare: func(_ context.Context, in spec.StopScrubInput, r *provider.Run) (detach.Spec, error) {
			return p.prepareScrub(actionScrubStop, in.Name, false, r)
		},
		Finalize: func(_ context.Context, in spec.StopScrubInput, x detach.Exit, r *provider.Run) (spec.ScrubResult, error) {
			return finalizeHelper[spec.ScrubResult](p, scrubSubject(actionScrubStop, in.Name), scrubCheck(in.Name), x, r)
		},
		LostDetail: func(_ *provider.Run, in spec.StopScrubInput) string { return scrubLost(actionScrubStop, in.Name) },
	}
}

// scrubAllowed is the configuration's answer for pool: nil, or 409
// not-allowed naming the key that refuses. Admission and Prepare both ask
// it, Prepare on the settings in force when it runs (DF-7 step 3).
func scrubAllowed(s Settings, pool string) error {
	if !s.Scrub.Enabled {
		return problem.New(problem.NotAllowed, "scrub start and stop are disabled: "+keyScrubEnabled+" is false").
			With("configKey", keyScrubEnabled)
	}
	if !slices.Contains(s.Scrub.Pools, pool) {
		return problem.New(problem.NotAllowed, fmt.Sprintf("pool %s is not in %s", pool, keyScrubPools)).
			With("configKey", keyScrubPools).With("pool", pool)
	}
	return nil
}

// admitScrubPool is the admission both scrub ops share, in DF-7 step 1's
// order: the pool's name, the configuration, the zfs feature (503) and the
// pool being imported (404). It reads only zpool list -H -o name.
func (p *Provider) admitScrubPool(ctx context.Context, pool string) (*zfsState, error) {
	if !validPoolName(pool) {
		return nil, invalidInput("/name", "is not a pool name")
	}
	if err := scrubAllowed(p.Settings(), pool); err != nil {
		return nil, err
	}
	z, err := p.zfsReady(ctx)
	if err != nil {
		return nil, err
	}
	names, err := p.runZFS(ctx, z, poolListWhat, zfs.Argv(z.paths, zfs.Text, zfs.CmdPoolNames))
	if err != nil {
		return nil, opError(err)
	}
	if !slices.Contains(nonEmptyLines(names), pool) {
		return nil, notImported(pool)
	}
	return z, nil
}

// admitScrub is storage.pools.scrub's admission (DF-7 step 1): after
// admitScrubPool, the pool's status refuses a scan in progress (a paused
// scrub is admitted and resumed, A-26) and any health but online, except
// that force admits a degraded pool (A-53). Facts: pool and force.
func (p *Provider) admitScrub(ctx context.Context, in spec.ScrubInput) error {
	z, err := p.admitScrubPool(ctx, in.Name)
	if err != nil {
		return err
	}
	status, _, err := readCmd(ctx, p, z, zfs.CmdPoolStatus, poolStatusWhat, zfs.ParseStatusJSON, zfs.ParseStatusText, in.Name)
	if err != nil {
		return opError(err)
	}
	p.observeStatus(status)
	sp, reason := statusOf(status, nil, in.Name)
	if sp == nil {
		return problem.New(problem.DependencyUnavailable, reason).With("provider", spec.Name)
	}
	if s := sp.Scan; s != nil && s.State == zfs.ScanScanning {
		return problem.New(problem.ScanInProgress, fmt.Sprintf("a %s is running on pool %s", s.Function, in.Name)).
			With("pool", in.Name).With("function", s.Function).With("state", s.State)
	}
	switch {
	case sp.Health == zfs.HealthOnline:
	case sp.Health == zfs.HealthDegraded && in.Force:
	case sp.Health == zfs.HealthDegraded:
		return problem.New(problem.PoolUnhealthy, fmt.Sprintf("pool %s is degraded: scrub it with force true", in.Name)).
			With("pool", in.Name).With("health", sp.Health)
	default:
		// zpool scrub cannot run on such a pool, or can block in the kernel
		// and leave the operation's unit stuck: force does not override it.
		return problem.New(problem.PoolUnhealthy, fmt.Sprintf("pool %s is %s: it is not scrubbed, whatever force says; "+
			"force overrides a degraded pool only", in.Name, sp.Health)).With("pool", in.Name).With("health", sp.Health)
	}
	audit.AddFact(ctx, "pool", in.Name)
	audit.AddFact(ctx, "force", strconv.FormatBool(in.Force))
	return nil
}

// admitStopScrub is storage.pools.stop-scrub's admission: the
// configuration, the zfs feature and the pool being imported only. The
// helper sends nothing when no scrub runs, so neither the scan nor the
// health refuses a stop. Facts: pool and force (always false).
func (p *Provider) admitStopScrub(ctx context.Context, in spec.StopScrubInput) error {
	if _, err := p.admitScrubPool(ctx, in.Name); err != nil {
		return err
	}
	audit.AddFact(ctx, "pool", in.Name)
	audit.AddFact(ctx, "force", strconv.FormatBool(false))
	return nil
}

// prepareScrub re-checks the configuration in force (a refusal fails the
// operation with nothing sent), writes the helper's input and returns its
// spec (DF-7 step 3). commandedAt is the provider's clock now, against which
// the helper accepts a scrub that finished at once (A-25).
func (p *Provider) prepareScrub(action, pool string, force bool, r *provider.Run) (detach.Spec, error) {
	s := p.Settings()
	if err := scrubAllowed(s, pool); err != nil {
		return detach.Spec{}, err
	}
	in := scrubHelperInput{
		OperationID: r.ID(), Action: action, Pool: pool, Force: force,
		Format: p.helperFormat(zfs.CmdPoolStatus).String(), CommandTimeout: s.CommandTimeout.String(),
		CommandedAt: p.now().UTC(),
	}
	return p.launchHelper(r, in, s.CommandTimeout, scrubSubject(action, pool))
}

// scrubSubject names a scrub helper in details: "the storage helper for
// scrub-start on pool z1".
func scrubSubject(action, pool string) string {
	return fmt.Sprintf("the storage helper for %s on pool %s", action, pool)
}

// scrubCheck is the command an operator runs by hand to see what a scrub
// helper did.
func scrubCheck(pool string) string { return "zpool status " + pool }

// scrubLost is the LostDetail of a scrub helper (DF-7 step 7).
func scrubLost(action, pool string) string {
	return lostDetail(scrubSubject(action, pool), scrubCheck(pool))
}
