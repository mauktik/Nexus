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
	"regexp"
	"strconv"
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

// phDigRetries is how often phDigRetried re-asks after no answer.
const phDigRetries = 2

// phDigRetried asks for name as a stub resolver does: an unanswered dig is
// retried up to phDigRetries times, 1 s apart, each with phDig's 6 s
// timeout. FTL v5.25.2 drops, with no reply, a query whose gravity check
// meets a gravity.db write lock (REPLY_WHEN_BUSY=DROP by default, busy
// timeout 0; it logs a "Gravity database is busy" triple), and the first
// query for a name after a list reload always checks gravity.db. A commit
// failing one concurrent query that way is out of scope (A-69); a stuck FTL
// leaves the name unanswered across every try. It returns the first answer,
// the last one and the number of tries.
func phDigRetried(name string) (first, last phAnswer, tries int) {
	first = phDig(name)
	last, tries = first, 1
	for ; last.status == "" && tries <= phDigRetries; tries++ {
		time.Sleep(time.Second)
		last = phDig(name)
	}
	return first, last, tries
}

// phBusyLines counts the "Gravity database is busy" lines in lines.
func phBusyLines(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "Gravity database is busy") {
			n++
		}
	}
	return n
}

// phStartHealthy starts a race test from a FTL with its lists loaded. The
// harness setup's own `pihole -a adlist del`, right after `pihole -g`, can
// meet the reload that `pihole -g` ends with (gravity.sh L946, `pihole
// restartdns reload`, SIGHUP; PROVENANCE "GRAVITY ENDS WITH restartdns
// reload") and leave FTL stuck before any test runs (seen once in six native
// runs: www.xi.example unanswered from the start); a reload-lists ends that
// state. Whether FTL answered first is
// logged, then it must be healthy.
func phStartHealthy(t *testing.T) {
	t.Helper()
	t.Logf("before the test: dig %s: %s; reload-lists took %s", phSplit, phDig(phSplit), phReloadLists(t))
	phRequireFTLHealthy(t)
}

