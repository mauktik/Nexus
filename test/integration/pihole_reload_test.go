//go:build integration

package integration

// Cases: TC-158 and TC-159 in planning/pihole-dns-zones/test-cases.md (WI
// 5.6): the FTL list-reload race on the real Pi-hole v5 image. FTL v5.25.2
// re-reads gravity.db on its database thread after `pihole restartdns
// reload-lists` has returned; a gravity.db commit during that re-read fails
// it, and FTL then answers pi.hole only, local records included, until a
// later reload succeeds (PROVENANCE "FTL RELOAD RACE", "FTL LIST RELOAD
// SIGNALS"). Every expected value is read from the host: dig @127.0.0.1,
// FTL's own log, pihole-FTL sqlite3.

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
)

const (
	phFTLLog = "/var/log/pihole/FTL.log"
	// phRaceComment marks every row these tests seed with pihole-FTL sqlite3.
	phRaceComment = "nexus-race"
)

// phLogSize is FTL.log's size.
func phLogSize(t *testing.T) int64 {
	t.Helper()
	fi, err := os.Stat(phFTLLog)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// phLogSince is FTL.log's whole lines from byte off on.
func phLogSince(t *testing.T, off int64) []string {
	t.Helper()
	b, err := os.ReadFile(phFTLLog)
	if err != nil {
		t.Fatal(err)
	}
	if off > int64(len(b)) {
		off = 0
	}
	s := string(b[off:])
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	} else {
		return nil
	}
	return strings.Split(s, "\n")
}

// phLogTime is a FTL.log line's timestamp, "[2006-01-02 15:04:05.000 …]"
// in local time (FTL v5.25.2 src/log.c get_timestr).
func phLogTime(line string) (time.Time, bool) {
	if len(line) < 24 || line[0] != '[' {
		return time.Time{}, false
	}
	ts, err := time.ParseInLocation("2006-01-02 15:04:05.000", line[1:24], time.Local)
	return ts, err == nil
}

// phLockedLines counts FTL.log's "database is locked" lines.
func phLockedLines(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(phFTLLog)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "database is locked")
}

// phReloadLists runs `pihole restartdns reload-lists` and waits for FTL's
// completion line, "Compiled … regex filters …" (src/regex.c L744), after
// it. It returns the time from FTL's start line to that one.
func phReloadLists(t *testing.T) time.Duration {
	t.Helper()
	off := phLogSize(t)
	mustSh(t, phPihole, "restartdns", "reload-lists")
	var d time.Duration
	waitUntil(t, 2*time.Minute, "FTL's list reload to complete", func() (bool, string) {
		var start, end time.Time
		for _, l := range phLogSince(t, off) {
			switch {
			case strings.Contains(l, "] Received: Real-time signal 0 ("):
				start, _ = phLogTime(l)
			case strings.Contains(l, "] Compiled ") && strings.Contains(l, " regex filters for "):
				end, _ = phLogTime(l)
			}
		}
		if start.IsZero() || end.IsZero() {
			return false, "no completion line yet"
		}
		d = end.Sub(start)
		return true, ""
	})
	return d
}

// phSeedRaceRegexes widens FTL's list reload (TC-158 setup): R enabled
// regex deny entries ^nexus-race-<n>\.invalid$, doubling R from 5,000 until
// a reload takes 1.5 s or R is 80,000. It returns R and the reload time.
func phSeedRaceRegexes(t *testing.T) (int, time.Duration) {
	t.Helper()
	var d time.Duration
	r := 5000
	for {
		phSQLWrite(t, fmt.Sprintf("BEGIN IMMEDIATE; DELETE FROM domainlist WHERE comment = '%s' AND type = 3; "+
			"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d) "+
			"INSERT INTO domainlist (type, domain, enabled, comment) "+
			`SELECT 3, '^nexus-race-' || i || '\.invalid$', 1, '%s' FROM n; COMMIT;`, phRaceComment, r, phRaceComment))
		phReloadLists(t) // the first reload after a change compiles a new set
		d = phReloadLists(t)
		t.Logf("seeded %d regex deny entries: a reload-lists takes %s (FTL's start line to its completion line)", r, d)
		if d >= 1500*time.Millisecond || r >= 80000 {
			return r, d
		}
		r *= 2
	}
}

