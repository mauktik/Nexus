// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-52, TC-HP-53, TC-HP-54, TC-HP-55, TC-HP-56, TC-HP-57, TC-HP-58, TC-HP-59, TC-HP-60, TC-HP-E12, TC-HP-E13, TC-HP-133, TC-HP-136, TC-HP-137, TC-HP-138.
// Owner WIs: 3.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package host

import (
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-HP-52: History capacity is historyDuration over sampleInterval, capped at 21,600 stored points, and overwrites the oldest. Owner WI 3.2. Requirements: FR-13, NFR-2.
//
// - Setup: configurations `(1h, 10s)`, `(24h, 2s)`, `(12h, 2s)` and `(10s, 10s)`. For each, the test computes `n = historyDuration / sampleInterval`, `k = ceil(n / MaxHistoryPoints)` and `capacity = ceil(n / k)` with A-40's formula, using the package's `MaxHistoryPoints`. Samples are `sampleInterval` apart from `T0 = 2026-01-01T00:00:00Z`.
// - Steps: build each history; append `k × capacity + k` samples with cpuPercent = i (i from 0) and every other value present.
// - Expected:
//   - `(k, capacity)` is `(1, 360)`, `(2, 21,600)`, `(1, 21,600)` and `(1, 1)`. The computed values must equal these literals, which guards the formula.
//   - For k = 1: the ring holds `capacity` points. The oldest has cpuPercent 1 (point 0 was overwritten), and the newest has cpuPercent `capacity`.
//   - For k = 2 (24 h at 2 s): `capacity + 1` points were committed, and the ring holds `capacity`. The oldest stored point is the mean of samples 2 and 3 (cpuPercent 2.5) with sample 3's timestamp. The newest is the mean of samples `2·capacity` and `2·capacity + 1`, with the last sample's timestamp. The test computes both from the appended series.
//   - `Query` with no step reports `stepSeconds ≥ k × sampleInterval` for each.
func TestTCHP52_HistoryCapacityIsHistoryDurationOverSampleIntervalAnd(t *testing.T) {
	cases := []struct {
		dur, iv     time.Duration
		k, capacity int
	}{
		{time.Hour, 10 * time.Second, 1, 360},
		{24 * time.Hour, 2 * time.Second, 2, 21600},
		{12 * time.Hour, 2 * time.Second, 1, 21600},
		{10 * time.Second, 10 * time.Second, 1, 1},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%v_at_%v", c.dur, c.iv), func(t *testing.T) {
			n := int(c.dur / c.iv)
			k := (n + spec.MaxHistoryPoints - 1) / spec.MaxHistoryPoints
			capacity := (n + k - 1) / k
			if k != c.k || capacity != c.capacity {
				t.Fatalf("A-40 formula gives (k %d, capacity %d), want (%d, %d)", k, capacity, c.k, c.capacity)
			}
			h := newHistory(historySamples(c.dur, c.iv), c.iv)
			if h.k != k || h.capacity != capacity {
				t.Fatalf("history (k %d, capacity %d), want (%d, %d)", h.k, h.capacity, k, capacity)
			}
			total := k*capacity + k
			at := func(i int) time.Time { return histT0.Add(time.Duration(i) * c.iv) }
			for i := range total {
				h.Append(fullSample(at(i), float64(i)))
			}
			pts := stored(h)
			if len(pts) != capacity {
				t.Fatalf("ring holds %d points, want %d", len(pts), capacity)
			}
			oldest, newest := pts[0], pts[len(pts)-1]
			if k == 1 {
				if oldest.agg[aggCPU] != 1 || newest.agg[aggCPU] != float32(capacity) {
					t.Errorf("oldest cpu %v newest cpu %v, want 1 and %d", oldest.agg[aggCPU], newest.agg[aggCPU], capacity)
				}
				if !newest.at.Equal(at(total - 1)) {
					t.Errorf("newest at %v, want %v", newest.at, at(total-1))
				}
			} else {
				if h.seq != uint64(capacity+1) {
					t.Errorf("%d points committed, want %d", h.seq, capacity+1)
				}
				wantOld := float32((2.0 + 3.0) / 2)
				if oldest.agg[aggCPU] != wantOld || !oldest.at.Equal(at(3)) {
					t.Errorf("oldest (%v, %v), want (%v, %v)", oldest.agg[aggCPU], oldest.at, wantOld, at(3))
				}
				wantNew := float32(float64(2*capacity+2*capacity+1) / 2)
				if newest.agg[aggCPU] != wantNew || !newest.at.Equal(at(total-1)) {
					t.Errorf("newest (%v, %v), want (%v, %v)", newest.agg[aggCPU], newest.at, wantNew, at(total-1))
				}
			}
			p := h.Query(at(total-1), spec.DefaultWindowSeconds, 0, nil)
			if min := k * int(c.iv/time.Second); p.StepSeconds < min {
				t.Errorf("stepSeconds %d < k × sampleInterval = %d", p.StepSeconds, min)
			}
		})
	}
}

