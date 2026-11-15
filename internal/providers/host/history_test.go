package host

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// histT0 is the T0 of the history TCs, 2026-01-01T00:00:00Z.
var histT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func fp(v float64) *float64 { return &v }

// fullSample is a sample at at with every aggregate present, derived from
// cpu so a stored point identifies the samples it averages.
func fullSample(at time.Time, cpu float64) histSample {
	return histSample{
		At:           at,
		CPUPercent:   fp(cpu),
		Load1:        fp(cpu / 100),
		MemUsed:      fp(1e9 + cpu),
		MemAvailable: fp(4e9 - cpu),
		SwapUsed:     fp(1e6 + cpu),
	}
}

// histEntities names n entities prefix00.. (or prefix0.. when n ≤ 10), each
// with both rates at v.
func histEntities(prefix string, n int, device bool, v *float64) []histEntity {
	es := make([]histEntity, n)
	for i := range es {
		name := fmt.Sprintf("%s%d", prefix, i)
		if n > 10 {
			name = fmt.Sprintf("%s%02d", prefix, i)
		}
		es[i] = histEntity{Name: name, Device: device, A: v, B: v}
	}
	return es
}

// storedPoint is one stored point read back from the ring, oldest first.
type storedPoint struct {
	at  time.Time
	agg [numAgg]float32
}

func stored(h *history) []storedPoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]storedPoint, 0, h.count)
	for i := range h.count {
		pos := (h.head + i) % h.capacity
		p := storedPoint{at: time.UnixMilli(h.ts[pos]).UTC()}
		for a := range p.agg {
			p.agg[a] = h.agg[a][pos]
		}
		out = append(out, p)
	}
	return out
}

// storedEntity reads one held entity's rings, oldest first; ok is false
// when the entity holds no slot.
func storedEntity(h *history, kind int, name string) (a, b []float32, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.ents[kind].byName[name]
	if !ok {
		return nil, nil, false
	}
	for i := range h.count {
		pos := (h.head + i) % h.capacity
		a = append(a, s.a[pos])
		b = append(b, s.b[pos])
	}
	return a, b, true
}

// heldNames lists the entities of kind holding a slot, in slot order.
func heldNames(h *history, kind int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var names []string
	for _, s := range h.ents[kind].slots {
		names = append(names, s.name)
	}
	return names
}

// findSeries returns the series with the metric, name and entity.
func findSeries(t *testing.T, p spec.MetricsProperties, metric, name, entity string) spec.Series {
	t.Helper()
	for _, s := range p.Series {
		if s.Metric == metric && s.Name == name && s.Entity == entity {
			return s
		}
	}
	t.Fatalf("no series %s/%s/%q in %d series", metric, name, entity, len(p.Series))
	return spec.Series{}
}

// relClose compares as float32 within a relative 1e-6.
func relClose(got, want float64) bool {
	g, w := float64(float32(got)), float64(float32(want))
	if w == 0 {
		return math.Abs(g) < 1e-6
	}
	return math.Abs(g-w)/math.Abs(w) <= 1e-6
}

// checkHistoryExplicit is the history-level form of D-5 until the shared
// assertExplicit (WI 4.x explicit_test.go) exists: it walks the JSON of p
// and requires that every null field of the properties object is a key of
// its unavailable map with a non-empty reason, that each key names a null
// field, and that each series has no null field. series[].values is the one
// documented place a null may stand without a reason.
func checkHistoryExplicit(t *testing.T, p spec.MetricsProperties) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var un map[string]string
	if u, ok := obj["unavailable"]; ok {
		if err := json.Unmarshal(u, &un); err != nil {
			t.Fatalf("unavailable: %v", err)
		}
	}
	for k, v := range obj {
		if string(v) == "null" {
			if un[k] == "" {
				t.Errorf("%s is null with no reason in unavailable %v", k, un)
			}
		}
	}
	for k := range un {
		if string(obj[k]) != "null" {
			t.Errorf("unavailable names %s, which is %s, not null", k, obj[k])
		}
	}
	var series []map[string]json.RawMessage
	if err := json.Unmarshal(obj["series"], &series); err != nil {
		t.Fatalf("series: %v (%s)", err, obj["series"])
	}
	for i, s := range series {
		for k, v := range s {
			if string(v) == "null" {
				t.Errorf("series[%d].%s is null", i, k)
			}
		}
		if string(s["values"]) == "" || s["values"][0] != '[' {
			t.Errorf("series[%d].values is %s, not a list", i, s["values"])
		}
	}
}

