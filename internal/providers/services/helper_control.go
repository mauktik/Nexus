package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// controlPoll is how often the helper reads the service's state while it
// waits for the target state.
const controlPoll = 250 * time.Millisecond

// controlQuoteBytes is the most of the init system's output a problem
// detail quotes. The operation log has all of it.
const controlQuoteBytes = 2 << 10

// controlNoExitStatus is the exit code recorded for an init-system tool
// that ended without one.
const controlNoExitStatus = -1

// controlHelperEnv is the control mode's seams. The zero value is the real
// host: svcctl's runner, /proc, /run, /etc and controlPoll.
type controlHelperEnv struct {
	Run      func(ctx context.Context, argv []string) (svcctl.Result, error)
	Poll     time.Duration
	ProcRoot string
	RunRoot  string
	EtcRoot  string
}

// controlTools are the tools the control mode acts through, by init system.
var controlTools = map[platform.InitSystem]string{
	platform.Systemd: "/usr/bin/systemctl",
	platform.OpenRC:  "/sbin/rc-service",
}

// runControlMode is the helper's control mode: it runs DF-3 step 4 on raw
// and writes result.json.
func runControlMode(ctx context.Context, dir string, raw []byte, stdout, _ io.Writer, env helperEnv) int {
	out, herr := runControl(ctx, raw, stdout, env.control)
	return writeHelperOutcome(dir, out, herr, stdout, io.Discard)
}

