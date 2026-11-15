// Package host is the read-only overview of the Linux host nexusd runs on
// (the "system" provider). Every file it touches goes through the source
// layer's allow-list (NFR-6, D-3), and every source that can block in the
// kernel goes through bounded (D-7).
package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"runtime/debug"
	"sync"
	"time"
)

// maxReadBytes bounds one read. procfs reports size 0 for its files, so a
// read is never sized from Stat; it reads to EOF up to this bound and fails
// beyond it rather than return a truncated file (NFR-4, FR-12).
const maxReadBytes = 4 << 20

// source is the provider's only path to the host: an allow-listed fs.FS and
// the Statfs seam, with the in-flight table of bounded calls (D-3, D-7).
type source struct {
	fsys    fs.FS         // allowFS: every access is checked
	timeout time.Duration // sourceTimeout
	// statfsSeam is Options.Statfs: stat then statfs of one mount point.
	statfsSeam func(string) (Usage, error)

	mu       sync.Mutex
	inflight map[string]*flight
}

// flight is one bounded call in progress. val and err are written before
// done is closed and read only after it is.
type flight struct {
	start time.Time
	done  chan struct{}
	val   any
	err   error
}

// newSource wraps fsys in the allow-list. A nil statfs is the platform's
// default seam.
func newSource(fsys fs.FS, timeout time.Duration, statfs func(string) (Usage, error)) *source {
	if statfs == nil {
		statfs = defaultStatfs
	}
	return &source{
		fsys:       allowFS{fsys: fsys},
		timeout:    timeout,
		statfsSeam: statfs,
		inflight:   make(map[string]*flight),
	}
}

// read returns the whole file. It needs a read entry and fails, instead of
// truncating, when the file is larger than maxReadBytes.
func (s *source) read(name string) ([]byte, error) {
	f, err := s.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only; a close error loses nothing
	b, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxReadBytes {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errTooLarge}
	}
	return b, nil
}

// readBounded reads a hardware-backed attribute (hwmon, thermal,
// scaling_cur_freq, NIC speed) under bounded, keyed by its absolute path.
func (s *source) readBounded(ctx context.Context, name string) ([]byte, error) {
	return bounded(ctx, s, displayPath(name), func() ([]byte, error) { return s.read(name) })
}

// readDir lists a directory. It needs a list entry. Entries are returned
// whatever their type: sysfs class entries are symlinks, not directories.
func (s *source) readDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(s.fsys, name)
}

// stat needs an entry of any kind.
func (s *source) stat(name string) (fs.FileInfo, error) {
	return fs.Stat(s.fsys, name)
}

// readLink needs a link entry.
func (s *source) readLink(name string) (string, error) {
	return fs.ReadLink(s.fsys, name)
}

// statfs runs the Statfs seam on a mount point under bounded. The key
// "statfs <mount>" covers the seam's stat as well as its statfs, which run
// in the one call (D-3, D-7, review Finding 2).
func (s *source) statfs(ctx context.Context, mountPoint string) (Usage, error) {
	return bounded(ctx, s, "statfs "+mountPoint, func() (Usage, error) { return s.statfsSeam(mountPoint) })
}

