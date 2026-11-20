// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/provider"
)

// staleReportAge is how old a source's last completed report may be and still
// be served, marked stale, when the current read fails or misses the deadline
// (A-49).
const staleReportAge = 5 * time.Minute

// maxBlockSignals is how many signals one block carries (DF-6 step 3).
const maxBlockSignals = 10

// healthRegistry is what the fan-in needs of core.Registry: Available, which
// never re-checks, to skip a disabled source, and AvailableForCall, which
// re-checks an unavailable one after RecheckInterval, inside its flight.
type healthRegistry interface {
	Available(name string) (bool, provider.Status)
	AvailableForCall(ctx context.Context, name string) (bool, provider.Status)
}

// healthFanIn is the daemon's provider.HealthSources (D-7, DF-6): one block
// per enabled provider implementing provider.HealthSource, sorted by name. It
// is keyed on the interface, never on a provider name.
//
// Each source has at most one call in flight (availability, then
// HealthSignals); a later Collect joins it until its own deadline. Each
// completed report is kept as the source's last report, also when it
// completes after the deadline of the Collect that started it.
//
// Recorded deviation from go/goroutine-exit-paths (architecture D-7): a
// source that ignores ctx keeps its flight goroutine past every waiting
// Collect. The flight table bounds this to one goroutine per source, which
// exits when the source returns (the host provider's bounded precedent).
type healthFanIn struct {
	reg     healthRegistry
	sources map[string]provider.HealthSource // By provider name; read-only after newHealthFanIn
	names   []string                         // The keys of sources, sorted
	log     *slog.Logger
	now     func() time.Time // Clock of the stale window

	mu      sync.Mutex
	flights map[string]*healthFlight         // In progress, by source
	last    map[string]provider.HealthReport // Last completed report, by source
}

// healthFlight is one source's call in flight. Its result fields are written
// before done is closed and read only after.
type healthFlight struct {
	done   chan struct{}
	joined int // Collects that started or joined it; under healthFanIn.mu

	report provider.HealthReport
	// reason is why there is no report: the source is unavailable
	// (capability), returned an error, or panicked. Empty with a report.
	reason     string
	capability bool // reason is the source's unavailability: never served stale
}

// newHealthFanIn builds the fan-in over reg from the providers that implement
// provider.HealthSource.
func newHealthFanIn(reg healthRegistry, providers []provider.Provider, log *slog.Logger) *healthFanIn {
	h := &healthFanIn{
		reg:     reg,
		sources: map[string]provider.HealthSource{},
		log:     log,
		now:     time.Now,
		flights: map[string]*healthFlight{},
		last:    map[string]provider.HealthReport{},
	}
	for _, p := range providers {
		if s, ok := p.(provider.HealthSource); ok {
			h.sources[p.Spec().Name] = s
		}
	}
	h.names = slices.Sorted(maps.Keys(h.sources))
	return h
}

// Collect implements provider.HealthSources. It returns by timeout (or when
// ctx is done) whatever the sources do; a disabled source gives no block.
func (h *healthFanIn) Collect(ctx context.Context, timeout time.Duration) []apitypes.ProviderSignals {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	type waiting struct {
		name string
		f    *healthFlight
	}
	var waits []waiting
	for _, name := range h.names {
		if _, st := h.reg.Available(name); st.State == apitypes.ProviderDisabled {
			continue
		}
		waits = append(waits, waiting{name, h.join(ctx, name, timeout)})
	}
	blocks := make([]apitypes.ProviderSignals, 0, len(waits))
	late := "" // Set once this Collect stops waiting: why a flight still running has no result
	for _, w := range waits {
		if late == "" {
			select {
			case <-w.f.done:
			case <-timer.C:
				late = fmt.Sprintf("the current read did not finish within %s", timeout)
			case <-ctx.Done():
				late = fmt.Sprintf("the current read was abandoned: %v", ctx.Err())
			}
		}
		blocks = append(blocks, h.block(w.name, w.f, late))
	}
	return blocks
}

