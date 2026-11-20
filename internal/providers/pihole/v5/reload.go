// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package v5

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// The FTL list-reload barrier (WI 5.6, A-64 to A-69). `pihole restartdns
// reload-lists` returns once it has sent SIGRTMIN (V-17); FTL v5.25.2 then
// re-reads gravity.db on its database thread, and a gravity.db commit during
// that re-read fails it: FTL answers pi.hole only, local records included,
// until a later reload succeeds (test/harness/pihole/PROVENANCE "FTL RELOAD
// RACE", "FTL LIST RELOAD SIGNALS"). So every gravity.db write first waits
// for a reload in flight to end (settleLists), and every reload Nexus sends
// is followed until FTL has reloaded (reloadListsAndWait).
//
// The bounds (A-67): poll the log every ListReloadPoll; a reload has ended
// once its log has been quiet for ListReloadQuiet; give up after
// ListReloadWait; reload again ListReloadRecoveries times; probe with up to
// ListReloadProbeTries queries of ListReloadProbeTimeout each.
const (
	ListReloadPoll         = 100 * time.Millisecond
	ListReloadQuiet        = 500 * time.Millisecond
	ListReloadWait         = 30 * time.Second
	ListReloadRecoveries   = 1
	ListReloadProbeTimeout = time.Second
	ListReloadProbeTries   = 3
	// listReloadTail is how much of the log's end the pre-write check reads.
	listReloadTail = 256 << 10
)

// ReloadTiming overrides the barrier's wall-clock bounds. Production leaves
// it zero, which means the ListReload constants above (A-67); tests set
// shorter values through Mutator.Reload so the barrier's real waits stay
// short. A zero field means its constant.
type ReloadTiming struct {
	Poll         time.Duration // ListReloadPoll
	Quiet        time.Duration // ListReloadQuiet
	Wait         time.Duration // ListReloadWait
	ProbeTimeout time.Duration // ListReloadProbeTimeout
}

// reloadTiming is m's barrier bounds, the constants where unset.
func (m *Mutator) reloadTiming() ReloadTiming {
	t := m.Reload
	if t.Poll <= 0 {
		t.Poll = ListReloadPoll
	}
	if t.Quiet <= 0 {
		t.Quiet = ListReloadQuiet
	}
	if t.Wait <= 0 {
		t.Wait = ListReloadWait
	}
	if t.ProbeTimeout <= 0 {
		t.ProbeTimeout = ListReloadProbeTimeout
	}
	return t
}

// FTL's log lines that mark a list reload, from TC-153's capture of FTL
// v5.25.2 and its source at the tag (PROVENANCE "FTL LIST RELOAD SIGNALS").
// Only these fixed patterns are ever matched; no log text reaches a result,
// a problem detail or the operation log.
var (
	// A start: "Received: Real-time signal 0 (34 -> 0)" when FTL receives
	// SIGRTMIN (src/signals.c L279; FTL reloads on real-time signal 0
	// only), or "Reloading DNS cache" on SIGHUP and at start
	// (src/dnsmasq_interface.c L1788).
	logStartRTMIN = regexp.MustCompile(`^Received: .* \([0-9]+ -> 0\)$`)
	// The completion: the line after the regex compilation, a reload's last
	// logged step before its quiet gravity.db reads (src/regex.c L744,
	// src/datastructure.c L511-514). A failed reload logs it too.
	logCompiled = regexp.MustCompile(`^Compiled [0-9]+ whitelist and [0-9]+ blacklist regex filters for [0-9]+ clients in [0-9.]+ msec$`)
	// A new FTL process (src/main.c L55): the reloads of the one before it
	// are over, whatever its log says.
	logFTLStarted = regexp.MustCompile(`^########## FTL started on .*! ##########$`)
)

const (
	logStartHUP = "Reloading DNS cache"
	// The failure markers (src/database/gravity-db.c L203, L1107, L1540).
	logLocked      = "database is locked"
	logNoGravityDB = "Gravity database not available"
)

