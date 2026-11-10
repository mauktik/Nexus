package apt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
)

// Files the mutations read.
const (
	DpkgStatus = "/var/lib/dpkg/status"
	DpkgLog    = "/var/log/dpkg.log"
	// PreFile is the pre-state Prepare writes into the operation's
	// directory, beside spec.json and exit.json.
	PreFile = "pre.json"
)

// Bounds of what Finalize reads.
const (
	maxStatusBytes  = 256 << 20
	maxDpkgLogBytes = 64 << 20
)

// preState is pre.json: the packages before the run, and where dpkg.log
// ended.
type preState struct {
	Taken    time.Time     `json:"taken"`
	Packages []snapPackage `json:"packages"`
	DpkgLog  logPosition   `json:"dpkgLog"`
}

// snapPackage is one dpkg package in a snapshot.
type snapPackage struct {
	Package      string `json:"package"`
	Architecture string `json:"architecture"`
	Version      string `json:"version"`
	Status       string `json:"status"`
}

// logPosition is a log file's identity and size at one moment.
type logPosition struct {
	Exists bool   `json:"exists"`
	Inode  uint64 `json:"inode"`
	Size   int64  `json:"size"`
}

// PackageChange is one package whose version or dpkg status a mutation
// changed. A version is empty when the package is not installed.
type PackageChange struct {
	Package      string
	Architecture string
	OldVersion   string
	NewVersion   string
	OldStatus    string // dpkg's status word; not-installed for a package dpkg did not know
	NewStatus    string
}

// MutationOutcome is what FinalizeMutation reads after an apt mutation.
type MutationOutcome struct {
	Changes []PackageChange // Sorted by package and architecture
	// Conffiles are the conffiles whose local version dpkg kept ("conffile
	// <path> keep" in dpkg.log), in log order.
	Conffiles []string
	// ConffilesComplete is false when dpkg.log was rotated, missing or too
	// large since Prepare, so Conffiles may lack some.
	ConffilesComplete bool
	RebootRequired    string // spec.RebootTrue, RebootFalse or RebootUnknown
	RebootPackages    []string
	ExitCode          int // apt-get's exit status; -1 when killed or never started
}

