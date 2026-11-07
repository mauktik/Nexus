package core

import (
	"context"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/systemspec"
)

// systemBindings binds the system.* operations implemented so far. The rest
// of internal/systemspec is bound as its machinery lands: the operation ops
// at 3.2 and system.audit.list at 2.3.
func (r *Registry) systemBindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(systemspec.CapabilitiesGet,
			func(ctx context.Context, in systemspec.CapabilitiesGetInput) (apitypes.Capabilities, error) {
				return r.Capabilities(ctx, in.Refresh), nil
			}),
	}
}
