package pihole

import (
	"context"
	"fmt"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/dnswrite"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

var _ backend.ServiceMutator = (*v5.Mutator)(nil)

// serviceLostDetail is the outcome-unknown detail of a restart-dns or
// flush-cache unit lost without a result.
const serviceLostDetail = "DNS may be restarting; check `nexus pihole get`"

// The read of FTL's new PID after a restart: the native Type=simple unit
// returns before FTL writes its PID file (A-56), so the read is retried
// every ftlPIDPollInterval for up to ftlPIDPollLimit until the file names a
// live FTL other than the one before. It is dnswrite's, which a DNS write's
// restart shares.
const (
	ftlPIDPollInterval = dnswrite.PIDPollInterval
	ftlPIDPollLimit    = dnswrite.PIDPollLimit
)

// pidPoll is the PID read's bounds. Production uses the constants above;
// the helper's test seams shorten them.
type pidPoll = dnswrite.PIDPoll

var defaultPIDPoll = dnswrite.DefaultPIDPoll

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
	before := dnswrite.PIDOf(c.Mutator.FTLPID())
	fmt.Fprintf(c.Out, "pihole: FTL pid before: %s\n", dnswrite.PIDText(before))
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
		after = dnswrite.PIDAfterRestart(ctx, c.Mutator, before, c.PIDPoll)
	} else {
		after = dnswrite.PIDOf(c.Mutator.FTLPID())
	}
	fmt.Fprintf(c.Out, "pihole: FTL pid after: %s\n", dnswrite.PIDText(after))
	return spec.ServiceResult{
		ActionResult: apitypes.ActionResult{Changed: true},
		FTLPidBefore: before, FTLPidAfter: after, ReadyAfterMs: ready.Milliseconds(),
	}, nil
}
