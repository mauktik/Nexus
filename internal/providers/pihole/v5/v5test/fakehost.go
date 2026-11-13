// Package v5test holds the T0 fakes of a Pi-hole v5 host. FakeHost
// implements v5.Runner over a temp root and encodes the verified v5.18.4
// behavior the provider depends on (NFR-7; architecture V-9, V-12, V-13,
// V-14), hazards included, so a production path that relies on a hazard
// fails against the fake instead of passing. Anything it does not encode
// exits 127 and is recorded.
package v5test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

// FTL events FakeHost records (FTLEvents).
const (
	EventRestart     = "restart"      // pihole restartdns
	EventReload      = "reload"       // pihole restartdns reload (SIGHUP)
	EventReloadLists = "reload-lists" // pihole restartdns reload-lists (SIGRTMIN)
)

// FakeFTLPID is the PID the seeded PID file and /proc/<pid>/comm name.
const FakeFTLPID = 4242

// FTLUnit is the native systemd unit of FTL on the v5 image.
const FTLUnit = "pihole-FTL.service"

// The start limit of FTLUnit: core v5.18.4
// advanced/Templates/pihole-FTL.systemd L14-16 (StartLimitBurst=5,
// StartLimitIntervalSec=60s; WI 5.1 found it on the image, A-60).
const (
	ftlStartLimitBurst    = 5
	ftlStartLimitInterval = 60 * time.Second
)

// checkURL is webpage.sh CheckUrl of Pi-hole core v5.18.4 (webpage.sh:542),
// as captured in internal/providers/pihole/spec/testdata/upstream-validators.txt.
var checkURL = regexp.MustCompile(`^[a-zA-Z0-9:/?&%=~._()-;]*$`)

// start is the fake clock's initial time.
var start = time.Date(2026, 3, 14, 15, 9, 26, 535_897_932, time.UTC)

// DomainRow is one domainlist row.
type DomainRow struct {
	ID      int64
	Type    int
	Domain  string
	Comment string
}

type faultKind int

const (
	faultExit   faultKind = iota // Exit with code, no effect
	faultSignal                  // Death by signal: (128+sig, nil)
	faultStart                   // Not started: (-1, err)
	faultIgnore                  // Exit 0, no effect
)

type fault struct {
	prefix []string
	kind   faultKind
	code   int
	sig    syscall.Signal
	once   bool
}

// child is the nohup re-enable child of `pihole disable Ns`: it lives in
// the calling unit's cgroup and dies with it (V-13).
type child struct {
	at time.Time
}

// timer is a transient systemd timer: its own unit, outside the calling
// unit's cgroup (A-27).
type timer struct {
	unit        string // nexus-pihole-reenable.timer
	at          time.Time
	description string
	argv        []string
}

// FakeHost is a fake Pi-hole v5 host under a temp root.
type FakeHost struct {
	t    testing.TB
	root string

	mu          sync.Mutex
	now         time.Time
	argvs       [][]string
	events      []string
	children    []child
	timers      []*timer
	faults      []*fault
	failAdlists map[string]bool
	nextPID     int
	ftl         *FakeFTL

	// FTLUnit's start rate limit, as systemd v247's ratelimit_below keeps
	// it: a window that opens at its first start, and the starts in it.
	limitBegin time.Time
	limitNum   int
	limitHit   bool // Result=start-limit-hit until reset-failed or a start
}

var _ v5.Runner = (*FakeHost)(nil)

