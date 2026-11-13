// Stub-style: SKIP-only (WI 3.2). Excluded from `make test` by the
// pihole_red build tag; the WI 3.2 developer deletes the //go:build line and
// these must pass. Cases: TC-74 (a-g) in planning/pihole-dns-zones/test-cases.md.
//
// FakeFTL fidelity: it must reproduce dnsmasq/FTL v5.25.2 behavior the DNS
// write pipeline depends on (architecture V-10, V-11, V-17; requirements
// "v5 command facts"): address= covers subdomains with longest-suffix match;
// `restartdns reload` (SIGHUP) flushes the cache but does NOT re-read conf
// files; only a full restart does; `pihole flush` clears the query log, not
// the cache; `--test` is vacuous without /etc/dnsmasq.conf. Queries go
// through the real dnscheck client (WI 2.3).
package v5test_test

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
)

const piholeFTL = "/usr/bin/pihole-FTL"

func dnsFile(h *v5test.FakeHost) string {
	return filepath.Join(h.Root(), "etc/dnsmasq.d/03-custom-dns-names.conf")
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLine(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s + "\n"); err != nil {
		t.Fatal(err)
	}
}

func query(t *testing.T, h *v5test.FakeHost, name string) dnscheck.Answer {
	t.Helper()
	a, err := dnscheck.Query(context.Background(), h.FTL().Addr(), name, time.Second)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	return a
}

func answers(t *testing.T, h *v5test.FakeHost, name, want string) {
	t.Helper()
	a := query(t, h, name)
	if want == "" {
		if a.RCode != "NXDOMAIN" || len(a.A) != 0 {
			t.Errorf("%s = %+v, want NXDOMAIN", name, a)
		}
		return
	}
	if len(a.A) != 1 || a.A[0] != netip.MustParseAddr(want) {
		t.Errorf("%s = %+v, want %s", name, a, want)
	}
}

// TC-74a: address= answers the name and every subdomain; the longest
// matching suffix wins.
func TestTC74a_AddressCoversSubdomainsLongestWins(t *testing.T) {
	h := v5test.NewFakeHost(t)
	write(t, dnsFile(h), "address=/xi.example/192.168.7.9\naddress=/photos.xi.example/192.168.7.7\n")
	run(t, h, pihole, "restartdns")
	answers(t, h, "photos.xi.example", "192.168.7.7")
	answers(t, h, "a.b.photos.xi.example", "192.168.7.7")
	answers(t, h, "www.xi.example", "192.168.7.9")
	answers(t, h, "xi.example", "192.168.7.9")
	answers(t, h, "notxi.example", "")
}

// TC-74b (hazard control: restart required): a line added after start is not
// served after `restartdns reload` (SIGHUP flushes the cache, keeps the PID,
// does not re-read conf files); a full `restartdns` serves it with a new PID.
func TestTC74b_ReloadDoesNotRereadConf(t *testing.T) {
	h := v5test.NewFakeHost(t)
	write(t, dnsFile(h), "address=/kiwi.home.local/192.168.7.5\n")
	run(t, h, pihole, "restartdns")
	pid := h.FTL().PID()
	appendLine(t, dnsFile(h), "address=/new.home.local/192.168.7.6")

	if code, _ := run(t, h, pihole, "restartdns", "reload"); code != 0 {
		t.Fatalf("reload exit %d", code)
	}
	answers(t, h, "new.home.local", "")
	if h.FTL().PID() != pid || h.FTL().CacheFlushes() != 1 {
		t.Errorf("reload: pid %d->%d, cache flushes %d; want same PID and 1 flush", pid, h.FTL().PID(), h.FTL().CacheFlushes())
	}

	run(t, h, pihole, "restartdns")
	answers(t, h, "new.home.local", "192.168.7.6")
	if h.FTL().PID() == pid {
		t.Error("full restart kept the PID")
	}
}

