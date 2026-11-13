package v5test

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

// Fault is an injectable FTL misbehavior (SetFault). Several can be set at
// once.
type Fault int

const (
	// FaultNone is FTL as verified.
	FaultNone Fault = iota
	// FaultRestartFails makes `pihole restartdns` exit 1 with FTL left
	// stopped, as a service restart whose new FTL did not start.
	FaultRestartFails
	// FaultNeverReady makes FTL stop answering after the next restart or
	// reload: the command exits 0, the process lives, nothing answers.
	FaultNeverReady
	// FaultWrongAnswer makes FTL answer every address= name with
	// WrongAnswerIP instead of the line's address.
	FaultWrongAnswer
	// FaultRollbackRestartFails lets the first restart after SetFault
	// succeed and makes every later one fail as FaultRestartFails does: the
	// change's restart works and the rollback's restart does not.
	FaultRollbackRestartFails
)

// WrongAnswerIP is the address FaultWrongAnswer answers (TEST-NET-1).
var WrongAnswerIP = netip.MustParseAddr("192.0.2.254")

// PiholeHostIP is what FTL answers for pi.hole.
var PiholeHostIP = netip.MustParseAddr("127.0.0.1")

// FTLStartDelay is how long a restarted FakeFTL takes to start answering.
// Queries sent meanwhile wait in the socket, as datagrams to a bound port
// do, and are answered once it starts.
const FTLStartDelay = 50 * time.Millisecond

type ftlState int

const (
	ftlStopped   ftlState = iota // No process; queries go unanswered
	ftlStarting                  // Forked, not answering yet: queries wait
	ftlAnswering                 // Serving the snapshot
	ftlHung                      // Alive, never answers (FaultNeverReady)
)

// ftlRule is what dnsmasq does for a domain and every name under it.
type ftlRule struct {
	local bool         // local=/d/, server=/d/ or address=/d/: NXDOMAIN
	a     []netip.Addr // address=/d/<ipv4> (and # as 0.0.0.0)
	other bool         // address=/d/<ipv6>: NOERROR without A records
}

// confLine is one option line dnsmasq loads, with where it came from.
type confLine struct {
	file string // Absolute path on the host, as dnsmasq names it
	n    int    // 1-based line number
	name string
	arg  string
}

// FakeFTL is the fake FTL of a FakeHost (FakeHost.FTL). It encodes the
// verified dnsmasq/FTL v5.25.2 behavior the provider depends on (NFR-7;
// architecture V-10, V-11, V-17; WI 0.1 transcripts in
// test/harness/pihole/transcripts):
//   - a UDP DNS responder on a random loopback port (Addr), answering from a
//     snapshot of the conf files taken at (re)start: address= covers the
//     name and every subdomain, the longest matching domain wins, local=
//     and server=/d/ make a domain NXDOMAIN, other names go to SetUpstream
//     answers or NXDOMAIN;
//   - `pihole restartdns` gives FTL a new PID. It returns before the PID
//     file names it: the new FTL writes the file when it starts answering
//     (A-56), and until then the file and /proc still name the old one;
//   - `pihole restartdns reload` (SIGHUP) flushes only the cache, keeps the
//     PID and does not re-read the conf files; it and reload-lists are
//     no-ops, exit 0, when FTL is not running (V-17);
//   - `pihole-FTL --test` checks the loaded files: an invalid address=
//     line prints dnsmasq's "Bad address in --address at line N of F" and
//     then FTL dies by SIGSEGV, (139, nil) through the runner contract, as
//     FTL v5.25.2 did on every run of WI 0.1 check 3 (SetTestExit switches
//     to a plain exit); without /etc/dnsmasq.conf nothing is checked and
//     it passes (V-10). It never touches the running FTL (A-21);
//   - a list reload is asynchronous (WI 5.6, A-64, A-65; PROVENANCE "FTL
//     LIST RELOAD SIGNALS"): `pihole restartdns reload-lists`, `reload`
//     and a restart log their start marker at once and exit 0, and FTL's
//     database thread re-reads gravity.db ReloadDuration later and logs
//     the completion marker. A signal during a reload makes one more
//     reload follow it (an event flag: several coalesce). A reload during
//     which gravity.db's file change counter changed failed: it logs the
//     captured failure lines, and FTL is then stuck, answering pi.hole and
//     dropping every other query, until a reload with no commit during it
//     succeeds;
//   - queries wait while a list reload runs and are answered when it ends
//     (PROVENANCE "FTL LIST RELOAD SIGNALS", QUERIES DURING A RELOAD: FTL
//     reloads under its shared-memory lock, src/datastructure.c L499-522),
//     one at a time as dnsmasq's single thread does; a client that gives up
//     first gets no reply. A queued reload begins as the one before it ends,
//     so they wait across both: on the image, a few queries held by the
//     first were answered in the ~100 ms between the two, and none sent
//     after the first completion line.
type FakeFTL struct {
	t    testing.TB
	root string
	conn *net.UDPConn
	addr netip.AddrPort
	serv sync.WaitGroup // The responder goroutine
	cbs  sync.WaitGroup // Pending and running timer callbacks

	mu       sync.Mutex
	closed   bool
	state    ftlState
	wake     chan struct{} // Closed and replaced on every state change
	gen      int           // Bumped by every restart and stop
	pid      int
	stale    []int // PIDs whose /proc entry lives until the new PID file is written
	nextPID  int
	rules    map[string]ftlRule
	upstream map[string]netip.Addr
	cache    map[string]netip.Addr
	faults   map[Fault]bool
	faulted  int // Restarts since the last SetFault
	testExit int // 0: --test dies by SIGSEGV on a bad line
	restarts int
	flushes  int
	pidLag   time.Duration
	timers   []*time.Timer

	// The list reload (WI 5.6).
	reloadDur  time.Duration
	reloading  bool     // A reload runs on FTL's database thread
	reloadNext bool     // A signal arrived during it: one more reload follows
	held       bool     // HoldReloads: a reload that reaches its end waits
	parked     []func() // Reloads that reached their end while held
	failNext   int      // FailNextReloads
	stuck      bool     // Answers pi.hole only
	completed  int      // Reloads that ended, failed or not
	reloadSeq  int      // Bumped by every reload that begins or EndReload ends
	reloadCtr  uint32   // gravity.db's change counter when the running reload began
	queries    []FTLQuery
	postReads  time.Duration  // SetPostCompileReads
	reading    bool           // After a completion line, FTL still reads gravity.db
	heldLag    time.Duration  // SetHeldQueryLag
	heldUntil  time.Time      // Queries held by a reload are answered no sooner
	queue      []pendingQuery // Queries received, answered one at a time in order
	qwake      chan struct{}  // Closed and replaced when a query is queued
	gapLeft    int            // Backlog queries that may still be answered between two queued reloads
	gapEpoch   int            // Queries that arrived before this many reloads ended are the backlog
}

