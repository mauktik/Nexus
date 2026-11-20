// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package zfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxProcesses is the most zpool and zfs processes Tools runs at once.
const MaxProcesses = 4

// ErrBusy is a call that could not start before its deadline because
// MaxProcesses tool processes were still running (a 503 for the caller).
var ErrBusy = fmt.Errorf("%d storage commands are still running", MaxProcesses)

// toolDirs are where Resolve looks for the tools, in order.
var toolDirs = []string{"/usr/sbin", "/sbin"}

// Paths are the resolved absolute paths of the tools; empty when missing.
type Paths struct{ Zpool, Zfs string }

// Resolve finds zpool and zfs at /usr/sbin, else /sbin, through stat
// (os.Stat in production, which follows symlinks). A candidate counts only
// when it is a regular file with an execute bit; otherwise the next one is
// tried. Each tool not found has a reason, keyed "zpool" or "zfs".
func Resolve(stat func(string) (fs.FileInfo, error)) (Paths, map[string]string) {
	var p Paths
	reasons := make(map[string]string)
	for _, tool := range []struct {
		name string
		dst  *string
	}{{"zpool", &p.Zpool}, {"zfs", &p.Zfs}} {
		var problems []string
		allMissing := true
		for _, dir := range toolDirs {
			path := dir + "/" + tool.name
			problem, missing := checkTool(stat, path)
			if problem == "" {
				*tool.dst = path
				break
			}
			allMissing = allMissing && missing
			problems = append(problems, path+" "+problem)
		}
		if *tool.dst != "" {
			continue
		}
		if allMissing {
			reasons[tool.name] = fmt.Sprintf("zfs: %s/%s not found (nor %s/%s)", toolDirs[0], tool.name, toolDirs[1], tool.name)
		} else {
			reasons[tool.name] = "zfs: " + strings.Join(problems, "; ")
		}
	}
	return p, reasons
}

// checkTool returns why path is not a usable tool ("" when it is), and
// whether that is because it does not exist.
func checkTool(stat func(string) (fs.FileInfo, error), path string) (string, bool) {
	fi, err := stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found", true
	case err != nil:
		return fmt.Sprintf("cannot be checked: %v", err), false
	case fi.IsDir():
		return "is a directory", false
	case !fi.Mode().IsRegular():
		return "is not a regular file", false
	case fi.Mode().Perm()&0o111 == 0:
		return fmt.Sprintf("is not executable (mode %04o)", fi.Mode().Perm()), false
	}
	return "", false
}

// TimeoutError is a call that did not finish within its bound.
type TimeoutError struct {
	What  string
	After time.Duration
}

func (e TimeoutError) Error() string {
	return fmt.Sprintf("%s did not finish within %s", e.What, e.After)
}

// PendingError is a call refused at once because the same command has been
// running for longer than the timeout, likely stuck in the kernel.
type PendingError struct {
	What  string
	Since time.Time
}

func (e PendingError) Error() string {
	return fmt.Sprintf("%s is still running since %s", e.What, e.Since.UTC().Format(time.RFC3339))
}

// Tools runs the resolved zpool and zfs, and nothing else, with at most one
// process per argv and at most MaxProcesses processes in all (D-3, NFR-2).
type Tools struct {
	paths   Paths
	run     Runner
	timeout func() time.Duration
	slots   chan struct{}

	mu      sync.Mutex
	flights map[string]*flight
}

// flight is one process and the callers sharing it. res and err are
// written before done is closed and only read after.
type flight struct {
	start time.Time
	done  chan struct{}
	res   Result
	err   error
}

// NewTools returns the runner over p. run nil is Exec; timeout bounds each
// process and is read on each call, so a reloaded commandTimeout applies.
func NewTools(p Paths, run Runner, timeout func() time.Duration) *Tools {
	if run == nil {
		run = Exec
	}
	return &Tools{
		paths:   p,
		run:     run,
		timeout: timeout,
		slots:   make(chan struct{}, MaxProcesses),
		flights: make(map[string]*flight),
	}
}

