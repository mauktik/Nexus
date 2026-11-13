// Cases: TC-96 to TC-103, TC-E11, TC-E12, TC-ERR20 to TC-ERR25 in
// planning/pihole-dns-zones/test-cases.md, TC-149 (WI 5.5), and the WI 4.1 branch tests
// (every syntax-test failure form, the backup failure, a duplicate covering
// line).
//
// The pipeline runs against FakeHost + FakeFTL (WI 3.1/3.2), which encode the
// verified v5 behavior: dnsmasq serves a snapshot of the conf files taken at
// (re)start, `restartdns reload` does not re-read them, address= covers
// subdomains, and a local= line NXDOMAINs a whole zone. Expected answers are
// computed from the fixture with dnsfile.Expected, not typed in.
package dnswrite_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/dnsfile"
	"github.com/mauktik/nexus/internal/providers/pihole/dnswrite"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
)

const (
	piholeBin    = "/usr/local/bin/pihole"
	ftlBin       = "/usr/bin/pihole-FTL"
	systemctlBin = "/usr/bin/systemctl"
	basename     = "03-custom-dns-names.conf"
)

type env struct {
	h        *v5test.FakeHost
	m        *v5.Mutator
	file     string
	stateDir string
	opDir    string
	orig     []byte
	log      bytes.Buffer
}

// setup seeds the fake host's dnsFile with fixture bytes (the FR-23 7-line
// file unless overridden), starts FTL on it, waits until that FTL answers
// and its PID file names it (unless the seed stops it), and clears the argv
// record.
func setup(t *testing.T, content []byte) *env {
	t.Helper()
	if content == nil {
		var err error
		content, err = os.ReadFile("../dnsfile/testdata/fr23-seven-lines.conf")
		if err != nil {
			t.Fatal(err)
		}
	}
	h := v5test.NewFakeHost(t)
	e := &env{h: h, m: &v5.Mutator{Paths: h.Paths(), Runner: h, Now: h.Now, Reload: v5test.FastReload(), GravityPoll: 50 * time.Millisecond},
		file:     filepath.Join(h.Root(), "etc/dnsmasq.d", basename),
		stateDir: filepath.Join(t.TempDir(), "pihole"), opDir: t.TempDir(), orig: content}
	if err := os.WriteFile(e.file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := h.Run(context.Background(), []string{piholeBin, "restartdns"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("seed restart: %d %v", code, err)
	}
	if h.FTL().PID() != 0 { // A planted invalid line stops FTL at start, as dnsmasq dies
		if _, err := dnscheck.WaitReady(context.Background(), h.FTL().Addr(), 5*time.Second); err != nil {
			t.Fatalf("seed FTL not ready: %v", err)
		}
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if pid, ok := v5.FTLPID(h.Paths()); ok && pid == h.FTL().PID() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("seed FTL never wrote its PID file")
			}
		}
	}
	h.ResetArgvs()
	return e
}

// noTemp fails when a dot-prefixed temp file is left beside dnsFile.
func noTemp(t *testing.T, e *env) {
	t.Helper()
	for _, en := range mustReadDir(t, filepath.Dir(e.file)) {
		if strings.HasPrefix(en.Name(), ".") {
			t.Errorf("temp file %s survived", en.Name())
		}
	}
}

func (e *env) apply(t *testing.T, mut dnswrite.Mutator, ifMatch string, cs ...dnsfile.Change) (spec.DNSChangeResult, *problem.Error) {
	t.Helper()
	if mut == nil {
		mut = e.m
	}
	return dnswrite.Apply(context.Background(),
		dnswrite.Request{DNSFile: e.file, StateDir: e.stateDir, OpDir: e.opDir, IfMatch: ifMatch, Changes: cs},
		dnswrite.Deps{Mutator: mut, DNSServer: e.h.FTL().Addr(), Out: &e.log, Now: e.h.Now})
}