// TC-158 (FR-10, FR-12, NFR-7, A-64, A-67) [hazard: reload-race]:
// back-to-back domain and adlist operations through nexusd leave FTL
// answering, and each domain is blocked as soon as its operation succeeds.
// Before WI 5.6 an operation returned as soon as `pihole restartdns
// reload-lists` exited, and the next operation in the lane could commit to
// gravity.db while FTL re-read it. Each dig is retried as a stub resolver
// would (phDigRetried): it lands, by the watcher's tick, inside the next
// operation's commit, which can make FTL drop that one query (A-69). FTL is
// stuck when a name stays unanswered across every try, or when new
// "database is locked" lines appear (A-64).
func TestPiholeListReloadRace(t *testing.T) {
	phStartHealthy(t)
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
	// explain logs, after a name stayed unanswered or answered wrong, what
	// FTL logged since off and whether it answers 2 s later.
	explain := func(off int64, name string) {
		time.Sleep(2 * time.Second)
		lines := phLogSince(t, off)
		if len(lines) > 40 {
			lines = lines[len(lines)-40:]
		}
		t.Logf("2 s later: dig %s: %s, dig %s: %s; FTL.log since the step:\n%s", name, phDig(name), phSplit, phDig(phSplit),
			strings.Join(lines, "\n"))
	}
	// dig asks for name with retries (phDigRetried). An answer on a retry is
	// logged as A-69's busy drop, with the "Gravity database is busy" lines
	// FTL logged since off; it is not a failure. It returns the last answer
	// and whether a try went unanswered.
	drops := 0
	dig := func(step string, off int64, name string) (phAnswer, bool) {
		first, last, tries := phDigRetried(name)
		if tries == 1 {
			return last, false
		}
		if last.status != "" {
			drops++
			t.Logf("%s: A-69 busy drop: dig %s went unanswered (%s), then answered %s on try %d; "+
				"FTL.log has %d 'Gravity database is busy' lines since the step", step, name, first, last, tries,
				phBusyLines(phLogSince(t, off)))
		}
		return last, true
	}
	// judge fails a step whose names stayed unanswered across every try
	// (FTL stuck), whose domain answered anything but blocked, or whose
	// www.xi.example answered anything but its local record.
	judge := func(step string, off int64, name string, blocked, split phAnswer, retried bool) {
		ok := true
		switch {
		case blocked.status == "":
			fail("%s: dig %s unanswered across %d tries (FTL stuck)", step, name, 1+phDigRetries)
			ok = false
		case !phIsBlocked(blocked):
			fail("%s: %s not blocked (%s) at the first answered dig after its op succeeded", step, name, blocked)
			ok = false
		}
		switch {
		case split.status == "":
			fail("%s: dig %s unanswered across %d tries (FTL stuck)", step, phSplit, 1+phDigRetries)
			ok = false
		case !phSplitAnswers(split):
			fail("%s: dig %s: %s, want %s", step, phSplit, split, phSplitIP)
			ok = false
		}
		if !ok || retried {
			explain(off, name)
		}
	}

	// 1. Burst: 10 add-domain operations posted without waiting.
	const burst = 10
	ops := make([]apitypes.Operation, burst)
	for i := range ops {
		ops[i] = phStart(t, n, "/actions/add-domain", fmt.Sprintf(`{"list":"deny","kind":"exact","value":"race-b%d.example"}`, i))
	}
	for i, op := range ops {
		off := phLogSize(t)
		op = phPollOp(t, n, op.ID, 5*time.Minute)
		name := fmt.Sprintf("race-b%d.example", i)
		step := fmt.Sprintf("burst %d", i)
		if op.Status != apitypes.StatusSucceeded {
			fail("%s: add-domain %s ended %s: %+v (an op failing on a locked database)", step, name, op.Status, op.Error)
			continue
		}
		blocked, bRetried := dig(step, off, name)
		split, sRetried := dig(step, off, phSplit)
		t.Logf("%s: add-domain %s succeeded in %s; dig %s: %s; dig %s: %s", step, name, phTook(op), name, blocked, phSplit, split)
		judge(step, off, name, blocked, split, bRetried || sRetried)
	}

	// 2. Pairs: add-domain, then at once add-adlist (no gravity), so the
	// adlist's write runs right behind the domain's reload.
	const pairs = 10
	for i := range pairs {
		name := fmt.Sprintf("race-a%d.example", i)
		step := fmt.Sprintf("pair %d", i)
		url := phFixture(t, fmt.Sprintf("race-%d.txt", i), []string{fmt.Sprintf("race-list-%d.example", i)})
		off := phLogSize(t)
		dop := phStart(t, n, "/actions/add-domain", fmt.Sprintf(`{"list":"deny","kind":"exact","value":%q}`, name))
		aop := phStart(t, n, "/actions/add-adlist", fmt.Sprintf(`{"url":%q}`, url))
		dop, aop = phPollOp(t, n, dop.ID, 5*time.Minute), phPollOp(t, n, aop.ID, 5*time.Minute)
		split, sRetried := dig(step, off, phSplit)
		blocked, bRetried := dig(step, off, name)
		t.Logf("%s: add-domain %s %s in %s, add-adlist %s %s in %s; dig %s: %s; dig %s: %s", step, name, dop.Status,
			phTook(dop), url, aop.Status, phTook(aop), phSplit, split, name, blocked)
		if dop.Status != apitypes.StatusSucceeded || aop.Status != apitypes.StatusSucceeded {
			fail("%s: add-domain %s %+v, add-adlist %s %+v", step, dop.Status, dop.Error, aop.Status, aop.Error)
		}
		judge(step, off, name, blocked, split, bRetried || sRetried)
	}

	// 3. No reload met a write.
	l1 := phLockedLines(t)
	t.Logf("FTL.log 'database is locked' lines: L0 %d, L1 %d", l0, l1)
	if l1 != l0 {
		fail("%d new 'database is locked' lines in FTL.log: a list reload met a gravity.db write (FTL stuck)", l1-l0)
		phFTLDiag(t)
	}
	t.Logf("TC-158 summary: R %d, D %s, %d failures, %d A-69 busy drops answered on a retry", r, d, len(failures), drops)
}

// phStuck reports FTL stuck: more 'database is locked' lines than before,
// www.xi.example unanswered, pi.hole answered.
func phStuck(t *testing.T, before int) bool {
	t.Helper()
	return phLockedLines(t) > before && phDig(phSplit).status == "" && phAnswers()
}

