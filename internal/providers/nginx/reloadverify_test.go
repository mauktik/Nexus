package nginx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// reloadDir holds the captured /proc sequences (testdata/reload/README.md).
const reloadDir = "testdata/reload"

const (
	testReloadTimeout = 15 * time.Second // reloadVerifyTimeout's default
	testNginxPath     = "/usr/sbin/nginx"
)

// captureHeader is the first line of a capture.
type captureHeader struct {
	Capture      string   `json:"capture"`
	IntervalMs   int64    `json:"intervalMs"`
	MasterPID    int      `json:"masterPid"`
	MasterExe    string   `json:"masterExe"`
	ReloadArgv   []string `json:"reloadArgv"`
	ReloadSentMs int64    `json:"reloadSentMs"`
	InitExitCode int      `json:"initExitCode"`
}

type captureProc struct {
	PID     int    `json:"pid"`
	Stat    string `json:"stat"`
	Cmdline string `json:"cmdline"`
}

type captureScan struct {
	Seq   int           `json:"seq"`
	Ms    int64         `json:"ms"`
	Procs []captureProc `json:"procs"`
}

// reloadCapture is one capture, split at the moment the reload was sent.
type reloadCapture struct {
	header captureHeader
	pre    []captureScan // scans before the reload command ran
	post   []captureScan // scans from then on
}

func loadCapture(t *testing.T, file string) reloadCapture {
	t.Helper()
	f, err := os.Open(filepath.Join(reloadDir, file))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 4<<20)
	var fx reloadCapture
	for n := 0; sc.Scan(); n++ {
		if n == 0 {
			if err := json.Unmarshal(sc.Bytes(), &fx.header); err != nil {
				t.Fatalf("%s header: %v", file, err)
			}
			continue
		}
		var s captureScan
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("%s line %d: %v", file, n+1, err)
		}
		if s.Ms < fx.header.ReloadSentMs {
			fx.pre = append(fx.pre, s)
		} else {
			fx.post = append(fx.post, s)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if fx.header.MasterPID <= 0 || len(fx.pre) == 0 || len(fx.post) == 0 {
		t.Fatalf("%s: incomplete capture: master %d, %d scans before and %d after the reload", file, fx.header.MasterPID, len(fx.pre), len(fx.post))
	}
	return fx
}

