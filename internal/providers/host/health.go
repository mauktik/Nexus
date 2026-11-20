package host

import (
	"context"
	"errors"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

const pathVmstat = "proc/vmstat"

// health is host.health.get.
func (p *Provider) health(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.Health], error) {
	return currentList(p, spec.HealthType, p.collectHealth(ctx, p.run.Load().snapshot())), nil
}

// collectHealth reads the health flags live and takes the top processes
// from snap (nil when the sampler is not running), with the fan-in's
// providerSignals. Each part that fails leaves only its own fields null with
// the reason (FR-12).
func (p *Provider) collectHealth(ctx context.Context, snap *snapshot) spec.HealthProperties {
	signals := p.providerSignals(ctx)
	out := p.readHealth(ctx, snap)
	out.ProviderSignals = signals()
	return out
}

// providerSignals starts the fan-in's Collect, bounded by sourceTimeout, in a
// goroutine beside the host's own reads (storage-provider DF-6), and returns
// the call that waits for its blocks: [] without a fan-in. Collect returns by
// its timeout whatever the sources do, so the wait adds at most that bound.
// The channel is buffered, so the goroutine exits even if no one waits.
func (p *Provider) providerSignals(ctx context.Context) func() spec.Array[apitypes.ProviderSignals] {
	hs := p.healthSources.Load()
	if hs == nil {
		return func() spec.Array[apitypes.ProviderSignals] { return nil }
	}
	timeout := p.Settings().SourceTimeout
	ch := make(chan []apitypes.ProviderSignals, 1)
	go func() { ch <- (*hs).Collect(ctx, timeout) }()
	return func() spec.Array[apitypes.ProviderSignals] { return <-ch }
}

// readHealth is the host's own health reads.
func (p *Provider) readHealth(ctx context.Context, snap *snapshot) spec.HealthProperties {
	now := p.now()
	out := spec.HealthProperties{
		Sampler:        snap.sampler(now),
		RebootRequired: p.rebootRequired(),
		FailedServices: p.failedServices(ctx),
	}
	if n, why := p.oomKills(); why != "" {
		setWhy(&out.Unavailable, "oomKills", why)
	} else {
		out.OOMKills = &n
	}

	byCPU, byMem, seconds, why := snap.processes(now)
	if why != "" {
		for _, k := range []string{"topByCpu", "topByMemory", "processSampleSeconds"} {
			setWhy(&out.Unavailable, k, why)
		}
		return out
	}
	out.TopByCPU, out.TopByMemory = p.topProcesses(byCPU, byMem)
	out.ProcessSampleSeconds = seconds
	return out
}

// oomKills is /proc/vmstat oom_kill, or the reason there is none: kernels
// before 4.13 do not count OOM kills. Nothing else (no kernel log) is read.
func (p *Provider) oomKills() (uint64, string) {
	b, err := p.src.read(pathVmstat)
	if err != nil {
		return 0, reasonFor(pathVmstat, err)
	}
	v, err := hostparse.Vmstat(b)
	if err != nil {
		return 0, reasonFor(pathVmstat, err)
	}
	if !v.OOMKillPresent {
		return 0, prefixNotPresent + displayPath(pathVmstat) + " oom_kill"
	}
	return v.OOMKill, ""
}

// failedOutcome is one svcctl.ListFailed call: its list or error, with the
// exit code the tool returned, which ListFailed's error does not carry.
type failedOutcome struct {
	list svcctl.FailedServices
	exit int
	err  error
}

// failedServices lists the init system's failed units or crashed services
// through svcctl.ListFailed, run under bounded keyed by the tool's argv0
// with sourceTimeout of its own, detached from the request as later
// requests may join the flight (D-7). The count is the whole list; names
// are its first spec.MaxFailedServiceNames, sorted. A failure leaves count
// null and names [] with the reason, which never quotes the tool's output
// (NFR-6).
func (p *Provider) failedServices(ctx context.Context) spec.FailedServices {
	init := p.plat.Init
	out := spec.FailedServices{Source: string(init)}
	argv0, ok := svcctl.FailedTool(init)
	if !ok {
		why := prefixNotSupported + "no failed-service listing for init system " + string(init)
		setWhy(&out.Unavailable, "count", why)
		setWhy(&out.Unavailable, "names", why)
		return out
	}

	res, err := bounded(ctx, p.src, argv0, func() (failedOutcome, error) {
		timeout := p.Settings().SourceTimeout
		var o failedOutcome
		run := func(ctx context.Context, argv []string) (svcctl.Result, error) {
			r, err := p.exec(ctx, argv)
			o.exit = r.ExitCode
			return r, err
		}
		o.list, o.err = svcctl.ListFailed(context.WithoutCancel(ctx), init, svcctl.Options{Run: run, Timeout: timeout})
		if errors.Is(o.err, context.DeadlineExceeded) {
			o.err = &timeoutError{what: argv0, after: timeout}
		}
		return o, nil
	})
	if err == nil {
		err = res.err
	}
	if err != nil {
		why := failedReason(argv0, err, res.exit)
		setWhy(&out.Unavailable, "count", why)
		setWhy(&out.Unavailable, "names", why)
		return out
	}

	count := res.list.Count
	out.Count = &count
	names := res.list.Names
	if len(names) > spec.MaxFailedServiceNames {
		names = names[:spec.MaxFailedServiceNames]
	}
	out.Names = append(spec.Array[string]{}, names...)
	out.NamesTruncated = count > len(out.Names)
	return out
}

// failedReason classifies a ListFailed failure: a timeout, a tool that
// was not started (missing: not present), a non-zero exit with its code, or
// any other failure after the tool started.
func failedReason(argv0 string, err error, exit int) string {
	var (
		timeout *timeoutError
		pending *pendingError
	)
	switch {
	case errors.As(err, &timeout), errors.As(err, &pending), errors.Is(err, svcctl.ErrNotStarted):
		return reasonFor(argv0, err)
	case exit != 0:
		return reasonCommandExit(argv0, exit)
	}
	return prefixCommand + argv0
}
