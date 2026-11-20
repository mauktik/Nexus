package storage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/vzdump"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// This file holds the health read of DF-5: collectHealth, which backs both
// storage.health.get and HealthSignals (the provider.HealthSource the
// daemon fans in to host.health.get, FR-16). It reads the features through
// the functions of DF-2 to DF-4 and never runs the snapshot listing, which
// no FR-15 rule uses.
//
// A signal's subject is one of:
//   - pool:<name>, dataset:<name>;
//   - guest:<type>/<vmid>, or guest:<vmid> for an expected guest with no
//     archive, whose type nothing names;
//   - share:<path>, bundle:<path>/<name>;
//   - zfs, for a zfs-unresponsive read whose argv names no pool (every
//     health read), else pool:<name>.

// The FR-15 signal ids.
const (
	signalPoolNotOnline   = "pool-not-online"
	signalPoolErrors      = "pool-errors"
	signalPoolCapacity    = "pool-capacity"
	signalScrubOverdue    = "scrub-overdue"
	signalScrubAgeUnknown = "scrub-age-unknown"
	signalScrubErrors     = "scrub-errors"
	signalDatasetQuota    = "dataset-quota"
	signalBackupStale     = "backup-stale"
	signalBackupFailed    = "backup-failed"
	signalTMStale         = "timemachine-stale"
	signalTMShareFull     = "timemachine-share-full"
	signalZFSUnresponsive = "zfs-unresponsive"
)

// subjectZFS is the subject of a zfs-unresponsive read that names no pool.
const subjectZFS = "zfs"

// maxFeatureReasons bounds the reasons joined under one feature's
// unavailable key; the rest are counted.
const maxFeatureReasons = 10

// healthReport is one collectHealth: the signals sorted and capped, the
// counts over every signal found, and which features were read.
type healthReport struct {
	observedAt  time.Time
	counts      apitypes.SignalCounts
	signals     []apitypes.HealthSignal
	truncated   bool
	checked     []string
	unavailable map[string]string
}

// featureHealth is what one feature's part of a collect found.
type featureHealth struct {
	read    bool // Something of the feature was read: it is in checked
	signals []apitypes.HealthSignal
	reasons []string // Why parts of it were not read
}

func (f *featureHealth) add(id, severity, subject, message string, at time.Time) {
	f.signals = append(f.signals, apitypes.HealthSignal{ID: id, Severity: severity, Subject: subject,
		Message: message, ObservedAt: at})
}

// healthGet is storage.health.get (DF-5): one item, current.
func (p *Provider) healthGet(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.Health], error) {
	r := p.collectHealth(ctx)
	props := spec.HealthProperties{ObservedAt: r.observedAt, Counts: r.counts,
		Signals: append(spec.Array[apitypes.HealthSignal]{}, r.signals...), SignalsTruncated: r.truncated,
		Checked: append(spec.Array[string]{}, r.checked...)}
	if len(r.unavailable) > 0 {
		props.Unavailable = spec.Unavailable(r.unavailable)
	}
	return apitypes.List[spec.Health]{Items: []spec.Health{resource(p, spec.HealthType, spec.CurrentName, props)}}, nil
}

// HealthSignals is the provider.HealthSource of FR-16: the report of
// storage.health.get, with Counts over every signal and Truncated when the
// 500-signal cap applied. It honours ctx's deadline: a read the deadline
// cuts short leaves its feature's reason in Unavailable.
func (p *Provider) HealthSignals(ctx context.Context) (provider.HealthReport, error) {
	r := p.collectHealth(ctx)
	return provider.HealthReport{ObservedAt: r.observedAt, Counts: r.counts, Signals: r.signals,
		Truncated: r.truncated, Unavailable: r.unavailable}, nil
}

var _ provider.HealthSource = (*Provider)(nil)

