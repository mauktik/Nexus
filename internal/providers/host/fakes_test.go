package host

// The remaining Options seam fakes of planning/system-provider/test-strategy.md
// (Mocking Strategy, Fake Fidelity Contracts): F-RUN (newFakeRun), F-TICK
// (newFakeTickers), F-CLK (newFakeClock), F-ADDRS (fakeAddrs) and F-UNAME
// (fakeUname), with SP-SANDBOX's command part (spSandboxRun). WI 3.1 creates
// them; the WI 3.3 and 4.x tests use them. F-FS and F-STATFS are in
// fakefs_test.go.
//
// What F-RUN encodes from svcctl.Run (internal/svcctl/svcctl.go, Run):
//   - a non-zero exit is (Result{ExitCode, Output}, nil), never an error;
//   - an argv[0] that is not absolute is refused with an error wrapping
//     svcctl.ErrNotStarted, before anything runs;
//   - a program that cannot start is ErrNotStarted wrapping the exec
//     *fs.PathError ("%w: %w"), so errors.Is(err, fs.ErrNotExist) holds for
//     a missing binary;
//   - a hung command returns ctx.Err() once ctx is done (Run kills the
//     process group then).
//
// What F-TICK encodes from time.Ticker: C delivers nothing after Stop.
// Unlike time.Ticker, C is unbuffered and Fire is a rendezvous, so a tick
// is never dropped or delivered before the receiver reached its select; no
// test may depend on time.Ticker dropping ticks for a slow receiver.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/svcctl"
)

// The fixed argv the provider runs (test-strategy.md, F-RUN).
var (
	argvTimedatectl     = []string{"/usr/bin/timedatectl", "show", "--property=NTPSynchronized", "--value"}
	argvChronyc         = []string{"/usr/bin/chronyc", "-c", "tracking"}
	argvSystemctlFailed = []string{"/usr/bin/systemctl", "list-units", "--state=failed", "--plain", "--no-legend",
		"--no-pager", "--full"}
	argvRCStatusCrashed = []string{"/bin/rc-status", "--crashed"}
)

// runEntry is one F-RUN table row.
type runEntry struct {
	output   []byte
	exit     int
	startErr error // notStarted: the exec error Run wraps
	hang     bool
}

// fakeRun is F-RUN: a table from exact argv to a result, with a recorder.
type fakeRun struct {
	// errorf reports an unexpected argv; t.Errorf outside the fake's own
	// self-test.
	errorf func(format string, args ...any)

	mu    sync.Mutex
	table map[string]runEntry
	calls [][]string
}

func argvKey(argv []string) string { return strings.Join(argv, "\x00") }

// newFakeRun returns F-RUN with the capture's recorded commands. The
// captures ran the same queries with the commands' interactive spelling
// (PROVENANCE): `timedatectl show -p NTPSynchronized --value`,
// `systemctl --failed --plain --no-legend` and `rc-status --crashed`
// (exit 1, no output: nothing crashed). Neither capture has chronyc.
func newFakeRun(t *testing.T, capture string) *fakeRun {
	t.Helper()
	r := &fakeRun{errorf: t.Errorf, table: map[string]runEntry{}}
	tool := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", capture, "tools", name))
		if err != nil {
			t.Fatalf("F-RUN: %v", err)
		}
		return b
	}
	switch capture {
	case capUBU:
		r.set(argvTimedatectl, tool("timedatectl"), 0)
		r.set(argvSystemctlFailed, tool("systemctl-failed"), 0)
	case capALP:
		r.set(argvRCStatusCrashed, tool("rc-status-crashed"), 1)
	default:
		t.Fatalf("F-RUN: unknown capture %q", capture)
	}
	return r
}

// set answers argv with output and exit code.
func (r *fakeRun) set(argv []string, output []byte, exit int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.table[argvKey(argv)] = runEntry{output: output, exit: exit}
}