// pendingQuery is a datagram FakeFTL received and has not answered yet.
type pendingQuery struct {
	pkt     []byte
	from    netip.AddrPort
	arrived int // Reloads that had ended when it arrived
}

// FTLQuery is one query FakeFTL received while running: its name, and how
// many reloads had completed when it arrived.
type FTLQuery struct {
	Name    string
	Reloads int
}

// FastReload is the list reload barrier's timing for tests: a fifth of the
// production bounds (v5.ListReloadPoll, v5.ListReloadQuiet), still several
// times FakeFTL's reload (DefaultReloadDuration), and a probe timeout that
// a healthy FakeFTL beats under load. The 30 s cap stays production's: tests
// that reach it advance a fake clock (see the fakeTime helper of package v5's
// tests), never the wall clock.
func FastReload() v5.ReloadTiming {
	return v5.ReloadTiming{Poll: 10 * time.Millisecond, Quiet: 50 * time.Millisecond, ProbeTimeout: 250 * time.Millisecond}
}

// DefaultPostCompileReads is how long after a reload's completion line
// FakeFTL still reads gravity.db, in real time unless SetPostCompileReads
// changes it. FTL's check_inaccessible_adlists does read it there, logs
// nothing when it succeeds and LogInaccessibleAdlists when a commit made it
// fail (src/datastructure.c L514, src/database/gravity-db.c L1529-1567;
// PROVENANCE "FTL LIST RELOAD SIGNALS"). How long the read takes is not
// measured, so this is a model parameter. On the device the read runs under
// lock_shm, which dnsmasq's hooks take too, so a query sent during it waits
// for it (PROVENANCE "QUERIES DURING A RELOAD"). FakeFTL is stricter, on
// purpose: it answers a query during the read, so that the barrier's quiet
// window, not a probe that outlasts the read, is what keeps a commit out of
// it.
const DefaultPostCompileReads = 20 * time.Millisecond

// DefaultHeldQueryLag is how long after a reload's completion line the
// queries it held are answered at the earliest: the first held reply came
// 29, 54 and 70 ms after the line in the three runs of PROVENANCE "QUERIES
// DURING A RELOAD" (the shortest is used). That is the permissive direction
// for a probe with a timeout: the device drained its backlog over up to
// ~0.8 s, and FakeFTL answers it at once, so a probe is never answered later
// here than on the device. Nothing models that drain; the barrier's probe
// timeout is pinned to 1 s (ListReloadProbeTimeout, reload_test.go) so the
// difference stays harmless. A query sent when no reload holds it is
// answered at once.
const DefaultHeldQueryLag = 29 * time.Millisecond

// QueuedGapAnswers is how many queries that were held when a reload ended
// FakeFTL answers before the reload queued behind it begins: 5 of the ~28
// held queries were answered in the ~100 ms between two queued reloads, and
// the rest only after the second (PROVENANCE "QUERIES DURING A RELOAD",
// queued reload). A query that arrives after the first completion line is
// held through both.
const QueuedGapAnswers = 5

// LogInaccessibleAdlists is the line FTL logs when its read after a
// reload's completion line cannot open gravity.db (src/database/gravity-db.c
// L1540).
const LogInaccessibleAdlists = "check_inaccessible_adlists(): Gravity database not available"

