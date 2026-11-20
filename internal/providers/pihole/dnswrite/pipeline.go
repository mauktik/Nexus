// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package dnswrite is the DNS file write pipeline (FR-22): the only code
// that replaces Pi-hole's local DNS records file. It runs in the privileged
// helper, under the pihole lane, and takes the pre-Nexus backup, swaps the
// file atomically, tests, restarts and verifies FTL, and rolls the file back
// on any failure after the swap.
package dnswrite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/dnsfile"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// Mutator is what the pipeline needs of a backend: FTL's PID, and the
// syntax test and restart.
type Mutator interface {
	backend.Mutator
	backend.ServiceMutator
}

// Request is one write of the DNS file.
type Request struct {
	DNSFile  string // The DNS file's path on this host
	StateDir string // The pihole state directory (A-34); the backup is under it
	OpDir    string // The operation's directory (0700); the rollback copy is in it
	IfMatch  string // The etag the caller read, "" for none
	Changes  []dnsfile.Change
	// Batch is true for apply-dns-changes: a refused change is
	// dns-batch-rejected naming its index, even when the batch holds one.
	Batch bool
}

// Deps are the pipeline's seams.
type Deps struct {
	Mutator   Mutator
	DNSServer netip.AddrPort // FTL's DNS address, 127.0.0.1:53 in production
	Out       io.Writer      // The oplog: one "pihole: …" line per step
	Now       func() time.Time
	// ReadyWindow is how long FTL has to answer after a restart, the
	// rollback's included; zero, in production, means dnscheck.ReadyWindow.
	ReadyWindow time.Duration
	// PIDPoll bounds the read of FTL's new PID after the restart; a zero
	// field, in production, means DefaultPIDPoll's.
	PIDPoll PIDPoll
	// BeforeSwap is a test hook, nil in production: it runs after the temp
	// file is fsynced and before the live file is re-read (step 5), so a
	// test can plant a hand edit at the race point.
	BeforeSwap func()
}

// MaxFileBytes caps the DNS file the pipeline reads (A-10), as the reads do.
const MaxFileBytes = 1 << 20

// RollbackCopyName is the rollback copy of the previous bytes in the
// operation's directory.
const RollbackCopyName = "dnsfile.rollback"

// The read of FTL's new PID after the restart, as restart-dns reads it
// (WI 3.2, A-56): every PIDPollInterval for up to PIDPollLimit after FTL
// answers, until the PID file names a live FTL other than the one before.
// restart-dns and a DNS write share this one read (review CR-6).
const (
	PIDPollInterval = 100 * time.Millisecond
	PIDPollLimit    = 2 * time.Second
)

// PIDPoll is the PID read's bounds. Production uses the constants above
// (DefaultPIDPoll); tests shorten them.
type PIDPoll struct{ Interval, Limit time.Duration }

// DefaultPIDPoll is production's PID read.
var DefaultPIDPoll = PIDPoll{Interval: PIDPollInterval, Limit: PIDPollLimit}

// withDefaults is poll with each unset field production's.
func (poll PIDPoll) withDefaults() PIDPoll {
	if poll.Interval <= 0 {
		poll.Interval = PIDPollInterval
	}
	if poll.Limit <= 0 {
		poll.Limit = PIDPollLimit
	}
	return poll
}

// nexusMarker starts every line Nexus adds to the file (zone and unzoned
// markers).
const nexusMarker = "# nexus:"

// owner is the DNS file's owner, group and mode, which every replacement
// keeps.
type owner struct {
	uid, gid int
	mode     os.FileMode
}

// pipeline is one Apply.
type pipeline struct {
	req  Request
	d    Deps
	path string // The DNS file
	dir  string
	base string
	own  owner
	old  []byte // The bytes before the write (steps 2 and 5)
	copy string // The rollback copy
}

