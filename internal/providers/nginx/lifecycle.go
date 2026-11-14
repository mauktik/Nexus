package nginx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// The lifecycle operations' helper actions (FR-5, FR-6). Each runs every
// check that can refuse without acting before its first change, so a
// refusal is definite and changes nothing: start reads the state before it
// tests, restart and reload test before they act, and reload identifies the
// master before anything else.
const (
	actionStart   = "start"
	actionStop    = "stop"
	actionRestart = "restart"
	actionReload  = "reload"
)

// statePoll is how often the helper reads the service's state while it
// waits for a start, stop or restart to reach its state.
const statePoll = 250 * time.Millisecond

// initOutputCap is the most of the init system's output a problem detail
// quotes. The operation log has all of it.
const initOutputCap = 2 << 10

// lifecycleBinding binds op, one of start, stop, restart and reload, to
// helper action. admit, when set, refuses a request before any operation
// exists.
func (p *Provider) lifecycleBinding(op def.Typed[spec.ServiceActionInput, spec.ServiceResult], action string,
	admit func(context.Context, spec.ServiceActionInput) error) provider.Binding {
	return provider.Detached(op, provider.DetachedSpec[spec.ServiceActionInput, spec.ServiceResult]{
		Admit: admit,
		Lane:  lane[spec.ServiceActionInput],
		Prepare: func(_ context.Context, _ spec.ServiceActionInput, r *provider.Run) (detach.Spec, error) {
			return p.prepare(action, helperInput{}, r)
		},
		Finalize: func(_ context.Context, _ spec.ServiceActionInput, x detach.Exit, r *provider.Run) (spec.ServiceResult, error) {
			return finalize[spec.ServiceResult](p, action, x, r)
		},
		LostDetail: func(r *provider.Run, _ spec.ServiceActionInput) string { return p.lostDetail(action, r) },
	})
}

// admitReload refuses a reload of an nginx that is not running with
// nginx-not-running (422), before any operation exists: a reload never
// starts nginx. The helper checks again, because the state can change while
// the operation waits in the lane.
func (p *Provider) admitReload(ctx context.Context, _ spec.ServiceActionInput) error {
	s := p.Settings()
	ctl, err := p.controller(s)
	if err != nil {
		return problem.New(problem.DependencyUnavailable, fmt.Sprintf("nginx needs a supported init system: %v", err)).
			With("provider", spec.Name)
	}
	st, err := ctl.Status(ctx, s.Service)
	if err != nil {
		return problem.New(problem.DependencyUnavailable, fmt.Sprintf("the state of service %s cannot be read from "+
			"%s: %v", s.Service, ctl.Init(), err)).With("provider", spec.Name)
	}
	if st.State != svcctl.Running {
		return problem.New(problem.NginxNotRunning, notRunningDetail(st)).With("state", string(st.State))
	}
	return nil
}

func notRunningDetail(st svcctl.Status) string {
	return fmt.Sprintf("nginx is %s, so there is nothing to reload: a reload never starts nginx; start it instead",
		st.State)
}

// with sets extension key of e to v.
func (e *helperError) with(key string, v any) *helperError {
	b, err := json.Marshal(v)
	if err != nil {
		e.Detail += fmt.Sprintf(" (its %s cannot be encoded: %v)", key, err)
		return e
	}
	if e.Ext == nil {
		e.Ext = map[string]json.RawMessage{}
	}
	e.Ext[key] = b
	return e
}

// serviceStatus reads the service's state before anything is done.
func serviceStatus(ctx context.Context, e env, service string) (svcctl.Status, *helperError) {
	st, err := e.svc.Status(ctx, service)
	if err != nil {
		return st, failure(problem.DependencyUnavailable, "the state of service %s cannot be read from %s: %v; "+
			"nothing was changed", service, e.svc.Init(), err)
	}
	if !st.Known {
		return st, failure(problem.DependencyUnavailable, "service %s is not known to %s; nothing was changed",
			service, e.svc.Init())
	}
	return st, nil
}

// pretest runs nginx -t before a change. An invalid configuration fails
// nginx-config-invalid and a test that cannot run dependency-unavailable;
// either way nothing has been done.
func pretest(ctx context.Context, e env, in helperInput) (TestOutput, *helperError) {
	say(e.out, "nginx-op: running %s -t", in.Nginx)
	t, err := e.nginx.Test(ctx)
	if err != nil {
		return t, failure(problem.DependencyUnavailable, "%v; nothing was changed", err)
	}
	if !t.Valid {
		say(e.out, "nginx-op: the configuration is invalid; nothing was changed")
		return t, failure(problem.NginxConfigInvalid, "nginx -t failed: the configuration is invalid; nothing was "+
			"changed").with("testOutput", t.Output)
	}
	say(e.out, "nginx-op: the configuration is valid")
	return t, nil
}

