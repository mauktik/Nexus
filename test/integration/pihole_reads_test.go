//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/problem"
	phspec "github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// The real Pi-hole v5 of the harness image (WI 5.1, WI 5.2): nexusd runs
// under the shipped unit with the pihole provider enabled, against FTL
// v5.25.2 under its native systemd unit (or, with INIT=sysv, the SysV
// script). Every expected value comes from the host itself: pihole,
// pihole-FTL sqlite3, dig @127.0.0.1, FTL's text API, the PID file and
// /proc, and the DNS file's bytes; never from Nexus (NFR-7).
//
// FTL start-limit hygiene (A-60): the native unit allows 5 starts per 60 s.
// Nexus resets the counter before each of its own restarts (WI 5.5); every
// start or restart a test makes directly goes through phResetFailed first,
// and every test that restarts or stops FTL ends with phRequireFTLHealthy,
// so a limit hit cannot leak into the next test.

const (
	phPihole      = "/usr/local/bin/pihole"
	phFTLBin      = "/usr/bin/pihole-FTL"
	phFTLUnit     = "pihole-FTL.service"
	phGravityDB   = "/etc/pihole/gravity.db"
	phSetupVars   = "/etc/pihole/setupVars.conf"
	phVersions    = "/etc/pihole/versions"
	phPIDFile     = "/run/pihole-FTL.pid"
	phDNSDir      = "/etc/dnsmasq.d"
	phDNSFile     = phDNSDir + "/03-custom-dns-names.conf"
	phInitFile    = "/etc/pihole-spike-init"
	phAt          = "/v1/providers/pihole"
	phFixtureRoot = "/srv/nexus-harness"
	phFixtureURL  = "http://127.0.0.1:8099"
	// phBlocked is on the harness baseline adlist: gravity blocks it.
	phBlocked = "baseline-one.example.com"
	// phSplit is the stub upstream's one public answer (TC-128).
	phSplit   = "www.xi.example"
	phSplitIP = "203.0.113.10"
	// phSeedSHA is the sha256 of the FR-23 seven-line seed run.sh installs
	// as the DNS file (PROVENANCE, DNSFILE SEED).
	phSeedSHA = "72772cf0139e932a5bad045262d3166ac7e66e20a4550babdfb1c58f95493bd6"
)

// phSeed is internal/providers/pihole/dnsfile/testdata/fr23-seven-lines.conf,
// the bytes run.sh installs; phSeedBytes checks it against phSeedSHA.
const phSeed = "address=/unity.local/192.168.7.1\n" +
	"address=/photon.home.local/192.168.7.5\n" +
	"address=/kiwi.home.local/192.168.7.5\n" +
	"address=/router.xi.example/192.168.7.1\n" +
	"address=/unity.lan/192.168.7.1\n" +
	"address=/photon.app.lan/192.168.7.5\n" +
	"address=/photos.xi.example/192.168.7.7\n"

// phSeedFiles are the files run.sh leaves in /etc/dnsmasq.d.
var phSeedFiles = []string{".", "..", "01-pihole.conf", "03-custom-dns-names.conf", "06-rfc6761.conf"}

func phSeedBytes(t *testing.T) []byte {
	t.Helper()
	if got := phSHA([]byte(phSeed)); got != phSeedSHA {
		t.Fatalf("the test's FR-23 seed has sha256 %s, PROVENANCE records %s", got, phSeedSHA)
	}
	return []byte(phSeed)
}

func phSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// piholed is a real nexusd under the shipped unit with the pihole provider
// enabled (and the extra provider lines), and a token with every pihole
// scope.
func piholed(t *testing.T, name, extra string) *nexusdInstance {
	t.Helper()
	n := newNexusd(t, name, "  pihole:\n    enabled: true\n"+extra)
	tok, _, err := auth.Create(filepath.Join(nexusdState, "nexusd-"+name), auth.CreateRequest{Name: "pihole",
		Scopes: []string{phspec.ReadScope, phspec.WriteScope, phspec.DNSScope}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	return n
}

// phOpsDir is the operations' directory of piholed(t, name, ...).
func phOpsDir(name string) string { return filepath.Join(nexusdState, "nexusd-"+name, "ops") }

// phStart posts a pihole mutation and requires 202.
func phStart(t *testing.T, n *nexusdInstance, path, body string) apitypes.Operation {
	t.Helper()
	code, b := n.post(phAt+path, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST %s %s: %d %s", path, body, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		t.Fatal(err)
	}
	return op
}

// phRun runs a pihole mutation to its end.
func phRun(t *testing.T, n *nexusdInstance, path, body string, timeout time.Duration) apitypes.Operation {
	t.Helper()
	op := n.waitOp(phStart(t, n, path, body).ID, timeout)
	t.Logf("POST %s %s: %s in %s", path, body, op.Status, phTook(op))
	return op
}

func phTook(op apitypes.Operation) time.Duration {
	if op.Started == nil || op.Ended == nil {
		return 0
	}
	return op.Ended.Sub(*op.Started).Round(time.Millisecond)
}

// phResult requires op Succeeded and decodes its result.
func phResult[T any](t *testing.T, op apitypes.Operation) T {
	t.Helper()
	var out T
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s %s ended %s: %+v", op.ID, op.Action, op.Status, op.Error)
	}
	if err := json.Unmarshal(op.Result, &out); err != nil {
		t.Fatalf("result of %s: %v\n%s", op.ID, err, op.Result)
	}
	return out
}

// phFailed requires op Failed with slug and returns its problem.
func phFailed(t *testing.T, op apitypes.Operation, slug problem.Slug) *problem.Error {
	t.Helper()
	if op.Status != apitypes.StatusFailed || op.Error == nil || op.Error.Slug() != slug {
		t.Fatalf("operation %s %s ended %s with %+v, want Failed %s", op.ID, op.Action, op.Status, op.Error, slug)
	}
	return op.Error
}

// phList reads every page of a pihole list.
func phList[T any](t *testing.T, n *nexusdInstance, typ, query string) []apitypes.Resource[T] {
	t.Helper()
	var all []apitypes.Resource[T]
	cursor := ""
	for range 1000 {
		q, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		path := phAt + "/" + typ
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		var page apitypes.List[apitypes.Resource[T]]
		n.mustGet(path, &page)
		all = append(all, page.Items...)
		if cursor = page.NextCursor; cursor == "" {
			return all
		}
	}
	t.Fatalf("%s: more than 1000 pages", typ)
	return nil
}

// phOne is the one item of a singleton list (status, summary, blocking).
func phOne[T any](t *testing.T, n *nexusdInstance, typ string) T {
	t.Helper()
	items := phList[T](t, n, typ, "")
	if len(items) != 1 || items[0].Name != phspec.SingletonName {
		t.Fatalf("%s: %d items, want the one named %s", typ, len(items), phspec.SingletonName)
	}
	return items[0].Properties
}

// phBash runs a bash script on the host and requires exit 0.
func phBash(t *testing.T, script string) string {
	t.Helper()
	out, code := phBashCode(script)
	if code != 0 {
		t.Fatalf("bash -c %q: exit %d\n%s", script, code, out)
	}
	return out
}

// phBashCode runs a bash script on the host and returns its combined
// output and exit code (-1 when it could not run or timed out).
func phBashCode(script string) (string, int) {
	out, err := sh(3*time.Minute, "bash", "-c", script)
	if err == nil {
		return out, 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode()
	}
	return out + err.Error(), -1
}

// phSQL runs a read-only query with pihole-FTL sqlite3 and returns its
// output, trimmed.
func phSQL(t *testing.T, query string) string {
	t.Helper()
	return strings.TrimSpace(mustSh(t, phFTLBin, "sqlite3", "-readonly", phGravityDB, query))
}

// phSQLWrite runs statements on gravity.db with pihole-FTL sqlite3, the
// shell Pi-hole itself uses (foreign keys on).
func phSQLWrite(t *testing.T, stmts string) {
	t.Helper()
	mustSh(t, phFTLBin, "sqlite3", phGravityDB, stmts)
}

// phSQLRows decodes a read-only query's rows with pihole-FTL sqlite3 -json.
func phSQLRows(t *testing.T, query string) []map[string]any {
	t.Helper()
	out := strings.TrimSpace(mustSh(t, phFTLBin, "sqlite3", "-readonly", "-json", phGravityDB, query))
	if out == "" {
		return nil
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("pihole-FTL sqlite3 -json %q: %v\n%s", query, err, out)
	}
	return rows
}

// phInfo is info.<property> as pihole-FTL sqlite3 reads it.
func phInfo(t *testing.T, property string) string {
	t.Helper()
	return phSQL(t, "SELECT value FROM info WHERE property = '"+property+"'")
}

// phFTLPID is the PID file's PID and whether /proc names it pihole-FTL.
func phFTLPID() (int, bool) {
	b, err := os.ReadFile(phPIDFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return pid, err == nil && strings.TrimSpace(string(comm)) == "pihole-FTL"
}

// phAnswer is one dig @127.0.0.1 answer.
type phAnswer struct {
	status string // NOERROR, NXDOMAIN, ...; empty when nothing answered
	addrs  []string
}

func (a phAnswer) String() string {
	if a.status == "" {
		return "no answer"
	}
	return a.status + " " + fmt.Sprint(a.addrs)
}

// phDig asks FTL on 127.0.0.1:53 for name's A records with dig.
func phDig(name string) phAnswer {
	out, _ := sh(10*time.Second, "dig", "+time=1", "+tries=1", "@127.0.0.1", name, "A", "+noall", "+answer", "+comments")
	var a phAnswer
	for line := range strings.Lines(out) {
		if _, rest, ok := strings.Cut(line, "status: "); ok {
			a.status = strings.TrimRight(strings.Fields(rest)[0], ",")
		}
		if f := strings.Fields(line); len(f) == 5 && f[2] == "IN" && f[3] == "A" {
			a.addrs = append(a.addrs, f[4])
		}
	}
	return a
}

// phAnswers reports whether FTL answers pi.hole.
func phAnswers() bool {
	a := phDig("pi.hole")
	return a.status == "NOERROR" && len(a.addrs) > 0
}

// phWaitFTL waits until a pihole-FTL other than old is named by the PID
// file and answers, and returns its PID.
func phWaitFTL(t *testing.T, timeout time.Duration, old int) int {
	t.Helper()
	var pid int
	waitUntil(t, timeout, "pihole-FTL answering", func() (bool, string) {
		p, ok := phFTLPID()
		pid = p
		return ok && p != old && phAnswers(), fmt.Sprintf("pid %d (%v), old %d", p, ok, old)
	})
	return pid
}

// phResetFailed is the one place a test clears FTL's start-limit counter:
// every start or restart a test makes directly runs it first (A-60).
func phResetFailed(t *testing.T) {
	t.Helper()
	mustSh(t, "systemctl", "reset-failed", phFTLUnit)
}

// phRestartFTL restarts FTL directly, outside Nexus, with `pihole
// restartdns`, and waits for the new FTL to answer.
func phRestartFTL(t *testing.T) int {
	t.Helper()
	old, _ := phFTLPID()
	phResetFailed(t)
	if out, code := phBashCode(phPihole + " restartdns"); code != 0 {
		t.Fatalf("pihole restartdns: exit %d\n%s", code, out)
	}
	return phWaitFTL(t, time.Minute, old)
}

// phStartFTL starts a stopped FTL directly and waits for it to answer.
func phStartFTL(t *testing.T) int {
	t.Helper()
	phResetFailed(t)
	mustSh(t, "systemctl", "start", phFTLUnit)
	return phWaitFTL(t, time.Minute, 0)
}

// phFTLDiag logs what FTL itself says when an answer is not what a test
// expected: its PID, the unit, and the tails of FTL.log and pihole.log.
func phFTLDiag(t *testing.T) {
	t.Helper()
	pid, ok := phFTLPID()
	out, _ := phBashCode("systemctl show -p Result,ActiveState,SubState,NRestarts " + phFTLUnit +
		"; tail -n 30 /var/log/pihole/FTL.log; tail -n 30 /var/log/pihole/pihole.log")
	t.Logf("FTL diagnostics: PID file %d (pihole-FTL %v)\n%s", pid, ok, out)
}

// phRequireFTLHealthy ends every test that restarts or stops FTL: the
// unit's Result is success and FTL answers.
func phRequireFTLHealthy(t *testing.T) {
	t.Helper()
	waitUntil(t, 30*time.Second, "pihole-FTL Result=success and answering", func() (bool, string) {
		r := showUnit(t, phFTLUnit, "Result")["Result"]
		return r == "success" && phAnswers(), "Result=" + r
	})
}

// phInit is the harness image's INIT (native or sysv).
func phInit(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(phInitFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// phDNSListing is `ls -a /etc/dnsmasq.d`.
func phDNSListing(t *testing.T) []string {
	t.Helper()
	return strings.Fields(mustSh(t, "ls", "-a", phDNSDir))
}

// phRequireNoStray requires /etc/dnsmasq.d to hold only the seeded files
// (TC-130).
func phRequireNoStray(t *testing.T, after string) {
	t.Helper()
	if got := phDNSListing(t); !slices.Equal(got, phSeedFiles) {
		t.Errorf("after %s, ls -a %s: %v, want only %v", after, phDNSDir, got, phSeedFiles)
	}
}

// phWriteDNSFile replaces the DNS file's bytes in place, root:root 0644,
// as a hand edit would.
func phWriteDNSFile(t *testing.T, b []byte) {
	t.Helper()
	if err := os.WriteFile(phDNSFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(phDNSFile, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(phDNSFile, 0o644); err != nil {
		t.Fatal(err)
	}
}

func phReadDNSFile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(phDNSFile)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// phUseSeed starts the test from the FR-23 seed, served by FTL, and puts it
// back at cleanup, restarting FTL when the file had changed so FTL serves
// the seed again for the next test.
func phUseSeed(t *testing.T) {
	t.Helper()
	seed := phSeedBytes(t)
	restore := func() {
		if string(phReadDNSFile(t)) != phSeed {
			phWriteDNSFile(t, seed)
			pid := phRestartFTL(t)
			t.Logf("restored the FR-23 seed as %s; FTL restarted (pid %d)", phDNSFile, pid)
		}
		phRequireNoStray(t, "restoring the seed")
	}
	restore()
	t.Cleanup(func() {
		restore()
		phRequireFTLHealthy(t)
	})
}

// phDropAdlist removes an adlist with pihole-FTL sqlite3, its gravity rows
// and group links first (A-58), and reloads FTL's lists. It is cleanup
// only; nothing is asserted through it.
func phDropAdlist(t *testing.T, address string) {
	t.Helper()
	q := strings.ReplaceAll(address, "'", "''")
	phSQLWrite(t, "BEGIN IMMEDIATE; "+
		"DELETE FROM gravity WHERE adlist_id IN (SELECT id FROM adlist WHERE address = '"+q+"'); "+
		"DELETE FROM adlist_by_group WHERE adlist_id IN (SELECT id FROM adlist WHERE address = '"+q+"'); "+
		"DELETE FROM adlist WHERE address = '"+q+"'; COMMIT;")
	mustSh(t, phPihole, "restartdns", "reload-lists")
}

// phVersionsFile is /etc/pihole/versions as KEY=VALUE.
func phVersionsFile(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(phVersions)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for line := range strings.Lines(string(b)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}

// phSetupVar is one setupVars.conf value.
func phSetupVar(t *testing.T, key string) string {
	t.Helper()
	b, err := os.ReadFile(phSetupVars)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return v
		}
	}
	return ""
}

// phFTLStats is FTL's own `>stats` over its text API, read with nc.
func phFTLStats(t *testing.T) map[string]string {
	t.Helper()
	out := mustSh(t, "bash", "-c", `printf '>stats\n>quit\n' | nc -q 2 127.0.0.1 4711`)
	m := map[string]string{}
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	if m["domains_being_blocked"] == "" {
		t.Fatalf(">stats: %q", out)
	}
	return m
}

func phCapability(t *testing.T, n *nexusdInstance, name string) apitypes.ProviderCapability {
	t.Helper()
	var caps apitypes.Capabilities
	n.mustGet("/v1/capabilities?refresh=true", &caps)
	for _, p := range caps.Providers {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no %s provider in capabilities", name)
	return apitypes.ProviderCapability{}
}

func phUnix(t *testing.T, v any) time.Time {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%v is not a unix time", v)
	}
	return time.Unix(int64(f), 0).UTC()
}

func phInt(v any) *int64 {
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	n := int64(f)
	return &n
}

func phStr(v any) string {
	s, _ := v.(string)
	return s
}

func phDeref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// The host's own domainlist types and adlist statuses (gravity.db schema,
// list.sh: 0 allow exact, 1 deny exact, 2 allow regex, 3 deny regex).
var (
	phTypes    = [][2]string{{"allow", "exact"}, {"deny", "exact"}, {"allow", "regex"}, {"deny", "regex"}}
	phStatuses = []string{"notDownloaded", "updated", "unchanged", "failedCached", "failedNoCache"}
)

// phHostDomains is every domainlist row, read with pihole-FTL sqlite3.
func phHostDomains(t *testing.T) map[string]phspec.DomainProperties {
	t.Helper()
	out := map[string]phspec.DomainProperties{}
	for _, r := range phSQLRows(t, "SELECT id, type, domain, enabled, date_added, date_modified, comment FROM domainlist") {
		lk := phTypes[int(r["type"].(float64))]
		id := *phInt(r["id"])
		out[strconv.FormatInt(id, 10)] = phspec.DomainProperties{PiholeID: id, List: lk[0], Kind: lk[1],
			Value: phStr(r["domain"]), Enabled: r["enabled"].(float64) != 0, Comment: phStr(r["comment"]),
			Added: phUnix(t, r["date_added"]), Modified: phUnix(t, r["date_modified"])}
	}
	return out
}

// phHostAdlists is every adlist row, read with pihole-FTL sqlite3.
func phHostAdlists(t *testing.T) map[string]phspec.AdlistProperties {
	t.Helper()
	out := map[string]phspec.AdlistProperties{}
	for _, r := range phSQLRows(t, "SELECT id, address, enabled, date_added, date_modified, comment, date_updated, number, status FROM adlist") {
		id := *phInt(r["id"])
		a := phspec.AdlistProperties{PiholeID: id, URL: phStr(r["address"]), Enabled: r["enabled"].(float64) != 0,
			Comment: phStr(r["comment"]), Added: phUnix(t, r["date_added"]), Modified: phUnix(t, r["date_modified"]),
			Domains: phInt(r["number"])}
		if s := phInt(r["status"]); s != nil && *s >= 0 && int(*s) < len(phStatuses) {
			a.Status = &phStatuses[*s]
		}
		if r["date_updated"] != nil {
			u := phUnix(t, r["date_updated"])
			a.LastUpdated = &u
		}
		out[strconv.FormatInt(id, 10)] = a
	}
	return out
}

// phFileRecord is a record as the file's own text says: an
// address=/<name>/<ipv4> line, its zone the longest `# nexus:zone` suffix.
type phFileRecord struct {
	ipv4  string
	zone  string
	lines int
}

var phRecordLine = regexp.MustCompile(`^address=/([a-z0-9.-]+)/([0-9.]+)$`)

// phParseFile reads a DNS file's zones and records from its text, for the
// simple files these tests write: no unmanaged address forms.
func phParseFile(b []byte) (zones []string, records map[string]phFileRecord) {
	records = map[string]phFileRecord{}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\n")
		if z, ok := strings.CutPrefix(line, "# nexus:zone "); ok {
			zones = append(zones, z)
		}
	}
	for line := range strings.Lines(string(b)) {
		m := phRecordLine.FindStringSubmatch(strings.TrimRight(line, "\n"))
		if m == nil {
			continue
		}
		r := records[m[1]]
		r.ipv4, r.lines = m[2], r.lines+1
		for _, z := range zones {
			if (m[1] == z || strings.HasSuffix(m[1], "."+z)) && len(z) > len(r.zone) {
				r.zone = z
			}
		}
		records[m[1]] = r
	}
	slices.Sort(zones)
	return zones, records
}

// phTC121File is a DNS file with two zones, an unzoned section, a comment
// and a duplicate, written for the read checks; FTL never loads it.
const phTC121File = "# operator comment\n\n" +
	"# nexus:zone app.lan\naddress=/photon.app.lan/192.168.7.5\naddress=/z.app.lan/192.168.7.6\n\n" +
	"# nexus:zone home.local\naddress=/kiwi.home.local/192.168.7.5\naddress=/nas.lab.home.local/192.168.7.9\n\n" +
	"# nexus:unzoned\naddress=/unity.lan/192.168.7.1\naddress=/dup.lan/192.168.7.2\naddress=/dup.lan/192.168.7.3\n"

// TC-121: every read equals the host's own view.
func TestPiholeReadsEqualHostView(t *testing.T) {
	phUseSeed(t)
	n := piholed(t, "tc121", "")

	// Seed one row of each domain type and two adlists gravity never ran
	// for, with the host's own shell.
	phSQLWrite(t, "BEGIN; "+
		"INSERT INTO domainlist (type, domain, enabled, comment) VALUES "+
		"(0, 'tc121-allow.example', 1, 'tc121 allow'), (1, 'tc121-deny.example', 0, 'tc121 deny'), "+
		`(2, '(^|\.)tc121\.example$', 1, 'tc121 allow regex'), (3, '^ad[s]?\.tc121;querytype=A', 1, NULL); `+
		"INSERT INTO adlist (address, enabled, comment) VALUES ('http://127.0.0.1:8099/tc121-a.txt', 1, 'tc121 a'), "+
		"('http://127.0.0.1:8099/tc121-b.txt', 0, NULL); COMMIT;")
	t.Cleanup(func() {
		phSQLWrite(t, "BEGIN; DELETE FROM domainlist WHERE domain LIKE '%tc121%'; "+
			"DELETE FROM adlist WHERE address LIKE '%tc121%'; COMMIT;")
	})

	// Status: versions file, PID file and /proc, dig, setupVars, the
	// re-enable timer and gravity.db's info table.
	vf := phVersionsFile(t)
	pid, running := phFTLPID()
	st := phOne[phspec.StatusProperties](t, n, phspec.StatusResourceType)
	t.Logf("status: %+v", st)
	t.Logf("host: CORE_VERSION=%s WEB_VERSION=%s FTL_VERSION=%s, PID file %d (pihole-FTL %v), BLOCKING_ENABLED=%s, "+
		"gravity_count=%s updated=%s", vf["CORE_VERSION"], vf["WEB_VERSION"], vf["FTL_VERSION"], pid, running,
		phSetupVar(t, "BLOCKING_ENABLED"), phInfo(t, "gravity_count"), phInfo(t, "updated"))
	pstatus := phBash(t, phPihole+" status")
	t.Logf("pihole status:\n%s", pstatus)
	updated, err := strconv.ParseInt(phInfo(t, "updated"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.ParseInt(phInfo(t, "gravity_count"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	wantBlocking := map[string]string{"true": phspec.BlockingEnabled, "false": phspec.BlockingDisabled}[phSetupVar(t, "BLOCKING_ENABLED")]
	timer := showUnit(t, "nexus-pihole-reenable.timer", "SubState")["SubState"]
	for _, c := range []struct {
		field     string
		got, want any
	}{
		{"backend", st.Backend, phspec.BackendV5CLI},
		{"coreVersion", phDeref(st.CoreVersion), vf["CORE_VERSION"]},
		{"webVersion", phDeref(st.WebVersion), vf["WEB_VERSION"]},
		{"ftlVersion", phDeref(st.FTLVersion), vf["FTL_VERSION"]},
		{"ftlRunning", phDeref(st.FTLRunning), running},
		{"dnsAnswering", phDeref(st.DNSAnswering), phAnswers()},
		{"blocking", st.Blocking, wantBlocking},
		{"reenableAt (timer " + timer + ")", st.ReenableAt == nil, timer != "waiting"},
		{"gravityDomains", phDeref(st.GravityDomains), count},
		{"gravityUpdated", phDeref(st.GravityUpdated), time.Unix(updated, 0).UTC()},
	} {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("status %s: Nexus %v, host %v", c.field, c.got, c.want)
		}
	}
	if !strings.Contains(pstatus, "FTL is listening on port 53") || (wantBlocking == phspec.BlockingEnabled) !=
		strings.Contains(pstatus, "Pi-hole blocking is enabled") {
		t.Errorf("pihole status disagrees with the host files: %s", pstatus)
	}

	// Summary: FTL's own >stats. The counters move with every query, so
	// the two reads are retried until they agree.
	var sum phspec.SummaryProperties
	var stats map[string]string
	agree := func() []string {
		var diffs []string
		for _, c := range []struct {
			field string
			got   any
			key   string
		}{
			{"queriesToday", phDeref(sum.QueriesToday), "dns_queries_today"},
			{"blockedToday", phDeref(sum.BlockedToday), "ads_blocked_today"},
			{"uniqueClients", phDeref(sum.UniqueClients), "unique_clients"},
			{"gravityDomains", phDeref(sum.GravityDomains), "domains_being_blocked"},
		} {
			if fmt.Sprint(c.got) != stats[c.key] {
				diffs = append(diffs, fmt.Sprintf("%s: Nexus %v, >stats %s=%s", c.field, c.got, c.key, stats[c.key]))
			}
		}
		want, err := strconv.ParseFloat(stats["ads_percentage_today"], 64)
		if got := phDeref(sum.BlockedPercent); err != nil || got == nil || got.(float64) != want {
			diffs = append(diffs, fmt.Sprintf("blockedPercent: Nexus %v, >stats %s", got, stats["ads_percentage_today"]))
		}
		if !sum.FTLAPIReachable {
			diffs = append(diffs, "ftlApiReachable false")
		}
		return diffs
	}
	var diffs []string
	for range 5 {
		sum = phOne[phspec.SummaryProperties](t, n, phspec.SummaryResourceType)
		stats = phFTLStats(t)
		if diffs = agree(); len(diffs) == 0 {
			break
		}
	}
	t.Logf("summary %+v; >stats %v", sum, stats)
	for _, d := range diffs {
		t.Errorf("summary %s", d)
	}

	// Blocking.
	bl := phOne[phspec.BlockingProperties](t, n, phspec.BlockingResourceType)
	if bl.State != wantBlocking || bl.ReenableAt != nil {
		t.Errorf("blocking %+v, host BLOCKING_ENABLED gives %s and no pending timer", bl, wantBlocking)
	}

	// Domains and adlists, every row and every field, and each get.
	hostDomains := phHostDomains(t)
	domains := phList[phspec.DomainProperties](t, n, phspec.DomainResourceType, "limit=1")
	if len(domains) != len(hostDomains) || len(domains) < 4 {
		t.Errorf("domains: Nexus %d rows, host %d", len(domains), len(hostDomains))
	}
	for _, d := range domains {
		if want, ok := hostDomains[d.Name]; !ok || fmt.Sprintf("%+v", d.Properties) != fmt.Sprintf("%+v", want) {
			t.Errorf("domain %s: Nexus %+v, host %+v", d.Name, d.Properties, want)
		}
		var one phspec.Domain
		n.mustGet(phAt+"/domains/"+d.Name, &one)
		if fmt.Sprintf("%+v", one.Properties) != fmt.Sprintf("%+v", d.Properties) {
			t.Errorf("domains get %s: %+v, list %+v", d.Name, one.Properties, d.Properties)
		}
	}
	var filtered []string
	for _, d := range phList[phspec.DomainProperties](t, n, phspec.DomainResourceType, "filter=list%3Ddeny&filter=kind%3Dregex") {
		filtered = append(filtered, d.Properties.Value)
	}
	if want := phSQL(t, "SELECT domain FROM domainlist WHERE type = 3 ORDER BY id"); strings.Join(filtered, "\n") != want {
		t.Errorf("domains filter list=deny kind=regex: Nexus %q, host %q", filtered, want)
	}
	hostAdlists := phHostAdlists(t)
	adlists := phList[phspec.AdlistProperties](t, n, phspec.AdlistResourceType, "")
	if len(adlists) != len(hostAdlists) || len(adlists) < 3 {
		t.Errorf("adlists: Nexus %d rows, host %d", len(adlists), len(hostAdlists))
	}
	for _, a := range adlists {
		want, ok := hostAdlists[a.Name]
		if !ok || fmt.Sprint(a.Properties.PiholeID, a.Properties.URL, a.Properties.Enabled, a.Properties.Comment,
			a.Properties.Added, a.Properties.Modified, phDeref(a.Properties.Domains), phDeref(a.Properties.Status),
			phDeref(a.Properties.LastUpdated)) != fmt.Sprint(want.PiholeID, want.URL, want.Enabled, want.Comment,
			want.Added, want.Modified, phDeref(want.Domains), phDeref(want.Status), phDeref(want.LastUpdated)) {
			t.Errorf("adlist %s: Nexus %+v, host %+v", a.Name, a.Properties, want)
		}
		var one phspec.Adlist
		n.mustGet(phAt+"/adlists/"+a.Name, &one)
		if one.Properties.URL != a.Properties.URL {
			t.Errorf("adlists get %s: %+v", a.Name, one.Properties)
		}
	}
	t.Logf("domains %d and adlists %d rows equal pihole-FTL sqlite3's", len(domains), len(adlists))

	// Zones and records: the file's own text (cat), for the seed and for a
	// file with zones, unzoned records and a duplicate. FTL never loads it.
	for _, file := range []string{phSeed, phTC121File} {
		phWriteDNSFile(t, []byte(file))
		raw := phReadDNSFile(t)
		etag := "sha256:" + phSHA(raw)
		zones, records := phParseFile(raw)
		t.Logf("cat %s:\n%s", phDNSFile, raw)
		var gotZones []string
		for _, z := range phList[phspec.ZoneProperties](t, n, phspec.ZoneResourceType, "") {
			gotZones = append(gotZones, z.Name)
			want := 0
			for _, r := range records {
				if r.zone == z.Name {
					want += r.lines
				}
			}
			if z.Properties.Zone != z.Name || z.Properties.Etag != etag || z.Properties.Records != want {
				t.Errorf("zone %s: %+v, want %d records and etag %s", z.Name, z.Properties, want, etag)
			}
			var one phspec.Zone
			n.mustGet(phAt+"/zones/"+z.Name, &one)
			if one.Properties != z.Properties {
				t.Errorf("zones get %s: %+v, list %+v", z.Name, one.Properties, z.Properties)
			}
		}
		if !slices.Equal(gotZones, zones) {
			t.Errorf("zones: Nexus %v, file %v", gotZones, zones)
		}
		got := phList[phspec.RecordProperties](t, n, phspec.RecordResourceType, "")
		lines := 0
		for _, r := range got {
			want, ok := records[r.Properties.FQDN]
			lines++
			zone := ""
			if r.Properties.Zone != nil {
				zone = *r.Properties.Zone
			}
			if !ok || r.Name != r.Properties.FQDN+"~A" || r.Properties.Type != "A" || zone != want.zone ||
				r.Properties.Duplicate != (want.lines > 1) || r.Properties.Etag != etag ||
				(!r.Properties.Duplicate && r.Properties.IPv4 != want.ipv4) {
				t.Errorf("record %s: %+v, file says %+v (etag %s)", r.Name, r.Properties, want, etag)
			}
			if !r.Properties.Duplicate {
				var one phspec.Record
				n.mustGet(phAt+"/records/"+url.PathEscape(r.Name), &one)
				if fmt.Sprint(one.Properties.FQDN, one.Properties.IPv4, phDeref(one.Properties.Zone), one.Properties.Etag) !=
					fmt.Sprint(r.Properties.FQDN, r.Properties.IPv4, phDeref(r.Properties.Zone), r.Properties.Etag) {
					t.Errorf("records get %s: %+v, list %+v", r.Name, one.Properties, r.Properties)
				}
			}
		}
		want := 0
		for _, r := range records {
			want += r.lines
		}
		if lines != want {
			t.Errorf("records: Nexus %d, file %d address lines", lines, want)
		}
		t.Logf("zones %v and %d records equal the file (etag %s)", gotZones, lines, etag)
	}

	// Capabilities details equal the image's versions.
	c := phCapability(t, n, phspec.Name)
	t.Logf("capabilities pihole: %s %v", c.Status, c.Details)
	for k, want := range map[string]any{"backend": phspec.BackendV5CLI, "coreVersion": vf["CORE_VERSION"],
		"webVersion": vf["WEB_VERSION"], "ftlVersion": vf["FTL_VERSION"], "dnsFile": phDNSFile, "dnsWrites": "available"} {
		if c.Status != apitypes.ProviderEnabled || fmt.Sprint(c.Details[k]) != fmt.Sprint(want) {
			t.Errorf("capabilities %s: %v (%s), host %v", k, c.Details[k], c.Status, want)
		}
	}
}

// TC-122: NFR-1, p95 under 200 ms over 50 calls of each read, with
// info.gravity_count=2000000, 100 records, 1,000 domain rows and 50 adlists.
func TestPiholeReadLatency(t *testing.T) {
	phUseSeed(t)
	n := piholed(t, "tc122", "")

	oldCount := phInfo(t, "gravity_count")
	phSQLWrite(t, "BEGIN; UPDATE info SET value = '2000000' WHERE property = 'gravity_count'; "+
		"WITH RECURSIVE i(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM i WHERE x < 1000) "+
		"INSERT INTO domainlist (type, domain, comment) SELECT x % 4, 'tc122-' || x || '.example', 'tc122 seed' FROM i; "+
		"WITH RECURSIVE i(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM i WHERE x < 50) "+
		"INSERT INTO adlist (address, comment) SELECT 'http://127.0.0.1:8099/tc122-' || x || '.txt', 'tc122 seed' FROM i; COMMIT;")
	t.Cleanup(func() {
		phSQLWrite(t, "BEGIN; UPDATE info SET value = '"+oldCount+"' WHERE property = 'gravity_count'; "+
			"DELETE FROM domainlist WHERE comment = 'tc122 seed'; DELETE FROM adlist WHERE comment = 'tc122 seed'; COMMIT;")
	})
	var file strings.Builder
	file.WriteString("# nexus:zone nfr.lan\n")
	for i := range 100 {
		fmt.Fprintf(&file, "address=/h%03d.nfr.lan/10.0.%d.%d\n", i, i/250, i%250+1)
	}
	phWriteDNSFile(t, []byte(file.String()))
	t.Logf("seeded: gravity_count %s, %s domain rows, %s adlists, %d records", phInfo(t, "gravity_count"),
		phSQL(t, "SELECT count(*) FROM domainlist"), phSQL(t, "SELECT count(*) FROM adlist"),
		strings.Count(file.String(), "address="))

	reads := []string{
		phAt + "/status", phAt + "/summary", phAt + "/blocking",
		phAt + "/domains?limit=1000", phAt + "/domains/" + phSQL(t, "SELECT max(id) FROM domainlist"),
		phAt + "/adlists?limit=1000", phAt + "/adlists/" + phSQL(t, "SELECT max(id) FROM adlist"),
		phAt + "/zones", phAt + "/zones/nfr.lan", phAt + "/records", phAt + "/records/h050.nfr.lan~A",
	}
	for _, path := range reads {
		var samples []time.Duration
		for range 50 {
			d, code, b := n.timed(http.MethodGet, path, "")
			if code != http.StatusOK {
				t.Fatalf("GET %s: %d %s", path, code, b)
			}
			samples = append(samples, d)
		}
		s := nfrSummarize(samples)
		t.Logf("NFR TC-122 GET %s: %s", path, s)
		if s.p95 >= 200*time.Millisecond {
			t.Errorf("TC-122: GET %s p95 %s, want under 200 ms", path, ms(s.p95))
		}
	}
	if got := phList[phspec.DomainProperties](t, n, phspec.DomainResourceType, "limit=1000"); len(got) < 1000 {
		t.Errorf("the seeded domain list has %d rows", len(got))
	}
}