// DefaultReloadDuration is how long a FakeFTL list reload takes, in real
// time, unless SetReloadDuration changes it. On the image a reload-lists
// took 15 ms to 119 ms from the command to its completion line (TC-153).
const DefaultReloadDuration = 20 * time.Millisecond

// The FTL log lines FakeFTL writes, copied from TC-153's capture of FTL
// v5.25.2 on the harness image (test/harness/pihole/PROVENANCE "FTL LIST
// RELOAD SIGNALS"), each with its source line at the v5.25.2 tag.
const (
	// LogReloadListsStart is logged when FTL receives SIGRTMIN, `pihole
	// restartdns reload-lists` (src/signals.c L279).
	LogReloadListsStart = "Received: Real-time signal 0 (34 -> 0)"
	// LogReloadStart is logged on SIGHUP, `pihole restartdns reload`, and
	// when FTL starts (src/dnsmasq_interface.c L1788).
	LogReloadStart = "Reloading DNS cache"
	// LogCompiled is the completion marker: the line after a reload's
	// regex compilation (src/regex.c L744), logged by a failed reload too.
	LogCompiled = "Compiled 0 whitelist and 0 blacklist regex filters for 1 clients in 0.2 msec"
)

// logReloadFailed are the lines of a reload that met a gravity.db commit,
// in the captured order (TC-153 step 6, INIT=native; src/database/gravity-db.c
// L1155, L203, L1107, L1540; src/regex.c L744). The blocking status line
// follows them.
var logReloadFailed = []string{
	"gravityDB_count(SELECT COUNT(DISTINCT domain) FROM vw_regex_blacklist) - SQL error step database is locked",
	"WARN: Database query failed, assuming there are no blacklist regex entries",
	`SQLite3 message: database is locked in "SELECT domain, CASE WHEN substr(domain, 1, 1) = '*' THEN '*' || ` +
		`substr(:input, - length(domain) + 1) ELSE :input END matcher FROM domain_audit WHERE matcher = domain;" (5)`,
	`gravityDB_open("SELECT EXISTS(... domain_audit ...)") - SQL error prepare: database is locked`,
	"gravityDB_count(4): Gravity database not available",
	"WARN: Database query failed, assuming there are no whitelist regex entries",
	LogCompiled,
	"check_inaccessible_adlists(): Gravity database not available",
}

// newFakeFTL starts FTL as FakeFTLPID, answering from the files under root.
func newFakeFTL(t testing.TB, root string) *FakeFTL {
	t.Helper()
	conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	if err != nil {
		t.Fatalf("v5test: FakeFTL listen: %v", err)
	}
	f := &FakeFTL{
		t: t, root: root, conn: conn, addr: conn.LocalAddr().(*net.UDPAddr).AddrPort(),
		state: ftlAnswering, wake: make(chan struct{}), pid: FakeFTLPID, nextPID: FakeFTLPID,
		upstream: map[string]netip.Addr{}, cache: map[string]netip.Addr{}, faults: map[Fault]bool{},
		reloadDur: DefaultReloadDuration, postReads: DefaultPostCompileReads, heldLag: DefaultHeldQueryLag,
		qwake: make(chan struct{}),
	}
	f.rules, _ = f.snapshot()
	f.serv.Add(2)
	go f.serve()
	go f.work()
	t.Cleanup(f.close)
	return f
}

// close stops the responder and every pending timer, and waits for them.
func (f *FakeFTL) close() {
	f.mu.Lock()
	f.closed = true
	for _, tm := range f.timers {
		if tm.Stop() {
			f.cbs.Done()
		}
	}
	f.timers = nil
	f.setState(f.state)
	f.wakeWorker()
	f.mu.Unlock()
	if err := f.conn.Close(); err != nil {
		f.t.Errorf("v5test: FakeFTL close: %v", err)
	}
	f.serv.Wait()
	f.cbs.Wait()
}

// Addr is where FTL answers DNS.
func (f *FakeFTL) Addr() netip.AddrPort { return f.addr }

// PID is FTL's current process, whether or not its PID file names it yet; 0
// when FTL is stopped.
func (f *FakeFTL) PID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == ftlStopped {
		return 0
	}
	return f.pid
}

// Restarts is how many times `pihole restartdns` ran.
func (f *FakeFTL) Restarts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts
}

// CacheFlushes is how many SIGHUPs (`pihole restartdns reload`) a running
// FTL received.
func (f *FakeFTL) CacheFlushes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushes
}

// SetFault replaces the faults of the following restarts, reloads and
// answers with xs; FaultNone alone, or nothing, clears them.
func (f *FakeFTL) SetFault(xs ...Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = map[Fault]bool{}
	f.faulted = 0
	for _, x := range xs {
		if x != FaultNone {
			f.faults[x] = true
		}
	}
}

