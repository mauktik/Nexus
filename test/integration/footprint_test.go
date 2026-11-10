//go:build integration

package integration

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	aptspec "github.com/mauktik/nexus/internal/providers/apt/spec"
	dockerspec "github.com/mauktik/nexus/internal/providers/docker/spec"
	scriptspec "github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// NFR measurements of WI 10.3 (TC-187 to TC-192) in the T1 harness: a real
// nexusd under the shipped unit, all three providers enabled, and 50 real
// containers on the nested Engine. The numbers are logged as "NFR ..."
// lines and recorded in docs/verification/measurements.md. T1 is arm64;
// the real x64 host repeats them at 11.1.

const (
	nfrContainerCount = 50
	nfrLabel          = "nexus.nfr=1"
	nfrRSSLimit       = 50 << 20
	nfrCPULimit       = 0.01 // Of one core
	nfrReadyLimit     = 2 * time.Second
	nfrStartRuns      = 10
	nfrSettle         = 30 * time.Second
	dockerDaemonJSON  = "/etc/docker/daemon.json"
)

// nfrIdle is the idle window; anything shorter than the NFR-1 five minutes
// is a dry run and says so.
var nfrIdle = flag.Duration("nfr.idle", 5*time.Minute, "TC-187/TC-188 idle window")

// nfrContainerNames are the 50 containers, nfr-00 to nfr-49.
func nfrContainerNames() []string {
	names := make([]string, nfrContainerCount)
	for i := range names {
		names[i] = fmt.Sprintf("nfr-%02d", i)
	}
	return names
}

// nfrContainers runs the 50 small containers (busybox sleep), removed again
// at cleanup, and returns their names.
func nfrContainers(t *testing.T) []string {
	t.Helper()
	names := nfrContainerNames()
	requireDocker(t, names...)
	var wg sync.WaitGroup
	errs := make(chan error, len(names))
	sem := make(chan struct{}, 8)
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if out, err := sh(2*time.Minute, "docker", "run", "-d", "--name", name, "--label", nfrLabel,
				testImage, "sleep", "86400"); err != nil {
				errs <- fmt.Errorf("docker run %s: %v\n%s", name, err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	running := strings.Fields(dockerCLI(t, "ps", "-q", "--filter", "label="+nfrLabel, "--filter", "status=running"))
	all := strings.Fields(dockerCLI(t, "ps", "-aq"))
	if len(running) != nfrContainerCount {
		t.Fatalf("%d labeled containers running, want %d", len(running), nfrContainerCount)
	}
	t.Logf("NFR env: %d nfr containers running, %d containers on the Engine in all; nested Engine %s",
		len(running), len(all), dockerCLI(t, "version", "--format", "{{.Server.Version}} (API {{.Server.APIVersion}}, {{.Server.Os}}/{{.Server.Arch}})"))
	return names
}

// nfrDaemon is the measured nexusd: docker, apt and scripts enabled, and a
// token holding every scope the measurements use.
type nfrDaemon struct {
	*nexusdInstance
	name     string
	stateDir string
	auditDir string
}

var nfrScopes = []string{
	dockerspec.ReadScope, dockerspec.WriteScope,
	aptspec.ReadScope, aptspec.WriteScope, aptspec.InstallScope,
	scriptspec.ReadScope, scriptspec.RunScope, "audit:read",
}

// nfrSetup installs the apt fixture (nxplain 1.0 installed, 2.0
// upgradable), the scripts ok and fail, and starts nexusd with all
// providers; socket is the Docker socket it is given, aptSettings more
// lines of the apt section.
func nfrSetup(t *testing.T, name, socket, aptSettings string) *nfrDaemon {
	t.Helper()
	aptFixture(t, true)
	return nfrStart(t, name, socket, aptSettings)
}

// nfrStart is nfrSetup on an apt fixture already in place.
func nfrStart(t *testing.T, name, socket, aptSettings string) *nfrDaemon {
	t.Helper()
	dir := testDir(t, name)
	ok := installFixture(t, dir, "ok.sh")
	fail := installFixture(t, dir, "fail.sh")
	providers := dockerSection(socket) +
		"  apt:\n    enabled: true\n" + aptSettings +
		"  scripts:\n    enabled: true\n    catalog:\n" + catalogYAML(entry{id: "ok", path: ok}, entry{id: "fail", path: fail})
	n := newNexusd(t, name, providers)
	d := &nfrDaemon{nexusdInstance: n, name: name,
		stateDir: filepath.Join(nexusdState, "nexusd-"+name), auditDir: filepath.Join("/var/log/nexus", "nexusd-"+name)}
	tok, _, err := auth.Create(d.stateDir, auth.CreateRequest{Name: "nfr", Scopes: nfrScopes})
	if err != nil {
		t.Fatal(err)
	}
	d.token = tok
	return d
}

// timed is one request with its wall time, the body read to the end.
func (n *nexusdInstance) timed(method, path, body string) (time.Duration, int, []byte) {
	n.t.Helper()
	req, err := http.NewRequest(method, "http://"+n.listen+path, strings.NewReader(body))
	if err != nil {
		n.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := n.client.Do(req)
	if err != nil {
		n.t.Fatalf("%s %s: %v", method, path, err)
	}
	b, err := io.ReadAll(resp.Body)
	d := time.Since(start)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		n.t.Fatalf("%s %s: %v", method, path, err)
	}
	return d, resp.StatusCode, b
}

// nfrStats summarizes samples with the nearest-rank p95 (TC-189).
type nfrStats struct {
	n                   int
	p50, p95, max, mean time.Duration
}

func nfrSummarize(samples []time.Duration) nfrStats {
	s := slices.Clone(samples)
	slices.Sort(s)
	if len(s) == 0 {
		return nfrStats{}
	}
	rank := func(p float64) time.Duration { return s[int(math.Ceil(p*float64(len(s))))-1] }
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	return nfrStats{n: len(s), p50: rank(0.50), p95: rank(0.95), max: s[len(s)-1], mean: sum / time.Duration(len(s))}
}

func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64)
}