// TC-HP-53: Resize keeps the newest points. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: capacity 360 with 360 points (cpu = i).
// - Steps: `Resize(100)`; then `Resize(500)` and append 10.
// - Expected: after 100: points 260..359 in order; after 500 + 10: 110 points, 260..359 then the 10 new ones; per-entity rings resized identically.
func TestTCHP53_ResizeKeepsTheNewestPoints(t *testing.T) {
	iv := 10 * time.Second
	h := newHistory(360, iv)
	appendN := func(from, to int) {
		for i := from; i < to; i++ {
			s := fullSample(histT0.Add(time.Duration(i)*iv), float64(i))
			s.Interfaces = []histEntity{{Name: "eth0", Device: true, A: fp(float64(i)), B: fp(float64(-i))}}
			s.Disks = []histEntity{{Name: "sda", Device: true, A: fp(float64(2 * i)), B: fp(float64(3 * i))}}
			h.Append(s)
		}
	}
	check := func(want []int) {
		t.Helper()
		pts := stored(h)
		if len(pts) != len(want) {
			t.Fatalf("%d points, want %d", len(pts), len(want))
		}
		ia, ib, ok1 := storedEntity(h, entIface, "eth0")
		da, db, ok2 := storedEntity(h, entDisk, "sda")
		if !ok1 || !ok2 || len(ia) != len(want) || len(da) != len(want) {
			t.Fatalf("entity rings: eth0 %v (%d), sda %v (%d), want %d each", ok1, len(ia), ok2, len(da), len(want))
		}
		for j, w := range want {
			if pts[j].agg[aggCPU] != float32(w) || !pts[j].at.Equal(histT0.Add(time.Duration(w)*iv)) {
				t.Fatalf("point %d is (%v, %v), want cpu %d", j, pts[j].agg[aggCPU], pts[j].at, w)
			}
			if ia[j] != float32(w) || ib[j] != float32(-w) || da[j] != float32(2*w) || db[j] != float32(3*w) {
				t.Fatalf("point %d entity values %v %v %v %v, want %d", j, ia[j], ib[j], da[j], db[j], w)
			}
		}
	}
	appendN(0, 360)
	h.Resize(100, iv)
	want := make([]int, 0, 110)
	for i := 260; i < 360; i++ {
		want = append(want, i)
	}
	check(want)
	h.Resize(500, iv)
	appendN(360, 370)
	for i := 360; i < 370; i++ {
		want = append(want, i)
	}
	check(want)
	if h.capacity != 500 {
		t.Errorf("capacity %d, want 500", h.capacity)
	}
}

// TC-HP-54: Query aligns to the step and averages each bucket. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: 360 points at 10 s from `T0 = 2026-01-01T00:00:05Z`, cpu = i.
// - Steps: `Query(now = T0+3595s, window 3600, step 60, [cpu])`.
// - Expected: `from` = `max(now−window, oldest)` aligned down to 60 s; `stepSeconds` 60; each value equals the mean of the points whose timestamp falls in `[from+i·60, from+(i+1)·60)`, computed in-test from the appended series; `points == len(values)`; `historyStart` = T0.
func TestTCHP54_QueryAlignsToTheStepAndAverages(t *testing.T) {
	t0 := histT0.Add(5 * time.Second)
	h := newHistory(360, 10*time.Second)
	type pt struct {
		at  time.Time
		cpu float64
	}
	var series []pt
	for i := range 360 {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		h.Append(fullSample(at, float64(i)))
		series = append(series, pt{at, float64(i)})
	}
	now := t0.Add(3595 * time.Second)
	p := h.Query(now, 3600, 60, []string{spec.MetricCPU})

	start := now.Add(-3600 * time.Second)
	if t0.After(start) {
		start = t0
	}
	wantFrom := time.Unix(start.Unix()/60*60, 0).UTC()
	if !p.From.Equal(wantFrom) || p.StepSeconds != 60 {
		t.Fatalf("from %v step %d, want %v and 60", p.From, p.StepSeconds, wantFrom)
	}
	if p.HistoryStart == nil || !p.HistoryStart.Equal(t0) {
		t.Errorf("historyStart %v, want %v", p.HistoryStart, t0)
	}
	if len(p.Series) != 1 {
		t.Fatalf("%d series, want 1", len(p.Series))
	}
	vals := p.Series[0].Values
	if p.Points != len(vals) || p.Points == 0 {
		t.Fatalf("points %d, len(values) %d", p.Points, len(vals))
	}
	for i, v := range vals {
		lo, hi := wantFrom.Add(time.Duration(i)*time.Minute), wantFrom.Add(time.Duration(i+1)*time.Minute)
		var sum float64
		var n int
		for _, s := range series {
			if !s.at.Before(lo) && s.at.Before(hi) {
				sum += s.cpu
				n++
			}
		}
		switch {
		case n == 0 && v != nil:
			t.Errorf("bucket %d = %v, want null", i, *v)
		case n > 0 && (v == nil || !relClose(*v, sum/float64(n))):
			t.Errorf("bucket %d = %v, want %v", i, v, sum/float64(n))
		}
	}
}

// TC-HP-55: Buckets without samples are null. Owner WI 3.2. Requirements: FR-13, FR-12.
//
// - Setup: points at 10 s with a 5-minute gap (no appends).
// - Steps: `Query(window 3600, step 60)`.
// - Expected: the buckets inside the gap are `null` (JSON `null` in `values`), the others numbers; `assertExplicit` accepts the result (the documented `series[].values` exemption).
func TestTCHP55_BucketsWithoutSamplesAreNull(t *testing.T) {
	h := newHistory(360, 10*time.Second)
	var ats []time.Time
	for i := range 360 {
		if i >= 120 && i < 150 { // 30 × 10 s: the 5-minute gap
			continue
		}
		at := histT0.Add(time.Duration(i) * 10 * time.Second)
		h.Append(fullSample(at, float64(i)))
		ats = append(ats, at)
	}
	now := ats[len(ats)-1]
	p := h.Query(now, 3600, 60, nil)
	if p.StepSeconds != 60 {
		t.Fatalf("stepSeconds %d, want 60", p.StepSeconds)
	}
	nulls := 0
	for i := range p.Points {
		lo, hi := p.From.Add(time.Duration(i)*time.Minute), p.From.Add(time.Duration(i+1)*time.Minute)
		has := slices.ContainsFunc(ats, func(a time.Time) bool { return !a.Before(lo) && a.Before(hi) })
		if !has {
			nulls++
		}
		for _, s := range p.Series {
			if has != (s.Values[i] != nil) {
				t.Errorf("%s/%s bucket %d at %v: value %v, samples in bucket %v", s.Metric, s.Name, i, lo, s.Values[i], has)
			}
		}
	}
	if nulls < 4 {
		t.Errorf("%d null buckets, want at least the 4 whole minutes of the gap", nulls)
	}
	raw, err := json.Marshal(p.Series[0].Values)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "null") {
		t.Errorf("values JSON %s has no null", raw)
	}
	checkHistoryExplicit(t, p)
}