// SetTestExit makes `pihole-FTL --test` exit code on a bad address= line
// instead of dying by SIGSEGV: 1 is the plain exit dnsmasq's code path
// predicts (V-10; FTL on amd64 is unobserved). 0 restores the SIGSEGV
// (139) that FTL v5.25.2 showed on aarch64 (WI 0.1 check 3).
func (f *FakeFTL) SetTestExit(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.testExit = code
}

// SetPIDFileLag delays the new PID file to d after a restarted FTL starts
// answering (default 0: written as it starts answering).
func (f *FakeFTL) SetPIDFileLag(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pidLag = d
}

// SetUpstream makes name, when no conf line covers it, resolve upstream to
// ip. Upstream answers are cached until a restart or a SIGHUP.
func (f *FakeFTL) SetUpstream(name string, ip netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upstream[canonical(name)] = ip
}

// Stop stops FTL: its process ends, the PID file is left stale, and nothing
// answers until `pihole restartdns`.
func (f *FakeFTL) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gen++
	f.resetReloads()
	f.removeProc(f.pid)
	f.reapStale()
	f.setState(ftlStopped)
}

// setState changes the state and wakes the queries waiting for a start.
// Callers hold mu.
func (f *FakeFTL) setState(s ftlState) {
	f.state = s
	f.wakeQueries()
}

// wakeQueries wakes the queries waiting for a start or for a reload to
// end. Callers hold mu.
func (f *FakeFTL) wakeQueries() {
	close(f.wake)
	f.wake = make(chan struct{})
}

// restart is `pihole restartdns`: `service pihole-FTL restart` (V-17). The
// new FTL starts answering FTLStartDelay later and only then writes its PID
// file. Callers hold the host's mu.
func (f *FakeFTL) restart(stdout io.Writer) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	f.gen++
	if f.state != ftlStopped {
		f.stale = append(f.stale, f.pid)
	}
	f.cache = map[string]netip.Addr{}
	later := f.faulted > 0
	f.faulted++
	if f.faults[FaultRestartFails] || later && f.faults[FaultRollbackRestartFails] {
		// The stop half of the restart ran: the old FTL is gone, its PID
		// file left stale, as Stop leaves it (TC-151).
		f.resetReloads()
		f.removeProc(f.pid)
		f.reapStale()
		f.setState(ftlStopped)
		fmt.Fprintln(stdout, "  [✗] Job for pihole-FTL.service failed because the control process exited with error code.")
		return 1
	}
	f.nextPID++
	f.pid = f.nextPID
	f.writeFile(filepath.Join(v5.ProcDir, strconv.Itoa(f.pid), "comm"), "pihole-FTL\n")
	rules, ok := f.snapshot()
	switch {
	case !ok:
		// dnsmasq dies on a bad line at start (EC_BADCONF): the restart
		// itself returned once FTL was forked.
		f.removeProc(f.pid)
		f.reapStale()
		f.setState(ftlStopped)
		return 0
	case f.faults[FaultNeverReady]:
		f.reapStale()
		f.setState(ftlHung)
		return 0
	}
	f.rules = rules
	f.setState(ftlStarting)
	f.resetReloads()
	f.logLine(true, "Using log file "+v5.ReadFTLConf(v5.Paths{Root: f.root}).LogFile)
	f.logLine(true, "########## FTL started on fakehost! ##########")
	f.logLine(true, "FTL version: v5.25.2")
	f.startReload(LogReloadStart)
	gen, pid := f.gen, f.pid
	f.after(FTLStartDelay, func() {
		if f.gen != gen {
			return
		}
		f.setState(ftlAnswering)
		f.after(f.pidLag, func() {
			if f.gen != gen {
				return
			}
			f.reapStale()
			f.writeFile(v5.ReadFTLConf(v5.Paths{Root: f.root}).PIDFile, strconv.Itoa(pid)+"\n")
		})
	})
	return 0
}

// signal sends SIGHUP (EventReload) or SIGRTMIN (EventReloadLists) to a
// running FTL, as `pihole restartdns reload|reload-lists` does. It reports
// false, a no-op, when FTL is not running (V-17).
func (f *FakeFTL) signal(ev string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == ftlStopped {
		return false
	}
	if ev == EventReload {
		// SIGHUP clears the cache; the conf files are not re-read. It
		// reloads the lists too (src/dnsmasq_interface.c L1811).
		f.flushes++
		f.cache = map[string]netip.Addr{}
		if f.faults[FaultNeverReady] {
			f.setState(ftlHung)
			return true
		}
		f.startReload(LogReloadStart)
		return true
	}
	if f.state != ftlHung {
		f.startReload(LogReloadListsStart)
	}
	return true
}

// SetReloadDuration sets how long the following list reloads take.
func (f *FakeFTL) SetReloadDuration(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadDur = d
}

// SetPostCompileReads sets how long after a successful reload's completion
// line FTL still reads gravity.db: a commit within it makes that read fail,
// and FTL logs LogInaccessibleAdlists. Zero means no such read.
func (f *FakeFTL) SetPostCompileReads(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postReads = d
}

// SetHeldQueryLag sets how long after a reload's completion line the queries
// it held are answered at the earliest (DefaultHeldQueryLag).
func (f *FakeFTL) SetHeldQueryLag(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heldLag = d
}

