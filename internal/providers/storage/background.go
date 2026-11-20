// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/timemachine"
)

// This file is the Background size walk of D-12 (FR-12): RunBackground
// walks each Time Machine bundle's bands/ at start and then every
// sizeInterval, and keeps the sizes in the provider's walk table, which
// storage.timemachine.list reads. No request starts a walk.
//
// One walk in flight per share: each share's walk runs in its own
// goroutine, recorded in the walk table on the provider (not on the
// RunBackground call), so a share whose walk is still stuck in a kernel call
// is skipped, with one debug line, by a later tick and by a RunBackground
// started after a reload. RunBackground never waits on a walk: on ctx done
// it returns at once, and its walks, whose ctx it was, stop between
// entries. Recorded deviation from go/goroutine-exit-paths (the same as
// D-15's, A-47): a walk stuck in a syscall cannot honour its ctx; the cost
// is bounded to one goroutine per configured share, which exits when the
// syscall returns.

var _ provider.Background = (*Provider)(nil)

// bundleSize is a bundle's last walk: its size, or why it has none.
type bundleSize struct {
	size   timemachine.Size
	reason string
}

// walkTable is the walks in flight, by share, and the sizes they found, by
// share and bundle directory. Its zero value is ready.
type walkTable struct {
	mu      sync.Mutex
	seq     uint64
	running map[string]uint64 // share → the id of its walk in flight
	sizes   map[string]map[string]bundleSize
	resume  map[string]string // share → the bundle its next walk starts with
}

// begin records a walk of share and returns its id, or returns the id of
// the walk in flight and false.
func (w *walkTable) begin(share string) (uint64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id, ok := w.running[share]; ok {
		return id, false
	}
	if w.running == nil {
		w.running = map[string]uint64{}
	}
	w.seq++
	w.running[share] = w.seq
	return w.seq, true
}

func (w *walkTable) end(share string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.running, share)
}

func (w *walkTable) size(share, bundle string) (bundleSize, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	bs, ok := w.sizes[share][bundle]
	return bs, ok
}

func (w *walkTable) put(share, bundle string, bs bundleSize) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sizes == nil {
		w.sizes = map[string]map[string]bundleSize{}
	}
	if w.sizes[share] == nil {
		w.sizes[share] = map[string]bundleSize{}
	}
	w.sizes[share][bundle] = bs
}

// resumeAt is the bundle share's next walk starts with: the first one the
// last walk had no budget left for, "" for the first in order.
func (w *walkTable) resumeAt(share string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.resume[share]
}

func (w *walkTable) setResume(share, bundle string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.resume == nil {
		w.resume = map[string]string{}
	}
	w.resume[share] = bundle
}

// keepOnly drops the sizes of share's bundles that its last full listing
// no longer holds.
func (w *walkTable) keepOnly(share string, bundles []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	keep := map[string]bool{}
	for _, b := range bundles {
		keep[b] = true
	}
	for b := range w.sizes[share] {
		if !keep[b] {
			delete(w.sizes[share], b)
		}
	}
}

// RunBackground is the size walk loop (provider.Background, D-12). Each
// round reads the settings, starts a walk of every configured share whose
// previous walk has ended, and waits sizeInterval, read before each wait.
// With no Time Machine path it walks nothing. It returns when ctx is done,
// without waiting for its walks.
func (p *Provider) RunBackground(ctx context.Context) {
	reported := map[string]uint64{} // share → stuck walk already logged by this loop
	for ctx.Err() == nil {
		s := p.Settings()
		for _, share := range s.TimeMachine.Paths {
			id, ok := p.walks.begin(share)
			if !ok {
				if reported[share] != id {
					reported[share] = id
					p.log.Debug("storage size walk skipped: the share's previous walk is still running", "share", share)
				}
				continue
			}
			go p.walkShare(ctx, share)
		}
		interval := s.TimeMachine.SizeInterval
		if interval <= 0 {
			interval = spec.DefaultSizeInterval
		}
		ch, stop := p.wait(interval)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-ch:
			stop()
		}
	}
}