// NewFakeHost builds a fresh host tree under t.TempDir(): setupVars.conf
// (BLOCKING_ENABLED=true and a WEBPASSWORD line), versions, pihole-FTL.conf,
// a gravity.db from the v5.18.4 schema, /etc/dnsmasq.conf loading
// /etc/dnsmasq.d, the dnsmasq.d files (03-custom-dns-names.conf empty), a
// non-empty query log, FTL's PID file and the two binaries.
func NewFakeHost(t testing.TB) *FakeHost {
	t.Helper()
	h := &FakeHost{t: t, root: t.TempDir(), now: start, failAdlists: map[string]bool{}, nextPID: 31000}
	versions, err := os.ReadFile(filepath.Join(sourceDir(t), "..", "testdata", "versions"))
	if err != nil {
		t.Fatalf("v5test: %v", err)
	}
	files := []struct {
		path string
		body string
		mode os.FileMode
	}{
		{v5.PiholeBin, "#!/bin/bash\n# fake\n", 0o755},
		{v5.PiholeFTLBin, "fake\n", 0o755},
		{v5.VersionsFile, string(versions), 0o644},
		{v5.SetupVarsFile, "PIHOLE_INTERFACE=eth0\nQUERY_LOGGING=true\n" +
			"WEBPASSWORD=" + WebPasswordHash + "\nBLOCKING_ENABLED=true\nDNSSEC=false\n", 0o644},
		{v5.FTLConfFile, "PRIVACYLEVEL=0\nPIDFILE=" + v5.DefaultFTLPIDFile + "\n", 0o644},
		{v5.DefaultFTLPIDFile, strconv.Itoa(FakeFTLPID) + "\n", 0o644},
		{filepath.Join(v5.ProcDir, strconv.Itoa(FakeFTLPID), "comm"), "pihole-FTL\n", 0o444},
		{v5.DnsmasqConfFile, "conf-dir=/etc/dnsmasq.d\n", 0o644},
		{"/etc/dnsmasq.d/01-pihole.conf", "addn-hosts=/etc/pihole/local.list\nlocalise-queries\n", 0o644},
		{"/etc/dnsmasq.d/06-rfc6761.conf", "server=/test/\nserver=/localhost/\nserver=/invalid/\n", 0o644},
		{"/etc/dnsmasq.d/03-custom-dns-names.conf", "", 0o644},
		{QueryLog, "Mar 14 15:00:01 dnsmasq[4242]: query[A] example.com from 127.0.0.1\n" +
			"Mar 14 15:00:01 dnsmasq[4242]: forwarded example.com to 1.1.1.1\n", 0o644},
	}
	for _, f := range files {
		h.writeFile(f.path, f.body, f.mode)
	}
	h.ftl = newFakeFTL(t, h.root)
	schema, err := os.ReadFile(filepath.Join(sourceDir(t), "..", "..", "gravitydb", "testdata", "gravity.db.sql"))
	if err != nil {
		t.Fatalf("v5test: %v", err)
	}
	h.withDB(func(db *sql.DB) error {
		_, err := db.Exec(string(schema))
		return err
	})
	return h
}

// WebPasswordHash is the planted WEBPASSWORD value: no result, log or
// audit record may ever contain it (FR-29).
const WebPasswordHash = "9f2c1e7a4b8d3f60c5a1e2d4b7f8091a3c6e5d4f2b1a0987c6d5e4f3a2b1c0d9e"

// QueryLog is Pi-hole's query log, which `pihole flush` empties.
const QueryLog = "/var/log/pihole/pihole.log"

func sourceDir(t testing.TB) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("v5test: cannot locate the package source")
	}
	return filepath.Dir(file)
}

func (h *FakeHost) writeFile(abs, body string, mode os.FileMode) {
	h.t.Helper()
	p := h.path(abs)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatalf("v5test: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		h.t.Fatalf("v5test: %v", err)
	}
}

func (h *FakeHost) path(abs string) string { return filepath.Join(h.root, abs) }

// withDB runs f on the host's gravity.db, opened for this call only, with
// foreign keys enforced, as `pihole-FTL sqlite3` opens it (PRAGMA
// foreign_keys = 1 on the v5 image; WI 5.1 PROVENANCE "ADLIST REPLACEMENT").
func (h *FakeHost) withDB(f func(*sql.DB) error) {
	h.t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.path(v5.GravityDBFile)+"?_pragma=foreign_keys(1)")
	if err != nil {
		h.t.Fatalf("v5test: open gravity.db: %v", err)
	}
	defer db.Close()
	if err := f(db); err != nil {
		h.t.Fatalf("v5test: gravity.db: %v", err)
	}
}

// Root is the host's root directory.
func (h *FakeHost) Root() string { return h.root }

// Paths are v5.Paths under the host's root.
func (h *FakeHost) Paths() v5.Paths { return v5.Paths{Root: h.root} }

// Now is the fake clock.
func (h *FakeHost) Now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

// Advance moves the fake clock by d. Re-enable children and timers that
// come due run synchronously, in time order.
func (h *FakeHost) Advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	end := h.now.Add(d)
	for {
		at, fire, ok := h.nextDue(end)
		if !ok {
			break
		}
		h.now = at
		fire()
	}
	h.now = end
}

