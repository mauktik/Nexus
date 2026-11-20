// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"errors"
	"strconv"
	"strings"
)

// SectorSize is the unit of the diskstats sector counters, whatever the
// device's own sector size.
const SectorSize = 512

// DiskStat is one line of /proc/diskstats (Documentation/ABI/testing/
// procfs-diskstats): fields 1-3, 4 (reads completed), 6 (sectors read), 8
// (writes completed) and 10 (sectors written).
type DiskStat struct {
	Major, Minor    uint32
	Name            string
	ReadsCompleted  uint64
	SectorsRead     uint64
	WritesCompleted uint64
	SectorsWritten  uint64
}

// BytesRead is SectorsRead in bytes.
func (d DiskStat) BytesRead() uint64 { return d.SectorsRead * SectorSize }

// BytesWritten is SectorsWritten in bytes.
func (d DiskStat) BytesWritten() uint64 { return d.SectorsWritten * SectorSize }

// Diskstats parses /proc/diskstats. A line carries 14 fields, 18 since Linux
// 4.18 (discards) or 20 since 5.5 (flushes); any line of at least 14 fields
// is read, so a later kernel's new columns are ignored. At least one device
// is the required anchor.
func Diskstats(b []byte) ([]DiskStat, error) {
	var out []DiskStat
	for l := range lines(b) {
		f := strings.Fields(l)
		if len(f) < 14 {
			continue
		}
		maj, err1 := strconv.ParseUint(f[0], 10, 32)
		mn, err2 := strconv.ParseUint(f[1], 10, 32)
		rc, ok1 := parseU64(f[3])
		sr, ok2 := parseU64(f[5])
		wc, ok3 := parseU64(f[7])
		sw, ok4 := parseU64(f[9])
		if err1 != nil || err2 != nil || !ok1 || !ok2 || !ok3 || !ok4 {
			continue
		}
		out = append(out, DiskStat{Major: uint32(maj), Minor: uint32(mn), Name: f[2],
			ReadsCompleted: rc, SectorsRead: sr, WritesCompleted: wc, SectorsWritten: sw})
	}
	if len(out) == 0 {
		return nil, errors.New("proc/diskstats: no device line")
	}
	return out, nil
}