// FTL registers one SQLite log callback for the whole process (src/database/
// common.c, sqlite3_config(SQLITE_CONFIG_LOG)), so a busy pihole-FTL.db logs
// `SQLite3 message: database is locked in "<sql>" (5)` as well, and only the
// statement tells the two databases apart. A failure line is one of
// gravity.db's own messages (gravityDB_*, check_inaccessible_adlists), or
// that callback's line whose SQL names a gravity.db view or table; a line
// about pihole-FTL.db is not (code review CR-7).
var (
	logGravityOwn = regexp.MustCompile(`^(gravityDB_|check_inaccessible_adlists)`)
	logSQLiteLock = regexp.MustCompile(`^SQLite3 message: database is locked in "(.*)" \([0-9]+\)$`)
	logGravitySQL = regexp.MustCompile(`\b(vw_[a-z_]+|domainlist|adlist|gravity|domain_audit)\b`)
)

// isGravityFailure reports whether msg is a failed gravity.db access.
func isGravityFailure(msg string) bool {
	if strings.Contains(msg, logNoGravityDB) {
		return true
	}
	if logGravityOwn.MatchString(msg) {
		return strings.Contains(msg, logLocked)
	}
	if m := logSQLiteLock.FindStringSubmatch(msg); m != nil {
		return logGravitySQL.MatchString(m[1])
	}
	return false
}

type logMarker int

const (
	markerNone logMarker = iota
	markerProcess
	markerStart
	markerEnd
	markerFailed
)

// classifyLogLine matches one FTL log line, "[<time> <pid><thread>] <msg>"
// (src/log.c), against the markers.
func classifyLogLine(line string) logMarker {
	if !strings.HasPrefix(line, "[") {
		return markerNone
	}
	_, msg, ok := strings.Cut(line, "] ")
	if !ok {
		return markerNone
	}
	msg = strings.TrimRight(msg, "\r")
	switch {
	case logFTLStarted.MatchString(msg):
		return markerProcess
	case msg == logStartHUP || logStartRTMIN.MatchString(msg):
		return markerStart
	case logCompiled.MatchString(msg):
		return markerEnd
	case isGravityFailure(msg):
		return markerFailed
	}
	return markerNone
}

// reloadRecord follows FTL's list reloads through its log. A record starts
// at a start line and ends at the completion line after it; the log's state
// is that of its last record. FTL reloads on one database thread, one
// reload at a time, and a reload request is an event flag (src/signals.c
// L290, src/database/database-thread.c L110): a second start line while a
// reload runs either asks for one more reload after it or merges with it,
// and the log cannot tell which. Such a record counts its starts, so the
// watch can wait long enough for a reload that may follow.
type reloadRecord struct {
	running   bool // The last start line has no completion line after it
	starts    int  // Start lines in the last record
	failed    bool // The last ended record has a failure line
	failSince bool // A failure line since the last record began or ended
}

func (r *reloadRecord) feed(m logMarker) {
	switch m {
	case markerProcess:
		r.running, r.starts, r.failed, r.failSince = false, 0, false, false
	case markerStart:
		if !r.running {
			r.running, r.starts, r.failed, r.failSince = true, 0, false, false
		}
		r.starts++
	case markerEnd:
		r.running, r.failed, r.failSince = false, r.failSince, false
	case markerFailed:
		r.failSince = true
		if !r.running {
			// FTL logs some failures after the completion line
			// (check_inaccessible_adlists, src/database/gravity-db.c L1540).
			r.failed = true
		}
	}
}

// failedNow reports a failed last record, or a running one that has
// logged a failure.
func (r *reloadRecord) failedNow() bool { return r.failed || r.running && r.failSince }

// logMark is where FTL's log was read up to: its inode and the offset
// after the last whole line read.
type logMark struct {
	ino  uint64
	size int64
}

// ftlLogWatch reads FTL's log as it grows. Its clock is the larger of the
// Mutator's clock since the watch began and the sum of its own sleeps, so
// its bounds hold under a fake clock that only m.sleep advances, and under
// one that stands still.
type ftlLogWatch struct {
	m       *Mutator
	tm      ReloadTiming
	path    string
	mark    logMark
	rec     reloadRecord
	started bool          // A start line was read after the watch began
	last    time.Duration // When the last marker line was read
	began   time.Duration // When the last record's first start was read; -1 if before the watch
	quiet   time.Duration // How long the log must be quiet; ListReloadQuiet or more
	t0      time.Time
	slept   time.Duration
	// lastErr is the log's open, stat or read error at the latest read, nil
	// once a read succeeds: why the watch may see nothing.
	lastErr error
}