// nextDue is the earliest child or timer due by end, removed, and what it
// runs.
func (h *FakeHost) nextDue(end time.Time) (time.Time, func(), bool) {
	ci, ti := -1, -1
	var at time.Time
	earlier := func(t time.Time) bool { return !t.After(end) && (ci < 0 && ti < 0 || t.Before(at)) }
	for i, c := range h.children {
		if earlier(c.at) {
			ci, at = i, c.at
		}
	}
	for i, tm := range h.timers {
		if earlier(tm.at) {
			ci, ti, at = -1, i, tm.at
		}
	}
	switch {
	case ti >= 0:
		tm := h.timers[ti]
		h.timers = slices.Delete(h.timers, ti, ti+1) // RemainAfterElapse=no
		return at, func() { h.exec(tm.argv, io.Discard, io.Discard) }, true
	case ci >= 0:
		h.children = slices.Delete(h.children, ci, ci+1)
		return at, func() { h.enable() }, true
	}
	return time.Time{}, nil, false
}

// EndUnit ends the calling operation unit: KillMode=control-group kills
// every process of its cgroup, so a native `pihole disable Ns` child dies
// and never re-enables (V-13). Transient timers are units of their own and
// survive.
func (h *FakeHost) EndUnit() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.children = nil
}

// Argvs is every argv run, in order.
func (h *FakeHost) Argvs() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([][]string, len(h.argvs))
	for i, a := range h.argvs {
		out[i] = slices.Clone(a)
	}
	return out
}

// ResetArgvs forgets the recorded argvs.
func (h *FakeHost) ResetArgvs() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.argvs = nil
}

// FTLEvents are the signals FTL received, in order: EventRestart,
// EventReload or EventReloadLists.
func (h *FakeHost) FTLEvents() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

// FTL is the host's FTL.
func (h *FakeHost) FTL() *FakeFTL { return h.ftl }

// ftlEvent sends a restartdns variant to FTL and records it. A full restart
// returns the command's exit code; reload and reload-lists exit 0 and are
// not recorded when FTL is not running, as `pihole` then sends no signal
// (V-17). A full restart is `systemctl restart pihole-FTL.service`, one
// start of FTLUnit: past the start limit the stop half has already run,
// the start is refused, and FTL stays stopped (A-60). Callers hold mu.
func (h *FakeHost) ftlEvent(ev string, stdout io.Writer) int {
	if ev == EventRestart {
		h.events = append(h.events, ev)
		if !h.ftlStartAllowed() {
			h.ftl.Stop()
			h.limitHit = true
			fmt.Fprintf(stdout, "  [✗] Job for %s failed because start of the service was attempted too often.\n", FTLUnit)
			return 1
		}
		h.limitHit = false
		return h.ftl.restart(stdout)
	}
	if h.ftl.signal(ev) {
		h.events = append(h.events, ev)
	} else {
		fmt.Fprintln(stdout, "  [i] FTL is not running")
	}
	return 0
}

// ftlStartAllowed is systemd v247's ratelimit_below (src/basic/ratelimit.c
// L11-38) for FTLUnit at the fake clock's time: a start more than the
// interval after the window opened opens a new one; otherwise it is allowed
// while fewer than the burst have started in the window, and counted. A
// refused start is not counted. Callers hold mu.
func (h *FakeHost) ftlStartAllowed() bool {
	if h.limitBegin.IsZero() || h.now.Sub(h.limitBegin) > ftlStartLimitInterval {
		h.limitBegin, h.limitNum = h.now, 0
	}
	if h.limitNum >= ftlStartLimitBurst {
		return false
	}
	h.limitNum++
	return true
}

// PrimeFTLStarts records n starts of FTLUnit at the fake clock's time, as
// if FTL had been (re)started n times outside Nexus. Starts past the limit
// are refused and not counted, as systemd's are.
func (h *FakeHost) PrimeFTLStarts(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for range n {
		h.ftlStartAllowed()
	}
}

// BlockingEnabled is BLOCKING_ENABLED in setupVars.conf, "" when absent.
func (h *FakeHost) BlockingEnabled() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.blockingEnabled()
}

func (h *FakeHost) blockingEnabled() string {
	b, err := os.ReadFile(h.path(v5.SetupVarsFile))
	if err != nil {
		h.t.Fatalf("v5test: %v", err)
	}
	val := ""
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "BLOCKING_ENABLED="); ok {
			val = v
		}
	}
	return val
}