// Apply runs steps 1-10 of the DNS file write pipeline (architecture, Data
// Flow) and returns the result, or the problem that ended it. Callers
// convert the problem to an error only through probErr.
//
//  1. The lane is the caller's.
//  2. Read the file; an IfMatch other than its etag is dns-file-conflict.
//  3. Apply the changes to the model; an unchanged model or identical bytes
//     is changed:false with nothing written. 3a: a file without a nexus
//     marker gets the pre-Nexus backup first, unless it exists.
//  4. Write the new bytes to a dot-prefixed temp file beside the live one
//     (never loaded by dnsmasq, A-31), with its owner, group and mode,
//     fsynced.
//  5. Re-read the live file; a changed etag is dns-file-conflict.
//  6. Write the rollback copy to the op directory, rename the temp file over
//     the live one and fsync the directory. 6a: a DNS-equivalent change
//     (A-42) ends here, with no test and no restart.
//  7. The syntax test passes only on (0, nil) (A-19); otherwise the file is
//     restored and re-tested, and the result is dns-config-test-failed.
//  8. One restart.
//  9. FTL answers within the readiness window, then every changed name is
//     verified against the new model (FR-24).
//  10. A failed restart, readiness or verification restores the file,
//     restarts FTL and waits for it; when that fails too the result is
//     dns-rollback-failed.
func Apply(ctx context.Context, req Request, d Deps) (spec.DNSChangeResult, *problem.Error) {
	if d.Out == nil {
		d.Out = io.Discard
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.ReadyWindow <= 0 {
		d.ReadyWindow = dnscheck.ReadyWindow
	}
	d.PIDPoll = d.PIDPoll.withDefaults()
	if d.Mutator == nil || !filepath.IsAbs(req.DNSFile) || req.StateDir == "" || req.OpDir == "" {
		return spec.DNSChangeResult{}, problem.New(problem.InternalError,
			"the DNS write pipeline was called without a mutator, an absolute DNS file, a state directory or an op directory; nothing was changed")
	}
	p := &pipeline{req: req, d: d, path: req.DNSFile, dir: filepath.Dir(req.DNSFile), base: filepath.Base(req.DNSFile),
		copy: filepath.Join(req.OpDir, RollbackCopyName)}
	return p.run(ctx)
}

func (p *pipeline) logf(format string, args ...any) {
	fmt.Fprintf(p.d.Out, "pihole: "+format+"\n", args...)
}

func (p *pipeline) run(ctx context.Context) (spec.DNSChangeResult, *problem.Error) {
	// Step 2.
	p.logf("reading %s", p.path)
	old, fi, err := readLive(p.path)
	if err != nil {
		return spec.DNSChangeResult{}, p.unreadable(err)
	}
	own, err := ownerOf(fi)
	if err != nil {
		return spec.DNSChangeResult{}, p.unreadable(err)
	}
	p.old, p.own = old, own
	etag0 := etagOf(old)
	if p.req.IfMatch != "" && p.req.IfMatch != etag0 {
		p.logf("ifMatch %s is not the current etag %s; nothing was changed", p.req.IfMatch, etag0)
		return spec.DNSChangeResult{}, conflict(p.req.IfMatch, etag0,
			"the DNS file changed since it was read (ifMatch is not its current etag); nothing was changed")
	}

	// Step 3.
	p.logf("applying %d change(s) to the DNS file model", len(p.req.Changes))
	f := dnsfile.Parse(old)
	before := f.Model()
	after := before
	for i, c := range p.req.Changes {
		if err := after.Apply(c); err != nil {
			return spec.DNSChangeResult{}, RefusalProblem(i, p.req.Batch, c, err)
		}
	}
	next := dnsfile.Render(f, after)
	if sameModel(before, after) || bytes.Equal(next, old) {
		p.logf("no change: the DNS file already says this; nothing was written")
		pid := PIDOf(p.d.Mutator.FTLPID())
		return spec.DNSChangeResult{Etag: etag0, FTLPidBefore: pid, FTLPidAfter: pid,
			Verified: []spec.VerifiedName{}}, nil
	}

	// Step 3a.
	var preNexus string
	if !hasMarker(old) {
		path, created, err := ensurePreNexusBackup(p.req.StateDir, p.path, old, own, p.d.Now())
		if err != nil {
			p.logf("pre-Nexus backup failed: %v; nothing was changed", err)
			return spec.DNSChangeResult{}, problem.New(problem.InternalError,
				fmt.Sprintf("the pre-Nexus backup of the DNS file could not be made at %s: %v; nothing was changed",
					PreNexusBackupPath(p.req.StateDir, p.path), err))
		}
		if created {
			preNexus = path
			p.logf("saved the pre-Nexus backup of %s to %s", p.path, path)
		} else {
			p.logf("the pre-Nexus backup %s exists; it is kept as it is", path)
		}
	}

	// Step 4.
	tmp, err := writeTemp(p.dir, p.base, next, own)
	if err != nil {
		p.logf("writing the temp file failed: %v; nothing was changed", err)
		return spec.DNSChangeResult{}, internal("writing the new DNS file beside %s failed: %v; nothing was changed", p.path, err)
	}
	p.logf("wrote the new DNS file to %s", tmp)
	swapped := false
	defer func() {
		if !swapped {
			if err := removeIfExists(tmp); err != nil {
				p.logf("removing the temp file %s failed: %v", tmp, err)
			}
		}
	}()
	if p.d.BeforeSwap != nil {
		p.d.BeforeSwap()
	}

	// Step 5.
	cur, _, err := readLive(p.path)
	if err != nil {
		return spec.DNSChangeResult{}, p.unreadable(err)
	}
	if etag := etagOf(cur); etag != etag0 {
		p.logf("the DNS file changed while the new one was written (etag %s, was %s); nothing was changed", etag, etag0)
		return spec.DNSChangeResult{}, conflict(etag0, etag,
			"the DNS file was changed by someone else during the write; nothing was changed")
	}

	// Step 6.
	if err := writeCopy(p.copy, p.req.OpDir, old); err != nil {
		p.logf("writing the rollback copy %s failed: %v; nothing was changed", p.copy, err)
		return spec.DNSChangeResult{}, internal("writing the rollback copy %s failed: %v; nothing was changed", p.copy, err)
	}
	p.logf("saved the previous DNS file to %s", p.copy)
	if err := os.Rename(tmp, p.path); err != nil {
		p.logf("replacing %s failed: %v; nothing was changed", p.path, err)
		return spec.DNSChangeResult{}, internal("replacing the DNS file %s failed: %v; nothing was changed", p.path, err)
	}
	swapped = true
	if err := syncDir(p.dir); err != nil {
		p.logf("fsync of %s failed after the swap: %v", p.dir, err)
		return spec.DNSChangeResult{}, p.restoreUnrestarted(fmt.Sprintf("fsync of %s failed after the DNS file was replaced: %v", p.dir, err))
	}
	etag := etagOf(next)
	p.logf("replaced %s (etag %s)", p.path, etag)
	res := spec.DNSChangeResult{Etag: etag, PreNexusBackup: preNexus, Verified: []spec.VerifiedName{}}
	res.Changed = true

	// Step 6a.
	if dnsfile.DNSEquivalent(old, next) {
		pid := PIDOf(p.d.Mutator.FTLPID())
		res.FTLPidBefore, res.FTLPidAfter = pid, pid
		p.logf("only comment lines changed: no configuration test and no DNS restart")
		return res, nil
	}

	// Step 7.
	p.logf("testing the DNS configuration with pihole-FTL --test")
	if code, err := p.d.Mutator.TestConfig(ctx, p.d.Out); !passed(code, err) {
		return spec.DNSChangeResult{}, p.testFailed(ctx, code, err)
	}
	p.logf("the DNS configuration test passed")

	// Step 8.
	res.FTLPidBefore = PIDOf(p.d.Mutator.FTLPID())
	p.logf("FTL pid before: %s", PIDText(res.FTLPidBefore))
	p.logf("restarting DNS with pihole restartdns")
	if err := p.d.Mutator.RestartDNS(ctx, p.d.Out); err != nil {
		p.logf("the DNS restart failed: %v", err)
		if rerr := p.rollback(ctx); rerr != nil {
			return spec.DNSChangeResult{}, rerr
		}
		return spec.DNSChangeResult{}, restartFailed(err)
	}

	// Step 9.
	p.logf("waiting up to %s for FTL to answer %s on %s", p.d.ReadyWindow, dnscheck.ReadyName, p.d.DNSServer)
	ready, err := dnscheck.WaitReady(ctx, p.d.DNSServer, p.d.ReadyWindow)
	if err != nil {
		p.logf("FTL did not answer after the restart: %v", err)
		if rerr := p.rollback(ctx); rerr != nil {
			return spec.DNSChangeResult{}, rerr
		}
		return spec.DNSChangeResult{}, problem.New(problem.DNSVerificationFailed,
			fmt.Sprintf("FTL did not answer %s on %s within %s after the restart: %v; the previous DNS file was restored and FTL restarted on it",
				dnscheck.ReadyName, p.d.DNSServer, p.d.ReadyWindow, err)).
			With("rolledBack", true).With("name", dnscheck.ReadyName)
	}
	p.logf("FTL answered after %d ms", ready.Milliseconds())
	res.FTLPidAfter = PIDAfterRestart(ctx, p.d.Mutator, res.FTLPidBefore, p.d.PIDPoll)
	p.logf("FTL pid after: %s", PIDText(res.FTLPidAfter))
	res.Restarted = true

	for _, w := range changedNames(before, after) {
		v := p.verify(ctx, w)
		res.Verified = append(res.Verified, v)
		if v.Passed {
			continue
		}
		if rerr := p.rollback(ctx); rerr != nil {
			return spec.DNSChangeResult{}, rerr
		}
		return spec.DNSChangeResult{}, problem.New(problem.DNSVerificationFailed,
			fmt.Sprintf("%s did not resolve as expected after the restart (expected %v, observed %v, rcode %q); "+
				"the previous DNS file was restored and FTL restarted on it", v.FQDN, v.Expected, v.Observed, v.RCode)).
			With("rolledBack", true).With("name", v.FQDN).With("expected", v.Expected).With("observed", v.Observed)
	}
	p.logf("verified %d changed name(s)", len(res.Verified))
	return res, nil
}

// passed is the syntax-test gate (A-19): only (0, nil) passes. A non-zero
// exit, a death by signal (128+signal) and a failure to run (-1, err) all
// fail, and no code is compared with 1.
func passed(code int, err error) bool { return code == 0 && err == nil }

// testFailed is step 7's failure: restore the previous bytes, re-test them
// with the same gate, and report dns-config-test-failed. FTL is not
// restarted: it still serves the configuration it loaded.
func (p *pipeline) testFailed(ctx context.Context, code int, err error) *problem.Error {
	what := fmt.Sprintf("pihole-FTL --test failed with exit code %d", code)
	if err != nil {
		what = fmt.Sprintf("the DNS configuration syntax test could not run (exit code %d): %v", code, err)
	}
	p.logf("%s; restoring the previous DNS file", what)
	if rerr := p.restore(); rerr != nil {
		p.logf("restoring %s failed: %v", p.path, rerr)
		return problem.New(problem.DNSRollbackFailed,
			fmt.Sprintf("%s, and restoring the previous DNS file failed: %v; FTL was not restarted and still serves the previous configuration, "+
				"but %s holds the new one; the previous bytes are at %s", what, rerr, p.path, p.copy)).
			With("rollbackCopy", p.copy)
	}
	p.logf("re-testing the restored DNS configuration")
	code2, err2 := p.d.Mutator.TestConfig(context.WithoutCancel(ctx), p.d.Out)
	pre := !passed(code2, err2)
	detail := what + "; the previous DNS file was restored and DNS was not restarted"
	if pre {
		detail += fmt.Sprintf("; the restored configuration fails the test too (exit code %d), so the failure was there before this change", code2)
	}
	p.logf("restored; the restored configuration %s the test", map[bool]string{true: "also fails", false: "passes"}[pre])
	return problem.New(problem.DNSConfigTestFailed, detail).
		With("exitCode", code).With("rolledBack", true).With("preExisting", pre)
}

// restoreUnrestarted restores the previous bytes after a failure between the
// swap and the restart, when FTL has not loaded the new file.
func (p *pipeline) restoreUnrestarted(what string) *problem.Error {
	if err := p.restore(); err != nil {
		p.logf("restoring %s failed: %v", p.path, err)
		return problem.New(problem.DNSRollbackFailed,
			fmt.Sprintf("%s, and restoring the previous DNS file failed: %v; FTL was not restarted; the previous bytes are at %s",
				what, err, p.copy)).With("rollbackCopy", p.copy)
	}
	p.logf("restored the previous DNS file; DNS was not restarted")
	return internal("%s; the previous DNS file was restored and DNS was not restarted", what)
}

// rollback is step 10: restore the previous bytes, restart FTL on them and
// wait for it to answer. It runs even when ctx has ended, and returns
// dns-rollback-failed, naming the rollback copy, when any part fails.
func (p *pipeline) rollback(ctx context.Context) *problem.Error {
	ctx = context.WithoutCancel(ctx)
	fail := func(what string, err error) *problem.Error {
		p.logf("rollback failed: %s: %v; DNS may be down; the previous bytes are at %s", what, err, p.copy)
		return problem.New(problem.DNSRollbackFailed,
			fmt.Sprintf("the DNS change failed and so did its rollback (%s: %v); DNS may be down; "+
				"the previous DNS file is at %s", what, err, p.copy)).With("rollbackCopy", p.copy)
	}
	p.logf("rolling back: restoring the previous DNS file from %s", p.copy)
	if err := p.restore(); err != nil {
		return fail("restoring the previous DNS file", err)
	}
	p.logf("rolling back: restarting DNS with pihole restartdns")
	if err := p.d.Mutator.RestartDNS(ctx, p.d.Out); err != nil {
		return fail("the rollback restart", err)
	}
	p.logf("rolling back: waiting up to %s for FTL to answer %s", p.d.ReadyWindow, dnscheck.ReadyName)
	if _, err := dnscheck.WaitReady(ctx, p.d.DNSServer, p.d.ReadyWindow); err != nil {
		return fail("FTL did not answer after the rollback restart", err)
	}
	p.logf("rolled back: FTL answers on the previous DNS file")
	return nil
}

// restore replaces the live file with the previous bytes, atomically, with
// the original owner, group and mode.
func (p *pipeline) restore() error {
	tmp, err := writeTemp(p.dir, p.base, p.old, p.own)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return errors.Join(err, removeIfExists(tmp))
	}
	return syncDir(p.dir)
}

