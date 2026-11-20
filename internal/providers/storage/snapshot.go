// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// Snapshot create (DF-8). Admission names the snapshot (D-11) and refuses
// before any operation exists; the helper, outside the sandbox, runs zfs
// snapshot with the nexus:created-by property and reads the snapshot back.

// The providers.storage keys that gate snapshot create, as not-allowed
// names them.
const (
	keySnapshotsEnabled  = "providers.storage.snapshots.enabled"
	keySnapshotsDatasets = "providers.storage.snapshots.datasets"
)

// snapshotLanePrefix starts the lane key of a dataset's snapshots.
const snapshotLanePrefix = "storage:snapshot:"

// snapshotTimeLayout is the time part of a snapshot name: UTC, to the
// second (FR-14).
const snapshotTimeLayout = "20060102T150405Z"

// lostSnapshotName is the detail of a snapshot operation that reaches
// Prepare without the name chosen at admission (DF-8 step 3, A-57).
const lostSnapshotName = "the snapshot name chosen at admission did not survive a nexusd restart; nothing was sent; " +
	"request the snapshot again"

var labelRE = regexp.MustCompile(spec.SnapshotLabelPattern)

// snapshotLane is dataset's lane: one snapshot at a time, a second
// answering 409 conflict.
func snapshotLane(dataset string) provider.Lane {
	return provider.Lane{Key: snapshotLanePrefix + dataset, Policy: provider.LaneReject}
}

// snapshotSpec is storage.datasets.snapshot's detached binding. Its
// Admission carries the snapshot name to Prepare in the json:"-" field
// Snapshot (D-11).
func (p *Provider) snapshotSpec() provider.DetachedSpec[spec.SnapshotInput, spec.SnapshotResult] {
	return provider.DetachedSpec[spec.SnapshotInput, spec.SnapshotResult]{
		Admission: p.admitSnapshot,
		Lane: func(_ context.Context, in spec.SnapshotInput) (provider.Lane, error) {
			return snapshotLane(in.Name), nil
		},
		Prepare: func(_ context.Context, in spec.SnapshotInput, r *provider.Run) (detach.Spec, error) {
			return p.prepareSnapshot(in, r)
		},
		Finalize: func(_ context.Context, in spec.SnapshotInput, x detach.Exit, r *provider.Run) (spec.SnapshotResult, error) {
			out, err := finalizeHelper[spec.SnapshotResult](p, snapshotSubject(in.Name), snapshotCheck(in.Name), x, r)
			if err != nil {
				return out, err
			}
			// The helper knows neither the host nor the item's ID.
			out.Snapshot = resource(p, spec.SnapshotsType, out.Snapshot.Name, out.Snapshot.Properties)
			return out, nil
		},
		LostDetail: func(_ *provider.Run, in spec.SnapshotInput) string {
			return lostDetail(snapshotSubject(in.Name), snapshotCheck(in.Name))
		},
	}
}

// snapshotName is the name of a snapshot taken at t: nexus-<UTC
// YYYYMMDDTHHMMSSZ>, with -<label> when a label is given (FR-14, D-11).
func snapshotName(t time.Time, label string) string {
	name := spec.SnapshotPrefix + t.UTC().Format(snapshotTimeLayout)
	if label != "" {
		name += "-" + label
	}
	return name
}

// snapshotAllowed is the configuration's answer for in: nil, or 409
// not-allowed naming the key that refuses. Admission and Prepare both ask
// it.
func snapshotAllowed(s Settings, in spec.SnapshotInput) error {
	if !s.Snapshots.Enabled {
		return problem.New(problem.NotAllowed, "snapshot create is disabled: "+keySnapshotsEnabled+" is false").
			With("configKey", keySnapshotsEnabled)
	}
	i := slices.IndexFunc(s.Snapshots.Datasets, func(d config.StorageSnapshotDataset) bool { return d.Name == in.Name })
	if i < 0 {
		return problem.New(problem.NotAllowed, fmt.Sprintf("dataset %s is not in %s", in.Name, keySnapshotsDatasets)).
			With("configKey", keySnapshotsDatasets).With("dataset", in.Name)
	}
	if in.Recursive && !s.Snapshots.Datasets[i].Recursive {
		key := fmt.Sprintf("%s[%d].recursive", keySnapshotsDatasets, i)
		return problem.New(problem.NotAllowed, fmt.Sprintf("a recursive snapshot of %s is not allowed: %s is false",
			in.Name, key)).With("configKey", key).With("dataset", in.Name)
	}
	return nil
}

