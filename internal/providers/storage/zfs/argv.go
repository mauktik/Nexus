// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package zfs

import (
	"strings"

	"github.com/mauktik/nexus/internal/providers/storage/spec"
)

// Command names one argv shape Argv builds (DF-1, DF-2, DF-7, DF-8).
type Command int

const (
	// CmdVersion is `zfs version` (DF-1).
	CmdVersion Command = iota + 1
	// CmdPoolNames is `zpool list -H -o name` (DF-1, and pools.get's 404).
	CmdPoolNames
	// CmdPoolList is `zpool list [NAME]` with `-o PoolListColumns` in both
	// formats (A-58).
	CmdPoolList
	// CmdPoolStatus is `zpool status [NAME]`.
	CmdPoolStatus
	// CmdPoolStatusVerbose is `zpool status -v [NAME]`.
	CmdPoolStatusVerbose
	// CmdPoolGet is `zpool get all NAME`.
	CmdPoolGet
	// CmdDatasetList is `zfs list -t filesystem,volume -o DatasetColumns [NAME]`.
	CmdDatasetList
	// CmdSnapshotTimes is `zfs list -H -p -t snapshot -o name,creation
	// [-d 1 NAME]` (the dataset reads' snapshotCount and newestSnapshotAt).
	CmdSnapshotTimes
	// CmdSnapshotList is `zfs list -H -p -t snapshot -o SnapshotColumns
	// [-d 1 DATASET]` (snapshots.list).
	CmdSnapshotList
	// CmdSnapshotListRecursive is the same with `-r DATASET`.
	CmdSnapshotListRecursive
	// CmdSnapshotGet is the same naming one DATASET@SNAP (the helper's read
	// of the snapshot it created, DF-8 step 4).
	CmdSnapshotGet
	// CmdScrubStart is `zpool scrub NAME` (DF-7).
	CmdScrubStart
	// CmdScrubStop is `zpool scrub -s NAME` (DF-7).
	CmdScrubStop
	// CmdSnapshotCreate is `zfs snapshot -o nexus:created-by=ID DATASET@SNAP`
	// with args ID, DATASET@SNAP (DF-8).
	CmdSnapshotCreate
	// CmdSnapshotCreateRecursive is the same with -r.
	CmdSnapshotCreateRecursive
)

var (
	jsonFlags = []string{"-j", "--json-int"}
	textFlags = []string{"-H", "-p"}
)

// Argv builds the argv of cmd for the tools at p in format f: the one place
// argv is built, so the provider, the helper and the capture-replaying fake
// use the same argv. JSON and text forms request the same -o columns.
// Snapshot listings are the -H -p text form in both formats (D-5 exception).
// args are the pool, dataset or snapshot names (and the operation ID of a
// snapshot create) and are always the last elements. Argv returns nil for a
// wrong number of args, or an empty arg or one starting with "-", so a name
// can never be read as an option.
func Argv(p Paths, f Format, cmd Command, args ...string) []string {
	for _, a := range args {
		if a == "" || strings.HasPrefix(a, "-") {
			return nil
		}
	}
	optional := func(base []string) []string {
		if len(args) > 1 {
			return nil
		}
		return append(base, args...)
	}
	one := func(base []string) []string {
		if len(args) != 1 {
			return nil
		}
		return append(base, args[0])
	}
	form := func(json, text []string) []string {
		if f == JSON {
			return json
		}
		return text
	}
	zpool := func(a ...string) []string { return append([]string{p.Zpool}, a...) }
	zfs := func(a ...string) []string { return append([]string{p.Zfs}, a...) }
	cat := func(parts ...[]string) []string {
		var out []string
		for _, x := range parts {
			out = append(out, x...)
		}
		return out
	}
	snapshots := func(cols string) []string {
		return zfs(cat([]string{"list"}, textFlags, []string{"-t", "snapshot", "-o", cols})...)
	}

	switch cmd {
	case CmdVersion:
		if len(args) != 0 {
			return nil
		}
		return zfs("version")
	case CmdPoolNames:
		if len(args) != 0 {
			return nil
		}
		return zpool("list", "-H", "-o", "name")
	case CmdPoolList:
		// zpool list -j prints only its default columns, which lack readonly,
		// unless -o is given, so both forms request PoolListColumns (A-58).
		return optional(zpool(cat([]string{"list"}, form(jsonFlags, textFlags), []string{"-o", PoolListColumns})...))
	case CmdPoolStatus:
		return optional(zpool(cat([]string{"status"}, form(jsonFlags, []string{"-p"}))...))
	case CmdPoolStatusVerbose:
		return optional(zpool(cat([]string{"status"}, form(jsonFlags, []string{"-p"}), []string{"-v"})...))
	case CmdPoolGet:
		return one(zpool(cat([]string{"get"}, form(jsonFlags, cat(textFlags, []string{"-o", PoolGetColumns})), []string{"all"})...))
	case CmdDatasetList:
		return optional(zfs(cat([]string{"list"}, form(jsonFlags, textFlags), []string{"-t", "filesystem,volume", "-o", DatasetColumns})...))
	case CmdSnapshotTimes:
		if len(args) == 1 {
			return one(append(snapshots(SnapshotTimeColumns), "-d", "1"))
		}
		return optional(snapshots(SnapshotTimeColumns))
	case CmdSnapshotList:
		if len(args) == 1 {
			return one(append(snapshots(SnapshotColumns), "-d", "1"))
		}
		return optional(snapshots(SnapshotColumns))
	case CmdSnapshotListRecursive:
		return one(append(snapshots(SnapshotColumns), "-r"))
	case CmdSnapshotGet:
		if len(args) != 1 || !strings.Contains(args[0], "@") {
			return nil
		}
		return one(snapshots(SnapshotColumns))
	case CmdScrubStart:
		return one(zpool("scrub"))
	case CmdScrubStop:
		return one(zpool("scrub", "-s"))
	case CmdSnapshotCreate, CmdSnapshotCreateRecursive:
		if len(args) != 2 || !strings.Contains(args[1], "@") {
			return nil
		}
		a := zfs("snapshot", "-o", spec.CreatedByProperty+"="+args[0])
		if cmd == CmdSnapshotCreateRecursive {
			a = append(a, "-r")
		}
		return append(a, args[1])
	}
	return nil
}
