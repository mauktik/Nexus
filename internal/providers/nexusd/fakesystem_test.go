package nexusd

// F-SYS: the fake systemd behind the helper's system interface
// (test-strategy.md, Fake Fidelity Contracts). It runs on a virtual clock
// and a temp-root Paths. A "binary" is a file whose first line is
// `#!fake nexusd <version> <behaviour>`, the behaviour being one of ok,
// exit-at-start, crash-after=<d>, no-healthz or hang-ready.
//
// What it encodes, from deploy/nexusd.service, systemd.service(5),
// systemd.unit(5) and deploy/install.sh:
//   - Type=notify: a start leaves the unit activating until READY, the
//     binary's ready delay (300 ms); hang-ready never gets there.
//   - Restart is --no-block: it returns at once, and the start job runs on
//     the next clock tick, giving a new PID (monotonic, never reused).
//   - The fake install script's restart blocks, as `systemctl restart` does
//     under Type=notify: it returns after READY, or exit 1 once the start
//     fails.
//   - Restart=on-failure with RestartSec 100 ms: a process that exits
//     re-spawns, counting NRestarts when the re-spawn is scheduled; a manual
//     start that runs resets NRestarts. Starts are rate limited with
//     systemd's defaults (StartLimitBurst=5 in StartLimitIntervalSec=10s;
//     the unit sets neither) as systemd's ratelimit_below does: a fixed
//     window that begins at the first start and is replaced by a new one at
//     the first start more than 10 s later. A start over the burst is
//     refused: the unit is failed with MainPID 0, keeping a failed Result
//     (exit-code after a binary that exits at start; start-limit-hit only
//     when the Result was success), so a binary that exits at start ends
//     failed with Result=exit-code and NRestarts 5. Restart (--no-block)
//     still returns nil and its start is refused later; the blocking restart
//     exits 1. The limit lifts by itself once the window has passed, and
//     ResetFailed lifts it at once. Real systemd 259 shows each of these
//     (TC-SM-129, build/harness/logs/20261113T210341Z/test.log).
//   - ResetFailed of an inactive unit fails with "Unit ... not loaded":
//     systemd unloads a stopped unit nothing references (enabled marks one
//     that stays loaded).
//   - ExeVersion("/proc/<pid>/exe") is the image the PID started from, so it
//     stays right after a rename over Paths.Binary (A-17).
//   - RunInstall models install.sh: version check, rename the binary and the
//     unit into place, daemon-reload, and restart only an active service.
//
// starts counts the start jobs requested (Restart and the install script's
// restart), never the automatic re-spawns, which count NRestarts instead.
// The fake has no goroutines: its transitions are events on the virtual
// clock, run only inside Sleep or Advance, in time order.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// virtClock is F-CLK for the helper: virtual time that moves only when the
// code under test sleeps or the test advances it.
type virtClock struct {
	now    time.Time
	events []*virtEvent
	seq    int
}

type virtEvent struct {
	at  time.Time
	seq int
	f   func()
}

func newVirtClock() *virtClock {
	return &virtClock{now: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)}
}

func (c *virtClock) Now() time.Time { return c.now }

// Sleep advances virtual time by d, running every event it crosses.
func (c *virtClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.runUntil(c.now.Add(d))
	return ctx.Err()
}

// Advance moves virtual time by d from the test. With nothing pending it
// panics, so a test cannot advance before the code has scheduled anything.
func (c *virtClock) Advance(d time.Duration) {
	if len(c.events) == 0 {
		panic("advance with no waiter")
	}
	c.runUntil(c.now.Add(d))
}

// after schedules f at now+d.
func (c *virtClock) after(d time.Duration, f func()) {
	c.seq++
	c.events = append(c.events, &virtEvent{at: c.now.Add(d), seq: c.seq, f: f})
}

func (c *virtClock) runUntil(target time.Time) {
	for {
		i := -1
		for j, ev := range c.events {
			if ev.at.After(target) {
				continue
			}
			if i < 0 || ev.at.Before(c.events[i].at) || (ev.at.Equal(c.events[i].at) && ev.seq < c.events[i].seq) {
				i = j
			}
		}
		if i < 0 {
			break
		}
		ev := c.events[i]
		c.events = slices.Delete(c.events, i, i+1)
		if ev.at.After(c.now) {
			c.now = ev.at
		}
		ev.f()
	}
	if target.After(c.now) {
		c.now = target
	}
}

