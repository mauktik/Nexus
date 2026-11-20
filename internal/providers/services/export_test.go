// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package services

import "github.com/mauktik/nexus/internal/provider"

// Injected returns what the daemon injected through UseOpsDir, UseDaemon and
// UseOperations, each zero until injected (TC-SV-117).
func (p *Provider) Injected() (opsDir string, d provider.Daemon, o provider.Operations) {
	if s := p.opsDir.Load(); s != nil {
		opsDir = *s
	}
	if r := p.daemon.Load(); r != nil {
		d = r.d
	}
	if r := p.ops.Load(); r != nil {
		o = r.o
	}
	return opsDir, d, o
}