// restartFailed is pihole-command-failed for a failed restart that was
// rolled back.
func restartFailed(err error) *problem.Error {
	const rolled = "; the previous DNS file was restored and FTL restarted on it"
	var pe *problem.Error
	if errors.As(err, &pe) && pe.Slug() == problem.PiholeCommandFailed {
		return problem.New(problem.PiholeCommandFailed, pe.Detail+rolled).
			With("command", pe.Extensions["command"]).With("exitCode", pe.Extensions["exitCode"]).With("rolledBack", true)
	}
	return problem.New(problem.PiholeCommandFailed, fmt.Sprintf("pihole restartdns failed: %v%s", err, rolled)).
		With("command", "pihole restartdns").With("exitCode", -1).With("rolledBack", true)
}

// want is the expected state of one changed record name.
type want struct {
	fqdn  string
	check dnscheck.Want
	// cover is every address dnsmasq should answer: the name's own line
	// for an add or update; for a remove, the lines of the longest
	// remaining name that covers it (all of them when that name is on
	// several lines), or none.
	cover []string
}

// changedNames are the record names whose addresses differ between the
// models (added, updated or removed, cascades included), sorted, with what
// each must now resolve to.
func changedNames(before, after dnsfile.Model) []want {
	b, a := addrsByName(before), addrsByName(after)
	var names []string
	for n, ips := range b {
		if !slices.Equal(ips, a[n]) {
			names = append(names, n)
		}
	}
	for n := range a {
		if _, ok := b[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]want, 0, len(names))
	for _, n := range names {
		if ips := a[n]; len(ips) > 0 {
			// Add and update refuse a duplicate name (FR-21), so a changed
			// name that remains is on exactly one line.
			out = append(out, want{fqdn: n, cover: ips,
				check: dnscheck.Want{Kind: dnscheck.WantSet, IP: netip.MustParseAddr(ips[0])}})
			continue
		}
		// Remove refuses a duplicate name too: one address was removed.
		w := want{fqdn: n, cover: covering(after, n),
			check: dnscheck.Want{Kind: dnscheck.WantRemove, IP: netip.MustParseAddr(b[n][0])}}
		if len(w.cover) > 0 {
			w.check.CoveredIP = netip.MustParseAddr(w.cover[0])
		}
		out = append(out, w)
	}
	return out
}

