// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/provider"
)

// sensitiveInput is a provider whose sensitive input paths depend on its
// configuration, such as the scripts catalog's sensitive parameters, and so
// cannot be declared in Op.Sensitive.
type sensitiveInput interface {
	SensitivePaths(action string, params json.RawMessage) []string
}

// sensitivePaths is the audit redactor's hook over providers: it asks the
// provider that owns a call for its configuration-dependent paths. The same
// redactor produces OpRecord.Input, so a path named here reaches neither
// the audit log nor state.db (NFR-4, A-66).
func sensitivePaths(providers []provider.Provider) audit.SensitivePaths {
	by := map[string]sensitiveInput{}
	for _, p := range providers {
		if s, ok := p.(sensitiveInput); ok {
			by[p.Spec().Name] = s
		}
	}
	if len(by) == 0 {
		return nil
	}
	return func(prov, action string, params json.RawMessage) []string {
		s, ok := by[prov]
		if !ok {
			return nil
		}
		return s.SensitivePaths(action, params)
	}
}
