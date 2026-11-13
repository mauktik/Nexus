// Cases: TC-75, TC-76, TC-ERR12, TC-ERR13 and TC-ERR29 in
// planning/pihole-dns-zones/test-cases.md (WI 3.2), and TC-148 and TC-ERR32
// (WI 5.5, which also changes TC-75's argv): the v5 service methods,
// and the restart-dns and flush-cache helper actions run through Prepare,
// the helper and Finalize against a FakeHost and its FakeFTL.
package pihole

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
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
	return h, &v5.Mutator{Paths: h.Paths(), Runner: h, Now: h.Now, Reload: v5test.FastReload(), GravityPoll: 50 * time.Millisecond}
}

func slugOf(t *testing.T, err error) (problem.Slug, *problem.Error) {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *problem.Error", err)
	}
	return pe.Slug(), pe
}

// resetFailedArgv is the reset of FTL's systemd start limit that precedes
// every restart Nexus makes (A-61).
var resetFailedArgv = []string{systemctlBin, "reset-failed", "pihole-FTL.service"}

// restartArgvs is n restarts as RestartDNS runs them: reset-failed, then
// `pihole restartdns`.
func restartArgvs(n int) [][]string {
	return slices.Repeat([][]string{resetFailedArgv, {piholeBin, "restartdns"}}, n)
}

// hasArgv reports whether any recorded argv satisfies pred.
func hasArgv(h *v5test.FakeHost, pred func([]string) bool) bool {
	return slices.ContainsFunc(h.Argvs(), pred)
}

