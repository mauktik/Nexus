package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// The provider's features, each available or not on its own (D-6): a
// handler gates on its feature, never on the provider as a whole.
const (
	featureZFS         = "zfs"
	featureBackups     = "backups"
	featureTimeMachine = "timeMachine"
)

// featureNames are the features in the order reasons are joined.
var featureNames = []string{featureZFS, featureBackups, featureTimeMachine}

// recheckInterval is how old an unavailable feature's last check must be
// before a call re-checks it. It mirrors core.RecheckInterval, the
// registry's re-check of an unavailable provider (D-6).
const recheckInterval = 5 * time.Second

// featureState is one feature's last check.
type featureState struct {
	Available bool
	Reason    string // Why not, "" when available
	CheckedAt time.Time
}

// zfsState is what a zfs check found. It is replaced whole by each check.
type zfsState struct {
	paths         zfs.Paths
	tools         *zfs.Tools   // Nil when zfs is unavailable
	version       *zfs.Version // Nil when zfs version did not run or parse
	versionReason string       // Why version is nil
	format        zfs.Format   // D-5: Text unless the version selects JSON
	pools         []string     // Imported pool names, in zpool list order
}

// features keeps each feature's state and the re-check in progress of
// each, so concurrent callers share one re-check.
type features struct {
	mu       sync.Mutex
	state    map[string]featureState
	rechecks map[string]chan struct{} // feature → closed when its re-check ends
}

func newFeatures() *features {
	f := &features{state: make(map[string]featureState), rechecks: make(map[string]chan struct{})}
	for _, name := range featureNames {
		f.state[name] = featureState{Reason: "not checked yet"}
	}
	return f
}

func (f *features) get(name string) featureState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[name]
}

func (f *features) set(name string, st featureState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[name] = st
}

// require answers nil when feature is available, and otherwise a 503
// capability-unavailable problem with the feature's reason. An unavailable
// feature whose last check is recheckInterval old or older is re-checked
// first, the re-check shared by concurrent callers (D-6). It logs nothing
// per call; a re-check logs only a change of availability (NFR-7).
func (p *Provider) require(ctx context.Context, feature string) error {
	st := p.feats.get(feature)
	if st.Available {
		return nil
	}
	if p.now().Sub(st.CheckedAt) >= recheckInterval {
		st = p.recheck(ctx, feature)
		if st.Available {
			return nil
		}
	}
	return unavailable(feature, st.Reason)
}

// unavailable is the problem of a feature that is not available.
func unavailable(feature, reason string) error {
	return problem.New(problem.CapabilityUnavailable, featureReason(feature, reason)).
		With("provider", spec.Name).With("reason", reason)
}

// featureReason is reason prefixed with the feature's name, as the
// preflight joins it. The zfs reasons already start with "zfs: ".
func featureReason(feature, reason string) string {
	if strings.HasPrefix(reason, feature+": ") {
		return reason
	}
	return feature + ": " + reason
}

// recheck re-runs feature's check, or joins the one in progress, and
// returns the state it stored. A caller whose ctx ends first gets the state
// as it is; the re-check goes on and the next call sees its result.
func (p *Provider) recheck(ctx context.Context, feature string) featureState {
	f := p.feats
	f.mu.Lock()
	st := f.state[feature]
	if st.Available || p.now().Sub(st.CheckedAt) < recheckInterval {
		// Another caller re-checked it meanwhile.
		f.mu.Unlock()
		return st
	}
	done, running := f.rechecks[feature]
	if !running {
		done = make(chan struct{})
		f.rechecks[feature] = done
		go p.runRecheck(feature, st, done)
	}
	f.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return f.get(feature)
}

// runRecheck is the one goroutine of a feature's re-check. Its context is
// not a caller's, so a caller giving up does not cancel it; it ends within
// commandTimeout because every zpool, zfs and filesystem access of a check
// is bounded by it (zfs.Tools, bounded).
func (p *Provider) runRecheck(feature string, before featureState, done chan struct{}) {
	ctx := context.Background()
	st := featureState{}
	defer func() {
		if r := recover(); r != nil {
			st = featureState{Reason: fmt.Sprintf("the %s check panicked: %v", feature, r)}
		}
		st.CheckedAt = p.now()
		f := p.feats
		f.mu.Lock()
		f.state[feature] = st
		delete(f.rechecks, feature)
		f.mu.Unlock()
		close(done)
		if st.Available != before.Available {
			p.log.Info("storage feature availability changed",
				"feature", feature, "available", st.Available, "reason", st.Reason)
		}
	}()
	s := p.Settings()
	if reason := p.checkHost(); reason != "" {
		st.Reason = reason
		return
	}
	switch feature {
	case featureZFS:
		z, reason := p.checkZFS(ctx, s.CommandTimeout)
		p.zfs.Store(z)
		st.Reason = reason
	case featureBackups:
		st.Reason = p.checkPaths(ctx, s.Backups.VzdumpPaths)
	case featureTimeMachine:
		st.Reason = p.checkPaths(ctx, s.TimeMachine.Paths)
	}
	st.Available = st.Reason == ""
}
