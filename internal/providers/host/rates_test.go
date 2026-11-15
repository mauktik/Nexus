package host

import (
	"slices"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/host/hostparse"
)

var t0 = time.Date(2026, 11, 15, 2, 37, 33, 0, time.UTC)

func cpuSample(at time.Time, total hostparse.CPUTimes, cpus ...hostparse.CPUTimes) sample {
	st := hostparse.Stat{Total: total}
	for i, c := range cpus {
		st.CPUs = append(st.CPUs, hostparse.PerCPU{ID: i, Times: c})
	}
	return sample{at: at, cpu: &st}
}

func TestComputeCPUShares(t *testing.T) {
	p := hostparse.CPUTimes{User: 100, Nice: 10, System: 50, Idle: 1000, IOWait: 20, IRQ: 5, SoftIRQ: 5, Steal: 10}
	// Δ: user 30, nice 0, system 10, idle 40, iowait 10, irq 0, softirq 0, steal 10 = 100
	c := hostparse.CPUTimes{User: 130, Nice: 10, System: 60, Idle: 1040, IOWait: 30, IRQ: 5, SoftIRQ: 5, Steal: 20}
	snap := compute(cpuSample(t0, p, p), cpuSample(t0.Add(10*time.Second), c, c), 10*time.Second, 4096)
	for _, x := range []struct {
		what string
		got  rateVal
		want float64
	}{
		{"usage", snap.cpu.usage, 50}, // 100 − 40 idle − 10 iowait
		{"user", snap.cpu.user, 30},
		{"system", snap.cpu.system, 10},
		{"iowait", snap.cpu.iowait, 10},
		{"steal", snap.cpu.steal, 10},
		{"cpu0 usage", snap.perCPU[0].usage, 50},
	} {
		if x.got.v == nil || *x.got.v != x.want {
			t.Errorf("%s = %+v, want %v", x.what, x.got, x.want)
		}
	}
}

// Any decreasing column is a reset of that line only, never a negative or
// wrapped share (A-9).
func TestComputeCPUColumnDecreaseIsAReset(t *testing.T) {
	p := hostparse.CPUTimes{User: 100, Idle: 1000, IOWait: 50}
	c := hostparse.CPUTimes{User: 110, Idle: 1090, IOWait: 49}
	ok := hostparse.CPUTimes{User: 110, Idle: 1090, IOWait: 50}
	snap := compute(cpuSample(t0, p, p, p), cpuSample(t0.Add(time.Second), ok, c, ok), time.Second, 4096)
	if snap.perCPU[0].usage.why != "counter reset: cpu0" || snap.perCPU[0].usage.v != nil {
		t.Errorf("cpu0 = %+v, want counter reset: cpu0", snap.perCPU[0].usage)
	}
	if v := snap.perCPU[1].usage.v; v == nil || *v != 10 {
		t.Errorf("cpu1 = %+v, want 10", snap.perCPU[1].usage)
	}
	if v := snap.cpu.usage.v; v == nil || *v != 10 {
		t.Errorf("total = %+v, want 10", snap.cpu.usage)
	}
}

// A CPU that came online since the previous sample is warming up; a line
// with no ticks reports why instead of dividing by zero.
func TestComputeNewAndIdleCPU(t *testing.T) {
	p := hostparse.CPUTimes{User: 100, Idle: 1000}
	c := hostparse.CPUTimes{User: 110, Idle: 1090}
	snap := compute(cpuSample(t0, c, p), cpuSample(t0.Add(time.Second), c, c, c), time.Second, 4096)
	if snap.perCPU[1].usage.why != reasonWarmingUp {
		t.Errorf("new cpu1 = %+v, want %q", snap.perCPU[1].usage, reasonWarmingUp)
	}
	if snap.cpu.usage.v != nil || snap.cpu.usage.why != "not reported: cpu counted no time in the interval" {
		t.Errorf("idle total = %+v, want not reported", snap.cpu.usage)
	}
}

// A source that failed in the previous sample, or a zero or negative
// elapsed time, gives warming up, never 0.
func TestComputeWithoutABaseIsWarmingUp(t *testing.T) {
	disks := map[string]hostparse.DiskStat{"vda": {Major: 253, Name: "vda", SectorsRead: 10}}
	prev := sample{at: t0, disksWhy: "permission denied: /proc/diskstats"}
	cur := sample{at: t0.Add(time.Second), disks: disks}
	snap := compute(prev, cur, time.Second, 4096)
	if d := snap.disks["vda"].readBps; d.v != nil || d.why != reasonWarmingUp {
		t.Errorf("vda after a failed base = %+v, want %q", d, reasonWarmingUp)
	}
	if snap.elapsed != time.Second {
		t.Errorf("elapsed = %v, want 1s", snap.elapsed)
	}

	snap = compute(sample{at: t0, disks: disks}, sample{at: t0, disks: disks}, 0, 4096)
	if d := snap.disks["vda"].readBps; d.v != nil || d.why != reasonWarmingUp || snap.elapsed != 0 {
		t.Errorf("zero elapsed: vda = %+v, elapsed %v; want %q and 0", d, snap.elapsed, reasonWarmingUp)
	}
}