// The storage shape over the whole configuration range: k is 1 or 2 and the
// capacity never exceeds MaxHistoryPoints (A-40).
func TestHistoryShapeWithinConfigBounds(t *testing.T) {
	for iv := spec.MinSampleInterval; iv <= spec.MaxSampleInterval; iv += time.Second {
		for d := iv; d <= spec.MaxHistoryDuration; d += 15 * time.Minute {
			n := historySamples(d, iv)
			k, c := historyShape(n)
			if k < 1 || k > 2 || c > spec.MaxHistoryPoints || c*k < n {
				t.Fatalf("%v at %v: n %d gives k %d capacity %d", d, iv, n, k, c)
			}
		}
	}
	if n := historySamples(time.Hour, 10*time.Second); n != 360 {
		t.Errorf("1h at 10s: n = %d, want 360", n)
	}
}

// A NaN from the sampler is a missing value, not a number to average.
func TestHistoryNaNInputIsNull(t *testing.T) {
	h := newHistory(historySamples(24*time.Hour, 2*time.Second), 2*time.Second)
	a := fullSample(histT0.Add(2*time.Second), 10)
	a.Load1 = fp(math.NaN())
	b := fullSample(histT0.Add(4*time.Second), 20)
	b.Load1 = nil
	h.Append(a)
	h.Append(b)
	pts := stored(h)
	if len(pts) != 1 {
		t.Fatalf("%d stored points, want 1", len(pts))
	}
	if v := pts[0].agg[aggLoad1]; !math.IsNaN(float64(v)) {
		t.Errorf("load1 = %v, want null", v)
	}
	if v := pts[0].agg[aggCPU]; v != 15 {
		t.Errorf("cpu = %v, want 15", v)
	}
}

// Within one sample a device outranks a non-device whose name sorts first,
// and a held slot is never taken from its entity by a later device.
func TestHistorySlotsDevicesFirstWithinOneSample(t *testing.T) {
	h := newHistory(10, 10*time.Second)
	s := fullSample(histT0, 1)
	s.Disks = append(histEntities("aloop", 32, false, fp(1)), histEntity{Name: "zdisk", Device: true, A: fp(2), B: fp(2)})
	h.Append(s)
	if _, _, ok := storedEntity(h, entDisk, "zdisk"); !ok {
		t.Errorf("device zdisk holds no slot; held %v", heldNames(h, entDisk))
	}
	if _, _, ok := storedEntity(h, entDisk, "aloop31"); ok {
		t.Errorf("aloop31, last by name of the non-devices, holds a slot")
	}
	p := h.Query(histT0, 3600, 0, []string{spec.MetricDisk})
	if p.EntitiesTruncated.Disks != 1 || p.EntitiesTruncated.Interfaces != 0 {
		t.Errorf("entitiesTruncated = %+v, want 0 interfaces 1 disk", p.EntitiesTruncated)
	}

	s2 := fullSample(histT0.Add(10*time.Second), 2)
	s2.Disks = append(histEntities("aloop", 32, false, fp(1)), histEntity{Name: "adev", Device: true})
	h.Append(s2)
	if _, _, ok := storedEntity(h, entDisk, "adev"); ok {
		t.Errorf("new device adev took a held slot")
	}
}

// A query whose now is before the oldest point answers zero points.
func TestHistoryQueryBeforeHistory(t *testing.T) {
	h := newHistory(360, 10*time.Second)
	h.Append(fullSample(histT0, 1))
	p := h.Query(histT0.Add(-time.Hour), 600, 0, nil)
	if p.Points != 0 {
		t.Errorf("points = %d, want 0", p.Points)
	}
	for _, s := range p.Series {
		if len(s.Values) != 0 {
			t.Errorf("%s/%s has %d values", s.Metric, s.Name, len(s.Values))
		}
	}
}

// Unknown and repeated metric names add no series, and the series follow
// the canonical metric order whatever the request order.
func TestHistoryQueryMetricOrderAndDuplicates(t *testing.T) {
	h := newHistory(360, 10*time.Second)
	h.Append(fullSample(histT0, 1))
	p := h.Query(histT0, 600, 0, []string{spec.MetricSwap, "bogus", spec.MetricCPU, spec.MetricSwap})
	var got []string
	for _, s := range p.Series {
		got = append(got, s.Metric+"/"+s.Name)
	}
	want := []string{"cpu/usagePercent", "swap/usedBytes"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("series %v, want %v", got, want)
	}
}