// serviceResult is the result for nginx observed in st.
func serviceResult(changed bool, st svcctl.Status, testOutput string, obs *spec.ReloadObservation) spec.ServiceResult {
	r := spec.ServiceResult{ActionResult: apitypes.ActionResult{Changed: changed}, State: string(st.State),
		TestOutput: testOutput, Reload: obs}
	if st.State == svcctl.Running && st.MainPID > 0 {
		pid := st.MainPID
		r.MainPID = &pid
	}
	return r
}

// runStart is the start action. A running nginx is left alone, without a
// test, so start stays idempotent when the configuration on disk is not the
// one nginx loaded (FR-5).
func runStart(ctx context.Context, e env, in helperInput) (any, *helperError) {
	service := in.Settings.Service
	st, herr := serviceStatus(ctx, e, service)
	if herr != nil {
		return nil, herr
	}
	if st.State == svcctl.Running {
		say(e.out, "nginx-op: %s is already running; nothing to do", service)
		return serviceResult(false, st, "", nil), nil
	}
	t, herr := pretest(ctx, e, in)
	if herr != nil {
		return nil, herr
	}
	return converge(ctx, e, in, svcctl.Start, svcctl.Running, t.Output)
}

// runStop is the stop action.
func runStop(ctx context.Context, e env, in helperInput) (any, *helperError) {
	service := in.Settings.Service
	st, herr := serviceStatus(ctx, e, service)
	if herr != nil {
		return nil, herr
	}
	if st.State == svcctl.Stopped {
		say(e.out, "nginx-op: %s is already stopped; nothing to do", service)
		return serviceResult(false, st, "", nil), nil
	}
	return converge(ctx, e, in, svcctl.Stop, svcctl.Stopped, "")
}

// runRestart is the restart action: it tests first, so a running nginx is
// never taken down for a configuration it cannot load, and from any state,
// stopped included, it ends running.
func runRestart(ctx context.Context, e env, in helperInput) (any, *helperError) {
	t, herr := pretest(ctx, e, in)
	if herr != nil {
		return nil, herr
	}
	return converge(ctx, e, in, svcctl.Restart, svcctl.Running, t.Output)
}

// converge runs action through the init system and waits for want.
func converge(ctx context.Context, e env, in helperInput, action svcctl.Action, want svcctl.State,
	testOutput string) (any, *helperError) {
	service := in.Settings.Service
	say(e.out, "nginx-op: %s %s through %s", action, service, e.svc.Init())
	res, err := e.svc.Do(ctx, action, service)
	if err != nil {
		if errors.Is(err, svcctl.ErrNotStarted) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) {
			return nil, failure(problem.DependencyUnavailable, "%s of %s through %s did not complete: %v", action,
				service, e.svc.Init(), err)
		}
		// The tool started and then ended without an exit status: it may have
		// acted before it died, so the state it left decides, as for a reload.
		say(e.out, "nginx-op: %s %s failed after it started: %v; waiting for the state", e.svc.Init(), action, err)
		res = svcctl.Result{ExitCode: noExitStatus, Output: []byte(err.Error())}
	}
	logInit(e, action, res)
	st, herr := awaitState(ctx, e, in, action, want, res)
	if herr != nil {
		return nil, herr
	}
	return serviceResult(true, st, testOutput, nil), nil
}

