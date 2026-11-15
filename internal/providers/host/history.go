package host

import (
	"cmp"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// histSample is one sampler reading as history records it. A nil value is
// one the sampler could not produce (a failed source, warming up), and is
// stored as null, never 0 (FR-12).
type histSample struct {
	At                                                 time.Time
	CPUPercent, Load1, MemUsed, MemAvailable, SwapUsed *float64
	Interfaces, Disks                                  []histEntity
}

// histEntity is one interface's or disk's rates in a sample. A and B are rx
// and tx bytes per second for an interface, read and write for a disk.
type histEntity struct {
	Name string
	// Device ranks the entity for a history slot: an interface with
	// /sys/class/net/<if>/device, or a disk that is not loop or dm.
	Device bool
	A, B   *float64
}

// Aggregate indexes of a stored point.
const (
	aggCPU = iota
	aggLoad1
	aggMemUsed
	aggMemAvailable
	aggSwapUsed
	numAgg
)

// Entity kinds: the index into history.ents.
const (
	entIface = iota
	entDisk
	numEnt
)

// null is a stored value that no sample had.
var null = float32(math.NaN())

// historySamples is n, the samples historyDuration covers at
// sampleInterval, at least 1.
func historySamples(duration, interval time.Duration) int {
	if interval <= 0 {
		return 1
	}
	return max(1, int((duration+interval-1)/interval))
}

// historyShape is the storage shape for n samples (A-40): one stored point
// per k samples, k = ceil(n / MaxHistoryPoints), and capacity ceil(n / k).
func historyShape(n int) (k, capacity int) {
	n = max(n, 1)
	k = ceilDiv(n, spec.MaxHistoryPoints)
	return k, ceilDiv(n, k)
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// mean accumulates one value over the samples of a pending point.
type mean struct {
	sum float64
	n   int
}

func (m *mean) add(v *float64) {
	if v != nil && !math.IsNaN(*v) {
		m.sum += *v
		m.n++
	}
}

// value is the mean of the samples that had the value, or null if none did.
func (m mean) value() float32 {
	if m.n == 0 {
		return null
	}
	return float32(m.sum / float64(m.n))
}

// pendingEntity is one entity's accumulation in the pending point.
type pendingEntity struct {
	device bool
	a, b   mean
}

// slot is one entity's history: two float32 rings aligned with the
// aggregate ring (8 bytes per point, A-27).
type slot struct {
	name     string
	lastSeen uint64 // seq of the newest stored point the entity is in
	a, b     []float32
}

// entityRings holds the slots of one entity kind, at most max of them,
// allocated only for entities seen (A-27).
type entityRings struct {
	max     int
	slots   []*slot // sorted by name
	byName  map[string]*slot
	pending map[string]pendingEntity
	denied  int      // entities of the newest stored point with no slot
	fresh   []string // scratch: pending entities without a slot
}

func newEntityRings(max int) entityRings {
	return entityRings{
		max:     max,
		byName:  make(map[string]*slot, max),
		pending: make(map[string]pendingEntity),
	}
}

func (e *entityRings) add(es []histEntity) {
	for _, x := range es {
		p := e.pending[x.Name]
		p.device = p.device || x.Device
		p.a.add(x.A)
		p.b.add(x.B)
		e.pending[x.Name] = p
	}
}

func (e *entityRings) free(s *slot) {
	delete(e.byName, s.name)
	if i, ok := slices.BinarySearchFunc(e.slots, s.name, func(s *slot, n string) int { return cmp.Compare(s.name, n) }); ok {
		e.slots = slices.Delete(e.slots, i, i+1)
	}
}

// commit writes the pending entities into the stored point at pos, whose
// seq is seq. oldest is the seq of the oldest point held after the commit.
// An entity in no held point frees its slot first (unseen for a full
// window); then pending entities without a slot take free ones, devices
// first, then by name. The rest are denied.
func (e *entityRings) commit(pos int, seq, oldest uint64, capacity int) {
	for i := len(e.slots) - 1; i >= 0; i-- {
		s := e.slots[i]
		if _, seen := e.pending[s.name]; !seen && s.lastSeen < oldest {
			e.free(s)
		}
	}
	for _, s := range e.slots {
		s.a[pos], s.b[pos] = null, null
	}
	e.fresh = e.fresh[:0]
	for name, p := range e.pending {
		s, ok := e.byName[name]
		if !ok {
			e.fresh = append(e.fresh, name)
			continue
		}
		s.a[pos], s.b[pos], s.lastSeen = p.a.value(), p.b.value(), seq
	}
	slices.SortFunc(e.fresh, func(x, y string) int {
		if dx, dy := e.pending[x].device, e.pending[y].device; dx != dy {
			if dx {
				return -1
			}
			return 1
		}
		return cmp.Compare(x, y)
	})
	e.denied = 0
	for _, name := range e.fresh {
		if len(e.slots) >= e.max {
			e.denied++
			continue
		}
		p := e.pending[name]
		s := &slot{name: name, lastSeen: seq, a: nullRing(capacity), b: nullRing(capacity)}
		s.a[pos], s.b[pos] = p.a.value(), p.b.value()
		e.byName[name] = s
		i, _ := slices.BinarySearchFunc(e.slots, name, func(s *slot, n string) int { return cmp.Compare(s.name, n) })
		e.slots = slices.Insert(e.slots, i, s)
	}
	clear(e.pending)
	clear(e.fresh) // drop the name references; the length is reset on the next commit
}

func nullRing(n int) []float32 {
	r := make([]float32, n)
	for i := range r {
		r[i] = null
	}
	return r
}

// history is the in-memory metrics history (FR-13): a fixed-capacity ring
// of stored points, each a Unix-ms timestamp plus float32 aggregates, and
// sparse per-entity rings for at most 32 interfaces and 32 disks. Memory is
// 28 bytes per point plus 8 per point per held slot (NFR-2, A-27, A-40). The
// sampler appends every sample; one point is stored per k samples.
//
// One mutex covers append, resize and query. Query buckets under the lock
// without copying the ring, so its garbage is bounded by its output.
type history struct {
	mu       sync.Mutex
	interval time.Duration
	k        int
	capacity int

	ts    []int64 // Unix ms of each stored point
	agg   [numAgg][]float32
	head  int    // ring index of the oldest stored point
	count int    // stored points held
	seq   uint64 // points committed since creation

	pendN   int
	pendAt  int64
	pendAgg [numAgg]mean

	ents [numEnt]entityRings
}

// newHistory sizes a history for n samples taken every interval.
func newHistory(n int, interval time.Duration) *history {
	h := &history{interval: interval}
	h.k, h.capacity = historyShape(n)
	h.ts = make([]int64, h.capacity)
	for a := range h.agg {
		h.agg[a] = make([]float32, h.capacity)
	}
	h.ents[entIface] = newEntityRings(spec.MaxHistoryInterfaces)
	h.ents[entDisk] = newEntityRings(spec.MaxHistoryDisks)
	return h
}

// Append accumulates s into the pending point and commits the point once it
// holds at least k samples (never exactly k, so a change of k cannot freeze
// the ring).
func (h *history) Append(s histSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pendAt = s.At.UnixMilli()
	for a, v := range [numAgg]*float64{s.CPUPercent, s.Load1, s.MemUsed, s.MemAvailable, s.SwapUsed} {
		h.pendAgg[a].add(v)
	}
	h.ents[entIface].add(s.Interfaces)
	h.ents[entDisk].add(s.Disks)
	h.pendN++
	if h.pendN >= h.k {
		h.commit()
	}
}

// commit stores the pending point: each value the mean of the samples that
// had it, at the timestamp of the last sample. It overwrites the oldest
// point when the ring is full.
func (h *history) commit() {
	pos := (h.head + h.count) % h.capacity
	if h.count == h.capacity {
		h.head = (h.head + 1) % h.capacity
	} else {
		h.count++
	}
	h.ts[pos] = h.pendAt
	for a := range h.agg {
		h.agg[a][pos] = h.pendAgg[a].value()
	}
	oldest := h.seq + 1 - uint64(h.count)
	for e := range h.ents {
		h.ents[e].commit(pos, h.seq, oldest, h.capacity)
	}
	h.seq++
	h.pendN = 0
	h.pendAgg = [numAgg]mean{}
}

// Resize resizes the history for n samples taken every interval. It first
// commits any pending partial point (the mean of its samples, at its last
// sample's timestamp), then recomputes k and the capacity and keeps the
// newest stored points at their original resolution. Query buckets by
// timestamp, so mixed resolutions are bucketed correctly.
func (h *history) Resize(n int, interval time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pendN > 0 {
		h.commit()
	}
	h.interval = interval
	k, capacity := historyShape(n)
	h.k = k
	if capacity == h.capacity {
		return
	}
	keep := min(h.count, capacity)
	skip := h.count - keep
	move := func(old []float32) []float32 {
		r := make([]float32, capacity)
		for i := range keep {
			r[i] = old[(h.head+skip+i)%h.capacity]
		}
		return r
	}
	ts := make([]int64, capacity)
	for i := range keep {
		ts[i] = h.ts[(h.head+skip+i)%h.capacity]
	}
	h.ts = ts
	for a := range h.agg {
		h.agg[a] = move(h.agg[a])
	}
	oldest := h.seq - uint64(keep)
	for e := range h.ents {
		er := &h.ents[e]
		for i := len(er.slots) - 1; i >= 0; i-- {
			s := er.slots[i]
			if s.lastSeen < oldest {
				er.free(s)
				continue
			}
			s.a, s.b = move(s.a), move(s.b)
		}
	}
	h.head, h.count, h.capacity = 0, keep, capacity
}

// seriesSource is one series of a query and the ring it is read from.
type seriesSource struct {
	metric, name, entity, unit string
	ring                       []float32
}

// sources lists the series of the requested metrics in the canonical metric
// order; no metrics means spec.DefaultMetrics (A-26). Network and disk give
// two series per held entity, by name.
func (h *history) sources(metrics []string) []seriesSource {
	if len(metrics) == 0 {
		metrics = spec.DefaultMetrics
	}
	var out []seriesSource
	entity := func(metric string, kind int, a, b string) {
		for _, s := range h.ents[kind].slots {
			out = append(out,
				seriesSource{metric, a, s.name, spec.UnitBytesPerSecond, s.a},
				seriesSource{metric, b, s.name, spec.UnitBytesPerSecond, s.b})
		}
	}
	for _, m := range spec.MetricNames {
		if !slices.Contains(metrics, m) {
			continue
		}
		switch m {
		case spec.MetricCPU:
			out = append(out, seriesSource{m, spec.SeriesUsagePercent, "", spec.UnitPercent, h.agg[aggCPU]})
		case spec.MetricLoad:
			out = append(out, seriesSource{m, spec.SeriesLoad1, "", spec.UnitLoad, h.agg[aggLoad1]})
		case spec.MetricMemory:
			out = append(out,
				seriesSource{m, spec.SeriesUsedBytes, "", spec.UnitBytes, h.agg[aggMemUsed]},
				seriesSource{m, spec.SeriesAvailableBytes, "", spec.UnitBytes, h.agg[aggMemAvailable]})
		case spec.MetricSwap:
			out = append(out, seriesSource{m, spec.SeriesUsedBytes, "", spec.UnitBytes, h.agg[aggSwapUsed]})
		case spec.MetricNetwork:
			entity(m, entIface, spec.SeriesRxBytesPerSecond, spec.SeriesTxBytesPerSecond)
		case spec.MetricDisk:
			entity(m, entDisk, spec.SeriesReadBytesPerSecond, spec.SeriesWriteBytesPerSecond)
		}
	}
	return out
}

// metricsGrid is the step grid of a metrics answer, shared by Query and the
// handler's answer without a run: the step is raised to at least
// ceil(window / maxPoints), then further until the grid from start (aligned
// down to the step) to now holds at most maxPoints points. It returns the
// aligned start, the step and the point count.
func metricsGrid(nowMs, startMs int64, windowSeconds, step, maxPoints int) (fromMs int64, stepOut, points int) {
	step = max(step, ceilDiv(windowSeconds, maxPoints))
	for {
		stepMs := int64(step) * 1000
		fromMs = floorDiv(startMs, stepMs) * stepMs
		points = 0
		if nowMs >= fromMs {
			points = int((nowMs-fromMs)/stepMs) + 1
		}
		if points <= maxPoints {
			return fromMs, step, points
		}
		step++
	}
}

// Query returns the requested metrics over windowSeconds before now, as
// MetricsProperties without sampler, which the handler fills.
//
//   - from is max(now − window, oldest) aligned down to the step;
//   - the step is the requested step (0: the stored resolution, k × the
//     sample interval) raised to at least ceil(window / 500), then further
//     until every series holds at most 500 values and the response at most
//     20,000;
//   - each value is the mean of the stored points in its step, null when
//     there are none.
//
// An empty history answers each series with no values and historyStart null
// with the reason warming up.
func (h *history) Query(now time.Time, windowSeconds, stepSeconds int, metrics []string) spec.MetricsProperties {
	if windowSeconds <= 0 {
		windowSeconds = spec.DefaultWindowSeconds
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	src := h.sources(metrics)
	step := stepSeconds
	if step <= 0 {
		step = max(1, int((time.Duration(h.k)*h.interval+time.Second-1)/time.Second))
	}
	maxPoints := spec.MaxSeriesPoints
	if len(src) > 0 {
		maxPoints = max(1, min(maxPoints, spec.MaxResponseValues/len(src)))
	}

	nowMs := now.UnixMilli()
	start := nowMs - int64(windowSeconds)*1000
	var oldestMs int64
	if h.count > 0 {
		oldestMs = h.ts[h.head]
		start = max(start, oldestMs)
	}
	fromMs, step, points := metricsGrid(nowMs, start, windowSeconds, step, maxPoints)
	stepMs := int64(step) * 1000

	p := spec.MetricsProperties{
		From:        time.UnixMilli(fromMs).UTC(),
		StepSeconds: step,
		EntitiesTruncated: spec.EntitiesTruncated{
			Interfaces: h.ents[entIface].denied,
			Disks:      h.ents[entDisk].denied,
		},
		Series: make(spec.Array[spec.Series], len(src)),
	}
	if h.count == 0 {
		points = 0
		p.Unavailable = spec.Unavailable{"historyStart": reasonWarmingUp}
	} else {
		start := time.UnixMilli(oldestMs).UTC()
		p.HistoryStart = &start
	}
	p.Points = points

	// One backing array each for the sums and the value pointers: the
	// allocation is the output's size, never the ring's.
	total := len(src) * points
	sums := make([]float64, total)
	counts := make([]int32, total)
	ptrs := make([]*float64, total)
	if points > 0 {
		for i := range h.count {
			pos := (h.head + i) % h.capacity
			ts := h.ts[pos]
			if ts < fromMs || ts > nowMs {
				continue
			}
			b := int((ts - fromMs) / stepMs)
			for j := range src {
				v := src[j].ring[pos]
				if math.IsNaN(float64(v)) {
					continue
				}
				sums[j*points+b] += float64(v)
				counts[j*points+b]++
			}
		}
		for i, n := range counts {
			if n > 0 {
				sums[i] /= float64(n)
				ptrs[i] = &sums[i]
			}
		}
	}
	for j, s := range src {
		p.Series[j] = spec.Series{
			Metric: s.metric, Name: s.name, Entity: s.entity, Unit: s.unit,
			Values: ptrs[j*points : (j+1)*points : (j+1)*points],
		}
	}
	return p
}

// floorDiv is a / b rounded toward negative infinity, for b > 0.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}