// PendingQueries is how many queries FTL has received and not answered
// yet, the one it is holding included.
func (f *FakeFTL) PendingQueries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}

// EndReload ends the reload in progress now, as its timer would: failed
// when gravity.db changed during it, and the reload a signal asked for
// meanwhile begins. With a long SetReloadDuration it lets a test end each
// reload when it chooses. It reports false when none runs.
func (f *FakeFTL) EndReload() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.reloading {
		return false
	}
	f.reloadSeq++
	f.endReload(f.reloadCtr)
	return true
}

// HoldReloads keeps a reload that reaches its end in progress until
// ReleaseReloads: no completion line, and its result is decided only then.
func (f *FakeFTL) HoldReloads() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = true
}

// ReleaseReloads ends the held reloads at once, failed when gravity.db
// changed during them, and lets the following ones end on time.
func (f *FakeFTL) ReleaseReloads() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = false
	parked := f.parked
	f.parked = nil
	for _, end := range parked {
		end()
	}
}

// FailNextReloads makes the next n reloads fail as a race does.
func (f *FakeFTL) FailNextReloads(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = n
}

// ForceStuck makes FTL stuck, with a failed reload record last in its log,
// as a race outside Nexus leaves it.
func (f *FakeFTL) ForceStuck() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logLine(true, LogReloadListsStart)
	f.logFailed()
	f.stuck = true
}

// ForceStuckUnlogged makes FTL stuck while its log's last record shows a
// completed reload: only a query tells.
func (f *FakeFTL) ForceStuckUnlogged() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stuck = true
}

// Stuck reports whether FTL answers pi.hole only.
func (f *FakeFTL) Stuck() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stuck
}

// ReloadsCompleted is how many list reloads ended, failed or not.
func (f *FakeFTL) ReloadsCompleted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completed
}

// Reloading reports whether a list reload is in progress (held or not).
func (f *FakeFTL) Reloading() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reloading
}

// WaitIdle waits, in real time, until no list reload is in progress or
// queued and FTL has finished reading gravity.db after the last. A test that
// runs `pihole -g`, which ends with a SIGHUP reload (gravity.sh L946), and
// then writes gravity.db must do what the real harness does, or it races
// that reload as the device would.
func (f *FakeFTL) WaitIdle(t testing.TB) {
	t.Helper()
	busy := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.reloading || f.reading
	}
	for deadline := time.Now().Add(10 * time.Second); busy(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("v5test: FakeFTL is still reloading its lists")
		}
	}
}

// Queries are the queries FTL received while answering, in order.
func (f *FakeFTL) Queries() []FTLQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

// LogPath is FTL's log on the host: pihole-FTL.conf's LOGFILE=, else
// /var/log/pihole/FTL.log, under the root.
func (f *FakeFTL) LogPath() string {
	return filepath.Join(f.root, v5.ReadFTLConf(v5.Paths{Root: f.root}).LogFile)
}

// startReload logs marker, the line FTL logs when the reload is asked for,
// and starts the reload on the database thread, or, while one runs, makes
// one more follow it. Callers hold mu.
func (f *FakeFTL) startReload(marker string) {
	f.logLine(true, marker)
	if f.reloading {
		f.reloadNext = true
		return
	}
	f.beginReload()
}

// beginReload reads gravity.db's change counter and ends the reload
// reloadDur later. Callers hold mu.
func (f *FakeFTL) beginReload() {
	f.reloading = true
	f.reloadSeq++
	counter, gen, seq := f.changeCounter(), f.gen, f.reloadSeq
	f.reloadCtr = counter
	end := func() {
		if f.gen == gen && f.reloadSeq == seq {
			f.endReload(counter)
		}
	}
	f.after(f.reloadDur, func() {
		if f.gen != gen || f.reloadSeq != seq {
			return
		}
		if f.held {
			f.parked = append(f.parked, end)
			return
		}
		end()
	})
}

// endReload fails the reload when gravity.db was committed to since it
// began, or when FailNextReloads asks; it then starts the reload a signal
// asked for meanwhile. A reload that succeeded still reads gravity.db for
// postReads (check_inaccessible_adlists), logging nothing unless a commit
// lands in that time; on the device that read holds queries (lock_shm),
// which FakeFTL does not model (DefaultPostCompileReads). When a reload
// follows, the queries held so far, up to QueuedGapAnswers, are answered
// before it begins. Callers hold mu.
func (f *FakeFTL) endReload(counter uint32) {
	failed := f.failNext > 0 || f.changeCounter() != counter
	if f.failNext > 0 {
		f.failNext--
	}
	if failed {
		f.logFailed()
	} else {
		f.logLine(false, LogCompiled)
		f.logBlocking()
	}
	f.readAfterCompile(failed)
	f.heldUntil = time.Now().Add(f.heldLag)
	f.stuck = failed
	f.completed++
	f.reloading = false
	f.gapLeft = 0
	if f.reloadNext {
		f.reloadNext = false
		f.gapLeft, f.gapEpoch = QueuedGapAnswers, f.completed
		f.beginReload()
	}
	f.wakeQueries()
}

