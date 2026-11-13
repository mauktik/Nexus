//go:build integration && linux

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/problem"
	phspec "github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// phGravityScript is what Nexus looks for in /proc/*/cmdline to wait for a
// gravity run (v5 GravityScript).
const phGravityScript = "/opt/pihole/gravity.sh"

// phFakeGravity starts a process whose cmdline names gravity.sh, so every
// domain, adlist and gravity op waits behind it: the lane gate of TC-129.
// The returned release ends it.
func phFakeGravity(t *testing.T) (release func()) {
	t.Helper()
	cmd := exec.Command("bash", "-c", "exec -a "+phGravityScript+" sleep 600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	t.Cleanup(release)
	waitUntil(t, 10*time.Second, "the fake gravity.sh", func() (bool, string) {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", cmd.Process.Pid))
		return bytes.Contains(b, []byte(phGravityScript)), string(b)
	})
	return release
}

// phWaitLog waits until operation id's output contains s.
func phWaitLog(t *testing.T, n *nexusdInstance, id, s string) {
	t.Helper()
	waitUntil(t, time.Minute, id+" logging "+s, func() (bool, string) {
		l := n.opLogs(id)
		return strings.Contains(l, s), l
	})
}

// TC-129: a hand edit between Admit and run gives dns-file-conflict and is
// kept; a planted invalid line fails the syntax test with preExisting,
// the bare test's exit code, the file restored, no core file and no
// restart.
func TestPiholeConflictAndConfigTest(t *testing.T) {
	phUseSeed(t)
	n := piholed(t, "tc129", "")
	t.Cleanup(func() { phSQLWrite(t, "DELETE FROM domainlist WHERE domain = 'tc129-gate.example';") })

	// 1. The gate holds the lane; the zone add, with the etag read before
	// the hand edit, is admitted and queues behind it.
	records := phList[phspec.RecordProperties](t, n, phspec.RecordResourceType, "")
	etag := records[0].Properties.Etag
	if etag != "sha256:"+phSeedSHA {
		t.Fatalf("record etag %s, the file's sha256 is %s", etag, phSeedSHA)
	}
	release := phFakeGravity(t)
	gate := phStart(t, n, "/actions/add-domain", `{"list":"deny","kind":"exact","value":"tc129-gate.example"}`)
	phWaitLog(t, n, gate.ID, "waiting for a gravity run")
	op := phStart(t, n, "/actions/add-zone", `{"zone":"home.local","ifMatch":"`+etag+`"}`)
	queued := n.operation(op.ID).Status
	edited := append(phSeedBytes(t), "# tc129 hand edit\n"...)
	phWriteDNSFile(t, edited)
	release()
	end := n.waitOp(op.ID, 2*time.Minute)
	gend := n.waitOp(gate.ID, 2*time.Minute)
	after := phReadDNSFile(t)
	t.Logf("gate %s %s; zone add %s was %s while gated, ended %s %+v; file kept the hand edit: %v", gate.ID, gend.Status,
		op.ID, queued, end.Status, end.Error, bytes.Equal(after, edited))
	t.Logf("zone add log:\n%s", n.opLogs(op.ID))
	phFailed(t, end, problem.DNSFileConflict)
	if queued != apitypes.StatusQueued || !bytes.Equal(after, edited) {
		t.Errorf("gated zone add was %s; file after:\n%s", queued, after)
	}
	phRequireNoStray(t, "the conflicted zone add")
	phWriteDNSFile(t, phSeedBytes(t)) // A comment only: FTL's configuration is unchanged

	// 2. A planted invalid unmanaged line, after a clean zone add.
	phDNSChange(t, n, "/actions/add-zone", `{"zone":"app.lan"}`)
	planted := append(phReadDNSFile(t), "address=/bad.lan/999.1.1.1\n"...)
	phWriteDNSFile(t, planted)
	_ = os.Remove("/core")
	scratch := t.TempDir()
	// The shell's $? is the code as a shell reports it: 128+signal for a
	// death by signal, as the helper's Runner does.
	bare := phBash(t, "cd "+scratch+" && ulimit -c 0 && { "+phFTLBin+" --test; echo \"rc=$?\"; }")
	_, rc, _ := strings.Cut(bare, "rc=")
	code, err := strconv.Atoi(strings.TrimSpace(rc))
	if err != nil {
		t.Fatalf("bare --test: %q", bare)
	}
	t.Logf("bare %s --test on the planted file: $? %d\n%s", phFTLBin, code, bare)
	if code == 0 {
		t.Fatalf("the bare syntax test passed the planted line")
	}
	pid0, _ := phFTLPID()
	since := time.Now().Add(-time.Second)
	fop := phDNSOp(t, n, "/actions/add-record", `{"fqdn":"x.app.lan","ipv4":"192.168.7.44"}`)
	pe := phFailed(t, fop, problem.DNSConfigTestFailed)
	restored := phReadDNSFile(t)
	pid1, _ := phFTLPID()
	journal, _ := sh(30*time.Second, "journalctl", "-u", phFTLUnit, "--since", "@"+strconv.FormatInt(since.Unix(), 10), "--no-pager")
	opDir := filepath.Join(phOpsDir("tc129"), fop.ID)
	cores, _ := filepath.Glob(filepath.Join(opDir, "core*"))
	rootCore := exists("/core")
	pattern, _ := os.ReadFile("/proc/sys/kernel/core_pattern")
	t.Logf("add-record over the planted line: %s %+v; file restored byte-identical %v; FTL PID %d -> %d; "+
		"core_pattern %q, /core %v, op dir cores %v; FTL journal in the window:\n%s", fop.Status, pe.Extensions,
		bytes.Equal(restored, planted), pid0, pid1, strings.TrimSpace(string(pattern)), rootCore, cores, journal)
	if ec := fmt.Sprint(pe.Extensions["exitCode"]); ec != strconv.Itoa(code) || ec == "0" ||
		pe.Extensions["preExisting"] != true || pe.Extensions["rolledBack"] != true {
		t.Errorf("dns-config-test-failed %v: want exitCode %d (the bare test's, non-zero), preExisting true", pe.Extensions, code)
	}
	if !bytes.Equal(restored, planted) {
		t.Errorf("the file was not restored byte for byte:\n%s", restored)
	}
	if rootCore || len(cores) > 0 {
		t.Errorf("core files left: /core %v, op directory %v (A-57)", rootCore, cores)
	}

	// 3. The failed test restarted nothing (A-21).
	if pid1 != pid0 || strings.Contains(journal, "Started") || strings.Contains(journal, "Stopping") {
		t.Errorf("FTL PID %d -> %d, or the journal shows a restart, after the failed syntax test", pid0, pid1)
	}
	if !phAnswers() {
		t.Error("FTL does not answer after the failed syntax test")
	}
	phWriteDNSFile(t, phSeedBytes(t)) // FTL still serves the seed: it never loaded the planted line
}

// phInotify watches dir for files created or moved in and reports their
// names when stopped.
type phInotify struct {
	f     *os.File
	mu    sync.Mutex
	names []string
	wg    sync.WaitGroup
}

func phWatchDir(t *testing.T, dir string) *phInotify {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syscall.InotifyAddWatch(fd, dir, syscall.IN_CREATE|syscall.IN_MOVED_TO); err != nil {
		t.Fatal(err)
	}
	w := &phInotify{f: os.NewFile(uintptr(fd), "inotify")}
	w.wg.Go(func() {
		buf := make([]byte, 64<<10)
		for {
			k, err := w.f.Read(buf)
			if err != nil {
				return
			}
			for off := 0; off+syscall.SizeofInotifyEvent <= k; {
				ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
				name := string(bytes.TrimRight(buf[off+syscall.SizeofInotifyEvent:off+syscall.SizeofInotifyEvent+int(ev.Len)], "\x00"))
				w.mu.Lock()
				w.names = append(w.names, name)
				w.mu.Unlock()
				off += syscall.SizeofInotifyEvent + int(ev.Len)
			}
		}
	})
	return w
}

func (w *phInotify) done() []string {
	_ = w.f.Close()
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.names
}

// TC-131: sandboxed reads under the shipped unit: no SQLite sidecar and no
// denial while reading; FTL stopped gives null fields without an error and
// leaves apt and docker as they were; an unreadable gravity.db makes the
// domain list dependency-unavailable while zones still read.
func TestPiholeFaultIsolation(t *testing.T) {
	n := piholed(t, "tc131", "  apt:\n    enabled: true\n  docker:\n    enabled: true\n")
	t.Cleanup(func() {
		if !phAnswers() {
			phStartFTL(t)
		}
		phRequireFTLHealthy(t)
	})
	apt0, docker0 := phCapability(t, n, "apt"), phCapability(t, n, "docker")
	t.Logf("before: apt %s %q; docker %s %q", apt0.Status, apt0.Reason, docker0.Status, docker0.Reason)

	// 3. 50 domain reads under inotify, and the journal of the window.
	since := time.Now().Add(-time.Second)
	w := phWatchDir(t, "/etc/pihole")
	for range 50 {
		phList[phspec.DomainProperties](t, n, phspec.DomainResourceType, "")
	}
	created := w.done()
	var sidecars []string
	for _, name := range created {
		if strings.HasSuffix(name, "-journal") || strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			sidecars = append(sidecars, name)
		}
	}
	journal := mustSh(t, "journalctl", "-u", nexusdUnit, "--since", "@"+strconv.FormatInt(since.Unix(), 10), "--no-pager")
	faults := journalFaults(journal)
	t.Logf("50 domain reads: files created in /etc/pihole %v; sidecars %v; journal denials %v", created, sidecars, faults)
	if len(sidecars) > 0 || len(faults) > 0 {
		t.Errorf("reads created %v or were denied: %v\n%s", sidecars, faults, journal)
	}

	// 1. FTL stopped.
	phResetFailed(t)
	mustSh(t, "systemctl", "stop", phFTLUnit)
	waitUntil(t, 30*time.Second, "FTL stopped", func() (bool, string) {
		_, ok := phFTLPID()
		return !ok && !phAnswers(), ""
	})
	code, b := n.get(phAt + "/status")
	var st apitypes.List[phspec.Status]
	_ = json.Unmarshal(b, &st)
	code2, b2 := n.get(phAt + "/summary")
	var sum apitypes.List[phspec.Summary]
	_ = json.Unmarshal(b2, &sum)
	apt1, docker1 := phCapability(t, n, "apt"), phCapability(t, n, "docker")
	t.Logf("FTL stopped: status %d %s; summary %d %s; apt %s; docker %s %q", code, b, code2, b2, apt1.Status,
		docker1.Status, docker1.Reason)
	if code != http.StatusOK || len(st.Items) != 1 || code2 != http.StatusOK || len(sum.Items) != 1 {
		t.Fatalf("reads with FTL stopped: status %d, summary %d", code, code2)
	}
	s, m := st.Items[0].Properties, sum.Items[0].Properties
	if s.FTLRunning == nil || *s.FTLRunning || s.DNSAnswering == nil || *s.DNSAnswering ||
		m.FTLAPIReachable || m.QueriesToday != nil || m.BlockedToday != nil || m.BlockedPercent != nil || m.UniqueClients != nil {
		t.Errorf("FTL stopped: status %+v summary %+v; want ftlRunning false, dnsAnswering false, null counters", s, m)
	}
	if apt1.Status != apitypes.ProviderEnabled || apt1.Status != apt0.Status || docker1.Status != docker0.Status {
		t.Errorf("apt %s -> %s, docker %s -> %s: want apt enabled and both unchanged", apt0.Status, apt1.Status,
			docker0.Status, docker1.Status)
	}
	phStartFTL(t)
	phRequireFTLHealthy(t)

	// 2. gravity.db unreadable.
	fi, err := os.Stat(phGravityDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(phGravityDB, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(phGravityDB, fi.Mode().Perm()); err != nil {
			t.Error(err)
		}
	})
	dcode, dslug, dp := n.problem(phAt + "/domains")
	zcode, zb := n.get(phAt + "/zones")
	if err := os.Chmod(phGravityDB, fi.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	// nexusd runs as root with CAP_DAC_READ_SEARCH in its bounding set
	// (deploy/nexusd.service), which bypasses read permission: chmod 000
	// does not make gravity.db unreadable to it. Recorded, not asserted;
	// the file being absent, as during gravity's swap, is the fault that
	// reaches nexusd.
	t.Logf("gravity.db chmod 000 (observation): domains %d %s %q; zones %d %s; mode restored to %v", dcode, dslug,
		dp.Detail, zcode, zb, fi.Mode().Perm())
	if zcode != http.StatusOK {
		t.Errorf("zones with gravity.db chmod 000: %d %s", zcode, zb)
	}
	aside := phGravityDB + ".tc131"
	if err := os.Rename(phGravityDB, aside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if exists(aside) {
			if err := os.Rename(aside, phGravityDB); err != nil {
				t.Error(err)
			}
		}
	})
	dcode, dslug, dp = n.problem(phAt + "/domains")
	zcode, zb = n.get(phAt + "/zones")
	if err := os.Rename(aside, phGravityDB); err != nil {
		t.Fatal(err)
	}
	t.Logf("gravity.db absent: domains %d %s %q; zones %d %s", dcode, dslug, dp.Detail, zcode, zb)
	if dslug != problem.DependencyUnavailable {
		t.Errorf("domains with gravity.db absent: %d %s, want dependency-unavailable", dcode, dslug)
	}
	if zcode != http.StatusOK {
		t.Errorf("zones with gravity.db absent: %d %s", zcode, zb)
	}
	if exists(phGravityDB+"-journal") || exists(phGravityDB+"-wal") {
		t.Error("a sidecar was left beside gravity.db")
	}
}