// collectHealth is DF-5. It reads the settings once, so a Configure during
// the collect applies to the next one. Each feature is gated by D-6 (an
// unavailable feature whose check is 5 s old is re-checked, shared with
// concurrent callers) and the available ones are read concurrently. A
// feature that is unavailable, or whose read failed, contributes no signal
// other than zfs-unresponsive and is named in unavailable; checked lists
// the features something was read of, so nothing unread is reported
// healthy.
func (p *Provider) collectHealth(ctx context.Context) healthReport {
	s := p.Settings()
	now := p.now()
	var zh, bh, th featureHealth
	gate := func(feature string, fh *featureHealth, read func(*featureHealth)) func() {
		return func() {
			if ok, reason := p.healthFeature(ctx, feature); !ok {
				fh.reasons = append(fh.reasons, reason)
				return
			}
			read(fh)
		}
	}
	parallel(
		gate(featureZFS, &zh, func(fh *featureHealth) { p.healthZFS(ctx, s, now, fh) }),
		gate(featureBackups, &bh, func(fh *featureHealth) { p.healthBackups(ctx, s, now, fh) }),
		gate(featureTimeMachine, &th, func(fh *featureHealth) { p.healthTimeMachine(ctx, s, now, fh) }),
	)

	r := healthReport{observedAt: now, checked: []string{}}
	var all []apitypes.HealthSignal
	for i, fh := range []*featureHealth{&zh, &bh, &th} {
		name := featureNames[i]
		if fh.read {
			r.checked = append(r.checked, name)
		}
		if reason := joinReasons(fh.reasons); reason != "" {
			if r.unavailable == nil {
				r.unavailable = map[string]string{}
			}
			r.unavailable[name] = reason
		}
		all = append(all, fh.signals...)
	}
	for _, sg := range all {
		switch sg.Severity {
		case apitypes.SeverityCritical:
			r.counts.Critical++
		case apitypes.SeverityWarning:
			r.counts.Warning++
		default:
			r.counts.Info++
		}
	}
	slices.SortFunc(all, compareSignals)
	if len(all) > spec.MaxSignals {
		all, r.truncated = all[:spec.MaxSignals:spec.MaxSignals], true
	}
	r.signals = all
	if r.signals == nil {
		r.signals = []apitypes.HealthSignal{}
	}
	return r
}

// severityRank orders critical, warning, info.
func severityRank(s string) int {
	switch s {
	case apitypes.SeverityCritical:
		return 0
	case apitypes.SeverityWarning:
		return 1
	}
	return 2
}

// compareSignals is DF-5's order: severity, then id, then subject.
func compareSignals(a, b apitypes.HealthSignal) int {
	if c := severityRank(a.Severity) - severityRank(b.Severity); c != 0 {
		return c
	}
	if c := strings.Compare(a.ID, b.ID); c != 0 {
		return c
	}
	return strings.Compare(a.Subject, b.Subject)
}

// joinReasons joins a feature's distinct reasons in order, at most
// maxFeatureReasons of them, counting the rest.
func joinReasons(reasons []string) string {
	var out []string
	seen := map[string]bool{}
	more := 0
	for _, r := range reasons {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		if len(out) == maxFeatureReasons {
			more++
			continue
		}
		out = append(out, r)
	}
	if more > 0 {
		out = append(out, fmt.Sprintf("and %d more", more))
	}
	return strings.Join(out, "; ")
}

// healthFeature is D-6 on the health path, as p.require applies it to a
// handler: an unavailable feature whose last check is recheckInterval old
// or older is re-checked first, the re-check shared with concurrent
// callers. It returns whether the feature is available and, when not, its
// reason.
func (p *Provider) healthFeature(ctx context.Context, feature string) (bool, string) {
	st := p.feats.get(feature)
	if !st.Available && p.now().Sub(st.CheckedAt) >= recheckInterval {
		st = p.recheck(ctx, feature)
	}
	return st.Available, st.Reason
}

