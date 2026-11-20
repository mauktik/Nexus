// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	phspec "github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// phFollow streams operation id's output with follow=true until it ends,
// calling each with every frame's arrival time and data.
func phFollow(t *testing.T, n *nexusdInstance, id string, each func(time.Time, string)) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+n.listen+"/v1/operations/"+id+"/logs?follow=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("follow %s: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("follow %s: %d %s", id, resp.StatusCode, b)
	}
	var out strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		out.WriteString(f.D)
		if each != nil && f.D != "" {
			each(time.Now(), f.D)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("follow %s: %v", id, err)
	}
	return out.String()
}

// phCLIHome is a HOME whose nexus CLI configuration names n as its default
// host, with n's token.
func phCLIHome(t *testing.T, n *nexusdInstance) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("defaultHost: harness\nhosts:\n  harness: {url: \"http://%s\", token: %s}\n", n.listen, n.token)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// phFixture writes a list into the fixture-http document root, removed at
// cleanup, and returns its URL.
func phFixture(t *testing.T, name string, domains []string) string {
	t.Helper()
	path := filepath.Join(phFixtureRoot, name)
	if err := os.WriteFile(path, []byte(strings.Join(domains, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return phFixtureURL + "/" + name
}

func phDomains(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%d.example", prefix, i+1)
	}
	return out
}

// phWaitDig polls dig until want holds, for up to timeout, and returns the
// last answer and how long it took.
func phWaitDig(name string, timeout time.Duration, want func(phAnswer) bool) (phAnswer, time.Duration, bool) {
	start := time.Now()
	for {
		a := phDig(name)
		if want(a) {
			return a, time.Since(start), true
		}
		if time.Since(start) > timeout {
			return a, time.Since(start), false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// phIsBlocked is FTL's blocked answer in the default NULL blocking mode
// (no BLOCKINGMODE in pihole-FTL.conf): 0.0.0.0.
func phIsBlocked(a phAnswer) bool {
	return a.status == "NOERROR" && len(a.addrs) == 1 && a.addrs[0] == "0.0.0.0"
}

// phIsUnblocked is the answer for a name FTL does not block, with the stub
// upstream behind it: NXDOMAIN, which a dig that timed out (no status) is
// not. Every other name of the harness is NXDOMAIN there (PROVENANCE
// "STUB UPSTREAM").
func phIsUnblocked(a phAnswer) bool { return a.status == "NXDOMAIN" && len(a.addrs) == 0 }

// phQueryLogCounts are the query log's row count in pihole-FTL.db and the
// line count of pihole.log, as the host reads them.
func phQueryLogCounts(t *testing.T) (int, int) {
	t.Helper()
	db, err := strconv.Atoi(strings.TrimSpace(mustSh(t, phFTLBin, "sqlite3", "-readonly", "/etc/pihole/pihole-FTL.db",
		"SELECT count(*) FROM queries")))
	if err != nil {
		t.Fatal(err)
	}
	log, err := strconv.Atoi(strings.Fields(mustSh(t, "wc", "-l", "/var/log/pihole/pihole.log"))[0])
	if err != nil {
		t.Fatal(err)
	}
	return db, log
}

// TC-123: restart-dns gives FTL a new PID, which ftlPidAfter names, and FTL
// answers (A-56); flush-cache keeps the PID, FTL answers, and the query log
// is not reset.
func TestPiholeRestartAndFlush(t *testing.T) {
	n := piholed(t, "tc123", "")
	t.Cleanup(func() { phRequireFTLHealthy(t) })
	for range 5 {
		phDig(phSplit)
	}

	before, _ := phFTLPID()
	res := phResult[phspec.ServiceResult](t, phRun(t, n, "/actions/restart-dns", "{}", 2*time.Minute))
	after, ok := phFTLPID()
	answers := phAnswers()
	t.Logf("restart-dns: result before %v after %v readyAfterMs %d; PID file %d -> %d (pihole-FTL %v); dig answers %v",
		phDeref(res.FTLPidBefore), phDeref(res.FTLPidAfter), res.ReadyAfterMs, before, after, ok, answers)
	if !ok || after == before || !answers {
		t.Errorf("restart-dns: PID %d -> %d (pihole-FTL %v), answers %v: want a new answering FTL", before, after, ok, answers)
	}
	if res.FTLPidBefore == nil || *res.FTLPidBefore != before || res.FTLPidAfter == nil || *res.FTLPidAfter != after {
		t.Errorf("restart-dns result pids %v -> %v, PID file %d -> %d", phDeref(res.FTLPidBefore), phDeref(res.FTLPidAfter), before, after)
	}

	for range 5 {
		phDig(phSplit)
	}
	db0, log0 := phQueryLogCounts(t)
	res = phResult[phspec.ServiceResult](t, phRun(t, n, "/actions/flush-cache", "{}", 2*time.Minute))
	pid, ok := phFTLPID()
	answers = phAnswers()
	db1, log1 := phQueryLogCounts(t)
	t.Logf("flush-cache: result before %v after %v; PID file %d (pihole-FTL %v); dig answers %v; "+
		"pihole-FTL.db queries %d -> %d, pihole.log lines %d -> %d", phDeref(res.FTLPidBefore), phDeref(res.FTLPidAfter),
		pid, ok, answers, db0, db1, log0, log1)
	if !ok || pid != after || !answers {
		t.Errorf("flush-cache: PID %d -> %d, answers %v: want the same answering FTL", after, pid, answers)
	}
	if res.FTLPidBefore == nil || res.FTLPidAfter == nil || *res.FTLPidBefore != pid || *res.FTLPidAfter != pid {
		t.Errorf("flush-cache result pids %v -> %v, want %d both", phDeref(res.FTLPidBefore), phDeref(res.FTLPidAfter), pid)
	}
	if db1 < db0 || log1 < log0 {
		t.Errorf("flush-cache reset the query log: pihole-FTL.db %d -> %d, pihole.log %d -> %d", db0, db1, log0, log1)
	}
}

// TC-124: the hazard first (pihole's own `disable 20s` child dies with its
// unit), then stop and start, then a Nexus timed stop that re-enables
// within ±2 s of reenableAt with nexusd stopped.
func TestPiholeTimedStopReenables(t *testing.T) {
	t.Cleanup(func() {
		// A timed stop leaves its transient timer until it fires; a failed
		// test must not leave it to re-enable under the next test.
		_, _ = sh(30*time.Second, "systemctl", "stop", "nexus-pihole-reenable.timer")
		if phSetupVar(t, "BLOCKING_ENABLED") != "true" {
			mustSh(t, phPihole, "enable")
		}
	})

	// Control: the re-enable child of `pihole disable 20s` lives in the
	// transient unit's cgroup and dies with it (V-13).
	ctl := phBash(t, "systemd-run --wait "+phPihole+" disable 20s")
	time.Sleep(25 * time.Second)
	got := phSetupVar(t, "BLOCKING_ENABLED")
	t.Logf("control: systemd-run --wait pihole disable 20s; after 25 s BLOCKING_ENABLED=%s\n%s", got, ctl)
	if got != "false" {
		t.Errorf("control: BLOCKING_ENABLED=%s 25 s after `pihole disable 20s` in its own unit, want false (the hazard)", got)
	}
	mustSh(t, phPihole, "enable")
	if got := phSetupVar(t, "BLOCKING_ENABLED"); got != "true" {
		t.Fatalf("pihole enable left BLOCKING_ENABLED=%s", got)
	}
	if a, _, ok := phWaitDig(phBlocked, 10*time.Second, phIsBlocked); !ok {
		t.Fatalf("%s is not blocked with blocking on: %s", phBlocked, a)
	}

	n := piholed(t, "tc124", "")

	// Stop and start, untimed.
	res := phResult[phspec.BlockingResult](t, phRun(t, n, "/actions/stop-blocking", "{}", time.Minute))
	off, offDig := phSetupVar(t, "BLOCKING_ENABLED"), phDig(phBlocked)
	t.Logf("stop-blocking: %+v; BLOCKING_ENABLED=%s; dig %s %s", res.BlockingProperties, off, phBlocked, offDig)
	if res.State != phspec.BlockingDisabled || res.ReenableAt != nil || off != "false" || !phIsUnblocked(offDig) {
		t.Errorf("stop-blocking: %+v, BLOCKING_ENABLED=%s, dig %s", res.BlockingProperties, off, offDig)
	}
	res = phResult[phspec.BlockingResult](t, phRun(t, n, "/actions/start-blocking", "{}", time.Minute))
	on := phSetupVar(t, "BLOCKING_ENABLED")
	onDig, _, blocked := phWaitDig(phBlocked, 5*time.Second, phIsBlocked)
	t.Logf("start-blocking: %+v; BLOCKING_ENABLED=%s; dig %s %s", res.BlockingProperties, on, phBlocked, onDig)
	if res.State != phspec.BlockingEnabled || on != "true" || !blocked {
		t.Errorf("start-blocking: %+v, BLOCKING_ENABLED=%s, dig %s", res.BlockingProperties, on, onDig)
	}

	// Timed stop of 20 s; nexusd stops as soon as the op succeeds.
	opStart := time.Now()
	res = phResult[phspec.BlockingResult](t, phRun(t, n, "/actions/stop-blocking", `{"durationSeconds":20}`, time.Minute))
	opDur := time.Since(opStart)
	lib(t, "stop-nexusd")
	stoppedAt := time.Now()
	if res.ReenableAt == nil || res.State != phspec.BlockingDisabled {
		t.Fatalf("timed stop-blocking: %+v", res.BlockingProperties)
	}
	t.Logf("timed stop-blocking: %+v; nexusd %s at %s", res.BlockingProperties, activeState(t, nexusdUnit),
		stoppedAt.UTC().Format(time.RFC3339Nano))
	if st := activeState(t, nexusdUnit); st != "inactive" && st != "failed" {
		t.Fatalf("nexusd is %s after stop", st)
	}
	timer := showUnit(t, "nexus-pihole-reenable.timer", "ActiveState", "SubState", "Description", "AccuracyUSec")
	t.Logf("nexus-pihole-reenable.timer: %v", timer)
	if d := phDig(phBlocked); !phIsUnblocked(d) || phSetupVar(t, "BLOCKING_ENABLED") != "false" {
		t.Errorf("blocking still on after the timed stop: dig %s", d)
	}
	// The host's own clock, not Nexus's reenableAt, anchors the duration:
	// blocking must still be off 15 s after the op began (a 20 s timer).
	time.Sleep(time.Until(opStart.Add(15 * time.Second)))
	if v := phSetupVar(t, "BLOCKING_ENABLED"); v != "false" {
		t.Errorf("BLOCKING_ENABLED=%s %s after the op began, want false until about 20 s", v, time.Since(opStart).Round(time.Millisecond))
	}
	var flipped time.Time
	waitUntil(t, 60*time.Second, "BLOCKING_ENABLED=true", func() (bool, string) {
		v := phSetupVar(t, "BLOCKING_ENABLED")
		if v == "true" {
			flipped = time.Now()
		}
		return v == "true", v
	})
	skew := flipped.Sub(*res.ReenableAt)
	blockedDig, after, blocked := phWaitDig(phBlocked, 5*time.Second, phIsBlocked)
	t.Logf("re-enabled at %s, reenableAt %s: %+.3fs; nexusd %s; dig %s %s %s after the flip",
		flipped.UTC().Format(time.RFC3339Nano), res.ReenableAt.Format(time.RFC3339Nano), skew.Seconds(),
		activeState(t, nexusdUnit), phBlocked, blockedDig, after.Round(time.Millisecond))
	if skew < -2*time.Second || skew > 2*time.Second {
		t.Errorf("re-enabled %+.3fs from reenableAt, want within ±2 s", skew.Seconds())
	}
	// Anchored to the host: 20 s from the op's start, ±2 s, and the op's
	// own duration, which the timer's start may trail by.
	if since := flipped.Sub(opStart); since < 18*time.Second || since > 22*time.Second+opDur {
		t.Errorf("re-enabled %s after the op began (op took %s), want 20 s ±2 s (+ the op's duration)", since.Round(time.Millisecond),
			opDur.Round(time.Millisecond))
	}
	if st := activeState(t, nexusdUnit); st != "inactive" && st != "failed" {
		t.Errorf("nexusd is %s: the re-enable must not need it", st)
	}
	if !blocked {
		t.Errorf("%s not blocked again after the re-enable: %s", phBlocked, blockedDig)
	}
}

// TC-125: the list.sh hazard on the real image, then deny add and remove,
// the cross-list 409 and two regexes stored byte for byte (the inert proof
// for inertAllowed's /value).
func TestPiholeDomainLists(t *testing.T) {
	const ctl, ctl2 = "ctl.example", "ctl2.example"
	regexes := []string{`(^|\.)rx\.example$`, `^ad[s]?\.;querytype=A`}
	t.Cleanup(func() {
		phSQLWrite(t, "DELETE FROM domainlist WHERE domain IN ('"+ctl+"', '"+ctl2+"', '"+phSplit+"', '"+
			strings.Join(regexes, "', '")+"');")
		mustSh(t, phPihole, "restartdns", "reload-lists")
	})

	// 1. Control: pihole -b moves an allow entry to the deny list.
	phBash(t, phPihole+" -w "+ctl+" && "+phPihole+" -b "+ctl)
	moved := phSQL(t, "SELECT type FROM domainlist WHERE domain = '"+ctl+"'")
	t.Logf("control: pihole -w %s; pihole -b %s -> domainlist.type %s", ctl, ctl, moved)
	if moved != "1" {
		t.Errorf("control: type %q after pihole -w then -b, want 1 (the hazard)", moved)
	}
	// FTL v5.25.2 reloads its lists on a thread after `pihole -w`'s signal;
	// `pihole -b`'s write right behind it can lock that reload out of
	// gravity.db ("database is locked", "Gravity database not available"),
	// after which FTL answers nothing but pi.hole until its next reload
	// (PROVENANCE, WI 5.2 FTL RELOAD RACE). The control's two back-to-back
	// writes are pihole's own, so the host's own reload ends that state.
	time.Sleep(time.Second)
	mustSh(t, phPihole, "restartdns", "reload-lists")
	if locked, _ := phBashCode("grep -c 'database is locked' /var/log/pihole/FTL.log"); strings.TrimSpace(locked) != "0" {
		t.Logf("control: FTL.log has %s 'database is locked' lines", strings.TrimSpace(locked))
	}

	n := piholed(t, "tc125", "")

	// 2. Deny add, then deny remove, of a name the upstream answers.
	upstream := func(a phAnswer) bool { return len(a.addrs) == 1 && a.addrs[0] == phSplitIP }
	if a, _, ok := phWaitDig(phSplit, 10*time.Second, upstream); !ok {
		phFTLDiag(t)
		t.Fatalf("before: dig %s %s, want %s", phSplit, a, phSplitIP)
	}
	body := fmt.Sprintf(`{"list":"deny","kind":"exact","value":%q,"comment":"tc125"}`, phSplit)
	add := phResult[phspec.DomainResult](t, phRun(t, n, "/actions/add-domain", body, time.Minute))
	row := phHostDomains(t)[strconv.FormatInt(add.Domain.Properties.PiholeID, 10)]
	blockedDig, took, _ := phWaitDig(phSplit, 5*time.Second, phIsBlocked)
	t.Logf("deny add %s: created %v %+v; host row %+v; dig %s after %s", phSplit, add.Created, add.Domain.Properties, row,
		blockedDig, took.Round(time.Millisecond))
	if !add.Created || fmt.Sprint(add.Domain.Properties) != fmt.Sprint(row) || row.List != "deny" || !phIsBlocked(blockedDig) {
		t.Errorf("deny add: result %+v, host row %+v, dig %s (want 0.0.0.0)", add.Domain.Properties, row, blockedDig)
	}
	rm := phResult[phspec.DomainRemoveResult](t, phRun(t, n, "/actions/remove-domain",
		fmt.Sprintf(`{"list":"deny","kind":"exact","value":%q}`, phSplit), time.Minute))
	freed, took, _ := phWaitDig(phSplit, 5*time.Second, upstream)
	left := phSQL(t, "SELECT count(*) FROM domainlist WHERE domain = '"+phSplit+"'")
	t.Logf("deny remove: %+v; rows left %s; dig %s after %s", rm.Removed, left, freed, took.Round(time.Millisecond))
	if rm.Removed.PiholeID != row.PiholeID || left != "0" || len(freed.addrs) != 1 || freed.addrs[0] != phSplitIP {
		t.Errorf("deny remove: removed %+v, rows left %s, dig %s (want %s)", rm.Removed, left, freed, phSplitIP)
	}

	// 3. allow/exact, then the same value on deny: 409 before any operation.
	allow := phResult[phspec.DomainResult](t, phRun(t, n, "/actions/add-domain",
		`{"list":"allow","kind":"exact","value":"`+ctl2+`"}`, time.Minute))
	before := phHostDomains(t)[strconv.FormatInt(allow.Domain.Properties.PiholeID, 10)]
	code, b := n.post(phAt+"/actions/add-domain", `{"list":"deny","kind":"exact","value":"`+ctl2+`"}`)
	var p problem.Error
	_ = json.Unmarshal(b, &p)
	after := phHostDomains(t)[strconv.FormatInt(allow.Domain.Properties.PiholeID, 10)]
	t.Logf("deny %s while on allow: %d %s; allow row before %+v after %+v", ctl2, code, b, before, after)
	if code != http.StatusConflict || p.Slug() != problem.Conflict || fmt.Sprint(before) != fmt.Sprint(after) || after.List != "allow" {
		t.Errorf("cross-list add: %d %s, allow row %+v -> %+v; want 409 and the row untouched", code, p.Slug(), before, after)
	}

	// 4. Regexes with `\.`, `[`, `?`, `$`, `|` and ";querytype=A" are stored
	// byte for byte.
	for _, rx := range regexes {
		in, err := json.Marshal(phspec.DomainAddInput{List: "deny", Kind: "regex", Value: rx})
		if err != nil {
			t.Fatal(err)
		}
		res := phResult[phspec.DomainResult](t, phRun(t, n, "/actions/add-domain", string(in), time.Minute))
		stored := phSQLRows(t, fmt.Sprintf("SELECT domain, hex(domain) AS h FROM domainlist WHERE id = %d", res.Domain.Properties.PiholeID))
		t.Logf("regex %q: result value %q; gravity.db %v", rx, res.Domain.Properties.Value, stored)
		if len(stored) != 1 || stored[0]["domain"] != rx || stored[0]["h"] != strings.ToUpper(fmt.Sprintf("%x", rx)) ||
			res.Domain.Properties.Value != rx || res.Domain.Properties.Kind != "regex" {
			t.Errorf("regex %q stored as %v, result %q", rx, stored, res.Domain.Properties.Value)
		}
	}
}

// TC-126: add-adlist with gravity, its log followed live; then the CLI's
// gravity update streams in order and exits 0.
func TestPiholeAdlistAndGravity(t *testing.T) {
	list := phDomains("tc126", 10)
	addr := phFixture(t, "tc126.txt", list)
	t.Cleanup(func() { phDropAdlist(t, addr) })
	n := piholed(t, "tc126", "")

	lists0 := phSQL(t, "SELECT count(*) FROM adlist")
	count0, _ := strconv.Atoi(phInfo(t, "gravity_count"))
	updated0, _ := strconv.Atoi(phInfo(t, "updated"))
	op := phStart(t, n, "/actions/add-adlist", `{"url":"`+addr+`","comment":"tc126","updateGravity":true}`)
	var lines int
	logs := phFollow(t, n, op.ID, func(time.Time, string) { lines++ })
	op = n.waitOp(op.ID, 10*time.Minute)
	res := phResult[phspec.AdlistResult](t, op)
	lists1 := phSQL(t, "SELECT count(*) FROM adlist")
	count1, _ := strconv.Atoi(phInfo(t, "gravity_count"))
	updated1, _ := strconv.Atoi(phInfo(t, "updated"))
	row := phHostAdlists(t)[strconv.FormatInt(res.Adlist.Properties.PiholeID, 10)]
	t.Logf("add-adlist %s with gravity: %d log frames followed, %s; adlist rows %s -> %s; gravity_count %d -> %d; "+
		"info.updated %d -> %d; result %+v gravity %+v; host row %+v", addr, lines, phTook(op), lists0, lists1, count0,
		count1, updated0, updated1, res.Adlist.Properties, res.Gravity, row)
	if l0, _ := strconv.Atoi(lists0); fmt.Sprint(l0+1) != lists1 {
		t.Errorf("adlist rows %s -> %s, want +1", lists0, lists1)
	}
	if count1 != count0+len(list) || updated1 <= updated0 {
		t.Errorf("gravity_count %d -> %d (want +%d), info.updated %d -> %d", count0, count1, len(list), updated0, updated1)
	}
	if res.Gravity == nil || res.Gravity.ExitCode != 0 || row.URL != addr || res.Adlist.Properties.URL != addr ||
		row.Domains == nil || *row.Domains != int64(len(list)) {
		t.Errorf("result %+v gravity %+v, host row %+v", res.Adlist.Properties, res.Gravity, row)
	}
	if !strings.Contains(logs, "Number of gravity domains") || !strings.Contains(logs, addr) {
		t.Errorf("followed log lacks pihole -g's lines:\n%s", logs)
	}
	if a := phDig(list[0]); !phIsBlocked(a) {
		t.Errorf("%s from the new list: dig %s, want blocked", list[0], a)
	}

	// The CLI, as a real process: --follow streams the gravity output to
	// stderr while it runs, in the operation log's order.
	home := phCLIHome(t, n)
	time.Sleep(1100 * time.Millisecond) // info.updated has a resolution of one second
	cmd := exec.Command(os.Args[0], "piholegravity", "update", "--follow", "--yes", "-o", "jsonl")
	cmd.Env = pipelineEnv(home)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	type line struct {
		at   time.Time
		text string
	}
	var got []line
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stderr)
	for sc.Scan() {
		got = append(got, line{time.Now(), sc.Text()})
	}
	err = cmd.Wait()
	exited := time.Now()
	var cliOp apitypes.Operation
	if jerr := json.Unmarshal(stdout.Bytes(), &cliOp); jerr != nil {
		t.Fatalf("nexus piholegravity update: %v, stdout %s", jerr, stdout.String())
	}
	t.Logf("nexus piholegravity update --follow: %v; operation %s %s ended %v; %d stderr lines, first at %v, exit at %v",
		err, cliOp.ID, cliOp.Status, cliOp.Ended, len(got), func() any {
			if len(got) == 0 {
				return nil
			}
			return got[0].at.Format(time.RFC3339Nano)
		}(), exited.Format(time.RFC3339Nano))
	if err != nil || cliOp.Status != apitypes.StatusSucceeded {
		t.Fatalf("nexus piholegravity update: %v, %s %+v", err, cliOp.Status, cliOp.Error)
	}
	opLog := n.opLogs(cliOp.ID)
	var frames strings.Builder
	sc = bufio.NewScanner(strings.NewReader(opLog))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err == nil {
			frames.WriteString(f.D)
		}
	}
	rest := frames.String()
	inOrder := 0
	for _, l := range got {
		// With -o jsonl each followed frame is a JSON object on stderr.
		var f struct {
			D string `json:"d"`
		}
		if json.Unmarshal([]byte(l.text), &f) != nil {
			continue
		}
		text := strings.TrimSpace(f.D)
		if text == "" {
			continue
		}
		i := strings.Index(rest, text)
		if i < 0 {
			continue
		}
		rest = rest[i+len(text):]
		inOrder++
	}
	if len(got) == 0 || cliOp.Ended == nil || !got[0].at.Before(*cliOp.Ended) {
		t.Errorf("no gravity line reached stderr before the operation ended (%d lines)", len(got))
	}
	if inOrder < 5 || !strings.Contains(frames.String(), "Number of gravity domains") {
		t.Errorf("only %d stderr lines found in the operation log's order:\n%v", inOrder, got)
	}
	if updated2, _ := strconv.Atoi(phInfo(t, "updated")); updated2 <= updated1 {
		t.Errorf("info.updated %d -> %d after the CLI's gravity update", updated1, updated2)
	}
	t.Logf("%d of %d stderr lines matched the operation log in order", inOrder, len(got))
}

// TC-150: the WI 5.4 real-device evidence. The hazard first: pihole's own
// `adlist del` fails on a list gravity processed; then remove-adlist
// removes such a list in one transaction, and a 100,000-domain list is
// removed while FTL keeps answering.
func TestPiholeAdlistRemoveAfterGravity(t *testing.T) {
	small := phFixture(t, "rm-small.txt", []string{"rm-target.example", "rm-small-2.example", "rm-small-3.example"})
	ctl := phFixture(t, "rm-ctl.txt", []string{"rm-ctl-1.example", "rm-ctl-2.example", "rm-ctl-3.example"})
	big := phFixture(t, "rm-big.txt", phDomains("rm-big", 100000))
	t.Cleanup(func() {
		for _, a := range []string{small, ctl, big} {
			phDropAdlist(t, a) // each waits for its reload to end
		}
		phRequireFTLHealthy(t)
	})
	n := piholed(t, "tc150", "")
	for _, a := range []string{small, ctl, big} {
		phResult[phspec.AdlistResult](t, phRun(t, n, "/actions/add-adlist", `{"url":"`+a+`"}`, time.Minute))
	}
	g := phResult[phspec.GravityResult](t, phRun(t, n, "/actions/update-gravity", "{}", 10*time.Minute))
	ids := map[string]string{}
	for _, a := range []string{small, ctl, big} {
		ids[a] = phSQL(t, "SELECT id FROM adlist WHERE address = '"+a+"'")
		t.Logf("adlist %s: id %s, %s gravity rows", a, ids[a], phSQL(t, "SELECT count(*) FROM gravity WHERE adlist_id = "+ids[a]))
	}
	t.Logf("gravity: %+v", g)

	// 1. Control: pihole -a adlist del on a processed list.
	out, code := phBashCode(phPihole + " -a adlist del " + ctl)
	rows := phSQL(t, "SELECT count(*) FROM adlist WHERE address = '"+ctl+"'")
	t.Logf("control: pihole -a adlist del %s: exit %d, adlist rows left %s\n%s", ctl, code, rows, out)
	if !strings.Contains(out, "FOREIGN KEY constraint failed") || code != 0 || rows != "1" {
		t.Errorf("control: exit %d, rows %s, output %q; want the FOREIGN KEY error, exit 0 and the row kept (the hazard)",
			code, rows, out)
	}

	// 2. remove-adlist of the small list: its domain stops being blocked
	// at once, with no gravity run.
	before := phDig("rm-target.example")
	updated0 := phInfo(t, "updated")
	op := phRun(t, n, "/actions/remove-adlist", `{"id":`+ids[small]+`}`, 2*time.Minute)
	res := phResult[phspec.AdlistRemoveResult](t, op)
	afterDig := phDig("rm-target.example")
	t.Logf("remove-adlist %s (id %s): %+v; dig rm-target.example %s before, %s right after; info.updated %s -> %s",
		small, ids[small], res.Removed, before, afterDig, updated0, phInfo(t, "updated"))
	if !phIsBlocked(before) || phIsBlocked(afterDig) || afterDig.status == "" {
		t.Errorf("rm-target.example: %s before, %s after; want blocked, then not blocked", before, afterDig)
	}
	if res.Removed.URL != small || res.Gravity != nil || phInfo(t, "updated") != updated0 {
		t.Errorf("remove-adlist result %+v gravity %+v, info.updated %s -> %s (want no gravity run)", res.Removed,
			res.Gravity, updated0, phInfo(t, "updated"))
	}

	// 3. Independent read: no rows anywhere for that id, no FK violation.
	counts := phSQL(t, "SELECT (SELECT count(*) FROM adlist WHERE id = "+ids[small]+") || ' ' || "+
		"(SELECT count(*) FROM adlist_by_group WHERE adlist_id = "+ids[small]+") || ' ' || "+
		"(SELECT count(*) FROM gravity WHERE adlist_id = "+ids[small]+")")
	fk := phSQL(t, "PRAGMA foreign_key_check")
	t.Logf("adlist / adlist_by_group / gravity rows for id %s: %s; PRAGMA foreign_key_check: %q", ids[small], counts, fk)
	if counts != "0 0 0" || fk != "" {
		t.Errorf("after remove-adlist: counts %s, foreign_key_check %q; want 0 0 0 and empty", counts, fk)
	}

	// 4. The 100,000-domain list, removed while dig pi.hole runs every
	// 200 ms (A-59).
	var digs, failed atomic.Int64
	var failures sync.Map
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				digs.Add(1)
				if a := phDig("pi.hole"); a.status != "NOERROR" || len(a.addrs) == 0 {
					failed.Add(1)
					failures.Store(time.Now().Format(time.RFC3339Nano), a.String())
				}
			}
		}
	})
	start := time.Now()
	bigOp := phRun(t, n, "/actions/remove-adlist", `{"id":`+ids[big]+`}`, 5*time.Minute)
	wall := time.Since(start)
	close(stop)
	wg.Wait()
	phResult[phspec.AdlistRemoveResult](t, bigOp)
	t.Logf("TC-150 remove-adlist of the 100,000-domain list: op %s (started to ended), %s POST to terminal; "+
		"dig pi.hole loop: %d queries, %d failed; gravity rows left for id %s: %s", phTook(bigOp), wall.Round(time.Millisecond),
		digs.Load(), failed.Load(), ids[big], phSQL(t, "SELECT count(*) FROM gravity WHERE adlist_id = "+ids[big]))
	failures.Range(func(k, v any) bool { t.Logf("dig failure at %s: %s", k, v); return true })
	if phTook(bigOp) >= 10*time.Second || failed.Load() != 0 || digs.Load() == 0 {
		t.Errorf("A-59: removal took %s with %d of %d digs failed; want under 10 s and none", phTook(bigOp), failed.Load(), digs.Load())
	}

	// 5. Nexus never ran `pihole -a adlist del`.
	for _, id := range []string{op.ID, bigOp.ID} {
		if l := n.opLogs(id); strings.Contains(l, "adlist del") {
			t.Errorf("operation %s ran adlist del:\n%s", id, l)
		}
	}
	t.Logf("op log of the small removal:\n%s", n.opLogs(op.ID))
}