// notStarted makes argv fail to start with cause (syscall.ENOENT: the
// binary is missing; syscall.EACCES: it cannot be executed).
func (r *fakeRun) notStarted(argv []string, cause error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.table[argvKey(argv)] = runEntry{startErr: &fs.PathError{Op: "fork/exec", Path: argv[0], Err: cause}}
}

// hang makes argv run until ctx is done.
func (r *fakeRun) hang(argv []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.table[argvKey(argv)] = runEntry{hang: true}
}

// Run is the Options.Run seam.
func (r *fakeRun) Run(ctx context.Context, argv []string) (svcctl.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), argv...))
	e, ok := r.table[argvKey(argv)]
	r.mu.Unlock()

	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return svcctl.Result{}, fmt.Errorf("%w: refusing to run %q: the program must be an absolute path",
			svcctl.ErrNotStarted, argv)
	}
	if !ok {
		r.errorf("F-RUN: unexpected argv %q", argv)
		return svcctl.Result{}, fmt.Errorf("%w: %w", svcctl.ErrNotStarted,
			&fs.PathError{Op: "fork/exec", Path: argv[0], Err: syscall.ENOENT})
	}
	switch {
	case e.startErr != nil:
		return svcctl.Result{}, fmt.Errorf("%w: %w", svcctl.ErrNotStarted, e.startErr)
	case e.hang:
		<-ctx.Done()
		return svcctl.Result{}, ctx.Err()
	}
	return svcctl.Result{ExitCode: e.exit, Output: append([]byte(nil), e.output...)}, nil
}

// recorded returns every argv run, in order.
func (r *fakeRun) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// count returns how many times argv ran.
func (r *fakeRun) count(argv []string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if argvKey(c) == argvKey(argv) {
			n++
		}
	}
	return n
}

// spSandboxOutput is what SP-SANDBOX's timedatectl prints. It is a marker,
// not a real message: reasons never quote command output (NFR-6), and a
// test can search for it.
const spSandboxOutput = "SP-SANDBOX timedatectl output, never quoted\n"

// spSandboxRun applies SP-SANDBOX's command part: timedatectl exits 1.
func spSandboxRun(r *fakeRun) { r.set(argvTimedatectl, []byte(spSandboxOutput), 1) }

// fakeTickers is the Options.Ticker seam of F-TICK: it records every
// ticker made, with its interval.
type fakeTickers struct {
	t *testing.T

	mu    sync.Mutex
	made  []*fakeTicker
	every []time.Duration
}

func newFakeTickers(t *testing.T) *fakeTickers { return &fakeTickers{t: t} }

// New is the Options.Ticker seam.
func (k *fakeTickers) New(d time.Duration) Ticker {
	tk := &fakeTicker{t: k.t, c: make(chan time.Time)}
	k.mu.Lock()
	k.made = append(k.made, tk)
	k.every = append(k.every, d)
	k.mu.Unlock()
	return tk
}

// intervals returns the interval of every ticker made, in order.
func (k *fakeTickers) intervals() []time.Duration {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]time.Duration(nil), k.every...)
}

// nth waits up to 2 s for the n-th ticker made (1-based) and returns it.
func (k *fakeTickers) nth(n int) *fakeTicker {
	k.t.Helper()
	var tk *fakeTicker
	eventually(k.t, 2*time.Second, func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		if len(k.made) >= n {
			tk = k.made[n-1]
			return true
		}
		return false
	}, "ticker %d made", n)
	return tk
}

// fakeTicker is one F-TICK ticker.
type fakeTicker struct {
	t *testing.T
	c chan time.Time

	mu      sync.Mutex
	events  []string // "reset <d>" and "stop", in order
	stopped bool
	cCalls  int // C() calls: the sampler's loop calls it on every pass, once before each tick and once more after it
	fired   int // ticks Fire has delivered
}

func (tk *fakeTicker) C() <-chan time.Time {
	tk.mu.Lock()
	tk.cCalls++
	tk.mu.Unlock()
	return tk.c
}