// TC-74c (V-10, V-11, WI 0.1 check 3): an invalid address= line in a file
// conf-dir loads makes --test print dnsmasq's "Bad address" line and die by
// SIGSEGV, (139, nil) through the runner contract, as FTL v5.25.2 did;
// SetTestExit(1) gives the plain exit 1 the code path predicts. Files
// dnsmasq skips are ignored, a missing /etc/dnsmasq.conf passes vacuously,
// and the running FTL is untouched (A-21).
func TestTC74c_ConfigTest(t *testing.T) {
	h := v5test.NewFakeHost(t)
	pid := h.FTL().PID()
	if code, out := run(t, h, piholeFTL, "--test"); code != 0 || !strings.Contains(out, "dnsmasq: syntax check OK.") {
		t.Fatalf("clean config: --test exit %d, output %q", code, out)
	}
	dir := filepath.Join(h.Root(), "etc/dnsmasq.d")
	for _, skipped := range []string{".03-custom.nexus-x.tmp", "03-custom.conf~", "#03-custom.conf#"} {
		write(t, filepath.Join(dir, skipped), "address=/bad.lan/999.1.1.1\n")
		if code, _ := run(t, h, piholeFTL, "--test"); code != 0 {
			t.Errorf("bad line in skipped %s: --test exit %d, want 0", skipped, code)
		}
		os.Remove(filepath.Join(dir, skipped))
	}
	appendLine(t, dnsFile(h), "address=/ok.lan/192.168.7.1")
	appendLine(t, dnsFile(h), "address=/bad.lan/999.1.1.1")
	code, out := run(t, h, piholeFTL, "--test")
	wantLine := "dnsmasq: Bad address in --address at line 2 of /etc/dnsmasq.d/03-custom-dns-names.conf"
	if code != 139 || !strings.Contains(out, wantLine) || !strings.Contains(out, "killed by signal SIGSEGV") {
		t.Errorf("bad address= line: --test exit %d, output %q; want 139 (SIGSEGV) after %q", code, out, wantLine)
	}
	h.FTL().SetTestExit(1)
	if code, out := run(t, h, piholeFTL, "--test"); code != 1 || !strings.Contains(out, wantLine) || strings.Contains(out, "SIGSEGV") {
		t.Errorf("SetTestExit(1): --test exit %d, output %q; want a plain exit 1", code, out)
	}
	h.FTL().SetTestExit(0)
	if code, _ := run(t, h, piholeFTL, "--test"); code != 139 {
		t.Errorf("SetTestExit(0): --test exit %d, want the SIGSEGV 139 again", code)
	}
	if h.FTL().PID() != pid || h.FTL().CacheFlushes() != 0 || len(h.FTLEvents()) != 0 {
		t.Errorf("a failed --test touched FTL: pid %d->%d, events %v", pid, h.FTL().PID(), h.FTLEvents())
	}
	answers(t, h, "pi.hole", "127.0.0.1")
	if err := os.Remove(filepath.Join(h.Root(), "etc/dnsmasq.conf")); err != nil {
		t.Fatal(err)
	}
	if code, _ := run(t, h, piholeFTL, "--test"); code != 0 {
		t.Errorf("no /etc/dnsmasq.conf: --test exit %d, want 0 (vacuous, V-10)", code)
	}
	if h.FTL().Restarts() != 0 {
		t.Error("--test restarted FTL")
	}
}

// TC-74d (hazard control: pihole flush != cache flush): `pihole flush` empties
// the query log and leaves the DNS cache and FTL alone.
func TestTC74d_PiholeFlushIsQueryLog(t *testing.T) {
	h := v5test.NewFakeHost(t)
	if h.QueryLogLines() == 0 {
		t.Fatal("seed query log is empty; the control needs lines")
	}
	pid := h.FTL().PID()
	if code, _ := run(t, h, pihole, "flush"); code != 0 {
		t.Fatalf("flush exit %d", code)
	}
	if h.QueryLogLines() != 0 {
		t.Errorf("query log has %d lines after pihole flush", h.QueryLogLines())
	}
	if h.FTL().CacheFlushes() != 0 || h.FTL().PID() != pid || countEvents(h, "reload") != 0 {
		t.Errorf("pihole flush touched FTL: flushes=%d pid %d->%d events %v", h.FTL().CacheFlushes(), pid, h.FTL().PID(), h.FTLEvents())
	}
}