// readAfterCompile is FTL's read of gravity.db after a reload's completion
// line: a commit during the next postReads makes it fail, logged as
// LogInaccessibleAdlists (a failed reload's own lines already include it).
// The window is [now, now+postReads] on FakeFTL's own clock, and a commit
// counts when its time, gravity.db's modification time, falls inside it. It
// is not judged by when the timer below runs: a callback that a loaded host
// (the race detector, a parallel `go test ./...`) runs late must not count a
// commit made after the window as one made during it (code review CR-23).
// Callers hold mu.
func (f *FakeFTL) readAfterCompile(failed bool) {
	if f.postReads <= 0 {
		return
	}
	counter, gen := f.changeCounter(), f.gen
	end := time.Now().Add(f.postReads)
	f.reading = true
	f.after(f.postReads, func() {
		if f.gen != gen {
			return
		}
		if !failed && f.changeCounter() != counter && f.lastCommit().Compare(end) <= 0 {
			f.logLine(false, LogInaccessibleAdlists)
		}
		f.reading = false
	})
}

// resetReloads forgets the reloads of a process that ended. Callers hold
// mu.
func (f *FakeFTL) resetReloads() {
	f.reloading, f.reloadNext, f.parked, f.stuck, f.gapLeft, f.reading = false, false, nil, false, 0, false
}

// logFailed logs a failed reload's lines. Callers hold mu.
func (f *FakeFTL) logFailed() {
	for _, l := range logReloadFailed {
		f.logLine(false, l)
	}
	f.logBlocking()
}

// logBlocking is the "Blocking status is …" line FTL logs after a reload
// (src/setupVars.c L256). Callers hold mu.
func (f *FakeFTL) logBlocking() {
	state := "enabled"
	if b := v5.ReadBlockingEnabled(v5.Paths{Root: f.root}); b != nil && !*b {
		state = "disabled"
	}
	f.logLine(false, "Blocking status is "+state)
}

// logLine appends msg to FTL's log as FTL v5.25.2 formats it, from the
// main process (M) or the database thread (/T). FTL opens the log for
// every line (src/log.c L133), so a deleted log is created again. Callers
// hold mu.
func (f *FakeFTL) logLine(main bool, msg string) {
	who := fmt.Sprintf("%dM", f.pid)
	if !main {
		who = fmt.Sprintf("%d/T%d", f.pid, f.pid+16)
	}
	p := f.LogPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Errorf("v5test: FakeFTL: %v", err)
		return
	}
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Errorf("v5test: FakeFTL: %v", err)
		return
	}
	defer fh.Close()
	if _, err := fmt.Fprintf(fh, "[%s %s] %s\n", time.Now().Format("2006-01-02 15:04:05.000"), who, msg); err != nil {
		f.t.Errorf("v5test: FakeFTL: %v", err)
	}
}

// changeCounter is gravity.db's file change counter, header bytes 24-27,
// which every commit in rollback-journal mode increments (sqlite.org
// fileformat2 §1.3.8); 0 when it cannot be read.
func (f *FakeFTL) changeCounter() uint32 {
	fh, err := os.Open(filepath.Join(f.root, v5.GravityDBFile))
	if err != nil {
		return 0
	}
	defer fh.Close()
	var b [4]byte
	if _, err := fh.ReadAt(b[:], 24); err != nil {
		return 0
	}
	return binary.BigEndian.Uint32(b[:])
}

// lastCommit is the time of gravity.db's latest commit, its modification
// time (a rollback-journal commit rewrites the file's header, and so
// touches it); the zero time when it cannot be read. Only the latest commit
// is known: a window's earlier commit is missed when a later one follows it
// before the window's timer runs, which needs that timer to be late.
func (f *FakeFTL) lastCommit() time.Time {
	fi, err := os.Stat(filepath.Join(f.root, v5.GravityDBFile))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// after runs fn under mu d from now, unless FakeFTL is closed first.
// Callers hold mu.
func (f *FakeFTL) after(d time.Duration, fn func()) {
	if f.closed {
		return
	}
	f.cbs.Add(1)
	f.timers = append(f.timers, time.AfterFunc(d, func() {
		defer f.cbs.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.closed {
			fn()
		}
	}))
}

// reapStale ends the processes a restart replaced. Callers hold mu.
func (f *FakeFTL) reapStale() {
	for _, pid := range f.stale {
		if pid != f.pid {
			f.removeProc(pid)
		}
	}
	f.stale = nil
}

func (f *FakeFTL) removeProc(pid int) {
	if err := os.RemoveAll(filepath.Join(f.root, v5.ProcDir, strconv.Itoa(pid))); err != nil {
		f.t.Errorf("v5test: FakeFTL: %v", err)
	}
}

func (f *FakeFTL) writeFile(abs, body string) {
	p := filepath.Join(f.root, abs)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Errorf("v5test: FakeFTL: %v", err)
		return
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Errorf("v5test: FakeFTL: %v", err)
	}
}

