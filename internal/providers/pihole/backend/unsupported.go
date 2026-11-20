// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"io"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// Reasons the Unsupported backend and the v6 DNS-write refusal give
// (FR-4, ASM-6).
const (
	ReasonV6NotImplemented = "v6 backend not implemented"
	ReasonV6DNSWrites      = "v6 DNS write path not verified (ASM-6)"
)

// Unsupported is the Pi-hole v6 backend: every Reader and Mutator method
// answers capability-unavailable{provider: pihole, reason: "v6 backend not
// implemented"}. DNS-file reads do not go through a backend, so they still
// work on v6.
type Unsupported struct{}

var (
	_ Reader          = Unsupported{}
	_ Mutator         = Unsupported{}
	_ ServiceMutator  = Unsupported{}
	_ BlockingMutator = Unsupported{}
	_ DomainMutator   = Unsupported{}
	_ AdlistMutator   = Unsupported{}
)

// unavailable returns a fresh problem each time: callers may add to it.
func unavailable() error {
	return problem.New(problem.CapabilityUnavailable, "pi-hole "+ReasonV6NotImplemented).
		With("provider", spec.Name).
		With("reason", ReasonV6NotImplemented)
}

// Name is the backend name.
func (Unsupported) Name() string { return spec.BackendUnsupported }

// Status answers capability-unavailable.
func (Unsupported) Status(context.Context) (spec.StatusProperties, error) {
	return spec.StatusProperties{}, unavailable()
}

// Summary answers capability-unavailable.
func (Unsupported) Summary(context.Context) (spec.SummaryProperties, error) {
	return spec.SummaryProperties{}, unavailable()
}

// Blocking answers capability-unavailable.
func (Unsupported) Blocking(context.Context) (spec.BlockingProperties, error) {
	return spec.BlockingProperties{}, unavailable()
}

// Domains answers capability-unavailable.
func (Unsupported) Domains(context.Context) ([]spec.DomainProperties, error) {
	return nil, unavailable()
}

// Adlists answers capability-unavailable.
func (Unsupported) Adlists(context.Context) ([]spec.AdlistProperties, error) {
	return nil, unavailable()
}

// FTLPID has no error result: it reports no PID.
func (Unsupported) FTLPID() (int, bool) { return 0, false }

// WaitExternalGravity answers capability-unavailable.
func (Unsupported) WaitExternalGravity(context.Context, io.Writer) error { return unavailable() }

// RestartDNS answers capability-unavailable.
func (Unsupported) RestartDNS(context.Context, io.Writer) error { return unavailable() }

// FlushCache answers capability-unavailable.
func (Unsupported) FlushCache(context.Context, io.Writer) error { return unavailable() }

// TestConfig answers capability-unavailable; the exit code is -1, which no
// caller reads as a passed test.
func (Unsupported) TestConfig(context.Context, io.Writer) (int, error) { return -1, unavailable() }

// StopBlocking answers capability-unavailable.
func (Unsupported) StopBlocking(context.Context, time.Duration, io.Writer) (spec.BlockingProperties, error) {
	return spec.BlockingProperties{}, unavailable()
}

// StartBlocking answers capability-unavailable.
func (Unsupported) StartBlocking(context.Context, io.Writer) (spec.BlockingProperties, error) {
	return spec.BlockingProperties{}, unavailable()
}

// AddDomain answers capability-unavailable.
func (Unsupported) AddDomain(context.Context, spec.DomainAddInput, io.Writer) (spec.DomainProperties, bool, error) {
	return spec.DomainProperties{}, false, unavailable()
}

// RemoveDomain answers capability-unavailable.
func (Unsupported) RemoveDomain(context.Context, int64, io.Writer) (spec.DomainProperties, error) {
	return spec.DomainProperties{}, unavailable()
}

// AddAdlist answers capability-unavailable.
func (Unsupported) AddAdlist(context.Context, string, string, io.Writer) (spec.AdlistProperties, bool, error) {
	return spec.AdlistProperties{}, false, unavailable()
}

// RemoveAdlist answers capability-unavailable.
func (Unsupported) RemoveAdlist(context.Context, int64, io.Writer) (spec.AdlistProperties, error) {
	return spec.AdlistProperties{}, unavailable()
}

// UpdateGravity answers capability-unavailable.
func (Unsupported) UpdateGravity(context.Context, io.Writer) (spec.GravityResult, error) {
	return spec.GravityResult{}, unavailable()
}