// TC-75: RestartDNS runs `systemctl reset-failed pihole-FTL.service`, then
// exactly `pihole restartdns`; FTL comes back with a new PID and answers on
// loopback.
func TestTC75_RestartDNS(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	pid := h.FTL().PID()
	var out bytes.Buffer
	if err := m.RestartDNS(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if got := h.Argvs(); !slices.EqualFunc(got, restartArgvs(1), slices.Equal) {
		t.Errorf("argv %q, want %q", got, restartArgvs(1))
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
	t.Parallel()
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
	t.Parallel()
	h, m := newV5(t)
	h.FailCommand([]string{piholeBin, "restartdns"}, 1)
	err := m.RestartDNS(context.Background(), &bytes.Buffer{})
	slug, pe := slugOf(t, err)
	if slug != "pihole-command-failed" || pe.Extensions["command"] != "pihole restartdns" || pe.Extensions["exitCode"] != 1 {
		t.Errorf("got %v %+v", slug, pe.Extensions)
	}
}

// TC-ERR32 (A-60, A-61): (a) a start refused by FTL's systemd start limit,
// here because reset-failed silently did nothing, is pihole-command-failed
// {command:"pihole restartdns", exitCode:1} whose detail names the start
// limit and the recovery command, read from `systemctl show
// pihole-FTL.service --property=Result`; (b) a reset-failed that exits 1, or
// (c) cannot run, is logged and the restart still runs and decides the
// outcome.
func TestTCERR32_StartLimitNamed(t *testing.T) {
	const recovery = "systemctl reset-failed pihole-FTL.service && pihole restartdns"
	showArgv := []string{systemctlBin, "show", "pihole-FTL.service", "--property=Result"}
	t.Run("start limit hit", func(t *testing.T) {
		h, m := newV5(t)
		h.PrimeFTLStarts(5)
		h.IgnoreCommand(resetFailedArgv)
		err := m.RestartDNS(context.Background(), &bytes.Buffer{})
		if err == nil {
			t.Fatalf("restart past the start limit succeeded; argvs %q", h.Argvs())
		}
		slug, pe := slugOf(t, err)
		if slug != problem.PiholeCommandFailed || pe.Extensions["command"] != "pihole restartdns" || pe.Extensions["exitCode"] != 1 {
			t.Errorf("got %v %+v", slug, pe.Extensions)
		}
		if !strings.Contains(pe.Detail, "systemd start limit") || !strings.Contains(pe.Detail, recovery) {
			t.Errorf("detail %q does not name the start limit and %q", pe.Detail, recovery)
		}
		if !hasArgv(h, func(a []string) bool { return slices.Equal(a, showArgv) }) {
			t.Errorf("argvs %q lack %q", h.Argvs(), showArgv)
		}
	})
	t.Run("other restart failure has no hint", func(t *testing.T) {
		h, m := newV5(t)
		h.FailCommand([]string{piholeBin, "restartdns"}, 1)
		_, pe := slugOf(t, m.RestartDNS(context.Background(), &bytes.Buffer{}))
		if strings.Contains(pe.Detail, "start limit") {
			t.Errorf("detail %q names the start limit; Result is success", pe.Detail)
		}
	})
	for _, tc := range []struct {
		name  string
		fault func(*v5test.FakeHost)
		log   string
	}{
		{"reset-failed exits 1", func(h *v5test.FakeHost) { h.FailCommand(resetFailedArgv, 1) },
			"pihole: systemctl reset-failed pihole-FTL.service exited 1; restarting anyway"},
		{"reset-failed cannot run", func(h *v5test.FakeHost) { h.FailCommandStart(resetFailedArgv) },
			"pihole: systemctl reset-failed pihole-FTL.service exited -1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, m := newV5(t)
			tc.fault(h)
			pid := h.FTL().PID()
			var out bytes.Buffer
			if err := m.RestartDNS(context.Background(), &out); err != nil {
				t.Fatalf("restart after a failed reset-failed: %v", err)
			}
			if !strings.Contains(out.String(), tc.log) || !strings.Contains(out.String(), "restarting anyway") {
				t.Errorf("output %q lacks %q ... restarting anyway", out.String(), tc.log)
			}
			if got := h.Argvs(); !slices.EqualFunc(got, restartArgvs(1), slices.Equal) || h.FTL().PID() == pid {
				t.Errorf("argvs %q pid %d -> %d; want %q and a new PID", got, pid, h.FTL().PID(), restartArgvs(1))
			}
		})
	}
}

// TC-ERR13 (V-10, A-19, WI 0.1 check 3): TestConfig returns the runner's
// (exitCode, err) for pihole-FTL --test unchanged: 0 for a clean config;
// 139 with an invalid address= line in dnsFile, the SIGSEGV death FTL
// v5.25.2 showed, reported as 128+11 with a nil error; 1 when the fake
// exits plainly instead; and (-1, err) when the command cannot start.
func TestTCERR13_TestConfigExitCode(t *testing.T) {
	t.Parallel()
	h, m := newV5(t)
	if code, err := m.TestConfig(context.Background(), &bytes.Buffer{}); err != nil || code != 0 {
		t.Fatalf("clean: %d, %v", code, err)
	}
	f := filepath.Join(h.Root(), "etc/dnsmasq.d/03-custom-dns-names.conf")
	if err := os.WriteFile(f, []byte("address=/bad.lan/999.1.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := m.TestConfig(context.Background(), &bytes.Buffer{}); err != nil || code != 139 {
		t.Errorf("bad line: exit %d, %v; want 139 (SIGSEGV) with a nil error (err is reserved for failing to run the command)", code, err)
	}
	if last := h.Argvs()[len(h.Argvs())-1]; !slices.Equal(last, []string{piholeFTLBin, "--test"}) {
		t.Errorf("argv %q, want [pihole-FTL --test]", last)
	}
	h.FTL().SetTestExit(1)
	if code, err := m.TestConfig(context.Background(), &bytes.Buffer{}); err != nil || code != 1 {
		t.Errorf("SetTestExit(1): exit %d, %v; want 1 with a nil error", code, err)
	}
	h.FailCommandStart([]string{piholeFTLBin, "--test"})
	if code, err := m.TestConfig(context.Background(), &bytes.Buffer{}); err == nil || code != -1 {
		t.Errorf("start failure: exit %d, %v; want (-1, err)", code, err)
	}
	if got := h.Argvs(); len(got) != 4 || !slices.EqualFunc(got, slices.Repeat([][]string{{piholeFTLBin, "--test"}}, 4), slices.Equal) {
		t.Errorf("argvs %q, want 4 x [pihole-FTL --test]", got)
	}
}

// serviceOp runs op's binding as the core would: Prepare, the helper on the
// operation's directory, then Finalize. window is the readiness window, 0
// for the production one.
func serviceOp(w *helperWorld, op def.Typed[spec.NoInput, spec.ServiceResult], id string, window time.Duration) (spec.ServiceResult, string, error) {
	w.t.Helper()
	df := serviceBinding(w, op)
	ctx := context.Background()
	if _, err := df.Prepare(ctx, spec.NoInput{}, provider.NewRun(id, nil)); err != nil {
		w.t.Fatalf("Prepare: %v", err)
	}
	inj := w.inject()
	inj.DNSServer, inj.ReadyWindow = w.h.FTL().Addr(), window
	var out bytes.Buffer
	if code := runHelper(ctx, filepath.Join(w.opsDir, id), &out, &out, inj); code != 0 {
		w.t.Fatalf("helper exit %d: %s", code, out.String())
	}
	res, err := df.Finalize(ctx, spec.NoInput{}, detach.Exit{}, provider.NewRun(id, nil))
	if err != nil {
		return spec.ServiceResult{}, out.String(), err
	}
	return res.(spec.ServiceResult), out.String(), nil
}

// serviceBinding is the provider's own binding of op.
func serviceBinding(w *helperWorld, op def.Typed[spec.NoInput, spec.ServiceResult]) provider.DetachedFuncs {
	w.t.Helper()
	for _, b := range w.p.Bindings() {
		if b.Op().ID == op.Op().ID {
			df, ok := b.Detached()
			if !ok {
				w.t.Fatalf("%s is not detached", op.Op().ID)
			}
			return df
		}
	}
	w.t.Fatalf("%s is not bound", op.Op().ID)
	return provider.DetachedFuncs{}
}

func pidStr(p *int) string {
	if p == nil {
		return "null"
	}
	return strconv.Itoa(*p)
}

// TC-75, action half (FR-14): restart-dns runs `systemctl reset-failed
// pihole-FTL.service` and `pihole restartdns` once each and returns a ServiceResult with the PID before, FTL's new PID and how long
// FTL took to answer.
func TestTC75_RestartDNSAction(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	res, out, err := serviceOp(w, spec.RestartDNS, "op-restart", 0)
	if err != nil {
		t.Fatalf("restart-dns: %v\n%s", err, out)
	}
	if got := w.h.Argvs(); !slices.EqualFunc(got, restartArgvs(1), slices.Equal) {
		t.Errorf("argv %q, want %q", got, restartArgvs(1))
	}
	newPID := w.h.FTL().PID()
	if !res.Changed || res.FTLPidBefore == nil || *res.FTLPidBefore != v5test.FakeFTLPID ||
		res.FTLPidAfter == nil || *res.FTLPidAfter != newPID || newPID == v5test.FakeFTLPID || res.ReadyAfterMs <= 0 {
		t.Errorf("result %+v (pids %s -> %s); want changed, %d -> %d", res, pidStr(res.FTLPidBefore),
			pidStr(res.FTLPidAfter), v5test.FakeFTLPID, newPID)
	}
	for _, step := range []string{"pihole: running pihole restartdns", "pihole: FTL answered after", "pihole: FTL pid after: " + strconv.Itoa(newPID)} {
		if !strings.Contains(out, step) {
			t.Errorf("output lacks %q:\n%s", step, out)
		}
	}
}

// TC-148 (hazard: start limit, A-60, A-61): six restart-dns ops on an
// unmoving fake clock all succeed, each with a new PID and FTL answering,
// though pihole-FTL.service allows 5 starts a minute: every `pihole
// restartdns` is preceded by `systemctl reset-failed pihole-FTL.service`,
// and FTL's Result stays success.
func TestTC148_SixRestartDNSOpsWithinAMinute(t *testing.T) {
	w := newHelperWorld(t)
	pid := w.h.FTL().PID()
	for i := 1; i <= 6; i++ {
		res, out, err := serviceOp(w, spec.RestartDNS, "op-restart-"+strconv.Itoa(i), 0)
		if err != nil {
			t.Fatalf("restart-dns %d of 6 within a minute: %v\n%s", i, err, out)
		}
		newPID := w.h.FTL().PID()
		if newPID == 0 || newPID == pid || res.FTLPidAfter == nil || *res.FTLPidAfter != newPID {
			t.Errorf("restart-dns %d: pid %d -> %d, ftlPidAfter %s; want a new running FTL", i, pid, newPID, pidStr(res.FTLPidAfter))
		}
		pid = newPID
	}
	if _, err := dnscheck.WaitReady(context.Background(), w.h.FTL().Addr(), 2*time.Second); err != nil {
		t.Errorf("FTL not answering after the sixth restart: %v", err)
	}
	if got := w.h.Argvs(); !slices.EqualFunc(got, restartArgvs(6), slices.Equal) {
		t.Errorf("argvs %q, want each pihole restartdns preceded by reset-failed: %q", got, restartArgvs(6))
	}
	var show bytes.Buffer
	if code, err := w.h.Run(context.Background(), []string{systemctlBin, "show", "pihole-FTL.service", "--property=Result"}, &show, &show); code != 0 || err != nil ||
		strings.TrimSpace(show.String()) != "Result=success" {
		t.Errorf("show Result: %d %v %q; want Result=success", code, err, show.String())
	}
}

// testPIDLimit is the PID read's limit in tests (helperWorld.inject): a
// fifth of production's ftlPIDPollLimit.
const testPIDLimit = ftlPIDPollLimit / 5

// The PID read's production bounds are the constants, unless a test seam
// shortens them (A-56).
func TestPIDPollDefaults(t *testing.T) {
	if ftlPIDPollInterval != 100*time.Millisecond || ftlPIDPollLimit != 2*time.Second {
		t.Errorf("PID poll %s for %s, want 100ms for 2s", ftlPIDPollInterval, ftlPIDPollLimit)
	}
	if got := helperSeams(nil).PIDPoll; got != (pidPoll{Interval: ftlPIDPollInterval, Limit: ftlPIDPollLimit}) {
		t.Errorf("production seams' PID poll %+v", got)
	}
	if got := helperSeams(&helperInject{}).PIDPoll; got != defaultPIDPoll {
		t.Errorf("an empty injection's PID poll %+v, want the defaults", got)
	}
}

// A-56: the new PID file appears only after FTL answers. restart-dns
// retries the read for up to its limit (2 s in production, testPIDLimit
// here), so ftlPidAfter is the new PID; after it the action reports what
// FTLPID returns, the old PID while the file still names a live FTL, or
// null when there is none, and that is not a failure.
func TestRestartDNSActionPIDFileLate(t *testing.T) {
	t.Run("within the limit", func(t *testing.T) {
		w := newHelperWorld(t)
		w.h.FTL().SetPIDFileLag(testPIDLimit / 5)
		res, out, err := serviceOp(w, spec.RestartDNS, "op-late", 0)
		if err != nil || res.FTLPidAfter == nil || *res.FTLPidAfter != w.h.FTL().PID() {
			t.Errorf("result %+v (after %s), %v; want ftlPidAfter %d\n%s", res, pidStr(res.FTLPidAfter), err, w.h.FTL().PID(), out)
		}
	})
	t.Run("never within the limit", func(t *testing.T) {
		w := newHelperWorld(t)
		w.h.FTL().SetPIDFileLag(time.Hour)
		start := time.Now()
		res, _, err := serviceOp(w, spec.RestartDNS, "op-stale", 0)
		if err != nil || res.FTLPidAfter == nil || *res.FTLPidAfter != v5test.FakeFTLPID {
			t.Errorf("result after %s, %v; want the old PID FTLPID still returns", pidStr(res.FTLPidAfter), err)
		}
		if d := time.Since(start); d < testPIDLimit {
			t.Errorf("gave up after %s, want %s of retries", d, testPIDLimit)
		}
	})
	t.Run("no PID file", func(t *testing.T) {
		w := newHelperWorld(t)
		w.h.FTL().SetPIDFileLag(time.Hour)
		if err := os.Remove(filepath.Join(w.h.Root(), v5.DefaultFTLPIDFile)); err != nil {
			t.Fatal(err)
		}
		res, out, err := serviceOp(w, spec.RestartDNS, "op-nopid", 0)
		if err != nil || res.FTLPidBefore != nil || res.FTLPidAfter != nil {
			t.Errorf("result %s -> %s, %v; want null -> null and no error\n%s", pidStr(res.FTLPidBefore), pidStr(res.FTLPidAfter), err, out)
		}
	})
}

// TC-76, action half (FR-15): flush-cache runs only `pihole restartdns
// reload`; FTL keeps its PID, the query log is kept, the cache was flushed
// once, and ftlPidBefore == ftlPidAfter.
func TestTC76_FlushCacheAction(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	logLines := w.h.QueryLogLines()
	res, out, err := serviceOp(w, spec.FlushCache, "op-flush", 0)
	if err != nil {
		t.Fatalf("flush-cache: %v\n%s", err, out)
	}
	if got := w.h.Argvs(); len(got) != 1 || !slices.Equal(got[0], []string{piholeBin, "restartdns", "reload"}) {
		t.Errorf("argv %q, want [[pihole restartdns reload]]", got)
	}
	if res.FTLPidBefore == nil || res.FTLPidAfter == nil || *res.FTLPidBefore != *res.FTLPidAfter ||
		*res.FTLPidAfter != v5test.FakeFTLPID || !res.Changed {
		t.Errorf("result %+v (pids %s -> %s); want the same PID %d", res, pidStr(res.FTLPidBefore), pidStr(res.FTLPidAfter), v5test.FakeFTLPID)
	}
	if w.h.QueryLogLines() != logLines || w.h.FTL().CacheFlushes() != 1 || w.h.FTL().Restarts() != 0 {
		t.Errorf("log %d->%d, flushes %d, restarts %d; want the log kept, 1 flush, no restart",
			logLines, w.h.QueryLogLines(), w.h.FTL().CacheFlushes(), w.h.FTL().Restarts())
	}
}

// TC-ERR12, action half: a failed restart command is pihole-command-failed
// through result.json, and FTL readiness is not waited for.
func TestRestartDNSActionCommandFailed(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	w.h.FTL().SetFault(v5test.FaultRestartFails)
	_, out, err := serviceOp(w, spec.RestartDNS, "op-fail", 0)
	slug, pe := slugOf(t, err)
	if slug != problem.PiholeCommandFailed || pe.Extensions["command"] != "pihole restartdns" || pe.Extensions["exitCode"] != 1 {
		t.Errorf("got %v %+v", slug, pe.Extensions)
	}
	if strings.Contains(out, "waiting up to") {
		t.Errorf("waited for FTL after a failed command:\n%s", out)
	}
}

// TC-ERR29 (FR-14, FR-15): FTL that never answers after a restart or a
// flush, and a flush while FTL is stopped (reload exits 0 as a no-op,
// V-17), each fail dns-verification-failed{rolledBack:false, name:pi.hole}.
func TestTCERR29_NeverReady(t *testing.T) {
	const window = 250 * time.Millisecond
	cases := []struct {
		name  string
		op    def.Typed[spec.NoInput, spec.ServiceResult]
		setup func(*v5test.FakeHost)
	}{
		{"restart never ready", spec.RestartDNS, func(h *v5test.FakeHost) { h.FTL().SetFault(v5test.FaultNeverReady) }},
		{"flush never ready", spec.FlushCache, func(h *v5test.FakeHost) { h.FTL().SetFault(v5test.FaultNeverReady) }},
		{"flush with FTL stopped", spec.FlushCache, func(h *v5test.FakeHost) { h.FTL().Stop() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newHelperWorld(t)
			tc.setup(w.h)
			_, out, err := serviceOp(w, tc.op, "op-nr", window)
			slug, pe := slugOf(t, err)
			if slug != problem.DNSVerificationFailed || pe.Extensions["rolledBack"] != false || pe.Extensions["name"] != "pi.hole" {
				t.Errorf("got %v %+v; want dns-verification-failed rolledBack:false name:pi.hole\n%s", slug, pe.Extensions, out)
			}
		})
	}
}

// restart-dns and flush-cache are bound detached with MaxRuntime 10 min and
// LostDetail "DNS may be restarting; check `nexus pihole get`", which a
// lost unit's outcome-unknown carries.
func TestServiceBindings(t *testing.T) {
	t.Parallel()
	const lost = "DNS may be restarting; check `nexus pihole get`"
	for _, op := range []def.Typed[spec.NoInput, spec.ServiceResult]{spec.RestartDNS, spec.FlushCache} {
		w := newHelperWorld(t)
		df := serviceBinding(w, op)
		run := provider.NewRun("op-lost", nil)
		s, err := df.Prepare(context.Background(), spec.NoInput{}, run)
		if err != nil || s.MaxRuntime != 10*time.Minute {
			t.Errorf("%s: Prepare MaxRuntime %s, %v; want 10m", op.Op().ID, s.MaxRuntime, err)
		}
		if df.LostDetail == nil || df.LostDetail(run, spec.NoInput{}) != lost {
			t.Errorf("%s: LostDetail is not %q", op.Op().ID, lost)
		}
		_, err = df.Finalize(context.Background(), spec.NoInput{}, detach.Exit{ExitCode: -1, Signal: "SIGKILL"}, run)
		if slug, pe := slugOf(t, err); slug != problem.OutcomeUnknown || !strings.Contains(pe.Detail, lost) {
			t.Errorf("%s: lost unit: %v %q; want outcome-unknown containing %q", op.Op().ID, slug, pe.Detail, lost)
		}
	}
}
