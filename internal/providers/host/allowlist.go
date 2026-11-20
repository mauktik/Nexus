// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"io/fs"
	"strings"
)

// access is the set of operations an allow-list entry permits (D-3, review
// Finding 1). A Stat is permitted by an entry of any kind.
type access uint8

const (
	// accRead permits Open (and so fs.ReadFile) and Stat.
	accRead access = 1 << iota
	// accList permits ReadDir and Stat, and refuses Open.
	accList
	// accStatOnly permits Stat only: existence and type, never content.
	accStatOnly
	// accLink permits ReadLink and Stat.
	accLink

	accAny = accRead | accList | accStatOnly | accLink
)

func (a access) String() string {
	switch a {
	case accRead:
		return "read"
	case accList:
		return "list"
	case accStatOnly:
		return "stat-only"
	case accLink:
		return "link"
	case accAny:
		return "stat"
	}
	var names []string
	for _, k := range []access{accRead, accList, accStatOnly, accLink} {
		if a&k != 0 {
			names = append(names, k.String())
		}
	}
	return strings.Join(names, "+")
}

// errNotAllowed is the error of an access the allow-list refuses. It is a
// programming bug, never a host condition: reasonFor maps it to
// "internal error: access not allow-listed", and TC-HP-102 keeps it from
// shipping.
var errNotAllowed = errors.New("access not allow-listed")

// Pattern segments. A segment holds at most one wildcard:
//   - "*" matches one or more characters of a valid name;
//   - "<n>" matches one or more ASCII digits (pids, CPU numbers).
//
// Every other character matches itself. A name never matches across "/".
const (
	wildName   = "*"
	wildDigits = "<n>"
)

// rebootMarkerWriters mirror apt.RebootMarkerWriters, written relative to
// the root (WI 4.4). A test asserts that the two lists agree.
var rebootMarkerWriters = []string{
	"usr/share/update-notifier/notify-reboot-required",
	"usr/share/perl5/NeedRestart/UI/Ubuntu.pm",
}

// allowList is every path the host provider may touch, each with its access
// kinds (WI 2.2). No mount point is ever a name here: mount points reach the
// kernel only through the Statfs seam (D-3, D-9).
//
// The OS release file is deliberately absent: the provider takes the OS
// from internal/platform, the single sanctioned reader of that file
// (TestNoReleaseSpecificLogic in internal/providers/apt).
var allowList = buildAllowList(
	// /proc (3.1, 3.3, 4.1-4.4).
	allow(accRead,
		"proc/stat", "proc/meminfo", "proc/loadavg", "proc/uptime", "proc/vmstat",
		"proc/diskstats", "proc/cpuinfo",
		"proc/self/mountinfo", "proc/1/mountinfo",
		"proc/net/dev", "proc/net/route", "proc/net/ipv6_route",
		"proc/pressure/cpu", "proc/pressure/memory", "proc/pressure/io",
		"proc/sys/kernel/hostname", "proc/sys/kernel/osrelease",
		"proc/<n>/stat", "proc/<n>/status",
	),
	// Network interface attributes (3.3, 4.3).
	allow(accRead,
		"sys/class/net/*/operstate", "sys/class/net/*/address", "sys/class/net/*/mtu",
		"sys/class/net/*/speed", "sys/class/net/*/type", "sys/class/net/*/ifindex",
	),
	// Block devices and partitions (4.2).
	allow(accRead,
		"sys/block/*/size", "sys/block/*/removable", "sys/block/*/dev",
		"sys/block/*/queue/rotational", "sys/block/*/device/model",
		"sys/block/*/*/size", "sys/block/*/*/dev", "sys/block/*/*/partition",
	),
	// Sensors (4.1).
	allow(accRead,
		"sys/class/hwmon/*/name", "sys/class/hwmon/*/temp*_input", "sys/class/hwmon/*/temp*_label",
		"sys/class/thermal/thermal_zone*/type", "sys/class/thermal/thermal_zone*/temp",
	),
	// CPU frequency and topology (4.1).
	allow(accRead,
		"sys/devices/system/cpu/cpu<n>/cpufreq/scaling_cur_freq",
		"sys/devices/system/cpu/cpu<n>/topology/core_id",
		"sys/devices/system/cpu/cpu<n>/topology/physical_package_id",
	),
	// DMI without serials, UUIDs or asset tags, and the hypervisor (2.4, 4.1).
	allow(accRead,
		"sys/class/dmi/id/sys_vendor", "sys/class/dmi/id/product_name",
		"sys/class/dmi/id/product_version", "sys/class/dmi/id/bios_vendor",
		"sys/class/dmi/id/bios_version", "sys/hypervisor/type",
	),
	// Configuration and markers with content (2.4, 4.1, 4.3, 4.4).
	allow(accRead,
		"etc/timezone", "etc/localtime", "etc/resolv.conf", "etc/passwd",
		"run/systemd/resolve/resolv.conf", "run/systemd/container",
		"var/run/reboot-required.pkgs",
	),
	allow(accLink, "etc/localtime"),
	allow(accList,
		"proc",
		"sys/class/net", "sys/block", "sys/block/*",
		"sys/class/hwmon", "sys/class/hwmon/*", "sys/class/thermal",
	),
	allow(accStatOnly,
		"sys/class/net/*/device",
		// Whether a loop device is backed, never what it is backed by: the
		// content is a host path (SEC-2).
		"sys/block/*/loop/backing_file",
		"run/.containerenv", ".dockerenv", "proc/vz", "proc/bc",
		"var/run/reboot-required", "lib/modules/*",
		"usr/bin/chronyc", "run/systemd/timesync", "run/chrony/chronyd.pid", "run/ntpd.pid",
	),
	allow(accStatOnly, rebootMarkerWriters...),
)