func (s nfrStats) String() string {
	return fmt.Sprintf("n=%d p50=%sms p95=%sms max=%sms mean=%sms", s.n, ms(s.p50), ms(s.p95), ms(s.max), ms(s.mean))
}

// nfrProc is one reading of a process's /proc/<pid>/status and stat.
type nfrProc struct {
	rss, hwm  int64 // Bytes
	threads   int
	ticks     int64 // utime+stime
	cminflt   int64 // Minor faults of waited-for children: grows with every child reaped
	cticks    int64 // cutime+cstime
	fds       int
	wallClock time.Time
}

func nfrReadProc(pid int) (nfrProc, error) {
	p := nfrProc{wallClock: time.Now()}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return p, err
	}
	for line := range strings.Lines(string(status)) {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "VmRSS":
			p.rss = n << 10
		case "VmHWM":
			p.hwm = n << 10
		case "Threads":
			p.threads = int(n)
		}
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return p, err
	}
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return p, fmt.Errorf("/proc/%d/stat: %q", pid, s)
	}
	// Field N of proc(5) is f[N-3] after the command name.
	f := strings.Fields(s[i+1:])
	if len(f) < 15 {
		return p, fmt.Errorf("/proc/%d/stat: %d fields", pid, len(f))
	}
	num := func(n int) int64 {
		v, _ := strconv.ParseInt(f[n-3], 10, 64)
		return v
	}
	p.cminflt = num(11)
	p.ticks = num(14) + num(15)
	p.cticks = num(16) + num(17)
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return p, err
	}
	p.fds = len(fds)
	return p, nil
}

// nfrCgroup is the unit's cgroup v2 directory. The harness runs with
// --cgroupns=host, so /proc/<pid>/cgroup is a path under /sys/fs/cgroup.
func nfrCgroup(t *testing.T, pid int) string {
	t.Helper()
	cg := pidCgroup(t, pid)
	_, path, ok := strings.Cut(cg, "::")
	if !ok {
		t.Fatalf("cgroup of %d: %q is not cgroup v2", pid, cg)
	}
	dir := filepath.Join("/sys/fs/cgroup", path)
	if !exists(filepath.Join(dir, "cpu.stat")) {
		t.Fatalf("%s has no cpu.stat", dir)
	}
	return dir
}