func (e *env) live(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) count(argv ...string) int {
	n := 0
	for _, a := range e.h.Argvs() {
		if slices.Equal(a, argv) {
			n++
		}
	}
	return n
}

func query(t *testing.T, e *env, name string) dnscheck.Answer {
	t.Helper()
	a, err := dnscheck.Query(context.Background(), e.h.FTL().Addr(), name, time.Second)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	return a
}

func etag(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustReadDir(t *testing.T, p string) []os.DirEntry {
	t.Helper()
	ents, err := os.ReadDir(p)
	if err != nil {
		t.Fatal(err)
	}
	return ents
}

var zones = []dnsfile.Change{dnsfile.AddZone{Zone: "home.local"}, dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddZone{Zone: "xi.example"}}

// TC-96: a record add runs the whole pipeline once: one syntax test, exactly
// one restart, verification of the new name against dnsfile.Expected, a
// rollback copy of the old bytes in the op directory, and a result that
// names both FTL PIDs.
func TestTC96_RecordAddHappyPath(t *testing.T) {
	e := setup(t, nil)
	pid := e.h.FTL().PID()
	res, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "nexus-test.app.lan", IPv4: "192.168.7.9"})
	if perr != nil {
		t.Fatalf("Apply: %v (log:\n%s)", perr, e.log.String())
	}
	if !res.Changed || !res.Restarted || res.Etag != etag(e.live(t)) {
		t.Errorf("result %+v", res)
	}
	if res.FTLPidBefore == nil || *res.FTLPidBefore != pid || res.FTLPidAfter == nil || *res.FTLPidAfter == pid {
		t.Errorf("pids before=%v after=%v, seed %d", res.FTLPidBefore, res.FTLPidAfter, pid)
	}
	if e.count(ftlBin, "--test") != 1 || e.count(piholeBin, "restartdns") != 1 {
		t.Errorf("argvs %q: want one --test and one restartdns (NFR-2)", e.h.Argvs())
	}
	if a := query(t, e, "nexus-test.app.lan"); len(a.A) != 1 || a.A[0] != netip.MustParseAddr("192.168.7.9") {
		t.Errorf("served %+v", a)
	}
	if len(res.Verified) != 1 || res.Verified[0].FQDN != "nexus-test.app.lan" || !res.Verified[0].Passed ||
		!slices.Equal(res.Verified[0].Expected, []string{"192.168.7.9"}) {
		t.Errorf("verified %+v", res.Verified)
	}
	if rb, err := os.ReadFile(filepath.Join(e.opDir, "dnsfile.rollback")); err != nil || !bytes.Equal(rb, e.orig) {
		t.Errorf("rollback copy: %v, equal=%v", err, bytes.Equal(rb, e.orig))
	}
	if n := strings.Count(e.log.String(), "pihole: "); n < 6 {
		t.Errorf("%d step lines in the log, want one per step:\n%s", n, e.log.String())
	}
}

// TC-97: the first write backs up the original bytes exactly once, 0600 in a
// 0700 directory outside /etc/dnsmasq.d, with metadata; later writes never
// touch it and do not report it.
func TestTC97_PreNexusBackupOnce(t *testing.T) {
	e := setup(t, nil)
	res, perr := e.apply(t, nil, "", zones...)
	if perr != nil {
		t.Fatal(perr)
	}
	bk := filepath.Join(e.stateDir, "backups", basename+".pre-nexus")
	if res.PreNexusBackup != bk {
		t.Errorf("preNexusBackup %q, want %q", res.PreNexusBackup, bk)
	}
	b, err := os.ReadFile(bk)
	if err != nil || !bytes.Equal(b, e.orig) {
		t.Fatalf("backup bytes differ from the original: %v", err)
	}
	fi := mustStat(t, bk)
	di := mustStat(t, filepath.Dir(bk))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Errorf("modes file %v dir %v, want 0600 / 0700", fi.Mode().Perm(), di.Mode().Perm())
	}
	meta, err := os.ReadFile(bk + ".json")
	if err != nil || !strings.Contains(string(meta), strings.TrimPrefix(etag(e.orig), "sha256:")) {
		t.Errorf("metadata %s %v: want the original's sha256", meta, err)
	}
	mt := fi.ModTime()
	res2, perr := e.apply(t, nil, "", dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"})
	if perr != nil {
		t.Fatal(perr)
	}
	fi2 := mustStat(t, bk)
	b2 := mustRead(t, bk)
	if res2.PreNexusBackup != "" || !fi2.ModTime().Equal(mt) || !bytes.Equal(b2, e.orig) {
		t.Errorf("second write touched or reported the backup: %q", res2.PreNexusBackup)
	}
}