type allowEntry struct {
	segs  []string
	kinds access
}

type allowGroup struct {
	kinds    access
	patterns []string
}

func allow(kinds access, patterns ...string) allowGroup {
	return allowGroup{kinds: kinds, patterns: patterns}
}

// buildAllowList merges the groups, so a pattern listed under two kinds
// (etc/localtime: read and link) is one entry with both.
func buildAllowList(groups ...allowGroup) []allowEntry {
	var out []allowEntry
	index := map[string]int{}
	for _, g := range groups {
		for _, p := range g.patterns {
			if !fs.ValidPath(p) || p == "." {
				panic("host: invalid allow-list pattern " + p)
			}
			segs := strings.Split(p, "/")
			for _, s := range segs {
				if strings.Count(s, wildName)+strings.Count(s, wildDigits) > 1 {
					panic("host: allow-list pattern with two wildcards in a segment: " + p)
				}
			}
			if i, ok := index[p]; ok {
				out[i].kinds |= g.kinds
				continue
			}
			index[p] = len(out)
			out = append(out, allowEntry{segs: segs, kinds: g.kinds})
		}
	}
	return out
}

// allowedKinds returns the access kinds the allow-list grants name, or 0.
// A name that is not a valid fs.FS path (absolute, "..", ".", empty
// elements) is granted nothing.
func allowedKinds(name string) access {
	if !fs.ValidPath(name) || name == "." {
		return 0
	}
	segs := strings.Split(name, "/")
	for _, s := range segs {
		if !validName(s) {
			return 0
		}
	}
	var kinds access
	for _, e := range allowList {
		if matchSegs(e.segs, segs) {
			kinds |= e.kinds
		}
	}
	return kinds
}

// permits reports whether name may be accessed with an operation needing
// one of want's kinds.
func permits(name string, want access) bool {
	return allowedKinds(name)&want != 0
}

func matchSegs(pattern, name []string) bool {
	if len(pattern) != len(name) {
		return false
	}
	for i, p := range pattern {
		if !matchSeg(p, name[i]) {
			return false
		}
	}
	return true
}

func matchSeg(pattern, name string) bool {
	for _, wild := range []string{wildName, wildDigits} {
		pre, suf, ok := strings.Cut(pattern, wild)
		if !ok {
			continue
		}
		if len(name) <= len(pre)+len(suf) || !strings.HasPrefix(name, pre) || !strings.HasSuffix(name, suf) {
			return false
		}
		mid := name[len(pre) : len(name)-len(suf)]
		return wild == wildName || allDigits(mid)
	}
	return pattern == name
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// validName reports whether s can be one path element: non-empty, not "."
// or "..", and free of "/", NUL and other control characters. fs.ValidPath
// accepts NUL, which the kernel would treat as the end of the path.
func validName(s string) bool {
	if s == "" || s == "." || s == ".." || !fs.ValidPath(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f || c == '/' {
			return false
		}
	}
	return true
}