// addrsByName maps each record name to its addresses, in file order.
func addrsByName(m dnsfile.Model) map[string][]string {
	out := map[string][]string{}
	for _, r := range m.Records {
		out[r.FQDN] = append(out[r.FQDN], r.IPv4)
	}
	return out
}

// covering is what dnsmasq answers for name from m: dnsfile.Expected picks
// the longest record name that covers it and returns its first address; a
// name on several lines is answered with all of them, so they are added,
// distinct, in file order. Nil when no record covers name.
func covering(m dnsfile.Model, name string) []string {
	ip, ok := dnsfile.Expected(m, name)
	if !ok {
		return nil
	}
	best := ""
	for _, r := range m.Records {
		if (name == r.FQDN || strings.HasSuffix(name, "."+r.FQDN)) && len(r.FQDN) > len(best) {
			best = r.FQDN
		}
	}
	out := []string{ip}
	for _, r := range m.Records {
		if r.FQDN == best && !slices.Contains(out, r.IPv4) {
			out = append(out, r.IPv4)
		}
	}
	return out
}

// verify queries one changed name (A-1: NameTries of NameTryTimeout) and
// applies dnscheck.Check (FR-24). Where the name stays covered by a name on
// several lines and the removed address is among them, dnsmasq answers the
// whole set, which Check's single CoveredIP cannot express, so the answer
// is compared with that set instead.
func (p *pipeline) verify(ctx context.Context, w want) spec.VerifiedName {
	expected := []string{}
	if w.check.Kind == dnscheck.WantSet || len(w.cover) > 0 {
		expected = slices.Clone(w.cover)
	}
	ans, ok, err := dnscheck.Verify(ctx, p.d.DNSServer, w.fqdn, w.check, dnscheck.NameTryTimeout, dnscheck.NameTries)
	if err == nil && ans.RCode != "" && w.check.Kind == dnscheck.WantRemove && len(w.cover) > 1 &&
		slices.Contains(w.cover, w.check.IP.String()) {
		ok = ans.RCode == "NOERROR" && sameSet(ans.A, w.cover)
	}
	observed := make([]string, 0, len(ans.A))
	for _, a := range ans.A {
		observed = append(observed, a.String())
	}
	v := spec.VerifiedName{FQDN: w.fqdn, Expected: expected, Observed: observed, RCode: ans.RCode, Passed: ok}
	switch {
	case err != nil:
		p.logf("verify %s: expected %v: %v: FAILED", w.fqdn, expected, err)
	case ans.RCode == "":
		p.logf("verify %s: no reply, so the removed name is no longer answered locally (A-45): passed", w.fqdn)
	default:
		p.logf("verify %s: expected %v, observed %v (%s): %s", w.fqdn, expected, observed, ans.RCode,
			map[bool]string{true: "passed", false: "FAILED"}[ok])
	}
	return v
}