// TC-98 (FR-19, NFR-3): declaring the three implied zones reorganizes the
// file into exactly the FR-23 rendering with no syntax test, no restart and
// an unchanged FTL PID.
func TestTC98_CommentOnlyNoRestart(t *testing.T) {
	e := setup(t, nil)
	pid := e.h.FTL().PID()
	res, perr := e.apply(t, nil, "", zones...)
	if perr != nil {
		t.Fatal(perr)
	}
	want := mustRead(t, "../dnsfile/testdata/fr23-seven-lines.rendered.conf")
	if !bytes.Equal(e.live(t), want) {
		t.Errorf("file\n%s\nwant FR-23 rendering\n%s", e.live(t), want)
	}
	if !res.Changed || res.Restarted || len(e.h.Argvs()) != 0 || e.h.FTL().PID() != pid {
		t.Errorf("changed=%v restarted=%v argvs=%q pid %d->%d", res.Changed, res.Restarted, e.h.Argvs(), pid, e.h.FTL().PID())
	}
}

// TC-99: a change that renders identical bytes is changed:false with no
// write (same inode and mtime) and no command.
func TestTC99_NoOpChange(t *testing.T) {
	rendered := mustRead(t, "../dnsfile/testdata/fr23-seven-lines.rendered.conf")
	e := setup(t, rendered)
	before := mustStat(t, e.file)
	res, perr := e.apply(t, nil, "", dnsfile.UpdateRecord{FQDN: "kiwi.home.local", IPv4: "192.168.7.5"})
	if perr != nil {
		t.Fatal(perr)
	}
	after := mustStat(t, e.file)
	if res.Changed || res.Restarted || len(e.h.Argvs()) != 0 || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("no-op wrote or ran something: %+v argvs %q", res, e.h.Argvs())
	}
}

// reloadInsteadOfRestart plants the wrong primitive: SIGHUP instead of a full
// restart. On real FTL (and in the fake) SIGHUP does not re-read conf files.
type reloadInsteadOfRestart struct{ *v5.Mutator }

func (r reloadInsteadOfRestart) RestartDNS(ctx context.Context, out io.Writer) error {
	return r.Mutator.FlushCache(ctx, out)
}

// TC-100 (regression: restart required to reload address= lines): with the
// restart swapped for `restartdns reload`, the new record is never served,
// so verification fails and the file is rolled back. This proves both the
// pipeline's verification and FakeFTL's fidelity.
func TestTC100_ReloadIsNotARestart(t *testing.T) {
	e := setup(t, nil)
	_, perr := e.apply(t, reloadInsteadOfRestart{e.m}, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "nexus-test.app.lan", IPv4: "192.168.7.9"})
	if perr == nil || perr.Slug() != "dns-verification-failed" {
		t.Fatalf("got %v, want dns-verification-failed: a reload does not serve new address= lines", perr)
	}
	if perr.Extensions["rolledBack"] != true || perr.Extensions["name"] != "nexus-test.app.lan" {
		t.Errorf("extensions %+v", perr.Extensions)
	}
	if !bytes.Equal(e.live(t), e.orig) {
		t.Error("live file not restored to the original bytes")
	}
}

var nonResolution = regexp.MustCompile(`(?m)^\s*(local|server|domain|rev-server)=`)