// healthZFS reads zpool list, zpool status and the dataset listing
// concurrently, with the functions and parsers of DF-2 (readCmd, the
// last-scrub memo, poolProps, readDatasets), and evaluates the pool, scrub
// and dataset rules. It runs the two pool reads itself rather than through
// readPools, which drops zpool status's failure when zpool list fails:
// health raises one zfs-unresponsive per failed argv.
func (p *Provider) healthZFS(ctx context.Context, s Settings, now time.Time, fh *featureHealth) {
	z := p.zfs.Load()
	if z == nil || z.tools == nil {
		fh.reasons = append(fh.reasons, "zfs: not checked yet")
		return
	}
	var list, status []zfs.Pool
	var ds []zfs.Dataset
	var listErr, statusErr, dsErr error
	var statusText bool
	parallel(func() {
		list, _, listErr = readCmd(ctx, p, z, zfs.CmdPoolList, poolListWhat, zfs.ParsePoolListJSON, zfs.ParsePoolListText)
	}, func() {
		status, statusText, statusErr = readCmd(ctx, p, z, zfs.CmdPoolStatus, poolStatusWhat, zfs.ParseStatusJSON, zfs.ParseStatusText)
	}, func() {
		ds, dsErr = p.readDatasets(ctx, z)
	})
	if statusErr == nil {
		p.observeStatus(status)
	}
	for _, f := range []struct {
		cmd zfs.Command
		err error
	}{{zfs.CmdPoolList, listErr}, {zfs.CmdPoolStatus, statusErr}, {zfs.CmdDatasetList, dsErr}} {
		if f.err == nil {
			continue
		}
		fh.reasons = append(fh.reasons, featureReason(featureZFS, fieldReason(f.err)))
		// A read the caller's deadline cut short is the caller's, not ZFS's:
		// its kind says so, per read, whatever the other reads did.
		if reason, ok := unresponsive(f.err); ok {
			fh.add(signalZFSUnresponsive, apitypes.SeverityCritical, unresponsiveSubject(p.healthArgv(z, f.cmd)),
				"a ZFS health read did not answer: "+reason, now)
		}
	}

	t := s.Thresholds
	if listErr == nil {
		fh.read = true
		for _, lp := range list {
			sp, reason := statusOf(status, statusErr, lp.Name)
			pp := p.poolProps(lp, sp, reason, statusText, now, false)
			poolSignals(fh, lp.Name, pp, t, now)
		}
	}
	if dsErr == nil {
		fh.read = true
		for _, d := range ds {
			dp := datasetProps(d)
			if (dp.UsedPercentBasis == basisQuota || dp.UsedPercentBasis == basisRefquota) && dp.UsedPercent != nil &&
				*dp.UsedPercent >= float64(t.QuotaWarn) {
				fh.add(signalDatasetQuota, apitypes.SeverityWarning, "dataset:"+d.Name,
					fmt.Sprintf("dataset %s uses %.1f%% of its %s (warning at %d%%)", d.Name, *dp.UsedPercent,
						dp.UsedPercentBasis, t.QuotaWarn), now)
			}
		}
	}
}

// healthArgv is the argv a health read of cmd ran, in the form readCmd
// chose for it (D-5).
func (p *Provider) healthArgv(z *zfsState, cmd zfs.Command) []string {
	format := z.format
	if _, ok := p.reads.textOnly.Load(cmd); ok {
		format = zfs.Text
	}
	return zfs.Argv(z.paths, format, cmd)
}