// sameSet reports whether got and want hold the same addresses, ignoring
// order and repeats.
func sameSet(got []netip.Addr, want []string) bool {
	seen := map[string]bool{}
	for _, a := range got {
		if !slices.Contains(want, a.String()) {
			return false
		}
		seen[a.String()] = true
	}
	return len(seen) == len(want)
}

// sameModel reports whether the changes left the zones and records as they
// were.
func sameModel(a, b dnsfile.Model) bool {
	return slices.Equal(a.Zones, b.Zones) && slices.Equal(a.Records, b.Records)
}

// hasMarker reports whether a line of b starts with "# nexus:": Nexus has
// written the file before.
func hasMarker(b []byte) bool {
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, nexusMarker) {
			return true
		}
	}
	return false
}

var errTooLarge = errors.New("larger than 1 MiB")

// readLive reads the DNS file, which must be a regular file (never followed
// through a symlink) of at most MaxFileBytes.
func readLive(path string) ([]byte, os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s: %w: not a regular file", path, fs.ErrInvalid)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(b) > MaxFileBytes {
		return nil, nil, errTooLarge
	}
	return b, fi, nil
}

func ownerOf(fi os.FileInfo) (owner, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return owner{}, fmt.Errorf("%s: no owner information", fi.Name())
	}
	return owner{uid: int(st.Uid), gid: int(st.Gid), mode: fi.Mode().Perm()}, nil
}