// phCleanRace removes every row the race tests seeded, reloads FTL's lists
// and requires FTL healthy.
func phCleanRace(t *testing.T) {
	t.Helper()
	phSQLWrite(t, "BEGIN IMMEDIATE; "+
		"DELETE FROM domainlist WHERE comment = '"+phRaceComment+"' OR domain LIKE 'race-%.example' "+
		"OR domain LIKE 'nexus-stuck-%.example'; "+
		"DELETE FROM gravity WHERE adlist_id IN (SELECT id FROM adlist WHERE address LIKE '%/race-%.txt'); "+
		"DELETE FROM adlist_by_group WHERE adlist_id IN (SELECT id FROM adlist WHERE address LIKE '%/race-%.txt'); "+
		"DELETE FROM adlist WHERE address LIKE '%/race-%.txt'; COMMIT;")
	t.Logf("cleanup: reload-lists took %s", phReloadLists(t))
	phRequireFTLHealthy(t)
}

// phPollOp polls an operation every 20 ms until it is terminal, so the
// test sees each Succeeded as soon as it is recorded.
func phPollOp(t *testing.T, n *nexusdInstance, id string, timeout time.Duration) apitypes.Operation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		op := n.operation(id)
		if terminal(op.Status) {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s still %s after %s", id, op.Status, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func phSplitAnswers(a phAnswer) bool { return len(a.addrs) == 1 && a.addrs[0] == phSplitIP }

// TC-158 (FR-10, FR-12, NFR-7, A-64, A-67) [hazard: reload-race]:
// back-to-back domain and adlist operations through nexusd leave FTL
// answering, and each domain is blocked as soon as its operation succeeds.
// Before WI 5.6 an operation returned as soon as `pihole restartdns
// reload-lists` exited, and the next operation in the lane could commit to
// gravity.db while FTL re-read it.
func TestPiholeListReloadRace(t *testing.T) {
	phRequireFTLHealthy(t)
	t.Cleanup(func() { phCleanRace(t) })
	l0 := phLockedLines(t)
	r, d := phSeedRaceRegexes(t)
	t.Logf("setup: %d 'database is locked' lines in FTL.log (L0); R = %d regexes, reload D = %s", l0, r, d)
	n := piholed(t, "tc158", "")
	var failures []string
	fail := func(format string, args ...any) {
		failures = append(failures, fmt.Sprintf(format, args...))
		t.Errorf(format, args...)
	}

	// 1. Burst: 10 add-domain operations posted without waiting.
	const burst = 10
	ops := make([]apitypes.Operation, burst)
	for i := range ops {
		ops[i] = phStart(t, n, "/actions/add-domain", fmt.Sprintf(`{"list":"deny","kind":"exact","value":"race-b%d.example"}`, i))
	}
	for i, op := range ops {
		op = phPollOp(t, n, op.ID, 5*time.Minute)
		name := fmt.Sprintf("race-b%d.example", i)
		if op.Status != apitypes.StatusSucceeded {
			fail("burst %d: add-domain %s ended %s: %+v (an op failing on a locked database)", i, name, op.Status, op.Error)
			continue
		}
		blocked, split := phDig(name), phDig(phSplit)
		t.Logf("burst %d: add-domain %s succeeded in %s; dig %s: %s; dig %s: %s", i, name, phTook(op), name, blocked, phSplit, split)
		if !phIsBlocked(blocked) {
			fail("burst %d: %s not blocked (%s) at the first dig after its op succeeded", i, name, blocked)
		}
		if !phSplitAnswers(split) {
			fail("burst %d: dig %s: %s (FTL stuck)", i, phSplit, split)
		}
	}

	// 2. Pairs: add-domain, then at once add-adlist (no gravity), so the
	// adlist's write runs right behind the domain's reload.
	const pairs = 10
	for i := range pairs {
		name := fmt.Sprintf("race-a%d.example", i)
		url := phFixture(t, fmt.Sprintf("race-%d.txt", i), []string{fmt.Sprintf("race-list-%d.example", i)})
		dop := phStart(t, n, "/actions/add-domain", fmt.Sprintf(`{"list":"deny","kind":"exact","value":%q}`, name))
		aop := phStart(t, n, "/actions/add-adlist", fmt.Sprintf(`{"url":%q}`, url))
		dop, aop = phPollOp(t, n, dop.ID, 5*time.Minute), phPollOp(t, n, aop.ID, 5*time.Minute)
		split, blocked := phDig(phSplit), phDig(name)
		t.Logf("pair %d: add-domain %s %s in %s, add-adlist %s %s in %s; dig %s: %s; dig %s: %s", i, name, dop.Status,
			phTook(dop), url, aop.Status, phTook(aop), phSplit, split, name, blocked)
		if dop.Status != apitypes.StatusSucceeded || aop.Status != apitypes.StatusSucceeded {
			fail("pair %d: add-domain %s %+v, add-adlist %s %+v", i, dop.Status, dop.Error, aop.Status, aop.Error)
		}
		if !phSplitAnswers(split) {
			fail("pair %d: dig %s: %s (FTL stuck)", i, phSplit, split)
		}
		if !phIsBlocked(blocked) {
			fail("pair %d: %s not blocked (%s)", i, name, blocked)
		}
	}

	// 3. No reload met a write.
	l1 := phLockedLines(t)
	t.Logf("FTL.log 'database is locked' lines: L0 %d, L1 %d", l0, l1)
	if l1 != l0 {
		fail("%d new 'database is locked' lines in FTL.log: a list reload met a gravity.db write", l1-l0)
		phFTLDiag(t)
	}
	t.Logf("TC-158 summary: R %d, D %s, %d failures", r, d, len(failures))
}

// phStuck reports FTL stuck: more 'database is locked' lines than before,
// www.xi.example unanswered, pi.hole answered.
func phStuck(t *testing.T, before int) bool {
	t.Helper()
	return phLockedLines(t) > before && phDig(phSplit).status == "" && phAnswers()
}

// phMakeStuck makes FTL stuck outside Nexus and says how, or returns "".
// First as TC-153 step 6 did, up to 20 rounds: `pihole -w X` writes and
// reloads, and `pihole -b X` writes during that reload. With a reload
// seconds long, -b's write mostly loses the lock instead, so then, up to 3
// times, a write lock is held across a reload's start: a sqlite3 shell
// holds BEGIN EXCLUSIVE for 3 s while `pihole restartdns reload-lists`
// runs, and FTL's gravityDB_open meets "database is locked" (PROVENANCE
// "FTL LIST RELOAD SIGNALS").
func phMakeStuck(t *testing.T) string {
	t.Helper()
	for i := 1; i <= 20; i++ {
		x := fmt.Sprintf("nexus-stuck-%d.example", i)
		before := phLockedLines(t)
		phBashCode(phPihole + " -w " + x + " && " + phPihole + " -b " + x)
		time.Sleep(2 * time.Second)
		if phStuck(t, before) {
			return fmt.Sprintf("pihole -w X && pihole -b X, round %d", i)
		}
	}
	for i := 1; i <= 3; i++ {
		before := phLockedLines(t)
		phBashCode("( echo 'BEGIN EXCLUSIVE;'; sleep 3; echo 'COMMIT;' ) | " + phFTLBin + " sqlite3 " + phGravityDB + " & " +
			"sleep 0.5; " + phPihole + " restartdns reload-lists; wait")
		time.Sleep(time.Second)
		if phStuck(t, before) {
			return fmt.Sprintf("a 3 s BEGIN EXCLUSIVE across reload-lists, attempt %d (20 hand rounds did not)", i)
		}
	}
	return ""
}

// TC-159 (FR-12, NFR-7, A-68, A-69) [hazard: reload-race]: FTL made stuck
// by hand, outside Nexus, as a web UI write racing a reload would; an
// add-adlist through nexusd reloads FTL's lists before it writes, and FTL
// answers afterwards. Before WI 5.6, FTL stayed stuck: an adlist add sends
// no reload.
func TestPiholeStuckFTLRepairedBeforeWrite(t *testing.T) {
	phRequireFTLHealthy(t)
	t.Cleanup(func() { phCleanRace(t) })
	r, d := phSeedRaceRegexes(t)
	n := piholed(t, "tc159", "")

	how := phMakeStuck(t)
	if how == "" {
		t.Skipf("TC-159 unverified on T1: 20 hand rounds and 3 held-lock reloads (R %d, D %s) did not make FTL stuck", r, d)
	}
	t.Logf("FTL stuck by hand, %s (R %d, D %s): dig %s %s, dig pi.hole answers", how, r, d, phSplit, phDig(phSplit))

	url := phFixture(t, "race-r.txt", []string{"race-r.example"})
	op := phStart(t, n, "/actions/add-adlist", fmt.Sprintf(`{"url":%q}`, url))
	log := phFollow(t, n, op.ID, nil)
	op = phPollOp(t, n, op.ID, 5*time.Minute)
	split := phDig(phSplit)
	t.Logf("add-adlist %s: %s in %s; dig %s: %s; op log:\n%s", url, op.Status, phTook(op), phSplit, split, log)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("add-adlist: %s %+v", op.Status, op.Error)
	}
	repair := strings.Index(log, "pihole: running pihole restartdns reload-lists")
	add := strings.Index(log, "pihole: running pihole -a adlist add")
	if repair < 0 || add < 0 || repair > add {
		t.Errorf("op log: want a recovery reload before `pihole -a adlist add`")
	}
	if !phSplitAnswers(split) {
		t.Errorf("dig %s after the op: %s (FTL still stuck)", phSplit, split)
		phFTLDiag(t)
	}
}
