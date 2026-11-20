// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
)

// PidStatInfo is what the host provider reads from /proc/<pid>/stat
// (proc_pid_stat(5)): the pid, comm, state, utime and stime (field 14 and 15,
// clock ticks), starttime (22, clock ticks after boot) and rss (24, pages).
type PidStatInfo struct {
	PID       int
	Comm      string
	State     string
	UTime     uint64
	STime     uint64
	StartTime uint64
	RSSPages  int64
}

// PidStat parses /proc/<pid>/stat. comm is printed unescaped between
// parentheses (fs/proc/array.c do_task_stat) and a process can set it to
// anything, ")" and spaces included, so it is the text between the first
// "(" and the last ")", and the numbered fields are counted after that ")".
// The whole record is the anchor: any malformed field is an error.
func PidStat(b []byte) (PidStatInfo, error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return PidStatInfo{}, errors.New("pid stat: no (comm)")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b[:open])))
	if err != nil || pid < 0 {
		return PidStatInfo{}, errors.New("pid stat: bad pid")
	}
	// f[0] is field 3 (state), so field n is f[n-3].
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 24-3+1 {
		return PidStatInfo{}, errors.New("pid stat: too few fields")
	}
	utime, ok1 := parseU64(f[14-3])
	stime, ok2 := parseU64(f[15-3])
	start, ok3 := parseU64(f[22-3])
	rss, err := strconv.ParseInt(f[24-3], 10, 64)
	if !ok1 || !ok2 || !ok3 || err != nil {
		return PidStatInfo{}, errors.New("pid stat: malformed field")
	}
	return PidStatInfo{PID: pid, Comm: string(b[open+1 : end]), State: f[0],
		UTime: utime, STime: stime, StartTime: start, RSSPages: rss}, nil
}

// PidStatusUID returns the real uid of /proc/<pid>/status: the first of the
// Uid: line's real, effective, saved and filesystem uids
// (proc_pid_status(5)).
func PidStatusUID(b []byte) (uint32, error) {
	for l := range lines(b) {
		rest, ok := strings.CutPrefix(l, "Uid:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if uid, err := strconv.ParseUint(f[0], 10, 32); err == nil {
			return uint32(uid), nil
		}
	}
	return 0, errors.New("pid status: no Uid line")
}