// configTest is `pihole-FTL --test` (V-10): dnsmasq's syntax check of the
// loaded files, on stderr as dnsmasq writes it (FTL v5.25.2
// src/dnsmasq/option.c L3055, L5511, L6219). It reads the files as they are
// now and does not touch the running FTL (A-21).
func (f *FakeFTL) configTest(stderr io.Writer) int {
	f.mu.Lock()
	exit := f.testExit
	f.mu.Unlock()
	lines, ok := f.load()
	if !ok {
		// No /etc/dnsmasq.conf: nothing is loaded, so nothing fails (V-10).
		fmt.Fprintln(stderr, "dnsmasq: syntax check OK.")
		return 0
	}
	for _, l := range lines {
		if l.name == "address" && !validAddress(l.arg) {
			fmt.Fprintf(stderr, "\ndnsmasq: Bad address in --address at line %d of %s\n", l.n, l.file)
			if exit != 0 {
				return exit
			}
			// FTL v5.25.2 then crashed (WI 0.1 check 3); the runner reports
			// the death as Exec does (A-19).
			fmt.Fprintf(stderr, "pihole: %s killed by signal %s\n", v5.PiholeFTLBin, v5.SignalName(syscall.SIGSEGV))
			return 128 + int(syscall.SIGSEGV)
		}
	}
	fmt.Fprintln(stderr, "dnsmasq: syntax check OK.")
	return 0
}

// validAddress is dnsmasq's check of an address= value (option.c L3040-3055):
// none, "#", or an address inet_pton accepts.
func validAddress(arg string) bool {
	_, val := splitDomains(arg)
	if val == "" || val == "#" {
		return true
	}
	ip, err := netip.ParseAddr(val)
	return err == nil && ip.Zone() == ""
}

// splitDomains splits "/d1/d2/value" into its domains and value; an arg
// without a leading '/' is all value.
func splitDomains(arg string) ([]string, string) {
	if !strings.HasPrefix(arg, "/") {
		return nil, arg
	}
	parts := strings.Split(arg[1:], "/")
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// load reads the option lines dnsmasq loads: /etc/dnsmasq.conf and, at
// each conf-dir= line, the directory's files in strcmp order, skipping
// names that start with '.', end with '~' or are #…#, anything not a
// regular file, and names a suffix filter excludes (V-11). It reports
// false without /etc/dnsmasq.conf.
func (f *FakeFTL) load() ([]confLine, bool) {
	main, err := f.readLines(v5.DnsmasqConfFile)
	if err != nil {
		return nil, false
	}
	var out []confLine
	for _, l := range main {
		if l.name != "conf-dir" {
			out = append(out, l)
			continue
		}
		parts := strings.Split(l.arg, ",")
		dir := strings.TrimSpace(parts[0])
		entries, err := os.ReadDir(filepath.Join(f.root, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") ||
				len(name) >= 2 && strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#") ||
				!suffixAdmits(parts[1:], name) {
				continue
			}
			abs := filepath.Join(dir, name)
			if fi, err := os.Stat(filepath.Join(f.root, abs)); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			lines, err := f.readLines(abs)
			if err == nil {
				out = append(out, lines...)
			}
		}
	}
	return out, true
}

// suffixAdmits applies conf-dir's filters: "*.x" admits only names ending
// in .x; ".x" skips names ending in .x.
func suffixAdmits(filters []string, name string) bool {
	admit, filtered := false, false
	for _, s := range filters {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "*"):
			filtered = true
			if strings.HasSuffix(name, s[1:]) {
				admit = true
			}
		case s != "" && strings.HasSuffix(name, s):
			return false
		}
	}
	return !filtered || admit
}

// readLines parses abs's option lines: "name=arg" or a bare "name"; blank
// and '#' lines are skipped.
func (f *FakeFTL) readLines(abs string) ([]confLine, error) {
	fh, err := os.Open(filepath.Join(f.root, abs))
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []confLine
	sc := bufio.NewScanner(fh)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, arg, _ := strings.Cut(line, "=")
		out = append(out, confLine{file: abs, n: n, name: strings.TrimSpace(name), arg: strings.TrimSpace(arg)})
	}
	return out, sc.Err()
}

// snapshot builds the rules a starting FTL serves. It reports false when a
// loaded address= line is invalid: dnsmasq would not start.
func (f *FakeFTL) snapshot() (map[string]ftlRule, bool) {
	lines, _ := f.load()
	rules := map[string]ftlRule{}
	for _, l := range lines {
		if l.name != "address" && l.name != "local" && l.name != "server" {
			continue
		}
		domains, val := splitDomains(l.arg)
		if l.name == "address" && !validAddress(l.arg) {
			return nil, false
		}
		for _, d := range domains {
			d = canonical(d)
			if d == "" || d == "#" {
				continue
			}
			r := rules[d]
			switch {
			case val == "":
				r.local = true
			case l.name != "address":
				continue // server=/d/upstream forwards, as an unlisted name does
			case val == "#":
				r.a = append(r.a, netip.IPv4Unspecified())
			default:
				if ip := netip.MustParseAddr(val); ip.Is4() {
					r.a = append(r.a, ip)
				} else {
					r.other = true
				}
			}
			rules[d] = r
		}
	}
	return rules, true
}