// TC-101 (CON-6): only dnsFile changes in /etc/dnsmasq.d: Pi-hole's files are
// byte-identical, no temp file survives, nothing else appears.
func TestTC101_OnlyDNSFileChanges(t *testing.T) {
	e := setup(t, nil)
	dir := filepath.Dir(e.file)
	snap := func() map[string][]byte {
		m := map[string][]byte{}
		for _, en := range mustReadDir(t, dir) {
			b := mustRead(t, filepath.Join(dir, en.Name()))
			m[en.Name()] = b
		}
		return m
	}
	before := snap()
	if _, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"}); perr != nil {
		t.Fatal(perr)
	}
	after := snap()
	if len(after) != len(before) {
		t.Errorf("entries %d -> %d: %v", len(before), len(after), keys(after))
	}
	for name, b := range before {
		if name != basename && !bytes.Equal(after[name], b) {
			t.Errorf("%s changed", name)
		}
	}
	if nonResolution.Match(e.live(t)) {
		t.Errorf("dnsFile gained a resolution-changing line:\n%s", e.live(t))
	}
}

func keys(m map[string][]byte) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	slices.Sort(k)
	return k
}

// TC-102 (regression: split-horizon xi.example): declaring xi.example and adding
// a LAN record under it leaves public names under xi.example answered by the
// upstream, never NXDOMAIN; the file gains no local=.
func TestTC102_SplitHorizonPublicNamesStillResolve(t *testing.T) {
	e := setup(t, nil)
	pub := netip.MustParseAddr("203.0.113.10")
	e.h.FTL().SetUpstream("www.xi.example", pub)
	if _, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "xi.example"}, dnsfile.AddRecord{FQDN: "nas.xi.example", IPv4: "192.168.7.20"}); perr != nil {
		t.Fatal(perr)
	}
	if a := query(t, e, "www.xi.example"); a.RCode != "NOERROR" || len(a.A) != 1 || a.A[0] != pub {
		t.Errorf("www.xi.example = %+v, want the upstream %s", a, pub)
	}
	if a := query(t, e, "nas.xi.example"); len(a.A) != 1 || a.A[0] != netip.MustParseAddr("192.168.7.20") {
		t.Errorf("nas.xi.example = %+v", a)
	}
	if nonResolution.Match(e.live(t)) {
		t.Errorf("file gained local=/server=:\n%s", e.live(t))
	}
}

// TC-103 (NFR-2): a cascade remove of a zone with two records is one
// restart and verifies every removed name.
func TestTC103_CascadeOneRestartAllNamesVerified(t *testing.T) {
	e := setup(t, nil)
	res, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "xi.example"}, dnsfile.RemoveZone{Zone: "xi.example", Cascade: true})
	if perr != nil {
		t.Fatal(perr)
	}
	if e.count(piholeBin, "restartdns") != 1 {
		t.Errorf("%d restarts", e.count(piholeBin, "restartdns"))
	}
	var names []string
	for _, v := range res.Verified {
		names = append(names, v.FQDN)
		if !v.Passed {
			t.Errorf("%+v not passed", v)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"photos.xi.example", "router.xi.example"}) {
		t.Errorf("verified %v", names)
	}
}

// TC-E11 (FR-24): removing a name still covered by a shorter-suffix line
// verifies against that line's IP.
func TestTCE11_RemoveCoveredByShorterSuffix(t *testing.T) {
	e := setup(t, []byte("# nexus:zone xi.example\naddress=/photos.xi.example/192.168.7.7\naddress=/xi.example/192.168.7.9\n"))
	res, perr := e.apply(t, nil, "", dnsfile.RemoveRecord{FQDN: "photos.xi.example"})
	if perr != nil {
		t.Fatal(perr)
	}
	if len(res.Verified) != 1 || !res.Verified[0].Passed || !slices.Equal(res.Verified[0].Observed, []string{"192.168.7.9"}) {
		t.Errorf("verified %+v, want photos.xi.example answered by the xi.example line", res.Verified)
	}
}