// setBlocking edits only the BLOCKING_ENABLED line, as pihole's
// addOrEditKeyValPair does.
func (h *FakeHost) setBlocking(on bool) {
	b, err := os.ReadFile(h.path(v5.SetupVarsFile))
	if err != nil {
		h.t.Fatalf("v5test: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	val := "BLOCKING_ENABLED=" + strconv.FormatBool(on)
	found := false
	for i, line := range lines {
		if strings.HasPrefix(line, "BLOCKING_ENABLED=") {
			lines[i], found = val, true
		}
	}
	if !found {
		lines = append(lines, val)
	}
	if err := os.WriteFile(h.path(v5.SetupVarsFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		h.t.Fatalf("v5test: %v", err)
	}
}

// TimerPending returns when the pending nexus-pihole-reenable timer fires.
func (h *FakeHost) TimerPending() (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if tm := h.timer(v5.ReenableTimerUnit); tm != nil {
		return tm.at, true
	}
	return time.Time{}, false
}

func (h *FakeHost) timer(unit string) *timer {
	for _, tm := range h.timers {
		if tm.unit == unit {
			return tm
		}
	}
	return nil
}

func (h *FakeHost) addFault(f *fault) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.faults = append(h.faults, f)
}

// FailCommand makes every argv starting with prefix exit code, with no
// effect.
func (h *FakeHost) FailCommand(prefix []string, code int) {
	h.addFault(&fault{prefix: slices.Clone(prefix), kind: faultExit, code: code})
}

// FailCommandOnce makes the next argv starting with prefix exit code, with
// no effect.
func (h *FakeHost) FailCommandOnce(prefix []string, code int) {
	h.addFault(&fault{prefix: slices.Clone(prefix), kind: faultExit, code: code, once: true})
}

// FailCommandSignal makes every argv starting with prefix die by sig, with
// no effect, reported as the real runner does: (128+sig, nil) and a line
// naming the signal on stderr.
func (h *FakeHost) FailCommandSignal(prefix []string, sig syscall.Signal) {
	h.addFault(&fault{prefix: slices.Clone(prefix), kind: faultSignal, sig: sig})
}

// FailCommandSignalOnce is FailCommandSignal for the next matching argv
// only.
func (h *FakeHost) FailCommandSignalOnce(prefix []string, sig syscall.Signal) {
	h.addFault(&fault{prefix: slices.Clone(prefix), kind: faultSignal, sig: sig, once: true})
}

// FailCommandStart makes every argv starting with prefix fail to start,
// reported as the real runner does: (-1, err).
func (h *FakeHost) FailCommandStart(prefix []string) {
	h.addFault(&fault{prefix: slices.Clone(prefix), kind: faultStart})
}

// FailCommandStartOnce is FailCommandStart for the next matching argv only.
func (h *FakeHost) FailCommandStartOnce(prefix []string) {
	h.addFault(&fault{prefix: slices.Clone(prefix), kind: faultStart, once: true})
}

// IgnoreCommand makes every argv starting with prefix exit 0 with no
// effect, as a command that silently did nothing.
func (h *FakeHost) IgnoreCommand(prefix []string) {
	h.addFault(&fault{prefix: slices.Clone(prefix), kind: faultIgnore})
}

// matchFault is the first fault whose prefix argv starts with, consumed
// when it is a one-shot.
func (h *FakeHost) matchFault(argv []string) *fault {
	for i, f := range h.faults {
		if len(argv) >= len(f.prefix) && slices.Equal(argv[:len(f.prefix)], f.prefix) {
			if f.once {
				h.faults = slices.Delete(h.faults, i, i+1)
			}
			return f
		}
	}
	return nil
}

// Run implements v5.Runner. It records argv, applies an injected fault,
// else what the v5 command does.
func (h *FakeHost) Run(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.argvs = append(h.argvs, slices.Clone(argv))
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return -1, fmt.Errorf("v5test: command %q is not an absolute path", argv)
	}
	if err := ctx.Err(); err != nil {
		return -1, fmt.Errorf("v5test: %s not started: %w", argv[0], err)
	}
	if f := h.matchFault(argv); f != nil {
		switch f.kind {
		case faultSignal:
			fmt.Fprintf(stderr, "pihole: %s killed by signal %s\n", argv[0], v5.SignalName(f.sig))
			return 128 + int(f.sig), nil
		case faultStart:
			return -1, fmt.Errorf("v5test: start %s: %w", argv[0], syscall.ENOEXEC)
		case faultIgnore:
			return 0, nil
		default:
			return f.code, nil
		}
	}
	return h.exec(argv, stdout, stderr), nil
}

// exec is what argv does on a v5.18.4 host. Callers hold mu.
func (h *FakeHost) exec(argv []string, stdout, stderr io.Writer) int {
	args := argv[1:]
	switch argv[0] {
	case v5.PiholeBin:
		return h.pihole(args, stdout, stderr)
	case v5.PiholeFTLBin:
		if len(args) != 1 || args[0] != "--test" {
			return unknown(argv, stderr)
		}
		return h.ftl.configTest(stderr)
	case detach.SystemdRun:
		return h.systemdRun(args, stdout, stderr)
	case detach.Systemctl:
		return h.systemctl(args, stdout, stderr)
	}
	return unknown(argv, stderr)
}

func unknown(argv []string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "v5test: no such command on the fake host: %q\n", argv)
	return 127
}

