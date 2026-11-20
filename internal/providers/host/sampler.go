// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"io/fs"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// maxScanEvery bounds the process-scan throttle: a scan runs at least every
// sixth tick (A-11).
const maxScanEvery = 6

// samplerRun is one RunBackground call's state. Only the run's own
// goroutine writes its fields; handlers read snap (atomic) and hist (its
// own mutex). It is dropped, with its history, when the run returns
// (NFR-2, ASM-3).
type samplerRun struct {
	p    *Provider
	snap atomic.Pointer[snapshot]
	hist *history

	settings  Settings // the settings the run last applied
	prev      sample   // the last good sample
	seq       uint64   // the seq of the newest snapshot
	scanEvery int      // a process scan runs every scanEvery ticks (A-11)
	untilScan int      // ticks left until the next scan, this one included
}

// snapshot is the run's newest snapshot, nil for a nil run (the sampler is
// not running) or before the first sample.
func (r *samplerRun) snapshot() *snapshot {
	if r == nil {
		return nil
	}
	return r.snap.Load()
}

// history is the run's metrics history, nil for a nil run.
func (r *samplerRun) history() *history {
	if r == nil {
		return nil
	}
	return r.hist
}

// RunBackground is the sampler (provider.Background). Off Linux it returns
// at once. Otherwise it stores a new run in p.run, samples once at once
// (the snapshot is warming up until a second sample exists), then samples
// on every tick of a ticker at sampleInterval. Each sample is recovered on
// its own, so a panic costs one tick. When ctx is done it stops the
// ticker, clears p.run only if it still holds this run (a newer
// overlapping run owns the state otherwise) and returns.
func (p *Provider) RunBackground(ctx context.Context) {
	if p.goos != "linux" {
		return
	}
	s := p.Settings()
	run := &samplerRun{
		p:         p,
		hist:      newHistory(historySamples(s.HistoryDuration, s.SampleInterval), s.SampleInterval),
		settings:  s,
		scanEvery: 1,
	}
	p.run.Store(run)
	run.recovered(run.sample)

	tk := p.newTicker(s.SampleInterval)
	for {
		select {
		case <-ctx.Done():
			tk.Stop()
			p.run.CompareAndSwap(run, nil)
			return
		case <-tk.C():
			run.recovered(func() { run.tick(tk) })
		}
	}
}

