// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// metricsGet calls the metrics handler and returns its properties, checked
// by oneItem (current, host tc, assertExplicit).
func (s *hostStack) metricsGet(in spec.MetricsInput) spec.MetricsProperties {
	s.t.Helper()
	out, err := s.p.metrics(context.Background(), in)
	return oneItem[spec.MetricsProperties](s.t, spec.MetricsType, out, err)
}

// setAdvanced makes F-FS serve the dynamic files from t1/ (true) or fs/
// (false), so ticks can alternate between the two samples.
func setAdvanced(f *fakeFS, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advanced = on
}

// wantValidationFailed checks err is validation-failed naming field.
func wantValidationFailed(t *testing.T, err error, field string) {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.ValidationFailed {
		t.Fatalf("err = %v, want validation-failed", err)
	}
	errs, _ := pe.Extensions["errors"].([]problem.FieldError)
	if len(errs) != 1 || errs[0].Path != "/"+field {
		t.Errorf("errors %v, want one naming /%s", pe.Extensions["errors"], field)
	}
}

// The handler refuses, itself, every input the schema refuses, naming the
// field, so a direct call can never reach the history with one (FR-13,
// FR-14); the boundaries pass.
func TestMetricsInputChecks(t *testing.T) {
	s := newHostStack(t, capUBU)
	for _, c := range []struct {
		in    spec.MetricsInput
		field string
	}{
		{spec.MetricsInput{StepSeconds: 1}, "stepSeconds"},
		{spec.MetricsInput{StepSeconds: -1}, "stepSeconds"},
		{spec.MetricsInput{StepSeconds: 3601}, "stepSeconds"},
		{spec.MetricsInput{WindowSeconds: 59}, "windowSeconds"},
		{spec.MetricsInput{WindowSeconds: -60}, "windowSeconds"},
		{spec.MetricsInput{WindowSeconds: 86401}, "windowSeconds"},
		{spec.MetricsInput{Metric: []string{"bogus"}}, "metric"},
		{spec.MetricsInput{Metric: []string{spec.MetricCPU, spec.MetricCPU}}, "metric"},
	} {
		_, err := s.p.metrics(context.Background(), c.in)
		wantValidationFailed(t, err, c.field)
	}
	for _, in := range []spec.MetricsInput{
		{}, {StepSeconds: 2}, {StepSeconds: 3600}, {WindowSeconds: 60}, {WindowSeconds: 86400},
		{Metric: spec.MetricNames},
	} {
		if _, err := s.p.metrics(context.Background(), in); err != nil {
			t.Errorf("%+v refused: %v", in, err)
		}
	}
}

// Without a run the answer is on the grid an empty history of the current
// settings would give (from aligned down to the step, the step the stored
// resolution raised to keep 500 points), with no series, no points and
// historyStart null with not running (FR-11, FR-13).
func TestMetricsWithoutARun(t *testing.T) {
	s := newHostStack(t, capUBU)
	now := s.clk.Now()
	for _, in := range []spec.MetricsInput{
		{}, {WindowSeconds: 86400}, {StepSeconds: 60, Metric: spec.MetricNames}, {WindowSeconds: 60, StepSeconds: 2},
	} {
		st := s.p.Settings()
		want := newHistory(historySamples(st.HistoryDuration, st.SampleInterval), st.SampleInterval).
			Query(now, in.WindowSeconds, in.StepSeconds, in.Metric)
		p := s.metricsGet(in)
		if p.StepSeconds != want.StepSeconds || !p.From.Equal(want.From) || p.Points != 0 || len(p.Series) != 0 ||
			p.HistoryStart != nil || p.Unavailable["historyStart"] != reasonNotRunning {
			t.Errorf("%+v: step %d from %v points %d series %d historyStart %v (%q); want step %d from %v",
				in, p.StepSeconds, p.From, p.Points, len(p.Series), p.HistoryStart, p.Unavailable["historyStart"],
				want.StepSeconds, want.From)
		}
		if p.Sampler.State != spec.SamplerNotRunning {
			t.Errorf("sampler.state %q", p.Sampler.State)
		}
	}
}

// A stale run still answers its history, with sampler stale (FR-11).
func TestMetricsStaleRunKeepsItsHistory(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ready()
	s.clk.Advance(31 * time.Second)
	p := s.metricsGet(spec.MetricsInput{})
	if p.Sampler.State != spec.SamplerStale || p.HistoryStart == nil || p.Points == 0 {
		t.Errorf("sampler %q historyStart %v points %d; want stale with the history", p.Sampler.State,
			p.HistoryStart, p.Points)
	}
}