// TC-HP-56: At most 500 points per series. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: 24 h at 2 s (k = 2, capacity 21,600; A-40), full.
// - Steps: `Query(window 86400, step 0, [cpu])`.
// - Expected: `stepSeconds ≥ max(ceil(86400/500), k × 2) = 173` (the right side computed in-test); every series has ≤ 500 values.
func TestTCHP56_AtMost500PointsPerSeries(t *testing.T) {
	iv := 2 * time.Second
	h := newHistory(historySamples(24*time.Hour, iv), iv)
	var last time.Time
	for i := range 43200 {
		last = histT0.Add(time.Duration(i) * iv)
		h.Append(fullSample(last, float64(i%100)))
	}
	if n := len(stored(h)); n != 21600 {
		t.Fatalf("ring holds %d, want 21600", n)
	}
	p := h.Query(last, 86400, 0, []string{spec.MetricCPU})
	want := max((86400+spec.MaxSeriesPoints-1)/spec.MaxSeriesPoints, h.k*2)
	if want != 173 {
		t.Fatalf("in-test bound %d, want 173", want)
	}
	if p.StepSeconds < want {
		t.Errorf("stepSeconds %d < %d", p.StepSeconds, want)
	}
	for _, s := range p.Series {
		if len(s.Values) > spec.MaxSeriesPoints || len(s.Values) != p.Points {
			t.Errorf("%s has %d values (points %d)", s.Name, len(s.Values), p.Points)
		}
	}
}

// TC-HP-57: At most 20,000 values per response. Owner WI 3.2. Requirements: FR-13, NFR-1.
//
// - Setup: capacity 1,800 at 2 s (1 h), 32 interfaces and 32 disks, all points filled.
// - Steps: `Query(window 3600, step 2, [cpu, load, memory, swap, network, disk])`.
// - Expected: `len(series) × points ≤ 20,000`; `stepSeconds > 2`; and the raise is not excessive: with `minimal` = the smallest multiple of the 2 s interval for which `len(series) × ceil(3600/step) ≤ 20,000` (computed in-test), `minimal ≤ stepSeconds ≤ 2 × minimal`.
func TestTCHP57_AtMost20000ValuesPerResponse(t *testing.T) {
	iv := 2 * time.Second
	h := newHistory(1800, iv)
	ifs := histEntities("eth", 32, true, fp(5))
	disks := histEntities("sd", 32, true, fp(7))
	var last time.Time
	for i := range 1800 {
		last = histT0.Add(time.Duration(i) * iv)
		s := fullSample(last, float64(i))
		s.Interfaces, s.Disks = ifs, disks
		h.Append(s)
	}
	p := h.Query(last, 3600, 2, spec.MetricNames)
	if len(p.Series) != 5+64+64 {
		t.Fatalf("%d series, want 133", len(p.Series))
	}
	if len(p.Series)*p.Points > spec.MaxResponseValues {
		t.Errorf("%d series × %d points > %d", len(p.Series), p.Points, spec.MaxResponseValues)
	}
	if p.StepSeconds <= 2 {
		t.Errorf("stepSeconds %d not raised", p.StepSeconds)
	}
	minimal := 2
	for len(p.Series)*((3600+minimal-1)/minimal) > spec.MaxResponseValues {
		minimal += 2
	}
	if p.StepSeconds < minimal || p.StepSeconds > 2*minimal {
		t.Errorf("stepSeconds %d outside [%d, %d]", p.StepSeconds, minimal, 2*minimal)
	}
	for _, s := range p.Series {
		if len(s.Values) != p.Points {
			t.Errorf("%s/%s/%s: %d values, points %d", s.Metric, s.Name, s.Entity, len(s.Values), p.Points)
		}
	}
	t.Logf("stepSeconds %d, points %d, minimal %d", p.StepSeconds, p.Points, minimal)
}

// TC-HP-58: Default metrics are cpu, load, memory and swap. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: a filled history with 2 interfaces and 1 disk.
// - Steps: `Query` with no metric; then with `[network]`; then `[disk]`.
// - Expected: default series metrics are exactly `cpu`, `load`, `memory` (used and available), `swap`, with units declared; no network or disk series; `[network]` gives rx and tx per interface with `entity` = interface name; `[disk]` gives read and write per disk.
func TestTCHP58_DefaultMetricsAreCpuLoadMemoryAnd(t *testing.T) {
	iv := 10 * time.Second
	h := newHistory(360, iv)
	var last time.Time
	for i := range 360 {
		last = histT0.Add(time.Duration(i) * iv)
		s := fullSample(last, float64(i))
		s.Interfaces = []histEntity{{Name: "eth1", Device: true, A: fp(1), B: fp(2)}, {Name: "eth0", Device: true, A: fp(3), B: fp(4)}}
		s.Disks = []histEntity{{Name: "sda", Device: true, A: fp(5), B: fp(6)}}
		h.Append(s)
	}
	shape := func(p spec.MetricsProperties) []string {
		var out []string
		for _, s := range p.Series {
			out = append(out, strings.Join([]string{s.Metric, s.Name, s.Entity, s.Unit}, "|"))
		}
		return out
	}
	cases := []struct {
		metrics []string
		want    []string
	}{
		{nil, []string{
			"cpu|usagePercent||percent", "load|load1||load",
			"memory|usedBytes||bytes", "memory|availableBytes||bytes", "swap|usedBytes||bytes",
		}},
		{[]string{spec.MetricNetwork}, []string{
			"network|rxBytesPerSecond|eth0|bytes/s", "network|txBytesPerSecond|eth0|bytes/s",
			"network|rxBytesPerSecond|eth1|bytes/s", "network|txBytesPerSecond|eth1|bytes/s",
		}},
		{[]string{spec.MetricDisk}, []string{
			"disk|readBytesPerSecond|sda|bytes/s", "disk|writeBytesPerSecond|sda|bytes/s",
		}},
	}
	for _, c := range cases {
		p := h.Query(last, 3600, 0, c.metrics)
		if got := shape(p); !slices.Equal(got, c.want) {
			t.Errorf("metrics %v: series %v, want %v", c.metrics, got, c.want)
		}
		for _, s := range p.Series {
			if len(s.Values) == 0 || s.Values[len(s.Values)-1] == nil {
				t.Errorf("%s/%s/%s has no newest value", s.Metric, s.Name, s.Entity)
			}
		}
	}
	p := h.Query(last, 3600, 0, []string{spec.MetricNetwork})
	if v := findSeries(t, p, spec.MetricNetwork, spec.SeriesTxBytesPerSecond, "eth0").Values; *v[len(v)-1] != 4 {
		t.Errorf("eth0 tx = %v, want 4", *v[len(v)-1])
	}
}