// wait is the Tick seam: a channel that fires after d, and its stop.
func (p *Provider) wait(d time.Duration) (<-chan time.Time, func()) {
	if p.tick != nil {
		return p.tick(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func (p *Provider) observe(share string, start bool) {
	if p.observeWalk != nil {
		p.observeWalk(share, start)
	}
}

// walkShare walks every bundle of share, at most the walk's entry bound and
// spec.SizeWalkTimeout in all, and records each bundle's size. It ends the
// share's entry in the walk table before reporting its end.
func (p *Provider) walkShare(ctx context.Context, share string) {
	defer p.observe(share, false)
	defer p.walks.end(share)
	defer func() {
		if r := recover(); r != nil {
			p.log.Warn("storage size walk panicked", "share", share, "panic", fmt.Sprint(r))
		}
	}()
	p.observe(share, true)
	deadline := time.Now().Add(spec.SizeWalkTimeout)
	root, err := p.openRoot(share)
	if err != nil {
		p.log.Warn("storage size walk: the share could not be opened", "share", share, "error", errText(err))
		return
	}
	defer func() { _ = root.Close() }() // read-only; a close error loses nothing
	dirs, _, err := timemachine.Bundles(root, spec.MaxBundles)
	if err != nil {
		p.log.Warn("storage size walk: the share could not be listed", "share", share, "error", errText(err))
		return
	}
	full := p.walkEntries
	if full <= 0 {
		full = spec.MaxSizeWalkEntries
	}
	budget := full
	// The budget is the share's, so the bundles after the one that spends it
	// are skipped and keep their last figure; the next walk starts with the
	// first of them, so no bundle starves behind a larger one.
	order := rotateFrom(dirs, p.walks.resumeAt(share))
	resume := ""
	for i, dir := range order {
		if ctx.Err() != nil {
			p.log.Debug("storage size walk stopped", "share", share)
			return
		}
		if budget <= 0 || !time.Now().Before(deadline) {
			if resume == "" {
				resume = dir
			}
			continue
		}
		used, starved := p.walkBundle(ctx, root, share, dir, budget, deadline, i > 0)
		budget -= used
		if starved && resume == "" {
			resume = dir
		}
	}
	p.walks.setResume(share, resume)
	p.walks.keepOnly(share, dirs)
	p.log.Debug("storage size walk finished", "share", share, "bundles", len(dirs))
}

// walkBundle walks one bundle and records its size, and returns the entries
// it counted and whether the walk was starved. A walk past the deadline
// keeps its partial figure, truncated. A truncated walk of a bundle that
// is not the first of its round (later), which the bundles before it left
// only part of the share's budget, is starved: it keeps the bundle's
// earlier figure, and records its partial one only when there is none. A
// walk its ctx stopped keeps the bundle's earlier figure, or records its
// partial one, truncated, when there is none. A failure or a panic is
// logged at warn and recorded as the bundle's reason; the walk goes on with
// the next bundle.
func (p *Provider) walkBundle(ctx context.Context, root Root, share, dir string, maxEntries int,
	deadline time.Time, later bool) (entries int, starved bool) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Warn("storage size walk of a bundle panicked", "share", share, "bundle", dir, "panic", fmt.Sprint(r))
			p.walks.put(share, dir, bundleSize{reason: "the size walk panicked"})
		}
	}()
	size, err := timemachine.WalkBands(ctx, root, dir, maxEntries, deadline)
	if !time.Now().Before(deadline) {
		size.Truncated = true
	}
	_, had := p.walks.size(share, dir)
	switch {
	case err == nil && size.Truncated && later && had:
		return size.Entries, true
	case err == nil:
		p.walks.put(share, dir, bundleSize{size: size})
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		if !had {
			size.Truncated = true
			p.walks.put(share, dir, bundleSize{size: size})
		}
	default:
		p.log.Warn("storage size walk of a bundle failed", "share", share, "bundle", dir, "error", errText(err))
		p.walks.put(share, dir, bundleSize{reason: "the size walk failed: " + errText(err)})
	}
	return size.Entries, false
}

// rotateFrom is dirs starting at start, then the ones before it; dirs
// itself when start is not among them.
func rotateFrom(dirs []string, start string) []string {
	i := slices.Index(dirs, start)
	if i <= 0 {
		return dirs
	}
	return append(slices.Clone(dirs[i:]), dirs[:i]...)
}
