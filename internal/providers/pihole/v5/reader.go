// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package v5

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/gravitydb"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// Blocking states (spec BlockingProperties.State, StatusProperties.Blocking).
const (
	BlockingEnabled  = "enabled"
	BlockingDisabled = "disabled"
	BlockingUnknown  = "unknown"
)

// Reader is the v5-cli backend's read half. It runs in the nexusd sandbox,
// reads host files, opens gravity.db read-only, and talks to FTL over its
// TCP text API and DNS. Runner is used only for `systemctl show` of the
// re-enable timer; nothing else is executed (A-26). Unknown values are nil
// (FR-6).
type Reader struct {
	Paths     Paths
	Runner    Runner
	DNSServer netip.AddrPort // 127.0.0.1:53 in production
}

// Name is the backend name.
func (r *Reader) Name() string { return spec.BackendV5CLI }

func (r *Reader) ftlAddr() netip.AddrPort { return FTLAddr(ReadFTLConf(r.Paths).Port) }

// Status combines the versions file (FTL >version when the file has no
// FTL_VERSION), the FTL PID, a DNS probe, blocking, the re-enable timer and
// gravity.db's info table. It fails on none of them. The network probes run
// concurrently, so FTL being down costs at most one FTL API deadline
// (A-2, NFR-1).
func (r *Reader) Status(ctx context.Context) (spec.StatusProperties, error) {
	st := spec.StatusProperties{Backend: spec.BackendV5CLI}

	vf, err := ReadVersions(r.Paths)
	if err != nil {
		// A missing or unreadable versions file leaves every version nil;
		// FTL_VERSION then comes from >version below.
		vf = VersionFile{}
	}
	st.CoreVersion, st.WebVersion, st.FTLVersion = vf.Core, vf.Web, vf.FTL
	setupVars := ReadBlockingEnabled(r.Paths)

	var (
		wg        sync.WaitGroup
		ftlVer    *string
		stats     *Stats
		answering *bool
		reenable  *time.Time
	)
	if st.FTLVersion == nil {
		wg.Go(func() { ftlVer = r.ftlVersion(ctx) })
	}
	if setupVars == nil {
		wg.Go(func() { stats = r.stats(ctx) })
	}
	if r.DNSServer.IsValid() {
		wg.Go(func() { answering = r.dnsAnswering(ctx) })
	}
	wg.Go(func() { reenable = ReenableTimer(ctx, r.Runner) })

	_, running := FTLPID(r.Paths)
	st.FTLRunning = &running
	st.GravityDomains, st.GravityUpdated = r.gravityInfo(ctx)

	wg.Wait()
	st.ReenableAt = reenable
	if st.FTLVersion == nil {
		st.FTLVersion = ftlVer
	}
	st.DNSAnswering = answering
	st.Blocking = blockingState(setupVars, stats)
	return st, nil
}

// Summary reads today's counters from FTL's `>stats` (V-2). When the API is
// unreachable the counters are nil and FTLAPIReachable is false. The
// blocked-domain count is `domains_being_blocked`, else info.gravity_count.
func (r *Reader) Summary(ctx context.Context) (spec.SummaryProperties, error) {
	var sum spec.SummaryProperties
	if s := r.stats(ctx); s != nil {
		sum.FTLAPIReachable = true
		sum.QueriesToday = s.DNSQueriesToday
		sum.BlockedToday = s.AdsBlockedToday
		sum.BlockedPercent = s.AdsPercentageToday
		sum.UniqueClients = s.UniqueClients
		sum.GravityDomains = s.DomainsBeingBlocked
	}
	if sum.GravityDomains == nil {
		sum.GravityDomains, _ = r.gravityInfo(ctx)
	}
	return sum, nil
}

// Blocking is BLOCKING_ENABLED from setupVars.conf, else the `>stats`
// status, else unknown, with the pending Nexus re-enable time (FR-8).
func (r *Reader) Blocking(ctx context.Context) (spec.BlockingProperties, error) {
	setupVars := ReadBlockingEnabled(r.Paths)
	var stats *Stats
	if setupVars == nil {
		stats = r.stats(ctx)
	}
	return spec.BlockingProperties{
		State:      blockingState(setupVars, stats),
		ReenableAt: ReenableTimer(ctx, r.Runner),
	}, nil
}

// Domains returns every domainlist row, ordered by id.
func (r *Reader) Domains(ctx context.Context) ([]spec.DomainProperties, error) {
	db, err := gravitydb.OpenReadOnly(ctx, r.Paths.GravityDBPath())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return db.Domains(ctx)
}

// Adlists returns every adlist row, ordered by id.
func (r *Reader) Adlists(ctx context.Context) ([]spec.AdlistProperties, error) {
	db, err := gravitydb.OpenReadOnly(ctx, r.Paths.GravityDBPath())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return db.Adlists(ctx)
}

// stats is the `>stats` reply, or nil when FTL's API cannot be reached.
func (r *Reader) stats(ctx context.Context) *Stats {
	s, err := FTLStats(ctx, r.ftlAddr(), FTLAPITimeout)
	if err != nil {
		return nil
	}
	return &s
}

// ftlVersion is FTL's `>version`, or nil when unreachable or not a version.
func (r *Reader) ftlVersion(ctx context.Context) *string {
	v, err := FTLVersion(ctx, r.ftlAddr(), FTLAPITimeout)
	if err != nil {
		return nil
	}
	return v
}

// dnsAnswering is true when the DNS server answered the probe with any
// RCODE within dnscheck.ProbeTimeout, false otherwise.
func (r *Reader) dnsAnswering(ctx context.Context) *bool {
	_, err := dnscheck.Query(ctx, r.DNSServer, dnscheck.ReadyName, dnscheck.ProbeTimeout)
	ok := err == nil
	return &ok
}

// gravityInfo is info.gravity_count and info.updated; nil when gravity.db
// cannot be read (FR-6).
func (r *Reader) gravityInfo(ctx context.Context) (*int64, *time.Time) {
	db, err := gravitydb.OpenReadOnly(ctx, r.Paths.GravityDBPath())
	if err != nil {
		return nil, nil
	}
	defer db.Close()
	info, err := db.Info(ctx)
	if err != nil {
		return nil, nil
	}
	return info.GravityCount, info.Updated
}

func blockingState(setupVars *bool, stats *Stats) string {
	switch {
	case setupVars != nil && *setupVars:
		return BlockingEnabled
	case setupVars != nil:
		return BlockingDisabled
	case stats != nil && stats.Status != nil:
		return *stats.Status
	}
	return BlockingUnknown
}
