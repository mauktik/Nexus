//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	phspec "github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// phDNSOp runs one DNS write to its end and checks, as TC-130 requires
// after every DNS op, that /etc/dnsmasq.d holds nothing but the seeded
// files.
func phDNSOp(t *testing.T, n *nexusdInstance, path, body string) apitypes.Operation {
	t.Helper()
	op := phRun(t, n, path, body, 3*time.Minute)
	phRequireNoStray(t, "POST "+path)
	return op
}

// phDNSChange runs a DNS write that must succeed and returns its result.
func phDNSChange(t *testing.T, n *nexusdInstance, path, body string) phspec.DNSChangeResult {
	t.Helper()
	res := phResult[phspec.DNSChangeResult](t, phDNSOp(t, n, path, body))
	t.Logf("%s: changed %v restarted %v pid %v -> %v verified %+v", path, res.Changed, res.Restarted,
		phDeref(res.FTLPidBefore), phDeref(res.FTLPidAfter), res.Verified)
	return res
}

// phDigIs reports whether dig answers name with exactly ip.
func phDigIs(t *testing.T, name, ip string) {
	t.Helper()
	if a := phDig(name); a.status != "NOERROR" || len(a.addrs) != 1 || a.addrs[0] != ip {
		t.Errorf("dig @127.0.0.1 %s: %s, commanded %s", name, a, ip)
	} else {
		t.Logf("dig @127.0.0.1 %s: %s (commanded %s)", name, a, ip)
	}
}

// phDigGone reports whether name no longer resolves locally: the stub
// upstream answers NXDOMAIN for it.
func phDigGone(t *testing.T, name string) {
	t.Helper()
	if a := phDig(name); a.status != "NXDOMAIN" {
		t.Errorf("dig @127.0.0.1 %s: %s, want NXDOMAIN after the remove", name, a)
	} else {
		t.Logf("dig @127.0.0.1 %s: %s", name, a)
	}
}

// phPIDWatch records every distinct live pihole-FTL PID the PID file names
// while it runs, polled every 20 ms.
type phPIDWatch struct {
	mu   sync.Mutex
	seen []int
	stop chan struct{}
	wg   sync.WaitGroup
}

