// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

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
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

// GravityScript is the script of every gravity run, whoever started it: a
// root process whose /proc/<pid>/cmdline has it as argv[1] (`bash
// /opt/pihole/gravity.sh`) marks a run outside Nexus (A-16, SEC-2).
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

// GravityRunning returns the PID of a gravity run: a process that root runs
// (real and effective uid 0, as `pihole -g` and Pi-hole's cron job do),
// whose /proc/<pid>/cmdline has GravityScript as argv[1], and whose
// /proc/<pid>/comm names a script interpreter (gravityComms). Each is
// needed: any local user can start a process whose command line merely
// contains the script's path, and Nexus would wait for it up to
// GravityWaitLimit with the pihole lane held (security review SEC-2). The
// guard against a setuid-root program such as `sudo /opt/pihole/gravity.sh`
// left at its password prompt is the REAL uid: such a program keeps its
// caller's real uid (measured on the T1 image: sudo at its prompt reads
// "Uid: 65534 0 0 0"; security review SEC-3, SEC-4). Do not relax the real-uid
// check. The comm check is defence in depth only: the kernel sets comm from
// the basename of the exec'd path, so a caller can spoof it through a symlink
// (`ln -s /usr/bin/sudo ~/bash`). On the T1 image, `pihole -g` and cron's `pihole
// updateGravity` run as `bash /opt/pihole/gravity.sh -g` with comm "bash"
// (PROVENANCE, "GRAVITY PROCESS SHAPE"). The lowest PID is returned when
// there are several.
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
		if !isGravityRun(p, e.Name()) {
			continue
		}
		if found == 0 || pid < found {
			found = pid
		}
	}
	return found, found != 0
}

// gravityComms are the /proc/<pid>/comm values of the process that runs the
// gravity script: the interpreter named by the script's `#!/usr/bin/env bash`
// line, a POSIX shell on another build, or the script's own name, which the
// kernel gives a process that was exec'd directly through its shebang.
var gravityComms = map[string]bool{"bash": true, "sh": true, "dash": true, "gravity.sh": true}

// isGravityRun reports whether /proc/<pid> is a root process whose
// interpreter runs GravityScript.
func isGravityRun(p Paths, pid string) bool {
	b, err := os.ReadFile(p.Host(filepath.Join(ProcDir, pid, "cmdline")))
	if err != nil {
		return false
	}
	argv := bytes.Split(bytes.TrimSuffix(b, []byte{0}), []byte{0})
	if len(argv) < 2 || string(argv[1]) != GravityScript {
		return false
	}
	comm, err := os.ReadFile(p.Host(filepath.Join(ProcDir, pid, "comm")))
	if err != nil || !gravityComms[strings.TrimSuffix(string(comm), "\n")] {
		return false
	}
	status, err := os.ReadFile(p.Host(filepath.Join(ProcDir, pid, "status")))
	if err != nil {
		return false
	}
	for line := range bytes.Lines(status) {
		if rest, ok := bytes.CutPrefix(line, []byte("Uid:")); ok {
			// Uid: real, effective, saved set, filesystem.
			uids := bytes.Fields(rest)
			return len(uids) >= 2 && string(uids[0]) == "0" && string(uids[1]) == "0"
		}
	}
	return false
}
