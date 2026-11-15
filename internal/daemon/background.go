package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"time"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/provider"
)

// backgroundStopTimeout bounds the wait for one run to return after its
// context is cancelled by a reload, and for all of them at shutdown. A var
// so tests can shorten it.
var backgroundStopTimeout = 5 * time.Second

// backgrounds runs the RunBackground of each provider that implements
// provider.Background while its configuration section is enabled (D-2). It
// is keyed on configuration enablement, not on a provider's name or its
// preflight. Only the Run loop calls its methods, so it has no lock.
type backgrounds struct {
	log     *slog.Logger
	runners map[string]provider.Background
	running map[string]*bgRun
}

// bgRun is one RunBackground call: cancel ends its context and done is
// closed when it has returned.
type bgRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// newBackgrounds indexes the providers that implement provider.Background
// by name. Nothing starts until reconcile.
func newBackgrounds(providers []provider.Provider, log *slog.Logger) *backgrounds {
	b := &backgrounds{log: log, runners: map[string]provider.Background{}, running: map[string]*bgRun{}}
	for _, p := range providers {
		if r, ok := p.(provider.Background); ok {
			b.runners[p.Spec().Name] = r
		}
	}
	return b
}

// reconcile stops every run whose provider is no longer enabled, waiting at
// most backgroundStopTimeout for each, then starts every enabled runner that
// is not running. A run that panicked stays recorded as running, so it is
// started again only once a reload disables and re-enables its provider.
// Names that are not runners are ignored.
func (b *backgrounds) reconcile(enabled map[string]bool) {
	for _, name := range slices.Sorted(maps.Keys(b.running)) {
		if !enabled[name] {
			b.stop(name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(b.runners)) {
		if _, running := b.running[name]; enabled[name] && !running {
			b.start(name, b.runners[name])
		}
	}
}

func (b *backgrounds) start(name string, r provider.Background) {
	ctx, cancel := context.WithCancel(context.Background())
	run := &bgRun{cancel: cancel, done: make(chan struct{})}
	b.running[name] = run
	b.log.Info("starting background provider", "provider", name)
	go func() {
		defer close(run.done)
		defer cancel()
		// A panic that escapes RunBackground ends this run only; the
		// provider stays enabled and nexusd keeps serving.
		defer func() {
			if v := recover(); v != nil {
				b.log.Error("background provider panicked", "provider", name, "panic", fmt.Sprint(v),
					"stack", string(debug.Stack()))
			}
		}()
		r.RunBackground(ctx)
	}()
}

// stop cancels the run of name, waits at most backgroundStopTimeout for it
// to return and forgets it either way. A run still returning after the
// bound is logged and left to finish on its own.
func (b *backgrounds) stop(name string) {
	run := b.running[name]
	delete(b.running, name)
	b.log.Info("stopping background provider", "provider", name)
	run.cancel()
	t := time.NewTimer(backgroundStopTimeout)
	defer t.Stop()
	select {
	case <-run.done:
	case <-t.C:
		b.log.Warn("background provider still running after its stop timeout", "provider", name,
			"timeout", backgroundStopTimeout.String())
	}
}

// stopAll cancels every run at once, then waits for all of them for at most
// min(backgroundStopTimeout, time left on ctx), one shared bound rather than
// one per runner; it logs each runner still running at warn and returns.
func (b *backgrounds) stopAll(ctx context.Context) {
	b.log.Info("stopping background providers")
	names := slices.Sorted(maps.Keys(b.running))
	for _, name := range names {
		b.running[name].cancel()
	}
	wait, cancel := context.WithTimeout(ctx, backgroundStopTimeout)
	defer cancel()
	for _, name := range names {
		select {
		case <-b.running[name].done:
		case <-wait.Done():
		}
	}
	for _, name := range names {
		select {
		case <-b.running[name].done:
		default:
			b.log.Warn("background provider still running after its stop timeout", "provider", name,
				"timeout", backgroundStopTimeout.String())
		}
	}
	clear(b.running)
}

// enabledOf is the set of provider names whose sections cfg enables.
func enabledOf(cfg *config.Config) map[string]bool {
	enabled := map[string]bool{}
	for name, pc := range providerConfigs(cfg) {
		enabled[name] = pc.Enabled
	}
	return enabled
}