// watchFTLLog reads the last listReloadTail bytes of FTL's log, whole lines
// only, to learn its state. The log's open, stat or read error is the
// watch's lastErr; the watch is usable either way, and reads the log from
// its start once it appears.
func (m *Mutator) watchFTLLog() *ftlLogWatch {
	tm := m.reloadTiming()
	w := &ftlLogWatch{m: m, tm: tm, path: m.Paths.FTLLogPath(), last: -tm.Quiet, began: -1, quiet: tm.Quiet,
		t0: m.now()}
	w.lastErr = w.readTail()
	return w
}

// readTail is watchFTLLog's read of the log's end.
func (w *ftlLogWatch) readTail() error {
	f, err := os.Open(w.path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	from := max(fi.Size()-listReloadTail, 0)
	b := make([]byte, fi.Size()-from)
	if _, err := f.ReadAt(b, from); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	skip := 0
	if from > 0 {
		// The first line is cut.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			skip = i + 1
		} else {
			skip = len(b)
		}
	}
	n := w.feed(b[skip:], false)
	w.mark = logMark{ino: inode(fi), size: from + int64(skip+n)}
	return nil
}

// poll reads the whole lines added since the mark; a log that cannot be
// read for now is read again at the next poll, and its error is lastErr.
func (w *ftlLogWatch) poll() { w.lastErr = w.read() }

// read is poll's read. A new inode or a size below the mark is a rotation
// (logrotate's create or copytruncate): the new file is read from its
// start, and every line in it is newer than the mark.
func (w *ftlLogWatch) read() error {
	fi, err := os.Stat(w.path)
	if err != nil {
		return err
	}
	ino, from := inode(fi), w.mark.size
	if ino != w.mark.ino || fi.Size() < w.mark.size {
		from = 0
		if !w.started {
			// The start line may have gone with the old file: a reload
			// is presumed to run until a completion line is read.
			w.started, w.began = true, w.elapsed()
			w.rec.feed(markerProcess)
			w.rec.feed(markerStart)
		}
	}
	if fi.Size() <= from {
		w.mark = logMark{ino: ino, size: from}
		return nil
	}
	f, err := os.Open(w.path)
	if err != nil {
		return err
	}
	defer f.Close()
	b := make([]byte, fi.Size()-from)
	k, err := f.ReadAt(b, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	n := w.feed(b[:k], true)
	w.mark = logMark{ino: ino, size: from + int64(n)}
	return nil
}

// feed classifies the whole lines of b and returns how many bytes they
// took. Lines read live (after the watch began) move the quiet clock.
func (w *ftlLogWatch) feed(b []byte, live bool) int {
	n := 0
	for {
		i := bytes.IndexByte(b[n:], '\n')
		if i < 0 {
			return n
		}
		m := classifyLogLine(string(b[n : n+i]))
		n += i + 1
		if m == markerNone {
			continue
		}
		newRecord := m == markerStart && !w.rec.running
		w.rec.feed(m)
		if !live {
			continue
		}
		now := w.elapsed()
		w.last = now
		switch {
		case newRecord:
			w.started, w.began, w.quiet = true, now, w.tm.Quiet
		case m == markerStart:
			w.started = true
		case m == markerEnd && w.rec.starts > 1:
			// A reload may follow this one: wait as long as this one
			// took, plus the database thread's 100 ms sleeps, for its
			// completion line.
			if w.began >= 0 {
				w.quiet = max(w.tm.Quiet, now-w.began+2*w.tm.Poll)
			}
		}
	}
}

func (w *ftlLogWatch) elapsed() time.Duration {
	return max(w.m.now().Sub(w.t0), w.slept)
}

// settled reports that no reload runs, or the running one has logged a
// failure, and no marker line has been read for ListReloadQuiet (or longer
// when a reload may follow): the quiet covers the gravity.db reads FTL
// makes after its completion line without logging (src/datastructure.c
// L514, check_inaccessible_adlists).
func (w *ftlLogWatch) settled() bool {
	return (!w.rec.running || w.rec.failSince) && w.elapsed()-w.last >= w.quiet
}

func (w *ftlLogWatch) sleep(ctx context.Context) error {
	if err := w.m.sleep(ctx, w.tm.Poll); err != nil {
		return err
	}
	w.slept += w.tm.Poll
	return nil
}

// inode is fi's inode number, 0 where the platform has none.
func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// ftlLogHostPath is FTL's log as a host path, for messages.
func (m *Mutator) ftlLogHostPath() string {
	if m.Paths.FTLLog != "" {
		return m.Paths.FTLLog
	}
	return ReadFTLConf(m.Paths).LogFile
}

// probeFTL asks FTL for nexus-probe-<16 random hex>.invalid, up to
// ListReloadProbeTries times (A-66). Any reply passes, whatever its rcode:
// a healthy FTL answers it NXDOMAIN from its own local-only domain, never
// forwarding it, and a stuck one answers nothing but pi.hole, which is why
// dnscheck.WaitReady cannot be the probe.
func (m *Mutator) probeFTL(ctx context.Context) (bool, error) {
	for range ListReloadProbeTries {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return false, fmt.Errorf("pihole: probe name: %w", err)
		}
		_, err := dnscheck.Query(ctx, m.DNSServer, "nexus-probe-"+hex.EncodeToString(b[:])+".invalid", m.reloadTiming().ProbeTimeout)
		if err == nil || errors.Is(err, dnscheck.ErrTruncated) {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, fmt.Errorf("pihole: probing FTL: %w", ctx.Err())
		}
	}
	return false, nil
}