// phWaitReloadsQuiet waits until FTL's log has had no reload start or
// completion line for quiet, for up to a minute.
func phWaitReloadsQuiet(t *testing.T, quiet time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		off := phLogSize(t)
		time.Sleep(quiet)
		busy := false
		for _, l := range phLogSince(t, off) {
			if strings.Contains(l, "] Received: Real-time signal 0 (") || strings.Contains(l, "] Reloading DNS cache") ||
				strings.Contains(l, "] Compiled ") {
				busy = true
			}
		}
		if !busy {
			return
		}
	}
	t.Fatal("FTL's log did not go quiet within a minute")
}

// phMakeStuck makes FTL stuck outside Nexus and says how, or returns "".
// First as TC-153 step 6 did, up to 20 rounds: `pihole -w X` writes and
// reloads, and `pihole -b X` writes during that reload. With a reload
// seconds long, -b's write mostly loses the lock instead, so then, up to 3
// times, a write lock is held across a reload's start: once FTL has no
// reload in flight (one in flight holds a read lock, and BEGIN EXCLUSIVE
// would fail at once), a sqlite3 shell holds BEGIN EXCLUSIVE for 4 s while
// `pihole restartdns reload-lists` runs, and FTL's gravityDB_open meets
// "database is locked" (PROVENANCE "FTL LIST RELOAD SIGNALS").
func phMakeStuck(t *testing.T, d time.Duration) string {
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
		phWaitReloadsQuiet(t, d+time.Second)
		before := phLockedLines(t)
		out, code := phBashCode("( echo '.timeout 10000'; echo 'BEGIN EXCLUSIVE;'; sleep 4; echo 'COMMIT;' ) | " +
			phFTLBin + " sqlite3 " + phGravityDB + " & sleep 1; " + phPihole + " restartdns reload-lists; wait")
		time.Sleep(time.Second)
		if phStuck(t, before) {
			return fmt.Sprintf("a 4 s BEGIN EXCLUSIVE across reload-lists, attempt %d (20 hand rounds did not)", i)
		}
		t.Logf("held-lock attempt %d did not make FTL stuck (exit %d): %s", i, code, strings.TrimSpace(out))
	}
	return ""
}

