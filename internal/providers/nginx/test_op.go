// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nginx

import (
	"context"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
)

// actionTest is nginx.actions.test's helper action.
const actionTest = "test"

// testBinding binds nginx.actions.test: `nginx -t` on the live
// configuration, run by the helper, because inside the systemd sandbox
// nginx cannot open its log and pid files (CON-2). It admits every request:
// the test changes nothing.
func (p *Provider) testBinding() provider.Binding {
	return provider.Detached(spec.Test, provider.DetachedSpec[spec.ConfigTestInput, spec.TestResult]{
		Lane: lane[spec.ConfigTestInput],
		Prepare: func(_ context.Context, _ spec.ConfigTestInput, r *provider.Run) (detach.Spec, error) {
			return p.prepare(actionTest, helperInput{}, r)
		},
		Finalize: func(_ context.Context, _ spec.ConfigTestInput, x detach.Exit, r *provider.Run) (spec.TestResult, error) {
			return finalize[spec.TestResult](p, actionTest, x, r)
		},
		LostDetail: func(r *provider.Run, _ spec.ConfigTestInput) string { return p.lostDetail(actionTest, r) },
	})
}

// runTest is the helper's test action. An invalid configuration is a
// result with Valid false; only a test that could not run or did not finish
// fails the operation, with dependency-unavailable.
func runTest(ctx context.Context, e env, in helperInput) (any, *helperError) {
	say(e.out, "nginx-op: running %s -t", in.Nginx)
	t, err := e.nginx.Test(ctx)
	if err != nil {
		return nil, failure(problem.DependencyUnavailable, "%v", err)
	}
	verdict := "valid"
	if !t.Valid {
		verdict = "invalid"
	}
	say(e.out, "nginx-op: the configuration is %s", verdict)
	return spec.TestResult{Valid: t.Valid, Output: t.Output, OutputTruncated: t.Truncated}, nil
}