// bounded runs fn, a source that can block in the kernel, for at most
// s.timeout, with at most one goroutine per key (D-7, A-42):
//   - no call in flight for what: start one and wait until its start plus
//     s.timeout;
//   - a call in flight for less than s.timeout: join it and wait until the
//     same deadline, starting nothing;
//   - a call in flight for s.timeout or longer: return still pending at
//     once, starting nothing.
//
// The goroutine removes its key when fn returns, so the next call after a
// stuck source recovers starts a fresh one.
//
// Recorded deviation from go/goroutine-exit-paths (architecture D-7): fn
// may sit in uninterruptible sleep (a hung NFS stat, a wedged sensor) and
// cannot be cancelled, so the goroutine has no cancellation path. The
// in-flight table bounds the cost to one goroutine per stuck source, and the
// goroutine exits when fn returns.
func bounded[T any](ctx context.Context, s *source, what string, fn func() (T, error)) (T, error) {
	var zero T
	now := time.Now()
	s.mu.Lock()
	f, ok := s.inflight[what]
	switch {
	case ok && now.Sub(f.start) >= s.timeout:
		s.mu.Unlock()
		return zero, &pendingError{what: what}
	case !ok:
		f = &flight{start: now, done: make(chan struct{})}
		s.inflight[what] = f
		go s.run(what, f, func() (any, error) { return fn() })
	}
	s.mu.Unlock()

	timer := time.NewTimer(time.Until(f.start.Add(s.timeout)))
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
		return zero, &timeoutError{what: what, after: s.timeout}
	case <-ctx.Done():
		return zero, &timeoutError{what: what, after: time.Since(now).Round(time.Millisecond), cause: ctx.Err()}
	}
}

// run is the one goroutine of a flight. A panic in fn is carried to the
// callers waiting on it, which re-panic, so it reaches the recovery of the
// request or sampler tick that asked instead of killing nexusd.
func (s *source) run(what string, f *flight, fn func() (any, error)) {
	defer func() {
		if r := recover(); r != nil {
			f.val, f.err = nil, &boundedPanic{what: what, value: r, stack: debug.Stack()}
		}
		s.mu.Lock()
		if s.inflight[what] == f {
			delete(s.inflight, what)
		}
		s.mu.Unlock()
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

// segKind is the kind of a dynamic path segment.
type segKind uint8

const (
	// segName is a kernel object name read from a listing: an interface,
	// a disk or partition, a hwmon or thermal entry, a kernel release.
	segName segKind = iota
	// segPID is a process id: ASCII digits only, so "self" and
	// "thread-self" are refused.
	segPID
)

// segPath joins dir, one dynamic segment and fixed trailing elements, after
// checking seg: a valid single element, never "." or "..", with no "/" or
// control character, and digits only for segPID. The allow-list then checks
// the result on access.
func segPath(kind segKind, dir, seg string, rest ...string) (string, error) {
	if !validName(seg) || (kind == segPID && !allDigits(seg)) {
		return "", &fs.PathError{Op: "segment", Path: dir, Err: errNotAllowed}
	}
	name := dir + "/" + seg
	for _, r := range rest {
		name += "/" + r
	}
	return name, nil
}

// allowFS is the allow-list filter over Options.FS. Every Open, ReadDir,
// Stat, Lstat and ReadLink is checked against the entry kinds before the
// underlying FS is called, so a refused access never reaches it.
type allowFS struct{ fsys fs.FS }

func (a allowFS) check(op string, name string, want access) error {
	if !permits(name, want) {
		return &fs.PathError{Op: op + " (" + want.String() + ")", Path: name, Err: errNotAllowed}
	}
	return nil
}

func (a allowFS) Open(name string) (fs.File, error) {
	if err := a.check("open", name, accRead); err != nil {
		return nil, err
	}
	return a.fsys.Open(name)
}

func (a allowFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := a.check("readdir", name, accList); err != nil {
		return nil, err
	}
	return fs.ReadDir(a.fsys, name)
}

func (a allowFS) Stat(name string) (fs.FileInfo, error) {
	if err := a.check("stat", name, accAny); err != nil {
		return nil, err
	}
	return fs.Stat(a.fsys, name)
}

func (a allowFS) Lstat(name string) (fs.FileInfo, error) {
	if err := a.check("lstat", name, accAny); err != nil {
		return nil, err
	}
	return fs.Lstat(a.fsys, name)
}

func (a allowFS) ReadLink(name string) (string, error) {
	if err := a.check("readlink", name, accLink); err != nil {
		return "", err
	}
	return fs.ReadLink(a.fsys, name)
}