// join returns the source's flight in progress, or starts one. The flight
// runs on a context without ctx's cancellation, because later collects share
// it; its HealthSignals call is bounded by timeout.
func (h *healthFanIn) join(ctx context.Context, name string, timeout time.Duration) *healthFlight {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.flights[name]
	if !ok {
		f = &healthFlight{done: make(chan struct{})}
		h.flights[name] = f
		go h.fly(context.WithoutCancel(ctx), name, timeout, f)
	}
	f.joined++
	return f
}

// fly is the one goroutine of a flight (DF-6 step 2): call-time availability,
// then HealthSignals. A panic is recovered, logged at error and reported as
// "internal error".
func (h *healthFanIn) fly(ctx context.Context, name string, timeout time.Duration, f *healthFlight) {
	defer func() {
		if v := recover(); v != nil {
			h.log.Error("health source panicked", "provider", name,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			f.report, f.reason, f.capability = provider.HealthReport{}, "internal error", false
		}
		h.mu.Lock()
		if f.reason == "" {
			h.last[name] = f.report
		}
		if h.flights[name] == f {
			delete(h.flights, name)
		}
		h.mu.Unlock()
		close(f.done)
	}()
	// The registry runs a re-check's preflight under WithoutCancel and its own
	// bound, so a slow re-check completes for the next collect.
	if ok, st := h.reg.AvailableForCall(ctx, name); !ok {
		f.reason, f.capability = "capability-unavailable: "+st.Reason, true
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	report, err := h.sources[name].HealthSignals(ctx)
	if err != nil {
		f.reason = err.Error()
		return
	}
	f.report = report
}

// block is the source's block for a Collect (DF-6 step 3). late is non-empty
// when the Collect stopped waiting; a flight that is done is used anyway.
func (h *healthFanIn) block(name string, f *healthFlight, late string) apitypes.ProviderSignals {
	reason, capability := late, false
	select {
	case <-f.done:
		if f.reason == "" {
			return reportBlock(name, f.report)
		}
		reason, capability = f.reason, f.capability
	default:
	}
	if !capability {
		h.mu.Lock()
		last, ok := h.last[name]
		h.mu.Unlock()
		if ok && h.now().Sub(last.ObservedAt) <= staleReportAge {
			b := reportBlock(name, last)
			b.Stale = true
			if b.Unavailable == nil {
				b.Unavailable = map[string]string{}
			}
			b.Unavailable["signals"] = reason
			return b
		}
	}
	return apitypes.ProviderSignals{
		Source:      name,
		Signals:     []apitypes.HealthSignal{},
		Unavailable: map[string]string{"signals": reason},
	}
}

// reportBlock is a report as a block: the source's counts, its signals
// re-sorted critical, warning, info (stable) and cut to maxBlockSignals, and
// its per-feature reasons. Nothing of the report is shared with the block.
func reportBlock(name string, r provider.HealthReport) apitypes.ProviderSignals {
	sorted := slices.Clone(r.Signals)
	slices.SortStableFunc(sorted, func(a, b apitypes.HealthSignal) int {
		return cmp.Compare(severityRank(a.Severity), severityRank(b.Severity))
	})
	signals := make([]apitypes.HealthSignal, min(len(sorted), maxBlockSignals))
	copy(signals, sorted)
	counts := r.Counts
	b := apitypes.ProviderSignals{
		Source:           name,
		Counts:           &counts,
		Signals:          signals,
		SignalsTruncated: r.Truncated || len(sorted) > maxBlockSignals,
		Unavailable:      maps.Clone(r.Unavailable),
	}
	if !r.ObservedAt.IsZero() {
		at := r.ObservedAt
		b.ObservedAt = &at
	}
	return b
}

// severityRank orders severities critical, warning, info, then words the
// fan-in does not know.
func severityRank(s string) int {
	switch s {
	case apitypes.SeverityCritical:
		return 0
	case apitypes.SeverityWarning:
		return 1
	case apitypes.SeverityInfo:
		return 2
	}
	return 3
}