// phSlowList serves a list at /slow.txt one line a second for lines lines,
// so a gravity run that downloads it takes that long. It reports each
// request's start on started.
func phSlowList(t *testing.T, lines int) (addr string, started <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{}, 16)
	mux := http.NewServeMux()
	mux.HandleFunc("/slow.txt", func(w http.ResponseWriter, r *http.Request) {
		select {
		case ch <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "text/plain")
		fl, _ := w.(http.Flusher)
		for i := range lines {
			if _, err := fmt.Fprintf(w, "slow-%d.example\n", i+1); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
			}
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String() + "/slow.txt", ch
}

// TC-132: an update-gravity survives nexusd's SIGKILL; a domain add queued
// during an external `pihole -g` waits for it and lands after its swap.
func TestPiholeSurvivalAndExternalGravity(t *testing.T) {
	slow, started := phSlowList(t, 20)
	t.Cleanup(func() {
		phDropAdlist(t, slow)
		phSQLWrite(t, "DELETE FROM domainlist WHERE domain = 'tc132-wait.example';")
	})
	n := piholed(t, "tc132", "")
	phResult[phspec.AdlistResult](t, phRun(t, n, "/actions/add-adlist", `{"url":"`+slow+`"}`, time.Minute))

	// 1. SIGKILL nexusd while gravity downloads the slow list.
	updated0, _ := strconv.Atoi(phInfo(t, "updated"))
	op := phStart(t, n, "/actions/update-gravity", "{}")
	select {
	case <-started:
	case <-time.After(2 * time.Minute):
		t.Fatal("gravity never requested the slow list")
	}
	pid := n.mainPID()
	mustSh(t, "systemctl", "kill", "-s", "KILL", nexusdUnit)
	waitUntil(t, 15*time.Second, "nexusd to stop", func() (bool, string) {
		st := activeState(t, nexusdUnit)
		return st == "failed" || st == "inactive", st
	})
	t.Logf("killed nexusd pid %d mid-gravity (op %s); op unit %s", pid, op.ID, activeState(t, "nexus-op-"+op.ID+".service"))
	lib(t, "start-nexusd")
	end := waitOpAcrossRestart(t, n, op.ID, 10*time.Minute)
	updated1, _ := strconv.Atoi(phInfo(t, "updated"))
	t.Logf("update-gravity across the kill: %s %s result %s error %+v; info.updated %d -> %d", end.Status, phTook(end),
		end.Result, end.Error, updated0, updated1)
	switch {
	case end.Status == apitypes.StatusSucceeded:
	case end.Status == apitypes.StatusFailed && end.Error != nil && end.Error.Slug() == problem.OutcomeUnknown:
		t.Logf("outcome-unknown with detail %q", end.Error.Detail)
	default:
		t.Errorf("update-gravity across the kill ended %s %+v, want Succeeded or outcome-unknown", end.Status, end.Error)
	}
	if updated1 <= updated0 {
		t.Errorf("info.updated %d -> %d: the gravity run did not complete", updated0, updated1)
	}

	// 2. A domain add queued during an external pihole -g.
	g := exec.Command("bash", "-c", phPihole+" -g")
	var gout bytes.Buffer
	g.Stdout, g.Stderr = &gout, &gout
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 30*time.Second, "pihole -g running", func() (bool, string) {
		out, _ := sh(5*time.Second, "pgrep", "-f", phGravityScript)
		return strings.TrimSpace(out) != "", out
	})
	add := phStart(t, n, "/actions/add-domain", `{"list":"deny","kind":"exact","value":"tc132-wait.example"}`)
	gerr := g.Wait()
	swapped := time.Now()
	aend := n.waitOp(add.ID, 2*time.Minute)
	logs := n.opLogs(add.ID)
	row := phSQL(t, "SELECT id || ' ' || type FROM domainlist WHERE domain = 'tc132-wait.example'")
	t.Logf("external pihole -g: %v, done at %s; add-domain %s ended %s at %v; row %q; log:\n%s", gerr,
		swapped.UTC().Format(time.RFC3339Nano), add.ID, aend.Status, aend.Ended, row, logs)
	if gerr != nil {
		t.Fatalf("pihole -g: %v\n%s", gerr, gout.String())
	}
	phResult[phspec.DomainResult](t, aend)
	if !strings.Contains(logs, "waiting for a gravity run") || row == "" || aend.Ended == nil || aend.Ended.Before(swapped) {
		t.Errorf("the add did not wait for the external gravity run, or its row is gone (row %q)", row)
	}
}

