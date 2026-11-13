//go:build pihole_red

// Stub-style: SKIP-only (WI 3.2). Excluded from `make test` by the
// pihole_red build tag; the WI 3.2 developer deletes the //go:build line and
// these must pass. Cases: TC-75, TC-76, TC-ERR12, TC-ERR13 in
// planning/pihole-dns-zones/test-cases.md. Helper-action level cases for the
// same WI (ServiceResult, readiness) are added by the developer beside these.
package pihole

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
)

const (
	piholeBin    = "/usr/local/bin/pihole"
	piholeFTLBin = "/usr/bin/pihole-FTL"
	systemdRun   = "/usr/bin/systemd-run"
	systemctlBin = "/usr/bin/systemctl"
)

// newV5 returns a fake v5 host and the production v5 Mutator wired to it
// through the test seams only (Paths root, Runner, clock).
func newV5(t *testing.T) (*v5test.FakeHost, *v5.Mutator) {
	t.Helper()
	h := v5test.NewFakeHost(t)
	return h, &v5.Mutator{Paths: h.Paths(), Runner: h, Now: h.Now}
}

func slugOf(t *testing.T, err error) (problem.Slug, *problem.Error) {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *problem.Error", err)
	}
	return pe.Slug(), pe
}

// hasArgv reports whether any recorded argv satisfies pred.
func hasArgv(h *v5test.FakeHost, pred func([]string) bool) bool {
	return slices.ContainsFunc(h.Argvs(), pred)
}

// TC-75: RestartDNS runs exactly `pihole restartdns`; FTL comes back with a
// new PID and answers on loopback.
func TestTC75_RestartDNS(t *testing.T) {
	h, m := newV5(t)
	pid := h.FTL().PID()
	var out bytes.Buffer
	if err := m.RestartDNS(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if got := h.Argvs(); len(got) != 1 || !slices.Equal(got[0], []string{piholeBin, "restartdns"}) {
		t.Errorf("argv %q, want [[pihole restartdns]]", got)
	}
	if h.FTL().PID() == pid {
		t.Error("PID unchanged after restart")
	}
	if _, err := dnscheck.WaitReady(context.Background(), h.FTL().Addr(), 2*time.Second); err != nil {
		t.Errorf("FTL not answering after restart: %v", err)
	}
}

// TC-76 (regression: `pihole flush` is not a cache flush, FR-15): FlushCache
// runs exactly `pihole restartdns reload`, never `pihole flush`/`-f`; the
// query log is untouched, the PID is unchanged, and the cache was flushed.
func TestTC76_FlushCacheNeverPiholeFlush(t *testing.T) {
	h, m := newV5(t)
	pid, logLines := h.FTL().PID(), h.QueryLogLines()
	if err := m.FlushCache(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := h.Argvs(); len(got) != 1 || !slices.Equal(got[0], []string{piholeBin, "restartdns", "reload"}) {
		t.Errorf("argv %q, want [[pihole restartdns reload]]", got)
	}
	if hasArgv(h, func(a []string) bool { return slices.Contains(a, "flush") || slices.Contains(a, "-f") }) {
		t.Error("FlushCache ran pihole flush, which deletes the query log")
	}
	if h.QueryLogLines() != logLines || h.FTL().PID() != pid || h.FTL().CacheFlushes() != 1 {
		t.Errorf("log %d->%d pid %d->%d flushes %d; want log kept, same PID, 1 flush",
			logLines, h.QueryLogLines(), pid, h.FTL().PID(), h.FTL().CacheFlushes())
	}
}

// TC-ERR12: a non-zero `pihole restartdns` is pihole-command-failed naming
// the command and exit code.
func TestTCERR12_RestartFailure(t *testing.T) {
	h, m := newV5(t)
	h.FailCommand([]string{piholeBin, "restartdns"}, 1)
	err := m.RestartDNS(context.Background(), &bytes.Buffer{})
	slug, pe := slugOf(t, err)
	if slug != "pihole-command-failed" || pe.Extensions["command"] != "pihole restartdns" || pe.Extensions["exitCode"] != 1 {
		t.Errorf("got %v %+v", slug, pe.Extensions)
	}
}

// TC-ERR13 (V-10, A-19): TestConfig returns pihole-FTL --test's exit code: 0
// for a clean config, 1 with an invalid address= line in dnsFile.
func TestTCERR13_TestConfigExitCode(t *testing.T) {
	h, m := newV5(t)
	if code, err := m.TestConfig(context.Background(), &bytes.Buffer{}); err != nil || code != 0 {
		t.Fatalf("clean: %d, %v", code, err)
	}
	f := filepath.Join(h.Root(), "etc/dnsmasq.d/03-custom-dns-names.conf")
	if err := os.WriteFile(f, []byte("address=/bad.lan/999.1.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := m.TestConfig(context.Background(), &bytes.Buffer{}); err != nil || code != 1 {
		t.Errorf("bad line: exit %d, %v; want exit 1 with a nil error (err is reserved for failing to run the command)", code, err)
	}
	if last := h.Argvs()[len(h.Argvs())-1]; !slices.Equal(last, []string{piholeFTLBin, "--test"}) {
		t.Errorf("argv %q, want [pihole-FTL --test]", last)
	}
}