// PrepareMutation is the Prepare of every apt mutation (upgrade, install,
// remove): args are apt-get's command and its arguments after the fixed
// options, with "--" before any package name. Under ctx, which a cancel of
// the queued operation cancels, it
//
//  1. waits until the dpkg frontend lock is free, for at most
//     providers.apt.lockWait, failing package-manager-busy at the limit;
//  2. runs dpkg --audit, failing package-manager-broken when it reports
//     anything and the lock is still free, and never repairing; output
//     while the lock is held again is a transaction that started in
//     between (S-1: audit is non-empty during any healthy one), so it
//     returns to the wait;
//  3. snapshots dpkg-query and dpkg.log's inode and size into pre.json.
//
// A cancel during the wait ends it, and nothing is launched. The unit runs
// apt-get with KillMode=process and providers.apt.maxRuntime.
func (p *Provider) PrepareMutation(ctx context.Context, r *provider.Run, args []string) (detach.Spec, error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	s := p.Settings()
	deadline := time.Now().Add(s.LockWait)
	for {
		if err := p.waitLock(ctx, r, deadline); err != nil {
			return detach.Spec{}, err
		}
		res, err := p.run(ctx, cmdAudit())
		if err != nil || res.ExitCode != 0 {
			if ctx.Err() != nil {
				return detach.Spec{}, fmt.Errorf("apt: dpkg --audit: %w", ctx.Err())
			}
			return detach.Spec{}, p.failed("dpkg --audit", res, err)
		}
		report := strings.TrimSpace(string(res.Stdout))
		if report == "" {
			break
		}
		st, err := p.probe(p.hostPath(FrontendLock))
		if err != nil {
			return detach.Spec{}, fmt.Errorf("apt: probe the dpkg frontend lock: %w", err)
		}
		if st.Held {
			p.log.Info("dpkg --audit reported a transaction that started after the lock was free; waiting again",
				"operationId", r.ID(), "holderPid", st.PID)
			continue
		}
		p.log.Warn("dpkg --audit reports an interrupted or broken package state", "operationId", r.ID(),
			"audit", report)
		return detach.Spec{}, problem.New(problem.PackageManagerBroken, "dpkg reports an interrupted or broken "+
			"package state, which Nexus does not repair; nothing was changed. Run dpkg --audit on the host to see it: "+
			firstLine(res.Stdout))
	}

	pre, err := p.snapshot(ctx)
	if err != nil {
		return detach.Spec{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("apt: create %s: %w", dir, err)
	}
	b, err := json.Marshal(pre)
	if err != nil {
		return detach.Spec{}, fmt.Errorf("apt: encode %s: %w", PreFile, err)
	}
	if err := writeFileAtomic(dir, PreFile, b); err != nil {
		return detach.Spec{}, err
	}
	return detach.Spec{
		Argv:       mutationArgv(time.Until(deadline), args...),
		Env:        Env(),
		Dir:        "/",
		KillMode:   detach.KillProcess,
		MaxRuntime: s.MaxRuntime,
	}, nil
}

func cmdAudit() command { return newCommand(Dpkg, "--audit") }

// snapshot is dpkg-query's view of every package, and where dpkg.log ends.
func (p *Provider) snapshot(ctx context.Context) (preState, error) {
	pos, err := p.logPosition()
	if err != nil {
		return preState{}, err
	}
	res, err := p.run(ctx, cmdDpkgQueryAll())
	if err != nil || res.ExitCode != 0 {
		return preState{}, p.failed("dpkg-query", res, err)
	}
	pkgs := []snapPackage{}
	for _, d := range parseDpkgQuery(res.Stdout) {
		pkgs = append(pkgs, snapPackage(d))
	}
	return preState{Taken: time.Now().UTC(), Packages: pkgs, DpkgLog: pos}, nil
}

// logPosition stats dpkg.log. It is taken before the dpkg-query snapshot,
// so a change between the two is read twice rather than missed.
func (p *Provider) logPosition() (logPosition, error) {
	fi, err := os.Stat(p.hostPath(DpkgLog))
	if errors.Is(err, fs.ErrNotExist) {
		return logPosition{}, nil
	}
	if err != nil {
		return logPosition{}, fmt.Errorf("apt: stat %s: %w", DpkgLog, err)
	}
	return logPosition{Exists: true, Inode: inode(fi), Size: fi.Size()}, nil
}

// FinalizeMutation reads the outcome of an apt mutation from files only,
// so it gives the same answer after a restart of nexusd: pre.json, dpkg's
// status file, dpkg.log from the offset Prepare recorded, the
// reboot-required marker, and exit.json's status. The error is nil when
// apt-get exited 0, package-manager-busy when it failed on a held lock,
// and apt-failed otherwise; the outcome is filled either way, since a
// failed run can still have changed packages.
func (p *Provider) FinalizeMutation(r *provider.Run, x detach.Exit) (MutationOutcome, error) {
	return p.finalizeMutation(r, x, p.exitFailure)
}

// finalizeMutation is FinalizeMutation with the judge of apt-get's exit:
// install and remove pass one that also explains a refused install, so the
// operation's stderr is read once.
func (p *Provider) finalizeMutation(r *provider.Run, x detach.Exit,
	failure func(what string, x detach.Exit, r *provider.Run) *problem.Error) (MutationOutcome, error) {
	out := MutationOutcome{ExitCode: x.ExitCode}
	dir, err := p.opDir(r.ID())
	if err != nil {
		return out, err
	}
	b, err := os.ReadFile(filepath.Join(dir, PreFile))
	if err != nil {
		return out, fmt.Errorf("apt: read %s of %s: %w", PreFile, r.ID(), err)
	}
	var pre preState
	if err := json.Unmarshal(b, &pre); err != nil {
		return out, fmt.Errorf("apt: decode %s of %s: %w", PreFile, r.ID(), err)
	}
	status, err := readBounded(p.hostPath(DpkgStatus), 0, maxStatusBytes)
	if err != nil {
		return out, fmt.Errorf("apt: read %s: %w", DpkgStatus, err)
	}
	out.Changes = diffPackages(pre.Packages, parseStatusFile(status))
	out.Conffiles, out.ConffilesComplete = p.keptConffiles(pre.DpkgLog)
	if out.ConffilesComplete && p.ucfManaged(out.Changes) {
		out.ConffilesComplete = false
	}
	out.RebootRequired, out.RebootPackages = p.rebootState()
	if perr := failure("apt-get", x, r); perr != nil {
		return out, perr
	}
	return out, nil
}

// parseStatusFile reads dpkg's status file into the snapshot's form: each
// record's package, architecture, version and status word (the last word
// of Status:, as dpkg-query's db:Status-Status prints it).
func parseStatusFile(b []byte) []snapPackage {
	var out []snapPackage
	for _, st := range parseStanzas(b) {
		words := strings.Fields(st["Status"])
		if len(words) != 3 {
			continue
		}
		d := dpkgPackage{Package: st["Package"], Architecture: st["Architecture"], Version: st["Version"],
			Status: words[2]}
		if !pkgNameRE.MatchString(d.Package) || !slices.Contains(dpkgStatuses, d.Status) ||
			(d.Architecture != "" && !archRE.MatchString(d.Architecture)) ||
			(d.Version != "" && !versionRE.MatchString(d.Version)) {
			continue
		}
		out = append(out, snapPackage(d))
	}
	return out
}

// diffPackages lists every package whose installed version or status word
// differs between before and after, by package and architecture. A
// package absent from one side is not-installed there.
func diffPackages(before, after []snapPackage) []PackageChange {
	type state struct{ version, status string }
	key := func(s snapPackage) string { return s.Package + ":" + s.Architecture }
	states := func(pkgs []snapPackage) map[string]state {
		m := make(map[string]state, len(pkgs))
		for _, s := range pkgs {
			v := s.Version
			if !listed(s.Status) {
				v = ""
			}
			m[key(s)] = state{version: v, status: s.Status}
		}
		return m
	}
	b, a := states(before), states(after)
	names := map[string]snapPackage{}
	for _, s := range slices.Concat(before, after) {
		names[key(s)] = s
	}
	changes := []PackageChange{}
	for k, s := range names {
		old, ok := b[k]
		if !ok {
			old.status = "not-installed"
		}
		cur, ok := a[k]
		if !ok {
			cur.status = "not-installed"
		}
		if old == cur {
			continue
		}
		changes = append(changes, PackageChange{Package: s.Package, Architecture: s.Architecture,
			OldVersion: old.version, NewVersion: cur.version, OldStatus: old.status, NewStatus: cur.status})
	}
	slices.SortFunc(changes, func(x, y PackageChange) int {
		if c := strings.Compare(x.Package, y.Package); c != 0 {
			return c
		}
		return strings.Compare(x.Architecture, y.Architecture)
	})
	return changes
}

// keptConffiles reads the conffile lines dpkg.log gained since pos and
// returns the paths whose local version dpkg kept. complete is false when
// the log was rotated or replaced (another inode, or shorter), missing, or
// too large to read: the paths found are then only some of them.
func (p *Provider) keptConffiles(pos logPosition) (paths []string, complete bool) {
	paths = []string{}
	path := p.hostPath(DpkgLog)
	cur, err := p.logPosition()
	if err != nil {
		p.log.Warn("cannot stat dpkg.log; the kept conffiles may be incomplete", "error", err.Error())
		return paths, false
	}
	if !cur.Exists {
		return paths, false
	}
	from, complete := pos.Size, true
	if !pos.Exists || cur.Inode != pos.Inode || cur.Size < pos.Size {
		// Rotated or created since Prepare: what the current file holds is
		// all after Prepare, but the rotated file may hold more.
		from, complete = 0, !pos.Exists
	}
	b, err := readBounded(path, from, maxDpkgLogBytes)
	if err != nil {
		p.log.Warn("cannot read dpkg.log; the kept conffiles may be incomplete", "error", err.Error())
		return paths, false
	}
	if int64(len(b)) == maxDpkgLogBytes {
		complete = false
	}
	seen := map[string]bool{}
	for _, c := range parseConffileKeeps(b) {
		if !seen[c] {
			seen[c] = true
			paths = append(paths, c)
		}
	}
	return paths, complete
}

// UcfRegistry lists the configuration files that ucf manages, one per line:
// the package name, whitespace, then the file's absolute path (ucfr(1);
// written by ucfr in the ucf package).
const UcfRegistry = "/var/lib/ucf/registry"

// ucfManaged reports whether a package the run left installed has a file
// ucf manages. dpkg's --force-conf options do not govern such a file: ucf
// keeps the local version unless told otherwise, and dpkg.log never records
// it, so kept conffiles cannot be listed in full (CR-26). A registry that
// cannot be read counts as ucf-managed, since nothing can be ruled out.
func (p *Provider) ucfManaged(changes []PackageChange) bool {
	b, err := os.ReadFile(p.hostPath(UcfRegistry))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		p.log.Warn("cannot read the ucf registry; the kept conffiles may be incomplete", "error", err.Error())
		return true
	}
	managed := map[string]bool{}
	for line := range lines(b) {
		if f := strings.Fields(line); len(f) >= 2 {
			managed[f[0]] = true
		}
	}
	for _, c := range changes {
		if c.NewStatus != "not-installed" && managed[c.Package] {
			return true
		}
	}
	return false
}

