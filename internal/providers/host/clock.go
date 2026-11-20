// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// Clock synchronization never comes from a syscall of systemd's @clock
// group (D-8, A-14): under the unit's SystemCallFilter one would kill
// nexusd. It comes from these two fixed read-only queries (A-15), run
// through svcctl.Run under bounded.
var (
	argvTimedatectlNTP = []string{"/usr/bin/timedatectl", "show", "--property=NTPSynchronized", "--value"}
	argvChronyTracking = []string{"/usr/bin/chronyc", "-c", "tracking"}
)

// pathChronyc is stat-only: its existence selects the chronyc query, and it
// is never opened.
const pathChronyc = "usr/bin/chronyc"

// timeSyncClients are the stat-only markers of the time-sync client a
// systemd host runs, in the order they are checked; with none the kernel
// keeps the time on its own.
var timeSyncClients = []struct{ marker, name string }{
	{"run/systemd/timesync", "systemd-timesyncd"},
	{"run/chrony/chronyd.pid", "chrony"},
	{"run/ntpd.pid", "ntpd"},
}

const (
	clockSourceKernel = "kernel"
	clockSourceChrony = "chrony"
)

// reasonNoTimeSync is a host with neither query.
var reasonNoTimeSync = prefixNotSupported + "no supported time-sync query (systemd-timedated, chrony)"

// clock is the clock synchronization state:
//   - systemd: timedatectl's NTPSynchronized, with the source the client
//     whose marker exists;
//   - otherwise, when usr/bin/chronyc exists: chronyc's leap status, source
//     chrony;
//   - otherwise unknown, not supported.
//
// A query that fails leaves synchronized unknown with the reason.
func (p *Provider) clock(ctx context.Context) spec.Clock {
	var out spec.Clock
	var argv []string
	var parse func([]byte) (bool, error)
	switch {
	case p.plat.Init == platform.Systemd:
		argv, parse = argvTimedatectlNTP, hostparse.TimedatectlNTPSynchronized
		src := clockSourceKernel
		for _, c := range timeSyncClients {
			if _, err := p.src.stat(c.marker); err == nil {
				src = c.name
				break
			}
		}
		out.Source = &src
	case p.chronycExists():
		argv, parse = argvChronyTracking, hostparse.ChronyTracking
		src := clockSourceChrony
		out.Source = &src
	default:
		out.Synchronized = unknownState
		setWhy(&out.Unavailable, "synchronized", reasonNoTimeSync)
		setWhy(&out.Unavailable, "source", reasonNoTimeSync)
		return out
	}

	synced, why := p.query(ctx, argv, parse)
	switch {
	case why != "":
		out.Synchronized = unknownState
		setWhy(&out.Unavailable, "synchronized", why)
	case synced:
		out.Synchronized = "true"
	default:
		out.Synchronized = "false"
	}
	return out
}

// unknownState is the "unknown" of a true, false or unknown field.
const unknownState = "unknown"

func (p *Provider) chronycExists() bool {
	_, err := p.src.stat(pathChronyc)
	return err == nil
}

// query runs one fixed command under bounded, keyed by its argv0, and
// parses its output. The command gets sourceTimeout of its own, detached
// from the request's cancellation, since later requests may join the same
// flight. Its output is never part of a reason (NFR-6).
func (p *Provider) query(ctx context.Context, argv []string, parse func([]byte) (bool, error)) (bool, string) {
	argv0 := argv[0]
	res, err := bounded(ctx, p.src, argv0, func() (svcctl.Result, error) {
		timeout := p.Settings().SourceTimeout
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		r, err := p.exec(cctx, argv)
		if errors.Is(err, context.DeadlineExceeded) {
			return r, &timeoutError{what: argv0, after: timeout}
		}
		return r, err
	})
	if err != nil {
		return false, reasonFor(argv0, err)
	}
	if res.ExitCode != 0 {
		return false, reasonCommandExit(argv0, res.ExitCode)
	}
	v, err := parse(res.Output)
	if err != nil {
		return false, prefixUnrecognized + argv0
	}
	return v, ""
}