// waitIdle waits up to 2 s for the receiver to be back in its select after
// every tick Fire has delivered. RunBackground evaluates C() once as it
// enters the loop and once per pass, so the receiver is idle when C() has
// been called fired+1 times: its sample, snapshot and history append for
// the last tick are all done, and nothing writes until the next Fire.
func (tk *fakeTicker) waitIdle() {
	tk.t.Helper()
	eventually(tk.t, 2*time.Second, func() bool {
		tk.mu.Lock()
		defer tk.mu.Unlock()
		return tk.cCalls >= tk.fired+1
	}, "receiver idle after the tick")
}

func (tk *fakeTicker) Reset(d time.Duration) {
	tk.mu.Lock()
	defer tk.mu.Unlock()
	tk.events = append(tk.events, "reset "+d.String())
	tk.stopped = false
}

func (tk *fakeTicker) Stop() {
	tk.mu.Lock()
	defer tk.mu.Unlock()
	tk.events = append(tk.events, "stop")
	tk.stopped = true
}

// Fire delivers one tick at now. It blocks until the receiver takes it and
// fails the test after 2 s, or at once on a stopped ticker.
func (tk *fakeTicker) Fire(now time.Time) {
	tk.t.Helper()
	tk.mu.Lock()
	stopped := tk.stopped
	tk.mu.Unlock()
	if stopped {
		tk.t.Fatalf("F-TICK: Fire on a stopped ticker")
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case tk.c <- now:
		tk.mu.Lock()
		tk.fired++
		tk.mu.Unlock()
	case <-timer.C:
		tk.t.Fatalf("F-TICK: no receiver took the tick within 2s")
	}
}

// recorded returns the Reset and Stop calls, in order.
func (tk *fakeTicker) recorded() []string {
	tk.mu.Lock()
	defer tk.mu.Unlock()
	return append([]string(nil), tk.events...)
}

// fakeClock is F-CLK: a settable Now. It fires nothing.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// newFakeClock starts at the captures' date (PROVENANCE).
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 15, 2, 37, 33, 0, time.UTC)}
}

// Now is the Options.Now seam.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// ipAddrJSON is the part of `ip -j addr` F-ADDRS reads.
type ipAddrJSON struct {
	IfName   string `json:"ifname"`
	AddrInfo []struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// fakeAddrs is F-ADDRS: the capture's tools/ip-addr as the Addrs seam, each
// interface's addresses in the kernel's order, lo included.
func fakeAddrs(t *testing.T, capture string) func() (map[string][]netip.Prefix, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", capture, "tools", "ip-addr"))
	if err != nil {
		t.Fatalf("F-ADDRS: %v", err)
	}
	var ifs []ipAddrJSON
	if err := json.Unmarshal(b, &ifs); err != nil {
		t.Fatalf("F-ADDRS: %v", err)
	}
	out := make(map[string][]netip.Prefix, len(ifs))
	for _, ifc := range ifs {
		prefixes := make([]netip.Prefix, 0, len(ifc.AddrInfo))
		for _, a := range ifc.AddrInfo {
			addr, err := netip.ParseAddr(a.Local)
			if err != nil {
				t.Fatalf("F-ADDRS: %s: %v", ifc.IfName, err)
			}
			prefixes = append(prefixes, netip.PrefixFrom(addr, a.PrefixLen))
		}
		out[ifc.IfName] = prefixes
	}
	return func() (map[string][]netip.Prefix, error) {
		cp := make(map[string][]netip.Prefix, len(out))
		for k, v := range out {
			cp[k] = append([]netip.Prefix(nil), v...)
		}
		return cp, nil
	}
}

// fakeUname is F-UNAME: the machine field of the capture's tools/uname-rm
// (`uname -rm`: release, then machine).
func fakeUname(t *testing.T, capture string) func() (string, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", capture, "tools", "uname-rm"))
	if err != nil {
		t.Fatalf("F-UNAME: %v", err)
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		t.Fatalf("F-UNAME: tools/uname-rm is %q, want release and machine", b)
	}
	return func() (string, error) { return f[1], nil }
}