// parseConffileKeeps returns the paths of dpkg.log's "<date> <time>
// conffile <path> keep" lines, as dpkg writes them when it keeps the local
// version of a changed conffile (--force-confold).
func parseConffileKeeps(b []byte) []string {
	var out []string
	for line := range lines(b) {
		f := strings.SplitN(line, " ", 4)
		if len(f) != 4 || f[2] != "conffile" {
			continue
		}
		rest := f[3]
		i := strings.LastIndexByte(rest, ' ')
		if i <= 0 || rest[i+1:] != "keep" || !strings.HasPrefix(rest, "/") {
			continue
		}
		out = append(out, rest[:i])
	}
	return out
}

// readBounded reads path from offset, at most max bytes.
func readBounded(path string, offset, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if _, err := io.Copy(&b, io.LimitReader(f, max)); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeFileAtomic replaces dir/name with b, mode 0600, through a synced
// temporary file and a rename, then syncs dir, so a reader after a crash
// sees the whole file or none.
func writeFileAtomic(dir, name string, b []byte) (err error) {
	f, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("apt: write %s: %w", name, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if _, err := f.Write(b); err != nil {
		return errors.Join(fmt.Errorf("apt: write %s: %w", name, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("apt: sync %s: %w", name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("apt: close %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("apt: rename %s: %w", name, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("apt: open %s: %w", dir, err)
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil && cerr != nil {
		serr = cerr
	}
	if serr != nil {
		return fmt.Errorf("apt: sync %s: %w", dir, serr)
	}
	return nil
}