func pidInfo(pid int, comm string, ticks, start uint64, rss int64) hostparse.PidStatInfo {
	return hostparse.PidStatInfo{PID: pid, Comm: comm, UTime: ticks, StartTime: start, RSSPages: rss}
}

func scanOf(at time.Time, ps ...hostparse.PidStatInfo) *procScan {
	s := &procScan{at: at, pids: map[int]hostparse.PidStatInfo{}}
	for _, p := range ps {
		s.pids[p.PID] = p
	}
	return s
}

func TestProcessRatesRanking(t *testing.T) {
	base := scanOf(t0,
		pidInfo(1, "a", 0, 10, 100),
		pidInfo(2, "b", 0, 10, 300),
		pidInfo(3, "c", 0, 10, 300),
		pidInfo(4, "d", 0, 10, 50),
		pidInfo(5, "reused", 0, 10, 9000),
		pidInfo(6, "back", 500, 10, 10),
		pidInfo(7, "e", 0, 10, 1),
		pidInfo(8, "f", 0, 10, -1),
	)
	cur := scanOf(t0.Add(20*time.Second),
		pidInfo(1, "a", 400, 10, 100),      // 400 ticks / 20 s = 20 %
		pidInfo(2, "b", 400, 10, 300),      // ties pid 1 on CPU
		pidInfo(3, "c", 4000, 10, 300),     // 200 %: more than one CPU
		pidInfo(4, "d", 20, 10, 50),        // 1 %
		pidInfo(5, "reused", 9999, 99, 99), // new starttime: not ranked
		pidInfo(6, "back", 100, 10, 10),    // ticks decreased: not ranked
		pidInfo(7, "e", 0, 10, 1),
		pidInfo(8, "f", 0, 10, -1), // negative rss is 0 bytes
		pidInfo(9, "new", 9999, 10, 99999),
	)
	byCPU, byMem, interval, why := processRates(base, cur, 4096)
	if why != "" || interval != 20*time.Second {
		t.Fatalf("interval %v, why %q", interval, why)
	}
	if got, want := pidsOf(byCPU), []int{3, 1, 2, 4, 7}; !slices.Equal(got, want) {
		t.Errorf("byCPU = %v, want %v", got, want)
	}
	if byCPU[0].cpuPercent != 200 || byCPU[1].cpuPercent != 20 || byCPU[3].cpuPercent != 1 {
		t.Errorf("cpu percents = %+v", byCPU)
	}
	if got, want := pidsOf(byMem), []int{2, 3, 1, 4, 7}; !slices.Equal(got, want) {
		t.Errorf("byMem = %v, want %v", got, want)
	}
	if byMem[0].rssBytes != 300*4096 {
		t.Errorf("rss = %d, want %d", byMem[0].rssBytes, 300*4096)
	}

	for _, c := range []struct {
		name      string
		base, cur *procScan
		why       string
	}{
		{"no scan yet", nil, nil, reasonWarmingUp},
		{"one scan", nil, cur, reasonWarmingUp},
		{"failed scan", base, &procScan{at: t0, why: "permission denied: /proc"}, "permission denied: /proc"},
		{"failed base", &procScan{at: t0, why: "permission denied: /proc"}, cur, reasonWarmingUp},
	} {
		if a, b, _, why := processRates(c.base, c.cur, 4096); a != nil || b != nil || why != c.why {
			t.Errorf("%s: %v %v %q, want empty with %q", c.name, a, b, why, c.why)
		}
	}
}

// A-11: the scan runs every ceil(cost / (0.5% of the interval)) ticks, at
// most every sixth, and every tick when it costs 0.5% or less.
func TestScanEveryFor(t *testing.T) {
	for _, c := range []struct {
		cost, interval time.Duration
		want           int
	}{
		{0, 10 * time.Second, 1},
		{50 * time.Millisecond, 10 * time.Second, 1},
		{51 * time.Millisecond, 10 * time.Second, 2},
		{120 * time.Millisecond, 10 * time.Second, 3},
		{300 * time.Millisecond, 10 * time.Second, 6},
		{time.Second, 10 * time.Second, 6},
		{25 * time.Millisecond, 2 * time.Second, 3},
	} {
		if got := scanEveryFor(c.cost, c.interval); got != c.want {
			t.Errorf("scanEveryFor(%v, %v) = %d, want %d", c.cost, c.interval, got, c.want)
		}
	}
}
