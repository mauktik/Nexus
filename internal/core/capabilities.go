package core

import (
	"context"
	"maps"
	"slices"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
)

// Capabilities describes this host and every compiled-in provider (FR-2).
// It reports the preflight results from startup, the last reload or the last
// refresh; with refresh it re-runs the preflights first. Nothing re-checks in
// the background (NFR-1).
func (r *Registry) Capabilities(ctx context.Context, refresh bool) apitypes.Capabilities {
	if refresh {
		r.Refresh(ctx)
	}
	snap := r.state.Load()
	out := make([]apitypes.ProviderCapability, 0, len(r.providers))
	for _, p := range r.providers {
		spec := p.Spec()
		c := apitypes.ProviderCapability{
			Name:          spec.Name,
			ResourceTypes: resourceTypes(spec.Ops),
			Actions:       actions(spec.Ops),
		}
		st := snap.providers[spec.Name]
		switch {
		case !st.enabled:
			c.Status, c.Reason = apitypes.ProviderDisabled, disabledReason(spec.Name)
		default:
			c.Status, c.Reason, c.Details = st.status.State, st.status.Reason, maps.Clone(st.status.Details)
		}
		out = append(out, c)
	}
	return apitypes.NewCapabilities(r.host, r.version, out)
}

// resourceTypes lists the distinct resource types of a provider's ops.
func resourceTypes(ops []def.Op) []string {
	var types []string
	for _, op := range ops {
		if op.ResourceType != "" && !slices.Contains(types, op.ResourceType) {
			types = append(types, op.ResourceType)
		}
	}
	slices.Sort(types)
	return types
}

// actions lists the distinct actions a provider can perform: the actions of
// its resource-action and provider-action ops. Subresources are reads.
func actions(ops []def.Op) []string {
	var acts []string
	for _, op := range ops {
		if op.Kind != def.KindResourceAction && op.Kind != def.KindProviderAction {
			continue
		}
		if op.Action != "" && !slices.Contains(acts, op.Action) {
			acts = append(acts, op.Action)
		}
	}
	slices.Sort(acts)
	return acts
}