func phWatchPIDs() *phPIDWatch {
	w := &phPIDWatch{stop: make(chan struct{})}
	w.wg.Go(func() {
		for {
			if pid, ok := phFTLPID(); ok {
				w.mu.Lock()
				if len(w.seen) == 0 || w.seen[len(w.seen)-1] != pid {
					w.seen = append(w.seen, pid)
				}
				w.mu.Unlock()
			}
			select {
			case <-w.stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	})
	return w
}

func (w *phPIDWatch) done() []int {
	close(w.stop)
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.seen)
}

// TC-127: the reload hazard on the real image; zone add with no restart;
// record add, update and remove where dig equals the commanded result; and
// a batch of three changes that costs one PID change.
func TestPiholeRecordsOnRealFTL(t *testing.T) {
	phUseSeed(t)
	n := piholed(t, "tc127", "")

	// 1. Control: a reload does not apply a new address= line.
	const ctlLine = "address=/ctl.app.lan/192.168.7.77\n"
	phWriteDNSFile(t, append(phSeedBytes(t), ctlLine...))
	mustSh(t, phPihole, "restartdns", "reload")
	time.Sleep(time.Second)
	ctl := phDig("ctl.app.lan")
	t.Logf("control: appended %q, pihole restartdns reload; dig ctl.app.lan %s", strings.TrimSpace(ctlLine), ctl)
	if slices.Contains(ctl.addrs, "192.168.7.77") {
		t.Errorf("control: the reload applied the new line (%s); the hazard did not reproduce", ctl)
	}
	phWriteDNSFile(t, phSeedBytes(t))
	phRestartFTL(t)

	// 2. zone add: comment lines only, no restart, the marker present.
	pid0, _ := phFTLPID()
	res := phDNSChange(t, n, "/actions/add-zone", `{"zone":"app.lan"}`)
	pid1, _ := phFTLPID()
	file := string(phReadDNSFile(t))
	t.Logf("zone add app.lan: FTL PID %d -> %d; file:\n%s", pid0, pid1, file)
	if pid1 != pid0 || res.Restarted || !strings.Contains(file, "# nexus:zone app.lan\n") ||
		res.FTLPidBefore == nil || *res.FTLPidBefore != pid0 || res.FTLPidAfter == nil || *res.FTLPidAfter != pid0 {
		t.Errorf("zone add: PID %d -> %d, restarted %v, result pids %v -> %v, marker present %v", pid0, pid1, res.Restarted,
			phDeref(res.FTLPidBefore), phDeref(res.FTLPidAfter), strings.Contains(file, "# nexus:zone app.lan"))
	}
	phDigIs(t, "photon.app.lan", "192.168.7.5")

	// 3. record add, update and remove; the record answers for its
	// subdomains too.
	res = phDNSChange(t, n, "/actions/add-record", `{"fqdn":"nexus-test.app.lan","ipv4":"192.168.7.88"}`)
	if pid, _ := phFTLPID(); !res.Restarted || res.FTLPidAfter == nil || *res.FTLPidAfter != pid || pid == pid1 {
		t.Errorf("record add: restarted %v, ftlPidAfter %v, PID file %d (was %d)", res.Restarted, phDeref(res.FTLPidAfter), pid, pid1)
	}
	phDigIs(t, "nexus-test.app.lan", "192.168.7.88")
	phDigIs(t, "a.nexus-test.app.lan", "192.168.7.88")
	phDNSChange(t, n, "/records/nexus-test.app.lan~A/update", `{"ipv4":"192.168.7.89"}`)
	phDigIs(t, "nexus-test.app.lan", "192.168.7.89")
	phDigIs(t, "a.nexus-test.app.lan", "192.168.7.89")
	phDNSChange(t, n, "/records/nexus-test.app.lan~A/remove", "{}")
	phDigGone(t, "nexus-test.app.lan")
	phDigGone(t, "a.nexus-test.app.lan")
	if strings.Contains(string(phReadDNSFile(t)), "nexus-test") {
		t.Errorf("the removed record is still in the file:\n%s", phReadDNSFile(t))
	}

	// 4. A batch of three changes restarts FTL once.
	// One restart is one systemd start of the unit. Under INIT=sysv the
	// SysV script's FTL forks to daemonize, so the PID file can name the
	// pre-fork PID briefly: the PIDs seen are logged, the starts counted.
	time.Sleep(1100 * time.Millisecond) // journalctl --since has one-second resolution
	since := time.Now()
	w := phWatchPIDs()
	res = phDNSChange(t, n, "/actions/apply-dns-changes",
		`{"changes":["add-record:b1.app.lan=192.168.7.91","add-record:b2.app.lan=192.168.7.92","update-record:photon.app.lan=192.168.7.95"]}`)
	pids := w.done()
	journal := mustSh(t, "journalctl", "-u", phFTLUnit, "--since", "@"+strconv.FormatInt(since.Unix(), 10), "--no-pager", "-o", "cat")
	starts := 0
	for line := range strings.Lines(journal) {
		if strings.HasPrefix(line, "Starting ") {
			starts++
		}
	}
	t.Logf("batch: %d unit start(s) in the journal; FTL PIDs seen during the op %v; result %v -> %v", starts, pids,
		phDeref(res.FTLPidBefore), phDeref(res.FTLPidAfter))
	if starts != 1 || len(pids) < 2 || res.FTLPidBefore == nil || res.FTLPidAfter == nil || *res.FTLPidBefore != pids[0] ||
		*res.FTLPidAfter != pids[len(pids)-1] || len(res.Verified) != 3 {
		t.Errorf("batch of 3: %d starts, PIDs %v, result %v -> %v, %d verified; want exactly one restart", starts, pids,
			phDeref(res.FTLPidBefore), phDeref(res.FTLPidAfter), len(res.Verified))
	}
	if phInit(t) == "native" && len(pids) != 2 {
		t.Errorf("batch of 3 under the native unit: PIDs %v, want exactly one PID change", pids)
	}
	phDigIs(t, "b1.app.lan", "192.168.7.91")
	phDigIs(t, "b2.app.lan", "192.168.7.92")
	phDigIs(t, "photon.app.lan", "192.168.7.95")
}

// phResolutionLines is every resolution-changing option line in
// /etc/dnsmasq.d, as grep reads them.
func phResolutionLines(t *testing.T) string {
	t.Helper()
	out, code := phBashCode(`grep -E '^(local|server|domain|rev-server)=' /etc/dnsmasq.d/*`)
	if code != 0 && code != 1 {
		t.Fatalf("grep: exit %d %s", code, out)
	}
	return out
}

// TC-128: split-horizon. A zone named like a public domain does not
// capture the names it has no record for: www.xi.example is still forwarded.
func TestPiholeSplitHorizon(t *testing.T) {
	phUseSeed(t)
	n := piholed(t, "tc128", "")
	lines0 := phResolutionLines(t)
	phDigIs(t, phSplit, phSplitIP)

	phDNSChange(t, n, "/actions/add-zone", `{"zone":"xi.example"}`)
	since := time.Now()
	phDNSChange(t, n, "/actions/add-record", `{"fqdn":"nas.xi.example","ipv4":"192.168.7.30"}`)
	phDigIs(t, phSplit, phSplitIP)
	phDigIs(t, "nas.xi.example", "192.168.7.30")
	phDigIs(t, "router.xi.example", "192.168.7.1")
	lines1 := phResolutionLines(t)
	stub, _ := sh(30*time.Second, "journalctl", "-u", "stub-upstream.service", "--since",
		"@"+strconv.FormatInt(since.Unix(), 10), "--no-pager")
	t.Logf("resolution lines before:\n%safter:\n%s", lines0, lines1)
	t.Logf("stub upstream since the record add:\n%s", stub)
	if lines1 != lines0 {
		t.Errorf("resolution-changing lines changed:\nbefore:\n%s\nafter:\n%s", lines0, lines1)
	}
	if !strings.Contains(stub, "query[A] "+phSplit+" from 127.0.0.1") {
		t.Errorf("the stub upstream did not see %s after the writes: FTL did not forward it", phSplit)
	}
	if strings.Contains(stub, "nas.xi.example") {
		t.Errorf("nas.xi.example reached the upstream; FTL should answer it locally")
	}
}

// TC-130: the first write's pre-Nexus backup holds the original bytes, at
// 0600 in a 0700 directory, root-owned; a later write keeps it.
func TestPiholeFirstWriteBackup(t *testing.T) {
	phUseSeed(t)
	n := piholed(t, "tc130", "")
	orig := phReadDNSFile(t)
	origSHA := phSHA(orig)
	if origSHA != phSeedSHA {
		t.Fatalf("the DNS file is not the FR-23 seed: sha256 %s", origSHA)
	}

	res := phDNSChange(t, n, "/actions/add-zone", `{"zone":"home.local"}`)
	want := filepath.Join(nexusdState, "nexusd-tc130", "pihole", "backups", "03-custom-dns-names.conf.pre-nexus")
	if res.PreNexusBackup != want {
		t.Fatalf("preNexusBackup %q, want %q", res.PreNexusBackup, want)
	}
	sum := strings.Fields(mustSh(t, "sha256sum", want))[0]
	stat := mustSh(t, "stat", "-c", "%n %a %U:%G", want, want+".json", filepath.Dir(want))
	t.Logf("sha256sum %s: %s (original %s)\n%s", want, sum, origSHA, stat)
	if sum != origSHA {
		t.Errorf("backup sha256 %s, original %s", sum, origSHA)
	}
	for path, mode := range map[string]os.FileMode{want: 0o600, want + ".json": 0o600, filepath.Dir(want): 0o700} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if fi.Mode().Perm() != mode || st.Uid != 0 || st.Gid != 0 {
			t.Errorf("%s: %v %d:%d, want %v root:root", path, fi.Mode().Perm(), st.Uid, st.Gid, mode)
		}
	}
	var meta map[string]any
	b, err := os.ReadFile(want + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s.json: %s", want, b)
	if fmt.Sprint(meta["sha256"]) != origSHA || fmt.Sprint(meta["size"]) != fmt.Sprint(len(orig)) {
		t.Errorf("backup metadata %v, original sha256 %s size %d", meta, origSHA, len(orig))
	}

	res = phDNSChange(t, n, "/actions/add-zone", `{"zone":"app.lan"}`)
	if again := strings.Fields(mustSh(t, "sha256sum", want))[0]; res.PreNexusBackup != "" || again != origSHA {
		t.Errorf("second write: preNexusBackup %q, backup sha256 %s; want none and the backup kept", res.PreNexusBackup, again)
	}
}