type reloadOutcome int

const (
	reloadDone reloadOutcome = iota
	reloadFailed
	reloadTimedOut
	reloadFTLDown
	reloadLogUnreadable // The wait timed out and FTL's log could not be read
)

// reloadListsAndWait runs `pihole restartdns reload-lists` and returns only
// once FTL has reloaded its lists: a record that started after the command
// has ended, the log has been quiet for ListReloadQuiet, and the probe was
// answered. A failed record or an unanswered probe reloads again,
// ListReloadRecoveries times; still failing, or with no record ended within
// ListReloadWait, it is pihole-command-failed{command: "pihole restartdns
// reload-lists", exitCode: 0, rolledBack: false}, whose detail starts with
// done, what was already changed: the committed write is not undone
// (A-68). When FTL is not running, the command is a no-op (V-17) and the
// lists load when FTL starts. A non-zero exit is pihole-command-failed as
// before.
func (m *Mutator) reloadListsAndWait(ctx context.Context, out io.Writer, done string) error {
	for attempt := 0; ; attempt++ {
		res, err := m.reloadListsOnce(ctx, out, done)
		switch {
		case err != nil:
			return err
		case res == reloadDone || res == reloadFTLDown:
			return nil
		case res == reloadLogUnreadable:
			return listReloadUnverified(done, m.ftlLogHostPath())
		case res == reloadTimedOut:
			return listReloadFailed(done, m.reloadTiming().Wait)
		case attempt >= ListReloadRecoveries:
			return listReloadFailed(done, 0)
		}
		fmt.Fprintln(out, "pihole: FTL's list reload failed (gravity.db was busy); reloading again")
	}
}

func (m *Mutator) reloadListsOnce(ctx context.Context, out io.Writer, done string) (reloadOutcome, error) {
	if _, ok := m.FTLPID(); !ok {
		if err := m.runReloadLists(ctx, out, done); err != nil {
			return 0, err
		}
		fmt.Fprintln(out, "pihole: FTL is not running; its lists load when it starts")
		return reloadFTLDown, nil
	}
	// A log that cannot be read yet is read once it can; the bound ends
	// the wait otherwise, and then w.lastErr says why.
	w := m.watchFTLLog()
	if err := m.runReloadLists(ctx, out, done); err != nil {
		return 0, err
	}
	for {
		if _, ok := m.FTLPID(); !ok {
			fmt.Fprintln(out, "pihole: FTL is not running; its lists load when it starts")
			return reloadFTLDown, nil
		}
		w.poll()
		if w.started && w.settled() {
			break
		}
		if w.elapsed() >= w.tm.Wait {
			if w.lastErr != nil {
				fmt.Fprintf(out, "pihole: FTL's log %s could not be read: %v\n", m.ftlLogHostPath(), w.lastErr)
				return reloadLogUnreadable, nil
			}
			return reloadTimedOut, nil
		}
		if err := w.sleep(ctx); err != nil {
			return 0, fmt.Errorf("pihole: waiting for FTL to reload its lists: %w", err)
		}
	}
	if w.rec.failedNow() {
		return reloadFailed, nil
	}
	answered, err := m.probeFTL(ctx)
	if err != nil {
		return 0, err
	}
	if !answered {
		return reloadFailed, nil
	}
	fmt.Fprintf(out, "pihole: FTL reloaded its lists in %d ms\n", w.last.Milliseconds())
	return reloadDone, nil
}

