package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// helperModeReboot is input.json's mode of the reboot.
const helperModeReboot = "reboot"

// rebootStopExit is the reboot helper's exit code for every stop it acts
// on: 128 + SIGTERM. The supervisor's canceled or interrupted report, not
// the code, decides the operation's status.
const rebootStopExit = 143

// rebootQuoteBytes is the most of the trigger's output a problem detail
// quotes. The operation log has all of it.
const rebootQuoteBytes = 2 << 10

// The stages at which rebootHelperEnv.hook is called, each just after the
// step it names, so a stop injected there is first seen by the next check.
const (
	stageAfterDelay   = "after-delay"   // The delay ran out; next: the first check (S1)
	stageAfterCheck1  = "after-check-1" // The first check found no stop; next: the second (S2, S3)
	stageAfterRecord  = "after-record"  // The record is written; next: the second check (S2, S3)
	stageAfterCheck2  = "after-check-2" // The second check found no stop; no check before the trigger (S4)
	stageAfterTrigger = "after-trigger" // The trigger succeeded and no stop was seen; next: the wait (S5)
)

// rebootHelperInput is input.json of the reboot, as Prepare wrote it (DF-4
// step 3).
type rebootHelperInput struct {
	Mode           string `json:"mode"`
	OperationID    string `json:"operationId"`
	Init           string `json:"init"`
	DelaySeconds   int    `json:"delaySeconds"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	// BootID and KernelRelease are the boot and kernel the reboot was
	// prepared in; AfterReboot reports the kernel as previousKernelRelease.
	BootID         string              `json:"bootId"`
	KernelRelease  string              `json:"kernelRelease"`
	RebootRequired spec.RebootRequired `json:"rebootRequired"`
	WhatIf         bool                `json:"whatIf,omitempty"`
	// Report is whatIf's result, written by the helper as it is.
	Report *spec.RebootResult `json:"report,omitempty"`
}

// rebootHelperEnv is the reboot mode's seams. The zero value is the real
// host: a timer Sleep that returns early when its context is done,
// svcctl.Run, /proc, time.Now, no hook and no log.
type rebootHelperEnv struct {
	Sleep    func(ctx context.Context, d time.Duration) error
	Run      func(ctx context.Context, argv []string) (svcctl.Result, error)
	ProcRoot string
	Now      func() time.Time
	// Out is the operation log. runRebootMode sets it to the helper's
	// stdout when it is nil.
	Out io.Writer
	// hook, when set, is called at each stage* just after its step.
	hook func(stage string)
	// cancelRequested, when set, replaces detach.CancelRequested.
	cancelRequested func(dir string) (bool, error)
}

// runRebootMode is the helper's reboot mode.
func runRebootMode(ctx context.Context, dir string, _ []byte, stdout, _ io.Writer, env helperEnv) int {
	e := env.reboot
	if e.Out == nil {
		e.Out = stdout
	}
	return runReboot(ctx, dir, e)
}

// sleepCtx sleeps d, returning ctx's error at once when ctx is done first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runReboot is the helper's reboot mode, D-3 S1 to S5 and D-4. stop is
// the stop context: done once a SIGTERM arrived (Helper makes it with
// signal.NotifyContext before input.json is read). A cancel is the cancel
// file in dir, then its SIGTERM. The helper checks for a stop exactly twice,
// before the record (step 2) and after it (step 4):
//
//  1. sleep the delay; a stop ends it with no record and no trigger (S1);
//  2. a stop seen here exits the same way (S1);
//  3. record the request with this boot's ID;
//  4. read the stop context, then the cancel file: a cancel retracts the
//     record and exits (S2); a SIGTERM with no cancel file keeps it and
//     exits (S3);
//  5. trigger, under context.WithoutCancel and svcctl's 30 s bound, in its
//     own process group: no stop can kill it (S4). A failed trigger
//     retracts the record and reports reboot-did-not-happen at stage
//     trigger;
//  6. a stop seen once the trigger succeeded exits with the record kept
//     (S4); otherwise wait the timeout, which a stop ends with the record
//     kept (S5), and which running out retracts the record and reports
//     reboot-did-not-happen at stage wait.
//
// Every stop exit is rebootStopExit with no result.json.
func runReboot(stop context.Context, dir string, env rebootHelperEnv) int {
	if env.Sleep == nil {
		env.Sleep = sleepCtx
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Out == nil {
		env.Out = io.Discard
	}
	if env.cancelRequested == nil {
		env.cancelRequested = detach.CancelRequested
	}
	hook := func(stage string) {
		if env.hook != nil {
			env.hook(stage)
		}
	}
	log := env.Out
	raw, ok, err := readHelperFile(dir, helperInputFile)
	if err != nil || !ok {
		return writeHelperOutcome(dir, nil, helperFailure(problem.InternalError,
			"the reboot helper's input cannot be read (found %v, %v); nothing was sent", ok, err), log, io.Discard)
	}
	var in rebootHelperInput
	if err := decodeHelperStrict(raw, &in); err != nil {
		return writeHelperOutcome(dir, nil, helperFailure(problem.InternalError,
			"the reboot helper's input cannot be decoded: %v; nothing was sent", err), log, io.Discard)
	}
	if in.WhatIf {
		if in.Report == nil {
			return writeHelperOutcome(dir, nil, helperFailure(problem.InternalError,
				"the reboot helper's whatIf input has no report; nothing was sent"), log, io.Discard)
		}
		helperSay(log, "whatIf: every guard was evaluated; nothing is rebooted")
		return writeHelperOutcome(dir, *in.Report, nil, log, io.Discard)
	}
	argv, err := svcctl.RebootArgv(platform.InitSystem(in.Init))
	if err != nil {
		return writeHelperOutcome(dir, nil, helperFailure(problem.DependencyUnavailable,
			"%v; nothing was sent", err), log, io.Discard)
	}
	delay := time.Duration(in.DelaySeconds) * time.Second
	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	// stopped reports a stop before the record. checkErr is set when the
	// cancel file could not be checked and no SIGTERM came: unknown is taken
	// for a cancel, since nothing has been recorded yet.
	stopped := func() (bool, error) {
		if stop.Err() != nil {
			return true, nil
		}
		cancel, err := env.cancelRequested(dir)
		if err != nil {
			helperSay(log, "the cancel request cannot be checked (%v); taking it as a cancel", err)
			return true, err
		}
		return cancel, nil
	}

	// Step 1 (S1).
	helperSay(log, "rebooting with %s in %s", strings.Join(argv, " "), delay)
	if err := env.Sleep(stop, delay); err != nil {
		helperSay(log, "stopped during the delay (%v); nothing was recorded or sent", err)
		return rebootStopExit
	}
	hook(stageAfterDelay)
	// Step 2 (S1).
	if st, cerr := stopped(); st {
		if cerr != nil {
			// As step 4 does: a stop nobody asked for reports why, so the
			// operation does not end "exited without reporting a result".
			return writeHelperOutcome(dir, nil, helperFailure(problem.InternalError,
				"the cancel request cannot be checked: %v; nothing was recorded or sent", cerr), log, io.Discard)
		}
		helperSay(log, "stopped before the reboot was recorded; nothing was recorded or sent")
		return rebootStopExit
	}
	hook(stageAfterCheck1)
	// Step 3.
	// The helper runs outside nexusd's sandbox and reads the boot ID PID 1
	// sees, as nexusd and the ops manager do (detach.ReadBoot).
	b, err := detach.ReadBoot(env.ProcRoot, nil)
	boot := b.ID
	if err != nil {
		return writeHelperOutcome(dir, nil, helperFailure(problem.DependencyUnavailable,
			"the boot ID cannot be read: %v, so the reboot could not be confirmed; nothing was sent", err),
			log, io.Discard)
	}
	if err := detach.RecordReboot(dir, detach.RebootRecord{BootID: boot, RequestedAt: env.Now().UTC()}); err != nil {
		herr := helperFailure(problem.InternalError, "the reboot request cannot be recorded: %v; nothing was sent", err)
		if rerr := detach.RetractReboot(dir); rerr != nil {
			herr.Detail += fmt.Sprintf(" (and a partial record cannot be removed: %v)", rerr)
		}
		return writeHelperOutcome(dir, nil, herr, log, io.Discard)
	}
	helperSay(log, "recorded the reboot request in boot %s", boot)
	hook(stageAfterRecord)
	// Step 4: the stop context first, then the file. Launcher.Cancel
	// writes the file before it signals, so a SIGTERM seen here with the
	// file absent is never a cancel (S3), and one with it always is (S2).
	term := stop.Err() != nil
	cancel, cerr := env.cancelRequested(dir)
	if cerr != nil {
		helperSay(log, "the cancel request cannot be checked (%v); taking it as a cancel", cerr)
		cancel = true
	}
	switch {
	case cancel:
		if err := detach.RetractReboot(dir); err != nil {
			helperSay(log, "canceled, but the reboot record cannot be removed: %v", err)
		} else {
			helperSay(log, "canceled: the reboot record is removed and nothing was sent")
		}
		if !term && cerr != nil {
			return writeHelperOutcome(dir, nil, helperFailure(problem.InternalError,
				"the cancel request cannot be checked: %v; nothing was sent", cerr), log, io.Discard)
		}
		return rebootStopExit
	case term:
		helperSay(log, "stopped with no cancel request after the reboot was recorded; the record is kept and "+
			"nothing was sent")
		return rebootStopExit
	}
	hook(stageAfterCheck2)

	// Step 5 (S4): no stop reaches the trigger.
	res, err := svcctl.Call(context.WithoutCancel(stop), argv, svcctl.Options{Run: env.Run})
	if err != nil || res.ExitCode != 0 {
		return rebootTriggerFailed(dir, argv, boot, res, err, log)
	}
	helperSay(log, "%s exited 0", strings.Join(argv, " "))
	if out := strings.TrimSpace(string(res.Output)); out != "" {
		_, _ = fmt.Fprintln(log, out)
	}
	// Step 6 (S4, S5).
	if stop.Err() != nil {
		helperSay(log, "stopped while the reboot was triggered; the record is kept")
		return rebootStopExit
	}
	hook(stageAfterTrigger)
	if err := env.Sleep(stop, timeout); err != nil {
		helperSay(log, "stopped while waiting for the host to go down (%v); the record is kept", err)
		return rebootStopExit
	}
	herr := helperFailure(problem.RebootDidNotHappen, "%s exited 0, but the host was still up in boot %s %s later; "+
		"nothing else was sent", strings.Join(argv, " "), boot, timeout).
		with("stage", "wait").with("bootId", boot)
	if err := detach.RetractReboot(dir); err != nil {
		herr.Detail += fmt.Sprintf(" (the reboot record cannot be removed: %v)", err)
	}
	return writeHelperOutcome(dir, nil, herr, log, io.Discard)
}

// rebootTriggerFailed retracts the record of a trigger that did not run or
// exited non-zero, and writes reboot-did-not-happen at stage trigger, with
// the exit code (null when the tool ended without one) and up to
// rebootQuoteBytes of its output.
func rebootTriggerFailed(dir string, argv []string, boot string, res svcctl.Result, err error, log io.Writer) int {
	cmd := strings.Join(argv, " ")
	var exitCode *int
	var detail string
	switch {
	case err != nil && errors.Is(err, svcctl.ErrNotStarted):
		helperSay(log, "%s could not run: %v", cmd, err)
		detail = fmt.Sprintf("%s could not run: %v; nothing was sent", cmd, err)
	case err != nil:
		helperSay(log, "%s failed: %v", cmd, err)
		detail = fmt.Sprintf("%s failed: %v", cmd, err)
	default:
		code := res.ExitCode
		exitCode = &code
		helperSay(log, "%s exited %d", cmd, code)
		if out := strings.TrimSpace(string(res.Output)); out != "" {
			_, _ = fmt.Fprintln(log, out)
		}
		detail = fmt.Sprintf("%s exited %d: %s", cmd, code, quoteBytes(res.Output, rebootQuoteBytes))
	}
	herr := helperFailure(problem.RebootDidNotHappen, "the reboot was not triggered: %s", detail).
		with("stage", "trigger").with("bootId", boot).with("exitCode", exitCode)
	if rerr := detach.RetractReboot(dir); rerr != nil {
		herr.Detail += fmt.Sprintf(" (the reboot record cannot be removed: %v)", rerr)
	}
	return writeHelperOutcome(dir, nil, herr, log, io.Discard)
}

// quoteBytes is b made valid UTF-8 and cut at a character boundary to at
// most max bytes.
func quoteBytes(b []byte, max int) string {
	q := strings.ToValidUTF8(string(b), "�")
	if len(q) > max {
		n := max
		for n > 0 && !utf8.RuneStart(q[n]) {
			n--
		}
		q = q[:n]
	}
	return q
}
