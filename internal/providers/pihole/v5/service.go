package v5

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/mauktik/nexus/internal/problem"
)

// RestartDNS runs `pihole restartdns`: `service pihole-FTL restart`, with
// no config test first (V-17). Anything but (0, nil) is
// pihole-command-failed{command, exitCode}.
func (m *Mutator) RestartDNS(ctx context.Context, out io.Writer) error {
	return m.runPihole(ctx, out, "restartdns")
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
	command := strings.Join(append([]string{"pihole"}, args...), " ")
	detail := fmt.Sprintf("%s exited with code %d", command, code)
	if err != nil {
		detail = fmt.Sprintf("%s could not run: %v", command, err)
	}
	return problem.New(problem.PiholeCommandFailed, detail).With("command", command).With("exitCode", code)
}