// TC-HP-59: Entity slots go to devices first and free after a full window unseen. Owner WI 3.2. Requirements: FR-13, NFR-2.
//
// - Setup: capacity 10; 33 interfaces: `eth0..eth9` with a device, `veth00..veth22` without.
// - Steps: append one point with all 33; query `[network]`; then append 10 points without `veth21` and `veth22` and with `wlan0` (a device) added.
// - Expected: all 10 devices hold slots; the 33rd by order (`veth22`) is denied and `entitiesTruncated.interfaces == 1`; `veth21` is then unseen for 10 appends (a full window, it is dropped from the later points too), its slot frees, and `wlan0` holds a slot in the next query with `entitiesTruncated.interfaces` reflecting only entities still denied.
func TestTCHP59_EntitySlotsGoToDevicesFirstAnd(t *testing.T) {
	iv := 10 * time.Second
	h := newHistory(10, iv)
	devs := histEntities("eth", 10, true, fp(1))
	veths := histEntities("veth", 23, false, fp(2))
	// Non-devices listed first, so the device-first order cannot come from the input order.
	s := fullSample(histT0, 0)
	s.Interfaces = append(slices.Clone(veths), devs...)
	h.Append(s)

	entities := func(p spec.MetricsProperties) map[string]bool {
		m := map[string]bool{}
		for _, s := range p.Series {
			m[s.Entity] = true
		}
		return m
	}
	p := h.Query(histT0, 3600, 0, []string{spec.MetricNetwork})
	got := entities(p)
	for _, d := range devs {
		if !got[d.Name] {
			t.Errorf("device %s holds no slot", d.Name)
		}
	}
	if got["veth22"] || !got["veth21"] || len(got) != spec.MaxHistoryInterfaces {
		t.Errorf("held %d entities, veth21 %v veth22 %v; want 32 with veth21 and not veth22", len(got), got["veth21"], got["veth22"])
	}
	if p.EntitiesTruncated.Interfaces != 1 {
		t.Errorf("entitiesTruncated.interfaces = %d, want 1", p.EntitiesTruncated.Interfaces)
	}

	later := append(slices.Clone(devs), veths[:21]...) // veth00..veth20
	later = append(later, histEntity{Name: "wlan0", Device: true, A: fp(3), B: fp(3)})
	var now time.Time
	for i := 1; i <= 10; i++ {
		now = histT0.Add(time.Duration(i) * iv)
		s := fullSample(now, float64(i))
		s.Interfaces = later
		h.Append(s)
		if i < 10 {
			if _, _, ok := storedEntity(h, entIface, "veth21"); !ok {
				t.Fatalf("veth21 freed after %d appends, before a full window", i)
			}
			if _, _, ok := storedEntity(h, entIface, "wlan0"); ok {
				t.Fatalf("wlan0 holds a slot after %d appends while all 32 are held", i)
			}
		}
	}
	p = h.Query(now, 3600, 0, []string{spec.MetricNetwork})
	got = entities(p)
	if got["veth21"] || !got["wlan0"] {
		t.Errorf("after a full window: veth21 held %v, wlan0 held %v; want false, true", got["veth21"], got["wlan0"])
	}
	if p.EntitiesTruncated.Interfaces != 0 {
		t.Errorf("entitiesTruncated.interfaces = %d, want 0 (no entity still denied)", p.EntitiesTruncated.Interfaces)
	}
	w := findSeries(t, p, spec.MetricNetwork, spec.SeriesRxBytesPerSecond, "wlan0").Values
	if w[len(w)-1] == nil || *w[len(w)-1] != 3 {
		t.Errorf("wlan0 newest rx = %v, want 3", w[len(w)-1])
	}
}

// fillFull builds a 24 h at 2 s history with 32 interfaces and 32 disks and
// fills it (43,200 appends, every value present). It returns the time of the
// last sample.
func fillFull(h *history) time.Time {
	iv := 2 * time.Second
	ifs := histEntities("eth", spec.MaxHistoryInterfaces, true, fp(1e6))
	disks := histEntities("sd", spec.MaxHistoryDisks, true, fp(2e6))
	s := fullSample(histT0, 1)
	s.Interfaces, s.Disks = ifs, disks
	var last time.Time
	for i := range 2 * spec.MaxHistoryPoints {
		last = histT0.Add(time.Duration(i) * iv)
		s.At = last
		h.Append(s)
	}
	return last
}

