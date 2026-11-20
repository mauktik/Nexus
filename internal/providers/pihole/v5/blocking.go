// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package v5

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// The units of the Nexus re-enable timer: systemd-run --unit=ReenableUnit
// creates ReenableTimerUnit, which starts reenableService.
const (
	ReenableUnit    = "nexus-pihole-reenable"
	reenableService = ReenableUnit + ".service"
)

// systemctlNotLoaded is systemctl's exit for a unit that is not loaded
// (EXIT_NOTINSTALLED for BUS_ERROR_NO_SUCH_UNIT): stopping a timer that
// already fired or never existed.
const systemctlNotLoaded = 5

// StopBlocking turns blocking off, for d when d > 0, else until
// StartBlocking. It never runs `pihole disable Ns`: that re-enable child
// lives in the op unit's cgroup and dies with it (V-13). Instead it
// replaces any pending Nexus re-enable timer, creates a transient systemd
// timer that runs `pihole enable` at now+d before disabling, so a timer
// that cannot be created never leaves blocking off for good, then runs
// `pihole disable` with no argument (A-28) and reads BLOCKING_ENABLED back,
// since disable always exits 0 (A-18). A failure after the timer exists
// removes it again.
func (m *Mutator) StopBlocking(ctx context.Context, d time.Duration, out io.Writer) (spec.BlockingProperties, error) {
	if err := m.stopReenableTimer(ctx, out); err != nil {
		return spec.BlockingProperties{}, err
	}
	var reenableAt *time.Time
	if d > 0 {
		at, err := m.startReenableTimer(ctx, d, out)
		if err != nil {
			return spec.BlockingProperties{}, err
		}
		reenableAt = &at
	}
	fail := func(err error) (spec.BlockingProperties, error) {
		if reenableAt != nil {
			if serr := m.stopReenableTimer(ctx, out); serr != nil {
				fmt.Fprintf(out, "pihole: the re-enable timer %s could not be removed: %v\n", ReenableTimerUnit, serr)
			}
		}
		return spec.BlockingProperties{}, err
	}
	fmt.Fprintln(out, "pihole: running pihole disable")
	if err := m.runPihole(ctx, out, "disable"); err != nil {
		return fail(err)
	}
	if got := ReadBlockingEnabled(m.Paths); got == nil || *got {
		return fail(readBackFailed("pihole disable", "false", got))
	}
	return spec.BlockingProperties{State: BlockingDisabled, ReenableAt: reenableAt}, nil
}

// StartBlocking removes any pending Nexus re-enable timer, runs `pihole
// enable`, which also kills a native re-enable child (A-47), and reads
// BLOCKING_ENABLED back, since enable always exits 0 (A-18).
func (m *Mutator) StartBlocking(ctx context.Context, out io.Writer) (spec.BlockingProperties, error) {
	if err := m.stopReenableTimer(ctx, out); err != nil {
		return spec.BlockingProperties{}, err
	}
	fmt.Fprintln(out, "pihole: running pihole enable")
	if err := m.runPihole(ctx, out, "enable"); err != nil {
		return spec.BlockingProperties{}, err
	}
	if got := ReadBlockingEnabled(m.Paths); got == nil || !*got {
		return spec.BlockingProperties{}, readBackFailed("pihole enable", "true", got)
	}
	return spec.BlockingProperties{State: BlockingEnabled}, nil
}

// stopReenableTimer stops ReenableTimerUnit, which is fine when it is not
// loaded (exit 5), then clears a failed state of the timer or its service
// so the unit name can be used again; that second step's errors are
// ignored.
func (m *Mutator) stopReenableTimer(ctx context.Context, out io.Writer) error {
	argv := []string{detach.Systemctl, "stop", ReenableTimerUnit}
	code, err := m.Runner.Run(ctx, argv, out, out)
	if err != nil || (code != 0 && code != systemctlNotLoaded) {
		return commandFailed("systemctl stop", code, err)
	}
	// A unit that is not loaded or not failed has nothing to reset, so a
	// failed reset does not fail the operation; it is logged, as
	// RestartDNS logs its own.
	reset := []string{detach.Systemctl, "reset-failed", ReenableTimerUnit, reenableService}
	if code, err := m.Runner.Run(ctx, reset, io.Discard, io.Discard); code != 0 || err != nil {
		msg := fmt.Sprintf("exited %d", code)
		if err != nil {
			msg = fmt.Sprintf("exited %d (%v)", code, err)
		}
		fmt.Fprintf(out, "pihole: systemctl reset-failed %s %s %s; continuing\n", ReenableTimerUnit, reenableService, msg)
	}
	return nil
}

// startReenableTimer creates the transient timer that runs `pihole enable`
// d from now (A-5, A-27): its own unit, outside the op unit's cgroup. The
// description carries the time in RFC 3339 UTC, which ReenableTimer reads
// back. It returns that time.
func (m *Mutator) startReenableTimer(ctx context.Context, d time.Duration, out io.Writer) (time.Time, error) {
	secs := int64((d + time.Second - 1) / time.Second)
	at := m.now().Add(time.Duration(secs) * time.Second).UTC()
	argv := []string{
		detach.SystemdRun,
		"--unit=" + ReenableUnit,
		fmt.Sprintf("--on-active=%ds", secs),
		"--timer-property=AccuracySec=1s",
		"--timer-property=RemainAfterElapse=no",
		"--description=" + ReenableDescriptionPrefix + at.Format(time.RFC3339Nano),
		m.Paths.PiholeCmd(), "enable",
	}
	fmt.Fprintf(out, "pihole: creating the re-enable timer %s for %s\n", ReenableTimerUnit, at.Format(time.RFC3339))
	code, err := m.Runner.Run(ctx, argv, out, out)
	if err != nil || code != 0 {
		return time.Time{}, commandFailed("systemd-run", code, err)
	}
	return at, nil
}

// commandFailed is pihole-command-failed{command, exitCode} for a command
// that did not end with (0, nil).
func commandFailed(command string, code int, err error) error {
	detail := fmt.Sprintf("%s exited with code %d", command, code)
	if err != nil {
		detail = fmt.Sprintf("%s could not run: %v", command, err)
	}
	return problem.New(problem.PiholeCommandFailed, detail+"; blocking was not changed").
		With("command", command).With("exitCode", code)
}

// readBackFailed is pihole-command-failed for a command that exited 0 but
// left BLOCKING_ENABLED other than want.
func readBackFailed(command, want string, got *bool) error {
	val := "absent or unreadable"
	if got != nil {
		val = fmt.Sprint(*got)
	}
	return problem.New(problem.PiholeCommandFailed, fmt.Sprintf("%s exited 0 but BLOCKING_ENABLED in %s is %s, not %s",
		command, SetupVarsFile, val, want)).
		With("command", command).With("exitCode", 0)
}