// TC-E12 (FR-22 step 4): the replacement keeps the original's mode, owner
// and group.
func TestTCE12_ModeAndOwnerPreserved(t *testing.T) {
	e := setup(t, nil)
	if err := os.Chmod(e.file, 0o640); err != nil {
		t.Fatal(err)
	}
	before := mustStat(t, e.file)
	if _, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}); perr != nil {
		t.Fatal(perr)
	}
	after := mustStat(t, e.file)
	if after.Mode() != before.Mode() || !sameOwner(before, after) {
		t.Errorf("mode %v -> %v, owner preserved=%v", before.Mode(), after.Mode(), sameOwner(before, after))
	}
}

// TC-ERR20: an ifMatch that differs from the current etag fails with
// dns-file-conflict before anything is written or run.
func TestTCERR20_IfMatchConflict(t *testing.T) {
	e := setup(t, nil)
	stale := "sha256:" + strings.Repeat("0", 64)
	_, perr := e.apply(t, nil, stale, dnsfile.AddZone{Zone: "app.lan"})
	if perr == nil || perr.Slug() != "dns-file-conflict" || perr.Extensions["currentEtag"] != etag(e.orig) {
		t.Fatalf("got %v", perr)
	}
	if !bytes.Equal(e.live(t), e.orig) || len(e.h.Argvs()) != 0 {
		t.Error("conflict changed the file or ran a command")
	}
}

// TC-ERR21: a hand edit between the read (step 2) and the swap (step 5) fails
// with dns-file-conflict; the hand edit survives and no temp file remains.
func TestTCERR21_HandEditRace(t *testing.T) {
	e := setup(t, nil)
	edited := append(append([]byte{}, e.orig...), "address=/hand.home.local/192.168.7.44\n"...)
	_, perr := dnswrite.Apply(context.Background(),
		dnswrite.Request{DNSFile: e.file, StateDir: e.stateDir, OpDir: e.opDir, Changes: []dnsfile.Change{dnsfile.AddZone{Zone: "app.lan"}}},
		dnswrite.Deps{Mutator: e.m, DNSServer: e.h.FTL().Addr(), Out: &e.log, Now: e.h.Now,
			BeforeSwap: func() {
				if err := os.WriteFile(e.file, edited, 0o644); err != nil {
					t.Errorf("BeforeSwap hand edit: %v", err)
				}
			}})
	if perr == nil || perr.Slug() != "dns-file-conflict" {
		t.Fatalf("got %v", perr)
	}
	if !bytes.Equal(e.live(t), edited) {
		t.Error("the hand edit was overwritten")
	}
	for _, en := range mustReadDir(t, filepath.Dir(e.file)) {
		if strings.HasPrefix(en.Name(), ".") {
			t.Errorf("temp file %s survived", en.Name())
		}
	}
}

// TC-ERR22: a failing syntax test restores the original bytes, never
// restarts, and reports preExisting by re-testing the restored file.
func TestTCERR22_ConfigTestFails(t *testing.T) {
	e := setup(t, nil)
	e.h.FailCommandOnce([]string{ftlBin, "--test"}, 1)
	_, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"})
	if perr == nil || perr.Slug() != "dns-config-test-failed" || perr.Extensions["rolledBack"] != true || perr.Extensions["preExisting"] != false {
		t.Fatalf("got %v", perr)
	}
	if !bytes.Equal(e.live(t), e.orig) || e.count(piholeBin, "restartdns") != 0 {
		t.Error("not restored, or restarted")
	}

	bad := append(append([]byte{}, e.orig...), "address=/bad.lan/999.1.1.1\n"...) // unmanaged, preserved
	e = setup(t, bad)
	_, perr = e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"})
	if perr == nil || perr.Slug() != "dns-config-test-failed" || perr.Extensions["preExisting"] != true {
		t.Fatalf("planted bad line: got %v, want preExisting=true", perr)
	}
	if !bytes.Equal(e.live(t), bad) {
		t.Error("not restored byte-identical")
	}
}

