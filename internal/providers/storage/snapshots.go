package storage

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

var (
	// createdByNexusRE is the operation-ID rule of internal/detach/
	// launcher.go:212 that a nexus:created-by value must match for
	// createdByNexus (D-11, A-23).
	createdByNexusRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	sinceRE          = regexp.MustCompile(spec.SincePattern)
)

// snapshotsList is storage.snapshots.list (DF-2): the -H -p text listing
// (D-5 exception) of every snapshot, of one dataset (-d 1) or of its
// subtree (-r), filtered by since and createdByNexus, sorted by createdAt
// descending then name, and paged by that key. A listing over the 4 MiB
// cap answers 503, never a partial list (A-39).
func (p *Provider) snapshotsList(ctx context.Context, in spec.SnapshotListInput) (apitypes.List[spec.Snapshot], error) {
	out := apitypes.List[spec.Snapshot]{Items: []spec.Snapshot{}}
	if in.Dataset != "" && !validDatasetName(in.Dataset) {
		return out, invalidInput("/dataset", "is not a dataset name")
	}
	var since time.Time
	if in.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, in.Since)
		if err != nil || !sinceRE.MatchString(in.Since) {
			return out, invalidInput("/since", "is not an RFC 3339 time")
		}
		since = t
	}
	switch in.CreatedByNexus {
	case "", spec.CreatedTrue, spec.CreatedFalse:
	default:
		return out, invalidInput("/createdByNexus", "must be true or false")
	}
	limit, err := pageLimit(in.Limit)
	if err != nil {
		return out, err
	}
	afterKey, err := decodeCursor(in.Cursor, func(k string) bool { _, _, ok := splitSnapshotKey(k); return ok })
	if err != nil {
		return out, err
	}
	z, err := p.zfsReady(ctx)
	if err != nil {
		return out, err
	}

	var argv []string
	switch {
	case in.Dataset == "":
		argv = zfs.Argv(z.paths, zfs.Text, zfs.CmdSnapshotList)
	case in.Recursive:
		argv = zfs.Argv(z.paths, zfs.Text, zfs.CmdSnapshotListRecursive, in.Dataset)
	default:
		argv = zfs.Argv(z.paths, zfs.Text, zfs.CmdSnapshotList, in.Dataset)
	}
	stdout, err := p.runZFS(ctx, z, snapshotWhat, argv)
	if err != nil {
		return out, opError(err)
	}
	snaps, err := zfs.ParseSnapshotsText(stdout)
	if err != nil {
		return out, opError(&readError{reason: boundReason(fmt.Sprintf("%s output could not be parsed: %v", snapshotWhat, err))})
	}

	items := make([]spec.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		nexus := createdByNexusRE.MatchString(s.CreatedBy)
		if in.Since != "" && s.CreatedAt.Before(since) ||
			in.CreatedByNexus == spec.CreatedTrue && !nexus || in.CreatedByNexus == spec.CreatedFalse && nexus {
			continue
		}
		items = append(items, resource(p, spec.SnapshotsType, s.Name, spec.SnapshotProperties{
			Dataset: s.Dataset, CreatedAt: s.CreatedAt, UsedBytes: s.Used, ReferencedBytes: s.Refer, Holds: s.Holds,
			CreatedByNexus: nexus, Unavailable: s.Unavailable,
		}))
	}
	slices.SortFunc(items, compareSnapshots)

	atOrBefore := func(spec.Snapshot) bool { return false }
	if afterKey != "" {
		at, name, _ := splitSnapshotKey(afterKey)
		atOrBefore = func(s spec.Snapshot) bool {
			c := s.Properties.CreatedAt.Unix()
			return c > at || c == at && s.Name <= name
		}
	}
	page, next := paginate(items, limit, atOrBefore, snapshotKey)
	out.Items = append(out.Items, page...)
	out.NextCursor = next
	return out, nil
}

// compareSnapshots orders snapshots by createdAt descending, then by name.
func compareSnapshots(a, b spec.Snapshot) int {
	if c := b.Properties.CreatedAt.Compare(a.Properties.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// snapshotKey is a snapshot's sort key in a cursor: its creation time in
// epoch seconds (the listing's own resolution), a space, and its name.
func snapshotKey(s spec.Snapshot) string {
	return strconv.FormatInt(s.Properties.CreatedAt.Unix(), 10) + " " + s.Name
}

// splitSnapshotKey reads a snapshotKey; ok is false for anything else.
func splitSnapshotKey(k string) (int64, string, bool) {
	sec, name, ok := strings.Cut(k, " ")
	if !ok || sec == "" || strings.Trim(sec, "0123456789") != "" {
		return 0, "", false
	}
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || len(name) > spec.MaxSnapshotNameBytes || !listedSnapshotRE.MatchString(name) {
		return 0, "", false
	}
	return n, name, true
}