// TC-74e (V-17): with FTL stopped, `restartdns reload` and `reload-lists`
// exit 0 as no-ops and nothing answers; `restartdns` starts it.
func TestTC74e_ReloadIsNoOpWhenFTLDown(t *testing.T) {
	h := v5test.NewFakeHost(t)
	h.FTL().Stop()
	for _, argv := range [][]string{{pihole, "restartdns", "reload"}, {pihole, "restartdns", "reload-lists"}} {
		if code, _ := run(t, h, argv...); code != 0 {
			t.Errorf("%v with FTL down: exit %d, want 0", argv, code)
		}
	}
	if _, err := dnscheck.Query(context.Background(), h.FTL().Addr(), "pi.hole", 300*time.Millisecond); err == nil {
		t.Error("stopped FTL answered")
	}
	run(t, h, pihole, "restartdns")
	if a := query(t, h, "pi.hole"); a.RCode == "" {
		t.Error("restarted FTL does not answer")
	}
}

// TC-74f (hazard control: split-horizon): a public name under xi.example is
// forwarded upstream while only address= lines exist; a planted local=
// line makes FakeFTL answer NXDOMAIN, so a regression that writes local=
// is caught by any test that queries a public name.
func TestTC74f_LocalLineBreaksSplitHorizon(t *testing.T) {
	h := v5test.NewFakeHost(t)
	h.FTL().SetUpstream("www.xi.example", netip.MustParseAddr("203.0.113.10"))
	write(t, dnsFile(h), "address=/photos.xi.example/192.168.7.7\n")
	run(t, h, pihole, "restartdns")
	answers(t, h, "www.xi.example", "203.0.113.10")
	answers(t, h, "photos.xi.example", "192.168.7.7")
	appendLine(t, dnsFile(h), "local=/xi.example/")
	run(t, h, pihole, "restartdns")
	answers(t, h, "www.xi.example", "")
}

// TC-74g: the fault switches used by the pipeline tests behave as named.
func TestTC74g_Faults(t *testing.T) {
	h := v5test.NewFakeHost(t)
	write(t, dnsFile(h), "address=/kiwi.home.local/192.168.7.5\n")
	h.FTL().SetFault(v5test.FaultRestartFails)
	if code, _ := run(t, h, pihole, "restartdns"); code == 0 {
		t.Error("FaultRestartFails: restartdns exit 0")
	}
	h.FTL().SetFault(v5test.FaultWrongAnswer)
	run(t, h, pihole, "restartdns")
	if a := query(t, h, "kiwi.home.local"); len(a.A) == 1 && a.A[0] == netip.MustParseAddr("192.168.7.5") {
		t.Error("FaultWrongAnswer still answers the file's IP")
	}
	h.FTL().SetFault(v5test.FaultNone)
	run(t, h, pihole, "restartdns")
	answers(t, h, "kiwi.home.local", "192.168.7.5")

	// FaultNeverReady: the restart and the reload exit 0, FTL lives, and
	// nothing answers.
	h.FTL().SetFault(v5test.FaultNeverReady)
	for _, argv := range [][]string{{pihole, "restartdns"}, {pihole, "restartdns", "reload"}} {
		if code, _ := run(t, h, argv...); code != 0 || h.FTL().PID() == 0 {
			t.Errorf("FaultNeverReady %v: exit %d, pid %d; want 0 with FTL alive", argv, code, h.FTL().PID())
		}
		if _, err := dnscheck.Query(context.Background(), h.FTL().Addr(), "pi.hole", 300*time.Millisecond); err == nil {
			t.Errorf("FaultNeverReady %v: FTL answered", argv)
		}
	}
	h.FTL().SetFault(v5test.FaultNone)
	run(t, h, pihole, "restartdns")
	answers(t, h, "kiwi.home.local", "192.168.7.5")
}