// TC-HP-60: History memory stays within the NFR-2 bounds. Owner WI 3.2. Requirements: NFR-2, FR-13.
//
// - Setup: `runtime.GC()` and `runtime.ReadMemStats` before and after.
// - Steps: build 24 h at 2 s (k = 2, capacity 21,600; A-40) with 32 interfaces and 32 disks, and fill it (43,200 appends); separately, build 1 h at 10 s (360 points) and fill it.
// - Expected: the `HeapAlloc` delta (history kept alive with `runtime.KeepAlive`) is under `liveHistoryBudget` = 16 MiB for the full 24 h ring, and under 1 MiB at 360 points. A-27 chose Finding 6 option (a): twice the live heap must stay under NFR-2's 32 MiB. The test holds the budget in the one constant `liveHistoryBudget`, with a comment citing A-27. This TC bounds the live heap only. TC-HP-133 and TC-HP-134 bound the heap including GC headroom, which NFR-2's RSS bound needs.
func TestTCHP60_HistoryMemoryStaysWithinTheNFR2(t *testing.T) {
	// liveHistoryBudget is A-27's live-heap bound (review Finding 6, option
	// (a)): with GOGC=100 the heap grows to about twice the live heap, and
	// twice this stays under NFR-2's 32 MiB.
	const liveHistoryBudget = 16 << 20
	const smallBudget = 1 << 20

	live := func(build func() *history) uint64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		h := build()
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(h)
		if after.HeapAlloc < before.HeapAlloc {
			return 0
		}
		return after.HeapAlloc - before.HeapAlloc
	}

	full := live(func() *history {
		h := newHistory(historySamples(24*time.Hour, 2*time.Second), 2*time.Second)
		fillFull(h)
		if h.count != spec.MaxHistoryPoints {
			t.Fatalf("ring holds %d, want %d", h.count, spec.MaxHistoryPoints)
		}
		return h
	})
	t.Logf("24 h at 2 s, 32+32 entities: live heap %d bytes (%.2f MiB)", full, float64(full)/(1<<20))
	if full >= liveHistoryBudget {
		t.Errorf("live history %d bytes ≥ liveHistoryBudget %d (A-27)", full, liveHistoryBudget)
	}

	small := live(func() *history {
		iv := 10 * time.Second
		h := newHistory(historySamples(time.Hour, iv), iv)
		s := fullSample(histT0, 1)
		s.Interfaces = histEntities("eth", 2, true, fp(1))
		s.Disks = histEntities("sd", 1, true, fp(1))
		for i := range 360 {
			s.At = histT0.Add(time.Duration(i) * iv)
			h.Append(s)
		}
		return h
	})
	t.Logf("1 h at 10 s: live heap %d bytes (%.1f KiB)", small, float64(small)/1024)
	if small >= smallBudget {
		t.Errorf("live history at 360 points %d bytes ≥ 1 MiB", small)
	}
}

// TC-HP-E12: Querying beyond the start of history, and an empty history. Owner WI 3.2. Requirements: FR-13.
//
// - Setup: 6 points at 10 s; and an empty ring.
// - Steps: `Query(window 86400, step 0)` on each.
// - Expected: 6 points: `from` = oldest aligned down to the step, `historyStart` = oldest, no leading nulls before `historyStart`; empty: each default metric series with `values []`, `points 0`, `historyStart` null with `warming up`.
func TestTCHPE12_QueryingBeyondTheStartOfHistoryAnd(t *testing.T) {
	iv := 10 * time.Second
	oldest := histT0.Add(7 * time.Second)
	h := newHistory(360, iv)
	var last time.Time
	for i := range 6 {
		last = oldest.Add(time.Duration(i) * iv)
		h.Append(fullSample(last, float64(i)))
	}
	p := h.Query(last, 86400, 0, nil)
	stepMs := int64(p.StepSeconds) * 1000
	wantFrom := time.UnixMilli(oldest.UnixMilli() / stepMs * stepMs).UTC()
	if !p.From.Equal(wantFrom) {
		t.Errorf("from %v, want oldest %v aligned down to %d s = %v", p.From, oldest, p.StepSeconds, wantFrom)
	}
	if p.HistoryStart == nil || !p.HistoryStart.Equal(oldest) {
		t.Errorf("historyStart %v, want %v", p.HistoryStart, oldest)
	}
	for _, s := range p.Series {
		if len(s.Values) == 0 || s.Values[0] == nil {
			t.Errorf("%s/%s starts with a null or is empty: %v", s.Metric, s.Name, s.Values)
		}
	}
	checkHistoryExplicit(t, p)

	empty := newHistory(360, iv)
	p = empty.Query(histT0, 86400, 0, nil)
	if p.Points != 0 || p.HistoryStart != nil || p.Unavailable["historyStart"] != reasonWarmingUp {
		t.Errorf("empty: points %d historyStart %v unavailable %v", p.Points, p.HistoryStart, p.Unavailable)
	}
	var metrics []string
	for _, s := range p.Series {
		metrics = append(metrics, s.Metric)
		raw, err := json.Marshal(s.Values)
		if err != nil || string(raw) != "[]" {
			t.Errorf("empty %s/%s values JSON %s (%v), want []", s.Metric, s.Name, raw, err)
		}
	}
	if want := []string{"cpu", "load", "memory", "memory", "swap"}; !slices.Equal(metrics, want) {
		t.Errorf("empty series metrics %v, want %v", metrics, want)
	}
	checkHistoryExplicit(t, p)
}

// TC-HP-E13: Concurrent appends and queries are race-free. Owner WI 3.2. Requirements: FR-13, NFR-4.
//
// - Setup: capacity 360.
// - Steps: one goroutine appends 10,000 points and resizes every 1,000 while 4 goroutines query.
// - Expected: no race report; each query result is internally consistent (`points == len(values)` per series, timestamps increasing).
func TestTCHPE13_ConcurrentAppendsAndQueriesAreRaceFree(t *testing.T) {
	iv := 10 * time.Second
	h := newHistory(360, iv)
	var last atomic.Int64
	last.Store(histT0.UnixMilli())
	done := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	report := func(s string) {
		select {
		case errs <- s:
		default:
		}
	}
	for r := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var prevFrom, prevStart time.Time
			for q := 0; ; q++ {
				select {
				case <-done:
					return
				default:
				}
				p := h.Query(time.UnixMilli(last.Load()), 86400, 0, spec.MetricNames)
				for _, s := range p.Series {
					if len(s.Values) != p.Points {
						report(fmt.Sprintf("reader %d query %d: %s/%s has %d values, points %d", r, q, s.Metric, s.Entity, len(s.Values), p.Points))
					}
				}
				if p.HistoryStart != nil {
					if p.HistoryStart.Before(prevStart) || p.From.Before(prevFrom) || p.From.After(*p.HistoryStart) {
						report(fmt.Sprintf("reader %d query %d: from %v historyStart %v after %v %v", r, q, p.From, *p.HistoryStart, prevFrom, prevStart))
					}
					prevFrom, prevStart = p.From, *p.HistoryStart
				}
			}
		}()
	}
	sizes := []int{100, 500, 360, 50}
	for i := range 10000 {
		at := histT0.Add(time.Duration(i) * iv)
		s := fullSample(at, float64(i))
		s.Interfaces = []histEntity{{Name: fmt.Sprintf("eth%d", i/2500), Device: true, A: fp(1), B: fp(2)}}
		h.Append(s)
		last.Store(at.UnixMilli())
		if i%1000 == 999 {
			h.Resize(sizes[(i/1000)%len(sizes)], iv)
		}
	}
	close(done)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TC-HP-133: A full 24 h history under repeated appends and full-window queries keeps the heap under 32 MiB. Owner WI 3.2. Requirements: NFR-2, FR-13.