// runReloadLists runs `pihole restartdns reload-lists`. A failure is
// pihole-command-failed whose detail adds done, what the committed write
// already changed.
func (m *Mutator) runReloadLists(ctx context.Context, out io.Writer, done string) error {
	fmt.Fprintln(out, "pihole: running pihole restartdns reload-lists")
	err := m.runPihole(ctx, out, "restartdns", "reload-lists")
	var pe *problem.Error
	if errors.As(err, &pe) {
		pe.Detail += "; " + done + ", but lists may not be reloaded"
	}
	return err
}

// listReloadFailed is the barrier's failure: FTL did not reload its lists,
// or not within wait (zero: it failed rather than timed out). Nothing is
// rolled back.
func listReloadFailed(done string, wait time.Duration) error {
	within := ""
	if wait > 0 {
		within = fmt.Sprintf(" within %d s", int(wait/time.Second))
	}
	const command = "pihole restartdns reload-lists"
	return problem.New(problem.PiholeCommandFailed, done+", but FTL could not reload its lists"+within+
		"; until a reload succeeds, FTL may answer only pi.hole: run `"+command+"`").
		With("command", command).With("exitCode", 0).With("rolledBack", false)
}

// listReloadUnverified is the barrier's failure when FTL's log could not be
// read at its last read (the wait ended on the latest read's error, not
// necessarily on one that failed throughout): Nexus cannot tell whether FTL
// reloaded its lists.
func listReloadUnverified(done, logPath string) error {
	const command = "pihole restartdns reload-lists"
	return problem.New(problem.PiholeCommandFailed, done+", but FTL's log "+logPath+" could not be read, so Nexus cannot "+
		"tell whether FTL reloaded its lists; if FTL answers only pi.hole, run `"+command+"`").
		With("command", command).With("exitCode", 0).With("rolledBack", false)
}

// settleLists is the check before every gravity.db write, and before every
// command that writes it, after WaitExternalGravity. With FTL running it
// reads FTL's log: a reload in progress is waited for, until its record
// ends and the log has been quiet for ListReloadQuiet, and for no longer
// than ListReloadWait (pihole-busy{waitedSeconds}); a failed last record,
// or an unanswered probe, means FTL is stuck from an earlier reload,
// whoever caused it, and its lists are reloaded before the write. Any
// failure fails the operation with nothing changed. A log FTL's running
// process does not let Nexus read is dependency-unavailable: Nexus cannot
// tell whether FTL is reloading. A reload started outside Nexus after the
// check can still race the write (A-69).
func (m *Mutator) settleLists(ctx context.Context, out io.Writer) error {
	if _, ok := m.FTLPID(); !ok {
		return nil // No reader to race.
	}
	w := m.watchFTLLog()
	if w.lastErr != nil {
		return unreadableLog(m.ftlLogHostPath())
	}
	if w.rec.running {
		fmt.Fprintln(out, "pihole: waiting for FTL to finish reloading its lists")
		for {
			if _, ok := m.FTLPID(); !ok {
				return nil
			}
			w.poll()
			if w.settled() {
				break
			}
			if waited := w.elapsed(); waited >= w.tm.Wait {
				if w.lastErr != nil {
					fmt.Fprintf(out, "pihole: FTL's log %s could not be read: %v\n", m.ftlLogHostPath(), w.lastErr)
					return unreadableLog(m.ftlLogHostPath())
				}
				return problem.New(problem.PiholeBusy, fmt.Sprintf("FTL did not finish reloading its lists within %d s; "+
					"nothing was changed", int(w.tm.Wait/time.Second))).With("waitedSeconds", int(waited/time.Second))
			}
			if err := w.sleep(ctx); err != nil {
				return fmt.Errorf("pihole: waiting for FTL to finish reloading its lists: %w", err)
			}
		}
	}
	if !w.rec.failedNow() {
		answered, err := m.probeFTL(ctx)
		if err != nil {
			return err
		}
		if answered {
			return nil
		}
	}
	return m.reloadListsAndWait(ctx, out, "nothing was changed")
}

// unreadableLog is dependency-unavailable for an FTL log Nexus cannot read
// while FTL runs, so it cannot tell whether FTL is reloading its lists.
func unreadableLog(path string) error {
	return problem.New(problem.DependencyUnavailable, fmt.Sprintf("FTL's log %s is not readable, so Nexus cannot tell "+
		"whether FTL is reloading its lists; nothing was changed", path)).With("provider", spec.Name)
}
