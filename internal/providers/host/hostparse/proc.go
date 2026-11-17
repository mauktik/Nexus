// Package hostparse holds the host provider's pure parsers (architecture
// D-4): each takes the bytes of one /proc, /sys, /etc or /run file and
// returns typed values. A parser skips a line it cannot read, returns an
// error only when a required anchor is missing (an empty file is one), and
// never panics on any input (NFR-4). Standard library only.
package hostparse

import (
	"bytes"
	"errors"
	"iter"
	"math"
	"slices"
	"strconv"
	"strings"
)

// lines yields each line of b without its "\n" or "\r\n" terminator.
func lines(b []byte) iter.Seq[string] {
	return func(yield func(string) bool) {
		for l := range bytes.Lines(b) {
			if !yield(strings.TrimRight(string(l), "\r\n")) {
				return
			}
		}
	}
}

func parseU64(s string) (uint64, bool) {
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

// parseFloat accepts finite decimal values only: strconv also takes "NaN",
// "Inf" and out-of-range values, none of which a kernel file prints.
func parseFloat(s string) (float64, bool) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// CPUTimes is one cpu line of /proc/stat in USER_HZ ticks. guest and
// guest_nice are not kept: the kernel already counts guest time in user and
// nice (kernel/sched/cputime.c account_guest_time), so adding them would
// count it twice.
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Total is the sum of every kept column.
func (c CPUTimes) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// Busy is Total without idle and iowait.
func (c CPUTimes) Busy() uint64 {
	return c.Total() - c.Idle - c.IOWait
}

// PerCPU is one cpuN line of /proc/stat.
type PerCPU struct {
	ID    int
	Times CPUTimes
}

// Stat is /proc/stat: the aggregate cpu line, the cpuN lines in file order
// and btime (seconds since the Epoch).
type Stat struct {
	Total       CPUTimes
	CPUs        []PerCPU
	BootTime    uint64
	HasBootTime bool
}

// cpuTimes reads the columns after a cpu label. Kernels before 2.6.11 print
// fewer than eight, so four (user nice system idle) is the minimum and a
// missing column is 0.
func cpuTimes(f []string) (CPUTimes, bool) {
	if len(f) < 4 {
		return CPUTimes{}, false
	}
	var v [8]uint64
	for i := 0; i < len(v) && i < len(f); i++ {
		n, ok := parseU64(f[i])
		if !ok {
			return CPUTimes{}, false
		}
		v[i] = n
	}
	return CPUTimes{User: v[0], Nice: v[1], System: v[2], Idle: v[3], IOWait: v[4], IRQ: v[5], SoftIRQ: v[6], Steal: v[7]}, true
}

// ProcStat parses /proc/stat. The aggregate "cpu " line is the required
// anchor.
func ProcStat(b []byte) (Stat, error) {
	var st Stat
	haveTotal := false
	for l := range lines(b) {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "cpu":
			if t, ok := cpuTimes(f[1:]); ok && !haveTotal {
				st.Total, haveTotal = t, true
			}
		case strings.HasPrefix(f[0], "cpu"):
			id, err := strconv.Atoi(f[0][len("cpu"):])
			if err != nil || id < 0 || slices.ContainsFunc(st.CPUs, func(c PerCPU) bool { return c.ID == id }) {
				continue
			}
			if t, ok := cpuTimes(f[1:]); ok {
				st.CPUs = append(st.CPUs, PerCPU{ID: id, Times: t})
			}
		case f[0] == "btime" && len(f) >= 2 && !st.HasBootTime:
			st.BootTime, st.HasBootTime = parseU64(f[1])
		}
	}
	if !haveTotal {
		return Stat{}, errors.New("proc/stat: no cpu line")
	}
	return st, nil
}

// meminfoKeys are the /proc/meminfo fields the host provider reads.
var meminfoKeys = []string{"MemTotal", "MemFree", "MemAvailable", "Buffers", "Cached", "SReclaimable", "Shmem", "SwapTotal", "SwapFree"}