// A-56, WI 0.1 check 2: `pihole restartdns` returns before the new FTL
// writes its PID file, so a read straight after the restart still names the
// old, live FTL; once the new FTL answers (and any lag passes) the file
// names the new PID and the old process is gone.
func TestFakeFTLPIDFileWrittenLate(t *testing.T) {
	h := v5test.NewFakeHost(t)
	h.FTL().SetPIDFileLag(time.Hour)
	run(t, h, pihole, "restartdns")
	newPID := h.FTL().PID()
	if newPID == v5test.FakeFTLPID {
		t.Fatal("restart kept the PID")
	}
	query(t, h, "pi.hole")
	if pid, ok := v5.FTLPID(h.Paths()); !ok || pid != v5test.FakeFTLPID {
		t.Errorf("FTLPID straight after the restart = %d, %v; want the old %d", pid, ok, v5test.FakeFTLPID)
	}

	h.FTL().SetPIDFileLag(0)
	run(t, h, pihole, "restartdns")
	newPID = h.FTL().PID()
	query(t, h, "pi.hole") // answered once the new FTL started, which writes the file
	deadline := time.Now().Add(2 * time.Second)
	for {
		pid, ok := v5.FTLPID(h.Paths())
		if ok && pid == newPID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("FTLPID = %d, %v; want the new %d", pid, ok, newPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(h.Root(), "proc", strconv.Itoa(v5test.FakeFTLPID))); err == nil {
		t.Error("the replaced FTL's /proc entry is still there")
	}
}

// A restart onto a file with an invalid address= line: dnsmasq dies at
// start (EC_BADCONF), so nothing answers until the line is fixed and FTL is
// restarted.
func TestFakeFTLRestartOntoBadConfig(t *testing.T) {
	h := v5test.NewFakeHost(t)
	write(t, dnsFile(h), "address=/bad.lan/300.1.1.1\n")
	run(t, h, pihole, "restartdns")
	if _, err := dnscheck.Query(context.Background(), h.FTL().Addr(), "pi.hole", 300*time.Millisecond); err == nil {
		t.Error("FTL answered with a bad conf line")
	}
	write(t, dnsFile(h), "address=/good.lan/192.168.7.3\n")
	run(t, h, pihole, "restartdns")
	answers(t, h, "good.lan", "192.168.7.3")
}

// FaultRollbackRestartFails (WI 4.1): the first restart after SetFault
// works and every later one fails with FTL stopped, so a pipeline's change
// restart succeeds and its rollback restart fails. Faults combine:
// FaultWrongAnswer still applies to the restart that worked, and a new
// SetFault starts the count again.
func TestFakeFTLRollbackRestartFails(t *testing.T) {
	h := v5test.NewFakeHost(t)
	write(t, dnsFile(h), "address=/kiwi.home.local/192.168.7.5\n")
	h.FTL().SetFault(v5test.FaultWrongAnswer, v5test.FaultRollbackRestartFails)
	if code, _ := run(t, h, pihole, "restartdns"); code != 0 {
		t.Fatalf("first restart after SetFault: exit %d, want 0", code)
	}
	if a := query(t, h, "kiwi.home.local"); len(a.A) != 1 || a.A[0] != v5test.WrongAnswerIP {
		t.Errorf("combined FaultWrongAnswer: kiwi.home.local = %+v, want %s", a, v5test.WrongAnswerIP)
	}
	for i := 0; i < 2; i++ {
		if code, _ := run(t, h, pihole, "restartdns"); code == 0 {
			t.Errorf("restart %d after the first: exit 0, want a failure", i+2)
		}
		if h.FTL().PID() != 0 {
			t.Errorf("restart %d after the first left FTL running (pid %d)", i+2, h.FTL().PID())
		}
	}
	if _, err := dnscheck.Query(context.Background(), h.FTL().Addr(), "pi.hole", 300*time.Millisecond); err == nil {
		t.Error("FTL answered after the failed rollback restart")
	}
	h.FTL().SetFault(v5test.FaultRollbackRestartFails)
	if code, _ := run(t, h, pihole, "restartdns"); code != 0 {
		t.Fatalf("first restart after a new SetFault: exit %d, want 0", code)
	}
	answers(t, h, "kiwi.home.local", "192.168.7.5")
	h.FTL().SetFault()
	h.Advance(time.Minute + time.Second) // Four starts so far; a sixth within 60 s is refused (A-60)
	for i := 0; i < 2; i++ {
		if code, _ := run(t, h, pihole, "restartdns"); code != 0 {
			t.Errorf("SetFault(): restart exit %d", code)
		}
	}
	answers(t, h, "kiwi.home.local", "192.168.7.5")
}

// ftlLog is FTL's log on the fake host from byte off on ("" when absent).
func ftlLog(t *testing.T, path string, off int64) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	if off > int64(len(b)) {
		return string(b)
	}
	return string(b[off:])
}

func logSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// eventually polls cond every 5 ms for up to 2 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// replies reports whether FakeFTL answers name at all within 300 ms.
func replies(h *v5test.FakeHost, name string) (dnscheck.Answer, bool) {
	a, err := dnscheck.Query(context.Background(), h.FTL().Addr(), name, 300*time.Millisecond)
	return a, err == nil
}