func (h *FakeHost) pihole(args []string, stdout, stderr io.Writer) int {
	argv := append([]string{v5.PiholeBin}, args...)
	if len(args) == 0 {
		return unknown(argv, stderr)
	}
	switch args[0] {
	case "enable":
		if len(args) != 1 {
			return unknown(argv, stderr)
		}
		fmt.Fprintln(stdout, "  [i] Enabling blocking")
		h.enable()
		fmt.Fprintln(stdout, "  [✓] Pi-hole Enabled")
		return 0
	case "disable":
		return h.disable(argv, args[1:], stdout, stderr)
	case "restartdns":
		switch {
		case len(args) == 1:
			return h.ftlEvent(EventRestart, stdout)
		case len(args) == 2 && args[1] == "reload":
			return h.ftlEvent(EventReload, stdout)
		case len(args) == 2 && args[1] == "reload-lists":
			return h.ftlEvent(EventReloadLists, stdout)
		}
		return unknown(argv, stderr)
	case "flush":
		if len(args) != 1 {
			return unknown(argv, stderr)
		}
		// Empties the query log; FTL is not touched (FR-15).
		if err := os.WriteFile(h.path(QueryLog), nil, 0o644); err != nil {
			h.t.Fatalf("v5test: %v", err)
		}
		fmt.Fprintln(stdout, "  [✓] Flushed /var/log/pihole/pihole.log")
		return 0
	case "-w", "-b", "--white-regex", "--regex":
		if len(args) != 2 {
			return unknown(argv, stderr)
		}
		return h.listSh(args[0], args[1], stdout)
	case "-a":
		if len(args) >= 4 && args[1] == "adlist" && (args[2] == "add" && len(args) <= 5 || args[2] == "del" && len(args) == 4) {
			comment := ""
			if len(args) == 5 {
				comment = args[4]
			}
			h.adlist(args[2], args[3], comment, stderr)
			return 0
		}
		return unknown(argv, stderr)
	case "-g":
		if len(args) != 1 {
			return unknown(argv, stderr)
		}
		return h.gravity(stdout)
	}
	return unknown(argv, stderr)
}

// enable is `pihole enable`: it kills any native re-enable child
// (killall -q pihole-reenable), sets BLOCKING_ENABLED=true and reloads the
// lists. It always exits 0 (V-13).
func (h *FakeHost) enable() {
	h.children = nil
	h.setBlocking(true)
	h.ftlEvent(EventReloadLists, io.Discard)
}

// disable is `pihole disable [Ns|Nm]`. With a duration it starts the nohup
// re-enable child in the calling unit's cgroup (V-13). It always exits 0.
func (h *FakeHost) disable(argv, args []string, stdout, stderr io.Writer) int {
	var d time.Duration
	switch len(args) {
	case 0:
	case 1:
		n, unit := args[0][:max(len(args[0])-1, 0)], args[0][max(len(args[0])-1, 0):]
		v, err := strconv.Atoi(n)
		if err != nil || v < 0 || (unit != "s" && unit != "m") {
			return unknown(argv, stderr)
		}
		d = time.Duration(v) * time.Second
		if unit == "m" {
			d = time.Duration(v) * time.Minute
		}
	default:
		return unknown(argv, stderr)
	}
	fmt.Fprintln(stdout, "  [i] Disabling blocking")
	if d > 0 {
		h.children = append(h.children, child{at: h.now.Add(d)})
	}
	h.setBlocking(false)
	h.ftlEvent(EventReloadLists, io.Discard)
	fmt.Fprintln(stdout, "  [✓] Pi-hole Disabled")
	return 0
}

// listTypes are list.sh's options and their domainlist.type.
var listTypes = map[string]int{"-w": 0, "-b": 1, "--white-regex": 2, "--regex": 3}

