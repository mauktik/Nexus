// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// The usedPercentBasis words (DF-2 step 4).
const (
	basisQuota     = "quota"
	basisRefquota  = "refquota"
	basisAvailable = "available"
)

// datasetListing is the last full dataset listing and when it was read.
type datasetListing struct {
	At    time.Time
	Items []zfs.Dataset
}

// maxDatasetListingAge is how long a full dataset listing stays the Time
// Machine read's match: past it the read lists the datasets again, so a
// share's dataset and quota never drift far from the host's.
const maxDatasetListingAge = 60 * time.Second

// lastDatasets is the last full dataset listing, from storage.datasets.list
// or the health read, which the Time Machine read matches a share's path
// against (WI 4.3, DF-4, A-8). ok is false before the first one and once it
// is older than maxDatasetListingAge.
func (p *Provider) lastDatasets() (datasetListing, bool) {
	l := p.reads.datasets.Load()
	if l == nil || p.now().Sub(l.At) >= maxDatasetListingAge {
		return datasetListing{}, false
	}
	return *l, true
}

// snapshotStats is one dataset's snapshot count and newest creation time.
type snapshotStats struct {
	count  int
	newest *time.Time
}

// datasetsList is storage.datasets.list (DF-2): the dataset listing and the
// name,creation snapshot listing, concurrently. A failed, timed-out or
// oversized snapshot listing leaves snapshotCount and newestSnapshotAt null
// with its reason on every item; the datasets still list.
func (p *Provider) datasetsList(ctx context.Context, in spec.DatasetListInput) (apitypes.List[spec.Dataset], error) {
	out := apitypes.List[spec.Dataset]{Items: []spec.Dataset{}}
	if in.Pool != "" && !validPoolName(in.Pool) {
		return out, invalidInput("/pool", "is not a pool name")
	}
	if in.Type != "" && in.Type != spec.TypeFilesystem && in.Type != spec.TypeVolume {
		return out, invalidInput("/type", "must be filesystem or volume")
	}
	limit, err := pageLimit(in.Limit)
	if err != nil {
		return out, err
	}
	after, err := decodeCursor(in.Cursor, validListedName)
	if err != nil {
		return out, err
	}
	z, err := p.zfsReady(ctx)
	if err != nil {
		return out, err
	}

	var ds []zfs.Dataset
	var stats map[string]snapshotStats
	var dsErr, snapErr error
	parallel(func() {
		ds, dsErr = p.readDatasets(ctx, z)
	}, func() {
		stats, snapErr = p.readSnapshotStats(ctx, z)
	})
	if dsErr != nil {
		return out, opError(dsErr)
	}
	all := make([]spec.Dataset, 0, len(ds))
	for _, d := range ds {
		all = append(all, resource(p, spec.DatasetsType, d.Name, withSnapshots(datasetProps(d), d.Name, stats, snapErr)))
	}
	slices.SortFunc(all, func(a, b spec.Dataset) int { return strings.Compare(a.Name, b.Name) })
	items := slices.DeleteFunc(slices.Clone(all), func(d spec.Dataset) bool {
		return in.Pool != "" && d.Properties.Pool != in.Pool || in.Type != "" && d.Properties.Type != in.Type
	})
	page, next := paginate(items, limit, func(d spec.Dataset) bool { return after != "" && d.Name <= after },
		func(d spec.Dataset) string { return d.Name })
	out.Items = append(out.Items, page...)
	out.NextCursor = next
	return out, nil
}

// datasetsGet is storage.datasets.get (DF-2): the dataset listing of NAME
// and its own snapshots (-d 1 NAME), concurrently, with the field rule of
// datasets.list. A dataset that does not exist is 404 (A-36).
func (p *Provider) datasetsGet(ctx context.Context, in spec.DatasetNameInput) (spec.Dataset, error) {
	if !validDatasetName(in.Name) {
		return spec.Dataset{}, invalidInput("/name", "is not a dataset name")
	}
	z, err := p.zfsReady(ctx)
	if err != nil {
		return spec.Dataset{}, err
	}
	var ds []zfs.Dataset
	var stats map[string]snapshotStats
	var dsErr, snapErr error
	parallel(func() {
		ds, dsErr = p.readDatasets(ctx, z, in.Name)
	}, func() {
		stats, snapErr = p.readSnapshotStats(ctx, z, in.Name)
	})
	if dsErr != nil {
		return spec.Dataset{}, opError(dsErr)
	}
	i := slices.IndexFunc(ds, func(d zfs.Dataset) bool { return d.Name == in.Name })
	if i < 0 {
		return spec.Dataset{}, problem.New(problem.NotFound, fmt.Sprintf("no filesystem or volume is named %q", in.Name))
	}
	return resource(p, spec.DatasetsType, in.Name, withSnapshots(datasetProps(ds[i]), in.Name, stats, snapErr)), nil
}