// writeTemp writes b to .<base>.nexus-<random>.tmp in dir: dot-prefixed so
// dnsmasq never loads it (V-11, A-31), on the same filesystem as the live
// file so the rename is atomic, with the given owner, group and mode, and
// fsynced. On failure nothing is left behind.
func writeTemp(dir, base string, b []byte, o owner) (string, error) {
	var id [4]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, "."+base+".nexus-"+hex.EncodeToString(id[:])+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if err := f.Chown(o.uid, o.gid); err != nil {
		return "", errors.Join(err, f.Close(), os.Remove(tmp))
	}
	if err := f.Chmod(o.mode); err != nil {
		return "", errors.Join(err, f.Close(), os.Remove(tmp))
	}
	if err := writeSyncClose(f, b); err != nil {
		return "", errors.Join(err, os.Remove(tmp))
	}
	return tmp, nil
}

// writeCopy writes the rollback copy, 0600, and fsyncs it and its
// directory.
func writeCopy(path, dir string, b []byte) error {
	if err := writeSynced(path, b); err != nil {
		return err
	}
	return syncDir(dir)
}

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (p *pipeline) unreadable(err error) *problem.Error {
	p.logf("reading %s failed: %v; nothing was changed", p.path, err)
	detail := fmt.Sprintf("the DNS file %s cannot be read; nothing was changed", p.path)
	if errors.Is(err, errTooLarge) {
		detail = fmt.Sprintf("the DNS file %s is larger than 1 MiB; nothing was changed", p.path)
	}
	return problem.New(problem.DependencyUnavailable, detail).With("provider", spec.Name)
}