// listSh is list.sh AddDomain (V-14). It interpolates the value into SQL
// unescaped, so a value with ' breaks the statement and nothing is stored;
// a value present with another type has that row's type changed in place,
// without a reload; only a new value reloads. It always exits 0.
func (h *FakeHost) listSh(opt, value string, stdout io.Writer) int {
	typ := listTypes[opt]
	if strings.Contains(value, "'") {
		fmt.Fprintln(stdout, "Error: near \"b\": syntax error")
		return 0
	}
	reload := false
	h.withDB(func(db *sql.DB) error {
		var id int64
		var cur int
		err := db.QueryRow(`SELECT id, type FROM domainlist WHERE domain = ? ORDER BY id LIMIT 1`, value).Scan(&id, &cur)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := db.Exec(`INSERT INTO domainlist (type, domain) VALUES (?, ?)`, typ, value); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "  [i] Adding %s to the list\n", value)
			reload = true
			return nil
		case err != nil:
			return err
		case cur == typ:
			fmt.Fprintf(stdout, "  [i] %s already exists in the list, no need to add!\n", value)
			return nil
		}
		_, err = db.Exec(`UPDATE domainlist SET type = ? WHERE id = ?`, typ, id)
		return err
	})
	if reload {
		h.ftlEvent(EventReloadLists, io.Discard)
	}
	return 0
}

// adlist is `pihole -a adlist add|del` (webpage.sh CustomizeAdLists
// L555-577, V-9): a URL CheckUrl refuses is silently not stored, a missing
// URL is silently not deleted, FTL is not reloaded, and the exit code is
// always 0. A del is the bare `DELETE FROM adlist WHERE address = ...`
// under foreign keys: while gravity rows reference the list the constraint
// refuses it, sqlite3 prints the error, the row stays, and the command
// still exits 0 (WI 5.1 PROVENANCE "ADLIST REPLACEMENT"). A list with no
// gravity rows is deleted, and the schema's tr_adlist_delete trigger
// removes its group links.
func (h *FakeHost) adlist(verb, url, comment string, stderr io.Writer) {
	if !checkURL.MatchString(url) {
		return
	}
	h.withDB(func(db *sql.DB) error {
		if verb == "add" {
			_, err := db.Exec(`INSERT OR IGNORE INTO adlist (address, comment) VALUES (?, ?)`, url, comment)
			return err
		}
		_, err := db.Exec(`DELETE FROM adlist WHERE address = ?`, url)
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
			fmt.Fprintln(stderr, "Error: stepping, FOREIGN KEY constraint failed (19)")
			return nil
		}
		return err
	})
}

// gravityListDomains is how many domains each successful list holds.
const gravityListDomains = 3

// GravityDomain is the i-th (from 0) gravity domain `pihole -g` stores for
// the adlist with id: unique per list.
func GravityDomain(id int64, i int) string {
	return fmt.Sprintf("d%d.adlist%d.example", i, id)
}

