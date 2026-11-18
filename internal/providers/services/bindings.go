package services

import "github.com/mauktik/nexus/internal/provider"

var (
	_ provider.Provider       = (*Provider)(nil)
	_ provider.OpsDirUser     = (*Provider)(nil)
	_ provider.DaemonUser     = (*Provider)(nil)
	_ provider.OperationsUser = (*Provider)(nil)
)

// Bindings binds the provider's operations. WI 5.1 binds them, with the
// catalog line; until then the provider serves none.
func (p *Provider) Bindings() []provider.Binding { return nil }