// poolSignals evaluates the pool and scrub rules of DF-5 on one pool's
// wire properties. A field that could not be read evaluates nothing.
func poolSignals(fh *featureHealth, name string, pp spec.PoolProperties, t config.StorageThresholdsConfig, now time.Time) {
	subject := "pool:" + name
	if pp.Health != zfs.HealthOnline {
		fh.add(signalPoolNotOnline, apitypes.SeverityCritical, subject,
			fmt.Sprintf("pool %s is %s, not online", name, pp.Health), now)
	}
	var e spec.ErrorCounts
	if pp.Errors != nil {
		e = *pp.Errors
	}
	var perm uint64
	if pp.PermanentErrors != nil {
		perm = *pp.PermanentErrors
	}
	if e.Read > 0 || e.Write > 0 || e.Checksum > 0 || perm > 0 {
		fh.add(signalPoolErrors, apitypes.SeverityCritical, subject,
			fmt.Sprintf("pool %s has %d read, %d write and %d checksum errors and %d files with permanent errors",
				name, e.Read, e.Write, e.Checksum, perm), now)
	}
	if c := pp.CapacityPercent; c != nil {
		msg := fmt.Sprintf("pool %s is %d%% full (warning at %d%%, critical at %d%%)", name, *c,
			t.PoolCapacityWarn, t.PoolCapacityCrit)
		switch {
		case *c >= t.PoolCapacityCrit:
			fh.add(signalPoolCapacity, apitypes.SeverityCritical, subject, msg, now)
		case *c >= t.PoolCapacityWarn:
			fh.add(signalPoolCapacity, apitypes.SeverityWarning, subject, msg, now)
		}
	}
	sc := pp.Scan
	if sc == nil {
		return
	}
	switch {
	case sc.Function == zfs.ScanNone:
		fh.add(signalScrubOverdue, apitypes.SeverityWarning, subject,
			fmt.Sprintf("no scrub has been recorded on pool %s", name), now)
	case sc.LastScrubAgeDays != nil:
		if *sc.LastScrubAgeDays > float64(t.ScrubMaxAgeDays) {
			fh.add(signalScrubOverdue, apitypes.SeverityWarning, subject,
				fmt.Sprintf("the last scrub of pool %s finished %.1f days ago, more than %d", name,
					*sc.LastScrubAgeDays, t.ScrubMaxAgeDays), now)
		}
	default:
		// D-14, A-21: the scan shown hides the last completed scrub and the
		// memo has none; the age is unknown, never overdue and never fine.
		fh.add(signalScrubAgeUnknown, apitypes.SeverityInfo, subject,
			fmt.Sprintf("the age of the last scrub of pool %s is unknown: it shows a %s %s and no finished scrub "+
				"was observed since nexusd started", name, sc.Function, sc.State), now)
	}
	if sc.Function == zfs.ScanScrub && sc.State == zfs.ScanFinished {
		var errs, repaired uint64
		if sc.ErrorsRepaired != nil {
			errs = *sc.ErrorsRepaired
		}
		if sc.BytesRepaired != nil {
			repaired = *sc.BytesRepaired
		}
		if errs > 0 || repaired > 0 {
			fh.add(signalScrubErrors, apitypes.SeverityCritical, subject,
				fmt.Sprintf("the last scrub of pool %s repaired %d bytes with %d errors", name, repaired, errs), now)
		}
	}
}

// unresponsive reports whether err, from a health read, is one DF-5 raises
// zfs-unresponsive for (A-48): pending, timed out, out of process slots,
// not started, or a non-zero exit, by the kind runZFS gave it. A parse
// failure, an output over the cap, an unexpected error and a wait the
// caller's deadline cut are only a reason in unavailable. It returns the
// reason, which names the command.
func unresponsive(err error) (string, bool) {
	var re *readError
	if !errors.As(err, &re) {
		return "", false
	}
	switch re.kind {
	case kindExit, kindTimeout, kindPending, kindBusy, kindNotStarted:
		return re.reason, true
	}
	return "", false
}

// unresponsiveSubject is a zfs-unresponsive signal's subject: pool:<name>
// when argv names a pool (or a dataset or snapshot of one) as its last
// operand, else zfs. zfs.Argv puts names last; the subcommand, an option
// and the value of -o, -t or -d are not names.
func unresponsiveSubject(argv []string) string {
	if len(argv) < 3 {
		return subjectZFS
	}
	last, prev := argv[len(argv)-1], argv[len(argv)-2]
	if strings.HasPrefix(last, "-") || slices.Contains([]string{"-o", "-t", "-d"}, prev) {
		return subjectZFS
	}
	pool, _, _ := strings.Cut(last, "/")
	pool, _, _ = strings.Cut(pool, "@")
	if !validPoolName(pool) {
		return subjectZFS
	}
	return "pool:" + pool
}

