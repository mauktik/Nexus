package pihole

import (
	"context"
	"fmt"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

var _ backend.ServiceMutator = (*v5.Mutator)(nil)

// serviceLostDetail is the outcome-unknown detail of a restart-dns or
// flush-cache unit lost without a result.
const serviceLostDetail = "DNS may be restarting; check `piholestatus get`"

// The read of FTL's new PID after a restart: the native Type=simple unit
// returns before FTL writes its PID file (A-56), so the read is retried
// every ftlPIDPollInterval for up to ftlPIDPollLimit until the file names a
// live FTL other than the one before.
const (
	ftlPIDPollInterval = 100 * time.Millisecond
	ftlPIDPollLimit    = 2 * time.Second
)

// serviceBindings binds restart-dns and flush-cache.
func (p *Provider) serviceBindings() []provider.Binding {
	lost := func(*provider.Run, spec.NoInput) string { return serviceLostDetail }
	return []provider.Binding{
		detachedOp(p, spec.RestartDNS, nil, maxRuntimeDefault, lost),
		detachedOp(p, spec.FlushCache, nil, maxRuntimeDefault, lost),
	}
}

// registerServiceActions adds the helper actions of restart-dns and
// flush-cache.
func registerServiceActions(m map[string]helperAction) {
	m[spec.RestartDNS.Op().ID] = func(ctx context.Context, c *helperCall) (any, error) {
		return runService(ctx, c, true)
	}
	m[spec.FlushCache.Op().ID] = func(ctx context.Context, c *helperCall) (any, error) {
		return runService(ctx, c, false)
	}
}

// runService restarts FTL (restart) or flushes its cache, then succeeds
// only once FTL answers pi.hole within the readiness window: a reload is a
// no-op exit 0 when FTL is down (V-17), so readiness is the gate.
func runService(ctx context.Context, c *helperCall, restart bool) (any, error) {
	sm, ok := c.Mutator.(backend.ServiceMutator)
	if !ok {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole backend %q cannot restart DNS; nothing was changed", c.In.Backend)))
	}
	command, run := "pihole restartdns reload", sm.FlushCache
	if restart {
		command, run = "pihole restartdns", sm.RestartDNS
	}
	before := pidOf(c.Mutator.FTLPID())
	fmt.Fprintf(c.Out, "pihole: FTL pid before: %s\n", pidText(before))
	fmt.Fprintf(c.Out, "pihole: running %s\n", command)
	if err := run(ctx, c.Out); err != nil {
		return nil, err
	}
	window := c.ReadyWindow
	fmt.Fprintf(c.Out, "pihole: waiting up to %s for FTL to answer %s on %s\n", window, dnscheck.ReadyName, c.DNSServer)
	ready, err := dnscheck.WaitReady(ctx, c.DNSServer, window)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("pihole: waiting for FTL after %s: %w", command, ctxErr)
		}
		return nil, probErr(problem.New(problem.DNSVerificationFailed,
			fmt.Sprintf("FTL did not answer %s on %s within %s after %s: %v; DNS may be down",
				dnscheck.ReadyName, c.DNSServer, window, command, err)).
			With("rolledBack", false).With("name", dnscheck.ReadyName))
	}
	fmt.Fprintf(c.Out, "pihole: FTL answered after %d ms\n", ready.Milliseconds())
	var after *int
	if restart {
		after = pidAfterRestart(ctx, c.Mutator, before)
	} else {
		after = pidOf(c.Mutator.FTLPID())
	}
	fmt.Fprintf(c.Out, "pihole: FTL pid after: %s\n", pidText(after))
	return spec.ServiceResult{
		ActionResult: apitypes.ActionResult{Changed: true},
		FTLPidBefore: before, FTLPidAfter: after, ReadyAfterMs: ready.Milliseconds(),
	}, nil
}

// pidAfterRestart reads FTL's PID every ftlPIDPollInterval until it names a
// live FTL other than before, for up to ftlPIDPollLimit; then it is
// whatever FTLPID returns, nil when none, which is not a failure (A-55,
// A-56).
func pidAfterRestart(ctx context.Context, m backend.Mutator, before *int) *int {
	t := time.NewTicker(ftlPIDPollInterval)
	defer t.Stop()
	limit := time.NewTimer(ftlPIDPollLimit)
	defer limit.Stop()
	for {
		pid := pidOf(m.FTLPID())
		if pid != nil && (before == nil || *pid != *before) {
			return pid
		}
		select {
		case <-ctx.Done():
			return pidOf(m.FTLPID())
		case <-limit.C:
			return pidOf(m.FTLPID())
		case <-t.C:
		}
	}
}

func pidOf(pid int, ok bool) *int {
	if !ok {
		return nil
	}
	return &pid
}

func pidText(pid *int) string {
	if pid == nil {
		return "none"
	}
	return fmt.Sprint(*pid)
}