// TC-ERR23: a wrong answer after the restart rolls back: the live file equals
// the rollback copy and FTL was restarted twice.
func TestTCERR23_VerificationFailsRollsBack(t *testing.T) {
	e := setup(t, nil)
	e.h.FTL().SetFault(v5test.FaultWrongAnswer)
	restarts := e.h.FTL().Restarts()
	pid := e.h.FTL().PID()
	events := len(e.h.FTLEvents())
	_, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"})
	if perr == nil || perr.Slug() != "dns-verification-failed" || perr.Extensions["rolledBack"] != true {
		t.Fatalf("got %v", perr)
	}
	rb := mustRead(t, filepath.Join(e.opDir, "dnsfile.rollback"))
	if !bytes.Equal(e.live(t), rb) || !bytes.Equal(rb, e.orig) {
		t.Error("live file is not the rollback copy of the original")
	}
	if got := e.h.FTL().Restarts() - restarts; got != 2 {
		t.Errorf("%d restarts, want 2 (change + rollback, NFR-2)", got)
	}
	if ev := e.h.FTLEvents()[events:]; len(ev) != 2 || ev[0] != v5test.EventRestart || ev[1] != v5test.EventRestart || e.h.FTL().PID() == pid || e.h.FTL().PID() == 0 {
		t.Errorf("FTL events %v, pid %d -> %d: want two restarts, each a new PID", ev, pid, e.h.FTL().PID())
	}
	if fi := mustStat(t, filepath.Join(e.opDir, "dnsfile.rollback")); fi.Mode().Perm() != 0o600 {
		t.Errorf("rollback copy mode %v, want 0600", fi.Mode().Perm())
	}
	noTemp(t, e)
}

// TC-ERR24: a failed restart rolls back and reports pihole-command-failed
// with rolledBack:true.
func TestTCERR24_RestartFailsRollsBack(t *testing.T) {
	e := setup(t, nil)
	e.h.FailCommandOnce([]string{piholeBin, "restartdns"}, 1)
	_, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"})
	if perr == nil || perr.Slug() != "pihole-command-failed" || perr.Extensions["rolledBack"] != true {
		t.Fatalf("got %v", perr)
	}
	if !bytes.Equal(e.live(t), e.orig) {
		t.Error("not rolled back")
	}
}

// TC-ERR25: when the rollback restart also fails, dns-rollback-failed names
// the rollback copy, which holds the original bytes.
func TestTCERR25_RollbackRestartFails(t *testing.T) {
	e := setup(t, nil)
	e.h.FTL().SetFault(v5test.FaultWrongAnswer, v5test.FaultRollbackRestartFails)
	_, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"})
	copyPath := filepath.Join(e.opDir, "dnsfile.rollback")
	if perr == nil || perr.Slug() != "dns-rollback-failed" || perr.Extensions["rollbackCopy"] != copyPath {
		t.Fatalf("got %v", perr)
	}
	if rb, err := os.ReadFile(copyPath); err != nil || !bytes.Equal(rb, e.orig) {
		t.Errorf("rollback copy: %v", err)
	}
}