func conflict(expected, current, detail string) *problem.Error {
	return problem.New(problem.DNSFileConflict, detail).With("expectedEtag", expected).With("currentEtag", current)
}

func internal(format string, args ...any) *problem.Error {
	return problem.New(problem.InternalError, fmt.Sprintf(format, args...))
}

// RefusalProblem maps the model's refusal of changes[i] (FR-19, FR-21,
// ASM-11), for Admit and for the pipeline alike: an existing zone or record,
// a zone that still has records, or a duplicate name is conflict; an
// undeclared zone or a missing record is not-found; a record name under no
// zone is dns-name-unzoned; an invalid change is validation-failed. In a
// batch (any size, FR-26) it is dns-batch-rejected naming the index, with
// the slug the single operation would have returned as its cause.
func RefusalProblem(i int, batch bool, c dnsfile.Change, err error) *problem.Error {
	var pe *problem.Error
	detail := err.Error() + "; nothing was changed"
	var notEmpty dnsfile.ErrZoneNotEmpty
	switch {
	case errors.Is(err, dnsfile.ErrZoneNotFound), errors.Is(err, dnsfile.ErrRecordNotFound):
		pe = problem.New(problem.NotFound, detail)
	case errors.Is(err, dnsfile.ErrUnzoned):
		pe = problem.New(problem.DNSNameUnzoned, detail)
		if ar, ok := c.(dnsfile.AddRecord); ok {
			pe = pe.With("name", ar.FQDN)
		}
	case errors.Is(err, dnsfile.ErrZoneExists), errors.Is(err, dnsfile.ErrRecordExists),
		errors.Is(err, dnsfile.ErrDuplicate), errors.As(err, &notEmpty):
		pe = problem.New(problem.Conflict, detail)
	case errors.Is(err, dnsfile.ErrInvalidChange):
		pe = problem.New(problem.ValidationFailed, "the input is invalid").
			With("errors", []problem.FieldError{{Path: "", Message: err.Error()}})
	default:
		pe = problem.New(problem.InternalError, detail)
	}
	if !batch {
		return pe
	}
	return BatchRejected(i, pe.Slug(), detail)
}

// BatchRejected is dns-batch-rejected for changes[i], whose single-operation
// refusal would have been the slug cause.
func BatchRejected(i int, cause problem.Slug, detail string) *problem.Error {
	return problem.New(problem.DNSBatchRejected, fmt.Sprintf("changes[%d]: %s", i, detail)).
		With("changeIndex", i).With("cause", string(cause))
}

// PIDOf is the pointer form of FTLPID's result: nil when no FTL runs.
func PIDOf(pid int, ok bool) *int {
	if !ok {
		return nil
	}
	return &pid
}

// PIDText is pid for the operation log.
func PIDText(pid *int) string {
	if pid == nil {
		return "none"
	}
	return fmt.Sprint(*pid)
}

// PIDAfterRestart reads FTL's PID every poll.Interval until it names a live
// FTL other than before, for up to poll.Limit; then it is whatever FTLPID
// returns, nil when none, which is not a failure (A-55, A-56).
func PIDAfterRestart(ctx context.Context, m backend.Mutator, before *int, poll PIDPoll) *int {
	poll = poll.withDefaults()
	t := time.NewTicker(poll.Interval)
	defer t.Stop()
	limit := time.NewTimer(poll.Limit)
	defer limit.Stop()
	for {
		pid := PIDOf(m.FTLPID())
		if pid != nil && (before == nil || *pid != *before) {
			return pid
		}
		select {
		case <-ctx.Done():
			return PIDOf(m.FTLPID())
		case <-limit.C:
			return PIDOf(m.FTLPID())
		case <-t.C:
		}
	}
}
