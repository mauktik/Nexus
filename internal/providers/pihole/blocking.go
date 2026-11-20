// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package pihole

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

var _ backend.BlockingMutator = (*v5.Mutator)(nil)

// blockingLostDetail is the outcome-unknown detail of a stop-blocking or
// start-blocking unit lost without a result.
const blockingLostDetail = "blocking may be off; a re-enable timer nexus-pihole-reenable.timer may exist; " +
	"run `piholeblocking get`"

// blockingBindings binds stop-blocking and start-blocking.
func (p *Provider) blockingBindings() []provider.Binding {
	return []provider.Binding{
		detachedOp(p, spec.StopBlocking, nil, maxRuntimeDefault,
			func(*provider.Run, spec.StopBlockingInput) string { return blockingLostDetail }),
		detachedOp(p, spec.StartBlocking, nil, maxRuntimeDefault,
			func(*provider.Run, spec.NoInput) string { return blockingLostDetail }),
	}
}

// registerBlockingActions adds the helper actions of stop-blocking and
// start-blocking.
func registerBlockingActions(m map[string]helperAction) {
	m[spec.StopBlocking.Op().ID] = runStopBlocking
	m[spec.StartBlocking.Op().ID] = runStartBlocking
}

// runStopBlocking disables blocking, for durationSeconds when present.
func runStopBlocking(ctx context.Context, c *helperCall) (any, error) {
	bm, err := blockingMutator(c)
	if err != nil {
		return nil, err
	}
	var in spec.StopBlockingInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the stop-blocking input: %v; nothing was changed", err)))
	}
	if in.DurationSeconds < 0 || in.DurationSeconds > spec.MaxDurationSeconds {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("durationSeconds %d is outside 1 to %d; nothing was changed", in.DurationSeconds, spec.MaxDurationSeconds)))
	}
	enabled, pending := blockingBefore(ctx, c)
	got, err := bm.StopBlocking(ctx, time.Duration(in.DurationSeconds)*time.Second, c.Out)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(c.Out, "pihole: blocking is %s, re-enable %s\n", got.State, reenableText(got.ReenableAt))
	// Unchanged only when blocking was already off with no pending re-enable
	// and none was asked for.
	changed := enabled == nil || *enabled || pending != nil || got.ReenableAt != nil
	return spec.BlockingResult{ActionResult: apitypes.ActionResult{Changed: changed}, BlockingProperties: got}, nil
}

// runStartBlocking enables blocking and cancels a pending re-enable.
func runStartBlocking(ctx context.Context, c *helperCall) (any, error) {
	bm, err := blockingMutator(c)
	if err != nil {
		return nil, err
	}
	enabled, pending := blockingBefore(ctx, c)
	got, err := bm.StartBlocking(ctx, c.Out)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(c.Out, "pihole: blocking is %s\n", got.State)
	// Blocking already on, with no pending re-enable to cancel, is unchanged.
	changed := enabled == nil || !*enabled || pending != nil
	return spec.BlockingResult{ActionResult: apitypes.ActionResult{Changed: changed}, BlockingProperties: got}, nil
}

func blockingMutator(c *helperCall) (backend.BlockingMutator, error) {
	bm, ok := c.Mutator.(backend.BlockingMutator)
	if !ok {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole backend %q cannot change blocking; nothing was changed", c.In.Backend)))
	}
	return bm, nil
}

// blockingBefore is BLOCKING_ENABLED (nil when unknown) and the pending
// Nexus re-enable before the change, logged; they decide changed.
func blockingBefore(ctx context.Context, c *helperCall) (*bool, *time.Time) {
	enabled := v5.ReadBlockingEnabled(c.Paths)
	pending := v5.ReenableTimer(ctx, c.Runner)
	state := "unknown"
	if enabled != nil {
		state = fmt.Sprint(*enabled)
	}
	fmt.Fprintf(c.Out, "pihole: before: BLOCKING_ENABLED=%s, re-enable %s\n", state, reenableText(pending))
	return enabled, pending
}

func reenableText(at *time.Time) string {
	if at == nil {
		return "none"
	}
	return "at " + at.UTC().Format(time.RFC3339)
}
