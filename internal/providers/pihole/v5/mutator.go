package v5

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

// GravityScript is the script of every gravity run, whoever started it: its
// path in a process's /proc/<pid>/cmdline marks a run outside Nexus (A-16).
const GravityScript = "/opt/pihole/gravity.sh"

// The wait for a gravity run outside Nexus (A-4): poll every 2 s and give up
// with pihole-busy after 30 min.
const (
	GravityPollInterval = 2 * time.Second
	GravityWaitLimit    = 30 * time.Minute
)

// Mutator is the v5-cli backend's mutation half. It runs only in the
// privileged helper. The area methods (service, blocking, domains,
// adlists) are added by their own work items, each with a compile-time
// assertion in package backend.
type Mutator struct {
	Paths  Paths
	Runner Runner
	// DNSServer is FTL's DNS address, 127.0.0.1:53 in production: the list
	// reload barrier's probe asks it (A-66).
	DNSServer netip.AddrPort
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Sleep waits d or until ctx ends; nil means a real timer. Tests advance
	// a fake clock with it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Reload overrides the list reload barrier's bounds; zero, in production,
	// means the ListReload constants (A-67).
	Reload ReloadTiming
	// GravityPoll is the wait between checks for a gravity run outside
	// Nexus; zero, in production, means GravityPollInterval.
	GravityPoll time.Duration
}

func (m *Mutator) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Mutator) sleep(ctx context.Context, d time.Duration) error {
	if m.Sleep != nil {
		return m.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// FTLPID is FTL's PID when the PID file names a live pihole-FTL (A-24).
func (m *Mutator) FTLPID() (int, bool) { return FTLPID(m.Paths) }

// WaitExternalGravity returns once no gravity run is in progress (V-7: a
// write made during one is lost at its database swap). While one runs it
// logs which, polls every GravityPollInterval, and after GravityWaitLimit
// fails pihole-busy{waitedSeconds}. A run that starts after the check can
// still lose a write (A-16).
func (m *Mutator) WaitExternalGravity(ctx context.Context, out io.Writer) error {
	start := m.now()
	logged := 0
	for {
		pid, ok := GravityRunning(m.Paths)
		if !ok {
			return nil
		}
		if pid != logged {
			fmt.Fprintf(out, "pihole: waiting for a gravity run (pid %d) to finish\n", pid)
			logged = pid
		}
		if waited := m.now().Sub(start); waited >= GravityWaitLimit {
			return problem.New(problem.PiholeBusy, fmt.Sprintf("a gravity run outside Nexus (pid %d) did not finish "+
				"within %s; nothing was changed", pid, GravityWaitLimit)).
				With("waitedSeconds", int(waited/time.Second))
		}
		poll := m.GravityPoll
		if poll <= 0 {
			poll = GravityPollInterval
		}
		if err := m.sleep(ctx, poll); err != nil {
			return fmt.Errorf("pihole: waiting for a gravity run (pid %d): %w", pid, err)
		}
	}
}

// GravityRunning returns the PID of a process whose /proc/<pid>/cmdline
// holds GravityScript, the lowest when there are several.
func GravityRunning(p Paths) (int, bool) {
	entries, err := os.ReadDir(p.Host(ProcDir))
	if err != nil {
		return 0, false
	}
	found := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile(p.Host(filepath.Join(ProcDir, e.Name(), "cmdline")))
		if err != nil || !bytes.Contains(b, []byte(GravityScript)) {
			continue
		}
		if found == 0 || pid < found {
			found = pid
		}
	}
	return found, found != 0
}
