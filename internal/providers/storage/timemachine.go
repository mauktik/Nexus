package storage

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/timemachine"
)

// This file holds the Time Machine read of DF-4: storage.timemachine.list
// and the per-share read the health read shares. Every filesystem access of
// a share runs under bounded (D-15), never under zfs.Tools: "statfs <path>"
// for its usage, "scan <path>" for its bundle listing and "read
// <path>/<bundle>" for one bundle's history plist and fallback mtimes. A
// stuck or failed access makes only that share, or that bundle's fields,
// unavailable with its reason. Bundle sizes come only from the Background
// walk's cache (D-12); no request walks.

// The lastBackupSource words (DF-4).
const (
	sourcePlist = "plist"
	sourceMtime = "mtime-best-effort"
)

// Reasons of the Time Machine read.
const (
	reasonNotComputed = "not computed yet"
	reasonNoDate      = "no completion date and no plist modification time: SnapshotHistory, Results and Info " +
		"plists are all missing"
	reasonNoDataset = "no mounted ZFS dataset has this path as its mountpoint"
	reasonNoQuota   = "the dataset has no quota"
	reasonNoBlocks  = "statfs reports no used or available blocks"
	reasonNoBackup  = "no bundle has a last backup or modification time"
)

// shareDataset is the dataset mounted at a share: its name, used bytes and
// quota.
type shareDataset struct {
	name        string
	used, quota *uint64
}

// datasetMatch is the share-to-dataset match of one request: the mounted
// datasets by mountpoint, or the reason there is none (zfs unavailable or
// its listing failed).
type datasetMatch struct {
	byMount map[string]shareDataset
	reason  string
}

// shareDatasets is the match of DF-4 (A-8): the cached full dataset listing
// (of storage.datasets.list or the health read, WI 4.2) while it is younger
// than maxDatasetListingAge, else a fresh listing; neither when zfs is
// unavailable. zfs is not this read's feature,
// so its state is read as the last check left it, never re-checked here: a
// zfs re-check bounded by commandTimeout must not hold a Time Machine read.
// Volumes and unmounted datasets match nothing.
func (p *Provider) shareDatasets(ctx context.Context) datasetMatch {
	st := p.feats.get(featureZFS)
	z := p.zfs.Load()
	switch {
	case !st.Available:
		return datasetMatch{reason: featureReason(featureZFS, st.Reason)}
	case z == nil || z.tools == nil:
		return datasetMatch{reason: "zfs: not checked yet"}
	}
	m := datasetMatch{byMount: map[string]shareDataset{}}
	add := func(name string, mountpoint *string, mounted *bool, used, quota *uint64) {
		if mountpoint != nil && (mounted == nil || *mounted) {
			m.byMount[*mountpoint] = shareDataset{name: name, used: used, quota: quota}
		}
	}
	if l, ok := p.lastDatasets(); ok {
		for _, d := range l.Items {
			add(d.Name, d.Mountpoint, d.Mounted, d.Used, d.Quota)
		}
		return m
	}
	ds, err := p.readDatasets(ctx, z)
	if err != nil {
		return datasetMatch{reason: fieldReason(err)}
	}
	for _, d := range ds {
		add(d.Name, d.Mountpoint, d.Mounted, d.Used, d.Quota)
	}
	return m
}

// shareName is a share's item name: its path, path-escaped.
func shareName(path string) string { return url.PathEscape(path) }

// validShareKey is the cursor check of timemachine.list: an escaped
// absolute path.
func validShareKey(k string) bool {
	p, err := url.PathUnescape(k)
	return err == nil && strings.HasPrefix(p, "/") && shareName(p) == k
}

