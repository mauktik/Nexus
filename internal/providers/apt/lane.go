package apt

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// LaneKey is the lane every apt mutation queues on, FIFO: one apt
// operation runs at a time on a host (FR-17). The queue is the core's, so
// it is persisted and restored across a restart of nexusd.
const LaneKey = "apt"

// aptLane is the FIFO lane of every apt mutation, refresh included. A
// whatIf preview uses provider.NoLane instead: apt-get -s as root does not
// wait on the dpkg lock (A-29).
func aptLane[In any](context.Context, In) (provider.Lane, error) {
	return provider.Lane{Key: LaneKey, Policy: provider.LaneQueue}, nil
}

// mutationArgv is apt-get's argv for a mutation: never a prompt, the local
// version of a changed conffile kept, and apt's own wait for the frontend
// lock bounded by what is left of lockWait, which closes the race between
// the probe reporting free and apt taking the lock (S-3). args follow,
// verbatim: the caller puts "--" before any package name.
func mutationArgv(lockTimeout time.Duration, args ...string) []string {
	argv := []string{AptGet, "-y",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		"-o", "DPkg::Lock::Timeout=" + strconv.Itoa(lockSeconds(lockTimeout)),
		"-o", "APT::Color=0",
	}
	return append(argv, args...)
}

// lockSeconds is DPkg::Lock::Timeout for what is left of lockWait: whole
// seconds, rounded up, and at least 1, because 0 makes apt try the lock
// once without waiting.
func lockSeconds(d time.Duration) int {
	s := math.Ceil(d.Seconds())
	if s < 1 {
		return 1
	}
	if s > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(s)
}

// lockErrorLines are the starts of apt's error lines for a lock it could not
// take: the dpkg frontend lock after DPkg::Lock::Timeout, or any lock
// (dpkg's, or the package lists' for apt-get update) held by another
// process. Captured from apt 3.2 in testdata/lock (TC-ERR48); stable across
// apt releases (apt-pkg/deb/debsystem.cc, apt-pkg/contrib/fileutl.cc). Only
// "E: " lines count: apt also prints "Waiting for cache lock: Could not get
// lock ..." as progress while it waits, and a run that then got the lock
// and failed for another reason is not busy.
var lockErrorLines = [][]byte{
	[]byte("E: Unable to acquire the dpkg frontend lock"),
	[]byte("E: Could not get lock"),
}

// lockContention reports whether apt's stderr says a lock was held.
func lockContention(stderr []byte) bool {
	for line := range lines(stderr) {
		l := bytes.TrimSpace([]byte(line))
		for _, prefix := range lockErrorLines {
			if bytes.HasPrefix(l, prefix) {
				return true
			}
		}
	}
	return false
}

// aptExitFailure is apt-get's exit status for a lock or any other error.
const aptExitFailure = 100

// aptFailure is the problem of an apt-get run that ended as x with stderr:
// nil for exit 0, package-manager-busy for exit 100 with apt's lock text,
// and apt-failed with the status for anything else. what names the run.
func aptFailure(what string, x detach.Exit, stderr []byte) *problem.Error {
	switch {
	case x.Error != "":
		return problem.New(problem.AptFailed, what+" could not be started: "+x.Error)
	case x.Signal != "":
		return problem.New(problem.AptFailed, what+" was killed by "+x.Signal)
	case x.ExitCode == 0:
		return nil
	case x.ExitCode == aptExitFailure && lockContention(stderr):
		return problem.New(problem.PackageManagerBusy, fmt.Sprintf("%s could not take a package manager lock that "+
			"another process held (exit status %d); nothing was changed, and the operation log names the holder", what,
			x.ExitCode))
	}
	return problem.New(problem.AptFailed,
		fmt.Sprintf("%s exited with status %d; its output is in the operation log", what, x.ExitCode))
}

// maxStderrBytes bounds the stderr read back from an operation's log.
const maxStderrBytes = 1 << 20

// opStderr reads the stderr frames of operation id's log, up to
// maxStderrBytes from its start: apt's lock errors come before any work.
// It reads files only, so it works after a restart.
func (p *Provider) opStderr(id string) ([]byte, error) {
	b, _, err := p.opOutput(id, apitypes.StreamStderr, maxStderrBytes)
	return b, err
}

// exitFailure is aptFailure with stderr read from the operation's log when
// the status could be a lock's. A log that cannot be read is logged and
// leaves apt-failed.
func (p *Provider) exitFailure(what string, x detach.Exit, r *provider.Run) *problem.Error {
	var stderr []byte
	if x.ExitCode == aptExitFailure && x.Error == "" && x.Signal == "" && r != nil {
		var err error
		if stderr, err = p.opStderr(r.ID()); err != nil {
			p.log.Warn("cannot read apt's output to tell a held lock from another failure", "operationId", r.ID(),
				"error", err.Error())
		}
	}
	return aptFailure(what, x, stderr)
}

// finalizeRefreshBusy is refresh's Finalize: package-manager-busy when
// apt-get update failed on a held lock (another apt-get update holds the
// lists lock), otherwise finalizeRefresh.
func (p *Provider) finalizeRefreshBusy(ctx context.Context, in spec.RefreshInput, x detach.Exit,
	r *provider.Run) (spec.RefreshResult, error) {
	if perr := p.exitFailure("apt-get update", x, r); perr != nil && perr.Slug() == problem.PackageManagerBusy {
		return spec.RefreshResult{}, perr
	}
	return finalizeRefresh(ctx, in, x, r)
}