// TC-149 (A-60, A-61): FTL's systemd start limit is 5 starts a minute. With
// 4 starts already in the window, a record add's restart is the fifth and
// its rollback restart would be the sixth. The rollback is not refused: the
// write ends dns-verification-failed{rolledBack:true}, the live file is the
// original, FTL answers the original records, and each of the two
// restarts is preceded by `systemctl reset-failed pihole-FTL.service`.
func TestTC149_RollbackNotRefusedByStartLimit(t *testing.T) {
	e := setup(t, nil)
	e.h.Advance(time.Minute + time.Second) // The seed restart's window closes
	e.h.PrimeFTLStarts(4)
	e.h.FTL().SetFault(v5test.FaultWrongAnswer)
	_, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"})
	if perr == nil || perr.Slug() != "dns-verification-failed" || perr.Extensions["rolledBack"] != true {
		t.Fatalf("got %v %q %+v; want dns-verification-failed rolledBack:true (log:\n%s)", perr.Slug(), perr.Detail, perr.Extensions, e.log.String())
	}
	if !bytes.Equal(e.live(t), e.orig) {
		t.Error("live file is not the original")
	}
	e.h.FTL().SetFault()
	if _, err := dnscheck.WaitReady(context.Background(), e.h.FTL().Addr(), 2*time.Second); err != nil {
		t.Fatalf("FTL not answering after the rollback: %v", err)
	}
	if a := query(t, e, "unity.lan"); len(a.A) != 1 || a.A[0] != netip.MustParseAddr("192.168.7.1") {
		t.Errorf("unity.lan = %+v, want the original 192.168.7.1", a)
	}
	if a := query(t, e, "x.app.lan"); len(a.A) != 0 {
		t.Errorf("x.app.lan = %+v after the rollback, want no address", a)
	}
	reset := []string{systemctlBin, "reset-failed", "pihole-FTL.service"}
	var restarts [][]string
	for _, a := range e.h.Argvs() {
		if slices.Equal(a, reset) || slices.Equal(a, []string{piholeBin, "restartdns"}) {
			restarts = append(restarts, a)
		}
	}
	want := slices.Repeat([][]string{reset, {piholeBin, "restartdns"}}, 2)
	if !slices.EqualFunc(restarts, want, slices.Equal) {
		t.Errorf("reset-failed and restartdns argvs %q, want %q", restarts, want)
	}
}

func sameOwner(a, b os.FileInfo) bool {
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sa.Uid == sb.Uid && sa.Gid == sb.Gid
}

// The syntax-test gate (A-19) for every failure form the runner reports,
// each with preExisting false (the restored file passes the re-test) and
// true (it fails too): the FakeFTL default SIGSEGV on a bad line (139),
// SetTestExit(1) (1), and a test that cannot start (-1). Every form
// restores the original bytes and never restarts.
func TestConfigTestFailureForms(t *testing.T) {
	bad := "address=/bad.lan/999.1.1.1\n" // unmanaged, preserved by every write
	cases := []struct {
		name        string
		planted     bool
		arrange     func(h *v5test.FakeHost)
		code        int
		preExisting bool
	}{
		{"sigsegv once", false, func(h *v5test.FakeHost) { h.FailCommandSignalOnce([]string{ftlBin, "--test"}, syscall.SIGSEGV) }, 139, false},
		{"sigsegv default on a planted line", true, func(*v5test.FakeHost) {}, 139, true},
		{"exit 1 once", false, func(h *v5test.FakeHost) { h.FailCommandOnce([]string{ftlBin, "--test"}, 1) }, 1, false},
		{"exit 1 on a planted line", true, func(h *v5test.FakeHost) { h.FTL().SetTestExit(1) }, 1, true},
		{"cannot start once", false, func(h *v5test.FakeHost) { h.FailCommandStartOnce([]string{ftlBin, "--test"}) }, -1, false},
		{"cannot start", false, func(h *v5test.FakeHost) { h.FailCommandStart([]string{ftlBin, "--test"}) }, -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seed []byte
			if tc.planted {
				orig := mustRead(t, "../dnsfile/testdata/fr23-seven-lines.conf")
				seed = append(append([]byte{}, orig...), bad...)
			}
			e := setup(t, seed)
			tc.arrange(e.h)
			restarts := e.h.FTL().Restarts()
			_, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"})
			if perr == nil || perr.Slug() != "dns-config-test-failed" {
				t.Fatalf("got %v, want dns-config-test-failed", perr)
			}
			if perr.Extensions["exitCode"] != tc.code || perr.Extensions["rolledBack"] != true || perr.Extensions["preExisting"] != tc.preExisting {
				t.Errorf("extensions %+v, want exitCode %d, rolledBack, preExisting %v", perr.Extensions, tc.code, tc.preExisting)
			}
			if tc.code == -1 && !strings.Contains(perr.Detail, "could not run") {
				t.Errorf("detail %q does not say the syntax test could not run", perr.Detail)
			}
			if !bytes.Equal(e.live(t), e.orig) {
				t.Error("not restored byte-identical")
			}
			if e.count(ftlBin, "--test") != 2 || e.count(piholeBin, "restartdns") != 0 || e.h.FTL().Restarts() != restarts {
				t.Errorf("argvs %q: want the test and its re-test, and no restart", e.h.Argvs())
			}
			noTemp(t, e)
		})
	}
}

