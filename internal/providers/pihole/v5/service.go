// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package v5

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
)

// FTLUnit is FTL's native systemd unit, from core v5.18.4
// advanced/Templates/pihole-FTL.systemd.
const FTLUnit = "pihole-FTL.service"

// startLimitHint is added to a failed restart's detail when FTL's systemd
// start limit refused it (A-60).
const startLimitHint = "; FTL's systemd start limit was reached and FTL is stopped; " +
	"recover with `systemctl reset-failed " + FTLUnit + " && pihole restartdns`"

// RestartDNS runs `pihole restartdns`: `service pihole-FTL restart`, with
// no config test first (V-17). Anything but (0, nil) is
// pihole-command-failed{command, exitCode}.
//
// FTLUnit allows 5 starts in 60 s (pihole-FTL.systemd L14-16), and a sixth
// restart stops FTL and is then refused, leaving the LAN without DNS
// (A-60). So every restart Nexus makes, the restart-dns action, a DNS
// write's restart and its rollback restart, first runs `systemctl
// reset-failed pihole-FTL.service`, which flushes the start rate limit
// counter (A-61). Systemd's own restarts of a crashing FTL stay limited. A
// reset that fails is logged and the restart still runs: its result decides
// the outcome. When the restart fails and FTL's Result is start-limit-hit,
// the detail names the limit and the recovery command.
func (m *Mutator) RestartDNS(ctx context.Context, out io.Writer) error {
	reset := []string{detach.Systemctl, "reset-failed", FTLUnit}
	code, err := m.Runner.Run(ctx, reset, out, out)
	if code != 0 || err != nil {
		msg := fmt.Sprintf("exited %d", code)
		if err != nil {
			msg = fmt.Sprintf("exited %d (%v)", code, err)
		}
		fmt.Fprintf(out, "pihole: systemctl reset-failed %s %s; restarting anyway\n", FTLUnit, msg)
	}
	argv := []string{m.Paths.PiholeCmd(), "restartdns"}
	code, err = m.Runner.Run(ctx, argv, out, out)
	if code == 0 && err == nil {
		return nil
	}
	hint := ""
	if m.startLimitHit(ctx) {
		hint = startLimitHint
	}
	return piholeFailed("pihole restartdns", code, err, hint)
}

// startLimitHit reports whether `systemctl show pihole-FTL.service
// --property=Result` reads start-limit-hit. A show that fails reports false.
func (m *Mutator) startLimitHit(ctx context.Context) bool {
	var buf strings.Builder
	code, err := m.Runner.Run(ctx, []string{detach.Systemctl, "show", FTLUnit, "--property=Result"}, &buf, io.Discard)
	if code != 0 || err != nil {
		return false
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "Result=start-limit-hit" {
			return true
		}
	}
	return false
}

// FlushCache runs `pihole restartdns reload`, a SIGHUP that clears FTL's
// DNS cache and keeps it running (V-17). It never runs `pihole flush`,
// which deletes the query log, not the cache (FR-15). It exits 0 as a
// no-op when FTL is not running, so callers gate on readiness.
func (m *Mutator) FlushCache(ctx context.Context, out io.Writer) error {
	return m.runPihole(ctx, out, "restartdns", "reload")
}

// TestConfig runs `pihole-FTL --test`, dnsmasq's syntax check of the files
// it loads (V-10), and returns the runner's (exitCode, err) unchanged (A-19).
// The test passed only on (0, nil): a non-zero exit, a death by signal (139
// for the SIGSEGV of FTL v5.25.2, WI 0.1 check 3) and a failure to run
// (-1, err) all fail it.
func (m *Mutator) TestConfig(ctx context.Context, out io.Writer) (int, error) {
	return m.Runner.Run(ctx, []string{m.Paths.PiholeFTLCmd(), "--test"}, out, out)
}

// runPihole runs `pihole args…`; anything but (0, nil) is
// pihole-command-failed.
func (m *Mutator) runPihole(ctx context.Context, out io.Writer, args ...string) error {
	argv := append([]string{m.Paths.PiholeCmd()}, args...)
	code, err := m.Runner.Run(ctx, argv, out, out)
	if code == 0 && err == nil {
		return nil
	}
	return piholeFailed(strings.Join(append([]string{"pihole"}, args...), " "), code, err, "")
}

// piholeFailed is pihole-command-failed{command, exitCode} for a pihole command
// that ran with code or could not run (err), its detail followed by hint.
func piholeFailed(command string, code int, err error, hint string) error {
	detail := fmt.Sprintf("%s exited with code %d", command, code)
	if err != nil {
		detail = fmt.Sprintf("%s could not run: %v", command, err)
	}
	return problem.New(problem.PiholeCommandFailed, detail+hint).With("command", command).With("exitCode", code)
}