func canonical(name string) string { return strings.ToLower(strings.TrimSuffix(name, ".")) }

// serve receives queries until close and queues them with the number of
// reloads that had ended when each arrived; work answers them.
func (f *FakeFTL) serve() {
	defer f.serv.Done()
	buf := make([]byte, 4096)
	for {
		n, from, err := f.conn.ReadFromUDPAddrPort(buf)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			continue
		}
		f.mu.Lock()
		f.queue = append(f.queue, pendingQuery{pkt: slices.Clone(buf[:n]), from: from, arrived: f.completed})
		f.wakeWorker()
		f.mu.Unlock()
	}
}

// wakeWorker wakes work to look at its queue. Callers hold mu.
func (f *FakeFTL) wakeWorker() {
	close(f.qwake)
	f.qwake = make(chan struct{})
}

// work answers the queue one query at a time in arrival order, as dnsmasq's
// single thread does: the head waits while FTL starts or reloads its lists,
// and is dropped, unanswered, when FTL is stopped or hung.
func (f *FakeFTL) work() {
	defer f.serv.Done()
	for {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return
		}
		if len(f.queue) == 0 {
			wake := f.qwake
			f.mu.Unlock()
			<-wake
			continue
		}
		q := f.queue[0]
		var reply []byte
		switch {
		case f.state == ftlAnswering && (!f.reloading || f.gapLeft > 0 && q.arrived < f.gapEpoch) &&
			q.arrived < f.completed && time.Now().Before(f.heldUntil):
			// Held by the reload that ended: answered DefaultHeldQueryLag
			// after its completion line at the earliest.
			wake, d := f.wake, time.Until(f.heldUntil)
			f.mu.Unlock()
			tm := time.NewTimer(d)
			select {
			case <-tm.C:
			case <-wake:
			}
			tm.Stop()
			continue
		case f.state == ftlAnswering && !f.reloading:
			reply = f.reply(q.pkt)
		case f.state == ftlAnswering && f.gapLeft > 0 && q.arrived < f.gapEpoch:
			// Held through the reload that just ended; answered before the
			// queued one begins (QueuedGapAnswers).
			f.gapLeft--
			reply = f.reply(q.pkt)
		case f.state == ftlAnswering || f.state == ftlStarting:
			wake := f.wake
			f.mu.Unlock()
			<-wake
			continue
		}
		f.queue = f.queue[1:]
		f.mu.Unlock()
		if reply != nil {
			_, _ = f.conn.WriteToUDPAddrPort(reply, q.from) // the client may have given up
		}
	}
}

// reply builds the response from the snapshot. Callers hold mu.
func (f *FakeFTL) reply(query []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	name := canonical(q.Name.String())
	f.queries = append(f.queries, FTLQuery{Name: name, Reloads: f.completed})
	if f.stuck && name != "pi.hole" {
		// A failed list reload leaves FTL answering pi.hole only (TC-153).
		return nil
	}
	rcode, ips := f.resolve(name)
	if q.Type != dnsmessage.TypeA {
		ips = nil
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionDesired: h.RecursionDesired,
		RecursionAvailable: true, RCode: rcode})
	b.EnableCompression()
	if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
		return nil
	}
	for _, ip := range ips {
		if b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET},
			dnsmessage.AResource{A: ip.As4()}) != nil {
			return nil
		}
	}
	msg, err := b.Finish()
	if err != nil {
		return nil
	}
	return msg
}

// resolve is dnsmasq's answer for name: the rule of the longest domain that
// is name or a parent of it on a label boundary, else upstream (cached).
// Callers hold mu.
func (f *FakeFTL) resolve(name string) (dnsmessage.RCode, []netip.Addr) {
	if name == "pi.hole" {
		return dnsmessage.RCodeSuccess, []netip.Addr{PiholeHostIP}
	}
	for d := name; d != ""; {
		if r, ok := f.rules[d]; ok {
			switch {
			case len(r.a) > 0:
				if f.faults[FaultWrongAnswer] {
					return dnsmessage.RCodeSuccess, []netip.Addr{WrongAnswerIP}
				}
				return dnsmessage.RCodeSuccess, r.a
			case r.other:
				return dnsmessage.RCodeSuccess, nil
			case r.local:
				return dnsmessage.RCodeNameError, nil
			}
		}
		_, rest, ok := strings.Cut(d, ".")
		if !ok {
			break
		}
		d = rest
	}
	if ip, ok := f.cache[name]; ok {
		return dnsmessage.RCodeSuccess, []netip.Addr{ip}
	}
	if ip, ok := f.upstream[name]; ok {
		f.cache[name] = ip
		return dnsmessage.RCodeSuccess, []netip.Addr{ip}
	}
	return dnsmessage.RCodeNameError, nil
}