// readDatasets is the dataset listing, of every filesystem and volume or of
// name, without the snapshot listing: health reads it too, and never runs
// the snapshot listing (DF-5).
func (p *Provider) readDatasets(ctx context.Context, z *zfsState, name ...string) ([]zfs.Dataset, error) {
	ds, _, err := readCmd(ctx, p, z, zfs.CmdDatasetList, datasetWhat, zfs.ParseDatasetsJSON, zfs.ParseDatasetsText, name...)
	if err == nil && len(name) == 0 {
		p.reads.datasets.Store(&datasetListing{At: p.now(), Items: ds})
	}
	return ds, err
}

// readSnapshotStats groups the name,creation snapshot listing, in the -H -p
// text form (D-5 exception), of every dataset or of name's own snapshots,
// by dataset.
func (p *Provider) readSnapshotStats(ctx context.Context, z *zfsState, name ...string) (map[string]snapshotStats, error) {
	out, err := p.runZFS(ctx, z, snapshotWhat, zfs.Argv(z.paths, zfs.Text, zfs.CmdSnapshotTimes, name...))
	if err != nil {
		return nil, err
	}
	snaps, err := zfs.ParseSnapshotsText(out)
	if err != nil {
		return nil, &readError{reason: boundReason(fmt.Sprintf("%s output could not be parsed: %v", snapshotWhat, err))}
	}
	stats := map[string]snapshotStats{}
	for _, s := range snaps {
		st := stats[s.Dataset]
		st.count++
		if st.newest == nil || s.CreatedAt.After(*st.newest) {
			st.newest = ptrTo(s.CreatedAt)
		}
		stats[s.Dataset] = st
	}
	return stats, nil
}

// withSnapshots sets snapshotCount and newestSnapshotAt of the dataset
// named name from stats, or null with snapErr's reason. A dataset with no
// snapshot has count 0 and newestSnapshotAt null.
func withSnapshots(dp spec.DatasetProperties, name string, stats map[string]snapshotStats, snapErr error) spec.DatasetProperties {
	if snapErr != nil {
		reason := fieldReason(snapErr)
		setReason(&dp.Unavailable, "snapshotCount", reason)
		setReason(&dp.Unavailable, "newestSnapshotAt", reason)
		return dp
	}
	st := stats[name]
	dp.SnapshotCount, dp.NewestSnapshotAt = ptrTo(st.count), st.newest
	return dp
}

// datasetProps converts one dataset to the wire type, without the snapshot
// fields, and computes usedPercent against the quota when one is set, else
// the refquota (referenced / refquota), else used plus available, with
// usedPercentBasis naming which (DF-2 step 4, ASM-6).
func datasetProps(d zfs.Dataset) spec.DatasetProperties {
	dp := spec.DatasetProperties{
		Pool: d.Pool, Type: d.Type,
		UsedBytes: d.Used, AvailableBytes: d.Avail, ReferencedBytes: d.Refer, UsedBySnapshotsBytes: d.UsedBySnapshots,
		QuotaBytes: d.Quota, RefquotaBytes: d.Refquota, ReservationBytes: d.Reservation,
		Compression: d.Compression, CompressRatio: d.CompressRatio, Recordsize: d.Recordsize,
		Mountpoint: d.Mountpoint, Mounted: d.Mounted, Encryption: d.Encryption, KeyStatus: d.KeyStatus,
	}
	for k, v := range d.Unavailable {
		setReason(&dp.Unavailable, k, v)
	}
	var num *uint64
	var den uint64
	denKnown := true
	switch {
	case d.Quota != nil:
		dp.UsedPercentBasis, num, den = basisQuota, d.Used, *d.Quota
	case d.Refquota != nil:
		dp.UsedPercentBasis, num, den = basisRefquota, d.Refer, *d.Refquota
	default:
		dp.UsedPercentBasis, num = basisAvailable, d.Used
		if d.Used != nil && d.Avail != nil {
			den = *d.Used + *d.Avail
		} else {
			denKnown = false
		}
	}
	switch {
	case num == nil || !denKnown:
		setReason(&dp.Unavailable, "usedPercent", "a size it is computed from could not be read")
	case den == 0:
		setReason(&dp.Unavailable, "usedPercent", "its "+dp.UsedPercentBasis+" basis is 0 bytes")
	default:
		dp.UsedPercent = ptrTo(float64(*num) / float64(den) * 100)
	}
	return dp
}
