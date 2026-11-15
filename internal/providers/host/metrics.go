package host

import (
	"context"
	"slices"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// metrics is host.metrics.get (FR-13). It refuses an input the schema
// refuses (stepSeconds 1 among them) before touching the history, then
// queries the current run's history; without a run it answers no series.
func (p *Provider) metrics(_ context.Context, in spec.MetricsInput) (apitypes.List[spec.Metrics], error) {
	if err := checkMetricsInput(in); err != nil {
		return apitypes.List[spec.Metrics]{}, err
	}
	run := p.run.Load()
	return currentList(p, spec.MetricsType, p.collectMetrics(run.history(), run.snapshot(), p.now(), in)), nil
}

// checkMetricsInput is the input schema's bounds (spec.refineMetrics),
// checked again in the handler, so a call that bypasses the dispatcher's
// validation is refused the same way, naming the field.
func checkMetricsInput(in spec.MetricsInput) *problem.Error {
	switch {
	case in.WindowSeconds != 0 && (in.WindowSeconds < spec.MinWindowSeconds || in.WindowSeconds > spec.MaxWindowSeconds):
		return invalidMetrics("/windowSeconds", "must be 60 to 86400")
	case in.StepSeconds < 0 || in.StepSeconds > spec.MaxStepSeconds || (in.StepSeconds > 0 && in.StepSeconds < spec.MinStepSeconds):
		return invalidMetrics("/stepSeconds", "must be 0 or 2 to 3600")
	case len(in.Metric) > len(spec.MetricNames):
		return invalidMetrics("/metric", "names at most 6 metrics")
	}
	for i, m := range in.Metric {
		if !slices.Contains(spec.MetricNames, m) || slices.Contains(in.Metric[:i], m) {
			return invalidMetrics("/metric", "must name each of cpu, load, memory, swap, network and disk at most once")
		}
	}
	return nil
}

func invalidMetrics(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}

// collectMetrics is the history h over the request with the sampler object
// of snap, both from one run read at now. Without a run (h nil) there is no
// history: no series and no points, historyStart null with not running, on
// the grid an empty history of the current settings would give.
func (p *Provider) collectMetrics(h *history, snap *snapshot, now time.Time, in spec.MetricsInput) spec.MetricsProperties {
	var out spec.MetricsProperties
	if h != nil {
		out = h.Query(now, in.WindowSeconds, in.StepSeconds, in.Metric)
	} else {
		out = p.noHistory(now, in)
	}
	out.Sampler = snap.sampler(now)
	return out
}

// noHistory is the metrics answer without a run. Its grid follows
// history.Query's rule for an empty history: the step is the requested one,
// or the stored resolution of the current settings, raised to keep the
// window within spec.MaxSeriesPoints; from is now minus the window, aligned
// down to the step.
func (p *Provider) noHistory(now time.Time, in spec.MetricsInput) spec.MetricsProperties {
	window := in.WindowSeconds
	if window <= 0 {
		window = spec.DefaultWindowSeconds
	}
	step := in.StepSeconds
	if step <= 0 {
		s := p.Settings()
		k, _ := historyShape(historySamples(s.HistoryDuration, s.SampleInterval))
		step = max(1, int((time.Duration(k)*s.SampleInterval+time.Second-1)/time.Second))
	}
	nowMs := now.UnixMilli()
	fromMs, step, _ := metricsGrid(nowMs, nowMs-int64(window)*1000, window, step, spec.MaxSeriesPoints)
	return spec.MetricsProperties{
		From:        time.UnixMilli(fromMs).UTC(),
		StepSeconds: step,
		Series:      spec.Array[spec.Series]{},
		Unavailable: spec.Unavailable{"historyStart": reasonNotRunning},
	}
}
