// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// flights is the in-flight table of the provider's bounded filesystem
// accesses (D-15): one flight per key, at most spec.MaxPathFlights flights
// per configured path. Same-package tests read it through count and
// countUnder.
type flights struct {
	// timeout is commandTimeout, read on each call so a Configure applies
	// to the next access.
	timeout func() time.Duration

	mu       sync.Mutex
	inflight map[string]*fsFlight     // key → flight
	slots    map[string]chan struct{} // configured path → its semaphore
}

// fsFlight is one bounded access in progress. val and err are written
// before done is closed and read only after it is.
type fsFlight struct {
	path  string
	start time.Time
	done  chan struct{}
	val   any
	err   error
}

func newFlights(timeout func() time.Duration) *flights {
	return &flights{timeout: timeout, inflight: make(map[string]*fsFlight), slots: make(map[string]chan struct{})}
}

// count is the number of flights in progress.
func (fl *flights) count() int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return len(fl.inflight)
}

// countUnder is the number of flights in progress under path.
func (fl *flights) countUnder(path string) int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	n := 0
	for _, f := range fl.inflight {
		if f.path == path {
			n++
		}
	}
	return n
}

// bounded runs fn, one filesystem access group under the configured path
// path (an open, a scan, a statfs or one file read), keyed by key ("open
// <path>", "scan <path>", "statfs <path>", "read <path>/<name>"). Copied
// from the host provider's bounded (internal/providers/host/source.go), with
// commandTimeout as the bound and a per-path gate added (D-15):
//   - a flight of key younger than commandTimeout: join it, starting
//     nothing, and wait until its start plus commandTimeout;
//   - a flight of key as old as commandTimeout or older: zfs.PendingError at
//     once, starting nothing;
//   - no flight of key, but a flight under path as old as commandTimeout or
//     older (the path is stuck): zfs.PendingError naming that flight's key
//     at once, starting nothing;
//   - otherwise take one of path's spec.MaxPathFlights slots, waiting at
//     most commandTimeout or until ctx is done (else zfs.TimeoutError), and
//     start one flight.
//
// A caller waits at most until the flight's start plus commandTimeout and
// at most until ctx is done; either ends in a zfs.TimeoutError naming key.
// The flight removes its key and frees its slot when fn returns, so the
// next access after a stuck one recovers starts a fresh flight. A panic in
// fn is carried to every caller waiting on it, which re-panics.
//
// Recorded deviation from go/goroutine-exit-paths (architecture D-15,
// A-47): fn may sit in a syscall the kernel holds in uninterruptible sleep
// (a suspended pool) and cannot be cancelled, so its goroutine has no
// cancellation path. The gate bounds the cost to spec.MaxPathFlights
// goroutines (and OS threads) per configured path, and each exits when its
// syscall returns.
func bounded[T any](ctx context.Context, fl *flights, path, key string, fn func() (T, error)) (T, error) {
	var zero T
	timeout := fl.timeout()
	called := time.Now()
	// A caller whose ctx has ended starts no flight and takes no slot: nobody
	// would wait for it, and it would keep a slot busy for later requests.
	if ctx.Err() != nil {
		return zero, zfs.TimeoutError{What: key}
	}

	fl.mu.Lock()
	f, ok := fl.inflight[key]
	switch {
	case ok && called.Sub(f.start) >= timeout:
		fl.mu.Unlock()
		return zero, zfs.PendingError{What: key, Since: f.start}
	case !ok:
		if stuck := fl.stuckLocked(path, timeout, called); stuck != nil {
			fl.mu.Unlock()
			return zero, *stuck
		}
		slots := fl.slotsLocked(path)
		fl.mu.Unlock()
		if err := acquireSlot(ctx, slots, timeout); err != nil {
			return zero, zfs.TimeoutError{What: key, After: time.Since(called).Round(time.Millisecond)}
		}
		fl.mu.Lock()
		// Another caller may have started key, or a flight under path may
		// have passed its timeout, while this one waited for its slot.
		if f, ok = fl.inflight[key]; ok {
			<-slots
		} else if stuck := fl.stuckLocked(path, timeout, time.Now()); stuck != nil {
			fl.mu.Unlock()
			<-slots
			return zero, *stuck
		} else {
			f = &fsFlight{path: path, start: time.Now(), done: make(chan struct{})}
			fl.inflight[key] = f
			go fl.run(key, f, slots, func() (any, error) { return fn() })
		}
	}
	fl.mu.Unlock()

	timer := time.NewTimer(time.Until(f.start.Add(timeout)))
	defer timer.Stop()
	select {
	case <-f.done:
		if p := (*boundedPanic)(nil); errors.As(f.err, &p) {
			panic(p)
		}
		if f.err != nil {
			return zero, f.err
		}
		v, _ := f.val.(T) // fn returned T; only a nil interface fails here
		return v, nil
	case <-timer.C:
		return zero, zfs.TimeoutError{What: key, After: timeout}
	case <-ctx.Done():
		return zero, zfs.TimeoutError{What: key, After: time.Since(called).Round(time.Millisecond)}
	}
}

// stuckLocked is the pending error of the oldest flight under path that is
// as old as timeout or older, nil when path is not stuck. fl.mu is held.
func (fl *flights) stuckLocked(path string, timeout time.Duration, now time.Time) *zfs.PendingError {
	var oldest *zfs.PendingError
	for key, f := range fl.inflight {
		if f.path != path || now.Sub(f.start) < timeout {
			continue
		}
		if oldest == nil || f.start.Before(oldest.Since) || f.start.Equal(oldest.Since) && key < oldest.What {
			oldest = &zfs.PendingError{What: key, Since: f.start}
		}
	}
	return oldest
}

// slotsLocked is path's semaphore, made on first use. fl.mu is held.
func (fl *flights) slotsLocked(path string) chan struct{} {
	s, ok := fl.slots[path]
	if !ok {
		s = make(chan struct{}, spec.MaxPathFlights)
		fl.slots[path] = s
	}
	return s
}

// acquireSlot takes a slot of slots, waiting at most timeout or until ctx
// is done.
func acquireSlot(ctx context.Context, slots chan<- struct{}, timeout time.Duration) error {
	select {
	case slots <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return nil
	case <-timer.C:
		return errNoSlot
	case <-ctx.Done():
		return errNoSlot
	}
}

var errNoSlot = errors.New("no flight slot")

// run is the one goroutine of a flight. A panic in fn is carried to the
// callers waiting on it, which re-panic, so it reaches the recovery of the
// request that asked instead of killing nexusd.
func (fl *flights) run(key string, f *fsFlight, slots <-chan struct{}, fn func() (any, error)) {
	defer func() {
		if r := recover(); r != nil {
			f.val, f.err = nil, &boundedPanic{what: key, value: r, stack: debug.Stack()}
		}
		fl.mu.Lock()
		if fl.inflight[key] == f {
			delete(fl.inflight, key)
		}
		fl.mu.Unlock()
		<-slots
		close(f.done)
	}()
	f.val, f.err = fn()
}

// boundedPanic is a panic raised inside a bounded call, with the stack of
// the goroutine that raised it.
type boundedPanic struct {
	what  string
	value any
	stack []byte
}

func (p *boundedPanic) Error() string {
	return fmt.Sprintf("panic in bounded call %s: %v\n%s", p.what, p.value, p.stack)
}