// procTree writes a scan as a /proc-shaped directory, with exe links for
// the PIDs in exe, and returns its root, so the production reader reads the
// captured bytes as it reads /proc.
func procTree(t *testing.T, s captureScan, exe map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range s.Procs {
		dir := filepath.Join(root, strconv.Itoa(p.PID))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(p.Stat+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(p.Cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
		writeProcStatus(t, dir, managedUID)
		writeProcNS(t, dir, hostPIDNS)
	}
	if _, err := os.Stat(filepath.Join(root, "1")); err != nil {
		init := filepath.Join(root, "1")
		if err := os.Mkdir(init, 0o755); err != nil {
			t.Fatal(err)
		}
		writeProcNS(t, init, hostPIDNS)
	}
	for pid, target := range exe {
		if err := os.Symlink(target, filepath.Join(root, strconv.Itoa(pid), "exe")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// snapshotOf parses a scan in memory with the production parsers, as
// procDir would read it. (Writing every scan to disk instead takes seconds
// on macOS.)
func snapshotOf(t *testing.T, s captureScan) map[int]Proc {
	t.Helper()
	snap := make(map[int]Proc, len(s.Procs))
	for _, cp := range s.Procs {
		p, err := parseProcStat([]byte(cp.Stat + "\n"))
		if err != nil || p.PID != cp.PID {
			t.Fatalf("scan %d pid %d: stat %q: %v", s.Seq, cp.PID, cp.Stat, err)
		}
		p.Title = procTitle([]byte(cp.Cmdline))
		snap[p.PID] = p
	}
	return snap
}

// replayReader is the fake ProcReader: one snapshot per Snapshot call, in
// order, then the last one for ever (test-strategy.md, Fake ProcReader).
type replayReader struct {
	mu    sync.Mutex
	snaps []map[int]Proc
	exe   map[int]string
	reads int
}

func (r *replayReader) Snapshot() (map[int]Proc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.snaps[min(r.reads, len(r.snaps)-1)]
	r.reads++
	return s, nil
}

func (r *replayReader) Exe(pid int) (string, error) {
	if e, ok := r.exe[pid]; ok {
		return e, nil
	}
	return "", os.ErrNotExist
}

// RealUID is the managed owner (root, in production) of every process that
// has an exe, as the captures were taken under root.
func (r *replayReader) RealUID(pid int) (uint32, error) {
	if _, ok := r.exe[pid]; ok {
		return managedUID, nil
	}
	return 0, os.ErrNotExist
}

// PIDNamespace is the host's of every process, as the captures were taken
// on the host.
func (r *replayReader) PIDNamespace(int) (string, error) { return hostPIDNS, nil }

func (r *replayReader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// replay prepares a capture for Verify: the master validated and the
// generation observed on the last scan before the reload, and a reader
// over the scans from the reload on.
func replay(t *testing.T, fx reloadCapture) (Generation, *replayReader) {
	t.Helper()
	ownedByTester(t) // The master's /proc entry is a temp directory the test owns
	exe := map[int]string{fx.header.MasterPID: fx.header.MasterExe}
	last := fx.pre[len(fx.pre)-1]
	r0 := procDir(procTree(t, last, exe))
	if snap, err := r0.Snapshot(); err != nil || !maps.Equal(snap, snapshotOf(t, last)) {
		t.Fatalf("%s: procDir read %v, %v; the in-memory parse gives %v", fx.header.Capture, snap, err, snapshotOf(t, last))
	}
	m, err := ValidateMaster(r0, fx.header.MasterPID, testNginxPath)
	if err != nil {
		t.Fatalf("%s: ValidateMaster: %v", fx.header.Capture, err)
	}
	before, err := Observe(r0, m)
	if err != nil {
		t.Fatalf("%s: Observe: %v", fx.header.Capture, err)
	}
	r := &replayReader{exe: exe}
	for _, s := range fx.post {
		r.snaps = append(r.snaps, snapshotOf(t, s))
	}
	return before, r
}

// verifyInBubble runs Verify inside a synctest bubble and returns the
// observation and the bubble time it took.
func verifyInBubble(t *testing.T, r ProcReader, before Generation, exitCode int) (obs reloadResult) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		o, err := Verify(context.Background(), r, before, exitCode, testReloadTimeout, DefaultReloadPoll)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		obs = reloadResult{verdict: o.Verdict, old: o.OldWorkers, new: o.NewWorkers, master: o.MasterPID, exit: o.InitExitCode, elapsed: time.Since(start)}
	})
	return obs
}

type reloadResult struct {
	verdict  string
	old, new []int
	master   int
	exit     int
	elapsed  time.Duration
}

// The reference classifier. It is written apart from Classify, on the raw
// captured strings, from the Data Flow 3 rules, so the production code
// never computes its own expected value.

type refProc struct {
	ppid  string
	state string
	start string
	title string
}

// refParse splits a raw stat line after the comm field and takes the title
// as the cmdline's first NUL-separated element.
func refParse(p captureProc) refProc {
	rest := strings.Fields(p.Stat[strings.LastIndex(p.Stat, ")")+1:])
	return refProc{state: rest[0], ppid: rest[1], start: rest[19], title: strings.SplitN(p.Cmdline, "\x00", 2)[0]}
}

func refIndex(s captureScan) map[int]refProc {
	m := map[int]refProc{}
	for _, p := range s.Procs {
		m[p.PID] = refParse(p)
	}
	return m
}

type refWorker struct {
	pid   int
	start string
}

func refWorkers(s map[int]refProc, master int) map[refWorker]bool {
	ws := map[refWorker]bool{}
	for pid, p := range s {
		if p.ppid == strconv.Itoa(master) && p.title == "nginx: worker process" && p.state != "Z" && p.state != "X" {
			ws[refWorker{pid, p.start}] = true
		}
	}
	return ws
}

func refPIDs(ws map[refWorker]bool) []int {
	var pids []int
	for w := range ws {
		pids = append(pids, w.pid)
	}
	slices.Sort(pids)
	return pids
}

// refClassify gives the verdict of scan s after the generation of scan b:
// confirmed or master-gone, or what the timeout would make of it.
func refClassify(b, s captureScan, master int) (string, []int) {
	bi, si := refIndex(b), refIndex(s)
	mb, ms := bi[master], si[master]
	if _, ok := si[master]; !ok || ms.start != mb.start || ms.state == "Z" || ms.state == "X" {
		return "master-gone", nil
	}
	old := refWorkers(bi, master)
	now := refWorkers(si, master)
	fresh := map[refWorker]bool{}
	for w := range now {
		if !old[w] {
			fresh[w] = true
		}
	}
	retired, unchanged := 0, 0
	for w := range old {
		p, ok := si[w.pid]
		switch {
		case !ok || p.start != w.start || p.ppid != strconv.Itoa(master) || p.state == "Z":
			retired++
		case p.title == "nginx: worker process is shutting down":
			retired++
		case p.title == "nginx: worker process":
			unchanged++
		}
	}
	switch {
	case len(fresh) > 0 && retired == len(old):
		return "confirmed", refPIDs(fresh)
	case len(fresh) == 0 && unchanged == len(old):
		return "rejected", nil
	}
	return "unconfirmed", refPIDs(fresh)
}

// refVerify replays the polling of Data Flow 3: a scan at once and every
// 100 ms up to the timeout, the last scan repeating once the file ends.
func refVerify(fx reloadCapture) (verdict string, old, fresh []int) {
	b := fx.pre[len(fx.pre)-1]
	old = refPIDs(refWorkers(refIndex(b), fx.header.MasterPID))
	polls := int(testReloadTimeout/(100*time.Millisecond)) + 1
	for i := 0; i < polls; i++ {
		s := fx.post[min(i, len(fx.post)-1)]
		v, n := refClassify(b, s, fx.header.MasterPID)
		if v == "confirmed" || v == "master-gone" || (i == 0 && fx.header.InitExitCode != 0 && v == "rejected") || i == polls-1 {
			return v, old, n
		}
	}
	panic("unreachable")
}

// nginx-provider TC-80: Classify gives the right verdict for every captured
// reload sequence. Owner WI 5.2. Requirements: FR-6, ASM-7, NFR-6.
func TestClassifyCapturedSequences(t *testing.T) {
	want := map[string]string{
		"valid":         VerdictConfirmed,
		"bind-rejected": VerdictRejected,
		"long-conn":     VerdictConfirmed,
		"master-killed": VerdictMasterGone,
		"invalid":       VerdictRejected,
	}
	for _, plat := range []string{"ubuntu", "alpine"} {
		for _, kase := range []string{"valid", "bind-rejected", "long-conn", "master-killed", "invalid"} {
			name := plat + "-" + kase
			t.Run(name, func(t *testing.T) {
				fx := loadCapture(t, name+".jsonl")
				refV, refOld, refNew := refVerify(fx)
				if refV != want[kase] {
					t.Fatalf("the reference classifier says %s for %s, want %s: the capture does not show the case", refV, name, want[kase])
				}
				before, r := replay(t, fx)
				got := verifyInBubble(t, r, before, fx.header.InitExitCode)
				if got.verdict != refV {
					t.Errorf("Verify verdict %s, reference %s", got.verdict, refV)
				}
				if got.master != fx.header.MasterPID || got.exit != fx.header.InitExitCode {
					t.Errorf("observation names master %d, exit %d; want %d, %d", got.master, got.exit, fx.header.MasterPID, fx.header.InitExitCode)
				}
				if !slices.Equal(got.old, refOld) || len(refOld) != 2 {
					t.Errorf("old workers %v, from the file %v (two expected: worker_processes 2)", got.old, refOld)
				}
				if !slices.Equal(got.new, refNew) {
					t.Errorf("new workers %v, from the file %v", got.new, refNew)
				}
				if got.verdict == VerdictConfirmed && len(got.new) != 2 {
					t.Errorf("confirmed with new workers %v, want two", got.new)
				}
				// Classify on each scan agrees with the reference too.
				b := fx.pre[len(fx.pre)-1]
				for i, s := range fx.post {
					rv, rn := refClassify(b, s, fx.header.MasterPID)
					c := Classify(before, r.snaps[i])
					if c.Verdict != rv || !slices.Equal(workerPIDs(c.NewWorkers), rn) {
						t.Errorf("scan %d: Classify %s %v, reference %s %v", s.Seq, c.Verdict, workerPIDs(c.NewWorkers), rv, rn)
					}
				}
				if kase == "long-conn" {
					last := fx.post[len(fx.post)-1]
					shutting := 0
					for _, p := range last.Procs {
						if refParse(p).title == "nginx: worker process is shutting down" {
							shutting++
						}
					}
					if shutting != 1 {
						t.Errorf("the last scan has %d workers shutting down, want 1", shutting)
					}
				}
				t.Logf("%s: %s after %v, old %v, new %v, init exit %d", name, got.verdict, got.elapsed, got.old, got.new, got.exit)
			})
		}
	}
}

// nginx-provider TC-81: Observe counts only the master's worker children.
// Owner WI 5.2. Requirements: FR-6, L-24.
func TestObserveFiltersWorkers(t *testing.T) {
	for _, file := range []string{"ubuntu-bind-rejected.jsonl", "alpine-bind-rejected.jsonl"} {
		t.Run(file, func(t *testing.T) {
			fx := loadCapture(t, file)
			s := fx.pre[0]
			snap := snapshotOf(t, s)
			m := fx.header.MasterPID
			// The scan has what the filter must leave out.
			var titles []string
			var foreign []int
			for _, p := range snap {
				titles = append(titles, p.Title)
				if p.Title == workerTitle && p.PPID != m {
					if pp, ok := snap[p.PPID]; ok && strings.HasPrefix(pp.Title, masterTitlePrefix) {
						foreign = append(foreign, p.PID)
					}
				}
			}
			for _, want := range []string{"nginx: cache manager process", "nginx: cache loader process"} {
				if !slices.Contains(titles, want) {
					t.Fatalf("the captured scan has no %q: %q", want, titles)
				}
			}
			if len(foreign) != 1 {
				t.Fatalf("the captured scan has %d workers of another master, want 1", len(foreign))
			}
			mp := snap[m]
			r := &replayReader{snaps: []map[int]Proc{snap}}
			g, err := Observe(r, Master{PID: m, StartTicks: mp.StartTicks})
			if err != nil {
				t.Fatal(err)
			}
			var want []Worker
			for _, p := range s.Procs {
				rp := refParse(p)
				if rp.ppid == strconv.Itoa(m) && rp.title == "nginx: worker process" {
					st, _ := strconv.ParseUint(rp.start, 10, 64)
					want = append(want, Worker{PID: p.PID, StartTicks: st})
				}
			}
			if len(want) != 2 || !slices.Equal(g.Workers, want) {
				t.Errorf("Observe workers %v, want exactly %v", g.Workers, want)
			}
			if slices.ContainsFunc(g.Workers, func(w Worker) bool { return slices.Contains(foreign, w.PID) }) {
				t.Errorf("Observe counted another master's worker %v", foreign)
			}
		})
	}
}

// TestObserveMasterGone: Observe refuses a master whose (pid, startTicks)
// changed, so no reload is sent (Data Flow 3 step 3).
func TestObserveMasterGone(t *testing.T) {
	fx := loadCapture(t, "ubuntu-valid.jsonl")
	snap := snapshotOf(t, fx.pre[0])
	m := Master{PID: fx.header.MasterPID, StartTicks: snap[fx.header.MasterPID].StartTicks}
	for name, mm := range map[string]Master{
		"other start time": {PID: m.PID, StartTicks: m.StartTicks + 1},
		"no such pid":      {PID: 99999, StartTicks: m.StartTicks},
	} {
		_, err := Observe(&replayReader{snaps: []map[int]Proc{snap}}, mm)
		if !errors.Is(err, ErrMasterGone) {
			t.Errorf("%s: Observe error %v, want ErrMasterGone", name, err)
		}
	}
}

// nginx-provider TC-82: Verify polls every 100 ms until confirmed or until
// the timeout. Owner WI 5.2. Requirements: FR-6, NFR-2, L-28.
func TestVerifyPollingCadence(t *testing.T) {
	fx := loadCapture(t, "ubuntu-valid.jsonl")
	before, full := replay(t, fx)
	unchanged := snapshotOf(t, fx.pre[len(fx.pre)-1])
	done := full.snaps[len(full.snaps)-1]
	if c := Classify(before, done); c.Verdict != VerdictConfirmed {
		t.Fatalf("the last captured scan is %s, not confirmed", c.Verdict)
	}

	// The first reader confirms only at its 7th snapshot.
	r := &replayReader{}
	for range 6 {
		r.snaps = append(r.snaps, unchanged)
	}
	r.snaps = append(r.snaps, done)
	got := verifyInBubble(t, r, before, 0)
	if got.verdict != VerdictConfirmed || got.elapsed < 600*time.Millisecond || got.elapsed > 700*time.Millisecond || r.count() != 7 {
		t.Errorf("first reader: %s after %v with %d snapshots, want confirmed after 600-700ms with 7", got.verdict, got.elapsed, r.count())
	}

	// The second never confirms: the generation stays as it was.
	r2 := &replayReader{snaps: []map[int]Proc{unchanged}}
	got = verifyInBubble(t, r2, before, 0)
	if d := got.elapsed - testReloadTimeout; d < -100*time.Millisecond || d > 100*time.Millisecond {
		t.Errorf("second reader returned after %v, want %v ±100ms", got.elapsed, testReloadTimeout)
	}
	if got.verdict != VerdictRejected {
		t.Errorf("second reader verdict %s, want rejected (no new worker, old ones unchanged)", got.verdict)
	}
	if want := int(testReloadTimeout/DefaultReloadPoll) + 1; r2.count() != want {
		t.Errorf("second reader read %d snapshots, want %d (one every 100ms from 0 to 15s)", r2.count(), want)
	}
}

// TestVerifyStopsOnContext: a cancelled context ends the wait with
// unconfirmed, since a reload was sent and its effect is unknown.
func TestVerifyStopsOnContext(t *testing.T) {
	fx := loadCapture(t, "ubuntu-bind-rejected.jsonl")
	before, r := replay(t, fx)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		obs, err := Verify(ctx, r, before, 0, testReloadTimeout, DefaultReloadPoll)
		if !errors.Is(err, context.DeadlineExceeded) || obs.Verdict != VerdictUnconfirmed {
			t.Errorf("Verify after the context ended: %s, %v; want unconfirmed, deadline exceeded", obs.Verdict, err)
		}
	})
}

// readmeRow is one row of testdata/reload/README.md's provenance table.
type readmeRow map[string]string

func readmeRows(t *testing.T) []readmeRow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(reloadDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	var rows []readmeRow
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		for i := range cells {
			cells[i] = strings.Trim(strings.TrimSpace(cells[i]), "`")
		}
		switch {
		case cols == nil:
			cols = cells
		case strings.HasPrefix(cells[0], "---"):
		default:
			if len(cells) != len(cols) {
				t.Fatalf("README row has %d cells, want %d: %s", len(cells), len(cols), l)
			}
			r := readmeRow{}
			for i, c := range cols {
				r[c] = cells[i]
			}
			rows = append(rows, r)
		}
	}
	return rows
}

