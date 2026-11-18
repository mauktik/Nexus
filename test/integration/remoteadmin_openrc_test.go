//go:build integration

package integration

// WI 5.3: nexusd remote administration on real OpenRC, the Alpine half of
// the feature's T1 matrix (planning/nexusd-remote-admin/test-cases.md
// TC-RA-104 to TC-RA-107, TC-RA-ERR37 and TC-RA-ERR38). Every case starts
// from a wiped host (raOpenRCHost): install.sh writes the shipped default
// /etc/nexus/nexusd.yaml and /etc/init.d/nexusd, sed enables the nexusd
// provider in place, lib.sh start-nexusd starts the shipped script
// unchanged (supervise-daemon) and waits for the ready line, `nexusd token
// create` mints the token ra (nexusd:read, nexusd:admin), and this binary
// runs as the nexus CLI over the pinned certificate. Cleanup puts back the
// harness install (freshHost).
//
// Token management and config get work as on systemd. Config set runs its
// helper in a systemd unit, so here it is refused at admission with
// config-set-unavailable, before any operation exists, and the reason is
// the one `nexus nexusd get` reports for configSet and selfManagement.
// Expected values come from the files (sha256, stat, tokens.json,
// audit.jsonl, /var/log/messages), never only from Nexus.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// raMessages is where the shipped script's output_logger and error_logger
// (logger -t nexusd) lines land on the Alpine harness: BusyBox syslogd's
// /var/log/messages. The script writes no /var/log/nexus/nexusd.log; the
// sinks sweep every file under /var/log/nexus anyway.
const raMessages = "/var/log/messages"

// raOpenRCHost is one OpenRC case's host. msgOff is /var/log/messages's
// size before nexusd started, so the case's own daemon lines start there.
type raOpenRCHost struct {
	*raHost
	msgOff int64
}

// newRAOpenRCHost prepares the T1-alpine host (test-cases.md TC-RA-104
// Setup).
func newRAOpenRCHost(t *testing.T) *raOpenRCHost {
	t.Helper()
	requireInit(t, platform.OpenRC)
	freshHost(t)
	runInstall(t)
	requireShippedScript(t)
	mustSh(t, "sed", "-i", `/^  nexusd:$/,/enabled:/ s/enabled: false/enabled: true/`, nexusdConfig)
	cfg := raRead(t, nexusdConfig)
	if v := raYAMLValue(t, cfg, "providers.nexusd.enabled"); v != true {
		t.Fatalf("providers.nexusd.enabled is %v after the sed, want true", v)
	}
	if v := raYAMLValue(t, cfg, "providers.host.enabled"); v != false {
		t.Fatalf("providers.host.enabled is %v in the default file, want false", v)
	}
	if out, err := sh(time.Minute, nexusdBin, "validate-config"); err != nil {
		t.Fatalf("validate-config: %v\n%s", err, out)
	}
	h := &raOpenRCHost{raHost: &raHost{smHost: &smHost{t: t}}}
	if fi, err := os.Stat(raMessages); err == nil {
		h.msgOff = fi.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	lib(t, "start-nexusd")
	if h.pid0 = openrcMainPID(t); h.pid0 == 0 {
		t.Fatalf("nexusd is %s with no main PID after start-nexusd", rcStatus(t))
	}
	h.token = createToken(t, "ra", spec.ReadScope, spec.AdminScope)
	h.fp = client.Fingerprint(readCert(t, filepath.Join(nexusdState, "tls", "cert.pem")).Raw)
	tc, err := client.PinnedTLS(h.fp)
	if err != nil {
		t.Fatal(err)
	}
	h.http = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}
	h.home = h.cliHome(h.token)
	t.Logf("T1-alpine host: install.sh default config with the nexusd provider enabled; /etc/init.d/nexusd "+
		"started, nexusd PID %d, certificate %s", h.pid0, h.fp)
	return h
}

// daemonLog is the case's nexusd lines: /var/log/messages from msgOff.
func (h *raOpenRCHost) daemonLog() []byte {
	h.t.Helper()
	b := raRead(h.t, raMessages)
	if int64(len(b)) < h.msgOff {
		h.t.Fatalf("%s shrank from %d to %d bytes during the case", raMessages, h.msgOff, len(b))
	}
	return b[h.msgOff:]
}