//
// - Setup:
//   - Finding 6 adds this test, and Finding 16 gives it the architecture's WI 3.2 shape: history only, with no sampler and no handlers, so its owner can pass it. It is the T0 stand-in for NFR-2's RSS bound, which TC-HP-60's live-heap bound does not cover. TC-HP-134 keeps the sampler-plus-handlers shape under WI 4.5.
//   - `debug.SetGCPercent(100)` (restored in cleanup), which models the default `GOGC`.
//   - Baseline: `runtime.GC()`, `debug.FreeOSMemory()`, then `runtime.ReadMemStats`.
//
// - Steps:
//  1. Build a history for 24 h at 2 s (k = 2, capacity 21,600; A-40) with 32 interfaces and 32 disks. Fill it to 21,600 stored points (43,200 appends, every value present).
//  2. Run 100 rounds of one `Append` followed by one `Query(window 86400, step 0, [cpu, load, memory, swap, network, disk])`.
//  3. Read `runtime.ReadMemStats` after each round, with no forced GC. Separately, measure the `TotalAlloc` delta of one full-window query.
//
// - Expected:
//   - Every reading of `HeapSys − HeapReleased` is below baseline + 32 MiB. The maximum is logged with `t.Logf`, so WI 5.4 can compare it with TC-HP-130's live RSS.
//   - One full-window query allocates under 1 MiB (`TotalAlloc` delta), so a query creates no ring-sized garbage (WI 3.2).
//   - The ring still holds 21,600 points at the end.
func TestTCHP133_AFull24hHistoryUnderRepeatedQueriesKeeps(t *testing.T) {
	const heapBound = 32 << 20 // NFR-2
	const queryBound = 1 << 20
	old := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(old) })

	var ms runtime.MemStats
	runtime.GC()
	debug.FreeOSMemory()
	runtime.ReadMemStats(&ms)
	baseline := ms.HeapSys - ms.HeapReleased

	iv := 2 * time.Second
	h := newHistory(historySamples(24*time.Hour, iv), iv)
	last := fillFull(h)
	s := fullSample(last, 2)
	s.Interfaces = histEntities("eth", spec.MaxHistoryInterfaces, true, fp(1e6))
	s.Disks = histEntities("sd", spec.MaxHistoryDisks, true, fp(2e6))

	var peak uint64
	for i := range 100 {
		s.At = last.Add(time.Duration(i+1) * iv)
		h.Append(s)
		p := h.Query(s.At, 86400, 0, spec.MetricNames)
		if len(p.Series)*p.Points > spec.MaxResponseValues {
			t.Fatalf("round %d: %d values", i, len(p.Series)*p.Points)
		}
		runtime.ReadMemStats(&ms)
		inUse := ms.HeapSys - ms.HeapReleased
		peak = max(peak, inUse)
		if inUse >= baseline+heapBound {
			t.Errorf("round %d: HeapSys − HeapReleased %d ≥ baseline %d + 32 MiB", i, inUse, baseline)
		}
	}
	t.Logf("max HeapSys − HeapReleased %d bytes (%.2f MiB), baseline %d bytes (%.2f MiB), delta %.2f MiB",
		peak, float64(peak)/(1<<20), baseline, float64(baseline)/(1<<20), float64(peak-baseline)/(1<<20))

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	p := h.Query(s.At, 86400, 0, spec.MetricNames)
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("one full-window query: %d series × %d points, %d bytes allocated", len(p.Series), p.Points, alloc)
	if alloc >= queryBound {
		t.Errorf("one full-window query allocated %d bytes ≥ 1 MiB", alloc)
	}
	if n := len(stored(h)); n != spec.MaxHistoryPoints {
		t.Errorf("ring holds %d points at the end, want %d", n, spec.MaxHistoryPoints)
	}
	runtime.KeepAlive(h)
}