// nfrCgroupUsage is cpu.stat usage_usec of a cgroup.
func nfrCgroupUsage(t *testing.T, dir string) time.Duration {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "usage_usec "); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return time.Duration(n) * time.Microsecond
		}
	}
	t.Fatalf("%s/cpu.stat has no usage_usec", dir)
	return 0
}

// nfrCgroupProcs lists the pids in a cgroup and its children.
func nfrCgroupProcs(dir string) []int {
	var pids []int
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				pids = append(pids, pid)
			}
		}
		return nil
	})
	return pids
}

// aptComms are the process names of apt and dpkg binaries (comm is cut to
// 15 characters).
var aptComms = []string{"apt", "apt-get", "apt-cache", "apt-config", "apt-mark", "dpkg", "dpkg-query", "dpkg-deb",
	"dpkg-divert", "dpkg-split", "dpkg-trigger", "dpkg-maintscrip", "unattended-upgr", "http", "file", "store", "gpgv"}

// nfrAptProcs returns "pid comm (ppid P)" of every process now running an
// apt or dpkg binary, across the container's pid namespace.
func nfrAptProcs() []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(b))
		if !slices.Contains(aptComms, comm) {
			continue
		}
		exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		// apt's methods are named http, file, store: count only real apt
		// paths for those generic names.
		if (comm == "http" || comm == "file" || comm == "store" || comm == "gpgv") && !strings.Contains(exe, "/apt/") &&
			!strings.HasSuffix(exe, "/gpgv") {
			continue
		}
		ppid := ""
		if st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			s := string(st)
			if i := strings.LastIndexByte(s, ')'); i >= 0 {
				if f := strings.Fields(s[i+1:]); len(f) > 1 {
					ppid = f[1]
				}
			}
		}
		out = append(out, fmt.Sprintf("%d %s %s (ppid %s)", pid, comm, exe, ppid))
	}
	return out
}

// dockerDebug turns on the nested Engine's debug log, so each API request
// it serves is a debug line in docker.service's journal (Engine 29:
// msg="handling GET request" module=api request-url=...; older Engines:
// msg="Calling GET ..."), and restores daemon.json at cleanup. debug is a reloadable
// option: SIGHUP (systemctl reload) applies it without a restart, so the
// running containers are untouched.
func dockerDebug(t *testing.T) {
	t.Helper()
	orig, had := readFile(t, dockerDaemonJSON)
	cfg := map[string]any{}
	if had && len(strings.TrimSpace(string(orig))) > 0 {
		if err := json.Unmarshal(orig, &cfg); err != nil {
			t.Fatalf("%s: %v", dockerDaemonJSON, err)
		}
	}
	cfg["debug"] = true
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dockerDaemonJSON), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dockerDaemonJSON, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			err = os.WriteFile(dockerDaemonJSON, orig, 0o644)
		} else {
			err = os.Remove(dockerDaemonJSON)
		}
		if err != nil {
			t.Error(err)
		}
		if out, err := sh(time.Minute, "systemctl", "reload", "docker.service"); err != nil {
			t.Errorf("reload docker: %v\n%s", err, out)
		}
	})
	mustSh(t, "systemctl", "reload", "docker.service")
	waitUntil(t, 30*time.Second, "the Engine in debug mode", func() (bool, string) {
		out, err := sh(10*time.Second, "docker", "info", "--format", "{{.Debug}}")
		return err == nil && strings.TrimSpace(out) == "true", out
	})
}

// dockerCalls returns the Engine's API request debug lines between since
// and until.
func dockerCalls(t *testing.T, since, until time.Time) []string {
	t.Helper()
	out := mustSh(t, "journalctl", "-u", "docker.service", "--no-pager", "-o", "cat",
		"--since", "@"+strconv.FormatFloat(float64(since.UnixMicro())/1e6, 'f', 6, 64),
		"--until", "@"+strconv.FormatFloat(float64(until.UnixMicro())/1e6, 'f', 6, 64))
	var calls []string
	for line := range strings.Lines(out) {
		if strings.Contains(line, "module=api") &&
			(strings.Contains(line, `msg="handling `) || strings.Contains(line, `msg="Calling `)) {
			calls = append(calls, strings.TrimSpace(line))
		}
	}
	return calls
}