// A duplicate covering line (carry-forward of the WI 1.5 review): removing
// photos.xi.example leaves it covered by xi.example, which is on two lines, so
// dnsmasq answers both addresses. The removed address is one of them, so
// verification compares the whole set and passes.
func TestRemoveCoveredByDuplicateLine(t *testing.T) {
	e := setup(t, []byte("# nexus:zone xi.example\naddress=/photos.xi.example/192.168.7.9\n"+
		"address=/xi.example/192.168.7.9\naddress=/xi.example/192.168.7.10\n"))
	res, perr := e.apply(t, nil, "", dnsfile.RemoveRecord{FQDN: "photos.xi.example"})
	if perr != nil {
		t.Fatalf("Apply: %v (log:\n%s)", perr, e.log.String())
	}
	if len(res.Verified) != 1 || !res.Verified[0].Passed ||
		!slices.Equal(res.Verified[0].Expected, []string{"192.168.7.9", "192.168.7.10"}) {
		t.Errorf("verified %+v, want both xi.example addresses expected and passed", res.Verified)
	}
}

// An ifMatch equal to the current etag is not a conflict.
func TestIfMatchCurrentEtagApplies(t *testing.T) {
	e := setup(t, nil)
	res, perr := e.apply(t, nil, etag(e.orig), dnsfile.AddZone{Zone: "app.lan"})
	if perr != nil || !res.Changed || res.Etag != etag(e.live(t)) {
		t.Fatalf("got %+v %v", res, perr)
	}
}

// A pre-Nexus backup that cannot be made stops the write before anything
// is written or run (FR-23).
func TestBackupFailureWritesNothing(t *testing.T) {
	e := setup(t, nil)
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.stateDir, "backups"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"})
	if perr == nil {
		t.Fatal("Apply succeeded without a backup")
	}
	if !bytes.Equal(e.live(t), e.orig) || len(e.h.Argvs()) != 0 {
		t.Error("the file changed, or a command ran")
	}
	if _, err := os.Stat(filepath.Join(e.opDir, "dnsfile.rollback")); err == nil {
		t.Error("a rollback copy was written")
	}
	noTemp(t, e)
}

// The pre-Nexus backup is never overwritten: after the first write, a hand
// edit that strips every marker makes the file look unmanaged again, and the
// next write keeps the original backup untouched and does not report it.
func TestBackupNeverOverwritten(t *testing.T) {
	e := setup(t, nil)
	if _, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "app.lan"}); perr != nil {
		t.Fatal(perr)
	}
	bk := filepath.Join(e.stateDir, "backups", basename+".pre-nexus")
	fi := mustStat(t, bk)
	hand := append(append([]byte{}, e.orig...), "address=/hand.home.local/192.168.7.44\n"...)
	if err := os.WriteFile(e.file, hand, 0o644); err != nil {
		t.Fatal(err)
	}
	res, perr := e.apply(t, nil, "", dnsfile.AddZone{Zone: "home.local"})
	if perr != nil {
		t.Fatal(perr)
	}
	if res.PreNexusBackup != "" || !bytes.Equal(mustRead(t, bk), e.orig) || !mustStat(t, bk).ModTime().Equal(fi.ModTime()) {
		t.Errorf("the backup was rewritten or reported again: %q", res.PreNexusBackup)
	}
}
