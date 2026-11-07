// Package core holds the registry of compiled-in providers and their
// bindings, provider capabilities, and the system.* bindings. Transports
// (REST, and MCP from 8.1) derive everything they serve from the registry.
package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
)

// DefaultPreflightTimeout bounds one provider's preflight, so a hung
// dependency cannot hold startup, a refresh or a reload.
const DefaultPreflightTimeout = 5 * time.Second

// RecheckInterval is how old the last preflight of an unavailable provider
// must be before a call to it re-runs the preflight (A-54).
const RecheckInterval = 5 * time.Second

// ProviderConfig is one provider's configuration section.
type ProviderConfig struct {
	Enabled bool
	Decode  func(into any) error // Strict decode of the section; nil means an absent section
}

// Options configures New.
type Options struct {
	Host    string // Reported in capabilities
	Version string // nexusd build version
	// Providers holds each provider's section by provider name. A provider
	// without one is disabled.
	Providers        map[string]ProviderConfig
	Logger           *slog.Logger     // Nil means slog.Default()
	PreflightTimeout time.Duration    // Zero means DefaultPreflightTimeout
	Now              func() time.Time // Clock for preflight re-checks; nil means time.Now
	// Audit is the audit log the Dispatcher writes and system.audit.list
	// reads. Nil means none: mutating requests then fail closed with
	// audit-unavailable.
	Audit *audit.Log
	// Ops is the operation manager that runs long-running ops and backs
	// system.operations.*. Nil means none: long-running ops then fail with
	// internal-error and the operation routes are not served.
	Ops *ops.Manager
	// StorageCheck is the storage hook (4.3) at Data Flow step 5: a non-nil
	// problem refuses a new mutation. Nil means no limit.
	StorageCheck func() *problem.Error
}

// Entry is one binding and the provider that owns it.
type Entry struct {
	Provider string
	Binding  provider.Binding
}

// Registry is the set of compiled-in providers, their bindings and their
// current availability. Availability is an immutable snapshot swapped
// atomically, so readers never see half of a refresh or reload.
type Registry struct {
	host, version    string
	log              *slog.Logger
	preflightTimeout time.Duration
	now              func() time.Time
	audit            *audit.Log
	ops              *ops.Manager
	storageCheck     func() *problem.Error
	providers        []provider.Provider // Sorted by name
	entries          []Entry             // Sorted by op ID

	writeMu sync.Mutex // Serializes Refresh and Apply
	state   atomic.Pointer[snapshot]
}

// snapshot is the availability of every provider at one moment.
type snapshot struct {
	providers map[string]providerState
}

type providerState struct {
	enabled bool            // By configuration
	status  provider.Status // Last preflight; meaningful only when enabled
	checked time.Time       // When the last preflight finished
}