// awaitState reads the service's state at once and then every statePoll
// until it is want, for at most stateTimeout. A definite other state ends
// the wait with dependency-unavailable naming it and the init system's own
// words; a state still in transition or unreadable at the bound is
// outcome-unknown.
func awaitState(ctx context.Context, e env, in helperInput, action svcctl.Action, want svcctl.State,
	res svcctl.Result) (svcctl.Status, *helperError) {
	service, timeout := in.Settings.Service, in.Settings.StateTimeout
	deadline := time.Now().Add(timeout)
	for {
		st, err := e.svc.Status(ctx, service)
		var last string
		switch {
		case err != nil:
			last = fmt.Sprintf("its state cannot be read: %v", err)
		case st.State == want:
			say(e.out, "nginx-op: %s is %s", service, want)
			return st, nil
		case definite(st, want):
			return st, failure(problem.DependencyUnavailable, "nginx is %s after %s, not %s (%s: %s)%s", st.State,
				action, want, e.svc.Init(), st.Raw, initClause(e, action, res))
		default:
			last = fmt.Sprintf("it is %s (%s: %s)", st.State, e.svc.Init(), st.Raw)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return st, failure(problem.OutcomeUnknown, "nginx did not become %s within %s after %s: %s%s", want,
				timeout, action, last, initClause(e, action, res))
		}
		t := time.NewTimer(min(statePoll, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return st, failure(problem.OutcomeUnknown, "the helper was stopped while nginx was not yet %s after %s: "+
				"%s (%v)", want, action, last, ctx.Err())
		case <-t.C:
		}
	}
}

// definite reports whether st is an end state other than want, which
// waiting will not change: an unknown service, failed, or the opposite of
// want.
func definite(st svcctl.Status, want svcctl.State) bool {
	switch {
	case !st.Known, st.State == svcctl.Failed:
		return true
	case want == svcctl.Running:
		return st.State == svcctl.Stopped
	case want == svcctl.Stopped:
		return st.State == svcctl.Running
	}
	return false
}

// noExitStatus is the exit code recorded for an init-system tool that ended
// without one.
const noExitStatus = -1

// logInit writes the init system's answer to action to the operation log.
func logInit(e env, action svcctl.Action, res svcctl.Result) {
	if res.ExitCode == noExitStatus {
		say(e.out, "nginx-op: %s %s ended without an exit status", e.svc.Init(), action)
	} else {
		say(e.out, "nginx-op: %s %s exited %d", e.svc.Init(), action, res.ExitCode)
	}
	if out := strings.TrimSpace(string(res.Output)); out != "" {
		say(e.out, "%s", out)
	}
}

// initClause quotes a failed init-system action for a problem detail.
func initClause(e env, action svcctl.Action, res svcctl.Result) string {
	if res.ExitCode == 0 {
		return ""
	}
	out := strings.TrimSpace(string(res.Output))
	if len(out) > initOutputCap {
		out = string(validUTF8Prefix([]byte(out[:initOutputCap]))) + "…"
	}
	if res.ExitCode == noExitStatus {
		return fmt.Sprintf("; %s %s ended without an exit status: %s", e.svc.Init(), action, out)
	}
	return fmt.Sprintf("; %s %s exited %d: %s", e.svc.Init(), action, res.ExitCode, out)
}

// runReload is the reload action (Data Flow 3). Nothing is tested or
// reloaded until the master is identified, and nothing is reloaded until
// nginx -t passed.
func runReload(ctx context.Context, e env, in helperInput) (any, *helperError) {
	running, m, st, herr := resolveMaster(ctx, e, in)
	if herr != nil {
		return nil, herr
	}
	if !running {
		say(e.out, "nginx-op: nginx is %s; nothing was reloaded", st.State)
		return nil, failure(problem.NginxNotRunning, "%s", notRunningDetail(st)).with("state", string(st.State))
	}
	t, herr := pretest(ctx, e, in)
	if herr != nil {
		return nil, herr
	}
	obs, herr := reloadAndVerify(ctx, e, in, m)
	if herr != nil {
		if herr.Slug == problem.NginxReloadFailed {
			herr = herr.with("testOutput", t.Output)
		}
		return nil, herr
	}
	return serviceResult(true, st, t.Output, &obs), nil
}

// resolveMaster is Data Flow 3 steps 1 and 2, which every change that may
// reload runs before its first change. running is false, with no error,
// when nginx is in any state but running; st is the state read. Running
// with a master that cannot be identified is dependency-unavailable: a
// definite refusal, since nothing has been done.
func resolveMaster(ctx context.Context, e env, in helperInput) (running bool, m Master, st svcctl.Status,
	herr *helperError) {
	service := in.Settings.Service
	st, err := e.svc.Status(ctx, service)
	if err != nil {
		return false, Master{}, st, failure(problem.DependencyUnavailable, "the state of service %s cannot be read "+
			"from %s: %v; nothing was changed", service, e.svc.Init(), err)
	}
	if st.State != svcctl.Running {
		return false, Master{}, st, nil
	}
	m, err = ValidateMaster(e.proc, st.MainPID, in.Nginx)
	if err != nil {
		return true, Master{}, st, failure(problem.DependencyUnavailable, "nginx master process could not be "+
			"identified: %v", err)
	}
	say(e.out, "nginx-op: nginx master process %d", m.PID)
	return true, m, st, nil
}

// reloadAndVerify is Data Flow 3 steps 3 to 6 against m, the master
// resolveMaster identified: it observes m's workers, reloads through the
// init system and verifies the new generation. The observation is returned
// whatever the outcome once a reload was sent. confirmed is success;
// rejected and master-gone are nginx-reload-failed with the observation;
// anything else is outcome-unknown, since a reload was sent and its effect
// is not known. A tool that never started (svcctl.ErrNotStarted) sent
// nothing and is dependency-unavailable, the one slug the callers read as
// "no reload was sent"; a tool that started and died is a sent reload.
func reloadAndVerify(ctx context.Context, e env, in helperInput, m Master) (spec.ReloadObservation, *helperError) {
	service := in.Settings.Service
	before, err := Observe(e.proc, m)
	if errors.Is(err, ErrMasterGone) {
		obs := spec.ReloadObservation{MasterPID: m.PID, OldWorkers: []int{}, NewWorkers: []int{},
			Verdict: spec.VerdictMasterGone}
		say(e.out, "nginx-op: nginx master process %d is gone; no reload was sent", m.PID)
		return obs, failure(problem.NginxReloadFailed, "nginx master process %d exited before the reload; no "+
			"reload was sent", m.PID).with("observed", obs)
	}
	if err != nil {
		return spec.ReloadObservation{}, failure(problem.DependencyUnavailable, "nginx's workers cannot be "+
			"observed: %v; no reload was sent", err)
	}
	say(e.out, "nginx-op: reload %s through %s (workers %v)", service, e.svc.Init(), workerPIDs(before.Workers))
	res, err := e.svc.Do(ctx, svcctl.Reload, service)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
			return spec.ReloadObservation{}, failure(problem.OutcomeUnknown, "the reload through %s did not "+
				"complete: %v; whether nginx applied the configuration is unknown", e.svc.Init(), err)
		case errors.Is(err, svcctl.ErrNotStarted):
			return spec.ReloadObservation{}, failure(problem.DependencyUnavailable, "the reload through %s could "+
				"not run: %v; nothing was reloaded", e.svc.Init(), err)
		}
		// The tool started and then failed without an exit status (a signal,
		// a wait error): it may have delivered the reload before it died, so
		// this is a reload that may have been sent. Its effect is verified
		// like any other, with no exit status (-1), and the callers roll
		// back as for a sent reload.
		say(e.out, "nginx-op: the reload through %s failed after it started: %v; verifying whether nginx "+
			"reloaded", e.svc.Init(), err)
		res = svcctl.Result{ExitCode: noExitStatus, Output: []byte(err.Error())}
	}
	logInit(e, svcctl.Reload, res)
	obs, err := Verify(ctx, e.proc, before, res.ExitCode, in.Settings.ReloadVerifyTimeout, DefaultReloadPoll)
	say(e.out, "nginx-op: reload %s: master %d, old workers %v, new workers %v, %s reload exited %d", obs.Verdict,
		obs.MasterPID, obs.OldWorkers, obs.NewWorkers, e.svc.Init(), obs.InitExitCode)
	switch {
	case err != nil:
		return obs, failure(problem.OutcomeUnknown, "reload verification stopped: %v; the reload was sent and "+
			"whether nginx applied the configuration is unknown (verdict %s)", err, obs.Verdict).with("observed", obs)
	case obs.Verdict == spec.VerdictConfirmed:
		return obs, nil
	case obs.Verdict == spec.VerdictRejected:
		return obs, failure(problem.NginxReloadFailed, "nginx kept its previous configuration: no new worker "+
			"processes%s", initClause(e, svcctl.Reload, res)).with("observed", obs)
	case obs.Verdict == spec.VerdictMasterGone:
		return obs, failure(problem.NginxReloadFailed, "nginx master process %d exited during the reload%s",
			m.PID, initClause(e, svcctl.Reload, res)).with("observed", obs)
	}
	return obs, failure(problem.OutcomeUnknown, "the reload was sent but not confirmed within %s: verdict %s "+
		"(master %d, old workers %v, new workers %v, %s reload exited %d)", in.Settings.ReloadVerifyTimeout,
		obs.Verdict, obs.MasterPID, obs.OldWorkers, obs.NewWorkers, e.svc.Init(), obs.InitExitCode).
		with("observed", obs)
}
