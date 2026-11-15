package host

import (
	"cmp"
	"slices"
	"strconv"
	"time"

	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// userHZ is the unit of the /proc/stat and /proc/<pid>/stat tick counters:
// USER_HZ, 100 on every architecture Linux exports to user space (A-7).
const userHZ = 100

// rateVal is one computed value, or the reason there is none. A missing
// value is never 0 (FR-12).
type rateVal struct {
	v   *float64
	why string
}

func have(x float64) rateVal     { return rateVal{v: &x} }
func missing(why string) rateVal { return rateVal{why: why} }

// cpuRates is the busy share of one CPU line and its split, in percent of
// that line's ticks over the interval (A-6).
type cpuRates struct {
	usage, user, system, iowait, steal rateVal
}

// diskRates is one /proc/diskstats device over the interval. major and
// minor are its maj:min in the newer sample, so a reader can join on name
// and maj:min (WI 4.2).
type diskRates struct {
	readBps, writeBps, readIOPS, writeIOPS rateVal
	major, minor                           uint32
}

// ifaceRates is one interface over the interval.
type ifaceRates struct {
	rx, tx rateVal
}

// procRate is one process of a top list.
type procRate struct {
	pid        int
	comm       string
	cpuPercent float64 // of one CPU (A-7)
	rssBytes   uint64
	startTime  uint64 // /proc/<pid>/stat starttime, to tell a reused pid from the sampled process
}

// snapshot is one immutable result of the sampler, published with an
// atomic store and read by handlers without a lock. Its maps and slices
// are never written after publication.
type snapshot struct {
	seq      uint64        // publications in this run, from 1 (tests wait on it)
	at       time.Time     // when the newest sample was taken
	interval time.Duration // sampleInterval when it was taken
	elapsed  time.Duration // between the two samples the rates use; 0 while warming up

	cpu       cpuRates
	perCPU    map[int]cpuRates
	perCPUWhy string // the reason there is no per-CPU map

	disks    map[string]diskRates
	disksWhy string
	ifaces   map[string]ifaceRates
	ifaceWhy string

	topCPU, topMem []procRate
	procsWhy       string        // the reason the top lists are empty
	procInterval   time.Duration // between the two scans the top lists use
}

// compute derives a snapshot from the last good sample prev and the new
// sample cur, taken elapsed apart (A-6, A-7, A-9):
//   - a source or entity that prev lacks gives "warming up", never 0 and
//     never "counter reset:";
//   - a counter that decreased, an interface whose ifindex changed, a disk
//     whose maj:min changed or a pid whose starttime changed gives
//     "counter reset: <entity>" for that entity and interval, with no wrap
//     arithmetic;
//   - an entity absent from cur has no entry.
//
// pageSize converts rss pages to bytes.
func compute(prev, cur sample, elapsed time.Duration, pageSize int) *snapshot {
	snap := &snapshot{at: cur.at}
	warm := prev.at.IsZero() || elapsed <= 0
	if !warm {
		snap.elapsed = elapsed
	}
	secs := elapsed.Seconds()

	switch {
	case cur.cpu == nil:
		snap.cpu = cpuMissing(cur.cpuWhy)
		snap.perCPUWhy = cur.cpuWhy
	case warm || prev.cpu == nil:
		snap.cpu = cpuMissing(reasonWarmingUp)
		snap.perCPU = make(map[int]cpuRates, len(cur.cpu.CPUs))
		for _, c := range cur.cpu.CPUs {
			snap.perCPU[c.ID] = cpuMissing(reasonWarmingUp)
		}
	default:
		snap.cpu = cpuShare("cpu", prev.cpu.Total, cur.cpu.Total)
		was := make(map[int]hostparse.CPUTimes, len(prev.cpu.CPUs))
		for _, c := range prev.cpu.CPUs {
			was[c.ID] = c.Times
		}
		snap.perCPU = make(map[int]cpuRates, len(cur.cpu.CPUs))
		for _, c := range cur.cpu.CPUs {
			if p, ok := was[c.ID]; ok {
				snap.perCPU[c.ID] = cpuShare("cpu"+strconv.Itoa(c.ID), p, c.Times)
			} else {
				snap.perCPU[c.ID] = cpuMissing(reasonWarmingUp)
			}
		}
	}

	if cur.disks == nil {
		snap.disksWhy = cur.disksWhy
	} else {
		snap.disks = make(map[string]diskRates, len(cur.disks))
		for name, d := range cur.disks {
			p, ok := prev.disks[name]
			var r diskRates
			switch {
			case warm || !ok:
				w := missing(reasonWarmingUp)
				r = diskRates{readBps: w, writeBps: w, readIOPS: w, writeIOPS: w}
			case p.Major != d.Major || p.Minor != d.Minor:
				c := missing(reasonCounterReset(name))
				r = diskRates{readBps: c, writeBps: c, readIOPS: c, writeIOPS: c}
			default:
				r = diskRates{
					readBps:   perSecond(name, p.SectorsRead, d.SectorsRead, hostparse.SectorSize, secs),
					writeBps:  perSecond(name, p.SectorsWritten, d.SectorsWritten, hostparse.SectorSize, secs),
					readIOPS:  perSecond(name, p.ReadsCompleted, d.ReadsCompleted, 1, secs),
					writeIOPS: perSecond(name, p.WritesCompleted, d.WritesCompleted, 1, secs),
				}
			}
			r.major, r.minor = d.Major, d.Minor
			snap.disks[name] = r
		}
	}

	if cur.ifaces == nil {
		snap.ifaceWhy = cur.ifacesWhy
	} else {
		snap.ifaces = make(map[string]ifaceRates, len(cur.ifaces))
		for name, i := range cur.ifaces {
			p, ok := prev.ifaces[name]
			switch {
			case warm || !ok:
				w := missing(reasonWarmingUp)
				snap.ifaces[name] = ifaceRates{w, w}
			case p.ifindex != 0 && i.ifindex != 0 && p.ifindex != i.ifindex:
				r := missing(reasonCounterReset(name))
				snap.ifaces[name] = ifaceRates{r, r}
			default:
				snap.ifaces[name] = ifaceRates{
					rx: perSecond(name, p.stat.RxBytes, i.stat.RxBytes, 1, secs),
					tx: perSecond(name, p.stat.TxBytes, i.stat.TxBytes, 1, secs),
				}
			}
		}
	}

	snap.topCPU, snap.topMem, snap.procInterval, snap.procsWhy = processRates(cur.procBase, cur.procs, pageSize)
	return snap
}

func cpuMissing(why string) cpuRates {
	m := missing(why)
	return cpuRates{m, m, m, m, m}
}

// cpuShare is A-6: busy = Δtotal − Δidle − Δiowait over Δtotal, and each
// split column's delta over Δtotal, in percent. Any column that decreased
// is a reset.
func cpuShare(entity string, p, c hostparse.CPUTimes) cpuRates {
	pc := []uint64{p.User, p.Nice, p.System, p.Idle, p.IOWait, p.IRQ, p.SoftIRQ, p.Steal}
	cc := []uint64{c.User, c.Nice, c.System, c.Idle, c.IOWait, c.IRQ, c.SoftIRQ, c.Steal}
	for i := range pc {
		if cc[i] < pc[i] {
			return cpuMissing(reasonCounterReset(entity))
		}
	}
	total := float64(c.Total() - p.Total())
	if total == 0 {
		return cpuMissing(reasonNotReported(entity + " counted no time in the interval"))
	}
	share := func(a, b uint64) rateVal { return have(float64(b-a) / total * 100) }
	return cpuRates{
		usage:  share(p.Busy(), c.Busy()),
		user:   share(p.User, c.User),
		system: share(p.System, c.System),
		iowait: share(p.IOWait, c.IOWait),
		steal:  share(p.Steal, c.Steal),
	}
}

// perSecond is (b − a) × unit / secs, or a reset when the counter
// decreased.
func perSecond(entity string, a, b, unit uint64, secs float64) rateVal {
	if b < a {
		return missing(reasonCounterReset(entity))
	}
	return have(float64(b-a) * float64(unit) / secs)
}

// processRates ranks the pids present in both scans with the same
// starttime (A-7). A pid in only one scan, or reused (its starttime
// changed), or whose ticks decreased, is not ranked. CPU % is of one CPU:
// Δ(utime+stime) / (USER_HZ × elapsed s) × 100. The lists are the top spec.TopProcesses by
// CPU and by RSS, descending, ties by pid ascending.
func processRates(base, cur *procScan, pageSize int) (byCPU, byMem []procRate, interval time.Duration, why string) {
	switch {
	case cur == nil || base == nil:
		return nil, nil, 0, reasonWarmingUp
	case cur.why != "":
		return nil, nil, 0, cur.why
	case base.why != "":
		return nil, nil, 0, reasonWarmingUp
	}
	interval = cur.at.Sub(base.at)
	if interval <= 0 {
		return nil, nil, 0, reasonWarmingUp
	}
	secs := interval.Seconds()
	ranked := make([]procRate, 0, len(cur.pids))
	for pid, c := range cur.pids {
		p, ok := base.pids[pid]
		if !ok || p.StartTime != c.StartTime {
			continue
		}
		pt, ct := p.UTime+p.STime, c.UTime+c.STime
		if ct < pt {
			continue
		}
		ranked = append(ranked, procRate{
			pid:        pid,
			comm:       c.Comm,
			cpuPercent: float64(ct-pt) / (userHZ * secs) * 100,
			rssBytes:   uint64(max(c.RSSPages, 0)) * uint64(pageSize),
			startTime:  c.StartTime,
		})
	}
	byCPU = topBy(ranked, func(a, b procRate) int { return cmp.Compare(b.cpuPercent, a.cpuPercent) })
	byMem = topBy(ranked, func(a, b procRate) int { return cmp.Compare(b.rssBytes, a.rssBytes) })
	return byCPU, byMem, interval, ""
}

// topBy is the first spec.TopProcesses (FR-10) of ps ordered by order, ties by pid ascending,
// in a slice of its own.
func topBy(ps []procRate, order func(a, b procRate) int) []procRate {
	slices.SortFunc(ps, func(a, b procRate) int {
		if c := order(a, b); c != 0 {
			return c
		}
		return cmp.Compare(a.pid, b.pid)
	})
	return slices.Clone(ps[:min(spec.TopProcesses, len(ps))])
}