// timemachineList is storage.timemachine.list (DF-4): the configured shares
// sorted by name, paged; only the page's shares are read.
func (p *Provider) timemachineList(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Share], error) {
	out := apitypes.List[spec.Share]{Items: []spec.Share{}}
	limit, err := pageLimit(in.Limit)
	if err != nil {
		return out, err
	}
	after, err := decodeCursor(in.Cursor, validShareKey)
	if err != nil {
		return out, err
	}
	if err := p.require(ctx, featureTimeMachine); err != nil {
		return out, err
	}
	s := p.Settings()
	paths := slices.Clone(s.TimeMachine.Paths)
	slices.SortFunc(paths, func(a, b string) int { return strings.Compare(shareName(a), shareName(b)) })
	page, next := paginate(paths, limit, func(path string) bool { return after != "" && shareName(path) <= after },
		shareName)
	if len(page) == 0 {
		return out, nil
	}
	shares := p.readShares(ctx, s, page)
	for i, path := range page {
		name := shareName(path)
		out.Items = append(out.Items, spec.Share{ID: apitypes.ResourceID(spec.Name, spec.TimeMachineType, name),
			Type: spec.TimeMachineType, Name: name, Host: p.host, Properties: shares[i]})
	}
	out.NextCursor = next
	return out, nil
}

// readShares reads each share of paths concurrently (DF-4).
// The dataset match runs beside them.
func (p *Provider) readShares(ctx context.Context, s Settings, paths []string) []spec.ShareProperties {
	now := p.now()
	out := make([]spec.ShareProperties, len(paths))
	var match datasetMatch
	fns := []func(){func() { match = p.shareDatasets(ctx) }}
	for i, path := range paths {
		fns = append(fns, func() { out[i] = p.readShare(ctx, path, s.TimeMachine.MaxAge, now) })
	}
	parallel(fns...)
	for i, path := range paths {
		shareDatasetProps(&out[i], path, match)
	}
	return out
}

// bundleList is one share's bundle listing.
type bundleList struct {
	dirs      []string
	truncated bool
}

// readShare is one share's usage from statfs and its bundles, each read on
// its own.
func (p *Provider) readShare(ctx context.Context, path string, maxAge time.Duration, now time.Time) spec.ShareProperties {
	sp := spec.ShareProperties{Path: path, Bundles: spec.Array[spec.BundleProperties]{}}

	u, err := bounded(ctx, p.fl, path, "statfs "+path, func() (Usage, error) { return p.statfs(path) })
	if err != nil {
		reason := fsReason(path, err)
		for _, f := range []string{"sizeBytes", "usedBytes", "availableBytes", "usedPercent"} {
			setReason(&sp.Unavailable, f, reason)
		}
	} else {
		usage(&sp, u)
	}

	l, err := bounded(ctx, p.fl, path, "scan "+path, func() (bundleList, error) {
		root, err := p.openRoot(path)
		if err != nil {
			return bundleList{}, err
		}
		defer func() { _ = root.Close() }() // read-only; a close error loses nothing
		dirs, truncated, err := timemachine.Bundles(root, spec.MaxBundles)
		return bundleList{dirs: dirs, truncated: truncated}, err
	})
	if err != nil {
		reason := fsReason(path, err)
		for _, f := range []string{"bundles", "bundleCount", "newestBackupAt"} {
			setReason(&sp.Unavailable, f, reason)
		}
		return sp
	}
	bundles := make([]spec.BundleProperties, len(l.dirs))
	jobs := make([]func(), len(l.dirs))
	for i, dir := range l.dirs {
		jobs[i] = func() { bundles[i] = p.readBundle(ctx, path, dir, maxAge, now) }
	}
	runPerPath(ctx, map[string][]func(){path: jobs})
	sp.Bundles, sp.BundleCount, sp.BundlesTruncated = bundles, len(bundles), l.truncated
	for _, b := range bundles {
		at := b.LastBackupAt
		if at == nil {
			at = b.LastModifiedAt
		}
		if at != nil && (sp.NewestBackupAt == nil || at.After(*sp.NewestBackupAt)) {
			sp.NewestBackupAt = at
		}
	}
	if sp.NewestBackupAt == nil {
		setReason(&sp.Unavailable, "newestBackupAt", reasonNoBackup)
	}
	return sp
}