// Run runs argv, whose argv[0] must be the resolved zpool or zfs; any other
// program is refused before exec (FR-20). For a given argv:
//   - no call in flight: take one of MaxProcesses slots, waiting at most
//     until ctx's deadline or the timeout (else ErrBusy), and start one;
//   - a call in flight for less than the timeout: join it;
//   - a call in flight for the timeout or longer: PendingError at once.
//
// A caller waits until the flight's start plus the timeout (TimeoutError)
// or until ctx is done. The Result is shared by every caller of the flight
// and must not be modified.
func (t *Tools) Run(ctx context.Context, argv []string) (Result, error) {
	if len(argv) == 0 || argv[0] == "" || argv[0] != t.paths.Zpool && argv[0] != t.paths.Zfs {
		return Result{}, fmt.Errorf("%w: refusing to run %q: only the resolved zpool and zfs are run", ErrNotStarted, argv)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	key := strings.Join(argv, "\x00")
	what := strings.Join(append([]string{filepath.Base(argv[0])}, argv[1:]...), " ")
	timeout := t.timeout()

	t.mu.Lock()
	f, ok := t.flights[key]
	if !ok {
		t.mu.Unlock()
		if err := t.acquire(ctx, timeout); err != nil {
			return Result{}, err
		}
		t.mu.Lock()
		// Another caller may have started the same argv while this one
		// waited for its slot; join that flight instead.
		if f, ok = t.flights[key]; ok {
			<-t.slots
		} else {
			f = &flight{start: time.Now(), done: make(chan struct{})}
			t.flights[key] = f
			go t.fly(key, what, f, argv, timeout)
		}
	}
	if ok && time.Since(f.start) >= timeout {
		t.mu.Unlock()
		return Result{}, PendingError{What: what, Since: f.start}
	}
	t.mu.Unlock()

	timer := time.NewTimer(time.Until(f.start.Add(timeout)))
	defer timer.Stop()
	select {
	case <-f.done:
		return f.res, f.err
	case <-timer.C:
		return Result{}, TimeoutError{What: what, After: timeout}
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{}, TimeoutError{What: what, After: time.Since(f.start).Round(time.Millisecond)}
		}
		return Result{}, ctx.Err()
	}
}

// acquire takes a process slot, waiting at most until ctx is done or
// timeout has passed. It returns ErrBusy when the timeout or ctx's deadline
// passed with every slot taken, and ctx.Err() when ctx was cancelled.
func (t *Tools) acquire(ctx context.Context, timeout time.Duration) error {
	select {
	case t.slots <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case t.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrBusy
	case <-ctx.Done():
		// Only an expired deadline means the slots stayed full; a caller that
		// went away (context.Canceled) did not wait out any saturation.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrBusy
		}
		return ctx.Err()
	}
}

// fly is the one goroutine of a flight. It runs the process under its own
// deadline, detached from any caller, and releases the slot and removes the
// key only when the runner has returned, that is when the process exited.
//
// Recorded deviation from go/goroutine-exit-paths (architecture D-3, A-31):
// a tool process in uninterruptible sleep survives SIGKILL, so this
// goroutine has no cancellation path. The in-flight table bounds the cost to
// one goroutine and one slot per stuck argv, at most MaxProcesses in all,
// and the goroutine exits when the process does.
func (t *Tools) fly(key, what string, f *flight, argv []string, timeout time.Duration) {
	ctx, cancel := context.WithDeadline(context.Background(), f.start.Add(timeout))
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			f.res, f.err = Result{}, fmt.Errorf("zfs: panic running %s: %v", what, r)
		}
		t.mu.Lock()
		if t.flights[key] == f {
			delete(t.flights, key)
		}
		t.mu.Unlock()
		<-t.slots
		close(f.done)
	}()
	f.res, f.err = t.run(ctx, argv)
	if f.err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		f.res, f.err = Result{}, TimeoutError{What: what, After: timeout}
	}
}