// nginx-provider TC-83: Reload fixtures carry their real-host provenance.
// Owner WI 5.2. Requirements: ASM-7, NFR-6.
func TestReloadFixtureProvenance(t *testing.T) {
	rows := readmeRows(t)
	byFile := map[string]readmeRow{}
	for _, r := range rows {
		byFile[r["File"]] = r
		for _, col := range []string{"Kind", "Host", "Distribution", "Init", "nginx", "Reload command", "Exit", "Capture command", "Date"} {
			if r[col] == "" {
				t.Errorf("%s: empty %s", r["File"], col)
			}
		}
		for col, v := range r {
			if strings.Contains(strings.ToUpper(v), "UNCONFIRMED") {
				t.Errorf("%s: %s is %q", r["File"], col, v)
			}
		}
		switch r["Kind"] {
		case "captured":
			if r["Base"] != "-" || r["Edited line"] != "-" {
				t.Errorf("%s: a capture names a base or an edited line", r["File"])
			}
		case "synthetic":
			checkSynthetic(t, r)
		default:
			t.Errorf("%s: kind %q is neither captured nor synthetic", r["File"], r["Kind"])
		}
	}

	files, err := filepath.Glob(filepath.Join(reloadDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		base := filepath.Base(f)
		if base == "README.md" {
			continue
		}
		if fi, err := os.Stat(f); err != nil || fi.IsDir() {
			continue
		}
		r, ok := byFile[base]
		if !ok {
			t.Errorf("%s has no README row", base)
			continue
		}
		if strings.HasSuffix(base, ".jsonl") {
			fx := loadCapture(t, base)
			if strconv.Itoa(fx.header.InitExitCode) != r["Exit"] {
				t.Errorf("%s: header exit %d, README %s", base, fx.header.InitExitCode, r["Exit"])
			}
		}
	}
	for _, plat := range []string{"ubuntu", "alpine"} {
		for _, kase := range []string{"valid", "bind-rejected", "long-conn", "master-killed"} {
			f := plat + "-" + kase + ".jsonl"
			if r, ok := byFile[f]; !ok || r["Kind"] != "captured" {
				t.Errorf("required case %s is missing or not a capture", f)
			}
		}
		if r, ok := byFile["exe-after-replace-"+plat+".txt"]; !ok || r["Kind"] != "captured" {
			t.Errorf("the %s exe-after-replace capture (L-53) is missing", plat)
		}
	}
}

// checkSynthetic checks that a synthetic sequence differs from its base
// capture in exactly the line the README names.
func checkSynthetic(t *testing.T, r readmeRow) {
	t.Helper()
	base, ok := fileLines(r["Base"])
	if !ok {
		t.Errorf("%s: base %q does not exist", r["File"], r["Base"])
		return
	}
	syn, ok := fileLines(r["File"])
	if !ok {
		t.Errorf("synthetic %s does not exist", r["File"])
		return
	}
	n, err := strconv.Atoi(r["Edited line"])
	if err != nil {
		t.Errorf("%s: edited line %q: %v", r["File"], r["Edited line"], err)
		return
	}
	if len(base) != len(syn) {
		t.Errorf("%s has %d lines, its base %d", r["File"], len(syn), len(base))
		return
	}
	for i := range base {
		if (base[i] != syn[i]) != (i+1 == n) {
			t.Errorf("%s line %d: differs from the base %v, README says only line %d does", r["File"], i+1, base[i] != syn[i], n)
		}
	}
}

func fileLines(name string) ([]string, bool) {
	b, err := os.ReadFile(filepath.Join(reloadDir, name))
	if err != nil {
		return nil, false
	}
	return strings.Split(string(b), "\n"), true
}

// nginx-provider TC-E18: A crashed and respawned worker is unconfirmed,
// not confirmed. Owner WI 5.2. Requirements: FR-6, NFR-2.
func TestRespawnedWorkerIsUnconfirmed(t *testing.T) {
	fx := loadCapture(t, "ubuntu-respawn.jsonl")
	before, r := replay(t, fx)
	got := verifyInBubble(t, r, before, fx.header.InitExitCode)
	if got.verdict != VerdictUnconfirmed {
		t.Errorf("respawned worker: %s, want unconfirmed (outcome-unknown)", got.verdict)
	}
	if got.elapsed != testReloadTimeout {
		t.Errorf("respawned worker: returned after %v, want the full %v", got.elapsed, testReloadTimeout)
	}
	if !slices.Equal(got.new, []int{990}) {
		t.Errorf("new workers %v, want the respawned [990]", got.new)
	}
	// Its base, unedited, is rejected.
	before, r = replay(t, loadCapture(t, "ubuntu-bind-rejected.jsonl"))
	if got := verifyInBubble(t, r, before, 0); got.verdict != VerdictRejected {
		t.Errorf("the base capture: %s, want rejected", got.verdict)
	}
}

// nginx-provider TC-E19: A non-zero init exit with an unchanged generation
// is rejected at once. Owner WI 5.2. Requirements: FR-6, ASM-7.
//
// The sequences are real captures of a reload of an invalid configuration,
// which both init systems answered with exit 1.
func TestNonZeroExitRejectedImmediately(t *testing.T) {
	for _, file := range []string{"ubuntu-invalid.jsonl", "alpine-invalid.jsonl"} {
		t.Run(file, func(t *testing.T) {
			fx := loadCapture(t, file)
			if fx.header.InitExitCode == 0 {
				t.Fatalf("%s: init exit 0, want a non-zero capture", file)
			}
			before, r := replay(t, fx)
			got := verifyInBubble(t, r, before, fx.header.InitExitCode)
			if got.verdict != VerdictRejected || got.elapsed >= 100*time.Millisecond || r.count() != 1 {
				t.Errorf("%s after %v with %d snapshots, want rejected at once from 1 snapshot", got.verdict, got.elapsed, r.count())
			}
			// The same sequence with exit 0 waits for the timeout.
			before, r = replay(t, fx)
			if got := verifyInBubble(t, r, before, 0); got.verdict != VerdictRejected || got.elapsed != testReloadTimeout {
				t.Errorf("exit 0: %s after %v, want rejected after %v", got.verdict, got.elapsed, testReloadTimeout)
			}
		})
	}
}

// nginx-provider TC-ERR15: ValidateMaster refuses a PID that is not the
// nginx master, and accepts an upgraded master. Owner WI 5.2.
// Requirements: FR-6, L-24, L-53.
func TestValidateMasterRefuses(t *testing.T) {
	ownedByTester(t) // The fake /proc entries are temp directories the test owns
	// The (deleted) form of row 4 is what the kernel showed after the
	// binary was replaced on both captured hosts.
	for _, plat := range []string{"ubuntu", "alpine"} {
		b, err := os.ReadFile(filepath.Join(reloadDir, "exe-after-replace-"+plat+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "after\n/usr/sbin/nginx (deleted)\n") {
			t.Fatalf("exe-after-replace-%s.txt does not show %q after the replacement", plat, "/usr/sbin/nginx (deleted)")
		}
	}

	// Every entry gets the captured Ubuntu master's stat line with its own
	// PID, so its start time is known.
	fx := loadCapture(t, "ubuntu-valid.jsonl")
	var masterStat string
	for _, p := range fx.pre[0].Procs {
		if p.PID == fx.header.MasterPID {
			masterStat = strings.TrimPrefix(p.Stat, strconv.Itoa(p.PID))
		}
	}
	start, err := strconv.ParseUint(refParse(captureProc{Stat: masterStat}).start, 10, 64)
	if err != nil || start == 0 {
		t.Fatalf("captured master stat %q: %v", masterStat, err)
	}

	const masterTitle = "nginx: master process /usr/sbin/nginx"
	pad := strings.Repeat("\x00", 40)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProcNS(t, filepath.Join(root, "1"), hostPIDNS)
	type row struct {
		name    string
		pid     int
		exe     string // "" for no entry at all
		cmdline string
		want    string // "" for accepted, else a word the error names
	}
	rows := []row{
		{"0 exact path", 100, "/usr/sbin/nginx", masterTitle + pad, ""},
		{"1 other exe", 101, "/usr/bin/sleep", masterTitle + pad, "exe"},
		{"2 worker title", 102, "/usr/sbin/nginx", "nginx: worker process" + pad, "title"},
		{"3 dead pid", 103, "", "", "gone"},
		{"3a pid 0", 0, "", "", "pid (0)"},
		{"4 upgraded binary", 104, "/usr/sbin/nginx (deleted)", masterTitle + pad, ""},
		{"5 other exe deleted", 105, "/usr/bin/sleep (deleted)", masterTitle + pad, "exe"},
		{"6 other path deleted", 106, "/usr/sbin/nginx.new (deleted)", masterTitle + pad, "exe"},
		{"7 deleted twice", 107, "/usr/sbin/nginx (deleted) (deleted)", masterTitle + pad, "exe"},
	}
	for _, rw := range rows {
		if rw.exe == "" {
			continue
		}
		dir := filepath.Join(root, strconv.Itoa(rw.pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stat := strconv.Itoa(rw.pid) + masterStat + "\n"
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(rw.cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
		writeProcStatus(t, dir, managedUID)
		writeProcNS(t, dir, hostPIDNS)
		if err := os.Symlink(rw.exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	r := procDir(root)
	for _, rw := range rows {
		m, err := ValidateMaster(r, rw.pid, testNginxPath)
		switch {
		case rw.want == "" && err != nil:
			t.Errorf("row %s: refused: %v", rw.name, err)
		case rw.want == "" && (m.PID != rw.pid || m.StartTicks != start):
			t.Errorf("row %s: master %+v, want pid %d start %d", rw.name, m, rw.pid, start)
		case rw.want != "" && err == nil:
			t.Errorf("row %s: accepted, want an error naming %q", rw.name, rw.want)
		case rw.want != "" && !strings.Contains(err.Error(), rw.want):
			t.Errorf("row %s: error %q does not name %q", rw.name, err, rw.want)
		}
		if err != nil {
			t.Logf("row %s: %v", rw.name, err)
		}
	}
}

// TestHostProcReader: on Linux the real reader sees this test process with
// its parent and start time; elsewhere it refuses.
func TestHostProcReader(t *testing.T) {
	r := HostProcReader()
	snap, err := r.Snapshot()
	if runtime.GOOS != "linux" {
		if err == nil || !strings.Contains(err.Error(), "needs Linux") {
			t.Fatalf("Snapshot on %s: %v, want an error that it needs Linux", runtime.GOOS, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	self, ok := snap[os.Getpid()]
	if !ok || self.PPID != os.Getppid() || self.StartTicks == 0 || self.Title == "" {
		t.Fatalf("own process %+v, want ppid %d, a start time and a title", self, os.Getppid())
	}
	exe, err := r.Exe(os.Getpid())
	want, _ := os.Executable()
	if err != nil || exe != want {
		t.Errorf("Exe(self) = %q, %v; want %q", exe, err, want)
	}
	if _, err := r.Exe(1 << 30); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Exe of a missing pid: %v, want not-exist", err)
	}
}