// usage sets a share's statfs fields: with used = blocks − bfree,
// sizeBytes = blocks × bsize, usedBytes = used × bsize, availableBytes =
// bavail × bsize and usedPercent = used / (used + bavail) × 100, df's Use%
// before it rounds up (DF-4, TA-12): it differs from (blocks − bavail) /
// blocks when bfree ≠ bavail, as on ext4 with reserved blocks.
func usage(sp *spec.ShareProperties, u Usage) {
	used := u.Blocks - min(u.Bfree, u.Blocks)
	sp.SizeBytes = ptrTo(u.Blocks * u.Bsize)
	sp.UsedBytes = ptrTo(used * u.Bsize)
	sp.AvailableBytes = ptrTo(u.Bavail * u.Bsize)
	if used+u.Bavail == 0 {
		setReason(&sp.Unavailable, "usedPercent", reasonNoBlocks)
		return
	}
	sp.UsedPercent = ptrTo(float64(used) / float64(used+u.Bavail) * 100)
}

// shareDatasetProps sets the share's dataset, quota and quota-based used
// percent from the match, or nulls them with the reason.
func shareDatasetProps(sp *spec.ShareProperties, path string, match datasetMatch) {
	reason := match.reason
	d, ok := match.byMount[path]
	if reason == "" && !ok {
		reason = reasonNoDataset
	}
	if reason != "" {
		for _, f := range []string{"dataset", "quotaBytes", "quotaUsedPercent"} {
			setReason(&sp.Unavailable, f, reason)
		}
		return
	}
	sp.Dataset = ptrTo(d.name)
	switch {
	case d.quota == nil || *d.quota == 0:
		setReason(&sp.Unavailable, "quotaBytes", reasonNoQuota)
		setReason(&sp.Unavailable, "quotaUsedPercent", reasonNoQuota)
	case d.used == nil:
		sp.QuotaBytes = d.quota
		setReason(&sp.Unavailable, "quotaUsedPercent", "the dataset's used bytes could not be read")
	default:
		sp.QuotaBytes = d.quota
		sp.QuotaUsedPercent = ptrTo(float64(*d.used) / float64(*d.quota) * 100)
	}
}

// readBundle is one bundle of a share: its history plist and fallback
// mtimes under bounded keyed "read <path>/<bundle>", and its size from the
// Background walk's cache. inProgress is always null (A-16). A bundle with
// neither a completion date nor a fallback mtime is stale, so a bundle
// never seen to back up is never shown fresh; one whose read failed has its
// staleness unknown, named in unavailable.
func (p *Provider) readBundle(ctx context.Context, path, dir string, maxAge time.Duration,
	now time.Time) spec.BundleProperties {
	bp := spec.BundleProperties{Name: timemachine.Name(dir)}
	_, reason := timemachine.Bundle{}.InProgress()
	setReason(&bp.Unavailable, "inProgress", reason)

	if sz, ok := p.walks.size(path, dir); !ok {
		setReason(&bp.Unavailable, "sizeBytes", reasonNotComputed)
	} else if sz.reason != "" {
		setReason(&bp.Unavailable, "sizeBytes", sz.reason)
	} else {
		bp.SizeBytes, bp.SizeTruncated = ptrTo(sz.size.Bytes), sz.size.Truncated
		bp.SizeComputedAt = ptrTo(sz.size.ComputedAt.UTC())
	}

	b, err := bounded(ctx, p.fl, path, "read "+path+"/"+dir, func() (timemachine.Bundle, error) {
		root, err := p.openRoot(path)
		if err != nil {
			return timemachine.Bundle{}, err
		}
		defer func() { _ = root.Close() }() // read-only; a close error loses nothing
		return timemachine.ReadBundle(root, dir), nil
	})
	if err != nil {
		reason := fsReason(path, err)
		for _, f := range []string{"lastBackupAt", "lastModifiedAt", "stale"} {
			setReason(&bp.Unavailable, f, reason)
		}
		return bp
	}
	bp.LastBackupAt, bp.LastModifiedAt = b.LastBackupAt, b.LastModifiedAt
	at := b.LastBackupAt
	switch {
	case at != nil:
		bp.LastBackupSource = sourcePlist
	case b.LastModifiedAt != nil:
		bp.LastBackupSource, at = sourceMtime, b.LastModifiedAt
		setReason(&bp.Unavailable, "lastBackupAt", b.LastBackupReason)
	default:
		setReason(&bp.Unavailable, "lastBackupAt", b.LastBackupReason)
		setReason(&bp.Unavailable, "lastModifiedAt", reasonNoDate)
	}
	bp.Stale = at == nil || now.Sub(*at) > maxAge
	return bp
}
