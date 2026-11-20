// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package apt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
)

// FrontendLock is dpkg's frontend lock, which dpkg and apt take before they
// change any package (dpkg 1.19.1 and later).
const FrontendLock = "/var/lib/dpkg/lock-frontend"

// lockPoll is how often a held lock is probed again.
const lockPoll = time.Second

// lockState is one probe of a lock.
type lockState struct {
	Held bool
	PID  int // The holder's PID; -1 for an open file description lock, which has none
}

// lockProbe reports whether a lock is held on the file at path.
type lockProbe func(path string) (lockState, error)

// probeLock asks the kernel, with F_GETLK, whether a write lock on the
// whole of path would conflict with a lock someone holds, and whose. It
// opens path read-only and never takes the lock: F_GETLK places nothing
// (fcntl(2)), and a read-only descriptor is all the nexusd sandbox allows
// (ProtectSystem=strict; verified A-30). A missing file is free: dpkg
// creates it when it first locks. F_GETLK does not report a lock of the
// calling process, and nexusd never holds this one.
func probeLock(path string) (lockState, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return lockState{}, nil
	}
	if err != nil {
		return lockState{}, err
	}
	defer f.Close()
	// The region dpkg and apt lock: the whole file (fileutl.cc GetLock).
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		return lockState{}, fmt.Errorf("fcntl F_GETLK %s: %w", path, err)
	}
	if lk.Type == syscall.F_UNLCK {
		return lockState{}, nil
	}
	return lockState{Held: true, PID: int(lk.Pid)}, nil
}

// waitLock probes the dpkg frontend lock until it is free, logging each new
// holder's PID. At deadline it fails package-manager-busy; a done ctx (a
// cancel of the queued operation, or shutdown) ends the wait with ctx's
// error. It never removes the lock file or signals the holder.
func (p *Provider) waitLock(ctx context.Context, r *provider.Run, deadline time.Time) error {
	path := p.hostPath(FrontendLock)
	lastPID := 0
	for {
		st, err := p.probe(path)
		if err != nil {
			return fmt.Errorf("apt: probe the dpkg frontend lock: %w", err)
		}
		if !st.Held {
			return nil
		}
		if st.PID != lastPID {
			lastPID = st.PID
			p.log.Info("waiting for the dpkg frontend lock", "operationId", r.ID(), "lock", FrontendLock,
				"holderPid", st.PID, "until", deadline.UTC().Format(time.RFC3339))
			say(r, "waiting for %s, held by process %d", FrontendLock, st.PID)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return problem.New(problem.PackageManagerBusy, fmt.Sprintf("the dpkg frontend lock was held by process %d "+
				"for all of providers.apt.lockWait (%s); nothing was changed", st.PID, p.Settings().LockWait))
		}
		t := time.NewTimer(min(p.lockPoll, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("apt: waiting for the dpkg frontend lock: %w", ctx.Err())
		case <-t.C:
		}
	}
}

// say writes a progress line to the operation's run output.
func say(r *provider.Run, format string, args ...any) {
	// Progress is best effort: a failed write changes nothing.
	_, _ = fmt.Fprintf(r.Output(), format+"\n", args...)
}