// admitSnapshot is storage.datasets.snapshot's admission (DF-8 step 1), in
// order: the input's names, the configuration, the full name's 255 bytes,
// the zfs feature (503), the dataset existing (404) and its pool's
// capacity (409 pool-low-space). Every refusal comes before any operation
// exists. It then names the snapshot and records the facts dataset,
// snapshot and recursive, so the requested record names the snapshot the
// helper creates.
func (p *Provider) admitSnapshot(ctx context.Context, in spec.SnapshotInput) (provider.Admitted[spec.SnapshotInput, spec.SnapshotResult], error) {
	var none provider.Admitted[spec.SnapshotInput, spec.SnapshotResult]
	if !validDatasetName(in.Name) {
		return none, invalidInput("/name", "is not a dataset name")
	}
	if in.Label != "" && !labelRE.MatchString(in.Label) {
		return none, invalidInput("/label", "does not match "+spec.SnapshotLabelPattern)
	}
	s := p.Settings()
	if err := snapshotAllowed(s, in); err != nil {
		return none, err
	}
	name := snapshotName(p.now(), in.Label)
	if full := in.Name + "@" + name; len(full) > spec.MaxSnapshotNameBytes {
		field := "/name"
		if in.Label != "" {
			field = "/label"
		}
		return none, invalidInput(field, fmt.Sprintf("makes the snapshot name %s@%s %d bytes long, over the %d-byte "+
			"limit of a ZFS name", in.Name, name, len(full), spec.MaxSnapshotNameBytes))
	}
	z, err := p.zfsReady(ctx)
	if err != nil {
		return none, err
	}
	datasets, _, err := readCmd(ctx, p, z, zfs.CmdDatasetList, datasetWhat, zfs.ParseDatasetsJSON, zfs.ParseDatasetsText, in.Name)
	if err != nil {
		return none, opError(err)
	}
	if !slices.ContainsFunc(datasets, func(d zfs.Dataset) bool { return d.Name == in.Name }) {
		return none, problem.New(problem.NotFound, fmt.Sprintf("no dataset is named %q", in.Name))
	}
	pool, _, _ := strings.Cut(in.Name, "/")
	if err := p.admitCapacity(ctx, z, s, pool); err != nil {
		return none, err
	}
	in.Snapshot = name
	audit.AddFact(ctx, "dataset", in.Name)
	audit.AddFact(ctx, "snapshot", name)
	audit.AddFact(ctx, "recursive", strconv.FormatBool(in.Recursive))
	return provider.Admitted[spec.SnapshotInput, spec.SnapshotResult]{In: in}, nil
}

// admitCapacity refuses a snapshot on pool when its capacity is at or above
// maxPoolCapacityPercent, or cannot be read.
func (p *Provider) admitCapacity(ctx context.Context, z *zfsState, s Settings, pool string) error {
	pools, _, err := readCmd(ctx, p, z, zfs.CmdPoolList, poolListWhat, zfs.ParsePoolListJSON, zfs.ParsePoolListText, pool)
	if err != nil {
		return opError(err)
	}
	i := slices.IndexFunc(pools, func(lp zfs.Pool) bool { return lp.Name == pool })
	if i < 0 || pools[i].Capacity == nil {
		return problem.New(problem.DependencyUnavailable, fmt.Sprintf("the capacity of pool %s cannot be read from %s, "+
			"so no snapshot is taken", pool, poolListWhat)).With("provider", spec.Name)
	}
	if c, limit := *pools[i].Capacity, s.Snapshots.MaxPoolCapacityPercent; c >= limit {
		return problem.New(problem.PoolLowSpace, fmt.Sprintf("pool %s is %d%% full, at or above %s's %d%%",
			pool, c, "providers.storage.snapshots.maxPoolCapacityPercent", limit)).
			With("pool", pool).With("capacityPercent", c).With("maxCapacityPercent", limit)
	}
	return nil
}

// prepareSnapshot re-checks the configuration in force, then writes the
// helper's input and returns its spec (DF-8 step 3). An input without the
// name chosen at admission, a queued operation that recovery runs after a
// restart (json:"-" fields are not kept), fails with nothing sent: the
// snapshot is never named anywhere but at admission (A-57).
func (p *Provider) prepareSnapshot(in spec.SnapshotInput, r *provider.Run) (detach.Spec, error) {
	s := p.Settings()
	if err := snapshotAllowed(s, in); err != nil {
		return detach.Spec{}, err
	}
	if in.Snapshot == "" {
		return detach.Spec{}, problem.New(problem.InternalError, lostSnapshotName)
	}
	hin := snapshotHelperInput{
		OperationID: r.ID(), Action: actionSnapshot, Dataset: in.Name, Snapshot: in.Snapshot, Recursive: in.Recursive,
		Format: p.helperFormat(zfs.CmdSnapshotGet).String(), CommandTimeout: s.CommandTimeout.String(),
	}
	return p.launchHelper(r, hin, s.CommandTimeout, snapshotSubject(in.Name))
}

// snapshotSubject names a snapshot helper in details.
func snapshotSubject(dataset string) string {
	return "the storage helper for snapshot on dataset " + dataset
}

// snapshotCheck is the command an operator runs by hand to see what a
// snapshot helper did.
func snapshotCheck(dataset string) string { return "zfs list -t snapshot " + dataset }