// The captured markers (PROVENANCE "FTL LIST RELOAD SIGNALS"); the test
// matches them by their own text, not the fake's constants.
const (
	markRTMIN    = "] Received: Real-time signal 0 (34 -> 0)\n"
	markHUP      = "] Reloading DNS cache\n"
	markCompiled = "] Compiled 0 whitelist and 0 blacklist regex filters for 1 clients in "
	markLocked   = "database is locked"
	markNoDB     = "Gravity database not available"
)

// lastRecord is the text of the log's last reload record, from its last
// start marker on.
func lastRecord(log string) string {
	i := max(strings.LastIndex(log, markRTMIN), strings.LastIndex(log, markHUP))
	if i < 0 {
		return log
	}
	return log[i:]
}

// TC-154 (NFR-7, A-64, A-65, A-66): FakeFTL encodes the asynchronous list
// reload and the reload race as TC-153 captured them on FTL v5.25.2.
func TestTC154_FakeFTLAsyncListReloadAndRace(t *testing.T) {
	t.Run("1-2 every reload is asynchronous", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			argv   []string
			marker string
		}{
			{"reload-lists", []string{pihole, "restartdns", "reload-lists"}, markRTMIN},
			{"reload", []string{pihole, "restartdns", "reload"}, markHUP},
			{"restart", []string{pihole, "restartdns"}, markHUP},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := v5test.NewFakeHost(t)
				f := h.FTL()
				f.SetReloadDuration(300 * time.Millisecond)
				path := f.LogPath()
				if path != filepath.Join(h.Root(), "var/log/pihole/FTL.log") {
					t.Fatalf("log path %s", path)
				}
				seed := ftlLog(t, path, 0)
				if !strings.Contains(seed, markCompiled) || strings.Contains(seed, markLocked) {
					t.Fatalf("seeded log should end in a completed reload:\n%s", seed)
				}
				off, before := logSize(t, path), f.ReloadsCompleted()
				if code, _ := run(t, h, tc.argv...); code != 0 {
					t.Fatalf("exit %d", code)
				}
				now := ftlLog(t, path, off)
				if !strings.Contains(now, tc.marker) || strings.Contains(now, markCompiled) || f.ReloadsCompleted() != before {
					t.Fatalf("at once: completed %d (was %d), log:\n%s", f.ReloadsCompleted(), before, now)
				}
				eventually(t, "the reload's completion", func() bool { return f.ReloadsCompleted() == before+1 })
				if got := ftlLog(t, path, off); !strings.Contains(got, markCompiled) || strings.Contains(got, markLocked) ||
					strings.Index(got, tc.marker) > strings.Index(got, markCompiled) {
					t.Errorf("after the reload:\n%s", got)
				}
				if f.Stuck() {
					t.Error("stuck after a clean reload")
				}
			})
		}
	})

	t.Run("3-4 a commit during a reload leaves FTL stuck until a clean reload", func(t *testing.T) {
		h := v5test.NewFakeHost(t)
		f := h.FTL()
		write(t, dnsFile(h), "address=/unity.lan/192.168.7.1\n")
		run(t, h, pihole, "restartdns")
		eventually(t, "the restart's reload", func() bool { _, ok := replies(h, "unity.lan"); return ok && !f.Reloading() })
		path := f.LogPath()
		off, before := logSize(t, path), f.ReloadsCompleted()
		f.HoldReloads()
		run(t, h, pihole, "restartdns", "reload-lists")
		h.SeedDomain(1, "raced.example.com", "")
		f.ReleaseReloads()
		eventually(t, "the held reload's end", func() bool { return f.ReloadsCompleted() == before+1 })
		rec := lastRecord(ftlLog(t, path, off))
		if !strings.Contains(rec, markLocked) || !strings.Contains(rec, markNoDB) || !f.Stuck() {
			t.Fatalf("stuck %v, record:\n%s", f.Stuck(), rec)
		}
		if a, ok := replies(h, "pi.hole"); !ok || len(a.A) != 1 {
			t.Errorf("pi.hole while stuck: %+v %v", a, ok)
		}
		for _, n := range []string{"x.invalid", "unity.lan"} {
			if a, ok := replies(h, n); ok {
				t.Errorf("%s answered while stuck: %+v", n, a)
			}
		}
		off = logSize(t, path)
		run(t, h, pihole, "restartdns", "reload-lists")
		eventually(t, "the clean reload", func() bool { return f.ReloadsCompleted() == before+2 })
		if rec := lastRecord(ftlLog(t, path, off)); f.Stuck() || strings.Contains(rec, markLocked) || !strings.Contains(rec, markCompiled) {
			t.Fatalf("stuck %v after a clean reload:\n%s", f.Stuck(), rec)
		}
		answers(t, h, "pi.hole", "127.0.0.1")
		answers(t, h, "x.invalid", "")
		answers(t, h, "unity.lan", "192.168.7.1")
	})

	t.Run("5 FailNextReloads ForceStuck ForceStuckUnlogged", func(t *testing.T) {
		h := v5test.NewFakeHost(t)
		f := h.FTL()
		path := f.LogPath()
		f.FailNextReloads(1)
		run(t, h, pihole, "restartdns", "reload-lists")
		eventually(t, "the failed reload", func() bool { return f.ReloadsCompleted() == 1 })
		if rec := lastRecord(ftlLog(t, path, 0)); !f.Stuck() || !strings.Contains(rec, markLocked) {
			t.Errorf("FailNextReloads(1): stuck %v, record:\n%s", f.Stuck(), rec)
		}
		run(t, h, pihole, "restartdns", "reload-lists")
		eventually(t, "the next reload", func() bool { return f.ReloadsCompleted() == 2 })
		if f.Stuck() {
			t.Error("FailNextReloads(1) failed more than one reload")
		}

		h = v5test.NewFakeHost(t)
		h.FTL().ForceStuck()
		if rec := lastRecord(ftlLog(t, h.FTL().LogPath(), 0)); !h.FTL().Stuck() || !strings.Contains(rec, markLocked) {
			t.Errorf("ForceStuck: stuck %v, record:\n%s", h.FTL().Stuck(), rec)
		}
		if _, ok := replies(h, "x.invalid"); ok {
			t.Error("ForceStuck: x.invalid answered")
		}

		h = v5test.NewFakeHost(t)
		off := logSize(t, h.FTL().LogPath())
		h.FTL().ForceStuckUnlogged()
		log := ftlLog(t, h.FTL().LogPath(), 0)
		if rec := lastRecord(log); !h.FTL().Stuck() || strings.Contains(rec, markLocked) || !strings.Contains(rec, markCompiled) ||
			logSize(t, h.FTL().LogPath()) != off {
			t.Errorf("ForceStuckUnlogged: stuck %v, record:\n%s", h.FTL().Stuck(), rec)
		}
		if _, ok := replies(h, "x.invalid"); ok {
			t.Error("ForceStuckUnlogged: x.invalid answered")
		}
	})

	t.Run("6 a stopped FTL reloads nothing", func(t *testing.T) {
		h := v5test.NewFakeHost(t)
		path := h.FTL().LogPath()
		h.FTL().Stop()
		off := logSize(t, path)
		if code, _ := run(t, h, pihole, "restartdns", "reload-lists"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		time.Sleep(3 * v5test.DefaultReloadDuration)
		if got := ftlLog(t, path, off); got != "" || h.FTL().Stuck() || h.FTL().ReloadsCompleted() != 0 {
			t.Errorf("stopped FTL: logged %q, stuck %v, completed %d", got, h.FTL().Stuck(), h.FTL().ReloadsCompleted())
		}
	})

	t.Run("7 LOGFILE moves the log", func(t *testing.T) {
		h := v5test.NewFakeHost(t)
		def := h.FTL().LogPath()
		off := logSize(t, def)
		write(t, filepath.Join(h.Root(), "etc/pihole/pihole-FTL.conf"), "PRIVACYLEVEL=0\nLOGFILE=/var/log/alt/FTL.log\n")
		alt := filepath.Join(h.Root(), "var/log/alt/FTL.log")
		if h.FTL().LogPath() != alt {
			t.Fatalf("log path %s, want %s", h.FTL().LogPath(), alt)
		}
		run(t, h, pihole, "restartdns", "reload-lists")
		eventually(t, "the reload", func() bool { return h.FTL().ReloadsCompleted() == 1 })
		if got := ftlLog(t, alt, 0); !strings.Contains(got, markRTMIN) || !strings.Contains(got, markCompiled) {
			t.Errorf("alternate log:\n%s", got)
		}
		if got := ftlLog(t, def, off); got != "" {
			t.Errorf("default log got %q", got)
		}
	})
}