// healthBackups reads the backup report of DF-3 and evaluates backup-stale
// and backup-failed per guest. A configured path that could not be listed
// is named in unavailable; the others are evaluated.
func (p *Provider) healthBackups(ctx context.Context, s Settings, now time.Time, fh *featureHealth) {
	rep, err := p.readBackupReport(ctx, s, false)
	if err != nil {
		fh.reasons = append(fh.reasons, problemReason(err))
		return
	}
	fh.read = true
	for _, d := range rep.directories {
		if !d.Available {
			fh.reasons = append(fh.reasons, d.Reason)
		}
	}
	maxAge := rep.maxAge.Hours()
	for _, g := range rep.guests {
		subject := fmt.Sprintf("guest:%d", g.VMID)
		guest := fmt.Sprintf("%d", g.VMID)
		if g.GuestType != "" {
			subject = fmt.Sprintf("guest:%s/%d", g.GuestType, g.VMID)
			guest = fmt.Sprintf("%s/%d", g.GuestType, g.VMID)
		}
		if g.Stale {
			msg := fmt.Sprintf("the latest backup of guest %s is %.0f hours old, more than %.0f", guest, ptrOr(g.AgeHours),
				maxAge)
			switch {
			case g.GuestType == "":
				msg = fmt.Sprintf("expected guest %d has no backup archive", g.VMID)
			case g.AgeHours == nil:
				msg = fmt.Sprintf("guest %s has no completed backup archive", guest)
			}
			fh.add(signalBackupStale, apitypes.SeverityWarning, subject, msg, now)
		}
		if g.LatestLogStatus == vzdump.LogError {
			fh.add(signalBackupFailed, apitypes.SeverityCritical, subject,
				fmt.Sprintf("the log of the latest backup of guest %s reports an error", guest), now)
		}
	}
}

// problemReason is the reason a feature's read answered err with: the
// problem's reason extension, else its text.
func problemReason(err error) string {
	var pe *problem.Error
	if errors.As(err, &pe) {
		if r, ok := pe.Extensions["reason"].(string); ok {
			return r
		}
	}
	return err.Error()
}

func ptrOr[T any](v *T) T {
	var zero T
	if v == nil {
		return zero
	}
	return *v
}

// healthTimeMachine reads each configured share as DF-4 does (statfs and
// its bundles; no dataset match, which no rule uses) and evaluates
// timemachine-share-full per share and timemachine-stale per bundle. A
// share's usage or bundle that could not be read is named in unavailable.
func (p *Provider) healthTimeMachine(ctx context.Context, s Settings, now time.Time, fh *featureHealth) {
	paths := s.TimeMachine.Paths
	shares := make([]spec.ShareProperties, len(paths))
	fns := make([]func(), len(paths))
	for i, path := range paths {
		fns[i] = func() { shares[i] = p.readShare(ctx, path, s.TimeMachine.MaxAge, now) }
	}
	if len(fns) > 0 {
		parallel(fns...)
	}
	t := s.Thresholds
	for i, sp := range shares {
		path := paths[i]
		reason := func(r string) {
			if !strings.Contains(r, path) {
				r = path + ": " + r
			}
			fh.reasons = append(fh.reasons, r)
		}
		if u := sp.UsedPercent; u != nil {
			fh.read = true
			msg := fmt.Sprintf("Time Machine share %s is %.1f%% full (warning at %d%%, critical at %d%%)", path, *u,
				t.ShareWarn, t.ShareCrit)
			switch {
			case *u >= float64(t.ShareCrit):
				fh.add(signalTMShareFull, apitypes.SeverityCritical, "share:"+path, msg, now)
			case *u >= float64(t.ShareWarn):
				fh.add(signalTMShareFull, apitypes.SeverityWarning, "share:"+path, msg, now)
			}
		} else {
			reason(sp.Unavailable["usedPercent"])
		}
		if r, ok := sp.Unavailable["bundles"]; ok {
			reason(r)
			continue
		}
		fh.read = true
		for _, b := range sp.Bundles {
			if r, ok := b.Unavailable["stale"]; ok {
				reason(r)
				continue
			}
			if !b.Stale {
				continue
			}
			msg := fmt.Sprintf("Time Machine bundle %s in %s has no backup date", b.Name, path)
			if at := b.LastBackupAt; at != nil || b.LastModifiedAt != nil {
				if at == nil {
					at = b.LastModifiedAt
				}
				msg = fmt.Sprintf("Time Machine bundle %s in %s last backed up at %s, more than %s ago", b.Name, path,
					at.UTC().Format(time.RFC3339), s.TimeMachine.MaxAge)
			}
			fh.add(signalTMStale, apitypes.SeverityWarning, "bundle:"+path+"/"+b.Name, msg, now)
		}
	}
}