// runControl is DF-3 step 4. Nothing reaches the init system, not even a
// read, until the name and its canonical unit have been checked against
// the protected set snapshotted in input.json. start on a running service
// and stop on a stopped one send nothing. Otherwise the action is sent and
// the state is polled until it is the target, a definite other state, or
// the entry's timeout.
func runControl(ctx context.Context, raw []byte, log io.Writer, env controlHelperEnv) (any, *helperError) {
	var in controlHelperInput
	if err := decodeHelperStrict(raw, &in); err != nil {
		return nil, helperFailure(problem.InternalError, "the helper's control input cannot be decoded: %v; nothing "+
			"was changed", err)
	}
	init := platform.InitSystem(in.Init)
	if herr := controlRecheck(in, init); herr != nil {
		helperSay(log, "%s of %s refused: %s; nothing was sent", in.Action, in.Service, herr.Detail)
		return nil, herr
	}
	tool := controlTools[init]
	opts := svcctl.Options{Run: env.Run, ProcRoot: env.ProcRoot, RunRoot: env.RunRoot, EtcRoot: env.EtcRoot}
	ctl, err := svcctl.New(init, opts)
	if err != nil {
		return nil, helperFailure(problem.DependencyUnavailable, "%v; nothing was changed", err)
	}
	before, herr := controlRead(ctx, init, in.Service, opts)
	if herr != nil {
		return nil, herr
	}
	if !before.Known {
		return nil, helperFailure(problem.NotFound, "%s has no service %s; nothing was changed", init, in.Service)
	}
	want := svcctl.Running
	if in.Action == spec.ActionStop {
		want = svcctl.Stopped
	}
	was := svcctl.State(before.State)
	if in.Action != spec.ActionRestart && was == want {
		helperSay(log, "%s is already %s (%s); nothing was sent", in.Service, was, before.RawState)
		return controlResult(in, false, before), nil
	}

	timeout := time.Duration(in.TimeoutSeconds * float64(time.Second))
	deadline := time.Now().Add(timeout)
	helperSay(log, "%s %s through %s (it is %s, %s)", in.Action, in.Service, tool, was, before.RawState)
	res, err := ctl.Do(ctx, svcctl.Action(in.Action), in.Service)
	if err != nil {
		switch {
		case errors.Is(err, svcctl.ErrNotStarted):
			helperSay(log, "%s could not run: %v; nothing was sent", tool, err)
			return nil, helperFailure(problem.DependencyUnavailable, "the %s of %s through %s could not run: %v; "+
				"nothing was changed, and %s is still %s", in.Action, in.Service, tool, err, in.Service, was)
		case ctx.Err() != nil:
			return nil, helperFailure(problem.OutcomeUnknown, "the helper was stopped during the %s of %s through "+
				"%s (%v); whether %s changed is unknown", in.Action, in.Service, tool, ctx.Err(), in.Service)
		}
		// The tool started and then ended without an exit status (killed,
		// timed out, a failed wait): it may have acted, so the state it
		// left decides.
		helperSay(log, "%s %s %s failed after it started: %v; waiting for the state", tool, in.Action, in.Service, err)
		res = svcctl.Result{ExitCode: controlNoExitStatus, Output: []byte(err.Error())}
	}
	controlLog(log, tool, in, res)

	poll := env.Poll
	if poll <= 0 {
		poll = controlPoll
	}
	for {
		st, herr := controlRead(ctx, init, in.Service, opts)
		var last string
		state := svcctl.State(st.State)
		switch {
		case herr != nil:
			last = herr.Detail
		case st.Known && state == want:
			helperSay(log, "%s is %s (%s)", in.Service, state, st.RawState)
			return controlResult(in, true, st), nil
		case st.Known && want == svcctl.Stopped && state == svcctl.Failed && st.MainPID == 0:
			// A stop leaves a failed unit failed (nginx's rule): with no main
			// process the service is not running, which is what stop is for.
			// It changed nothing when the unit was failed before.
			helperSay(log, "%s is %s (%s), with no main process: it is not running", in.Service, state, st.RawState)
			return controlResult(in, was != svcctl.Failed, st), nil
		case controlDefinite(st, want, res):
			return nil, helperFailure(problem.ServiceActionFailed, "%s is %s (%s) after %s, not %s%s", in.Service,
				state, st.RawState, in.Action, want, controlQuote(tool, in.Action, res)).
				with("service", in.Service).with("action", in.Action).with("state", st.State).
				with("rawState", st.RawState).with("exitCode", res.ExitCode)
		default:
			last = fmt.Sprintf("it is %s (%s)", state, st.RawState)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, helperFailure(problem.OutcomeUnknown, "%s did not become %s within %s after %s: %s%s",
				in.Service, want, timeout, in.Action, last, controlQuote(tool, in.Action, res))
		}
		t := time.NewTimer(min(poll, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, helperFailure(problem.OutcomeUnknown, "the helper was stopped while %s was not yet %s "+
				"after %s: %s (%v)", in.Service, want, in.Action, last, ctx.Err())
		case <-t.C:
		}
	}
}

// controlRecheck is the helper's first step: the action, and the name and
// its canonical unit against the protected set in input.json, with the
// init system's built-in list added, so a set that lost entries still
// protects them (D-6). It reads and sends nothing.
func controlRecheck(in controlHelperInput, init platform.InitSystem) *helperError {
	set := &protectedSet{entries: slices.Concat(in.Protected, newProtectedSet(init, nil).entries)}
	if ok, why := set.protected(in.Service, in.CanonicalName); ok {
		return helperFailure(problem.ProtectedService, "%s is protected: %s", in.Service, why).
			with("service", in.Service).with("reason", why)
	}
	if !serviceName.MatchString(in.Service) {
		return helperFailure(problem.ValidationFailed, "%q is not a service name: it must match %s",
			in.Service, spec.ServiceNamePattern)
	}
	if !slices.Contains(spec.Actions, in.Action) || !slices.Contains(in.Actions, in.Action) {
		return helperFailure(problem.NotAllowed, "%s is not an action providers.services.control allows on %s",
			in.Action, in.Service).with("service", in.Service).with("action", in.Action).
			with("configKey", controlConfigKey)
	}
	if _, ok := controlTools[init]; !ok {
		return helperFailure(problem.DependencyUnavailable, "the services helper needs systemd or OpenRC, not %q", in.Init)
	}
	return nil
}

// controlRead reads the service's state as get does (svcctl.Describe).
func controlRead(ctx context.Context, init platform.InitSystem, name string, opts svcctl.Options) (svcctl.Details,
	*helperError) {
	d, err := svcctl.Describe(ctx, init, name, opts)
	if err != nil {
		return d, helperFailure(problem.DependencyUnavailable, "the state of %s cannot be read from %s: %v",
			name, init, err)
	}
	return d, nil
}

// controlDefinite reports whether d is an end state other than want that
// waiting will not change: an unknown service, failed, or the opposite of
// want. A running service after a stop whose tool exited 0 may be a stale
// read, so the wait goes on to its bound; after a tool that failed it is
// definite.
func controlDefinite(d svcctl.Details, want svcctl.State, res svcctl.Result) bool {
	state := svcctl.State(d.State)
	switch {
	case !d.Known, state == svcctl.Failed:
		return true
	case want == svcctl.Running:
		return state == svcctl.Stopped
	case want == svcctl.Stopped:
		return state == svcctl.Running && res.ExitCode != 0
	}
	return false
}

// controlResult is the ServiceResult of in observed in d.
func controlResult(in controlHelperInput, changed bool, d svcctl.Details) spec.ServiceResult {
	r := spec.ServiceResult{ActionResult: apitypes.ActionResult{Changed: changed}, Service: in.Service,
		Action: in.Action, State: d.State, RawState: d.RawState, Since: d.Since}
	if svcctl.State(d.State) == svcctl.Running && d.MainPID > 0 {
		pid := d.MainPID
		r.MainPID = &pid
	}
	return r
}

// controlLog writes the init system's answer to the action to the
// operation log, all of it.
func controlLog(log io.Writer, tool string, in controlHelperInput, res svcctl.Result) {
	if res.ExitCode == controlNoExitStatus {
		helperSay(log, "%s %s %s ended without an exit status", tool, in.Action, in.Service)
	} else {
		helperSay(log, "%s %s %s exited %d", tool, in.Action, in.Service, res.ExitCode)
	}
	if out := strings.TrimSpace(string(res.Output)); out != "" {
		_, _ = fmt.Fprintln(log, out)
	}
}

// controlQuote quotes a failed action for a problem detail: the exit code
// and the first controlQuoteBytes of the output, cut at a character
// boundary. It is empty when the tool exited 0.
func controlQuote(tool, action string, res svcctl.Result) string {
	if res.ExitCode == 0 {
		return ""
	}
	out := res.Output
	if len(out) > controlQuoteBytes {
		n := controlQuoteBytes
		for n > 0 && !utf8.RuneStart(out[n]) {
			n--
		}
		out = out[:n]
	}
	q := strings.ToValidUTF8(string(out), "�")
	if res.ExitCode == controlNoExitStatus {
		return fmt.Sprintf("; %s %s ended without an exit status: %s", tool, action, q)
	}
	return fmt.Sprintf("; %s %s exited %d: %s", tool, action, res.ExitCode, q)
}