// TC-HP-136: A pair-averaged point keeps the values either sample has. Owner WI 3.2. Requirements: FR-13, FR-12, NFR-2.
//
// - Setup: an empty history for 24 h at 2 s (k = 2; A-40), `T0 = 2026-01-01T00:00:00Z`.
// - Steps:
//  1. Append sample A at T0 + 2 s: cpuPercent 10, load1 1.0, memUsed null, memAvailable 4e9, swapUsed null; interface `eth0` rx 100 B/s; `eth1` absent.
//  2. `Query(now = T0 + 2 s, window 3600, step 4)`.
//  3. Append sample B at T0 + 4 s: cpuPercent 20, load1 null, memUsed 1e9, memAvailable 6e9, swapUsed null; `eth0` absent; `eth1` rx 300 B/s.
//  4. `Query(now = T0 + 4 s, window 3600, step 4, [cpu, load, memory, swap, network])`.
//
// - Expected:
//   - Step 2: no stored point yet. A single pending sample is not committed (WI 3.2: `Append` commits after k samples), so every bucket is null.
//   - Step 4: exactly one stored point, with B's timestamp (T0 + 4 s). Its values are cpuPercent 15, load1 1.0, memUsed 1e9, memAvailable 5e9, swapUsed null, `eth0` rx 100 and `eth1` rx 300. Each value is the mean of the samples that have it, and null only where neither has it. Values are compared as float32 within a relative 1e-6.
//   - The null is JSON `null` in `values` and passes `assertExplicit` (the `series[].values` exemption).
func TestTCHP136_APairAveragedPointKeepsTheValues(t *testing.T) {
	iv := 2 * time.Second
	h := newHistory(historySamples(24*time.Hour, iv), iv)
	if h.k != 2 {
		t.Fatalf("k = %d, want 2", h.k)
	}
	a := histSample{
		At: histT0.Add(2 * time.Second), CPUPercent: fp(10), Load1: fp(1.0), MemAvailable: fp(4e9),
		Interfaces: []histEntity{{Name: "eth0", Device: true, A: fp(100)}},
	}
	h.Append(a)
	p := h.Query(histT0.Add(2*time.Second), 3600, 4, nil)
	if n := len(stored(h)); n != 0 {
		t.Fatalf("step 2: %d stored points, want 0", n)
	}
	if p.HistoryStart != nil {
		t.Errorf("step 2: historyStart %v, want null", p.HistoryStart)
	}
	for _, s := range p.Series {
		for i, v := range s.Values {
			if v != nil {
				t.Errorf("step 2: %s/%s bucket %d = %v, want null", s.Metric, s.Name, i, *v)
			}
		}
	}

	b := histSample{
		At: histT0.Add(4 * time.Second), CPUPercent: fp(20), MemUsed: fp(1e9), MemAvailable: fp(6e9),
		Interfaces: []histEntity{{Name: "eth1", Device: true, A: fp(300)}},
	}
	h.Append(b)
	pts := stored(h)
	if len(pts) != 1 || !pts[0].at.Equal(b.At) {
		t.Fatalf("step 4: stored %v, want one point at %v", pts, b.At)
	}
	now := histT0.Add(4 * time.Second)
	p = h.Query(now, 3600, 4, []string{spec.MetricCPU, spec.MetricLoad, spec.MetricMemory, spec.MetricSwap, spec.MetricNetwork})
	idx := int(now.Sub(p.From) / (time.Duration(p.StepSeconds) * time.Second))
	value := func(metric, name, entity string) *float64 {
		v := findSeries(t, p, metric, name, entity).Values
		if idx >= len(v) {
			t.Fatalf("%s/%s/%s has %d values, bucket %d", metric, name, entity, len(v), idx)
		}
		return v[idx]
	}
	want := []struct {
		metric, name, entity string
		v                    *float64
	}{
		{spec.MetricCPU, spec.SeriesUsagePercent, "", fp(15)},
		{spec.MetricLoad, spec.SeriesLoad1, "", fp(1.0)},
		{spec.MetricMemory, spec.SeriesUsedBytes, "", fp(1e9)},
		{spec.MetricMemory, spec.SeriesAvailableBytes, "", fp(5e9)},
		{spec.MetricSwap, spec.SeriesUsedBytes, "", nil},
		{spec.MetricNetwork, spec.SeriesRxBytesPerSecond, "eth0", fp(100)},
		{spec.MetricNetwork, spec.SeriesRxBytesPerSecond, "eth1", fp(300)},
	}
	for _, w := range want {
		got := value(w.metric, w.name, w.entity)
		switch {
		case w.v == nil && got != nil:
			t.Errorf("%s/%s/%s = %v, want null", w.metric, w.name, w.entity, *got)
		case w.v != nil && (got == nil || !relClose(*got, *w.v)):
			t.Errorf("%s/%s/%s = %v, want %v", w.metric, w.name, w.entity, got, *w.v)
		}
	}
	raw, err := json.Marshal(findSeries(t, p, spec.MetricSwap, spec.SeriesUsedBytes, "").Values)
	if err != nil || !strings.Contains(string(raw), "null") {
		t.Errorf("swap values JSON %s (%v), want a null", raw, err)
	}
	checkHistoryExplicit(t, p)
}