// availability is `nexus nexusd get -o json`'s configSet and
// selfManagement.
func (h *raOpenRCHost) availability() (configSet, selfMgmt spec.Availability) {
	h.t.Helper()
	r := h.cli("nexusd", "get", "-o", "json")
	var ds []spec.Daemon
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &ds) != nil || len(ds) != 1 {
		h.t.Fatalf("nexus nexusd get -o json: exit %d, stdout %q, want one daemon", r.code, r.stdout)
	}
	return ds[0].Properties.ConfigSet, ds[0].Properties.SelfManagement
}

// raTokenFile is tokens.json as the test reads it.
type raTokenFile struct {
	Tokens []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"tokens"`
}

// requireTokenStore checks tokens.json is root:root 0600 and valid JSON,
// and returns its token names.
func requireTokenStore(t *testing.T) []string {
	t.Helper()
	if uid, gid, perm := fileStat(t, raTokens); uid != 0 || gid != 0 || perm != 0o600 {
		t.Errorf("%s is %d:%d %04o, want root:root 0600", raTokens, uid, gid, perm)
	}
	var f raTokenFile
	if err := json.Unmarshal(raRead(t, raTokens), &f); err != nil {
		t.Fatalf("%s is not valid JSON: %v", raTokens, err)
	}
	names := make([]string, 0, len(f.Tokens))
	for _, tk := range f.Tokens {
		if tk.Name == "" || len(tk.SHA256) != 64 {
			t.Errorf("%s: a malformed record %+v", raTokens, tk)
		}
		names = append(names, tk.Name)
	}
	return names
}

// TC-RA-104 (WI 5.3; FR-9, FR-1, FR-2, FR-3, NFR-6): on OpenRC, token add,
// get and remove round-trip and apply on the next request.
func TestTCRA104_OnOpenRCTokenAddGetAndRemoveRoundTripAndApplyOn(t *testing.T) {
	h := newRAOpenRCHost(t)

	// Steps 1 and 2.
	r := h.cli("nexusdtoken", "add", "oc", "--scopes", spec.ReadScope, "--yes", "-o", "json")
	var oc spec.TokenAddResult
	if adds := jsonItems[spec.TokenAddResult](t, r.stdout); r.code == 0 && len(adds) == 1 {
		oc = adds[0]
	}
	if oc.Token == "" || oc.Name != "oc" || !slices.Equal(oc.Scopes, []string{spec.ReadScope}) {
		t.Fatalf("step 1: exit %d, result %+v, want 0 with token oc scoped %s", r.code, oc, spec.ReadScope)
	}
	if code, _, b := h.raDo(oc.Token, http.MethodGet, smDaemonsAt, "", nil); code != http.StatusOK {
		t.Errorf("step 2: GET %s with oc: %d %s, want 200", smDaemonsAt, code, b)
	}
	if names := requireTokenStore(t); !slices.Contains(names, "oc") || !slices.Contains(names, "ra") {
		t.Errorf("after the add: %s holds %q, want ra and oc", raTokens, names)
	}

	// Step 3.
	g := h.cli("nexusdtoken", "get", "-o", "json")
	var names, selfs []string
	for _, tk := range jsonItems[spec.Token](t, g.stdout) {
		names = append(names, tk.Name)
		if tk.Properties.Self {
			selfs = append(selfs, tk.Name)
		}
		if tk.Name == "oc" && (tk.Properties.TokenID != oc.TokenID ||
			!slices.Equal(tk.Properties.Scopes, []string{spec.ReadScope})) {
			t.Errorf("step 3: oc is %+v, want token ID %s scoped %s", tk.Properties, oc.TokenID, spec.ReadScope)
		}
	}
	if g.code != 0 || !slices.Contains(names, "oc") || !slices.Equal(selfs, []string{"ra"}) {
		t.Errorf("step 3: exit %d, names %q, self true for %q; want 0, oc listed, self only on ra", g.code, names, selfs)
	}
	if strings.Contains(g.stdout, oc.Token) {
		t.Error("step 3: the listing holds oc's secret")
	}

	// Steps 4 and 5.
	if rm := h.cli("nexusdtoken", "remove", "oc", "--yes", "-o", "json"); rm.code != 0 {
		t.Errorf("step 4: nexusdtoken remove oc: exit %d, want 0", rm.code)
	}
	if code, _, b := h.raDo(oc.Token, http.MethodGet, smDaemonsAt, "", nil); code != http.StatusUnauthorized {
		t.Errorf("step 5: GET %s with the removed oc: %d %s, want 401", smDaemonsAt, code, b)
	}
	if code, _, b := h.raDo(h.token, http.MethodGet, smDaemonsAt, "", nil); code != http.StatusOK {
		t.Errorf("step 5: GET %s with ra: %d %s, want 200", smDaemonsAt, code, b)
	}

	// Store.
	if names := requireTokenStore(t); !slices.Equal(names, []string{"ra"}) {
		t.Errorf("after the remove: %s holds %q, want only ra", raTokens, names)
	}
}

// raLeafKeys lists the dotted leaf keys of a YAML document: every key
// whose value is a scalar, a list or an empty map. A list is one leaf, as
// the configuration schema has it.
func raLeafKeys(t *testing.T, data []byte) []string {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("decode the configuration: %v", err)
	}
	var out []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			if sub, ok := v.(map[string]any); ok && len(sub) > 0 {
				walk(p, sub)
				continue
			}
			out = append(out, p)
		}
	}
	walk("", root)
	slices.Sort(out)
	return out
}

// raTableRows splits a CLI table into its header fields and its rows.
func raTableRows(t *testing.T, r smRun) ([]string, []string) {
	t.Helper()
	rows := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(rows) == 0 || rows[0] == "" {
		t.Fatalf("nexus %s: no table on stdout", strings.Join(r.args, " "))
	}
	return strings.Fields(rows[0]), rows[1:]
}

// TC-RA-105 (WI 5.3; FR-9, FR-6): on OpenRC, config get works.
func TestTCRA105_OnOpenRCConfigGetWorks(t *testing.T) {
	h := newRAOpenRCHost(t)
	wantHeader := []string{"HOST", "NAME", "VALUE", "EFFECTIVE", "SOURCE"}
	leaves := raLeafKeys(t, raRead(t, nexusdConfig))

	// Step 1.
	r := h.cli("nexusdconfig", "get", "-o", "table")
	header, rows := raTableRows(t, r)
	if r.code != 0 || !slices.Equal(header, wantHeader) {
		t.Fatalf("step 1: exit %d, header %q; want 0 and %q", r.code, header, wantHeader)
	}
	var keys []string
	for _, row := range rows {
		if f := strings.Fields(row); len(f) > 1 {
			keys = append(keys, f[1])
		}
	}
	slices.Sort(keys)
	if len(rows) != len(leaves) || !slices.Equal(keys, leaves) {
		var missing, extra []string
		for _, k := range leaves {
			if !slices.Contains(keys, k) {
				missing = append(missing, k)
			}
		}
		for _, k := range keys {
			if !slices.Contains(leaves, k) {
				extra = append(extra, k)
			}
		}
		t.Errorf("step 1: %d rows, want one per leaf key of %s (%d); missing %q, extra %q", len(rows), nexusdConfig,
			len(leaves), missing, extra)
	}
	t.Logf("step 1: %d rows for the %d leaf keys of the installed file", len(rows), len(leaves))

	// Step 2.
	k := h.cli("nexusdconfig", "get", "providers.nexusd.enabled", "-o", "table")
	header, rows = raTableRows(t, k)
	if k.code != 0 || !slices.Equal(header, wantHeader) || len(rows) != 1 {
		t.Fatalf("step 2: exit %d, table %q; want 0 and one row", k.code, k.stdout)
	}
	if f := strings.Fields(rows[0]); len(f) != 5 || f[1] != "providers.nexusd.enabled" || f[2] != "true" ||
		f[3] != "true" || f[4] != "file" {
		t.Errorf("step 2: row %q, want providers.nexusd.enabled true true file", rows[0])
	}
}

// TC-RA-106 (WI 5.3; FR-9): on OpenRC, `nexus nexusd get` reports configSet
// unavailable with a reason.
func TestTCRA106_OnOpenRCNexusNexusdGetReportsConfigSet(t *testing.T) {
	h := newRAOpenRCHost(t)
	cs, sm := h.availability()
	if cs.Available || !strings.Contains(cs.Reason, "systemd") || cs.Reason != sm.Reason || sm.Available {
		t.Errorf("configSet %+v, selfManagement %+v; want configSet unavailable with a reason naming systemd, "+
			"equal to selfManagement's", cs, sm)
	}
	// The REST answer is the same.
	if d := h.daemon(); d.ConfigSet != cs || d.SelfManagement != sm {
		t.Errorf("GET %s: configSet %+v, selfManagement %+v; the CLI printed %+v, %+v", smDaemonsAt, d.ConfigSet,
			d.SelfManagement, cs, sm)
	}
	t.Logf("configSet reason: %q", cs.Reason)
}

// TC-RA-107 (WI 5.3; NFR-1, NFR-6): on OpenRC, token secrets reach no sink.
func TestTCRA107_OnOpenRCTokenSecretsReachNoSink(t *testing.T) {
	h := newRAOpenRCHost(t)
	// GET /v1/audit needs audit:read (test-cases.md names it
	// /v1/system/audit; the route is /v1/audit, as in TC-RA-100).
	auditor := createToken(t, "auditor", "audit:read")

	// Step 1.
	code, hdr, body := h.raDo(h.token, http.MethodPost, raTokensAt+"/t1oc/add", `{}`, nil)
	var add spec.TokenAddResult
	if code != http.StatusOK || json.Unmarshal(body, &add) != nil || add.Token == "" {
		t.Fatalf("step 1: %d %s, want 200 with a token", code, body)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("step 1: Cache-Control %q, want no-store", cc)
	}
	parts := strings.SplitN(add.Token, "_", 3)
	hash := tokenHashes(t)[add.Name]
	if len(parts) != 3 || hash == "" {
		t.Fatalf("token %s: %q is not nxs_<id>_<secret>, or tokens.json has no hash for it", add.Name, add.Token)
	}
	s := raSecret{name: add.Name, token: add.Token, secret: parts[2], hash: hash}
	// The token authenticates, then is removed, so the auth path and the
	// remove reach every sink too.
	if code, _, b := h.raDo(s.token, http.MethodGet, smDaemonsAt, "", nil); code != http.StatusForbidden {
		t.Errorf("GET %s with the scopeless %s: %d %s, want 403: authenticated, not authorized", smDaemonsAt, s.name,
			code, b)
	}
	if code, _, b := h.raDo(h.token, http.MethodPost, raTokensAt+"/"+s.name+"/remove", `{}`, nil); code != http.StatusOK {
		t.Errorf("remove %s: %d %s", s.name, code, b)
	}

	// Non-vacuity, as in TC-RA-100: the answer carried the secret, and
	// tokens.json held its hash.
	if !strings.Contains(string(body), s.token) {
		t.Error("non-vacuity: the step 1 body lacks its token")
	}

	// Step 2.
	sinks := map[string][]byte{}
	dbs, err := filepath.Glob(raStateDB + "*")
	if err != nil {
		t.Fatal(err)
	}
	audits, err := filepath.Glob(smAudit + "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) == 0 || len(audits) == 0 {
		t.Fatalf("non-vacuity: state.db* %q, audit.jsonl* %q", dbs, audits)
	}
	for _, f := range dbs {
		sinks[f] = raRead(t, f)
	}
	// Every regular file under ops/ and /var/log/nexus: audit.jsonl*, and a
	// nexusd.log were there one.
	for _, root := range []string{smOpsDir, logDir} {
		if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				sinks[p] = raRead(t, p)
			}
			return nil
		}); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	for _, f := range audits {
		if _, ok := sinks[f]; !ok {
			t.Errorf("non-vacuity: %s was not swept", f)
		}
	}
	dlog := h.daemonLog()
	if !bytes.Contains(dlog, []byte("nexusd ready")) {
		t.Errorf("non-vacuity: %s since nexusd started has no \"nexusd ready\" line", raMessages)
	}
	sinks[raMessages+" (the daemon log)"] = dlog
	var opsBodies []byte
	for _, b := range h.pages(h.token, "/v1/operations") {
		opsBodies = append(opsBodies, b...)
	}
	sinks["GET /v1/operations"] = opsBodies
	var auditBodies []byte
	for _, b := range h.pages(auditor, "/v1/audit") {
		auditBodies = append(auditBodies, b...)
	}
	sinks["GET /v1/audit"] = auditBodies
	if !bytes.Contains(auditBodies, []byte(spec.TokensAdd.Op().ID)) ||
		!bytes.Contains(auditBodies, []byte(spec.TokensRemove.Op().ID)) {
		t.Errorf("non-vacuity: GET /v1/audit has no %s and %s records", spec.TokensAdd.Op().ID, spec.TokensRemove.Op().ID)
	}
	for name, b := range sinks {
		for _, n := range s.needles() {
			if c := bytes.Count(b, []byte(n)); c != 0 {
				t.Errorf("%s holds %s's secret material %d times", name, s.name, c)
			}
		}
	}
	t.Logf("swept %d sinks for the token, its secret and its hash: 0 occurrences expected", len(sinks))
}

// raDirEntries lists dir's entry names, none when it does not exist.
func raDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// TC-RA-ERR37 (WI 5.3; FR-9, OOS-2, NFR-6): on OpenRC, config set is
// refused with config-set-unavailable and the file is untouched.
func TestTCRAERR37_OnOpenRCConfigSetIsRefusedWithConfigSet(t *testing.T) {
	h := newRAOpenRCHost(t)
	cs, _ := h.availability()
	if cs.Available || !strings.Contains(cs.Reason, "systemd") {
		t.Fatalf("configSet %+v, want unavailable with a reason naming systemd", cs)
	}
	before := fileState(t)
	if before.bak {
		t.Fatalf("%s exists before any config set", raBackup)
	}
	ids0 := h.opIDs()
	ops0 := raDirEntries(t, smOpsDir)
	cg0 := raDirEntries(t, opsCgroup)
	mark := auditMark(t)
	typ, ok := problem.Lookup(problem.ConfigSetUnavailable)
	if !ok || typ.Status != http.StatusServiceUnavailable || typ.Exit != 4 {
		t.Fatalf("problem type %s is %+v, want 503 with exit code 4", problem.ConfigSetUnavailable, typ)
	}

	// Step 1.
	r := h.cli("nexusdconfig", "update", "--yes", "providers.host.enabled=true")
	p := requireRefused(t, r, problem.ConfigSetUnavailable, http.StatusServiceUnavailable)
	if reason, _ := p["reason"].(string); reason != cs.Reason || r.code != 4 {
		t.Errorf("step 1: exit %d, reason %q; want exit 4 and nexusd get's configSet reason %q", r.code, reason, cs.Reason)
	}

	// Step 2.
	code, hdr, body := h.raDo(h.token, http.MethodPost, raUpdateAt, `{"changes":["providers.host.enabled=true"]}`, nil)
	var p2 map[string]any
	if json.Unmarshal(body, &p2) != nil || code != http.StatusServiceUnavailable ||
		smSlug(p2) != string(problem.ConfigSetUnavailable) || int(asFloat(p2["status"])) != http.StatusServiceUnavailable {
		t.Errorf("step 2: POST %s: %d %s, want 503 %s", raUpdateAt, code, body, problem.ConfigSetUnavailable)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, problem.ContentType) {
		t.Errorf("step 2: Content-Type %q, want %s", ct, problem.ContentType)
	}
	if reason, _ := p2["reason"].(string); reason != cs.Reason {
		t.Errorf("step 2: reason %q, want nexusd get's configSet reason %q", reason, cs.Reason)
	}

	// Nothing changed.
	requireFileState(t, "after the refusals", before)
	if exists(raBackup) {
		t.Errorf("%s exists after the refusals", raBackup)
	}
	if v := raYAMLValue(t, raRead(t, nexusdConfig), "providers.host.enabled"); v != false {
		t.Errorf("providers.host.enabled is %v on disk, want false", v)
	}
	if ids := h.opIDs(); !slices.Equal(ids, ids0) {
		t.Errorf("GET /v1/operations: %q, want %q: no operation created", ids, ids0)
	}
	if e := raDirEntries(t, smOpsDir); !slices.Equal(e, ops0) {
		t.Errorf("%s holds %q, want %q: nothing launched", smOpsDir, e, ops0)
	}
	if e := raDirEntries(t, opsCgroup); !slices.Equal(e, cg0) {
		t.Errorf("%s holds %q, want %q: nothing launched", opsCgroup, e, cg0)
	}

	// Audit.
	refused := 0
	for _, a := range auditSince(t, mark) {
		if a.Action == spec.UpdateConfig.Op().ID && a.Outcome == "refused" &&
			a.Problem == string(problem.ConfigSetUnavailable) {
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("audit: %d refused %s records for %s, want one per call (2)", refused, problem.ConfigSetUnavailable,
			spec.UpdateConfig.Op().ID)
	}
}

// TC-RA-ERR38 (WI 5.3; FR-5, NFR-6): on OpenRC, the 429 path and the token
// cap behave as on systemd.
func TestTCRAERR38_OnOpenRCThe429PathAndTheTokenCapBehaveAsOn(t *testing.T) {
	h := newRAOpenRCHost(t)

	// The cap: a store of spec.MaxRemoteTokens, filled locally, refuses a
	// remote add with token-limit-reached. A refusal leaves the rolling
	// window as it was.
	n0 := len(tokenHashes(t))
	var fillers []string
	for i := n0; i < spec.MaxRemoteTokens; i++ {
		name := fmt.Sprintf("cap%03d", i)
		if _, _, err := auth.Create(nexusdState, auth.CreateRequest{Name: name}); err != nil {
			t.Fatalf("fill %s: %v", name, err)
		}
		fillers = append(fillers, name)
	}
	capSHA := sha(t, raTokens)
	code, _, body := h.raDo(h.token, http.MethodPost, raTokensAt+"/over/add", `{}`, nil)
	var pc map[string]any
	if json.Unmarshal(body, &pc) != nil || code != http.StatusConflict || smSlug(pc) != string(problem.TokenLimitReached) ||
		int(asFloat(pc["maxTokens"])) != spec.MaxRemoteTokens {
		t.Errorf("the cap: %d %s, want 409 %s with maxTokens %d", code, body, problem.TokenLimitReached, spec.MaxRemoteTokens)
	}
	if got := sha(t, raTokens); got != capSHA {
		t.Errorf("the cap: %s sha256 %s, want %s unchanged", raTokens, got, capSHA)
	}
	for _, name := range fillers {
		if _, err := auth.Revoke(nexusdState, name); err != nil {
			t.Fatalf("revoke %s: %v", name, err)
		}
	}
	if n := len(tokenHashes(t)); n != n0 {
		t.Fatalf("%d tokens after the fillers' removal, want %d", n, n0)
	}

	// Step 1.
	start := time.Now()
	for i := range spec.TokenMutationsPerMinute {
		name := fmt.Sprintf("rl%02d", i)
		if code, _, b := h.raDo(h.token, http.MethodPost, raTokensAt+"/"+name+"/add", `{}`, nil); code != http.StatusOK {
			t.Fatalf("step 1: add %s: %d %s, want 200", name, code, b)
		}
	}
	sha0 := sha(t, raTokens)

	// Step 2.
	code, hdr, body := h.raDo(h.token, http.MethodPost, raTokensAt+"/rl10/add", `{}`, nil)
	took := time.Since(start)
	if took >= time.Minute {
		t.Fatalf("the %d adds and the next took %s, not within a minute", spec.TokenMutationsPerMinute, took)
	}
	var p map[string]any
	if json.Unmarshal(body, &p) != nil || code != http.StatusTooManyRequests || smSlug(p) != string(problem.RateLimited) ||
		int(asFloat(p["status"])) != http.StatusTooManyRequests {
		t.Errorf("step 2: %d %s, want 429 %s", code, body, problem.RateLimited)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, problem.ContentType) {
		t.Errorf("step 2: Content-Type %q, want %s", ct, problem.ContentType)
	}
	ra, err := strconv.Atoi(hdr.Get("Retry-After"))
	if err != nil || ra < 1 || ra > 60 {
		t.Errorf("step 2: Retry-After %q, want an integer from 1 to 60", hdr.Get("Retry-After"))
	}
	if got := sha(t, raTokens); got != sha0 {
		t.Errorf("step 2: %s sha256 %s, want %s unchanged", raTokens, got, sha0)
	}
	if slices.Contains(requireTokenStore(t), "rl10") {
		t.Errorf("step 2: %s holds rl10", raTokens)
	}
	t.Logf("%d adds then a 429 in %s, Retry-After %s", spec.TokenMutationsPerMinute, took.Round(time.Millisecond),
		hdr.Get("Retry-After"))
}