// Meminfo parses /proc/meminfo into bytes (the file's kB × 1024) for the
// keys the provider needs; other keys are dropped. MemTotal is the required
// anchor. A key whose line is malformed, or not in kB, is absent.
func Meminfo(b []byte) (map[string]uint64, error) {
	out := map[string]uint64{}
	for l := range lines(b) {
		key, rest, ok := strings.Cut(l, ":")
		if !ok || !slices.Contains(meminfoKeys, key) {
			continue
		}
		if _, dup := out[key]; dup {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 2 || f[1] != "kB" {
			continue
		}
		kb, ok := parseU64(f[0])
		if !ok || kb > math.MaxUint64/1024 {
			continue
		}
		out[key] = kb * 1024
	}
	if _, ok := out["MemTotal"]; !ok {
		return nil, errors.New("proc/meminfo: no MemTotal line")
	}
	return out, nil
}

// LoadAverage is the first three fields of /proc/loadavg.
type LoadAverage struct {
	Load1, Load5, Load15 float64
}

// Loadavg parses /proc/loadavg.
func Loadavg(b []byte) (LoadAverage, error) {
	for l := range lines(b) {
		f := strings.Fields(l)
		if len(f) < 3 {
			break
		}
		l1, ok1 := parseFloat(f[0])
		l5, ok5 := parseFloat(f[1])
		l15, ok15 := parseFloat(f[2])
		if ok1 && ok5 && ok15 {
			return LoadAverage{Load1: l1, Load5: l5, Load15: l15}, nil
		}
		break
	}
	return LoadAverage{}, errors.New("proc/loadavg: no load averages")
}

// Uptime parses /proc/uptime: the seconds since boot (its first field).
func Uptime(b []byte) (float64, error) {
	for l := range lines(b) {
		f := strings.Fields(l)
		if len(f) == 0 {
			break
		}
		if s, ok := parseFloat(f[0]); ok && s >= 0 {
			return s, nil
		}
		break
	}
	return 0, errors.New("proc/uptime: no uptime")
}

// VMStat is what the host provider reads from /proc/vmstat: oom_kill, which
// kernels before 4.13 do not have, so its absence is distinct from 0.
type VMStat struct {
	OOMKill        uint64
	OOMKillPresent bool
}

// Vmstat parses /proc/vmstat. The anchor is any well-formed "name value"
// line.
func Vmstat(b []byte) (VMStat, error) {
	var v VMStat
	anchored := false
	for l := range lines(b) {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		n, ok := parseU64(f[1])
		if !ok {
			continue
		}
		anchored = true
		if f[0] == "oom_kill" && !v.OOMKillPresent {
			v.OOMKill, v.OOMKillPresent = n, true
		}
	}
	if !anchored {
		return VMStat{}, errors.New("proc/vmstat: no counter line")
	}
	return v, nil
}

// PressureLine is one line of a /proc/pressure file: the stall percentages
// over 10, 60 and 300 seconds and the total stall time in microseconds.
type PressureLine struct {
	Avg10, Avg60, Avg300 float64
	Total                uint64
}

// PressureStat is one /proc/pressure file. Full is nil when the file has no
// full line (the cpu file before Linux 5.13).
type PressureStat struct {
	Some, Full *PressureLine
}

// Pressure parses /proc/pressure/{cpu,memory,io}. The some line is the
// required anchor. A line needs avg10, avg60 and avg300; total is optional
// and unknown keys are ignored.
func Pressure(b []byte) (PressureStat, error) {
	var p PressureStat
	for l := range lines(b) {
		f := strings.Fields(l)
		if len(f) == 0 || (f[0] != "some" && f[0] != "full") {
			continue
		}
		pl, ok := pressureLine(f[1:])
		if !ok {
			continue
		}
		if f[0] == "some" && p.Some == nil {
			p.Some = &pl
		} else if f[0] == "full" && p.Full == nil {
			p.Full = &pl
		}
	}
	if p.Some == nil {
		return PressureStat{}, errors.New("proc/pressure: no some line")
	}
	return p, nil
}

func pressureLine(kvs []string) (PressureLine, bool) {
	var pl PressureLine
	var seen [3]bool
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		var dst *float64
		switch k {
		case "avg10":
			dst, seen[0] = &pl.Avg10, true
		case "avg60":
			dst, seen[1] = &pl.Avg60, true
		case "avg300":
			dst, seen[2] = &pl.Avg300, true
		case "total":
			n, ok := parseU64(v)
			if !ok {
				return PressureLine{}, false
			}
			pl.Total = n
			continue
		default:
			continue
		}
		f, ok := parseFloat(v)
		if !ok {
			return PressureLine{}, false
		}
		*dst = f
	}
	return pl, seen[0] && seen[1] && seen[2]
}

// CPUInfo is what the host provider reads from /proc/cpuinfo: the first
// model name, the cpu MHz of each processor that reports one (none on
// aarch64), whether the first flags line holds the hypervisor flag, and the
// processor count.
type CPUInfo struct {
	Model      string
	MHz        []float64
	Hypervisor bool
	Processors int
}

// Cpuinfo parses /proc/cpuinfo. A processor line is the required anchor.
func Cpuinfo(b []byte) (CPUInfo, error) {
	var c CPUInfo
	flagsSeen := false
	for l := range lines(b) {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "processor":
			c.Processors++
		case "model name":
			if c.Model == "" {
				c.Model = strings.TrimSpace(v)
			}
		case "cpu MHz":
			if f := strings.Fields(v); len(f) > 0 {
				if mhz, ok := parseFloat(f[0]); ok {
					c.MHz = append(c.MHz, mhz)
				}
			}
		case "flags":
			if !flagsSeen {
				flagsSeen = true
				c.Hypervisor = slices.Contains(strings.Fields(v), "hypervisor")
			}
		}
	}
	if c.Processors == 0 {
		return CPUInfo{}, errors.New("proc/cpuinfo: no processor line")
	}
	return c, nil
}
