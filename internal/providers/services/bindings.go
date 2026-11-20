// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"slices"

	"github.com/mauktik/nexus/internal/provider"
)

var (
	_ provider.Provider       = (*Provider)(nil)
	_ provider.OpsDirUser     = (*Provider)(nil)
	_ provider.DaemonUser     = (*Provider)(nil)
	_ provider.OperationsUser = (*Provider)(nil)
)

// Bindings binds the provider's eight operations: list, get and logs, the
// four service controls, and the reboot.
func (p *Provider) Bindings() []provider.Binding {
	return slices.Concat(p.readBindings(), p.controlBindings(), p.rebootBindings())
}