// New builds the registry: it checks the provider set, configures every
// provider from its section, and runs the preflight of each enabled one. A
// failed preflight makes that provider unavailable and never fails New. A
// Configure error does, because it is a configuration error.
func New(providers []provider.Provider, opts Options) (*Registry, error) {
	r := &Registry{
		host:             opts.Host,
		version:          opts.Version,
		log:              opts.Logger,
		preflightTimeout: opts.PreflightTimeout,
		now:              opts.Now,
		audit:            opts.Audit,
		ops:              opts.Ops,
		storageCheck:     opts.StorageCheck,
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.preflightTimeout <= 0 {
		r.preflightTimeout = DefaultPreflightTimeout
	}

	seenProvider := map[string]bool{def.SystemProvider: true}
	seenOp := map[string]bool{}
	for _, p := range providers {
		spec := p.Spec()
		if spec.Name == "" {
			return nil, errors.New("provider with an empty name")
		}
		if seenProvider[spec.Name] {
			return nil, fmt.Errorf("provider %q: duplicate or reserved name", spec.Name)
		}
		seenProvider[spec.Name] = true
		for _, b := range p.Bindings() {
			op := b.Op()
			if op.Provider != spec.Name {
				return nil, fmt.Errorf("provider %q: binding for op %q of provider %q", spec.Name, op.ID, op.Provider)
			}
			if seenOp[op.ID] {
				return nil, fmt.Errorf("provider %q: op %q bound twice", spec.Name, op.ID)
			}
			seenOp[op.ID] = true
			r.entries = append(r.entries, Entry{Provider: spec.Name, Binding: b})
		}
		r.providers = append(r.providers, p)
	}
	for _, b := range r.systemBindings() {
		r.entries = append(r.entries, Entry{Provider: def.SystemProvider, Binding: b})
	}
	slices.SortFunc(r.providers, func(a, b provider.Provider) int { return cmp.Compare(a.Spec().Name, b.Spec().Name) })
	slices.SortFunc(r.entries, func(a, b Entry) int { return cmp.Compare(a.Binding.Op().ID, b.Binding.Op().ID) })

	if err := r.configure(opts.Providers); err != nil {
		return nil, err
	}
	r.state.Store(r.preflightAll(context.Background(), opts.Providers))
	return r, nil
}

// Entries returns every binding, system ones included, sorted by op ID.
func (r *Registry) Entries() []Entry { return slices.Clone(r.entries) }

// Specs returns every provider's declarations, sorted by name. The system
// operations are not among them.
func (r *Registry) Specs() []def.ProviderSpec {
	specs := make([]def.ProviderSpec, len(r.providers))
	for i, p := range r.providers {
		specs[i] = p.Spec()
	}
	return specs
}

// Host is the host name reported to clients.
func (r *Registry) Host() string { return r.host }

// Available reports whether a provider's operations may run now. System
// operations always may. When not, the status carries the reason.
func (r *Registry) Available(name string) (bool, provider.Status) {
	if name == def.SystemProvider {
		return true, provider.Status{State: apitypes.ProviderEnabled}
	}
	st, ok := r.state.Load().providers[name]
	switch {
	case !ok:
		return false, provider.Status{State: apitypes.ProviderUnavailable, Reason: "provider is not compiled in"}
	case !st.enabled:
		return false, provider.Status{State: apitypes.ProviderDisabled, Reason: disabledReason(name)}
	}
	return st.status.State == apitypes.ProviderEnabled, st.status
}

// availableForCall is Available for a call about to run: when the provider
// is unavailable and its last preflight is older than RecheckInterval, the
// preflight is re-run first, so a repaired dependency is noticed without a
// reload (NFR-3). Concurrent callers share one re-check. A disabled provider
// is never re-checked: only configuration enables it.
func (r *Registry) availableForCall(ctx context.Context, name string) (bool, provider.Status) {
	if ok, st := r.Available(name); ok || !r.stale(r.state.Load(), name) {
		return ok, st
	}
	r.writeMu.Lock()
	cur := r.state.Load()
	if r.stale(cur, name) {
		if i := slices.IndexFunc(r.providers, func(p provider.Provider) bool { return p.Spec().Name == name }); i >= 0 {
			st := r.preflight(context.WithoutCancel(ctx), r.providers[i])
			next := &snapshot{providers: maps.Clone(cur.providers)}
			next.providers[name] = providerState{enabled: true, status: st, checked: r.now()}
			r.state.Store(next)
			if st.State == apitypes.ProviderEnabled {
				r.log.Info("provider available after re-check", "provider", name)
			}
		}
	}
	r.writeMu.Unlock()
	return r.Available(name)
}

// stale reports whether a provider is enabled, unavailable, and last
// checked more than RecheckInterval ago.
func (r *Registry) stale(s *snapshot, name string) bool {
	st, ok := s.providers[name]
	return ok && st.enabled && st.status.State != apitypes.ProviderEnabled && r.now().Sub(st.checked) > RecheckInterval
}

func disabledReason(name string) string {
	return fmt.Sprintf("disabled in configuration (providers.%s.enabled is false)", name)
}

// Refresh re-runs the preflight of every enabled provider and swaps in the
// result. Cancellation of ctx, such as a client disconnecting, does not cut
// the preflights short: an interrupted check would otherwise be recorded as
// an unavailable provider. Each preflight stays bounded by its timeout.
func (r *Registry) Refresh(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur := r.state.Load()
	cfgs := make(map[string]ProviderConfig, len(cur.providers))
	for name, st := range cur.providers {
		cfgs[name] = ProviderConfig{Enabled: st.enabled}
	}
	r.state.Store(r.preflightAll(ctx, cfgs))
}

// Apply reconfigures every provider from new sections and swaps provider
// enablement atomically, with a fresh preflight for each enabled provider.
// If any provider rejects its section, enablement is left as it was and the
// error is returned. Configure runs before the swap, so a provider must make
// a repeated Configure safe alongside running handlers.
func (r *Registry) Apply(ctx context.Context, cfgs map[string]ProviderConfig) error {
	ctx = context.WithoutCancel(ctx)
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if err := r.configure(cfgs); err != nil {
		return err
	}
	r.state.Store(r.preflightAll(ctx, cfgs))
	return nil
}

// configure hands each provider its section, enabled or not, so a bad
// section of a disabled provider is still reported.
func (r *Registry) configure(cfgs map[string]ProviderConfig) error {
	var errs []error
	for _, p := range r.providers {
		name := p.Spec().Name
		decode := cfgs[name].Decode
		if decode == nil {
			decode = func(any) error { return nil }
		}
		if err := p.Configure(decode); err != nil {
			errs = append(errs, fmt.Errorf("provider %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// preflightAll runs the preflight of every enabled provider concurrently.
func (r *Registry) preflightAll(ctx context.Context, cfgs map[string]ProviderConfig) *snapshot {
	next := &snapshot{providers: make(map[string]providerState, len(r.providers))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range r.providers {
		name := p.Spec().Name
		if !cfgs[name].Enabled {
			mu.Lock()
			next.providers[name] = providerState{}
			mu.Unlock()
			continue
		}
		wg.Go(func() {
			st := r.preflight(ctx, p)
			mu.Lock()
			next.providers[name] = providerState{enabled: true, status: st, checked: r.now()}
			mu.Unlock()
		})
	}
	wg.Wait()
	for _, name := range slices.Sorted(maps.Keys(next.providers)) {
		st := next.providers[name]
		if st.enabled && st.status.State != apitypes.ProviderEnabled {
			r.log.Warn("provider unavailable", "provider", name, "reason", st.status.Reason)
		}
	}
	return next
}

// preflight runs one provider's preflight, bounded by the preflight timeout.
// A panic or an overrun makes the provider unavailable. The panic value is
// logged with its stack and never reported to clients. An overrunning
// preflight's goroutine ends when the preflight returns; its result is
// dropped into the buffered channel.
func (r *Registry) preflight(ctx context.Context, p provider.Provider) provider.Status {
	name := p.Spec().Name
	ctx, cancel := context.WithTimeout(ctx, r.preflightTimeout)
	defer cancel()
	result := make(chan provider.Status, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				r.log.Error("provider preflight panicked", "provider", name,
					"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				result <- provider.Status{State: apitypes.ProviderUnavailable, Reason: "preflight failed with an internal error"}
			}
		}()
		result <- p.Preflight(ctx)
	}()
	select {
	case st := <-result:
		return normalize(st)
	case <-ctx.Done():
		return provider.Status{
			State:  apitypes.ProviderUnavailable,
			Reason: fmt.Sprintf("preflight did not finish within %s", r.preflightTimeout),
		}
	}
}

// normalize makes a preflight result either enabled, or unavailable with a
// reason. A preflight cannot report disabled: only configuration can.
func normalize(st provider.Status) provider.Status {
	if st.State == apitypes.ProviderEnabled {
		st.Reason = ""
		return st
	}
	st.State = apitypes.ProviderUnavailable
	if st.Reason == "" {
		st.Reason = "preflight failed without a reason"
	}
	return st
}