// TC-159 (FR-12, NFR-7, A-68, A-69) [hazard: reload-race]: FTL made stuck
// by hand, outside Nexus, as a web UI write racing a reload would; an
// add-adlist through nexusd reloads FTL's lists before it writes, and FTL
// answers afterwards. Before WI 5.6, FTL stayed stuck: an adlist add sends
// no reload.
func TestPiholeStuckFTLRepairedBeforeWrite(t *testing.T) {
	phStartHealthy(t)
	t.Cleanup(func() { phCleanRace(t) })
	r, d := phSeedRaceRegexes(t)
	n := piholed(t, "tc159", "")

	how := phMakeStuck(t, d)
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

// phCompiledRe is a completion line's "in X msec": how long the reload's
// regex read took, which ends at the line's own timestamp.
var phCompiledRe = regexp.MustCompile(`\] Compiled [0-9]+ whitelist and [0-9]+ blacklist regex filters for [0-9]+ clients in ([0-9.]+) msec$`)

// Regression (WI 5.6, Phase 5 verification) [hazard: reload-race]: a write
// that arrives just after FTL logged the completion of a reload with a
// second reload queued behind it must wait for that queued reload too. Two
// `pihole restartdns reload-lists` outside Nexus, the second while the first
// runs, make FTL's database thread reload once more after the first
// completes (the event flag, src/signals.c L290), with no start line of its
// own; it starts after the thread's 100 ms sleep (src/database/
// database-thread.c L128). The test waits for the first completion line and
// at once posts an add-domain through nexusd. Its pre-write check reads a
// record that has ended with two start lines. The commit's time is
// gravity.db's mtime after the op (nothing else writes it meanwhile; FTL
// only reads it): it raced if it fell inside a reload's read, which ends at
// a completion line and took that line's "in X msec" (plus the reopen and
// count before it). The user-visible symptoms are asserted as well: a new
// "database is locked" or "Gravity database not available" line, FTL not
// answering www.xi.example, the domain not blocked.
func TestPiholeWriteWaitsForQueuedReload(t *testing.T) {
	phStartHealthy(t)
	t.Cleanup(func() { phCleanRace(t) })
	r, d := phSeedRaceRegexes(t)
	n := piholed(t, "queued", "")
	for round := range 3 {
		name := fmt.Sprintf("race-q%d.example", round)
		phWaitReloadsQuiet(t, d+time.Second)
		off := phLogSize(t)
		mustSh(t, phPihole, "restartdns", "reload-lists")
		time.Sleep(300 * time.Millisecond)
		mustSh(t, phPihole, "restartdns", "reload-lists")
		var first time.Time
		for deadline := time.Now().Add(time.Minute); first.IsZero(); time.Sleep(2 * time.Millisecond) {
			for _, l := range phLogSince(t, off) {
				if phCompiledRe.MatchString(l) {
					first, _ = phLogTime(l)
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("no completion line within a minute")
			}
		}
		posted := time.Now()
		op := phStart(t, n, "/actions/add-domain", fmt.Sprintf(`{"list":"deny","kind":"exact","value":%q}`, name))
		log := phFollow(t, n, op.ID, nil)
		op = phPollOp(t, n, op.ID, 5*time.Minute)
		fi, err := os.Stat(phGravityDB)
		if err != nil {
			t.Fatal(err)
		}
		committed := fi.ModTime()
		phWaitReloadsQuiet(t, d+time.Second)
		split, blocked := phDig(phSplit), phDig(name)
		lines := phLogSince(t, off)
		var failed, raced []string
		var signalled time.Time // the first start line after the commit: Nexus's own reload-lists
		compiled := 0
		for _, l := range lines {
			if strings.Contains(l, "database is locked") || strings.Contains(l, "Gravity database not available") {
				failed = append(failed, l)
			}
			if ts, ok := phLogTime(l); ok && signalled.IsZero() && ts.After(committed) &&
				strings.Contains(l, "] Received: Real-time signal 0 (") {
				signalled = ts
			}
			m := phCompiledRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			compiled++
			// The reload's read of the regexes took m[1] up to this line;
			// its reopen and counts before that take a few ms more. A
			// reload whose read began after a start line that followed
			// the commit was started after the commit.
			end, _ := phLogTime(l)
			ms, _ := strconv.ParseFloat(m[1], 64)
			read := end.Add(-time.Duration(ms * float64(time.Millisecond)))
			if !signalled.IsZero() && signalled.Before(read) {
				continue
			}
			if committed.After(read.Add(-30*time.Millisecond)) && committed.Before(end.Add(time.Millisecond)) {
				raced = append(raced, l)
			}
		}
		nexusReloads := strings.Count(log, "pihole: running pihole restartdns reload-lists")
		t.Logf("round %d (R %d, D %s): first completion %s, posted %s, committed %s (gravity.db mtime); add-domain %s in %s; "+
			"%d completion lines, %d from Nexus's reloads; dig %s: %s; dig %s: %s\nop log:\n%s\nFTL.log since the first signal:\n%s",
			round, r, d, first.Format("15:04:05.000"), posted.Format("15:04:05.000"), committed.Format("15:04:05.000"),
			op.Status, phTook(op), compiled, nexusReloads, phSplit, split, name, blocked, log, strings.Join(lines, "\n"))
		if compiled < 2+nexusReloads {
			t.Errorf("round %d: the second signal did not queue a reload (%d completion lines): the case was not set up", round, compiled)
		}
		if op.Status != apitypes.StatusSucceeded {
			t.Errorf("round %d: add-domain %s %+v", round, op.Status, op.Error)
		}
		if len(raced) > 0 {
			t.Errorf("round %d: the commit at %s fell inside a reload's read of gravity.db:\n%s", round,
				committed.Format("15:04:05.000"), strings.Join(raced, "\n"))
		}
		if len(failed) > 0 {
			t.Errorf("round %d: a list reload failed (it met the commit):\n%s", round, strings.Join(failed, "\n"))
		}
		if !phSplitAnswers(split) {
			t.Errorf("round %d: dig %s after the op: %s (FTL stuck)", round, phSplit, split)
			phFTLDiag(t)
		}
		if !phIsBlocked(blocked) {
			t.Errorf("round %d: %s not blocked after the op: %s", round, name, blocked)
		}
		if t.Failed() {
			return
		}
	}
}
