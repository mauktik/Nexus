// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
)

// HealthSource is a provider whose health signals the daemon fans in to the
// host provider's health read, one block per enabled source. The mechanism is
// keyed on this interface, never on a provider name.
type HealthSource interface {
	// HealthSignals must honour ctx's deadline; it never mutates. The daemon
	// keeps at most one call in flight per source and does not wait past its
	// own bound for a call that ignores ctx.
	HealthSignals(ctx context.Context) (HealthReport, error)
}

// HealthReport is one completed health read of a HealthSource.
type HealthReport struct {
	ObservedAt  time.Time
	Counts      apitypes.SignalCounts   // Every signal found, including any past the source's own cap
	Signals     []apitypes.HealthSignal // Sorted critical first (the fan-in re-sorts, stably)
	Truncated   bool                    // The source capped Signals (Counts still counts all)
	Unavailable map[string]string       // Feature -> reason, for parts not read
}

// HealthSources is the daemon's fan-in. Collect returns one block per enabled
// HealthSource, sorted by provider name, within timeout whatever the sources do.
type HealthSources interface {
	Collect(ctx context.Context, timeout time.Duration) []apitypes.ProviderSignals
}

// HealthSourcesUser is a provider that reads the fan-in. The daemon calls
// UseHealthSources once, after the registry exists and before serving.
type HealthSourcesUser interface{ UseHealthSources(HealthSources) }