// gravity is `pihole -g` (V-12): one progress block per adlist, in id
// order, status 1 (updated) or 4 (failed, no cache) for a URL given to
// FailAdlistDownload, info.updated = now in seconds and gravity_count. It
// replaces every gravity row: each list that downloads gets
// gravityListDomains rows (GravityDomain, adlist_id), a failed one none,
// so a processed list is referenced by gravity.adlist_id. It exits 0 even
// when lists fail.
func (h *FakeHost) gravity(stdout io.Writer) int {
	fmt.Fprintln(stdout, "  [i] Neutrino emissions detected...")
	h.withDB(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT id, address FROM adlist ORDER BY id`)
		if err != nil {
			return err
		}
		type list struct {
			id   int64
			addr string
		}
		var lists []list
		for rows.Next() {
			var l list
			if err := rows.Scan(&l.id, &l.addr); err != nil {
				rows.Close()
				return err
			}
			lists = append(lists, l)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if _, err := db.Exec(`DELETE FROM gravity`); err != nil {
			return err
		}
		total := 0
		for _, l := range lists {
			fmt.Fprintf(stdout, "  [i] Target: %s\n", l.addr)
			status, number := 1, gravityListDomains
			if h.failAdlists[l.addr] {
				status, number = 4, 0
				fmt.Fprintln(stdout, "  [✗] Status: Download failed")
				fmt.Fprintln(stdout, "  [✗] List download failed: no cached list available")
			} else {
				fmt.Fprintln(stdout, "  [✓] Status: Retrieval successful")
				fmt.Fprintf(stdout, "  [i] Received %d domains\n", number)
			}
			total += number
			for i := range number {
				if _, err := db.Exec(`INSERT INTO gravity (domain, adlist_id) VALUES (?, ?)`, GravityDomain(l.id, i), l.id); err != nil {
					return err
				}
			}
			if _, err := db.Exec(`UPDATE adlist SET status = ?, number = ?, date_updated = ? WHERE id = ?`,
				status, number, h.now.Unix(), l.id); err != nil {
				return err
			}
		}
		fmt.Fprintf(stdout, "  [i] Number of gravity domains: %d\n", total)
		for k, v := range map[string]int64{"updated": h.now.Unix(), "gravity_count": int64(total)} {
			if _, err := db.Exec(`INSERT OR REPLACE INTO info (property, value) VALUES (?, ?)`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
	fmt.Fprintln(stdout, "  [✓] Done.")
	return 0
}

// systemdRun is `systemd-run --unit=U --on-active=Ns [--timer-property=…]
// [--description=…] CMD…`: a transient timer U.timer that runs CMD once
// when the clock passes now+N. A loaded unit of that name fails, as
// systemd does.
func (h *FakeHost) systemdRun(args []string, stdout, stderr io.Writer) int {
	argv := append([]string{detach.SystemdRun}, args...)
	tm := &timer{}
	var onActive time.Duration
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "--"); i++ {
		key, val, _ := strings.Cut(args[i], "=")
		switch key {
		case "--unit":
			tm.unit = strings.TrimSuffix(val, ".service") + ".timer"
		case "--on-active":
			secs, ok := strings.CutSuffix(val, "s")
			n, err := strconv.Atoi(secs)
			if !ok || err != nil || n <= 0 {
				return unknown(argv, stderr)
			}
			onActive = time.Duration(n) * time.Second
		case "--description":
			tm.description = val
		case "--timer-property":
		default:
			return unknown(argv, stderr)
		}
	}
	tm.argv = slices.Clone(args[i:])
	if tm.unit == "" || onActive == 0 || len(tm.argv) == 0 || !filepath.IsAbs(tm.argv[0]) {
		return unknown(argv, stderr)
	}
	if h.timer(tm.unit) != nil {
		fmt.Fprintf(stderr, "Failed to start transient timer unit: Unit %s was already loaded or has a fragment file.\n", tm.unit)
		return 1
	}
	tm.at = h.now.Add(onActive)
	h.timers = append(h.timers, tm)
	fmt.Fprintf(stderr, "Running timer as unit: %s\n", tm.unit)
	return 0
}

// systemctl is `systemctl stop|show|reset-failed <unit>` for the fake's
// transient timers, and `systemctl reset-failed|show --property=Result` for
// FTLUnit.
func (h *FakeHost) systemctl(args []string, stdout, stderr io.Writer) int {
	argv := append([]string{detach.Systemctl}, args...)
	if len(args) < 2 {
		return unknown(argv, stderr)
	}
	unit := args[1]
	if unit == FTLUnit {
		return h.systemctlFTL(argv, args, stdout, stderr)
	}
	tm := h.timer(unit)
	switch {
	case args[0] == "stop" && len(args) == 2:
		if tm == nil {
			fmt.Fprintf(stderr, "Failed to stop %s: Unit %s not loaded.\n", unit, unit)
			return 5
		}
		h.timers = slices.DeleteFunc(h.timers, func(x *timer) bool { return x == tm })
		return 0
	case args[0] == "reset-failed" && len(args) == 2:
		if tm == nil {
			fmt.Fprintf(stderr, "Failed to reset failed state of unit %s: Unit %s not loaded.\n", unit, unit)
			return 1
		}
		return 0
	case args[0] == "show" && len(args) == 3 && args[2] == "--property=SubState,Description":
		if tm == nil {
			fmt.Fprintf(stdout, "SubState=dead\nDescription=%s\n", unit)
			return 0
		}
		fmt.Fprintf(stdout, "SubState=waiting\nDescription=%s\n", tm.description)
		return 0
	}
	return unknown(argv, stderr)
}

// systemctlFTL is systemctl for FTLUnit. reset-failed flushes the start
// rate limit counter and the failed result (systemctl(1) reset-failed);
// show --property=Result reads start-limit-hit after a refused start and
// success otherwise.
func (h *FakeHost) systemctlFTL(argv, args []string, stdout, stderr io.Writer) int {
	switch {
	case args[0] == "reset-failed" && len(args) == 2:
		h.limitBegin, h.limitNum, h.limitHit = time.Time{}, 0, false
		return 0
	case args[0] == "show" && len(args) == 3 && args[2] == "--property=Result":
		result := "success"
		if h.limitHit {
			result = "start-limit-hit"
		}
		fmt.Fprintf(stdout, "Result=%s\n", result)
		return 0
	}
	return unknown(argv, stderr)
}

// SeedDomain inserts a domainlist row directly and returns its id.
func (h *FakeHost) SeedDomain(typ int, value, comment string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var id int64
	h.withDB(func(db *sql.DB) error {
		res, err := db.Exec(`INSERT INTO domainlist (type, domain, comment) VALUES (?, ?, ?)`, typ, value, comment)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id
}

// DomainRows are the domainlist rows, by id.
func (h *FakeHost) DomainRows() []DomainRow {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []DomainRow
	h.withDB(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT id, type, domain, coalesce(comment, '') FROM domainlist ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r DomainRow
			if err := rows.Scan(&r.ID, &r.Type, &r.Domain, &r.Comment); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out
}

// Adlists are the adlist addresses, by id.
func (h *FakeHost) Adlists() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	h.withDB(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT address FROM adlist ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out
}

// AdlistStatus is the adlist's status, -1 when it is not there.
func (h *FakeHost) AdlistStatus(url string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	status := -1
	h.withDB(func(db *sql.DB) error {
		err := db.QueryRow(`SELECT status FROM adlist WHERE address = ?`, url).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return status
}

// AdlistRefs counts the gravity and adlist_by_group rows that reference
// the adlist with id.
func (h *FakeHost) AdlistRefs(id int64) (gravity, groups int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.withDB(func(db *sql.DB) error {
		return db.QueryRow(`SELECT (SELECT count(*) FROM gravity WHERE adlist_id = ?),
			(SELECT count(*) FROM adlist_by_group WHERE adlist_id = ?)`, id, id).Scan(&gravity, &groups)
	})
	return gravity, groups
}

// ForeignKeys is PRAGMA foreign_keys on the handle FakeHost's own commands
// use: 1 when they enforce foreign keys.
func (h *FakeHost) ForeignKeys() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	on := -1
	h.withDB(func(db *sql.DB) error {
		return db.QueryRow(`PRAGMA foreign_keys`).Scan(&on)
	})
	return on
}

// ForeignKeyViolations is the number of rows PRAGMA foreign_key_check
// reports for the whole database.
func (h *FakeHost) ForeignKeyViolations() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	h.withDB(func(db *sql.DB) error {
		rows, err := db.Query(`PRAGMA foreign_key_check`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			n++
		}
		return rows.Err()
	})
	return n
}

// ExecGravityDB runs one statement on gravity.db, foreign keys on, as a test
// hook: planting a table or re-inserting a row as a gravity swap would.
func (h *FakeHost) ExecGravityDB(query string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.withDB(func(db *sql.DB) error {
		_, err := db.Exec(query, args...)
		return err
	})
}

// FailAdlistDownload makes `pihole -g` fail to download url.
func (h *FakeHost) FailAdlistDownload(url string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failAdlists[url] = true
}

// GravityInfo is info.updated and info.gravity_count; zero when gravity has
// not run.
func (h *FakeHost) GravityInfo() (time.Time, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var updated time.Time
	var count int64
	h.withDB(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT property, value FROM info WHERE property IN ('updated', 'gravity_count')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var v int64
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			if k == "updated" {
				updated = time.Unix(v, 0).UTC()
			} else {
				count = v
			}
		}
		return rows.Err()
	})
	return updated, count
}

// QueryLogLines is the number of lines in the query log.
func (h *FakeHost) QueryLogLines() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, err := os.ReadFile(h.path(QueryLog))
	if err != nil {
		h.t.Fatalf("v5test: %v", err)
	}
	return bytes.Count(b, []byte("\n"))
}

// PlantGravityProcess makes a gravity run outside Nexus appear: a
// /proc/<pid>/cmdline naming /opt/pihole/gravity.sh, as cron's
// `pihole updateGravity` does (A-16). stop ends it.
func (h *FakeHost) PlantGravityProcess() (pid int, stop func()) {
	h.mu.Lock()
	h.nextPID++
	pid = h.nextPID
	h.mu.Unlock()
	dir := filepath.Join(v5.ProcDir, strconv.Itoa(pid))
	h.writeFile(filepath.Join(dir, "cmdline"), "/bin/bash\x00"+v5.GravityScript+"\x00", 0o444)
	h.writeFile(filepath.Join(dir, "comm"), "gravity.sh\n", 0o444)
	return pid, func() {
		if err := os.RemoveAll(h.path(dir)); err != nil {
			h.t.Errorf("v5test: %v", err)
		}
	}
}