// phAttack is one input sent through one string parameter.
type phAttack struct {
	method, path string // path may hold %s for a path-segment attack
	body         map[string]any
	field        string // the body field, or "" for the path segment
	inert        bool   // add-domain or remove-domain /value with kind regex
}

// phAttacks are the conformance list's FR-25 attacks and the pihole ones
// (internal/conformance negative_test.go attacks and piholeAttacks).
var phAttacks = []struct{ name, value string }{
	{"semicolon", "; rm -rf /"}, {"substitution", "$(id)"}, {"backticks", "`id`"}, {"pipe", "|"}, {"and", "&&"},
	{"long-flag", "--force"}, {"short-flag", "-o"}, {"traversal", "../../etc/passwd"}, {"newline", "a\nid"},
	{"nul", "a\x00id"}, {"64KiB", strings.Repeat("A", 64<<10)},
	{"slash", "a/b"}, {"equals", "a=b"}, {"quote", "a'b"},
}

// phDBSnapshot is every domainlist and adlist row and the DNS file's hash.
func phDBSnapshot(t *testing.T) string {
	t.Helper()
	return phSQL(t, "SELECT * FROM domainlist ORDER BY id") + "\n" + phSQL(t, "SELECT * FROM adlist ORDER BY id") + "\n" +
		phSHA(phReadDNSFile(t))
}