// TC-151: the WI 5.5 real-device evidence. The hazard first: a sixth bare
// `pihole restartdns` within a minute leaves FTL stopped; then seven Nexus
// restarts back to back all succeed.
func TestPiholeStartLimit(t *testing.T) {
	native := phInit(t) == "native"
	t.Cleanup(func() {
		if !phAnswers() {
			phStartFTL(t)
		}
		phRequireFTLHealthy(t)
	})
	limits := showUnit(t, phFTLUnit, "StartLimitBurst", "StartLimitIntervalUSec", "Restart", "RestartUSec")
	t.Logf("INIT=%s; systemctl show pihole-FTL: %v", phInit(t), limits)
	if native && (limits["StartLimitBurst"] != "5" || limits["StartLimitIntervalUSec"] != "1min") {
		t.Errorf("start limit %v, want StartLimitBurst=5 StartLimitIntervalUSec=1min (A-60)", limits)
	}

	// Control: six bare restarts in a row.
	phResetFailed(t)
	since := time.Now().Add(-time.Second)
	var codes []int
	var last string
	for i := 1; i <= 6; i++ {
		out, code := phBashCode(phPihole + " restartdns")
		codes = append(codes, code)
		last = out
		t.Logf("control restart %d: exit %d %q", i, code, strings.TrimSpace(out))
	}
	result := showUnit(t, phFTLUnit, "Result", "ActiveState")
	_, _, answered := phWaitDig("pi.hole", 5*time.Second, func(a phAnswer) bool { return a.status == "NOERROR" })
	journal, _ := sh(30*time.Second, "journalctl", "-u", phFTLUnit, "--since", "@"+strconv.FormatInt(since.Unix(), 10), "--no-pager")
	repeated := strings.Contains(journal, "Start request repeated too quickly")
	t.Logf("control: exits %v; sixth output %q; %v; FTL answered within 5 s: %v; journal has "+
		"\"Start request repeated too quickly\": %v; \"attempted too often\" in the sixth's output: %v",
		codes, strings.TrimSpace(last), result, answered, repeated, strings.Contains(last, "attempted too often"))
	if native {
		if fmt.Sprint(codes[:5]) != "[0 0 0 0 0]" || codes[5] == 0 || result["Result"] != "start-limit-hit" || answered || !repeated {
			t.Errorf("control: exits %v, %v, answered %v, journal start-limit line %v; want restarts 1-5 exit 0, the sixth "+
				"non-zero with Result=start-limit-hit and FTL not answering (the hazard)", codes, result, answered, repeated)
		}
	}

	// Recover.
	pid := phStartFTL(t)
	t.Logf("recovered: reset-failed and systemctl start; FTL pid %d answers", pid)

	// Seven Nexus restarts through the CLI, back to back.
	n := piholed(t, "tc151", "")
	home := phCLIHome(t, n)
	for i := 1; i <= 7; i++ {
		before, _ := phFTLPID()
		r := runNexus(home, "piholeresolver", "restart", "--yes", "-o", "jsonl")
		var op apitypes.Operation
		jerr := json.Unmarshal([]byte(r.stdout), &op)
		var res phspec.ServiceResult
		if jerr == nil {
			jerr = json.Unmarshal(op.Result, &res)
		}
		after, ok := phFTLPID()
		answers := phAnswers()
		res2 := showUnit(t, phFTLUnit, "Result")["Result"]
		t.Logf("nexus piholeresolver restart %d: exit %d, %s, ftlPidAfter %v; PID file %d -> %d; answers %v; Result=%s",
			i, r.code, op.Status, phDeref(res.FTLPidAfter), before, after, answers, res2)
		if r.code != 0 || jerr != nil || op.Status != apitypes.StatusSucceeded || !ok || after == before ||
			res.FTLPidAfter == nil || *res.FTLPidAfter != after || !answers || res2 != "success" {
			msg := fmt.Sprintf("Nexus restart %d: exit %d (%v) %s %+v; PID %d -> %d; answers %v; Result=%s\nstderr: %s",
				i, r.code, jerr, op.Status, op.Error, before, after, answers, res2, r.stderr)
			if native {
				t.Error(msg)
			} else {
				t.Log("INIT=sysv, recorded only: " + msg)
			}
		}
	}
}