// recovered runs one unit of the sampler's work, logging a panic with its
// stack instead of letting it end nexusd (NFR-4). The run's last good
// sample and snapshot stay as they were, so the next tick's deltas cover
// the longer interval.
func (r *samplerRun) recovered(fn func()) {
	defer func() {
		if v := recover(); v != nil {
			r.p.log.Error("host sampler: tick panicked", "panic", v, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// tick applies changed settings, then samples. A changed interval resets
// the ticker; a changed interval or history duration resizes the history,
// keeping the newest points (FR-13).
func (r *samplerRun) tick(tk Ticker) {
	s := r.p.Settings()
	old := r.settings
	if s.SampleInterval != old.SampleInterval {
		tk.Reset(s.SampleInterval)
		r.p.log.Info("sampler interval changed", "from", old.SampleInterval, "to", s.SampleInterval)
	}
	if s.SampleInterval != old.SampleInterval || s.HistoryDuration != old.HistoryDuration {
		r.hist.Resize(historySamples(s.HistoryDuration, s.SampleInterval), s.SampleInterval)
	}
	r.settings = s
	r.sample()
}

// sample reads the counters, computes the snapshot against the last good
// sample, publishes it and appends it to the history. The run's state is
// committed only after the snapshot is computed, so a panic in reading or
// computing leaves the previous sample as the base.
func (r *samplerRun) sample() {
	interval := r.settings.SampleInterval
	scan := r.untilScan <= 1
	cur := readSample(r.p.src, r.p.now, r.p.now(), scan)
	if scan {
		cur.procBase = r.prev.procs
	} else {
		cur.procs, cur.procBase = r.prev.procs, r.prev.procBase
	}

	snap := compute(r.prev, cur, cur.at.Sub(r.prev.at), r.p.pageSize)
	snap.seq = r.seq + 1
	snap.interval = interval

	scanEvery, untilScan := r.scanEvery, r.untilScan-1
	if scan {
		scanEvery = scanEveryFor(cur.scanCost, interval)
		untilScan = scanEvery
		if scanEvery != r.scanEvery {
			r.p.log.Info("host sampler: process scan throttled", "scanCost", cur.scanCost,
				"interval", interval, "every", scanEvery)
		}
	}

	r.snap.Store(snap)
	r.prev, r.seq, r.scanEvery, r.untilScan = cur, snap.seq, scanEvery, untilScan
	r.hist.Append(histSampleOf(cur, snap, wholeDisks(r.p.src)))
}

// scanEveryFor is A-11: a process scan that costs more than 0.5% of the
// interval runs every ceil(cost / (0.005 × interval)) ticks, at most every
// sixth.
func scanEveryFor(cost, interval time.Duration) int {
	budget := interval / 200
	if budget <= 0 || cost <= budget {
		return 1
	}
	return int(min((cost+budget-1)/budget, maxScanEvery))
}

// histSampleOf is the history's record of a sample: CPU % and the entity
// rates from the snapshot, load and memory from the sample itself. Its disks
// are the whole block devices of whole (wholeDisks), joined on name and
// maj:min, so a partition or a device the disks op does not list never takes
// one of the 32 history disk slots (orchestrator ruling after WI 4.2). The
// snapshot keeps every /proc/diskstats line.
func histSampleOf(cur sample, snap *snapshot, whole map[string]devOf) histSample {
	h := histSample{At: cur.at, CPUPercent: snap.cpu.usage.v}
	if cur.load != nil {
		h.Load1 = ptr(cur.load.Load1)
	}
	if total, ok := cur.mem["MemTotal"]; ok {
		if avail, ok := cur.mem["MemAvailable"]; ok {
			h.MemAvailable = ptr(float64(avail))
			if total >= avail {
				h.MemUsed = ptr(float64(total - avail))
			}
		}
	}
	if total, ok := cur.mem["SwapTotal"]; ok {
		if free, ok := cur.mem["SwapFree"]; ok && total >= free {
			h.SwapUsed = ptr(float64(total - free))
		}
	}
	for name, i := range snap.ifaces {
		h.Interfaces = append(h.Interfaces, histEntity{Name: name, Device: cur.ifaces[name].device, A: i.rx.v, B: i.tx.v})
	}
	for name, d := range snap.disks {
		if dev, ok := whole[name]; !ok || dev != (devOf{d.major, d.minor}) {
			continue
		}
		h.Disks = append(h.Disks, histEntity{Name: name, Device: !isVirtualDisk(name), A: d.readBps.v, B: d.writeBps.v})
	}
	return h
}

// wholeDisks is the whole block devices the disks op lists, by kernel name
// with the maj:min of sys/block/<d>/dev: sys/block less ram*, zram* and
// loop* without loop/backing_file (collectDisks' rule). A device whose dev
// cannot be read is left out, and an unlistable sys/block gives none, since
// no diskstats line can then be told from a partition. Every read is kernel
// memory, so none is bounded.
func wholeDisks(src *source) map[string]devOf {
	entries, err := src.readDir(blockDir)
	if err != nil {
		return nil
	}
	out := make(map[string]devOf, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") {
			continue
		}
		devPath, err := segPath(segName, blockDir, name, "dev")
		if err != nil {
			continue
		}
		if strings.HasPrefix(name, "loop") {
			backing, _ := segPath(segName, blockDir, name, "loop", "backing_file") // name passed segPath
			if _, err := src.stat(backing); errors.Is(err, fs.ErrNotExist) {
				continue // an unbacked loop device
			}
		}
		if dev, why := readAttr(src, devPath, parseDev); why == "" {
			out[name] = dev
		}
	}
	return out
}

// isVirtualDisk is a loop or device-mapper device, which ranks after real
// disks for a history slot.
func isVirtualDisk(name string) bool {
	return strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "dm-")
}

func ptr[T any](v T) *T { return &v }

// state is the sampler state of a snapshot read at now (FR-11, A-10):
// not-running without one, stale when it is older than 3 × its interval,
// warming-up before a second sample, else ready.
func (s *snapshot) state(now time.Time) string {
	switch {
	case s == nil:
		return spec.SamplerNotRunning
	case now.Sub(s.at) > 3*s.interval:
		return spec.SamplerStale
	case s.elapsed == 0:
		return spec.SamplerWarmingUp
	}
	return spec.SamplerReady
}

// blocked is the reason none of the snapshot's computed values may be used
// at now, or "" when they may: not running, or stale. While warming up the
// values carry their own reason.
func (s *snapshot) blocked(now time.Time) string {
	switch s.state(now) {
	case spec.SamplerNotRunning:
		return reasonNotRunning
	case spec.SamplerStale:
		return reasonStale(now.Sub(s.at))
	}
	return ""
}

// val is a computed value as a response field: the value, or nil and the
// reason.
func (s *snapshot) val(now time.Time, v rateVal) (*float64, string) {
	if why := s.blocked(now); why != "" {
		return nil, why
	}
	if v.v == nil {
		return nil, v.why
	}
	x := *v.v
	return &x, ""
}

// sampler is the response's sampler object at now.
func (s *snapshot) sampler(now time.Time) spec.Sampler {
	out := spec.Sampler{State: s.state(now)}
	if s == nil {
		out.Unavailable = spec.Unavailable{"sampledAt": reasonNotRunning, "intervalSeconds": reasonNotRunning,
			"ageSeconds": reasonNotRunning}
		return out
	}
	at := s.at.UTC()
	out.SampledAt = &at
	out.AgeSeconds = ptr(now.Sub(s.at).Seconds())
	if s.elapsed > 0 {
		out.IntervalSeconds = ptr(s.elapsed.Seconds())
	} else {
		out.Unavailable = spec.Unavailable{"intervalSeconds": reasonWarmingUp}
	}
	return out
}

// processes is the top lists and the interval they cover, or empty lists,
// nil and the reason.
func (s *snapshot) processes(now time.Time) (byCPU, byMem []procRate, seconds *float64, why string) {
	if why := s.blocked(now); why != "" {
		return nil, nil, nil, why
	}
	if s.procsWhy != "" {
		return nil, nil, nil, s.procsWhy
	}
	return s.topCPU, s.topMem, ptr(s.procInterval.Seconds()), ""
}