// TC-HP-137: A resize across storage resolutions keeps the old points and buckets both resolutions correctly. Owner WI 3.2. Requirements: FR-13, NFR-2.
//
// - Setup: a history for 12 h at 2 s (k = 1, capacity 21,600; A-40), `T0 = 2026-01-01T00:00:00Z`.
// - Steps:
//  1. Append 600 samples at T0 + 2 s·i (i = 1..600), cpuPercent = i.
//  2. `Resize` to 24 h at 2 s (k = 2, capacity 21,600).
//  3. Append 600 samples at T0 + 1200 s + 2 s·j (j = 1..600), cpuPercent = 600 + j.
//  4. `Query(now = T0 + 2400 s, window 3600, step 60, [cpu])`.
//  5. `Query(now = T0 + 2400 s, window 1000, step 0, [cpu])`, a window inside the k = 2 span.
//
// - Expected:
//   - The ring holds 900 points: the first 600 at their 2 s resolution, then 300 pair means, each with the later sample's timestamp (the mean of cpuPercent `600 + 2m − 1` and `600 + 2m`). The test computes this stored-point list from A-40's rule.
//   - Step 4: each bucket equals the mean of the stored points whose timestamps fall in it, computed in-test from that stored-point list rather than from the raw samples. This includes the bucket that straddles T0 + 1200 s.
//   - Step 5: `stepSeconds` is 4 (the stored resolution, k × 2 s; A-40), and no bucket in the window is null.
func TestTCHP137_AResizeAcrossStorageResolutionsKeepsThe(t *testing.T) {
	iv := 2 * time.Second
	h := newHistory(historySamples(12*time.Hour, iv), iv)
	type pt struct {
		at  time.Time
		cpu float64
	}
	var want []pt
	for i := 1; i <= 600; i++ {
		at := histT0.Add(time.Duration(i) * iv)
		h.Append(fullSample(at, float64(i)))
		want = append(want, pt{at, float64(i)})
	}
	h.Resize(historySamples(24*time.Hour, iv), iv)
	if h.k != 2 || h.capacity != 21600 {
		t.Fatalf("after resize k %d capacity %d, want 2 and 21600", h.k, h.capacity)
	}
	for j := 1; j <= 600; j++ {
		h.Append(fullSample(histT0.Add(1200*time.Second+time.Duration(j)*iv), float64(600+j)))
	}
	for m := 1; m <= 300; m++ {
		want = append(want, pt{histT0.Add(1200*time.Second + time.Duration(2*m)*iv), float64(600+2*m-1+600+2*m) / 2})
	}
	pts := stored(h)
	if len(pts) != len(want) {
		t.Fatalf("ring holds %d points, want %d", len(pts), len(want))
	}
	for i, w := range want {
		if !pts[i].at.Equal(w.at) || !relClose(float64(pts[i].agg[aggCPU]), w.cpu) {
			t.Fatalf("point %d is (%v, %v), want (%v, %v)", i, pts[i].at, pts[i].agg[aggCPU], w.at, w.cpu)
		}
	}

	now := histT0.Add(2400 * time.Second)
	p := h.Query(now, 3600, 60, []string{spec.MetricCPU})
	if p.StepSeconds != 60 {
		t.Fatalf("step 4: stepSeconds %d, want 60", p.StepSeconds)
	}
	straddle := false
	for i, v := range p.Series[0].Values {
		lo, hi := p.From.Add(time.Duration(i)*time.Minute), p.From.Add(time.Duration(i+1)*time.Minute)
		var sum float64
		var n int
		for _, w := range want {
			if !w.at.Before(lo) && w.at.Before(hi) {
				sum += w.cpu
				n++
			}
		}
		if b := histT0.Add(1200 * time.Second); !b.Before(lo) && b.Before(hi) {
			straddle = true
		}
		switch {
		case n == 0 && v != nil:
			t.Errorf("step 4 bucket %d = %v, want null", i, *v)
		case n > 0 && (v == nil || !relClose(*v, sum/float64(n))):
			t.Errorf("step 4 bucket %d = %v, want %v", i, v, sum/float64(n))
		}
	}
	if !straddle {
		t.Errorf("no bucket covers T0 + 1200 s")
	}

	p = h.Query(now, 1000, 0, []string{spec.MetricCPU})
	if p.StepSeconds != 4 {
		t.Errorf("step 5: stepSeconds %d, want 4 (k × 2 s)", p.StepSeconds)
	}
	for i, v := range p.Series[0].Values {
		if v == nil {
			t.Errorf("step 5: bucket %d at %v is null", i, p.From.Add(time.Duration(i*p.StepSeconds)*time.Second))
		}
	}
}

// TC-HP-138: A resize from k = 2 to k = 1 with a pending sample commits it and history keeps recording. Owner WI 3.2. Requirements: FR-13, NFR-2.
//
// - Setup: a history for 24 h at 2 s (k = 2), T0 = 2026-01-01T00:00:00Z.
// - Steps: append 601 samples (one pending); Resize to 12 h at 2 s (k = 1); append 10 more; Query over the boundary.
// - Expected: 300 pair means, then sample 601 alone at its own timestamp, then 10 single points; newest stored point is sample 611; no null bucket across the boundary.
func TestTCHP138_AResizeFromK2ToK1WithAPendingSample(t *testing.T) {
	iv := 2 * time.Second
	at := func(i int) time.Time { return histT0.Add(time.Duration(i) * iv) }
	h := newHistory(historySamples(24*time.Hour, iv), iv)
	for i := 1; i <= 601; i++ {
		h.Append(fullSample(at(i), float64(i)))
	}
	h.Resize(historySamples(12*time.Hour, iv), iv)
	if h.k != 1 {
		t.Fatalf("after resize k = %d, want 1", h.k)
	}
	for i := 602; i <= 611; i++ {
		h.Append(fullSample(at(i), float64(i)))
	}
	type pt struct {
		at  time.Time
		cpu float64
	}
	var want []pt
	for m := 1; m <= 300; m++ {
		want = append(want, pt{at(2 * m), float64(2*m-1+2*m) / 2})
	}
	want = append(want, pt{at(601), 601})
	for i := 602; i <= 611; i++ {
		want = append(want, pt{at(i), float64(i)})
	}
	pts := stored(h)
	if len(pts) != 311 {
		t.Fatalf("ring holds %d points, want 311", len(pts))
	}
	for i, w := range want {
		if !pts[i].at.Equal(w.at) || !relClose(float64(pts[i].agg[aggCPU]), w.cpu) {
			t.Fatalf("point %d is (%v, %v), want (%v, %v)", i, pts[i].at, pts[i].agg[aggCPU], w.at, w.cpu)
		}
	}
	if newest := pts[len(pts)-1]; newest.agg[aggCPU] != 611 || !newest.at.Equal(at(611)) {
		t.Errorf("newest stored point (%v, %v), want sample 611", newest.at, newest.agg[aggCPU])
	}

	p := h.Query(histT0.Add(1224*time.Second), 3600, 0, []string{spec.MetricCPU})
	lo, hi := histT0.Add(1202*time.Second), histT0.Add(1222*time.Second)
	step := time.Duration(p.StepSeconds) * time.Second
	checked := 0
	for i, v := range p.Series[0].Values {
		bs, be := p.From.Add(time.Duration(i)*step), p.From.Add(time.Duration(i+1)*step)
		if be.After(lo) && !bs.After(hi) { // the bucket overlaps [T0+1202 s, T0+1222 s]
			checked++
			if v == nil {
				t.Errorf("bucket %d [%v, %v) is null", i, bs, be)
			}
		}
	}
	if checked == 0 {
		t.Errorf("no bucket covers T0 + 1202 s .. T0 + 1222 s (from %v step %v)", p.From, step)
	}
	if math.IsNaN(float64(pts[300].agg[aggCPU])) {
		t.Errorf("sample 601's point is null")
	}
}