// TC-133: attacks through every pihole string input of the real provider,
// the idle footprint, and the CLI's exit codes.
func TestPiholeAttacksCLIAndFootprint(t *testing.T) {
	phUseSeed(t)
	n := piholed(t, "tc133", "")
	phDNSChange(t, n, "/actions/add-zone", `{"zone":"app.lan"}`)
	phDNSChange(t, n, "/actions/add-record", `{"fqdn":"nexus-test.app.lan","ipv4":"192.168.7.88"}`)
	etag := phList[phspec.ZoneProperties](t, n, phspec.ZoneResourceType, "")[0].Properties.Etag

	targets := []phAttack{
		{"POST", "/actions/add-domain", map[string]any{"list": "deny", "kind": "exact", "value": "x.example"}, "list", false},
		{"POST", "/actions/add-domain", map[string]any{"list": "deny", "kind": "exact", "value": "x.example"}, "kind", false},
		{"POST", "/actions/add-domain", map[string]any{"list": "deny", "kind": "exact", "value": "x.example"}, "value", false},
		{"POST", "/actions/add-domain", map[string]any{"list": "deny", "kind": "regex", "value": "x"}, "value", true},
		{"POST", "/actions/add-domain", map[string]any{"list": "deny", "kind": "exact", "value": "x.example", "comment": "c"}, "comment", false},
		{"POST", "/actions/remove-domain", map[string]any{"list": "deny", "kind": "regex", "value": "x"}, "list", false},
		{"POST", "/actions/remove-domain", map[string]any{"list": "deny", "kind": "regex", "value": "x"}, "kind", false},
		{"POST", "/actions/remove-domain", map[string]any{"list": "deny", "kind": "regex", "value": "x"}, "value", true},
		{"POST", "/actions/add-adlist", map[string]any{"url": "http://127.0.0.1:8099/x.txt"}, "url", false},
		{"POST", "/actions/add-adlist", map[string]any{"url": "http://127.0.0.1:8099/x.txt", "comment": "c"}, "comment", false},
		{"POST", "/actions/remove-adlist", map[string]any{"url": "http://127.0.0.1:8099/x.txt"}, "url", false},
		{"POST", "/actions/add-zone", map[string]any{"zone": "atk.lan"}, "zone", false},
		{"POST", "/actions/add-zone", map[string]any{"zone": "atk.lan", "ifMatch": etag}, "ifMatch", false},
		{"POST", "/zones/%s/remove", map[string]any{}, "", false},
		{"POST", "/zones/app.lan/remove", map[string]any{"ifMatch": etag}, "ifMatch", false},
		{"POST", "/actions/add-record", map[string]any{"fqdn": "a.app.lan", "ipv4": "192.168.7.1"}, "fqdn", false},
		{"POST", "/actions/add-record", map[string]any{"fqdn": "a.app.lan", "ipv4": "192.168.7.1"}, "ipv4", false},
		{"POST", "/actions/add-record", map[string]any{"fqdn": "a.app.lan", "ipv4": "192.168.7.1", "ifMatch": etag}, "ifMatch", false},
		{"POST", "/records/%s/update", map[string]any{"ipv4": "192.168.7.2"}, "", false},
		{"POST", "/records/nexus-test.app.lan~A/update", map[string]any{"ipv4": "192.168.7.2"}, "ipv4", false},
		{"POST", "/records/nexus-test.app.lan~A/update", map[string]any{"ipv4": "192.168.7.2", "ifMatch": etag}, "ifMatch", false},
		{"POST", "/records/%s/remove", map[string]any{}, "", false},
		{"POST", "/records/nexus-test.app.lan~A/remove", map[string]any{"ifMatch": etag}, "ifMatch", false},
		{"POST", "/actions/apply-dns-changes", map[string]any{"changes": []string{"add-zone:atk.lan"}}, "changes", false},
		{"POST", "/actions/apply-dns-changes", map[string]any{"changes": []string{"add-zone:atk.lan"}, "ifMatch": etag}, "ifMatch", false},
		{"GET", "/domains?filter=%s", nil, "", false},
		{"GET", "/records?filter=%s", nil, "", false},
		{"GET", "/domains?cursor=%s", nil, "", false},
	}
	before := phDBSnapshot(t)
	refused, inert, segment404 := 0, 0, 0
	for _, tg := range targets {
		for _, a := range phAttacks {
			label := fmt.Sprintf("%s %s %s/%s", tg.method, tg.path, tg.field, a.name)
			path, body := tg.path, ""
			if tg.field == "" {
				esc := url.PathEscape(a.value)
				if tg.method == "GET" {
					esc = url.QueryEscape(a.value)
				}
				path = fmt.Sprintf(tg.path, esc)
			}
			if tg.body != nil {
				in := map[string]any{}
				for k, v := range tg.body {
					in[k] = v
				}
				if tg.field == "changes" {
					in["changes"] = []string{a.value}
				} else if tg.field != "" {
					in[tg.field] = a.value
				}
				raw, err := json.Marshal(in)
				if err != nil {
					t.Fatal(err)
				}
				body = string(raw)
			}
			var code int
			var b []byte
			if tg.method == "GET" {
				code, b = n.get(phAt + path)
			} else {
				code, b = n.post(phAt+path, body)
			}
			var p problem.Error
			_ = json.Unmarshal(b, &p)
			switch {
			case code == http.StatusBadRequest && p.Slug() == problem.ValidationFailed:
				refused++
			case tg.field == "" && code == http.StatusNotFound:
				// A path segment holding "/" or a NUL cannot name the route.
				segment404++
			case tg.inert && tg.path == "/actions/remove-domain" && code == http.StatusNotFound && p.Slug() == problem.NotFound:
				inert++ // Looked up as data; no such entry
			case tg.inert && code == http.StatusAccepted:
				var op apitypes.Operation
				if err := json.Unmarshal(b, &op); err != nil {
					t.Fatal(err)
				}
				res := phResult[phspec.DomainResult](t, n.waitOp(op.ID, time.Minute))
				stored := phSQLRows(t, fmt.Sprintf("SELECT domain FROM domainlist WHERE id = %d", res.Domain.Properties.PiholeID))
				if len(stored) != 1 || stored[0]["domain"] != a.value {
					t.Errorf("%s: stored %v, want the exact attack bytes", label, stored)
				}
				phResult[phspec.DomainRemoveResult](t, n.waitOp(phStart(t, n, "/actions/remove-domain",
					fmt.Sprintf(`{"id":%d}`, res.Domain.Properties.PiholeID)).ID, time.Minute))
				inert++
			default:
				t.Errorf("%s: %d %.300s; want 400 validation-failed, or inert", label, code, b)
			}
		}
	}
	phRequireNoStray(t, "the attacks")
	if after := phDBSnapshot(t); after != before {
		t.Errorf("the attacks changed gravity.db or the DNS file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	t.Logf("attacks: %d targets x %d values: %d refused 400, %d inert (stored byte for byte or looked up as data), "+
		"%d path segments that cannot route (404)", len(targets), len(phAttacks), refused, inert, segment404)

	// 3. The CLI's exit codes.
	home := phCLIHome(t, n)
	r := runNexus(home, "piholerecord", "update", "nexus-test.app.lan~A", "--ipv4", "0.0.0.0", "--yes", "-o", "json")
	t.Logf("nexus piholerecord update nexus-test.app.lan~A --ipv4 0.0.0.0: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	if r.code != cli.ExitUsage || !strings.Contains(r.stderr, "/properties/ipv4") {
		t.Errorf("record update to 0.0.0.0: exit %d, want 2 with the problem", r.code)
	}
	r = runNexus(home, "piholezone", "add", "--zone", "app.lan", "--yes", "-o", "json")
	t.Logf("nexus piholezone add --zone app.lan: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "conflict") {
		t.Errorf("zone add of an existing zone: exit %d, want 1 with the problem", r.code)
	}

	// 2. Idle RSS of nexusd with pihole enabled, after 60 s.
	pid := n.mainPID()
	var peak int64
	for range 60 {
		time.Sleep(time.Second)
		p, err := nfrReadProc(pid)
		if err != nil {
			t.Fatal(err)
		}
		peak = max(peak, p.rss)
	}
	p, err := nfrReadProc(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("NFR TC-133 nexusd (pihole enabled) idle 60 s: RSS now %.2f MiB, max %.2f MiB, VmHWM %.2f MiB",
		mib(p.rss), mib(peak), mib(p.hwm))
	if peak >= nfrRSSLimit {
		t.Errorf("idle RSS %.2f MiB, want under 50 MiB", mib(peak))
	}
}

// TC-152: FakeFTL's TestFakeFTLRestartOntoBadConfig against the real FTL:
// a restart onto an invalid address= line, Nexus not involved.
func TestPiholeRestartOntoBadConfig(t *testing.T) {
	const scratch = phDNSDir + "/99-tc152.conf"
	recovered := false
	t.Cleanup(func() {
		_ = os.Remove(scratch)
		if !recovered {
			phStartFTL(t)
		}
		phRequireFTLHealthy(t)
		phRequireNoStray(t, "TC-152")
	})
	phResetFailed(t)
	if err := os.WriteFile(scratch, []byte("address=/bad.example/999.1.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, code := phBashCode(phPihole + " restartdns")
	t.Logf("INIT=%s; pihole restartdns onto %s: exit %d %q", phInit(t), scratch, code, strings.TrimSpace(out))
	answered := false
	var firstAnswer time.Duration
	for time.Since(start) < 15*time.Second {
		if phAnswers() {
			answered, firstAnswer = true, time.Since(start)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Until(start.Add(15 * time.Second)))
	at15 := showUnit(t, phFTLUnit, "Result", "NRestarts", "ActiveState", "SubState")
	answers15 := phAnswers()
	time.Sleep(time.Until(start.Add(30 * time.Second)))
	at30 := showUnit(t, phFTLUnit, "Result", "NRestarts", "ActiveState", "SubState")
	answers30 := phAnswers()
	journal, _ := sh(30*time.Second, "journalctl", "-u", phFTLUnit, "--since", "@"+strconv.FormatInt(start.Unix()-1, 10), "--no-pager")
	t.Logf("TC-152 observed: restartdns exit %d; answered within 15 s %v (%s); at 15 s %v, answers %v; at 30 s %v, answers %v",
		code, answered, firstAnswer, at15, answers15, at30, answers30)
	t.Logf("FTL journal from the restart:\n%s", lastLines(journal, 25))
	if answered || answers15 {
		t.Errorf("FakeFTL fidelity: the real FTL answered within 15 s of a restart onto a bad address= line; "+
			"TestFakeFTLRestartOntoBadConfig models it not answering (A-63). Exit %d, %v", code, at15)
	}

	if err := os.Remove(scratch); err != nil {
		t.Fatal(err)
	}
	pid := phStartFTL(t)
	recovered = true
	t.Logf("recovered: removed %s, reset-failed, systemctl start; FTL pid %d answers; ls -a %s: %v", scratch, pid,
		phDNSDir, phDNSListing(t))
	if exists(scratch) {
		t.Errorf("%s still exists", scratch)
	}
}

func lastLines(s string, n int) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
