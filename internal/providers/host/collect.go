// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"time"

	"github.com/mauktik/nexus/internal/providers/host/hostparse"
)

// The counter sources the sampler reads on each tick.
const (
	pathStat      = "proc/stat"
	pathDiskstats = "proc/diskstats"
	pathNetDev    = "proc/net/dev"
	pathMeminfo   = "proc/meminfo"
	pathLoadavg   = "proc/loadavg"
	pathProc      = "proc"
)

// sample is one reading of the counters the sampler derives rates from. A
// nil source (cpu, disks, ifaces, mem, load) was not read, and its why is
// the reason; an entity absent from a read source did not exist then.
type sample struct {
	at time.Time

	cpu    *hostparse.Stat
	cpuWhy string

	disks    map[string]hostparse.DiskStat
	disksWhy string

	ifaces    map[string]ifaceSample
	ifacesWhy string

	mem     map[string]uint64
	memWhy  string
	load    *hostparse.LoadAverage
	loadWhy string

	// procs is the newest process scan and procBase the one before it,
	// both carried from an earlier sample on a tick that does not scan, so
	// per-process rates always cover two consecutive scans.
	procs, procBase *procScan
	// scanned is whether this sample made procs; scanCost is what that
	// scan took.
	scanned  bool
	scanCost time.Duration
}

// ifaceSample is one interface of /proc/net/dev with the identity it had.
type ifaceSample struct {
	stat    hostparse.NetDevStat
	ifindex int64 // 0 when sys/class/net/<if>/ifindex could not be read
	device  bool  // sys/class/net/<if>/device exists: a physical NIC
}

// procScan is one read of every /proc/<pid>/stat. A failed scan has a why
// and no pids.
type procScan struct {
	at   time.Time
	pids map[int]hostparse.PidStatInfo
	why  string
}

// readSample reads the counters at now, and every /proc/<pid>/stat when
// scanProcs is set, timing the scan with clock. Each source that fails
// records its own reason and leaves the others untouched (FR-12). It reads
// no file outside the allow-list and holds no lock.
func readSample(src *source, clock func() time.Time, now time.Time, scanProcs bool) sample {
	s := sample{at: now}

	if b, err := src.read(pathStat); err != nil {
		s.cpuWhy = reasonFor(pathStat, err)
	} else if st, err := hostparse.ProcStat(b); err != nil {
		s.cpuWhy = reasonFor(pathStat, err)
	} else {
		s.cpu = &st
	}

	if b, err := src.read(pathDiskstats); err != nil {
		s.disksWhy = reasonFor(pathDiskstats, err)
	} else if ds, err := hostparse.Diskstats(b); err != nil {
		s.disksWhy = reasonFor(pathDiskstats, err)
	} else {
		s.disks = make(map[string]hostparse.DiskStat, len(ds))
		for _, d := range ds {
			s.disks[d.Name] = d
		}
	}

	if b, err := src.read(pathNetDev); err != nil {
		s.ifacesWhy = reasonFor(pathNetDev, err)
	} else if nd, err := hostparse.NetDev(b); err != nil {
		s.ifacesWhy = reasonFor(pathNetDev, err)
	} else {
		s.ifaces = make(map[string]ifaceSample, len(nd))
		for _, n := range nd {
			s.ifaces[n.Name] = readIface(src, n)
		}
	}

	if b, err := src.read(pathMeminfo); err != nil {
		s.memWhy = reasonFor(pathMeminfo, err)
	} else if m, err := hostparse.Meminfo(b); err != nil {
		s.memWhy = reasonFor(pathMeminfo, err)
	} else {
		s.mem = m
	}

	if b, err := src.read(pathLoadavg); err != nil {
		s.loadWhy = reasonFor(pathLoadavg, err)
	} else if l, err := hostparse.Loadavg(b); err != nil {
		s.loadWhy = reasonFor(pathLoadavg, err)
	} else {
		s.load = &l
	}

	if scanProcs {
		start := clock()
		s.procs = scanProcesses(src, start)
		s.scanned = true
		s.scanCost = clock().Sub(start)
	}
	return s
}

// readIface adds an interface's ifindex and whether it is a device. Either
// failing leaves the zero value: an unknown ifindex never signals a reset.
func readIface(src *source, n hostparse.NetDevStat) ifaceSample {
	is := ifaceSample{stat: n}
	if name, err := segPath(segName, "sys/class/net", n.Name, "ifindex"); err == nil {
		if b, err := src.read(name); err == nil {
			if v, err := hostparse.ScalarInt(b); err == nil {
				is.ifindex = v
			}
		}
	}
	if name, err := segPath(segName, "sys/class/net", n.Name, "device"); err == nil {
		_, err := src.stat(name)
		is.device = err == nil
	}
	return is
}

// scanProcesses reads /proc/<pid>/stat for every pid listed in /proc. A
// process that exits between the listing and its read, or whose stat cannot
// be read or parsed, is left out.
func scanProcesses(src *source, at time.Time) *procScan {
	ps := &procScan{at: at}
	entries, err := src.readDir(pathProc)
	if err != nil {
		ps.why = reasonFor(pathProc, err)
		return ps
	}
	ps.pids = make(map[int]hostparse.PidStatInfo, len(entries))
	for _, e := range entries {
		if !allDigits(e.Name()) {
			continue
		}
		name, err := segPath(segPID, pathProc, e.Name(), "stat")
		if err != nil {
			continue
		}
		b, err := src.read(name)
		if err != nil {
			continue
		}
		info, err := hostparse.PidStat(b)
		if err != nil {
			continue
		}
		ps.pids[info.PID] = info
	}
	return ps
}