// journalWindow is the journal of unit (or every unit matching it) between
// since and until.
func journalWindow(t *testing.T, since, until time.Time, args ...string) []string {
	t.Helper()
	all := append([]string{"--no-pager", "-o", "short-precise",
		"--since", "@" + strconv.FormatFloat(float64(since.UnixMicro())/1e6, 'f', 6, 64),
		"--until", "@" + strconv.FormatFloat(float64(until.UnixMicro())/1e6, 'f', 6, 64)}, args...)
	out, err := sh(time.Minute, "journalctl", all...)
	if err != nil && !strings.Contains(out, "No entries") {
		t.Fatalf("journalctl %v: %v\n%s", all, err, out)
	}
	var lines []string
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "-- ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// nfrStartup restarts nexusd.service runs times and returns, per run, the
// time from the main process's exec to READY=1 as systemd recorded them
// (ExecMainStartTimestampMonotonic to ActiveEnterTimestampMonotonic), and
// the wall time of `systemctl start`.
func nfrStartup(t *testing.T, runs int) (ready, wall []time.Duration) {
	t.Helper()
	mono := func(p map[string]string, k string) time.Duration {
		v, err := strconv.ParseInt(p[k], 10, 64)
		if err != nil || v == 0 {
			t.Fatalf("%s=%q", k, p[k])
		}
		return time.Duration(v) * time.Microsecond
	}
	for range runs {
		lib(t, "stop-nexusd")
		start := time.Now()
		lib(t, "start-nexusd")
		w := time.Since(start)
		p := showUnit(t, nexusdUnit, "ExecMainStartTimestampMonotonic", "ActiveEnterTimestampMonotonic", "ActiveState")
		if p["ActiveState"] != "active" {
			t.Fatalf("nexusd %s after start", p["ActiveState"])
		}
		ready = append(ready, mono(p, "ActiveEnterTimestampMonotonic")-mono(p, "ExecMainStartTimestampMonotonic"))
		wall = append(wall, w)
	}
	return ready, wall
}

// nfrWarmup is a client's use of every provider: reads of each, a script
// run and a docker start, each to its end.
func nfrWarmup(t *testing.T, n *nfrDaemon, containers []string) {
	t.Helper()
	for _, path := range []string{"/v1/capabilities", "/v1/operations", containersAt, containersAt + "/" + containers[0],
		packagesAt, packagesAt + "/" + aptPlain, upgradableAt, "/v1/providers/scripts/scripts"} {
		if _, code, b := n.timed(http.MethodGet, path, ""); code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, code, b)
		}
	}
	for _, at := range []string{"/v1/providers/scripts/scripts/ok/invoke", containersAt + "/" + containers[1] + "/start"} {
		op := startOp(t, n.nexusdInstance, at, "{}")
		if end := n.waitOp(op.ID, 2*time.Minute); end.Status != apitypes.StatusSucceeded {
			t.Fatalf("%s: %s %+v", at, end.Status, end.Error)
		}
	}
}

// startOp posts a long-running op and requires 202.
func startOp(t *testing.T, n *nexusdInstance, at, body string) opRef {
	t.Helper()
	_, code, b := n.timed(http.MethodPost, at, body)
	var op opRef
	if err := json.Unmarshal(b, &op); err != nil || code != http.StatusAccepted || op.ID == "" {
		t.Fatalf("POST %s: %d %s", at, code, b)
	}
	return op
}

type opRef struct {
	ID string `json:"id"`
}