// The fake's systemd constants.
const (
	fakeReadyDelay    = 300 * time.Millisecond
	fakeRestartSec    = 100 * time.Millisecond // RestartSec default, systemd.service(5)
	fakeExitDelay     = 10 * time.Millisecond  // How long an exit-at-start binary runs
	fakeStartBurst    = 5                      // DefaultStartLimitBurst
	fakeStartInterval = 10 * time.Second       // DefaultStartLimitIntervalSec
	fakeStartTimeout  = 90 * time.Second       // DefaultTimeoutStartSec, for the blocking restart
)

// fakeBinary is a parsed `#!fake nexusd <version> <behaviour>` file.
type fakeBinary struct {
	version    string
	behaviour  string
	crashAfter time.Duration
}

// writeFakeBinary writes a fake binary at path through a rename, so the
// path gets a new inode, as install.sh's mv does.
func writeFakeBinary(t *testing.T, path, version, behaviour string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".fake-tmp"
	if err := os.WriteFile(tmp, []byte("#!fake nexusd "+version+" "+behaviour+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func readFakeBinary(path string) (fakeBinary, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return fakeBinary{}, err
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(line))
	if len(f) != 4 || f[0] != "#!fake" || f[1] != "nexusd" {
		return fakeBinary{}, fmt.Errorf("%s: exec format error", path)
	}
	bin := fakeBinary{version: f[2], behaviour: f[3]}
	if d, ok := strings.CutPrefix(bin.behaviour, "crash-after="); ok {
		bin.crashAfter, err = time.ParseDuration(d)
		if err != nil {
			return fakeBinary{}, fmt.Errorf("%s: %w", path, err)
		}
		bin.behaviour = "crash-after"
	}
	switch bin.behaviour {
	case "ok", "exit-at-start", "crash-after", "no-healthz", "hang-ready":
		return bin, nil
	}
	return fakeBinary{}, fmt.Errorf("%s: unknown fake behaviour %q", path, bin.behaviour)
}

type fakeProc struct {
	pid   int
	bin   fakeBinary
	alive bool
}

// fakeCall is one recorded call into the fake.
type fakeCall struct {
	at   time.Time
	name string
	arg  string
}

// fakeInstall are the options of the fake install script.
type fakeInstall struct {
	exitBefore      int    // Exit with this code before changing anything
	exitAfterRename int    // Exit with this code after the binary rename (the v9.9.3 analogue)
	noRestart       bool   // Never restart the service
	hang            bool   // Block until ctx ends
	installAlt      string // Install a binary reporting this version (the v9.9.5 analogue)
}

type fakeSystem struct {
	*virtClock
	paths Paths

	active, sub, result string
	mainPID             int
	nRestarts           int
	nextPID             int
	procs               map[int]*fakeProc
	gen                 int // Bumped by every start and stop: a stale auto-restart does nothing

	starts  int       // Start jobs requested: Restart and the install script's restart
	rlBegin time.Time // The start limit's window; zero: none yet
	rlNum   int       // Starts counted in the window, refused ones too
	enabled bool      // The unit stays loaded while inactive

	calls []fakeCall

	healthz func(ctx context.Context, t healthTarget) error // Nil: the fake's own
	install fakeInstall
}

var _ system = (*fakeSystem)(nil)

// newFakeSystem returns an inactive unit under a temp root whose first
// process will get firstPID.
func newFakeSystem(t *testing.T, firstPID int) *fakeSystem {
	t.Helper()
	root := t.TempDir()
	return &fakeSystem{
		virtClock: newVirtClock(),
		paths: Paths{
			Binary:   filepath.Join(root, "usr/local/bin/nexusd"),
			Prev:     filepath.Join(root, "usr/local/bin/nexusd.prev"),
			UnitFile: filepath.Join(root, "etc/systemd/system/nexusd.service"),
			Unit:     "nexusd.service",
		},
		active: "inactive", sub: "dead", result: "success",
		nextPID: firstPID,
		procs:   map[int]*fakeProc{},
	}
}

// bootStart installs a binary and starts the unit as at boot (not counted
// in starts), without letting time pass.
func (f *fakeSystem) bootStart(t *testing.T, version, behaviour string) {
	t.Helper()
	writeFakeBinary(t, f.paths.Binary, version, behaviour)
	f.start(true)
}

// reachStartLimit puts the unit at its start limit as a binary that exits
// at start does: Paths.Binary becomes such a binary, the unit is
// restarted, and a second passes.
func (f *fakeSystem) reachStartLimit(t *testing.T) {
	t.Helper()
	writeFakeBinary(t, f.paths.Binary, "v0.0.0", "exit-at-start")
	if err := f.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.runUntil(f.now.Add(time.Second))
	if f.active != "failed" {
		t.Fatalf("fake unit did not reach its start limit: %s/%s", f.active, f.sub)
	}
}

// bootActive is bootStart, then waits out the ready delay.
func (f *fakeSystem) bootActive(t *testing.T, version, behaviour string) {
	t.Helper()
	f.bootStart(t, version, behaviour)
	f.runUntil(f.now.Add(fakeReadyDelay))
	if f.active != "active" {
		t.Fatalf("fake unit did not become active: %s/%s", f.active, f.sub)
	}
}

func (f *fakeSystem) record(name, arg string) {
	f.calls = append(f.calls, fakeCall{at: f.Now(), name: name, arg: arg})
}

// callsOf returns the recorded calls named name.
func (f *fakeSystem) callsOf(name string) []fakeCall {
	var out []fakeCall
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// startAllowed counts one start against the start limit, as systemd's
// ratelimit_below: a start more than the interval after the window began
// opens a new window.
func (f *fakeSystem) startAllowed() bool {
	if f.rlBegin.IsZero() || f.Now().Sub(f.rlBegin) > fakeStartInterval {
		f.rlBegin, f.rlNum = f.Now(), 1
		return true
	}
	f.rlNum++
	return f.rlNum <= fakeStartBurst
}

// refuseStart is a start over the limit: the unit fails, keeping a failed
// Result.
func (f *fakeSystem) refuseStart() {
	f.mainPID = 0
	f.active, f.sub = "failed", "failed"
	if f.result == "success" {
		f.result = "start-limit-hit"
	}
}

// stopMain stops the main process cleanly, as a restart's stop half does.
func (f *fakeSystem) stopMain() {
	f.gen++
	if p := f.procs[f.mainPID]; p != nil {
		p.alive = false
	}
	f.mainPID = 0
}

// start runs one start attempt of the unit from the file at Paths.Binary;
// manual is a start job rather than an automatic re-spawn.
func (f *fakeSystem) start(manual bool) {
	f.gen++
	if !f.startAllowed() {
		f.refuseStart()
		return
	}
	if manual {
		f.nRestarts = 0
	}
	bin, err := readFakeBinary(f.paths.Binary)
	if err != nil {
		bin = fakeBinary{behaviour: "exit-at-start"} // 203/EXEC: fails like an exit at start
	}
	p := &fakeProc{pid: f.nextPID, bin: bin, alive: true}
	f.nextPID++
	f.procs[p.pid] = p
	f.mainPID = p.pid
	f.active, f.sub, f.result = "activating", "start", "success"
	switch bin.behaviour {
	case "exit-at-start":
		f.after(fakeExitDelay, func() { f.exit(p) })
	case "hang-ready":
	default:
		f.after(fakeReadyDelay, func() {
			if p.alive && f.mainPID == p.pid && f.active == "activating" {
				f.active, f.sub = "active", "running"
			}
		})
		if bin.behaviour == "crash-after" {
			f.after(bin.crashAfter, func() { f.exit(p) })
		}
	}
}

// exit is a process ending with a non-zero code: Restart=on-failure
// re-spawns it after RestartSec unless the start limit refuses it.
func (f *fakeSystem) exit(p *fakeProc) {
	if !p.alive {
		return
	}
	p.alive = false
	if f.mainPID != p.pid {
		return
	}
	f.mainPID = 0
	f.active, f.sub, f.result = "activating", "auto-restart", "exit-code"
	gen := f.gen
	f.after(fakeRestartSec, func() {
		if f.gen != gen {
			return
		}
		f.nRestarts++
		f.start(false)
	})
}

func (f *fakeSystem) Show(context.Context) (unitState, error) {
	f.record("Show", fmt.Sprintf("MainPID=%d ActiveState=%s", f.mainPID, f.active))
	return unitState{ActiveState: f.active, SubState: f.sub, Result: f.result, MainPID: f.mainPID,
		NRestarts: f.nRestarts}, nil
}

// Restart only enqueues the job, so it returns nil even at the start
// limit; the start is refused when the job runs.
func (f *fakeSystem) Restart(context.Context) error {
	f.record("Restart", "--no-block")
	f.starts++
	f.after(0, func() {
		f.stopMain()
		f.start(true)
	})
	return nil
}

func (f *fakeSystem) DaemonReload(context.Context) error {
	f.record("DaemonReload", "")
	return nil
}

func (f *fakeSystem) ResetFailed(context.Context) error {
	f.record("ResetFailed", "")
	if f.active == "inactive" && !f.enabled {
		return fmt.Errorf("systemctl reset-failed %[1]s: exit status 1: Failed to reset failed state of unit "+
			"%[1]s: Unit %[1]s not loaded.", f.paths.Unit)
	}
	f.rlBegin, f.rlNum = time.Time{}, 0
	if f.active == "failed" {
		f.active, f.sub = "inactive", "dead"
	}
	f.result = "success"
	f.nRestarts = 0
	return nil
}

func (f *fakeSystem) ExeVersion(_ context.Context, path string) (string, error) {
	f.record("ExeVersion", path)
	if rest, ok := strings.CutPrefix(path, "/proc/"); ok {
		if pidText, ok := strings.CutSuffix(rest, "/exe"); ok {
			pid, err := strconv.Atoi(pidText)
			p := f.procs[pid]
			if err != nil || p == nil || !p.alive {
				return "", fmt.Errorf("%s: no such file or directory", path)
			}
			return p.bin.version, nil
		}
	}
	bin, err := readFakeBinary(path)
	if err != nil {
		return "", err
	}
	return bin.version, nil
}

func (f *fakeSystem) Healthz(ctx context.Context, t healthTarget) error {
	f.record("Healthz", t.URL)
	if f.healthz != nil {
		return f.healthz(ctx, t)
	}
	if p := f.procs[f.mainPID]; p != nil && p.alive && f.active == "active" &&
		(p.bin.behaviour == "ok" || p.bin.behaviour == "crash-after") {
		return nil
	}
	host := t.URL
	if u, err := url.Parse(t.URL); err == nil {
		host = u.Host
	}
	return fmt.Errorf("dial tcp %s: connect: connection refused", host)
}

// RunInstall is the scripted model of deploy/install.sh.
func (f *fakeSystem) RunInstall(ctx context.Context, script, dir string, out io.Writer) (int, error) {
	f.record("RunInstall", script)
	say := func(format string, args ...any) { fmt.Fprintf(out, "install.sh: "+format+"\n", args...) }
	in := f.install
	if in.hang {
		<-ctx.Done()
		return -1, fmt.Errorf("%s: %w", script, ctx.Err())
	}
	if in.exitBefore != 0 {
		say("failing before any change")
		return in.exitBefore, nil
	}
	src := filepath.Join(dir, "nexusd")
	bin, err := readFakeBinary(src)
	if err != nil {
		say("%s does not run on this host: %v", src, err)
		return 1, nil
	}
	if in.installAlt != "" {
		b, err := os.ReadFile(src)
		if err != nil {
			return 1, nil
		}
		alt := strings.Replace(string(b), " "+bin.version+" ", " "+in.installAlt+" ", 1)
		if err := os.WriteFile(src, []byte(alt), 0o755); err != nil {
			return 1, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(f.paths.Binary), 0o755); err != nil {
		return 1, nil
	}
	if err := os.Rename(src, f.paths.Binary); err != nil {
		say("mv: %v", err)
		return 1, nil
	}
	if in.exitAfterRename != 0 {
		say("failing after the binary rename")
		return in.exitAfterRename, nil
	}
	if unit := filepath.Join(dir, "nexusd.service"); fileExists(unit) {
		if err := os.MkdirAll(filepath.Dir(f.paths.UnitFile), 0o755); err != nil {
			return 1, nil
		}
		if err := os.Rename(unit, f.paths.UnitFile); err != nil {
			say("mv: %v", err)
			return 1, nil
		}
	}
	say("installed nexusd %s", bin.version)
	f.record("DaemonReload", "install.sh")
	if in.noRestart || f.active != "active" {
		return 0, nil
	}
	return f.blockingRestart(ctx, say), nil
}

// blockingRestart is `systemctl restart nexusd.service` under Type=notify:
// it returns 0 after READY and 1 once the start fails or is refused at the
// start limit.
func (f *fakeSystem) blockingRestart(ctx context.Context, say func(string, ...any)) int {
	f.record("Restart", "blocking")
	f.starts++
	f.stopMain()
	f.start(true)
	pid := f.mainPID
	if pid == 0 {
		say("Job for %s failed (Result=%s).", f.paths.Unit, f.result)
		return 1
	}
	deadline := f.Now().Add(fakeStartTimeout)
	for {
		switch {
		case f.mainPID == pid && f.active == "active":
			say("restarted the running nexusd.service onto the new binary")
			return 0
		case !f.procs[pid].alive:
			say("Job for %s failed because the control process exited with error code.", f.paths.Unit)
			return 1
		case !f.Now().Before(deadline):
			say("Job for %s failed because a timeout was exceeded.", f.paths.Unit)
			return 1
		}
		if err := f.Sleep(ctx, 10*time.Millisecond); err != nil {
			return 1
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TC-SM-63 (WI 4.1; NFR-6): The fake system honours its fidelity contract
func TestTCSM63_TheFakeSystemHonoursItsFidelity(t *testing.T) {
	ctx := context.Background()

	// Step 1: --no-block restart of an active unit.
	f := newFakeSystem(t, 200)
	f.bootActive(t, "v9.9.2", "ok")
	if err := f.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Show(ctx); st.MainPID != 200 || st.ActiveState != "active" {
		t.Fatalf("right after Restart: %+v, want the same MainPID 200, active", st)
	}
	if err := f.Sleep(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Show(ctx); st.MainPID != 201 || st.ActiveState != "activating" {
		t.Fatalf("next tick: %+v, want a new PID 201 activating", st)
	}
	if err := f.Sleep(ctx, fakeReadyDelay); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Show(ctx); st.MainPID != 201 || st.ActiveState != "active" || st.SubState != "running" {
		t.Fatalf("after 300ms: %+v, want PID 201 active/running", st)
	}
	if f.starts != 1 {
		t.Errorf("starts = %d, want 1", f.starts)
	}

	// Step 2: install.sh restarts only an active service.
	f = newFakeSystem(t, 200)
	pkg := t.TempDir()
	writeFakeBinary(t, filepath.Join(pkg, "nexusd"), "v9.9.2", "ok")
	var out bytes.Buffer
	code, err := f.RunInstall(ctx, filepath.Join(pkg, "install.sh"), pkg, &out)
	if err != nil || code != 0 {
		t.Fatalf("RunInstall = %d, %v\n%s", code, err, out.String())
	}
	if f.starts != 0 || f.mainPID != 0 {
		t.Errorf("inactive unit: starts = %d, MainPID = %d, want 0 and 0", f.starts, f.mainPID)
	}
	// ... and an active one is restarted, blocking until READY.
	f.bootActive(t, "v9.9.1", "ok")
	writeFakeBinary(t, filepath.Join(pkg, "nexusd"), "v9.9.2", "ok")
	if code, err := f.RunInstall(ctx, filepath.Join(pkg, "install.sh"), pkg, &out); err != nil || code != 0 {
		t.Fatalf("RunInstall on an active unit = %d, %v\n%s", code, err, out.String())
	}
	if f.starts != 1 || f.active != "active" || f.procs[f.mainPID].bin.version != "v9.9.2" {
		t.Errorf("active unit: starts = %d, %s, running %+v; want 1 start, active on v9.9.2", f.starts, f.active,
			f.procs[f.mainPID].bin)
	}

	// Step 3: a binary that exits at start reaches the start limit: the
	// explicit start and four re-spawns run, the fifth re-spawn is refused,
	// and the unit ends failed with the last start's Result=exit-code and
	// NRestarts 5, as systemd 259 does (TC-SM-129).
	f = newFakeSystem(t, 200)
	f.bootActive(t, "v9.9.1", "ok")
	// The boot start is older than the 10s start-limit interval.
	if err := f.Sleep(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	writeFakeBinary(t, f.paths.Binary, "v9.9.2", "exit-at-start")
	if err := f.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	f.Advance(time.Second)
	st, _ := f.Show(ctx)
	if st.Result != "exit-code" || st.ActiveState != "failed" || st.MainPID != 0 || st.NRestarts != 5 {
		t.Errorf("after 1s: %+v, want failed, exit-code, MainPID 0, NRestarts 5", st)
	}
	if f.starts != 1 {
		t.Errorf("starts = %d, want 1 (only the explicit start)", f.starts)
	}

	// Step 4: within the interval a --no-block restart is accepted, and its
	// start is refused later; the unit stays failed.
	if err := f.Restart(ctx); err != nil {
		t.Errorf("Restart at the start limit = %v, want nil (--no-block only enqueues)", err)
	}
	if err := f.Sleep(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Show(ctx); st.ActiveState != "failed" || st.Result != "exit-code" || st.MainPID != 0 ||
		st.NRestarts != 5 {
		t.Errorf("after a restart at the start limit: %+v, want failed, exit-code, MainPID 0, NRestarts 5", st)
	}

	// Step 5: once StartLimitIntervalSec has passed since the window began,
	// a start runs again without ResetFailed, and a manual start resets
	// NRestarts.
	if err := f.Sleep(ctx, fakeStartInterval); err != nil {
		t.Fatal(err)
	}
	pid := f.nextPID
	if err := f.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.Sleep(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Show(ctx); st.ActiveState != "activating" || st.MainPID != pid || st.NRestarts != 0 {
		t.Errorf("a restart after the interval: %+v, want activating, MainPID %d, NRestarts 0", st, pid)
	}

	// Step 6: ResetFailed clears the limit at once.
	f.Advance(time.Second)
	if st, _ := f.Show(ctx); st.ActiveState != "failed" {
		t.Fatalf("the second burst: %+v, want failed at the start limit", st)
	}
	if err := f.ResetFailed(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Show(ctx); st.ActiveState != "inactive" || st.SubState != "dead" || st.Result != "success" ||
		st.NRestarts != 0 {
		t.Errorf("after ResetFailed: %+v, want inactive/dead, success, NRestarts 0", st)
	}
	pid = f.nextPID
	if err := f.Restart(ctx); err != nil {
		t.Errorf("Restart after ResetFailed = %v, want nil", err)
	}
	if err := f.Sleep(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Show(ctx); st.ActiveState != "activating" || st.MainPID != pid {
		t.Errorf("a restart after ResetFailed: %+v, want activating, MainPID %d", st, pid)
	}

	// Step 7: reset-failed of an inactive unit that systemd has unloaded
	// fails.
	f = newFakeSystem(t, 200)
	if err := f.ResetFailed(ctx); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("ResetFailed of an unloaded unit = %v, want Unit nexusd.service not loaded", err)
	}

	// Step 8: /proc/<pid>/exe is the image the PID started from.
	f = newFakeSystem(t, 200)
	f.bootActive(t, "v9.9.1", "ok")
	writeFakeBinary(t, f.paths.Binary, "v9.9.2", "ok")
	if v, err := f.ExeVersion(ctx, "/proc/200/exe"); err != nil || v != "v9.9.1" {
		t.Errorf("ExeVersion(/proc/200/exe) = %q, %v; want the old image's v9.9.1", v, err)
	}
	if v, err := f.ExeVersion(ctx, f.paths.Binary); err != nil || v != "v9.9.2" {
		t.Errorf("ExeVersion(binary) = %q, %v; want v9.9.2", v, err)
	}

	// Step 9: Advance with no waiter panics.
	func() {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "advance with no waiter") {
				t.Errorf("Advance with nothing pending: recovered %v, want a panic with advance with no waiter", r)
			}
		}()
		newVirtClock().Advance(time.Second)
	}()
}
