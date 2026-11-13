package v5test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

// Fault is an injectable FTL misbehavior (SetFault).
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
//     it passes (V-10). It never touches the running FTL (A-21).
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
	fault    Fault
	testExit int // 0: --test dies by SIGSEGV on a bad line
	restarts int
	flushes  int
	pidLag   time.Duration
	timers   []*time.Timer
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
		upstream: map[string]netip.Addr{}, cache: map[string]netip.Addr{},
	}
	f.rules, _ = f.snapshot()
	f.serv.Add(1)
	go f.serve()
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

// SetFault sets the fault for the following restarts, reloads and answers.
func (f *FakeFTL) SetFault(x Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fault = x
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
	f.removeProc(f.pid)
	f.reapStale()
	f.setState(ftlStopped)
}

// setState changes the state and wakes the queries waiting for a start.
// Callers hold mu.
func (f *FakeFTL) setState(s ftlState) {
	f.state = s
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
	if f.fault == FaultRestartFails {
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
	case f.fault == FaultNeverReady:
		f.reapStale()
		f.setState(ftlHung)
		return 0
	}
	f.rules = rules
	f.setState(ftlStarting)
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
		// SIGHUP clears the cache; the conf files are not re-read.
		f.flushes++
		f.cache = map[string]netip.Addr{}
		if f.fault == FaultNeverReady {
			f.setState(ftlHung)
		}
	}
	return true
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

// serve answers queries until close.
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
		if reply := f.answer(buf[:n]); reply != nil {
			_, _ = f.conn.WriteToUDPAddrPort(reply, from) // the client may have given up
		}
	}
}

// answer is the reply to query, waiting while FTL starts; nil when FTL is
// stopped or hung, or query is not a DNS query.
func (f *FakeFTL) answer(query []byte) []byte {
	for {
		f.mu.Lock()
		switch f.state {
		case ftlAnswering:
			defer f.mu.Unlock()
			return f.reply(query)
		case ftlStarting:
			wake := f.wake
			closed := f.closed
			f.mu.Unlock()
			if closed {
				return nil
			}
			<-wake
		default:
			f.mu.Unlock()
			return nil
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
	rcode, ips := f.resolve(canonical(q.Name.String()))
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
				if f.fault == FaultWrongAnswer {
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