// TC-187 and TC-188 in one idle window: startup to ready, idle RSS and CPU
// with 50 real containers, and no Docker request and no child process
// (apt or any other) from nexusd while idle. Binary sizes are TC-2's
// assertion at T0; `make nfr` prints them from dist/.
func TestNFRIdleFootprint(t *testing.T) {
	nfrLoad(t)
	if *nfrIdle < 5*time.Minute {
		t.Logf("NFR DRY RUN: idle window %v is shorter than NFR-1's 5 minutes; not a measurement", *nfrIdle)
	}
	// Debug logging first: a reload that needed a restart would stop the
	// containers.
	dockerDebug(t)
	containers := nfrContainers(t)
	n := nfrSetup(t, "nfr-idle", dockerSocket, "")
	if fi, err := os.Stat(nexusdBin); err == nil {
		t.Logf("NFR harness binary %s: %d bytes (unstripped linux/%s build; the release size is make nfr's)",
			nexusdBin, fi.Size(), os.Getenv("NEXUS_HARNESS_ARCH"))
	}

	// Startup to ready.
	ready, wall := nfrStartup(t, nfrStartRuns)
	rs, ws := nfrSummarize(ready), nfrSummarize(wall)
	t.Logf("NFR TC-187 startup exec->READY=1 (systemd monotonic) over %d starts: %s; each %v", nfrStartRuns, rs, ready)
	t.Logf("NFR TC-187 startup systemctl start wall time: %s", ws)
	if rs.max >= nfrReadyLimit {
		t.Errorf("TC-187: startup to ready max %v, want under %v", rs.max, nfrReadyLimit)
	}

	pid := n.mainPID()
	cg := nfrCgroup(t, pid)
	atStart, err := nfrReadProc(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("NFR TC-187 after start (no request yet): RSS %.1f MiB, threads %d, fds %d", mib(atStart.rss), atStart.threads, atStart.fds)

	// A client's use, which is also the positive control of both
	// detectors: it must show Docker requests and reaped children.
	warmStart := time.Now()
	before, err := nfrReadProc(pid)
	if err != nil {
		t.Fatal(err)
	}
	nfrWarmup(t, n, containers)
	after, err := nfrReadProc(pid)
	if err != nil {
		t.Fatal(err)
	}
	warmCalls := dockerCalls(t, warmStart, time.Now().Add(time.Second))
	t.Logf("NFR TC-188 control: during the warm-up the Engine logged %d API calls and nexusd's cminflt grew by %d (children reaped)",
		len(warmCalls), after.cminflt-before.cminflt)
	if len(warmCalls) == 0 {
		t.Fatal("TC-188: the Engine's debug log shows no request during the warm-up: the Docker detector cannot discriminate")
	}
	if after.cminflt == before.cminflt {
		t.Fatal("TC-188: nexusd's cminflt did not grow while it ran apt reads: the child detector cannot discriminate")
	}

	// Settle: preflight done, the operation watcher idle.
	time.Sleep(nfrSettle)

	// The idle window.
	cpu0 := nfrCgroupUsage(t, cg)
	p0, err := nfrReadProc(pid)
	if err != nil {
		t.Fatal(err)
	}
	winStart := time.Now()
	var (
		mu         sync.Mutex
		children   = map[string]bool{}
		aptSeen    = map[string]bool{}
		rssSamples []int64
		stop       = make(chan struct{})
		done       = make(chan struct{})
	)
	go func() {
		defer close(done)
		fast := time.NewTicker(50 * time.Millisecond)
		defer fast.Stop()
		slow := time.NewTicker(time.Second)
		defer slow.Stop()
		for {
			select {
			case <-stop:
				return
			case <-fast.C:
				procs := nfrCgroupProcs(cg)
				apts := nfrAptProcs()
				mu.Lock()
				for _, p := range procs {
					if p != pid {
						comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", p))
						children[fmt.Sprintf("%d %s", p, strings.TrimSpace(string(comm)))] = true
					}
				}
				for _, a := range apts {
					aptSeen[a] = true
				}
				mu.Unlock()
			case <-slow.C:
				if p, err := nfrReadProc(pid); err == nil {
					mu.Lock()
					rssSamples = append(rssSamples, p.rss)
					mu.Unlock()
				}
			}
		}
	}()
	time.Sleep(*nfrIdle)
	close(stop)
	<-done
	winEnd := time.Now()
	cpu1 := nfrCgroupUsage(t, cg)
	p1, err := nfrReadProc(pid)
	if err != nil {
		t.Fatal(err)
	}
	if n.mainPID() != pid {
		t.Fatalf("nexusd restarted during the window: pid %d, was %d", n.mainPID(), pid)
	}

	window := winEnd.Sub(winStart)
	cgCPU := float64(cpu1-cpu0) / float64(window)
	tickCPU := float64(p1.ticks-p0.ticks) / 100 / window.Seconds() // USER_HZ is 100 on Linux
	maxRSS := slices.Max(rssSamples)
	var sumRSS int64
	for _, r := range rssSamples {
		sumRSS += r
	}
	t.Logf("NFR TC-187 idle window %v (%d RSS samples at 1 s)", window.Round(time.Millisecond), len(rssSamples))
	t.Logf("NFR TC-187 idle RSS: max %.2f MiB, mean %.2f MiB, start %.2f MiB, end %.2f MiB, VmHWM %.2f MiB; threads %d -> %d, fds %d -> %d",
		mib(maxRSS), mib(sumRSS/int64(len(rssSamples))), mib(p0.rss), mib(p1.rss), mib(p1.hwm), p0.threads, p1.threads, p0.fds, p1.fds)
	t.Logf("NFR TC-187 idle CPU: cgroup usage %v over %v = %.4f%% of one core; /proc ticks %d = %.4f%%",
		(cpu1 - cpu0).Round(time.Microsecond), window.Round(time.Millisecond), cgCPU*100, p1.ticks-p0.ticks, tickCPU*100)
	if maxRSS >= nfrRSSLimit {
		t.Errorf("TC-187: idle RSS max %.2f MiB, want under 50 MiB", mib(maxRSS))
	}
	if cgCPU >= nfrCPULimit {
		t.Errorf("TC-187: idle CPU %.4f%% of one core, want under 1%%", cgCPU*100)
	}

	// TC-188: nothing in the window.
	calls := dockerCalls(t, winStart, winEnd)
	nexusdLog := journalWindow(t, winStart, winEnd, "-u", nexusdUnit)
	opUnits := journalWindow(t, winStart, winEnd, "-u", "nexus-op-*")
	mu.Lock()
	kids, apts := slices.Sorted(mapsKeys(children)), slices.Sorted(mapsKeys(aptSeen))
	mu.Unlock()
	t.Logf("NFR TC-188 idle window: %d Docker API calls, cminflt +%d, cutime+cstime +%d, %d other processes in the unit's cgroup, %d apt/dpkg processes seen (any parent), %d nexusd journal lines, %d op-unit journal lines",
		len(calls), p1.cminflt-p0.cminflt, p1.cticks-p0.cticks, len(kids), len(apts), len(nexusdLog), len(opUnits))
	if len(calls) > 0 {
		t.Errorf("TC-188: %d Docker API calls while idle:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if p1.cminflt != p0.cminflt || p1.cticks != p0.cticks || len(kids) > 0 {
		t.Errorf("TC-188: nexusd started processes while idle: cminflt %d -> %d, children seen %q", p0.cminflt, p1.cminflt, kids)
	}
	if len(apts) > 0 {
		t.Errorf("TC-188: apt or dpkg ran while idle: %q", apts)
	}
	if len(opUnits) > 0 {
		t.Errorf("TC-188: op units logged while idle:\n%s", strings.Join(opUnits, "\n"))
	}
	if len(nexusdLog) > 0 {
		t.Logf("nexusd journal lines in the window:\n%s", strings.Join(nexusdLog, "\n"))
	}
}

// nfrLoad logs the load the numbers were taken under: the harness shares
// the Colima VM's kernel, so /proc/loadavg is the VM's.
func nfrLoad(t *testing.T) {
	t.Helper()
	b, _ := os.ReadFile("/proc/loadavg")
	t.Logf("NFR load at start: VM loadavg %s, %d CPUs", strings.TrimSpace(string(b)), runtime.NumCPU())
	t.Cleanup(func() {
		b, _ := os.ReadFile("/proc/loadavg")
		t.Logf("NFR load at end: VM loadavg %s", strings.TrimSpace(string(b)))
	})
}

func mib(b int64) float64 { return float64(b) / (1 << 20) }

func mapsKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
